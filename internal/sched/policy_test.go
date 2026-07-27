package sched

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"proxback/internal/agentmgr"
	"proxback/internal/engine"
	"proxback/internal/pvesim"
	"proxback/internal/s3sim"
	"proxback/internal/store"
)

// testPolicyMinute is what a "minute" of policy time is worth in these tests.
// The waits are real — the timers, the contexts and the cancellations all run
// exactly as they do in production — only shorter.
const testPolicyMinute = 40 * time.Millisecond

// ---------------------------------------------------------------- run control

// TestRunUnderPolicyRetriesAFlakySource covers the promise of retryCount: a
// source that fails twice and then succeeds produces one successful run, with
// every attempt visible in the log.
func TestRunUnderPolicyRetriesAFlakySource(t *testing.T) {
	h := newPolicyHarness(t)
	ctx := context.Background()

	// The export endpoint fails the first two times it is asked, exactly the way
	// a busy storage backend does.
	h.faults.failExports(2)

	job, err := h.st.CreateJob(ctx, &store.Job{
		Name: "flaky-vm", Kind: store.SourceVM, TargetID: h.target.ID,
		Schedule: store.ManualSchedule(), Retention: store.KeepLast(2), Enabled: true,
		Sources: store.JobSources{{HostID: h.host.ID, VMID: 101, Name: "db-01"}},
		Policy: store.JobPolicy{
			Quiesce: store.QuiesceNone, RetryCount: 3, RetryDelayMinutes: 1,
			ScriptTimeoutSeconds: 30,
		},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	run := h.runJob(job.ID)
	if run.Status != store.RunSuccess {
		t.Fatalf("run finished %q: %s", run.Status, run.Error)
	}
	lines := h.lines(run.ID)
	// Two failures, each announced with the delay before the next attempt, and
	// the attempt that finally worked.
	for _, want := range []string{
		"attempt 1 of 4 failed",
		"attempt 2 of 4 starting",
		"attempt 2 of 4 failed",
		"attempt 3 of 4 starting",
		"attempt 3 of 4 succeeded",
	} {
		requireLine(t, lines, want)
	}
	if n := countLines(lines, "retrying in 1 minute"); n != 2 {
		t.Fatalf("the log announced %d retry delays, want one per failed attempt", n)
	}
	// The restore point of the successful attempt is the only one there is.
	backups, err := h.st.ListBackups(ctx, store.BackupFilter{JobID: job.ID})
	if err != nil {
		t.Fatalf("list backups: %v", err)
	}
	if len(backups) != 1 {
		t.Fatalf("a retried run produced %d restore points, want 1", len(backups))
	}
}

// TestRunUnderPolicyGivesUpAfterTheLastAttempt covers the other end: a source
// that never recovers fails the run, and the log says the attempts ran out.
func TestRunUnderPolicyGivesUpAfterTheLastAttempt(t *testing.T) {
	h := newPolicyHarness(t)
	ctx := context.Background()
	h.faults.failExports(100)

	job, err := h.st.CreateJob(ctx, &store.Job{
		Name: "broken-vm", Kind: store.SourceVM, TargetID: h.target.ID,
		Schedule: store.ManualSchedule(), Retention: store.KeepLast(2), Enabled: true,
		Sources: store.JobSources{{HostID: h.host.ID, VMID: 101, Name: "db-01"}},
		Policy:  store.JobPolicy{RetryCount: 2, RetryDelayMinutes: 1},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	run := h.runJob(job.ID)
	if run.Status != store.RunFailed {
		t.Fatalf("run finished %q, want failed", run.Status)
	}
	requireLine(t, h.lines(run.ID), "attempt 3 of 3 failed")
	requireLine(t, h.lines(run.ID), "no attempts left")
}

// TestRunUnderPolicyDoesNotRetryACancellation is the rule that keeps a cancel
// button meaning what it says: an operator who stops a run has stopped it, not
// asked for it to be tried again.
func TestRunUnderPolicyDoesNotRetryACancellation(t *testing.T) {
	h := newPolicyHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	policy := store.JobPolicy{RetryCount: 3, RetryDelayMinutes: 1}

	_, err := h.m.runUnderPolicy(ctx, "run-cancel", policy, func(attemptCtx context.Context) (*engine.Stats, error) {
		attempts++
		cancel()
		return nil, context.Canceled
	})
	if attempts != 1 {
		t.Fatalf("a cancelled run was attempted %d times", attempts)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled run reported %v", err)
	}
}

// TestRunUnderPolicyEnforcesTheDurationLimit covers maxDurationMinutes: the run
// is cancelled, and the error says that is what happened rather than surfacing
// whatever the interrupted transfer complained about.
func TestRunUnderPolicyEnforcesTheDurationLimit(t *testing.T) {
	h := newPolicyHarness(t)
	policy := store.JobPolicy{MaxDurationMinutes: 1, RetryCount: 2, RetryDelayMinutes: 1}

	started := time.Now()
	attempts := 0
	_, err := h.m.runUnderPolicy(context.Background(), "run-slow", policy,
		func(attemptCtx context.Context) (*engine.Stats, error) {
			attempts++
			<-attemptCtx.Done()
			return nil, fmt.Errorf("read disk: %w", attemptCtx.Err())
		})
	if !errors.Is(err, ErrMaxDuration) {
		t.Fatalf("run over its limit reported %v, want ErrMaxDuration", err)
	}
	if !strings.Contains(err.Error(), "1 minute") {
		t.Fatalf("the limit error does not name the limit: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("a run stopped by its duration limit was attempted %d times", attempts)
	}
	if took := time.Since(started); took > 40*testPolicyMinute {
		t.Fatalf("the duration limit took %s to bite", took)
	}
}

// TestBackupStopsAtTheDurationLimit runs the limit against a real backup: a
// slow export is cut off and the run fails with the duration error, not with a
// transport one.
func TestBackupStopsAtTheDurationLimit(t *testing.T) {
	h := newPolicyHarness(t)
	ctx := context.Background()
	h.faults.delayExports(30 * time.Second)

	job, err := h.st.CreateJob(ctx, &store.Job{
		Name: "slow-vm", Kind: store.SourceVM, TargetID: h.target.ID,
		Schedule: store.ManualSchedule(), Retention: store.KeepLast(2), Enabled: true,
		Sources: store.JobSources{{HostID: h.host.ID, VMID: 101, Name: "db-01"}},
		Policy:  store.JobPolicy{MaxDurationMinutes: 1},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	run := h.runJob(job.ID)
	if run.Status != store.RunFailed {
		t.Fatalf("run finished %q (%s), want failed", run.Status, run.Error)
	}
	if !strings.Contains(run.Error, "exceeded its maximum duration") {
		t.Fatalf("run error = %q, want the duration limit", run.Error)
	}
	requireLine(t, h.lines(run.ID), "policy: this run is limited to 1 minute")
	if backups, err := h.st.ListBackups(ctx, store.BackupFilter{JobID: job.ID}); err != nil || len(backups) != 0 {
		t.Fatalf("an abandoned run left %d restore points (%v)", len(backups), err)
	}
}

// ---------------------------------------------------------------- window

func TestWindowCheck(t *testing.T) {
	night := &store.BackupWindow{Start: "22:00", End: "06:00"}
	clock := func(h, m int) time.Time {
		return time.Date(2026, time.March, 1, h, m, 0, 0, time.Local)
	}
	for _, c := range []struct {
		name        string
		window      *store.BackupWindow
		origin      string
		at          time.Time
		wantAllowed bool
		wantNote    string
	}{
		{"no window, scheduled", nil, TriggerScheduled, clock(12, 0), true, ""},
		{"inside the window, scheduled", night, TriggerScheduled, clock(23, 30), true, ""},
		{"just after midnight, scheduled", night, TriggerScheduled, clock(2, 0), true, ""},
		{"outside the window, scheduled", night, TriggerScheduled, clock(12, 0), false, "outside the backup window (22:00–06:00)"},
		{"on the closing minute, scheduled", night, TriggerScheduled, clock(6, 0), false, "outside the backup window"},
		{"inside the window, manual", night, TriggerManual, clock(23, 30), true, ""},
		{"outside the window, manual", night, TriggerManual, clock(12, 0), true, "a manual run is always allowed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			policy := store.DefaultPolicy()
			policy.Window = c.window
			allowed, note := windowCheck(policy, c.origin, c.at)
			if allowed != c.wantAllowed {
				t.Fatalf("allowed = %v, want %v (note %q)", allowed, c.wantAllowed, note)
			}
			if c.wantNote == "" {
				if note != "" {
					t.Fatalf("note = %q, want none", note)
				}
				return
			}
			if !strings.Contains(note, c.wantNote) {
				t.Fatalf("note = %q, want it to contain %q", note, c.wantNote)
			}
		})
	}
}

// TestTriggerRespectsTheWindow covers the two paths through the trigger: the
// scheduler is refused outside the window and an operator never is — but the
// override is recorded, so nobody has to wonder later why a run happened at
// noon.
func TestTriggerRespectsTheWindow(t *testing.T) {
	h := newPolicyHarness(t)
	ctx := context.Background()

	// A window that cannot contain "now", wherever and whenever the test runs:
	// it opens one minute from now and closes two minutes from now.
	now := time.Now()
	closed := &store.BackupWindow{
		Start: now.Add(time.Hour).Format("15:04"),
		End:   now.Add(2 * time.Hour).Format("15:04"),
	}
	job, err := h.st.CreateJob(ctx, &store.Job{
		Name: "windowed", Kind: store.SourceVM, TargetID: h.target.ID,
		Schedule: store.ManualSchedule(), Retention: store.KeepLast(2), Enabled: true,
		Sources: store.JobSources{{HostID: h.host.ID, VMID: 101, Name: "db-01"}},
		Policy:  store.JobPolicy{Window: closed},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	if _, err := h.m.TriggerScheduledJob(ctx, job.ID); !errors.Is(err, ErrOutsideWindow) {
		t.Fatalf("a scheduled run outside the window = %v, want ErrOutsideWindow", err)
	}
	// Nothing was recorded: a skipped firing is not a run.
	runs, err := h.st.ListRuns(ctx, job.ID, 10)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("a skipped firing created %d runs", len(runs))
	}

	// The same job, started by a person, runs — and says so.
	run := h.runJob(job.ID)
	if run.Status != store.RunSuccess {
		t.Fatalf("manual run finished %q: %s", run.Status, run.Error)
	}
	requireLine(t, h.lines(run.ID), "a manual run is always allowed")

	// Inside the window the scheduler is allowed through.
	open := &store.BackupWindow{Start: "00:00", End: "23:59"}
	job.Policy.Window = open
	if err := h.st.UpdateJob(ctx, job); err != nil {
		t.Fatalf("update job: %v", err)
	}
	runID, err := h.m.TriggerScheduledJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("a scheduled run inside the window = %v", err)
	}
	h.wait(runID)
	// A run started inside the window carries no override note.
	for _, line := range h.lines(runID) {
		if strings.Contains(line, "manual run is always allowed") {
			t.Fatalf("a scheduled in-window run logged the manual override: %q", line)
		}
	}
}

// ---------------------------------------------------------------- exclusions

// TestExcludeDisksIsHonouredOnTheExportPath covers the path where per-disk
// exclusion is expressible: the excluded disk is neither read nor stored.
func TestExcludeDisksIsHonouredOnTheExportPath(t *testing.T) {
	h := newPolicyHarness(t)
	ctx := context.Background()

	job, err := h.st.CreateJob(ctx, &store.Job{
		Name: "web-without-scsi1", Kind: store.SourceVM, TargetID: h.target.ID,
		Schedule: store.ManualSchedule(), Retention: store.KeepLast(2), Enabled: true,
		Sources: store.JobSources{{HostID: h.host.ID, VMID: 100, Name: "web-01"}},
		Policy:  store.JobPolicy{ExcludeDisks: []string{"scsi1"}},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	run := h.runJob(job.ID)
	if run.Status != store.RunSuccess {
		t.Fatalf("run finished %q: %s", run.Status, run.Error)
	}
	requireLine(t, h.lines(run.ID), "web-01: policy excludes scsi1 — 1 disk will be backed up")

	backups, err := h.st.ListBackups(ctx, store.BackupFilter{JobID: job.ID})
	if err != nil || len(backups) != 1 {
		t.Fatalf("list backups = %d (%v)", len(backups), err)
	}
	if len(backups[0].Disks) != 1 || backups[0].Disks[0].Name != "scsi0" {
		t.Fatalf("restore point disks = %+v, want scsi0 only", backups[0].Disks)
	}
	// One 16 MiB disk, not two.
	if run.BytesProcessed != pvesim.DiskSize {
		t.Fatalf("processed %d bytes, want one disk (%d)", run.BytesProcessed, pvesim.DiskSize)
	}
}

// TestExcludeDisksIsRefusedOnTheHelperPath is the honesty case: a helper
// streams the whole guest as one vzdump archive, so the exclusion cannot be
// applied — and ProxBack refuses rather than storing an archive that quietly
// contains the disk the operator excluded.
func TestExcludeDisksIsRefusedOnTheHelperPath(t *testing.T) {
	h := newPolicyHarness(t)
	ctx := context.Background()
	h.registerHelper("pve1")

	job, err := h.st.CreateJob(ctx, &store.Job{
		Name: "helper-with-exclusions", Kind: store.SourceVM, TargetID: h.target.ID,
		Schedule: store.ManualSchedule(), Retention: store.KeepLast(2), Enabled: true,
		Sources: store.JobSources{{HostID: h.host.ID, VMID: 100, Name: "web-01"}},
		Policy:  store.JobPolicy{ExcludeDisks: []string{"scsi1"}},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	run := h.runJob(job.ID)
	if run.Status != store.RunFailed {
		t.Fatalf("run finished %q, want failed", run.Status)
	}
	for _, want := range []string{
		"cannot be honoured on the node-helper path",
		"backup=0",
	} {
		if !strings.Contains(run.Error, want) {
			t.Fatalf("run error = %q, want it to mention %q", run.Error, want)
		}
	}
	// Nothing was read: the refusal happens while the run is still planning.
	if len(h.helper.exports()) != 0 {
		t.Fatalf("the helper was asked to export despite the refusal")
	}
}

// TestExcludingEveryDiskIsRefused: a restore point with nothing in it is not a
// backup, so the policy is refused rather than obeyed.
func TestExcludingEveryDiskIsRefused(t *testing.T) {
	h := newPolicyHarness(t)
	ctx := context.Background()
	job, err := h.st.CreateJob(ctx, &store.Job{
		Name: "nothing-left", Kind: store.SourceVM, TargetID: h.target.ID,
		Schedule: store.ManualSchedule(), Retention: store.KeepLast(2), Enabled: true,
		Sources: store.JobSources{{HostID: h.host.ID, VMID: 101, Name: "db-01"}},
		Policy:  store.JobPolicy{ExcludeDisks: []string{"scsi0"}},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	run := h.runJob(job.ID)
	if run.Status != store.RunFailed || !strings.Contains(run.Error, "no disks to back up") {
		t.Fatalf("run = %q / %q, want a refusal", run.Status, run.Error)
	}
}

// ---------------------------------------------------------------- quiescing

// TestQuiesceReportsWhatActuallyHappened is the anti-lie test: a policy that
// asks for guest-agent quiescing on a guest that has no guest agent must
// produce a warning and a restore point described as crash-consistent, never
// silence.
func TestQuiesceReportsWhatActuallyHappened(t *testing.T) {
	h := newPolicyHarness(t)
	ctx := context.Background()
	job, err := h.st.CreateJob(ctx, &store.Job{
		Name: "quiesced", Kind: store.SourceVM, TargetID: h.target.ID,
		Schedule: store.ManualSchedule(), Retention: store.KeepLast(2), Enabled: true,
		Sources: store.JobSources{{HostID: h.host.ID, VMID: 101, Name: "db-01"}},
		Policy:  store.JobPolicy{Quiesce: store.QuiesceGuestAgent},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	run := h.runJob(job.ID)
	if run.Status != store.RunSuccess {
		t.Fatalf("run finished %q: %s", run.Status, run.Error)
	}
	warning := requireLine(t, h.lines(run.ID), "policy asks for guest-agent quiescing")
	if !strings.Contains(warning, "crash-consistent") || !strings.HasPrefix(warning, "warning:") {
		t.Fatalf("quiesce line = %q, want a warning that says crash-consistent", warning)
	}
}

func TestGuestAgentEnabled(t *testing.T) {
	for _, c := range []struct {
		cfg  map[string]any
		want bool
	}{
		{map[string]any{}, false},
		{map[string]any{"agent": "1"}, true},
		{map[string]any{"agent": "0"}, false},
		{map[string]any{"agent": "enabled=1,fstrim_cloned_disks=1"}, true},
		{map[string]any{"agent": "enabled=0"}, false},
		{map[string]any{"agent": float64(1)}, true},
		{map[string]any{"agent": float64(0)}, false},
		{map[string]any{"agent": "fstrim_cloned_disks=1"}, false},
	} {
		if got := guestAgentEnabled(c.cfg); got != c.want {
			t.Errorf("guestAgentEnabled(%v) = %v, want %v", c.cfg, got, c.want)
		}
	}
}

// ---------------------------------------------------------------- scripts

// TestPreScriptFailureStopsBeforeAnyDataMoves is the contract of a pre-script:
// if it fails, the estate is exactly as it was — nothing was read, nothing was
// uploaded, no restore point exists.
func TestPreScriptFailureStopsBeforeAnyDataMoves(t *testing.T) {
	h := newPolicyHarness(t)
	ctx := context.Background()
	h.registerHelper("pve1")
	h.helper.failPhase(phasePre, "freeze.sh: database is locked")

	job, err := h.st.CreateJob(ctx, &store.Job{
		Name: "scripted", Kind: store.SourceVM, TargetID: h.target.ID,
		Schedule: store.ManualSchedule(), Retention: store.KeepLast(2), Enabled: true,
		Sources: store.JobSources{{HostID: h.host.ID, VMID: 101, Name: "db-01"}},
		Policy: store.JobPolicy{
			PreScript: "/usr/local/bin/freeze.sh --db", PostScript: "/usr/local/bin/thaw.sh",
			ScriptTimeoutSeconds: 30,
		},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	run := h.runJob(job.ID)
	if run.Status != store.RunFailed {
		t.Fatalf("run finished %q, want failed", run.Status)
	}
	if !strings.Contains(run.Error, "pre-script") {
		t.Fatalf("run error = %q, want it to name the pre-script", run.Error)
	}
	if run.BytesProcessed != 0 || run.BytesUploaded != 0 {
		t.Fatalf("a failed pre-script still moved %d/%d bytes", run.BytesProcessed, run.BytesUploaded)
	}
	if len(h.helper.exports()) != 0 {
		t.Fatalf("the helper exported despite the pre-script failing")
	}
	if backups, err := h.st.ListBackups(ctx, store.BackupFilter{JobID: job.ID}); err != nil || len(backups) != 0 {
		t.Fatalf("a failed pre-script left %d restore points (%v)", len(backups), err)
	}
	// The post-script is not run when the pre-script refused.
	if got := h.helper.scriptPhases(); len(got) != 1 || got[0] != phasePre {
		t.Fatalf("scripts run = %v, want only the pre-script", got)
	}
	// What the script said reaches the operator; what the script *is* does not.
	lines := h.lines(run.ID)
	requireLine(t, lines, "db-01: pre-script: freeze.sh: database is locked")
	for _, line := range lines {
		if strings.Contains(line, "--db") {
			t.Fatalf("the script body leaked into the run log: %q", line)
		}
	}
}

// TestPostScriptFailureKeepsTheRestorePoint: the run failed, because the
// operator asked for the script to matter — but the data it took is real and
// stays.
func TestPostScriptFailureKeepsTheRestorePoint(t *testing.T) {
	h := newPolicyHarness(t)
	ctx := context.Background()
	h.registerHelper("pve1")
	h.helper.failPhase(phasePost, "thaw.sh: exit 1")

	job, err := h.st.CreateJob(ctx, &store.Job{
		Name: "scripted-post", Kind: store.SourceVM, TargetID: h.target.ID,
		Schedule: store.ManualSchedule(), Retention: store.KeepLast(2), Enabled: true,
		Sources: store.JobSources{{HostID: h.host.ID, VMID: 101, Name: "db-01"}},
		Policy: store.JobPolicy{
			PreScript: "/usr/local/bin/freeze.sh", PostScript: "/usr/local/bin/thaw.sh",
			ScriptTimeoutSeconds: 30,
		},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	run := h.runJob(job.ID)
	if run.Status != store.RunFailed {
		t.Fatalf("run finished %q, want failed", run.Status)
	}
	if !strings.Contains(run.Error, "post-script") {
		t.Fatalf("run error = %q, want it to name the post-script", run.Error)
	}
	backups, err := h.st.ListBackups(ctx, store.BackupFilter{JobID: job.ID})
	if err != nil {
		t.Fatalf("list backups: %v", err)
	}
	if len(backups) != 1 {
		t.Fatalf("a failed post-script left %d restore points, want the one it was taken from", len(backups))
	}
	requireLine(t, h.lines(run.ID), "the restore point taken before the post-script is kept")
	if got := h.helper.scriptPhases(); len(got) != 2 || got[0] != phasePre || got[1] != phasePost {
		t.Fatalf("scripts run = %v, want the pre-script then the post-script", got)
	}
}

// TestScriptsRunOnTheHelperAndAreLogged is the happy path: both scripts run on
// the node that holds the data, their output is captured, and the timeout the
// policy set travels with them.
func TestScriptsRunOnTheHelperAndAreLogged(t *testing.T) {
	h := newPolicyHarness(t)
	ctx := context.Background()
	h.registerHelper("pve1")

	job, err := h.st.CreateJob(ctx, &store.Job{
		Name: "scripted-ok", Kind: store.SourceVM, TargetID: h.target.ID,
		Schedule: store.ManualSchedule(), Retention: store.KeepLast(2), Enabled: true,
		Sources: store.JobSources{{HostID: h.host.ID, VMID: 101, Name: "db-01"}},
		Policy: store.JobPolicy{
			PreScript: "/usr/local/bin/freeze.sh", PostScript: "/usr/local/bin/thaw.sh",
			ScriptTimeoutSeconds: 90,
		},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	run := h.runJob(job.ID)
	if run.Status != store.RunSuccess {
		t.Fatalf("run finished %q: %s", run.Status, run.Error)
	}
	lines := h.lines(run.ID)
	requireLine(t, lines, "db-01: running the pre-script on node pve1 (timeout 1m30s)")
	requireLine(t, lines, "db-01: running the post-script on node pve1")
	requireLine(t, lines, "db-01: pre-script: ran /usr/local/bin/freeze.sh")
	requireLine(t, lines, "db-01: post-script: ran /usr/local/bin/thaw.sh")

	calls := h.helper.scriptCalls()
	if len(calls) != 2 {
		t.Fatalf("the helper ran %d scripts, want 2", len(calls))
	}
	if calls[0].TimeoutSeconds != 90 {
		t.Fatalf("the pre-script carried a %ds timeout, want the policy's 90", calls[0].TimeoutSeconds)
	}
	if calls[0].Script != "/usr/local/bin/freeze.sh" || calls[1].Script != "/usr/local/bin/thaw.sh" {
		t.Fatalf("the helper was handed %q and %q", calls[0].Script, calls[1].Script)
	}
}

// TestVMScriptWithoutAHelperIsRefused: the scripts are meant to run where the
// data lives. On the export path there is no such place, and pretending to have
// run them would be the worst possible answer.
func TestVMScriptWithoutAHelperIsRefused(t *testing.T) {
	h := newPolicyHarness(t)
	ctx := context.Background()
	job, err := h.st.CreateJob(ctx, &store.Job{
		Name: "scripted-no-helper", Kind: store.SourceVM, TargetID: h.target.ID,
		Schedule: store.ManualSchedule(), Retention: store.KeepLast(2), Enabled: true,
		Sources: store.JobSources{{HostID: h.host.ID, VMID: 101, Name: "db-01"}},
		Policy:  store.JobPolicy{PreScript: "/usr/local/bin/freeze.sh"},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	run := h.runJob(job.ID)
	if run.Status != store.RunFailed {
		t.Fatalf("run finished %q, want failed", run.Status)
	}
	if !strings.Contains(run.Error, "node helper") {
		t.Fatalf("run error = %q, want it to ask for a node helper", run.Error)
	}
	if backups, _ := h.st.ListBackups(ctx, store.BackupFilter{JobID: job.ID}); len(backups) != 0 {
		t.Fatalf("a refused script run still produced %d restore points", len(backups))
	}
}

// ---------------------------------------------------------------- retention

// TestGFSRetentionPrunesWhatThePolicySays runs enough backups to exercise the
// pruning pass and checks it against the pure function's own verdict.
func TestGFSRetentionPrunesWhatThePolicySays(t *testing.T) {
	t.Setenv(GCGraceEnv, "0")
	h := newPolicyHarness(t)
	ctx := context.Background()

	job, err := h.st.CreateJob(ctx, &store.Job{
		Name: "gfs", Kind: store.SourceVM, TargetID: h.target.ID,
		Schedule: store.ManualSchedule(), Retention: store.RetentionPolicy{KeepLast: 2},
		Enabled: true,
		Sources: store.JobSources{{HostID: h.host.ID, VMID: 101, Name: "db-01"}},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, _, err := h.sim.Mutate(101); err != nil {
			t.Fatalf("mutate: %v", err)
		}
		if run := h.runJob(job.ID); run.Status != store.RunSuccess {
			t.Fatalf("run %d finished %q: %s", i, run.Status, run.Error)
		}
	}
	backups, err := h.st.ListBackups(ctx, store.BackupFilter{JobID: job.ID})
	if err != nil {
		t.Fatalf("list backups: %v", err)
	}
	// Every run prunes down to the two newest points; the fourth run's own
	// point plus its predecessor are what remain.
	if len(backups) != 2 {
		t.Fatalf("keep-last-2 left %d restore points", len(backups))
	}

	// A policy that keeps nothing prunes nothing: the last copy of a workload
	// is never thrown away because a counter was left at zero.
	job.Retention = store.RetentionPolicy{}
	if err := h.st.UpdateJob(ctx, job); err != nil {
		t.Fatalf("update job: %v", err)
	}
	if _, _, err := h.sim.Mutate(101); err != nil {
		t.Fatalf("mutate: %v", err)
	}
	if run := h.runJob(job.ID); run.Status != store.RunSuccess {
		t.Fatalf("run under an empty retention finished %q: %s", run.Status, run.Error)
	}
	backups, err = h.st.ListBackups(ctx, store.BackupFilter{JobID: job.ID})
	if err != nil {
		t.Fatalf("list backups: %v", err)
	}
	if len(backups) != 3 {
		t.Fatalf("an empty retention pruned down to %d restore points, want everything kept", len(backups))
	}
}

// countLines reports how many logged lines contain want.
func countLines(lines []string, want string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, want) {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------- harness

// policyHarness is a real scheduler over the Proxmox and S3 simulators, with a
// fault-injecting proxy in front of Proxmox and an optional stand-in for a node
// helper — everything a protection policy can act on.
type policyHarness struct {
	t      *testing.T
	st     *store.Store
	m      *Manager
	sim    *pvesim.Sim
	faults *faultyPVE
	helper *fakeNodeHelper
	host   *store.PVEHost
	target *store.S3Target
}

func newPolicyHarness(t *testing.T) *policyHarness {
	t.Helper()
	ctx := context.Background()
	log := discardLog()

	s3, err := s3sim.New("")
	if err != nil {
		t.Fatalf("start s3-sim: %v", err)
	}
	t.Cleanup(func() { _ = s3.Close() })
	s3srv := httptest.NewServer(s3.Handler)
	t.Cleanup(s3srv.Close)

	sim := pvesim.New(log)
	faults := &faultyPVE{inner: sim.Handler()}
	pveSrv := httptest.NewServer(faults)
	t.Cleanup(pveSrv.Close)

	st, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	host, err := st.CreatePVEHost(ctx, &store.PVEHost{
		Name: "pve-sim", BaseURL: pveSrv.URL,
		TokenID: "root@pam!proxback", TokenSecret: "sim-token-secret", InsecureTLS: true,
	})
	if err != nil {
		t.Fatalf("create host: %v", err)
	}
	target, err := st.CreateS3Target(ctx, &store.S3Target{
		Name: "vm-storage", Endpoint: s3srv.URL, Region: "us-east-1", Bucket: "proxback-policy",
		AccessKey: "proxback", SecretKey: "proxback-secret", PathStyle: true,
	})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}

	m := New(st, agentmgr.New(st, log), log)
	// A policy minute is milliseconds here: the waits are real, only short.
	m.policyMinute = testPolicyMinute
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	t.Cleanup(m.Stop)

	return &policyHarness{t: t, st: st, m: m, sim: sim, faults: faults, host: host, target: target}
}

// registerHelper stands a fake node helper up for a node and enrolls it, so the
// guests on that node take the vzdump path.
func (h *policyHarness) registerHelper(node string) *fakeNodeHelper {
	h.t.Helper()
	helper := newFakeNodeHelper(h.t)
	seen := time.Now().UTC()
	if _, err := h.st.CreateHelper(context.Background(), &store.NodeHelper{
		HostID: h.host.ID, Node: node, Address: helper.host, Port: helper.port,
		Version: "0.5.0", AccessSecret: helper.secret, APIKeyHash: "hash-" + node,
		LastSeen: &seen,
	}); err != nil {
		h.t.Fatalf("register helper: %v", err)
	}
	h.helper = helper
	return helper
}

func (h *policyHarness) runJob(jobID string) *store.JobRun {
	h.t.Helper()
	runID, err := h.m.TriggerJob(context.Background(), jobID)
	if err != nil {
		h.t.Fatalf("trigger job: %v", err)
	}
	return h.wait(runID)
}

func (h *policyHarness) wait(runID string) *store.JobRun {
	h.t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		run, err := h.st.RunByID(ctx, runID)
		if err != nil {
			h.t.Fatalf("load run: %v", err)
		}
		if run.Status != store.RunRunning {
			return run
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("run %s still running after 60s", runID)
	return nil
}

func (h *policyHarness) lines(runID string) []string {
	h.t.Helper()
	logged, err := h.st.RunLog(context.Background(), runID)
	if err != nil {
		h.t.Fatalf("read run log: %v", err)
	}
	out := make([]string, 0, len(logged))
	for _, l := range logged {
		out = append(out, l.Line)
	}
	return out
}

// faultyPVE proxies the Proxmox simulator and can make disk exports fail or
// crawl, which is how a flaky storage backend and a run that outlives its
// duration limit are reproduced without touching the simulator.
type faultyPVE struct {
	inner http.Handler

	mu      sync.Mutex
	failNum int
	delay   time.Duration
}

func (f *faultyPVE) failExports(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNum = n
}

func (f *faultyPVE) delayExports(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delay = d
}

func (f *faultyPVE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, "proxback-export") {
		f.mu.Lock()
		fail := f.failNum > 0
		if fail {
			f.failNum--
		}
		delay := f.delay
		f.mu.Unlock()
		if fail {
			http.Error(w, `{"data":null,"errors":{"storage":"temporarily unavailable"}}`,
				http.StatusInternalServerError)
			return
		}
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
	}
	f.inner.ServeHTTP(w, r)
}

// helperScriptCall is one script the fake helper was asked to run.
type helperScriptCall struct {
	Script         string `json:"script"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
	Phase          string `json:"phase"`
}

// fakeNodeHelper stands in for the proxback-helper daemon: it serves a vzdump
// stream and runs policy scripts, and can be told to fail either phase.
type fakeNodeHelper struct {
	srv    *httptest.Server
	host   string
	port   int
	secret string
	data   []byte

	mu         sync.Mutex
	scripts    []helperScriptCall
	exportsN   int
	failPhases map[string]string
}

func newFakeNodeHelper(t *testing.T) *fakeNodeHelper {
	t.Helper()
	h := &fakeNodeHelper{
		secret:     "fake-helper-secret",
		data:       make([]byte, 6<<20),
		failPhases: map[string]string{},
	}
	for i := range h.data {
		h.data[i] = byte(i * 7)
	}
	h.srv = httptest.NewServer(h.handler())
	t.Cleanup(h.srv.Close)
	host, port, err := net.SplitHostPort(strings.TrimPrefix(h.srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split helper url: %v", err)
	}
	h.host = host
	if h.port, err = strconv.Atoi(port); err != nil {
		t.Fatalf("helper port %q: %v", port, err)
	}
	return h
}

func (h *fakeNodeHelper) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/export/", func(w http.ResponseWriter, r *http.Request) {
		if !h.authorized(w, r) {
			return
		}
		h.mu.Lock()
		h.exportsN++
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(h.data)
	})
	mux.HandleFunc("/script", func(w http.ResponseWriter, r *http.Request) {
		if !h.authorized(w, r) {
			return
		}
		var call helperScriptCall
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		h.scripts = append(h.scripts, call)
		failure, failing := h.failPhases[call.Phase]
		h.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if failing {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": false, "output": failure, "error": "exit status 1",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "output": "ran " + call.Script,
		})
	})
	return mux
}

func (h *fakeNodeHelper) authorized(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+h.secret {
		http.Error(w, `{"error":"invalid access secret"}`, http.StatusUnauthorized)
		return false
	}
	return true
}

// failPhase makes the given script phase report a non-zero exit, with output.
func (h *fakeNodeHelper) failPhase(phase, output string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failPhases[phase] = output
}

func (h *fakeNodeHelper) scriptCalls() []helperScriptCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]helperScriptCall(nil), h.scripts...)
}

func (h *fakeNodeHelper) scriptPhases() []string {
	out := []string{}
	for _, c := range h.scriptCalls() {
		out = append(out, c.Phase)
	}
	return out
}

// exports reports how many vzdump streams the helper served.
func (h *fakeNodeHelper) exports() []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]int, h.exportsN)
	return out
}
