// Package nodedeploy installs the ProxBack node helper on a Proxmox VE node
// over SSH, so an operator never has to open a shell on the node: the server
// verifies the host key fingerprint, authenticates with the password the
// operator typed once, streams the staged helper binary to
// /usr/local/bin/proxback-helper and runs the helper's own --install.
//
// The password is used for exactly one connection. It is never persisted, never
// logged and never allowed into an error string (see Params.scrubErr).
package nodedeploy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Remote paths. The binary is written to a temporary name and moved into place
// so a helper that is currently running is replaced atomically rather than
// truncated under itself.
const (
	installPath = "/usr/local/bin/proxback-helper"
	tempPath    = installPath + ".tmp"
)

// Timeouts and limits.
const (
	// DialTimeout bounds the TCP connect and the SSH handshake.
	DialTimeout = 15 * time.Second
	// TotalTimeout bounds a whole deployment: connect, upload and install.
	TotalTimeout = 120 * time.Second
	// OutputTailBytes is how much remote output is kept per command.
	OutputTailBytes = 16 << 10
)

// DefaultSSHPort and DefaultHelperPort fill in the optional Params fields.
const (
	DefaultSSHPort    = 22
	DefaultHelperPort = 8007
)

// uploadCommand receives the helper binary on stdin. It is a single sh -c so
// the whole write/chmod/move sequence needs only one session.
const uploadCommand = "sh -c 'cat > " + tempPath +
	" && chmod 0755 " + tempPath +
	" && mv " + tempPath + " " + installPath + "'"

// Params describes one deployment.
type Params struct {
	// Address is the node's SSH host or IP; Port defaults to 22.
	Address string
	Port    int
	// Username and Password authenticate the SSH connection. On Proxmox this is
	// normally root and the node's root password.
	Username string
	Password string
	// ExpectedFingerprint is the SHA256 host key fingerprint the operator has
	// confirmed ("SHA256:base64"). Empty means "not confirmed yet": the
	// handshake is aborted before authentication and a *FingerprintError
	// carrying the node's actual fingerprint is returned.
	ExpectedFingerprint string
	// BinaryPath is the staged linux/amd64 helper binary on the server.
	BinaryPath string
	// ServerURL, EnrollToken and HelperPort are passed to the helper's
	// installer; the token is single-use and minted per deployment.
	ServerURL   string
	EnrollToken string
	HelperPort  int
}

// Result reports what happened, one line per step. Lines are safe to show an
// operator: no password ever reaches them.
type Result struct {
	Log []string
}

// FingerprintError says the node's SSH host key was not confirmed. Nothing was
// authenticated and no command ran; the caller shows Fingerprint to the
// operator and retries with it as Params.ExpectedFingerprint.
type FingerprintError struct {
	Fingerprint string
}

func (e *FingerprintError) Error() string {
	return "nodedeploy: unconfirmed SSH host key fingerprint " + e.Fingerprint
}

// Deploy performs the deployment. It always returns the log lines it managed
// to produce, even alongside an error, so a partial deployment is visible.
func Deploy(ctx context.Context, p Params) (Result, error) {
	var res Result
	if err := p.normalize(); err != nil {
		return res, err
	}
	size, err := binarySize(p.BinaryPath)
	if err != nil {
		return res, err
	}

	ctx, cancel := context.WithTimeout(ctx, TotalTimeout)
	defer cancel()

	client, fingerprint, err := dial(ctx, p)
	if err != nil {
		return res, err
	}
	defer client.Close()
	addr := net.JoinHostPort(p.Address, strconv.Itoa(p.Port))
	res.Log = append(res.Log, fmt.Sprintf("connected to %s (%s)", addr, fingerprint))

	if out, err := upload(client, p.BinaryPath); err != nil {
		return res, p.scrubErr(fmt.Errorf("upload to %s failed: %w%s", installPath, err, suffix(out)))
	}
	res.Log = append(res.Log, fmt.Sprintf("uploaded proxback-helper (%s)", humanBytes(size)))

	out, err := run(client, installerCommand(p))
	if err != nil {
		return res, p.scrubErr(fmt.Errorf("helper installer failed: %w%s", err, suffix(out)))
	}
	res.Log = append(res.Log, "installer: "+orNoOutput(out))
	return res, nil
}

// normalize validates and defaults the parameters.
func (p *Params) normalize() error {
	p.Address = strings.TrimSpace(p.Address)
	p.Username = strings.TrimSpace(p.Username)
	p.ServerURL = strings.TrimSpace(p.ServerURL)
	p.ExpectedFingerprint = strings.TrimSpace(p.ExpectedFingerprint)
	switch {
	case p.Address == "":
		return errors.New("nodedeploy: address is required")
	case p.Username == "":
		return errors.New("nodedeploy: username is required")
	case p.Password == "":
		return errors.New("nodedeploy: password is required")
	case p.BinaryPath == "":
		return errors.New("nodedeploy: binaryPath is required")
	case p.ServerURL == "":
		return errors.New("nodedeploy: serverUrl is required")
	case p.EnrollToken == "":
		return errors.New("nodedeploy: enrollToken is required")
	}
	if p.Port <= 0 {
		p.Port = DefaultSSHPort
	}
	if p.HelperPort <= 0 {
		p.HelperPort = DefaultHelperPort
	}
	return nil
}

// installerCommand is the enrollment + systemd install run on the node. The
// token travels on the remote command line only.
func installerCommand(p Params) string {
	return fmt.Sprintf("%s --server %s --token %s --port %d --install",
		installPath, p.ServerURL, p.EnrollToken, p.HelperPort)
}

