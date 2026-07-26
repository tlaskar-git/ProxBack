package store_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
