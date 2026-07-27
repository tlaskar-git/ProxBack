package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const jobCols = `id, name, kind, target_id, schedule, retention, enabled, sources, tag_filter, created_at`

func scanJob(sc interface{ Scan(...any) error }) (*Job, error) {
	var j Job
	var enabled int
	var sources, created string
	if err := sc.Scan(&j.ID, &j.Name, &j.Kind, &j.TargetID, &j.Schedule, &j.Retention, &enabled,
		&sources, &j.TagFilter, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan job: %w", err)
	}
	j.Enabled = enabled != 0
	j.CreatedAt = parseTime(created)
	if err := json.Unmarshal([]byte(sources), &j.Sources); err != nil {
		return nil, fmt.Errorf("job %s sources: %w", j.ID, err)
	}
	if j.Sources == nil {
		j.Sources = JobSources{}
	}
	return &j, nil
}

// CreateJob inserts a job definition.
func (s *Store) CreateJob(ctx context.Context, j *Job) (*Job, error) {
	if j.ID == "" {
		j.ID = NewID()
	}
	if j.Schedule == "" {
		j.Schedule = "manual"
	}
	if j.Sources == nil {
		j.Sources = JobSources{}
	}
	j.CreatedAt = Now()
	raw, err := json.Marshal(j.Sources)
	if err != nil {
		return nil, fmt.Errorf("encode job sources: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO jobs (`+jobCols+`) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		j.ID, j.Name, j.Kind, j.TargetID, j.Schedule, j.Retention, boolInt(j.Enabled), string(raw),
		j.TagFilter, fmtTime(j.CreatedAt))
	if err != nil {
		return nil, fmt.Errorf("create job: %w", err)
	}
	return j, nil
}

// UpdateJob writes all mutable fields of a job.
func (s *Store) UpdateJob(ctx context.Context, j *Job) error {
	raw, err := json.Marshal(j.Sources)
	if err != nil {
		return fmt.Errorf("encode job sources: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET name=?, kind=?, target_id=?, schedule=?, retention=?, enabled=?, sources=?, tag_filter=? WHERE id=?`,
		j.Name, j.Kind, j.TargetID, j.Schedule, j.Retention, boolInt(j.Enabled), string(raw), j.TagFilter, j.ID)
	if err != nil {
		return fmt.Errorf("update job: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListJobs returns all jobs ordered by name.
func (s *Store) ListJobs(ctx context.Context) ([]*Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobCols+` FROM jobs ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// JobByID loads one job.
func (s *Store) JobByID(ctx context.Context, id string) (*Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = ?`, id))
}

// DeleteJob removes a job definition.
func (s *Store) DeleteJob(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete job: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountJobs returns the number of configured jobs.
func (s *Store) CountJobs(ctx context.Context) (int, error) { return s.count(ctx, `jobs`) }

// ---------------------------------------------------------------- job runs

const runCols = `id, job_id, job_name, kind, status, started_at, finished_at,
	bytes_processed, bytes_uploaded, dedup_ratio, error, progress_pct, current_step`

func scanRun(sc interface{ Scan(...any) error }) (*JobRun, error) {
	var r JobRun
	var started string
	var finished, errStr sql.NullString
	if err := sc.Scan(&r.ID, &r.JobID, &r.JobName, &r.Kind, &r.Status, &started, &finished,
		&r.BytesProcessed, &r.BytesUploaded, &r.DedupRatio, &errStr, &r.ProgressPct, &r.CurrentStep); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan job run: %w", err)
	}
	r.StartedAt = parseTime(started)
	r.FinishedAt = nullTime(finished)
	r.Error = nullStr(errStr)
	return &r, nil
}

// CreateRun inserts a running job run row.
func (s *Store) CreateRun(ctx context.Context, r *JobRun) (*JobRun, error) {
	if r.ID == "" {
		r.ID = NewID()
	}
	if r.Status == "" {
		r.Status = RunRunning
	}
	if r.Kind == "" {
		r.Kind = RunKindBackup
	}
	if r.StartedAt.IsZero() {
		r.StartedAt = Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO job_runs (`+runCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.JobID, r.JobName, r.Kind, r.Status, fmtTime(r.StartedAt), fmtTimePtr(r.FinishedAt),
		r.BytesProcessed, r.BytesUploaded, r.DedupRatio, strOrNull(r.Error), r.ProgressPct, r.CurrentStep)
	if err != nil {
		return nil, fmt.Errorf("create job run: %w", err)
	}
	return r, nil
}

// UpdateRunProgress persists an engine progress callback.
func (s *Store) UpdateRunProgress(ctx context.Context, runID string, processed, uploaded int64, pct float64, step string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE job_runs SET bytes_processed=?, bytes_uploaded=?, progress_pct=?, current_step=? WHERE id=?`,
		processed, uploaded, pct, step, runID)
	if err != nil {
		return fmt.Errorf("update run progress: %w", err)
	}
	return nil
}

// FinishRun records a terminal run state.
func (s *Store) FinishRun(ctx context.Context, runID, status string, processed, uploaded int64, dedupRatio float64, runErr string) error {
	now := Now()
	pct := 100.0
	if status != RunSuccess {
		var cur float64
		_ = s.db.QueryRowContext(ctx, `SELECT progress_pct FROM job_runs WHERE id = ?`, runID).Scan(&cur)
		pct = cur
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE job_runs SET status=?, finished_at=?, bytes_processed=?, bytes_uploaded=?, dedup_ratio=?, error=?, progress_pct=? WHERE id=?`,
		status, fmtTime(now), processed, uploaded, dedupRatio, strOrNull(runErr), pct, runID)
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	return nil
}

// SetRunStep updates just the human readable current step.
func (s *Store) SetRunStep(ctx context.Context, runID, step string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE job_runs SET current_step=? WHERE id=?`, step, runID); err != nil {
		return fmt.Errorf("set run step: %w", err)
	}
	return nil
}

// RunByID loads one run.
func (s *Store) RunByID(ctx context.Context, id string) (*JobRun, error) {
	return scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM job_runs WHERE id = ?`, id))
}

// ListRuns returns runs, optionally filtered by job, newest first.
func (s *Store) ListRuns(ctx context.Context, jobID string, limit int) ([]*JobRun, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT ` + runCols + ` FROM job_runs`
	args := []any{}
	if jobID != "" {
		q += ` WHERE job_id = ?`
		args = append(args, jobID)
	}
	q += ` ORDER BY started_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()
	var out []*JobRun
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastRunForJob returns the most recent run of a job, or ErrNotFound.
func (s *Store) LastRunForJob(ctx context.Context, jobID string) (*JobRun, error) {
	return scanRun(s.db.QueryRowContext(ctx,
		`SELECT `+runCols+` FROM job_runs WHERE job_id = ? ORDER BY started_at DESC, id DESC LIMIT 1`, jobID))
}

// HasRunningRun reports whether a job currently has a run in progress.
func (s *Store) HasRunningRun(ctx context.Context, jobID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM job_runs WHERE job_id = ? AND status = ?`, jobID, RunRunning).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check running run: %w", err)
	}
	return n > 0, nil
}

// RunCountsSince returns per-status run counts for runs started at or after since.
func (s *Store) RunCountsSince(ctx context.Context, since time.Time) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM job_runs WHERE started_at >= ? GROUP BY status`, fmtTime(since))
	if err != nil {
		return nil, fmt.Errorf("run counts: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("scan run counts: %w", err)
		}
		out[status] = n
	}
	return out, rows.Err()
}

// DeleteJobRun removes one run from the history together with its activity log.
// Restore points and chunk data are untouched: a run row is history, not data.
func (s *Store) DeleteJobRun(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM job_runs WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete job run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return s.DeleteRunLog(ctx, id)
}

// DeleteJobRunsByStatus bulk-removes terminal runs (and their activity logs) in
// the given statuses, optionally restricted to one job. An empty jobID clears
// runs of every job. Runs still in progress are never deleted, whatever the
// caller asks for. It returns the number of runs removed.
func (s *Store) DeleteJobRunsByStatus(ctx context.Context, statuses []string, jobID string) (int, error) {
	wanted := make([]any, 0, len(statuses))
	for _, st := range statuses {
		if st == "" || st == RunRunning {
			continue
		}
		wanted = append(wanted, st)
	}
	if len(wanted) == 0 {
		return 0, nil
	}
	where := ` WHERE status IN (` + placeholders(len(wanted)) + `) AND status <> ?`
	args := append(append([]any{}, wanted...), RunRunning)
	if jobID != "" {
		where += ` AND job_id = ?`
		args = append(args, jobID)
	}
	// The log rows go first: once the runs are gone their ids are unrecoverable.
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM run_log WHERE run_id IN (SELECT id FROM job_runs`+where+`)`, args...); err != nil {
		return 0, fmt.Errorf("delete run logs by status: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM job_runs`+where, args...)
	if err != nil {
		return 0, fmt.Errorf("delete job runs by status: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete job runs by status: %w", err)
	}
	return int(n), nil
}

// placeholders renders n comma separated SQL bind markers.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// MarkOrphanRunsFailed flags runs left running by an unclean shutdown.
func (s *Store) MarkOrphanRunsFailed(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE job_runs SET status=?, finished_at=?, error=? WHERE status=?`,
		RunFailed, fmtTime(Now()), "interrupted by server restart", RunRunning)
	if err != nil {
		return fmt.Errorf("mark orphan runs: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- backups

const backupCols = `id, job_id, run_id, source_kind, source_id, source_name, target_id,
	created_at, size_bytes, uploaded_bytes, kind, parent_id, disks`

func scanBackup(sc interface{ Scan(...any) error }) (*Backup, error) {
	var b Backup
	var created, disks string
	var parent sql.NullString
	if err := sc.Scan(&b.ID, &b.JobID, &b.RunID, &b.SourceKind, &b.SourceID, &b.SourceName, &b.TargetID,
		&created, &b.SizeBytes, &b.UploadedBytes, &b.Kind, &parent, &disks); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan backup: %w", err)
	}
	b.CreatedAt = parseTime(created)
	b.ParentID = nullStr(parent)
	if err := json.Unmarshal([]byte(disks), &b.Disks); err != nil {
		return nil, fmt.Errorf("backup %s disks: %w", b.ID, err)
	}
	if b.Disks == nil {
		b.Disks = []Disk{}
	}
	return &b, nil
}

// CreateBackup inserts a restore point.
func (s *Store) CreateBackup(ctx context.Context, b *Backup) (*Backup, error) {
	if b.ID == "" {
		b.ID = NewID()
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = Now()
	}
	if b.Kind == "" {
		b.Kind = BackupFull
	}
	if b.Disks == nil {
		b.Disks = []Disk{}
	}
	raw, err := json.Marshal(b.Disks)
	if err != nil {
		return nil, fmt.Errorf("encode backup disks: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO backups (`+backupCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		b.ID, b.JobID, b.RunID, b.SourceKind, b.SourceID, b.SourceName, b.TargetID,
		fmtTime(b.CreatedAt), b.SizeBytes, b.UploadedBytes, b.Kind, strOrNull(b.ParentID), string(raw))
	if err != nil {
		return nil, fmt.Errorf("create backup: %w", err)
	}
	return b, nil
}

// BackupFilter narrows a backup listing.
type BackupFilter struct {
	SourceKind string
	SourceID   string
	TargetID   string
	JobID      string
}

// ListBackups returns restore points newest first.
func (s *Store) ListBackups(ctx context.Context, f BackupFilter) ([]*Backup, error) {
	q := `SELECT ` + backupCols + ` FROM backups WHERE 1=1`
	args := []any{}
	if f.SourceKind != "" {
		q += ` AND source_kind = ?`
		args = append(args, f.SourceKind)
	}
	if f.SourceID != "" {
		q += ` AND source_id = ?`
		args = append(args, f.SourceID)
	}
	if f.TargetID != "" {
		q += ` AND target_id = ?`
		args = append(args, f.TargetID)
	}
	if f.JobID != "" {
		q += ` AND job_id = ?`
		args = append(args, f.JobID)
	}
	q += ` ORDER BY created_at DESC, id DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}
	defer rows.Close()
	var out []*Backup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// BackupByID loads one restore point.
func (s *Store) BackupByID(ctx context.Context, id string) (*Backup, error) {
	return scanBackup(s.db.QueryRowContext(ctx, `SELECT `+backupCols+` FROM backups WHERE id = ?`, id))
}

// LatestBackupForSource returns the newest restore point for a source on a target.
func (s *Store) LatestBackupForSource(ctx context.Context, sourceKind, sourceID, targetID string) (*Backup, error) {
	return scanBackup(s.db.QueryRowContext(ctx,
		`SELECT `+backupCols+` FROM backups
		 WHERE source_kind = ? AND source_id = ? AND target_id = ?
		 ORDER BY created_at DESC, id DESC LIMIT 1`, sourceKind, sourceID, targetID))
}

// DeleteBackup removes a restore point row.
func (s *Store) DeleteBackup(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM backups WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete backup: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ClearBackupParent detaches children of a deleted restore point so the chain
// display in the UI does not dangle.
func (s *Store) ClearBackupParent(ctx context.Context, parentID string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE backups SET parent_id = NULL WHERE parent_id = ?`, parentID); err != nil {
		return fmt.Errorf("clear backup parent: %w", err)
	}
	return nil
}

// TotalLogicalBytes returns the sum of all restore point sizes.
func (s *Store) TotalLogicalBytes(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes),0) FROM backups`).Scan(&n); err != nil {
		return 0, fmt.Errorf("total logical bytes: %w", err)
	}
	return n, nil
}
