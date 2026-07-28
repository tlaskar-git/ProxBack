// Package sched owns job scheduling and execution: a cron scheduler for job
// schedules, a run queue with a global concurrency limit, run cancellation and
// the orchestration of VM (agentless) and agent (file level) backups, restores
// and retention.
package sched

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"proxback/internal/agentmgr"
	"proxback/internal/blobstore"
	"proxback/internal/engine"
	"proxback/internal/helperclient"
	"proxback/internal/notify"
	"proxback/internal/pve"
	"proxback/internal/s3target"
	"proxback/internal/store"
)

// Errors returned by the manager.
var (
	// ErrAlreadyRunning is returned when a job already has a run in progress.
	ErrAlreadyRunning = errors.New("sched: job already running")
	// ErrNotRunning is returned when cancelling a run that is not in progress.
	ErrNotRunning = errors.New("sched: run is not in progress")
	// ErrNoJob is returned when retrying a run that belongs to no job — a
	// restore or a verification, which has nothing to re-run.
	ErrNoJob = errors.New("sched: run has no job to re-run")
)

// Bounding an agent run lives in agentmgr, which can tell waiting-to-be-picked-up
// apart from picked-up-and-working: see agentmgr.DefaultPickupTimeout and
// DefaultStallTimeout. A single deadline here could only ever cap total run
// length, which is what policy.maxDurationMinutes is for.

// maxConcurrency caps the size of the run token pool.
const maxConcurrency = 64

// Manager schedules and executes runs.
type Manager struct {
	st     *store.Store
	agents *agentmgr.Manager
	// helperClient talks to the node helpers that make agentless VM backup work
	// on real Proxmox hosts.
	helperClient *helperclient.Client
	notifier     *notify.Notifier
	log          *slog.Logger

	cron *cron.Cron

	baseCtx  context.Context
	stopBase context.CancelFunc
	wg       sync.WaitGroup

	gate *gate

	mu           sync.Mutex
	cancels      map[string]context.CancelFunc
	entries      map[string]cron.EntryID
	targetActive map[string]int
	// verifying maps a backup id to the id of the verify run in flight for it,
	// so a second verify of the same restore point is rejected.
	verifying map[string]string
	// monitors holds the live per-source and throughput state of the runs in
	// flight, keyed by run id. A run has an entry exactly while it is running.
	monitors map[string]*runMonitor

	// policyMinute is how long one minute of policy time lasts — a retry delay
	// of 5 or a duration limit of 10. It is a minute everywhere except in
	// tests, which shrink it so the behaviour can be exercised in milliseconds
	// instead of in minutes. Zero means a real minute.
	policyMinute time.Duration
	// httpClient carries policy script calls to node helpers. Nil builds the
	// default client.
	httpClient *http.Client
}

// New builds a scheduler manager.
func New(st *store.Store, agents *agentmgr.Manager, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{
		st:           st,
		agents:       agents,
		helperClient: helperclient.New(log),
		notifier:     notify.New(log),
		log:          log,
		gate:         newGate(store.DefaultConcurrency),
		cancels:      map[string]context.CancelFunc{},
		entries:      map[string]cron.EntryID{},
		targetActive: map[string]int{},
		verifying:    map[string]string{},
		monitors:     map[string]*runMonitor{},
	}
	m.cron = cron.New(cron.WithLogger(cron.DiscardLogger))
	return m
}

// Start initialises the scheduler. The context bounds every run it launches.
func (m *Manager) Start(ctx context.Context) error {
	m.baseCtx, m.stopBase = context.WithCancel(context.WithoutCancel(ctx))
	if err := m.st.MarkOrphanRunsFailed(ctx); err != nil {
		return err
	}
	// Their per-source rows go with them: nothing may still look like it is
	// backing up after the process that was doing it has gone.
	if err := m.st.SkipOrphanRunSources(ctx); err != nil {
		return err
	}
	settings, err := m.st.Settings(ctx)
	if err != nil {
		return err
	}
	m.SetConcurrency(settings.Concurrency)
	if err := m.ReloadSchedules(ctx); err != nil {
		return err
	}
	m.cron.Start()
	m.log.Info("scheduler started", "concurrency", settings.Concurrency)
	return nil
}

