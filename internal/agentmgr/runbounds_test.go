package agentmgr_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"proxback/internal/agentmgr"
)

/*
A run used to be wrapped in a single five-minute deadline covering the whole
backup, so an agent that collected the work and kept uploading was still killed
at five minutes with "context deadline exceeded". A user hit exactly that: 248
MiB read, 67.5 MiB transferred, failed at 5m0.002s.

These tests hold the two halves apart. Waiting to be collected is bounded.
Working is not — only going quiet is.
*/

// dispatchTo drains the agent's queue, which is what the real agent's heartbeat
// does and what marks the run as collected.
func dispatchTo(t *testing.T, m *agentmgr.Manager, agentID string) {
	t.Helper()
	if !waitFor(t, func() bool {
		jobs, _ := m.Heartbeat(context.Background(), agentID, agentmgr.HeartbeatRequest{})
		return len(jobs) > 0
	}) {
		t.Fatal("no dispatch arrived for the agent")
	}
}

func TestARunThatKeepsUploadingIsNotKilledForTakingTooLong(t *testing.T) {
	m, st := newManager(t)
	a := newAgent(t, st, "0.6.5")
	// Both bounds are far shorter than the work, so anything that caps total
	// run length rather than silence will fire.
	m.PickupTimeout = 150 * time.Millisecond
	m.StallTimeout = 150 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, err := m.RunBackup(context.Background(), agentmgr.BackupRequest{
			RunID: "long-run", AgentID: a.ID, Paths: []string{"/data"},
		})
		done <- err
	}()

	dispatchTo(t, m, a.ID)

	// Keep reporting progress for well over both timeouts, as a healthy backup
	// of a large volume does.
	deadline := time.After(900 * time.Millisecond)
	tick := time.NewTicker(30 * time.Millisecond)
	defer tick.Stop()
	for working := true; working; {
		select {
		case err := <-done:
			t.Fatalf("run ended while it was still making progress: %v", err)
		case <-deadline:
			working = false
		case <-tick.C:
			m.NoteProgressForTest("long-run")
		}
	}

	// Still running, six stall windows later.
	select {
	case err := <-done:
		t.Fatalf("run ended while it was still making progress: %v", err)
	default:
	}
}

func TestARunTheAgentNeverCollectsFailsAsNotCollected(t *testing.T) {
	m, st := newManager(t)
	a := newAgent(t, st, "0.6.5")
	m.PickupTimeout = 80 * time.Millisecond
	m.StallTimeout = time.Hour

	start := time.Now()
	_, err := m.RunBackup(context.Background(), agentmgr.BackupRequest{
		RunID: "never-taken", AgentID: a.ID, Paths: []string{"/data"},
	})
	if !errors.Is(err, agentmgr.ErrPickupTimeout) {
		t.Fatalf("err = %v, want ErrPickupTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("waited %s for the pickup bound", elapsed)
	}
}

func TestACollectedRunThatGoesQuietFailsAsStalled(t *testing.T) {
	m, st := newManager(t)
	a := newAgent(t, st, "0.6.5")
	m.PickupTimeout = time.Hour
	m.StallTimeout = 120 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, err := m.RunBackup(context.Background(), agentmgr.BackupRequest{
			RunID: "goes-quiet", AgentID: a.ID, Paths: []string{"/data"},
		})
		done <- err
	}()

	dispatchTo(t, m, a.ID)

	select {
	case err := <-done:
		if !errors.Is(err, agentmgr.ErrStalled) {
			t.Fatalf("err = %v, want ErrStalled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a run that went quiet was never failed")
	}
}

// The distinction is the point: a failed run has to say which of the two
// happened, because they call for different things from the operator.
func TestTheTwoFailuresAreDistinguishable(t *testing.T) {
	if errors.Is(agentmgr.ErrStalled, agentmgr.ErrPickupTimeout) ||
		errors.Is(agentmgr.ErrPickupTimeout, agentmgr.ErrStalled) {
		t.Fatal("the pickup and stall failures are not distinguishable")
	}
}
