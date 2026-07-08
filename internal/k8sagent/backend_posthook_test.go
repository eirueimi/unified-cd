package k8sagent

import (
	"context"
	"testing"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestK8sBackend_RunPostHook_ExecsIntoGivenContainer locks in the fix for the
// post-refactor regression where the shared orchestrator (agentlib.RunClaim)
// always drained post: hooks with an empty container string, losing the
// named-container routing a runsIn.container step's post hook needs (the
// pre-refactor k8s orchestrate loop routed a hook into the same container the
// step ran in). This drives the REAL k8sBackend.RunPostHook (not the parity
// fake) with a non-zero container and a zero scope, and asserts the exec
// lands in that container rather than the pod's default ("").
func TestK8sBackend_RunPostHook_ExecsIntoGivenContainer(t *testing.T) {
	ex := &fakeExec{exit: 0}
	a := &K8sAgent{exec: ex}
	b := newK8sBackend(a, "run-1", "pod-default", "/workspace")

	err := b.RunPostHook(context.Background(), agentlib.ScopeHandle{}, "build", "cleanup.sh", nil)
	require.NoError(t, err)

	assert.Equal(t, "pod-default", ex.gotPod, "non-scoped post hook must target the default pod")
	assert.Equal(t, "build", ex.gotContainer, "post hook must exec into the given container when non-empty and scope is zero")
	assert.Equal(t, "cleanup.sh", ex.gotScript)
}

// TestK8sBackend_RunPostHook_DefaultContainerWhenEmpty is the companion case:
// a plain step (no runsIn.container) queues its post hook with container ==
// "", and RunPostHook must still exec into the pod's default container (not
// panic / not require a non-empty container).
func TestK8sBackend_RunPostHook_DefaultContainerWhenEmpty(t *testing.T) {
	ex := &fakeExec{exit: 0}
	a := &K8sAgent{exec: ex}
	b := newK8sBackend(a, "run-1", "pod-default", "/workspace")

	err := b.RunPostHook(context.Background(), agentlib.ScopeHandle{}, "", "cleanup.sh", nil)
	require.NoError(t, err)

	assert.Equal(t, "pod-default", ex.gotPod)
	assert.Equal(t, "", ex.gotContainer, "empty container means the pod's default container")
}
