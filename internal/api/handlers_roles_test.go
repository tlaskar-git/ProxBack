package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"proxback/internal/auth"
	"proxback/internal/store"
)

// The password every account in these tests is created with. It is long enough
// to satisfy the same rule the setup flow applies.
const testPassword = "correct-horse-battery"

// account creates a user with a role and returns a session cookie for them, so a
// test can exercise the API as that role through the real middleware.
func (ts *testServer) account(t *testing.T, username string, role store.Role) *http.Cookie {
	t.Helper()
	ctx := context.Background()
	user, err := auth.New(ts.st).CreateUserWithRole(ctx, username, testPassword, role)
	if err != nil {
		t.Fatalf("create %s %s: %v", role, username, err)
	}
	token, err := auth.New(ts.st).StartSession(ctx, user.ID)
	if err != nil {
		t.Fatalf("start session for %s: %v", username, err)
	}
	return &http.Cookie{Name: auth.SessionCookieName, Value: token}
}

// as performs a request as whoever holds cookie and returns the status and the
// raw body. A nil cookie sends no session at all.
func (ts *testServer) as(t *testing.T, cookie *http.Cookie, method, path string, body any) (int, []byte) {
	t.Helper()
	var rdr *strings.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = strings.NewReader(string(raw))
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	ts.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// auditRows reads the whole trail straight out of the store, which is how the
// secret scan gets at every column rather than only the ones an endpoint shows.
func (ts *testServer) auditRows(t *testing.T) []*store.AuditEntry {
	t.Helper()
	rows, err := ts.st.AuditEntries(context.Background(), store.AuditFilter{Limit: store.MaxAuditLimit})
	if err != nil {
		t.Fatalf("read audit trail: %v", err)
	}
	return rows
}

// TestRoleCapabilityMatrix is the enforcement contract: for every role, every
// route either answers or answers 403, decided by the group the route is
// registered in and nothing else. The assertion is deliberately about 403 rather
// than about success, because most of these requests are malformed on purpose —
// what matters is whether authorisation let them through to be judged.
func TestRoleCapabilityMatrix(t *testing.T) {
	ts := newTestServer(t)
	cookies := map[store.Role]*http.Cookie{
		store.RoleAdmin:    ts.cookie, // newTestServer's admin
		store.RoleOperator: ts.account(t, "opsy", store.RoleOperator),
		store.RoleViewer:   ts.account(t, "looky", store.RoleViewer),
	}

	routes := []struct {
		method string
		path   string
		// needs is the least role the route requires. RoleViewer means any
		// signed-in user.
		needs store.Role
		// skipAllowed leaves out the "not refused" half of the check for routes
		// whose handler would reach the network.
		skipAllowed bool
	}{
		// Reads, and the two routes about the caller's own session.
		{method: http.MethodGet, path: "/api/me", needs: store.RoleViewer},
		{method: http.MethodGet, path: "/api/dashboard", needs: store.RoleViewer},
		{method: http.MethodGet, path: "/api/posture", needs: store.RoleViewer},
		{method: http.MethodGet, path: "/api/hosts", needs: store.RoleViewer},
		{method: http.MethodGet, path: "/api/vms", needs: store.RoleViewer},
		{method: http.MethodGet, path: "/api/targets", needs: store.RoleViewer},
		{method: http.MethodGet, path: "/api/jobs", needs: store.RoleViewer},
		{method: http.MethodGet, path: "/api/runs", needs: store.RoleViewer},
		{method: http.MethodGet, path: "/api/backups", needs: store.RoleViewer},
		{method: http.MethodGet, path: "/api/agents", needs: store.RoleViewer},
		{method: http.MethodGet, path: "/api/helpers", needs: store.RoleViewer},
		{method: http.MethodGet, path: "/api/settings", needs: store.RoleViewer},
		// Changing your own password is not a privilege: it is your account.
		{method: http.MethodPost, path: "/api/me/password", needs: store.RoleViewer},

		// Operator: running work and editing jobs.
		{method: http.MethodPost, path: "/api/jobs", needs: store.RoleOperator},
		{method: http.MethodPatch, path: "/api/jobs/no-such-job", needs: store.RoleOperator},
		{method: http.MethodDelete, path: "/api/jobs/no-such-job", needs: store.RoleOperator},
		{method: http.MethodPost, path: "/api/jobs/no-such-job/run", needs: store.RoleOperator},
		{method: http.MethodPost, path: "/api/runs/clear", needs: store.RoleOperator},
		{method: http.MethodDelete, path: "/api/runs/no-such-run", needs: store.RoleOperator},
		{method: http.MethodPost, path: "/api/runs/no-such-run/cancel", needs: store.RoleOperator},
		{method: http.MethodPost, path: "/api/runs/no-such-run/retry", needs: store.RoleOperator},
		{method: http.MethodPost, path: "/api/backups/no-such-backup/verify", needs: store.RoleOperator},
		{method: http.MethodDelete, path: "/api/backups/no-such-backup", needs: store.RoleOperator},
		{method: http.MethodPost, path: "/api/restores", needs: store.RoleOperator},

		// Admin: users, the trail, credentials, infrastructure, settings, updates.
		{method: http.MethodGet, path: "/api/users", needs: store.RoleAdmin},
		{method: http.MethodPost, path: "/api/users", needs: store.RoleAdmin},
		{method: http.MethodPatch, path: "/api/users/9999", needs: store.RoleAdmin},
		{method: http.MethodDelete, path: "/api/users/9999", needs: store.RoleAdmin},
		{method: http.MethodGet, path: "/api/audit", needs: store.RoleAdmin},
		{method: http.MethodPost, path: "/api/hosts", needs: store.RoleAdmin},
		{method: http.MethodDelete, path: "/api/hosts/no-such-host", needs: store.RoleAdmin},
		{method: http.MethodPost, path: "/api/targets", needs: store.RoleAdmin},
		{method: http.MethodPost, path: "/api/targets/no-such-target/test", needs: store.RoleAdmin},
		{method: http.MethodDelete, path: "/api/targets/no-such-target", needs: store.RoleAdmin},
		{method: http.MethodPost, path: "/api/agents/enroll-token", needs: store.RoleAdmin},
		{method: http.MethodDelete, path: "/api/agents/no-such-agent", needs: store.RoleAdmin},
		{method: http.MethodPost, path: "/api/helpers/enroll-token", needs: store.RoleAdmin},
		{method: http.MethodPost, path: "/api/helpers/deploy", needs: store.RoleAdmin},
		{method: http.MethodPost, path: "/api/helpers/no-such-helper/assign", needs: store.RoleAdmin},
		{method: http.MethodDelete, path: "/api/helpers/no-such-helper", needs: store.RoleAdmin},
		{method: http.MethodPut, path: "/api/settings", needs: store.RoleAdmin},
		{method: http.MethodPost, path: "/api/settings/test-webhook", needs: store.RoleAdmin},
		// Applying an update contacts the release repository, so only the refusal
		// half is asserted here.
		{method: http.MethodPost, path: "/api/update/apply", needs: store.RoleAdmin, skipAllowed: true},
		{method: http.MethodGet, path: "/api/downloads/status", needs: store.RoleAdmin},
	}

	for _, route := range routes {
		for _, role := range store.Roles {
			var body any
			if route.method != http.MethodGet && route.method != http.MethodDelete {
				body = map[string]any{}
			}
			code, raw := ts.as(t, cookies[role], route.method, route.path, body)
			allowed := role.AtLeast(route.needs)
			switch {
			case !allowed && code != http.StatusForbidden:
				t.Errorf("%s %s as %s = %d (%s), want 403", route.method, route.path, role, code, raw)
			case !allowed:
				// The refusal must be a JSON error, never an empty body and never a
				// 404 pretending the endpoint is not there.
				var out map[string]any
				if err := json.Unmarshal(raw, &out); err != nil || out["error"] == nil {
					t.Errorf("%s %s as %s: body %q, want a JSON error", route.method, route.path, role, raw)
				}
			case allowed && !route.skipAllowed && (code == http.StatusForbidden || code == http.StatusUnauthorized):
				t.Errorf("%s %s as %s = %d (%s), want it to be let through",
					route.method, route.path, role, code, raw)
			}
		}
		// And with no session at all, everything under /api is 401.
		if code, _ := ts.as(t, nil, route.method, route.path, nil); code != http.StatusUnauthorized {
			t.Errorf("%s %s with no session = %d, want 401", route.method, route.path, code)
		}
	}
}

// The concrete cases from the PLAN's table: a viewer cannot create a job, an
// operator can create and run one but cannot create a storage target, and an
// admin can do both.
func TestRoleBoundaryOnJobsAndTargets(t *testing.T) {
	ts := newTestServer(t)
	tgt := ts.target(t)
	operator := ts.account(t, "opsy", store.RoleOperator)
	viewer := ts.account(t, "looky", store.RoleViewer)

	if code, raw := ts.as(t, viewer, http.MethodPost, "/api/jobs",
		jobBody(tgt.ID, map[string]any{"kind": "manual"})); code != http.StatusForbidden {
		t.Fatalf("viewer creating a job = %d (%s), want 403", code, raw)
	}
	code, raw := ts.as(t, operator, http.MethodPost, "/api/jobs",
		jobBody(tgt.ID, map[string]any{"kind": "manual"}))
	if code != http.StatusOK {
		t.Fatalf("operator creating a job = %d (%s), want 200", code, raw)
	}
	var created struct {
		ID string `json:"id"`
	}
	decodeJSONBody(t, raw, &created)

	targetBody := map[string]any{
		"name": "second-bucket", "kind": "s3", "endpoint": "http://127.0.0.1:1",
		"bucket": "proxback-2", "accessKey": "k", "secretKey": "s", "pathStyle": true,
	}
	if code, raw := ts.as(t, operator, http.MethodPost, "/api/targets", targetBody); code != http.StatusForbidden {
		t.Fatalf("operator creating a target = %d (%s), want 403", code, raw)
	}
	if code, raw := ts.as(t, ts.cookie, http.MethodPost, "/api/targets", targetBody); code != http.StatusOK {
		t.Fatalf("admin creating a target = %d (%s), want 200", code, raw)
	}
	// The operator may edit and delete the job they made; the viewer may only see it.
	if code, raw := ts.as(t, operator, http.MethodPatch, "/api/jobs/"+created.ID,
		map[string]any{"enabled": false}); code != http.StatusOK {
		t.Fatalf("operator editing a job = %d (%s), want 200", code, raw)
	}
	if code, raw := ts.as(t, viewer, http.MethodGet, "/api/jobs", nil); code != http.StatusOK {
		t.Fatalf("viewer listing jobs = %d (%s), want 200", code, raw)
	}
	if code, raw := ts.as(t, viewer, http.MethodDelete, "/api/jobs/"+created.ID, nil); code != http.StatusForbidden {
		t.Fatalf("viewer deleting a job = %d (%s), want 403", code, raw)
	}
}

// GET /api/me carries the role, so the console can hide what a user cannot do.
func TestMeReportsTheRole(t *testing.T) {
	ts := newTestServer(t)
	for _, tc := range []struct {
		cookie *http.Cookie
		role   store.Role
	}{
		{ts.cookie, store.RoleAdmin},
		{ts.account(t, "opsy", store.RoleOperator), store.RoleOperator},
		{ts.account(t, "looky", store.RoleViewer), store.RoleViewer},
	} {
		code, raw := ts.as(t, tc.cookie, http.MethodGet, "/api/me", nil)
		if code != http.StatusOK {
			t.Fatalf("GET /api/me as %s = %d (%s)", tc.role, code, raw)
		}
		var out struct {
			User struct {
				Username string     `json:"username"`
				Role     store.Role `json:"role"`
			} `json:"user"`
			Role store.Role `json:"role"`
		}
		decodeJSONBody(t, raw, &out)
		if out.User.Role != tc.role || out.Role != tc.role {
			t.Fatalf("GET /api/me as %s = %+v, want role %s on the user and at the top level",
				tc.role, out, tc.role)
		}
	}
}

// User administration: creation, validation, the two safety rules, and the
// guarantee that no response ever carries a password hash.
func TestUserAdministration(t *testing.T) {
	ts := newTestServer(t)

	// Creation validates its input.
	for _, bad := range []struct {
		body map[string]any
		code int
		what string
	}{
		{map[string]any{"password": testPassword, "role": "viewer"}, http.StatusBadRequest, "no username"},
		{map[string]any{"username": "shorty", "password": "1234567", "role": "viewer"}, http.StatusBadRequest, "short password"},
		{map[string]any{"username": "norole", "password": testPassword}, http.StatusBadRequest, "no role"},
		{map[string]any{"username": "boss", "password": testPassword, "role": "superuser"}, http.StatusBadRequest, "unknown role"},
		{map[string]any{"username": "admin", "password": testPassword, "role": "admin"}, http.StatusConflict, "duplicate name"},
	} {
		if code, raw := ts.as(t, ts.cookie, http.MethodPost, "/api/users", bad.body); code != bad.code {
			t.Errorf("create user with %s = %d (%s), want %d", bad.what, code, raw, bad.code)
		}
	}

	code, raw := ts.as(t, ts.cookie, http.MethodPost, "/api/users",
		map[string]any{"username": "opsy", "password": testPassword, "role": "operator"})
	if code != http.StatusOK {
		t.Fatalf("create operator = %d (%s)", code, raw)
	}
	var operator userDTO
	decodeJSONBody(t, raw, &operator)
	if operator.Role != store.RoleOperator || operator.ID == 0 || operator.CreatedAt.IsZero() {
		t.Fatalf("created operator = %+v", operator)
	}
	if operator.LastLoginAt != nil {
		t.Fatalf("created operator has a lastLoginAt of %v, want it absent", operator.LastLoginAt)
	}

	// The listing carries the contract's fields and never a hash.
	code, raw = ts.as(t, ts.cookie, http.MethodGet, "/api/users", nil)
	if code != http.StatusOK {
		t.Fatalf("list users = %d (%s)", code, raw)
	}
	var users []userDTO
	decodeJSONBody(t, raw, &users)
	if len(users) != 2 {
		t.Fatalf("users = %+v, want the admin and the operator", users)
	}
	assertNoHash(t, "GET /api/users", raw)

	// A role change, and a password reset by an admin.
	code, raw = ts.as(t, ts.cookie, http.MethodPatch, "/api/users/"+itoa(operator.ID),
		map[string]any{"role": "viewer"})
	if code != http.StatusOK {
		t.Fatalf("demote operator = %d (%s)", code, raw)
	}
	var patched userDTO
	decodeJSONBody(t, raw, &patched)
	if patched.Role != store.RoleViewer {
		t.Fatalf("patched user = %+v, want role viewer", patched)
	}
	assertNoHash(t, "PATCH /api/users/{id}", raw)
	if code, raw := ts.as(t, ts.cookie, http.MethodPatch, "/api/users/"+itoa(operator.ID),
		map[string]any{}); code != http.StatusBadRequest {
		t.Fatalf("empty patch = %d (%s), want 400", code, raw)
	}
	if code, raw := ts.as(t, ts.cookie, http.MethodPatch, "/api/users/"+itoa(operator.ID),
		map[string]any{"password": "short"}); code != http.StatusBadRequest {
		t.Fatalf("patch with a short password = %d (%s), want 400", code, raw)
	}
	if code, _ := ts.as(t, ts.cookie, http.MethodPatch, "/api/users/9999",
		map[string]any{"role": "viewer"}); code != http.StatusNotFound {
		t.Fatalf("patching an unknown user = %d, want 404", code)
	}

	// Deletion.
	if code, raw := ts.as(t, ts.cookie, http.MethodDelete, "/api/users/"+itoa(operator.ID), nil); code != http.StatusOK {
		t.Fatalf("delete user = %d (%s)", code, raw)
	}
	if code, _ := ts.as(t, ts.cookie, http.MethodDelete, "/api/users/9999", nil); code != http.StatusNotFound {
		t.Fatalf("deleting an unknown user = %d, want 404", code)
	}
}

// The last admin can be neither deleted nor demoted, through the API, with a
// message that says why — and the refusal is recorded.
func TestLastAdminCannotBeRemovedOrDemoted(t *testing.T) {
	ts := newTestServer(t)
	admin, err := ts.st.UserByUsername(context.Background(), "admin")
	if err != nil {
		t.Fatalf("load admin: %v", err)
	}

	code, raw := ts.as(t, ts.cookie, http.MethodDelete, "/api/users/"+itoa(admin.ID), nil)
	if code != http.StatusConflict {
		t.Fatalf("deleting the last admin = %d (%s), want 409", code, raw)
	}
	if msg := errorMessage(t, raw); !strings.Contains(msg, "last admin") {
		t.Errorf("delete refusal = %q, want it to explain the last-admin rule", msg)
	}
	code, raw = ts.as(t, ts.cookie, http.MethodPatch, "/api/users/"+itoa(admin.ID),
		map[string]any{"role": "operator"})
	if code != http.StatusConflict {
		t.Fatalf("demoting the last admin = %d (%s), want 409", code, raw)
	}
	if msg := errorMessage(t, raw); !strings.Contains(msg, "last admin") {
		t.Errorf("demotion refusal = %q, want it to explain the last-admin rule", msg)
	}
	// The account is untouched: it is still an admin and can still administer.
	if code, _ := ts.as(t, ts.cookie, http.MethodGet, "/api/users", nil); code != http.StatusOK {
		t.Fatalf("admin lost access after the refusals: %d", code)
	}

	// With a second admin the first can go, and then the rule protects the second.
	second, err := auth.New(ts.st).CreateUserWithRole(context.Background(), "root2", testPassword, store.RoleAdmin)
	if err != nil {
		t.Fatalf("create second admin: %v", err)
	}
	if code, raw := ts.as(t, ts.cookie, http.MethodPatch, "/api/users/"+itoa(admin.ID),
		map[string]any{"role": "operator"}); code != http.StatusOK {
		t.Fatalf("demoting an admin while a second exists = %d (%s), want 200", code, raw)
	}
	// The demoted admin's own session is now an operator's: the check reads the
	// user afresh on every request, so a live session cannot outrank its account.
	if code, raw := ts.as(t, ts.cookie, http.MethodGet, "/api/users", nil); code != http.StatusForbidden {
		t.Fatalf("demoted admin listing users = %d (%s), want 403", code, raw)
	}
	if _, err := ts.st.UserByID(context.Background(), second.ID); err != nil {
		t.Fatalf("second admin: %v", err)
	}
}

// Deleting a user revokes their sessions immediately: the cookie they hold stops
// working on the very next request.
func TestDeletingAUserRevokesTheirSession(t *testing.T) {
	ts := newTestServer(t)
	victim := ts.account(t, "opsy", store.RoleOperator)

	if code, raw := ts.as(t, victim, http.MethodGet, "/api/me", nil); code != http.StatusOK {
		t.Fatalf("operator GET /api/me = %d (%s), want 200", code, raw)
	}
	user, err := ts.st.UserByUsername(context.Background(), "opsy")
	if err != nil {
		t.Fatalf("load operator: %v", err)
	}
	if code, raw := ts.as(t, ts.cookie, http.MethodDelete, "/api/users/"+itoa(user.ID), nil); code != http.StatusOK {
		t.Fatalf("delete operator = %d (%s)", code, raw)
	}
	if code, raw := ts.as(t, victim, http.MethodGet, "/api/me", nil); code != http.StatusUnauthorized {
		t.Fatalf("deleted user's session = %d (%s), want 401", code, raw)
	}
}

// A non-admin changes their own password through the existing endpoint, with no
// role required, and the new password is what works afterwards.
func TestNonAdminChangesOwnPassword(t *testing.T) {
	ts := newTestServer(t)
	viewer := ts.account(t, "looky", store.RoleViewer)

	if code, raw := ts.as(t, viewer, http.MethodPost, "/api/me/password", map[string]any{
		"currentPassword": "wrong-password", "newPassword": "a-brand-new-secret",
	}); code != http.StatusUnauthorized {
		t.Fatalf("wrong current password = %d (%s), want 401", code, raw)
	}
	if code, raw := ts.as(t, viewer, http.MethodPost, "/api/me/password", map[string]any{
		"currentPassword": testPassword, "newPassword": "a-brand-new-secret",
	}); code != http.StatusOK {
		t.Fatalf("viewer changing their own password = %d (%s), want 200", code, raw)
	}
	// The new password is live and the role has not changed.
	code, raw := ts.as(t, nil, http.MethodPost, "/api/login", map[string]any{
		"username": "looky", "password": "a-brand-new-secret",
	})
	if code != http.StatusOK {
		t.Fatalf("login with the new password = %d (%s), want 200", code, raw)
	}
	assertNoHash(t, "POST /api/login", raw)
	var out struct {
		User userDTO `json:"user"`
	}
	decodeJSONBody(t, raw, &out)
	if out.User.Role != store.RoleViewer {
		t.Fatalf("logged-in user = %+v, want role viewer", out.User)
	}
	if code, _ := ts.as(t, nil, http.MethodPost, "/api/login", map[string]any{
		"username": "looky", "password": testPassword,
	}); code != http.StatusUnauthorized {
		t.Fatalf("login with the old password = %d, want 401", code)
	}
}

// A successful action and a denied one are both recorded, with the right result,
// actor and source address — and the trail is readable through the API with its
// filters.
func TestAuditRecordsSuccessAndDenial(t *testing.T) {
	ts := newTestServer(t)
	tgt := ts.target(t)
	viewer := ts.account(t, "looky", store.RoleViewer)

	code, raw := ts.as(t, ts.cookie, http.MethodPost, "/api/jobs",
		jobBody(tgt.ID, map[string]any{"kind": "manual"}))
	if code != http.StatusOK {
		t.Fatalf("admin creating a job = %d (%s)", code, raw)
	}
	if code, raw := ts.as(t, viewer, http.MethodPost, "/api/jobs",
		jobBody(tgt.ID, map[string]any{"kind": "manual"})); code != http.StatusForbidden {
		t.Fatalf("viewer creating a job = %d (%s), want 403", code, raw)
	}

	created := findAudit(t, ts.auditRows(t), store.AuditJobCreate)
	if created.Result != store.AuditOK || created.Actor != "admin" || created.ObjectName != "nightly-vms" {
		t.Fatalf("job creation entry = %+v", created)
	}
	if created.SourceIP != "192.0.2.1" {
		t.Fatalf("job creation entry source ip = %q, want the request's address without its port", created.SourceIP)
	}
	if created.At.IsZero() || created.ActorID == 0 || created.ObjectKind != "job" {
		t.Fatalf("job creation entry = %+v", created)
	}
	denied := findAudit(t, ts.auditRows(t), store.AuditAccessDenied)
	if denied.Result != store.AuditDenied || denied.Actor != "looky" {
		t.Fatalf("denied entry = %+v", denied)
	}
	if !strings.Contains(denied.ObjectID, "/api/jobs") || !strings.Contains(denied.Detail, "viewer") {
		t.Fatalf("denied entry = %+v, want it to name the route and the role", denied)
	}

	// Through the API: admin only, newest first, and the three filters work.
	if code, raw := ts.as(t, viewer, http.MethodGet, "/api/audit", nil); code != http.StatusForbidden {
		t.Fatalf("viewer reading the audit trail = %d (%s), want 403", code, raw)
	}
	code, raw = ts.as(t, ts.cookie, http.MethodGet, "/api/audit", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/audit = %d (%s)", code, raw)
	}
	var entries []store.AuditEntry
	decodeJSONBody(t, raw, &entries)
	if len(entries) < 2 {
		t.Fatalf("audit entries = %+v", entries)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].At.After(entries[i-1].At) {
			t.Fatalf("entries are not newest first: %+v", entries)
		}
	}
	// The wire shape the console is built against.
	var wire []map[string]any
	decodeJSONBody(t, raw, &wire)
	for _, field := range []string{"id", "at", "actor", "actorId", "action",
		"objectKind", "objectId", "objectName", "result", "sourceIp", "detail"} {
		if _, ok := wire[0][field]; !ok {
			t.Errorf("audit entry has no %q field: %+v", field, wire[0])
		}
	}

	// Two denials by now: the job the viewer could not create and the trail they
	// could not read. Both are theirs and both say denied.
	_, raw = ts.as(t, ts.cookie, http.MethodGet, "/api/audit?action="+store.AuditAccessDenied, nil)
	decodeJSONBody(t, raw, &entries)
	if len(entries) != 2 {
		t.Fatalf("action filter = %+v, want the two denials", entries)
	}
	for _, e := range entries {
		if e.Action != store.AuditAccessDenied || e.Result != store.AuditDenied {
			t.Fatalf("action filter returned %+v", e)
		}
	}
	_, raw = ts.as(t, ts.cookie, http.MethodGet, "/api/audit?actor=looky", nil)
	decodeJSONBody(t, raw, &entries)
	if len(entries) != 2 {
		t.Fatalf("actor filter = %+v, want only the viewer's entries", entries)
	}
	for _, e := range entries {
		if e.Actor != "looky" {
			t.Fatalf("actor filter returned %+v", e)
		}
	}
	_, raw = ts.as(t, ts.cookie, http.MethodGet, "/api/audit?limit=1", nil)
	decodeJSONBody(t, raw, &entries)
	if len(entries) != 1 {
		t.Fatalf("limit filter = %+v", entries)
	}
	if code, _ := ts.as(t, ts.cookie, http.MethodGet, "/api/audit?limit=0", nil); code != http.StatusBadRequest {
		t.Fatalf("limit=0 = %d, want 400", code)
	}
	if code, _ := ts.as(t, ts.cookie, http.MethodGet, "/api/audit?limit=abc", nil); code != http.StatusBadRequest {
		t.Fatalf("limit=abc = %d, want 400", code)
	}
}

