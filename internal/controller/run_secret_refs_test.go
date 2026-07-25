package controller

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareRunSpecResolvesSecretNameParameter(t *testing.T) {
	spec := dsl.Spec{Steps: []dsl.StepEntry{{
		Name: "deploy",
		Env: map[string]string{
			"TOKEN": `{{ index .Secrets .Params.token_secret }}`,
		},
		Run: "true",
	}}}

	raw, err := prepareRunSpec(spec, map[string]string{"token_secret": "deploy-token"})

	require.NoError(t, err)
	assert.Contains(t, string(raw), `index .Secrets \"deploy-token\"`)
	assert.NotContains(t, string(raw), `.Params.token_secret`)
}

func TestPrepareRunSpecResolvesEmptyOptionalSecretNameParameter(t *testing.T) {
	spec := dsl.Spec{Steps: []dsl.StepEntry{{
		Name: "deploy",
		Env: map[string]string{
			"TOKEN": `{{ if .Params.token_secret }}{{ index .Secrets .Params.token_secret }}{{ end }}`,
		},
		Run: "true",
	}}}

	raw, err := prepareRunSpec(spec, map[string]string{"token_secret": ""})

	require.NoError(t, err)
	assert.Contains(t, string(raw), `index .Secrets \"\"`)
	assert.Contains(t, string(raw), `if .Params.token_secret`)
}

func TestPrepareRunSpecRejectsRuntimeOnlySecretName(t *testing.T) {
	spec := dsl.Spec{Steps: []dsl.StepEntry{{
		Name: "deploy",
		Env: map[string]string{
			"TOKEN": `{{ index .Secrets .Steps.discover.Outputs.token_secret }}`,
		},
		Run: "true",
	}}}

	_, err := prepareRunSpec(spec, nil)

	require.Error(t, err)
	assert.ErrorContains(t, err, "dynamic secret name must be resolved from a parameter before execution")
}

func TestPrepareRunSpecDoesNotMutateCallerExecutableFields(t *testing.T) {
	const unresolved = `{{ index .Secrets .Params.token_secret }}`
	spec := dsl.Spec{
		Steps: []dsl.StepEntry{
			{
				Name: "main",
				Run:  unresolved,
				Env:  map[string]string{"TOKEN": unresolved},
			},
			{
				Parallel: []dsl.Step{{
					Name: "parallel",
					Run:  unresolved,
					Env:  map[string]string{"TOKEN": unresolved},
				}},
			},
		},
		Finally: []dsl.StepEntry{{
			Name: "cleanup",
			Run:  unresolved,
			Env:  map[string]string{"TOKEN": unresolved},
		}},
	}

	_, err := prepareRunSpec(spec, map[string]string{"token_secret": "deploy-token"})

	require.NoError(t, err)
	assert.Equal(t, unresolved, spec.Steps[0].Run)
	assert.Equal(t, unresolved, spec.Steps[0].Env["TOKEN"])
	assert.Equal(t, unresolved, spec.Steps[1].Parallel[0].Run)
	assert.Equal(t, unresolved, spec.Steps[1].Parallel[0].Env["TOKEN"])
	assert.Equal(t, unresolved, spec.Finally[0].Run)
	assert.Equal(t, unresolved, spec.Finally[0].Env["TOKEN"])
}
