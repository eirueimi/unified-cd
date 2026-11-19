package k8sagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// ensureTestPVCs creates the PVCs of vols through a throwaway agent, as a real
// apply would, without involving the agent under test (whose pod manager may
// be instrumented).
func ensureTestPVCs(t *testing.T, cs *fake.Clientset, vols ...api.DesiredVolume) {
	t.Helper()
	if len(vols) == 0 {
		return
	}
	a := newApplyAgent(cs)
	for _, st := range a.applyVolumes(context.Background(), vols) {
		require.True(t, st.Ready, "%+v", st)
	}
}

// seedVolumes makes vols offered on a, without any API call.
func seedVolumes(a *K8sAgent, vols ...api.DesiredVolume) {
	a.volState.mu.Lock()
	defer a.volState.mu.Unlock()
	a.volState.vols = map[string]*volumeEntry{}
	a.volState.reserved = map[string]string{}
	a.volState.orphanLogged = map[string]bool{}
	for _, v := range vols {
		a.volState.vols[v.Name] = &volumeEntry{mode: v.Mode, offered: true}
		a.volState.reserved[claimNameFor("agent1", v.Name)] = v.Name
	}
}

func newApplyAgent(cs *fake.Clientset) *K8sAgent {
	return &K8sAgent{
		cfg:         Config{AgentID: "k8s-apply", AgentName: "agent1", Namespace: testVolNS, PodImage: "ubuntu:22.04"},
		pm:          volPM{NewPodManager(cs, testVolNS, "ubuntu:22.04")},
		kube:        cs,
		podBindings: newPodBindingSet(),
	}
}

// testClock is a clock several agents (replicas, or a restarted process) can
// share; advance moves it.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// clockedApplyAgent is newApplyAgent on clock c.
func clockedApplyAgent(cs *fake.Clientset, c *testClock) *K8sAgent {
	a := newApplyAgent(cs)
	a.clock = c.Now
	return a
}

func del(name, mode string) api.DesiredVolume {
	d := vcfg(name, mode, "1Gi")
	d.ReclaimPolicy = dsl.ReclaimDelete
	return d
}

func statusOf(sts []api.AgentVolumeStatus, name string) api.AgentVolumeStatus {
	for _, s := range sts {
		if s.Name == name {
			return s
		}
	}
	return api.AgentVolumeStatus{Name: "<missing " + name + ">"}
}

func addPod(t *testing.T, cs *fake.Clientset, p *corev1.Pod) {
	t.Helper()
	_, err := cs.CoreV1().Pods(testVolNS).Create(context.Background(), p, metav1.CreateOptions{})
	require.NoError(t, err)
}

func deletePod(t *testing.T, cs *fake.Clientset, name string) {
	t.Helper()
	require.NoError(t, cs.CoreV1().Pods(testVolNS).Delete(context.Background(), name, metav1.DeleteOptions{}))
}

func countVerb(cs *fake.Clientset, verb, resource string) int {
	n := 0
	for _, a := range cs.Actions() {
		if a.GetVerb() == verb && a.GetResource().Resource == resource {
			n++
		}
	}
	return n
}

func TestK8sApplyCreatesPVC(t *testing.T) {
	cs := fake.NewSimpleClientset()
	a := newApplyAgent(cs)
	fast := vcfg("fast", "shared", "20Gi")
	fast.StorageClassName = "gp3"
	sts := a.applyVolumes(context.Background(), []api.DesiredVolume{vcfg("unity-main", "exclusive", "200Gi"), fast})

	assert.Equal(t, api.AgentVolumeStatus{Name: "unity-main", Ready: true}, statusOf(sts, "unity-main"))
	assert.Equal(t, api.AgentVolumeStatus{Name: "fast", Ready: true}, statusOf(sts, "fast"))
	assert.Equal(t, []string{"fast", "unity-main"}, a.volumeNames())

	pvcs := listPVCs(t, cs)
	require.Len(t, pvcs, 2)
	um := pvcs["agent1.unity-main"]
	assert.Equal(t, "200Gi", storageOf(um))
	assert.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, um.Spec.AccessModes)
	assert.Equal(t, map[string]string{labelVolumeOwner: "agent1", labelVolumeName: "unity-main"}, um.Labels)
	assert.Equal(t, dsl.ReclaimRetain, um.Annotations[annotationReclaimPolicy])
	assert.Nil(t, um.Spec.StorageClassName, "an unset storageClassName must stay nil, not an empty string")
	assert.Empty(t, um.OwnerReferences, "an ordinary PVC, not tied to any pod")
	assert.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, pvcs["agent1.fast"].Spec.AccessModes)
	require.NotNil(t, pvcs["agent1.fast"].Spec.StorageClassName)
	assert.Equal(t, "gp3", *pvcs["agent1.fast"].Spec.StorageClassName)

	// A second apply of the same set changes nothing.
	before := countVerb(cs, "create", "persistentvolumeclaims")
	a.applyVolumes(context.Background(), []api.DesiredVolume{vcfg("unity-main", "exclusive", "200Gi"), fast})
	assert.Equal(t, before, countVerb(cs, "create", "persistentvolumeclaims"))
}

