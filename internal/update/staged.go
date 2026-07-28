package update

// Staged binaries: the in-guest agent and the Proxmox node helper.
//
// The server hands these two binaries out from <dataDir>/downloads — the
// console's "Deploy Agent" and "Deploy helper" flows serve them to guests and
// upload them to nodes. Until this file existed they were written exactly once,
// by the installer, and nothing ever refreshed them: a server that upgraded
// itself from 0.2.x to 0.6.0 kept handing out the agent its installer had left
// behind. That is how a current server came to install an agent that predated
// Windows-service support, failing every Windows install with SCM error 1053
// while the console reported a healthy, up-to-date server.
//
// So: applying an update refreshes them (see Checker.RefreshStaged), startup
// heals an installation that already drifted (Checker.ReconcileStaged), and
// InspectStaged makes the state of the directory reportable instead of silent.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// StagedDirName is the subdirectory of the data dir the agent and node helper
// binaries are served from.
const StagedDirName = "downloads"

// versionSidecarSuffix names the file that records which release a staged
// binary came from: "proxback-agent-windows-amd64.exe.version".
//
// Recording the version on the way in is the only honest way to know it. The
// alternative — executing the binary with --version to ask it — is not
// available: half the staged binaries are for another platform (a Linux server
// cannot run proxback-agent-windows-amd64.exe), and executing a freshly
// downloaded artifact just to inspect it is a poor pattern regardless.
//
// A sidecar per binary is preferred over one manifest for the whole directory
// because each binary is staged independently: the sidecar is written with the
// same temp-file-plus-rename dance immediately after its binary lands, so there
// is no shared file to serialise on, no read-modify-write race between two
// refreshes, and a half-refreshed directory still describes itself correctly.
// An absent sidecar means "unknown" — never a guess.
const versionSidecarSuffix = ".version"

// ErrVersionUnknown is returned by StagedVersion when no version was recorded
// alongside a staged binary — an installation that predates version recording,
// or a file copied in by hand.
var ErrVersionUnknown = errors.New("update: no version recorded for this staged binary")

// StagedArtifact is one binary the server stages for deployment.
type StagedArtifact struct {
	// Name is both the file name in <dataDir>/downloads and the release asset
	// name. They are deliberately identical: one name, no mapping to get wrong.
	Name string
	// Kind is "agent" or "node helper", for log lines and operator-facing text.
	Kind string
	// GOOS and GOARCH describe the platform the binary is for.
	GOOS, GOARCH string
}

// StagedArtifacts lists the binaries the server hands out. It matches the
// allow-list the /downloads endpoint serves and the assets deploy/install.sh
// stages, because those are the same three files.
func StagedArtifacts() []StagedArtifact {
	return []StagedArtifact{
		{Name: "proxback-agent-linux-amd64", Kind: "agent", GOOS: "linux", GOARCH: "amd64"},
		{Name: "proxback-agent-windows-amd64.exe", Kind: "agent", GOOS: "windows", GOARCH: "amd64"},
		{Name: "proxback-helper-linux-amd64", Kind: "node helper", GOOS: "linux", GOARCH: "amd64"},
	}
}

// StagedDir returns <dataDir>/downloads.
func StagedDir(dataDir string) string { return filepath.Join(dataDir, StagedDirName) }

// StagedPath returns the full path of a staged binary.
func StagedPath(dataDir, name string) string { return filepath.Join(StagedDir(dataDir), name) }

// StagedVersionPath returns the full path of a staged binary's version sidecar.
func StagedVersionPath(dataDir, name string) string {
	return StagedPath(dataDir, name) + versionSidecarSuffix
}

// StagedVersion returns the version recorded alongside a staged binary, or
// ErrVersionUnknown when none was recorded.
func StagedVersion(dataDir, name string) (string, error) {
	raw, err := os.ReadFile(StagedVersionPath(dataDir, name))
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrVersionUnknown
	}
	if err != nil {
		return "", fmt.Errorf("update: read recorded version for %s: %w", name, err)
	}
	v := strings.TrimSpace(string(raw))
	if v == "" {
		return "", ErrVersionUnknown
	}
	return v, nil
}

// StagedStatus is the on-disk state of one staged binary.
type StagedStatus struct {
	StagedArtifact
	// Present reports whether the binary itself is on disk.
	Present bool
	// Version is the recorded release version, empty when it is not known.
	Version string
	// Reason explains an empty Version in words an operator can act on.
	Reason  string
	Size    int64
	ModTime time.Time
}

