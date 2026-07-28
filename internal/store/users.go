package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------- roles

// Role is what a user is allowed to do. The three roles are strictly ordered —
// every capability of a lesser role belongs to a greater one — so a capability
// question is answered by one comparison rather than by string equality
// scattered through the handlers.
type Role string

// The three roles. Their meaning is the PLAN's capability table:
//
//	admin    everything, including users, credentials, hosts, targets, settings
//	         and software updates
//	operator run, cancel and retry jobs, restore, verify, create and edit jobs
//	viewer   read everything except secrets, no mutation at all
const (
	RoleAdmin    Role = "admin"
	RoleOperator Role = "operator"
	RoleViewer   Role = "viewer"
)

// Roles lists the known roles, most privileged first.
var Roles = []Role{RoleAdmin, RoleOperator, RoleViewer}

// rank orders the roles. An unknown value ranks below every real role, so a row
// this build does not understand can do nothing at all rather than silently
// gaining privilege.
func (r Role) rank() int {
	switch r {
	case RoleAdmin:
		return 3
	case RoleOperator:
		return 2
	case RoleViewer:
		return 1
	default:
		return 0
	}
}

// Valid reports whether r is one of the three known roles.
func (r Role) Valid() bool { return r.rank() > 0 }

// AtLeast reports whether r carries every capability of other. An unknown role
// on either side answers false: authorisation fails closed.
func (r Role) AtLeast(other Role) bool {
	return r.Valid() && other.Valid() && r.rank() >= other.rank()
}

// CanRead reports whether r may read the estate. Every role may.
func (r Role) CanRead() bool { return r.AtLeast(RoleViewer) }

// CanOperate reports whether r may run, cancel and retry jobs, restore, verify
// and create or edit jobs.
func (r Role) CanOperate() bool { return r.AtLeast(RoleOperator) }

// CanAdminister reports whether r may touch users, credentials, hosts, storage
// targets, node helpers, agents, settings and software updates.
func (r Role) CanAdminister() bool { return r.AtLeast(RoleAdmin) }

// ParseRole validates a role received from a client.
func ParseRole(v string) (Role, bool) {
	r := Role(v)
	return r, r.Valid()
}

// ErrLastAdmin is returned when an operation would leave the installation
// without an admin. You cannot lock yourself out.
var ErrLastAdmin = errors.New("the last admin cannot be removed or demoted")

// ErrDuplicateUser is returned when a username is already taken.
var ErrDuplicateUser = errors.New("username is already taken")

// migrateUserRoles gives users written before roles existed the admin role. The
// added column defaults to the empty string rather than to "admin" so that a
// value this code did not write can never be mistaken for a privileged one; the
// backfill below is the only thing that grants admin, and it only ever touches
// rows that predate the column. The pass is idempotent: it runs on every open.
func (s *Store) migrateUserRoles(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE users SET role = ? WHERE role IS NULL OR role = ''`, string(RoleAdmin)); err != nil {
		return fmt.Errorf("migrate user roles: %w", err)
	}
	return nil
}

// CountUsers returns the number of configured users.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

// CreateUser inserts a user with an already-hashed password and a role.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string, role Role) (*User, error) {
	if !role.Valid() {
		return nil, fmt.Errorf("create user %q: unknown role %q", username, role)
	}
	now := Now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, role, created_at) VALUES (?,?,?,?)`,
		username, passwordHash, string(role), fmtTime(now))
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicateUser
		}
		return nil, fmt.Errorf("create user %q: %w", username, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("create user %q: %w", username, err)
	}
	return &User{
		ID: id, Username: username, Role: role,
		PasswordHash: passwordHash, CreatedAt: now,
	}, nil
}

// isUniqueViolation reports whether err is SQLite's complaint about a UNIQUE
// constraint, which for users means the name is taken.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique constraint")
}

// userColumns is the one column list every user read uses, so a scan can never
// drift from the query that feeds it.
const userColumns = `id, username, role, password_hash, created_at, last_login_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanUserRow(row rowScanner) (*User, error) {
	var u User
	var role, created string
	var lastLogin sql.NullString
	err := row.Scan(&u.ID, &u.Username, &role, &u.PasswordHash, &created, &lastLogin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan user: %w", err)
	}
	u.Role = Role(role)
	u.CreatedAt = parseTime(created)
	u.LastLoginAt = nullTime(lastLogin)
	return &u, nil
}

// UserByUsername looks a user up by name.
func (s *Store) UserByUsername(ctx context.Context, username string) (*User, error) {
	return scanUserRow(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE username = ?`, username))
}

// UserByID looks a user up by id.
func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUserRow(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

// ListUsers returns every user, oldest first. Password hashes are populated —
// the API layer is responsible for never putting them on the wire.
func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userColumns+` FROM users ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUserRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return out, nil
}

// CountAdmins returns how many users hold the admin role.
func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE role = ?`, string(RoleAdmin)).Scan(&n); err != nil {
		return 0, fmt.Errorf("count admins: %w", err)
	}
	return n, nil
}

// UpdateUserRole changes a user's role. Demoting the last admin returns
// ErrLastAdmin: an installation with no admin can never be administered again.
func (s *Store) UpdateUserRole(ctx context.Context, id int64, role Role) error {
	if !role.Valid() {
		return fmt.Errorf("update role of user %d: unknown role %q", id, role)
	}
	u, err := s.UserByID(ctx, id)
	if err != nil {
		return err
	}
	if u.Role == role {
		return nil
	}
	if u.Role == RoleAdmin {
		admins, err := s.CountAdmins(ctx)
		if err != nil {
			return err
		}
		if admins <= 1 {
			return ErrLastAdmin
		}
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET role = ? WHERE id = ?`, string(role), id)
	if err != nil {
		return fmt.Errorf("update role of user %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUser removes a user and, with them, every session they hold: an account
// that no longer exists must not keep a live browser session. Deleting the last
// admin returns ErrLastAdmin.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	u, err := s.UserByID(ctx, id)
	if err != nil {
		return err
	}
	if u.Role == RoleAdmin {
		admins, err := s.CountAdmins(ctx)
		if err != nil {
			return err
		}
		if admins <= 1 {
			return ErrLastAdmin
		}
	}
	// Sessions go first: if the row deletion fails the account still exists, and
	// a revoked session is a safe outcome either way.
	if err := s.DeleteUserSessions(ctx, id); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete user %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchUserLogin records a successful sign-in.
func (s *Store) TouchUserLogin(ctx context.Context, id int64, at time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE users SET last_login_at = ? WHERE id = ?`, fmtTime(at), id); err != nil {
		return fmt.Errorf("record login of user %d: %w", id, err)
	}
	return nil
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

// DeleteUserSessions revokes every session a user holds.
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("delete sessions of user %d: %w", userID, err)
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
