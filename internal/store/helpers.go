package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const helperCols = `id, host_id, node, address, port, version, access_secret_enc, api_key_hash, last_seen, registered_at`

func (s *Store) scanHelper(sc interface{ Scan(...any) error }) (*NodeHelper, error) {
	var h NodeHelper
	var enc []byte
	var lastSeen sql.NullString
	var registered string
	if err := sc.Scan(&h.ID, &h.HostID, &h.Node, &h.Address, &h.Port, &h.Version, &enc,
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
// of one host runs exactly one helper, so an existing registration for the same
// (host, node) pair is replaced — re-running the installer re-enrolls rather
// than duplicating. Crucially it is only that pair: a node called "pve1" in one
// cluster must never displace the "pve1" of another.
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
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM helpers WHERE host_id = ? AND node = ?`, h.HostID, h.Node); err != nil {
		return nil, fmt.Errorf("replace helper for node %q: %w", h.Node, err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO helpers (`+helperCols+`) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		h.ID, h.HostID, h.Node, h.Address, h.Port, h.Version, enc, h.APIKeyHash,
		fmtTimePtr(h.LastSeen), fmtTime(h.RegisteredAt))
	if err != nil {
		return nil, fmt.Errorf("create helper: %w", err)
	}
	return h, nil
}

// AssignHelperHost binds an existing registration to a Proxmox host, which is
// what turns an unassigned helper into a routable one without redeploying it.
// It fails when the host already has a helper for that node, because that pair
// is the helper's identity.
func (s *Store) AssignHelperHost(ctx context.Context, id, hostID string) error {
	h, err := s.HelperByID(ctx, id)
	if err != nil {
		return err
	}
	if h.HostID == hostID {
		return nil
	}
	var clash int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM helpers WHERE host_id = ? AND node = ? AND id <> ?`,
		hostID, h.Node, id).Scan(&clash); err != nil {
		return fmt.Errorf("assign helper host: %w", err)
	}
	if clash > 0 {
		return fmt.Errorf("that host already has a node helper for node %q", h.Node)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE helpers SET host_id = ? WHERE id = ?`, hostID, id)
	if err != nil {
		return fmt.Errorf("assign helper host: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListHelpers returns every registered helper ordered by host then node name.
func (s *Store) ListHelpers(ctx context.Context) ([]*NodeHelper, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+helperCols+` FROM helpers ORDER BY node, host_id`)
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

// HelperFor loads the helper that serves a node of a specific Proxmox host.
// This is the only lookup used for routing: resolving by node name alone would
// send a backup or a restore to the "pve1" of the wrong cluster. An empty
// hostID answers ErrNotFound rather than matching unassigned registrations.
func (s *Store) HelperFor(ctx context.Context, hostID, node string) (*NodeHelper, error) {
	if hostID == "" || node == "" {
		return nil, ErrNotFound
	}
	return s.scanHelper(s.db.QueryRowContext(ctx,
		`SELECT `+helperCols+` FROM helpers WHERE host_id = ? AND node = ?
		 ORDER BY registered_at DESC LIMIT 1`, hostID, node))
}

// UnassignedHelperForNode finds a registration that carries the node's name but
// no host. It exists so a run can tell "no helper at all" (fall back to the
// export extension) from "a helper is installed but nobody knows which cluster
// it belongs to", which is an operator action, not a silent guess.
func (s *Store) UnassignedHelperForNode(ctx context.Context, node string) (*NodeHelper, error) {
	if node == "" {
		return nil, ErrNotFound
	}
	return s.scanHelper(s.db.QueryRowContext(ctx,
		`SELECT `+helperCols+` FROM helpers WHERE host_id = '' AND node = ?
		 ORDER BY registered_at DESC LIMIT 1`, node))
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
