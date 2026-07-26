package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const pveHostCols = `id, name, base_url, token_id, token_secret_enc, insecure_tls, status, last_seen, created_at`

func (s *Store) scanPVEHost(sc interface{ Scan(...any) error }) (*PVEHost, error) {
	var h PVEHost
	var enc []byte
	var insecure int
	var lastSeen sql.NullString
	var created string
	if err := sc.Scan(&h.ID, &h.Name, &h.BaseURL, &h.TokenID, &enc, &insecure, &h.Status, &lastSeen, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan pve host: %w", err)
	}
	secret, err := s.Decrypt(enc)
	if err != nil {
		return nil, fmt.Errorf("pve host %s token secret: %w", h.ID, err)
	}
	h.TokenSecret = secret
	h.InsecureTLS = insecure != 0
	h.LastSeen = nullTime(lastSeen)
	h.CreatedAt = parseTime(created)
	return &h, nil
}

// CreatePVEHost stores a Proxmox host, encrypting the API token secret.
func (s *Store) CreatePVEHost(ctx context.Context, h *PVEHost) (*PVEHost, error) {
	if h.ID == "" {
		h.ID = NewID()
	}
	if h.Status == "" {
		h.Status = "unknown"
	}
	h.CreatedAt = Now()
	enc, err := s.Encrypt(h.TokenSecret)
	if err != nil {
		return nil, err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO pve_hosts (`+pveHostCols+`) VALUES (?,?,?,?,?,?,?,?,?)`,
		h.ID, h.Name, h.BaseURL, h.TokenID, enc, boolInt(h.InsecureTLS), h.Status,
		fmtTimePtr(h.LastSeen), fmtTime(h.CreatedAt))
	if err != nil {
		return nil, fmt.Errorf("create pve host: %w", err)
	}
	return h, nil
}

// ListPVEHosts returns all configured hosts ordered by name.
func (s *Store) ListPVEHosts(ctx context.Context) ([]*PVEHost, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+pveHostCols+` FROM pve_hosts ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list pve hosts: %w", err)
	}
	defer rows.Close()
	var out []*PVEHost
	for rows.Next() {
		h, err := s.scanPVEHost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// PVEHostByID loads one host.
func (s *Store) PVEHostByID(ctx context.Context, id string) (*PVEHost, error) {
	return s.scanPVEHost(s.db.QueryRowContext(ctx, `SELECT `+pveHostCols+` FROM pve_hosts WHERE id = ?`, id))
}

// UpdatePVEHostStatus records a connectivity probe result.
func (s *Store) UpdatePVEHostStatus(ctx context.Context, id, status string, lastSeen *time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE pve_hosts SET status = ?, last_seen = ? WHERE id = ?`,
		status, fmtTimePtr(lastSeen), id)
	if err != nil {
		return fmt.Errorf("update pve host status: %w", err)
	}
	return nil
}

// DeletePVEHost removes a host and its cached inventory.
func (s *Store) DeletePVEHost(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM vms_cache WHERE host_id = ?`, id); err != nil {
		return fmt.Errorf("delete host vms: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM pve_hosts WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete pve host: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------- vms cache

// ReplaceVMCache replaces the cached inventory for one host.
func (s *Store) ReplaceVMCache(ctx context.Context, hostID string, vms []VM) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin vm cache tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM vms_cache WHERE host_id = ?`, hostID); err != nil {
		return fmt.Errorf("clear vm cache: %w", err)
	}
	now := fmtTime(Now())
	for _, v := range vms {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO vms_cache (host_id, vmid, name, node, status, maxdisk, maxmem, uptime, updated_at)
			 VALUES (?,?,?,?,?,?,?,?,?)`,
			hostID, v.VMID, v.Name, v.Node, v.Status, v.MaxDisk, v.MaxMem, v.Uptime, now)
		if err != nil {
			return fmt.Errorf("insert vm cache row: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit vm cache: %w", err)
	}
	return nil
}

// ListCachedVMs returns the cached inventory across all hosts.
func (s *Store) ListCachedVMs(ctx context.Context) ([]VM, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.host_id, h.name, c.vmid, c.name, c.node, c.status, c.maxdisk, c.maxmem, c.uptime
		   FROM vms_cache c LEFT JOIN pve_hosts h ON h.id = c.host_id
		 ORDER BY c.vmid`)
	if err != nil {
		return nil, fmt.Errorf("list cached vms: %w", err)
	}
	defer rows.Close()
	out := []VM{}
	for rows.Next() {
		var v VM
		var hostName sql.NullString
		if err := rows.Scan(&v.HostID, &hostName, &v.VMID, &v.Name, &v.Node, &v.Status, &v.MaxDisk, &v.MaxMem, &v.Uptime); err != nil {
			return nil, fmt.Errorf("scan cached vm: %w", err)
		}
		v.HostName = nullStr(hostName)
		out = append(out, v)
	}
	return out, rows.Err()
}

// CachedVM returns one cached VM row.
func (s *Store) CachedVM(ctx context.Context, hostID string, vmid int) (*VM, error) {
	var v VM
	err := s.db.QueryRowContext(ctx,
		`SELECT host_id, vmid, name, node, status, maxdisk, maxmem, uptime FROM vms_cache WHERE host_id = ? AND vmid = ?`,
		hostID, vmid).Scan(&v.HostID, &v.VMID, &v.Name, &v.Node, &v.Status, &v.MaxDisk, &v.MaxMem, &v.Uptime)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load cached vm: %w", err)
	}
	return &v, nil
}

// CountVMs returns the number of cached guests.
func (s *Store) CountVMs(ctx context.Context) (int, error) { return s.count(ctx, `vms_cache`) }

// CountPVEHosts returns the number of configured hosts.
func (s *Store) CountPVEHosts(ctx context.Context) (int, error) { return s.count(ctx, `pve_hosts`) }

func (s *Store) count(ctx context.Context, table string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
		return 0, fmt.Errorf("count %s: %w", table, err)
	}
	return n, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
