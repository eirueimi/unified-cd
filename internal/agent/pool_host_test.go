package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
)

// poolRT is podFakeRT plus the hooks the pool tests need: containers are
// created running, Exec can be intercepted per call, and the LogsSince
// boundaries the sidecar pump asks for are recorded.
type poolRT struct {
	podFakeRT
	mu     sync.Mutex
	execFn func(ctx context.Context, h crt.ContainerHandle, s crt.ExecSpec) (code int, err error, handled bool)
	sinces []time.Time
	// repull makes every Pull assign a new image ID, as re-pulling a moved tag.
	repull bool
	pulls  int
}

func (f *poolRT) Pull(ctx context.Context, image string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.repull {
		return f.podFakeRT.Pull(ctx, image)
	}
	f.pulls++
	if f.imageIDs == nil {
		f.imageIDs = map[string]string{}
	}
	f.imageIDs[image] = "id-" + strconv.Itoa(f.pulls)
	return nil
}

func (f *poolRT) Create(ctx context.Context, s crt.CreateSpec) (crt.ContainerHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, err := f.podFakeRT.Create(ctx, s)
	if f.running == nil {
		f.running = map[string]bool{}
	}
	f.running[h.ID] = true
	return h, err
}

func (f *poolRT) Exec(ctx context.Context, h crt.ContainerHandle, s crt.ExecSpec, so, se io.Writer) (int, error) {
	f.mu.Lock()
	fn := f.execFn
	f.mu.Unlock()
	if fn != nil {
		if code, err, handled := fn(ctx, h, s); handled {
			f.mu.Lock()
			f.execs = append(f.execs, struct{ id, script string }{h.ID, s.Script})
			f.mu.Unlock()
			return code, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.podFakeRT.Exec(ctx, h, s, so, se)
}

func (f *poolRT) Remove(ctx context.Context, h crt.ContainerHandle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.podFakeRT.Remove(ctx, h)
}

func (f *poolRT) LogsSince(_ context.Context, _ crt.ContainerHandle, since time.Time, _, _ io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sinces = append(f.sinces, since)
	return nil
}

func (f *poolRT) removedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removed...)
}

func (f *poolRT) execTargets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, e := range f.execs {
		out = append(out, e.id)
	}
	return out
}

type poolEnv struct {
	t  *testing.T
	h  *isolatedHarness
	rt *poolRT
	a  *Agent
}

func newPoolEnv(t *testing.T, rt *poolRT) *poolEnv {
	t.Helper()
	h := newIsolatedHarness()
	srv := newIsolatedServer(t, "pool-agent", h)
	a := &Agent{
		ID: "pool-agent", Client: NewClient(srv.URL, "tok"),
		PauseImage: "pause:img", RunnerImage: "runner:img",
		PoolMaxIdle: 4, PoolIdleTimeout: 30 * time.Minute,
	}
	a.runtimeOnce.Do(func() {})
	a.resolvedRuntime = rt
	a.hostPool = newHostPodPool(rt, a.PoolIdleTimeout, a.PoolMaxIdle)
	return &poolEnv{t: t, h: h, rt: rt, a: a}
}

// reuseTemplate is a podTemplate with a mysql sidecar and reuse: true; a is the
// value of its env var A.
func reuseTemplate(a string) *dsl.PodTemplate {
	return &dsl.PodTemplate{Reuse: true, Spec: map[string]any{
		"containers": []any{
			map[string]any{"name": "mysql", "image": "mysql:8",
				"env": []any{map[string]any{"name": "A", "value": a}}},
		},
	}}
}

func (e *poolEnv) claim(runID, job string, pt *dsl.PodTemplate) api.ClaimResponse {
	return api.ClaimResponse{
		RunID: runID, JobName: job, PodTemplate: pt,
		Stages: []api.ClaimStage{{Step: &api.ClaimStep{Index: 0, Name: "s1", Run: "echo " + runID}}},
	}
}

