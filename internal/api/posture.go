package api

import (
	"net/http"
	"sort"
	"time"

	"proxback/internal/sched"
	"proxback/internal/store"
)

// Protection posture.
//
// The dashboard used to answer "are my workloads protected?" by looking at the
// newest successful run anywhere: one healthy job made the whole estate look
// green, and an estate with nothing in it reported "0 / 0 — all guests in a
// job". This endpoint answers the question the way an operator means it, one
// workload at a time:
//
//   - what job, if any, governs it (including membership by tag filter),
//   - what recovery point objective that job's schedule promises,
//   - when that workload itself last succeeded and last failed,
//   - how many restore points it actually has,
//   - when its data was last verified.
//
// Nothing is inferred from another workload's outcome, and an empty estate is
// reported as "unknown" rather than as a perfect score.

// Verdicts and per-workload statuses.
const (
	PostureProtected   = "protected"
	PostureAtRisk      = "at_risk"
	PostureUnprotected = "unprotected"
	PostureUnknown     = "unknown"
)

// Reason codes reported alongside the verdict, so the UI can explain it rather
// than just colour it.
const (
	ReasonNoJob           = "no_job"
	ReasonNoRestorePoints = "no_restore_points"
	ReasonLastRunFailed   = "last_run_failed"
	ReasonRPOExceeded     = "rpo_exceeded"
)

type postureCountsDTO struct {
	Protected   int `json:"protected"`
	AtRisk      int `json:"atRisk"`
	Unprotected int `json:"unprotected"`
}

type postureReasonDTO struct {
	Code string `json:"code"`
	// Workloads is how many workloads the reason applies to.
	Workloads int    `json:"workloads"`
	Detail    string `json:"detail"`
}

// postureWorkloadDTO is one workload's protection state.
type postureWorkloadDTO struct {
	Kind string `json:"kind"` // vm | agent
	ID   string `json:"id"`
	Name string `json:"name"`
	// VMID is the guest id an operator recognises. Absent for agents. The
	// console shows this, never the internal composite ID.
	VMID     int    `json:"vmid,omitempty"`
	HostName string `json:"hostName"`
	Node     string `json:"node"`
	// Policy is the name of the job that governs the workload, absent when none
	// does.
	Policy string `json:"policy,omitempty"`
	// Enabled reports whether that job is enabled. False when there is no job.
	Enabled bool `json:"enabled"`
	// RPOHours is the objective the job's schedule implies; absent for manual
	// and advanced schedules, which promise nothing.
	RPOHours      *float64   `json:"rpoHours,omitempty"`
	LastSuccessAt *time.Time `json:"lastSuccessAt,omitempty"`
	// AgeHours is how old the newest restore point is.
	AgeHours *float64 `json:"ageHours,omitempty"`
	// WithinRPO is whether that age is inside the objective plus its grace
	// window; absent when there is no objective to be inside of.
	WithinRPO      *bool      `json:"withinRpo,omitempty"`
	LastFailureAt  *time.Time `json:"lastFailureAt,omitempty"`
	LastVerifiedAt *time.Time `json:"lastVerifiedAt,omitempty"`
	RestorePoints  int        `json:"restorePoints"`
	Status         string     `json:"status"`
}

type postureDTO struct {
	Verdict   string               `json:"verdict"`
	Counts    postureCountsDTO     `json:"counts"`
	Reasons   []postureReasonDTO   `json:"reasons"`
	Workloads []postureWorkloadDTO `json:"workloads"`
}

// postureWorkload is one workload as the evaluator sees it, before any judgment
// is applied. Keeping the inputs in a plain struct is what makes the rules
// testable without a database.
type postureWorkload struct {
	Kind string
	ID   string
	Name string
	// VMID is the operator-facing guest id; zero for agents.
	VMID     int
	HostName string
	Node     string
	// Job is the governing enabled job, nil when the workload is in none.
	Job *store.Job
	// LastSuccessAt is when this workload itself last produced a restore point.
	LastSuccessAt *time.Time
	// LastFailureAt is when this workload itself last failed, from its own row
	// in a run's per-source breakdown.
	LastFailureAt *time.Time
	// LastOutcomeFailed reports whether the most recent finished attempt at this
	// workload failed — not whether it has ever failed.
	LastOutcomeFailed bool
	LastVerifiedAt    *time.Time
	RestorePoints     int
}

