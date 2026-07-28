package e2e

// Fleet updates, end to end.
//
// The production failure this covers: a server reached 0.6.2 while the Windows
// agent on a protected machine stayed on 0.6.1 and went on failing with a bug
// fixed in 0.6.2. Nothing updated the agent, and nothing showed the drift
// either — the console displayed that agent as "1.0.0", because its version was
// recorded once at registration and never refreshed.
//
// The whole loop is driven here through the public API with a fake agent that
// speaks the real protocol: it registers at an old version, the list says it is
// behind, the operator dispatches an update, the agent collects it on its next
// poll, downloads the binary from the server's own /downloads, and only when it
// heartbeats at the new version does the console call it up to date.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"proxback/internal/update"
	"proxback/internal/version"
)

// apiFleetAgent mirrors a row of GET /api/agents including the drift fields the
// console flags outdated components with.
type apiFleetAgent struct {
	ID              string `json:"id"`
	Hostname        string `json:"hostname"`
	OS              string `json:"os"`
	Arch            string `json:"arch"`
	Version         string `json:"version"`
	Status          string `json:"status"`
	ServerVersion   string `json:"serverVersion"`
	UpToDate        bool   `json:"upToDate"`
	UpdateAvailable bool   `json:"updateAvailable"`
}

// apiDispatch mirrors one work item from the heartbeat response.
type apiDispatch struct {
	Type      string `json:"type"`
	Version   string `json:"version"`
	Asset     string `json:"asset"`
	Sha256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
}

type apiHeartbeat struct {
	Jobs          []apiDispatch `json:"jobs"`
	ServerVersion string        `json:"serverVersion"`
}

// fakeAgent is an enrolled agent that speaks the wire protocol without running
// any of the agent package: it registers, heartbeats with whatever version it
// claims to be, and collects dispatches.
type fakeAgent struct {
	h       *harness
	id      string
	key     string
	version string
}

// enrollFakeAgent registers an agent at the version it claims to be running.
func (h *harness) enrollFakeAgent(hostname, ver, goos, goarch string) *fakeAgent {
	h.t.Helper()
	var enroll struct {
		Token string `json:"token"`
	}
	h.ok(http.MethodPost, "/api/agents/enroll-token", nil, &enroll)
	if enroll.Token == "" {
		h.t.Fatal("no enrollment token was issued")
	}
	var out struct {
		AgentID string `json:"agentId"`
		APIKey  string `json:"apiKey"`
	}
	code, raw := h.helperCall(http.MethodPost, "/api/agents/register", "", map[string]any{
		"token": enroll.Token, "hostname": hostname,
		"os": goos, "arch": goarch, "version": ver,
	})
	if code != http.StatusOK {
		h.t.Fatalf("register a fake agent: status %d, body %s", code, raw)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		h.t.Fatalf("decode registration %s: %v", raw, err)
	}
	if out.AgentID == "" || out.APIKey == "" {
		h.t.Fatalf("registration returned %+v", out)
	}
	return &fakeAgent{h: h, id: out.AgentID, key: out.APIKey, version: ver}
}

// beat sends one heartbeat carrying the version the agent currently claims, and
// returns whatever work came back.
func (a *fakeAgent) beat() apiHeartbeat {
	a.h.t.Helper()
	code, raw := a.h.helperCall(http.MethodPost, "/api/agents/heartbeat", a.key, map[string]any{
		"version": a.version, "os": "windows", "arch": "amd64",
	})
	if code != http.StatusOK {
		a.h.t.Fatalf("heartbeat: status %d, body %s", code, raw)
	}
	var out apiHeartbeat
	if err := json.Unmarshal(raw, &out); err != nil {
		a.h.t.Fatalf("decode heartbeat %s: %v", raw, err)
	}
	return out
}

// agentRow re-reads one agent through the listing endpoint.
func (h *harness) agentRow(id string) apiFleetAgent {
	h.t.Helper()
	var agents []apiFleetAgent
	h.ok(http.MethodGet, "/api/agents", nil, &agents)
	for _, a := range agents {
		if a.ID == id {
			return a
		}
	}
	h.t.Fatalf("agent %s is not in %+v", id, agents)
	return apiFleetAgent{}
}

