package engine_test

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"proxback/internal/blobstore"
	"proxback/internal/engine"
	"proxback/internal/s3sim"
	"proxback/internal/s3target"
	"proxback/internal/store"
)

const targetID = "target-1"

// backend is one blobstore.Store implementation. Every engine behaviour below is
// asserted against all of them: dedup, compression, manifests, restore,
// verification and orphan collection are supposed to be identical whether a target
// is object storage or a mounted path, and the only way to keep that true is to run
// the same expectations over both.
type backend struct {
	name string
	open func(t *testing.T) blobstore.Store
}

func backends() []backend {
	return []backend{
		{"s3", openS3Store},
		{"filesystem", openFilesystemStore},
	}
}

// openS3Store starts the S3 simulator and returns a client for it.
func openS3Store(t *testing.T) blobstore.Store {
	t.Helper()
	ctx := context.Background()

	sim, err := s3sim.New("")
	if err != nil {
		t.Fatalf("s3 sim: %v", err)
	}
	t.Cleanup(func() { _ = sim.Close() })
	srv := httptest.NewServer(sim.Handler)
	t.Cleanup(srv.Close)

	client, err := s3target.New(ctx, s3target.Config{
		Endpoint: srv.URL, Region: "us-east-1", Bucket: "proxback-test",
		AccessKey: "key", SecretKey: "secret", PathStyle: true,
	})
	if err != nil {
		t.Fatalf("s3 client: %v", err)
	}
	if err := client.EnsureBucket(ctx); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}
	return client
}

// openFilesystemStore returns a filesystem target rooted in a temp directory —
// the local disk, USB disk or mounted NAS share case.
func openFilesystemStore(t *testing.T) blobstore.Store {
	t.Helper()
	fs, err := blobstore.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatalf("filesystem store: %v", err)
	}
	return fs
}

// newEngineOn builds an engine over one backend with a fresh chunk index.
func newEngineOn(t *testing.T, be backend, opts engine.Options) (*engine.Engine, blobstore.Store, *store.Store) {
	t.Helper()
	bs := be.open(t)
	st, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return engine.NewWithOptions(bs, targetID, st, nil, opts), bs, st
}

// eachBackend runs fn against a freshly built engine on every store
// implementation.
func eachBackend(t *testing.T, fn func(t *testing.T, eng *engine.Engine, bs blobstore.Store, st *store.Store)) {
	t.Helper()
	eachBackendWithOptions(t, engine.Options{}, fn)
}

// eachBackendWithOptions is eachBackend for the behaviours that depend on engine
// options — compression and the orphan-collection grace window.
func eachBackendWithOptions(t *testing.T, opts engine.Options,
	fn func(t *testing.T, eng *engine.Engine, bs blobstore.Store, st *store.Store),
) {
	t.Helper()
	for _, be := range backends() {
		t.Run(be.name, func(t *testing.T) {
			eng, bs, st := newEngineOn(t, be, opts)
			fn(t, eng, bs, st)
		})
	}
}

// deterministicBytes builds reproducible test content.
func deterministicBytes(n int, seed uint64) []byte {
	out := make([]byte, n)
	x := seed
	for i := range out {
		x = x*6364136223846793005 + 1442695040888963407
		out[i] = byte(x >> 33)
	}
	return out
}

func TestTargetProbe(t *testing.T) {
	for _, be := range backends() {
		t.Run(be.name, func(t *testing.T) {
			bs := be.open(t)
			if err := bs.Test(context.Background()); err != nil {
				t.Fatalf("target probe: %v", err)
			}
		})
	}
}

