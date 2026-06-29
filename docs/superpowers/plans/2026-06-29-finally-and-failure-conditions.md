# `finally` block & `if` status condition functions — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a job-level `finally:` block and `failure()` / `success()` / `always()` condition functions to step `if:`, so a step can be guaranteed to run when a job fails (notify / cleanup / rollback).

**Architecture:** Approach A (status-aware single pipeline). The agent tracks a run-level "failed" flag during execution; each step's `if:` is evaluated against that status with GitHub-style implicit `success()` semantics, so steps after a failure auto-skip unless they opt in with `failure()`/`always()`. The `finally` block is a second stage list, compiled by the controller and run by the agent after the main DAG completes — on success, failure, or cancellation.

**Tech Stack:** Go 1.26, `github.com/google/cel-go` v0.28.1 (CEL for `if:`), testify. Schema/docs are generated via `go generate ./internal/dsl/`.

## Global Constraints

- Go module: `github.com/eirueimi/unified-cd`. Go 1.26.2.
- `failFast` and step `needs` are already removed — do not reintroduce them.
- Status vocabulary is exactly `failure()` / `success()` / `always()`, **job-wide**. No `cancelled()`, no per-step status access.
- On cancellation (timeout/manual): `finally` runs; `failure()` returns **false**.
- A `finally` step that itself fails marks the run **Failed**; all `finally` steps still run to completion first.
- `finally` block uses the same structure as `steps` (StepEntry list: stage + `parallel`).
- Secret values must never be written to logs (existing masker pattern).
- Spec of record: `docs/superpowers/specs/2026-06-29-finally-and-failure-conditions-design.md`.

---

## File map

| File | Responsibility | Change |
|---|---|---|
| `internal/dsl/types.go` | Job schema structs | Add `Finally []StepEntry` to `Spec` |
| `internal/dsl/parse.go` | Parse + validate | Validate `finally` entries (shared helper) |
| `internal/dsl/condition.go` | `if:` evaluation | Status functions + implicit `success()`; new signature |
| `internal/api/types.go` | Wire types | Add `Finally []ClaimStage` to `ClaimResponse` |
| `internal/controller/api_agent.go` | Compile run → stages | `buildStages` helper; compile `finally`; collect secrets |
| `internal/agent/agent.go` | Execute run | Failure tracking, status-aware `if`, run `finally`, status precedence |
| `docs/jobs.md`, `schemas/`, `docs/field-reference.md` | Docs + generated schema | Document feature; regenerate |

---

## Task 1: DSL — `finally` field and validation

**Files:**
- Modify: `internal/dsl/types.go` (the `Spec` struct, ~line 20)
- Modify: `internal/dsl/parse.go` (`Validate`, `checkForbiddenJobFields`)
- Test: `internal/dsl/parse_test.go`

**Interfaces:**
- Produces: `dsl.Spec.Finally []StepEntry` (yaml `finally`); validation rejecting malformed finally entries and duplicate step names across `steps`+`finally`.

- [ ] **Step 1: Write the failing test**

Add to `internal/dsl/parse_test.go`:

```go
func TestParse_FinallyValid(t *testing.T) {
	y := `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: with-finally
spec:
  steps:
    - name: build
      run: make build
  finally:
    - name: notify
      run: ./notify.sh
    - name: rollback
      if: failure()
      run: ./rollback.sh`
	job, err := Parse(strings.NewReader(y))
	require.NoError(t, err)
	require.Len(t, job.Spec.Finally, 2)
	assert.Equal(t, "notify", job.Spec.Finally[0].Name)
	assert.Equal(t, "failure()", job.Spec.Finally[1].If)
}

func TestParse_FinallyDuplicateNameAcrossStepsAndFinally(t *testing.T) {
	y := `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: dup
spec:
  steps:
    - name: build
      run: make build
  finally:
    - name: build
      run: ./cleanup.sh`
	_, err := Parse(strings.NewReader(y))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate step name")
}

func TestParse_FinallyStepMissingAction(t *testing.T) {
	y := `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: bad
spec:
  steps:
    - name: build
      run: make build
  finally:
    - name: cleanup`
	_, err := Parse(strings.NewReader(y))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "one of run, call, or uses is required")
}
```