// A failed sign-in is recorded with the attempted username and without the
// password, and a successful one stamps the account.
func TestAuditRecordsSignIn(t *testing.T) {
	ts := newTestServer(t)

	if code, _ := ts.as(t, nil, http.MethodPost, "/api/login",
		map[string]any{"username": "ghost", "password": "hunter2hunter2"}); code != http.StatusUnauthorized {
		t.Fatal("a login with an unknown user succeeded")
	}
	failed := findAudit(t, ts.auditRows(t), store.AuditSignInFailed)
	if failed.Actor != "ghost" || failed.Result != store.AuditDenied || failed.ActorID != 0 {
		t.Fatalf("failed sign-in entry = %+v", failed)
	}

	if code, raw := ts.as(t, nil, http.MethodPost, "/api/login",
		map[string]any{"username": "admin", "password": "correct-horse-battery"}); code != http.StatusOK {
		t.Fatalf("admin login = %d (%s)", code, raw)
	}
	ok := findAudit(t, ts.auditRows(t), store.AuditSignIn)
	if ok.Actor != "admin" || ok.Result != store.AuditOK || ok.ActorID == 0 {
		t.Fatalf("sign-in entry = %+v", ok)
	}
	user, err := ts.st.UserByUsername(context.Background(), "admin")
	if err != nil {
		t.Fatalf("load admin: %v", err)
	}
	if user.LastLoginAt == nil {
		t.Fatal("a successful sign-in did not record lastLoginAt")
	}
}

