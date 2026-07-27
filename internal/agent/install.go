package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	// ServiceName is the OS-level service/unit name the agent registers itself
	// under. On Windows it is the SCM service name and the event log source; on
	// Linux it is the systemd unit (with a ".service" suffix).
	ServiceName = "ProxBackAgent"

	// ServiceDisplayName is the friendly name shown in services.msc.
	ServiceDisplayName = "ProxBack Agent"

	// ServiceDescription is the service description registered with the OS.
	ServiceDescription = "ProxBack in-guest backup agent: heartbeats to the ProxBack server and runs file-level backup and restore jobs."

	// UnitName is the systemd unit name used on Linux.
	UnitName = "proxback-agent.service"

	// UnitPath is where the systemd unit is written on Linux.
	UnitPath = "/etc/systemd/system/proxback-agent.service"

	// LinuxInstallPath is where the agent binary is installed on Linux.
	LinuxInstallPath = "/usr/local/bin/proxback-agent"

	// RestartDelay is how long the OS waits before restarting a crashed agent.
	RestartDelay = 60 * time.Second

	// FailureResetPeriod is how long the OS waits before forgetting past
	// failures when counting restarts.
	FailureResetPeriod = 24 * time.Hour
)

// InstallOptions carries the settings baked into the registered service command
// line so a service-started agent behaves like the one the operator just ran.
type InstallOptions struct {
	// ConfigDir is the directory holding agent.json. Required.
	ConfigDir string
	// InsecureTLS adds --insecure to the service command line.
	InsecureTLS bool
	// LogLevel adds --log-level to the service command line when non-empty and
	// not the default.
	LogLevel string
	// Interval adds --interval when non-zero and not the default.
	Interval time.Duration
}

// serviceArgs renders the full command line the service is registered with.
// It is deliberately pure so it can be asserted on in tests.
func (o InstallOptions) serviceArgs() []string {
	return append([]string{"--config", o.ConfigDir}, o.extraServiceArgs()...)
}

// extraServiceArgs renders every service argument except --config, which the
// systemd unit template renders itself.
func (o InstallOptions) extraServiceArgs() []string {
	var args []string
	if o.InsecureTLS {
		args = append(args, "--insecure")
	}
	if lvl := strings.TrimSpace(o.LogLevel); lvl != "" && lvl != "info" {
		args = append(args, "--log-level", lvl)
	}
	if o.Interval > 0 && o.Interval != DefaultHeartbeatInterval {
		args = append(args, "--interval", o.Interval.String())
	}
	return args
}

// DefaultConfigDir returns the platform default configuration directory.
func DefaultConfigDir() string { return defaultConfigDir() }

func defaultConfigDir() string {
	if runtime.GOOS == "windows" {
		if pd := os.Getenv("ProgramData"); pd != "" {
			return filepath.Join(pd, "ProxBack")
		}
		return `C:\ProgramData\ProxBack`
	}
	return "/etc/proxback"
}

// EnsureConfigDir creates the configuration directory if it is missing. The
// agent stores an API key there, so it is created owner-only; on Windows the
// mode is ignored and the directory inherits the parent ACL, which for
// %ProgramData% and %ProgramFiles% means administrators-writable.
func EnsureConfigDir(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("agent: config dir required")
	}
	info, err := os.Stat(dir)
	switch {
	case err == nil && info.IsDir():
		return nil
	case err == nil:
		return fmt.Errorf("agent: config dir %q exists but is not a directory", dir)
	case !os.IsNotExist(err):
		return fmt.Errorf("agent: inspect config dir %q: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("agent: create config dir %q: %w (run this elevated, or pass --config with a writable directory)", dir, err)
	}
	return nil
}

// Enrolled reports whether configDir already holds usable agent credentials.
// The installer uses it to refuse to register a service that has nothing to
// authenticate with, which would otherwise start, fail and be restarted
// forever by the service manager.
func Enrolled(configDir string) bool {
	raw, err := os.ReadFile(filepath.Join(configDir, ConfigFileName)) //nolint:gosec // operator supplied path
	if err != nil {
		return false
	}
	var sc storedConfig
	if err := json.Unmarshal(raw, &sc); err != nil {
		return false
	}
	return sc.ServerURL != "" && sc.APIKey != ""
}

// InstallBinaryPath returns the stable location the agent binary is installed
// to for the running platform.
func InstallBinaryPath() string {
	return installBinaryPath(runtime.GOOS, os.Getenv("ProgramFiles"))
}

// installBinaryPath resolves the install location for goos. It joins Windows
// paths by hand rather than via filepath so the result is identical whichever
// platform the test runs on.
func installBinaryPath(goos, programFiles string) string {
	if goos != "windows" {
		return LinuxInstallPath
	}
	programFiles = strings.TrimRight(strings.TrimSpace(programFiles), `\/`)
	if programFiles == "" {
		programFiles = `C:\Program Files`
	}
	return programFiles + `\ProxBack\proxback-agent.exe`
}

// resolveBinarySource returns the absolute path of the running executable and
// whether it already lives at the install destination, in which case the
// installer must not copy over itself.
func resolveBinarySource(dest string) (src string, alreadyInstalled bool, err error) {
	exe, err := os.Executable()
	if err != nil {
		return "", false, fmt.Errorf("agent: locate running binary: %w", err)
	}
	if abs, aerr := filepath.Abs(exe); aerr == nil {
		exe = abs
	}
	return exe, sameFile(exe, dest), nil
}

// sameFile reports whether two paths are the same file on disk.
func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

