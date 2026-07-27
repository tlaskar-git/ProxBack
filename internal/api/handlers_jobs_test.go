package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"proxback/internal/store"
)

// target creates a storage target a job can point at. No run in these tests
// reaches it; jobs simply refuse to exist without one.
func (ts *testServer) target(t *testing.T) *store.S3Target {
	t.Helper()
	tgt, err := ts.st.CreateS3Target(context.Background(), &store.S3Target{
		Name: "vm-storage", Endpoint: "http://127.0.0.1:1", Region: "us-east-1",
		Bucket: "proxback", AccessKey: "k", SecretKey: "s", PathStyle: true,
	})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	return tgt
}

// jobBody is a valid vm job creation body with the given schedule.
func jobBody(targetID string, schedule any) map[string]any {
	return map[string]any{
		"name": "nightly-vms", "kind": "vm", "targetId": targetID,
		"schedule": schedule, "retention": 2, "enabled": true,
		"sources": []map[string]any{{"hostId": "h1", "vmid": 100, "name": "web-01"}},
	}
}

// TestJobScheduleObjectRoundTrip is the contract the schedule picker is built
// against: the object goes in, the object plus its rendered label comes back,
// and a schedule that fires produces a nextRun.
func TestJobScheduleObjectRoundTrip(t *testing.T) {
	ts := newTestServer(t)
	tgt := ts.target(t)

	for _, c := range []struct {
		schedule map[string]any
		label    string
		fires    bool
	}{
		{map[string]any{"kind": "manual"}, "Manual", false},
		{map[string]any{"kind": "hourly", "minute": 30}, "Every hour at :30", true},
		{map[string]any{"kind": "daily", "time": "02:00"}, "Daily at 02:00", true},
		{map[string]any{"kind": "weekly", "time": "03:00", "weekdays": []int{0, 6}},
			"Weekly on Sun, Sat at 03:00", true},
		{map[string]any{"kind": "monthly", "time": "01:00", "dayOfMonth": 1},
			"Monthly on day 1 at 01:00", true},
		{map[string]any{"kind": "advanced", "cron": "*/15 * * * *"}, "Custom (*/15 * * * *)", true},
	} {
		code, body := ts.request(t, http.MethodPost, "/api/jobs", jobBody(tgt.ID, c.schedule))
		if code != http.StatusOK {
			t.Fatalf("POST with %v = %d (%v)", c.schedule, code, body)
		}
		got, ok := body["schedule"].(map[string]any)
		if !ok {
			t.Fatalf(`response["schedule"] = %#v, want an object`, body["schedule"])
		}
		for field, want := range c.schedule {
			if !equalJSON(got[field], want) {
				t.Errorf("schedule[%q] = %#v, want %#v", field, got[field], want)
			}
		}
		if len(got) != len(c.schedule) {
			t.Errorf("schedule = %#v, want exactly the fields of a %s schedule", got, c.schedule["kind"])
		}
		if body["scheduleLabel"] != c.label {
			t.Errorf(`response["scheduleLabel"] = %v, want %q`, body["scheduleLabel"], c.label)
		}
		next, _ := body["nextRun"].(string)
		if c.fires {
			if next == "" {
				t.Errorf("a %s schedule reports no nextRun", c.schedule["kind"])
				continue
			}
			at, err := time.Parse(time.RFC3339, next)
			if err != nil {
				t.Errorf("nextRun %q is not RFC3339: %v", next, err)
			} else if !at.After(time.Now()) {
				t.Errorf("nextRun %s is not in the future", at)
			}
		} else if body["nextRun"] != nil {
			t.Errorf("a manual schedule reports nextRun %v, want null", body["nextRun"])
		}
		ts.request(t, http.MethodDelete, "/api/jobs/"+body["id"].(string), nil)
	}
}

// equalJSON compares a decoded JSON value with the literal it was sent as,
// bridging the float64/int and []any/[]int differences of a round trip.
func equalJSON(got, want any) bool {
	a, err := json.Marshal(got)
	if err != nil {
		return false
	}
	b, err := json.Marshal(want)
	if err != nil {
		return false
	}
	return string(a) == string(b)
}