func (e *poolEnv) run(c api.ClaimResponse, workDir string, o claimOpts) string {
	e.t.Helper()
	e.h.mu.Lock()
	e.h.finishStatus = ""
	e.h.mu.Unlock()
	e.a.executeRunWith(context.Background(), c, workDir, o)
	e.h.mu.Lock()
	defer e.h.mu.Unlock()
	return e.h.finishStatus
}

func (e *poolEnv) sysLog() string {
	e.h.mu.Lock()
	defer e.h.mu.Unlock()
	var msgs []string
	for _, l := range e.h.logsByStep[-1] {
		msgs = append(msgs, l.Line)
	}
	return strings.Join(msgs, "\n")
}

var poolable = claimOpts{Poolable: true}

func TestPool_ReuseHit(t *testing.T) {
	f := &poolRT{}
	e := newPoolEnv(t, f)
	wd := t.TempDir()
	require.Equal(t, "Succeeded", e.run(e.claim("r1", "job-a", reuseTemplate("1")), wd, poolable))
	require.Len(t, f.created, 3)
	require.Equal(t, 1, e.a.hostPool.Len())

	require.Equal(t, "Succeeded", e.run(e.claim("r2", "job-a", reuseTemplate("1")), wd, poolable))
	assert.Len(t, f.created, 3, "the second run creates nothing")
	assert.Empty(t, f.removedIDs(), "nothing is removed on a hit")
	targets := f.execTargets()
	require.NotEmpty(t, targets)
	assert.Equal(t, "c2", targets[len(targets)-1], "the second run's step runs in the first run's job container")
	assert.Equal(t, 1, e.a.hostPool.Len(), "the pod is released again")
}

func TestPool_MissOnDifferentPodDefinition(t *testing.T) {
	f := &poolRT{}
	e := newPoolEnv(t, f)
	wd := t.TempDir()
	require.Equal(t, "Succeeded", e.run(e.claim("r1", "job-a", reuseTemplate("1")), wd, poolable))
	require.Equal(t, "Succeeded", e.run(e.claim("r2", "job-a", reuseTemplate("2")), wd, poolable))
	assert.Len(t, f.created, 6)
	assert.ElementsMatch(t, []string{"c0", "c1", "c2"}, f.removedIDs())
	assert.Equal(t, 1, e.a.hostPool.Len())
}

func TestPool_MissOnDifferentJobOrSlot(t *testing.T) {
	f := &poolRT{}
	e := newPoolEnv(t, f)
	wd1, wd2 := t.TempDir(), t.TempDir()
	require.Equal(t, "Succeeded", e.run(e.claim("r1", "job-a", reuseTemplate("1")), wd1, poolable))

	// Another slot (workDir): its own pool entry, a miss, and job-a's pod stays.
	require.Equal(t, "Succeeded", e.run(e.claim("r2", "job-a", reuseTemplate("1")), wd2, poolable))
	assert.Len(t, f.created, 6)
	assert.Empty(t, f.removedIDs())
	assert.Equal(t, 2, e.a.hostPool.Len())

	// Another job on the same workDir: a miss that removes the old pod.
	require.Equal(t, "Succeeded", e.run(e.claim("r3", "job-b", reuseTemplate("1")), wd1, poolable))
	assert.Len(t, f.created, 9)
	assert.ElementsMatch(t, []string{"c0", "c1", "c2"}, f.removedIDs())
}

func TestPool_HitEmptiesWorkspaceViaPauseExec(t *testing.T) {
	f := &poolRT{}
	e := newPoolEnv(t, f)
	wd := t.TempDir()
	require.Equal(t, "Succeeded", e.run(e.claim("r1", "job-a", reuseTemplate("1")), wd, poolable))
	f.mu.Lock()
	before := len(f.execs)
	f.mu.Unlock()
	require.Equal(t, "Succeeded", e.run(e.claim("r2", "job-a", reuseTemplate("1")), wd, poolable))

	f.mu.Lock()
	defer f.mu.Unlock()
	require.Greater(t, len(f.execs), before+1)
	first := f.execs[before]
	assert.Equal(t, "c0", first.id, "the first exec after the acquire is in the pause container")
	assert.Equal(t, ClearWorkspaceScript("/workspace", nil), first.script)
	assert.Equal(t, "c2", f.execs[before+1].id, "the step follows the clear")
}

