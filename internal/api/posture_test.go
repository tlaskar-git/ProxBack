package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"proxback/internal/store"
)

func hoursAgo(now time.Time, h float64) *time.Time {
	t := now.Add(-time.Duration(h * float64(time.Hour)))
	return &t
}

func dailyJob(name string) *store.Job {
	return &store.Job{
		Name: name, Kind: store.SourceVM, Enabled: true,
		Schedule: store.Schedule{Kind: store.ScheduleDaily, Time: "02:00"},
	}
}

// TestPostureMatrix is the whole verdict logic in one table. The rules it pins
// down are the ones the old dashboard got wrong: a workload is judged by its
// own history, and an estate with nothing in it is "unknown" rather than
// perfect.
func TestPostureMatrix(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	manual := &store.Job{Name: "on-demand", Kind: store.SourceVM, Enabled: true, Schedule: store.ManualSchedule()}

	for _, c := range []struct {
		what        string
		workloads   []postureWorkload
		wantVerdict string
		wantCounts  postureCountsDTO
		wantStatus  []string
		wantReasons []string
	}{
		{
			what:        "an empty estate says nothing",
			workloads:   nil,
			wantVerdict: PostureUnknown,
			wantCounts:  postureCountsDTO{},
		},
		{
			what: "a fresh daily backup is protected",
			workloads: []postureWorkload{{
				Kind: store.SourceVM, ID: "h1_100", Name: "web-01", Job: dailyJob("nightly"),
				LastSuccessAt: hoursAgo(now, 3), RestorePoints: 4,
			}},
			wantVerdict: PostureProtected,
			wantCounts:  postureCountsDTO{Protected: 1},
			wantStatus:  []string{PostureProtected},
		},
		{
			what: "a guest in no job is unprotected",
			workloads: []postureWorkload{{
				Kind: store.SourceVM, ID: "h1_101", Name: "db-01",
			}},
			wantVerdict: PostureUnprotected,
			wantCounts:  postureCountsDTO{Unprotected: 1},
			wantStatus:  []string{PostureUnprotected},
			wantReasons: []string{ReasonNoJob},
		},
		{
			what: "a daily backup one day plus the grace window old is still protected",
			workloads: []postureWorkload{{
				Kind: store.SourceVM, ID: "h1_100", Name: "web-01", Job: dailyJob("nightly"),
				// 24h RPO + 6h grace: 29 hours has not lapsed yet.
				LastSuccessAt: hoursAgo(now, 29), RestorePoints: 2,
			}},
			wantVerdict: PostureProtected,
			wantCounts:  postureCountsDTO{Protected: 1},
			wantStatus:  []string{PostureProtected},
		},
		{
			what: "a daily backup past the grace window is at risk",
			workloads: []postureWorkload{{
				Kind: store.SourceVM, ID: "h1_100", Name: "web-01", Job: dailyJob("nightly"),
				LastSuccessAt: hoursAgo(now, 31), RestorePoints: 2,
			}},
			wantVerdict: PostureAtRisk,
			wantCounts:  postureCountsDTO{AtRisk: 1},
			wantStatus:  []string{PostureAtRisk},
			wantReasons: []string{ReasonRPOExceeded},
		},
		{
			what: "a manual job promises no RPO, so age alone never condemns it",
			workloads: []postureWorkload{{
				Kind: store.SourceVM, ID: "h1_100", Name: "web-01", Job: manual,
				LastSuccessAt: hoursAgo(now, 2000), RestorePoints: 1,
			}},
			wantVerdict: PostureProtected,
			wantCounts:  postureCountsDTO{Protected: 1},
			wantStatus:  []string{PostureProtected},
		},
		{
			what: "a job with no restore point yet is at risk",
			workloads: []postureWorkload{{
				Kind: store.SourceVM, ID: "h1_102", Name: "app-01", Job: dailyJob("nightly"),
			}},
			wantVerdict: PostureAtRisk,
			wantCounts:  postureCountsDTO{AtRisk: 1},
			wantStatus:  []string{PostureAtRisk},
			wantReasons: []string{ReasonNoRestorePoints},
		},
		{
			what: "one workload's failure is not covered up by another's success",
			workloads: []postureWorkload{
				{
					Kind: store.SourceVM, ID: "h1_100", Name: "web-01", Job: dailyJob("nightly"),
					LastSuccessAt: hoursAgo(now, 1), RestorePoints: 3,
				},
				{
					Kind: store.SourceVM, ID: "h1_101", Name: "db-01", Job: dailyJob("nightly"),
					// It has restore points and a recent one, but its own most
					// recent attempt failed.
					LastSuccessAt: hoursAgo(now, 1), RestorePoints: 3,
					LastOutcomeFailed: true, LastFailureAt: hoursAgo(now, 0.5),
				},
			},
			wantVerdict: PostureAtRisk,
			wantCounts:  postureCountsDTO{Protected: 1, AtRisk: 1},
			wantStatus:  []string{PostureProtected, PostureAtRisk},
			wantReasons: []string{ReasonLastRunFailed},
		},
		{
			what: "unprotected outranks at risk in the roll-up",
			workloads: []postureWorkload{
				{Kind: store.SourceVM, ID: "h1_100", Name: "web-01", Job: dailyJob("nightly")},
				{Kind: store.SourceVM, ID: "h1_101", Name: "db-01"},
			},
			wantVerdict: PostureUnprotected,
			wantCounts:  postureCountsDTO{AtRisk: 1, Unprotected: 1},
			wantStatus:  []string{PostureAtRisk, PostureUnprotected},
			wantReasons: []string{ReasonNoJob, ReasonNoRestorePoints},
		},
	} {
		t.Run(c.what, func(t *testing.T) {
			got := evaluatePosture(c.workloads, now)
			if got.Verdict != c.wantVerdict {
				t.Errorf("verdict = %q, want %q", got.Verdict, c.wantVerdict)
			}
			if got.Counts != c.wantCounts {
				t.Errorf("counts = %+v, want %+v", got.Counts, c.wantCounts)
			}
			if len(got.Workloads) != len(c.wantStatus) {
				t.Fatalf("workloads = %d, want %d", len(got.Workloads), len(c.wantStatus))
			}
			for i, want := range c.wantStatus {
				if got.Workloads[i].Status != want {
					t.Errorf("workload %s status = %q, want %q",
						got.Workloads[i].ID, got.Workloads[i].Status, want)
				}
			}
			codes := make([]string, 0, len(got.Reasons))
			for _, r := range got.Reasons {
				codes = append(codes, r.Code)
				if r.Workloads < 1 || r.Detail == "" {
					t.Errorf("reason %+v does not explain itself", r)
				}
			}
			if len(codes) != len(c.wantReasons) {
				t.Fatalf("reasons = %v, want %v", codes, c.wantReasons)
			}
			for i, want := range c.wantReasons {
				if codes[i] != want {
					t.Errorf("reason %d = %q, want %q", i, codes[i], want)
				}
			}
			// Every workload the API reports carries the numbers the UI shows.
			for _, w := range got.Workloads {
				if w.Status != PostureUnprotected && w.Policy == "" {
					t.Errorf("workload %s is %s but names no policy", w.ID, w.Status)
				}
			}
		})
	}
}

