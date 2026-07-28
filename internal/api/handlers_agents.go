package api

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"proxback/internal/agentmgr"
	"proxback/internal/engine"
	"proxback/internal/store"
	"proxback/internal/version"
)

type agentDTO struct {
	ID           string     `json:"id"`
	Hostname     string     `json:"hostname"`
	OS           string     `json:"os"`
	Arch         string     `json:"arch"`
	Version      string     `json:"version"`
	Status       string     `json:"status"`
	LastSeen     *time.Time `json:"lastSeen"`
	RegisteredAt time.Time  `json:"registeredAt"`
	// ServerVersion, UpToDate and UpdateAvailable are the drift the console
	// exists to show. They ride on each row rather than in a list envelope
	// because this endpoint has always returned a bare array, and a row that
	// answers "is this one behind?" by itself is also the row a detail page can
	// render without fetching the list.
	ServerVersion   string `json:"serverVersion"`
	UpToDate        bool   `json:"upToDate"`
	UpdateAvailable bool   `json:"updateAvailable"`
}

func toAgentDTO(a *store.Agent) agentDTO {
	upToDate, updateAvailable := versionDrift(a.Version)
	return agentDTO{
		ID: a.ID, Hostname: a.Hostname, OS: a.OS, Arch: a.Arch, Version: a.Version,
		Status: agentmgr.Status(a), LastSeen: a.LastSeen, RegisteredAt: a.RegisteredAt,
		ServerVersion: version.Version, UpToDate: upToDate, UpdateAvailable: updateAvailable,
	}
}

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := s.st.ListAgents(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	out := make([]agentDTO, 0, len(agents))
	for _, a := range agents {
		out = append(out, toAgentDTO(a))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateEnrollToken(w http.ResponseWriter, r *http.Request) {
	tok, err := s.agents.CreateEnrollToken(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	// Minting an enrollment token is what admits a new agent, so it is the agent
	// creation the trail can record. The token value itself is a credential and is
	// never written here.
	s.audit(r, store.AuditEntry{
		Action: store.AuditAgentCreate, ObjectKind: "agent",
		Detail: "issued an agent enrollment token, valid until " + tok.ExpiresAt.Format(time.RFC3339),
	})
	writeJSON(w, http.StatusOK, map[string]any{"token": tok.Token, "expiresAt": tok.ExpiresAt})
}

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	name := ""
	if agent, err := s.st.AgentByID(r.Context(), id); err == nil {
		name = agent.Hostname
	}
	if err := s.st.DeleteAgent(r.Context(), id); err != nil {
		s.notFoundOr(w, err, "agent")
		return
	}
	s.audit(r, store.AuditEntry{
		Action: store.AuditAgentDelete, ObjectKind: "agent", ObjectID: id, ObjectName: name,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------- agent facing

func (s *Server) handleAgentRegister(w http.ResponseWriter, r *http.Request) {
	var body agentmgr.RegisterRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	res, err := s.agents.Register(r.Context(), body)
	if err != nil {
		if errors.Is(err, agentmgr.ErrBadToken) {
			writeError(w, http.StatusUnauthorized, "invalid or expired enrollment token")
			return
		}
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleAgentHeartbeat(w http.ResponseWriter, r *http.Request) {
	agent := agentFrom(r.Context())
	// The body is optional and its contents advisory: an agent that predates
	// version reporting sends "{}", and a malformed one must still count as a
	// heartbeat. Liveness is never denied over an optional field.
	beat := decodeOptionalJSON[agentmgr.HeartbeatRequest](r)
	jobs, err := s.agents.Heartbeat(r.Context(), agent.ID, beat)
	if err != nil {
		s.serverError(w, err)
		return
	}
	// serverVersion travels back on every beat so an agent can see, without
	// being told, whether it is the build its server hands out.
	writeJSON(w, http.StatusOK, map[string]any{
		"jobs": jobs, "serverVersion": version.Version,
	})
}

func (s *Server) handleAgentChunk(w http.ResponseWriter, r *http.Request) {
	agent := agentFrom(r.Context())
	runID := chi.URLParam(r, "runId")
	sha := r.Header.Get("X-Chunk-Sha256")
	if sha == "" {
		writeError(w, http.StatusBadRequest, "X-Chunk-Sha256 header is required")
		return
	}
	var totalBytes int64
	if v := r.Header.Get("X-Total-Bytes"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			totalBytes = n
		}
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, engine.ChunkSize+1024))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read chunk body: "+err.Error())
		return
	}
	if len(data) == 0 {
		writeError(w, http.StatusBadRequest, "empty chunk body")
		return
	}
	deduped, err := s.agents.AcceptChunk(r.Context(), runID, agent.ID, sha, data, totalBytes)
	if err != nil {
		s.agentRunError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sha256": sha, "size": len(data), "deduped": deduped,
	})
}

func (s *Server) handleAgentComplete(w http.ResponseWriter, r *http.Request) {
	agent := agentFrom(r.Context())
	var body agentmgr.CompleteRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := s.agents.Complete(r.Context(), chi.URLParam(r, "runId"), agent.ID, body); err != nil {
		s.agentRunError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAgentFail(w http.ResponseWriter, r *http.Request) {
	agent := agentFrom(r.Context())
	var body struct {
		Error string `json:"error"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := s.agents.Fail(r.Context(), chi.URLParam(r, "runId"), agent.ID, body.Error); err != nil {
		s.agentRunError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAgentRestoreStream(w http.ResponseWriter, r *http.Request) {
	agent := agentFrom(r.Context())
	runID := chi.URLParam(r, "runId")
	w.Header().Set("Content-Type", "application/x-tar")
	if err := s.agents.RestoreStream(r.Context(), runID, agent.ID, w); err != nil {
		// Headers may already be on the wire; log and drop the connection so the
		// agent treats the transfer as failed.
		s.log.Error("restore stream failed", "run", runID, "error", err)
		panic(http.ErrAbortHandler)
	}
}

func (s *Server) agentRunError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, agentmgr.ErrUnknownRun):
		writeError(w, http.StatusNotFound, "unknown or finished run")
	case errors.Is(err, agentmgr.ErrUnauthorized):
		writeError(w, http.StatusForbidden, "run does not belong to this agent")
	case errors.Is(err, engine.ErrHashMismatch):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.serverError(w, err)
	}
}
