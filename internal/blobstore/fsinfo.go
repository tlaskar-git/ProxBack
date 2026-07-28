package blobstore

// fsProbe is everything the platform layer can say about the filesystem holding
// a path. Every field is optional: a platform ProxBack has no syscalls for still
// has to be able to run a filesystem target, it just reports less about it.
type fsProbe struct {
	// FreeBytes and TotalBytes are the capacity available to this user and the
	// filesystem's size. Both zero means the platform could not report capacity.
	FreeBytes  int64
	TotalBytes int64
	// Type is the filesystem's name ("ext4", "nfs", "NTFS"), empty when unknown.
	Type string
	// ID identifies the filesystem the path lives on: two paths with the same
	// non-empty ID are on the same filesystem, which is how both the mount-point
	// check and the same-filesystem-as-the-data-directory check are made. Empty
	// means the platform cannot express filesystem identity, and the checks that
	// depend on it are reported as unknown rather than silently passing.
	ID string
}

// capacityKnown reports whether the platform answered with real numbers.
func (p fsProbe) capacityKnown() bool { return p.TotalBytes > 0 }
