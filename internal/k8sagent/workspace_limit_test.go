package k8sagent

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

var q20Gi = resource.MustParse("20Gi")

func limitFixturePod() *corev1.Pod {
	return &corev1.Pod{Spec: corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: "job"},
			{Name: "side", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}}},
		},
		Volumes: []corev1.Volume{
			{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: ucdToolsVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "side-data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
	}}
}

func wsVol(p *corev1.Pod) *corev1.Volume {
	for i := range p.Spec.Volumes {
		if p.Spec.Volumes[i].Name == "workspace" {
			return &p.Spec.Volumes[i]
		}
	}
	return nil
}

func TestApplyWorkspaceLimit_SetsSizeLimitAndJobRequestOnly(t *testing.T) {
	p := limitFixturePod()
	applyWorkspaceLimit(p, q20Gi)

	sl := wsVol(p).EmptyDir.SizeLimit
	require.NotNil(t, sl)
	assert.Zero(t, sl.Cmp(q20Gi))

	got := p.Spec.Containers[0].Resources.Requests[corev1.ResourceEphemeralStorage]
	assert.Zero(t, got.Cmp(q20Gi))
	assert.Empty(t, p.Spec.Containers[0].Resources.Limits, "no container limits may be added")
	assert.NotContains(t, p.Spec.Containers[1].Resources.Requests, corev1.ResourceEphemeralStorage)
	assert.Empty(t, p.Spec.Containers[1].Resources.Limits)

	assert.Nil(t, p.Spec.Volumes[1].EmptyDir.SizeLimit, "tools volume untouched")
	assert.Nil(t, p.Spec.Volumes[2].EmptyDir.SizeLimit, "sidecar volume untouched")
}

func TestApplyWorkspaceLimit_ExistingEphemeralStorageWins(t *testing.T) {
	one := corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")}
	for name, set := range map[string]func(p *corev1.Pod){
		"request on job":     func(p *corev1.Pod) { p.Spec.Containers[0].Resources.Requests = one },
		"limit on job":       func(p *corev1.Pod) { p.Spec.Containers[0].Resources.Limits = one },
		"limit on another":   func(p *corev1.Pod) { p.Spec.Containers[1].Resources.Limits = one },
		"request on another": func(p *corev1.Pod) { p.Spec.Containers[1].Resources.Requests = one },
		"init container limit": func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "i", Resources: corev1.ResourceRequirements{Limits: one}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := limitFixturePod()
			set(p)
			want := p.DeepCopy()
			applyWorkspaceLimit(p, q20Gi)
			assert.Equal(t, want, p, "pod must be left untouched, including sizeLimit")
		})
	}
}

func TestApplyWorkspaceLimit_ExistingSizeLimitIsKept(t *testing.T) {
	p := limitFixturePod()
	five := resource.MustParse("5Gi")
	wsVol(p).EmptyDir.SizeLimit = &five
	want := p.DeepCopy()
	applyWorkspaceLimit(p, q20Gi)
	assert.Equal(t, want, p, "an existing sizeLimit leaves the pod untouched")
}

func TestApplyWorkspaceLimit_MissingPartsLeavePodUntouched(t *testing.T) {
	emptyWS := corev1.Volume{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
	pvcWS := corev1.Volume{Name: "workspace", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "c"}}}
	for name, p := range map[string]*corev1.Pod{
		"no workspace, no job":       {Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "x"}}}},
		"no workspace, with job":     {Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "job"}}}},
		"pvc workspace, job":         {Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "job"}}, Volumes: []corev1.Volume{pvcWS}}},
		"emptyDir workspace, no job": {Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "x"}}, Volumes: []corev1.Volume{emptyWS}}},
		"empty pod":                  {},
	} {
		t.Run(name, func(t *testing.T) {
			want := p.DeepCopy()
			applyWorkspaceLimit(p, q20Gi)
			assert.Equal(t, want, p)
		})
	}
	applyWorkspaceLimit(nil, q20Gi)
}

func TestPoolKey_FoldsInWorkspaceLimit(t *testing.T) {
	tmpl := inlineTemplate("golang:1.24-alpine")
	k := func(l string) string {
		return poolKeyWithLimit("", nil, tmpl, "fb", SidecarSpec{}, testShimImage, l)
	}
	assert.Equal(t, k("20Gi"), k("20Gi"))
	assert.NotEqual(t, k("20Gi"), k("50Gi"))
	assert.NotEqual(t, k("20Gi"), k(""))
	assert.Equal(t, k(""), poolKey("", nil, tmpl, "fb", SidecarSpec{}, testShimImage), "unset limit keeps the existing key")
}

