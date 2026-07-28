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
	"strconv"
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
	kind           TEXT NOT NULL DEFAULT 's3',
	path           TEXT NOT NULL DEFAULT '',
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
	purpose    TEXT NOT NULL DEFAULT 'agent',
	host_id    TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS helpers (
	id                TEXT PRIMARY KEY,
	host_id           TEXT NOT NULL DEFAULT '',
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
	current_step    TEXT NOT NULL DEFAULT '',
	restore_meta    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_runs_job ON job_runs(job_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_runs_started ON job_runs(started_at DESC);

CREATE TABLE IF NOT EXISTS run_sources (
	run_id          TEXT NOT NULL,
	seq             INTEGER NOT NULL,
	name            TEXT NOT NULL,
	kind            TEXT NOT NULL,
	source_id       TEXT NOT NULL DEFAULT '',
	host_id         TEXT NOT NULL DEFAULT '',
	host_name       TEXT NOT NULL DEFAULT '',
	node            TEXT NOT NULL DEFAULT '',
	status          TEXT NOT NULL DEFAULT 'pending',
	size_bytes      INTEGER NOT NULL DEFAULT 0,
	bytes_processed INTEGER NOT NULL DEFAULT 0,
	bytes_uploaded  INTEGER NOT NULL DEFAULT 0,
	started_at      TEXT,
	finished_at     TEXT,
	error           TEXT,
	PRIMARY KEY (run_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_run_sources_run ON run_sources(run_id);

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
	host_id        TEXT NOT NULL DEFAULT '',
	host_name      TEXT NOT NULL DEFAULT '',
	target_id      TEXT NOT NULL,
	created_at     TEXT NOT NULL,
	size_bytes     INTEGER NOT NULL DEFAULT 0,
	uploaded_bytes INTEGER NOT NULL DEFAULT 0,
	kind           TEXT NOT NULL DEFAULT 'full',
	parent_id      TEXT,
	disks          TEXT NOT NULL DEFAULT '[]',
	last_verified_at   TEXT,
	last_verify_result TEXT NOT NULL DEFAULT '',
	verified_bytes     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_backups_source ON backups(source_kind, source_id, target_id, created_at DESC);

CREATE TABLE IF NOT EXISTS chunk_index (
	target_id TEXT NOT NULL,
	sha256    TEXT NOT NULL,
	size      INTEGER NOT NULL,
	added_at  TEXT NOT NULL DEFAULT '',
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
	// When a chunk was uploaded. Orphan collection uses it to spare recent
	// uploads, which is what makes an interrupted backup resumable.
	{"chunk_index", "added_at", "TEXT NOT NULL DEFAULT ''"},
	// A helper belongs to a Proxmox host: two clusters can each contain a node
	// called "pve1". Registrations written before this column existed migrate to
	// the empty string and are reported as "unassigned" — never used for routing.
	{"helpers", "host_id", "TEXT NOT NULL DEFAULT ''"},
	// A helper enrollment token is minted for one host, so the helper inherits
	// its cluster identity at registration.
	{"enroll_tokens", "host_id", "TEXT NOT NULL DEFAULT ''"},
	// Restore points carry the cluster their workload lived in. Existing rows are
	// backfilled from the "<hostId>_<vmid>" source id, which already encodes it.
	{"backups", "host_id", "TEXT NOT NULL DEFAULT ''"},
	{"backups", "host_name", "TEXT NOT NULL DEFAULT ''"},
	// Verification evidence attached to the restore point rather than only to
	// run history.
	{"backups", "last_verified_at", "TEXT"},
	{"backups", "last_verify_result", "TEXT NOT NULL DEFAULT ''"},
	{"backups", "verified_bytes", "INTEGER NOT NULL DEFAULT 0"},
	// Which workload a run source row backed up, and where it lived.
	{"run_sources", "source_id", "TEXT NOT NULL DEFAULT ''"},
	{"run_sources", "host_id", "TEXT NOT NULL DEFAULT ''"},
	{"run_sources", "host_name", "TEXT NOT NULL DEFAULT ''"},
	// The structured destination of a restore run (mode, host, node, vmid,
	// storage) as JSON, so run history can show where a VM went.
	{"job_runs", "restore_meta", "TEXT NOT NULL DEFAULT ''"},
	// A job's GFS retention as JSON. The original integer column stays as the
	// keep-last mirror; rows written before this column existed hold the empty
	// string and are migrated into the object form on open.
	{"jobs", "retention_policy", "TEXT NOT NULL DEFAULT ''"},
	// A job's optional protection policy as JSON. Empty — which is what every
	// existing row holds — means the defaults, i.e. exactly the behaviour a job
	// had before policies existed.
	{"jobs", "policy", "TEXT NOT NULL DEFAULT ''"},
	// A user's role. The default is deliberately the empty string rather than a
	// real role: migrateUserRoles is the only thing that grants admin, and it
	// only ever touches rows that predate the column, so a value this code did
	// not write can never be mistaken for a privileged one.
	{"users", "role", "TEXT NOT NULL DEFAULT ''"},
	// When a user last signed in. Never set for a user who has not.
	{"users", "last_login_at", "TEXT"},
	// A backup target is either object storage or a mounted path. Every row
	// written before this column existed is object storage, which is exactly what
	// the default says, so an upgraded installation keeps working untouched.
	{"s3_targets", "kind", "TEXT NOT NULL DEFAULT 's3'"},
	// The base path of a filesystem target; empty for an S3 target.
	{"s3_targets", "path", "TEXT NOT NULL DEFAULT ''"},
}

// helperIdentitySQL enforces the (host_id, node) identity of a node helper. It
// runs after the column migration above, because a database written by an
// earlier release only grows host_id at that point. Unassigned rows all carry
// host_id = ”, and the pre-migration code already kept at most one row per
// node, so no existing database can violate it.
const helperIdentitySQL = `
CREATE UNIQUE INDEX IF NOT EXISTS idx_helpers_host_node ON helpers(host_id, node);
`

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	for _, c := range addedColumns {
		if err := s.addColumn(ctx, c.table, c.column, c.definition); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, helperIdentitySQL); err != nil {
		return fmt.Errorf("apply helper identity index: %w", err)
	}
	if err := s.backfillChunkAddedAt(ctx); err != nil {
		return err
	}
	if err := s.backfillBackupHostIdentity(ctx); err != nil {
		return err
	}
	if err := s.migrateJobSchedules(ctx); err != nil {
		return err
	}
	if err := s.migrateJobRetention(ctx); err != nil {
		return err
	}
	if err := s.migrateUserRoles(ctx); err != nil {
		return err
	}
	if err := s.migrateAudit(ctx); err != nil {
		return err
	}
	return s.normalizeTimestamps(ctx)
}

// backfillBackupHostIdentity gives restore points written before they carried a
// cluster identity the host they belong to. A VM restore point's source id is
// "<hostId>_<vmid>", so the host is already encoded there and nothing has to be
// guessed; the display name comes from the pve_hosts row when it still exists.
// Agent restore points have no host and are left alone. The pass is idempotent:
// it only touches rows whose host_id is still empty.
func (s *Store) backfillBackupHostIdentity(ctx context.Context) error {
	names := map[string]string{}
	rows, err := s.db.QueryContext(ctx, `SELECT id, name FROM pve_hosts`)
	if err != nil {
		return fmt.Errorf("backfill backup host identity: %w", err)
	}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return fmt.Errorf("backfill backup host identity: %w", err)
		}
		names[id] = name
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("backfill backup host identity: %w", err)
	}

	type fix struct{ id, hostID, hostName string }
	// The whole set is collected before anything is written: the pool is limited
	// to a single connection, so updating while a query is still open deadlocks.
	var fixes []fix
	rows, err = s.db.QueryContext(ctx,
		`SELECT id, source_id FROM backups WHERE source_kind = ? AND host_id = ''`, SourceVM)
	if err != nil {
		return fmt.Errorf("backfill backup host identity: %w", err)
	}
	for rows.Next() {
		var id, sourceID string
		if err := rows.Scan(&id, &sourceID); err != nil {
			rows.Close()
			return fmt.Errorf("backfill backup host identity: %w", err)
		}
		hostID, ok := HostIDFromSourceID(sourceID)
		if !ok {
			continue
		}
		fixes = append(fixes, fix{id: id, hostID: hostID, hostName: names[hostID]})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("backfill backup host identity: %w", err)
	}
	for _, f := range fixes {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE backups SET host_id = ?, host_name = ? WHERE id = ?`,
			f.hostID, f.hostName, f.id); err != nil {
			return fmt.Errorf("backfill backup host identity: %w", err)
		}
	}
	return nil
}

// HostIDFromSourceID recovers the Proxmox host from a VM source id of the form
// "<hostId>_<vmid>". The second result is false when the id does not have that
// shape (an agent source id, for instance).
func HostIDFromSourceID(sourceID string) (string, bool) {
	i := strings.LastIndex(sourceID, "_")
	if i <= 0 || i == len(sourceID)-1 {
		return "", false
	}
	if _, err := strconv.Atoi(sourceID[i+1:]); err != nil {
		return "", false
	}
	return sourceID[:i], true
}

// migrateJobSchedules converts jobs written before v0.4.0, whose schedule column
// held "manual" or a bare cron expression, into the structured schedule object.
// A cron a preset can express becomes that preset; anything else becomes an
// advanced schedule with the expression preserved, so no job's timing changes.
// Rows already holding the JSON object are left alone, which makes the pass
// idempotent — it runs on every open.
func (s *Store) migrateJobSchedules(ctx context.Context) error {
	type conversion struct{ id, schedule string }
	// The whole set is collected before anything is written: the pool is limited
	// to a single connection, so updating while a query is still open deadlocks.
	var pending []conversion
	rows, err := s.db.QueryContext(ctx, `SELECT id, schedule FROM jobs`)
	if err != nil {
		return fmt.Errorf("migrate job schedules: %w", err)
	}
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return fmt.Errorf("migrate job schedules: %w", err)
		}
		if strings.HasPrefix(strings.TrimSpace(raw), "{") {
			continue
		}
		encoded, err := encodeSchedule(ParseLegacySchedule(raw))
		if err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, conversion{id: id, schedule: encoded})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("migrate job schedules: %w", err)
	}
	for _, c := range pending {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE jobs SET schedule = ? WHERE id = ?`, c.schedule, c.id); err != nil {
			return fmt.Errorf("migrate job schedules: %w", err)
		}
	}
	return nil
}

