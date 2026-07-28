package helper_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"proxback/internal/helper"
	"proxback/internal/helpermgr"
)

// newHelperBinary is a stand-in for a real node helper build.
var newHelperBinary = []byte("\x7fELF proxback-helper 0.6.2 " + strings.Repeat("payload", 64))

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// updatableHelper builds an enrolled helper whose server is a stand-in for
// /downloads and whose self-update replaces dest rather than the test binary.
// It returns the helper, its own HTTP server and the file that will be replaced.
func updatableHelper(t *testing.T, runner helper.Runner, serve func(http.ResponseWriter, *http.Request)) (*helper.Helper, *httptest.Server, string) {
	t.Helper()
	downloads := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/downloads/") {
			http.NotFound(w, r)
			return
		}
		serve(w, r)
	}))
	t.Cleanup(downloads.Close)

	binDir := t.TempDir()
	dest := filepath.Join(binDir, "proxback-helper")
	if err := os.WriteFile(dest, []byte("the previous build"), 0o755); err != nil { //nolint:gosec // a fake binary
		t.Fatalf("write current binary: %v", err)
	}

	cfgDir := t.TempDir()
	raw, err := json.Marshal(map[string]any{
		"serverUrl":    downloads.URL,
		"helperId":     "helper-1",
		"apiKey":       "api-key-1",
		"accessSecret": testSecret,
		"node":         testNode,
		"port":         9007,
	})
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, helper.ConfigFileName), raw, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	h, err := helper.New(helper.Config{
		ConfigDir: cfgDir, Runner: runner, Logger: discardLog(), BinaryPath: dest,
	})
	if err != nil {
		t.Fatalf("new helper: %v", err)
	}
	if err := h.Enroll(context.Background()); err != nil {
		t.Fatalf("load enrollment: %v", err)
	}
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	return h, srv, dest
}

// serveBytes answers every download with body.
func serveBytes(body []byte, contentType string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(body)
	}
}

// postUpdate asks a helper to update itself.
func postUpdate(t *testing.T, srv *httptest.Server, secret string, req helpermgr.UpdateRequest) *http.Response {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("encode update request: %v", err)
	}
	return request(t, http.MethodPost, srv.URL+"/update", secret, strings.NewReader(string(raw)))
}

func assertNoStagingFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".proxback-update-") {
			t.Errorf("a staging file survived: %s", e.Name())
		}
	}
}

func assertPreviousBuildIntact(t *testing.T, dest string) {
	t.Helper()
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("the previous binary is gone: %v", err)
	}
	if string(got) != "the previous build" {
		t.Fatalf("the previous binary was modified: %q", got)
	}
	assertNoStagingFiles(t, filepath.Dir(dest))
}

