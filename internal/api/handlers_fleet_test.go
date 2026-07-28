package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"proxback/internal/agentmgr"
	"proxback/internal/helpermgr"
	"proxback/internal/store"
	"proxback/internal/version"
)

// ---------------------------------------------------------------- drift

// TestVersionDrift pins the two questions every agent and helper row answers.
// The pair is deliberately not one boolean: a component ahead of its server is
// neither current nor a candidate for an "update" that would downgrade it.
func TestVersionDrift(t *testing.T) {
	older := "0.6.1"
	if version.Version == older {
		t.Fatalf("this test assumes the server is newer than %s", older)
	}
	cases := []struct {
		name            string
		component       string
		upToDate        bool
		updateAvailable bool
	}{
		{"the same build as the server", version.Version, true, false},
		{"one release behind", older, false, true},
		{"far behind", "0.2.0", false, true},
		{"ahead of the server", "99.0.0", false, false},
		{"the bogus 1.0.0 the console used to show", "1.0.0", false, false},
		{"never reported", "", false, true},
		{"unparsable", "not-a-version", false, true},
		{"tagged with a v", "v" + version.Version, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upToDate, updateAvailable := versionDrift(tc.component)
			if upToDate != tc.upToDate || updateAvailable != tc.updateAvailable {
				t.Fatalf("versionDrift(%q) = %v/%v, want %v/%v",
					tc.component, upToDate, updateAvailable, tc.upToDate, tc.updateAvailable)
			}
		})
	}
}

// ---------------------------------------------------------------- fixtures

// stageAgentBinary writes a stand-in agent binary where /downloads serves from
// and returns its bytes, so a test can check what the dispatch measured.
func (ts *testServer) stageAgentBinary(t *testing.T, name string) []byte {
	t.Helper()
	body := []byte("MZ stand-in agent build for " + name)
	path := filepath.Join(ts.dataDir, "downloads", name)
	if err := os.WriteFile(path, body, 0o755); err != nil { //nolint:gosec // a fake binary
		t.Fatalf("stage %s: %v", name, err)
	}
	return body
}

func (ts *testServer) createAgent(t *testing.T, hostname, ver, goos, goarch string) *store.Agent {
	t.Helper()
	a, err := ts.st.CreateAgent(context.Background(), &store.Agent{
		Hostname: hostname, OS: goos, Arch: goarch, Version: ver,
		APIKeyHash: "hash-" + hostname,
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	return a
}

func (ts *testServer) createHelper(t *testing.T, node, ver string) *store.NodeHelper {
	t.Helper()
	h, err := ts.st.CreateHelper(context.Background(), &store.NodeHelper{
		HostID: ts.hostID, Node: node, Address: "192.0.2.20", Port: 8007,
		Version: ver, AccessSecret: "secret-" + node, APIKeyHash: "hash-" + node,
	})
	if err != nil {
		t.Fatalf("create helper: %v", err)
	}
	return h
}

// getJSON performs an authenticated GET and decodes into out.
func (ts *testServer) getJSON(t *testing.T, path string, out any) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(ts.cookie)
	rec := httptest.NewRecorder()
	ts.ServeHTTP(rec, req)
	if out != nil && rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("decode %s: %v (%s)", path, err, rec.Body.String())
		}
	}
	return rec.Code
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------- list rows

func TestAgentListCarriesTheDrift(t *testing.T) {
	ts := newTestServer(t)
	ts.createAgent(t, "behind", "0.6.1", "windows", "amd64")
	ts.createAgent(t, "current", version.Version, "linux", "amd64")

	var agents []agentDTO
	if code := ts.getJSON(t, "/api/agents", &agents); code != http.StatusOK {
		t.Fatalf("GET /api/agents = %d", code)
	}
	if len(agents) != 2 {
		t.Fatalf("agents = %d, want 2", len(agents))
	}
	by := map[string]agentDTO{}
	for _, a := range agents {
		by[a.Hostname] = a
	}
	behind, current := by["behind"], by["current"]
	if behind.ServerVersion != version.Version || current.ServerVersion != version.Version {
		t.Fatalf("serverVersion = %q/%q, want %q",
			behind.ServerVersion, current.ServerVersion, version.Version)
	}
	if behind.UpToDate || !behind.UpdateAvailable {
		t.Errorf("the 0.6.1 agent = upToDate %v / updateAvailable %v, want false/true",
			behind.UpToDate, behind.UpdateAvailable)
	}
	if !current.UpToDate || current.UpdateAvailable {
		t.Errorf("the current agent = upToDate %v / updateAvailable %v, want true/false",
			current.UpToDate, current.UpdateAvailable)
	}
}

