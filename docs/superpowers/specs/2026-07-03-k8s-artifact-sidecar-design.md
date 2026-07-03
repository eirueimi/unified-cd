# Design: k8s-agent artifact support via a workspace sidecar (A3)

**Date:** 2026-07-03
**Status:** Approved (pending implementation plan)

## Problem

The Kubernetes agent silently ignores `uploadArtifact` / `downloadArtifact`
steps: `internal/k8sagent/agent.go` `makeRunStep` dispatches only `run` and
`approval`, so an artifact step falls through, execs an empty command, and
reports **Succeeded** without moving any files. Job authors believe artifacts
were uploaded/downloaded when nothing happened. The standard agent implements
artifacts (tar+zstd over the controller's `/api/v1/runs/{runID}/artifacts/{name}`
endpoint), but its approach reads a path on the agent's local filesystem — the
k8s job's files live inside the pod's workspace volume, which the agent process
cannot access directly.

## Goal

Make `uploadArtifact` / `downloadArtifact` work on the k8s agent, for **any**
user-supplied pod image, by running the transfer in a controlled sidecar
container that shares the pod's workspace volume. Wire format stays compatible
with the standard agent (tar + zstd, same controller endpoint).

## Why a sidecar (and why it's tractable)

The k8s-agent's pod model ALREADY supports what a sidecar needs
([internal/k8sagent/podbuilder.go](../../../internal/k8sagent/podbuilder.go)):
- **Multi-container pods** are supported (podTemplate `Containers` merged via
  `mergeContainers`).
- **The workspace volume is auto-mounted into ALL containers**
  (`injectWorkspace` — "injects a workspace volume mount into all containers").
- **Steps route to a named container** via `step.Container` and `ExecStep`;
  all containers get `sleep infinity` to stay exec-able.

So the sidecar reuses existing infrastructure: inject one more container (our
image with `tar`/`zstd`), it gets the workspace mount for free, and the agent
execs the transfer into it. A user's minimal/distroless job image is irrelevant
because the transfer runs in OUR sidecar, not the job container.

**Token isolation:** the transfer needs a bearer token for the controller's
artifact endpoint. It is set as an env var on the **sidecar container only**.
Job steps run in the *job* container and cannot read another container's
process environment, so a malicious step cannot steal it via `env`.

## Scope decisions