func TestChunkingAndDedup(t *testing.T) {
	eachBackend(t, func(t *testing.T, eng *engine.Engine, _ blobstore.Store, st *store.Store) {
		ctx := context.Background()

		// 10 MiB: two full 4 MiB chunks plus a 2 MiB tail.
		const size = 10 << 20
		data := deterministicBytes(size, 42)

		sess := eng.NewSession(size, nil)
		dm, err := sess.BackupStream(ctx, "scsi0", bytes.NewReader(data))
		if err != nil {
			t.Fatalf("backup: %v", err)
		}
		if len(dm.Chunks) != 3 {
			t.Fatalf("chunk count = %d, want 3", len(dm.Chunks))
		}
		if dm.Chunks[0].Size != engine.ChunkSize || dm.Chunks[1].Size != engine.ChunkSize {
			t.Fatalf("full chunk sizes = %d, %d", dm.Chunks[0].Size, dm.Chunks[1].Size)
		}
		if dm.Chunks[2].Size != 2<<20 {
			t.Fatalf("tail chunk size = %d, want %d", dm.Chunks[2].Size, 2<<20)
		}
		if dm.SizeBytes != size {
			t.Fatalf("disk size = %d, want %d", dm.SizeBytes, size)
		}
		stats := sess.Stats()
		if stats.BytesProcessed != size || stats.BytesUploaded != size {
			t.Fatalf("first pass stats = %+v", stats)
		}
		if stats.ChunksUploaded != 3 || stats.ChunksDeduped != 0 {
			t.Fatalf("first pass chunk stats = %+v", stats)
		}
		if stats.Pct < 99.9 {
			t.Fatalf("progress pct = %v, want ~100", stats.Pct)
		}

		count, stored, err := st.ChunkStats(ctx, targetID)
		if err != nil {
			t.Fatalf("chunk stats: %v", err)
		}
		if count != 3 || stored != size {
			t.Fatalf("chunk index = %d chunks / %d bytes", count, stored)
		}

		// Re-running identical content must upload nothing.
		sess2 := eng.NewSession(size, nil)
		dm2, err := sess2.BackupStream(ctx, "scsi0", bytes.NewReader(data))
		if err != nil {
			t.Fatalf("second backup: %v", err)
		}
		stats2 := sess2.Stats()
		if stats2.BytesUploaded != 0 || stats2.ChunksDeduped != 3 {
			t.Fatalf("dedup pass stats = %+v", stats2)
		}
		if stats2.DedupRatio() != 1 {
			t.Fatalf("dedup ratio = %v, want 1", stats2.DedupRatio())
		}
		for i := range dm.Chunks {
			if dm.Chunks[i] != dm2.Chunks[i] {
				t.Fatalf("chunk %d differs between identical runs", i)
			}
		}

		// Changing one chunk must upload exactly that chunk.
		mutated := append([]byte(nil), data...)
		copy(mutated[0:engine.ChunkSize], deterministicBytes(engine.ChunkSize, 99))
		sess3 := eng.NewSession(size, nil)
		if _, err := sess3.BackupStream(ctx, "scsi0", bytes.NewReader(mutated)); err != nil {
			t.Fatalf("third backup: %v", err)
		}
		stats3 := sess3.Stats()
		if stats3.BytesUploaded != engine.ChunkSize || stats3.ChunksUploaded != 1 {
			t.Fatalf("incremental stats = %+v, want exactly one uploaded chunk", stats3)
		}
	})
}

func TestDedupViaHeadFallback(t *testing.T) {
	eachBackend(t, func(t *testing.T, eng *engine.Engine, _ blobstore.Store, st *store.Store) {
		ctx := context.Background()
		data := deterministicBytes(1<<20, 7)

		sess := eng.NewSession(int64(len(data)), nil)
		dm, err := sess.BackupStream(ctx, "d", bytes.NewReader(data))
		if err != nil {
			t.Fatalf("backup: %v", err)
		}
		sha := dm.Chunks[0].Sha256

		// Drop the index row: the engine must fall back to a HEAD and still dedup.
		if err := st.DeleteChunk(ctx, targetID, sha); err != nil {
			t.Fatalf("delete chunk index: %v", err)
		}
		sess2 := eng.NewSession(int64(len(data)), nil)
		if _, err := sess2.BackupStream(ctx, "d", bytes.NewReader(data)); err != nil {
			t.Fatalf("backup after index loss: %v", err)
		}
		if up := sess2.Stats().BytesUploaded; up != 0 {
			t.Fatalf("uploaded %d bytes after index loss, want 0 (HEAD fallback)", up)
		}
		if has, err := st.HasChunk(ctx, targetID, sha); err != nil || !has {
			t.Fatalf("index not repaired by HEAD fallback: %v, %v", has, err)
		}
	})
}

