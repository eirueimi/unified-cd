package k8sagent

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

func TestClaimNameFor_Injective(t *testing.T) {
	assert.NotEqual(t, claimNameFor("a-b", "c"), claimNameFor("a", "b-c"))
	assert.Equal(t, "agent.vol", claimNameFor("agent", "vol"))
}

// A 63-character agent name and a 63-character volume name are both legal, and
// everything derived from the claim must stay within its own limit.
func TestEnsurePVC_UsesDottedName(t *testing.T) {
	agent := strings.Repeat("a", 63)
	vol := strings.Repeat("v", 63)
	cs := fake.NewSimpleClientset()
	a := newApplyAgent(cs)
	a.cfg.AgentName = agent

	sts := a.applyVolumes(context.Background(), []api.DesiredVolume{vcfg(vol, "exclusive", "1Gi")})
	require.Equal(t, api.AgentVolumeStatus{Name: vol, Ready: true}, statusOf(sts, vol))

	claim := agent + "." + vol
	pvcs := listPVCs(t, cs)
	require.Contains(t, pvcs, claim)
	assert.Empty(t, validation.IsDNS1123Subdomain(claim))
	for k, v := range pvcs[claim].Labels {
		assert.Empty(t, validation.IsQualifiedName(k), k)
		assert.Empty(t, validation.IsValidLabelValue(v), v)
	}

	vols, _, err := planPodVolumes(agent, map[string]declaredVolume{vol: {Name: vol, Mode: "exclusive"}},
		[]dsl.VolumeMount{{Name: dsl.StringOrSlice{vol}, MountPath: "cache"}}, "/workspace")
	require.NoError(t, err)
	pod, err := BuildPod("run-abc123", testVolNS, nil, nil, "img", SidecarSpec{Image: "sidecar:1"}, testShimImage, vols)
	require.NoError(t, err)
	var sawClaim bool
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			sawClaim = true
			assert.Equal(t, claim, v.PersistentVolumeClaim.ClaimName)
		}
	}
	assert.True(t, sawClaim)
	for k, v := range pod.Labels {
		assert.Empty(t, validation.IsQualifiedName(k), k)
		assert.Empty(t, validation.IsValidLabelValue(v), v)
	}
	pod.Status.Phase = corev1.PodRunning
	assert.Equal(t, map[string]bool{vol: true}, heldVolumes([]corev1.Pod{*pod}, agent))
	assert.Empty(t, heldVolumes([]corev1.Pod{*pod}, "other"))
}

func TestEnsurePVC_IgnoresLegacyDashedPVC(t *testing.T) {
	ctx := context.Background()
	legacy := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "agent1-vol", Namespace: testVolNS,
			Labels:      map[string]string{labelVolumeOwner: "agent1", labelVolumeName: "vol"},
			Annotations: map[string]string{annotationReclaimPolicy: dsl.ReclaimDelete},
		},
		Spec: corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}},
	}
	cs := fake.NewSimpleClientset(legacy)
	c := newTestClock()
	a := clockedApplyAgent(cs, c)

	d := del("vol", "exclusive")
	sts := a.applyVolumes(ctx, []api.DesiredVolume{d})
	assert.Equal(t, api.AgentVolumeStatus{Name: "vol", Ready: true}, statusOf(sts, "vol"))
	pvcs := listPVCs(t, cs)
	require.Contains(t, pvcs, "agent1.vol", "a new, empty claim is created")
	assert.Equal(t, legacy.Annotations, pvcs["agent1-vol"].Annotations, "the legacy claim is not patched")

	// The volume leaves the set for longer than the grace: only the new claim
	// is reclaimed, the legacy one is never touched.
	a.applyVolumes(ctx, nil)
	c.advance(reclaimGrace)
	a.applyVolumes(ctx, nil)
	pvcs = listPVCs(t, cs)
	assert.NotContains(t, pvcs, "agent1.vol")
	require.Contains(t, pvcs, "agent1-vol", "the legacy claim is never reclaimed")
	assert.Equal(t, legacy.Annotations, pvcs["agent1-vol"].Annotations)
	for _, act := range cs.Actions() {
		if pa, ok := act.(k8stesting.PatchAction); ok {
			assert.NotEqual(t, "agent1-vol", pa.GetName(), "legacy claim patched")
		}
		if da, ok := act.(k8stesting.DeleteAction); ok {
			assert.NotEqual(t, "agent1-vol", da.GetName(), "legacy claim deleted")
		}
	}
}

// The pod-create path takes volMu; a PVC create that is slow must not hold it.
func TestApply_PodCreateNotBlockedByPVCCreate(t *testing.T) {
	cs := fake.NewSimpleClientset()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	cs.PrependReactor("create", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		once.Do(func() { close(entered) })
		<-release
		return false, nil, nil
	})
	a := newApplyAgent(cs)

	done := make(chan struct{})
	go func() {
		defer close(done)
		a.applyVolumes(context.Background(), []api.DesiredVolume{vcfg("v", "exclusive", "1Gi")})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("the PVC create was never attempted")
	}

	locked := make(chan struct{})
	go func() {
		a.volMu.Lock()
		a.volMu.Unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("volMu was held across a PVC create")
	}
	close(release)
	<-done
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestReclaimScheduledLoggedOnce(t *testing.T) {
	var buf syncBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	c := newTestClock()
	a := clockedApplyAgent(cs, c)
	count := func() int { return strings.Count(buf.String(), "reclaim scheduled") }

	a.applyVolumes(ctx, []api.DesiredVolume{del("d", "exclusive")})
	for i := 0; i < 3; i++ {
		a.applyVolumes(ctx, nil)
	}
	assert.Equal(t, 1, count(), "once per absence period")

	a.applyVolumes(ctx, []api.DesiredVolume{del("d", "exclusive")}) // the volume returns
	for i := 0; i < 3; i++ {
		a.applyVolumes(ctx, nil) // and leaves again
	}
	assert.Equal(t, 2, count(), "a new absence period logs again")
}

func TestReclaimDelete_PreconditionConflict(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	var conflict atomic.Bool
	conflict.Store(true)
	var attempts atomic.Int32
	cs.PrependReactor("delete", "persistentvolumeclaims", func(act k8stesting.Action) (bool, runtime.Object, error) {
		attempts.Add(1)
		if conflict.Load() {
			return true, nil, apierrors.NewConflict(pvcResource.GroupResource(), act.(k8stesting.DeleteAction).GetName(), assert.AnError)
		}
		return false, nil, nil
	})
	c := newTestClock()
	a := clockedApplyAgent(cs, c)

	a.applyVolumes(ctx, []api.DesiredVolume{del("d", "exclusive")})
	a.applyVolumes(ctx, nil)
	c.advance(reclaimGrace)
	var sts []api.AgentVolumeStatus
	require.NotPanics(t, func() { sts = a.applyVolumes(ctx, nil) })
	assert.Equal(t, int32(1), attempts.Load())
	assert.Contains(t, listPVCs(t, cs), "agent1.d", "a conflicting delete leaves the PVC")
	assert.Empty(t, sts, "a conflict is not a reclaim failure; it is decided again next time")

	conflict.Store(false)
	a.applyVolumes(ctx, nil)
	assert.Equal(t, int32(2), attempts.Load(), "the next apply retries")
	assert.NotContains(t, listPVCs(t, cs), "agent1.d")
}