// evaluatePosture applies the protection rules to a set of workloads. It is
// pure: given the same inputs and the same "now" it always answers the same,
// which is what the posture matrix test relies on.
func evaluatePosture(workloads []postureWorkload, now time.Time) postureDTO {
	out := postureDTO{
		Verdict:   PostureUnknown,
		Reasons:   []postureReasonDTO{},
		Workloads: []postureWorkloadDTO{},
	}
	if len(workloads) == 0 {
		// An empty estate is not protected and not at risk; there is simply
		// nothing to say. Reporting "protected" here is the lie the old
		// dashboard told.
		return out
	}

	reasons := map[string]int{}
	for _, w := range workloads {
		d := postureWorkloadDTO{
			Kind: w.Kind, ID: w.ID, Name: w.Name, VMID: w.VMID,
			HostName: w.HostName, Node: w.Node,
			LastSuccessAt: w.LastSuccessAt, LastFailureAt: w.LastFailureAt,
			LastVerifiedAt: w.LastVerifiedAt, RestorePoints: w.RestorePoints,
		}
		if w.LastSuccessAt != nil {
			age := now.Sub(*w.LastSuccessAt).Hours()
			if age < 0 {
				age = 0
			}
			d.AgeHours = &age
		}
		switch {
		case w.Job == nil:
			// No enabled job covers it: nothing is even trying.
			d.Status = PostureUnprotected
			reasons[ReasonNoJob]++
		default:
			d.Policy = w.Job.Name
			d.Enabled = w.Job.Enabled
			rpo, hasRPO := w.Job.Schedule.RPO()
			if hasRPO {
				hours := rpo.Hours()
				d.RPOHours = &hours
			}
			d.Status = PostureProtected
			// Each rule is evaluated against this workload alone. The order only
			// decides which reason is reported first; any one of them is enough.
			if w.RestorePoints == 0 {
				d.Status = PostureAtRisk
				reasons[ReasonNoRestorePoints]++
			}
			if w.LastOutcomeFailed {
				d.Status = PostureAtRisk
				reasons[ReasonLastRunFailed]++
			}
			if hasRPO {
				deadline := rpo + store.RPOGrace(rpo)
				within := w.LastSuccessAt != nil && now.Sub(*w.LastSuccessAt) <= deadline
				d.WithinRPO = &within
				if !within && w.RestorePoints > 0 {
					// Zero restore points is already reported above; reporting it
					// twice would double-count the same workload.
					d.Status = PostureAtRisk
					reasons[ReasonRPOExceeded]++
				}
			}
		}
		switch d.Status {
		case PostureProtected:
			out.Counts.Protected++
		case PostureAtRisk:
			out.Counts.AtRisk++
		default:
			out.Counts.Unprotected++
		}
		out.Workloads = append(out.Workloads, d)
	}

	switch {
	case out.Counts.Unprotected > 0:
		out.Verdict = PostureUnprotected
	case out.Counts.AtRisk > 0:
		out.Verdict = PostureAtRisk
	default:
		out.Verdict = PostureProtected
	}
	out.Reasons = postureReasons(reasons)
	return out
}

// postureReasons renders the aggregated reasons in a stable order, worst first.
func postureReasons(counts map[string]int) []postureReasonDTO {
	details := map[string]string{
		ReasonNoJob:           "in no enabled backup job",
		ReasonNoRestorePoints: "in a job but with no restore point yet",
		ReasonLastRunFailed:   "their most recent backup attempt failed",
		ReasonRPOExceeded:     "their newest restore point is older than the job's schedule allows",
	}
	order := map[string]int{
		ReasonNoJob: 0, ReasonNoRestorePoints: 1, ReasonLastRunFailed: 2, ReasonRPOExceeded: 3,
	}
	out := make([]postureReasonDTO, 0, len(counts))
	for code, n := range counts {
		if n == 0 {
			continue
		}
		out = append(out, postureReasonDTO{Code: code, Workloads: n, Detail: details[code]})
	}
	sort.Slice(out, func(i, j int) bool { return order[out[i].Code] < order[out[j].Code] })
	return out
}

