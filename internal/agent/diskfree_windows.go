//go:build windows

package agent

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// diskStats returns the bytes available (to the calling user) and the total
// size of the volume containing path, from a single GetDiskFreeSpaceEx call.
// An unmeasurable path is an error, never zeros.
func diskStats(path string) (free, total uint64, err error) {
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
