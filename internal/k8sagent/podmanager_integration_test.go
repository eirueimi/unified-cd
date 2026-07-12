//go:build k8s

package k8sagent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPodManager_WaitForPodRunning_Integration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client, _ := newTestKubeClient(t)
	shimImage := testShimImageOrSkip(t)
	ns := newTestNamespace(t, client)
	pm := NewPodManager(client, ns, testImage)

	pod, err := BuildPod(uniqueRunID("wait"), ns, nil, nil, testImage, SidecarSpec{}, shimImage)
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
	shimImage := testShimImageOrSkip(t)
	ns := newTestNamespace(t, client)
	pm := NewPodManager(client, ns, testImage)

	pod, err := BuildPod(uniqueRunID("waitcancel"), ns, nil, nil, testImage, SidecarSpec{}, shimImage)
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
