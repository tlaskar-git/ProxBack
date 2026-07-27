package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"proxback/internal/store"
)

// request sends an authenticated request with an optional JSON body and returns
// the status code together with the decoded object body.
func (ts *testServer) request(t *testing.T, method, path string, body any) (int, map[string]any) {
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
	req.AddCookie(ts.cookie)
	rec := httptest.NewRecorder()
	ts.ServeHTTP(rec, req)

	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s %s response %q: %v", method, path, rec.Body.String(), err)
		}
	}
	return rec.Code, out
}

// finishedRun creates a run in a terminal state with one log line.
func (ts *testServer) finishedRun(t *testing.T, jobID, status string) string {
	t.Helper()
	ctx := context.Background()
	run, err := ts.st.CreateRun(ctx, &store.JobRun{JobID: jobID, JobName: "nightly"})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := ts.st.AppendRunLog(ctx, run.ID, "run started"); err != nil {
		t.Fatalf("append log: %v", err)
	}
	if err := ts.st.FinishRun(ctx, run.ID, status, 1, 1, 0, ""); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	return run.ID
}

func TestRunLogEndpoint(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()

	run, err := ts.st.CreateRun(ctx, &store.JobRun{JobID: "j1", JobName: "nightly-vms"})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	for _, line := range []string{
		`vm run queued for "nightly-vms"`,
		"web-01: starting 2 disks via Proxmox disk export on node pve1",
		"run succeeded in 1.2s — 32.0 MiB processed, 0 B uploaded, 100% deduplicated",
	} {
		if err := ts.st.AppendRunLog(ctx, run.ID, line); err != nil {
			t.Fatalf("append %q: %v", line, err)
		}
	}

	code, body := ts.request(t, http.MethodGet, "/api/runs/"+run.ID+"/log", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %+v", code, body)
	}
	lines, ok := body["lines"].([]any)
	if !ok || len(lines) != 3 {
		t.Fatalf(`response["lines"] = %#v`, body["lines"])
	}
	first, ok := lines[0].(map[string]any)
	if !ok {
		t.Fatalf("lines[0] = %#v", lines[0])
	}
	if first["line"] != `vm run queued for "nightly-vms"` {
		t.Errorf(`lines[0]["line"] = %v`, first["line"])
	}
	ts64, _ := first["ts"].(string)
	if ts64 == "" || !strings.Contains(ts64, "T") {
		t.Errorf(`lines[0]["ts"] = %q, want an RFC3339 timestamp`, ts64)
	}
	last, _ := lines[2].(map[string]any)
	if s, _ := last["line"].(string); !strings.HasPrefix(s, "run succeeded") {
		t.Errorf(`lines[2]["line"] = %q, want the terminal line last`, s)
	}
}

