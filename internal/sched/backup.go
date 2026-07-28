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
		ctx := m.detached()
		if err := m.st.UpdateRunProgress(ctx, runID,
			s.BytesProcessed, s.BytesUploaded, s.Pct, s.CurrentStep); err != nil {
			m.log.Warn("could not persist run progress", "run", runID, "error", err)
		}
		// The same callback advances the source in flight and takes the
		// throughput sample; both are throttled inside the monitor.
		if mon := m.monitorFor(runID); mon != nil {
			mon.progress(ctx, s)
		}
	}
}

func statsPtr(sess *engine.Session) *engine.Stats {
	s := sess.Stats()
	return &s
}

// executeBackup runs a job under its protection policy: inside the run's
// maximum duration, retried as many times as the policy allows, and with the
// policy's transfer ceiling in force.
func (m *Manager) executeBackup(ctx context.Context, run *store.JobRun, job *store.Job) (*engine.Stats, error) {
	policy := job.Policy.Normalized()
	if policy.UploadLimitMbpsOverride > 0 {
		m.logRun(ctx, run.ID, "policy: upload capped at %d Mbps for this run",
			policy.UploadLimitMbpsOverride)
		// Whatever happens to the run, the server-wide ceiling goes back.
		defer m.applyGlobalUploadLimit(m.detached())
	}

	return m.runUnderPolicy(ctx, run.ID, policy, func(attemptCtx context.Context) (*engine.Stats, error) {
		return m.backupAttempt(attemptCtx, run, job, policy)
	})
}

// runUnderPolicy applies the run-control half of a protection policy to one
// piece of work: the duration limit that bounds it and the retries that follow
// a failure. It is separate from what the work *is* so the policy can be
// exercised — and reasoned about — without a Proxmox host behind it.
//
// A cancellation is never retried. An operator who cancels a run means it, and
// a run that hit its own duration limit has already been told to stop.
func (m *Manager) runUnderPolicy(ctx context.Context, runID string, policy store.JobPolicy,
	attempt func(context.Context) (*engine.Stats, error)) (*engine.Stats, error) {
	runCtx := ctx
	if policy.MaxDurationMinutes > 0 {
		m.logRun(ctx, runID, "policy: this run is limited to %s",
			countNoun(policy.MaxDurationMinutes, "minute"))
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, m.policyMinutes(policy.MaxDurationMinutes))
		defer cancel()
	}

	attempts := policy.RetryCount + 1
	var stats *engine.Stats
	var err error
	for n := 1; n <= attempts; n++ {
		if n > 1 {
			m.logRun(ctx, runID, "attempt %d of %d starting", n, attempts)
		}
		stats, err = attempt(runCtx)
		if err == nil {
			if n > 1 {
				m.logRun(ctx, runID, "attempt %d of %d succeeded", n, attempts)
			}
			return stats, nil
		}
		if runCtx.Err() != nil || errors.Is(err, context.Canceled) {
			break
		}
		if n == attempts {
			if attempts > 1 {
				m.logRun(ctx, runID, "attempt %d of %d failed: %v — no attempts left", n, attempts, err)
			}
			break
		}
		m.logRun(ctx, runID, "attempt %d of %d failed: %v — retrying in %s",
			n, attempts, err, countNoun(policy.RetryDelayMinutes, "minute"))
		if werr := m.retryWait(runCtx, m.policyMinutes(policy.RetryDelayMinutes)); werr != nil {
			// The wait was cut short by cancellation or by the duration limit;
			// the failure that prompted the retry is the one worth reporting.
			break
		}
	}
	// A run killed by its own duration limit says so, rather than surfacing as
	// whatever the interrupted transfer happened to complain about.
	if policy.MaxDurationMinutes > 0 && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return stats, fmt.Errorf("%w of %s, so it was canceled",
			ErrMaxDuration, countNoun(policy.MaxDurationMinutes, "minute"))
	}
	return stats, err
}

