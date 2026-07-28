package api

import (
	"net/http"
	"os"
	"testing"

	"proxback/internal/store"
	"proxback/internal/update"
	"proxback/internal/version"
)

// stagedStatusBody mirrors GET /api/downloads/status exactly, so the shape the
// agent and node helper deployment pages are built against stays honest.
type stagedStatusBody struct {
	ServerVersion string `json:"serverVersion"`
	AllMatch      bool   `json:"allMatch"`
	Artifacts     []struct {
		Name          string  `json:"name"`
		Kind          string  `json:"kind"`
		OS            string  `json:"os"`
		Present       bool    `json:"present"`
		Version       string  `json:"version"`
		MatchesServer bool    `json:"matchesServer"`
		SizeBytes     int64   `json:"sizeBytes"`
		ModifiedAt    *string `json:"modifiedAt"`
		Reason        string  `json:"reason"`
	} `json:"artifacts"`
}

func (ts *testServer) downloadsStatus(t *testing.T) stagedStatusBody {
	t.Helper()
	code, raw := ts.getRaw(t, "/api/downloads/status")
	if code != http.StatusOK {
		t.Fatalf("GET /api/downloads/status = %d (%s)", code, raw)
	}
	var out stagedStatusBody
	decodeJSONBody(t, raw, &out)
	if out.ServerVersion != version.Version {
		t.Fatalf("serverVersion = %q, want %q", out.ServerVersion, version.Version)
	}
	if len(out.Artifacts) != len(update.StagedArtifacts()) {
		t.Fatalf("reported %d artifacts, want %d", len(out.Artifacts), len(update.StagedArtifacts()))
	}
	return out
}

// stagedBinary writes a stand-in staged binary, with a recorded version when ver
// is not empty.
func (ts *testServer) stagedBinary(t *testing.T, name, body, ver string) {
	t.Helper()
	if err := os.WriteFile(update.StagedPath(ts.dataDir, name), []byte(body), 0o755); err != nil { //nolint:gosec // fake binary
		t.Fatalf("stage %s: %v", name, err)
	}
	if ver == "" {
		return
	}
	if err := os.WriteFile(update.StagedVersionPath(ts.dataDir, name), []byte(ver+"\n"), 0o644); err != nil {
		t.Fatalf("record version of %s: %v", name, err)
	}
}

