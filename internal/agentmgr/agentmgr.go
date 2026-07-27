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
	"sync"
	"time"

	"proxback/internal/auth"
	"proxback/internal/engine"
	"proxback/internal/store"
)

// OnlineWindow is how recently an agent must have been seen to count as online.
const OnlineWindow = 45 * time.Second

// Errors returned by the manager.
var (
	ErrUnauthorized = errors.New("agentmgr: unauthorized")
	ErrUnknownRun   = errors.New("agentmgr: unknown or finished run")
	ErrBadToken     = errors.New("agentmgr: invalid or expired enrollment token")
)

// Dispatch is a work item handed to an agent on heartbeat.
type Dispatch struct {
	RunID     string   `json:"runId"`
	Type      string   `json:"type"` // backup | restore
	JobID     string   `json:"jobId,omitempty"`
	JobName   string   `json:"jobName,omitempty"`
	Paths     []string `json:"paths,omitempty"`
	DestPath  string   `json:"destPath,omitempty"`
	ChunkSize int64    `json:"chunkSize"`
}

// Dispatch types.
const (
	DispatchBackup  = "backup"
	DispatchRestore = "restore"
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
}

// Manager owns agent state and in-flight agent-driven runs.
type Manager struct {
	st  *store.Store
	log *slog.Logger

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
		st:      st,
		log:     log,
		pending: map[string][]Dispatch{},
		active:  map[string]*runState{},
	}
}

// ---------------------------------------------------------------- enrollment

// CreateEnrollToken issues a single-use enrollment token valid for 24 hours.
func (m *Manager) CreateEnrollToken(ctx context.Context) (*store.EnrollToken, error) {
	tok, err := auth.NewEnrollToken()
	if err != nil {
		return nil, err
	}
	return m.st.CreateEnrollToken(ctx, tok, store.EnrollPurposeAgent, auth.EnrollTokenTTL)
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

// Heartbeat records liveness and returns any queued dispatches for the agent.
func (m *Manager) Heartbeat(ctx context.Context, agentID string) ([]Dispatch, error) {
	if err := m.st.TouchAgent(ctx, agentID, store.Now()); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.pending[agentID]
	if len(q) == 0 {
		return []Dispatch{}, nil
	}
	delete(m.pending, agentID)
	return q, nil
}

func (m *Manager) enqueue(agentID string, d Dispatch, rs *runState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active[d.RunID] = rs
	m.pending[agentID] = append(m.pending[agentID], d)
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
	Engine  *engine.Engine
	Session *engine.Session
}

// RunBackup dispatches a file backup to the agent and blocks until the agent
// reports completion, failure, or the context is done.
func (m *Manager) RunBackup(ctx context.Context, req BackupRequest) (Result, error) {
	d := Dispatch{
		RunID:     req.RunID,
		Type:      DispatchBackup,
		JobID:     req.JobID,
		JobName:   req.JobName,
		Paths:     req.Paths,
		ChunkSize: engine.ChunkSize,
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

func (m *Manager) wait(ctx context.Context, runID string, rs *runState) (Result, error) {
	select {
	case res := <-rs.done:
		return res, res.Err
	case <-ctx.Done():
		m.abandon(runID)
		return Result{}, ctx.Err()
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
	return uploaded == 0, nil
}

// CompleteRequest is the body of POST /api/agents/runs/{runId}/complete.
type CompleteRequest struct {
	Disks []engine.DiskManifest `json:"disks"`
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
	for _, d := range rs.manifest.Disks {
		if err := rs.sess.RestoreDisk(ctx, d, w); err != nil {
			return err
		}
	}
	return nil
}

func statsOf(rs *runState) engine.Stats {
	if rs.sess == nil {
		return engine.Stats{}
	}
	return rs.sess.Stats()
}
