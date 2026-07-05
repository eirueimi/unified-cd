# runsIn uses-scope: artifacts & cache — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a `uses` template that runs in an isolated environment (uses-level `runsIn.image`) save and restore artifacts and cache from that isolated environment.

**Architecture:** A uses-level `runsIn.image` turns the whole inlined template into a "scope": one isolated environment (a long-lived container on host, a dedicated pod on k8s) with its own scratch filesystem, living for the template's steps. Steps are tagged at parse time with a shared `ScopeID`; the agent provisions the scope environment lazily on the scope's first step, routes that scope's run/artifact/cache steps into it, and tears it down at the scope boundary. Step-level `runsIn.image` is unchanged (single pure call).

**Tech Stack:** Go (module `github.com/eirueimi/unified-cd`); container runtime CLIs (docker/podman/nerdctl/wslc) via `internal/runtime`; Kubernetes client-go via `internal/k8sagent`. Design spec: `docs/superpowers/specs/2026-07-05-runsin-uses-scope-artifacts-design.md`.

## Global Constraints

- **English only** — all code, comments, commit messages, and docs (AGENTS.md).
- **Backend parity** — host agent and k8s agent must behave identically for scope semantics.
- **Isolation contract** — the scope filesystem is NEVER the outer job workspace. Inputs via `with:`/`download-artifact`; outputs via `upload-artifact` (run-scoped object store) / `outputs:`/stdout.
- **Scope trigger** — scope mode is entered ONLY by a uses-level `runsIn.image`. uses-level `runsIn.container` and step-level `runsIn` keep current behavior.
- **Nested runsIn** — an inner step declaring its own `runsIn.image`/`runsIn.container` inside a scoped `uses` is a parse error.
- **Failure policy** — `upload-artifact`/`download-artifact` in scope are fail-loud; `cache` in scope is warn+skip (matches existing per-backend policy).
- TDD, DRY, YAGNI, frequent commits. Run `make test-short` for unit tests (Docker not required); integration tests are Docker/envtest-gated.

## File Structure

**Phase 1 — DSL / inline / API (parse-time scope tagging):**
- Modify `internal/dsl/types.go` — add `ScopeID`, `ScopeImage` to `StepEntry` and `Step`.
- Modify `internal/gittemplate/inline.go` — scope tagging + nested-runsIn validation in `expandUsesStep`.
- Modify `internal/api/types.go` — add `ScopeID`, `ScopeImage` to `ClaimStep`.
- Modify the compile path that builds `api.ClaimStep` from `dsl` steps — copy the two fields through.

**Phase 2 — Container runtime lifecycle (host):**
- Modify `internal/runtime/runtime.go` — extend `ContainerRuntime` with `Create`/`Exec`/`CopyIn`/`CopyOut`/`Remove`; add `ContainerHandle`, `ExecSpec`.
- Modify `internal/runtime/ocicli.go` — implement the lifecycle for docker-compatible CLIs.
- Modify `internal/runtime/apple.go` — implement or explicitly reject the lifecycle for `appleContainer`.

**Phase 3 — host agent scope manager:**
- Create `internal/agent/scope.go` — `scopeManager` (provision / exec / artifact / cache / teardown against a runtime container).
- Modify `internal/agent/agent.go` — route steps carrying `ScopeID` through the scope manager.

**Phase 4 — k8s agent scope pod manager:**
- Create `internal/k8sagent/scopepod.go` — `buildScopePod` + scope pod lifecycle.
- Modify `internal/k8sagent/agent.go` — route scope steps into the scope pod (exec + sidecar artifact/cache).

**Phase 5 — parity & regression tests** (folded into the tasks above; final task validates cross-backend parity).

---

## Task 1: DSL scope fields

**Files:**
- Modify: `internal/dsl/types.go:80-126` (`StepEntry`, `Step`)
- Test: `internal/dsl/scope_fields_test.go` (create)

**Interfaces:**
- Produces: `StepEntry.ScopeID string`, `StepEntry.ScopeImage string`, and the same two fields on `Step`. YAML/JSON tags: `scopeID,omitempty` / `scopeImage,omitempty`. These are set by inline expansion (Task 2), never authored by users.

- [ ] **Step 1: Write the failing test**

```go
// internal/dsl/scope_fields_test.go
package dsl

import (
	"testing"

	"sigs.k8s.io/yaml"
)

func TestStepEntryScopeFieldsRoundTrip(t *testing.T) {
	se := StepEntry{Name: "x", Run: "true", ScopeID: "scope:build", ScopeImage: "golang:1.22"}
	b, err := yaml.Marshal(se)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got StepEntry
	if err := yaml.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ScopeID != "scope:build" || got.ScopeImage != "golang:1.22" {
		t.Fatalf("scope fields lost: %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dsl/ -run TestStepEntryScopeFieldsRoundTrip`
Expected: FAIL — `got.ScopeID` unknown field / compile error `unknown field ScopeID`.

- [ ] **Step 3: Add the fields**

In `internal/dsl/types.go`, add to BOTH `StepEntry` (after `RunsIn`, before `TimeoutMinutes`) and `Step`:

```go
	// Scope tagging: set by inline expansion when a uses-level runsIn.image
	// makes the whole template one isolated scope. Steps sharing ScopeID run
	// in one environment. Not user-authored.
	ScopeID    string `yaml:"scopeID,omitempty" json:"scopeID,omitempty"`
	ScopeImage string `yaml:"scopeImage,omitempty" json:"scopeImage,omitempty"`
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/dsl/ -run TestStepEntryScopeFieldsRoundTrip`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/dsl/types.go internal/dsl/scope_fields_test.go
git commit -m "feat(dsl): add ScopeID/ScopeImage step fields for uses scopes"
```

---

## Task 2: Inline scope tagging + nested-runsIn validation

**Files:**
- Modify: `internal/gittemplate/inline.go:86-226` (`expandUsesStep`)
- Test: `internal/gittemplate/inline_scope_test.go` (create)

**Interfaces:**
- Consumes: `StepEntry.ScopeID`/`ScopeImage` (Task 1); existing `expandUsesStep(usesName string, with map[string]string, tplSpec dsl.Spec, outerRunsIn *dsl.RunsIn) ([]dsl.StepEntry, error)`.
- Produces: when `outerRunsIn != nil && outerRunsIn.Image != ""` (scope mode), every expanded step (concrete + parallel members) gets `ScopeID = scopeIDFor(usesName)` and `ScopeImage = outerRunsIn.Image`, and its `RunsIn` is left nil. A helper `scopeIDFor(usesName string) string` returns `"scope:" + usesName`. In scope mode, an inner step with a non-nil `RunsIn` (Image or Container set) makes `expandUsesStep` return an error.

- [ ] **Step 1: Write the failing tests**

```go
// internal/gittemplate/inline_scope_test.go
package gittemplate

