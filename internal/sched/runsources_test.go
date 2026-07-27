package sched

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"proxback/internal/store"
)

// sources reads a run's per-source breakdown.
func (h *runLogHarness) sources(runID string) []store.RunSource {
	h.t.Helper()
	out, err := h.st.RunSources(context.Background(), runID)
	if err != nil {
		h.t.Fatalf("read run sources: %v", err)
	}
	return out
}

// TestRunSourcesRecordABackupRun drives a real two-guest backup against the
// simulators and asserts the rows the visual monitor draws: one per VM, in run
// order, ending success with the bytes they actually moved.
func TestRunSourcesRecordABackupRun(t *testing.T) {
	h := newRunLogHarness(t)
	ctx := context.Background()

	job, err := h.st.CreateJob(ctx, &store.Job{
		Name: "nightly-vms", Kind: store.SourceVM, TargetID: h.target.ID,
		Schedule: store.ManualSchedule(), Retention: store.KeepLast(2), Enabled: true,
		Sources: store.JobSources{
			{HostID: h.host.ID, VMID: 100, Name: "web-01"},
			{HostID: h.host.ID, VMID: 101, Name: "db-01"},
		},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	run := h.runJob(job.ID)
	if run.Status != store.RunSuccess {
		t.Fatalf("run finished %q: %s", run.Status, run.Error)
	}

	got := h.sources(run.ID)
	if len(got) != 2 {
		t.Fatalf("run recorded %d sources, want 2: %+v", len(got), got)
	}
	for i, want := range []struct {
		name string
		size int64
	}{
		{"web-01", 32 << 20}, // two 16 MiB disks
		{"db-01", 16 << 20},  // one
	} {
		src := got[i]
		if src.Seq != i || src.Name != want.name {
			t.Fatalf("source %d = %+v, want seq %d named %s", i, src, i, want.name)
		}
		if src.Status != store.SourceSuccess {
			t.Errorf("%s finished %q (%s), want success", src.Name, src.Status, src.Error)
		}
		if src.Kind != store.SourceVM || src.Node != "pve1" {
			t.Errorf("%s = kind %q on node %q, want a vm on pve1", src.Name, src.Kind, src.Node)
		}
		if src.SizeBytes != want.size {
			t.Errorf("%s sizeBytes = %d, want %d", src.Name, src.SizeBytes, want.size)
		}
		if src.BytesProcessed != want.size {
			t.Errorf("%s bytesProcessed = %d, want %d", src.Name, src.BytesProcessed, want.size)
		}
		// Nothing is on the target yet, so every byte had to travel.
		if src.BytesUploaded == 0 {
			t.Errorf("%s uploaded nothing on a first full backup", src.Name)
		}
		if src.ProgressPct != 100 {
			t.Errorf("%s progressPct = %v, want 100", src.Name, src.ProgressPct)
		}
		if src.StartedAt == nil || src.FinishedAt == nil {
			t.Errorf("%s timing = %v … %v, want both set", src.Name, src.StartedAt, src.FinishedAt)
		}
		if src.Error != "" {
			t.Errorf("%s recorded error %q on a successful backup", src.Name, src.Error)
		}
	}
	// The per-source figures add up to the run's own.
	var processed, uploaded int64
	for _, src := range got {
		processed += src.BytesProcessed
		uploaded += src.BytesUploaded
	}
	if processed != run.BytesProcessed || uploaded != run.BytesUploaded {
		t.Errorf("sources total %d/%d bytes, the run reports %d/%d",
			processed, uploaded, run.BytesProcessed, run.BytesUploaded)
	}
	// The live throughput sample belongs to the run in flight and goes with it.
	if bps := h.m.ThroughputBps(run.ID); bps != 0 {
		t.Errorf("a finished run still reports %v B/s", bps)
	}

	// A second run gets its own rows, deduplicating against the first.
	second := h.runJob(job.ID)
	if second.Status != store.RunSuccess {
		t.Fatalf("second run finished %q: %s", second.Status, second.Error)
	}
	for _, src := range h.sources(second.ID) {
		if src.Status != store.SourceSuccess || src.BytesProcessed != src.SizeBytes {
			t.Errorf("second run's %s = %+v", src.Name, src)
		}
		if src.BytesUploaded != 0 {
			t.Errorf("second run's %s uploaded %d bytes, want 0 (all chunks known)", src.Name, src.BytesUploaded)
		}
	}
	if len(h.sources(run.ID)) != 2 {
		t.Error("the first run's sources disappeared when the second one ran")
	}
}

// TestRunSourcesRecordAFailure proves the monitor tells an operator which guest
// broke and why — and that the guests the run never reached are closed out as
// skipped rather than left spinning as pending.
func TestRunSourcesRecordAFailure(t *testing.T) {
	h := newRunLogHarness(t)
	ctx := context.Background()

	// A storage target that is not there: planning succeeds (it only talks to
	// Proxmox) and the first guest fails as soon as the engine touches S3.
	dead := httptest.NewServer(nil)
	deadURL := dead.URL
	dead.Close()
	broken, err := h.st.CreateS3Target(ctx, &store.S3Target{
		Name: "unreachable", Endpoint: deadURL, Region: "us-east-1", Bucket: "gone",
		AccessKey: "proxback", SecretKey: "proxback-secret", PathStyle: true,
	})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}

	job, err := h.st.CreateJob(ctx, &store.Job{
		Name: "broken-target", Kind: store.SourceVM, TargetID: broken.ID,
		Schedule: store.ManualSchedule(), Retention: store.KeepLast(2), Enabled: true,
		Sources: store.JobSources{
			{HostID: h.host.ID, VMID: 100, Name: "web-01"},
			{HostID: h.host.ID, VMID: 101, Name: "db-01"},
		},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	run := h.runJob(job.ID)
	if run.Status != store.RunFailed {
		t.Fatalf("run against an unreachable target finished %q", run.Status)
	}

	got := h.sources(run.ID)
	if len(got) != 2 {
		t.Fatalf("failed run recorded %d sources, want 2: %+v", len(got), got)
	}
	failed := got[0]
	if failed.Name != "web-01" || failed.Status != store.SourceFailed {
		t.Fatalf("first source = %+v, want web-01 failed", failed)
	}
	if failed.Error == "" {
		t.Error("the failed source carries no error")
	}
	if failed.Error != run.Error {
		t.Errorf("source error %q, run error %q — the monitor and the run must agree",
			failed.Error, run.Error)
	}
	if failed.FinishedAt == nil {
		t.Error("the failed source was never closed out")
	}
	// The guest the run never reached keeps the size it was planned with, which
	// is how the monitor can show a total before anything has moved.
	skipped := got[1]
	if skipped.Name != "db-01" || skipped.Status != store.SourceSkipped {
		t.Fatalf("second source = %+v, want db-01 skipped", skipped)
	}
	if skipped.SizeBytes != 16<<20 {
		t.Errorf("skipped source sizeBytes = %d, want the size it was planned with", skipped.SizeBytes)
	}
	if skipped.BytesProcessed != 0 || skipped.StartedAt != nil {
		t.Errorf("skipped source looks like it ran: %+v", skipped)
	}
}

// TestRunMonitorThroughput covers the sampling behind throughputBps: a figure
// only appears once a full window has elapsed, and it is the bytes of that
// window over its length.
func TestRunMonitorThroughput(t *testing.T) {
	h := newRunLogHarness(t)
	mon := newRunMonitor(h.st, discardLog(), "run-1")

	mon.mu.Lock()
	mon.sample(0)
	if mon.bps != 0 {
		t.Errorf("the first sample produced a rate of %v, want 0", mon.bps)
	}
	// A second sample inside the window is too soon to say anything.
	mon.sample(4 << 20)
	if mon.bps != 0 {
		t.Errorf("a sub-window sample produced %v, want 0", mon.bps)
	}
	// Pretend the window has passed: 8 MiB in two seconds is 4 MiB/s.
	mon.sampleAt = time.Now().Add(-2 * time.Second)
	mon.sampleBytes = 0
	mon.sample(8 << 20)
	got := mon.bps
	mon.mu.Unlock()
	if want := float64(4 << 20); got < want*0.9 || got > want*1.1 {
		t.Errorf("throughput = %v B/s, want about %v", got, want)
	}

	// A run this manager is not executing has no monitor and therefore no rate.
	if bps := h.m.ThroughputBps("run-1"); bps != 0 {
		t.Errorf("ThroughputBps for an unknown run = %v, want 0", bps)
	}
}

// TestDeletingARunRemovesItsSources keeps history cleanup from leaving orphan
// source rows behind.
func TestDeletingARunRemovesItsSources(t *testing.T) {
	h := newRunLogHarness(t)
	ctx := context.Background()

	job, err := h.st.CreateJob(ctx, &store.Job{
		Name: "nightly-vms", Kind: store.SourceVM, TargetID: h.target.ID,
		Schedule: store.ManualSchedule(), Retention: store.KeepLast(2), Enabled: true,
		Sources: store.JobSources{{HostID: h.host.ID, VMID: 100, Name: "web-01"}},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	run := h.runJob(job.ID)
	if len(h.sources(run.ID)) != 1 {
		t.Fatalf("run recorded %d sources, want 1", len(h.sources(run.ID)))
	}
	if err := h.st.DeleteJobRun(ctx, run.ID); err != nil {
		t.Fatalf("delete run: %v", err)
	}
	if got := h.sources(run.ID); len(got) != 0 {
		t.Fatalf("sources survived the run's deletion: %+v", got)
	}
}
