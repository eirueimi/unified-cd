package dsl

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func lit(items ...string) ForeachSource { return ForeachSource{Literal: items} }

func TestEvalMatrix_CartesianOrderAndKey(t *testing.T) {
	def := MatrixDef{Dimensions: []MatrixDimension{
		{Name: "os", Source: lit("linux", "windows")},
		{Name: "arch", Source: lit("amd64", "arm64")},
	}}
	combos, err := EvalMatrix(def, TemplateData{}, 0)
	require.NoError(t, err)
	keys := make([]string, len(combos))
	for i, c := range combos {
		keys[i] = c.Key
	}
	// 次元は宣言順、値はリスト順
	require.Equal(t, []string{"linux/amd64", "linux/arm64", "windows/amd64", "windows/arm64"}, keys)
	require.Equal(t, map[string]string{"os": "linux", "arch": "amd64"}, combos[0].Values)
}

func TestEvalMatrix_Exclude(t *testing.T) {
	def := MatrixDef{
		Dimensions: []MatrixDimension{
			{Name: "os", Source: lit("linux", "windows")},
			{Name: "arch", Source: lit("amd64", "arm64")},
		},
		Exclude: []map[string]string{{"os": "windows", "arch": "arm64"}},
	}
	combos, err := EvalMatrix(def, TemplateData{}, 0)
	require.NoError(t, err)
	require.Len(t, combos, 3)
	for _, c := range combos {
		require.NotEqual(t, "windows/arm64", c.Key)
	}
}

func TestEvalMatrix_ExcludePartialMatch(t *testing.T) {
	// 部分指定は一致する全組み合わせを除外(GHA互換)
	def := MatrixDef{
		Dimensions: []MatrixDimension{
			{Name: "os", Source: lit("linux", "windows")},
			{Name: "arch", Source: lit("amd64", "arm64")},
		},
		Exclude: []map[string]string{{"os": "windows"}},
	}
	combos, err := EvalMatrix(def, TemplateData{}, 0)
	require.NoError(t, err)
	require.Len(t, combos, 2)
}

func TestEvalMatrix_EmptyDimensionYieldsZeroCombos(t *testing.T) {
	def := MatrixDef{Dimensions: []MatrixDimension{
		{Name: "os", Source: ForeachSource{Expr: "{{ .Params.none }}"}},
	}}
	combos, err := EvalMatrix(def, TemplateData{Params: map[string]string{}}, 0)
	require.NoError(t, err)
	require.Empty(t, combos)
}

func TestEvalMatrix_SlashInValueRejected(t *testing.T) {
	def := MatrixDef{Dimensions: []MatrixDimension{{Name: "os", Source: lit("linux/gnu")}}}
	_, err := EvalMatrix(def, TemplateData{}, 0)
	require.ErrorContains(t, err, "must not contain")
}

func TestEvalMatrix_CapExceeded(t *testing.T) {
	def := MatrixDef{Dimensions: []MatrixDimension{
		{Name: "a", Source: lit("1", "2", "3")},
		{Name: "b", Source: lit("1", "2", "3")},
	}}
	_, err := EvalMatrix(def, TemplateData{}, 8)
	require.ErrorContains(t, err, "exceed")
}

func TestEvalMatrix_CapExceeded_BailsBeforeFullMaterialization(t *testing.T) {
	// 3 dimensions of 100 items each = 1,000,000 combinations. If EvalMatrix
	// materialized the full cartesian product before checking the cap, this
	// test would allocate ~1M MatrixCombo values (each with its own map) just
	// to then discard them. maxCombos is tiny (10) so a correct implementation
	// must detect the overflow while still expanding dimensions, not after.
	hundred := make([]string, 100)
	for i := range hundred {
		hundred[i] = fmt.Sprintf("v%d", i)
	}
	def := MatrixDef{Dimensions: []MatrixDimension{
		{Name: "a", Source: lit(hundred...)},
		{Name: "b", Source: lit(hundred...)},
		{Name: "c", Source: lit(hundred...)},
	}}

	start := time.Now()
	combos, err := EvalMatrix(def, TemplateData{}, 10)
	elapsed := time.Since(start)

	require.Nil(t, combos)
	require.ErrorContains(t, err, "exceed")
	require.ErrorContains(t, err, "10")
	// A naive implementation that first builds the full 1,000,000-element
	// product before checking the cap is dramatically slower and far more
	// memory-hungry than one that bails during expansion. This threshold is
	// generous but still catches full materialization.
	require.Less(t, elapsed, 500*time.Millisecond, "EvalMatrix took too long; looks like it materialized the full cartesian product before enforcing the cap")
}

func TestEvalMatrix_WithinCap_MatchesPreviousBehavior(t *testing.T) {
	def := MatrixDef{
		Dimensions: []MatrixDimension{
			{Name: "os", Source: lit("linux", "windows")},
			{Name: "arch", Source: lit("amd64", "arm64")},
		},
		Exclude: []map[string]string{{"os": "windows", "arch": "arm64"}},
	}
	combos, err := EvalMatrix(def, TemplateData{}, 8)
	require.NoError(t, err)
	require.Len(t, combos, 3)
	keys := make([]string, len(combos))
	for i, c := range combos {
		keys[i] = c.Key
	}
	require.Equal(t, []string{"linux/amd64", "linux/arm64", "windows/amd64"}, keys)
	require.Equal(t, map[string]string{"os": "linux", "arch": "amd64"}, combos[0].Values)
	for _, c := range combos {
		require.NotEqual(t, "windows/arm64", c.Key)
	}
}

func TestEvalMatrix_DynamicSource(t *testing.T) {
	def := MatrixDef{Dimensions: []MatrixDimension{
		{Name: "env", Source: ForeachSource{Expr: "$envs"}},
	}}
	combos, err := EvalMatrix(def, TemplateData{Params: map[string]string{"envs": `["dev","prod"]`}}, 0)
	require.NoError(t, err)
	require.Len(t, combos, 2)
	require.Equal(t, "dev", combos[0].Values["env"])
}

func TestOutputValueString(t *testing.T) {
	require.Equal(t, "plain", OutputValueString("plain"))
	require.Equal(t, `{"linux/amd64":"1.2"}`, OutputValueString(map[string]string{"linux/amd64": "1.2"}))
}

func TestTemplate_MatrixVarAndAggregatedOutputs(t *testing.T) {
	data := TemplateData{
		Matrix: map[string]string{"os": "linux"},
		Steps: map[string]StepData{
			"build": {Outputs: map[string]any{
				"version": map[string]string{"linux/amd64": "1.2", "linux/arm64": "1.3"},
			}},
		},
	}
	out, err := ExpandTemplate(`{{ .Matrix.os }}`, data)
	require.NoError(t, err)
	require.Equal(t, "linux", out)

	out, err = ExpandTemplate(`{{ index .Steps.build.Outputs.version "linux/arm64" }}`, data)
	require.NoError(t, err)
	require.Equal(t, "1.3", out)

	// keys はソート済み
	out, err = ExpandTemplate(`{{ keys .Steps.build.Outputs.version }}`, data)
	require.NoError(t, err)
	require.Equal(t, "[linux/amd64 linux/arm64]", out)
}
