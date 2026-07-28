package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"proxback/internal/sched"
	"proxback/internal/store"
)

type jobDTO struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	TargetID   string `json:"targetId"`
	TargetName string `json:"targetName"`
	// Schedule is always the structured object, whatever form it was sent in.
	Schedule store.Schedule `json:"schedule"`
	// ScheduleLabel is the rendered English summary the UI shows verbatim.
	ScheduleLabel string `json:"scheduleLabel"`
	// Retention is always the GFS object, whatever form it was sent in.
	Retention store.RetentionPolicy `json:"retention"`
	// Policy is always the complete protection policy, so a client never has to
	// know which release wrote the job to know what it does.
	Policy  store.JobPolicy  `json:"policy"`
	Enabled bool             `json:"enabled"`
	Sources store.JobSources `json:"sources"`
	// TagFilter is null when the job uses its static source list.
	TagFilter *string `json:"tagFilter"`
	// NextRun is null for manual schedules and disabled jobs.
	NextRun *time.Time    `json:"nextRun"`
	LastRun *store.JobRun `json:"lastRun"`
}

func (s *Server) jobDTOs(r *http.Request, jobs []*store.Job) ([]jobDTO, error) {
	targets, err := s.st.ListS3Targets(r.Context())
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, t := range targets {
		names[t.ID] = t.Name
	}
	now := store.Now()
	out := make([]jobDTO, 0, len(jobs))
	for _, j := range jobs {
		d := jobDTO{
			ID: j.ID, Name: j.Name, Kind: j.Kind, TargetID: j.TargetID,
			TargetName: names[j.TargetID], Schedule: j.Schedule.Normalized(),
			ScheduleLabel: j.Schedule.Label(),
			Retention:     j.Retention, Policy: j.Policy.Normalized(),
			Enabled: j.Enabled, Sources: j.Sources,
			NextRun: sched.NextRun(j.Schedule, j.Enabled, now),
		}
		if j.TagFilter != "" {
			tag := j.TagFilter
			d.TagFilter = &tag
		}
		if d.Sources == nil {
			d.Sources = store.JobSources{}
		}
		last, err := s.st.LastRunForJob(r.Context(), j.ID)
		switch {
		case err == nil:
			d.LastRun = last
		case errors.Is(err, store.ErrNotFound):
		default:
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.st.ListJobs(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	out, err := s.jobDTOs(r, jobs)
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type jobRequest struct {
	Name     *string `json:"name"`
	Kind     *string `json:"kind"`
	TargetID *string `json:"targetId"`
	// Schedule accepts the structured object and, for existing automation, the
	// bare string earlier releases took ("manual" or a cron expression).
	Schedule *store.Schedule `json:"schedule"`
	// Retention accepts the GFS object and, for existing jobs and older
	// clients, the bare integer that means keep-last-N.
	Retention *store.RetentionPolicy `json:"retention"`
	// Policy is the optional protection policy; omitting it leaves the job's
	// current one alone, and a job created without one gets the defaults.
	Policy    *store.JobPolicy  `json:"policy"`
	Enabled   *bool             `json:"enabled"`
	Sources   *store.JobSources `json:"sources"`
	TagFilter *string           `json:"tagFilter"`
}

// validateJobPolicies checks the retention and protection policies of a job
// about to be written. Every message names the field it is about, because the
// console shows it next to the input that produced it.
func validateJobPolicies(job *store.Job) error {
	if err := job.Retention.Validate(); err != nil {
		return err
	}
	return job.Policy.Validate(job.Kind)
}

// normalizeTagFilter matches the normalisation applied to guest tags so a
// filter typed as "Prod" still selects guests tagged "prod".
func normalizeTagFilter(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

// validateJob checks a job's membership definition. A vm job may leave sources
// empty when it carries a tag filter, because membership is then resolved from
// the cached inventory at run start.
func validateJob(kind string, sources store.JobSources, tagFilter string) error {
	if tagFilter != "" && kind != store.SourceVM {
		return errors.New("tagFilter is only supported for vm jobs")
	}
	if len(sources) == 0 && tagFilter == "" {
		return errors.New("at least one source is required")
	}
	switch kind {
	case store.SourceVM:
		for _, src := range sources {
			if src.HostID == "" || src.VMID == 0 {
				return errors.New("vm job sources need hostId and vmid")
			}
		}
	case store.SourceAgent:
		for _, src := range sources {
			if src.AgentID == "" {
				return errors.New("agent job sources need agentId")
			}
			if len(src.Paths) == 0 {
				return errors.New("agent job sources need at least one path")
			}
			for _, p := range src.Paths {
				if strings.TrimSpace(p) == "" {
					return errors.New("agent job include paths must not be empty")
				}
			}
		}
	default:
		return errors.New(`kind must be "vm" or "agent"`)
	}
	return nil
}

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var body jobRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	job := &store.Job{
		Schedule:  store.ManualSchedule(),
		Retention: store.DefaultRetention(),
		Policy:    store.DefaultPolicy(),
		Enabled:   true,
	}
	if body.Name != nil {
		job.Name = strings.TrimSpace(*body.Name)
	}
	if body.Kind != nil {
		job.Kind = *body.Kind
	}
	if body.TargetID != nil {
		job.TargetID = *body.TargetID
	}
	if body.Schedule != nil {
		job.Schedule = *body.Schedule
	}
	if body.Retention != nil {
		job.Retention = *body.Retention
	}
	if body.Policy != nil {
		job.Policy = body.Policy.Normalized()
	}
	if body.Enabled != nil {
		job.Enabled = *body.Enabled
	}
	if body.Sources != nil {
		job.Sources = *body.Sources
	}
	if body.TagFilter != nil {
		job.TagFilter = normalizeTagFilter(*body.TagFilter)
	}
	if job.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if job.TargetID == "" {
		writeError(w, http.StatusBadRequest, "targetId is required")
		return
	}
	if _, err := s.st.S3TargetByID(r.Context(), job.TargetID); err != nil {
		s.notFoundOr(w, err, "target")
		return
	}
	if err := validateJob(job.Kind, job.Sources, job.TagFilter); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := job.Schedule.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateJobPolicies(job); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := s.st.CreateJob(r.Context(), job)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if err := s.sched.ReloadSchedules(r.Context()); err != nil {
		s.log.Warn("could not reload schedules", "error", err)
	}
	s.audit(r, store.AuditEntry{
		Action: store.AuditJobCreate, ObjectKind: "job",
		ObjectID: created.ID, ObjectName: created.Name,
		Detail: "kind " + created.Kind + ", " + created.Schedule.Label(),
	})
	out, err := s.jobDTOs(r, []*store.Job{created})
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out[0])
}

func (s *Server) handlePatchJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.st.JobByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.notFoundOr(w, err, "job")
		return
	}
	var body jobRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Name != nil {
		job.Name = strings.TrimSpace(*body.Name)
	}
	if body.Kind != nil {
		job.Kind = *body.Kind
	}
	if body.TargetID != nil {
		job.TargetID = *body.TargetID
	}
	if body.Schedule != nil {
		job.Schedule = body.Schedule.Normalized()
	}
	if body.Retention != nil {
		job.Retention = *body.Retention
	}
	if body.Policy != nil {
		job.Policy = body.Policy.Normalized()
	}
	if body.Enabled != nil {
		job.Enabled = *body.Enabled
	}
	if body.Sources != nil {
		job.Sources = *body.Sources
	}
	// An explicit empty tagFilter clears it and returns the job to its static
	// source list.
	if body.TagFilter != nil {
		job.TagFilter = normalizeTagFilter(*body.TagFilter)
	}
	if job.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if _, err := s.st.S3TargetByID(r.Context(), job.TargetID); err != nil {
		s.notFoundOr(w, err, "target")
		return
	}
	if err := validateJob(job.Kind, job.Sources, job.TagFilter); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := job.Schedule.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateJobPolicies(job); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.st.UpdateJob(r.Context(), job); err != nil {
		s.notFoundOr(w, err, "job")
		return
	}
	if err := s.sched.ReloadSchedules(r.Context()); err != nil {
		s.log.Warn("could not reload schedules", "error", err)
	}
	s.audit(r, store.AuditEntry{
		Action: store.AuditJobModify, ObjectKind: "job",
		ObjectID: job.ID, ObjectName: job.Name,
		Detail: "kind " + job.Kind + ", " + job.Schedule.Label(),
	})
	out, err := s.jobDTOs(r, []*store.Job{job})
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out[0])
}

