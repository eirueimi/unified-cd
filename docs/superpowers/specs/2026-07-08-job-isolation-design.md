# Job-level isolation: isolated-by-default jobs, `native: true`, and the claim pod on the host agent

- Date: 2026-07-08
- Status: Design approved (implementation plan pending)
- Related: [2026-07-07-host-runsin-container-design.md](2026-07-07-host-runsin-container-design.md)
  (named containers, superseded in part), [2026-07-05-runsin-design.md](2026-07-05-runsin-design.md)
  (base `runsIn`, step-level image form removed by this design),
  [2026-07-06-shared-orchestrator-design.md](2026-07-06-shared-orchestrator-design.md) (the
  `ExecBackend` seam this design builds on)

## Background & motivation

Two forces drove this design:

1. **Sidecar port collisions.** When a job's `podTemplate` declares a service
   sidecar (e.g. MySQL) and two runs execute concurrently on one host agent,
   publishing the sidecar on a host port collides. On the k8s agent this
   problem does not exist: one pod = one network namespace, sidecars are
   reached on `localhost`, and pods are isolated from each other.
2. **The half-isolated middle state.** The 2026-07-07 design let a host agent
   accept a podTemplate job while running default steps as host processes.
   A host process cannot join the pod's network namespace, so sidecars are
   not reachable on `localhost`, semantics diverge from k8s, and the same
   YAML behaves differently per backend.

**Goal:** make the pod model the default on both backends. An unmarked job
runs fully containerized inside a per-claim "pod" (shared netns + shared
workspace) on the host agent, exactly as it does on k8s. Jobs that exist to
use the host itself (Xcode, signing identities, attached devices) opt out
explicitly with `native: true` and run as host processes, with no container
features. Breaking changes are accepted.

**Prior art (validation).** Concourse CI embodies the same split: Linux
workers run every task in a container (no host execution at all), while
macOS/Windows workers use Houdini, a deliberate no-op isolation backend —
i.e. "native" — selected by platform. Concourse has no built-in sidecars
(users nest docker-compose inside privileged tasks, incidentally obtaining
per-task network isolation) and solves root-ownership via user namespaces.
This design makes the sidecar case a first-class feature and recommends
rootless podman for the same userns fix. The shared-workspace model (GitHub
Actions style) is retained; Concourse's explicit input/output volume flow is
deliberately not adopted.

## Scope and non-goals

- **In scope:** `spec.native`; the host-agent claim pod (pause container,
  netns sharing, eager sidecars, workspace bind mount); removal of
  step-level `runsIn.image`; `container:` as the canonical step field;
  per-job workspace subdirectories; root-ownership handling; parity cases.
- **Out of scope (YAGNI):** microVM execution; readiness/liveness probes for
  sidecars; host-port publishing for sidecars; honoring arbitrary k8s
  PodSpec fields on the host beyond the existing supported subset; capability
  -based claim filtering (agents advertising "I have a runtime"); automatic
  workspace GC.

## Confirmed decisions

1. **Isolated is the default; `native: true` is the opt-out.** An unmarked
   job has pod semantics on both backends (k8s: a real pod; host: the claim
   pod below). `spec.native: true` runs every step as a host process. This
   matches k8s (which has no native concept — unmarked YAML behaves the same
   everywhere) and is safe-by-default. Breaking change accepted.
2. **Host isolated jobs emulate a pod with a pause container.** A per-claim
   pause container owns the network namespace; every other claim container
   joins it via `--network container:<pause>`. Sidecars are reached on
   `localhost`, matching k8s exactly. No host ports are published, so
   concurrent claims can never collide.
3. **Step-level `runsIn.image` is deleted.** Job-level isolation + named
   containers replace its role. The `uses:`-template-level `runsIn.image`
   (scope inlining via `ScopeID`/`ScopeImage`) is kept — it is a separate
   code path and is untouched.
4. **`container: X` is the canonical step field; step-level `runsIn:` is
   removed.** This inverts today's normalization (flat `container:` is
   currently the deprecated form). Step-level `runsIn.resources` is removed
   too: resource limits live on the podTemplate container definition (the
   k8s way; the host already honors `resources.limits` from there).
5. **Workspace layout: `wsBase/working<slot>/<sanitized-job-name>`.** The
   slot level stays (fixed, bounded, operationally predictable — a
   deliberate rejection of Jenkins-style dynamic `@2` suffixes); a per-job
   subdirectory is added so jobs never share a directory. Carry-over across
   claims of the same job is preserved (CleanWorkspace default stays false).
