package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"proxback/internal/store"
)

// TestJobPolicyRoundTrip is the contract the Advanced protection step is built
// against: what the console sends comes back complete, and a job created
// without a policy still reports one, so the client never has to guess which
// release wrote the job.
func TestJobPolicyRoundTrip(t *testing.T) {
	ts := newTestServer(t)
	tgt := ts.target(t)

	body := jobBody(tgt.ID, map[string]any{"kind": "manual"})
	code, created := ts.request(t, http.MethodPost, "/api/jobs", body)
	if code != http.StatusOK {
		t.Fatalf("POST /api/jobs = %d (%v)", code, created)
	}
	policy, ok := created["policy"].(map[string]any)
	if !ok {
		t.Fatalf(`response["policy"] = %#v, want an object`, created["policy"])
	}
	if policy["quiesce"] != "none" || policy["retryDelayMinutes"] != float64(5) ||
		policy["scriptTimeoutSeconds"] != float64(30) || policy["window"] != nil {
		t.Fatalf("default policy = %#v", policy)
	}
	// The lists are always arrays, never null: the console maps over them.
	for _, field := range []string{"excludeDisks", "excludePaths"} {
		if _, ok := policy[field].([]any); !ok {
			t.Fatalf("policy[%q] = %#v, want an array", field, policy[field])
		}
	}

	// A full policy survives a PATCH unchanged.
	full := map[string]any{
		"quiesce": "guest-agent", "excludeDisks": []string{"scsi1"}, "excludePaths": []string{},
		"retryCount": 3, "retryDelayMinutes": 10, "maxDurationMinutes": 120,
		"window":    map[string]any{"start": "22:00", "end": "06:00"},
		"preScript": "/usr/local/bin/freeze.sh", "postScript": "/usr/local/bin/thaw.sh",
		"scriptTimeoutSeconds": 60, "uploadLimitMbpsOverride": 250,
	}
	id, _ := created["id"].(string)
	code, patched := ts.request(t, http.MethodPatch, "/api/jobs/"+id, map[string]any{"policy": full})
	if code != http.StatusOK {
		t.Fatalf("PATCH policy = %d (%v)", code, patched)
	}
	got, _ := patched["policy"].(map[string]any)
	for field, want := range full {
		if !equalJSON(got[field], want) {
			t.Errorf("policy[%q] = %#v, want %#v", field, got[field], want)
		}
	}

	// It is persisted, not merely echoed.
	code, listed := ts.request(t, http.MethodGet, "/api/jobs/"+id+"/retention-preview", nil)
	if code != http.StatusOK {
		t.Fatalf("the job did not survive the patch: %d (%v)", code, listed)
	}
	stored, err := ts.st.JobByID(context.Background(), id)
	if err != nil {
		t.Fatalf("load job: %v", err)
	}
	if stored.Policy.Quiesce != store.QuiesceGuestAgent || stored.Policy.RetryCount != 3 ||
		stored.Policy.Window == nil || stored.Policy.Window.Start != "22:00" ||
		stored.Policy.UploadLimitMbpsOverride != 250 {
		t.Fatalf("stored policy = %+v", stored.Policy)
	}

	// An omitted policy on a later PATCH leaves the stored one alone: a client
	// that only renames a job must not silently reset its protection.
	code, renamed := ts.request(t, http.MethodPatch, "/api/jobs/"+id, map[string]any{"name": "renamed"})
	if code != http.StatusOK {
		t.Fatalf("PATCH name = %d (%v)", code, renamed)
	}
	kept, _ := renamed["policy"].(map[string]any)
	if kept["quiesce"] != "guest-agent" || kept["retryCount"] != float64(3) {
		t.Fatalf("policy after an unrelated patch = %#v", kept)
	}
}