func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	// The job is read first so the trail can name what was deleted; after the
	// delete there is nothing left to ask.
	name := ""
	if job, err := s.st.JobByID(r.Context(), id); err == nil {
		name = job.Name
	}
	if err := s.st.DeleteJob(r.Context(), id); err != nil {
		s.notFoundOr(w, err, "job")
		return
	}
	if err := s.sched.ReloadSchedules(r.Context()); err != nil {
		s.log.Warn("could not reload schedules", "error", err)
	}
	s.audit(r, store.AuditEntry{
		Action: store.AuditJobDelete, ObjectKind: "job", ObjectID: id, ObjectName: name,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// retentionPreviewEntry is one restore point's fate under a retention policy.
// Reasons names every rule that kept it and is empty for a pruned point.
type retentionPreviewEntry struct {
	BackupID  string    `json:"backupId"`
	CreatedAt time.Time `json:"createdAt"`
	Reasons   []string  `json:"reasons"`
}

type retentionPreviewDTO struct {
	Keeps  []retentionPreviewEntry `json:"keeps"`
	Prunes []retentionPreviewEntry `json:"prunes"`
}

// retentionFromQuery reads a candidate policy off the query string, falling
// back to the job's saved one field by field. The console sends the policy
// currently in its form so an operator can see what an edit would do *before*
// saving it; a request with no parameters previews what is stored.
func retentionFromQuery(q url.Values, saved store.RetentionPolicy) (store.RetentionPolicy, error) {
	out := saved
	for _, f := range []struct {
		name  string
		field *int
	}{
		{"keepLast", &out.KeepLast},
		{"keepDaily", &out.KeepDaily},
		{"keepWeekly", &out.KeepWeekly},
		{"keepMonthly", &out.KeepMonthly},
		{"keepYearly", &out.KeepYearly},
	} {
		raw := strings.TrimSpace(q.Get(f.name))
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			return out, fmt.Errorf("%s must be a whole number, got %q", f.name, raw)
		}
		*f.field = n
	}
	return out, nil
}

// handleRetentionPreview answers what a retention policy would keep and what it
// would prune, evaluated against the restore points the job actually holds. It
// is a pure question: nothing is deleted, nothing is saved, and the answer
// comes from the same store.EvaluateRetention the pruning pass runs on.
func (s *Server) handleRetentionPreview(w http.ResponseWriter, r *http.Request) {
	job, err := s.st.JobByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.notFoundOr(w, err, "job")
		return
	}
	policy, err := retentionFromQuery(r.URL.Query(), job.Retention)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := policy.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// The workloads this job protects are the ones it has actually produced
	// restore points for; a job that has never run has nothing to preview.
	mine, err := s.st.ListBackups(r.Context(), store.BackupFilter{JobID: job.ID})
	if err != nil {
		s.serverError(w, err)
		return
	}
	type workload struct{ kind, id string }
	var order []workload
	seen := map[workload]struct{}{}
	for _, b := range mine {
		key := workload{kind: b.SourceKind, id: b.SourceID}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		order = append(order, key)
	}

	out := retentionPreviewDTO{
		Keeps:  []retentionPreviewEntry{},
		Prunes: []retentionPreviewEntry{},
	}
	// Retention acts per workload on one target — one source's history never
	// decides another source's fate, and the pruning pass sees every point of
	// that source on that target, whichever job wrote it. The preview asks
	// exactly the same question of exactly the same set, so what an operator is
	// shown before saving is what will happen.
	for _, source := range order {
		list, err := s.st.ListBackups(r.Context(), store.BackupFilter{
			SourceKind: source.kind, SourceID: source.id, TargetID: job.TargetID,
		})
		if err != nil {
			s.serverError(w, err)
			return
		}
		pts := make([]store.RetentionPoint, 0, len(list))
		for _, b := range list {
			pts = append(pts, store.RetentionPoint{ID: b.ID, CreatedAt: b.CreatedAt})
		}
		plan := store.EvaluateRetention(pts, policy)
		out.Keeps = append(out.Keeps, previewEntries(plan.Keeps)...)
		out.Prunes = append(out.Prunes, previewEntries(plan.Prunes)...)
	}
	sortPreview(out.Keeps)
	sortPreview(out.Prunes)
	writeJSON(w, http.StatusOK, out)
}

