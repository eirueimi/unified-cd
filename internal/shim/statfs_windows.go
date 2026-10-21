//go:build windows

package shim

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// statfs measures path with a single GetDiskFreeSpaceEx call, returning the
// bytes available to the calling user (not the volume's total free bytes,
// which ignores per-user quotas) and the volume's total size.
func statfs(path string) (free, total uint64, err error) {
	dirName, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, fmt.Errorf("encode path %s: %w", path, err)
	}
	var freeBytesAvailable, totalBytes, totalFreeBytes uint64
	if err := windows.GetDiskFreeSpaceEx(dirName, &freeBytesAvailable, &totalBytes, &totalFreeBytes); err != nil {
		return 0, 0, fmt.Errorf("GetDiskFreeSpaceEx %s: %w", path, err)
	}
	return freeBytesAvailable, totalBytes, nil
}