func TestPool_ClearFailureFallsBackToFresh(t *testing.T) {
	f := &poolRT{}
	e := newPoolEnv(t, f)
	wd := t.TempDir()
	require.Equal(t, "Succeeded", e.run(e.claim("r1", "job-a", reuseTemplate("1")), wd, poolable))
	f.mu.Lock()
	f.execFn = func(_ context.Context, h crt.ContainerHandle, _ crt.ExecSpec) (int, error, bool) {
		if h.ID == "c0" {
			return 1, nil, true
		}
		return 0, nil, false
	}
	f.mu.Unlock()
	require.Equal(t, "Succeeded", e.run(e.claim("r2", "job-a", reuseTemplate("1")), wd, poolable))
	assert.ElementsMatch(t, []string{"c0", "c1", "c2"}, f.removedIDs(), "the old pod is removed")
	assert.Len(t, f.created, 6, "a fresh pod is created")
	assert.Equal(t, 1, e.a.hostPool.Len())
}

func TestPool_UnhealthyPooledPodDiscarded(t *testing.T) {
	f := &poolRT{}
	e := newPoolEnv(t, f)
	wd := t.TempDir()
	require.Equal(t, "Succeeded", e.run(e.claim("r1", "job-a", reuseTemplate("1")), wd, poolable))
	f.mu.Lock()
	f.running["c1"] = false // the sidecar exited while the pod sat idle
	f.mu.Unlock()
	require.Equal(t, "Succeeded", e.run(e.claim("r2", "job-a", reuseTemplate("1")), wd, poolable))
	assert.Len(t, f.created, 6)
	assert.ElementsMatch(t, []string{"c0", "c1", "c2"}, f.removedIDs())
}

// A :latest image is re-pulled on every claim; when the tag moved, the new image
// ID changes the fingerprint and the pooled pod is discarded.
func TestPool_DigestChangeDiscards(t *testing.T) {
	f := &poolRT{repull: true}
	e := newPoolEnv(t, f)
	e.a.RunnerImage = "runner:latest"
	wd := t.TempDir()
	require.Equal(t, "Succeeded", e.run(e.claim("r1", "job-a", reuseTemplate("1")), wd, poolable))
	f.mu.Lock()
	pullsAfterFirst := f.pulls
	f.mu.Unlock()
	require.Equal(t, "Succeeded", e.run(e.claim("r2", "job-a", reuseTemplate("1")), wd, poolable))
	f.mu.Lock()
	assert.Greater(t, f.pulls, pullsAfterFirst, "the mutable tag is pulled again")
	f.mu.Unlock()
	assert.Len(t, f.created, 6)
	assert.ElementsMatch(t, []string{"c0", "c1", "c2"}, f.removedIDs())
}

func TestPool_ReuseReappliesFSGroup(t *testing.T) {
	f := &poolRT{}
	e := newPoolEnv(t, f)
	wd := t.TempDir()
	pt := func() *dsl.PodTemplate {
		p := reuseTemplate("1")
		p.Spec["securityContext"] = map[string]any{"fsGroup": 2000}
		return p
	}
	fsgroupExecs := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		n := 0
		for _, s := range f.execSpecs {
			if len(s.Shell) > 1 && s.Shell[1] == "fsgroup" {
				n++
			}
		}
		return n
	}
	require.Equal(t, "Succeeded", e.run(e.claim("r1", "job-a", pt()), wd, poolable))
	require.Equal(t, 1, fsgroupExecs(), "Start applies fsGroup once")
	require.Equal(t, "Succeeded", e.run(e.claim("r2", "job-a", pt()), wd, poolable))
	assert.Len(t, f.created, 3, "the pod was reused")
	assert.Equal(t, 2, fsgroupExecs(), "the hit re-applies fsGroup to the emptied workspace")
}

