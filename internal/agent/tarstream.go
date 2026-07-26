package agent

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// writeTar streams the include paths as a deterministic tar archive. Entries are
// named "<base of include path>/<relative path>" so multiple include paths can
// never collide.
func writeTar(w io.Writer, paths []string) error {
	tw := tar.NewWriter(w)
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("agent: refusing empty include path")
		}
		root := filepath.Clean(p)
		info, err := os.Stat(root)
		if err != nil {
			return fmt.Errorf("agent: stat include path %q: %w", root, err)
		}
		base := filepath.Base(root)
		if !info.IsDir() {
			if err := addFile(tw, root, base, info); err != nil {
				return err
			}
			continue
		}
		walkErr := filepath.WalkDir(root, func(path string, de fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			name := base
			if rel != "." {
				name = base + "/" + filepath.ToSlash(rel)
			}
			info, err := de.Info()
			if err != nil {
				return err
			}
			switch {
			case de.IsDir():
				return addDir(tw, name, info)
			case info.Mode().IsRegular():
				return addFile(tw, path, name, info)
			default:
				// Symlinks, devices and sockets are skipped.
				return nil
			}
		})
		if walkErr != nil {
			return fmt.Errorf("agent: walk %q: %w", root, walkErr)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("agent: finish tar: %w", err)
	}
	return nil
}

func addDir(tw *tar.Writer, name string, info os.FileInfo) error {
	h := &tar.Header{
		Name:     strings.TrimSuffix(name, "/") + "/",
		Mode:     int64(info.Mode().Perm()),
		Typeflag: tar.TypeDir,
		ModTime:  info.ModTime().UTC().Truncate(time.Second),
		Format:   tar.FormatUSTAR,
	}
	if err := tw.WriteHeader(h); err != nil {
		return fmt.Errorf("agent: tar dir header %q: %w", name, err)
	}
	return nil
}

func addFile(tw *tar.Writer, path, name string, info os.FileInfo) error {
	h := &tar.Header{
		Name:     name,
		Mode:     int64(info.Mode().Perm()),
		Size:     info.Size(),
		Typeflag: tar.TypeReg,
		ModTime:  info.ModTime().UTC().Truncate(time.Second),
		Format:   tar.FormatUSTAR,
	}
	if err := tw.WriteHeader(h); err != nil {
		return fmt.Errorf("agent: tar file header %q: %w", name, err)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("agent: open %q: %w", path, err)
	}
	defer f.Close()
	n, err := io.Copy(tw, f)
	if err != nil {
		return fmt.Errorf("agent: read %q: %w", path, err)
	}
	if n != info.Size() {
		return fmt.Errorf("agent: %q changed size while reading (%d != %d)", path, n, info.Size())
	}
	return nil
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

// estimateSize sums the size of every regular file under the include paths so the
// server can report a meaningful progress percentage.
func estimateSize(paths []string) (int64, error) {
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
		err = filepath.WalkDir(root, func(_ string, de fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			// Every tar entry costs a 512 byte header.
			total += 512
			if de.IsDir() {
				return nil
			}
			fi, err := de.Info()
			if err != nil {
				return err
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
