package k8sagent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestPodPool_ClaimAndRelease(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	pm := NewPodManager(fakeClient, "default", "golang:1.24-alpine")
	pool := NewPodPool(fakeClient, "default", pm)

	// First Claim creates a new Pod
	pod1, err := pool.ClaimPod(context.Background(), "run-001", "golang", nil, nil, "golang:1.24-alpine")
	require.NoError(t, err)
	assert.NotEmpty(t, pod1.PodName)

	// Release the Pod (reuse: true)
	err = pool.ReleasePod(context.Background(), pod1, true)
	require.NoError(t, err)

	// Second Claim reuses the existing Pod
	pod2, err := pool.ClaimPod(context.Background(), "run-002", "golang", nil, nil, "golang:1.24-alpine")
	require.NoError(t, err)
	assert.Equal(t, pod1.PodName, pod2.PodName)
}

func TestPodPool_ReleaseDeletes(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	pm := NewPodManager(fakeClient, "default", "golang:1.24-alpine")
	pool := NewPodPool(fakeClient, "default", pm)

	pod, err := pool.ClaimPod(context.Background(), "run-001", "golang", nil, nil, "golang:1.24-alpine")
	require.NoError(t, err)

	// reuse: false → delete
	err = pool.ReleasePod(context.Background(), pod, false)
	require.NoError(t, err)

	pods, _ := fakeClient.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{})
	assert.Empty(t, pods.Items)
}

func TestPodPool_Restore_IdlePod(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	pm := NewPodManager(fakeClient, "default", "golang:1.24-alpine")
	pool := NewPodPool(fakeClient, "default", pm)

	// Manually create an idle Pod
	pod, _ := BuildPod("run-existing", "default", nil, nil, "golang:1.24-alpine")
	pod.Annotations = map[string]string{
		annoPoolTemplate: "golang",
		annoPoolStatus:   poolStatusIdle,
	}
	created, _ := fakeClient.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})

	err := pool.Restore(context.Background(), nil)
	require.NoError(t, err)

	// After Restore, the Pod should be retrievable from the pool
	claimed, err := pool.ClaimPod(context.Background(), "run-new", "golang", nil, nil, "golang:1.24-alpine")
	require.NoError(t, err)
	assert.Equal(t, created.Name, claimed.PodName)
}
