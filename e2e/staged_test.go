package e2e

// The staged agent and node helper binaries, end to end.
//
// This is the production failure that motivated the whole file: a server that
// self-updated 0.2.x -> 0.6.0 kept serving the agent its installer had staged in
// <data>/downloads. That agent predated Windows-service support, so every Windows
// install failed with SCM error 1053, and the console showed a healthy,
// up-to-date server the whole time. The two halves of the fix are asserted here
// through the public API: an already-drifted installation heals itself on
// startup, and applying an update restages the binaries from the same release.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path"
	"sort"
	"strings"
	"testing"
	"time"

	"proxback/internal/app"
	"proxback/internal/update"
	"proxback/internal/version"
)

const (
	stagedAgentLinux   = "proxback-agent-linux-amd64"
	stagedAgentWindows = "proxback-agent-windows-amd64.exe"
	stagedHelperLinux  = "proxback-helper-linux-amd64"
)

// apiStagedArtifact mirrors one entry of GET /api/downloads/status.
type apiStagedArtifact struct {
	Name          string  `json:"name"`
	Kind          string  `json:"kind"`
	OS            string  `json:"os"`
	Present       bool    `json:"present"`
	Version       string  `json:"version"`
	MatchesServer bool    `json:"matchesServer"`
	SizeBytes     int64   `json:"sizeBytes"`
	ModifiedAt    *string `json:"modifiedAt"`
	Reason        string  `json:"reason"`
}

type apiDownloadsStatus struct {
	ServerVersion string              `json:"serverVersion"`
	AllMatch      bool                `json:"allMatch"`
	Artifacts     []apiStagedArtifact `json:"artifacts"`
}

