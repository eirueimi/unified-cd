# k8s-agent `finally` / `if:` Parity — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bring the Kubernetes agent (`internal/k8sagent`) to parity with the standard agent for `if:` condition evaluation (including the `failure()`/`success()`/`always()` status functions) and the job-level `finally` block.

**Architecture:** Split `executeRun` into pod-lifecycle (create/wait/delete pod — untestable without a cluster) and a pure `orchestrate(ctx, c, stepExec)` core (stage iteration, `if:` evaluation, status tracking, `finally`, status reporting) that takes the pod-exec as an injected callback. This makes the orchestration unit-testable with a mock controller + fake exec, mirroring the standard agent's `runJobStages` harness. Then add status-aware non-aborting execution and the `finally` block to that core.

**Tech Stack:** Go 1.26, `internal/dsl` (`EvalCondition`, `RunStatusView`), `internal/agent` (shared `Client`, `LogPusher`, `EvalForeachSource`), testify.

## Global Constraints

- Go module `github.com/eirueimi/unified-cd`, Go 1.26.2.
- The standard agent (`internal/agent/agent.go`) is the reference for semantics; match it.
- `if:` evaluation: every step (including empty `if:`) goes through `dsl.EvalCondition(step.If, tplData, statusView(), implicitSuccess)`. `implicitSuccess=true` for main stages, `false` for `finally`. On `false` → report step `Skipped`. On eval error, `EvalCondition` returns `(true, err)` → the step RUNS (fail-safe); log a warning, do not skip.
- Status functions semantics (from `dsl.EvalCondition`): `failure()`=`status.Failed`, `success()`=`!Failed && !Cancelled`, `always()`=true.
- A non-`continueOnError` step failure sets the shared failed flag and does NOT abort the stage loop — later steps auto-skip via `if:`. A `continueOnError` step failure does NOT set the flag.
- `finally` runs after the main stages on success or failure, using a FROZEN status snapshot (so finally steps don't auto-skip each other; a no-`if` finally step always runs). A finally step failure marks the run `Failed`.
- Final status precedence: `Failed > Succeeded` (cancellation is out of scope — see below).
- **Out of scope (pre-existing gap, do NOT add here):** mid-run cancellation detection. The k8s-agent does not poll for `RunCancelled` today; this plan does not add it. `statusView().Cancelled` is therefore always `false`. Note this difference in docs.
- **Out of scope:** secrets masking, cache/artifact/call steps, post-hooks in k8s — pre-existing gaps unrelated to `if:`/`finally`.
- Do NOT change `internal/agent/pipeline.go` or `internal/agent/agent.go` (the standard agent is already done).
- New unit tests must be plain (NOT behind `//go:build k8s`) so they run in `go test ./internal/k8sagent/...` without a cluster.

---

## File map

| File | Responsibility | Change |
|---|---|---|
| `internal/k8sagent/agent.go` | k8s run execution | Split `executeRun` into pod-lifecycle + `orchestrate`; add `if:`/status/`finally` to `orchestrate` |
| `internal/k8sagent/orchestrate_test.go` | unit tests for orchestration | New — mock controller + fake `podStepExec` |
| `docs/jobs.md` | docs | Remove the "k8s agent does not run finally/if:" caveat once parity lands |

---

## Task 1: Extract a testable `orchestrate` core (pure refactor)

**Goal:** Separate pod lifecycle from run orchestration with NO behavior change, and add a non-`k8s`-tagged test harness. This unlocks fast unit tests for Tasks 2–3.

**Files:**
- Modify: `internal/k8sagent/agent.go` (`executeRun`, lines 75–273)
- Test: `internal/k8sagent/orchestrate_test.go` (new)

**Interfaces:**
- Produces:
  - `type podStepExec func(ctx context.Context, step api.ClaimStep, expandedRun string) (exitCode int, stdout string, err error)`
  - `func (a *K8sAgent) orchestrate(ctx context.Context, c api.ClaimResponse, stepExec podStepExec)` — runs the stages, reports step/run status, promotes outputs. Same behavior as today (sequential; a non-continueOnError step failure aborts subsequent stages).

- [ ] **Step 1: Write the failing test (harness + current behavior)**

Create `internal/k8sagent/orchestrate_test.go`:

```go
package k8sagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// orchestrateHarness stands up a mock controller, records step statuses by
// name and the final run status, and runs orchestrate with a fake stepExec.
type orchestrateHarness struct {
	statuses map[string]string
	runState string // current run status served by GetRun
	final    string // status passed to FinishRun
}

// fakeExec returns exit code / stdout / err per step name.
type fakeStep struct {
	exit   int
	stdout string
}

func runOrchestrate(t *testing.T, c api.ClaimResponse, fakes map[string]fakeStep) (map[string]string, string) {
	t.Helper()
	h := &orchestrateHarness{statuses: map[string]string{}, runState: "Running"}
	var mu sync.Mutex

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/{id}/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		_ = decodeJSON(r, &req)
		mu.Lock()
		if req.StepName != "" {
			h.statuses[req.StepName] = req.Status
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/v1/agents/{id}/logs", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /api/v1/agents/{id}/runs/{runId}/steps/{idx}/outputs", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /api/v1/agents/{id}/runs/{runId}/outputs", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /api/v1/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock(); st := h.runState; mu.Unlock()
		writeJSON(w, api.Run{ID: c.RunID, Status: api.RunStatus(st)})
	})
	mux.HandleFunc("POST /api/v1/agents/{id}/runs/{runId}/finish", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Status string `json:"status"` }
		_ = decodeJSON(r, &req)
		mu.Lock(); h.final = req.Status; mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := agentlib.NewClient(srv.URL, "tok")
	a := &K8sAgent{cfg: Config{AgentID: "k8s-1"}, client: client}

	stepExec := func(_ context.Context, step api.ClaimStep, _ string) (int, string, error) {
		f, ok := fakes[step.Name]
		if !ok {
			return 0, "", nil
		}
		return f.exit, f.stdout, nil
	}
	a.orchestrate(context.Background(), c, stepExec)

	mu.Lock(); defer mu.Unlock()
	return h.statuses, h.final
}

func TestOrchestrate_SequentialAbortsOnFailure(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "boom", Run: "x"}},
		{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "after", Run: "y"}},
	}}
	statuses, final := runOrchestrate(t, c, map[string]fakeStep{"boom": {exit: 1}})
	assert.Equal(t, "Failed", statuses["boom"])
	assert.NotContains(t, statuses, "after", "current behavior: stage after a failure does not run")
	assert.Equal(t, "Failed", final)
}
```

Add small JSON helpers at the bottom of the test file (or reuse existing test helpers if `decodeJSON`/`writeJSON` already exist in the package's non-tagged tests — check `podmanager_test.go`/`pool_test.go` first; if present, do not redefine):

```go
func decodeJSON(r *http.Request, v any) error { return json.NewDecoder(r.Body).Decode(v) }
func writeJSON(w http.ResponseWriter, v any)  { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
```

(Import `encoding/json`. Verify the exact agent route paths against `internal/controller/server.go` and the `agentlib.Client` method URLs in `internal/agent/client.go` — adjust the mux patterns to match the real endpoints the client calls, e.g. the steps/logs/finish/outputs paths. The GetRun path must match what `Client.GetRun` requests.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/k8sagent/ -run TestOrchestrate_SequentialAbortsOnFailure -v`
Expected: FAIL — `a.orchestrate` undefined.

- [ ] **Step 3: Extract `orchestrate` from `executeRun`**

In `internal/k8sagent/agent.go`, define the callback type and refactor. Replace the body from line 145 (`overallStatus := api.RunSucceeded`) through line 272 (`FinishRun`) by moving it into a new method, and have `executeRun` build the production `stepExec` and call it.

Add the type (near the top, after imports):

```go
// podStepExec runs a single already-expanded step inside the pod and returns
// the exit code, captured stdout, and any infrastructure error.
type podStepExec func(ctx context.Context, step api.ClaimStep, expandedRun string) (exitCode int, stdout string, err error)
```

In `executeRun`, after the pod is running (after line 143's cleanWorkspace block), replace lines 145–272 with the production callback + a call to `orchestrate`:

```go
	stepExec := func(execCtx context.Context, step api.ClaimStep, expandedRun string) (int, string, error) {
		var stdoutBuf strings.Builder
		stderrPusher := agentlib.NewLogPusher(a.client, a.cfg.AgentID, c.RunID, step.Index, "stderr")
		stdoutWriter := io.MultiWriter(&stdoutBuf, &logLineWriter{
			client: a.client, agentID: a.cfg.AgentID, runID: c.RunID, stepIdx: step.Index, stream: "stdout",
		})
		ec, execErr := a.exec.ExecStep(execCtx, podName, step.Container, expandedRun, stdoutWriter, stderrPusher)
		stderrPusher.Flush(execCtx)
		return ec, stdoutBuf.String(), execErr
	}
	a.orchestrate(ctx, c, stepExec)
}
```

Then add the new `orchestrate` method, moving the stage loop + output promotion + finish into it, with the per-step exec replaced by `stepExec`:

```go
// orchestrate runs the claim's stages, reporting step/run status, using stepExec
// to run each step's command. Pure of pod lifecycle so it is unit-testable.
func (a *K8sAgent) orchestrate(ctx context.Context, c api.ClaimResponse, stepExec podStepExec) {
	overallStatus := api.RunSucceeded
	stepCtx := dsl.TemplateData{Params: c.Params, Steps: map[string]dsl.StepData{}}

	// runOneStep executes a single step via stepExec. Returns true if it did not fail.
	runOneStep := func(step api.ClaimStep) bool {
		started := time.Now().UTC()
		_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
			RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.Name, Status: "Running", StartedAt: started,
		})

		tplData := dsl.TemplateData{Params: stepCtx.Params, Steps: stepCtx.Steps}
		if step.ForeachKey != "" {
			tplData.Foreach = map[string]string{step.ForeachKey: step.ForeachValue}
		}
		expandedRun, _ := dsl.ExpandTemplate(step.Run, tplData)
		if expandedRun == "" {
			expandedRun = step.Run
		}

		ec, capturedStdout, execErr := stepExec(ctx, step, expandedRun)

		status := "Succeeded"
		if execErr != nil || ec != 0 {
			status = "Failed"
		} else {
			capturedOutputs := map[string]string{}
			outCtx := dsl.TemplateData{Params: stepCtx.Params, Steps: stepCtx.Steps, Stdout: capturedStdout}
			for outKey, outTpl := range step.Outputs {
				if val, err := dsl.ExpandTemplate(outTpl, outCtx); err == nil {
					capturedOutputs[outKey] = val
				}
			}
			if len(capturedOutputs) > 0 {
				stepCtx.Steps[step.Name] = dsl.StepData{Outputs: capturedOutputs}
				_ = a.client.SetStepOutputs(ctx, a.cfg.AgentID, c.RunID, step.Index, capturedOutputs)
			}
		}

		_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
			RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.Name,
			Status: status, ExitCode: ec, StartedAt: started, EndedAt: time.Now().UTC(),
		})
		return status != "Failed"
	}

	for _, stage := range c.Stages {
		for _, step := range api.StageSteps(stage) {
			if step.Foreach != nil {
				data := dsl.TemplateData{Params: c.Params, Steps: stepCtx.Steps}
				items, err := agentlib.EvalForeachSource(step.Foreach.Source, data)
				if err != nil {
					slog.Error("k8s: foreach expansion failed", "step", step.Name, "error", err)
					_ = a.client.FinishRun(ctx, a.cfg.AgentID, c.RunID, api.RunFailed)
					return
				}
				failed := false
				for _, item := range items {
					variant := step
					variant.Foreach = nil
					variant.ForeachKey = step.Foreach.Key
					variant.ForeachValue = item
					if !runOneStep(variant) {
						overallStatus = api.RunFailed
						failed = true
						break
					}
				}
				if failed {
					break
				}
			} else {
				if !runOneStep(step) {
					overallStatus = api.RunFailed
					break
				}
			}
		}
		if overallStatus == api.RunFailed {
			break
		}
	}

	runOutputs := map[string]string{}
	for _, stage := range c.Stages {
		for _, step := range api.StageSteps(stage) {
			if sd, ok := stepCtx.Steps[step.Name]; ok {
				for _, outName := range c.JobOutputs {
					if val, ok := sd.Outputs[outName]; ok {
						runOutputs[outName] = val
					}
				}
			}
		}
	}
	if len(runOutputs) > 0 {
		_ = a.client.SetRunOutputs(ctx, a.cfg.AgentID, c.RunID, runOutputs)
	}
	_ = a.client.FinishRun(ctx, a.cfg.AgentID, c.RunID, overallStatus)
}
```

(The moved code is byte-for-byte the existing logic except the inline `a.exec.ExecStep`/log-writer setup is now in the `stepExec` callback. Remove the now-unused inline pieces from `executeRun`. Keep `io`/`strings` imports — still used by the callback and `logLineWriter`.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/k8sagent/ -run TestOrchestrate -v && go build ./...`
Expected: PASS; module builds. Also run `go test -tags k8s ./internal/k8sagent/ -run xxx 2>&1 | head` is NOT required (no cluster); just ensure `go build -tags k8s ./internal/k8sagent/` still compiles so the tagged integration tests aren't broken by the refactor.

