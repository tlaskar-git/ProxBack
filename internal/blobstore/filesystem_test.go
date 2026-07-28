package blobstore

// The filesystem store's tests live inside the package because two of the
// guarantees can only be proven from the inside: the atomicity of a write needs a
// failure injected between "temporary file created" and "data written", and the
// temp-file naming convention is not part of the public contract.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

func newFS(t *testing.T) (*Filesystem, string) {
	t.Helper()
	dir := t.TempDir()
	f, err := NewFilesystem(dir)
	if err != nil {
		t.Fatalf("NewFilesystem(%s): %v", dir, err)
	}
	return f, dir
}

func TestFilesystemRoundTrip(t *testing.T) {
	ctx := context.Background()
	f, dir := newFS(t)

	const key = "chunks/9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	data := bytes.Repeat([]byte("proxback"), 1024)

	if _, exists, err := f.Head(ctx, key); err != nil || exists {
		t.Fatalf("Head before Put = %v, %v; want false, nil", exists, err)
	}
	if _, err := f.GetBytes(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetBytes before Put = %v, want ErrNotFound", err)
	}
	if err := f.Put(ctx, key, data); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// The object landed exactly where the documented layout says, so an operator
	// can inspect the tree and rsync it offsite.
	onDisk := filepath.Join(dir, "chunks", "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08")
	raw, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("chunk is not at %s: %v", onDisk, err)
	}
	if !bytes.Equal(raw, data) {
		t.Fatal("the file on disk is not the bytes that were stored")
	}

	size, exists, err := f.Head(ctx, key)
	if err != nil || !exists || size != int64(len(data)) {
		t.Fatalf("Head = %d, %v, %v; want %d, true, nil", size, exists, err, len(data))
	}
	got, err := f.GetBytes(ctx, key)
	if err != nil {
		t.Fatalf("GetBytes: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("GetBytes returned different bytes")
	}
	rc, err := f.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	streamed := make([]byte, len(data))
	if _, err := rc.Read(streamed); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	_ = rc.Close()
	if !bytes.Equal(streamed, data) {
		t.Fatal("Get streamed different bytes")
	}

	// Overwriting is replacement, not appending.
	replacement := []byte("shorter")
	if err := f.Put(ctx, key, replacement); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, err = f.GetBytes(ctx, key)
	if err != nil || !bytes.Equal(got, replacement) {
		t.Fatalf("after overwrite = %q (%v)", got, err)
	}

	// A manifest key nests directories that did not exist.
	manifest := "manifests/vm/host-1_100/backup-1.json"
	if err := f.Put(ctx, manifest, []byte(`{"backupId":"backup-1"}`)); err != nil {
		t.Fatalf("put manifest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifests", "vm", "host-1_100", "backup-1.json")); err != nil {
		t.Fatalf("manifest is not at the documented path: %v", err)
	}

	if err := f.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, exists, err := f.Head(ctx, key); err != nil || exists {
		t.Fatalf("Head after Delete = %v, %v", exists, err)
	}
}

