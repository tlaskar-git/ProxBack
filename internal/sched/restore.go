package sched

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"proxback/internal/agentmgr"
	"proxback/internal/engine"
	"proxback/internal/store"
)

func (m *Manager) executeRestore(ctx context.Context, run *store.JobRun, backup *store.Backup,
	spec RestoreSpec, plan restorePlan) (*engine.Stats, error) {
	eng, _, err := m.engineFor(ctx, backup.TargetID)
	if err != nil {
		return nil, err
	}
	man, err := eng.ReadManifest(ctx, backup.SourceKind, backup.SourceID, backup.ID)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	sess := eng.NewSession(man.TotalSize(), m.progressFn(run.ID))

	switch backup.SourceKind {
	case store.SourceVM:
		if err := m.restoreVM(ctx, run.ID, sess, man, spec, plan); err != nil {
			return statsPtr(sess), err
		}
	case store.SourceAgent:
		if err := m.restoreAgent(ctx, sess, eng, man, run, spec); err != nil {
			return statsPtr(sess), err
		}
	default:
		return nil, fmt.Errorf("sched: unsupported source kind %q", backup.SourceKind)
	}
	sess.SetStep("Completed")
	sess.Flush()
	return statsPtr(sess), nil
}

// executeVerify re-reads every chunk of a restore point and validates its
// SHA-256 and size against the manifest. Nothing is written.
//
// What success means is exactly this: the stored data is intact and complete.
// It is evidence of integrity, not of recoverability — no guest has been booted
// from it — and nothing in ProxBack may present it as a tested restore.
func (m *Manager) executeVerify(ctx context.Context, run *store.JobRun, backup *store.Backup) (*engine.Stats, error) {
	eng, _, err := m.engineFor(ctx, backup.TargetID)
	if err != nil {
		return nil, err
	}
	man, err := eng.ReadManifest(ctx, backup.SourceKind, backup.SourceID, backup.ID)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	sess := eng.NewSession(man.TotalSize(), m.progressFn(run.ID))
	m.logRun(ctx, run.ID, "verifying %s restore point of %s taken %s (%s)",
		backup.Kind, backup.SourceName, backup.CreatedAt.Format(time.RFC3339),
		humanBytes(man.TotalSize()))
	if err := sess.VerifyBackup(ctx, man); err != nil {
		return statsPtr(sess), err
	}
	sess.SetStep("Verified")
	sess.Flush()
	m.logRun(ctx, run.ID, "integrity verified: every stored chunk was re-read and its SHA-256 matched "+
		"(%s). This proves the stored data is intact; it is not a restore test.",
		humanBytes(sess.Stats().BytesProcessed))
	return statsPtr(sess), nil
}

// recordVerification attaches a verification's outcome to the restore point it
// examined, so the evidence lives with the point rather than only in run
// history. A cancelled verification proves nothing and is not recorded.
func (m *Manager) recordVerification(backupID string, stats *engine.Stats, verifyErr error) {
	if errors.Is(verifyErr, context.Canceled) {
		return
	}
	result := store.VerifyPassed
	if verifyErr != nil {
		result = store.VerifyFailed
	}
	var verified int64
	if stats != nil && verifyErr == nil {
		verified = stats.BytesProcessed
	}
	if err := m.st.RecordBackupVerification(m.detached(), backupID, store.Now(), result, verified); err != nil {
		m.log.Warn("could not record the verification result",
			"backup", backupID, "result", result, "error", err)
	}
}

