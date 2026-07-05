package dsl

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvalCondition_Empty(t *testing.T) {
	ok, err := EvalCondition("", TemplateData{}, RunStatusView{}, true)
	require.NoError(t, err)
	assert.True(t, ok, "empty expr with no failure should return true (implicit success)")
}

func TestEvalCondition_LiteralTrue(t *testing.T) {
	ok, err := EvalCondition("true", TemplateData{}, RunStatusView{}, true)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestEvalCondition_LiteralFalse(t *testing.T) {
	ok, err := EvalCondition("false", TemplateData{}, RunStatusView{}, true)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestEvalCondition_ParamsTrue(t *testing.T) {
	data := TemplateData{Params: map[string]string{"env": "production"}}
	ok, err := EvalCondition(`params.env == "production"`, data, RunStatusView{}, true)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestEvalCondition_ParamsFalse(t *testing.T) {
	data := TemplateData{Params: map[string]string{"env": "staging"}}
	ok, err := EvalCondition(`params.env == "production"`, data, RunStatusView{}, true)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestEvalCondition_LogicalAnd(t *testing.T) {
	data := TemplateData{Params: map[string]string{"env": "production", "region": "us-east-1"}}
	ok, err := EvalCondition(`params.env == "production" && params.region == "us-east-1"`, data, RunStatusView{}, true)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestEvalCondition_InOperator(t *testing.T) {
	data := TemplateData{Params: map[string]string{"env": "staging"}}
	ok, err := EvalCondition(`params.env in ["production", "staging"]`, data, RunStatusView{}, true)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestEvalCondition_InvalidExpr(t *testing.T) {
	ok, err := EvalCondition("params.env ==", TemplateData{}, RunStatusView{}, true)
	assert.Error(t, err)
	assert.True(t, ok, "on compile error should return true (safe default: run the step)")
}

func TestEvalCondition_NonBoolResult(t *testing.T) {
	data := TemplateData{Params: map[string]string{"env": "production"}}
	ok, err := EvalCondition("params.env", data, RunStatusView{}, true)
	assert.Error(t, err)
	assert.True(t, ok, "on type error should return true (safe default)")
}

func TestEvalCondition_StepOutputs(t *testing.T) {
	data := TemplateData{
		Steps: map[string]StepData{
			"build": {Outputs: map[string]any{"ok": "true"}},
		},
	}
	ok, err := EvalCondition(`steps.build.outputs.ok == "true"`, data, RunStatusView{}, true)
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
			got, err := EvalCondition(tc.expr, TemplateData{}, tc.status, tc.implicitSucc)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
