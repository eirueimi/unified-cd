package k8sagent

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// statfsExec is a stepExecutor whose ExecStepArgv answers `ucd-sh statfs <path>`
// the way the shim does, from a table keyed by path. A path with no entry, or
// one marked failing, fails the way the shim fails: nothing on stdout, a message
// on stderr, exit 1.
type statfsExec struct {
	mu    sync.Mutex
	out   map[string]string // path -> stdout to write on success
	fail  map[string]bool   // path -> exit 1 with empty stdout
	calls [][]string
	// override replaces the whole answer for every call when set.
	override func(ctx context.Context, argv []string, stdout io.Writer) (int, error)
}

func (f *statfsExec) ExecStep(context.Context, string, string, string, []string, []string, io.Writer, io.Writer) (int, error) {
	return 0, nil
}

func (f *statfsExec) ExecStepArgv(ctx context.Context, _, _ string, argv []string, stdout, stderr io.Writer) (int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), argv...))
	f.mu.Unlock()
	if f.override != nil {
		return f.override(ctx, argv, stdout)
	}
	if len(argv) != 3 || argv[0] != ucdShimPath || argv[1] != "statfs" {
		return 127, nil
	}
	f.mu.Lock()
	out, ok := f.out[argv[2]]
	failed := f.fail[argv[2]]
	f.mu.Unlock()
	if failed || !ok {
		_, _ = io.WriteString(stderr, "[ucd-sh] statfs: no such file or directory\n")
		return 1, nil
	}
	_, _ = io.WriteString(stdout, out)
	return 0, nil
}

func (f *statfsExec) argvs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.calls...)
}

func (f *statfsExec) set(path, stdout string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.out == nil {
		f.out = map[string]string{}
	}
	f.out[path] = stdout
}

func (f *statfsExec) setFailing(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail == nil {
		f.fail = map[string]bool{}
	}
	f.fail[path] = true
}

func reading(free, total uint64) string { return fmt.Sprintf("free=%d total=%d\n", free, total) }

func storageAgent(ex stepExecutor) *K8sAgent {
	return &K8sAgent{cfg: Config{AgentID: "k8s-st-1", AgentName: "agent1"}, exec: ex}
}

func measure(a *K8sAgent, vols ...PodVolume) {
	a.measureVolumes(context.Background(), "run-pod", "job", "/workspace", vols)
}

func TestK8sStorage_MeasuresEachMountedVolume(t *testing.T) {
	ex := &statfsExec{out: map[string]string{
		"/workspace/.ucd-cache/go": reading(10, 100),
		"/root/.cache":             reading(2<<30, 8<<30),
	}}
	a := storageAgent(ex)

	before := time.Now()
	measure(a,
		PodVolume{Name: "go-cache", ClaimName: "agent1-go-cache", MountPath: ".ucd-cache/go"},
		PodVolume{Name: "tool-cache", ClaimName: "agent1-tool-cache", MountPath: "/root/.cache"},
	)
	after := time.Now()

	got := a.storageReading().Volumes
	require.Len(t, got, 2)
	assert.Equal(t, api.FilesystemStats{FreeBytes: 10, TotalBytes: 100}, got["go-cache"].FilesystemStats)
	assert.Equal(t, api.FilesystemStats{FreeBytes: 2 << 30, TotalBytes: 8 << 30}, got["tool-cache"].FilesystemStats)
	for name, v := range got {
		assert.False(t, v.MeasuredAt.Before(before) || v.MeasuredAt.After(after), "%s: MeasuredAt %v is not the time of the measurement", name, v.MeasuredAt)
	}
	// The shim is run directly, with the path as its own argv element: nothing
	// is interpolated into a shell string, and no `df` is assumed in the image.
	assert.ElementsMatch(t, [][]string{
		{ucdShimPath, "statfs", "/workspace/.ucd-cache/go"},
		{ucdShimPath, "statfs", "/root/.cache"},
	}, ex.argvs())
}

