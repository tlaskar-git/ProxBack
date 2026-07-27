package helperclient_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"proxback/internal/helperclient"
)

// fakeHelper is a stand-in for the daemon on a Proxmox node.
type fakeHelper struct {
	content []byte
	// exportStatus, when non-zero, is answered instead of streaming content.
	exportStatus int

	mu       sync.Mutex
	paths    []string
	auth     []string
	imported []byte
	query    string
}

func (f *fakeHelper) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		f.note(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"node":"pve2","version":"9.9.9"}`))
	})
	mux.HandleFunc("/export/", func(w http.ResponseWriter, r *http.Request) {
		f.note(r)
		if f.exportStatus != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.exportStatus)
			_, _ = w.Write([]byte(`{"error":"vzdump failed: exit status 255"}`))
			return
		}
		_, _ = w.Write(f.content)
	})
	mux.HandleFunc("/import/", func(w http.ResponseWriter, r *http.Request) {
		f.note(r)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.imported = raw
		f.query = r.URL.RawQuery
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	return mux
}

func (f *fakeHelper) note(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
}

func (f *fakeHelper) seen() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...), append([]string(nil), f.auth...)
}

// serve starts the fake helper and returns the host and port a client needs.
func serve(t *testing.T, f *fakeHelper) (string, int) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split %s: %v", srv.URL, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("port %q: %v", port, err)
	}
	return host, n
}

func client() *helperclient.Client {
	return helperclient.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

const secret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestExportStreamsAndAuthenticates(t *testing.T) {
	fake := &fakeHelper{content: bytes.Repeat([]byte("vma"), 4096)}
	host, port := serve(t, fake)

	rc, err := client().Export(context.Background(), host, port, secret, 103)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close export: %v", err)
	}
	if !bytes.Equal(got, fake.content) {
		t.Fatalf("export returned %d bytes, want %d", len(got), len(fake.content))
	}
	paths, auth := fake.seen()
	if len(paths) != 1 || paths[0] != "GET /export/103" {
		t.Fatalf("helper saw %v", paths)
	}
	if auth[0] != "Bearer "+secret {
		t.Fatalf("helper saw auth %q", auth[0])
	}
}

func TestExportSurfacesTheHelperError(t *testing.T) {
	fake := &fakeHelper{exportStatus: http.StatusBadGateway}
	host, port := serve(t, fake)

	_, err := client().Export(context.Background(), host, port, secret, 103)
	if err == nil {
		t.Fatal("export of a failing helper returned no error")
	}
	var herr *helperclient.Error
	if !errors.As(err, &herr) {
		t.Fatalf("error %v is not a *helperclient.Error", err)
	}
	if herr.Status != http.StatusBadGateway {
		t.Fatalf("error status = %d", herr.Status)
	}
	if !strings.Contains(err.Error(), "vzdump failed") {
		t.Fatalf("error %q does not carry the helper's message", err)
	}
}

func TestImportSendsBodyStorageAndForce(t *testing.T) {
	for _, c := range []struct {
		storage   string
		force     bool
		wantQuery string
	}{
		{"", false, ""},
		{"local-lvm", false, "storage=local-lvm"},
		{"", true, "force=1"},
		{"nvme pool", true, "force=1&storage=nvme+pool"},
	} {
		fake := &fakeHelper{}
		host, port := serve(t, fake)
		payload := bytes.Repeat([]byte("archive"), 1024)
		err := client().Import(context.Background(), host, port, secret, 9993,
			c.storage, c.force, bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("import: %v", err)
		}
		fake.mu.Lock()
		gotBody, gotQuery := fake.imported, fake.query
		fake.mu.Unlock()
		if !bytes.Equal(gotBody, payload) {
			t.Fatalf("helper received %d bytes, want %d", len(gotBody), len(payload))
		}
		if gotQuery != c.wantQuery {
			t.Fatalf("import query = %q, want %q", gotQuery, c.wantQuery)
		}
		paths, _ := fake.seen()
		if paths[0] != "POST /import/9993" {
			t.Fatalf("helper saw %v", paths)
		}
	}
}

func TestHealth(t *testing.T) {
	fake := &fakeHelper{}
	host, port := serve(t, fake)

	h, err := client().Health(context.Background(), host, port, secret)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if h.Node != "pve2" || h.Version != "9.9.9" {
		t.Fatalf("health = %+v", h)
	}
}

func TestUnreachableHelperFails(t *testing.T) {
	// Port 1 on the loopback interface is never listening.
	if _, err := client().Health(context.Background(), "127.0.0.1", 1, secret); err == nil {
		t.Fatal("health of an unreachable helper returned no error")
	}
}
