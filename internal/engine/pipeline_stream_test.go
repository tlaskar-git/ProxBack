package engine_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"proxback/internal/engine"
	"proxback/internal/s3sim"
	"proxback/internal/s3target"
	"proxback/internal/store"
)

// s3Stub wraps the S3 simulator so a test can stagger chunk uploads — which is
// what makes them complete out of stream order — and fail one specific chunk.
type s3Stub struct {
	next http.Handler

	mu        sync.Mutex
	putDelay  time.Duration
	stagger   time.Duration
	failChunk string
	puts      int
}

func (s *s3Stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/"+engine.ChunkPrefix) {
		s.mu.Lock()
		s.puts++
		delay, stagger, fail := s.putDelay, s.stagger, s.failChunk
		s.mu.Unlock()
		if fail != "" && strings.HasSuffix(r.URL.Path, fail) {
			http.Error(w, "injected upload failure", http.StatusInternalServerError)
			return
		}
		if stagger > 0 {
			delay += rand.N(stagger) //nolint:gosec // test jitter, not crypto
		}
		if delay > 0 {
			time.Sleep(delay)
		}
	}
	s.next.ServeHTTP(w, r)
}

func (s *s3Stub) putCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts
}

func (s *s3Stub) failChunkKey(sha string) {
	s.mu.Lock()
	s.failChunk = sha
	s.mu.Unlock()
}

// newStubEngine builds an engine whose target is the S3 simulator behind a stub
// that can delay and fail uploads.
func newStubEngine(t *testing.T, opts engine.Options) (*engine.Engine, *s3target.Client, *store.Store, *s3Stub) {
	t.Helper()
	ctx := context.Background()

	sim, err := s3sim.New("")
	if err != nil {
		t.Fatalf("s3 sim: %v", err)
	}
	t.Cleanup(func() { _ = sim.Close() })
	stub := &s3Stub{next: sim.Handler}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)

	client, err := s3target.New(ctx, s3target.Config{
		Endpoint: srv.URL, Region: "us-east-1", Bucket: "proxback-pipeline",
		AccessKey: "key", SecretKey: "secret", PathStyle: true,
	})
	if err != nil {
		t.Fatalf("s3 client: %v", err)
	}
	if err := client.EnsureBucket(ctx); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}
	st, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return engine.NewWithOptions(client, targetID, st, nil, opts), client, st, stub
}

// compressibleBytes builds content zstd can shrink substantially without being a
// single run of one byte, the way a real filesystem or VMA stream is.
func compressibleBytes(n int) []byte {
	pattern := []byte("ProxBack v0.3.2 chunk payload: repetitive, structured, very compressible. ")
	out := make([]byte, 0, n+len(pattern))
	for len(out) < n {
		out = append(out, pattern...)
	}
	return out[:n]
}

// restoreAll reassembles a disk manifest into memory.
func restoreAll(t *testing.T, eng *engine.Engine, dm engine.DiskManifest) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := eng.NewSession(dm.SizeBytes, nil).RestoreDisk(context.Background(), dm, &buf); err != nil {
		t.Fatalf("restore %s: %v", dm.Name, err)
	}
	return buf.Bytes()
}

// ---------------------------------------------------------------- ordering