// TestJobScheduleAcceptsBareStrings keeps existing automation working: the cron
// string earlier releases took is still accepted and becomes the preset it
// expresses, or an advanced schedule when it expresses none.
func TestJobScheduleAcceptsBareStrings(t *testing.T) {
	ts := newTestServer(t)
	tgt := ts.target(t)

	for _, c := range []struct {
		schedule string
		kind     string
		label    string
	}{
		{"manual", "manual", "Manual"},
		{"", "manual", "Manual"},
		{"0 2 * * *", "daily", "Daily at 02:00"},
		{"*/15 * * * *", "advanced", "Custom (*/15 * * * *)"},
	} {
		code, body := ts.request(t, http.MethodPost, "/api/jobs", jobBody(tgt.ID, c.schedule))
		if code != http.StatusOK {
			t.Fatalf("POST with schedule %q = %d (%v)", c.schedule, code, body)
		}
		got, _ := body["schedule"].(map[string]any)
		if got["kind"] != c.kind || body["scheduleLabel"] != c.label {
			t.Errorf("schedule %q became %#v labelled %v, want kind %q labelled %q",
				c.schedule, got, body["scheduleLabel"], c.kind, c.label)
		}
		id, _ := body["id"].(string)

		// PATCH takes both forms too.
		code, body = ts.request(t, http.MethodPatch, "/api/jobs/"+id,
			map[string]any{"schedule": map[string]any{"kind": "weekly", "time": "23:15", "weekdays": []int{5}}})
		if code != http.StatusOK {
			t.Fatalf("PATCH = %d (%v)", code, body)
		}
		if body["scheduleLabel"] != "Weekly on Fri at 23:15" {
			t.Errorf(`patched scheduleLabel = %v`, body["scheduleLabel"])
		}
		ts.request(t, http.MethodDelete, "/api/jobs/"+id, nil)
	}
}

