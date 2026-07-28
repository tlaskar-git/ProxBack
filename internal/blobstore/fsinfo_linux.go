//go:build linux

package blobstore

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// probePath reports capacity, filesystem type and filesystem identity for a path
// on Linux — the platform ProxBack's server actually ships on.
//
// Identity is the containing device from stat(2). Comparing a directory's device
// with its parent's is the standard way to ask "is this a mount point?", and
// comparing the target's with the data directory's answers "am I about to back up
// onto the disk I am running from?".
func probePath(path string) (fsProbe, error) {
	var out fsProbe
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return out, fmt.Errorf("statfs %s: %w", path, err)
	}
	bsize := int64(st.Bsize)
	if bsize > 0 {
		// Bavail, not Bfree: the reserved blocks root can still write into are not
		// space a backup may count on.
		out.FreeBytes = int64(st.Bavail) * bsize //nolint:gosec // kernel-reported block counts
		out.TotalBytes = int64(st.Blocks) * bsize
	}
	out.Type = linuxFSName(int64(st.Type))

	var sb unix.Stat_t
	if err := unix.Stat(path, &sb); err != nil {
		return out, fmt.Errorf("stat %s: %w", path, err)
	}
	out.ID = fmt.Sprintf("dev:%d", uint64(sb.Dev)) //nolint:unconvert // Dev is not uint64 on every arch
	return out, nil
}

// linuxFSName maps the statfs magic to the name an operator would recognise. An
// unmapped filesystem is reported by its magic rather than as "unknown", because
// that number is still a usable clue in a support conversation.
func linuxFSName(magic int64) string {
	switch magic {
	case unix.EXT4_SUPER_MAGIC:
		return "ext2/3/4"
	case unix.XFS_SUPER_MAGIC:
		return "xfs"
	case unix.BTRFS_SUPER_MAGIC:
		return "btrfs"
	case zfsSuperMagic:
		return "zfs"
	case unix.NFS_SUPER_MAGIC:
		return "nfs"
	case unix.SMB_SUPER_MAGIC:
		return "smb"
	case unix.SMB2_SUPER_MAGIC, unix.CIFS_SUPER_MAGIC:
		return "cifs/smb"
	case unix.TMPFS_MAGIC:
		return "tmpfs"
	case unix.OVERLAYFS_SUPER_MAGIC:
		return "overlayfs"
	case unix.FUSE_SUPER_MAGIC:
		return "fuse"
	case unix.EXFAT_SUPER_MAGIC:
		return "exfat"
	case unix.MSDOS_SUPER_MAGIC:
		return "vfat"
	default:
		return fmt.Sprintf("unknown (magic 0x%x)", magic)
	}
}

// zfsSuperMagic is ZFS-on-Linux's superblock magic. It has no name in
// golang.org/x/sys/unix, and a NAS target on a ZFS dataset is a case worth
// naming.
const zfsSuperMagic = 0x2fc12fc1
