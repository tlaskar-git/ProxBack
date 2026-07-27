package api

import (
	"errors"
	"net/http"
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
	ScheduleLabel string           `json:"scheduleLabel"`
	Retention     int              `json:"retention"`
	Enabled       bool             `json:"enabled"`
	Sources       store.JobSources `json:"sources"`
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
			Retention:     j.Retention, Enabled: j.Enabled, Sources: j.Sources,
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
	Schedule  *store.Schedule   `json:"schedule"`
	Retention *int              `json:"retention"`
	Enabled   *bool             `json:"enabled"`
	Sources   *store.JobSources `json:"sources"`
	TagFilter *string           `json:"tagFilter"`
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
	job := &store.Job{Schedule: store.ManualSchedule(), Retention: 7, Enabled: true}
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
	created, err := s.st.CreateJob(r.Context(), job)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if err := s.sched.ReloadSchedules(r.Context()); err != nil {
		s.log.Warn("could not reload schedules", "error", err)
	}
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
	if err := s.st.UpdateJob(r.Context(), job); err != nil {
		s.notFoundOr(w, err, "job")
		return
	}
	if err := s.sched.ReloadSchedules(r.Context()); err != nil {
		s.log.Warn("could not reload schedules", "error", err)
	}
	out, err := s.jobDTOs(r, []*store.Job{job})
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out[0])
}

func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteJob(r.Context(), chi.URLParam(r, "id")); err != nil {
		s.notFoundOr(w, err, "job")
		return
	}
	if err := s.sched.ReloadSchedules(r.Context()); err != nil {
		s.log.Warn("could not reload schedules", "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleRunJob(w http.ResponseWriter, r *http.Request) {
	runID, err := s.sched.TriggerJob(r.Context(), chi.URLParam(r, "id"))
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
	writeJSON(w, http.StatusOK, map[string]string{"runId": runID})
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
	runID, err := s.sched.RetryRun(r.Context(), chi.URLParam(r, "id"))
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
	writeJSON(w, http.StatusOK, map[string]int{"deleted": n})
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := s.st.RunByID(r.Context(), id); err != nil {
		s.notFoundOr(w, err, "run")
		return
	}
	if err := s.sched.Cancel(id); err != nil {
		writeError(w, http.StatusConflict, "run is not in progress")
		return
	}
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
	runID, err := s.sched.Verify(r.Context(), chi.URLParam(r, "id"))
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
	writeJSON(w, http.StatusOK, map[string]string{"runId": runID})
}

func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	if err := s.sched.DeleteBackup(r.Context(), chi.URLParam(r, "id")); err != nil {
		s.notFoundOr(w, err, "backup")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

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
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "backup not found")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"runId": runID})
}
