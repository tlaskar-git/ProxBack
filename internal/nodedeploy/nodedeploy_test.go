package nodedeploy_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"proxback/internal/nodedeploy"
)

const testPassword = "s3cr3t-root-pw"

// expected remote commands, spelled out here rather than shared with the
// implementation so the wire contract is actually pinned.
const (
	wantUpload = "sh -c 'cat > /usr/local/bin/proxback-helper.tmp" +
		" && chmod 0755 /usr/local/bin/proxback-helper.tmp" +
		" && mv /usr/local/bin/proxback-helper.tmp /usr/local/bin/proxback-helper'"
	wantInstall = "/usr/local/bin/proxback-helper --server https://proxback.local:8443" +
		" --token enroll-token-123 --port 8007 --install"
)

// stageBinary writes a fake helper binary of the given size and returns its path.
func stageBinary(t *testing.T, size int) (string, []byte) {
	t.Helper()
	body := make([]byte, size)
	if _, err := rand.Read(body); err != nil {
		t.Fatalf("random binary body: %v", err)
	}
	path := filepath.Join(t.TempDir(), "proxback-helper-linux-amd64")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("stage binary: %v", err)
	}
	return path, body
}

func params(s *fakeSSH, binary, fingerprint string) nodedeploy.Params {
	return nodedeploy.Params{
		Address:             s.host,
		Port:                s.port,
		Username:            "root",
		Password:            testPassword,
		ExpectedFingerprint: fingerprint,
		BinaryPath:          binary,
		ServerURL:           "https://proxback.local:8443",
		EnrollToken:         "enroll-token-123",
	}
}

func TestDeployInstallsTheHelper(t *testing.T) {
	srv := startFakeSSH(t, testPassword)
	srv.setReply(func(index int, _ string) (string, int) {
		if index == 1 {
			return "enrolled as helper h-1 for node pve1 (port 8007)\nsystemctl enable --now proxback-helper.service\n", 0
		}
		return "", 0
	})
	binary, body := stageBinary(t, 3<<20) // 3 MiB

	res, err := nodedeploy.Deploy(context.Background(), params(srv, binary, srv.fingerprint()))
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	cmds := srv.commands()
	if len(cmds) != 2 {
		t.Fatalf("ran %d commands, want 2: %+v", len(cmds), cmds)
	}
	if cmds[0].Command != wantUpload {
		t.Errorf("upload command =\n  %q\nwant\n  %q", cmds[0].Command, wantUpload)
	}
	if cmds[1].Command != wantInstall {
		t.Errorf("installer command =\n  %q\nwant\n  %q", cmds[1].Command, wantInstall)
	}
	if !bytes.Equal(cmds[0].Stdin, body) {
		t.Errorf("uploaded %d bytes, want the exact %d byte binary", len(cmds[0].Stdin), len(body))
	}
	if len(cmds[1].Stdin) != 0 {
		t.Errorf("installer received %d bytes of stdin, want none", len(cmds[1].Stdin))
	}

	if len(res.Log) != 3 {
		t.Fatalf("log = %#v, want 3 lines", res.Log)
	}
	wantConnected := "connected to " + srv.addr() + " (" + srv.fingerprint() + ")"
	if res.Log[0] != wantConnected {
		t.Errorf("log[0] = %q, want %q", res.Log[0], wantConnected)
	}
	if res.Log[1] != "uploaded proxback-helper (3.0 MiB)" {
		t.Errorf("log[1] = %q", res.Log[1])
	}
	if !strings.HasPrefix(res.Log[2], "installer: enrolled as helper h-1") ||
		!strings.Contains(res.Log[2], "systemctl enable --now") {
		t.Errorf("log[2] = %q", res.Log[2])
	}
	for i, line := range res.Log {
		if strings.Contains(line, testPassword) {
			t.Fatalf("log[%d] leaks the password: %q", i, line)
		}
	}
}

