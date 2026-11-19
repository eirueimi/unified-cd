//go:build windows

package cli

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isSharingViolation reports ERROR_SHARING_VIOLATION and ERROR_LOCK_VIOLATION.
func isSharingViolation(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