// InspectStaged reports the state of every staged binary in <dataDir>/downloads.
// It never touches the network and never executes anything.
func InspectStaged(dataDir string) []StagedStatus {
	arts := StagedArtifacts()
	out := make([]StagedStatus, 0, len(arts))
	for _, art := range arts {
		st := StagedStatus{StagedArtifact: art}
		info, err := os.Stat(StagedPath(dataDir, art.Name))
		switch {
		case err != nil || info.IsDir():
			st.Reason = "not staged on this server — the " + art.Kind +
				" cannot be deployed until this binary is present in <data>/" + StagedDirName
			out = append(out, st)
			continue
		default:
			st.Present = true
			st.Size = info.Size()
			st.ModTime = info.ModTime().UTC()
		}
		ver, verr := StagedVersion(dataDir, art.Name)
		switch {
		case errors.Is(verr, ErrVersionUnknown):
			st.Reason = "no version was recorded when this binary was staged, so what it " +
				"contains cannot be determined — it predates version recording or was copied in by hand"
		case verr != nil:
			st.Reason = "the recorded version could not be read: " + verr.Error()
		default:
			st.Version = ver
		}
		out = append(out, st)
	}
	return out
}

// StagedFailure is one binary that could not be refreshed, with the reason.
type StagedFailure struct {
	Name string
	Err  error
}

// StagedRefresh reports what one refresh pass did.
type StagedRefresh struct {
	// Version is the release the binaries were taken from.
	Version string
	// Updated names the binaries that were rewritten.
	Updated []string
	// Skipped names the binaries the release does not publish. They keep
	// whatever was staged before.
	Skipped []string
	// Failed names the binaries whose download or verification failed. They too
	// keep whatever was staged before.
	Failed []StagedFailure
	// UpToDate names the binaries a reconciliation left alone because their
	// recorded version already matched.
	UpToDate []string
}

// Changed reports whether the pass rewrote anything.
func (r StagedRefresh) Changed() bool { return len(r.Updated) > 0 }

// Summary is a one-line, operator-readable account of the pass.
func (r StagedRefresh) Summary() string {
	parts := []string{fmt.Sprintf("%d refreshed", len(r.Updated))}
	if len(r.UpToDate) > 0 {
		parts = append(parts, fmt.Sprintf("%d already current", len(r.UpToDate)))
	}
	if len(r.Skipped) > 0 {
		parts = append(parts, fmt.Sprintf("%d not published by the release (%s)",
			len(r.Skipped), strings.Join(r.Skipped, ", ")))
	}
	for _, f := range r.Failed {
		parts = append(parts, f.Name+" failed: "+f.Err.Error())
	}
	return strings.Join(parts, ", ")
}

// RefreshStaged downloads every staged binary published by rel into
// <dataDir>/downloads, verifying each against the release's checksums.txt
// exactly as the server binary is, writing each atomically, and recording the
// version alongside it.
//
// It is deliberately forgiving. An asset the release does not publish for one
// platform, or one corrupted download, leaves the previously staged copy in
// place and does not fail the pass: refreshing what the console hands out
// matters, but never as much as the server upgrade that triggered it.
func (c *Checker) RefreshStaged(ctx context.Context, rel *Release, dataDir string) (StagedRefresh, error) {
	return c.refreshStaged(ctx, rel, dataDir, nil)
}

// ReconcileStaged heals an installation whose staged binaries drifted from the
// server that serves them: it compares each recorded version against want (the
// running server's own version) and refreshes only those that are missing or
// mismatched.
//
// When everything already matches it returns without touching the network at
// all, so the common case of a healthy installation costs nothing. It is best
// effort by design — an installation without internet access gets an error it
// can log, and keeps the binaries it has.
func (c *Checker) ReconcileStaged(ctx context.Context, dataDir, want string) (StagedRefresh, error) {
	out := StagedRefresh{Version: want}
	stale := map[string]bool{}
	for _, st := range InspectStaged(dataDir) {
		if st.Present && st.Version == want {
			out.UpToDate = append(out.UpToDate, st.Name)
			continue
		}
		stale[st.Name] = true
	}
	if len(stale) == 0 {
		return out, nil
	}
	// The staged agent must match the server that hands it out, which is not
	// necessarily the newest release — so this asks for the server's own tag
	// rather than "latest".
	rel, err := c.ByTag(ctx, want)
	if err != nil {
		return out, err
	}
	res, err := c.refreshStaged(ctx, rel, dataDir, stale)
	if err != nil {
		return out, err
	}
	res.UpToDate = out.UpToDate
	return res, nil
}