func TestDeployRejectsAnUnconfirmedHostKey(t *testing.T) {
	// An empty fingerprint is the first-contact case; a wrong one is a changed
	// or spoofed host. Both must stop before authentication.
	for _, tc := range []struct {
		name        string
		fingerprint string
	}{
		{"not confirmed yet", ""},
		{"mismatch", "SHA256:GG3RcZLcuKzXTGzKPRIcOsuTbcnDVSuJPHVGoOAcUZk"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := startFakeSSH(t, testPassword)
			binary, _ := stageBinary(t, 1024)

			res, err := nodedeploy.Deploy(context.Background(), params(srv, binary, tc.fingerprint))
			var fe *nodedeploy.FingerprintError
			if !errors.As(err, &fe) {
				t.Fatalf("Deploy error = %v (%T), want *FingerprintError", err, err)
			}
			if fe.Fingerprint != srv.fingerprint() {
				t.Errorf("reported fingerprint = %q, want %q", fe.Fingerprint, srv.fingerprint())
			}
			if len(res.Log) != 0 {
				t.Errorf("log = %#v, want nothing", res.Log)
			}
			if cmds := srv.commands(); len(cmds) != 0 {
				t.Errorf("ran %d commands, want none: %+v", len(cmds), cmds)
			}
			if n := srv.authAttempts(); n != 0 {
				t.Errorf("authentication attempts = %d, want 0", n)
			}
			if strings.Contains(err.Error(), testPassword) {
				t.Error("error leaks the password")
			}
		})
	}
}

// A confirmed fingerprint without the SHA256: prefix still matches.
func TestDeployAcceptsAFingerprintWithoutThePrefix(t *testing.T) {
	srv := startFakeSSH(t, testPassword)
	binary, _ := stageBinary(t, 1024)
	bare := strings.TrimPrefix(srv.fingerprint(), "SHA256:")

	if _, err := nodedeploy.Deploy(context.Background(), params(srv, binary, bare)); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if cmds := srv.commands(); len(cmds) != 2 {
		t.Fatalf("ran %d commands, want 2", len(cmds))
	}
}

func TestDeployWrongPassword(t *testing.T) {
	srv := startFakeSSH(t, testPassword)
	binary, _ := stageBinary(t, 1024)
	p := params(srv, binary, srv.fingerprint())
	p.Password = "wrong-password"

	_, err := nodedeploy.Deploy(context.Background(), p)
	if err == nil {
		t.Fatal("Deploy with a wrong password succeeded")
	}
	var fe *nodedeploy.FingerprintError
	if errors.As(err, &fe) {
		t.Fatalf("wrong password reported as a fingerprint problem: %v", err)
	}
	if !strings.Contains(err.Error(), "root@"+srv.addr()) {
		t.Errorf("error = %v, want it to name the ssh login", err)
	}
	if strings.Contains(err.Error(), p.Password) {
		t.Error("error leaks the password")
	}
	if n := srv.authAttempts(); n == 0 {
		t.Error("the server saw no authentication attempt")
	}
	if cmds := srv.commands(); len(cmds) != 0 {
		t.Errorf("ran %d commands after failed auth, want none: %+v", len(cmds), cmds)
	}
}

func TestDeploySurfacesInstallerFailure(t *testing.T) {
	srv := startFakeSSH(t, testPassword)
	srv.setReply(func(index int, _ string) (string, int) {
		if index == 1 {
			return "enrollment failed: invalid or expired enrollment token\n", 1
		}
		return "", 0
	})
	binary, _ := stageBinary(t, 1024)

	res, err := nodedeploy.Deploy(context.Background(), params(srv, binary, srv.fingerprint()))
	if err == nil {
		t.Fatal("Deploy succeeded despite a failing installer")
	}
	if !strings.Contains(err.Error(), "invalid or expired enrollment token") {
		t.Errorf("error = %v, want the installer output tail", err)
	}
	if !strings.Contains(err.Error(), "installer failed") {
		t.Errorf("error = %v, want it to name the failing step", err)
	}
	// The steps that did succeed are still reported.
	if len(res.Log) != 2 || !strings.HasPrefix(res.Log[1], "uploaded proxback-helper") {
		t.Errorf("log = %#v, want the connect and upload lines", res.Log)
	}
}

