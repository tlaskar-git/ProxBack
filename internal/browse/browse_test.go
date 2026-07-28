package browse_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"testing"
	"time"

	"proxback/internal/browse"
	"proxback/internal/engine"
)

// memChunks stands in for a target: it holds chunks by content address and
// counts fetches, which is how the tests below prove that reading one file does
// not drag the whole backup down the wire.
type memChunks struct {
	data   map[string][]byte
	fetches int
}

func (m *memChunks) ReadChunk(_ context.Context, ch engine.Chunk) ([]byte, error) {
	b, ok := m.data[ch.Sha256]
	if !ok {
		return nil, fmt.Errorf("no such chunk %s", ch.Sha256)
	}
	m.fetches++
	return b, nil
}

// store cuts a stream into fixed chunks exactly as engine.readChunks does, so
// the offset arithmetic under test is the arithmetic the real manifests use.
func store(t *testing.T, blob []byte, chunkSize int) (*memChunks, engine.DiskManifest) {
	t.Helper()
	m := &memChunks{data: map[string][]byte{}}
	dm := engine.DiskManifest{Name: "files.tar", SizeBytes: int64(len(blob))}
	for off := 0; off < len(blob); off += chunkSize {
		end := min(off+chunkSize, len(blob))
		part := blob[off:end]
		sum := sha256.Sum256(part)
		sha := hex.EncodeToString(sum[:])
		m.data[sha] = part
		dm.Chunks = append(dm.Chunks, engine.Chunk{Sha256: sha, Size: int64(len(part))})
	}
	return m, dm
}

type member struct {
	name string
	body []byte
	dir  bool
}

