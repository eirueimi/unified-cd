package k8sagent

import (
	"context"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The tests below pin the FOURTH cleanup window — scope/Pod teardown — on the
// Kubernetes backend. RunClaim hands CloseScopes a context.WithTimeout over
// context.WithoutCancel(claim ctx) (internal/agent/orchestrator.go's teardown
// defer), and the operator-facing documentation sizes rollouts against that
// ceiling. CloseScopes previously re-stripped it — DeletePod ran on
// context.WithoutCancel(ctx), and the run-cancel-watch join was a bare
// sync.WaitGroup.Wait() — so on this backend the documented bound did not
// exist at all. Both tests hang (and fail on their own 10s bound) without the
// fix; neither is a timing race, because the fake never releases what it parks
// on.
//
// Why this matters in production and not only in a fake: no
// rest.Config.Timeout is set, and none can be — the same rest.Config drives
// exec streams and follow-mode log reads, which are legitimately long-lived
// (cmd/k8s-agent/main.go). A Kubernetes API server that accepts connections
// but stops answering therefore has nothing else to stop it.

// closeScopesWithin runs b.CloseScopes(ctx) and fails the test if it has not
// returned within limit.
func closeScopesWithin(t *testing.T, b *k8sBackend, ctx context.Context, limit time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		b.CloseScopes(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("CloseScopes did not return within %s: it did not honour its teardown budget", limit)
	}
}

// TestK8sBackend_CloseScopes_DeletePodHonoursBudget proves the per-Pod DELETE
// sweep is bounded by the caller's context. The fake parks the first (and
// only) DeletePod call on a gate that is never closed and returns ctx.Err()
// when the context ends — which is what a real DeletePod against an
// unresponsive API server does once client-go's request context expires.
func TestK8sBackend_CloseScopes_DeletePodHonoursBudget(t *testing.T) {
	pm := &fakePM{
		blockFirstDelete: make(chan struct{}), // deliberately never closed
		deleteStarted:    make(chan struct{}),
	}
	a := &K8sAgent{cfg: Config{Namespace: "default"}, pm: pm}
	b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

	// One live scope, so CloseScopes has a Pod to delete.
	_, err := b.EnsureScope(context.Background(), api.ClaimStep{ScopeID: "scope:build", ScopeImage: "golang:1.22"}, nil)
	require.NoError(t, err)
	require.Equal(t, 1, pm.creations())

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	closeScopesWithin(t, b, ctx, 10*time.Second)

	<-pm.deleteStarted // the delete was genuinely attempted, not skipped
	pm.mu.Lock()
	defer pm.mu.Unlock()
	assert.Empty(t, pm.deleted,
		"the delete must have been abandoned at the deadline, leaving the Pod to runPodGC's label sweep")
}

// TestK8sBackend_CloseScopes_WatchJoinHonoursBudget proves the run-cancel
// watch join is bounded too. sync.WaitGroup.Wait takes no context, so a watch
// goroutine that has not yet observed b.scopeCtx.Done() — parked inside a
// GetRun that the controller never answers — could pin teardown past its
// ceiling no matter what deadline the caller set.
//
// The counter is incremented directly rather than by starting a real watch:
// the real watch's GetRun runs on b.scopeCtx, which CloseScopes cancels
// moments earlier, so it cannot be made to outlive the deadline through the
// public path. What is under test is the join primitive itself.
func TestK8sBackend_CloseScopes_WatchJoinHonoursBudget(t *testing.T) {
	pm := &fakePM{}
	a := &K8sAgent{cfg: Config{Namespace: "default"}, pm: pm}
	b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

	b.runCancelWatchWG.Add(1)
	// Release it at test end so CloseScopes' own join goroutine unwinds
	// rather than leaking for the rest of the package's run.
	t.Cleanup(func() { b.runCancelWatchWG.Done() })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	closeScopesWithin(t, b, ctx, 10*time.Second)
}
