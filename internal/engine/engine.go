// Package engine implements ProxBack's chunk-based, deduplicating backup engine:
// 4 MiB fixed chunking with SHA-256 content addressing, a per-target chunk index
// with an S3 HEAD fallback, JSON manifests, verified restores and orphan chunk
// garbage collection.
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"proxback/internal/s3target"
)

// ErrHashMismatch is returned when restored data fails verification.
var ErrHashMismatch = errors.New("engine: chunk hash verification failed")

// ChunkIndex is the per-target dedup index (implemented by *store.Store).
type ChunkIndex interface {
	HasChunk(ctx context.Context, targetID, sha string) (bool, error)
	AddChunk(ctx context.Context, targetID, sha string, size int64) error
	DeleteChunk(ctx context.Context, targetID, sha string) error
	// ChunkAddedAt returns every indexed chunk of a target with the time it was
	// recorded, which is what lets garbage collection spare recent uploads.
	ChunkAddedAt(ctx context.Context, targetID string) (map[string]time.Time, error)
}

// DefaultGCGrace is how long a chunk is protected from orphan collection after
// it was uploaded. An interrupted backup uploads chunks but never writes the
// manifest that references them, so without the grace window the next GC pass
// would delete exactly the work the retry wants to deduplicate against.
const DefaultGCGrace = 24 * time.Hour

// Options tunes the engine. The zero value is the shipped default: 4 concurrent
// chunk uploads, zstd chunk compression and a 24 h orphan-collection grace.
type Options struct {
	// UploadConcurrency is the number of chunk uploads kept in flight per stream
	// (1–16, 0 selects DefaultUploadConcurrency).
	UploadConcurrency int
	// Compression is CompressionZstd (also the zero value's meaning) or
	// CompressionOff.
	Compression string
	// GCGrace overrides DefaultGCGrace. A negative value disables the grace
	// window, which is only sensible in tests.
	GCGrace time.Duration
}

// Engine is bound to exactly one backup target.
type Engine struct {
	s3       *s3target.Client
	targetID string
	idx      ChunkIndex
	log      *slog.Logger

	workers  int
	compress bool
	gcGrace  time.Duration

	// inflight coalesces concurrent stores of the same chunk. Without it two
	// workers holding identical chunks would both miss the index and both upload,
	// so a zero-filled disk would report different byte counts depending on the
	// worker count.
	inflightMu sync.Mutex
	inflight   map[string]*chunkStore
}

// chunkStore is one in-progress upload other workers can wait on.
type chunkStore struct {
	done chan struct{}
	err  error
}

// New builds an engine for one target with the default options.
func New(s3c *s3target.Client, targetID string, idx ChunkIndex, log *slog.Logger) *Engine {
	return NewWithOptions(s3c, targetID, idx, log, Options{})
}

// NewWithOptions builds an engine for one target, normalising out-of-range
// options to their defaults so a bad setting can never break a backup.
func NewWithOptions(s3c *s3target.Client, targetID string, idx ChunkIndex, log *slog.Logger, opts Options) *Engine {
	if log == nil {
		log = slog.Default()
	}
	workers := opts.UploadConcurrency
	if workers < MinUploadConcurrency {
		workers = DefaultUploadConcurrency
	}
	if workers > MaxUploadConcurrency {
		workers = MaxUploadConcurrency
	}
	grace := opts.GCGrace
	switch {
	case grace == 0:
		grace = DefaultGCGrace
	case grace < 0:
		grace = 0
	}
	return &Engine{
		s3:       s3c,
		targetID: targetID,
		idx:      idx,
		log:      log,
		workers:  workers,
		compress: opts.Compression != CompressionOff,
		gcGrace:  grace,
		inflight: map[string]*chunkStore{},
	}
}

// UploadConcurrency reports the engine's worker count.
func (e *Engine) UploadConcurrency() int { return e.workers }

// CompressionEnabled reports whether chunks are stored zstd compressed.
func (e *Engine) CompressionEnabled() bool { return e.compress }

// GCGrace reports how long a chunk is protected from orphan collection.
func (e *Engine) GCGrace() time.Duration { return e.gcGrace }

// TargetID returns the target this engine writes to.
func (e *Engine) TargetID() string { return e.targetID }

