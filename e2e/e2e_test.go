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
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	VMID     int      `json:"vmid"`
	Name     string   `json:"name"`
	Node     string   `json:"node"`
	Status   string   `json:"status"`
	Tags     []string `json:"tags"`
	MaxDisk  int64    `json:"maxdisk"`
	MaxMem   int64    `json:"maxmem"`
	Uptime   int64    `json:"uptime"`
	HostID   string   `json:"hostId"`
	HostName string   `json:"hostName"`
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

// apiRunLogLine mirrors one line of GET /api/runs/{id}/log. The timestamp stays
// a string so the test can assert it really is RFC3339 on the wire.
type apiRunLogLine struct {
	TS   string `json:"ts"`
	Line string `json:"line"`
}

type apiRunLog struct {
	Lines []apiRunLogLine `json:"lines"`
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
	TagFilter  *string        `json:"tagFilter"`
	NextRun    *string        `json:"nextRun"`
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

type apiHelper struct {
	ID           string  `json:"id"`
	Node         string  `json:"node"`
	Address      string  `json:"address"`
	Port         int     `json:"port"`
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
	WebhookURL  string `json:"webhookUrl"`
	NotifyOn    string `json:"notifyOn"`
}

// webhookPayload mirrors the notification body in the contract.
type webhookPayload struct {
	Event          string  `json:"event"`
	Server         string  `json:"server"`
	Job            string  `json:"job"`
	Kind           string  `json:"kind"`
	Status         string  `json:"status"`
	BytesProcessed int64   `json:"bytesProcessed"`
	BytesUploaded  int64   `json:"bytesUploaded"`
	DedupRatio     float64 `json:"dedupRatio"`
	Error          string  `json:"error"`
	StartedAt      string  `json:"startedAt"`
	FinishedAt     string  `json:"finishedAt"`
	DurationSec    float64 `json:"durationSec"`
}

// webhookCollector is a stand-in for the operator's automation endpoint.
type webhookCollector struct {
	url string

	mu       sync.Mutex
	payloads []webhookPayload
}

func (c *webhookCollector) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p webhookPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		c.payloads = append(c.payloads, p)
		c.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
}

func (c *webhookCollector) all() []webhookPayload {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]webhookPayload(nil), c.payloads...)
}

// ---------------------------------------------------------------- fake node helper

// helperRequest is one call a node helper received.
type helperRequest struct {
	Method string
	Path   string
	Query  string
	Auth   string
	Bytes  int
}

// nodeHelper is an in-process stand-in for the proxback-helper daemon that runs
// as root on a Proxmox node. It answers the same three endpoints: /healthz,
// /export/{vmid} (a deterministic archive instead of a real vzdump stream) and
// /import/{vmid} (capturing what would have gone into qmrestore).
type nodeHelper struct {
	node    string
	secret  string
	content []byte
	url     string
	port    int

	mu       sync.Mutex
	requests []helperRequest
	imported map[int][]byte
}

func newNodeHelper(t *testing.T, node string, size int, seed uint64) *nodeHelper {
	t.Helper()
	nh := &nodeHelper{
		node:     node,
		secret:   "e2e-helper-access-secret-" + node,
		content:  pseudoBytes(size, seed),
		imported: map[int][]byte{},
	}
	srv := httptest.NewServer(nh.handler())
	t.Cleanup(srv.Close)
	nh.url = srv.URL
	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split helper url %s: %v", srv.URL, err)
	}
	if nh.port, err = strconv.Atoi(port); err != nil {
		t.Fatalf("helper port %q: %v", port, err)
	}
	if host != "127.0.0.1" {
		t.Fatalf("helper listening on %s, want 127.0.0.1 so the server can reach it", host)
	}
	return nh
}

func (nh *nodeHelper) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		nh.record(r, 0)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"node":%q,"version":"e2e"}`, nh.node)
	})
	mux.HandleFunc("/export/", func(w http.ResponseWriter, r *http.Request) {
		if !nh.authorized(w, r) {
			return
		}
		nh.record(r, len(nh.content))
		// Chunked, exactly like the real helper: no Content-Length is knowable.
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(nh.content)
	})
	mux.HandleFunc("/import/", func(w http.ResponseWriter, r *http.Request) {
		if !nh.authorized(w, r) {
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		nh.record(r, len(raw))
		vmid, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/import/"))
		if err != nil {
			http.Error(w, "bad vmid", http.StatusBadRequest)
			return
		}
		nh.mu.Lock()
		nh.imported[vmid] = raw
		nh.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	return mux
}

func (nh *nodeHelper) authorized(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") == "Bearer "+nh.secret {
		return true
	}
	nh.record(r, 0)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"invalid access secret"}`))
	return false
}

func (nh *nodeHelper) record(r *http.Request, n int) {
	nh.mu.Lock()
	defer nh.mu.Unlock()
	nh.requests = append(nh.requests, helperRequest{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
		Auth: r.Header.Get("Authorization"), Bytes: n,
	})
}

func (nh *nodeHelper) seen() []helperRequest {
	nh.mu.Lock()
	defer nh.mu.Unlock()
	return append([]helperRequest(nil), nh.requests...)
}

