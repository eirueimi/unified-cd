# `downloadArtifact.runId` + call-step `ChildRunID` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a parent job download artifacts uploaded by a `call:` child run, via an optional template-expandable `runId:` field on `downloadArtifact` and a new `{{ .Steps.<step>.ChildRunID }}` template value.

**Architecture:** The DSL and wire types gain one optional string field each; the shared orchestrator (used by both host and k8s agents) records the child run ID into step template data on call-step success, and `executeDownloadArtifact` expands/validates `runId` before handing the target run ID to the backend. Both backends are already parameterized by run ID, so no backend or controller changes are needed.

**Tech Stack:** Go, `text/template` (via `dsl.ExpandTemplate`), httptest fake-controller harness in `internal/agent` tests.

**Spec:** `docs/superpowers/specs/2026-07-25-download-artifact-runid-design.md`

## Global Constraints

- All committed text (code, comments, docs, commit messages) must be in English (AGENTS.md).
- Work happens in the worktree `C:\Users\arimax\unified-cd-project\unified-cd-download-artifact-runid` on branch `download-artifact-runid`; never commit in the main tree.
- `go` commands in this worktree need `GOFLAGS=-buildvcs=false` (VCS stamping fails in linked worktrees on this machine).
- `docs/field-reference.md` and `schemas/unified-cd.schema.json` are generated — never hand-edit; run `go generate ./...` after changing `internal/dsl/types.go` and commit the diff (AGENTS.md).
- The expanded `runId` value must match `^[A-Za-z0-9_-]{1,64}$` (URL-path / S3-key safety); empty-after-expansion is a failure.
- `runId` template expansion context is restricted to `Params`, `Steps`, `Matrix`, `Foreach` — never `Secrets` or `Stdout`.

---

### Task 1: DSL types — `runId` field, `StepData.ChildRunID`, regenerate schema

**Files:**
- Modify: `internal/dsl/types.go:320-323` (DownloadArtifactStep)
- Modify: `internal/dsl/template.go:27-32` (StepData)
- Test: `internal/dsl/parse_test.go`, `internal/dsl/template_test.go`
- Regenerate: `schemas/unified-cd.schema.json`, `docs/field-reference.md` (via `go generate ./...`)

**Interfaces:**
- Produces: `dsl.DownloadArtifactStep.RunID string` (yaml `runId,omitempty`); `dsl.StepData.ChildRunID any` (string for plain call steps, `map[string]string` keyed by combination key for matrix call steps).

- [ ] **Step 1: Write the failing tests**

In `internal/dsl/parse_test.go` (next to `TestParse_StepOutputs`):

```go
func TestParse_DownloadArtifactRunID(t *testing.T) {
	yamlDoc := `
apiVersion: unified.cd/v1
kind: Job
metadata:
  name: fetch
spec:
  steps:
    - name: fetch-child-binary
      downloadArtifact:
        name: app-binary
        runId: "{{ .Steps.build-app.ChildRunID }}"
        destDir: artifacts
`
	job, err := ParseJob([]byte(yamlDoc))
	require.NoError(t, err)
	require.NotNil(t, job.Spec.Steps[0].DownloadArtifact)
	assert.Equal(t, "{{ .Steps.build-app.ChildRunID }}", job.Spec.Steps[0].DownloadArtifact.RunID)
}
```

(Adapt the surrounding YAML skeleton to match how `TestParse_StepOutputs` at `parse_test.go:120` builds a minimal job — reuse its header/parse call verbatim if `ParseJob` is named differently there.)

In `internal/dsl/template_test.go` (next to `TestExpandTemplate_StepOutputs`):

