package agent

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
)

func TestApplyStepOutputs_Bare(t *testing.T) {
	steps := map[string]dsl.StepData{}
	ApplyStepOutputs(steps, "build", "", map[string]string{"bin": "app"})
	assert.Equal(t, "app", steps["build"].Outputs["bin"])
}

func TestApplyStepOutputs_MatrixAggregatesByCombinationKey(t *testing.T) {
	steps := map[string]dsl.StepData{}
	ApplyStepOutputs(steps, "build", "linux/amd64", map[string]string{"bin": "a"})
	ApplyStepOutputs(steps, "build", "linux/arm64", map[string]string{"bin": "b"})
	// Matches pipeline.go setStepMatrixOutputs' current behavior exactly:
	// Outputs["bin"] is a map[string]string keyed by combination-key.
	got := steps["build"].Outputs
	assert.Contains(t, got, "bin")
	assert.Equal(t, map[string]string{
		"linux/amd64": "a",
		"linux/arm64": "b",
	}, got["bin"])
}

func TestApplyStepOutputs_MatrixDoesNotMutatePriorSnapshot(t *testing.T) {
	steps := map[string]dsl.StepData{}
	ApplyStepOutputs(steps, "build", "v1", map[string]string{"k": "1"})
	before := steps["build"]
	ApplyStepOutputs(steps, "build", "v2", map[string]string{"k": "2"})
	// Copy-on-write: a previously captured StepData/Outputs snapshot is untouched.
	assert.Equal(t, map[string]string{"v1": "1"}, before.Outputs["k"])
	assert.Equal(t, map[string]string{"v1": "1", "v2": "2"}, steps["build"].Outputs["k"])
}