// The trail must never become a place secrets are kept. A single planted value is
// pushed through every endpoint that takes a credential, and then every column of
// every written row is scanned for it.
func TestAuditNeverStoresSecrets(t *testing.T) {
	ts := newTestServer(t)
	const planted = "PLANTED-SECRET-8f3a9c2b-do-not-store"

	// A new account whose password is the planted secret.
	if code, raw := ts.as(t, ts.cookie, http.MethodPost, "/api/users", map[string]any{
		"username": "snoop", "password": planted, "role": "operator",
	}); code != http.StatusOK {
		t.Fatalf("create user = %d (%s)", code, raw)
	}
	// A storage target whose secret key is the planted secret. The endpoint is a
	// closed port, so the probe fails immediately and the target is still stored.
	if code, raw := ts.as(t, ts.cookie, http.MethodPost, "/api/targets", map[string]any{
		"name": "secret-bucket", "kind": "s3", "endpoint": "http://127.0.0.1:1",
		"bucket": "vault", "accessKey": planted + "-access", "secretKey": planted, "pathStyle": true,
	}); code != http.StatusOK {
		t.Fatalf("create target = %d (%s)", code, raw)
	}
	// Settings whose webhook URL carries the planted secret in its path, which is
	// exactly how a real webhook token arrives.
	if code, raw := ts.as(t, ts.cookie, http.MethodPut, "/api/settings", map[string]any{
		"webhookUrl": "https://hooks.example.com/services/" + planted,
	}); code != http.StatusOK {
		t.Fatalf("save settings = %d (%s)", code, raw)
	}
	// A failed sign-in whose password is the planted secret.
	if code, _ := ts.as(t, nil, http.MethodPost, "/api/login", map[string]any{
		"username": "snoop", "password": planted + "-wrong",
	}); code != http.StatusUnauthorized {
		t.Fatal("a login with a wrong password succeeded")
	}
	// A password change that fails because the current password is wrong, and one
	// that succeeds — the new password is the planted secret.
	snoop := ts.account(t, "snoop2", store.RoleViewer)
	if code, _ := ts.as(t, snoop, http.MethodPost, "/api/me/password", map[string]any{
		"currentPassword": planted, "newPassword": planted + "-new",
	}); code != http.StatusUnauthorized {
		t.Fatal("a password change with a wrong current password succeeded")
	}
	if code, raw := ts.as(t, snoop, http.MethodPost, "/api/me/password", map[string]any{
		"currentPassword": testPassword, "newPassword": planted + "-new",
	}); code != http.StatusOK {
		t.Fatalf("change own password = %d (%s)", code, raw)
	}
	// An admin resetting somebody's password to the planted secret.
	user, err := ts.st.UserByUsername(context.Background(), "snoop")
	if err != nil {
		t.Fatalf("load snoop: %v", err)
	}
	if code, raw := ts.as(t, ts.cookie, http.MethodPatch, "/api/users/"+itoa(user.ID), map[string]any{
		"password": planted + "-reset",
	}); code != http.StatusOK {
		t.Fatalf("reset password = %d (%s)", code, raw)
	}

	rows := ts.auditRows(t)
	if len(rows) < 5 {
		t.Fatalf("only %d audit entries were written; the scan would prove nothing", len(rows))
	}
	for _, e := range rows {
		for field, value := range map[string]string{
			"actor": e.Actor, "action": e.Action, "objectKind": e.ObjectKind,
			"objectId": e.ObjectID, "objectName": e.ObjectName,
			"result": e.Result, "sourceIp": e.SourceIP, "detail": e.Detail,
		} {
			if strings.Contains(value, planted) {
				t.Errorf("audit entry %d has the planted secret in %s: %q", e.ID, field, value)
			}
		}
	}
	// And it is not in the API's own rendering of the trail either.
	_, raw := ts.as(t, ts.cookie, http.MethodGet, "/api/audit?limit=1000", nil)
	if strings.Contains(string(raw), planted) {
		t.Errorf("GET /api/audit body contains the planted secret: %s", raw)
	}
}

// ---------------------------------------------------------------- helpers

// assertNoHash proves a response body carries nothing that looks like a stored
// password.
func assertNoHash(t *testing.T, what string, raw []byte) {
	t.Helper()
	body := string(raw)
	for _, needle := range []string{"passwordHash", "password_hash", "$2a$", "$2b$", "$2y$"} {
		if strings.Contains(body, needle) {
			t.Errorf("%s response contains %q: %s", what, needle, body)
		}
	}
}

// errorMessage pulls the "error" string out of a JSON error body.
func errorMessage(t *testing.T, raw []byte) string {
	t.Helper()
	var out map[string]any
	decodeJSONBody(t, raw, &out)
	msg, _ := out["error"].(string)
	return msg
}

// findAudit returns the newest entry with the given action.
func findAudit(t *testing.T, rows []*store.AuditEntry, action string) *store.AuditEntry {
	t.Helper()
	for _, e := range rows {
		if e.Action == action {
			return e
		}
	}
	t.Fatalf("no %q entry in the trail: %+v", action, rows)
	return nil
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }
