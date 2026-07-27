package store_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"proxback/internal/store"
)

// legacyIdentitySQL turns a current database back into the shape an
// installation written before cluster identity has on disk: helpers keyed by
// node name alone and restore points that record no host. The columns really
// are dropped, so re-opening exercises the ALTER TABLE upgrade path and not
// just the backfill.
const legacyIdentitySQL = `
DROP INDEX IF EXISTS idx_helpers_host_node;
ALTER TABLE helpers DROP COLUMN host_id;
ALTER TABLE backups DROP COLUMN host_id;
ALTER TABLE backups DROP COLUMN host_name;
ALTER TABLE backups DROP COLUMN last_verified_at;
ALTER TABLE backups DROP COLUMN last_verify_result;
ALTER TABLE backups DROP COLUMN verified_bytes;
`

// TestMigrationGivesExistingRowsAnIdentity covers the upgrade of an installation
// written before ProxBack knew which cluster things belonged to:
//
//   - a node helper keeps working as a row but becomes "unassigned", and is
//     never resolved for routing, because its node name alone cannot say which
//     cluster's machine it is;
//   - a restore point is backfilled from the "<hostId>_<vmid>" source id it
//     already carries, so nothing has to be guessed.
func TestMigrationGivesExistingRowsAnIdentity(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// 1. Write the rows with the current code — the encrypted access secret has
	// to be real for the upgraded database to be readable at all — then strip
	// the identity columns back off.
	seed, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	host, err := seed.CreatePVEHost(ctx, &store.PVEHost{
		ID: "h1", Name: "cluster-a", BaseURL: "https://pve:8006",
		TokenID: "root@pam!pb", TokenSecret: "secret",
	})
	if err != nil {
		t.Fatalf("create host: %v", err)
	}
	if host.ID != "h1" {
		t.Fatalf("host id = %q, want the one the fixture asked for", host.ID)
	}
	if _, err := seed.CreateHelper(ctx, &store.NodeHelper{
		ID: "helper-1", HostID: "h1", Node: "pve1", Address: "10.0.0.11", Port: 8007,
		Version: "0.4.0", AccessSecret: "legacy-access-secret", APIKeyHash: "hash-1",
	}); err != nil {
		t.Fatalf("create helper: %v", err)
	}
	for _, b := range []struct{ id, kind, sourceID, name string }{
		{"b-vm", store.SourceVM, "h1_100", "web-01"},
		{"b-orphan", store.SourceVM, "gone_101", "db-01"},
		{"b-agent", store.SourceAgent, "agent-7", "fileserver"},
	} {
		if _, err := seed.CreateBackup(ctx, &store.Backup{
			ID: b.id, SourceKind: b.kind, SourceID: b.sourceID, SourceName: b.name,
			TargetID: "t1",
		}); err != nil {
			t.Fatalf("create backup %s: %v", b.id, err)
		}
	}
	if _, err := seed.DB().ExecContext(ctx, legacyIdentitySQL); err != nil {
		t.Fatalf("strip the identity columns: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seeded store: %v", err)
	}

	// 2. Re-open with the current code: the migration must run in place.
	st, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatalf("open upgraded store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// 1. The helper survives, unassigned.
	helpers, err := st.ListHelpers(ctx)
	if err != nil || len(helpers) != 1 {
		t.Fatalf("helpers after upgrade = %+v (%v)", helpers, err)
	}
	if helpers[0].HostID != "" || helpers[0].Node != "pve1" {
		t.Fatalf("legacy helper = %+v, want it unassigned but intact", helpers[0])
	}
	// 2. Nothing routes to it: not by the host it might have belonged to, and
	// not by node name — the lookup that could pick the wrong cluster no longer
	// exists.
	if _, err := st.HelperFor(ctx, "h1", "pve1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unassigned helper resolved for routing: %v", err)
	}
	unassigned, err := st.UnassignedHelperForNode(ctx, "pve1")
	if err != nil || unassigned.ID != "helper-1" {
		t.Fatalf("UnassignedHelperForNode = %+v (%v)", unassigned, err)
	}
	// 3. Binding it makes it routable, no redeployment needed.
	if err := st.AssignHelperHost(ctx, "helper-1", "h1"); err != nil {
		t.Fatalf("assign legacy helper: %v", err)
	}
	bound, err := st.HelperFor(ctx, "h1", "pve1")
	if err != nil || bound.ID != "helper-1" || bound.HostID != "h1" {
		t.Fatalf("helper after assignment = %+v (%v)", bound, err)
	}

	// 4. Restore points learned their cluster from the source id prefix. The
	// display name comes from the host row when it is still configured; a point
	// whose host has been removed keeps the id and simply has no name.
	for _, want := range []struct{ id, hostID, hostName string }{
		{"b-vm", "h1", "cluster-a"},
		{"b-orphan", "gone", ""},
		{"b-agent", "", ""},
	} {
		got, err := st.BackupByID(ctx, want.id)
		if err != nil {
			t.Fatalf("load backup %s: %v", want.id, err)
		}
		if got.HostID != want.hostID || got.HostName != want.hostName {
			t.Fatalf("backup %s = host %q/%q, want %q/%q",
				want.id, got.HostID, got.HostName, want.hostID, want.hostName)
		}
		// Verification evidence starts empty rather than claiming anything.
		if got.LastVerifiedAt != nil || got.LastVerifyResult != "" || got.VerifiedBytes != 0 {
			t.Fatalf("backup %s arrived with verification evidence: %+v", want.id, got)
		}
	}
}

