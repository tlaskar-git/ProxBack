package sched

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"proxback/internal/engine"
	"proxback/internal/store"
)

// sourceWriteInterval throttles the per-source row writes. The engine already
// coalesces progress callbacks to a few per second; this keeps the database
// side to roughly one write per source per second, which is fast enough for a
// 2 s polling UI and cheap enough for a run of dozens of sources.
const sourceWriteInterval = time.Second

// throughputWindow is the shortest trailing window a throughput sample is taken
// over. Shorter windows make the figure jump around; longer ones make it lag.
const throughputWindow = time.Second

// runMonitor follows one run in flight: which source is currently being backed
// up, how far it has got, and how fast the run is moving. The per-source rows
// live in the database (they have to survive a page reload); the sampling state
// behind throughputBps is in memory only and disappears with the run, which is
// exactly the API contract — 0 when the run is not running.
type runMonitor struct {
	st    *store.Store
	log   *slog.Logger
	runID string

	mu sync.Mutex
	// seq is the source currently in flight, or -1 when none is.
	seq int
	// base is the run-wide byte count when the active source started, so the
	// per-source figures are deltas of the shared session counters.
	baseProcessed int64
	baseUploaded  int64
	lastWrite     time.Time

	sampleAt    time.Time
	sampleBytes int64
	bps         float64
}

func newRunMonitor(st *store.Store, log *slog.Logger, runID string) *runMonitor {
	return &runMonitor{st: st, log: log, runID: runID, seq: -1}
}

// monitorFor returns the monitor of a run in flight, or nil.
func (m *Manager) monitorFor(runID string) *runMonitor {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.monitors[runID]
}

// ThroughputBps reports a run's current speed in bytes per second, averaged over
// the last sample window. It is 0 for a run that is not in flight.
func (m *Manager) ThroughputBps(runID string) float64 {
	mon := m.monitorFor(runID)
	if mon == nil {
		return 0
	}
	mon.mu.Lock()
	defer mon.mu.Unlock()
	return mon.bps
}

// plan records every source the run intends to walk, all pending and carrying
// their known size, so the monitor can show the whole run — and its byte total —
// before the first source starts.
func (m *runMonitor) plan(ctx context.Context, sources []store.RunSource) {
	if err := m.st.ReplaceRunSources(ctx, m.runID, sources); err != nil {
		m.log.Warn("could not record the run's sources", "run", m.runID, "error", err)
	}
}

// begin marks a source as the one in flight and anchors the byte deltas to the
// run's counters as they stand now.
func (m *runMonitor) begin(ctx context.Context, seq int, at engine.Stats) {
	m.mu.Lock()
	m.seq = seq
	m.baseProcessed = at.BytesProcessed
	m.baseUploaded = at.BytesUploaded
	m.lastWrite = time.Time{}
	m.mu.Unlock()
	if err := m.st.StartRunSource(ctx, m.runID, seq); err != nil {
		m.log.Warn("could not start the run source row", "run", m.runID, "seq", seq, "error", err)
	}
}

// progress is called from the engine's throttled progress callback. It advances
// the active source's byte counts (at most once per sourceWriteInterval) and
// takes the throughput sample.
func (m *runMonitor) progress(ctx context.Context, s engine.Stats) {
	m.mu.Lock()
	m.sample(s.BytesProcessed)
	seq := m.seq
	processed := s.BytesProcessed - m.baseProcessed
	uploaded := s.BytesUploaded - m.baseUploaded
	write := seq >= 0 && time.Since(m.lastWrite) >= sourceWriteInterval
	if write {
		m.lastWrite = time.Now()
	}
	m.mu.Unlock()
	if !write {
		return
	}
	if err := m.st.UpdateRunSourceProgress(ctx, m.runID, seq, max64(processed, 0), max64(uploaded, 0)); err != nil {
		m.log.Warn("could not update run source progress", "run", m.runID, "seq", seq, "error", err)
	}
}

// sample updates the trailing-window throughput. The caller holds the lock.
func (m *runMonitor) sample(processed int64) {
	now := time.Now()
	if m.sampleAt.IsZero() {
		m.sampleAt, m.sampleBytes = now, processed
		return
	}
	elapsed := now.Sub(m.sampleAt)
	if elapsed < throughputWindow {
		return
	}
	if delta := processed - m.sampleBytes; delta >= 0 {
		m.bps = float64(delta) / elapsed.Seconds()
	}
	m.sampleAt, m.sampleBytes = now, processed
}

// setSize records a source's total once it is known, which for an agent stream
// is only after the agent has produced it.
func (m *runMonitor) setSize(ctx context.Context, seq int, size int64) {
	if err := m.st.SetRunSourceSize(ctx, m.runID, seq, size); err != nil {
		m.log.Warn("could not record the run source size", "run", m.runID, "seq", seq, "error", err)
	}
}

// finish closes the active source with its final byte counts and status.
func (m *runMonitor) finish(ctx context.Context, status string, at engine.Stats, srcErr string) {
	m.mu.Lock()
	seq := m.seq
	processed := at.BytesProcessed - m.baseProcessed
	uploaded := at.BytesUploaded - m.baseUploaded
	m.seq = -1
	m.mu.Unlock()
	if seq < 0 {
		return
	}
	if err := m.st.FinishRunSource(ctx, m.runID, seq, status,
		max64(processed, 0), max64(uploaded, 0), srcErr); err != nil {
		m.log.Warn("could not finish the run source row", "run", m.runID, "seq", seq, "error", err)
	}
}

// closeOut is called once the run itself is over: whatever the run never got to
// is marked skipped rather than left pending forever.
func (m *runMonitor) closeOut(ctx context.Context) {
	if err := m.st.SkipPendingRunSources(ctx, m.runID); err != nil {
		m.log.Warn("could not close out the run's sources", "run", m.runID, "error", err)
	}
}

func max64(v, floor int64) int64 {
	if v < floor {
		return floor
	}
	return v
}