If `strings` is not already imported in `parse_test.go`, add it.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dsl/ -run TestParse_Finally -v`
Expected: FAIL — `Spec` has no field `Finally` (compile error), or finally not validated.

- [ ] **Step 3: Add the field**

In `internal/dsl/types.go`, add `Finally` to `Spec` (after `Steps`):

```go
type Spec struct {
	Params         Params       `yaml:"params"`
	Concurrency    *Concurrency `yaml:"concurrency,omitempty"`
	AgentSelector  []string     `yaml:"agentSelector,omitempty"`
	Steps          []StepEntry  `yaml:"steps"`
	// Finally runs after the main DAG completes, on success, failure, or
	// cancellation. Same structure as Steps. A finally step's `if:` defaults to
	// always-run; use if: failure()/success() to filter. A finally step that
	// fails marks the run Failed (after all finally steps run).
	Finally        []StepEntry  `yaml:"finally,omitempty"`
	TimeoutMinutes float64      `yaml:"timeoutMinutes,omitempty"`
	PodTemplate    *PodTemplate `yaml:"podTemplate,omitempty"`
}
```

- [ ] **Step 4: Extract a step-list validator and validate `finally`**

In `internal/dsl/parse.go`, extract the existing per-entry validation loop (currently inside `Validate`, the `for i, entry := range j.Spec.Steps` block at lines 94–131) into a helper, then call it for both `steps` and `finally`:

```go
// validateStepEntries validates a list of StepEntry (steps or finally),
// accumulating step names into nameSet for duplicate detection across the
// whole job. pathPrefix is "spec.steps" or "spec.finally".
func validateStepEntries(entries []StepEntry, pathPrefix string, nameSet map[string]bool) error {
	for i, entry := range entries {
		if len(entry.Parallel) > 0 {
			if entry.Name != "" || entry.Run != "" || entry.Call != nil || entry.Uses != nil {
				return fmt.Errorf("%s[%d]: parallel: block must not have name, run, call, or uses fields", pathPrefix, i)
			}
			for j2, st := range entry.Parallel {
				if err := validateStepFull(st.Name, st.Run, st.Call, st.Uses, st.Cache, st.Foreach, fmt.Sprintf("%s[%d].parallel[%d]", pathPrefix, i, j2), nameSet); err != nil {
					return err
				}
				if err := validateCacheStep(st.Name, st.Cache); err != nil {
					return err
				}
				if err := validateUsesStep(st.Name, st.Uses, st.Call); err != nil {
					return err
				}
				if st.Post != nil && st.Post.Run == "" {
					return fmt.Errorf("step %q: post.run is required when post is specified", st.Name)
				}
			}
		} else {
			if entry.Name == "" {
				return fmt.Errorf("%s[%d]: name is required (or use parallel: for a parallel block)", pathPrefix, i)
			}
			if err := validateStepFull(entry.Name, entry.Run, entry.Call, entry.Uses, entry.Cache, entry.Foreach, fmt.Sprintf("%s[%d]", pathPrefix, i), nameSet); err != nil {
				return err
			}
			if err := validateCacheStep(entry.Name, entry.Cache); err != nil {
				return err
			}
			if err := validateUsesStep(entry.Name, entry.Uses, entry.Call); err != nil {
				return err
			}
			if entry.Post != nil && entry.Post.Run == "" {
				return fmt.Errorf("step %q: post.run is required when post is specified", entry.Name)
			}
		}
	}
	return nil
}
```

Then replace the inline loop in `Validate` (lines 91–131) with:

```go
	// Collect step names for duplicate detection across steps and finally.
	nameSet := map[string]bool{}
	if err := validateStepEntries(j.Spec.Steps, "spec.steps", nameSet); err != nil {
		return err
	}
	if err := validateStepEntries(j.Spec.Finally, "spec.finally", nameSet); err != nil {
		return err
	}
```

(`spec.steps` must still contain at least one step — keep the existing check at lines 87–89. `finally` may be empty.)

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/dsl/ -run 'TestParse' -v`
Expected: PASS (new finally tests pass; existing parse tests still pass).

- [ ] **Step 6: Commit**

```bash
git add internal/dsl/types.go internal/dsl/parse.go internal/dsl/parse_test.go
git commit -m "feat(dsl): add spec.finally block with validation"
```

---

## Task 2: DSL — status condition functions and implicit `success()`

**Files:**
- Modify: `internal/dsl/condition.go` (rewrite `EvalCondition`)
- Test: `internal/dsl/condition_test.go`

**Interfaces:**
- Produces:
  - `type RunStatusView struct { Failed bool; Cancelled bool }`
  - `func EvalCondition(expr string, data TemplateData, status RunStatusView, implicitSuccess bool) (bool, error)`
  - CEL functions in `if:`: `failure()` → `status.Failed`; `success()` → `!Failed && !Cancelled`; `always()` → `true`.
  - When `implicitSuccess` is true and `expr` contains no status-function call, the boolean result is ANDed with `success()`. Empty `expr` → `success()` when `implicitSuccess`, else `true`.
- Consumes: nothing new (cel-go already a dependency).

