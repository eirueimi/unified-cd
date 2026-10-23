package k8sagent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// volPM is a podManager over a fake clientset, so a pod CreatePod made is a pod
// ListPods returns and a pod DeletePod removed is gone: the same truth a
// cluster would give. The only fake part is that a pod is Running at once.
type volPM struct{ *PodManager }

func (volPM) WaitForPodRunning(context.Context, string) error { return nil }

// volFixture runs executeRun against a fake controller and a fake clientset.
type volFixture struct {
	a      *K8sAgent
	cs     *fake.Clientset
	ex     *envCaptureExec
	runID  string
	finish chan api.RunStatus

	mu        sync.Mutex
	logs      []string
	runStatus api.RunStatus
}

func newVolFixture(t *testing.T, vols []config.VolumeConfig) *volFixture {
	t.Helper()
	const agentID = "k8s-vol-1"
	f := &volFixture{runID: "run-vol-1", finish: make(chan api.RunStatus, 4), runStatus: api.RunRunning, cs: fake.NewSimpleClientset()}

	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", ok)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+f.runID+"/outputs", ok)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+f.runID+"/steps/{idx}/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.logs = append(f.logs, string(body))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+f.runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.finish <- api.RunStatus(body["status"])
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/runs/"+f.runID, func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		st := f.runStatus
		f.mu.Unlock()
		orchestrateWriteJSON(w, api.Run{ID: f.runID, Status: st})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	f.ex = &envCaptureExec{}
	f.a = &K8sAgent{
		cfg:    Config{AgentID: agentID, AgentName: "agent1", Namespace: testVolNS, PodImage: "ubuntu:22.04", Volumes: vols},
		client: agentlib.NewClient(srv.URL, "tok"),
		pm:     volPM{NewPodManager(f.cs, testVolNS, "ubuntu:22.04")},
		exec:   f.ex,
		kube:   f.cs,
	}
	// The claims exist before a run mounts or locks one, as after a real startup.
	installPVCCompareAndSwap(f.cs)
	if len(vols) > 0 {
		require.NoError(t, EnsurePVCs(context.Background(), f.cs, testVolNS, "agent1", vols))
	}

	old := agentlib.CancelPollInterval
	agentlib.CancelPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { agentlib.CancelPollInterval = old })
	return f
}

func (f *volFixture) setRunStatus(s api.RunStatus) {
	f.mu.Lock()
	f.runStatus = s
	f.mu.Unlock()
}

func (f *volFixture) logText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.logs, "\n")
}

func (f *volFixture) claim(vols []dsl.VolumeMount) api.ClaimResponse {
	return api.ClaimResponse{
		RunID:   f.runID,
		JobName: "vol-job",
		Volumes: vols,
		Stages: []api.ClaimStage{{Step: &api.ClaimStep{
			Index: 0, StageIndex: 0, Name: "build", Run: "echo hi",
		}}},
	}
}

func (f *volFixture) holder(name string, vols ...string) {
	_, err := f.cs.CoreV1().Pods(testVolNS).Create(context.Background(), claimPod(name, "agent1", corev1.PodRunning, vols...), metav1.CreateOptions{})
	if err != nil {
		panic(err)
	}
}

func (f *volFixture) waitFinish(t *testing.T) api.RunStatus {
	t.Helper()
	select {
	case s := <-f.finish:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("FinishRun not called")
		return ""
	}
}

func (f *volFixture) createdPods(t *testing.T) []corev1.Pod {
	t.Helper()
	l, err := f.cs.CoreV1().Pods(testVolNS).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	return l.Items
}

func mount(name, as, mountPath string) dsl.VolumeMount {
	return dsl.VolumeMount{Name: dsl.StringOrSlice{name}, As: as, MountPath: mountPath}
}