func TestPool_LastLogTimeErrorFallsBackToFresh(t *testing.T) {
	f := &skewedRT{}
	e := newPoolEnv(t, &f.poolRT)
	e.a.resolvedRuntime = f
	e.a.hostPool = newHostPodPool(f, e.a.PoolIdleTimeout, e.a.PoolMaxIdle)
	wd := t.TempDir()
	require.Equal(t, "Succeeded", e.run(e.claim("r1", "job-a", reuseTemplate("1")), wd, poolable))
	f.err = errors.New("logs unavailable")
	require.Equal(t, "Succeeded", e.run(e.claim("r2", "job-a", reuseTemplate("1")), wd, poolable))
	assert.ElementsMatch(t, []string{"c0", "c1", "c2"}, f.removedIDs(), "a pod whose log boundary is unknown is never reused")
	assert.Len(t, f.created, 6)
}

func TestSidecarLogBoundary_NewestOfSeveralSidecars(t *testing.T) {
	f := &poolRT{}
	pt := &dsl.PodTemplate{Reuse: true, Spec: map[string]any{"containers": []any{
		map[string]any{"name": "a", "image": "a:1"},
		map[string]any{"name": "b", "image": "b:1"},
	}}}
	m := newClaimPodManager(f, "/w", "/workspace", "pause", "runner", "")
	require.NoError(t, m.Start(context.Background(), pt)) // pause c0, a c1, b c2, job c3
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f.logTimes = map[string]time.Time{"c1": t0.Add(5 * time.Second), "c2": t0}
	got, err := m.sidecarLogBoundary(context.Background())
	require.NoError(t, err)
	assert.True(t, got.Equal(t0.Add(5*time.Second+time.Nanosecond)), "the newest line of any sidecar, %s", got)
	f.logTimes = nil
	got, err = m.sidecarLogBoundary(context.Background())
	require.NoError(t, err)
	assert.True(t, got.IsZero(), "no output anywhere: no boundary")
}

func TestHostPodFingerprint_UsesExpandedImageAndItsID(t *testing.T) {
	base := hostPodFingerprintInput{
		JobName: "j", WorkDir: "/w", MountPath: "/workspace",
		Defs:       []containerDef{{Name: "job", Image: "app:1.0"}},
		ImageIDs:   map[string]string{"app:1.0": "sha256:a"},
		PauseImage: "pause", RunnerImage: "runner", ToolsDir: "/t",
	}
	fp0, err := hostPodFingerprint(base)
	require.NoError(t, err)
	again, _ := hostPodFingerprint(base)
	assert.Equal(t, fp0, again, "deterministic")

	expanded := base
	expanded.Defs = []containerDef{{Name: "job", Image: "app:2.0"}} // the same template expanded with another param
	expanded.ImageIDs = map[string]string{"app:2.0": "sha256:a"}
	fp1, _ := hostPodFingerprint(expanded)
	assert.NotEqual(t, fp0, fp1, "a different expanded image reference is a different pod")

	moved := base
	moved.ImageIDs = map[string]string{"app:1.0": "sha256:b"}
	fp2, _ := hostPodFingerprint(moved)
	assert.NotEqual(t, fp0, fp2, "the same reference resolving to another image ID is a different pod")
}

func TestPool_IdleEviction(t *testing.T) {
	f := &poolRT{}
	p := newHostPodPool(f, 30*time.Minute, 4)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return t0 }
	m := newClaimPodManager(f, "/w", "/workspace", "pause", "runner", "")
	require.NoError(t, m.Start(context.Background(), mysqlTemplate()))
	p.Release(context.Background(), "/w", "j", "fp", m, true)

	p.evictExpired(context.Background(), t0.Add(29*time.Minute))
	assert.Equal(t, 1, p.Len(), "kept before the idle timeout")
	assert.Empty(t, f.removedIDs())
	p.evictExpired(context.Background(), t0.Add(30*time.Minute))
	assert.Equal(t, 0, p.Len())
	assert.ElementsMatch(t, []string{"c0", "c1", "c2"}, f.removedIDs())

	// idleTimeout 0 never evicts by age and StartEviction does nothing.
	p0 := newHostPodPool(f, 0, 4)
	p0.StartEviction(context.Background())
	assert.Nil(t, p0.evictCancel)
	p0.Close(context.Background())
}