// migrateJobRetention converts jobs written before v0.5.0, whose retention was
// a bare "keep the last N" integer, into the GFS object. The conversion is
// exact — {"keepLast":N} prunes precisely what N pruned — so no job's retention
// changes across the upgrade, and the integer column is left in place as the
// keep-last mirror. Rows that already hold an object are skipped, which makes
// the pass idempotent: it runs on every open.
func (s *Store) migrateJobRetention(ctx context.Context) error {
	type conversion struct {
		id       string
		policy   string
		keepLast int
	}
	// The whole set is collected before anything is written: the pool is limited
	// to a single connection, so updating while a query is still open deadlocks.
	var pending []conversion
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, retention, retention_policy FROM jobs`)
	if err != nil {
		return fmt.Errorf("migrate job retention: %w", err)
	}
	for rows.Next() {
		var id, object string
		var legacy int
		if err := rows.Scan(&id, &legacy, &object); err != nil {
			rows.Close()
			return fmt.Errorf("migrate job retention: %w", err)
		}
		if strings.HasPrefix(strings.TrimSpace(object), "{") {
			continue
		}
		encoded, err := encodeRetention(KeepLast(legacy))
		if err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, conversion{id: id, policy: encoded, keepLast: legacy})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("migrate job retention: %w", err)
	}
	for _, c := range pending {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE jobs SET retention_policy = ?, retention = ? WHERE id = ?`,
			c.policy, c.keepLast, c.id); err != nil {
			return fmt.Errorf("migrate job retention: %w", err)
		}
	}
	return nil
}

