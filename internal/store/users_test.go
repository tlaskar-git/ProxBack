package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, for building the legacy fixture

	"proxback/internal/store"
)

// The role model is a strict order, so a capability question is one comparison.
// An unrecognised role — a row written by a future release, or a corrupted one —
// can do nothing at all: authorisation fails closed.
func TestRoleCapabilities(t *testing.T) {
	for _, tc := range []struct {
		role                      store.Role
		read, operate, administer bool
	}{
		{store.RoleAdmin, true, true, true},
		{store.RoleOperator, true, true, false},
		{store.RoleViewer, true, false, false},
		{store.Role(""), false, false, false},
		{store.Role("superuser"), false, false, false},
	} {
		if got := tc.role.CanRead(); got != tc.read {
			t.Errorf("%q.CanRead() = %v, want %v", tc.role, got, tc.read)
		}
		if got := tc.role.CanOperate(); got != tc.operate {
			t.Errorf("%q.CanOperate() = %v, want %v", tc.role, got, tc.operate)
		}
		if got := tc.role.CanAdminister(); got != tc.administer {
			t.Errorf("%q.CanAdminister() = %v, want %v", tc.role, got, tc.administer)
		}
	}
	if _, ok := store.ParseRole("admin"); !ok {
		t.Error(`ParseRole("admin") rejected a real role`)
	}
	if _, ok := store.ParseRole("Admin"); ok {
		t.Error(`ParseRole("Admin") accepted a role that is not one of the three`)
	}
}

// A user is created with a role, read back with it, and never loses their hash to
// a caller that did not ask for it. Names are unique.
func TestUserRolesAndDuplicates(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()

	admin, err := st.CreateUser(ctx, "root", "hash-a", store.RoleAdmin)
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if admin.Role != store.RoleAdmin {
		t.Fatalf("created admin = %+v", admin)
	}
	if _, err := st.CreateUser(ctx, "viewer", "hash-b", store.RoleViewer); err != nil {
		t.Fatalf("create viewer: %v", err)
	}
	if _, err := st.CreateUser(ctx, "root", "hash-c", store.RoleOperator); !errors.Is(err, store.ErrDuplicateUser) {
		t.Fatalf("duplicate username = %v, want ErrDuplicateUser", err)
	}
	if _, err := st.CreateUser(ctx, "nonsense", "hash-d", store.Role("root")); err == nil {
		t.Fatal("an unknown role was accepted")
	}

	users, err := st.ListUsers(ctx)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 2 || users[0].Username != "root" || users[1].Role != store.RoleViewer {
		t.Fatalf("users = %+v", users)
	}
	if n, err := st.CountAdmins(ctx); err != nil || n != 1 {
		t.Fatalf("admins = %d (%v), want 1", n, err)
	}
}

// The last admin can be neither demoted nor deleted: an installation with no
// admin can never be administered again, so the store refuses before the API
// ever has to.
func TestLastAdminIsProtected(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()

	admin, err := st.CreateUser(ctx, "root", "hash-a", store.RoleAdmin)
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	operator, err := st.CreateUser(ctx, "opsy", "hash-b", store.RoleOperator)
	if err != nil {
		t.Fatalf("create operator: %v", err)
	}

	if err := st.UpdateUserRole(ctx, admin.ID, store.RoleOperator); !errors.Is(err, store.ErrLastAdmin) {
		t.Fatalf("demoting the last admin = %v, want ErrLastAdmin", err)
	}
	if err := st.DeleteUser(ctx, admin.ID); !errors.Is(err, store.ErrLastAdmin) {
		t.Fatalf("deleting the last admin = %v, want ErrLastAdmin", err)
	}
	// With a second admin in place both become possible again.
	if err := st.UpdateUserRole(ctx, operator.ID, store.RoleAdmin); err != nil {
		t.Fatalf("promote operator: %v", err)
	}
	if err := st.UpdateUserRole(ctx, admin.ID, store.RoleViewer); err != nil {
		t.Fatalf("demote the first admin once a second exists: %v", err)
	}
	// And the rule holds for the one that is left.
	if err := st.DeleteUser(ctx, operator.ID); !errors.Is(err, store.ErrLastAdmin) {
		t.Fatalf("deleting the remaining admin = %v, want ErrLastAdmin", err)
	}
}

// Deleting a user takes their sessions with them: an account that no longer
// exists must not keep browsing on a cookie it already holds.
func TestDeleteUserRevokesSessions(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()

	if _, err := st.CreateUser(ctx, "root", "hash-a", store.RoleAdmin); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	victim, err := st.CreateUser(ctx, "opsy", "hash-b", store.RoleOperator)
	if err != nil {
		t.Fatalf("create operator: %v", err)
	}
	if _, err := st.CreateSession(ctx, "tok-victim", victim.ID, time.Hour); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := st.SessionByToken(ctx, "tok-victim"); err != nil {
		t.Fatalf("session should exist: %v", err)
	}
	if err := st.DeleteUser(ctx, victim.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if _, err := st.SessionByToken(ctx, "tok-victim"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session after deletion = %v, want ErrNotFound", err)
	}
	if _, err := st.UserByID(ctx, victim.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("user after deletion = %v, want ErrNotFound", err)
	}
}

// oldUsersSchemaSQL is the users table as shipped installations have it on disk:
// no role, no last sign-in.
const oldUsersSchemaSQL = `
CREATE TABLE users (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	username      TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	created_at    TEXT NOT NULL
);
`

// The single account of an existing installation becomes the admin. That is the
// only thing that grants admin on migration, and it only touches rows that
// predate the column.
func TestMigrationGivesTheExistingUserAdmin(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	dsn := "file:" + filepath.ToSlash(filepath.Join(dir, store.DBFileName))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open old db: %v", err)
	}
	if _, err := db.Exec(oldUsersSchemaSQL); err != nil {
		t.Fatalf("apply old users schema: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO users (username, password_hash, created_at)
		 VALUES ('admin', 'bcrypt-hash', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert legacy user: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close old db: %v", err)
	}

	st, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatalf("open upgraded store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	user, err := st.UserByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("load migrated user: %v", err)
	}
	if user.Role != store.RoleAdmin {
		t.Fatalf("migrated user role = %q, want admin", user.Role)
	}
	if user.PasswordHash != "bcrypt-hash" || user.LastLoginAt != nil {
		t.Fatalf("migrated user = %+v, want the hash untouched and no sign-in", user)
	}
	// A sign-in is recorded from then on.
	at := store.Now()
	if err := st.TouchUserLogin(ctx, user.ID, at); err != nil {
		t.Fatalf("touch login: %v", err)
	}
	again, err := st.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	if again.LastLoginAt == nil || !again.LastLoginAt.Equal(at) {
		t.Fatalf("lastLoginAt = %v, want %v", again.LastLoginAt, at)
	}
}
