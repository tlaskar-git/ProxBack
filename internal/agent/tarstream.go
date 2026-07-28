package agent

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// maxRecordedSkips bounds the skip list carried back to the server. A backup of
// a whole drive can legitimately skip thousands of locked files; the operator
// needs a representative sample and a count, not every path.
const maxRecordedSkips = 50

// tarOptions tunes a backup walk.
type tarOptions struct {
	// Exclude holds glob patterns from the job's protection policy. A pattern
	// matches against the entry's archive name, its path relative to the
	// include root, and its base name, so both "**/node_modules" and
	// "node_modules" do what an operator expects.
	Exclude []string
}

// tarReport describes what a walk actually managed to archive. Skips are not
// failures: a live filesystem always contains files that cannot be read, and a
// backup that aborts on the first locked file protects nothing.
type tarReport struct {
	Files    int
	Dirs     int
	Excluded int
	// Skipped samples the entries that could not be read, up to
	// maxRecordedSkips. SkippedCount is the true total.
	Skipped      []string
	SkippedCount int
	// Changed counts files whose size moved while they were being read.
	Changed int
}

func (r *tarReport) skip(pathname string, err error) {
	r.SkippedCount++
	if len(r.Skipped) < maxRecordedSkips {
		r.Skipped = append(r.Skipped, fmt.Sprintf("%s: %v", pathname, err))
	}
}

