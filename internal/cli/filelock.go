package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const (
	lockRetryInterval = 50 * time.Millisecond
	lockWaitTimeout   = 10 * time.Second
	lockStaleAfter    = 30 * time.Second
)

// lockFile takes an exclusive advisory lock by creating path with O_EXCL. It
// works the same on Linux, macOS and Windows (no flock). It retries every 50 ms
// for up to 10 s and removes a lock file older than 30 s (left by a crashed
// process). The returned unlock function removes the lock file.
func lockFile(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(lockWaitTimeout)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		// Windows reports a lock file pending deletion as access denied.
		if !errors.Is(err, fs.ErrExist) && !errors.Is(err, fs.ErrPermission) {
			return nil, err
		}
		if st, serr := os.Stat(path); serr == nil && time.Since(st.ModTime()) > lockStaleAfter {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for lock %s", path)
		}
		time.Sleep(lockRetryInterval)
	}
}
