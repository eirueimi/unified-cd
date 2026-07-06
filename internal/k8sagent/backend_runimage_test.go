package k8sagent

import (
	"context"
	"io"
	"testing"

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
