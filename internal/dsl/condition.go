package dsl

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
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

// conditionVar declares ONE variable an if: expression can reference.
//
// The CEL declaration (name + type) and the runtime value live in the SAME
// record on purpose. Before this table the declarations and the activation map
// were two hand-maintained lists in two places with nothing checking they
// agreed — which is exactly how `vars` came to be a run-time variable that no
// if: expression could see. Deriving both halves from one table (see
// conditionEnvOptions and conditionActivation) makes that class of drift
// impossible rather than merely tested for: there is no second list to forget.
//
// value receives the undefined-key recorder so a variable MAY choose
// missing-key-reads-as-empty semantics (see `vars`); a variable that wants
// CEL's default "no such key" error just ignores it.
type conditionVar struct {
	name  string
	typ   *cel.Type
	value func(data TemplateData, undef *undefinedKeys) any
}

// conditionVars is the single source of truth for the if: environment.
//
// Adding a variable here declares it to the compiler AND binds it at eval
// time. TestConditionVars_EveryDeclaredVarIsBound evaluates a trivial
// expression against every entry, so an entry whose value does not fit its
// declared type fails a test instead of failing open at run time.
var conditionVars = []conditionVar{
	{
		name: "params",
		typ:  cel.MapType(cel.StringType, cel.StringType),
		// NOTE: params keeps CEL's default map semantics, so `params.TYPO`
		// raises "no such key" and EvalCondition's fail-safe runs the step.
		// That is the same always-runs trap `vars` had; it is left alone here
		// deliberately (changing the meaning of every existing params-gated
		// condition is not a change to make as a side effect of adding vars)
		// and is reported separately.
		value: func(d TemplateData, _ *undefinedKeys) any { return orEmptyStrings(d.Params) },
	},
	{
		name: "vars",
		typ:  cel.MapType(cel.StringType, cel.StringType),
		// vars reads an undefined key as the empty string instead of raising,
		// matching `{{ .Vars.X }}` in a template (which expands to empty) and
		// keeping a typo'd gate CLOSED rather than fail-open. Every undefined
		// key that was actually consulted is recorded and surfaced by the
		// caller — see EvalCondition's warnings return.
		value: func(d TemplateData, undef *undefinedKeys) any {
			return defaultingMap{
				Mapper:  types.NewStringStringMap(types.DefaultTypeAdapter, orEmptyStrings(d.Vars)),
				varName: "vars",
				undef:   undef,
			}
		},
	},
	{
		name:  "steps",
		typ:   cel.MapType(cel.StringType, cel.DynType),
		value: func(d TemplateData, _ *undefinedKeys) any { return stepsActivation(d.Steps) },
	},
	{
		name:  "secrets",
		typ:   cel.MapType(cel.StringType, cel.StringType),
		value: func(d TemplateData, _ *undefinedKeys) any { return orEmptyStrings(d.Secrets) },
	},
}