func TestPool_MaxIdleEvictsOldest(t *testing.T) {
	f := &poolRT{}
	p := newHostPodPool(f, 30*time.Minute, 2)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var ids [3][]string
	for i := 0; i < 3; i++ {
		p.now = func() time.Time { return t0.Add(time.Duration(i) * time.Second) }
		m := newClaimPodManager(f, "/w"+string(rune('0'+i)), "/workspace", "pause", "runner", "")
		require.NoError(t, m.Start(context.Background(), &dsl.PodTemplate{Reuse: true}))
		ids[i] = []string{"c" + string(rune('0'+2*i)), "c" + string(rune('0'+2*i+1))}
		p.Release(context.Background(), "/w"+string(rune('0'+i)), "j", "fp", m, true)
	}
	assert.Equal(t, 2, p.Len())
	assert.ElementsMatch(t, ids[0], f.removedIDs(), "the pod idle the longest is removed")
	assert.Equal(t, []string{"/w1", "/w2"}, p.WorkDirs())
}

func TestPool_InterruptedExecDiscardsPod(t *testing.T) {
	f := &poolRT{}
	e := newPoolEnv(t, f)
	prev := CancelPollInterval
	CancelPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { CancelPollInterval = prev })
	f.mu.Lock()
	f.execFn = func(ctx context.Context, h crt.ContainerHandle, _ crt.ExecSpec) (int, error, bool) {
		if h.ID != "c2" {
			return 0, nil, false
		}
		e.h.mu.Lock()
		e.h.runStatus = api.RunCancelled
		e.h.mu.Unlock()
		<-ctx.Done() // the client side of a docker exec is killed with the context
		return -1, ctx.Err(), true
	}
	f.mu.Unlock()
	e.run(e.claim("r1", "job-a", reuseTemplate("1")), t.TempDir(), poolable)
	assert.Equal(t, 0, e.a.hostPool.Len(), "a pod whose exec was interrupted may still run its process: never reused")
	assert.ElementsMatch(t, []string{"c0", "c1", "c2"}, f.removedIDs())
}

func TestPool_FailedStepStillPooled(t *testing.T) {
	f := &poolRT{}
	e := newPoolEnv(t, f)
	f.mu.Lock()
	f.execFn = func(_ context.Context, h crt.ContainerHandle, _ crt.ExecSpec) (int, error, bool) {
		return 1, nil, h.ID == "c2"
	}
	f.mu.Unlock()
	assert.Equal(t, "Failed", e.run(e.claim("r1", "job-a", reuseTemplate("1")), t.TempDir(), poolable))
	assert.Equal(t, 1, e.a.hostPool.Len(), "a non-zero exit is not an interrupted exec")
	assert.Empty(t, f.removedIDs())
}

// noPodRT hides the PodRuntime methods of a runtime.
type noPodRT struct{ crt.ContainerRuntime }

func TestPool_NotPooledReasonsLogged(t *testing.T) {
	const msg = "podTemplate.reuse: claim pod not pooled"
	cases := []struct {
		name   string
		reason string
		setup  func(e *poolEnv, f *poolRT) claimOpts
	}{
		{"detached", "detached", func(e *poolEnv, f *poolRT) claimOpts { return claimOpts{Poolable: false} }},
		{"no PodRuntime", "no container inspection", func(e *poolEnv, f *poolRT) claimOpts {
			e.a.resolvedRuntime = noPodRT{f}
			return poolable
		}},
		{"poolMaxIdle 0", "poolMaxIdle is 0", func(e *poolEnv, f *poolRT) claimOpts {
			e.a.PoolMaxIdle = 0
			e.a.hostPool = nil
			return poolable
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			defer captureSlog(&buf)()
			f := &poolRT{}
			e := newPoolEnv(t, f)
			o := tc.setup(e, f)
			require.Equal(t, "Succeeded", e.run(e.claim("r1", "job-a", reuseTemplate("1")), t.TempDir(), o))
			out := buf.String()
			assert.Equal(t, 1, strings.Count(out, msg), out)
			assert.Contains(t, out, "reason=")
			assert.Contains(t, out, tc.reason)
			assert.ElementsMatch(t, []string{"c0", "c1", "c2"}, f.removedIDs(), "the pod is removed at the end of the run")
			if e.a.hostPool != nil {
				assert.Equal(t, 0, e.a.hostPool.Len())
			}
		})
	}
}

