//go:build linux || darwin || freebsd || dragonfly

// This is an explicit list rather than `unix`: golang.org/x/sys/unix has no
// Statfs on netbsd, solaris or illumos, and openbsd's Statfs_t has no Bavail,
// so `unix` would break the build there. Those fall through to
// statfs_other.go.

package shim

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// statfs measures path with a single statfs call. Bavail is the space
// available to an unprivileged user (not Bfree, which includes root's
// reserve), matching what a job step can actually write.
func statfs(path string) (free, total uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), uint64(st.Blocks) * uint64(st.Bsize), nil
}