func TestHostIDFromSourceID(t *testing.T) {
	for _, c := range []struct {
		sourceID string
		want     string
		ok       bool
	}{
		{"h1_100", "h1", true},
		{"a1b2c3d4_9999", "a1b2c3d4", true},
		{"host_with_underscores_100", "host_with_underscores", true},
		{"agent-7", "", false},
		{"h1_", "", false},
		{"_100", "", false},
		{"h1_abc", "", false},
		{"", "", false},
	} {
		got, ok := store.HostIDFromSourceID(c.sourceID)
		if got != c.want || ok != c.ok {
			t.Errorf("HostIDFromSourceID(%q) = %q, %v; want %q, %v", c.sourceID, got, ok, c.want, c.ok)
		}
	}
}

// TestReductionMetricsAreOneDefinition pins the arithmetic the whole product
// reports. The case that mattered: a run that read 32 MiB and uploaded nothing
// is 100% avoided and has no ratio at all — it used to be shown as both
// "1.0×" and "100% deduplicated" at the same time.
func TestReductionMetricsAreOneDefinition(t *testing.T) {
	for _, c := range []struct {
		what               string
		processed          int64
		uploaded           int64
		wantPct            float64
		wantRatio          float64
		wantRatioAvailable bool
		wantSummary        string
	}{
		{"nothing uploaded", 32 << 20, 0, 100, 0, false, "100% avoided (nothing needed uploading)"},
		{"nothing processed", 0, 0, 0, 0, false, "0% avoided"},
		{"a quarter uploaded", 32 << 20, 8 << 20, 75, 4, true, "75% avoided (4.0× reduction)"},
		{"all uploaded", 16 << 20, 16 << 20, 0, 1, true, "0% avoided"},
		{"compression made it bigger", 100, 120, 0, 100.0 / 120, true, "0% avoided"},
	} {
		t.Run(c.what, func(t *testing.T) {
			if got := store.ReductionPct(c.processed, c.uploaded); math.Abs(got-c.wantPct) > 1e-9 {
				t.Errorf("ReductionPct = %v, want %v", got, c.wantPct)
			}
			ratio, ok := store.ReductionRatio(c.processed, c.uploaded)
			if ok != c.wantRatioAvailable {
				t.Fatalf("ReductionRatio availability = %v, want %v", ok, c.wantRatioAvailable)
			}
			if ok && math.Abs(ratio-c.wantRatio) > 1e-9 {
				t.Errorf("ReductionRatio = %v, want %v", ratio, c.wantRatio)
			}
			if got := store.ReductionSummary(c.processed, c.uploaded); got != c.wantSummary {
				t.Errorf("ReductionSummary = %q, want %q", got, c.wantSummary)
			}
		})
	}
}

// A run's reported metrics always agree with each other, because they all come
// from the byte counters through one function.
func TestRunMetricsAgreeWithEachOther(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)

	run, err := st.CreateRun(ctx, &store.JobRun{JobID: "j1", JobName: "nightly"})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	// A deduplicated re-run: everything read, nothing uploaded.
	if err := st.FinishRun(ctx, run.ID, store.RunSuccess, 32<<20, 0, 1, ""); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	got, err := st.RunByID(ctx, run.ID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if got.ReductionPct != 100 || got.DedupRatio != 1 {
		t.Fatalf("deduplicated run = %.2f%% / %.2f ratio", got.ReductionPct, got.DedupRatio)
	}
	if got.ReductionRatio != nil {
		t.Fatalf("a run that uploaded nothing reported a %.1f× ratio; it is unbounded", *got.ReductionRatio)
	}

	// A restore reads only, so no reduction figure is reported at all — even
	// though it "processed" bytes and "uploaded" none.
	restore, err := st.CreateRun(ctx, &store.JobRun{
		JobName: "Restore web-01", Kind: store.RunKindRestore,
	})
	if err != nil {
		t.Fatalf("create restore run: %v", err)
	}
	if err := st.FinishRun(ctx, restore.ID, store.RunSuccess, 32<<20, 0, 0, ""); err != nil {
		t.Fatalf("finish restore run: %v", err)
	}
	loaded, err := st.RunByID(ctx, restore.ID)
	if err != nil {
		t.Fatalf("load restore run: %v", err)
	}
	if loaded.ReductionPct != 0 || loaded.DedupRatio != 0 || loaded.ReductionRatio != nil {
		t.Fatalf("restore run reported deduplication: %+v", loaded)
	}
}