| Question | Decision |
|---|---|
| Which steps | **k8s `uploadArtifact` + `downloadArtifact` only.** |
| **Cache on k8s** | **OUT of scope.** Cache is stored by the agent DIRECT to S3 (`caches/<hash>.tar.zst`), not via the controller — a k8s sidecar would need S3 credentials or a new controller cache-proxy. Separate follow-up. |
| Transfer mechanism | Sidecar runs `tar` + `zstd` + `curl` against the controller artifact endpoint (self-contained; no agent-side streaming, no `ExecStepWithStdin`). |
| Token delivery | **Env var on the sidecar** (isolated from the job container by the container boundary). Secret-based hardening (so the token isn't in the pod spec) is a documented follow-up. |
| Wire format | tar + zstd, `PUT`/`GET /api/v1/runs/{runID}/artifacts/{name}` (same as the standard agent → cross-agent compatible). |
| Sidecar image | New small `docker/artifact-sidecar.Dockerfile` (alpine + `tar` + `zstd` + `curl` + `ca-certificates`). |

## Non-goals (YAGNI / separate)

- Cache on k8s (needs S3-creds-in-pod or a controller cache proxy).
- Secret-based token delivery (env for v1; the token appears in the pod spec —
  documented caveat; not readable by job steps).
- The human-facing artifact API / CLI / e2e (items B/C/E — separate).
- Making cache/artifact steps *fail loudly* on agents that don't support them
  (a separate small safety fix; this design makes k8s artifact actually work).

## Design

### 1. Sidecar image — `docker/artifact-sidecar.Dockerfile`

```dockerfile
FROM alpine:3.20
RUN apk add --no-cache tar zstd curl ca-certificates
```

Runs `sleep infinity` (injected by the existing `injectSleepInfinity`). Small,
fixed image published like the runner image.

### 2. Auto-inject the sidecar — `internal/k8sagent/podbuilder.go`

In `BuildPod`, after the base pod spec is built and BEFORE `injectWorkspace`
(so the sidecar receives the workspace mount automatically), append a container:

```go
const artifactSidecarName = "unified-artifact"

// injected container (conceptual):
corev1.Container{
    Name:    artifactSidecarName,
    Image:   <sidecar image>,          // from agent config, default the published sidecar image
    Command: []string{"sleep", "infinity"},
    Env: []corev1.EnvVar{
        {Name: "UNIFIED_SERVER", Value: <controller in-cluster URL>},
        {Name: "UNIFIED_AGENT_TOKEN", Value: <agent token>},
    },
}
```

`BuildPod`'s signature gains the controller URL + agent token + sidecar image
(threaded from the k8s-agent config). The sidecar name `unified-artifact` is
reserved (validation/merge must not let a user podTemplate collide with it —
if a template defines a container named `unified-artifact`, error or override
with a clear message).

### 3. Dispatch in `makeRunStep` — `internal/k8sagent/agent.go`

Add branches (after the `if:` gate, alongside the approval branch, BEFORE the
run/exec branch):

```go
if step.UploadArtifact != nil {
    // exec into the sidecar; path is relative to the workspace mountPath.
    script := fmt.Sprintf(
        `tar cf - -C %q . | zstd -q | curl -fsS -X PUT `+
        `-H "Authorization: Bearer $UNIFIED_AGENT_TOKEN" --data-binary @- `+
        `"$UNIFIED_SERVER/api/v1/runs/%s/artifacts/%s"`,
        filepath.Join(mountPath, step.UploadArtifact.Path), c.RunID, step.UploadArtifact.Name)
    // run via artifactExec(ctx, artifactSidecarName, script) -> exit code
    ...
}
if step.DownloadArtifact != nil {
    dest := step.DownloadArtifact.DestDir; if dest == "" { dest = "." }
    script := fmt.Sprintf(
        `mkdir -p %q && curl -fsS -H "Authorization: Bearer $UNIFIED_AGENT_TOKEN" `+
        `"$UNIFIED_SERVER/api/v1/runs/%s/artifacts/%s" | zstd -dq | tar xf - -C %q`,
        filepath.Join(mountPath, dest), c.RunID, step.DownloadArtifact.Name, filepath.Join(mountPath, dest))
    ...
}
```

Report the step `Running` then `Succeeded`/`Failed` from the exec exit code;
record failure into the failed flag on non-zero (respecting `continueOnError`),
mirroring the run/approval branches.

**Testability seam:** the transfer runs through an injectable
`artifactExec func(ctx, container, script string) (exitCode int, stderr string, err error)`.
In production it calls `a.exec.ExecStep(ctx, podName, container, script, io.Discard, stderrPusher)`.
In the cluster-free `orchestrate_test`, a fake records `(container, script)` and
returns a configurable exit code — so a unit test asserts an `uploadArtifact`
step execs into `unified-artifact` with a `tar … | zstd … | curl -X PUT …`
command (and download likewise), without a real cluster.

### 4. Tests

- **Unit (cluster-free):**
  - `podbuilder_test.go`: `BuildPod` produces a pod whose containers include
    `unified-artifact` with the workspace volume mount and the two env vars.
  - `orchestrate_test.go`: an `uploadArtifact` step → `artifactExec` called with
    container `unified-artifact` and a PUT command; a `downloadArtifact` step →
    a GET+extract command; a non-zero exit → step `Failed` + run `Failed`.
- **Integration (`//go:build k8s`, needs a real cluster — not run in this
  environment):** a real pod round-trip: a `run` step writes a file, an
  `uploadArtifact` step stores it, a `downloadArtifact` step restores it into a
  fresh dir, a `run` step verifies it.

### 5. Docs

`docs/kubernetes-integration.md` (and the `docs/jobs.md` Artifacts section):
document that k8s artifacts work via an auto-injected `unified-artifact`
sidecar; the token is set on that sidecar's env (not readable by job steps;
appears in the pod spec — Secret hardening is a follow-up); **cache is not yet
supported on the k8s agent**.

## Touch points

| Path | Change |
|---|---|
| `docker/artifact-sidecar.Dockerfile` | new sidecar image (tar+zstd+curl) |
| `internal/k8sagent/podbuilder.go` | inject the `unified-artifact` sidecar (workspace mount + env); reserve the name |
| `internal/k8sagent/agent.go` | dispatch `uploadArtifact`/`downloadArtifact` via the sidecar exec; `artifactExec` seam |
| `internal/k8sagent/config.go` / `cmd/k8s-agent` | thread controller URL + token + sidecar image into `BuildPod` |
| `internal/k8sagent/podbuilder_test.go`, `orchestrate_test.go` | unit tests |
| `internal/k8sagent/*_k8s_test.go` (`//go:build k8s`) | integration round-trip |
| `docs/kubernetes-integration.md`, `docs/jobs.md` | document behavior + caveats |

## Acceptance criteria

- Unit tests pass (cluster-free): the sidecar is injected with the workspace
  mount + env; artifact steps dispatch to the sidecar exec with correct
  tar/zstd/curl commands; a failed transfer fails the step/run.
- `go build ./...` and `go build -tags k8s ./internal/k8sagent/` compile.
- A `//go:build k8s` integration test exists that round-trips an artifact
  through a real pod (documented as requiring a cluster; not run here).
- Docs state the sidecar behavior, the token caveat, and that cache remains
  unsupported on k8s.
