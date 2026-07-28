// Package browse turns a stored backup back into something that can be looked
// inside: a random-access view of the backed-up stream, an index of what is in
// it, and the extraction of a single file out of it.
//
// The whole package rests on one property of how backups are written. A stream
// is cut into chunks of a fixed size before anything else happens to it, and
// compression is applied per chunk afterwards, so a byte offset in the original
// stream still lands in a predictable chunk. That is what makes it possible to
// read one file out of a 250 GiB backup by fetching a few megabytes, instead of
// downloading the whole thing to reach the middle of it.
package browse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"

	"proxback/internal/engine"
)

// ErrOutOfRange is returned for a read that starts past the end of the stream.
var ErrOutOfRange = errors.New("browse: read past the end of the stream")

// ChunkReader fetches one verified chunk. engine.Engine satisfies it.
type ChunkReader interface {
	ReadChunk(ctx context.Context, ch engine.Chunk) ([]byte, error)
}

// DefaultCacheChunks is how many decoded chunks a reader keeps.
//
// Reading a file means touching a handful of chunks, usually adjacent, and
// walking a filesystem means returning to the same metadata chunks over and
// over. A small cache turns that from repeated downloads into repeated map
// lookups; at 4 MiB a chunk this is 32 MiB per open backup.
const DefaultCacheChunks = 8

/*
StreamReaderAt is a random-access view of one backed-up stream.

It is an io.ReaderAt rather than an io.ReadSeeker because every consumer above
it — tar indexing, partition tables, filesystem drivers — reads scattered small
extents rather than a cursor moving forward, and because io.ReaderAt is safe to
share between goroutines.
*/
type StreamReaderAt struct {
	ctx    context.Context
	src    ChunkReader
	chunks []engine.Chunk
	// starts[i] is the offset in the logical stream at which chunks[i] begins.
	// Built from the recorded sizes rather than assumed to be a multiple of the
	// chunk size, so a manifest written by any version stays readable.
	starts []int64
	size   int64

	mu    sync.Mutex
	cache map[int][]byte
	order []int
	limit int
}

// NewStreamReaderAt builds a random-access view of one disk or stream of a
// restore point. The context bounds every chunk fetch the reader makes.
func NewStreamReaderAt(ctx context.Context, src ChunkReader, dm engine.DiskManifest) *StreamReaderAt {
	starts := make([]int64, len(dm.Chunks))
	var at int64
	for i, ch := range dm.Chunks {
		starts[i] = at
		at += ch.Size
	}
	return &StreamReaderAt{
		ctx:    ctx,
		src:    src,
		chunks: dm.Chunks,
		starts: starts,
		size:   at,
		cache:  map[int][]byte{},
		limit:  DefaultCacheChunks,
	}
}

// Size is the length of the reconstructed stream.
func (r *StreamReaderAt) Size() int64 { return r.size }

// ReadAt implements io.ReaderAt over the stored chunks.
func (r *StreamReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("browse: negative offset %d", off)
	}
	if off >= r.size {
		return 0, io.EOF
	}
	var n int
	for n < len(p) {
		if off+int64(n) >= r.size {
			return n, io.EOF
		}
		idx := r.chunkAt(off + int64(n))
		if idx < 0 {
			return n, ErrOutOfRange
		}
		data, err := r.chunk(idx)
		if err != nil {
			return n, err
		}
		within := (off + int64(n)) - r.starts[idx]
		if within >= int64(len(data)) {
			// The manifest says this chunk is longer than the bytes behind it.
			// ReadChunk already rejects that, so reaching here means a manifest
			// with a zero size; refuse rather than return the wrong bytes.
			return n, fmt.Errorf("browse: chunk %d is shorter than its manifest entry", idx)
		}
		n += copy(p[n:], data[within:])
	}
	return n, nil
}

// chunkAt reports which chunk holds a logical offset.
func (r *StreamReaderAt) chunkAt(off int64) int {
	i := sort.Search(len(r.starts), func(i int) bool { return r.starts[i] > off }) - 1
	if i < 0 || i >= len(r.chunks) {
		return -1
	}
	return i
}

// chunk returns a decoded chunk, from cache when it is there.
func (r *StreamReaderAt) chunk(idx int) ([]byte, error) {
	r.mu.Lock()
	if data, ok := r.cache[idx]; ok {
		r.mu.Unlock()
		return data, nil
	}
	r.mu.Unlock()

	// Fetched outside the lock: a chunk fetch is a network round trip, and
	// holding the lock across it would serialise every reader of this backup.
	data, err := r.src.ReadChunk(r.ctx, r.chunks[idx])
	if err != nil {
		return nil, fmt.Errorf("browse: chunk %d (%s): %w", idx, short(r.chunks[idx].Sha256), err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.cache[idx]; ok {
		return existing, nil // another goroutine won the race; keep one copy
	}
	r.cache[idx] = data
	r.order = append(r.order, idx)
	for len(r.order) > r.limit {
		delete(r.cache, r.order[0])
		r.order = r.order[1:]
	}
	return data, nil
}

// SectionReader returns a reader over one extent of the stream, which is how a
// single file is pulled out of an archive.
func (r *StreamReaderAt) SectionReader(off, n int64) *io.SectionReader {
	return io.NewSectionReader(r, off, n)
}

func short(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}
