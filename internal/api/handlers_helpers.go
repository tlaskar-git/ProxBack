package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"proxback/internal/helpermgr"
	"proxback/internal/store"
)

type helperDTO struct {
	ID           string     `json:"id"`
	Node         string     `json:"node"`
	Address      string     `json:"address"`
	Port         int        `json:"port"`
	Version      string     `json:"version"`
	Status       string     `json:"status"`
	LastSeen     *time.Time `json:"lastSeen"`
	RegisteredAt time.Time  `json:"registeredAt"`
}

func toHelperDTO(h *store.NodeHelper) helperDTO {
	return helperDTO{
		ID: h.ID, Node: h.Node, Address: h.Address, Port: h.Port, Version: h.Version,
		Status: helpermgr.Status(h), LastSeen: h.LastSeen, RegisteredAt: h.RegisteredAt,
	}
}

func (s *Server) handleListHelpers(w http.ResponseWriter, r *http.Request) {
	helpers, err := s.st.ListHelpers(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	out := make([]helperDTO, 0, len(helpers))
	for _, h := range helpers {
		out = append(out, toHelperDTO(h))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateHelperEnrollToken(w http.ResponseWriter, r *http.Request) {
	tok, err := s.helpers.CreateEnrollToken(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": tok.Token, "expiresAt": tok.ExpiresAt})
}

func (s *Server) handleDeleteHelper(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteHelper(r.Context(), chi.URLParam(r, "id")); err != nil {
		s.notFoundOr(w, err, "node helper")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------- helper facing

func (s *Server) handleHelperRegister(w http.ResponseWriter, r *http.Request) {
	var body helpermgr.RegisterRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Node) == "" || body.AccessSecret == "" {
		writeError(w, http.StatusBadRequest, "node and accessSecret are required")
		return
	}
	res, err := s.helpers.Register(r.Context(), body, r.RemoteAddr)
	if err != nil {
		if errors.Is(err, helpermgr.ErrBadToken) {
			writeError(w, http.StatusUnauthorized, "invalid or expired enrollment token")
			return
		}
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleHelperHeartbeat(w http.ResponseWriter, r *http.Request) {
	h := helperFrom(r.Context())
	if err := s.helpers.Heartbeat(r.Context(), h.ID); err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