// TestDownloadsStatus covers the three states an operator can be in when they
// open a deployment page: nothing staged, the wrong build staged, and the right
// build staged. The middle one is the bug this endpoint exists for — a server
// that upgraded itself while continuing to hand out its installer's agent.
func TestDownloadsStatus(t *testing.T) {
	ts := newTestServer(t)

	const (
		agentLinux   = "proxback-agent-linux-amd64"
		agentWindows = "proxback-agent-windows-amd64.exe"
		helperLinux  = "proxback-helper-linux-amd64"
	)

	// ---- nothing staged: every artifact absent, with a reason ---------------
	fresh := ts.downloadsStatus(t)
	if fresh.AllMatch {
		t.Error("allMatch is true with nothing staged at all")
	}
	for _, a := range fresh.Artifacts {
		if a.Present || a.MatchesServer || a.Reason == "" || a.ModifiedAt != nil || a.SizeBytes != 0 {
			t.Errorf("unstaged artifact %s = %+v", a.Name, a)
		}
		if a.Kind == "" || a.OS == "" {
			t.Errorf("artifact %s carries no kind/os: %+v", a.Name, a)
		}
	}

	// ---- the failing installation: a stale agent, an unknown-version helper --
	ts.stagedBinary(t, agentWindows, "MZ agent from 0.2.x", "0.2.1")
	ts.stagedBinary(t, agentLinux, "ELF agent copied in by hand", "")
	ts.stagedBinary(t, helperLinux, "ELF helper "+version.Version, version.Version)

	drifted := ts.downloadsStatus(t)
	if drifted.AllMatch {
		t.Error("allMatch is true although a stale agent is staged")
	}
	byName := map[string]int{}
	for i, a := range drifted.Artifacts {
		byName[a.Name] = i
	}

	stale := drifted.Artifacts[byName[agentWindows]]
	switch {
	case !stale.Present:
		t.Errorf("staged Windows agent reported absent: %+v", stale)
	case stale.Version != "0.2.1":
		t.Errorf("staged Windows agent version = %q, want the recorded 0.2.1", stale.Version)
	case stale.MatchesServer:
		t.Errorf("a 0.2.1 agent reported as matching a %s server", version.Version)
	case stale.Reason == "":
		t.Error("a mismatched agent carries no reason")
	case stale.SizeBytes != int64(len("MZ agent from 0.2.x")) || stale.ModifiedAt == nil:
		t.Errorf("staged Windows agent = %+v, want a size and a modification time", stale)
	}

	unknown := drifted.Artifacts[byName[agentLinux]]
	if !unknown.Present || unknown.Version != "" || unknown.MatchesServer || unknown.Reason == "" {
		t.Errorf("hand-copied agent = %+v, want present, version unknown, with a reason", unknown)
	}

	matching := drifted.Artifacts[byName[helperLinux]]
	if !matching.MatchesServer || matching.Version != version.Version || matching.Reason != "" {
		t.Errorf("current helper = %+v, want a clean match", matching)
	}

	// ---- everything current: allMatch, no reasons ---------------------------
	ts.stagedBinary(t, agentWindows, "MZ agent "+version.Version, version.Version)
	ts.stagedBinary(t, agentLinux, "ELF agent "+version.Version, version.Version)
	healed := ts.downloadsStatus(t)
	if !healed.AllMatch {
		t.Fatalf("allMatch is false with every binary current: %+v", healed.Artifacts)
	}
	for _, a := range healed.Artifacts {
		if !a.Present || !a.MatchesServer || a.Reason != "" {
			t.Errorf("current artifact %s = %+v", a.Name, a)
		}
	}
}

// The endpoint lives in the admin group — the same group as applying an update
// and deploying a helper. TestRoleEnforcementAcrossTheAPI asserts the refusal
// for every other role; this asserts that a viewer's own reads do not include it.
func TestDownloadsStatusIsAdminOnly(t *testing.T) {
	ts := newTestServer(t)
	viewer := ts.account(t, "looky", store.RoleViewer)
	operator := ts.account(t, "opsy", store.RoleOperator)

	for role, cookie := range map[store.Role]*http.Cookie{
		store.RoleViewer: viewer, store.RoleOperator: operator,
	} {
		code, raw := ts.as(t, cookie, http.MethodGet, "/api/downloads/status", nil)
		if code != http.StatusForbidden {
			t.Errorf("GET /api/downloads/status as %s = %d (%s), want 403", role, code, raw)
		}
	}
	if code, raw := ts.as(t, ts.cookie, http.MethodGet, "/api/downloads/status", nil); code != http.StatusOK {
		t.Fatalf("GET /api/downloads/status as admin = %d (%s)", code, raw)
	}
}

// The version sidecars sit next to the binaries in <data>/downloads, and must not
// be reachable through the public download endpoint: it serves an allow-list of
// exactly the three binaries and nothing else in the directory.
func TestDownloadEndpointServesOnlyTheStagedBinaries(t *testing.T) {
	ts := newTestServer(t)
	ts.stagedBinary(t, "proxback-agent-linux-amd64", "ELF agent", version.Version)

	code, body := ts.getRaw(t, "/downloads/proxback-agent-linux-amd64")
	if code != http.StatusOK || string(body) != "ELF agent" {
		t.Fatalf("staged agent download = %d (%s)", code, body)
	}
	for _, name := range []string{
		"proxback-agent-linux-amd64.version",
		"proxback.db",
		"..%2Fproxback.db",
	} {
		if code, _ := ts.getRaw(t, "/downloads/"+name); code != http.StatusNotFound {
			t.Errorf("GET /downloads/%s = %d, want 404", name, code)
		}
	}
}
