package agent

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
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

func TestSanitizeJobName_Degenerate(t *testing.T) {
	for _, in := range []string{"..", ".", "...", "con", "CON", "Prn", "nul", "aux", "com1", "COM9", "lpt1", "nul.txt"} {
		got := sanitizeJobName(in)
		assert.NotContains(t, []string{".", "..", "..."}, got, "input %q", in)
		assert.NotEqual(t, "con", strings.ToLower(got), "input %q -> reserved", in)
		require.NotEmpty(t, got)
		// The result must be a safe single segment (no dot-only, not reserved).
	}
	// ordinary names pass through unchanged
	assert.Equal(t, "my-job", sanitizeJobName("my-job"))
	assert.Equal(t, "job_1.2", sanitizeJobName("job_1.2"))
}

func TestClaimWorkDir(t *testing.T) {
	got := claimWorkDir("/base", 1, "my-job")
	assert.Equal(t, filepath.Join("/base", "working1", "my-job"), got)
}

func noRuntime() (crt.ContainerRuntime, error) { return nil, os.ErrNotExist }

// TestContainerCleanup_KeepAliveCommand is the regression test for the
// sidecar-sleep-infinity fix: containerCleanup creates a busybox container
// then Execs a cleanup script into it. Since CreateSpec.Entrypoint no longer
// defaults to "sleep infinity", this caller must set it explicitly, or
// busybox's default entrypoint reads stdin, hits EOF immediately, and the
// container exits before Exec can run the cleanup script. The keep-alive is
// now the ucd-sh shim's pause mode, not sleep infinity (see Component 4 of
// the step-shell-shim design).
func TestContainerCleanup_KeepAliveCommand(t *testing.T) {
	f := &podFakeRT{}
	err := containerCleanup(context.Background(), t.TempDir(), func() (crt.ContainerRuntime, error) { return f, nil }, "")
	require.NoError(t, err)
	require.Len(t, f.created, 1)
	assert.Equal(t, []string{"/.ucd/ucd-sh", "pause"}, f.created[0].Entrypoint)
}

// TestContainerCleanup_MountsToolsDirReadOnly verifies the cleanup container
// carries the same read-only /.ucd shim mount as every other container this
// agent creates, alongside the existing workspace mount, when toolsDir is
// set.
func TestContainerCleanup_MountsToolsDirReadOnly(t *testing.T) {
	f := &podFakeRT{}
	workDir := t.TempDir()
	err := containerCleanup(context.Background(), workDir, func() (crt.ContainerRuntime, error) { return f, nil }, "/host/tools")
	require.NoError(t, err)
	require.Len(t, f.created, 1)
	mounts := f.created[0].Mounts
	require.Len(t, mounts, 2)
	assert.Equal(t, crt.Mount{HostPath: workDir, ContainerPath: "/w"}, mounts[0])
	assert.Equal(t, crt.Mount{HostPath: "/host/tools", ContainerPath: "/.ucd", ReadOnly: true}, mounts[1])
}

// --- the default workspace is ephemeral ---

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func TestPrepareWorkspace_CreatesTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "w")
	require.NoError(t, prepareWorkspace(context.Background(), dir, "isolated", nil, false, noRuntime, ""))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "an ordinary workspace starts empty; the agent leaves nothing in it")
}

func TestPrepareWorkspace_CleansByDefault(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "stale.txt"), "x")
	writeFile(t, filepath.Join(dir, "sub", "deep.txt"), "x")
	require.NoError(t, prepareWorkspace(context.Background(), dir, "native", nil, false, noRuntime, ""))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "a file left by a previous run must be gone although clean=false")
}

// A mode flip used to be the only reason an ordinary workspace was reset. It is
// cleaned every run now, so the flip needs no special case and must not fail.
func TestPrepareWorkspace_ModeFlipOnAnOrdinaryWorkspaceJustCleans(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, prepareWorkspace(context.Background(), dir, "isolated", nil, false, noRuntime, ""))
	writeFile(t, filepath.Join(dir, "left.txt"), "x")
	require.NoError(t, prepareWorkspace(context.Background(), dir, "native", nil, false, noRuntime, ""))
	assert.NoFileExists(t, filepath.Join(dir, "left.txt"))
}