// Stats is the progress snapshot handed to a ProgressFunc.
type Stats struct {
	BytesProcessed int64   `json:"bytesProcessed"`
	BytesUploaded  int64   `json:"bytesUploaded"`
	BytesDeduped   int64   `json:"bytesDeduped"`
	ChunksTotal    int     `json:"chunksTotal"`
	ChunksUploaded int     `json:"chunksUploaded"`
	ChunksDeduped  int     `json:"chunksDeduped"`
	CurrentStep    string  `json:"currentStep"`
	Pct            float64 `json:"progressPct"`
}

// DedupRatio is the fraction of processed bytes that did not need uploading.
func (s Stats) DedupRatio() float64 {
	if s.BytesProcessed <= 0 {
		return 0
	}
	r := 1 - float64(s.BytesUploaded)/float64(s.BytesProcessed)
	if r < 0 {
		return 0
	}
	return r
}

// ProgressFunc receives throttled progress updates.
type ProgressFunc func(Stats)

// Session accumulates progress across the streams of one backup or restore.
type Session struct {
	e     *Engine
	prog  ProgressFunc
	mu    sync.Mutex
	st    Stats
	total int64
	last  time.Time
}

// NewSession starts a progress-tracking session. total is the expected number of
// bytes to process and may be 0 when unknown.
func (e *Engine) NewSession(total int64, prog ProgressFunc) *Session {
	return &Session{e: e, prog: prog, total: total}
}

// SetTotal updates the expected byte total (used when it only becomes known late).
func (s *Session) SetTotal(total int64) {
	s.mu.Lock()
	if total > s.total {
		s.total = total
	}
	s.mu.Unlock()
}

// SetStep sets the human readable current step and flushes an update.
func (s *Session) SetStep(step string) {
	s.mu.Lock()
	s.st.CurrentStep = step
	snap := s.snapshotLocked()
	s.last = time.Now()
	s.mu.Unlock()
	s.emit(snap)
}

// Stats returns the current progress snapshot.
func (s *Session) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

// Flush emits the current progress unconditionally.
func (s *Session) Flush() {
	s.mu.Lock()
	snap := s.snapshotLocked()
	s.last = time.Now()
	s.mu.Unlock()
	s.emit(snap)
}

func (s *Session) snapshotLocked() Stats {
	out := s.st
	if s.total > 0 {
		out.Pct = float64(out.BytesProcessed) / float64(s.total) * 100
		if out.Pct > 100 {
			out.Pct = 100
		}
	}
	return out
}

func (s *Session) emit(snap Stats) {
	if s.prog != nil {
		s.prog(snap)
	}
}

// record accounts for one processed chunk.
func (s *Session) record(size, uploaded int64) {
	s.mu.Lock()
	s.st.BytesProcessed += size
	s.st.ChunksTotal++
	if uploaded > 0 {
		s.st.BytesUploaded += uploaded
		s.st.ChunksUploaded++
	} else {
		s.st.BytesDeduped += size
		s.st.ChunksDeduped++
	}
	var snap Stats
	send := false
	if time.Since(s.last) >= 200*time.Millisecond {
		snap = s.snapshotLocked()
		s.last = time.Now()
		send = true
	}
	s.mu.Unlock()
	if send {
		s.emit(snap)
	}
}

// RecordChunk accounts for a chunk that was pushed in by an agent.
func (s *Session) RecordChunk(size, uploaded int64) { s.record(size, uploaded) }

// StoreChunk hashes data, uploads it if the target does not have it yet, and
// returns the chunk hash plus the number of bytes actually uploaded.
func (e *Engine) StoreChunk(ctx context.Context, data []byte) (sha string, uploaded int64, err error) {
	sum := sha256.Sum256(data)
	sha = hex.EncodeToString(sum[:])
	up, err := e.storeChunkWithHash(ctx, sha, data)
	return sha, up, err
}

// StoreChunkVerified stores a chunk whose hash was supplied by a caller (an
// agent), rejecting the chunk if the hash does not match the data.
func (e *Engine) StoreChunkVerified(ctx context.Context, sha string, data []byte) (uploaded int64, err error) {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, sha) {
		return 0, fmt.Errorf("%w: declared %s, actual %s", ErrHashMismatch, sha, got)
	}
	return e.storeChunkWithHash(ctx, got, data)
}

