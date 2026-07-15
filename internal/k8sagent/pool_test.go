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
	pod1, err := pool.ClaimPod(context.Background(), "run-001", "golang", nil, nil, "golang:1.24-alpine", SidecarSpec{}, testShimImage)
	require.NoError(t, err)
	assert.NotEmpty(t, pod1.PodName)

	// Release the Pod (reuse: true)
	err = pool.ReleasePod(context.Background(), pod1, true)
	require.NoError(t, err)

	// Second Claim reuses the existing Pod
	pod2, err := pool.ClaimPod(context.Background(), "run-002", "golang", nil, nil, "golang:1.24-alpine", SidecarSpec{}, testShimImage)
	require.NoError(t, err)
	assert.Equal(t, pod1.PodName, pod2.PodName)
}

func TestPodPool_ReleaseDeletes(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	pm := NewPodManager(fakeClient, "default", "golang:1.24-alpine")
	pool := NewPodPool(fakeClient, "default", pm)

	pod, err := pool.ClaimPod(context.Background(), "run-001", "golang", nil, nil, "golang:1.24-alpine", SidecarSpec{}, testShimImage)
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
	pod, _ := BuildPod("run-existing", "default", nil, nil, "golang:1.24-alpine", SidecarSpec{}, testShimImage)
	pod.Annotations = map[string]string{
		annoPoolTemplate: "golang",
		annoPoolStatus:   poolStatusIdle,
	}
	created, _ := fakeClient.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})

	err := pool.Restore(context.Background(), nil)
	require.NoError(t, err)

	// After Restore, the Pod should be retrievable from the pool
	claimed, err := pool.ClaimPod(context.Background(), "run-new", "golang", nil, nil, "golang:1.24-alpine", SidecarSpec{}, testShimImage)
	require.NoError(t, err)
	assert.Equal(t, created.Name, claimed.PodName)
}

// TestPodPool_Restore_UnnamedTemplateReclaimed is the regression test for the
// leak this branch introduced: the pod GC now protects any pod carrying
// annoPoolStatus (idle or in-use), including ones from an unnamed/inline
// reuse podTemplate whose annoPoolTemplate is "". Restore used to key solely
// off annoPoolTemplate and `continue`d (skipped entirely) on an empty value,
// so such a pod was neither re-adopted into the pool NOR deleted — nothing
// ever reclaimed it, and every agent restart leaked one more running pod.
// Restore must now delete pool-managed pods with an empty annoPoolTemplate
// instead of skipping them, since the pool has no key to re-adopt them under.
func TestPodPool_Restore_UnnamedTemplateReclaimed(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	pm := NewPodManager(fakeClient, "default", "golang:1.24-alpine")
	pool := NewPodPool(fakeClient, "default", pm)

	// Idle pod from an unnamed (inline) reuse template: pool-status set,
	// pool-template empty.
	idlePod, _ := BuildPod("run-idle-unnamed", "default", nil, nil, "golang:1.24-alpine", SidecarSpec{}, testShimImage)
	idlePod.Name = "ucd-run-idle-unnamed"
	idlePod.Annotations = map[string]string{
		annoPoolTemplate: "",
		annoPoolStatus:   poolStatusIdle,
	}
	_, err := fakeClient.CoreV1().Pods("default").Create(context.Background(), idlePod, metav1.CreateOptions{})
	require.NoError(t, err)

	// In-use pod from an unnamed (inline) reuse template.
	inUsePod, _ := BuildPod("run-inuse-unnamed", "default", nil, nil, "golang:1.24-alpine", SidecarSpec{}, testShimImage)
	inUsePod.Name = "ucd-run-inuse-unnamed"
	inUsePod.Annotations = map[string]string{
		annoPoolTemplate: "",
		annoPoolStatus:   poolStatusInUse,
		annoPoolRunID:    "run-inuse-unnamed",
	}
	_, err = fakeClient.CoreV1().Pods("default").Create(context.Background(), inUsePod, metav1.CreateOptions{})
	require.NoError(t, err)

	err = pool.Restore(context.Background(), nil)
	require.NoError(t, err)

	// Neither pod was adopted into the pool's idle map...
	claimed, err := pool.ClaimPod(context.Background(), "run-new", "", nil, nil, "golang:1.24-alpine", SidecarSpec{}, testShimImage)
	require.NoError(t, err)
	assert.NotEqual(t, "ucd-run-idle-unnamed", claimed.PodName)
	assert.NotEqual(t, "ucd-run-inuse-unnamed", claimed.PodName)

	// ...and both were deleted from Kubernetes, not left running forever.
	pods, err := fakeClient.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	for _, p := range pods.Items {
		assert.NotEqual(t, "ucd-run-idle-unnamed", p.Name, "idle unnamed-template pod must be reclaimed on restart")
		assert.NotEqual(t, "ucd-run-inuse-unnamed", p.Name, "in-use unnamed-template pod must be reclaimed on restart")
	}
}
