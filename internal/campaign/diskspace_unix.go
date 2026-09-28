//go:build darwin || linux

package campaign

import "golang.org/x/sys/unix"

// freeBytes reports the bytes available to an unprivileged user on the
// filesystem holding path.
func freeBytes(path string) (uint64, bool) {
	var st unix.Statfs_t
	if unix.Statfs(path, &st) != nil {
		return 0, false
	}
	return st.Bavail * uint64(st.Bsize), true //nolint:gosec,unconvert // Bsize is int64 on Linux and uint32 on darwin; both are small and positive
}
