package k8sagent

import (
	"context"
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// inlineTemplate builds an unnamed (inline) reuse podTemplate whose primary
// container runs the given image. Two calls with the same image are equal in
// value but distinct in identity — exactly what poolKey must treat as "the
// same pod shape".
func inlineTemplate(image string) *dsl.PodTemplate {
	return &dsl.PodTemplate{
		Reuse: true,
		Spec: map[string]any{
			"containers": []any{
				map[string]any{"name": "job", "image": image},
			},
		},
	}
}

func TestPoolKey_Deterministic(t *testing.T) {
	// Equal-valued but separately constructed inputs must hash identically,
	// including across repeated calls (Restore depends on this stability).
	k1 := poolKey("", nil, inlineTemplate("golang:1.24-alpine"), "fallback:img", SidecarSpec{Image: "sidecar:img"}, testShimImage)
	k2 := poolKey("", nil, inlineTemplate("golang:1.24-alpine"), "fallback:img", SidecarSpec{Image: "sidecar:img"}, testShimImage)
	assert.Equal(t, k1, k2)
	assert.Len(t, k1, 32, "truncated sha256 hex digest (16 bytes)")
}

func TestPoolKey_DistinguishesInlineSpecs(t *testing.T) {
	// Two unnamed inline templates share templateName "" — the old
	// template-name pool key collided them. Differing only in container
	// image, they must produce different keys.
	kGo := poolKey("", nil, inlineTemplate("golang:1.24-alpine"), "fallback:img", SidecarSpec{}, testShimImage)
	kNode := poolKey("", nil, inlineTemplate("node:22-alpine"), "fallback:img", SidecarSpec{}, testShimImage)
	assert.NotEqual(t, kGo, kNode)

	// And identical inline specs must produce the same key.
	kGo2 := poolKey("", nil, inlineTemplate("golang:1.24-alpine"), "fallback:img", SidecarSpec{}, testShimImage)
	assert.Equal(t, kGo, kGo2)
}

func TestPoolKey_DistinguishesNamedTemplateOverrides(t *testing.T) {
	agentTmpls := map[string]AgentPodTemplate{
		"golang": {Spec: map[string]any{
			"containers": []any{map[string]any{"name": "job", "image": "golang:1.24-alpine"}},
		}},
	}
	plain := &dsl.PodTemplate{Name: "golang", Reuse: true}
	overridden := &dsl.PodTemplate{Name: "golang", Reuse: true, Override: &dsl.PodSpecPatch{
		Containers: []map[string]any{{"name": "job", "image": "golang:1.23-alpine"}},
	}}

	kPlain := poolKey("golang", agentTmpls, plain, "fallback:img", SidecarSpec{}, testShimImage)
	kOverridden := poolKey("golang", agentTmpls, overridden, "fallback:img", SidecarSpec{}, testShimImage)
	assert.NotEqual(t, kPlain, kOverridden,
		"same named template with a different per-job override is a different pod shape")

	// Only the resolved template for THIS name is hashed: adding an unrelated
	// template to the agent config must not re-key existing pools.
	withUnrelated := map[string]AgentPodTemplate{
		"golang": agentTmpls["golang"],
		"node":   {Spec: map[string]any{"containers": []any{map[string]any{"name": "job", "image": "node:22"}}}},
	}
	assert.Equal(t, kPlain, poolKey("golang", withUnrelated, plain, "fallback:img", SidecarSpec{}, testShimImage))
}

func TestPoolKey_DistinguishesNamedFromUnnamed(t *testing.T) {
	kUnnamed := poolKey("", nil, inlineTemplate("golang:1.24-alpine"), "fallback:img", SidecarSpec{}, testShimImage)
	agentTmpls := map[string]AgentPodTemplate{
		"golang": {Spec: map[string]any{
			"containers": []any{map[string]any{"name": "job", "image": "golang:1.24-alpine"}},
		}},
	}
	kNamed := poolKey("golang", agentTmpls, &dsl.PodTemplate{Name: "golang", Reuse: true}, "fallback:img", SidecarSpec{}, testShimImage)
	assert.NotEqual(t, kUnnamed, kNamed)
}

func TestPodPool_ClaimAndRelease(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	pm := NewPodManager(fakeClient, "default", "golang:1.24-alpine")
	pool := NewPodPool(fakeClient, "default", pm)

	// First Claim creates a new Pod
	pod1, err := pool.ClaimPod(context.Background(), "run-001", "golang", nil, nil, "golang:1.24-alpine", SidecarSpec{}, testShimImage)
	require.NoError(t, err)
	assert.NotEmpty(t, pod1.PodName)
	assert.NotEmpty(t, pod1.PoolKey)

	// The created pod carries the pool-key annotation for Restore.
	created, err := fakeClient.CoreV1().Pods("default").Get(context.Background(), pod1.PodName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, pod1.PoolKey, created.Annotations[annoPoolKey])

	// Release the Pod (reuse: true)
	err = pool.ReleasePod(context.Background(), pod1, true)
	require.NoError(t, err)

	// Second Claim with identical inputs reuses the existing Pod
	pod2, err := pool.ClaimPod(context.Background(), "run-002", "golang", nil, nil, "golang:1.24-alpine", SidecarSpec{}, testShimImage)
	require.NoError(t, err)
	assert.Equal(t, pod1.PodName, pod2.PodName)
	assert.Equal(t, pod1.PoolKey, pod2.PoolKey)
}

// TestPodPool_ClaimPod_NoCrossServe is the regression test for the pool-key
// collision: two unnamed inline reuse templates both have templateName "",
// and the old template-name-keyed pool handed job B job A's idle pod — wrong
// image, wrong persisted workspace. A claim must only be served an idle pod
// whose pod-shape hash matches its own.
func TestPodPool_ClaimPod_NoCrossServe(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	pm := NewPodManager(fakeClient, "default", "golang:1.24-alpine")
	pool := NewPodPool(fakeClient, "default", pm)

	// Job A: unnamed inline template, golang image. Claim then release →
	// idle pod sits in the pool under key-A.
	podA, err := pool.ClaimPod(context.Background(), "run-a", "", nil, inlineTemplate("golang:1.24-alpine"), "golang:1.24-alpine", SidecarSpec{}, testShimImage)
	require.NoError(t, err)
	require.NoError(t, pool.ReleasePod(context.Background(), podA, true))

	// Job B: also unnamed (templateName ""), but a DIFFERENT inline spec.
	// It must NOT be handed job A's pod — it creates a fresh one.
	podB, err := pool.ClaimPod(context.Background(), "run-b", "", nil, inlineTemplate("node:22-alpine"), "golang:1.24-alpine", SidecarSpec{}, testShimImage)
	require.NoError(t, err)
	assert.NotEqual(t, podA.PodName, podB.PodName, "distinct inline specs must never share a pod")
	assert.NotEqual(t, podA.PoolKey, podB.PoolKey)

	// Positive control: a claim with job A's exact inputs DOES reuse A's pod.
	podA2, err := pool.ClaimPod(context.Background(), "run-a2", "", nil, inlineTemplate("golang:1.24-alpine"), "golang:1.24-alpine", SidecarSpec{}, testShimImage)
	require.NoError(t, err)
	assert.Equal(t, podA.PodName, podA2.PodName)
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

	// Manually create an idle Pod annotated with the pool key a matching
	// claim will compute.
	key := poolKey("golang", nil, nil, "golang:1.24-alpine", SidecarSpec{}, testShimImage)
	pod, _ := BuildPod("run-existing", "default", nil, nil, "golang:1.24-alpine", SidecarSpec{}, testShimImage)
	pod.Annotations = map[string]string{
		annoPoolTemplate: "golang",
		annoPoolKey:      key,
		annoPoolStatus:   poolStatusIdle,
	}
	created, _ := fakeClient.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})

	err := pool.Restore(context.Background(), nil)
	require.NoError(t, err)

	// After Restore, the Pod should be retrievable from the pool
	claimed, err := pool.ClaimPod(context.Background(), "run-new", "golang", nil, nil, "golang:1.24-alpine", SidecarSpec{}, testShimImage)
	require.NoError(t, err)
	assert.Equal(t, created.Name, claimed.PodName)
	assert.Equal(t, key, claimed.PoolKey)
}