- [ ] **Step 1: Write the failing tests**

First, update the **9 existing** calls in `internal/dsl/condition_test.go` to the new signature by appending `, RunStatusView{}, true` to each `EvalCondition(...)` call. For example:

```go
ok, err := EvalCondition("", TemplateData{}, RunStatusView{}, true)
// ...
ok, err := EvalCondition(`params.env == "production"`, data, RunStatusView{}, true)
```

(Apply the same `, RunStatusView{}, true` addition to all nine existing tests: `Empty`, `LiteralTrue`, `LiteralFalse`, `ParamsTrue`, `ParamsFalse`, `LogicalAnd`, `InOperator`, `InvalidExpr`, `NonBoolResult`, `StepOutputs`.)

Then append the new behavior tests:

```go
func TestEvalCondition_StatusFunctions(t *testing.T) {
	cases := []struct {
		name           string
		expr           string
		status         RunStatusView
		implicitSucc   bool
		want           bool
	}{
		// failure()
		{"failure_when_failed", "failure()", RunStatusView{Failed: true}, true, true},
		{"failure_when_ok", "failure()", RunStatusView{}, true, false},
		{"failure_when_cancelled", "failure()", RunStatusView{Cancelled: true}, true, false},
		// success()
		{"success_when_ok", "success()", RunStatusView{}, true, true},
		{"success_when_failed", "success()", RunStatusView{Failed: true}, true, false},
		{"success_when_cancelled", "success()", RunStatusView{Cancelled: true}, true, false},
		// always()
		{"always_when_failed", "always()", RunStatusView{Failed: true}, true, true},
		{"always_when_cancelled", "always()", RunStatusView{Cancelled: true}, true, true},
		// implicit success(): no-if step after a failure is skipped
		{"empty_after_failure_implicit", "", RunStatusView{Failed: true}, true, false},
		{"empty_ok_implicit", "", RunStatusView{}, true, true},
		// implicit success(): a non-status expr is ANDed with success()
		{"nonstatus_after_failure_implicit", "true", RunStatusView{Failed: true}, true, false},
		{"nonstatus_ok_implicit", "true", RunStatusView{}, true, true},
		// finally semantics: implicitSuccess=false → empty is always-run
		{"empty_finally_after_failure", "", RunStatusView{Failed: true}, false, true},
		{"nonstatus_finally_after_failure", "true", RunStatusView{Failed: true}, false, true},
		{"failure_in_finally", "failure()", RunStatusView{Failed: true}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EvalCondition(tc.expr, TemplateData{}, tc.status, tc.implicitSucc)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/dsl/ -run TestEvalCondition -v`
Expected: FAIL — `EvalCondition` signature mismatch (too many args) / `RunStatusView` undefined.

- [ ] **Step 3: Rewrite `condition.go`**

Replace the entire contents of `internal/dsl/condition.go` with:

```go
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

	if implicitSuccess && !statusFuncRe.MatchString(expr) {
		return b && successVal, nil
	}
	return b, nil
}
```

- [ ] **Step 4: Update the existing agent caller so the package compiles**

In `internal/agent/agent.go` (~line 264), update the single `EvalCondition` call to the new signature, with no behavior change for now (real status wired in Task 4):

```go
			ok, err := dsl.EvalCondition(step.If, ifData, dsl.RunStatusView{}, true)
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/dsl/ -run TestEvalCondition -v && go build ./...`
Expected: PASS, and the whole module builds.

- [ ] **Step 6: Commit**

```bash
git add internal/dsl/condition.go internal/dsl/condition_test.go internal/agent/agent.go
git commit -m "feat(dsl): failure()/success()/always() if: functions with implicit success()"
```

---

## Task 3: API + controller — compile the `finally` block

**Files:**
- Modify: `internal/api/types.go` (`ClaimResponse`)
- Modify: `internal/controller/api_agent.go` (`buildClaimResponse`)
- Test: `internal/controller/api_agent_test.go` (create if absent)

**Interfaces:**
- Produces: `api.ClaimResponse.Finally []ClaimStage`; helper `buildStages(entries []dsl.StepEntry, stepIdx *int, secretsNeeded map[string]struct{}) []api.ClaimStage`.
- Consumes: `dsl.Spec.Finally` (Task 1).

- [ ] **Step 1: Write the failing test**

Create/append `internal/controller/api_agent_test.go`:

