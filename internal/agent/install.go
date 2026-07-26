package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// InstallInstructions returns OS-appropriate service registration instructions
// for the current binary.
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
		fmt.Fprintf(&b, "ProxBack agent — Windows service setup\n")
		fmt.Fprintf(&b, "=====================================\n\n")
		fmt.Fprintf(&b, "1. Enroll once (writes %s):\n", filepath.Join(configDir, ConfigFileName))
		fmt.Fprintf(&b, "   \"%s\" --server %s --token %s --config \"%s\" --once\n\n", exe, serverURL, token, configDir)
		fmt.Fprintf(&b, "2. Register the service (run as Administrator):\n")
		fmt.Fprintf(&b, "   sc.exe create ProxBackAgent binPath= \"\\\"%s\\\" --config \\\"%s\\\"\" start= auto DisplayName= \"ProxBack Agent\"\n", exe, configDir)
		fmt.Fprintf(&b, "   sc.exe description ProxBackAgent \"ProxBack in-guest backup agent\"\n")
		fmt.Fprintf(&b, "   sc.exe start ProxBackAgent\n\n")
		fmt.Fprintf(&b, "Remove with:\n   sc.exe stop ProxBackAgent && sc.exe delete ProxBackAgent\n")
	default:
		unit := "/etc/systemd/system/proxback-agent.service"
		fmt.Fprintf(&b, "ProxBack agent — systemd setup\n")
		fmt.Fprintf(&b, "==============================\n\n")
		fmt.Fprintf(&b, "1. Enroll once (writes %s):\n", filepath.Join(configDir, ConfigFileName))
		fmt.Fprintf(&b, "   sudo %s --server %s --token %s --config %s --once\n\n", exe, serverURL, token, configDir)
		fmt.Fprintf(&b, "2. Write %s:\n\n", unit)
		fmt.Fprintf(&b, `[Unit]
Description=ProxBack Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s --config %s
Restart=always
RestartSec=10
User=root

[Install]
WantedBy=multi-user.target
`, exe, configDir)
		fmt.Fprintf(&b, "\n3. Enable and start:\n")
		fmt.Fprintf(&b, "   sudo systemctl daemon-reload\n")
		fmt.Fprintf(&b, "   sudo systemctl enable --now proxback-agent\n")
	}
	return b.String()
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
