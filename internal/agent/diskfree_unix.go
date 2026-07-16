//go:build !windows

package agent

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// freeBytes returns the number of bytes available (to an unprivileged user)
// on the filesystem containing path.
func freeBytes(path string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}
