package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const targetCols = `id, name, endpoint, region, bucket, access_key, secret_key_enc, path_style, status, created_at`

func (s *Store) scanTarget(sc interface{ Scan(...any) error }) (*S3Target, error) {
	var t S3Target
	var enc []byte
	var pathStyle int
	var created string
	if err := sc.Scan(&t.ID, &t.Name, &t.Endpoint, &t.Region, &t.Bucket, &t.AccessKey, &enc, &pathStyle, &t.Status, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan s3 target: %w", err)
	}
	secret, err := s.Decrypt(enc)
	if err != nil {
		return nil, fmt.Errorf("s3 target %s secret key: %w", t.ID, err)
	}
	t.SecretKey = secret
	t.PathStyle = pathStyle != 0
	t.CreatedAt = parseTime(created)
	return &t, nil
}

// CreateS3Target stores a backup target, encrypting the secret key.
func (s *Store) CreateS3Target(ctx context.Context, t *S3Target) (*S3Target, error) {
	if t.ID == "" {
		t.ID = NewID()
	}
	if t.Status == "" {
		t.Status = "unknown"
	}
	t.CreatedAt = Now()
	enc, err := s.Encrypt(t.SecretKey)
	if err != nil {
		return nil, err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO s3_targets (`+targetCols+`) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Name, t.Endpoint, t.Region, t.Bucket, t.AccessKey, enc, boolInt(t.PathStyle), t.Status, fmtTime(t.CreatedAt))
	if err != nil {
		return nil, fmt.Errorf("create s3 target: %w", err)
	}
	return t, nil
}

// ListS3Targets returns all targets ordered by name.
func (s *Store) ListS3Targets(ctx context.Context) ([]*S3Target, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+targetCols+` FROM s3_targets ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list s3 targets: %w", err)
	}
	defer rows.Close()
	var out []*S3Target
	for rows.Next() {
		t, err := s.scanTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// S3TargetByID loads one target with its decrypted secret key.
func (s *Store) S3TargetByID(ctx context.Context, id string) (*S3Target, error) {
	return s.scanTarget(s.db.QueryRowContext(ctx, `SELECT `+targetCols+` FROM s3_targets WHERE id = ?`, id))
}

// UpdateS3TargetStatus records a connectivity probe result.
func (s *Store) UpdateS3TargetStatus(ctx context.Context, id, status string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE s3_targets SET status = ? WHERE id = ?`, status, id); err != nil {
		return fmt.Errorf("update s3 target status: %w", err)
	}
	return nil
}

// DeleteS3Target removes a target plus its chunk index rows.
func (s *Store) DeleteS3Target(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM chunk_index WHERE target_id = ?`, id); err != nil {
		return fmt.Errorf("delete target chunk index: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM s3_targets WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete s3 target: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountS3Targets returns the number of configured targets.
func (s *Store) CountS3Targets(ctx context.Context) (int, error) { return s.count(ctx, `s3_targets`) }

// ---------------------------------------------------------------- chunk index

// HasChunk reports whether the chunk is known to exist on the target.
func (s *Store) HasChunk(ctx context.Context, targetID, sha string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chunk_index WHERE target_id = ? AND sha256 = ?`, targetID, sha).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("chunk index lookup: %w", err)
	}
	return n > 0, nil
}

// AddChunk records a chunk as present on the target.
func (s *Store) AddChunk(ctx context.Context, targetID, sha string, size int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO chunk_index (target_id, sha256, size) VALUES (?,?,?)
		 ON CONFLICT(target_id, sha256) DO UPDATE SET size = excluded.size`,
		targetID, sha, size)
	if err != nil {
		return fmt.Errorf("add chunk index: %w", err)
	}
	return nil
}

// DeleteChunk removes a chunk from the index.
func (s *Store) DeleteChunk(ctx context.Context, targetID, sha string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM chunk_index WHERE target_id = ? AND sha256 = ?`, targetID, sha); err != nil {
		return fmt.Errorf("delete chunk index: %w", err)
	}
	return nil
}

// ChunkShas returns every indexed chunk hash for a target.
func (s *Store) ChunkShas(ctx context.Context, targetID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT sha256 FROM chunk_index WHERE target_id = ?`, targetID)
	if err != nil {
		return nil, fmt.Errorf("list chunk index: %w", err)
	}
	defer rows.Close()
	var shas []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, fmt.Errorf("list chunk index: %w", err)
		}
		shas = append(shas, sha)
	}
	return shas, rows.Err()
}

// ChunkStats returns the number of indexed chunks and their total size for a
// target. Passing an empty targetID aggregates across all targets.
func (s *Store) ChunkStats(ctx context.Context, targetID string) (count int, bytes int64, err error) {
	q := `SELECT COUNT(*), COALESCE(SUM(size),0) FROM chunk_index`
	args := []any{}
	if targetID != "" {
		q += ` WHERE target_id = ?`
		args = append(args, targetID)
	}
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&count, &bytes); err != nil {
		return 0, 0, fmt.Errorf("chunk stats: %w", err)
	}
	return count, bytes, nil
}