func TestPool_ReuseWithVolumesFailsRun(t *testing.T) {
	f := &poolRT{}
	e := newPoolEnv(t, f)
	c := e.claim("r1", "job-a", reuseTemplate("1"))
	c.Volumes = []dsl.VolumeMount{{Name: dsl.StringOrSlice{"data"}}}
	assert.Equal(t, "Failed", e.run(c, t.TempDir(), poolable))
	assert.Equal(t, "spec.volumes cannot be combined with podTemplate.reuse: a reused pod keeps the volumes it was created with, which contradicts a volume's lifecycle", e.sysLog())
	assert.Empty(t, f.created)
}

func TestGCWorkspaces_SkipsPooledWorkDirs(t *testing.T) {
	base := t.TempDir()
	held := filepath.Join(base, "working0", "pooled-job")
	other := filepath.Join(base, "working0", "other-job")
	for _, d := range []string{held, other} {
		require.NoError(t, os.MkdirAll(d, 0o755))
		old := time.Now().Add(-30 * 24 * time.Hour)
		require.NoError(t, os.Chtimes(d, old, old))
	}
	f := &poolRT{}
	p := newHostPodPool(f, 30*time.Minute, 4)
	m := newClaimPodManager(f, held, "/workspace", "pause", "runner", "")
	require.NoError(t, m.Start(context.Background(), &dsl.PodTemplate{Reuse: true}))
	p.Release(context.Background(), held, "pooled-job", "fp", m, true)

	removed, err := gcWorkspaces(context.Background(), base, 7*24*time.Hour, gcActiveSet(nil, p), time.Now(), noRuntime, "")
	require.NoError(t, err)
	assert.DirExists(t, held, "a directory the pool's pod mounts survives")
	assert.NoDirExists(t, other)
	assert.Equal(t, []string{other}, removed)
}

func TestPool_CloseRemovesIdle(t *testing.T) {
	f := &poolRT{}
	p := newHostPodPool(f, 30*time.Minute, 4)
	m := newClaimPodManager(f, "/w", "/workspace", "pause", "runner", "")
	require.NoError(t, m.Start(context.Background(), &dsl.PodTemplate{Reuse: true}))
	p.Release(context.Background(), "/w", "j", "fp", m, true)
	p.Close(context.Background())
	assert.Equal(t, 0, p.Len())
	assert.ElementsMatch(t, []string{"c0", "c1"}, f.removedIDs())

	// A pod released after Close is removed, not pooled.
	m2 := newClaimPodManager(f, "/w", "/workspace", "pause", "runner", "")
	require.NoError(t, m2.Start(context.Background(), &dsl.PodTemplate{Reuse: true}))
	p.Release(context.Background(), "/w", "j", "fp", m2, true)
	assert.Equal(t, 0, p.Len())
}