// Stop cancels in-flight runs and waits for them to unwind.
func (m *Manager) Stop() {
	cronCtx := m.cron.Stop()
	<-cronCtx.Done()
	if m.stopBase != nil {
		m.stopBase()
	}
	m.mu.Lock()
	for _, cancel := range m.cancels {
		cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// SetConcurrency adjusts the global run concurrency limit.
func (m *Manager) SetConcurrency(n int) {
	if n <= 0 {
		n = store.DefaultConcurrency
	}
	if n > maxConcurrency {
		n = maxConcurrency
	}
	m.gate.SetLimit(n)
}

// Concurrency reports the configured limit.
func (m *Manager) Concurrency() int { return m.gate.Limit() }

// ReloadSchedules rebuilds the cron entries from the job table.
func (m *Manager) ReloadSchedules(ctx context.Context) error {
	jobs, err := m.st.ListJobs(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	for _, id := range m.entries {
		m.cron.Remove(id)
	}
	m.entries = map[string]cron.EntryID{}
	m.mu.Unlock()

	for _, j := range jobs {
		spec := j.Schedule.Cron()
		if !j.Enabled || spec == "" {
			continue
		}
		jobID := j.ID
		name := j.Name
		schedule := j.Schedule
		id, err := m.cron.AddFunc(spec, func() {
			// A monthly "last day" schedule fires on every day that could be the
			// last one; ShouldRun discards the ones that are not.
			if !schedule.ShouldRun(time.Now()) {
				return
			}
			if _, err := m.TriggerScheduledJob(m.baseContext(), jobID); err != nil {
				if errors.Is(err, ErrAlreadyRunning) {
					m.log.Warn("scheduled run skipped, already running", "job", name)
					return
				}
				if errors.Is(err, ErrOutsideWindow) {
					// The window is a deliberate operator setting, not a fault:
					// the reason is recorded and the next firing is tried.
					m.log.Info("scheduled run skipped, outside the job's backup window",
						"job", name, "reason", err)
					return
				}
				m.log.Error("scheduled run failed to start", "job", name, "error", err)
			}
		})
		if err != nil {
			m.log.Error("invalid job schedule", "job", name, "schedule", spec, "error", err)
			continue
		}
		m.mu.Lock()
		m.entries[jobID] = id
		m.mu.Unlock()
	}
	return nil
}

// nextRunProbes bounds the search for the next real firing. Only the monthly
// last-day schedule ever skips a candidate, and it can skip at most the three
// days between the 28th and the month's last day.
const nextRunProbes = 8

// NextRun returns the next time a schedule fires, in UTC. It is nil for manual
// schedules, disabled jobs and schedules whose expression the cron parser
// rejects — exactly the cases where the API contract asks for a null nextRun.
//
// Times are computed in the server's local zone, which is what the structured
// schedule means by "02:00"; the answer is converted to UTC for the wire.
func NextRun(schedule store.Schedule, enabled bool, now time.Time) *time.Time {
	spec := schedule.Cron()
	if !enabled || spec == "" {
		return nil
	}
	parsed, err := cron.ParseStandard(spec)
	if err != nil {
		return nil
	}
	next := now.Local()
	for i := 0; i < nextRunProbes; i++ {
		next = parsed.Next(next)
		if next.IsZero() {
			return nil
		}
		if schedule.ShouldRun(next) {
			utc := next.UTC()
			return &utc
		}
	}
	return nil
}

func (m *Manager) baseContext() context.Context {
	if m.baseCtx == nil {
		return context.Background()
	}
	return m.baseCtx
}

// ---------------------------------------------------------------- triggering

// TriggerJob starts a job run on an operator's request and returns its id. A
// manual run is never refused by the job's backup window: the window says when
// ProxBack may start a run by itself, not when a person may.
func (m *Manager) TriggerJob(ctx context.Context, jobID string) (string, error) {
	return m.trigger(ctx, jobID, TriggerManual)
}

// TriggerScheduledJob starts a job run from its schedule. It is the only path
// the backup window can refuse, answering ErrOutsideWindow.
func (m *Manager) TriggerScheduledJob(ctx context.Context, jobID string) (string, error) {
	return m.trigger(ctx, jobID, TriggerScheduled)
}

func (m *Manager) trigger(ctx context.Context, jobID, origin string) (string, error) {
	job, err := m.st.JobByID(ctx, jobID)
	if err != nil {
		return "", err
	}
	policy := job.Policy.Normalized()
	// The window governs starting, in the server's local time — which is what
	// the schedule's "02:00" means too.
	allowed, note := windowCheck(policy, origin, time.Now())
	if !allowed {
		return "", fmt.Errorf("%w: %s: %s", ErrOutsideWindow, job.Name, note)
	}
	running, err := m.st.HasRunningRun(ctx, jobID)
	if err != nil {
		return "", err
	}
	if running {
		return "", ErrAlreadyRunning
	}
	run, err := m.st.CreateRun(ctx, &store.JobRun{
		JobID:       job.ID,
		JobName:     job.Name,
		Kind:        store.RunKindBackup,
		Status:      store.RunRunning,
		CurrentStep: "Queued",
	})
	if err != nil {
		return "", err
	}
	if note != "" {
		m.logRun(ctx, run.ID, "%s", note)
	}
	m.launch(run, job.Kind, func(runCtx context.Context) (*engine.Stats, error) {
		return m.executeBackup(runCtx, run, job)
	}, nil)
	return run.ID, nil
}

// RetryRun re-runs the job a finished (or failed) run belongs to, through the
// normal trigger path — so it honours the concurrency limit, the run queue and
// the job's current definition rather than replaying the old run. Restore and
// verification runs have no job behind them and answer ErrNoJob.
func (m *Manager) RetryRun(ctx context.Context, runID string) (string, error) {
	run, err := m.st.RunByID(ctx, runID)
	if err != nil {
		return "", err
	}
	if run.JobID == "" {
		return "", ErrNoJob
	}
	return m.TriggerJob(ctx, run.JobID)
}

// VMRestoreTarget is where a VM restore should be written.
type VMRestoreTarget struct {
	HostID string `json:"hostId"`
	Node   string `json:"node"`
	VMID   int    `json:"vmid"`
	// Storage overrides where a helper-backed restore places the guest's disks
	// (qmrestore --storage). Empty leaves the choice recorded in the archive.
	// It has no effect on legacy per-disk restore points.
	Storage string `json:"storage,omitempty"`
}

// AgentRestoreTarget is where an agent restore should be written.
type AgentRestoreTarget struct {
	AgentID  string `json:"agentId"`
	DestPath string `json:"destPath"`
}

// RestoreSpec is the body of POST /api/restores.
type RestoreSpec struct {
	BackupID string              `json:"backupId"`
	VM       *VMRestoreTarget    `json:"vm,omitempty"`
	Agent    *AgentRestoreTarget `json:"agent,omitempty"`
	// Mode is "alongside" (the default) or "overwrite". A request that does not
	// say is treated as alongside: a restore must never destroy a running guest
	// because a field was left out.
	Mode string `json:"mode,omitempty"`
	// ConfirmName must equal the destination guest's current name for an
	// overwrite. It is the operator typing out what they are about to destroy.
	ConfirmName string `json:"confirmName,omitempty"`
}

// Restore-safety errors. They are separate values because the API answers 409
// for them: the request was well formed, the estate says no.
var (
	// ErrVMIDInUse is returned when an "alongside" restore targets a VMID that
	// already exists on the destination host.
	ErrVMIDInUse = errors.New("sched: the target VMID already exists on this host")
	// ErrConfirmName is returned when an "overwrite" restore does not carry the
	// destination guest's current name.
	ErrConfirmName = errors.New("sched: overwrite needs a matching confirmName")
	// ErrBadRestoreMode is returned for an unrecognised mode.
	ErrBadRestoreMode = errors.New(`sched: mode must be "alongside" or "overwrite"`)
)

// restorePlan is the checked destination of a restore: what the run will do and
// where, resolved before the run row exists so an unsafe request is refused
// synchronously rather than becoming a failed run in the history.
type restorePlan struct {
	meta  store.RestoreMeta
	node  string
	vmid  int
	force bool
}

// planVMRestore resolves and safety-checks a VM restore destination.
//
//   - alongside (the default): the VMID must not exist on that host. Restoring
//     next to the original is the safe operation, so it is what a request that
//     says nothing gets.
//   - overwrite: the VMID must exist and confirmName must be its current name.
//     Nothing else unlocks it.
func (m *Manager) planVMRestore(ctx context.Context, backup *store.Backup, spec RestoreSpec) (*restorePlan, error) {
	if spec.VM == nil {
		return nil, errors.New("vm restore target required")
	}
	mode := spec.Mode
	if mode == "" {
		mode = store.RestoreAlongside
	}
	if !store.ValidRestoreMode(mode) {
		return nil, ErrBadRestoreMode
	}
	hostID := spec.VM.HostID
	if hostID == "" {
		hostID = backup.HostID
	}
	if hostID == "" {
		return nil, errors.New("vm restore needs a hostId")
	}
	host, err := m.st.PVEHostByID(ctx, hostID)
	if err != nil {
		return nil, fmt.Errorf("load proxmox host: %w", err)
	}
	client, err := PVEClient(host)
	if err != nil {
		return nil, err
	}
	vmid := spec.VM.VMID
	if vmid == 0 {
		if vmid, err = vmidFromSourceID(backup.SourceID); err != nil {
			return nil, err
		}
	}
	vms, err := client.AllVMs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list guests on %s: %w", host.Name, err)
	}
	var existing *pve.VM
	for i := range vms {
		if vms[i].VMID == vmid {
			existing = &vms[i]
			break
		}
	}
	plan := &restorePlan{
		vmid: vmid,
		node: spec.VM.Node,
		meta: store.RestoreMeta{
			Mode: mode, HostID: host.ID, HostName: host.Name,
			Node: spec.VM.Node, VMID: vmid, Storage: spec.VM.Storage,
		},
	}
	switch mode {
	case store.RestoreAlongside:
		if existing != nil {
			return nil, fmt.Errorf("%w: vm %d (%s) already exists on %s/%s — pick a free VMID "+
				"(GET /api/hosts/%s/free-vmid suggests one) or restore with mode \"overwrite\"",
				ErrVMIDInUse, vmid, existing.Name, host.Name, existing.Node, host.ID)
		}
	case store.RestoreOverwrite:
		if existing == nil {
			return nil, fmt.Errorf("%w: vm %d does not exist on %s, so there is nothing to overwrite — "+
				"restore it alongside instead", ErrConfirmName, vmid, host.Name)
		}
		if strings.TrimSpace(spec.ConfirmName) != existing.Name {
			return nil, fmt.Errorf("%w: type the destination guest's current name (%q) to confirm "+
				"overwriting vm %d on %s/%s", ErrConfirmName, existing.Name, vmid, host.Name, existing.Node)
		}
		plan.force = true
	}
	if plan.node == "" && existing != nil {
		plan.node = existing.Node
	}
	plan.meta.Node = plan.node
	return plan, nil
}

// TriggerRestore starts a restore run and returns its id. The destination is
// checked — and persisted — before the run exists, so an unsafe restore is
// refused rather than started.
func (m *Manager) TriggerRestore(ctx context.Context, spec RestoreSpec) (string, error) {
	backup, err := m.st.BackupByID(ctx, spec.BackupID)
	if err != nil {
		return "", err
	}
	var plan *restorePlan
	switch backup.SourceKind {
	case store.SourceVM:
		if plan, err = m.planVMRestore(ctx, backup, spec); err != nil {
			return "", err
		}
	case store.SourceAgent:
		if spec.Agent == nil {
			return "", errors.New("agent restore target required")
		}
		mode := spec.Mode
		if mode == "" {
			mode = store.RestoreAlongside
		}
		if !store.ValidRestoreMode(mode) {
			return "", ErrBadRestoreMode
		}
		plan = &restorePlan{meta: store.RestoreMeta{
			Mode: mode, AgentID: spec.Agent.AgentID, DestPath: spec.Agent.DestPath,
		}}
	}
	meta := plan.meta
	run, err := m.st.CreateRun(ctx, &store.JobRun{
		JobID:       "",
		JobName:     "Restore " + backup.SourceName,
		Kind:        store.RunKindRestore,
		Status:      store.RunRunning,
		CurrentStep: "Queued",
		Restore:     &meta,
	})
	if err != nil {
		return "", err
	}
	resolved := *plan
	m.launch(run, store.RunKindRestore, func(runCtx context.Context) (*engine.Stats, error) {
		return m.executeRestore(runCtx, run, backup, spec, resolved)
	}, nil)
	return run.ID, nil
}

// FreeVMID returns the lowest guest id at or above after that no guest in used
// occupies. Proxmox reserves everything below 100, so that is the floor
// whatever the caller asks for.
func FreeVMID(used []int, after int) int {
	const firstUserVMID = 100
	if after < firstUserVMID {
		after = firstUserVMID
	}
	taken := make(map[int]struct{}, len(used))
	for _, id := range used {
		taken[id] = struct{}{}
	}
	for id := after; ; id++ {
		if _, busy := taken[id]; !busy {
			return id
		}
	}
}

// FreeVMIDForHost asks a Proxmox host for its guest list and returns the lowest
// free id at or above after.
func (m *Manager) FreeVMIDForHost(ctx context.Context, hostID string, after int) (int, error) {
	host, err := m.st.PVEHostByID(ctx, hostID)
	if err != nil {
		return 0, err
	}
	client, err := PVEClient(host)
	if err != nil {
		return 0, err
	}
	vms, err := client.AllVMs(ctx)
	if err != nil {
		return 0, fmt.Errorf("list guests on %s: %w", host.Name, err)
	}
	used := make([]int, 0, len(vms))
	for _, v := range vms {
		used = append(used, v.VMID)
	}
	return FreeVMID(used, after), nil
}

// Verify starts a restore-point verification run and returns its id. The run
// goes through the normal queue, so it honours the concurrency limit and can be
// cancelled like any other run.
func (m *Manager) Verify(ctx context.Context, backupID string) (string, error) {
	backup, err := m.st.BackupByID(ctx, backupID)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	if _, busy := m.verifying[backup.ID]; busy {
		m.mu.Unlock()
		return "", ErrAlreadyRunning
	}
	// Reserve the slot before the row exists so two concurrent requests cannot
	// both get through.
	m.verifying[backup.ID] = ""
	m.mu.Unlock()

	release := func() {
		m.mu.Lock()
		delete(m.verifying, backup.ID)
		m.mu.Unlock()
	}
	run, err := m.st.CreateRun(ctx, &store.JobRun{
		JobID:       "",
		JobName:     "Verify " + backup.SourceName,
		Kind:        store.RunKindVerify,
		Status:      store.RunRunning,
		CurrentStep: "Queued",
	})
	if err != nil {
		release()
		return "", err
	}
	m.mu.Lock()
	m.verifying[backup.ID] = run.ID
	m.mu.Unlock()

	m.launch(run, store.RunKindVerify, func(runCtx context.Context) (*engine.Stats, error) {
		stats, err := m.executeVerify(runCtx, run, backup)
		// The evidence belongs to the restore point, whichever way it went.
		m.recordVerification(backup.ID, stats, err)
		return stats, err
	}, release)
	return run.ID, nil
}

// Cancel cancels an in-flight run.
func (m *Manager) Cancel(runID string) error {
	m.mu.Lock()
	cancel, ok := m.cancels[runID]
	m.mu.Unlock()
	if !ok {
		return ErrNotRunning
	}
	cancel()
	return nil
}

// launch runs fn on a worker goroutine, gated by the concurrency limit, and
// records the terminal state of the run. notifyKind is the run kind reported to
// the notification webhook ("vm", "agent", "restore" or "verify"). onDone, when
// non-nil, is invoked once the run has left the queue by any path.
func (m *Manager) launch(run *store.JobRun, notifyKind string, fn func(context.Context) (*engine.Stats, error), onDone func()) {
	runCtx, cancel := context.WithCancel(m.baseContext())
	m.mu.Lock()
	m.cancels[run.ID] = cancel
	m.monitors[run.ID] = newRunMonitor(m.st, m.log, run.ID)
	m.mu.Unlock()

	m.logRun(runCtx, run.ID, "%s run queued for %q", notifyKind, run.JobName)

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		if onDone != nil {
			defer onDone()
		}
		defer func() {
			m.mu.Lock()
			delete(m.cancels, run.ID)
			m.mu.Unlock()
		}()

		if err := m.gate.Acquire(runCtx); err != nil {
			m.finish(run.ID, notifyKind, nil, err)
			return
		}
		defer m.gate.Release()

		if err := m.st.SetRunStep(m.detached(), run.ID, "Starting"); err != nil {
			m.log.Warn("could not update run step", "run", run.ID, "error", err)
		}
		m.logRun(runCtx, run.ID, "run started")
		stats, err := func() (st *engine.Stats, err error) {
			defer func() {
				if p := recover(); p != nil {
					m.log.Error("run panicked", "run", run.ID, "panic", p)
					err = fmt.Errorf("internal error: %v", p)
				}
			}()
			return fn(runCtx)
		}()
		m.finish(run.ID, notifyKind, stats, err)
	}()
}

func (m *Manager) detached() context.Context {
	return context.WithoutCancel(m.baseContext())
}

func (m *Manager) finish(runID, notifyKind string, stats *engine.Stats, runErr error) {
	ctx := m.detached()
	// The run is over: nothing it never reached may stay pending, and the live
	// throughput sample goes with the monitor (0 once the run is not running).
	m.mu.Lock()
	mon := m.monitors[runID]
	delete(m.monitors, runID)
	m.mu.Unlock()
	if mon != nil {
		mon.closeOut(ctx)
	}
	cur, err := m.st.RunByID(ctx, runID)
	if err != nil {
		m.log.Error("could not load run to finish it", "run", runID, "error", err)
		return
	}
	processed, uploaded := cur.BytesProcessed, cur.BytesUploaded
	if stats != nil {
		processed, uploaded = stats.BytesProcessed, stats.BytesUploaded
	}
	// One definition of data reduction for the whole product: the stored ratio
	// is always store.ReductionPct expressed as a fraction, never a separately
	// computed number that could disagree with what the API reports.
	ratio := store.ReductionPct(processed, uploaded) / 100
	if cur.Kind == store.RunKindRestore || cur.Kind == store.RunKindVerify {
		// Deduplication is a backup-side concept; restores and verifies read
		// only, so any reduction figure would be meaningless.
		ratio = 0
	}
	status := store.RunSuccess
	msg := ""
	switch {
	case runErr == nil:
	case errors.Is(runErr, context.Canceled):
		status = store.RunCanceled
		msg = "canceled"
	default:
		status = store.RunFailed
		msg = runErr.Error()
	}
	if err := m.st.FinishRun(ctx, runID, status, processed, uploaded, ratio, msg); err != nil {
		m.log.Error("could not finish run", "run", runID, "error", err)
	}
	m.logTerminal(ctx, cur, status, processed, uploaded, msg)
	if runErr != nil {
		m.log.Error("run finished", "run", runID, "status", status, "error", runErr)
	} else {
		m.log.Info("run finished", "run", runID, "status", status,
			"bytesProcessed", processed, "bytesUploaded", uploaded)
	}
	m.notifyFinished(cur, notifyKind, notify.Payload{
		Event:          notify.EventRunFinished,
		Job:            cur.JobName,
		Kind:           notifyKind,
		Status:         status,
		BytesProcessed: processed,
		BytesUploaded:  uploaded,
		DedupRatio:     ratio,
		Error:          msg,
		StartedAt:      cur.StartedAt,
	})
}

// ---------------------------------------------------------------- run log

// logRun appends one line to a run's persisted activity log, which is what the
// UI shows when an operator opens a run. Logging must never be able to fail a
// run, so every error is logged and swallowed; the context is detached from
// cancellation so the cancellation and failure lines still get written.
func (m *Manager) logRun(ctx context.Context, runID, format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if err := m.st.AppendRunLog(context.WithoutCancel(ctx), runID, line); err != nil {
		m.log.Warn("could not append run log line", "run", runID, "error", err)
	}
}

// logTerminal writes the last line of a run: the success summary, the
// cancellation note or the full error.
func (m *Manager) logTerminal(ctx context.Context, run *store.JobRun, status string,
	processed, uploaded int64, msg string) {
	dur := store.Now().Sub(run.StartedAt).Round(time.Millisecond)
	switch status {
	case store.RunSuccess:
		// Deduplication is a backup-side concept, so restores and verifies only
		// report what they read.
		if run.Kind == store.RunKindRestore || run.Kind == store.RunKindVerify {
			m.logRun(ctx, run.ID, "run succeeded in %s — %s processed", dur, humanBytes(processed))
			return
		}
		m.logRun(ctx, run.ID, "run succeeded in %s — %s processed, %s uploaded, %s",
			dur, humanBytes(processed), humanBytes(uploaded),
			store.ReductionSummary(processed, uploaded))
	case store.RunCanceled:
		m.logRun(ctx, run.ID, "run canceled after %s", dur)
	default:
		m.logRun(ctx, run.ID, "run failed after %s: %s", dur, msg)
	}
}

// countNoun renders a count with its noun pluralised, e.g. "1 VM" / "2 VMs".
func countNoun(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// humanBytes formats a size the way the run log shows it, e.g. 4.0 MiB.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := [...]string{"KiB", "MiB", "GiB", "TiB"}
	v, i := float64(n), -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// notifyFinished fires the run webhook when the configured policy matches.
// Delivery happens on its own goroutine so a slow or dead endpoint can never
// hold up a run, and every failure is logged rather than propagated.
func (m *Manager) notifyFinished(run *store.JobRun, notifyKind string, payload notify.Payload) {
	ctx := m.detached()
	settings, err := m.st.Settings(ctx)
	if err != nil {
		m.log.Warn("could not read settings for run notification", "run", run.ID, "error", err)
		return
	}
	if settings.WebhookURL == "" {
		return
	}
	switch settings.NotifyOn {
	case store.NotifyAll:
	case store.NotifyFailures:
		if payload.Status == store.RunSuccess {
			return
		}
	default: // "off" or anything unrecognised
		return
	}
	if notifyKind == "" {
		notifyKind = store.RunKindBackup
		payload.Kind = notifyKind
	}
	payload.Server = settings.ServerName
	payload.FinishedAt = store.Now()
	if !payload.StartedAt.IsZero() {
		payload.DurationSec = payload.FinishedAt.Sub(payload.StartedAt).Seconds()
	}
	url := settings.WebhookURL
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.notifier.Notify(m.detached(), url, payload)
	}()
}

// ---------------------------------------------------------------- helpers

// GCGraceEnv can shorten the orphan-collection grace window. It exists for the
// end-to-end suite and for operators who knowingly want tighter collection;
// "0" disables the grace period entirely. Anything unparseable is ignored.
const GCGraceEnv = "PROXBACK_GC_GRACE"

// gcGrace resolves the grace window that protects recently uploaded chunks from
// orphan collection.
func gcGrace() time.Duration {
	raw := strings.TrimSpace(os.Getenv(GCGraceEnv))
	if raw == "" {
		return engine.DefaultGCGrace
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return engine.DefaultGCGrace
	}
	if d == 0 {
		// engine.Options reads a zero grace as "use the default", so ask for the
		// disabled behaviour explicitly.
		return -1
	}
	return d
}

// engineFor builds the engine for a target under the global throughput
// settings. Restores, verifications and ad-hoc deletions use it; a job run goes
// through engineForJob so its policy can raise or lower the ceiling.
func (m *Manager) engineFor(ctx context.Context, targetID string) (*engine.Engine, *store.S3Target, error) {
	return m.engineForJob(ctx, targetID, 0)
}

// engineForJob builds the engine for a target, reading the throughput settings
// on every call so a change to upload concurrency, compression or the rate
// limit takes effect on the next run without a restart.
//
// uploadLimitOverride is the job policy's per-job ceiling in Mbps; 0 inherits
// the global one. The rate limiter is one process-wide token bucket by design
// — an operator's limit is a limit on what the server does to the uplink, not
// on what one stream does — so an override is in force for as long as the run
// that set it lasts, and applyGlobalUploadLimit puts the global ceiling back
// when it finishes. Two concurrent runs with different overrides therefore see
// the ceiling of whichever started last; that is the honest consequence of a
// server-wide bucket, and the global limit is always restored.
func (m *Manager) engineForJob(ctx context.Context, targetID string, uploadLimitOverride int) (*engine.Engine, *store.S3Target, error) {
	target, err := m.st.S3TargetByID(ctx, targetID)
	if err != nil {
		return nil, nil, fmt.Errorf("load backup target: %w", err)
	}
	settings, err := m.st.Settings(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("load settings: %w", err)
	}
	bs, err := StoreForTarget(ctx, target)
	if err != nil {
		return nil, nil, err
	}
	// One token bucket for the whole process: the operator's limit is a limit on
	// what the server does to the uplink, not on what one stream does.
	limit := settings.UploadLimitMbps
	if uploadLimitOverride > 0 {
		limit = uploadLimitOverride
	}
	engine.SetUploadLimitMbps(float64(limit))
	eng := engine.NewWithOptions(bs, target.ID, m.st, m.log, engine.Options{
		UploadConcurrency: settings.UploadConcurrency,
		Compression:       settings.Compression,
		GCGrace:           gcGrace(),
	})
	return eng, target, nil
}

// applyGlobalUploadLimit puts the server-wide upload ceiling back in force
// after a run that raised or lowered it for itself.
func (m *Manager) applyGlobalUploadLimit(ctx context.Context) {
	settings, err := m.st.Settings(ctx)
	if err != nil {
		m.log.Warn("could not restore the global upload limit", "error", err)
		return
	}
	engine.SetUploadLimitMbps(float64(settings.UploadLimitMbps))
}

// StoreForTarget opens the storage a target's kind implies: an S3 client for
// object storage, a filesystem store for a mounted path. It is the single place
// where a target's kind is turned into behaviour — the engine, the scheduler and
// the API all work with the resulting blobstore.Store and never branch on kind
// again.
func StoreForTarget(ctx context.Context, t *store.S3Target) (blobstore.Store, error) {
	if t.IsFilesystem() {
		return blobstore.NewFilesystem(t.Path)
	}
	return s3target.New(ctx, s3target.Config{
		Endpoint:  t.Endpoint,
		Region:    t.Region,
		Bucket:    t.Bucket,
		AccessKey: t.AccessKey,
		SecretKey: t.SecretKey,
		PathStyle: t.PathStyle,
	})
}

// PVEClient builds a client for a stored Proxmox host.
func PVEClient(h *store.PVEHost) (*pve.Client, error) {
	return pve.New(pve.Config{
		BaseURL:     h.BaseURL,
		TokenID:     h.TokenID,
		TokenSecret: h.TokenSecret,
		InsecureTLS: h.InsecureTLS,
	})
}

// VMSourceID is the stable source identifier of a VM backup source.
func VMSourceID(hostID string, vmid int) string {
	return fmt.Sprintf("%s_%d", hostID, vmid)
}

func (m *Manager) markTargetActive(targetID string) {
	m.mu.Lock()
	m.targetActive[targetID]++
	m.mu.Unlock()
}

// markTargetIdle decrements the per-target active counter and reports whether the
// target became idle (so it is safe to garbage collect orphan chunks).
func (m *Manager) markTargetIdle(targetID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.targetActive[targetID]--
	if m.targetActive[targetID] <= 0 {
		delete(m.targetActive, targetID)
		return true
	}
	return false
}
