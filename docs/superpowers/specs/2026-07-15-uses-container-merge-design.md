# `uses:` JobTemplate Resource, Pod-Shape Merge (Containers + Volumes), Finally Resolution, and Container-Reference Validation — Design

**Status:** Approved (design decisions locked 2026-07-15; scope expanded 2026-07-16 to volumes + finally-resolution bug after a full `uses:` drop audit; 2026-07-16 the dynamic unsupported-field guard was replaced by a dedicated **`kind: JobTemplate`** resource — schema-level enforcement instead of enumeration).

**Goal:** Give `uses:` an explicit contract: a `uses:` target must be a **`kind: JobTemplate`** — a dedicated resource whose schema contains *only* what `uses:` can honor (steps, params, shell, description, and a pod-shape subset of podTemplate). Merge the template's `podTemplate` **containers and the volumes they mount** into the caller, fix the never-resolved `uses:`-in-`finally:` bug, and validate `container:` references at run creation.

## Problem (verified in code)

A `uses:` template is a full `dsl.Job`, but the expansion (`internal/gittemplate/inline.go` `expandUsesStep`, driven by `internal/gittemplate/resolve.go` `resolveSteps`/`ResolveSpec`) inlines **only** the template's `Steps`/`Params`/`Shell` — everything else is **silently discarded**, with no validation anywhere:

1. **Containers dropped:** a template whose steps say `container: foo` and whose own `podTemplate` defines `foo` produces a caller spec referencing `foo` with no `foo` container. Fails only at execution: host agent `container %q is not defined in the job's podTemplate` (`internal/agent/claim_pod.go:372`); k8s agent an opaque API-server error (`internal/k8sagent/executor.go`). A plain typo in a direct (non-`uses:`) `container:` fails the same way.
2. **Volumes dropped:** even with containers merged, a template container whose `volumeMounts` reference a volume defined in the template's `podTemplate.spec.volumes` breaks at pod build — the volume never reaches the caller.
3. **`uses:` in `finally:` is never resolved:** `HasGitURIs` scans only `spec.Steps` (`resolve.go:59`) and `ResolveSpec` resolves only `spec.Steps` (`resolve.go:88`), yet parse **allows** `uses:` in `finally:` (`parse.go:169`). A `uses:` step in `finally:` reaches the agent raw and fails at execution.
4. **Every other template field is silently ignored:** `podTemplate.workspace`/`reuse`/`cleanWorkspace`/`override`/`name`, spec-level `agentSelector`/`concurrency`/`timeoutMinutes`/`native`, and the template's own `finally:` steps. An author declares them; nothing happens; no error. (What DOES work: steps, params/`with:`, outputs, shell, and secrets — `secretsNeeded` is derived by scanning resolved steps, so template secret refs propagate automatically.) The root cause: `uses:` points at a full `kind: Job`, whose schema promises far more than `uses:` can honor. The fix is a dedicated resource whose schema *is* the contract.

## Scope

- **Pod-shape merge: non-scope `uses:` only** (the outer uses step has no `runsIn.image`). In scope mode the template runs in a fresh isolated scope pod and `container:` is already rejected by the DSL (`inline.go` `checkScopeStepAllowed`), so there is no caller-pod container/volume to merge; a scope-mode JobTemplate declaring a `podTemplate` is a resolution error (§6). The **`kind: JobTemplate` requirement (§6) applies in both modes**. Existing step/scope execution semantics are otherwise unchanged.
- Applies to the real resolution path used for run creation. The exported `ExpandUsesStep` (used by the controller only for shell-composition verification / planned-step display) keeps returning steps only — it does not need the merged containers.

## Design

### 1. Carry the template's containers AND volumes into the caller (pod-shape auto-merge)

During `uses:` resolution, in addition to inlining steps, collect the container definitions from `tplSpec.PodTemplate.Spec["containers"]` **and the volume definitions from `tplSpec.PodTemplate.Spec["volumes"]`** and merge the ones the caller lacks into the caller's `spec.PodTemplate.Spec` (`"containers"` / `"volumes"` respectively). Volumes travel with the containers that mount them — merging one without the other would fail at pod build.

- **Threading:** `expandUsesStep` (internal) gains extra return values — the container definitions and volume definitions (`[]map[string]any` each) the template contributes. `resolveSteps` accumulates these across its recursion (a `uses:` template may itself use `uses:`; nested contributions bubble up). `ResolveSpec` performs the actual merge into `spec.PodTemplate` after `resolveSteps` returns and before re-marshalling. The exported `ExpandUsesStep` wrapper discards the contribution returns (its caller does not consume them).
- **Merge rule — caller keeps its own, template fills gaps:** for each template container/volume by `name`, if the caller's `podTemplate` already defines one with that name, do not merge the template's (subject to the collision rule below). Otherwise append the template's definition to the caller's list.
- **If the caller has no `podTemplate` at all**, create one (`&dsl.PodTemplate{Spec: map[string]any{...}}`) holding the merged definitions.