// TestPodPool_Restore_UnnamedTemplateReadopted covers the pool-key successor
// to the old unnamed-template delete-on-restart behavior: an idle pod from an
// unnamed (inline) reuse template — empty annoPoolTemplate — that carries the
// annoPoolKey pod-shape hash IS re-adoptable across an agent restart, because
// the pool is keyed by that hash, not by template name. It must be restored
// into the pool bucket for its key (not deleted, not leaked) and served to
// the next claim whose inputs hash to the same key.
func TestPodPool_Restore_UnnamedTemplateReadopted(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	pm := NewPodManager(fakeClient, "default", "golang:1.24-alpine")
	pool := NewPodPool(fakeClient, "default", pm)

	jobTmpl := inlineTemplate("golang:1.24-alpine")
	key := poolKey("", nil, jobTmpl, "golang:1.24-alpine", SidecarSpec{}, testShimImage)

	idlePod, _ := BuildPod("run-idle-unnamed", "default", nil, jobTmpl, "golang:1.24-alpine", SidecarSpec{}, testShimImage)
	idlePod.Name = "ucd-run-idle-unnamed"
	idlePod.Annotations = map[string]string{
		annoPoolTemplate: "", // unnamed template — key comes from the hash
		annoPoolKey:      key,
		annoPoolStatus:   poolStatusIdle,
	}
	_, err := fakeClient.CoreV1().Pods("default").Create(context.Background(), idlePod, metav1.CreateOptions{})
	require.NoError(t, err)

	err = pool.Restore(context.Background(), nil)
	require.NoError(t, err)

	// The pod sits in the pool under its annotated pool key.
	pool.mu.Lock()
	require.Len(t, pool.pods[key], 1)
	assert.Equal(t, "ucd-run-idle-unnamed", pool.pods[key][0].PodName)
	assert.Equal(t, key, pool.pods[key][0].PoolKey)
	pool.mu.Unlock()

	// It was NOT deleted from Kubernetes.
	_, err = fakeClient.CoreV1().Pods("default").Get(context.Background(), "ucd-run-idle-unnamed", metav1.GetOptions{})
	require.NoError(t, err, "re-adoptable pod must not be deleted on restart")

	// And a claim with matching inputs is served exactly this pod.
	claimed, err := pool.ClaimPod(context.Background(), "run-new", "", nil, inlineTemplate("golang:1.24-alpine"), "golang:1.24-alpine", SidecarSpec{}, testShimImage)
	require.NoError(t, err)
	assert.Equal(t, "ucd-run-idle-unnamed", claimed.PodName)
}

