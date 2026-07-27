//go:build windows

package agent

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

// IsElevated reports whether the process is running with an elevated token.
func IsElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// ElevationHint is the message shown when an install or uninstall is attempted
// without the rights it needs.
const ElevationHint = "administrator rights are required: right-click Command Prompt or PowerShell and choose \"Run as administrator\", then run this command again"

// Install copies the binary to a stable location, registers the ProxBack agent
// with the Windows Service Control Manager, starts it and verifies that it
// actually reached the running state.
func Install(w io.Writer, opts InstallOptions) error {
	if !IsElevated() {
		return errors.New("agent: " + ElevationHint)
	}
	if opts.ConfigDir == "" {
		opts.ConfigDir = defaultConfigDir()
	}
	if err := EnsureConfigDir(opts.ConfigDir); err != nil {
		return err
	}
	dest := InstallBinaryPath()
	src, installed, err := resolveBinarySource(dest)
	if err != nil {
		return err
	}
	if installed {
		fmt.Fprintf(w, "binary already installed at %s\n", dest)
	} else {
		if err := copyExecutable(src, dest); err != nil {
			return err
		}
		fmt.Fprintf(w, "copied %s to %s\n", src, dest)
	}

	sc, err := newServiceController()
	if err != nil {
		return err
	}
	defer sc.Close()

	spec := serviceSpec{
		Name:         ServiceName,
		DisplayName:  ServiceDisplayName,
		Description:  ServiceDescription,
		BinaryPath:   dest,
		Args:         opts.serviceArgs(),
		RestartDelay: RestartDelay,
		ResetPeriod:  FailureResetPeriod,
	}
	if err := installService(w, sc, spec, defaultServicePoll); err != nil {
		return err
	}
	fmt.Fprintf(w, "\nProxBack agent installed. Check it with:\n")
	fmt.Fprintf(w, "  sc.exe query %s\n", ServiceName)
	fmt.Fprintf(w, "  Get-EventLog -LogName Application -Source %s -Newest 20\n", ServiceName)
	return nil
}

// Uninstall stops and removes the service and its event log source.
func Uninstall(w io.Writer) error {
	if !IsElevated() {
		return errors.New("agent: " + ElevationHint)
	}
	sc, err := newServiceController()
	if err != nil {
		return err
	}
	defer sc.Close()
	return uninstallService(w, sc, ServiceName, defaultServicePoll)
}

// ---------------------------------------------------------------- SCM adapter

// windowsServiceController adapts the Windows service manager to
// serviceController. It is the only place in the package that talks to the SCM.
type windowsServiceController struct{ m *mgr.Mgr }

func newServiceController() (serviceController, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, fmt.Errorf("agent: connect to the Windows service manager: %w (%s)", err, ElevationHint)
	}
	return &windowsServiceController{m: m}, nil
}

func (c *windowsServiceController) Close() error { return c.m.Disconnect() }

func (c *windowsServiceController) open(name string) (*mgr.Service, error) {
	s, err := c.m.OpenService(name)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return nil, errServiceAbsent
		}
		// Older Windows builds report a plain "specified service does not
		// exist" text through a non-matching errno; treat that as absent too
		// rather than turning a missing service into a hard failure.
		if strings.Contains(strings.ToLower(err.Error()), "does not exist") {
			return nil, errServiceAbsent
		}
		return nil, err
	}
	return s, nil
}

func (c *windowsServiceController) Status(name string) (serviceState, error) {
	s, err := c.open(name)
	if errors.Is(err, errServiceAbsent) {
		return serviceAbsent, nil
	}
	if err != nil {
		return serviceUnknown, err
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return serviceUnknown, err
	}
	return stateFromSvc(st.State), nil
}

func stateFromSvc(s svc.State) serviceState {
	switch s {
	case svc.Stopped:
		return serviceStopped
	case svc.StartPending:
		return serviceStartPending
	case svc.StopPending:
		return serviceStopPending
	case svc.Running:
		return serviceRunning
	default:
		return serviceUnknown
	}
}

func (c *windowsServiceController) Create(spec serviceSpec) error {
	s, err := c.m.CreateService(spec.Name, spec.BinaryPath, mgr.Config{
		DisplayName:  spec.DisplayName,
		Description:  spec.Description,
		StartType:    mgr.StartAutomatic,
		ErrorControl: mgr.ErrorNormal,
	}, spec.Args...)
	if err != nil {
		return err
	}
	defer s.Close()
	// Restart on failure so a crash, an OOM kill or a bad upgrade does not
	// leave the guest silently unprotected. Three identical actions cover the
	// first, second and every subsequent failure.
	delay := spec.RestartDelay
	if delay <= 0 {
		delay = RestartDelay
	}
	reset := spec.ResetPeriod
	if reset <= 0 {
		reset = FailureResetPeriod
	}
	actions := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: delay},
		{Type: mgr.ServiceRestart, Delay: delay},
		{Type: mgr.ServiceRestart, Delay: delay},
	}
	if err := s.SetRecoveryActions(actions, uint32(reset/time.Second)); err != nil {
		return fmt.Errorf("set failure actions: %w", err)
	}
	// A non-zero exit code is a "failure" for our purposes even though Windows
	// only counts crashes by default.
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("set failure actions for non-crash exits: %w", err)
	}
	return nil
}

func (c *windowsServiceController) Start(name string) error {
	s, err := c.open(name)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.Start()
}

func (c *windowsServiceController) Stop(name string) error {
	s, err := c.open(name)
	if err != nil {
		return err
	}
	defer s.Close()
	if _, err := s.Control(svc.Stop); err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return nil
		}
		return err
	}
	return nil
}

func (c *windowsServiceController) Delete(name string) error {
	s, err := c.open(name)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.Delete()
}

func (c *windowsServiceController) RegisterEventSource(name string) error {
	err := eventlog.InstallAsEventCreate(name, eventlog.Info|eventlog.Warning|eventlog.Error)
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return nil
	}
	return err
}

func (c *windowsServiceController) RemoveEventSource(name string) error {
	err := eventlog.Remove(name)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "cannot find") {
		return nil
	}
	return err
}
