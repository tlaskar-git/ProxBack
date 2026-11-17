// Package helperclient is the server-side client for the ProxBack node helper's
// HTTP API: whole-VM export (a vzdump VMA stream), import (piped into qmrestore)
// and a health probe. Transfers are unbounded in time — a multi-hundred-GiB
// vzdump run may take hours — so only the wait for the first response byte is
// bounded, and for an export that wait is long (see ExportHeaderTimeout).
package helperclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// HeaderTimeout bounds how long we wait for a helper to start answering a
// request that does no work before it answers (health, import).
const HeaderTimeout = 60 * time.Second

// ExportHeaderTimeout bounds how long an export may take to send its first
// byte. The helper answers an export only when vzdump writes its first byte, and
// vzdump does real work before that: it waits for the node's global vzdump lock
// (held by any other backup job on the node, Proxmox's own included), runs the
// guest-agent fs-freeze and sets up its snapshot. On a busy node that is well
// over a minute, so the general HeaderTimeout fails healthy exports.
const ExportHeaderTimeout = 30 * time.Minute

// Client talks to node helpers. It is safe for concurrent use.
type Client struct {
	hc *http.Client
	// export carries export requests. Its transport has no response header
	// timeout of its own: Export enforces exportHeaderTimeout per request, so
	// the failure can say what the helper was most likely waiting for.
	export              *http.Client
	exportHeaderTimeout time.Duration
	log                 *slog.Logger
}

// SetExportHeaderTimeout overrides ExportHeaderTimeout. Tests use it; a value of
// zero or less restores the default.
func (c *Client) SetExportHeaderTimeout(d time.Duration) {
	if d <= 0 {
		d = ExportHeaderTimeout
	}
	c.exportHeaderTimeout = d
}

// New builds a client.
func New(log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		hc: &http.Client{Transport: &http.Transport{
			ResponseHeaderTimeout: HeaderTimeout,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   2,
		}},
		export: &http.Client{Transport: &http.Transport{
			IdleConnTimeout:     90 * time.Second,
			MaxIdleConnsPerHost: 2,
		}},
		exportHeaderTimeout: ExportHeaderTimeout,
		log:                 log,
	}
}

// Error describes a non-2xx response from a helper.
type Error struct {
	Method string
	URL    string
	Status int
	Body   string
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Body)
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("node helper: %s %s: http %d: %s", e.Method, e.URL, e.Status, msg)
}

// baseURL builds the helper's HTTP root. Helpers speak plain HTTP on the
// management network alongside the Proxmox cluster traffic.
func baseURL(addr string, port int) string {
	if port <= 0 {
		port = 8007
	}
	return "http://" + net.JoinHostPort(addr, strconv.Itoa(port))
}

func (c *Client) newRequest(ctx context.Context, method, url, secret string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("node helper: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	return req, nil
}

// errorFor drains a failed response into an *Error.
func errorFor(method, url string, resp *http.Response) *Error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	body := string(raw)
	// Helpers answer failures as {"error": "..."}; surface just the message.
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Error != "" {
		body = env.Error
	}
	return &Error{Method: method, URL: url, Status: resp.StatusCode, Body: body}
}

// Export opens the VMA stream of a whole guest. The caller must close the
// returned reader. A stream that ends early (the helper aborts the connection
// when vzdump fails mid-run) surfaces as a read error, never as a clean EOF.
func (c *Client) Export(ctx context.Context, addr string, port int, secret string, vmid int) (io.ReadCloser, error) {
	target := fmt.Sprintf("%s/export/%d", baseURL(addr, port), vmid)
	// The request context lives as long as the body: cancelling it is how the
	// header wait is cut short, and it is released when the caller closes the
	// stream.
	reqCtx, cancel := context.WithCancel(ctx)
	req, err := c.newRequest(reqCtx, http.MethodGet, target, secret, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	var timedOut atomic.Bool
	timer := time.AfterFunc(c.exportHeaderTimeout, func() {
		timedOut.Store(true)
		cancel()
	})
	resp, err := c.export.Do(req)
	timer.Stop()
	if err != nil {
		cancel()
		if timedOut.Load() && ctx.Err() == nil {
			return nil, fmt.Errorf("node helper: export vm %d: vzdump sent no data within %s: "+
				"it is most likely waiting for the node's vzdump lock (another backup job on this node) "+
				"or for a guest-agent fs-freeze; check `journalctl -u proxback-helper` and "+
				"/var/log/vzdump/qemu-%d.log on the node", vmid, c.exportHeaderTimeout, vmid)
		}
		return nil, fmt.Errorf("node helper: export vm %d: %w", vmid, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer cancel()
		defer resp.Body.Close()
		return nil, errorFor(http.MethodGet, target, resp)
	}
	c.log.Debug("node helper export started", "url", target, "vmid", vmid)
	return &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}, nil
}

// cancelOnClose releases an export's request context once the stream is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// Import streams a VMA archive into qmrestore on the helper's node. storage
// selects the target storage ("" leaves the archive's own choice in place) and
// force allows overwriting an existing guest with the same vmid.
func (c *Client) Import(ctx context.Context, addr string, port int, secret string, vmid int, storage string, force bool, r io.Reader) error {
	target := fmt.Sprintf("%s/import/%d", baseURL(addr, port), vmid)
	q := url.Values{}
	if storage != "" {
		q.Set("storage", storage)
	}
	if force {
		q.Set("force", "1")
	}
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	req, err := c.newRequest(ctx, http.MethodPost, target, secret, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	// The archive size is not known up front, so stream chunked.
	req.ContentLength = -1
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("node helper: import vm %d: %w", vmid, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errorFor(http.MethodPost, target, resp)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return nil
}

// Health is the helper's /healthz answer.
type Health struct {
	Node    string `json:"node"`
	Version string `json:"version"`
}

// Health probes a helper. The endpoint is unauthenticated, but the secret is
// still sent so the call works against helpers that choose to require it.
func (c *Client) Health(ctx context.Context, addr string, port int, secret string) (Health, error) {
	var out Health
	target := baseURL(addr, port) + "/healthz"
	req, err := c.newRequest(ctx, http.MethodGet, target, secret, nil)
	if err != nil {
		return out, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return out, fmt.Errorf("node helper: health %s: %w", target, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, errorFor(http.MethodGet, target, resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return out, fmt.Errorf("node helper: health %s: read body: %w", target, err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("node helper: health %s: decode response: %w", target, err)
	}
	return out, nil
}
