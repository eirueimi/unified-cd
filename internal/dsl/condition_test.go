package dsl

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvalCondition_Empty(t *testing.T) {
	ok, _, err := EvalCondition("", TemplateData{}, RunStatusView{}, true)
	require.NoError(t, err)
	assert.True(t, ok, "empty expr with no failure should return true (implicit success)")
}

func TestEvalCondition_LiteralTrue(t *testing.T) {
	ok, _, err := EvalCondition("true", TemplateData{}, RunStatusView{}, true)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestEvalCondition_LiteralFalse(t *testing.T) {
	ok, _, err := EvalCondition("false", TemplateData{}, RunStatusView{}, true)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestEvalCondition_ParamsTrue(t *testing.T) {
	data := TemplateData{Params: map[string]string{"env": "production"}}
	ok, _, err := EvalCondition(`params.env == "production"`, data, RunStatusView{}, true)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestEvalCondition_ParamsFalse(t *testing.T) {
	data := TemplateData{Params: map[string]string{"env": "staging"}}
	ok, _, err := EvalCondition(`params.env == "production"`, data, RunStatusView{}, true)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestEvalCondition_LogicalAnd(t *testing.T) {
	data := TemplateData{Params: map[string]string{"env": "production", "region": "us-east-1"}}
	ok, _, err := EvalCondition(`params.env == "production" && params.region == "us-east-1"`, data, RunStatusView{}, true)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestEvalCondition_InOperator(t *testing.T) {
	data := TemplateData{Params: map[string]string{"env": "staging"}}
	ok, _, err := EvalCondition(`params.env in ["production", "staging"]`, data, RunStatusView{}, true)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestEvalCondition_InvalidExpr(t *testing.T) {
	ok, _, err := EvalCondition("params.env ==", TemplateData{}, RunStatusView{}, true)
	assert.Error(t, err)
	assert.True(t, ok, "on compile error should return true (safe default: run the step)")
}

func TestEvalCondition_NonBoolResult(t *testing.T) {
	data := TemplateData{Params: map[string]string{"env": "production"}}
	ok, _, err := EvalCondition("params.env", data, RunStatusView{}, true)
	assert.Error(t, err)
	assert.True(t, ok, "on type error should return true (safe default)")
}

func TestEvalCondition_StepOutputs(t *testing.T) {
	data := TemplateData{
		Steps: map[string]StepData{
			"build": {Outputs: map[string]any{"ok": "true"}},
		},
	}
	ok, _, err := EvalCondition(`steps.build.outputs.ok == "true"`, data, RunStatusView{}, true)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestEvalCondition_StatusFunctions(t *testing.T) {
	cases := []struct {
		name         string
		expr         string
		status       RunStatusView
		implicitSucc bool
		want         bool
	}{
		// failure()
		{"failure_when_failed", "failure()", RunStatusView{Failed: true}, true, true},
		{"failure_when_ok", "failure()", RunStatusView{}, true, false},
		{"failure_when_cancelled", "failure()", RunStatusView{Cancelled: true}, true, false},
		// success()
		{"success_when_ok", "success()", RunStatusView{}, true, true},
		{"success_when_failed", "success()", RunStatusView{Failed: true}, true, false},
		{"success_when_cancelled", "success()", RunStatusView{Cancelled: true}, true, false},
		// always()
		{"always_when_failed", "always()", RunStatusView{Failed: true}, true, true},
		{"always_when_cancelled", "always()", RunStatusView{Cancelled: true}, true, true},
		// implicit success(): no-if step after a failure is skipped
		{"empty_after_failure_implicit", "", RunStatusView{Failed: true}, true, false},
		{"empty_ok_implicit", "", RunStatusView{}, true, true},
		// implicit success(): a non-status expr is ANDed with success()
		{"nonstatus_after_failure_implicit", "true", RunStatusView{Failed: true}, true, false},
		{"nonstatus_ok_implicit", "true", RunStatusView{}, true, true},
		// finally semantics: implicitSuccess=false → empty is always-run
		{"empty_finally_after_failure", "", RunStatusView{Failed: true}, false, true},
		{"nonstatus_finally_after_failure", "true", RunStatusView{Failed: true}, false, true},
		{"failure_in_finally", "failure()", RunStatusView{Failed: true}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := EvalCondition(tc.expr, TemplateData{}, tc.status, tc.implicitSucc)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// --- vars binding -----------------------------------------------------------

// A vars-gated condition must actually GATE. Before vars was bound, the
// expression failed to compile, EvalCondition returned (true, err) — fail-safe
// — and the step ran regardless of what the variable said.
func TestEvalCondition_VarsGates(t *testing.T) {
	cases := []struct {
		name string
		vars map[string]string
		want bool
	}{
		{"matches", map[string]string{"ENV": "prod"}, true},
		{"does_not_match", map[string]string{"ENV": "staging"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, warns, err := EvalCondition(`vars.ENV == "prod"`, TemplateData{Vars: tc.vars}, RunStatusView{}, true)
			require.NoError(t, err)
			assert.Empty(t, warns)
			assert.Equal(t, tc.want, ok)
		})
	}
}

func TestEvalCondition_VarsIndexForm(t *testing.T) {
	data := TemplateData{Vars: map[string]string{"my-key": "yes"}}
	ok, _, err := EvalCondition(`vars["my-key"] == "yes"`, data, RunStatusView{}, true)
	require.NoError(t, err)
	assert.True(t, ok)
}

// An UNDEFINED vars key reads as the empty string rather than erroring, so the
// gate stays SHUT instead of failing open, and the caller is told which key it
// was.
func TestEvalCondition_VarsUndefinedKeyIsEmptyAndWarns(t *testing.T) {
	data := TemplateData{Vars: map[string]string{"ENV": "prod"}}
	ok, warns, err := EvalCondition(`vars.TYPO == "prod"`, data, RunStatusView{}, true)
	require.NoError(t, err, "an undefined vars key must not error — an error is fail-open")
	assert.False(t, ok, "an undefined key must not run the step")
	require.Len(t, warns, 1)
	assert.Contains(t, warns[0], "vars.TYPO")
	assert.Contains(t, warns[0], "undefined")
}

func TestEvalCondition_VarsUndefinedKeyComparesEqualToEmpty(t *testing.T) {
	ok, warns, err := EvalCondition(`vars.NOPE == ""`, TemplateData{}, RunStatusView{}, false)
	require.NoError(t, err)
	assert.True(t, ok, "an undefined key reads as the empty string")
	require.Len(t, warns, 1)
	assert.Contains(t, warns[0], "vars.NOPE")
}

func TestEvalCondition_VarsUndefinedKeyIndexFormAlsoDefaults(t *testing.T) {
	ok, warns, err := EvalCondition(`vars["NOPE"] == "x"`, TemplateData{}, RunStatusView{}, false)
	require.NoError(t, err)
	assert.False(t, ok)
	require.Len(t, warns, 1)
	assert.Contains(t, warns[0], "vars.NOPE")
}

// Defaulting the VALUE must not cost the ability to test PRESENCE: `in` goes
// through Contains, not Find, so it still answers truthfully. (`has(vars.X)`
// does not — CEL routes it through Find — which is why `in` is the documented
// spelling.)
func TestEvalCondition_VarsPresenceTestStaysTruthful(t *testing.T) {
	data := TemplateData{Vars: map[string]string{"ENV": "prod"}}

	ok, warns, err := EvalCondition(`"ENV" in vars`, data, RunStatusView{}, false)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Empty(t, warns)

	ok, warns, err = EvalCondition(`"NOPE" in vars`, data, RunStatusView{}, false)
	require.NoError(t, err)
	assert.False(t, ok, "in: must report an undefined key as absent, not as empty-string-present")
	assert.Empty(t, warns, "a presence test is not an undefined-key read")
}

// The other half of the presence-testing trade-off, which was documented in
// three places and asserted in none: has(vars.X) is ALWAYS TRUE.
//
// cel-go routes both a value read and a presence test through Mapper.Find, and
// defaultingMap.Find never reports "not found" for a string key — that is the
// whole mechanism that keeps a mistyped gate shut. So has() cannot distinguish
// a defined key from an undefined one here, and an author who reaches for the
// spelling CEL documentation teaches gets a condition that is true no matter
// what. Pinning it means the docs telling people to write `"X" in vars`
// instead cannot quietly stop being true: if a future change to defaultingMap
// makes has() honest, this test fails and the docs get revisited, rather than
// the guidance rotting into superstition.
func TestEvalCondition_HasVarsIsAlwaysTrue(t *testing.T) {
	data := TemplateData{Vars: map[string]string{"ENV": "prod"}}

	ok, _, err := EvalCondition(`has(vars.ENV)`, data, RunStatusView{}, false)
	require.NoError(t, err)
	assert.True(t, ok, "has() on a defined key is true, as anyone would expect")

	ok, _, err = EvalCondition(`has(vars.NOPE)`, data, RunStatusView{}, false)
	require.NoError(t, err)
	assert.True(t, ok,
		"has(vars.NOPE) is TRUE for an undefined key: cel-go routes has() through the same "+
			"Find that defaults an undefined key to the empty string. This is why the documented "+
			"presence test is `\"NOPE\" in vars` — see TestEvalCondition_VarsPresenceTestStaysTruthful.")

	// The contrast, in one place, so the reason for the documented spelling is
	// visible without cross-referencing: `in` disagrees with has() on exactly
	// the key that matters.
	inOK, _, err := EvalCondition(`"NOPE" in vars`, data, RunStatusView{}, false)
	require.NoError(t, err)
	assert.False(t, inOK, "`in` is the spelling that answers truthfully")
}

// params has the SAME shape vars had: an undefined key raises "no such key",
// which is an eval error, which is fail-safe — the step RUNS. This is a KNOWN
// ASYMMETRY with vars (which reads an undefined key as empty, keeping the gate
// shut) and it is left in place deliberately. This comment is the only record
// of that decision in code, so it carries its own reasoning:
//
// Two fixes were considered and both rejected.
//
//  1. Make params read as empty, like vars. That changes the meaning of every
//     params-gated condition already in service: a step that runs today would
//     start skipping, silently, on the next agent upgrade.
//
//  2. Reject an if: that names an undeclared param at apply time. This looked
//     safe — it changes no run-time behaviour at all — but it rests on the
//     declared set being authoritative, and it is not. See resolveParams in
//     internal/controller/params.go, whose own doc comment says "Params not
//     declared as inputs are passed through unchanged"; all five of its caller
//     paths can introduce one (CLI --param, run re-trigger, webhook
//     paramsMapping, schedule params, and a call: step's with:), and
//     spec.concurrency.orLocks synthesizes {NAME}_LOCK_VALUE keys on top of
//     that. `unified-cd run trigger job --param DEPLOY_TARGET=x` against
//     `if: params.DEPLOY_TARGET == "x"` is documented, supported, and works —
//     so a typo and a legitimate pass-through reference are statically
//     indistinguishable, and rejecting one rejects the other.
//
// What IS caught at apply time is a malformed expression (see
// ValidateConditionExpr) — that check only rejects what would fail to compile
// at run time anyway.
func TestEvalCondition_ParamsUndefinedKeyStillFailsOpen(t *testing.T) {
	data := TemplateData{Params: map[string]string{"env": "staging"}}
	ok, _, err := EvalCondition(`params.TYPO == "prod"`, data, RunStatusView{}, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no such key")
	assert.True(t, ok, "params keeps CEL's no-such-key error, which is fail-open")
}

// --- declaration/activation drift guard -------------------------------------

// Every variable declared in conditionVars must also be BOUND at eval time,
// with a value that fits its declared type. Both halves are derived from the
// one table, so this cannot fail by omission — it fails when a new entry's
// value function returns something CEL cannot adapt to the declared type,
// which is the only way the pair can still disagree.
func TestConditionVars_EveryDeclaredVarIsBound(t *testing.T) {
	require.NotEmpty(t, conditionVars)
	for _, v := range conditionVars {
		t.Run(v.name, func(t *testing.T) {
			// size() forces the binding to be resolved and type-checked
			// without depending on any particular key being present.
			expr := "size(" + v.name + ") >= 0"
			ok, _, err := EvalCondition(expr, TemplateData{}, RunStatusView{}, false)
			require.NoError(t, err, "variable %q is declared but not usable at eval time", v.name)
			assert.True(t, ok)
		})
	}
}

func TestConditionActivation_CoversEveryDeclaredVar(t *testing.T) {
	act := conditionActivation(TemplateData{}, &undefinedKeys{})
	assert.Len(t, act, len(conditionVars))
	for _, v := range conditionVars {
		assert.Contains(t, act, v.name)
	}
}

// --- apply-time validation --------------------------------------------------

func TestValidateConditionExpr(t *testing.T) {
	valid := []string{
		"",
		"always()",
		"failure()",
		`params.env == "production"`,
		`vars.ENV == "prod"`,
		`"ENV" in vars`,
		`secrets.TOKEN != ""`,
		// steps is map(string, dyn): a step reference is only resolvable at
		// run time and must still compile at apply time.
		`steps.build.outputs.result == "ok"`,
		`params.env in ["production", "staging"]`,
	}
	for _, expr := range valid {
		assert.NoError(t, ValidateConditionExpr(expr), "expr %q must be accepted", expr)
	}

	invalid := []string{
		"params.env ==",                     // syntax error
		`nope.env == "x"`,                   // unknown identifier
		`{{ eq .Params.env "production" }}`, // a Go template, not CEL — the documented trap
		`params.env == 1`,                   // type mismatch
	}
	for _, expr := range invalid {
		assert.Error(t, ValidateConditionExpr(expr), "expr %q must be rejected", expr)
	}
}

// A dyn-typed expression must NOT be rejected at apply time even though
// EvalCondition's bool check rejects it at run time: apply-time validation
// stops at "does it compile" precisely so it can never reject something whose
// shape is only knowable at run time.
func TestValidateConditionExpr_DoesNotApplyBoolCheck(t *testing.T) {
	require.NoError(t, ValidateConditionExpr("steps.build.outputs.ok"))
}

func TestJobValidate_RejectsMalformedIf(t *testing.T) {
	j := &Job{
		APIVersion: SupportedAPIVersion,
		Kind:       "Job",
		Metadata:   Metadata{Name: "j"},
		Spec: Spec{
			Steps: []StepEntry{{Name: "s", Run: "echo hi", If: `{{ eq .Params.env "prod" }}`}},
		},
	}
	err := j.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not compile")
}

func TestJobValidate_AcceptsVarsGatedIf(t *testing.T) {
	j := &Job{
		APIVersion: SupportedAPIVersion,
		Kind:       "Job",
		Metadata:   Metadata{Name: "j"},
		Spec: Spec{
			Steps: []StepEntry{{Name: "s", Run: "echo hi", If: `vars.ENV == "prod"`}},
		},
	}
	require.NoError(t, j.Validate())
}