func TestUpdateReplacesTheBinary(t *testing.T) {
	_, srv, dest := updatableHelper(t, &fakeRunner{}, serveBytes(newHelperBinary, ""))

	resp := postUpdate(t, srv, testSecret, helpermgr.UpdateRequest{
		Version: "0.6.2", Asset: "proxback-helper-linux-amd64",
		Sha256: sha256Hex(newHelperBinary), SizeBytes: int64(len(newHelperBinary)),
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out helpermgr.UpdateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !out.OK || out.Version != "0.6.2" || !out.Restarting {
		t.Fatalf("response = %+v", out)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if string(got) != string(newHelperBinary) {
		t.Fatalf("installed binary is not the downloaded one (%d bytes)", len(got))
	}
	assertNoStagingFiles(t, filepath.Dir(dest))
	if _, err := os.Stat(dest + ".old"); err == nil {
		t.Error("the previous binary was left behind on a platform that can remove it")
	}
}

func TestUpdateRejectsATruncatedDownload(t *testing.T) {
	_, srv, dest := updatableHelper(t, &fakeRunner{}, serveBytes(newHelperBinary[:16], ""))

	resp := postUpdate(t, srv, testSecret, helpermgr.UpdateRequest{
		Version: "0.6.2", Asset: "proxback-helper-linux-amd64",
		Sha256: sha256Hex(newHelperBinary), SizeBytes: int64(len(newHelperBinary)),
	})
	if resp.StatusCode != http.StatusBadGateway {
		resp.Body.Close()
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if msg := errorBody(t, resp); !strings.Contains(msg, "truncated") {
		t.Errorf("error = %q, want it to name the truncation", msg)
	}
	assertPreviousBuildIntact(t, dest)
}

func TestUpdateRejectsTheWrongContent(t *testing.T) {
	wrong := make([]byte, len(newHelperBinary))
	copy(wrong, "a different build entirely")
	_, srv, dest := updatableHelper(t, &fakeRunner{}, serveBytes(wrong, ""))

	resp := postUpdate(t, srv, testSecret, helpermgr.UpdateRequest{
		Version: "0.6.2", Asset: "proxback-helper-linux-amd64",
		Sha256: sha256Hex(newHelperBinary), SizeBytes: int64(len(newHelperBinary)),
	})
	if resp.StatusCode != http.StatusBadGateway {
		resp.Body.Close()
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if msg := errorBody(t, resp); !strings.Contains(msg, "checksum mismatch") {
		t.Errorf("error = %q, want a checksum mismatch", msg)
	}
	assertPreviousBuildIntact(t, dest)
}

func TestUpdateRejectsAWebPage(t *testing.T) {
	page := []byte("<!DOCTYPE html><html><body>proxy error</body></html>")
	_, srv, dest := updatableHelper(t, &fakeRunner{}, serveBytes(page, "application/octet-stream"))

	// No size, no checksum: the weakest instruction the helper must still refuse.
	resp := postUpdate(t, srv, testSecret, helpermgr.UpdateRequest{
		Version: "0.6.2", Asset: "proxback-helper-linux-amd64",
	})
	if resp.StatusCode != http.StatusBadGateway {
		resp.Body.Close()
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if msg := errorBody(t, resp); !strings.Contains(msg, "HTML page") {
		t.Errorf("error = %q, want the HTML page refused", msg)
	}
	assertPreviousBuildIntact(t, dest)
}

// blockingExportRunner holds an export open until the test releases it, so the
// helper genuinely has work in flight rather than merely claiming to.
type blockingExportRunner struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingExportRunner) Run(ctx context.Context, c helper.Command) (string, error) {
	b.once.Do(func() { close(b.started) })
	if c.Stdout != nil {
		_, _ = c.Stdout.Write([]byte("VMA"))
	}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return "", nil
}

// TestUpdateRefusesWhileAnExportIsRunning is the safety rule from the node's
// side: the helper knows whether vzdump is streaming and the server does not,
// so the helper is the one that says no.
func TestUpdateRefusesWhileAnExportIsRunning(t *testing.T) {
	runner := &blockingExportRunner{started: make(chan struct{}), release: make(chan struct{})}
	_, srv, dest := updatableHelper(t, runner, serveBytes(newHelperBinary, ""))

	exportDone := make(chan struct{})
	go func() {
		defer close(exportDone)
		resp := request(t, http.MethodGet, srv.URL+"/export/101", testSecret, nil)
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
	}()
	select {
	case <-runner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the export never started")
	}

	resp := postUpdate(t, srv, testSecret, helpermgr.UpdateRequest{
		Version: "0.6.2", Asset: "proxback-helper-linux-amd64",
		Sha256: sha256Hex(newHelperBinary), SizeBytes: int64(len(newHelperBinary)),
	})
	if resp.StatusCode != http.StatusConflict {
		resp.Body.Close()
		t.Fatalf("status = %d, want 409 while an export is in flight", resp.StatusCode)
	}
	if msg := errorBody(t, resp); !strings.Contains(msg, "in flight") {
		t.Errorf("error = %q, want it to say work is in flight", msg)
	}
	assertPreviousBuildIntact(t, dest)

	// Once the export finishes the same request is accepted.
	close(runner.release)
	<-exportDone
	ok := postUpdate(t, srv, testSecret, helpermgr.UpdateRequest{
		Version: "0.6.2", Asset: "proxback-helper-linux-amd64",
		Sha256: sha256Hex(newHelperBinary), SizeBytes: int64(len(newHelperBinary)),
	})
	defer ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("status after the export finished = %d, want 200", ok.StatusCode)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(newHelperBinary) {
		t.Fatal("the binary was not replaced once the node was idle")
	}
}

func TestUpdateRequiresTheAccessSecret(t *testing.T) {
	_, srv, dest := updatableHelper(t, &fakeRunner{}, serveBytes(newHelperBinary, ""))
	resp := postUpdate(t, srv, "not-the-secret", helpermgr.UpdateRequest{
		Asset: "proxback-helper-linux-amd64",
	})
	if resp.StatusCode != http.StatusUnauthorized {
		resp.Body.Close()
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
	assertPreviousBuildIntact(t, dest)
}

func TestUpdateRequiresAnAsset(t *testing.T) {
	_, srv, dest := updatableHelper(t, &fakeRunner{}, serveBytes(newHelperBinary, ""))
	resp := postUpdate(t, srv, testSecret, helpermgr.UpdateRequest{Version: "0.6.2"})
	if resp.StatusCode != http.StatusBadRequest {
		resp.Body.Close()
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	assertPreviousBuildIntact(t, dest)
}

// TestUpdateStopsTheRunLoop proves the restart actually happens: Run returns
// ErrRestartRequired, which is what makes the command exit non-zero so systemd
// starts the new binary.
func TestUpdateStopsTheRunLoop(t *testing.T) {
	downloads := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(newHelperBinary)
	}))
	t.Cleanup(downloads.Close)

	binDir := t.TempDir()
	dest := filepath.Join(binDir, "proxback-helper")
	if err := os.WriteFile(dest, []byte("the previous build"), 0o755); err != nil { //nolint:gosec // a fake binary
		t.Fatalf("write current binary: %v", err)
	}
	// A port well away from the 8007 default, so a real helper on the
	// developer's machine cannot make this test flake.
	const port = 18108
	cfgDir := t.TempDir()
	raw, _ := json.Marshal(map[string]any{
		"serverUrl": downloads.URL, "helperId": "helper-1", "apiKey": "k",
		"accessSecret": testSecret, "node": testNode, "port": port,
	})
	if err := os.WriteFile(filepath.Join(cfgDir, helper.ConfigFileName), raw, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	h, err := helper.New(helper.Config{
		ConfigDir: cfgDir, Runner: &fakeRunner{}, Logger: discardLog(),
		BinaryPath: dest, HeartbeatInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("new helper: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- h.Run(ctx) }()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForHealthz(t, base)
	req := helpermgr.UpdateRequest{
		Version: "0.6.2", Asset: "proxback-helper-linux-amd64",
		Sha256: sha256Hex(newHelperBinary), SizeBytes: int64(len(newHelperBinary)),
	}
	body, _ := json.Marshal(req)
	resp := request(t, http.MethodPost, base+"/update", testSecret, strings.NewReader(string(body)))
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	select {
	case err := <-runErr:
		if !errors.Is(err, helper.ErrRestartRequired) {
			t.Fatalf("Run returned %v, want ErrRestartRequired", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the run loop did not stop after an update was installed")
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(newHelperBinary) {
		t.Fatal("the binary was not replaced")
	}
}

func waitForHealthz(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/healthz") //nolint:noctx // short-lived test probe
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the helper never started serving on %s", base)
}
