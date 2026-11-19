# Host agent claim-pod lifecycle parity (TODO 45) — design

Status: agreed in conversation (2026-11-20); written spec awaiting approval

## Why

The same job behaves differently on host and Kubernetes agents. The user's concrete pain:
on their host machines, creating a container from a heavy image (preparing the layered
filesystem for a new container) takes noticeable time on every run. The Kubernetes agent
can keep a job's pod between runs (`podTemplate.reuse: true`); the host agent cannot.
Separately, a host claim pod has no start timeout (a hung pull holds an agent slot
forever), cannot be cancelled while starting, never re-pulls `:latest`/untagged images,
and a crashed host agent leaves its containers behind.

Evidence (from TODO.md item 45):
- `reuse` is read only by `internal/k8sagent/agent.go:449` and
  `internal/k8sagent/podbuilder.go:335`, plus apply-time checks in
  `internal/dsl/volumes.go:125`.
- `pod.Start` runs before `RunClaim` and its cancel poller (`internal/agent/agent.go:955`).
  Scope containers start the same way (`internal/agent/scope.go:94`).
- On the k8s side, the corresponding mechanisms are `PodStartTimeoutDuration`
  (`internal/k8sagent/config.go`), `startRunCancelWatch` (`internal/k8sagent/backend.go`),
  `runPodGC` (`internal/k8sagent/podgc.go`) and `PoolIdleTimeout`.

## Goals

1. **Host claim-pod reuse.** A job with `podTemplate.reuse: true` keeps its claim pod's
   containers after the run, and a later run of a job with the same pod definition on the
   same agent uses them instead of creating new ones. The semantics match Kubernetes:
   - The workspace is emptied before the next run.
   - Other container state (installed packages, files outside the workspace, warm caches
     in the container filesystem) is kept.
   - `reuse` combined with `spec.volumes` stays rejected at apply.
2. **Start limits, cancellation and pulls.**
   - The pull and the start each have their own limit:
     - `podPullTimeout` (default 30m) bounds the image pull.
     - `podStartTimeout` (default 5m, the same as the Kubernetes default) bounds the time
       from the end of the pull until the pod's containers are running.
     - Both are agent settings (flag and config).
     - Kubernetes measures the pull inside its single `podStartTimeout`. The docs state
       this difference.
   - Cancelling a run during pull or start abandons the claim. The partial containers are
     removed, and the agent never calls `FinishRun` a second time.
   - `:latest` and untagged images are always pulled before a run, as kubelet does. Other
     tags are pulled only when missing.
3. **Crash cleanup.** At startup, the host agent removes leftover claim and scope
   containers from a previous process of the same agent that are not tracked by the
   current process. It identifies them by labels it already sets or adds: the agent ID,
   plus a claim-pod marker. A labelled startup sweep is enough, because a running agent
   tracks its own containers.
4. **Tests and docs.** Each of the above is covered by tests. The "Crash-orphaned claim
   containers" section and the `reuse` docs are updated, along with
   `docs/reference/configuration.md` (new settings). Item 45 is removed from TODO.md.

## Non-goals

- Image pre-pulling or declaring images ahead of time (a separate idea if first-run pulls
  matter).
- Changing the container runtime's storage driver. The docs may mention that `vfs` or
  fuse-overlayfs make container creation slow.
- Reusing pods across different agents or machines.

## Anti-goals

- A reused container leaking one run's workspace, secrets or environment into the next
  run. Run-scoped env and credentials are re-injected per run, exactly as on Kubernetes.
  Anything a job writes outside the workspace persists, which is documented and is the
  point of reuse.
- Reusing a container across jobs whose pod definitions differ, including a different
  image digest after a `:latest` re-pull.
- Removing containers that belong to another agent process running on the same machine.

## Decided design

### Pool

- **Pool key.** The pool is keyed by a fingerprint of the resolved pod definition: the
  images (resolved digests), containers, commands, env, mounts and security context.
  Use the same inputs the Kubernetes agent uses for its reuse matching, if it has a
  fingerprint; otherwise define one and document it.
  - Run-specific values (run ID, secrets, credentials, workspace path contents) are
    excluded from the key and injected per run.
- **Idle timeout.** `poolIdleTimeout` (agent setting, same name and meaning as the
  Kubernetes agent) evicts idle pooled pods. `0` means never evict, as on Kubernetes.
  - The default is the Kubernetes agent's default. The builder checks it.
- **Pool size.** `poolMaxIdle` is the maximum number of idle pooled pods per agent
  (default 4). This is the designer's call; it bounds disk and memory. When full, the
  oldest idle pod is evicted.
- **Health check before reuse.** If the containers are not running, or the runtime
  reports them unhealthy, the pod is discarded and a fresh one is created.
- **Fresh image.** A pooled pod built from an image that a `:latest` pull has since
  changed is discarded.
- **Workspace.** The workspace directory mounted into the pod is emptied before reuse,
  by the same helper the Kubernetes agent uses or an equivalent host one. Named volumes
  are unaffected; `reuse` plus `spec.volumes` is already rejected.
