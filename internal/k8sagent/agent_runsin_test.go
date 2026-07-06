package k8sagent

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
)

func TestExecContainer_FromRunsIn(t *testing.T) {
	// RunsIn.Container becomes the exec target container name (the sole source of truth after normalization)
	assert.Equal(t, "tools", execContainer(api.ClaimStep{RunsIn: &dsl.RunsIn{Container: "tools"}}))
	// No RunsIn specified means the default container (empty string)
	assert.Equal(t, "", execContainer(api.ClaimStep{}))
	// RunsIn.Image only (not a named container) is also empty = default
	assert.Equal(t, "", execContainer(api.ClaimStep{RunsIn: &dsl.RunsIn{Image: "golang:1.22"}}))
}

func TestImageStepEnv(t *testing.T) {
	original := map[string]string{"FOO": "bar"}
	step := api.ClaimStep{Env: original}

	out := imageStepEnv(step)

	assert.Equal(t, "bar", out["FOO"])
	assert.Equal(t, "linux", out["UNIFIED_AGENT_OS"], "runsIn.image pod is a linux container")
	// must not mutate the input map (the claim's step.Env)
	assert.Equal(t, map[string]string{"FOO": "bar"}, original)
	_, hasKey := original["UNIFIED_AGENT_OS"]
	assert.False(t, hasKey, "imageStepEnv must not inject UNIFIED_AGENT_OS into the caller's map")
}

func TestImageStepEnv_NilEnv(t *testing.T) {
	out := imageStepEnv(api.ClaimStep{})
	assert.Equal(t, "linux", out["UNIFIED_AGENT_OS"], "runsIn.image pod is a linux container")
	assert.Len(t, out, 1)
}

func TestExecStepEnv(t *testing.T) {
	out := execStepEnv(api.ClaimStep{Env: map[string]string{"FOO": "bar"}})
	assert.Contains(t, out, "UNIFIED_AGENT_OS=linux", "pod-exec steps always run on linux")
	assert.Contains(t, out, "FOO=bar")
}

func TestExecStepEnv_NilEnv(t *testing.T) {
	out := execStepEnv(api.ClaimStep{})
	assert.Equal(t, []string{"UNIFIED_AGENT_OS=linux"}, out)
}

func TestImageStepDeadline(t *testing.T) {
	// zero TimeoutMinutes -> default 1h
	assert.Equal(t, int64(3600), imageStepDeadline(api.ClaimStep{}))
	// set TimeoutMinutes -> TimeoutMinutes*60
	assert.Equal(t, int64(120), imageStepDeadline(api.ClaimStep{TimeoutMinutes: 2.0}))
}
