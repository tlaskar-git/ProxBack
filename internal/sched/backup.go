package sched

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"proxback/internal/agentmgr"
	"proxback/internal/engine"
	"proxback/internal/pve"
	"proxback/internal/store"
)

// SnapshotTaskTimeout bounds how long we wait for PVE snapshot tasks.
const SnapshotTaskTimeout = 10 * time.Minute

func (m *Manager) progressFn(runID string) engine.ProgressFunc {
	return func(s engine.Stats) {
		if err := m.st.UpdateRunProgress(m.detached(), runID,
			s.BytesProcessed, s.BytesUploaded, s.Pct, s.CurrentStep); err != nil {
			m.log.Warn("could not persist run progress", "run", runID, "error", err)
		}
	}
}

func statsPtr(sess *engine.Session) *engine.Stats {
	s := sess.Stats()
	return &s
}

func (m *Manager) executeBackup(ctx context.Context, run *store.JobRun, job *store.Job) (stats *engine.Stats, runErr error) {
	eng, target, err := m.engineFor(ctx, job.TargetID)
	if err != nil {
		return nil, err
	}
	m.markTargetActive(target.ID)
	defer func() {
		if !m.markTargetIdle(target.ID) {
			return
		}
		// Only a run that got all the way to its manifests may collect orphans. A
		// cancelled or failed run has uploaded chunks that no manifest references
		// yet; collecting them would throw away exactly the work its retry wants
		// to deduplicate against. (The engine's grace window is the second line of
		// defence, for runs whose process died outright.)
		if runErr != nil || ctx.Err() != nil {
			m.log.Info("skipping orphan chunk collection after an unsuccessful run",
				"target", target.ID, "run", run.ID)
			m.logRun(ctx, run.ID,
				"skipped garbage collection: the run did not complete, so its uploaded chunks are kept for the next attempt")
			return
		}
		res, err := eng.GC(m.detached())
		if err != nil {
			m.log.Error("garbage collection failed", "target", target.ID, "error", err)
			m.logRun(ctx, run.ID, "warning: garbage collection failed: %v", err)
			return
		}
		if res.ChunksSkippedRecent > 0 {
			m.log.Info("recent chunks spared by the collection grace window",
				"target", target.ID, "chunksSkippedRecent", res.ChunksSkippedRecent,
				"bytesSkippedRecent", res.BytesSkippedRecent)
			m.logRun(ctx, run.ID, "garbage collection kept %s of recently uploaded chunks (%s)",
				humanBytes(res.BytesSkippedRecent), countNoun(res.ChunksSkippedRecent, "chunk"))
		}
		if res.ChunksDeleted > 0 {
			m.log.Info("orphan chunks collected", "target", target.ID,
				"chunks", res.ChunksDeleted, "bytes", res.BytesFreed)
			m.logRun(ctx, run.ID, "garbage collection freed %s (%s)",
				humanBytes(res.BytesFreed), countNoun(res.ChunksDeleted, "orphan chunk"))
		}
	}()

	switch job.Kind {
	case store.SourceVM:
		return m.backupVMJob(ctx, run, job, eng)
	case store.SourceAgent:
		return m.backupAgentJob(ctx, run, job, eng)
	default:
		return nil, fmt.Errorf("sched: unsupported job kind %q", job.Kind)
	}
}

// ---------------------------------------------------------------- vm backups

type vmPlan struct {
	client   *pve.Client
	hostID   string
	node     string
	vmid     int
	name     string
	sourceID string
	disks    []pve.DiskInfo
	// helper is the node helper that owns this guest's node, or nil when the node
	// has none and the export extension has to be tried instead.
	helper     *store.NodeHelper
	totalBytes int64
}

// resolveTagFilter expands a job's tag filter into concrete VM sources from the
// cached inventory. Membership is therefore dynamic: guests tagged in Proxmox
// after the job was created are picked up on the next run.
func (m *Manager) resolveTagFilter(ctx context.Context, job *store.Job) (store.JobSources, error) {
	vms, err := m.st.ListCachedVMs(ctx)
	if err != nil {
		return nil, err
	}
	out := store.JobSources{}
	for _, v := range vms {
		if !v.HasTag(job.TagFilter) {
			continue
		}
		out = append(out, store.JobSource{HostID: v.HostID, VMID: v.VMID, Name: v.Name})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no VMs carry tag %q", job.TagFilter)
	}
	m.log.Info("tag filter resolved", "job", job.Name, "tag", job.TagFilter, "vms", len(out))
	return out, nil
}

