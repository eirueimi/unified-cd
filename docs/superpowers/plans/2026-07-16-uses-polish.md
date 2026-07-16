# `uses:` Polish Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Propagate a `uses:` step's `if:` to its whole expansion; let a JobTemplate declare `finally:` (spliced into the caller's finally); validate container references at apply time for plain jobs; validate podTemplate container/volume name shape (DNS-1123 label) and normalize reserved-name checks.

**Architecture:** All changes live in `internal/dsl` (validation, JobTemplate schema) and `internal/gittemplate` (expansion/resolution). `if:` is CEL (`internal/dsl/condition.go`) so two conditions combine textually as `(A) && (B)`. Template finally steps bubble up through `resolveSteps` alongside the pod contribution and are appended to the caller's `spec.Finally` in `ResolveSpec`.

**Tech Stack:** Go; existing test harnesses: `internal/gittemplate/merge_test.go` helpers (`resolveToSpec`, `defNames`, `stubFetcher`, `mapFetcher`, `mustMarshalSpec`), `internal/dsl` table tests.

## Global Constraints

- `if:` combining: `combineIf(outer, inner)` → `""` if both empty; the non-empty one if only one; `(outer) && (inner)` otherwise. The INNER operand is the already-`rewriteRefs`-rewritten template if; the OUTER operand is caller-context CEL and is NEVER rewritten.
- The outer `if:` applies to EVERY expanded step: synthetic `__inputs`, every renamed concrete step, every parallel sub-step, and the output-capture step.
- Template `finally:` steps: prefixed (`usesName__`), ref-rewritten, shell-stamped, outer-`if:`-combined like body steps; **appended after the caller's own finally steps**; **rejected in scope mode** (`runsIn.image` on the uses step) with a clear resolve error.
- Apply-time `ValidateContainerReferences` runs in `Job.Validate` ONLY when no step in `Spec.Steps`/`Spec.Finally` (including `parallel:` sub-steps) has `Uses != nil`.
- `ValidateDNS1123Label`: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`, max 63 chars. Applied to every podTemplate container/volume name in `Job.Validate` AND `JobTemplate.Validate`. `IsReservedContainerName`/`IsReservedVolumeName` normalize with `strings.TrimSpace` + `strings.ToLower` before comparing.
- Generated artifacts: `JobTemplateSpec` gains `Finally` → run `go generate ./...` and commit the regenerated `schemas/unified-cd.schema.json` + `docs/field-reference.md` (schemagen scans `*_types.go`; docgen consumes the schema).
- All new failures are deterministic parse/apply/resolution errors naming the offending item.
- Before pushing: **full suite** `go test ./... -count=1` (repo-file-consuming tests live in `internal/shim` and `internal/dsl`). No `-race` (CGO disabled).

---

### Task 1: `dsl` — DNS-1123 label validation + normalized reserved names

**Files:**
- Modify: `internal/dsl/container.go` (normalize `IsReserved*`; add `ValidateDNS1123Label`)
- Modify: `internal/dsl/parse.go` (`Job.Validate` validates podTemplate container/volume names)
- Modify: `internal/dsl/jobtemplate.go` (`JobTemplate.Validate` likewise)
- Test: `internal/dsl/container_test.go`, `internal/dsl/parse_test.go` (or a new `podtemplate_names_test.go`)

**Interfaces:**
- Produces: `func ValidateDNS1123Label(name string) error`; `func validatePodTemplateNames(pt *PodTemplate) error` (unexported helper usable by both Validate paths — for JobTemplate call it on `ToSpec().PodTemplate` or validate the typed lists directly).

- [ ] **Step 1: Write the failing tests**

Append to `internal/dsl/container_test.go`:

```go
func TestValidateDNS1123Label(t *testing.T) {
	for _, ok := range []string{"job", "tools", "a", "a-1", "x0"} {
		if err := ValidateDNS1123Label(ok); err != nil {
			t.Errorf("%q should be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "JOB", " job", "job ", "My_Container", "-a", "a-", "a.b",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} { // 64 chars
		if err := ValidateDNS1123Label(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestIsReserved_NormalizesVariants(t *testing.T) {
	for _, v := range []string{"JOB", " job ", "Job", "UNIFIED-ARTIFACT", "Ucd-Shim"} {
		if !IsReservedContainerName(v) {
			t.Errorf("container %q should be treated as reserved (normalized)", v)
		}
	}
	for _, v := range []string{"WORKSPACE", " ucd-tools "} {
		if !IsReservedVolumeName(v) {
			t.Errorf("volume %q should be treated as reserved (normalized)", v)
		}
	}
	if IsReservedContainerName("jobs") || IsReservedVolumeName("workspaces") {
		t.Error("non-reserved names must not match")
	}
}
```

Add a validation test (in `internal/dsl/podtemplate_names_test.go`):

```go
package dsl

import (
	"strings"
	"testing"
)

func podTplJobYAML(containerName, volumeName string) string {
	return `apiVersion: unified-cd/v1
kind: Job
metadata: {name: x}
spec:
  podTemplate:
    spec:
      containers: [{name: "` + containerName + `", image: img}]
      volumes: [{name: "` + volumeName + `", emptyDir: {}}]
  steps:
    - {name: s, run: echo}
`
}

func TestJobValidate_PodTemplateNameShape(t *testing.T) {
	if _, err := Parse(strings.NewReader(podTplJobYAML("tools", "cache"))); err != nil {
		t.Fatalf("valid names must pass: %v", err)
	}
	for _, bad := range []struct{ c, v, want string }{
		{"My_Tools", "cache", "My_Tools"},
		{"tools", "Cache Vol", "Cache Vol"},
	} {
		_, err := Parse(strings.NewReader(podTplJobYAML(bad.c, bad.v)))
		if err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("names %q/%q: want error naming %q, got %v", bad.c, bad.v, bad.want, err)
		}
	}
}

func TestJobTemplateValidate_PodTemplateNameShape(t *testing.T) {
	y := `apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: t}
spec:
  podTemplate:
    spec:
      containers: [{name: "Bad Name", image: img}]
  steps:
    - {name: s, run: echo}
`
	if _, err := ParseJobTemplate([]byte(y)); err == nil || !strings.Contains(err.Error(), "Bad Name") {
		t.Errorf("JobTemplate podTemplate name shape must be validated, got %v", err)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/dsl/ -run 'DNS1123|Normalizes|PodTemplateNameShape' -count=1`
Expected: FAIL (undefined `ValidateDNS1123Label`; exact-match reserved checks; no name validation).

- [ ] **Step 3: Implement**

In `internal/dsl/container.go`: add near the reserved constants (add `"regexp"`, `"strings"` to imports):

```go
// dns1123LabelRe matches a valid Kubernetes container/volume name (DNS-1123
// label): lowercase alphanumerics and '-', starting and ending alphanumeric.
var dns1123LabelRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ValidateDNS1123Label rejects a name that is not a valid DNS-1123 label
// (the shape Kubernetes requires for container and volume names). Catching
// this at parse time turns an opaque pod-build API error into a clear
// authoring error — and closes case/whitespace evasion of the reserved-name
// checks.
func ValidateDNS1123Label(name string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if len(name) > 63 {
		return fmt.Errorf("name %q exceeds 63 characters", name)
	}
	if !dns1123LabelRe.MatchString(name) {
		return fmt.Errorf("name %q is not a valid DNS-1123 label (lowercase alphanumerics and '-', must start/end alphanumeric)", name)
	}
	return nil
}
```

Normalize the reserved checks (replace the two functions' bodies):

```go
// IsReservedContainerName reports whether name is a system-reserved container
// name. Comparison is normalized (trimmed, lowercased) so case/whitespace
// variants cannot evade the reservation; shape validation rejects such
// variants outright, this is defense in depth.
func IsReservedContainerName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return n == PrimaryContainerName || n == ArtifactSidecarContainerName || n == UcdShimContainerName
}

// IsReservedVolumeName reports whether name is a system-reserved volume name
// (normalized like IsReservedContainerName).
func IsReservedVolumeName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return n == WorkspaceVolumeName || n == UcdToolsVolumeName
}
```

Add the shared helper:

```go
// validatePodTemplateNames checks every container and volume name declared in
// pt for DNS-1123-label shape. Nil-safe.
func validatePodTemplateNames(pt *PodTemplate) error {
	for _, c := range PodTemplateContainers(pt) {
		if err := ValidateDNS1123Label(DefName(c)); err != nil {
			return fmt.Errorf("podTemplate container %w", err)
		}
	}
	for _, v := range PodTemplateVolumes(pt) {
		if err := ValidateDNS1123Label(DefName(v)); err != nil {
			return fmt.Errorf("podTemplate volume %w", err)
		}
	}
	return nil
}
```

In `internal/dsl/parse.go` `Job.Validate`, after the existing native/podTemplate incompatibility check (~line 159-161), add:

```go
	if err := validatePodTemplateNames(j.Spec.PodTemplate); err != nil {
		return err
	}
```

In `internal/dsl/jobtemplate.go` `JobTemplate.Validate`, after the steps validation, add (the typed lists convert cheaply through ToSpec's shape — validate directly):

```go
	if pt := t.Spec.PodTemplate; pt != nil {
		for _, c := range pt.Spec.Containers {
			if err := ValidateDNS1123Label(DefName(c)); err != nil {
				return fmt.Errorf("podTemplate container %w", err)
			}
		}
		for _, v := range pt.Spec.Volumes {
			if err := ValidateDNS1123Label(DefName(v)); err != nil {
				return fmt.Errorf("podTemplate volume %w", err)
			}
		}
	}
```

- [ ] **Step 4: Run to verify pass + package regression**

Run: `go test ./internal/dsl/ -count=1`
Expected: PASS. If any existing fixture uses a name the new shape validation rejects, inspect: fix the fixture if incidental (uppercase/underscore names in tests must become valid labels), never weaken the validation. NOTE: `templates/buildkit-rootless-build-push.yaml` uses names `job`/`buildkitd` (valid); examples use lowercase names — expected clean.

Also: `go test ./internal/gittemplate/ ./internal/shim/ -count=1` (both consume repo YAML).
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dsl/
git commit -m "feat(dsl): DNS-1123 name validation for podTemplate containers/volumes; normalized reserved checks"
```

---

### Task 2: `dsl` — apply-time container-reference validation for plain jobs

**Files:**
- Modify: `internal/dsl/parse.go` (`Job.Validate` + `specHasUses` helper)
- Test: `internal/dsl/parse_test.go` (or `podtemplate_names_test.go`)

**Interfaces:**
- Consumes: `ValidateContainerReferences` (exists, `container.go`).
- Produces: `func specHasUses(spec Spec) bool` (unexported).

- [ ] **Step 1: Write the failing test**

```go
func TestJobValidate_ContainerRefs_PlainVsUses(t *testing.T) {
	// Plain job with dangling container -> apply-time error.
	plainBad := `apiVersion: unified-cd/v1
kind: Job
metadata: {name: x}
spec:
  steps:
    - {name: s, container: ghost, run: echo}
`
	if _, err := Parse(strings.NewReader(plainBad)); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("plain job with dangling container must fail apply validation, got %v", err)
	}

	// Same dangling ref but the spec carries a uses: step -> deferred (passes Validate).
	usesBearing := `apiVersion: unified-cd/v1
kind: Job
metadata: {name: x}
spec:
  steps:
    - {name: s, container: ghost, run: echo}
    - name: tpl
      uses: {job: "git://github.com/org/repo/t.yaml@v1"}
`
	if _, err := Parse(strings.NewReader(usesBearing)); err != nil {
		t.Errorf("uses-bearing spec must defer container validation to resolution, got %v", err)
	}

	// Valid plain references still pass.
	plainOK := `apiVersion: unified-cd/v1
kind: Job
metadata: {name: x}
spec:
  podTemplate:
    spec:
      containers: [{name: tools, image: img}]
  steps:
    - {name: a, container: tools, run: echo}
    - {name: b, container: job, run: echo}
`
	if _, err := Parse(strings.NewReader(plainOK)); err != nil {
		t.Errorf("valid plain refs must pass: %v", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/dsl/ -run TestJobValidate_ContainerRefs -count=1`
Expected: FAIL — plain dangling ref currently passes Validate.

- [ ] **Step 3: Implement**

In `internal/dsl/parse.go`, add:

```go
// specHasUses reports whether any step (or parallel sub-step) in Steps or
// Finally is a uses: step. A uses-bearing spec cannot be container-validated
// at apply time — a template's pod-shape merge may satisfy references later —
// so validation defers to the post-resolution sweeper.
func specHasUses(spec Spec) bool {
	scan := func(entries []StepEntry) bool {
		for _, e := range entries {
			if e.Uses != nil {
				return true
			}
			for _, p := range e.Parallel {
				if p.Uses != nil {
					return true
				}
			}
		}
		return false
	}
	return scan(spec.Steps) || scan(spec.Finally)
}
```

In `Job.Validate`, after the podTemplate-name validation from Task 1, add:

```go
	// Plain (uses-free) jobs get container-reference validation at apply time;
	// uses-bearing jobs defer to the post-resolution check in the controller
	// (the template merge may supply the referenced container).
	if !specHasUses(j.Spec) {
		if err := ValidateContainerReferences(j.Spec); err != nil {
			return err
		}
	}
```

- [ ] **Step 4: Run to verify pass + regressions**

Run: `go test ./internal/dsl/ ./internal/controller/ -count=1`
Expected: PASS (controller apply tests may exercise jobs — fix incidental fixtures with genuinely dangling refs if any surface; do not weaken).

- [ ] **Step 5: Commit**

```bash
git add internal/dsl/
git commit -m "feat(dsl): apply-time container-reference validation for uses-free jobs"
```

---

### Task 3: `dsl` — JobTemplate `finally:` (schema + validate + ToSpec) + regen

**Files:**
- Modify: `internal/dsl/jobtemplate_types.go` (add `Finally`)
- Modify: `internal/dsl/jobtemplate.go` (validate + ToSpec)
- Modify (generated): `schemas/unified-cd.schema.json`, `docs/field-reference.md` via `go generate ./...`
- Test: `internal/dsl/jobtemplate_test.go`

**Interfaces:**
- Produces: `JobTemplateSpec.Finally []StepEntry`; `ToSpec()` carries it into `Spec.Finally`.

- [ ] **Step 1: Write the failing test**

Append to `internal/dsl/jobtemplate_test.go`:

```go
func TestJobTemplate_Finally(t *testing.T) {
	y := `apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: t}
spec:
  steps:
    - {name: s, run: echo}
  finally:
    - {name: cleanup, run: echo bye}
`
	tpl, err := ParseJobTemplate([]byte(y))
	if err != nil {
		t.Fatalf("finally must now be accepted on a JobTemplate: %v", err)
	}
	if len(tpl.Spec.Finally) != 1 || tpl.Spec.Finally[0].Name != "cleanup" {
		t.Fatalf("finally not parsed: %+v", tpl.Spec.Finally)
	}
	spec := tpl.ToSpec()
	if len(spec.Finally) != 1 || spec.Finally[0].Name != "cleanup" {
		t.Fatalf("ToSpec must carry finally: %+v", spec.Finally)
	}

	// Duplicate name across steps+finally rejected (shared nameSet).
	dup := `apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: t}
spec:
  steps:
    - {name: s, run: echo}
  finally:
    - {name: s, run: echo}
`
	if _, err := ParseJobTemplate([]byte(dup)); err == nil {
		t.Fatal("duplicate step name across steps/finally must be rejected")
	}
}
```

Also UPDATE the existing strict-decode test: `TestParseJobTemplate_UnknownFieldsRejected` has a `"finally"` case asserting rejection — REMOVE that case (finally is now schema-legal) and keep the others.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/dsl/ -run 'TestJobTemplate_Finally|UnknownFieldsRejected' -count=1`
Expected: `TestJobTemplate_Finally` FAILS (strict decode rejects finally today).

- [ ] **Step 3: Implement**

`internal/dsl/jobtemplate_types.go` — add to `JobTemplateSpec` (after `Steps`) and update the type comment (remove `finally` from the rejected-fields list, describe the splice):

```go
	// Finally steps run in the CALLER's finally phase (appended after the
	// caller's own finally steps, prefixed like all inlined steps). Rejected
	// in scope mode (runsIn.image), where the scope pod's lifetime ends with
	// the template body.
	Finally []StepEntry `yaml:"finally,omitempty"`
```

`internal/dsl/jobtemplate.go` `Validate` — after the steps `validateStepEntries` call, add (same shared `nameSet`, mirroring `Job.Validate`'s finally handling with `allowDeferredHooks=false, native=false`):

```go
	if err := validateStepEntries(t.Spec.Finally, "spec.finally", nameSet, false, false); err != nil {
		return err
	}
```

`ToSpec` — carry it: add `Finally: t.Spec.Finally,` to the `Spec{...}` literal.

- [ ] **Step 4: Regenerate schema/docs + run tests**

Run: `go generate ./...` (commits below include the regenerated `schemas/unified-cd.schema.json` + `docs/field-reference.md` — verify `grep -c finally schemas/unified-cd.schema.json` grew for JobTemplate).
Run: `go test ./internal/dsl/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dsl/ schemas/ docs/field-reference.md
git commit -m "feat(dsl): JobTemplate finally: schema + validation + ToSpec; regen schema/docs"
```

---

### Task 4: gittemplate — outer `if:` propagation (+`combineIf`)

**Files:**
- Modify: `internal/gittemplate/inline.go` (thread `outerIf`; `combineIf`; apply to all produced steps)
- Modify: `internal/gittemplate/resolve.go` (pass `s.If` at the call site)
- Test: `internal/gittemplate/if_propagation_test.go` (create)

**Interfaces:**
- `expandUsesStep(usesName string, with map[string]string, tplSpec dsl.Spec, outerRunsIn *dsl.RunsIn, outerContainer, outerIf string) (...)` — new trailing `outerIf` param. Exported `ExpandUsesStep` gains the same param (its only caller is tests/controller-verification — grep and update).
- `func combineIf(outer, inner string) string`.

- [ ] **Step 1: Write the failing tests**

Create `internal/gittemplate/if_propagation_test.go` (package `gittemplate_test`; reuse `resolveToSpec`/`stubFetcher`/`mustMarshalSpec` from `merge_test.go`):

```go
package gittemplate_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

const ifTmpl = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: t}
spec:
  steps:
    - {name: plain, run: echo a}
    - {name: gated, if: params.x == "1", run: echo b}
    - parallel:
        - {name: p1, run: echo c}
`

func TestResolveSpec_OuterIfPropagates(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{{
			Name: "u",
			If:   `failure()`,
			Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/t.yaml@v1"},
		}},
	})
	s, err := resolveToSpec(t, &stubFetcher{data: []byte(ifTmpl)}, specJSON)
	require.NoError(t, err)

	byName := map[string]dsl.StepEntry{}
	var parallelIf string
	for _, e := range s.Steps {
		if e.Parallel != nil {
			parallelIf = e.Parallel[0].If
			continue
		}
		byName[e.Name] = e
	}
	// Synthetic inputs step, plain body step, capture step: outer if verbatim.
	require.Equal(t, `failure()`, byName["u__inputs"].If, "inputs step must carry the outer if")
	require.Equal(t, `failure()`, byName["u__plain"].If)
	require.Equal(t, `failure()`, byName["u"].If, "capture step must carry the outer if")
	// Gated body step: AND-combined with the rewritten inner if.
	require.Contains(t, byName["u__gated"].If, "failure()")
	require.Contains(t, byName["u__gated"].If, "&&")
	require.Contains(t, byName["u__gated"].If, `== "1"`)
	// Parallel sub-step gated too.
	require.Equal(t, `failure()`, parallelIf)
}

func TestResolveSpec_NoOuterIf_InnerPreserved(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{{
			Name: "u",
			Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/t.yaml@v1"},
		}},
	})
	s, err := resolveToSpec(t, &stubFetcher{data: []byte(ifTmpl)}, specJSON)
	require.NoError(t, err)
	for _, e := range s.Steps {
		if e.Name == "u__gated" {
			require.NotContains(t, e.If, "&&", "no outer if -> inner if unchanged")
			require.Contains(t, e.If, `== "1"`)
		}
		if e.Name == "u__plain" {
			require.Empty(t, e.If)
		}
	}
}
```

Add a unit test for the combiner (package `gittemplate`, e.g. in a new `internal/gittemplate/combineif_test.go`):

```go
package gittemplate

import "testing"

func TestCombineIf(t *testing.T) {
	cases := []struct{ outer, inner, want string }{
		{"", "", ""},
		{"failure()", "", "failure()"},
		{"", "params.x == \"1\"", "params.x == \"1\""},
		{"failure()", "params.x == \"1\"", "(failure()) && (params.x == \"1\")"},
	}
	for _, c := range cases {
		if got := combineIf(c.outer, c.inner); got != c.want {
			t.Errorf("combineIf(%q,%q) = %q, want %q", c.outer, c.inner, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/gittemplate/ -run 'OuterIf|CombineIf|NoOuterIf' -count=1`
Expected: FAIL (no propagation today; `combineIf` undefined).

- [ ] **Step 3: Implement**

`internal/gittemplate/inline.go`:

```go
// combineIf merges the outer uses step's if: with an inlined step's own
// (already ref-rewritten) if:. Both are CEL; parenthesized && keeps each
// operand's semantics. Empty operands drop out. Note condition.go's
// implicit-success rule keys on the presence of a status function
// (failure()/success()/always()) anywhere in the text — an outer failure()
// therefore correctly overrides the main-DAG implicit skip for the whole
// combined expression.
func combineIf(outer, inner string) string {
	switch {
	case outer == "" && inner == "":
		return ""
	case outer == "":
		return inner
	case inner == "":
		return outer
	default:
		return "(" + outer + ") && (" + inner + ")"
	}
}
```

Signature: add trailing `outerIf string` to `expandUsesStep` (and `ExpandUsesStep`; update its callers — grep `ExpandUsesStep(` repo-wide). Apply at the four construction sites:
- `__inputs` step (inline.go ~256): add `If: outerIf,`.
- Parallel sub-steps (~271): change `ns.If = rewriteRefs(ps.If, usesName, innerNames)` → `ns.If = combineIf(outerIf, rewriteRefs(ps.If, usesName, innerNames))`.
- Concrete steps (~332): `If: combineIf(outerIf, rewriteRefs(inner.If, usesName, innerNames)),`.
- Capture step (~437): add `If: outerIf,`.

`internal/gittemplate/resolve.go` call site (~205): pass `s.If` as the new argument.

- [ ] **Step 4: Run tests + package**

Run: `go test ./internal/gittemplate/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gittemplate/
git commit -m "fix(gittemplate): propagate a uses step's if: to its whole expansion (G: silent drop)"
```

---

### Task 5: gittemplate — template `finally:` splice

**Files:**
- Modify: `internal/gittemplate/inline.go` (expand finally list; scope-mode rejection)
- Modify: `internal/gittemplate/resolve.go` (bubble + append to caller Finally)
- Test: `internal/gittemplate/finally_splice_test.go` (create)

**Interfaces:**
- `expandUsesStep` returns the expanded template-finally steps: widen `podContribution` with `finally []dsl.StepEntry` (keeps signatures stable) OR add a 4th return — choose the contribution-struct field (less churn). `resolveSteps` accumulates `contrib.finally` across nesting; `ResolveSpec` appends to `spec.Finally` AFTER resolving the caller's own finally (so caller finally steps precede spliced ones), then runs the existing `checkGlobalNameCollisions`.

- [ ] **Step 1: Write the failing tests**

Create `internal/gittemplate/finally_splice_test.go` (package `gittemplate_test`):

```go
package gittemplate_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

const finallyTpl = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: t}
spec:
  steps:
    - {name: work, run: echo w}
  finally:
    - {name: cleanup, if: failure(), run: echo bye}
`

func TestResolveSpec_TemplateFinallySplices(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{{Name: "u", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/t.yaml@v1"}}},
		Finally: []dsl.StepEntry{{Name: "callerFin", Run: "echo mine"}},
	})
	s, err := resolveToSpec(t, &stubFetcher{data: []byte(finallyTpl)}, specJSON)
	require.NoError(t, err)
	require.Len(t, s.Finally, 2)
	require.Equal(t, "callerFin", s.Finally[0].Name, "caller's own finally steps come first")
	require.Equal(t, "u__cleanup", s.Finally[1].Name, "template finally appended, prefixed")
	require.Equal(t, "failure()", s.Finally[1].If)
}

func TestResolveSpec_TemplateFinally_OuterIfCombined(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{{Name: "u", If: `params.go == "1"`, Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/t.yaml@v1"}}},
	})
	s, err := resolveToSpec(t, &stubFetcher{data: []byte(finallyTpl)}, specJSON)
	require.NoError(t, err)
	require.Len(t, s.Finally, 1)
	require.Contains(t, s.Finally[0].If, "&&")
	require.Contains(t, s.Finally[0].If, "failure()")
}

func TestResolveSpec_TemplateFinally_ScopeModeRejected(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{{
			Name:   "u",
			Uses:   &dsl.UsesStep{Job: "git://github.com/org/repo/t.yaml@v1"},
			RunsIn: &dsl.RunsIn{Image: "alpine:3"},
		}},
	})
	_, err := resolveToSpec(t, &stubFetcher{data: []byte(finallyTpl)}, specJSON)
	require.Error(t, err)
	require.Contains(t, err.Error(), "finally")
}