// TestExecuteRun_ExposesVolumePathsToSteps is the parity test for the one
// thing this project exists to guarantee: a job that reads
// $UNIFIED_VOLUME_GO_CACHE sees the same variable on Kubernetes as it does on a
// host agent.
func TestExecuteRun_ExposesVolumePathsToSteps(t *testing.T) {
	f := newVolFixture(t, []config.VolumeConfig{vcfg("go-cache", "exclusive", "10Gi"), vcfg("tool-cache", "shared", "10Gi")})
	var observed []corev1.Pod
	f.a.exec = &podCapturingExec{inner: f.ex, onExec: func() { observed = f.createdPods(t) }}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f.a.executeRun(ctx, f.claim([]dsl.VolumeMount{
		mount("go-cache", "go-cache", ".ucd-cache/go"),
		mount("tool-cache", "tools", "/root/.cache"),
	}))
	require.Equal(t, api.RunSucceeded, f.waitFinish(t))

	f.ex.mu.Lock()
	defer f.ex.mu.Unlock()
	require.Len(t, f.ex.calls, 1)
	env := f.ex.calls[0].env
	assert.Contains(t, env, dsl.VolumeEnvName("go-cache")+"=/workspace/.ucd-cache/go")
	assert.Contains(t, env, dsl.VolumeEnvName("tools")+"=/root/.cache", "the variable is named by the alias, not the volume")
	assert.NotContains(t, strings.Join(env, "\n"), "TOOL_CACHE", "the resolved volume name must not leak into the variable")

	require.Len(t, observed, 1, "the claim pod existed while the step ran")
	claims := map[string]bool{}
	for _, v := range observed[0].Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			claims[v.PersistentVolumeClaim.ClaimName] = true
		}
	}
	assert.Equal(t, map[string]bool{"agent1-go-cache": true, "agent1-tool-cache": true}, claims)
}

// podCapturingExec calls onExec once a step runs, then delegates.
type podCapturingExec struct {
	inner  *envCaptureExec
	onExec func()
}

func (p *podCapturingExec) ExecStep(ctx context.Context, pod, container, script string, shell, env []string, stdout, stderr io.Writer) (int, error) {
	p.onExec()
	return p.inner.ExecStep(ctx, pod, container, script, shell, env, stdout, stderr)
}
func (p *podCapturingExec) ExecStepArgv(ctx context.Context, pod, container string, argv []string, stdout, stderr io.Writer) (int, error) {
	return p.inner.ExecStepArgv(ctx, pod, container, argv, stdout, stderr)
}

func TestExecuteRun_PicksAFreeCandidate(t *testing.T) {
	f := newVolFixture(t, []config.VolumeConfig{vcfg("cache-1", "exclusive", "1Gi"), vcfg("cache-2", "exclusive", "1Gi")})
	f.holder("other-run", "cache-1")
	var observed []corev1.Pod
	f.a.exec = &podCapturingExec{inner: f.ex, onExec: func() { observed = f.createdPods(t) }}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f.a.executeRun(ctx, f.claim([]dsl.VolumeMount{
		{Name: dsl.StringOrSlice{"cache-1", "cache-2"}, As: "cache", MountPath: ".cache"},
	}))
	require.Equal(t, api.RunSucceeded, f.waitFinish(t))

	var claims []string
	for _, p := range observed {
		if p.Name == "other-run" {
			continue
		}
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				claims = append(claims, v.PersistentVolumeClaim.ClaimName)
			}
		}
	}
	assert.Equal(t, []string{"agent1-cache-2"}, claims, "the held candidate is skipped, not waited for")

	f.ex.mu.Lock()
	defer f.ex.mu.Unlock()
	assert.Contains(t, f.ex.calls[0].env, "UNIFIED_VOLUME_CACHE=/workspace/.cache")
}

