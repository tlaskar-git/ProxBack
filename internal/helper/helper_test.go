package helper_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"proxback/internal/helper"
	"proxback/internal/helpermgr"
	"proxback/internal/version"
)

// ---------------------------------------------------------------- fake runner

type recordedCall struct {
	Name  string
	Args  []string
	Stdin []byte
}

// fakeRunner stands in for vzdump/qmrestore so the handlers can be exercised on
// any platform, including the Windows box this project is developed on.
type fakeRunner struct {
	// stdout is written to the command's stdout before returning.
	stdout []byte
	// err is the exit error to report.
	err error
	// stderr is the captured stderr tail to report.
	stderr string
	// drainStdin makes the runner read the whole of stdin, like qmrestore does.
	drainStdin bool

	mu    sync.Mutex
	calls []recordedCall
}

func (f *fakeRunner) Run(_ context.Context, c helper.Command) (string, error) {
	rec := recordedCall{Name: c.Name, Args: c.Args}
	if c.Stdin != nil && f.drainStdin {
		raw, err := io.ReadAll(c.Stdin)
		if err != nil {
			return "reading stdin: " + err.Error(), err
		}
		rec.Stdin = raw
	}
	if len(f.stdout) > 0 && c.Stdout != nil {
		// Write in pieces so the handler's streaming path is really used.
		for off := 0; off < len(f.stdout); off += 64 << 10 {
			end := off + (64 << 10)
			if end > len(f.stdout) {
				end = len(f.stdout)
			}
			if _, err := c.Stdout.Write(f.stdout[off:end]); err != nil {
				f.record(rec)
				return f.stderr, err
			}
		}
	}
	f.record(rec)
	return f.stderr, f.err
}

func (f *fakeRunner) record(c recordedCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

func (f *fakeRunner) took() []recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedCall(nil), f.calls...)
}

// ---------------------------------------------------------------- harness

const (
	testSecret = "b6f0ba2f1c8d4e5a9077aa11bb22cc33dd44ee55ff6600112233445566778899"
	testNode   = "pve-test-1"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// enrolled writes a config file as a completed enrollment would and returns a
// helper serving over httptest, so no network enrollment is needed to test the
// export/import handlers.
func enrolled(t *testing.T, runner helper.Runner) (*helper.Helper, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	raw, err := json.Marshal(map[string]any{
		"serverUrl":    "http://proxback.invalid",
		"helperId":     "helper-1",
		"apiKey":       "api-key-1",
		"accessSecret": testSecret,
		"node":         testNode,
		"port":         9007,
	})
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, helper.ConfigFileName), raw, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	h, err := helper.New(helper.Config{ConfigDir: dir, Runner: runner, Logger: discardLog()})
	if err != nil {
		t.Fatalf("new helper: %v", err)
	}
	if err := h.Enroll(context.Background()); err != nil {
		t.Fatalf("load enrollment: %v", err)
	}
	if h.AccessSecret() != testSecret || h.Node() != testNode || h.Port() != 9007 {
		t.Fatalf("stored enrollment not loaded: secret %q node %q port %d",
			h.AccessSecret(), h.Node(), h.Port())
	}
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	return h, srv
}

