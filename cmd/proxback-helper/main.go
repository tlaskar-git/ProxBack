// Command proxback-helper is the ProxBack node helper: a root daemon on a
// Proxmox VE node that wraps the node's own vzdump/qmrestore so the ProxBack
// server can back up and restore whole guests without an in-guest agent and
// without any Proxmox API extension.
//
// First run on a node (the one-liner the web UI shows):
//
//	proxback-helper --server https://proxback:8443 --token <enrollment-token> --install
//
// Afterwards systemd runs it with the stored configuration:
//
//	proxback-helper --config /etc/proxback-helper
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"proxback/internal/helper"
	"proxback/internal/version"
)

func main() {
	server := flag.String("server", "", "ProxBack server base URL, e.g. https://proxback:8443")
	token := flag.String("token", "", "one-time enrollment token (first run only)")
	configDir := flag.String("config", helper.DefaultConfigDir, "directory holding the helper configuration and keys")
	// 0 means "keep whatever was stored at enrollment", so a helper enrolled on a
	// non-default port keeps it across the restarts systemd performs without the
	// flag. First enrollment falls back to helper.DefaultPort.
	port := flag.Int("port", 0, "port to serve the export/import API on (default 8007)")
	install := flag.Bool("install", false, "enroll, then install and start the systemd service")
	insecure := flag.Bool("insecure", false, "skip TLS certificate verification when talking to the server")
	showVersion := flag.Bool("version", false, "print the helper version and exit")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn or error")
	flag.Parse()

	if *showVersion {
		fmt.Printf("proxback-helper %s (%s/%s)\n", version.Version, runtime.GOOS, runtime.GOARCH)
		return
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "invalid --log-level %q\n", *logLevel)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	h, err := helper.New(helper.Config{
		ServerURL:   *server,
		Token:       *token,
		ConfigDir:   *configDir,
		Port:        *port,
		InsecureTLS: *insecure,
		Logger:      log,
	})
	if err != nil {
		log.Error("helper could not start", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *install {
		// Enrollment first: an installed service with no credentials is useless.
		if err := h.Enroll(ctx); err != nil {
			log.Error("enrollment failed", "error", err)
			os.Exit(1)
		}
		fmt.Printf("enrolled as helper %s for node %s (port %d)\n", h.HelperID(), h.Node(), h.Port())
		if err := helper.Install(os.Stdout, *configDir); err != nil {
			log.Error("install failed", "error", err)
			os.Exit(1)
		}
		return
	}

	if err := h.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("helper exited with error", "error", err)
		os.Exit(1)
	}
	log.Info("node helper stopped")
}
