package helper

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// InstallPath is where the helper binary lives on a Proxmox node.
const InstallPath = "/usr/local/bin/proxback-helper"

// UnitPath is the systemd unit the installer writes.
const UnitPath = "/etc/systemd/system/proxback-helper.service"

// UnitName is the systemd unit name.
const UnitName = "proxback-helper.service"

// ServiceUnit is the systemd unit content. It is kept byte-identical to
// deploy/proxback-helper.service (a test enforces that) so an operator who
// installs by hand and one who runs --install end up with the same node.
const ServiceUnit = `[Unit]
Description=ProxBack node helper (vzdump/qmrestore streaming for agentless VM backup)
Documentation=https://github.com/tlaskar-git/ProxBack
After=network-online.target pve-cluster.service
Wants=network-online.target

[Service]
Type=simple
# Must run as root on the Proxmox node: it invokes vzdump and qmrestore.
ExecStart=/usr/local/bin/proxback-helper --config /etc/proxback-helper
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
`

// Install makes the enrolled helper a running systemd service: the binary is
// copied to InstallPath (unless it is already running from there), the unit is
// written and systemd is told to enable and start it. Every step is reported on
// w. When systemctl is unavailable — a container, a non-systemd host, a
// developer's laptop — it degrades to printing the manual steps instead of
// failing, because enrollment has already succeeded by this point and losing
// that would be worse.
func Install(w io.Writer, configDir string) error {
	if configDir == "" {
		configDir = DefaultConfigDir
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("helper: locate running binary: %w", err)
	}
	if abs, aerr := filepath.Abs(exe); aerr == nil {
		exe = abs
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		fmt.Fprint(w, Instructions(exe, configDir))
		return nil
	}
	if sameFile(exe, InstallPath) {
		fmt.Fprintf(w, "binary already installed at %s\n", InstallPath)
	} else {
		if err := copyExecutable(exe, InstallPath); err != nil {
			return err
		}
		fmt.Fprintf(w, "copied %s to %s\n", exe, InstallPath)
	}
	if err := os.WriteFile(UnitPath, []byte(ServiceUnit), 0o644); err != nil { //nolint:gosec // unit files are world readable
		return fmt.Errorf("helper: write %s: %w", UnitPath, err)
	}
	fmt.Fprintf(w, "wrote %s\n", UnitPath)
	for _, args := range [][]string{
		{"daemon-reload"},
		{"enable", "--now", UnitName},
	} {
		out, err := exec.Command("systemctl", args...).CombinedOutput() //nolint:gosec // fixed argv
		if err != nil {
			return fmt.Errorf("helper: systemctl %s: %w: %s",
				strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		fmt.Fprintf(w, "systemctl %s\n", strings.Join(args, " "))
	}
	fmt.Fprintf(w, "\nProxBack node helper installed and started. Check it with:\n")
	fmt.Fprintf(w, "  systemctl status %s\n", UnitName)
	fmt.Fprintf(w, "  curl -s http://127.0.0.1:%d/healthz\n", DefaultPort)
	return nil
}

// Instructions renders the manual installation steps.
func Instructions(exe, configDir string) string {
	if exe == "" {
		exe = InstallPath
	}
	if configDir == "" {
		configDir = DefaultConfigDir
	}
	var b strings.Builder
	fmt.Fprintf(&b, "ProxBack node helper — manual systemd setup\n")
	fmt.Fprintf(&b, "==========================================\n\n")
	fmt.Fprintf(&b, "systemctl was not found, so nothing was changed. On a Proxmox node run:\n\n")
	fmt.Fprintf(&b, "  install -m 0755 %s %s\n", exe, InstallPath)
	fmt.Fprintf(&b, "  cat > %s <<'UNIT'\n%sUNIT\n", UnitPath, ServiceUnit)
	fmt.Fprintf(&b, "  systemctl daemon-reload\n")
	fmt.Fprintf(&b, "  systemctl enable --now %s\n\n", UnitName)
	fmt.Fprintf(&b, "The helper reads its enrollment from %s.\n", filepath.Join(configDir, ConfigFileName))
	return b.String()
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

// copyExecutable copies src over dst, writing through a temporary file so a
// running binary is replaced atomically rather than truncated under itself.
func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("helper: open %s: %w", src, err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("helper: create %s: %w", filepath.Dir(dst), err)
	}
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("helper: create %s: %w", tmp, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("helper: copy to %s: %w", tmp, err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("helper: close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("helper: install %s: %w", dst, err)
	}
	return nil
}
