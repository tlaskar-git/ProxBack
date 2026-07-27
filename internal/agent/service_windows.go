//go:build windows

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
)

// RunningAsService reports whether this process was started by the Windows
// Service Control Manager rather than from a console.
func RunningAsService() (bool, error) { return svc.IsWindowsService() }

// stopGrace is how long the handler waits for the agent to unwind after a Stop
// or Shutdown before reporting Stopped anyway. The SCM's own patience is
// finite, and a hung upload must not turn "stop the service" into "reboot the
// machine".
const stopGrace = 20 * time.Second

// Event log ids. Windows shows these next to each record; keeping them stable
// lets operators filter on them.
const (
	eventStarted  = 1
	eventStopping = 2
	eventStopped  = 3
	eventFailed   = 4
	eventLog      = 5
)

// RunService runs the agent under the Windows Service Control Manager.
//
// This is the fix for the bug that made a correctly registered service die with
// error 1053: the SCM starts the binary and then waits for it to call
// StartServiceCtrlDispatcher and report SERVICE_RUNNING. A plain console binary
// never does, so after ~30s the SCM declares the start failed and kills it.
// Here svc.Run performs that handshake and Execute reports the state
// transitions the SCM is waiting for.
//
// run is handed a logger that additionally writes to the Windows Event Log,
// because a service has no stderr for its output to go to.
func RunService(name string, base *slog.Logger, run func(ctx context.Context, log *slog.Logger) error) error {
	if base == nil {
		base = slog.Default()
	}
	h := &windowsService{name: name, run: run, log: base, runLog: base}
	// Opening the event log is best effort: the source is registered by
	// --install, but a hand-registered service may not have it and that is no
	// reason to refuse to run.
	if elog, err := eventlog.Open(name); err == nil {
		h.elog = elog
		h.runLog = slog.New(&eventLogHandler{inner: base.Handler(), elog: elog})
		defer elog.Close()
	}
	if err := svc.Run(name, h); err != nil {
		h.report(eventFailed, slog.LevelError, fmt.Sprintf("service dispatcher failed: %v", err))
		return fmt.Errorf("agent: run as windows service: %w", err)
	}
	return nil
}

type windowsService struct {
	name string
	// log records the service lifecycle; it does not mirror to the event log
	// because report writes those records itself with per-event ids.
	log *slog.Logger
	// runLog is handed to the agent and mirrors everything to the event log.
	runLog *slog.Logger
	elog   *eventlog.Log
	run    func(ctx context.Context, log *slog.Logger) error
}

// Execute implements svc.Handler.
func (s *windowsService) Execute(_ []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	// Tell the SCM we are on our way up before doing anything that can block.
	changes <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- s.run(ctx, s.runLog) }()

	changes <- svc.Status{State: svc.Running, Accepts: accepted}
	s.report(eventStarted, slog.LevelInfo, "ProxBack agent service started")

	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				// The SCM asks for the current status; echo it back verbatim.
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s.report(eventStopping, slog.LevelInfo, fmt.Sprintf("stop requested (%v); shutting the agent down", c.Cmd))
				// The wait hint stops the SCM declaring the stop hung while an
				// in-flight upload unwinds.
				changes <- svc.Status{State: svc.StopPending, WaitHint: uint32(stopGrace / time.Millisecond)}
				cancel()
				select {
				case err := <-done:
					s.reportExit(err)
				case <-time.After(stopGrace):
					s.report(eventStopped, slog.LevelWarn,
						fmt.Sprintf("agent did not unwind within %s; stopping anyway", stopGrace))
				}
				changes <- svc.Status{State: svc.Stopped}
				return false, 0
			default:
				// Unexpected control codes are ignored rather than fatal.
				s.report(eventLog, slog.LevelWarn, fmt.Sprintf("unexpected service control request %v", c.Cmd))
			}
		case err := <-done:
			// The agent returned on its own. That is a failure unless the
			// context was cancelled, and the SCM needs a non-zero exit code so
			// the configured restart action kicks in.
			changes <- svc.Status{State: svc.StopPending}
			code := s.reportExit(err)
			changes <- svc.Status{State: svc.Stopped}
			return false, code
		}
	}
}

// reportExit logs how the agent loop ended and returns the service exit code.
func (s *windowsService) reportExit(err error) uint32 {
	switch {
	case err == nil, errors.Is(err, context.Canceled):
		s.report(eventStopped, slog.LevelInfo, "ProxBack agent service stopped")
		return 0
	default:
		s.report(eventFailed, slog.LevelError, "ProxBack agent exited with an error: "+err.Error())
		return 1
	}
}

// report writes to both the agent logger and the Windows Event Log.
func (s *windowsService) report(id uint32, level slog.Level, msg string) {
	if s.log != nil {
		s.log.Log(context.Background(), level, msg)
	}
	if s.elog == nil {
		return
	}
	switch {
	case level >= slog.LevelError:
		_ = s.elog.Error(id, msg)
	case level >= slog.LevelWarn:
		_ = s.elog.Warning(id, msg)
	default:
		_ = s.elog.Info(id, msg)
	}
}

// eventLogHandler mirrors every record to the Windows Event Log in addition to
// whatever the wrapped handler does. Under the SCM stderr goes nowhere, so
// without this a service failure leaves no trace an operator can find.
type eventLogHandler struct {
	inner slog.Handler
	elog  *eventlog.Log
}

func (h *eventLogHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *eventLogHandler) Handle(ctx context.Context, rec slog.Record) error {
	err := h.inner.Handle(ctx, rec)
	msg := formatRecord(rec)
	switch {
	case rec.Level >= slog.LevelError:
		_ = h.elog.Error(eventLog, msg)
	case rec.Level >= slog.LevelWarn:
		_ = h.elog.Warning(eventLog, msg)
	case rec.Level >= slog.LevelInfo:
		_ = h.elog.Info(eventLog, msg)
	}
	return err
}

func (h *eventLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &eventLogHandler{inner: h.inner.WithAttrs(attrs), elog: h.elog}
}

func (h *eventLogHandler) WithGroup(name string) slog.Handler {
	return &eventLogHandler{inner: h.inner.WithGroup(name), elog: h.elog}
}

// formatRecord renders a slog record as one readable event log line.
func formatRecord(rec slog.Record) string {
	var b strings.Builder
	b.WriteString(rec.Message)
	rec.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value.Any())
		return true
	})
	return b.String()
}
