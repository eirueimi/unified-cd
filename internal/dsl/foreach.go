package dsl

import (
	"encoding/json"
	"fmt"
	"strings"
)

// EvalForeachSource resolves a ForeachSource to a []string using current template data.
// Literal: returned as-is.
// Expr starting with "$name": JSON-decode params[name] as []string; fall back to comma-split.
// Expr containing "{{": evaluate as Go template; split result on whitespace then commas.
func EvalForeachSource(src ForeachSource, data TemplateData) ([]string, error) {
	if len(src.Literal) > 0 {
		return src.Literal, nil
	}
	expr := src.Expr
	if strings.HasPrefix(expr, "$") {
		paramName := strings.TrimPrefix(expr, "$")
		raw, ok := data.Params[paramName]
		if !ok {
			return nil, fmt.Errorf("param %q not found", paramName)
		}
		var items []string
		if err := json.Unmarshal([]byte(raw), &items); err != nil {
			items = strings.Split(raw, ",")
		}
		return items, nil
	}
	result, err := ExpandTemplate(expr, data)
	if err != nil {
		return nil, err
	}
	result = strings.TrimSpace(result)
	if result == "" {
		return nil, nil
	}
	result = strings.TrimPrefix(result, "[")
	result = strings.TrimSuffix(result, "]")
	parts := strings.Fields(result)
	if len(parts) == 1 {
		parts = strings.Split(result, ",")
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts, nil
}