func previewEntries(decisions []store.RetentionDecision) []retentionPreviewEntry {
	out := make([]retentionPreviewEntry, 0, len(decisions))
	for _, d := range decisions {
		reasons := d.Reasons
		if reasons == nil {
			reasons = []string{}
		}
		out = append(out, retentionPreviewEntry{
			BackupID: d.ID, CreatedAt: d.CreatedAt, Reasons: reasons,
		})
	}
	return out
}

// sortPreview orders a preview list newest first, which is how the console
// lists restore points everywhere else.
func sortPreview(entries []retentionPreviewEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if !entries[i].CreatedAt.Equal(entries[j].CreatedAt) {
			return entries[i].CreatedAt.After(entries[j].CreatedAt)
		}
		return entries[i].BackupID > entries[j].BackupID
	})
}

func (s *Server) handleRunJob(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "id")
	runID, err := s.sched.TriggerJob(r.Context(), jobID)
	if err != nil {
		switch {
		case errors.Is(err, sched.ErrAlreadyRunning):
			writeError(w, http.StatusConflict, "job is already running")
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "job not found")
		default:
			s.serverError(w, err)
		}
		return
	}
	// What is recorded is the request: who started which job, and the run it
	// produced. How the run ends is the run's own history — the trail does not
	// reach into the scheduler to wait for it.
	s.audit(r, store.AuditEntry{
		Action: store.AuditRunStart, ObjectKind: "job",
		ObjectID: jobID, ObjectName: s.jobName(r, jobID),
		Detail: "started run " + runID + " manually",
	})
	writeJSON(w, http.StatusOK, map[string]string{"runId": runID})
}

