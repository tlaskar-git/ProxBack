package blobstore

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// capacityAndIdentitySupported reports whether this platform can answer the
// questions the mount-point and same-filesystem checks are built on. Where it
// cannot, the tests below assert the "unknown" behaviour instead of skipping in
// silence — a check that quietly passes because the platform said nothing would be
// the worst of both worlds.
func capacityAndIdentitySupported(t *testing.T, path string) bool {
	t.Helper()
	probe, err := probePath(path)
	if err != nil {
		t.Logf("platform %s cannot probe %s (%v): asserting the unknown-diagnosis path instead",
			runtime.GOOS, path, err)
		return false
	}
	if probe.ID == "" {
		t.Logf("platform %s does not express filesystem identity: asserting the unknown-diagnosis path instead",
			runtime.GOOS)
		return false
	}
	return true
}

func warningFor(d Diagnosis, code string) (Warning, bool) {
	for _, w := range d.Warnings {
		if w.Code == code {
			return w, true
		}
	}
	return Warning{}, false
}

func codes(d Diagnosis) []string {
	out := make([]string, 0, len(d.Warnings))
	for _, w := range d.Warnings {
		out = append(out, w.Code)
	}
	return out
}

// TestCheckHealthyPath is the happy path an operator sees for a mounted share: it
// passes, it reports capacity, and it says nothing alarming beyond what is true of
// a temp directory.
func TestCheckHealthyPath(t *testing.T) {
	dir := t.TempDir()
	d, err := Check(CheckRequest{Path: dir})
	if err != nil {
		t.Fatalf("Check(%s): %v", dir, err)
	}
	if d.Path != dir {
		abs, _ := filepath.Abs(dir)
		if d.Path != abs {
			t.Fatalf("diagnosis path = %s, want %s", d.Path, dir)
		}
	}
	if capacityAndIdentitySupported(t, dir) {
		if d.TotalBytes <= 0 || d.FreeBytes <= 0 {
			t.Fatalf("capacity = %d free of %d total, want both positive on %s", d.FreeBytes, d.TotalBytes, runtime.GOOS)
		}
		if d.FreeBytes > d.TotalBytes {
			t.Fatalf("free %d exceeds total %d", d.FreeBytes, d.TotalBytes)
		}
		if d.FilesystemType == "" {
			t.Errorf("no filesystem type reported for %s on %s", dir, runtime.GOOS)
		}
		t.Logf("%s: %s, %d bytes free of %d, mounted at %s",
			dir, d.FilesystemType, d.FreeBytes, d.TotalBytes, d.MountPoint)
		if _, ok := warningFor(d, WarnCapacityUnknown); ok {
			t.Errorf("capacity was reported yet warned as unknown: %v", codes(d))
		}
	} else if _, ok := warningFor(d, WarnCapacityUnknown); !ok {
		t.Errorf("platform cannot report capacity but no %s warning: %v", WarnCapacityUnknown, codes(d))
	}
	// The probe file must be gone.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("Check left %d entries in the target: %+v", len(entries), entries)
	}
}

// TestCheckDetectsANonMountPoint is the NAS-did-not-mount diagnostic: a directory
// that is not where a filesystem is mounted gets a warning naming the filesystem it
// really lives on.
func TestCheckDetectsANonMountPoint(t *testing.T) {
	parent := t.TempDir()
	nested := filepath.Join(parent, "mnt", "nas")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	d, err := Check(CheckRequest{Path: nested})
	if err != nil {
		t.Fatalf("Check(%s): %v", nested, err)
	}
	if !capacityAndIdentitySupported(t, nested) {
		if _, ok := warningFor(d, WarnMountPointUnknown); !ok {
			t.Fatalf("platform cannot express mount points but no %s warning: %v", WarnMountPointUnknown, codes(d))
		}
		return
	}
	if d.IsMountPoint {
		t.Fatalf("%s was reported as a mount point", nested)
	}
	warn, ok := warningFor(d, WarnNotMountPoint)
	if !ok {
		t.Fatalf("no %s warning for a plain nested directory: %v", WarnNotMountPoint, codes(d))
	}
	if !strings.Contains(warn.Detail, nested) {
		t.Errorf("warning does not name the path: %s", warn.Detail)
	}
	if d.MountPoint == "" || d.MountPoint == nested {
		t.Errorf("mount point = %q, want the filesystem above %s", d.MountPoint, nested)
	}
	if !strings.Contains(warn.Detail, d.MountPoint) {
		t.Errorf("warning does not name the real mount point %s: %s", d.MountPoint, warn.Detail)
	}
	t.Logf("%s: %s", warn.Code, warn.Detail)
}