func request(t *testing.T, method, url, secret string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, body)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func errorBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var out struct {
		Error string `json:"error"`
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return out.Error
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

// ---------------------------------------------------------------- healthz

func TestHealthzIsUnauthenticated(t *testing.T) {
	_, srv := enrolled(t, &fakeRunner{})

	resp := request(t, http.MethodGet, srv.URL+"/healthz", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Node    string `json:"node"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if out.Node != testNode || out.Version != version.Version {
		t.Fatalf("health = %+v, want node %q version %q", out, testNode, version.Version)
	}
}

// ---------------------------------------------------------------- export

func TestExportStreamsVzdumpStdout(t *testing.T) {
	want := pseudoBytes(3<<20, 7) // spans several handler writes
	runner := &fakeRunner{stdout: want}
	_, srv := enrolled(t, runner)

	resp := request(t, http.MethodGet, srv.URL+"/export/103", testSecret, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /export/103 = %d, want 200", resp.StatusCode)
	}
	// The archive size is unknowable up front, so the response must be chunked.
	if resp.ContentLength != -1 {
		t.Fatalf("export declared Content-Length %d, want a chunked response", resp.ContentLength)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read export body: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("export streamed %d bytes, want the %d bytes vzdump produced", len(got), len(want))
	}

	calls := runner.took()
	if len(calls) != 1 {
		t.Fatalf("runner saw %d commands, want 1: %+v", len(calls), calls)
	}
	if calls[0].Name != "vzdump" {
		t.Fatalf("export ran %q, want vzdump", calls[0].Name)
	}
	if got, want := strings.Join(calls[0].Args, " "), "103 --mode snapshot --compress 0 --stdout"; got != want {
		t.Fatalf("vzdump argv = %q, want %q", got, want)
	}
}

func TestExportFailureBeforeOutputSurfacesStderr(t *testing.T) {
	runner := &fakeRunner{
		err:    fmt.Errorf("exit status 255"),
		stderr: "ERROR: Backup of VM 103 failed - no such volume 'local-lvm:vm-103-disk-0'\n",
	}
	_, srv := enrolled(t, runner)

	resp := request(t, http.MethodGet, srv.URL+"/export/103", testSecret, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("failed export = %d, want 502", resp.StatusCode)
	}
	msg := errorBody(t, resp)
	if !strings.Contains(msg, "no such volume") || !strings.Contains(msg, "exit status 255") {
		t.Fatalf("error body = %q, want the vzdump stderr tail and exit status", msg)
	}
}

func TestExportFailureMidStreamAbortsTheConnection(t *testing.T) {
	runner := &fakeRunner{
		stdout: pseudoBytes(128<<10, 11),
		err:    fmt.Errorf("exit status 1"),
		stderr: "ERROR: interrupted by signal",
	}
	_, srv := enrolled(t, runner)

	resp := request(t, http.MethodGet, srv.URL+"/export/103", testSecret, nil)
	defer resp.Body.Close()
	// Headers are long gone by the time vzdump fails, so the only honest signal
	// is a truncated body: the server must never look like a clean 200.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mid-stream failure = %d, want the already-committed 200", resp.StatusCode)
	}
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("reading a truncated export succeeded; the server did not abort the connection")
	}
}

func TestExportRequiresTheAccessSecret(t *testing.T) {
	runner := &fakeRunner{stdout: []byte("never")}
	_, srv := enrolled(t, runner)

	for _, c := range []struct{ what, secret string }{
		{"no bearer token", ""},
		{"a wrong bearer token", strings.Repeat("a", len(testSecret))},
	} {
		resp := request(t, http.MethodGet, srv.URL+"/export/103", c.secret, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("export with %s = %d, want 401", c.what, resp.StatusCode)
		}
		resp.Body.Close()
	}
	if calls := runner.took(); len(calls) != 0 {
		t.Fatalf("unauthorized requests still ran %+v", calls)
	}
}

func TestExportRejectsABadVMID(t *testing.T) {
	_, srv := enrolled(t, &fakeRunner{})
	resp := request(t, http.MethodGet, srv.URL+"/export/not-a-number", testSecret, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("export of a non-numeric vmid = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

// ---------------------------------------------------------------- import

func TestImportPipesTheBodyIntoQmrestore(t *testing.T) {
	payload := pseudoBytes(2<<20, 23)
	runner := &fakeRunner{drainStdin: true}
	_, srv := enrolled(t, runner)

	resp := request(t, http.MethodPost, srv.URL+"/import/9993", testSecret, bytes.NewReader(payload))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /import/9993 = %d, want 200", resp.StatusCode)
	}
	var out struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode import response: %v", err)
	}
	if !out.OK {
		t.Fatal(`import response is not {"ok":true}`)
	}

	calls := runner.took()
	if len(calls) != 1 || calls[0].Name != "qmrestore" {
		t.Fatalf("import ran %+v, want one qmrestore", calls)
	}
	if got, want := strings.Join(calls[0].Args, " "), "- 9993"; got != want {
		t.Fatalf("qmrestore argv = %q, want %q", got, want)
	}
	if !bytes.Equal(calls[0].Stdin, payload) {
		t.Fatalf("qmrestore received %d bytes on stdin, want the %d streamed",
			len(calls[0].Stdin), len(payload))
	}
}

func TestImportArgvCarriesStorageAndForce(t *testing.T) {
	for _, c := range []struct {
		query string
		want  string
	}{
		{"", "- 9993"},
		{"?storage=local-lvm", "- 9993 --storage local-lvm"},
		{"?force=1", "- 9993 --force"},
		{"?storage=nvme%20pool&force=1", "- 9993 --storage nvme pool --force"},
		// Anything other than force=1 is not a request to overwrite a guest.
		{"?force=0", "- 9993"},
	} {
		runner := &fakeRunner{drainStdin: true}
		_, srv := enrolled(t, runner)
		resp := request(t, http.MethodPost, srv.URL+"/import/9993"+c.query, testSecret, strings.NewReader("vma"))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("import%s = %d, want 200", c.query, resp.StatusCode)
		}
		resp.Body.Close()
		calls := runner.took()
		if len(calls) != 1 {
			t.Fatalf("import%s ran %d commands", c.query, len(calls))
		}
		if got := strings.Join(calls[0].Args, " "); got != c.want {
			t.Fatalf("import%s argv = %q, want %q", c.query, got, c.want)
		}
	}
}

func TestImportFailureSurfacesStderr(t *testing.T) {
	runner := &fakeRunner{
		drainStdin: true,
		err:        fmt.Errorf("exit status 2"),
		stderr:     "unable to restore: CT/VM 9993 already exists on node",
	}
	_, srv := enrolled(t, runner)

	resp := request(t, http.MethodPost, srv.URL+"/import/9993", testSecret, strings.NewReader("vma"))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("failed import = %d, want 502", resp.StatusCode)
	}
	if msg := errorBody(t, resp); !strings.Contains(msg, "already exists") {
		t.Fatalf("error body = %q, want the qmrestore stderr", msg)
	}
}

func TestImportRequiresTheAccessSecret(t *testing.T) {
	runner := &fakeRunner{drainStdin: true}
	_, srv := enrolled(t, runner)
	resp := request(t, http.MethodPost, srv.URL+"/import/9993", "", strings.NewReader("vma"))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated import = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
	if calls := runner.took(); len(calls) != 0 {
		t.Fatalf("unauthorized import still ran %+v", calls)
	}
}

// ---------------------------------------------------------------- enrollment

// fakeServer is the ProxBack server as far as enrollment is concerned.
type fakeServer struct {
	mu         sync.Mutex
	registered []helpermgr.RegisterRequest
	heartbeats []string
}

func (f *fakeServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/helpers/register", func(w http.ResponseWriter, r *http.Request) {
		var body helpermgr.RegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.registered = append(f.registered, body)
		n := len(f.registered)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(helpermgr.RegisterResponse{
			HelperID: fmt.Sprintf("helper-%d", n), APIKey: fmt.Sprintf("api-key-%d", n),
		})
	})
	mux.HandleFunc("/api/helpers/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.heartbeats = append(f.heartbeats, r.Header.Get("Authorization"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func (f *fakeServer) registrations() []helpermgr.RegisterRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]helpermgr.RegisterRequest(nil), f.registered...)
}

func (f *fakeServer) beats() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.heartbeats...)
}

func TestEnrollmentRoundTrip(t *testing.T) {
	fake := &fakeServer{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	dir := t.TempDir()

	first, err := helper.New(helper.Config{
		ServerURL: srv.URL + "/", Token: "enroll-me", ConfigDir: dir,
		Port: 8107, Node: "pve9", Logger: discardLog(),
	})
	if err != nil {
		t.Fatalf("new helper: %v", err)
	}
	if err := first.Enroll(context.Background()); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	regs := fake.registrations()
	if len(regs) != 1 {
		t.Fatalf("server saw %d registrations, want 1", len(regs))
	}
	reg := regs[0]
	if reg.Token != "enroll-me" || reg.Node != "pve9" || reg.Port != 8107 {
		t.Fatalf("registration = %+v", reg)
	}
	if reg.Version != version.Version {
		t.Fatalf("registration version = %q, want %q", reg.Version, version.Version)
	}
	if len(reg.AccessSecret) != 64 {
		t.Fatalf("access secret is %d characters, want 64 hex characters", len(reg.AccessSecret))
	}
	if first.HelperID() != "helper-1" || first.AccessSecret() != reg.AccessSecret {
		t.Fatalf("helper state after enrollment = %s / %s", first.HelperID(), first.AccessSecret())
	}

	// The configuration is private to root.
	path := filepath.Join(dir, helper.ConfigFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("config mode = %o, want 600", perm)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(raw), reg.AccessSecret) || !strings.Contains(string(raw), "api-key-1") {
		t.Fatalf("config does not hold the credentials: %s", raw)
	}

	// A restart reuses the stored enrollment: no token, no second registration.
	second, err := helper.New(helper.Config{ConfigDir: dir, Logger: discardLog()})
	if err != nil {
		t.Fatalf("new helper: %v", err)
	}
	if err := second.Enroll(context.Background()); err != nil {
		t.Fatalf("re-enroll: %v", err)
	}
	if len(fake.registrations()) != 1 {
		t.Fatal("restart registered a second time")
	}
	if second.AccessSecret() != reg.AccessSecret || second.HelperID() != "helper-1" {
		t.Fatalf("restart lost the enrollment: %s / %s", second.HelperID(), second.AccessSecret())
	}
	if second.Node() != "pve9" || second.Port() != 8107 {
		t.Fatalf("restart lost node/port: %s:%d", second.Node(), second.Port())
	}

	// Heartbeats carry the API key.
	if err := second.Heartbeat(context.Background()); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	beats := fake.beats()
	if len(beats) != 1 || beats[0] != "Bearer api-key-1" {
		t.Fatalf("heartbeat auth = %v", beats)
	}
}

func TestEnrollmentNeedsServerAndToken(t *testing.T) {
	h, err := helper.New(helper.Config{ConfigDir: t.TempDir(), Logger: discardLog()})
	if err != nil {
		t.Fatalf("new helper: %v", err)
	}
	if err := h.Enroll(context.Background()); err == nil || !strings.Contains(err.Error(), "--server") {
		t.Fatalf("enroll without a server = %v, want a --server complaint", err)
	}
	h, err = helper.New(helper.Config{
		ServerURL: "http://proxback.invalid", ConfigDir: t.TempDir(), Logger: discardLog(),
	})
	if err != nil {
		t.Fatalf("new helper: %v", err)
	}
	if err := h.Enroll(context.Background()); err == nil || !strings.Contains(err.Error(), "--token") {
		t.Fatalf("enroll without a token = %v, want a --token complaint", err)
	}
}

// ---------------------------------------------------------------- run loop

func TestRunServesAndHeartbeats(t *testing.T) {
	fake := &fakeServer{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	// A port well away from the 8007 default, so a real helper (or anything else)
	// on the developer's machine cannot make this test flake.
	const port = 18107
	h, err := helper.New(helper.Config{
		ServerURL: srv.URL, Token: "enroll-me", ConfigDir: t.TempDir(),
		Port: port, Node: "pve9", HeartbeatInterval: 20 * time.Millisecond,
		Runner: &fakeRunner{}, Logger: discardLog(),
	})
	if err != nil {
		t.Fatalf("new helper: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()

	deadline := time.Now().Add(10 * time.Second)
	for len(fake.beats()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(fake.beats()) == 0 {
		cancel()
		<-done
		t.Fatal("helper never heartbeated")
	}
	if h.Port() != port {
		t.Fatalf("helper port = %d, want %d", h.Port(), port)
	}
	// The API is really being served on the configured port.
	resp := request(t, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/healthz", port), "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz on the running helper = %d", resp.StatusCode)
	}
	cancel()
	if err := <-done; err != nil && !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("Run returned %v", err)
	}
}

// ---------------------------------------------------------------- install

// TestServiceUnitMatchesDeployedUnit keeps the unit --install writes identical to
// the one an operator installs by hand from deploy/.
func TestServiceUnitMatchesDeployedUnit(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "proxback-helper.service"))
	if err != nil {
		t.Fatalf("read deploy/proxback-helper.service: %v", err)
	}
	want := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if got := helper.ServiceUnit; got != want {
		t.Fatalf("helper.ServiceUnit differs from deploy/proxback-helper.service:\n--- const ---\n%s\n--- file ---\n%s", got, want)
	}
}

func TestInstructionsMentionEveryStep(t *testing.T) {
	out := helper.Instructions("/tmp/proxback-helper", "/etc/proxback-helper")
	for _, want := range []string{
		helper.InstallPath, helper.UnitPath, "systemctl daemon-reload",
		"systemctl enable --now " + helper.UnitName, "helper.json",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("instructions do not mention %q:\n%s", want, out)
		}
	}
}
