// Package agentmgr implements the server side of the in-guest agent protocol:
// enrollment tokens, registration, heartbeat/online tracking, poll-based job
// dispatch and the chunk upload / manifest complete / restore streaming
// endpoints the agent drives.
package agentmgr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"proxback/internal/auth"
	"proxback/internal/engine"
	"proxback/internal/store"
)

// OnlineWindow is how recently an agent must have been seen to count as online.
const OnlineWindow = 45 * time.Second

// Bounds on an agent-driven run.
//
// These are two different questions and they get two different answers. Waiting
// for an agent to collect a dispatch is bounded tightly, because an agent that
// has not taken the work within a few heartbeats is not going to. Once it has
// the work, how long the backup legitimately takes is a property of the data,
// not of the protocol — a first full backup of a large volume runs for hours —
// so the only failure worth detecting is the agent going quiet.
//
// A single deadline over the whole run cannot express that. One did, and it
// failed healthy backups at five minutes with "context deadline exceeded"
// however much progress they were making. The operator-facing cap on run length
// is policy.maxDurationMinutes, which is configurable and says so when it fires.
const (
	// DefaultPickupTimeout bounds the wait for an agent to collect a dispatch.
	// The agent polls every 15s, so this is many chances to be seen.
	DefaultPickupTimeout = 5 * time.Minute
	// DefaultStallTimeout bounds how long a collected run may report nothing.
	// It has to clear the quiet stretch before the first chunk, when the agent
	// is still walking the filesystem, which on a large volume is minutes.
	DefaultStallTimeout = 30 * time.Minute
)

// Errors returned by the manager.
var (
	ErrUnauthorized = errors.New("agentmgr: unauthorized")
	ErrUnknownRun   = errors.New("agentmgr: unknown or finished run")
	ErrBadToken     = errors.New("agentmgr: invalid or expired enrollment token")
	// ErrAgentBusy refuses a self-update to an agent that has work in flight.
	// Applying an update restarts the agent, and a restart in the middle of a
	// backup throws away everything the guest has uploaded so far.
	ErrAgentBusy = errors.New("agentmgr: the agent has a run in flight")
	// ErrPickupTimeout and ErrStalled replace a bare deadline error with the
	// distinction that matters when reading a failed run: the agent never took
	// the work, or it took it and went quiet.
	ErrPickupTimeout = errors.New("agentmgr: the agent did not collect this run")
	ErrStalled       = errors.New("agentmgr: the agent stopped reporting progress")
)

