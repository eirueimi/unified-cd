//go:build windows

package agent

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// diskStats returns the bytes available (to the calling user) and the total
// size of the volume containing path, from a single GetDiskFreeSpaceEx call.
// An unmeasurable path is an error, never zeros, and so is a volume that
// reports a total of 0: there is no capacity to be a fraction of, and "0 free
// of 0" would read as completely full.
func diskStats(path string) (free, total uint64, err error) {
	dirName, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, fmt.Errorf("encode path %s: %w", path, err)
	}
	var freeBytesAvailable, totalBytes, totalFreeBytes uint64
	if err := windows.GetDiskFreeSpaceEx(dirName, &freeBytesAvailable, &totalBytes, &totalFreeBytes); err != nil {
		return 0, 0, fmt.Errorf("GetDiskFreeSpaceEx %s: %w", path, err)
	}
	if totalBytes == 0 {
		return 0, 0, fmt.Errorf("%s: filesystem reports a total size of 0", path)
	}
	return freeBytesAvailable, totalBytes, nil
}
