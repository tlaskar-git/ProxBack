// Package blobstore defines the object namespace ProxBack's backups live in and
// the operations the engine performs on it, so a target can be S3-compatible
// object storage or a local/NAS-mounted directory without the engine caring.
//
// The key space is deliberately the S3 one — "chunks/<sha256>" and
// "manifests/<kind>/<sourceId>/<backupId>.json" — for every implementation. A
// filesystem target is therefore the same tree an S3 target holds, which is what
// lets an operator inspect it with `ls`, rsync it offsite, or migrate a target
// between kinds without translating anything.
package blobstore

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotFound is returned when an object does not exist. Every implementation
// reports a missing object with this error so callers (restore, GC, the manifest
// readers) can branch on it without knowing the target's kind.
var ErrNotFound = errors.New("blobstore: object not found")

// Object is one entry from a listing.
type Object struct {
	Key  string
	Size int64
	// LastModified is the target's own timestamp for the object. Not every S3
	// implementation reports it, so treat the zero value as "unknown".
	LastModified time.Time
}

// Store is the whole storage contract of the backup engine: content-addressed
// chunks and JSON manifests written, read, listed and deleted by key.
//
// Implementations must agree on three behaviours the engine depends on:
//   - a missing object is reported as ErrNotFound from Get/GetBytes, and as
//     exists=false with a nil error from Head;
//   - deleting an object that is not there is not an error;
//   - a write is either completely visible or not visible at all — a reader must
//     never observe a partially written object, because the next verification
//     pass would read it as corruption.
type Store interface {
	// Put stores an object, replacing any object already under the key.
	Put(ctx context.Context, key string, data []byte) error
	// Get opens an object for reading. The caller must close the reader.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// GetBytes reads a whole object into memory.
	GetBytes(ctx context.Context, key string) ([]byte, error)
	// Head reports whether an object exists and how many bytes it stores.
	Head(ctx context.Context, key string) (size int64, exists bool, err error)
	// Delete removes an object. Deleting a missing object is not an error.
	Delete(ctx context.Context, key string) error
	// List returns every object whose key starts with prefix.
	List(ctx context.Context, prefix string) ([]Object, error)
	// Walk calls fn for every object whose key starts with prefix, streaming the
	// listing instead of materialising it. An error from fn stops the walk and is
	// returned unchanged, so callers can use errors.Is on it.
	Walk(ctx context.Context, prefix string, fn func(Object) error) error
	// Test performs a write/read/delete round trip on a probe object, which is
	// what the operator's "test connection" button reports on.
	Test(ctx context.Context) error
}
