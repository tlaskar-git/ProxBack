package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"proxback/internal/agentmgr"
)

// newBinary is a stand-in for a real agent build. Nothing here cares what is in
// it, only that the bytes that arrive are the bytes the server measured.
var newBinary = []byte("MZ\x00\x00 proxback-agent 0.6.2 " + strings.Repeat("payload", 64))

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// downloadsServer stands in for the server's /downloads endpoint. body is what
// it serves; contentType overrides the default when a test needs to imitate a
// proxy answering with a web page.
func downloadsServer(t *testing.T, body []byte, contentType string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/downloads/") {
			http.NotFound(w, r)
			return
		}
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		} else {
			w.Header().Set("Content-Type", "application/octet-stream")
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// testAgent builds an agent that is already enrolled against srv and whose
// self-update would replace dest rather than the test binary.
func testAgent(t *testing.T, serverURL, dest string) *Agent {
	t.Helper()
	a, err := New(Config{
		ServerURL:  serverURL,
		ConfigDir:  t.TempDir(),
		BinaryPath: dest,
		Logger:     testLogger(),
	})
	if err != nil {
		t.Fatalf("build agent: %v", err)
	}
	a.self = storedConfig{ServerURL: serverURL, AgentID: "ag-1", APIKey: "key"}
	return a
}

// writeCurrentBinary lays down the binary a self-update will replace.
func writeCurrentBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dest := filepath.Join(dir, "proxback-agent")
	if err := os.WriteFile(dest, []byte("the previous build"), 0o755); err != nil { //nolint:gosec // a fake binary
		t.Fatalf("write current binary: %v", err)
	}
	return dest
}

// assertNoTempFiles is the "leaves nothing behind" half of every case below: a
// failed update that litters the install directory with half-downloaded
// binaries is its own kind of damage.
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), updateTempPrefix) {
			t.Errorf("a staging file survived: %s", e.Name())
		}
	}
}

func TestSelfUpdateReplacesTheBinary(t *testing.T) {
	dest := writeCurrentBinary(t)
	srv := downloadsServer(t, newBinary, "")
	a := testAgent(t, srv.URL, dest)

	d := agentmgr.Dispatch{
		Type: agentmgr.DispatchUpdate, Version: "0.6.2",
		Asset: "proxback-agent-linux-amd64", Sha256: sha256Hex(newBinary),
		SizeBytes: int64(len(newBinary)),
	}
	if err := a.runSelfUpdate(context.Background(), d); err != nil {
		t.Fatalf("self-update: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if string(got) != string(newBinary) {
		t.Fatalf("installed binary is not the downloaded one (%d bytes)", len(got))
	}
	if !a.restartRequired() {
		t.Error("a successful update did not ask for a restart")
	}
	assertNoTempFiles(t, filepath.Dir(dest))
	// The previous binary is moved aside, not left as a permanent copy: on
	// Linux, where the running image is not held open, it is cleaned up.
	if _, err := os.Stat(dest + oldBinarySuffix); err == nil {
		t.Error("the previous binary was left behind on a platform that can remove it")
	}
}

func TestSelfUpdateRejectsATruncatedDownload(t *testing.T) {
	dest := writeCurrentBinary(t)
	// The server serves less than it promised: a cut connection, a proxy that
	// gave up, a partially staged file.
	srv := downloadsServer(t, newBinary[:20], "")
	a := testAgent(t, srv.URL, dest)

	err := a.runSelfUpdate(context.Background(), agentmgr.Dispatch{
		Version: "0.6.2", Asset: "proxback-agent-linux-amd64",
		Sha256: sha256Hex(newBinary), SizeBytes: int64(len(newBinary)),
	})
	if err == nil {
		t.Fatal("a truncated download was accepted")
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("error = %v, want it to name the truncation", err)
	}
	assertOriginalIntact(t, dest)
	if a.restartRequired() {
		t.Error("a failed update asked for a restart")
	}
}

func TestSelfUpdateRejectsTheWrongContent(t *testing.T) {
	dest := writeCurrentBinary(t)
	// The right length, the wrong bytes: exactly what a size check alone would
	// wave through.
	wrong := make([]byte, len(newBinary))
	copy(wrong, "a different build entirely")
	srv := downloadsServer(t, wrong, "")
	a := testAgent(t, srv.URL, dest)

	err := a.runSelfUpdate(context.Background(), agentmgr.Dispatch{
		Version: "0.6.2", Asset: "proxback-agent-linux-amd64",
		Sha256: sha256Hex(newBinary), SizeBytes: int64(len(newBinary)),
	})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("error = %v, want a checksum mismatch", err)
	}
	assertOriginalIntact(t, dest)
}

