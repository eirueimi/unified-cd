# Job-Level Isolation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Isolated-by-default jobs: the host agent runs an unmarked job's steps inside a per-claim "pod" (pause container + shared netns + eager sidecars + bind-mounted workspace), `spec.native: true` opts a job into today's host-process execution, and step-level `runsIn.image` is deleted.

**Architecture:** The shared orchestrator (`RunClaim`/`ExecBackend`) is unchanged except for deleting `RunImage`. All new behavior lives in the host backend: a `claimPodManager` (pause + eager containers, k8s-pod emulation via `--network container:`) replaces the lazy `namedContainerManager`, and `executeRun` branches native/isolated on the new `ClaimResponse.Native`. k8s gains only native-rejection. Parity is enforced by a new `paritycases` case.

**Tech Stack:** Go, docker/podman/nerdctl CLI (via `internal/runtime`), k8s client-go (k8s agent), testify.

**Spec:** [docs/superpowers/specs/2026-07-08-job-isolation-design.md](../specs/2026-07-08-job-isolation-design.md)

## Global Constraints

- Breaking changes are accepted and expected; every removal gets a clear migration error message.
- `apiVersion: unified-cd/v1` stays unchanged.
- Primary container name is exactly `"job"` (k8s exec fallback, `internal/k8sagent/executor.go:54`).
- Default runner image fallback: `ghcr.io/eirueimi/unified-cd-runner:v0.0.3` (same as k8s `defaultPodSpec`).
- Workspace mount path default `/workspace`, overridable by `podTemplate.workspace.mountPath` (existing `hostNamedMountPath`).
- Apple `container` runtime is NOT supported for isolated jobs (no `--network container:` support) — hard error.
- Run `go build ./...` and the package's tests after each task; run `go test ./...` before the final task.
- All commits on a feature branch (e.g. `feat/job-isolation`), commit after every task.

---

### Task 1: DSL — `spec.native`, restrict step-level `runsIn:` to uses entries

**Files:**
- Modify: `internal/dsl/types.go` (Spec struct ~line 23-37; RunsIn doc ~line 159-176)
- Modify: `internal/dsl/parse.go` (`normalizeRunsIn` ~line 279; `Validate` step loops ~lines 225-275; `checkForbiddenJobFields` ~line 67)
- Test: `internal/dsl/native_test.go` (create), `internal/dsl/runsin_test.go` (modify)
- Modify: `schemas/unified-cd.schema.json` (add `native`, remove step-level `runsIn` except on uses entries)

**Interfaces:**
- Produces: `dsl.Spec.Native bool` (`yaml:"native,omitempty" json:"native,omitempty"`); validation guarantees: a non-uses step never has `RunsIn` set after a successful `Parse`; a uses entry's `RunsIn` may only have `Image` set; `native: true` is incompatible with `podTemplate` and with any step `container:`.
- Consumes: existing `Spec`, `StepEntry`, `Step`, `RunsIn` types.

- [ ] **Step 1: Write failing tests**

```go
// internal/dsl/native_test.go
package dsl

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const nativeJobYAML = `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: ios-release }
spec:
  native: true
  steps:
    - name: build
      run: xcodebuild
`

func TestParse_NativeTrue(t *testing.T) {
	j, err := Parse(strings.NewReader(nativeJobYAML))
	require.NoError(t, err)
	assert.True(t, j.Spec.Native)
}

func TestValidate_NativeRejectsPodTemplate(t *testing.T) {
	y := `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: bad }
spec:
  native: true
  podTemplate:
    spec:
      containers: [{ name: mysql, image: mysql:8 }]
  steps:
    - name: s
      run: echo hi
`
	_, err := Parse(strings.NewReader(y))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "native")
	assert.Contains(t, err.Error(), "podTemplate")
}

func TestValidate_NativeRejectsContainerStep(t *testing.T) {
	y := `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: bad }
spec:
  native: true
  steps:
    - name: s
      container: mysql
      run: echo hi
`
	_, err := Parse(strings.NewReader(y))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "native")
	assert.Contains(t, err.Error(), "container")
}

func TestValidate_StepRunsInRejected(t *testing.T) {
	y := `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: bad }
spec:
  steps:
    - name: s
      runsIn: { image: golang:1.22 }
      run: go build
`
	_, err := Parse(strings.NewReader(y))
	require.Error(t, err)
	// migration hint
	assert.Contains(t, err.Error(), "runsIn")
	assert.Contains(t, err.Error(), "container:")
}

func TestValidate_UsesRunsInImageStillAllowed(t *testing.T) {
	y := `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: ok }
spec:
  steps:
    - name: tpl
      uses: { source: "git::https://example.com/x.git//tpl.yaml" }
      runsIn: { image: golang:1.22 }
`
	_, err := Parse(strings.NewReader(y))
	require.NoError(t, err)
}

func TestValidate_UsesRunsInContainerRejected(t *testing.T) {
	y := `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: bad }
spec:
  steps:
    - name: tpl
      uses: { source: "git::https://example.com/x.git//tpl.yaml" }
      runsIn: { container: mysql }
`
	_, err := Parse(strings.NewReader(y))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runsIn.container")
}
```

Adjust `internal/dsl/runsin_test.go`: existing tests asserting plain-step
`runsIn:` parses successfully must be inverted to expect the new error;
tests for `container:` (flat) must keep passing. Keep the
`runsIn.image`+`runsIn.container` mutual-exclusion test but move its subject
to a uses entry.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/dsl/ -run 'TestParse_Native|TestValidate_Native|TestValidate_StepRunsIn|TestValidate_Uses' -v`
Expected: FAIL (`Spec` has no field `Native`; runsIn currently accepted on plain steps)

- [ ] **Step 3: Implement**

In `internal/dsl/types.go`, add to `Spec` (after `PodTemplate`):

```go
	// Native opts the whole job into host-process execution (no claim pod,
	// no podTemplate, no container: steps). Host agents only; the default
	// (false) is the isolated pod model on both backends.
	Native bool `yaml:"native,omitempty" json:"native,omitempty"`
```

Update the `RunsIn` doc comment: it is now legal ONLY on a `uses:` entry, in
the `image` form (declares the template's isolated scope).

In `internal/dsl/parse.go`, replace `normalizeRunsIn` with:

```go
// checkStepExecTarget enforces the post-2026-07-08 rules: a plain step may
// use container: (canonical); step-level runsIn: is removed. A uses: entry
// may carry runsIn.image (the template's isolated scope) but nothing else.
func checkStepExecTarget(container string, runsIn *RunsIn, isUses bool, path, name string) error {
	if runsIn == nil {
		return nil
	}
	if !isUses {
		return fmt.Errorf("%s (%s): step-level runsIn: is no longer supported — use container: <podTemplate container name>, or move image isolation to the job's podTemplate or a uses: template", path, name)
	}
	if runsIn.Container != "" {
		return fmt.Errorf("%s (%s): runsIn.container is not valid on a uses: step — set container: on the template's steps instead", path, name)
	}
	if runsIn.Image == "" {
		return fmt.Errorf("%s (%s): uses runsIn: requires image:", path, name)
	}
	if container != "" {
		return fmt.Errorf("%s (%s): cannot set both container: and runsIn:", path, name)
	}
	return nil
}
```

At the two former `normalizeRunsIn` call sites (parallel sub-steps ~line 231,
entries ~line 267) call `checkStepExecTarget(st.Container, st.RunsIn, st.Uses != nil, subPath, st.Name)`
(and the entry equivalent) and drop the `st.RunsIn = ri` assignment — the flat
`Container` field is now the canonical carrier and is no longer folded.

In `Job.Validate()`, after the existing spec-level checks add:

```go
	if j.Spec.Native {
		if j.Spec.PodTemplate != nil {
			return fmt.Errorf("spec.native: true is incompatible with spec.podTemplate — a native job runs host processes only")
		}
	}