func TestK8sApplyKeepsExistingPVC(t *testing.T) {
	existing := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "agent1.go-cache", Namespace: testVolNS,
			Labels: map[string]string{labelVolumeOwner: "agent1", labelVolumeName: "go-cache", "data": "precious"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, // differs from exclusive
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: mustQty("5Gi")}, // differs from 20Gi
			},
		},
	}
	cs := fake.NewSimpleClientset(existing)
	a := newApplyAgent(cs)
	sts := a.applyVolumes(context.Background(), []api.DesiredVolume{vcfg("go-cache", "exclusive", "20Gi")})

	assert.True(t, statusOf(sts, "go-cache").Ready, "a mismatch only warns")
	assert.Zero(t, countVerb(cs, "create", "persistentvolumeclaims"), "never recreated")
	assert.Zero(t, countVerb(cs, "delete", "persistentvolumeclaims"))
	got := listPVCs(t, cs)["agent1.go-cache"]
	assert.Equal(t, "precious", got.Labels["data"])
	assert.Equal(t, "5Gi", storageOf(got), "not resized")
	assert.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, got.Spec.AccessModes, "access mode unchanged")
}

// A claim that carries no owner label was not created by this agent: it is
// used, but never annotated, so it can never be reclaimed.
func TestK8sApplyForeignPVCIsUsedButNeverReclaimable(t *testing.T) {
	foreign := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "agent1.v", Namespace: testVolNS}}
	cs := fake.NewSimpleClientset(foreign)
	a := newApplyAgent(cs)
	sts := a.applyVolumes(context.Background(), []api.DesiredVolume{del("v", "exclusive")})
	assert.True(t, statusOf(sts, "v").Ready)
	assert.Empty(t, listPVCs(t, cs)["agent1.v"].Annotations)

	a.applyVolumes(context.Background(), []api.DesiredVolume{})
	assert.Contains(t, listPVCs(t, cs), "agent1.v")
}

func TestK8sApplyUpdatesReclaimAnnotation(t *testing.T) {
	cs := fake.NewSimpleClientset()
	a := newApplyAgent(cs)
	a.applyVolumes(context.Background(), []api.DesiredVolume{vcfg("v", "exclusive", "1Gi")})
	assert.Equal(t, dsl.ReclaimRetain, listPVCs(t, cs)["agent1.v"].Annotations[annotationReclaimPolicy])

	sts := a.applyVolumes(context.Background(), []api.DesiredVolume{del("v", "exclusive")})
	assert.True(t, statusOf(sts, "v").Ready)
	assert.Equal(t, dsl.ReclaimDelete, listPVCs(t, cs)["agent1.v"].Annotations[annotationReclaimPolicy])
}