func orEmptyStrings(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func stepsActivation(steps map[string]StepData) map[string]any {
	out := make(map[string]any, len(steps))
	for name, sd := range steps {
		outputs := sd.Outputs
		if outputs == nil {
			outputs = map[string]any{}
		}
		out[name] = map[string]any{"outputs": outputs}
	}
	return out
}

// undefinedKeys records every undefined key a defaultingMap served during one
// evaluation, so the caller can say WHICH key was silently empty. Only one
// goroutine evaluates a given program, so no locking is needed.
type undefinedKeys struct{ seen map[string]bool }

func (u *undefinedKeys) record(varName, key string) {
	if u.seen == nil {
		u.seen = map[string]bool{}
	}
	u.seen[varName+"."+key] = true
}

func (u *undefinedKeys) sorted() []string {
	if len(u.seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(u.seen))
	for k := range u.seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// defaultingMap is a CEL map whose UNDEFINED keys read as the empty string
// instead of raising "no such key", recording each one on the way through.
//
// Why: CEL raises an error for a missing map key, EvalCondition's error path
// is fail-safe ("run the step"), so `vars.TYPO == "prod"` would run a step its
// author meant to gate — invisibly, and with the author's intent inverted.
// Empty-on-missing keeps the gate shut and matches the template side, where
// `{{ .Vars.TYPO }}` expands to empty.
//
// Presence testing: `"X" in vars` still answers truthfully — the `in` operator
// goes through Contains, which is inherited from the real map. `has(vars.X)`
// does NOT: CEL routes both a value read and a presence test through Find, so
// a Find that always succeeds makes has() always true. Use `"X" in vars`.
type defaultingMap struct {
	traits.Mapper
	varName string
	undef   *undefinedKeys
}

func (m defaultingMap) Find(key ref.Val) (ref.Val, bool) {
	v, found := m.Mapper.Find(key)
	if found || types.IsError(v) {
		// A wrong-typed key yields an error value with found=false; pass it
		// through untouched so CEL reports the type problem as it always did.
		return v, found
	}
	s, ok := key.(types.String)
	if !ok {
		return v, false
	}
	m.undef.record(m.varName, string(s))
	return types.String(""), true
}

// Get backs the index form (`vars["X"]`) for any planner path that does not go
// through Find, so both spellings default identically.
func (m defaultingMap) Get(key ref.Val) ref.Val {
	v, found := m.Find(key)
	if !found {
		return types.ValOrErr(key, "no such key: %v", key)
	}
	return v
}

// conditionEnvOptions builds the if: environment from conditionVars plus the
// three status functions. status supplies the function bindings; the
// DECLARATIONS do not depend on it, which is what lets ValidateConditionExpr
// compile an expression at apply time against the very same environment the
// agent will use at run time.
func conditionEnvOptions(status RunStatusView) []cel.EnvOption {
	successVal := !status.Failed && !status.Cancelled
	opts := make([]cel.EnvOption, 0, len(conditionVars)+3)
	for _, v := range conditionVars {
		opts = append(opts, cel.Variable(v.name, v.typ))
	}
	return append(opts,
		cel.Function("failure", cel.Overload("failure_bool", []*cel.Type{}, cel.BoolType,
			cel.FunctionBinding(func(...ref.Val) ref.Val { return types.Bool(status.Failed) }))),
		cel.Function("success", cel.Overload("success_bool", []*cel.Type{}, cel.BoolType,
			cel.FunctionBinding(func(...ref.Val) ref.Val { return types.Bool(successVal) }))),
		cel.Function("always", cel.Overload("always_bool", []*cel.Type{}, cel.BoolType,
			cel.FunctionBinding(func(...ref.Val) ref.Val { return types.Bool(true) }))),
	)
}

// conditionActivation builds the eval-time variable bindings from the same
// table the declarations come from.
func conditionActivation(data TemplateData, undef *undefinedKeys) map[string]any {
	act := make(map[string]any, len(conditionVars))
	for _, v := range conditionVars {
		act[v.name] = v.value(data, undef)
	}
	return act
}

// ValidateConditionExpr compiles an if: expression against the SAME
// environment EvalCondition uses at run time, so a malformed condition is
// rejected while its author is present instead of failing open on the agent.
//
// It reports ONLY what env.Compile rejects — a syntax error, an unknown
// identifier, a type mismatch. Because the declarations are identical (see
// conditionEnvOptions), anything rejected here is guaranteed to hit the exact
// same compile error at run time and therefore to fail open, so this can never
// reject a condition that actually works today.
//
// It deliberately does NOT apply EvalCondition's "must return bool" check.
// `steps` is map(string, dyn), so `steps.build.outputs.ok` type-checks to dyn
// and would be rejected by that check even though its shape is only knowable
// at run time. Such an expression is already broken at run time — but failing
// an APPLY on it would break re-applying a job that currently applies fine,
// which is a bigger blast radius than the check is worth. Run-time keeps the
// bool check; apply-time stops at "does it compile".
func ValidateConditionExpr(expr string) error {
	if expr == "" {
		return nil
	}
	env, err := cel.NewEnv(conditionEnvOptions(RunStatusView{})...)
	if err != nil {
		return fmt.Errorf("if: cel env: %w", err)
	}
	if _, iss := env.Compile(expr); iss != nil && iss.Err() != nil {
		return fmt.Errorf("if: expression %q does not compile (if: is a CEL expression, not a Go template): %w", expr, iss.Err())
	}
	return nil
}

// EvalCondition evaluates a CEL expression and returns a bool, any warnings
// worth showing the run's author, and an error.
//
// Variables (all derived from conditionVars):
//
//	params   map(string, string) — Run parameters
//	vars     map(string, string) — plain-text variables (kind: Vars + spec.vars)
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
// An UNDEFINED vars key reads as the empty string rather than erroring (see
// defaultingMap) and is reported in warnings. warnings are never a reason to
// change the result — the caller's job is to make them visible, and the agent
// puts them in the RUN's own log, because a condition that quietly did not
// mean what it says is invisible in the agent's process log.
//
// On compile or evaluation error it returns (true, nil, err) (fail-safe = run
// the step).
func EvalCondition(expr string, data TemplateData, status RunStatusView, implicitSuccess bool) (bool, []string, error) {
	successVal := !status.Failed && !status.Cancelled

	if expr == "" {
		if implicitSuccess {
			return successVal, nil, nil
		}
		return true, nil, nil
	}

	env, err := cel.NewEnv(conditionEnvOptions(status)...)
	if err != nil {
		return true, nil, fmt.Errorf("if: cel env: %w", err)
	}

	ast, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return true, nil, fmt.Errorf("if: expression %q compile error: %w", expr, iss.Err())
	}
	if !ast.OutputType().IsExactType(cel.BoolType) {
		return true, nil, fmt.Errorf("if: expression %q must return bool, got %s", expr, ast.OutputType())
	}

	prg, err := env.Program(ast)
	if err != nil {
		return true, nil, fmt.Errorf("if: program: %w", err)
	}

	undef := &undefinedKeys{}
	out, _, err := prg.Eval(conditionActivation(data, undef))
	if err != nil {
		return true, nil, fmt.Errorf("if: expression %q eval error: %w", expr, err)
	}

	var warnings []string
	if missing := undef.sorted(); len(missing) > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"if: expression %q referenced undefined %s — an undefined key reads as the empty string, "+
				"so the condition was evaluated as though it were \"\" (check the spelling, or define it in a kind: Vars manifest or the job's spec.vars)",
			expr, strings.Join(missing, ", ")))
	}

	b, ok := out.Value().(bool)
	if !ok {
		// OutputType check above guarantees this branch is unreachable
		return true, nil, fmt.Errorf("if: expression %q returned non-bool", expr)
	}

	if implicitSuccess && !statusFuncRe.MatchString(expr) {
		return b && successVal, warnings, nil
	}
	return b, warnings, nil
}
