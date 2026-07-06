package k8sagent

import (
	"context"
	"io"
	"testing"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestK8sBackend_RunImage_HonorsExpandedEnv locks in the fix for the
// regression where k8sBackend.RunImage ignored its env parameter (the
// orchestrator's already-template-expanded "KEY=VALUE" pairs) and instead
// rebuilt the pod's env from the raw, unexpanded step.Env map. This test
// drives the REAL k8sBackend (not a parity fake — parity's fake podManager
// bypasses this code path entirely and would not catch the regression) so the
// pod spec actually created by RunImage is inspected.
//
// The step's raw Env still carries the literal template string
// ("{{ .Params.x }}"), mirroring what a ClaimStep looks like before the
// orchestrator expands it. env (RunImage's parameter) carries the resolved
// value plus UNIFIED_AGENT_OS=linux, exactly as internal/agent/orchestrator.go
// builds extraEnv before calling b.RunImage(...).
func TestK8sBackend_RunImage_HonorsExpandedEnv(t *testing.T) {
	pm := &fakePM{}
	ex := &fakeExec{stdout: "ok\n", exit: 0}
	a := &K8sAgent{pm: pm, exec: ex}
	b := newK8sBackend(a, "run-1", "pod-default", "/workspace")

	step := api.ClaimStep{
		Env:    map[string]string{"TOKEN": "{{ .Params.x }}"},
		RunsIn: &dsl.RunsIn{Image: "alpine:3.20"},
	}

	env := []string{"TOKEN=resolved-value", "UNIFIED_AGENT_OS=linux"}

	code, err := b.RunImage(context.Background(), step, "echo hi", env, io.Discard, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, 0, code)

	require.NotNil(t, pm.created)
	require.Len(t, pm.created.Spec.Containers, 1)

	gotEnv := map[string]string{}
	for _, e := range pm.created.Spec.Containers[0].Env {
		gotEnv[e.Name] = e.Value
	}

	assert.Equal(t, "resolved-value", gotEnv["TOKEN"], "pod env must carry the orchestrator's expanded value")
	assert.NotEqual(t, "{{ .Params.x }}", gotEnv["TOKEN"], "pod env must NOT carry the raw, unexpanded template string")
	assert.Equal(t, "linux", gotEnv["UNIFIED_AGENT_OS"], "UNIFIED_AGENT_OS must still be present exactly once")
}

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
