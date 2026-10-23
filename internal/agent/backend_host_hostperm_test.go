package agent

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/artifact"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// copyOutRT is a fake runtime that records CopyOut calls and materialises the
// copied path as a directory holding one file, like `docker cp` would.
type copyOutRT struct {
	podFakeRT
	copyOuts []struct{ from, to string }
}

func (f *copyOutRT) CopyOut(_ context.Context, _ crt.ContainerHandle, containerPath, hostPath string) error {
	f.copyOuts = append(f.copyOuts, struct{ from, to string }{containerPath, hostPath})
	if err := os.MkdirAll(hostPath, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(hostPath, "from-pause.txt"), []byte("x"), 0o644)
}

func permPT(podSC map[string]any, containerSC map[string]any) *dsl.PodTemplate {
	c := map[string]any{"name": "app", "image": "busybox"}
	if containerSC != nil {
		c["securityContext"] = containerSC
	}
	spec := map[string]any{"containers": []any{c}}
	if podSC != nil {
		spec["securityContext"] = podSC
	}
	return &dsl.PodTemplate{Spec: spec}
}

// podBackend returns an isolated hostBackend whose claim pod runs on rt.
func podBackend(t *testing.T, a *Agent, rt crt.ContainerRuntime, pt *dsl.PodTemplate) (*hostBackend, string) {
	t.Helper()
	workDir := t.TempDir()
	m := newClaimPodManager(rt, workDir, "/workspace", "pause:img", "runner:img", "/host/tools")
	require.NoError(t, m.Start(context.Background(), pt))
	return newHostBackend(a, "r1", "test-job", workDir, m), workDir
}

// chownExecs returns the chown-as invocations as "owner path" strings.
func chownExecs(f *podFakeRT) []string {
	var out []string
	for _, s := range f.execSpecs {
		if len(s.Shell) >= 2 && s.Shell[1] == "chown-as" {
			out = append(out, strings.Join(s.Shell[2:], " ")+" "+s.Script)
		}
	}
	return out
}

func downloadAgent(t *testing.T) *Agent {
	t.Helper()
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("a"), 0o644))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/runs/r1/artifacts/art", func(w http.ResponseWriter, r *http.Request) {
		rc := artifact.StreamTarZstdExcluding(src, nil)
		defer rc.Close()
		_, _ = io.Copy(w, rc)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Agent{ID: "a1", Client: NewClient(srv.URL, "tok"), CacheStore: objectstore.NewLocalObjectStore(t.TempDir())}
}

func TestHostBackend_Download_ChownsAsThePodLevelIdentity(t *testing.T) {
	f := &podFakeRT{}
	b, workDir := podBackend(t, downloadAgent(t), f, permPT(map[string]any{"runAsUser": 1000, "runAsGroup": 2000, "fsGroup": 2000}, nil))

	dest := filepath.Join(workDir, "out")
	require.NoError(t, b.DownloadArtifact(context.Background(), ScopeHandle{}, "r1", "art", dest))

	assert.FileExists(t, filepath.Join(dest, "a.txt"))
	assert.Equal(t, []string{"1000:2000 /workspace/out"}, chownExecs(f))
	last := f.execSpecs[len(f.execSpecs)-1]
	assert.Equal(t, []string{"/.ucd/ucd-sh", "chown-as", "1000:2000"}, last.Shell, "run through the shim")
	assert.Equal(t, "c0", f.execs[len(f.execs)-1].id, "the root pause container does the chown")
}

// Only the job container sets runAsUser: the k8s artifact sidecar then runs as
// root, so the downloaded files are root-owned (design 6.4).
func TestHostBackend_Download_ContainerLevelUserOnlyChownsToRoot(t *testing.T) {
	f := &podFakeRT{}
	b, workDir := podBackend(t, downloadAgent(t), f, permPT(nil, map[string]any{"runAsUser": 1000}))

	require.NoError(t, b.DownloadArtifact(context.Background(), ScopeHandle{}, "r1", "art", filepath.Join(workDir, "out")))
	assert.Equal(t, []string{"0:0 /workspace/out"}, chownExecs(f))
}

func TestHostBackend_Download_NoSecurityContextRunsNoChown(t *testing.T) {
	f := &podFakeRT{}
	b, workDir := podBackend(t, downloadAgent(t), f, permPT(nil, nil))

	require.NoError(t, b.DownloadArtifact(context.Background(), ScopeHandle{}, "r1", "art", filepath.Join(workDir, "out")))
	assert.Empty(t, chownExecs(f))
}

func TestHostBackend_Download_IntoAVolumeUsesItsContainerPath(t *testing.T) {
	f := &podFakeRT{}
	b, _ := podBackend(t, downloadAgent(t), f, permPT(map[string]any{"runAsUser": 1000}, nil))
	volHost := t.TempDir()
	b.volumeRefs = []volumeMountRef{{Volume: "v", MountPath: "cache", Rel: "cache", HostPath: volHost}}

	require.NoError(t, b.DownloadArtifact(context.Background(), ScopeHandle{}, "r1", "art", filepath.Join(volHost, "sub")))
	assert.Equal(t, []string{"1000:0 /workspace/cache/sub"}, chownExecs(f))
}

