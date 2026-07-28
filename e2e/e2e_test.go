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
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
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
	"proxback/internal/sched"
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
	ID   string `json:"id"`
	Name string `json:"name"`
	// Kind is "s3" or "filesystem"; the fields of the other kind stay empty.
	Kind       string `json:"kind"`
	Path       string `json:"path"`
	Endpoint   string `json:"endpoint"`
	Bucket     string `json:"bucket"`
	Region     string `json:"region"`
	PathStyle  bool   `json:"pathStyle"`
	Status     string `json:"status"`
	FreeBytes  int64  `json:"freeBytes"`
	TotalBytes int64  `json:"totalBytes"`
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
	// ReductionPct and ReductionRatio are the one definition of data reduction;
	// the ratio is absent when nothing was uploaded, because it is unbounded.
	ReductionPct   float64         `json:"reductionPct"`
	ReductionRatio *float64        `json:"reductionRatio"`
	Restore        *apiRestoreMeta `json:"restore"`
	Error          string          `json:"error"`
	ProgressPct    float64         `json:"progressPct"`
	CurrentStep    string          `json:"currentStep"`
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

// apiSchedule mirrors the structured schedule object. Every field is decoded so
// the test can prove the server emits exactly the shape the UI is built against.
type apiSchedule struct {
	Kind       string `json:"kind"`
	Minute     *int   `json:"minute"`
	Time       string `json:"time"`
	Weekdays   []int  `json:"weekdays"`
	DayOfMonth *int   `json:"dayOfMonth"`
	Cron       string `json:"cron"`
}

// apiRetention mirrors the GFS retention object a job carries from v0.5.0 on.
// A bare integer is still accepted on the way in; the way out is always this.
type apiRetention struct {
	KeepLast    int `json:"keepLast"`
	KeepDaily   int `json:"keepDaily"`
	KeepWeekly  int `json:"keepWeekly"`
	KeepMonthly int `json:"keepMonthly"`
	KeepYearly  int `json:"keepYearly"`
}

// apiWindow is the wall-clock window a run may start inside.
type apiWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// apiPolicy mirrors a job's protection policy.
type apiPolicy struct {
	Quiesce                 string     `json:"quiesce"`
	ExcludeDisks            []string   `json:"excludeDisks"`
	ExcludePaths            []string   `json:"excludePaths"`
	RetryCount              int        `json:"retryCount"`
	RetryDelayMinutes       int        `json:"retryDelayMinutes"`
	MaxDurationMinutes      int        `json:"maxDurationMinutes"`
	Window                  *apiWindow `json:"window"`
	PreScript               string     `json:"preScript"`
	PostScript              string     `json:"postScript"`
	ScriptTimeoutSeconds    int        `json:"scriptTimeoutSeconds"`
	UploadLimitMbpsOverride int        `json:"uploadLimitMbpsOverride"`
}

// apiRetentionEntry is one restore point in a retention preview.
type apiRetentionEntry struct {
	BackupID  string   `json:"backupId"`
	CreatedAt string   `json:"createdAt"`
	Reasons   []string `json:"reasons"`
}

// apiRetentionPreview is GET /api/jobs/{id}/retention-preview.
type apiRetentionPreview struct {
	Keeps  []apiRetentionEntry `json:"keeps"`
	Prunes []apiRetentionEntry `json:"prunes"`
}

type apiJob struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Kind          string         `json:"kind"`
	TargetID      string         `json:"targetId"`
	TargetName    string         `json:"targetName"`
	Schedule      apiSchedule    `json:"schedule"`
	ScheduleLabel string         `json:"scheduleLabel"`
	Retention     apiRetention   `json:"retention"`
	Policy        apiPolicy      `json:"policy"`
	Enabled       bool           `json:"enabled"`
	Sources       []apiJobSource `json:"sources"`
	TagFilter     *string        `json:"tagFilter"`
	NextRun       *string        `json:"nextRun"`
	LastRun       *apiRun        `json:"lastRun"`
}

// apiRunSource mirrors one entry of GET /api/runs/{id}'s sources array.
type apiRunSource struct {
	Seq            int     `json:"seq"`
	Name           string  `json:"name"`
	Kind           string  `json:"kind"`
	Node           string  `json:"node"`
	Status         string  `json:"status"`
	BytesProcessed int64   `json:"bytesProcessed"`
	BytesUploaded  int64   `json:"bytesUploaded"`
	SizeBytes      int64   `json:"sizeBytes"`
	ProgressPct    float64 `json:"progressPct"`
	StartedAt      *string `json:"startedAt"`
	FinishedAt     *string `json:"finishedAt"`
	Error          string  `json:"error"`
}

// apiRunDetail is GET /api/runs/{id}: a run plus the per-source breakdown that
// drives the visual monitor.
type apiRunDetail struct {
	apiRun
	Sources       []apiRunSource `json:"sources"`
	ThroughputBps float64        `json:"throughputBps"`
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
	HostID        string    `json:"hostId"`
	HostName      string    `json:"hostName"`
	TargetID      string    `json:"targetId"`
	CreatedAt     string    `json:"createdAt"`
	SizeBytes     int64     `json:"sizeBytes"`
	UploadedBytes int64     `json:"uploadedBytes"`
	Kind          string    `json:"kind"`
	ParentID      string    `json:"parentId"`
	Disks         []apiDisk `json:"disks"`
	// Verification evidence: integrity only, never a claim about restorability.
	LastVerifiedAt   *string `json:"lastVerifiedAt"`
	LastVerifyResult string  `json:"lastVerifyResult"`
	VerifiedBytes    int64   `json:"verifiedBytes"`
}

// apiRestoreMeta is the persisted destination of a restore run.
type apiRestoreMeta struct {
	Mode     string `json:"mode"`
	HostID   string `json:"hostId"`
	HostName string `json:"hostName"`
	Node     string `json:"node"`
	VMID     int    `json:"vmid"`
	Storage  string `json:"storage"`
	AgentID  string `json:"agentId"`
	DestPath string `json:"destPath"`
}

// apiPosture mirrors GET /api/posture.
type apiPosture struct {
	Verdict string `json:"verdict"`
	Counts  struct {
		Protected   int `json:"protected"`
		AtRisk      int `json:"atRisk"`
		Unprotected int `json:"unprotected"`
	} `json:"counts"`
	Reasons []struct {
		Code      string `json:"code"`
		Workloads int    `json:"workloads"`
		Detail    string `json:"detail"`
	} `json:"reasons"`
	Workloads []apiPostureWorkload `json:"workloads"`
}

type apiPostureWorkload struct {
	Kind           string   `json:"kind"`
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	HostName       string   `json:"hostName"`
	Node           string   `json:"node"`
	Policy         string   `json:"policy"`
	Enabled        bool     `json:"enabled"`
	RPOHours       *float64 `json:"rpoHours"`
	LastSuccessAt  *string  `json:"lastSuccessAt"`
	AgeHours       *float64 `json:"ageHours"`
	WithinRPO      *bool    `json:"withinRpo"`
	LastFailureAt  *string  `json:"lastFailureAt"`
	LastVerifiedAt *string  `json:"lastVerifiedAt"`
	RestorePoints  int      `json:"restorePoints"`
	Status         string   `json:"status"`
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
	HostID       string  `json:"hostId"`
	HostName     string  `json:"hostName"`
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
	ServerName        string `json:"serverName"`
	Concurrency       int    `json:"concurrency"`
	WebhookURL        string `json:"webhookUrl"`
	NotifyOn          string `json:"notifyOn"`
	UploadConcurrency int    `json:"uploadConcurrency"`
	Compression       string `json:"compression"`
	UploadLimitMbps   int    `json:"uploadLimitMbps"`
	// Timezone is read-only: the zone the server's schedules are expressed in.
	Timezone string `json:"timezone"`
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
	srv     *httptest.Server

	mu       sync.Mutex
	requests []helperRequest
	imported map[int][]byte
	scripts  []helperScript
	// scriptFailures maps a phase ("pre"/"post") to the output a failing script
	// of that phase reports.
	scriptFailures map[string]string
}

// helperScript is one policy script the helper was asked to run.
type helperScript struct {
	Script         string `json:"script"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
	Phase          string `json:"phase"`
}

func newNodeHelper(t *testing.T, node string, size int, seed uint64) *nodeHelper {
	t.Helper()
	nh := &nodeHelper{
		node: node,
		// The secret is per instance, not per node name: two clusters can each
		// contain a "pve1", and the whole point of these tests is telling them
		// apart.
		secret:         fmt.Sprintf("e2e-helper-access-secret-%s-%x", node, seed),
		content:        pseudoBytes(size, seed),
		imported:       map[int][]byte{},
		scriptFailures: map[string]string{},
	}
	srv := httptest.NewServer(nh.handler())
	t.Cleanup(srv.Close)
	nh.srv = srv
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
	// Policy scripts run where the data lives, so a vm job's pre/post scripts
	// are executed by the helper on the node. The stand-in records what it was
	// asked to run and can be told to report a non-zero exit.
	mux.HandleFunc("/script", func(w http.ResponseWriter, r *http.Request) {
		if !nh.authorized(w, r) {
			return
		}
		var call helperScript
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		nh.record(r, len(call.Script))
		nh.mu.Lock()
		nh.scripts = append(nh.scripts, call)
		failure, failing := nh.scriptFailures[call.Phase]
		nh.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if failing {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": false, "output": failure, "error": "exit status 1",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "output": "ran " + call.Script + " on " + nh.node,
		})
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

// scriptsRun returns the policy scripts the helper was asked to execute.
func (nh *nodeHelper) scriptsRun() []helperScript {
	nh.mu.Lock()
	defer nh.mu.Unlock()
	return append([]helperScript(nil), nh.scripts...)
}

// failScript makes the given phase report a non-zero exit with output.
func (nh *nodeHelper) failScript(phase, output string) {
	nh.mu.Lock()
	defer nh.mu.Unlock()
	nh.scriptFailures[phase] = output
}

// clearScriptFailures puts the helper back to running scripts successfully.
func (nh *nodeHelper) clearScriptFailures() {
	nh.mu.Lock()
	defer nh.mu.Unlock()
	nh.scriptFailures = map[string]string{}
}

// stop takes the helper off the network, the way a node that has been shut down
// or lost its route behaves. Calls to it then fail rather than hang.
func (nh *nodeHelper) stop() { nh.srv.Close() }

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
	//
	// The server reconciles its staged agent and node helper binaries against the
	// release repository on startup. Point that at a repository with no releases:
	// a test must never depend on the internet, and this is also the air-gapped
	// path — the staged binaries are left exactly as the test finds them, which is
	// what the download subtests below assert on.
	noReleases := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(noReleases.Close)
	t.Setenv("PROXBACK_UPDATE_API", noReleases.URL)

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

// helperEnrollToken mints a helper enrollment token for one Proxmox host. The
// host travels with the token, which is how the registering helper learns which
// cluster its node belongs to.
func (h *harness) helperEnrollToken(hostID string) string {
	h.t.Helper()
	var out struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expiresAt"`
	}
	h.ok(http.MethodPost, "/api/helpers/enroll-token", map[string]any{"hostId": hostID}, &out)
	if out.Token == "" || out.ExpiresAt == "" {
		h.t.Fatalf("helper enroll token response = %+v", out)
	}
	return out.Token
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