```go
package controller

import (
	"encoding/json"
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildClaimResponse_Finally(t *testing.T) {
	spec := dsl.Spec{
		Steps: []dsl.StepEntry{
			{Name: "build", Run: "make build"},
		},
		Finally: []dsl.StepEntry{
			{Name: "notify", Run: "./notify.sh {{ secrets.HOOK }}"},
			{Name: "rollback", If: "failure()", Run: "./rollback.sh"},
		},
	}
	raw, err := json.Marshal(spec)
	require.NoError(t, err)

	resp, err := buildClaimResponse(&store.ClaimedRun{ID: "run1", JobName: "j", Spec: raw})
	require.NoError(t, err)

	require.Len(t, resp.Stages, 1)
	require.Len(t, resp.Finally, 2)
	// Flat step indices continue across steps -> finally.
	assert.Equal(t, 0, resp.Stages[0].Step.Index)
	assert.Equal(t, 1, resp.Finally[0].Step.Index)
	assert.Equal(t, 2, resp.Finally[1].Step.Index)
	assert.Equal(t, "failure()", resp.Finally[1].Step.If)
	// Secrets referenced only in finally are still collected.
	assert.Contains(t, resp.SecretsNeeded, "HOOK")
}
```

(If `store.ClaimedRun` has additional required fields for `buildClaimResponse`, set only those the function reads: `ID`, `JobName`, `Spec`, `Params`. Check `internal/store` for the exact struct.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestBuildClaimResponse_Finally -v`
Expected: FAIL — `resp.Finally` undefined / empty.

- [ ] **Step 3: Add the API field**

In `internal/api/types.go`, add to `ClaimResponse` (after `Stages`):

```go
	Stages         []ClaimStage      `json:"stages"`
	Finally        []ClaimStage      `json:"finally,omitempty"`
```

- [ ] **Step 4: Extract `buildStages` and compile `finally`**

In `internal/controller/api_agent.go`, replace the `for stageIdx, entry := range spec.Steps { ... }` block (lines 143–171) by extracting it into a helper and calling it twice:

```go
	stepIdx := 0 // flat step counter across steps and finally
	resp.Stages = buildStages(spec.Steps, &stepIdx, secretsNeeded)
	resp.Finally = buildStages(spec.Finally, &stepIdx, secretsNeeded)
```

And add the helper (place it next to `buildOneClaimStep`):

```go
// buildStages compiles a list of StepEntry into ClaimStages, advancing the
// shared flat step index and collecting referenced secret names.
func buildStages(entries []dsl.StepEntry, stepIdx *int, secretsNeeded map[string]struct{}) []api.ClaimStage {
	stages := make([]api.ClaimStage, 0, len(entries))
	for stageIdx, entry := range entries {
		if len(entry.Parallel) > 0 {
			stage := api.ClaimStage{Parallel: make([]api.ClaimStep, 0, len(entry.Parallel))}
			for _, st := range entry.Parallel {
				cs := buildOneClaimStep(*stepIdx, stageIdx, dsl.StepEntry{
					Name: st.Name, If: st.If, Env: st.Env, Run: st.Run,
					Outputs: st.Outputs, Call: st.Call, Uses: st.Uses, Cache: st.Cache,
					UploadArtifact: st.UploadArtifact, DownloadArtifact: st.DownloadArtifact,
					Post: st.Post, ContinueOnError: st.ContinueOnError, Container: st.Container,
					TimeoutMinutes: st.TimeoutMinutes, Foreach: st.Foreach,
				})
				stage.Parallel = append(stage.Parallel, cs)
				collectSecretNames(st.Run, secretsNeeded)
				for _, v := range st.Env {
					collectSecretNames(v, secretsNeeded)
				}
				*stepIdx++
			}
			stages = append(stages, stage)
		} else {
			cs := buildOneClaimStep(*stepIdx, stageIdx, entry)
			stages = append(stages, api.ClaimStage{Step: &cs})
			collectSecretNames(entry.Run, secretsNeeded)
			for _, v := range entry.Env {
				collectSecretNames(v, secretsNeeded)
			}
			*stepIdx++
		}
	}
	return stages
}
```

Update the `resp` initialiser's `Stages: make(...)` field (line 131) — it is now assigned by `buildStages`, so remove the inline `Stages` initialiser or leave it (it is overwritten). Keep `secretsNeeded` declared before the two `buildStages` calls.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/controller/ -run TestBuildClaimResponse -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/api/types.go internal/controller/api_agent.go internal/controller/api_agent_test.go
git commit -m "feat(controller): compile spec.finally into ClaimResponse.Finally"
```

---

## Task 4: Agent — failure tracking, status-aware skip (main DAG)

**Files:**
- Modify: `internal/agent/agent.go` (`executeRun` and the step-execution closure)
- Test: `internal/agent/agent_finally_test.go` (new) — main-DAG skip cases

**Interfaces:**
- Produces (within `agent` package): a reusable step-runner that evaluates `if:` against a status view and records failures into a flag instead of aborting the pipeline. After the main DAG, the run is Failed iff a non-`continueOnError` step failed while not cancelled.
- Consumes: `dsl.EvalCondition(..., status, implicitSuccess)` (Task 2), `RunPipeline` (unchanged — it still runs all stages because the step func no longer returns an error on step failure).

**Key behavior change:** the step function stops returning an error when a step fails. Instead it sets a shared `anyStepFailed` flag (respecting `continueOnError`) and reports `Failed`. Because the func returns `nil`, `RunPipeline` proceeds to later stages, where each step's `if:` (with implicit `success()`) auto-skips normal steps after a failure.

- [ ] **Step 1: Write the failing test**

Create `internal/agent/agent_finally_test.go`. Model it on the existing harness in `internal/agent/agent_test.go` (reuse its mock-controller/test-server setup helper — inspect that file for the helper name and copy its pattern). The test submits a 2-step job where step 1 fails and asserts step 2 is reported `Skipped` and the run is `Failed`:

```go
func TestExecuteRun_StepAfterFailureIsSkipped(t *testing.T) {
	// Job: step "boom" (exit 1) then step "after" (no if).
	// Expect: boom -> Failed, after -> Skipped, run -> Failed.
	steps := []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "boom", Run: "exit 1"}},
		{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "after", Run: "echo hi"}},
	}
	statuses, runStatus := runJobStages(t, steps, nil) // helper: returns per-step status map + final run status
	assert.Equal(t, "Failed", statuses["boom"])
	assert.Equal(t, "Skipped", statuses["after"])
	assert.Equal(t, "Failed", runStatus)
}

func TestExecuteRun_AlwaysStepRunsAfterFailure(t *testing.T) {
	steps := []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "boom", Run: "exit 1"}},
		{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "cleanup", If: "always()", Run: "echo bye"}},
	}
	statuses, runStatus := runJobStages(t, steps, nil)
	assert.Equal(t, "Failed", statuses["boom"])
	assert.Equal(t, "Succeeded", statuses["cleanup"])
	assert.Equal(t, "Failed", runStatus)
}
```

Write a `runJobStages(t, stages, finally)` helper in this test file that stands up the existing agent test server (copy the construction from `agent_test.go`), serves a `ClaimResponse{Stages: stages, Finally: finally}`, records every `StepReportRequest` status by step name, captures the final `FinishRun` status, and runs one claim. (The `finally` argument is unused until Task 5 — pass `nil` here.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agent/ -run 'TestExecuteRun_StepAfterFailure|TestExecuteRun_AlwaysStep' -v`
Expected: FAIL — today `after` is never reported (pipeline aborts), so `statuses["after"]` is empty (not `Skipped`); `always()` step likewise does not run.

- [ ] **Step 3: Add failure tracking and a status view in `executeRun`**

In `internal/agent/agent.go`, inside `executeRun`, after `var cancelledByMaster atomic.Bool` (line 202) add:

```go
	var anyStepFailed atomic.Bool // a non-continueOnError step failed (used for if: status)

	statusView := func() dsl.RunStatusView {
		cancelled := cancelledByMaster.Load()
		return dsl.RunStatusView{
			Failed:    anyStepFailed.Load() && !cancelled,
			Cancelled: cancelled,
		}
	}
```

- [ ] **Step 4: Use the status view for `if:` and stop aborting on failure**

In the step closure passed to `RunPipeline` (starts at line 260):

(a) Replace the `if:` evaluation (line 264) to pass the live status with implicit `success()`:

```go
			ok, err := dsl.EvalCondition(step.If, ifData, statusView(), true)
```

(b) Add a local helper at the top of the closure body (before the cache/artifact branches) to record a failure and report it, then continue:

```go
		markFailed := func(reportCtx context.Context) {
			if !step.ContinueOnError && !cancelledByMaster.Load() {
				anyStepFailed.Store(true)
			}
			_ = a.Client.ReportStep(reportCtx, a.ID, api.StepReportRequest{
				RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex,
				StepName: step.Name, Status: "Failed", EndedAt: time.Now().UTC(),
			})
		}
```

(c) Wrap the cache/artifact branches (lines 287–295) so an internal error records a failure instead of returning an error:

```go
		if step.Cache != nil {
			if err := a.executeCacheStep(stepCtx, step, c.RunID, sctx, &postHooks); err != nil {
				slog.Error("cache step failed", "step", step.Name, "error", err)
				markFailed(context.WithoutCancel(stepCtx))
			}
			return nil
		}
		if step.UploadArtifact != nil {
			if err := a.executeUploadArtifact(stepCtx, step, c.RunID); err != nil {
				slog.Error("upload artifact failed", "step", step.Name, "error", err)
				markFailed(context.WithoutCancel(stepCtx))
			}
			return nil
		}
		if step.DownloadArtifact != nil {
			if err := a.executeDownloadArtifact(stepCtx, step, c.RunID); err != nil {
				slog.Error("download artifact failed", "step", step.Name, "error", err)
				markFailed(context.WithoutCancel(stepCtx))
			}
			return nil
		}
```

(d) Replace the run/call failure terminus (lines 400–402) so it records the failure and returns `nil` rather than an error:

```go
		if status == "Failed" {
			if !step.ContinueOnError && !cancelledByMaster.Load() {
				anyStepFailed.Store(true)
			}
			return nil
		}
		return nil
```

(The existing `ReportStep` with `Status: status` at lines 387–399 already reports the `Failed` status for the run/call path, so `markFailed` is only used by the cache/artifact branches, which otherwise would not report `Failed`.)

- [ ] **Step 5: Update the final-status computation**

Replace the `overallStatus` switch (lines 424–432) with a failed-first precedence sourced from the flag (structural `dagErr` — e.g. foreach expansion — also counts as failure when not cancelled):

```go
	cancelled := cancelledByMaster.Load()
	mainFailed := anyStepFailed.Load() || (dagErr != nil && !cancelled)

	var overallStatus api.RunStatus
	switch {
	case mainFailed:
		overallStatus = api.RunFailed
	case cancelled:
		overallStatus = api.RunCancelled
	default:
		overallStatus = api.RunSucceeded
	}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/agent/ -run 'TestExecuteRun_StepAfterFailure|TestExecuteRun_AlwaysStep' -v`
Expected: PASS.

- [ ] **Step 7: Run the full agent + pipeline suite for regressions**

Run: `go test ./internal/agent/... -v`
Expected: PASS. Note: `pipeline_test.go` is unaffected — its bare callbacks still return errors, and `RunPipeline` still aborts when the callback returns an error; only the production step func changed (it now returns `nil` on failure). If any existing agent test asserted that a step after a failure never reported a status, update it to expect `Skipped`.

- [ ] **Step 8: Commit**

```bash
git add internal/agent/agent.go internal/agent/agent_finally_test.go
git commit -m "feat(agent): status-aware if: skip; record failures without aborting the DAG"
```

---

## Task 5: Agent — run the `finally` block and final-status precedence

**Files:**
- Modify: `internal/agent/agent.go` (`executeRun`: extract a reusable step runner; run `finally`)
- Test: `internal/agent/agent_finally_test.go` (append finally cases)

**Interfaces:**
- Consumes: `c.Finally []api.ClaimStage` (Task 3), `statusView`/`anyStepFailed` (Task 4), `dsl.EvalCondition(..., status, implicitSuccess=false)` for finally.
- Produces: finally execution after the main DAG with a frozen main status; `finallyFailed` flips the run to `Failed`.

**Refactor note:** the step-execution closure (lines 260–404) must be reused for finally with two differences: it evaluates `if:` against a **frozen** status (not the live one) with `implicitSuccess=false`, and it records failures into a **separate** flag. Extract the closure into a factory.

- [ ] **Step 1: Write the failing tests**

Append to `internal/agent/agent_finally_test.go`:

```go
func TestExecuteRun_FinallyRunsOnFailure(t *testing.T) {
	stages := []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "boom", Run: "exit 1"}},
	}
	finally := []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 1, StageIndex: 0, Name: "notify", Run: "echo notify"}},
		{Step: &api.ClaimStep{Index: 2, StageIndex: 1, Name: "rollback", If: "failure()", Run: "echo rb"}},
		{Step: &api.ClaimStep{Index: 3, StageIndex: 2, Name: "only-success", If: "success()", Run: "echo no"}},
	}
	statuses, runStatus := runJobStages(t, stages, finally)
	assert.Equal(t, "Succeeded", statuses["notify"], "no-if finally step always runs")
	assert.Equal(t, "Succeeded", statuses["rollback"], "failure() runs on failure")
	assert.Equal(t, "Skipped", statuses["only-success"], "success() skips on failure")
	assert.Equal(t, "Failed", runStatus)
}