// TestBackupStreamKeepsStreamOrderWhenUploadsFinishOutOfOrder is the central
// guarantee of the parallel pipeline: the manifest is the stream's order, not the
// order the uploads happened to complete in.
func TestBackupStreamKeepsStreamOrderWhenUploadsFinishOutOfOrder(t *testing.T) {
	ctx := context.Background()
	eng, _, _, stub := newStubEngine(t, engine.Options{UploadConcurrency: 8})
	// Randomised per-upload latency: with 8 workers in flight the completion
	// order is essentially guaranteed to differ from the read order.
	stub.mu.Lock()
	stub.stagger = 25 * time.Millisecond
	stub.mu.Unlock()

	// 12 chunks of distinct content plus a short tail, so a swapped chunk cannot
	// hide behind identical sizes.
	const chunks = 12
	var data []byte
	for i := 0; i < chunks; i++ {
		data = append(data, deterministicBytes(engine.ChunkSize, uint64(1000+i))...)
	}
	data = append(data, deterministicBytes(123456, 77)...)

	sess := eng.NewSession(int64(len(data)), nil)
	dm, err := sess.BackupStream(ctx, "scsi0", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if len(dm.Chunks) != chunks+1 {
		t.Fatalf("manifest holds %d chunks, want %d", len(dm.Chunks), chunks+1)
	}
	// Every chunk must sit exactly where the stream put it, with its exact size.
	off := 0
	for i, c := range dm.Chunks {
		want := engine.ChunkSize
		if i == chunks {
			want = 123456
		}
		if c.Size != int64(want) {
			t.Fatalf("chunk %d size = %d, want %d", i, c.Size, want)
		}
		if got := shaOf(data[off : off+want]); got != c.Sha256 {
			t.Fatalf("chunk %d is out of order: manifest says %s, stream has %s", i, c.Sha256, got)
		}
		off += want
	}
	if dm.SizeBytes != int64(len(data)) {
		t.Fatalf("disk size = %d, want %d", dm.SizeBytes, len(data))
	}
	if st := sess.Stats(); st.BytesProcessed != int64(len(data)) || st.ChunksTotal != chunks+1 {
		t.Fatalf("stats = %+v", st)
	}
	if got := stub.putCount(); got < chunks+1 {
		t.Fatalf("only %d chunk uploads reached the target", got)
	}
	// And the ultimate proof of ordering: the restore reproduces the input.
	if !bytes.Equal(restoreAll(t, eng, dm), data) {
		t.Fatal("restored stream differs from the input")
	}
}

// TestConcurrencyDoesNotChangeTheResult pins the invariant that makes the
// concurrent pipeline safe to ship: whatever the worker count, the manifest and
// every byte counter come out identical — including for a stream full of
// duplicate chunks, where concurrent workers must coalesce instead of both
// uploading.
func TestConcurrencyDoesNotChangeTheResult(t *testing.T) {
	ctx := context.Background()

	// Half distinct content, half repeated: chunk 0 recurs three times.
	repeat := deterministicBytes(engine.ChunkSize, 5)
	var data []byte
	data = append(data, repeat...)
	data = append(data, deterministicBytes(engine.ChunkSize, 6)...)
	data = append(data, repeat...)
	data = append(data, deterministicBytes(engine.ChunkSize, 7)...)
	data = append(data, repeat...)
	data = append(data, deterministicBytes(1<<20, 8)...)

	type outcome struct {
		chunks []engine.Chunk
		size   int64
		stats  engine.Stats
	}
	run := func(workers int) outcome {
		eng, _, _, _ := newStubEngine(t, engine.Options{UploadConcurrency: workers})
		sess := eng.NewSession(int64(len(data)), nil)
		dm, err := sess.BackupStream(ctx, "scsi0", bytes.NewReader(data))
		if err != nil {
			t.Fatalf("backup with %d workers: %v", workers, err)
		}
		if !bytes.Equal(restoreAll(t, eng, dm), data) {
			t.Fatalf("restore with %d workers differs from the input", workers)
		}
		return outcome{chunks: dm.Chunks, size: dm.SizeBytes, stats: sess.Stats()}
	}

	serial := run(1)
	for _, workers := range []int{2, 4, 8, 16} {
		got := run(workers)
		if len(got.chunks) != len(serial.chunks) {
			t.Fatalf("%d workers produced %d chunks, serial produced %d", workers, len(got.chunks), len(serial.chunks))
		}
		for i := range serial.chunks {
			if got.chunks[i] != serial.chunks[i] {
				t.Fatalf("%d workers: chunk %d = %+v, serial = %+v", workers, i, got.chunks[i], serial.chunks[i])
			}
		}
		if got.size != serial.size {
			t.Fatalf("%d workers: disk size %d, serial %d", workers, got.size, serial.size)
		}
		if got.stats.BytesProcessed != serial.stats.BytesProcessed ||
			got.stats.BytesUploaded != serial.stats.BytesUploaded ||
			got.stats.BytesDeduped != serial.stats.BytesDeduped ||
			got.stats.ChunksTotal != serial.stats.ChunksTotal ||
			got.stats.ChunksUploaded != serial.stats.ChunksUploaded ||
			got.stats.ChunksDeduped != serial.stats.ChunksDeduped {
			t.Fatalf("%d workers: stats %+v, serial %+v", workers, got.stats, serial.stats)
		}
	}
	// The duplicated chunk must have been uploaded once and deduplicated twice,
	// which is exactly what a serial pipeline would have done.
	if serial.stats.ChunksUploaded != 4 || serial.stats.ChunksDeduped != 2 {
		t.Fatalf("baseline accounting = %+v, want 4 uploads / 2 dedups", serial.stats)
	}
}

// TestBackupStreamAbortsOnTheFirstUploadError proves a failing target fails the
// stream (with the underlying error) instead of yielding a short manifest, and
// that nothing is left running afterwards.
func TestBackupStreamAbortsOnTheFirstUploadError(t *testing.T) {
	ctx := context.Background()
	eng, _, _, stub := newStubEngine(t, engine.Options{UploadConcurrency: 4})

	data := deterministicBytes(10*engine.ChunkSize, 31)
	// Fail the 4th chunk: earlier and later chunks are in flight around it.
	stub.failChunkKey(shaOf(data[3*engine.ChunkSize : 4*engine.ChunkSize]))

	goroutinesBefore := runtime.NumGoroutine()
	sess := eng.NewSession(int64(len(data)), nil)
	dm, err := sess.BackupStream(ctx, "scsi0", bytes.NewReader(data))
	if err == nil {
		t.Fatal("backup succeeded despite a failing target")
	}
	if !strings.Contains(err.Error(), "store chunk 3") {
		t.Fatalf("error = %v, want it to name the failing chunk", err)
	}
	if !strings.Contains(err.Error(), "s3") && !strings.Contains(err.Error(), "500") &&
		!strings.Contains(err.Error(), "StatusCode") && !strings.Contains(err.Error(), "InternalError") {
		t.Fatalf("error = %v, want the target's failure wrapped in it", err)
	}
	if len(dm.Chunks) != 0 {
		t.Fatalf("failed stream still returned %d chunks", len(dm.Chunks))
	}
	// Every worker must have joined: no goroutine may outlive the call.
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > goroutinesBefore+2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if leaked := runtime.NumGoroutine() - goroutinesBefore; leaked > 2 {
		t.Fatalf("%d goroutines outlived the failed stream", leaked)
	}

	// The engine is still usable: a stream that avoids the poisoned chunk works.
	stub.failChunkKey("")
	good := deterministicBytes(2*engine.ChunkSize, 32)
	if _, err := eng.NewSession(int64(len(good)), nil).
		BackupStream(ctx, "scsi1", bytes.NewReader(good)); err != nil {
		t.Fatalf("backup after a failure: %v", err)
	}
}

// TestBackupStreamRespectsCancellation makes sure an operator's cancel still
// stops the pipeline promptly and reports the cancellation, not some derived
// upload error.
func TestBackupStreamRespectsCancellation(t *testing.T) {
	eng, _, _, stub := newStubEngine(t, engine.Options{UploadConcurrency: 2})
	stub.mu.Lock()
	stub.putDelay = 50 * time.Millisecond
	stub.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	data := deterministicBytes(20*engine.ChunkSize, 41)
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()
	_, err := eng.NewSession(int64(len(data)), nil).BackupStream(ctx, "scsi0", bytes.NewReader(data))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled backup returned %v, want context.Canceled", err)
	}
}