### 2. Reserved names — never injectable

A `uses:` template may **not** contribute:
- a **container** whose name is reserved: `"job"` (the injected primary) or the artifact sidecar `"unified-artifact"` (`internal/k8sagent/podbuilder.go:14`);
- a **volume** whose name is reserved: `"workspace"` (the injected workspace volume, `podbuilder.go` `buildWorkspaceVolume`) or `"ucd-tools"` (the shim-carrier emptyDir, `podbuilder.go:32`).

If a non-scope `uses:` template's `podTemplate` defines either, resolution fails with a clear error (the template cannot override the caller's primary, the internal sidecar, or the system volumes). Keep the reserved sets in one place: add `dsl.IsReservedContainerName(name) bool` (`"job"`, `"unified-artifact"`) and `dsl.IsReservedVolumeName(name) bool` (`"workspace"`, `"ucd-tools"`) in package `dsl`; the k8s agent's existing literals (`artifactSidecarName`, `ucdToolsVolume`, the `"workspace"` volume) should reference/stay in sync with these constants (documented, since `dsl` cannot import `k8sagent`).

### 3. Name collision — identical dedups, differing errors

When both the caller and a `uses:` template define a container (or volume) with the same non-reserved name:
- If the two definitions are **JSON-equal**, treat as no conflict — keep the caller's, drop the template's duplicate.
- If they **differ**, resolution fails with an error naming the container/volume, the caller, and the `uses:` template — the author resolves it (rename, or align the definitions).

### 4. Container-reference validation (closes the silent gap)

After resolution + merge, validate every step's `container:` against the **effective** podTemplate. A reference is valid iff it is empty (defaults to `job`), a reserved name (`job`/`unified-artifact`), or the name of a container present in the effective `podTemplate.Spec["containers"]`. An invalid reference → a clear run-creation error: which step, which container name, and — when the step came from a `uses:` expansion — that neither the caller nor the template defines it.

- **Placement:** this validation runs in the controller's git-resolution sweeper (`internal/controller/scheduler.go` `resolveGitPendingRuns`), **immediately after a successful `ResolveSpec`**, on the fully-resolved + merged spec — before the resolved spec is persisted via `UpdateRunSpec`. A failure fails the run deterministically (the same terminal-failure path the resolver already uses for `IsResolveError`), so the author sees a clear reason instead of a later runtime exec failure. Because the resolved spec at that point contains both the caller's own steps and the expanded template steps, this one pass validates **both** the caller's own direct `container:` references and the template steps' references against the merged podTemplate.
- **Coverage boundary (accurate):** the sweeper processes only runs whose spec contains git URIs (`HasGitURIs`), i.e. `uses:` jobs. A **pure non-`uses:` job** (no git URIs) skips the sweeper, so its direct-`container:`-typo case is *not* covered by this pass and retains today's runtime failure. Closing that is deliberately **out of scope** here: the natural home would be `Job.Validate()`, but that same function also validates *fetched templates* (`resolve.go:158`), and a template may legitimately reference a container it expects the caller to provide — so a blanket parse-time reference check there would wrongly reject valid templates. A dedicated apply-time check for top-level non-`uses:` jobs is a clean future addition but not part of this feature.

### 5. Host-vs-k8s routing

The merge happens at resolution, before the controller picks an agent, so the routing predicate `dsl.PodTemplateNeedsKubernetes` (`internal/dsl/podtemplate.go:27`) automatically re-evaluates on the merged podTemplate. A caller that merges a k8s-only container (e.g. one carrying `volumeMounts` or `securityContext`) therefore correctly becomes k8s-routed with no special handling. A test asserts this. (Note: merging pod-level `volumes` adds a non-`containers` key to `pt.Spec`, which also makes `PodTemplateNeedsKubernetes` true — correct, since the host claim-pod builder cannot express pod volumes.)

### 6. `kind: JobTemplate` — the schema IS the contract