func TestExecuteRun_CancelDuringStart_NeverPoolsPartialPod(t *testing.T) {
	h := newIsolatedHarness()
	f := &pullRT{blockAt: 2, onBlock: cancelOnBlock(t, h), podFakeRT: podFakeRT{imageIDs: map[string]string{
		"pause:img": "p", "mysql:8": "m", "runner:img": "r",
	}}}
	srv := newIsolatedServer(t, "pool-agent", h)
	a := &Agent{ID: "pool-agent", Client: NewClient(srv.URL, "tok"), PauseImage: "pause:img", RunnerImage: "runner:img",
		PoolMaxIdle: 4, PoolIdleTimeout: time.Hour}
	a.runtimeOnce.Do(func() {})
	a.resolvedRuntime = f
	a.hostPool = newHostPodPool(f, a.PoolIdleTimeout, a.PoolMaxIdle)
	c := api.ClaimResponse{RunID: "r1", JobName: "job-a", PodTemplate: reuseTemplate("1"),
		Stages: []api.ClaimStage{{Step: &api.ClaimStep{Index: 0, Name: "s1", Run: "echo hi"}}}}
	a.executeRunWith(context.Background(), c, t.TempDir(), poolable)

	h.mu.Lock()
	defer h.mu.Unlock()
	assert.Zero(t, h.finishCalls)
	assert.Equal(t, 0, a.hostPool.Len(), "a partially started pod is never pooled")
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.created)
	assert.Len(t, f.removed, len(f.created), "every created container is removed")
}

// skewedRT is a runtime whose clock runs ahead of the agent's by skew. Its
// containers' log lines and LastLogTime are stamped by that clock, and
// LogsSince records the boundary it is asked to resume at.
type skewedRT struct {
	poolRT
	skew time.Duration
	last time.Time // runtime-clock stamp of the newest sidecar log line
	err  error     // when set, LastLogTime fails
}

func (f *skewedRT) LastLogTime(context.Context, crt.ContainerHandle) (time.Time, error) {
	return f.last, f.err
}

// TestPool_ReuseLogBoundaryComesFromRuntimeClock: the boundary a reused pod's
// sidecar log resumes at is derived from the runtime's own log timestamps, so a
// runtime clock that differs from the agent's (Docker Desktop's VM, a remote
// daemon) neither replays the previous run's output nor drops this run's.
func TestPool_ReuseLogBoundaryComesFromRuntimeClock(t *testing.T) {
	for _, skew := range []time.Duration{2 * time.Hour, -2 * time.Hour} {
		t.Run(skew.String(), func(t *testing.T) {
			f := &skewedRT{skew: skew}
			e := newPoolEnv(t, &f.poolRT)
			// newPoolEnv wired &f.poolRT; point the agent at the skewed wrapper.
			e.a.resolvedRuntime = f
			e.a.hostPool = newHostPodPool(f, e.a.PoolIdleTimeout, e.a.PoolMaxIdle)
			wd := t.TempDir()

			require.Equal(t, "Succeeded", e.run(e.claim("r1", "job-a", reuseTemplate("1")), wd, poolable))
			f.mu.Lock()
			assert.Empty(t, f.sinces, "a fresh pod streams the whole sidecar log")
			f.mu.Unlock()

			// The previous run's sidecar wrote its last line at this runtime time.
			f.last = time.Now().Add(skew)
			require.Equal(t, "Succeeded", e.run(e.claim("r2", "job-a", reuseTemplate("1")), wd, poolable))

			f.mu.Lock()
			defer f.mu.Unlock()
			require.Len(t, f.sinces, 1)
			assert.True(t, f.sinces[0].Equal(f.last.Add(time.Nanosecond)),
				"resume strictly after the runtime-stamped last line, got %s want %s", f.sinces[0], f.last.Add(time.Nanosecond))
			assert.Greater(t, f.sinces[0].Sub(time.Now()).Abs(), time.Hour,
				"the agent's own clock is not the boundary")
		})
	}
}

func TestPool_ReuseWithSilentSidecarStreamsWholeLog(t *testing.T) {
	f := &poolRT{}
	e := newPoolEnv(t, f)
	wd := t.TempDir()
	require.Equal(t, "Succeeded", e.run(e.claim("r1", "job-a", reuseTemplate("1")), wd, poolable))
	require.Equal(t, "Succeeded", e.run(e.claim("r2", "job-a", reuseTemplate("1")), wd, poolable))
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Empty(t, f.sinces, "no sidecar output yet: the pump needs no boundary")
}
