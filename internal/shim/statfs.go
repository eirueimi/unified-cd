package shim

import "fmt"

// Statfs returns the bytes available (to an unprivileged user) and the total
// size of the filesystem containing path. It backs `ucd-sh statfs`, which a
// Kubernetes agent execs inside a job container to measure a volume it cannot
// reach from outside the pod, without assuming `df` exists in the image.
//
// An unmeasurable path is an error, never zeros: the caller parses the
// result as numbers, so a failure must not be representable as "0 free of 0".
// A filesystem that reports a total of 0 (some pseudo filesystems do) is
// treated the same way, since there is no capacity to be a fraction of.
//
// The per-platform measurement is statfs (statfs_unix.go,
// statfs_windows.go, statfs_other.go). It is deliberately separate from the
// agent's own disk helper: the shim is a standalone binary and must not
// import internal/agent.
func Statfs(path string) (free, total uint64, err error) {
	free, total, err = statfs(path)
	if err != nil {
		return 0, 0, err
	}
	if total == 0 {
		return 0, 0, fmt.Errorf("%s: filesystem reports a total size of 0", path)
	}
	return free, total, nil
}