- [ ] **Step 5: Commit**

```bash
git add internal/k8sagent/agent.go internal/k8sagent/orchestrate_test.go
git commit -m "refactor(k8sagent): extract testable orchestrate core from executeRun"
```

---

## Task 2: `if:` evaluation + status-aware non-aborting execution

**Files:**
- Modify: `internal/k8sagent/agent.go` (`orchestrate`)
- Test: `internal/k8sagent/orchestrate_test.go`

**Interfaces:**
- Consumes: `orchestrate` / `podStepExec` (Task 1), `dsl.EvalCondition(expr, data, dsl.RunStatusView, implicitSuccess) (bool, error)`.
- Produces: `orchestrate` now evaluates `if:` per step (reports `Skipped`), records failures into a shared flag without aborting, respects `continueOnError`, and computes final status from the flag.

- [ ] **Step 1: Write the failing tests**

Append to `internal/k8sagent/orchestrate_test.go`:

```go
func TestOrchestrate_NoIfStepSkippedAfterFailure(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "boom", Run: "x"}},
		{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "after", Run: "y"}},
	}}
	statuses, final := runOrchestrate(t, c, map[string]fakeStep{"boom": {exit: 1}})
	assert.Equal(t, "Failed", statuses["boom"])
	assert.Equal(t, "Skipped", statuses["after"], "no-if step auto-skips after a failure")
	assert.Equal(t, "Failed", final)
}

func TestOrchestrate_AlwaysStepRunsAfterFailure(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "boom", Run: "x"}},
		{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "cleanup", If: "always()", Run: "y"}},
	}}
	statuses, final := runOrchestrate(t, c, map[string]fakeStep{"boom": {exit: 1}})
	assert.Equal(t, "Succeeded", statuses["cleanup"])
	assert.Equal(t, "Failed", final)
}

func TestOrchestrate_ContinueOnErrorDoesNotFailRun(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "flaky", Run: "x", ContinueOnError: true}},
		{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "after", Run: "y"}},
	}}
	statuses, final := runOrchestrate(t, c, map[string]fakeStep{"flaky": {exit: 1}})
	assert.Equal(t, "Failed", statuses["flaky"])
	assert.Equal(t, "Succeeded", statuses["after"], "continueOnError failure does not block later steps")
	assert.Equal(t, "Succeeded", final)
}
```

