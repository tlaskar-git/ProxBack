package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------- API shapes

// apiAccount mirrors an entry of GET /api/users. It is decoded strictly so the
// contract the console is built against stays honest — including the promise that
// no password hash is on the wire.
type apiAccount struct {
	ID          int64   `json:"id"`
	Username    string  `json:"username"`
	Role        string  `json:"role"`
	CreatedAt   string  `json:"createdAt"`
	LastLoginAt *string `json:"lastLoginAt"`
}

// apiAuditEntry mirrors an entry of GET /api/audit.
type apiAuditEntry struct {
	ID         int64  `json:"id"`
	At         string `json:"at"`
	Actor      string `json:"actor"`
	ActorID    int64  `json:"actorId"`
	Action     string `json:"action"`
	ObjectKind string `json:"objectKind"`
	ObjectID   string `json:"objectId"`
	ObjectName string `json:"objectName"`
	Result     string `json:"result"`
	SourceIP   string `json:"sourceIp"`
	Detail     string `json:"detail"`
}

// ---------------------------------------------------------------- sessions

// browser is one signed-in client with a cookie jar of its own, so several roles
// can be driven through the real API side by side.
type browser struct {
	t      *testing.T
	client *http.Client
	base   string
	who    string
}

// browserFor signs a user in and returns their session.
func (h *harness) browserFor(username, password string) *browser {
	h.t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		h.t.Fatalf("cookie jar for %s: %v", username, err)
	}
	b := &browser{
		t:      h.t,
		client: &http.Client{Jar: jar, Timeout: 2 * time.Minute},
		base:   h.base,
		who:    username,
	}
	var out struct {
		User apiUser `json:"user"`
	}
	b.ok(http.MethodPost, "/api/login",
		map[string]string{"username": username, "password": password}, &out)
	if out.User.Username != username {
		h.t.Fatalf("%s signed in as %+v", username, out.User)
	}
	return b
}

