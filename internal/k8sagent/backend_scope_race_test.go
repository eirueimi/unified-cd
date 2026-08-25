package k8sagent

import (
	"context"
	"sync"
	"testing"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestK8sBackend_EnsureScope_SameKeyCreatesOnePod drives EnsureScope
// concurrently for one scope key, which is what a parallel: group or a matrix
// whose members share a uses-scope does once the backend reports Concurrent.
//
// Two properties are asserted, and they fail for different reasons:
//
//   - Under -race, the old map[string]string version reports a data race on
//     b.scopePods — concurrent read and write of a plain map.
//   - Even without -race, the old check-then-act let both goroutines miss the
//     cache and each create a pod. One won the map entry; the other's pod was
//     orphaned, because CloseScopes only deletes pods that made it into the
//     map. The creation count is what catches that.
func TestK8sBackend_EnsureScope_SameKeyCreatesOnePod(t *testing.T) {
	pm := &fakePM{}
	a := &K8sAgent{cfg: Config{Namespace: "default"}, pm: pm}
	b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

	step := api.ClaimStep{ScopeID: "scope:build", ScopeImage: "golang:1.22"}

	const goroutines = 8
	var wg sync.WaitGroup
	payloads := make([]any, goroutines)
	errs := make([]error, goroutines)
	start := make(chan struct{})

	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			h, err := b.EnsureScope(context.Background(), step, nil)
			payloads[i], _ = agentlib.ScopeHandlePayload(h)
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	for i := range goroutines {
		require.NoError(t, errs[i], "goroutine %d", i)
	}
	assert.Equal(t, 1, pm.creations(), "one scope key must produce exactly one pod, however many steps ask for it at once")
	for i := range goroutines {
		assert.Equal(t, payloads[0], payloads[i], "every caller for one key must get the same scope pod")
	}
}

// TestK8sBackend_EnsureScope_DifferentKeysDoNotSerialize proves the in-flight
// entry is per key rather than one lock around the whole function. The fake
// blocks the first WaitForPodRunning until released; if scope creation were
// serialised under a single mutex, the second key's request would block behind
// it and this test would deadlock until the test timeout.
func TestK8sBackend_EnsureScope_DifferentKeysDoNotSerialize(t *testing.T) {
	pm := &fakePM{
		blockFirstWait: make(chan struct{}),
		waitStarted:    make(chan struct{}),
	}
	a := &K8sAgent{cfg: Config{Namespace: "default"}, pm: pm}
	b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

	first := make(chan struct{})
	go func() {
		defer close(first)
		_, _ = b.EnsureScope(context.Background(), api.ClaimStep{ScopeID: "scope:a", ScopeImage: "golang:1.22"}, nil)
	}()

	// Wait until the first key is definitely parked in its pod wait, so the
	// second key's request is unambiguously concurrent with it.
	<-pm.waitStarted

	second := make(chan error, 1)
	go func() {
		_, err := b.EnsureScope(context.Background(), api.ClaimStep{ScopeID: "scope:b", ScopeImage: "node:22"}, nil)
		second <- err
	}()

	// Under a single lock held across creation, the second key would block
	// acquiring it and this would time out instead.
	require.NoError(t, <-second, "a second scope key must not wait on the first key's pod")

	close(pm.blockFirstWait)
	<-first
}

// TestK8sBackend_EnsureScope_FailureIsNotCached proves a failed creation does
// not poison the key for the rest of the claim: the entry is dropped so a
// later step retries, rather than inheriting an error it did not cause. Scope
// pods fail for reasons that are frequently transient — image pull, quota, a
// node filling up.
func TestK8sBackend_EnsureScope_FailureIsNotCached(t *testing.T) {
	pm := &fakePM{waitErr: assert.AnError}
	a := &K8sAgent{cfg: Config{Namespace: "default"}, pm: pm}
	b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

	step := api.ClaimStep{ScopeID: "scope:build", ScopeImage: "golang:1.22"}

	_, err := b.EnsureScope(context.Background(), step, nil)
	require.Error(t, err, "the fake's wait error must surface")

	pm.mu.Lock()
	pm.waitErr = nil
	pm.mu.Unlock()

	_, err = b.EnsureScope(context.Background(), step, nil)
	require.NoError(t, err, "a later step must get its own attempt, not the cached failure")
	assert.Equal(t, 2, pm.creations(), "the retry must actually create a pod rather than return a cached entry")
}

// TestK8sBackend_CloseScopes_DoesNotDoubleDeleteRacingFailedCreate drives
// CloseScopes concurrently with an in-flight ensureScopePod attempt that is
// about to fail its WaitForPodRunning, which is exactly the ownership race
// described at CloseScopes and in createScopePod's failure branch: e.name is
// recorded before e.err is known, so a CloseScopes that reads the entry in
// that window sees e.err == nil (not yet set) and e.name != "" and would,
// without the ownership handoff, issue its own DeletePod for the same pod
// createScopePod's own failure branch also deletes.
//
// In production this cannot actually overlap (CloseScopes is deferred until
// after RunPipeline returns and runParallel has joined), so this test drives
// the two calls directly against the backend, bypassing that ordering, to
// pin the handoff itself rather than the orchestrator's scheduling. It also
// doubles as a race check on the ownership logic: fakePM.DeletePod appends to
// a slice without its own lock, so two concurrent deletes would be flagged by
// -race even before the assertions below run.
func TestK8sBackend_CloseScopes_DoesNotDoubleDeleteRacingFailedCreate(t *testing.T) {
	pm := &fakePM{
		waitErr:        assert.AnError,
		blockFirstWait: make(chan struct{}),
		waitStarted:    make(chan struct{}),
	}
	a := &K8sAgent{cfg: Config{Namespace: "default"}, pm: pm}
	b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

	step := api.ClaimStep{ScopeID: "scope:build", ScopeImage: "golang:1.22"}

	ensureDone := make(chan error, 1)
	go func() {
		_, err := b.EnsureScope(context.Background(), step, nil)
		ensureDone <- err
	}()

	// Wait until the in-flight attempt has recorded e.name and is parked in
	// WaitForPodRunning, so CloseScopes below is unambiguously racing a
	// window where e.err is still unset.
	<-pm.waitStarted

	// CloseScopes claims the entry (e.err == nil, e.name != "") and deletes
	// its pod before the in-flight attempt ever learns it failed.
	b.CloseScopes(context.Background())

	// Now let WaitForPodRunning return the failure. createScopePod's ownership
	// check must find its key no longer maps to its own entry and skip its
	// own delete.
	close(pm.blockFirstWait)
	require.Error(t, <-ensureDone, "the fake's wait error must still surface to the caller")

	assert.Equal(t, 1, pm.creations(), "exactly one scope pod should have been created")
	assert.Len(t, pm.deleted, 1, "the pod must be deleted exactly once, not once by CloseScopes and again by createScopePod's failure branch")
	if len(pm.deleted) > 0 {
		assert.Equal(t, pm.createdNm, pm.deleted[0], "the deleted pod must be the one that was created")
	}
}
