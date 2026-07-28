package blobstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Warning codes reported by Check. They are part of the API contract: the console
// renders each one with its own copy, so a code is never repurposed.
const (
	// WarnNotMountPoint means the path's filesystem is mounted somewhere above it.
	// That is fine for a deliberately chosen subdirectory of a share, and is the
	// signature of disaster when the path *was* meant to be the mount point: the
	// NAS did not mount, so backups would quietly fill the local disk instead.
	WarnNotMountPoint = "not_a_mount_point"
	// WarnMountPointUnknown means the platform cannot express filesystem identity,
	// so the mount-point question could not be answered either way.
	WarnMountPointUnknown = "mount_point_unknown"
	// WarnSameFilesystemAsDataDir is reported when the target shares a filesystem
	// with ProxBack's own data directory and the operator has explicitly allowed
	// it. Without the override this is a refusal, not a warning.
	WarnSameFilesystemAsDataDir = "same_filesystem_as_data_dir"
	// WarnSameFilesystemUnknown means the comparison against the data directory
	// could not be made.
	WarnSameFilesystemUnknown = "same_filesystem_unknown"
	// WarnCapacityUnknown means free/total space could not be determined.
	WarnCapacityUnknown = "capacity_unknown"
)

// Warning is one structured diagnostic from a filesystem target check. The code
// is for the UI, the detail is the sentence an operator reads.
type Warning struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// Diagnosis is what a filesystem connection test found out. It is filled in as
// far as the check got, so capacity and warnings are still reported alongside a
// failure.
type Diagnosis struct {
	Path           string `json:"path"`
	FreeBytes      int64  `json:"freeBytes"`
	TotalBytes     int64  `json:"totalBytes"`
	FilesystemType string `json:"filesystemType,omitempty"`
	// MountPoint is the filesystem the target actually lives on, as far as it could
	// be established ("/" when a supposed NAS path is really the root disk).
	MountPoint string `json:"mountPoint,omitempty"`
	// IsMountPoint reports whether the target path is itself where a filesystem is
	// mounted.
	IsMountPoint bool `json:"isMountPoint"`
	// SameFilesystemAsDataDir reports whether the target shares a filesystem with
	// ProxBack's data directory. It can only be true in a successful check when the
	// operator allowed it explicitly.
	SameFilesystemAsDataDir bool      `json:"sameFilesystemAsDataDir"`
	Warnings                []Warning `json:"warnings"`
}

// CheckRequest asks for a filesystem target's diagnostics.
type CheckRequest struct {
	// Path is the target's base path.
	Path string
	// DataDir is ProxBack's own data directory, compared against the target so a
	// backup cannot be pointed at the disk the server is running from.
	DataDir string
	// AllowSameFilesystem turns that refusal into a warning. It exists for the
	// single-disk homelab where the operator genuinely means it.
	AllowSameFilesystem bool
}

// Check runs the diagnostics an operator needs before trusting a NAS or local
// path with their backups: the path exists, is a directory, and is genuinely
// writable (probe file written, read back, removed); how much space it has and
// what kind of filesystem it is; whether it is a mount point; and whether it is
// the same filesystem ProxBack itself lives on.
//
// A returned error is a refusal — the target must not be created. Warnings are
// advisory and are also returned alongside a nil error.
func Check(req CheckRequest) (Diagnosis, error) {
	d := Diagnosis{Warnings: []Warning{}}
	path := strings.TrimSpace(req.Path)
	if path == "" {
		return d, errors.New("filesystem target: path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return d, fmt.Errorf("filesystem target: resolve %q: %w", path, err)
	}
	d.Path = abs

	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return d, fmt.Errorf("filesystem target: %s does not exist — create the directory, "+
				"or mount the share there first", abs)
		}
		return d, fmt.Errorf("filesystem target: cannot inspect %s: %w", abs, err)
	}
	if !info.IsDir() {
		return d, fmt.Errorf("filesystem target: %s is not a directory", abs)
	}

	probe, probeErr := probePath(abs)
	d.FreeBytes, d.TotalBytes, d.FilesystemType = probe.FreeBytes, probe.TotalBytes, probe.Type
	switch {
	case probeErr != nil:
		d.warn(WarnCapacityUnknown, fmt.Sprintf("free space for %s could not be read: %v", abs, probeErr))
	case !probe.capacityKnown():
		d.warn(WarnCapacityUnknown, fmt.Sprintf("this platform does not report free space for %s", abs))
	}

	if err := probeWritable(abs); err != nil {
		return d, err
	}

	d.checkMountPoint(abs, probe)
	if err := d.checkDataDir(req, probe); err != nil {
		return d, err
	}
	return d, nil
}