// TestParallelUploadsAreFasterThanSerialOnes is the reason this pipeline exists:
// with a target that has latency (a real uplink), overlapping uploads has to beat
// doing them one at a time.
func TestParallelUploadsAreFasterThanSerialOnes(t *testing.T) {
	ctx := context.Background()
	const delay = 30 * time.Millisecond
	const chunks = 12
	data := make([]byte, 0, chunks*engine.ChunkSize)
	for i := 0; i < chunks; i++ {
		data = append(data, deterministicBytes(engine.ChunkSize, uint64(3000+i))...)
	}

	measure := func(workers int) time.Duration {
		eng, _, _, stub := newStubEngine(t, engine.Options{UploadConcurrency: workers})
		stub.mu.Lock()
		stub.putDelay = delay
		stub.mu.Unlock()
		start := time.Now()
		if _, err := eng.NewSession(int64(len(data)), nil).
			BackupStream(ctx, "scsi0", bytes.NewReader(data)); err != nil {
			t.Fatalf("backup with %d workers: %v", workers, err)
		}
		return time.Since(start)
	}

	serial := measure(1)
	parallel := measure(6)
	t.Logf("%d chunks at %v per upload: 1 worker %v, 6 workers %v (%.1fx)",
		chunks, delay, serial.Round(time.Millisecond), parallel.Round(time.Millisecond),
		float64(serial)/float64(parallel))
	if parallel*2 > serial {
		t.Fatalf("6 workers took %v against %v serial: the pipeline is not overlapping uploads", parallel, serial)
	}
}