func TestExecuteRun_FinallyStepFailureMarksRunFailed(t *testing.T) {
	stages := []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "ok", Run: "echo ok"}},
	}
	finally := []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 1, StageIndex: 0, Name: "cleanup-boom", Run: "exit 1"}},
		{Step: &api.ClaimStep{Index: 2, StageIndex: 1, Name: "cleanup-after", Run: "echo still"}},
	}
	statuses, runStatus := runJobStages(t, stages, finally)
	assert.Equal(t, "Failed", statuses["cleanup-boom"])
	assert.Equal(t, "Succeeded", statuses["cleanup-after"], "finally runs all steps to completion")
	assert.Equal(t, "Failed", runStatus, "a finally step failure fails the run")
}
```

(A cancellation case is best covered by extending the existing cancel test in `agent_cancel_test.go` to include a `finally` step and assert it still runs and that `failure()` is false; add it if the harness there supports injecting `Finally`.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agent/ -run TestExecuteRun_Finally -v`
Expected: FAIL — finally is never executed yet.

- [ ] **Step 3: Extract the step runner into a factory**

In `internal/agent/agent.go`, refactor the closure currently passed inline to `RunPipeline` (lines 260–404) into a factory so it can be reused. Define it inside `executeRun` (it captures `a`, `c`, `sctx`, `masker`, `workDir`, `postHooks`, `hookStack`):