func TestResolveWorkspaceSizeLimit(t *testing.T) {
	cfg := Config{
		WorkspaceSizeLimit: "20Gi",
		PodTemplates: map[string]AgentPodTemplate{
			"big":   {WorkspaceSizeLimit: "100Gi"},
			"plain": {},
		},
	}
	q, ok := resolveWorkspaceSizeLimit(cfg, "big")
	require.True(t, ok)
	assert.Equal(t, "100Gi", q.String())
	q, ok = resolveWorkspaceSizeLimit(cfg, "plain")
	require.True(t, ok)
	assert.Equal(t, "20Gi", q.String())
	q, ok = resolveWorkspaceSizeLimit(cfg, "")
	require.True(t, ok)
	assert.Equal(t, "20Gi", q.String())

	_, ok = resolveWorkspaceSizeLimit(Config{}, "")
	assert.False(t, ok)
	_, ok = resolveWorkspaceSizeLimit(Config{PodTemplates: map[string]AgentPodTemplate{"big": {WorkspaceSizeLimit: "1Gi"}}}, "other")
	assert.False(t, ok)
	// an unparsable value (Validate rejects it at startup) must not apply
	_, ok = resolveWorkspaceSizeLimit(Config{WorkspaceSizeLimit: "lots"}, "")
	assert.False(t, ok)
}

func TestBuildAndCreate_AppliesConfiguredLimit(t *testing.T) {
	pm := &fakePM{}
	a := &K8sAgent{cfg: Config{Namespace: "default", PodImage: "busybox", WorkspaceSizeLimit: "20Gi"}, pm: pm}
	_, err := a.buildAndCreate(context.Background(), api.ClaimResponse{RunID: "run1"}, SidecarSpec{}, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, pm.created)
	require.NotNil(t, wsVol(pm.created))
	sl := wsVol(pm.created).EmptyDir.SizeLimit
	require.NotNil(t, sl)
	assert.Equal(t, "20Gi", sl.String())
	var job *corev1.Container
	for i := range pm.created.Spec.Containers {
		if pm.created.Spec.Containers[i].Name == "job" {
			job = &pm.created.Spec.Containers[i]
		}
	}
	require.NotNil(t, job)
	got := job.Resources.Requests[corev1.ResourceEphemeralStorage]
	assert.Equal(t, "20Gi", got.String())
	assert.Empty(t, job.Resources.Limits)
}

func TestBuildAndCreate_NoConfigIsIdenticalToBuildPod(t *testing.T) {
	pm := &fakePM{}
	cfg := Config{Namespace: "default", PodImage: "busybox"}
	a := &K8sAgent{cfg: cfg, pm: pm}
	_, err := a.buildAndCreate(context.Background(), api.ClaimResponse{RunID: "run1"}, SidecarSpec{}, nil, nil)
	require.NoError(t, err)
	golden, err := BuildPod("run1", cfg.Namespace, cfg.PodTemplates, nil, cfg.PodImage, SidecarSpec{}, cfg.ShimImage, nil)
	require.NoError(t, err)
	assert.Equal(t, golden, pm.created)
	assert.Nil(t, wsVol(pm.created).EmptyDir.SizeLimit)
}

func TestScopePod_AppliesConfiguredLimitToItsScratchVolume(t *testing.T) {
	p := buildScopePod("run1", "default", "s", "img", nil, SidecarSpec{}, "shim", corev1.ResourceRequirements{})
	applyScopeWorkspaceLimit(p, Config{WorkspaceSizeLimit: "20Gi"})
	require.NotNil(t, wsVol(p).EmptyDir.SizeLimit)
	assert.Equal(t, "20Gi", wsVol(p).EmptyDir.SizeLimit.String())
	for _, c := range p.Spec.Containers {
		assert.Empty(t, c.Resources.Limits)
	}
	got := p.Spec.Containers[0].Resources.Requests[corev1.ResourceEphemeralStorage]
	assert.Equal(t, "20Gi", got.String(), "the scope pod's step container carries the request")

	p2 := buildScopePod("run1", "default", "s", "img", nil, SidecarSpec{}, "shim", corev1.ResourceRequirements{})
	want := p2.DeepCopy()
	applyScopeWorkspaceLimit(p2, Config{})
	assert.Equal(t, want, p2)
}

func TestPool_CreatePoolPodGetsTheTemplateLimit(t *testing.T) {
	cs := fake.NewSimpleClientset()
	pm := NewPodManager(cs, "default", "img")
	pool := NewPodPool(cs, "default", pm)
	cfg := Config{Namespace: "default", WorkspaceSizeLimit: "20Gi", PodTemplates: map[string]AgentPodTemplate{
		"big": {WorkspaceSizeLimit: "100Gi", Spec: map[string]any{"containers": []any{map[string]any{"name": "job", "image": "x"}}}},
	}}
	_ = NewK8sAgent(cfg, nil, pm, nil, pool) // wires the pool's limit resolver
	_, err := pool.createPoolPod(context.Background(), "run1", "k", cfg.PodTemplates, &dsl.PodTemplate{Name: "big", Reuse: true}, "img", SidecarSpec{}, "")
	require.NoError(t, err)
	pods, err := cs.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pods.Items, 1)
	require.NotNil(t, wsVol(&pods.Items[0]))
	require.NotNil(t, wsVol(&pods.Items[0]).EmptyDir.SizeLimit)
	assert.Equal(t, "100Gi", wsVol(&pods.Items[0]).EmptyDir.SizeLimit.String())
}

