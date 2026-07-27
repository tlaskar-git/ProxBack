package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// Upload concurrency bounds, matching the settings contract.
const (
	// DefaultUploadConcurrency is the number of chunk uploads kept in flight.
	DefaultUploadConcurrency = 4
	// MinUploadConcurrency is the smallest accepted worker count.
	MinUploadConcurrency = 1
	// MaxUploadConcurrency is the largest accepted worker count.
	MaxUploadConcurrency = 16
)

// poolAllocations counts every ChunkSize buffer the engine has ever allocated
// for a read pool. Bounded memory is a correctness property here — a 4 TiB disk
// must not need 4 TiB of RAM — so it is instrumented rather than assumed.
var poolAllocations atomic.Int64

// bufPool hands out ChunkSize read buffers from a fixed free list. It never
// allocates more than max of them: a backup's memory footprint is bounded by the
// worker count, not by the size of the disk being read, so a 4 TiB disk streams
// through the same few buffers a 4 MiB one does.
type bufPool struct {
	free chan []byte
	max  int

	mu        sync.Mutex
	allocated int
}

func newBufPool(max int) *bufPool {
	if max < 1 {
		max = 1
	}
	return &bufPool{free: make(chan []byte, max), max: max}
}

// get returns a buffer to read into. It allocates only while the pool is below
// its cap; after that it waits for a worker to hand one back (or for ctx).
func (p *bufPool) get(ctx context.Context) ([]byte, error) {
	select {
	case b := <-p.free:
		return b[:ChunkSize], nil
	default:
	}
	p.mu.Lock()
	if p.allocated < p.max {
		p.allocated++
		p.mu.Unlock()
		poolAllocations.Add(1)
		return make([]byte, ChunkSize), nil
	}
	p.mu.Unlock()
	select {
	case b := <-p.free:
		return b[:ChunkSize], nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// put recycles a buffer. It never blocks: the free list has room for every
// buffer the pool is allowed to allocate.
func (p *bufPool) put(b []byte) {
	if cap(b) < ChunkSize {
		return
	}
	select {
	case p.free <- b[:ChunkSize]:
	default:
	}
}

// allocations reports how many buffers the pool has ever allocated.
func (p *bufPool) allocations() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.allocated
}

// chunkUnit is one chunk on its way to a worker, tagged with its position in the
// stream so the manifest can be assembled in order whatever order uploads finish
// in.
type chunkUnit struct {
	seq int
	buf []byte
	n   int
}

// BackupStream chunks r, uploading only chunks that are not already on the
// target, and returns the resulting disk manifest.
//
// Reads stay strictly sequential — that is what keeps chunk boundaries, and
// therefore deduplication, stable — but each chunk is handed to a bounded pool of
// upload workers, so hashing, compressing and PUTting the previous chunks
// overlaps with reading the next one instead of blocking it. Workers complete out
// of order; every chunk carries its sequence number and the manifest is assembled
// by sequence, so DiskManifest.Chunks is always in stream order.
//
// The first worker error cancels the rest of the stream and is what the call
// returns; no goroutine outlives the call.
func (s *Session) BackupStream(ctx context.Context, name string, r io.Reader) (DiskManifest, error) {
	dm := DiskManifest{Name: name, Chunks: []Chunk{}}
	workers := s.e.workers
	// One buffer per worker plus the one being read into: enough to keep every
	// worker fed without ever holding the whole stream in memory.
	pool := newBufPool(workers + 1)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	units := make(chan chunkUnit)

	var (
		mu       sync.Mutex
		chunks   []Chunk
		firstErr error
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		mu.Unlock()
	}
	place := func(seq int, c Chunk) {
		mu.Lock()
		for len(chunks) <= seq {
			chunks = append(chunks, Chunk{})
		}
		chunks[seq] = c
		mu.Unlock()
	}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Draining the channel rather than returning early is what keeps the
			// producer from blocking on a send after a sibling failed, and what
			// guarantees every buffer finds its way back to the pool.
			for u := range units {
				if ctx.Err() == nil {
					sha, uploaded, err := s.e.StoreChunk(ctx, u.buf[:u.n])
					if err != nil {
						fail(fmt.Errorf("engine: store chunk %d of %q: %w", u.seq, name, err))
					} else {
						place(u.seq, Chunk{Sha256: sha, Size: int64(u.n)})
						s.record(int64(u.n), uploaded)
					}
				}
				pool.put(u.buf)
			}
		}()
	}

	readErr := s.readChunks(ctx, name, r, pool, units)
	close(units)
	wg.Wait()

	mu.Lock()
	err := firstErr
	mu.Unlock()
	switch {
	case err != nil:
		// A worker failure cancels the reader, so the read error is only noise
		// here: report what actually went wrong.
		return dm, err
	case readErr != nil:
		return dm, readErr
	}
	for i, c := range chunks {
		if c.Sha256 == "" {
			return dm, fmt.Errorf("engine: %q chunk %d was never stored", name, i)
		}
		dm.SizeBytes += c.Size
	}
	if len(chunks) > 0 {
		dm.Chunks = chunks
	}
	return dm, nil
}

// readChunks fills buffers from r and dispatches them to the workers in stream
// order, stopping as soon as the stream ends, a read fails or the context is
// cancelled (which is how a worker's failure reaches the reader).
func (s *Session) readChunks(ctx context.Context, name string, r io.Reader, pool *bufPool, units chan<- chunkUnit) error {
	for seq := 0; ; {
		if err := ctx.Err(); err != nil {
			return err
		}
		buf, err := pool.get(ctx)
		if err != nil {
			return err
		}
		n, readErr := io.ReadFull(r, buf)
		if n > 0 {
			select {
			case units <- chunkUnit{seq: seq, buf: buf, n: n}:
				seq++
			case <-ctx.Done():
				pool.put(buf)
				return ctx.Err()
			}
		} else {
			pool.put(buf)
		}
		switch {
		case readErr == nil:
			continue
		case errors.Is(readErr, io.EOF), errors.Is(readErr, io.ErrUnexpectedEOF):
			return nil
		default:
			return fmt.Errorf("engine: read %q: %w", name, readErr)
		}
	}
}