// TestPodPool_Restore_MissingPoolKeyDeleted covers pool-managed pods created
// by an agent build from before the pool was keyed by pod-shape hash: they
// carry annoPoolStatus but no annoPoolKey, so there is no key to re-adopt
// them under and guessing one would risk the exact wrong-pod collision the
// key exists to prevent. Restore must delete them (idle and in-use alike) —
// skipping them would leak them forever, since the GC's annoPoolStatus-based
// protection means nothing else ever deletes a pool-managed pod.
func TestPodPool_Restore_MissingPoolKeyDeleted(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	pm := NewPodManager(fakeClient, "default", "golang:1.24-alpine")
	pool := NewPodPool(fakeClient, "default", pm)

	// Idle pool pod without a pool-key annotation (pre-pool-key build).
	idlePod, _ := BuildPod("run-idle-old", "default", nil, nil, "golang:1.24-alpine", SidecarSpec{}, testShimImage)
	idlePod.Name = "ucd-run-idle-old"
	idlePod.Annotations = map[string]string{
		annoPoolTemplate: "golang",
		annoPoolStatus:   poolStatusIdle,
	}
	_, err := fakeClient.CoreV1().Pods("default").Create(context.Background(), idlePod, metav1.CreateOptions{})
	require.NoError(t, err)

	// In-use pool pod without a pool-key annotation.
	inUsePod, _ := BuildPod("run-inuse-old", "default", nil, nil, "golang:1.24-alpine", SidecarSpec{}, testShimImage)
	inUsePod.Name = "ucd-run-inuse-old"
	inUsePod.Annotations = map[string]string{
		annoPoolTemplate: "",
		annoPoolStatus:   poolStatusInUse,
		annoPoolRunID:    "run-inuse-old",
	}
	_, err = fakeClient.CoreV1().Pods("default").Create(context.Background(), inUsePod, metav1.CreateOptions{})
	require.NoError(t, err)

	err = pool.Restore(context.Background(), nil)
	require.NoError(t, err)

	// Neither pod was adopted into the pool's idle map...
	pool.mu.Lock()
	for key, idle := range pool.pods {
		assert.Empty(t, idle, "no pod should be adopted under key %q", key)
	}
	pool.mu.Unlock()

	// ...and both were deleted from Kubernetes, not left running forever.
	pods, err := fakeClient.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	for _, p := range pods.Items {
		assert.NotEqual(t, "ucd-run-idle-old", p.Name, "idle keyless pool pod must be reclaimed on restart")
		assert.NotEqual(t, "ucd-run-inuse-old", p.Name, "in-use keyless pool pod must be reclaimed on restart")
	}
}