// TestExecuteRun_WaitsForAnExclusiveVolumeAnotherPodHolds: the loser of a claim
// race waits holding its slot rather than mounting a volume somebody else is
// using. The holder here is a pod, not anything in this process: another
// replica's pod would look exactly the same.
func TestExecuteRun_WaitsForAnExclusiveVolumeAnotherPodHolds(t *testing.T) {
	f := newVolFixture(t, []config.VolumeConfig{vcfg("unity-main", "exclusive", "1Gi")})
	f.holder("other-run", "unity-main")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		f.a.executeRun(ctx, f.claim([]dsl.VolumeMount{mount("unity-main", "", ".")}))
		close(done)
	}()

	time.Sleep(300 * time.Millisecond)
	pods := f.createdPods(t)
	require.Len(t, pods, 1, "no run pod may be created while another pod holds the volume")
	assert.Equal(t, "other-run", pods[0].Name)
	assert.Contains(t, f.logText(), "unity-main", "the run says what it is waiting for")

	require.NoError(t, f.cs.CoreV1().Pods(testVolNS).Delete(context.Background(), "other-run", metav1.DeleteOptions{}))
	require.Equal(t, api.RunSucceeded, f.waitFinish(t))
	<-done
}

func TestExecuteRun_DoesNotWaitForASharedVolume(t *testing.T) {
	f := newVolFixture(t, []config.VolumeConfig{vcfg("go-cache", "shared", "1Gi")})
	f.holder("other-run", "go-cache")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f.a.executeRun(ctx, f.claim([]dsl.VolumeMount{mount("go-cache", "", ".ucd-cache/go")}))
	require.Equal(t, api.RunSucceeded, f.waitFinish(t))
}

func TestExecuteRun_StopsWaitingWhenTheRunIsCancelled(t *testing.T) {
	f := newVolFixture(t, []config.VolumeConfig{vcfg("unity-main", "exclusive", "1Gi")})
	f.holder("other-run", "unity-main")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		f.a.executeRun(ctx, f.claim([]dsl.VolumeMount{mount("unity-main", "", ".")}))
		close(done)
	}()
	time.Sleep(150 * time.Millisecond)
	f.setRunStatus(api.RunCancelled)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("executeRun kept waiting for a volume after the controller cancelled the run")
	}
	select {
	case s := <-f.finish:
		t.Fatalf("a cancelled run must not be finished again by the agent, got %s", s)
	default:
	}
	assert.Len(t, f.createdPods(t), 1, "only the holder exists")
}

func TestExecuteRun_FailsOnAVolumeTheAgentDoesNotDeclare(t *testing.T) {
	f := newVolFixture(t, []config.VolumeConfig{vcfg("go-cache", "exclusive", "1Gi")})
	f.a.executeRun(context.Background(), f.claim([]dsl.VolumeMount{mount("nope", "", ".cache")}))
	assert.Equal(t, api.RunFailed, f.waitFinish(t))
	assert.Contains(t, f.logText(), "nope")
	assert.Empty(t, f.createdPods(t))
}

// TestExecuteRun_RejectsASharedVolumeAtTheWorkspaceRoot mirrors the host
// agent: only the agent knows a volume's mode, so it is the agent that refuses
// several runs checking out over each other with no exclusion.
func TestExecuteRun_RejectsASharedVolumeAtTheWorkspaceRoot(t *testing.T) {
	f := newVolFixture(t, []config.VolumeConfig{vcfg("go-cache", "shared", "1Gi")})
	f.a.executeRun(context.Background(), f.claim([]dsl.VolumeMount{mount("go-cache", "", ".")}))
	assert.Equal(t, api.RunFailed, f.waitFinish(t))
	assert.Contains(t, f.logText(), "shared")
	assert.Empty(t, f.createdPods(t))
}

func TestExecuteRun_RejectsAMountPathThatEscapesTheWorkspace(t *testing.T) {
	f := newVolFixture(t, []config.VolumeConfig{vcfg("go-cache", "exclusive", "1Gi")})
	f.a.executeRun(context.Background(), f.claim([]dsl.VolumeMount{mount("go-cache", "", "../outside")}))
	assert.Equal(t, api.RunFailed, f.waitFinish(t))
	assert.Contains(t, f.logText(), "escapes")
	assert.Empty(t, f.createdPods(t))
}

