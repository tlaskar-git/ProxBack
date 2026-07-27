package store_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, for building the legacy fixture

	"proxback/internal/store"
)

func open(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, dir
}

func TestEncryptionRoundTrip(t *testing.T) {
	st, dir := open(t)

	key, err := os.ReadFile(filepath.Join(dir, store.KeyFileName))
	if err != nil {
		t.Fatalf("key file: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("key length = %d, want 32", len(key))
	}

	const secret = "s3cr3t-value-with-üñíçø∂é"
	sealed, err := st.Encrypt(secret)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if strings.Contains(string(sealed), secret) {
		t.Fatal("ciphertext contains the plaintext")
	}
	got, err := st.Decrypt(sealed)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != secret {
		t.Fatalf("decrypt = %q, want %q", got, secret)
	}

	// Nonces must differ between seals of the same plaintext.
	other, err := st.Encrypt(secret)
	if err != nil {
		t.Fatalf("encrypt again: %v", err)
	}
	if string(other) == string(sealed) {
		t.Fatal("two seals of the same plaintext are identical")
	}

	// Tampering must be detected.
	sealed[len(sealed)-1] ^= 0xFF
	if _, err := st.Decrypt(sealed); err == nil {
		t.Fatal("tampered ciphertext decrypted without error")
	}
}

func TestSecretsEncryptedAtRest(t *testing.T) {
	ctx := context.Background()
	st, dir := open(t)

	if _, err := st.CreatePVEHost(ctx, &store.PVEHost{
		Name: "pve", BaseURL: "https://pve:8006",
		TokenID: "root@pam!pb", TokenSecret: "pve-token-secret-abc",
	}); err != nil {
		t.Fatalf("create host: %v", err)
	}
	if _, err := st.CreateS3Target(ctx, &store.S3Target{
		Name: "b2", Endpoint: "https://s3.example", Region: "us-east-1", Bucket: "bk",
		AccessKey: "AK", SecretKey: "s3-secret-key-xyz", PathStyle: true,
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}

	hosts, err := st.ListPVEHosts(ctx)
	if err != nil || len(hosts) != 1 {
		t.Fatalf("list hosts: %v (%d hosts)", err, len(hosts))
	}
	if hosts[0].TokenSecret != "pve-token-secret-abc" {
		t.Fatalf("token secret = %q", hosts[0].TokenSecret)
	}
	targets, err := st.ListS3Targets(ctx)
	if err != nil || len(targets) != 1 {
		t.Fatalf("list targets: %v (%d targets)", err, len(targets))
	}
	if targets[0].SecretKey != "s3-secret-key-xyz" {
		t.Fatalf("secret key = %q", targets[0].SecretKey)
	}

	// Close so the WAL is checkpointed, then scan the raw database file.
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, store.DBFileName))
	if err != nil {
		t.Fatalf("read db: %v", err)
	}
	for _, secret := range []string{"pve-token-secret-abc", "s3-secret-key-xyz"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("plaintext secret %q found in the database file", secret)
		}
	}
}

func TestChunkIndex(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)

	has, err := st.HasChunk(ctx, "t1", "abc")
	if err != nil || has {
		t.Fatalf("HasChunk on empty index = %v, %v", has, err)
	}
	if err := st.AddChunk(ctx, "t1", "abc", 1024); err != nil {
		t.Fatalf("AddChunk: %v", err)
	}
	// Idempotent.
	if err := st.AddChunk(ctx, "t1", "abc", 1024); err != nil {
		t.Fatalf("AddChunk twice: %v", err)
	}
	if has, err = st.HasChunk(ctx, "t1", "abc"); err != nil || !has {
		t.Fatalf("HasChunk after add = %v, %v", has, err)
	}
	if has, err = st.HasChunk(ctx, "t2", "abc"); err != nil || has {
		t.Fatalf("chunk index leaked across targets")
	}
	n, bytes, err := st.ChunkStats(ctx, "t1")
	if err != nil || n != 1 || bytes != 1024 {
		t.Fatalf("ChunkStats = %d, %d, %v", n, bytes, err)
	}
	if err := st.DeleteChunk(ctx, "t1", "abc"); err != nil {
		t.Fatalf("DeleteChunk: %v", err)
	}
	if has, err = st.HasChunk(ctx, "t1", "abc"); err != nil || has {
		t.Fatalf("HasChunk after delete = %v, %v", has, err)
	}
}

