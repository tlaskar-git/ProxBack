//go:build windows

package blobstore

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
)

// probePath is the Windows equivalent of the Linux statfs/stat pair:
// GetDiskFreeSpaceEx for capacity, GetVolumeInformation for the filesystem type,
// and the volume's mount path (GetVolumePathName) as filesystem identity.
//
// The volume mount path is the honest analogue of st_dev here: every path on the
// same volume resolves to the same mount path, and a directory that *is* a mount
// path is where another volume is attached — a drive root like "D:\" or a folder
// a volume is mounted into, which is exactly the NTFS notion of a mount point.
func probePath(path string) (fsProbe, error) {
	var out fsProbe
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return out, fmt.Errorf("path %s: %w", path, err)
	}
	var freeToCaller, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeToCaller, &total, &totalFree); err != nil {
		return out, fmt.Errorf("GetDiskFreeSpaceEx %s: %w", path, err)
	}
	out.FreeBytes = int64(freeToCaller) //nolint:gosec // volume sizes fit an int64
	out.TotalBytes = int64(total)

	volume := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumePathName(p, &volume[0], uint32(len(volume))); err != nil {
		return out, fmt.Errorf("GetVolumePathName %s: %w", path, err)
	}
	mount := windows.UTF16ToString(volume)
	out.ID = "volume:" + strings.ToLower(mount)

	root, err := windows.UTF16PtrFromString(mount)
	if err != nil {
		return out, fmt.Errorf("volume %s: %w", mount, err)
	}
	name := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumeInformation(root, nil, 0, nil, nil, nil, &name[0], uint32(len(name))); err == nil {
		out.Type = windows.UTF16ToString(name)
	}
	return out, nil
}