// A workload's staleness is measured against the job's own schedule, not
// against a fixed window.
func TestPostureRPOComesFromTheSchedule(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	hourly := &store.Job{
		Name: "hourly", Kind: store.SourceVM, Enabled: true,
		Schedule: store.Schedule{Kind: store.ScheduleHourly, Minute: 0},
	}
	weekly := &store.Job{
		Name: "weekly", Kind: store.SourceVM, Enabled: true,
		Schedule: store.Schedule{Kind: store.ScheduleWeekly, Time: "03:00", Weekdays: []int{0}},
	}
	// Three hours old: far past an hourly job's objective, comfortably inside a
	// weekly one's.
	got := evaluatePosture([]postureWorkload{
		{Kind: store.SourceVM, ID: "a", Name: "a", Job: hourly, LastSuccessAt: hoursAgo(now, 3), RestorePoints: 1},
		{Kind: store.SourceVM, ID: "b", Name: "b", Job: weekly, LastSuccessAt: hoursAgo(now, 3), RestorePoints: 1},
	}, now)
	if got.Workloads[0].Status != PostureAtRisk {
		t.Errorf("an hourly workload 3h stale is %q", got.Workloads[0].Status)
	}
	if got.Workloads[1].Status != PostureProtected {
		t.Errorf("a weekly workload 3h stale is %q", got.Workloads[1].Status)
	}
	if got.Workloads[0].RPOHours == nil || *got.Workloads[0].RPOHours != 1 {
		t.Errorf("hourly rpoHours = %v, want 1", got.Workloads[0].RPOHours)
	}
	if got.Workloads[1].RPOHours == nil || *got.Workloads[1].RPOHours != 168 {
		t.Errorf("weekly rpoHours = %v, want 168", got.Workloads[1].RPOHours)
	}
	if got.Workloads[0].WithinRPO == nil || *got.Workloads[0].WithinRPO {
		t.Error("the stale hourly workload claims to be within its RPO")
	}
}

