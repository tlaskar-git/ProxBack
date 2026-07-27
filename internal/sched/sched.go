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
	"os"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"proxback/internal/agentmgr"
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
)

// ManualSchedule marks a job that is never scheduled automatically.
const ManualSchedule = "manual"

// AgentDispatchTimeout bounds how long a run waits for an agent to pick up work.
const AgentDispatchTimeout = 5 * time.Minute

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
		if !j.Enabled || j.Schedule == "" || j.Schedule == ManualSchedule {
			continue
		}
		jobID := j.ID
		name := j.Name
		spec := j.Schedule
		id, err := m.cron.AddFunc(spec, func() {
			if _, err := m.TriggerJob(m.baseContext(), jobID); err != nil {
				if errors.Is(err, ErrAlreadyRunning) {
					m.log.Warn("scheduled run skipped, already running", "job", name)
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

// ValidateSchedule checks a cron spec (or "manual").
func ValidateSchedule(spec string) error {
	if spec == "" || spec == ManualSchedule {
		return nil
	}
	if _, err := cron.ParseStandard(spec); err != nil {
		return fmt.Errorf("invalid schedule %q: %w", spec, err)
	}
	return nil
}

// NextRun returns the next time a schedule fires, in UTC. It is nil for manual
// schedules, disabled jobs and specs the cron parser rejects — exactly the cases
// where the API contract asks for a null nextRun.
func NextRun(schedule string, enabled bool, now time.Time) *time.Time {
	if !enabled || schedule == "" || schedule == ManualSchedule {
		return nil
	}
	spec, err := cron.ParseStandard(schedule)
	if err != nil {
		return nil
	}
	next := spec.Next(now)
	if next.IsZero() {
		return nil
	}
	utc := next.UTC()
	return &utc
}

func (m *Manager) baseContext() context.Context {
	if m.baseCtx == nil {
		return context.Background()
	}
	return m.baseCtx
}

// ---------------------------------------------------------------- triggering

// TriggerJob starts a job run and returns its id.
func (m *Manager) TriggerJob(ctx context.Context, jobID string) (string, error) {
	job, err := m.st.JobByID(ctx, jobID)
	if err != nil {
		return "", err
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
	m.launch(run, job.Kind, func(runCtx context.Context) (*engine.Stats, error) {
		return m.executeBackup(runCtx, run, job)
	}, nil)
	return run.ID, nil
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
}

// TriggerRestore starts a restore run and returns its id.
func (m *Manager) TriggerRestore(ctx context.Context, spec RestoreSpec) (string, error) {
	backup, err := m.st.BackupByID(ctx, spec.BackupID)
	if err != nil {
		return "", err
	}
	switch {
	case backup.SourceKind == store.SourceVM && spec.VM == nil:
		return "", errors.New("vm restore target required")
	case backup.SourceKind == store.SourceAgent && spec.Agent == nil:
		return "", errors.New("agent restore target required")
	}
	run, err := m.st.CreateRun(ctx, &store.JobRun{
		JobID:       "",
		JobName:     "Restore " + backup.SourceName,
		Kind:        store.RunKindRestore,
		Status:      store.RunRunning,
		CurrentStep: "Queued",
	})
	if err != nil {
		return "", err
	}
	m.launch(run, store.RunKindRestore, func(runCtx context.Context) (*engine.Stats, error) {
		return m.executeRestore(runCtx, run, backup, spec)
	}, nil)
	return run.ID, nil
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
		return m.executeVerify(runCtx, run, backup)
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
	cur, err := m.st.RunByID(ctx, runID)
	if err != nil {
		m.log.Error("could not load run to finish it", "run", runID, "error", err)
		return
	}
	processed, uploaded := cur.BytesProcessed, cur.BytesUploaded
	ratio := cur.DedupRatio
	if stats != nil {
		processed, uploaded = stats.BytesProcessed, stats.BytesUploaded
		ratio = stats.DedupRatio()
	} else if processed > 0 {
		ratio = 1 - float64(uploaded)/float64(processed)
		if ratio < 0 {
			ratio = 0
		}
	}
	if cur.Kind == store.RunKindRestore || cur.Kind == store.RunKindVerify {
		// Deduplication is a backup-side concept; restores and verifies read
		// only, so a "100% deduplicated" ratio would be meaningless.
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
	m.logTerminal(ctx, cur, status, processed, uploaded, ratio, msg)
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
	processed, uploaded int64, ratio float64, msg string) {
	dur := store.Now().Sub(run.StartedAt).Round(time.Millisecond)
	switch status {
	case store.RunSuccess:
		// Deduplication is a backup-side concept, so restores and verifies only
		// report what they read.
		if run.Kind == store.RunKindRestore || run.Kind == store.RunKindVerify {
			m.logRun(ctx, run.ID, "run succeeded in %s — %s processed", dur, humanBytes(processed))
			return
		}
		m.logRun(ctx, run.ID, "run succeeded in %s — %s processed, %s uploaded, %.0f%% deduplicated",
			dur, humanBytes(processed), humanBytes(uploaded), ratio*100)
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

// engineFor builds the engine for a target, reading the throughput settings on
// every call so a change to upload concurrency, compression or the rate limit
// takes effect on the next run without a restart.
func (m *Manager) engineFor(ctx context.Context, targetID string) (*engine.Engine, *store.S3Target, error) {
	target, err := m.st.S3TargetByID(ctx, targetID)
	if err != nil {
		return nil, nil, fmt.Errorf("load backup target: %w", err)
	}
	settings, err := m.st.Settings(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("load settings: %w", err)
	}
	client, err := s3target.New(ctx, s3target.Config{
		Endpoint:  target.Endpoint,
		Region:    target.Region,
		Bucket:    target.Bucket,
		AccessKey: target.AccessKey,
		SecretKey: target.SecretKey,
		PathStyle: target.PathStyle,
	})
	if err != nil {
		return nil, nil, err
	}
	// One token bucket for the whole process: the operator's limit is a limit on
	// what the server does to the uplink, not on what one stream does.
	engine.SetUploadLimitMbps(float64(settings.UploadLimitMbps))
	eng := engine.NewWithOptions(client, target.ID, m.st, m.log, engine.Options{
		UploadConcurrency: settings.UploadConcurrency,
		Compression:       settings.Compression,
		GCGrace:           gcGrace(),
	})
	return eng, target, nil
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
