package agent

import (
	"errors"
	"fmt"
	"io"
	"time"
)

// serviceState is a platform-neutral view of a service's lifecycle state.
type serviceState string

const (
	serviceStopped      serviceState = "stopped"
	serviceStartPending serviceState = "start pending"
	serviceRunning      serviceState = "running"
	serviceStopPending  serviceState = "stop pending"
	serviceUnknown      serviceState = "unknown"
	serviceAbsent       serviceState = "not installed"
)

// serviceSpec describes the registration the installer wants to create.
type serviceSpec struct {
	Name        string
	DisplayName string
	Description string
	BinaryPath  string
	Args        []string
	// RestartDelay is how long the service manager waits before restarting the
	// service after it fails.
	RestartDelay time.Duration
	// ResetPeriod is how long the service manager waits before forgetting past
	// failures.
	ResetPeriod time.Duration
}

// serviceController is the small slice of a platform service manager the
// installer needs. Everything that talks to the Windows SCM lives behind it so
// the install and uninstall logic can be exercised with a fake on any platform.
type serviceController interface {
	// Status reports the current state, or serviceAbsent when the service is
	// not registered at all.
	Status(name string) (serviceState, error)
	Create(spec serviceSpec) error
	Start(name string) error
	Stop(name string) error
	Delete(name string) error
	// RegisterEventSource makes the service's log messages readable in the
	// platform's event log. A no-op where there is no such concept.
	RegisterEventSource(name string) error
	RemoveEventSource(name string) error
	Close() error
}

// servicePoll bounds how long the installer waits for a state transition.
type servicePoll struct {
	Interval time.Duration
	Timeout  time.Duration
}

// defaultServicePoll gives the SCM 45s to bring the service up. The service
// itself must report SERVICE_RUNNING within ~30s or the SCM fails the start
// with error 1053, so waiting a little longer than that lets us surface the
// SCM's own verdict rather than a timeout of our own invention.
var defaultServicePoll = servicePoll{Interval: 500 * time.Millisecond, Timeout: 45 * time.Second}

// errServiceAbsent is returned by helpers asked to act on a service that is not
// registered.
var errServiceAbsent = errors.New("service is not installed")

// installService registers, starts and then verifies the service. Any previous
// registration is removed first so --install is repeatable. The verification
// step is the point of the whole function: a service that is created and
// started but never reaches Running is exactly the failure mode operators hit,
// and it must be reported as an error rather than a cheerful success message.
func installService(w io.Writer, sc serviceController, spec serviceSpec, poll servicePoll) error {
	if spec.Name == "" || spec.BinaryPath == "" {
		return errors.New("agent: service name and binary path are required")
	}
	state, err := sc.Status(spec.Name)
	if err != nil {
		return fmt.Errorf("agent: query service %s: %w", spec.Name, err)
	}
	if state != serviceAbsent {
		fmt.Fprintf(w, "service %s already exists (%s); replacing it\n", spec.Name, state)
		if err := removeService(w, sc, spec.Name, poll); err != nil {
			return err
		}
	}
	if err := sc.RegisterEventSource(spec.Name); err != nil {
		// Not fatal: the agent still logs to its own logger, and an operator
		// without the registry permission should not be blocked from running.
		fmt.Fprintf(w, "warning: could not register the event log source: %v\n", err)
	}
	if err := sc.Create(spec); err != nil {
		return fmt.Errorf("agent: create service %s: %w", spec.Name, err)
	}
	fmt.Fprintf(w, "registered service %s -> %s %v\n", spec.Name, spec.BinaryPath, spec.Args)
	if err := sc.Start(spec.Name); err != nil {
		return fmt.Errorf("agent: start service %s: %w", spec.Name, err)
	}
	if err := waitForState(sc, spec.Name, serviceRunning, poll); err != nil {
		last, qerr := sc.Status(spec.Name)
		if qerr != nil {
			last = serviceUnknown
		}
		return fmt.Errorf("agent: service %s did not reach the running state (last state: %s): %w\n"+
			"Check the Windows Event Log (Application, source %s) and try running the binary interactively:\n"+
			"  %s %v", spec.Name, last, err, spec.Name, spec.BinaryPath, spec.Args)
	}
	fmt.Fprintf(w, "service %s is running\n", spec.Name)
	return nil
}

// uninstallService stops and removes the service. It is idempotent: removing a
// service that is not installed reports that and succeeds, so an operator can
// run --uninstall twice, or after a half-finished install, without having to
// reason about what state the machine is in.
func uninstallService(w io.Writer, sc serviceController, name string, poll servicePoll) error {
	state, err := sc.Status(name)
	if err != nil {
		return fmt.Errorf("agent: query service %s: %w", name, err)
	}
	if state == serviceAbsent {
		fmt.Fprintf(w, "service %s is not installed\n", name)
	} else if err := removeService(w, sc, name, poll); err != nil {
		return err
	}
	if err := sc.RemoveEventSource(name); err != nil {
		fmt.Fprintf(w, "warning: could not remove the event log source: %v\n", err)
	}
	return nil
}

// removeService stops (if needed) and deletes an existing service.
func removeService(w io.Writer, sc serviceController, name string, poll servicePoll) error {
	state, err := sc.Status(name)
	if err != nil {
		return fmt.Errorf("agent: query service %s: %w", name, err)
	}
	if state == serviceAbsent {
		return nil
	}
	if state != serviceStopped {
		if err := sc.Stop(name); err != nil && !errors.Is(err, errServiceAbsent) {
			return fmt.Errorf("agent: stop service %s: %w", name, err)
		}
		if err := waitForState(sc, name, serviceStopped, poll); err != nil {
			return fmt.Errorf("agent: service %s did not stop: %w", name, err)
		}
		fmt.Fprintf(w, "stopped service %s\n", name)
	}
	if err := sc.Delete(name); err != nil && !errors.Is(err, errServiceAbsent) {
		return fmt.Errorf("agent: delete service %s: %w", name, err)
	}
	fmt.Fprintf(w, "removed service %s\n", name)
	return nil
}

// waitForState polls until the service reaches want or the timeout expires.
// serviceAbsent counts as reaching serviceStopped: a service deleted from under
// us is, for the caller's purposes, no longer running.
func waitForState(sc serviceController, name string, want serviceState, poll servicePoll) error {
	if poll.Interval <= 0 {
		poll.Interval = defaultServicePoll.Interval
	}
	if poll.Timeout <= 0 {
		poll.Timeout = defaultServicePoll.Timeout
	}
	deadline := time.Now().Add(poll.Timeout)
	for {
		state, err := sc.Status(name)
		if err != nil {
			return err
		}
		if state == want || (want == serviceStopped && state == serviceAbsent) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %q (state: %s)", poll.Timeout, want, state)
		}
		time.Sleep(poll.Interval)
	}
}