// TestJobScheduleValidation keeps a schedule the scheduler could not honour out
// of the database, with a 400 the UI can put in front of the operator.
func TestJobScheduleValidation(t *testing.T) {
	ts := newTestServer(t)
	tgt := ts.target(t)

	for _, bad := range []any{
		map[string]any{"kind": "yearly"},
		map[string]any{"kind": "hourly", "minute": 60},
		map[string]any{"kind": "hourly", "minute": -1},
		map[string]any{"kind": "daily", "time": "25:00"},
		map[string]any{"kind": "daily", "time": "2:00"},
		map[string]any{"kind": "daily"},
		map[string]any{"kind": "weekly", "time": "03:00"},
		map[string]any{"kind": "weekly", "time": "03:00", "weekdays": []int{7}},
		map[string]any{"kind": "monthly", "time": "01:00", "dayOfMonth": 0},
		map[string]any{"kind": "monthly", "time": "01:00", "dayOfMonth": 32},
		map[string]any{"kind": "advanced"},
		map[string]any{"kind": "advanced", "cron": "not a cron"},
		"not a cron",
	} {
		code, body := ts.request(t, http.MethodPost, "/api/jobs", jobBody(tgt.ID, bad))
		if code != http.StatusBadRequest {
			t.Fatalf("POST with schedule %v = %d (%v), want 400", bad, code, body)
		}
		if msg, _ := body["error"].(string); msg == "" {
			t.Errorf("POST with schedule %v returned 400 without a message", bad)
		}
	}

	// Nothing was created by any of them.
	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	req.AddCookie(ts.cookie)
	rec := httptest.NewRecorder()
	ts.ServeHTTP(rec, req)
	var jobs []any
	if err := json.Unmarshal(rec.Body.Bytes(), &jobs); err != nil {
		t.Fatalf("decode jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("a rejected schedule created %d jobs", len(jobs))
	}
}

// ---------------------------------------------------------------- run detail

// TestRunDetailCarriesSources covers the shape the visual monitor polls: the run
// itself plus one entry per object, always an array, with a throughput figure.
func TestRunDetailCarriesSources(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()

	run, err := ts.st.CreateRun(ctx, &store.JobRun{JobID: "j1", JobName: "nightly-vms"})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := ts.st.ReplaceRunSources(ctx, run.ID, []store.RunSource{
		{Seq: 0, Name: "web-01", Kind: store.SourceVM, Node: "pve1", SizeBytes: 32 << 20},
		{Seq: 1, Name: "db-01", Kind: store.SourceVM, Node: "pve2", SizeBytes: 16 << 20},
	}); err != nil {
		t.Fatalf("plan run sources: %v", err)
	}
	if err := ts.st.StartRunSource(ctx, run.ID, 0); err != nil {
		t.Fatalf("start source: %v", err)
	}
	if err := ts.st.UpdateRunSourceProgress(ctx, run.ID, 0, 16<<20, 8<<20); err != nil {
		t.Fatalf("update source progress: %v", err)
	}

	code, body := ts.request(t, http.MethodGet, "/api/runs/"+run.ID, nil)
	if code != http.StatusOK {
		t.Fatalf("GET run = %d (%v)", code, body)
	}
	if body["id"] != run.ID || body["jobName"] != "nightly-vms" {
		t.Fatalf("run detail lost the run's own fields: %v", body)
	}
	if _, ok := body["throughputBps"]; !ok {
		t.Error("run detail carries no throughputBps")
	}
	if bps, _ := body["throughputBps"].(float64); bps != 0 {
		t.Errorf("throughputBps = %v for a run this server is not executing, want 0", bps)
	}
	sources, ok := body["sources"].([]any)
	if !ok || len(sources) != 2 {
		t.Fatalf(`response["sources"] = %#v, want two entries`, body["sources"])
	}
	first, _ := sources[0].(map[string]any)
	for field, want := range map[string]any{
		"seq": float64(0), "name": "web-01", "kind": "vm", "node": "pve1",
		"status": store.SourceRunning, "sizeBytes": float64(32 << 20),
		"bytesProcessed": float64(16 << 20), "bytesUploaded": float64(8 << 20),
		"progressPct": float64(50),
	} {
		if first[field] != want {
			t.Errorf("sources[0][%q] = %#v, want %#v", field, first[field], want)
		}
	}
	if s, _ := first["startedAt"].(string); s == "" {
		t.Error("the running source has no startedAt")
	}
	second, _ := sources[1].(map[string]any)
	if second["status"] != store.SourcePending || second["progressPct"] != float64(0) {
		t.Errorf("sources[1] = %#v, want a pending entry", second)
	}
	if _, present := second["startedAt"]; present {
		t.Errorf("a pending source reports startedAt: %#v", second)
	}

	// A run with no sources still answers with an array, never null.
	bare, err := ts.st.CreateRun(ctx, &store.JobRun{JobName: "Restore web-01", Kind: store.RunKindRestore})
	if err != nil {
		t.Fatalf("create restore run: %v", err)
	}
	_, body = ts.request(t, http.MethodGet, "/api/runs/"+bare.ID, nil)
	if got, ok := body["sources"].([]any); !ok || len(got) != 0 {
		t.Fatalf(`restore run["sources"] = %#v, want an empty array`, body["sources"])
	}

	// The list stays cheap: no per-source breakdown there.
	req := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
	req.AddCookie(ts.cookie)
	rec := httptest.NewRecorder()
	ts.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), `"sources"`) {
		t.Errorf("GET /api/runs carries the per-source breakdown: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------- retry

func TestRetryRun(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	// The retry really starts the job, so the run goroutine has to unwind before
	// the store closes.
	t.Cleanup(ts.sched.Stop)

	tgt := ts.target(t)
	code, body := ts.request(t, http.MethodPost, "/api/jobs", jobBody(tgt.ID, "manual"))
	if code != http.StatusOK {
		t.Fatalf("create job = %d (%v)", code, body)
	}
	jobID, _ := body["id"].(string)

	// A run that is over, of a job that is idle: retrying starts a new run.
	first := ts.finishedRun(t, jobID, store.RunFailed)
	code, body = ts.request(t, http.MethodPost, "/api/runs/"+first+"/retry", nil)
	if code != http.StatusOK {
		t.Fatalf("retry = %d (%v)", code, body)
	}
	retryID, _ := body["runId"].(string)
	if retryID == "" || retryID == first {
		t.Fatalf(`response["runId"] = %v, want a new run`, body["runId"])
	}
	retried, err := ts.st.RunByID(ctx, retryID)
	if err != nil {
		t.Fatalf("load the retry run: %v", err)
	}
	if retried.JobID != jobID {
		t.Errorf("the retry belongs to job %q, want %q", retried.JobID, jobID)
	}

	// 409 while that job has a run in flight.
	code, body = ts.request(t, http.MethodPost, "/api/runs/"+first+"/retry", nil)
	if code != http.StatusConflict {
		// The first retry may already have finished; force the state explicitly.
		if _, err := ts.st.CreateRun(ctx, &store.JobRun{JobID: jobID, JobName: "nightly-vms"}); err != nil {
			t.Fatalf("create an in-flight run: %v", err)
		}
		code, body = ts.request(t, http.MethodPost, "/api/runs/"+first+"/retry", nil)
	}
	if code != http.StatusConflict {
		t.Fatalf("retry while the job is running = %d (%v), want 409", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "running") {
		t.Errorf(`response["error"] = %q`, msg)
	}

	// 404 for a run with no job behind it — a restore or a verification.
	restore, err := ts.st.CreateRun(ctx, &store.JobRun{JobName: "Restore web-01", Kind: store.RunKindRestore})
	if err != nil {
		t.Fatalf("create restore run: %v", err)
	}
	if err := ts.st.FinishRun(ctx, restore.ID, store.RunSuccess, 1, 1, 0, ""); err != nil {
		t.Fatalf("finish restore run: %v", err)
	}
	code, body = ts.request(t, http.MethodPost, "/api/runs/"+restore.ID+"/retry", nil)
	if code != http.StatusNotFound {
		t.Fatalf("retry of a restore run = %d (%v), want 404", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "no job") {
		t.Errorf(`response["error"] = %q`, msg)
	}

	// 404 for a run that does not exist at all.
	if code, _ = ts.request(t, http.MethodPost, "/api/runs/does-not-exist/retry", nil); code != http.StatusNotFound {
		t.Fatalf("retry of an unknown run = %d, want 404", code)
	}

	// And it needs a session like every other run endpoint.
	req := httptest.NewRequest(http.MethodPost, "/api/runs/"+first+"/retry", nil)
	rec := httptest.NewRecorder()
	ts.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("retry without a session = %d, want 401", rec.Code)
	}
}