```go
	// makeStepRunner builds the per-step execution function.
	//   statusFn        — supplies the RunStatusView used to evaluate if:
	//   implicitSuccess — true for the main DAG, false for finally
	//   failedFlag      — set when a non-continueOnError step fails
	makeStepRunner := func(statusFn func() dsl.RunStatusView, implicitSuccess bool, failedFlag *atomic.Bool) func(context.Context, api.ClaimStep) error {
		return func(stepCtx context.Context, step api.ClaimStep) error {
			// ... existing body from lines 261–403, with these edits:
			//  - if: uses dsl.EvalCondition(step.If, ifData, statusFn(), implicitSuccess)
			//  - markFailed / the run-failure terminus set failedFlag (not anyStepFailed directly)
			//    respecting step.ContinueOnError && !cancelledByMaster.Load()
		}
	}
```

Move the existing closure body verbatim into the factory, applying exactly the edits from Task 4 steps 4(a)–(d) but referencing the parameters: `statusFn()` for the status, `implicitSuccess` for the flag, and `failedFlag.Store(true)` in place of `anyStepFailed.Store(true)`.

Then build the main runner from the factory:

```go
	mainRunner := makeStepRunner(statusView, true, &anyStepFailed)
	dagErr := RunPipeline(runCtx, c.Stages, getData, mainRunner)
```