```

and inside the step/parallel validation loops (same places as
`checkStepExecTarget`), when `j.Spec.Native` and the step has
`Container != ""`, return
`fmt.Errorf("%s (%s): container: requires an isolated job — remove spec.native", path, name)`.

In `schemas/unified-cd.schema.json`: add `"native": {"type": "boolean"}` to
the spec properties; remove `runsIn` from the plain-step definition (keep it,
image-only, on the uses-entry definition if the schema distinguishes them —
mirror whatever structure the schema file already uses for step entries).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/dsl/ -v`
Expected: PASS (including adjusted runsin_test.go)

- [ ] **Step 5: Commit**

```bash
git add internal/dsl schemas/unified-cd.schema.json
git commit -m "feat(dsl)!: spec.native; step-level runsIn removed (container: is canonical)"
```

---

### Task 2: gittemplate — inline expansion without step-level runsIn

**Files:**
- Modify: `internal/gittemplate/inline.go` (~lines 195-240 shown above, plus `checkScopeStepAllowed` and wherever `scopeMode`/`outerRunsIn` are derived)
- Test: `internal/gittemplate/inline_runsin_test.go`, `internal/gittemplate/inline_scope_test.go` (modify)

**Interfaces:**
- Consumes: Task 1's rule (uses entries carry `RunsIn.Image` only).
- Produces: inline expansion emits steps whose `RunsIn` is ALWAYS nil; scope mode (`ScopeID`/`ScopeImage`) unchanged; a uses entry's flat `Container` propagates to inner steps that have none; a template step carrying `runsIn:` is a resolve-time error with a migration hint.

- [ ] **Step 1: Write failing tests**

Add to `internal/gittemplate/inline_runsin_test.go` (follow the existing
test fixtures in that file for how templates are fed in):

```go
func TestInline_TemplateStepRunsInRejected(t *testing.T) {
	// a template whose inner step declares runsIn: {image: ...} must fail
	// resolve with a migration hint (step-level runsIn was removed).
	// Build the template fixture exactly like the existing scope tests do,
	// with one inner step carrying RunsIn.
	_, err := expandForTest(t, templateWithInnerRunsIn) // adapt to this file's existing helper
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runsIn")
	assert.Contains(t, err.Error(), "container:")
}

func TestInline_UsesContainerPropagatesToInnerSteps(t *testing.T) {
	// uses entry with Container: "tools" and a template of two plain steps →
	// both inlined steps get Container "tools"; an inner step that already
	// sets container: keeps its own.
}
```

Update existing tests that exercised non-scope `outerRunsIn` propagation
(`ns.RunsIn = outerRunsIn`) to the new flat-Container propagation.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/gittemplate/ -run TestInline -v`
Expected: FAIL

- [ ] **Step 3: Implement**

In `internal/gittemplate/inline.go`:

- Derive `scopeMode` solely from the uses entry's `RunsIn != nil && RunsIn.Image != ""` (Task 1 guarantees that is the only surviving form). Delete `outerRunsIn` propagation.
- In both the parallel and concrete-step branches, replace

```go
	} else {
		ns.RunsIn = ps.RunsIn
		if ns.RunsIn == nil {
			ns.RunsIn = outerRunsIn
		}
	}
```

with

```go
	} else if ns.Container == "" {
		ns.Container = outerContainer // the uses entry's flat container:, "" if unset
	}
```

- Before that branch, reject template steps that still carry `runsIn:`:

```go
	if ps.RunsIn != nil {
		return nil, fmt.Errorf("template step %q: step-level runsIn: is no longer supported — use container: (see 2026-07-08 job isolation)", ps.Name)
	}
```

(in scope mode, keep `checkScopeStepAllowed` but change its `runsIn`
parameter to the same nil-check plus the step's `Container` — a scoped
template still cannot contain container:/nested exec-target steps; preserve
that function's existing rejections, swapping its RunsIn checks for the
Container field.)

- Set `ns.RunsIn = nil` unconditionally in scope mode (as today, line 204/232).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/gittemplate/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/gittemplate
git commit -m "feat(gittemplate)!: inline expansion uses flat container:; template step runsIn rejected"
```

---

### Task 3: api + controller — `ClaimResponse.Native`, drop `ClaimStep.RunsIn`

**Files:**
- Modify: `internal/api/types.go` (ClaimResponse ~line 78-90; ClaimStep ~line 100-124)
- Modify: `internal/controller/api_agent.go` (buildClaimResponse ~line 142-155; step conversion ~lines 210-234)
- Modify: `internal/agent/agent_os.go` (whole file)
- Test: `internal/controller/api_agent_test.go` (adjust), `internal/agent/agent_os_test.go` (adjust)

**Interfaces:**
- Produces: `api.ClaimResponse.Native bool` (`json:"native,omitempty"`); `api.ClaimStep` has NO `RunsIn` field — `Container string` is the only exec-target field; `agentOSForStep(step, defaultOS)` returns "linux" when `step.ScopeID != "" || step.Container != ""`.
- Consumes: `dsl.Spec.Native` (Task 1).

- [ ] **Step 1: Write failing tests**

In `internal/controller/api_agent_test.go`, add:

```go
func TestBuildClaimResponse_ThreadsNative(t *testing.T) {
	spec := dsl.Spec{Native: true, Steps: []dsl.StepEntry{{Name: "s", Run: "true"}}}
	b, _ := json.Marshal(spec)
	resp, err := buildClaimResponse(&store.ClaimedRun{ID: "r1", JobName: "j", Spec: b})
	require.NoError(t, err)
	assert.True(t, resp.Native)
}

func TestBuildClaimResponse_StepContainerThreaded(t *testing.T) {
	spec := dsl.Spec{Steps: []dsl.StepEntry{{Name: "s", Run: "true", Container: "mysql"}}}
	b, _ := json.Marshal(spec)
	resp, err := buildClaimResponse(&store.ClaimedRun{ID: "r1", JobName: "j", Spec: b})
	require.NoError(t, err)
	require.NotNil(t, resp.Stages[0].Step)
	assert.Equal(t, "mysql", resp.Stages[0].Step.Container)
}
```

In `internal/agent/agent_os_test.go`, replace the RunsIn-based cases:

```go
func TestAgentOSForStep(t *testing.T) {
	assert.Equal(t, "linux", agentOSForStep(api.ClaimStep{ScopeID: "scope:build"}, runtime.GOOS))
	assert.Equal(t, "linux", agentOSForStep(api.ClaimStep{Container: "mysql"}, runtime.GOOS))
	assert.Equal(t, runtime.GOOS, agentOSForStep(api.ClaimStep{}, runtime.GOOS))
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/controller/ -run TestBuildClaimResponse -v; go test ./internal/agent/ -run TestAgentOSForStep -v`
Expected: FAIL (no Native field / compile errors referencing RunsIn)

- [ ] **Step 3: Implement**

`internal/api/types.go`: add `Native bool \`json:"native,omitempty"\`` to
`ClaimResponse` (after `PodTemplate`); delete the
`RunsIn *dsl.RunsIn` field from `ClaimStep` (line 114). `Container` stays.

`internal/controller/api_agent.go`: in `buildClaimResponse` add
`Native: spec.Native` to the `api.ClaimResponse` literal; delete the two
`RunsIn: st.RunsIn` / `RunsIn: entry.RunsIn` copies (lines ~211/231) — the
existing `Container:` copies already carry the exec target.

`internal/agent/agent_os.go`:

```go
// agentOSForStep reports the OS a step actually runs on, for the
// UNIFIED_AGENT_OS env var. A uses-scope step or a container: step executes
// in a Linux container regardless of backend, so it reports "linux"; every
// other step reports defaultOS (ExecBackend.DefaultAgentOS() — the host
// backend itself reports "linux" for an isolated claim, runtime.GOOS for a
// native one; k8s always "linux").
func agentOSForStep(step api.ClaimStep, defaultOS string) string {
	if step.ScopeID != "" || step.Container != "" {
		return "linux"
	}
	return defaultOS
}
```

Fix remaining compile errors from the `ClaimStep.RunsIn` removal in
non-agent packages ONLY mechanically here (`internal/k8sagent/agent.go`'s
`execContainer` becomes):

```go
func execContainer(s api.ClaimStep) string {
	return s.Container
}
```

(the orchestrator/backends still reference `step.RunsIn` — that is Task 6/7's
job; to keep this task compiling, update `internal/agent/orchestrator.go`'s
dispatch now, since it is a one-line change that belongs to this seam:
replace the two RunsIn cases (lines ~376-379) with a single

```go
				case step.Container != "":
					ec, runErr = b.RunNamedContainer(stepCtx, step, step.Container, expandedRun, extraEnv, stdoutTee, shippedStderr)
```

and the post-hook capture (line ~432-434) with `container := step.Container`.
Delete `RunImage` from the `ExecBackend` interface (`internal/agent/backend.go:15`)
and both trivial implementations (`hostBackend.RunImage`,
`k8sBackend.RunImage`), plus `RunStepContainer` (`internal/agent/runner.go:146`)
and its test `internal/agent/runner_container_test.go`, k8s
`buildImageStepPod`/`runImageStep`/`imageStepDeadline` and their tests
(`podbuilder_image_test.go`, `podbuilder_resources_test.go`,
`runimage_test.go`, `backend_runimage_test.go`) — keep `imageStepEnv` (still
used by `ensureScopePod`). Update `internal/k8sagent/fakebackend_test.go` and
`parity_k8s_test.go`'s `parityK8sBackend` to drop their `RunImage` methods.
Update `hostContainerLimits` callers: if it was only used by `RunImage`,
delete it too (check `internal/agent/agent_runsin_test.go` and prune tests
that exercised step-level runsIn.image).)

- [ ] **Step 4: Run build + tests**

Run: `go build ./... ; go test ./internal/api/ ./internal/controller/ ./internal/agent/ ./internal/k8sagent/ ./internal/dsl/`
Expected: PASS (some agent tests referencing deleted behavior were pruned in Step 3)

- [ ] **Step 5: Commit**

```bash
git add internal/api internal/controller internal/agent internal/k8sagent
git commit -m "feat(api)!: ClaimResponse.Native; ClaimStep.Container replaces RunsIn; RunImage seam deleted"
```

---

### Task 4: runtime — netns joining (`CreateSpec.NetworkContainer`)

**Files:**
- Modify: `internal/runtime/runtime.go` (CreateSpec ~line 28-42)
- Modify: `internal/runtime/ocicli.go` (createArgs ~line 70-89)
- Modify: `internal/runtime/apple.go` (Create — reject NetworkContainer)
- Test: `internal/runtime/ocicli_create_test.go` (extend existing argv tests; follow the existing `execCommand` capture seam)

**Interfaces:**
- Produces: `CreateSpec.NetworkContainer string` — when non-empty, `ociCLI.Create` argv contains `--network container:<id>`; `appleContainer.Create` returns an error mentioning "not supported".

- [ ] **Step 1: Write failing test**

```go
func TestCreateArgs_NetworkContainer(t *testing.T) {
	r := &ociCLI{bin: "docker"}
	args := r.createArgs(CreateSpec{Image: "busybox", NetworkContainer: "abc123"})
	assert.Contains(t, strings.Join(args, " "), "--network container:abc123")
}

func TestCreateArgs_NoNetworkByDefault(t *testing.T) {
	r := &ociCLI{bin: "docker"}
	args := r.createArgs(CreateSpec{Image: "busybox"})
	assert.NotContains(t, strings.Join(args, " "), "--network")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/runtime/ -run TestCreateArgs_Network -v`
Expected: FAIL (unknown field NetworkContainer)

- [ ] **Step 3: Implement**

`runtime.go` — add to `CreateSpec`:

```go
	// NetworkContainer joins the created container into another container's
	// network namespace (docker/podman/nerdctl `--network container:<id>`).
	// Used by the host agent's claim pod: every claim container joins the
	// pause container's netns so sidecars are reachable on localhost,
	// mirroring a k8s pod. Empty = default network.
	NetworkContainer string
```

`ocicli.go` — in `createArgs`, after the mem/cpu flags:

```go
	if spec.NetworkContainer != "" {
		args = append(args, "--network", "container:"+spec.NetworkContainer)
	}
```

`apple.go` — at the top of `Create`:

```go
	if spec.NetworkContainer != "" {
		return ContainerHandle{}, fmt.Errorf("apple container: joining another container's network namespace is not supported (claim pod requires docker/podman/nerdctl)")
	}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/runtime/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/runtime
git commit -m "feat(runtime): CreateSpec.NetworkContainer joins a container netns"
```

---

### Task 5: workspace — per-job dirs, mode marker, cleanWorkspace, EPERM fallback

**Files:**
- Create: `internal/agent/workspace.go`
- Modify: `internal/agent/agent.go` (runLoop ~line 226-256)
- Test: `internal/agent/workspace_test.go` (create), `internal/agent/agent_test.go` (`TestAgent_CleanWorkspace` ~line 259 — adjust expected path)

**Interfaces:**
- Produces:
  - `sanitizeJobName(name string) string` — maps any rune outside `[A-Za-z0-9._-]` to `-`; empty result becomes `"job"`.
  - `claimWorkDir(wsBase string, slot int, jobName string) string` = `filepath.Join(wsBase, fmt.Sprintf("working%d", slot), sanitizeJobName(jobName))`.
  - `prepareWorkspace(ctx context.Context, workDir, mode string, clean bool, rtFn func() (crt.ContainerRuntime, error)) error` — mode is `"native"` or `"isolated"`; handles marker mismatch + cleaning + EPERM fallback; always ensures workDir exists and writes the marker.
- Consumes: `crt.ContainerRuntime` (Run for the cleanup container).

- [ ] **Step 1: Write failing tests**

```go
// internal/agent/workspace_test.go
package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
)

func TestSanitizeJobName(t *testing.T) {
	assert.Equal(t, "integration-test", sanitizeJobName("integration-test"))
	assert.Equal(t, "a-b-c", sanitizeJobName("a/b:c"))
	assert.Equal(t, "job", sanitizeJobName(""))
}

func TestClaimWorkDir(t *testing.T) {
	got := claimWorkDir("/base", 1, "my-job")
	assert.Equal(t, filepath.Join("/base", "working1", "my-job"), got)
}

func TestPrepareWorkspace_CreatesDirAndMarker(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "w")
	require.NoError(t, prepareWorkspace(context.Background(), dir, "isolated", false, noRuntime))
	b, err := os.ReadFile(filepath.Join(dir, ".ucd-mode"))
	require.NoError(t, err)
	assert.Equal(t, "isolated", string(b))
}

func TestPrepareWorkspace_KeepsFilesWhenNoClean(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("x"), 0o644))
	require.NoError(t, prepareWorkspace(context.Background(), dir, "native", false, noRuntime))
	_, err := os.Stat(filepath.Join(dir, "keep.txt"))
	assert.NoError(t, err)
}

func TestPrepareWorkspace_CleanRemovesFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stale.txt"), []byte("x"), 0o644))
	require.NoError(t, prepareWorkspace(context.Background(), dir, "native", true, noRuntime))
	_, err := os.Stat(filepath.Join(dir, "stale.txt"))
	assert.True(t, os.IsNotExist(err))
}

func TestPrepareWorkspace_ModeFlipResets(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, prepareWorkspace(context.Background(), dir, "isolated", false, noRuntime))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "left.txt"), []byte("x"), 0o644))
	// flip to native, no clean requested → marker mismatch forces a reset
	require.NoError(t, prepareWorkspace(context.Background(), dir, "native", false, noRuntime))
	_, err := os.Stat(filepath.Join(dir, "left.txt"))
	assert.True(t, os.IsNotExist(err))
	b, _ := os.ReadFile(filepath.Join(dir, ".ucd-mode"))
	assert.Equal(t, "native", string(b))
}

func noRuntime() (crt.ContainerRuntime, error) { return nil, os.ErrNotExist }
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agent/ -run 'TestSanitizeJobName|TestClaimWorkDir|TestPrepareWorkspace' -v`
Expected: FAIL (undefined functions)