func TestManifestAndRestore(t *testing.T) {
	eachBackend(t, func(t *testing.T, eng *engine.Engine, bs blobstore.Store, _ *store.Store) {
		ctx := context.Background()
		data := deterministicBytes(9<<20, 11)

		sess := eng.NewSession(int64(len(data)), nil)
		dm, err := sess.BackupStream(ctx, "scsi0", bytes.NewReader(data))
		if err != nil {
			t.Fatalf("backup: %v", err)
		}
		man := &engine.Manifest{
			BackupID: "b1", SourceKind: store.SourceVM, SourceID: "host_100",
			SourceName: "web-01", TargetID: targetID, Kind: engine.KindFull,
			SizeBytes: dm.SizeBytes, Disks: []engine.DiskManifest{dm},
		}
		if err := eng.WriteManifest(ctx, man); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
		if ok, err := eng.ManifestExists(ctx, man.SourceKind, man.SourceID, man.BackupID); err != nil || !ok {
			t.Fatalf("manifest missing: %v, %v", ok, err)
		}
		got, err := eng.ReadManifest(ctx, man.SourceKind, man.SourceID, man.BackupID)
		if err != nil {
			t.Fatalf("read manifest: %v", err)
		}
		if got.ChunkCount() != len(dm.Chunks) || got.TotalSize() != dm.SizeBytes {
			t.Fatalf("manifest round trip = %d chunks / %d bytes", got.ChunkCount(), got.TotalSize())
		}
		if got.ChunkSize != engine.ChunkSize {
			t.Fatalf("manifest chunk size = %d", got.ChunkSize)
		}

		var buf bytes.Buffer
		rsess := eng.NewSession(got.TotalSize(), nil)
		if err := rsess.RestoreDisk(ctx, got.Disks[0], &buf); err != nil {
			t.Fatalf("restore: %v", err)
		}
		if !bytes.Equal(buf.Bytes(), data) {
			t.Fatal("restored bytes differ from the source")
		}

		// Corrupt a chunk on the target: restore must refuse it.
		if err := bs.Put(ctx, engine.ChunkKey(dm.Chunks[0].Sha256), deterministicBytes(engine.ChunkSize, 5)); err != nil {
			t.Fatalf("corrupt chunk: %v", err)
		}
		var buf2 bytes.Buffer
		err = eng.NewSession(got.TotalSize(), nil).RestoreDisk(ctx, got.Disks[0], &buf2)
		if !errors.Is(err, engine.ErrHashMismatch) {
			t.Fatalf("restore of corrupted chunk error = %v, want ErrHashMismatch", err)
		}
	})
}

