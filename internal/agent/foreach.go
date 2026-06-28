package agent

import (
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// EvalForeachSource resolves a ClaimForeachSource to a []string using current template data.
func EvalForeachSource(src api.ClaimForeachSource, data dsl.TemplateData) ([]string, error) {
	return dsl.EvalForeachSource(dsl.ForeachSource{
		Literal: src.Literal,
		Expr:    src.Expr,
	}, data)
}
