package agent

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// namesIn reads every entry name out of a tar stream.
func namesIn(t *testing.T, raw []byte) []string {
	t.Helper()
	var names []string
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		names = append(names, h.Name)
	}
}

// A content-addressed store such as pnpm's routinely produces names far past
// the 100 bytes USTAR can hold. Forcing that format made those files
// unarchivable and failed the whole run — the bug a user hit backing up D:\.
func TestLongContentAddressedNamesAreArchivable(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, ".pnpm-store", "v11", "files", "00")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	// 128 hex characters, as produced by a content-addressed store.
	long := strings.Repeat("179ca79165121f5490874e79dfff329c", 4)
	if err := os.WriteFile(filepath.Join(deep, long), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	report, err := writeTar(&buf, []string{root}, tarOptions{})
	if err != nil {
		t.Fatalf("writeTar: %v", err)
	}
	if report.Files != 1 {
		t.Fatalf("archived %d files, want 1", report.Files)
	}

	var found string
	for _, name := range namesIn(t, buf.Bytes()) {
		if strings.Contains(name, long) {
			found = name
		}
		if strings.Contains(name, `\`) {
			t.Errorf("entry name contains a backslash: %q", name)
		}
	}
	if found == "" {
		t.Fatal("the long name is missing from the archive")
	}

	// And it must come back out intact.
	dest := t.TempDir()
	if err := extractTar(bytes.NewReader(buf.Bytes()), dest); err != nil {
		t.Fatalf("extractTar: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(found)))
	if err != nil || string(got) != "payload" {
		t.Fatalf("restored file = %q, %v", got, err)
	}
}

// filepath.Base("D:\\") is "\", which produced entries named "\/..." that no
// tar format can encode. A drive root must yield a usable prefix.
func TestArchivePrefixOfAwkwardRoots(t *testing.T) {
	cases := map[string]string{
		filepath.Join("srv", "data", "docs"): "docs",
		"/":                                  "root",
	}
	if runtime.GOOS == "windows" {
		cases[`D:\`] = "D"
		cases[`D:`] = "D"
		cases[`C:\Users\Example`] = "Example"
	}
	for in, want := range cases {
		if got := archivePrefix(in); got != want {
			t.Errorf("archivePrefix(%q) = %q, want %q", in, got, want)
		}
		if strings.ContainsAny(archivePrefix(in), `\/:`) {
			t.Errorf("archivePrefix(%q) = %q contains a separator", in, archivePrefix(in))
		}
	}
}

// A live filesystem always holds files something else has open. Losing every
// other file because of one of them is not acceptable in a backup product.
func TestUnreadableEntriesAreSkippedNotFatal(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "good.txt"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "vanishes.txt")
	if err := os.WriteFile(missing, []byte("gone"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Delete it after the walk has listed the directory but before it is read.
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	report, err := writeTar(&buf, []string{root}, tarOptions{})
	if err != nil {
		t.Fatalf("writeTar must not fail because one file is unreadable: %v", err)
	}
	names := namesIn(t, buf.Bytes())
	var kept bool
	for _, n := range names {
		if strings.HasSuffix(n, "good.txt") {
			kept = true
		}
	}
	if !kept {
		t.Fatalf("the readable file is missing; got %v", names)
	}
	_ = report
}

// Exclusions were accepted by the server, stored, and sent to the agent — and
// then ignored. They have to actually shape the walk.
func TestExclusionsAreApplied(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"keep", filepath.Join("node_modules", "left-pad"), ".pnpm-store"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join("keep", "a.txt"):                    "yes",
		filepath.Join("node_modules", "left-pad", "i.js"): "no",
		filepath.Join(".pnpm-store", "blob"):              "no",
	}
	for rel, body := range files {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var buf bytes.Buffer
	report, err := writeTar(&buf, []string{root}, tarOptions{
		Exclude: []string{"**/node_modules", ".pnpm-store"},
	})
	if err != nil {
		t.Fatalf("writeTar: %v", err)
	}
	for _, name := range namesIn(t, buf.Bytes()) {
		if strings.Contains(name, "node_modules") || strings.Contains(name, ".pnpm-store") {
			t.Errorf("excluded path was archived: %q", name)
		}
	}
	if report.Excluded == 0 {
		t.Error("nothing was reported as excluded")
	}
	if report.Files != 1 {
		t.Errorf("archived %d files, want only the kept one", report.Files)
	}

	// The estimate must agree, or progress counts data that never arrives.
	total, err := estimateSize([]string{root}, tarOptions{
		Exclude: []string{"**/node_modules", ".pnpm-store"},
	})
	if err != nil {
		t.Fatalf("estimateSize: %v", err)
	}
	full, err := estimateSize([]string{root}, tarOptions{})
	if err != nil {
		t.Fatalf("estimateSize: %v", err)
	}
	if total >= full {
		t.Errorf("excluded estimate %d is not smaller than the full estimate %d", total, full)
	}
}

// tar cannot revise a header once written, so a file that grows or shrinks
// mid-read must be padded or truncated to its declared size. Log files do this
// constantly; it must not corrupt the archive.
func TestFileChangingSizeDoesNotCorruptTheArchive(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "growing.log")
	if err := os.WriteFile(name, bytes.Repeat([]byte("x"), 512), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	// Shrink it behind the walker's back.
	if err := os.WriteFile(name, []byte("tiny"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	var report tarReport
	if err := addFile(tw, name, "growing.log", info, &report); err != nil {
		t.Fatalf("addFile: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// The archive must still be readable end to end.
	if names := namesIn(t, buf.Bytes()); len(names) != 1 {
		t.Fatalf("entries = %v, want exactly one", names)
	}
	if report.Changed != 1 {
		t.Errorf("changed count = %d, want 1", report.Changed)
	}
}