func TestPrepareWorkspace_CleanupFailureOnAnOrdinaryWorkspaceFailsTheRun(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "stale.txt"), "x")
	failRemoval(t, func(string) bool { return true })
	err := prepareWorkspace(context.Background(), dir, "native", nil, false, noRuntime, "")
	require.Error(t, err, "a run must not start on a previous run's files")
	assert.Contains(t, err.Error(), dir)
}

func TestFinishWorkspace_SweepsAfterTheRun(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "out", "build.bin"), "x")
	finishWorkspace(context.Background(), dir, nil, noRuntime, "")
	_, err := os.Stat(dir)
	assert.True(t, os.IsNotExist(err), "the run's output must not hold disk until the job's next run")
}

func TestFinishWorkspace_SweepFailureIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "out.txt"), "x")
	failRemoval(t, func(string) bool { return true })
	var logs bytes.Buffer
	defer captureSlog(&logs)()
	// finishWorkspace returns nothing: there is no error for the caller to turn
	// into a failed run. It must warn, and not panic.
	finishWorkspace(context.Background(), dir, nil, noRuntime, "")
	assert.Contains(t, logs.String(), "sweep after the run failed")
	assert.FileExists(t, filepath.Join(dir, "out.txt"), "the failing remover left the tree in place")
}

func TestFinishWorkspace_LeavesAVolumeBackedWorkspaceAlone(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Library", "cache.bin"), "x")
	vol := &volumeWorkspace{Name: "unity", ModeMarker: filepath.Join(t.TempDir(), "unity")}
	finishWorkspace(context.Background(), dir, vol, noRuntime, "")
	assert.FileExists(t, filepath.Join(dir, "Library", "cache.bin"))
}

// --- a volume-backed workspace is never reset on its own ---

func testVolume(t *testing.T) *volumeWorkspace {
	t.Helper()
	return &volumeWorkspace{Name: "unity", ModeMarker: volumeModeMarker(t.TempDir(), "unity")}
}

func TestPrepareWorkspace_VolumeBackedIsNotCleaned(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Library", "cache.bin"), "x")
	require.NoError(t, prepareWorkspace(context.Background(), dir, "native", testVolume(t), false, noRuntime, ""))
	assert.FileExists(t, filepath.Join(dir, "Library", "cache.bin"))
}