// copyExecutable copies src over dst through a temporary file so a running
// binary is replaced atomically rather than truncated under itself.
func copyExecutable(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // operator supplied path
	if err != nil {
		return fmt.Errorf("agent: open %s: %w", src, err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("agent: create %s: %w", filepath.Dir(dst), err)
	}
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755) //nolint:gosec // executable
	if err != nil {
		return fmt.Errorf("agent: create %s: %w", tmp, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("agent: copy to %s: %w", tmp, err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("agent: close %s: %w", tmp, err)
	}
	// Windows refuses to rename over an existing file that is in use; removing
	// first turns "in use" into a clear error at the remove step instead.
	if _, err := os.Stat(dst); err == nil {
		if err := os.Remove(dst); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("agent: replace %s: %w (stop the service first)", dst, err)
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("agent: install %s: %w", dst, err)
	}
	return nil
}

// InstallInstructions returns OS-appropriate service registration instructions
// for the current binary. It changes nothing: --install does the real work and
// this is what --print-install shows for operators who would rather run the
// steps themselves.
func InstallInstructions(serverURL, token, configDir string) string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		exe = "proxback-agent"
	}
	exe, _ = filepath.Abs(exe)
	if serverURL == "" {
		serverURL = "https://proxback.example.com:8443"
	}
	if token == "" {
		token = "<enrollment-token>"
	}
	if configDir == "" {
		configDir = defaultConfigDir()
	}
	var b strings.Builder
	switch runtime.GOOS {
	case "windows":
		dest := InstallBinaryPath()
		fmt.Fprintf(&b, "ProxBack agent — Windows service setup\n")
		fmt.Fprintf(&b, "=====================================\n\n")
		fmt.Fprintf(&b, "The supported way is one elevated command, which enrolls, installs and starts the service:\n\n")
		fmt.Fprintf(&b, "   \"%s\" --server %s --token %s --install\n\n", exe, serverURL, token)
		fmt.Fprintf(&b, "To do the same by hand instead:\n\n")
		fmt.Fprintf(&b, "1. Enroll once (writes %s):\n", filepath.Join(configDir, ConfigFileName))
		fmt.Fprintf(&b, "   \"%s\" --server %s --token %s --config \"%s\" --once\n\n", exe, serverURL, token, configDir)
		fmt.Fprintf(&b, "2. Copy the binary somewhere stable:\n")
		fmt.Fprintf(&b, "   mkdir \"%s\"\n", filepath.Dir(dest))
		fmt.Fprintf(&b, "   copy \"%s\" \"%s\"\n\n", exe, dest)
		fmt.Fprintf(&b, "3. Register the service (run as Administrator):\n")
		fmt.Fprintf(&b, "   sc.exe create %s binPath= \"\\\"%s\\\" --config \\\"%s\\\"\" start= auto DisplayName= \"%s\"\n", ServiceName, dest, configDir, ServiceDisplayName)
		fmt.Fprintf(&b, "   sc.exe description %s \"%s\"\n", ServiceName, ServiceDescription)
		fmt.Fprintf(&b, "   sc.exe failure %s reset= %d actions= restart/%d\n", ServiceName,
			int(FailureResetPeriod/time.Second), int(RestartDelay/time.Millisecond))
		fmt.Fprintf(&b, "   sc.exe start %s\n\n", ServiceName)
		fmt.Fprintf(&b, "Remove with:\n   \"%s\" --uninstall\n", dest)
	default:
		fmt.Fprintf(&b, "ProxBack agent — systemd setup\n")
		fmt.Fprintf(&b, "==============================\n\n")
		fmt.Fprintf(&b, "The supported way is one command, which enrolls, installs and starts the unit:\n\n")
		fmt.Fprintf(&b, "   sudo %s --server %s --token %s --install\n\n", exe, serverURL, token)
		fmt.Fprintf(&b, "To do the same by hand instead:\n\n")
		fmt.Fprintf(&b, "1. Enroll once (writes %s):\n", filepath.Join(configDir, ConfigFileName))
		fmt.Fprintf(&b, "   sudo %s --server %s --token %s --config %s --once\n\n", exe, serverURL, token, configDir)
		fmt.Fprintf(&b, "2. Install the binary and write %s:\n\n", UnitPath)
		fmt.Fprintf(&b, "   sudo install -m 0755 %s %s\n", exe, LinuxInstallPath)
		fmt.Fprintf(&b, "   sudo tee %s <<'UNIT'\n%sUNIT\n", UnitPath, SystemdUnit(LinuxInstallPath, configDir, nil))
		fmt.Fprintf(&b, "\n3. Enable and start:\n")
		fmt.Fprintf(&b, "   sudo systemctl daemon-reload\n")
		fmt.Fprintf(&b, "   sudo systemctl enable --now %s\n\n", UnitName)
		fmt.Fprintf(&b, "Remove with:\n   sudo %s --uninstall\n", LinuxInstallPath)
	}
	return b.String()
}

// SystemdUnit renders the systemd unit for an agent started from exe with the
// given config dir and extra arguments.
func SystemdUnit(exe, configDir string, extraArgs []string) string {
	if exe == "" {
		exe = LinuxInstallPath
	}
	if configDir == "" {
		configDir = "/etc/proxback"
	}
	execStart := fmt.Sprintf("%s --config %s", exe, configDir)
	if len(extraArgs) > 0 {
		execStart += " " + strings.Join(extraArgs, " ")
	}
	return fmt.Sprintf(`[Unit]
Description=%s
Documentation=https://github.com/tlaskar-git/ProxBack
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s
Restart=always
RestartSec=%d
User=root

[Install]
WantedBy=multi-user.target
`, ServiceDisplayName, execStart, int(RestartDelay/time.Second))
}
