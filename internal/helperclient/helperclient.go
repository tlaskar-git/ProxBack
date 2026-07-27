// Package helperclient is the server-side client for the ProxBack node helper's
// HTTP API: whole-VM export (a vzdump VMA stream), import (piped into qmrestore)
// and a health probe. Transfers are unbounded in time — a multi-hundred-GiB
// vzdump run may take hours — so only the response header wait is bounded.
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
	"time"
)

// HeaderTimeout bounds how long we wait for a helper to start answering.
const HeaderTimeout = 60 * time.Second

// Client talks to node helpers. It is safe for concurrent use.
type Client struct {
	hc  *http.Client
	log *slog.Logger
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
		log: log,
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
	req, err := c.newRequest(ctx, http.MethodGet, target, secret, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("node helper: export vm %d: %w", vmid, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, errorFor(http.MethodGet, target, resp)
	}
	c.log.Debug("node helper export started", "url", target, "vmid", vmid)
	return resp.Body, nil
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
