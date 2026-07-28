package agent

// Self-update: the agent replaces its own binary with the build its server
// hands out and asks its service manager to restart it.
//
// Until this existed nothing ever updated an agent. The server updated itself
// and refreshed the binaries it staged, and every already-installed agent went
// on running whatever it was installed with — invisibly, because the version
// was recorded once at registration. A user reached 0.6.2 on the server while a
// protected Windows machine kept running a 0.6.1 agent, kept failing on a bug
// fixed in 0.6.2, and the console showed that agent as "1.0.0".
//
// The rules here are the ones that make this safe to run unattended in someone
// else's guest:
//
//   - the binary comes from the server the agent is enrolled with, addressed by
//     asset name, never by a URL a dispatch supplies;
//   - nothing is swapped until the whole download has arrived and matched the
//     size and checksum the server measured, so a truncated transfer or a proxy
//     login page can never be installed;
//   - the swap is a rename in the destination directory, and the previous
//     binary is moved aside first rather than overwritten, because Windows will
//     not let a running image be written to but will let it be renamed;
//   - a failure at any point leaves the previous binary in place and running,
//     and leaves no temp file behind.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"proxback/internal/agentmgr"
)

// ErrRestartRequired is returned by Run and RunOnce once a self-update has been
// installed. The process must exit for the new binary to run: on Linux the unit
// carries Restart=always and on Windows the service's recovery actions restart
// it after a non-zero exit, so exiting is the restart.
var ErrRestartRequired = errors.New("agent: update installed; restart to run it")

// updateTempPrefix names the staging file. It lives in the destination
// directory so the install is a rename within one filesystem, and it is
// removed on every path — success or failure.
const updateTempPrefix = ".proxback-update-"

// oldBinarySuffix is where the previous binary is moved before the new one
// takes its place. On Windows the running image cannot be deleted until the
// process exits, so one of these may survive until the restart; that is
// expected, and it is also the copy a rollback puts back.
const oldBinarySuffix = ".old"

// binaryPath returns the file a self-update would replace: the configured one,
// or the running executable.
func (a *Agent) binaryPath() (string, error) {
	if p := strings.TrimSpace(a.cfg.BinaryPath); p != "" {
		return p, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("agent: locate the running binary: %w", err)
	}
	if abs, aerr := filepath.Abs(exe); aerr == nil {
		exe = abs
	}
	return exe, nil
}

// restartRequired reports whether an update has been installed and the process
// now has to exit for it to take effect.
func (a *Agent) restartRequired() bool { return a.restart.Load() }

// runSelfUpdate applies an update dispatch.
//
// It never reports a run outcome to the server: there is no run. The server
// learns whether this worked from the version on the next heartbeat, which is
// the only evidence that cannot lie about it.
func (a *Agent) runSelfUpdate(ctx context.Context, d agentmgr.Dispatch) error {
	if strings.TrimSpace(d.Asset) == "" {
		return errors.New("agent: update dispatch names no asset")
	}
	dest, err := a.binaryPath()
	if err != nil {
		return err
	}
	a.log.Info("self-update starting", "version", d.Version, "asset", d.Asset,
		"binary", dest, "bytes", d.SizeBytes)
	if err := a.applyUpdate(ctx, d, dest); err != nil {
		// The previous binary is still in place and still running. Saying so is
		// the point: an operator who pressed "update" must not be left assuming
		// it worked.
		a.log.Error("self-update failed; the agent is still running its previous binary",
			"version", d.Version, "asset", d.Asset, "binary", dest, "error", err)
		return err
	}
	a.log.Info("self-update installed; restarting to run it",
		"version", d.Version, "binary", dest)
	a.restart.Store(true)
	return nil
}

// applyUpdate downloads the asset the dispatch names from this agent's own
// server and installs it over dest.
func (a *Agent) applyUpdate(ctx context.Context, d agentmgr.Dispatch, dest string) error {
	if strings.TrimSpace(a.self.ServerURL) == "" {
		return errors.New("agent: no server address stored; cannot fetch an update")
	}
	url := a.self.ServerURL + "/downloads/" + d.Asset
	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, updateTempPrefix+"*")
	if err != nil {
		return fmt.Errorf("agent: stage the update in %s (is it writable by the service?): %w", dir, err)
	}
	tmpPath := tmp.Name()
	// Removes the staging file on every failure path, and is a harmless no-op
	// once the install below has renamed it away: no temp file survives either.
	defer func() { _ = os.Remove(tmpPath) }()

	if err := a.download(ctx, url, tmp, d); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("agent: finish the download of %s: %w", d.Asset, err)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return fmt.Errorf("agent: mark %s executable: %w", d.Asset, err)
	}
	return installBinary(tmpPath, dest)
}