// ---------------------------------------------------------------- compression

func TestCompressionRoundTripAndAccounting(t *testing.T) {
	ctx := context.Background()
	eng, client, st, _ := newStubEngine(t, engine.Options{Compression: engine.CompressionZstd})

	data := compressibleBytes(9 << 20)
	sess := eng.NewSession(int64(len(data)), nil)
	dm, err := sess.BackupStream(ctx, "scsi0", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	stats := sess.Stats()
	if stats.BytesProcessed != int64(len(data)) {
		t.Fatalf("processed %d bytes, want the raw %d", stats.BytesProcessed, len(data))
	}
	if stats.BytesUploaded >= stats.BytesProcessed/2 {
		t.Fatalf("uploaded %d of %d raw bytes: compressible content was not compressed",
			stats.BytesUploaded, stats.BytesProcessed)
	}

	// The manifest still describes raw chunks: sizes and the total are raw bytes,
	// and the keys are the hashes of the raw chunks.
	if dm.SizeBytes != int64(len(data)) {
		t.Fatalf("manifest size = %d, want the raw %d", dm.SizeBytes, len(data))
	}
	off := 0
	for i, c := range dm.Chunks {
		if got := shaOf(data[off : off+int(c.Size)]); got != c.Sha256 {
			t.Fatalf("chunk %d is not addressed by its raw content", i)
		}
		off += int(c.Size)
		stored, err := client.GetBytes(ctx, engine.ChunkKey(c.Sha256))
		if err != nil {
			t.Fatalf("read stored chunk %d: %v", i, err)
		}
		if !bytes.HasPrefix(stored, []byte("PBZ1")) {
			t.Fatalf("stored chunk %d is not PBZ1 framed", i)
		}
		if int64(len(stored)) >= c.Size {
			t.Fatalf("stored chunk %d is %d bytes for %d raw", i, len(stored), c.Size)
		}
	}
	// The dedup index keeps counting raw bytes: it indexes stream content.
	if count, indexed, err := st.ChunkStats(ctx, targetID); err != nil ||
		count != len(dm.Chunks) || indexed != int64(len(data)) {
		t.Fatalf("chunk index = %d chunks / %d bytes (%v), want %d / %d",
			count, indexed, err, len(dm.Chunks), len(data))
	}

	if !bytes.Equal(restoreAll(t, eng, dm), data) {
		t.Fatal("restore of a compressed backup differs from the source")
	}
	// Verification walks the same decompression path.
	man := &engine.Manifest{
		BackupID: "b-zstd", SourceKind: store.SourceVM, SourceID: "s1", TargetID: targetID,
		SizeBytes: dm.SizeBytes, Disks: []engine.DiskManifest{dm},
	}
	if err := eng.NewSession(man.TotalSize(), nil).VerifyBackup(ctx, man); err != nil {
		t.Fatalf("verify a compressed backup: %v", err)
	}
}

// TestIncompressibleChunksAreStoredRaw covers already compressed or encrypted
// disks: spending CPU to make the object bigger would be the worst of both.
func TestIncompressibleChunksAreStoredRaw(t *testing.T) {
	ctx := context.Background()
	eng, client, _, _ := newStubEngine(t, engine.Options{Compression: engine.CompressionZstd})

	data := deterministicBytes(6<<20, 4242)
	sess := eng.NewSession(int64(len(data)), nil)
	dm, err := sess.BackupStream(ctx, "scsi0", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if up := sess.Stats().BytesUploaded; up != int64(len(data)) {
		t.Fatalf("uploaded %d bytes of incompressible data, want the raw %d", up, len(data))
	}
	for i, c := range dm.Chunks {
		stored, err := client.GetBytes(ctx, engine.ChunkKey(c.Sha256))
		if err != nil {
			t.Fatalf("read stored chunk %d: %v", i, err)
		}
		if bytes.HasPrefix(stored, []byte("PBZ1")) {
			t.Fatalf("incompressible chunk %d was stored compressed", i)
		}
		if int64(len(stored)) != c.Size {
			t.Fatalf("stored chunk %d is %d bytes, raw is %d", i, len(stored), c.Size)
		}
	}
	if !bytes.Equal(restoreAll(t, eng, dm), data) {
		t.Fatal("restore differs from the source")
	}
}

// TestMixedRawAndCompressedChunksRestore is the upgrade path: a manifest written
// after compression was enabled routinely references chunks uploaded before it,
// and both forms have to reassemble into one stream.
func TestMixedRawAndCompressedChunksRestore(t *testing.T) {
	ctx := context.Background()
	off, client, st, _ := newStubEngine(t, engine.Options{Compression: engine.CompressionOff})

	// First half uploaded with compression off (stored raw, pre-v0.3.2 style).
	first := compressibleBytes(8 << 20)
	if _, err := off.NewSession(int64(len(first)), nil).
		BackupStream(ctx, "scsi0", bytes.NewReader(first)); err != nil {
		t.Fatalf("uncompressed backup: %v", err)
	}

	// The same target, now with compression on, backs up the first half followed
	// by new content: the old chunks dedup as raw, the new ones are compressed.
	on := engine.NewWithOptions(client, targetID, st, nil, engine.Options{Compression: engine.CompressionZstd})
	full := append(append([]byte(nil), first...), compressibleBytes(8<<20+7777)...)
	sess := on.NewSession(int64(len(full)), nil)
	dm, err := sess.BackupStream(ctx, "scsi0", bytes.NewReader(full))
	if err != nil {
		t.Fatalf("compressed backup: %v", err)
	}

	raw, compressed := 0, 0
	for _, c := range dm.Chunks {
		stored, err := client.GetBytes(ctx, engine.ChunkKey(c.Sha256))
		if err != nil {
			t.Fatalf("read stored chunk: %v", err)
		}
		if bytes.HasPrefix(stored, []byte("PBZ1")) {
			compressed++
		} else {
			raw++
		}
	}
	if raw == 0 || compressed == 0 {
		t.Fatalf("manifest is not mixed: %d raw / %d compressed chunks", raw, compressed)
	}
	if !bytes.Equal(restoreAll(t, on, dm), full) {
		t.Fatal("restore of a mixed manifest differs from the source")
	}
	// And the other direction: an engine with compression off restores compressed
	// chunks just as well — the stored form is self describing.
	if !bytes.Equal(restoreAll(t, off, dm), full) {
		t.Fatal("restore of a mixed manifest with compression off differs from the source")
	}
}

// TestRawChunkStartingWithTheMagicStillRestores covers the one ambiguity the
// magic introduces: raw data that happens to begin with "PBZ1". Decompression
// fails, the reader falls back to raw, and the SHA-256 check confirms it.
func TestRawChunkStartingWithTheMagicStillRestores(t *testing.T) {
	ctx := context.Background()
	// Compression off, so the bytes reach the bucket verbatim — magic and all.
	eng, client, st, _ := newStubEngine(t, engine.Options{Compression: engine.CompressionOff})

	data := append([]byte("PBZ1"), deterministicBytes(2<<20, 909)...)
	dm, err := eng.NewSession(int64(len(data)), nil).BackupStream(ctx, "scsi0", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	stored, err := client.GetBytes(ctx, engine.ChunkKey(dm.Chunks[0].Sha256))
	if err != nil {
		t.Fatalf("read stored chunk: %v", err)
	}
	if !bytes.HasPrefix(stored, []byte("PBZ1")) {
		t.Fatal("precondition: the stored chunk should start with the magic")
	}
	if !bytes.Equal(restoreAll(t, eng, dm), data) {
		t.Fatal("a raw chunk carrying the magic did not restore")
	}
	// A reader with compression enabled must reach the same conclusion.
	on := engine.NewWithOptions(client, targetID, st, nil, engine.Options{Compression: engine.CompressionZstd})
	if !bytes.Equal(restoreAll(t, on, dm), data) {
		t.Fatal("a raw chunk carrying the magic did not restore with compression on")
	}
}

// TestDedupIsIndependentOfTheCompressionSetting is what makes the setting safe to
// flip on a live installation: identity is the raw chunk, so nothing re-uploads.
func TestDedupIsIndependentOfTheCompressionSetting(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct{ name, first, second string }{
		{"off-then-zstd", engine.CompressionOff, engine.CompressionZstd},
		{"zstd-then-off", engine.CompressionZstd, engine.CompressionOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng, client, st, _ := newStubEngine(t, engine.Options{Compression: tc.first})
			data := compressibleBytes(12 << 20)

			first := eng.NewSession(int64(len(data)), nil)
			dm1, err := first.BackupStream(ctx, "scsi0", bytes.NewReader(data))
			if err != nil {
				t.Fatalf("first backup: %v", err)
			}
			if first.Stats().BytesUploaded == 0 {
				t.Fatal("first backup uploaded nothing")
			}

			flipped := engine.NewWithOptions(client, targetID, st, nil, engine.Options{Compression: tc.second})
			second := flipped.NewSession(int64(len(data)), nil)
			dm2, err := second.BackupStream(ctx, "scsi0", bytes.NewReader(data))
			if err != nil {
				t.Fatalf("second backup: %v", err)
			}
			if up := second.Stats().BytesUploaded; up != 0 {
				t.Fatalf("changing compression re-uploaded %d bytes; the dedup index must not care", up)
			}
			if second.Stats().DedupRatio() != 1 {
				t.Fatalf("dedup ratio = %v, want 1", second.Stats().DedupRatio())
			}
			if len(dm1.Chunks) != len(dm2.Chunks) {
				t.Fatalf("chunk counts differ: %d then %d", len(dm1.Chunks), len(dm2.Chunks))
			}
			for i := range dm1.Chunks {
				if dm1.Chunks[i] != dm2.Chunks[i] {
					t.Fatalf("chunk %d differs after flipping compression", i)
				}
			}
			if !bytes.Equal(restoreAll(t, flipped, dm2), data) {
				t.Fatal("restore after flipping compression differs from the source")
			}
		})
	}
}

// ---------------------------------------------------------------- rate limit

// TestUploadRateLimitCapsThroughput checks the token bucket actually holds the
// uplink down, and that a single chunk larger than a second's worth of tokens
// cannot wedge it.
func TestUploadRateLimitCapsThroughput(t *testing.T) {
	ctx := context.Background()
	eng, _, _, _ := newStubEngine(t, engine.Options{UploadConcurrency: 4, Compression: engine.CompressionOff})

	// 100 Mbps = 12.5 MB/s. The bucket's burst is one chunk, so 24 MiB of
	// incompressible data must take at least (24 MiB - 4 MiB) / 12.5 MB/s ≈ 1.6 s.
	const mbps = 100
	engine.SetUploadLimitMbps(mbps)
	t.Cleanup(func() { engine.SetUploadLimitMbps(0) })
	if got := engine.UploadLimitMbps(); got != mbps {
		t.Fatalf("UploadLimitMbps = %v, want %d", got, mbps)
	}

	data := deterministicBytes(24<<20, 5150)
	start := time.Now()
	sess := eng.NewSession(int64(len(data)), nil)
	if _, err := sess.BackupStream(ctx, "scsi0", bytes.NewReader(data)); err != nil {
		t.Fatalf("rate limited backup: %v", err)
	}
	elapsed := time.Since(start)
	uploaded := sess.Stats().BytesUploaded
	bytesPerSec := float64(mbps) * 1e6 / 8
	// Generous floor: the burst plus scheduling slack are on the test's side.
	floor := time.Duration(float64(uploaded-int64(engine.ChunkSize)) / bytesPerSec * 0.8 * float64(time.Second))
	t.Logf("uploaded %d bytes at %d Mbps in %v (floor %v)", uploaded, mbps, elapsed.Round(time.Millisecond), floor)
	if elapsed < floor {
		t.Fatalf("uploading %d bytes at %d Mbps took %v, expected at least %v", uploaded, mbps, elapsed, floor)
	}

	// Unlimited again: the same stream must not be throttled, and a limit smaller
	// than one chunk must still make progress rather than deadlock.
	engine.SetUploadLimitMbps(1)
	small := deterministicBytes(engine.ChunkSize, 6161)
	done := make(chan error, 1)
	go func() {
		_, err := eng.NewSession(int64(len(small)), nil).
			BackupStream(ctx, "scsi1", bytes.NewReader(small))
		done <- err
	}()
	select {
	case err := <-done:
		// 1 Mbps with a one-chunk burst: the first chunk goes out immediately.
		if err != nil {
			t.Fatalf("backup under a tiny limit: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a chunk larger than the rate limit's per-second budget deadlocked the upload")
	}
}

// ---------------------------------------------------------------- gc grace

// TestGCGraceKeepsAnInterruptedBackupResumable is the regression test for the
// production incident where a run interrupted by a server restart had its 24.8 GB
// of already uploaded chunks collected as orphans, forcing a full re-upload.
func TestGCGraceKeepsAnInterruptedBackupResumable(t *testing.T) {
	ctx := context.Background()
	eng, client, st, _ := newStubEngine(t, engine.Options{UploadConcurrency: 4})

	data := deterministicBytes(20<<20, 8181)
	// An interrupted run: chunks are uploaded and indexed, but the process dies
	// before the manifest is written, so nothing references them.
	interrupted := eng.NewSession(int64(len(data)), nil)
	dm, err := interrupted.BackupStream(ctx, "scsi0", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("interrupted backup: %v", err)
	}
	uploaded := interrupted.Stats().BytesUploaded
	if uploaded != int64(len(data)) {
		t.Fatalf("first pass uploaded %d of %d bytes", uploaded, len(data))
	}

	res, err := eng.GC(ctx)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if res.ChunksDeleted != 0 || res.BytesFreed != 0 {
		t.Fatalf("gc deleted %d recently uploaded chunks (%d bytes); an interrupted backup must survive it",
			res.ChunksDeleted, res.BytesFreed)
	}
	if res.ChunksSkippedRecent != len(dm.Chunks) || res.BytesSkippedRecent != int64(len(data)) {
		t.Fatalf("gc = %+v, want %d chunks / %d bytes reported as recent",
			res, len(dm.Chunks), len(data))
	}
	// Requirement: they stay indexed, so the retry deduplicates against them.
	if res.IndexRowsDropped != 0 {
		t.Fatalf("gc dropped %d index rows for chunks that are still on the target", res.IndexRowsDropped)
	}
	for _, c := range dm.Chunks {
		if has, err := st.HasChunk(ctx, targetID, c.Sha256); err != nil || !has {
			t.Fatalf("chunk %s lost its index row: %v, %v", c.Sha256, has, err)
		}
		if _, ok, err := client.Head(ctx, engine.ChunkKey(c.Sha256)); err != nil || !ok {
			t.Fatalf("chunk %s is gone from the target: %v, %v", c.Sha256, ok, err)
		}
	}

	// The retry re-uploads (almost) nothing and produces a restorable manifest.
	retry := eng.NewSession(int64(len(data)), nil)
	dm2, err := retry.BackupStream(ctx, "scsi0", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("retry backup: %v", err)
	}
	if up := retry.Stats().BytesUploaded; up != 0 {
		t.Fatalf("the retry re-uploaded %d bytes; the interrupted run's work was wasted", up)
	}
	man := &engine.Manifest{
		BackupID: "b-resumed", SourceKind: store.SourceVM, SourceID: "s1", TargetID: targetID,
		SizeBytes: dm2.SizeBytes, Disks: []engine.DiskManifest{dm2},
	}
	if err := eng.WriteManifest(ctx, man); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if !bytes.Equal(restoreAll(t, eng, dm2), data) {
		t.Fatal("the resumed backup does not restore byte-identically")
	}
	// Now that a manifest references them, a pass collects nothing at all.
	if res, err := eng.GC(ctx); err != nil || res.ChunksDeleted != 0 || res.ChunksSkippedRecent != 0 {
		t.Fatalf("gc after the retry = %+v (%v), want a clean pass", res, err)
	}
}

// TestGCStillCollectsChunksOlderThanTheGraceWindow keeps retention honest: the
// grace window delays collection, it does not disable it.
func TestGCStillCollectsChunksOlderThanTheGraceWindow(t *testing.T) {
	ctx := context.Background()
	eng, _, st, _ := newStubEngine(t, engine.Options{})

	data := deterministicBytes(8<<20, 2727)
	dm, err := eng.NewSession(int64(len(data)), nil).BackupStream(ctx, "scsi0", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if len(dm.Chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(dm.Chunks))
	}
	// Age the first chunk past the window; leave the second one fresh.
	old := time.Now().Add(-2 * engine.DefaultGCGrace)
	if err := st.SetChunkAddedAt(ctx, targetID, dm.Chunks[0].Sha256, old); err != nil {
		t.Fatalf("age chunk: %v", err)
	}

	res, err := eng.GC(ctx)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if res.ChunksDeleted != 1 || res.BytesFreed != int64(engine.ChunkSize) {
		t.Fatalf("gc = %+v, want the aged chunk collected", res)
	}
	if res.ChunksSkippedRecent != 1 {
		t.Fatalf("gc = %+v, want the fresh chunk spared", res)
	}
	if has, err := st.HasChunk(ctx, targetID, dm.Chunks[0].Sha256); err != nil || has {
		t.Fatalf("the collected chunk kept its index row: %v, %v", has, err)
	}
}

// TestGCGraceCanBeDisabled documents the option the scheduler passes through for
// installations (and tests) that want immediate collection.
func TestGCGraceCanBeDisabled(t *testing.T) {
	ctx := context.Background()
	eng, _, st, _ := newStubEngine(t, engine.Options{GCGrace: -1})
	if eng.GCGrace() != 0 {
		t.Fatalf("GCGrace = %v, want the grace window disabled", eng.GCGrace())
	}
	data := deterministicBytes(4<<20, 3131)
	dm, err := eng.NewSession(int64(len(data)), nil).BackupStream(ctx, "scsi0", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	res, err := eng.GC(ctx)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if res.ChunksDeleted != 1 || res.ChunksSkippedRecent != 0 {
		t.Fatalf("gc = %+v, want the fresh orphan collected", res)
	}
	if has, _ := st.HasChunk(ctx, targetID, dm.Chunks[0].Sha256); has {
		t.Fatal("collected chunk kept its index row")
	}
}

// TestOptionsAreNormalised proves a nonsense setting can never break a backup.
func TestOptionsAreNormalised(t *testing.T) {
	cases := []struct {
		in          engine.Options
		wantWorkers int
		wantZstd    bool
		wantGrace   time.Duration
	}{
		{engine.Options{}, engine.DefaultUploadConcurrency, true, engine.DefaultGCGrace},
		{engine.Options{UploadConcurrency: 0}, engine.DefaultUploadConcurrency, true, engine.DefaultGCGrace},
		{engine.Options{UploadConcurrency: -5}, engine.DefaultUploadConcurrency, true, engine.DefaultGCGrace},
		{engine.Options{UploadConcurrency: 99}, engine.MaxUploadConcurrency, true, engine.DefaultGCGrace},
		{engine.Options{UploadConcurrency: 7, Compression: engine.CompressionOff}, 7, false, engine.DefaultGCGrace},
		{engine.Options{Compression: "nonsense"}, engine.DefaultUploadConcurrency, true, engine.DefaultGCGrace},
		{engine.Options{GCGrace: time.Minute}, engine.DefaultUploadConcurrency, true, time.Minute},
	}
	for i, tc := range cases {
		e := engine.NewWithOptions(nil, "t", nil, nil, tc.in)
		if e.UploadConcurrency() != tc.wantWorkers {
			t.Errorf("case %d: workers = %d, want %d", i, e.UploadConcurrency(), tc.wantWorkers)
		}
		if e.CompressionEnabled() != tc.wantZstd {
			t.Errorf("case %d: compression = %v, want %v", i, e.CompressionEnabled(), tc.wantZstd)
		}
		if e.GCGrace() != tc.wantGrace {
			t.Errorf("case %d: gc grace = %v, want %v", i, e.GCGrace(), tc.wantGrace)
		}
	}
}

// shaOf is the content address of a chunk: the hash of its RAW bytes, whatever
// form it ends up stored in.
func shaOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
