//go:build windows

package gittemplate

// checkOwnedPrivateDir is a no-op on Windows, where Unix ownership and mode
// bits do not apply.
func checkOwnedPrivateDir(string) error { return nil }
