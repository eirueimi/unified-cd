package agent

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

// isolatedBackendWithVolumes builds the host backend an isolated claim gets for
// mounts: the volume references come from the same relativeVolumeMounts the
// claim path calls, so the tests exercise the real mount table.
func isolatedBackendWithVolumes(t *testing.T, m *VolumeManager, mounts ...dsl.VolumeMount) (*hostBackend, string) {
	t.Helper()
	refs, err := relativeVolumeMounts(mounts, m)
	require.NoError(t, err)
	workDir := t.TempDir()
	pod := newClaimPodManager(&podFakeRT{}, workDir, "/workspace", "p", "r", "")
	b := newHostBackend(&Agent{}, "r1", "job", workDir, pod)
	b.volumeRefs = refs
	return b, workDir
}

// On an isolated run a volume is bind-mounted into the container only; the host
// directory at its mount path is an empty mountpoint. Artifact and cache steps
// read and write the host side, so a path inside a mount must be mapped to the
// volume's own host directory, which holds what the container wrote.
func TestResolveArtifactPath_InsideAVolumeMountResolvesToTheVolume(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("go-cache"))
	b, workDir := isolatedBackendWithVolumes(t, m, vmount("go-cache", "gc", "cache"))
	vol := m.PathOf("go-cache")

	for p, want := range map[string]string{
		"cache/out.tar":       filepath.Join(vol, "out.tar"),
		"cache/a/b/c":         filepath.Join(vol, "a", "b", "c"),
		"./cache/out.tar":     filepath.Join(vol, "out.tar"),
		"cache/x/../out.tar":  filepath.Join(vol, "out.tar"),
		"other/../cache/obj/": filepath.Join(vol, "obj"),
	} {
		got, err := b.ResolveArtifactPath(ScopeHandle{}, p)
		require.NoError(t, err, p)
		require.Equal(t, want, got, p)
		require.NotContains(t, got, workDir, "%q must not resolve under the empty host mountpoint", p)
	}
}

func TestResolveCachePath_InsideAVolumeMountResolvesToTheVolume(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("go-cache"))
	b, _ := isolatedBackendWithVolumes(t, m, vmount("go-cache", "gc", ".ucd-cache/go"))

	got, err := b.ResolveCachePath(ScopeHandle{}, ".ucd-cache/go/pkg/mod")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(m.PathOf("go-cache"), "pkg", "mod"), got)
}

func TestResolveArtifactPath_AtAVolumeMountRootResolvesToTheVolume(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("go-cache"))
	b, _ := isolatedBackendWithVolumes(t, m, vmount("go-cache", "gc", ".ucd-cache/go"))

	for _, p := range []string{".ucd-cache/go", ".ucd-cache/go/", "./.ucd-cache/go/../go"} {
		got, err := b.ResolveArtifactPath(ScopeHandle{}, p)
		require.NoError(t, err, p)
		require.Equal(t, m.PathOf("go-cache"), got, p)
	}
}

// Task 1 permits a volume at "." alongside one at a sub-path; a path inside
// both belongs to the deeper mount, as it does inside the container. Nested
// relative mounts follow the same rule.
func TestResolveArtifactPath_LongestMountWins(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("ws"), exclusive("a"), exclusive("b"))
	b, workDir := isolatedBackendWithVolumes(t, m,
		vmount("ws", "ws", "."), vmount("a", "a", "dir"), vmount("b", "b", "dir/inner"))

	got, err := b.ResolveArtifactPath(ScopeHandle{}, "dir/inner/x")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(m.PathOf("b"), "x"), got, "the deepest mount wins")

	got, err = b.ResolveArtifactPath(ScopeHandle{}, "dir/x")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(m.PathOf("a"), "x"), got)

	// The root volume is the workspace itself: nothing to map.
	got, err = b.ResolveArtifactPath(ScopeHandle{}, "plain/x")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(workDir, "plain", "x"), got)
}

func TestResolveArtifactPath_OutsideEveryVolumeIsUnchanged(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("go-cache"))
	b, workDir := isolatedBackendWithVolumes(t, m, vmount("go-cache", "gc", ".ucd-cache/go"))

	// Elsewhere, an ancestor of the mount, a near miss, and the default.
	for p, want := range map[string]string{
		"other/out":         filepath.Join(workDir, "other", "out"),
		".ucd-cache":        filepath.Join(workDir, ".ucd-cache"),
		".ucd-cache/gopher": filepath.Join(workDir, ".ucd-cache", "gopher"),
		"":                  workDir,
	} {
		got, err := b.ResolveArtifactPath(ScopeHandle{}, p)
		require.NoError(t, err, "%q", p)
		require.Equal(t, want, got, "%q", p)
	}

	// A scoped step works in its own container, which mounts no volume.
	got, err := b.ResolveArtifactPath(ScopeHandle{opaque: 1}, ".ucd-cache/go/x")
	require.NoError(t, err)
	require.Equal(t, scopeWorkDir+"/.ucd-cache/go/x", got)

	// A native run reaches the volume through a link on the host: no mapping.
	native := newHostBackend(&Agent{}, "r1", "job", t.TempDir(), nil)
	got, err = native.ResolveArtifactPath(ScopeHandle{}, ".ucd-cache/go/obj")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(native.workDir, ".ucd-cache", "go", "obj"), got)
}

// Widening where a legal path may point must not widen what is legal: a path
// that climbs out of the workspace, including from inside a mount path, is
// refused, and so is an absolute one.
func TestResolveArtifactPath_StillRefusesAnEscape(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("go-cache"))
	b, _ := isolatedBackendWithVolumes(t, m, vmount("go-cache", "gc", "cache"))
	abs := filepath.Join(m.PathOf("go-cache"), "x")

	for _, p := range []string{"..", "../x", "cache/../../x", abs} {
		for kind, resolve := range map[string]func(ScopeHandle, string) (string, error){
			"artifact": b.ResolveArtifactPath, "cache": b.ResolveCachePath,
		} {
			_, err := resolve(ScopeHandle{}, p)
			require.Error(t, err, "%s %q", kind, p)
			require.Contains(t, err.Error(), "escapes the workspace", "%s %q", kind, p)
		}
	}
}