```go
func TestExpandTemplate_ChildRunID(t *testing.T) {
	data := TemplateData{
		Steps: map[string]StepData{"build-app": {ChildRunID: "run-child-1"}},
	}
	out, err := ExpandTemplate("{{ .Steps.build-app.ChildRunID }}", data)
	require.NoError(t, err)
	assert.Equal(t, "run-child-1", out)
}

func TestExpandTemplate_ChildRunID_MatrixAggregated(t *testing.T) {
	data := TemplateData{
		Steps: map[string]StepData{"build": {ChildRunID: map[string]string{
			"linux/amd64": "run-a",
			"linux/arm64": "run-b",
		}}},
	}
	out, err := ExpandTemplate(`{{ index .Steps.build.ChildRunID "linux/arm64" }}`, data)
	require.NoError(t, err)
	assert.Equal(t, "run-b", out)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `GOFLAGS=-buildvcs=false go test ./internal/dsl/ -run 'TestParse_DownloadArtifactRunID|TestExpandTemplate_ChildRunID' -v`
Expected: compile error — `unknown field RunID` / `unknown field ChildRunID`.

- [ ] **Step 3: Add the fields**

`internal/dsl/types.go` — replace the DownloadArtifactStep definition:

```go
type DownloadArtifactStep struct {
	Name    string `yaml:"name"`
	DestDir string `yaml:"destDir,omitempty"` // defaults to the current directory if omitted
	// RunID selects the run to download from. Template-expandable (e.g.
	// "{{ .Steps.build-app.ChildRunID }}" to fetch from a call step's child
	// run). Empty means the current run. The expanded value must match
	// ^[A-Za-z0-9_-]{1,64}$; expansion excludes Secrets and Stdout.
	RunID string `yaml:"runId,omitempty"`
}
```

`internal/dsl/template.go` — extend StepData:

```go
// StepData holds the captured outputs of a previously executed step.
// Non-matrix steps store plain strings; matrix steps store an aggregated
// map[string]string keyed by combination key (e.g. "linux/amd64").
type StepData struct {
	Outputs map[string]any
	// ChildRunID is set for completed call: steps only. A plain string for
	// non-matrix call steps; a map[string]string keyed by combination key
	// for matrix call steps (mirroring Outputs aggregation).
	ChildRunID any
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `GOFLAGS=-buildvcs=false go test ./internal/dsl/ -count=1`
Expected: PASS (whole package — guards against schema-tag regressions).

- [ ] **Step 5: Regenerate schema + field reference**

Run: `GOFLAGS=-buildvcs=false go generate ./...`
Expected: `schemas/unified-cd.schema.json` gains a `runId` property under `DownloadArtifactStep`; `docs/field-reference.md` gains the field row. Verify with `git diff --stat` that ONLY generated files plus your edits changed.

- [ ] **Step 6: Commit**

```bash
git add internal/dsl/types.go internal/dsl/template.go internal/dsl/parse_test.go internal/dsl/template_test.go schemas/unified-cd.schema.json docs/field-reference.md
git commit -m "feat(dsl): add downloadArtifact.runId and StepData.ChildRunID"
```

---

### Task 2: Wire type + controller claim conversion

**Files:**
- Modify: `internal/api/types.go:355-358` (api.DownloadArtifactStep)
- Modify: `internal/controller/api_agent.go:390-392` (buildOneClaimStep)
- Test: `internal/controller/api_agent_test.go` (near the existing `buildOneClaimStep` test at line 861)

**Interfaces:**
- Consumes: `dsl.DownloadArtifactStep.RunID` (Task 1).
- Produces: `api.DownloadArtifactStep.RunID string` (json `runId,omitempty`), carried on ClaimStep to agents.

- [ ] **Step 1: Write the failing test**

In `internal/controller/api_agent_test.go`:

```go
func TestBuildOneClaimStep_DownloadArtifactRunID(t *testing.T) {
	entry := dsl.StepEntry{
		Name: "fetch",
		DownloadArtifact: &dsl.DownloadArtifactStep{
			Name:    "app-binary",
			DestDir: "artifacts",
			RunID:   "{{ .Steps.build-app.ChildRunID }}",
		},
	}
	cs := buildOneClaimStep(0, 0, entry, nil)
	require.NotNil(t, cs.DownloadArtifact)
	assert.Equal(t, "app-binary", cs.DownloadArtifact.Name)
	assert.Equal(t, "artifacts", cs.DownloadArtifact.DestDir)
	assert.Equal(t, "{{ .Steps.build-app.ChildRunID }}", cs.DownloadArtifact.RunID)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `GOFLAGS=-buildvcs=false go test ./internal/controller/ -run TestBuildOneClaimStep_DownloadArtifactRunID -v`
Expected: compile error — `unknown field RunID in struct literal of type api.DownloadArtifactStep` (dsl side compiles after Task 1).

- [ ] **Step 3: Add wire field + conversion**

`internal/api/types.go`:

```go
type DownloadArtifactStep struct {
	Name    string `json:"name"`
	DestDir string `json:"destDir,omitempty"`
	// RunID selects the run to download from; template-expanded and
	// validated agent-side. Empty means the current run.
	RunID string `json:"runId,omitempty"`
}
```

`internal/controller/api_agent.go` (line 390-392):

```go
if entry.DownloadArtifact != nil {
	cs.DownloadArtifact = &api.DownloadArtifactStep{Name: entry.DownloadArtifact.Name, DestDir: entry.DownloadArtifact.DestDir, RunID: entry.DownloadArtifact.RunID}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `GOFLAGS=-buildvcs=false go test ./internal/controller/ -run TestBuildOneClaimStep -count=1 -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/types.go internal/controller/api_agent.go internal/controller/api_agent_test.go
git commit -m "feat(api): carry downloadArtifact.runId on ClaimStep"
```

---

### Task 3: Record `ChildRunID` on call-step success

**Files:**
- Modify: `internal/agent/stepoutputs.go` (new pure helper `ApplyChildRunID`)
- Modify: `internal/agent/pipeline.go` (new `safeStepCtx.setCallStepResult`)
- Modify: `internal/agent/orchestrator.go:448-453` (call branch uses the new setter)
- Test: `internal/agent/stepoutputs_test.go`, `internal/agent/agent_callrun_test.go`

**Interfaces:**
- Consumes: `dsl.StepData.ChildRunID` (Task 1); `ExecuteCallStep`'s existing `childRunID` return.
- Produces: `ApplyChildRunID(steps map[string]dsl.StepData, stepName, matrixKey, childRunID string)`; `(*safeStepCtx).setCallStepResult(name, comboKey string, outputs map[string]string, childRunID string)`.

- [ ] **Step 1: Write the failing unit tests**

In `internal/agent/stepoutputs_test.go`:

```go
func TestApplyChildRunID_Plain(t *testing.T) {
	steps := map[string]dsl.StepData{}
	ApplyStepOutputs(steps, "call-child", "", map[string]string{"v": "1"})
	ApplyChildRunID(steps, "call-child", "", "run-child-1")
	assert.Equal(t, "run-child-1", steps["call-child"].ChildRunID)
	assert.Equal(t, "1", steps["call-child"].Outputs["v"])
}

func TestApplyChildRunID_MatrixAggregates(t *testing.T) {
	steps := map[string]dsl.StepData{}
	ApplyChildRunID(steps, "call-child", "linux/amd64", "run-a")
	ApplyChildRunID(steps, "call-child", "linux/arm64", "run-b")
	assert.Equal(t, map[string]string{
		"linux/amd64": "run-a",
		"linux/arm64": "run-b",
	}, steps["call-child"].ChildRunID)
}

// Copy-on-write: a snapshot of the StepData taken before a later matrix
// variant lands must not observe the merge (mirrors ApplyStepOutputs' COW
// contract for concurrent snapshot safety).
func TestApplyChildRunID_MatrixCopyOnWrite(t *testing.T) {
	steps := map[string]dsl.StepData{}
	ApplyChildRunID(steps, "call-child", "linux/amd64", "run-a")
	snap := steps["call-child"].ChildRunID.(map[string]string)
	ApplyChildRunID(steps, "call-child", "linux/arm64", "run-b")
	assert.Equal(t, map[string]string{"linux/amd64": "run-a"}, snap)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `GOFLAGS=-buildvcs=false go test ./internal/agent/ -run TestApplyChildRunID -v`
Expected: compile error — `undefined: ApplyChildRunID`.

- [ ] **Step 3: Implement helper + setter + orchestrator wiring**

Append to `internal/agent/stepoutputs.go`:

```go
// ApplyChildRunID records a call step's child run ID into steps under
// stepName, next to the outputs ApplyStepOutputs recorded.
//
// If matrixKey is empty, it sets StepData.ChildRunID to the plain string.
// If matrixKey is non-empty, it merges into an aggregated map[string]string
// keyed by combination key, using the same copy-on-write discipline as
// ApplyStepOutputs: the merged map is rebuilt fresh on every call so a
// previously published snapshot is never mutated.
//
// Like ApplyStepOutputs this is a pure function and does not lock; callers
// sharing steps across goroutines must hold their own lock.
func ApplyChildRunID(steps map[string]dsl.StepData, stepName, matrixKey, childRunID string) {
	sd := steps[stepName]
	if matrixKey == "" {
		sd.ChildRunID = childRunID
		steps[stepName] = sd
		return
	}
	merged := map[string]string{matrixKey: childRunID}
	if prev, ok := sd.ChildRunID.(map[string]string); ok {
		for k, v := range prev {
			merged[k] = v
		}
		merged[matrixKey] = childRunID
	}
	sd.ChildRunID = merged
	steps[stepName] = sd
}
```

Append to `internal/agent/pipeline.go` (after `setStepMatrixOutputs`):

```go
// setCallStepResult records a completed call step's child outputs and child
// run ID in one critical section, with the same copy-on-write rebuild as
// setStepMatrixOutputs so concurrent snapshots never observe partial writes.
func (s *safeStepCtx) setCallStepResult(name, comboKey string, outputs map[string]string, childRunID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	newSteps := make(map[string]dsl.StepData, len(s.data.Steps)+1)
	for k, v := range s.data.Steps {
		newSteps[k] = v
	}
	ApplyStepOutputs(newSteps, name, comboKey, outputs)
	ApplyChildRunID(newSteps, name, comboKey, childRunID)
	s.data.Steps = newSteps
}
```

In `internal/agent/orchestrator.go`, replace the success branch of the call step (currently lines 448-453):

```go
} else {
	sctx.setCallStepResult(step.Name, step.MatrixKey, childOutputs, childRunID)
	if len(childOutputs) > 0 {
```

(i.e. the `if step.MatrixKey != "" { setStepMatrixOutputs } else { setStep }` pair collapses into the single `setCallStepResult` call; `ApplyStepOutputs` with an empty matrixKey has identical replace semantics to the old `setStep` path. The `run:`-step outputs path at lines ~608-613 is NOT touched.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `GOFLAGS=-buildvcs=false go test ./internal/agent/ -run 'TestApplyChildRunID|TestApplyStepOutputs|TestExecuteRun_CallStep' -count=1 -v`
Expected: PASS, including all existing call-step tests.

- [ ] **Step 5: Commit**

```bash
git add internal/agent/stepoutputs.go internal/agent/pipeline.go internal/agent/orchestrator.go internal/agent/stepoutputs_test.go
git commit -m "feat(agent): expose call-step child run ID as StepData.ChildRunID"
```

---

### Task 4: `executeDownloadArtifact` — expand and validate `runId`

**Files:**
- Modify: `internal/agent/orchestrator.go` — `executeDownloadArtifact` (line ~830) and its call site (line ~406-418)
- Test: `internal/agent/agent_download_runid_test.go` (new file)

**Interfaces:**
- Consumes: `api.DownloadArtifactStep.RunID` (Task 2); `dsl.ExpandTemplate(v string, data dsl.TemplateData) (string, error)`.
- Produces: `executeDownloadArtifact(ctx, client, agentID, step, runID string, b, scope, tplData dsl.TemplateData) error` — new trailing `tplData` parameter; download targets the expanded run ID.

- [ ] **Step 1: Write the failing tests**

Create `internal/agent/agent_download_runid_test.go`. Model the harness on `TestExecuteRun_DownloadArtifact_ScopedUsesAbsoluteContainerPath` (`agent_scope_test.go:217`) but native (no scope):

```go
package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// downloadRunIDHarness drives a single-step downloadArtifact claim through
// the fake-controller harness. It serves artifact "bin" for exactly one run
// ID (serveRunID) and records every artifact GET path and the finish status.
func downloadRunIDHarness(t *testing.T, stepRunID, serveRunID string) (finalStatus string, artifactHits *int32, workDir string) {
	t.Helper()
	const agentID = "dl-runid-agent"
	const runID = "run-parent"
	workDir = t.TempDir()

	var hits int32
	finishCh := make(chan string, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/runs/"+serveRunID+"/artifacts/bin", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write(makeAgentTestTarZstd(t, map[string]string{"bin.txt": "child-binary"})) //nolint:errcheck
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Status string `json:"status"`
		}
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		select {
		case finishCh <- body.Status:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}
	resp := api.ClaimResponse{
		Native:  true,
		RunID:   runID,
		JobName: "test-dl-runid",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0, StageIndex: 0, Name: "fetch",
				DownloadArtifact: &api.DownloadArtifactStep{Name: "bin", RunID: stepRunID},
			}},
		},
	}
	a.executeRun(context.Background(), resp, workDir)

	select {
	case finalStatus = <-finishCh:
	default:
		t.Fatal("FinishRun was not called")
	}
	return finalStatus, &hits, workDir
}