- [ ] **Step 3: Implement `internal/agent/workspace.go`**

```go
package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	crt "github.com/eirueimi/unified-cd/internal/runtime"
)

// modeMarkerFile records which execution mode (native|isolated) last used a
// per-job workspace directory. A mismatch on the next claim (the job
// definition flipped native↔isolated) forces a directory reset so root-owned
// leftovers from a previous isolated run can never break a native run.
const modeMarkerFile = ".ucd-mode"

// sanitizeJobName makes a job name safe as a single path segment. Job names
// are already restricted by DSL validation; this is a defensive escape.
func sanitizeJobName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "job"
	}
	return b.String()
}

// claimWorkDir is the per-claim workspace directory: slot level (fixed,
// bounded — the claim-loop concurrency dimension) then job level (so two
// jobs never share a directory and carry-over is always "this job's own
// previous state"). See the 2026-07-08 job-isolation design.
func claimWorkDir(wsBase string, slot int, jobName string) string {
	return filepath.Join(wsBase, fmt.Sprintf("working%d", slot), sanitizeJobName(jobName))
}

// prepareWorkspace readies workDir for a claim running in mode
// ("native"|"isolated"): resets the directory when cleaning is requested OR
// the recorded mode flipped, falling back to a root cleanup container when a
// plain RemoveAll hits permission errors (root-owned files written by
// rootful-docker containers), then ensures the directory exists and records
// the mode marker.
func prepareWorkspace(ctx context.Context, workDir, mode string, clean bool, rtFn func() (crt.ContainerRuntime, error)) error {
	prev, _ := os.ReadFile(filepath.Join(workDir, modeMarkerFile))
	flipped := len(prev) > 0 && string(prev) != mode
	if clean || flipped {
		if flipped && !clean {
			slog.Info("workspace mode changed; resetting directory", "dir", workDir, "from", string(prev), "to", mode)
		}
		if err := os.RemoveAll(workDir); err != nil {
			slog.Warn("workspace clean failed; retrying via cleanup container", "dir", workDir, "error", err)
			if cerr := containerCleanup(ctx, workDir, rtFn); cerr != nil {
				slog.Warn("cleanup container failed; proceeding with dirty workspace", "dir", workDir, "error", cerr)
			} else if err := os.RemoveAll(workDir); err != nil {
				slog.Warn("workspace clean still failing after cleanup container", "dir", workDir, "error", err)
			}
		}
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return fmt.Errorf("create workspace %s: %w", workDir, err)
	}
	if err := os.WriteFile(filepath.Join(workDir, modeMarkerFile), []byte(mode), 0o644); err != nil {
		slog.Warn("write workspace mode marker failed", "dir", workDir, "error", err)
	}
	return nil
}

// containerCleanup deletes workDir's contents as root via a throwaway
// container, for files a rootful container runtime left owned by root.
func containerCleanup(ctx context.Context, workDir string, rtFn func() (crt.ContainerRuntime, error)) error {
	rt, err := rtFn()
	if err != nil {
		return fmt.Errorf("no container runtime for cleanup: %w", err)
	}
	h, err := rt.Create(ctx, crt.CreateSpec{
		Image:   "busybox",
		WorkDir: "/w",
		Mounts:  []crt.Mount{{HostPath: workDir, ContainerPath: "/w"}},
	})
	if err != nil {
		return err
	}
	defer func() { _ = rt.Remove(ctx, h) }()
	ec, err := rt.Exec(ctx, h, crt.ExecSpec{Script: "rm -rf /w/* /w/.[!.]* /w/..?* 2>/dev/null; true"}, io.Discard, io.Discard)
	if err != nil {
		return err
	}
	if ec != 0 {
		return fmt.Errorf("cleanup container exited %d", ec)
	}
	return nil
}
```

Note: the EPERM fallback path itself is exercised via the fake runtime in
Task 6's fakes if desired; on Windows dev machines `RemoveAll` EPERM is hard
to simulate, so the unit tests above cover marker/clean/flip logic and the
fallback is additionally covered by the docker-gated integration test
(Task 8).

In `internal/agent/agent.go` `runLoop`, replace the workDir computation and
the `a.CleanWorkspace` block (lines ~228, 246-253):

```go
func (a *Agent) runLoop(claimCtx, runCtx context.Context, slot int, wsBase string) {
	for {
		if claimCtx.Err() != nil {
			return
		}
		resp, err := a.Client.Claim(claimCtx, a.ID, "30s", a.Labels)
		// ... (unchanged error/empty handling) ...
		workDir := claimWorkDir(wsBase, slot, resp.JobName)
		mode := "isolated"
		if resp.Native {
			mode = "native"
		}
		clean := a.CleanWorkspace || (resp.PodTemplate != nil && resp.PodTemplate.CleanWorkspace)
		if err := prepareWorkspace(runCtx, workDir, mode, clean, a.containerRuntime); err != nil {
			slog.Error("prepare workspace failed", "dir", workDir, "error", err)
			// fail the claim cleanly: FinishRun(Failed) via executeRun's
			// early-fail helper (see Task 7 — reuse failClaim there)
		}
		a.executeRun(runCtx, resp, workDir)
	}
}
```

Adjust `TestAgent_CleanWorkspace` (agent_test.go:259): the sentinel file now
lives under `working0/<jobname>/`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/agent/ -run 'TestSanitize|TestClaimWorkDir|TestPrepareWorkspace|TestAgent_CleanWorkspace' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/agent
git commit -m "feat(agent): per-job workspace dirs, mode marker, cleanup-container fallback"
```

---

### Task 6: claim pod manager (pause + eager containers + primary)

**Files:**
- Create: `internal/agent/claim_pod.go` (absorbs and replaces `internal/agent/named_container.go`'s manager; keep `containerDef`, `parseContainerDef`, `limitStrings`, `hostNamedMountPath` — move them here and delete `named_container.go`)
- Test: `internal/agent/claim_pod_test.go` (create; port relevant cases from `named_container_test.go` if present, then delete stale tests)

**Interfaces:**
- Consumes: `crt.ContainerRuntime` (Task 4's `NetworkContainer`), `dsl.PodTemplate`, `containerDef`/`parseContainerDef`.
- Produces:
  - `newClaimPodManager(rt crt.ContainerRuntime, workDir, mountPath, pauseImage, runnerImage string) *claimPodManager`
  - `(*claimPodManager) Start(ctx context.Context, pt *dsl.PodTemplate) error` — pause first, then every container def, eagerly.
  - `(*claimPodManager) Exec(ctx context.Context, container, script string, env []string, stdout, stderr io.Writer) (int, error)` — `container == ""` targets `"job"`.
  - `(*claimPodManager) CloseAll(ctx context.Context)` — containers then pause.
  - `claimContainerDefs(pt *dsl.PodTemplate, runnerImage string) []containerDef` — all podTemplate containers in order; injects `{Name: "job", Image: runnerImage}` if absent.
  - const `primaryContainerName = "job"`.

- [ ] **Step 1: Write failing tests**

```go
// internal/agent/claim_pod_test.go
package agent

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/eirueimi/unified-cd/internal/dsl"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
)