func TestHelperListCarriesTheDrift(t *testing.T) {
	ts := newTestServer(t)
	ts.createHelper(t, "pve1", "0.5.0")
	ts.createHelper(t, "pve2", version.Version)

	var helpers []helperDTO
	if code := ts.getJSON(t, "/api/helpers", &helpers); code != http.StatusOK {
		t.Fatalf("GET /api/helpers = %d", code)
	}
	by := map[string]helperDTO{}
	for _, h := range helpers {
		by[h.Node] = h
	}
	if by["pve1"].UpToDate || !by["pve1"].UpdateAvailable {
		t.Errorf("pve1 = %+v, want behind", by["pve1"])
	}
	if !by["pve2"].UpToDate || by["pve2"].UpdateAvailable {
		t.Errorf("pve2 = %+v, want current", by["pve2"])
	}
	if by["pve2"].ServerVersion != version.Version {
		t.Errorf("serverVersion = %q", by["pve2"].ServerVersion)
	}
}

// ---------------------------------------------------------------- agent update

func TestUpdateAgentDispatchesToAnIdleAgent(t *testing.T) {
	ts := newTestServer(t)
	binary := ts.stageAgentBinary(t, "proxback-agent-windows-amd64.exe")
	a := ts.createAgent(t, "ws-01", "0.6.1", "windows", "amd64")

	code, body := ts.post(t, "/api/agents/"+a.ID+"/update", nil)
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %+v", code, body)
	}
	if body["asset"] != "proxback-agent-windows-amd64.exe" || body["toVersion"] != version.Version {
		t.Fatalf("response = %+v", body)
	}
	// The response must not claim the agent has been updated: nothing has
	// happened in the guest yet.
	if note, _ := body["note"].(string); note == "" {
		t.Error("the response does not say when the update is actually confirmed")
	}

	// The agent collects the dispatch on its next poll, and it carries what the
	// server measured on the file it will serve.
	jobs, err := ts.Server.agents.Heartbeat(context.Background(), a.ID, agentmgr.HeartbeatRequest{Version: "0.6.1"})
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Type != agentmgr.DispatchUpdate {
		t.Fatalf("dispatches = %+v, want one update", jobs)
	}
	d := jobs[0]
	if d.Asset != "proxback-agent-windows-amd64.exe" || d.Version != version.Version {
		t.Errorf("dispatch = %+v", d)
	}
	if d.Sha256 != sha256Hex(binary) || d.SizeBytes != int64(len(binary)) {
		t.Errorf("dispatch measurements = %s/%d, want %s/%d",
			d.Sha256, d.SizeBytes, sha256Hex(binary), len(binary))
	}

	// Until the agent heartbeats at the new version, the console must go on
	// showing it as behind. An update is applied when the component says so.
	var agents []agentDTO
	ts.getJSON(t, "/api/agents", &agents)
	if len(agents) != 1 || agents[0].Version != "0.6.1" || agents[0].UpToDate {
		t.Fatalf("after dispatch the agent reads %+v, want it still on 0.6.1", agents)
	}

	// The confirming heartbeat is what flips it.
	if _, err := ts.Server.agents.Heartbeat(context.Background(), a.ID,
		agentmgr.HeartbeatRequest{Version: version.Version}); err != nil {
		t.Fatalf("confirming heartbeat: %v", err)
	}
	ts.getJSON(t, "/api/agents", &agents)
	if !agents[0].UpToDate || agents[0].UpdateAvailable || agents[0].Version != version.Version {
		t.Fatalf("after the confirming heartbeat the agent reads %+v", agents[0])
	}
}