Also UPDATE `TestOrchestrate_SequentialAbortsOnFailure` from Task 1: under the new model the second step is reported `Skipped` (not absent). Change its assertion to `assert.Equal(t, "Skipped", statuses["after"])` and keep `final == "Failed"`. (Rename it to `TestOrchestrate_FailureSkipsRest` for clarity.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/k8sagent/ -run TestOrchestrate -v`
Expected: FAIL — no `if:` handling yet (no `Skipped` reported; `always()` step skipped because the loop aborts).

- [ ] **Step 3: Add status tracking + `if:` + non-abort to `orchestrate`**

Replace the `overallStatus`/loop logic in `orchestrate` with status-aware execution. At the top of `orchestrate`:

```go
	var anyStepFailed atomic.Bool
	statusView := func() dsl.RunStatusView {
		return dsl.RunStatusView{Failed: anyStepFailed.Load(), Cancelled: false}
	}
```

Change `runOneStep` to evaluate `if:` first and to record failure instead of signalling abort. Replace the start of `runOneStep` (before the "Running" report) with an `if:` gate, and at the end record the failure:

```go
	runOneStep := func(step api.ClaimStep) {
		tplData := dsl.TemplateData{Params: stepCtx.Params, Steps: stepCtx.Steps}
		if step.ForeachKey != "" {
			tplData.Foreach = map[string]string{step.ForeachKey: step.ForeachValue}
		}

		ok, err := dsl.EvalCondition(step.If, tplData, statusView(), true)
		if err != nil {
			slog.Warn("k8s: if condition eval failed, running step", "step", step.Name, "error", err)
		}
		if !ok {
			_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
				RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.Name, Status: "Skipped",
			})
			return
		}

		started := time.Now().UTC()
		_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
			RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.Name, Status: "Running", StartedAt: started,
		})

		expandedRun, _ := dsl.ExpandTemplate(step.Run, tplData)
		if expandedRun == "" {
			expandedRun = step.Run
		}
		ec, capturedStdout, execErr := stepExec(ctx, step, expandedRun)

		status := "Succeeded"
		if execErr != nil || ec != 0 {
			status = "Failed"
		} else {
			capturedOutputs := map[string]string{}
			outCtx := dsl.TemplateData{Params: stepCtx.Params, Steps: stepCtx.Steps, Stdout: capturedStdout}
			for outKey, outTpl := range step.Outputs {
				if val, err := dsl.ExpandTemplate(outTpl, outCtx); err == nil {
					capturedOutputs[outKey] = val
				}
			}
			if len(capturedOutputs) > 0 {
				stepCtx.Steps[step.Name] = dsl.StepData{Outputs: capturedOutputs}
				_ = a.client.SetStepOutputs(ctx, a.cfg.AgentID, c.RunID, step.Index, capturedOutputs)
			}
		}

		_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
			RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.Name,
			Status: status, ExitCode: ec, StartedAt: started, EndedAt: time.Now().UTC(),
		})
		if status == "Failed" && !step.ContinueOnError {
			anyStepFailed.Store(true)
		}
	}
