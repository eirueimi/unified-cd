# k8s Agent Resilience (G-A1 / G-A2 / G-A3) — Design

**Status:** Approved in principle (2026-07-15); this document is the written spec.

**Goal:** Close three resilience gaps in the k8s agent so that a stuck/unschedulable run pod cannot wedge a run forever, a run reaped by the controller stops executing side-effectful steps, and agent shutdown/overload behaves like the host agent (graceful drain, bounded concurrency, retried failure reporting).

**Scope:** `internal/k8sagent/*`, `internal/agent/orchestrator.go`, `cmd/k8s-agent/main.go`. No controller or DSL changes. No new database schema.

**Guiding principle:** The **host agent already implements** the drain, bounded-concurrency, and retried-fail-report patterns (`internal/agent/agent.go`, `internal/agent/shutdown.go`, `cmd/agent/main.go`). Wherever possible we **port the host's proven semantics to k8s** rather than invent new ones, and reuse existing shared helpers (`agentlib.RetryUntilSuccess`, `isTerminalRunStatus`).

---

## Background: current behavior

`K8sAgent.Run` (`internal/k8sagent/agent.go:71-137`) is a claim loop that, for every claimed run, spawns `go a.executeRun(ctx, resp)` (`agent.go:135`) — **unbounded**, on a **single** agent-root `ctx` shared by claiming, execution, and heartbeat. `executeRun` (`agent.go:147-258`) acquires a pod (pooled or fresh), waits for it to be Running (`agent.go:218`), then hands off to the shared orchestrator `agentlib.RunClaim` (`internal/agent/orchestrator.go`) which runs the steps and reports results. The cancel poller lives inside `RunClaim` (`orchestrator.go:107-130`).

The host agent (`internal/agent/agent.go:146-265`) does the same job but with a `claimCtx`/`runCtx` split, a `DrainTimeout`, an in-flight `WaitGroup`, N bounded slot goroutines honoring `MaxConcurrent`, and a retried `failRun`. The k8s agent has none of these.

---

## G-A1 — Bound the run-pod wait; make it cancel-aware

### Problem
`PodManager.WaitForPodRunning` (`internal/k8sagent/podmanager.go:96-115`) polls the pod phase in an **infinite** loop, exiting only on `ctx.Done()`. Run pods use `RestartPolicy: Never`, so a `Pending` / `ImagePullBackOff` / unschedulable pod **never** transitions to `Failed` — the loop spins forever. The call site (`agent.go:218`) passes the bare agent-root `ctx` with **no timeout** (unlike the scope-pod path `internal/k8sagent/backend.go:153-160`, which already wraps the wait in `imagePodStartTimeout`). Because the cancel poller only starts later inside `RunClaim` (`orchestrator.go:107`), a user cancel cannot break the wait either. Net: an unschedulable run is stuck `Running` forever, uncancellable and only cleared by the controller's stuck-run reaper.