// TestMountPointOfARootIsItself checks the other half of the comparison: the root
// of a filesystem must be recognised as a mount point, or the warning above would
// fire for a correctly mounted share.
func TestMountPointOfARootIsItself(t *testing.T) {
	dir := t.TempDir()
	if !capacityAndIdentitySupported(t, dir) {
		t.Logf("skipping the mount-point assertion: %s does not express filesystem identity", runtime.GOOS)
		return
	}
	probe, err := probePath(dir)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	mount, known := nearestMountPoint(dir, probe.ID)
	if !known {
		t.Fatalf("the filesystem holding %s could not be traced to a mount point", dir)
	}
	// The traced mount point is itself a mount point: tracing from it stays put.
	mountProbe, err := probePath(mount)
	if err != nil {
		t.Fatalf("probe %s: %v", mount, err)
	}
	again, known := nearestMountPoint(mount, mountProbe.ID)
	if !known || again != mount {
		t.Fatalf("nearestMountPoint(%s) = %s (known=%v), want itself", mount, again, known)
	}
	t.Logf("%s lives on the filesystem mounted at %s", dir, mount)
}

// TestCheckRefusesTheDataDirectorysFilesystem is the foot-gun the plan names:
// backing up onto the disk ProxBack itself runs from. Both directories are temp
// directories, so on any platform that reports filesystem identity they are on the
// same filesystem.
func TestCheckRefusesTheDataDirectorysFilesystem(t *testing.T) {
	dataDir := t.TempDir()
	target := t.TempDir()
	if !capacityAndIdentitySupported(t, target) {
		d, err := Check(CheckRequest{Path: target, DataDir: dataDir})
		if err != nil {
			t.Fatalf("Check on a platform without filesystem identity = %v, want a pass with a warning", err)
		}
		if _, ok := warningFor(d, WarnSameFilesystemUnknown); !ok {
			t.Fatalf("no %s warning: %v", WarnSameFilesystemUnknown, codes(d))
		}
		return
	}

	_, err := Check(CheckRequest{Path: target, DataDir: dataDir})
	if err == nil {
		t.Fatal("a target on the data directory's own filesystem was accepted")
	}
	for _, want := range []string{target, dataDir, "allowSameFilesystem"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
	t.Logf("refusal: %v", err)

	// The single-disk homelab says so explicitly and gets a warning instead.
	d, err := Check(CheckRequest{Path: target, DataDir: dataDir, AllowSameFilesystem: true})
	if err != nil {
		t.Fatalf("Check with allowSameFilesystem: %v", err)
	}
	if !d.SameFilesystemAsDataDir {
		t.Fatal("sameFilesystemAsDataDir was not reported")
	}
	warn, ok := warningFor(d, WarnSameFilesystemAsDataDir)
	if !ok {
		t.Fatalf("no %s warning: %v", WarnSameFilesystemAsDataDir, codes(d))
	}
	if !strings.Contains(warn.Detail, dataDir) {
		t.Errorf("warning does not name the data directory: %s", warn.Detail)
	}
}

func TestCheckRejectsUnusablePaths(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-mounted")
	_, err := Check(CheckRequest{Path: missing})
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("Check on a missing path = %v, want an error naming %s", err, missing)
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error should say the path does not exist: %v", err)
	}

	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Check(CheckRequest{Path: file}); err == nil ||
		!strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Check on a regular file = %v", err)
	}

	if _, err := Check(CheckRequest{Path: "   "}); err == nil {
		t.Fatal("an empty path was accepted")
	}
}

// TestCheckDetectsAnUnwritablePath covers the read-only export: the path is there,
// it is a directory, and a backup would still fail on the first chunk.
func TestCheckDetectsAnUnwritablePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping: Windows permissions are not expressible with os.Chmod, " +
			"so an unwritable directory cannot be constructed portably here")
	}
	if os.Geteuid() == 0 {
		t.Skip("skipping: running as root, which is allowed to write into a 0500 directory")
	}
	dir := t.TempDir()
	readonly := filepath.Join(dir, "readonly")
	if err := os.Mkdir(readonly, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(readonly, 0o700) })
	_, err := Check(CheckRequest{Path: readonly})
	if err == nil {
		t.Fatal("an unwritable directory was accepted as a target")
	}
	if !strings.Contains(err.Error(), "not writable") || !strings.Contains(err.Error(), readonly) {
		t.Fatalf("error = %v, want it to say %s is not writable", err, readonly)
	}
}

func TestCapacityIsBestEffort(t *testing.T) {
	free, total := Capacity(filepath.Join(t.TempDir(), "gone"))
	if free != 0 || total != 0 {
		t.Fatalf("Capacity of a missing path = %d/%d, want 0/0", free, total)
	}
	if free, total := Capacity("  "); free != 0 || total != 0 {
		t.Fatalf("Capacity of an empty path = %d/%d, want 0/0", free, total)
	}
	dir := t.TempDir()
	free, total = Capacity(dir)
	if capacityAndIdentitySupported(t, dir) {
		if free <= 0 || total <= 0 {
			t.Fatalf("Capacity(%s) = %d/%d, want positive numbers on %s", dir, free, total, runtime.GOOS)
		}
	} else if free != 0 || total != 0 {
		t.Fatalf("Capacity(%s) = %d/%d on a platform that cannot report it", dir, free, total)
	}
}