// backupAttempt is one attempt at a job: build the engine, run the policy's
// pre-script where the data lives, back the sources up, run the post-script,
// and collect orphan chunks if the attempt got all the way through.
func (m *Manager) backupAttempt(ctx context.Context, run *store.JobRun, job *store.Job,
	policy store.JobPolicy) (stats *engine.Stats, runErr error) {
	eng, target, err := m.engineForJob(ctx, job.TargetID, policy.UploadLimitMbpsOverride)
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
		return m.backupVMJob(ctx, run, job, policy, eng)
	case store.SourceAgent:
		return m.backupAgentJob(ctx, run, job, policy, eng)
	default:
		return nil, fmt.Errorf("sched: unsupported job kind %q", job.Kind)
	}
}

// ---------------------------------------------------------------- vm backups

type vmPlan struct {
	client *pve.Client
	hostID string
	// hostName is the cluster's display name, carried so restore points and run
	// sources can identify a workload as "cluster / name (vmid) / node".
	hostName string
	node     string
	vmid     int
	name     string
	sourceID string
	disks    []pve.DiskInfo
	// helper is the node helper that owns this guest's node, or nil when the node
	// has none and the export extension has to be tried instead.
	helper *store.NodeHelper
	// guestAgent records whether qemu-guest-agent is enabled in the guest's
	// configuration, which is what decides whether a requested filesystem freeze
	// can actually happen.
	guestAgent bool
	// excluded names the disks the policy left out of this guest's backup.
	excluded   []string
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

// applyDiskExclusions removes the disks the policy leaves out and reports which
// ones went. Excluding every disk is refused: a restore point with no data in
// it is not a backup, and the operator meant something else.
func applyDiskExclusions(disks []pve.DiskInfo, exclude []string, vmName string) ([]pve.DiskInfo, []string, error) {
	if len(exclude) == 0 {
		return disks, nil, nil
	}
	skip := make(map[string]struct{}, len(exclude))
	for _, d := range exclude {
		skip[strings.ToLower(strings.TrimSpace(d))] = struct{}{}
	}
	kept := make([]pve.DiskInfo, 0, len(disks))
	var dropped []string
	for _, d := range disks {
		if _, off := skip[strings.ToLower(d.Name)]; off {
			dropped = append(dropped, d.Name)
			continue
		}
		kept = append(kept, d)
	}
	if len(kept) == 0 {
		return nil, nil, fmt.Errorf("sched: policy.excludeDisks leaves %s with no disks to back up", vmName)
	}
	return kept, dropped, nil
}

func (m *Manager) planVMJob(ctx context.Context, job *store.Job, policy store.JobPolicy) ([]vmPlan, int64, error) {
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
	hostNames := map[string]string{}
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
			hostNames[src.HostID] = host.Name
		}
		p := vmPlan{
			client: client, hostID: src.HostID, hostName: hostNames[src.HostID],
			vmid: src.VMID, name: src.Name,
		}
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
		p.guestAgent = guestAgentEnabled(cfg)
		if p.disks, p.excluded, err = applyDiskExclusions(p.disks, policy.ExcludeDisks, p.name); err != nil {
			return nil, 0, err
		}
		// The guest's disk sizes are the progress estimate for both paths; a
		// vzdump archive's real size is only known once it has been produced.
		for _, d := range p.disks {
			p.totalBytes += d.SizeBytes
		}
		if p.helper, err = m.helperForNode(ctx, p.hostID, p.node); err != nil {
			return nil, 0, err
		}
		// A helper streams the whole guest as one vzdump archive, so a per-disk
		// exclusion cannot be expressed on that path. Rather than store an
		// archive that quietly contains the disk the policy excludes, the run
		// stops here and says what to do instead.
		if p.helper != nil && len(policy.ExcludeDisks) > 0 {
			return nil, 0, fmt.Errorf("%w: %s is backed up by the node helper on %s as a single "+
				"whole-VM vzdump archive, which cannot leave %s out — exclude the disk in the guest's "+
				"own configuration instead (set backup=0 on it in Proxmox), or drop policy.excludeDisks",
				ErrExcludeDisksUnsupported, p.name, p.node, strings.Join(policy.ExcludeDisks, ", "))
		}
		total += p.totalBytes
		plans = append(plans, p)
	}
	return plans, total, nil
}

