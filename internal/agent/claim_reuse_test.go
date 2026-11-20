package agent

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareWorkspace_KeepDirPreservesInode(t *testing.T) {
	for _, mode := range []string{"isolated", "native"} {
		t.Run(mode, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "ws")
			require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
			writeFile(t, filepath.Join(dir, "file.txt"), "x")
			writeFile(t, filepath.Join(dir, ".hidden"), "x")
			writeFile(t, filepath.Join(dir, "sub", "n.txt"), "x")
			before, err := os.Stat(dir)
			require.NoError(t, err)

			require.NoError(t, prepareWorkspace(context.Background(), dir, mode, nil, false, true, noRuntime, ""))
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			assert.Empty(t, entries, "the workspace is emptied, dot-files included")
			after, err := os.Stat(dir)
			require.NoError(t, err)
			assert.True(t, os.SameFile(before, after), "the directory itself is never recreated")
			if mode == "isolated" && runtime.GOOS != "windows" {
				assert.Equal(t, os.FileMode(0o777), after.Mode().Perm())
			}

			writeFile(t, filepath.Join(dir, "out.bin"), "x")
			writeFile(t, filepath.Join(dir, ".out"), "x")
			finishWorkspace(context.Background(), dir, nil, true, noRuntime, "")
			entries, err = os.ReadDir(dir)
			require.NoError(t, err)
			assert.Empty(t, entries)
			after2, err := os.Stat(dir)
			require.NoError(t, err)
			assert.True(t, os.SameFile(before, after2), "finishWorkspace keeps the directory too")
		})
	}
}

func TestPrepareWorkspace_NoKeepDirRecreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ws")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	writeFile(t, filepath.Join(dir, "file.txt"), "x")
	require.NoError(t, prepareWorkspace(context.Background(), dir, "native", nil, false, false, noRuntime, ""))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
	finishWorkspace(context.Background(), dir, nil, false, noRuntime, "")
	_, err = os.Stat(dir)
	assert.True(t, os.IsNotExist(err), "without keepDir the directory is removed")
}

func TestRunLoop_KeepDirOnlyForPoolableClaims(t *testing.T) {
	reuse := &dsl.PodTemplate{Reuse: true}
	iso := api.ClaimResponse{PodTemplate: reuse}
	tests := []struct {
		name string
		c    api.ClaimResponse
		o    claimOpts
		want bool
	}{
		{"reuse isolated", iso, claimOpts{Poolable: true}, true},
		{"native", api.ClaimResponse{Native: true, PodTemplate: reuse}, claimOpts{Poolable: true}, false},
		{"detached", iso, claimOpts{Poolable: false}, false},
		{"with volumes", api.ClaimResponse{PodTemplate: reuse, Volumes: []dsl.VolumeMount{{}}}, claimOpts{Poolable: true}, false},
		{"reuse false", api.ClaimResponse{PodTemplate: &dsl.PodTemplate{}}, claimOpts{Poolable: true}, false},
		{"no template", api.ClaimResponse{}, claimOpts{Poolable: true}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, claimPoolable(tc.c, tc.o))
		})
	}
}

// sinceRT records which log method the pump used.
type sinceRT struct {
	crtNoop
	mu     sync.Mutex
	calls  []string
	sinces []time.Time
}

func (f *sinceRT) Logs(_ context.Context, h crt.ContainerHandle, _, _ io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Logs:"+h.ID)
	return nil
}

func (f *sinceRT) LogsSince(_ context.Context, h crt.ContainerHandle, since time.Time, _, _ io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "LogsSince:"+h.ID)
	f.sinces = append(f.sinces, since)
	return nil
}
func (f *sinceRT) LastLogTime(context.Context, crt.ContainerHandle) (time.Time, error) {
	return time.Time{}, nil
}
func (f *sinceRT) ImageID(context.Context, string) (string, bool, error) { return "", false, nil }
func (f *sinceRT) ContainerRunning(context.Context, crt.ContainerHandle) (bool, error) {
	return false, nil
}
func (f *sinceRT) ListContainers(context.Context, map[string]string) ([]crt.ContainerInfo, error) {
	return nil, nil
}

func TestSidecarPump_ReusedPodStreamsSinceClaimStart(t *testing.T) {
	rec := newRecordingClient()
	defer rec.srv.Close()
	sc := []SidecarHandle{{Name: "mysql", Ordinal: 0, Handle: crt.ContainerHandle{ID: "c1"}}}

	t0 := time.Now().Add(-time.Minute)
	f := &sinceRT{}
	pump := newSidecarLogPump(f, rec.client, "agent-1", "run-1", nil, sc, t0)
	pump.Start(context.Background())
	pump.Stop(context.Background())
	assert.Equal(t, []string{"LogsSince:c1"}, f.calls)
	require.Len(t, f.sinces, 1)
	assert.True(t, f.sinces[0].Equal(t0))

	f = &sinceRT{}
	pump = newSidecarLogPump(f, rec.client, "agent-1", "run-1", nil, sc, time.Time{})
	pump.Start(context.Background())
	pump.Stop(context.Background())
	assert.Equal(t, []string{"Logs:c1"}, f.calls, "a fresh pod streams everything")
}

func TestClearWorkspaceScript_MovedVerbatim(t *testing.T) {
	assert.Equal(t, `rm -rf '/workspace'/* '/workspace'/.[!.]* '/workspace'/..?*`, ClearWorkspaceScript("/workspace", nil))
}

func TestClaimPod_CreateSpecsCarryNoRunSpecificValues(t *testing.T) {
	f := &pullRT{}
	h := newIsolatedHarness()
	srv := newIsolatedServer(t, "reuse-agent", h)
	a := &Agent{ID: "reuse-agent", Client: NewClient(srv.URL, "tok"), PauseImage: "pause:img", RunnerImage: "runner:img"}
	a.runtimeOnce.Do(func() {})
	a.resolvedRuntime = f
	claim := api.ClaimResponse{
		RunID:       "run-7f3a9c",
		JobName:     "reuse-job",
		PodTemplate: mysqlTemplate(),
		Params:      map[string]string{"branch": "param-value-1"},
		Vars:        map[string]string{"region": "var-value-2"},
		Stages: []api.ClaimStage{{Step: &api.ClaimStep{
			Index: 0, Name: "s1", Run: "echo $TOKEN",
			Env: map[string]string{"TOKEN": "s3cr3t-value"},
		}}},
	}
	a.executeRun(context.Background(), claim, t.TempDir())

	require.NotEmpty(t, f.created)
	for _, spec := range f.created {
		b, err := json.Marshal(spec)
		require.NoError(t, err)
		for _, bad := range []string{"run-7f3a9c", "s3cr3t-value", "TOKEN", "param-value-1", "var-value-2"} {
			assert.NotContains(t, string(b), bad, "CreateSpec must be job-stable (image %s)", spec.Image)
		}
	}
	var sawToken bool
	for _, s := range f.execSpecs {
		for _, e := range s.Env {
			if strings.Contains(e, "TOKEN=s3cr3t-value") {
				sawToken = true
			}
		}
	}
	assert.True(t, sawToken, "the step's env is delivered at Exec time")
}