func TestK8sApplyRemoveWhileHeld(t *testing.T) {
	cs := fake.NewSimpleClientset()
	a := newApplyAgent(cs)
	ctx := context.Background()
	a.applyVolumes(ctx, []api.DesiredVolume{vcfg("u", "exclusive", "1Gi"), vcfg("other", "shared", "1Gi")})
	addPod(t, cs, claimPod("run-1", "agent1", corev1.PodRunning, "u"))

	sts := a.applyVolumes(ctx, []api.DesiredVolume{vcfg("other", "shared", "1Gi")})
	assert.Equal(t, []string{"other"}, a.volumeNames(), "a removed volume is no longer offered, even while a pod holds it")
	assert.Len(t, sts, 1)
	assert.Contains(t, listPVCs(t, cs), "agent1.u", "the PVC is kept")

	deletePod(t, cs, "run-1")
	a.applyVolumes(ctx, []api.DesiredVolume{vcfg("other", "shared", "1Gi")})
	assert.Equal(t, []string{"other"}, a.volumeNames())
	assert.Contains(t, listPVCs(t, cs), "agent1.u", "a Retain volume's PVC is never deleted")
	assert.Zero(t, countVerb(cs, "delete", "persistentvolumeclaims"))
}

func TestK8sApplyAgentNameMustBeADNSLabel(t *testing.T) {
	cs := fake.NewSimpleClientset()
	a := newApplyAgent(cs)
	a.cfg.AgentName = "Bad_Name" // the config check refuses it first; this is the defence
	sts := a.applyVolumes(context.Background(), []api.DesiredVolume{vcfg("good", "exclusive", "1Gi")})
	require.Len(t, sts, 1)
	assert.False(t, sts[0].Ready)
	assert.Contains(t, sts[0].Reason, `invalid: agentName "Bad_Name" is not usable in a PVC name`)
	assert.Empty(t, listPVCs(t, cs))
}

func TestK8sApplyInvalidDeclarations(t *testing.T) {
	cases := map[string]api.DesiredVolume{
		"bad mode":    vcfg("a", "private", "1Gi"),
		"upper name":  vcfg("Unity", "exclusive", "1Gi"),
		"empty size":  vcfg("a", "exclusive", ""),
		"bad size":    vcfg("a", "exclusive", "lots"),
		"zero size":   vcfg("a", "exclusive", "0"),
		"negative sz": vcfg("a", "exclusive", "-5Gi"),
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			cs := fake.NewSimpleClientset()
			a := newApplyAgent(cs)
			sts := a.applyVolumes(context.Background(), []api.DesiredVolume{d})
			require.Len(t, sts, 1)
			assert.False(t, sts[0].Ready)
			assert.True(t, strings.HasPrefix(sts[0].Reason, "invalid: "), sts[0].Reason)
			assert.Empty(t, listPVCs(t, cs))
			assert.Empty(t, a.volumeNames())
		})
	}
	t.Run("no agentName", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		a := newApplyAgent(cs)
		a.cfg.AgentName = ""
		sts := a.applyVolumes(context.Background(), []api.DesiredVolume{vcfg("a", "exclusive", "1Gi")})
		assert.False(t, sts[0].Ready)
		assert.Contains(t, sts[0].Reason, "agentName")
	})
	t.Run("duplicate", func(t *testing.T) {
		a := newApplyAgent(fake.NewSimpleClientset())
		sts := a.applyVolumes(context.Background(), []api.DesiredVolume{vcfg("a", "exclusive", "1Gi"), vcfg("a", "shared", "1Gi")})
		assert.Equal(t, "invalid: volume delivered more than once", sts[1].Reason)
		assert.Equal(t, []string{"a"}, a.volumeNames())
	})
}

func TestK8sApplyCreateFailureIsRetried(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var mu sync.Mutex
	fail := true
	cs.PrependReactor("create", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return true, nil, errors.New("quota exceeded")
		}
		return false, nil, nil
	})
	a := newApplyAgent(cs)
	sts := a.applyVolumes(context.Background(), []api.DesiredVolume{vcfg("v", "exclusive", "1Gi")})
	assert.Equal(t, api.AgentVolumeStatus{Name: "v", Reason: "create PVC: quota exceeded"}, sts[0])
	assert.Empty(t, a.volumeNames())

	mu.Lock()
	fail = false
	mu.Unlock()
	sts = a.applyVolumes(context.Background(), []api.DesiredVolume{vcfg("v", "exclusive", "1Gi")})
	assert.Equal(t, api.AgentVolumeStatus{Name: "v", Ready: true}, sts[0])
	assert.Equal(t, []string{"v"}, a.volumeNames())
}