// storeChunkWithHash stores one chunk, coalescing concurrent attempts at the
// same content so the byte accounting does not depend on the worker count: the
// first caller uploads, the others wait for it and are credited as deduplicated,
// exactly as they would have been by a serial pipeline.
func (e *Engine) storeChunkWithHash(ctx context.Context, sha string, data []byte) (int64, error) {
	for {
		e.inflightMu.Lock()
		if pending, ok := e.inflight[sha]; ok {
			e.inflightMu.Unlock()
			select {
			case <-pending.done:
			case <-ctx.Done():
				return 0, ctx.Err()
			}
			if pending.err != nil {
				// The upload we were waiting on failed; do the work ourselves
				// rather than reporting a chunk that is not there.
				continue
			}
			return 0, nil
		}
		pending := &chunkStore{done: make(chan struct{})}
		e.inflight[sha] = pending
		e.inflightMu.Unlock()

		uploaded, err := e.putChunk(ctx, sha, data)

		e.inflightMu.Lock()
		delete(e.inflight, sha)
		e.inflightMu.Unlock()
		pending.err = err
		close(pending.done)
		return uploaded, err
	}
}

// putChunk performs the actual dedup check and upload of one chunk. The chunk's
// key and index row stay keyed on the hash of the RAW bytes; only the object's
// payload is compressed, so existing chunks, existing manifests and the dedup
// index keep working whatever the compression setting is.
func (e *Engine) putChunk(ctx context.Context, sha string, data []byte) (int64, error) {
	has, err := e.idx.HasChunk(ctx, e.targetID, sha)
	if err != nil {
		return 0, fmt.Errorf("engine: chunk index: %w", err)
	}
	if has {
		return 0, nil
	}
	// Cache miss: the chunk may still be on the target (fresh index, or an index
	// row lost). Fall back to a HEAD before spending upload bandwidth.
	if _, exists, err := e.s3.Head(ctx, ChunkKey(sha)); err != nil {
		return 0, err
	} else if exists {
		if err := e.idx.AddChunk(ctx, e.targetID, sha, int64(len(data))); err != nil {
			return 0, fmt.Errorf("engine: chunk index: %w", err)
		}
		return 0, nil
	}
	body := data
	if e.compress {
		body = compressChunk(data)
	}
	if err := waitUpload(ctx, len(body)); err != nil {
		return 0, err
	}
	if err := e.s3.Put(ctx, ChunkKey(sha), body); err != nil {
		return 0, err
	}
	// The index records the raw chunk size: it is the dedup index of the stream's
	// content, not a bucket inventory.
	if err := e.idx.AddChunk(ctx, e.targetID, sha, int64(len(data))); err != nil {
		return 0, fmt.Errorf("engine: chunk index: %w", err)
	}
	// Uploaded bytes are the bytes actually PUT, so a run's figure reflects real
	// bandwidth; processed bytes stay raw.
	return int64(len(body)), nil
}

// HasChunk reports whether a chunk is present on the target, consulting the
// index first and falling back to a HEAD request.
func (e *Engine) HasChunk(ctx context.Context, sha string) (bool, error) {
	has, err := e.idx.HasChunk(ctx, e.targetID, sha)
	if err != nil {
		return false, fmt.Errorf("engine: chunk index: %w", err)
	}
	if has {
		return true, nil
	}
	size, exists, err := e.s3.Head(ctx, ChunkKey(sha))
	if err != nil {
		return false, err
	}
	if exists {
		// Recovering an index row from the object alone: the raw size is not
		// knowable without downloading the chunk, so a compressed chunk is
		// indexed with its stored size. Dedup only cares about the hash; the size
		// is a reporting figure.
		if err := e.idx.AddChunk(ctx, e.targetID, sha, size); err != nil {
			return false, fmt.Errorf("engine: chunk index: %w", err)
		}
	}
	return exists, nil
}

// WriteManifest serialises a manifest to its S3 key.
func (e *Engine) WriteManifest(ctx context.Context, m *Manifest) error {
	if m.ChunkSize == 0 {
		m.ChunkSize = ChunkSize
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("engine: encode manifest: %w", err)
	}
	key := ManifestKey(m.SourceKind, m.SourceID, m.BackupID)
	if err := e.s3.Put(ctx, key, raw); err != nil {
		return err
	}
	e.log.Debug("manifest written", "key", key, "chunks", m.ChunkCount(), "bytes", m.SizeBytes)
	return nil
}

