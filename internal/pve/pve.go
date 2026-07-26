// Package pve is a minimal Proxmox VE API client covering the endpoints ProxBack
// needs: node/guest inventory, guest config, snapshots, task polling and the
// ProxBack disk export/import extension endpoints.
package pve

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config configures a client.
type Config struct {
	BaseURL     string // e.g. https://pve.example.com:8006
	TokenID     string // e.g. root@pam!proxback
	TokenSecret string
	InsecureTLS bool
	Timeout     time.Duration
	Logger      *slog.Logger
}

// Client talks to one Proxmox VE host.
type Client struct {
	baseURL string
	tokenID string
	secret  string
	hc      *http.Client
	log     *slog.Logger
}

// New builds a client. Streaming requests are not subject to the configured
// timeout (it is applied as a dial/response-header timeout only).
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("pve: base url required")
	}
	if _, err := url.Parse(cfg.BaseURL); err != nil {
		return nil, fmt.Errorf("pve: parse base url: %w", err)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	tr := &http.Transport{
		ResponseHeaderTimeout: timeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   4,
	}
	if cfg.InsecureTLS {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // operator opt-in
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		tokenID: cfg.TokenID,
		secret:  cfg.TokenSecret,
		hc:      &http.Client{Transport: tr},
		log:     log,
	}, nil
}

// AuthHeader returns the value ProxBack sends in the Authorization header.
func (c *Client) AuthHeader() string {
	return fmt.Sprintf("PVEAPIToken=%s=%s", c.tokenID, c.secret)
}

func (c *Client) newRequest(ctx context.Context, method, path string, query url.Values, body io.Reader) (*http.Request, error) {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, fmt.Errorf("pve: build request: %w", err)
	}
	req.Header.Set("Authorization", c.AuthHeader())
	return req, nil
}

// APIError describes a non-2xx response from the PVE API.
type APIError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("pve: %s %s: http %d: %s", e.Method, e.Path, e.Status, strings.TrimSpace(e.Body))
}

// do performs a request and decodes the PVE {"data": ...} envelope into out.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, form url.Values, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := c.newRequest(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("pve: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("pve: %s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{Method: method, Path: path, Status: resp.StatusCode, Body: string(raw)}
	}
	if out == nil {
		return nil
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("pve: %s %s: decode envelope: %w", method, path, err)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("pve: %s %s: decode data: %w", method, path, err)
	}
	return nil
}

// Node is a cluster node.
type Node struct {
	Node   string `json:"node"`
	Status string `json:"status"`
}

