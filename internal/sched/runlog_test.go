package sched

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"proxback/internal/agentmgr"
	"proxback/internal/pvesim"
	"proxback/internal/s3sim"
	"proxback/internal/store"
)

// runLogHarness drives real backup runs against the Proxmox and S3 simulators so
// the assertions below are about what an operator actually reads after a run,
// not about a hand-written log.
type runLogHarness struct {
	t      *testing.T
	st     *store.Store
	m      *Manager
	sim    *pvesim.Sim
	host   *store.PVEHost
	target *store.S3Target
}

func newRunLogHarness(t *testing.T) *runLogHarness {
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
	pveSrv := httptest.NewServer(sim.Handler())
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
		Name: "vm-storage", Endpoint: s3srv.URL, Region: "us-east-1", Bucket: "proxback-sched",
		AccessKey: "proxback", SecretKey: "proxback-secret", PathStyle: true,
	})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}

	m := New(st, agentmgr.New(st, log), log)
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	t.Cleanup(m.Stop)

	return &runLogHarness{t: t, st: st, m: m, sim: sim, host: host, target: target}
}

// runJob triggers a job and waits for it to leave the running state.
func (h *runLogHarness) runJob(jobID string) *store.JobRun {
	h.t.Helper()
	ctx := context.Background()
	runID, err := h.m.TriggerJob(ctx, jobID)
	if err != nil {
		h.t.Fatalf("trigger job: %v", err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		run, err := h.st.RunByID(ctx, runID)
		if err != nil {
			h.t.Fatalf("load run: %v", err)
		}
		if run.Status != store.RunRunning {
			return run
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("run %s still running after 60s", runID)
	return nil
}

// lines returns a run's activity log as plain strings.
func (h *runLogHarness) lines(runID string) []string {
	h.t.Helper()
	logged, err := h.st.RunLog(context.Background(), runID)
	if err != nil {
		h.t.Fatalf("read run log: %v", err)
	}
	out := make([]string, 0, len(logged))
	for _, l := range logged {
		if l.TS.IsZero() {
			h.t.Fatalf("log line %q has no timestamp", l.Line)
		}
		out = append(out, l.Line)
	}
	return out
}

// requireLine fails unless exactly one logged line contains want.
func requireLine(t *testing.T, lines []string, want string) string {
	t.Helper()
	var found string
	n := 0
	for _, l := range lines {
		if strings.Contains(l, want) {
			found = l
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want exactly one line containing %q, got %d in:\n%s", want, n, strings.Join(lines, "\n"))
	}
	return found
}

// TestRunLogRecordsABackupRun is the test that keeps the run detail view honest:
// a real two-disk VM backup has to explain itself in a handful of lines.
func TestRunLogRecordsABackupRun(t *testing.T) {
	// Collection normally spares chunks younger than a day, which every chunk in
	// a test is. Disable the window so this test stays about the log lines a
	// pruning run produces; the window itself is covered by the engine tests and
	// by TestUnsuccessfulRunDoesNotCollectOrphanChunks.
	t.Setenv(GCGraceEnv, "0")
	h := newRunLogHarness(t)
	ctx := context.Background()

	job, err := h.st.CreateJob(ctx, &store.Job{
		Name: "nightly-vms", Kind: store.SourceVM, TargetID: h.target.ID,
		Schedule: store.ManualSchedule(), Retention: 1, Enabled: true,
		Sources: store.JobSources{{HostID: h.host.ID, VMID: 100, Name: "web-01"}},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	first := h.runJob(job.ID)
	if first.Status != store.RunSuccess {
		t.Fatalf("first run finished %q: %s", first.Status, first.Error)
	}
	lines := h.lines(first.ID)

	requireLine(t, lines, `vm run queued for "nightly-vms"`)
	requireLine(t, lines, "run started")
	requireLine(t, lines, "backing up 1 VM: web-01")
	// The source-start line says where the bytes come from, which is the first
	// thing an operator needs when a run is slow or broken.
	requireLine(t, lines, "web-01: starting 2 disks via Proxmox disk export on node pve1")
	// web-01 has two 16 MiB disks and nothing is on the target yet.
	done := requireLine(t, lines, "web-01: finished")
	for _, want := range []string{"32.0 MiB processed", "32.0 MiB uploaded", "0% deduplicated", "full restore point"} {
		if !strings.Contains(done, want) {
			t.Errorf("source completion line %q does not mention %q", done, want)
		}
	}
	terminal := requireLine(t, lines, "run succeeded")
	for _, want := range []string{"32.0 MiB processed", "32.0 MiB uploaded"} {
		if !strings.Contains(terminal, want) {
			t.Errorf("terminal line %q does not mention %q", terminal, want)
		}
	}
	if last := lines[len(lines)-1]; last != terminal {
		t.Errorf("terminal line is not last: %q", last)
	}
	// One line per event: chunk-level progress must never reach the log.
	if len(lines) > 12 {
		t.Fatalf("a single-VM run produced %d log lines:\n%s", len(lines), strings.Join(lines, "\n"))
	}

	// Change a quarter of scsi0, so the second run is a deduplicating incremental
	// that also prunes (retention 1) and collects the orphaned chunk.
	if _, chunks, err := h.sim.Mutate(100); err != nil || chunks != 1 {
		t.Fatalf("mutate = %d chunks (%v), want 1", chunks, err)
	}
	second := h.runJob(job.ID)
	if second.Status != store.RunSuccess {
		t.Fatalf("second run finished %q: %s", second.Status, second.Error)
	}
	lines = h.lines(second.ID)
	done = requireLine(t, lines, "web-01: finished")
	for _, want := range []string{"32.0 MiB processed", "4.0 MiB uploaded", "88% deduplicated", "incremental restore point"} {
		if !strings.Contains(done, want) {
			t.Errorf("incremental completion line %q does not mention %q", done, want)
		}
	}
	requireLine(t, lines, "web-01: retention pruned 1 restore point (keeping the last 1)")
	requireLine(t, lines, "garbage collection freed 4.0 MiB (1 orphan chunk)")
	requireLine(t, lines, "run succeeded")

	// Each run keeps its own log.
	if got := h.lines(first.ID); len(got) == 0 {
		t.Fatal("the first run's log disappeared when the second one ran")
	}
}

// TestUnsuccessfulRunDoesNotCollectOrphanChunks is the second half of the
// interrupted-backup fix: a run that does not reach its manifests must not
// garbage collect at all. Chunks it (or an earlier interrupted run) uploaded are
// unreferenced by definition, and collecting them makes the retry re-upload
// everything. The grace window is disabled here so the only thing that can keep
// the chunk alive is the failure rule itself.
func TestUnsuccessfulRunDoesNotCollectOrphanChunks(t *testing.T) {
	t.Setenv(GCGraceEnv, "0")
	h := newRunLogHarness(t)
	ctx := context.Background()

	eng, _, err := h.m.engineFor(ctx, h.target.ID)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	// What an interrupted backup leaves on the target: an uploaded, indexed chunk
	// that no manifest references yet.
	orphan := make([]byte, 1<<20)
	for i := range orphan {
		orphan[i] = byte(i * 7)
	}
	sha, uploaded, err := eng.StoreChunk(ctx, orphan)
	if err != nil || uploaded == 0 {
		t.Fatalf("store orphan chunk: %d bytes, %v", uploaded, err)
	}

	// A run that fails before writing any manifest.
	failing, err := h.st.CreateJob(ctx, &store.Job{
		Name: "staging-tagged", Kind: store.SourceVM, TargetID: h.target.ID,
		Schedule: store.ManualSchedule(), Retention: 2, Enabled: true, TagFilter: "staging",
	})
	if err != nil {
		t.Fatalf("create failing job: %v", err)
	}
	run := h.runJob(failing.ID)
	if run.Status != store.RunFailed {
		t.Fatalf("run finished %q, want failed", run.Status)
	}
	requireLine(t, h.lines(run.ID), "skipped garbage collection")

	if has, err := eng.HasChunk(ctx, sha); err != nil || !has {
		t.Fatalf("a failed run collected the chunk an interrupted backup had uploaded: %v, %v", has, err)
	}

	// A run that does succeed still collects it: the fix delays collection, it
	// does not disable it.
	good, err := h.st.CreateJob(ctx, &store.Job{
		Name: "nightly-vms", Kind: store.SourceVM, TargetID: h.target.ID,
		Schedule: store.ManualSchedule(), Retention: 2, Enabled: true,
		Sources: store.JobSources{{HostID: h.host.ID, VMID: 100, Name: "web-01"}},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if ok := h.runJob(good.ID); ok.Status != store.RunSuccess {
		t.Fatalf("backup run finished %q: %s", ok.Status, ok.Error)
	}
	if has, err := eng.HasChunk(ctx, sha); err != nil || has {
		t.Fatalf("a successful run left the orphan chunk behind: %v, %v", has, err)
	}
}

// TestRunLogRecordsAFailure proves the failure a user has to diagnose ends up in
// the log verbatim, not just in the run's error field.
func TestRunLogRecordsAFailure(t *testing.T) {
	h := newRunLogHarness(t)
	ctx := context.Background()

	job, err := h.st.CreateJob(ctx, &store.Job{
		Name: "staging-tagged", Kind: store.SourceVM, TargetID: h.target.ID,
		Schedule: store.ManualSchedule(), Retention: 2, Enabled: true, TagFilter: "staging",
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	run := h.runJob(job.ID)
	if run.Status != store.RunFailed {
		t.Fatalf("run of an unmatched tag filter finished %q", run.Status)
	}
	lines := h.lines(run.ID)
	requireLine(t, lines, `vm run queued for "staging-tagged"`)
	failure := requireLine(t, lines, "run failed")
	if !strings.Contains(failure, `no VMs carry tag "staging"`) {
		t.Fatalf("terminal line = %q, want the full error", failure)
	}
	if !strings.Contains(lines[len(lines)-1], "run failed") {
		t.Fatalf("the failure is not the last line: %+v", lines)
	}
}