func (m *Manager) backupVMJob(ctx context.Context, run *store.JobRun, job *store.Job,
	policy store.JobPolicy, eng *engine.Engine) (*engine.Stats, error) {
	plans, total, err := m.planVMJob(ctx, job, policy)
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

	// Every guest is recorded up front, pending and with its known size, so the
	// monitor can draw the whole run — one row per VM, advancing independently —
	// from the moment it starts.
	mon := m.monitorFor(run.ID)
	if mon != nil {
		planned := make([]store.RunSource, 0, len(plans))
		for i, p := range plans {
			planned = append(planned, store.RunSource{
				Seq: i, Name: p.name, Kind: store.SourceVM,
				SourceID: p.sourceID, HostID: p.hostID, HostName: p.hostName, Node: p.node,
				Status: store.SourcePending, SizeBytes: p.totalBytes,
			})
		}
		mon.plan(ctx, planned)
	}

	for i, p := range plans {
		if err := ctx.Err(); err != nil {
			return statsPtr(sess), err
		}
		before := sess.Stats()
		if mon != nil {
			mon.begin(ctx, i, before)
		}
		err := m.backupOneVM(ctx, run, job, policy, eng, sess, p, snapname, before)
		if mon != nil {
			mon.finish(ctx, sourceOutcome(err), sess.Stats(), errText(err))
		}
		if err != nil {
			return statsPtr(sess), err
		}
	}

	sess.SetStep("Completed")
	sess.Flush()
	return statsPtr(sess), nil
}

// backupOneVM backs up a single guest: export (helper or Proxmox extension),
// manifest, restore point, retention. before is the run's byte count when the
// guest started, which is what makes the per-source figures deltas of the
// session's shared counters.
func (m *Manager) backupOneVM(ctx context.Context, run *store.JobRun, job *store.Job, policy store.JobPolicy,
	eng *engine.Engine, sess *engine.Session, p vmPlan, snapname string, before engine.Stats) error {
	if len(p.excluded) > 0 {
		m.logRun(ctx, run.ID, "%s: policy excludes %s — %s will be backed up",
			p.name, strings.Join(p.excluded, ", "), countNoun(len(p.disks), "disk"))
	}
	m.logQuiesce(ctx, run.ID, policy, p)
	// The pre-script runs before a single byte of this guest moves, so a
	// failure here leaves the estate exactly as it was.
	if err := m.runVMScript(ctx, run.ID, policy, p, phasePre); err != nil {
		return err
	}
	var disks []engine.DiskManifest
	if p.helper != nil {
		// vzdump owns snapshot consistency, so ProxBack must not create one.
		m.logRun(ctx, run.ID, "%s: starting vzdump stream via helper on node %s", p.name, p.node)
		var err error
		if disks, err = m.backupViaHelper(ctx, run.ID, sess, p); err != nil {
			return err
		}
	} else {
		m.logRun(ctx, run.ID, "%s: starting %s via Proxmox disk export on node %s",
			p.name, countNoun(len(p.disks), "disk"), p.node)
		sess.SetStep("Snapshotting " + p.name)
		upid, err := p.client.CreateSnapshot(ctx, p.node, p.vmid, snapname)
		if err != nil {
			return fmt.Errorf("snapshot %s: %w", p.name, err)
		}
		if err := p.client.WaitTask(ctx, p.node, upid, SnapshotTaskTimeout); err != nil {
			return fmt.Errorf("snapshot %s: %w", p.name, err)
		}
		if disks, err = m.exportDisks(ctx, run.ID, sess, p, snapname); err != nil {
			return err
		}
	}
	after := sess.Stats()

	parentID, err := m.parentFor(ctx, store.SourceVM, p.sourceID, eng.TargetID())
	if err != nil {
		return err
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
		return err
	}
	if _, err := m.st.CreateBackup(ctx, &store.Backup{
		ID:            backupID,
		JobID:         job.ID,
		RunID:         run.ID,
		SourceKind:    store.SourceVM,
		SourceID:      p.sourceID,
		SourceName:    p.name,
		HostID:        p.hostID,
		HostName:      p.hostName,
		TargetID:      eng.TargetID(),
		CreatedAt:     man.CreatedAt,
		SizeBytes:     size,
		UploadedBytes: uploaded,
		Kind:          man.Kind,
		ParentID:      parentID,
		Disks:         storeDisks,
	}); err != nil {
		return err
	}
	m.logSourceDone(ctx, run.ID, p.name, after.BytesProcessed-before.BytesProcessed, uploaded, man.Kind)
	// The post-script runs once the restore point exists. A failure fails the
	// run — the operator asked for the script to matter — but the restore point
	// stays: it is real, it is verifiable, and throwing it away would punish the
	// data for the script's mistake.
	if err := m.runVMScript(ctx, run.ID, policy, p, phasePost); err != nil {
		m.logRun(ctx, run.ID, "%s: the restore point taken before the post-script is kept", p.name)
		return err
	}
	return m.applyRetention(ctx, run.ID, eng, job, store.SourceVM, p.sourceID)
}

