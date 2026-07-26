// Package agent implements the ProxBack in-guest agent: enrollment, the
// heartbeat/poll loop, file-level backup as a tar stream chunked through the
// server, and restore download and unpacking.
package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"proxback/internal/agentmgr"
	"proxback/internal/engine"
)

// Version is the agent build version reported at registration.
const Version = "1.0.0"

// DefaultHeartbeatInterval is how often the agent polls the server.
const DefaultHeartbeatInterval = 15 * time.Second

// ConfigFileName is the name of the agent's stored configuration.
const ConfigFileName = "agent.json"

// StreamName is the manifest stream name used for file backups.
const StreamName = "files.tar"

// Config configures an agent.
type Config struct {
	ServerURL         string
	Token             string
	ConfigDir         string
	HeartbeatInterval time.Duration
	InsecureTLS       bool
	Logger            *slog.Logger
	HTTPClient        *http.Client
}

type storedConfig struct {
	ServerURL string `json:"serverUrl"`
	AgentID   string `json:"agentId"`
	APIKey    string `json:"apiKey"`
}

// Agent is a running ProxBack agent.
type Agent struct {
	cfg  Config
	log  *slog.Logger
	hc   *http.Client
	self storedConfig
}

// New builds an agent.
func New(cfg Config) (*Agent, error) {
	if cfg.ConfigDir == "" {
		return nil, errors.New("agent: config dir required")
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
		tr := &http.Transport{MaxIdleConnsPerHost: 4}
		if cfg.InsecureTLS {
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // operator opt-in
		}
		hc = &http.Client{Transport: tr, Timeout: 10 * time.Minute}
	}
	return &Agent{cfg: cfg, log: log, hc: hc}, nil
}

func (a *Agent) configPath() string { return filepath.Join(a.cfg.ConfigDir, ConfigFileName) }

func (a *Agent) loadConfig() error {
	raw, err := os.ReadFile(a.configPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("agent: read config: %w", err)
	}
	if err := json.Unmarshal(raw, &a.self); err != nil {
		return fmt.Errorf("agent: parse config: %w", err)
	}
	return nil
}

func (a *Agent) saveConfig() error {
	if err := os.MkdirAll(a.cfg.ConfigDir, 0o700); err != nil {
		return fmt.Errorf("agent: create config dir: %w", err)
	}
	raw, err := json.MarshalIndent(a.self, "", "  ")
	if err != nil {
		return fmt.Errorf("agent: encode config: %w", err)
	}
	if err := os.WriteFile(a.configPath(), raw, 0o600); err != nil {
		return fmt.Errorf("agent: write config: %w", err)
	}
	return nil
}

// AgentID returns the enrolled agent id (empty before registration).
func (a *Agent) AgentID() string { return a.self.AgentID }