// matching returns every recorded call to one method/path pair.
func (nh *nodeHelper) matching(method, path string) []helperRequest {
	var out []helperRequest
	for _, r := range nh.seen() {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (nh *nodeHelper) importedFor(vmid int) []byte {
	nh.mu.Lock()
	defer nh.mu.Unlock()
	return nh.imported[vmid]
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
	hook     *webhookCollector
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 0. Webhook collector. It is started first so it outlives the server and
	// still answers notifications fired during shutdown.
	hook := &webhookCollector{}
	hookSrv := httptest.NewServer(hook.handler())
	t.Cleanup(hookSrv.Close)
	hook.url = hookSrv.URL + "/proxback"

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
		hook:     hook,
	}
}

// awaitWebhook waits for a delivered payload matching pred. Delivery is
// asynchronous by design, so polling is the only honest way to assert on it.
func (h *harness) awaitWebhook(timeout time.Duration, pred func(webhookPayload) bool) webhookPayload {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for _, p := range h.hook.all() {
			if pred(p) {
				return p
			}
		}
		if !time.Now().Before(deadline) {
			h.t.Fatalf("no matching webhook payload within %s (received %d: %+v)",
				timeout, len(h.hook.all()), h.hook.all())
		}
		time.Sleep(50 * time.Millisecond)
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

// startRun triggers a job and returns the new run id without judging the
// outcome, for the cases where a failure is the expected result.
func (h *harness) startRun(jobID string) string {
	h.t.Helper()
	var started struct {
		RunID string `json:"runId"`
	}
	h.ok(http.MethodPost, "/api/jobs/"+jobID+"/run", nil, &started)
	if started.RunID == "" {
		h.t.Fatal("run trigger returned an empty runId")
	}
	return started.RunID
}

// verify starts a restore point verification and returns the new run id.
func (h *harness) verify(backupID string) string {
	h.t.Helper()
	var started struct {
		RunID string `json:"runId"`
	}
	h.ok(http.MethodPost, "/api/backups/"+backupID+"/verify", nil, &started)
	if started.RunID == "" {
		h.t.Fatal("verify returned an empty runId")
	}
	return started.RunID
}

// runJob triggers a job and waits for it to succeed.
func (h *harness) runJob(jobID string) apiRun {
	h.t.Helper()
	run := h.waitRun(h.startRun(jobID), 90*time.Second)
	if run.Status != "success" {
		h.t.Fatalf("run %s finished %q: %s (step %q)", run.ID, run.Status, run.Error, run.CurrentStep)
	}
	if run.FinishedAt == nil {
		h.t.Fatalf("successful run %s has no finishedAt", run.ID)
	}
	return run
}

// helperCall performs a helper-facing API call the way the real daemon does:
// with its own client (no browser session) and an optional bearer key.
func (h *harness) helperCall(method, path, bearer string, body any) (int, []byte) {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("encode %s %s: %v", method, path, err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.base+path, rdr) //nolint:noctx // test helper
	if err != nil {
		h.t.Fatalf("build %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
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

// registerHelper enrolls a fake node helper through the real registration
// endpoint and returns the status plus the decoded response.
func (h *harness) registerHelper(nh *nodeHelper, token string) (int, struct {
	HelperID string `json:"helperId"`
	APIKey   string `json:"apiKey"`
}) {
	h.t.Helper()
	var out struct {
		HelperID string `json:"helperId"`
		APIKey   string `json:"apiKey"`
	}
	code, raw := h.helperCall(http.MethodPost, "/api/helpers/register", "", map[string]any{
		"token":        token,
		"node":         nh.node,
		"port":         nh.port,
		"version":      "e2e",
		"accessSecret": nh.secret,
	})
	if code == http.StatusOK {
		if err := json.Unmarshal(raw, &out); err != nil {
			h.t.Fatalf("decode registration %s: %v", raw, err)
		}
	}
	return code, out
}

// login re-authenticates the browser session.
func (h *harness) login() {
	h.t.Helper()
	var out struct {
		User apiUser `json:"user"`
	}
	h.ok(http.MethodPost, "/api/login", map[string]string{
		"username": adminUser, "password": adminPass,
	}, &out)
	if out.User.Username != adminUser {
		h.t.Fatalf("re-login returned %+v", out.User)
	}
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

// manifest reads a restore point's manifest object straight off the target so
// the test can reach the chunk hashes the engine recorded.
func (h *harness) manifest(bucket, sourceKind, sourceID, backupID string) engine.Manifest {
	h.t.Helper()
	key := engine.ManifestKey(sourceKind, sourceID, backupID)
	raw, err := h.s3Client(bucket).GetBytes(context.Background(), key)
	if err != nil {
		h.t.Fatalf("read manifest %s: %v", key, err)
	}
	var m engine.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		h.t.Fatalf("decode manifest %s: %v", key, err)
	}
	return m
}

// corruptChunk overwrites a chunk object with garbage of the same length,
// simulating silent bit rot on the storage target.
func (h *harness) corruptChunk(bucket, sha string, size int64) {
	h.t.Helper()
	if err := h.s3Client(bucket).Put(context.Background(), engine.ChunkKey(sha), pseudoBytes(int(size), 0xC0FFEE)); err != nil {
		h.t.Fatalf("corrupt chunk %s: %v", sha, err)
	}
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
		// Proxmox reports tags as one semicolon separated string; the API must
		// surface them as a lower-cased, sorted array — never null.
		assertTags(t, "live", vms)

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
		// Tags survive the round trip through vms_cache.
		assertTags(t, "cached", cached)
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

	// ---- Tag filtered jobs: dynamic membership from the cached inventory -----
	var tagJob apiJob
	t.Run("14-tag-filter-job", func(t *testing.T) {
		// sources may be empty when a tagFilter is set: membership is resolved
		// at run start, so guests tagged later are picked up automatically.
		h.ok(http.MethodPost, "/api/jobs", map[string]any{
			"name":      "prod-tagged",
			"kind":      "vm",
			"targetId":  vmTarget.ID,
			"schedule":  "manual",
			"retention": 2,
			"enabled":   true,
			"sources":   []map[string]any{},
			"tagFilter": "prod",
		}, &tagJob)
		if tagJob.ID == "" || tagJob.TagFilter == nil || *tagJob.TagFilter != "prod" {
			t.Fatalf("tag filtered job = %+v (tagFilter %v)", tagJob, tagJob.TagFilter)
		}
		if len(tagJob.Sources) != 0 {
			t.Fatalf("tag filtered job invented static sources: %+v", tagJob.Sources)
		}

		// A tag filter is meaningless for agent jobs.
		code, body := h.do(http.MethodPost, "/api/jobs", map[string]any{
			"name": "tagged-agent", "kind": "agent", "targetId": agtTarget.ID,
			"sources":   []map[string]any{{"agentId": agentInfo.ID, "paths": []string{srcDir}}},
			"tagFilter": "prod",
		})
		if code != http.StatusBadRequest {
			t.Fatalf("agent job with a tagFilter = %d (%s), want 400", code, body)
		}
		// Without either sources or a tag filter a vm job has no membership.
		code, body = h.do(http.MethodPost, "/api/jobs", map[string]any{
			"name": "empty-vm-job", "kind": "vm", "targetId": vmTarget.ID,
			"sources": []map[string]any{},
		})
		if code != http.StatusBadRequest {
			t.Fatalf("vm job without sources or tagFilter = %d (%s), want 400", code, body)
		}

		// web-01 (prod;web, 2 disks) and db-01 (prod;db, 1 disk) match; the two
		// dev guests must not. Their content is already on the target.
		run := h.runJob(tagJob.ID)
		if run.BytesProcessed != int64(48*mib) {
			t.Fatalf("tag filtered run processed %d bytes, want %d (web-01 + db-01)",
				run.BytesProcessed, 48*mib)
		}
		if run.BytesUploaded != 0 {
			t.Fatalf("tag filtered run uploaded %d bytes, want 0 (all chunks known)", run.BytesUploaded)
		}

		var backups []apiBackup
		h.ok(http.MethodGet, "/api/backups?jobId="+tagJob.ID, nil, &backups)
		if len(backups) != 2 {
			t.Fatalf("tag filtered run produced %d restore points, want 2: %+v", len(backups), backups)
		}
		got := map[string]string{}
		for _, b := range backups {
			got[b.SourceID] = b.SourceName
		}
		for sourceID, name := range map[string]string{
			host.ID + "_100": "web-01",
			host.ID + "_101": "db-01",
		} {
			if got[sourceID] != name {
				t.Fatalf("tag filtered run backed up %v, want %s as %s", got, sourceID, name)
			}
		}

		// A filter that matches nothing fails the run with a clear message
		// rather than silently succeeding with an empty backup.
		var orphan apiJob
		h.ok(http.MethodPost, "/api/jobs", map[string]any{
			"name": "staging-tagged", "kind": "vm", "targetId": vmTarget.ID,
			"schedule": "manual", "retention": 2, "enabled": true,
			"sources": []map[string]any{}, "tagFilter": "staging",
		}, &orphan)
		failed := h.waitRun(h.startRun(orphan.ID), 60*time.Second)
		if failed.Status != "failed" {
			t.Fatalf("run of an unmatched tag filter finished %q", failed.Status)
		}
		if !strings.Contains(failed.Error, `no VMs carry tag "staging"`) {
			t.Fatalf("unmatched tag filter error = %q", failed.Error)
		}
		h.ok(http.MethodDelete, "/api/jobs/"+orphan.ID, nil, nil)

		// Clearing the filter is possible, and then sources are required again.
		var cleared apiJob
		h.ok(http.MethodPatch, "/api/jobs/"+tagJob.ID, map[string]any{
			"tagFilter": "", "sources": []map[string]any{{"hostId": host.ID, "vmid": 100, "name": "web-01"}},
		}, &cleared)
		if cleared.TagFilter != nil {
			t.Fatalf("cleared tagFilter = %v, want null", *cleared.TagFilter)
		}
		// Put it back for the verify subtest's restore points.
		h.ok(http.MethodPatch, "/api/jobs/"+tagJob.ID, map[string]any{
			"tagFilter": "PROD", "sources": []map[string]any{},
		}, &cleared)
		if cleared.TagFilter == nil || *cleared.TagFilter != "prod" {
			t.Fatalf("tagFilter was not normalised to lower case: %v", cleared.TagFilter)
		}
	})

	// ---- Restore point verification ---------------------------------------
	t.Run("15-verify-restore-point", func(t *testing.T) {
		var backups []apiBackup
		h.ok(http.MethodGet, "/api/backups?jobId="+tagJob.ID, nil, &backups)
		var point apiBackup
		for _, b := range backups {
			if b.SourceName == "web-01" {
				point = b
			}
		}
		if point.ID == "" {
			t.Fatalf("no web-01 restore point to verify: %+v", backups)
		}

		run := h.waitRun(h.verify(point.ID), 90*time.Second)
		if run.Status != "success" {
			t.Fatalf("verify of a healthy restore point finished %q: %s", run.Status, run.Error)
		}
		if run.JobName != "Verify web-01" {
			t.Fatalf("verify run jobName = %q, want %q", run.JobName, "Verify web-01")
		}
		if run.JobID != "" {
			t.Fatalf("verify run is attached to job %q, want none", run.JobID)
		}
		if run.BytesProcessed != point.SizeBytes {
			t.Fatalf("verify read %d bytes, want the whole point (%d)", run.BytesProcessed, point.SizeBytes)
		}
		if run.BytesUploaded != 0 {
			t.Fatalf("verify uploaded %d bytes, want 0", run.BytesUploaded)
		}

		// Unknown restore points 404.
		if code, body := h.do(http.MethodPost, "/api/backups/does-not-exist/verify", nil); code != http.StatusNotFound {
			t.Fatalf("verify of an unknown backup = %d (%s), want 404", code, body)
		}

		// Now rot one chunk on the target. The manifest still points at it, so
		// verification must fail on the hash.
		man := h.manifest(vmBucket, point.SourceKind, point.SourceID, point.ID)
		if len(man.Disks) == 0 || len(man.Disks[0].Chunks) == 0 {
			t.Fatalf("manifest has no chunks: %+v", man)
		}
		bad := man.Disks[0].Chunks[0]
		h.corruptChunk(vmBucket, bad.Sha256, bad.Size)

		run = h.waitRun(h.verify(point.ID), 90*time.Second)
		if run.Status != "failed" {
			t.Fatalf("verify of a corrupted restore point finished %q", run.Status)
		}
		if !strings.Contains(run.Error, "chunk hash verification failed") {
			t.Fatalf("verify error = %q, want a hash verification failure", run.Error)
		}
	})

	// ---- Webhook notifications --------------------------------------------
	t.Run("16-webhook-notifications", func(t *testing.T) {
		// Nothing configured yet.
		if code, body := h.do(http.MethodPost, "/api/settings/test-webhook", nil); code != http.StatusBadRequest {
			t.Fatalf("test-webhook without a saved URL = %d (%s), want 400", code, body)
		}
		if code, body := h.do(http.MethodPut, "/api/settings", map[string]any{"notifyOn": "sometimes"}); code != http.StatusBadRequest {
			t.Fatalf("PUT settings with a bogus notifyOn = %d (%s), want 400", code, body)
		}
		if code, body := h.do(http.MethodPut, "/api/settings", map[string]any{"webhookUrl": "ftp://nope"}); code != http.StatusBadRequest {
			t.Fatalf("PUT settings with a non-http webhookUrl = %d (%s), want 400", code, body)
		}

		var settings apiSettings
		h.ok(http.MethodPut, "/api/settings", map[string]any{
			"webhookUrl": h.hook.url, "notifyOn": "all",
		}, &settings)
		if settings.WebhookURL != h.hook.url || settings.NotifyOn != "all" {
			t.Fatalf("saved notification settings = %+v", settings)
		}

		run := h.runJob(vmJob.ID)
		p := h.awaitWebhook(30*time.Second, func(p webhookPayload) bool {
			return p.Job == "nightly-vms" && p.Status == "success"
		})
		if p.Event != "run.finished" {
			t.Fatalf("payload event = %q, want run.finished", p.Event)
		}
		if p.Kind != "vm" {
			t.Fatalf("payload kind = %q, want vm", p.Kind)
		}
		if p.Server != "lab" {
			t.Fatalf("payload server = %q, want the configured serverName", p.Server)
		}
		if p.BytesProcessed != run.BytesProcessed {
			t.Fatalf("payload bytesProcessed = %d, want %d", p.BytesProcessed, run.BytesProcessed)
		}
		if p.StartedAt == "" || p.FinishedAt == "" || p.DurationSec < 0 {
			t.Fatalf("payload timing = %+v", p)
		}
		if p.Error != "" {
			t.Fatalf("successful run reported error %q", p.Error)
		}

		// The test endpoint posts a sample payload to the saved URL.
		var probe struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		h.ok(http.MethodPost, "/api/settings/test-webhook", nil, &probe)
		if !probe.OK {
			t.Fatalf("test-webhook returned ok=false: %s", probe.Error)
		}
		sample := h.awaitWebhook(15*time.Second, func(p webhookPayload) bool { return p.Job == "Webhook test" })
		if sample.Event != "run.finished" || sample.Status != "success" || sample.Server != "lab" {
			t.Fatalf("sample payload = %+v", sample)
		}

		// notifyOn=failures must stay quiet for successes and still report
		// failures.
		h.ok(http.MethodPut, "/api/settings", map[string]any{"notifyOn": "failures"}, &settings)
		if settings.NotifyOn != "failures" {
			t.Fatalf("notifyOn = %q", settings.NotifyOn)
		}
		before := len(h.hook.all())
		h.runJob(vmJob.ID)

		var orphan apiJob
		h.ok(http.MethodPost, "/api/jobs", map[string]any{
			"name": "unmatched-tag", "kind": "vm", "targetId": vmTarget.ID,
			"schedule": "manual", "retention": 2, "enabled": true,
			"sources": []map[string]any{}, "tagFilter": "nowhere",
		}, &orphan)
		failed := h.waitRun(h.startRun(orphan.ID), 60*time.Second)
		if failed.Status != "failed" {
			t.Fatalf("expected the unmatched tag job to fail, got %q", failed.Status)
		}
		fp := h.awaitWebhook(30*time.Second, func(p webhookPayload) bool {
			return p.Job == "unmatched-tag" && p.Status == "failed"
		})
		if fp.Error == "" {
			t.Fatalf("failure payload carries no error: %+v", fp)
		}
		for _, p := range h.hook.all()[before:] {
			if p.Status == "success" {
				t.Fatalf("notifyOn=failures still delivered a success payload: %+v", p)
			}
		}
		h.ok(http.MethodDelete, "/api/jobs/"+orphan.ID, nil, nil)
	})

	// ---- nextRun ----------------------------------------------------------
	t.Run("17-next-run", func(t *testing.T) {
		var jobs []apiJob
		h.ok(http.MethodGet, "/api/jobs", nil, &jobs)
		if len(jobs) == 0 {
			t.Fatal("no jobs to inspect")
		}
		for _, j := range jobs {
			if j.Schedule == "manual" && j.NextRun != nil {
				t.Fatalf("manual job %q reports nextRun %q, want null", j.Name, *j.NextRun)
			}
		}

		var job apiJob
		h.ok(http.MethodPatch, "/api/jobs/"+vmJob.ID, map[string]any{"schedule": "0 2 * * *"}, &job)
		if job.NextRun == nil {
			t.Fatal("a scheduled, enabled job reports a null nextRun")
		}
		next, err := time.Parse(time.RFC3339, *job.NextRun)
		if err != nil {
			t.Fatalf("nextRun %q is not RFC3339: %v", *job.NextRun, err)
		}
		if !next.After(time.Now()) {
			t.Fatalf("nextRun %s is not in the future", next)
		}
		if next.Sub(time.Now()) > 25*time.Hour {
			t.Fatalf("nextRun %s is more than a day out for a daily schedule", next)
		}

		// Disabled jobs never fire, so they report no next run.
		h.ok(http.MethodPatch, "/api/jobs/"+vmJob.ID, map[string]any{"enabled": false}, &job)
		if job.NextRun != nil {
			t.Fatalf("disabled job reports nextRun %q, want null", *job.NextRun)
		}

		// Back to a manual, enabled job for the remaining checks.
		h.ok(http.MethodPatch, "/api/jobs/"+vmJob.ID, map[string]any{
			"schedule": "manual", "enabled": true,
		}, &job)
		if job.NextRun != nil {
			t.Fatalf("manual job reports nextRun %q, want null", *job.NextRun)
		}
		if job.Schedule != "manual" || !job.Enabled {
			t.Fatalf("job not restored to manual/enabled: %+v", job)
		}
		// An unparsable cron spec is rejected outright.
		if code, body := h.do(http.MethodPatch, "/api/jobs/"+vmJob.ID, map[string]any{"schedule": "not a cron"}); code != http.StatusBadRequest {
			t.Fatalf("PATCH with an invalid schedule = %d (%s), want 400", code, body)
		}
	})

	t.Run("18-misc-contract-checks", func(t *testing.T) {
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

	// ---- Node helper: the real-Proxmox agentless path ----------------------
	//
	// mail-01 (#103) lives on pve2 and is untouched by every subtest above, so
	// giving pve2 a helper cannot disturb the extension-backed guests on pve1.
	helper := newNodeHelper(t, "pve2", 12*mib, 0xC0FFEE01)
	var helperKey string
	var helperInfo apiHelper

	t.Run("19-node-helper-enrollment", func(t *testing.T) {
		h.login() // the previous subtest logged out

		var helpers []apiHelper
		h.ok(http.MethodGet, "/api/helpers", nil, &helpers)
		if len(helpers) != 0 {
			t.Fatalf("a fresh install already knows helpers: %+v", helpers)
		}

		var enroll struct {
			Token     string `json:"token"`
			ExpiresAt string `json:"expiresAt"`
		}
		h.ok(http.MethodPost, "/api/helpers/enroll-token", nil, &enroll)
		if enroll.Token == "" || enroll.ExpiresAt == "" {
			t.Fatalf("helper enroll token response = %+v", enroll)
		}

		code, res := h.registerHelper(helper, enroll.Token)
		if code != http.StatusOK {
			t.Fatalf("helper registration = %d", code)
		}
		if res.HelperID == "" || res.APIKey == "" {
			t.Fatalf("helper registration returned %+v", res)
		}
		helperKey = res.APIKey

		// Single use, and an agent token is not a helper token.
		if code, _ := h.registerHelper(helper, enroll.Token); code != http.StatusUnauthorized {
			t.Fatalf("reusing a helper enrollment token = %d, want 401", code)
		}
		var agentEnroll struct {
			Token string `json:"token"`
		}
		h.ok(http.MethodPost, "/api/agents/enroll-token", nil, &agentEnroll)
		if code, _ := h.registerHelper(helper, agentEnroll.Token); code != http.StatusUnauthorized {
			t.Fatalf("an agent enrollment token registered a helper = %d, want 401", code)
		}

		// Heartbeats need the helper's own API key.
		if code, body := h.helperCall(http.MethodPost, "/api/helpers/heartbeat", "", nil); code != http.StatusUnauthorized {
			t.Fatalf("helper heartbeat without a key = %d (%s), want 401", code, body)
		}
		if code, body := h.helperCall(http.MethodPost, "/api/helpers/heartbeat", "not-the-key", nil); code != http.StatusUnauthorized {
			t.Fatalf("helper heartbeat with a wrong key = %d (%s), want 401", code, body)
		}
		if code, body := h.helperCall(http.MethodPost, "/api/helpers/heartbeat", helperKey, map[string]any{}); code != http.StatusOK {
			t.Fatalf("helper heartbeat = %d (%s), want 200", code, body)
		}

		h.ok(http.MethodGet, "/api/helpers", nil, &helpers)
		if len(helpers) != 1 {
			t.Fatalf("helper list = %+v, want exactly one", helpers)
		}
		helperInfo = helpers[0]
		if helperInfo.Node != "pve2" || helperInfo.Port != helper.port {
			t.Fatalf("registered helper = %+v, want node pve2 on port %d", helperInfo, helper.port)
		}
		// The address is learned from the connection, never from the body.
		if helperInfo.Address != "127.0.0.1" {
			t.Fatalf("helper address = %q, want the request's remote IP", helperInfo.Address)
		}
		if helperInfo.Status != "online" || helperInfo.LastSeen == nil {
			t.Fatalf("heartbeating helper reports %+v, want online", helperInfo)
		}
		if helperInfo.Version != "e2e" || helperInfo.RegisteredAt == "" {
			t.Fatalf("helper metadata = %+v", helperInfo)
		}
		if strings.Contains(string(mustJSON(t, helpers)), helper.secret) {
			t.Fatal("helper listing leaked the access secret")
		}

		// The install one-liner downloads the helper binary by this name.
		if err := os.WriteFile(filepath.Join(h.dataDir, "downloads", "proxback-helper-linux-amd64"),
			[]byte("ELF-fake-helper"), 0o644); err != nil {
			t.Fatalf("stage helper download: %v", err)
		}
		code2, body := h.do(http.MethodGet, "/downloads/proxback-helper-linux-amd64", nil)
		if code2 != http.StatusOK || string(body) != "ELF-fake-helper" {
			t.Fatalf("helper download = %d (%s)", code2, body)
		}
	})

	var helperJob apiJob
	var helperPoint apiBackup
	mailSource := host.ID + "_103"

	t.Run("20-helper-backed-vm-backup", func(t *testing.T) {
		h.ok(http.MethodPost, "/api/jobs", map[string]any{
			"name":      "mail-via-helper",
			"kind":      "vm",
			"targetId":  vmTarget.ID,
			"schedule":  "manual",
			"retention": 2,
			"enabled":   true,
			"sources":   []map[string]any{{"hostId": host.ID, "vmid": 103, "name": "mail-01"}},
		}, &helperJob)
		if helperJob.ID == "" {
			t.Fatalf("helper job = %+v", helperJob)
		}

		run := h.runJob(helperJob.ID)
		if run.BytesProcessed != int64(12*mib) {
			t.Fatalf("helper backup processed %d bytes, want the whole %d byte archive",
				run.BytesProcessed, 12*mib)
		}
		if run.BytesUploaded != int64(12*mib) {
			t.Fatalf("first helper backup uploaded %d bytes, want %d", run.BytesUploaded, 12*mib)
		}
		if !strings.Contains(run.CurrentStep, "Completed") {
			t.Fatalf("helper backup finished on step %q", run.CurrentStep)
		}

		// The archive really came from the helper, with the access secret it
		// generated at enrollment.
		exports := helper.matching(http.MethodGet, "/export/103")
		if len(exports) != 1 {
			t.Fatalf("helper saw %d exports of vm 103, want 1: %+v", len(exports), helper.seen())
		}
		if exports[0].Auth != "Bearer "+helper.secret {
			t.Fatalf("export authorization = %q, want the helper's access secret", exports[0].Auth)
		}

		// A whole guest is one manifest stream named "vma".
		var backups []apiBackup
		h.ok(http.MethodGet, "/api/backups?sourceKind=vm&sourceId="+mailSource, nil, &backups)
		if len(backups) != 1 {
			t.Fatalf("mail-01 restore points = %d, want 1", len(backups))
		}
		helperPoint = backups[0]
		if len(helperPoint.Disks) != 1 || helperPoint.Disks[0].Name != "vma" {
			t.Fatalf("helper backup disks = %+v, want exactly [{vma}]", helperPoint.Disks)
		}
		if helperPoint.Disks[0].SizeBytes != int64(12*mib) || helperPoint.SizeBytes != int64(12*mib) {
			t.Fatalf("helper backup sizes = %+v", helperPoint)
		}
		if helperPoint.Kind != "full" || helperPoint.SourceName != "mail-01" {
			t.Fatalf("helper backup metadata = %+v", helperPoint)
		}
		man := h.manifest(vmBucket, helperPoint.SourceKind, helperPoint.SourceID, helperPoint.ID)
		if len(man.Disks) != 1 || man.Disks[0].Name != "vma" || len(man.Disks[0].Chunks) != 3 {
			t.Fatalf("stored manifest = %+v, want one vma stream of 3 chunks", man.Disks)
		}

		// An unchanged re-run still asks the helper for the archive but stores
		// nothing new.
		again := h.runJob(helperJob.ID)
		if again.BytesUploaded != 0 {
			t.Fatalf("unchanged helper re-run uploaded %d bytes, want 0", again.BytesUploaded)
		}
		if again.DedupRatio < 0.999 {
			t.Fatalf("unchanged helper re-run dedupRatio = %v, want ~1", again.DedupRatio)
		}
		if got := len(helper.matching(http.MethodGet, "/export/103")); got != 2 {
			t.Fatalf("helper saw %d exports after the second run, want 2", got)
		}

		// Nodes without a helper are untouched: pve1 guests keep streaming
		// per-disk through the export extension.
		legacy := h.runJob(vmJob.ID)
		if legacy.Status != "success" {
			t.Fatalf("extension-backed job failed after a helper appeared: %s", legacy.Error)
		}
		var web []apiBackup
		h.ok(http.MethodGet, "/api/backups?sourceKind=vm&sourceId="+host.ID+"_100", nil, &web)
		if len(web) == 0 {
			t.Fatal("web-01 has no restore points")
		}
		names := make([]string, 0, len(web[0].Disks))
		for _, d := range web[0].Disks {
			names = append(names, d.Name)
		}
		if strings.Join(names, ",") != "scsi0,scsi1" {
			t.Fatalf("web-01 disks = %v, want the per-disk streams", names)
		}
	})

	t.Run("21-helper-restore-is-byte-identical", func(t *testing.T) {
		var started struct {
			RunID string `json:"runId"`
		}
		// Side by side into a free vmid: no --force, because nothing is being
		// overwritten.
		h.ok(http.MethodPost, "/api/restores", map[string]any{
			"backupId": helperPoint.ID,
			"vm":       map[string]any{"hostId": host.ID, "node": "pve2", "vmid": 9993},
		}, &started)
		run := h.waitRun(started.RunID, 90*time.Second)
		if run.Status != "success" {
			t.Fatalf("helper restore %q: %s", run.Status, run.Error)
		}
		if run.BytesProcessed != int64(12*mib) {
			t.Fatalf("helper restore processed %d bytes, want %d", run.BytesProcessed, 12*mib)
		}

		got := helper.importedFor(9993)
		if len(got) != len(helper.content) {
			t.Fatalf("helper received %d bytes, exported %d", len(got), len(helper.content))
		}
		if !bytes.Equal(got, helper.content) {
			t.Fatal("restored archive differs from the one the helper exported")
		}
		imports := helper.matching(http.MethodPost, "/import/9993")
		if len(imports) != 1 {
			t.Fatalf("helper saw %d imports of 9993: %+v", len(imports), helper.seen())
		}
		if imports[0].Auth != "Bearer "+helper.secret {
			t.Fatalf("import authorization = %q", imports[0].Auth)
		}
		if strings.Contains(imports[0].Query, "force") {
			t.Fatalf("restore into a new vmid passed %q, want no force flag", imports[0].Query)
		}
		if strings.Contains(imports[0].Query, "storage") {
			t.Fatalf("restore without a storage passed %q", imports[0].Query)
		}

		// Restoring onto the source vmid is the overwrite case, and the optional
		// storage override reaches qmrestore.
		h.ok(http.MethodPost, "/api/restores", map[string]any{
			"backupId": helperPoint.ID,
			"vm": map[string]any{
				"hostId": host.ID, "node": "pve2", "vmid": 103, "storage": "local-lvm",
			},
		}, &started)
		run = h.waitRun(started.RunID, 90*time.Second)
		if run.Status != "success" {
			t.Fatalf("in-place helper restore %q: %s", run.Status, run.Error)
		}
		imports = helper.matching(http.MethodPost, "/import/103")
		if len(imports) != 1 {
			t.Fatalf("helper saw %d imports of 103", len(imports))
		}
		if !strings.Contains(imports[0].Query, "force=1") {
			t.Fatalf("in-place restore query = %q, want force=1", imports[0].Query)
		}
		if !strings.Contains(imports[0].Query, "storage=local-lvm") {
			t.Fatalf("in-place restore query = %q, want the storage override", imports[0].Query)
		}
		if !bytes.Equal(helper.importedFor(103), helper.content) {
			t.Fatal("in-place restored archive differs from the exported one")
		}

		// A whole-guest archive cannot be restored to a node without a helper,
		// and the operator is told exactly what to do about it.
		h.ok(http.MethodPost, "/api/restores", map[string]any{
			"backupId": helperPoint.ID,
			"vm":       map[string]any{"hostId": host.ID, "node": "pve1", "vmid": 9994},
		}, &started)
		failed := h.waitRun(started.RunID, 90*time.Second)
		if failed.Status != "failed" {
			t.Fatalf("restore to a helperless node finished %q", failed.Status)
		}
		if !strings.Contains(failed.Error, "no ProxBack node helper installed") ||
			!strings.Contains(failed.Error, `node "pve1"`) {
			t.Fatalf("helperless restore error = %q", failed.Error)
		}

		// Removing the registration is possible, and then the node is helperless
		// again.
		h.ok(http.MethodDelete, "/api/helpers/"+helperInfo.ID, nil, nil)
		var helpers []apiHelper
		h.ok(http.MethodGet, "/api/helpers", nil, &helpers)
		if len(helpers) != 0 {
			t.Fatalf("helper list after delete = %+v", helpers)
		}
		if code, _ := h.do(http.MethodDelete, "/api/helpers/"+helperInfo.ID, nil); code != http.StatusNotFound {
			t.Fatalf("deleting an unknown helper = %d, want 404", code)
		}
		// Its API key stops working immediately.
		if code, _ := h.helperCall(http.MethodPost, "/api/helpers/heartbeat", helperKey, nil); code != http.StatusUnauthorized {
			t.Fatalf("a deleted helper's key still heartbeats = %d, want 401", code)
		}
		// mail-01 falls straight back to the per-disk extension path, which is
		// what the simulator implements. (On a real host the extension answers
		// 501 and the run fails with the install-the-helper message instead —
		// see TestMapExportErrorExplainsAMissingNodeHelper.)
		mail := h.runJob(helperJob.ID)
		if mail.BytesProcessed != int64(16*mib) {
			t.Fatalf("extension-backed mail-01 processed %d bytes, want the 16 MiB disk",
				mail.BytesProcessed)
		}
		var mailPoints []apiBackup
		h.ok(http.MethodGet, "/api/backups?sourceKind=vm&sourceId="+mailSource, nil, &mailPoints)
		if len(mailPoints) == 0 {
			t.Fatal("mail-01 has no restore points")
		}
		if len(mailPoints[0].Disks) != 1 || mailPoints[0].Disks[0].Name != "scsi0" {
			t.Fatalf("newest mail-01 point = %+v, want the per-disk scsi0 stream", mailPoints[0].Disks)
		}
	})

	// ---- Run activity log and history cleanup ------------------------------
	//
	// mail-01's job is the youngest history in the install, so its run is the
	// one whose log and deletion are inspected here.
	t.Run("22-run-activity-log", func(t *testing.T) {
		run := h.runJob(helperJob.ID)

		var got apiRunLog
		h.ok(http.MethodGet, "/api/runs/"+run.ID+"/log", nil, &got)
		if len(got.Lines) == 0 {
			t.Fatal("a successful run produced no activity log")
		}
		var joined []string
		for _, l := range got.Lines {
			if l.Line == "" {
				t.Fatalf("empty log line in %+v", got.Lines)
			}
			if _, err := time.Parse(time.RFC3339, l.TS); err != nil {
				t.Fatalf("log line timestamp %q is not RFC3339: %v", l.TS, err)
			}
			joined = append(joined, l.Line)
		}
		all := strings.Join(joined, "\n")
		// The log names the guest that was backed up …
		if !strings.Contains(all, "mail-01") {
			t.Fatalf("run log never mentions the VM it backed up:\n%s", all)
		}
		// … and ends with the terminal summary.
		if !strings.Contains(joined[len(joined)-1], "run succeeded") {
			t.Fatalf("last log line = %q, want the success summary", joined[len(joined)-1])
		}
		// One line per event, never per chunk.
		if len(joined) > 20 {
			t.Fatalf("a single-VM run logged %d lines:\n%s", len(joined), all)
		}

		// Unknown runs 404 rather than answering with an empty log.
		if code, body := h.do(http.MethodGet, "/api/runs/does-not-exist/log", nil); code != http.StatusNotFound {
			t.Fatalf("log of an unknown run = %d (%s), want 404", code, body)
		}
	})

	t.Run("23-delete-run-keeps-restore-points", func(t *testing.T) {
		run := h.runJob(helperJob.ID)

		var before []apiBackup
		h.ok(http.MethodGet, "/api/backups?sourceKind=vm&sourceId="+mailSource, nil, &before)
		if len(before) == 0 {
			t.Fatal("mail-01 has no restore points to protect")
		}

		h.ok(http.MethodDelete, "/api/runs/"+run.ID, nil, nil)

		// The run is gone from the history and from the run listing.
		if code, body := h.do(http.MethodGet, "/api/runs/"+run.ID, nil); code != http.StatusNotFound {
			t.Fatalf("GET a deleted run = %d (%s), want 404", code, body)
		}
		var runs []apiRun
		h.ok(http.MethodGet, "/api/runs?limit=200", nil, &runs)
		for _, r := range runs {
			if r.ID == run.ID {
				t.Fatalf("deleted run %s still listed in /api/runs", run.ID)
			}
		}
		// Its log went with it.
		if code, body := h.do(http.MethodGet, "/api/runs/"+run.ID+"/log", nil); code != http.StatusNotFound {
			t.Fatalf("log of a deleted run = %d (%s), want 404", code, body)
		}
		// Deleting it twice is a 404, not a silent success.
		if code, _ := h.do(http.MethodDelete, "/api/runs/"+run.ID, nil); code != http.StatusNotFound {
			t.Fatalf("deleting a deleted run = %d, want 404", code)
		}

		// The restore points the run produced are untouched, and still restore.
		var after []apiBackup
		h.ok(http.MethodGet, "/api/backups?sourceKind=vm&sourceId="+mailSource, nil, &after)
		if len(after) != len(before) {
			t.Fatalf("restore points went from %d to %d when a run was deleted", len(before), len(after))
		}
		if after[0].ID != before[0].ID || after[0].SizeBytes != before[0].SizeBytes {
			t.Fatalf("newest restore point changed: %+v vs %+v", after[0], before[0])
		}
		var started struct {
			RunID string `json:"runId"`
		}
		h.ok(http.MethodPost, "/api/restores", map[string]any{
			"backupId": after[0].ID,
			"vm":       map[string]any{"hostId": host.ID, "node": "pve1", "vmid": 9995},
		}, &started)
		restore := h.waitRun(started.RunID, 90*time.Second)
		if restore.Status != "success" {
			t.Fatalf("restore from an orphaned point %q: %s", restore.Status, restore.Error)
		}
		want := h.fetchRaw(fmt.Sprintf("%s/sim/disk/103/scsi0", h.simURL))
		gotBytes := h.fetchRaw(fmt.Sprintf("%s/sim/imported/9995/scsi0", h.simURL))
		if !bytes.Equal(gotBytes, want) {
			t.Fatal("a restore point whose run was deleted no longer restores byte-identically")
		}
	})

	t.Run("24-clear-run-history", func(t *testing.T) {
		var before []apiRun
		h.ok(http.MethodGet, "/api/runs?limit=500", nil, &before)
		if len(before) == 0 {
			t.Fatal("no run history to clear")
		}
		for _, r := range before {
			if r.Status == "running" {
				t.Fatalf("run %s is still running; the clear assertion would be racy", r.ID)
			}
		}

		// A bad scope changes nothing.
		if code, body := h.do(http.MethodPost, "/api/runs/clear", map[string]any{"scope": "everything"}); code != http.StatusBadRequest {
			t.Fatalf("clear with a bogus scope = %d (%s), want 400", code, body)
		}
		var unchanged []apiRun
		h.ok(http.MethodGet, "/api/runs?limit=500", nil, &unchanged)
		if len(unchanged) != len(before) {
			t.Fatalf("a rejected clear removed runs: %d -> %d", len(before), len(unchanged))
		}

		var cleared struct {
			Deleted int `json:"deleted"`
		}
		h.ok(http.MethodPost, "/api/runs/clear", map[string]any{"scope": "finished"}, &cleared)
		if cleared.Deleted != len(before) {
			t.Fatalf("clear reported %d deleted, want %d", cleared.Deleted, len(before))
		}
		var remaining []apiRun
		h.ok(http.MethodGet, "/api/runs?limit=500", nil, &remaining)
		if len(remaining) != 0 {
			t.Fatalf("run history still holds %d runs after a finished clear: %+v", len(remaining), remaining)
		}
		// Clearing history is not data loss: the restore points survive.
		var backups []apiBackup
		h.ok(http.MethodGet, "/api/backups", nil, &backups)
		if len(backups) == 0 {
			t.Fatal("clearing run history destroyed every restore point")
		}
		// And jobs still report no lastRun rather than a dangling one.
		var jobs []apiJob
		h.ok(http.MethodGet, "/api/jobs", nil, &jobs)
		for _, j := range jobs {
			if j.LastRun != nil {
				t.Fatalf("job %q reports lastRun %+v after the history was cleared", j.Name, j.LastRun)
			}
		}
		// A second clear finds nothing left.
		h.ok(http.MethodPost, "/api/runs/clear", map[string]any{"scope": "failed"}, &cleared)
		if cleared.Deleted != 0 {
			t.Fatalf("clearing an empty history deleted %d runs", cleared.Deleted)
		}
	})
}

// ---------------------------------------------------------------- test data

// assertTags checks an inventory listing against the simulator's tag topology.
func assertTags(t *testing.T, what string, vms []apiVM) {
	t.Helper()
	want := map[int][]string{
		100: {"prod", "web"}, // web-01
		101: {"db", "prod"},  // db-01
		102: {"dev"},         // app-01
		103: {"dev", "mail"}, // mail-01
	}
	for _, v := range vms {
		if v.Tags == nil {
			t.Fatalf("%s guest %d (%s) has null tags, want an array", what, v.VMID, v.Name)
		}
		expected, known := want[v.VMID]
		if !known {
			continue
		}
		if strings.Join(v.Tags, ",") != strings.Join(expected, ",") {
			t.Fatalf("%s guest %d (%s) tags = %v, want %v", what, v.VMID, v.Name, v.Tags, expected)
		}
	}
}

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