func (b *browser) do(method, path string, body any) (int, []byte) {
	b.t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			b.t.Fatalf("encode %s %s: %v", method, path, err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, b.base+path, rdr) //nolint:noctx // test helper
	if err != nil {
		b.t.Fatalf("build %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.client.Do(req)
	if err != nil {
		b.t.Fatalf("%s %s as %s: %v", method, path, b.who, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		b.t.Fatalf("%s %s as %s: read body: %v", method, path, b.who, err)
	}
	return resp.StatusCode, raw
}

func (b *browser) ok(method, path string, body, out any) {
	b.t.Helper()
	code, raw := b.do(method, path, body)
	if code != http.StatusOK {
		b.t.Fatalf("%s %s as %s: status %d, body %s", method, path, b.who, code, raw)
	}
	if out == nil {
		return
	}
	if err := json.Unmarshal(raw, out); err != nil {
		b.t.Fatalf("%s %s as %s: decode %s: %v", method, path, b.who, raw, err)
	}
}

// waitRun polls a run with this session's own cookies until it leaves the running
// state — which also proves a viewer or operator may watch what they started.
func (b *browser) waitRun(runID string, timeout time.Duration) apiRun {
	b.t.Helper()
	deadline := time.Now().Add(timeout)
	var run apiRun
	for time.Now().Before(deadline) {
		b.ok(http.MethodGet, "/api/runs/"+runID, nil, &run)
		if run.Status != "running" {
			return run
		}
		time.Sleep(50 * time.Millisecond)
	}
	b.t.Fatalf("run %s still running after %s (step %q)", runID, timeout, run.CurrentStep)
	return run
}

// refused requires a 403 with a JSON error — never a silent success and never a
// 404 pretending the endpoint is not there.
func (b *browser) refused(method, path string, body any) string {
	b.t.Helper()
	code, raw := b.do(method, path, body)
	if code != http.StatusForbidden {
		b.t.Fatalf("%s %s as %s = %d (%s), want 403", method, path, b.who, code, raw)
	}
	var out struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Error == "" {
		b.t.Fatalf("%s %s as %s: refusal body %q, want a JSON error", method, path, b.who, raw)
	}
	return out.Error
}

// ---------------------------------------------------------------- the scenario

// TestRolesAndAuditTrail drives the whole v0.6.0 boundary through the real API:
// an admin delegates, an operator runs work but cannot touch storage, a viewer
// can only look, and the admin can see every one of those attempts in the trail
// afterwards. Three cookie jars, one server, no shortcuts through the store.
func TestRolesAndAuditTrail(t *testing.T) {
	h := newHarness(t)

	// A fresh install seeds admin/admin. The admin here keeps that password: what
	// is under test is authorisation, not password hygiene, which the main suite
	// already covers.
	admin := h.browserFor("admin", "admin")

	const operatorPass = "operator-password"
	const viewerPass = "viewer-password"

	// ---- the admin sets the estate up and delegates -------------------------
	var host apiHost
	admin.ok(http.MethodPost, "/api/hosts", map[string]any{
		"name": "pve-sim", "baseUrl": h.simURL,
		"tokenId": "root@pam!proxback", "tokenSecret": "sim-token-secret",
		"insecureTLS": true,
	}, &host)

	var target apiTarget
	admin.ok(http.MethodPost, "/api/targets", map[string]any{
		"name": "vm-storage", "endpoint": h.s3URL, "region": "us-east-1",
		"bucket": vmBucket, "accessKey": "proxback", "secretKey": "proxback-secret",
		"pathStyle": true,
	}, &target)

	var job apiJob
	admin.ok(http.MethodPost, "/api/jobs", map[string]any{
		"name": "nightly-vms", "kind": "vm", "targetId": target.ID,
		"schedule": "manual", "retention": 2, "enabled": true,
		"sources": []map[string]any{{"hostId": host.ID, "vmid": 100, "name": "web-01"}},
	}, &job)
	if job.ID == "" {
		t.Fatalf("created job = %+v", job)
	}

	var operatorAccount, viewerAccount apiAccount
	admin.ok(http.MethodPost, "/api/users", map[string]any{
		"username": "opsy", "password": operatorPass, "role": "operator",
	}, &operatorAccount)
	admin.ok(http.MethodPost, "/api/users", map[string]any{
		"username": "looky", "password": viewerPass, "role": "viewer",
	}, &viewerAccount)
	if operatorAccount.Role != "operator" || viewerAccount.Role != "viewer" {
		t.Fatalf("created accounts = %+v, %+v", operatorAccount, viewerAccount)
	}

	// The listing carries the contract's fields and no hash.
	var accounts []apiAccount
	code, raw := admin.do(http.MethodGet, "/api/users", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/users = %d (%s)", code, raw)
	}
	if err := json.Unmarshal(raw, &accounts); err != nil {
		t.Fatalf("decode users: %v", err)
	}
	if len(accounts) != 3 {
		t.Fatalf("users = %+v, want the admin, the operator and the viewer", accounts)
	}
	for _, needle := range []string{"passwordHash", "password_hash", "$2a$", "$2b$", operatorPass, viewerPass} {
		if strings.Contains(string(raw), needle) {
			t.Fatalf("GET /api/users leaked %q: %s", needle, raw)
		}
	}
	for _, a := range accounts {
		if a.Role == "" || a.CreatedAt == "" {
			t.Fatalf("account %+v is missing contract fields", a)
		}
	}

	operator := h.browserFor("opsy", operatorPass)
	viewer := h.browserFor("looky", viewerPass)

	// Each session knows its own role, which is how the console decides what to
	// show. What it hides is courtesy; the rest of this test is the enforcement.
	for _, who := range []struct {
		b    *browser
		role string
	}{{admin, "admin"}, {operator, "operator"}, {viewer, "viewer"}} {
		var me struct {
			User apiAccount `json:"user"`
			Role string     `json:"role"`
		}
		who.b.ok(http.MethodGet, "/api/me", nil, &me)
		if me.User.Role != who.role || me.Role != who.role {
			t.Fatalf("GET /api/me as %s = %+v, want role %s", who.b.who, me, who.role)
		}
		if me.User.LastLoginAt == nil {
			t.Fatalf("GET /api/me as %s has no lastLoginAt after signing in", who.b.who)
		}
	}

	// ---- the operator runs work but cannot touch storage -------------------
	var started struct {
		RunID string `json:"runId"`
	}
	operator.ok(http.MethodPost, "/api/jobs/"+job.ID+"/run", nil, &started)
	if started.RunID == "" {
		t.Fatal("the operator's run produced no run id")
	}
	run := operator.waitRun(started.RunID, 3*time.Minute)
	if run.Status != "success" {
		t.Fatalf("the operator's run finished %q: %s", run.Status, run.Error)
	}

	msg := operator.refused(http.MethodPost, "/api/targets", map[string]any{
		"name": "operator-bucket", "endpoint": h.s3URL, "region": "us-east-1",
		"bucket": "not-allowed", "accessKey": "k", "secretKey": "s", "pathStyle": true,
	})
	if !strings.Contains(msg, "admin") {
		t.Errorf("refusal message = %q, want it to name the role required", msg)
	}
	operator.refused(http.MethodPost, "/api/hosts", map[string]any{
		"name": "another", "baseUrl": h.simURL, "tokenId": "root@pam!x", "tokenSecret": "y",
	})
	operator.refused(http.MethodGet, "/api/users", nil)
	operator.refused(http.MethodGet, "/api/audit", nil)
	// The target really was not created.
	var targets []apiTarget
	operator.ok(http.MethodGet, "/api/targets", nil, &targets)
	if len(targets) != 1 {
		t.Fatalf("targets after the refusal = %+v, want only the admin's", targets)
	}

	// ---- the viewer can only look -----------------------------------------
	viewer.refused(http.MethodPost, "/api/jobs/"+job.ID+"/run", nil)
	viewer.refused(http.MethodPost, "/api/jobs", map[string]any{
		"name": "sneaky", "kind": "vm", "targetId": target.ID,
		"sources": []map[string]any{{"hostId": host.ID, "vmid": 101}},
	})
	viewer.refused(http.MethodDelete, "/api/jobs/"+job.ID, nil)
	viewer.refused(http.MethodPost, "/api/restores", map[string]any{"backupId": "whatever"})
	viewer.refused(http.MethodGet, "/api/audit", nil)
	var seenJobs []apiJob
	viewer.ok(http.MethodGet, "/api/jobs", nil, &seenJobs)
	if len(seenJobs) != 1 {
		t.Fatalf("viewer sees jobs %+v, want the one that exists", seenJobs)
	}
	var runs []apiRun
	viewer.ok(http.MethodGet, "/api/runs", nil, &runs)
	if len(runs) == 0 {
		t.Fatal("viewer cannot see run history")
	}
	// A viewer changing their own password needs no role at all.
	viewer.ok(http.MethodPost, "/api/me/password", map[string]any{
		"currentPassword": viewerPass, "newPassword": "viewer-new-password",
	}, nil)

	// ---- the admin sees all of it in the trail ------------------------------
	var trail []apiAuditEntry
	admin.ok(http.MethodGet, "/api/audit?limit=1000", nil, &trail)
	if len(trail) < 5 {
		t.Fatalf("audit trail has %d entries: %+v", len(trail), trail)
	}
	// Newest first.
	for i := 1; i < len(trail); i++ {
		if trail[i].ID >= trail[i-1].ID {
			t.Fatalf("audit trail is not newest first: %d then %d", trail[i-1].ID, trail[i].ID)
		}
	}
	// The operator's run is recorded as theirs, and it succeeded.
	runStart := findEntry(t, trail, func(e apiAuditEntry) bool {
		return e.Action == "run.start" && e.Actor == "opsy"
	})
	if runStart.Result != "ok" || runStart.ObjectID != job.ID || !strings.Contains(runStart.Detail, started.RunID) {
		t.Fatalf("run start entry = %+v", runStart)
	}
	if runStart.SourceIP == "" || runStart.At == "" || runStart.ActorID != operatorAccount.ID {
		t.Fatalf("run start entry = %+v, want an actor id, a source address and a timestamp", runStart)
	}
	// Both refused attempts are there, as denials, naming who tried what.
	operatorDenied := findEntry(t, trail, func(e apiAuditEntry) bool {
		return e.Result == "denied" && e.Actor == "opsy" && strings.Contains(e.ObjectID, "/api/targets")
	})
	if operatorDenied.Action != "access.denied" || !strings.Contains(operatorDenied.Detail, "operator") {
		t.Fatalf("operator denial entry = %+v", operatorDenied)
	}
	viewerDenied := findEntry(t, trail, func(e apiAuditEntry) bool {
		return e.Result == "denied" && e.Actor == "looky" && strings.Contains(e.ObjectID, "/run")
	})
	if viewerDenied.Action != "access.denied" || !strings.Contains(viewerDenied.Detail, "viewer") {
		t.Fatalf("viewer denial entry = %+v", viewerDenied)
	}
	// Delegation itself, the estate the admin built, and the sign-ins are all in
	// the trail too.
	for _, want := range []struct {
		action, actor string
	}{
		{"user.create", "admin"},
		{"host.create", "admin"},
		{"target.create", "admin"},
		{"job.create", "admin"},
		{"session.signin", "opsy"},
		{"session.signin", "looky"},
		{"user.modify", "looky"}, // the viewer's own password change
	} {
		findEntry(t, trail, func(e apiAuditEntry) bool {
			return e.Action == want.action && e.Actor == want.actor
		})
	}
	// No secret from any of it was stored.
	for _, secret := range []string{"sim-token-secret", "proxback-secret", operatorPass, viewerPass, "viewer-new-password"} {
		if strings.Contains(string(mustJSON(t, trail)), secret) {
			t.Fatalf("the audit trail contains the secret %q", secret)
		}
	}
	// The filters the console uses.
	var filtered []apiAuditEntry
	admin.ok(http.MethodGet, "/api/audit?action=access.denied", nil, &filtered)
	if len(filtered) == 0 {
		t.Fatal("no denied entries when filtering by action")
	}
	for _, e := range filtered {
		if e.Action != "access.denied" || e.Result != "denied" {
			t.Fatalf("action filter returned %+v", e)
		}
	}
	admin.ok(http.MethodGet, "/api/audit?actor=opsy", nil, &filtered)
	if len(filtered) == 0 {
		t.Fatal("no entries when filtering by actor")
	}
	for _, e := range filtered {
		if e.Actor != "opsy" {
			t.Fatalf("actor filter returned %+v", e)
		}
	}
	admin.ok(http.MethodGet, "/api/audit?limit=2", nil, &filtered)
	if len(filtered) != 2 {
		t.Fatalf("limit filter returned %d entries", len(filtered))
	}

	// ---- the last admin cannot be removed ----------------------------------
	var me struct {
		User apiAccount `json:"user"`
	}
	admin.ok(http.MethodGet, "/api/me", nil, &me)
	for _, attempt := range []struct {
		method, path string
		body         any
		what         string
	}{
		{http.MethodDelete, "/api/users/" + strconv.FormatInt(me.User.ID, 10), nil, "delete"},
		{http.MethodPatch, "/api/users/" + strconv.FormatInt(me.User.ID, 10),
			map[string]any{"role": "operator"}, "demote"},
	} {
		code, raw := admin.do(attempt.method, attempt.path, attempt.body)
		if code != http.StatusConflict {
			t.Fatalf("%s the last admin = %d (%s), want 409", attempt.what, code, raw)
		}
		if !strings.Contains(string(raw), "last admin") {
			t.Fatalf("%s refusal = %s, want it to explain the last-admin rule", attempt.what, raw)
		}
	}
	// The admin still administers, so nothing was half-applied.
	admin.ok(http.MethodGet, "/api/users", nil, &accounts)

	// ---- deleting a user revokes their session at once ---------------------
	admin.ok(http.MethodDelete, "/api/users/"+strconv.FormatInt(operatorAccount.ID, 10), nil, nil)
	if code, raw := operator.do(http.MethodGet, "/api/me", nil); code != http.StatusUnauthorized {
		t.Fatalf("the deleted operator's session = %d (%s), want 401", code, raw)
	}
	if code, _ := h.browserLoginAttempt("opsy", operatorPass); code != http.StatusUnauthorized {
		t.Fatalf("signing in as the deleted operator = %d, want 401", code)
	}
	// And the record holds both the deletion that happened and the one that was
	// refused: the attempt on the last admin is as interesting as the success.
	admin.ok(http.MethodGet, "/api/audit?action=user.delete", nil, &trail)
	if len(trail) != 2 {
		t.Fatalf("user deletion entries = %+v, want the success and the refusal", trail)
	}
	if trail[0].ObjectName != "opsy" || trail[0].Actor != "admin" || trail[0].Result != "ok" {
		t.Fatalf("newest user deletion entry = %+v", trail[0])
	}
	if trail[1].ObjectName != "admin" || trail[1].Result != "error" ||
		!strings.Contains(trail[1].Detail, "last admin") {
		t.Fatalf("refused deletion entry = %+v", trail[1])
	}
}

// browserLoginAttempt tries a sign-in without requiring it to work.
func (h *harness) browserLoginAttempt(username, password string) (int, []byte) {
	h.t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		h.t.Fatalf("cookie jar: %v", err)
	}
	b := &browser{t: h.t, client: &http.Client{Jar: jar, Timeout: time.Minute}, base: h.base, who: username}
	return b.do(http.MethodPost, "/api/login",
		map[string]string{"username": username, "password": password})
}

// findEntry returns the newest trail entry matching pred, failing the test when
// there is none.
func findEntry(t *testing.T, trail []apiAuditEntry, pred func(apiAuditEntry) bool) apiAuditEntry {
	t.Helper()
	for _, e := range trail {
		if pred(e) {
			return e
		}
	}
	t.Fatalf("no matching audit entry in %+v", trail)
	return apiAuditEntry{}
}
