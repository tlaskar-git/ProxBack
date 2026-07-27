package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Run source statuses. A source is inserted pending when the run is planned,
// flipped to running when the engine reaches it, and ends in one of the three
// terminal states.
const (
	SourcePending = "pending"
	SourceRunning = "running"
	SourceSuccess = "success"
	SourceFailed  = "failed"
	SourceSkipped = "skipped"
)

const runSourceCols = `seq, name, kind, node, status, size_bytes, bytes_processed,
	bytes_uploaded, started_at, finished_at, error`

// ReplaceRunSources writes a run's per-source breakdown, replacing whatever was
// there. It is called once at run start with every source the run intends to
// walk, so the monitor can show the whole plan — including the sizes that make
// up the run's total — before any bytes move.
func (s *Store) ReplaceRunSources(ctx context.Context, runID string, sources []RunSource) error {
	if runID == "" {
		return fmt.Errorf("replace run sources: empty run id")
	}
	if err := s.DeleteRunSources(ctx, runID); err != nil {
		return err
	}
	for i := range sources {
		src := sources[i]
		if src.Status == "" {
			src.Status = SourcePending
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO run_sources (run_id, `+runSourceCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			runID, src.Seq, src.Name, src.Kind, src.Node, src.Status, src.SizeBytes,
			src.BytesProcessed, src.BytesUploaded, fmtTimePtr(src.StartedAt), fmtTimePtr(src.FinishedAt),
			strOrNull(src.Error)); err != nil {
			return fmt.Errorf("insert run source %d: %w", src.Seq, err)
		}
	}
	return nil
}

// StartRunSource marks a source as the one currently being backed up.
func (s *Store) StartRunSource(ctx context.Context, runID string, seq int) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE run_sources SET status=?, started_at=? WHERE run_id=? AND seq=?`,
		SourceRunning, fmtTime(Now()), runID, seq); err != nil {
		return fmt.Errorf("start run source: %w", err)
	}
	return nil
}

// UpdateRunSourceProgress records the byte counts of the source in flight. It is
// called from the throttled progress path, never per chunk.
func (s *Store) UpdateRunSourceProgress(ctx context.Context, runID string, seq int, processed, uploaded int64) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE run_sources SET bytes_processed=?, bytes_uploaded=? WHERE run_id=? AND seq=?`,
		processed, uploaded, runID, seq); err != nil {
		return fmt.Errorf("update run source progress: %w", err)
	}
	return nil
}

// SetRunSourceSize records a source's total size once it becomes known. An
// agent stream has no size until the agent has produced it, so its row learns
// the figure late rather than never.
func (s *Store) SetRunSourceSize(ctx context.Context, runID string, seq int, size int64) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE run_sources SET size_bytes=? WHERE run_id=? AND seq=?`, size, runID, seq); err != nil {
		return fmt.Errorf("set run source size: %w", err)
	}
	return nil
}

// FinishRunSource records a source's terminal state and final byte counts.
func (s *Store) FinishRunSource(ctx context.Context, runID string, seq int, status string,
	processed, uploaded int64, srcErr string) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE run_sources SET status=?, bytes_processed=?, bytes_uploaded=?, finished_at=?, error=?
		 WHERE run_id=? AND seq=?`,
		status, processed, uploaded, fmtTime(Now()), strOrNull(srcErr), runID, seq); err != nil {
		return fmt.Errorf("finish run source: %w", err)
	}
	return nil
}

// SkipPendingRunSources marks every source a failed or cancelled run never got
// to as skipped, so the monitor never leaves rows spinning as pending after the
// run itself is over.
func (s *Store) SkipPendingRunSources(ctx context.Context, runID string) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE run_sources SET status=?, finished_at=? WHERE run_id=? AND status IN (?,?)`,
		SourceSkipped, fmtTime(Now()), runID, SourcePending, SourceRunning); err != nil {
		return fmt.Errorf("skip pending run sources: %w", err)
	}
	return nil
}

// SkipOrphanRunSources closes out the source rows of runs that an unclean
// shutdown left behind. It is the run_sources half of MarkOrphanRunsFailed and
// is called at startup, when nothing is in flight by definition, so every row
// still pending or running belongs to a run that is over.
func (s *Store) SkipOrphanRunSources(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE run_sources SET status=?, finished_at=? WHERE status IN (?,?)`,
		SourceSkipped, fmtTime(Now()), SourcePending, SourceRunning); err != nil {
		return fmt.Errorf("skip orphan run sources: %w", err)
	}
	return nil
}

// RunSources returns a run's per-source breakdown in run order. The result is
// never nil so it serialises as an empty JSON array.
func (s *Store) RunSources(ctx context.Context, runID string) ([]RunSource, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runSourceCols+` FROM run_sources WHERE run_id = ? ORDER BY seq`, runID)
	if err != nil {
		return nil, fmt.Errorf("list run sources: %w", err)
	}
	defer rows.Close()
	out := []RunSource{}
	for rows.Next() {
		var src RunSource
		var started, finished, srcErr sql.NullString
		if err := rows.Scan(&src.Seq, &src.Name, &src.Kind, &src.Node, &src.Status, &src.SizeBytes,
			&src.BytesProcessed, &src.BytesUploaded, &started, &finished, &srcErr); err != nil {
			return nil, fmt.Errorf("scan run source: %w", err)
		}
		src.StartedAt = nullTime(started)
		src.FinishedAt = nullTime(finished)
		src.Error = nullStr(srcErr)
		src.ProgressPct = src.progress()
		out = append(out, src)
	}
	return out, rows.Err()
}

// DeleteRunSources removes a run's per-source breakdown.
func (s *Store) DeleteRunSources(ctx context.Context, runID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM run_sources WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("delete run sources: %w", err)
	}
	return nil
}
