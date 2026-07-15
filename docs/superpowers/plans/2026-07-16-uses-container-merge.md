# `uses:` JobTemplate Resource + Pod-Shape Merge + Finally Resolution Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Require `uses:` targets to be a new **`kind: JobTemplate`** resource whose strict schema contains only what `uses:` can honor; merge the template's `podTemplate` containers **and volumes** into the caller (gap-fill); resolve `uses:` steps in `finally:` (today never resolved); validate `container:` references at run creation.

**Architecture:** A new `dsl.JobTemplate` type (strict-decoded via the codebase's standard `KnownFields(true)` pattern) replaces `dsl.Job` as the fetched-template shape; `JobTemplate.ToSpec()` feeds the existing `expandUsesStep`. Container/volume definitions the caller lacks are collected during expansion (`internal/gittemplate/inline.go`), bubbled up through the resolve recursion as a `podContribution`, and merged into the caller's `spec.PodTemplate` in `ResolveSpec` — which now also resolves `spec.Finally`. A pure `dsl` helper validates the resolved spec's container references; the controller's git-resolution sweeper calls it after a successful resolve and fails the run deterministically on error.

**Tech Stack:** Go; `internal/dsl` (types + pure helpers), `internal/gittemplate` (resolve/inline), `internal/controller/scheduler.go`. Tests reuse the existing `stubFetcher`/`mapFetcher`/`NewResolver`/`mustMarshalSpec` harness in `internal/gittemplate/resolve_test.go` and the `store.NewTestPostgres` harness in `internal/controller/scheduler_test.go`.

## Global Constraints

- **`uses:` targets must be `kind: JobTemplate`.** A fetched `kind: Job` → resolution error: `uses: targets must be kind: JobTemplate (got kind: Job); convert the template, or invoke the job with call:`. JobTemplate schema: `apiVersion`, `kind`, `metadata{name,labels,annotations}`, `spec{description, params, shell, podTemplate{spec{containers, volumes}}, steps}`. Strict decode (`yaml.Decoder.KnownFields(true)`) — any other field errors naming the field. No dynamic field-guard enumeration anywhere.
- **Pod-shape merge: non-scope `uses:` only** (outer uses step has no `runsIn.image`). Scope mode contributes nothing; a scope-mode JobTemplate that declares a `podTemplate` at all is a resolution ERROR (not a silent drop).
- **Reserved names never injectable:** containers `job` / `unified-artifact`; volumes `workspace` / `ucd-tools`. Template declaring one → resolution error.
- **Merge rule:** gap-fill only. Same name in caller and template (containers or volumes): **JSON-equal → dedup** (keep caller's/first); **differing → resolution error**.
- **Finally resolution:** `HasGitURIs` scans `spec.Finally` too; `ResolveSpec` resolves `spec.Finally` via the same `resolveSteps` path; expanded names must be globally unique across Steps + Finally.
- **Reserved-name constants live in `dsl`** (`PrimaryContainerName`, `ArtifactSidecarContainerName`, `WorkspaceVolumeName`, `UcdToolsVolumeName`); `k8sagent`'s existing literals alias them.
- **Validation placement:** `dsl.ValidateContainerReferences(spec)` runs in `internal/controller/scheduler.go` `resolveGitPendingRuns`, after a successful `ResolveSpec`, before `UpdateRunSpec`; on error the run is failed deterministically (system log line + `MarkRunFinished(RunFailed)` + `bo.Success`).
- All new failures are **deterministic resolution errors** (`newResolveError` in gittemplate — check `IsResolveError` fires; plain error from `dsl`), never silent, never deferred to exec time.
- Do NOT use `-race` in tests (CGO disabled in this env).

---

### Task 1: `dsl` reserved-name constants + container/volume accessors

**Files:**
- Create: `internal/dsl/container.go`
- Modify: `internal/k8sagent/podbuilder.go` (alias `artifactSidecarName` and `ucdToolsVolume` to the dsl constants)
- Test: `internal/dsl/container_test.go`

**Interfaces:**
- Produces:
  - `const dsl.PrimaryContainerName = "job"`, `const dsl.ArtifactSidecarContainerName = "unified-artifact"`, `const dsl.WorkspaceVolumeName = "workspace"`, `const dsl.UcdToolsVolumeName = "ucd-tools"`
  - `func dsl.IsReservedContainerName(name string) bool`, `func dsl.IsReservedVolumeName(name string) bool`
  - `func dsl.PodTemplateContainers(pt *PodTemplate) []map[string]any`, `func dsl.PodTemplateVolumes(pt *PodTemplate) []map[string]any` (nil-safe; defs in declared order)
  - `func dsl.DefName(def map[string]any) string` (the `"name"` field of a definition map, or "")

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
	for _, n := range []string{"", "foo", "Job", "workspace"} {
		if IsReservedContainerName(n) {
			t.Errorf("%q should not be a reserved container name", n)
		}
	}
}

func TestIsReservedVolumeName(t *testing.T) {
	for _, n := range []string{"workspace", "ucd-tools"} {
		if !IsReservedVolumeName(n) {
			t.Errorf("%q should be reserved", n)
		}
	}
	for _, n := range []string{"", "cache", "job"} {
		if IsReservedVolumeName(n) {
			t.Errorf("%q should not be a reserved volume name", n)
		}
	}
}

func TestPodTemplateAccessors(t *testing.T) {
	if got := PodTemplateContainers(nil); got != nil {
		t.Fatalf("nil template containers: want nil, got %v", got)
	}
	if got := PodTemplateVolumes(nil); got != nil {
		t.Fatalf("nil template volumes: want nil, got %v", got)
	}
	pt := &PodTemplate{Spec: map[string]any{
		"containers": []any{
			map[string]any{"name": "foo", "image": "x"},
			"not-a-map", // skipped
		},
		"volumes": []any{
			map[string]any{"name": "cache", "emptyDir": map[string]any{}},
		},
	}}
	cs := PodTemplateContainers(pt)
	if len(cs) != 1 || DefName(cs[0]) != "foo" {
		t.Fatalf("containers: got %v", cs)
	}
	vs := PodTemplateVolumes(pt)
	if len(vs) != 1 || DefName(vs[0]) != "cache" {
		t.Fatalf("volumes: got %v", vs)
	}
	if DefName(map[string]any{}) != "" {
		t.Fatalf("missing name should be empty string")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/dsl/ -run 'IsReservedContainerName|IsReservedVolumeName|PodTemplateAccessors' -count=1`
Expected: FAIL (compile error — undefined identifiers).

- [ ] **Step 3: Create the helpers**

Create `internal/dsl/container.go`:

```go
package dsl

import "fmt"

// Reserved container/volume names are injected/owned by the system, never
// user- or template-supplied. PrimaryContainerName is the default exec target
// for a step with no container:. ArtifactSidecarContainerName is the internal
// artifact/cache sidecar. WorkspaceVolumeName is the injected workspace volume;
// UcdToolsVolumeName carries the ucd-sh shim (see internal/k8sagent/podbuilder.go,
// whose constants alias these). A uses: template may not inject any of them, and
// the reserved container names are always valid container: targets even when
// absent from a podTemplate's container list.
const (
	PrimaryContainerName         = "job"
	ArtifactSidecarContainerName = "unified-artifact"
	WorkspaceVolumeName          = "workspace"
	UcdToolsVolumeName           = "ucd-tools"
)

// IsReservedContainerName reports whether name is a system-reserved container name.
func IsReservedContainerName(name string) bool {
	return name == PrimaryContainerName || name == ArtifactSidecarContainerName
}

// IsReservedVolumeName reports whether name is a system-reserved volume name.
func IsReservedVolumeName(name string) bool {
	return name == WorkspaceVolumeName || name == UcdToolsVolumeName
}

// PodTemplateContainers returns pt's container definition maps (from
// pt.Spec["containers"]) in declared order. Nil-safe; skips non-map entries.
func PodTemplateContainers(pt *PodTemplate) []map[string]any {
	return podTemplateDefs(pt, "containers")
}

// PodTemplateVolumes returns pt's volume definition maps (from
// pt.Spec["volumes"]) in declared order. Nil-safe; skips non-map entries.
func PodTemplateVolumes(pt *PodTemplate) []map[string]any {
	return podTemplateDefs(pt, "volumes")
}

func podTemplateDefs(pt *PodTemplate, key string) []map[string]any {
	if pt == nil {
		return nil
	}
	raw, _ := pt.Spec[key].([]any)
	var out []map[string]any
	for _, r := range raw {
		if d, ok := r.(map[string]any); ok {
			out = append(out, d)
		}
	}
	return out
}

// DefName returns the "name" field of a container/volume definition map, or "".
func DefName(def map[string]any) string {
	n, _ := def["name"].(string)
	return n
}
```

(The `fmt` import is used by Task 2's `ValidateContainerReferences` in this same file; if the compiler complains about an unused import at this step, add it in Task 2 instead.)

- [ ] **Step 4: Alias the k8sagent constants**

In `internal/k8sagent/podbuilder.go`, change:

```go
const artifactSidecarName = "unified-artifact"
```
to:
```go
const artifactSidecarName = dsl.ArtifactSidecarContainerName
```

and:

```go
const ucdToolsVolume = "ucd-tools"
```
to:
```go
const ucdToolsVolume = dsl.UcdToolsVolumeName
```

Confirm the file imports `dsl` (it does — check; add if missing).

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/dsl/ -run 'IsReserved|PodTemplateAccessors' -count=1 && go build ./internal/k8sagent/`
Expected: PASS and clean build.

- [ ] **Step 6: Commit**

```bash
git add internal/dsl/container.go internal/dsl/container_test.go internal/k8sagent/podbuilder.go
git commit -m "feat(dsl): reserved container/volume name constants + podTemplate accessors"
```

---

### Task 2: `dsl.ValidateContainerReferences`

**Files:**
- Modify: `internal/dsl/container.go`
- Test: `internal/dsl/container_test.go`

**Interfaces:**
- Consumes: `PodTemplateContainers`, `DefName`, `IsReservedContainerName` (Task 1).
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
			{Name: "a", Run: "echo"},                  // empty container -> ok
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

Append to `internal/dsl/container.go` (ensure `import "fmt"` is present):

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
		if n := DefName(c); n != "" {
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

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/dsl/ -run 'TestValidateContainerReferences|IsReserved|PodTemplateAccessors' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dsl/container.go internal/dsl/container_test.go
git commit -m "feat(dsl): ValidateContainerReferences over a resolved spec"
```

---

### Task 3: `dsl.JobTemplate` — strict-schema template resource

**Files:**
- Create: `internal/dsl/jobtemplate.go`
- Test: `internal/dsl/jobtemplate_test.go`

**Interfaces:**
- Consumes: `validateStepEntries(entries, pathPrefix, nameSet, allowDeferredHooks, native)` and `validShellArgv` (existing unexported helpers in `internal/dsl/parse.go`); `checkNeedsInEntries` (parse.go); `ValidateName`; `SupportedAPIVersion`.
- Produces:
  - Types: `JobTemplate{APIVersion, Kind, Metadata, Spec JobTemplateSpec}`, `JobTemplateSpec{Description string; Params Params; Shell []string; PodTemplate *JobTemplatePodTemplate; Steps []StepEntry}`, `JobTemplatePodTemplate{Spec JobTemplatePodSpec}`, `JobTemplatePodSpec{Containers []map[string]any; Volumes []map[string]any}`.
  - `func ParseJobTemplate(data []byte) (*JobTemplate, error)` — strict decode + validate. A `kind: Job` document returns an error containing `kind: JobTemplate` and `call:` (the convert-or-call guidance).
  - `func (t *JobTemplate) ToSpec() Spec` — converts to the `dsl.Spec` shape `expandUsesStep` consumes (podTemplate subset → `PodTemplate{Spec: map[string]any{"containers": []any{...}, "volumes": []any{...}}}`; omitted when the template has no podTemplate).

- [ ] **Step 1: Write the failing test**

Create `internal/dsl/jobtemplate_test.go`:

```go
package dsl

import (
	"strings"
	"testing"
)

const validTemplateYAML = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: tools-tmpl}
spec:
  description: builds with tools
  params:
    inputs:
      - {name: target, type: string, default: all}
  shell: ["/bin/sh", "-c"]
  podTemplate:
    spec:
      containers:
        - {name: tools, image: alpine:3}
      volumes:
        - {name: toolcache, emptyDir: {}}
  steps:
    - {name: build, container: tools, run: make $target}
`

func TestParseJobTemplate_Valid(t *testing.T) {
	tpl, err := ParseJobTemplate([]byte(validTemplateYAML))
	if err != nil {
		t.Fatalf("valid template must parse, got %v", err)
	}
	if tpl.Metadata.Name != "tools-tmpl" || len(tpl.Spec.Steps) != 1 {
		t.Fatalf("unexpected parse result: %+v", tpl)
	}
	if len(tpl.Spec.PodTemplate.Spec.Containers) != 1 || len(tpl.Spec.PodTemplate.Spec.Volumes) != 1 {
		t.Fatalf("podTemplate subset not parsed: %+v", tpl.Spec.PodTemplate)
	}
}

func TestParseJobTemplate_KindJobRejectedWithGuidance(t *testing.T) {
	y := strings.Replace(validTemplateYAML, "kind: JobTemplate", "kind: Job", 1)
	_, err := ParseJobTemplate([]byte(y))
	if err == nil {
		t.Fatal("kind: Job must be rejected")
	}
	if !strings.Contains(err.Error(), "kind: JobTemplate") || !strings.Contains(err.Error(), "call:") {
		t.Fatalf("error must guide conversion or call:, got %v", err)
	}
}

func TestParseJobTemplate_UnknownFieldsRejected(t *testing.T) {
	cases := map[string]string{
		"agentSelector":       "spec:\n  agentSelector: [gpu]\n  steps:\n    - {name: s, run: echo}",
		"finally":             "spec:\n  steps:\n    - {name: s, run: echo}\n  finally:\n    - {name: f, run: echo}",
		"podTemplate.reuse":   "spec:\n  podTemplate:\n    reuse: true\n    spec:\n      containers: [{name: t, image: x}]\n  steps:\n    - {name: s, run: echo}",
		"podSpec nodeSelector": "spec:\n  podTemplate:\n    spec:\n      nodeSelector: {disk: ssd}\n      containers: [{name: t, image: x}]\n  steps:\n    - {name: s, run: echo}",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			y := "apiVersion: unified-cd/v1\nkind: JobTemplate\nmetadata: {name: x}\n" + body + "\n"
			if _, err := ParseJobTemplate([]byte(y)); err == nil {
				t.Fatalf("unknown field %s must be rejected by strict decode", name)
			}
		})
	}
}

func TestParseJobTemplate_BasicValidation(t *testing.T) {
	noSteps := "apiVersion: unified-cd/v1\nkind: JobTemplate\nmetadata: {name: x}\nspec: {}\n"
	if _, err := ParseJobTemplate([]byte(noSteps)); err == nil {
		t.Fatal("a template with no steps must be rejected")
	}
	noName := "apiVersion: unified-cd/v1\nkind: JobTemplate\nmetadata: {}\nspec:\n  steps:\n    - {name: s, run: echo}\n"
	if _, err := ParseJobTemplate([]byte(noName)); err == nil {
		t.Fatal("a template with no metadata.name must be rejected")
	}
}

func TestJobTemplateToSpec(t *testing.T) {
	tpl, err := ParseJobTemplate([]byte(validTemplateYAML))
	if err != nil {
		t.Fatal(err)
	}
	spec := tpl.ToSpec()
	if len(spec.Steps) != 1 || len(spec.Shell) != 2 || len(spec.Params.Inputs) != 1 {
		t.Fatalf("ToSpec basic fields: %+v", spec)
	}
	if got := DefName(PodTemplateContainers(spec.PodTemplate)[0]); got != "tools" {
		t.Fatalf("ToSpec containers: got %q", got)
	}
	if got := DefName(PodTemplateVolumes(spec.PodTemplate)[0]); got != "toolcache" {
		t.Fatalf("ToSpec volumes: got %q", got)
	}

	// No podTemplate -> nil in the produced Spec.
	plain := "apiVersion: unified-cd/v1\nkind: JobTemplate\nmetadata: {name: x}\nspec:\n  steps:\n    - {name: s, run: echo}\n"
	tpl2, err := ParseJobTemplate([]byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	if tpl2.ToSpec().PodTemplate != nil {
		t.Fatal("template without podTemplate must produce a nil Spec.PodTemplate")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/dsl/ -run 'ParseJobTemplate|JobTemplateToSpec' -count=1`
Expected: FAIL (undefined types/functions).

- [ ] **Step 3: Implement**

Create `internal/dsl/jobtemplate.go`:

```go
package dsl

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// JobTemplate is the resource a uses: step points at. Unlike a full Job, its
// schema contains ONLY what uses: can honor — the template's steps are inlined
// into the CALLER's run and pod, so fields that would shape a different pod,
// agent, or run (agentSelector, concurrency, timeoutMinutes, native, finally,
// podTemplate reuse/workspace/override, pod-level spec keys) do not exist here
// and are rejected by strict decoding. A job that needs its own pod/agent/run
// semantics should be invoked with call: instead.
type JobTemplate struct {
	APIVersion string          `yaml:"apiVersion"`
	Kind       string          `yaml:"kind"`
	Metadata   Metadata        `yaml:"metadata"`
	Spec       JobTemplateSpec `yaml:"spec"`
}

// JobTemplateSpec is the uses:-supported subset of a job spec.
type JobTemplateSpec struct {
	Description string                  `yaml:"description,omitempty"`
	Params      Params                  `yaml:"params,omitempty"`
	Shell       []string                `yaml:"shell,omitempty"`
	PodTemplate *JobTemplatePodTemplate `yaml:"podTemplate,omitempty"`
	Steps       []StepEntry             `yaml:"steps"`
}

// JobTemplatePodTemplate is the pod-shape subset a template may contribute to
// the caller's pod: containers and the volumes they mount. Nothing else.
type JobTemplatePodTemplate struct {
	Spec JobTemplatePodSpec `yaml:"spec,omitempty"`
}

// JobTemplatePodSpec holds the mergeable pod-shape lists.
type JobTemplatePodSpec struct {
	Containers []map[string]any `yaml:"containers,omitempty"`
	Volumes    []map[string]any `yaml:"volumes,omitempty"`
}

// ParseJobTemplate strictly decodes and validates a kind: JobTemplate document.
// A kind: Job document gets an explicit conversion hint.
func ParseJobTemplate(data []byte) (*JobTemplate, error) {
	// Pre-sniff kind for a friendly error before the strict decode (a Job
	// document would otherwise fail on its first Job-only field, which is a
	// confusing message for what is really a wrong-kind problem).
	var head struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
	}
	if err := yaml.Unmarshal(data, &head); err != nil {
		return nil, err
	}
	if head.Kind == "Job" {
		return nil, fmt.Errorf("uses: targets must be kind: JobTemplate (got kind: Job); convert the template, or invoke the job with call:")
	}

	// The same forbidden-field pre-checks Job parsing applies (clear errors
	// for removed syntax like needs:).
	if err := checkForbiddenJobFields(data); err != nil {
		return nil, err
	}

	var tpl JobTemplate
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&tpl); err != nil {
		return nil, err
	}
	if err := tpl.Validate(); err != nil {
		return nil, err
	}
	return &tpl, nil
}

// Validate checks the JobTemplate's own invariants, reusing the step-level
// validation Job.Validate uses (native=false: a template is never native).
func (t *JobTemplate) Validate() error {
	if t.APIVersion != SupportedAPIVersion {
		return fmt.Errorf("unsupported apiVersion %q (want %q)", t.APIVersion, SupportedAPIVersion)
	}
	if t.Kind != "JobTemplate" {
		return fmt.Errorf("unsupported kind %q (want \"JobTemplate\")", t.Kind)
	}
	if t.Metadata.Name == "" {
		return fmt.Errorf("metadata.name is required")
	}
	if err := ValidateName(t.Metadata.Name); err != nil {
		return fmt.Errorf("metadata.name %w", err)
	}
	if len(t.Spec.Steps) == 0 {
		return fmt.Errorf("spec.steps must contain at least one step")
	}
	if err := validShellArgv(t.Spec.Shell); err != nil {
		return fmt.Errorf("spec.shell: %w", err)
	}
	nameSet := map[string]bool{}
	if err := validateStepEntries(t.Spec.Steps, "spec.steps", nameSet, true, false); err != nil {
		return err
	}
	for i, p := range t.Spec.Params.Inputs {
		if p.Name == "" {
			return fmt.Errorf("spec.params.inputs[%d].name is required", i)
		}
	}
	return nil
}

// ToSpec converts the template into the dsl.Spec shape the uses: expansion
// consumes: the podTemplate subset becomes a regular PodTemplate whose Spec map
// carries only containers/volumes.
func (t *JobTemplate) ToSpec() Spec {
	spec := Spec{
		Params:      t.Spec.Params,
		Shell:       t.Spec.Shell,
		Steps:       t.Spec.Steps,
		Description: t.Spec.Description,
	}
	if pt := t.Spec.PodTemplate; pt != nil {
		m := map[string]any{}
		if len(pt.Spec.Containers) > 0 {
			list := make([]any, 0, len(pt.Spec.Containers))
			for _, c := range pt.Spec.Containers {
				list = append(list, c)
			}
			m["containers"] = list
		}
		if len(pt.Spec.Volumes) > 0 {
			list := make([]any, 0, len(pt.Spec.Volumes))
			for _, v := range pt.Spec.Volumes {
				list = append(list, v)
			}
			m["volumes"] = list
		}
		spec.PodTemplate = &PodTemplate{Spec: m}
	}
	return spec
}
```

Check the exact validation the Job's params outputs get (`parse.go` after inputs) — if `Job.Validate` also validates `Params.Outputs` names, mirror that loop too. Match what exists; do not invent extra rules.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/dsl/ -run 'ParseJobTemplate|JobTemplateToSpec' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dsl/jobtemplate.go internal/dsl/jobtemplate_test.go
git commit -m "feat(dsl): kind: JobTemplate resource — strict uses: target schema"
```

---

### Task 4: gittemplate — kind gate, pod-shape contribution (containers + volumes) + merge

**Files:**
- Modify: `internal/gittemplate/inline.go` (`expandUsesStep` returns a `podContribution`; guard; `ExpandUsesStep` wrapper adapts)
- Modify: `internal/gittemplate/resolve.go` (`resolveSteps` accumulates; `ResolveSpec` merges via `mergeContribution`)
- Test: `internal/gittemplate/merge_test.go` (create)

**Interfaces:**
- Consumes: `dsl.PodTemplateContainers`, `dsl.PodTemplateVolumes`, `dsl.DefName`, `dsl.IsReservedContainerName`, `dsl.IsReservedVolumeName` (Task 1); `dsl.ParseJobTemplate` + `JobTemplate.ToSpec` (Task 3).
- Produces (internal to package):
  - `type podContribution struct { containers, volumes []map[string]any }`
  - `expandUsesStep(...) ([]dsl.StepEntry, podContribution, error)` — contribution is non-scope only; reserved names rejected; scope-mode + template-podTemplate rejected.
  - `resolveSteps(...) ([]dsl.StepEntry, podContribution, error)` — fetched YAML parsed via `dsl.ParseJobTemplate` (kind gate), converted via `ToSpec`; accumulates contributions across siblings and nested `uses`.
  - `mergeContribution(spec *dsl.Spec, contrib podContribution) error` — gap-fills `containers` and `volumes` into `spec.PodTemplate`, dedup-identical / error-differing.
  - `ExpandUsesStep` (exported) keeps its current `([]dsl.StepEntry, error)` signature.

- [ ] **Step 1: Write the failing tests**

Create `internal/gittemplate/merge_test.go`. Notes: `stubFetcher`, `mapFetcher`, `mustMarshalSpec`, and the no-op `CredentialFunc` used by existing tests live in `internal/gittemplate/resolve_test.go` (package `gittemplate_test`) — reuse them (grep for the exact names; if the credential fn has a different name, use that). Confirm `dsl.RunsIn`'s image field is `Image`.

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

// resolveToSpec resolves specJSON with the given fetcher and unmarshals the result.
func resolveToSpec(t *testing.T, fetcher gittemplate.FetcherInterface, specJSON []byte) (dsl.Spec, error) {
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

func defNames(defs []map[string]any) []string {
	var out []string
	for _, d := range defs {
		out = append(out, dsl.DefName(d))
	}
	return out
}

// Template with its own container + the volume it mounts, and a step targeting it.
const tmplWithToolsAndVolume = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: tmpl}
spec:
  podTemplate:
    spec:
      containers:
        - name: tools
          image: alpine:3
          volumeMounts: [{name: toolcache, mountPath: /tc}]
      volumes:
        - {name: toolcache, emptyDir: {}}
  steps:
    - name: run-in-tools
      container: tools
      run: echo hi
`

func TestResolveSpec_MergesTemplateContainerAndVolume(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{{Name: "u", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/tmpl.yaml@v1"}}},
	})
	s, err := resolveToSpec(t, &stubFetcher{data: []byte(tmplWithToolsAndVolume)}, specJSON)
	require.NoError(t, err)
	require.Contains(t, defNames(dsl.PodTemplateContainers(s.PodTemplate)), "tools", "template container must be merged")
	require.Contains(t, defNames(dsl.PodTemplateVolumes(s.PodTemplate)), "toolcache", "template volume must be merged")
}

func TestResolveSpec_CallerContainerKept_IdenticalDedup(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{
		PodTemplate: &dsl.PodTemplate{Spec: map[string]any{"containers": []any{
			map[string]any{"name": "tools", "image": "alpine:3", "volumeMounts": []any{map[string]any{"name": "toolcache", "mountPath": "/tc"}}},
		}}},
		Steps: []dsl.StepEntry{{Name: "u", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/tmpl.yaml@v1"}}},
	})
	s, err := resolveToSpec(t, &stubFetcher{data: []byte(tmplWithToolsAndVolume)}, specJSON)
	require.NoError(t, err)
	n := 0
	for _, name := range defNames(dsl.PodTemplateContainers(s.PodTemplate)) {
		if name == "tools" {
			n++
		}
	}
	require.Equal(t, 1, n, "identical caller+template container must dedup to one")
	require.Contains(t, defNames(dsl.PodTemplateVolumes(s.PodTemplate)), "toolcache", "volume still merged")
}

func TestResolveSpec_CollisionDiffers_Errors(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{
		PodTemplate: &dsl.PodTemplate{Spec: map[string]any{"containers": []any{
			map[string]any{"name": "tools", "image": "ubuntu:22.04"},
		}}},
		Steps: []dsl.StepEntry{{Name: "u", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/tmpl.yaml@v1"}}},
	})
	_, err := resolveToSpec(t, &stubFetcher{data: []byte(tmplWithToolsAndVolume)}, specJSON)
	require.Error(t, err)
	require.True(t, gittemplate.IsResolveError(err), "differing collision must be a deterministic resolve error")
	require.Contains(t, err.Error(), "tools")
}

const tmplReservedContainer = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: tmpl}
spec:
  podTemplate:
    spec:
      containers: [{name: job, image: evil:latest}]
  steps:
    - {name: s, run: echo hi}
`

const tmplReservedVolume = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: tmpl}
spec:
  podTemplate:
    spec:
      volumes: [{name: workspace, emptyDir: {}}]
  steps:
    - {name: s, run: echo hi}
`

func TestResolveSpec_ReservedNameInjection_Errors(t *testing.T) {
	for name, tmpl := range map[string]string{"container job": tmplReservedContainer, "volume workspace": tmplReservedVolume} {
		t.Run(name, func(t *testing.T) {
			specJSON := mustMarshalSpec(dsl.Spec{
				Steps: []dsl.StepEntry{{Name: "u", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/tmpl.yaml@v1"}}},
			})
			_, err := resolveToSpec(t, &stubFetcher{data: []byte(tmpl)}, specJSON)
			require.Error(t, err)
			require.True(t, gittemplate.IsResolveError(err))
		})
	}
}

// Kind gate: a kind: Job target is rejected with conversion guidance, and an
// unknown field on a JobTemplate surfaces as a deterministic resolve error.
const tmplKindJob = `
apiVersion: unified-cd/v1
kind: Job
metadata: {name: tmpl}
spec:
  steps:
    - {name: s, run: echo hi}
`

const tmplUnknownField = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: tmpl}
spec:
  agentSelector: [gpu]
  steps:
    - {name: s, run: echo hi}
`

func TestResolveSpec_KindGate(t *testing.T) {
	cases := map[string]struct {
		tmpl    string
		errWant string
	}{
		"kind Job rejected":    {tmplKindJob, "kind: JobTemplate"},
		"unknown field errors": {tmplUnknownField, "agentSelector"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			specJSON := mustMarshalSpec(dsl.Spec{
				Steps: []dsl.StepEntry{{Name: "u", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/tmpl.yaml@v1"}}},
			})
			_, err := resolveToSpec(t, &stubFetcher{data: []byte(tc.tmpl)}, specJSON)
			require.Error(t, err)
			require.True(t, gittemplate.IsResolveError(err), "kind/schema violations must be deterministic resolve errors")
			require.Contains(t, err.Error(), tc.errWant)
		})
	}
}

func TestResolveSpec_ScopeMode_PodTemplateRejected(t *testing.T) {
	// A uses step WITH runsIn.image is scope mode: a template podTemplate is an error.
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{{
			Name:   "u",
			Uses:   &dsl.UsesStep{Job: "git://github.com/org/repo/tmpl.yaml@v1"},
			RunsIn: &dsl.RunsIn{Image: "alpine:3"},
		}},
	})
	_, err := resolveToSpec(t, &stubFetcher{data: []byte(tmplWithToolsAndVolume)}, specJSON)
	require.Error(t, err)
	require.True(t, gittemplate.IsResolveError(err))
	require.Contains(t, err.Error(), "podTemplate")
}

func TestResolveSpec_ScopeMode_NoPodTemplate_OK(t *testing.T) {
	// Scope mode with a plain steps-only template still works (no merge, no error).
	const plain = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: tmpl}
spec:
  steps:
    - {name: s, run: echo hi}
`
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{{
			Name:   "u",
			Uses:   &dsl.UsesStep{Job: "git://github.com/org/repo/tmpl.yaml@v1"},
			RunsIn: &dsl.RunsIn{Image: "alpine:3"},
		}},
	})
	s, err := resolveToSpec(t, &stubFetcher{data: []byte(plain)}, specJSON)
	require.NoError(t, err)
	require.Nil(t, s.PodTemplate, "scope mode must not create/merge a podTemplate")
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/gittemplate/ -run 'TestResolveSpec_Merges|TestResolveSpec_CallerContainerKept|TestResolveSpec_CollisionDiffers|TestResolveSpec_ReservedNameInjection|TestResolveSpec_KindGate|TestResolveSpec_ScopeMode' -count=1`
Expected: FAIL (no merge, no kind gate today — a `kind: JobTemplate` document is currently rejected by `Job.Validate`'s kind check, and `kind: Job` is currently accepted).

- [ ] **Step 3: `expandUsesStep` — scope-mode check + contribution collection**

In `internal/gittemplate/inline.go`:

Add the contribution type near the top of the file:

```go
// podContribution is what a non-scope uses: template contributes to the
// caller's podTemplate: the container and volume definitions its steps need.
// (What a template may declare at all is enforced structurally by the
// kind: JobTemplate schema — dsl.ParseJobTemplate — not here.)
type podContribution struct {
	containers []map[string]any
	volumes    []map[string]any
}
```

Change `expandUsesStep`'s signature and prologue (the schema already constrained the template; only two semantic rules live here — scope-mode podTemplate rejection and reserved names):

```go
func expandUsesStep(usesName string, with map[string]string, tplSpec dsl.Spec, outerRunsIn *dsl.RunsIn, outerContainer string) ([]dsl.StepEntry, podContribution, error) {
	if len(tplSpec.Steps) == 0 {
		return nil, podContribution{}, fmt.Errorf("template job has no steps")
	}

	scopeMode := outerRunsIn != nil && outerRunsIn.Image != ""

	// A scope-mode uses (runsIn.image) runs the template in its own scope pod:
	// a template podTemplate cannot be honored there — reject loudly instead of
	// silently dropping it.
	if scopeMode && tplSpec.PodTemplate != nil {
		return nil, podContribution{}, fmt.Errorf("template declares a podTemplate, but this uses: step has runsIn.image (scope mode): the template runs in its own scope pod, so its podTemplate cannot be honored")
	}

	// Non-scope uses: contribute the template's podTemplate containers and the
	// volumes they mount, so the caller's pod gains what the template's steps
	// target. Reserved names are never injectable.
	var contrib podContribution
	if !scopeMode {
		for _, c := range dsl.PodTemplateContainers(tplSpec.PodTemplate) {
			name := dsl.DefName(c)
			if name == "" {
				continue
			}
			if dsl.IsReservedContainerName(name) {
				return nil, podContribution{}, fmt.Errorf("template defines reserved container name %q, which cannot be injected into the caller", name)
			}
			contrib.containers = append(contrib.containers, c)
		}
		for _, v := range dsl.PodTemplateVolumes(tplSpec.PodTemplate) {
			name := dsl.DefName(v)
			if name == "" {
				continue
			}
			if dsl.IsReservedVolumeName(name) {
				return nil, podContribution{}, fmt.Errorf("template defines reserved volume name %q, which cannot be injected into the caller", name)
			}
			contrib.volumes = append(contrib.volumes, v)
		}
	}
```

Then update **every** other `return` in `expandUsesStep`: `return nil, <err>` → `return nil, podContribution{}, <err>`; the final `return result, nil` → `return result, contrib, nil`. (The compiler will flag any missed site.)

Update the exported wrapper:

```go
func ExpandUsesStep(usesName string, with map[string]string, tplSpec dsl.Spec, outerRunsIn *dsl.RunsIn, outerContainer string) ([]dsl.StepEntry, error) {
	steps, _, err := expandUsesStep(usesName, with, tplSpec, outerRunsIn, outerContainer)
	return steps, err
}
```

- [ ] **Step 4: `resolveSteps` accumulates; `ResolveSpec` merges**

In `internal/gittemplate/resolve.go`:

`resolveSteps` signature → `([]dsl.StepEntry, podContribution, error)`; declare `var contrib podContribution` next to `var out []dsl.StepEntry`; update all early returns to `return nil, podContribution{}, <err>`.

**Replace the fetched-YAML parse** (currently `resolve.go:154-160`, a lenient `yaml.Unmarshal` into `dsl.Job` + `job.Validate()`) with the strict JobTemplate parse + conversion:

```go
		tpl, err := dsl.ParseJobTemplate(rawYAML)
		if err != nil {
			return nil, podContribution{}, newResolveError("step %q: fetched template %q: %v", s.Name, rawURI, err)
		}
		tplSpec := tpl.ToSpec()
```

(The `dsl.Job`/`yaml.Unmarshal` block is deleted; the `yaml` import may become unused in `resolve.go` — remove it if so.)

Recursion + expansion block becomes:

```go
		nestedPath := append(append([]string{}, path...), rawURI)
		nestedSteps, nestedContrib, err := r.resolveSteps(ctx, tplSpec.Steps, credFn, depth+1, nestedPath)
		if err != nil {
			return nil, podContribution{}, err
		}
		tplSpec.Steps = nestedSteps

		expanded, expandContrib, err := expandUsesStep(s.Name, s.Uses.WithAsStrings(), tplSpec, s.RunsIn, s.Container)
		if err != nil {
			return nil, podContribution{}, newResolveError("step %q: expand uses: %v", s.Name, err)
		}

		for _, es := range expanded {
			if es.Name == s.Name {
				continue // expected: the output-capture step intentionally reuses the uses step's own name
			}
			if seen[es.Name] {
				return nil, podContribution{}, newResolveError("step %q: expanded step name %q collides with an existing step", s.Name, es.Name)
			}
			seen[es.Name] = true
		}

		out = append(out, expanded...)
		contrib.containers = append(contrib.containers, nestedContrib.containers...)
		contrib.containers = append(contrib.containers, expandContrib.containers...)
		contrib.volumes = append(contrib.volumes, nestedContrib.volumes...)
		contrib.volumes = append(contrib.volumes, expandContrib.volumes...)
```

Final return: `return out, contrib, nil`.

In `ResolveSpec`, capture + merge (Finally resolution is Task 5 — here just adapt to the new signatures):

```go
	resolvedSteps, contrib, err := r.resolveSteps(ctx, spec.Steps, credFn, 0, nil)
	if err != nil {
		return nil, err
	}
	spec.Steps = resolvedSteps

	if err := mergeContribution(&spec, contrib); err != nil {
		return nil, err
	}

	out, err := json.Marshal(spec)
```

Add `mergeContribution` + helpers (add `"bytes"` to imports; `encoding/json` and `fmt` are already imported):

```go
// mergeContribution fills spec.PodTemplate with the containers and volumes
// contributed by uses: templates that the caller lacks. A name already present
// (caller or a previously-merged contribution) is kept once if the definitions
// are JSON-equal, or is a deterministic resolve error if they differ. Reserved
// names were already rejected at contribution time.
func mergeContribution(spec *dsl.Spec, contrib podContribution) error {
	if len(contrib.containers) == 0 && len(contrib.volumes) == 0 {
		return nil
	}
	if spec.PodTemplate == nil {
		spec.PodTemplate = &dsl.PodTemplate{}
	}
	if spec.PodTemplate.Spec == nil {
		spec.PodTemplate.Spec = map[string]any{}
	}
	if err := mergeDefs(spec.PodTemplate, "containers", contrib.containers); err != nil {
		return err
	}
	return mergeDefs(spec.PodTemplate, "volumes", contrib.volumes)
}

// mergeDefs gap-fills named definition maps into pt.Spec[key].
func mergeDefs(pt *dsl.PodTemplate, key string, defs []map[string]any) error {
	if len(defs) == 0 {
		return nil
	}
	rawList, _ := pt.Spec[key].([]any)
	existing := map[string]map[string]any{}
	for _, r := range rawList {
		if d, ok := r.(map[string]any); ok {
			if n := dsl.DefName(d); n != "" {
				existing[n] = d
			}
		}
	}
	for _, d := range defs {
		name := dsl.DefName(d)
		if name == "" {
			continue
		}
		if prev, ok := existing[name]; ok {
			eq, err := jsonEqual(prev, d)
			if err != nil {
				return wrapResolveError(fmt.Errorf("compare %s %q: %w", strings.TrimSuffix(key, "s"), name, err))
			}
			if !eq {
				return newResolveError("%s %q is defined differently by the caller (or another uses template) and a uses template; rename one or align their definitions", strings.TrimSuffix(key, "s"), name)
			}
			continue // identical -> dedup
		}
		existing[name] = d
		rawList = append(rawList, d)
	}
	pt.Spec[key] = rawList
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

(Confirm `strings` is imported in `resolve.go` — it is, for `HasGitURIs`.)

- [ ] **Step 5: Run the merge/kind-gate tests + full package (fixture migration)**

Run: `go test ./internal/gittemplate/ -run 'TestResolveSpec_Merges|TestResolveSpec_CallerContainerKept|TestResolveSpec_CollisionDiffers|TestResolveSpec_ReservedNameInjection|TestResolveSpec_KindGate|TestResolveSpec_ScopeMode' -count=1`
Expected: PASS.

Run: `go test ./internal/gittemplate/ -count=1`
Expected: many EXISTING tests in `resolve_test.go` (and any other test whose fixture is FETCHED as a template — grep `kind: Job` in `internal/gittemplate/*_test.go`) now FAIL at the kind gate. **Migrate those fixtures to `kind: JobTemplate`** — this is the intended breaking change. While converting, a fixture that declares fields absent from the JobTemplate schema will fail strict decode; adjust the fixture to the supported shape (the templates in existing tests use steps/params/shell only, so conversion should be mechanical). Tests asserting the OLD behavior (e.g. `resolve_test.go:24`'s `kind: Job` cache fixture is just cache bytes — unaffected; only actually-resolved templates matter). Do not weaken the kind gate to keep a fixture alive. `fetch_test.go`/`fetchdir_test.go` fixtures are fetched-bytes-only (never parsed as templates) — leave them unless a test pipes them through `ResolveSpec`.

- [ ] **Step 6: Nested-uses bubbling + routing-flip tests**

Append to `merge_test.go` (copy the URI-key convention from `TestResolveSpec_RecursiveUses` in `resolve_test.go`):

```go
func TestResolveSpec_NestedUses_BubblesPodShape(t *testing.T) {
	const inner = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: inner}
spec:
  podTemplate:
    spec:
      containers: [{name: deep, image: alpine:3}]
      volumes: [{name: deepvol, emptyDir: {}}]
  steps:
    - {name: leaf, container: deep, run: echo hi}
`
	const outer = `
apiVersion: unified-cd/v1
kind: JobTemplate
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
	require.Contains(t, defNames(dsl.PodTemplateContainers(s.PodTemplate)), "deep")
	require.Contains(t, defNames(dsl.PodTemplateVolumes(s.PodTemplate)), "deepvol")
}

func TestResolveSpec_MergedK8sOnlyContainer_FlipsRouting(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{{Name: "u", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/tmpl.yaml@v1"}}},
	})
	s, err := resolveToSpec(t, &stubFetcher{data: []byte(tmplWithToolsAndVolume)}, specJSON)
	require.NoError(t, err)
	require.True(t, dsl.PodTemplateNeedsKubernetes(s.PodTemplate),
		"a merged container with volumeMounts (and pod volumes) must make the podTemplate require kubernetes")
}
```

Run: `go test ./internal/gittemplate/ -run 'TestResolveSpec_NestedUses_BubblesPodShape|TestResolveSpec_MergedK8sOnlyContainer' -count=1`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/gittemplate/ internal/dsl/
git commit -m "feat(gittemplate): uses targets must be kind: JobTemplate; merge template pod shape into caller"
```

---

### Task 5: Resolve `uses:` in `finally:`

**Files:**
- Modify: `internal/gittemplate/resolve.go` (`HasGitURIs` scans Finally; `ResolveSpec` resolves Finally; cross-list name-collision check)
- Test: `internal/gittemplate/finally_resolve_test.go` (create)

**Interfaces:**
- Consumes: `resolveSteps`, `mergeContribution` (Task 4).
- Produces: `ResolveSpec` output where `spec.Finally`'s uses steps are expanded and their pod-shape contributions merged; a global duplicate-step-name check across resolved Steps + Finally.

- [ ] **Step 1: Write the failing tests**

Create `internal/gittemplate/finally_resolve_test.go`:

```go
package gittemplate_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/gittemplate"
)

const finallyTmpl = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: tmpl}
spec:
  podTemplate:
    spec:
      containers: [{name: tools, image: alpine:3}]
  steps:
    - {name: cleanup, container: tools, run: echo bye}
`

func TestHasGitURIs_FinallyOnly(t *testing.T) {
	spec := mustMarshalSpec(dsl.Spec{
		Steps:   []dsl.StepEntry{{Name: "main", Run: "echo hi"}},
		Finally: []dsl.StepEntry{{Name: "fin", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/t.yaml@v1"}}},
	})
	require.True(t, gittemplate.HasGitURIs(spec), "a finally-only uses spec must be seen by the resolver")
}

func TestResolveSpec_ResolvesFinallyUses(t *testing.T) {
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps:   []dsl.StepEntry{{Name: "main", Run: "echo hi"}},
		Finally: []dsl.StepEntry{{Name: "fin", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/t.yaml@v1"}}},
	})
	s, err := resolveToSpec(t, &stubFetcher{data: []byte(finallyTmpl)}, specJSON)
	require.NoError(t, err)
	// The finally uses step must be expanded (no Uses left) and prefixed steps present.
	for _, e := range s.Finally {
		require.Nil(t, e.Uses, "finally uses step must be expanded")
	}
	names := map[string]bool{}
	for _, e := range s.Finally {
		names[e.Name] = true
	}
	require.True(t, names["fin__cleanup"], "template step must be inlined into finally with the uses prefix; got %v", names)
	// Its pod-shape contribution merges into the caller's podTemplate.
	require.Contains(t, defNames(dsl.PodTemplateContainers(s.PodTemplate)), "tools")
}

func TestResolveSpec_FinallyNameCollisionWithSteps_Errors(t *testing.T) {
	// The caller already has a main step named `fin__cleanup`; the finally
	// expansion would produce the same name -> deterministic error.
	specJSON := mustMarshalSpec(dsl.Spec{
		Steps: []dsl.StepEntry{
			{Name: "fin__cleanup", Run: "echo clash"},
		},
		Finally: []dsl.StepEntry{{Name: "fin", Uses: &dsl.UsesStep{Job: "git://github.com/org/repo/t.yaml@v1"}}},
	})
	_, err := resolveToSpec(t, &stubFetcher{data: []byte(finallyTmpl)}, specJSON)
	require.Error(t, err)
	require.True(t, gittemplate.IsResolveError(err))
	require.Contains(t, err.Error(), "fin__cleanup")
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/gittemplate/ -run 'TestHasGitURIs_FinallyOnly|TestResolveSpec_ResolvesFinallyUses|TestResolveSpec_FinallyNameCollision' -count=1`
Expected: FAIL — `HasGitURIs` returns false for finally-only; `ResolveSpec` leaves finally unresolved.

- [ ] **Step 3: Implement**

In `internal/gittemplate/resolve.go`:

`HasGitURIs` — scan both lists:

```go
	for _, s := range spec.Steps {
		if s.Uses != nil && strings.HasPrefix(s.Uses.Job, "git://") {
			return true
		}
	}
	for _, s := range spec.Finally {
		if s.Uses != nil && strings.HasPrefix(s.Uses.Job, "git://") {
			return true
		}
	}
	return false
```

`ResolveSpec` — resolve Finally through the same path, merge both contributions, then run a global duplicate-name check:

```go
	resolvedSteps, contrib, err := r.resolveSteps(ctx, spec.Steps, credFn, 0, nil)
	if err != nil {
		return nil, err
	}
	spec.Steps = resolvedSteps

	if len(spec.Finally) > 0 {
		resolvedFinally, fcontrib, ferr := r.resolveSteps(ctx, spec.Finally, credFn, 0, nil)
		if ferr != nil {
			return nil, ferr
		}
		spec.Finally = resolvedFinally
		contrib.containers = append(contrib.containers, fcontrib.containers...)
		contrib.volumes = append(contrib.volumes, fcontrib.volumes...)
	}

	if err := mergeContribution(&spec, contrib); err != nil {
		return nil, err
	}

	// Step names must be unique across the whole resolved spec (parse enforces
	// this for authored names via a shared nameSet; expansion must not
	// reintroduce a duplicate across the Steps/Finally boundary).
	if err := checkGlobalNameCollisions(spec.Steps, spec.Finally); err != nil {
		return nil, err
	}

	out, err := json.Marshal(spec)
```

Add the helper:

```go
// checkGlobalNameCollisions rejects a resolved spec whose expanded step names
// collide across the main DAG and finally lists. Within each list resolveSteps'
// own seen-map already guards; this closes the cross-list hole opened by
// expanding uses in both lists independently.
func checkGlobalNameCollisions(steps, finally []dsl.StepEntry) error {
	seen := map[string]bool{}
	record := func(name string) error {
		if name == "" {
			return nil
		}
		if seen[name] {
			return newResolveError("step name %q appears in both the main steps and finally after uses expansion; rename one", name)
		}
		seen[name] = true
		return nil
	}
	for _, list := range [][]dsl.StepEntry{steps, finally} {
		for _, e := range list {
			if err := record(e.Name); err != nil {
				return err
			}
			for _, p := range e.Parallel {
				if err := record(p.Name); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
```

- [ ] **Step 4: Run the tests + full package**

Run: `go test ./internal/gittemplate/ -run 'TestHasGitURIs|TestResolveSpec' -count=1`
Expected: PASS (including all pre-existing `TestResolveSpec_*` and `TestHasGitURIs*` tests — the no-op/ignore tests must still pass).

Run: `go test ./internal/gittemplate/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gittemplate/resolve.go internal/gittemplate/finally_resolve_test.go
git commit -m "fix(gittemplate): resolve uses: steps in finally; cross-list name-collision check"
```

---

### Task 6: Validate container references in the resolver sweeper

**Files:**
- Modify: `internal/controller/scheduler.go` (`resolveGitPendingRuns`, after successful `ResolveSpec`)
- Test: `internal/controller/scheduler_test.go`

**Interfaces:**
- Consumes: `dsl.ValidateContainerReferences` (Task 2); the merge from Tasks 4-5.

- [ ] **Step 1: Write the failing test**

The existing harness (`internal/controller/scheduler_test.go`) uses a real `store.NewTestPostgres(t)`, `pg.CreateRun(...)` with a uses-spec JSON, `resolveGitPendingRuns(ctx, pg, resolver, nil, bo, deadline)`, then `pg.GetRun` + assert status. It defines fetchers implementing `Fetch(ctx, uri, token, sshKey) ([]byte, error)` (see `badYAMLFetcher` at `scheduler_test.go:162`). Model the new tests on `TestResolveGitPendingRuns_DeterministicErrorFailsRun` (line 66):

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
	tmpl := []byte("apiVersion: unified-cd/v1\nkind: JobTemplate\nmetadata: {name: tmpl}\nspec:\n  steps:\n    - {name: s, container: ghost, run: echo hi}\n")
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

	tmpl := []byte("apiVersion: unified-cd/v1\nkind: JobTemplate\nmetadata: {name: tmpl}\nspec:\n  podTemplate:\n    spec:\n      containers: [{name: tools, image: alpine:3}]\n  steps:\n    - {name: s, container: tools, run: echo hi}\n")
	resolver := gittemplate.NewResolver(contentFetcher{yaml: tmpl}, nil)
	bo := newFailureBackoff(time.Minute, time.Hour, 10_000)
	resolveGitPendingRuns(t.Context(), pg, resolver, nil, bo, time.Hour)

	got, err := pg.GetRun(t.Context(), run.ID)
	require.NoError(t, err)
	assert.NotEqual(t, api.RunFailed, got.Status, "a merged-container uses job must resolve successfully")
}
```

(If `t.Context()` is unavailable, use `context.Background()` — match the surrounding file's style. `store.NewTestPostgres` requires the test Postgres the existing resolver tests already use.)

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/controller/ -run 'TestResolveGitPendingRuns_FailsOnDanglingContainer|TestResolveGitPendingRuns_MergedTemplateContainer' -count=1`
Expected: `FailsOnDanglingContainer` FAILS (no validation today → run not Failed); `MergedTemplateContainerResolvesOK` may already pass (merge landed in Task 4) — that is fine, it locks the behavior.

- [ ] **Step 3: Wire validation into the sweeper**

In `internal/controller/scheduler.go`, in `resolveGitPendingRuns`, immediately after the `ResolveSpec` error block (ends ~line 343) and BEFORE `if err := st.UpdateRunSpec(ctx, r.ID, resolved); ...` (~line 349), insert:

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

- [ ] **Step 4: Run the tests + full package**

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

### Task 7: Docs + final sweep

**Files:**
- Modify: the `uses:` reference doc (find it: `grep -rln "uses:" docs/*.md` — likely `docs/jobs.md` or a dedicated templates page).

- [ ] **Step 1: Locate the doc**

Run: `grep -rln "uses:" docs/`
Pick the page documenting `uses:` composition. Read it to match heading/style.

- [ ] **Step 2: Document the behavior**

Add/update a subsection covering:
- **`uses:` targets must be `kind: JobTemplate`** — show the full schema (`description`, `params`, `shell`, `podTemplate.spec.containers`/`volumes`, `steps`), a migration note for existing `kind: Job` templates (change the `kind`, drop unsupported fields), and when to use `call:` instead (a job needing its own pod/agent/run semantics). Any field outside the schema is a run-creation error naming the field.
- A non-scope JobTemplate's `podTemplate.spec` **containers and volumes** are merged into the caller (gap-fill; caller's same-name definition wins if identical, differing is a run-creation error; container names `job`/`unified-artifact` and volume names `workspace`/`ucd-tools` cannot be injected). In scope mode (`runsIn.image`) a JobTemplate podTemplate is an error.
- An undefined `container:` in a resolved `uses:` job now fails at run creation with a clear message (previously an opaque runtime failure).
- `uses:` now works inside `finally:` (previously silently unresolved).

- [ ] **Step 3: Final sweep**

Run: `gofmt -l internal/dsl/ internal/gittemplate/ internal/controller/ internal/k8sagent/` — NOTE: pre-existing repo-wide CRLF condition lists many untouched files; only act on files you created/edited if newly unformatted for a real reason.
Run: `go build ./...`
Run: `go vet ./internal/dsl/ ./internal/gittemplate/ ./internal/controller/ ./internal/k8sagent/`
Run: `go test ./internal/dsl/ ./internal/gittemplate/ ./internal/controller/ -count=1`
All must pass (no `-race`; CGO disabled).

- [ ] **Step 4: Commit**

```bash
git add docs/
git commit -m "docs: kind: JobTemplate for uses:, pod-shape merge, finally resolution"
```

---

## Notes for the executor

- **Task ordering matters:** 1→2 (dsl helpers) and 3 (JobTemplate) feed 4 (merge+kind gate); 5 (finally) builds on 4's signatures; 6 (validation) needs 2+4. Execute in order.
- **Reuse existing test fixtures.** `stubFetcher`, `mapFetcher`, `mustMarshalSpec`, and the no-op `CredentialFunc` live in `internal/gittemplate/resolve_test.go` (package `gittemplate_test`). Grep for exact names/signatures first; copy the URI-key convention from `TestResolveSpec_RecursiveUses`. `resolveToSpec`/`defNames` are defined once in `merge_test.go` and shared by `finally_resolve_test.go` (same package).
- **Fixture migration is expected in Task 4:** every existing test whose template YAML is actually resolved (`resolve_test.go` fixtures fetched via stub/map fetchers) must become `kind: JobTemplate`. Do not weaken the kind gate to avoid the migration.
- **`expandUsesStep` has many `return` sites** — every one must gain the `podContribution{}` slot. Compile early; the compiler flags missed sites.
- **Do not touch `ExpandUsesStep`'s exported signature** — the wrapper adapts.
- **In Task 3, reuse `dsl`'s existing unexported validators** (`validateStepEntries`, `validShellArgv`, `checkForbiddenJobFields`, `ValidateName`) — `jobtemplate.go` is in the same package. Verify their exact signatures before writing `Validate`.
- **The finally-expansion prefix (`fin__cleanup`)** assumes `prefixedName` produces `<usesName>__<innerName>` — verify against `inline.go:70-72` before asserting.
