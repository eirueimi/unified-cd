//go:build !windows

package agent

import (
	"errors"
	"syscall"
)

// processAlive reports whether a process with this ID exists on this machine.
// EPERM means it exists and belongs to someone else.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