// A literal runId downloads from that run, not the current one.
func TestExecuteRun_DownloadArtifact_RunIDOverride(t *testing.T) {
	status, hits, workDir := downloadRunIDHarness(t, "run-other", "run-other")
	assert.Equal(t, "Succeeded", status)
	assert.EqualValues(t, 1, atomic.LoadInt32(hits))
	got, err := os.ReadFile(filepath.Join(workDir, "bin.txt"))
	require.NoError(t, err)
	assert.Equal(t, "child-binary", string(got))
}

// Empty runId keeps downloading from the current run (regression guard).
func TestExecuteRun_DownloadArtifact_EmptyRunIDUsesCurrentRun(t *testing.T) {
	status, hits, _ := downloadRunIDHarness(t, "", "run-parent")
	assert.Equal(t, "Succeeded", status)
	assert.EqualValues(t, 1, atomic.LoadInt32(hits))
}

// A runId template that fails to expand fails the step without any request.
func TestExecuteRun_DownloadArtifact_BadRunIDTemplate_Fails(t *testing.T) {
	status, hits, _ := downloadRunIDHarness(t, "{{ .Steps.missing.ChildRunID.bogus }}", "run-parent")
	assert.Equal(t, "Failed", status)
	assert.EqualValues(t, 0, atomic.LoadInt32(hits))
}