func TestDeploySurfacesUploadFailure(t *testing.T) {
	srv := startFakeSSH(t, testPassword)
	srv.setReply(func(index int, _ string) (string, int) {
		if index == 0 {
			return "sh: 1: cannot create /usr/local/bin/proxback-helper.tmp: Read-only file system\n", 2
		}
		return "", 0
	})
	binary, _ := stageBinary(t, 1024)

	res, err := nodedeploy.Deploy(context.Background(), params(srv, binary, srv.fingerprint()))
	if err == nil {
		t.Fatal("Deploy succeeded despite a failing upload")
	}
	if !strings.Contains(err.Error(), "Read-only file system") {
		t.Errorf("error = %v, want the remote output", err)
	}
	if cmds := srv.commands(); len(cmds) != 1 {
		t.Errorf("ran %d commands, want only the upload: %+v", len(cmds), cmds)
	}
	if len(res.Log) != 1 {
		t.Errorf("log = %#v, want only the connect line", res.Log)
	}
}

// A long installer output is truncated to a bounded tail, keeping the end.
func TestDeployBoundsTheOutputTail(t *testing.T) {
	srv := startFakeSSH(t, testPassword)
	srv.setReply(func(index int, _ string) (string, int) {
		if index == 1 {
			return strings.Repeat("x", 40<<10) + "TAIL-MARKER", 0
		}
		return "", 0
	})
	binary, _ := stageBinary(t, 1024)

	res, err := nodedeploy.Deploy(context.Background(), params(srv, binary, srv.fingerprint()))
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	line := res.Log[len(res.Log)-1]
	if len(line) > nodedeploy.OutputTailBytes+128 {
		t.Errorf("installer log line is %d bytes, want it bounded to ~%d", len(line), nodedeploy.OutputTailBytes)
	}
	if !strings.HasSuffix(line, "TAIL-MARKER") {
		t.Error("the tail of the output was not kept")
	}
	if !strings.Contains(line, "truncated") {
		t.Errorf("truncation is not signalled: %q", line[:64])
	}
}

func TestDeployValidatesParams(t *testing.T) {
	binary, _ := stageBinary(t, 16)
	base := nodedeploy.Params{
		Address: "192.0.2.10", Username: "root", Password: "pw",
		BinaryPath: binary, ServerURL: "https://proxback.local", EnrollToken: "tok",
	}
	for _, tc := range []struct {
		name   string
		mutate func(*nodedeploy.Params)
		want   string
	}{
		{"no address", func(p *nodedeploy.Params) { p.Address = "" }, "address is required"},
		{"no username", func(p *nodedeploy.Params) { p.Username = "" }, "username is required"},
		{"no password", func(p *nodedeploy.Params) { p.Password = "" }, "password is required"},
		{"no binary", func(p *nodedeploy.Params) { p.BinaryPath = "" }, "binaryPath is required"},
		{"no serverUrl", func(p *nodedeploy.Params) { p.ServerURL = "" }, "serverUrl is required"},
		{"no token", func(p *nodedeploy.Params) { p.EnrollToken = "" }, "enrollToken is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.mutate(&p)
			_, err := nodedeploy.Deploy(context.Background(), p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Deploy error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestDeployMissingBinary(t *testing.T) {
	p := nodedeploy.Params{
		Address: "192.0.2.10", Username: "root", Password: "pw",
		BinaryPath: filepath.Join(t.TempDir(), "absent"),
		ServerURL:  "https://proxback.local", EnrollToken: "tok",
	}
	if _, err := nodedeploy.Deploy(context.Background(), p); err == nil ||
		!strings.Contains(err.Error(), "helper binary") {
		t.Fatalf("Deploy error = %v, want a missing binary error", err)
	}
}