func (m *Manager) planVMJob(ctx context.Context, job *store.Job) ([]vmPlan, int64, error) {
	sources := job.Sources
	if job.TagFilter != "" {
		resolved, err := m.resolveTagFilter(ctx, job)
		if err != nil {
			return nil, 0, err
		}
		sources = resolved
	}
	if len(sources) == 0 {
		return nil, 0, errors.New("sched: vm job has no sources")
	}
	var total int64
	plans := make([]vmPlan, 0, len(sources))
	clients := map[string]*pve.Client{}
	for _, src := range sources {
		if src.HostID == "" || src.VMID == 0 {
			return nil, 0, errors.New("sched: vm job source needs hostId and vmid")
		}
		client, ok := clients[src.HostID]
		if !ok {
			host, err := m.st.PVEHostByID(ctx, src.HostID)
			if err != nil {
				return nil, 0, fmt.Errorf("load proxmox host: %w", err)
			}
			client, err = PVEClient(host)
			if err != nil {
				return nil, 0, err
			}
			clients[src.HostID] = client
		}
		p := vmPlan{client: client, hostID: src.HostID, vmid: src.VMID, name: src.Name}
		p.sourceID = VMSourceID(src.HostID, src.VMID)
		if cached, err := m.st.CachedVM(ctx, src.HostID, src.VMID); err == nil {
			p.node = cached.Node
			if p.name == "" {
				p.name = cached.Name
			}
		}
		if p.node == "" {
			node, err := client.FindNodeForVM(ctx, src.VMID)
			if err != nil {
				return nil, 0, err
			}
			p.node = node
		}
		cfg, err := client.Config(ctx, p.node, src.VMID)
		if err != nil {
			return nil, 0, err
		}
		if p.name == "" {
			if n, ok := cfg["name"].(string); ok && n != "" {
				p.name = n
			} else {
				p.name = "vm-" + strconv.Itoa(src.VMID)
			}
		}
		p.disks = pve.ParseDisks(cfg)
		if len(p.disks) == 0 {
			return nil, 0, fmt.Errorf("sched: vm %d has no backup-eligible disks", src.VMID)
		}
		// The guest's disk sizes are the progress estimate for both paths; a
		// vzdump archive's real size is only known once it has been produced.
		for _, d := range p.disks {
			p.totalBytes += d.SizeBytes
		}
		if p.helper, err = m.helperForNode(ctx, p.node); err != nil {
			return nil, 0, err
		}
		total += p.totalBytes
		plans = append(plans, p)
	}
	return plans, total, nil
}

