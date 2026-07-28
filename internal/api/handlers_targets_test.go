package api

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"proxback/internal/blobstore"
	"proxback/internal/store"
)

// fsTargetBody is a valid filesystem target creation body. The test server's data
// directory is a temp directory, so on any platform that can compare filesystems
// a temp-directory target shares one with it — which the API refuses unless the
// operator says otherwise. Tests that want a *working* target therefore say so,
// exactly as a single-disk homelab operator would.
func fsTargetBody(name, path string) map[string]any {
	return map[string]any{"name": name, "kind": "filesystem", "path": path, "allowSameFilesystem": true}
}

// sameFilesystemDetectable reports whether this platform can tell that two paths
// share a filesystem. Where it cannot, the refusal cannot be provoked, and the
// test says so rather than skipping in silence.
func sameFilesystemDetectable(t *testing.T, a, b string) bool {
	t.Helper()
	d, err := blobstore.Check(blobstore.CheckRequest{Path: a, DataDir: b, AllowSameFilesystem: true})
	if err != nil {
		t.Fatalf("check %s: %v", a, err)
	}
	for _, w := range d.Warnings {
		if w.Code == blobstore.WarnSameFilesystemUnknown || w.Code == blobstore.WarnMountPointUnknown {
			t.Logf("platform %s cannot compare filesystems (%s: %s): asserting the unknown path",
				runtime.GOOS, w.Code, w.Detail)
			return false
		}
	}
	return d.SameFilesystemAsDataDir
}

// TestCreateTargetRejectsAMixedShape is the validation matrix. A target is either
// object storage or a path; a request that is both is a mistake worth naming,
// because silently ignoring half of it would send backups somewhere the operator
// did not choose.
func TestCreateTargetRejectsAMixedShape(t *testing.T) {
	ts := newTestServer(t)
	dir := t.TempDir()

	cases := []struct {
		name string
		body map[string]any
		want []string // fragments the 400 must contain
	}{
		{
			name: "filesystem without a path",
			body: map[string]any{"name": "nas", "kind": "filesystem"},
			want: []string{"path"},
		},
		{
			name: "filesystem with a bucket",
			body: map[string]any{"name": "nas", "kind": "filesystem", "path": dir, "bucket": "proxback"},
			want: []string{"filesystem target takes only", "bucket"},
		},
		{
			name: "filesystem with S3 credentials",
			body: map[string]any{
				"name": "nas", "kind": "filesystem", "path": dir,
				"endpoint": "https://s3.example", "accessKey": "k", "secretKey": "s", "pathStyle": true,
			},
			want: []string{"endpoint", "accessKey", "secretKey", "pathStyle"},
		},
		{
			name: "s3 with a path",
			body: map[string]any{"name": "objects", "kind": "s3", "bucket": "proxback", "path": dir},
			want: []string{`no "path"`, "filesystem"},
		},
		{
			name: "s3 without a bucket",
			body: map[string]any{"name": "objects", "kind": "s3"},
			want: []string{"bucket"},
		},
		{
			name: "s3 with allowSameFilesystem",
			body: map[string]any{"name": "objects", "bucket": "proxback", "allowSameFilesystem": true},
			want: []string{"allowSameFilesystem", "filesystem target"},
		},
		{
			name: "unknown kind",
			body: map[string]any{"name": "weird", "kind": "ftp", "path": dir},
			want: []string{"unknown target kind", `"s3"`, `"filesystem"`},
		},
		{
			name: "no name",
			body: map[string]any{"kind": "filesystem", "path": dir},
			want: []string{"name is required"},
		},
		{
			name: "path that does not exist",
			body: fsTargetBody("nas", filepath.Join(dir, "not-mounted")),
			want: []string{"does not exist", "not-mounted"},
		},
		{
			name: "path that is a file",
			body: fsTargetBody("nas", writeFile(t, filepath.Join(dir, "a-file"))),
			want: []string{"not a directory"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := ts.request(t, http.MethodPost, "/api/targets", c.body)
			if code != http.StatusBadRequest {
				t.Fatalf("POST /api/targets = %d (%v), want 400", code, body)
			}
			msg, _ := body["error"].(string)
			for _, want := range c.want {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not mention %q", msg, want)
				}
			}
			t.Logf("400: %s", msg)
		})
	}

	// Nothing was stored by any of the refusals.
	targets, err := ts.st.ListS3Targets(t.Context())
	if err != nil {
		t.Fatalf("list targets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("%d targets were created by rejected requests", len(targets))
	}
}