// An expanded runId that violates ^[A-Za-z0-9_-]{1,64}$ (path traversal,
// URL structure characters, empty) fails the step without any request.
func TestExecuteRun_DownloadArtifact_InvalidRunIDValue_Fails(t *testing.T) {
	for _, bad := range []string{"../evil", "a/b", "run id", "{{ .Steps.missing.ChildRunID }}"} {
		t.Run(bad, func(t *testing.T) {
			status, hits, _ := downloadRunIDHarness(t, bad, "run-parent")
			assert.Equal(t, "Failed", status)
			assert.EqualValues(t, 0, atomic.LoadInt32(hits))
		})
	}
}
```

Note on the last case: `{{ .Steps.missing.ChildRunID }}` expands to an empty string (missing map key → nil → `<no value>`/empty depending on template option); if `ExpandTemplate` renders `<no value>`, the pattern still rejects it. Either way the step must fail with no request.

- [ ] **Step 2: Run tests to verify they fail**

Run: `GOFLAGS=-buildvcs=false go test ./internal/agent/ -run TestExecuteRun_DownloadArtifact_ -v`
Expected: `TestExecuteRun_DownloadArtifact_RunIDOverride` FAILS (downloads from run-parent, gets 404 → step Failed); the two negative tests may pass or fail incidentally — the override test failing is the signal.

- [ ] **Step 3: Implement expansion + validation**

In `internal/agent/orchestrator.go`:

Add near the top of the file (with the other package-level vars, or directly above `executeDownloadArtifact`):

```go
// artifactRunIDRe constrains the expanded downloadArtifact.runId value. The
// value is spliced into a URL path (host backend) and a sidecar --run
// argument (k8s backend), so it must not contain path separators, dots, or
// any URL-structure characters.
var artifactRunIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
```

Change the call site (currently line ~406-418) to build template data and pass it:

```go
if step.DownloadArtifact != nil {
	scope, serr := resolveScope(stepCtx, step, b)
	if serr != nil {
		slog.Error("download artifact failed", "step", step.Name, "error", serr)
		markFailed(context.WithoutCancel(stepCtx))
		return nil
	}
	dlData := sctx.snapshot()
	if step.MatrixValues != nil {
		dlData.Matrix = step.MatrixValues
		dlData.Foreach = step.MatrixValues
	}
	if err := executeDownloadArtifact(stepCtx, client, agentID, step, c.RunID, b, scope, dlData); err != nil {
		slog.Error("download artifact failed", "step", step.Name, "error", err)
		markFailed(context.WithoutCancel(stepCtx))
	}
	return nil
}
```

Change `executeDownloadArtifact` (line ~830): new signature and target-run resolution before the destDir logic. The failure paths reuse the function's existing "report Failed" block shape:

```go
// executeDownloadArtifact runs a download-artifact step, mirroring
// executeUploadArtifact's path resolution (see ExecBackend.ResolveArtifactPath).
// tplData is used to expand DownloadArtifact.RunID; Secrets and Stdout are
// deliberately excluded from the expansion context because the expanded
// value is embedded in a URL path and appears in logs (same precedent as
// call: param expansion in ExecuteCallStep).
func executeDownloadArtifact(ctx context.Context, client *Client, agentID string, step api.ClaimStep, runID string, b ExecBackend, scope ScopeHandle, tplData dsl.TemplateData) error {
	started := time.Now().UTC()
	_ = client.ReportStep(ctx, agentID, api.StepReportRequest{
		RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Running", StartedAt: started,
	})

	da := step.DownloadArtifact
	failStep := func(err error) error {
		slog.Error("download-artifact failed", "step", step.Name, "error", err)
		_ = client.ReportStep(ctx, agentID, api.StepReportRequest{
			RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Failed",
			StartedAt: started, EndedAt: time.Now().UTC(),
		})
		return fmt.Errorf("download-artifact %q: %w", da.Name, err)
	}

	targetRunID := runID
	if da.RunID != "" {
		restricted := dsl.TemplateData{Params: tplData.Params, Steps: tplData.Steps, Matrix: tplData.Matrix, Foreach: tplData.Foreach}
		expanded, err := dsl.ExpandTemplate(da.RunID, restricted)
		if err != nil {
			return failStep(fmt.Errorf("runId template: %w", err))
		}
		if !artifactRunIDRe.MatchString(expanded) {
			// Do not echo the expanded value: it is attacker-influenced on
			// the failure path and would land in operator-read logs.
			return failStep(fmt.Errorf("runId expanded to a value not matching %s", artifactRunIDRe.String()))
		}
		targetRunID = expanded
	}

	destDir := da.DestDir
	if destDir == "" {
		destDir = "."
	}
	resolvedDestDir, err := b.ResolveArtifactPath(scope, destDir)
	if err != nil {
		return failStep(err)
	}

	if err := b.DownloadArtifact(ctx, scope, targetRunID, da.Name, resolvedDestDir); err != nil {
		return failStep(err)
	}
	// ... keep the existing success ReportStep tail unchanged ...
```

Refactor note: the two existing inline failure blocks (path rejection, download failure) collapse into `failStep`; keep their behavior identical (same report fields, same wrapped error format). Keep the existing success-report tail exactly as-is. Add `"regexp"` and the `dsl` import if missing (dsl is already imported in orchestrator.go).

- [ ] **Step 4: Run tests to verify they pass**

Run: `GOFLAGS=-buildvcs=false go test ./internal/agent/ -run 'TestExecuteRun_DownloadArtifact|TestExecuteRun_UploadArtifact' -count=1 -v`
Expected: PASS, including the pre-existing scoped download test (its call site now passes tplData).

- [ ] **Step 5: Commit**

```bash
git add internal/agent/orchestrator.go internal/agent/agent_download_runid_test.go
git commit -m "feat(agent): downloadArtifact.runId selects the source run"
```

---

### Task 5: Integration + k8s parity tests

**Files:**
- Test: `internal/agent/agent_download_runid_test.go` (extend)
- Test: `internal/k8sagent/orchestrate_test.go` (extend, near `TestOrchestrate_DownloadArtifactDispatchesToSidecar` at line 494)

**Interfaces:**
- Consumes: everything from Tasks 1-4. No new production code — if these tests need production changes, STOP and report.

- [ ] **Step 1: Write the host end-to-end test (call → ChildRunID → download)**

Append to `internal/agent/agent_download_runid_test.go`:

```go
// Full flow: a call step records its child run ID, and a later
// downloadArtifact with runId: "{{ .Steps.<call>.ChildRunID }}" downloads
// the child run's artifact.
func TestExecuteRun_CallThenDownloadChildArtifact(t *testing.T) {
	const agentID = "call-dl-agent"
	const runID = "run-parent"
	const childRunID = "run-child-42"
	workDir := t.TempDir()

	finishCh := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/children", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.Run{ID: childRunID, Status: api.RunSucceeded}) //nolint:errcheck
	})
	mux.HandleFunc("GET /api/v1/runs/"+childRunID, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.Run{ID: childRunID, Status: api.RunSucceeded}) //nolint:errcheck
	})
	mux.HandleFunc("GET /api/v1/runs/"+childRunID+"/outputs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.RunOutputs{Outputs: map[string]string{}}) //nolint:errcheck
	})
	mux.HandleFunc("GET /api/v1/runs/"+childRunID+"/artifacts/app-binary", func(w http.ResponseWriter, r *http.Request) {
		w.Write(makeAgentTestTarZstd(t, map[string]string{"app.txt": "from-child"})) //nolint:errcheck
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Status string `json:"status"`
		}
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		select {
		case finishCh <- body.Status:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}
	resp := api.ClaimResponse{
		Native:  true,
		RunID:   runID,
		JobName: "test-call-dl",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0, StageIndex: 0, Name: "build-app",
				Call: &api.ClaimCallStep{Job: "build"},
			}},
			{Step: &api.ClaimStep{
				Index: 1, StageIndex: 1, Name: "fetch",
				DownloadArtifact: &api.DownloadArtifactStep{
					Name:  "app-binary",
					RunID: "{{ .Steps.build-app.ChildRunID }}",
				},
			}},
		},
	}
	a.executeRun(context.Background(), resp, workDir)

	select {
	case status := <-finishCh:
		assert.Equal(t, "Succeeded", status)
	default:
		t.Fatal("FinishRun was not called")
	}
	got, err := os.ReadFile(filepath.Join(workDir, "app.txt"))
	require.NoError(t, err)
	assert.Equal(t, "from-child", string(got))
}
```

(Reuse the existing imports; the file already imports everything needed except possibly nothing new.)

- [ ] **Step 2: Write the k8s parity test**

Append to `internal/k8sagent/orchestrate_test.go` after `TestOrchestrate_DownloadArtifactDispatchesToSidecar`:

```go
func TestOrchestrate_DownloadArtifactRunIDOverridesSidecarRun(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "dl",
			DownloadArtifact: &api.DownloadArtifactStep{Name: "app", DestDir: "out", RunID: "r-child"}}},
	}}
	rec, statuses, _ := runOrchestrateArtifact(t, c, 0)
	require.Len(t, rec, 1)
	assert.Equal(t, []string{"unified-sidecar", "artifact", "download",
		"--run", "r-child", "--name", "app", "--dest", "/workspace/out"}, rec[0].argv)
	assert.Equal(t, "Succeeded", statuses["dl"])
}
```

- [ ] **Step 3: Run both new tests**

Run: `GOFLAGS=-buildvcs=false go test ./internal/agent/ -run TestExecuteRun_CallThenDownloadChildArtifact -count=1 -v && GOFLAGS=-buildvcs=false go test ./internal/k8sagent/ -run TestOrchestrate_DownloadArtifact -count=1 -v`
Expected: PASS. These are pure integration tests over Tasks 1-4; if either fails, fix the production code from the earlier tasks — do not weaken the tests.

- [ ] **Step 4: Commit**

```bash
git add internal/agent/agent_download_runid_test.go internal/k8sagent/orchestrate_test.go
git commit -m "test(agent,k8sagent): call-to-download child artifact flow and sidecar parity"
```

---

### Task 6: Documentation

**Files:**
- Modify: `docs/jobs.md` — Artifacts section (line ~1194-1223) and `call:` section (line ~630-673)

**Interfaces:**
- Consumes: final YAML surface from Tasks 1-5. No code.

- [ ] **Step 1: Update the Artifacts section**

In `docs/jobs.md`, replace the section intro line (currently "Upload and download files between jobs within the same or across runs.") with:

```markdown
Upload and download files between steps and jobs. By default artifacts are
scoped to the current run; a `downloadArtifact` step can fetch from another
run — most usefully a `call:` child run — with `runId:`.
```

Extend the example block's download step and add the cross-run subsection after the existing "Artifacts are stored in..." paragraph:

```markdown
### Downloading from another run (`runId`)

