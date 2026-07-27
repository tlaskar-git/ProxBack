package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const helperCols = `id, node, address, port, version, access_secret_enc, api_key_hash, last_seen, registered_at`

func (s *Store) scanHelper(sc interface{ Scan(...any) error }) (*NodeHelper, error) {
	var h NodeHelper
	var enc []byte
	var lastSeen sql.NullString
	var registered string
	if err := sc.Scan(&h.ID, &h.Node, &h.Address, &h.Port, &h.Version, &enc,
		&h.APIKeyHash, &lastSeen, &registered); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan helper: %w", err)
	}
	secret, err := s.Decrypt(enc)
	if err != nil {
		return nil, fmt.Errorf("helper %s access secret: %w", h.ID, err)
	}
	h.AccessSecret = secret
	h.LastSeen = nullTime(lastSeen)
	h.RegisteredAt = parseTime(registered)
	return &h, nil
}

// CreateHelper registers a node helper, encrypting its access secret. A node
// runs exactly one helper, so an existing registration for the same node is
// replaced — re-running the installer re-enrolls rather than duplicating.
func (s *Store) CreateHelper(ctx context.Context, h *NodeHelper) (*NodeHelper, error) {
	if h.ID == "" {
		h.ID = NewID()
	}
	if h.Port == 0 {
		h.Port = DefaultHelperPort
	}
	h.RegisteredAt = Now()
	enc, err := s.Encrypt(h.AccessSecret)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM helpers WHERE node = ?`, h.Node); err != nil {
		return nil, fmt.Errorf("replace helper for node %q: %w", h.Node, err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO helpers (`+helperCols+`) VALUES (?,?,?,?,?,?,?,?,?)`,
		h.ID, h.Node, h.Address, h.Port, h.Version, enc, h.APIKeyHash,
		fmtTimePtr(h.LastSeen), fmtTime(h.RegisteredAt))
	if err != nil {
		return nil, fmt.Errorf("create helper: %w", err)
	}
	return h, nil
}

// ListHelpers returns every registered helper ordered by node name.
func (s *Store) ListHelpers(ctx context.Context) ([]*NodeHelper, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+helperCols+` FROM helpers ORDER BY node`)
	if err != nil {
		return nil, fmt.Errorf("list helpers: %w", err)
	}
	defer rows.Close()
	var out []*NodeHelper
	for rows.Next() {
		h, err := s.scanHelper(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// HelperByID loads one helper.
func (s *Store) HelperByID(ctx context.Context, id string) (*NodeHelper, error) {
	return s.scanHelper(s.db.QueryRowContext(ctx, `SELECT `+helperCols+` FROM helpers WHERE id = ?`, id))
}

// HelperByNode loads the helper registered for a Proxmox node name.
func (s *Store) HelperByNode(ctx context.Context, node string) (*NodeHelper, error) {
	return s.scanHelper(s.db.QueryRowContext(ctx,
		`SELECT `+helperCols+` FROM helpers WHERE node = ? ORDER BY registered_at DESC LIMIT 1`, node))
}

// HelperByKeyHash resolves a helper from a hashed API key.
func (s *Store) HelperByKeyHash(ctx context.Context, hash string) (*NodeHelper, error) {
	return s.scanHelper(s.db.QueryRowContext(ctx,
		`SELECT `+helperCols+` FROM helpers WHERE api_key_hash = ?`, hash))
}

// TouchHelper records a heartbeat.
func (s *Store) TouchHelper(ctx context.Context, id string, at time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE helpers SET last_seen = ? WHERE id = ?`, fmtTime(at), id); err != nil {
		return fmt.Errorf("touch helper: %w", err)
	}
	return nil
}

// DeleteHelper removes a helper registration.
func (s *Store) DeleteHelper(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM helpers WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete helper: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountHelpers returns the number of registered helpers.
func (s *Store) CountHelpers(ctx context.Context) (int, error) { return s.count(ctx, `helpers`) }