// sortedTimeColumns are the TEXT timestamp columns SQLite orders or compares as
// strings. Rows written by earlier releases used a variable width fraction, so
// they are rewritten in the canonical layout on open; without that, two rows
// written in the same second can still compare in the wrong order.
var sortedTimeColumns = []struct{ table, column string }{
	{"backups", "created_at"},
	{"job_runs", "started_at"},
	{"job_runs", "finished_at"},
	{"helpers", "registered_at"},
	{"enroll_tokens", "expires_at"},
	{"sessions", "expires_at"},
}

func (s *Store) normalizeTimestamps(ctx context.Context) error {
	for _, c := range sortedTimeColumns {
		type fix struct {
			rowid int64
			value string
		}
		// The whole set is collected before anything is written: the pool is
		// limited to a single connection, so updating while a query is still open
		// would deadlock.
		var fixes []fix
		rows, err := s.db.QueryContext(ctx, fmt.Sprintf(
			`SELECT rowid, %s FROM %s WHERE %s IS NOT NULL AND %s <> ''`, c.column, c.table, c.column, c.column))
		if err != nil {
			return fmt.Errorf("normalize %s.%s: %w", c.table, c.column, err)
		}
		for rows.Next() {
			var id int64
			var raw string
			if err := rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return fmt.Errorf("normalize %s.%s: %w", c.table, c.column, err)
			}
			t := parseTime(raw)
			if t.IsZero() {
				continue // not a timestamp we wrote; leave it alone
			}
			if canonical := fmtTime(t); canonical != raw {
				fixes = append(fixes, fix{rowid: id, value: canonical})
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("normalize %s.%s: %w", c.table, c.column, err)
		}
		for _, f := range fixes {
			if _, err := s.db.ExecContext(ctx, fmt.Sprintf(
				`UPDATE %s SET %s = ? WHERE rowid = ?`, c.table, c.column), f.value, f.rowid); err != nil {
				return fmt.Errorf("normalize %s.%s: %w", c.table, c.column, err)
			}
		}
	}
	return nil
}

// backfillChunkAddedAt stamps chunk rows written before the column existed with
// the time of this migration. That is deliberately conservative: those chunks are
// treated as freshly uploaded, so they enjoy one grace window of protection from
// orphan collection — far cheaper than deleting a chunk that is still wanted.
func (s *Store) backfillChunkAddedAt(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE chunk_index SET added_at = ? WHERE added_at = '' OR added_at IS NULL`,
		fmtTime(Now())); err != nil {
		return fmt.Errorf("backfill chunk_index.added_at: %w", err)
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

// timeLayout is RFC3339 with a fixed nine digit fraction. The fixed width is the
// whole point: these columns are TEXT and SQLite orders and compares them as
// strings, while time.RFC3339Nano drops trailing zeros. With that layout
// "…:00.9Z" sorts after "…:00.85Z", and a restore point created exactly on a
// second ("…:00Z") sorts as the newest one there is — which silently breaks the
// backup chain's parent, "latest restore point" lookups and retention's idea of
// which points to prune.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func fmtTime(t time.Time) string { return t.UTC().Format(timeLayout) }

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
