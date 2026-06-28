package dsl

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvalCondition_Empty(t *testing.T) {
	ok, err := EvalCondition("", TemplateData{})
	require.NoError(t, err)
	assert.True(t, ok, "empty expr should return true")
}

func TestEvalCondition_LiteralTrue(t *testing.T) {
	ok, err := EvalCondition("true", TemplateData{})
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestEvalCondition_LiteralFalse(t *testing.T) {
	ok, err := EvalCondition("false", TemplateData{})
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestEvalCondition_ParamsTrue(t *testing.T) {
	data := TemplateData{Params: map[string]string{"env": "production"}}
	ok, err := EvalCondition(`params.env == "production"`, data)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestEvalCondition_ParamsFalse(t *testing.T) {
	data := TemplateData{Params: map[string]string{"env": "staging"}}
	ok, err := EvalCondition(`params.env == "production"`, data)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestEvalCondition_LogicalAnd(t *testing.T) {
	data := TemplateData{Params: map[string]string{"env": "production", "region": "us-east-1"}}
	ok, err := EvalCondition(`params.env == "production" && params.region == "us-east-1"`, data)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestEvalCondition_InOperator(t *testing.T) {
	data := TemplateData{Params: map[string]string{"env": "staging"}}
	ok, err := EvalCondition(`params.env in ["production", "staging"]`, data)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestEvalCondition_InvalidExpr(t *testing.T) {
	ok, err := EvalCondition("params.env ==", TemplateData{})
	assert.Error(t, err)
	assert.True(t, ok, "on compile error should return true (safe default: run the step)")
}

func TestEvalCondition_NonBoolResult(t *testing.T) {
	data := TemplateData{Params: map[string]string{"env": "production"}}
	ok, err := EvalCondition("params.env", data)
	assert.Error(t, err)
	assert.True(t, ok, "on type error should return true (safe default)")
}

func TestEvalCondition_StepOutputs(t *testing.T) {
	data := TemplateData{
		Steps: map[string]StepData{
			"build": {Outputs: map[string]string{"ok": "true"}},
		},
	}
	ok, err := EvalCondition(`steps.build.outputs.ok == "true"`, data)
	require.NoError(t, err)
	assert.True(t, ok)
}
