package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// DefaultConcurrency is the default number of simultaneously executing runs.
const DefaultConcurrency = 2

// DefaultServerName is the default display name for the installation.
const DefaultServerName = "ProxBack"

func (s *Store) settingValue(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read setting %q: %w", key, err)
	}
	return v, nil
}

func (s *Store) setSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value)
	if err != nil {
		return fmt.Errorf("write setting %q: %w", key, err)
	}
	return nil
}

// Settings returns the global settings, filling in defaults for missing keys.
func (s *Store) Settings(ctx context.Context) (Settings, error) {
	out := Settings{ServerName: DefaultServerName, Concurrency: DefaultConcurrency}
	name, err := s.settingValue(ctx, "serverName")
	switch {
	case err == nil:
		out.ServerName = name
	case errors.Is(err, ErrNotFound):
	default:
		return out, err
	}
	conc, err := s.settingValue(ctx, "concurrency")
	switch {
	case err == nil:
		if n, cerr := strconv.Atoi(conc); cerr == nil && n > 0 {
			out.Concurrency = n
		}
	case errors.Is(err, ErrNotFound):
	default:
		return out, err
	}
	return out, nil
}

// SaveSettings persists the global settings.
func (s *Store) SaveSettings(ctx context.Context, st Settings) error {
	if st.ServerName == "" {
		st.ServerName = DefaultServerName
	}
	if st.Concurrency <= 0 {
		st.Concurrency = DefaultConcurrency
	}
	if err := s.setSetting(ctx, "serverName", st.ServerName); err != nil {
		return err
	}
	return s.setSetting(ctx, "concurrency", strconv.Itoa(st.Concurrency))
}