func (m *Manager) restoreVM(ctx context.Context, runID string, sess *engine.Session,
	man *engine.Manifest, spec RestoreSpec, plan restorePlan) error {
	if spec.VM == nil {
		return errors.New("sched: vm restore target required")
	}
	hostID := spec.VM.HostID
	if hostID == "" {
		hostID = plan.meta.HostID
	}
	host, err := m.st.PVEHostByID(ctx, hostID)
	if err != nil {
		return fmt.Errorf("load proxmox host: %w", err)
	}
	client, err := PVEClient(host)
	if err != nil {
		return err
	}
	sourceVMID, sourceErr := vmidFromSourceID(man.SourceID)
	// The destination was resolved and safety-checked before the run started.
	vmid := plan.vmid
	if vmid == 0 {
		if vmid = spec.VM.VMID; vmid == 0 {
			if sourceErr != nil {
				return sourceErr
			}
			vmid = sourceVMID
		}
	}
	viaHelper := IsVMAManifest(man.Disks)
	node := plan.node
	if node == "" {
		node, err = client.FindNodeForVM(ctx, vmid)
		if err != nil {
			// Restoring into a vmid that does not exist yet is the normal
			// alongside case: fall back to the source's node.
			if sourceErr != nil {
				return err
			}
			if node, err = client.FindNodeForVM(ctx, sourceVMID); err != nil {
				return err
			}
		}
	}
	if viaHelper {
		// A whole-guest archive can only go back through qmrestore, which only
		// the node helper can run.
		h, err := m.requireHelperForNode(ctx, host.ID, node)
		if err != nil {
			return err
		}
		m.logRun(ctx, runID, "restoring %s into vm %d on %s/%s (%s) via qmrestore on the node helper",
			man.SourceName, vmid, host.Name, node, plan.meta.Mode)
		// --force overwrites an existing guest, so it is set only for a restore
		// the operator explicitly confirmed as an overwrite.
		return m.restoreViaHelper(ctx, sess, man, h, vmid, spec.VM.Storage, plan.force)
	}
	m.logRun(ctx, runID, "restoring %s into vm %d on %s/%s (%s) via per-disk import (%s)",
		man.SourceName, vmid, host.Name, node, plan.meta.Mode, countNoun(len(man.Disks), "disk"))
	for _, disk := range man.Disks {
		if err := ctx.Err(); err != nil {
			return err
		}
		sess.SetStep(fmt.Sprintf("Restoring %s %s", man.SourceName, disk.Name))
		pr, pw := io.Pipe()
		errc := make(chan error, 1)
		go func(d engine.DiskManifest) {
			err := sess.RestoreDisk(ctx, d, pw)
			_ = pw.CloseWithError(err)
			errc <- err
		}(disk)
		importErr := client.ImportDisk(ctx, node, vmid, disk.Name, pr, disk.SizeBytes)
		_ = pr.Close()
		restoreErr := <-errc
		// When the host rejects the import, the engine goroutine dies with a
		// closed-pipe write error; the API error is the one worth reporting.
		if restoreErr != nil && !errors.Is(restoreErr, io.ErrClosedPipe) {
			return restoreErr
		}
		if importErr != nil {
			return importErr
		}
		if restoreErr != nil {
			return restoreErr
		}
	}
	return nil
}

func (m *Manager) restoreAgent(ctx context.Context, sess *engine.Session, eng *engine.Engine, man *engine.Manifest, run *store.JobRun, spec RestoreSpec) error {
	if spec.Agent == nil || spec.Agent.AgentID == "" {
		return errors.New("sched: agent restore target required")
	}
	if spec.Agent.DestPath == "" {
		return errors.New("sched: agent restore needs a destPath")
	}
	agent, err := m.st.AgentByID(ctx, spec.Agent.AgentID)
	if err != nil {
		return fmt.Errorf("load agent: %w", err)
	}
	if !agentmgr.Online(agent) {
		return fmt.Errorf("sched: agent %s is offline", agent.Hostname)
	}
	m.logRun(ctx, run.ID, "restoring %s to agent %s into %s",
		man.SourceName, agent.Hostname, spec.Agent.DestPath)
	sess.SetStep("Dispatching restore to agent " + agent.Hostname)
	// Bounded by agentmgr's pickup and stall watchdogs, not by a flat deadline:
	// restoring a large file set legitimately takes longer than any fixed
	// timeout worth setting.
	_, err = m.agents.RunRestore(ctx, agentmgr.RestoreRequest{
		RunID:    run.ID,
		AgentID:  agent.ID,
		JobName:  run.JobName,
		DestPath: spec.Agent.DestPath,
		Manifest: man,
		Engine:   eng,
		Session:  sess,
	})
	return err
}

// vmidFromSourceID extracts the guest id from a "<hostID>_<vmid>" source id.
func vmidFromSourceID(sourceID string) (int, error) {
	i := strings.LastIndex(sourceID, "_")
	if i < 0 {
		return 0, fmt.Errorf("sched: cannot derive vmid from source id %q", sourceID)
	}
	vmid, err := strconv.Atoi(sourceID[i+1:])
	if err != nil {
		return 0, fmt.Errorf("sched: cannot derive vmid from source id %q: %w", sourceID, err)
	}
	return vmid, nil
}
