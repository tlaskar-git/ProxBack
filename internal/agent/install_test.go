package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestInstallBinaryPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		goos         string
		programFiles string
		want         string
	}{
		{"linux ignores ProgramFiles", "linux", `C:\Program Files`, LinuxInstallPath},
		{"darwin", "darwin", "", LinuxInstallPath},
		{"windows default", "windows", "", `C:\Program Files\ProxBack\proxback-agent.exe`},
		{"windows custom", "windows", `D:\Apps`, `D:\Apps\ProxBack\proxback-agent.exe`},
		{"windows trailing separator", "windows", `D:\Apps\`, `D:\Apps\ProxBack\proxback-agent.exe`},
		{"windows padded", "windows", `  E:\PF  `, `E:\PF\ProxBack\proxback-agent.exe`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := installBinaryPath(tc.goos, tc.programFiles); got != tc.want {
				t.Fatalf("installBinaryPath(%q, %q) = %q, want %q", tc.goos, tc.programFiles, got, tc.want)
			}
		})
	}
}

func TestInstallBinaryPathUsesEnvironment(t *testing.T) {
	// Not parallel: mutates the process environment.
	t.Setenv("ProgramFiles", `X:\PF`)
	got := InstallBinaryPath()
	if want := installBinaryPath(runtime.GOOS, `X:\PF`); got != want {
		t.Fatalf("InstallBinaryPath() = %q, want %q", got, want)
	}
}

func TestEnsureConfigDirCreatesMissingDirectory(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "nested", "ProxBack")
	if err := EnsureConfigDir(dir); err != nil {
		t.Fatalf("EnsureConfigDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat after create: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", dir)
	}
	// Creating an existing directory must succeed unchanged.
	if err := EnsureConfigDir(dir); err != nil {
		t.Fatalf("EnsureConfigDir (second call): %v", err)
	}
}

func TestEnsureConfigDirRejectsBadInput(t *testing.T) {
	t.Parallel()
	if err := EnsureConfigDir("  "); err == nil {
		t.Fatal("EnsureConfigDir(blank) = nil, want error")
	}
	file := filepath.Join(t.TempDir(), "agent.cfg")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	err := EnsureConfigDir(file)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("EnsureConfigDir(file) = %v, want a not-a-directory error", err)
	}
}

func TestInstallOptionsServiceArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		opts InstallOptions
		want []string
	}{
		{
			name: "defaults carry only the config dir",
			opts: InstallOptions{ConfigDir: `C:\ProgramData\ProxBack`, LogLevel: "info", Interval: DefaultHeartbeatInterval},
			want: []string{"--config", `C:\ProgramData\ProxBack`},
		},
		{
			name: "non-default settings are baked in",
			opts: InstallOptions{ConfigDir: "/etc/proxback", InsecureTLS: true, LogLevel: "debug", Interval: 90 * time.Second},
			want: []string{"--config", "/etc/proxback", "--insecure", "--log-level", "debug", "--interval", "1m30s"},
		},
		{
			name: "blank log level is omitted",
			opts: InstallOptions{ConfigDir: "/etc/proxback"},
			want: []string{"--config", "/etc/proxback"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.opts.serviceArgs()
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("serviceArgs() = %v, want %v", got, tc.want)
			}
			// extraServiceArgs is the same list minus --config <dir>.
			extra := tc.opts.extraServiceArgs()
			if strings.Join(extra, " ") != strings.Join(tc.want[2:], " ") {
				t.Fatalf("extraServiceArgs() = %v, want %v", extra, tc.want[2:])
			}
		})
	}
}

func TestSystemdUnitRendersExecStart(t *testing.T) {
	t.Parallel()
	unit := SystemdUnit("/usr/local/bin/proxback-agent", "/etc/proxback", []string{"--insecure"})
	for _, want := range []string{
		"ExecStart=/usr/local/bin/proxback-agent --config /etc/proxback --insecure",
		"Restart=always",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit)
		}
	}
	// Empty arguments fall back to the documented defaults.
	def := SystemdUnit("", "", nil)
	if !strings.Contains(def, "ExecStart="+LinuxInstallPath+" --config /etc/proxback") {
		t.Fatalf("default unit ExecStart wrong:\n%s", def)
	}
}

func TestInstallInstructionsDocumentTheRealInstaller(t *testing.T) {
	t.Parallel()
	got := InstallInstructions("https://proxback:8443", "tok-123", t.TempDir())
	for _, want := range []string{"--install", "--uninstall", "https://proxback:8443", "tok-123"} {
		if !strings.Contains(got, want) {
			t.Fatalf("instructions missing %q:\n%s", want, got)
		}
	}
}

func TestDefaultConfigDirIsAbsolute(t *testing.T) {
	t.Parallel()
	if dir := DefaultConfigDir(); !filepath.IsAbs(dir) {
		t.Fatalf("DefaultConfigDir() = %q, want an absolute path", dir)
	}
}

func TestEnrolled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if Enrolled(dir) {
		t.Fatal("Enrolled on an empty directory = true, want false")
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(body), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
	}
	write("not json")
	if Enrolled(dir) {
		t.Fatal("Enrolled on a corrupt config = true, want false")
	}
	write(`{"serverUrl":"https://proxback:8443"}`)
	if Enrolled(dir) {
		t.Fatal("Enrolled without an API key = true, want false")
	}
	write(`{"serverUrl":"https://proxback:8443","agentId":"a1","apiKey":"k1"}`)
	if !Enrolled(dir) {
		t.Fatal("Enrolled on a complete config = false, want true")
	}
}

func TestCopyExecutableReplacesDestination(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "sub", "dst.bin")
	if err := os.WriteFile(src, []byte("new"), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write src: %v", err)
	}
	if err := copyExecutable(src, dst); err != nil {
		t.Fatalf("copyExecutable: %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "new" {
		t.Fatalf("dst = %q, want %q", got, "new")
	}
	// A second copy must overwrite rather than fail.
	if err := os.WriteFile(src, []byte("newer"), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatalf("rewrite src: %v", err)
	}
	if err := copyExecutable(src, dst); err != nil {
		t.Fatalf("copyExecutable (overwrite): %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "newer" {
		t.Fatalf("dst = %q, want %q", got, "newer")
	}
}

func TestResolveBinarySourceDetectsSelf(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable unavailable: %v", err)
	}
	src, installed, err := resolveBinarySource(self)
	if err != nil {
		t.Fatalf("resolveBinarySource: %v", err)
	}
	if !installed {
		t.Fatalf("resolveBinarySource(%q) reported not installed, src=%q", self, src)
	}
	if !filepath.IsAbs(src) {
		t.Fatalf("src = %q, want an absolute path", src)
	}
	if _, installed, err = resolveBinarySource(filepath.Join(t.TempDir(), "nope.exe")); err != nil || installed {
		t.Fatalf("resolveBinarySource(missing) = installed %v, err %v", installed, err)
	}
}