func TestVerifyBackup(t *testing.T) {
	eachBackend(t, func(t *testing.T, eng *engine.Engine, bs blobstore.Store, _ *store.Store) {
		ctx := context.Background()

		// Two disks so the verify walks a whole restore point, not just one stream.
		first := deterministicBytes(9<<20, 21)
		second := deterministicBytes(4<<20, 22)

		sess := eng.NewSession(int64(len(first)+len(second)), nil)
		dm1, err := sess.BackupStream(ctx, "scsi0", bytes.NewReader(first))
		if err != nil {
			t.Fatalf("backup scsi0: %v", err)
		}
		dm2, err := sess.BackupStream(ctx, "scsi1", bytes.NewReader(second))
		if err != nil {
			t.Fatalf("backup scsi1: %v", err)
		}
		man := &engine.Manifest{
			BackupID: "b-verify", SourceKind: store.SourceVM, SourceID: "host_100",
			SourceName: "web-01", TargetID: targetID, Kind: engine.KindFull,
			SizeBytes: dm1.SizeBytes + dm2.SizeBytes,
			Disks:     []engine.DiskManifest{dm1, dm2},
		}

		// A healthy restore point verifies, reporting progress over every byte.
		var last engine.Stats
		vsess := eng.NewSession(man.TotalSize(), func(s engine.Stats) { last = s })
		if err := vsess.VerifyBackup(ctx, man); err != nil {
			t.Fatalf("verify healthy backup: %v", err)
		}
		if got := vsess.Stats().BytesProcessed; got != man.TotalSize() {
			t.Fatalf("verify processed %d bytes, want %d", got, man.TotalSize())
		}
		if vsess.Stats().BytesUploaded != 0 {
			t.Fatalf("verify uploaded %d bytes, want 0", vsess.Stats().BytesUploaded)
		}
		if last.CurrentStep != "Verifying web-01 scsi1" {
			t.Fatalf("last step = %q, want the second disk's verify step", last.CurrentStep)
		}

		// Corrupt one chunk object: verification must catch it as a hash mismatch.
		if err := bs.Put(ctx, engine.ChunkKey(dm2.Chunks[0].Sha256),
			deterministicBytes(int(dm2.Chunks[0].Size), 999)); err != nil {
			t.Fatalf("corrupt chunk: %v", err)
		}
		err = eng.NewSession(man.TotalSize(), nil).VerifyBackup(ctx, man)
		if !errors.Is(err, engine.ErrHashMismatch) {
			t.Fatalf("verify of corrupted backup = %v, want ErrHashMismatch", err)
		}

		// A truncated chunk is caught too, as a size mismatch rather than a hash one.
		short := []byte("too short")
		if err := bs.Put(ctx, engine.ChunkKey(dm1.Chunks[0].Sha256), short); err != nil {
			t.Fatalf("truncate chunk: %v", err)
		}
		if err := eng.NewSession(man.TotalSize(), nil).VerifyBackup(ctx, man); err == nil {
			t.Fatal("verify accepted a truncated chunk")
		}
	})
}

func TestGarbageCollectOrphanChunks(t *testing.T) {
	eachBackend(t, func(t *testing.T, eng *engine.Engine, _ blobstore.Store, st *store.Store) {
		ctx := context.Background()

		keep := deterministicBytes(4<<20, 1)
		drop := deterministicBytes(4<<20, 2)

		sessKeep := eng.NewSession(int64(len(keep)), nil)
		dmKeep, err := sessKeep.BackupStream(ctx, "keep", bytes.NewReader(keep))
		if err != nil {
			t.Fatalf("backup keep: %v", err)
		}
		sessDrop := eng.NewSession(int64(len(drop)), nil)
		dmDrop, err := sessDrop.BackupStream(ctx, "drop", bytes.NewReader(drop))
		if err != nil {
			t.Fatalf("backup drop: %v", err)
		}

		kept := &engine.Manifest{
			BackupID: "keep", SourceKind: store.SourceVM, SourceID: "s1", TargetID: targetID,
			SizeBytes: dmKeep.SizeBytes, Disks: []engine.DiskManifest{dmKeep},
		}
		dropped := &engine.Manifest{
			BackupID: "drop", SourceKind: store.SourceVM, SourceID: "s1", TargetID: targetID,
			SizeBytes: dmDrop.SizeBytes, Disks: []engine.DiskManifest{dmDrop},
		}
		if err := eng.WriteManifest(ctx, kept); err != nil {
			t.Fatalf("write kept: %v", err)
		}
		if err := eng.WriteManifest(ctx, dropped); err != nil {
			t.Fatalf("write dropped: %v", err)
		}

		// Nothing is orphaned yet.
		res, err := eng.GC(ctx)
		if err != nil {
			t.Fatalf("gc: %v", err)
		}
		if res.ChunksDeleted != 0 || res.ManifestsScanned != 2 {
			t.Fatalf("first gc = %+v", res)
		}

		if err := eng.DeleteManifest(ctx, dropped.SourceKind, dropped.SourceID, dropped.BackupID); err != nil {
			t.Fatalf("delete manifest: %v", err)
		}
		// Chunks are only collectable once they are older than the grace window that
		// protects an interrupted run's uploads, so age them deliberately. The window
		// itself is covered by TestGCGraceKeepsAnInterruptedBackupResumable.
		aged := time.Now().Add(-2 * engine.DefaultGCGrace)
		for _, sha := range []string{dmKeep.Chunks[0].Sha256, dmDrop.Chunks[0].Sha256} {
			if err := st.SetChunkAddedAt(ctx, targetID, sha, aged); err != nil {
				t.Fatalf("age chunk: %v", err)
			}
		}
		res, err = eng.GC(ctx)
		if err != nil {
			t.Fatalf("gc after prune: %v", err)
		}
		if res.ChunksDeleted != 1 || res.BytesFreed != 4<<20 {
			t.Fatalf("gc after prune = %+v, want 1 chunk / 4 MiB freed", res)
		}
		if has, err := st.HasChunk(ctx, targetID, dmDrop.Chunks[0].Sha256); err != nil || has {
			t.Fatalf("orphan chunk still indexed: %v, %v", has, err)
		}
		if has, err := st.HasChunk(ctx, targetID, dmKeep.Chunks[0].Sha256); err != nil || !has {
			t.Fatalf("referenced chunk was collected: %v, %v", has, err)
		}

		// The surviving restore point must still restore.
		var buf bytes.Buffer
		if err := eng.NewSession(kept.TotalSize(), nil).RestoreDisk(ctx, dmKeep, &buf); err != nil {
			t.Fatalf("restore after gc: %v", err)
		}
		if !bytes.Equal(buf.Bytes(), keep) {
			t.Fatal("restore after gc returned wrong bytes")
		}
	})
}