// stageAgentBinary writes a stand-in agent build into <data>/downloads, which is
// both what /downloads serves and what an update dispatch points a guest at.
func (h *harness) stageAgentBinary(name, body string) []byte {
	h.t.Helper()
	dir := update.StagedDir(h.dataDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatalf("create %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil { //nolint:gosec // a fake binary
		h.t.Fatalf("stage %s: %v", name, err)
	}
	return []byte(body)
}

// loginSeeded signs in as the account a fresh install seeds. The main suite
// changes that password on its own harness; this one still has it.
func (h *harness) loginSeeded() {
	h.t.Helper()
	h.ok(http.MethodPost, "/api/login",
		map[string]string{"username": adminUser, "password": "admin"}, nil)
}

// TestAgentUpdateLoop is the whole gap this closes, driven through the API.
func TestAgentUpdateLoop(t *testing.T) {
	h := newHarness(t)
	h.loginSeeded()

	const oldVersion = "0.6.1"
	if version.Version == oldVersion {
		t.Fatalf("this test assumes the server is newer than %s", oldVersion)
	}
	agent := h.enrollFakeAgent("ws-01", oldVersion, "windows", "amd64")

	// ---- 1. the drift is visible ------------------------------------------
	row := h.agentRow(agent.id)
	if row.Version != oldVersion {
		t.Fatalf("registered version = %q, want %q", row.Version, oldVersion)
	}
	if row.ServerVersion != version.Version {
		t.Fatalf("serverVersion = %q, want %q", row.ServerVersion, version.Version)
	}
	if row.UpToDate || !row.UpdateAvailable {
		t.Fatalf("an agent one release behind reads upToDate %v / updateAvailable %v, want false/true",
			row.UpToDate, row.UpdateAvailable)
	}

	// ---- 2. nothing is dispatched that this server cannot serve ------------
	// The harness runs against a repository with no releases, so nothing is
	// staged yet and the update must be refused rather than sent.
	if code, raw := h.do(http.MethodPost, "/api/agents/"+agent.id+"/update", nil); code != http.StatusConflict {
		t.Fatalf("updating with nothing staged = %d, want 409; body %s", code, raw)
	}

	binary := h.stageAgentBinary("proxback-agent-windows-amd64.exe",
		"MZ stand-in proxback-agent "+version.Version)
	want := sha256.Sum256(binary)
	wantSum := hex.EncodeToString(want[:])

	// ---- 3. the operator dispatches the update -----------------------------
	code, raw := h.do(http.MethodPost, "/api/agents/"+agent.id+"/update", nil)
	if code != http.StatusAccepted {
		t.Fatalf("update dispatch = %d, want 202; body %s", code, raw)
	}
	var dispatchResp struct {
		OK          bool   `json:"ok"`
		Asset       string `json:"asset"`
		FromVersion string `json:"fromVersion"`
		ToVersion   string `json:"toVersion"`
		Note        string `json:"note"`
	}
	if err := json.Unmarshal(raw, &dispatchResp); err != nil {
		t.Fatalf("decode dispatch response %s: %v", raw, err)
	}
	if !dispatchResp.OK || dispatchResp.ToVersion != version.Version ||
		dispatchResp.FromVersion != oldVersion || dispatchResp.Note == "" {
		t.Fatalf("dispatch response = %+v", dispatchResp)
	}

	// Dispatching is not applying: until the agent says otherwise it is still
	// running the old build, and the console must keep saying so.
	if row := h.agentRow(agent.id); row.UpToDate || row.Version != oldVersion {
		t.Fatalf("after dispatch the agent reads %+v, want it still on %s", row, oldVersion)
	}

	// ---- 4. the agent collects it on its next poll -------------------------
	beat := agent.beat()
	if beat.ServerVersion != version.Version {
		t.Fatalf("heartbeat serverVersion = %q, want %q", beat.ServerVersion, version.Version)
	}
	if len(beat.Jobs) != 1 || beat.Jobs[0].Type != "update" {
		t.Fatalf("dispatches = %+v, want one update", beat.Jobs)
	}
	d := beat.Jobs[0]
	if d.Version != version.Version || d.Asset != "proxback-agent-windows-amd64.exe" {
		t.Fatalf("update dispatch = %+v", d)
	}
	if d.Sha256 != wantSum || d.SizeBytes != int64(len(binary)) {
		t.Fatalf("dispatch measurements = %s/%d, want %s/%d",
			d.Sha256, d.SizeBytes, wantSum, len(binary))
	}

	// ---- 5. what the dispatch points at is what the server serves ----------
	got := h.fetchRaw(h.base + "/downloads/" + d.Asset)
	if sum := sha256.Sum256(got); hex.EncodeToString(sum[:]) != d.Sha256 {
		t.Fatalf("/downloads/%s does not match the checksum the dispatch carried", d.Asset)
	}

	// A dispatch is handed out once; a second poll must not re-apply it.
	if again := agent.beat(); len(again.Jobs) != 0 {
		t.Fatalf("the update was dispatched twice: %+v", again.Jobs)
	}

	// ---- 6. the confirming heartbeat is what settles it --------------------
	agent.version = version.Version
	agent.beat()
	row = h.agentRow(agent.id)
	if !row.UpToDate || row.UpdateAvailable || row.Version != version.Version {
		t.Fatalf("after the confirming heartbeat the agent reads %+v, want up to date on %s",
			row, version.Version)
	}
	if row.Status != "online" {
		t.Fatalf("agent status = %q after heartbeating, want online", row.Status)
	}

	// ---- 7. and there is nothing left to update ----------------------------
	if code, raw := h.do(http.MethodPost, "/api/agents/"+agent.id+"/update", nil); code != http.StatusConflict {
		t.Fatalf("updating an up-to-date agent = %d, want 409; body %s", code, raw)
	}

	// The trail records who asked for it, and what it did.
	var trail []apiAuditEntry
	h.ok(http.MethodGet, "/api/audit?action=agent.update", nil, &trail)
	if len(trail) == 0 {
		t.Fatal("the update was not recorded in the audit trail")
	}
	if trail[0].ObjectName != "ws-01" {
		t.Fatalf("audit entry = %+v", trail[0])
	}
}

// TestAgentDowngradeIsVisible: the point of reporting the version on every beat
// is that the record follows the component, in both directions.
func TestAgentDowngradeIsVisible(t *testing.T) {
	h := newHarness(t)
	h.loginSeeded()

	agent := h.enrollFakeAgent("ws-02", version.Version, "linux", "amd64")
	agent.beat()
	if row := h.agentRow(agent.id); !row.UpToDate {
		t.Fatalf("an agent registered at the server version reads %+v", row)
	}
	// Someone rolls the guest back by hand. The very next beat must say so.
	agent.version = "0.5.0"
	agent.beat()
	row := h.agentRow(agent.id)
	if row.Version != "0.5.0" || row.UpToDate || !row.UpdateAvailable {
		t.Fatalf("after a rollback the agent reads %+v, want 0.5.0 and behind", row)
	}
}