func TestJobSourcesAcceptObjectOrArray(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)

	var sources store.JobSources
	if err := sources.UnmarshalJSON([]byte(`{"agentId":"a1","paths":["/etc"]}`)); err != nil {
		t.Fatalf("unmarshal object: %v", err)
	}
	if len(sources) != 1 || sources[0].AgentID != "a1" || len(sources[0].Paths) != 1 {
		t.Fatalf("object form decoded as %+v", sources)
	}
	if err := sources.UnmarshalJSON([]byte(`[{"hostId":"h","vmid":100,"name":"web"}]`)); err != nil {
		t.Fatalf("unmarshal array: %v", err)
	}
	if len(sources) != 1 || sources[0].VMID != 100 {
		t.Fatalf("array form decoded as %+v", sources)
	}

	job, err := st.CreateJob(ctx, &store.Job{
		Name: "j", Kind: store.SourceVM, TargetID: "t", Retention: 3, Enabled: true,
		Sources: sources,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	loaded, err := st.JobByID(ctx, job.ID)
	if err != nil {
		t.Fatalf("load job: %v", err)
	}
	if len(loaded.Sources) != 1 || loaded.Sources[0].VMID != 100 {
		t.Fatalf("round tripped sources = %+v", loaded.Sources)
	}
}

func TestRunLifecycle(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)

	run, err := st.CreateRun(ctx, &store.JobRun{JobID: "j1", JobName: "nightly"})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if run.Status != store.RunRunning {
		t.Fatalf("status = %q", run.Status)
	}
	running, err := st.HasRunningRun(ctx, "j1")
	if err != nil || !running {
		t.Fatalf("HasRunningRun = %v, %v", running, err)
	}
	if err := st.UpdateRunProgress(ctx, run.ID, 100, 40, 25, "Backing up"); err != nil {
		t.Fatalf("progress: %v", err)
	}
	got, err := st.RunByID(ctx, run.ID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if got.BytesProcessed != 100 || got.BytesUploaded != 40 || got.ProgressPct != 25 || got.CurrentStep != "Backing up" {
		t.Fatalf("run after progress = %+v", got)
	}
	if err := st.FinishRun(ctx, run.ID, store.RunSuccess, 400, 40, 0.9, ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	got, err = st.RunByID(ctx, run.ID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if got.Status != store.RunSuccess || got.FinishedAt == nil || got.ProgressPct != 100 {
		t.Fatalf("finished run = %+v", got)
	}
	if running, _ = st.HasRunningRun(ctx, "j1"); running {
		t.Fatal("job still reported as running")
	}
	counts, err := st.RunCountsSince(ctx, time.Now().Add(-time.Hour))
	if err != nil || counts[store.RunSuccess] != 1 {
		t.Fatalf("RunCountsSince = %v, %v", counts, err)
	}
}

func TestHelperCRUD(t *testing.T) {
	ctx := context.Background()
	st, dir := open(t)

	if n, err := st.CountHelpers(ctx); err != nil || n != 0 {
		t.Fatalf("CountHelpers on a fresh install = %d (%v)", n, err)
	}
	if _, err := st.HelperByNode(ctx, "pve1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("HelperByNode on a fresh install = %v, want ErrNotFound", err)
	}

	seen := time.Now().UTC().Truncate(time.Millisecond)
	h, err := st.CreateHelper(ctx, &store.NodeHelper{
		Node: "pve1", Address: "10.0.0.11", Port: 8007, Version: "0.3.0",
		AccessSecret: "helper-access-secret-abc", APIKeyHash: "hash-1", LastSeen: &seen,
	})
	if err != nil {
		t.Fatalf("create helper: %v", err)
	}
	if h.ID == "" || h.RegisteredAt.IsZero() {
		t.Fatalf("created helper = %+v", h)
	}
	// A port left unset falls back to the well-known one.
	other, err := st.CreateHelper(ctx, &store.NodeHelper{
		Node: "pve2", Address: "10.0.0.12", Version: "0.3.0",
		AccessSecret: "helper-access-secret-def", APIKeyHash: "hash-2",
	})
	if err != nil {
		t.Fatalf("create second helper: %v", err)
	}
	if other.Port != store.DefaultHelperPort {
		t.Fatalf("default port = %d, want %d", other.Port, store.DefaultHelperPort)
	}

	list, err := st.ListHelpers(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list helpers = %+v (%v)", list, err)
	}
	if list[0].Node != "pve1" || list[1].Node != "pve2" {
		t.Fatalf("helpers not ordered by node: %s, %s", list[0].Node, list[1].Node)
	}
	if list[0].AccessSecret != "helper-access-secret-abc" {
		t.Fatalf("access secret round trip = %q", list[0].AccessSecret)
	}

	byNode, err := st.HelperByNode(ctx, "pve2")
	if err != nil || byNode.ID != other.ID {
		t.Fatalf("HelperByNode = %+v (%v)", byNode, err)
	}
	byKey, err := st.HelperByKeyHash(ctx, "hash-1")
	if err != nil || byKey.ID != h.ID || byKey.AccessSecret != "helper-access-secret-abc" {
		t.Fatalf("HelperByKeyHash = %+v (%v)", byKey, err)
	}
	if _, err := st.HelperByKeyHash(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("HelperByKeyHash with an unknown key = %v, want ErrNotFound", err)
	}
	if byID, err := st.HelperByID(ctx, h.ID); err != nil || byID.Node != "pve1" {
		t.Fatalf("HelperByID = %+v (%v)", byID, err)
	}

	// A heartbeat moves lastSeen.
	beat := seen.Add(time.Minute)
	if err := st.TouchHelper(ctx, h.ID, beat); err != nil {
		t.Fatalf("touch helper: %v", err)
	}
	touched, err := st.HelperByID(ctx, h.ID)
	if err != nil {
		t.Fatalf("reload helper: %v", err)
	}
	if touched.LastSeen == nil || !touched.LastSeen.Equal(beat) {
		t.Fatalf("lastSeen = %v, want %v", touched.LastSeen, beat)
	}

	// Re-enrolling a node replaces its registration rather than duplicating it.
	replaced, err := st.CreateHelper(ctx, &store.NodeHelper{
		Node: "pve1", Address: "10.0.0.99", Port: 8107, Version: "0.4.0",
		AccessSecret: "helper-access-secret-new", APIKeyHash: "hash-3",
	})
	if err != nil {
		t.Fatalf("re-enroll helper: %v", err)
	}
	if n, err := st.CountHelpers(ctx); err != nil || n != 2 {
		t.Fatalf("CountHelpers after re-enrollment = %d (%v), want 2", n, err)
	}
	if _, err := st.HelperByID(ctx, h.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old registration for pve1 survived re-enrollment: %v", err)
	}
	if _, err := st.HelperByKeyHash(ctx, "hash-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old api key for pve1 still authenticates: %v", err)
	}
	current, err := st.HelperByNode(ctx, "pve1")
	if err != nil || current.ID != replaced.ID || current.Address != "10.0.0.99" || current.Port != 8107 {
		t.Fatalf("pve1 helper after re-enrollment = %+v (%v)", current, err)
	}

	if err := st.DeleteHelper(ctx, replaced.ID); err != nil {
		t.Fatalf("delete helper: %v", err)
	}
	if err := st.DeleteHelper(ctx, replaced.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleting a helper twice = %v, want ErrNotFound", err)
	}

	// The access secret is encrypted at rest like every other stored secret.
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, store.DBFileName))
	if err != nil {
		t.Fatalf("read db: %v", err)
	}
	if strings.Contains(string(raw), "helper-access-secret-def") {
		t.Fatal("plaintext helper access secret found in the database file")
	}
}