// TestExecuteRun_VolumesAreNotPooled: a pooled pod's volumes are fixed when it
// is created and successive runs need different ones, so a claim that carries
// volumes never draws from the pool. apply-time validation already refuses the
// combination; this is the backstop for a claim that arrives anyway.
func TestExecuteRun_VolumesAreNotPooled(t *testing.T) {
	f := newVolFixture(t, []config.VolumeConfig{vcfg("go-cache", "exclusive", "1Gi")})
	c := f.claim([]dsl.VolumeMount{mount("go-cache", "", ".cache")})
	c.PodTemplate = &dsl.PodTemplate{Reuse: true}
	f.a.executeRun(context.Background(), c)
	assert.Equal(t, api.RunFailed, f.waitFinish(t))
	assert.Contains(t, f.logText(), "reuse")
	assert.Empty(t, f.createdPods(t))
}

// TestExecuteRun_CleanWorkspaceWipesAVolumeAtTheRoot: cleanWorkspace is the one
// deliberate way to destroy a volume's data, and it must reach a PVC the same
// way it reaches a host directory. Without it a poisoned persistent workspace on
// Kubernetes could only be cleared by hand.
func TestExecuteRun_CleanWorkspaceWipesAVolumeAtTheRoot(t *testing.T) {
	scripts := func(f *volFixture) []string {
		f.ex.mu.Lock()
		defer f.ex.mu.Unlock()
		var out []string
		for _, c := range f.ex.calls {
			out = append(out, c.script)
		}
		return out
	}
	cleared := func(ss []string) bool {
		for _, s := range ss {
			if strings.Contains(s, "rm -rf") {
				return true
			}
		}
		return false
	}

	t.Run("job asks for it", func(t *testing.T) {
		f := newVolFixture(t, []config.VolumeConfig{vcfg("unity-main", "exclusive", "1Gi")})
		c := f.claim([]dsl.VolumeMount{mount("unity-main", "", ".")})
		c.CleanWorkspace = true
		f.a.executeRun(context.Background(), c)
		require.Equal(t, api.RunSucceeded, f.waitFinish(t))
		ss := scripts(f)
		require.NotEmpty(t, ss)
		assert.True(t, cleared(ss[:1]), "the wipe runs before the first step: %v", ss)
		assert.Contains(t, ss[0], "'/workspace'")
	})
	t.Run("podTemplate asks for it", func(t *testing.T) {
		f := newVolFixture(t, []config.VolumeConfig{vcfg("unity-main", "exclusive", "1Gi")})
		c := f.claim([]dsl.VolumeMount{mount("unity-main", "", ".")})
		c.PodTemplate = &dsl.PodTemplate{CleanWorkspace: true}
		f.a.executeRun(context.Background(), c)
		require.Equal(t, api.RunSucceeded, f.waitFinish(t))
		assert.True(t, cleared(scripts(f)))
	})
	t.Run("nobody asks", func(t *testing.T) {
		f := newVolFixture(t, []config.VolumeConfig{vcfg("unity-main", "exclusive", "1Gi")})
		f.a.executeRun(context.Background(), f.claim([]dsl.VolumeMount{mount("unity-main", "", ".")}))
		require.Equal(t, api.RunSucceeded, f.waitFinish(t))
		assert.False(t, cleared(scripts(f)), "a volume is never wiped unless the job asks")
	})
	t.Run("a volume at a subdirectory is not the workspace", func(t *testing.T) {
		f := newVolFixture(t, []config.VolumeConfig{vcfg("go-cache", "exclusive", "1Gi")})
		c := f.claim([]dsl.VolumeMount{mount("go-cache", "", ".cache")})
		c.CleanWorkspace = true
		f.a.executeRun(context.Background(), c)
		require.Equal(t, api.RunSucceeded, f.waitFinish(t))
		assert.False(t, cleared(scripts(f)), "cleanWorkspace wipes the workspace, and only a volume AT the workspace root is the workspace")
	})
	// The pod mounts every volume into the container the wipe runs in, so a
	// plain rm of the root's children would recurse through a nested mount
	// point and empty that volume while the run reported success.
	t.Run("a volume mounted below the root is not wiped", func(t *testing.T) {
		f := newVolFixture(t, []config.VolumeConfig{vcfg("unity-main", "exclusive", "1Gi"), vcfg("go-cache", "exclusive", "1Gi")})
		c := f.claim([]dsl.VolumeMount{mount("unity-main", "", "."), mount("go-cache", "", ".ucd-cache/go")})
		c.CleanWorkspace = true
		f.a.executeRun(context.Background(), c)
		require.Equal(t, api.RunSucceeded, f.waitFinish(t))
		ss := scripts(f)
		require.NotEmpty(t, ss)
		wipe := ss[0]
		assert.True(t, cleared(ss[:1]), "the root volume is still wiped: %v", ss)
		assert.Contains(t, wipe, `'/workspace/.ucd-cache/go') ;;`, "the nested mount point is skipped")
		assert.Contains(t, wipe, `'/workspace/.ucd-cache') ucd_wipe "$1/$e";`, "its parent is descended into, not removed")
		assert.NotContains(t, wipe, `'/workspace'/*`, "the root is never globbed straight into a removal")
		assert.NotContains(t, wipe, "rm -rf '/workspace", "no removal names a path directly")
	})
}

