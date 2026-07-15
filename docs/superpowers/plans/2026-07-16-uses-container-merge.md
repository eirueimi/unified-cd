# `uses:` Container Auto-Merge + Reference Validation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Merge a non-scope `uses:` template's own `podTemplate` containers into the caller's pod (gap-fill), and validate every `container:` reference of a resolved `uses:` spec at run creation so an undefined container fails with a clear message instead of an opaque runtime exec error.

**Architecture:** Container definitions the caller lacks are collected during `uses:` expansion (`internal/gittemplate`), bubbled up through the resolve recursion, and merged into the caller's `spec.PodTemplate` in `ResolveSpec`. A pure `dsl` helper validates the fully-resolved spec's container references; the controller's git-resolution sweeper calls it after a successful resolve and fails the run deterministically on error.

**Tech Stack:** Go; `internal/dsl` (types + pure helpers), `internal/gittemplate` (resolve/inline), `internal/controller/scheduler.go`. Tests use the existing `stubFetcher`/`mapFetcher`/`NewResolver`/`mustMarshalSpec` harness in `internal/gittemplate/resolve_test.go`.

## Global Constraints

- **Scope: non-scope `uses:` only.** When the outer uses step has `runsIn.image` (scope mode), contribute **no** containers — the template runs in its own scope pod and `container:` is already rejected there.
- **Reserved names never injectable:** a template may not contribute a container named `job` or `unified-artifact`; doing so is a resolution error.
- **Merge rule:** gap-fill only — merge a template container only if the caller lacks that name. Same name in caller and template (or contributed by two templates): if the definitions are **JSON-equal**, dedup (keep the first/caller); if they **differ**, resolution error.
- **Reserved-name constants live in `dsl`** (`PrimaryContainerName = "job"`, `ArtifactSidecarContainerName = "unified-artifact"`); `k8sagent`'s existing `artifactSidecarName` must alias the `dsl` constant to stay in sync.
- **Validation placement:** `dsl.ValidateContainerReferences(spec)` runs in `internal/controller/scheduler.go` `resolveGitPendingRuns`, after a successful `ResolveSpec`, before `UpdateRunSpec`; on error the run is failed deterministically (mirror the existing `IsResolveError` branch: system log line + `MarkRunFinished(RunFailed)` + `bo.Success`).
- All new failures are **deterministic run-creation errors** (`newResolveError` in gittemplate; a plain error from `dsl`), never silent, never deferred to exec time.
- Only `containers` are merged — not volumes or other podTemplate spec keys.

---

### Task 1: `dsl` reserved-container-name constants + container accessors

**Files:**
- Create: `internal/dsl/container.go`
- Modify: `internal/k8sagent/podbuilder.go:14` (alias the sidecar name to the new dsl constant)
- Test: `internal/dsl/container_test.go`

**Interfaces:**
- Produces:
  - `const dsl.PrimaryContainerName = "job"`, `const dsl.ArtifactSidecarContainerName = "unified-artifact"`
  - `func dsl.IsReservedContainerName(name string) bool`
  - `func dsl.PodTemplateContainers(pt *PodTemplate) []map[string]any` (nil-safe; container defs in declared order)
  - `func dsl.ContainerDefName(def map[string]any) string`

- [ ] **Step 1: Write the failing test**

Create `internal/dsl/container_test.go`:

```go
package dsl

import "testing"

func TestIsReservedContainerName(t *testing.T) {
	for _, n := range []string{"job", "unified-artifact"} {
		if !IsReservedContainerName(n) {
			t.Errorf("%q should be reserved", n)
		}
	}
	for _, n := range []string{"", "foo", "Job", "build"} {
		if IsReservedContainerName(n) {
			t.Errorf("%q should not be reserved", n)
		}
	}
}

func TestPodTemplateContainers_AndName(t *testing.T) {
	if got := PodTemplateContainers(nil); got != nil {
		t.Fatalf("nil template: want nil, got %v", got)
	}
	pt := &PodTemplate{Spec: map[string]any{"containers": []any{
		map[string]any{"name": "foo", "image": "x"},
		map[string]any{"name": "bar"},
		"not-a-map", // skipped
	}}}
	got := PodTemplateContainers(pt)
	if len(got) != 2 {
		t.Fatalf("want 2 container defs, got %d", len(got))
	}
	if ContainerDefName(got[0]) != "foo" || ContainerDefName(got[1]) != "bar" {
		t.Fatalf("names: got %q,%q", ContainerDefName(got[0]), ContainerDefName(got[1]))
	}
	if ContainerDefName(map[string]any{}) != "" {
		t.Fatalf("missing name should be empty string")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/dsl/ -run 'IsReservedContainerName|PodTemplateContainers_AndName' -count=1`
Expected: FAIL (compile error — undefined identifiers).

- [ ] **Step 3: Create the helpers**

Create `internal/dsl/container.go`:

```go
package dsl

// Reserved container names are injected/owned by the system, never user- or
// template-supplied. PrimaryContainerName is the default exec target for a step
// with no container:. ArtifactSidecarContainerName is the internal artifact/cache
// sidecar. A uses: template may not inject either, and both are always valid
// container: targets even when absent from a podTemplate's container list.
const (
	PrimaryContainerName         = "job"
	ArtifactSidecarContainerName = "unified-artifact"
)

// IsReservedContainerName reports whether name is a system-reserved container name.
func IsReservedContainerName(name string) bool {
	return name == PrimaryContainerName || name == ArtifactSidecarContainerName
}

// PodTemplateContainers returns pt's container definition maps (from
// pt.Spec["containers"]) in declared order. Nil-safe; skips non-map entries.
func PodTemplateContainers(pt *PodTemplate) []map[string]any {
	if pt == nil {
		return nil
	}
	raw, _ := pt.Spec["containers"].([]any)
	var out []map[string]any
	for _, r := range raw {
		if c, ok := r.(map[string]any); ok {
			out = append(out, c)
		}
	}
	return out
}

// ContainerDefName returns the "name" field of a container definition map, or "".
func ContainerDefName(def map[string]any) string {
	n, _ := def["name"].(string)
	return n
}
```

- [ ] **Step 4: Alias the k8sagent sidecar constant to the dsl constant**

In `internal/k8sagent/podbuilder.go:14`, change:

```go
const artifactSidecarName = "unified-artifact"
```

to:

```go
const artifactSidecarName = dsl.ArtifactSidecarContainerName
```

