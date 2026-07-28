package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

const (
	agentLinux   = "proxback-agent-linux-amd64"
	agentWindows = "proxback-agent-windows-amd64.exe"
	helperLinux  = "proxback-helper-linux-amd64"
)

// fakeRelease serves a GitHub release — by "latest" and by tag — whose assets
// are the three staged binaries plus a checksums.txt computed over them.
type fakeRelease struct {
	srv *httptest.Server
	tag string

	mu      sync.Mutex
	files   map[string][]byte // asset name -> payload
	corrupt map[string]bool   // asset names whose checksums.txt entry is wrong
	broken  map[string]bool   // asset names whose download fails outright
	hits    map[string]int    // downloads per asset
}

func newFakeRelease(t *testing.T, tag string, files map[string][]byte) *fakeRelease {
	t.Helper()
	f := &fakeRelease{
		tag: tag, files: files,
		corrupt: map[string]bool{}, broken: map[string]bool{}, hits: map[string]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/tester/proxback/releases/latest", f.serveRelease)
	mux.HandleFunc("/repos/tester/proxback/releases/tags/", func(w http.ResponseWriter, r *http.Request) {
		if path.Base(r.URL.Path) != f.tag {
			http.NotFound(w, r)
			return
		}
		f.serveRelease(w, r)
	})
	mux.HandleFunc("/dl/", f.serveAsset)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// names returns the advertised asset names in a stable order.
func (f *fakeRelease) names() []string {
	out := make([]string, 0, len(f.files))
	for name := range f.files {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (f *fakeRelease) checksums() string {
	var b strings.Builder
	for _, name := range f.names() {
		sum := sha256.Sum256(f.files[name])
		hexSum := hex.EncodeToString(sum[:])
		if f.corrupt[name] {
			hexSum = "deadbeef" + hexSum[8:]
		}
		fmt.Fprintf(&b, "%s  %s\n", hexSum, name)
	}
	return b.String()
}

func (f *fakeRelease) serveRelease(w http.ResponseWriter, _ *http.Request) {
	assets := make([]string, 0, len(f.files)+1)
	for _, name := range f.names() {
		assets = append(assets, fmt.Sprintf(`{"name":%q,"size":%d,"browser_download_url":"%s/dl/%s"}`,
			name, len(f.files[name]), f.srv.URL, name))
	}
	assets = append(assets, fmt.Sprintf(
		`{"name":"checksums.txt","size":1,"browser_download_url":"%s/dl/checksums.txt"}`, f.srv.URL))
	fmt.Fprintf(w, `{"tag_name":%q,"name":"ProxBack %s","body":"notes","html_url":"http://x",`+
		`"published_at":"2026-07-26T12:00:00Z","assets":[%s]}`,
		f.tag, f.tag, strings.Join(assets, ","))
}

func (f *fakeRelease) serveAsset(w http.ResponseWriter, r *http.Request) {
	name := path.Base(r.URL.Path)
	f.mu.Lock()
	f.hits[name]++
	broken := f.broken[name]
	f.mu.Unlock()
	if name == "checksums.txt" {
		_, _ = w.Write([]byte(f.checksums()))
		return
	}
	body, ok := f.files[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if broken {
		// A download that dies halfway: some bytes arrive, then the connection
		// is cut. Nothing under the real name may survive this.
		w.Header().Set("Content-Length", fmt.Sprint(len(body)+64))
		_, _ = w.Write(body[:len(body)/2])
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		panic(http.ErrAbortHandler)
	}
	_, _ = w.Write(body)
}

func (f *fakeRelease) downloads(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[name]
}

// stagedPayloads is a release that publishes all three staged binaries.
func stagedPayloads(tag string) map[string][]byte {
	return map[string][]byte{
		agentLinux:                          []byte("ELF agent " + tag),
		agentWindows:                        []byte("MZ agent " + tag),
		helperLinux:                         []byte("ELF helper " + tag),
		ServerAssetName("linux", "amd64"):   []byte("ELF server " + tag),
		ServerAssetName("windows", "amd64"): []byte("MZ server " + tag),
	}
}

// stage writes a binary and, when ver is not empty, its recorded version — the
// state an installation is in before a refresh reaches it.
func stage(t *testing.T, dataDir, name, body, ver string) {
	t.Helper()
	if err := os.MkdirAll(StagedDir(dataDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(StagedPath(dataDir, name), []byte(body), 0o755); err != nil { //nolint:gosec // fake binary
		t.Fatal(err)
	}
	if ver != "" {
		if err := os.WriteFile(StagedVersionPath(dataDir, name), []byte(ver+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readStaged(t *testing.T, dataDir, name string) string {
	t.Helper()
	raw, err := os.ReadFile(StagedPath(dataDir, name))
	if err != nil {
		t.Fatalf("read staged %s: %v", name, err)
	}
	return string(raw)
}

// requireNoTempFiles proves a pass left no partial artifact behind: a stray temp
// file is a truncated binary waiting to be renamed into place by mistake.
func requireNoTempFiles(t *testing.T, dataDir string) {
	t.Helper()
	strays, err := filepath.Glob(filepath.Join(StagedDir(dataDir), ".proxback-staged-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(strays) > 0 {
		t.Fatalf("temp files survived the refresh: %v", strays)
	}
}

func TestRefreshStagedWritesEveryArtifact(t *testing.T) {
	ctx := context.Background()
	rel := newFakeRelease(t, "v9.9.9", stagedPayloads("9.9.9"))
	c := newTestChecker(rel.srv.URL)
	dataDir := t.TempDir()

	release, err := c.Latest(ctx)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	res, err := c.RefreshStaged(ctx, release, dataDir)
	if err != nil {
		t.Fatalf("RefreshStaged: %v", err)
	}
	if len(res.Updated) != 3 || len(res.Skipped) != 0 || len(res.Failed) != 0 {
		t.Fatalf("refresh result = %+v", res)
	}
	for _, name := range []string{agentLinux, agentWindows, helperLinux} {
		want := string(stagedPayloads("9.9.9")[name])
		if got := readStaged(t, dataDir, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
		ver, err := StagedVersion(dataDir, name)
		if err != nil || ver != "9.9.9" {
			t.Errorf("recorded version of %s = %q, %v", name, ver, err)
		}
	}
	// The server binary is not a staged artifact: Apply owns it, and staging a
	// copy of it would be a second thing to keep in step.
	if _, err := os.Stat(StagedPath(dataDir, ServerAssetName("linux", "amd64"))); err == nil {
		t.Error("the server binary was staged into the downloads directory")
	}
	requireNoTempFiles(t, dataDir)

	for _, st := range InspectStaged(dataDir) {
		if !st.Present || st.Version != "9.9.9" || st.Reason != "" || st.Size == 0 {
			t.Errorf("InspectStaged %s = %+v", st.Name, st)
		}
	}
}

func TestRefreshStagedRejectsCorruptedAssetWithoutTouchingTheStagedCopy(t *testing.T) {
	ctx := context.Background()
	rel := newFakeRelease(t, "v9.9.9", stagedPayloads("9.9.9"))
	rel.corrupt[agentWindows] = true
	c := newTestChecker(rel.srv.URL)
	dataDir := t.TempDir()
	stage(t, dataDir, agentWindows, "MZ agent 0.6.0", "0.6.0")

	release, err := c.Latest(ctx)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	res, err := c.RefreshStaged(ctx, release, dataDir)
	if err != nil {
		t.Fatalf("RefreshStaged returned an error for one bad asset: %v", err)
	}
	if len(res.Failed) != 1 || res.Failed[0].Name != agentWindows {
		t.Fatalf("failures = %+v, want just %s", res.Failed, agentWindows)
	}
	if !strings.Contains(res.Failed[0].Err.Error(), "checksum mismatch") {
		t.Errorf("failure reason = %v, want a checksum mismatch", res.Failed[0].Err)
	}
	// The previously staged copy — and its recorded version — must be untouched.
	if got := readStaged(t, dataDir, agentWindows); got != "MZ agent 0.6.0" {
		t.Errorf("staged agent after a rejected download = %q", got)
	}
	if ver, err := StagedVersion(dataDir, agentWindows); err != nil || ver != "0.6.0" {
		t.Errorf("recorded version after a rejected download = %q, %v", ver, err)
	}
	// The other two are unaffected by their neighbour's failure.
	if len(res.Updated) != 2 {
		t.Errorf("updated = %v, want the two sound assets", res.Updated)
	}
	requireNoTempFiles(t, dataDir)
}

func TestRefreshStagedSurvivesATruncatedDownload(t *testing.T) {
	ctx := context.Background()
	rel := newFakeRelease(t, "v9.9.9", stagedPayloads("9.9.9"))
	rel.broken[helperLinux] = true
	c := newTestChecker(rel.srv.URL)
	dataDir := t.TempDir()
	stage(t, dataDir, helperLinux, "ELF helper 0.6.0", "0.6.0")

	release, err := c.Latest(ctx)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	res, err := c.RefreshStaged(ctx, release, dataDir)
	if err != nil {
		t.Fatalf("RefreshStaged: %v", err)
	}
	if len(res.Failed) != 1 || res.Failed[0].Name != helperLinux {
		t.Fatalf("failures = %+v, want just %s", res.Failed, helperLinux)
	}
	if got := readStaged(t, dataDir, helperLinux); got != "ELF helper 0.6.0" {
		t.Errorf("staged helper after a truncated download = %q", got)
	}
	requireNoTempFiles(t, dataDir)
}

func TestRefreshStagedSkipsAMissingPlatformAsset(t *testing.T) {
	ctx := context.Background()
	files := stagedPayloads("9.9.9")
	delete(files, agentWindows) // a release that published no Windows agent
	rel := newFakeRelease(t, "v9.9.9", files)
	c := newTestChecker(rel.srv.URL)
	dataDir := t.TempDir()

	release, err := c.Latest(ctx)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	res, err := c.RefreshStaged(ctx, release, dataDir)
	if err != nil {
		t.Fatalf("a missing platform asset failed the whole refresh: %v", err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != agentWindows {
		t.Fatalf("skipped = %v, want just %s", res.Skipped, agentWindows)
	}
	if len(res.Updated) != 2 || len(res.Failed) != 0 {
		t.Fatalf("refresh result = %+v", res)
	}
	if _, err := os.Stat(StagedPath(dataDir, agentWindows)); err == nil {
		t.Error("a Windows agent appeared although the release published none")
	}
	requireNoTempFiles(t, dataDir)
}

func TestReconcileStagedHealsAStaleInstallation(t *testing.T) {
	ctx := context.Background()
	rel := newFakeRelease(t, "v0.6.0", stagedPayloads("0.6.0"))
	c := newTestChecker(rel.srv.URL)
	dataDir := t.TempDir()

	// The installation this fixes: a 26-July agent staged by an 0.2.x installer,
	// with no recorded version at all, on a server that has since become 0.6.0.
	stage(t, dataDir, agentWindows, "MZ agent from 0.2.x", "")
	stage(t, dataDir, agentLinux, "ELF agent from 0.2.x", "0.2.1")

	res, err := c.ReconcileStaged(ctx, dataDir, "0.6.0")
	if err != nil {
		t.Fatalf("ReconcileStaged: %v", err)
	}
	if len(res.Updated) != 3 || len(res.UpToDate) != 0 {
		t.Fatalf("reconcile result = %+v", res)
	}
	for _, name := range []string{agentLinux, agentWindows, helperLinux} {
		if got, want := readStaged(t, dataDir, name), string(stagedPayloads("0.6.0")[name]); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
		if ver, err := StagedVersion(dataDir, name); err != nil || ver != "0.6.0" {
			t.Errorf("recorded version of %s = %q, %v", name, ver, err)
		}
	}
	requireNoTempFiles(t, dataDir)
}

func TestReconcileStagedLeavesMatchingBinariesAlone(t *testing.T) {
	ctx := context.Background()
	rel := newFakeRelease(t, "v0.6.0", stagedPayloads("0.6.0"))
	c := newTestChecker(rel.srv.URL)
	dataDir := t.TempDir()
	for _, name := range []string{agentLinux, agentWindows, helperLinux} {
		stage(t, dataDir, name, "already the right build of "+name, "0.6.0")
	}

	res, err := c.ReconcileStaged(ctx, dataDir, "0.6.0")
	if err != nil {
		t.Fatalf("ReconcileStaged: %v", err)
	}
	if len(res.Updated) != 0 || len(res.UpToDate) != 3 {
		t.Fatalf("reconcile result = %+v, want nothing done", res)
	}
	// Not one byte fetched: a healthy installation must cost no network at all,
	// or every restart of every server would pull three binaries for nothing.
	for _, name := range []string{agentLinux, agentWindows, helperLinux, "checksums.txt"} {
		if n := rel.downloads(name); n != 0 {
			t.Errorf("%s was downloaded %d times although it already matched", name, n)
		}
	}
	for _, name := range []string{agentLinux, agentWindows, helperLinux} {
		if got := readStaged(t, dataDir, name); got != "already the right build of "+name {
			t.Errorf("%s was rewritten: %q", name, got)
		}
	}
}

func TestReconcileStagedKeepsFilesWhenTheNetworkIsUnreachable(t *testing.T) {
	ctx := context.Background()
	// An air-gapped installation: nothing answers on the release API.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	c := newTestChecker(dead.URL)
	dataDir := t.TempDir()
	stage(t, dataDir, agentWindows, "MZ agent from 0.2.x", "0.2.1")

	if _, err := c.ReconcileStaged(ctx, dataDir, "0.6.0"); err == nil {
		t.Fatal("ReconcileStaged reported success with no network")
	}
	if got := readStaged(t, dataDir, agentWindows); got != "MZ agent from 0.2.x" {
		t.Errorf("staged agent after an unreachable network = %q", got)
	}
	if ver, err := StagedVersion(dataDir, agentWindows); err != nil || ver != "0.2.1" {
		t.Errorf("recorded version after an unreachable network = %q, %v", ver, err)
	}
	requireNoTempFiles(t, dataDir)
}

func TestReconcileStagedNeedsTheServersOwnRelease(t *testing.T) {
	ctx := context.Background()
	// The repository's newest release is 9.9.9, but this server is 0.6.0: it must
	// stage the 0.6.0 agent, not the newest one, and say so when 0.6.0 is gone.
	rel := newFakeRelease(t, "v9.9.9", stagedPayloads("9.9.9"))
	c := newTestChecker(rel.srv.URL)
	dataDir := t.TempDir()
	stage(t, dataDir, agentLinux, "ELF agent from 0.2.x", "0.2.1")

	if _, err := c.ReconcileStaged(ctx, dataDir, "0.6.0"); err == nil {
		t.Fatal("reconciliation fell back to the newest release")
	}
	if got := readStaged(t, dataDir, agentLinux); got != "ELF agent from 0.2.x" {
		t.Errorf("staged agent = %q, want the old one left in place", got)
	}
}

func TestInspectStagedReportsWhyAVersionIsUnknown(t *testing.T) {
	dataDir := t.TempDir()
	stage(t, dataDir, agentLinux, "copied in by hand", "")

	byName := map[string]StagedStatus{}
	for _, st := range InspectStaged(dataDir) {
		byName[st.Name] = st
	}
	if len(byName) != 3 {
		t.Fatalf("InspectStaged returned %d artifacts", len(byName))
	}
	handRolled := byName[agentLinux]
	if !handRolled.Present || handRolled.Version != "" || handRolled.Reason == "" {
		t.Errorf("hand-copied binary = %+v, want present with an unknown version and a reason", handRolled)
	}
	if _, err := StagedVersion(dataDir, agentLinux); err != ErrVersionUnknown {
		t.Errorf("StagedVersion for an unrecorded binary = %v, want ErrVersionUnknown", err)
	}
	absent := byName[helperLinux]
	if absent.Present || absent.Reason == "" {
		t.Errorf("missing binary = %+v, want absent with a reason", absent)
	}
}

func TestByTagFindsTheRequestedRelease(t *testing.T) {
	ctx := context.Background()
	rel := newFakeRelease(t, "v0.6.0", stagedPayloads("0.6.0"))
	c := newTestChecker(rel.srv.URL)

	// With or without the leading v, and never a different tag.
	for _, ask := range []string{"0.6.0", "v0.6.0"} {
		got, err := c.ByTag(ctx, ask)
		if err != nil || got.Version() != "0.6.0" {
			t.Fatalf("ByTag(%q) = %+v, %v", ask, got, err)
		}
	}
	if _, err := c.ByTag(ctx, "0.5.0"); err == nil {
		t.Fatal("ByTag found a release that was never published")
	}
}