func TestUpdateAgentRefusesARunInFlight(t *testing.T) {
	ts := newTestServer(t)
	ts.stageAgentBinary(t, "proxback-agent-linux-amd64")
	a := ts.createAgent(t, "busy-01", "0.6.1", "linux", "amd64")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = ts.Server.agents.RunBackup(ctx, agentmgr.BackupRequest{
			RunID: "run-1", AgentID: a.ID, Paths: []string{"/data"},
		})
	}()
	waitUntil(t, func() bool { return ts.Server.agents.Busy(a.ID) })

	code, body := ts.post(t, "/api/agents/"+a.ID+"/update", nil)
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %+v", code, body)
	}
	msg, _ := body["error"].(string)
	if msg == "" || !strings.Contains(msg, "run in flight") {
		t.Fatalf("error = %q, want it to name the run in flight", msg)
	}
	cancel()
	<-done
}

func TestUpdateAgentRefusesAnUnstagedPlatform(t *testing.T) {
	ts := newTestServer(t)
	// Nothing staged at all: the server would be telling a guest to download
	// something it cannot serve.
	a := ts.createAgent(t, "ws-02", "0.6.1", "windows", "amd64")
	code, body := ts.post(t, "/api/agents/"+a.ID+"/update", nil)
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %+v", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "not staged") {
		t.Fatalf("error = %q, want it to say the binary is not staged", msg)
	}

	// A platform this server stages nothing for is refused before it looks at
	// the disk at all.
	mac := ts.createAgent(t, "mac-01", "0.6.1", "darwin", "arm64")
	code, body = ts.post(t, "/api/agents/"+mac.ID+"/update", nil)
	if code != http.StatusConflict {
		t.Fatalf("darwin status = %d, want 409; body = %+v", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "no binary for that platform") {
		t.Fatalf("darwin error = %q", msg)
	}
}

func TestUpdateAgentRefusesOneAlreadyOnTheServerBuild(t *testing.T) {
	ts := newTestServer(t)
	ts.stageAgentBinary(t, "proxback-agent-linux-amd64")
	a := ts.createAgent(t, "current-01", version.Version, "linux", "amd64")

	code, body := ts.post(t, "/api/agents/"+a.ID+"/update", nil)
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %+v", code, body)
	}
	// ...unless the operator insists, which is how a corrupted install is
	// repaired without deleting and re-enrolling the agent.
	code, body = ts.post(t, "/api/agents/"+a.ID+"/update?force=1", nil)
	if code != http.StatusAccepted {
		t.Fatalf("forced status = %d, want 202; body = %+v", code, body)
	}
}

func TestUpdateAgentUnknownID(t *testing.T) {
	ts := newTestServer(t)
	code, _ := ts.post(t, "/api/agents/nope/update", nil)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

func TestUpdateAllAgentsSkipsWithReasons(t *testing.T) {
	ts := newTestServer(t)
	ts.stageAgentBinary(t, "proxback-agent-linux-amd64")
	behind := ts.createAgent(t, "behind-01", "0.6.1", "linux", "amd64")
	ts.createAgent(t, "current-01", version.Version, "linux", "amd64")
	ts.createAgent(t, "mac-01", "0.6.1", "darwin", "arm64")

	code, body := ts.post(t, "/api/agents/update-all", nil)
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %+v", code, body)
	}
	dispatched, _ := body["dispatched"].([]any)
	skipped, _ := body["skipped"].([]any)
	if len(dispatched) != 1 || dispatched[0] != behind.ID {
		t.Fatalf("dispatched = %+v, want just the agent that is behind", dispatched)
	}
	if len(skipped) != 2 {
		t.Fatalf("skipped = %+v, want the current one and the unstaged platform", skipped)
	}
	for _, s := range skipped {
		row, _ := s.(map[string]any)
		if reason, _ := row["reason"].(string); reason == "" {
			t.Errorf("a skipped agent carries no reason: %+v", row)
		}
	}
}

// ---------------------------------------------------------------- helper update

