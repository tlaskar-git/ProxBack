package sched

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"proxback/internal/store"
)

// Policy errors. They are distinct values because each one is a different
// answer to "why did my backup not do what the policy says?", and the API and
// the run log both have to be able to tell them apart.
var (
	// ErrOutsideWindow is returned when a scheduled run would start outside the
	// job's backup window. A manual run is never refused for this reason.
	ErrOutsideWindow = errors.New("sched: outside the job's backup window")
	// ErrMaxDuration is returned when a run outlived policy.maxDurationMinutes
	// and was cancelled because of it.
	ErrMaxDuration = errors.New("sched: run exceeded its maximum duration")
	// ErrExcludeDisksUnsupported is returned when a guest would be backed up by
	// a node helper — one whole-VM vzdump archive — while the policy asks for
	// individual disks to be left out. vzdump has no per-disk exclusion on the
	// stdout path (disks are excluded in the guest's own configuration with
	// backup=0), so honouring the policy is impossible and pretending to have
	// honoured it would be a lie about what the restore point contains.
	ErrExcludeDisksUnsupported = errors.New("sched: policy.excludeDisks cannot be honoured on the node-helper path")
	// ErrScriptNeedsHelper is returned when a vm job carries a pre/post script
	// but the guest's node has no helper to run it on: the scripts are meant to
	// run where the data lives, and there is nowhere to run them.
	ErrScriptNeedsHelper = errors.New("sched: policy scripts need a ProxBack node helper on the guest's node")
	// ErrScriptFailed wraps a non-zero script exit.
	ErrScriptFailed = errors.New("sched: policy script failed")
)

// Trigger origins. The origin decides one thing only: whether the job's backup
// window may refuse the run.
const (
	TriggerManual    = "manual"
	TriggerScheduled = "scheduled"
)

// scriptOutputTail bounds how much script output is copied into the run log.
// A script that prints a megabyte still gets its last words recorded, and the
// run log stays readable.
const scriptOutputTail = 4 << 10

// scriptHTTPHeaderTimeout bounds how long the server waits for a helper to
// start answering a script request. The script's own timeout is enforced on the
// helper, and again here by the request context.
const scriptHTTPHeaderTimeout = 30 * time.Second

// Script phases, as they appear in the run log and in the helper request.
const (
	phasePre  = "pre"
	phasePost = "post"
)

// windowCheck reports whether a run may start now under the job's policy. A
// manual run is always allowed; the second result is the sentence the run log
// records so the operator can see the window was consciously overridden.
func windowCheck(policy store.JobPolicy, origin string, now time.Time) (allowed bool, note string) {
	w := policy.Window
	if w == nil {
		return true, ""
	}
	if w.Contains(now) {
		return true, ""
	}
	if origin == TriggerManual {
		return true, fmt.Sprintf("started outside the backup window (%s) — a manual run is always allowed", w)
	}
	return false, fmt.Sprintf("outside the backup window (%s)", w)
}

// policyMinutes converts a policy's minute count into a duration. Tests shrink
// the unit rather than skipping the wait, so what they exercise is the real
// code path with real timers.
func (m *Manager) policyMinutes(n int) time.Duration {
	unit := m.policyMinute
	if unit <= 0 {
		unit = time.Minute
	}
	return time.Duration(n) * unit
}

// retryWait blocks for d, or until the run is cancelled.
func (m *Manager) retryWait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ---------------------------------------------------------------- quiescing

// guestAgentEnabled reads a guest's configuration for an enabled
// qemu-guest-agent. Proxmox writes the option as "1", "0" or a property string
// such as "enabled=1,fstrim_cloned_disks=1".
func guestAgentEnabled(cfg map[string]any) bool {
	raw, ok := cfg["agent"]
	if !ok {
		return false
	}
	var v string
	switch t := raw.(type) {
	case string:
		v = t
	case float64:
		return t != 0
	case int:
		return t != 0
	case bool:
		return t
	default:
		return false
	}
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return false
	}
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		switch part {
		case "1", "on", "true", "yes", "enabled=1", "enabled=on", "enabled=true":
			return true
		case "0", "off", "false", "no", "enabled=0", "enabled=off", "enabled=false":
			return false
		}
	}
	return false
}

// logQuiesce records what actually happened to the guest's filesystem before it
// was read. ProxBack never claims a freeze it did not get: the whole value of
// the setting is that the run log distinguishes a quiesced restore point from a
// crash-consistent one.
func (m *Manager) logQuiesce(ctx context.Context, runID string, policy store.JobPolicy, p vmPlan) {
	if policy.Quiesce != store.QuiesceGuestAgent {
		return
	}
	switch {
	case p.helper == nil:
		m.logRun(ctx, runID, "warning: %s: policy asks for guest-agent quiescing, but this guest is read "+
			"through the Proxmox disk-export path, which cannot freeze the filesystem — "+
			"this restore point is crash-consistent", p.name)
	case !p.guestAgent:
		m.logRun(ctx, runID, "warning: %s: policy asks for guest-agent quiescing, but qemu-guest-agent is "+
			"not enabled on this guest — vzdump cannot freeze the filesystem, so this restore point is "+
			"crash-consistent", p.name)
	default:
		m.logRun(ctx, runID, "%s: guest-agent quiescing requested and qemu-guest-agent is enabled; "+
			"vzdump freezes the guest filesystem for the snapshot", p.name)
	}
}