// governingJob picks the enabled job that protects a workload. A workload can
// legitimately be in several; the most protective one — the tightest RPO —
// decides, because that is the promise the operator would judge it against.
// Jobs without an RPO (manual, advanced) lose to any that has one.
func governingJob(jobs []*store.Job, member func(*store.Job) bool) *store.Job {
	var best *store.Job
	var bestRPO time.Duration
	var bestHasRPO bool
	for _, j := range jobs {
		if !j.Enabled || !member(j) {
			continue
		}
		rpo, hasRPO := j.Schedule.RPO()
		if best == nil || moreProtective(rpo, hasRPO, j.Name, bestRPO, bestHasRPO, best.Name) {
			best, bestRPO, bestHasRPO = j, rpo, hasRPO
		}
	}
	return best
}

// moreProtective reports whether one candidate job promises more than another:
// any objective beats none, a tighter objective beats a looser one, and the
// name breaks ties so the answer never depends on row order.
func moreProtective(rpo time.Duration, hasRPO bool, name string,
	bestRPO time.Duration, bestHasRPO bool, bestName string) bool {
	switch {
	case hasRPO != bestHasRPO:
		return hasRPO
	case hasRPO && rpo != bestRPO:
		return rpo < bestRPO
	default:
		return name < bestName
	}
}

// jobCoversVM reports whether a vm job's membership includes a guest, honouring
// both static sources and dynamic tag-filter membership.
func jobCoversVM(j *store.Job, vm store.VM) bool {
	if j.Kind != store.SourceVM {
		return false
	}
	if j.TagFilter != "" {
		return vm.HasTag(j.TagFilter)
	}
	for _, src := range j.Sources {
		if src.HostID == vm.HostID && src.VMID == vm.VMID {
			return true
		}
	}
	return false
}

// jobCoversAgent reports whether an agent job backs up a given agent.
func jobCoversAgent(j *store.Job, agentID string) bool {
	if j.Kind != store.SourceAgent {
		return false
	}
	for _, src := range j.Sources {
		if src.AgentID == agentID {
			return true
		}
	}
	return false
}

func (s *Server) handlePosture(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	jobs, err := s.st.ListJobs(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	vms, err := s.st.ListCachedVMs(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	agents, err := s.st.ListAgents(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	backups, err := s.st.ListBackups(ctx, store.BackupFilter{})
	if err != nil {
		s.serverError(w, err)
		return
	}
	outcomes, err := s.st.LatestSourceOutcomes(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}

	// Restore points are folded per workload: how many there are, when the
	// newest one was taken (the workload's own last success) and when its data
	// was last verified.
	type pointStats struct {
		count      int
		newest     *time.Time
		verifiedAt *time.Time
	}
	points := map[string]*pointStats{}
	for _, b := range backups {
		key := b.SourceKind + "/" + b.SourceID
		st, ok := points[key]
		if !ok {
			st = &pointStats{}
			points[key] = st
		}
		st.count++
		created := b.CreatedAt
		if st.newest == nil || created.After(*st.newest) {
			st.newest = &created
		}
		if b.LastVerifiedAt != nil && (st.verifiedAt == nil || b.LastVerifiedAt.After(*st.verifiedAt)) {
			verified := *b.LastVerifiedAt
			st.verifiedAt = &verified
		}
	}

	workloads := make([]postureWorkload, 0, len(vms)+len(agents))
	add := func(kind, id, name string, vmid int, hostName, node string, job *store.Job) {
		wl := postureWorkload{
			Kind: kind, ID: id, Name: name, VMID: vmid,
			HostName: hostName, Node: node, Job: job,
		}
		if st, ok := points[kind+"/"+id]; ok {
			wl.RestorePoints = st.count
			wl.LastSuccessAt = st.newest
			wl.LastVerifiedAt = st.verifiedAt
		}
		if outcome, ok := outcomes[id]; ok {
			if outcome.Status == store.SourceFailed {
				wl.LastOutcomeFailed = true
				failed := outcome.FinishedAt
				wl.LastFailureAt = &failed
			}
		}
		workloads = append(workloads, wl)
	}
	for _, vm := range vms {
		guest := vm
		add(store.SourceVM, sched.VMSourceID(vm.HostID, vm.VMID), vm.Name, vm.VMID, vm.HostName, vm.Node,
			governingJob(jobs, func(j *store.Job) bool { return jobCoversVM(j, guest) }))
	}
	for _, a := range agents {
		agentID := a.ID
		add(store.SourceAgent, a.ID, a.Hostname, 0, "", "",
			governingJob(jobs, func(j *store.Job) bool { return jobCoversAgent(j, agentID) }))
	}

	writeJSON(w, http.StatusOK, evaluatePosture(workloads, store.Now()))
}