import (
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

func scopedTemplate() dsl.Spec {
	return dsl.Spec{Steps: []dsl.StepEntry{
		{Name: "compile", Run: "make build"},
		{Name: "save", UploadArtifact: &dsl.UploadArtifactStep{Name: "bin", Path: "./out"}},
	}}
}

func TestExpandUsesScopeTagsSteps(t *testing.T) {
	out, err := expandUsesStep("build", map[string]string{}, scopedTemplate(), &dsl.RunsIn{Image: "golang:1.22"})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	for _, s := range out {
		if s.Name == inputsStepName("build") {
			continue // the synthetic inputs step
		}
		if s.ScopeID != "scope:build" || s.ScopeImage != "golang:1.22" {
			t.Fatalf("step %q not scope-tagged: %+v", s.Name, s)
		}
		if s.RunsIn != nil {
			t.Fatalf("step %q should not carry RunsIn in scope mode: %+v", s.Name, s.RunsIn)
		}
	}
}

func TestExpandUsesNestedRunsInIsError(t *testing.T) {
	tpl := dsl.Spec{Steps: []dsl.StepEntry{
		{Name: "lint", Run: "golangci-lint run", RunsIn: &dsl.RunsIn{Image: "golangci/lint:latest"}},
	}}
	_, err := expandUsesStep("build", map[string]string{}, tpl, &dsl.RunsIn{Image: "golang:1.22"})
	if err == nil || !strings.Contains(err.Error(), "lint") {
		t.Fatalf("expected nested-runsIn error naming step, got %v", err)
	}
}

func TestExpandUsesContainerModeUnchanged(t *testing.T) {
	// uses-level runsIn.container is NOT scope mode: keep propagating RunsIn.
	out, err := expandUsesStep("build", map[string]string{}, scopedTemplate(), &dsl.RunsIn{Container: "builder"})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	for _, s := range out {
		if s.ScopeID != "" {
			t.Fatalf("container mode must not scope-tag: %+v", s)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/gittemplate/ -run TestExpandUses`
Expected: FAIL — steps are not scope-tagged; nested runsIn is silently propagated instead of erroring.

- [ ] **Step 3: Implement scope tagging + validation**

At the top of `expandUsesStep`, after the empty-steps guard (line ~89), add:

```go
	scopeMode := outerRunsIn != nil && outerRunsIn.Image != ""
	var scopeID, scopeImage string
	if scopeMode {
		scopeID = scopeIDFor(usesName)
		scopeImage = outerRunsIn.Image
	}
```

Add the helper near the other name helpers in this file:

```go
// scopeIDFor returns the scope identity shared by all steps expanded from a
// uses-level runsIn.image invocation. The agent keys the scope environment on
// (ScopeID, MatrixKey) so matrix variants get independent environments.
func scopeIDFor(usesName string) string { return "scope:" + usesName }
```

In BOTH the parallel-member branch (line ~163-166) and the concrete-step branch (line ~182-185), replace the current `RunsIn` propagation:

```go
			ns.RunsIn = ps.RunsIn        // (parallel) — existing
			if ns.RunsIn == nil {
				ns.RunsIn = outerRunsIn
			}
```

with scope-aware handling (parallel version shown; mirror it in the concrete branch using `inner` in place of `ps`):

```go
			if scopeMode {
				if ps.RunsIn != nil && (ps.RunsIn.Image != "" || ps.RunsIn.Container != "") {
					return nil, fmt.Errorf("step %q: runsIn is not allowed inside a uses running with runsIn.image (the scope is a single environment)", ps.Name)
				}
				ns.ScopeID = scopeID
				ns.ScopeImage = scopeImage
				ns.RunsIn = nil
			} else {
				ns.RunsIn = ps.RunsIn
				if ns.RunsIn == nil {
					ns.RunsIn = outerRunsIn
				}
			}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/gittemplate/ -run TestExpandUses`
Expected: PASS (all three)

- [ ] **Step 5: Run the package suite for regressions**

Run: `go test ./internal/gittemplate/`
Expected: PASS — existing `inline_runsin_test.go` (step-level and container propagation) still green.

- [ ] **Step 6: Commit**

```bash
git add internal/gittemplate/inline.go internal/gittemplate/inline_scope_test.go
git commit -m "feat(inline): tag uses-scope steps and reject nested runsIn"
```

---

## Task 3: Carry scope fields into ClaimStep

**Files:**
- Modify: `internal/api/types.go:91-113` (`ClaimStep`)
- Modify: the compile/flatten path that constructs `api.ClaimStep` from `dsl` steps (locate — see Step 1)
- Test: `internal/api/scope_claimstep_test.go` (create) + a compile test in the located package

**Interfaces:**
- Consumes: `dsl` `StepEntry.ScopeID`/`ScopeImage` (Task 1).
- Produces: `ClaimStep.ScopeID string` and `ClaimStep.ScopeImage string` (JSON `scopeID`/`scopeImage`), populated by the compile path from the corresponding dsl fields.

- [ ] **Step 1: Locate the compile path**

Run: `grep -rn "api.ClaimStep{" internal/ | grep -v _test`
Expected: one primary construction site (the job compiler that flattens dsl steps into `[]api.ClaimStep`). Note its file:line — later steps call it `<compiler>`.

- [ ] **Step 2: Write the failing test**

```go
// internal/api/scope_claimstep_test.go
package api

import (
	"encoding/json"
	"testing"
)

func TestClaimStepScopeFieldsJSON(t *testing.T) {
	cs := ClaimStep{Index: 0, Name: "compile", Run: "make", ScopeID: "scope:build", ScopeImage: "golang:1.22"}
	b, _ := json.Marshal(cs)
	var got ClaimStep
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ScopeID != "scope:build" || got.ScopeImage != "golang:1.22" {
		t.Fatalf("scope fields lost: %+v", got)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestClaimStepScopeFieldsJSON`
Expected: FAIL — unknown fields `ScopeID`/`ScopeImage`.

- [ ] **Step 4: Add the fields + wire the compiler**

In `internal/api/types.go` `ClaimStep`, after `RunsIn` (line 105):

```go
	ScopeID    string `json:"scopeID,omitempty"`
	ScopeImage string `json:"scopeImage,omitempty"`
```

In `<compiler>` (from Step 1), where each `api.ClaimStep` is built from a dsl step, copy the fields:

```go
		ScopeID:    src.ScopeID,
		ScopeImage: src.ScopeImage,
```

(`src` = the dsl `StepEntry`/`Step` being flattened; match the surrounding field-assignment style.)

- [ ] **Step 5: Add a compiler test**

In the compiler's package test file, add a case: compile a job whose step has `ScopeID`/`ScopeImage` set and assert the produced `ClaimStep` carries them. Model it on an existing compile test in that file (copy its setup, add the two assertions).

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/api/ && go test ./<compiler-package>/`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/api/types.go internal/api/scope_claimstep_test.go <compiler files>
git commit -m "feat(api): pass ScopeID/ScopeImage through to ClaimStep"
```

---

## Task 4: Container runtime lifecycle interface

**Files:**
- Modify: `internal/runtime/runtime.go:14-31`
- Test: `internal/runtime/lifecycle_contract_test.go` (create)

**Interfaces:**
- Produces, added to `ContainerRuntime`:
  - `Create(ctx context.Context, spec CreateSpec) (ContainerHandle, error)` — start a detached long-lived container (`run -d <image> sleep infinity`).
  - `Exec(ctx context.Context, h ContainerHandle, spec ExecSpec, stdout, stderr io.Writer) (int, error)` — run a script inside it.
  - `CopyIn(ctx context.Context, h ContainerHandle, hostPath, containerPath string) error` — host → container.
  - `CopyOut(ctx context.Context, h ContainerHandle, containerPath, hostPath string) error` — container → host.
  - `Remove(ctx context.Context, h ContainerHandle) error` — force-remove.
  - Types: `type ContainerHandle struct { ID string }`; `type CreateSpec struct { Image string; Env []string; CPULimit, MemLimit string }`; `type ExecSpec struct { Script string; Env []string; Shell []string }`.

- [ ] **Step 1: Write the failing test (interface shape via a compile-time assertion)**

```go
// internal/runtime/lifecycle_contract_test.go
package runtime

import (
	"context"
	"io"
	"testing"
)

// scopeRuntime is the subset of ContainerRuntime the agent scope manager needs.
type scopeRuntime interface {
	Create(ctx context.Context, spec CreateSpec) (ContainerHandle, error)
	Exec(ctx context.Context, h ContainerHandle, spec ExecSpec, stdout, stderr io.Writer) (int, error)
	CopyIn(ctx context.Context, h ContainerHandle, hostPath, containerPath string) error
	CopyOut(ctx context.Context, h ContainerHandle, containerPath, hostPath string) error
	Remove(ctx context.Context, h ContainerHandle) error
}

func TestOCICLISatisfiesScopeRuntime(t *testing.T) {
	var _ scopeRuntime = &ociCLI{bin: "docker"}
	var _ ContainerRuntime = &ociCLI{bin: "docker"}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/runtime/ -run TestOCICLISatisfiesScopeRuntime`
Expected: FAIL to compile — `ociCLI` does not implement the new methods; `CreateSpec`/`ExecSpec`/`ContainerHandle` undefined.

- [ ] **Step 3: Extend the interface and add types**

In `internal/runtime/runtime.go`, add types and extend the interface:

```go
// ContainerHandle identifies a running long-lived container (scope environment).
type ContainerHandle struct{ ID string }

// CreateSpec describes a detached long-lived container for a uses scope.
type CreateSpec struct {
	Image    string
	Env      []string // KEY=VALUE, injected as -e
	CPULimit string
	MemLimit string
}

// ExecSpec describes one script execution inside a running container.
type ExecSpec struct {
	Script string
	Env    []string // KEY=VALUE, injected as -e on exec
	Shell  []string // defaults to {"sh","-c"}
}
```

Extend `ContainerRuntime` (append to the interface body):

```go
	// Long-lived scope lifecycle (uses-level runsIn.image).
	Create(ctx context.Context, spec CreateSpec) (ContainerHandle, error)
	Exec(ctx context.Context, h ContainerHandle, spec ExecSpec, stdout, stderr io.Writer) (int, error)
	CopyIn(ctx context.Context, h ContainerHandle, hostPath, containerPath string) error
	CopyOut(ctx context.Context, h ContainerHandle, containerPath, hostPath string) error
	Remove(ctx context.Context, h ContainerHandle) error
```

(This will not compile until Tasks 5 and 6 implement the methods on `ociCLI` and `appleContainer` — that is expected; keep going.)

- [ ] **Step 4: Commit the interface (compiles after Tasks 5–6)**

Defer running until Task 6. For now:

```bash
git add internal/runtime/runtime.go internal/runtime/lifecycle_contract_test.go
git commit -m "feat(runtime): declare scope container lifecycle interface"
```

---

## Task 5: ociCLI lifecycle implementation

**Files:**
- Modify: `internal/runtime/ocicli.go`
- Test: `internal/runtime/ocicli_lifecycle_test.go` (create)

**Interfaces:**
- Consumes: `CreateSpec`, `ExecSpec`, `ContainerHandle` (Task 4).
- Produces: the five lifecycle methods on `*ociCLI`, shelling out to `<bin> run -d`, `<bin> exec`, `<bin> cp`, `<bin> rm -f`. A package var `execCommand = exec.CommandContext` is introduced so tests can capture argv without invoking a real runtime.

- [ ] **Step 1: Write the failing test (argv capture via injected execCommand)**

```go
// internal/runtime/ocicli_lifecycle_test.go
package runtime

import (
	"context"
	"os/exec"
	"testing"
)

func withFakeExec(t *testing.T, record *[][]string) {
	t.Helper()
	orig := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		*record = append(*record, append([]string{name}, args...))
		// `true` exists on the test host (Linux/macOS/Git-Bash) and exits 0.
		return orig(ctx, "true")
	}
	t.Cleanup(func() { execCommand = orig })
}

func TestOCICLICopyOutArgv(t *testing.T) {
	var rec [][]string
	withFakeExec(t, &rec)
	r := &ociCLI{bin: "docker"}
	if err := r.CopyOut(context.Background(), ContainerHandle{ID: "abc"}, "/out/app", "/tmp/app"); err != nil {
		t.Fatalf("CopyOut: %v", err)
	}
	got := rec[0]
	want := []string{"docker", "cp", "abc:/out/app", "/tmp/app"}
	if len(got) != len(want) {
		t.Fatalf("argv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv = %v, want %v", got, want)
		}
	}
}

func TestOCICLICopyInArgv(t *testing.T) {
	var rec [][]string
	withFakeExec(t, &rec)
	r := &ociCLI{bin: "podman"}
	if err := r.CopyIn(context.Background(), ContainerHandle{ID: "xyz"}, "/tmp/deps", "/work/deps"); err != nil {
		t.Fatalf("CopyIn: %v", err)
	}
	want := []string{"podman", "cp", "/tmp/deps", "xyz:/work/deps"}
	for i := range want {
		if rec[0][i] != want[i] {
			t.Fatalf("argv = %v, want %v", rec[0], want)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/runtime/ -run 'TestOCICLICopy'`
Expected: FAIL to compile — `execCommand`, `CopyOut`, `CopyIn` undefined.

- [ ] **Step 3: Implement the lifecycle**

In `internal/runtime/ocicli.go`, add the indirected exec var and methods (and switch `Run`/`Pull` to `execCommand` for consistency):

```go
// execCommand is indirected for testability.
var execCommand = exec.CommandContext

func (r *ociCLI) Create(ctx context.Context, spec CreateSpec) (ContainerHandle, error) {
	args := []string{"run", "-d"}
	if spec.CPULimit != "" {
		args = append(args, "--cpus", spec.CPULimit)
	}
	if spec.MemLimit != "" {
		args = append(args, "--memory", spec.MemLimit)
	}
	for _, e := range spec.Env {
		args = append(args, "-e", e)
	}
	args = append(args, spec.Image, "sleep", "infinity")
	out, err := execCommand(ctx, r.bin, args...).Output()
	if err != nil {
		return ContainerHandle{}, fmt.Errorf("%s run -d: %w", r.bin, err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return ContainerHandle{}, fmt.Errorf("%s run -d: empty container id", r.bin)
	}
	return ContainerHandle{ID: id}, nil
}

func (r *ociCLI) Exec(ctx context.Context, h ContainerHandle, spec ExecSpec, stdout, stderr io.Writer) (int, error) {
	args := []string{"exec"}
	for _, e := range spec.Env {
		args = append(args, "-e", e)
	}
	shell := spec.Shell
	if len(shell) == 0 {
		shell = []string{"sh", "-c"}
	}
	args = append(args, h.ID)
	args = append(args, shell...)
	args = append(args, spec.Script)
	cmd := execCommand(ctx, r.bin, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return -1, err
}

func (r *ociCLI) CopyIn(ctx context.Context, h ContainerHandle, hostPath, containerPath string) error {
	return execCommand(ctx, r.bin, "cp", hostPath, h.ID+":"+containerPath).Run()
}

func (r *ociCLI) CopyOut(ctx context.Context, h ContainerHandle, containerPath, hostPath string) error {
	return execCommand(ctx, r.bin, "cp", h.ID+":"+containerPath, hostPath).Run()
}

func (r *ociCLI) Remove(ctx context.Context, h ContainerHandle) error {
	return execCommand(ctx, r.bin, "rm", "-f", h.ID).Run()
}
```

Add imports `fmt` and `strings` to the file's import block.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/runtime/ -run 'TestOCICLICopy'`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/ocicli.go internal/runtime/ocicli_lifecycle_test.go
git commit -m "feat(runtime): implement scope lifecycle for docker-compatible CLIs"
```

---

## Task 6: appleContainer lifecycle parity

**Files:**
- Modify: `internal/runtime/apple.go`
- Test: `internal/runtime/apple_lifecycle_test.go` (create)

**Interfaces:**
- Consumes: Task 4 types.
- Produces: the five lifecycle methods on `*appleContainer`. If Apple's `container` CLI shares docker `cp`/`exec`/`run -d` grammar, reuse the same argv; otherwise implement per its grammar. This task's job is to make the package compile against the extended interface and keep `Detect("container")` viable.

- [ ] **Step 1: Read the existing driver**

Read `internal/runtime/apple.go` fully. Determine whether `appleContainer` wraps the same argv as `ociCLI` (many methods there likely already mirror docker). Note its existing `Run`/`Pull` shape.

- [ ] **Step 2: Write the failing test**

```go
// internal/runtime/apple_lifecycle_test.go
package runtime

import "testing"

func TestAppleSatisfiesInterface(t *testing.T) {
	var _ ContainerRuntime = &appleContainer{}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/runtime/ -run TestAppleSatisfiesInterface`
Expected: FAIL to compile — `appleContainer` missing the five methods.

- [ ] **Step 4: Implement the methods**

If the Apple CLI is docker-compatible for these verbs, delegate to the same argv shape as `ociCLI` (copy the five method bodies from Task 5, substituting the Apple binary name/field). If any verb differs, implement that verb per the Apple CLI grammar. If Apple's CLI genuinely cannot support long-lived exec, implement the methods to return `fmt.Errorf("apple container runtime does not support uses-scope (runsIn.image on a uses); use docker/podman")` — a run-time hard error consistent with the "no silent fallback" rule.

- [ ] **Step 5: Run the whole runtime package**

Run: `go test ./internal/runtime/`
Expected: PASS — including Task 4's `lifecycle_contract_test.go` and Task 5's argv tests, now that the package compiles.

- [ ] **Step 6: Commit**

```bash
git add internal/runtime/apple.go internal/runtime/apple_lifecycle_test.go
git commit -m "feat(runtime): apple container scope lifecycle parity"
```

---

## Task 7: host scope manager

**Files:**
- Create: `internal/agent/scope.go`
- Test: `internal/agent/scope_test.go` (create)

**Interfaces:**
- Consumes: `runtime.ContainerRuntime` lifecycle (Tasks 4–6); `api.ClaimStep`.
- Produces:
  - `type scopeManager struct { rt runtime.ContainerRuntime; open map[string]runtime.ContainerHandle }`
  - `func newScopeManager(rt runtime.ContainerRuntime) *scopeManager`
  - `func (m *scopeManager) key(step api.ClaimStep) string` → `step.ScopeID + "\x00" + step.MatrixKey`
  - `func (m *scopeManager) ensure(ctx, step, env []string) (runtime.ContainerHandle, error)` — Create on first use for a key, cached after.
  - `func (m *scopeManager) exec(ctx, h, script string, env []string, stdout, stderr io.Writer) (int, error)`
  - `func (m *scopeManager) copyOutToTemp(ctx, h, containerPath string) (hostPath string, cleanup func(), err error)`
  - `func (m *scopeManager) copyIn(ctx, h, hostPath, containerPath string) error`
  - `func (m *scopeManager) closeKey(ctx, key string)` and `func (m *scopeManager) closeAll(ctx)` — Remove and forget.

- [ ] **Step 1: Write the failing test with a fake runtime**

```go
// internal/agent/scope_test.go
package agent

import (
	"context"
	"io"
	"testing"

	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/eirueimi/unified-cd/internal/api"
)

type fakeRT struct {
	created  int
	removed  int
	lastExec string
}

func (f *fakeRT) Name() string    { return "fake" }
func (f *fakeRT) Available() bool  { return true }
func (f *fakeRT) Pull(context.Context, string) error { return nil }
func (f *fakeRT) Run(context.Context, crt.RunSpec, io.Writer, io.Writer) (int, error) { return 0, nil }
func (f *fakeRT) Create(context.Context, crt.CreateSpec) (crt.ContainerHandle, error) {
	f.created++
	return crt.ContainerHandle{ID: "c1"}, nil
}
func (f *fakeRT) Exec(_ context.Context, _ crt.ContainerHandle, spec crt.ExecSpec, _ , _ io.Writer) (int, error) {
	f.lastExec = spec.Script
	return 0, nil
}
func (f *fakeRT) CopyIn(context.Context, crt.ContainerHandle, string, string) error  { return nil }
func (f *fakeRT) CopyOut(context.Context, crt.ContainerHandle, string, string) error { return nil }
func (f *fakeRT) Remove(context.Context, crt.ContainerHandle) error { f.removed++; return nil }

func TestScopeManagerReusesEnvPerKey(t *testing.T) {
	f := &fakeRT{}
	m := newScopeManager(f)
	ctx := context.Background()
	s := api.ClaimStep{ScopeID: "scope:build", ScopeImage: "img", MatrixKey: ""}

	if _, err := m.ensure(ctx, s, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ensure(ctx, s, nil); err != nil {
		t.Fatal(err)
	}
	if f.created != 1 {
		t.Fatalf("expected 1 Create for same key, got %d", f.created)
	}
	m.closeAll(ctx)
	if f.removed != 1 {
		t.Fatalf("expected 1 Remove, got %d", f.removed)
	}
}

func TestScopeManagerKeyIncludesMatrix(t *testing.T) {
	m := newScopeManager(&fakeRT{})
	a := m.key(api.ClaimStep{ScopeID: "s", MatrixKey: "linux"})
	b := m.key(api.ClaimStep{ScopeID: "s", MatrixKey: "windows"})
	if a == b {
		t.Fatal("matrix variants must have distinct scope keys")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agent/ -run TestScopeManager`
Expected: FAIL to compile — `newScopeManager`, `scopeManager` undefined.

- [ ] **Step 3: Implement `internal/agent/scope.go`**

```go
package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/eirueimi/unified-cd/internal/api"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
)

// scopeManager owns the isolated environments for uses-level runsIn.image
// scopes on the host agent. One environment per (ScopeID, MatrixKey).
type scopeManager struct {
	rt   crt.ContainerRuntime
	open map[string]crt.ContainerHandle
}

func newScopeManager(rt crt.ContainerRuntime) *scopeManager {
	return &scopeManager{rt: rt, open: map[string]crt.ContainerHandle{}}
}

func (m *scopeManager) key(step api.ClaimStep) string {
	return step.ScopeID + "\x00" + step.MatrixKey
}

// ensure returns the scope container for step, creating it on first use.
func (m *scopeManager) ensure(ctx context.Context, step api.ClaimStep, env []string) (crt.ContainerHandle, error) {
	k := m.key(step)
	if h, ok := m.open[k]; ok {
		return h, nil
	}
	h, err := m.rt.Create(ctx, crt.CreateSpec{Image: step.ScopeImage, Env: env})
	if err != nil {
		return crt.ContainerHandle{}, fmt.Errorf("provision scope %q (image %q): %w", step.ScopeID, step.ScopeImage, err)
	}
	m.open[k] = h
	return h, nil
}

func (m *scopeManager) exec(ctx context.Context, h crt.ContainerHandle, script string, env []string, stdout, stderr io.Writer) (int, error) {
	return m.rt.Exec(ctx, h, crt.ExecSpec{Script: script, Env: env}, stdout, stderr)
}

// copyOutToTemp copies a container path to a fresh host temp dir and returns
// the host path plus a cleanup func.
func (m *scopeManager) copyOutToTemp(ctx context.Context, h crt.ContainerHandle, containerPath string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "ucd-scope-out-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	dst := dir + string(os.PathSeparator) + "artifact"
	if err := m.rt.CopyOut(ctx, h, containerPath, dst); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return dst, cleanup, nil
}

func (m *scopeManager) copyIn(ctx context.Context, h crt.ContainerHandle, hostPath, containerPath string) error {
	return m.rt.CopyIn(ctx, h, hostPath, containerPath)
}

func (m *scopeManager) closeKey(ctx context.Context, key string) {
	h, ok := m.open[key]
	if !ok {
		return
	}
	if err := m.rt.Remove(ctx, h); err != nil {
		slog.Warn("scope teardown failed", "container", h.ID, "error", err)
	}
	delete(m.open, key)
}

func (m *scopeManager) closeAll(ctx context.Context) {
	for k := range m.open {
		m.closeKey(ctx, k)
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/agent/ -run TestScopeManager`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/agent/scope.go internal/agent/scope_test.go
git commit -m "feat(agent): host scope manager for uses-scope environments"
```

---

## Task 8: Wire the scope manager into the host agent step loop

**Files:**
- Modify: `internal/agent/agent.go` — the step dispatch (cache `:382`, upload `:389`, download `:396`, run switch `:451-465`) and the claim loop that owns `workDir`
- Test: `internal/agent/agent_scope_test.go` (create) — an integration-style test gated on a real runtime, plus a unit test of the routing predicate

**Interfaces:**
- Consumes: `scopeManager` (Task 7); `api.ClaimStep.ScopeID`.
- Produces: within a claim, a single `*scopeManager` (created lazily when the first scoped step appears, using `a.containerRuntime()`), deferred `closeAll` at claim end. Step dispatch gains: when `step.ScopeID != ""`, run/cache/upload/download route to the scope environment instead of `workDir`.

- [ ] **Step 1: Read the claim loop**

Read `internal/agent/agent.go` around the claim handler that sets up `workDir` and iterates steps (the function containing lines 360–478). Identify: where `workDir` is created, where `postHooks` run, and where the per-step closure returns.

- [ ] **Step 2: Write the routing unit test**

```go
// internal/agent/agent_scope_test.go
package agent

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
)

func TestIsScopedStep(t *testing.T) {
	if !isScopedStep(api.ClaimStep{ScopeID: "scope:x"}) {
		t.Fatal("expected scoped")
	}
	if isScopedStep(api.ClaimStep{}) {
		t.Fatal("expected not scoped")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/agent/ -run TestIsScopedStep`
Expected: FAIL to compile — `isScopedStep` undefined.

- [ ] **Step 4: Add the predicate and wire routing**

Add the predicate in `internal/agent/scope.go`:

```go
func isScopedStep(step api.ClaimStep) bool { return step.ScopeID != "" }
```

In `agent.go`, in the claim handler:

1. Declare a lazily-initialized scope manager for the claim:

```go
	var scopes *scopeManager
	getScopes := func() (*scopeManager, error) {
		if scopes != nil {
			return scopes, nil
		}
		rt, err := a.containerRuntime()
		if err != nil {
			return nil, fmt.Errorf("uses-scope requires a container runtime: %w", err)
		}
		scopes = newScopeManager(rt)
		return scopes, nil
	}
	defer func() {
		if scopes != nil {
			scopes.closeAll(context.WithoutCancel(stepCtx))
		}
	}()
```

2. In the run branch (`default:` at line ~463), before the existing `RunStepCapture`, handle scoped steps first:

```go
				case isScopedStep(step):
					sm, serr := getScopes()
					if serr != nil {
						runErr = serr
						ec = -1
						break
					}
					h, herr := sm.ensure(stepCtx, step, extraEnv)
					if herr != nil {
						runErr = herr
						ec = -1
						break
					}
					capturedStdout, ec, runErr = sm.exec(stepCtx, h, expandedRun, extraEnv, io.Discard, stderrPusher)
```

(Insert this as the FIRST `case` in the `switch` at line 451 so it takes precedence over the per-step `RunsIn` cases; scoped steps never carry `RunsIn`.)

3. For cache/upload/download (lines 382/389/396), route to the scope FS when `isScopedStep(step)`. Extract the scoped path handling into helpers on `scopeManager` used by the existing `executeUploadArtifact`/`executeDownloadArtifact`/`executeCacheStep`. Minimal approach — pass an optional scope handle:

- `executeUploadArtifact`: when scoped, `copyOutToTemp` the artifact path from the scope container, then upload the temp path (instead of `resolveWorkspacePath(workDir, ua.Path)`).
- `executeDownloadArtifact`: when scoped, download to a host temp dir (existing client call), then `copyIn` to the scope container path.
- `executeCacheStep`: when scoped, `copyOutToTemp` the cache path for save; for restore, restore to a host temp dir then `copyIn`. Keep warn+skip on error.

Add a scope handle parameter (nil when not scoped) to each of the three functions and branch on it. Show the upload change explicitly:

```go
func (a *Agent) executeUploadArtifact(ctx context.Context, step api.ClaimStep, runID, workDir string, sm *scopeManager, h crt.ContainerHandle) error {
	ua := step.UploadArtifact
	var path string
	if sm != nil {
		p, cleanup, err := sm.copyOutToTemp(ctx, h, ua.Path)
		if err != nil {
			// fail-loud (artifact policy)
			return fmt.Errorf("upload-artifact %q: copy from scope: %w", ua.Name, err)
		}
		defer cleanup()
		path = p
	} else {
		path = resolveWorkspacePath(workDir, ua.Path)
	}
	// ... existing ReportStep + a.Client.UploadArtifact(ctx, runID, ua.Name, path) ...
}
```

Thread `sm`/`h` from the dispatch site: when `isScopedStep(step)`, call `sm, _ := getScopes(); h, _ := sm.ensure(...)` once per step group and pass them in; otherwise pass `nil, crt.ContainerHandle{}`.

- [ ] **Step 5: Run the unit test + package build**

Run: `go test ./internal/agent/ -run TestIsScopedStep && go build ./...`
Expected: PASS + build OK.

- [ ] **Step 6: Add a runtime-gated integration test**

```go
// internal/agent/agent_scope_integration_test.go
//go:build integration

package agent
// A test that, when docker/podman is present, runs a scoped step that writes a
// file, an upload-artifact that captures it, and asserts the artifact bytes.
// Skip via t.Skip if runtime.Detect("") returns an error.
```

Fill in the body using the real `Agent` claim path against a local fake object store (mirror an existing artifact integration test in this package if present; otherwise assert via a stub `Client` that records `UploadArtifact` path contents).

- [ ] **Step 7: Run full package (short) + commit**

Run: `go test -short ./internal/agent/`
Expected: PASS

```bash
git add internal/agent/agent.go internal/agent/scope.go internal/agent/agent_scope_test.go internal/agent/agent_scope_integration_test.go
git commit -m "feat(agent): route uses-scope run/artifact/cache to isolated container"
```

---

## Task 9: k8s scope pod builder

**Files:**
- Create: `internal/k8sagent/scopepod.go`
- Test: `internal/k8sagent/scopepod_test.go` (create)

**Interfaces:**
- Consumes: existing sidecar injection (`SidecarSpec`, the artifact sidecar container) and `buildImageStepPod` conventions in `podbuilder.go`.
- Produces: `func buildScopePod(runID, namespace, scopeID, image string, env map[string]string, sidecar SidecarSpec) *corev1.Pod` — a pod with: the scope `image` container (kept alive with `sleep infinity`) mounting a private `emptyDir` scratch volume at a fixed `scopeMountPath` (const `"/workspace"`); the artifact sidecar mounting the SAME scratch volume; and NO outer-workspace PVC. `GenerateName` `ucd-scope-<short>-`.

- [ ] **Step 1: Write the failing test**

```go
// internal/k8sagent/scopepod_test.go
package k8sagent

import "testing"

func TestBuildScopePodHasScratchAndSidecarNoWorkspacePVC(t *testing.T) {
	pod := buildScopePod("run123", "ci", "scope:build", "golang:1.22",
		map[string]string{"K": "V"}, SidecarSpec{Image: "sidecar:1", S3SecretName: "s3"})

	// scratch volume is emptyDir, mounted by both the step and the sidecar
	var scratch *string
	for _, v := range pod.Spec.Volumes {
		if v.Name == "workspace" {
			if v.EmptyDir == nil {
				t.Fatal("scope scratch volume must be emptyDir, not a PVC")
			}
			n := v.Name
			scratch = &n
		}
	}
	if scratch == nil {
		t.Fatal("missing scratch volume")
	}
	names := map[string]bool{}
	for _, c := range pod.Spec.Containers {
		names[c.Name] = true
		mounted := false
		for _, m := range c.VolumeMounts {
			if m.Name == "workspace" {
				mounted = true
			}
		}
		if !mounted {
			t.Fatalf("container %q does not mount the scratch volume", c.Name)
		}
	}
	if !names["step"] {
		t.Fatal("missing step container")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/k8sagent/ -run TestBuildScopePod`
Expected: FAIL to compile — `buildScopePod` undefined.

- [ ] **Step 3: Read the sidecar/pod helpers**

Read `internal/k8sagent/podbuilder.go` — `buildImageStepPod` (309-351), `injectWorkspace` (223-263), and how `SidecarSpec` is turned into a sidecar container (grep `SidecarSpec` and the artifact sidecar container builder). Reuse the sidecar-container constructor; substitute an `emptyDir` scratch volume for the workspace PVC.

- [ ] **Step 4: Implement `buildScopePod`**

Model on `buildImageStepPod`, but add a shared `emptyDir` volume named `workspace` mounted at `scopeMountPath` on BOTH the `step` container and the artifact sidecar container. Reuse the existing sidecar-container builder used by `BuildPod`. (Write the concrete `*corev1.Pod` literal following `buildImageStepPod`'s structure: `RestartPolicyNever`, sorted env, `Command: []string{"sleep","infinity"}` on the step container; add `Volumes: []corev1.Volume{{Name:"workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}` and matching `VolumeMounts` on both containers.) Add `const scopeMountPath = "/workspace"`.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/k8sagent/ -run TestBuildScopePod`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/k8sagent/scopepod.go internal/k8sagent/scopepod_test.go
git commit -m "feat(k8sagent): build isolated scope pod with scratch volume + sidecar"
```

---

## Task 10: Route scope steps in the k8s agent

**Files:**
- Modify: `internal/k8sagent/agent.go` — `stepExec` (`:189-210`), cache (`:360`), upload (`:408`), download (`:432`)
- Test: `internal/k8sagent/agent_scope_test.go` (create) with a fake clientset

**Interfaces:**
- Consumes: `buildScopePod` (Task 9); `api.ClaimStep.ScopeID`.
- Produces: per claim, a scope pod manager keyed on `(ScopeID, MatrixKey)`: lazily create the scope pod, exec scope run steps into its `step` container (instead of the pooled pod), run scope artifact/cache via the scope pod's sidecar against `scopeMountPath`, and GC the scope pod at claim end. Non-scoped steps are unchanged.

- [ ] **Step 1: Write the failing test**

```go
// internal/k8sagent/agent_scope_test.go
package k8sagent

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
)

func TestScopePodKeyDistinctPerMatrix(t *testing.T) {
	a := scopeKey(api.ClaimStep{ScopeID: "s", MatrixKey: "linux"})
	b := scopeKey(api.ClaimStep{ScopeID: "s", MatrixKey: "windows"})
	if a == b || a == "" {
		t.Fatalf("bad scope keys: %q %q", a, b)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/k8sagent/ -run TestScopePodKey`
Expected: FAIL to compile — `scopeKey` undefined.

- [ ] **Step 3: Add key helper + scope pod tracking**

Add to `internal/k8sagent/scopepod.go`:

```go
func scopeKey(step api.ClaimStep) string { return step.ScopeID + "\x00" + step.MatrixKey }
```

In `agent.go`'s claim handler, add a `map[string]string` (scope key → scope pod name) and a helper `ensureScopePod(ctx, step) (podName string, err error)` that creates via `buildScopePod` + the client on first use, waits for readiness (reuse the `imagePodStartTimeout`-bounded wait used by `runImageStep`), caches, and is GC'd on claim end (add scope pod names to the existing pod GC/cleanup path).

- [ ] **Step 4: Route in `stepExec` and artifact/cache**

- In `stepExec` (`:189`), before the existing `step.RunsIn.Image` branch, add:

```go
			if step.ScopeID != "" {
				podName, err := ensureScopePod(execCtx, step)
				if err != nil {
					return -1, "", err
				}
				ec, execErr = a.exec.ExecStep(execCtx, podName, "step", expandedRun, stdoutWriter, stderrPusher)
				stderrPusher.Flush(execCtx)
				return ec, stdoutBuf.String(), execErr
			}
```

- For upload (`:408`)/download (`:432`)/cache (`:360`): when `step.ScopeID != ""`, target the scope pod's sidecar and `scopeMountPath` instead of the pooled pod's `artifactSidecarName`/`mountPath`. Factor the sidecar container name + mount path into locals chosen by scope-ness, e.g.:

```go
			sidecar, mount := artifactSidecarName, mountPath
			if step.ScopeID != "" {
				sidecar, mount = scopeSidecarName(ensuredPodName), scopeMountPath
			}
```

Use `sidecar`/`mount` in the existing `path.Join(mount, ...)` and `sidecarExec(execCtx, sidecar, argv)` calls. Cache stays warn+skip; artifacts stay fail-loud.

- [ ] **Step 5: Run tests + build**

Run: `go test ./internal/k8sagent/ -run TestScopePodKey && go build ./...`
Expected: PASS + build OK.

- [ ] **Step 6: Add a fake-clientset lifecycle test**

Add a test that drives a claim with one scoped run step + one scoped upload-artifact through the k8s agent with a fake clientset and fake exec, asserting: `buildScopePod` was created once, exec targeted the `step` container, the sidecar argv used `scopeMountPath`, and the scope pod was deleted at claim end. Model on the existing k8s agent tests (`agent_runsin_test.go`).

- [ ] **Step 7: Run package (short) + commit**

Run: `go test -short ./internal/k8sagent/`
Expected: PASS

```bash
git add internal/k8sagent/agent.go internal/k8sagent/scopepod.go internal/k8sagent/agent_scope_test.go
git commit -m "feat(k8sagent): route uses-scope steps to a dedicated scope pod"
```

---

## Task 11: Docs, schema regen, and backend-parity check

**Files:**
- Modify: `docs/resources.md` (Job/uses `runsIn` section) — document uses-scope semantics
- Regenerate: `schemas/unified-cd.schema.json`, `docs/field-reference.md` via `go generate ./internal/dsl/`
- Test: a table-style parity test asserting host and k8s produce the same scope routing decisions

**Interfaces:**
- Consumes: everything above.
- Produces: user-facing docs for uses-scope + regenerated schema/field-reference reflecting `ScopeID`/`ScopeImage` (these are internal fields; confirm whether they should be excluded from the schema — see Step 2).

- [ ] **Step 1: Document uses-scope**

In `docs/resources.md`, in the `runsIn` documentation, add a subsection: uses-level `runsIn.image` = isolated scope; artifacts/cache inside capture the scope; step-level unchanged; nested runsIn is an error; inputs via `with:`/`download-artifact`, outputs via `upload-artifact`/`outputs`.

- [ ] **Step 2: Decide schema exposure of scope fields**

`ScopeID`/`ScopeImage` are internal (set by inline expansion, not user-authored). Since the DSL schema is generated from struct tags, add `schema:"-"`-style exclusion IF the generator supports it (check `cmd/schemagen`), otherwise leave them (harmless, `omitempty`). Regenerate:

Run: `go generate ./internal/dsl/`
Expected: `schemas/unified-cd.schema.json` and `docs/field-reference.md` updated (or unchanged if excluded).

- [ ] **Step 3: Write the parity test**

```go
// internal/dsl/scope_parity_test.go  (or a suitable shared test package)
// Assert that a scoped ClaimStep is classified as "scope" routing on both the
// host predicate (isScopedStep) and the k8s predicate (step.ScopeID != ""):
// both must key on (ScopeID, MatrixKey). This guards backend divergence.
```

Since the two predicates live in different packages, assert the shared invariant: `step.ScopeID != ""` iff scope routing, and that both `agent.newScopeManager(...).key` and `k8sagent.scopeKey` incorporate `MatrixKey` (already covered by Tasks 7 & 10 tests — this step cross-links them in a comment and runs both).

- [ ] **Step 4: Full build + test**

Run: `go build ./... && make test-short`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add docs/resources.md docs/field-reference.md schemas/unified-cd.schema.json internal/dsl/scope_parity_test.go
git commit -m "docs(runsin): document uses-scope artifacts & cache; regen schema"
```

---

## Self-Review Notes

- **Spec coverage:** two-tier semantics (Tasks 2, 8, 10) · isolation contract / no outer workspace (Tasks 7, 9) · ScopeID/ScopeImage data model + matrix variants (Tasks 1, 3, 7, 10) · host lifecycle Create/Exec/CopyIn/CopyOut/Remove (Tasks 4–6) · k8s scope pod + sidecar (Tasks 9–10) · nested-runsIn parse error (Task 2) · artifact fail-loud / cache warn+skip (Tasks 8, 10) · runtime-absent hard error (Task 8) · docs/schema (Task 11) · backend parity (Task 11).
- **Open implementation detail:** the exact compile site for Task 3 and the exact sidecar-container constructor for Task 9 are located by grep during the task (instructions given) rather than hard-coded here, because they were not read during planning. All new units (scope manager, scope pod builder, runtime lifecycle) have complete code.
- **Naming consistency:** `scopeManager.key` (host) and `scopeKey` (k8s) both = `ScopeID + "\x00" + MatrixKey`. Scope mount path const `scopeMountPath = "/workspace"`. Handle type `runtime.ContainerHandle{ID}`.