// podFakeRT records Create/Exec/Remove calls. Mirror the existing fakeRT in
// scope_test.go but capture CreateSpec per call and return handle IDs "c0",
// "c1", ... in order.
type podFakeRT struct {
	created []crt.CreateSpec
	execs   []struct {
		id     string
		script string
	}
	removed []string
}

func (f *podFakeRT) Name() string     { return "fake" }
func (f *podFakeRT) Available() bool  { return true }
func (f *podFakeRT) Pull(context.Context, string) error { return nil }
func (f *podFakeRT) Run(context.Context, crt.RunSpec, io.Writer, io.Writer) (int, error) {
	return 0, nil
}
func (f *podFakeRT) Create(_ context.Context, s crt.CreateSpec) (crt.ContainerHandle, error) {
	f.created = append(f.created, s)
	return crt.ContainerHandle{ID: fmtID(len(f.created) - 1)}, nil
}
func (f *podFakeRT) Exec(_ context.Context, h crt.ContainerHandle, s crt.ExecSpec, _, _ io.Writer) (int, error) {
	f.execs = append(f.execs, struct{ id, script string }{h.ID, s.Script})
	return 0, nil
}
func (f *podFakeRT) CopyIn(context.Context, crt.ContainerHandle, string, string) error  { return nil }
func (f *podFakeRT) CopyOut(context.Context, crt.ContainerHandle, string, string) error { return nil }
func (f *podFakeRT) Remove(_ context.Context, h crt.ContainerHandle) error {
	f.removed = append(f.removed, h.ID)
	return nil
}
func fmtID(i int) string { return "c" + string(rune('0'+i)) }

func mysqlTemplate() *dsl.PodTemplate {
	return &dsl.PodTemplate{Spec: map[string]any{
		"containers": []any{
			map[string]any{"name": "mysql", "image": "mysql:8"},
		},
	}}
}

func TestClaimPod_StartPauseFirstThenEager(t *testing.T) {
	f := &podFakeRT{}
	m := newClaimPodManager(f, "/host/w", "/workspace", "pause:img", "runner:img")
	require.NoError(t, m.Start(context.Background(), mysqlTemplate()))

	require.Len(t, f.created, 3) // pause, mysql, injected "job"
	pause := f.created[0]
	assert.Equal(t, "pause:img", pause.Image)
	assert.Empty(t, pause.NetworkContainer)
	assert.Empty(t, pause.Mounts, "pause carries no workspace mount")

	for _, spec := range f.created[1:] {
		assert.Equal(t, "c0", spec.NetworkContainer, "every claim container joins the pause netns")
		require.Len(t, spec.Mounts, 1)
		assert.Equal(t, "/host/w", spec.Mounts[0].HostPath)
		assert.Equal(t, "/workspace", spec.Mounts[0].ContainerPath)
		assert.Equal(t, "/workspace", spec.WorkDir)
	}
	assert.Equal(t, "mysql:8", f.created[1].Image)
	assert.Equal(t, "runner:img", f.created[2].Image, "job container injected from runner image")
}

func TestClaimPod_JobFromTemplateNotInjected(t *testing.T) {
	f := &podFakeRT{}
	pt := &dsl.PodTemplate{Spec: map[string]any{
		"containers": []any{
			map[string]any{"name": "job", "image": "golang:1.22"},
		},
	}}
	m := newClaimPodManager(f, "/w", "/workspace", "pause:img", "runner:img")
	require.NoError(t, m.Start(context.Background(), pt))
	require.Len(t, f.created, 2) // pause + job (no injection)
	assert.Equal(t, "golang:1.22", f.created[1].Image)
}

func TestClaimPod_NilTemplateGetsDefaultJob(t *testing.T) {
	f := &podFakeRT{}
	m := newClaimPodManager(f, "/w", "/workspace", "pause:img", "runner:img")
	require.NoError(t, m.Start(context.Background(), nil))
	require.Len(t, f.created, 2) // pause + injected job
	assert.Equal(t, "runner:img", f.created[1].Image)
}

func TestClaimPod_ExecTargets(t *testing.T) {
	f := &podFakeRT{}
	m := newClaimPodManager(f, "/w", "/workspace", "p", "r")
	require.NoError(t, m.Start(context.Background(), mysqlTemplate()))

	_, err := m.Exec(context.Background(), "", "echo default", nil, io.Discard, io.Discard)
	require.NoError(t, err)
	_, err = m.Exec(context.Background(), "mysql", "echo sidecar", nil, io.Discard, io.Discard)
	require.NoError(t, err)
	_, err = m.Exec(context.Background(), "nope", "x", nil, io.Discard, io.Discard)
	require.Error(t, err, "unknown container name")

	// default targeted the injected job container (created 3rd → id c2),
	// sidecar targeted mysql (id c1)
	assert.Equal(t, "c2", f.execs[0].id)
	assert.Equal(t, "c1", f.execs[1].id)
}