```

Replace the stage loop so it never aborts (visit every stage/step; the `if:` auto-skip handles post-failure behavior):

```go
	for _, stage := range c.Stages {
		for _, step := range api.StageSteps(stage) {
			if step.Foreach != nil {
				data := dsl.TemplateData{Params: c.Params, Steps: stepCtx.Steps}
				items, err := agentlib.EvalForeachSource(step.Foreach.Source, data)
				if err != nil {
					slog.Error("k8s: foreach expansion failed", "step", step.Name, "error", err)
					anyStepFailed.Store(true)
					continue
				}
				for _, item := range items {
					variant := step
					variant.Foreach = nil
					variant.ForeachKey = step.Foreach.Key
					variant.ForeachValue = item
					runOneStep(variant)
				}
			} else {
				runOneStep(step)
			}
		}
	}
```

Replace the final-status computation (was `overallStatus`) — after output promotion:

```go
	overallStatus := api.RunSucceeded
	if anyStepFailed.Load() {
		overallStatus = api.RunFailed
	}
	_ = a.client.FinishRun(ctx, a.cfg.AgentID, c.RunID, overallStatus)
```

Add `"sync/atomic"` to imports. Remove the now-unused `overallStatus := api.RunSucceeded` line at the top and the old abort-based loop.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/k8sagent/ -run TestOrchestrate -v && go build ./... && go build -tags k8s ./internal/k8sagent/`
Expected: PASS; both build variants compile.

