package api

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"proxback/internal/notify"
	"proxback/internal/store"
)

// ---------------------------------------------------------------- dashboard

type last24hDTO struct {
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Running   int `json:"running"`
}

type dashboardDTO struct {
	VMCount         int             `json:"vmCount"`
	AgentCount      int             `json:"agentCount"`
	HostCount       int             `json:"hostCount"`
	TargetCount     int             `json:"targetCount"`
	JobCount        int             `json:"jobCount"`
	Last24h         last24hDTO      `json:"last24h"`
	StorageBytes    int64           `json:"storageBytes"`
	DedupSavedBytes int64           `json:"dedupSavedBytes"`
	RecentRuns      []*store.JobRun `json:"recentRuns"`
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := dashboardDTO{RecentRuns: []*store.JobRun{}}
	var err error
	if out.VMCount, err = s.st.CountVMs(ctx); err != nil {
		s.serverError(w, err)
		return
	}
	if out.AgentCount, err = s.st.CountAgents(ctx); err != nil {
		s.serverError(w, err)
		return
	}
	if out.HostCount, err = s.st.CountPVEHosts(ctx); err != nil {
		s.serverError(w, err)
		return
	}
	if out.TargetCount, err = s.st.CountS3Targets(ctx); err != nil {
		s.serverError(w, err)
		return
	}
	if out.JobCount, err = s.st.CountJobs(ctx); err != nil {
		s.serverError(w, err)
		return
	}
	counts, err := s.st.RunCountsSince(ctx, store.Now().Add(-24*time.Hour))
	if err != nil {
		s.serverError(w, err)
		return
	}
	out.Last24h = last24hDTO{
		Succeeded: counts[store.RunSuccess],
		Failed:    counts[store.RunFailed] + counts[store.RunCanceled],
		Running:   counts[store.RunRunning],
	}
	_, stored, err := s.st.ChunkStats(ctx, "")
	if err != nil {
		s.serverError(w, err)
		return
	}
	out.StorageBytes = stored
	logical, err := s.st.TotalLogicalBytes(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if saved := logical - stored; saved > 0 {
		out.DedupSavedBytes = saved
	}
	runs, err := s.st.ListRuns(ctx, "", 10)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if runs != nil {
		out.RecentRuns = runs
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------- settings

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.st.Settings(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	current, err := s.st.Settings(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	var body struct {
		ServerName  *string `json:"serverName"`
		Concurrency *int    `json:"concurrency"`
		WebhookURL  *string `json:"webhookUrl"`
		NotifyOn    *string `json:"notifyOn"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.ServerName != nil {
		current.ServerName = strings.TrimSpace(*body.ServerName)
	}
	if body.Concurrency != nil {
		if *body.Concurrency < 1 {
			writeError(w, http.StatusBadRequest, "concurrency must be at least 1")
			return
		}
		current.Concurrency = *body.Concurrency
	}
	if body.WebhookURL != nil {
		url := strings.TrimSpace(*body.WebhookURL)
		if err := validateWebhookURL(url); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		current.WebhookURL = url
	}
	if body.NotifyOn != nil {
		if !store.ValidNotifyOn(*body.NotifyOn) {
			writeError(w, http.StatusBadRequest, `notifyOn must be "off", "failures" or "all"`)
			return
		}
		current.NotifyOn = *body.NotifyOn
	}
	if err := s.st.SaveSettings(r.Context(), current); err != nil {
		s.serverError(w, err)
		return
	}
	saved, err := s.st.Settings(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.sched.SetConcurrency(saved.Concurrency)
	writeJSON(w, http.StatusOK, saved)
}

// validateWebhookURL keeps the stored value to something the notifier can
// actually POST to. An empty URL is valid and disables notifications.
func validateWebhookURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("webhookUrl is not a valid URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("webhookUrl must be an http:// or https:// URL")
	}
	return nil
}

// handleTestWebhook posts a sample payload to the saved webhook URL so the
// operator can confirm the endpoint before relying on it.
func (s *Server) handleTestWebhook(w http.ResponseWriter, r *http.Request) {
	settings, err := s.st.Settings(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	if settings.WebhookURL == "" {
		writeError(w, http.StatusBadRequest, "no webhookUrl is configured")
		return
	}
	now := store.Now()
	payload := notify.Payload{
		Event:      notify.EventRunFinished,
		Server:     settings.ServerName,
		Job:        "Webhook test",
		Kind:       store.SourceVM,
		Status:     store.RunSuccess,
		StartedAt:  now,
		FinishedAt: now,
	}
	if err := s.notifier.Send(r.Context(), settings.WebhookURL, payload); err != nil {
		s.log.Warn("webhook test failed", "url", settings.WebhookURL, "error", err)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- downloads

// allowedDownloads maps the public download names to their file names in
// <dataDir>/downloads. The map is the whole of the name validation: nothing
// outside it can be requested, so no path can escape the downloads directory.
var allowedDownloads = map[string]string{
	"proxback-agent-linux-amd64":       "proxback-agent-linux-amd64",
	"proxback-agent-windows-amd64.exe": "proxback-agent-windows-amd64.exe",
	"proxback-helper-linux-amd64":      "proxback-helper-linux-amd64",
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	file, ok := allowedDownloads[name]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown download")
		return
	}
	full := filepath.Join(s.dataDir, "downloads", file)
	f, err := os.Open(full)
	if err != nil {
		writeError(w, http.StatusNotFound,
			"binary not available on this server; build it and place it in <data>/downloads/")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		writeError(w, http.StatusNotFound, "binary not available")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeContent(w, r, name, st.ModTime(), f)
}

// ---------------------------------------------------------------- SPA

func (s *Server) serveSPA(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" || name == "." {
		name = "index.html"
	}
	if s.serveEmbedded(w, r, name) {
		return
	}
	if s.serveEmbedded(w, r, "index.html") {
		return
	}
	writeError(w, http.StatusNotFound, "web UI is not available in this build")
}

func (s *Server) serveEmbedded(w http.ResponseWriter, r *http.Request, name string) bool {
	f, err := s.spa.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		return false
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		raw, err := io.ReadAll(f)
		if err != nil {
			return false
		}
		rs = bytes.NewReader(raw)
	}
	http.ServeContent(w, r, name, time.Time{}, rs)
	return true
}
