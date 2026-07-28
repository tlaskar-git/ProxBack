package helper

// Self-update: the node helper replaces its own binary with the build its
// server hands out and lets systemd restart it.
//
// A node helper is installed once, over SSH, and then left alone for years.
// Nothing ever updated it, and — because the version was recorded at
// registration and never looked at again — nothing ever showed that it had
// fallen behind the server either. This is the other half of the fix that gave
// agents the same ability; the difference is only in how the work arrives. An
// agent polls; a helper is told, over the authenticated HTTP API the server
// already uses for export and import.
//
// It deliberately mirrors internal/agent/selfupdate.go step for step —
// download from the enrolled server by asset name, verify size and checksum
// before anything is swapped, move the old binary aside rather than overwrite
// it, restore it if the install fails, leave no temp file — rather than sharing
// code with it. The agent and the helper are two standalone binaries with no
// package in common beyond the version stamp, exactly as their installers,
// service units and copyExecutable already are.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"proxback/internal/helpermgr"
)

// ErrRestartRequired is returned by Run once a self-update has been installed.
// The process must exit for the new binary to run; the unit carries
// Restart=on-failure, so a non-zero exit is what brings the new build up.
var ErrRestartRequired = errors.New("helper: update installed; restart to run it")

// updateTempPrefix names the staging file, created in the destination directory
// so the install is a rename within one filesystem and removed on every path.
const updateTempPrefix = ".proxback-update-"

// oldBinarySuffix is where the previous binary is moved before the new one
// takes its place, and where a failed install puts it back from.
const oldBinarySuffix = ".old"

// updateDownloadTimeout bounds fetching the new binary. It is far longer than
// the helper's ordinary client timeout because this is a multi-megabyte
// transfer over whatever link the node has, not an API call.
const updateDownloadTimeout = 15 * time.Minute

// binaryPath returns the file a self-update would replace: the configured one,
// or the running executable.
func (h *Helper) binaryPath() (string, error) {
	if p := strings.TrimSpace(h.cfg.BinaryPath); p != "" {
		return p, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("helper: locate the running binary: %w", err)
	}
	if abs, aerr := filepath.Abs(exe); aerr == nil {
		exe = abs
	}
	return exe, nil
}

// busy reports whether the helper has node work in flight — an export, an
// import or a policy script. It is what makes the 409 below honest rather than
// hopeful.
func (h *Helper) busy() bool { return h.inflight.Load() > 0 }

// startWork marks a node command as in flight and returns the matching finish.
func (h *Helper) startWork() func() {
	h.inflight.Add(1)
	return func() { h.inflight.Add(-1) }
}

// handleUpdate replaces the helper's own binary with the build the server is
// serving and schedules the restart.
//
// It answers only once the new binary is installed, so a download that fails
// verification is reported to the operator as a failure there and then instead
// of as a silence. It still is not proof that the update took: that is the
// version on the next heartbeat, and nothing here claims otherwise.
func (h *Helper) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !h.authorized(r) {
		writeErr(w, http.StatusUnauthorized, "invalid access secret")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read request: "+err.Error())
		return
	}
	var req helpermgr.UpdateRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "decode request: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Asset) == "" {
		writeErr(w, http.StatusBadRequest, "asset is required")
		return
	}
	// Never restart on top of a running export, import or policy script: the
	// server would see a truncated stream and fail a backup that was working.
	if h.busy() {
		writeErr(w, http.StatusConflict,
			"this node helper has an export, import or policy script in flight; "+
				"updating it would restart it and abort that work — try again when the run has finished")
		return
	}
	dest, err := h.binaryPath()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.log.Info("self-update starting", "version", req.Version, "asset", req.Asset,
		"binary", dest, "bytes", req.SizeBytes)
	if err := h.applyUpdate(r.Context(), req, dest); err != nil {
		h.log.Error("self-update failed; the node helper is still running its previous binary",
			"version", req.Version, "asset", req.Asset, "binary", dest, "error", err)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	h.log.Info("self-update installed; restarting to run it", "version", req.Version, "binary", dest)
	// Signalling before the response is written is safe and deliberate: Run
	// shuts the HTTP server down gracefully, which waits for this handler to
	// return, so the answer still reaches the server that asked for it.
	h.requestRestart()
	writeJSON(w, http.StatusOK, helpermgr.UpdateResponse{
		OK: true, Version: req.Version, Restarting: true,
	})
}

// requestRestart asks the run loop to exit so the new binary is started. Once
// is enough, and a second update must not close the channel twice.
func (h *Helper) requestRestart() {
	h.restartOnce.Do(func() { close(h.restartc) })
}

