package blobstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrPathUnavailable reports that a filesystem target's base path has gone: the
// NAS was unmounted, the share renamed, the USB disk pulled. It is distinct from
// ErrNotFound on purpose — "the object is not there" and "the storage is not
// there" call for very different operator responses, and treating the second as
// the first would let a backup happily "deduplicate" against a vanished target.
var ErrPathUnavailable = errors.New("filesystem target path is not available")

// tmpPrefix marks a file that is mid-write. Such files are invisible to listings
// so a garbage collection pass can never mistake an in-flight upload for a
// stray object, and never counts one towards a target's contents.
const tmpPrefix = ".pbtmp-"

// probePrefix names the connection-test probe file written into the base path.
const probePrefix = ".proxback-probe-"

// Filesystem stores objects as files under a base path — a local disk, a USB
// disk, a ZFS dataset, or an NFS/SMB share the operator has mounted with the OS.
// ProxBack deliberately implements no network filesystem protocol: the mount is
// the operating system's job, and pointing a target at the mount path is how
// Proxmox Backup Server, Veeam repositories and restic all work.
//
// # Layout
//
// The tree is byte-for-byte the S3 layout: "<base>/chunks/<sha256>" and
// "<base>/manifests/<kind>/<sourceId>/<backupId>.json", flat chunk directory
// included. Sharding chunks into "chunks/ab/cd/<sha>" would be kinder to a
// directory with a million entries, but it would also mean a filesystem target
// and an S3 target are no longer the same tree — and being the same tree is what
// makes `aws s3 sync`, rsync-to-offsite and migrating a target between kinds
// work with no translation step, which is a documented promise of this feature.
// The flat directory is defensible on the filesystems this actually runs on:
// ext4 with dir_index (the default), XFS and NTFS all index large directories
// with hashed B-trees, so lookup by name — which is what dedup does, once per
// chunk — does not degrade with entry count, and ZFS never had the problem.
// Listing is a single streaming readdir per directory (see Walk), never a
// recursive descent through hundreds of thousands of intermediate nodes.
type Filesystem struct {
	base string
}

var _ Store = (*Filesystem)(nil)

// writeHook, when set, runs after the temporary file for an atomic write has
// been created and before any data is written to it. It exists so a test can
// inject a mid-write failure (a full disk, a mount that vanished) and assert
// that no partial destination file is left behind.
var writeHook func(tmpPath string) error

// NewFilesystem opens a filesystem target rooted at path. The path must already
// exist and be a directory: ProxBack never creates a target's root, because the
// classic NAS accident is writing into an empty mountpoint directory after the
// share failed to mount, and creating the directory would hide exactly that.
func NewFilesystem(path string) (*Filesystem, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, errors.New("filesystem target: path is required")
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return nil, fmt.Errorf("filesystem target: resolve %q: %w", trimmed, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("filesystem target: %w: %s (unmounted, renamed or removed?)",
				ErrPathUnavailable, abs)
		}
		return nil, fmt.Errorf("filesystem target: cannot use %s: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("filesystem target: %s is not a directory", abs)
	}
	return &Filesystem{base: abs}, nil
}

// Path returns the absolute base path of the target.
func (f *Filesystem) Path() string { return f.base }

// ---------------------------------------------------------------- keys

// resolve maps an object key onto a path inside the base directory, refusing
// anything that could escape it.
func (f *Filesystem) resolve(key string) (string, error) {
	rel, err := relPath(key, false)
	if err != nil {
		return "", fmt.Errorf("filesystem target: %w", err)
	}
	return filepath.Join(f.base, rel), nil
}

// relPath validates a key (or a listing prefix's directory part) and converts it
// to a relative filesystem path. Keys are the S3 key space: slash separated,
// never absolute, never containing a traversal segment.
func relPath(key string, isPrefix bool) (string, error) {
	if key == "" {
		if isPrefix {
			return "", nil
		}
		return "", errors.New("empty object key")
	}
	if strings.ContainsAny(key, `\:`) {
		return "", fmt.Errorf("invalid object key %q: only forward slashes separate segments", key)
	}
	parts := strings.Split(key, "/")
	for _, p := range parts {
		switch {
		case p == "":
			return "", fmt.Errorf("invalid object key %q: empty path segment", key)
		case p == "." || p == "..":
			return "", fmt.Errorf("invalid object key %q: relative path segment", key)
		case strings.HasPrefix(p, tmpPrefix):
			return "", fmt.Errorf("invalid object key %q: %q is reserved for in-progress writes", key, tmpPrefix)
		}
	}
	return filepath.Join(parts...), nil
}

