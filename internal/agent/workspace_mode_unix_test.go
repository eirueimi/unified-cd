//go:build !windows

package agent

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func modeOf(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	require.NoError(t, err)
	return fi.Mode().Perm()
}

// An isolated ephemeral workspace behaves like a Kubernetes emptyDir: 0777,
// whatever the agent's umask, so a job container running as any UID can write
// to it. The mode is set by an explicit chmod, which umask cannot narrow.
func TestPrepareWorkspace_IsolatedEphemeralIsWorldWritableDespiteUmask(t *testing.T) {
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })

	dir := filepath.Join(t.TempDir(), "working0", "job")
	require.NoError(t, prepareWorkspace(context.Background(), dir, "isolated", nil, false, noRuntime, ""))
	assert.Equal(t, os.FileMode(0o777), modeOf(t, dir))
}

// A native workspace is the agent user's own directory: it keeps 0755.
func TestPrepareWorkspace_NativeEphemeralStays0755(t *testing.T) {
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })

	dir := filepath.Join(t.TempDir(), "working0", "job")
	require.NoError(t, prepareWorkspace(context.Background(), dir, "native", nil, false, noRuntime, ""))
	assert.Equal(t, os.FileMode(0o755), modeOf(t, dir))
}

// A volume used as the workspace is a PVC on Kubernetes, not an emptyDir: its
// directory is never re-moded.
func TestPrepareWorkspace_VolumeWorkspaceModeIsUntouched(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o750))
	require.NoError(t, prepareWorkspace(context.Background(), dir, "isolated", testVolume(t), false, noRuntime, ""))
	assert.Equal(t, os.FileMode(0o750), modeOf(t, dir))
}