func TestClaimPod_CloseAllRemovesContainersThenPause(t *testing.T) {
	f := &podFakeRT{}
	m := newClaimPodManager(f, "/w", "/workspace", "p", "r")
	require.NoError(t, m.Start(context.Background(), mysqlTemplate()))
	m.CloseAll(context.Background())
	require.Len(t, f.removed, 3)
	assert.Equal(t, "c0", f.removed[len(f.removed)-1], "pause removed last")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agent/ -run TestClaimPod -v`
Expected: FAIL (undefined newClaimPodManager)

- [ ] **Step 3: Implement `internal/agent/claim_pod.go`**

Move `containerDef`, `containerSupportedFields`, `parseContainerDef`,
`limitStrings` from `named_container.go` verbatim, then add:

```go
const primaryContainerName = "job"

// claimContainerDefs returns every container the claim pod must run, in
// podTemplate order, injecting the default runner as the "job" (primary)
// container when the template does not define one. A nil podTemplate yields
// just the injected primary — the host twin of k8s defaultPodSpec.
func claimContainerDefs(pt *dsl.PodTemplate, runnerImage string) []containerDef {
	var defs []containerDef
	if pt != nil {
		containers, _ := pt.Spec["containers"].([]any)
		for _, raw := range containers {
			c, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			name, _ := c["name"].(string)
			if name == "" {
				continue
			}
			defs = append(defs, parseContainerDef(name, c))
		}
	}
	for _, d := range defs {
		if d.Name == primaryContainerName {
			return defs
		}
	}
	return append(defs, containerDef{Name: primaryContainerName, Image: runnerImage})
}

// claimPodManager emulates a k8s pod on the host runtime for one claim: a
// pause container owns the network namespace; every podTemplate container
// (plus the injected "job" primary) joins it via --network container: and
// bind-mounts the claim workspace. Sidecars are therefore reachable on
// localhost from every step, and two concurrent claims can never collide on
// ports (separate netns, nothing published).
type claimPodManager struct {
	rt          crt.ContainerRuntime
	workDir     string
	mountPath   string
	pauseImage  string
	runnerImage string

	mu    sync.Mutex
	pause crt.ContainerHandle
	open  map[string]crt.ContainerHandle // container name → handle
}

func newClaimPodManager(rt crt.ContainerRuntime, workDir, mountPath, pauseImage, runnerImage string) *claimPodManager {
	return &claimPodManager{rt: rt, workDir: workDir, mountPath: mountPath,
		pauseImage: pauseImage, runnerImage: runnerImage, open: map[string]crt.ContainerHandle{}}
}

// Start builds the claim pod eagerly: pause first (netns owner), then every
// container def. Sidecars must be listening before any step runs, which is
// why this is claim-start eager, not step-time lazy.
func (m *claimPodManager) Start(ctx context.Context, pt *dsl.PodTemplate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	pause, err := m.rt.Create(ctx, crt.CreateSpec{Image: m.pauseImage})
	if err != nil {
		return fmt.Errorf("claim pod: start pause container (image %q): %w", m.pauseImage, err)
	}
	m.pause = pause
	for _, def := range claimContainerDefs(pt, m.runnerImage) {
		h, err := m.rt.Create(ctx, crt.CreateSpec{
			Image:            def.Image,
			Env:              def.Env,
			CPULimit:         def.CPULimit,
			MemLimit:         def.MemLimit,
			WorkDir:          m.mountPath,
			Mounts:           []crt.Mount{{HostPath: m.workDir, ContainerPath: m.mountPath}},
			NetworkContainer: pause.ID,
		})
		if err != nil {
			m.closeAllLocked(ctx)
			return fmt.Errorf("claim pod: start container %q (image %q): %w", def.Name, def.Image, err)
		}
		m.open[def.Name] = h
	}
	return nil
}

// Exec runs script in the named claim-pod container; "" targets the primary
// ("job") container, mirroring k8s exec's empty-container fallback
// (internal/k8sagent/executor.go).
func (m *claimPodManager) Exec(ctx context.Context, container, script string, env []string, stdout, stderr io.Writer) (int, error) {
	if container == "" {
		container = primaryContainerName
	}
	m.mu.Lock()
	h, ok := m.open[container]
	m.mu.Unlock()
	if !ok {
		return -1, fmt.Errorf("container %q is not defined in the job's podTemplate", container)
	}
	return m.rt.Exec(ctx, h, crt.ExecSpec{Script: script, Env: env}, stdout, stderr)
}

func (m *claimPodManager) CloseAll(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeAllLocked(ctx)
}

func (m *claimPodManager) closeAllLocked(ctx context.Context) {
	for name, h := range m.open {
		if err := m.rt.Remove(ctx, h); err != nil {
			slog.Warn("claim pod teardown: container remove failed", "container", name, "error", err)
		}
	}
	m.open = map[string]crt.ContainerHandle{}
	if m.pause.ID != "" {
		if err := m.rt.Remove(ctx, m.pause); err != nil {
			slog.Warn("claim pod teardown: pause remove failed", "error", err)
		}
		m.pause = crt.ContainerHandle{}
	}
}
```

Keep `hostNamedMountPath` (in backend_host.go) as is — it is reused for the
mount path. Delete `named_container.go`'s `namedContainerManager` (the
`namedContainerDef` single-lookup helper dies with it; `container:`
resolution is now `m.open` membership). Port/delete its tests: WARN-on-
unsupported-field tests move to a `claimContainerDefs` test; delete
`named_container_integration_test.go` (superseded by Task 8's integration
test).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/agent/ -run 'TestClaimPod|TestParseContainerDef|TestClaimContainerDefs' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/agent
git commit -m "feat(agent): claimPodManager — pause netns, eager sidecars, primary job container"
```

---

### Task 7: host backend + executeRun — native/isolated branch

**Files:**
- Modify: `internal/agent/backend_host.go` (struct ~line 26; RunDefault ~91; RunNamedContainer ~136; RunPostHook ~316; CloseScopes ~176; DefaultAgentOS ~307; ResolveCachePath ~298; delete namedContainers/RunImage leftovers)
- Modify: `internal/agent/agent.go` (executeRun ~line 267; Agent struct ~line 60-70: add `PauseImage`, `RunnerImage string`)
- Modify: `cmd/agent/main.go` (flags/config for `pause-image`, `runner-image`), `internal/config/agent.go` (file fields `pauseImage`, `runnerImage`)
- Test: `internal/agent/backend_isolated_test.go` (create), adjust `internal/agent/agent_runsin_test.go` / `backend_host_test.go` (prune step-runsIn.image cases; keep container: dispatch cases, now against the claim pod)

**Interfaces:**
- Consumes: `claimPodManager` (Task 6), `ClaimResponse.Native` (Task 3), `prepareWorkspace` (Task 5).
- Produces: `newHostBackend(a, runID, workDir string, pod *claimPodManager) *hostBackend` (signature change: `pod` replaces `podTemplate`; nil pod = native claim). Behavior contract: pod != nil → RunDefault/RunNamedContainer/RunPostHook exec via pod, `DefaultAgentOS() == "linux"`, `ResolveCachePath` non-scoped = `resolveWorkspacePath(workDir, p)`; pod == nil → today's native behavior exactly.

- [ ] **Step 1: Write failing tests**

```go
// internal/agent/backend_isolated_test.go
package agent

import (
	"context"
	"io"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/eirueimi/unified-cd/internal/api"
)

func isolatedBackendForTest(t *testing.T) (*hostBackend, *podFakeRT) {
	t.Helper()
	f := &podFakeRT{}
	m := newClaimPodManager(f, "/w", "/workspace", "p", "r")
	require.NoError(t, m.Start(context.Background(), mysqlTemplate()))
	b := newHostBackend(&Agent{}, "run1", "/w", m)
	return b, f
}

func TestHostBackend_Isolated_RunDefaultExecsPrimary(t *testing.T) {
	b, f := isolatedBackendForTest(t)
	_, err := b.RunDefault(context.Background(), api.ClaimStep{Name: "s"}, "echo hi", nil, io.Discard, io.Discard)
	require.NoError(t, err)
	require.NotEmpty(t, f.execs)
	assert.Equal(t, "c2", f.execs[0].id) // injected "job" primary
}

func TestHostBackend_Isolated_RunNamedContainer(t *testing.T) {
	b, f := isolatedBackendForTest(t)
	_, err := b.RunNamedContainer(context.Background(), api.ClaimStep{}, "mysql", "echo hi", nil, io.Discard, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, "c1", f.execs[0].id)

	_, err = b.RunNamedContainer(context.Background(), api.ClaimStep{}, "nope", "x", nil, io.Discard, io.Discard)
	assert.Error(t, err)
}

func TestHostBackend_Isolated_DefaultAgentOSIsLinux(t *testing.T) {
	b, _ := isolatedBackendForTest(t)
	assert.Equal(t, "linux", b.DefaultAgentOS())
}

func TestHostBackend_Native_DefaultAgentOSIsHost(t *testing.T) {
	b := newHostBackend(&Agent{}, "run1", "/w", nil)
	assert.Equal(t, runtime.GOOS, b.DefaultAgentOS())
}

func TestHostBackend_Isolated_ResolveCachePathJoinsWorkDir(t *testing.T) {
	b, _ := isolatedBackendForTest(t)
	got := b.ResolveCachePath(ScopeHandle{}, "node_modules")
	assert.Equal(t, resolveWorkspacePath("/w", "node_modules"), got)
}

func TestHostBackend_Native_ResolveCachePathUnresolved(t *testing.T) {
	b := newHostBackend(&Agent{}, "run1", "/w", nil)
	assert.Equal(t, "node_modules", b.ResolveCachePath(ScopeHandle{}, "node_modules"))
}

func TestHostBackend_Isolated_PostHookRunsInStepContainer(t *testing.T) {
	b, f := isolatedBackendForTest(t)
	require.NoError(t, b.RunPostHook(context.Background(), ScopeHandle{}, "mysql", "echo post", nil))
	assert.Equal(t, "c1", f.execs[0].id)
	// container=="" post hook goes to the primary
	require.NoError(t, b.RunPostHook(context.Background(), ScopeHandle{}, "", "echo post2", nil))
	assert.Equal(t, "c2", f.execs[1].id)
}

func TestHostBackend_Isolated_CloseScopesTearsDownPod(t *testing.T) {
	b, f := isolatedBackendForTest(t)
	b.CloseScopes(context.Background())
	assert.NotEmpty(t, f.removed)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agent/ -run TestHostBackend -v`
Expected: FAIL (newHostBackend signature; behaviors missing)

- [ ] **Step 3: Implement**

`backend_host.go`:

```go
type hostBackend struct {
	a       *Agent
	runID   string
	workDir string
	// pod is the claim pod backing an ISOLATED claim (nil for native: true).
	// When set, default steps exec into its primary container, container:
	// steps into the named container, and DefaultAgentOS reports "linux".
	pod *claimPodManager

	scopesMu sync.Mutex
	scopes   *scopeManager

	masker *secrets.Masker
}

func newHostBackend(a *Agent, runID, workDir string, pod *claimPodManager) *hostBackend {
	return &hostBackend{a: a, runID: runID, workDir: workDir, pod: pod}
}

func (b *hostBackend) RunDefault(ctx context.Context, step api.ClaimStep, script string, env []string, stdout, stderr io.Writer) (int, error) {
	if b.pod != nil {
		return b.pod.Exec(ctx, "", script, env, stdout, stderr)
	}
	return RunStep(ctx, script, stdout, stderr, env, b.workDir)
}

func (b *hostBackend) RunNamedContainer(ctx context.Context, step api.ClaimStep, container, script string, env []string, stdout, stderr io.Writer) (int, error) {
	if b.pod == nil {
		return -1, fmt.Errorf("container: %q requires an isolated job (this claim is native)", container)
	}
	return b.pod.Exec(ctx, container, script, env, stdout, stderr)
}

func (b *hostBackend) DefaultAgentOS() string {
	if b.pod != nil {
		return "linux"
	}
	return runtime.GOOS
}

func (b *hostBackend) ResolveCachePath(scope ScopeHandle, p string) string {
	if !scope.IsZero() {
		return resolveScopePath(p)
	}
	if b.pod != nil {
		// isolated claims are pod-semantics: resolve against the claim
		// workspace like the k8s backend joins the pod mount path (the bind
		// mount makes host workDir and in-container mountPath the same tree).
		return resolveWorkspacePath(b.workDir, p)
	}
	return p // native: legacy as-authored behavior
}

func (b *hostBackend) RunPostHook(ctx context.Context, scope ScopeHandle, container, script string, env []string) error {
	if sm, h, ok := unwrapHostScope(scope); ok {
		_, err := sm.exec(ctx, h, script, env, nil, nil)
		return err
	}
	if b.pod != nil {
		_, err := b.pod.Exec(ctx, container, script, env, nil, nil)
		return err
	}
	_, _, err := RunStepCapture(ctx, script, nil, env, b.workDir)
	return err
}

func (b *hostBackend) CloseScopes(ctx context.Context) {
	// ... existing scopes teardown unchanged ...
	if b.pod != nil {
		b.pod.CloseAll(ctx)
	}
}
```

Delete `namedContainers()`, `namedMu`/`named` fields, and the old
`RunNamedContainer`/`RunPostHook` named-container paths. Note the isolated
post-hook with `container==""` now runs in the PRIMARY container (was: host
process) — pod semantics, matching k8s where every default exec targets the
"job" container.

`agent.go` — Agent struct gains:

```go
	// PauseImage / RunnerImage back isolated claims' pods: PauseImage holds
	// the claim netns; RunnerImage is the injected "job" primary when the
	// podTemplate defines none (host twin of the k8s fallback image).
	PauseImage  string
	RunnerImage string
```

`executeRun` replacement:

```go
func (a *Agent) executeRun(ctx context.Context, c api.ClaimResponse, workDir string) {
	failClaim := func(msg string, err error) {
		slog.Error(msg, "runId", c.RunID, "error", err)
		retryUntilSuccess(ctx, func(cc context.Context) error {
			return a.Client.FinishRun(cc, a.ID, c.RunID, api.RunFailed)
		})
	}

	var pod *claimPodManager
	if !c.Native {
		rt, err := a.containerRuntime()
		if err != nil {
			failClaim("isolated job requires a container runtime (docker/podman/nerdctl); mark the job native: true or route it via agentSelector", err)
			return
		}
		pod = newClaimPodManager(rt, workDir, hostNamedMountPath(c.PodTemplate), a.PauseImage, a.RunnerImage)
		if err := pod.Start(ctx, c.PodTemplate); err != nil {
			pod.CloseAll(context.WithoutCancel(ctx))
			failClaim("claim pod construction failed", err)
			return
		}
	}
	backend := newHostBackend(a, c.RunID, workDir, pod)
	RunClaim(ctx, a.Client, a.ID, c, backend)
}
```

(The old podTemplate-WARN block is deleted — a podTemplate on an isolated
claim is now first-class. Verify `retryUntilSuccess` is accessible here; it
lives in this package per orchestrator.go usage.)

Defaults in `cmd/agent/main.go` (mirror the existing `clean-workspace`
flag/config pattern at line ~56/117):

```go
pauseImage := flag.String("pause-image", orDefault(eff.PauseImage, "busybox:1.36"), "image for the claim pod's pause (netns-holder) container")
runnerImage := flag.String("runner-image", orDefault(eff.RunnerImage, "ghcr.io/eirueimi/unified-cd-runner:v0.0.3"), "default primary container image for isolated jobs without a podTemplate job container")
```

with matching `PauseImage`/`RunnerImage string` fields in
`internal/config/agent.go`'s file struct + effective-config merge, and
`a.PauseImage = *pauseImage` / `a.RunnerImage = *runnerImage` wiring.
(Write `orDefault(s, def string) string` inline in main.go if absent.)

- [ ] **Step 4: Run build + tests**

Run: `go build ./... ; go test ./internal/agent/ ./internal/k8sagent/ ./internal/config/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/agent internal/config cmd/agent
git commit -m "feat(agent)!: isolated claims run in the claim pod; native: true keeps host processes"
```

---

### Task 8: k8s native rejection + host integration test

**Files:**
- Modify: `internal/k8sagent/agent.go` (top of the claim-execution func, where `c api.ClaimResponse` is first handled — same place the pod is acquired)
- Test: `internal/k8sagent/agent_native_test.go` (create; follow the fake-client pattern of `agent_env_test.go`)
- Create: `internal/agent/claim_pod_integration_test.go` (docker/podman-gated; follow `agent_scope_integration_test.go`'s skip pattern)

**Interfaces:**
- Consumes: `ClaimResponse.Native` (Task 3), full host stack (Task 7).
- Produces: k8s agent finishes a native claim as `RunFailed` without creating a pod.

- [ ] **Step 1: Write failing k8s test**

```go
func TestK8sExecuteRun_NativeClaimFailsFast(t *testing.T) {
	// Arrange the same fake client/pod-manager harness as
	// TestExecuteRun_DefaultStep_EnvInjected (agent_env_test.go), with
	// c := api.ClaimResponse{RunID: "r1", Native: true, Stages: ...one step...}
	// Act: run the claim through the k8s agent's executeRun equivalent.
	// Assert: FinishRun was called with api.RunFailed AND the fake pod
	// manager recorded zero CreatePod calls.
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/k8sagent/ -run TestK8sExecuteRun_Native -v`
Expected: FAIL (claim proceeds / pod created)

- [ ] **Step 3: Implement rejection**

At the top of the k8s claim execution path (before pod acquisition):

```go
	if c.Native {
		slog.Error("native: true jobs are host-only; the k8s agent cannot run them", "runId", c.RunID)
		agentlib.RetryUntilSuccess(ctx, func(cc context.Context) error {
			return a.client.FinishRun(cc, a.cfg.AgentID, c.RunID, api.RunFailed)
		})
		return
	}
```

(match the file's actual retry/finish helpers — the k8s agent already
finishes failed claims somewhere in this function; reuse that exact call
shape.)

- [ ] **Step 4: Write the host integration test**

```go
//go:build integration_container

// internal/agent/claim_pod_integration_test.go
// Gated exactly like agent_scope_integration_test.go (same build tag /
// runtime.Detect skip). Uses busybox only.
package agent

// TestClaimPod_Integration_SidecarLocalhostAndWorkspace:
//  podTemplate: containers:
//    - name: web
//      image: busybox
//      (keep-alive sleep infinity; the test step starts httpd itself)
//  claim (Native=false) with steps:
//    1. default step: echo hello > /workspace/hello.txt
//    2. container: web step: httpd -p 12080 -h /workspace (start daemon), then
//       wget -qO- http://localhost:12080/hello.txt from the DEFAULT step 3
//    3. default step: wget -qO- http://localhost:12080/hello.txt | grep hello
//       && echo "UNIFIED_AGENT_OS=$UNIFIED_AGENT_OS"
//  Assert: run Succeeded; step-3 stdout contains "hello" and
//  "UNIFIED_AGENT_OS=linux" (netns shared + workspace shared + OS var).
//
// TestClaimPod_Integration_ConcurrentClaimsNoPortCollision:
//  two claims of the same job shape, each: sidecar-less podTemplate with a
//  "job" container; step runs `httpd -p 12080 -h /workspace && sleep 2`.
//  Run both RunClaim calls concurrently (two claimPodManagers, two workDirs).
//  Assert: both runs Succeeded (isolated netns → same port, no collision).
```

Write these as real tests following `agent_scope_integration_test.go`'s
harness (fake controller client, real `runtime.Detect("")`, skip on error).

- [ ] **Step 5: Run tests**

Run: `go test ./internal/k8sagent/ -run TestK8sExecuteRun_Native -v`
Expected: PASS
Run (only on a machine with docker): `go test -tags integration_container ./internal/agent/ -run TestClaimPod_Integration -v`
Expected: PASS (or SKIP without a runtime)

- [ ] **Step 6: Commit**

```bash
git add internal/k8sagent internal/agent
git commit -m "feat(k8sagent): reject native claims; integration tests for the claim pod"
```

---

### Task 9: parity case — isolated dispatch

**Files:**
- Modify: `internal/paritycases/scenarios.go` (append a Case)
- Modify: `internal/agent/parity_host_test.go`, `internal/k8sagent/parity_k8s_test.go` (drivers: the host driver needs a fake container runtime for isolated claims; the k8s parity backend already fakes exec)

**Interfaces:**
- Consumes: `paritycases.Case`/`Expectation` (existing), fake runtimes.
- Produces: one shared case proving both backends dispatch identically: default step → primary/"job" container; `container: X` step → container X; outputs flow.

- [ ] **Step 1: Add the case (it will fail on the host driver first)**

Append to `scenarios.go`'s case list:

```go
	{
		Name: "isolated dispatch: default step hits the job container, container: hits the named one",
		Claim: func() api.ClaimResponse {
			return api.ClaimResponse{
				RunID: "run-isolated", JobName: "isolated-job",
				PodTemplate: &dsl.PodTemplate{Spec: map[string]any{
					"containers": []any{
						map[string]any{"name": "tools", "image": "busybox"},
					},
				}},
				Stages: []api.ClaimStage{
					{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "main",
						Run: "echo from-primary"}},
					{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "side",
						Container: "tools", Run: "echo from-tools"}},
				},
			}
		},
		Expect: Expectation{
			StepStatus:  map[string]string{"main": "Succeeded", "side": "Succeeded"},
			RunFinished: "Succeeded",
			LogMustContain: []LogLine{
				{Step: "main", Stream: "stdout", Substring: "from-primary"},
				{Step: "side", Stream: "stdout", Substring: "from-tools"},
			},
		},
	},
```

(add the `dsl` import to scenarios.go if missing.)

- [ ] **Step 2: Run both parity drivers to see the failure mode**

Run: `go test ./internal/agent/ -run TestParity -v; go test ./internal/k8sagent/ -run TestParity -v`
Expected: host driver FAILS (it constructs the backend natively / no fake runtime); k8s driver may already pass (exec is faked). Note exactly what breaks.

- [ ] **Step 3: Teach the host parity driver about isolated claims**

In `parity_host_test.go`'s driver, when the case's claim has `Native == false`,
build the backend the way `executeRun` does but with a fake runtime whose
`Exec` runs the script through the SAME local shell the driver already uses
for host steps (so `echo` output still flows into the captured logs), e.g. a
`shellFakeRT` implementing `crt.ContainerRuntime` where:
- `Create` returns sequential handles and records the CreateSpec,
- `Exec(h, spec, stdout, stderr)` invokes `RunStep(ctx, spec.Script, stdout, stderr, spec.Env, <driver temp dir>)`,
- `Remove` records teardown.

Then assert (inside the driver, after Assert): the exec'd container for step
"main" was the injected `"job"` handle and for "side" the `"tools"` handle —
mirroring k8s's `execContainer` fallback. Keep this assertion in the DRIVER
(both drivers can verify their own dispatch target; the shared Expectation
carries the observable log/status contract).

- [ ] **Step 4: Run both drivers**

Run: `go test ./internal/agent/ -run TestParity -v; go test ./internal/k8sagent/ -run TestParity -v`
Expected: PASS on both

- [ ] **Step 5: Commit**

```bash
git add internal/paritycases internal/agent internal/k8sagent
git commit -m "test(parity): isolated dispatch case runs on both backends"
```

---

### Task 10: full sweep, docs, migration guide

**Files:**
- Modify: `docs/agents.md`, `docs/jobs.md`, `docs/resources.md`, `docs/field-reference.md`, `docs/kubernetes-integration.md`, `docs/configuration.md`, `README.md` (where step runsIn / host podTemplate behavior is described)
- Create: `docs/migration-2026-07-job-isolation.md`
- Modify: `TODO.md` (drop superseded runsIn items), `cmd/docgen` output if field docs are generated (run it and commit the diff)

- [ ] **Step 1: Full test sweep**

Run: `go build ./... && go test ./...`
Expected: PASS everywhere. Fix any straggler references to
`ClaimStep.RunsIn`, `RunImage`, `namedContainerManager`, `normalizeRunsIn`
(grep for each; there must be zero hits outside docs/history).

- [ ] **Step 2: Write docs**

Cover, in the files listed (match each file's existing tone/structure):
- Isolated-by-default model + `native: true` (jobs.md, field-reference.md; YAML examples from the spec's Schema section).
- Claim pod on the host: pause/netns, sidecars on `localhost`, no published ports, supported runtimes (docker/podman/nerdctl; Apple `container` excluded), no readiness probes (steps retry themselves) (agents.md, kubernetes-integration.md).
- `container:` canonical / step `runsIn` removed / uses `runsIn.image` kept (field-reference.md, resources.md).
- Workspace: `working<slot>/<job>` layout, carry-over default, `podTemplate.cleanWorkspace` on host, `.ucd-mode` marker, operator GC responsibility, macOS/Windows file-sharing requirement, rootless-podman-first recommendation (configuration.md, agents.md).
- New agent flags/config: `pause-image`, `runner-image` (configuration.md).

`docs/migration-2026-07-job-isolation.md` — a table: before → after for
(1) unmarked host job → add `native: true` OR install a runtime,
(2) step `runsIn: {image: X}` → podTemplate container + `container:` or a uses template,
(3) step `runsIn: {container: X}` → `container: X`,
(4) `runsIn.resources` → `podTemplate.spec.containers[].resources.limits`.

- [ ] **Step 3: Commit**

```bash
git add docs TODO.md README.md
git commit -m "docs: job-level isolation — native opt-out, claim pod, migration guide"
```