// archivePrefix derives the name an include path's contents live under inside
// the archive.
//
// filepath.Base is not usable directly: on Windows it answers `\` for a drive
// root such as `D:\`, which produced entries named "\/..." that no tar format
// can encode. A drive root becomes its letter, a Unix root becomes "root", and
// anything else keeps its base name.
func archivePrefix(root string) string {
	if vol := filepath.VolumeName(root); vol != "" {
		// `D:\` or `D:` -> "D"; a UNC share `\\host\share` -> "host_share".
		trimmed := strings.Trim(root[len(vol):], `\/`)
		if trimmed == "" {
			cleaned := strings.NewReplacer(`\`, "_", "/", "_", ":", "").Replace(vol)
			return strings.Trim(cleaned, "_")
		}
	}
	base := filepath.Base(root)
	switch base {
	case "/", `\`, ".", "..", "":
		return "root"
	}
	return base
}

// excluded reports whether an entry matches any of the policy's patterns.
func excluded(patterns []string, archiveName, relName, baseName string) bool {
	for _, pattern := range patterns {
		p := strings.TrimSpace(filepath.ToSlash(pattern))
		if p == "" {
			continue
		}
		for _, candidate := range []string{archiveName, relName, baseName} {
			if matchGlob(p, candidate) {
				return true
			}
		}
	}
	return false
}

// matchGlob is path.Match extended with "**", which spans separators. Anything
// path.Match rejects as malformed simply does not match, so a bad pattern can
// never silently exclude everything.
func matchGlob(pattern, name string) bool {
	if pattern == name {
		return true
	}
	if !strings.Contains(pattern, "**") {
		ok, err := path.Match(pattern, name)
		return err == nil && ok
	}
	// Split on "**" and require the literal parts to appear in order.
	parts := strings.Split(pattern, "**")
	rest := name
	for i, part := range parts {
		part = strings.Trim(part, "/")
		if part == "" {
			continue
		}
		idx := strings.Index(rest, part)
		if idx < 0 {
			return false
		}
		if i == 0 && !strings.HasPrefix(rest, part) && !strings.HasPrefix(pattern, "**") {
			return false
		}
		rest = rest[idx+len(part):]
	}
	return true
}

// writeTar streams the include paths as a tar archive. Entries are named
// "<prefix>/<relative path>" so multiple include paths can never collide.
//
// Entries that cannot be read are skipped and reported rather than failing the
// run: on a real machine a walk meets files held open by other processes,
// permission-denied directories and paths that vanish mid-walk, none of which
// should cost the operator every other file on the disk.
func writeTar(w io.Writer, paths []string, opts tarOptions) (tarReport, error) {
	var report tarReport
	tw := tar.NewWriter(w)
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			return report, fmt.Errorf("agent: refusing empty include path")
		}
		root := filepath.Clean(p)
		info, err := os.Stat(root)
		if err != nil {
			// The include path itself is configuration: if it is wrong, say so.
			return report, fmt.Errorf("agent: stat include path %q: %w", root, err)
		}
		prefix := archivePrefix(root)
		if !info.IsDir() {
			if err := addFile(tw, root, prefix, info, &report); err != nil {
				return report, err
			}
			continue
		}

		walkErr := filepath.WalkDir(root, func(pathname string, de fs.DirEntry, err error) error {
			if err != nil {
				// An unreadable directory costs its subtree, not the backup.
				if pathname == root {
					return err
				}
				report.skip(pathname, err)
				if de != nil && de.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			rel, err := filepath.Rel(root, pathname)
			if err != nil {
				report.skip(pathname, err)
				return nil
			}
			relSlash := filepath.ToSlash(rel)
			name := prefix
			if relSlash != "." {
				name = prefix + "/" + relSlash
			}
			if relSlash != "." && excluded(opts.Exclude, name, relSlash, filepath.Base(pathname)) {
				report.Excluded++
				if de.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			info, err := de.Info()
			if err != nil {
				report.skip(pathname, err)
				return nil
			}
			switch {
			case de.IsDir():
				if err := addDir(tw, name, info); err != nil {
					return err
				}
				report.Dirs++
				return nil
			case info.Mode().IsRegular():
				return addFile(tw, pathname, name, info, &report)
			default:
				// Symlinks, junctions, devices and sockets are not followed:
				// following them duplicates data and can loop.
				return nil
			}
		})
		if walkErr != nil {
			return report, fmt.Errorf("agent: walk %q: %w", root, walkErr)
		}
	}
	if err := tw.Close(); err != nil {
		return report, fmt.Errorf("agent: finish tar: %w", err)
	}
	return report, nil
}

// header builds an entry header. Format is deliberately left unset so the tar
// writer picks the least exotic encoding each entry actually needs — forcing
// USTAR made any path over 100 bytes unrepresentable, which is ordinary for
// content-addressed stores such as pnpm's.
func header(name string, info os.FileInfo, typeflag byte, size int64) *tar.Header {
	return &tar.Header{
		Name:     name,
		Mode:     int64(info.Mode().Perm()),
		Size:     size,
		Typeflag: typeflag,
		ModTime:  info.ModTime().UTC().Truncate(time.Second),
	}
}

func addDir(tw *tar.Writer, name string, info os.FileInfo) error {
	h := header(strings.TrimSuffix(name, "/")+"/", info, tar.TypeDir, 0)
	if err := tw.WriteHeader(h); err != nil {
		return fmt.Errorf("agent: tar dir header %q: %w", name, err)
	}
	return nil
}

// addFile writes one regular file. A file that cannot be opened is skipped. A
// file whose size moves while being read is padded or truncated to the size
// already declared in its header, because tar cannot revise it — log files grow
// during a backup and that must not corrupt the archive or fail the run.
func addFile(tw *tar.Writer, pathname, name string, info os.FileInfo, report *tarReport) error {
	f, err := os.Open(pathname)
	if err != nil {
		report.skip(pathname, err)
		return nil
	}
	defer f.Close()

	size := info.Size()
	if err := tw.WriteHeader(header(name, info, tar.TypeReg, size)); err != nil {
		return fmt.Errorf("agent: tar file header %q: %w", name, err)
	}
	n, err := io.Copy(tw, io.LimitReader(f, size))
	if err != nil {
		// The header is already committed, so the entry must be completed with
		// the declared number of bytes or the archive is malformed.
		if padErr := pad(tw, size-n); padErr != nil {
			return fmt.Errorf("agent: pad %q after read error: %w", name, padErr)
		}
		report.skip(pathname, err)
		report.Changed++
		report.Files++
		return nil
	}
	if n < size {
		if err := pad(tw, size-n); err != nil {
			return fmt.Errorf("agent: pad %q: %w", name, err)
		}
		report.Changed++
	}
	report.Files++
	return nil
}

func pad(w io.Writer, n int64) error {
	if n <= 0 {
		return nil
	}
	_, err := io.CopyN(w, zeroReader{}, n)
	return err
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// extractTar unpacks a tar stream below dest, rejecting path traversal.
func extractTar(r io.Reader, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("agent: create restore dir %q: %w", dest, err)
	}
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return fmt.Errorf("agent: resolve restore dir: %w", err)
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("agent: read tar: %w", err)
		}
		rel := filepath.Clean(filepath.FromSlash(h.Name))
		if rel == "." {
			continue
		}
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("agent: refusing unsafe tar entry %q", h.Name)
		}
		target := filepath.Join(absDest, rel)
		if !strings.HasPrefix(target, absDest+string(filepath.Separator)) {
			return fmt.Errorf("agent: refusing unsafe tar entry %q", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, permOf(h.Mode, 0o755)); err != nil {
				return fmt.Errorf("agent: create dir %q: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("agent: create parent of %q: %w", target, err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, permOf(h.Mode, 0o644))
			if err != nil {
				return fmt.Errorf("agent: create %q: %w", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("agent: write %q: %w", target, err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("agent: close %q: %w", target, err)
			}
			if !h.ModTime.IsZero() {
				_ = os.Chtimes(target, h.ModTime, h.ModTime)
			}
		default:
			// Unsupported entry types are ignored.
		}
	}
}

func permOf(mode int64, fallback os.FileMode) os.FileMode {
	p := os.FileMode(mode).Perm()
	if p == 0 {
		return fallback
	}
	return p
}

// estimateSize sums the size of every regular file under the include paths so
// the server can report a meaningful progress percentage. It tolerates the same
// unreadable entries the backup walk does, since an estimate that fails would
// stop a backup that could otherwise have run.
func estimateSize(paths []string, opts tarOptions) (int64, error) {
	var total int64
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			return 0, fmt.Errorf("agent: refusing empty include path")
		}
		root := filepath.Clean(p)
		info, err := os.Stat(root)
		if err != nil {
			return 0, fmt.Errorf("agent: stat include path %q: %w", root, err)
		}
		if !info.IsDir() {
			total += info.Size() + 1024
			continue
		}
		prefix := archivePrefix(root)
		err = filepath.WalkDir(root, func(pathname string, de fs.DirEntry, err error) error {
			if err != nil {
				if pathname == root {
					return err
				}
				if de != nil && de.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			rel, relErr := filepath.Rel(root, pathname)
			if relErr != nil {
				return nil
			}
			relSlash := filepath.ToSlash(rel)
			if relSlash != "." {
				name := prefix + "/" + relSlash
				if excluded(opts.Exclude, name, relSlash, filepath.Base(pathname)) {
					if de.IsDir() {
						return fs.SkipDir
					}
					return nil
				}
			}
			// Every tar entry costs a 512 byte header.
			total += 512
			if de.IsDir() {
				return nil
			}
			fi, err := de.Info()
			if err != nil {
				return nil
			}
			if fi.Mode().IsRegular() {
				total += fi.Size()
			}
			return nil
		})
		if err != nil {
			return 0, fmt.Errorf("agent: size %q: %w", root, err)
		}
	}
	return total, nil
}