func TestEnrollTokenPurposesAreSeparate(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)

	agentTok, err := st.CreateEnrollToken(ctx, "tok-agent", store.EnrollPurposeAgent, time.Hour)
	if err != nil {
		t.Fatalf("create agent token: %v", err)
	}
	if agentTok.Purpose != store.EnrollPurposeAgent || agentTok.ExpiresAt.Before(agentTok.CreatedAt) {
		t.Fatalf("agent token = %+v", agentTok)
	}
	if _, err := st.CreateEnrollToken(ctx, "tok-helper", store.EnrollPurposeHelper, time.Hour); err != nil {
		t.Fatalf("create helper token: %v", err)
	}

	// A token minted for one flow must never work in the other.
	if err := st.ConsumeEnrollToken(ctx, "tok-agent", store.EnrollPurposeHelper); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("agent token consumed as a helper token: %v", err)
	}
	if err := st.ConsumeEnrollToken(ctx, "tok-helper", store.EnrollPurposeAgent); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("helper token consumed as an agent token: %v", err)
	}

	if err := st.ConsumeEnrollToken(ctx, "tok-helper", store.EnrollPurposeHelper); err != nil {
		t.Fatalf("consume helper token: %v", err)
	}
	// Single use.
	if err := st.ConsumeEnrollToken(ctx, "tok-helper", store.EnrollPurposeHelper); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("helper token consumed twice: %v", err)
	}
	loaded, err := st.EnrollTokenByValue(ctx, "tok-helper")
	if err != nil {
		t.Fatalf("load helper token: %v", err)
	}
	if loaded.Purpose != store.EnrollPurposeHelper || loaded.UsedAt == nil {
		t.Fatalf("consumed helper token = %+v", loaded)
	}

	// Expired tokens are refused.
	if _, err := st.CreateEnrollToken(ctx, "tok-stale", store.EnrollPurposeHelper, -time.Minute); err != nil {
		t.Fatalf("create expired token: %v", err)
	}
	if err := st.ConsumeEnrollToken(ctx, "tok-stale", store.EnrollPurposeHelper); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired token accepted: %v", err)
	}
}