- [ ] **Step 4: Run the finally block after the main DAG**

After the post-hooks loop (after line 422) and before computing `overallStatus`, add:

```go
	// Freeze the main-DAG status for finally if: evaluation.
	cancelled := cancelledByMaster.Load()
	mainFailed := anyStepFailed.Load() || (dagErr != nil && !cancelled)

	var finallyFailed atomic.Bool
	if len(c.Finally) > 0 {
		frozen := dsl.RunStatusView{Failed: mainFailed, Cancelled: cancelled}
		finallyStatus := func() dsl.RunStatusView { return frozen }
		finallyRunner := makeStepRunner(finallyStatus, false, &finallyFailed)
		// Use a non-cancelling context so finally runs even when the run was cancelled.
		finallyCtx := context.WithoutCancel(ctx)
		if err := RunPipeline(finallyCtx, c.Finally, getData, finallyRunner); err != nil {
			slog.Warn("finally: structural error", "runId", c.RunID, "error", err)
			finallyFailed.Store(true)
		}
	}
```

Note: finally steps do not auto-skip one another because `finallyStatus` is frozen (constant) — a `finally` step failing does not change the status seen by sibling `finally` steps. The per-step `markFailed` inside the runner must guard with `!cancelledByMaster.Load()`; for finally, prefer guarding the `failedFlag.Store` on `!step.ContinueOnError` only (cancellation does not suppress a genuine finally failure). Implement the finally failure recording as: `if !step.ContinueOnError { finallyFailed.Store(true) }`. To keep one code path, have `markFailed`/the terminus consult a captured `suppressOnCancel bool` (true for main, false for finally) passed into `makeStepRunner`; add that parameter.

Concretely, extend the factory signature:

```go
	makeStepRunner := func(statusFn func() dsl.RunStatusView, implicitSuccess bool, failedFlag *atomic.Bool, suppressOnCancel bool) func(context.Context, api.ClaimStep) error {
```

and the failure recording becomes:

```go
		recordFailure := func() {
			if step.ContinueOnError {
				return
			}
			if suppressOnCancel && cancelledByMaster.Load() {
				return
			}
			failedFlag.Store(true)
		}
```

Use `recordFailure()` in `markFailed` and at the run/call failure terminus. Build runners as:

```go
	mainRunner := makeStepRunner(statusView, true, &anyStepFailed, true)
	// ...
	finallyRunner := makeStepRunner(finallyStatus, false, &finallyFailed, false)
```