func TestGCReconcilesIndexAfterBucketLoss(t *testing.T) {
	eachBackend(t, func(t *testing.T, eng *engine.Engine, bs blobstore.Store, st *store.Store) {
		ctx := context.Background()

		data := deterministicBytes(4<<20, 7)
		sess := eng.NewSession(int64(len(data)), nil)
		dm, err := sess.BackupStream(ctx, "scsi0", bytes.NewReader(data))
		if err != nil {
			t.Fatalf("backup: %v", err)
		}
		sha := dm.Chunks[0].Sha256

		// Simulate out-of-band chunk loss (lifecycle rule, manual cleanup): the
		// object vanishes but the index row survives.
		if err := bs.Delete(ctx, engine.ChunkKey(sha)); err != nil {
			t.Fatalf("delete chunk object: %v", err)
		}
		if has, _ := st.HasChunk(ctx, targetID, sha); !has {
			t.Fatal("precondition: index row should still exist")
		}

		res, err := eng.GC(ctx)
		if err != nil {
			t.Fatalf("gc: %v", err)
		}
		if res.IndexRowsDropped != 1 {
			t.Fatalf("gc = %+v, want 1 index row dropped", res)
		}
		if has, _ := st.HasChunk(ctx, targetID, sha); has {
			t.Fatal("stale index row survived GC; dedup would skip re-upload forever")
		}

		// The next backup of the same data must re-upload the chunk.
		sess2 := eng.NewSession(int64(len(data)), nil)
		if _, err := sess2.BackupStream(ctx, "scsi0", bytes.NewReader(data)); err != nil {
			t.Fatalf("backup after reconcile: %v", err)
		}
		if sess2.Stats().BytesUploaded != int64(len(data)) {
			t.Fatalf("uploaded %d bytes after reconcile, want %d", sess2.Stats().BytesUploaded, len(data))
		}
	})
}

// TestSyncChunkIndexRebuildsFromTheTarget covers recovery from a lost index on
// both kinds of target: the objects that are there are the truth.
func TestSyncChunkIndexRebuildsFromTheTarget(t *testing.T) {
	eachBackend(t, func(t *testing.T, eng *engine.Engine, _ blobstore.Store, st *store.Store) {
		ctx := context.Background()
		data := deterministicBytes(9<<20, 13)
		dm, err := eng.NewSession(int64(len(data)), nil).BackupStream(ctx, "scsi0", bytes.NewReader(data))
		if err != nil {
			t.Fatalf("backup: %v", err)
		}
		for _, c := range dm.Chunks {
			if err := st.DeleteChunk(ctx, targetID, c.Sha256); err != nil {
				t.Fatalf("drop index row: %v", err)
			}
		}
		n, err := eng.SyncChunkIndex(ctx)
		if err != nil {
			t.Fatalf("sync chunk index: %v", err)
		}
		if n != len(dm.Chunks) {
			t.Fatalf("sync indexed %d chunks, want %d", n, len(dm.Chunks))
		}
		for _, c := range dm.Chunks {
			if has, err := st.HasChunk(ctx, targetID, c.Sha256); err != nil || !has {
				t.Fatalf("chunk %s was not reindexed: %v, %v", c.Sha256, has, err)
			}
		}
	})
}

