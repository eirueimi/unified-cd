# `runsIn` uses-scope: artifacts & cache in isolated environments

- Date: 2026-07-05
- Status: Design approved (implementation plan pending)
- Related: [2026-07-05-runsin-design.md](2026-07-05-runsin-design.md) (base `runsIn` abstraction)

## Background & motivation

Today `runsIn.image` isolates a **single** `run` step: the host agent runs
`<runtime> run --rm`, the k8s agent spins up a throwaway pod. The isolated
environment deliberately does not share the job workspace (the "pure function"
isolation contract from the base `runsIn` design).

`uses:` templates are inlined at parse time (`internal/gittemplate/inline.go`
`expandUsesStep`), so a `runsIn` declared on a `uses` step propagates to each
inlined step **independently**. Each isolated `run` step therefore becomes its
own island with a fresh throwaway environment. Meanwhile artifact and cache
steps are dispatched by step kind and always operate on the shared job
workspace (`workDir` on host, the `/workspace` volume on k8s), ignoring
`runsIn` entirely.

Consequence: an artifact/cache step inside a `runsIn`-wrapped template cannot
capture anything produced in the isolated environment. Files written by an
isolated `run` step never reach the workspace (they die with `--rm` / pod GC),
so a following `upload-artifact` reads an empty/absent path.

**Goal:** let a `uses` template that runs in an isolated environment save and
restore artifacts and cache from **that** environment.

## Two-tier semantics

`runsIn` keeps its per-step meaning, but gains a distinct meaning at the `uses`
level:

| `runsIn` location | Behavior |
|---|---|
| step-level `image:` (on a plain `run` step) | **Unchanged.** Single throwaway isolated call ("pure function"). No artifact/cache. |
| uses-level `image:` | **Scope mode (this feature).** The whole inlined template runs in one isolated environment ("scope") that lives for the template's steps. |
| uses-level `container:` | **Unchanged.** Exec into a named pre-provisioned container; not scope mode. |
| `uses` without `runsIn` | **Unchanged.** Current flatten / per-step behavior; this feature is inert. |

Scope mode is triggered **only** by a uses-level `runsIn.image`.

## Isolation contract (intent preserved)

The scope does **not** share the outer job workspace. It gets a fresh scratch
filesystem.

- **Inputs** enter via `with:` (env vars, as today) and `download-artifact`
  (which writes into the scope filesystem).
- **Outputs** leave via `upload-artifact` (pushed to the run-scoped object
  store) and `outputs:` / stdout.

Because artifacts are keyed by `runID` in the object store, they cross the
isolation boundary naturally: no outer-workspace sharing is needed, and on k8s
there is **no ReadWriteMany PVC requirement** (the scope pod owns a private
scratch volume and pushes artifacts to object storage, not to the outer
workspace PVC).

## Data model

Each inlined step gains two fields:

- `ScopeID` — groups steps that came from the same scoped `uses` invocation.
- `ScopeImage` — the image the scope environment runs.

Steps sharing a `ScopeID` run in one environment. Under `matrix` / `foreach`,
each variant of the `uses` step gets a **distinct** `ScopeID` (independent
scope instance / independent environment).

These fields flow: `dsl.StepEntry` → `api.ClaimStep` (controller passes them
through unchanged) → agent.

## Components & changes

1. **Parse / inline (`internal/gittemplate/inline.go`)**
   - In `expandUsesStep`, when `outerRunsIn.Image != ""`, assign a common
     `ScopeID` + `ScopeImage` to every expanded step instead of propagating
     `runsIn` onto each step individually.
   - `ScopeID` is derived deterministically from the outer `uses` step name (and
     matrix variant key when present) so it is stable across runs and unique per
     variant.
   - **Validation:** if any inner step declares its own `runsIn.image` or
     `runsIn.container` while the `uses` is in scope mode → **parse error**
     naming the offending step (scope must be homogeneous).

2. **API types (`internal/api/types.go`)**
   - `ClaimStep` gains `ScopeID string` and `ScopeImage string`. Controller
     serialization passes them through; no scheduling changes.

3. **host agent (`internal/agent`)**
   - A **scope manager** in the step loop: on the first step carrying a not-yet-
     open `ScopeID`, provision the scope container; route that scope's
     run/artifact/cache steps into it; tear it down at the scope boundary.

4. **Container runtime driver (`internal/runtime`)**
   - Extend `ContainerRuntime` with a long-lived lifecycle:
     `Create`/`Start` (detached, e.g. `run -d <image> sleep infinity`), `Exec`
     (run a script inside the running container), `CopyIn` (`docker cp` host
     path → container path, for `download-artifact` / cache restore), `CopyOut`
     (container path → host path, for `upload-artifact` / cache save), and
     `Remove`. This follows the CRI-shaped lifecycle the base `runsIn` design
     anticipated ("add exec/stop/remove later").

