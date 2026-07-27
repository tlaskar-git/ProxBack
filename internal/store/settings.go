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

// DefaultNotifyOn disables run notifications until an operator opts in.
const DefaultNotifyOn = NotifyOff

// Throughput defaults. They apply to databases that have no such rows, which is
// every database created before v0.3.2: the settings table is key/value, so an
// upgrade needs nothing but this default handling.
const (
	// DefaultUploadConcurrency is how many chunk uploads run in parallel.
	DefaultUploadConcurrency = 4
	// DefaultCompression compresses chunks with zstd.
	DefaultCompression = CompressionZstd
	// DefaultUploadLimitMbps leaves upload throughput unlimited.
	DefaultUploadLimitMbps = 0
)

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
	out := Settings{
		ServerName:        DefaultServerName,
		Concurrency:       DefaultConcurrency,
		NotifyOn:          DefaultNotifyOn,
		UploadConcurrency: DefaultUploadConcurrency,
		Compression:       DefaultCompression,
		UploadLimitMbps:   DefaultUploadLimitMbps,
	}
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
	hook, err := s.settingValue(ctx, "webhookUrl")
	switch {
	case err == nil:
		out.WebhookURL = hook
	case errors.Is(err, ErrNotFound):
	default:
		return out, err
	}
	notify, err := s.settingValue(ctx, "notifyOn")
	switch {
	case err == nil:
		if ValidNotifyOn(notify) {
			out.NotifyOn = notify
		}
	case errors.Is(err, ErrNotFound):
	default:
		return out, err
	}
	upConc, err := s.settingValue(ctx, "uploadConcurrency")
	switch {
	case err == nil:
		if n, cerr := strconv.Atoi(upConc); cerr == nil && n >= MinUploadConcurrency && n <= MaxUploadConcurrency {
			out.UploadConcurrency = n
		}
	case errors.Is(err, ErrNotFound):
	default:
		return out, err
	}
	compression, err := s.settingValue(ctx, "compression")
	switch {
	case err == nil:
		if ValidCompression(compression) {
			out.Compression = compression
		}
	case errors.Is(err, ErrNotFound):
	default:
		return out, err
	}
	limit, err := s.settingValue(ctx, "uploadLimitMbps")
	switch {
	case err == nil:
		if n, cerr := strconv.Atoi(limit); cerr == nil && n >= MinUploadLimitMbps && n <= MaxUploadLimitMbps {
			out.UploadLimitMbps = n
		}
	case errors.Is(err, ErrNotFound):
	default:
		return out, err
	}
	return out, nil
}

// SaveSettings persists the global settings, normalising invalid values to
// their defaults.
func (s *Store) SaveSettings(ctx context.Context, st Settings) error {
	if st.ServerName == "" {
		st.ServerName = DefaultServerName
	}
	if st.Concurrency <= 0 {
		st.Concurrency = DefaultConcurrency
	}
	if !ValidNotifyOn(st.NotifyOn) {
		st.NotifyOn = DefaultNotifyOn
	}
	if st.UploadConcurrency < MinUploadConcurrency || st.UploadConcurrency > MaxUploadConcurrency {
		st.UploadConcurrency = DefaultUploadConcurrency
	}
	if !ValidCompression(st.Compression) {
		st.Compression = DefaultCompression
	}
	if st.UploadLimitMbps < MinUploadLimitMbps || st.UploadLimitMbps > MaxUploadLimitMbps {
		st.UploadLimitMbps = DefaultUploadLimitMbps
	}
	if err := s.setSetting(ctx, "serverName", st.ServerName); err != nil {
		return err
	}
	if err := s.setSetting(ctx, "concurrency", strconv.Itoa(st.Concurrency)); err != nil {
		return err
	}
	if err := s.setSetting(ctx, "webhookUrl", st.WebhookURL); err != nil {
		return err
	}
	if err := s.setSetting(ctx, "notifyOn", st.NotifyOn); err != nil {
		return err
	}
	if err := s.setSetting(ctx, "uploadConcurrency", strconv.Itoa(st.UploadConcurrency)); err != nil {
		return err
	}
	if err := s.setSetting(ctx, "compression", st.Compression); err != nil {
		return err
	}
	return s.setSetting(ctx, "uploadLimitMbps", strconv.Itoa(st.UploadLimitMbps))
}