func writeFile(t *testing.T, path string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestCreateFilesystemTargetRefusesTheDataDirectorysDisk is the foot-gun guard:
// backing up onto the disk ProxBack runs from is refused, and the refusal explains
// how to override it deliberately.
func TestCreateFilesystemTargetRefusesTheDataDirectorysDisk(t *testing.T) {
	ts := newTestServer(t)
	dir := t.TempDir()
	if !sameFilesystemDetectable(t, dir, ts.dataDir) {
		code, body := ts.request(t, http.MethodPost, "/api/targets",
			map[string]any{"name": "nas", "kind": "filesystem", "path": dir})
		if code != http.StatusOK {
			t.Fatalf("on a platform that cannot compare filesystems the target should be accepted, got %d (%v)", code, body)
		}
		return
	}

	code, body := ts.request(t, http.MethodPost, "/api/targets",
		map[string]any{"name": "nas", "kind": "filesystem", "path": dir})
	if code != http.StatusBadRequest {
		t.Fatalf("POST /api/targets on the data directory's filesystem = %d (%v), want 400", code, body)
	}
	msg, _ := body["error"].(string)
	for _, want := range []string{"same filesystem", ts.dataDir, "allowSameFilesystem"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not mention %q", msg, want)
		}
	}

	// With the override it is accepted, and the risk is reported as a warning
	// rather than swallowed.
	code, body = ts.request(t, http.MethodPost, "/api/targets", fsTargetBody("nas", dir))
	if code != http.StatusOK {
		t.Fatalf("POST /api/targets with allowSameFilesystem = %d (%v)", code, body)
	}
	if body["kind"] != store.TargetKindFilesystem {
		t.Fatalf("created target kind = %v", body["kind"])
	}
	if !hasWarning(body, blobstore.WarnSameFilesystemAsDataDir) {
		t.Fatalf("no %s warning in %v", blobstore.WarnSameFilesystemAsDataDir, body["warnings"])
	}
}

func hasWarning(body map[string]any, code string) bool {
	warnings, _ := body["warnings"].([]any)
	for _, w := range warnings {
		if m, ok := w.(map[string]any); ok && m["code"] == code {
			return true
		}
	}
	return false
}

