// Command s3-sim serves an S3-compatible object store for ProxBack development
// and tests. Signatures are accepted without validation; use path-style
// addressing and any access key pair.
package main

import (
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"proxback/internal/s3sim"
)

func main() {
	listen := flag.String("listen", ":9000", "address to listen on")
	dir := flag.String("dir", "", "directory for a persistent bolt backend (empty means in-memory)")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn or error")
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		slog.Error("invalid log level", "value", *logLevel)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	if *dir != "" {
		if err := os.MkdirAll(*dir, 0o755); err != nil {
			log.Error("could not create backend dir", "dir", *dir, "error", err)
			os.Exit(1)
		}
	}
	sim, err := s3sim.New(*dir)
	if err != nil {
		log.Error("s3-sim could not start", "error", err)
		os.Exit(1)
	}
	defer func() {
		if cerr := sim.Close(); cerr != nil {
			log.Error("s3-sim shutdown error", "error", cerr)
		}
	}()

	backend := "memory"
	if *dir != "" {
		backend = "bolt:" + *dir
	}
	srv := &http.Server{
		Addr:              *listen,
		Handler:           sim.Handler,
		ReadHeaderTimeout: 30 * time.Second,
	}
	log.Info("s3-sim listening", "addr", *listen, "backend", backend)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("s3-sim failed", "error", err)
		os.Exit(1)
	}
}