A `uses:` target must be a **`kind: JobTemplate`** resource. Its schema contains only what `uses:` can honor; everything else is a **strict-decode unknown-field error** (the codebase's standard `yaml.Decoder.KnownFields(true)` pattern, as in `dsl.Parse` at `parse.go:78`), so unsupported fields are structurally impossible rather than enumerated by a guard.

```yaml
apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: unity-build}
spec:
  description: "..."   # optional documentation
  params: {...}         # inputs/outputs, same as Job
  shell: [...]          # optional default interpreter for the template's steps
  podTemplate:          # pod-shape subset ONLY
    spec:
      containers: [...]
      volumes: [...]
  steps: [...]          # same StepEntry schema as Job (nested uses: allowed)
```

**New `dsl` types:** `JobTemplate{APIVersion, Kind, Metadata, Spec JobTemplateSpec}`; `JobTemplateSpec{Params, Steps, Shell, Description, PodTemplate *JobTemplatePodTemplate}`; `JobTemplatePodTemplate{Spec JobTemplatePodSpec}`; `JobTemplatePodSpec{Containers, Volumes []map[string]any}`. Because `agentSelector`, `concurrency`, `timeoutMinutes`, `native`, `finally`, `podTemplate.reuse`/`workspace`/`override`/`cleanWorkspace`/`name`, and non-`containers`/`volumes` pod-spec keys simply do not exist on these types, strict decoding rejects them with a precise field error — no guard enumeration to maintain, no drift as `Job` grows.

**Parsing/validation:** `dsl.ParseJobTemplate(data []byte) (*JobTemplate, error)` — strict decode + `Validate()` (apiVersion, `Kind == "JobTemplate"`, metadata.name, ≥1 step, step-level validation reusing the same helpers `Job.Validate` uses with `native=false`, plus the existing forbidden-field pre-checks such as `needs:`). `JobTemplate.ToSpec() dsl.Spec` converts to the `dsl.Spec` shape `expandUsesStep` already consumes (podTemplate subset → `PodTemplate{Spec: map[string]any{"containers": ..., "volumes": ...}}`).

**Resolver kind gate:** `resolveSteps` parses the fetched YAML with `ParseJobTemplate` instead of the current lenient `yaml.Unmarshal` into `dsl.Job` + `Job.Validate` (`resolve.go:154-160`). If the fetched document's `kind` is `Job`, the error explicitly says: `uses: targets must be kind: JobTemplate (got kind: Job); convert the template, or invoke the job with call:`. (A cheap two-field pre-sniff of `apiVersion`/`kind` produces this friendly message before the strict decode.)

**Remaining semantic checks (schema can't express these):** reserved container/volume names (§2) and the scope-mode rule — a `uses:` step with `runsIn.image` (scope mode) whose JobTemplate declares a `podTemplate` is a resolution error (the template runs in its own scope pod built from `runsIn.image`; its pod shape cannot be honored). These stay in `expandUsesStep`.

**Migration:** this is a breaking change for existing `uses:` targets, which are `kind: Job` files in git repos. Accepted deliberately: pod-shape `uses:` was half-broken until this branch, so real template assets are minimal, and the error message states the exact conversion. Dual-use ambiguity (one YAML both applied as a job and `uses:`-ed) disappears structurally — a template is a distinct artifact.

### 7. Resolve `uses:` in `finally:` (bug fix)

`HasGitURIs` scans only `spec.Steps` and `ResolveSpec` resolves only `spec.Steps`, while parse permits `uses:` in `finally:` — so a `finally:` `uses:` step is handed to the agent unresolved and fails opaquely at execution. Fix both layers:

- `HasGitURIs` also scans `spec.Finally` for unresolved `uses:` git URIs.
- `ResolveSpec` also resolves `spec.Finally` through the same `resolveSteps` path (own recursion, same depth/cycle rules); its contributed containers/volumes merge into the same caller podTemplate; expanded finally steps are validated by the same container-reference pass.
- Step-name collision checking between `Steps` and `Finally` expansions: the existing per-list `seen` map guards within a list; expanded finally step names must also not collide with main-DAG names (build the seen set across both lists).

## Components / files

- `internal/gittemplate/inline.go` — `expandUsesStep` returns contributed containers; reserved-name rejection at collection time.
- `internal/gittemplate/resolve.go` — `resolveSteps` accumulates contributed containers across recursion; `ResolveSpec` merges into `spec.PodTemplate` (gap-fill + collision policy) before marshalling.
- `internal/dsl/` — `IsReservedContainerName` / `IsReservedVolumeName` + container/volume accessors; a container-reference validation helper (`ValidateContainerReferences(spec dsl.Spec) error`) operating on a resolved `dsl.Spec` (returns a descriptive error). Reusable, pure, unit-testable.
- `internal/dsl/jobtemplate.go` (new) — `JobTemplate`/`JobTemplateSpec`/`JobTemplatePodTemplate`/`JobTemplatePodSpec` types, `ParseJobTemplate` (strict decode + validate), `JobTemplate.ToSpec()`.
- `internal/gittemplate/inline.go` (additional) — scope-mode podTemplate rejection (semantic check; the schema handles everything else).
- `internal/gittemplate/resolve.go` (additional) — fetched YAML parsed via `dsl.ParseJobTemplate` with a kind-aware error for `kind: Job`; `HasGitURIs` scans `Finally`; `ResolveSpec` resolves `Finally` via `resolveSteps` with a cross-list step-name collision check; volume contributions merged alongside containers.
- `internal/controller/scheduler.go` — in `resolveGitPendingRuns`, after a successful `ResolveSpec` and before `UpdateRunSpec`, unmarshal the resolved spec and call `dsl.ValidateContainerReferences`; on error, fail the run deterministically (mirror the existing `IsResolveError` terminal-fail branch).

## Error handling

All new failure modes are **run-creation errors** (surfaced to the trigger), never silent and never deferred to exec time: wrong `kind` (with an explicit convert-or-use-`call:` message), strict-decode unknown fields (with the exact field), reserved-name injection (container or volume), differing-definition collision, scope-mode podTemplate, and dangling container reference. Each error names the offending item, the step (for references), and the `uses:` template (for merge/collision), so the author can act without reading agent logs.

## Testing

Unit tests (package `gittemplate`, plus `dsl` for the helpers):
- **Gap-fill (containers):** caller lacks `foo`, template defines `foo` → merged; a step's `container: foo` resolves; validation passes.
- **Gap-fill (volumes):** template container mounts a template-defined volume → both merged; caller-defined volume with the same name wins (identical dedup / differing error, same as containers).
- **Caller wins:** caller defines `foo`, template also defines an identical `foo` → deduped, caller's kept, no error.
- **Collision differs:** caller and template define `foo` (container or volume) differently → run-creation error naming both.
- **Reserved-name injection:** template defines container `job`/`unified-artifact` or volume `workspace`/`ucd-tools` → error.
- **Kind gate:** a `uses:` target with `kind: Job` → error telling the author to convert to `kind: JobTemplate` or use `call:`.
- **Strict schema:** a JobTemplate declaring `agentSelector` / `finally:` / `podTemplate.reuse` (unknown fields on the JobTemplate types) → strict-decode error naming the field.
- **ParseJobTemplate/ToSpec unit tests (dsl):** valid template parses; kind/apiVersion/name/empty-steps rejected; `ToSpec` produces the expected `dsl.Spec` (podTemplate subset → `Spec["containers"/"volumes"]`).
- **Dangling reference:** `container: bar` defined nowhere (no caller def, no template contribution) → clear error naming step + `bar`. Also the direct (`uses:`-less) typo variant.
- **Finally resolution:** a caller with a `uses:` step in `finally:` → resolved/expanded (steps inlined, containers merged); `HasGitURIs` returns true for a finally-only uses spec; expanded finally names must not collide with main-DAG names.
- **Scope mode:** a `uses:` step with `runsIn.image` + a JobTemplate declaring a podTemplate → error; a plain steps-only JobTemplate in scope mode still works (no merge).
- **Nested uses:** a template that itself uses another template contributes containers/volumes that bubble up to the outermost caller.
- **Routing flip:** merging a container with a k8s-only field makes `PodTemplateNeedsKubernetes` return true for the merged podTemplate.
- **No-op:** a spec with no `uses:` and valid direct references resolves + validates unchanged.

## Docs

Document in the `uses:` reference docs:
- **`uses:` targets must be `kind: JobTemplate`** — full schema reference (steps, params, shell, description, podTemplate.spec.containers/volumes), a migration note for existing `kind: Job` templates, and when to use `call:` instead (a job needing its own pod/agent/run semantics).
- A non-scope JobTemplate's `podTemplate` **containers and volumes** are merged into the caller (gap-fill; caller's same-name definition wins if identical, differing is an error; container names `job`/`unified-artifact` and volume names `workspace`/`ucd-tools` can't be injected).
- An undefined `container:` is now a run-creation error rather than a runtime failure.
- `uses:` now works in `finally:` (previously silently unresolved).

## Out of scope

- Scope-mode (`runsIn.image`) merge behavior — still contributes nothing (podTemplate rejected there).
- A JobTemplate-level `finally:` (not in the schema; possible future design for template cleanup).
- Apply-time validation of direct references in non-`uses:` jobs (future UX enhancement).
- Registering JobTemplates as controller resources (apply/store/WebUI) — a JobTemplate lives in git and is fetched by `uses:`; no controller CRUD in this feature.
- `override:` (`PodSpecPatch`) merging — not in the JobTemplate schema.
