// Command proxback-server is the ProxBack control-plane server: REST API,
// embedded React SPA, backup scheduler and agent endpoints in one binary.
//
// The server speaks plain HTTP. TLS termination is intentionally out of scope:
// run it behind nginx, Caddy, Traefik or the Proxmox host's reverse proxy and
// terminate TLS there. See deploy/README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"proxback/internal/app"
)

func main() {
	listen := flag.String("listen", ":8443", "address to listen on (plain HTTP; terminate TLS in front of this)")
	dataDir := flag.String("data", "./data", "data directory for the SQLite database, encryption key and agent downloads")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn or error")
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "invalid --log-level %q\n", *logLevel)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	if err := run(*listen, *dataDir, log); err != nil {
		log.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

func run(listen, dataDir string, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	instance, err := app.New(ctx, app.Options{DataDir: dataDir, Logger: log})
	if err != nil {
		return err
	}
	defer func() {
		if cerr := instance.Close(); cerr != nil {
			log.Error("shutdown error", "error", cerr)
		}
	}()

	srv := &http.Server{
		Addr:              listen,
		Handler:           instance.Handler,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("proxback server listening", "addr", listen, "data", instance.DataDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		log.Info("shutdown requested")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	return <-errc
}
