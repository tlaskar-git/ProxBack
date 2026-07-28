package agentmgr_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"proxback/internal/agentmgr"
	"proxback/internal/engine"
	"proxback/internal/store"
)

func newManager(t *testing.T) (*agentmgr.Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return agentmgr.New(st, slog.New(slog.NewTextHandler(io.Discard, nil))), st
}

func newAgent(t *testing.T, st *store.Store, version string) *store.Agent {
	t.Helper()
	a, err := st.CreateAgent(context.Background(), &store.Agent{
		Hostname: "ws-01", OS: "windows", Arch: "amd64", Version: version, APIKeyHash: "hash",
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	return a
}

// TestHeartbeatRefreshesTheReportedVersion is the fix for the drift a user hit
// in production: a server on 0.6.2 whose Windows agent was still 0.6.1 and was
// displayed as "1.0.0", because the version was captured at registration and
// never looked at again.
func TestHeartbeatRefreshesTheReportedVersion(t *testing.T) {
	ctx := context.Background()
	m, st := newManager(t)
	a := newAgent(t, st, "0.6.1")

	if _, err := m.Heartbeat(ctx, a.ID, agentmgr.HeartbeatRequest{
		Version: "0.6.2", OS: "windows", Arch: "amd64",
	}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	got, err := st.AgentByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reload agent: %v", err)
	}
	if got.Version != "0.6.2" {
		t.Fatalf("version after an upgrade heartbeat = %q, want 0.6.2", got.Version)
	}
	if got.LastSeen == nil || !agentmgr.Online(got) {
		t.Fatalf("the agent is not online after a heartbeat (lastSeen %v)", got.LastSeen)
	}

	// A downgrade is drift too — an operator who rolled a guest back must see
	// that, not a stale claim that it is current.
	if _, err := m.Heartbeat(ctx, a.ID, agentmgr.HeartbeatRequest{Version: "0.5.9"}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	got, _ = st.AgentByID(ctx, a.ID)
	if got.Version != "0.5.9" {
		t.Fatalf("version after a downgrade heartbeat = %q, want 0.5.9", got.Version)
	}
}

// TestHeartbeatWithoutAVersionLeavesTheStoredOneAlone keeps an older agent
// working: it sends "{}" and must not have its recorded version erased.
func TestHeartbeatWithoutAVersionLeavesTheStoredOneAlone(t *testing.T) {
	ctx := context.Background()
	m, st := newManager(t)
	a := newAgent(t, st, "0.6.1")

	if _, err := m.Heartbeat(ctx, a.ID, agentmgr.HeartbeatRequest{}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	got, err := st.AgentByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reload agent: %v", err)
	}
	if got.Version != "0.6.1" || got.OS != "windows" || got.Arch != "amd64" {
		t.Fatalf("a silent heartbeat changed the agent: %+v", got)
	}
	if !agentmgr.Online(got) {
		t.Fatal("a silent heartbeat did not count as liveness")
	}
}

// TestQueueUpdateIsCollectedOnTheNextPoll covers the delivery mechanism: an
// agent already polls the heartbeat endpoint for work, and its self-update is
// one more dispatch on that same poll.
func TestQueueUpdateIsCollectedOnTheNextPoll(t *testing.T) {
	ctx := context.Background()
	m, st := newManager(t)
	a := newAgent(t, st, "0.6.1")

	want := agentmgr.Dispatch{
		Version: "0.6.2", Asset: "proxback-agent-windows-amd64.exe",
		Sha256: "abc123", SizeBytes: 4242,
	}
	if err := m.QueueUpdate(a.ID, want); err != nil {
		t.Fatalf("queue update: %v", err)
	}
	jobs, err := m.Heartbeat(ctx, a.ID, agentmgr.HeartbeatRequest{Version: "0.6.1"})
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("dispatches on the next poll = %d, want 1: %+v", len(jobs), jobs)
	}
	got := jobs[0]
	if got.Type != agentmgr.DispatchUpdate {
		t.Errorf("dispatch type = %q, want %q", got.Type, agentmgr.DispatchUpdate)
	}
	if got.Asset != want.Asset || got.Sha256 != want.Sha256 || got.SizeBytes != want.SizeBytes {
		t.Errorf("dispatch = %+v, want asset/sha/size %s/%s/%d",
			got, want.Asset, want.Sha256, want.SizeBytes)
	}
	// A dispatch is handed out once. Polling again must not re-apply it.
	again, err := m.Heartbeat(ctx, a.ID, agentmgr.HeartbeatRequest{})
	if err != nil {
		t.Fatalf("second heartbeat: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("the update was dispatched twice: %+v", again)
	}
}

// TestQueueUpdateReplacesAPendingOne stops two clicks queueing two swaps of the
// same binary, the second of which would run against the already-new file.
func TestQueueUpdateReplacesAPendingOne(t *testing.T) {
	ctx := context.Background()
	m, st := newManager(t)
	a := newAgent(t, st, "0.6.1")

	for _, ver := range []string{"0.6.2", "0.6.3"} {
		if err := m.QueueUpdate(a.ID, agentmgr.Dispatch{Version: ver, Asset: "agent"}); err != nil {
			t.Fatalf("queue update %s: %v", ver, err)
		}
	}
	jobs, err := m.Heartbeat(ctx, a.ID, agentmgr.HeartbeatRequest{})
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Version != "0.6.3" {
		t.Fatalf("pending dispatches = %+v, want one update to 0.6.3", jobs)
	}
}

// TestQueueUpdateRefusesAnAgentWithARunInFlight is the safety rule: applying an
// update restarts the agent, and restarting it mid-backup throws away every
// byte the guest has uploaded.
func TestQueueUpdateRefusesAnAgentWithARunInFlight(t *testing.T) {
	m, st := newManager(t)
	a := newAgent(t, st, "0.6.1")

	// RunBackup blocks until the agent reports the run finished, so it is
	// started in the background and the dispatch it queues is what makes the
	// agent busy.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = m.RunBackup(ctx, agentmgr.BackupRequest{
			RunID: "run-1", AgentID: a.ID, JobName: "files", Paths: []string{"/data"},
		})
	}()

	if !waitFor(t, func() bool { return m.Busy(a.ID) }) {
		t.Fatal("the agent never became busy after a backup was dispatched")
	}
	err := m.QueueUpdate(a.ID, agentmgr.Dispatch{Version: "0.6.2", Asset: "agent"})
	if !errors.Is(err, agentmgr.ErrAgentBusy) {
		t.Fatalf("queueing an update for a busy agent = %v, want ErrAgentBusy", err)
	}

	// Another agent is not affected by this one's run.
	idle := newAgent(t, st, "0.6.1")
	if m.Busy(idle.ID) {
		t.Fatal("an idle agent reports busy because a different agent is running")
	}
	if err := m.QueueUpdate(idle.ID, agentmgr.Dispatch{Version: "0.6.2", Asset: "agent"}); err != nil {
		t.Fatalf("queueing an update for an idle agent: %v", err)
	}

	// Once the run finishes the agent is updatable again.
	if err := m.Fail(context.Background(), "run-1", a.ID, "cancelled by the test"); err != nil {
		t.Fatalf("fail run: %v", err)
	}
	<-done
	if m.Busy(a.ID) {
		t.Fatal("the agent is still busy after its run finished")
	}
	if err := m.QueueUpdate(a.ID, agentmgr.Dispatch{Version: "0.6.2", Asset: "agent"}); err != nil {
		t.Fatalf("queueing an update after the run finished: %v", err)
	}
}

