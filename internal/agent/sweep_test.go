package agent

import (
	"context"
	"testing"
	"time"

	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/stretchr/testify/assert"
)

func seededRT() *podFakeRT {
	mk := func(id string, labels map[string]string) crt.ContainerInfo {
		return crt.ContainerInfo{Handle: crt.ContainerHandle{ID: id}, Labels: labels}
	}
	return &podFakeRT{containers: map[string]crt.ContainerInfo{
		"k1": mk("k1", map[string]string{labelAgentID: "a1", labelClaimPod: "true"}),
		"k2": mk("k2", map[string]string{labelAgentID: "a1", labelScope: "true"}),
		"k3": mk("k3", map[string]string{labelAgentID: "a2", labelClaimPod: "true"}),
		"k4": mk("k4", map[string]string{labelAgentID: "a1"}),
		"k5": mk("k5", nil),
	}}
}

func TestStartupSweep_RemovesOnlyOwnLabelledContainers(t *testing.T) {
	f := seededRT()
	removed := sweepOrphanContainers(context.Background(), f, "a1")
	assert.Equal(t, []string{"k1", "k2"}, removed)
	assert.Equal(t, []string{"k1", "k2"}, f.removed)
	assert.Contains(t, f.containers, "k3")
	assert.Contains(t, f.containers, "k4")
	assert.Contains(t, f.containers, "k5")
}

// listAllRT returns every container regardless of the filter, to prove the
// sweep re-checks the returned labels itself.
type listAllRT struct{ *podFakeRT }

func (l listAllRT) ListContainers(ctx context.Context, _ map[string]string) ([]crt.ContainerInfo, error) {
	return l.podFakeRT.ListContainers(ctx, nil)
}

func TestStartupSweep_RechecksLabels(t *testing.T) {
	f := seededRT()
	removed := sweepOrphanContainers(context.Background(), listAllRT{f}, "a1")
	assert.Equal(t, []string{"k1", "k2"}, removed)
}

func TestStartupSweep_NoPodRuntimeIsNoop(t *testing.T) {
	f := &fakeRT{} // does not implement crt.PodRuntime
	assert.Empty(t, sweepOrphanContainers(context.Background(), f, "a1"))
	assert.Equal(t, 0, f.removed)
}

func TestStartupSweep_EmptyAgentIDIsNoop(t *testing.T) {
	f := seededRT()
	assert.Empty(t, sweepOrphanContainers(context.Background(), f, ""))
	assert.Empty(t, f.removed)
}

func TestLabelHelpers(t *testing.T) {
	assert.Nil(t, claimPodLabels(""))
	assert.Nil(t, scopeLabels(""))
	assert.Equal(t, map[string]string{labelAgentID: "a1", labelClaimPod: "true"}, claimPodLabels("a1"))
	assert.Equal(t, map[string]string{labelAgentID: "a1", labelScope: "true"}, scopeLabels("a1"))
}

// blockingListRT blocks ListContainers until ctx is done.
type blockingListRT struct{ *podFakeRT }

func (b blockingListRT) ListContainers(ctx context.Context, _ map[string]string) ([]crt.ContainerInfo, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestStartupSweep_TimesOutOnWedgedRuntime(t *testing.T) {
	prev := sweepTimeout
	sweepTimeout = 50 * time.Millisecond
	t.Cleanup(func() { sweepTimeout = prev })
	done := make(chan []string, 1)
	go func() { done <- sweepOrphanContainers(context.Background(), blockingListRT{seededRT()}, "a1") }()
	select {
	case removed := <-done:
		assert.Empty(t, removed)
	case <-time.After(5 * time.Second):
		t.Fatal("sweep did not return after its timeout")
	}
}
