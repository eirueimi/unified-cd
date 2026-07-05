package k8sagent

import (
	"runtime"
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

func TestExpandStepEnv(t *testing.T) {
	td := dsl.TemplateData{Stdout: "v1"}
	// literal passes through; a template value is expanded
	out := expandStepEnv(map[string]string{
		"LIT": "plain",
		"TPL": "{{ .Stdout }}",
	}, td)
	assert.Equal(t, "plain", out["LIT"])
	assert.Equal(t, "v1", out["TPL"])
	// nil in, nil-safe out
	assert.Nil(t, expandStepEnv(nil, td))
}

func TestImageStepEnv(t *testing.T) {
	original := map[string]string{"FOO": "bar"}
	step := api.ClaimStep{Env: original}

	out := imageStepEnv(step)

	assert.Equal(t, "bar", out["FOO"])
	assert.Equal(t, runtime.GOOS, out["UNIFIED_AGENT_OS"])
	// must not mutate the input map (the claim's step.Env)
	assert.Equal(t, map[string]string{"FOO": "bar"}, original)
	_, hasKey := original["UNIFIED_AGENT_OS"]
	assert.False(t, hasKey, "imageStepEnv must not inject UNIFIED_AGENT_OS into the caller's map")
}

func TestImageStepEnv_NilEnv(t *testing.T) {
	out := imageStepEnv(api.ClaimStep{})
	assert.Equal(t, runtime.GOOS, out["UNIFIED_AGENT_OS"])
	assert.Len(t, out, 1)
}

func TestImageStepDeadline(t *testing.T) {
	// zero TimeoutMinutes -> default 1h
	assert.Equal(t, int64(3600), imageStepDeadline(api.ClaimStep{}))
	// set TimeoutMinutes -> TimeoutMinutes*60
	assert.Equal(t, int64(120), imageStepDeadline(api.ClaimStep{TimeoutMinutes: 2.0}))
}
