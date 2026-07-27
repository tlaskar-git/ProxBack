package store

import (
	"context"
	"fmt"
)

// RunLogCap is the number of activity lines kept per run. Older lines are
// dropped as new ones arrive, so a pathological run can never grow the database
// without bound.
const RunLogCap = 500

// AppendRunLog records one activity line for a run, stamping it with the current
// time, and enforces RunLogCap by dropping the oldest lines of that run.
func (s *Store) AppendRunLog(ctx context.Context, runID, line string) error {
	if runID == "" {
		return fmt.Errorf("append run log: empty run id")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO run_log (run_id, ts, line) VALUES (?,?,?)`,
		runID, fmtTime(Now()), line); err != nil {
		return fmt.Errorf("append run log: %w", err)
	}
	n, err := s.CountRunLog(ctx, runID)
	if err != nil {
		return err
	}
	if n <= RunLogCap {
		return nil
	}
	// The surviving window is the newest RunLogCap rows; everything older goes.
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM run_log WHERE run_id = ? AND id NOT IN (
			SELECT id FROM run_log WHERE run_id = ? ORDER BY id DESC LIMIT ?
		 )`, runID, runID, RunLogCap); err != nil {
		return fmt.Errorf("trim run log: %w", err)
	}
	return nil
}

// RunLog returns a run's activity lines oldest first. The result is never nil so
// it serialises as an empty JSON array.
func (s *Store) RunLog(ctx context.Context, runID string) ([]RunLogLine, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ts, line FROM run_log WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("read run log: %w", err)
	}
	defer rows.Close()
	out := []RunLogLine{}
	for rows.Next() {
		var ts, line string
		if err := rows.Scan(&ts, &line); err != nil {
			return nil, fmt.Errorf("scan run log line: %w", err)
		}
		out = append(out, RunLogLine{TS: parseTime(ts), Line: line})
	}
	return out, rows.Err()
}

// DeleteRunLog removes every activity line of a run.
func (s *Store) DeleteRunLog(ctx context.Context, runID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM run_log WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("delete run log: %w", err)
	}
	return nil
}

// CountRunLog returns how many activity lines a run currently has.
func (s *Store) CountRunLog(ctx context.Context, runID string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run_log WHERE run_id = ?`, runID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count run log: %w", err)
	}
	return n, nil
}
