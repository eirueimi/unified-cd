# Step-Level Automatic Retry Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a `run:` step declare `retry: {attempts, backoff}` so the agent re-runs it on failure, up to `attempts` total tries, with a fixed `backoff` wait between tries.

**Architecture:** A new `dsl.RetrySpec` on `Step`/`StepEntry` is validated at apply time (run-steps only), carried verbatim onto `api.ClaimStep`, and executed by the agent orchestrator: the existing single-pass run/scope/container execution block is wrapped in a retry loop with per-attempt timeout, a stderr separator between tries, and a ctx-aware backoff sleep.

**Tech Stack:** Go (internal/dsl, internal/api, internal/controller, internal/agent).

**Spec:** docs/superpowers/specs/2026-07-14-step-retry-design.md — read it first; it is the authority on semantics.

## Global Constraints

- `retry: {attempts int, backoff string}`. `attempts` must be `>= 1` (1 = no retry). `backoff` is a Go duration string (`"30s"`); empty = `0` (immediate). Both are validation errors when malformed.
- `retry:` is valid ONLY on a `run:` step (a step with `Run != ""`, which covers plain run, `runsIn.image`/scope, and `container:` steps). `retry:` on any non-run step (call/uses/cache/uploadArtifact/downloadArtifact/approval) is a validation error at apply time.
- Retry re-runs on ANY failure: non-zero exit, exec/infra error (`runErr`), or a per-try timeout. A master/user cancellation (`cancelledByMaster`) is NEVER retried — the loop breaks and the step reports `Cancelled`.
- `timeoutMinutes` is applied PER ATTEMPT (each try gets its own timeout). `continueOnError` applies only AFTER attempts are exhausted (it wraps the retry).
- All attempts stream to the same step log; a stderr separator line marks each retry. Output-template capture and step outputs use the final successful attempt.
- No DB/schema change and no UI change in this plan. Retries are visible via the log separators. (The `attempts`-count badge in the run detail UI, and persisting the attempt count on the step report, are a deferred follow-up — they need a `step_reports` migration and are not required for the feature to function; see the spec's "UI (optional, small)".)
- The repo builds in module mode; local runs may need `GOFLAGS=-mod=mod` but must NOT commit go.mod/go.sum. English prose. Per-task gates: `go build ./...`, `go vet ./...`, named test packages `-count=1`.

---

### Task 1: DSL `RetrySpec` + validation

**Files:**
- Modify: `internal/dsl/types.go` (add `RetrySpec` type; add `Retry *RetrySpec` to `Step` and `StepEntry`)
- Modify: `internal/dsl/parse.go` (validate retry in `validateStepEntries`)
- Test: `internal/dsl/parse_test.go` (or the step-validation test file — grep `validateStepEntries`/`func TestValidate` in `internal/dsl/*_test.go`)

**Interfaces:**
- Produces: `dsl.RetrySpec{Attempts int, Backoff string}`; `dsl.Step.Retry *RetrySpec`; `dsl.StepEntry.Retry *RetrySpec`; a validation helper `validateRetry(name, path string, retry *RetrySpec, isRunStep bool) error`.

- [ ] **Step 1: Write the failing tests**

Add to the dsl validation test file (mirror an existing `Job.Validate()` error-case test — grep `Validate()` there):

```go
func TestValidate_Retry_OnRunStep_OK(t *testing.T) {
	j := mustParseJob(t, `apiVersion: unified-cd/v1
kind: Job
metadata: {name: j}
spec:
  steps:
    - name: flaky
      run: "true"
      retry: {attempts: 3, backoff: 30s}`)
	require.NoError(t, j.Validate())
}

func TestValidate_Retry_AttemptsMustBePositive(t *testing.T) {
	_, err := dsl.Parse(strings.NewReader(`apiVersion: unified-cd/v1
kind: Job
metadata: {name: j}
spec:
  steps:
    - {name: s, run: "true", retry: {attempts: 0}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "retry.attempts")
}