func TestPodTemplateNeedsKubernetes_NotAffectedByInjectedRequest(t *testing.T) {
	pt := &dsl.PodTemplate{Spec: map[string]any{"containers": []any{map[string]any{"name": "job", "image": "x"}}}}
	require.False(t, dsl.PodTemplateNeedsKubernetes(pt))
	// the agent-side injection works on the built pod, never on the job's template
	p := limitFixturePod()
	applyWorkspaceLimit(p, q20Gi)
	assert.False(t, dsl.PodTemplateNeedsKubernetes(pt))
}

// --- explainPodLoss ---

const evictMsg = `Usage of EmptyDir volume "workspace" exceeds the limit "20Gi".`

func evictedPod() *corev1.Pod {
	return &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted", Message: evictMsg}}
}

func lossBackend(pm *fakePM, cfg Config) *k8sBackend {
	return &k8sBackend{a: &K8sAgent{cfg: cfg, pm: pm}, runID: "r", podName: "ucd-run-abc"}
}

func TestExplainPodLoss_KubeletMessageFirstThenHint(t *testing.T) {
	orig := errors.New("stream closed")
	b := lossBackend(&fakePM{getPod: evictedPod()}, Config{WorkspaceSizeLimit: "20Gi"})
	err := b.explainPodLoss(context.Background(), "ucd-run-abc", orig)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "ucd-run-abc")
	assert.Contains(t, msg, evictMsg)
	i, j := strings.Index(msg, evictMsg), strings.Index(msg, "workspaceSizeLimit")
	require.GreaterOrEqual(t, j, 0, "hint must name workspaceSizeLimit: %s", msg)
	require.GreaterOrEqual(t, i, 0)
	assert.Less(t, i, j, "kubelet message must come before the hint")
	assert.ErrorIs(t, err, orig)
}

func TestExplainPodLoss_NoLimitConfiguredOmitsHint(t *testing.T) {
	b := lossBackend(&fakePM{getPod: evictedPod()}, Config{})
	err := b.explainPodLoss(context.Background(), "ucd-run-abc", errors.New("stream closed"))
	assert.Contains(t, err.Error(), evictMsg)
	assert.NotContains(t, err.Error(), "workspaceSizeLimit")
}

func TestExplainPodLoss_LeavesErrorAloneWhenNotALoss(t *testing.T) {
	orig := errors.New("stream closed")
	ctx := context.Background()
	cfg := Config{WorkspaceSizeLimit: "20Gi"}

	running := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	assert.Same(t, orig, lossBackend(&fakePM{getPod: running}, cfg).explainPodLoss(ctx, "p", orig))
	assert.Same(t, orig, lossBackend(&fakePM{}, cfg).explainPodLoss(ctx, "p", orig), "NotFound")
	assert.Same(t, orig, lossBackend(&fakePM{getErr: errors.New("boom")}, cfg).explainPodLoss(ctx, "p", orig))
	assert.Nil(t, lossBackend(&fakePM{getPod: evictedPod()}, cfg).explainPodLoss(ctx, "p", nil), "nil stays nil")
}

func TestExplainPodLoss_BoundsTheLookup(t *testing.T) {
	old := podLossLookupTimeout
	assert.Equal(t, 5*time.Second, old, "the production bound is 5s")
	podLossLookupTimeout = 50 * time.Millisecond
	t.Cleanup(func() { podLossLookupTimeout = old })
	orig := errors.New("stream closed")
	b := lossBackend(&fakePM{getBlock: true}, Config{WorkspaceSizeLimit: "20Gi"})
	start := time.Now()
	err := b.explainPodLoss(context.Background(), "p", orig)
	assert.Same(t, orig, err)
	assert.Less(t, time.Since(start), 2*time.Second)
}

func TestRunDefault_WrapsExecErrorOfAnEvictedPod(t *testing.T) {
	pm := &fakePM{getPod: evictedPod()}
	ex := &fakeExec{exit: 1, err: errors.New("stream closed")}
	a := &K8sAgent{cfg: Config{WorkspaceSizeLimit: "20Gi"}, pm: pm, exec: ex}
	b := &k8sBackend{a: a, podName: "ucd-run-abc"}
	_, err := b.RunDefault(context.Background(), api.ClaimStep{}, "true", nil, io.Discard, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), evictMsg)
}
