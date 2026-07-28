package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, for building the legacy fixture

	"proxback/internal/store"
)

// legacyTargetsSQL is the pre-v0.6.0 s3_targets table, exactly as shipped
// installations have it on disk: no kind, no path, because object storage was the
// only kind of target there was.
const legacyTargetsSQL = `
DROP TABLE s3_targets;
CREATE TABLE s3_targets (
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
`

func TestTargetKindsRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)

	// A creation that says nothing about kind is object storage, which is what
	// every client written before filesystem targets sends.
	objects, err := st.CreateS3Target(ctx, &store.S3Target{
		Name: "b2", Endpoint: "https://s3.eu-central-003.backblazeb2.com",
		Region: "eu-central-003", Bucket: "proxback", AccessKey: "k", SecretKey: "s", PathStyle: true,
	})
	if err != nil {
		t.Fatalf("create s3 target: %v", err)
	}
	if objects.Kind != store.TargetKindS3 {
		t.Fatalf("kind = %q, want %q", objects.Kind, store.TargetKindS3)
	}

	nas, err := st.CreateS3Target(ctx, &store.S3Target{
		Name: "a-nas", Kind: store.TargetKindFilesystem, Path: filepath.FromSlash("/mnt/nas/proxback"),
	})
	if err != nil {
		t.Fatalf("create filesystem target: %v", err)
	}

	loaded, err := st.S3TargetByID(ctx, nas.ID)
	if err != nil {
		t.Fatalf("load filesystem target: %v", err)
	}
	if !loaded.IsFilesystem() || loaded.Path != filepath.FromSlash("/mnt/nas/proxback") {
		t.Fatalf("filesystem target round trip = %+v", loaded)
	}
	if loaded.Bucket != "" || loaded.Endpoint != "" || loaded.AccessKey != "" || loaded.SecretKey != "" {
		t.Fatalf("filesystem target carries S3 fields: %+v", loaded)
	}

	loadedS3, err := st.S3TargetByID(ctx, objects.ID)
	if err != nil {
		t.Fatalf("load s3 target: %v", err)
	}
	if loadedS3.IsFilesystem() || loadedS3.Path != "" || loadedS3.SecretKey != "s" {
		t.Fatalf("s3 target round trip = %+v", loadedS3)
	}

	all, err := st.ListS3Targets(ctx)
	if err != nil {
		t.Fatalf("list targets: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("listed %d targets, want 2", len(all))
	}
	// Ordered by name: the filesystem target comes first, and both kinds appear in
	// the same list — nothing in the store is per-kind.
	if all[0].Kind != store.TargetKindFilesystem || all[1].Kind != store.TargetKindS3 {
		t.Fatalf("listed kinds = %q, %q", all[0].Kind, all[1].Kind)
	}

	// A hand-edited row with no kind reads as object storage rather than as an
	// unusable target.
	if _, err := st.DB().ExecContext(ctx, `UPDATE s3_targets SET kind = '' WHERE id = ?`, objects.ID); err != nil {
		t.Fatalf("blank the kind: %v", err)
	}
	blank, err := st.S3TargetByID(ctx, objects.ID)
	if err != nil {
		t.Fatalf("load blanked target: %v", err)
	}
	if blank.Kind != store.TargetKindS3 {
		t.Fatalf("blank kind read as %q, want %q", blank.Kind, store.TargetKindS3)
	}
}

// TestLegacyTargetRowsMigrateToTheS3Kind opens a database whose s3_targets table
// predates the kind and path columns: the migration must add them and every
// existing target must keep working as object storage.
func TestLegacyTargetRowsMigrateToTheS3Kind(t *testing.T) {
	ctx := context.Background()

	// 1. A current store, only to produce a secret sealed with this data
	// directory's key — the legacy row has to be readable afterwards.
	st, dir := open(t)
	sealed, err := st.Encrypt("legacy-secret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// 2. Rewind the table to its pre-v0.6.0 shape and put a target in it.
	dsn := "file:" + filepath.ToSlash(filepath.Join(dir, store.DBFileName))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := db.Exec(legacyTargetsSQL); err != nil {
		t.Fatalf("apply legacy targets schema: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO s3_targets (id, name, endpoint, region, bucket, access_key, secret_key_enc,
		                         path_style, status, created_at)
		 VALUES ('t-legacy', 'existing-bucket', 'https://s3.example', 'us-east-1', 'proxback',
		         'access', ?, 1, 'online', '2026-01-01T00:00:00.000000000Z')`, sealed); err != nil {
		t.Fatalf("insert legacy target: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	// 3. Reopen with the current code: the columns appear and the row is intact.
	upgraded, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatalf("open upgraded store: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })

	target, err := upgraded.S3TargetByID(ctx, "t-legacy")
	if err != nil {
		t.Fatalf("load migrated target: %v", err)
	}
	if target.Kind != store.TargetKindS3 {
		t.Fatalf("migrated target kind = %q, want %q", target.Kind, store.TargetKindS3)
	}
	if target.Path != "" {
		t.Fatalf("migrated target has path %q", target.Path)
	}
	if target.Bucket != "proxback" || target.SecretKey != "legacy-secret" || target.Status != "online" {
		t.Fatalf("migrated target = %+v", target)
	}

	// And a filesystem target can be created alongside it.
	nas, err := upgraded.CreateS3Target(ctx, &store.S3Target{
		Name: "nas", Kind: store.TargetKindFilesystem, Path: filepath.FromSlash("/srv/backups"),
	})
	if err != nil {
		t.Fatalf("create filesystem target on an upgraded database: %v", err)
	}
	if err := upgraded.DeleteS3Target(ctx, nas.ID); err != nil {
		t.Fatalf("delete filesystem target: %v", err)
	}
}