// A volume mounted at the workspace root is the workspace itself, and the one
// whose headroom matters most: it is measured at the workspace path.
func TestK8sStorage_MeasuresARootVolumeAtTheWorkspace(t *testing.T) {
	ex := &statfsExec{out: map[string]string{"/workspace": reading(5, 50)}}
	a := storageAgent(ex)

	measure(a, PodVolume{Name: "unity-main", ClaimName: "agent1-unity-main", MountPath: "."})

	got := a.storageReading().Volumes
	require.Contains(t, got, "unity-main")
	assert.Equal(t, api.FilesystemStats{FreeBytes: 5, TotalBytes: 50}, got["unity-main"].FilesystemStats)
}

func TestK8sStorage_OmitsWorkspaceAndThreshold(t *testing.T) {
	ex := &statfsExec{out: map[string]string{"/workspace/c": reading(1, 2)}}
	a := storageAgent(ex)
	measure(a, PodVolume{Name: "c", MountPath: "c"})

	s := a.storageReading()
	require.NotNil(t, s)
	assert.Equal(t, "agent1", s.AgentName, "the series are labelled with the configured agentName, not the agent ID")
	assert.Nil(t, s.Workspace, "a Kubernetes agent has no workspace filesystem of its own")
	assert.Nil(t, s.MinFreeDiskBytes, "minFreeDisk is a host-only setting: absent, not zero")
	assert.Nil(t, s.ClaimBlockedByDisk, "there is no claim-time disk check here: absent, not false")
}

func TestK8sStorage_AFailedExecCachesNothing(t *testing.T) {
	cases := map[string]struct {
		stdout string
		exit   int
		err    error
	}{
		"nonzero exit with no output":     {exit: 1},
		"usage exit":                      {exit: 2},
		"nonzero exit despite valid line": {stdout: reading(1, 2), exit: 1},
		"exec infrastructure error":       {exit: 1, err: assert.AnError},
		"error despite valid line":        {stdout: reading(1, 2), err: assert.AnError},
		"empty output":                    {},
		"missing newline":                 {stdout: "free=1 total=2"},
		"partial line":                    {stdout: "free=1 total="},
		"only free":                       {stdout: "free=1\n"},
		"two lines":                       {stdout: reading(1, 2) + reading(3, 4)},
		"trailing text":                   {stdout: reading(1, 2) + "extra\n"},
		"leading text":                    {stdout: "warning\n" + reading(1, 2)},
		"leading space":                   {stdout: " free=1 total=2\n"},
		"trailing space":                  {stdout: "free=1 total=2 \n"},
		"crlf":                            {stdout: "free=1 total=2\r\n"},
		"reversed fields":                 {stdout: "total=2 free=1\n"},
		"comma separated":                 {stdout: "free=1,total=2\n"},
		"negative":                        {stdout: "free=-1 total=2\n"},
		"signed":                          {stdout: "free=+1 total=2\n"},
		"hex":                             {stdout: "free=0x10 total=0x20\n"},
		"decimal point":                   {stdout: "free=1.5 total=2\n"},
		"overflows uint64":                {stdout: "free=1 total=18446744073709551616\n"},
		"zero total":                      {stdout: "free=0 total=0\n"},
		"free larger than total":          {stdout: "free=3 total=2\n"},
		"empty number":                    {stdout: "free= total=2\n"},
		"unrelated command output":        {stdout: "sh: 1: /.ucd/ucd-sh: not found\n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ex := &statfsExec{override: func(_ context.Context, _ []string, stdout io.Writer) (int, error) {
				_, _ = io.WriteString(stdout, tc.stdout)
				return tc.exit, tc.err
			}}
			a := storageAgent(ex)
			measure(a, PodVolume{Name: "v", MountPath: "v"})

			assert.Empty(t, a.storageReading().Volumes, "no entry, no zero, no timestamp")
		})
	}

	t.Run("a failure leaves the previous reading and its timestamp untouched", func(t *testing.T) {
		ex := &statfsExec{out: map[string]string{"/workspace/v": reading(7, 70)}}
		a := storageAgent(ex)
		measure(a, PodVolume{Name: "v", MountPath: "v"})
		first := a.storageReading().Volumes["v"]
		require.NotZero(t, first.MeasuredAt)

		ex.setFailing("/workspace/v")
		measure(a, PodVolume{Name: "v", MountPath: "v"})

		assert.Equal(t, first, a.storageReading().Volumes["v"])
	})

	t.Run("one volume failing does not stop the others", func(t *testing.T) {
		ex := &statfsExec{out: map[string]string{"/workspace/b": reading(1, 2)}}
		a := storageAgent(ex)
		measure(a, PodVolume{Name: "a", MountPath: "a"}, PodVolume{Name: "b", MountPath: "b"})

		got := a.storageReading().Volumes
		assert.NotContains(t, got, "a")
		assert.Contains(t, got, "b")
	})
}

