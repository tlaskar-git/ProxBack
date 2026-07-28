package browse

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
)

// Entry is one file or directory inside a backup.
//
// Offset and Size describe where the content sits in the stream the entry came
// out of, which is what makes single-file extraction a short ranged read rather
// than a full restore. A directory has no content and carries Offset 0.
type Entry struct {
	// Path is slash-separated and relative to the root of the backup, with no
	// leading slash — the same shape whether the guest was Windows or Linux.
	Path  string    `json:"path"`
	Size  int64     `json:"size"`
	Mode  uint32    `json:"mode"`
	MTime time.Time `json:"mtime"`
	Dir   bool      `json:"dir"`
	// Link is the target of a symlink or hard link, empty otherwise. Listed but
	// never followed: resolving a link inside a backup could walk out of it.
	Link string `json:"link,omitempty"`
	// Stream names which disk/stream of the restore point this entry is in, so
	// an extraction knows which chunk list to read.
	Stream string `json:"stream"`
	Offset int64   `json:"offset"`
}

// Index is the browsable content of one restore point.
type Index struct {
	BackupID string  `json:"backupId"`
	Entries  []Entry `json:"entries"`
	// Truncated says the archive held more entries than the cap allowed, so the
	// listing is incomplete. Surfaced rather than silently dropped: a file
	// browser that quietly omits files is worse than one that admits it.
	Truncated bool `json:"truncated,omitempty"`
}

// MaxIndexEntries caps how many entries one index holds.
//
// An index is held in memory to be served, and a runaway walk of a filesystem
// with millions of files would otherwise be an out-of-memory rather than a slow
// page. At this cap an index is tens of MB.
const MaxIndexEntries = 2_000_000

// Reasons a restore point cannot be listed.
var (
	// ErrNotBrowsable is returned for a stream whose format nothing here reads.
	ErrNotBrowsable = errors.New("browse: this backup's format cannot be browsed")
	// ErrVMNotIndexable separates "not built yet" from "cannot be done". A VM
	// backup is a VMA stream of raw disk images, so filenames only exist once
	// the partition table and the guest filesystem have been read; until that
	// lands, saying so beats presenting an empty folder as if the disk were.
	ErrVMNotIndexable = errors.New("browse: file-level browsing of VM images is not available yet — restore the whole VM, or install the agent in the guest for file-level backups")
)

/*
IndexTar walks a tar stream and records where every member's content begins.

The walk reads headers and skips over content, so on a seekable source it
touches only the chunks holding tar headers rather than the whole archive. That
is the difference between indexing a large agent backup in seconds and
downloading all of it.
*/
func IndexTar(ctx context.Context, r io.ReaderAt, size int64, stream string) ([]Entry, bool, error) {
	sr := io.NewSectionReader(r, 0, size)
	tr := tar.NewReader(sr)
	entries := make([]Entry, 0, 256)
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// A truncated archive still describes everything before the break,
			// and half an index beats none when someone is trying to recover.
			if len(entries) > 0 {
				return entries, true, nil
			}
			return nil, false, fmt.Errorf("browse: read tar: %w", err)
		}
		name := normalizePath(h.Name)
		if name == "" {
			continue
		}
		isDir := h.Typeflag == tar.TypeDir || strings.HasSuffix(h.Name, "/")
		e := Entry{
			Path:   name,
			Size:   h.Size,
			Mode:   uint32(h.Mode),
			MTime:  h.ModTime,
			Dir:    isDir,
			Stream: stream,
		}
		switch h.Typeflag {
		case tar.TypeSymlink, tar.TypeLink:
			e.Link = h.Linkname
		}
		if !isDir && h.Size > 0 {
			// CurrentOffset is the position of this member's content, which is
			// exactly the ranged read a later extraction needs.
			e.Offset = curOffset(tr, sr, size)
		}
		entries = append(entries, e)
		if len(entries) >= MaxIndexEntries {
			return entries, true, nil
		}
	}
	return entries, false, nil
}

// curOffset reports where the current tar member's content starts.
//
// archive/tar does not expose this, so it is recovered from how far the
// underlying reader has been advanced: after Next returns, the section reader
// sits exactly at the member's first content byte.
func curOffset(_ *tar.Reader, sr *io.SectionReader, size int64) int64 {
	at, err := sr.Seek(0, io.SeekCurrent)
	if err != nil || at < 0 || at > size {
		return 0
	}
	return at
}

// normalizePath makes an archive member name safe and uniform to display.
//
// Members are attacker-influenced in the sense that they come from whatever was
// on the protected machine, so anything that could climb out of the tree —
// absolute paths, "..", Windows drive letters and backslashes — is flattened
// here rather than trusted later.
func normalizePath(name string) string {
	p := strings.ReplaceAll(name, `\`, "/")
	// A Windows path keeps its drive as the leading segment ("C:/x" -> "C/x"),
	// matching how the agent already names members. A job may cover both C: and
	// D:, and dropping the letter would collapse two different files onto one
	// path.
	if len(p) > 1 && p[1] == ':' {
		p = p[:1] + p[2:]
	}
	p = path.Clean("/" + p)
	p = strings.TrimPrefix(p, "/")
	if p == "." || p == ".." {
		return ""
	}
	return p
}

// Children lists the immediate contents of one directory of an index.
//
// Directories that exist only implicitly — a tar may hold "a/b/c.txt" with no
// entry for "a/b" — are synthesised, so browsing never dead-ends in a folder
// the archive did not bother to name.
func Children(entries []Entry, dir string) []Entry {
	dir = strings.Trim(normalizePath(dir), "/")
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	seen := map[string]int{}
	out := make([]Entry, 0, 64)
	for _, e := range entries {
		if !strings.HasPrefix(e.Path, prefix) || e.Path == dir {
			continue
		}
		rest := e.Path[len(prefix):]
		if rest == "" {
			continue
		}
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			name := rest[:i]
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = len(out)
			out = append(out, Entry{Path: prefix + name, Dir: true})
			continue
		}
		if at, ok := seen[rest]; ok {
			// A real entry replaces the placeholder synthesised for it.
			out[at] = e
			continue
		}
		seen[rest] = len(out)
		out = append(out, e)
	}
	sortEntries(out)
	return out
}

// Search returns entries whose name contains the query, for finding a file
// without knowing which directory it was in.
func Search(entries []Entry, query string, limit int) []Entry {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	if limit <= 0 {
		limit = 200
	}
	out := make([]Entry, 0, limit)
	for _, e := range entries {
		if e.Dir {
			continue
		}
		if strings.Contains(strings.ToLower(path.Base(e.Path)), q) {
			out = append(out, e)
			if len(out) >= limit {
				break
			}
		}
	}
	sortEntries(out)
	return out
}

// Find returns the entry at an exact path.
func Find(entries []Entry, p string) (Entry, bool) {
	want := normalizePath(p)
	for _, e := range entries {
		if e.Path == want {
			return e, true
		}
	}
	return Entry{}, false
}

// sortEntries orders a listing the way a file browser is expected to: folders
// first, then case-insensitively by name.
func sortEntries(out []Entry) {
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(path.Base(out[i].Path)) < strings.ToLower(path.Base(out[j].Path))
	})
}
