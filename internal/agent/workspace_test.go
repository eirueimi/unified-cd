package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSanitizeJobName(t *testing.T) {
	assert.Equal(t, "integration-test", sanitizeJobName("integration-test"))
	assert.Equal(t, "a-b-c", sanitizeJobName("a/b:c"))
	assert.Equal(t, "job", sanitizeJobName(""))
}

func TestClaimWorkDir(t *testing.T) {
	got := claimWorkDir("/base", 1, "my-job")
	assert.Equal(t, filepath.Join("/base", "working1", "my-job"), got)
}

func TestPrepareWorkspace_CreatesDirAndMarker(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "w")
	require.NoError(t, prepareWorkspace(context.Background(), dir, "isolated", false, noRuntime))
	b, err := os.ReadFile(filepath.Join(dir, ".ucd-mode"))
	require.NoError(t, err)
	assert.Equal(t, "isolated", string(b))
}

func TestPrepareWorkspace_KeepsFilesWhenNoClean(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("x"), 0o644))
	require.NoError(t, prepareWorkspace(context.Background(), dir, "native", false, noRuntime))
	_, err := os.Stat(filepath.Join(dir, "keep.txt"))
	assert.NoError(t, err)
}

func TestPrepareWorkspace_CleanRemovesFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stale.txt"), []byte("x"), 0o644))
	require.NoError(t, prepareWorkspace(context.Background(), dir, "native", true, noRuntime))
	_, err := os.Stat(filepath.Join(dir, "stale.txt"))
	assert.True(t, os.IsNotExist(err))
}

func TestPrepareWorkspace_ModeFlipResets(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, prepareWorkspace(context.Background(), dir, "isolated", false, noRuntime))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "left.txt"), []byte("x"), 0o644))
	// flip to native, no clean requested → marker mismatch forces a reset
	require.NoError(t, prepareWorkspace(context.Background(), dir, "native", false, noRuntime))
	_, err := os.Stat(filepath.Join(dir, "left.txt"))
	assert.True(t, os.IsNotExist(err))
	b, _ := os.ReadFile(filepath.Join(dir, ".ucd-mode"))
	assert.Equal(t, "native", string(b))
}

func noRuntime() (crt.ContainerRuntime, error) { return nil, os.ErrNotExist }

// TestContainerCleanup_KeepAliveCommand is the regression test for the
// sidecar-sleep-infinity fix: containerCleanup creates a busybox container
// then Execs a cleanup script into it. Since CreateSpec.Command no longer
// defaults to "sleep infinity", this caller must set it explicitly, or
// busybox's default entrypoint reads stdin, hits EOF immediately, and the
// container exits before Exec can run the cleanup script.
func TestContainerCleanup_KeepAliveCommand(t *testing.T) {
	f := &podFakeRT{}
	err := containerCleanup(context.Background(), t.TempDir(), func() (crt.ContainerRuntime, error) { return f, nil })
	require.NoError(t, err)
	require.Len(t, f.created, 1)
	assert.Equal(t, []string{"sleep", "infinity"}, f.created[0].Command)
}
