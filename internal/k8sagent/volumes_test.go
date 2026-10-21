package k8sagent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

const testVolNS = "ci"

func storageOf(p corev1.PersistentVolumeClaim) string {
	q := p.Spec.Resources.Requests[corev1.ResourceStorage]
	return q.String()
}

func vcfg(name, mode, size string) config.VolumeConfig {
	return config.VolumeConfig{Name: name, Mode: mode, Size: size}
}

func listPVCs(t *testing.T, cs *fake.Clientset) map[string]corev1.PersistentVolumeClaim {
	t.Helper()
	l, err := cs.CoreV1().PersistentVolumeClaims(testVolNS).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	out := map[string]corev1.PersistentVolumeClaim{}
	for _, p := range l.Items {
		out[p.Name] = p
	}
	return out
}

func TestEnsurePVCs_CreatesOnePerVolume(t *testing.T) {
	cs := fake.NewSimpleClientset()
	err := EnsurePVCs(context.Background(), cs, testVolNS, "agent1", []config.VolumeConfig{
		vcfg("unity-main", "exclusive", "200Gi"),
		vcfg("go-cache", "exclusive", "20Gi"),
	})
	require.NoError(t, err)

	pvcs := listPVCs(t, cs)
	require.Len(t, pvcs, 2)
	got := pvcs["agent1-unity-main"]
	assert.Equal(t, "200Gi", storageOf(got))
	// An ordinary PVC, not a generic ephemeral volume: it must outlive any pod.
	assert.Empty(t, got.OwnerReferences)
}

func TestEnsurePVCs_IsIdempotentAndLeavesAnExistingClaimAlone(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cfgs := []config.VolumeConfig{vcfg("go-cache", "exclusive", "20Gi")}
	require.NoError(t, EnsurePVCs(context.Background(), cs, testVolNS, "agent1", cfgs))
	before := listPVCs(t, cs)["agent1-go-cache"]

	// A bigger size in the config must not rewrite a claim that holds data.
	cfgs[0].Size = "40Gi"
	require.NoError(t, EnsurePVCs(context.Background(), cs, testVolNS, "agent1", cfgs))
	after := listPVCs(t, cs)
	require.Len(t, after, 1)
	assert.Equal(t, storageOf(before), storageOf(after["agent1-go-cache"]))
}

func TestEnsurePVCs_ExclusiveIsRWO_SharedIsRWX(t *testing.T) {
	cs := fake.NewSimpleClientset()
	require.NoError(t, EnsurePVCs(context.Background(), cs, testVolNS, "agent1", []config.VolumeConfig{
		vcfg("unity-main", "exclusive", "200Gi"),
		vcfg("go-cache", "shared", "20Gi"),
	}))
	pvcs := listPVCs(t, cs)
	assert.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, pvcs["agent1-unity-main"].Spec.AccessModes)
	assert.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, pvcs["agent1-go-cache"].Spec.AccessModes)
}

// TestEnsurePVCs_UnsetStorageClassLeavesTheFieldNil is the spec section 10
// defect: storageClassName "" means "no StorageClass" in Kubernetes, so the
// cluster default is not applied and the claim stays Pending. Unset must be
// nil, not a pointer to the empty string.
func TestEnsurePVCs_UnsetStorageClassLeavesTheFieldNil(t *testing.T) {
	cs := fake.NewSimpleClientset()
	withClass := vcfg("fast", "exclusive", "10Gi")
	withClass.StorageClassName = "gp3"
	require.NoError(t, EnsurePVCs(context.Background(), cs, testVolNS, "agent1", []config.VolumeConfig{
		vcfg("plain", "exclusive", "10Gi"), withClass,
	}))
	pvcs := listPVCs(t, cs)

	assert.Nil(t, pvcs["agent1-plain"].Spec.StorageClassName, "an unset storageClassName must not be sent as an empty string")
	require.NotNil(t, pvcs["agent1-fast"].Spec.StorageClassName)
	assert.Equal(t, "gp3", *pvcs["agent1-fast"].Spec.StorageClassName)
}

func TestEnsurePVCs_RequiresSize(t *testing.T) {
	cs := fake.NewSimpleClientset()
	for _, size := range []string{"", "lots", "0", "-5Gi"} {
		err := EnsurePVCs(context.Background(), cs, testVolNS, "agent1", []config.VolumeConfig{vcfg("go-cache", "exclusive", size)})
		require.Error(t, err, "size %q", size)
		assert.Contains(t, err.Error(), "go-cache")
		assert.Contains(t, err.Error(), "size")
	}
	assert.Empty(t, listPVCs(t, cs), "an invalid config must create nothing")
}

