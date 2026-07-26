package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const agentCols = `id, hostname, os, arch, version, api_key_hash, last_seen, registered_at`

func scanAgent(sc interface{ Scan(...any) error }) (*Agent, error) {
	var a Agent
	var lastSeen sql.NullString
	var registered string
	if err := sc.Scan(&a.ID, &a.Hostname, &a.OS, &a.Arch, &a.Version, &a.APIKeyHash, &lastSeen, &registered); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan agent: %w", err)
	}
	a.LastSeen = nullTime(lastSeen)
	a.RegisteredAt = parseTime(registered)
	return &a, nil
}

// CreateAgent registers a new agent.
func (s *Store) CreateAgent(ctx context.Context, a *Agent) (*Agent, error) {
	if a.ID == "" {
		a.ID = NewID()
	}
	a.RegisteredAt = Now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO agents (`+agentCols+`) VALUES (?,?,?,?,?,?,?,?)`,
		a.ID, a.Hostname, a.OS, a.Arch, a.Version, a.APIKeyHash, fmtTimePtr(a.LastSeen), fmtTime(a.RegisteredAt))
	if err != nil {
		return nil, fmt.Errorf("create agent: %w", err)
	}
	return a, nil
}

// ListAgents returns all agents ordered by hostname.
func (s *Store) ListAgents(ctx context.Context) ([]*Agent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+agentCols+` FROM agents ORDER BY hostname`)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	defer rows.Close()
	var out []*Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AgentByID loads one agent.
func (s *Store) AgentByID(ctx context.Context, id string) (*Agent, error) {
	return scanAgent(s.db.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id = ?`, id))
}

// AgentByKeyHash resolves an agent from a hashed API key.
func (s *Store) AgentByKeyHash(ctx context.Context, hash string) (*Agent, error) {
	return scanAgent(s.db.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE api_key_hash = ?`, hash))
}

// TouchAgent records a heartbeat.
func (s *Store) TouchAgent(ctx context.Context, id string, at time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE agents SET last_seen = ? WHERE id = ?`, fmtTime(at), id); err != nil {
		return fmt.Errorf("touch agent: %w", err)
	}
	return nil
}

// DeleteAgent removes an agent.
func (s *Store) DeleteAgent(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM agents WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete agent: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountAgents returns the number of enrolled agents.
func (s *Store) CountAgents(ctx context.Context) (int, error) { return s.count(ctx, `agents`) }

// ---------------------------------------------------------------- enroll tokens

// CreateEnrollToken stores a single-use enrollment token.
func (s *Store) CreateEnrollToken(ctx context.Context, token string, ttl time.Duration) (*EnrollToken, error) {
	now := Now()
	et := &EnrollToken{Token: token, CreatedAt: now, ExpiresAt: now.Add(ttl)}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO enroll_tokens (token, created_at, expires_at, used_at) VALUES (?,?,?,NULL)`,
		et.Token, fmtTime(et.CreatedAt), fmtTime(et.ExpiresAt))
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
		`SELECT token, created_at, expires_at, used_at FROM enroll_tokens WHERE token = ?`, token).
		Scan(&et.Token, &created, &expires, &used)
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

// ConsumeEnrollToken atomically marks an unused, unexpired token as used.
func (s *Store) ConsumeEnrollToken(ctx context.Context, token string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE enroll_tokens SET used_at = ? WHERE token = ? AND used_at IS NULL AND expires_at > ?`,
		fmtTime(Now()), token, fmtTime(Now()))
	if err != nil {
		return fmt.Errorf("consume enroll token: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
