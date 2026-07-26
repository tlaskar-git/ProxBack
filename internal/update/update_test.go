package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		current, candidate string
		want               bool
	}{
		{"0.1.0", "0.2.0", true},
		{"0.2.0", "0.2.0", false},
		{"0.2.0", "0.1.9", false},
		{"0.2.0", "v0.3.0", true},
		{"1.9.0", "1.10.0", true},
		{"0.2.0", "1.0.0", true},
		{"0.2.0", "0.2.1", true},
		{"0.2.0", "garbage", false},
		{"dev", "0.0.1", true}, // unparseable current: any release is newer
		{"0.2.0", "0.2.1-rc.1", true},
	}
	for _, c := range cases {
		if got := Newer(c.current, c.candidate); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.current, c.candidate, got, c.want)
		}
	}
}

func TestServerAssetName(t *testing.T) {
	if got := ServerAssetName("linux", "amd64"); got != "proxback-server-linux-amd64" {
		t.Fatalf("linux asset name = %q", got)
	}
	if got := ServerAssetName("windows", "amd64"); got != "proxback-server-windows-amd64.exe" {
		t.Fatalf("windows asset name = %q", got)
	}
}

// fakeGitHub serves a latest-release document plus asset downloads.
func fakeGitHub(t *testing.T, tag string, binary []byte, withChecksums, corruptChecksum bool) *httptest.Server {
	t.Helper()
	assetName := ServerAssetName(runtime.GOOS, runtime.GOARCH)
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/repos/tester/proxback/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		assets := fmt.Sprintf(`{"name":%q,"size":%d,"browser_download_url":"%s/dl/%s"}`,
			assetName, len(binary), srv.URL, assetName)
		if withChecksums {
			assets += fmt.Sprintf(`,{"name":"checksums.txt","size":1,"browser_download_url":"%s/dl/checksums.txt"}`, srv.URL)
		}
		fmt.Fprintf(w, `{"tag_name":%q,"name":"ProxBack %s","body":"notes","html_url":"http://x","published_at":"2026-07-26T12:00:00Z","assets":[%s]}`,
			tag, tag, assets)
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case assetName:
			_, _ = w.Write(binary)
		case "checksums.txt":
			sum := sha256.Sum256(binary)
			hexSum := hex.EncodeToString(sum[:])
			if corruptChecksum {
				hexSum = "deadbeef" + hexSum[8:]
			}
			fmt.Fprintf(w, "%s  %s\n", hexSum, assetName)
		default:
			http.NotFound(w, r)
		}
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newTestChecker(api string) *Checker {
	c := New(nil)
	c.Repo = "tester/proxback"
	c.APIBase = api
	return c
}

func TestLatestAndApply(t *testing.T) {
	ctx := context.Background()
	newBinary := []byte("#!new-binary-v2")
	srv := fakeGitHub(t, "v9.9.9", newBinary, true, false)
	c := newTestChecker(srv.URL)

	rel, err := c.Latest(ctx)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rel.Version() != "9.9.9" || !Newer("0.2.0", rel.Version()) {
		t.Fatalf("release = %+v", rel)
	}
	asset, err := rel.AssetFor(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatalf("AssetFor: %v", err)
	}

	dir := t.TempDir()
	binPath := filepath.Join(dir, "proxback-server")
	if err := os.WriteFile(binPath, []byte("old-binary-v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.Apply(ctx, rel, asset, binPath); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := os.ReadFile(binPath)
	if err != nil || string(got) != string(newBinary) {
		t.Fatalf("binary after apply = %q, %v", got, err)
	}
	old, err := os.ReadFile(binPath + ".old")
	if err != nil || string(old) != "old-binary-v1" {
		t.Fatalf("old binary = %q, %v", old, err)
	}
}

func TestApplyRejectsBadChecksum(t *testing.T) {
	ctx := context.Background()
	srv := fakeGitHub(t, "v9.9.9", []byte("evil"), true, true)
	c := newTestChecker(srv.URL)

	rel, err := c.Latest(ctx)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	asset, _ := rel.AssetFor(runtime.GOOS, runtime.GOARCH)
	dir := t.TempDir()
	binPath := filepath.Join(dir, "proxback-server")
	if err := os.WriteFile(binPath, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.Apply(ctx, rel, asset, binPath); err == nil {
		t.Fatal("Apply accepted a corrupted download")
	}
	if got, _ := os.ReadFile(binPath); string(got) != "old" {
		t.Fatalf("binary was replaced despite checksum failure: %q", got)
	}
}

func TestLatestNoReleases(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	c := newTestChecker(srv.URL)
	if _, err := c.Latest(context.Background()); !errors.Is(err, ErrNoReleases) {
		t.Fatalf("err = %v, want ErrNoReleases", err)
	}
}