// TestFilesystemTargetLifecycle walks the whole contract the console is built
// against: create, list with capacity, test with structured diagnostics, and the
// unmounted-share case.
func TestFilesystemTargetLifecycle(t *testing.T) {
	ts := newTestServer(t)
	root := t.TempDir()
	// A subdirectory, so the target is deliberately not a mount point.
	dir := filepath.Join(root, "share", "proxback")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	code, created := ts.request(t, http.MethodPost, "/api/targets", fsTargetBody("nas", dir))
	if code != http.StatusOK {
		t.Fatalf("POST /api/targets = %d (%v)", code, created)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("created target has no id: %v", created)
	}
	if created["kind"] != store.TargetKindFilesystem {
		t.Errorf("kind = %v, want %q", created["kind"], store.TargetKindFilesystem)
	}
	// The stored path is absolute: a relative one would depend on the server's
	// working directory.
	if got, _ := created["path"].(string); !filepath.IsAbs(got) || got != dir {
		t.Errorf("path = %v, want the absolute %s", created["path"], dir)
	}
	if created["status"] != "online" {
		t.Errorf("status = %v, want online", created["status"])
	}
	// An S3 target's fields stay empty rather than being invented.
	for _, field := range []string{"endpoint", "bucket", "region"} {
		if created[field] != "" {
			t.Errorf("%s = %v on a filesystem target, want empty", field, created[field])
		}
	}

	code, raw := ts.getRaw(t, "/api/targets")
	if code != http.StatusOK {
		t.Fatalf("GET /api/targets = %d", code)
	}
	var listed []struct {
		ID         string `json:"id"`
		Kind       string `json:"kind"`
		Path       string `json:"path"`
		FreeBytes  int64  `json:"freeBytes"`
		TotalBytes int64  `json:"totalBytes"`
	}
	decodeJSONBody(t, raw, &listed)
	if len(listed) != 1 || listed[0].ID != id {
		t.Fatalf("target list = %+v", listed)
	}
	if listed[0].Kind != store.TargetKindFilesystem || listed[0].Path != dir {
		t.Fatalf("listed target = %+v", listed[0])
	}
	// Capacity is sampled live, so free space legitimately differs between the
	// handler's reading and ours — anything else writing to the volume moves it.
	// Assert the shape and plausibility of the numbers, never equality with a
	// second independent sample.
	free, total := blobstore.Capacity(dir)
	if listed[0].TotalBytes != total {
		t.Errorf("listed total = %d, want the volume's %d", listed[0].TotalBytes, total)
	}
	if total > 0 {
		if listed[0].FreeBytes <= 0 {
			t.Errorf("GET /api/targets reported no free space on %s", runtime.GOOS)
		}
		if listed[0].FreeBytes > listed[0].TotalBytes {
			t.Errorf("listed free %d exceeds total %d", listed[0].FreeBytes, listed[0].TotalBytes)
		}
		// A wildly different reading would mean the handler measured the wrong
		// path; a few MiB of drift is normal. 5% of the volume is generous.
		if drift := listed[0].FreeBytes - free; drift > total/20 || drift < -total/20 {
			t.Errorf("listed free %d is implausibly far from %d on the same volume",
				listed[0].FreeBytes, free)
		}
	}

	// The connection test reports diagnostics, not just pass/fail.
	code, probe := ts.request(t, http.MethodPost, "/api/targets/"+id+"/test", nil)
	if code != http.StatusOK || probe["ok"] != true {
		t.Fatalf("target test = %d (%v)", code, probe)
	}
	if probe["path"] != dir {
		t.Errorf("test path = %v, want %s", probe["path"], dir)
	}
	if probe["isMountPoint"] == true {
		t.Errorf("a nested directory was reported as a mount point: %v", probe)
	}
	if _, ok := probe["warnings"].([]any); !ok {
		t.Fatalf("test returned no warnings array: %v", probe)
	}
	// The not-a-mount-point warning is the NAS-did-not-mount diagnostic.
	if !hasWarning(probe, blobstore.WarnNotMountPoint) && !hasWarning(probe, blobstore.WarnMountPointUnknown) {
		t.Errorf("no mount-point diagnostic for %s: %v", dir, probe["warnings"])
	}

	// Now the share goes away, the way an unmounted NAS does. The test must fail
	// with an error naming the path, and the target must be marked as broken.
	if err := os.RemoveAll(root); err != nil {
		t.Skipf("cannot simulate an unmounted share: %v", err)
	}
	code, probe = ts.request(t, http.MethodPost, "/api/targets/"+id+"/test", nil)
	if code != http.StatusOK {
		t.Fatalf("target test after unmount = %d (%v)", code, probe)
	}
	if probe["ok"] != false {
		t.Fatalf("target test on a vanished path reported ok: %v", probe)
	}
	msg, _ := probe["error"].(string)
	if !strings.Contains(msg, dir) {
		t.Errorf("error %q does not name the path %s", msg, dir)
	}
	target, err := ts.st.S3TargetByID(t.Context(), id)
	if err != nil {
		t.Fatalf("load target: %v", err)
	}
	if target.Status != "error" {
		t.Errorf("target status = %q after a failed test, want error", target.Status)
	}
}

// TestCreateTargetInfersTheKind keeps every existing client working: a body with
// bucket and credentials is still an S3 target, with no "kind" field in sight.
func TestCreateTargetInfersTheKind(t *testing.T) {
	ts := newTestServer(t)

	code, body := ts.request(t, http.MethodPost, "/api/targets", map[string]any{
		"name": "objects", "endpoint": "s3.example.com", "bucket": "proxback",
		"accessKey": "k", "secretKey": "s", "pathStyle": true,
	})
	if code != http.StatusOK {
		t.Fatalf("POST /api/targets = %d (%v)", code, body)
	}
	if body["kind"] != store.TargetKindS3 {
		t.Fatalf("kind = %v, want %q", body["kind"], store.TargetKindS3)
	}
	if body["path"] != "" {
		t.Errorf("path = %v on an S3 target", body["path"])
	}
	// The endpoint is still normalised, and the probe still decides the status.
	if body["endpoint"] != "https://s3.example.com" {
		t.Errorf("endpoint = %v, want it normalised", body["endpoint"])
	}
	if body["freeBytes"] != float64(0) || body["totalBytes"] != float64(0) {
		t.Errorf("an S3 target reported capacity: %v/%v", body["freeBytes"], body["totalBytes"])
	}

	// A body with only a path is a filesystem target.
	dir := t.TempDir()
	code, body = ts.request(t, http.MethodPost, "/api/targets",
		map[string]any{"name": "nas", "path": dir, "allowSameFilesystem": true})
	if code != http.StatusOK {
		t.Fatalf("POST /api/targets with only a path = %d (%v)", code, body)
	}
	if body["kind"] != store.TargetKindFilesystem {
		t.Fatalf("kind = %v, want %q", body["kind"], store.TargetKindFilesystem)
	}
}
