package gittemplate

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExpandUses_PropagatesRunsIn(t *testing.T) {
	tplSpec := dsl.Spec{
		Steps: []dsl.StepEntry{
			{Name: "compile", Run: "go build"},
			{Name: "special", Run: "echo hi", RunsIn: &dsl.RunsIn{Image: "alpine:3"}},
		},
	}
	outerRunsIn := &dsl.RunsIn{Image: "golang:1.22"}

	expanded, err := expandUsesStep("tmpl", nil, tplSpec, outerRunsIn)
	require.NoError(t, err)

	var compile, special dsl.StepEntry
	var foundCompile, foundSpecial bool
	for _, es := range expanded {
		switch es.Name {
		case "tmpl__compile":
			compile = es
			foundCompile = true
		case "tmpl__special":
			special = es
			foundSpecial = true
		}
	}
	require.True(t, foundCompile, "expected inlined step tmpl__compile")
	require.True(t, foundSpecial, "expected inlined step tmpl__special")

	// inherits the uses step's runsIn since it has none of its own
	require.NotNil(t, compile.RunsIn)
	assert.Equal(t, "golang:1.22", compile.RunsIn.Image)

	// keeps its own runsIn (no override)
	require.NotNil(t, special.RunsIn)
	assert.Equal(t, "alpine:3", special.RunsIn.Image)
}