5. **k8s agent (`internal/k8sagent`)**
   - A **scope pod manager**: build a dedicated scope pod = `ScopeImage`
     container + a private scratch `emptyDir` volume + the artifact sidecar
     (reusing `BuildPod` / sidecar injection). Exec all scope steps into it.
     artifact/cache go through the scope pod's sidecar against the scratch
     volume. Tear down on scope end; `podgc` is the backstop.

6. **artifact / cache execution**
   - Currently target `workDir` (host) / `mountPath` (k8s). In scope mode they
     target the **scope environment's** filesystem instead:
     - host: outputs (`upload-artifact`, cache save) `CopyOut` from the scope
       container to a host temp dir, then reuse the existing upload / cache-save
       path; inputs (`download-artifact`, cache restore) run the existing
       fetch to a host temp dir, then `CopyIn` to the scope container.
     - k8s: the scope pod's sidecar reads/writes the scratch volume directly in
       both directions (same argv shape as the pooled-pod path, different pod);
       no host round-trip needed.

## Scope lifecycle

- A scope is a **contiguous run of steps** sharing a `ScopeID` (a `uses`
  expands to contiguous steps).
- **Lazy provision:** create the environment on the scope's first step.
- **Teardown:** when the next step has a different `ScopeID` / no scope, or the
  run ends. Teardown is best-effort (`defer` + log on failure); on k8s `podgc`
  is the backstop.
- **failFast:** if a scope step fails, the scope's remaining steps are skipped;
  teardown still runs.
- **parallel:** `parallel` sub-steps inside a scoped `uses` exec concurrently
  into the **same** scope environment (concurrent `docker exec` / `kubectl
  exec`, both supported).
- **matrix / foreach:** one independent scope environment per variant.

## Validation & error handling

| Situation | Behavior |
|---|---|
| Nested `runsIn.image`/`container` inside a scoped `uses` | **Parse error** naming the step |
| host: container runtime absent when provisioning a scope | **Run-time hard error** (no silent host fallback, matching base `runsIn.image`) |
| Scope environment fails to start (e.g. image pull failure) | Fail the scope's first step, skip the rest, report the reason; k8s applies a start-wait bound like `imagePodStartTimeout` |
| `upload-artifact` in scope, `CopyOut` path missing (host) | upload-artifact fails, error names the path (**fail-loud**, matching existing artifact semantics) |
| `cache` step failure in scope | **warn + skip** (build continues), matching the existing lenient cache policy on both backends |
| Teardown failure | best-effort log; k8s `podgc` backstop |

## Testing

- **Unit**
  - `inline.go`: scope tagging (`ScopeID`/`ScopeImage` assignment) and the
    nested-`runsIn` parse error.
  - host runtime driver `Create`/`Exec`/`CopyOut`/`Remove` lifecycle via a fake
    runtime.
  - k8s `buildScopePod`: has a scratch volume + artifact sidecar and does **not**
    mount the outer workspace.
- **Integration**
  - host: `download-artifact → compile → upload-artifact → cache` round-trips
    through a real runtime (docker/podman) when available; gated skip otherwise.
  - k8s: fake clientset / envtest for scope pod lifecycle and the sidecar path.
- **Backend parity**: the same template + scope yields the same artifact/cache
  result on host and k8s.
- **Regression**: `uses` without `runsIn`, and step-level `runsIn.image`, both
  keep their current behavior.

## Out of scope (YAGNI)

- Sharing the outer job workspace into a scope (isolation is the point).
- Artifact/cache support for step-level `runsIn.image` (single pure call stays
  artifact/cache-free).
- Nested per-step `runsIn` inside a scope (parse error).
- Cross-scope filesystem sharing.

## Implementation order (rough)

1. DSL/inline: `ScopeID`/`ScopeImage` tagging + nested-`runsIn` validation +
   `api.ClaimStep` fields.
2. `internal/runtime`: extend `ContainerRuntime` with the long-lived lifecycle
   (`Create`/`Exec`/`CopyIn`/`CopyOut`/`Remove`) + OCI-CLI driver impl.
3. host agent scope manager: provision/route/teardown; artifact/cache via
   `CopyIn`/`CopyOut`.
4. k8s agent scope pod manager: `buildScopePod` + exec routing + sidecar
   artifact/cache + GC.
5. Backend-parity and regression tests.