func TestSelfUpdateRejectsAWebPage(t *testing.T) {
	dest := writeCurrentBinary(t)
	page := []byte("<!DOCTYPE html><html><body>Sign in to the guest network</body></html>")
	// A captive portal answers 200 and calls itself anything it likes, so the
	// body has to be the signal. The dispatch carries no size or checksum here,
	// which is the weakest case the agent must still refuse.
	srv := downloadsServer(t, page, "application/octet-stream")
	a := testAgent(t, srv.URL, dest)

	err := a.runSelfUpdate(context.Background(), agentmgr.Dispatch{
		Version: "0.6.2", Asset: "proxback-agent-linux-amd64",
	})
	if err == nil || !strings.Contains(err.Error(), "HTML page") {
		t.Fatalf("error = %v, want the HTML page to be refused", err)
	}
	assertOriginalIntact(t, dest)
}

func TestSelfUpdateRejectsAnErrorStatus(t *testing.T) {
	dest := writeCurrentBinary(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unknown download", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	a := testAgent(t, srv.URL, dest)

	err := a.runSelfUpdate(context.Background(), agentmgr.Dispatch{
		Version: "0.6.2", Asset: "proxback-agent-linux-amd64",
	})
	if err == nil || !strings.Contains(err.Error(), "http 404") {
		t.Fatalf("error = %v, want the 404 reported", err)
	}
	assertOriginalIntact(t, dest)
}

func TestSelfUpdateNeedsAnAsset(t *testing.T) {
	dest := writeCurrentBinary(t)
	a := testAgent(t, "http://127.0.0.1:1", dest)
	if err := a.runSelfUpdate(context.Background(), agentmgr.Dispatch{Version: "0.6.2"}); err == nil {
		t.Fatal("an update dispatch with no asset was accepted")
	}
	assertOriginalIntact(t, dest)
}

// assertOriginalIntact is the promise a failed update makes: the binary the
// service is running is still there, unchanged, and nothing was staged beside
// it.
func assertOriginalIntact(t *testing.T, dest string) {
	t.Helper()
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("the previous binary is gone: %v", err)
	}
	if string(got) != "the previous build" {
		t.Fatalf("the previous binary was modified: %q", got)
	}
	assertNoTempFiles(t, filepath.Dir(dest))
}

// TestPollAppliesAnUpdateDispatch drives the whole delivery path the server
// uses: the agent polls, the dispatch is on the poll response, and the agent
// comes back asking to be restarted.
func TestPollAppliesAnUpdateDispatch(t *testing.T) {
	dest := writeCurrentBinary(t)
	var beats []agentmgr.HeartbeatRequest
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agents/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var beat agentmgr.HeartbeatRequest
		_ = json.NewDecoder(r.Body).Decode(&beat)
		beats = append(beats, beat)
		jobs := []agentmgr.Dispatch{}
		if len(beats) == 1 {
			jobs = append(jobs, agentmgr.Dispatch{
				Type: agentmgr.DispatchUpdate, Version: "0.6.2",
				Asset: "proxback-agent-linux-amd64", Sha256: sha256Hex(newBinary),
				SizeBytes: int64(len(newBinary)),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jobs": jobs, "serverVersion": "0.6.2"})
	})
	mux.HandleFunc("/downloads/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(newBinary)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := testAgent(t, srv.URL, dest)
	err := a.pollOnce(context.Background())
	if !errors.Is(err, ErrRestartRequired) {
		t.Fatalf("pollOnce after an update = %v, want ErrRestartRequired", err)
	}
	if len(beats) != 1 || beats[0].Version != Version || beats[0].OS == "" || beats[0].Arch == "" {
		t.Fatalf("heartbeat body = %+v, want the running version, os and arch", beats)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != string(newBinary) {
		t.Fatalf("the binary was not replaced (err %v, %d bytes)", err, len(got))
	}
}

// TestFailedUpdateDoesNotStopTheAgent: an update that cannot be applied is a
// logged failure, not the end of the agent. The guest stays protected by the
// build it already has.
func TestFailedUpdateDoesNotStopTheAgent(t *testing.T) {
	dest := writeCurrentBinary(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agents/heartbeat", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []agentmgr.Dispatch{{
			Type: agentmgr.DispatchUpdate, Version: "0.6.2",
			Asset: "proxback-agent-linux-amd64", SizeBytes: 99999,
		}}})
	})
	mux.HandleFunc("/downloads/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("short"))
	})
	// A failed self-update must not be reported as a failed run: there is no
	// run, and /api/agents/runs//fail does not exist. A request there fails the
	// test rather than 404ing quietly.
	mux.HandleFunc("/api/agents/runs/", func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("a failed self-update reported a run failure to %s", r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := testAgent(t, srv.URL, dest)
	if err := a.pollOnce(context.Background()); err != nil {
		t.Fatalf("a failed update ended the poll cycle: %v", err)
	}
	if a.restartRequired() {
		t.Error("a failed update asked for a restart")
	}
	assertOriginalIntact(t, dest)
}