func TestResolveSpec_TemplateFinally_NameCollisionErrors(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps:   []dsl.StepEntry{{Name: "u", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/t.yaml@v1"}}},
		Finally: []dsl.StepEntry{{Name: "u__cleanup", Run: "echo clash"}},
	})
	_, err := resolveToSpec(t, &stubFetcher{data: []byte(finallyTpl)}, specJSON)
	require.Error(t, err)
	require.Contains(t, err.Error(), "u__cleanup")
}

func TestResolveSpec_UsesInFinally_WithTemplateFinally(t *testing.T) {
	// A uses step sitting in the caller's finally whose template ALSO has finally:
	// both the body expansion and the spliced finally land in spec.Finally.
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps:   []dsl.StepEntry{{Name: "main", Run: "echo hi"}},
		Finally: []dsl.StepEntry{{Name: "fin", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/t.yaml@v1"}}},
	})
	s, err := resolveToSpec(t, &stubFetcher{data: []byte(finallyTpl)}, specJSON)
	require.NoError(t, err)
	names := map[string]bool{}
	for _, e := range s.Finally {
		names[e.Name] = true
	}
	require.True(t, names["fin__work"], "body expansion in finally")
	require.True(t, names["fin__cleanup"], "template finally spliced too; got %v", names)
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/gittemplate/ -run 'TemplateFinally|UsesInFinally_WithTemplateFinally' -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement**

`inline.go`:
- Widen the contribution: `type podContribution struct { containers, volumes []map[string]any; finally []dsl.StepEntry }` (update the doc comment: it now carries everything a template contributes OUTSIDE the caller-DAG position).
- In `expandUsesStep`, after the scope-mode podTemplate rejection, add:

```go
	if scopeMode && len(tplSpec.Finally) > 0 {
		return nil, podContribution{}, fmt.Errorf("template declares finally:, but this uses: step has runsIn.image (scope mode): the scope pod's lifetime ends with the template body, so its finally cannot be honored")
	}
```

- Expand the finally list with the SAME transformations as body steps (prefix, `rewriteRefs` on Run/If/Env/Outputs, shell stamping, `combineIf(outerIf, ...)`, cache/artifact sub-struct handling). Factor the existing per-step renaming logic into a helper if it isn't already reusable (`renameStep(inner dsl.Step|StepEntry, ...)`) — do NOT duplicate the whole block twice; extract a function both the body loop and the finally loop call. Template finally steps must NOT include synthetic inputs/capture steps (they reference `usesName__inputs` outputs via the same rewriting, which is valid — the inputs step lives in the main DAG and runs before finally by definition). Note: parallel blocks inside template finally are allowed iff `validateStepEntries` allowed them (it does — same entry validator); handle them like body parallels.
- Append expanded finally steps to `contrib.finally` (also stamp shell like body steps).

`resolve.go`:
- `resolveSteps`: accumulate `nestedContrib.finally` and `expandContrib.finally` into `contrib.finally` (alongside containers/volumes).
- `ResolveSpec`: after BOTH lists are resolved and contributions merged, append: `spec.Finally = append(spec.Finally, contrib.finally...)` — where `contrib` is the combined main+finally contribution. Order guarantee: caller's resolved finally first, then spliced template-finally in encounter order. Then the existing `checkGlobalNameCollisions(spec.Steps, spec.Finally)` runs LAST (it already exists — ensure the splice happens before it).

- [ ] **Step 4: Run tests + full gittemplate + dsl**

Run: `go test ./internal/gittemplate/ ./internal/dsl/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gittemplate/
git commit -m "feat(gittemplate): splice a JobTemplate's finally: into the caller's finally"
```

---

### Task 6: Docs + full sweep

**Files:**
- Modify: `docs/jobs.md` (uses `if:` semantics; template `finally:` contract + ordering; apply-time validation behavior change), `docs/resources.md` (JobTemplate section gains finally), `docs/migration-2026-07-uses-jobtemplate.md` (finally now supported — update the "template declaring finally" row), `docs/troubleshooting.md` (new parse errors: DNS-1123 names, apply-time dangling container).

- [ ] **Step 1: Update the docs** per the spec's Docs section: (1) `uses:` steps support `if:` (whole expansion gated; combined with inner ifs; status-function semantics same as plain steps); (2) JobTemplate `finally:` — spliced after the caller's own finally, prefixed, rejected in scope mode; (3) plain jobs now fail apply on dangling `container:` refs; (4) podTemplate container/volume names must be DNS-1123 labels (apply-time error). Update the migration guide's finally row (was "remove the field"; now "supported — spliced into the caller's finally").

- [ ] **Step 2: Full sweep**

Run: `go build ./... && go generate ./...` (confirm no further generated diff), `go vet ./internal/dsl/ ./internal/gittemplate/ ./internal/controller/`, and the FULL suite `go test ./... -count=1`.
Expected: all green (shim corpus consumes templates/examples — new validation must not break them; fix incidental fixtures, never weaken validation).

- [ ] **Step 3: Commit**

```bash
git add docs/
git commit -m "docs: uses if: propagation, JobTemplate finally, apply-time validation"
```

---

## Notes for the executor

- Order matters: 1→2 (dsl validation) are independent of 3 (finally schema), but 5 needs 3 (Finally field) and 4 (`combineIf` + outerIf threading). Execute 1,2,3,4,5,6.
- `ExpandUsesStep`'s exported signature changes in Task 4 (gains `outerIf`) — grep callers (`internal/controller`?) and update; if the only non-test caller passes "", that's correct.
- When factoring the step-renaming helper in Task 5, keep behavior byte-identical for body steps (the if-propagation tests from Task 4 must stay green).
- Reuse `resolveToSpec`/`stubFetcher`/`mustMarshalSpec` from `merge_test.go` — same package `gittemplate_test`.
- Full-suite before finishing: `go test ./... -count=1` (merge-discipline rule).