// Nodes lists cluster nodes.
func (c *Client) Nodes(ctx context.Context) ([]Node, error) {
	var out []Node
	if err := c.do(ctx, http.MethodGet, "/api2/json/nodes", nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// VM is a QEMU guest as reported by the API.
type VM struct {
	VMID    int    `json:"vmid"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	MaxDisk int64  `json:"maxdisk"`
	MaxMem  int64  `json:"maxmem"`
	Uptime  int64  `json:"uptime"`
	Node    string `json:"-"`
}

// VMs lists QEMU guests on a node.
func (c *Client) VMs(ctx context.Context, node string) ([]VM, error) {
	var out []VM
	path := "/api2/json/nodes/" + url.PathEscape(node) + "/qemu"
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Node = node
	}
	return out, nil
}

// AllVMs lists QEMU guests across every node.
func (c *Client) AllVMs(ctx context.Context) ([]VM, error) {
	nodes, err := c.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	var out []VM
	for _, n := range nodes {
		vms, err := c.VMs(ctx, n.Node)
		if err != nil {
			return nil, err
		}
		out = append(out, vms...)
	}
	return out, nil
}

// Config returns the raw guest configuration map.
func (c *Client) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	out := map[string]any{}
	path := fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/config", url.PathEscape(node), vmid)
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DiskInfo is a virtual disk discovered in a guest config.
type DiskInfo struct {
	Name      string // config key, e.g. scsi0
	Volume    string // storage volume reference
	SizeBytes int64
}

var diskKeyRe = regexp.MustCompile(`^(scsi|virtio|sata|ide)\d+$`)
var sizeRe = regexp.MustCompile(`(?:^|,)size=([0-9]+(?:\.[0-9]+)?)([KMGTP]?)`)

// ParseDisks extracts the backup-eligible disks from a guest config map.
func ParseDisks(cfg map[string]any) []DiskInfo {
	var out []DiskInfo
	for k, v := range cfg {
		if !diskKeyRe.MatchString(k) {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		if strings.Contains(s, "media=cdrom") {
			continue
		}
		d := DiskInfo{Name: k}
		if i := strings.Index(s, ","); i >= 0 {
			d.Volume = s[:i]
		} else {
			d.Volume = s
		}
		if m := sizeRe.FindStringSubmatch(s); m != nil {
			d.SizeBytes = parseSize(m[1], m[2])
		}
		out = append(out, d)
	}
	sortDisks(out)
	return out
}

func sortDisks(d []DiskInfo) {
	for i := 1; i < len(d); i++ {
		for j := i; j > 0 && d[j].Name < d[j-1].Name; j-- {
			d[j], d[j-1] = d[j-1], d[j]
		}
	}
}

func parseSize(num, unit string) int64 {
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0
	}
	mult := float64(1)
	switch unit {
	case "K":
		mult = 1 << 10
	case "M":
		mult = 1 << 20
	case "G":
		mult = 1 << 30
	case "T":
		mult = 1 << 40
	case "P":
		mult = 1 << 50
	}
	return int64(f * mult)
}

// CreateSnapshot creates a guest snapshot and returns the task UPID.
func (c *Client) CreateSnapshot(ctx context.Context, node string, vmid int, snapname string) (string, error) {
	var upid string
	path := fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/snapshot", url.PathEscape(node), vmid)
	form := url.Values{"snapname": {snapname}}
	if err := c.do(ctx, http.MethodPost, path, nil, form, &upid); err != nil {
		return "", err
	}
	return upid, nil
}

// DeleteSnapshot removes a guest snapshot and returns the task UPID.
func (c *Client) DeleteSnapshot(ctx context.Context, node string, vmid int, snapname string) (string, error) {
	var upid string
	path := fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/snapshot/%s", url.PathEscape(node), vmid, url.PathEscape(snapname))
	if err := c.do(ctx, http.MethodDelete, path, nil, nil, &upid); err != nil {
		return "", err
	}
	return upid, nil
}

// TaskStatus is the result of a task status poll.
type TaskStatus struct {
	UPID       string `json:"upid"`
	Status     string `json:"status"`
	ExitStatus string `json:"exitstatus"`
}

// Done reports whether the task has finished.
func (t TaskStatus) Done() bool { return t.Status == "stopped" }

// OK reports whether the task finished successfully.
func (t TaskStatus) OK() bool { return t.Done() && (t.ExitStatus == "OK" || t.ExitStatus == "") }

// TaskStatus polls one task.
func (c *Client) TaskStatus(ctx context.Context, node, upid string) (TaskStatus, error) {
	var out TaskStatus
	path := fmt.Sprintf("/api2/json/nodes/%s/tasks/%s/status", url.PathEscape(node), url.PathEscape(upid))
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return out, err
	}
	return out, nil
}

// WaitTask polls a task until it finishes, errors, or the context is done.
func (c *Client) WaitTask(ctx context.Context, node, upid string, timeout time.Duration) error {
	if upid == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	interval := 200 * time.Millisecond
	for {
		st, err := c.TaskStatus(ctx, node, upid)
		if err != nil {
			return err
		}
		if st.Done() {
			if !st.OK() {
				return fmt.Errorf("pve: task %s failed: %s", upid, st.ExitStatus)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pve: task %s did not finish within %s", upid, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
		if interval < 2*time.Second {
			interval *= 2
		}
	}
}

// DiskStream is an open disk export.
type DiskStream struct {
	io.ReadCloser
	// Size is the disk size in bytes when the server reported a Content-Length.
	Size int64
}

// ExportDisk opens the raw disk stream of a guest disk. When snapshot is
// non-empty the snapshot's view of the disk is exported.
func (c *Client) ExportDisk(ctx context.Context, node string, vmid int, disk, snapshot string) (*DiskStream, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/proxback-export/%s", url.PathEscape(node), vmid, url.PathEscape(disk))
	q := url.Values{}
	if snapshot != "" {
		q.Set("snapshot", snapshot)
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, q, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pve: export %s vm %d disk %s: %w", node, vmid, disk, err)
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return nil, &APIError{Method: http.MethodGet, Path: path, Status: resp.StatusCode, Body: string(raw)}
	}
	return &DiskStream{ReadCloser: resp.Body, Size: resp.ContentLength}, nil
}

// ImportDisk streams a restored disk image back to the host. size may be -1 when
// unknown, in which case chunked transfer encoding is used.
func (c *Client) ImportDisk(ctx context.Context, node string, vmid int, disk string, r io.Reader, size int64) error {
	path := fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/proxback-import/%s", url.PathEscape(node), vmid, url.PathEscape(disk))
	req, err := c.newRequest(ctx, http.MethodPost, path, nil, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if size >= 0 {
		req.ContentLength = size
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("pve: import %s vm %d disk %s: %w", node, vmid, disk, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{Method: http.MethodPost, Path: path, Status: resp.StatusCode, Body: string(raw)}
	}
	return nil
}

// FindNodeForVM locates the node hosting a guest.
func (c *Client) FindNodeForVM(ctx context.Context, vmid int) (string, error) {
	vms, err := c.AllVMs(ctx)
	if err != nil {
		return "", err
	}
	for _, v := range vms {
		if v.VMID == vmid {
			return v.Node, nil
		}
	}
	return "", fmt.Errorf("pve: vm %d not found on host", vmid)
}