// oldSchemaSQL is the pre-v0.3.0 definition of the tables that gained columns,
// exactly as shipped installations have them on disk.
const oldSchemaSQL = `
CREATE TABLE vms_cache (
	host_id    TEXT NOT NULL,
	vmid       INTEGER NOT NULL,
	name       TEXT NOT NULL,
	node       TEXT NOT NULL,
	status     TEXT NOT NULL,
	maxdisk    INTEGER NOT NULL DEFAULT 0,
	maxmem     INTEGER NOT NULL DEFAULT 0,
	uptime     INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (host_id, vmid)
);
CREATE TABLE jobs (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	kind       TEXT NOT NULL,
	target_id  TEXT NOT NULL,
	schedule   TEXT NOT NULL DEFAULT 'manual',
	retention  INTEGER NOT NULL DEFAULT 7,
	enabled    INTEGER NOT NULL DEFAULT 1,
	sources    TEXT NOT NULL DEFAULT '[]',
	created_at TEXT NOT NULL
);
CREATE TABLE enroll_tokens (
	token      TEXT PRIMARY KEY,
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	used_at    TEXT
);
`

// TestMigrationUpgradesOldDatabase proves an existing production database opens
// cleanly with the new code: the added columns appear and the rows written by
// the old schema survive untouched.
func TestMigrationUpgradesOldDatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// 1. Build a database with the old schema and populate it.
	dsn := "file:" + filepath.ToSlash(filepath.Join(dir, store.DBFileName))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open old db: %v", err)
	}
	if _, err := db.Exec(oldSchemaSQL); err != nil {
		t.Fatalf("apply old schema: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO vms_cache (host_id, vmid, name, node, status, maxdisk, maxmem, uptime, updated_at)
		 VALUES ('h1', 100, 'web-01', 'pve1', 'running', 1024, 2048, 3600, '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert legacy vm: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO jobs (id, name, kind, target_id, schedule, retention, enabled, sources, created_at)
		 VALUES ('j1', 'legacy-nightly', 'vm', 't1', '0 3 * * *', 5, 1,
		         '[{"hostId":"h1","vmid":100,"name":"web-01"}]', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert legacy job: %v", err)
	}
	// An enrollment token issued before helpers existed: it predates the purpose
	// column and must still enroll an agent afterwards.
	if _, err := db.Exec(
		`INSERT INTO enroll_tokens (token, created_at, expires_at, used_at)
		 VALUES ('legacy-token', '2026-01-01T00:00:00Z', '2099-01-01T00:00:00Z', NULL)`); err != nil {
		t.Fatalf("insert legacy enroll token: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close old db: %v", err)
	}

	// 2. Open it with the current code: the migration must run in place.
	st, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatalf("open upgraded store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	for table, column := range map[string]string{
		"vms_cache":     "tags",
		"jobs":          "tag_filter",
		"enroll_tokens": "purpose",
	} {
		if !hasColumn(t, st, table, column) {
			t.Fatalf("migration did not add %s.%s", table, column)
		}
	}
	// Tables introduced by a later release are created outright.
	if !hasColumn(t, st, "helpers", "access_secret_enc") {
		t.Fatal("migration did not create the helpers table")
	}

	// 3. Legacy data survived and reads back through the new accessors.
	vm, err := st.CachedVM(ctx, "h1", 100)
	if err != nil {
		t.Fatalf("load legacy vm: %v", err)
	}
	if vm.Name != "web-01" || vm.Node != "pve1" || vm.MaxDisk != 1024 || vm.Uptime != 3600 {
		t.Fatalf("legacy vm changed: %+v", vm)
	}
	if vm.Tags == nil || len(vm.Tags) != 0 {
		t.Fatalf("legacy vm tags = %#v, want an empty slice", vm.Tags)
	}
	job, err := st.JobByID(ctx, "j1")
	if err != nil {
		t.Fatalf("load legacy job: %v", err)
	}
	if job.Name != "legacy-nightly" || job.Schedule != "0 3 * * *" || job.Retention != 5 || !job.Enabled {
		t.Fatalf("legacy job changed: %+v", job)
	}
	if len(job.Sources) != 1 || job.Sources[0].VMID != 100 {
		t.Fatalf("legacy job sources = %+v", job.Sources)
	}
	if job.TagFilter != "" {
		t.Fatalf("legacy job tagFilter = %q, want empty", job.TagFilter)
	}

	// 4. The new columns are writable on the upgraded database.
	job.TagFilter = "prod"
	if err := st.UpdateJob(ctx, job); err != nil {
		t.Fatalf("update upgraded job: %v", err)
	}
	if reloaded, err := st.JobByID(ctx, "j1"); err != nil || reloaded.TagFilter != "prod" {
		t.Fatalf("tagFilter round trip = %+v (%v)", reloaded, err)
	}
	if err := st.ReplaceVMCache(ctx, "h1", []store.VM{
		{VMID: 100, Name: "web-01", Node: "pve1", Status: "running", Tags: []string{"Web", "prod"}},
	}); err != nil {
		t.Fatalf("replace vm cache: %v", err)
	}
	vms, err := st.ListCachedVMs(ctx)
	if err != nil || len(vms) != 1 {
		t.Fatalf("list cached vms = %+v (%v)", vms, err)
	}
	if strings.Join(vms[0].Tags, ",") != "prod,web" {
		t.Fatalf("stored tags = %v, want [prod web]", vms[0].Tags)
	}
	if !vms[0].HasTag("PROD") || vms[0].HasTag("dev") {
		t.Fatalf("HasTag is wrong for %v", vms[0].Tags)
	}
	// The legacy token gained the default purpose, so it still enrolls an agent
	// and still cannot enroll a helper.
	legacy, err := st.EnrollTokenByValue(ctx, "legacy-token")
	if err != nil {
		t.Fatalf("load legacy enroll token: %v", err)
	}
	if legacy.Purpose != store.EnrollPurposeAgent {
		t.Fatalf("legacy token purpose = %q, want %q", legacy.Purpose, store.EnrollPurposeAgent)
	}
	if err := st.ConsumeEnrollToken(ctx, "legacy-token", store.EnrollPurposeHelper); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("legacy agent token enrolled a helper: %v", err)
	}
	if err := st.ConsumeEnrollToken(ctx, "legacy-token", store.EnrollPurposeAgent); err != nil {
		t.Fatalf("consume legacy token: %v", err)
	}
	// And the new table is writable on the upgraded database.
	if _, err := st.CreateHelper(ctx, &store.NodeHelper{
		Node: "pve1", Address: "10.0.0.11", Version: "0.3.0",
		AccessSecret: "s", APIKeyHash: "h",
	}); err != nil {
		t.Fatalf("create helper on the upgraded database: %v", err)
	}
	if upgraded, err := st.HelperByNode(ctx, "pve1"); err != nil || upgraded.Port != store.DefaultHelperPort {
		t.Fatalf("helper on the upgraded database = %+v (%v)", upgraded, err)
	}

	// 5. Re-opening is idempotent (the ALTER TABLE must not error twice).
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	again, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatalf("reopen migrated store: %v", err)
	}
	defer again.Close()
	if _, err := again.JobByID(ctx, "j1"); err != nil {
		t.Fatalf("job missing after reopen: %v", err)
	}
}

