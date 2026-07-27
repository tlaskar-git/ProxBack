// Package helper implements the ProxBack node helper: a small root daemon
// installed on every Proxmox VE node that makes agentless VM image backup work
// on real hardware.
//
// Real Proxmox has no disk-export API, so instead of inventing one the helper
// wraps the node's own tooling. Export runs
// `vzdump <vmid> --mode snapshot --compress 0 --stdout` and streams the VMA
// archive to the server; import pipes an archive back into `qmrestore - <vmid>`.
// Snapshot consistency, storage types and qemu-agent filesystem freeze are all
// PVE's business, which is exactly the point.
//
// The daemon enrolls once with a single-use token, generates its own access
// secret (the bearer token the server must present on /export and /import),
// persists both in a 0600 config file and then serves HTTP on :8007 while
// heartbeating to the server every 30 seconds.
package helper

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"proxback/internal/helpermgr"
	"proxback/internal/version"
)

// DefaultPort is the port the helper listens on.
const DefaultPort = 8007

// DefaultHeartbeatInterval is how often the helper reports liveness.
const DefaultHeartbeatInterval = 30 * time.Second

// ConfigFileName is the name of the helper's stored configuration.
const ConfigFileName = "helper.json"

// DefaultConfigDir is where the configuration lives on a Proxmox node.
const DefaultConfigDir = "/etc/proxback-helper"

// stderrTailBytes bounds how much command stderr is retained for error reports.
const stderrTailBytes = 8 << 10

// Config configures a helper.
type Config struct {
	ServerURL string
	Token     string
	ConfigDir string
	// Port overrides the stored/default listen port.
	Port              int
	HeartbeatInterval time.Duration
	InsecureTLS       bool
	Logger            *slog.Logger
	HTTPClient        *http.Client
	// Runner executes vzdump/qmrestore. Nil uses the real exec-based runner.
	Runner Runner
	// Node overrides the detected node name (tests and unusual hostnames).
	Node string
}

// storedConfig is the on-disk state written after enrollment.
type storedConfig struct {
	ServerURL    string `json:"serverUrl"`
	HelperID     string `json:"helperId"`
	APIKey       string `json:"apiKey"`
	AccessSecret string `json:"accessSecret"`
	Node         string `json:"node"`
	Port         int    `json:"port"`
}

// Helper is a running node helper.
type Helper struct {
	cfg    Config
	log    *slog.Logger
	hc     *http.Client
	runner Runner

	mu   sync.Mutex
	self storedConfig
}

// New builds a helper.
func New(cfg Config) (*Helper, error) {
	if cfg.ConfigDir == "" {
		return nil, errors.New("helper: config dir required")
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = DefaultHeartbeatInterval
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	hc := cfg.HTTPClient
	if hc == nil {
		tr := &http.Transport{MaxIdleConnsPerHost: 2}
		if cfg.InsecureTLS {
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // operator opt-in
		}
		hc = &http.Client{Transport: tr, Timeout: time.Minute}
	}
	runner := cfg.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	return &Helper{cfg: cfg, log: log, hc: hc, runner: runner}, nil
}

// ---------------------------------------------------------------- command exec

// Command is one node command the helper runs.
type Command struct {
	Name  string
	Args  []string
	Stdin io.Reader
	// Stdout receives the command's standard output as it is produced.
	Stdout io.Writer
}

// Runner executes node commands. The real implementation shells out to
// vzdump/qmrestore; tests substitute a fake so the handlers can be exercised on
// any platform. Run returns the tail of the command's stderr alongside the exit
// error.
type Runner interface {
	Run(ctx context.Context, cmd Command) (stderr string, err error)
}

// ExecRunner runs commands with os/exec.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, c Command) (string, error) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Stdin = c.Stdin
	cmd.Stdout = c.Stdout
	tail := &tailBuffer{max: stderrTailBytes}
	cmd.Stderr = tail
	err := cmd.Run()
	return tail.String(), err
}

