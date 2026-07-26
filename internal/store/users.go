package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CountUsers returns the number of configured users.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

// CreateUser inserts a user with an already-hashed password.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string) (*User, error) {
	now := Now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, created_at) VALUES (?,?,?)`,
		username, passwordHash, fmtTime(now))
	if err != nil {
		return nil, fmt.Errorf("create user %q: %w", username, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("create user %q: %w", username, err)
	}
	return &User{ID: id, Username: username, PasswordHash: passwordHash, CreatedAt: now}, nil
}

func scanUser(row *sql.Row) (*User, error) {
	var u User
	var created string
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan user: %w", err)
	}
	u.CreatedAt = parseTime(created)
	return &u, nil
}

// UserByUsername looks a user up by name.
func (s *Store) UserByUsername(ctx context.Context, username string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, created_at FROM users WHERE username = ?`, username))
}

// UserByID looks a user up by id.
func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, created_at FROM users WHERE id = ?`, id))
}

// UpdateUserPassword replaces a user's password hash.
func (s *Store) UpdateUserPassword(ctx context.Context, id int64, passwordHash string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, id)
	if err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------- default password flag

const defaultPasswordKey = "defaultAdminPassword"

// SetDefaultPasswordFlag records whether the seeded admin/admin credentials are
// still in place.
func (s *Store) SetDefaultPasswordFlag(ctx context.Context, on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	return s.setSetting(ctx, defaultPasswordKey, v)
}

// DefaultPasswordFlag reports whether the seeded default credentials are still
// in place.
func (s *Store) DefaultPasswordFlag(ctx context.Context) (bool, error) {
	v, err := s.settingValue(ctx, defaultPasswordKey)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v == "1", nil
}

// ---------------------------------------------------------------- sessions

// CreateSession stores an opaque session token.
func (s *Store) CreateSession(ctx context.Context, token string, userID int64, ttl time.Duration) (*Session, error) {
	now := Now()
	sess := &Session{Token: token, UserID: userID, CreatedAt: now, ExpiresAt: now.Add(ttl)}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token, user_id, created_at, expires_at) VALUES (?,?,?,?)`,
		sess.Token, sess.UserID, fmtTime(sess.CreatedAt), fmtTime(sess.ExpiresAt))
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return sess, nil
}

// SessionByToken returns a non-expired session.
func (s *Store) SessionByToken(ctx context.Context, token string) (*Session, error) {
	var sess Session
	var created, expires string
	err := s.db.QueryRowContext(ctx,
		`SELECT token, user_id, created_at, expires_at FROM sessions WHERE token = ?`, token).
		Scan(&sess.Token, &sess.UserID, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	sess.CreatedAt = parseTime(created)
	sess.ExpiresAt = parseTime(expires)
	if time.Now().UTC().After(sess.ExpiresAt) {
		_ = s.DeleteSession(ctx, token)
		return nil, ErrNotFound
	}
	return &sess, nil
}

// DeleteSession removes a session token.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token = ?`, token); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DeleteOtherSessions removes every session of a user except the given token.
// Used after a password change so stolen sessions die with the old password.
func (s *Store) DeleteOtherSessions(ctx context.Context, userID int64, keepToken string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE user_id = ? AND token <> ?`, userID, keepToken); err != nil {
		return fmt.Errorf("delete other sessions: %w", err)
	}
	return nil
}

// PurgeExpiredSessions deletes sessions whose expiry has passed.
func (s *Store) PurgeExpiredSessions(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, fmtTime(Now())); err != nil {
		return fmt.Errorf("purge sessions: %w", err)
	}
	return nil
}