// TestK8sBackend_VolumePathsAreForTheDefaultPodOnly: a scope pod mounts no
// volume, so a path there would name something the step cannot reach. Same rule
// as the host backend.
func TestK8sBackend_VolumePathsAreForTheDefaultPodOnly(t *testing.T) {
	b := &k8sBackend{volumePaths: map[string]string{"go-cache": "/workspace/.cache"}}
	assert.Equal(t, map[string]string{"go-cache": "/workspace/.cache"}, b.VolumePaths(agentlib.ScopeHandle{}))
	assert.Nil(t, b.VolumePaths(wrapK8sScope("scope-pod")))
}

func TestClaimRequest_ReportsDeclaredAndBusyExclusiveVolumes(t *testing.T) {
	f := newVolFixture(t, []config.VolumeConfig{
		vcfg("unity-main", "exclusive", "1Gi"), vcfg("go-cache", "shared", "1Gi"), vcfg("idle", "exclusive", "1Gi"),
	})
	f.holder("r1", "unity-main", "go-cache")

	req := f.a.claimRequest(context.Background())
	assert.Equal(t, []string{"go-cache", "idle", "unity-main"}, req.Volumes)
	assert.Equal(t, []string{"unity-main"}, req.BusyVolumes, "a shared volume is never busy, and an unheld one is not either")
}

func TestClaimRequest_AnAgentWithoutVolumesSendsNothing(t *testing.T) {
	f := newVolFixture(t, nil)
	f.holder("r1", "whatever")
	assert.Equal(t, api.ClaimRequest{}, f.a.claimRequest(context.Background()))
}

// TestRun_RegistersItsVolumes: the controller routes volume jobs by what an
// agent registered, so a k8s-agent that provisioned PVCs but did not advertise
// them would never receive a job that needs one.
func TestRun_RegistersItsVolumesAndProvisionsTheirClaims(t *testing.T) {
	var mu sync.Mutex
	var reg api.AgentRegisterRequest
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, r *http.Request) { drainRequestBody(r); w.WriteHeader(http.StatusOK) }
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		_ = json.NewDecoder(r.Body).Decode(&reg)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/v1/agents/k8s-1/heartbeat", ok)
	mux.HandleFunc("DELETE /api/v1/agents/k8s-1", ok)
	mux.HandleFunc("POST /api/v1/agents/k8s-1/runs/reconcile", func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		orchestrateWriteJSON(w, map[string]int{"failedRuns": 0})
	})
	mux.HandleFunc("POST /api/v1/agents/k8s-1/claim", func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		orchestrateWriteJSON(w, api.ClaimResponse{})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cs := fake.NewSimpleClientset()
	cfg := Config{AgentID: "k8s-1", AgentName: "agent1", Namespace: testVolNS, MaxConcurrent: 2,
		Volumes: []config.VolumeConfig{vcfg("unity-main", "exclusive", "5Gi"), vcfg("go-cache", "shared", "1Gi")}}
	pm := NewPodManager(cs, testVolNS, "img")
	a := NewK8sAgent(cfg, agentlib.NewClient(srv.URL, "tok"), pm, nil, NewPodPool(cs, testVolNS, pm))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(reg.Volumes) == 2
	}, 5*time.Second, 20*time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	assert.Equal(t, []string{"go-cache", "unity-main"}, reg.Volumes)
	mu.Unlock()
	assert.Contains(t, listPVCs(t, cs), "agent1-unity-main")
	assert.Contains(t, listPVCs(t, cs), "agent1-go-cache")
}

