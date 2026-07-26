// Package e2e drives a complete ProxBack installation end to end: the S3
// simulator, the Proxmox VE simulator, the server and an in-process agent, all on
// ephemeral ports with temporary directories, exercised exclusively through the
// public REST API with a cookie jar.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"proxback/internal/agent"
	"proxback/internal/app"
	"proxback/internal/engine"
	"proxback/internal/pvesim"
	"proxback/internal/s3sim"
	"proxback/internal/s3target"
)

// ---------------------------------------------------------------- API shapes
//
// These types mirror the REST contract exactly; decoding into them keeps the
// server honest about field names and JSON shapes.

type apiUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type apiHost struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	BaseURL     string  `json:"baseUrl"`
	TokenID     string  `json:"tokenId"`
	InsecureTLS bool    `json:"insecureTLS"`
	Status      string  `json:"status"`
	LastSeen    *string `json:"lastSeen"`
}

type apiVM struct {
	VMID     int    `json:"vmid"`
	Name     string `json:"name"`
	Node     string `json:"node"`
	Status   string `json:"status"`
	MaxDisk  int64  `json:"maxdisk"`
	MaxMem   int64  `json:"maxmem"`
	Uptime   int64  `json:"uptime"`
	HostID   string `json:"hostId"`
	HostName string `json:"hostName"`
}

type apiTarget struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Endpoint  string `json:"endpoint"`
	Bucket    string `json:"bucket"`
	Region    string `json:"region"`
	PathStyle bool   `json:"pathStyle"`
	Status    string `json:"status"`
}

type apiJobSource struct {
	HostID  string   `json:"hostId,omitempty"`
	VMID    int      `json:"vmid,omitempty"`
	Name    string   `json:"name,omitempty"`
	AgentID string   `json:"agentId,omitempty"`
	Paths   []string `json:"paths,omitempty"`
}

type apiRun struct {
	ID             string  `json:"id"`
	JobID          string  `json:"jobId"`
	JobName        string  `json:"jobName"`
	Status         string  `json:"status"`
	StartedAt      string  `json:"startedAt"`
	FinishedAt     *string `json:"finishedAt"`
	BytesProcessed int64   `json:"bytesProcessed"`
	BytesUploaded  int64   `json:"bytesUploaded"`
	DedupRatio     float64 `json:"dedupRatio"`
	Error          string  `json:"error"`
	ProgressPct    float64 `json:"progressPct"`
	CurrentStep    string  `json:"currentStep"`
}

type apiJob struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Kind       string         `json:"kind"`
	TargetID   string         `json:"targetId"`
	TargetName string         `json:"targetName"`
	Schedule   string         `json:"schedule"`
	Retention  int            `json:"retention"`
	Enabled    bool           `json:"enabled"`
	Sources    []apiJobSource `json:"sources"`
	LastRun    *apiRun        `json:"lastRun"`
}