- [ ] **Step 5: Commit**

```bash
git add internal/k8sagent/agent.go internal/k8sagent/orchestrate_test.go
git commit -m "feat(k8sagent): evaluate if: with status functions; record failures without aborting"
```

---

## Task 3: `finally` block + docs

**Files:**
- Modify: `internal/k8sagent/agent.go` (`orchestrate`)
- Modify: `docs/jobs.md` (remove the k8s caveat)
- Test: `internal/k8sagent/orchestrate_test.go`

**Interfaces:**
- Consumes: `orchestrate` with status-aware `runOneStep` (Task 2), `c.Finally []api.ClaimStage`.
- Produces: `orchestrate` runs `c.Finally` after the main stages with a frozen status; a finally step failure marks the run `Failed`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/k8sagent/orchestrate_test.go`:

```go
func TestOrchestrate_FinallyRunsOnFailure(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "boom", Run: "x"}},
		},
		Finally: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 1, StageIndex: 0, Name: "notify", Run: "y"}},
			{Step: &api.ClaimStep{Index: 2, StageIndex: 1, Name: "rollback", If: "failure()", Run: "z"}},
			{Step: &api.ClaimStep{Index: 3, StageIndex: 2, Name: "only-ok", If: "success()", Run: "w"}},
		}}
	statuses, final := runOrchestrate(t, c, map[string]fakeStep{"boom": {exit: 1}})
	assert.Equal(t, "Succeeded", statuses["notify"], "no-if finally step always runs")
	assert.Equal(t, "Succeeded", statuses["rollback"], "failure() runs on failure")
	assert.Equal(t, "Skipped", statuses["only-ok"], "success() skips on failure")
	assert.Equal(t, "Failed", final)
}