func TestK8sApplyModeChangeWaitsForHolders(t *testing.T) {
	cs := fake.NewSimpleClientset()
	a := newApplyAgent(cs)
	ctx := context.Background()
	a.applyVolumes(ctx, []api.DesiredVolume{vcfg("u", "exclusive", "1Gi")})
	addPod(t, cs, claimPod("run-1", "agent1", corev1.PodRunning, "u"))

	sts := a.applyVolumes(ctx, []api.DesiredVolume{vcfg("u", "shared", "1Gi")})
	assert.False(t, sts[0].Ready)
	assert.Contains(t, sts[0].Reason, "waiting")
	assert.Empty(t, a.volumeNames(), "not offered while a pod holds it")

	deletePod(t, cs, "run-1")
	sts = a.applyVolumes(ctx, []api.DesiredVolume{vcfg("u", "shared", "1Gi")})
	assert.True(t, sts[0].Ready)
	assert.Equal(t, agentlib.VolumeModeShared, a.declaredVolumes()["u"].Mode)
	assert.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, listPVCs(t, cs)["agent1.u"].Spec.AccessModes,
		"the existing PVC's access mode is not changed")
}

func TestK8sReclaimDeletesPVC(t *testing.T) {
	ctx := context.Background()
	t.Run("Delete with no holder", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		c := newTestClock()
		a := clockedApplyAgent(cs, c)
		a.applyVolumes(ctx, []api.DesiredVolume{del("d", "exclusive")})
		a.applyVolumes(ctx, []api.DesiredVolume{})
		pvcs := listPVCs(t, cs)
		require.Contains(t, pvcs, "agent1.d", "kept during the grace")
		assert.Equal(t, c.Now().Format(time.RFC3339), pvcs["agent1.d"].Annotations[annotationLeftDesiredAt])
		c.advance(reclaimGrace - time.Second)
		a.applyVolumes(ctx, []api.DesiredVolume{})
		require.Contains(t, listPVCs(t, cs), "agent1.d", "absent for less than the grace")
		c.advance(time.Second)
		a.applyVolumes(ctx, []api.DesiredVolume{})
		assert.NotContains(t, listPVCs(t, cs), "agent1.d")
	})
	t.Run("Retain is kept", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		a := newApplyAgent(cs)
		a.applyVolumes(ctx, []api.DesiredVolume{vcfg("r", "exclusive", "1Gi")})
		a.applyVolumes(ctx, []api.DesiredVolume{})
		assert.Contains(t, listPVCs(t, cs), "agent1.r")
	})
	t.Run("a holder defers it until the pod is gone", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		c := newTestClock()
		a := clockedApplyAgent(cs, c)
		a.applyVolumes(ctx, []api.DesiredVolume{del("d", "exclusive")})
		addPod(t, cs, claimPod("run-1", "agent1", corev1.PodRunning, "d"))
		a.applyVolumes(ctx, []api.DesiredVolume{})
		c.advance(reclaimGrace)
		a.applyVolumes(ctx, []api.DesiredVolume{})
		assert.Contains(t, listPVCs(t, cs), "agent1.d", "a live pod holds it")
		deletePod(t, cs, "run-1")
		a.applyVolumes(ctx, []api.DesiredVolume{})
		assert.NotContains(t, listPVCs(t, cs), "agent1.d")
	})
	t.Run("a pod that mounts the claim without the label also holds it", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		a := newApplyAgent(cs)
		a.applyVolumes(ctx, []api.DesiredVolume{del("d", "exclusive")})
		addPod(t, cs, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: testVolNS, Labels: map[string]string{"app": "unified-cd-agent"}},
			Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "x", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "agent1.d"}}}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		})
		a.applyVolumes(ctx, []api.DesiredVolume{})
		assert.Contains(t, listPVCs(t, cs), "agent1.d")
	})
	t.Run("a volume still in the set is never reclaimed, even when it fails to apply", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		a := newApplyAgent(cs)
		a.applyVolumes(ctx, []api.DesiredVolume{del("d", "exclusive")})
		bad := del("d", "exclusive")
		bad.Size = "lots"
		a.applyVolumes(ctx, []api.DesiredVolume{bad})
		assert.Contains(t, listPVCs(t, cs), "agent1.d")
	})
	t.Run("survives an agent restart", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		c := newTestClock()
		a := clockedApplyAgent(cs, c)
		a.applyVolumes(ctx, []api.DesiredVolume{del("d", "exclusive")})
		b := clockedApplyAgent(cs, c) // a new process: no memory of the volume
		b.applyVolumes(ctx, []api.DesiredVolume{})
		c.advance(reclaimGrace)
		b.applyVolumes(ctx, []api.DesiredVolume{})
		assert.NotContains(t, listPVCs(t, cs), "agent1.d")
	})
	t.Run("a restart in the middle of the grace keeps the clock", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		c := newTestClock()
		a := clockedApplyAgent(cs, c)
		a.applyVolumes(ctx, []api.DesiredVolume{del("d", "exclusive")})
		a.applyVolumes(ctx, []api.DesiredVolume{})
		c.advance(reclaimGrace / 2)
		b := clockedApplyAgent(cs, c)
		b.applyVolumes(ctx, []api.DesiredVolume{})
		require.Contains(t, listPVCs(t, cs), "agent1.d")
		c.advance(reclaimGrace / 2)
		b.applyVolumes(ctx, []api.DesiredVolume{})
		assert.NotContains(t, listPVCs(t, cs), "agent1.d", "the annotation carried the clock over the restart")
	})
	t.Run("back in the set within the grace: kept, and the clock restarts", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		c := newTestClock()
		a := clockedApplyAgent(cs, c)
		a.applyVolumes(ctx, []api.DesiredVolume{del("d", "exclusive")})
		a.applyVolumes(ctx, []api.DesiredVolume{})
		c.advance(reclaimGrace - time.Second)
		sts := a.applyVolumes(ctx, []api.DesiredVolume{del("d", "exclusive")})
		require.True(t, statusOf(sts, "d").Ready)
		require.NotContains(t, listPVCs(t, cs)["agent1.d"].Annotations, annotationLeftDesiredAt, "cleared while desired")

		a.applyVolumes(ctx, []api.DesiredVolume{})
		c.advance(reclaimGrace - time.Second)
		a.applyVolumes(ctx, []api.DesiredVolume{})
		require.Contains(t, listPVCs(t, cs), "agent1.d", "the clock restarted when it left again")
		c.advance(time.Second)
		a.applyVolumes(ctx, []api.DesiredVolume{})
		assert.NotContains(t, listPVCs(t, cs), "agent1.d")
	})
	t.Run("a replica that still desires it clears the mark", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		c := newTestClock()
		a := clockedApplyAgent(cs, c)
		b := clockedApplyAgent(cs, c) // same agentName, diverging delivered set
		a.applyVolumes(ctx, []api.DesiredVolume{del("d", "exclusive")})
		for i := 0; i < 3; i++ {
			a.applyVolumes(ctx, []api.DesiredVolume{})
			b.applyVolumes(ctx, []api.DesiredVolume{del("d", "exclusive")})
			c.advance(reclaimGrace)
		}
		a.applyVolumes(ctx, []api.DesiredVolume{})
		assert.Contains(t, listPVCs(t, cs), "agent1.d", "never absent from every replica's set for the grace")
	})
	t.Run("a lock stamped within the lock grace defers it", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		c := newTestClock()
		a := clockedApplyAgent(cs, c)
		a.applyVolumes(ctx, []api.DesiredVolume{del("d", "exclusive")})
		a.applyVolumes(ctx, []api.DesiredVolume{})
		c.advance(reclaimGrace)
		// Another replica took the lock and is about to create its pod.
		pvc, err := cs.CoreV1().PersistentVolumeClaims(testVolNS).Get(ctx, "agent1.d", metav1.GetOptions{})
		require.NoError(t, err)
		pvc.Annotations[annoVolumeHeldBy] = "run-9"
		pvc.Annotations[annoVolumeHeldAt] = c.Now().Add(-time.Second).Format(time.RFC3339)
		_, err = cs.CoreV1().PersistentVolumeClaims(testVolNS).Update(ctx, pvc, metav1.UpdateOptions{})
		require.NoError(t, err)

		a.applyVolumes(ctx, []api.DesiredVolume{})
		require.Contains(t, listPVCs(t, cs), "agent1.d", "a fresh lock may precede its pod")
		c.advance(a.lockGrace())
		a.applyVolumes(ctx, []api.DesiredVolume{})
		assert.NotContains(t, listPVCs(t, cs), "agent1.d", "a stale lock with no pod does not hold it")
	})
	t.Run("a Retain flip delivered while the volume fails to apply is recorded", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		c := newTestClock()
		a := clockedApplyAgent(cs, c)
		a.applyVolumes(ctx, []api.DesiredVolume{del("d", "exclusive")})
		bad := vcfg("d", "exclusive", "lots") // Retain, and an invalid size
		sts := a.applyVolumes(ctx, []api.DesiredVolume{bad})
		require.False(t, statusOf(sts, "d").Ready)
		require.Equal(t, dsl.ReclaimRetain, listPVCs(t, cs)["agent1.d"].Annotations[annotationReclaimPolicy])

		a.applyVolumes(ctx, []api.DesiredVolume{})
		c.advance(reclaimGrace)
		a.applyVolumes(ctx, []api.DesiredVolume{})
		assert.Contains(t, listPVCs(t, cs), "agent1.d")
	})
	t.Run("another agent's PVC and unlabelled PVCs are never touched", func(t *testing.T) {
		mk := func(name string, labels, ann map[string]string) *corev1.PersistentVolumeClaim {
			return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testVolNS, Labels: labels, Annotations: ann}}
		}
		delAnn := map[string]string{annotationReclaimPolicy: dsl.ReclaimDelete}
		cs := fake.NewSimpleClientset(
			mk("agent2-d", map[string]string{labelVolumeOwner: "agent2", labelVolumeName: "d"}, delAnn),
			mk("agent1.nolabel", nil, delAnn),
			mk("renamed", map[string]string{labelVolumeOwner: "agent1", labelVolumeName: "d"}, delAnn), // name is not agent1.d
		)
		a := newApplyAgent(cs)
		a.applyVolumes(ctx, []api.DesiredVolume{})
		assert.Len(t, listPVCs(t, cs), 3)
	})
	t.Run("a failed delete is reported and retried", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		var mu sync.Mutex
		fail := true
		cs.PrependReactor("delete", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
			mu.Lock()
			defer mu.Unlock()
			if fail {
				return true, nil, errors.New("forbidden")
			}
			return false, nil, nil
		})
		c := newTestClock()
		a := clockedApplyAgent(cs, c)
		a.applyVolumes(ctx, []api.DesiredVolume{del("d", "exclusive")})
		require.Empty(t, a.applyVolumes(ctx, []api.DesiredVolume{}), "nothing new is reported while waiting")
		c.advance(reclaimGrace)
		sts := a.applyVolumes(ctx, []api.DesiredVolume{})
		assert.Equal(t, []api.AgentVolumeStatus{{Name: "d", Reason: "reclaim: forbidden"}}, sts)
		assert.Contains(t, listPVCs(t, cs), "agent1.d")
		mu.Lock()
		fail = false
		mu.Unlock()
		sts = a.applyVolumes(ctx, []api.DesiredVolume{})
		assert.Empty(t, sts)
		assert.NotContains(t, listPVCs(t, cs), "agent1.d")
	})
	t.Run("nothing is deleted when pods cannot be listed", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		a := newApplyAgent(cs)
		a.applyVolumes(ctx, []api.DesiredVolume{del("d", "exclusive")})
		cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("api down")
		})
		a.applyVolumes(ctx, []api.DesiredVolume{})
		assert.Contains(t, listPVCs(t, cs), "agent1.d")
	})
}

