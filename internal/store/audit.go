package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The audit trail is append-only: rows are written, read and — once the table
// reaches AuditRetention entries — trimmed from the oldest end. Nothing updates
// a row, because an audit entry that can be edited is not evidence.
//
// Secrets are never written here. Callers pass object names and short factual
// details; passwords, API keys, tokens and secret access keys must not appear in
// any column, and TestAuditNeverStoresSecrets in the API layer plants a secret
// through the real endpoints and scans every written row for it.

// AuditRetention is how many entries the trail keeps. The oldest are trimmed on
// write. It is a variable so tests can exercise trimming without writing fifty
// thousand rows.
var AuditRetention = 50000

// Audit results.
const (
	// AuditOK records an action that happened.
	AuditOK = "ok"
	// AuditDenied records an action that was refused because the actor's role
	// does not carry it. An operator probing admin endpoints is exactly what an
	// audit trail is for.
	AuditDenied = "denied"
	// AuditError records an action that was attempted and failed.
	AuditError = "error"
)

// Audit actions. They are "<object>.<verb>" so the console can group them and
// the ?action= filter can select one kind of event.
const (
	AuditSignIn       = "session.signin"
	AuditSignInFailed = "session.signin_failed"
	AuditSignOut      = "session.signout"

	AuditUserCreate = "user.create"
	AuditUserModify = "user.modify"
	AuditUserDelete = "user.delete"

	AuditHostCreate = "host.create"
	AuditHostDelete = "host.delete"

	AuditTargetCreate = "target.create"
	AuditTargetDelete = "target.delete"

	AuditHelperCreate = "helper.create"
	AuditHelperModify = "helper.modify"
	AuditHelperDelete = "helper.delete"

	AuditAgentCreate = "agent.create"
	AuditAgentDelete = "agent.delete"

	AuditJobCreate = "job.create"
	AuditJobModify = "job.modify"
	AuditJobDelete = "job.delete"

	AuditRunStart  = "run.start"
	AuditRunCancel = "run.cancel"
	AuditRunRetry  = "run.retry"
	AuditRunDelete = "run.delete"

	AuditRestoreStart = "restore.start"
	AuditVerifyStart  = "backup.verify"
	AuditBackupDelete = "backup.delete"

	AuditSettingsModify = "settings.modify"
	AuditUpdateApply    = "update.apply"

	// AuditAccessDenied records a request refused by the role middleware. The
	// object names the route that was probed, because at that point the request
	// never reached a handler and there is no object to name.
	AuditAccessDenied = "access.denied"
)

// AuditEntry is one recorded event.
type AuditEntry struct {
	ID int64     `json:"id"`
	At time.Time `json:"at"`
	// Actor is the acting user's name, or the attempted username for a failed
	// sign-in. Empty for an unauthenticated event with no name to record.
	Actor string `json:"actor"`
	// ActorID is the acting user's id, 0 when there is no authenticated user.
	ActorID    int64  `json:"actorId"`
	Action     string `json:"action"`
	ObjectKind string `json:"objectKind"`
	ObjectID   string `json:"objectId"`
	ObjectName string `json:"objectName"`
	// Result is AuditOK, AuditDenied or AuditError.
	Result string `json:"result"`
	// SourceIP is the client address the request came from, without its port.
	SourceIP string `json:"sourceIp"`
	// Detail is a short factual note — never a secret value.
	Detail string `json:"detail"`
}

// AuditFilter selects entries for AuditEntries. The zero value selects the most
// recent DefaultAuditLimit entries.
type AuditFilter struct {
	Limit  int
	Action string
	Actor  string
}

// Audit limits.
const (
	// DefaultAuditLimit is how many entries GET /api/audit returns when the
	// request does not say.
	DefaultAuditLimit = 100
	// MaxAuditLimit caps one page, so a client cannot ask for the whole trail in
	// one response.
	MaxAuditLimit = 1000
)