func buildTar(t *testing.T, members []member) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, mem := range members {
		h := &tar.Header{Name: mem.name, ModTime: time.Unix(1700000000, 0)}
		if mem.dir {
			h.Typeflag, h.Mode = tar.TypeDir, 0o755
		} else {
			h.Typeflag, h.Mode, h.Size = tar.TypeReg, 0o644, int64(len(mem.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatalf("write header %s: %v", mem.name, err)
		}
		if !mem.dir {
			if _, err := tw.Write(mem.body); err != nil {
				t.Fatalf("write body %s: %v", mem.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

func body(seed int64, n int) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	r.Read(b)
	return b
}

/*
The offset recorded for a member is inferred from how far the tar reader has
advanced, because archive/tar does not report it. If that inference is ever
wrong the browser hands back the wrong bytes under the right filename, which is
the worst way for a backup product to fail — so it is checked against the real
content of every member, at sizes that straddle chunk boundaries.
*/
func TestAnIndexedFileExtractsToExactlyItsOriginalBytes(t *testing.T) {
	const chunk = 64 << 10
	members := []member{
		{name: "srv/", dir: true},
		{name: "srv/small.txt", body: []byte("hello")},
		{name: "srv/exactly-one-chunk.bin", body: body(1, chunk)},
		{name: "srv/spans-chunks.bin", body: body(2, chunk*3+1234)},
		{name: "srv/nested/deep/file.log", body: body(3, 5000)},
		{name: "srv/empty.txt", body: nil},
		{name: "srv/last.bin", body: body(4, 77777)},
	}
	blob := buildTar(t, members)
	src, dm := store(t, blob, chunk)

	ctx := context.Background()
	r := browse.NewStreamReaderAt(ctx, src, dm)
	entries, truncated, err := browse.IndexTar(ctx, r, r.Size(), "files.tar")
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if truncated {
		t.Fatal("index reported truncation for a complete archive")
	}

	for _, mem := range members {
		if mem.dir {
			continue
		}
		want := mem.body
		e, ok := browse.Find(entries, mem.name)
		if !ok {
			t.Fatalf("%s missing from the index", mem.name)
		}
		if e.Size != int64(len(want)) {
			t.Fatalf("%s size = %d, want %d", mem.name, e.Size, len(want))
		}
		got, err := io.ReadAll(r.SectionReader(e.Offset, e.Size))
		if err != nil {
			t.Fatalf("read %s: %v", mem.name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s extracted %d bytes that do not match the original", mem.name, len(got))
		}
	}
}

// The point of indexing is to avoid pulling the whole backup down to reach one
// file in the middle of it.
func TestExtractingOneFileDoesNotReadTheWholeBackup(t *testing.T) {
	const chunk = 64 << 10
	// ~6 MiB of archive, one small file at the very end.
	members := []member{
		{name: "bulk/a.bin", body: body(9, 3<<20)},
		{name: "bulk/b.bin", body: body(10, 3<<20)},
		{name: "bulk/needle.txt", body: []byte("the file someone actually wants")},
	}
	blob := buildTar(t, members)
	src, dm := store(t, blob, chunk)
	total := len(dm.Chunks)

	ctx := context.Background()
	r := browse.NewStreamReaderAt(ctx, src, dm)
	entries, _, err := browse.IndexTar(ctx, r, r.Size(), "files.tar")
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	afterIndex := src.fetches

	e, ok := browse.Find(entries, "bulk/needle.txt")
	if !ok {
		t.Fatal("needle.txt missing from the index")
	}
	got, err := io.ReadAll(r.SectionReader(e.Offset, e.Size))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "the file someone actually wants" {
		t.Fatalf("extracted %q", got)
	}
	if extra := src.fetches - afterIndex; extra > 2 {
		t.Fatalf("extracting one small file fetched %d chunks, want at most 2", extra)
	}
	// Indexing skips over content, so it must not have touched every chunk.
	if afterIndex >= total {
		t.Fatalf("indexing fetched %d of %d chunks; it should skip file bodies", afterIndex, total)
	}
}

func TestListingADirectoryShowsFoldersFirstAndSynthesisesMissingOnes(t *testing.T) {
	// No entry for "srv/logs" itself — a tar is allowed to omit it, and the
	// browser still has to offer the folder.
	entries := []browse.Entry{
		{Path: "srv/zebra.txt"},
		{Path: "srv/logs/app.log"},
		{Path: "srv/Apple.txt"},
		{Path: "srv/logs/db.log"},
		{Path: "other/thing.txt"},
	}
	got := browse.Children(entries, "srv")
	var names []string
	for _, e := range got {
		names = append(names, strings.TrimPrefix(e.Path, "srv/")+map[bool]string{true: "/", false: ""}[e.Dir])
	}
	want := []string{"logs/", "Apple.txt", "zebra.txt"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("listing = %v, want %v", names, want)
	}
}

// Names come from whatever was on the protected machine, so anything that could
// climb out of the tree is flattened at index time rather than trusted later.
func TestArchiveNamesCannotEscapeTheBackupRoot(t *testing.T) {
	cases := map[string]string{
		"/etc/passwd":            "etc/passwd",
		"../../etc/shadow":       "etc/shadow",
		`C:\Users\Example\a.txt`: "C/Users/Example/a.txt",
		`srv\logs\app.log`:       "srv/logs/app.log",
		"./srv/./x":              "srv/x",
	}
	for in, want := range cases {
		blob := buildTar(t, []member{{name: in, body: []byte("x")}})
		src, dm := store(t, blob, 64<<10)
		r := browse.NewStreamReaderAt(context.Background(), src, dm)
		entries, _, err := browse.IndexTar(context.Background(), r, r.Size(), "files.tar")
		if err != nil {
			t.Fatalf("index %q: %v", in, err)
		}
		if len(entries) != 1 || entries[0].Path != want {
			t.Fatalf("%q indexed as %+v, want path %q", in, entries, want)
		}
	}
}

func TestSearchFindsAFileWithoutKnowingItsDirectory(t *testing.T) {
	entries := []browse.Entry{
		{Path: "srv/a/config.yaml"},
		{Path: "srv/b/CONFIG.json"},
		{Path: "srv/b/notes.txt"},
		{Path: "srv/b", Dir: true},
	}
	got := browse.Search(entries, "config", 10)
	if len(got) != 2 {
		t.Fatalf("search returned %d entries, want 2: %+v", len(got), got)
	}
	for _, e := range got {
		if e.Dir {
			t.Fatal("search returned a directory")
		}
	}
}
