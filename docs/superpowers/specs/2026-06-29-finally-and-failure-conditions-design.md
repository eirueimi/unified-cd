# Design: `finally` block & status condition functions

**Date:** 2026-06-29
**Status:** Approved (pending implementation plan)

## Problem

unified-cd has no way to guarantee that a step runs when a job fails. The only
related feature is the per-step `post:` hook ([internal/dsl/types.go:156](../../../internal/dsl/types.go)),
which is registered only when the step it is attached to **succeeds**
([internal/agent/agent.go:376](../../../internal/agent/agent.go)). There is no
job-level `finally`/`onFailure` block and no `if: failure()` / `if: always()`
condition — the `if` CEL environment exposes only `params`, `steps.*.outputs`,
and `secrets` ([internal/dsl/condition.go:24](../../../internal/dsl/condition.go)),
not run status.

This blocks the common CD needs: notify on failure, release resources, roll back.

## Goals

1. **Job-level `finally:` block** — steps that run after the main DAG completes,
   regardless of success / failure / cancellation.
2. **Status condition functions** in `if:` — `failure()`, `success()`, `always()`
   (job-wide scope).

Both are wanted (not either/or).

## Non-goals (YAGNI)

- `cancelled()` as a distinct exposed function (cancellation is folded into the
  `failure()`/`success()` semantics below).
- Per-step status access (`steps.X.status`).
- Changing the existing `post:` hook behavior.
- Controller-side orchestration of finally/failure handlers.

## Key semantic decisions

| Question | Decision |
|---|---|
| Feature shape | **Both** a job-level `finally` block **and** step-level `failure()`/`success()`/`always()` |
| Status vocabulary | `failure()` / `success()` / `always()`, **job-wide** (no `cancelled()`, no per-step status) |
| On cancellation (timeout / manual) | `finally` **runs** (it is `always`); `failure()` returns **false** (cancel ≠ failure) |
| `finally` block structure | Same as `steps` — StepEntry list (stage + `parallel`) |
| `finally` / `failure()` step itself fails | Run becomes **Failed**. All `finally` steps run to completion first (no fail-fast inside `finally`). |

## Existing execution model (baseline)

- `failFast` was **removed** ([internal/dsl/parse.go:47](../../../internal/dsl/parse.go),
  [internal/dsl/types.go:25](../../../internal/dsl/types.go)). Current semantics:
  **a stage failure aborts subsequent stages; already-started steps in the
  failing stage run to completion.**
- `RunPipeline` ([internal/agent/pipeline.go:56](../../../internal/agent/pipeline.go))
  runs stages sequentially and returns on the first stage error → later stages
  are skipped.
- The controller compiles `spec.Steps` → `resp.Stages` in
  [buildClaimResponse](../../../internal/controller/api_agent.go) (api_agent.go:121).
- The agent evaluates each step's `if` via `dsl.EvalCondition`
  ([internal/agent/agent.go:262](../../../internal/agent/agent.go)).

## Chosen approach: status-aware single pipeline (Approach A)

Rejected alternatives:
- **B — two-phase (normal DAG, then a failure-handler pass):** smaller core
  change, but an inline `if: always()` step would run at the *end* instead of in
  its declared position — counter-intuitive.
- **C — controller orchestration:** a run executes on a single agent / single
  workspace, so crossing the agent boundary only adds latency and complexity.

Approach A keeps steps running in their declared position and reuses the
condition + stage infrastructure. The cost is a careful change to
`RunPipeline`'s abort-on-failure behavior, made backward-compatible by the
"implicit `success()`" rule below.

## Design

### ① DSL / schema — `internal/dsl/types.go`, `parse.go`

Add `Finally []StepEntry` to `Spec`. It is parsed and validated with the same
rules as `Steps` (reuse StepEntry validation). `finally` entries may use
`parallel`, `uploadArtifact`, `if`, etc.

```yaml
spec:
  steps:
    - name: build
      run: make build
    - name: deploy
      run: make deploy
  finally:
    - name: notify          # always runs (cleanup / notify)
      run: ./notify.sh "{{ .RunID }}"
    - name: rollback
      if: failure()         # failure only
      run: ./rollback.sh
```

### ② Condition functions & core semantics — `internal/dsl/condition.go`

Add three nullary CEL functions usable in any `if:`: `failure()`, `success()`,
`always()`. Inject run status into the CEL env as two booleans: `failed`,
`cancelled`.

