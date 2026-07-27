package sched

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"proxback/internal/agentmgr"
	"proxback/internal/engine"
	"proxback/internal/store"
)

func (m *Manager) executeRestore(ctx context.Context, run *store.JobRun, backup *store.Backup, spec RestoreSpec) (*engine.Stats, error) {
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
		if err := m.restoreVM(ctx, sess, man, spec); err != nil {
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

// executeVerify re-reads every chunk of a restore point and validates it
// against the manifest. Nothing is written: success means the point restores.
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
	if err := sess.VerifyBackup(ctx, man); err != nil {
		return statsPtr(sess), err
	}
	sess.SetStep("Verified")
	sess.Flush()
	return statsPtr(sess), nil
}

func (m *Manager) restoreVM(ctx context.Context, sess *engine.Session, man *engine.Manifest, spec RestoreSpec) error {
	if spec.VM == nil {
		return errors.New("sched: vm restore target required")
	}
	host, err := m.st.PVEHostByID(ctx, spec.VM.HostID)
	if err != nil {
		return fmt.Errorf("load proxmox host: %w", err)
	}
	client, err := PVEClient(host)
	if err != nil {
		return err
	}
	sourceVMID, sourceErr := vmidFromSourceID(man.SourceID)
	vmid := spec.VM.VMID
	if vmid == 0 {
		if sourceErr != nil {
			return sourceErr
		}
		vmid = sourceVMID
	}
	viaHelper := IsVMAManifest(man.Disks)
	node := spec.VM.Node
	if node == "" {
		node, err = client.FindNodeForVM(ctx, vmid)
		if err != nil {
			// Restoring a helper-backed point into a vmid that does not exist yet
			// is the normal side-by-side case: fall back to the source's node.
			if !viaHelper || sourceErr != nil {
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
		h, err := m.requireHelperForNode(ctx, node)
		if err != nil {
			return err
		}
		// --force overwrites an existing guest, which is only ever what the
		// operator meant when restoring onto the same vmid it came from.
		force := sourceErr == nil && vmid == sourceVMID
		return m.restoreViaHelper(ctx, sess, man, h, vmid, spec.VM.Storage, force)
	}
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
	sess.SetStep("Dispatching restore to agent " + agent.Hostname)
	dctx, cancel := context.WithTimeout(ctx, AgentDispatchTimeout)
	defer cancel()
	_, err = m.agents.RunRestore(dctx, agentmgr.RestoreRequest{
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
