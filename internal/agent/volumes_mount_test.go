package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// vmount builds a VolumeMount naming one volume.
func vmount(name, as, mountPath string) dsl.VolumeMount {
	return dsl.VolumeMount{Name: dsl.StringOrSlice{name}, As: as, MountPath: mountPath}
}

// absMount returns a POSIX-absolute mountPath together with the host path a
// native run realises it at. On Unix they are the same string, under t's temp
// dir. On Windows a POSIX path such as /dir/cache is rooted at the current
// drive, so the test picks a throwaway top-level directory and removes it.
func absMount(t *testing.T) (mountPath, hostPath string) {
	t.Helper()
	if runtime.GOOS != "windows" {
		hostPath = filepath.Join(t.TempDir(), "cache")
		return filepath.ToSlash(hostPath), hostPath
	}
	cwd, err := os.Getwd()
	require.NoError(t, err)
	drive := filepath.VolumeName(cwd)
	dir := "ucdvol-" + strings.ReplaceAll(t.Name(), "/", "-")
	top := drive + `\` + dir
	if err := os.MkdirAll(top, 0o755); err != nil {
		t.Skipf("cannot create a top-level directory on %s: %v", drive, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(top) })
	return "/" + dir + "/cache", filepath.Join(top, "cache")
}

// resolvesTo reports whether linkPath leads into dir: a file written through
// the link must appear in dir. It checks behaviour rather than Readlink text,
// which differs between symlinks and Windows junctions.
func requireLinkLeadsTo(t *testing.T, linkPath, dir string) {
	t.Helper()
	fi, err := os.Lstat(linkPath)
	require.NoError(t, err)
	require.NotZero(t, fi.Mode()&(os.ModeSymlink|os.ModeIrregular), "%s is not a link", linkPath)
	marker := "probe-" + filepath.Base(t.Name())
	require.NoError(t, os.WriteFile(filepath.Join(linkPath, marker), []byte("x"), 0o644))
	require.FileExists(t, filepath.Join(dir, marker))
	require.NoError(t, os.Remove(filepath.Join(dir, marker)))
}

func requireNotExist(t *testing.T, p string) {
	t.Helper()
	_, err := os.Lstat(p)
	require.True(t, os.IsNotExist(err), "%s should not exist (err=%v)", p, err)
}

func TestMountVolumes_RelativeNative_Symlink(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("go-cache"))
	workDir := t.TempDir()

	resolved, cleanup, err := mountVolumes(workDir, []dsl.VolumeMount{vmount("go-cache", "", ".ucd-cache/go")}, m, true)
	require.NoError(t, err)
	defer func() { _ = cleanup() }()

	link := filepath.Join(workDir, ".ucd-cache", "go")
	requireLinkLeadsTo(t, link, m.PathOf("go-cache"))
	require.Equal(t, map[string]string{"go-cache": link}, resolved)
}

func TestMountVolumes_AbsoluteNative_Symlink(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("go-cache"))
	mountPath, hostPath := absMount(t)

	resolved, cleanup, err := mountVolumes(t.TempDir(), []dsl.VolumeMount{vmount("go-cache", "gc", mountPath)}, m, true)
	require.NoError(t, err)
	defer func() { _ = cleanup() }()

	requireLinkLeadsTo(t, hostPath, m.PathOf("go-cache"))
	require.Equal(t, map[string]string{"gc": hostPath}, resolved)
}

func TestMountVolumes_SameVolumeAtTwoPaths(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("go-cache"))
	workDir := t.TempDir()
	mounts := []dsl.VolumeMount{
		vmount("go-cache", "a", "one"),
		vmount("go-cache", "b", "two/deep"),
	}
	resolved, cleanup, err := mountVolumes(workDir, mounts, m, true)
	require.NoError(t, err)
	requireLinkLeadsTo(t, filepath.Join(workDir, "one"), m.PathOf("go-cache"))
	requireLinkLeadsTo(t, filepath.Join(workDir, "two", "deep"), m.PathOf("go-cache"))
	require.Len(t, resolved, 2)
	require.NoError(t, cleanup())
	requireNotExist(t, filepath.Join(workDir, "one"))
	requireNotExist(t, filepath.Join(workDir, "two", "deep"))
}

func TestMountVolumes_RefusesToClobberExistingPath(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("go-cache"))
	other := t.TempDir() // an operator's own directory the job must not repoint

	t.Run("directory", func(t *testing.T) {
		workDir := t.TempDir()
		target := filepath.Join(workDir, ".ucd-cache", "go")
		require.NoError(t, os.MkdirAll(target, 0o755))
		keep := filepath.Join(target, "precious.txt")
		require.NoError(t, os.WriteFile(keep, []byte("mine"), 0o644))

		_, _, err := mountVolumes(workDir, []dsl.VolumeMount{vmount("go-cache", "", ".ucd-cache/go")}, m, true)
		require.Error(t, err)
		require.Contains(t, err.Error(), target, "the error must name the path")
		require.Contains(t, err.Error(), "not created by this agent")
		require.FileExists(t, keep, "existing content must survive")
	})
	t.Run("regular file", func(t *testing.T) {
		workDir := t.TempDir()
		target := filepath.Join(workDir, "cachefile")
		require.NoError(t, os.WriteFile(target, []byte("mine"), 0o644))
		_, _, err := mountVolumes(workDir, []dsl.VolumeMount{vmount("go-cache", "", "cachefile")}, m, true)
		require.Error(t, err)
		b, rerr := os.ReadFile(target)
		require.NoError(t, rerr)
		require.Equal(t, "mine", string(b))
	})
	t.Run("link to somewhere else", func(t *testing.T) {
		workDir := t.TempDir()
		target := filepath.Join(workDir, "cachelink")
		require.NoError(t, makeDirLink(other, target))
		_, _, err := mountVolumes(workDir, []dsl.VolumeMount{vmount("go-cache", "", "cachelink")}, m, true)
		require.Error(t, err, "a link this agent did not create (it points outside every volume) must not be repointed")
		require.Contains(t, err.Error(), "not created by this agent")
		requireLinkLeadsTo(t, target, other)
	})
	t.Run("stale link of ours is replaced", func(t *testing.T) {
		workDir := t.TempDir()
		target := filepath.Join(workDir, "cachelink")
		require.NoError(t, makeDirLink(m.PathOf("go-cache"), target))
		_, cleanup, err := mountVolumes(workDir, []dsl.VolumeMount{vmount("go-cache", "", "cachelink")}, m, true)
		require.NoError(t, err)
		requireLinkLeadsTo(t, target, m.PathOf("go-cache"))
		require.NoError(t, cleanup())
	})
	t.Run("absolute", func(t *testing.T) {
		mountPath, hostPath := absMount(t)
		require.NoError(t, os.MkdirAll(hostPath, 0o755))
		keep := filepath.Join(hostPath, "precious.txt")
		require.NoError(t, os.WriteFile(keep, []byte("mine"), 0o644))
		_, _, err := mountVolumes(t.TempDir(), []dsl.VolumeMount{vmount("go-cache", "gc", mountPath)}, m, true)
		require.Error(t, err)
		require.Contains(t, err.Error(), hostPath)
		require.FileExists(t, keep)
	})
}

func TestMountVolumes_CleanupRemovesLinks(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("go-cache"), shared("mods"))
	workDir := t.TempDir()
	mountPath, hostPath := absMount(t)
	mounts := []dsl.VolumeMount{
		vmount("go-cache", "", ".ucd-cache/go"),
		vmount("mods", "", mountPath),
	}
	_, cleanup, err := mountVolumes(workDir, mounts, m, true)
	require.NoError(t, err)

	// Data written through a link belongs to the volume and must outlive it.
	require.NoError(t, os.WriteFile(filepath.Join(workDir, ".ucd-cache", "go", "obj"), []byte("o"), 0o644))

	require.NoError(t, cleanup())
	requireNotExist(t, filepath.Join(workDir, ".ucd-cache", "go"))
	requireNotExist(t, hostPath)
	require.FileExists(t, filepath.Join(m.PathOf("go-cache"), "obj"), "cleanup removes links, never volume data")
	require.NoError(t, cleanup(), "cleanup is idempotent")

	// The registry the startup sweep reads has nothing left to sweep.
	require.NoError(t, sweepStaleVolumeLinks(m))
}

func TestSweepStaleVolumeLinks_RemovesOursLeavesTheirs(t *testing.T) {
	m, base := newTestVolumes(t, exclusive("a"), exclusive("b"), exclusive("c"))
	workDir := t.TempDir()
	mountPath, hostPath := absMount(t)
	theirs := t.TempDir()

	// Three mounts that a crash left behind: nobody calls cleanup.
	_, _, err := mountVolumes(workDir, []dsl.VolumeMount{
		vmount("a", "", "rel-a"),
		vmount("b", "", mountPath),
		vmount("c", "", "rel-c"),
	}, m, true)
	require.NoError(t, err)

	// The operator repointed rel-c after the crash: it is no longer ours.
	repointed := filepath.Join(workDir, "rel-c")
	require.NoError(t, os.Remove(repointed))
	require.NoError(t, makeDirLink(theirs, repointed))

	// And a link the agent never created, in a place it never recorded.
	unrelated := filepath.Join(base, "unrelated-link")
	require.NoError(t, makeDirLink(theirs, unrelated))
	// A real directory in the registry's path set must never be removed.
	require.NoError(t, sweepStaleVolumeLinks(m))

	requireNotExist(t, filepath.Join(workDir, "rel-a"))
	requireNotExist(t, hostPath)
	requireLinkLeadsTo(t, repointed, theirs)
	requireLinkLeadsTo(t, unrelated, theirs)
	require.DirExists(t, m.PathOf("a"))
	require.DirExists(t, m.PathOf("b"))

	// A second sweep has nothing to do.
	require.NoError(t, sweepStaleVolumeLinks(m))
	requireLinkLeadsTo(t, repointed, theirs)
}

func TestSweepStaleVolumeLinks_NilManagerAndNoRegistry(t *testing.T) {
	require.NoError(t, sweepStaleVolumeLinks(nil))
	m, _ := newTestVolumes(t, exclusive("a"))
	require.NoError(t, sweepStaleVolumeLinks(m))
}

func TestMountVolumes_RejectsSharedAtWorkspaceRoot(t *testing.T) {
	m, _ := newTestVolumes(t, shared("s"), exclusive("x"))
	for _, native := range []bool{true, false} {
		_, _, err := mountVolumes(t.TempDir(), []dsl.VolumeMount{vmount("s", "", ".")}, m, native)
		require.Error(t, err, "native=%v", native)
		require.Contains(t, err.Error(), "shared")
		require.Contains(t, err.Error(), "workspace root")

		_, cleanup, err := mountVolumes(t.TempDir(), []dsl.VolumeMount{vmount("x", "", ".")}, m, native)
		require.NoError(t, err, "an exclusive volume at the root is the intended use (native=%v)", native)
		require.NoError(t, cleanup())
	}
}

func TestMountVolumes_RootMountIsTheVolumeItself(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("unity"), exclusive("go-cache"))
	mounts := []dsl.VolumeMount{
		vmount("unity", "", "."),
		vmount("go-cache", "", ".ucd-cache/go"),
	}
	// The caller runs the job in the volume directory.
	require.Equal(t, m.PathOf("unity"), workspaceVolumeDir(mounts, m))

	resolved, cleanup, err := mountVolumes(m.PathOf("unity"), mounts, m, true)
	require.NoError(t, err)
	require.Equal(t, m.PathOf("unity"), resolved["unity"], "the root volume needs no link")
	requireLinkLeadsTo(t, filepath.Join(m.PathOf("unity"), ".ucd-cache", "go"), m.PathOf("go-cache"))
	require.NoError(t, cleanup())
	requireNotExist(t, filepath.Join(m.PathOf("unity"), ".ucd-cache", "go"))
	require.DirExists(t, m.PathOf("unity"))

	require.Equal(t, "", workspaceVolumeDir([]dsl.VolumeMount{vmount("go-cache", "", "x")}, m))
}

func TestMountVolumes_NativeAbsolutePathInsideTheWorkspaceIsRejected(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("a"))
	workDir := t.TempDir()
	inside := filepath.ToSlash(filepath.Join(workDir, "sub"))
	if runtime.GOOS == "windows" {
		// A POSIX-rooted path on Windows cannot spell a drive-qualified
		// directory, so reach the workspace through its own rooted form.
		inside = strings.TrimPrefix(inside, filepath.VolumeName(workDir))
	}
	_, _, err := mountVolumes(workDir, []dsl.VolumeMount{vmount("a", "", inside)}, m, true)
	require.Error(t, err)
	require.Contains(t, err.Error(), "inside the workspace")
}

func TestMountVolumes_RequiresAChosenName(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("a"), exclusive("b"))
	_, _, err := mountVolumes(t.TempDir(), []dsl.VolumeMount{{Name: dsl.StringOrSlice{"a", "b"}, As: "x", MountPath: "p"}}, m, true)
	require.Error(t, err)
	_, _, err = mountVolumes(t.TempDir(), []dsl.VolumeMount{vmount("nope", "", "p")}, m, true)
	require.Error(t, err)
	_, _, err = mountVolumes(t.TempDir(), []dsl.VolumeMount{vmount("a", "", "p")}, nil, true)
	require.Error(t, err, "a nil manager declares no volumes")
}

func TestMountVolumes_IsolatedPreparesMountpointsOnly(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("go-cache"))
	workDir := t.TempDir()
	mounts := []dsl.VolumeMount{vmount("go-cache", "", ".ucd-cache/go"), vmount("go-cache", "x", "/abs/in/container")}
	_, cleanup, err := mountVolumes(workDir, mounts, m, false)
	require.NoError(t, err)
	fi, err := os.Lstat(filepath.Join(workDir, ".ucd-cache", "go"))
	require.NoError(t, err)
	require.True(t, fi.IsDir() && fi.Mode()&os.ModeSymlink == 0, "an isolated run mounts in the container; the host mountpoint is a plain directory")
	require.NoError(t, cleanup())
}

func TestIsolatedVolumeMounts(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("unity"), exclusive("go-cache"))
	root, extra, resolved, err := isolatedVolumeMounts("/workspace", []dsl.VolumeMount{
		vmount("unity", "", "."),
		vmount("go-cache", "gc", ".ucd-cache/go"),
		vmount("go-cache", "abs", "/root/.cache"),
	}, m)
	require.NoError(t, err)
	require.Equal(t, m.PathOf("unity"), root)
	require.Len(t, extra, 2)
	require.Equal(t, m.PathOf("go-cache"), extra[0].HostPath)
	require.Equal(t, "/workspace/.ucd-cache/go", extra[0].ContainerPath)
	require.Equal(t, "/root/.cache", extra[1].ContainerPath)
	require.Equal(t, map[string]string{"unity": "/workspace", "gc": "/workspace/.ucd-cache/go", "abs": "/root/.cache"}, resolved)

	_, _, _, err = isolatedVolumeMounts("/workspace", []dsl.VolumeMount{vmount("nope", "", "x")}, m)
	require.Error(t, err)
}

func TestClaimPod_MountsNamedVolumesOnEveryWorkspaceContainer(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("unity"), exclusive("go-cache"))
	f := &podFakeRT{}
	pod := newClaimPodManager(f, m.PathOf("unity"), "/workspace", "pause:img", "runner:img", "/host/tools")
	_, extra, _, err := isolatedVolumeMounts("/workspace", []dsl.VolumeMount{
		vmount("unity", "", "."),
		vmount("go-cache", "gc", ".ucd-cache/go"),
	}, m)
	require.NoError(t, err)
	pod.volumeMounts = extra
	require.NoError(t, pod.Start(context.Background(), mysqlTemplate()))

	require.Len(t, f.created, 3) // pause, mysql, injected "job"
	require.Len(t, f.created[0].Mounts, 1, "the pause container mounts only /.ucd")
	for _, spec := range f.created[1:] {
		require.Len(t, spec.Mounts, 3)
		require.Equal(t, m.PathOf("unity"), spec.Mounts[0].HostPath, "the root volume is the workspace mount")
		require.Equal(t, "/workspace", spec.Mounts[0].ContainerPath)
		require.Equal(t, m.PathOf("go-cache"), spec.Mounts[1].HostPath)
		require.Equal(t, "/workspace/.ucd-cache/go", spec.Mounts[1].ContainerPath)
		require.Equal(t, "/.ucd", spec.Mounts[2].ContainerPath)
	}
}

// On an isolated run a volume is bind-mounted into the container only. The
// host directory at its mount path is an empty mountpoint, and artifact and
// cache steps read and write the host side, so a path inside a mount would
// archive nothing and save nothing while the run goes green. It must fail loudly.
func TestHostBackend_IsolatedPathInsideVolumeMountIsRejected(t *testing.T) {
	refs := []volumeMountRef{{Volume: "go-cache", MountPath: ".ucd-cache/go"}}
	pod := newClaimPodManager(&podFakeRT{}, "/w", "/workspace", "p", "r", "")
	isolated := newHostBackend(&Agent{}, "r1", "job", t.TempDir(), pod)
	isolated.volumeRefs = refs

	resolvers := map[string]func(ScopeHandle, string) (string, error){
		"artifact": isolated.ResolveArtifactPath,
		"cache":    isolated.ResolveCachePath,
	}
	for kind, resolve := range resolvers {
		for _, p := range []string{".ucd-cache/go", ".ucd-cache/go/obj", "./.ucd-cache/go/../go/x", ".ucd-cache/go/"} {
			_, err := resolve(ScopeHandle{}, p)
			require.Error(t, err, "%s path %q", kind, p)
			require.Contains(t, err.Error(), p, "the error names the path")
			require.Contains(t, err.Error(), `"go-cache"`, "the error names the volume")
			require.Contains(t, err.Error(), ".ucd-cache/go", "the error names the mount path")
		}
		// Outside the mount, an ancestor of it, a near miss, and the default.
		for _, p := range []string{"other/out", ".ucd-cache", ".ucd-cache/gopher", ""} {
			_, err := resolve(ScopeHandle{}, p)
			require.NoError(t, err, "%s path %q", kind, p)
		}
		// A scoped step works in its own container, not on the host mountpoint.
		_, err := resolve(ScopeHandle{opaque: 1}, ".ucd-cache/go/x")
		require.NoError(t, err)
	}

	// A native run reaches the volume through a link on the host: no problem.
	native := newHostBackend(&Agent{}, "r1", "job", t.TempDir(), nil)
	native.volumeRefs = refs
	_, err := native.ResolveArtifactPath(ScopeHandle{}, ".ucd-cache/go/obj")
	require.NoError(t, err)
}

func TestExecuteRun_Isolated_ArtifactAndCachePathsInsideAVolumeMountFailTheRun(t *testing.T) {
	steps := map[string]*api.ClaimStep{
		"upload-artifact": {Index: 0, Name: "up", UploadArtifact: &api.UploadArtifactStep{Name: "out", Path: ".ucd-cache/go/out"}},
		"cache":           {Index: 0, Name: "c", Cache: &dsl.CacheStep{Path: ".ucd-cache/go", Key: "k"}},
	}
	for kind, step := range steps {
		t.Run(kind, func(t *testing.T) {
			const agentID = "iso-vol-agent"
			h := newIsolatedHarness()
			srv := newIsolatedServer(t, agentID, h)
			m, _ := newTestVolumes(t, exclusive("go-cache"))
			f := &podFakeRT{}
			a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok"), PauseImage: "pause:img", RunnerImage: "runner:img", Volumes: m}
			a.runtimeOnce.Do(func() {})
			a.resolvedRuntime = f

			var buf bytes.Buffer
			defer captureSlog(&buf)()
			a.executeRun(context.Background(), api.ClaimResponse{
				RunID: "run-iso-vol", JobName: "j", PodTemplate: mysqlTemplate(),
				Volumes: []dsl.VolumeMount{vmount("go-cache", "gc", ".ucd-cache/go")},
				Stages:  []api.ClaimStage{{Step: step}},
			}, t.TempDir())

			h.mu.Lock()
			defer h.mu.Unlock()
			require.Equal(t, "Failed", h.finishStatus, "a green run would read as having saved the files")
			out := buf.String()
			require.Contains(t, out, ".ucd-cache/go")
			require.Contains(t, out, "go-cache")
			require.Contains(t, out, "bind-mounted")
		})
	}
}

// --- reservation ---

func TestReserveRunVolumes_PicksFreeCandidateAndNamesIt(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("c1"), exclusive("c2"))
	cand := dsl.VolumeMount{Name: dsl.StringOrSlice{"c1", "c2"}, As: "cache", MountPath: "p"}

	c1, rel1, err := reserveRunVolumes(context.Background(), m, "r1", []dsl.VolumeMount{cand}, true, nil)
	require.NoError(t, err)
	require.Equal(t, dsl.StringOrSlice{"c1"}, c1[0].Name)
	require.Equal(t, "cache", c1[0].As)
	require.Equal(t, "p", c1[0].MountPath)
	require.Equal(t, dsl.StringOrSlice{"c1", "c2"}, cand.Name, "the claim's own mounts are not mutated")

	c2, rel2, err := reserveRunVolumes(context.Background(), m, "r2", []dsl.VolumeMount{cand}, true, nil)
	require.NoError(t, err)
	require.Equal(t, dsl.StringOrSlice{"c2"}, c2[0].Name, "the second run takes the other candidate instead of waiting")
	rel1()
	rel2()
	require.Empty(t, m.Busy())
}

func TestReserveRunVolumes_SameVolumeTwiceIsHeldUntilTheRunEnds(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("a"))
	mounts := []dsl.VolumeMount{vmount("a", "x", "one"), vmount("a", "y", "two")}
	chosen, release, err := reserveRunVolumes(context.Background(), m, "r1", mounts, true, nil)
	require.NoError(t, err)
	require.Equal(t, dsl.StringOrSlice{"a"}, chosen[1].Name)
	require.Equal(t, []string{"a"}, m.Busy())

	waiter := make(chan error, 1)
	go func() {
		_, rel, err := reserveRunVolumes(context.Background(), m, "r2", []dsl.VolumeMount{vmount("a", "", "p")}, true, nil)
		if rel != nil {
			rel()
		}
		waiter <- err
	}()
	select {
	case <-waiter:
		t.Fatal("another run took the volume while the first run still holds it")
	case <-time.After(150 * time.Millisecond):
	}
	release()
	select {
	case err := <-waiter:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter never got the volume after the run released it")
	}
}

// Two entries whose candidate lists differ must not collapse onto one volume:
// a job author who wrote two caches expects two.
func TestReserveRunVolumes_DifferentCandidateSetsGetDistinctVolumes(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("c1"), exclusive("c2"))
	wide := dsl.VolumeMount{Name: dsl.StringOrSlice{"c1", "c2"}, As: "wide", MountPath: "p1"}
	narrow := vmount("c1", "", "p2")
	for name, mounts := range map[string][]dsl.VolumeMount{"wide first": {wide, narrow}, "narrow first": {narrow, wide}} {
		t.Run(name, func(t *testing.T) {
			chosen, release, err := reserveRunVolumes(context.Background(), m, "r1", mounts, true, nil)
			require.NoError(t, err)
			defer release()
			got := map[string]string{}
			for _, c := range chosen {
				got[c.MountPath] = c.Name[0]
			}
			require.Equal(t, map[string]string{"p1": "c2", "p2": "c1"}, got, "[c1] is constrained to c1, so [c1,c2] must take c2")
		})
	}

	// The same list twice also yields two volumes while two are available.
	chosen, release, err := reserveRunVolumes(context.Background(), m, "r2", []dsl.VolumeMount{
		{Name: dsl.StringOrSlice{"c1", "c2"}, As: "a", MountPath: "p1"},
		{Name: dsl.StringOrSlice{"c1", "c2"}, As: "b", MountPath: "p2"},
	}, true, nil)
	require.NoError(t, err)
	defer release()
	require.NotEqual(t, chosen[0].Name[0], chosen[1].Name[0])
}

// The legal aliasing case: nothing else to choose from, so one volume is
// mounted at both paths, held once.
func TestReserveRunVolumes_IdenticalSingleCandidateIsOneVolume(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("c1"), exclusive("c2"))
	chosen, release, err := reserveRunVolumes(context.Background(), m, "r1", []dsl.VolumeMount{vmount("c1", "x", "p1"), vmount("c1", "y", "p2")}, true, nil)
	require.NoError(t, err)
	require.Equal(t, dsl.StringOrSlice{"c1"}, chosen[0].Name)
	require.Equal(t, dsl.StringOrSlice{"c1"}, chosen[1].Name)
	require.Equal(t, []string{"c1"}, m.Busy())
	release()
	require.Empty(t, m.Busy())

	// A candidate this agent does not declare is not an alternative.
	chosen, release, err = reserveRunVolumes(context.Background(), m, "r1", []dsl.VolumeMount{
		{Name: dsl.StringOrSlice{"c1", "undeclared"}, As: "x", MountPath: "p1"}, vmount("c1", "y", "p2"),
	}, true, nil)
	require.NoError(t, err)
	defer release()
	require.Equal(t, dsl.StringOrSlice{"c1"}, chosen[0].Name)
}

// When the only unchosen candidate is busy the run waits for it; it does not
// fall back to a volume it already holds for the other entry.
func TestReserveRunVolumes_WaitsForAnUnchosenCandidateInsteadOfAliasing(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("c1"), exclusive("c2"))
	_, holdC2, err := m.Reserve("other", []string{"c2"})
	require.NoError(t, err)

	type result struct {
		chosen  []dsl.VolumeMount
		release func()
		err     error
	}
	got := make(chan result, 1)
	go func() {
		c, rel, err := reserveRunVolumes(context.Background(), m, "r1", []dsl.VolumeMount{
			{Name: dsl.StringOrSlice{"c1", "c2"}, As: "wide", MountPath: "p1"}, vmount("c1", "", "p2"),
		}, true, nil)
		got <- result{c, rel, err}
	}()
	select {
	case r := <-got:
		t.Fatalf("aliased instead of waiting: %+v (err %v)", r.chosen, r.err)
	case <-time.After(200 * time.Millisecond):
	}
	holdC2()
	select {
	case r := <-got:
		require.NoError(t, r.err)
		require.Equal(t, "c2", r.chosen[0].Name[0])
		require.Equal(t, "c1", r.chosen[1].Name[0])
		r.release()
	case <-time.After(5 * time.Second):
		t.Fatal("never got the volume after it was released")
	}
}

// A failed unlink of an absolute path leaves a machine-wide stale link. Another
// volume's run handed that path would silently repoint it, so the path stays
// reserved for the life of the process. A relative link is per-run and the next
// mount replaces a stale link it owns, so nothing is held back for it.
func TestMountVolumes_FailedUnlinkOfAnAbsolutePathKeepsItsReservation(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("a"), exclusive("b"))
	mountPath, _ := absMount(t)
	m.removeLink = func(string) error { return errors.New("injected unlink failure") }

	chosen, release, err := reserveRunVolumes(context.Background(), m, "r1", []dsl.VolumeMount{vmount("a", "g", mountPath), vmount("a", "rel", "sub")}, true, nil)
	require.NoError(t, err)
	_, cleanup, err := mountVolumes(t.TempDir(), chosen, m, true)
	require.NoError(t, err)
	require.Error(t, cleanup(), "the failure is reported")
	release()

	require.Equal(t, []string{path.Clean(mountPath)}, m.Busy(), "volume a is free again, the absolute path is not")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, _, err = reserveRunVolumes(ctx, m, "r2", []dsl.VolumeMount{vmount("b", "g", mountPath)}, true, nil)
	require.ErrorIs(t, err, context.DeadlineExceeded, "another run must not be handed a path whose link is still there")

	// The same failure on a relative path pins nothing.
	m2, _ := newTestVolumes(t, exclusive("a"))
	m2.removeLink = func(string) error { return errors.New("injected unlink failure") }
	c2, rel2, err := reserveRunVolumes(context.Background(), m2, "r1", []dsl.VolumeMount{vmount("a", "", "sub")}, true, nil)
	require.NoError(t, err)
	_, cleanup2, err := mountVolumes(t.TempDir(), c2, m2, true)
	require.NoError(t, err)
	require.Error(t, cleanup2())
	rel2()
	require.Empty(t, m2.Busy())
}

func TestMountVolumes_AbsoluteNativePathIsReservedMachineWide(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("a"), exclusive("b"))
	mountPath, hostPath := absMount(t)

	// Run 1 mounts volume a at the absolute path.
	c1, rel1, err := reserveRunVolumes(context.Background(), m, "r1", []dsl.VolumeMount{vmount("a", "g", mountPath)}, true, nil)
	require.NoError(t, err)
	_, cleanup1, err := mountVolumes(t.TempDir(), c1, m, true)
	require.NoError(t, err)
	require.Contains(t, m.Busy(), path.Clean(mountPath), "claim-time Busy reports the absolute path")

	// Run 2 wants the same path for a different volume. Volume b is free, so
	// only the machine-wide path can hold it back.
	got := make(chan error, 1)
	var c2 []dsl.VolumeMount
	var rel2 func()
	go func() {
		var err error
		c2, rel2, err = reserveRunVolumes(context.Background(), m, "r2", []dsl.VolumeMount{vmount("b", "g", mountPath)}, true, nil)
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("the second native run did not wait for the absolute path (err=%v)", err)
	case <-time.After(150 * time.Millisecond):
	}
	// It has not repointed the link.
	requireLinkLeadsTo(t, hostPath, m.PathOf("a"))

	require.NoError(t, cleanup1())
	rel1()
	select {
	case err := <-got:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the second run stayed blocked after the first released the path")
	}
	_, cleanup2, err := mountVolumes(t.TempDir(), c2, m, true)
	require.NoError(t, err)
	requireLinkLeadsTo(t, hostPath, m.PathOf("b"))
	require.NoError(t, cleanup2())
	rel2()
}

func TestReserveRunVolumes_IsolatedAbsolutePathIsNotMachineWide(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("a"), exclusive("b"))
	// An absolute mountPath on an isolated run is a container path: two runs
	// may use the same one with different volumes.
	_, rel1, err := reserveRunVolumes(context.Background(), m, "r1", []dsl.VolumeMount{vmount("a", "g", "/root/.cache")}, false, nil)
	require.NoError(t, err)
	_, rel2, err := reserveRunVolumes(context.Background(), m, "r2", []dsl.VolumeMount{vmount("b", "g", "/root/.cache")}, false, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, m.Busy())
	rel1()
	rel2()
}

func TestReserveRunVolumes_CancelledWaitReleasesWhatItAcquired(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("a"), exclusive("b"))
	_, holdB, err := m.Reserve("other", []string{"b"})
	require.NoError(t, err)
	defer holdB()

	ctx, cancel := context.WithCancel(context.Background())
	var waited atomic.Bool
	got := make(chan error, 1)
	go func() {
		_, _, err := reserveRunVolumes(ctx, m, "r1", []dsl.VolumeMount{vmount("a", "", "x"), vmount("b", "", "y")}, true, func(string) { waited.Store(true) })
		got <- err
	}()
	require.Eventually(t, waited.Load, 5*time.Second, 10*time.Millisecond, "the wait hook fires when the run starts to wait")
	cancel()
	select {
	case err := <-got:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling did not end the wait")
	}
	require.Equal(t, []string{"b"}, m.Busy(), "volume a, taken before the wait, is released again")
}

// Two runs naming the same two exclusive volumes in opposite orders must not
// deadlock on each other: each would otherwise hold its first and wait for the
// one the other holds.
func TestReserveRunVolumes_OppositeOrderDoesNotDeadlock(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("a"), exclusive("b"))
	ab := []dsl.VolumeMount{vmount("a", "", "pa"), vmount("b", "", "pb")}
	ba := []dsl.VolumeMount{vmount("b", "", "pb"), vmount("a", "", "pa")}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 300; i++ {
			var wg sync.WaitGroup
			start := make(chan struct{})
			for r, mounts := range [][]dsl.VolumeMount{ab, ba} {
				wg.Add(1)
				go func(id string, mounts []dsl.VolumeMount) {
					defer wg.Done()
					<-start
					_, rel, err := reserveRunVolumes(context.Background(), m, id, mounts, true, nil)
					if err != nil {
						t.Error(err)
						return
					}
					rel()
				}("r"+string(rune('0'+r)), mounts)
			}
			close(start)
			wg.Wait()
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("two runs naming the same volumes in opposite orders deadlocked")
	}
	require.Empty(t, m.Busy())
}

// --- the claim loop ---

type finishRecord struct {
	runID  string
	status string
}

// volumeLoopServer serves a scripted sequence of claims for agent id "v1" and
// records step and finish calls per run. claimN is called with the zero-based
// claim index and returns the response, or nil to hold the claim open.
type volumeLoopServer struct {
	srv       *httptest.Server
	mu        sync.Mutex
	stepRuns  map[string]int
	finishes  []finishRecord
	claimBody []api.ClaimRequest
	finished  chan finishRecord
	stepSeen  chan string
	// served counts claim responses that carried a run.
	served atomic.Int32
	// registered is the last registration request the agent sent.
	registered api.AgentRegisterRequest
	// runStatus overrides what GET /runs/{id} reports (default Running).
	runStatus map[string]api.RunStatus
	// logs are the lines the agent shipped into runs' logs (bulk endpoint).
	logs []api.LogAppendRequest
}

func newVolumeLoopServer(t *testing.T, claimN func(n int, stepSeen <-chan string) *api.ClaimResponse) *volumeLoopServer {
	t.Helper()
	s := &volumeLoopServer{
		stepRuns: map[string]int{},
		finished: make(chan finishRecord, 16),
		stepSeen: make(chan string, 64),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		var req api.AgentRegisterRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.registered = req
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	var claims atomic.Int32
	mux.HandleFunc("POST /api/v1/agents/v1/claim", func(w http.ResponseWriter, r *http.Request) {
		var body api.ClaimRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.claimBody = append(s.claimBody, body)
		s.mu.Unlock()
		n := int(claims.Add(1)) - 1
		w.Header().Set("Content-Type", "application/json")
		resp := claimN(n, s.stepSeen)
		if resp == nil {
			select {
			case <-r.Context().Done():
			case <-time.After(30 * time.Second):
			}
			resp = &api.ClaimResponse{}
		}
		_ = json.NewEncoder(w).Encode(resp)
		if resp.RunID != "" {
			s.served.Add(1)
		}
	})
	mux.HandleFunc("GET /api/v1/runs/{run}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		st, ok := s.runStatus[r.PathValue("run")]
		s.mu.Unlock()
		if !ok {
			st = api.RunRunning
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Run{ID: r.PathValue("run"), Status: st})
	})
	mux.HandleFunc("POST /api/v1/agents/v1/runs/{run}/steps/{idx}/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		var lines []api.LogAppendRequest
		_ = json.NewDecoder(r.Body).Decode(&lines)
		s.mu.Lock()
		s.logs = append(s.logs, lines...)
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/v1/agents/v1/steps", func(w http.ResponseWriter, r *http.Request) {
		var body api.StepReportRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.stepRuns[body.RunID]++
		s.mu.Unlock()
		select {
		case s.stepSeen <- body.RunID:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/v1/runs/{run}/finish", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Status string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		rec := finishRecord{runID: r.PathValue("run"), status: body.Status}
		s.mu.Lock()
		s.finishes = append(s.finishes, rec)
		s.mu.Unlock()
		s.finished <- rec
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/logs/bulk") || strings.HasSuffix(r.URL.Path, "/logs") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *volumeLoopServer) stepCount(runID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stepRuns[runID]
}

func volumeClaim(runID, script string, mounts ...dsl.VolumeMount) *api.ClaimResponse {
	c := claimResp(runID, script)
	c.Volumes = mounts
	return &c
}

func waitFinish(t *testing.T, s *volumeLoopServer, runID string) finishRecord {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case rec := <-s.finished:
			if rec.runID == runID {
				return rec
			}
		case <-deadline:
			t.Fatalf("run %s never finished", runID)
		}
	}
}

func volumeAgent(t *testing.T, s *volumeLoopServer, slots int, vols ...string) (*Agent, string) {
	t.Helper()
	ws := t.TempDir()
	var cfgs []config.VolumeConfig
	for _, v := range vols {
		cfgs = append(cfgs, exclusive(v))
	}
	m, err := NewVolumeManager(ws, cfgs)
	require.NoError(t, err)
	return &Agent{
		ID:                    "v1",
		Client:                NewClient(s.srv.URL, "tok"),
		MaxConcurrent:         slots,
		MaxDetachedConcurrent: -1,
		WorkspaceDir:          ws,
		Volumes:               m,
	}, ws
}

func TestRunLoop_SecondSlotWaitsForExclusiveVolume(t *testing.T) {
	barrier := filepath.Join(t.TempDir(), "barrier")
	require.NoError(t, os.WriteFile(barrier, []byte("hold"), 0o644))
	mount := vmount("unity", "", ".ucd-cache/lib")

	srv := newVolumeLoopServer(t, func(n int, stepSeen <-chan string) *api.ClaimResponse {
		switch n {
		case 0:
			return volumeClaim("run-1", "while [ -f "+filepath.ToSlash(barrier)+" ]; do sleep 0.01; done", mount)
		case 1:
			// Hand the second slot its run only once run-1 holds the volume,
			// so which run wins is not left to goroutine scheduling.
			select {
			case <-stepSeen:
			case <-time.After(20 * time.Second):
			}
			return volumeClaim("run-2", "echo second", mount)
		}
		return nil
	})
	a, ws := volumeAgent(t, srv, 2, "unity")

	claimCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(claimCtx) }()

	// While run-1 holds the exclusive volume, run-2 has been claimed but must
	// neither mount nor run.
	require.Eventually(t, func() bool { return len(a.Volumes.Busy()) == 1 && srv.served.Load() >= 2 }, 20*time.Second, 10*time.Millisecond)
	time.Sleep(400 * time.Millisecond)
	links, _ := filepath.Glob(filepath.Join(ws, "working*", "test", ".ucd-cache", "lib"))
	require.Len(t, links, 1, "only the holder has mounted the volume")
	require.Equal(t, 0, srv.stepCount("run-2"), "the loser must not run while the holder is running")

	require.NoError(t, os.Remove(barrier))
	require.Equal(t, "Succeeded", waitFinish(t, srv, "run-1").status)
	require.Equal(t, "Succeeded", waitFinish(t, srv, "run-2").status)
	require.Positive(t, srv.stepCount("run-2"), "once the holder is done the waiter runs")
	// The run reports its result before the claim loop unwinds and releases.
	require.Eventually(t, func() bool { return len(a.Volumes.Busy()) == 0 }, 10*time.Second, 10*time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return")
	}
	left, _ := filepath.Glob(filepath.Join(ws, "working*", "test", ".ucd-cache", "lib"))
	require.Empty(t, left, "links are removed when the runs end")
}

func TestRunLoop_VolumeWaitIsAbandonedOnDrain(t *testing.T) {
	barrier := filepath.Join(t.TempDir(), "barrier")
	require.NoError(t, os.WriteFile(barrier, []byte("hold"), 0o644))
	mount := vmount("unity", "", ".ucd-cache/lib")

	srv := newVolumeLoopServer(t, func(n int, stepSeen <-chan string) *api.ClaimResponse {
		switch n {
		case 0:
			return volumeClaim("run-1", "while [ -f "+filepath.ToSlash(barrier)+" ]; do sleep 0.01; done", mount)
		case 1:
			select {
			case <-stepSeen:
			case <-time.After(20 * time.Second):
			}
			return volumeClaim("run-2", "echo never", mount)
		}
		return nil
	})
	a, _ := volumeAgent(t, srv, 2, "unity")
	// No DrainTimeout: the run context is not cancelled by the drain, so only
	// the claim context (the drain itself) can break the wait.

	claimCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(claimCtx) }()

	require.Eventually(t, func() bool { return len(a.Volumes.Busy()) == 1 }, 20*time.Second, 10*time.Millisecond)
	// Wait until run-2 is claimed and is therefore blocked on the volume.
	require.Eventually(t, func() bool { return srv.served.Load() >= 2 }, 20*time.Second, 10*time.Millisecond)
	time.Sleep(300 * time.Millisecond)

	cancel() // begin the drain; run-1 is still running

	rec := waitFinish(t, srv, "run-2")
	require.Equal(t, "Failed", rec.status, "the abandoned run reports why it did not run")
	require.Equal(t, 0, srv.stepCount("run-2"), "an abandoned run must not proceed unmounted")
	require.FileExists(t, barrier, "run-1 is still running: the wait ended without waiting for the holder")
	require.Equal(t, []string{"unity"}, a.Volumes.Busy())

	require.NoError(t, os.Remove(barrier))
	require.Equal(t, "Succeeded", waitFinish(t, srv, "run-1").status)
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return after the drain")
	}
	require.Empty(t, a.Volumes.Busy())
}

// A user cancelling a run that is waiting for a volume ends the wait too. The
// run is already terminal on the controller, so the agent must not report a
// result for it (that would race the cancel), and the slot is free again.
func TestRunLoop_VolumeWaitEndsWhenTheRunIsCancelled(t *testing.T) {
	prev := CancelPollInterval
	CancelPollInterval = 30 * time.Millisecond
	defer func() { CancelPollInterval = prev }()

	barrier := filepath.Join(t.TempDir(), "barrier")
	require.NoError(t, os.WriteFile(barrier, []byte("hold"), 0o644))
	mount := vmount("unity", "", ".ucd-cache/lib")

	srv := newVolumeLoopServer(t, func(n int, stepSeen <-chan string) *api.ClaimResponse {
		switch n {
		case 0:
			return volumeClaim("run-1", "while [ -f "+filepath.ToSlash(barrier)+" ]; do sleep 0.01; done", mount)
		case 1:
			select {
			case <-stepSeen:
			case <-time.After(20 * time.Second):
			}
			return volumeClaim("run-2", "echo never", mount)
		}
		return nil
	})
	a, _ := volumeAgent(t, srv, 2, "unity")
	claimCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(claimCtx) }()

	require.Eventually(t, func() bool { return srv.served.Load() >= 2 }, 20*time.Second, 10*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	srv.mu.Lock()
	claimsBefore := len(srv.claimBody)
	srv.runStatus = map[string]api.RunStatus{"run-2": api.RunCancelled}
	srv.mu.Unlock()

	// The waiting slot gives up and goes back to claiming while run-1 still holds the volume.
	require.Eventually(t, func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return len(srv.claimBody) > claimsBefore
	}, 20*time.Second, 10*time.Millisecond, "the cancelled run must stop waiting")
	require.FileExists(t, barrier)
	require.Equal(t, []string{"unity"}, a.Volumes.Busy())
	require.Equal(t, 0, srv.stepCount("run-2"))
	srv.mu.Lock()
	for _, f := range srv.finishes {
		require.NotEqual(t, "run-2", f.runID, "a run the controller already finished is not finished again")
	}
	srv.mu.Unlock()

	require.NoError(t, os.Remove(barrier))
	waitFinish(t, srv, "run-1")
	cancel()
	<-done
}

// A job that asked for a clean workspace and got a volume instead must be told
// in the run's own log: the agent's process log is not where its author looks.
func TestRunLoop_CleanWorkspaceOnAVolumeWorkspaceIsReportedInTheRunLog(t *testing.T) {
	srv := newVolumeLoopServer(t, func(n int, _ <-chan string) *api.ClaimResponse {
		if n == 0 {
			return volumeClaim("run-1", "echo hi", vmount("unity", "", "."))
		}
		return nil
	})
	a, _ := volumeAgent(t, srv, 1, "unity")
	a.CleanWorkspace = true
	claimCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(claimCtx) }()
	require.Equal(t, "Succeeded", waitFinish(t, srv, "run-1").status)
	cancel()
	<-done

	srv.mu.Lock()
	defer srv.mu.Unlock()
	var found bool
	for _, l := range srv.logs {
		if l.RunID == "run-1" && l.StepIndex == -1 && strings.Contains(l.Line, "cleanWorkspace") && strings.Contains(l.Line, "not applied") {
			found = true
		}
	}
	require.True(t, found, "the run's System log must say the requested clean was not performed; got %+v", srv.logs)
}

func TestRunLoop_ClaimReportsBusyVolumes(t *testing.T) {
	barrier := filepath.Join(t.TempDir(), "barrier")
	require.NoError(t, os.WriteFile(barrier, []byte("hold"), 0o644))
	mount := vmount("unity", "", ".ucd-cache/lib")

	srv := newVolumeLoopServer(t, func(n int, stepSeen <-chan string) *api.ClaimResponse {
		switch n {
		case 0:
			return volumeClaim("run-1", "while [ -f "+filepath.ToSlash(barrier)+" ]; do sleep 0.01; done", mount)
		case 1:
			// An empty answer sends the second slot straight back to claim,
			// now that run-1 holds the volume.
			select {
			case <-stepSeen:
			case <-time.After(20 * time.Second):
			}
			return &api.ClaimResponse{}
		}
		return nil
	})
	a, _ := volumeAgent(t, srv, 2, "unity")
	claimCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(claimCtx) }()

	// The second slot claims while run-1 holds the volume and must say so.
	require.Eventually(t, func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		for _, b := range srv.claimBody {
			for _, v := range b.BusyVolumes {
				if v == "unity" {
					return true
				}
			}
		}
		return false
	}, 20*time.Second, 20*time.Millisecond, "a claim made while the volume is held must report it busy")

	require.NoError(t, os.Remove(barrier))
	waitFinish(t, srv, "run-1")
	cancel()
	<-done
}

func TestRunLoop_RunWithVolumesOnAnAgentWithoutAnyFails(t *testing.T) {
	srv := newVolumeLoopServer(t, func(n int, _ <-chan string) *api.ClaimResponse {
		if n == 0 {
			return volumeClaim("run-1", "echo hi", vmount("unity", "", "p"))
		}
		return nil
	})
	a, _ := volumeAgent(t, srv, 1)
	a.Volumes = nil
	claimCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = a.Run(claimCtx) }()
	require.Equal(t, "Failed", waitFinish(t, srv, "run-1").status)
	require.Equal(t, 0, srv.stepCount("run-1"))
}

func TestRun_AdvertisesVolumesAndSweepsStaleLinksBeforeClaiming(t *testing.T) {
	srv := newVolumeLoopServer(t, func(int, <-chan string) *api.ClaimResponse { return nil })
	a, ws := volumeAgent(t, srv, 1, "unity", "go-cache")

	// A link a crashed predecessor left behind.
	stale := filepath.Join(ws, "stale-link")
	_, _, err := mountVolumes(ws, []dsl.VolumeMount{vmount("unity", "", "stale-link")}, a.Volumes, true)
	require.NoError(t, err)
	requireLinkLeadsTo(t, stale, a.Volumes.PathOf("unity"))

	claimCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(claimCtx) }()
	require.Eventually(t, func() bool {
		_, err := os.Lstat(stale)
		return os.IsNotExist(err)
	}, 20*time.Second, 20*time.Millisecond, "the startup sweep removes the stale link")
	cancel()
	<-done

	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Equal(t, []string{"go-cache", "unity"}, srv.registered.Volumes)
}
