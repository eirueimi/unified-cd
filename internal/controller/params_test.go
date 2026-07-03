package controller

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveParams_MissingRequired_Errors(t *testing.T) {
	inputs := []dsl.Input{
		{Name: "image", Type: "string", Required: true},
	}
	_, err := resolveParams(inputs, map[string]string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "image")
}

func TestResolveParams_MissingRequired_MultipleNamesAllListed(t *testing.T) {
	inputs := []dsl.Input{
		{Name: "image", Type: "string", Required: true},
		{Name: "tag", Type: "string", Required: true},
	}
	_, err := resolveParams(inputs, map[string]string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "image")
	assert.Contains(t, err.Error(), "tag")
}

func TestResolveParams_OmittedWithDefault_Filled(t *testing.T) {
	inputs := []dsl.Input{
		{Name: "tag", Type: "string", Default: "latest"},
	}
	got, err := resolveParams(inputs, map[string]string{})
	require.NoError(t, err)
	assert.Equal(t, "latest", got["tag"])
}

func TestResolveParams_ProvidedValue_Unchanged(t *testing.T) {
	inputs := []dsl.Input{
		{Name: "tag", Type: "string", Default: "latest"},
	}
	got, err := resolveParams(inputs, map[string]string{"tag": "v2"})
	require.NoError(t, err)
	assert.Equal(t, "v2", got["tag"])
}

func TestResolveParams_RequiredWithDefault_NoErrorWhenOmitted(t *testing.T) {
	// A default satisfies the required constraint even when the caller omits it.
	inputs := []dsl.Input{
		{Name: "env", Type: "string", Required: true, Default: "staging"},
	}
	got, err := resolveParams(inputs, map[string]string{})
	require.NoError(t, err)
	assert.Equal(t, "staging", got["env"])
}

func TestResolveParams_NonBoolDefault_FormattedAsString(t *testing.T) {
	inputs := []dsl.Input{
		{Name: "run_tests", Type: "bool", Default: true},
	}
	got, err := resolveParams(inputs, map[string]string{})
	require.NoError(t, err)
	assert.Equal(t, "true", got["run_tests"])
}

func TestResolveParams_UndeclaredParams_PassThrough(t *testing.T) {
	got, err := resolveParams(nil, map[string]string{"extra": "value"})
	require.NoError(t, err)
	assert.Equal(t, "value", got["extra"])
}

func TestResolveParams_NoInputs_ReturnsSuppliedUnchanged(t *testing.T) {
	got, err := resolveParams(nil, map[string]string{"k": "v"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"k": "v"}, got)
}

func TestResolveParams_DoesNotMutateSuppliedMap(t *testing.T) {
	inputs := []dsl.Input{
		{Name: "tag", Type: "string", Default: "latest"},
	}
	supplied := map[string]string{}
	_, err := resolveParams(inputs, supplied)
	require.NoError(t, err)
	assert.Empty(t, supplied, "resolveParams must not mutate the caller's map")
}