func hasColumn(t *testing.T, st *store.Store, table, column string) bool {
	t.Helper()
	rows, err := st.DB().Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column name: %v", err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns: %v", err)
	}
	return false
}

func TestSettingsNotificationDefaults(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)

	s, err := st.Settings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if s.WebhookURL != "" || s.NotifyOn != store.NotifyOff {
		t.Fatalf("notification defaults = %+v", s)
	}
	if err := st.SaveSettings(ctx, store.Settings{
		ServerName: "lab", Concurrency: 2,
		WebhookURL: "https://hooks.example/proxback", NotifyOn: store.NotifyAll,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if s, err = st.Settings(ctx); err != nil || s.WebhookURL != "https://hooks.example/proxback" || s.NotifyOn != store.NotifyAll {
		t.Fatalf("saved notification settings = %+v (%v)", s, err)
	}
	// An unrecognised policy falls back to "off" rather than notifying wildly.
	if err := st.SaveSettings(ctx, store.Settings{ServerName: "lab", Concurrency: 2, NotifyOn: "sometimes"}); err != nil {
		t.Fatalf("save invalid: %v", err)
	}
	if s, err = st.Settings(ctx); err != nil || s.NotifyOn != store.NotifyOff {
		t.Fatalf("invalid notifyOn stored as %+v (%v)", s, err)
	}
	if store.ValidNotifyOn("sometimes") || !store.ValidNotifyOn(store.NotifyFailures) {
		t.Fatal("ValidNotifyOn is wrong")
	}
}

func TestSettingsDefaults(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)

	s, err := st.Settings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if s.ServerName != store.DefaultServerName || s.Concurrency != store.DefaultConcurrency {
		t.Fatalf("defaults = %+v", s)
	}
	if err := st.SaveSettings(ctx, store.Settings{ServerName: "lab", Concurrency: 5}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if s, err = st.Settings(ctx); err != nil || s.ServerName != "lab" || s.Concurrency != 5 {
		t.Fatalf("saved settings = %+v (%v)", s, err)
	}
}
