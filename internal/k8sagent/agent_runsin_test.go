package k8sagent

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
)

func TestExecContainer_FromContainer(t *testing.T) {
	// Container is the exec target container name (the sole source of truth after normalization)
	assert.Equal(t, "tools", execContainer(api.ClaimStep{Container: "tools"}))
	// No Container specified means the default container (empty string)
	assert.Equal(t, "", execContainer(api.ClaimStep{}))
}

func TestImageStepEnv(t *testing.T) {
	original := map[string]string{"FOO": "bar"}
	step := api.ClaimStep{Env: original}

	out := imageStepEnv(step)

	assert.Equal(t, "bar", out["FOO"])
	assert.Equal(t, "linux", out["UNIFIED_AGENT_OS"], "scope pod is a linux container")
	assert.Equal(t, "/workspace", out["UNIFIED_WORKSPACE"], "scope pod's fixed working directory")
	// must not mutate the input map (the claim's step.Env)
	assert.Equal(t, map[string]string{"FOO": "bar"}, original)
	_, hasKey := original["UNIFIED_AGENT_OS"]
	assert.False(t, hasKey, "imageStepEnv must not inject UNIFIED_AGENT_OS into the caller's map")
	_, hasWorkspaceKey := original["UNIFIED_WORKSPACE"]
	assert.False(t, hasWorkspaceKey, "imageStepEnv must not inject UNIFIED_WORKSPACE into the caller's map")
}

func TestImageStepEnv_NilEnv(t *testing.T) {
	out := imageStepEnv(api.ClaimStep{})
	assert.Equal(t, "linux", out["UNIFIED_AGENT_OS"], "scope pod is a linux container")
	assert.Equal(t, "/workspace", out["UNIFIED_WORKSPACE"], "scope pod's fixed working directory")
	assert.Len(t, out, 2)
}