// refreshStaged does the work of RefreshStaged, restricted to the names in only
// when only is non-nil.
func (c *Checker) refreshStaged(ctx context.Context, rel *Release, dataDir string, only map[string]bool) (StagedRefresh, error) {
	out := StagedRefresh{Version: rel.Version()}
	dir := StagedDir(dataDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return out, fmt.Errorf("update: create %s: %w", dir, err)
	}
	checksums, err := c.releaseChecksums(ctx, rel)
	if err != nil {
		return out, err
	}
	assets := make(map[string]Asset, len(rel.Assets))
	for _, a := range rel.Assets {
		assets[a.Name] = a
	}
	for _, art := range StagedArtifacts() {
		if only != nil && !only[art.Name] {
			continue
		}
		asset, ok := assets[art.Name]
		if !ok {
			out.Skipped = append(out.Skipped, art.Name)
			c.log.Warn("release does not publish this binary; leaving the staged copy in place",
				"asset", art.Name, "kind", art.Kind, "version", rel.Version())
			continue
		}
		if err := c.stageAsset(ctx, asset, dir, checksums, rel.Version()); err != nil {
			out.Failed = append(out.Failed, StagedFailure{Name: art.Name, Err: err})
			c.log.Error("could not refresh staged binary; the previously staged copy is untouched",
				"asset", art.Name, "kind", art.Kind, "error", err)
			continue
		}
		out.Updated = append(out.Updated, art.Name)
		c.log.Info("staged binary refreshed", "asset", art.Name, "kind", art.Kind, "version", rel.Version())
	}
	return out, nil
}

// releaseChecksums fetches the release's checksums.txt, or returns "" when the
// release does not publish one — the same policy Apply uses for the server
// binary, so the two cannot disagree about what is verified.
func (c *Checker) releaseChecksums(ctx context.Context, rel *Release) (string, error) {
	for _, a := range rel.Assets {
		if a.Name != "checksums.txt" {
			continue
		}
		var buf strings.Builder
		if err := c.download(ctx, a.DownloadURL, &buf); err != nil {
			return "", fmt.Errorf("update: fetch checksums: %w", err)
		}
		return buf.String(), nil
	}
	c.log.Warn("release publishes no checksums.txt; staged binaries cannot be verified",
		"version", rel.Version())
	return "", nil
}

// stageAsset downloads one asset into dir under its own name and records ver
// beside it. Nothing is written under the real name until the whole download
// has arrived and verified, so a failure can never leave a truncated binary
// that a guest would go on to install.
func (c *Checker) stageAsset(ctx context.Context, asset Asset, dir, checksums, ver string) error {
	tmp, err := os.CreateTemp(dir, ".proxback-staged-*")
	if err != nil {
		return fmt.Errorf("update: stage %s (is %s writable by the service?): %w", asset.Name, dir, err)
	}
	tmpPath := tmp.Name()
	// Removes the temp file on every failure path, and is a harmless no-op once
	// the rename below has moved it away: no stray temp file survives either way.
	defer func() { _ = os.Remove(tmpPath) }()

	sum := sha256.New()
	if err := c.download(ctx, asset.DownloadURL, io.MultiWriter(tmp, sum)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("update: finish download of %s: %w", asset.Name, err)
	}
	want := checksumFor(checksums, asset.Name)
	switch {
	case checksums == "":
		// releaseChecksums has already warned that the release publishes none.
	case want == "":
		c.log.Warn("release checksums.txt has no entry for asset", "asset", asset.Name)
	default:
		if got := hex.EncodeToString(sum.Sum(nil)); got != want {
			return fmt.Errorf("update: checksum mismatch for %s: got %s want %s", asset.Name, got, want)
		}
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return fmt.Errorf("update: mark %s executable: %w", asset.Name, err)
	}
	if err := os.Rename(tmpPath, filepath.Join(dir, asset.Name)); err != nil {
		return fmt.Errorf("update: install staged %s: %w", asset.Name, err)
	}
	// The sidecar is written only once the binary is in place, so a reader never
	// sees a version that does not describe the file next to it.
	if err := writeFileAtomic(filepath.Join(dir, asset.Name+versionSidecarSuffix),
		[]byte(ver+"\n"), 0o644); err != nil {
		return fmt.Errorf("update: record version of %s: %w", asset.Name, err)
	}
	return nil
}

// writeFileAtomic writes data through a temp file in the same directory and a
// rename, so a reader never observes a partial file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".proxback-staged-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