// backupByID re-reads one restore point through the listing endpoint, which is
// the only way the API exposes a single point.
func (h *harness) backupByID(sourceKind, sourceID, id string) apiBackup {
	h.t.Helper()
	var points []apiBackup
	h.ok(http.MethodGet, "/api/backups?sourceKind="+sourceKind+"&sourceId="+sourceID, nil, &points)
	for _, p := range points {
		if p.ID == id {
			return p
		}
	}
	h.t.Fatalf("restore point %s not in %+v", id, points)
	return apiBackup{}
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
	// Orphan chunk collection spares chunks younger than a day so an interrupted
	// backup stays resumable. Every chunk in a test run is seconds old, so the
	// window is switched off here and the suite keeps asserting on collection;
	// the window itself is covered in internal/engine and internal/sched.
	t.Setenv(sched.GCGraceEnv, "0")
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
		// The job was created with a bare integer retention, which every older
		// client still sends; it reads back as the GFS object that means the
		// same thing, and the job carries the default protection policy.
		if vmJob.TargetName != "vm-storage" || vmJob.Schedule.Kind != "manual" {
			t.Fatalf("job fields = %+v", vmJob)
		}
		if (vmJob.Retention != apiRetention{KeepLast: 2}) {
			t.Fatalf("job retention = %+v, want keepLast 2 and nothing else", vmJob.Retention)
		}
		if vmJob.Policy.Quiesce != "none" || vmJob.Policy.RetryDelayMinutes != 5 ||
			vmJob.Policy.ScriptTimeoutSeconds != 30 || vmJob.Policy.Window != nil ||
			len(vmJob.Policy.ExcludeDisks) != 0 {
			t.Fatalf("job policy = %+v, want the defaults", vmJob.Policy)
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
		// Restoring onto the guest it came from destroys that guest, so it is an
		// overwrite and needs the destination's current name typed back. A
		// request that says nothing is refused rather than silently obeyed.
		code, body := h.do(http.MethodPost, "/api/restores", map[string]any{
			"backupId": newest.ID,
			"vm":       map[string]any{"hostId": host.ID, "node": "pve1", "vmid": 100},
		})
		if code != http.StatusConflict {
			t.Fatalf("restore onto the live guest without a mode = %d (%s), want 409", code, body)
		}
		if !strings.Contains(string(body), "already exists") {
			t.Fatalf("refusal = %s", body)
		}
		h.ok(http.MethodPost, "/api/restores", map[string]any{
			"backupId":    newest.ID,
			"mode":        "overwrite",
			"confirmName": "web-01",
			"vm":          map[string]any{"hostId": host.ID, "node": "pve1", "vmid": 100},
		}, &started)
		run := h.waitRun(started.RunID, 90*time.Second)
		if run.Status != "success" {
			t.Fatalf("restore run %q: %s", run.Status, run.Error)
		}
		// The destination is history, not just a log line.
		if run.Restore == nil || run.Restore.Mode != "overwrite" || run.Restore.VMID != 100 ||
			run.Restore.Node != "pve1" || run.Restore.HostName != "pve-sim" {
			t.Fatalf("restore run destination = %+v", run.Restore)
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
		// closed-pipe error masked it). No mode is given, so it is alongside —
		// the safe default.
		var free struct {
			VMID int `json:"vmid"`
		}
		h.ok(http.MethodGet, "/api/hosts/"+host.ID+"/free-vmid", nil, &free)
		if free.VMID != 104 {
			t.Fatalf("free vmid = %d, want the first gap above the simulator's guests", free.VMID)
		}
		h.ok(http.MethodGet, "/api/hosts/"+host.ID+"/free-vmid?after=9000", nil, &free)
		if free.VMID != 9000 {
			t.Fatalf("free vmid after 9000 = %d", free.VMID)
		}
		h.ok(http.MethodPost, "/api/restores", map[string]any{
			"backupId": newest.ID,
			"vm":       map[string]any{"hostId": host.ID, "node": "pve1", "vmid": 9999},
		}, &started)
		run = h.waitRun(started.RunID, 90*time.Second)
		if run.Status != "success" {
			t.Fatalf("side-by-side restore run %q: %s", run.Status, run.Error)
		}
		if run.Restore == nil || run.Restore.Mode != "alongside" || run.Restore.VMID != 9999 {
			t.Fatalf("a restore without a mode recorded %+v, want alongside", run.Restore)
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
		// Nothing can deduplicate on a first backup, so the uploaded figure is the
		// processed one minus whatever zstd saved. The payload is pseudo-random and
		// therefore incompressible apart from the tar's zero padding, so the two
		// stay within a percent of each other.
		if full.BytesUploaded > full.BytesProcessed {
			t.Fatalf("first agent backup uploaded %d bytes for %d processed",
				full.BytesUploaded, full.BytesProcessed)
		}
		if full.BytesUploaded < full.BytesProcessed/100*99 {
			t.Fatalf("first agent backup uploaded only %d of %d bytes; nothing should have deduplicated",
				full.BytesUploaded, full.BytesProcessed)
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

	/*
		Recovering one file out of a restore point, without restoring anything.

		This is the whole chain against a real backup on a real target: the stored
		chunks are indexed into a file listing, one file is located by name, and its
		bytes come back byte-for-byte — while the 12 MiB file sitting next to it in
		the same archive is never read.
	*/
	t.Run("11b-browse-and-recover-a-single-file", func(t *testing.T) {
		var backups []apiBackup
		h.ok(http.MethodGet, "/api/backups?sourceKind=agent&sourceId="+agentInfo.ID, nil, &backups)
		if len(backups) == 0 {
			t.Fatal("no agent restore point to browse")
		}
		point := backups[0].ID

		// The archive root is an absolute temp path, so the file is found by name
		// rather than by a path this test would have to reconstruct.
		var found struct {
			Entries []struct {
				Name string `json:"name"`
				Path string `json:"path"`
				Size int64  `json:"size"`
				Dir  bool   `json:"dir"`
			} `json:"entries"`
		}
		h.ok(http.MethodGet, "/api/backups/"+point+"/files?search=notes.txt", nil, &found)
		if len(found.Entries) != 1 {
			t.Fatalf("search for notes.txt returned %d entries, want 1: %+v", len(found.Entries), found.Entries)
		}
		hit := found.Entries[0]
		if hit.Dir || hit.Name != "notes.txt" {
			t.Fatalf("search hit = %+v", hit)
		}
		const want = "ProxBack agent backup test tree\n"
		if hit.Size != int64(len(want)) {
			t.Fatalf("notes.txt listed as %d bytes, want %d", hit.Size, len(want))
		}

		// Walking to its folder must show it too, so browsing and search agree.
		parent := path.Dir(hit.Path)
		var listing struct {
			Path    string `json:"path"`
			Entries []struct {
				Name string `json:"name"`
				Dir  bool   `json:"dir"`
			} `json:"entries"`
		}
		h.ok(http.MethodGet, "/api/backups/"+point+"/files?path="+url.QueryEscape(parent), nil, &listing)
		var names []string
		for _, e := range listing.Entries {
			names = append(names, e.Name)
		}
		if !slices.Contains(names, "notes.txt") {
			t.Fatalf("listing of %s = %v, want it to contain notes.txt", parent, names)
		}

		// And the bytes themselves.
		code, body := h.do(http.MethodGet,
			"/api/backups/"+point+"/files/download?path="+url.QueryEscape(hit.Path), nil)
		if code != http.StatusOK {
			t.Fatalf("download = %d: %s", code, body)
		}
		if string(body) != want {
			t.Fatalf("recovered %q, want %q", body, want)
		}

		// A folder is not a file, and a path that is not in the point is a 404
		// rather than an empty download that looks like an empty file.
		if code, _ := h.do(http.MethodGet,
			"/api/backups/"+point+"/files/download?path="+url.QueryEscape(parent), nil); code == http.StatusOK {
			t.Fatal("downloading a folder succeeded")
		}
		if code, _ := h.do(http.MethodGet,
			"/api/backups/"+point+"/files/download?path=no/such/file", nil); code != http.StatusNotFound {
			t.Fatalf("downloading a missing file = %d, want 404", code)
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

		// v0.3.2 throughput settings: present with their defaults on a database
		// that has never stored them, validated on PUT, and safe to change
		// between runs.
		if settings.UploadConcurrency != 4 || settings.Compression != "zstd" || settings.UploadLimitMbps != 0 {
			t.Fatalf("throughput defaults = %+v, want 4 / zstd / 0", settings)
		}
		h.ok(http.MethodPut, "/api/settings", map[string]any{
			"uploadConcurrency": 8, "compression": "off", "uploadLimitMbps": 500,
		}, &settings)
		if settings.UploadConcurrency != 8 || settings.Compression != "off" || settings.UploadLimitMbps != 500 {
			t.Fatalf("updated throughput settings = %+v", settings)
		}
		for _, bad := range []map[string]any{
			{"uploadConcurrency": 0},
			{"uploadConcurrency": 17},
			{"uploadLimitMbps": -1},
			{"uploadLimitMbps": 10001},
			{"compression": "gzip"},
		} {
			if code, body := h.do(http.MethodPut, "/api/settings", bad); code != http.StatusBadRequest {
				t.Fatalf("PUT settings %v = %d (%s), want 400", bad, code, body)
			}
		}
		h.ok(http.MethodGet, "/api/settings", nil, &settings)
		if settings.UploadConcurrency != 8 || settings.Compression != "off" || settings.UploadLimitMbps != 500 {
			t.Fatalf("a rejected PUT changed the stored settings: %+v", settings)
		}
		// Back to the shipped defaults for the runs that follow.
		h.ok(http.MethodPut, "/api/settings", map[string]any{
			"uploadConcurrency": 4, "compression": "zstd", "uploadLimitMbps": 0,
		}, &settings)
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

		// The run detail carries the per-object breakdown the visual monitor
		// draws: one entry per guest the tag resolved to, each finished.
		var detail apiRunDetail
		h.ok(http.MethodGet, "/api/runs/"+run.ID, nil, &detail)
		if detail.ID != run.ID || detail.Status != "success" {
			t.Fatalf("run detail = %+v", detail.apiRun)
		}
		if len(detail.Sources) != 2 {
			t.Fatalf("run detail exposes %d sources, want 2: %+v", len(detail.Sources), detail.Sources)
		}
		if detail.ThroughputBps != 0 {
			t.Errorf("a finished run reports throughputBps %v, want 0", detail.ThroughputBps)
		}
		bySource := map[string]apiRunSource{}
		for i, src := range detail.Sources {
			if src.Seq != i {
				t.Errorf("source %d carries seq %d", i, src.Seq)
			}
			bySource[src.Name] = src
		}
		for name, size := range map[string]int64{
			"web-01": 32 * mib, // two disks
			"db-01":  16 * mib, // one
		} {
			src, ok := bySource[name]
			if !ok {
				t.Fatalf("run detail has no source for %s: %+v", name, detail.Sources)
			}
			if src.Status != "success" {
				t.Errorf("%s finished %q (%s), want success", name, src.Status, src.Error)
			}
			if src.Kind != "vm" || src.Node == "" {
				t.Errorf("%s = kind %q on node %q", name, src.Kind, src.Node)
			}
			if src.SizeBytes != size || src.BytesProcessed != size {
				t.Errorf("%s = %d of %d bytes, want %d of %d",
					name, src.BytesProcessed, src.SizeBytes, size, size)
			}
			// Everything this run read was already on the target.
			if src.BytesUploaded != 0 {
				t.Errorf("%s uploaded %d bytes, want 0 (all chunks known)", name, src.BytesUploaded)
			}
			if src.ProgressPct != 100 {
				t.Errorf("%s progressPct = %v, want 100", name, src.ProgressPct)
			}
			if src.StartedAt == nil || src.FinishedAt == nil || src.Error != "" {
				t.Errorf("%s timing/error = %v … %v (%q)", name, src.StartedAt, src.FinishedAt, src.Error)
			}
		}
		// The list stays cheap: the breakdown is only on the detail endpoint.
		_, rawList := h.do(http.MethodGet, "/api/runs?limit=5", nil)
		if strings.Contains(string(rawList), `"sources"`) {
			t.Errorf("GET /api/runs carries the per-source breakdown: %s", rawList)
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
		// The evidence lands on the restore point, not only in run history — and
		// it is evidence of integrity, which is all a verification can prove.
		verified := h.backupByID(point.SourceKind, point.SourceID, point.ID)
		if verified.LastVerifyResult != "passed" || verified.LastVerifiedAt == nil {
			t.Fatalf("verified restore point = %+v", verified)
		}
		if verified.VerifiedBytes != point.SizeBytes {
			t.Fatalf("verifiedBytes = %d, want the %d bytes it re-read",
				verified.VerifiedBytes, point.SizeBytes)
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
		// The failure is recorded on the point too, so the UI never shows a
		// stale "passed" beside data that no longer hashes.
		rotten := h.backupByID(point.SourceKind, point.SourceID, point.ID)
		if rotten.LastVerifyResult != "failed" || rotten.LastVerifiedAt == nil {
			t.Fatalf("corrupted restore point = %+v, want a recorded failure", rotten)
		}
		if rotten.VerifiedBytes != 0 {
			t.Fatalf("a failed verification claims %d verified bytes", rotten.VerifiedBytes)
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
			if j.Schedule.Kind == "manual" && j.NextRun != nil {
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
		if job.Schedule.Kind != "manual" || !job.Enabled {
			t.Fatalf("job not restored to manual/enabled: %+v", job)
		}
		// An unparsable cron spec is rejected outright.
		if code, body := h.do(http.MethodPatch, "/api/jobs/"+vmJob.ID, map[string]any{"schedule": "not a cron"}); code != http.StatusBadRequest {
			t.Fatalf("PATCH with an invalid schedule = %d (%s), want 400", code, body)
		}
	})

	// ---- structured schedules ---------------------------------------------
	t.Run("17b-structured-schedule", func(t *testing.T) {
		// The shape the schedule picker sends: no cron anywhere in sight.
		var daily apiJob
		h.ok(http.MethodPost, "/api/jobs", map[string]any{
			"name": "daily-vms", "kind": "vm", "targetId": vmTarget.ID,
			"schedule":  map[string]any{"kind": "daily", "time": "02:00"},
			"retention": 2, "enabled": true,
			"sources": []map[string]any{{"hostId": host.ID, "vmid": 100, "name": "web-01"}},
		}, &daily)
		if daily.Schedule.Kind != "daily" || daily.Schedule.Time != "02:00" {
			t.Fatalf("schedule round trip = %+v", daily.Schedule)
		}
		if daily.Schedule.Cron != "" || daily.Schedule.Minute != nil || daily.Schedule.DayOfMonth != nil {
			t.Fatalf("a daily schedule carries fields of other kinds: %+v", daily.Schedule)
		}
		if daily.ScheduleLabel != "Daily at 02:00" {
			t.Fatalf("scheduleLabel = %q, want %q", daily.ScheduleLabel, "Daily at 02:00")
		}
		if daily.NextRun == nil {
			t.Fatal("a daily schedule reports no nextRun")
		}
		next, err := time.Parse(time.RFC3339, *daily.NextRun)
		if err != nil {
			t.Fatalf("nextRun %q is not RFC3339: %v", *daily.NextRun, err)
		}
		if !next.After(time.Now()) {
			t.Fatalf("nextRun %s is not in the future", next)
		}
		if next.Sub(time.Now()) > 25*time.Hour {
			t.Fatalf("nextRun %s is more than a day out for a daily schedule", next)
		}
		if local := next.Local(); local.Hour() != 2 || local.Minute() != 0 {
			t.Fatalf("nextRun %s is not 02:00 in the server's zone", local)
		}

		// It survives a reload, and the other kinds render their labels too.
		var jobs []apiJob
		h.ok(http.MethodGet, "/api/jobs", nil, &jobs)
		var found bool
		for _, j := range jobs {
			if j.ID != daily.ID {
				continue
			}
			found = true
			if j.Schedule.Kind != "daily" || j.Schedule.Time != "02:00" || j.ScheduleLabel != "Daily at 02:00" {
				t.Fatalf("listed job = %+v / %q", j.Schedule, j.ScheduleLabel)
			}
		}
		if !found {
			t.Fatalf("the scheduled job is missing from the listing")
		}

		var weekly apiJob
		h.ok(http.MethodPatch, "/api/jobs/"+daily.ID, map[string]any{
			"schedule": map[string]any{"kind": "weekly", "time": "03:00", "weekdays": []int{0, 6}},
		}, &weekly)
		if weekly.ScheduleLabel != "Weekly on Sun, Sat at 03:00" {
			t.Fatalf("weekly label = %q", weekly.ScheduleLabel)
		}
		if len(weekly.Schedule.Weekdays) != 2 || weekly.Schedule.Weekdays[0] != 0 || weekly.Schedule.Weekdays[1] != 6 {
			t.Fatalf("weekdays = %v", weekly.Schedule.Weekdays)
		}

		// The server's timezone is reported so the UI can say what "02:00" means.
		var settings apiSettings
		h.ok(http.MethodGet, "/api/settings", nil, &settings)
		if settings.Timezone == "" {
			t.Fatal("GET /api/settings reports no timezone")
		}

		// A schedule the scheduler could not honour never reaches the database.
		for _, bad := range []any{
			map[string]any{"kind": "daily", "time": "25:00"},
			map[string]any{"kind": "weekly", "time": "03:00", "weekdays": []int{}},
			map[string]any{"kind": "monthly", "time": "01:00", "dayOfMonth": 32},
			map[string]any{"kind": "advanced", "cron": "not a cron"},
			map[string]any{"kind": "yearly"},
		} {
			code, body := h.do(http.MethodPatch, "/api/jobs/"+daily.ID, map[string]any{"schedule": bad})
			if code != http.StatusBadRequest {
				t.Fatalf("PATCH with schedule %v = %d (%s), want 400", bad, code, body)
			}
		}
		h.ok(http.MethodDelete, "/api/jobs/"+daily.ID, nil, nil)
	})

	// ---- retry -------------------------------------------------------------
	t.Run("17c-retry-a-run", func(t *testing.T) {
		var runs []apiRun
		h.ok(http.MethodGet, "/api/runs?jobId="+vmJob.ID+"&limit=1", nil, &runs)
		if len(runs) == 0 {
			t.Fatal("the vm job has no runs to retry")
		}
		var started struct {
			RunID string `json:"runId"`
		}
		h.ok(http.MethodPost, "/api/runs/"+runs[0].ID+"/retry", nil, &started)
		if started.RunID == "" || started.RunID == runs[0].ID {
			t.Fatalf("retry returned runId %q", started.RunID)
		}
		// While that run is in flight the job refuses another. The retry may
		// already have finished on a fast machine, in which case a second one is
		// legitimately accepted — and then has to be waited for as well, so it
		// cannot collide with the subtests that follow.
		code, body := h.do(http.MethodPost, "/api/runs/"+runs[0].ID+"/retry", nil)
		switch code {
		case http.StatusConflict:
		case http.StatusOK:
			var second struct {
				RunID string `json:"runId"`
			}
			if err := json.Unmarshal(body, &second); err != nil {
				t.Fatalf("decode second retry %s: %v", body, err)
			}
			h.waitRun(second.RunID, 60*time.Second)
		default:
			t.Fatalf("second retry = %d (%s), want 409 or 200", code, body)
		}
		retried := h.waitRun(started.RunID, 60*time.Second)
		if retried.JobID != vmJob.ID || retried.Status != "success" {
			t.Fatalf("retry run = %+v", retried)
		}
		// A restore run has no job behind it, so there is nothing to re-run.
		var restores []apiRun
		h.ok(http.MethodGet, "/api/runs?limit=100", nil, &restores)
		for _, r := range restores {
			if r.JobID != "" {
				continue
			}
			if code, body := h.do(http.MethodPost, "/api/runs/"+r.ID+"/retry", nil); code != http.StatusNotFound {
				t.Fatalf("retry of the jobless run %q = %d (%s), want 404", r.JobName, code, body)
			}
			break
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

		// A helper enrollment token is minted for a host. Without one there is no
		// token: the resulting registration could not be routed to, because a
		// bare node name does not say which cluster's machine it is.
		for _, bad := range []any{map[string]any{}, map[string]any{"hostId": "no-such-host"}} {
			if code, body := h.do(http.MethodPost, "/api/helpers/enroll-token", bad); code != http.StatusBadRequest {
				t.Fatalf("enroll-token %v = %d (%s), want 400", bad, code, body)
			}
		}
		token := h.helperEnrollToken(host.ID)

		code, res := h.registerHelper(helper, token)
		if code != http.StatusOK {
			t.Fatalf("helper registration = %d", code)
		}
		if res.HelperID == "" || res.APIKey == "" {
			t.Fatalf("helper registration returned %+v", res)
		}
		helperKey = res.APIKey

		// Single use, and an agent token is not a helper token.
		if code, _ := h.registerHelper(helper, token); code != http.StatusUnauthorized {
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
		// The helper inherited the cluster from the token it enrolled with.
		if helperInfo.HostID != host.ID || helperInfo.HostName != "pve-sim" {
			t.Fatalf("registered helper host = %q/%q, want the host the token was minted for",
				helperInfo.HostID, helperInfo.HostName)
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

		// Restoring onto the source vmid is the overwrite case: it needs the
		// destination guest's current name, and is refused without it.
		code, body := h.do(http.MethodPost, "/api/restores", map[string]any{
			"backupId": helperPoint.ID,
			"mode":     "overwrite",
			"vm": map[string]any{
				"hostId": host.ID, "node": "pve2", "vmid": 103, "storage": "local-lvm",
			},
		})
		if code != http.StatusConflict {
			t.Fatalf("overwrite without a confirmName = %d (%s), want 409", code, body)
		}
		// The refusal names the guest, so the operator knows what to type.
		if !strings.Contains(string(body), "mail-01") {
			t.Fatalf("refusal %s does not say what to type", body)
		}
		if code, body := h.do(http.MethodPost, "/api/restores", map[string]any{
			"backupId": helperPoint.ID, "mode": "overwrite", "confirmName": "not-mail-01",
			"vm": map[string]any{"hostId": host.ID, "node": "pve2", "vmid": 103},
		}); code != http.StatusConflict {
			t.Fatalf("overwrite with the wrong confirmName = %d (%s), want 409", code, body)
		}
		// With the right name it goes through, and the optional storage override
		// reaches qmrestore.
		h.ok(http.MethodPost, "/api/restores", map[string]any{
			"backupId":    helperPoint.ID,
			"mode":        "overwrite",
			"confirmName": "mail-01",
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

	// ---- v0.3.2: compressed, concurrent uploads still restore byte for byte --
	t.Run("25-compressed-parallel-backup-and-restore", func(t *testing.T) {
		// The simulator's disks are pseudo-random and therefore incompressible, so
		// the compressible source is a real agent payload: log-like text files, the
		// kind of content a file backup is mostly made of.
		compSrc := filepath.Join(t.TempDir(), "compressible")
		compDest := filepath.Join(t.TempDir(), "compressible-restored")
		writeCompressibleTree(t, compSrc)

		var settings apiSettings
		h.ok(http.MethodPut, "/api/settings", map[string]any{
			"compression": "zstd", "uploadConcurrency": 4, "uploadLimitMbps": 0,
		}, &settings)
		if settings.Compression != "zstd" || settings.UploadConcurrency != 4 {
			t.Fatalf("settings for the compressed run = %+v", settings)
		}

		var job apiJob
		h.ok(http.MethodPost, "/api/jobs", map[string]any{
			"name":      "compressible-files",
			"kind":      "agent",
			"targetId":  agtTarget.ID,
			"schedule":  "manual",
			"retention": 2,
			"enabled":   true,
			"sources":   []map[string]any{{"agentId": agentInfo.ID, "paths": []string{compSrc}}},
		}, &job)

		run := h.runJob(job.ID)
		if run.Status != "success" {
			t.Fatalf("compressed backup %q: %s", run.Status, run.Error)
		}
		if run.BytesProcessed < 16*mib {
			t.Fatalf("compressed backup processed %d bytes, want the whole tree", run.BytesProcessed)
		}
		// The whole point of compression: fewer bytes on the wire than off the
		// disk. Nothing deduplicates here — this content has never been backed up.
		if run.BytesUploaded >= run.BytesProcessed {
			t.Fatalf("compressed backup uploaded %d bytes for %d processed; compression did nothing",
				run.BytesUploaded, run.BytesProcessed)
		}
		if run.BytesUploaded > run.BytesProcessed/2 {
			t.Fatalf("compressed backup uploaded %d of %d bytes; expected compressible content to shrink far more",
				run.BytesUploaded, run.BytesProcessed)
		}
		t.Logf("compressed run: %d bytes processed, %d uploaded (%.1f%% of the wire saved)",
			run.BytesProcessed, run.BytesUploaded,
			100*(1-float64(run.BytesUploaded)/float64(run.BytesProcessed)))

		// The restore point is a normal one, and it restores byte-identically.
		var backups []apiBackup
		h.ok(http.MethodGet, "/api/backups?sourceKind=agent&sourceId="+agentInfo.ID, nil, &backups)
		if len(backups) == 0 {
			t.Fatal("the compressed run produced no restore point")
		}
		point := backups[0]
		if point.SizeBytes != run.BytesProcessed {
			t.Fatalf("restore point holds %d bytes, the run processed %d", point.SizeBytes, run.BytesProcessed)
		}

		var started struct {
			RunID string `json:"runId"`
		}
		h.ok(http.MethodPost, "/api/restores", map[string]any{
			"backupId": point.ID,
			"agent":    map[string]any{"agentId": agentInfo.ID, "destPath": compDest},
		}, &started)
		restore := h.waitRun(started.RunID, 90*time.Second)
		if restore.Status != "success" {
			t.Fatalf("restore of a compressed backup %q: %s", restore.Status, restore.Error)
		}
		diffTrees(t, compSrc, filepath.Join(compDest, filepath.Base(compSrc)))

		// Verification decompresses through the same path.
		verifyRun := h.verify(point.ID)
		if v := h.waitRun(verifyRun, 90*time.Second); v.Status != "success" {
			t.Fatalf("verify of a compressed backup %q: %s", v.Status, v.Error)
		}

		// Re-running with compression switched off must still deduplicate against
		// the compressed chunks: identity is the raw chunk, not its stored form.
		h.ok(http.MethodPut, "/api/settings", map[string]any{"compression": "off"}, &settings)
		again := h.runJob(job.ID)
		if again.Status != "success" {
			t.Fatalf("re-run with compression off %q: %s", again.Status, again.Error)
		}
		if again.BytesUploaded != 0 {
			t.Fatalf("turning compression off re-uploaded %d bytes; dedup must not depend on it",
				again.BytesUploaded)
		}
		h.ok(http.MethodPut, "/api/settings", map[string]any{"compression": "zstd"}, &settings)
	})

	// ---- Two clusters that each contain a node called "pve1" ---------------
	//
	// This is the defect that could route backup traffic to the wrong physical
	// machine: helpers used to be keyed by node name alone, so the second
	// cluster's "pve1" deleted the first one's and every job for either cluster
	// went to whichever helper had registered last.
	t.Run("26-two-clusters-share-a-node-name", func(t *testing.T) {
		// A second Proxmox cluster, with the same node names and guest ids as
		// the first — exactly the situation the old key could not express.
		simB := pvesim.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
		simBSrv := httptest.NewServer(simB.Handler())
		t.Cleanup(simBSrv.Close)

		var hostB apiHost
		h.ok(http.MethodPost, "/api/hosts", map[string]any{
			"name":        "pve-sim-b",
			"baseUrl":     simBSrv.URL,
			"tokenId":     "root@pam!proxback",
			"tokenSecret": "sim-b-token-secret",
			"insecureTLS": true,
		}, &hostB)
		if hostB.ID == "" || hostB.ID == host.ID {
			t.Fatalf("second host = %+v", hostB)
		}
		var vmsB []apiVM
		h.ok(http.MethodGet, "/api/hosts/"+hostB.ID+"/vms", nil, &vmsB)
		if len(vmsB) != 4 {
			t.Fatalf("second cluster inventory = %d guests", len(vmsB))
		}
		// Both clusters really do hold a pve1 with a web-01 numbered 100.
		nodesA := map[string]bool{}
		var cached []apiVM
		h.ok(http.MethodGet, "/api/vms", nil, &cached)
		for _, v := range cached {
			if v.HostID == host.ID {
				nodesA[v.Node] = true
			}
		}
		if !nodesA["pve1"] {
			t.Fatalf("the first cluster has no pve1: %+v", cached)
		}

		// One helper per cluster, both for a node called pve1, each with its own
		// access secret and its own archive content.
		helperA := newNodeHelper(t, "pve1", 5*mib, 0xA11CE)
		helperB := newNodeHelper(t, "pve1", 7*mib, 0xB0B)
		if code, res := h.registerHelper(helperA, h.helperEnrollToken(host.ID)); code != http.StatusOK {
			t.Fatalf("registering cluster A's pve1 helper = %d (%+v)", code, res)
		}
		if code, res := h.registerHelper(helperB, h.helperEnrollToken(hostB.ID)); code != http.StatusOK {
			t.Fatalf("registering cluster B's pve1 helper = %d (%+v)", code, res)
		}

		// Neither registration displaced the other.
		var helpers []apiHelper
		h.ok(http.MethodGet, "/api/helpers", nil, &helpers)
		byHost := map[string]apiHelper{}
		for _, hp := range helpers {
			if hp.Node == "pve1" {
				byHost[hp.HostID] = hp
			}
		}
		if len(byHost) != 2 {
			t.Fatalf("pve1 helpers = %+v, want one per cluster", helpers)
		}
		if byHost[host.ID].ID == "" || byHost[hostB.ID].ID == "" {
			t.Fatalf("a cluster lost its pve1 helper: %+v", helpers)
		}
		if byHost[host.ID].ID == byHost[hostB.ID].ID {
			t.Fatal("both clusters resolved to the same helper registration")
		}
		if byHost[host.ID].HostName != "pve-sim" || byHost[hostB.ID].HostName != "pve-sim-b" {
			t.Fatalf("helper listings do not name their cluster: %+v", helpers)
		}
		for _, hp := range []apiHelper{byHost[host.ID], byHost[hostB.ID]} {
			if hp.Status != "online" {
				t.Fatalf("freshly registered helper %s is %q", hp.ID, hp.Status)
			}
		}

		// A backup of cluster A's web-01 must reach cluster A's helper, and only
		// that one.
		runJobFor := func(name, hostID string, vmid int) apiRun {
			t.Helper()
			var job apiJob
			h.ok(http.MethodPost, "/api/jobs", map[string]any{
				"name": name, "kind": "vm", "targetId": vmTarget.ID,
				"schedule": "manual", "retention": 2, "enabled": true,
				"sources": []map[string]any{{"hostId": hostID, "vmid": vmid, "name": "web-01"}},
			}, &job)
			return h.runJob(job.ID)
		}
		runA := runJobFor("cluster-a-web", host.ID, 100)
		if got := len(helperA.matching(http.MethodGet, "/export/100")); got != 1 {
			t.Fatalf("cluster A's helper saw %d exports, want 1: %+v", got, helperA.seen())
		}
		if got := len(helperB.seen()); got != 0 {
			t.Fatalf("cluster B's helper was called for a cluster A backup: %+v", helperB.seen())
		}
		if runA.BytesProcessed != int64(len(helperA.content)) {
			t.Fatalf("cluster A backup read %d bytes, want cluster A's archive (%d)",
				runA.BytesProcessed, len(helperA.content))
		}

		runB := runJobFor("cluster-b-web", hostB.ID, 100)
		if got := len(helperB.matching(http.MethodGet, "/export/100")); got != 1 {
			t.Fatalf("cluster B's helper saw %d exports, want 1: %+v", got, helperB.seen())
		}
		if got := len(helperA.matching(http.MethodGet, "/export/100")); got != 1 {
			t.Fatalf("cluster A's helper was called again for a cluster B backup: %+v", helperA.seen())
		}
		if runB.BytesProcessed != int64(len(helperB.content)) {
			t.Fatalf("cluster B backup read %d bytes, want cluster B's archive (%d)",
				runB.BytesProcessed, len(helperB.content))
		}

		// The two restore points are the same guest name in different clusters,
		// and the API says which is which.
		for _, c := range []struct {
			hostID, hostName string
			size             int
		}{
			{host.ID, "pve-sim", len(helperA.content)},
			{hostB.ID, "pve-sim-b", len(helperB.content)},
		} {
			var points []apiBackup
			h.ok(http.MethodGet, "/api/backups?sourceKind=vm&sourceId="+c.hostID+"_100", nil, &points)
			if len(points) == 0 {
				t.Fatalf("%s has no restore points for web-01", c.hostName)
			}
			newest := points[0]
			if newest.HostID != c.hostID || newest.HostName != c.hostName {
				t.Fatalf("restore point identity = %q/%q, want %q/%q",
					newest.HostID, newest.HostName, c.hostID, c.hostName)
			}
			if newest.SourceName != "web-01" {
				t.Fatalf("restore point name = %q", newest.SourceName)
			}
			if len(newest.Disks) != 1 || newest.Disks[0].Name != "vma" ||
				newest.SizeBytes != int64(c.size) {
				t.Fatalf("%s restore point = %+v, want its own cluster's archive", c.hostName, newest)
			}
		}

		// Cluster B's helper is removed; cluster A keeps its own.
		h.ok(http.MethodDelete, "/api/helpers/"+byHost[hostB.ID].ID, nil, nil)
		h.ok(http.MethodGet, "/api/helpers", nil, &helpers)
		found := false
		for _, hp := range helpers {
			if hp.ID == byHost[host.ID].ID {
				found = true
			}
			if hp.ID == byHost[hostB.ID].ID {
				t.Fatal("cluster B's helper survived its deletion")
			}
		}
		if !found {
			t.Fatal("deleting cluster B's helper removed cluster A's")
		}
	})

	// ---- Protection posture ------------------------------------------------
	t.Run("27-protection-posture", func(t *testing.T) {
		// A guest whose node has a helper that has gone off the network: its own
		// backups fail while every other job in the estate keeps succeeding.
		dead := newNodeHelper(t, "pve2", 3*mib, 0xDEAD)
		if code, res := h.registerHelper(dead, h.helperEnrollToken(host.ID)); code != http.StatusOK {
			t.Fatalf("registering the pve2 helper = %d (%+v)", code, res)
		}
		dead.stop()

		var failing apiJob
		h.ok(http.MethodPost, "/api/jobs", map[string]any{
			"name": "mail-nightly", "kind": "vm", "targetId": vmTarget.ID,
			"schedule":  map[string]any{"kind": "daily", "time": "02:00"},
			"retention": 2, "enabled": true,
			"sources": []map[string]any{{"hostId": host.ID, "vmid": 103, "name": "mail-01"}},
		}, &failing)
		failedRun := h.waitRun(h.startRun(failing.ID), 90*time.Second)
		if failedRun.Status != "failed" {
			t.Fatalf("a backup through an unreachable helper finished %q", failedRun.Status)
		}

		var posture apiPosture
		h.ok(http.MethodGet, "/api/posture", nil, &posture)
		if len(posture.Workloads) == 0 {
			t.Fatal("posture reports no workloads for a populated estate")
		}
		byID := map[string]apiPostureWorkload{}
		for _, w := range posture.Workloads {
			byID[w.ID] = w
		}
		mail := byID[host.ID+"_103"]
		if mail.Status != "at_risk" {
			t.Fatalf("mail-01, whose own last run failed, is %q: %+v", mail.Status, mail)
		}
		if mail.LastFailureAt == nil {
			t.Fatalf("mail-01 is at risk but reports no failure time: %+v", mail)
		}
		if mail.Policy == "" || mail.RestorePoints == 0 {
			t.Fatalf("mail-01 posture = %+v", mail)
		}
		if mail.HostName != "pve-sim" || mail.Node != "pve2" || mail.Name != "mail-01" {
			t.Fatalf("mail-01 is not identified as cluster/name/node: %+v", mail)
		}
		// A guest whose own job succeeded is not dragged down by mail-01, and a
		// guest in no job at all is unprotected rather than merely at risk.
		web := byID[host.ID+"_100"]
		if web.Status != "protected" || web.Policy == "" {
			t.Fatalf("web-01 posture = %+v, want protected (its own job succeeded)", web)
		}
		unprotected := 0
		for _, w := range posture.Workloads {
			if w.Status == "unprotected" {
				unprotected++
				if w.Policy != "" {
					t.Fatalf("an unprotected workload names a policy: %+v", w)
				}
			}
		}
		if unprotected != posture.Counts.Unprotected {
			t.Fatalf("counts.unprotected = %d, but %d workloads say so",
				posture.Counts.Unprotected, unprotected)
		}
		total := posture.Counts.Protected + posture.Counts.AtRisk + posture.Counts.Unprotected
		if total != len(posture.Workloads) {
			t.Fatalf("counts total %d for %d workloads", total, len(posture.Workloads))
		}
		// The verdict is the worst state in the estate, and the reasons explain it.
		want := "at_risk"
		if posture.Counts.Unprotected > 0 {
			want = "unprotected"
		}
		if posture.Verdict != want {
			t.Fatalf("verdict = %q, want %q for %+v", posture.Verdict, want, posture.Counts)
		}
		sawFailure := false
		for _, r := range posture.Reasons {
			if r.Code == "last_run_failed" {
				sawFailure = true
			}
			if r.Workloads < 1 || r.Detail == "" {
				t.Fatalf("reason %+v explains nothing", r)
			}
		}
		if !sawFailure {
			t.Fatalf("reasons %+v never mention the failed backup", posture.Reasons)
		}
	})

	// ---- Step 12: protection policy and GFS retention ----------------------
	//
	// The console's Advanced protection and Retention steps are built against
	// this: a policy round-trips, its effects show up in the run log, and the
	// retention preview says exactly what the pruning pass then does.
	t.Run("28-protection-policy-and-gfs-retention", func(t *testing.T) {
		// A live helper for cluster A's pve1 and a target of its own, so this
		// job's restore points are the only history retention has to reason
		// about and the guest's node really can run policy scripts.
		policyHelper := newNodeHelper(t, "pve1", 4*mib, 0x9011CE)
		if code, res := h.registerHelper(policyHelper, h.helperEnrollToken(host.ID)); code != http.StatusOK {
			t.Fatalf("registering the policy helper = %d (%+v)", code, res)
		}
		var policyTarget apiTarget
		h.ok(http.MethodPost, "/api/targets", map[string]any{
			"name": "policy-storage", "endpoint": h.s3URL, "region": "us-east-1",
			"bucket": "proxback-policy", "accessKey": "proxback",
			"secretKey": "proxback-secret", "pathStyle": true,
		}, &policyTarget)

		// A per-disk exclusion cannot be expressed to vzdump on the helper
		// path, so ProxBack refuses the run rather than storing an archive that
		// quietly contains the excluded disk.
		var refused apiJob
		h.ok(http.MethodPost, "/api/jobs", map[string]any{
			"name": "policy-excluded", "kind": "vm", "targetId": policyTarget.ID,
			"schedule": map[string]any{"kind": "manual"}, "retention": 2, "enabled": true,
			"sources": []map[string]any{{"hostId": host.ID, "vmid": 100, "name": "web-01"}},
			"policy":  map[string]any{"excludeDisks": []string{"scsi1"}},
		}, &refused)
		refusedRun := h.waitRun(h.startRun(refused.ID), 90*time.Second)
		if refusedRun.Status != "failed" {
			t.Fatalf("a helper-backed run with excludeDisks finished %q", refusedRun.Status)
		}
		for _, want := range []string{"excludeDisks cannot be honoured", "backup=0"} {
			if !strings.Contains(refusedRun.Error, want) {
				t.Fatalf("refusal = %q, want it to mention %q", refusedRun.Error, want)
			}
		}

		// An invalid policy never reaches a run at all: the field is named.
		code, body := h.do(http.MethodPost, "/api/jobs", map[string]any{
			"name": "policy-invalid", "kind": "vm", "targetId": policyTarget.ID,
			"schedule": map[string]any{"kind": "manual"}, "retention": 2, "enabled": true,
			"sources": []map[string]any{{"hostId": host.ID, "vmid": 100, "name": "web-01"}},
			"policy":  map[string]any{"retryCount": 9},
		})
		if code != http.StatusBadRequest || !strings.Contains(string(body), "policy.retryCount") {
			t.Fatalf("a job with 9 retries = %d (%s), want a 400 naming policy.retryCount", code, body)
		}

		policy := map[string]any{
			"quiesce":            "guest-agent",
			"retryCount":         0,
			"retryDelayMinutes":  1,
			"maxDurationMinutes": 30,
			// Open all day; the window is closed further down to check that a
			// manual run is still allowed through it.
			"window":                  map[string]any{"start": "00:00", "end": "23:59"},
			"preScript":               "/usr/local/bin/freeze.sh",
			"postScript":              "/usr/local/bin/thaw.sh",
			"scriptTimeoutSeconds":    45,
			"uploadLimitMbpsOverride": 2000,
		}
		var job apiJob
		h.ok(http.MethodPost, "/api/jobs", map[string]any{
			"name": "policy-web", "kind": "vm", "targetId": policyTarget.ID,
			"schedule": map[string]any{"kind": "manual"},
			// The GFS object, not an integer: keep the newest point and the
			// newest point of each of the last two days.
			"retention": map[string]any{"keepLast": 1, "keepDaily": 2},
			"enabled":   true,
			"sources":   []map[string]any{{"hostId": host.ID, "vmid": 100, "name": "web-01"}},
			"policy":    policy,
		}, &job)
		if job.ID == "" {
			t.Fatalf("created job = %+v", job)
		}
		if job.Retention.KeepLast != 1 || job.Retention.KeepDaily != 2 || job.Retention.KeepWeekly != 0 {
			t.Fatalf("job retention = %+v", job.Retention)
		}
		if job.Policy.Quiesce != "guest-agent" || job.Policy.MaxDurationMinutes != 30 ||
			job.Policy.ScriptTimeoutSeconds != 45 || job.Policy.UploadLimitMbpsOverride != 2000 ||
			job.Policy.PreScript != "/usr/local/bin/freeze.sh" {
			t.Fatalf("job policy = %+v", job.Policy)
		}
		if job.Policy.Window == nil || job.Policy.Window.Start != "00:00" {
			t.Fatalf("job policy window = %+v", job.Policy.Window)
		}

		var log struct {
			Lines []struct {
				Line string `json:"line"`
			} `json:"lines"`
		}
		readLog := func(runID string) string {
			t.Helper()
			h.ok(http.MethodGet, "/api/runs/"+runID+"/log", nil, &log)
			var joined []string
			for _, l := range log.Lines {
				joined = append(joined, l.Line)
			}
			return strings.Join(joined, "\n")
		}

		run := h.runJob(job.ID)
		text := readLog(run.ID)
		for _, want := range []string{
			"policy: upload capped at 2000 Mbps",
			"policy: this run is limited to 30 minutes",
			"web-01: running the pre-script on node pve1 (timeout 45s)",
			"web-01: pre-script: ran /usr/local/bin/freeze.sh on pve1",
			"web-01: running the post-script on node pve1",
			// The guest has no qemu-guest-agent, and ProxBack says so rather
			// than claiming a freeze that never happened.
			"policy asks for guest-agent quiescing",
			"crash-consistent",
		} {
			if !strings.Contains(text, want) {
				t.Fatalf("the run log never says %q:\n%s", want, text)
			}
		}
		// The scripts really ran on the node, with the policy's timeout.
		scripts := policyHelper.scriptsRun()
		if len(scripts) != 2 || scripts[0].Phase != "pre" || scripts[1].Phase != "post" {
			t.Fatalf("the helper ran %+v, want a pre-script then a post-script", scripts)
		}
		if scripts[0].TimeoutSeconds != 45 {
			t.Fatalf("the pre-script carried a %ds timeout, want 45", scripts[0].TimeoutSeconds)
		}

		// Two more runs, each with a change so the restore points differ.
		for i := 0; i < 2; i++ {
			if _, _, err := h.sim.Mutate(100); err != nil {
				t.Fatalf("mutate web-01: %v", err)
			}
			h.runJob(job.ID)
		}

		// Everything ran on one day, so keepLast 1 and keepDaily 2 both point
		// at the newest restore point: exactly one survives.
		var backups []apiBackup
		h.ok(http.MethodGet, "/api/backups?jobId="+job.ID, nil, &backups)
		if len(backups) != 1 {
			t.Fatalf("GFS retention left %d restore points, want 1", len(backups))
		}
		survivor := backups[0].ID

		// The preview and the pruning pass are the same decision: whatever the
		// preview would prune has already been pruned, and what it keeps is
		// exactly what is on the target.
		var preview apiRetentionPreview
		h.ok(http.MethodGet, "/api/jobs/"+job.ID+"/retention-preview", nil, &preview)
		if len(preview.Prunes) != 0 {
			t.Fatalf("the preview would still prune %d points that retention left in place: %+v",
				len(preview.Prunes), preview.Prunes)
		}
		if len(preview.Keeps) != 1 || preview.Keeps[0].BackupID != survivor {
			t.Fatalf("preview keeps %+v, want the one surviving restore point %s",
				preview.Keeps, survivor)
		}
		if strings.Join(preview.Keeps[0].Reasons, "+") != "last+daily" {
			t.Fatalf("preview reasons = %v, want [last daily]", preview.Keeps[0].Reasons)
		}

		// A candidate policy in the query previews an unsaved edit without
		// touching anything: keeping nothing prunes everything, on paper.
		var candidate apiRetentionPreview
		h.ok(http.MethodGet,
			"/api/jobs/"+job.ID+"/retention-preview?keepLast=0&keepDaily=0&keepWeekly=0&keepMonthly=0&keepYearly=0",
			nil, &candidate)
		if len(candidate.Keeps) != 0 || len(candidate.Prunes) != 1 {
			t.Fatalf("an empty candidate policy previews %d keeps / %d prunes, want 0/1",
				len(candidate.Keeps), len(candidate.Prunes))
		}
		h.ok(http.MethodGet, "/api/backups?jobId="+job.ID, nil, &backups)
		if len(backups) != 1 || backups[0].ID != survivor {
			t.Fatalf("the preview changed the estate: %+v", backups)
		}

		// A failing post-script fails the run but keeps the restore point it
		// was taken from: the data is real, and the script's mistake is not
		// the data's fault.
		policyHelper.failScript("post", "thaw.sh: could not resume replication")
		if _, _, err := h.sim.Mutate(100); err != nil {
			t.Fatalf("mutate web-01: %v", err)
		}
		postRun := h.waitRun(h.startRun(job.ID), 90*time.Second)
		if postRun.Status != "failed" || !strings.Contains(postRun.Error, "post-script") {
			t.Fatalf("a failing post-script produced %q / %q", postRun.Status, postRun.Error)
		}
		if text := readLog(postRun.ID); !strings.Contains(text, "the restore point taken before the post-script is kept") {
			t.Fatalf("the run log does not say the restore point survived:\n%s", text)
		}
		h.ok(http.MethodGet, "/api/backups?jobId="+job.ID, nil, &backups)
		if len(backups) == 0 {
			t.Fatal("a failing post-script destroyed the restore point it had just taken")
		}

		// A failing pre-script stops before any data moves.
		policyHelper.clearScriptFailures()
		policyHelper.failScript("pre", "freeze.sh: database is locked")
		exportsBefore := len(policyHelper.matching(http.MethodGet, "/export/100"))
		preRun := h.waitRun(h.startRun(job.ID), 90*time.Second)
		if preRun.Status != "failed" || !strings.Contains(preRun.Error, "pre-script") {
			t.Fatalf("a failing pre-script produced %q / %q", preRun.Status, preRun.Error)
		}
		if preRun.BytesProcessed != 0 || preRun.BytesUploaded != 0 {
			t.Fatalf("a failing pre-script still moved %d/%d bytes",
				preRun.BytesProcessed, preRun.BytesUploaded)
		}
		if got := len(policyHelper.matching(http.MethodGet, "/export/100")); got != exportsBefore {
			t.Fatalf("the helper exported %d times after a failing pre-script, want %d",
				got, exportsBefore)
		}
		policyHelper.clearScriptFailures()

		// A scheduled run outside the job's window is skipped, while the
		// operator's own "Run now" is not — that asymmetry is the whole point
		// of a window, so the API must never refuse a manual start for it.
		closed := time.Now().Add(3 * time.Hour)
		h.ok(http.MethodPatch, "/api/jobs/"+job.ID, map[string]any{
			"policy": map[string]any{
				"quiesce": "none",
				"window": map[string]any{
					"start": closed.Format("15:04"),
					"end":   closed.Add(time.Hour).Format("15:04"),
				},
			},
		}, &job)
		if job.Policy.Window == nil {
			t.Fatalf("the window was not saved: %+v", job.Policy)
		}
		manual := h.runJob(job.ID)
		if !strings.Contains(readLog(manual.ID), "a manual run is always allowed") {
			t.Fatalf("a manual run outside the window did not record the override: %+v", log.Lines)
		}

		// And a retention policy that keeps nothing never gets to delete the
		// last copy: the run applies retention and the restore point remains.
		h.ok(http.MethodPatch, "/api/jobs/"+job.ID, map[string]any{
			"retention": map[string]any{"keepLast": 0, "keepDaily": 0},
		}, nil)
		if _, _, err := h.sim.Mutate(100); err != nil {
			t.Fatalf("mutate web-01: %v", err)
		}
		h.runJob(job.ID)
		h.ok(http.MethodGet, "/api/backups?jobId="+job.ID, nil, &backups)
		if len(backups) == 0 {
			t.Fatal("a retention policy that keeps nothing deleted every restore point")
		}
	})

	// ---- Step 29: the whole product against a filesystem target -------------
	//
	// A homelab or SMB backs up to a NAS or a local disk first and copies offsite
	// second, so everything above has to work with no object storage anywhere:
	// backup, dedup, incremental, verification, a byte-identical restore and
	// retention with orphan collection, all against a path.
	t.Run("29-filesystem-target-end-to-end", func(t *testing.T) {
		s3ChunksBefore, s3BytesBefore := h.chunkBytes(vmBucket)

		// The helpers registered by the steps above went away with their
		// subtests, so this scenario is the agentless one: the server exports
		// each disk from the Proxmox API itself. Retiring the stale
		// registrations is what an operator would do too — a helper that no
		// longer answers is not a route to keep.
		var helpers []struct {
			ID   string `json:"id"`
			Node string `json:"node"`
		}
		h.ok(http.MethodGet, "/api/helpers", nil, &helpers)
		for _, hp := range helpers {
			h.ok(http.MethodDelete, "/api/helpers/"+hp.ID, nil, nil)
		}

		share := filepath.Join(t.TempDir(), "nas", "proxback")
		if err := os.MkdirAll(share, 0o755); err != nil {
			t.Fatalf("create the share: %v", err)
		}

		// Backing up onto the filesystem ProxBack itself runs from is refused, and
		// the refusal says how to accept the risk on purpose. The E2E data
		// directory and this share are both temp directories, so that is the case
		// here on any platform that can compare filesystems.
		code, body := h.do(http.MethodPost, "/api/targets", map[string]any{
			"name": "nas-storage", "kind": "filesystem", "path": share,
		})
		switch code {
		case http.StatusBadRequest:
			if !strings.Contains(string(body), "allowSameFilesystem") {
				t.Fatalf("refusal does not explain the override: %s", body)
			}
			t.Logf("same-filesystem refusal: %s", body)
		case http.StatusOK:
			t.Logf("the share is on a different filesystem than the data directory; " +
				"the same-filesystem refusal is covered in internal/api and internal/blobstore")
			var accepted apiTarget
			if err := json.Unmarshal(body, &accepted); err != nil {
				t.Fatalf("decode target: %v", err)
			}
			h.ok(http.MethodDelete, "/api/targets/"+accepted.ID, nil, nil)
		default:
			t.Fatalf("POST /api/targets = %d (%s)", code, body)
		}

		// A mix of the two shapes is refused rather than half applied.
		if code, body := h.do(http.MethodPost, "/api/targets", map[string]any{
			"name": "confused", "kind": "filesystem", "path": share, "bucket": vmBucket,
		}); code != http.StatusBadRequest || !strings.Contains(string(body), "bucket") {
			t.Fatalf("a filesystem target with a bucket = %d (%s), want 400 naming bucket", code, body)
		}

		var nasTarget apiTarget
		h.ok(http.MethodPost, "/api/targets", map[string]any{
			"name": "nas-storage", "kind": "filesystem", "path": share,
			"allowSameFilesystem": true,
		}, &nasTarget)
		if nasTarget.ID == "" || nasTarget.Kind != "filesystem" || nasTarget.Path != share {
			t.Fatalf("created filesystem target = %+v", nasTarget)
		}
		if nasTarget.Bucket != "" || nasTarget.Endpoint != "" {
			t.Fatalf("filesystem target carries object storage fields: %+v", nasTarget)
		}
		if nasTarget.Status != "online" {
			t.Fatalf("filesystem target status = %q, want online", nasTarget.Status)
		}
		if nasTarget.TotalBytes <= 0 || nasTarget.FreeBytes <= 0 {
			t.Logf("this platform does not report capacity for %s", share)
		}

		// The listing keeps both kinds side by side, with capacity for the path.
		var targets []apiTarget
		h.ok(http.MethodGet, "/api/targets", nil, &targets)
		var listed apiTarget
		for _, tg := range targets {
			switch tg.ID {
			case nasTarget.ID:
				listed = tg
			default:
				if tg.Kind != "s3" {
					t.Fatalf("existing target %s reports kind %q, want s3", tg.Name, tg.Kind)
				}
				if tg.Path != "" || tg.FreeBytes != 0 || tg.TotalBytes != 0 {
					t.Fatalf("an S3 target reports a path or capacity: %+v", tg)
				}
			}
		}
		if listed.ID == "" || listed.Path != share || listed.TotalBytes != nasTarget.TotalBytes {
			t.Fatalf("listed filesystem target = %+v", listed)
		}

		// The connection test is a diagnosis, not a pass/fail: the share is a
		// subdirectory, so it is not a mount point and the console is told so.
		var probe struct {
			OK             bool   `json:"ok"`
			Error          string `json:"error"`
			Path           string `json:"path"`
			FreeBytes      int64  `json:"freeBytes"`
			TotalBytes     int64  `json:"totalBytes"`
			FilesystemType string `json:"filesystemType"`
			MountPoint     string `json:"mountPoint"`
			IsMountPoint   bool   `json:"isMountPoint"`
			Warnings       []struct {
				Code   string `json:"code"`
				Detail string `json:"detail"`
			} `json:"warnings"`
		}
		h.ok(http.MethodPost, "/api/targets/"+nasTarget.ID+"/test", nil, &probe)
		if !probe.OK {
			t.Fatalf("filesystem target test failed: %s", probe.Error)
		}
		if probe.Path != share {
			t.Fatalf("test reported path %q, want %s", probe.Path, share)
		}
		if probe.IsMountPoint {
			t.Fatalf("a subdirectory was reported as a mount point: %+v", probe)
		}
		if len(probe.Warnings) == 0 {
			t.Fatal("the connection test reported no diagnostics for a non-mount-point share")
		}
		for _, w := range probe.Warnings {
			if w.Code == "" || w.Detail == "" {
				t.Fatalf("warning %+v is not usable by a console", w)
			}
			t.Logf("target diagnostic %s: %s", w.Code, w.Detail)
		}

		// ---- on-disk helpers: the point of a path target is that you can look
		// at it, so the assertions below read the tree directly.
		chunkFiles := func() []string {
			entries, err := os.ReadDir(filepath.Join(share, "chunks"))
			if err != nil {
				t.Fatalf("read the chunk directory: %v", err)
			}
			out := make([]string, 0, len(entries))
			for _, e := range entries {
				out = append(out, e.Name())
			}
			return out
		}
		chunkTotal := func() int64 {
			var total int64
			for _, name := range chunkFiles() {
				info, err := os.Stat(filepath.Join(share, "chunks", name))
				if err != nil {
					t.Fatalf("stat chunk %s: %v", name, err)
				}
				total += info.Size()
			}
			return total
		}
		manifestFiles := func() []string {
			var out []string
			root := filepath.Join(share, "manifests")
			if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					if os.IsNotExist(err) {
						return nil
					}
					return err
				}
				if !info.IsDir() {
					rel, relErr := filepath.Rel(share, path)
					if relErr != nil {
						return relErr
					}
					out = append(out, filepath.ToSlash(rel))
				}
				return nil
			}); err != nil {
				t.Fatalf("walk the manifest tree: %v", err)
			}
			sort.Strings(out)
			return out
		}

		// ---- a full backup ---------------------------------------------------
		var nasJob apiJob
		h.ok(http.MethodPost, "/api/jobs", map[string]any{
			"name": "nightly-nas", "kind": "vm", "targetId": nasTarget.ID,
			"schedule": "manual", "retention": 2, "enabled": true,
			"sources": []map[string]any{
				{"hostId": host.ID, "vmid": 101, "name": "db-01"},
			},
		}, &nasJob)
		if nasJob.TargetName != "nas-storage" {
			t.Fatalf("job target = %q", nasJob.TargetName)
		}

		full := h.runJob(nasJob.ID)
		if full.BytesProcessed != int64(16*mib) || full.BytesUploaded != int64(16*mib) {
			t.Fatalf("first run to the share = %d processed / %d uploaded, want %d / %d",
				full.BytesProcessed, full.BytesUploaded, 16*mib, 16*mib)
		}
		// The layout on disk is exactly the documented one, and it is the same
		// layout an S3 target holds: chunks/<sha> and manifests/<kind>/<id>/<id>.json.
		if got := chunkFiles(); len(got) != 4 {
			t.Fatalf("the share holds %d chunks, want 4 (16 MiB in 4 MiB chunks): %v", len(got), got)
		}
		for _, name := range chunkFiles() {
			if len(name) != 64 {
				t.Fatalf("chunk file %q is not a SHA-256 content address", name)
			}
		}
		if total := chunkTotal(); total != int64(16*mib) {
			t.Fatalf("the share holds %d chunk bytes, want %d", total, 16*mib)
		}
		manifests := manifestFiles()
		if len(manifests) != 1 ||
			!strings.HasPrefix(manifests[0], "manifests/vm/"+host.ID+"_101/") ||
			!strings.HasSuffix(manifests[0], ".json") {
			t.Fatalf("manifest tree = %v", manifests)
		}

		// ---- an unchanged re-run deduplicates, an incremental uploads the delta
		dedup := h.runJob(nasJob.ID)
		if dedup.BytesUploaded != 0 || dedup.DedupRatio < 0.999 {
			t.Fatalf("unchanged re-run to the share uploaded %d bytes (ratio %v)",
				dedup.BytesUploaded, dedup.DedupRatio)
		}
		if got := len(chunkFiles()); got != 4 {
			t.Fatalf("a deduplicated run grew the share to %d chunks", got)
		}

		disk, changed, err := h.sim.Mutate(101)
		if err != nil {
			t.Fatalf("mutate db-01: %v", err)
		}
		if changed != 1 {
			t.Fatalf("the simulator changed %d chunks, want 1", changed)
		}
		incr := h.runJob(nasJob.ID)
		if incr.BytesUploaded != int64(4*mib) {
			t.Fatalf("incremental to the share uploaded %d bytes, want one 4 MiB chunk", incr.BytesUploaded)
		}
		if got := len(chunkFiles()); got != 5 {
			t.Fatalf("the share holds %d chunks after an incremental, want 5", got)
		}

		var points []apiBackup
		h.ok(http.MethodGet, "/api/backups?targetId="+nasTarget.ID, nil, &points)
		if len(points) != 2 {
			t.Fatalf("restore points on the share = %d, want 2 (keep last 2)", len(points))
		}
		newest := points[0]
		if newest.Kind != "incremental" || newest.ParentID != points[1].ID {
			t.Fatalf("newest point = %q with parent %q", newest.Kind, newest.ParentID)
		}
		if newest.TargetID != nasTarget.ID || len(newest.Disks) != 1 {
			t.Fatalf("newest point = %+v", newest)
		}

		// ---- verification re-hashes every chunk on the share -----------------
		verify := h.waitRun(h.verify(newest.ID), 90*time.Second)
		if verify.Status != "success" {
			t.Fatalf("verification against the share finished %q: %s", verify.Status, verify.Error)
		}
		if verify.BytesProcessed != newest.SizeBytes || verify.BytesUploaded != 0 {
			t.Fatalf("verification read %d bytes and uploaded %d, want %d / 0",
				verify.BytesProcessed, verify.BytesUploaded, newest.SizeBytes)
		}
		verified := h.backupByID(newest.SourceKind, newest.SourceID, newest.ID)
		if verified.LastVerifyResult != "passed" || verified.LastVerifiedAt == nil ||
			verified.VerifiedBytes != newest.SizeBytes {
			t.Fatalf("verification evidence on the restore point = %+v", verified)
		}

		// ---- a byte-identical restore ---------------------------------------
		node := ""
		for _, v := range vms {
			if v.VMID == 101 {
				node = v.Node
			}
		}
		if node == "" {
			t.Fatal("db-01 is not in the inventory")
		}
		var started struct {
			RunID string `json:"runId"`
		}
		h.ok(http.MethodPost, "/api/restores", map[string]any{
			"backupId": newest.ID,
			"vm":       map[string]any{"hostId": host.ID, "node": node, "vmid": 9998},
		}, &started)
		restore := h.waitRun(started.RunID, 90*time.Second)
		if restore.Status != "success" {
			t.Fatalf("restore from the share finished %q: %s", restore.Status, restore.Error)
		}
		if restore.Restore == nil || restore.Restore.Mode != "alongside" || restore.Restore.VMID != 9998 {
			t.Fatalf("restore destination = %+v", restore.Restore)
		}
		want := h.fetchRaw(fmt.Sprintf("%s/sim/disk/101/%s", h.simURL, disk))
		got := h.fetchRaw(fmt.Sprintf("%s/sim/imported/9998/%s", h.simURL, disk))
		if len(got) != len(want) {
			t.Fatalf("restored %s is %d bytes, the live disk is %d", disk, len(got), len(want))
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("restored %s differs from the live disk", disk)
		}

		// ---- retention prunes and orphan chunks leave the share --------------
		h.ok(http.MethodPatch, "/api/jobs/"+nasJob.ID, map[string]any{"retention": 1}, &nasJob)
		pruned := h.runJob(nasJob.ID)
		if pruned.BytesUploaded != 0 {
			t.Fatalf("the pruning run uploaded %d bytes", pruned.BytesUploaded)
		}
		h.ok(http.MethodGet, "/api/backups?targetId="+nasTarget.ID, nil, &points)
		if len(points) != 1 {
			t.Fatalf("keep-last-1 left %d restore points on the share", len(points))
		}
		if manifests := manifestFiles(); len(manifests) != 1 {
			t.Fatalf("the share holds %d manifests after pruning, want 1: %v", len(manifests), manifests)
		}
		// The chunk the pruned restore point referenced is unreferenced now, and
		// orphan collection removes it from the share (the 24 h grace is switched
		// off for this suite).
		if got := chunkFiles(); len(got) != 4 {
			t.Fatalf("the share holds %d chunks after collection, want 4: %v", len(got), got)
		}
		if total := chunkTotal(); total != int64(16*mib) {
			t.Fatalf("the share holds %d chunk bytes after collection, want %d", total, 16*mib)
		}
		// The surviving restore point still restores, which is what makes the
		// collection above safe rather than merely tidy.
		h.ok(http.MethodPost, "/api/restores", map[string]any{
			"backupId": points[0].ID,
			"vm":       map[string]any{"hostId": host.ID, "node": node, "vmid": 9997},
		}, &started)
		if run := h.waitRun(started.RunID, 90*time.Second); run.Status != "success" {
			t.Fatalf("restore after collection finished %q: %s", run.Status, run.Error)
		}
		if !bytes.Equal(h.fetchRaw(fmt.Sprintf("%s/sim/imported/9997/%s", h.simURL, disk)), want) {
			t.Fatalf("restore after collection differs from the live disk")
		}

		// No object storage was involved in any of this.
		if chunks, bytesOnS3 := h.chunkBytes(vmBucket); chunks != s3ChunksBefore || bytesOnS3 != s3BytesBefore {
			t.Fatalf("the object storage bucket changed during a filesystem-only scenario: "+
				"%d chunks / %d bytes, was %d / %d", chunks, bytesOnS3, s3ChunksBefore, s3BytesBefore)
		}

		// Deleting the target leaves the operator's data on the share: ProxBack
		// removes its own bookkeeping, never the files on someone's NAS.
		h.ok(http.MethodDelete, "/api/targets/"+nasTarget.ID, nil, nil)
		if got := len(chunkFiles()); got != 4 {
			t.Fatalf("deleting the target removed %d chunks from the share", 4-got)
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

// writeCompressibleTree writes a payload that behaves like real files rather
// than like the simulator's pseudo-random disks: structured, repetitive text
// that zstd can genuinely shrink, so the compression assertions are about
// compression and not about luck.
func writeCompressibleTree(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Every line is distinct and carries a pseudo-random field, so the content is
	// compressible the way real logs are rather than degenerately repetitive.
	write := func(rel string, size int, seed uint64) {
		var b strings.Builder
		b.Grow(size + 128)
		x := seed
		for i := 0; b.Len() < size; i++ {
			x = x*6364136223846793005 + 1442695040888963407
			fmt.Fprintf(&b, "2026-07-27T%02d:%02d:%02d.%03dZ level=info component=proxback run=%s msg=\"chunk stored\" seq=%d sha=%016x size=4194304\n",
				i%24, i%60, (i*7)%60, i%1000, rel, i, x>>3)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(b.String())[:size], 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("logs/server.log", 9*mib, 11)
	write("logs/agent.log", 7*mib, 22)
	write("logs/tail.log", 613*1024, 33)
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
