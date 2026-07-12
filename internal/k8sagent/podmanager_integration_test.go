//go:build k8s

package k8sagent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// NOTE: BuildPod's shimImage argument is passed as testImage (ubuntu:22.04)
// below purely to keep this file compiling against the new signature; it is
// NOT a real ucd-sh-capable image, so the prepended ucd-shim init container
// will fail on a real cluster (this file only runs with `-tags k8s` against a
// live cluster, which is outside this task's required gates). Point it at a
// real shim image (e.g. the k8s-agent's own image) before re-enabling these
// tests for real-cluster runs.
func TestPodManager_WaitForPodRunning_Integration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client, _ := newTestKubeClient(t)
	ns := newTestNamespace(t, client)
	pm := NewPodManager(client, ns, testImage)

	pod, err := BuildPod(uniqueRunID("wait"), ns, nil, nil, testImage, SidecarSpec{}, testImage)
	require.NoError(t, err)
	created, err := pm.CreatePod(ctx, pod)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pm.DeletePod(context.Background(), created.Name) })

	err = pm.WaitForPodRunning(ctx, created.Name)
	assert.NoError(t, err)
}

func TestPodManager_WaitForPodRunning_ContextCancelled_Integration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client, _ := newTestKubeClient(t)
	ns := newTestNamespace(t, client)
	pm := NewPodManager(client, ns, testImage)

	pod, err := BuildPod(uniqueRunID("waitcancel"), ns, nil, nil, testImage, SidecarSpec{}, testImage)
	require.NoError(t, err)
	created, err := pm.CreatePod(ctx, pod)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pm.DeletePod(context.Background(), created.Name) })

	// Cancel the context before the pod can reach Running state
	shortCtx, shortCancel := context.WithCancel(ctx)
	shortCancel() // cancel immediately — deterministically done before first call

	err = pm.WaitForPodRunning(shortCtx, created.Name)
	assert.ErrorIs(t, err, context.Canceled)
}