- [ ] **Step 5: Final status includes finally**

Replace the `overallStatus` switch from Task 4 (which used `mainFailed`/`cancelled`) with one that also considers `finallyFailed`:

```go
	var overallStatus api.RunStatus
	switch {
	case mainFailed || finallyFailed.Load():
		overallStatus = api.RunFailed
	case cancelled:
		overallStatus = api.RunCancelled
	default:
		overallStatus = api.RunSucceeded
	}
```

(Remove the now-duplicated `cancelled`/`mainFailed` declarations added in Task 4 step 5 — they now live just before the finally block. Ensure each is declared once.)

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/agent/ -run TestExecuteRun -v`
Expected: PASS (all finally cases + Task 4 cases).

- [ ] **Step 7: Full suite**

Run: `go test ./... -short`
Expected: PASS. (Use `make test-short` to skip Docker integration tests.)

- [ ] **Step 8: Commit**

```bash
git add internal/agent/agent.go internal/agent/agent_finally_test.go internal/agent/agent_cancel_test.go
git commit -m "feat(agent): execute finally block after the main DAG with frozen status"
```

---

## Task 6: Docs and generated schema

**Files:**
- Modify: `docs/jobs.md` (new `finally` + status-functions sections)
- Regenerate: `schemas/unified-cd.schema.json`, `docs/field-reference.md`

**Interfaces:** none (documentation + generated artifacts).

- [ ] **Step 1: Regenerate schema and field reference**

The `Finally` field (Task 1) and any doc comments are picked up by the generators (they parse `internal/dsl/types.go`).

Run: `go generate ./internal/dsl/`
Expected: `schemas/unified-cd.schema.json` and `docs/field-reference.md` updated to include `finally`.

Verify: `git diff --stat schemas/ docs/field-reference.md` shows changes referencing `finally`.

- [ ] **Step 2: Document the feature in `docs/jobs.md`**

Add two sections. Under the table of contents and body, add a `finally` section and an `if:` status-functions subsection:

````markdown
## Finally Block (`finally`)

Steps under `spec.finally` run **after the main `steps` DAG completes** —
whether it succeeded, failed, or was cancelled. Use it for notifications,
cleanup, or rollback.

```yaml
spec:
  steps:
    - name: deploy
      run: ./deploy.sh
  finally:
    - name: notify          # no if: → always runs
      run: ./notify.sh "{{ .Params.env }}"
    - name: rollback
      if: failure()         # only when a step failed
      run: ./rollback.sh
```

- `finally` uses the same structure as `steps` (stages + `parallel`).
- A `finally` step with no `if:` always runs.
- All `finally` steps run to completion; a `finally` step that fails marks the
  run **Failed**.
- On cancellation, `finally` still runs, but `failure()` is `false`.

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
````

(Also add `- [Finally Block](#finally-block-finally)` and `- [Status Functions in if](#status-functions-in-if)` entries to the Table of Contents at the top of `docs/jobs.md`.)

- [ ] **Step 3: Verify the schema is valid JSON and mentions finally**

Run: `python -c "import json;json.load(open('schemas/unified-cd.schema.json'))" && grep -c finally schemas/unified-cd.schema.json`
Expected: no JSON error; grep count ≥ 1.

- [ ] **Step 4: Commit**

```bash
git add docs/jobs.md docs/field-reference.md schemas/unified-cd.schema.json
git commit -m "docs: document finally block and if: status functions; regenerate schema"
```

---

## Final verification

- [ ] Run the full suite: `make test-short` (or `go test ./... -short`) — all green.
- [ ] Build all binaries: `make build` — succeeds.
- [ ] Manual smoke (optional): apply a job with a failing step and a `finally: notify` step; confirm `notify` runs and the run is `Failed`.

---

## Self-review notes (coverage of the spec)

- Spec §① `finally` field + validation → Task 1.
- Spec §② status functions + implicit `success()` (incl. cancel → `failure()` false) → Task 2 (truth table) + Task 4 (status view computes `Failed && !Cancelled`).
- Spec §③ `ClaimResponse.Finally` + flat indices → Task 3.
- Spec §④ `buildStages` helper + secrets from finally → Task 3.
- Spec §⑤ failure tracking, non-aborting pipeline, status-aware skip, frozen finally status, no fail-fast inside finally, status precedence `Failed > Cancelled > Succeeded` → Tasks 4 + 5.
- Spec §⑥ tests → Tasks 1–5 (parse, condition truth table, controller compile, agent skip/finally/cancel).
- Spec §⑦ docs + regenerated schema/field-reference → Task 6.