- **Cancellation.** A cancelled or failed run returns its pod to the pool only if the pod
  is still healthy. A run cancelled during start never pools a partial pod.

### Timeouts and cancellation

- **Pull.** The pull runs under `podPullTimeout`.
- **Start.** Container creation and start run under `podStartTimeout`.
- **Cancel.** A cancel watcher (the host counterpart of `startRunCancelWatch`) starts
  when the claim is accepted, not after `pod.Start`.
- **Errors.** Timeouts produce run failure messages that name the setting:
  - `claim pod image pull exceeded podPullTimeout (30m0s)`
  - `claim pod start exceeded podStartTimeout (5m0s)`

  The builder confirms the formatting matches Go's `time.Duration` String.

### Crash sweep

- **When.** At startup, before claiming, the agent lists containers with its agent-ID
  label and the claim-pod or scope marker, and removes them.
- **Pooled pods.** Pooled pods from a previous process are also removed. Pool state is
  in memory only, so it is not restored across restarts. This is the designer's call.
- **Shared runtimes.** The sweep is scoped by agent ID, so two agents on one machine
  sharing a runtime never remove each other's containers.

## Designer's calls (settled after the builder check)

1. **Pool scope (user decision, 2026-11-20): same job on the same slot only.**
   - The host workspace is a per-slot, per-job bind mount (`working<slot>/<job>`, via
     `claimWorkDir`). It is fixed when the container is created.
   - The pool key therefore includes the slot and the job name, as well as the pod
     fingerprint. A pooled pod is reused only by a later run of the same job on the same
     slot.
   - The docs say that hit rates are best with few slots.
2. **Run-specific values.**
   - Nothing run-specific may be baked into the containers when they are created. That
     covers the run ID, secrets, credential mounts and tokens. All of it is delivered at
     Exec time.
   - The builder lists every create-time input in `claim_pod.go`/`scope.go`. Each one
     must be either (a) stable for the job and part of the key, or (b) moved to Exec time.
   - If an input can be neither, the job's pod is not pooled. The run proceeds unpooled
     and logs one Info line saying why.
3. **Fingerprint.**
   - A host-specific hash over:
     - the resolved container specs (images by resolved digest, commands, env that is not
       run-specific, mounts, security context);
     - the agent-level inputs `PauseImage`, `RunnerImage`, `ToolsDir`, and the
       security-context probe result.
   - Add a runtime "inspect image digest" call if the runtime interface lacks one, with
     the fake runtime updated.
4. **Health.** A pooled pod is reusable when inspect reports all its containers running.
   No healthcheck support is added.
5. **Workspace emptying.**
   - The workspace is emptied inside the container via Exec, like the Kubernetes
     `clearReusedWorkspace`, because files may be owned by container uids.
   - If emptying fails, the pod is discarded, a fresh pod is created, and the run
     continues.
6. **Labels and sweep.**
   - Claim and scope containers gain the labels `unified-cd/agent-id=<agentID>` and
     `unified-cd/claim-pod=true` (scope: `unified-cd/scope=true`).
   - The startup sweep removes containers carrying this agent's ID label. One agent ID
     per running process is required, as it already is for heartbeats; the docs say so.
   - Containers left by a pre-upgrade agent have no labels and are not swept. The docs
     tell operators to remove them once (`docker ps -a --filter ...` guidance).
7. **Idle timeout default on host.** The host defaults to 30m. On Kubernetes, unset means
   never evict. Combined with `poolMaxIdle: 4`, the host default bounds idle containers.
   The docs state that the defaults differ.
8. **Pulls.**
   - The pull becomes its own step in the runtime interface, run under `podPullTimeout`
     before creation.
   - `:latest` and untagged images are pulled once per claim.
   - A pull failure fails the run. It never silently falls back to a stale local image.
   - A pooled pod whose image digest no longer matches the freshly pulled digest is
     discarded.

## Left to the builder

- **Code.** The exact pool data structure, the fingerprint inputs (aligned with
  Kubernetes), and where the cancel watcher moves.
- **Tests.** At least the following, using the existing fake or recording runtime:
  - reuse hit and miss;
  - workspace emptied;
  - a different pod definition is not reused;
  - an unhealthy pooled pod is discarded;
  - idle eviction and `poolMaxIdle`;
  - pull timeout and start timeout, with the exact messages;
  - cancel during pull or start, with exactly one `FinishRun` and no pooled partial pod;
  - `:latest` re-pull, and a pooled pod discarded on digest change;
  - the startup sweep removes only its own agent's labelled containers.
- **Docs.** `docs/operator-manual/agents.md` ("Crash-orphaned claim containers", the new
  pool section, and a note on slow container creation with `vfs`/fuse-overlayfs),
  `docs/user-guide/writing-jobs` `reuse` docs (now host and Kubernetes),
  `docs/reference/configuration.md`, and TODO.md (remove 45).