func TestOrchestrate_FinallyStepFailureMarksRunFailed(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "ok", Run: "x"}},
		},
		Finally: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 1, StageIndex: 0, Name: "cleanup-boom", Run: "y"}},
			{Step: &api.ClaimStep{Index: 2, StageIndex: 1, Name: "cleanup-after", Run: "z"}},
		}}
	statuses, final := runOrchestrate(t, c, map[string]fakeStep{"cleanup-boom": {exit: 1}})
	assert.Equal(t, "Failed", statuses["cleanup-boom"])
	assert.Equal(t, "Succeeded", statuses["cleanup-after"], "all finally steps run to completion")
	assert.Equal(t, "Failed", final)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/k8sagent/ -run TestOrchestrate_Finally -v`
Expected: FAIL — `finally` never runs.

- [ ] **Step 3: Add `finally` execution to `orchestrate`**

The main `runOneStep` records into `anyStepFailed` and evaluates `if:` against `statusView()` (live). For `finally`, evaluate `if:` against a FROZEN snapshot and record finally failures into a separate flag, then fold both into the final status.

Refactor `runOneStep` into a factory parametrized by the status source, the implicit-success flag, and the failure flag (mirroring the standard agent's `makeStepRunner`):

```go
	makeRunStep := func(statusFn func() dsl.RunStatusView, implicitSuccess bool, failedFlag *atomic.Bool) func(api.ClaimStep) {
		return func(step api.ClaimStep) {
			tplData := dsl.TemplateData{Params: stepCtx.Params, Steps: stepCtx.Steps}
			if step.ForeachKey != "" {
				tplData.Foreach = map[string]string{step.ForeachKey: step.ForeachValue}
			}
			ok, err := dsl.EvalCondition(step.If, tplData, statusFn(), implicitSuccess)
			if err != nil {
				slog.Warn("k8s: if condition eval failed, running step", "step", step.Name, "error", err)
			}
			if !ok {
				_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
					RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.Name, Status: "Skipped",
				})
				return
			}
			started := time.Now().UTC()
			_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
				RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.Name, Status: "Running", StartedAt: started,
			})
			expandedRun, _ := dsl.ExpandTemplate(step.Run, tplData)
			if expandedRun == "" {
				expandedRun = step.Run
			}
			ec, capturedStdout, execErr := stepExec(ctx, step, expandedRun)
			status := "Succeeded"
			if execErr != nil || ec != 0 {
				status = "Failed"
			} else {
				capturedOutputs := map[string]string{}
				outCtx := dsl.TemplateData{Params: stepCtx.Params, Steps: stepCtx.Steps, Stdout: capturedStdout}
				for outKey, outTpl := range step.Outputs {
					if val, err := dsl.ExpandTemplate(outTpl, outCtx); err == nil {
						capturedOutputs[outKey] = val
					}
				}
				if len(capturedOutputs) > 0 {
					stepCtx.Steps[step.Name] = dsl.StepData{Outputs: capturedOutputs}
					_ = a.client.SetStepOutputs(ctx, a.cfg.AgentID, c.RunID, step.Index, capturedOutputs)
				}
			}
			_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
				RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.Name,
				Status: status, ExitCode: ec, StartedAt: started, EndedAt: time.Now().UTC(),
			})
			if status == "Failed" && !step.ContinueOnError {
				failedFlag.Store(true)
			}
		}
	}

	mainRun := makeRunStep(statusView, true, &anyStepFailed)