// sourceOutcome maps a source's outcome to its recorded status. A cancellation
// is not the source's fault, so it is recorded as skipped rather than failed.
func sourceOutcome(err error) string {
	switch {
	case err == nil:
		return store.SourceSuccess
	case errors.Is(err, context.Canceled):
		return store.SourceSkipped
	default:
		return store.SourceFailed
	}
}

// errText renders an error for a run source row.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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

func (m *Manager) backupAgentJob(ctx context.Context, run *store.JobRun, job *store.Job,
	policy store.JobPolicy, eng *engine.Engine) (stats *engine.Stats, runErr error) {
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
	// An agent job walks exactly one source; its size is only known once the
	// agent has streamed it, so the row starts at zero bytes.
	mon := m.monitorFor(run.ID)
	if mon != nil {
		mon.plan(ctx, []store.RunSource{{
			Seq: 0, Name: agent.Hostname, Kind: store.SourceAgent, SourceID: agent.ID,
			Status: store.SourcePending,
		}})
		mon.begin(ctx, 0, sess.Stats())
		defer func() {
			mon.finish(m.detached(), sourceOutcome(runErr), sess.Stats(), errText(runErr))
		}()
	}
	m.logRun(ctx, run.ID, "%s: starting file backup via agent (%s: %s)",
		agent.Hostname, countNoun(len(src.Paths), "path"), strings.Join(src.Paths, ", "))
	// The walk, the exclusions and the scripts all happen inside the guest,
	// where the files are: they travel with the dispatch and the agent applies
	// them. The run log says "sent", not "applied", because this side of the
	// wire cannot witness what the agent did with them.
	if len(policy.ExcludePaths) > 0 {
		m.logRun(ctx, run.ID, "%s: policy sends %s to exclude from the walk: %s",
			agent.Hostname, countNoun(len(policy.ExcludePaths), "pattern"),
			strings.Join(policy.ExcludePaths, ", "))
	}
	if policy.HasScripts() {
		m.logRun(ctx, run.ID, "%s: policy scripts are dispatched to the agent to run in the guest (timeout %ds)",
			agent.Hostname, policy.ScriptTimeoutSecondsOrDefault())
	}
	sess.SetStep("Dispatching to agent " + agent.Hostname)

	/* No deadline of our own here. agentmgr bounds the wait for the agent to
	   collect the work and the silence afterwards; ctx already carries
	   policy.maxDurationMinutes, which is the operator's cap on run length. A
	   flat timeout across the whole call used to kill healthy backups the
	   moment they ran longer than it, however much data they were moving. */
	res, err := m.agents.RunBackup(ctx, agentmgr.BackupRequest{
		RunID:                run.ID,
		AgentID:              agent.ID,
		JobID:                job.ID,
		JobName:              job.Name,
		Paths:                src.Paths,
		ExcludePaths:         policy.ExcludePaths,
		PreScript:            policy.PreScript,
		PostScript:           policy.PostScript,
		ScriptTimeoutSeconds: policy.ScriptTimeoutSecondsOrDefault(),
		Engine:               eng,
		Session:              sess,
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
	// The agent's stream size only becomes known here, so the source row learns
	// its total after the fact rather than at plan time.
	if mon != nil {
		mon.setSize(ctx, 0, size)
	}
	snapshot := sess.Stats()

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
		UploadedBytes: snapshot.BytesUploaded,
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
		UploadedBytes: snapshot.BytesUploaded,
		Kind:          man.Kind,
		ParentID:      parentID,
		Disks:         storeDisks,
	}); err != nil {
		return statsPtr(sess), err
	}
	m.logSourceDone(ctx, run.ID, agent.Hostname, snapshot.BytesProcessed, snapshot.BytesUploaded, man.Kind)
	if err := m.applyRetention(ctx, run.ID, eng, job, store.SourceAgent, agent.ID); err != nil {
		return statsPtr(sess), err
	}
	sess.SetStep("Completed")
	sess.Flush()
	return statsPtr(sess), nil
}