// Verification evidence lives on the restore point, and says only what it can:
// the stored data was re-read and matched.
func TestRecordBackupVerification(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)

	b, err := st.CreateBackup(ctx, &store.Backup{
		SourceKind: store.SourceVM, SourceID: "h1_100", SourceName: "web-01",
		HostID: "h1", HostName: "cluster-a", TargetID: "t1", SizeBytes: 4096,
	})
	if err != nil {
		t.Fatalf("create backup: %v", err)
	}
	if b.LastVerifiedAt != nil || b.LastVerifyResult != "" {
		t.Fatalf("a new restore point already claims verification: %+v", b)
	}

	at := store.Now()
	if err := st.RecordBackupVerification(ctx, b.ID, at, store.VerifyPassed, 4096); err != nil {
		t.Fatalf("record verification: %v", err)
	}
	got, err := st.BackupByID(ctx, b.ID)
	if err != nil {
		t.Fatalf("load backup: %v", err)
	}
	if got.LastVerifyResult != store.VerifyPassed || got.VerifiedBytes != 4096 {
		t.Fatalf("verified backup = %+v", got)
	}
	if got.LastVerifiedAt == nil || !got.LastVerifiedAt.Equal(at) {
		t.Fatalf("lastVerifiedAt = %v, want %v", got.LastVerifiedAt, at)
	}

	// A later failure overwrites the evidence: what matters is the latest word.
	later := at.Add(time.Hour)
	if err := st.RecordBackupVerification(ctx, b.ID, later, store.VerifyFailed, 0); err != nil {
		t.Fatalf("record failed verification: %v", err)
	}
	got, err = st.BackupByID(ctx, b.ID)
	if err != nil {
		t.Fatalf("reload backup: %v", err)
	}
	if got.LastVerifyResult != store.VerifyFailed || got.VerifiedBytes != 0 {
		t.Fatalf("failed verification = %+v", got)
	}

	if err := st.RecordBackupVerification(ctx, b.ID, later, "maybe", 0); err == nil {
		t.Fatal("an unknown verification result was accepted")
	}
	if err := st.RecordBackupVerification(ctx, "no-such-backup", later, store.VerifyPassed, 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("verifying an unknown backup = %v, want ErrNotFound", err)
	}
}

// The restore destination is persisted with the run, so history can say where a
// VM went without anyone reading a log line.
func TestRestoreMetadataRoundTrips(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)

	run, err := st.CreateRun(ctx, &store.JobRun{
		JobName: "Restore web-01", Kind: store.RunKindRestore,
		Restore: &store.RestoreMeta{
			Mode: store.RestoreOverwrite, HostID: "h1", HostName: "cluster-a",
			Node: "pve1", VMID: 100, Storage: "local-lvm",
		},
	})
	if err != nil {
		t.Fatalf("create restore run: %v", err)
	}
	got, err := st.RunByID(ctx, run.ID)
	if err != nil {
		t.Fatalf("load restore run: %v", err)
	}
	if got.Restore == nil {
		t.Fatal("restore run lost its destination")
	}
	if got.Restore.Mode != store.RestoreOverwrite || got.Restore.VMID != 100 ||
		got.Restore.Node != "pve1" || got.Restore.HostName != "cluster-a" ||
		got.Restore.Storage != "local-lvm" {
		t.Fatalf("restore destination = %+v", got.Restore)
	}

	// A backup run carries none, and reports none.
	backup, err := st.CreateRun(ctx, &store.JobRun{JobID: "j1", JobName: "nightly"})
	if err != nil {
		t.Fatalf("create backup run: %v", err)
	}
	loaded, err := st.RunByID(ctx, backup.ID)
	if err != nil {
		t.Fatalf("load backup run: %v", err)
	}
	if loaded.Restore != nil {
		t.Fatalf("a backup run has a restore destination: %+v", loaded.Restore)
	}
}

