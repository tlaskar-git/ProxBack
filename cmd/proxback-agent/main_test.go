package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"proxback/internal/agent"
)

// capture runs the command with the given arguments and returns its exit code
// together with everything it wrote.
func capture(args ...string) (code int, stdout, stderr string) {
	var out, errOut strings.Builder
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestVersionFlag(t *testing.T) {
	code, stdout, _ := capture("--version")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d", code, exitOK)
	}
	if !strings.Contains(stdout, agent.Version) {
		t.Fatalf("stdout %q does not contain the version %q", stdout, agent.Version)
	}
}

func TestPrintInstallPrintsInstructionsAndChangesNothing(t *testing.T) {
	dir := t.TempDir()
	code, stdout, _ := capture("--print-install", "--server", "https://proxback:8443", "--token", "tok-1", "--config", dir)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d", code, exitOK)
	}
	for _, want := range []string{"https://proxback:8443", "tok-1", "--install"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("instructions do not mention %q:\n%s", want, stdout)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read config dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("--print-install wrote %d entries into the config dir, want none", len(entries))
	}
}

func TestInstallAndUninstallAreMutuallyExclusive(t *testing.T) {
	code, _, stderr := capture("--install", "--uninstall")
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "mutually exclusive") {
		t.Fatalf("stderr %q does not explain the conflict", stderr)
	}
}

func TestInvalidLogLevelIsAUsageError(t *testing.T) {
	code, _, stderr := capture("--log-level", "shouty")
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "invalid --log-level") {
		t.Fatalf("stderr %q does not name the bad flag", stderr)
	}
}

func TestUnknownFlagIsAUsageError(t *testing.T) {
	if code, _, _ := capture("--not-a-flag"); code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
}

func TestInstallRefusesWithoutCredentials(t *testing.T) {
	// A guest that was never enrolled must not get a service registered: it
	// would start, fail to authenticate and be restarted forever.
	dir := t.TempDir()
	code, _, stderr := capture("--install", "--config", dir)
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "no stored agent credentials") {
		t.Fatalf("stderr %q does not explain what is missing", stderr)
	}
	// Nothing was installed, so nothing to clean up.
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("refused install still wrote %d entries", len(entries))
	}
}

func TestInstallAcceptsAPreEnrolledConfigDir(t *testing.T) {
	// With credentials on disk the usage check passes and the platform
	// installer takes over; without elevation that fails, which is exactly the
	// message an operator needs. Either way it must not be a usage error.
	dir := t.TempDir()
	body := `{"serverUrl":"https://proxback:8443","agentId":"a1","apiKey":"k1"}`
	if err := os.WriteFile(filepath.Join(dir, agent.ConfigFileName), []byte(body), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if !agent.Enrolled(dir) {
		t.Fatal("seeded config is not recognised as enrolled")
	}
	if agent.IsElevated() {
		t.Skip("running elevated: --install would register a real service")
	}
	// Unprivileged, the platform installer refuses (Windows, systemd) or
	// degrades to printing instructions (no systemd). Never a usage error.
	code, _, _ := capture("--install", "--config", dir)
	if code == exitUsage {
		t.Fatalf("exit = %d: a pre-enrolled config dir must clear the usage check", code)
	}
}

func TestRunWritesNothingToStdoutOnUsageErrors(t *testing.T) {
	if _, stdout, _ := capture("--install", "--uninstall"); stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
}

func TestAgentIsNotAServiceUnderGoTest(t *testing.T) {
	// The interactive-vs-service decision: a test binary is always
	// interactive, on every platform, so run() must take the foreground path.
	isService, err := agent.RunningAsService()
	if err != nil {
		t.Fatalf("RunningAsService: %v", err)
	}
	if isService {
		t.Fatal("RunningAsService() = true under go test")
	}
}