// checkMountPoint establishes where the target's filesystem is mounted and warns
// when that is not the target path itself.
func (d *Diagnosis) checkMountPoint(abs string, probe fsProbe) {
	if probe.ID == "" {
		d.warn(WarnMountPointUnknown, fmt.Sprintf(
			"this platform cannot report which filesystem %s is on, so ProxBack cannot tell "+
				"whether a share is really mounted there", abs))
		return
	}
	mount, known := nearestMountPoint(abs, probe.ID)
	d.MountPoint = mount
	if !known {
		d.warn(WarnMountPointUnknown, fmt.Sprintf(
			"the filesystem holding %s could not be traced to a mount point", abs))
		return
	}
	if mount == abs {
		d.IsMountPoint = true
		return
	}
	d.warn(WarnNotMountPoint, fmt.Sprintf(
		"%s is not a mount point: it is a directory on the filesystem mounted at %s. "+
			"That is expected for a subdirectory of a share, but if you expected a NAS to be "+
			"mounted at %s then it is not mounted, and backups would fill %s instead",
		abs, mount, abs, mount))
}

// checkDataDir refuses a target that shares a filesystem with ProxBack's data
// directory, because a backup that dies with the disk it was protecting is not a
// backup. The operator can override it, and then gets a warning instead.
func (d *Diagnosis) checkDataDir(req CheckRequest, probe fsProbe) error {
	dataDir := strings.TrimSpace(req.DataDir)
	if dataDir == "" {
		return nil
	}
	dataAbs, err := filepath.Abs(dataDir)
	if err != nil {
		d.warn(WarnSameFilesystemUnknown, fmt.Sprintf("data directory %q could not be resolved: %v", dataDir, err))
		return nil
	}
	dataProbe, err := probePath(dataAbs)
	if err != nil || dataProbe.ID == "" || probe.ID == "" {
		detail := fmt.Sprintf("ProxBack cannot tell whether %s is on the same filesystem as its "+
			"data directory %s", d.Path, dataAbs)
		if err != nil {
			detail += ": " + err.Error()
		}
		d.warn(WarnSameFilesystemUnknown, detail)
		return nil
	}
	if dataProbe.ID != probe.ID {
		return nil
	}
	d.SameFilesystemAsDataDir = true
	if !req.AllowSameFilesystem {
		return fmt.Errorf("filesystem target: %s is on the same filesystem as ProxBack's data "+
			"directory %s — a backup on the disk it is protecting is lost with that disk. "+
			"Point the target at a different disk or share, or set allowSameFilesystem to "+
			"accept the risk deliberately", d.Path, dataAbs)
	}
	d.warn(WarnSameFilesystemAsDataDir, fmt.Sprintf(
		"%s is on the same filesystem as ProxBack's data directory %s, allowed explicitly: "+
			"losing that disk loses the backups with it", d.Path, dataAbs))
	return nil
}

func (d *Diagnosis) warn(code, detail string) {
	d.Warnings = append(d.Warnings, Warning{Code: code, Detail: detail})
}

// nearestMountPoint climbs from path while the filesystem identity stays the
// same; the last directory that still belongs to it is where that filesystem is
// mounted. known is false when the climb was cut short (a parent that cannot be
// probed), in which case the returned path is only the best guess.
func nearestMountPoint(path, id string) (mount string, known bool) {
	current := path
	for {
		parent := filepath.Dir(current)
		if parent == current {
			// Reached the root of the path hierarchy (or a volume root on Windows).
			return current, true
		}
		probe, err := probePath(parent)
		if err != nil || probe.ID == "" {
			return current, false
		}
		if probe.ID != id {
			return current, true
		}
		current = parent
	}
}

// Capacity reports free and total bytes for a path, best effort: zeroes when the
// platform cannot say or the path is not available. It is what GET /api/targets
// uses to show a filesystem target's fill level.
func Capacity(path string) (free, total int64) {
	if strings.TrimSpace(path) == "" {
		return 0, 0
	}
	probe, err := probePath(path)
	if err != nil {
		return 0, 0
	}
	return probe.FreeBytes, probe.TotalBytes
}