### Design
1. **New config field `PodStartTimeout`** on `k8sagent.Config` (`config.go`), yaml key `podStartTimeout`, overridable by env `UNIFIED_K8S_POD_START_TIMEOUT` (Go `time.Duration` string, e.g. `5m`). Default **5m** (matches the existing `imagePodStartTimeout` const). `Validate` clamps non-positive to the 5m default.
2. **Bound the wait.** At `agent.go:218`, wrap the wait context in `context.WithTimeout(ctx, cfg.PodStartTimeout)`. On timeout, `WaitForPodRunning` returns via `ctx.Done()` with a clear error (`pod did not become ready within <d>`).
3. **Make the wait cancel-aware.** Run a lightweight watcher goroutine for the duration of the wait that polls the controller (`client.GetRun`) every `CancelPollInterval` (existing 5s default) and cancels the wait context if the run has reached a **terminal** controller status (`isTerminalRunStatus`: Cancelled/Failed/Succeeded). This lets a user cancel — or a controller reap — break the wait before the pod is ready, instead of waiting out the full timeout. The watcher is cancelled as soon as the wait returns.
4. **Clean failure.** When the wait fails for any reason (timeout, terminal status, or pod error), `executeRun` must: (a) delete the pod if we created a fresh one, or release/clean the pooled pod so it is not leaked; and (b) report the run failed via the retried `failRun` helper from G-A3(b) with a System (`step -1`) log line stating the reason (e.g. `run pod did not become ready within 5m0s`). If the wait was aborted because the run is already terminal at the controller, skip the FinishRun override (the controller's status is authoritative) but still clean up the pod.

### Rationale
The timeout alone makes a wedged run self-heal (fail + become reapable) within a bounded window; the cancel-aware watcher restores user cancellation during the pre-Running window. Both mirror behavior the rest of the system already assumes.

---

## G-A2 — Cancel poller reacts to any terminal status, not just Cancelled

### Problem
The cancel poller inside `RunClaim` (`orchestrator.go:117-127`) checks **only** `run.Status == api.RunCancelled` (`orchestrator.go:122`). If the controller marks a run `RunFailed` out-of-band — e.g. a stuck-run reaper trips during a network partition, then the partition heals — the agent keeps executing subsequent (possibly side-effectful) steps because it never sees the `Failed`. The controller believes the run is over; the agent does not.

### Design
1. **Broaden the stop condition** at `orchestrator.go:122` from `run.Status == api.RunCancelled` to **any terminal status** using the shared `isTerminalRunStatus` helper (Succeeded/Failed/Cancelled). Practically, while a run is executing only Cancelled or Failed can appear; Succeeded is included for completeness/robustness.
2. **Preserve correct labeling.** Distinguish the two reasons the master terminated the run:
   - **Cancelled** → existing behavior: set `cancelledByMaster` so in-flight steps are relabeled `Cancelled` and `cancelRun()` stops the pipeline.
   - **Failed (or other terminal)** → set a new `reapedByMaster atomic.Bool` and call `cancelRun()` to stop the pipeline. Steps stop; the run is **not** relabeled Cancelled.
3. **Do not resurrect a master-terminal run.** When `reapedByMaster` is set, `RunClaim` must **skip its terminal `FinishRun` override** (`orchestrator.go:698-700`) — the controller already holds the authoritative terminal status, and re-reporting would be redundant or could overwrite it. (For the Cancelled path, existing behavior is retained.)
4. Emit a System (`step -1`) log line noting the run was stopped because the controller reported it `<status>`, for operator visibility.

### Rationale
Turns the poller from a cancel-only watcher into a general "the controller says this run is over — stop now" signal, closing the partition-heal side-effect window. Reusing `isTerminalRunStatus` keeps the terminal set defined in one place.

---

## G-A3 — Graceful drain, bounded concurrency, retried failure reporting

Three sub-fixes, all porting host-agent semantics.

### (a) Graceful drain
**Problem:** `K8sAgent.Run` uses a single `ctx` for claiming and execution, has no `WaitGroup` over in-flight `executeRun` goroutines, and no drain timeout. On SIGTERM (e.g. a rollout), in-flight runs are cancelled mid-step and abandoned; the heartbeat done-channel is discarded. The host agent (`internal/agent/agent.go:213-254`) does this correctly.

**Design — port the host model:**
- Split contexts: keep the incoming `ctx` as **`claimCtx`** (stops the claim loop on shutdown) and derive a **`runCtx`** (`context.WithCancel(context.Background())`) under which in-flight runs continue after claiming stops (cordon).
- Bind the **heartbeat to `runCtx`**, not `claimCtx`, so draining runs are not reaped as dead; join its done-channel on exit.
- Add a **`DrainTimeout`** config field (yaml `drainTimeout`, flag `--drain-timeout`, env `UNIFIED_K8S_DRAIN_TIMEOUT`). Default **0 = wait indefinitely**, matching the host agent (`config/agent.go`, `cmd/agent/main.go:68`). When `> 0`, a goroutine cancels `runCtx` once `DrainTimeout` elapses after `claimCtx` is done — bounding how long we wait for in-flight runs (host model, `internal/agent/agent.go:217-228`).
- Track in-flight `executeRun` goroutines with a `sync.WaitGroup`; on shutdown, stop claiming, wait for the group (or drain timeout), join the heartbeat, then `Deregister`.

### (b) Retried failure reporting on pod-acquisition failure
**Problem:** The four pre-orchestration failure paths in `executeRun` use single-shot `_ = a.client.FinishRun(ctx, …, api.RunFailed)`: pool claim (`agent.go:186`), BuildPod (`agent.go:201`), CreatePod (`agent.go:207`), WaitForPodRunning (`agent.go:220`). A transient controller error there discards the report and leaves the run stuck `Running` until the reaper trips. The host's `Agent.failRun` (`internal/agent/agent.go:367-379`) retries via `retryUntilSuccess` and emits a System log line.

**Design:** Introduce a k8s `failRun(ctx, runID, msg)` helper that (1) appends a System (`step -1`) log line with `msg` via the client (best-effort), then (2) calls `FinishRun(..., api.RunFailed)` wrapped in the shared exported `agentlib.RetryUntilSuccess` (`internal/agent/retry.go:60`). Replace the four single-shot sites with calls to it. This also serves G-A1's clean-failure requirement.

**Retry context note:** during drain, retries must not block forever — the retry uses `runCtx` so they are bounded by `DrainTimeout`.

### (c) Enforce `maxConcurrent`
**Problem:** `k8sagent.Config.MaxConcurrent` (`config.go:29`, defaulted to 5) is parsed but **never read** in `agent.go` — `go a.executeRun(ctx, resp)` (`agent.go:135`) is unbounded, so a burst of claims spawns unbounded goroutines and pods.

**Design:** Gate run execution with a **counting semaphore** (`chan struct{}` of size `cfg.MaxConcurrent`). Acquire a slot **before claiming** (or before spawning `executeRun`) and release it when the run goroutine finishes. A semaphore — rather than the host's N-fixed-slot workspace model — fits k8s because pods are per-run (there are no reusable per-slot workspaces to pre-create). Effect: at most `MaxConcurrent` runs (and thus run pods from this agent) execute at once; excess claims wait. Interacts cleanly with the WaitGroup from (a): the semaphore bounds live goroutines, the WaitGroup drains them.

---

## Testing

Unit tests (package `internal/k8sagent`, plus `internal/agent` for the poller), using the existing fakes (`fakePodManager`, fake master client — see `internal/k8sagent/report_retry_test.go`, `pool_test.go`, `config_test.go`):

- **G-A1 timeout:** a pod stuck `Pending` past `PodStartTimeout` → `executeRun` reports `RunFailed` (retried) with the timeout reason, and the fresh pod is deleted (no leak). Pooled variant: the pooled pod is released/cleaned.
- **G-A1 cancel-aware:** while the pod is still `Pending`, the fake controller flips the run to `Cancelled`/`Failed` → the wait aborts before the timeout; pod cleaned; no FinishRun override when already terminal.
- **G-A1 config:** `PodStartTimeout` parses from yaml and `UNIFIED_K8S_POD_START_TIMEOUT`; non-positive clamps to 5m.
- **G-A2:** poller sees `RunFailed` from the controller → pipeline stops (`runCtx` cancelled), `reapedByMaster` set, terminal `FinishRun` override skipped; `RunCancelled` still labels steps `Cancelled` as before.
- **G-A3(a) drain:** with an in-flight run, cancelling `claimCtx` stops new claims but lets the in-flight run finish under `runCtx`; `Run` returns only after the WaitGroup drains; heartbeat joined. A run exceeding `DrainTimeout` is cancelled.
- **G-A3(b):** each of the four pod-acquisition failure sites retries `FinishRun` on transient error (mirror `TestOrchestrate_ReportRetriesUntilSuccess`).
- **G-A3(c):** with `MaxConcurrent=2` and 3 simultaneous claimable runs, at most 2 `executeRun` run concurrently; the third starts only after a slot frees.

## Docs

Update the k8s agent config/ops docs to document `podStartTimeout` / `UNIFIED_K8S_POD_START_TIMEOUT`, `drainTimeout` / `UNIFIED_K8S_DRAIN_TIMEOUT` / `--drain-timeout`, and that `maxConcurrent` is now enforced. Note the new graceful-drain shutdown behavior (parity with the host agent's `--drain-timeout`).

## Out of scope
- Host agent changes (already has these).
- PVC-backed workspace persistence, pod pre-warming, or reuse-pool changes (separate work, PR #50).
- Controller-side reaper tuning.