```

Change the main loop to call `mainRun(...)` instead of `runOneStep(...)`. After the main loop + output promotion, add the finally block:

```go
	mainFailed := anyStepFailed.Load()

	var finallyFailed atomic.Bool
	if len(c.Finally) > 0 {
		frozen := dsl.RunStatusView{Failed: mainFailed, Cancelled: false}
		finallyRun := makeRunStep(func() dsl.RunStatusView { return frozen }, false, &finallyFailed)
		for _, stage := range c.Finally {
			for _, step := range api.StageSteps(stage) {
				if step.Foreach != nil {
					data := dsl.TemplateData{Params: c.Params, Steps: stepCtx.Steps}
					items, err := agentlib.EvalForeachSource(step.Foreach.Source, data)
					if err != nil {
						slog.Error("k8s: finally foreach expansion failed", "step", step.Name, "error", err)
						finallyFailed.Store(true)
						continue
					}
					for _, item := range items {
						variant := step
						variant.Foreach = nil
						variant.ForeachKey = step.Foreach.Key
						variant.ForeachValue = item
						finallyRun(variant)
					}
				} else {
					finallyRun(step)
				}
			}
		}
	}

	overallStatus := api.RunSucceeded
	if mainFailed || finallyFailed.Load() {
		overallStatus = api.RunFailed
	}
	_ = a.client.FinishRun(ctx, a.cfg.AgentID, c.RunID, overallStatus)
```

(Remove the Task-2 `overallStatus`/`FinishRun` lines — they are now computed after finally. Ensure `FinishRun` is called exactly once.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/k8sagent/ -run TestOrchestrate -v && go build ./... && go build -tags k8s ./internal/k8sagent/`
Expected: PASS; both build variants compile.

- [ ] **Step 5: Remove the k8s caveat from docs**

In `docs/jobs.md`, find the caveat added when the standard-agent finally feature shipped (a `>` blockquote in the `## Finally Block` and `## Status Functions in if:` sections saying the Kubernetes agent does not run finally / evaluate `if:`). Remove those two notes now that parity exists. Verify: `grep -ni "kubernetes agent does not" docs/jobs.md` returns nothing. Do NOT remove the note that `cache:`/`post:` are unsupported in finally (still true). If a note about k8s cancellation is desired, you may add one line that mid-run cancellation detection is not yet implemented on the k8s-agent (the one remaining difference) — keep it accurate.

- [ ] **Step 6: Commit**

```bash
git add internal/k8sagent/agent.go internal/k8sagent/orchestrate_test.go docs/jobs.md
git commit -m "feat(k8sagent): run finally block after main stages; remove parity caveat from docs"
```

---

## Final verification

- [ ] `go test ./internal/k8sagent/... -v` — all orchestration unit tests pass.
- [ ] `go build ./...` and `go build -tags k8s ./internal/k8sagent/` — both compile (the tagged integration tests still build).
- [ ] `go test ./... -short` — full suite green.

## Self-review notes (coverage vs the parity goal)

- `if:` evaluation incl. `failure()`/`success()`/`always()` on main stages → Task 2.
- Non-aborting status-aware execution + `continueOnError` → Task 2.
- `finally` block with frozen status, all-run-to-completion, finally-failure → Failed → Task 3.
- Testability without a real cluster (the integration tests are `//go:build k8s`) → Task 1 seam + non-tagged unit tests.
- Cancellation explicitly OUT of scope (pre-existing gap) — documented in Task 3 Step 5 and Global Constraints.
- Docs caveat removal → Task 3 Step 5.
