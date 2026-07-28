package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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

// TouchAgent records a heartbeat without changing what the agent is running.
func (s *Store) TouchAgent(ctx context.Context, id string, at time.Time) error {
	return s.RecordAgentHeartbeat(ctx, id, at, "", "", "")
}

// RecordAgentHeartbeat records a heartbeat and refreshes what the agent says it
// is running.
//
// The version is written on every beat rather than only at registration, which
// is the difference between a console that shows the fleet and one that shows
// what the fleet looked like the day it was installed: an agent that upgrades —
// or is downgraded — is reflected within one heartbeat. That gap is not
// hypothetical. A server that had reached 0.6.2 displayed one of its Windows
// agents as "1.0.0" while it was in fact still running 0.6.1 and still failing
// with a bug fixed in 0.6.2, because the version was captured once, at
// registration, and never looked at again.
//
// Empty fields are left alone, so an agent old enough to send nothing keeps the
// version it registered with rather than having it erased.
func (s *Store) RecordAgentHeartbeat(ctx context.Context, id string, at time.Time, version, goos, goarch string) error {
	sets := []string{`last_seen = ?`}
	args := []any{fmtTime(at)}
	for _, f := range []struct{ col, val string }{
		{`version`, version}, {`os`, goos}, {`arch`, goarch},
	} {
		if v := strings.TrimSpace(f.val); v != "" {
			sets = append(sets, f.col+` = ?`)
			args = append(args, v)
		}
	}
	args = append(args, id)
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET `+strings.Join(sets, `, `)+` WHERE id = ?`, args...)
	if err != nil {
		return fmt.Errorf("record agent heartbeat: %w", err)
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