- `always()` → `true`
- `failure()` → `failed` (true only when a real step Failed; **false on
  cancel-only**)
- `success()` → `!failed && !cancelled`

**Implicit `success()` rule (GitHub Actions style):** if an `if:` expression does
**not** reference any status function (`always`/`failure`/`success`), it is
implicitly ANDed with `success()`. An empty `if:` is treated as `success()`.

`EvalCondition`'s signature gains a status argument
(e.g. `RunStatusView{Failed, Cancelled bool}`); existing callers pass the
zero value (success state), preserving current behavior.

Truth table:

| Situation | no `if` | `if: failure()` | `if: always()` | `if: <non-status expr>` |
|---|---|---|---|---|
| before any failure | run | skip | run | eval expr (implicit success() = true) |
| after a step Failed | **skip** | **run** | run | skip |
| cancelled | skip | skip | run | skip |

This reproduces today's "abort subsequent stages on failure" behavior for
unmodified jobs, because before any failure `failed == cancelled == false`, so
the implicit `success()` is true and nothing changes.

### ③ API types — `internal/api/types.go`

Add `Finally []ClaimStage` to `ClaimResponse`. `finally` steps continue the flat
step-index counter so `ReportStep` and the web UI render them correctly.

### ④ Controller compile — `internal/controller/api_agent.go`

Extract the existing `spec.Steps` → stages loop from `buildClaimResponse` into a
helper, e.g. `buildStages(entries []dsl.StepEntry, stepIdx *int, secretsNeeded map[string]struct{}) []api.ClaimStage`.
Use it for both `resp.Stages` (from `spec.Steps`) and `resp.Finally` (from
`spec.Finally`), sharing the `stepIdx` counter. Collect secret names from
`finally` too.

### ⑤ Agent execution — `internal/agent/pipeline.go`, `agent.go`

1. **Failure tracking:** add `anyFailed atomic.Bool`, set when a
   non-`continueOnError` step reports `Failed`. Cancellation continues to use the
   existing `cancelledByMaster`.
2. **`RunPipeline` change:** a stage failure no longer returns early — subsequent
   stages are still visited. Each step's `if` is evaluated (rule ②) with the
   current `(failed, cancelled)` status; a `false` result reports `Skipped`. The
   "auto-skip normal steps after a failure" behavior falls out of the implicit
   `success()` rule automatically.
3. **`finally` execution:** after the main DAG completes (on success, failure,
   **or** cancellation), run `resp.Finally`.
   - `finally` `if:` evaluation uses a **frozen snapshot** of the main DAG's final
     status (`mainFailed`, `mainCancelled`).
   - `finally` steps do **not** auto-skip each other — all `finally` steps run to
     completion (no fail-fast inside `finally`).
   - A `finally` step failure is recorded into the overall-Failed flag.
4. **Final status** ([internal/agent/agent.go:424](../../../internal/agent/agent.go) area):
   precedence `Failed (main or finally) > Cancelled > Succeeded`.

### ⑥ Tests

- `parse_test.go`: `finally` parsing & validation.
- `condition` tests: `failure()`/`success()`/`always()` and the implicit
  `success()` truth table.
- Agent integration tests:
  1. a step fails → `finally` runs, a `failure()` step runs, a success-only step
     is skipped;
  2. run cancelled → `finally` runs, `failure()` is false;
  3. a `finally` step itself fails → run is Failed (all `finally` steps still ran).

### ⑦ Docs / schema

- `docs/jobs.md`: add a `finally:` section and document the `if:` status
  functions.
- Regenerate `schemas/unified-cd.schema.json` (schemagen) and
  `docs/field-reference.md` (docgen).

## Touch points summary

| File | Change |
|---|---|
| `internal/dsl/types.go` | `Finally []StepEntry` on `Spec`; CEL status view type |
| `internal/dsl/parse.go` | parse/validate `finally` |
| `internal/dsl/condition.go` | status functions + implicit `success()`; new status arg |
| `internal/api/types.go` | `Finally []ClaimStage` on `ClaimResponse` |
| `internal/controller/api_agent.go` | `buildStages` helper; compile `finally`; secrets |
| `internal/agent/pipeline.go` | non-aborting pipeline; status-aware skip |
| `internal/agent/agent.go` | `anyFailed` tracking; run `finally`; final status precedence |
| `docs/jobs.md`, `schemas/`, `docs/field-reference.md` | docs + regenerated schema |
