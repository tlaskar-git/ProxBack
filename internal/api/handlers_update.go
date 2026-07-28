package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"proxback/internal/store"
	"proxback/internal/update"
	"proxback/internal/version"
)

// stagedFailureMessages flattens the per-binary failures for the JSON response.
// Nil rather than an empty slice, so a clean refresh emits null and the console
// has one thing to test.
func stagedFailureMessages(res update.StagedRefresh) []string {
	var out []string
	for _, f := range res.Failed {
		out = append(out, f.Name+": "+f.Err.Error())
	}
	return out
}

type updateStatusDTO struct {
	CurrentVersion  string `json:"currentVersion"`
	LatestVersion   string `json:"latestVersion,omitempty"`
	UpdateAvailable bool   `json:"updateAvailable"`
	ReleaseNotes    string `json:"releaseNotes,omitempty"`
	ReleaseURL      string `json:"releaseUrl,omitempty"`
	PublishedAt     string `json:"publishedAt,omitempty"`
	AssetName       string `json:"assetName,omitempty"`
	AssetAvailable  bool   `json:"assetAvailable"`
	CheckError      string `json:"checkError,omitempty"`
}

// handleUpdateStatus performs a live check against the release repository and
// reports whether a newer server build is available for this platform.
func (s *Server) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	out := updateStatusDTO{CurrentVersion: version.Version}
	checker := update.New(s.log)
	rel, err := checker.Latest(r.Context())
	switch {
	case errors.Is(err, update.ErrNoReleases):
		// A repo without releases is a valid state, not an error.
	case err != nil:
		out.CheckError = err.Error()
	default:
		out.LatestVersion = rel.Version()
		out.ReleaseURL = rel.HTMLURL
		out.ReleaseNotes = rel.Body
		out.PublishedAt = rel.PublishedAt.UTC().Format(time.RFC3339)
		out.UpdateAvailable = update.Newer(version.Version, rel.Version())
		if asset, aerr := rel.AssetFor(update.CurrentPlatform()); aerr == nil {
			out.AssetName = asset.Name
			out.AssetAvailable = true
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleUpdateApply downloads the latest release binary, swaps it over the
// running executable and, when wired, schedules a graceful restart.
func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	// Installing an update restarts the server, which would cancel anything in
	// flight and throw away its uploaded work. Refuse unless the operator
	// explicitly overrides with ?force=1.
	if r.URL.Query().Get("force") != "1" {
		running, err := s.st.CountRunningRuns(r.Context())
		if err != nil {
			s.serverError(w, err)
			return
		}
		if running > 0 {
			noun := "run is"
			if running > 1 {
				noun = "runs are"
			}
			writeError(w, http.StatusConflict, fmt.Sprintf(
				"%d %s still in progress — installing an update restarts the server and cancels them. "+
					"Wait for them to finish, or cancel them first.", running, noun))
			return
		}
	}
	checker := update.New(s.log)
	rel, err := checker.Latest(r.Context())
	if err != nil {
		if errors.Is(err, update.ErrNoReleases) {
			writeError(w, http.StatusConflict, "no releases have been published yet")
			return
		}
		s.serverError(w, err)
		return
	}
	if !update.Newer(version.Version, rel.Version()) {
		writeError(w, http.StatusConflict, "already running the latest version")
		return
	}
	asset, err := rel.AssetFor(update.CurrentPlatform())
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	binPath, err := os.Executable()
	if err != nil {
		s.serverError(w, err)
		return
	}
	if err := checker.Apply(r.Context(), rel, asset, binPath); err != nil {
		s.serverError(w, err)
		return
	}
	// Refresh what this server hands out, from the same release, before the
	// restart below. Without this the server upgrades and the staged agent does
	// not: a 0.6.0 server keeps installing the agent its installer left behind,
	// which is how every Windows install came to fail with SCM error 1053 while
	// the console reported a healthy, up-to-date server. It is best effort — a
	// staged binary that cannot be refreshed is logged and reported by
	// GET /api/downloads/status, and never undoes a successful server upgrade.
	staged := s.refreshStagedBinaries(r, rel, checker)
	s.audit(r, store.AuditEntry{
		Action: store.AuditUpdateApply, ObjectKind: "server", ObjectName: "proxback-server",
		Detail: "updated from " + version.Version + " to " + rel.Version(),
	})
	restarting := s.restart != nil
	if restarting {
		s.log.Info("update installed; restarting", "version", rel.Version())
		go func() {
			// Give the response time to flush before shutting down.
			time.Sleep(500 * time.Millisecond)
			s.restart()
		}()
	} else {
		s.log.Info("update installed; restart the service to run it", "version", rel.Version())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"version":    rel.Version(),
		"restarting": restarting,
		// What the console will hand out from here on, so the operator learns
		// about a staged binary that could not be refreshed at the moment they
		// update rather than the next time an install mysteriously fails.
		"stagedBinaries": map[string]any{
			"refreshed": staged.Updated,
			"skipped":   staged.Skipped,
			"failed":    stagedFailureMessages(staged),
		},
	})
}