// srcTemplate is a named AGENT pod template that moves the workspace. The job
// that names it cannot see the mount path: only the agent config has it.
func srcTemplate() map[string]AgentPodTemplate {
	return map[string]AgentPodTemplate{"golang": {
		Workspace: &dsl.WorkspaceConfig{MountPath: "/src"},
		Spec:      map[string]any{"containers": []any{map[string]any{"name": "job", "image": "golang"}}},
	}}
}

// TestExecuteRun_AgentTemplateWorkspaceIsHonouredForVolumes: the pod, the
// UNIFIED_VOLUME_* values, the backend's mount path and the cleanWorkspace wipe
// must all agree on where the workspace is. A named agent template can set
// workspace.mountPath; if the volume code looks only at the job's podTemplate it
// mounts the volume under /src, tells the step /workspace, and wipes a directory
// that is not the volume: the one deliberate way to destroy volume data
// silently does nothing.
func TestExecuteRun_AgentTemplateWorkspaceIsHonouredForVolumes(t *testing.T) {
	f := newVolFixture(t, []config.VolumeConfig{vcfg("unity-main", "exclusive", "1Gi"), vcfg("go-cache", "exclusive", "1Gi")})
	f.a.cfg.PodTemplates = srcTemplate()
	var observed []corev1.Pod
	f.a.exec = &podCapturingExec{inner: f.ex, onExec: func() { observed = f.createdPods(t) }}

	c := f.claim([]dsl.VolumeMount{mount("unity-main", "", "."), mount("go-cache", "go-cache", ".cache")})
	c.PodTemplate = &dsl.PodTemplate{Name: "golang"}
	c.CleanWorkspace = true
	f.a.executeRun(context.Background(), c)
	require.Equal(t, api.RunSucceeded, f.waitFinish(t))

	require.Len(t, observed, 1)
	mounts := map[string]string{}
	for _, m := range observed[0].Spec.Containers[0].VolumeMounts {
		mounts[m.Name] = m.MountPath
	}
	assert.Equal(t, "/src", mounts["workspace"])
	assert.Equal(t, "/src/.cache", mounts["ucd-vol-1"])

	f.ex.mu.Lock()
	defer f.ex.mu.Unlock()
	require.Len(t, f.ex.calls, 2, "the wipe, then the step")
	assert.Contains(t, f.ex.calls[0].script, "rm -rf")
	assert.Contains(t, f.ex.calls[0].script, "'/src'", "the wipe targets the directory the volume is mounted at")
	assert.NotContains(t, f.ex.calls[0].script, "'/workspace'")
	assert.Contains(t, f.ex.calls[1].env, dsl.VolumeEnvName("go-cache")+"=/src/.cache")
	assert.Contains(t, f.ex.calls[1].env, "UNIFIED_WORKSPACE=/src")
}

func TestResolveWorkspaceConfig(t *testing.T) {
	agent := srcTemplate()
	job := &dsl.WorkspaceConfig{MountPath: "/job"}
	assert.Nil(t, resolveWorkspaceConfig(agent, nil))
	assert.Equal(t, "/src", workspaceMountOf(resolveWorkspaceConfig(agent, &dsl.PodTemplate{Name: "golang"})), "a named template's workspace")
	assert.Equal(t, "/job", workspaceMountOf(resolveWorkspaceConfig(agent, &dsl.PodTemplate{Name: "golang", Workspace: job})), "the job overrides it")
	assert.Equal(t, "/job", workspaceMountOf(resolveWorkspaceConfig(agent, &dsl.PodTemplate{Spec: map[string]any{"containers": []any{}}, Workspace: job})), "an inline spec uses the job's")
	assert.Equal(t, "/workspace", workspaceMountOf(resolveWorkspaceConfig(agent, &dsl.PodTemplate{Name: "missing"})))
}

