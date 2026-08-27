# How the DSL is evaluated

A manifest goes through **two different evaluation systems** with different
syntax, different data, and — until recently — opposite behaviour for the same
mistake. Knowing which is which is most of what you need.

| | `{{ ... }}` interpolation | `if:` conditions |
|---|---|---|
| Engine | Go `text/template` | CEL |
| Entry point | `dsl.ExpandTemplate` | `dsl.EvalCondition` |
| Data | `dsl.TemplateData` (7 fields) | a **subset** — see below |
| Undefined key | empty string, silently | empty string, plus a run-log line |
| On error | the run or step fails | **fail-open: the step runs** |

## The subset that `if:` can see

`TemplateData` carries seven fields. `if:` conditions expose **four**:

| Field | `{{ }}` | `if:` |
|---|---|---|
| `Params` | yes | yes |
| `Vars` | yes | yes |
| `Steps` | yes | yes |
| `Secrets` | yes | yes |
| `Stdout` | yes | **no** |
| `Foreach` | yes | **no** |
| `Matrix` | yes | **no** |

The `if:` bindings are the `conditionVars` table in
`internal/dsl/condition.go`. That table is derived rather than hand-maintained
in two places — the CEL *declarations* and the *activation* must agree, and
keeping them as one table is what stops them drifting. See
[Invariants — hand-maintained lists drift](invariants.md).

**So an `if:` cannot branch on the current matrix combination or foreach item.**
If you need that, the shape to reach for is a `when`-style filter on the matrix
definition, not a condition — and if you are adding matrix/foreach to CEL,
adding a `conditionVars` entry is the mechanical part; deciding what an
undefined key means is the part that matters.

## The asymmetry that caused a real bug

`ExpandTemplate` parses with `Option("missingkey=zero")`. An undefined key
therefore expands to the **empty string, silently** — no error, no warning.

CEL does the opposite: an undefined map key raises `no such key`. And
`EvalCondition` returns `(true, nil, err)` on any evaluation error, because a
broken expression must not silently skip work.

Put together, those two reasonable decisions produced this:

```yaml
steps:
  - name: deploy
    if: params.deply == "yes"     # typo
    run: ./deploy.sh
```

The typo raised `no such key`, the fail-safe ran the step, and **a gate written
to be closed defaulted open**. The same trap existed for `secrets`.

Both now behave like `{{ }}` — an undefined key reads as the empty string — and
additionally write a System line into the run's own log, so the typo is
visible rather than merely harmless.

`steps` deliberately still uses CEL's default map semantics: it is
`map(string, dyn)`, so defaulting a missing step to `""` would only move the
failure to the `.outputs` access rather than closing it.

!!! warning "If you add anything to the CEL environment"
    Decide explicitly what an undefined reference does, and remember that
    **erroring is the dangerous choice, not the safe one**. Fail-open means an
    error opens gates.

## Where each thing happens

Evaluation is spread across the controller and the agent, and which side does
what determines what data is available.

**Controller, at run creation:**

- `resolveParams` fills declared inputs from supplied values and `default:`.
- `ExpandAgentSelector` interpolates params, so routing can be computed per
  run.
- `spec.displayName` is interpolated once.

**Controller, while Pending:**

- `uses:` git templates are fetched and inlined into the run's stored spec,
  by `RunGitResolver`.
- `ExpandConcurrency` interpolates params into the concurrency mutex/pool/
  orLock names — not at creation, but later, when `RunScheduler` dequeues the
  run (`tryQueueRun`, transitioning it from Pending to Queued). A concurrency
  key that depends on a param is therefore not resolved until the run is
  about to be queued, not when it is created.

**Agent, per step:**

- `ExpandTemplate` on the step's `run:` text, `env:`, cache keys and so on,
  with the full `TemplateData` including prior steps' outputs.
- `EvalCondition` on `if:`, with the four-field subset.

The split matters because **a prior step's outputs cannot exist at run
creation**. Anything the controller expands can only see params.

## `resolveParams` passes undeclared params through, by design

This surprises people, and there is a decision recorded behind it.

A caller may supply a param the job never declared, and `resolveParams` keeps
it. Five separate caller paths can introduce one, and the function's own doc
comment says the pass-through is intentional.

A consequence: **a typo and a legitimate pass-through are statically
indistinguishable.** Rejecting undeclared params at apply time was proposed,
investigated, and deliberately withdrawn for that reason. The runtime lever —
an undefined key reading as empty, plus a visible System line — is the agreed
mechanism. Do not re-propose apply-time rejection without new evidence.

One more subtlety in the same function: an explicitly supplied **empty** value
is treated as unset, so a declared `default:` still applies. A param with no
declared default keeps the caller's empty string, because there is nothing to
fall back to.

## Validating a param's value

Three mechanisms, and they interact:

- `pattern:` — a regular expression the value must match.
- `choices:` — a fixed list; a strict allow-list, and mutually exclusive with
  `pattern:`.
- `unvalidated: true` — an explicit opt-out.

Params are interpolated into step shell text, so a param fed from an untrusted
source is a command-injection vector. `validateWebhookPayloadMappedParams`
therefore requires **one of the three** on any param templated from a webhook
payload.

**If you add a fourth mechanism, wire it into that gate.** Otherwise an author
using the new mechanism is told to add a redundant `pattern:`, and the path of
least resistance becomes `unvalidated: true` — which makes the feature a net
loss for security.

## Where to look

| Question | File |
|---|---|
| What is a valid manifest? | `internal/dsl/parse.go` |
| What can `if:` see, and what does an undefined key do? | `internal/dsl/condition.go` |
| How does `{{ }}` expand? | `internal/dsl/template.go` |
| How are params resolved and validated at run creation? | `internal/controller/params.go` |
| What does the schema require? | generated — see [Architecture](architecture.md#generated-artifacts) |
