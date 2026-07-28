//go:build !linux && !windows

package blobstore

// probePath is the graceful fallback for platforms ProxBack's server does not
// ship on (macOS and the BSDs, where the statfs struct differs). A filesystem
// target still works there — the round-trip probe is pure Go — but capacity is
// reported as zero and the mount-point and same-filesystem checks report
// "unknown" rather than pretending to have passed.
func probePath(_ string) (fsProbe, error) { return fsProbe{}, nil }