func (m *Manager) backupVMJob(ctx context.Context, run *store.JobRun, job *store.Job, eng *engine.Engine) (*engine.Stats, error) {
	plans, total, err := m.planVMJob(ctx, job)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(plans))
	for _, p := range plans {
		names = append(names, p.name)
	}
	if job.TagFilter != "" {
		m.logRun(ctx, run.ID, "resolved tag %q to %s: %s",
			job.TagFilter, countNoun(len(plans), "VM"), strings.Join(names, ", "))
	} else {
		m.logRun(ctx, run.ID, "backing up %s: %s",
			countNoun(len(plans), "VM"), strings.Join(names, ", "))
	}
	sess := eng.NewSession(total, m.progressFn(run.ID))
	snapname := "proxback-" + run.ID[:8]

	for _, p := range plans {
		if err := ctx.Err(); err != nil {
			return statsPtr(sess), err
		}
		before := sess.Stats()

		var disks []engine.DiskManifest
		if p.helper != nil {
			// vzdump owns snapshot consistency, so ProxBack must not create one.
			m.logRun(ctx, run.ID, "%s: starting vzdump stream via helper on node %s", p.name, p.node)
			var err error
			if disks, err = m.backupViaHelper(ctx, run.ID, sess, p); err != nil {
				return statsPtr(sess), err
			}
		} else {
			m.logRun(ctx, run.ID, "%s: starting %s via Proxmox disk export on node %s",
				p.name, countNoun(len(p.disks), "disk"), p.node)
			sess.SetStep("Snapshotting " + p.name)
			upid, err := p.client.CreateSnapshot(ctx, p.node, p.vmid, snapname)
			if err != nil {
				return statsPtr(sess), fmt.Errorf("snapshot %s: %w", p.name, err)
			}
			if err := p.client.WaitTask(ctx, p.node, upid, SnapshotTaskTimeout); err != nil {
				return statsPtr(sess), fmt.Errorf("snapshot %s: %w", p.name, err)
			}
			if disks, err = m.exportDisks(ctx, run.ID, sess, p, snapname); err != nil {
				return statsPtr(sess), err
			}
		}
		after := sess.Stats()

		parentID, err := m.parentFor(ctx, store.SourceVM, p.sourceID, eng.TargetID())
		if err != nil {
			return statsPtr(sess), err
		}

		var size int64
		storeDisks := make([]store.Disk, 0, len(disks))
		for _, d := range disks {
			size += d.SizeBytes
			storeDisks = append(storeDisks, store.Disk{Name: d.Name, SizeBytes: d.SizeBytes})
		}
		uploaded := after.BytesUploaded - before.BytesUploaded

		backupID := store.NewID()
		man := &engine.Manifest{
			BackupID:      backupID,
			JobID:         job.ID,
			JobName:       job.Name,
			RunID:         run.ID,
			SourceKind:    store.SourceVM,
			SourceID:      p.sourceID,
			SourceName:    p.name,
			TargetID:      eng.TargetID(),
			CreatedAt:     store.Now(),
			Kind:          engine.Kind(parentID),
			ParentID:      parentID,
			SizeBytes:     size,
			UploadedBytes: uploaded,
			ChunkSize:     engine.ChunkSize,
			Disks:         disks,
		}
		sess.SetStep("Writing manifest for " + p.name)
		if err := eng.WriteManifest(ctx, man); err != nil {
			return statsPtr(sess), err
		}
		if _, err := m.st.CreateBackup(ctx, &store.Backup{
			ID:            backupID,
			JobID:         job.ID,
			RunID:         run.ID,
			SourceKind:    store.SourceVM,
			SourceID:      p.sourceID,
			SourceName:    p.name,
			TargetID:      eng.TargetID(),
			CreatedAt:     man.CreatedAt,
			SizeBytes:     size,
			UploadedBytes: uploaded,
			Kind:          man.Kind,
			ParentID:      parentID,
			Disks:         storeDisks,
		}); err != nil {
			return statsPtr(sess), err
		}
		m.logSourceDone(ctx, run.ID, p.name, after.BytesProcessed-before.BytesProcessed, uploaded, man.Kind)
		if err := m.applyRetention(ctx, run.ID, eng, job, store.SourceVM, p.sourceID); err != nil {
			return statsPtr(sess), err
		}
	}

	sess.SetStep("Completed")
	sess.Flush()
	return statsPtr(sess), nil
}

// exportDisks streams every disk of the snapshot through the engine, always
// removing the snapshot afterwards.
func (m *Manager) exportDisks(ctx context.Context, runID string, sess *engine.Session, p vmPlan, snapname string) (out []engine.DiskManifest, err error) {
	defer func() {
		cleanup := m.detached()
		upid, derr := p.client.DeleteSnapshot(cleanup, p.node, p.vmid, snapname)
		if derr != nil {
			m.log.Warn("could not remove snapshot", "vm", p.vmid, "snapshot", snapname, "error", derr)
			m.logRun(ctx, runID, "warning: %s: could not remove snapshot %s: %v", p.name, snapname, derr)
			return
		}
		if werr := p.client.WaitTask(cleanup, p.node, upid, time.Minute); werr != nil {
			m.log.Warn("snapshot removal task failed", "vm", p.vmid, "snapshot", snapname, "error", werr)
			m.logRun(ctx, runID, "warning: %s: removing snapshot %s failed: %v", p.name, snapname, werr)
		}
	}()
	for _, d := range p.disks {
		sess.SetStep(fmt.Sprintf("Backing up %s %s", p.name, d.Name))
		stream, err := p.client.ExportDisk(ctx, p.node, p.vmid, d.Name, snapname)
		if err != nil {
			// Only the simulator implements the export extension. On a real node
			// the answer is "no such endpoint", which means the node needs a helper.
			return nil, mapExportError(err, p.node)
		}
		dm, err := sess.BackupStream(ctx, d.Name, stream)
		closeErr := stream.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			m.log.Warn("closing disk export stream", "vm", p.vmid, "disk", d.Name, "error", closeErr)
		}
		out = append(out, dm)
	}
	return out, nil
}