// Membership by tag filter counts: a guest picked up dynamically is protected
// by that job, and a disabled job protects nothing.
func TestPostureJobMembership(t *testing.T) {
	tagged := &store.Job{
		Name: "prod-tagged", Kind: store.SourceVM, Enabled: true, TagFilter: "prod",
		Schedule: store.Schedule{Kind: store.ScheduleDaily, Time: "02:00"},
	}
	static := &store.Job{
		Name: "static", Kind: store.SourceVM, Enabled: true,
		Schedule: store.Schedule{Kind: store.ScheduleDaily, Time: "02:00"},
		Sources:  store.JobSources{{HostID: "h1", VMID: 101}},
	}
	disabled := &store.Job{
		Name: "off", Kind: store.SourceVM, Enabled: false,
		Schedule: store.Schedule{Kind: store.ScheduleDaily, Time: "02:00"},
		Sources:  store.JobSources{{HostID: "h1", VMID: 102}},
	}
	jobs := []*store.Job{tagged, static, disabled}

	prod := store.VM{VMID: 100, HostID: "h1", Name: "web-01", Tags: []string{"prod", "web"}}
	if got := governingJob(jobs, func(j *store.Job) bool { return jobCoversVM(j, prod) }); got != tagged {
		t.Errorf("tagged guest governed by %v, want the tag-filter job", got)
	}
	listed := store.VM{VMID: 101, HostID: "h1", Name: "db-01"}
	if got := governingJob(jobs, func(j *store.Job) bool { return jobCoversVM(j, listed) }); got != static {
		t.Errorf("listed guest governed by %v, want the static job", got)
	}
	// A guest only named by a disabled job is in no job at all.
	off := store.VM{VMID: 102, HostID: "h1", Name: "app-01"}
	if got := governingJob(jobs, func(j *store.Job) bool { return jobCoversVM(j, off) }); got != nil {
		t.Errorf("a disabled job governs %v", got)
	}
	// Identically numbered guests in another cluster are not covered by this
	// cluster's job.
	elsewhere := store.VM{VMID: 101, HostID: "h2", Name: "db-01"}
	if got := governingJob(jobs, func(j *store.Job) bool { return jobCoversVM(j, elsewhere) }); got != nil {
		t.Errorf("another cluster's vm 101 is governed by %v", got)
	}

	// When several jobs cover a workload, the tightest objective is the promise
	// it is judged against.
	hourly := &store.Job{
		Name: "hourly", Kind: store.SourceVM, Enabled: true, TagFilter: "prod",
		Schedule: store.Schedule{Kind: store.ScheduleHourly},
	}
	if got := governingJob([]*store.Job{tagged, hourly},
		func(j *store.Job) bool { return jobCoversVM(j, prod) }); got != hourly {
		t.Errorf("governing job = %v, want the hourly one", got)
	}
}