// Dispatch is a work item handed to an agent on heartbeat.
//
// The protection-policy fields travel with the work because the work happens
// inside the guest: the file walk, the exclusions and the pre/post scripts all
// run where the files are, and the server has no vantage point from which to
// apply them. An agent that predates them ignores them, which is why the run
// log records them as sent rather than as applied.
type Dispatch struct {
	RunID     string   `json:"runId"`
	Type      string   `json:"type"` // backup | restore
	JobID     string   `json:"jobId,omitempty"`
	JobName   string   `json:"jobName,omitempty"`
	Paths     []string `json:"paths,omitempty"`
	DestPath  string   `json:"destPath,omitempty"`
	ChunkSize int64    `json:"chunkSize"`
	// ExcludePaths are glob patterns the walk must skip. The syntax is
	// store.MatchGlob's: filepath.Match per segment, plus "**" for any number of
	// segments.
	ExcludePaths []string `json:"excludePaths,omitempty"`
	// PreScript runs before the walk starts and PostScript after it finishes;
	// a failing pre-script must abort the backup before any data is read.
	PreScript  string `json:"preScript,omitempty"`
	PostScript string `json:"postScript,omitempty"`
	// ScriptTimeoutSeconds bounds each of them.
	ScriptTimeoutSeconds int `json:"scriptTimeoutSeconds,omitempty"`

	// The self-update fields, set only when Type is DispatchUpdate.
	//
	// Asset is a file name under the server's own /downloads endpoint, never a
	// URL: an agent fetches its new binary from the server it is enrolled with
	// and from nowhere else, whatever a dispatch claims. Sha256 and SizeBytes
	// are what the server measured on the file it is about to hand out, so the
	// guest can refuse anything that arrives truncated or rewritten in transit.
	Version   string `json:"version,omitempty"`
	Asset     string `json:"asset,omitempty"`
	Sha256    string `json:"sha256,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
}

// Dispatch types.
const (
	DispatchBackup  = "backup"
	DispatchRestore = "restore"
	// DispatchUpdate tells an agent to replace its own binary with the build
	// this server hands out and restart. It carries no run: nothing in the
	// job_runs table describes it, and its outcome is reported by the version
	// on the agent's next heartbeat rather than by a completion call.
	DispatchUpdate = "update"
)

// Result is what the agent-driven run produced.
type Result struct {
	Disks []engine.DiskManifest
	Stats engine.Stats
	Err   error
}

type runState struct {
	dispatch Dispatch
	agentID  string
	eng      *engine.Engine
	sess     *engine.Session
	manifest *engine.Manifest // restore only
	done     chan Result
	closed   bool
	// picked is closed when the agent collects this dispatch on a heartbeat,
	// which is the moment waiting-to-be-taken becomes work-in-progress.
	picked     chan struct{}
	pickedOnce sync.Once
	// progress is the last time this run showed a sign of life. Chunk uploads
	// are the only such sign during a backup: the agent runs the job inside its
	// poll loop, so it deliberately stops heartbeating until the job is done.
	progress time.Time
}

// markPicked records that the agent has taken the work. Safe to call more than
// once — a dispatch can be handed out again if a heartbeat is retried.
func (rs *runState) markPicked() {
	rs.pickedOnce.Do(func() { close(rs.picked) })
}

// Manager owns agent state and in-flight agent-driven runs.
type Manager struct {
	st  *store.Store
	log *slog.Logger

	// PickupTimeout and StallTimeout bound the two phases of a run. They are
	// fields rather than constants so tests can drive both without waiting out
	// the production values.
	PickupTimeout time.Duration
	StallTimeout  time.Duration

	mu      sync.Mutex
	pending map[string][]Dispatch // agentID -> queued dispatches
	active  map[string]*runState  // runID -> state
}

// New builds a manager.
func New(st *store.Store, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		st:            st,
		log:           log,
		PickupTimeout: DefaultPickupTimeout,
		StallTimeout:  DefaultStallTimeout,
		pending:       map[string][]Dispatch{},
		active:        map[string]*runState{},
	}
}

// ---------------------------------------------------------------- enrollment

// CreateEnrollToken issues a single-use enrollment token valid for 24 hours.
func (m *Manager) CreateEnrollToken(ctx context.Context) (*store.EnrollToken, error) {
	tok, err := auth.NewEnrollToken()
	if err != nil {
		return nil, err
	}
	// An agent belongs to no Proxmox host, so its token carries none.
	return m.st.CreateEnrollToken(ctx, tok, store.EnrollPurposeAgent, "", auth.EnrollTokenTTL)
}

// RegisterRequest is the body of POST /api/agents/register.
type RegisterRequest struct {
	Token    string `json:"token"`
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Version  string `json:"version"`
}

// RegisterResponse is the body returned by POST /api/agents/register.
type RegisterResponse struct {
	AgentID string `json:"agentId"`
	APIKey  string `json:"apiKey"`
}

// Register consumes an enrollment token and creates an agent with a fresh API key.
func (m *Manager) Register(ctx context.Context, req RegisterRequest) (*RegisterResponse, error) {
	if req.Token == "" {
		return nil, ErrBadToken
	}
	if err := m.st.ConsumeEnrollToken(ctx, req.Token, store.EnrollPurposeAgent); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrBadToken
		}
		return nil, err
	}
	key, err := auth.NewAgentKey()
	if err != nil {
		return nil, err
	}
	hostname := req.Hostname
	if hostname == "" {
		hostname = "unknown"
	}
	now := store.Now()
	a := &store.Agent{
		Hostname:   hostname,
		OS:         req.OS,
		Arch:       req.Arch,
		Version:    req.Version,
		APIKeyHash: auth.HashAgentKey(key),
		LastSeen:   &now,
	}
	if _, err := m.st.CreateAgent(ctx, a); err != nil {
		return nil, err
	}
	m.log.Info("agent registered", "agentId", a.ID, "hostname", a.Hostname, "os", a.OS, "arch", a.Arch)
	return &RegisterResponse{AgentID: a.ID, APIKey: key}, nil
}

// Authenticate resolves an agent from a bearer API key.
func (m *Manager) Authenticate(ctx context.Context, key string) (*store.Agent, error) {
	if key == "" {
		return nil, ErrUnauthorized
	}
	a, err := m.st.AgentByKeyHash(ctx, auth.HashAgentKey(key))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrUnauthorized
		}
		return nil, err
	}
	return a, nil
}

// Online reports whether an agent counts as online.
func Online(a *store.Agent) bool {
	if a == nil || a.LastSeen == nil {
		return false
	}
	return time.Since(*a.LastSeen) <= OnlineWindow
}

// Status returns "online" or "offline" for an agent.
func Status(a *store.Agent) string {
	if Online(a) {
		return "online"
	}
	return "offline"
}

// ---------------------------------------------------------------- heartbeat

// HeartbeatRequest is the body of POST /api/agents/heartbeat.
//
// Every field is optional. An agent built before the server asked for them
// sends an empty object and keeps whatever it registered with — reporting the
// running version must never be the reason a heartbeat stops working.
type HeartbeatRequest struct {
	// Version is the build the agent is running right now, which is not
	// necessarily the one it registered with: an agent that self-updates says so
	// here, and that is how the console learns the update took.
	Version string `json:"version,omitempty"`
	OS      string `json:"os,omitempty"`
	Arch    string `json:"arch,omitempty"`
}

// Heartbeat records liveness, refreshes what the agent reports it is running,
// and returns any queued dispatches for it.
func (m *Manager) Heartbeat(ctx context.Context, agentID string, req HeartbeatRequest) ([]Dispatch, error) {
	if err := m.st.RecordAgentHeartbeat(ctx, agentID, store.Now(), req.Version, req.OS, req.Arch); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.pending[agentID]
	if len(q) == 0 {
		return []Dispatch{}, nil
	}
	delete(m.pending, agentID)
	/* Handing the dispatch over is the only moment the server can observe the
	   agent taking the work: from here until the run completes the agent is
	   busy in its poll loop and stops heartbeating, so this is also the last
	   liveness signal before the chunks start arriving. */
	now := time.Now()
	for _, d := range q {
		if rs, ok := m.active[d.RunID]; ok {
			rs.progress = now
			rs.markPicked()
		}
	}
	return q, nil
}

// ---------------------------------------------------------------- self-update

// Busy reports whether the agent has work in flight. It is the check that stops
// an operator restarting an agent in the middle of a backup.
//
// A run counts from the moment it is dispatched, not from the moment the agent
// collects it: enqueue registers the run and queues the dispatch together, so
// the live runs are exactly the entries in active. The pending queue is not
// consulted, because a dispatch left there after its run was abandoned or
// failed describes work that no longer exists — treating that as busy would
// leave an agent permanently unupdatable after one cancelled run.
func (m *Manager) Busy(agentID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.busyLocked(agentID)
}

func (m *Manager) busyLocked(agentID string) bool {
	for _, rs := range m.active {
		if rs.agentID == agentID && !rs.closed {
			return true
		}
	}
	return false
}

// QueueUpdate hands an agent a self-update to pick up on its next poll, or
// answers ErrAgentBusy when it has work in flight.
//
// A pending update replaces any earlier one rather than queueing beside it: two
// dispatches would have the agent download and swap the same binary twice, and
// the second swap would run against a binary that is already the new one.
func (m *Manager) QueueUpdate(agentID string, d Dispatch) error {
	d.Type = DispatchUpdate
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busyLocked(agentID) {
		return ErrAgentBusy
	}
	q := m.pending[agentID]
	kept := make([]Dispatch, 0, len(q)+1)
	for _, existing := range q {
		if existing.Type != DispatchUpdate {
			kept = append(kept, existing)
		}
	}
	m.pending[agentID] = append(kept, d)
	m.log.Info("agent self-update queued", "agentId", agentID, "version", d.Version, "asset", d.Asset)
	return nil
}

func (m *Manager) enqueue(agentID string, d Dispatch, rs *runState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rs.picked = make(chan struct{})
	rs.progress = time.Now()
	m.active[d.RunID] = rs
	m.pending[agentID] = append(m.pending[agentID], d)
}

// noteProgress records a sign of life from a run. Called on every accepted
// chunk, which during a backup is the only thing the agent sends.
func (m *Manager) noteProgress(runID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rs, ok := m.active[runID]; ok {
		rs.progress = time.Now()
	}
}

// sinceProgress reports how long a run has been silent, and whether it is still
// active at all.
func (m *Manager) sinceProgress(runID string) (time.Duration, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rs, ok := m.active[runID]
	if !ok {
		return 0, false
	}
	return time.Since(rs.progress), true
}

func (m *Manager) takeRun(runID, agentID string) (*runState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rs, ok := m.active[runID]
	if !ok {
		return nil, ErrUnknownRun
	}
	if rs.agentID != agentID {
		return nil, ErrUnauthorized
	}
	return rs, nil
}

func (m *Manager) finish(runID string, res Result) {
	m.mu.Lock()
	rs, ok := m.active[runID]
	if !ok || rs.closed {
		m.mu.Unlock()
		return
	}
	rs.closed = true
	delete(m.active, runID)
	m.mu.Unlock()
	rs.done <- res
}

func (m *Manager) abandon(runID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rs, ok := m.active[runID]; ok {
		rs.closed = true
		delete(m.active, runID)
	}
}

// ---------------------------------------------------------------- backup

// BackupRequest asks an agent to perform a file backup.
type BackupRequest struct {
	RunID   string
	AgentID string
	JobID   string
	JobName string
	Paths   []string
	// ExcludePaths, PreScript, PostScript and ScriptTimeoutSeconds are the parts
	// of the job's protection policy that only the agent can carry out.
	ExcludePaths         []string
	PreScript            string
	PostScript           string
	ScriptTimeoutSeconds int
	Engine               *engine.Engine
	Session              *engine.Session
}

// RunBackup dispatches a file backup to the agent and blocks until the agent
// reports completion, failure, or the context is done.
func (m *Manager) RunBackup(ctx context.Context, req BackupRequest) (Result, error) {
	d := Dispatch{
		RunID:                req.RunID,
		Type:                 DispatchBackup,
		JobID:                req.JobID,
		JobName:              req.JobName,
		Paths:                req.Paths,
		ChunkSize:            engine.ChunkSize,
		ExcludePaths:         req.ExcludePaths,
		PreScript:            req.PreScript,
		PostScript:           req.PostScript,
		ScriptTimeoutSeconds: req.ScriptTimeoutSeconds,
	}
	rs := &runState{dispatch: d, agentID: req.AgentID, eng: req.Engine, sess: req.Session, done: make(chan Result, 1)}
	m.enqueue(req.AgentID, d, rs)
	return m.wait(ctx, req.RunID, rs)
}

// RestoreRequest asks an agent to restore a manifest to a directory.
type RestoreRequest struct {
	RunID    string
	AgentID  string
	JobName  string
	DestPath string
	Manifest *engine.Manifest
	Engine   *engine.Engine
	Session  *engine.Session
}

// RunRestore dispatches a restore to the agent and blocks until it finishes.
func (m *Manager) RunRestore(ctx context.Context, req RestoreRequest) (Result, error) {
	d := Dispatch{
		RunID:     req.RunID,
		Type:      DispatchRestore,
		JobName:   req.JobName,
		DestPath:  req.DestPath,
		ChunkSize: engine.ChunkSize,
	}
	rs := &runState{
		dispatch: d, agentID: req.AgentID, eng: req.Engine, sess: req.Session,
		manifest: req.Manifest, done: make(chan Result, 1),
	}
	m.enqueue(req.AgentID, d, rs)
	return m.wait(ctx, req.RunID, rs)
}

// wait blocks until the run finishes, bounded in two phases.
//
// Until the agent collects the dispatch, the bound is PickupTimeout — an agent
// that has not taken the work after many heartbeats is not going to. Once it
// has, there is no bound on how long the backup may legitimately take, only on
// how long it may say nothing: a run that goes quiet for StallTimeout has lost
// its agent. The caller's ctx still carries policy.maxDurationMinutes, which
// remains the only cap on total run length.
func (m *Manager) wait(ctx context.Context, runID string, rs *runState) (Result, error) {
	pickup := time.NewTimer(m.PickupTimeout)
	defer pickup.Stop()

	select {
	case res := <-rs.done:
		return res, res.Err
	case <-ctx.Done():
		m.abandon(runID)
		return Result{}, ctx.Err()
	case <-pickup.C:
		m.abandon(runID)
		return Result{}, fmt.Errorf("%w within %s — it may be offline, or stopped",
			ErrPickupTimeout, m.PickupTimeout)
	case <-rs.picked:
	}

	/* Polling rather than one long timer: the deadline moves forward with every
	   chunk, so what is being watched is the gap between signs of life, not a
	   fixed point in time. */
	tick := m.StallTimeout / 10
	if tick <= 0 {
		tick = time.Second
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	for {
		select {
		case res := <-rs.done:
			return res, res.Err
		case <-ctx.Done():
			m.abandon(runID)
			return Result{}, ctx.Err()
		case <-ticker.C:
			silent, active := m.sinceProgress(runID)
			if !active {
				// finish() removed it; the result is on its way.
				continue
			}
			if silent >= m.StallTimeout {
				m.abandon(runID)
				return Result{}, fmt.Errorf("%w for %s", ErrStalled, silent.Round(time.Second))
			}
		}
	}
}

// AcceptChunk stores a chunk uploaded by an agent for an in-flight run.
func (m *Manager) AcceptChunk(ctx context.Context, runID, agentID, sha string, data []byte, totalBytes int64) (deduped bool, err error) {
	rs, err := m.takeRun(runID, agentID)
	if err != nil {
		return false, err
	}
	if rs.dispatch.Type != DispatchBackup {
		return false, fmt.Errorf("agentmgr: run %s is not a backup run", runID)
	}
	if totalBytes > 0 && rs.sess != nil {
		rs.sess.SetTotal(totalBytes)
	}
	uploaded, err := rs.eng.StoreChunkVerified(ctx, sha, data)
	if err != nil {
		return false, err
	}
	if rs.sess != nil {
		rs.sess.RecordChunk(int64(len(data)), uploaded)
	}
	m.noteProgress(runID)
	return uploaded == 0, nil
}

// CompleteRequest is the body of POST /api/agents/runs/{runId}/complete.
type CompleteRequest struct {
	Disks []engine.DiskManifest `json:"disks"`
	// What the guest could and could not archive. A live filesystem always
	// holds files that cannot be read, and the operator has to learn that from
	// the run rather than from a restore. All optional: an older agent simply
	// reports nothing.
	FilesTotal    int      `json:"filesTotal,omitempty"`
	SkippedTotal  int      `json:"skippedTotal,omitempty"`
	SkippedSample []string `json:"skippedSample,omitempty"`
	ExcludedTotal int      `json:"excludedTotal,omitempty"`
}

// Omissions renders the run-log line describing what a completed agent backup
// left out, or "" when it archived everything it was asked to.
func (r CompleteRequest) Omissions() string {
	if r.SkippedTotal == 0 && r.ExcludedTotal == 0 {
		return ""
	}
	parts := []string{}
	if r.SkippedTotal > 0 {
		part := fmt.Sprintf("%d file(s) could not be read and were skipped", r.SkippedTotal)
		if len(r.SkippedSample) > 0 {
			part += " (e.g. " + r.SkippedSample[0] + ")"
		}
		parts = append(parts, part)
	}
	if r.ExcludedTotal > 0 {
		parts = append(parts, fmt.Sprintf("%d entr(ies) matched an exclusion", r.ExcludedTotal))
	}
	return strings.Join(parts, "; ")
}

// Complete finalises an agent-driven run.
func (m *Manager) Complete(ctx context.Context, runID, agentID string, req CompleteRequest) error {
	rs, err := m.takeRun(runID, agentID)
	if err != nil {
		return err
	}
	if rs.dispatch.Type == DispatchRestore {
		m.finish(runID, Result{Stats: statsOf(rs)})
		return nil
	}
	if len(req.Disks) == 0 {
		return errors.New("agentmgr: complete requires at least one stream")
	}
	// Every chunk the agent references must actually be present on the target.
	for i := range req.Disks {
		d := &req.Disks[i]
		if d.Name == "" {
			d.Name = "files.tar"
		}
		var size int64
		for _, c := range d.Chunks {
			ok, err := rs.eng.HasChunk(ctx, c.Sha256)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("agentmgr: run %s references missing chunk %s", runID, c.Sha256)
			}
			size += c.Size
		}
		if d.SizeBytes == 0 {
			d.SizeBytes = size
		}
	}
	m.finish(runID, Result{Disks: req.Disks, Stats: statsOf(rs)})
	return nil
}

// Fail marks an agent-driven run as failed.
func (m *Manager) Fail(ctx context.Context, runID, agentID, message string) error {
	if _, err := m.takeRun(runID, agentID); err != nil {
		return err
	}
	if message == "" {
		message = "agent reported failure"
	}
	m.finish(runID, Result{Err: fmt.Errorf("agent: %s", message)})
	return nil
}

// RestoreStream writes the restore tar stream for a dispatched restore run.
func (m *Manager) RestoreStream(ctx context.Context, runID, agentID string, w io.Writer) error {
	rs, err := m.takeRun(runID, agentID)
	if err != nil {
		return err
	}
	if rs.dispatch.Type != DispatchRestore || rs.manifest == nil {
		return fmt.Errorf("agentmgr: run %s is not a restore run", runID)
	}
	/* A restore is one long response rather than many small requests, so bytes
	   leaving here are its only sign of life. Without this the stall watchdog
	   would see silence for the whole transfer and kill a working restore. */
	pw := &progressWriter{w: w, note: func() { m.noteProgress(runID) }}
	for _, d := range rs.manifest.Disks {
		if err := rs.sess.RestoreDisk(ctx, d, pw); err != nil {
			return err
		}
	}
	return nil
}

// progressWriter reports liveness as bytes pass through it.
type progressWriter struct {
	w    io.Writer
	note func()
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	if n > 0 {
		p.note()
	}
	return n, err
}

func statsOf(rs *runState) engine.Stats {
	if rs.sess == nil {
		return engine.Stats{}
	}
	return rs.sess.Stats()
}