// ---------------------------------------------------------------- errors

// unavailable builds the error that names the vanished target path. Operators get
// "path /mnt/nas is not available" rather than "open
// /mnt/nas/chunks/9f86d…: no such file or directory".
func (f *Filesystem) unavailable(op string) error {
	return fmt.Errorf("filesystem target: cannot %s: %w: %s (unmounted, renamed or removed?)",
		op, ErrPathUnavailable, f.base)
}

// baseAvailable reports whether the base directory is still there. It is only
// consulted when an operation has already failed with "not exist", so the extra
// stat never sits on a hot path.
func (f *Filesystem) baseAvailable() bool {
	info, err := os.Stat(f.base)
	return err == nil && info.IsDir()
}

// notFound turns a "no such file" into either ErrNotFound or, when the whole
// target has gone, the unmistakable path error.
func (f *Filesystem) notFound(op, key string) error {
	if !f.baseAvailable() {
		return f.unavailable(op + " " + key)
	}
	return fmt.Errorf("%w: %s", ErrNotFound, key)
}

// wrap annotates a syscall failure with the operation, the key and the target
// path, and promotes "the base path is gone" to ErrPathUnavailable.
func (f *Filesystem) wrap(op, key string, err error) error {
	if errors.Is(err, fs.ErrNotExist) && !f.baseAvailable() {
		return f.unavailable(op + " " + key)
	}
	if key == "" {
		return fmt.Errorf("filesystem target %s: %s: %w", f.base, op, err)
	}
	return fmt.Errorf("filesystem target %s: %s %s: %w", f.base, op, key, err)
}

// ---------------------------------------------------------------- reads

// Put stores an object atomically: the data is written to a temporary file in the
// destination's own directory, flushed to stable storage, and only then renamed
// onto the key. A crash, a full disk or a yanked mount can therefore leave a
// stray temporary file (invisible to listings, cleaned up on the next attempt)
// but never a truncated chunk that a later verification would read as corruption.
func (f *Filesystem) Put(ctx context.Context, key string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := f.resolve(key)
	if err != nil {
		return err
	}
	// The base path is checked before any directory is created. MkdirAll would
	// happily recreate a vanished mount point as a local directory, which is
	// precisely the accident this feature exists to prevent: backups filling the
	// root disk while the operator believes they are going to the NAS.
	if !f.baseAvailable() {
		return f.unavailable("put " + key)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return f.wrap("create the directory for", key, err)
	}
	if err := writeFileAtomic(path, data); err != nil {
		return f.wrap("put", key, err)
	}
	return nil
}

// writeFileAtomic is the temp-file + fsync + rename dance. The temporary file is
// created in the *destination directory* so the rename is a same-filesystem
// operation, which POSIX guarantees is atomic; a temp file in /tmp would turn the
// rename into a copy and reintroduce exactly the torn-write it is there to
// prevent.
func writeFileAtomic(path string, data []byte) error {
	dir, name := filepath.Split(path)
	tmp, err := os.CreateTemp(dir, tmpPrefix+name+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if hook := writeHook; hook != nil {
		if err := hook(tmpName); err != nil {
			return err
		}
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	// fsync before the rename: without it a crash can publish the name while the
	// blocks are still in page cache, which is the torn chunk in its subtlest form.
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	committed = true
	syncDir(dir)
	return nil
}

// syncDir flushes a directory entry so the rename itself survives a power cut.
// It is best effort: Windows cannot fsync a directory handle opened this way, and
// some network filesystems refuse it, neither of which makes the write unsafe.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// Get opens an object for reading.
func (f *Filesystem) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := f.resolve(key)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, f.notFound("get", key)
		}
		return nil, f.wrap("get", key, err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, f.wrap("get", key, err)
	}
	if info.IsDir() {
		_ = file.Close()
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return file, nil
}

// GetBytes reads a whole object into memory.
func (f *Filesystem) GetBytes(ctx context.Context, key string) ([]byte, error) {
	rc, err := f.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, f.wrap("read", key, err)
	}
	return b, nil
}

// Head reports whether an object exists and its stored size.
func (f *Filesystem) Head(ctx context.Context, key string) (int64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	path, err := f.resolve(key)
	if err != nil {
		return 0, false, err
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// A missing object is a miss — but a missing *target* must never read
			// as one, or a backup onto an unmounted NAS would report every chunk
			// as absent and then fail on the write anyway.
			if !f.baseAvailable() {
				return 0, false, f.unavailable("head " + key)
			}
			return 0, false, nil
		}
		return 0, false, f.wrap("head", key, err)
	}
	if info.IsDir() {
		return 0, false, nil
	}
	return info.Size(), true, nil
}

