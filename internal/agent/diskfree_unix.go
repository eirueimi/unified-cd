//go:build !windows

package agent

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// diskStats returns the bytes available (to an unprivileged user) and the
// total size of the filesystem containing path, from a single statfs call.
// An unmeasurable path is an error, never zeros.
func diskStats(path string) (free, total uint64, err error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), uint64(stat.Blocks) * uint64(stat.Bsize), nil
}
