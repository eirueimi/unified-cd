package agent

import (
	"fmt"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// ExpandMatrixStep expands a matrix-bearing ClaimStep into one copy per
// combination (MatrixValues/MatrixKey set, Matrix cleared). Non-matrix steps
// are returned as a single-element slice unchanged. Shared by the standard
// agent pipeline and the k8s agent orchestrator.
func ExpandMatrixStep(step api.ClaimStep, data dsl.TemplateData, maxCombos int) ([]api.ClaimStep, error) {
	if step.Matrix == nil {
		return []api.ClaimStep{step}, nil
	}
	def := dsl.MatrixDef{Exclude: step.Matrix.Exclude}
	for _, d := range step.Matrix.Dimensions {
		def.Dimensions = append(def.Dimensions, dsl.MatrixDimension{
			Name:   d.Name,
			Source: dsl.ForeachSource{Literal: d.Source.Literal, Expr: d.Source.Expr},
		})
	}
	combos, err := dsl.EvalMatrix(def, data, maxCombos)
	if err != nil {
		return nil, fmt.Errorf("matrix expansion for step %q: %w", step.Name, err)
	}
	out := make([]api.ClaimStep, len(combos))
	for i, c := range combos {
		s := step
		s.Matrix = nil
		s.MatrixValues = c.Values
		s.MatrixKey = c.Key
		out[i] = s
	}
	return out, nil
}
