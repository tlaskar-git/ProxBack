// Package api implements ProxBack's HTTP surface: the REST API consumed by the
// React control panel, the agent-facing endpoints, agent binary downloads and
// serving of the embedded SPA.
package api

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"proxback/internal/agentmgr"
	"proxback/internal/auth"
	"proxback/internal/helpermgr"
	"proxback/internal/nodedeploy"
	"proxback/internal/notify"
	"proxback/internal/sched"
	"proxback/internal/store"
)

// webdist holds the built SPA. The placeholder index.html is replaced by the
// real `vite build` output at integration time.
//
//go:embed all:webdist
var webdist embed.FS

// Config wires the API layer to its dependencies.
type Config struct {
	Store   *store.Store
	Auth    *auth.Service
	Agents  *agentmgr.Manager
	Helpers *helpermgr.Manager
	Sched   *sched.Manager
	DataDir string
	Logger  *slog.Logger
	// OnRestartRequested is invoked after a software update is applied so the
	// process can shut down gracefully (systemd Restart=always brings the new
	// binary up). Nil disables the automatic restart.
	OnRestartRequested func()
}

// Server is the ProxBack HTTP handler.
type Server struct {
	st       *store.Store
	auth     *auth.Service
	agents   *agentmgr.Manager
	helpers  *helpermgr.Manager
	sched    *sched.Manager
	notifier *notify.Notifier
	dataDir  string
	log      *slog.Logger
	restart  func()

	// deployHelper installs a node helper over SSH. It is a field so tests can
	// exercise the endpoint without a real SSH server.
	deployHelper func(context.Context, nodedeploy.Params) (nodedeploy.Result, error)

	spa    fs.FS
	router chi.Router
}

// New builds the HTTP handler.
func New(cfg Config) (*Server, error) {
	if cfg.Store == nil || cfg.Auth == nil || cfg.Agents == nil || cfg.Helpers == nil || cfg.Sched == nil {
		return nil, errors.New("api: store, auth, agents, helpers and sched are required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	spa, err := fs.Sub(webdist, "webdist")
	if err != nil {
		return nil, fmt.Errorf("api: embedded web assets: %w", err)
	}
	s := &Server{
		st:       cfg.Store,
		auth:     cfg.Auth,
		agents:   cfg.Agents,
		helpers:  cfg.Helpers,
		sched:    cfg.Sched,
		notifier: notify.New(log),
		dataDir:  cfg.DataDir,
		log:      log,
		restart:  cfg.OnRestartRequested,
		spa:      spa,

		deployHelper: nodedeploy.Deploy,
	}
	s.router = s.routes()
	return s, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.router.ServeHTTP(w, r) }

func (s *Server) routes() chi.Router {
	root := chi.NewRouter()
	root.Use(middleware.Recoverer)
	root.Use(s.requestLogger)

	root.Mount("/api", s.apiRoutes())
	root.Get("/downloads/{name}", s.handleDownload)
	root.NotFound(s.serveSPA)
	root.MethodNotAllowed(s.serveSPA)
	return root
}

func (s *Server) apiRoutes() chi.Router {
	r := chi.NewRouter()
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	})

	// Unauthenticated: first-run setup, login and agent enrollment.
	r.Get("/setup/status", s.handleSetupStatus)
	r.Post("/setup", s.handleSetup)
	r.Post("/login", s.handleLogin)
	// Agent and node helper registration authenticate with the single-use
	// enrollment token in the body; no permanent key exists yet at this point.
	r.Post("/agents/register", s.handleAgentRegister)
	r.Post("/helpers/register", s.handleHelperRegister)

	// Agent API key authenticated routes.
	r.Group(func(r chi.Router) {
		r.Use(s.requireAgent)
		r.Post("/agents/heartbeat", s.handleAgentHeartbeat)
		r.Post("/agents/runs/{runId}/chunks", s.handleAgentChunk)
		r.Post("/agents/runs/{runId}/complete", s.handleAgentComplete)
		r.Post("/agents/runs/{runId}/fail", s.handleAgentFail)
		r.Get("/agents/restores/{runId}/stream", s.handleAgentRestoreStream)
	})

	// Node helper API key authenticated routes.
	r.Group(func(r chi.Router) {
		r.Use(s.requireHelper)
		r.Post("/helpers/heartbeat", s.handleHelperHeartbeat)
	})

	// Session cookie authenticated routes.
	r.Group(func(r chi.Router) {
		r.Use(s.requireSession)

		r.Post("/logout", s.handleLogout)
		r.Get("/me", s.handleMe)
		r.Post("/me/password", s.handleChangePassword)

		r.Get("/update/status", s.handleUpdateStatus)
		r.Post("/update/apply", s.handleUpdateApply)

		r.Get("/dashboard", s.handleDashboard)
		r.Get("/posture", s.handlePosture)

		r.Get("/hosts", s.handleListHosts)
		r.Post("/hosts", s.handleCreateHost)
		r.Post("/hosts/{id}/test", s.handleTestHost)
		r.Delete("/hosts/{id}", s.handleDeleteHost)
		r.Get("/hosts/{id}/vms", s.handleHostVMs)
		r.Get("/hosts/{id}/free-vmid", s.handleFreeVMID)
		r.Get("/vms", s.handleListVMs)

		r.Get("/targets", s.handleListTargets)
		r.Post("/targets", s.handleCreateTarget)
		r.Post("/targets/{id}/test", s.handleTestTarget)
		r.Delete("/targets/{id}", s.handleDeleteTarget)

		r.Get("/jobs", s.handleListJobs)
		r.Post("/jobs", s.handleCreateJob)
		r.Patch("/jobs/{id}", s.handlePatchJob)
		r.Delete("/jobs/{id}", s.handleDeleteJob)
		r.Post("/jobs/{id}/run", s.handleRunJob)
		r.Get("/jobs/{id}/retention-preview", s.handleRetentionPreview)

		r.Get("/runs", s.handleListRuns)
		r.Post("/runs/clear", s.handleClearRuns)
		r.Get("/runs/{id}", s.handleGetRun)
		r.Get("/runs/{id}/log", s.handleRunLog)
		r.Delete("/runs/{id}", s.handleDeleteRun)
		r.Post("/runs/{id}/cancel", s.handleCancelRun)
		r.Post("/runs/{id}/retry", s.handleRetryRun)

		r.Get("/backups", s.handleListBackups)
		r.Post("/backups/{id}/verify", s.handleVerifyBackup)
		r.Delete("/backups/{id}", s.handleDeleteBackup)
		r.Post("/restores", s.handleCreateRestore)

		r.Get("/agents", s.handleListAgents)
		r.Post("/agents/enroll-token", s.handleCreateEnrollToken)
		r.Delete("/agents/{id}", s.handleDeleteAgent)

		r.Get("/helpers", s.handleListHelpers)
		r.Post("/helpers/enroll-token", s.handleCreateHelperEnrollToken)
		r.Post("/helpers/deploy", s.handleDeployHelper)
		r.Post("/helpers/{id}/assign", s.handleAssignHelper)
		r.Delete("/helpers/{id}", s.handleDeleteHelper)

		r.Get("/settings", s.handleGetSettings)
		r.Put("/settings", s.handlePutSettings)
		r.Post("/settings/test-webhook", s.handleTestWebhook)
	})
	return r
}

