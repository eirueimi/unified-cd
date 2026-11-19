# Host Claim-Pod Lifecycle Parity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the host agent's claim pod the Kubernetes agent's lifecycle: labelled containers with a startup crash sweep, a separate image-pull step with pull and start timeouts, `:latest` re-pulls, cancellation during pull/start, and `podTemplate.reuse` pooling.

**Architecture:** The container runtime gains `Labels` on `CreateSpec` and an optional `crt.PodRuntime` interface (image IDs, running state, label listing, logs since a time). `executeRun` (`internal/agent/agent.go`) gets a claim-start phase: a cancel watcher, then pull under `podPullTimeout`, then pool acquire or `claimPodManager.Start` under `podStartTimeout`. A new in-memory `hostPodPool` (`internal/agent/pool_host.go`) keeps one idle claim pod per `working<slot>/<job>` directory, tagged with a fingerprint of the resolved pod. `hostBackend.CloseScopes` hands the pod back to the pool instead of removing it.

**Tech Stack:** Go 1.26, the `internal/runtime` OCI CLI driver (docker/podman/nerdctl/wslc), testify, `httptest` fake controller (`newIsolatedServer` in `internal/agent/agent_isolated_test.go`), and the recording fake runtime `podFakeRT` (`internal/agent/claim_pod_test.go`).

**Spec:** `docs/superpowers/specs/2026-11-20-host-claim-pod-parity-design.md` (approved; "Designer's calls" are binding).

## Global Constraints

- `podPullTimeout`: default `30m`. Agent flag `--pod-pull-timeout`, config `podPullTimeout`, env `UNIFIED_AGENT_POD_PULL_TIMEOUT`. A non-positive value means the default.
- `podStartTimeout`: default `5m`, the same as the Kubernetes default. Flag `--pod-start-timeout`, config `podStartTimeout`, env `UNIFIED_AGENT_POD_START_TIMEOUT`. A non-positive value means the default.
- `poolIdleTimeout`: host default `30m`. `0` means never evict. Flag `--pool-idle-timeout`, config `poolIdleTimeout`, env `UNIFIED_AGENT_POOL_IDLE_TIMEOUT`. The Kubernetes agent's default stays unset (never evict).
- `poolMaxIdle`: default `4` idle pooled pods per agent. When the pool is full, the oldest idle pod is evicted. Flag `--pool-max-idle`, config `poolMaxIdle`, env `UNIFIED_AGENT_POOL_MAX_IDLE`. `0` disables host pooling.
- The exact run-failure strings:
  - `claim pod image pull exceeded podPullTimeout (30m0s)`
  - `claim pod start exceeded podStartTimeout (5m0s)`

  Each is built with `fmt.Sprintf("... (%s)", d)` from a `time.Duration`. The whole run-log line is exactly this string, with no `claim pod construction failed:` prefix.
- Labels:
  - `unified-cd/agent-id=<agentID>` on every claim and scope container.
  - `unified-cd/claim-pod=true` on claim-pod containers (pause included).
  - `unified-cd/scope=true` on scope containers.
- Pool scope: the same job on the same slot with the same fingerprint. The pool is keyed by `workDir` (`claimWorkDir(wsBase, slot, job)` = `working<slot>/<job>`), and each entry stores the job name and the fingerprint.
- `:latest` and untagged images are pulled once per claim. Other tags and digest-pinned refs are pulled only when missing. A pull failure fails the run, with no fallback to a stale local image.
- Pool state is in memory only. Pooled pods from a previous process are removed by the startup sweep.
- **Repo rules.**
  - English only, no PII (AGENTS.md).
  - Docs must stay consistent with the code.
  - Commit with `git -c user.name=eirueimi -c user.email="128969149+eirueimi@users.noreply.github.com" commit`. Every message ends with:
    ```
    Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
    Claude-Session: https://claude.ai/code/session_01G3NWwwXnjYtSfzJjZYTrnV
    ```

## Rulings (spec gaps resolved by this plan)

1. **What "digest" means.** The image ID from `image inspect --format {{.Id}}`. It exists for local-only images, which have no RepoDigest.
2. **Pool shape.** Pooling within the same job and slot leaves at most one idle pod per `workDir`. The pool is therefore a `map[workDir]*pooledHostPod`. A fingerprint mismatch on acquire, such as a new image ID after a `:latest` pull, removes the old pod.
3. **Bind-mount inode.** `prepareWorkspace` and `finishWorkspace` remove the workspace directory itself. A pooled container would then keep a bind mount to the deleted inode. For a poolable claim, both of them empty only the directory's contents (`keepDir`). The workspace GC skips directories that the pool holds.
4. **What counts as healthy at release.** The pod must be running, and no step exec may have been interrupted by context cancellation. A `docker exec` that is killed on the client side keeps its process running inside the container. So a cancelled, timed-out or step-timed-out run discards its pod. A run that failed with a non-zero step exit is pooled if its containers are running.
5. **`FinishRun` on cancel during pull/start.** The controller already holds the terminal state, so the agent abandons the claim and sends no `FinishRun`. This matches the Kubernetes `awaitPodRunning` abandonment. Tests assert 0 agent `FinishRun` calls, so there is a single terminal transition.
6. **Claims that are never pooled.** Each logs one Info line naming the reason:
   - detached runs (the per-run workDir is never reused);
   - runtimes that do not implement `crt.PodRuntime`;
   - `poolMaxIdle: 0`.

   `reuse` together with `c.Volumes` fails the run, as on Kubernetes: `spec.volumes cannot be combined with podTemplate.reuse: a reused pod keeps the volumes it was created with, which contradicts a volume's lifecycle`.