6. **Root-ownership on Linux hosts** is handled by (a) recommending rootless
   podman first (container root maps to the agent user; the problem
   disappears), (b) per-job directories (a native job never inherits an
   isolated job's root-owned files — different directory), (c) an
   EPERM fallback in the cleaning path (delete via a root cleanup container),
   and (d) a mode marker that resets the directory when a job's
   native/isolated mode flips.
7. **Parity is enforced by shared parity cases**, not shared wiring code:
   a `paritycases` entry asserting "sidecar reachable on localhost from a
   default step; concurrent claims isolated" runs against both backends.
8. **`native` is host-only.** The k8s agent cannot honor it; a native job
   claimed by a k8s agent fails fast with a clear error. Routing is the
   operator's job via `agentSelector`.
9. **Apple `container` is out of scope for the claim pod** (no reliable
   `--network container:` equivalent; containers are per-VM). Supported
   runtimes for isolated jobs: docker, podman, nerdctl. macOS hosts run
   isolated jobs via docker/podman (Linux VM); Apple `container` remains
   usable only where it already was.

## Schema & validation

```yaml
apiVersion: unified-cd/v1
kind: Job
metadata: { name: integration-test }
spec:
  podTemplate:
    spec:
      containers:
        - name: mysql
          image: mysql:8
          env: [{ name: MYSQL_ALLOW_EMPTY_PASSWORD, value: "1" }]
  steps:
    - name: test
      run: ./gradlew test          # default: primary container, mysql on localhost:3306
    - name: dump
      container: mysql             # exec into the named sidecar
      run: mysqldump ...
---
apiVersion: unified-cd/v1
kind: Job
metadata: { name: ios-release }
spec:
  native: true                     # host processes; no container features
  agentSelector: [macos]
  steps:
    - name: build
      run: xcodebuild ...
```

- `Spec.Native bool` (`yaml:"native,omitempty"`), sibling of `PodTemplate`.
- `StepEntry`/`Step`: the `RunsIn` field is **removed**; `Container string`
  stays and is canonical. `normalizeRunsIn` is deleted; parsing rejects a
  step-level `runsIn:` key with a migration hint ("use container:, or move
  image isolation to the job's podTemplate / a uses template").
- Compile-time validation:
  - `native: true` + `podTemplate` → error
  - `native: true` + any step `container:` → error
  - step-level `runsIn:` (any form) → error; `uses:` templates keep their
    template-level `runsIn.image` (inlined to `ScopeID`/`ScopeImage` as today)
- `container: X` requires a `podTemplate` defining container `X`
  (unchanged rule, now compile-time checkable for isolated jobs).

## Host execution model

### Claim pod construction (isolated jobs)

Built lazily-at-claim-start by a `claimPodManager` (successor of
`namedContainerManager`, `internal/agent/named_container.go`):

1. **Pause container**: minimal image (agent config, e.g. busybox `sleep
   infinity` or a pause image), owns the netns. One per claim.
2. **Primary container**: the target of default (`container:`-less) steps.
   Resolution must match the k8s agent's existing behavior for which
   container default steps exec into (verify at plan time); when the
   podTemplate defines none, inject the agent-configured default runner
   image (parallel to k8s `defaultPodSpec`).
3. **All podTemplate containers eager-started** (`sleep infinity` keep-alive,
   honoring the existing supported subset: name/image/env/resources.limits,
   WARN on the rest), each with `--network container:<pause>` and the
   workspace bind mount.
4. **Workspace**: the claim's host workDir bind-mounted at `/workspace` (or
   `podTemplate.workspace.mountPath`) into every container;
   `-w <mountPath>` set. Host-side agent operations (artifact upload/download,
   cache, teardown) keep using the host path — the bind mount makes them 1:1.
5. **No `-p`/port publishing.** Within-claim port duplication (two sidecars
   both binding 3306) fails exactly as it would in a k8s pod — parity-correct.

Step dispatch inside an isolated job: default step → exec into primary;
`container: X` → exec into X; `uses:` scope steps → existing isolated scope
containers (unchanged; scopes stay outside the claim netns, matching k8s
where scope pods are separate pods). `post:` hooks run in the same container
the step body ran in. `UNIFIED_AGENT_OS=linux` for every step of an isolated
job.

Teardown (`CloseScopes` claim-end hook): remove all claim containers, then
the pause container; best-effort, logged.

### Native jobs

Exactly today's host behavior: steps are host processes rooted at the claim
workDir, `UNIFIED_AGENT_OS=<host os>`, no container runtime required, no
podTemplate, no `container:` steps. `uses:` scope steps still work if a
runtime happens to be present (unchanged from today: scopes already require
a runtime).

### Path resolution parity fix

For **isolated** jobs the host backend resolves non-scoped cache paths
against the claim workDir (k8s joins the pod mount path; the bind mount
makes these equivalent). **Native** jobs keep the legacy host behavior
(cache path as authored). Artifact resolution is already workDir-rooted and
equivalent on both.

## Workspace layout & hygiene

```
wsBase/
  working0/
    integration-test/    ← claim workDir for job "integration-test" on slot 0
      .ucd-mode          ← mode marker: "isolated" | "native"
    ios-release/
  working1/
    integration-test/    ← same job, concurrent run on slot 1 (separate dir)
```

- `workDir = wsBase/working<slot>/<sanitize(metadata.name)>`. Job names are
  already restricted by DSL name validation; sanitization is a defensive
  escape for path-unsafe characters.
- **Carry-over default preserved** (`CleanWorkspace` stays opt-in, default
  false). Per-job subdirectories make carry-over meaningful: a job inherits
  its *own* previous state, never another job's files (this also fixes
  today's cross-job file mixing within a slot).
- **`podTemplate.cleanWorkspace: true` is honored by the host agent** (clean
  the job's workDir at claim start), the same per-job knob k8s pool pods
  already honor. Effective cleaning = agent-level `CleanWorkspace` OR
  job-level `cleanWorkspace`.
- **EPERM fallback:** when cleaning fails on root-owned files (rootful
  docker), retry via a cleanup container
  (`<rt> run --rm -v <workDir>:/w busybox rm -rf /w/. …`), then recreate.
- **Mode marker:** `.ucd-mode` records the job mode last run in the dir; if
  a claim's mode differs (a job definition flipped native↔isolated), reset
  the directory via the cleanup-container path before use. Closes the one
  remaining root-leftover hole.
- **Ops notes (docs):** per-job dirs accumulate — disk GC is an operator
  responsibility. On macOS/Windows, `wsBase` must live under a path the
  container runtime's file sharing exposes (e.g. `/Users` on Docker
  Desktop). Rootless podman is the first-choice deployment on Linux hosts.

## Deletion of step-level `runsIn.image`

Removed: `ExecBackend.RunImage` and both implementations, the orchestrator
dispatch case (`orchestrator.go` `case step.RunsIn.Image != ""`),
`buildImageStepPod` (k8s), host `RunImage` + one-shot `RunSpec` usage where
now unused, step-level image validation, and the `RunsIn` step field itself.
Kept: the entire uses-scope path (`ScopeID`/`ScopeImage`, `EnsureScope`/
`RunInScope`, scope pods/containers) — it never touched `RunImage`.
Migration: a step needing a specific image becomes a podTemplate container +
`container:`, or a `uses:` template with template-level `runsIn.image`.

## k8s agent changes (minimal)

- Reject `native: true` claims fast (clear error).
- `RunImage`/`buildImageStepPod` deletion (above).
- Everything else is already pod-semantics; no behavior change.

## Error handling

| Situation | Behavior |
|---|---|
| Isolated job claimed by a host agent with no container runtime | Run fails fast with a clear error ("isolated jobs require docker/podman/nerdctl; mark the job native: true or route it via agentSelector"). Operators route native-only agents by labels. |
| `native: true` claimed by k8s agent | Run fails fast (native is host-only). |
| Pause/sidecar/primary fails to start (image pull etc.) | Run fails; already-started claim containers torn down. |
| Two sidecars bind the same port in one claim | Fails inside the claim (pod-parity behavior). |
| Sidecar not yet ready when a step connects | MVP: no probes; steps retry/wait themselves. Documented limitation. |
| Teardown failure | Best-effort, logged. |
| Workspace clean EPERM (root-owned leftovers) | Cleanup-container fallback; if that also fails, WARN and proceed (today's behavior). |
| Job mode flipped native↔isolated | Mode marker triggers a directory reset. |

## Testing

- **Unit — claimPodManager** (fake runtime): pause created first; all
  podTemplate containers eager-started with `--network container:<pause>`,
  bind mount, workdir; primary resolution (incl. default-runner injection);
  teardown order (containers then pause); one construction per claim under
  concurrent step entry.
- **Unit — runtime argv**: `--network container:<id>` emitted by
  `ociCLI.Create`; no `-p` ever.
- **Unit — schema/validation**: `native`+podTemplate, `native`+`container:`,
  step `runsIn:` rejected (with hint); `container:` requires podTemplate
  definition; parse of `spec.native`.
- **Unit — workspace**: per-job path computation + sanitization; mode-marker
  reset; cleanWorkspace OR-combination; EPERM fallback invoked (fake
  runtime).
- **Integration (docker/podman-gated)**: podTemplate with a TCP sidecar; a
  default step reaches it on `localhost`; two concurrent claims of the same
  job run without port collision; workspace file written by a `container:`
  step visible to a default step and to artifact upload.
- **Parity (`internal/paritycases`)**: "sidecar on localhost + cross-claim
  isolation" case executed by both `parity_host_test.go` and
  `parity_k8s_test.go`.
- **Regression**: native jobs behave exactly like today's host jobs;
  uses-scope inline/exec/artifact paths unchanged; k8s unmarked jobs
  unchanged.

## Implementation order (rough)

1. DSL: `spec.native`, step `RunsIn` removal / `container:` canonicalization,
   validation + parser errors with migration hints; schema JSON update.
2. `internal/runtime`: `CreateSpec.Network` (join `container:<id>`), argv
   tests.
3. `internal/agent`: `claimPodManager` (pause, eager start, primary
   resolution, teardown) replacing `namedContainerManager`'s lazy model;
   workspace layout (per-job dir, mode marker, EPERM fallback,
   `podTemplate.cleanWorkspace`).
4. Backend dispatch: host `RunDefault` → primary exec for isolated claims;
   `RunNamedContainer` → claim pod lookup; cache-path parity fix; k8s native
   rejection.
5. Deletion: `RunImage` seam-wide, `buildImageStepPod`, orchestrator case.
6. Parity case + integration tests.
7. Docs: agents.md / jobs.md / resources.md / kubernetes-integration.md /
   configuration.md (rootless podman recommendation, file-sharing note, GC
   note, migration guide for the breaking changes).