// download fetches url into dst, verifying as it goes that what arrived is the
// binary the server said it was serving.
func (a *Agent) download(ctx context.Context, url string, dst io.Writer, d agentmgr.Dispatch) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("agent: build the download request: %w", err)
	}
	// /downloads is not authenticated, but the key is sent anyway so an
	// installation that chooses to protect it keeps working.
	req.Header.Set("Authorization", "Bearer "+a.self.APIKey)
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("agent: download %s: %w: %w", url, ErrServerUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("agent: download %s: http %d: %s", url, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
		return fmt.Errorf("agent: download %s answered %s, not a binary: "+
			"something between the guest and the server is serving a web page", url, ct)
	}
	sum := sha256.New()
	sniff := &htmlSniffer{}
	n, err := io.Copy(io.MultiWriter(dst, sum, sniff), resp.Body)
	if err != nil {
		return fmt.Errorf("agent: download %s: %w", url, err)
	}
	return verifyDownload(d, n, sum.Sum(nil), sniff.looksLikeHTML())
}

// verifyDownload decides whether what arrived may be installed. It is separate
// from the transfer so every rejection reads the same way whatever produced it.
func verifyDownload(d agentmgr.Dispatch, got int64, sum []byte, html bool) error {
	switch {
	case got == 0:
		return errors.New("agent: the update download was empty")
	case html:
		return errors.New("agent: the update download is an HTML page, not a binary: " +
			"a proxy or portal is answering in place of the ProxBack server")
	case d.SizeBytes > 0 && got != d.SizeBytes:
		return fmt.Errorf("agent: the update download is %d bytes but the server said %d — "+
			"it arrived truncated or altered", got, d.SizeBytes)
	}
	if want := strings.ToLower(strings.TrimSpace(d.Sha256)); want != "" {
		if have := hex.EncodeToString(sum); have != want {
			return fmt.Errorf("agent: checksum mismatch for %s: got %s, want %s", d.Asset, have, want)
		}
	}
	return nil
}

// installBinary puts the staged file at dest, keeping the previous binary until
// the new one is in place.
//
// The move-aside is not merely tidy: on Windows a running executable cannot be
// overwritten, but it can be renamed, which is exactly how the server's own
// updater replaces itself. If the second rename fails the first is undone, so a
// failed update always leaves a working binary where the service expects one.
func installBinary(staged, dest string) error {
	old := dest + oldBinarySuffix
	_ = os.Remove(old)
	if err := os.Rename(dest, old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("agent: move the current binary aside: %w", err)
	}
	if err := os.Rename(staged, dest); err != nil {
		if rberr := os.Rename(old, dest); rberr != nil {
			return fmt.Errorf("agent: install %s: %w (and the previous binary could not be "+
				"put back: %v — restore it from %s)", dest, err, rberr, old)
		}
		return fmt.Errorf("agent: install %s: %w (the previous binary is back in place)", dest, err)
	}
	// Best effort: Windows holds the running image open until the process
	// exits, so this fails there and the file is cleaned up by the next update.
	_ = os.Remove(old)
	return nil
}

// htmlSniffer watches the first bytes of a download for the tell-tale start of
// a web page. A captive portal or a reverse proxy error page is served with a
// 200 and any content type it likes; the body is the honest signal.
type htmlSniffer struct {
	head []byte
	done bool
}

func (h *htmlSniffer) Write(p []byte) (int, error) {
	if !h.done {
		h.head = append(h.head, p...)
		if len(h.head) >= 64 {
			h.head = h.head[:64]
			h.done = true
		}
	}
	return len(p), nil
}

func (h *htmlSniffer) looksLikeHTML() bool {
	head := strings.ToLower(strings.TrimSpace(string(h.head)))
	return strings.HasPrefix(head, "<!doctype html") ||
		strings.HasPrefix(head, "<html") ||
		strings.HasPrefix(head, "<?xml") && strings.Contains(head, "html")
}
