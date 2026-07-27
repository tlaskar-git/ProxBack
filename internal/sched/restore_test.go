package sched

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"proxback/internal/store"
)

// TestFreeVMID covers the id a side-by-side restore is offered. Proxmox
// reserves everything below 100, so that is the floor whatever is asked for.
func TestFreeVMID(t *testing.T) {
	for _, c := range []struct {
		what  string
		used  []int
		after int
		want  int
	}{
		{"an empty cluster", nil, 0, 100},
		{"the first gap", []int{100, 101, 103}, 0, 102},
		{"a contiguous cluster", []int{100, 101, 102}, 0, 103},
		{"below the reserved floor", []int{100}, 7, 101},
		{"from a hint", []int{100, 101, 9000}, 9000, 9001},
		{"a hint that is already free", []int{100, 101}, 500, 500},
		{"unordered input", []int{9000, 100, 102, 101}, 0, 103},
	} {
		if got := FreeVMID(c.used, c.after); got != c.want {
			t.Errorf("FreeVMID(%s) = %d, want %d", c.what, got, c.want)
		}
	}
}

// restoreHarness reuses the run-log harness: it already wires the Proxmox and
// S3 simulators to a real manager, which is what a restore has to be checked
// against.
func newRestoreHarness(t *testing.T) *runLogHarness { return newRunLogHarness(t) }

