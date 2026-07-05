package dsl

import (
	"fmt"
	"regexp"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

// RunStatusView exposes the run-level status to if: condition functions.
type RunStatusView struct {
	Failed    bool // a non-continueOnError step failed (and the run was not cancelled)
	Cancelled bool // the run was cancelled (timeout or manual)
}

// statusFuncRe matches a call to a status function: always(), failure(), success().
// This is a textual heuristic and could (rarely) match a status-function name inside
// a string literal; the fail-direction is "run the step", which is safe.
var statusFuncRe = regexp.MustCompile(`\b(?:always|failure|success)\s*\(`)

// EvalCondition evaluates a CEL expression and returns a bool.
//
// Variables:
//
//	params   map(string, string) — Run parameters
//	steps    map(string, dyn)    — completed steps; access via steps.name.outputs.key
//	secrets  map(string, string) — resolved secret values
//
// Functions (zero-arg):
//
//	failure()  → status.Failed
//	success()  → !status.Failed && !status.Cancelled
//	always()   → true
//
// implicitSuccess applies GitHub-style semantics: when true and expr references
// no status function, the result is ANDed with success(); an empty expr is
// treated as success(). When false (used for finally), an empty expr means
// always-run and a non-status expr is evaluated literally.
//
// On compile or evaluation error it returns (true, err) (fail-safe = run the step).
func EvalCondition(expr string, data TemplateData, status RunStatusView, implicitSuccess bool) (bool, error) {
	successVal := !status.Failed && !status.Cancelled

	if expr == "" {
		if implicitSuccess {
			return successVal, nil
		}
		return true, nil
	}

	env, err := cel.NewEnv(
		cel.Variable("params", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("steps", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("secrets", cel.MapType(cel.StringType, cel.StringType)),
		cel.Function("failure", cel.Overload("failure_bool", []*cel.Type{}, cel.BoolType,
			cel.FunctionBinding(func(...ref.Val) ref.Val { return types.Bool(status.Failed) }))),
		cel.Function("success", cel.Overload("success_bool", []*cel.Type{}, cel.BoolType,
			cel.FunctionBinding(func(...ref.Val) ref.Val { return types.Bool(successVal) }))),
		cel.Function("always", cel.Overload("always_bool", []*cel.Type{}, cel.BoolType,
			cel.FunctionBinding(func(...ref.Val) ref.Val { return types.Bool(true) }))),
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
			outputs = map[string]any{}
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

	if implicitSuccess && !statusFuncRe.MatchString(expr) {
		return b && successVal, nil
	}
	return b, nil
}
