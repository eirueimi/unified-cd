//go:build !windows

package agent

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// diskStats returns the bytes available (to an unprivileged user) and the
// total size of the filesystem containing path, from a single statfs call.
// An unmeasurable path is an error, never zeros, and so is a filesystem that
// reports a total of 0 (some pseudo filesystems do): there is no capacity to be
// a fraction of, and "0 free of 0" would read as completely full.
func diskStats(path string) (free, total uint64, err error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	total = uint64(stat.Blocks) * uint64(stat.Bsize)
	if total == 0 {
		return 0, 0, fmt.Errorf("%s: filesystem reports a total size of 0", path)
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), total, nil
}