// TestFilesystemPutIsAtomic is the reason Put never writes in place: a chunk that
// is half written but fully named would pass dedup (the index says it is there)
// and fail verification months later, which is the worst failure a backup product
// can have. A failure mid-write must leave the destination exactly as it was.
func TestFilesystemPutIsAtomic(t *testing.T) {
	ctx := context.Background()
	f, dir := newFS(t)

	const key = "chunks/aaaa"
	original := []byte("the previous, good version of this object")
	if err := f.Put(ctx, key, original); err != nil {
		t.Fatalf("seed put: %v", err)
	}

	injected := errors.New("simulated write failure: no space left on device")
	writeHook = func(string) error { return injected }
	t.Cleanup(func() { writeHook = nil })

	// A brand new key whose write fails must not become visible at all.
	const fresh = "chunks/bbbb"
	err := f.Put(ctx, fresh, bytes.Repeat([]byte("x"), 1<<20))
	if !errors.Is(err, injected) {
		t.Fatalf("Put with a failing write returned %v, want the injected error", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "chunks", "bbbb")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("a failed write left the destination behind: %v", statErr)
	}
	if _, exists, err := f.Head(ctx, fresh); err != nil || exists {
		t.Fatalf("a failed write is visible to Head: %v, %v", exists, err)
	}

	// An overwrite that fails must leave the previous version untouched, byte for
	// byte — never truncated.
	if err := f.Put(ctx, key, []byte("the new version that never made it")); !errors.Is(err, injected) {
		t.Fatalf("failed overwrite returned %v", err)
	}
	got, err := f.GetBytes(ctx, key)
	if err != nil {
		t.Fatalf("previous version is unreadable after a failed overwrite: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("a failed overwrite damaged the stored object: %q", got)
	}

	// And no temporary file is left lying around, in the tree or in a listing.
	writeHook = nil
	entries, err := os.ReadDir(filepath.Join(dir, "chunks"))
	if err != nil {
		t.Fatalf("read chunks dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			t.Fatalf("temporary file %s survived a failed write", e.Name())
		}
	}
	objs, err := f.List(ctx, "chunks/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 1 || objs[0].Key != key {
		t.Fatalf("listing after failed writes = %+v, want only %s", objs, key)
	}
}

// TestFilesystemTemporaryFilesAreInvisible covers the crash case the hook cannot:
// a temp file left behind by a killed process must not be listed, because GC would
// otherwise see an object whose name is not a chunk hash.
func TestFilesystemTemporaryFilesAreInvisible(t *testing.T) {
	ctx := context.Background()
	f, dir := newFS(t)
	if err := f.Put(ctx, "chunks/real", []byte("real")); err != nil {
		t.Fatalf("put: %v", err)
	}
	leftover := filepath.Join(dir, "chunks", tmpPrefix+"real-123456")
	if err := os.WriteFile(leftover, []byte("half written"), 0o644); err != nil {
		t.Fatalf("simulate crash leftover: %v", err)
	}
	objs, err := f.List(ctx, "chunks/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 1 || objs[0].Key != "chunks/real" {
		t.Fatalf("listing = %+v, want just chunks/real", objs)
	}
	// It is also not addressable as an object, so nothing can restore from it.
	if err := f.Put(ctx, "chunks/"+tmpPrefix+"nope", []byte("x")); err == nil {
		t.Fatal("a key in the reserved temp namespace was accepted")
	}
}

func TestFilesystemListAndWalk(t *testing.T) {
	ctx := context.Background()
	f, _ := newFS(t)

	keys := []string{
		"chunks/aa11", "chunks/aa22", "chunks/bb33",
		"manifests/vm/host_100/b1.json",
		"manifests/vm/host_100/b2.json",
		"manifests/vm/host_101/b3.json",
		"manifests/agent/agent-1/b4.json",
	}
	for i, k := range keys {
		if err := f.Put(ctx, k, bytes.Repeat([]byte{byte('a' + i)}, i+1)); err != nil {
			t.Fatalf("put %s: %v", k, err)
		}
	}

	cases := []struct {
		prefix string
		want   []string
	}{
		{"", keys},
		{"chunks/", []string{"chunks/aa11", "chunks/aa22", "chunks/bb33"}},
		{"chunks/aa", []string{"chunks/aa11", "chunks/aa22"}},
		{"manifests/", []string{
			"manifests/agent/agent-1/b4.json",
			"manifests/vm/host_100/b1.json",
			"manifests/vm/host_100/b2.json",
			"manifests/vm/host_101/b3.json",
		}},
		{"manifests/vm/host_100/", []string{"manifests/vm/host_100/b1.json", "manifests/vm/host_100/b2.json"}},
		{"manifests/vm/host_10", []string{
			"manifests/vm/host_100/b1.json",
			"manifests/vm/host_100/b2.json",
			"manifests/vm/host_101/b3.json",
		}},
		{"nothing/here/", nil},
	}
	for _, tc := range cases {
		objs, err := f.List(ctx, tc.prefix)
		if err != nil {
			t.Fatalf("List(%q): %v", tc.prefix, err)
		}
		got := make([]string, 0, len(objs))
		for _, o := range objs {
			got = append(got, o.Key)
			if o.Size <= 0 {
				t.Errorf("List(%q) reported %s with size %d", tc.prefix, o.Key, o.Size)
			}
			if o.LastModified.IsZero() {
				t.Errorf("List(%q) reported %s with no timestamp", tc.prefix, o.Key)
			}
		}
		sort.Strings(got)
		want := append([]string(nil), tc.want...)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("List(%q) = %v, want %v", tc.prefix, got, want)
		}

		// Walk must agree with List, and must stop when the callback says so.
		var walked []string
		if err := f.Walk(ctx, tc.prefix, func(o Object) error {
			walked = append(walked, o.Key)
			return nil
		}); err != nil {
			t.Fatalf("Walk(%q): %v", tc.prefix, err)
		}
		sort.Strings(walked)
		if strings.Join(walked, ",") != strings.Join(want, ",") {
			t.Errorf("Walk(%q) = %v, want %v", tc.prefix, walked, want)
		}
	}

	// An error from the callback comes back unchanged so callers can match on it.
	sentinel := errors.New("stop")
	seen := 0
	err := f.Walk(ctx, "chunks/", func(Object) error {
		seen++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Walk error = %v, want the callback's own error", err)
	}
	if seen != 1 {
		t.Fatalf("Walk kept going after the callback failed (%d calls)", seen)
	}
}

// TestFilesystemReadsATreeItDidNotWrite is the offsite-copy promise: a target
// populated by rsync (or restored from an S3 target) is a valid target. It doubles
// as the large-ish listing case, where the walk must stay cheap.
func TestFilesystemReadsATreeItDidNotWrite(t *testing.T) {
	ctx := context.Background()
	f, dir := newFS(t)

	const n = 4000
	chunks := filepath.Join(dir, "chunks")
	if err := os.MkdirAll(chunks, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("%064x", i)
		if err := os.WriteFile(filepath.Join(chunks, name), []byte{byte(i), byte(i >> 8)}, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	start := time.Now()
	count := 0
	var bytesSeen int64
	if err := f.Walk(ctx, "chunks/", func(o Object) error {
		count++
		bytesSeen += o.Size
		if !strings.HasPrefix(o.Key, "chunks/") {
			t.Fatalf("walk produced key %q", o.Key)
		}
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	t.Logf("walked %d chunks in %v", count, time.Since(start).Round(time.Millisecond))
	if count != n || bytesSeen != 2*n {
		t.Fatalf("walk saw %d chunks / %d bytes, want %d / %d", count, bytesSeen, n, 2*n)
	}
	objs, err := f.List(ctx, "chunks/")
	if err != nil || len(objs) != n {
		t.Fatalf("List returned %d objects (%v), want %d", len(objs), err, n)
	}
	// A chunk written by something else is readable through the store.
	got, err := f.GetBytes(ctx, "chunks/"+fmt.Sprintf("%064x", 7))
	if err != nil || len(got) != 2 || got[0] != 7 {
		t.Fatalf("reading an rsynced chunk = %v, %v", got, err)
	}
}

// TestFilesystemDeleteMissingIsNotAnError pins the S3 semantics retention and GC
// rely on: pruning something twice is not a failure.
func TestFilesystemDeleteMissingIsNotAnError(t *testing.T) {
	ctx := context.Background()
	f, _ := newFS(t)
	if err := f.Delete(ctx, "chunks/never-existed"); err != nil {
		t.Fatalf("deleting a missing object = %v, want nil", err)
	}
	if err := f.Put(ctx, "chunks/cc", []byte("x")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := f.Delete(ctx, "chunks/cc"); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if err := f.Delete(ctx, "chunks/cc"); err != nil {
		t.Fatalf("second delete = %v, want nil", err)
	}
	// Deleting a manifest of a source whose directory is gone is the same case.
	if err := f.Delete(ctx, "manifests/vm/gone/b1.json"); err != nil {
		t.Fatalf("deleting a manifest under a missing directory = %v, want nil", err)
	}
}

// TestFilesystemPathDisappearsMidRun is the unmounted-NAS case. Every operation
// must name the path and say it is unavailable, rather than surfacing a bare
// syscall error — and a missing *target* must never be reported as a missing
// object, which would let a backup "deduplicate" against storage that is gone.
func TestFilesystemPathDisappearsMidRun(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "nas")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := NewFilesystem(dir)
	if err != nil {
		t.Fatalf("NewFilesystem: %v", err)
	}
	if err := f.Put(ctx, "chunks/dd", []byte("before the mount went away")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Skipf("cannot simulate a vanishing mount on %s: %v", runtime.GOOS, err)
	}

	ops := map[string]func() error{
		"put":    func() error { return f.Put(ctx, "chunks/ee", []byte("x")) },
		"get":    func() error { _, err := f.GetBytes(ctx, "chunks/dd"); return err },
		"head":   func() error { _, _, err := f.Head(ctx, "chunks/dd"); return err },
		"delete": func() error { return f.Delete(ctx, "chunks/dd") },
		"list":   func() error { _, err := f.List(ctx, "chunks/"); return err },
		"test":   func() error { return f.Test(ctx) },
	}
	for name, op := range ops {
		err := op()
		if err == nil {
			t.Fatalf("%s succeeded against a vanished path", name)
		}
		if !errors.Is(err, ErrPathUnavailable) {
			t.Fatalf("%s error = %v, want ErrPathUnavailable", name, err)
		}
		if errors.Is(err, ErrNotFound) {
			t.Fatalf("%s reported a vanished target as a missing object: %v", name, err)
		}
		if !strings.Contains(err.Error(), dir) {
			t.Fatalf("%s error does not name the path %s: %v", name, dir, err)
		}
	}

	// Opening a target whose path is not there says so up front.
	if _, err := NewFilesystem(dir); !errors.Is(err, ErrPathUnavailable) ||
		!strings.Contains(err.Error(), dir) {
		t.Fatalf("NewFilesystem on a vanished path = %v", err)
	}
}

func TestNewFilesystemRejectsBadPaths(t *testing.T) {
	if _, err := NewFilesystem("  "); err == nil {
		t.Fatal("an empty path was accepted")
	}
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := NewFilesystem(file)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("NewFilesystem on a regular file = %v", err)
	}
}

// TestFilesystemRejectsEscapingKeys keeps the store inside its base path: keys
// come from manifests, and a manifest is a file on storage the operator may not
// fully control.
func TestFilesystemRejectsEscapingKeys(t *testing.T) {
	ctx := context.Background()
	f, dir := newFS(t)
	for _, key := range []string{
		"", "../escape", "chunks/../../escape", "chunks//double", "/absolute",
		`chunks\windows`, "C:/absolute", "chunks/.", "chunks/..",
	} {
		if err := f.Put(ctx, key, []byte("x")); err == nil {
			t.Errorf("Put(%q) was accepted", key)
		}
		if _, err := f.GetBytes(ctx, key); err == nil {
			t.Errorf("GetBytes(%q) was accepted", key)
		}
		if err := f.Delete(ctx, key); err == nil {
			t.Errorf("Delete(%q) was accepted", key)
		}
	}
	// Nothing escaped: the parent of the base path is untouched.
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a key escaped the base path: %v", err)
	}
}

func TestFilesystemTestProbeLeavesNothingBehind(t *testing.T) {
	ctx := context.Background()
	f, dir := newFS(t)
	if err := f.Test(ctx); err != nil {
		t.Fatalf("Test: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read base dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("the probe left %d entries behind: %+v", len(entries), entries)
	}
}
