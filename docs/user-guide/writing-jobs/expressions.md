# Expressions and Conditions

## Template Syntax

Job YAML values support Go template expressions (`{{ expr }}`).

### Available variables

| Variable | Available in | Description |
|---|---|---|
| `{{ .Params.NAME }}` | `run`, `env`, `agentSelector`, `concurrency`, `outputs`, `displayName`, `call.with`, `uses.with`, `cache.key`, `cache.path`, `cache.restoreKeys` | Input parameter value |
| `{{ .Vars.NAME }}` | `run`, `env`, `outputs`, `call.with`, `cache.key`, `cache.path`, `cache.restoreKeys` | Plain-text variable — a global `kind: Vars` manifest merged with the job's `spec.vars`. See [Variables](../variables.md). |
| `{{ .Steps.NAME.Outputs.KEY }}` | `run`, `env`, `outputs` | Output from a completed step |
| `{{ secrets.NAME }}` | `env` values, `run` strings | Decrypted secret value |

> **None of these `{{ }}` forms work in `if:`.** `if:` is
> [CEL](https://github.com/google/cel-go), not a Go template — the spellings
> there are `params.NAME`, `vars.NAME`, `steps.NAME.outputs.KEY` and
> `secrets.NAME`, with no braces and no leading dot. A `{{ }}` expression in an
> `if:` is rejected at apply time; see
> [Conditional Execution (`if`)](steps.md#conditional-execution-if).

> **`agentSelector:` and `concurrency:` cannot use `{{ .Vars.NAME }}`.** Both
> are expanded by the controller when the run is *created*, and variables are
> merged later, when the run is claimed — so a `.Vars` reference in either field
> expands to the empty string, silently. Use `{{ .Params.NAME }}` there. See
> [Variables: where variables do not reach](../variables.md#where-variables-do-not-reach).

> Step status is not exposed as a template variable. To branch on a step's
> outcome in an `if:` expression, use the CEL functions `failure()`,
> `success()`, or `always()` (see [Status Functions in `if:`](#status-functions-in-if)).

### Template functions

Standard Go template functions are available, plus:

| Function | Example | Description |
|---|---|---|
| `trim` | `{{ .Stdout \| trim }}` | Remove leading/trailing whitespace |
| `trimSpace` | `{{ .Stdout \| trimSpace }}` | Same as `trim` |
| `eq` | `{{ eq .Params.env "prod" }}` | Equality |
| `ne` | `{{ ne .Params.env "prod" }}` | Inequality |
| `and` | `{{ and (eq .Params.a "x") (eq .Params.b "y") }}` | Logical AND |
| `or` | `{{ or (eq .Params.a "x") (eq .Params.b "y") }}` | Logical OR |
| `not` | `{{ not (eq .Params.a "x") }}` | Logical NOT |

---

## Status Functions in `if:`

Three zero-argument functions are available in any step `if:` (job-wide scope):

| Function | True when |
|---|---|
| `failure()` | a previous non-`continueOnError` step has failed (not on cancel) |
| `success()` | no step has failed and the run was not cancelled |
| `always()`  | always |

If an `if:` expression does **not** mention a status function, it is implicitly
treated as requiring `success()` — so a normal step is skipped once an earlier
step has failed (GitHub Actions semantics). Add `if: failure()` or
`if: always()` to opt in to running after a failure.

> **Compile/eval errors fail open.** `if:` is CEL — see the warning under
> [Conditional Execution](steps.md#conditional-execution-if). An `if:` that doesn't
> compile (e.g. leftover `{{ }}` Go-template syntax) does not fail the run or
> skip the step: the step **runs anyway**, and only a warning is written to
> the agent log. Double-check any non-trivial `if:` expression before relying
> on it to gate a sensitive step (e.g. a production deploy).

---