// A heartbeat that does not deliver a set (error, 204, a 200 without a
// "volumes" key) must never reach applyVolumes: an empty set reclaims every
// Delete volume.
func TestK8sNoDeleteWithoutADeliveredSet(t *testing.T) {
	for _, mode := range []string{"500", "204", "nokey", "garbage"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			cur := "set"
			beats := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/heartbeat") {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				mu.Lock()
				m := cur
				beats++
				mu.Unlock()
				switch m {
				case "500":
					http.Error(w, "boom", http.StatusInternalServerError)
				case "204":
					w.WriteHeader(http.StatusNoContent)
				case "nokey":
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"revision":"x"}`))
				case "garbage":
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte("{not json"))
				default:
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(api.HeartbeatResponse{Volumes: []api.DesiredVolume{del("d", "exclusive")}, Revision: "r1"})
				}
			}))
			t.Cleanup(srv.Close)

			cs := fake.NewSimpleClientset()
			a := newApplyAgent(cs)
			a.client = agentlib.NewClient(srv.URL, "t")
			ctx, cancel := context.WithCancel(context.Background())
			done := agentlib.StartHeartbeat(ctx, a.client, a.cfg.AgentID, 20*time.Millisecond,
				a.buildHeartbeat(func() []string { return []string{} }), func(resp *api.HeartbeatResponse) { a.onHeartbeat(ctx, resp) })
			t.Cleanup(func() { cancel(); <-done })
			require.Eventually(t, func() bool { return len(a.volumeNames()) == 1 }, 3*time.Second, 5*time.Millisecond)

			mu.Lock()
			cur = mode
			start := beats
			mu.Unlock()
			require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return beats >= start+4 }, 3*time.Second, 5*time.Millisecond)

			assert.Contains(t, listPVCs(t, cs), "agent1.d", "a beat without a delivered set must not reclaim")
			assert.Equal(t, []string{"d"}, a.volumeNames())
			assert.Zero(t, countVerb(cs, "delete", "persistentvolumeclaims"))
		})
	}
}

// A delivered empty set is authoritative: it does reclaim, and the claim then
// says the agent's (empty) list is known.
func TestK8sDeliveredEmptySetReclaims(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := newTestClock()
	a := clockedApplyAgent(cs, c)
	a.onHeartbeat(context.Background(), &api.HeartbeatResponse{Volumes: []api.DesiredVolume{del("d", "exclusive")}, Revision: "r1"})
	assert.Equal(t, []string{"d"}, a.claimRequest(context.Background()).Volumes)
	assert.True(t, a.claimRequest(context.Background()).VolumesKnown)

	a.onHeartbeat(context.Background(), &api.HeartbeatResponse{Volumes: []api.DesiredVolume{}, Revision: "r2"})
	c.advance(reclaimGrace)
	a.onHeartbeat(context.Background(), &api.HeartbeatResponse{Volumes: []api.DesiredVolume{}, Revision: "r2"})
	assert.NotContains(t, listPVCs(t, cs), "agent1.d")
	req := a.claimRequest(context.Background())
	assert.True(t, req.VolumesKnown)
	assert.Empty(t, req.Volumes)
}

// A PVC that is being deleted is not offered: it disappears as soon as its last
// pod releases it.
func TestK8sApplyTerminatingPVCNotReady(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()
	cs := fake.NewSimpleClientset(&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "agent1.d", Namespace: testVolNS, DeletionTimestamp: &now, Finalizers: []string{"kubernetes.io/pvc-protection"},
		Labels: map[string]string{labelVolumeOwner: "agent1", labelVolumeName: "d"},
	}})
	a := newApplyAgent(cs)
	sts := a.applyVolumes(ctx, []api.DesiredVolume{vcfg("d", "exclusive", "1Gi")})
	assert.Equal(t, api.AgentVolumeStatus{Name: "d", Reason: "create PVC: agent1.d is being deleted"}, statusOf(sts, "d"))
	assert.Empty(t, a.volumeNames())

	// Once it is gone, the next beat creates a fresh one and offers it.
	require.NoError(t, cs.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), testVolNS, "agent1.d"))
	sts = a.applyVolumes(ctx, []api.DesiredVolume{vcfg("d", "exclusive", "1Gi")})
	assert.True(t, statusOf(sts, "d").Ready)
	assert.Equal(t, []string{"d"}, a.volumeNames())
}

func TestK8sClaimRequestVolumesKnown(t *testing.T) {
	a := newApplyAgent(fake.NewSimpleClientset())
	req := a.claimRequest(context.Background())
	assert.False(t, req.VolumesKnown, "no set has been delivered yet")
	assert.Empty(t, req.Volumes)

	a.onHeartbeat(context.Background(), &api.HeartbeatResponse{Volumes: []api.DesiredVolume{vcfg("u", "exclusive", "1Gi")}, Revision: "r"})
	req = a.claimRequest(context.Background())
	assert.True(t, req.VolumesKnown)
	assert.Equal(t, []string{"u"}, req.Volumes)
}

func TestK8sHeartbeatReportsVolumeStatus(t *testing.T) {
	a := newApplyAgent(fake.NewSimpleClientset())
	build := a.buildHeartbeat(func() []string { return []string{} })
	assert.Empty(t, build().VolumeStatus, "nothing applied yet")
	a.onHeartbeat(context.Background(), &api.HeartbeatResponse{Volumes: []api.DesiredVolume{vcfg("u", "exclusive", "1Gi"), vcfg("Bad", "exclusive", "1Gi")}, Revision: "r"})
	got := build().VolumeStatus
	require.Len(t, got, 2)
	assert.Equal(t, api.AgentVolumeStatus{Name: "u", Ready: true}, statusOf(got, "u"))
	assert.False(t, statusOf(got, "Bad").Ready)
}

// A job that mounts a named volume's PVC directly is refused even when
// allowPersistentVolumeClaims lists the claim: the volumes are delivered at
// run time, so the old startup check became this per-claim one.
func TestK8sPodPolicyRefusesANamedVolumesClaim(t *testing.T) {
	a := newApplyAgent(fake.NewSimpleClientset())
	a.applyVolumes(context.Background(), []api.DesiredVolume{vcfg("go-cache", "exclusive", "1Gi")})
	a.cfg.JobPodPolicy.AllowPersistentVolumeClaims = []string{"agent1.go-cache", "other"}

	tmpl := func(claim string) *dsl.PodTemplate {
		return &dsl.PodTemplate{Spec: map[string]any{
			"containers": []any{map[string]any{"name": "job", "image": "golang"}},
			"volumes":    []any{map[string]any{"name": "p", "persistentVolumeClaim": map[string]any{"claimName": claim}}},
		}}
	}
	err := a.cfg.JobPodPolicy.checkJobPodTemplate(nil, tmpl("agent1.go-cache"), a.namedVolumeOfClaim)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `this agent's named volume "go-cache"; jobs reach it only through spec.volumes`)
	assert.NoError(t, a.cfg.JobPodPolicy.checkJobPodTemplate(nil, tmpl("other"), a.namedVolumeOfClaim))
}

func TestK8sConfigRejectsVolumesKey(t *testing.T) {
	const msg = `k8s agent config: "volumes" is no longer supported; declare volumes on the controller with kind: AgentVolume (see docs/operator-manual/migrations/2026-11-18-controller-declared-volumes.md)`
	for name, body := range map[string]string{
		"entries":    "volumes:\n  - name: a\n    mode: shared\n    size: 1Gi\n",
		"empty list": "volumes: []\n",
		"null":       "volumes:\n",
		"with other": "server: http://localhost:8080\nagentName: ci\nvolumes:\n  - name: a\n",
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "k8s-agent.yaml")
			require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
			_, err := LoadConfig(p)
			require.EqualError(t, err, msg)
		})
	}
	p := filepath.Join(t.TempDir(), "ok.yaml")
	require.NoError(t, os.WriteFile(p, []byte("server: http://localhost:8080\nagentName: ci\n"), 0o600))
	c, err := LoadConfig(p)
	require.NoError(t, err)
	assert.Equal(t, "ci", c.AgentName)
}

func TestK8sConfigValidateAgentName(t *testing.T) {
	c := Config{Server: "http://localhost:8080", EnrollmentPolicy: "p", AgentName: "Bad_Name"}
	err := c.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agentName")
	c.AgentName = "ci-main"
	require.NoError(t, c.Validate())
	c.AgentName = ""
	require.NoError(t, c.Validate())
}

func mustQty(s string) resource.Quantity { return resource.MustParse(s) }