// The endpoint itself: a fresh install has nothing to say, and it says so.
func TestPostureEndpointOnAnEmptyEstate(t *testing.T) {
	ts := newTestServer(t)
	code, body := ts.request(t, http.MethodGet, "/api/posture", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %+v", code, body)
	}
	if body["verdict"] != PostureUnknown {
		t.Errorf("verdict = %v, want %q", body["verdict"], PostureUnknown)
	}
	counts, _ := body["counts"].(map[string]any)
	for _, key := range []string{"protected", "atRisk", "unprotected"} {
		if v, ok := counts[key].(float64); !ok || v != 0 {
			t.Errorf("counts[%q] = %v, want 0", key, counts[key])
		}
	}
	if _, ok := body["workloads"].([]any); !ok {
		t.Errorf("workloads = %#v, want an array", body["workloads"])
	}
	if _, ok := body["reasons"].([]any); !ok {
		t.Errorf("reasons = %#v, want an array", body["reasons"])
	}
}

// TestPostureEndpointReadsTheEstate walks the whole path: cached inventory,
// jobs, restore points and per-workload run outcomes.
func TestPostureEndpointReadsTheEstate(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()

	if err := ts.st.ReplaceVMCache(ctx, ts.hostID, []store.VM{
		{VMID: 100, Name: "web-01", Node: "pve1", Status: "running",
			HostID: ts.hostID, HostName: "cluster-a", Tags: []string{"prod"}},
		{VMID: 101, Name: "db-01", Node: "pve1", Status: "running",
			HostID: ts.hostID, HostName: "cluster-a"},
	}); err != nil {
		t.Fatalf("cache vms: %v", err)
	}
	job, err := ts.st.CreateJob(ctx, &store.Job{
		Name: "nightly", Kind: store.SourceVM, TargetID: "t1", Enabled: true,
		Schedule: store.Schedule{Kind: store.ScheduleDaily, Time: "02:00"},
		Sources:  store.JobSources{{HostID: ts.hostID, VMID: 100}},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	sourceID := ts.hostID + "_100"
	verified := store.Now()
	backup, err := ts.st.CreateBackup(ctx, &store.Backup{
		JobID: job.ID, SourceKind: store.SourceVM, SourceID: sourceID, SourceName: "web-01",
		HostID: ts.hostID, HostName: "cluster-a", TargetID: "t1", SizeBytes: 4096,
	})
	if err != nil {
		t.Fatalf("create backup: %v", err)
	}
	if err := ts.st.RecordBackupVerification(ctx, backup.ID, verified, store.VerifyPassed, 4096); err != nil {
		t.Fatalf("record verification: %v", err)
	}

	code, raw := ts.getRaw(t, "/api/posture")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", code, raw)
	}
	var got postureDTO
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode posture %s: %v", raw, err)
	}
	// One guest is in the job, the other is in none — so the estate is
	// unprotected however healthy the first one looks.
	if got.Verdict != PostureUnprotected {
		t.Fatalf("verdict = %q, want %q (db-01 is in no job)", got.Verdict, PostureUnprotected)
	}
	if got.Counts != (postureCountsDTO{Protected: 1, Unprotected: 1}) {
		t.Fatalf("counts = %+v", got.Counts)
	}
	byID := map[string]postureWorkloadDTO{}
	for _, w := range got.Workloads {
		byID[w.ID] = w
	}
	web := byID[sourceID]
	if web.Status != PostureProtected || web.Policy != "nightly" || !web.Enabled {
		t.Fatalf("web-01 posture = %+v", web)
	}
	if web.HostName != "cluster-a" || web.Node != "pve1" || web.Name != "web-01" {
		t.Fatalf("web-01 is not identified as cluster/name/node: %+v", web)
	}
	if web.RestorePoints != 1 || web.LastVerifiedAt == nil {
		t.Fatalf("web-01 evidence = %+v", web)
	}
	if web.RPOHours == nil || *web.RPOHours != 24 {
		t.Fatalf("web-01 rpoHours = %v, want 24", web.RPOHours)
	}
	db := byID[ts.hostID+"_101"]
	if db.Status != PostureUnprotected || db.Policy != "" {
		t.Fatalf("db-01 posture = %+v", db)
	}
}

// getRaw performs an authenticated GET and returns the undecoded body, for the
// endpoints whose shape is a struct rather than a map.
func (ts *testServer) getRaw(t *testing.T, path string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(ts.cookie)
	rec := httptest.NewRecorder()
	ts.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// decodeJSONBody unmarshals a response body into a typed value.
func decodeJSONBody(t *testing.T, raw []byte, out any) {
	t.Helper()
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}
