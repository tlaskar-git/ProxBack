// Package app wires the ProxBack server components together so both the
// cmd/proxback-server binary and the E2E test can build a complete server.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"proxback/internal/agentmgr"
	"proxback/internal/api"
	"proxback/internal/auth"
	"proxback/internal/sched"
	"proxback/internal/store"
)

// Options configures a server instance.
type Options struct {
	DataDir string
	Logger  *slog.Logger
	// OnRestartRequested is called after a software update is installed so the
	// process can exit gracefully and be restarted by its supervisor.
	OnRestartRequested func()
}

// App is a fully wired ProxBack server.
type App struct {
	Store   *store.Store
	Auth    *auth.Service
	Agents  *agentmgr.Manager
	Sched   *sched.Manager
	Handler http.Handler
	DataDir string

	log *slog.Logger
}

// New opens the data directory and wires every component. The scheduler is
// started, so Close must be called to shut it down.
func New(ctx context.Context, opts Options) (*App, error) {
	if opts.DataDir == "" {
		return nil, fmt.Errorf("app: data dir required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	dataDir, err := filepath.Abs(opts.DataDir)
	if err != nil {
		return nil, fmt.Errorf("app: resolve data dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "downloads"), 0o755); err != nil {
		return nil, fmt.Errorf("app: create downloads dir: %w", err)
	}
	st, err := store.Open(ctx, dataDir)
	if err != nil {
		return nil, err
	}
	if err := st.PurgeExpiredSessions(ctx); err != nil {
		log.Warn("could not purge expired sessions", "error", err)
	}
	authSvc := auth.New(st)
	if created, err := authSvc.SeedDefaultAdmin(ctx); err != nil {
		st.Close()
		return nil, fmt.Errorf("app: seed default admin: %w", err)
	} else if created {
		log.Warn("created default administrator account — sign in and change the password immediately",
			"username", auth.DefaultAdminUsername, "password", auth.DefaultAdminPassword)
	}
	agents := agentmgr.New(st, log)
	scheduler := sched.New(st, agents, log)
	if err := scheduler.Start(ctx); err != nil {
		st.Close()
		return nil, err
	}
	handler, err := api.New(api.Config{
		Store:              st,
		Auth:               authSvc,
		Agents:             agents,
		Sched:              scheduler,
		DataDir:            dataDir,
		Logger:             log,
		OnRestartRequested: opts.OnRestartRequested,
	})
	if err != nil {
		scheduler.Stop()
		st.Close()
		return nil, err
	}
	return &App{
		Store: st, Auth: authSvc, Agents: agents, Sched: scheduler,
		Handler: handler, DataDir: dataDir, log: log,
	}, nil
}

// Close stops the scheduler and closes the database.
func (a *App) Close() error {
	a.Sched.Stop()
	return a.Store.Close()
}
