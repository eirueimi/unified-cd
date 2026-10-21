//go:build !linux && !darwin && !freebsd && !dragonfly && !windows

package shim

import "errors"

// statfs is unsupported on every platform statfs_unix.go and
// statfs_windows.go do not cover (e.g. netbsd, openbsd, solaris, plan9,
// js/wasm). It exists only so the package still compiles there; the shim is
// only ever deployed into linux job containers.
func statfs(path string) (free, total uint64, err error) {
	return 0, 0, errors.New("statfs is not supported on this platform")
}