// TestAPendingUpdateDoesNotCountAsBusy: an update waiting to be collected must
// not block a second attempt, which would otherwise be unrefusable until the
// agent came back online.
func TestAPendingUpdateDoesNotCountAsBusy(t *testing.T) {
	m, st := newManager(t)
	a := newAgent(t, st, "0.6.1")
	if err := m.QueueUpdate(a.ID, agentmgr.Dispatch{Version: "0.6.2", Asset: "agent"}); err != nil {
		t.Fatalf("queue update: %v", err)
	}
	if m.Busy(a.ID) {
		t.Fatal("a pending update makes the agent look busy")
	}
}

// waitFor polls pred for up to two seconds. The dispatch it waits on is queued
// by another goroutine, so there is nothing to synchronise on but the effect.
func waitFor(t *testing.T, pred func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// TestChunkSizeTravelsWithABackupDispatch guards the assumption the update
// dispatch is added beside: the existing dispatch fields still arrive.
func TestChunkSizeTravelsWithABackupDispatch(t *testing.T) {
	m, st := newManager(t)
	a := newAgent(t, st, "0.6.1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, _ = m.RunBackup(ctx, agentmgr.BackupRequest{
			RunID: "run-2", AgentID: a.ID, Paths: []string{"/data"},
			ExcludePaths: []string{"**/tmp"},
		})
	}()
	var jobs []agentmgr.Dispatch
	if !waitFor(t, func() bool {
		jobs, _ = m.Heartbeat(ctx, a.ID, agentmgr.HeartbeatRequest{})
		return len(jobs) > 0
	}) {
		t.Fatal("no backup dispatch arrived")
	}
	if jobs[0].ChunkSize != engine.ChunkSize || len(jobs[0].ExcludePaths) != 1 {
		t.Fatalf("backup dispatch = %+v", jobs[0])
	}
}
