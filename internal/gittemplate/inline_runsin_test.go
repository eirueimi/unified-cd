package gittemplate

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExpandUses_PropagatesContainer(t *testing.T) {
	tplSpec := dsl.Spec{
		Steps: []dsl.StepEntry{
			{Name: "compile", Run: "go build"},
			{Name: "special", Run: "echo hi", Container: "alpine"},
		},
	}

	expanded, _, err := expandUsesStep("tmpl", nil, tplSpec, nil, "builder", "")
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

	// inherits the uses step's flat container: since it has none of its own
	assert.Equal(t, "builder", compile.Container)

	// keeps its own container: (no override)
	assert.Equal(t, "alpine", special.Container)

	// inline expansion never emits step-level RunsIn
	assert.Nil(t, compile.RunsIn)
	assert.Nil(t, special.RunsIn)
}

func TestInline_TemplateStepRunsInRejected(t *testing.T) {
	// a template whose inner step declares runsIn: {image: ...} must fail
	// resolve with a migration hint (step-level runsIn was removed).
	templateWithInnerRunsIn := dsl.Spec{
		Steps: []dsl.StepEntry{
			{Name: "lint", Run: "golangci-lint run", RunsIn: &dsl.RunsIn{Image: "golangci/lint:latest"}},
		},
	}
	_, _, err := expandUsesStep("build", nil, templateWithInnerRunsIn, nil, "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runsIn")
	assert.Contains(t, err.Error(), "container:")
}

func TestInline_UsesContainerPropagatesToInnerSteps(t *testing.T) {
	// uses entry with Container: "tools" and a template of two plain steps ->
	// both inlined steps get Container "tools"; an inner step that already
	// sets container: keeps its own.
	tplSpec := dsl.Spec{
		Steps: []dsl.StepEntry{
			{Name: "one", Run: "echo one"},
			{Name: "two", Run: "echo two", Container: "custom"},
		},
	}
	expanded, _, err := expandUsesStep("job", nil, tplSpec, nil, "tools", "")
	require.NoError(t, err)

	var one, two dsl.StepEntry
	for _, es := range expanded {
		switch es.Name {
		case "job__one":
			one = es
		case "job__two":
			two = es
		}
	}
	assert.Equal(t, "tools", one.Container)
	assert.Equal(t, "custom", two.Container)
}
