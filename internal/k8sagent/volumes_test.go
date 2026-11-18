package k8sagent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

const testVolNS = "ci"

func storageOf(p corev1.PersistentVolumeClaim) string {
	q := p.Spec.Resources.Requests[corev1.ResourceStorage]
	return q.String()
}

func vcfg(name, mode, size string) api.DesiredVolume {
	return api.DesiredVolume{Name: name, Mode: mode, Size: size, ReclaimPolicy: dsl.ReclaimRetain}
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
