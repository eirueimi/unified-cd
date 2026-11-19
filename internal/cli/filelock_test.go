package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLockFile_ExclusiveAndReleased(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml.lock")
	unlock, err := lockFile(p)
	require.NoError(t, err)

	got := make(chan struct{})
	go func() {
		u2, err := lockFile(p)
		if err == nil {
			u2()
		}
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("second lock acquired while first was held")
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("second lock never acquired after unlock")
	}
	_, err = os.Stat(p)
	assert.True(t, os.IsNotExist(err))
}

func TestLockFile_RemovesStaleLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml.lock")
	require.NoError(t, os.WriteFile(p, nil, 0o600))
	old := time.Now().Add(-time.Minute)
	require.NoError(t, os.Chtimes(p, old, old))
	unlock, err := lockFile(p)
	require.NoError(t, err)
	unlock()
}

func TestLockFile_UnlockKeepsForeignLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml.lock")
	unlock, err := lockFile(p)
	require.NoError(t, err)
	// Another process broke our (apparently stale) lock and took its own.
	require.NoError(t, os.WriteFile(p, []byte("someone-else"), 0o600))
	unlock()
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	assert.Equal(t, "someone-else", string(b))
}

func TestLockFile_StaleBreakLeavesNoStaleFiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml.lock")
	require.NoError(t, os.WriteFile(p, []byte("dead"), 0o600))
	old := time.Now().Add(-time.Minute)
	require.NoError(t, os.Chtimes(p, old, old))
	unlock, err := lockFile(p)
	require.NoError(t, err)
	unlock()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}
