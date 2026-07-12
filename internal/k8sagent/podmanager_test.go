package k8sagent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestPodManager_CreatePod(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	pm := NewPodManager(fakeClient, "default", "golang:1.24-alpine")

	pod, err := pm.CreateJobPod(context.Background(), "run-abc123", map[string]string{"app": "test"})
	require.NoError(t, err)
	assert.NotEmpty(t, pod.Name)
	assert.Equal(t, "default", pod.Namespace)

	pods, err := fakeClient.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, pods.Items, 1)
	assert.Equal(t, pod.Name, pods.Items[0].Name)
}

func TestPodManager_DeletePod(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	pm := NewPodManager(fakeClient, "default", "golang:1.24-alpine")

	pod, _ := pm.CreateJobPod(context.Background(), "run-abc123", nil)
	require.NoError(t, pm.DeletePod(context.Background(), pod.Name))

	pods, _ := fakeClient.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{})
	assert.Empty(t, pods.Items)
}

func TestPodManager_BuildPodSpec(t *testing.T) {
	pm := &PodManager{namespace: "test-ns", podImage: "golang:1.24-alpine"}
	spec := pm.buildPodSpec("run-abc123")

	assert.Equal(t, corev1.RestartPolicyNever, spec.Spec.RestartPolicy)
	require.Len(t, spec.Spec.Containers, 1)
	assert.Equal(t, "job", spec.Spec.Containers[0].Name)
	assert.Equal(t, "golang:1.24-alpine", spec.Spec.Containers[0].Image)
	assert.Contains(t, spec.Name, "run-abc")
	assert.Equal(t, "test-ns", spec.Namespace)
}

func TestPodManager_CreatePod_FromBuilt(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	pm := NewPodManager(fakeClient, "default", "golang:1.24-alpine")

	pod, err := BuildPod("run-abc999", "default", nil, nil, "golang:1.24-alpine", SidecarSpec{}, testShimImage)
	require.NoError(t, err)

	created, err := pm.CreatePod(context.Background(), pod)
	require.NoError(t, err)
	assert.NotEmpty(t, created.Name)
	assert.Equal(t, "default", created.Namespace)
}

func TestPodManager_UpdatePodAnnotations(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	pm := NewPodManager(fakeClient, "default", "golang:1.24-alpine")

	pod, _ := BuildPod("run-abc999", "default", nil, nil, "golang:1.24-alpine", SidecarSpec{}, testShimImage)
	created, _ := pm.CreatePod(context.Background(), pod)

	err := pm.UpdatePodAnnotations(context.Background(), created.Name, map[string]string{
		"unified-cd/pool-status": "idle",
	}, created.ResourceVersion)
	require.NoError(t, err)

	updated, _ := fakeClient.CoreV1().Pods("default").Get(context.Background(), created.Name, metav1.GetOptions{})
	assert.Equal(t, "idle", updated.Annotations["unified-cd/pool-status"])
}
