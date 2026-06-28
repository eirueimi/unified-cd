package dsl

import (
	"fmt"

	"github.com/google/cel-go/cel"
)

// EvalCondition evaluates a CEL expression and returns a bool.
// If expr is an empty string, it returns true (no condition = always execute).
// On compile or evaluation error it returns (true, err) (fail-safe = execute the step).
//
// Variables:
//
//	params   map(string, string) — Run parameters
//	steps    map(string, dyn)    — completed steps; access via steps.name.outputs.key
//	secrets  map(string, string) — resolved secret values
func EvalCondition(expr string, data TemplateData) (bool, error) {
	if expr == "" {
		return true, nil
	}

	env, err := cel.NewEnv(
		cel.Variable("params", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("steps", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("secrets", cel.MapType(cel.StringType, cel.StringType)),
	)
	if err != nil {
		return true, fmt.Errorf("if: cel env: %w", err)
	}

	ast, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return true, fmt.Errorf("if: expression %q compile error: %w", expr, iss.Err())
	}
	if !ast.OutputType().IsExactType(cel.BoolType) {
		return true, fmt.Errorf("if: expression %q must return bool, got %s", expr, ast.OutputType())
	}

	prg, err := env.Program(ast)
	if err != nil {
		return true, fmt.Errorf("if: program: %w", err)
	}

	params := data.Params
	if params == nil {
		params = map[string]string{}
	}
	secrets := data.Secrets
	if secrets == nil {
		secrets = map[string]string{}
	}
	stepsAny := make(map[string]any, len(data.Steps))
	for name, sd := range data.Steps {
		outputs := sd.Outputs
		if outputs == nil {
			outputs = map[string]string{}
		}
		stepsAny[name] = map[string]any{"outputs": outputs}
	}

	out, _, err := prg.Eval(map[string]any{
		"params":  params,
		"steps":   stepsAny,
		"secrets": secrets,
	})
	if err != nil {
		return true, fmt.Errorf("if: expression %q eval error: %w", expr, err)
	}

	b, ok := out.Value().(bool)
	if !ok {
		// OutputType check above guarantees this branch is unreachable
		return true, fmt.Errorf("if: expression %q returned non-bool", expr)
	}
	return b, nil
}