// jobName is the job's display name for a trail entry, empty when it cannot be
// read. An audit lookup must never be the reason a request fails.
func (s *Server) jobName(r *http.Request, jobID string) string {
	if jobID == "" {
		return ""
	}
	job, err := s.st.JobByID(r.Context(), jobID)
	if err != nil {
		return ""
	}
	return job.Name
}

// ---------------------------------------------------------------- runs

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	jobID := r.URL.Query().Get("jobId")
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	runs, err := s.st.ListRuns(r.Context(), jobID, limit)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if runs == nil {
		runs = []*store.JobRun{}
	}
	writeJSON(w, http.StatusOK, runs)
}

// runDetailDTO is one run plus the per-object breakdown that drives the visual
// monitor. The list endpoint deliberately does not carry it: a run of 8 VMs
// costs 8 extra rows, and the list is polled every two seconds.
type runDetailDTO struct {
	*store.JobRun
	// Sources is always an array, empty for restores and verifications.
	Sources []store.RunSource `json:"sources"`
	// ThroughputBps is the run's current speed over the last sample window, 0
	// when the run is not in flight.
	ThroughputBps float64 `json:"throughputBps"`
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	run, err := s.st.RunByID(r.Context(), id)
	if err != nil {
		s.notFoundOr(w, err, "run")
		return
	}
	sources, err := s.st.RunSources(r.Context(), id)
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runDetailDTO{
		JobRun:        run,
		Sources:       sources,
		ThroughputBps: s.sched.ThroughputBps(id),
	})
}

