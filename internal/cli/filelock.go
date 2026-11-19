package cli

import (
	"crypto/rand"
	"encoding/hex"
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
// for up to 10 s. A lock file older than 30 s (left by a crashed process) is
// broken by renaming it to a unique name first, so two waiters cannot both
// delete and then both create. The lock file holds a random nonce and the
// returned unlock function removes the file only if it still holds that nonce.
func lockFile(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	nonce, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(lockWaitTimeout)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, werr := f.WriteString(nonce)
			cerr := f.Close()
			if werr != nil || cerr != nil {
				_ = os.Remove(path)
				return nil, errors.Join(werr, cerr)
			}
			return func() {
				if b, rerr := os.ReadFile(path); rerr == nil && string(b) == nonce {
					_ = os.Remove(path)
				}
			}, nil
		}
		// Windows reports a lock file pending deletion as access denied.
		if !errors.Is(err, fs.ErrExist) && !errors.Is(err, fs.ErrPermission) {
			return nil, err
		}
		if st, serr := os.Stat(path); serr == nil && time.Since(st.ModTime()) > lockStaleAfter {
			if breakStaleLock(path) {
				continue
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for lock %s", path)
		}
		time.Sleep(lockRetryInterval)
	}
}

// breakStaleLock moves a stale lock out of the way by renaming it to a unique
// name; only the process whose rename succeeds removes it. If the renamed file
// turns out to be fresh (another process broke the stale lock and took a new
// one between our stat and rename), it is put back when the path is free.
func breakStaleLock(path string) bool {
	suffix, err := randomHex(8)
	if err != nil {
		return false
	}
	moved := path + "." + suffix + ".stale"
	if err := os.Rename(path, moved); err != nil {
		return false
	}
	if st, err := os.Stat(moved); err == nil && time.Since(st.ModTime()) <= lockStaleAfter {
		// Residual window: between our rename and this link-back the path is
		// free. If a third process creates the lock there, the link fails and
		// the owner of the lock we moved keeps running without a lock file
		// while the third process holds one, so two critical sections can
		// overlap. The window is a few syscalls wide and the lock only guards
		// config-file rewrites, so it is accepted; the link is best effort.
		_ = os.Link(moved, path)
	}
	_ = os.Remove(moved)
	return true
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