// tailBuffer keeps only the last max bytes written to it, so a chatty command
// cannot exhaust memory while its final complaint is still reported.
type tailBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if excess := t.buf.Len() - t.max; excess > 0 {
		t.buf.Next(excess)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(t.buf.String())
}

// VzdumpArgs is the argument list for a whole-guest export. Compression is off:
// the ProxBack engine deduplicates and the server owns storage efficiency.
func VzdumpArgs(vmid int) []string {
	return []string{strconv.Itoa(vmid), "--mode", "snapshot", "--compress", "0", "--stdout"}
}

// QmrestoreArgs is the argument list for restoring a VMA archive from stdin.
func QmrestoreArgs(vmid int, storage string, force bool) []string {
	args := []string{"-", strconv.Itoa(vmid)}
	if storage != "" {
		args = append(args, "--storage", storage)
	}
	if force {
		args = append(args, "--force")
	}
	return args
}

// ---------------------------------------------------------------- config

func (h *Helper) configPath() string { return filepath.Join(h.cfg.ConfigDir, ConfigFileName) }

func (h *Helper) loadConfig() error {
	raw, err := os.ReadFile(h.configPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("helper: read config: %w", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := json.Unmarshal(raw, &h.self); err != nil {
		return fmt.Errorf("helper: parse config: %w", err)
	}
	return nil
}

func (h *Helper) saveConfig() error {
	if err := os.MkdirAll(h.cfg.ConfigDir, 0o700); err != nil {
		return fmt.Errorf("helper: create config dir: %w", err)
	}
	h.mu.Lock()
	raw, err := json.MarshalIndent(h.self, "", "  ")
	h.mu.Unlock()
	if err != nil {
		return fmt.Errorf("helper: encode config: %w", err)
	}
	if err := os.WriteFile(h.configPath(), raw, 0o600); err != nil {
		return fmt.Errorf("helper: write config: %w", err)
	}
	return nil
}

func (h *Helper) snapshot() storedConfig {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.self
}

// HelperID returns the enrolled helper id (empty before registration).
func (h *Helper) HelperID() string { return h.snapshot().HelperID }

// AccessSecret returns the bearer token the server must present.
func (h *Helper) AccessSecret() string { return h.snapshot().AccessSecret }

// Node returns the Proxmox node name the helper reported at registration.
func (h *Helper) Node() string { return h.snapshot().Node }

// Port returns the port the helper listens on.
func (h *Helper) Port() int {
	if p := h.snapshot().Port; p > 0 {
		return p
	}
	return DefaultPort
}

// NodeName returns the short form of the machine's hostname, which is how a node
// is named in a Proxmox cluster.
func NodeName() string {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		return "unknown"
	}
	if i := strings.IndexByte(hostname, '.'); i > 0 {
		hostname = hostname[:i]
	}
	return hostname
}

// newAccessSecret generates the 32 byte (64 hex character) access secret.
func newAccessSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", fmt.Errorf("helper: generate access secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// ---------------------------------------------------------------- enrollment

// Enroll loads the stored configuration, registering with the server when no API
// key is present yet.
func (h *Helper) Enroll(ctx context.Context) error {
	if err := h.loadConfig(); err != nil {
		return err
	}
	h.mu.Lock()
	if h.cfg.ServerURL != "" {
		h.self.ServerURL = strings.TrimRight(h.cfg.ServerURL, "/")
	}
	if h.cfg.Port > 0 {
		h.self.Port = h.cfg.Port
	}
	if h.self.Port <= 0 {
		h.self.Port = DefaultPort
	}
	if h.cfg.Node != "" {
		h.self.Node = h.cfg.Node
	}
	if h.self.Node == "" {
		h.self.Node = NodeName()
	}
	enrolled := h.self.APIKey != ""
	serverURL := h.self.ServerURL
	node, port := h.self.Node, h.self.Port
	h.mu.Unlock()

	if serverURL == "" {
		return errors.New("helper: --server is required on first run")
	}
	if enrolled {
		// A restart must not lose the port or node the operator configured.
		return h.saveConfig()
	}
	if h.cfg.Token == "" {
		return errors.New("helper: no stored API key; pass --token with an enrollment token")
	}
	secret, err := newAccessSecret()
	if err != nil {
		return err
	}
	req := helpermgr.RegisterRequest{
		Token:        h.cfg.Token,
		Node:         node,
		Port:         port,
		Version:      version.Version,
		AccessSecret: secret,
	}
	var res helpermgr.RegisterResponse
	if err := h.doJSON(ctx, http.MethodPost, "/api/helpers/register", req, &res, false); err != nil {
		return fmt.Errorf("helper: register: %w", err)
	}
	h.mu.Lock()
	h.self.HelperID = res.HelperID
	h.self.APIKey = res.APIKey
	h.self.AccessSecret = secret
	h.mu.Unlock()
	if err := h.saveConfig(); err != nil {
		return err
	}
	h.log.Info("node helper enrolled", "helperId", res.HelperID, "node", node, "port", port)
	return nil
}

// Heartbeat reports liveness once.
func (h *Helper) Heartbeat(ctx context.Context) error {
	return h.doJSON(ctx, http.MethodPost, "/api/helpers/heartbeat", map[string]any{}, nil, true)
}

// ---------------------------------------------------------------- run loop

// Run enrolls if necessary, serves the HTTP API and heartbeats until the context
// is cancelled.
func (h *Helper) Run(ctx context.Context) error {
	if err := h.Enroll(ctx); err != nil {
		return err
	}
	addr := ":" + strconv.Itoa(h.Port())
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("helper: listen on %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler:           h.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		h.log.Info("node helper serving", "addr", ln.Addr().String(),
			"node", h.Node(), "server", h.snapshot().ServerURL)
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errc <- err
	}()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.heartbeatLoop(ctx)
	}()

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		h.log.Warn("node helper shutdown", "error", err)
	}
	wg.Wait()
	if serveErr != nil {
		return serveErr
	}
	return ctx.Err()
}

