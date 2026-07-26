// Command pve-sim serves a Proxmox VE API simulator for ProxBack development and
// tests: 2 nodes, 4 guests with deterministic disk content, snapshots, the
// ProxBack disk export/import extension endpoints and sim-only helpers.
package main

import (
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"proxback/internal/pvesim"
)

func main() {
	listen := flag.String("listen", ":8006", "address to listen on")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn or error")
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		slog.Error("invalid log level", "value", *logLevel)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	sim := pvesim.New(log)
	srv := &http.Server{
		Addr:              *listen,
		Handler:           sim.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
	}
	log.Info("pve-sim listening", "addr", *listen, "topology", sim.Describe())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("pve-sim failed", "error", err)
		os.Exit(1)
	}
}