`downloadArtifact.runId` selects the run to download from. It is
template-expandable; combined with `{{ .Steps.<call-step>.ChildRunID }}`
(set on every successful `call:` step) it retrieves artifacts produced by a
called job:

​```yaml
steps:
  - name: build-app
    call:
      job: build
      with: { tag: "{{ .Params.tag }}" }

  - name: fetch-child-binary
    downloadArtifact:
      name: app-binary                              # name in the child run
      runId: "{{ .Steps.build-app.ChildRunID }}"    # default: current run
      destDir: artifacts
​```

- For matrix `call:` steps, `ChildRunID` aggregates per combination key,
  like outputs: `{{ index .Steps.build-app.ChildRunID "linux/amd64" }}`.
- The expanded `runId` must match `^[A-Za-z0-9_-]{1,64}$`; the expansion
  context excludes `Secrets` and `Stdout`. A template or validation failure
  fails the step.
- `runId` works on both the standard and Kubernetes agents.
​```
```

(Remove the stray trailing fence from the snippet above when editing; the zero-width characters around the inner fences are only there to survive this plan document.)

- [ ] **Step 2: Cross-link from the `call:` section**

After the paragraph "`call` steps wait for the called job to complete..." (line ~655), add:

```markdown
On success the child run's ID is available to later steps as
`{{ .Steps.<call-step-name>.ChildRunID }}` — see
[Downloading from another run (`runId`)](#downloading-from-another-run-runid)
for fetching the child's artifacts.
```