func (h *Helper) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(h.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		beatCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := h.Heartbeat(beatCtx)
		cancel()
		if err != nil && ctx.Err() == nil {
			h.log.Warn("heartbeat failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ---------------------------------------------------------------- transport

func (h *Helper) doJSON(ctx context.Context, method, path string, body, out any, withKey bool) error {
	self := h.snapshot()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("helper: encode request: %w", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, self.ServerURL+path, rdr)
	if err != nil {
		return fmt.Errorf("helper: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if withKey {
		req.Header.Set("Authorization", "Bearer "+self.APIKey)
	}
	resp, err := h.hc.Do(req)
	if err != nil {
		return fmt.Errorf("helper: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("helper: %s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("helper: %s %s: http %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("helper: %s %s: decode response: %w", method, path, err)
	}
	return nil
}

// ---------------------------------------------------------------- HTTP API

// Handler returns the helper's HTTP handler. Routing is done by hand: the helper
// is a three-endpoint daemon that must stay a single dependency-free binary.
func (h *Helper) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", h.handleHealth)
	mux.HandleFunc("/export/", h.handleExport)
	mux.HandleFunc("/import/", h.handleImport)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (h *Helper) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"node": h.Node(), "version": version.Version,
	})
}

// authorized checks the bearer access secret in constant time.
func (h *Helper) authorized(r *http.Request) bool {
	want := h.AccessSecret()
	if want == "" {
		return false
	}
	got := bearerToken(r)
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	v := r.Header.Get("Authorization")
	if len(v) <= len(prefix) || !strings.EqualFold(v[:len(prefix)], prefix) {
		return ""
	}
	return v[len(prefix):]
}

// vmidFrom parses the trailing path element of /export/{vmid} or /import/{vmid}.
func vmidFrom(path, prefix string) (int, error) {
	raw := strings.Trim(strings.TrimPrefix(path, prefix), "/")
	if raw == "" || strings.Contains(raw, "/") {
		return 0, fmt.Errorf("expected %s{vmid}", prefix)
	}
	vmid, err := strconv.Atoi(raw)
	if err != nil || vmid <= 0 {
		return 0, fmt.Errorf("invalid vmid %q", raw)
	}
	return vmid, nil
}

// handleExport streams a vzdump archive of one guest to the caller.
//
// The response is chunked with no Content-Length, because the archive size is
// unknowable before vzdump has produced it. If vzdump fails before writing a
// single byte the caller gets a clean 502 with the stderr tail; if it fails
// after streaming started the connection is aborted deliberately, so the server
// sees a truncated stream and fails the run rather than storing a partial
// archive as if it were complete.
func (h *Helper) handleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !h.authorized(r) {
		writeErr(w, http.StatusUnauthorized, "invalid access secret")
		return
	}
	vmid, err := vmidFrom(r.URL.Path, "/export/")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	fw := &flushWriter{w: w, f: flusherOf(w)}
	w.Header().Set("Content-Type", "application/octet-stream")
	h.log.Info("export started", "vmid", vmid)
	stderr, runErr := h.runner.Run(r.Context(), Command{
		Name: "vzdump", Args: VzdumpArgs(vmid), Stdout: fw,
	})
	if runErr == nil {
		h.log.Info("export finished", "vmid", vmid, "bytes", fw.written)
		if fw.written == 0 {
			// vzdump exited 0 without producing an archive: nothing to restore.
			writeErr(w, http.StatusBadGateway, "vzdump produced no output")
		}
		return
	}
	if fw.written == 0 {
		h.log.Error("vzdump failed before producing output", "vmid", vmid, "error", runErr, "stderr", stderr)
		writeErr(w, http.StatusBadGateway, commandError("vzdump", runErr, stderr))
		return
	}
	h.log.Error("vzdump failed mid-stream, aborting the connection",
		"vmid", vmid, "bytes", fw.written, "error", runErr, "stderr", stderr)
	panic(http.ErrAbortHandler)
}

// handleImport pipes the request body into qmrestore.
func (h *Helper) handleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !h.authorized(r) {
		writeErr(w, http.StatusUnauthorized, "invalid access secret")
		return
	}
	vmid, err := vmidFrom(r.URL.Path, "/import/")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	storage := r.URL.Query().Get("storage")
	force := r.URL.Query().Get("force") == "1"
	h.log.Info("import started", "vmid", vmid, "storage", storage, "force", force)
	stderr, runErr := h.runner.Run(r.Context(), Command{
		Name: "qmrestore", Args: QmrestoreArgs(vmid, storage, force), Stdin: r.Body,
	})
	if runErr != nil {
		h.log.Error("qmrestore failed", "vmid", vmid, "error", runErr, "stderr", stderr)
		writeErr(w, http.StatusBadGateway, commandError("qmrestore", runErr, stderr))
		return
	}
	h.log.Info("import finished", "vmid", vmid)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// commandError renders a failed command as one operator-readable line.
func commandError(name string, runErr error, stderr string) string {
	if stderr == "" {
		return fmt.Sprintf("%s failed: %v", name, runErr)
	}
	return fmt.Sprintf("%s failed: %v: %s", name, runErr, stderr)
}

// flushWriter forwards the command's stdout to the client immediately and counts
// what went out, so the handler knows whether headers are already committed.
type flushWriter struct {
	w       io.Writer
	f       http.Flusher
	written int64
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	f.written += int64(n)
	if f.f != nil {
		f.f.Flush()
	}
	return n, err
}

func flusherOf(w http.ResponseWriter) http.Flusher {
	if f, ok := w.(http.Flusher); ok {
		return f
	}
	return nil
}