func TestK8sStorage_AFreshAgentReportsNoVolumes(t *testing.T) {
	// No executor at all: producing a reading must never exec, because it runs
	// on the heartbeat goroutine.
	a := &K8sAgent{cfg: Config{AgentID: "k8s-st-1", AgentName: "agent1"}}

	s := a.storageReading()
	require.NotNil(t, s)
	assert.Equal(t, "agent1", s.AgentName)
	assert.Empty(t, s.Volumes, "a restart loses the cache: those series are absent until a run mounts the volume again")
	assert.Nil(t, s.Workspace)
	assert.Nil(t, s.MinFreeDiskBytes)
	assert.Nil(t, s.ClaimBlockedByDisk)
}

func TestK8sStorage_ReadingsSurviveUntilTheNextMeasurement(t *testing.T) {
	ex := &statfsExec{out: map[string]string{"/workspace/v": reading(7, 70)}}
	a := storageAgent(ex)
	measure(a, PodVolume{Name: "v", MountPath: "v"})

	first := a.storageReading().Volumes["v"]
	for i := 0; i < 3; i++ { // heartbeats
		assert.Equal(t, first, a.storageReading().Volumes["v"], "reading a beat must not consume or clear the cache")
	}

	time.Sleep(2 * time.Millisecond)
	ex.set("/workspace/v", reading(6, 70))
	measure(a, PodVolume{Name: "v", MountPath: "v"})

	next := a.storageReading().Volumes["v"]
	assert.Equal(t, uint64(6), next.FreeBytes)
	assert.True(t, next.MeasuredAt.After(first.MeasuredAt))
}

// The returned map is the caller's: a later measurement must not mutate a
// reading the heartbeat is still encoding.
func TestK8sStorage_AReadingIsASnapshot(t *testing.T) {
	ex := &statfsExec{out: map[string]string{"/workspace/a": reading(1, 2), "/workspace/b": reading(3, 4)}}
	a := storageAgent(ex)
	measure(a, PodVolume{Name: "a", MountPath: "a"})
	snap := a.storageReading()

	measure(a, PodVolume{Name: "b", MountPath: "b"})

	assert.Len(t, snap.Volumes, 1)
	assert.Len(t, a.storageReading().Volumes, 2)
}

// Several runs measure at once while the heartbeat reads; the suite runs under
// -race, which is the assertion here.
func TestK8sStorage_ConcurrentRunsAndHeartbeats(t *testing.T) {
	ex := &statfsExec{out: map[string]string{
		"/workspace/a": reading(1, 2), "/workspace/b": reading(3, 4), "/workspace/c": reading(5, 6),
	}}
	a := storageAgent(ex)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				measure(a, PodVolume{Name: "a", MountPath: "a"}, PodVolume{Name: "b", MountPath: "b"}, PodVolume{Name: "c", MountPath: "c"})
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				for range a.storageReading().Volumes {
				}
			}
		}()
	}
	wg.Wait()
	assert.Len(t, a.storageReading().Volumes, 3)
}

// Two runs mounting one shared volume can finish measuring out of order. The
// older reading must not replace the newer.
func TestK8sStorage_AnOlderReadingNeverReplacesANewerOne(t *testing.T) {
	var r volumeReadings
	t1 := time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC)
	r.set("v", api.FilesystemStats{FreeBytes: 1, TotalBytes: 10}, t1)
	r.set("v", api.FilesystemStats{FreeBytes: 9, TotalBytes: 10}, t1.Add(-time.Second))

	got := r.snapshot()["v"]
	assert.Equal(t, uint64(1), got.FreeBytes)
	assert.Equal(t, t1, got.MeasuredAt)
}

// --- through executeRun -----------------------------------------------------

