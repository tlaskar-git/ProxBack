//go:build !windows

package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// IsElevated reports whether the process has the rights an install needs.
func IsElevated() bool { return os.Geteuid() == 0 }

// ElevationHint is the message shown when an install or uninstall is attempted
// without the rights it needs.
const ElevationHint = "root is required: re-run this command with sudo"

// Install makes the enrolled agent a running systemd service: the binary is
// copied to LinuxInstallPath (unless it is already running from there), the
// unit is written, and systemd is told to enable, start and confirm it. When
// systemctl is unavailable — a container, a non-systemd host, a developer's
// laptop — it degrades to printing the manual steps instead of failing,
// because enrollment has already succeeded by this point and losing that would
// be worse.
func Install(w io.Writer, opts InstallOptions) error {
	if opts.ConfigDir == "" {
		opts.ConfigDir = defaultConfigDir()
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		fmt.Fprintf(w, "systemctl was not found, so nothing was changed.\n\n")
		fmt.Fprint(w, InstallInstructions("", "", opts.ConfigDir))
		return nil
	}
	if !IsElevated() {
		return errors.New("agent: " + ElevationHint)
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

	unit := SystemdUnit(dest, opts.ConfigDir, opts.extraServiceArgs())
	if err := os.WriteFile(UnitPath, []byte(unit), 0o644); err != nil { //nolint:gosec // unit files are world readable
		return fmt.Errorf("agent: write %s: %w", UnitPath, err)
	}
	fmt.Fprintf(w, "wrote %s\n", UnitPath)
	for _, args := range [][]string{
		{"daemon-reload"},
		{"enable", "--now", UnitName},
	} {
		if err := systemctl(args...); err != nil {
			return err
		}
		fmt.Fprintf(w, "systemctl %s\n", strings.Join(args, " "))
	}
	if err := waitForState(systemdController{}, UnitName, serviceRunning, defaultServicePoll); err != nil {
		return fmt.Errorf("agent: %s did not become active: %w\nInspect it with:\n  systemctl status %s\n  journalctl -u %s -n 50",
			UnitName, err, UnitName, UnitName)
	}
	fmt.Fprintf(w, "\nProxBack agent installed and started. Check it with:\n")
	fmt.Fprintf(w, "  systemctl status %s\n", UnitName)
	return nil
}

// Uninstall stops, disables and removes the systemd unit. It is idempotent:
// running it twice, or after a half-finished install, succeeds.
func Uninstall(w io.Writer) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		fmt.Fprintf(w, "systemctl was not found; remove %s by hand\n", UnitPath)
		return nil
	}
	if !IsElevated() {
		return errors.New("agent: " + ElevationHint)
	}
	state, err := systemdController{}.Status(UnitName)
	if err != nil {
		return fmt.Errorf("agent: query %s: %w", UnitName, err)
	}
	if state == serviceAbsent {
		fmt.Fprintf(w, "%s is not installed\n", UnitName)
		return nil
	}
	// disable --now both stops the unit and removes the enablement symlinks;
	// a unit that is loaded but already stopped is not an error for systemd.
	if err := systemctl("disable", "--now", UnitName); err != nil {
		return err
	}
	fmt.Fprintf(w, "systemctl disable --now %s\n", UnitName)
	if err := os.Remove(UnitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("agent: remove %s: %w", UnitPath, err)
	}
	fmt.Fprintf(w, "removed %s\n", UnitPath)
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	fmt.Fprintf(w, "systemctl daemon-reload\n")
	return nil
}

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", args...).CombinedOutput() //nolint:gosec // fixed argv
	if err != nil {
		return fmt.Errorf("agent: systemctl %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// systemdController is the serviceController view of systemd. Only the query
// side is used on Linux — install and uninstall drive systemctl directly, the
// same way internal/helper does — but implementing the interface lets the
// wait-for-state helper be shared with the Windows path.
type systemdController struct{}

func (systemdController) Status(name string) (serviceState, error) {
	// The unit file is the authority on "installed": systemctl is-active
	// reports "inactive" for units it has never heard of, which would make an
	// absent unit indistinguishable from a stopped one.
	if _, err := os.Stat(UnitPath); errors.Is(err, os.ErrNotExist) {
		return serviceAbsent, nil
	}
	out, _ := exec.Command("systemctl", "is-active", name).Output() //nolint:gosec // fixed argv
	switch strings.TrimSpace(string(out)) {
	case "active":
		return serviceRunning, nil
	case "activating", "reloading":
		return serviceStartPending, nil
	case "deactivating":
		return serviceStopPending, nil
	case "inactive", "failed":
		return serviceStopped, nil
	default:
		return serviceUnknown, nil
	}
}

func (systemdController) Create(serviceSpec) error         { return errors.New("unsupported") }
func (systemdController) Start(string) error               { return systemctl("start", UnitName) }
func (systemdController) Stop(string) error                { return systemctl("stop", UnitName) }
func (systemdController) Delete(string) error              { return os.Remove(UnitPath) }
func (systemdController) RegisterEventSource(string) error { return nil }
func (systemdController) RemoveEventSource(string) error   { return nil }
func (systemdController) Close() error                     { return nil }
