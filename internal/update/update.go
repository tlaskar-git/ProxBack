// Package update checks GitHub releases for newer ProxBack versions and swaps
// the running binary in place. The caller decides when to restart.
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultRepo is the GitHub repository releases are pulled from. Override with
// the PROXBACK_UPDATE_REPO environment variable (owner/name).
const DefaultRepo = "tlaskar-git/ProxBack"

// ErrNoReleases is returned when the repository has no published releases yet.
var ErrNoReleases = errors.New("update: no releases published yet")

// ErrNoAsset is returned when the latest release has no binary for this platform.
var ErrNoAsset = errors.New("update: release has no asset for this platform")

// Asset is one downloadable file attached to a release.
type Asset struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	DownloadURL string `json:"browser_download_url"`
}

// Release describes a published GitHub release.
type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []Asset   `json:"assets"`
}

// Version returns the release version without a leading "v".
func (r *Release) Version() string { return strings.TrimPrefix(r.TagName, "v") }

// Checker queries GitHub for the latest release.
type Checker struct {
	Repo    string // owner/name
	APIBase string // https://api.github.com, overridable for tests
	hc      *http.Client
	log     *slog.Logger
}

// New builds a checker for the configured repository.
func New(log *slog.Logger) *Checker {
	repo := os.Getenv("PROXBACK_UPDATE_REPO")
	if repo == "" {
		repo = DefaultRepo
	}
	api := os.Getenv("PROXBACK_UPDATE_API")
	if api == "" {
		api = "https://api.github.com"
	}
	if log == nil {
		log = slog.Default()
	}
	return &Checker{Repo: repo, APIBase: api, hc: &http.Client{Timeout: 30 * time.Second}, log: log}
}

// Latest fetches the newest published release.
func (c *Checker) Latest(ctx context.Context) (*Release, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", c.APIBase, c.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("update: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "proxback-updater")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("update: query releases: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoReleases
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("update: GitHub responded %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var rel Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("update: decode release: %w", err)
	}
	return &rel, nil
}

// ServerAssetName is the release asset filename for a server build on the
// given platform.
func ServerAssetName(goos, goarch string) string {
	name := fmt.Sprintf("proxback-server-%s-%s", goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// AssetFor picks the server binary asset for this platform.
func (r *Release) AssetFor(goos, goarch string) (Asset, error) {
	want := ServerAssetName(goos, goarch)
	for _, a := range r.Assets {
		if a.Name == want {
			return a, nil
		}
	}
	return Asset{}, fmt.Errorf("%w (looked for %s)", ErrNoAsset, want)
}

// checksumFor extracts the sha256 for name from a checksums.txt body
// ("<hex>  <name>" per line). Empty when absent.
func checksumFor(checksums, name string) string {
	for _, line := range strings.Split(checksums, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && (fields[1] == name || fields[1] == "*"+name) {
			return strings.ToLower(fields[0])
		}
	}
	return ""
}

// Newer reports whether candidate is a strictly newer semantic version than
// current. Non-numeric parts fall back to string comparison; a malformed
// candidate is never newer.
func Newer(current, candidate string) bool {
	cur, okCur := parseSemver(current)
	cand, okCand := parseSemver(candidate)
	if !okCand {
		return false
	}
	if !okCur {
		return true
	}
	for i := 0; i < 3; i++ {
		if cand[i] != cur[i] {
			return cand[i] > cur[i]
		}
	}
	return false
}

func parseSemver(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) < 1 || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// Apply downloads the asset, verifies it against the release's checksums.txt
// when one is published, and atomically swaps it over binPath. The previous
// binary is kept as binPath+".old" (required on Windows, useful everywhere).
// The process must be restarted for the new binary to run.
func (c *Checker) Apply(ctx context.Context, rel *Release, asset Asset, binPath string) error {
	dir := filepath.Dir(binPath)
	tmp, err := os.CreateTemp(dir, ".proxback-update-*")
	if err != nil {
		return fmt.Errorf("update: stage file (is %s writable by the service?): %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	sum := sha256.New()
	if err := c.download(ctx, asset.DownloadURL, io.MultiWriter(tmp, sum)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("update: finish download: %w", err)
	}

	// Verify against checksums.txt when the release ships one.
	for _, a := range rel.Assets {
		if a.Name != "checksums.txt" {
			continue
		}
		var buf strings.Builder
		if err := c.download(ctx, a.DownloadURL, &buf); err != nil {
			return fmt.Errorf("update: fetch checksums: %w", err)
		}
		want := checksumFor(buf.String(), asset.Name)
		if want == "" {
			c.log.Warn("release checksums.txt has no entry for asset", "asset", asset.Name)
			break
		}
		got := hex.EncodeToString(sum.Sum(nil))
		if got != want {
			return fmt.Errorf("update: checksum mismatch for %s: got %s want %s", asset.Name, got, want)
		}
		c.log.Info("update checksum verified", "asset", asset.Name)
		break
	}

	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return fmt.Errorf("update: mark executable: %w", err)
	}
	old := binPath + ".old"
	_ = os.Remove(old)
	if err := os.Rename(binPath, old); err != nil {
		return fmt.Errorf("update: move current binary aside: %w", err)
	}
	if err := os.Rename(tmpPath, binPath); err != nil {
		// Try to roll back so the installation is not left without a binary.
		_ = os.Rename(old, binPath)
		return fmt.Errorf("update: install new binary: %w", err)
	}
	c.log.Info("update applied", "version", rel.Version(), "binary", binPath)
	return nil
}

func (c *Checker) download(ctx context.Context, url string, dst io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("update: build download request: %w", err)
	}
	req.Header.Set("User-Agent", "proxback-updater")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("update: download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("update: download %s: HTTP %d", url, resp.StatusCode)
	}
	if _, err := io.Copy(dst, resp.Body); err != nil {
		return fmt.Errorf("update: download %s: %w", url, err)
	}
	return nil
}

// CurrentPlatform returns the goos/goarch of the running server.
func CurrentPlatform() (string, string) { return runtime.GOOS, runtime.GOARCH }