func TestUpdateHelperTellsItOverItsOwnAPI(t *testing.T) {
	ts := newTestServer(t)
	binary := ts.stageHelperBinary(t)
	body, err := os.ReadFile(binary)
	if err != nil {
		t.Fatalf("read staged helper: %v", err)
	}
	h := ts.createHelper(t, "pve1", "0.6.1")

	var got helpermgr.UpdateRequest
	var gotAddr string
	ts.requestHelperUpdate = func(_ context.Context, target *store.NodeHelper, req helpermgr.UpdateRequest) (helpermgr.UpdateResponse, error) {
		got, gotAddr = req, target.Address
		return helpermgr.UpdateResponse{OK: true, Version: req.Version, Restarting: true}, nil
	}

	code, resp := ts.post(t, "/api/helpers/"+h.ID+"/update", nil)
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %+v", code, resp)
	}
	if gotAddr != "192.0.2.20" {
		t.Errorf("the request went to %q", gotAddr)
	}
	if got.Asset != helperBinaryName || got.Version != version.Version {
		t.Errorf("update request = %+v", got)
	}
	if got.Sha256 != sha256Hex(body) || got.SizeBytes != int64(len(body)) {
		t.Errorf("measurements = %s/%d, want %s/%d",
			got.Sha256, got.SizeBytes, sha256Hex(body), len(body))
	}
	// Installed is not running: the row still reads the old version until the
	// helper heartbeats at the new one.
	var helpers []helperDTO
	ts.getJSON(t, "/api/helpers", &helpers)
	if helpers[0].Version != "0.6.1" || helpers[0].UpToDate {
		t.Fatalf("after the update the helper reads %+v, want it still on 0.6.1", helpers[0])
	}
	if err := ts.Server.helpers.Heartbeat(context.Background(), h.ID,
		helpermgr.HeartbeatRequest{Version: version.Version}); err != nil {
		t.Fatalf("confirming heartbeat: %v", err)
	}
	ts.getJSON(t, "/api/helpers", &helpers)
	if !helpers[0].UpToDate {
		t.Fatalf("after the confirming heartbeat the helper reads %+v", helpers[0])
	}
}

func TestUpdateHelperRefusesWhenTheNodeIsBusy(t *testing.T) {
	ts := newTestServer(t)
	ts.stageHelperBinary(t)
	h := ts.createHelper(t, "pve1", "0.6.1")
	ts.requestHelperUpdate = func(context.Context, *store.NodeHelper, helpermgr.UpdateRequest) (helpermgr.UpdateResponse, error) {
		return helpermgr.UpdateResponse{}, helpermgr.ErrHelperBusy
	}
	code, body := ts.post(t, "/api/helpers/"+h.ID+"/update", nil)
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %+v", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "in flight") {
		t.Fatalf("error = %q", msg)
	}
}

func TestUpdateHelperReportsAnUnreachableNode(t *testing.T) {
	ts := newTestServer(t)
	ts.stageHelperBinary(t)
	h := ts.createHelper(t, "pve1", "0.6.1")
	ts.requestHelperUpdate = func(context.Context, *store.NodeHelper, helpermgr.UpdateRequest) (helpermgr.UpdateResponse, error) {
		return helpermgr.UpdateResponse{}, helpermgr.ErrHelperUnreachable
	}
	code, body := ts.post(t, "/api/helpers/"+h.ID+"/update", nil)
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %+v", code, body)
	}
}

func TestUpdateHelperRefusesWithoutAStagedBinary(t *testing.T) {
	ts := newTestServer(t)
	h := ts.createHelper(t, "pve1", "0.6.1")
	called := false
	ts.requestHelperUpdate = func(context.Context, *store.NodeHelper, helpermgr.UpdateRequest) (helpermgr.UpdateResponse, error) {
		called = true
		return helpermgr.UpdateResponse{}, nil
	}
	code, body := ts.post(t, "/api/helpers/"+h.ID+"/update", nil)
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %+v", code, body)
	}
	if called {
		t.Error("the node was contacted even though there is nothing to hand it")
	}
}

// The role these endpoints require is asserted where every other route's is:
// the capability matrix in handlers_roles_test.go.

// ---------------------------------------------------------------- small helpers

// waitUntil polls pred for up to two seconds. The condition it waits on is
// produced by another goroutine, so the effect is the only thing to wait on.
func waitUntil(t *testing.T, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the condition never became true")
}