// Delete removes an object. Deleting an object that is not there is not an
// error, matching the S3 client — retention and GC both rely on it.
//
// Emptied directories are left in place. Removing them would race with a
// concurrent Put into the same prefix for no gain: an empty "manifests/vm/<id>/"
// costs one inode and lists as nothing, exactly like the missing prefix it
// stands in for on object storage.
func (f *Filesystem) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := f.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if !f.baseAvailable() {
				return f.unavailable("delete " + key)
			}
			return nil
		}
		return f.wrap("delete", key, err)
	}
	return nil
}

// ---------------------------------------------------------------- listing

// Walk streams every object under prefix. It descends with filepath.WalkDir, so
// one directory's entries are read at a time rather than the whole tree being
// materialised — which is what makes a GC pass over hundreds of thousands of
// chunks bounded in memory by the caller's own bookkeeping rather than by the
// listing.
func (f *Filesystem) Walk(ctx context.Context, prefix string, fn func(Object) error) error {
	root := f.base
	if dir := prefixDir(prefix); dir != "" {
		rel, err := relPath(dir, true)
		if err != nil {
			return fmt.Errorf("filesystem target: %w", err)
		}
		root = filepath.Join(f.base, rel)
	}
	if _, err := os.Stat(root); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Nothing under the prefix yet — the same answer S3 gives for an empty
			// prefix. Unless the target itself is gone, which is not "empty".
			if !f.baseAvailable() {
				return f.unavailable("list " + prefix)
			}
			return nil
		}
		return f.wrap("list", prefix, err)
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// Retention deletes a source's manifest directory while a GC pass may be
			// walking it; that is normal and must not fail the pass. A base path that
			// disappeared is not.
			if errors.Is(err, fs.ErrNotExist) {
				if !f.baseAvailable() {
					return f.unavailable("list " + prefix)
				}
				return nil
			}
			return f.wrap("list", prefix, err)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, tmpPrefix) || strings.HasPrefix(name, probePrefix) {
			return nil
		}
		rel, relErr := filepath.Rel(f.base, p)
		if relErr != nil {
			return f.wrap("list", prefix, relErr)
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			// Deleted between the readdir and the stat: it is simply not there.
			if errors.Is(infoErr, fs.ErrNotExist) {
				return nil
			}
			return f.wrap("list", key, infoErr)
		}
		return fn(Object{Key: key, Size: info.Size(), LastModified: info.ModTime().UTC()})
	})
}

// List returns every object under prefix.
func (f *Filesystem) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	if err := f.Walk(ctx, prefix, func(o Object) error {
		out = append(out, o)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// prefixDir returns the directory part of a listing prefix ("chunks/" →
// "chunks", "manifests/vm/host_1" → "manifests/vm"), which is where a walk can
// start instead of at the root.
func prefixDir(prefix string) string {
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		return prefix[:i]
	}
	return ""
}

// ---------------------------------------------------------------- probe

// Test writes, reads back and removes a probe file in the base path. It is the
// filesystem equivalent of the S3 client's round trip: it proves the path is
// there and writable rather than merely present, which is the difference between
// a working NAS target and a read-only export.
func (f *Filesystem) Test(_ context.Context) error {
	return probeWritable(f.base)
}

// probeWritable is the writability proof used by both Test and Check.
func probeWritable(base string) error {
	want := []byte("proxback filesystem target probe")
	file, err := os.CreateTemp(base, probePrefix+"*")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("filesystem target: %w: %s (unmounted, renamed or removed?)",
				ErrPathUnavailable, base)
		}
		return fmt.Errorf("filesystem target %s is not writable: %w", base, err)
	}
	name := file.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := file.Write(want); err != nil {
		_ = file.Close()
		return fmt.Errorf("filesystem target %s is not writable: %w", base, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("filesystem target %s did not accept a flush: %w", base, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("filesystem target %s did not accept a write: %w", base, err)
	}
	got, err := os.ReadFile(name)
	if err != nil {
		return fmt.Errorf("filesystem target %s is not readable: %w", base, err)
	}
	if string(got) != string(want) {
		return fmt.Errorf("filesystem target %s returned different bytes than were written to it", base)
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("filesystem target %s does not allow deletion: %w", base, err)
	}
	return nil
}