// ---------------------------------------------------------------- middleware

func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		if strings.HasPrefix(r.URL.Path, "/api") {
			s.log.Debug("http",
				"method", r.Method, "path", r.URL.Path,
				"status", ww.Status(), "bytes", ww.BytesWritten(),
				"duration", time.Since(start).String())
		}
	})
}

type ctxKey string

const (
	ctxUser   ctxKey = "user"
	ctxAgent  ctxKey = "agent"
	ctxHelper ctxKey = "helper"
)

func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := auth.SessionTokenFromRequest(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		user, err := s.auth.UserForSession(r.Context(), token)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusUnauthorized, "authentication required")
				return
			}
			s.serverError(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxUser, user)))
	})
}

func userFrom(ctx context.Context) *store.User {
	u, _ := ctx.Value(ctxUser).(*store.User)
	return u
}

func (s *Server) requireAgent(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := auth.BearerToken(r)
		agent, err := s.agents.Authenticate(r.Context(), key)
		if err != nil {
			if errors.Is(err, agentmgr.ErrUnauthorized) {
				writeError(w, http.StatusUnauthorized, "invalid agent key")
				return
			}
			s.serverError(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxAgent, agent)))
	})
}

func agentFrom(ctx context.Context) *store.Agent {
	a, _ := ctx.Value(ctxAgent).(*store.Agent)
	return a
}

func (s *Server) requireHelper(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := auth.BearerToken(r)
		h, err := s.helpers.Authenticate(r.Context(), key)
		if err != nil {
			if errors.Is(err, helpermgr.ErrUnauthorized) {
				writeError(w, http.StatusUnauthorized, "invalid node helper key")
				return
			}
			s.serverError(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxHelper, h)))
	})
}

func helperFrom(ctx context.Context) *store.NodeHelper {
	h, _ := ctx.Value(ctxHelper).(*store.NodeHelper)
	return h
}

// ---------------------------------------------------------------- helpers

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Default().Warn("could not encode response", "error", err)
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) serverError(w http.ResponseWriter, err error) {
	s.log.Error("request failed", "error", err)
	writeError(w, http.StatusInternalServerError, err.Error())
}

// notFoundOr maps store.ErrNotFound to 404 and anything else to 500.
func (s *Server) notFoundOr(w http.ResponseWriter, err error, what string) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, what+" not found")
		return
	}
	s.serverError(w, err)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}