// The 200GB path: the mode marker says isolated, this run is native, and the
// old behaviour was RemoveAll of the whole workspace. It must fail instead.
func TestPrepareWorkspace_VolumeBackedModeFlipFailsAndKeepsData(t *testing.T) {
	dir := t.TempDir()
	vol := testVolume(t)
	require.NoError(t, prepareWorkspace(context.Background(), dir, "isolated", vol, false, noRuntime, ""))
	writeFile(t, filepath.Join(dir, "Library", "cache.bin"), "x")
	writeFile(t, filepath.Join(dir, "top.txt"), "x")

	err := prepareWorkspace(context.Background(), dir, "native", vol, false, noRuntime, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"unity"`)
	assert.Contains(t, err.Error(), "isolated")
	assert.Contains(t, err.Error(), "native")
	assert.Contains(t, err.Error(), "spec.cleanWorkspace", "the remedy a native job can actually use")
	assert.Contains(t, err.Error(), vol.ModeMarker, "the operator's manual exit")
	assert.NotContains(t, err.Error(), "a isolated")
	assert.FileExists(t, filepath.Join(dir, "Library", "cache.bin"))
	assert.FileExists(t, filepath.Join(dir, "top.txt"))

	// The failed run did not move the marker, so the job's original mode still works.
	require.NoError(t, prepareWorkspace(context.Background(), dir, "isolated", vol, false, noRuntime, ""))
}

func TestPrepareWorkspace_VolumeBackedMarkerIsNotWrittenIntoTheVolume(t *testing.T) {
	dir := t.TempDir()
	vol := testVolume(t)
	require.NoError(t, prepareWorkspace(context.Background(), dir, "isolated", vol, false, noRuntime, ""))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing the agent writes may appear in a volume")
	b, err := os.ReadFile(vol.ModeMarker)
	require.NoError(t, err)
	assert.Equal(t, "isolated", string(b))
}

func TestPrepareWorkspace_CleanWorkspaceWipesAVolume(t *testing.T) {
	dir := t.TempDir()
	vol := testVolume(t)
	writeFile(t, filepath.Join(dir, "Library", "cache.bin"), "x")
	writeFile(t, filepath.Join(dir, ".hidden"), "x")
	require.NoError(t, prepareWorkspace(context.Background(), dir, "native", vol, true, noRuntime, ""))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "the documented escape hatch empties the volume")
	_, err = os.Stat(dir)
	assert.NoError(t, err, "the volume directory itself stays: it may be a mount point")
}

// clean=true is also the explicit way past a mode flip.
func TestPrepareWorkspace_CleanWorkspaceMovesPastAModeFlip(t *testing.T) {
	dir := t.TempDir()
	vol := testVolume(t)
	require.NoError(t, prepareWorkspace(context.Background(), dir, "isolated", vol, false, noRuntime, ""))
	writeFile(t, filepath.Join(dir, "left.txt"), "x")
	require.NoError(t, prepareWorkspace(context.Background(), dir, "native", vol, true, noRuntime, ""))
	assert.NoFileExists(t, filepath.Join(dir, "left.txt"))
	b, _ := os.ReadFile(vol.ModeMarker)
	assert.Equal(t, "native", string(b))
}

// cleanupRT is a runtime whose cleanup container really removes what the
// "root-owned" tree holds, and records that it was used.
type cleanupRT struct {
	podFakeRT
	dir string
	// onExec runs when the cleanup script does.
	onExec func()
}

func (f *cleanupRT) Exec(ctx context.Context, h crt.ContainerHandle, s crt.ExecSpec, o, e io.Writer) (int, error) {
	if f.onExec != nil {
		f.onExec()
	}
	entries, _ := os.ReadDir(f.dir)
	for _, en := range entries {
		_ = os.RemoveAll(filepath.Join(f.dir, en.Name()))
	}
	return f.podFakeRT.Exec(ctx, h, s, o, e)
}

// failRemoval makes the agent's own removal fail for the paths match accepts,
// as it does for files a rootful container left owned by root.
func failRemoval(t *testing.T, match func(path string) bool) {
	t.Helper()
	orig := removeAll
	removeAll = func(p string) error {
		if match(p) {
			return os.ErrPermission
		}
		return orig(p)
	}
	t.Cleanup(func() { removeAll = orig })
}

func TestPrepareWorkspace_CleanWorkspaceFallsBackToTheCleanupContainer(t *testing.T) {
	dir := t.TempDir()
	vol := testVolume(t)
	writeFile(t, filepath.Join(dir, "rootfile"), "x")

	cleaned := false
	rt := &cleanupRT{dir: dir, onExec: func() { cleaned = true }}
	// The plain removal fails until the cleanup container has run.
	failRemoval(t, func(string) bool { return !cleaned })

	require.NoError(t, prepareWorkspace(context.Background(), dir, "native", vol, true,
		func() (crt.ContainerRuntime, error) { return rt, nil }, "/host/tools"))

	require.Len(t, rt.created, 1, "the root cleanup container must have run")
	require.Len(t, rt.created[0].Mounts, 2)
	assert.Equal(t, crt.Mount{HostPath: dir, ContainerPath: "/w"}, rt.created[0].Mounts[0])
	assert.Equal(t, "/host/tools", rt.created[0].Mounts[1].HostPath, "the cleanup container carries the shim mount")
	assert.NoFileExists(t, filepath.Join(dir, "rootfile"))
}

func TestPrepareWorkspace_CleanWorkspaceFailureFailsTheRunAndNamesTheVolume(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "rootfile"), "x")
	failRemoval(t, func(string) bool { return true })
	err := prepareWorkspace(context.Background(), dir, "native", testVolume(t), true, noRuntime, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"unity"`)
}

// Removing a link inside a workspace must never reach the volume it points to:
// a native run's volume links are removed with the workspace around them.
func TestWipe_DoesNotFollowALinkIntoAVolume(t *testing.T) {
	target := t.TempDir()
	writeFile(t, filepath.Join(target, "precious.bin"), "x")
	dir := t.TempDir()
	if err := os.Symlink(target, filepath.Join(dir, "cache")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	require.NoError(t, prepareWorkspace(context.Background(), dir, "native", nil, false, noRuntime, ""))
	assert.FileExists(t, filepath.Join(target, "precious.bin"))

	require.NoError(t, os.Symlink(target, filepath.Join(dir, "cache")))
	require.NoError(t, prepareWorkspace(context.Background(), dir, "native", testVolume(t), true, noRuntime, ""))
	assert.FileExists(t, filepath.Join(target, "precious.bin"))
}