func (s apiDownloadsStatus) byName(t *testing.T, name string) apiStagedArtifact {
	t.Helper()
	for _, a := range s.Artifacts {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("%s is not reported by /api/downloads/status: %+v", name, s.Artifacts)
	return apiStagedArtifact{}
}

// ---------------------------------------------------------------- fake release

// releaseSim serves a GitHub release, by "latest" and by tag, whose assets are
// the three staged binaries plus a checksums.txt computed over them. The payloads
// carry the version in their bytes so a test can tell one build from another.
type releaseSim struct {
	srv   *httptest.Server
	tag   string
	files map[string][]byte
}

func newReleaseSim(t *testing.T, ver string) *releaseSim {
	t.Helper()
	r := &releaseSim{tag: "v" + ver, files: map[string][]byte{
		stagedAgentLinux:   []byte("ELF proxback-agent " + ver),
		stagedAgentWindows: []byte("MZ proxback-agent " + ver),
		stagedHelperLinux:  []byte("ELF proxback-helper " + ver),
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/tester/proxback/releases/latest", r.serveRelease)
	mux.HandleFunc("/repos/tester/proxback/releases/tags/", func(w http.ResponseWriter, req *http.Request) {
		if path.Base(req.URL.Path) != r.tag {
			http.NotFound(w, req)
			return
		}
		r.serveRelease(w, req)
	})
	mux.HandleFunc("/dl/", r.serveAsset)
	r.srv = httptest.NewServer(mux)
	t.Cleanup(r.srv.Close)
	return r
}

func (r *releaseSim) names() []string {
	out := make([]string, 0, len(r.files))
	for name := range r.files {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (r *releaseSim) serveRelease(w http.ResponseWriter, _ *http.Request) {
	assets := make([]string, 0, len(r.files)+1)
	for _, name := range r.names() {
		assets = append(assets, fmt.Sprintf(`{"name":%q,"size":%d,"browser_download_url":"%s/dl/%s"}`,
			name, len(r.files[name]), r.srv.URL, name))
	}
	assets = append(assets, fmt.Sprintf(
		`{"name":"checksums.txt","size":1,"browser_download_url":"%s/dl/checksums.txt"}`, r.srv.URL))
	fmt.Fprintf(w, `{"tag_name":%q,"name":"ProxBack %s","body":"notes","html_url":"http://x",`+
		`"published_at":"2026-07-28T12:00:00Z","assets":[%s]}`,
		r.tag, r.tag, strings.Join(assets, ","))
}

func (r *releaseSim) serveAsset(w http.ResponseWriter, req *http.Request) {
	name := path.Base(req.URL.Path)
	if name == "checksums.txt" {
		var b strings.Builder
		for _, n := range r.names() {
			sum := sha256.Sum256(r.files[n])
			fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(sum[:]), n)
		}
		_, _ = w.Write([]byte(b.String()))
		return
	}
	body, ok := r.files[name]
	if !ok {
		http.NotFound(w, req)
		return
	}
	_, _ = w.Write(body)
}

// ---------------------------------------------------------------- harness

// stagedHarness is a complete server on a caller-prepared data dir, signed in as
// the seeded administrator. It carries no simulators: nothing here runs a backup.
type stagedHarness struct {
	t       *testing.T
	client  *http.Client
	base    string
	dataDir string
}

func newStagedHarness(t *testing.T, dataDir string) *stagedHarness {
	t.Helper()
	instance, err := app.New(context.Background(), app.Options{
		DataDir: dataDir,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		if err := instance.Close(); err != nil {
			t.Logf("server shutdown: %v", err)
		}
	})
	srv := httptest.NewServer(instance.Handler)
	t.Cleanup(srv.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	h := &stagedHarness{t: t, client: &http.Client{Jar: jar, Timeout: time.Minute}, base: srv.URL, dataDir: dataDir}
	// A fresh install seeds admin/admin, so no setup call is needed.
	h.post("/api/login", map[string]string{"username": "admin", "password": "admin"})
	return h
}

func (h *stagedHarness) post(path string, body any) (int, []byte) {
	h.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		h.t.Fatalf("encode %s: %v", path, err)
	}
	req, err := http.NewRequest(http.MethodPost, h.base+path, bytes.NewReader(raw)) //nolint:noctx // test helper
	if err != nil {
		h.t.Fatalf("build POST %s: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	return h.send(req)
}

func (h *stagedHarness) get(path string) (int, []byte) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.base+path, nil) //nolint:noctx // test helper
	if err != nil {
		h.t.Fatalf("build GET %s: %v", path, err)
	}
	return h.send(req)
}

func (h *stagedHarness) send(req *http.Request) (int, []byte) {
	h.t.Helper()
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("%s %s: read body: %v", req.Method, req.URL.Path, err)
	}
	return resp.StatusCode, raw
}

func (h *stagedHarness) status() apiDownloadsStatus {
	h.t.Helper()
	code, raw := h.get("/api/downloads/status")
	if code != http.StatusOK {
		h.t.Fatalf("GET /api/downloads/status = %d (%s)", code, raw)
	}
	var out apiDownloadsStatus
	if err := json.Unmarshal(raw, &out); err != nil {
		h.t.Fatalf("decode %s: %v", raw, err)
	}
	if out.ServerVersion != version.Version {
		h.t.Fatalf("serverVersion = %q, want %q", out.ServerVersion, version.Version)
	}
	return out
}

// awaitAllMatch polls until every staged binary matches the server. The startup
// reconciliation is a background pass by design — it must never delay startup —
// so polling is the only honest way to observe it.
func (h *stagedHarness) awaitAllMatch(timeout time.Duration) apiDownloadsStatus {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	var last apiDownloadsStatus
	for time.Now().Before(deadline) {
		last = h.status()
		if last.AllMatch {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatalf("staged binaries still do not match after %s: %+v", timeout, last.Artifacts)
	return last
}

// prepareStaleDataDir builds the installation the bug produced: a downloads
// directory holding the agent and helper an old installer left behind.
func prepareStaleDataDir(t *testing.T) string {
	t.Helper()
	dataDir := t.TempDir()
	if err := os.MkdirAll(update.StagedDir(dataDir), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{stagedAgentLinux, stagedAgentWindows, stagedHelperLinux} {
		if err := os.WriteFile(update.StagedPath(dataDir, name), []byte("stale 0.2.1 build of "+name), 0o755); err != nil { //nolint:gosec // fake binary
			t.Fatal(err)
		}
	}
	// The Windows agent is the one that failed in production, and the installer
	// that staged it recorded nothing at all — so its version is unknowable
	// rather than merely old.
	if err := os.WriteFile(update.StagedVersionPath(dataDir, stagedAgentLinux), []byte("0.2.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dataDir
}

// TestStagedBinariesHealOnStartup is the user's server, restarted after the fix:
// the stale binaries are replaced by the ones from its own release, without
// anybody intervening, and the download endpoint hands out the new ones.
func TestStagedBinariesHealOnStartup(t *testing.T) {
	rel := newReleaseSim(t, version.Version)
	t.Setenv("PROXBACK_UPDATE_API", rel.srv.URL)
	t.Setenv("PROXBACK_UPDATE_REPO", "tester/proxback")

	dataDir := prepareStaleDataDir(t)
	h := newStagedHarness(t, dataDir)

	got := h.awaitAllMatch(30 * time.Second)
	for _, name := range []string{stagedAgentLinux, stagedAgentWindows, stagedHelperLinux} {
		a := got.byName(t, name)
		if !a.Present || !a.MatchesServer || a.Version != version.Version || a.Reason != "" {
			t.Errorf("healed artifact %s = %+v", name, a)
		}
		if a.ModifiedAt == nil || a.SizeBytes != int64(len(rel.files[name])) {
			t.Errorf("healed artifact %s = %+v, want the release's size and a modification time", name, a)
		}
		// What a guest or a node would actually receive is the new build.
		code, body := h.get("/downloads/" + name)
		if code != http.StatusOK || !bytes.Equal(body, rel.files[name]) {
			t.Errorf("GET /downloads/%s = %d (%s), want the refreshed binary", name, code, body)
		}
	}
}

// TestStagedBinariesRefreshedByAnUpdate simulates the other half: a server that
// has just installed a new release restages the binaries it hands out from that
// same release. The staging step is driven directly rather than through
// POST /api/update/apply, because that call swaps the running executable — which
// here is the test binary itself.
func TestStagedBinariesRefreshedByAnUpdate(t *testing.T) {
	// No release repository for this server's own version: startup reconciliation
	// finds nothing and leaves the stale binaries alone, which is the state an
	// update then has to correct.
	noReleases := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(noReleases.Close)
	t.Setenv("PROXBACK_UPDATE_API", noReleases.URL)
	t.Setenv("PROXBACK_UPDATE_REPO", "tester/proxback")

	dataDir := prepareStaleDataDir(t)
	h := newStagedHarness(t, dataDir)

	// Before: the console can already see the operator is about to hand out the
	// wrong build. That visibility is the point of the endpoint.
	before := h.status()
	if before.AllMatch {
		t.Fatal("a stale installation reports every staged binary as matching")
	}
	stale := before.byName(t, stagedAgentWindows)
	if !stale.Present || stale.Version != "" || stale.MatchesServer || stale.Reason == "" {
		t.Fatalf("stale Windows agent = %+v, want present, version unknown, with a reason", stale)
	}

	// The update: a release for this server's own version, staged exactly as
	// POST /api/update/apply stages it.
	rel := newReleaseSim(t, version.Version)
	t.Setenv("PROXBACK_UPDATE_API", rel.srv.URL)
	checker := update.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	release, err := checker.Latest(context.Background())
	if err != nil {
		t.Fatalf("fetch fake release: %v", err)
	}
	res, err := checker.RefreshStaged(context.Background(), release, dataDir)
	if err != nil {
		t.Fatalf("RefreshStaged: %v", err)
	}
	if len(res.Updated) != 3 || len(res.Failed) != 0 || len(res.Skipped) != 0 {
		t.Fatalf("refresh result = %+v", res)
	}

	// After: the staged agent is the new one, and the API says so.
	after := h.status()
	if !after.AllMatch {
		t.Fatalf("staged binaries still mismatched after the update: %+v", after.Artifacts)
	}
	agent := after.byName(t, stagedAgentWindows)
	if !agent.MatchesServer || agent.Version != version.Version {
		t.Fatalf("staged Windows agent after the update = %+v", agent)
	}
	code, body := h.get("/downloads/" + stagedAgentWindows)
	if code != http.StatusOK || !bytes.Equal(body, rel.files[stagedAgentWindows]) {
		t.Fatalf("GET /downloads/%s = %d (%s), want the release's agent", stagedAgentWindows, code, body)
	}
}
