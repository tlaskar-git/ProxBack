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
	"proxback/internal/update"
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

// settingsDTO is the stored settings plus the read-only facts about the server
// the UI needs to explain them. Schedules are expressed in the server's local
// zone, so the UI has to be able to say which one that is.
type settingsDTO struct {
	store.Settings
	// Timezone is the server's local zone name, e.g. "Europe/London". It is
	// read-only: PUT ignores it.
	Timezone string `json:"timezone"`
}

func (s *Server) settingsDTO(settings store.Settings) settingsDTO {
	return settingsDTO{Settings: settings, Timezone: localTimezone()}
}

// localTimezone names the zone the server schedules jobs in. Go's time.Local is
// called "Local" when the zone came from /etc/localtime rather than from $TZ, so
// the IANA name is recovered from the usual system files; the zone abbreviation
// is the last resort (which is what a Windows host reports).
func localTimezone() string {
	if name := time.Local.String(); name != "" && name != "Local" && name != "UTC" {
		return name
	}
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		if name := zoneNameFromPath(link); name != "" {
			return name
		}
	}
	if raw, err := os.ReadFile("/etc/timezone"); err == nil {
		if name := strings.TrimSpace(string(raw)); name != "" {
			return name
		}
	}
	if name := time.Local.String(); name != "" {
		return name
	}
	zone, _ := time.Now().Zone()
	if zone == "" {
		return "UTC"
	}
	return zone
}

// zoneNameFromPath extracts "Europe/London" from a zoneinfo path such as
// /usr/share/zoneinfo/Europe/London.
func zoneNameFromPath(p string) string {
	p = filepath.ToSlash(p)
	const marker = "zoneinfo/"
	i := strings.LastIndex(p, marker)
	if i < 0 {
		return ""
	}
	return strings.Trim(p[i+len(marker):], "/")
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.st.Settings(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.settingsDTO(settings))
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	current, err := s.st.Settings(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	var body struct {
		ServerName        *string `json:"serverName"`
		Concurrency       *int    `json:"concurrency"`
		WebhookURL        *string `json:"webhookUrl"`
		NotifyOn          *string `json:"notifyOn"`
		UploadConcurrency *int    `json:"uploadConcurrency"`
		Compression       *string `json:"compression"`
		UploadLimitMbps   *int    `json:"uploadLimitMbps"`
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
	if body.UploadConcurrency != nil {
		if *body.UploadConcurrency < store.MinUploadConcurrency || *body.UploadConcurrency > store.MaxUploadConcurrency {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("uploadConcurrency must be between %d and %d",
				store.MinUploadConcurrency, store.MaxUploadConcurrency))
			return
		}
		current.UploadConcurrency = *body.UploadConcurrency
	}
	if body.Compression != nil {
		if !store.ValidCompression(*body.Compression) {
			writeError(w, http.StatusBadRequest, `compression must be "zstd" or "off"`)
			return
		}
		current.Compression = *body.Compression
	}
	if body.UploadLimitMbps != nil {
		if *body.UploadLimitMbps < store.MinUploadLimitMbps || *body.UploadLimitMbps > store.MaxUploadLimitMbps {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("uploadLimitMbps must be between %d and %d (0 = unlimited)",
				store.MinUploadLimitMbps, store.MaxUploadLimitMbps))
			return
		}
		current.UploadLimitMbps = *body.UploadLimitMbps
	}
	// What changed is recorded as a list of field names, never as values: a
	// webhook URL routinely carries a token in its path, and an audit trail must
	// not become a place secrets are kept.
	var changed []string
	for _, f := range []struct {
		name    string
		changed bool
	}{
		{"serverName", body.ServerName != nil},
		{"concurrency", body.Concurrency != nil},
		{"webhookUrl", body.WebhookURL != nil},
		{"notifyOn", body.NotifyOn != nil},
		{"uploadConcurrency", body.UploadConcurrency != nil},
		{"compression", body.Compression != nil},
		{"uploadLimitMbps", body.UploadLimitMbps != nil},
	} {
		if f.changed {
			changed = append(changed, f.name)
		}
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
	if len(changed) > 0 {
		s.audit(r, store.AuditEntry{
			Action: store.AuditSettingsModify, ObjectKind: "settings",
			Detail: "changed " + strings.Join(changed, ", "),
		})
	}
	writeJSON(w, http.StatusOK, s.settingsDTO(saved))
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

// allowedDownloads is the set of names /downloads serves. It is derived from the
// one list of staged artifacts in internal/update so this endpoint, the
// post-update refresh and GET /api/downloads/status can never disagree about
// which files exist — and so the version sidecars beside them are not servable.
//
// The set is the whole of the name validation: nothing outside it can be
// requested, so no path can escape the downloads directory.
var allowedDownloads = func() map[string]struct{} {
	arts := update.StagedArtifacts()
	out := make(map[string]struct{}, len(arts))
	for _, a := range arts {
		out[a.Name] = struct{}{}
	}
	return out
}()

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if _, ok := allowedDownloads[name]; !ok {
		writeError(w, http.StatusNotFound, "unknown download")
		return
	}
	full := update.StagedPath(s.dataDir, name)
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
