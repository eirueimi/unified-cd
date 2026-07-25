package controller

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareRunSpecPreservesStoredJSONShape(t *testing.T) {
	const unresolved = `{{ index .Secrets .Params.token_secret }}`
	specJSON := []byte(`{
		"agentSelector":["kind:linux"],
		"steps":[
			{"name":"main","run":"` + unresolved + `","env":{"TOKEN":"` + unresolved + `"}},
			{"parallel":[{"name":"parallel","run":"` + unresolved + `","env":{"TOKEN":"` + unresolved + `"}}]}
		],
		"finally":[{"name":"cleanup","run":"` + unresolved + `","env":{"TOKEN":"` + unresolved + `"}}],
		"x-extension":{"preserve":true}
	}`)
	before := append([]byte(nil), specJSON...)

	got, err := prepareRunSpec(specJSON, map[string]string{"token_secret": "deploy-token"})

	require.NoError(t, err)
	assert.Equal(t, before, specJSON, "the caller-owned JSON bytes must not be mutated")
	var actual any
	require.NoError(t, json.Unmarshal(got, &actual))
	var expected any
	require.NoError(t, json.Unmarshal([]byte(`{
		"agentSelector":["kind:linux"],
		"steps":[
			{"name":"main","run":"{{ index .Secrets \"deploy-token\" }}","env":{"TOKEN":"{{ index .Secrets \"deploy-token\" }}"}},
			{"parallel":[{"name":"parallel","run":"{{ index .Secrets \"deploy-token\" }}","env":{"TOKEN":"{{ index .Secrets \"deploy-token\" }}"}}]}
		],
		"finally":[{"name":"cleanup","run":"{{ index .Secrets \"deploy-token\" }}","env":{"TOKEN":"{{ index .Secrets \"deploy-token\" }}"}}],
		"x-extension":{"preserve":true}
	}`), &expected))
	assert.Equal(t, expected, actual)
}

func TestPrepareRunSpecResolvesEmptyOptionalSecretNameParameter(t *testing.T) {
	specJSON := []byte(`{
		"steps":[{"name":"deploy","run":"true","env":{"TOKEN":"{{ if .Params.token_secret }}{{ index .Secrets .Params.token_secret }}{{ end }}"}}]
	}`)

	raw, err := prepareRunSpec(specJSON, map[string]string{"token_secret": ""})

	require.NoError(t, err)
	assert.Contains(t, string(raw), `index .Secrets \"\"`)
	assert.Contains(t, string(raw), `if .Params.token_secret`)
}

func TestPrepareRunSpecRejectsRuntimeOnlySecretName(t *testing.T) {
	specJSON := []byte(`{
		"steps":[{"name":"deploy","run":"true","env":{"TOKEN":"{{ index .Secrets .Steps.discover.Outputs.token_secret }}"}}]
	}`)

	_, err := prepareRunSpec(specJSON, nil)

	require.Error(t, err)
	assert.ErrorContains(t, err, "dynamic secret name must be resolved from a parameter before execution")
}

func TestPrepareRunSpecPreservesExistingGoStyleKeySpelling(t *testing.T) {
	specJSON := []byte(`{
		"Steps":[{"Name":"deploy","Run":"{{ index .Secrets .Params.token_secret }}",
		"Env":{"TOKEN":"{{ index .Secrets .Params.token_secret }}"}}],
		"Finally":null
	}`)

	got, err := prepareRunSpec(specJSON, map[string]string{"token_secret": "deploy-token"})

	require.NoError(t, err)
	var snapshot map[string]any
	require.NoError(t, json.Unmarshal(got, &snapshot))
	assert.Contains(t, snapshot, "Steps")
	assert.NotContains(t, snapshot, "steps")
	assert.Contains(t, snapshot, "Finally")
}

func TestPrepareRunSpecRejectsWrongStepsContainerType(t *testing.T) {
	_, err := prepareRunSpec([]byte(`{"steps":"not-an-array"}`), nil)

	require.Error(t, err)
	assert.ErrorContains(t, err, `field "steps" must be an array`)
}

func TestPrepareRunSpecRejectsNullRoot(t *testing.T) {
	_, err := prepareRunSpec([]byte(`null`), nil)

	require.Error(t, err)
	assert.ErrorContains(t, err, "stored run spec must be an object")
}
