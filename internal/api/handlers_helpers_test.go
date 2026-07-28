package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"proxback/internal/agentmgr"
	"proxback/internal/auth"
	"proxback/internal/helpermgr"
	"proxback/internal/nodedeploy"
	"proxback/internal/sched"
	"proxback/internal/store"
)

// testServer wires a real store, auth service and managers to the HTTP handler
// and returns it with a valid session cookie. The scheduler is created but not
// started: no test here runs a job.
type testServer struct {
	*Server
	st      *store.Store
	dataDir string
	cookie  *http.Cookie
	// hostID is a configured Proxmox host, because a node helper is identified
	// by the host it belongs to as much as by its node name.
	hostID string
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "downloads"), 0o755); err != nil {
		t.Fatalf("create downloads dir: %v", err)
	}
	st, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := auth.New(st)
	user, err := authSvc.CreateUser(ctx, "admin", "correct-horse-battery")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	token, err := authSvc.StartSession(ctx, user.ID)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	agents := agentmgr.New(st, log)
	srv, err := New(Config{
		Store:   st,
		Auth:    authSvc,
		Agents:  agents,
		Helpers: helpermgr.New(st, log),
		Sched:   sched.New(st, agents, log),
		DataDir: dataDir,
		Logger:  log,
		// No unit test may reach out to the release repository: the startup
		// reconciliation of the staged binaries is exercised deliberately, in
		// internal/update and in TestDownloadsStatus, against a fake release.
		DisableStagedBinaryRefresh: true,
	})
	if err != nil {
		t.Fatalf("build api server: %v", err)
	}
	host, err := st.CreatePVEHost(ctx, &store.PVEHost{
		Name: "cluster-a", BaseURL: "https://pve-a.example:8006",
		TokenID: "root@pam!proxback", TokenSecret: "secret", Status: "online",
	})
	if err != nil {
		t.Fatalf("create host: %v", err)
	}
	return &testServer{
		Server: srv, st: st, dataDir: dataDir,
		cookie: &http.Cookie{Name: auth.SessionCookieName, Value: token},
		hostID: host.ID,
	}
}

// stageHelperBinary writes a stand-in helper binary where the deployment looks
// for it.
func (ts *testServer) stageHelperBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(ts.dataDir, "downloads", helperBinaryName)
	if err := os.WriteFile(path, []byte("ELF-ish payload"), 0o755); err != nil { //nolint:gosec // a fake binary
		t.Fatalf("stage helper binary: %v", err)
	}
	return path
}

// post sends an authenticated JSON request and decodes the response body.
func (ts *testServer) post(t *testing.T, path string, body any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(ts.cookie)
	rec := httptest.NewRecorder()
	ts.ServeHTTP(rec, req)

	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode response %q: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, out
}

func (ts *testServer) deployBody() map[string]any {
	body := deployBody()
	body["hostId"] = ts.hostID
	return body
}

func deployBody() map[string]any {
	return map[string]any{
		"node":               "pve1",
		"address":            "192.0.2.10",
		"username":           "root",
		"password":           "root-password",
		"serverUrl":          "https://proxback.local:8443",
		"hostKeyFingerprint": "SHA256:abc123",
	}
}

func TestDeployHelperEndpointHappyPath(t *testing.T) {
	ts := newTestServer(t)
	binary := ts.stageHelperBinary(t)

	var got nodedeploy.Params
	ts.deployHelper = func(_ context.Context, p nodedeploy.Params) (nodedeploy.Result, error) {
		got = p
		return nodedeploy.Result{Log: []string{
			"connected to 192.0.2.10:22 (SHA256:abc123)",
			"uploaded proxback-helper (15.1 MiB)",
			"installer: enrolled as helper h-1 for node pve1 (port 8007)",
		}}, nil
	}

	code, body := ts.post(t, "/api/helpers/deploy", ts.deployBody())
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %+v", code, body)
	}
	if body["ok"] != true {
		t.Errorf(`response["ok"] = %v, want true`, body["ok"])
	}
	lines, ok := body["log"].([]any)
	if !ok || len(lines) != 3 {
		t.Fatalf(`response["log"] = %#v`, body["log"])
	}
	if lines[1] != "uploaded proxback-helper (15.1 MiB)" {
		t.Errorf("log[1] = %v", lines[1])
	}
	// No helper has registered, so it cannot be online.
	if body["helperOnline"] != false {
		t.Errorf(`response["helperOnline"] = %v, want false`, body["helperOnline"])
	}

	// Defaults are applied and the staged binary is what gets deployed.
	if got.Port != 22 || got.HelperPort != store.DefaultHelperPort {
		t.Errorf("ports = %d/%d, want 22/%d", got.Port, got.HelperPort, store.DefaultHelperPort)
	}
	if got.Address != "192.0.2.10" || got.Username != "root" || got.Password != "root-password" {
		t.Errorf("connection params = %+v", nodedeploy.Params{
			Address: got.Address, Username: got.Username, Password: "…",
		})
	}
	if got.BinaryPath != binary {
		t.Errorf("binary path = %q, want %q", got.BinaryPath, binary)
	}
	if got.ServerURL != "https://proxback.local:8443" || got.ExpectedFingerprint != "SHA256:abc123" {
		t.Errorf("serverUrl/fingerprint = %q/%q", got.ServerURL, got.ExpectedFingerprint)
	}
	// The token is minted server side and must be a live helper enrollment token.
	if got.EnrollToken == "" {
		t.Fatal("no enrollment token was passed to the deployment")
	}
	tok, err := ts.st.EnrollTokenByValue(context.Background(), got.EnrollToken)
	if err != nil {
		t.Fatalf("minted token is not stored: %v", err)
	}
	if tok.Purpose != store.EnrollPurposeHelper {
		t.Errorf("token purpose = %q, want %q", tok.Purpose, store.EnrollPurposeHelper)
	}
}