func TestEnsurePVCs_RejectsAnUnusableConfig(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cases := map[string][]config.VolumeConfig{
		"bad mode":        {vcfg("a", "private", "1Gi")},
		"empty mode":      {vcfg("a", "", "1Gi")},
		"upper case name": {vcfg("Unity", "exclusive", "1Gi")},
		"underscore name": {vcfg("go_cache", "exclusive", "1Gi")},
		"duplicate":       {vcfg("a", "exclusive", "1Gi"), vcfg("a", "shared", "1Gi")},
	}
	for name, cfgs := range cases {
		t.Run(name, func(t *testing.T) {
			require.Error(t, EnsurePVCs(context.Background(), cs, testVolNS, "agent1", cfgs))
			assert.Empty(t, listPVCs(t, cs))
		})
	}
	t.Run("no agent name", func(t *testing.T) {
		require.Error(t, EnsurePVCs(context.Background(), cs, testVolNS, "", []config.VolumeConfig{vcfg("a", "exclusive", "1Gi")}))
	})
	t.Run("claim name too long for a label", func(t *testing.T) {
		long := "v-1234567890123456789012345678901234567890123456789012345678"
		require.Error(t, EnsurePVCs(context.Background(), cs, testVolNS, "agent1", []config.VolumeConfig{vcfg(long, "exclusive", "1Gi")}))
	})
}

func TestEnsurePVCs_PrefixesWithAgentName(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cfgs := []config.VolumeConfig{vcfg("go-cache", "exclusive", "20Gi")}
	require.NoError(t, EnsurePVCs(context.Background(), cs, testVolNS, "ci-a", cfgs))
	require.NoError(t, EnsurePVCs(context.Background(), cs, testVolNS, "ci-b", cfgs))

	pvcs := listPVCs(t, cs)
	assert.Contains(t, pvcs, "ci-a-go-cache")
	assert.Contains(t, pvcs, "ci-b-go-cache")
	assert.Len(t, pvcs, 2, "two agents in one namespace keep independent copies")
}

// TestEnsurePVCs_DoesNotDeleteAPVCForARemovedVolume: removing a line from the
// agent config must not destroy tens of gigabytes. The claim stays; the agent
// only stops advertising it.
func TestEnsurePVCs_DoesNotDeleteAPVCForARemovedVolume(t *testing.T) {
	cs := fake.NewSimpleClientset()
	ctx := context.Background()
	require.NoError(t, EnsurePVCs(ctx, cs, testVolNS, "agent1", []config.VolumeConfig{
		vcfg("unity-main", "exclusive", "200Gi"), vcfg("go-cache", "exclusive", "20Gi"),
	}))

	require.NoError(t, EnsurePVCs(ctx, cs, testVolNS, "agent1", []config.VolumeConfig{vcfg("go-cache", "exclusive", "20Gi")}))
	assert.Contains(t, listPVCs(t, cs), "agent1-unity-main", "a volume dropped from the config must keep its PVC")

	require.NoError(t, EnsurePVCs(ctx, cs, testVolNS, "agent1", nil))
	assert.Len(t, listPVCs(t, cs), 2, "an empty config deletes nothing either")

	for _, a := range cs.Actions() {
		assert.NotEqual(t, "delete", a.GetVerb(), "EnsurePVCs must never issue a delete: %v", a)
	}
}

func TestEnsurePVCs_CreateRaceIsNotAnError(t *testing.T) {
	cs := fake.NewSimpleClientset()
	// Another replica creates the claim between our Get and our Create.
	cs.PrependReactor("create", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewAlreadyExists(corev1.Resource("persistentvolumeclaims"), "agent1-go-cache")
	})
	require.NoError(t, EnsurePVCs(context.Background(), cs, testVolNS, "agent1", []config.VolumeConfig{vcfg("go-cache", "exclusive", "20Gi")}))
}

