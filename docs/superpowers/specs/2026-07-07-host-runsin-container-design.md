# `runsIn.container` on the host agent (named, workspace-shared containers)

- Date: 2026-07-07
- Status: Design approved (implementation plan pending)
- Related: [2026-07-05-runsin-design.md](2026-07-05-runsin-design.md) (base `runsIn`), [2026-07-05-runsin-uses-scope-artifacts-design.md](2026-07-05-runsin-uses-scope-artifacts-design.md) (isolated scope)

## Background & motivation

`runsIn.container: X` means "exec the step into a pre-provisioned, named
container." On the k8s agent this already works: `X` names a container in the
job's `podTemplate.spec.containers`, all pod containers share the pod's
workspace volume, and the agent execs the step into container `X`. On the host
agent it is a run-time error today — `hostBackend.RunNamedContainer` is a stub
that returns "not supported on the host agent."

`runsIn.image` (already host-supported) runs a step in a **fresh, isolated,
unmounted** container — a pure function with no workspace. What is missing on
the host is the **workspace-shared** counterpart: run a step in a reproducible
container that shares the job workspace, so multi-step flows, `cache`,
`uploadArtifact`, `downloadArtifact`, and `outputs:` work without the
`with:`/artifact round-trip an isolated scope requires.

**Goal:** implement `runsIn.container` on the host agent so it reaches parity
with k8s — the named container comes from the job's `podTemplate`, the step
execs into it, and the container shares the job workspace.

## Scope (MVP) and non-goals