// Run enrolls if necessary and then loops until the context is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.Enroll(ctx); err != nil {
		return err
	}
	a.log.Info("agent running", "server", a.self.ServerURL, "agentId", a.self.AgentID,
		"interval", a.cfg.HeartbeatInterval.String())
	ticker := time.NewTicker(a.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		if err := a.pollOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			a.log.Warn("heartbeat failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// RunOnce enrolls and performs a single heartbeat/poll cycle.
func (a *Agent) RunOnce(ctx context.Context) error {
	if err := a.Enroll(ctx); err != nil {
		return err
	}
	return a.pollOnce(ctx)
}

// Enroll loads the stored configuration, registering with the server when no API
// key is present yet.
func (a *Agent) Enroll(ctx context.Context) error {
	if err := a.loadConfig(); err != nil {
		return err
	}
	if a.cfg.ServerURL != "" {
		a.self.ServerURL = strings.TrimRight(a.cfg.ServerURL, "/")
	}
	if a.self.ServerURL == "" {
		return errors.New("agent: --server is required on first run")
	}
	if a.self.APIKey != "" {
		return nil
	}
	if a.cfg.Token == "" {
		return errors.New("agent: no stored API key; pass --token with an enrollment token")
	}
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "proxback-agent"
	}
	req := agentmgr.RegisterRequest{
		Token:    a.cfg.Token,
		Hostname: hostname,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		Version:  Version,
	}
	var res agentmgr.RegisterResponse
	if err := a.doJSON(ctx, http.MethodPost, "/api/agents/register", req, &res, false); err != nil {
		return fmt.Errorf("agent: register: %w", err)
	}
	a.self.AgentID = res.AgentID
	a.self.APIKey = res.APIKey
	if err := a.saveConfig(); err != nil {
		return err
	}
	a.log.Info("agent enrolled", "agentId", res.AgentID, "hostname", hostname)
	return nil
}

// pollOnce sends a heartbeat and executes any dispatched work.
func (a *Agent) pollOnce(ctx context.Context) error {
	var res struct {
		Jobs []agentmgr.Dispatch `json:"jobs"`
	}
	if err := a.doJSON(ctx, http.MethodPost, "/api/agents/heartbeat", map[string]any{}, &res, true); err != nil {
		return err
	}
	for _, d := range res.Jobs {
		if err := ctx.Err(); err != nil {
			return err
		}
		a.log.Info("dispatch received", "type", d.Type, "run", d.RunID)
		var err error
		switch d.Type {
		case agentmgr.DispatchBackup:
			err = a.runBackup(ctx, d)
		case agentmgr.DispatchRestore:
			err = a.runRestore(ctx, d)
		default:
			err = fmt.Errorf("unsupported dispatch type %q", d.Type)
		}
		if err != nil {
			a.log.Error("dispatch failed", "type", d.Type, "run", d.RunID, "error", err)
			a.reportFailure(ctx, d.RunID, err)
		}
	}
	return nil
}

func (a *Agent) reportFailure(ctx context.Context, runID string, cause error) {
	body := map[string]string{"error": cause.Error()}
	failCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := a.doJSON(failCtx, http.MethodPost, "/api/agents/runs/"+runID+"/fail", body, nil, true); err != nil {
		a.log.Warn("could not report failure", "run", runID, "error", err)
	}
}

// ---------------------------------------------------------------- backup

func (a *Agent) runBackup(ctx context.Context, d agentmgr.Dispatch) error {
	if len(d.Paths) == 0 {
		return errors.New("backup dispatch has no include paths")
	}
	chunkSize := int(d.ChunkSize)
	if chunkSize <= 0 {
		chunkSize = engine.ChunkSize
	}
	total, err := estimateSize(d.Paths)
	if err != nil {
		return err
	}

	pr, pw := io.Pipe()
	tarErr := make(chan error, 1)
	go func() {
		err := writeTar(pw, d.Paths)
		_ = pw.CloseWithError(err)
		tarErr <- err
	}()

	stream := engine.DiskManifest{Name: StreamName, Chunks: []engine.Chunk{}}
	buf := make([]byte, chunkSize)
	uploadErr := func() error {
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			n, readErr := io.ReadFull(pr, buf)
			if n > 0 {
				sum := sha256.Sum256(buf[:n])
				sha := hex.EncodeToString(sum[:])
				if err := a.uploadChunk(ctx, d.RunID, sha, buf[:n], total); err != nil {
					return err
				}
				stream.Chunks = append(stream.Chunks, engine.Chunk{Sha256: sha, Size: int64(n)})
				stream.SizeBytes += int64(n)
			}
			switch {
			case readErr == nil:
				continue
			case errors.Is(readErr, io.EOF), errors.Is(readErr, io.ErrUnexpectedEOF):
				return nil
			default:
				return fmt.Errorf("agent: read tar stream: %w", readErr)
			}
		}
	}()
	_ = pr.CloseWithError(uploadErr)
	if terr := <-tarErr; terr != nil && uploadErr == nil {
		uploadErr = terr
	}
	if uploadErr != nil {
		return uploadErr
	}

	complete := agentmgr.CompleteRequest{Disks: []engine.DiskManifest{stream}}
	if err := a.doJSON(ctx, http.MethodPost, "/api/agents/runs/"+d.RunID+"/complete", complete, nil, true); err != nil {
		return fmt.Errorf("agent: complete run: %w", err)
	}
	a.log.Info("backup uploaded", "run", d.RunID, "bytes", stream.SizeBytes, "chunks", len(stream.Chunks))
	return nil
}

func (a *Agent) uploadChunk(ctx context.Context, runID, sha string, data []byte, total int64) error {
	url := a.self.ServerURL + "/api/agents/runs/" + runID + "/chunks"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("agent: build chunk request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.self.APIKey)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Chunk-Sha256", sha)
	if total > 0 {
		req.Header.Set("X-Total-Bytes", strconv.FormatInt(total, 10))
	}
	req.ContentLength = int64(len(data))
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("agent: upload chunk: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("agent: upload chunk: http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

// ---------------------------------------------------------------- restore

func (a *Agent) runRestore(ctx context.Context, d agentmgr.Dispatch) error {
	if d.DestPath == "" {
		return errors.New("restore dispatch has no destination path")
	}
	url := a.self.ServerURL + "/api/agents/restores/" + d.RunID + "/stream"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("agent: build restore request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.self.APIKey)
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("agent: restore stream: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("agent: restore stream: http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if err := extractTar(resp.Body, d.DestPath); err != nil {
		return err
	}
	if err := a.doJSON(ctx, http.MethodPost, "/api/agents/runs/"+d.RunID+"/complete",
		agentmgr.CompleteRequest{}, nil, true); err != nil {
		return fmt.Errorf("agent: complete restore: %w", err)
	}
	a.log.Info("restore unpacked", "run", d.RunID, "dest", d.DestPath)
	return nil
}

// ---------------------------------------------------------------- transport

func (a *Agent) doJSON(ctx context.Context, method, path string, body, out any, withKey bool) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("agent: encode request: %w", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.self.ServerURL+path, rdr)
	if err != nil {
		return fmt.Errorf("agent: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if withKey {
		req.Header.Set("Authorization", "Bearer "+a.self.APIKey)
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("agent: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("agent: %s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("agent: %s %s: http %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("agent: %s %s: decode response: %w", method, path, err)
	}
	return nil
}
