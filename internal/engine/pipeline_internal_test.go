package engine

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fullIndex is a chunk index that reports every chunk as already present, so the
// upload pipeline can be driven without an S3 target behind it.
type fullIndex struct{}

func (fullIndex) HasChunk(context.Context, string, string) (bool, error) { return true, nil }
func (fullIndex) AddChunk(context.Context, string, string, int64) error  { return nil }
func (fullIndex) DeleteChunk(context.Context, string, string) error      { return nil }
func (fullIndex) ChunkAddedAt(context.Context, string) (map[string]time.Time, error) {
	return map[string]time.Time{}, nil
}

func TestBufPoolNeverAllocatesMoreThanItsCap(t *testing.T) {
	ctx := context.Background()
	const max = 3
	p := newBufPool(max)

	// Take every buffer the pool may allocate.
	held := make([][]byte, 0, max)
	for i := 0; i < max; i++ {
		b, err := p.get(ctx)
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		if len(b) != ChunkSize {
			t.Fatalf("buffer %d is %d bytes, want %d", i, len(b), ChunkSize)
		}
		held = append(held, b)
	}
	if p.allocations() != max {
		t.Fatalf("pool allocated %d buffers, want %d", p.allocations(), max)
	}

	// The next request must wait for a buffer instead of allocating a fourth.
	got := make(chan []byte, 1)
	go func() {
		b, err := p.get(ctx)
		if err != nil {
			t.Errorf("blocked get: %v", err)
			close(got)
			return
		}
		got <- b
	}()
	select {
	case <-got:
		t.Fatal("pool handed out a buffer beyond its cap")
	default:
	}
	p.put(held[0])
	if b := <-got; len(b) != ChunkSize {
		t.Fatalf("recycled buffer is %d bytes", len(b))
	}
	if p.allocations() != max {
		t.Fatalf("pool grew to %d buffers under contention, want %d", p.allocations(), max)
	}

	// A cancelled context releases a waiter rather than wedging it.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := p.get(cctx); err == nil {
		t.Fatal("get on a cancelled context returned a buffer")
	}
}

func TestBufPoolIsSafeUnderConcurrentUse(t *testing.T) {
	ctx := context.Background()
	p := newBufPool(4)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				b, err := p.get(ctx)
				if err != nil {
					t.Errorf("get: %v", err)
					return
				}
				p.put(b)
			}
		}()
	}
	wg.Wait()
	if got := p.allocations(); got > 4 {
		t.Fatalf("pool allocated %d buffers, want at most 4", got)
	}
}

// TestBackupStreamMemoryIsBoundedByTheWorkerCount is the memory-safety guarantee
// of the parallel pipeline: streaming a disk far larger than RAM would allow must
// never allocate more than one read buffer per worker plus the one being filled.
func TestBackupStreamMemoryIsBoundedByTheWorkerCount(t *testing.T) {
	ctx := context.Background()
	const workers = 3
	// An index that claims to have every chunk keeps the test about buffers: no
	// chunk reaches S3, so no S3 target is needed.
	e := NewWithOptions(nil, "t", fullIndex{}, discardLogger(), Options{UploadConcurrency: workers})

	before := poolAllocations.Load()
	// 64 MiB through a 3-worker pipeline: 16 chunks, at most 4 buffers.
	data := bytes.Repeat([]byte{0xA5}, 64<<20)
	sess := e.NewSession(int64(len(data)), nil)
	dm, err := sess.BackupStream(ctx, "big", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if len(dm.Chunks) != 16 || dm.SizeBytes != int64(len(data)) {
		t.Fatalf("manifest = %d chunks / %d bytes", len(dm.Chunks), dm.SizeBytes)
	}
	if grew := poolAllocations.Load() - before; grew > workers+1 {
		t.Fatalf("streaming 64 MiB allocated %d buffers, want at most %d", grew, workers+1)
	}
}