func TestK8sStorage_ExecuteRunMeasuresMountedVolumes(t *testing.T) {
	f := newVolFixture(t, []config.VolumeConfig{vcfg("go-cache", "exclusive", "10Gi"), vcfg("unity", "exclusive", "10Gi")})
	ex := &statfsExec{out: map[string]string{
		"/workspace/.ucd-cache/go": reading(10, 100),
		"/workspace":               reading(20, 200),
	}}
	f.a.exec = &stepAndStatfsExec{steps: f.ex, statfs: ex}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f.a.executeRun(ctx, f.claim([]dsl.VolumeMount{
		mount("go-cache", "go-cache", ".ucd-cache/go"),
		mount("unity", "", "."),
	}))
	require.Equal(t, api.RunSucceeded, f.waitFinish(t))

	got := f.a.storageReading().Volumes
	require.Len(t, got, 2)
	assert.Equal(t, uint64(10), got["go-cache"].FreeBytes)
	assert.Equal(t, uint64(20), got["unity"].FreeBytes)
}

// A measurement is a metric: a failing or hung exec must neither fail the run
// nor hold it up for longer than the measurement's own bound.
func TestK8sStorage_AFailingMeasurementDoesNotFailTheRun(t *testing.T) {
	f := newVolFixture(t, []config.VolumeConfig{vcfg("go-cache", "exclusive", "10Gi")})
	f.a.exec = &stepAndStatfsExec{steps: f.ex, statfs: &statfsExec{override: func(context.Context, []string, io.Writer) (int, error) {
		return 1, assert.AnError
	}}}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f.a.executeRun(ctx, f.claim([]dsl.VolumeMount{mount("go-cache", "go-cache", ".cache")}))

	require.Equal(t, api.RunSucceeded, f.waitFinish(t))
	assert.Empty(t, f.a.storageReading().Volumes)
}

func TestK8sStorage_AHungMeasurementIsBoundedAndDoesNotFailTheRun(t *testing.T) {
	old := volumeMeasureTimeout
	volumeMeasureTimeout = 100 * time.Millisecond
	t.Cleanup(func() { volumeMeasureTimeout = old })

	f := newVolFixture(t, []config.VolumeConfig{vcfg("go-cache", "exclusive", "10Gi")})
	f.a.exec = &stepAndStatfsExec{steps: f.ex, statfs: &statfsExec{override: func(ctx context.Context, _ []string, _ io.Writer) (int, error) {
		<-ctx.Done()
		return 1, ctx.Err()
	}}}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	f.a.executeRun(ctx, f.claim([]dsl.VolumeMount{mount("go-cache", "go-cache", ".cache")}))

	require.Equal(t, api.RunSucceeded, f.waitFinish(t))
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Empty(t, f.a.storageReading().Volumes)
}

// A run with no volumes (and a pooled pod mounts none) measures nothing.
func TestK8sStorage_ARunWithoutVolumesExecsNoStatfs(t *testing.T) {
	f := newVolFixture(t, nil)
	ex := &statfsExec{}
	f.a.exec = &stepAndStatfsExec{steps: f.ex, statfs: ex}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f.a.executeRun(ctx, f.claim(nil))

	require.Equal(t, api.RunSucceeded, f.waitFinish(t))
	assert.Empty(t, ex.argvs())
	assert.Empty(t, f.a.storageReading().Volumes)
}

// stepAndStatfsExec sends the shim's statfs to its own fake and everything
// else (the run's steps) to the fixture's recorder.
type stepAndStatfsExec struct {
	steps  *envCaptureExec
	statfs *statfsExec
}

func (s *stepAndStatfsExec) ExecStep(ctx context.Context, pod, container, script string, shell, env []string, stdout, stderr io.Writer) (int, error) {
	return s.steps.ExecStep(ctx, pod, container, script, shell, env, stdout, stderr)
}

func (s *stepAndStatfsExec) ExecStepArgv(ctx context.Context, pod, container string, argv []string, stdout, stderr io.Writer) (int, error) {
	if len(argv) > 1 && argv[0] == ucdShimPath && argv[1] == "statfs" {
		return s.statfs.ExecStepArgv(ctx, pod, container, argv, stdout, stderr)
	}
	return s.steps.ExecStepArgv(ctx, pod, container, argv, stdout, stderr)
}