7. **Scope containers** are labelled and swept, but never pooled. Their images are not part of the pull step (out of the spec's scope).
8. **Sidecar logs on a reused pod** stream with `LogsSince(claimStart)`. Otherwise the previous run's sidecar output, masked with the previous run's secrets, would replay into this run's log.
9. **`ClearWorkspaceScript`** moves from `internal/k8sagent/workspace_clear.go` to `internal/agent`, so both agents share one helper. The host runs it as root in the pause container.
10. **Apple `container`** does not implement `crt.PodRuntime`. It cannot back a claim pod, and its scope containers are not swept. The docs say so.
11. **Graceful shutdown.** `Agent.Run` removes the idle pooled pods (`pool.Close`) after the slots stop.

## Review Focus

1. **Cross-run leakage through a reused container.** A pooled pod must give the next run an empty workspace, no exec process left over from the previous run, and no previous env. It is pinned by:
   - `TestPool_HitEmptiesWorkspaceViaPauseExec`;
   - `TestPool_InterruptedExecDiscardsPod`;
   - `TestClaimPod_CreateSpecsCarryNoRunSpecificValues`.

   The first two are in Task 5 and the last is in Task 4.
2. **Stale bind mount.** A workspace directory that is removed and recreated leaves the pooled container writing to a deleted inode. Its outputs and artifacts then vanish without an error. Pinned by `TestPrepareWorkspace_KeepDirPreservesInode` (Task 4) and `TestGCWorkspaces_SkipsPooledWorkDirs` (Task 5).
3. **Sidecar log replay.** A reused sidecar's earlier output would leak into the next run's log, masked with the wrong run's secrets. Pinned by `TestSidecarPump_ReusedPodStreamsSinceClaimStart` (Task 4).
4. **The sweep removes containers it does not own.** That includes another agent's containers on a shared runtime, and unlabelled containers. Pinned by `TestStartupSweep_RemovesOnlyOwnLabelledContainers` (Task 1).
5. **Timeout and cancel confused with each other.** A cancel during pull must not produce a Failed `FinishRun`, and a timeout must not be swallowed as a cancel. Pinned by:
   - `TestExecuteRun_CancelDuringPull_AbandonsWithoutFinishRun` (Task 3);
   - `TestExecuteRun_PullTimeout_FailsWithExactMessage` (Task 2).

---

### Task 1: Labels and the startup crash sweep

**Files:**
- Modify:
  - `internal/runtime/runtime.go` (`CreateSpec` ~L28, a new optional interface after `ImageInspector` ~L77)
  - `internal/runtime/ocicli.go` (`createArgs` ~L114, new methods after `ExitCode` ~L268)
  - `internal/agent/claim_pod.go` (`claimPodManager` fields ~L295, both `rt.Create` calls ~L389 and ~L427)
  - `internal/agent/scope.go` (`scopeManager` ~L31, `ensure` ~L94)
  - `internal/agent/backend_host.go` (`getScopes` ~L135)
  - `internal/agent/agent.go` (`Run`, after `reapStagingDirs` ~L542)
  - New: `internal/agent/sweep.go`
- Test:
  - `internal/runtime/ocicli_test.go`
  - `internal/agent/claim_pod_test.go` (extend `podFakeRT`)
  - `internal/agent/scope_test.go`
  - New: `internal/agent/sweep_test.go`
- Docs:
  - `docs/operator-manual/agents.md` § "Crash-orphaned claim containers" (~L846)

**Interfaces:**
- Produces:
  - `crt.CreateSpec.Labels map[string]string`. ociCLI emits `--label k=v` in sorted key order. Apple ignores it.
  - The optional interface, for ociCLI only:
    ```go
    type PodRuntime interface {
        ImageID(ctx context.Context, image string) (id string, present bool, err error)
        ContainerRunning(ctx context.Context, h ContainerHandle) (bool, error)
        ListContainers(ctx context.Context, labels map[string]string) ([]ContainerInfo, error)
        LogsSince(ctx context.Context, h ContainerHandle, since time.Time, stdout, stderr io.Writer) error
    }
    type ContainerInfo struct { Handle ContainerHandle; Labels map[string]string }
    ```
    How ociCLI implements each method:
    - `ImageID`: `image inspect --format {{.Id}}`. A non-zero exit means `present=false` with a nil error.
    - `ContainerRunning`: `inspect -f {{.State.Running}}`.
    - `ListContainers`: `ps -a --no-trunc --filter label=k=v ... --format {{.ID}}\t{{json .Labels}}`. The builder verifies that podman and nerdctl accept this format. If they do not, use a per-ID `inspect -f {{json .Config.Labels}}`.
    - `LogsSince`: `logs -f --since <RFC3339Nano>`.
  - Label constants in `internal/agent/sweep.go`:
    - `labelAgentID = "unified-cd/agent-id"`
    - `labelClaimPod = "unified-cd/claim-pod"`
    - `labelScope = "unified-cd/scope"`
  - `func claimPodLabels(agentID string) map[string]string` and `func scopeLabels(agentID string) map[string]string`. Each returns nil when `agentID == ""`.
  - `func sweepOrphanContainers(ctx context.Context, rt crt.ContainerRuntime, agentID string) (removed []string)`:
    - It is a no-op if `agentID == ""` or the runtime does not implement `crt.PodRuntime`. In that case it logs one Info line.
    - Otherwise it lists with filter `{labelAgentID: agentID}`. It removes only the containers whose returned labels also carry `agent-id == agentID` (re-checked) and either `claim-pod=true` or `scope=true`.
  - `claimPodManager.agentID string` and `scopeManager.agentID string`, set by `executeRun` and `getScopes` from `a.ID`.
- Consumes: `Agent.ID` and `Agent.containerRuntime()`.

- [ ] **Step 1: Write failing tests.**
  - `TestCreateArgs_Labels` (ocicli): `Labels{"unified-cd/scope":"true","unified-cd/agent-id":"a1"}` yields `--label unified-cd/agent-id=a1 --label unified-cd/scope=true` before the image.
  - `TestOCI_PodRuntimeArgs`: uses the existing `execCommand` seam to assert the argv of `ImageID`, `ContainerRunning`, `ListContainers` and `LogsSince`. For `ImageID`, a non-zero exit gives `present=false, err=nil`.
  - `TestClaimPod_LabelsOnEveryContainer`: with `m.agentID="a1"`, all 3 `CreateSpec`s (pause, mysql, job) carry exactly `{"unified-cd/agent-id":"a1","unified-cd/claim-pod":"true"}`.
  - `TestScope_LabelsOnScopeContainer`: carries `{"unified-cd/agent-id":"a1","unified-cd/scope":"true"}`.
  - `TestStartupSweep_RemovesOnlyOwnLabelledContainers`: the fake runtime holds containers for `a1` claim-pod, `a1` scope, `a2` claim-pod, `a1` with no marker, and one unlabelled container. Only the first two are removed. `removed == []string{<a1-claim>, <a1-scope>}`.
  - `TestStartupSweep_NoPodRuntimeIsNoop`: a runtime that lacks `PodRuntime` causes no `Remove` call and no panic.
  - `TestStartupSweep_EmptyAgentIDIsNoop`.
- [ ] **Step 2: Run the tests and confirm they fail.**

  ```
  go test -race ./internal/runtime/ ./internal/agent/ -run 'Labels|PodRuntimeArgs|StartupSweep'
  ```
- [ ] **Step 3: Implement.**
  - Add the label constants and helpers, the `PodRuntime` methods on ociCLI, and `podFakeRT` support. `podFakeRT` keeps `containers map[id]ContainerInfo`, `running map[id]bool` and `imageIDs map[string]string`, so later tasks can reuse it.
  - Set the labels in both `Create` sites and in `scope.go`.
  - Call `sweepOrphanContainers` in `Agent.Run` after `reapStagingDirs`, before any slot starts. Log `removed orphaned containers from a previous agent process` with `count`.
- [ ] **Step 4: Run the tests and confirm they pass.** Run the same command, then `go build ./... && go vet ./internal/agent/ ./internal/runtime/`.
- [ ] **Step 5: Update the docs.**
  - Rewrite "Crash-orphaned claim containers": the startup sweep, the labels, and why one agent ID per process is required.
  - Explain that containers from a pre-upgrade agent are unlabelled and need a one-time manual removal. Show `docker ps -a --filter ancestor=<pauseImage>` (and the podman equivalent), and `docker ps -a --filter label=unified-cd/agent-id=<id>` for inspection.
  - Note that Apple `container` scope containers are not swept.
- [ ] **Step 6: Commit** `feat(agent): label host claim/scope containers and sweep own orphans at startup (TODO 45)`.

### Task 2: Pull step, pull and start timeouts, `:latest` re-pull

**Files:**
- Modify:
  - `internal/imageref/imageref.go`
  - `internal/config/agent.go` (struct ~L139, env block ~L250, file merge ~L358)
  - `cmd/unified-cd-agent/main.go` (flags ~L119, wiring ~L305)
  - `internal/agent/agent.go` (`Agent` fields near `PauseImage`; `executeRun` isolated branch ~L1040-1098)
  - New: `internal/agent/claim_start.go`
- Test:
  - `internal/imageref/imageref_test.go`
  - `internal/config/config_test.go`
  - New: `internal/agent/claim_start_test.go`
- Docs:
  - `docs/reference/configuration.md` (host flag table and sample ~L240-420, plus a note next to the k8s `podStartTimeout` row ~L657)

**Interfaces:**
- Produces:
  - `func imageref.AlwaysPull(ref string) bool`. It is true when the ref has no digest and its tag is empty or `latest`. The tag is the part after the last `:` that follows the last `/`.
  - `config.AgentFile`/`Effective` fields:
    - `PodPullTimeout time.Duration` (`yaml:"podPullTimeout"`)
    - `PodStartTimeout time.Duration` (`yaml:"podStartTimeout"`)
  - `Agent` fields `PodPullTimeout` and `PodStartTimeout`, plus accessors `podPullTimeout()` and `podStartTimeout()` that return the default for values `<= 0`.
  - `const defaultPodPullTimeout = 30 * time.Minute` and `const defaultPodStartTimeout = 5 * time.Minute`.
  - `func claimImages(defs []containerDef, pauseImage string) []string`: pause first, then the container images in order, deduplicated.
  - `func pullClaimImages(ctx context.Context, rt crt.ContainerRuntime, images []string) (ids map[string]string, err error)`:
    - It pulls an image when `AlwaysPull(ref)` is true, or when `ImageID` reports it is not present.
    - With `PodRuntime` it returns the post-pull IDs. Without it, it pulls only the `AlwaysPull` refs and returns a nil map.
  - `var errPullTimeout`, `var errStartTimeout` (sentinels for `context.WithTimeoutCause`), and `func pullTimeoutMsg(d time.Duration) string` / `func startTimeoutMsg(d time.Duration) string`.
- Consumes: `claimContainerDefs`, `crt.PodRuntime` (Task 1).

- [ ] **Step 1: Write failing tests.**
  - `TestAlwaysPull`:
    - true for `alpine`, `alpine:latest`, `reg:5000/ns/alpine` and `reg:5000/ns/alpine:latest`;
    - false for `alpine:3.20`, `reg:5000/alpine:3.20` and `alpine@sha256:…`;
    - false for `busybox:1.36@sha256:…`.
  - `TestTimeoutMessages`: `pullTimeoutMsg(30*time.Minute) == "claim pod image pull exceeded podPullTimeout (30m0s)"` and `startTimeoutMsg(5*time.Minute) == "claim pod start exceeded podStartTimeout (5m0s)"`.
  - `TestPullClaimImages_LatestAlwaysPulledOthersOnlyWhenMissing`: `mysql:8` (present) and `pause@sha256:…` (present) are not pulled, while `runner:latest` (present) is pulled. Each image is pulled once even when two containers name it.
  - `TestPullClaimImages_PullFailureFailsNoStaleFallback`: `Pull("app:latest")` errors, so the function returns the error even though `ImageID` reports it present.
  - `TestExecuteRun_PullTimeout_FailsWithExactMessage`:
    - Set `a.PodPullTimeout = 50*time.Millisecond`, with a fake `Pull` that blocks until ctx is done.
    - The run finishes `Failed`. The step -1 log contains exactly `claim pod image pull exceeded podPullTimeout (50ms)`.
    - No container is created.
  - `TestExecuteRun_StartTimeout_FailsWithExactMessage`: with `PodStartTimeout = 50ms` and the second `Create` blocking, the message is exactly `claim pod start exceeded podStartTimeout (50ms)`, and every created container is removed.
  - `TestExecuteRun_PullPrecedesCreate`: the fake records the call order, and every `Pull` comes before the first `Create`.
  - `TestAgentEffective_PodTimeoutsDefaultsEnvFile`: unset gives 0 (the agent applies the defaults). Env `UNIFIED_AGENT_POD_PULL_TIMEOUT=10m` gives 10m. A file value overrides env.
- [ ] **Step 2: Run the tests and confirm they fail.**

  ```
  go test -race ./internal/imageref/ ./internal/config/ ./internal/agent/ -run 'AlwaysPull|TimeoutMessages|PullClaimImages|PullTimeout|StartTimeout|PullPrecedesCreate|PodTimeouts'
  ```
- [ ] **Step 3: Implement.**
  - In `executeRun`, resolve `defs` once, then:
    1. Pull under `pullCtx := context.WithTimeoutCause(ctx, a.podPullTimeout(), errPullTimeout)`. If `context.Cause(pullCtx) == errPullTimeout`, call `a.failRun(ctx, runID, pullTimeoutMsg(d))`.
    2. Run `pod.Start` under `context.WithTimeoutCause(ctx, a.podStartTimeout(), errStartTimeout)`, with the same check and `startTimeoutMsg`.
  - Other start errors keep the current `claim pod construction failed: …` text.
  - Wire the flags, env and file values in config and main.
- [ ] **Step 4: Run the tests and confirm they pass.** Run the same command, then `go test -race ./internal/agent/ -run 'Isolated|ClaimPod'` to confirm no regression.
- [ ] **Step 5: Update `configuration.md`.**
  - Add the host rows and sample lines for `podPullTimeout`/`podStartTimeout` with their defaults and env vars.
  - Explain that the host measures the pull separately, while Kubernetes counts the pull inside its single `podStartTimeout`. Add the same sentence to the k8s `podStartTimeout` row.
  - Explain that `:latest` and untagged images are always pulled, with no stale fallback.
- [ ] **Step 6: Commit** `feat(agent): separate claim-pod pull step with podPullTimeout/podStartTimeout and :latest re-pull (TODO 45)`.

### Task 3: Cancel during pull or start

**Files:**
- Modify:
  - `internal/agent/claim_start.go`
  - `internal/agent/agent.go` (`executeRun` isolated branch)
- Test: `internal/agent/claim_start_test.go`
- Docs: `docs/operator-manual/agents.md` (one sentence beside the timeouts: a cancel during pull/start abandons the claim)

**Interfaces:**
- Produces:
  ```go
  func startClaimCancelWatch(ctx context.Context, client *Client, runID string, interval time.Duration) (wctx context.Context, terminal func() bool, stop func())
  ```
  - It polls `client.GetRun` every `interval` (the caller passes the `CancelPollInterval` snapshot).
  - On any terminal status (`isTerminalRunStatus`), it records that the run is terminal and cancels `wctx`.
  - `stop` cancels the watcher and joins its goroutine. This is the host counterpart of `startRunCancelWatch`.
- Consumes: `isTerminalRunStatus` (`orchestrator.go`) and the Task 2 pull and start contexts, which now derive from `wctx`.

- [ ] **Step 1: Write failing tests.** The harness's `GET /api/v1/runs/{id}` switches to `Cancelled` once the fake's blocking `Pull` or `Create` is entered. Set `CancelPollInterval = 10ms` in the test, and restore it in Cleanup.
  - `TestExecuteRun_CancelDuringPull_AbandonsWithoutFinishRun`: `finishCalls == 0`, no `Create`, and no step -1 failure line.
  - `TestExecuteRun_CancelDuringStart_RemovesPartialsWithoutFinishRun`: the second `Create` blocks, then cancel. `finishCalls == 0`, and `removed` holds every created ID (pause included).
  - The "no pooled partial pod" half of the spec test is `TestExecuteRun_CancelDuringStart_NeverPoolsPartialPod`, written in Task 5 because the pool does not exist yet.
  - `TestStartClaimCancelWatch_StopJoins`: after `stop()`, no further `GetRun` request arrives (the request counter is stable for 3 intervals).
- [ ] **Step 2: Run the tests and confirm they fail.**

  ```
  go test -race ./internal/agent/ -run 'CancelDuring|StartClaimCancelWatch'
  ```
- [ ] **Step 3: Implement.**
  - Start the watcher at the top of the isolated branch, before the pull. On any pull or start error, check `terminal()` first. If it is true:
    1. call `pod.CloseAll(context.WithoutCancel(ctx))`;
    2. log `slog.Info("run became terminal during claim pod start; abandoning", "runId", …)`;
    3. return without `failRun`.
  - Call `stop()` before `runClaimFn`, because RunClaim's own poller takes over.
- [ ] **Step 4: Run the tests and confirm they pass.** Run the same command, then `go test -race ./internal/agent/`.
- [ ] **Step 5: Update the docs** (the sentence above).
- [ ] **Step 6: Commit** `feat(agent): cancel a host claim during image pull or pod start (TODO 45)`.

### Task 4: Make a claim pod safe to reuse (create-time audit, workspace and sidecar logs)

**Files:**
- Modify:
  - `internal/agent/workspace.go` (`prepareWorkspace` ~L128, `finishWorkspace` ~L218)
  - `internal/agent/agent.go` (`runLoop` ~L880-883: compute `keepDir`; `executeRun` → `executeRunWith`)
  - `internal/agent/sidecar_logs.go` (`newSidecarLogPump` ~L38, `stream` ~L78)
  - `internal/agent/backend_host.go` (`SetMasker` ~L580)
  - `internal/k8sagent/workspace_clear.go` (delegate)
  - New: `internal/agent/workspace_clear.go` (moved `ClearWorkspaceScript`, with its test moved too)
- Test:
  - `internal/agent/workspace_test.go` (or the existing prepareWorkspace test file)
  - `internal/agent/sidecar_logs_test.go`
  - `internal/agent/claim_pod_test.go`
  - `internal/k8sagent/workspace_clear_test.go` (keep passing via the delegate)

**Interfaces:**
- Produces:
  - `prepareWorkspace(ctx, workDir, mode string, vol *volumeWorkspace, clean, keepDir bool, rtFn, toolsDir string) error` and `finishWorkspace(ctx, workDir string, vol *volumeWorkspace, keepDir bool, rtFn, toolsDir string)`. With `keepDir`, the ordinary workspace is emptied with `wipeDir(…, contentsOnly=true, …)`, and the directory itself is never removed. Update the `prepareWorkspaceFn` stubs in the tests.
  - `type claimOpts struct { Poolable bool }` and `func (a *Agent) executeRunWith(ctx context.Context, c api.ClaimResponse, workDir string, o claimOpts)`. `executeRun(ctx, c, workDir)` stays as a wrapper with `claimOpts{}`, so the existing tests are untouched. `runLoop` passes `Poolable: !detached`.
  - `func claimPoolable(c api.ClaimResponse, o claimOpts) bool`. It returns `o.Poolable && !c.Native && c.PodTemplate != nil && c.PodTemplate.Reuse && len(c.Volumes) == 0`, and `runLoop` uses it for `keepDir`.
  - `func ClearWorkspaceScript(root string, vols []MountedVolume) string`. The body moves verbatim, and k8sagent's `clearWorkspaceScript` calls it.
  - `newSidecarLogPump(rt, client, agentID, runID, masker, sidecars, since time.Time)`. With `!since.IsZero()` and `rt` implementing `crt.PodRuntime`, it streams via `LogsSince`. Otherwise it uses `Logs`. `hostBackend.claimSince time.Time` is set by `executeRunWith` (zero for a fresh pod).
- Consumes: `crt.PodRuntime.LogsSince` (Task 1).
- **Create-time audit (record it in the `claimPodManager.Start` doc comment):**
  - Every `CreateSpec` input is job-stable and goes into the Task 5 fingerprint:
    - Image, Env (literal podTemplate values), CPU/Mem limits;
    - WorkDir (mountPath) and Mounts (workDir, toolsDir; `volumeMounts` are empty when poolable);
    - User (from image user + security), GroupAdd, Cap*, NoNewPrivileges, ReadOnlyRootfs, Entrypoint/Args;
    - the pause image.
  - Run-specific values are already delivered at Exec time: step env, secrets and credentials (`ExecSpec.Env`).
  - `runLog` and the fsGroup chown are per start, and Task 5 re-applies them on a hit.
  - Nothing else needs moving. The test below locks this in.

- [ ] **Step 1: Write failing tests.**
  - `TestClaimPod_CreateSpecsCarryNoRunSpecificValues`:
    - Drive `executeRun` with `RunID: "run-7f3a9c"`, a step with secret env `TOKEN=s3cr3t-value` and `c.Params`/`c.Vars` values.
    - JSON-encode every recorded `CreateSpec`. None contains `run-7f3a9c`, `s3cr3t-value` or `TOKEN`.
    - The step's `ExecSpec.Env` does contain `TOKEN=s3cr3t-value`.
  - `TestPrepareWorkspace_KeepDirPreservesInode`:
    - Create workDir with a file and a dot-file, and record `os.Stat` (`os.SameFile`).
    - After `prepareWorkspace(…, keepDir=true, …)`, the dir is empty, `os.SameFile(before, after)` is true, and the mode is `0777` for isolated (skipped on Windows).
    - The same holds for `finishWorkspace`.
    - With `keepDir=false`, the existing behaviour (the dir is recreated) is unchanged.
  - `TestRunLoop_KeepDirOnlyForPoolableClaims`: covers `claimPoolable` in a table:
    - reuse+isolated → true;
    - native → false;
    - detached (`Poolable:false`) → false;
    - with volumes → false;
    - reuse false → false.
  - `TestSidecarPump_ReusedPodStreamsSinceClaimStart`: with `since = t0`, the fake records `LogsSince(h, t0)` and no `Logs` call. With a zero `since`, it records `Logs` only.
  - `TestClearWorkspaceScript_MovedVerbatim`: for root `/workspace` with no vols, the script is `rm -rf '/workspace'/* '/workspace'/.[!.]* '/workspace'/..?*`. The existing k8sagent tests keep passing.
- [ ] **Step 2: Run the tests and confirm they fail.**

  ```
  go test -race ./internal/agent/ ./internal/k8sagent/ -run 'NoRunSpecificValues|KeepDir|SinceClaimStart|ClearWorkspaceScript'
  ```
- [ ] **Step 3: Implement** the signatures above, with no behaviour change for non-poolable claims.
- [ ] **Step 4: Run the tests and confirm they pass.** Run the same command, then `go test -race ./internal/agent/ ./internal/k8sagent/`.
- [ ] **Step 5: Docs.** None in this task (no user-visible change yet). Task 5 documents the combined behaviour.
- [ ] **Step 6: Commit** `refactor(agent): prepare host claim pods for reuse (keep workspace dir, sidecar logs since claim start, shared clear script) (TODO 45)`.

### Task 5: Host pod pool

**Files:**
- New:
  - `internal/agent/pool_host.go`
  - `internal/agent/pool_host_test.go`
- Modify:
  - `internal/agent/agent.go`:
    - `Agent` fields: `PoolIdleTimeout time.Duration`, `PoolMaxIdle int`, `hostPool *hostPodPool`.
    - `Run`: create the pool, start eviction, and `pool.Close` after `wg.Wait()`.
    - `executeRunWith`: acquire and start.
    - The GC call sites ~L616 and in `workspaceGCLoop`: union the pool's workDirs into the active set.
  - `internal/agent/backend_host.go`:
    - `CloseScopes` ~L211: release instead of `CloseAll`.
    - `RunDefault`/`RunNamedContainer`/`RunPostHook`: set `b.execInterrupted` when `ctx.Err() != nil` after an exec.
  - `internal/config/agent.go` and `cmd/unified-cd-agent/main.go`:
    - `PoolIdleTimeout *time.Duration` (`yaml:"poolIdleTimeout"`), where nil means 30m;
    - `PoolMaxIdle *int` (`yaml:"poolMaxIdle"`), where nil means 4;
    - the flags and env from the Global Constraints.
- Docs:
  - `docs/operator-manual/agents.md` (new section "Claim-pod pool (`podTemplate.reuse`)")
  - `docs/reference/configuration.md` (host `poolIdleTimeout`/`poolMaxIdle`, plus a note on the k8s `poolIdleTimeout` row that the defaults differ)
  - `docs/user-guide/writing-jobs/isolation-and-containers.md` (~L339, ~L451)
  - `docs/user-guide/resources/job.md` (~L59)

**Interfaces:**
- Produces:
  ```go
  type hostPodFingerprintInput struct {
      JobName, WorkDir, MountPath           string
      Defs                                  []containerDef      // claimContainerDefs output, incl. Security
      PodSecurity                           dsl.EffectiveSecurity
      ImageIDs                              map[string]string   // from pullClaimImages
      PauseImage, RunnerImage, ToolsDir     string
      MaxMappedID                           int64               // security-context probe result
  }
  func hostPodFingerprint(in hostPodFingerprintInput) (string, error) // sha256 hex of json.Marshal(in)
  type hostPodPool struct { /* mu; idle map[string]*pooledHostPod keyed by workDir; idleTimeout; maxIdle; rt; now func() time.Time */ }
  type pooledHostPod struct { pod *claimPodManager; jobName, fingerprint string; idleSince time.Time }
  func newHostPodPool(rt crt.ContainerRuntime, idleTimeout time.Duration, maxIdle int) *hostPodPool
  func (p *hostPodPool) Acquire(ctx context.Context, workDir, jobName, fp string) (*claimPodManager, bool)
  func (p *hostPodPool) Release(ctx context.Context, workDir, jobName, fp string, m *claimPodManager, healthy bool)
  func (p *hostPodPool) StartEviction(ctx context.Context)
  func (p *hostPodPool) WorkDirs() []string
  func (p *hostPodPool) Len() int
  func (p *hostPodPool) Close(ctx context.Context)
  func podRunning(ctx context.Context, rt crt.PodRuntime, m *claimPodManager) bool
  func (m *claimPodManager) prepareReuse(ctx context.Context, pt *dsl.PodTemplate) error // ClearWorkspaceScript via pause exec, then re-apply fsGroup
  ```
  How these methods behave:
  - `Acquire` removes the entry for `workDir`. It returns a hit only when the entry's `jobName` and `fp` match and `podRunning` is true. In every other case it removes the old pod's containers (`CloseAll`) and returns a miss.
  - `Release` with `!healthy` removes the pod. Otherwise it stores the pod with `idleSince = now`. If the pool now holds more than `maxIdle` entries, it removes the oldest.
  - `StartEviction` ticks at `idleTimeout/2`, with a minimum of 30s. It is a no-op when `idleTimeout == 0`.
- Consumes:
  - `pullClaimImages` IDs (Task 2), the watcher and timeouts (Tasks 2-3);
  - `claimPoolable`, `ClearWorkspaceScript` and `claimSince` (Task 4);
  - `crt.PodRuntime` (Task 1).
- `executeRunWith` flow for a poolable claim:
  1. Pull and compute `fp`.
  2. `Acquire`.
  3. On a hit:
     - Set `runLog` and run `prepareReuse` under the start timeout.
     - If that fails, call `CloseAll`, log Info `reused claim pod could not be prepared; creating a fresh one`, and fall through to `Start`.
     - Set `claimSince = time.Now()` before `prepareReuse`.
  4. On a miss, `Start`.
  5. Record `backend.poolKey = {workDir, jobName, fp}`.
  6. `CloseScopes` calls `Release(…, healthy = !b.execInterrupted && podRunning(…))`.

  A start-phase failure or cancel never reaches `Release`.

- [ ] **Step 1: Write failing tests.** Unit tests use `podFakeRT` with the `PodRuntime` state, and an injected `now`. Agent-level tests run two claims through `executeRunWith(…, claimOpts{Poolable:true})` with the same workDir.
  - `TestPool_ReuseHit`: second run → `len(f.created) == 3` in total (no new creates), and the second run's step exec targets the same `job` handle `c2`.
  - `TestPool_MissOnDifferentPodDefinition`: the second run's podTemplate env differs (`A=1` → `A=2`), so there are 3 new creates and the old 3 IDs are in `removed`.
  - `TestPool_MissOnDifferentJobOrSlot`: a different workDir, or a different jobName on the same workDir, misses.
  - `TestPool_HitEmptiesWorkspaceViaPauseExec`: on a hit, the first exec after acquire targets the pause handle `c0` with the script `ClearWorkspaceScript(mountPath, nil)`. It precedes the step's exec.
  - `TestPool_ClearFailureFallsBackToFresh`: the pause exec returns exit 1, so the old IDs are removed, 3 new containers are created, and the run is `Succeeded`.
  - `TestPool_UnhealthyPooledPodDiscarded`: `running["c1"] = false` (the sidecar exited) before the second run → miss, and the old IDs are removed.
  - `TestPool_DigestChangeDiscards`: `imageIDs["runner:latest"]` changes between runs → the pull happens, then a miss, and the old pod is removed.
  - `TestPool_IdleEviction`: `idleTimeout = 30m`. `evictExpired` at `now+29m` keeps the pod and at `now+30m` removes it. With `idleTimeout = 0`, `StartEviction` is a no-op.
  - `TestPool_MaxIdleEvictsOldest`: with `maxIdle = 2` and three releases at t0, t1 and t2, the t0 pod is removed and `Len() == 2`.
  - `TestPool_InterruptedExecDiscardsPod`: the run is cancelled mid-step (the fake `Exec` blocks until ctx is done), so the pod is removed at `CloseScopes` and `Len() == 0`.
  - `TestPool_FailedStepStillPooled`: the step exits 1 → `Failed`, and `Len() == 1`.
  - `TestPool_NotPooledReasonsLogged`: detached, a runtime without `PodRuntime`, and `PoolMaxIdle == 0` each log one Info line `podTemplate.reuse: claim pod not pooled` with a `reason` attribute, and the containers are removed at the end.
  - `TestPool_ReuseWithVolumesFailsRun`: the exact message from ruling 6.
  - `TestGCWorkspaces_SkipsPooledWorkDirs`: an aged workDir held by the pool survives `gcWorkspaces` with the union active set.
  - `TestPool_CloseRemovesIdle`.
  - `TestExecuteRun_CancelDuringStart_NeverPoolsPartialPod`: with `reuse: true`, the second `Create` blocks and the run is cancelled. Then `finishCalls == 0`, `Len() == 0`, and every created ID is in `removed`.
  - `TestAgentEffective_PoolSettings`: nil gives 30m/4, an explicit `0` is kept as 0, and env and file precedence are honoured.
- [ ] **Step 2: Run the tests and confirm they fail.**

  ```
  go test -race ./internal/agent/ ./internal/config/ -run 'Pool|SkipsPooledWorkDirs|NeverPoolsPartialPod'
  ```
- [ ] **Step 3: Implement** the pool, the fingerprint, `prepareReuse`, and the wiring above.
- [ ] **Step 4: Run the tests and confirm they pass.** Run the same command, then `go test -race ./internal/agent/ ./internal/config/ ./internal/k8sagent/ ./cmd/...` and `go vet ./...`.
- [ ] **Step 5: Update the docs.**
  - `agents.md` pool section:
    - what is kept (the container filesystem, background processes, sidecar state);
    - that the workspace is emptied;
    - the pool scope (same job, same slot), and that hit rates are best with few slots;
    - the fingerprint inputs;
    - the health rules, including that a cancelled run discards its pod;
    - that pooled pods are not restored after a restart (the sweep removes them);
    - the shutdown removal;
    - that `poolIdleTimeout` defaults differ from Kubernetes.
  - The user-guide `reuse` rows: drop "k8s only / the host agent ignores this" and say that both agents pool.
  - `configuration.md`: the rows and sample lines.
- [ ] **Step 6: Commit** `feat(agent): pool host claim pods for podTemplate.reuse (TODO 45)`.

### Task 6: Docs sweep and TODO

**Files:**
- `docs/operator-manual/agents.md`
- `docs/user-guide/writing-jobs/volumes.md` (~L172, "a pooled Kubernetes Pod")
- `docs/user-guide/writing-jobs/isolation-and-containers.md` (~L482-534)
- `docs/user-guide/resources/job.md`
- `docs/reference/configuration.md`
- `TODO.md` (remove item 45, ~L12-17)

- [ ] **Step 1: Find every stale statement.**

  ```
  grep -rn -i "host agent ignores\|k8s only.*reuse\|reuse.*k8s only\|no automatic container GC\|possible future work\|Kubernetes agent only: the host" docs/ README.md examples/ charts/
  grep -rn "podStartTimeout\|poolIdleTimeout" docs/
  ```

  Fix each hit so that it states the current host behaviour.
- [ ] **Step 2: Add a slow-creation note to `agents.md`.** Container creation is slow on the `vfs` storage driver and on fuse-overlayfs (rootless). `reuse: true` amortises this cost, and changing the driver is out of scope.
- [ ] **Step 3: Remove item 45 from `TODO.md`.** Check that no other TODO item references it (`grep -n "45" TODO.md`).
- [ ] **Step 4: Verify.**
  - `go build ./... && go test ./internal/agent/ ./internal/runtime/ ./internal/config/ ./internal/imageref/ ./internal/k8sagent/`
  - If `mkdocs.yml` validation exists in CI, run `mkdocs build --strict`.
- [ ] **Step 5: Commit** `docs: host claim-pod lifecycle parity (TODO 45)`.

---

## Self-review against the spec

The spec's goals and required tests map to tasks as follows:

| Spec item | Where it is covered |
|---|---|
| Goal 1: reuse hit/miss, workspace emptied, other state kept, `reuse`+volumes rejected | Task 5 (ruling 6 is the runtime backstop; the apply check in `dsl/volumes.go:125` is unchanged) |
| Goal 2: separate timeouts with exact strings, flag+config, docs on the k8s difference | Task 2 |
| Goal 2: cancel during pull/start, partial pods removed, no second `FinishRun` | Task 3 |
| Goal 2: `:latest` and untagged images re-pulled | Task 2 |
| Goal 3: labels and the startup sweep, scoped by agent ID | Task 1 |
| Goal 4: TODO removed and docs updated | Tasks 1, 2, 5 and 6 |

Designer's calls:

| Call | Where it is covered |
|---|---|
| 1. Pool scope: same job and slot (keyed by `working<slot>/<job>`) | Task 5 |
| 2. Create-time input audit, Info line when a claim is not pooled | Tasks 4 and 5 |
| 3. Fingerprint inputs, including the image-ID runtime call and fake | Tasks 1 and 5 |
| 4. Health = all containers running | Task 5 (ruling 4 adds interrupted execs) |
| 5. Emptying via exec, fallback to fresh | Task 5 |
| 6. Labels, sweep, pre-upgrade guidance | Task 1 |
| 7. Host idle default 30m, docs on differing defaults | Task 5 |
| 8. Pull step, once per claim, no stale fallback, digest discard | Tasks 2 and 5 |

Every test in "Left to the builder" has a named test:

| Required test | Named test |
|---|---|
| Hit and miss | `ReuseHit`, `MissOnDifferent*` |
| Workspace emptied | `HitEmptiesWorkspaceViaPauseExec` |
| Different definition not reused | `MissOnDifferentPodDefinition` |
| Unhealthy pod discarded | `UnhealthyPooledPodDiscarded` |
| Idle eviction and `poolMaxIdle` | `IdleEviction`, `MaxIdleEvictsOldest` |
| Timeouts with exact messages | `PullTimeout_*`, `StartTimeout_*` |
| Cancel | `CancelDuring*` |
| `:latest` re-pull and digest discard | `PullClaimImages_LatestAlways…`, `DigestChangeDiscards` |
| Sweep removes only its own containers | `StartupSweep_RemovesOnlyOwnLabelledContainers` |

Non-goals respected: no pre-pull and no storage-driver change (only a note). Pods are never shared across agents, because the sweep and the pool are per agent ID and per process.
