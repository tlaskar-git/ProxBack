package api

// The staged agent and node helper binaries the console hands out, and their
// visibility. See internal/update/staged.go for why they exist and how they are
// refreshed; this file is the part an operator can see.

import (
	"context"
	"net/http"
	"time"

	"proxback/internal/store"
	"proxback/internal/update"
	"proxback/internal/version"
)

// stagedRefreshTimeout caps the startup reconciliation. It is generous because
// it covers three binary downloads over whatever link the installation has, and
// it blocks nothing: the pass runs in the background.
const stagedRefreshTimeout = 10 * time.Minute

type stagedArtifactDTO struct {
	Name string `json:"name"`
	// Kind is "agent" or "node helper", so the console can show each deployment
	// page only the artifacts it is about.
	Kind    string `json:"kind"`
	OS      string `json:"os"`
	Present bool   `json:"present"`
	// Version is the release recorded when the binary was staged, empty when it
	// is not known. Reason then says why.
	Version       string     `json:"version"`
	MatchesServer bool       `json:"matchesServer"`
	SizeBytes     int64      `json:"sizeBytes"`
	ModifiedAt    *time.Time `json:"modifiedAt"`
	// Reason is set whenever MatchesServer is false: either the binary is
	// missing, or its version is unknown, or it is a different build from this
	// server. It is written for an operator about to deploy, not for a log.
	Reason string `json:"reason,omitempty"`
}

type downloadsStatusDTO struct {
	ServerVersion string              `json:"serverVersion"`
	Artifacts     []stagedArtifactDTO `json:"artifacts"`
	// AllMatch is the single question the deployment pages ask: is everything
	// this server hands out the same build as this server?
	AllMatch bool `json:"allMatch"`
}

// handleDownloadsStatus reports the staged agent and node helper binaries
// against the running server's version.
//
// It exists because the failure it describes used to be invisible: a server that
// self-updated kept serving the agent its installer staged, the console showed a
// healthy up-to-date server, and the only symptom was every Windows install
// failing with SCM error 1053. An operator about to press "Deploy Agent" can now
// see that they are about to hand out the wrong build.
func (s *Server) handleDownloadsStatus(w http.ResponseWriter, r *http.Request) {
	out := downloadsStatusDTO{
		ServerVersion: version.Version,
		Artifacts:     []stagedArtifactDTO{},
		AllMatch:      true,
	}
	for _, st := range update.InspectStaged(s.dataDir) {
		dto := stagedArtifactDTO{
			Name: st.Name, Kind: st.Kind, OS: st.GOOS,
			Present: st.Present, Version: st.Version,
			SizeBytes: st.Size, Reason: st.Reason,
		}
		if st.Present {
			mod := st.ModTime
			dto.ModifiedAt = &mod
		}
		dto.MatchesServer = st.Present && st.Version == version.Version
		if !dto.MatchesServer {
			out.AllMatch = false
			if dto.Reason == "" {
				dto.Reason = "this staged " + st.Kind + " is version " + st.Version +
					" but the server is " + version.Version +
					" — deploying it would hand out the wrong build"
			}
		}
		out.Artifacts = append(out.Artifacts, dto)
	}
	writeJSON(w, http.StatusOK, out)
}

// refreshStagedBinaries brings <dataDir>/downloads in line with the release just
// installed. It is called after an update is applied, before the restart, and
// its failure never fails the update: the server upgrade is what matters.
func (s *Server) refreshStagedBinaries(r *http.Request, rel *update.Release, checker *update.Checker) update.StagedRefresh {
	res, err := checker.RefreshStaged(r.Context(), rel, s.dataDir)
	if err != nil {
		s.log.Error("could not refresh the staged agent and node helper binaries after the update; "+
			"they still hold the previous build — check /api/downloads/status before deploying them",
			"version", rel.Version(), "error", err)
		s.audit(r, store.AuditEntry{
			Action: store.AuditDownloadsRefresh, Result: store.AuditError,
			ObjectKind: "downloads", ObjectName: update.StagedDirName,
			Detail: "refresh to " + rel.Version() + " failed: " + err.Error(),
		})
		return res
	}
	result := store.AuditOK
	if len(res.Failed) > 0 {
		result = store.AuditError
	}
	s.audit(r, store.AuditEntry{
		Action: store.AuditDownloadsRefresh, Result: result,
		ObjectKind: "downloads", ObjectName: update.StagedDirName,
		Detail: "staged binaries for " + rel.Version() + ": " + res.Summary(),
	})
	return res
}

// reconcileStagedBinaries heals an installation whose staged binaries drifted
// from the server serving them.
//
// It runs at startup rather than only on update because the servers that already
// drifted will never apply another update in time to fix themselves, and their
// operators should not have to fix it by hand. It is best effort throughout: it
// blocks nothing, it is never fatal, and an installation without internet access
// simply keeps the binaries it has and gets a log line saying so.
func (s *Server) reconcileStagedBinaries() {
	ctx, cancel := context.WithTimeout(context.Background(), stagedRefreshTimeout)
	defer cancel()

	checker := update.New(s.log)
	res, err := checker.ReconcileStaged(ctx, s.dataDir, version.Version)
	if err != nil {
		s.log.Warn("could not refresh the staged agent and node helper binaries; "+
			"the existing ones are left in place — see /api/downloads/status before deploying them. "+
			"This is expected on an installation without access to the release repository.",
			"serverVersion", version.Version, "error", err)
		return
	}
	if !res.Changed() && len(res.Failed) == 0 && len(res.Skipped) == 0 {
		s.log.Debug("staged agent and node helper binaries match this server",
			"serverVersion", version.Version)
		return
	}
	s.log.Info("reconciled the staged agent and node helper binaries",
		"serverVersion", version.Version, "result", res.Summary())
}
