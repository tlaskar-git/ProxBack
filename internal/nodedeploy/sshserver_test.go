package nodedeploy_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

// fakeSSH is an in-process SSH server standing in for a Proxmox node's sshd. It
// accepts one fixed password, records every exec request with the stdin it
// received and lets a test decide each command's output and exit status.
type fakeSSH struct {
	t        *testing.T
	password string
	host     string
	port     int
	pub      ssh.PublicKey

	// reply decides what a command prints and what it exits with. nil means
	// "print nothing, exit 0".
	reply func(index int, cmd string) (output string, exit int)

	mu       sync.Mutex
	execs    []execRecord
	authTry  int
	listener net.Listener
}

type execRecord struct {
	Command string
	Stdin   []byte
}

// startFakeSSH brings the server up on an ephemeral loopback port and tears it
// down with the test.
func startFakeSSH(t *testing.T, password string) *fakeSSH {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("host key signer: %v", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	host, portStr, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatalf("split listener address %q: %v", l.Addr(), err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("listener port %q: %v", portStr, err)
	}
	s := &fakeSSH{
		t: t, password: password, host: host, port: port,
		pub: signer.PublicKey(), listener: l,
	}
	cfg := &ssh.ServerConfig{PasswordCallback: s.checkPassword}
	cfg.AddHostKey(signer)

	go s.serve(cfg)
	t.Cleanup(func() { _ = l.Close() })
	return s
}

// fingerprint is the SHA256 fingerprint of the server's host key, as an
// operator would confirm it.
func (s *fakeSSH) fingerprint() string { return ssh.FingerprintSHA256(s.pub) }

func (s *fakeSSH) checkPassword(_ ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
	s.mu.Lock()
	s.authTry++
	s.mu.Unlock()
	if string(pass) == s.password {
		return nil, nil
	}
	return nil, errors.New("password rejected")
}

func (s *fakeSSH) serve(cfg *ssh.ServerConfig) {
	for {
		nc, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handleConn(nc, cfg)
	}
}

func (s *fakeSSH) handleConn(nc net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		// Expected for the fingerprint-mismatch and bad-password cases.
		_ = nc.Close()
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		if nch.ChannelType() != "session" {
			_ = nch.Reject(ssh.UnknownChannelType, "only sessions are supported")
			continue
		}
		ch, chReqs, err := nch.Accept()
		if err != nil {
			return
		}
		go s.handleSession(ch, chReqs)
	}
}

func (s *fakeSSH) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
			_ = req.Reply(false, nil)
			return
		}
		if req.WantReply {
			_ = req.Reply(true, nil)
		}
		// Read stdin to EOF first: the upload streams the binary here.
		stdin, err := io.ReadAll(ch)
		if err != nil {
			s.t.Errorf("read stdin for %q: %v", payload.Command, err)
		}
		out, code := s.record(payload.Command, stdin)
		if out != "" {
			if _, err := io.WriteString(ch, out); err != nil {
				return
			}
		}
		_ = ch.CloseWrite()
		_, _ = ch.SendRequest("exit-status", false,
			ssh.Marshal(struct{ Status uint32 }{uint32(code)})) //nolint:gosec // small non-negative exit codes
		return
	}
}

// setReply installs the per-command response decider.
func (s *fakeSSH) setReply(fn func(index int, cmd string) (string, int)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reply = fn
}

func (s *fakeSSH) record(cmd string, stdin []byte) (string, int) {
	s.mu.Lock()
	index := len(s.execs)
	s.execs = append(s.execs, execRecord{Command: cmd, Stdin: stdin})
	reply := s.reply
	s.mu.Unlock()
	if reply == nil {
		return "", 0
	}
	return reply(index, cmd)
}

// commands returns the recorded exec requests in order.
func (s *fakeSSH) commands() []execRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]execRecord(nil), s.execs...)
}

// authAttempts counts password authentication attempts the server saw.
func (s *fakeSSH) authAttempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authTry
}

func (s *fakeSSH) addr() string { return fmt.Sprintf("%s:%d", s.host, s.port) }
