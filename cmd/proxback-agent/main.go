// Command proxback-agent is the ProxBack in-guest agent for file-level backups.
//
// First run:
//
//	proxback-agent --server https://proxback:8443 --token <enrollment-token>
//
// Subsequent runs read the stored agent id and API key from the config directory:
//
//	proxback-agent --config /etc/proxback
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
	"time"

	"proxback/internal/agent"
)

func main() {
	server := flag.String("server", "", "ProxBack server base URL, e.g. https://proxback:8443")
	token := flag.String("token", "", "one-time enrollment token (first run only)")
	configDir := flag.String("config", agent.DefaultConfigDir(), "directory holding the agent configuration and API key")
	install := flag.Bool("install", false, "print service installation instructions for this OS and exit")
	once := flag.Bool("once", false, "enroll, run a single heartbeat/poll cycle and exit")
	interval := flag.Duration("interval", agent.DefaultHeartbeatInterval, "heartbeat interval")
	insecure := flag.Bool("insecure", false, "skip TLS certificate verification when talking to the server")
	showVersion := flag.Bool("version", false, "print the agent version and exit")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn or error")
	flag.Parse()

	if *showVersion {
		fmt.Printf("proxback-agent %s (%s/%s)\n", agent.Version, runtime.GOOS, runtime.GOARCH)
		return
	}
	if *install {
		fmt.Print(agent.InstallInstructions(*server, *token, *configDir))
		return
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "invalid --log-level %q\n", *logLevel)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	a, err := agent.New(agent.Config{
		ServerURL:         *server,
		Token:             *token,
		ConfigDir:         *configDir,
		HeartbeatInterval: *interval,
		InsecureTLS:       *insecure,
		Logger:            log,
	})
	if err != nil {
		log.Error("agent could not start", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *once {
		runCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		if err := a.RunOnce(runCtx); err != nil {
			log.Error("agent run failed", "error", err)
			os.Exit(1)
		}
		return
	}

	if err := a.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("agent exited with error", "error", err)
		os.Exit(1)
	}
	log.Info("agent stopped")
}