// TestScheduleRPO covers the objective each schedule implies — the number a
// workload's staleness is judged against.
func TestScheduleRPO(t *testing.T) {
	for _, c := range []struct {
		what     string
		schedule store.Schedule
		want     time.Duration
		ok       bool
	}{
		{"hourly", store.Schedule{Kind: store.ScheduleHourly, Minute: 30}, time.Hour, true},
		{"daily", store.Schedule{Kind: store.ScheduleDaily, Time: "02:00"}, 24 * time.Hour, true},
		{"weekly", store.Schedule{Kind: store.ScheduleWeekly, Time: "03:00", Weekdays: []int{0}}, 168 * time.Hour, true},
		{"monthly", store.Schedule{Kind: store.ScheduleMonthly, Time: "01:00", DayOfMonth: 1}, 720 * time.Hour, true},
		{"manual", store.ManualSchedule(), 0, false},
		// An advanced expression can mean anything at all, so it promises
		// nothing and no workload is judged stale against it.
		{"advanced", store.Schedule{Kind: store.ScheduleAdvanced, CronExpr: "*/15 * * * *"}, 0, false},
	} {
		got, ok := c.schedule.RPO()
		if got != c.want || ok != c.ok {
			t.Errorf("%s RPO = %v, %v; want %v, %v", c.what, got, ok, c.want, c.ok)
		}
	}
}

func TestRPOGrace(t *testing.T) {
	// A quarter of the objective, never below an hour.
	for rpo, want := range map[time.Duration]time.Duration{
		time.Hour:        time.Hour,
		24 * time.Hour:   6 * time.Hour,
		168 * time.Hour:  42 * time.Hour,
		720 * time.Hour:  180 * time.Hour,
		30 * time.Minute: time.Hour,
	} {
		if got := store.RPOGrace(rpo); got != want {
			t.Errorf("RPOGrace(%v) = %v, want %v", rpo, got, want)
		}
	}
}

// Run sources record which workload they walked, which is what lets protection
// posture judge a guest by its own outcome rather than by the newest success
// anywhere.
func TestLatestSourceOutcomesArePerWorkload(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)

	write := func(runID, sourceID, status string) {
		t.Helper()
		if _, err := st.CreateRun(ctx, &store.JobRun{
			ID: runID, JobID: "j1", JobName: "nightly",
		}); err != nil {
			t.Fatalf("create run %s: %v", runID, err)
		}
		if err := st.ReplaceRunSources(ctx, runID, []store.RunSource{{
			Seq: 0, Name: sourceID, Kind: store.SourceVM, SourceID: sourceID,
			HostID: "h1", HostName: "cluster-a", Node: "pve1",
		}}); err != nil {
			t.Fatalf("plan run %s: %v", runID, err)
		}
		if err := st.FinishRunSource(ctx, runID, 0, status, 10, 10, ""); err != nil {
			t.Fatalf("finish run source %s: %v", runID, err)
		}
		// The finished_at column is what orders the outcomes; the store writes it
		// with millisecond resolution, so runs written back to back need a gap.
		time.Sleep(2 * time.Millisecond)
	}
	write("r1", "h1_100", store.SourceFailed)
	write("r2", "h1_100", store.SourceSuccess)
	write("r3", "h1_101", store.SourceSuccess)
	write("r4", "h1_101", store.SourceFailed)

	outcomes, err := st.LatestSourceOutcomes(ctx)
	if err != nil {
		t.Fatalf("latest source outcomes: %v", err)
	}
	if got := outcomes["h1_100"].Status; got != store.SourceSuccess {
		t.Fatalf("h1_100 latest outcome = %q, want the later success", got)
	}
	if got := outcomes["h1_101"].Status; got != store.SourceFailed {
		t.Fatalf("h1_101 latest outcome = %q, want the later failure", got)
	}
	if outcomes["h1_101"].FinishedAt.IsZero() {
		t.Fatal("a finished outcome has no timestamp")
	}
	if _, ok := outcomes["h1_999"]; ok {
		t.Fatal("a workload that never ran has an outcome")
	}

	// The identity also survives the read path the monitor uses.
	sources, err := st.RunSources(ctx, "r4")
	if err != nil || len(sources) != 1 {
		t.Fatalf("run sources = %+v (%v)", sources, err)
	}
	if sources[0].SourceID != "h1_101" || sources[0].HostName != "cluster-a" || sources[0].HostID != "h1" {
		t.Fatalf("run source lost its workload identity: %+v", sources[0])
	}
}