- **In scope:** a step with `runsIn.container: X` runs in a long-lived container
  named `X` (from the job's `podTemplate.spec.containers`) with the host
  workspace bind-mounted, the step exec'd into it. Only the container(s)
  actually referenced by a `runsIn.container` step are started (lazy,
  single-container).
- **Out of scope (YAGNI):** starting *all* podTemplate containers eagerly;
  sidecar-to-sidecar `localhost` networking / a pod-equivalent shared network
  namespace; honoring arbitrary k8s PodSpec fields (volumes beyond the
  workspace, securityContext, resource requests, initContainers, ports).

## Confirmed decisions

1. **Container definitions come from the job's `podTemplate.spec.containers`** —
   the same source the k8s agent uses, so one job YAML works on both backends.
2. **Host honors a supported subset** — `name`, `image`, `command`, `env`, and
   `resources.limits` (→ `--cpus`/`--memory`). Any host-unsupported k8s-only
   field on the container is **ignored with a WARN log**, not an error.
3. **MVP = single-container exec** — lazily start only the referenced container,
   with the workspace bind-mounted. No sidecar networking.

## Architecture

Implement the existing `ExecBackend.RunNamedContainer` hook on the host backend;
the orchestrator already dispatches `runsIn.container` steps to it
(`orchestrator.go`). A new claim-scoped `namedContainerManager` (sibling of the
existing `scopeManager`) owns the containers. Unlike a uses-scope — which is
**isolated/unmounted** and needs `copyIn`/`copyOut` for cache/artifact — a named
container **bind-mounts the job workspace**, so files it writes land in the host
`workDir` and the existing non-scope cache/artifact/output paths see them with
no copy step.

## Components & changes

1. **`internal/runtime` — bind mount + workdir in `CreateSpec`.**
   `CreateSpec` already carries `WorkDir` (from the uses-scope work). Add a
   workspace bind mount, e.g.:
   ```go
   type Mount struct{ HostPath, ContainerPath string }
   // CreateSpec gains:  Mounts []Mount
   ```
   `ociCLI.Create` and `appleContainer.Create` emit `-v <HostPath>:<ContainerPath>`
   for each mount (in addition to the existing `-w <WorkDir>`, `-e`, cpu/mem).
   A `CreateSpec` with no mounts is unchanged (uses-scope keeps using an empty
   scratch container).

2. **`internal/agent/named_container.go` — `namedContainerManager` (new).**
   - `func newNamedContainerManager(rt crt.ContainerRuntime, workDir, mountPath string) *namedContainerManager`
   - `open map[string]crt.ContainerHandle` keyed by **container name**, guarded
     by a `sync.Mutex` (parallel: stages exec concurrently on the host —
     `ConcurrencyMode: Concurrent`).
   - `ensure(ctx, name string, def containerDef) (crt.ContainerHandle, error)` —
     first use per name: `rt.Create(CreateSpec{Image: def.Image, Env: def.Env,
     Shell/Command from def, WorkDir: mountPath, Mounts: [{workDir, mountPath}],
     CPULimit/MemLimit: def.limits})` with the keep-alive command
     (`def.command` or `sleep infinity`); cached thereafter.
   - `exec(ctx, h, script, env, stdout, stderr) (int, error)` — `rt.Exec`.
   - `closeAll(ctx)` — `rt.Remove` every open container; best-effort.

3. **`internal/agent` — podTemplate container extraction helper.**
   `func namedContainerDef(pt *dsl.PodTemplate, name string) (containerDef, error)`
   parses `pt.spec.containers`, finds the entry whose `name == name`, and returns
   `{Image, Command, Env, CPULimit, MemLimit}`. WARN (once per container) on any
   host-unsupported field present on that container. Errors: no podTemplate; no
   container named `name`.

4. **`internal/agent/backend_host.go` — implement `RunNamedContainer`.**
   `PodTemplate` lives on `ClaimResponse` (claim-level), NOT on `ClaimStep`, so
   `RunNamedContainer(step, ...)` cannot read it from `step`. Thread it into the
   backend at construction: `newHostBackend(a, c.RunID, workDir, c.PodTemplate)`
   (the host claim loop already has `c.PodTemplate`, `agent.go`), and store it on
   `hostBackend`. Then:
   ```
   rt := b.a.containerRuntime()              // absent → hard error
   def := namedContainerDef(b.podTemplate, container)  // missing/unknown → error
   nm := b.namedContainers(rt)               // lazy, per claim (mutex, like getScopes)
   h  := nm.ensure(ctx, container, def)
   return nm.exec(ctx, h, script, env, stdout, stderr)
   ```
   `hostBackend` gains a lazily-created `*namedContainerManager` (+mutex),
   mirroring `getScopes`. `CloseScopes` (the claim-end teardown hook) also closes
   the named-container manager.

5. **`internal/agent/backend_host.go` — post hooks in the named container.**
   `RunPostHook` already receives `container` (the step's `RunsIn.Container`,
   passed by `orchestrator.go`). When `container != ""` and non-scoped, run the
   post script via the named-container manager (exec into container `X`) instead
   of on the host workspace, so a `runsIn.container` step's `post:` runs where
   the step body ran. The container is still alive at post-hook time (torn down
   only at claim end).

6. **`internal/agent/agent_os.go` — `UNIFIED_AGENT_OS`.**
   Extend `agentOSForStep` to also return `"linux"` for a `runsIn.container`
   step (it runs in a Linux container, same rationale as `runsIn.image`).

7. **Cache / artifact / outputs — no change needed.** A `runsIn.container` step
   is not a uses-scope (`ScopeID == ""`), so `cache`/`uploadArtifact`/
   `downloadArtifact`/`outputs:` steps take the existing non-scope host path
   against `workDir`. Because the named container bind-mounts `workDir`, files
   it writes are already there. This is the key simplification over isolated
   scopes.

## Workspace & mount path

- Mount path defaults to `/workspace`; if the job's
  `podTemplate.workspace.mountPath` is set, use that (so it matches k8s). The
  container's working directory is set to the mount path.
- Bind mount: `-v <host workDir>:<mountPath>`. The host `workDir` is the claim's
  working directory the host agent already uses for non-container steps, so a
  `runsIn.container` step and a plain host step operate on the same tree.

## Error handling

| Situation | Behavior |
|---|---|
| Container runtime absent | Run-time hard error (no silent host fallback; same as `runsIn.image`) |
| Step has no `podTemplate` | Error: `runsIn.container %q requires a podTemplate that defines it` |
| Name not in `podTemplate.spec.containers` | Error: `container %q is not defined in the job's podTemplate` |
| Container fails to start (e.g. image pull) | Step reported Failed with the error |
| Host-unsupported PodSpec field on the container | WARN (once per container), continue with the supported subset |
| Teardown failure | Best-effort, logged |
| Concurrent `parallel:` steps referencing the same name | `namedContainerManager` mutex makes `ensure` atomic — one `Create` per name |

Parsing is unchanged: `container:` still normalizes to `runsIn.container`, and
`runsIn.image` + `runsIn.container` remain mutually exclusive (compile-time).

## Testing

- **Unit — runtime:** `ociCLI.Create` argv includes `-v <host>:<mount>` and
  `-w <mount>` when `CreateSpec.Mounts`/`WorkDir` are set (argv-capture via the
  existing `execCommand` seam); appleContainer parity.
- **Unit — `namedContainerManager`** (fake runtime): one `Create` per name,
  `closeAll` removes; the `CreateSpec` carries the bind mount + workdir.
- **Unit — `namedContainerDef`:** extracts `name→image/command/env`; returns an
  error for a missing podTemplate / unknown name; WARNs on an unsupported field.
- **Unit — `RunNamedContainer`** (fake runtime): unknown container / no
  podTemplate / runtime absent → error; valid → execs into the named container
  (assert the exec target handle).
- **Integration (docker/podman-gated):** a job whose `podTemplate` defines a
  container `tools`, a `runsIn.container: tools` step that writes a file under
  the workspace, and a following plain host step (or `uploadArtifact`) that reads
  it — asserting the workspace is shared. Also assert `UNIFIED_AGENT_OS=linux`
  inside the container. Skip when `runtime.Detect("")` errors.
- **Regression:** `runsIn.image` (isolated, unmounted) and plain host steps are
  unchanged; a `CreateSpec` with no mounts behaves as before.

## Implementation order (rough)

1. `internal/runtime`: add `Mount`/`CreateSpec.Mounts`; emit `-v` in ociCLI +
   apple `Create`; argv tests.
2. `internal/agent`: `namedContainerDef` extraction helper (+WARN) and tests.
3. `internal/agent/named_container.go`: `namedContainerManager` + tests.
4. `internal/agent/backend_host.go`: implement `RunNamedContainer`, wire the
   lazy manager + `CloseScopes` teardown; `agent_os.go` linux for
   `runsIn.container`; `RunPostHook` named-container routing; tests.
5. Integration test + docs (jobs.md / resources.md / kubernetes-integration.md:
   note `runsIn.container` now works on the host, sharing the workspace, from the
   job's podTemplate; single-container MVP limitation).