// handleRetryRun re-runs the job a run belongs to. It goes through the normal
// trigger path, so the retry uses the job as it stands now and obeys the same
// "one run per job" rule as a manual start.
func (s *Server) handleRetryRun(w http.ResponseWriter, r *http.Request) {
	sourceRun := chi.URLParam(r, "id")
	runID, err := s.sched.RetryRun(r.Context(), sourceRun)
	if err != nil {
		switch {
		case errors.Is(err, sched.ErrAlreadyRunning):
			writeError(w, http.StatusConflict, "job is already running")
		case errors.Is(err, sched.ErrNoJob):
			writeError(w, http.StatusNotFound, "this run has no job to re-run")
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "run not found")
		default:
			s.serverError(w, err)
		}
		return
	}
	name := ""
	if run, rerr := s.st.RunByID(r.Context(), runID); rerr == nil {
		name = run.JobName
	}
	s.audit(r, store.AuditEntry{
		Action: store.AuditRunRetry, ObjectKind: "run",
		ObjectID: runID, ObjectName: name,
		Detail: "re-ran run " + sourceRun,
	})
	writeJSON(w, http.StatusOK, map[string]string{"runId": runID})
}

// handleRunLog serves a run's persisted activity log. The lines array is always
// present, even for a run that produced none.
func (s *Server) handleRunLog(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := s.st.RunByID(r.Context(), id); err != nil {
		s.notFoundOr(w, err, "run")
		return
	}
	lines, err := s.st.RunLog(r.Context(), id)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if lines == nil {
		lines = []store.RunLogLine{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

// handleDeleteRun removes one run from the history. Only the run row and its
// activity log go: restore points and chunk data are never touched.
func (s *Server) handleDeleteRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	run, err := s.st.RunByID(r.Context(), id)
	if err != nil {
		s.notFoundOr(w, err, "run")
		return
	}
	if run.Status == store.RunRunning {
		writeError(w, http.StatusConflict, "cannot delete a running run — cancel it first")
		return
	}
	if err := s.st.DeleteJobRun(r.Context(), id); err != nil {
		s.notFoundOr(w, err, "run")
		return
	}
	s.audit(r, store.AuditEntry{
		Action: store.AuditRunDelete, ObjectKind: "run", ObjectID: id, ObjectName: run.JobName,
		Detail: "removed from run history",
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// Scopes accepted by POST /api/runs/clear.
const (
	clearScopeFinished = "finished"
	clearScopeFailed   = "failed"
)

type clearRunsRequest struct {
	Scope string `json:"scope"`
	JobID string `json:"jobId"`
}

func (s *Server) handleClearRuns(w http.ResponseWriter, r *http.Request) {
	var body clearRunsRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	var statuses []string
	switch body.Scope {
	case clearScopeFinished:
		statuses = []string{store.RunSuccess, store.RunFailed, store.RunCanceled}
	case clearScopeFailed:
		statuses = []string{store.RunFailed}
	default:
		writeError(w, http.StatusBadRequest, `scope must be "finished" or "failed"`)
		return
	}
	n, err := s.st.DeleteJobRunsByStatus(r.Context(), statuses, body.JobID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, store.AuditEntry{
		Action: store.AuditRunDelete, ObjectKind: "run", ObjectID: body.JobID,
		ObjectName: s.jobName(r, body.JobID),
		Detail:     fmt.Sprintf("cleared %d %s runs from history", n, body.Scope),
	})
	writeJSON(w, http.StatusOK, map[string]int{"deleted": n})
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	run, err := s.st.RunByID(r.Context(), id)
	if err != nil {
		s.notFoundOr(w, err, "run")
		return
	}
	if err := s.sched.Cancel(id); err != nil {
		writeError(w, http.StatusConflict, "run is not in progress")
		return
	}
	s.audit(r, store.AuditEntry{
		Action: store.AuditRunCancel, ObjectKind: "run", ObjectID: id, ObjectName: run.JobName,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------- backups

func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	backups, err := s.st.ListBackups(r.Context(), store.BackupFilter{
		SourceKind: q.Get("sourceKind"),
		SourceID:   q.Get("sourceId"),
		TargetID:   q.Get("targetId"),
		JobID:      q.Get("jobId"),
	})
	if err != nil {
		s.serverError(w, err)
		return
	}
	if backups == nil {
		backups = []*store.Backup{}
	}
	writeJSON(w, http.StatusOK, backups)
}

func (s *Server) handleVerifyBackup(w http.ResponseWriter, r *http.Request) {
	backupID := chi.URLParam(r, "id")
	runID, err := s.sched.Verify(r.Context(), backupID)
	if err != nil {
		switch {
		case errors.Is(err, sched.ErrAlreadyRunning):
			writeError(w, http.StatusConflict, "a verification of this restore point is already running")
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "backup not found")
		default:
			s.serverError(w, err)
		}
		return
	}
	s.audit(r, store.AuditEntry{
		Action: store.AuditVerifyStart, ObjectKind: "backup",
		ObjectID: backupID, ObjectName: s.backupName(r, backupID),
		Detail: "started verification run " + runID,
	})
	writeJSON(w, http.StatusOK, map[string]string{"runId": runID})
}

// backupName is the workload a restore point belongs to, for a trail entry.
func (s *Server) backupName(r *http.Request, backupID string) string {
	b, err := s.st.BackupByID(r.Context(), backupID)
	if err != nil {
		return ""
	}
	return b.SourceName
}

func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	// Read the restore point before it goes, so the trail can say what was
	// destroyed rather than only which id was requested.
	name, detail := "", ""
	if b, err := s.st.BackupByID(r.Context(), id); err == nil {
		name = b.SourceName
		detail = fmt.Sprintf("%s restore point of %s, created %s",
			b.Kind, b.SourceName, b.CreatedAt.Format(time.RFC3339))
	}
	if err := s.sched.DeleteBackup(r.Context(), id); err != nil {
		s.notFoundOr(w, err, "backup")
		return
	}
	s.audit(r, store.AuditEntry{
		Action: store.AuditBackupDelete, ObjectKind: "backup",
		ObjectID: id, ObjectName: name, Detail: detail,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleCreateRestore starts a restore. The destination is checked before the
// run exists: "alongside" (the default, and what a request that says nothing
// gets) refuses a VMID that is already in use, and "overwrite" refuses to run
// without the destination guest's current name typed back. Both answer 409 —
// the request was well formed, the estate says no.
func (s *Server) handleCreateRestore(w http.ResponseWriter, r *http.Request) {
	var spec sched.RestoreSpec
	if !decodeJSON(w, r, &spec) {
		return
	}
	if spec.BackupID == "" {
		writeError(w, http.StatusBadRequest, "backupId is required")
		return
	}
	runID, err := s.sched.TriggerRestore(r.Context(), spec)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "backup not found")
		case errors.Is(err, sched.ErrVMIDInUse), errors.Is(err, sched.ErrConfirmName):
			writeError(w, http.StatusConflict, err.Error())
		default:
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	// A restore is the one operation that writes into the estate, so the trail
	// records both halves of the decision: the mode and the destination.
	s.audit(r, store.AuditEntry{
		Action: store.AuditRestoreStart, ObjectKind: "backup",
		ObjectID: spec.BackupID, ObjectName: s.backupName(r, spec.BackupID),
		Detail: restoreDetail(spec) + ", run " + runID,
	})
	writeJSON(w, http.StatusOK, map[string]string{"runId": runID})
}

// restoreDetail describes a restore in one line: the mode it runs in and where
// the data lands. Nothing here is a secret — a host id, a node, a VMID, a
// storage name or a destination path.
func restoreDetail(spec sched.RestoreSpec) string {
	mode := spec.Mode
	if mode == "" {
		mode = "alongside"
	}
	out := "mode " + mode
	switch {
	case spec.VM != nil:
		out += fmt.Sprintf(", to host %s node %s vmid %d", spec.VM.HostID, spec.VM.Node, spec.VM.VMID)
		if spec.VM.Storage != "" {
			out += " storage " + spec.VM.Storage
		}
	case spec.Agent != nil:
		out += fmt.Sprintf(", to agent %s path %s", spec.Agent.AgentID, spec.Agent.DestPath)
	default:
		out += ", to the original location"
	}
	return out
}