func TestValidate_Retry_BadBackoff(t *testing.T) {
	_, err := dsl.Parse(strings.NewReader(`apiVersion: unified-cd/v1
kind: Job
metadata: {name: j}
spec:
  steps:
    - {name: s, run: "true", retry: {attempts: 2, backoff: "nope"}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "retry.backoff")
}

func TestValidate_Retry_OnNonRunStep_Errors(t *testing.T) {
	_, err := dsl.Parse(strings.NewReader(`apiVersion: unified-cd/v1
kind: Job
metadata: {name: j}
spec:
  steps:
    - name: c
      cache: {path: /tmp, key: k}
      retry: {attempts: 2}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "retry:")
}
```

If a `mustParseJob` helper does not exist, inline `dsl.Parse(strings.NewReader(...))` + `require.NoError` and call `.Validate()` — check whether `dsl.Parse` already calls `Validate` internally (grep `func Parse` in parse.go: it returns `(*Job, error)` and validates; if so, a parse error already covers validation and the OK case just needs `require.NoError` on Parse).

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/dsl/ -run 'Validate_Retry' -count=1`
Expected: compile failure (`Retry` field undefined).

- [ ] **Step 3: Add the `RetrySpec` type + fields (types.go)**

Add the type near the other step-related types (e.g. after `CacheStep`/`PostStep`):

```go
// RetrySpec configures automatic re-runs of a failing run: step.
type RetrySpec struct {
	// Attempts is the total number of tries (1 = no retry). Must be >= 1.
	Attempts int `yaml:"attempts" json:"attempts"`
	// Backoff is a fixed wait between tries as a Go duration (e.g. "30s").
	// Empty means 0 (immediate retry).
	Backoff string `yaml:"backoff,omitempty" json:"backoff,omitempty"`
}
```

Add `Retry *RetrySpec` to BOTH `Step` and `StepEntry` (place it next to `TimeoutMinutes` in each struct):

```go
	Retry          *RetrySpec  `yaml:"retry,omitempty" json:"retry,omitempty"`
```

- [ ] **Step 4: Add validation (parse.go)**

Add the helper (place it near `validateStepFull`):

```go
// validateRetry checks a step's retry: block. retry is only valid on a run:
// step (Run != ""); attempts must be >= 1 and backoff must parse as a duration.
func validateRetry(name, path string, retry *RetrySpec, isRunStep bool) error {
	if retry == nil {
		return nil
	}
	if !isRunStep {
		return fmt.Errorf("%s (%s): retry: is only valid on a run: step", path, name)
	}
	if retry.Attempts < 1 {
		return fmt.Errorf("%s (%s): retry.attempts must be >= 1 (1 = no retry)", path, name)
	}
	if retry.Backoff != "" {
		if _, err := time.ParseDuration(retry.Backoff); err != nil {
			return fmt.Errorf("%s (%s): retry.backoff %q is not a valid duration: %w", path, name, retry.Backoff, err)
		}
	}
	return nil
}
```

Ensure `time` is imported in parse.go (add it if missing). Call `validateRetry` in `validateStepEntries` for BOTH the parallel sub-step path (near the `checkStepExecTarget(st...)` call ~line 270) and the entry path (near `checkStepExecTarget(entry...)` ~line 313):

```go
			// (parallel sub-step st)
			if err := validateRetry(st.Name, subPath, st.Retry, st.Run != ""); err != nil {
				return err
			}
```
```go
			// (entry)
			if err := validateRetry(entry.Name, entryPath, entry.Retry, entry.Run != ""); err != nil {
				return err
			}
```

- [ ] **Step 5: Run tests + build**

Run: `go build ./... && go test ./internal/dsl/ -run 'Validate_Retry' -count=1`
Expected: build clean, all PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/dsl/
git commit -m "feat(dsl): add step retry: {attempts, backoff} with validation (run steps only)"
```

---

### Task 2: Carry `retry` onto `api.ClaimStep`

**Files:**
- Modify: `internal/api/types.go` (add `Retry *dsl.RetrySpec` to `ClaimStep`)
- Modify: `internal/controller/api_agent.go` (`buildOneClaimStep` copies `entry.Retry`; confirm `stepToStepEntry` carries `Retry` for parallel sub-steps)
- Test: `internal/controller/api_agent_test.go` (or where `buildOneClaimStep`/`buildStages` is tested — grep `buildOneClaimStep`)

**Interfaces:**
- Consumes: `dsl.RetrySpec` (Task 1).
- Produces: `api.ClaimStep.Retry *dsl.RetrySpec` (populated by the controller, consumed by the agent in Task 3).

- [ ] **Step 1: Write the failing test**

Add to the controller test file (mirror an existing `buildOneClaimStep` field-mapping assertion — grep `buildOneClaimStep(` in `internal/controller/*_test.go`):

```go
func TestBuildOneClaimStep_CarriesRetry(t *testing.T) {
	entry := dsl.StepEntry{Name: "flaky", Run: "true", Retry: &dsl.RetrySpec{Attempts: 3, Backoff: "30s"}}
	cs := buildOneClaimStep(0, 0, entry, nil)
	require.NotNil(t, cs.Retry)
	assert.Equal(t, 3, cs.Retry.Attempts)
	assert.Equal(t, "30s", cs.Retry.Backoff)
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/controller/ -run 'BuildOneClaimStep_CarriesRetry' -count=1`
Expected: compile failure (`ClaimStep` has no `Retry`).

- [ ] **Step 3: Add the field to `api.ClaimStep` (types.go)**

`api.ClaimStep` already carries `Cache *dsl.CacheStep`, so the `dsl` import exists. Add, next to `TimeoutMinutes`:

```go
	Retry *dsl.RetrySpec `json:"retry,omitempty"`
```

- [ ] **Step 4: Copy it in `buildOneClaimStep` (api_agent.go)**

In the `cs := api.ClaimStep{...}` literal, add `Retry: entry.Retry,` (next to `TimeoutMinutes: entry.TimeoutMinutes,`).

Then confirm `stepToStepEntry` (the function that converts a `dsl.Step` in a `parallel:` block to a `dsl.StepEntry` before `buildOneClaimStep`) copies `Retry` — grep `func stepToStepEntry`; it lists fields like `Post: st.Post, ContinueOnError: st.ContinueOnError, ...`. Add `Retry: st.Retry,` to that literal so a `retry:` on a parallel sub-step is not dropped.

- [ ] **Step 5: Run test + build**

Run: `go build ./... && go test ./internal/controller/ -run 'BuildOneClaimStep_CarriesRetry' -count=1`
Expected: build clean, PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/api/types.go internal/controller/api_agent.go
git commit -m "feat(api,controller): carry step retry onto ClaimStep"
```

---

### Task 3: Orchestrator retry loop

**Files:**
- Modify: `internal/agent/orchestrator.go` (the step-level timeout application ~line 250; the run/scope/container execution block ~lines 368-450 inside `makeStepRunner`)
- Test: `internal/agent/orchestrator_retry_test.go` (new) — drive `makeStepRunner`/`RunPipeline` against a fake `ExecBackend`, or grep for the existing orchestrator test harness (`func TestRunPipeline`/a fake backend in `internal/agent/*_test.go`) and mirror it.

**Interfaces:**
- Consumes: `api.ClaimStep.Retry *dsl.RetrySpec` (Task 2); the existing `b.RunDefault`/`RunInScope`/`RunNamedContainer`, `cancelledByMaster`, `step.TimeoutMinutes`, `b.StepLogWriters`.
- Produces: retry behavior; a package-level `var retrySleep = func(ctx, d) error` so tests can stub the backoff wait.

- [ ] **Step 1: Write the failing test**

Create `internal/agent/orchestrator_retry_test.go`. Use the existing orchestrator test scaffolding — find a fake `ExecBackend` in `internal/agent/*_test.go` (grep `RunDefault` in `_test.go`; the parity/backend tests define one). If none is reusable, define a minimal `retryFakeBackend` implementing `agentlib.ExecBackend` whose `RunDefault` returns a scripted sequence of `(exitCode, err)` and records call count. Test the loop via the smallest entry point that runs one step (grep how existing tests invoke a single step — likely `RunClaim`/`RunPipeline` with a one-step claim, or a direct call to the runner). Assertions:

```go
// A step that fails twice then succeeds runs exactly 3 times and ends Succeeded.
func TestRetry_FailsThenSucceeds(t *testing.T) {
	// backend.RunDefault returns (1,nil),(1,nil),(0,nil) on calls 1,2,3
	// step: Retry{Attempts:3, Backoff:"1ms"}
	// assert: RunDefault called 3 times; final reported status "Succeeded"
}

// All attempts fail → Failed, called exactly Attempts times.
func TestRetry_AllFail(t *testing.T) { /* (1,nil)x3, Attempts:3 → Failed, 3 calls */ }

// attempts:1 (or no retry) runs exactly once.
func TestRetry_NoRetryRunsOnce(t *testing.T) { /* (1,nil), no Retry → Failed, 1 call */ }

// A master cancellation between tries stops immediately (no extra attempts).
func TestRetry_CancelNotRetried(t *testing.T) { /* set cancelledByMaster before/at call 1; assert 1 call, status Cancelled */ }
```

Stub the backoff: set the package var `retrySleep` to a no-op (or record durations) in the test so it runs instantly. Capture the reported final status via the fake client's recorded `ReportStep` calls (mirror how existing orchestrator tests capture reports).

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/agent/ -run 'TestRetry_' -count=1`
Expected: FAIL (no retry loop yet — a failing step runs once).

- [ ] **Step 3: Add the backoff sleeper var + per-attempt timeout gate**

Near the top of orchestrator.go (package scope), add:

```go
// retrySleep waits d honoring ctx (a var so tests run instantly). Returns
// ctx.Err() if the wait is cancelled.
var retrySleep = func(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
```

Change the existing per-step timeout application (the `if step.TimeoutMinutes > 0 { stepCtx, stepCancel = context.WithTimeout(...) }` block, ~line 250) so it is NOT applied when the step has retry — retry applies the timeout per attempt instead:

```go
	// A retry step applies its timeout per ATTEMPT inside the run loop below,
	// so the whole-step timeout is skipped here (it would otherwise cap the
	// entire retry budget). Non-retry steps keep the single per-step timeout.
	if step.TimeoutMinutes > 0 && step.Retry == nil {
		var stepCancel context.CancelFunc
		stepCtx, stepCancel = context.WithTimeout(stepCtx, time.Duration(step.TimeoutMinutes*float64(time.Minute)))
		defer stepCancel()
	}
```

(Keep the exact existing `stepCancel`/`defer` shape; only add the `&& step.Retry == nil` condition and the comment.)

- [ ] **Step 4: Wrap the run execution block in the retry loop**

The current code (in the `else` branch, i.e. NOT a `call:` step) does one pass:
1. expand template + build `extraEnv`,
2. open `b.StepLogWriters`, run one of `RunInScope`/`RunNamedContainer`/`RunDefault` → `ec, runErr`,
3. `finishLogs`, then set `status`/`exitCode` and capture outputs on success.

Refactor steps (2) into an attempt loop. Replace the single execution + `finishLogs` + the `if runErr != nil || ec != 0` failure block with:

```go
	attempts := 1
	var backoff time.Duration
	if step.Retry != nil {
		attempts = step.Retry.Attempts
		backoff, _ = time.ParseDuration(step.Retry.Backoff) // validated at apply time; "" → 0
	}

	var ec int
	var runErr error
	var capturedStdout string
	for try := 1; try <= attempts; try++ {
		// Per-attempt timeout (retry steps only; non-retry steps use stepCtx as-is).
		attemptCtx := stepCtx
		var attemptCancel context.CancelFunc
		if step.Retry != nil && step.TimeoutMinutes > 0 {
			attemptCtx, attemptCancel = context.WithTimeout(stepCtx, time.Duration(step.TimeoutMinutes*float64(time.Minute)))
		}

		shippedStdout, shippedStderr, finishLogs := b.StepLogWriters(attemptCtx, step.Index)
		var stdoutBuf bytes.Buffer
		stdoutTee := io.MultiWriter(&stdoutBuf, shippedStdout)
		switch {
		case isScopedStep(step):
			h, herr := b.EnsureScope(attemptCtx, step, extraEnv)
			if herr != nil {
				runErr, ec = herr, -1
			} else {
				stepScope = h
				ec, runErr = b.RunInScope(attemptCtx, h, expandedRun, step.Shell, extraEnv, stdoutTee, shippedStderr)
			}
		case step.Container != "":
			ec, runErr = b.RunNamedContainer(attemptCtx, step, step.Container, expandedRun, extraEnv, stdoutTee, shippedStderr)
		default:
			ec, runErr = b.RunDefault(attemptCtx, step, expandedRun, extraEnv, stdoutTee, shippedStderr)
		}
		capturedStdout = stdoutBuf.String()

		if runErr != nil && !cancelledByMaster.Load() {
			fmt.Fprintf(shippedStderr, "unified-cd: step %q failed to execute: %v\n", step.Name, runErr)
			slog.Warn("step exec error", "run", c.RunID, "step", step.Name, "container", step.Container, "error", runErr)
		}
		finishLogs(attemptCtx)
		if attemptCancel != nil {
			attemptCancel()
		}

		if runErr == nil && ec == 0 {
			break // success
		}
		if cancelledByMaster.Load() {
			break // never retry a master/user cancellation
		}
		if try < attempts {
			// Separator on the NEXT attempt's stderr writer so it lands in the log.
			nextStdout, nextStderr, nextFinish := b.StepLogWriters(stepCtx, step.Index)
			_ = nextStdout
			fmt.Fprintf(nextStderr, "── retry %d/%d after %s (previous: exit %d) ──\n", try+1, attempts, backoff, ec)
			nextFinish(stepCtx)
			if serr := retrySleep(stepCtx, backoff); serr != nil {
				break // cancelled during backoff
			}
		}
	}
	exitCode = ec

	if runErr != nil || ec != 0 {
		status = "Failed"
		if runErr != nil && cancelledByMaster.Load() {
			status = "Cancelled"
		}
	} else {
		// ... the existing success branch (output-template capture + step
		//     outputs), unchanged, using capturedStdout ...
	}
```

Keep the EXISTING success branch body (output capture, `SetStepOutputs`, etc.) verbatim — only its surrounding single-pass execution moved into the loop, and `capturedStdout` is now the loop's last value. The `expandedRun`/`extraEnv` computation stays BEFORE the loop (unchanged). `stepScope` remains declared before the loop (set inside the scope case). Remove the now-duplicated pre-loop single execution.

- [ ] **Step 5: Run tests + build**

Run: `go build ./... && go vet ./... && go test ./internal/agent/ -run 'TestRetry_' -count=1`
Expected: build/vet clean, all PASS. Then run the whole agent suite once to catch regressions in the refactored execution block: `go test ./internal/agent/ -count=1` — expect PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/agent/orchestrator.go internal/agent/orchestrator_retry_test.go
git commit -m "feat(agent): retry a failing run step up to attempts with per-try timeout + backoff"
```

---

### Task 4: Docs

**Files:**
- Modify: `docs/jobs.md` (the step reference — grep `continueOnError`/`timeoutMinutes` to find where step fields are documented)

- [ ] **Step 1: Document `retry`**

Next to `continueOnError`/`timeoutMinutes`, document `retry: {attempts, backoff}`: a run step re-runs on failure up to `attempts` total tries (1 = no retry), waiting `backoff` (a duration, default 0) between tries. State the semantics: any failure (non-zero exit, exec error, or a per-attempt timeout) is retried; a run cancellation is not; `timeoutMinutes` bounds each attempt; `continueOnError` applies after attempts are exhausted; `retry` is only valid on `run:` steps. Add a short example:

```yaml
- name: flaky-integration-test
  run: go test ./it/...
  timeoutMinutes: 5     # bounds EACH attempt
  retry:
    attempts: 3
    backoff: 30s
```

- [ ] **Step 2: Confirm nothing regressed; commit**

Run: `go test ./internal/dsl/ -count=1`
Expected: PASS.

```bash
git add docs/jobs.md
git commit -m "docs: document step retry: {attempts, backoff}"
```

## Self-Review

**Spec coverage:** DSL `retry` + validation (run-steps-only, attempts>=1, backoff parse) → T1; wire onto ClaimStep → T2; orchestrator retry loop with per-attempt timeout, cancel-not-retried, ctx-aware backoff, log separators, success-only output capture → T3; docs → T4. The spec's "UI (optional, small)" attempts badge + `StepReport.Attempts` persistence are explicitly deferred (Global Constraints) — they require a `step_reports` migration and are not needed for the functional feature (retries are visible via the T3 log separators). Flag this deferral to the human at execution handoff.

**Placeholder scan:** T3 Step 4's success branch says "the existing success branch body ... verbatim" — that is a direct instruction to preserve concrete existing code the implementer is reading in-place (a refactor-in-place, not an invented placeholder); the full loop structure around it is shown. T1 Step 1 test helper `mustParseJob` is guarded with a fallback to inline `dsl.Parse`. All new code (RetrySpec, validateRetry, the retry loop, retrySleep) is shown in full.

**Type consistency:** `dsl.RetrySpec{Attempts int, Backoff string}` used identically in T1 (definition), T2 (`api.ClaimStep.Retry *dsl.RetrySpec`, `buildOneClaimStep`/`stepToStepEntry` copies), and T3 (`step.Retry.Attempts`/`.Backoff`, parsed via `time.ParseDuration`). `retrySleep(ctx, d) error` defined and used in T3. The per-attempt timeout gate (`step.Retry == nil`) in T3 Step 3 matches the per-attempt timeout application in T3 Step 4.