type apiDisk struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"sizeBytes"`
}

type apiBackup struct {
	ID            string    `json:"id"`
	JobID         string    `json:"jobId"`
	SourceKind    string    `json:"sourceKind"`
	SourceID      string    `json:"sourceId"`
	SourceName    string    `json:"sourceName"`
	TargetID      string    `json:"targetId"`
	CreatedAt     string    `json:"createdAt"`
	SizeBytes     int64     `json:"sizeBytes"`
	UploadedBytes int64     `json:"uploadedBytes"`
	Kind          string    `json:"kind"`
	ParentID      string    `json:"parentId"`
	Disks         []apiDisk `json:"disks"`
}

type apiAgent struct {
	ID           string  `json:"id"`
	Hostname     string  `json:"hostname"`
	OS           string  `json:"os"`
	Arch         string  `json:"arch"`
	Version      string  `json:"version"`
	Status       string  `json:"status"`
	LastSeen     *string `json:"lastSeen"`
	RegisteredAt string  `json:"registeredAt"`
}

type apiDashboard struct {
	VMCount     int `json:"vmCount"`
	AgentCount  int `json:"agentCount"`
	HostCount   int `json:"hostCount"`
	TargetCount int `json:"targetCount"`
	JobCount    int `json:"jobCount"`
	Last24h     struct {
		Succeeded int `json:"succeeded"`
		Failed    int `json:"failed"`
		Running   int `json:"running"`
	} `json:"last24h"`
	StorageBytes    int64    `json:"storageBytes"`
	DedupSavedBytes int64    `json:"dedupSavedBytes"`
	RecentRuns      []apiRun `json:"recentRuns"`
}

type apiSettings struct {
	ServerName  string `json:"serverName"`
	Concurrency int    `json:"concurrency"`
}

// ---------------------------------------------------------------- harness

type harness struct {
	t        *testing.T
	client   *http.Client
	base     string
	sim      *pvesim.Sim
	simURL   string
	s3URL    string
	dataDir  string
	instance *app.App
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 1. S3 simulator.
	s3, err := s3sim.New("")
	if err != nil {
		t.Fatalf("start s3-sim: %v", err)
	}
	t.Cleanup(func() { _ = s3.Close() })
	s3srv := httptest.NewServer(s3.Handler)
	t.Cleanup(s3srv.Close)

	// 1b. Proxmox VE simulator.
	sim := pvesim.New(log)
	pveSrv := httptest.NewServer(sim.Handler())
	t.Cleanup(pveSrv.Close)

	// 1c. ProxBack server.
	dataDir := t.TempDir()
	instance, err := app.New(context.Background(), app.Options{DataDir: dataDir, Logger: log})
	if err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		if err := instance.Close(); err != nil {
			t.Logf("server shutdown: %v", err)
		}
	})
	srv := httptest.NewServer(instance.Handler)
	t.Cleanup(srv.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	return &harness{
		t:        t,
		client:   &http.Client{Jar: jar, Timeout: 5 * time.Minute},
		base:     srv.URL,
		sim:      sim,
		simURL:   pveSrv.URL,
		s3URL:    s3srv.URL,
		dataDir:  dataDir,
		instance: instance,
	}
}

// do performs an API call and returns the status code plus the raw body.
func (h *harness) do(method, path string, body any) (int, []byte) {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("encode %s %s: %v", method, path, err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.base+path, rdr)
	if err != nil {
		h.t.Fatalf("build %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return resp.StatusCode, raw
}

// ok performs an API call, requires 200 and decodes the body into out.
func (h *harness) ok(method, path string, body, out any) {
	h.t.Helper()
	code, raw := h.do(method, path, body)
	if code != http.StatusOK {
		h.t.Fatalf("%s %s: status %d, body %s", method, path, code, raw)
	}
	if out == nil {
		return
	}
	if err := json.Unmarshal(raw, out); err != nil {
		h.t.Fatalf("%s %s: decode %s: %v", method, path, raw, err)
	}
}

// waitRun polls a run until it leaves the running state.
func (h *harness) waitRun(runID string, timeout time.Duration) apiRun {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	var run apiRun
	for time.Now().Before(deadline) {
		h.ok(http.MethodGet, "/api/runs/"+runID, nil, &run)
		if run.Status != "running" {
			return run
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatalf("run %s still running after %s (step %q)", runID, timeout, run.CurrentStep)
	return run
}

// runJob triggers a job and waits for it to succeed.
func (h *harness) runJob(jobID string) apiRun {
	h.t.Helper()
	var started struct {
		RunID string `json:"runId"`
	}
	h.ok(http.MethodPost, "/api/jobs/"+jobID+"/run", nil, &started)
	if started.RunID == "" {
		h.t.Fatal("run trigger returned an empty runId")
	}
	run := h.waitRun(started.RunID, 90*time.Second)
	if run.Status != "success" {
		h.t.Fatalf("run %s finished %q: %s (step %q)", run.ID, run.Status, run.Error, run.CurrentStep)
	}
	if run.FinishedAt == nil {
		h.t.Fatalf("successful run %s has no finishedAt", run.ID)
	}
	return run
}

// fetchRaw downloads a simulator byte stream.
func (h *harness) fetchRaw(url string) []byte {
	h.t.Helper()
	resp, err := http.Get(url) //nolint:noctx // test helper
	if err != nil {
		h.t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("GET %s: read: %v", url, err)
	}
	return raw
}

// s3Client builds a direct client against a simulator bucket so the test can
// assert on the object layout the engine produced.
func (h *harness) s3Client(bucket string) *s3target.Client {
	h.t.Helper()
	c, err := s3target.New(context.Background(), s3target.Config{
		Endpoint: h.s3URL, Region: "us-east-1", Bucket: bucket,
		AccessKey: "proxback", SecretKey: "proxback-secret", PathStyle: true,
	})
	if err != nil {
		h.t.Fatalf("s3 client: %v", err)
	}
	return c
}

func (h *harness) objects(bucket, prefix string) []s3target.Object {
	h.t.Helper()
	objs, err := h.s3Client(bucket).List(context.Background(), prefix)
	if err != nil {
		h.t.Fatalf("list %s/%s: %v", bucket, prefix, err)
	}
	return objs
}

func (h *harness) chunkBytes(bucket string) (count int, total int64) {
	h.t.Helper()
	for _, o := range h.objects(bucket, engine.ChunkPrefix) {
		count++
		total += o.Size
	}
	return count, total
}

// ---------------------------------------------------------------- the scenario

const (
	vmBucket    = "proxback-vm"
	agentBucket = "proxback-agent"
	adminUser   = "admin"
	adminPass   = "proxback-e2e-password"
	mib         = 1 << 20
)

func TestEndToEnd(t *testing.T) {
	h := newHarness(t)

	// ---- Step 9a: unauthenticated access is rejected -----------------------
	t.Run("01-unauthenticated-rejected", func(t *testing.T) {
		if code, _ := h.do(http.MethodGet, "/api/jobs", nil); code != http.StatusUnauthorized {
			t.Fatalf("GET /api/jobs without a session = %d, want 401", code)
		}
		if code, _ := h.do(http.MethodGet, "/api/me", nil); code != http.StatusUnauthorized {
			t.Fatalf("GET /api/me without a session = %d, want 401", code)
		}
		if code, _ := h.do(http.MethodPost, "/api/agents/heartbeat", map[string]any{}); code != http.StatusUnauthorized {
			t.Fatalf("agent heartbeat without a key = %d, want 401", code)
		}
	})

	// ---- Step 2: default admin login, forced password hygiene --------------
	t.Run("02-default-login-and-password-change", func(t *testing.T) {
		// A fresh install seeds admin/admin, so setup is never needed and the
		// login page is told to hint at the default credentials.
		var status struct {
			NeedsSetup   bool `json:"needsSetup"`
			DefaultLogin bool `json:"defaultLogin"`
		}
		h.ok(http.MethodGet, "/api/setup/status", nil, &status)
		if status.NeedsSetup {
			t.Fatal("fresh install reports needsSetup despite seeded admin")
		}
		if !status.DefaultLogin {
			t.Fatal("fresh install does not report defaultLogin")
		}
		if code, _ := h.do(http.MethodPost, "/api/setup", map[string]string{"username": "x", "password": "12345678"}); code != http.StatusConflict {
			t.Fatalf("setup on a seeded install = %d, want 409", code)
		}

		// Wrong password must be rejected.
		if code, _ := h.do(http.MethodPost, "/api/login", map[string]string{"username": adminUser, "password": "nope"}); code != http.StatusUnauthorized {
			t.Fatalf("login with a wrong password = %d, want 401", code)
		}
		var login struct {
			User apiUser `json:"user"`
		}
		h.ok(http.MethodPost, "/api/login", map[string]string{"username": adminUser, "password": "admin"}, &login)
		if login.User.Username != adminUser || login.User.ID == 0 {
			t.Fatalf("login returned %+v", login.User)
		}

		// /api/me nags about the default password until it is changed.
		var me struct {
			User               apiUser `json:"user"`
			MustChangePassword bool    `json:"mustChangePassword"`
		}
		h.ok(http.MethodGet, "/api/me", nil, &me)
		if me.User.Username != adminUser || !me.MustChangePassword {
			t.Fatalf("/api/me returned %+v", me)
		}

		// Change the password: wrong current rejected, short new rejected.
		if code, _ := h.do(http.MethodPost, "/api/me/password", map[string]string{"currentPassword": "nope", "newPassword": adminPass}); code != http.StatusUnauthorized {
			t.Fatalf("password change with wrong current = %d, want 401", code)
		}
		if code, _ := h.do(http.MethodPost, "/api/me/password", map[string]string{"currentPassword": "admin", "newPassword": "short"}); code != http.StatusBadRequest {
			t.Fatalf("password change with short new = %d, want 400", code)
		}
		h.ok(http.MethodPost, "/api/me/password", map[string]string{"currentPassword": "admin", "newPassword": adminPass}, nil)

		// The nag clears, admin/admin stops working, the new password works.
		h.ok(http.MethodGet, "/api/me", nil, &me)
		if me.MustChangePassword {
			t.Fatal("mustChangePassword still true after password change")
		}
		h.ok(http.MethodGet, "/api/setup/status", nil, &status)
		if status.DefaultLogin {
			t.Fatal("defaultLogin still true after password change")
		}
		if code, _ := h.do(http.MethodPost, "/api/login", map[string]string{"username": adminUser, "password": "admin"}); code != http.StatusUnauthorized {
			t.Fatalf("default password still accepted after change = %d, want 401", code)
		}
		h.ok(http.MethodPost, "/api/login", map[string]string{"username": adminUser, "password": adminPass}, &login)
		if login.User.Username != adminUser {
			t.Fatalf("login with new password returned %+v", login.User)
		}
	})

	var host apiHost
	var vmTarget, agtTarget apiTarget

	// ---- Step 2 (cont): host + target, both Test OK ------------------------
	t.Run("03-add-host-and-targets", func(t *testing.T) {
		h.ok(http.MethodPost, "/api/hosts", map[string]any{
			"name":        "pve-sim",
			"baseUrl":     h.simURL,
			"tokenId":     "root@pam!proxback",
			"tokenSecret": "sim-token-secret",
			"insecureTLS": true,
		}, &host)
		if host.ID == "" || host.BaseURL != h.simURL || host.TokenID != "root@pam!proxback" {
			t.Fatalf("created host = %+v", host)
		}

		var hosts []apiHost
		h.ok(http.MethodGet, "/api/hosts", nil, &hosts)
		if len(hosts) != 1 || hosts[0].ID != host.ID {
			t.Fatalf("host list = %+v", hosts)
		}
		if strings.Contains(string(mustJSON(t, hosts)), "sim-token-secret") {
			t.Fatal("host listing leaked the API token secret")
		}

		var test struct {
			OK    bool   `json:"ok"`
			Nodes int    `json:"nodes"`
			Error string `json:"error"`
		}
		h.ok(http.MethodPost, "/api/hosts/"+host.ID+"/test", nil, &test)
		if !test.OK || test.Nodes != 2 {
			t.Fatalf("host test = %+v, want ok with 2 nodes", test)
		}

		for _, spec := range []struct {
			name   string
			bucket string
			out    *apiTarget
		}{
			{"vm-storage", vmBucket, &vmTarget},
			{"agent-storage", agentBucket, &agtTarget},
		} {
			h.ok(http.MethodPost, "/api/targets", map[string]any{
				"name":      spec.name,
				"endpoint":  h.s3URL,
				"region":    "us-east-1",
				"bucket":    spec.bucket,
				"accessKey": "proxback",
				"secretKey": "proxback-secret",
				"pathStyle": true,
			}, spec.out)
			if spec.out.ID == "" || spec.out.Bucket != spec.bucket {
				t.Fatalf("created target = %+v", *spec.out)
			}
			var probe struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			h.ok(http.MethodPost, "/api/targets/"+spec.out.ID+"/test", nil, &probe)
			if !probe.OK {
				t.Fatalf("target %s test failed: %s", spec.name, probe.Error)
			}
		}

		var targets []apiTarget
		h.ok(http.MethodGet, "/api/targets", nil, &targets)
		if len(targets) != 2 {
			t.Fatalf("target list has %d entries", len(targets))
		}
		if strings.Contains(string(mustJSON(t, targets)), "proxback-secret") {
			t.Fatal("target listing leaked the S3 secret key")
		}

		// The simulator recorded our PVEAPIToken header.
		seen := h.sim.AuthSeen()
		if len(seen) == 0 || !strings.HasPrefix(seen[0], "PVEAPIToken=root@pam!proxback=") {
			t.Fatalf("simulator saw auth headers %v", seen)
		}
	})

	var vms []apiVM
	t.Run("04-inventory", func(t *testing.T) {
		h.ok(http.MethodGet, "/api/hosts/"+host.ID+"/vms", nil, &vms)
		if len(vms) != 4 {
			t.Fatalf("live inventory has %d guests, want 4", len(vms))
		}
		nodes := map[string]int{}
		for _, v := range vms {
			nodes[v.Node]++
			if v.Name == "" || v.MaxDisk == 0 {
				t.Fatalf("guest %d looks empty: %+v", v.VMID, v)
			}
		}
		if len(nodes) != 2 {
			t.Fatalf("guests span %d nodes, want 2", len(nodes))
		}
		var cached []apiVM
		h.ok(http.MethodGet, "/api/vms", nil, &cached)
		if len(cached) != 4 {
			t.Fatalf("cached inventory has %d guests", len(cached))
		}
		for _, v := range cached {
			if v.HostID != host.ID || v.HostName != "pve-sim" {
				t.Fatalf("cached guest missing host info: %+v", v)
			}
		}
	})

	var vmJob apiJob
	var run1 apiRun

	// ---- Step 3: first VM backup ------------------------------------------
	t.Run("05-first-vm-backup-is-full", func(t *testing.T) {
		h.ok(http.MethodPost, "/api/jobs", map[string]any{
			"name":      "nightly-vms",
			"kind":      "vm",
			"targetId":  vmTarget.ID,
			"schedule":  "manual",
			"retention": 2,
			"enabled":   true,
			"sources": []map[string]any{
				{"hostId": host.ID, "vmid": 100, "name": "web-01"},
				{"hostId": host.ID, "vmid": 101, "name": "db-01"},
			},
		}, &vmJob)
		if vmJob.ID == "" || vmJob.Kind != "vm" || len(vmJob.Sources) != 2 {
			t.Fatalf("created job = %+v", vmJob)
		}
		if vmJob.TargetName != "vm-storage" || vmJob.Retention != 2 || vmJob.Schedule != "manual" {
			t.Fatalf("job fields = %+v", vmJob)
		}
		if vmJob.LastRun != nil {
			t.Fatalf("new job already has a lastRun: %+v", vmJob.LastRun)
		}

		run1 = h.runJob(vmJob.ID)
		// 3 disks x 16 MiB across the two guests.
		if want := int64(48 * mib); run1.BytesProcessed != want {
			t.Fatalf("bytesProcessed = %d, want %d", run1.BytesProcessed, want)
		}
		if want := int64(48 * mib); run1.BytesUploaded != want {
			t.Fatalf("first run bytesUploaded = %d, want %d (nothing to dedup yet)", run1.BytesUploaded, want)
		}
		if run1.ProgressPct < 99.9 {
			t.Fatalf("progressPct = %v", run1.ProgressPct)
		}

		// Manifests and chunks exist on the target.
		manifests := h.objects(vmBucket, engine.ManifestPrefix)
		if len(manifests) != 2 {
			t.Fatalf("target holds %d manifests, want 2", len(manifests))
		}
		count, total := h.chunkBytes(vmBucket)
		if count != 12 || total != 48*mib {
			t.Fatalf("target holds %d chunks / %d bytes, want 12 / %d", count, total, 48*mib)
		}

		var backups []apiBackup
		h.ok(http.MethodGet, "/api/backups?targetId="+vmTarget.ID, nil, &backups)
		if len(backups) != 2 {
			t.Fatalf("restore points = %d, want 2", len(backups))
		}
		for _, b := range backups {
			if b.Kind != "full" || b.ParentID != "" {
				t.Fatalf("first backup of %s is %q with parent %q, want full/none", b.SourceName, b.Kind, b.ParentID)
			}
			if b.SourceKind != "vm" || b.JobID != vmJob.ID || b.TargetID != vmTarget.ID {
				t.Fatalf("backup metadata = %+v", b)
			}
			switch b.SourceName {
			case "web-01":
				if len(b.Disks) != 2 || b.SizeBytes != 32*mib {
					t.Fatalf("web-01 backup = %+v, want 2 disks / 32 MiB", b)
				}
			case "db-01":
				if len(b.Disks) != 1 || b.SizeBytes != 16*mib {
					t.Fatalf("db-01 backup = %+v, want 1 disk / 16 MiB", b)
				}
			default:
				t.Fatalf("unexpected source %q", b.SourceName)
			}
		}

		var jobs []apiJob
		h.ok(http.MethodGet, "/api/jobs", nil, &jobs)
		if len(jobs) != 1 || jobs[0].LastRun == nil || jobs[0].LastRun.ID != run1.ID {
			t.Fatalf("job lastRun not reported: %+v", jobs)
		}
	})

	// ---- Step 4: unchanged re-run deduplicates ----------------------------
	t.Run("06-unchanged-rerun-dedups", func(t *testing.T) {
		run := h.runJob(vmJob.ID)
		if run.BytesProcessed != int64(48*mib) {
			t.Fatalf("bytesProcessed = %d, want %d", run.BytesProcessed, 48*mib)
		}
		if run.BytesUploaded != 0 {
			t.Fatalf("unchanged re-run uploaded %d bytes, want 0", run.BytesUploaded)
		}
		if run.DedupRatio < 0.999 {
			t.Fatalf("dedupRatio = %v, want ~1", run.DedupRatio)
		}
		count, total := h.chunkBytes(vmBucket)
		if count != 12 || total != 48*mib {
			t.Fatalf("chunk store grew to %d chunks / %d bytes", count, total)
		}

		// The second restore point of each source is incremental with a parent.
		var backups []apiBackup
		h.ok(http.MethodGet, "/api/backups?sourceKind=vm&sourceId="+host.ID+"_100", nil, &backups)
		if len(backups) != 2 {
			t.Fatalf("web-01 restore points = %d, want 2", len(backups))
		}
		if backups[0].Kind != "incremental" || backups[0].ParentID != backups[1].ID {
			t.Fatalf("second backup = %q parent %q, want incremental with parent %s",
				backups[0].Kind, backups[0].ParentID, backups[1].ID)
		}
	})

	// ---- Step 5: mutate a disk, incremental uploads only changed chunks ----
	t.Run("07-incremental-after-mutation", func(t *testing.T) {
		var mutation struct {
			VMID          int    `json:"vmid"`
			Disk          string `json:"disk"`
			ChunksChanged int    `json:"chunksChanged"`
			ChunkSize     int64  `json:"chunkSize"`
		}
		resp, err := http.Post(h.simURL+"/sim/mutate/100", "application/json", nil) //nolint:noctx // test helper
		if err != nil {
			t.Fatalf("mutate: %v", err)
		}
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(&mutation); err != nil {
			t.Fatalf("decode mutate response: %v", err)
		}
		if mutation.ChunksChanged != 1 || mutation.Disk != "scsi0" {
			t.Fatalf("mutation = %+v, want 1 changed chunk on scsi0", mutation)
		}

		run := h.runJob(vmJob.ID)
		if run.BytesProcessed != int64(48*mib) {
			t.Fatalf("bytesProcessed = %d", run.BytesProcessed)
		}
		if run.BytesUploaded != int64(4*mib) {
			t.Fatalf("incremental uploaded %d bytes, want exactly one 4 MiB chunk", run.BytesUploaded)
		}

		count, total := h.chunkBytes(vmBucket)
		if count != 13 || total != 52*mib {
			t.Fatalf("chunk store = %d chunks / %d bytes, want 13 / %d", count, total, 52*mib)
		}

		var backups []apiBackup
		h.ok(http.MethodGet, "/api/backups?sourceKind=vm&sourceId="+host.ID+"_100", nil, &backups)
		// Retention (keep last 2) has already pruned the original full backup.
		if len(backups) != 2 {
			t.Fatalf("web-01 restore points = %d, want 2 after retention", len(backups))
		}
		newest := backups[0]
		if newest.Kind != "incremental" {
			t.Fatalf("newest backup kind = %q, want incremental", newest.Kind)
		}
		if newest.ParentID != backups[1].ID {
			t.Fatalf("newest parentId = %q, want %q", newest.ParentID, backups[1].ID)
		}
		if newest.UploadedBytes != int64(4*mib) {
			t.Fatalf("newest backup uploadedBytes = %d, want %d", newest.UploadedBytes, 4*mib)
		}
	})

	// ---- Step 6: restore a VM and compare bytes ---------------------------
	t.Run("08-vm-restore-is-byte-identical", func(t *testing.T) {
		var backups []apiBackup
		h.ok(http.MethodGet, "/api/backups?sourceKind=vm&sourceId="+host.ID+"_100", nil, &backups)
		newest := backups[0]

		var started struct {
			RunID string `json:"runId"`
		}
		h.ok(http.MethodPost, "/api/restores", map[string]any{
			"backupId": newest.ID,
			"vm":       map[string]any{"hostId": host.ID, "node": "pve1", "vmid": 100},
		}, &started)
		run := h.waitRun(started.RunID, 90*time.Second)
		if run.Status != "success" {
			t.Fatalf("restore run %q: %s", run.Status, run.Error)
		}
		if !strings.HasPrefix(run.JobName, "Restore ") {
			t.Fatalf("restore run jobName = %q", run.JobName)
		}
		if run.BytesProcessed != int64(32*mib) {
			t.Fatalf("restore processed %d bytes, want %d", run.BytesProcessed, 32*mib)
		}

		for _, disk := range []string{"scsi0", "scsi1"} {
			want := h.fetchRaw(fmt.Sprintf("%s/sim/disk/100/%s", h.simURL, disk))
			got := h.fetchRaw(fmt.Sprintf("%s/sim/imported/100/%s", h.simURL, disk))
			if len(got) != len(want) {
				t.Fatalf("restored %s is %d bytes, source is %d", disk, len(got), len(want))
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("restored %s differs from the live disk", disk)
			}
		}
		// The restore run appears in the run list.
		var runs []apiRun
		h.ok(http.MethodGet, "/api/runs?limit=50", nil, &runs)
		found := false
		for _, r := range runs {
			if r.ID == run.ID {
				found = true
			}
		}
		if !found {
			t.Fatal("restore run missing from /api/runs")
		}

		// Side-by-side restore into a free VMID must create the guest, like
		// qmrestore does (regression: the sim used to 404 and the engine's
		// closed-pipe error masked it).
		h.ok(http.MethodPost, "/api/restores", map[string]any{
			"backupId": newest.ID,
			"vm":       map[string]any{"hostId": host.ID, "node": "pve1", "vmid": 9999},
		}, &started)
		run = h.waitRun(started.RunID, 90*time.Second)
		if run.Status != "success" {
			t.Fatalf("side-by-side restore run %q: %s", run.Status, run.Error)
		}
		for _, disk := range []string{"scsi0", "scsi1"} {
			want := h.fetchRaw(fmt.Sprintf("%s/sim/disk/100/%s", h.simURL, disk))
			got := h.fetchRaw(fmt.Sprintf("%s/sim/imported/9999/%s", h.simURL, disk))
			if !bytes.Equal(got, want) {
				t.Fatalf("side-by-side restored %s differs from the live disk", disk)
			}
		}
	})

	// ---- Step 8: retention prunes and orphan chunks are collected ----------
	t.Run("09-retention-and-orphan-gc", func(t *testing.T) {
		before, beforeBytes := h.chunkBytes(vmBucket)
		if before != 13 {
			t.Fatalf("expected 13 chunks before the pruning run, got %d", before)
		}
		var prunedIDs []string
		var backups []apiBackup
		h.ok(http.MethodGet, "/api/backups?sourceKind=vm&sourceId="+host.ID+"_100", nil, &backups)
		prunedIDs = append(prunedIDs, backups[len(backups)-1].ID)

		run := h.runJob(vmJob.ID)
		if run.BytesUploaded != 0 {
			t.Fatalf("fourth run uploaded %d bytes, want 0", run.BytesUploaded)
		}

		h.ok(http.MethodGet, "/api/backups?sourceKind=vm&sourceId="+host.ID+"_100", nil, &backups)
		if len(backups) != 2 {
			t.Fatalf("keep-last-2 left %d restore points", len(backups))
		}
		for _, gone := range prunedIDs {
			for _, b := range backups {
				if b.ID == gone {
					t.Fatalf("restore point %s should have been pruned", gone)
				}
			}
		}
		// 2 sources x keep 2 = 4 manifests.
		if manifests := h.objects(vmBucket, engine.ManifestPrefix); len(manifests) != 4 {
			t.Fatalf("target holds %d manifests, want 4", len(manifests))
		}
		after, afterBytes := h.chunkBytes(vmBucket)
		if after != 12 || afterBytes != 48*mib {
			t.Fatalf("after gc: %d chunks / %d bytes (was %d / %d), want 12 / %d",
				after, afterBytes, before, beforeBytes, 48*mib)
		}

		// Deleting a restore point through the API also works.
		h.ok(http.MethodDelete, "/api/backups/"+backups[len(backups)-1].ID, nil, nil)
		var remaining []apiBackup
		h.ok(http.MethodGet, "/api/backups?sourceKind=vm&sourceId="+host.ID+"_100", nil, &remaining)
		if len(remaining) != 1 {
			t.Fatalf("after delete there are %d restore points, want 1", len(remaining))
		}
	})

	// ---- Step 7: agent enrollment, backup, incremental, restore ------------
	srcDir := filepath.Join(t.TempDir(), "payload")
	destDir := filepath.Join(t.TempDir(), "restored")
	var agentInfo apiAgent
	var agentJob apiJob

	// The agent must outlive this subtest, so its lifecycle is bound to the
	// parent test rather than the subtest that starts it.
	parent := t
	agentConfigDir := t.TempDir()

	t.Run("10-agent-enroll", func(t *testing.T) {
		writeTestTree(t, srcDir)

		var enroll struct {
			Token     string `json:"token"`
			ExpiresAt string `json:"expiresAt"`
		}
		h.ok(http.MethodPost, "/api/agents/enroll-token", nil, &enroll)
		if enroll.Token == "" || enroll.ExpiresAt == "" {
			t.Fatalf("enroll token response = %+v", enroll)
		}

		ag, err := agent.New(agent.Config{
			ServerURL:         h.base,
			Token:             enroll.Token,
			ConfigDir:         agentConfigDir,
			HeartbeatInterval: 200 * time.Millisecond,
			Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err != nil {
			t.Fatalf("build agent: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		parent.Cleanup(cancel)
		go func() {
			_ = ag.Run(ctx)
		}()

		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			var agents []apiAgent
			h.ok(http.MethodGet, "/api/agents", nil, &agents)
			if len(agents) == 1 && agents[0].Status == "online" {
				agentInfo = agents[0]
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if agentInfo.ID == "" {
			t.Fatal("agent never came online")
		}
		if agentInfo.Hostname == "" || agentInfo.OS == "" || agentInfo.Arch == "" || agentInfo.Version == "" {
			t.Fatalf("agent registration incomplete: %+v", agentInfo)
		}

		// An enrollment token is single use.
		code, _ := h.do(http.MethodPost, "/api/agents/register", map[string]any{
			"token": enroll.Token, "hostname": "impostor", "os": "linux", "arch": "amd64", "version": "1",
		})
		if code != http.StatusUnauthorized {
			t.Fatalf("reusing an enrollment token = %d, want 401", code)
		}
	})

	t.Run("11-agent-backup-and-incremental", func(t *testing.T) {
		h.ok(http.MethodPost, "/api/jobs", map[string]any{
			"name":      "workstation-files",
			"kind":      "agent",
			"targetId":  agtTarget.ID,
			"schedule":  "manual",
			"retention": 2,
			"enabled":   true,
			"sources":   []map[string]any{{"agentId": agentInfo.ID, "paths": []string{srcDir}}},
		}, &agentJob)
		if agentJob.ID == "" || agentJob.Kind != "agent" {
			t.Fatalf("agent job = %+v", agentJob)
		}
		if len(agentJob.Sources) != 1 || agentJob.Sources[0].AgentID != agentInfo.ID {
			t.Fatalf("agent job sources = %+v", agentJob.Sources)
		}

		full := h.runJob(agentJob.ID)
		if full.BytesProcessed < 12*mib {
			t.Fatalf("agent backup processed %d bytes, want >= %d", full.BytesProcessed, 12*mib)
		}
		if full.BytesUploaded != full.BytesProcessed {
			t.Fatalf("first agent backup uploaded %d of %d bytes", full.BytesUploaded, full.BytesProcessed)
		}

		var backups []apiBackup
		h.ok(http.MethodGet, "/api/backups?sourceKind=agent&sourceId="+agentInfo.ID, nil, &backups)
		if len(backups) != 1 || backups[0].Kind != "full" || len(backups[0].Disks) != 1 {
			t.Fatalf("agent restore points = %+v", backups)
		}
		if backups[0].Disks[0].Name != "files.tar" {
			t.Fatalf("agent stream name = %q", backups[0].Disks[0].Name)
		}

		// An unchanged re-run must upload nothing at all.
		same := h.runJob(agentJob.ID)
		if same.BytesUploaded != 0 {
			t.Fatalf("unchanged agent re-run uploaded %d bytes, want 0", same.BytesUploaded)
		}

		// Change one small file at the end of the archive: only the last chunk moves.
		mutateTestTree(t, srcDir)
		inc := h.runJob(agentJob.ID)
		if inc.BytesUploaded == 0 {
			t.Fatal("agent incremental uploaded nothing after a file changed")
		}
		if inc.BytesUploaded >= inc.BytesProcessed {
			t.Fatalf("agent incremental uploaded %d of %d bytes; expected only the changed chunk",
				inc.BytesUploaded, inc.BytesProcessed)
		}
		if inc.BytesUploaded > 2*mib {
			t.Fatalf("agent incremental uploaded %d bytes, expected well under 2 MiB", inc.BytesUploaded)
		}

		h.ok(http.MethodGet, "/api/backups?sourceKind=agent&sourceId="+agentInfo.ID, nil, &backups)
		if len(backups) != 2 {
			t.Fatalf("agent restore points after retention = %d, want 2", len(backups))
		}
		if backups[0].Kind != "incremental" || backups[0].ParentID != backups[1].ID {
			t.Fatalf("agent incremental chain = %q parent %q, want parent %s",
				backups[0].Kind, backups[0].ParentID, backups[1].ID)
		}
	})

	t.Run("12-agent-restore-round-trip", func(t *testing.T) {
		var backups []apiBackup
		h.ok(http.MethodGet, "/api/backups?sourceKind=agent&sourceId="+agentInfo.ID, nil, &backups)

		var started struct {
			RunID string `json:"runId"`
		}
		h.ok(http.MethodPost, "/api/restores", map[string]any{
			"backupId": backups[0].ID,
			"agent":    map[string]any{"agentId": agentInfo.ID, "destPath": destDir},
		}, &started)
		run := h.waitRun(started.RunID, 90*time.Second)
		if run.Status != "success" {
			t.Fatalf("agent restore %q: %s", run.Status, run.Error)
		}
		diffTrees(t, srcDir, filepath.Join(destDir, filepath.Base(srcDir)))
	})

	// ---- Dashboard, settings, cancellation, misc --------------------------
	t.Run("13-dashboard-and-settings", func(t *testing.T) {
		var dash apiDashboard
		h.ok(http.MethodGet, "/api/dashboard", nil, &dash)
		if dash.VMCount != 4 || dash.HostCount != 1 || dash.TargetCount != 2 || dash.JobCount != 2 || dash.AgentCount != 1 {
			t.Fatalf("dashboard counts = %+v", dash)
		}
		if dash.Last24h.Succeeded < 7 || dash.Last24h.Failed != 0 || dash.Last24h.Running != 0 {
			t.Fatalf("dashboard last24h = %+v", dash.Last24h)
		}
		if dash.StorageBytes <= 0 || dash.DedupSavedBytes <= 0 {
			t.Fatalf("dashboard storage = %d stored / %d saved", dash.StorageBytes, dash.DedupSavedBytes)
		}
		if len(dash.RecentRuns) == 0 {
			t.Fatal("dashboard has no recent runs")
		}

		var settings apiSettings
		h.ok(http.MethodGet, "/api/settings", nil, &settings)
		if settings.ServerName != "ProxBack" || settings.Concurrency != 2 {
			t.Fatalf("default settings = %+v", settings)
		}
		h.ok(http.MethodPut, "/api/settings", map[string]any{"serverName": "lab", "concurrency": 3}, &settings)
		if settings.ServerName != "lab" || settings.Concurrency != 3 {
			t.Fatalf("updated settings = %+v", settings)
		}
		h.ok(http.MethodGet, "/api/settings", nil, &settings)
		if settings.Concurrency != 3 {
			t.Fatalf("settings did not persist: %+v", settings)
		}
	})

	t.Run("14-misc-contract-checks", func(t *testing.T) {
		// 409 when a job is already running.
		var started struct {
			RunID string `json:"runId"`
		}
		h.ok(http.MethodPost, "/api/jobs/"+vmJob.ID+"/run", nil, &started)
		if code, body := h.do(http.MethodPost, "/api/jobs/"+vmJob.ID+"/run", nil); code != http.StatusConflict {
			t.Fatalf("second concurrent run = %d (%s), want 409", code, body)
		}
		// Cancellation moves the run to canceled.
		h.ok(http.MethodPost, "/api/runs/"+started.RunID+"/cancel", nil, nil)
		run := h.waitRun(started.RunID, 60*time.Second)
		if run.Status != "canceled" && run.Status != "success" {
			t.Fatalf("canceled run status = %q", run.Status)
		}

		// Unknown API routes return JSON errors, not the SPA.
		code, body := h.do(http.MethodGet, "/api/does-not-exist", nil)
		if code != http.StatusNotFound || !strings.Contains(string(body), `"error"`) {
			t.Fatalf("unknown api route = %d (%s)", code, body)
		}
		// Unknown SPA routes fall back to index.html.
		resp, err := h.client.Get(h.base + "/jobs")
		if err != nil {
			t.Fatalf("GET /jobs: %v", err)
		}
		spa, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(spa), "ProxBack") {
			t.Fatalf("SPA fallback = %d (%s)", resp.StatusCode, spa)
		}
		// Missing agent downloads report a JSON error.
		code, body = h.do(http.MethodGet, "/downloads/proxback-agent-linux-amd64", nil)
		if code != http.StatusNotFound || !strings.Contains(string(body), `"error"`) {
			t.Fatalf("missing download = %d (%s)", code, body)
		}
		// A present agent binary is served.
		if err := os.WriteFile(filepath.Join(h.dataDir, "downloads", "proxback-agent-windows-amd64.exe"),
			[]byte("MZ-fake-binary"), 0o644); err != nil {
			t.Fatalf("stage download: %v", err)
		}
		code, body = h.do(http.MethodGet, "/downloads/proxback-agent-windows-amd64.exe", nil)
		if code != http.StatusOK || string(body) != "MZ-fake-binary" {
			t.Fatalf("staged download = %d (%s)", code, body)
		}

		// Logout invalidates the session.
		h.ok(http.MethodPost, "/api/logout", nil, nil)
		if code, _ := h.do(http.MethodGet, "/api/jobs", nil); code != http.StatusUnauthorized {
			t.Fatalf("GET /api/jobs after logout = %d, want 401", code)
		}
	})
}

// ---------------------------------------------------------------- test data

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// writeTestTree creates a deterministic file tree with one large file (so the
// tar stream spans several chunks) and small files at the end of tar order.
func writeTestTree(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write := func(rel string, data []byte) {
		p := filepath.Join(dir, rel)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	write("aaa-big.bin", pseudoBytes(12*mib, 1234))
	write("sub/notes.txt", []byte("ProxBack agent backup test tree\n"))
	write("zzz-tail.txt", pseudoBytes(4096, 99))
}

// mutateTestTree rewrites the last file in tar order, keeping its size identical
// so only the final chunk of the tar stream changes.
func mutateTestTree(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "zzz-tail.txt"), pseudoBytes(4096, 4242), 0o644); err != nil {
		t.Fatalf("mutate: %v", err)
	}
}

func pseudoBytes(n int, seed uint64) []byte {
	out := make([]byte, n)
	x := seed
	for i := range out {
		x = x*6364136223846793005 + 1442695040888963407
		out[i] = byte(x >> 33)
	}
	return out
}

// diffTrees fails when two directory trees differ in structure or file contents.
func diffTrees(t *testing.T, want, got string) {
	t.Helper()
	wantFiles := treeOf(t, want)
	gotFiles := treeOf(t, got)
	if len(wantFiles) == 0 {
		t.Fatalf("source tree %s is empty", want)
	}
	wantKeys := sortedKeys(wantFiles)
	gotKeys := sortedKeys(gotFiles)
	if strings.Join(wantKeys, "\n") != strings.Join(gotKeys, "\n") {
		t.Fatalf("restored tree differs.\nwant:\n%s\ngot:\n%s",
			strings.Join(wantKeys, "\n"), strings.Join(gotKeys, "\n"))
	}
	for _, k := range wantKeys {
		if !bytes.Equal(wantFiles[k], gotFiles[k]) {
			t.Fatalf("restored file %q differs (%d vs %d bytes)", k, len(wantFiles[k]), len(gotFiles[k]))
		}
	}
}

func treeOf(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if info.IsDir() {
			out[rel+"/"] = nil
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = data
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