Confirm `internal/k8sagent/podbuilder.go` already imports `"github.com/eirueimi/unified-cd/internal/dsl"` (it uses `dsl.` elsewhere); if not, add it.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/dsl/ -run 'IsReservedContainerName|PodTemplateContainers_AndName' -count=1 && go build ./internal/k8sagent/`
Expected: PASS and clean build.

- [ ] **Step 6: Commit**

```bash
git add internal/dsl/container.go internal/dsl/container_test.go internal/k8sagent/podbuilder.go
git commit -m "feat(dsl): reserved container-name constants + podTemplate container accessors"
```

---

### Task 2: `dsl.ValidateContainerReferences`

**Files:**
- Modify: `internal/dsl/container.go`
- Test: `internal/dsl/container_test.go`

**Interfaces:**
- Consumes: `PodTemplateContainers`, `ContainerDefName`, `IsReservedContainerName` (Task 1).
- Produces: `func dsl.ValidateContainerReferences(spec Spec) error` — nil if every step's `container:` resolves; else a descriptive error.

- [ ] **Step 1: Write the failing test**

Append to `internal/dsl/container_test.go`:

```go
func TestValidateContainerReferences(t *testing.T) {
	pt := &PodTemplate{Spec: map[string]any{"containers": []any{
		map[string]any{"name": "tools", "image": "x"},
	}}}

	// Valid: empty (default), reserved, and a defined container; parallel + finally.
	okSpec := Spec{
		PodTemplate: pt,
		Steps: []StepEntry{
			{Name: "a", Run: "echo"},                 // empty container -> ok
			{Name: "b", Container: "job", Run: "e"},   // reserved -> ok
			{Name: "c", Container: "tools", Run: "e"}, // defined -> ok
			{Parallel: []Step{{Name: "p", Container: "tools", Run: "e"}}},
			{Name: "scoped", Container: "ignored", ScopeID: "s1"}, // scope-tagged -> skipped
		},
		Finally: []StepEntry{{Name: "f", Container: "tools", Run: "e"}},
	}
	if err := ValidateContainerReferences(okSpec); err != nil {
		t.Fatalf("valid spec should pass, got %v", err)
	}

	// Invalid: main-DAG step references an undefined container.
	badMain := Spec{PodTemplate: pt, Steps: []StepEntry{{Name: "x", Container: "missing", Run: "e"}}}
	if err := ValidateContainerReferences(badMain); err == nil {
		t.Fatal("undefined container in a step must error")
	}

	// Invalid: inside a parallel block.
	badPar := Spec{PodTemplate: pt, Steps: []StepEntry{{Parallel: []Step{{Name: "y", Container: "missing", Run: "e"}}}}}
	if err := ValidateContainerReferences(badPar); err == nil {
		t.Fatal("undefined container in a parallel step must error")
	}

	// Invalid: inside finally.
	badFin := Spec{PodTemplate: pt, Finally: []StepEntry{{Name: "z", Container: "missing", Run: "e"}}}
	if err := ValidateContainerReferences(badFin); err == nil {
		t.Fatal("undefined container in a finally step must error")
	}

	// Reserved is valid even with no podTemplate at all.
	if err := ValidateContainerReferences(Spec{Steps: []StepEntry{{Name: "a", Container: "unified-artifact", Run: "e"}}}); err != nil {
		t.Fatalf("reserved container with no podTemplate should pass, got %v", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/dsl/ -run TestValidateContainerReferences -count=1`
Expected: FAIL (undefined `ValidateContainerReferences`).

- [ ] **Step 3: Implement**

Append to `internal/dsl/container.go` (add `"fmt"` to the file's imports — introduce an import block):

```go
// ValidateContainerReferences checks that every step's container: reference in
// spec resolves to a real target: empty (defaults to the primary container), a
// reserved name, or a container defined in spec.PodTemplate. Scope-tagged steps
// (ScopeID set) run in their own scope pod, not the caller's pod, so their
// container is not checked here. Returns a descriptive error on the first
// invalid reference. Intended to run on a fully-resolved spec (after uses merge).
func ValidateContainerReferences(spec Spec) error {
	defined := map[string]bool{}
	for _, c := range PodTemplateContainers(spec.PodTemplate) {
		if n := ContainerDefName(c); n != "" {
			defined[n] = true
		}
	}
	check := func(stepName, container, scopeID string) error {
		if container == "" || scopeID != "" {
			return nil
		}
		if IsReservedContainerName(container) || defined[container] {
			return nil
		}
		return fmt.Errorf("step %q references container %q, which is not defined in the job's podTemplate", stepName, container)
	}
	walk := func(entries []StepEntry) error {
		for _, e := range entries {
			if err := check(e.Name, e.Container, e.ScopeID); err != nil {
				return err
			}
			for _, p := range e.Parallel {
				if err := check(p.Name, p.Container, p.ScopeID); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(spec.Steps); err != nil {
		return err
	}
	return walk(spec.Finally)
}
```

The `container.go` file header becomes:

```go
package dsl

import "fmt"
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/dsl/ -run 'TestValidateContainerReferences|IsReservedContainerName|PodTemplateContainers' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dsl/container.go internal/dsl/container_test.go
git commit -m "feat(dsl): ValidateContainerReferences over a resolved spec"
```

---

### Task 3: gittemplate — collect + bubble + merge the template's containers

**Files:**
- Modify: `internal/gittemplate/inline.go` (`expandUsesStep` returns contributed containers; `ExpandUsesStep` wrapper adapts)
- Modify: `internal/gittemplate/resolve.go` (`resolveSteps` accumulates; `ResolveSpec` merges via a new `mergeContributedContainers`)
- Test: `internal/gittemplate/merge_test.go` (create)

**Interfaces:**
- Consumes: `dsl.PodTemplateContainers`, `dsl.ContainerDefName`, `dsl.IsReservedContainerName` (Task 1).
- Produces (internal to package):
  - `expandUsesStep(...) ([]dsl.StepEntry, []map[string]any, error)` — the third-return is the template's own contributed container defs (non-scope only; reserved names rejected).
  - `resolveSteps(...) ([]dsl.StepEntry, []map[string]any, error)` — accumulates contributed containers across siblings and nested `uses`.
  - `mergeContributedContainers(spec *dsl.Spec, contributed []map[string]any) error` — gap-fills into `spec.PodTemplate`, dedup-identical / error-differing.
  - `ExpandUsesStep` (exported) keeps its current `([]dsl.StepEntry, error)` signature.

- [ ] **Step 1: Write the failing tests**

Create `internal/gittemplate/merge_test.go`:

```go
package gittemplate_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/gittemplate"
)
```

(Add any further imports only if a later test needs them; keep the block minimal so there are no unused-import compile errors.)

// resolveToSpec resolves specJSON with the given fetcher and unmarshals the result.
func resolveToSpec(t *testing.T, fetcher gittemplate.Fetcher, specJSON []byte) (dsl.Spec, error) {
	t.Helper()
	resolver := gittemplate.NewResolver(fetcher, nil)
	out, err := resolver.ResolveSpec(context.Background(), specJSON, nilCred)
	if err != nil {
		return dsl.Spec{}, err
	}
	var s dsl.Spec
	require.NoError(t, json.Unmarshal(out, &s))
	return s, nil
}

func containerNames(pt *dsl.PodTemplate) []string {
	var out []string
	for _, c := range dsl.PodTemplateContainers(pt) {
		out = append(out, dsl.ContainerDefName(c))
	}
	return out
}

// Template declares its own podTemplate container `tools` and a step targeting it.
const tmplWithTools = `
apiVersion: unified-cd/v1
kind: Job
metadata: {name: tmpl}
spec:
  podTemplate:
    spec:
      containers:
        - {name: tools, image: alpine:3}
  steps:
    - name: run-in-tools
      container: tools
      run: echo hi
`

func TestResolveSpec_MergesTemplateContainer(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{{Name: "u", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/tmpl.yaml@v1"}}},
	})
	s, err := resolveToSpec(t, &stubFetcher{data: []byte(tmplWithTools)}, specJSON)
	require.NoError(t, err)
	require.Contains(t, containerNames(s.PodTemplate), "tools", "template container must be merged into the caller")
}

func TestResolveSpec_CallerContainerKept_IdenticalDedup(t *testing.T) {
	// Caller already defines an identical `tools`.
	specJSON := mustMarshalSpec(dsl.Spec{
		PodTemplate: &dsl.PodTemplate{Spec: map[string]any{"containers": []any{
			map[string]any{"name": "tools", "image": "alpine:3"},
		}}},
		Steps: []dsl.StepEntry{{Name: "u", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/tmpl.yaml@v1"}}},
	})
	s, err := resolveToSpec(t, &stubFetcher{data: []byte(tmplWithTools)}, specJSON)
	require.NoError(t, err)
	// Exactly one `tools`, not duplicated.
	n := 0
	for _, name := range containerNames(s.PodTemplate) {
		if name == "tools" {
			n++
		}
	}
	require.Equal(t, 1, n, "identical caller+template container must dedup to one")
}

func TestResolveSpec_CollisionDiffers_Errors(t *testing.T) {
	// Caller defines `tools` with a DIFFERENT image than the template.
	specJSON := mustMarshalSpec(dsl.Spec{
		PodTemplate: &dsl.PodTemplate{Spec: map[string]any{"containers": []any{
			map[string]any{"name": "tools", "image": "ubuntu:22.04"},
		}}},
		Steps: []dsl.StepEntry{{Name: "u", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/tmpl.yaml@v1"}}},
	})
	_, err := resolveToSpec(t, &stubFetcher{data: []byte(tmplWithTools)}, specJSON)
	require.Error(t, err)
	require.True(t, gittemplate.IsResolveError(err), "differing collision must be a deterministic resolve error")
	require.Contains(t, err.Error(), "tools")
}

const tmplReservedName = `
apiVersion: unified-cd/v1
kind: Job
metadata: {name: tmpl}
spec:
  podTemplate:
    spec:
      containers:
        - {name: job, image: evil:latest}
  steps:
    - {name: s, run: echo hi}
`

func TestResolveSpec_ReservedNameInjection_Errors(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{{Name: "u", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/tmpl.yaml@v1"}}},
	})
	_, err := resolveToSpec(t, &stubFetcher{data: []byte(tmplReservedName)}, specJSON)
	require.Error(t, err)
	require.True(t, gittemplate.IsResolveError(err))
	require.Contains(t, err.Error(), "job")
}

const tmplScopeContainer = `
apiVersion: unified-cd/v1
kind: Job
metadata: {name: tmpl}
spec:
  podTemplate:
    spec:
      containers:
        - {name: tools, image: alpine:3}
  steps:
    - {name: s, run: echo hi}
`

func TestResolveSpec_ScopeMode_DoesNotMerge(t *testing.T) {
	// A uses step WITH runsIn.image is scope mode: no container merge.
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{{
			Name:   "u",
			Uses:   &dsl.UsesStep{Job: "git://github.com/org/repo/tmpl.yaml@v1"},
			RunsIn: &dsl.RunsIn{Image: "alpine:3"},
		}},
	})
	s, err := resolveToSpec(t, &stubFetcher{data: []byte(tmplScopeContainer)}, specJSON)
	require.NoError(t, err)
	require.NotContains(t, containerNames(s.PodTemplate), "tools", "scope-mode uses must not merge containers")
}
```

Notes for the implementer:
- `nilCred` is the no-op `CredentialFunc` used by existing resolve tests — reuse it (grep `resolve_test.go` for its definition; if it is named differently, use that). `stubFetcher`, `mapFetcher`, and `mustMarshalSpec` are defined in `resolve_test.go` (same `gittemplate_test` package) — reuse them, do not redefine.
- `dsl.RunsIn` field is `Image` (confirm the exact field name in `internal/dsl/types.go`).

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/gittemplate/ -run 'TestResolveSpec_Merges|TestResolveSpec_CallerContainerKept|TestResolveSpec_CollisionDiffers|TestResolveSpec_ReservedNameInjection|TestResolveSpec_ScopeMode' -count=1`
Expected: FAIL (containers not merged today; no reserved/collision errors).

- [ ] **Step 3: `expandUsesStep` returns contributed containers**

In `internal/gittemplate/inline.go`:

Change the internal signature and add container collection. The function currently starts:

```go
func expandUsesStep(usesName string, with map[string]string, tplSpec dsl.Spec, outerRunsIn *dsl.RunsIn, outerContainer string) ([]dsl.StepEntry, error) {
	if len(tplSpec.Steps) == 0 {
		return nil, fmt.Errorf("template job has no steps")
	}

	scopeMode := outerRunsIn != nil && outerRunsIn.Image != ""
```

Change it to:

```go
func expandUsesStep(usesName string, with map[string]string, tplSpec dsl.Spec, outerRunsIn *dsl.RunsIn, outerContainer string) ([]dsl.StepEntry, []map[string]any, error) {
	if len(tplSpec.Steps) == 0 {
		return nil, nil, fmt.Errorf("template job has no steps")
	}

	scopeMode := outerRunsIn != nil && outerRunsIn.Image != ""

	// Non-scope uses: contribute the template's own podTemplate containers so
	// the caller's pod gains the containers the template's steps target. Reserved
	// names are never injectable (they would override the caller's primary or the
	// internal artifact sidecar). Scope mode runs the template in its own pod, so
	// nothing is contributed there.
	var contributed []map[string]any
	if !scopeMode {
		for _, c := range dsl.PodTemplateContainers(tplSpec.PodTemplate) {
			name := dsl.ContainerDefName(c)
			if name == "" {
				continue
			}
			if dsl.IsReservedContainerName(name) {
				return nil, nil, fmt.Errorf("template defines reserved container name %q, which cannot be injected into the caller", name)
			}
			contributed = append(contributed, c)
		}
	}
```

Then update **every** `return` in `expandUsesStep` to include the containers slot:
- Each existing `return nil, fmt.Errorf(...)` / `return nil, err` becomes `return nil, nil, <same error>`.
- The final `return result, nil` becomes `return result, contributed, nil`.

Update the exported wrapper (`inline.go:55`):

```go
func ExpandUsesStep(usesName string, with map[string]string, tplSpec dsl.Spec, outerRunsIn *dsl.RunsIn, outerContainer string) ([]dsl.StepEntry, error) {
	steps, _, err := expandUsesStep(usesName, with, tplSpec, outerRunsIn, outerContainer)
	return steps, err
}
```

Confirm `inline.go` imports `dsl` (it does — it uses `dsl.StepEntry`).

- [ ] **Step 4: `resolveSteps` accumulates; `ResolveSpec` merges**

In `internal/gittemplate/resolve.go`:

Change `resolveSteps` to return the accumulated contributed containers. Update the signature:

```go
func (r *Resolver) resolveSteps(
	ctx context.Context,
	steps []dsl.StepEntry,
	credFn CredentialFunc,
	depth int,
	path []string,
) ([]dsl.StepEntry, []map[string]any, error) {
	if depth > maxUsesDepth {
		return nil, nil, newResolveError("uses nesting exceeds max depth %d", maxUsesDepth)
	}
```

In the loop body, thread containers through. Replace the recursion + expansion block (currently lines ~162-184) with:

```go
		nestedPath := append(append([]string{}, path...), rawURI)
		nestedSteps, nestedContainers, err := r.resolveSteps(ctx, job.Spec.Steps, credFn, depth+1, nestedPath)
		if err != nil {
			return nil, nil, err
		}
		job.Spec.Steps = nestedSteps

		expanded, expandContainers, err := expandUsesStep(s.Name, s.Uses.WithAsStrings(), job.Spec, s.RunsIn, s.Container)
		if err != nil {
			return nil, nil, newResolveError("step %q: expand uses: %v", s.Name, err)
		}

		for _, es := range expanded {
			if es.Name == s.Name {
				continue // expected: the output-capture step intentionally reuses the uses step's own name
			}
			if seen[es.Name] {
				return nil, nil, newResolveError("step %q: expanded step name %q collides with an existing step", s.Name, es.Name)
			}
			seen[es.Name] = true
		}

		out = append(out, expanded...)
		contributed = append(contributed, nestedContainers...)
		contributed = append(contributed, expandContainers...)
```

Add the accumulator declaration next to `var out []dsl.StepEntry`:

```go
	var out []dsl.StepEntry
	var contributed []map[string]any
```

Update the other early returns in `resolveSteps` (the `depth`/URI-parse/fetch/validate/cycle branches) from `return nil, <err>` to `return nil, nil, <err>`, and the non-uses `continue` path is unchanged. Final return:

```go
	return out, contributed, nil
```

In `ResolveSpec`, capture the containers and merge before marshalling:

```go
	resolvedSteps, contributed, err := r.resolveSteps(ctx, spec.Steps, credFn, 0, nil)
	if err != nil {
		return nil, err
	}
	spec.Steps = resolvedSteps

	if err := mergeContributedContainers(&spec, contributed); err != nil {
		return nil, err
	}

	out, err := json.Marshal(spec)
```

Add `mergeContributedContainers` (new function in `resolve.go`; add `"bytes"` to imports — `encoding/json` is already imported):

```go
// mergeContributedContainers fills spec.PodTemplate with containers contributed
// by uses: templates that the caller lacks. A name already present (caller or a
// previously-merged contribution) is kept once if the definitions are JSON-equal,
// or is a deterministic resolve error if they differ. Only container names the
// caller does not already define are added; reserved names were already rejected
// at contribution time.
func mergeContributedContainers(spec *dsl.Spec, contributed []map[string]any) error {
	if len(contributed) == 0 {
		return nil
	}
	existing := map[string]map[string]any{}
	for _, c := range dsl.PodTemplateContainers(spec.PodTemplate) {
		if n := dsl.ContainerDefName(c); n != "" {
			existing[n] = c
		}
	}
	var rawList []any
	if spec.PodTemplate != nil {
		rawList, _ = spec.PodTemplate.Spec["containers"].([]any)
	}
	for _, c := range contributed {
		name := dsl.ContainerDefName(c)
		if name == "" {
			continue
		}
		if prev, ok := existing[name]; ok {
			eq, err := jsonEqual(prev, c)
			if err != nil {
				return wrapResolveError(fmt.Errorf("compare container %q: %w", name, err))
			}
			if !eq {
				return newResolveError("container %q is defined differently by the caller (or another uses template) and a uses template; rename one or align their definitions", name)
			}
			continue // identical -> dedup
		}
		existing[name] = c
		rawList = append(rawList, c)
	}
	if spec.PodTemplate == nil {
		spec.PodTemplate = &dsl.PodTemplate{}
	}
	if spec.PodTemplate.Spec == nil {
		spec.PodTemplate.Spec = map[string]any{}
	}
	spec.PodTemplate.Spec["containers"] = rawList
	return nil
}

// jsonEqual compares two values by their canonical JSON encoding (map keys are
// sorted by encoding/json), so it is order- and numeric-representation-stable
// across YAML- and JSON-sourced maps.
func jsonEqual(a, b any) (bool, error) {
	ba, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ba, bb), nil
}
```

- [ ] **Step 5: Run the merge tests + full package**

Run: `go test ./internal/gittemplate/ -run 'TestResolveSpec_Merges|TestResolveSpec_CallerContainerKept|TestResolveSpec_CollisionDiffers|TestResolveSpec_ReservedNameInjection|TestResolveSpec_ScopeMode' -count=1`
Expected: PASS.

Run: `go test ./internal/gittemplate/ -count=1`
Expected: PASS (existing resolve/inline tests unaffected — `ExpandUsesStep`'s signature is unchanged).

- [ ] **Step 6: Add a nested-uses container-bubbling test**

Model it on `TestResolveSpec_RecursiveUses` (`resolve_test.go`) using `mapFetcher`. Outer template uses an inner template; the inner template defines a container `deep` and a step targeting it. Assert the caller's resolved `podTemplate` contains `deep` (it bubbled up two levels).

```go
func TestResolveSpec_NestedUses_BubblesContainer(t *testing.T) {
	const inner = `
apiVersion: unified-cd/v1
kind: Job
metadata: {name: inner}
spec:
  podTemplate:
    spec:
      containers: [{name: deep, image: alpine:3}]
  steps:
    - {name: leaf, container: deep, run: echo hi}
`
	const outer = `
apiVersion: unified-cd/v1
kind: Job
metadata: {name: outer}
spec:
  steps:
    - {name: mid, uses: {job: "git://github.com/org/repo/inner.yaml@v1"}}
`
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{{Name: "top", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/outer.yaml@v1"}}},
	})
	fetcher := &mapFetcher{byURI: map[string][]byte{
		"git://github.com/org/repo/outer.yaml@v1": []byte(outer),
		"git://github.com/org/repo/inner.yaml@v1": []byte(inner),
	}}
	s, err := resolveToSpec(t, fetcher, specJSON)
	require.NoError(t, err)
	require.Contains(t, containerNames(s.PodTemplate), "deep", "nested template container must bubble up to the caller")
}
```

(Confirm `mapFetcher`'s key format matches how `TestResolveSpec_RecursiveUses` keys it — copy that test's URI-key convention exactly.)

Run: `go test ./internal/gittemplate/ -run TestResolveSpec_NestedUses_BubblesContainer -count=1`
Expected: PASS.

- [ ] **Step 7: Add a routing-flip test**

Merging a container that carries a k8s-only field (e.g. `volumeMounts`) must make the merged podTemplate host-ineligible, so the controller routes the run to a k8s agent. Add to `merge_test.go`:

```go
func TestResolveSpec_MergedK8sOnlyContainer_FlipsRouting(t *testing.T) {
	const tmpl = `
apiVersion: unified-cd/v1
kind: Job
metadata: {name: tmpl}
spec:
  podTemplate:
    spec:
      containers:
        - name: tools
          image: alpine:3
          volumeMounts: [{name: cache, mountPath: /c}]
  steps:
    - {name: s, container: tools, run: echo hi}
`
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{{Name: "u", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/tmpl.yaml@v1"}}},
	})
	s, err := resolveToSpec(t, &stubFetcher{data: []byte(tmpl)}, specJSON)
	require.NoError(t, err)
	require.True(t, dsl.PodTemplateNeedsKubernetes(s.PodTemplate),
		"a merged container with volumeMounts must make the podTemplate require kubernetes")
}
```

Run: `go test ./internal/gittemplate/ -run TestResolveSpec_MergedK8sOnlyContainer_FlipsRouting -count=1`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/gittemplate/inline.go internal/gittemplate/resolve.go internal/gittemplate/merge_test.go
git commit -m "feat(gittemplate): merge uses: template podTemplate containers into caller"
```

---

### Task 4: Validate container references in the resolver sweeper

**Files:**
- Modify: `internal/controller/scheduler.go` (`resolveGitPendingRuns`, after successful `ResolveSpec`)
- Test: `internal/controller/scheduler_test.go`

**Interfaces:**
- Consumes: `dsl.ValidateContainerReferences` (Task 2); the merge from Task 3 (so a valid uses spec passes).

- [ ] **Step 1: Write the failing test**

The existing harness (`internal/controller/scheduler_test.go`) uses a real `store.NewTestPostgres(t)`, `pg.CreateRun(...)` with a uses-spec JSON, `resolveGitPendingRuns(ctx, pg, resolver, nil, bo, deadline)`, then `pg.GetRun` + assert status. It defines fetchers implementing `Fetch(ctx, uri, token, sshKey) ([]byte, error)` (see `badYAMLFetcher` at `scheduler_test.go:162`). Model the new test on `TestResolveGitPendingRuns_DeterministicErrorFailsRun` (line 66), adding a content-returning fetcher.

Add to `internal/controller/scheduler_test.go`:

```go
// contentFetcher returns fixed YAML for any URI (a valid template, unlike
// badYAMLFetcher which returns malformed YAML).
type contentFetcher struct{ yaml []byte }

func (c contentFetcher) Fetch(ctx context.Context, uri gittemplate.URI, token, sshKey string) ([]byte, error) {
	return c.yaml, nil
}

// TestResolveGitPendingRuns_FailsOnDanglingContainer: a uses spec that resolves
// to a step referencing a container defined nowhere (neither caller nor template)
// is failed deterministically at resolution, not persisted as runnable.
func TestResolveGitPendingRuns_FailsOnDanglingContainer(t *testing.T) {
	pg := store.NewTestPostgres(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	specJSON := []byte(`{"steps":[{"name":"tpl","uses":{"job":"git://github.com/org/repo/job.yaml@v1"}}]}`)
	run, err := pg.CreateRun(t.Context(), "j", nil, specJSON, nil, nil, "")
	require.NoError(t, err)

	// Template step targets `ghost`, which no podTemplate (caller or template) defines.
	tmpl := []byte("apiVersion: unified-cd/v1\nkind: Job\nmetadata: {name: tmpl}\nspec:\n  steps:\n    - {name: s, container: ghost, run: echo hi}\n")
	resolver := gittemplate.NewResolver(contentFetcher{yaml: tmpl}, nil)
	bo := newFailureBackoff(time.Minute, time.Hour, 10_000)
	resolveGitPendingRuns(t.Context(), pg, resolver, nil, bo, time.Hour)

	got, err := pg.GetRun(t.Context(), run.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunFailed, got.Status, "a dangling container reference must fail the run at resolution")
}

// TestResolveGitPendingRuns_MergedTemplateContainerResolvesOK: a template that
// defines its own container AND targets it resolves + validates successfully
// (the container is merged into the caller), so the run is NOT failed.
func TestResolveGitPendingRuns_MergedTemplateContainerResolvesOK(t *testing.T) {
	pg := store.NewTestPostgres(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	specJSON := []byte(`{"steps":[{"name":"tpl","uses":{"job":"git://github.com/org/repo/job.yaml@v1"}}]}`)
	run, err := pg.CreateRun(t.Context(), "j", nil, specJSON, nil, nil, "")
	require.NoError(t, err)

	tmpl := []byte("apiVersion: unified-cd/v1\nkind: Job\nmetadata: {name: tmpl}\nspec:\n  podTemplate:\n    spec:\n      containers: [{name: tools, image: alpine:3}]\n  steps:\n    - {name: s, container: tools, run: echo hi}\n")
	resolver := gittemplate.NewResolver(contentFetcher{yaml: tmpl}, nil)
	bo := newFailureBackoff(time.Minute, time.Hour, 10_000)
	resolveGitPendingRuns(t.Context(), pg, resolver, nil, bo, time.Hour)

	got, err := pg.GetRun(t.Context(), run.ID)
	require.NoError(t, err)
	assert.NotEqual(t, api.RunFailed, got.Status, "a merged-container uses job must resolve successfully")
}
```

Note: `store.NewTestPostgres(t)` requires the test Postgres these controller tests already depend on (the existing resolver tests use it, so the environment is available). If `t.Context()` is not available in this Go version's tests, use `context.Background()` as the other tests in the file do — match the surrounding style.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/controller/ -run TestResolveGitPendingRuns_FailsOnDanglingContainer -count=1`
Expected: FAIL — today the dangling reference is not validated, so the run would be persisted as runnable (no `MarkRunFinished(RunFailed)`).

- [ ] **Step 3: Wire validation into the sweeper**

In `internal/controller/scheduler.go`, in `resolveGitPendingRuns`, immediately after the `ResolveSpec` error block (the `if err != nil { ... continue }` that ends around line 343) and BEFORE `if err := st.UpdateRunSpec(ctx, r.ID, resolved); ...` (line 349), insert:

```go
		// Validate that every container: reference in the fully-resolved spec
		// (after uses merge) resolves to a real container. An undefined reference
		// is a deterministic authoring error — fail the run now with a clear
		// reason instead of letting it fail opaquely at exec time.
		var rspec dsl.Spec
		if uerr := json.Unmarshal(resolved, &rspec); uerr != nil {
			slog.Error("git resolver: unmarshal resolved spec, failing run", "runID", r.ID, "error", uerr)
			if ferr := st.MarkRunFinished(ctx, r.ID, api.RunFailed); ferr != nil {
				slog.Warn("git resolver: mark run failed failed", "runID", r.ID, "error", ferr)
			}
			bo.Success(r.ID)
			continue
		}
		if verr := dsl.ValidateContainerReferences(rspec); verr != nil {
			slog.Error("git resolver: invalid container reference, failing run", "runID", r.ID, "error", verr)
			if _, lerr := st.AppendLog(ctx, r.ID, systemLogStepIndex, "stderr", time.Now(), verr.Error()); lerr != nil {
				slog.Warn("git resolver: append system log", "runID", r.ID, "error", lerr)
			}
			if ferr := st.MarkRunFinished(ctx, r.ID, api.RunFailed); ferr != nil {
				slog.Warn("git resolver: mark run failed failed", "runID", r.ID, "error", ferr)
			}
			bo.Success(r.ID)
			continue
		}
```

(`dsl`, `encoding/json`, `api`, `time`, `slog`, and `systemLogStepIndex` are all already in scope in this file.)

- [ ] **Step 4: Run the test + full package**

Run: `go test ./internal/controller/ -run 'TestResolveGitPendingRuns' -count=1`
Expected: PASS.

Run: `go test ./internal/controller/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/scheduler.go internal/controller/scheduler_test.go
git commit -m "feat(controller): validate resolved container references, fail run on dangling ref"
```

---

### Task 5: Docs + final sweep

**Files:**
- Modify: the `uses:` reference doc (find it: `grep -rln "uses:" docs/*.md` — likely `docs/jobs.md` or a dedicated templates/uses page).

- [ ] **Step 1: Locate the doc**

Run: `grep -rln "uses:" docs/`
Pick the page documenting `uses:` composition. Read it to match heading/style.

- [ ] **Step 2: Document the behavior**

Add a short subsection covering:
- A non-scope `uses:` template's `podTemplate` containers are **merged into the caller** so the template's `container:` steps work without the caller redefining them.
- Merge is **gap-fill**: a container the caller already defines is kept; if the caller and template define the same name **identically** it dedups, if **differently** the run fails at resolution with a clear error.
- The reserved names `job` and `unified-artifact` **cannot** be injected by a template.
- An undefined `container:` in a resolved `uses:` job now **fails at run creation** with a message naming the step and container, instead of an opaque runtime failure. (Scope-mode `uses:` — a uses step with `runsIn.image` — is unchanged: no merge, `container:` still not allowed there.)

- [ ] **Step 3: Final sweep**

Run: `gofmt -l internal/dsl/ internal/gittemplate/ internal/controller/` — NOTE: this repo has a pre-existing repo-wide CRLF condition that makes `gofmt -l` list many untouched files. Only act if a file YOU created/edited is newly unformatted for a real reason; otherwise note the pre-existing condition.
Run: `go build ./...`
Run: `go vet ./internal/dsl/ ./internal/gittemplate/ ./internal/controller/ ./internal/k8sagent/`
Run: `go test ./internal/dsl/ ./internal/gittemplate/ ./internal/controller/ -count=1`
All must pass (do NOT use `-race`; CGO is disabled in this env).

- [ ] **Step 4: Commit**

```bash
git add docs/
git commit -m "docs: uses: container merge + resolution-time reference validation"
```

---

## Notes for the executor

- **Task ordering matters:** Tasks 1→2 (dsl helpers) are consumed by Task 3 (merge) and Task 4 (validation). Execute in order.
- **Reuse existing test fixtures.** `stubFetcher`, `mapFetcher`, `mustMarshalSpec`, and the no-op `CredentialFunc` live in `internal/gittemplate/resolve_test.go` (package `gittemplate_test`). Do not redefine them; grep for the exact names/signatures first. Copy the URI-key convention from `TestResolveSpec_RecursiveUses` for the nested test.
- **`expandUsesStep` has many `return` sites** — every one must gain the extra `nil` container slot. Compile early and often; the compiler will point out any missed return.
- **Do not touch `ExpandUsesStep`'s exported signature** — the wrapper adapts, so no external/test caller breaks.
- **Confirm `dsl.RunsIn`'s image field name** (`Image`) against `internal/dsl/types.go` before writing the scope-mode test.