// scrubErr keeps the password out of an error even if a library ever puts it
// into a message.
func (p Params) scrubErr(err error) error {
	if err == nil || p.Password == "" {
		return err
	}
	msg := err.Error()
	clean := strings.ReplaceAll(msg, p.Password, "[redacted]")
	if clean == msg {
		return err
	}
	return errors.New(clean)
}

func binarySize(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("nodedeploy: helper binary %s: %w", path, err)
	}
	if st.IsDir() {
		return 0, fmt.Errorf("nodedeploy: helper binary %s is a directory", path)
	}
	return st.Size(), nil
}

// ---------------------------------------------------------------- connection

// hostKeyChecker verifies the node's host key against the fingerprint the
// operator confirmed. It records what it saw so a mismatch can be reported
// back for confirmation, and it never lets an unconfirmed key through — the
// handshake fails before any authentication is attempted.
type hostKeyChecker struct {
	expected string

	mu          sync.Mutex
	fingerprint string
	mismatch    bool
}

// errHostKeyRejected aborts the handshake. It is internal: callers see
// *FingerprintError.
var errHostKeyRejected = errors.New("ssh: host key not confirmed")

func (c *hostKeyChecker) check(_ string, _ net.Addr, key ssh.PublicKey) error {
	fp := ssh.FingerprintSHA256(key)
	ok := c.expected != "" && normalizeFingerprint(c.expected) == fp

	c.mu.Lock()
	c.fingerprint = fp
	c.mismatch = !ok
	c.mu.Unlock()

	if !ok {
		return errHostKeyRejected
	}
	return nil
}

func (c *hostKeyChecker) result() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fingerprint, c.mismatch
}

// normalizeFingerprint accepts the fingerprint with or without the "SHA256:"
// prefix; the base64 body itself is case sensitive and is left alone.
func normalizeFingerprint(fp string) string {
	fp = strings.TrimSpace(fp)
	if fp == "" {
		return ""
	}
	if !strings.HasPrefix(fp, "SHA256:") {
		return "SHA256:" + fp
	}
	return fp
}

// dial connects, verifies the host key and authenticates. It returns the
// verified fingerprint alongside the client.
func dial(ctx context.Context, p Params) (*ssh.Client, string, error) {
	addr := net.JoinHostPort(p.Address, strconv.Itoa(p.Port))
	d := net.Dialer{Timeout: DialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, "", p.scrubErr(fmt.Errorf("nodedeploy: connect to %s: %w", addr, err))
	}
	// The SSH handshake has no context support, so the context is enforced by
	// closing the connection under it.
	stop := closeOnCancel(ctx, conn)

	checker := &hostKeyChecker{expected: p.ExpectedFingerprint}
	cfg := &ssh.ClientConfig{
		User:            p.Username,
		HostKeyCallback: checker.check,
		Timeout:         DialTimeout,
		// Password first; PVE's sshd sometimes offers only
		// keyboard-interactive, which for a password prompt takes the same
		// answer.
		Auth: []ssh.AuthMethod{
			ssh.Password(p.Password),
			ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = p.Password
				}
				return answers, nil
			}),
		},
	}
	_ = conn.SetDeadline(time.Now().Add(DialTimeout))
	sc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		stop()
		if fp, mismatch := checker.result(); mismatch {
			// Nothing was authenticated and nothing ran: the operator has to
			// confirm this fingerprint first.
			return nil, "", &FingerprintError{Fingerprint: fp}
		}
		return nil, "", p.scrubErr(fmt.Errorf("nodedeploy: ssh to %s@%s: %w", p.Username, addr, err))
	}
	// Transfers are short but not instant; the total budget owns them now.
	_ = conn.SetDeadline(time.Time{})

	client := ssh.NewClient(sc, chans, reqs)
	// Once the client is closed the watchdog has nothing left to guard.
	go func() {
		_ = client.Wait()
		stop()
	}()
	fingerprint, _ := checker.result()
	return client, fingerprint, nil
}

// closeOnCancel closes c when ctx is done. Calling the returned func releases
// the watchdog goroutine; it is safe to call more than once.
func closeOnCancel(ctx context.Context, c net.Conn) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = c.Close()
		case <-done:
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// ---------------------------------------------------------------- commands

// upload streams the binary from disk into the remote install command. The
// file is never held in memory.
func upload(client *ssh.Client, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("open ssh session: %w", err)
	}
	defer sess.Close()

	var out tailBuffer
	sess.Stdin = f
	sess.Stdout = &out
	sess.Stderr = &out
	if err := sess.Run(uploadCommand); err != nil {
		return out.String(), err
	}
	return out.String(), nil
}

// run executes one command, returning the tail of its combined output.
func run(client *ssh.Client, cmd string) (string, error) {
	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("open ssh session: %w", err)
	}
	defer sess.Close()

	var out tailBuffer
	sess.Stdout = &out
	sess.Stderr = &out
	if err := sess.Run(cmd); err != nil {
		return out.String(), err
	}
	return out.String(), nil
}

// tailBuffer keeps the last OutputTailBytes bytes written to it. Remote stdout
// and stderr are copied concurrently, hence the mutex.
type tailBuffer struct {
	mu        sync.Mutex
	buf       []byte
	truncated bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if extra := len(t.buf) - OutputTailBytes; extra > 0 {
		t.buf = t.buf[extra:]
		t.truncated = true
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := strings.TrimSpace(string(t.buf))
	if t.truncated && s != "" {
		return "[earlier output truncated] " + s
	}
	return s
}

// suffix renders captured output for an error message.
func suffix(out string) string {
	if out == "" {
		return ""
	}
	return ": " + out
}

func orNoOutput(out string) string {
	if out == "" {
		return "(no output)"
	}
	return out
}

// humanBytes formats a size the way the deployment log shows it, e.g. 15.1 MiB.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := [...]string{"KiB", "MiB", "GiB", "TiB"}
	v, i := float64(n), -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}