// claimPod is a claim pod of the given agent holding the named volumes.
func claimPod(name, agent string, phase corev1.PodPhase, volumes ...string) *corev1.Pod {
	labels := map[string]string{"app": "unified-cd-agent"}
	for _, v := range volumes {
		labels[volumeLabelPrefix+agent+"-"+v] = v
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testVolNS, Labels: labels},
		Status:     corev1.PodStatus{Phase: phase},
	}
}

// TestBusyVolumes_FromLivePods: occupancy is read from the cluster, so two
// replicas under one agent ID (and a restarted one) agree without sharing any
// process state.
func TestBusyVolumes_FromLivePods(t *testing.T) {
	cs := fake.NewSimpleClientset(
		claimPod("run-1", "agent1", corev1.PodRunning, "unity-main"),
		claimPod("run-2", "agent1", corev1.PodPending, "go-cache", "tool-cache"),
		claimPod("run-3", "agent1", corev1.PodSucceeded, "finished-vol"),
		claimPod("run-4", "agent1", corev1.PodFailed, "failed-vol"),
		claimPod("run-5", "agent2", corev1.PodRunning, "someone-elses"),
		claimPod("run-6", "agent1", corev1.PodRunning), // holds no volume
	)

	replicaA, err := BusyVolumes(context.Background(), cs, "agent1", testVolNS)
	require.NoError(t, err)
	replicaB, err := BusyVolumes(context.Background(), cs, "agent1", testVolNS)
	require.NoError(t, err)

	want := []string{"go-cache", "tool-cache", "unity-main"}
	assert.Equal(t, want, replicaA)
	assert.Equal(t, replicaA, replicaB, "two replicas must see the same occupancy")

	other, err := BusyVolumes(context.Background(), cs, "agent2", testVolNS)
	require.NoError(t, err)
	assert.Equal(t, []string{"someone-elses"}, other)
}

func TestBusyVolumes_NothingHeldIsEmptyNotNil(t *testing.T) {
	got, err := BusyVolumes(context.Background(), fake.NewSimpleClientset(), "agent1", testVolNS)
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)
}

// TestBusyVolumes_ReadsWhatBuildPodLabels closes the loop between the writer
// and the reader: a pod BuildPod produced is busy, and a volume nobody mounted
// is not.
func TestBusyVolumes_ReadsWhatBuildPodLabels(t *testing.T) {
	pod, err := BuildPod("run-abc123", testVolNS, nil, nil, "img", SidecarSpec{}, testShimImage, []PodVolume{
		{Name: "go-cache", ClaimName: "agent1-go-cache", MountPath: ".ucd-cache/go"},
	})
	require.NoError(t, err)
	cs := fake.NewSimpleClientset(pod)

	got, err := BusyVolumes(context.Background(), cs, "agent1", testVolNS)
	require.NoError(t, err)
	assert.Equal(t, []string{"go-cache"}, got)
}

func TestBuildPod_MountsNamedVolumes(t *testing.T) {
	vols := []PodVolume{
		{Name: "unity-main", ClaimName: "agent1-unity-main", MountPath: "."},
		{Name: "go-cache", ClaimName: "agent1-go-cache", MountPath: ".ucd-cache/go"},
		{Name: "tool-cache", ClaimName: "agent1-tool-cache", MountPath: "/root/.cache"},
	}
	jobTmpl := &dsl.PodTemplate{Spec: map[string]any{
		"containers": []any{
			map[string]any{"name": "job", "image": "golang"},
			map[string]any{"name": "db", "image": "postgres"},
		},
	}}
	pod, err := BuildPod("run-abc123", testVolNS, nil, jobTmpl, "img", SidecarSpec{Image: "sidecar:1"}, testShimImage, vols)
	require.NoError(t, err)

	volByName := map[string]corev1.Volume{}
	for _, v := range pod.Spec.Volumes {
		volByName[v.Name] = v
	}

	// The root volume IS the workspace: the PVC takes the emptyDir's place.
	ws := volByName["workspace"]
	require.NotNil(t, ws.PersistentVolumeClaim, "a volume at . replaces the workspace emptyDir")
	assert.Nil(t, ws.EmptyDir)
	assert.Equal(t, "agent1-unity-main", ws.PersistentVolumeClaim.ClaimName)

	// Every container sees every mount, the sidecar included, so an artifact or
	// cache path inside a volume reads the volume and not an empty mountpoint.
	want := map[string]string{
		"workspace": "/workspace",
		"ucd-vol-1": "/workspace/.ucd-cache/go",
		"ucd-vol-2": "/root/.cache",
	}
	require.Len(t, pod.Spec.Containers, 3, "job, db and the artifact sidecar")
	for _, c := range pod.Spec.Containers {
		got := map[string]string{}
		for _, m := range c.VolumeMounts {
			got[m.Name] = m.MountPath
		}
		for name, mp := range want {
			assert.Equal(t, mp, got[name], "container %s mount %s", c.Name, name)
		}
	}
	assert.Equal(t, "agent1-go-cache", volByName["ucd-vol-1"].PersistentVolumeClaim.ClaimName)
	assert.Equal(t, "agent1-tool-cache", volByName["ucd-vol-2"].PersistentVolumeClaim.ClaimName)

	// The workspace mount comes before anything nested inside it.
	idx := map[string]int{}
	for i, m := range pod.Spec.Containers[0].VolumeMounts {
		idx[m.Name] = i
	}
	assert.Less(t, idx["workspace"], idx["ucd-vol-1"])

	// The pod is labelled with what it holds; that label is what BusyVolumes reads.
	assert.Equal(t, "unity-main", pod.Labels[volumeLabelPrefix+"agent1-unity-main"])
	assert.Equal(t, "go-cache", pod.Labels[volumeLabelPrefix+"agent1-go-cache"])
	assert.Equal(t, "tool-cache", pod.Labels[volumeLabelPrefix+"agent1-tool-cache"])
	assert.Equal(t, "unified-cd-agent", pod.Labels["app"], "the pod GC still finds the pod")
}

