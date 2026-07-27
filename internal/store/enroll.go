package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Enrollment token purposes. One table serves both enrollment flows, so a token
// minted for an agent can never register a node helper (or the other way
// round). Databases written before node helpers existed default to "agent".
const (
	EnrollPurposeAgent  = "agent"
	EnrollPurposeHelper = "helper"
)

// CreateEnrollToken stores a single-use enrollment token for one purpose.
// hostID binds a helper token to the Proxmox host the node belongs to; it is
// empty for agent tokens.
func (s *Store) CreateEnrollToken(ctx context.Context, token, purpose, hostID string, ttl time.Duration) (*EnrollToken, error) {
	if purpose == "" {
		purpose = EnrollPurposeAgent
	}
	now := Now()
	et := &EnrollToken{Token: token, Purpose: purpose, HostID: hostID, CreatedAt: now, ExpiresAt: now.Add(ttl)}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO enroll_tokens (token, created_at, expires_at, used_at, purpose, host_id) VALUES (?,?,?,NULL,?,?)`,
		et.Token, fmtTime(et.CreatedAt), fmtTime(et.ExpiresAt), et.Purpose, et.HostID)
	if err != nil {
		return nil, fmt.Errorf("create enroll token: %w", err)
	}
	return et, nil
}

// EnrollTokenByValue loads an enrollment token.
func (s *Store) EnrollTokenByValue(ctx context.Context, token string) (*EnrollToken, error) {
	var et EnrollToken
	var created, expires string
	var used sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT token, created_at, expires_at, used_at, purpose, host_id FROM enroll_tokens WHERE token = ?`, token).
		Scan(&et.Token, &created, &expires, &used, &et.Purpose, &et.HostID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load enroll token: %w", err)
	}
	et.CreatedAt = parseTime(created)
	et.ExpiresAt = parseTime(expires)
	et.UsedAt = nullTime(used)
	return &et, nil
}

// ConsumeEnrollToken atomically marks an unused, unexpired token of the given
// purpose as used.
func (s *Store) ConsumeEnrollToken(ctx context.Context, token, purpose string) error {
	_, err := s.ConsumeEnrollTokenFor(ctx, token, purpose)
	return err
}

// ConsumeEnrollTokenFor consumes a token and returns it, so the caller can read
// what the token was minted for — for a helper token, the Proxmox host whose
// identity the registering node inherits.
func (s *Store) ConsumeEnrollTokenFor(ctx context.Context, token, purpose string) (*EnrollToken, error) {
	if purpose == "" {
		purpose = EnrollPurposeAgent
	}
	now := fmtTime(Now())
	res, err := s.db.ExecContext(ctx,
		`UPDATE enroll_tokens SET used_at = ?
		 WHERE token = ? AND purpose = ? AND used_at IS NULL AND expires_at > ?`,
		now, token, purpose, now)
	if err != nil {
		return nil, fmt.Errorf("consume enroll token: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.EnrollTokenByValue(ctx, token)
}