// A run that logged nothing still answers with an array, never null: the UI
// iterates the field unconditionally.
func TestRunLogEndpointAlwaysReturnsAnArray(t *testing.T) {
	ts := newTestServer(t)
	run, err := ts.st.CreateRun(context.Background(), &store.JobRun{JobID: "j1", JobName: "quiet"})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/runs/"+run.ID+"/log", nil)
	req.AddCookie(ts.cookie)
	rec := httptest.NewRecorder()
	ts.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"lines":[]}` {
		t.Fatalf("body = %s, want an empty lines array", got)
	}
}

func TestRunLogEndpointUnknownRun(t *testing.T) {
	ts := newTestServer(t)
	code, body := ts.request(t, http.MethodGet, "/api/runs/does-not-exist/log", nil)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %+v", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "run not found") {
		t.Errorf(`response["error"] = %q`, msg)
	}
}

// TestDeleteRunningRunIsRejected is the guard that keeps history cleanup from
// orphaning a run that is still writing to the target.
func TestDeleteRunningRunIsRejected(t *testing.T) {
	ts := newTestServer(t)
	run, err := ts.st.CreateRun(context.Background(), &store.JobRun{JobID: "j1", JobName: "nightly"})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	code, body := ts.request(t, http.MethodDelete, "/api/runs/"+run.ID, nil)
	if code != http.StatusConflict {
		t.Fatalf("status = %d, body = %+v", code, body)
	}
	if body["error"] != "cannot delete a running run — cancel it first" {
		t.Errorf(`response["error"] = %v`, body["error"])
	}
	if _, err := ts.st.RunByID(context.Background(), run.ID); err != nil {
		t.Fatalf("the running run was deleted anyway: %v", err)
	}
}

func TestDeleteRunKeepsRestorePoints(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	runID := ts.finishedRun(t, "j1", store.RunSuccess)
	backup, err := ts.st.CreateBackup(ctx, &store.Backup{
		JobID: "j1", RunID: runID, SourceKind: store.SourceVM, SourceID: "h1_100",
		SourceName: "web-01", TargetID: "t1", SizeBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("create backup: %v", err)
	}

	code, body := ts.request(t, http.MethodDelete, "/api/runs/"+runID, nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %+v", code, body)
	}
	if body["ok"] != true {
		t.Errorf(`response["ok"] = %v`, body["ok"])
	}
	if _, err := ts.st.RunByID(ctx, runID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("run survived the delete: %v", err)
	}
	if n, err := ts.st.CountRunLog(ctx, runID); err != nil || n != 0 {
		t.Fatalf("run log survived the delete: %d lines (%v)", n, err)
	}
	if _, err := ts.st.BackupByID(ctx, backup.ID); err != nil {
		t.Fatalf("deleting a run removed its restore point: %v", err)
	}

	// Deleting it again is a 404.
	if code, _ = ts.request(t, http.MethodDelete, "/api/runs/"+runID, nil); code != http.StatusNotFound {
		t.Fatalf("deleting a deleted run = %d, want 404", code)
	}
	if code, _ = ts.request(t, http.MethodDelete, "/api/runs/does-not-exist", nil); code != http.StatusNotFound {
		t.Fatalf("deleting an unknown run = %d, want 404", code)
	}
}

func TestClearRuns(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()

	// Bad scopes are refused before anything is touched.
	for _, body := range []any{
		map[string]any{"scope": "everything"},
		map[string]any{},
		map[string]any{"scope": ""},
	} {
		code, got := ts.request(t, http.MethodPost, "/api/runs/clear", body)
		if code != http.StatusBadRequest {
			t.Fatalf("clear with body %+v = %d (%+v), want 400", body, code, got)
		}
		if msg, _ := got["error"].(string); !strings.Contains(msg, "scope") {
			t.Errorf(`response["error"] = %q`, msg)
		}
	}

	success := ts.finishedRun(t, "j1", store.RunSuccess)
	failed := ts.finishedRun(t, "j1", store.RunFailed)
	canceled := ts.finishedRun(t, "j1", store.RunCanceled)
	otherJob := ts.finishedRun(t, "j2", store.RunFailed)
	running, err := ts.st.CreateRun(ctx, &store.JobRun{JobID: "j1", JobName: "nightly"})
	if err != nil {
		t.Fatalf("create running run: %v", err)
	}

	// scope=failed, scoped to one job.
	code, body := ts.request(t, http.MethodPost, "/api/runs/clear",
		map[string]any{"scope": "failed", "jobId": "j1"})
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %+v", code, body)
	}
	if n, _ := body["deleted"].(float64); n != 1 {
		t.Fatalf(`response["deleted"] = %v, want 1`, body["deleted"])
	}
	if _, err := ts.st.RunByID(ctx, failed); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("j1's failed run survived: %v", err)
	}
	if _, err := ts.st.RunByID(ctx, otherJob); err != nil {
		t.Fatalf("j2's failed run was cleared by a j1-scoped request: %v", err)
	}

	// scope=finished across every job removes success + failed + canceled and
	// leaves the run in progress alone.
	code, body = ts.request(t, http.MethodPost, "/api/runs/clear", map[string]any{"scope": "finished"})
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %+v", code, body)
	}
	if n, _ := body["deleted"].(float64); n != 3 {
		t.Fatalf(`response["deleted"] = %v, want 3`, body["deleted"])
	}
	for _, id := range []string{success, canceled, otherJob} {
		if _, err := ts.st.RunByID(ctx, id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("terminal run %s survived the clear: %v", id, err)
		}
	}
	if _, err := ts.st.RunByID(ctx, running.ID); err != nil {
		t.Fatalf("the running run was cleared: %v", err)
	}

	// Nothing terminal is left, so a second clear reports zero.
	code, body = ts.request(t, http.MethodPost, "/api/runs/clear", map[string]any{"scope": "finished"})
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %+v", code, body)
	}
	if n, _ := body["deleted"].(float64); n != 0 {
		t.Fatalf(`second clear deleted %v runs, want 0`, body["deleted"])
	}
}

func TestRunHistoryEndpointsRequireASession(t *testing.T) {
	ts := newTestServer(t)
	run, err := ts.st.CreateRun(context.Background(), &store.JobRun{JobID: "j1", JobName: "nightly"})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/runs/" + run.ID + "/log"},
		{http.MethodDelete, "/api/runs/" + run.ID},
		{http.MethodPost, "/api/runs/clear"},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(`{"scope":"finished"}`))
		rec := httptest.NewRecorder()
		ts.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a session = %d, want 401", c.method, c.path, rec.Code)
		}
	}
}
