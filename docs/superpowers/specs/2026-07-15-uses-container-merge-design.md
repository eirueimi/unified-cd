# `uses:` Container Auto-Merge + Container-Reference Validation — Design

**Status:** Approved (design decisions locked 2026-07-15).

**Goal:** Make a non-scope `uses:` template that targets its own `container:` "just work" by carrying the template's `podTemplate` containers into the caller's pod, and turn today's opaque runtime exec failure for an undefined `container:` into a clear error at run-creation.

## Problem (verified in code)

A `uses:` template is a full `dsl.Job` and may declare its own `podTemplate` with containers. But the expansion (`internal/gittemplate/inline.go` `expandUsesStep`, driven by `internal/gittemplate/resolve.go` `resolveSteps`/`ResolveSpec`) inlines **only** the template's `Steps`/`Params`/`Shell` — it **silently discards `tplSpec.PodTemplate`**. So a template whose steps say `container: foo` and whose own `podTemplate` defines `foo` produces a caller spec that references `foo` with no `foo` container. There is **no apply-time or resolution-time validation** of container references, so it fails only at execution: the host agent returns `container %q is not defined in the job's podTemplate` (`internal/agent/claim_pod.go:372`); the k8s agent gets an opaque API-server error (`internal/k8sagent/executor.go`). This same silent gap also affects a plain typo in a direct (non-`uses:`) `container:` reference.

## Scope

- **Non-scope `uses:` only** (the outer uses step has no `runsIn.image`). In scope mode the template runs in a fresh isolated scope pod and `container:` is already rejected by the DSL (`inline.go` `checkScopeStepAllowed`), so there is no caller-pod container to merge — the feature does not apply and must not change scope-mode behavior.
- Applies to the real resolution path used for run creation. The exported `ExpandUsesStep` (used by the controller only for shell-composition verification / planned-step display) keeps returning steps only — it does not need the merged containers.

## Design

### 1. Carry the template's containers into the caller (auto-merge)

During `uses:` resolution, in addition to inlining steps, collect the container definitions from `tplSpec.PodTemplate.Spec["containers"]` and merge the ones the caller lacks into the caller's `spec.PodTemplate.Spec["containers"]`.

- **Threading:** `expandUsesStep` (internal) gains a second return value — the list of container definitions (`[]map[string]any`) the template contributes. `resolveSteps` accumulates these across its recursion (a `uses:` template may itself use `uses:`; nested contributions bubble up). `ResolveSpec` performs the actual merge into `spec.PodTemplate` after `resolveSteps` returns and before re-marshalling. The exported `ExpandUsesStep` wrapper discards the container return (its caller does not consume it).
- **Merge rule — caller keeps its own, template fills gaps:** for each template container by `name`, if the caller's `podTemplate` already defines a container with that name, do not merge the template's (subject to the collision rule below). Otherwise append the template's container to the caller's list.
- **If the caller has no `podTemplate` at all**, create one (`&dsl.PodTemplate{Spec: map[string]any{"containers": []any{...}}}`) holding the merged containers.

### 2. Reserved names — never injectable

A `uses:` template may **not** contribute a container whose name is reserved: `"job"` (the injected primary) or the artifact sidecar `"unified-artifact"` (`internal/k8sagent/podbuilder.go:14`). If a non-scope `uses:` template's `podTemplate` defines a container with a reserved name, resolution fails with a clear error (the template cannot override the caller's primary or the internal sidecar). To keep the reserved set in one place, add `dsl.IsReservedContainerName(name) bool` (returns true for `"job"` and `"unified-artifact"`) in package `dsl`; the k8s agent's existing `artifactSidecarName` literal should reference/stay in sync with this constant (documented, since `dsl` cannot import `k8sagent`).

### 3. Name collision — identical dedups, differing errors

When both the caller and a `uses:` template define a container with the same non-reserved name:
- If the two container definitions are **deep-equal** (`reflect.DeepEqual` on the `map[string]any`), treat as no conflict — keep the caller's, drop the template's duplicate.
- If they **differ**, resolution fails with an error naming the container, the caller, and the `uses:` template — the author resolves it (rename, or align the definitions).

### 4. Container-reference validation (closes the silent gap)

After resolution + merge, validate every step's `container:` against the **effective** podTemplate. A reference is valid iff it is empty (defaults to `job`), a reserved name (`job`/`unified-artifact`), or the name of a container present in the effective `podTemplate.Spec["containers"]`. An invalid reference → a clear run-creation error: which step, which container name, and — when the step came from a `uses:` expansion — that neither the caller nor the template defines it.

- **Placement:** this validation runs in the controller's git-resolution sweeper (`internal/controller/scheduler.go` `resolveGitPendingRuns`), **immediately after a successful `ResolveSpec`**, on the fully-resolved + merged spec — before the resolved spec is persisted via `UpdateRunSpec`. A failure fails the run deterministically (the same terminal-failure path the resolver already uses for `IsResolveError`), so the author sees a clear reason instead of a later runtime exec failure. Because the resolved spec at that point contains both the caller's own steps and the expanded template steps, this one pass validates **both** the caller's own direct `container:` references and the template steps' references against the merged podTemplate.
- **Coverage boundary (accurate):** the sweeper processes only runs whose spec contains git URIs (`HasGitURIs`), i.e. `uses:` jobs. A **pure non-`uses:` job** (no git URIs) skips the sweeper, so its direct-`container:`-typo case is *not* covered by this pass and retains today's runtime failure. Closing that is deliberately **out of scope** here: the natural home would be `Job.Validate()`, but that same function also validates *fetched templates* (`resolve.go:158`), and a template may legitimately reference a container it expects the caller to provide — so a blanket parse-time reference check there would wrongly reject valid templates. A dedicated apply-time check for top-level non-`uses:` jobs is a clean future addition but not part of this feature.

### 5. Host-vs-k8s routing

The merge happens at resolution, before the controller picks an agent, so the routing predicate `dsl.PodTemplateNeedsKubernetes` (`internal/dsl/podtemplate.go:27`) automatically re-evaluates on the merged podTemplate. A caller that merges a k8s-only container (e.g. one carrying `volumeMounts` or `securityContext`) therefore correctly becomes k8s-routed with no special handling. A test asserts this.

## Components / files

- `internal/gittemplate/inline.go` — `expandUsesStep` returns contributed containers; reserved-name rejection at collection time.
- `internal/gittemplate/resolve.go` — `resolveSteps` accumulates contributed containers across recursion; `ResolveSpec` merges into `spec.PodTemplate` (gap-fill + collision policy) before marshalling.
- `internal/dsl/` — `IsReservedContainerName` + container accessors; a container-reference validation helper (`ValidateContainerReferences(spec dsl.Spec) error`) operating on a resolved `dsl.Spec` (returns a descriptive error). Reusable, pure, unit-testable.
- `internal/controller/scheduler.go` — in `resolveGitPendingRuns`, after a successful `ResolveSpec` and before `UpdateRunSpec`, unmarshal the resolved spec and call `dsl.ValidateContainerReferences`; on error, fail the run deterministically (mirror the existing `IsResolveError` terminal-fail branch).

## Error handling

All new failure modes are **run-creation errors** (surfaced to the trigger), never silent and never deferred to exec time: reserved-name injection, differing-definition collision, and dangling container reference. Each error names the offending container, the step (for references), and the `uses:` template (for merge/collision), so the author can act without reading agent logs.

## Testing

Unit tests (package `gittemplate`, plus `dsl` for the helpers):
- **Gap-fill:** caller lacks `foo`, template defines `foo` → merged; a step's `container: foo` resolves; validation passes.
- **Caller wins:** caller defines `foo`, template also defines an identical `foo` → deduped, caller's kept, no error.
- **Collision differs:** caller and template define `foo` differently → run-creation error naming both.
- **Reserved-name injection:** template defines `job` (or `unified-artifact`) → error.
- **Dangling reference:** `container: bar` defined nowhere (no caller def, no template contribution) → clear error naming step + `bar`. Also the direct (`uses:`-less) typo variant.
- **Scope mode unaffected:** a `uses:` step with `runsIn.image` → no container merge, existing behavior preserved (container: already rejected there).
- **Nested uses:** a template that itself uses another template contributes containers that bubble up to the outermost caller.
- **Routing flip:** merging a container with a k8s-only field makes `PodTemplateNeedsKubernetes` return true for the merged podTemplate.
- **No-op:** a spec with no `uses:` and valid direct references resolves + validates unchanged.

## Docs

Document in the `uses:` reference docs: a non-scope `uses:` template's `podTemplate` containers are merged into the caller (gap-fill; caller's same-name definition wins if identical, differing is an error; `job`/`unified-artifact` can't be injected), and that an undefined `container:` is now a run-creation error rather than a runtime failure.

## Out of scope

- Scope-mode (`runsIn.image`) behavior — unchanged.
- Volumes / other podTemplate spec keys — only `containers` are merged (a template needing shared volumes is a larger design; YAGNI here).
- Apply-time validation of direct references (future UX enhancement).
- `override:` (`PodSpecPatch`) merging — separate mechanism, untouched.
