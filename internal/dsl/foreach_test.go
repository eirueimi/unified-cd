package dsl_test

import (
	"testing"

	"github.com/unified-cd/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvalForeachSource_Literal(t *testing.T) {
	src := dsl.ForeachSource{Literal: []string{"prod", "staging"}}
	items, err := dsl.EvalForeachSource(src, dsl.TemplateData{})
	require.NoError(t, err)
	assert.Equal(t, []string{"prod", "staging"}, items)
}

func TestEvalForeachSource_ParamJSONArray(t *testing.T) {
	src := dsl.ForeachSource{Expr: "$envs"}
	data := dsl.TemplateData{Params: map[string]string{"envs": `["prod","staging","dev"]`}}
	items, err := dsl.EvalForeachSource(src, data)
	require.NoError(t, err)
	assert.Equal(t, []string{"prod", "staging", "dev"}, items)
}

func TestEvalForeachSource_ParamCommaSplit(t *testing.T) {
	src := dsl.ForeachSource{Expr: "$envs"}
	data := dsl.TemplateData{Params: map[string]string{"envs": "prod,staging,dev"}}
	items, err := dsl.EvalForeachSource(src, data)
	require.NoError(t, err)
	assert.Equal(t, []string{"prod", "staging", "dev"}, items)
}

func TestEvalForeachSource_ParamNotFound(t *testing.T) {
	src := dsl.ForeachSource{Expr: "$missing"}
	_, err := dsl.EvalForeachSource(src, dsl.TemplateData{Params: map[string]string{}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"missing"`)
}

func TestEvalForeachSource_TemplateExpr(t *testing.T) {
	src := dsl.ForeachSource{Expr: `{{ .Params.envs | split "," }}`}
	data := dsl.TemplateData{Params: map[string]string{"envs": "prod,staging"}}
	items, err := dsl.EvalForeachSource(src, data)
	require.NoError(t, err)
	assert.Equal(t, []string{"prod", "staging"}, items)
}

func TestEvalForeachSource_TemplateError(t *testing.T) {
	src := dsl.ForeachSource{Expr: `{{ .Params.envs | split`}
	_, err := dsl.EvalForeachSource(src, dsl.TemplateData{Params: map[string]string{"envs": "prod"}})
	require.Error(t, err)
}