const auditSchemaSQL = `
CREATE TABLE IF NOT EXISTS audit_log (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	at          TEXT NOT NULL,
	actor       TEXT NOT NULL DEFAULT '',
	actor_id    INTEGER NOT NULL DEFAULT 0,
	action      TEXT NOT NULL,
	object_kind TEXT NOT NULL DEFAULT '',
	object_id   TEXT NOT NULL DEFAULT '',
	object_name TEXT NOT NULL DEFAULT '',
	result      TEXT NOT NULL DEFAULT 'ok',
	source_ip   TEXT NOT NULL DEFAULT '',
	detail      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_audit_action ON audit_log(action, id DESC);
CREATE INDEX IF NOT EXISTS idx_audit_actor ON audit_log(actor, id DESC);
`

// migrateAudit creates the audit trail. It is here rather than in the main
// schema because the trail owns its own table and nothing else reads it.
func (s *Store) migrateAudit(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, auditSchemaSQL); err != nil {
		return fmt.Errorf("apply audit schema: %w", err)
	}
	return nil
}

const auditColumns = `id, at, actor, actor_id, action, object_kind, object_id, object_name, result, source_ip, detail`

// AppendAudit writes one entry and trims the trail to the newest AuditRetention
// entries. It fills in the timestamp and defaults the result to AuditOK, so a
// call site only has to say what happened.
//
// Callers must treat a failure as non-fatal: an audit write must never fail the
// operation it describes. The API layer logs it and carries on.
func (s *Store) AppendAudit(ctx context.Context, e AuditEntry) (*AuditEntry, error) {
	if e.At.IsZero() {
		e.At = Now()
	}
	if e.Result == "" {
		e.Result = AuditOK
	}
	if e.Action == "" {
		return nil, fmt.Errorf("append audit: action is required")
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log
			(at, actor, actor_id, action, object_kind, object_id, object_name, result, source_ip, detail)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		fmtTime(e.At), e.Actor, e.ActorID, e.Action,
		e.ObjectKind, e.ObjectID, e.ObjectName, e.Result, e.SourceIP, e.Detail)
	if err != nil {
		return nil, fmt.Errorf("append audit %q: %w", e.Action, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("append audit %q: %w", e.Action, err)
	}
	e.ID = id
	// Trim by id rather than by counting rows: ids are allocated in order and
	// nothing but this trim ever deletes from the table, so they stay contiguous
	// and "everything at or below id-N" is exactly the excess. That keeps the
	// write path to one indexed range delete with no extra query.
	if AuditRetention > 0 {
		if cutoff := id - int64(AuditRetention); cutoff > 0 {
			if _, err := s.db.ExecContext(ctx,
				`DELETE FROM audit_log WHERE id <= ?`, cutoff); err != nil {
				return &e, fmt.Errorf("trim audit log: %w", err)
			}
		}
	}
	return &e, nil
}

// AuditEntries returns matching entries, newest first.
func (s *Store) AuditEntries(ctx context.Context, f AuditFilter) ([]*AuditEntry, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultAuditLimit
	}
	if limit > MaxAuditLimit {
		limit = MaxAuditLimit
	}
	query := `SELECT ` + auditColumns + ` FROM audit_log`
	var where []string
	var args []any
	if action := strings.TrimSpace(f.Action); action != "" {
		where = append(where, `action = ?`)
		args = append(args, action)
	}
	if actor := strings.TrimSpace(f.Actor); actor != "" {
		where = append(where, `actor = ?`)
		args = append(args, actor)
	}
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit log: %w", err)
	}
	defer rows.Close()
	var out []*AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at string
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.ActorID, &e.Action,
			&e.ObjectKind, &e.ObjectID, &e.ObjectName, &e.Result, &e.SourceIP, &e.Detail); err != nil {
			return nil, fmt.Errorf("scan audit entry: %w", err)
		}
		e.At = parseTime(at)
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list audit log: %w", err)
	}
	return out, nil
}

// CountAuditEntries returns how many entries the trail holds.
func (s *Store) CountAuditEntries(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count audit log: %w", err)
	}
	return n, nil
}
