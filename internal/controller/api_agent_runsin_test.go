package controller

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildOneClaimStep_CopiesRunsIn(t *testing.T) {
	entry := dsl.StepEntry{
		Name:   "build",
		Run:    "go build",
		RunsIn: &dsl.RunsIn{Image: "golang:1.22"},
	}
	cs := buildOneClaimStep(0, 0, entry)
	require.NotNil(t, cs.RunsIn)
	assert.Equal(t, "golang:1.22", cs.RunsIn.Image)
}

func TestBuildOneClaimStep_CopiesRunsInContainer(t *testing.T) {
	entry := dsl.StepEntry{
		Name:   "deploy",
		Run:    "kubectl apply",
		RunsIn: &dsl.RunsIn{Container: "tools"},
	}
	cs := buildOneClaimStep(0, 0, entry)
	require.NotNil(t, cs.RunsIn)
	assert.Equal(t, "tools", cs.RunsIn.Container)
}