// ReadManifest loads a manifest from S3.
func (e *Engine) ReadManifest(ctx context.Context, sourceKind, sourceID, backupID string) (*Manifest, error) {
	key := ManifestKey(sourceKind, sourceID, backupID)
	raw, err := e.s3.GetBytes(ctx, key)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("engine: decode manifest %s: %w", key, err)
	}
	return &m, nil
}

// ManifestExists reports whether a manifest object is present.
func (e *Engine) ManifestExists(ctx context.Context, sourceKind, sourceID, backupID string) (bool, error) {
	_, ok, err := e.s3.Head(ctx, ManifestKey(sourceKind, sourceID, backupID))
	return ok, err
}

// DeleteManifest removes a manifest object.
func (e *Engine) DeleteManifest(ctx context.Context, sourceKind, sourceID, backupID string) error {
	return e.s3.Delete(ctx, ManifestKey(sourceKind, sourceID, backupID))
}

// RestoreDisk streams a disk back out of the target, verifying every chunk's
// SHA-256 and the total size.
func (s *Session) RestoreDisk(ctx context.Context, dm DiskManifest, w io.Writer) error {
	var written int64
	for _, ch := range dm.Chunks {
		if err := ctx.Err(); err != nil {
			return err
		}
		stored, err := s.e.s3.GetBytes(ctx, ChunkKey(ch.Sha256))
		if err != nil {
			return fmt.Errorf("engine: restore %s: %w", dm.Name, err)
		}
		// Chunks may be stored compressed or raw, in any mix within one manifest
		// (the setting can change between runs). decodeChunk sniffs which; the
		// SHA-256 check below is the arbiter either way.
		data := decodeChunk(stored)
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != ch.Sha256 {
			return fmt.Errorf("%w: %s expected %s got %s", ErrHashMismatch, dm.Name, ch.Sha256, got)
		}
		if int64(len(data)) != ch.Size {
			return fmt.Errorf("engine: restore %s: chunk %s size %d, manifest says %d",
				dm.Name, ch.Sha256, len(data), ch.Size)
		}
		if _, err := w.Write(data); err != nil {
			return fmt.Errorf("engine: restore %s: write: %w", dm.Name, err)
		}
		written += int64(len(data))
		s.record(int64(len(data)), 0)
	}
	if dm.SizeBytes != 0 && written != dm.SizeBytes {
		return fmt.Errorf("engine: restore %s: wrote %d bytes, manifest says %d", dm.Name, written, dm.SizeBytes)
	}
	return nil
}

// VerifyBackup streams every chunk of every disk of a restore point through the
// restore path's verification — per-chunk SHA-256 plus size checks — discarding
// the data. It proves the point is restorable without writing anywhere.
func (s *Session) VerifyBackup(ctx context.Context, m *Manifest) error {
	if len(m.Disks) == 0 {
		return fmt.Errorf("engine: backup %s has no disks to verify", m.BackupID)
	}
	for _, disk := range m.Disks {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.SetStep(fmt.Sprintf("Verifying %s %s", m.SourceName, disk.Name))
		if err := s.RestoreDisk(ctx, disk, io.Discard); err != nil {
			return err
		}
	}
	return nil
}

// GCResult summarises a garbage collection pass.
type GCResult struct {
	ManifestsScanned int
	ChunksScanned    int
	ChunksDeleted    int
	BytesFreed       int64
	IndexRowsDropped int
	// ChunksSkippedRecent counts unreferenced chunks left in place because they
	// are younger than the grace window — most likely the uploads of a backup
	// that was interrupted before it could write its manifest.
	ChunksSkippedRecent int
	BytesSkippedRecent  int64
}