// ---------------------------------------------------------------- agent backups

func (m *Manager) backupAgentJob(ctx context.Context, run *store.JobRun, job *store.Job, eng *engine.Engine) (*engine.Stats, error) {
	if len(job.Sources) == 0 || job.Sources[0].AgentID == "" {
		return nil, errors.New("sched: agent job needs an agentId source")
	}
	src := job.Sources[0]
	agent, err := m.st.AgentByID(ctx, src.AgentID)
	if err != nil {
		return nil, fmt.Errorf("load agent: %w", err)
	}
	if !agentmgr.Online(agent) {
		return nil, fmt.Errorf("sched: agent %s is offline", agent.Hostname)
	}
	if len(src.Paths) == 0 {
		return nil, errors.New("sched: agent job needs at least one include path")
	}
	for _, p := range src.Paths {
		if strings.TrimSpace(p) == "" {
			return nil, errors.New("sched: agent job include paths must not be empty")
		}
	}

	sess := eng.NewSession(0, m.progressFn(run.ID))
	m.logRun(ctx, run.ID, "%s: starting file backup via agent (%s: %s)",
		agent.Hostname, countNoun(len(src.Paths), "path"), strings.Join(src.Paths, ", "))
	sess.SetStep("Dispatching to agent " + agent.Hostname)

	dctx, cancel := context.WithTimeout(ctx, AgentDispatchTimeout)
	defer cancel()
	res, err := m.agents.RunBackup(dctx, agentmgr.BackupRequest{
		RunID:   run.ID,
		AgentID: agent.ID,
		JobID:   job.ID,
		JobName: job.Name,
		Paths:   src.Paths,
		Engine:  eng,
		Session: sess,
	})
	if err != nil {
		return statsPtr(sess), err
	}

	parentID, err := m.parentFor(ctx, store.SourceAgent, agent.ID, eng.TargetID())
	if err != nil {
		return statsPtr(sess), err
	}

	var size int64
	storeDisks := make([]store.Disk, 0, len(res.Disks))
	for _, d := range res.Disks {
		size += d.SizeBytes
		storeDisks = append(storeDisks, store.Disk{Name: d.Name, SizeBytes: d.SizeBytes})
	}
	stats := sess.Stats()

	backupID := store.NewID()
	man := &engine.Manifest{
		BackupID:      backupID,
		JobID:         job.ID,
		JobName:       job.Name,
		RunID:         run.ID,
		SourceKind:    store.SourceAgent,
		SourceID:      agent.ID,
		SourceName:    agent.Hostname,
		TargetID:      eng.TargetID(),
		CreatedAt:     store.Now(),
		Kind:          engine.Kind(parentID),
		ParentID:      parentID,
		SizeBytes:     size,
		UploadedBytes: stats.BytesUploaded,
		ChunkSize:     engine.ChunkSize,
		Disks:         res.Disks,
	}
	sess.SetStep("Writing manifest for " + agent.Hostname)
	if err := eng.WriteManifest(ctx, man); err != nil {
		return statsPtr(sess), err
	}
	if _, err := m.st.CreateBackup(ctx, &store.Backup{
		ID:            backupID,
		JobID:         job.ID,
		RunID:         run.ID,
		SourceKind:    store.SourceAgent,
		SourceID:      agent.ID,
		SourceName:    agent.Hostname,
		TargetID:      eng.TargetID(),
		CreatedAt:     man.CreatedAt,
		SizeBytes:     size,
		UploadedBytes: stats.BytesUploaded,
		Kind:          man.Kind,
		ParentID:      parentID,
		Disks:         storeDisks,
	}); err != nil {
		return statsPtr(sess), err
	}
	m.logSourceDone(ctx, run.ID, agent.Hostname, stats.BytesProcessed, stats.BytesUploaded, man.Kind)
	if err := m.applyRetention(ctx, run.ID, eng, job, store.SourceAgent, agent.ID); err != nil {
		return statsPtr(sess), err
	}
	sess.SetStep("Completed")
	sess.Flush()
	return statsPtr(sess), nil
}