// backupOf runs a one-guest job so there is a real restore point to restore.
func (h *runLogHarness) backupOf(t *testing.T, vmid int, name string) *store.Backup {
	t.Helper()
	ctx := context.Background()
	job, err := h.st.CreateJob(ctx, &store.Job{
		Name: "restore-fixture-" + name, Kind: store.SourceVM, TargetID: h.target.ID,
		Schedule: store.ManualSchedule(), Retention: store.KeepLast(2), Enabled: true,
		Sources: store.JobSources{{HostID: h.host.ID, VMID: vmid, Name: name}},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	run := h.runJob(job.ID)
	if run.Status != store.RunSuccess {
		t.Fatalf("fixture backup finished %q: %s", run.Status, run.Error)
	}
	points, err := h.st.ListBackups(ctx, store.BackupFilter{
		SourceKind: store.SourceVM, SourceID: VMSourceID(h.host.ID, vmid),
	})
	if err != nil || len(points) == 0 {
		t.Fatalf("restore points = %+v (%v)", points, err)
	}
	return points[0]
}

// TestRestoreAlongsideRefusesAnExistingVMID is the safety rule that matters
// most: a restore that says nothing about its mode must never land on a guest
// that already exists.
func TestRestoreAlongsideRefusesAnExistingVMID(t *testing.T) {
	h := newRestoreHarness(t)
	ctx := context.Background()
	point := h.backupOf(t, 100, "web-01")

	// No mode at all — the legacy request shape — onto the guest it came from.
	_, err := h.m.TriggerRestore(ctx, RestoreSpec{
		BackupID: point.ID,
		VM:       &VMRestoreTarget{HostID: h.host.ID, Node: "pve1", VMID: 100},
	})
	if !errors.Is(err, ErrVMIDInUse) {
		t.Fatalf("legacy restore onto an existing guest = %v, want ErrVMIDInUse", err)
	}
	// The message tells the operator both ways out.
	if !strings.Contains(err.Error(), "free-vmid") || !strings.Contains(err.Error(), "overwrite") {
		t.Fatalf("refusal = %q, want it to name the free-vmid suggestion and the overwrite mode", err)
	}
	// And an explicit alongside is refused identically.
	if _, err := h.m.TriggerRestore(ctx, RestoreSpec{
		BackupID: point.ID, Mode: store.RestoreAlongside,
		VM: &VMRestoreTarget{HostID: h.host.ID, Node: "pve1", VMID: 100},
	}); !errors.Is(err, ErrVMIDInUse) {
		t.Fatalf("alongside restore onto an existing guest = %v", err)
	}
	// Nothing was started: a refused restore leaves no run behind.
	runs, err := h.st.ListRuns(ctx, "", 50)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	for _, r := range runs {
		if r.Kind == store.RunKindRestore {
			t.Fatalf("a refused restore created run %s", r.ID)
		}
	}

	// A free id is accepted, and the destination is recorded with the run.
	free, err := h.m.FreeVMIDForHost(ctx, h.host.ID, 0)
	if err != nil {
		t.Fatalf("free vmid: %v", err)
	}
	if free != 104 {
		t.Fatalf("free vmid = %d, want the first gap after the simulator's guests", free)
	}
	runID, err := h.m.TriggerRestore(ctx, RestoreSpec{
		BackupID: point.ID,
		VM:       &VMRestoreTarget{HostID: h.host.ID, Node: "pve1", VMID: free},
	})
	if err != nil {
		t.Fatalf("alongside restore into a free vmid: %v", err)
	}
	run := h.awaitRun(t, runID)
	if run.Status != store.RunSuccess {
		t.Fatalf("alongside restore finished %q: %s", run.Status, run.Error)
	}
	if run.Restore == nil {
		t.Fatal("the restore run recorded no destination")
	}
	if run.Restore.Mode != store.RestoreAlongside || run.Restore.VMID != free ||
		run.Restore.Node != "pve1" || run.Restore.HostName != h.host.Name {
		t.Fatalf("restore destination = %+v", run.Restore)
	}
}

// An overwrite is unlocked by typing the destination guest's current name, and
// by nothing else.
func TestRestoreOverwriteNeedsAMatchingConfirmName(t *testing.T) {
	h := newRestoreHarness(t)
	ctx := context.Background()
	point := h.backupOf(t, 100, "web-01")

	for _, c := range []struct{ what, confirm string }{
		{"no confirmation", ""},
		{"the wrong name", "web-02"},
		{"the right name in the wrong case", "WEB-01"},
	} {
		_, err := h.m.TriggerRestore(ctx, RestoreSpec{
			BackupID: point.ID, Mode: store.RestoreOverwrite, ConfirmName: c.confirm,
			VM: &VMRestoreTarget{HostID: h.host.ID, Node: "pve1", VMID: 100},
		})
		if !errors.Is(err, ErrConfirmName) {
			t.Fatalf("overwrite with %s = %v, want ErrConfirmName", c.what, err)
		}
		if !strings.Contains(err.Error(), `"web-01"`) {
			t.Fatalf("refusal %q does not tell the operator what to type", err)
		}
	}

	// Overwriting something that is not there is not an overwrite.
	if _, err := h.m.TriggerRestore(ctx, RestoreSpec{
		BackupID: point.ID, Mode: store.RestoreOverwrite, ConfirmName: "web-01",
		VM: &VMRestoreTarget{HostID: h.host.ID, Node: "pve1", VMID: 9999},
	}); !errors.Is(err, ErrConfirmName) {
		t.Fatalf("overwrite of a non-existent guest = %v", err)
	}

	// The right name goes through, and the run records that it was an overwrite.
	runID, err := h.m.TriggerRestore(ctx, RestoreSpec{
		BackupID: point.ID, Mode: store.RestoreOverwrite, ConfirmName: "web-01",
		VM: &VMRestoreTarget{HostID: h.host.ID, Node: "pve1", VMID: 100},
	})
	if err != nil {
		t.Fatalf("confirmed overwrite: %v", err)
	}
	run := h.awaitRun(t, runID)
	if run.Status != store.RunSuccess {
		t.Fatalf("confirmed overwrite finished %q: %s", run.Status, run.Error)
	}
	if run.Restore == nil || run.Restore.Mode != store.RestoreOverwrite || run.Restore.VMID != 100 {
		t.Fatalf("overwrite destination = %+v", run.Restore)
	}

	// An unrecognised mode is refused outright rather than guessed at.
	if _, err := h.m.TriggerRestore(ctx, RestoreSpec{
		BackupID: point.ID, Mode: "clobber",
		VM: &VMRestoreTarget{HostID: h.host.ID, Node: "pve1", VMID: 9999},
	}); !errors.Is(err, ErrBadRestoreMode) {
		t.Fatalf("unknown restore mode = %v", err)
	}
}

// A verification writes its evidence onto the restore point, and says only what
// it proved.
func TestVerifyRecordsEvidenceOnTheRestorePoint(t *testing.T) {
	h := newRestoreHarness(t)
	ctx := context.Background()
	point := h.backupOf(t, 101, "db-01")

	runID, err := h.m.Verify(ctx, point.ID)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	run := h.awaitRun(t, runID)
	if run.Status != store.RunSuccess {
		t.Fatalf("verify finished %q: %s", run.Status, run.Error)
	}
	verified, err := h.st.BackupByID(ctx, point.ID)
	if err != nil {
		t.Fatalf("reload restore point: %v", err)
	}
	if verified.LastVerifyResult != store.VerifyPassed || verified.LastVerifiedAt == nil {
		t.Fatalf("verified restore point = %+v", verified)
	}
	if verified.VerifiedBytes != point.SizeBytes {
		t.Fatalf("verifiedBytes = %d, want the %d bytes it read", verified.VerifiedBytes, point.SizeBytes)
	}

	// The log says integrity, and is explicit that this is not a restore test.
	lines := h.lines(run.ID)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "integrity verified") {
		t.Fatalf("verify log does not report integrity:\n%s", joined)
	}
	if !strings.Contains(joined, "not a restore test") {
		t.Fatalf("verify log does not disclaim restore testing:\n%s", joined)
	}
	for _, forbidden := range []string{"restorable", "would restore", "restore will succeed"} {
		if strings.Contains(strings.ToLower(joined), forbidden) {
			t.Fatalf("verify log claims recoverability (%q):\n%s", forbidden, joined)
		}
	}
}

// awaitRun waits for a run to leave the running state and returns it.
func (h *runLogHarness) awaitRun(t *testing.T, runID string) *store.JobRun {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		run, err := h.st.RunByID(ctx, runID)
		if err != nil {
			t.Fatalf("load run: %v", err)
		}
		if run.Status != store.RunRunning {
			return run
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s still running after 60s", runID)
	return nil
}
