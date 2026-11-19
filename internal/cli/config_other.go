//go:build !windows

package cli

// isSharingViolation is Windows-specific; elsewhere no error is retried.
func isSharingViolation(error) bool { return false }
