package api

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"proxback/internal/store"
)

// ---------------------------------------------------------------- recording

// audit records one event in the append-only trail.
//
// It is deliberately best effort: a failed write is logged and never returned,
// because an audit problem must not fail the operation it describes. Call it
// with the object the action was about; the actor, the source address and the
// timestamp are filled in from the request.
//
// Never pass a secret. Detail is a short factual note — field names, a mode, a
// destination — and passwords, tokens, keys and secret values must not reach it.
func (s *Server) audit(r *http.Request, e store.AuditEntry) {
	if e.Actor == "" && e.ActorID == 0 {
		if u := userFrom(r.Context()); u != nil {
			e.Actor, e.ActorID = u.Username, u.ID
		}
	}
	if e.SourceIP == "" {
		e.SourceIP = clientIP(r)
	}
	// The request's context is cancelled the moment the response is finished, and
	// recording what just happened is not the client's business, so the write
	// gets its own short-lived one.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if _, err := s.st.AppendAudit(ctx, e); err != nil {
		s.log.Error("could not record audit event", "action", e.Action, "error", err)
	}
}

// clientIP is the address the request came from, without its port. It is taken
// from the connection rather than from a header: a forwarded-for header is
// client-supplied and an audit trail that can be forged is worthless.
func clientIP(r *http.Request) string {
	addr := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// ---------------------------------------------------------------- reading

// handleListAudit serves the audit trail newest first. It is admin only, which
// the route group it is registered in enforces.
func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := store.AuditFilter{
		Action: strings.TrimSpace(q.Get("action")),
		Actor:  strings.TrimSpace(q.Get("actor")),
	}
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive whole number")
			return
		}
		filter.Limit = n
	}
	entries, err := s.st.AuditEntries(r.Context(), filter)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if entries == nil {
		entries = []*store.AuditEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}