// applyUpdate downloads the named asset from this helper's own server and
// installs it over dest.
func (h *Helper) applyUpdate(ctx context.Context, req helpermgr.UpdateRequest, dest string) error {
	self := h.snapshot()
	if strings.TrimSpace(self.ServerURL) == "" {
		return errors.New("helper: no server address stored; cannot fetch an update")
	}
	// The URL is built from the stored server address and the asset name, never
	// from anything in the request: a node must only ever run a binary that came
	// from the server it is enrolled with.
	url := self.ServerURL + "/downloads/" + req.Asset
	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, updateTempPrefix+"*")
	if err != nil {
		return fmt.Errorf("helper: stage the update in %s (is it writable?): %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if err := h.download(ctx, url, tmp, req); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("helper: finish the download of %s: %w", req.Asset, err)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return fmt.Errorf("helper: mark %s executable: %w", req.Asset, err)
	}
	return installBinary(tmpPath, dest)
}

// download fetches url into dst and verifies that what arrived is the binary
// the server said it was serving.
func (h *Helper) download(ctx context.Context, url string, dst io.Writer, req helpermgr.UpdateRequest) error {
	ctx, cancel := context.WithTimeout(ctx, updateDownloadTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("helper: build the download request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+h.snapshot().APIKey)
	// The helper's ordinary client is bounded at a minute, which is right for an
	// API call and wrong for a binary; the transport (and so the operator's
	// --insecure choice) is kept.
	dl := &http.Client{Transport: h.hc.Transport, Timeout: updateDownloadTimeout}
	resp, err := dl.Do(httpReq)
	if err != nil {
		return fmt.Errorf("helper: download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("helper: download %s: http %d: %s", url, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
		return fmt.Errorf("helper: download %s answered %s, not a binary", url, ct)
	}
	sum := sha256.New()
	sniff := &htmlSniffer{}
	n, err := io.Copy(io.MultiWriter(dst, sum, sniff), resp.Body)
	if err != nil {
		return fmt.Errorf("helper: download %s: %w", url, err)
	}
	return verifyDownload(req, n, sum.Sum(nil), sniff.looksLikeHTML())
}

// verifyDownload decides whether what arrived may be installed.
func verifyDownload(req helpermgr.UpdateRequest, got int64, sum []byte, html bool) error {
	switch {
	case got == 0:
		return errors.New("helper: the update download was empty")
	case html:
		return errors.New("helper: the update download is an HTML page, not a binary: " +
			"a proxy is answering in place of the ProxBack server")
	case req.SizeBytes > 0 && got != req.SizeBytes:
		return fmt.Errorf("helper: the update download is %d bytes but the server said %d — "+
			"it arrived truncated or altered", got, req.SizeBytes)
	}
	if want := strings.ToLower(strings.TrimSpace(req.Sha256)); want != "" {
		if have := hex.EncodeToString(sum); have != want {
			return fmt.Errorf("helper: checksum mismatch for %s: got %s, want %s", req.Asset, have, want)
		}
	}
	return nil
}

// installBinary puts the staged file at dest, keeping the previous binary until
// the new one is in place and putting it back if the install fails. Renaming
// the running image aside rather than writing over it is what makes this work
// on every platform, and it is what the server's own updater does.
func installBinary(staged, dest string) error {
	old := dest + oldBinarySuffix
	_ = os.Remove(old)
	if err := os.Rename(dest, old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("helper: move the current binary aside: %w", err)
	}
	if err := os.Rename(staged, dest); err != nil {
		if rberr := os.Rename(old, dest); rberr != nil {
			return fmt.Errorf("helper: install %s: %w (and the previous binary could not be "+
				"put back: %v — restore it from %s)", dest, err, rberr, old)
		}
		return fmt.Errorf("helper: install %s: %w (the previous binary is back in place)", dest, err)
	}
	_ = os.Remove(old)
	return nil
}

// htmlSniffer watches the first bytes of a download for the start of a web
// page, which a captive portal or a proxy error page will happily serve with a
// 200 and any content type it likes.
type htmlSniffer struct {
	head []byte
	done bool
}

func (s *htmlSniffer) Write(p []byte) (int, error) {
	if !s.done {
		s.head = append(s.head, p...)
		if len(s.head) >= 64 {
			s.head = s.head[:64]
			s.done = true
		}
	}
	return len(p), nil
}

func (s *htmlSniffer) looksLikeHTML() bool {
	head := strings.ToLower(strings.TrimSpace(string(s.head)))
	return strings.HasPrefix(head, "<!doctype html") || strings.HasPrefix(head, "<html")
}