// ---------------------------------------------------------------- shared

// logSourceDone records the one line an operator reads to know what a source
// actually cost: bytes seen, bytes that had to travel, and how much of the
// traffic that saved. The reduction phrase comes from store.ReductionSummary,
// the single definition the API reports too, so the log and the UI can never
// describe the same run differently.
func (m *Manager) logSourceDone(ctx context.Context, runID, name string, processed, uploaded int64, kind string) {
	m.logRun(ctx, runID, "%s: finished — %s processed, %s uploaded, %s (%s restore point)",
		name, humanBytes(processed), humanBytes(uploaded),
		store.ReductionSummary(processed, uploaded), kind)
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

// applyRetention prunes the restore points of one source that the job's GFS
// policy does not retain. The decision itself is store.EvaluateRetention — the
// same pure function GET /api/jobs/{id}/retention-preview answers with — so
// what an operator was shown before saving is what actually happens.
func (m *Manager) applyRetention(ctx context.Context, runID string, eng *engine.Engine, job *store.Job, sourceKind, sourceID string) error {
	policy := job.Retention
	if policy.Total() <= 0 {
		// No rule retains anything. That is how a pre-v0.5.0 "retention 0"
		// (which meant "never prune") reads, and it is also the one policy that
		// would leave nothing to recover from, so nothing is pruned either way.
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
	if len(list) == 0 {
		return nil
	}
	points := make([]store.RetentionPoint, 0, len(list))
	byID := make(map[string]*store.Backup, len(list))
	for _, b := range list {
		points = append(points, store.RetentionPoint{ID: b.ID, CreatedAt: b.CreatedAt})
		byID[b.ID] = b
	}
	plan := store.EvaluateRetention(points, policy)
	if len(plan.Prunes) == 0 {
		return nil
	}
	if len(plan.Keeps) == 0 {
		// A policy that retains none of the points it is shown would delete the
		// last copy of this workload. Refuse, and say why: the operator can see
		// the same verdict in the retention preview before saving.
		m.logRun(ctx, runID, "%s: retention (%s) would keep no restore point at all, so nothing was pruned",
			list[0].SourceName, policy.Describe())
		return nil
	}
	pruned := 0
	for _, decision := range plan.Prunes {
		b := byID[decision.ID]
		if b == nil {
			continue
		}
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
			"source", b.SourceName, "job", job.Name, "retention", policy.Describe())
	}
	if pruned > 0 {
		m.logRun(ctx, runID, "%s: retention (%s) pruned %s, %s kept",
			list[0].SourceName, policy.Describe(), countNoun(pruned, "restore point"),
			countNoun(len(plan.Keeps), "restore point"))
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