// TestJobPolicyValidationAnswers400 checks that every refusal names the field
// the console has to highlight.
func TestJobPolicyValidationAnswers400(t *testing.T) {
	ts := newTestServer(t)
	tgt := ts.target(t)

	for _, c := range []struct {
		name      string
		kind      string
		policy    map[string]any
		wantField string
	}{
		{"unknown quiesce mode", "vm", map[string]any{"quiesce": "vss"}, "policy.quiesce"},
		{"excluded disks on an agent job", "agent", map[string]any{"excludeDisks": []string{"scsi1"}}, "policy.excludeDisks"},
		{"a disk key that is not one", "vm", map[string]any{"excludeDisks": []string{"/dev/sda"}}, "policy.excludeDisks"},
		{"excluded paths on a vm job", "vm", map[string]any{"excludePaths": []string{"**/tmp"}}, "policy.excludePaths"},
		{"a malformed glob", "agent", map[string]any{"excludePaths": []string{"var/[log"}}, "policy.excludePaths"},
		{"too many retries", "vm", map[string]any{"retryCount": 9}, "policy.retryCount"},
		{"a retry delay beyond two hours", "vm", map[string]any{"retryDelayMinutes": 999}, "policy.retryDelayMinutes"},
		{"a duration limit beyond a week", "vm", map[string]any{"maxDurationMinutes": 20000}, "policy.maxDurationMinutes"},
		{"a window that is not a clock", "vm", map[string]any{"window": map[string]any{"start": "10pm", "end": "06:00"}}, "policy.window.start"},
		{"a window of zero length", "vm", map[string]any{"window": map[string]any{"start": "22:00", "end": "22:00"}}, "policy.window"},
		{"a script timeout beyond an hour", "vm", map[string]any{"scriptTimeoutSeconds": 7200}, "policy.scriptTimeoutSeconds"},
		{"a transfer ceiling beyond the maximum", "vm", map[string]any{"uploadLimitMbpsOverride": 99999}, "policy.uploadLimitMbpsOverride"},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := jobBody(tgt.ID, map[string]any{"kind": "manual"})
			body["policy"] = c.policy
			body["kind"] = c.kind
			if c.kind == "agent" {
				agent, err := ts.st.CreateAgent(context.Background(), &store.Agent{
					Hostname: "files-01", OS: "linux", Arch: "amd64", APIKeyHash: "h",
				})
				if err != nil {
					t.Fatalf("create agent: %v", err)
				}
				body["sources"] = []map[string]any{{"agentId": agent.ID, "paths": []string{"/srv"}}}
			}
			code, out := ts.request(t, http.MethodPost, "/api/jobs", body)
			if code != http.StatusBadRequest {
				t.Fatalf("POST with %v = %d (%v), want 400", c.policy, code, out)
			}
			msg, _ := out["error"].(string)
			if !strings.Contains(msg, c.wantField) {
				t.Fatalf("error = %q, want it to name %s", msg, c.wantField)
			}
		})
	}
}

