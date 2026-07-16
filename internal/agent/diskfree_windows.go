//go:build windows

package agent

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// freeBytes returns the number of bytes available (to the calling user) on
// the volume containing path.
func freeBytes(path string) (uint64, error) {
	dirName, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("encode path %s: %w", path, err)
	}
	var freeBytesAvailable, totalBytes, totalFreeBytes uint64
	if err := windows.GetDiskFreeSpaceEx(dirName, &freeBytesAvailable, &totalBytes, &totalFreeBytes); err != nil {
		return 0, fmt.Errorf("GetDiskFreeSpaceEx %s: %w", path, err)
	}
	return freeBytesAvailable, nil
}