// A helper that has already registered for the node is reported as online.
func TestDeployHelperEndpointReportsHelperOnline(t *testing.T) {
	ts := newTestServer(t)
	ts.stageHelperBinary(t)
	ts.deployHelper = func(context.Context, nodedeploy.Params) (nodedeploy.Result, error) {
		return nodedeploy.Result{Log: []string{"connected to 192.0.2.10:22 (SHA256:abc123)"}}, nil
	}
	now := store.Now()
	if _, err := ts.st.CreateHelper(context.Background(), &store.NodeHelper{
		HostID: ts.hostID, Node: "pve1", Address: "192.0.2.10", Port: store.DefaultHelperPort,
		Version: "0.3.1", AccessSecret: "secret", APIKeyHash: "hash", LastSeen: &now,
	}); err != nil {
		t.Fatalf("create helper: %v", err)
	}

	code, body := ts.post(t, "/api/helpers/deploy", ts.deployBody())
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %+v", code, body)
	}
	if body["helperOnline"] != true {
		t.Errorf(`response["helperOnline"] = %v, want true`, body["helperOnline"])
	}
}

func TestDeployHelperEndpointFingerprintConflict(t *testing.T) {
	ts := newTestServer(t)
	ts.stageHelperBinary(t)
	ts.deployHelper = func(context.Context, nodedeploy.Params) (nodedeploy.Result, error) {
		return nodedeploy.Result{}, &nodedeploy.FingerprintError{Fingerprint: "SHA256:realkey"}
	}

	body := ts.deployBody()
	delete(body, "hostKeyFingerprint")
	code, got := ts.post(t, "/api/helpers/deploy", body)
	if code != http.StatusConflict {
		t.Fatalf("status = %d, body = %+v", code, got)
	}
	if got["fingerprint"] != "SHA256:realkey" {
		t.Errorf(`response["fingerprint"] = %v`, got["fingerprint"])
	}
	if s, _ := got["error"].(string); !strings.Contains(s, "fingerprint") {
		t.Errorf(`response["error"] = %v`, got["error"])
	}
}

func TestDeployHelperEndpointDeploymentFailure(t *testing.T) {
	ts := newTestServer(t)
	ts.stageHelperBinary(t)
	ts.deployHelper = func(context.Context, nodedeploy.Params) (nodedeploy.Result, error) {
		return nodedeploy.Result{Log: []string{"connected to 192.0.2.10:22 (SHA256:abc123)"}},
			&testError{"nodedeploy: ssh to root@192.0.2.10:22: ssh: unable to authenticate"}
	}

	code, got := ts.post(t, "/api/helpers/deploy", ts.deployBody())
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %+v", code, got)
	}
	msg, _ := got["error"].(string)
	if !strings.Contains(msg, "unable to authenticate") {
		t.Errorf(`response["error"] = %q`, msg)
	}
	if strings.Contains(msg, "root-password") {
		t.Error("the error leaks the password")
	}
}

func TestDeployHelperEndpointRequiresTheStagedBinary(t *testing.T) {
	ts := newTestServer(t)
	called := false
	ts.deployHelper = func(context.Context, nodedeploy.Params) (nodedeploy.Result, error) {
		called = true
		return nodedeploy.Result{}, nil
	}

	code, got := ts.post(t, "/api/helpers/deploy", ts.deployBody())
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %+v", code, got)
	}
	if msg, _ := got["error"].(string); !strings.Contains(msg, helperBinaryName) {
		t.Errorf(`response["error"] = %q, want it to name the missing binary`, msg)
	}
	if called {
		t.Error("deployment was attempted without a staged binary")
	}
}

func TestDeployHelperEndpointValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"no hostId", func(b map[string]any) { delete(b, "hostId") }, "hostId is required"},
		{"no node", func(b map[string]any) { delete(b, "node") }, "node is required"},
		{"blank address", func(b map[string]any) { b["address"] = "  " }, "address is required"},
		{"no username", func(b map[string]any) { delete(b, "username") }, "username is required"},
		{"no password", func(b map[string]any) { delete(b, "password") }, "password is required"},
		{"no serverUrl", func(b map[string]any) { delete(b, "serverUrl") }, "serverUrl is required"},
		{"ssh serverUrl", func(b map[string]any) { b["serverUrl"] = "ssh://proxback.local" }, "http://"},
		{"hostless serverUrl", func(b map[string]any) { b["serverUrl"] = "https://" }, "http://"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t)
			ts.stageHelperBinary(t)
			called := false
			ts.deployHelper = func(context.Context, nodedeploy.Params) (nodedeploy.Result, error) {
				called = true
				return nodedeploy.Result{}, nil
			}
			body := ts.deployBody()
			tc.mutate(body)

			code, got := ts.post(t, "/api/helpers/deploy", body)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %+v", code, got)
			}
			if msg, _ := got["error"].(string); !strings.Contains(msg, tc.want) {
				t.Errorf(`response["error"] = %q, want it to mention %q`, msg, tc.want)
			}
			if called {
				t.Error("an invalid request reached the deployment")
			}
		})
	}
}

func TestDeployHelperEndpointRequiresASession(t *testing.T) {
	ts := newTestServer(t)
	ts.stageHelperBinary(t)
	ts.deployHelper = func(context.Context, nodedeploy.Params) (nodedeploy.Result, error) {
		t.Error("deployment ran for an unauthenticated request")
		return nodedeploy.Result{}, nil
	}
	raw, err := json.Marshal(ts.deployBody())
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/helpers/deploy", strings.NewReader(string(raw)))
	rec := httptest.NewRecorder()
	ts.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
	}
}

// testError is a plain error whose message the handler must pass through.
type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }
