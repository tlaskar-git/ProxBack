// Package store implements ProxBack's SQLite persistence layer.
//
// The driver is modernc.org/sqlite (pure Go, no CGO). Secrets (S3 secret keys and
// Proxmox API token secrets) are encrypted at rest with AES-GCM using a 32 byte key
// stored next to the database in <dataDir>/secret.key, generated on first run.
package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// KeyFileName is the name of the AES-GCM key file inside the data directory.
const KeyFileName = "secret.key"

// DBFileName is the name of the SQLite database file inside the data directory.
const DBFileName = "proxback.db"

// Store owns the database handle and the at-rest encryption key.
type Store struct {
	db  *sql.DB
	key []byte
}

// Open opens (creating if necessary) the ProxBack database inside dataDir and
// applies the schema.
func Open(ctx context.Context, dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir %q: %w", dataDir, err)
	}
	key, err := loadOrCreateKey(filepath.Join(dataDir, KeyFileName))
	if err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.ToSlash(filepath.Join(dataDir, DBFileName)) +
		"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Serialise access: SQLite writers are exclusive anyway and this avoids
	// spurious "database is locked" errors under concurrent job runs.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	s := &Store{db: db, key: key}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// DB exposes the raw handle (used by tests).
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

const schemaSQL = `
CREATE TABLE IF NOT EXISTS users (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	username      TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	created_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
	token      TEXT PRIMARY KEY,
	user_id    INTEGER NOT NULL,
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS pve_hosts (
	id               TEXT PRIMARY KEY,
	name             TEXT NOT NULL,
	base_url         TEXT NOT NULL,
	token_id         TEXT NOT NULL,
	token_secret_enc BLOB NOT NULL,
	insecure_tls     INTEGER NOT NULL DEFAULT 0,
	status           TEXT NOT NULL DEFAULT 'unknown',
	last_seen        TEXT,
	created_at       TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS vms_cache (
	host_id    TEXT NOT NULL,
	vmid       INTEGER NOT NULL,
	name       TEXT NOT NULL,
	node       TEXT NOT NULL,
	status     TEXT NOT NULL,
	maxdisk    INTEGER NOT NULL DEFAULT 0,
	maxmem     INTEGER NOT NULL DEFAULT 0,
	uptime     INTEGER NOT NULL DEFAULT 0,
	tags       TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL,
	PRIMARY KEY (host_id, vmid)
);

CREATE TABLE IF NOT EXISTS s3_targets (
	id             TEXT PRIMARY KEY,
	name           TEXT NOT NULL,
	endpoint       TEXT NOT NULL,
	region         TEXT NOT NULL,
	bucket         TEXT NOT NULL,
	access_key     TEXT NOT NULL,
	secret_key_enc BLOB NOT NULL,
	path_style     INTEGER NOT NULL DEFAULT 1,
	status         TEXT NOT NULL DEFAULT 'unknown',
	created_at     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS agents (
	id            TEXT PRIMARY KEY,
	hostname      TEXT NOT NULL,
	os            TEXT NOT NULL,
	arch          TEXT NOT NULL,
	version       TEXT NOT NULL,
	api_key_hash  TEXT NOT NULL,
	last_seen     TEXT,
	registered_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_agents_key ON agents(api_key_hash);

CREATE TABLE IF NOT EXISTS enroll_tokens (
	token      TEXT PRIMARY KEY,
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	used_at    TEXT,
	purpose    TEXT NOT NULL DEFAULT 'agent'
);

CREATE TABLE IF NOT EXISTS helpers (
	id                TEXT PRIMARY KEY,
	node              TEXT NOT NULL,
	address           TEXT NOT NULL,
	port              INTEGER NOT NULL DEFAULT 8007,
	version           TEXT NOT NULL,
	access_secret_enc BLOB NOT NULL,
	api_key_hash      TEXT NOT NULL,
	last_seen         TEXT,
	registered_at     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_helpers_key ON helpers(api_key_hash);
CREATE INDEX IF NOT EXISTS idx_helpers_node ON helpers(node);

CREATE TABLE IF NOT EXISTS jobs (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	kind       TEXT NOT NULL,
	target_id  TEXT NOT NULL,
	schedule   TEXT NOT NULL DEFAULT 'manual',
	retention  INTEGER NOT NULL DEFAULT 7,
	enabled    INTEGER NOT NULL DEFAULT 1,
	sources    TEXT NOT NULL DEFAULT '[]',
	tag_filter TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS job_runs (
	id              TEXT PRIMARY KEY,
	job_id          TEXT NOT NULL DEFAULT '',
	job_name        TEXT NOT NULL,
	kind            TEXT NOT NULL DEFAULT 'backup',
	status          TEXT NOT NULL,
	started_at      TEXT NOT NULL,
	finished_at     TEXT,
	bytes_processed INTEGER NOT NULL DEFAULT 0,
	bytes_uploaded  INTEGER NOT NULL DEFAULT 0,
	dedup_ratio     REAL NOT NULL DEFAULT 0,
	error           TEXT,
	progress_pct    REAL NOT NULL DEFAULT 0,
	current_step    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_runs_job ON job_runs(job_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_runs_started ON job_runs(started_at DESC);

CREATE TABLE IF NOT EXISTS run_log (
	id     INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id TEXT NOT NULL,
	ts     TEXT NOT NULL,
	line   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_run_log_run ON run_log(run_id, id);

CREATE TABLE IF NOT EXISTS backups (
	id             TEXT PRIMARY KEY,
	job_id         TEXT NOT NULL DEFAULT '',
	run_id         TEXT NOT NULL DEFAULT '',
	source_kind    TEXT NOT NULL,
	source_id      TEXT NOT NULL,
	source_name    TEXT NOT NULL,
	target_id      TEXT NOT NULL,
	created_at     TEXT NOT NULL,
	size_bytes     INTEGER NOT NULL DEFAULT 0,
	uploaded_bytes INTEGER NOT NULL DEFAULT 0,
	kind           TEXT NOT NULL DEFAULT 'full',
	parent_id      TEXT,
	disks          TEXT NOT NULL DEFAULT '[]'
);
CREATE INDEX IF NOT EXISTS idx_backups_source ON backups(source_kind, source_id, target_id, created_at DESC);

CREATE TABLE IF NOT EXISTS chunk_index (
	target_id TEXT NOT NULL,
	sha256    TEXT NOT NULL,
	size      INTEGER NOT NULL,
	PRIMARY KEY (target_id, sha256)
);

CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

// addedColumns lists columns introduced after a table's original definition.
// The schema above uses CREATE TABLE IF NOT EXISTS, so databases created by an
// earlier ProxBack release keep their original column set and are upgraded in
// place by ALTER TABLE. Every entry must be additive and carry a default so
// existing rows stay valid; never remove or reorder entries.
var addedColumns = []struct{ table, column, definition string }{
	{"vms_cache", "tags", "TEXT NOT NULL DEFAULT ''"},
	{"jobs", "tag_filter", "TEXT NOT NULL DEFAULT ''"},
	// Enrollment tokens are shared by agents and node helpers; tokens written
	// before helpers existed are agent tokens.
	{"enroll_tokens", "purpose", "TEXT NOT NULL DEFAULT 'agent'"},
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	for _, c := range addedColumns {
		if err := s.addColumn(ctx, c.table, c.column, c.definition); err != nil {
			return err
		}
	}
	return nil
}

// addColumn applies an additive schema upgrade, treating "column already
// exists" as success so the migration is idempotent.
func (s *Store) addColumn(ctx context.Context, table, column, definition string) error {
	stmt := fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, definition)
	if _, err := s.db.ExecContext(ctx, stmt); err != nil {
		if isDuplicateColumn(err) {
			return nil
		}
		return fmt.Errorf("add column %s.%s: %w", table, column, err)
	}
	return nil
}

// isDuplicateColumn reports whether err is SQLite's complaint about adding a
// column that is already present.
func isDuplicateColumn(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate column name")
}

// ---------------------------------------------------------------- encryption

func loadOrCreateKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(b) != 32 {
			return nil, fmt.Errorf("key file %q: expected 32 bytes, got %d", path, len(b))
		}
		return b, nil
	case errors.Is(err, os.ErrNotExist):
		key := make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, fmt.Errorf("generate key: %w", err)
		}
		if err := os.WriteFile(path, key, 0o600); err != nil {
			return nil, fmt.Errorf("write key file %q: %w", path, err)
		}
		return key, nil
	default:
		return nil, fmt.Errorf("read key file %q: %w", path, err)
	}
}

func (s *Store) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, fmt.Errorf("aes cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("aes-gcm: %w", err)
	}
	return aead, nil
}

// Encrypt seals plaintext with AES-GCM; the nonce is prefixed to the ciphertext.
func (s *Store) Encrypt(plain string) ([]byte, error) {
	aead, err := s.gcm()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	return aead.Seal(nonce, nonce, []byte(plain), nil), nil
}

// Decrypt opens a value produced by Encrypt.
func (s *Store) Decrypt(sealed []byte) (string, error) {
	aead, err := s.gcm()
	if err != nil {
		return "", err
	}
	if len(sealed) < aead.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	nonce, ct := sealed[:aead.NonceSize()], sealed[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plain), nil
}

// ---------------------------------------------------------------- time helpers

// Now returns the current time normalised the way the store persists it.
func Now() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func fmtTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return fmtTime(*t)
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func nullTime(ns sql.NullString) *time.Time {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	t := parseTime(ns.String)
	return &t
}

func nullStr(ns sql.NullString) string {
	if !ns.Valid {
		return ""
	}
	return ns.String
}

func strOrNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}