func TestHostBackend_CacheRestore_ChownsOnAHitOnly(t *testing.T) {
	ctx := context.Background()
	f := &podFakeRT{}
	b, workDir := podBackend(t, downloadAgent(t), f, permPT(map[string]any{"runAsUser": 1000, "runAsGroup": 1000}, nil))

	dest := filepath.Join(workDir, "deps")
	hit, err := b.CacheRestore(ctx, ScopeHandle{}, "k", nil, dest)
	require.NoError(t, err)
	require.False(t, hit)
	assert.Empty(t, chownExecs(f), "a miss writes nothing, so there is nothing to re-own")

	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "dep"), []byte("d"), 0o644))
	require.NoError(t, b.CacheSave(ctx, ScopeHandle{}, "k", src, 1))

	hit, err = b.CacheRestore(ctx, ScopeHandle{}, "k", nil, dest)
	require.NoError(t, err)
	require.True(t, hit)
	assert.Equal(t, []string{"1000:1000 /workspace/deps"}, chownExecs(f))
}

// failExecRT fails every exec (the chown-as helper) with exit code 1.
type failExecRT struct{ podFakeRT }

func (f *failExecRT) Exec(ctx context.Context, h crt.ContainerHandle, s crt.ExecSpec, o, e io.Writer) (int, error) {
	_, _ = f.podFakeRT.Exec(ctx, h, s, o, e)
	return 1, nil
}

func TestHostBackend_ChownFailureFailsTheDownload(t *testing.T) {
	f := &failExecRT{}
	b, workDir := podBackend(t, downloadAgent(t), f, permPT(map[string]any{"runAsUser": 1000}, nil))
	err := b.DownloadArtifact(context.Background(), ScopeHandle{}, "r1", "art", filepath.Join(workDir, "out"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chown-as")
}

func TestHostBackend_ReadFallback_RetriesThroughCopyOutOnPermissionError(t *testing.T) {
	f := &copyOutRT{}
	b, workDir := podBackend(t, downloadAgent(t), f, permPT(nil, nil))
	src := filepath.Join(workDir, "build")

	var paths []string
	err := b.readWithFallback(context.Background(), "artifact", src, func(p string, _ []string) error {
		paths = append(paths, p)
		if len(paths) == 1 {
			return &fs.PathError{Op: "open", Path: p, Err: fs.ErrPermission}
		}
		_, statErr := os.Stat(filepath.Join(p, "from-pause.txt"))
		return statErr
	})
	require.NoError(t, err)

	require.Len(t, paths, 2)
	assert.Equal(t, src, paths[0], "the first attempt reads the host path")
	assert.NotEqual(t, src, paths[1])
	require.Len(t, f.copyOuts, 1)
	assert.Equal(t, "/workspace/build", f.copyOuts[0].from)
	_, statErr := os.Stat(paths[1])
	assert.True(t, os.IsNotExist(statErr), "the temp copy is removed afterwards")
}

func TestHostBackend_ReadFallback_ExclusionsAreMappedIntoTheCopy(t *testing.T) {
	f := &copyOutRT{}
	b, workDir := podBackend(t, downloadAgent(t), f, permPT(nil, nil))
	volHost := t.TempDir()
	b.volumeRefs = []volumeMountRef{{Volume: "v", MountPath: "data", Rel: "data", HostPath: volHost}}

	var got []string
	var copied string
	calls := 0
	err := b.readWithFallback(context.Background(), "artifact", workDir, func(p string, ex []string) error {
		calls++
		if calls == 1 {
			return fs.ErrPermission
		}
		copied, got = p, ex
		return nil
	})
	require.NoError(t, err)
	require.Len(t, f.copyOuts, 1)
	assert.Equal(t, "/workspace", f.copyOuts[0].from)
	assert.Equal(t, []string{filepath.Join(copied, "data")}, got, "the volume mount point is excluded in the copy")
}

func TestHostBackend_ReadFallback_OtherErrorsAreReturnedUntouched(t *testing.T) {
	f := &copyOutRT{}
	b, workDir := podBackend(t, downloadAgent(t), f, permPT(nil, nil))
	boom := errors.New("boom")
	err := b.readWithFallback(context.Background(), "artifact", workDir, func(string, []string) error { return boom })
	assert.ErrorIs(t, err, boom)
	assert.Empty(t, f.copyOuts)
}

func TestHostBackend_ReadFallback_NativeClaimHasNoPauseToCopyFrom(t *testing.T) {
	b := newHostBackend(downloadAgent(t), "r1", "test-job", t.TempDir(), nil)
	err := b.readWithFallback(context.Background(), "artifact", b.workDir, func(string, []string) error { return fs.ErrPermission })
	assert.ErrorIs(t, err, fs.ErrPermission)
}