func TestBuildPod_MountsVolumesRelativeToACustomWorkspace(t *testing.T) {
	jobTmpl := &dsl.PodTemplate{
		Workspace: &dsl.WorkspaceConfig{MountPath: "/src"},
		Spec:      map[string]any{"containers": []any{map[string]any{"name": "job", "image": "golang"}}},
	}
	pod, err := BuildPod("run-abc123", testVolNS, nil, jobTmpl, "img", SidecarSpec{}, testShimImage, []PodVolume{
		{Name: "go-cache", ClaimName: "agent1-go-cache", MountPath: "cache/go"},
	})
	require.NoError(t, err)
	got := map[string]string{}
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		got[m.Name] = m.MountPath
	}
	assert.Equal(t, "/src/cache/go", got["ucd-vol-0"])
}

func TestBuildPod_NoVolumesAddsNoLabelsOrClaims(t *testing.T) {
	pod, err := BuildPod("run-abc123", testVolNS, nil, nil, "img", SidecarSpec{}, testShimImage, nil)
	require.NoError(t, err)
	for k := range pod.Labels {
		assert.NotContains(t, k, volumeLabelPrefix)
	}
	for _, v := range pod.Spec.Volumes {
		assert.Nil(t, v.PersistentVolumeClaim)
	}
}

func validVolumeConfig() Config {
	return Config{Server: "http://localhost:8080", EnrollmentPolicy: "p", AgentName: "ci-main",
		Volumes: []config.VolumeConfig{vcfg("go-cache", "exclusive", "20Gi")}}
}

func TestConfigValidate_Volumes(t *testing.T) {
	c := validVolumeConfig()
	require.NoError(t, c.Validate())

	t.Run("no volumes needs no agentName", func(t *testing.T) {
		c := validVolumeConfig()
		c.AgentName, c.Volumes = "", nil
		require.NoError(t, c.Validate())
	})
	t.Run("volumes need an agentName", func(t *testing.T) {
		c := validVolumeConfig()
		c.AgentName = ""
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "agentName")
	})
	t.Run("size is required", func(t *testing.T) {
		c := validVolumeConfig()
		c.Volumes[0].Size = ""
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "size")
	})
}

func TestLoadConfig_Volumes(t *testing.T) {
	path := writeTempYAML(t, `
server: http://localhost:8080
agentName: ci-main
volumes:
  - name: unity-main
    mode: exclusive
    size: 200Gi
    storageClassName: gp3
  - name: go-cache
    mode: shared
    size: 20Gi
`)
	got, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "ci-main", got.AgentName)
	require.Len(t, got.Volumes, 2)
	assert.Equal(t, config.VolumeConfig{Name: "unity-main", Mode: "exclusive", Size: "200Gi", StorageClassName: "gp3"}, got.Volumes[0])
	assert.Equal(t, "shared", got.Volumes[1].Mode)
}
