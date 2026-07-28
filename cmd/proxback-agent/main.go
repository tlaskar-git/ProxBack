// Command proxback-agent is the ProxBack in-guest agent for file-level backups.
//
// It runs on any Windows or Linux guest — a Proxmox VM, a cloud instance or a
// bare metal box. Nothing about it assumes Proxmox: it talks only to the
// ProxBack server over HTTPS.
//
// Install as a service (elevated on Windows, root on Linux), which enrolls,
// registers and starts it:
//
//	proxback-agent --server https://proxback:8443 --token <enrollment-token> --install
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
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"proxback/internal/agent"
)

// Exit codes. 2 means "you asked for something impossible" and is reserved for
// usage errors, matching the flag package's own convention.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run is main with its I/O and arguments injected so flag handling and the
// early-exit paths can be tested without spawning a process.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("proxback-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("server", "", "ProxBack server base URL, e.g. https://proxback:8443")
	token := fs.String("token", "", "one-time enrollment token (first run only)")
	configDir := fs.String("config", agent.DefaultConfigDir(), "directory holding the agent configuration and API key")
	install := fs.Bool("install", false, "enroll, then install and start the agent as a system service")
	uninstall := fs.Bool("uninstall", false, "stop and remove the agent service")
	printInstall := fs.Bool("print-install", false, "print service installation instructions for this OS and exit")
	once := fs.Bool("once", false, "enroll, run a single heartbeat/poll cycle and exit")
	interval := fs.Duration("interval", agent.DefaultHeartbeatInterval, "heartbeat interval")
	insecure := fs.Bool("insecure", false, "skip TLS certificate verification when talking to the server")
	showVersion := fs.Bool("version", false, "print the agent version and exit")
	logLevel := fs.String("log-level", "info", "log level: debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	if *showVersion {
		fmt.Fprintf(stdout, "proxback-agent %s (%s/%s)\n", agent.Version, runtime.GOOS, runtime.GOARCH)
		return exitOK
	}
	if *printInstall {
		fmt.Fprint(stdout, agent.InstallInstructions(*server, *token, *configDir))
		return exitOK
	}
	if *install && *uninstall {
		fmt.Fprintln(stderr, "--install and --uninstall are mutually exclusive")
		return exitUsage
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintf(stderr, "invalid --log-level %q\n", *logLevel)
		return exitUsage
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	cfg := agent.Config{
		ServerURL:         *server,
		Token:             *token,
		ConfigDir:         *configDir,
		HeartbeatInterval: *interval,
		InsecureTLS:       *insecure,
		Logger:            log,
	}

	if *uninstall {
		if err := agent.Uninstall(stdout); err != nil {
			log.Error("uninstall failed", "error", err)
			return exitError
		}
		return exitOK
	}

	if *install {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		switch {
		case *server != "" || *token != "":
			// Enrollment first: an installed service with no credentials is
			// useless, and a bad token is far easier to diagnose here than
			// from a service that will not start.
			a, err := agent.New(cfg)
			if err != nil {
				log.Error("agent could not start", "error", err)
				return exitError
			}
			if err := a.Enroll(ctx); err != nil {
				log.Error("enrollment failed", "error", err)
				return exitError
			}
			fmt.Fprintf(stdout, "enrolled as agent %s\n", a.AgentID())
		case !agent.Enrolled(*configDir):
			fmt.Fprintf(stderr,
				"no stored agent credentials in %s: pass --server and --token so --install can enroll first\n",
				*configDir)
			return exitUsage
		}
		if err := agent.Install(stdout, agent.InstallOptions{
			ConfigDir:   *configDir,
			InsecureTLS: *insecure,
			LogLevel:    *logLevel,
			Interval:    *interval,
		}); err != nil {
			log.Error("install failed", "error", err)
			return exitError
		}
		return exitOK
	}

	// runAgent is the actual workload, shared by the interactive and the
	// Windows service paths. It takes its logger as an argument because under
	// the SCM the logger also writes to the Windows Event Log.
	runAgent := func(ctx context.Context, log *slog.Logger) error {
		runCfg := cfg
		runCfg.Logger = log
		a, err := agent.New(runCfg)
		if err != nil {
			return err
		}
		if *once {
			runCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
			defer cancel()
			return a.RunOnce(runCtx)
		}
		return a.Run(ctx)
	}

	// Started by the Windows Service Control Manager? Then the process must
	// perform the SCM handshake and report SERVICE_RUNNING, or the SCM kills it
	// after ~30s with error 1053.
	isService, err := agent.RunningAsService()
	if err != nil {
		log.Warn("could not determine whether this is a service start; assuming interactive", "error", err)
	}
	if isService {
		if err := agent.RunService(agent.ServiceName, log, runAgent); err != nil {
			log.Error("service host failed", "error", err)
			return exitError
		}
		return exitOK
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = runAgent(ctx, log)
	switch {
	case errors.Is(err, agent.ErrRestartRequired):
		// The binary on disk is now a different build from the one running. The
		// service manager is what restarts it — systemd because the unit
		// carries Restart=always, the Windows SCM because a non-zero exit
		// triggers the recovery action --install registers — so exiting with a
		// failure code is the restart.
		log.Info("update installed; exiting so the service manager starts the new version")
		return exitError
	case err != nil && !errors.Is(err, context.Canceled):
		log.Error("agent exited with error", "error", err)
		return exitError
	}
	log.Info("agent stopped")
	return exitOK
}