// TestJobRetentionAcceptsBothForms is the compatibility promise: a bare integer
// still works and reads back as the object that means the same thing.
func TestJobRetentionAcceptsBothForms(t *testing.T) {
	ts := newTestServer(t)
	tgt := ts.target(t)

	body := jobBody(tgt.ID, map[string]any{"kind": "manual"})
	body["retention"] = 5
	code, created := ts.request(t, http.MethodPost, "/api/jobs", body)
	if code != http.StatusOK {
		t.Fatalf("POST with an integer retention = %d (%v)", code, created)
	}
	got, ok := created["retention"].(map[string]any)
	if !ok {
		t.Fatalf(`response["retention"] = %#v, want an object`, created["retention"])
	}
	if got["keepLast"] != float64(5) || got["keepWeekly"] != float64(0) {
		t.Fatalf("retention = %#v, want keepLast 5 and nothing else", got)
	}

	id, _ := created["id"].(string)
	gfs := map[string]any{"keepLast": 7, "keepDaily": 0, "keepWeekly": 4, "keepMonthly": 6, "keepYearly": 1}
	code, patched := ts.request(t, http.MethodPatch, "/api/jobs/"+id, map[string]any{"retention": gfs})
	if code != http.StatusOK {
		t.Fatalf("PATCH with a GFS object = %d (%v)", code, patched)
	}
	for field, want := range gfs {
		if !equalJSON(patched["retention"].(map[string]any)[field], want) {
			t.Errorf("retention[%q] = %#v, want %#v", field, patched["retention"].(map[string]any)[field], want)
		}
	}

	// A negative counter is refused by name.
	code, out := ts.request(t, http.MethodPatch, "/api/jobs/"+id,
		map[string]any{"retention": map[string]any{"keepWeekly": -1}})
	if code != http.StatusBadRequest {
		t.Fatalf("PATCH with a negative counter = %d (%v), want 400", code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "retention.keepWeekly") {
		t.Fatalf("error = %q, want it to name retention.keepWeekly", msg)
	}
}

// TestRetentionPreview covers the endpoint the retention step polls: it answers
// for the policy in the query string when there is one, for the saved policy
// when there is not, and it never changes anything.
func TestRetentionPreview(t *testing.T) {
	ts := newTestServer(t)
	tgt := ts.target(t)
	ctx := context.Background()

	job, err := ts.st.CreateJob(ctx, &store.Job{
		Name: "nightly", Kind: store.SourceVM, TargetID: tgt.ID,
		Schedule: store.ManualSchedule(), Retention: store.KeepLast(2), Enabled: true,
		Sources: store.JobSources{{HostID: ts.hostID, VMID: 100, Name: "web-01"}},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	// Five daily restore points, newest last.
	base := time.Date(2026, time.March, 1, 3, 0, 0, 0, time.UTC)
	var created []string
	for i := 0; i < 5; i++ {
		b, err := ts.st.CreateBackup(ctx, &store.Backup{
			JobID: job.ID, SourceKind: store.SourceVM, SourceID: ts.hostID + "_100",
			SourceName: "web-01", TargetID: tgt.ID, CreatedAt: base.AddDate(0, 0, i),
			SizeBytes: 1, UploadedBytes: 1,
		})
		if err != nil {
			t.Fatalf("create backup %d: %v", i, err)
		}
		created = append(created, b.ID)
	}

	// The saved policy (keep last 2) keeps the two newest.
	code, out := ts.request(t, http.MethodGet, "/api/jobs/"+job.ID+"/retention-preview", nil)
	if code != http.StatusOK {
		t.Fatalf("GET retention-preview = %d (%v)", code, out)
	}
	keeps, prunes := previewLists(t, out)
	if len(keeps) != 2 || len(prunes) != 3 {
		t.Fatalf("saved policy preview = %d keeps / %d prunes, want 2/3", len(keeps), len(prunes))
	}
	if keeps[0]["backupId"] != created[4] || keeps[1]["backupId"] != created[3] {
		t.Fatalf("keeps = %v, want the two newest points newest first", keeps)
	}
	reasons, _ := keeps[0]["reasons"].([]any)
	if len(reasons) != 1 || reasons[0] != "last" {
		t.Fatalf("reasons = %#v, want [last]", keeps[0]["reasons"])
	}
	// A pruned point carries an empty reasons array, never null.
	if r, ok := prunes[0]["reasons"].([]any); !ok || len(r) != 0 {
		t.Fatalf("pruned reasons = %#v, want an empty array", prunes[0]["reasons"])
	}

	// A candidate policy in the query wins over the saved one, so an unsaved
	// edit can be previewed.
	code, out = ts.request(t, http.MethodGet,
		"/api/jobs/"+job.ID+"/retention-preview?keepLast=1&keepDaily=3&keepWeekly=0&keepMonthly=0&keepYearly=0", nil)
	if code != http.StatusOK {
		t.Fatalf("GET retention-preview with a candidate = %d (%v)", code, out)
	}
	keeps, prunes = previewLists(t, out)
	if len(keeps) != 3 || len(prunes) != 2 {
		t.Fatalf("candidate preview = %d keeps / %d prunes, want 3/2", len(keeps), len(prunes))
	}
	// The newest point is kept by both rules and says so.
	reasons, _ = keeps[0]["reasons"].([]any)
	if len(reasons) != 2 || reasons[0] != "last" || reasons[1] != "daily" {
		t.Fatalf("reasons of the newest point = %#v, want [last daily]", keeps[0]["reasons"])
	}

	// Nothing was deleted and nothing was saved: a preview is a question.
	backups, err := ts.st.ListBackups(ctx, store.BackupFilter{JobID: job.ID})
	if err != nil || len(backups) != 5 {
		t.Fatalf("preview changed the restore points: %d (%v)", len(backups), err)
	}
	stored, err := ts.st.JobByID(ctx, job.ID)
	if err != nil || stored.Retention != store.KeepLast(2) {
		t.Fatalf("preview changed the saved policy: %+v (%v)", stored.Retention, err)
	}

	// A policy that keeps nothing is answered honestly rather than softened —
	// the console shows the warning that recovery would be impossible.
	code, out = ts.request(t, http.MethodGet, "/api/jobs/"+job.ID+"/retention-preview?keepLast=0", nil)
	if code != http.StatusOK {
		t.Fatalf("GET retention-preview with an empty policy = %d (%v)", code, out)
	}
	keeps, prunes = previewLists(t, out)
	if len(keeps) != 0 || len(prunes) != 5 {
		t.Fatalf("empty policy preview = %d keeps / %d prunes, want 0/5", len(keeps), len(prunes))
	}
}

func TestRetentionPreviewRejectsNonsense(t *testing.T) {
	ts := newTestServer(t)
	tgt := ts.target(t)
	job, err := ts.st.CreateJob(context.Background(), &store.Job{
		Name: "nightly", Kind: store.SourceVM, TargetID: tgt.ID,
		Schedule: store.ManualSchedule(), Retention: store.KeepLast(2), Enabled: true,
		Sources: store.JobSources{{HostID: ts.hostID, VMID: 100}},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	for _, c := range []struct{ query, want string }{
		{"?keepLast=many", "keepLast"},
		{"?keepWeekly=-2", "retention.keepWeekly"},
	} {
		code, out := ts.request(t, http.MethodGet, "/api/jobs/"+job.ID+"/retention-preview"+c.query, nil)
		if code != http.StatusBadRequest {
			t.Fatalf("preview %s = %d (%v), want 400", c.query, code, out)
		}
		if msg, _ := out["error"].(string); !strings.Contains(msg, c.want) {
			t.Fatalf("preview %s error = %q, want it to name %s", c.query, msg, c.want)
		}
	}
	code, out := ts.request(t, http.MethodGet, "/api/jobs/does-not-exist/retention-preview", nil)
	if code != http.StatusNotFound {
		t.Fatalf("preview of a missing job = %d (%v), want 404", code, out)
	}
}

// TestRetentionPreviewIsPerWorkload: one guest's history never decides
// another's fate, so a job covering two guests keeps N points for each.
func TestRetentionPreviewIsPerWorkload(t *testing.T) {
	ts := newTestServer(t)
	tgt := ts.target(t)
	ctx := context.Background()

	job, err := ts.st.CreateJob(ctx, &store.Job{
		Name: "two-guests", Kind: store.SourceVM, TargetID: tgt.ID,
		Schedule: store.ManualSchedule(), Retention: store.KeepLast(1), Enabled: true,
		Sources: store.JobSources{
			{HostID: ts.hostID, VMID: 100, Name: "web-01"},
			{HostID: ts.hostID, VMID: 101, Name: "db-01"},
		},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	base := time.Date(2026, time.March, 1, 3, 0, 0, 0, time.UTC)
	for _, vmid := range []int{100, 101} {
		for i := 0; i < 3; i++ {
			if _, err := ts.st.CreateBackup(ctx, &store.Backup{
				JobID: job.ID, SourceKind: store.SourceVM,
				SourceID:   fmt.Sprintf("%s_%d", ts.hostID, vmid),
				SourceName: fmt.Sprintf("vm-%d", vmid), TargetID: tgt.ID,
				CreatedAt: base.AddDate(0, 0, i), SizeBytes: 1,
			}); err != nil {
				t.Fatalf("create backup: %v", err)
			}
		}
	}
	code, out := ts.request(t, http.MethodGet, "/api/jobs/"+job.ID+"/retention-preview", nil)
	if code != http.StatusOK {
		t.Fatalf("GET retention-preview = %d (%v)", code, out)
	}
	keeps, prunes := previewLists(t, out)
	if len(keeps) != 2 || len(prunes) != 4 {
		t.Fatalf("keep-last-1 over two guests = %d keeps / %d prunes, want 2/4", len(keeps), len(prunes))
	}
}

// previewLists decodes the two arrays of a retention preview.
func previewLists(t *testing.T, body map[string]any) (keeps, prunes []map[string]any) {
	t.Helper()
	for _, c := range []struct {
		field string
		out   *[]map[string]any
	}{{"keeps", &keeps}, {"prunes", &prunes}} {
		raw, ok := body[c.field].([]any)
		if !ok {
			t.Fatalf("preview[%q] = %#v, want an array", c.field, body[c.field])
		}
		for _, entry := range raw {
			obj, ok := entry.(map[string]any)
			if !ok {
				t.Fatalf("preview[%q] entry = %#v, want an object", c.field, entry)
			}
			if _, ok := obj["backupId"].(string); !ok {
				t.Fatalf("preview entry has no backupId: %#v", obj)
			}
			if _, ok := obj["createdAt"].(string); !ok {
				t.Fatalf("preview entry has no createdAt: %#v", obj)
			}
			*c.out = append(*c.out, obj)
		}
	}
	return keeps, prunes
}
