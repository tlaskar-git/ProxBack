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
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"proxback/internal/agentmgr"
	"proxback/internal/engine"
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
	log    *slog.Logger

	cron *cron.Cron

	baseCtx  context.Context
	stopBase context.CancelFunc
	wg       sync.WaitGroup

	gate *gate

	mu           sync.Mutex
	cancels      map[string]context.CancelFunc
	entries      map[string]cron.EntryID
	targetActive map[string]int
}

// New builds a scheduler manager.
func New(st *store.Store, agents *agentmgr.Manager, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{
		st:           st,
		agents:       agents,
		log:          log,
		gate:         newGate(store.DefaultConcurrency),
		cancels:      map[string]context.CancelFunc{},
		entries:      map[string]cron.EntryID{},
		targetActive: map[string]int{},
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
	m.launch(run, func(runCtx context.Context) (*engine.Stats, error) {
		return m.executeBackup(runCtx, run, job)
	})
	return run.ID, nil
}

// VMRestoreTarget is where a VM restore should be written.
type VMRestoreTarget struct {
	HostID string `json:"hostId"`
	Node   string `json:"node"`
	VMID   int    `json:"vmid"`
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
	m.launch(run, func(runCtx context.Context) (*engine.Stats, error) {
		return m.executeRestore(runCtx, run, backup, spec)
	})
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
// records the terminal state of the run.
func (m *Manager) launch(run *store.JobRun, fn func(context.Context) (*engine.Stats, error)) {
	runCtx, cancel := context.WithCancel(m.baseContext())
	m.mu.Lock()
	m.cancels[run.ID] = cancel
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		defer func() {
			m.mu.Lock()
			delete(m.cancels, run.ID)
			m.mu.Unlock()
		}()

		if err := m.gate.Acquire(runCtx); err != nil {
			m.finish(run.ID, nil, err)
			return
		}
		defer m.gate.Release()

		if err := m.st.SetRunStep(m.detached(), run.ID, "Starting"); err != nil {
			m.log.Warn("could not update run step", "run", run.ID, "error", err)
		}
		stats, err := func() (st *engine.Stats, err error) {
			defer func() {
				if p := recover(); p != nil {
					m.log.Error("run panicked", "run", run.ID, "panic", p)
					err = fmt.Errorf("internal error: %v", p)
				}
			}()
			return fn(runCtx)
		}()
		m.finish(run.ID, stats, err)
	}()
}

func (m *Manager) detached() context.Context {
	return context.WithoutCancel(m.baseContext())
}

func (m *Manager) finish(runID string, stats *engine.Stats, runErr error) {
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
	if cur.Kind == store.RunKindRestore {
		// Deduplication is a backup-side concept; a restore never uploads.
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
	if runErr != nil {
		m.log.Error("run finished", "run", runID, "status", status, "error", runErr)
	} else {
		m.log.Info("run finished", "run", runID, "status", status,
			"bytesProcessed", processed, "bytesUploaded", uploaded)
	}
}

// ---------------------------------------------------------------- helpers

func (m *Manager) engineFor(ctx context.Context, targetID string) (*engine.Engine, *store.S3Target, error) {
	target, err := m.st.S3TargetByID(ctx, targetID)
	if err != nil {
		return nil, nil, fmt.Errorf("load backup target: %w", err)
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
	return engine.New(client, target.ID, m.st, m.log), target, nil
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