func TestKindAndKeys(t *testing.T) {
	if engine.Kind("") != engine.KindFull {
		t.Fatal("no parent should be a full backup")
	}
	if engine.Kind("abc") != engine.KindIncremental {
		t.Fatal("a parent should make the backup incremental")
	}
	if got := engine.ChunkKey("deadbeef"); got != "chunks/deadbeef" {
		t.Fatalf("ChunkKey = %q", got)
	}
	if got := engine.ManifestKey("vm", "host_100", "b1"); got != "manifests/vm/host_100/b1.json" {
		t.Fatalf("ManifestKey = %q", got)
	}
}

// TestTheTwoKindsProduceTheSameTree is the migration promise: the object layout
// does not depend on the kind of target, so a target can be copied from object
// storage to a NAS (or the other way) with rsync and no translation.
func TestTheTwoKindsProduceTheSameTree(t *testing.T) {
	ctx := context.Background()
	data := deterministicBytes(9<<20, 4711)
	keysOf := map[string][]string{}
	for _, be := range backends() {
		eng, bs, _ := newEngineOn(t, be, engine.Options{})
		dm, err := eng.NewSession(int64(len(data)), nil).BackupStream(ctx, "scsi0", bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s backup: %v", be.name, err)
		}
		man := &engine.Manifest{
			BackupID: "b1", SourceKind: store.SourceVM, SourceID: "host_100",
			SourceName: "web-01", TargetID: targetID, Kind: engine.KindFull,
			SizeBytes: dm.SizeBytes, Disks: []engine.DiskManifest{dm},
		}
		if err := eng.WriteManifest(ctx, man); err != nil {
			t.Fatalf("%s write manifest: %v", be.name, err)
		}
		objs, err := bs.List(ctx, "")
		if err != nil {
			t.Fatalf("%s list: %v", be.name, err)
		}
		for _, o := range objs {
			keysOf[be.name] = append(keysOf[be.name], o.Key)
		}
		sort.Strings(keysOf[be.name])
	}
	first := backends()[0].name
	for _, be := range backends()[1:] {
		if len(keysOf[be.name]) != len(keysOf[first]) {
			t.Fatalf("%s holds %v, %s holds %v", be.name, keysOf[be.name], first, keysOf[first])
		}
		for i := range keysOf[first] {
			if keysOf[be.name][i] != keysOf[first][i] {
				t.Fatalf("key %d differs: %s has %q, %s has %q",
					i, be.name, keysOf[be.name][i], first, keysOf[first][i])
			}
		}
	}
	t.Logf("both kinds hold exactly: %v", keysOf[first])
}

func TestProgressCallback(t *testing.T) {
	eachBackend(t, func(t *testing.T, eng *engine.Engine, _ blobstore.Store, _ *store.Store) {
		ctx := context.Background()
		data := deterministicBytes(8<<20, 3)

		var last engine.Stats
		calls := 0
		sess := eng.NewSession(int64(len(data)), func(s engine.Stats) {
			calls++
			last = s
		})
		sess.SetStep("Backing up scsi0")
		if _, err := sess.BackupStream(ctx, "scsi0", bytes.NewReader(data)); err != nil {
			t.Fatalf("backup: %v", err)
		}
		sess.SetStep("Completed")
		sess.Flush()
		if calls < 2 {
			t.Fatalf("progress callback fired %d times", calls)
		}
		if last.CurrentStep != "Completed" {
			t.Fatalf("final step = %q", last.CurrentStep)
		}
		if last.BytesProcessed != int64(len(data)) || last.Pct < 99.9 {
			t.Fatalf("final stats = %+v", last)
		}
	})
}