// TestBuildPod_UsesResolveWorkspaceConfig keeps the pod and the agent's idea of
// the workspace from drifting apart again: BuildPod must mount the workspace
// where resolveWorkspaceConfig says.
func TestBuildPod_UsesResolveWorkspaceConfig(t *testing.T) {
	for _, jt := range []*dsl.PodTemplate{
		nil,
		{Name: "golang"},
		{Name: "golang", Workspace: &dsl.WorkspaceConfig{MountPath: "/job"}},
		{Spec: map[string]any{"containers": []any{map[string]any{"name": "job", "image": "x"}}}, Workspace: &dsl.WorkspaceConfig{MountPath: "/inline"}},
		{Workspace: &dsl.WorkspaceConfig{MountPath: "/lone-mountpath"}},
	} {
		pod, err := BuildPod("run-abc123", testVolNS, srcTemplate(), jt, "img", SidecarSpec{}, testShimImage, nil)
		require.NoError(t, err)
		want := workspaceMountOf(resolveWorkspaceConfig(srcTemplate(), jt))
		got := ""
		for _, m := range pod.Spec.Containers[0].VolumeMounts {
			if m.Name == "workspace" {
				got = m.MountPath
			}
		}
		assert.Equal(t, want, got, "template %+v", jt)
	}
}

// An absolute mountPath that is the workspace is the workspace root, which is
// spelled ".". Left alone it gives the pod two mounts at one path and the API
// server rejects the pod with "Duplicate value" at create time.
func TestExecuteRun_RejectsAnAbsoluteMountPathThatIsTheWorkspace(t *testing.T) {
	t.Run("default workspace", func(t *testing.T) {
		f := newVolFixture(t, []config.VolumeConfig{vcfg("unity-main", "exclusive", "1Gi")})
		f.a.executeRun(context.Background(), f.claim([]dsl.VolumeMount{mount("unity-main", "", "/workspace")}))
		assert.Equal(t, api.RunFailed, f.waitFinish(t))
		assert.Contains(t, f.logText(), "is the workspace itself")
		assert.Empty(t, f.createdPods(t))
	})
	t.Run("a workspace an agent template moved", func(t *testing.T) {
		f := newVolFixture(t, []config.VolumeConfig{vcfg("unity-main", "exclusive", "1Gi")})
		f.a.cfg.PodTemplates = srcTemplate()
		c := f.claim([]dsl.VolumeMount{mount("unity-main", "", "/src")})
		c.PodTemplate = &dsl.PodTemplate{Name: "golang"}
		f.a.executeRun(context.Background(), c)
		assert.Equal(t, api.RunFailed, f.waitFinish(t))
		assert.Contains(t, f.logText(), "is the workspace itself")
		assert.Empty(t, f.createdPods(t))
	})
}

// A podTemplate that sets only workspace.mountPath (no name, no spec) builds
// the default pod, and the workspace is mounted at that path, as on the host
// agent. Every consumer of the mount path goes through resolveWorkspaceConfig.
func TestLoneWorkspaceMountPath(t *testing.T) {
	jt := &dsl.PodTemplate{Workspace: &dsl.WorkspaceConfig{MountPath: "/data"}}
	assert.Equal(t, "/data", workspaceMountOf(resolveWorkspaceConfig(srcTemplate(), jt)))

	pod, err := BuildPod("run-abc123", testVolNS, srcTemplate(), jt, "img", SidecarSpec{}, testShimImage, nil)
	require.NoError(t, err)
	var mounts []string
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == "workspace" {
			mounts = append(mounts, m.MountPath)
		}
	}
	assert.Equal(t, []string{"/data"}, mounts)
	assert.Equal(t, "/data", pod.Spec.Containers[0].WorkingDir)
}