// GC deletes chunks on the target that no manifest references any more and
// removes them from the chunk index.
func (e *Engine) GC(ctx context.Context) (GCResult, error) {
	var res GCResult
	manifests, err := e.s3.List(ctx, ManifestPrefix)
	if err != nil {
		return res, err
	}
	referenced := make(map[string]struct{}, 1024)
	for _, o := range manifests {
		if !strings.HasSuffix(o.Key, ".json") {
			continue
		}
		raw, err := e.s3.GetBytes(ctx, o.Key)
		if err != nil {
			if errors.Is(err, s3target.ErrNotFound) {
				continue
			}
			return res, err
		}
		var m Manifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return res, fmt.Errorf("engine: gc decode %s: %w", o.Key, err)
		}
		res.ManifestsScanned++
		for _, d := range m.Disks {
			for _, c := range d.Chunks {
				referenced[c.Sha256] = struct{}{}
			}
		}
	}
	chunks, err := e.s3.List(ctx, ChunkPrefix)
	if err != nil {
		return res, err
	}
	indexed, err := e.idx.ChunkAddedAt(ctx, e.targetID)
	if err != nil {
		return res, fmt.Errorf("engine: gc chunk index: %w", err)
	}
	now := time.Now()
	present := make(map[string]struct{}, len(chunks))
	for _, o := range chunks {
		res.ChunksScanned++
		sha := strings.TrimPrefix(o.Key, ChunkPrefix)
		present[sha] = struct{}{}
		if _, ok := referenced[sha]; ok {
			continue
		}
		// A run that was interrupted (cancelled, crashed, server restarted for an
		// update) has uploaded chunks that no manifest references yet. Deleting
		// them would make the retry re-upload everything from scratch, so any
		// chunk younger than the grace window is left alone; it is still indexed,
		// so the retry deduplicates against it.
		if age, ok := e.chunkAge(now, sha, indexed, o); ok && age < e.gcGrace {
			res.ChunksSkippedRecent++
			res.BytesSkippedRecent += o.Size
			continue
		}
		if err := e.s3.Delete(ctx, o.Key); err != nil {
			return res, err
		}
		if err := e.idx.DeleteChunk(ctx, e.targetID, sha); err != nil {
			return res, fmt.Errorf("engine: gc chunk index: %w", err)
		}
		res.ChunksDeleted++
		res.BytesFreed += o.Size
	}
	// Reconcile the other direction: index rows whose chunk is gone from the
	// bucket (lifecycle rules, out-of-band deletion) would make dedup skip
	// uploads forever and produce unrestorable backups. Drop them so the next
	// run re-uploads. Chunks spared by the grace window are present, so they keep
	// their rows.
	for sha := range indexed {
		if _, ok := present[sha]; ok {
			continue
		}
		if err := e.idx.DeleteChunk(ctx, e.targetID, sha); err != nil {
			return res, fmt.Errorf("engine: gc chunk index: %w", err)
		}
		res.IndexRowsDropped++
	}
	e.log.Info("garbage collection finished", "target", e.targetID,
		"manifests", res.ManifestsScanned, "chunksScanned", res.ChunksScanned,
		"chunksDeleted", res.ChunksDeleted, "bytesFreed", res.BytesFreed,
		"indexRowsDropped", res.IndexRowsDropped,
		"chunksSkippedRecent", res.ChunksSkippedRecent,
		"bytesSkippedRecent", res.BytesSkippedRecent)
	return res, nil
}

// chunkAge reports how long ago a chunk was stored, and whether that could be
// established at all. The chunk index is the source of truth — it is written the
// moment a PUT succeeds — and the object's LastModified is only a fallback for
// chunks with no index row, because S3 implementations vary in how (and whether)
// they report it.
func (e *Engine) chunkAge(now time.Time, sha string, indexed map[string]time.Time, o s3target.Object) (time.Duration, bool) {
	if e.gcGrace <= 0 {
		return 0, false
	}
	if ts, ok := indexed[sha]; ok && !ts.IsZero() {
		return now.Sub(ts), true
	}
	if !o.LastModified.IsZero() {
		return now.Sub(o.LastModified), true
	}
	return 0, false
}

// SyncChunkIndex rebuilds the per-target chunk index from the objects present on
// the target. It is used after an index loss. As with the HEAD fallback, rebuilt
// rows carry the stored object size rather than the raw chunk size, and are
// stamped with the time of the rebuild — which also gives them one grace window
// of protection from collection.
func (e *Engine) SyncChunkIndex(ctx context.Context) (int, error) {
	objs, err := e.s3.List(ctx, ChunkPrefix)
	if err != nil {
		return 0, err
	}
	for _, o := range objs {
		sha := strings.TrimPrefix(o.Key, ChunkPrefix)
		if err := e.idx.AddChunk(ctx, e.targetID, sha, o.Size); err != nil {
			return 0, fmt.Errorf("engine: sync chunk index: %w", err)
		}
	}
	return len(objs), nil
}