// ---------------------------------------------------------------- scripts

// scriptRequest is the body of the node helper's POST /script.
type scriptRequest struct {
	Script         string `json:"script"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
	Phase          string `json:"phase"`
}

// scriptResponse is the helper's answer. Output is the combined stdout/stderr
// tail; Error is set when the script exited non-zero or was killed.
type scriptResponse struct {
	OK     bool   `json:"ok"`
	Output string `json:"output"`
	Error  string `json:"error"`
}

// runHelperScript executes one policy script on a node helper and returns its
// captured output. The script body itself never leaves this function: it is
// sent to the helper and nothing logs it, because an operator's script can hold
// anything from a database password to a paging key.
func (m *Manager) runHelperScript(ctx context.Context, h *store.NodeHelper, phase, script string, timeout time.Duration) (string, error) {
	body, err := json.Marshal(scriptRequest{
		Script:         script,
		TimeoutSeconds: int(timeout / time.Second),
		Phase:          phase,
	})
	if err != nil {
		return "", fmt.Errorf("encode script request: %w", err)
	}
	// The helper enforces the timeout too; this is the transport's own bound so
	// a wedged helper cannot hold a run open forever.
	reqCtx, cancel := context.WithTimeout(ctx, timeout+scriptHTTPHeaderTimeout)
	defer cancel()
	target := "http://" + net.JoinHostPort(h.Address, strconv.Itoa(helperPort(h))) + "/script"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build script request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.AccessSecret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.scriptClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("run %s-script on node %s: %w", phase, h.Node, err)
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return "", fmt.Errorf("run %s-script on node %s: read response: %w", phase, h.Node, readErr)
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return "", fmt.Errorf("run %s-script on node %s: this node helper is too old to run policy scripts — "+
			"redeploy it from Hosts → Node helpers", phase, h.Node)
	}
	var out scriptResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("run %s-script on node %s: http %d: %s",
			phase, h.Node, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if resp.StatusCode != http.StatusOK || !out.OK {
		msg := strings.TrimSpace(out.Error)
		if msg == "" {
			msg = fmt.Sprintf("http %d", resp.StatusCode)
		}
		return out.Output, fmt.Errorf("%w: %s-script on node %s: %s", ErrScriptFailed, phase, h.Node, msg)
	}
	return out.Output, nil
}

// helperPort resolves the port a helper listens on.
func helperPort(h *store.NodeHelper) int {
	if h.Port > 0 {
		return h.Port
	}
	return store.DefaultHelperPort
}

// scriptClient returns the HTTP client policy scripts travel on.
func (m *Manager) scriptClient() *http.Client {
	if m.httpClient != nil {
		return m.httpClient
	}
	return &http.Client{Transport: &http.Transport{
		ResponseHeaderTimeout: scriptHTTPHeaderTimeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   2,
	}}
}

// runVMScript runs one phase of a vm job's policy scripts for a single guest,
// on the helper that owns its node — which is where the guest's data lives.
//
// A guest read through the Proxmox disk-export path has no helper, so there is
// nowhere the script could run; that is an error rather than a silent skip.
func (m *Manager) runVMScript(ctx context.Context, runID string, policy store.JobPolicy, p vmPlan, phase string) error {
	script := policy.PreScript
	if phase == phasePost {
		script = policy.PostScript
	}
	if script == "" {
		return nil
	}
	if p.helper == nil {
		return fmt.Errorf("%w: %s needs a helper on node %q to run the %s-script",
			ErrScriptNeedsHelper, p.name, p.node, phase)
	}
	timeout := time.Duration(policy.ScriptTimeoutSecondsOrDefault()) * time.Second
	started := time.Now()
	m.logRun(ctx, runID, "%s: running the %s-script on node %s (timeout %s)",
		p.name, phase, p.node, timeout)
	output, err := m.runHelperScript(ctx, p.helper, phase, script, timeout)
	m.logScriptOutput(ctx, runID, p.name, phase, output)
	if err != nil {
		m.logRun(ctx, runID, "%s: the %s-script failed after %s: %v",
			p.name, phase, time.Since(started).Round(time.Millisecond), err)
		return err
	}
	m.logRun(ctx, runID, "%s: the %s-script finished in %s",
		p.name, phase, time.Since(started).Round(time.Millisecond))
	return nil
}

// logScriptOutput copies a bounded tail of a script's combined output into the
// run log. The script body is never logged — only what it said.
func (m *Manager) logScriptOutput(ctx context.Context, runID, source, phase, output string) {
	output = strings.TrimSpace(output)
	if output == "" {
		return
	}
	truncated := false
	if len(output) > scriptOutputTail {
		output = output[len(output)-scriptOutputTail:]
		truncated = true
	}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		m.logRun(ctx, runID, "%s: %s-script: %s", source, phase, line)
	}
	if truncated {
		m.logRun(ctx, runID, "%s: %s-script: (earlier output omitted; only the last %d bytes are kept)",
			source, phase, scriptOutputTail)
	}
}