// ---------------------------------------------------------------- shared

// logSourceDone records the one line an operator reads to know what a source
// actually cost: bytes seen, bytes that had to travel, and how much of it the
// chunk index already knew.
func (m *Manager) logSourceDone(ctx context.Context, runID, name string, processed, uploaded int64, kind string) {
	dedup := 0.0
	if processed > 0 {
		if dedup = (1 - float64(uploaded)/float64(processed)) * 100; dedup < 0 {
			dedup = 0
		}
	}
	m.logRun(ctx, runID, "%s: finished — %s processed, %s uploaded, %.0f%% deduplicated (%s restore point)",
		name, humanBytes(processed), humanBytes(uploaded), dedup, kind)
}

// parentFor returns the id of the previous restore point for a source on a
// target, which is what makes the next backup incremental.
func (m *Manager) parentFor(ctx context.Context, sourceKind, sourceID, targetID string) (string, error) {
	prev, err := m.st.LatestBackupForSource(ctx, sourceKind, sourceID, targetID)
	switch {
	case err == nil:
		return prev.ID, nil
	case errors.Is(err, store.ErrNotFound):
		return "", nil
	default:
		return "", err
	}
}

// applyRetention prunes restore points beyond the job's keep-last-N window.
func (m *Manager) applyRetention(ctx context.Context, runID string, eng *engine.Engine, job *store.Job, sourceKind, sourceID string) error {
	keep := job.Retention
	if keep <= 0 {
		return nil
	}
	list, err := m.st.ListBackups(ctx, store.BackupFilter{
		SourceKind: sourceKind,
		SourceID:   sourceID,
		TargetID:   eng.TargetID(),
	})
	if err != nil {
		return err
	}
	if len(list) <= keep {
		return nil
	}
	pruned := 0
	for _, b := range list[keep:] {
		if err := eng.DeleteManifest(ctx, b.SourceKind, b.SourceID, b.ID); err != nil {
			return err
		}
		if err := m.st.ClearBackupParent(ctx, b.ID); err != nil {
			return err
		}
		if err := m.st.DeleteBackup(ctx, b.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		pruned++
		m.log.Info("retention pruned restore point", "backup", b.ID,
			"source", b.SourceName, "job", job.Name, "keep", keep)
	}
	if pruned > 0 {
		m.logRun(ctx, runID, "%s: retention pruned %s (keeping the last %d)",
			list[0].SourceName, countNoun(pruned, "restore point"), keep)
	}
	return nil
}

// DeleteBackup removes a restore point, its manifest, and garbage collects any
// chunks that are now orphaned.
func (m *Manager) DeleteBackup(ctx context.Context, backupID string) error {
	b, err := m.st.BackupByID(ctx, backupID)
	if err != nil {
		return err
	}
	eng, target, err := m.engineFor(ctx, b.TargetID)
	if err != nil {
		return err
	}
	if err := eng.DeleteManifest(ctx, b.SourceKind, b.SourceID, b.ID); err != nil {
		return err
	}
	if err := m.st.ClearBackupParent(ctx, b.ID); err != nil {
		return err
	}
	if err := m.st.DeleteBackup(ctx, b.ID); err != nil {
		return err
	}
	m.mu.Lock()
	idle := m.targetActive[target.ID] == 0
	m.mu.Unlock()
	if idle {
		if _, err := eng.GC(ctx); err != nil {
			return err
		}
	}
	return nil
}