- [ ] **Step 3: Verify docs consistency**

Run: `grep -n "across runs" docs/jobs.md`
Expected: no hit claiming step-level cross-run downloads without `runId` (the human API/CLI subsection is fine as-is).

- [ ] **Step 4: Commit**

```bash
git add docs/jobs.md
git commit -m "docs: document downloadArtifact.runId and call-step ChildRunID"
```

---

### Task 7: Full verification

**Files:** none (verification only)

- [ ] **Step 1: Full local suite**

Run: `GOFLAGS=-buildvcs=false go build ./... && GOFLAGS=-buildvcs=false go vet ./... && GOFLAGS=-buildvcs=false go test ./... -count=1 2>&1 | tail -30`
Expected: all packages `ok`. Known transient: ubuntu dockertest `[setup failed]` flake does not apply locally; e2e tests may require docker — if an e2e package fails for environment (not logic) reasons, note it and confirm the affected package passes in CI later.

- [ ] **Step 2: Verify generated files are in sync**

Run: `GOFLAGS=-buildvcs=false go generate ./... && git status --short`
Expected: no diff (Task 1 already committed the regenerated files).

- [ ] **Step 3: Report**

Summarize: branch `download-artifact-runid`, commits list (`git log --oneline main..HEAD`), test results. Hand off to superpowers:finishing-a-development-branch (push, PR, wait for CI per merge discipline).
