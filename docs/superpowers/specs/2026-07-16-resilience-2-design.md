# Agent Resilience Wave 2 — Panic Recovery, Lost-Claim Reconcile, Disk Preflight + Workspace GC, LogPusher Drop Marker — Design

**Status:** Approved 2026-07-16 (Branch C of the A→B→C→D program; decisions locked via Q&A — heartbeat body for reconcile, disk preflight + age-based opt-in GC, single branch).

**Goal:** Close the four remaining agent-resilience gaps from the 2026-07-15 failsafe audit (items 4, 6, 7, 8): a panicking step no longer kills the process; a lost claim is reconciled without a restart; disk exhaustion is prevented (preflight) and mitigable (opt-in GC); dropped log lines become visible.

## 1. Panic recovery (audit item 4)

**Problem (verified):** no `recover()` anywhere. A panic in a step body, template expansion, or backend exec (host parallel goroutine `pipeline.go` `runParallel`, or either agent's per-run goroutine) crashes the whole process, killing every concurrent run — and leaves those runs `Running` until the reaper trips.

**Design:**
- Primary seam: `runOne` in `internal/agent/pipeline.go` — the single wrapper every step invocation flows through (sequential, parallel, single-step; both agents via `RunClaim`→`RunPipeline`). A `defer func(){ if r := recover(); r != nil { <convert to error> } }()` turns a step panic into the returned `error`, which flows through the existing failure machinery (`recordFailure`/`anyStepFailed`/`overallStatus = RunFailed`). Include the recovered value + a stack (`debug.Stack()`) in the error and a `slog.Error`, and emit it into the step's own log (System line) so the author sees the panic, not just an opaque failure.
- Defense-in-depth outer guards (so a panic OUTSIDE a step still fails only its run): wrap the host slot goroutine body (`internal/agent/agent.go` runLoop/executeRun) and the k8s dispatch goroutine body (`internal/k8sagent/agent.go`) in a recover that reports the run failed via the existing retried fail path (host `failRun`, k8s `failRun`) rather than leaving it `Running`.
- Non-run-execution goroutines (heartbeat, GC, drain watchdog, cancel poller): a recover that logs and (for long-lived loops) does not take the process down — but these are out of the step blast radius; keep minimal (log-and-return; the loop's supervisor already handles restart-on-next-tick where applicable). Prefer NOT to silently swallow — every recover logs at Error with the stack.

## 2. Lost-claim periodic reconcile (audit item 6)

**Problem (verified):** a run claimed server-side (marked `Running` in the same SQL as the claim) whose HTTP response the agent never received is `Running` forever — the agent isn't executing it, but its heartbeat keeps `last_seen_at` fresh, so the stuck-run reaper never fires. Only a process restart's startup `ReconcileRuns` fixes it.

**Design (heartbeat-carried active set; backward compatible):**
- Each agent tracks a live set of run IDs it is currently executing: a mutex-guarded `map[string]struct{}` on `*Agent`/`*K8sAgent`, enrolled right after a non-empty claim (host `runLoop` before `executeRun`; k8s dispatch goroutine before `dispatch`) and retired when execution returns.
- The heartbeat request gains an optional body carrying `activeRunIDs []string` (today it is bodyless). `StartHeartbeat` reads the active set each beat.
- Controller `handleAgentHeartbeat`: if the request carries an active-run list, enumerate `ListRunningRunIDsByAgent(agentID)` and `failOrphanedRun` any run assigned to this agent that is **not** in the reported set **and** whose `claimed_at` is older than a grace window (reuse the reaper's grace threading; a just-claimed run whose response is still in flight must not be failed). A heartbeat with **no body** (old agent) skips reconciliation entirely — fully backward compatible; those agents keep relying on startup reconcile.
- Grace: the active-run reconcile only fails runs `claimed_at` older than a configurable grace (default aligned with the existing claim grace / reaper staleAfter — pick the smaller safe value, e.g. 60s) so the claim→heartbeat race can never fail a healthy run.

## 3. Disk preflight + opt-in workspace GC (audit item 7)

**Problem (verified):** per-job host workspace dirs (`working<slot>/<job>`) persist across runs by design (inter-run cache) and are never swept across jobs; no free-disk check before claiming. Disk fills silently.

**Design:**
- **Preflight (host agent):** a `minFreeDisk` config knob (bytes or percent; `AgentConfig` + flag/env). Before each `Claim`, check free space on the workspace filesystem (`Statfs`/`GetDiskFreeSpace` via a small cross-platform helper — `golang.org/x/sys` is likely already vendored; else build-tag a windows/unix pair). Below threshold → skip claiming this tick (log + backoff, like the claim-error path), never destructive. k8s agent workspaces are pod volumes, not the agent's disk — preflight is host-only.
- **Opt-in age-based GC (host agent):** a `workspaceRetentionDays` config knob (default 0 = disabled — persistent workspaces are a feature; GC must be explicit). When > 0, a periodic sweep (startup + on an interval, mirroring `runPodGC`) removes `working<slot>/<job>` dirs whose last-use is older than the retention. Last-use: the dir has no timestamp today; use the dir's mtime, OR (more robust) touch a `.ucd-lastused` marker at run start — decide toward mtime first (simplest; steps writing into the dir bump mtime naturally), documented. **Exclusions:** never touch `wsBase` itself, never touch dot-prefixed siblings (`.ucd-tools` shim dir), never touch a dir for a currently-active run (cross-check the active set from §2). Age-based + opt-in + active-exclusion = safe.

## 4. LogPusher drop marker + write-stall fix (audit item 8 / F9)

**Problem (verified):** `internal/agent/runner.go` `LogPusher` caps its pending buffer at 1MiB and drops oldest **silently** on send failure; the write path flushes synchronously under the lock with a `context.Background()` (60s client timeout) → each 4KiB write can block 60s under partition, and blocks every other writer to that pusher.

**Design:**
- **Drop marker:** accumulate a dropped-line counter in `appendPendingLocked` each time a batch is discarded. On the next SUCCESSFUL flush, if the counter > 0, prepend one synthetic System (`stepIndex -1` or the step's index) log line — e.g. `[N log line(s) dropped: controller unreachable]` — and reset the counter. Uses the existing `AppendLogBulk` + masker path already in `flushLocked`.
- **Write-stall:** give the write-path flush a bounded context (e.g. a few seconds) instead of `context.Background()`, so a partition can't consume the full 60s client timeout while holding `p.mu`; the existing 2s auto-flush ticker remains the steady-state drain. (Alternative — drop the synchronous flush and rely on the ticker — is riskier for burst buffering; prefer the bounded context.)

## Components / files
- `internal/agent/pipeline.go` (`runOne` recover), `internal/agent/agent.go` + `internal/k8sagent/agent.go` (outer goroutine recover; active-run set; preflight; GC), `internal/agent/heartbeat.go` + `internal/agent/client.go` (heartbeat body), `internal/controller/api_agent.go` (heartbeat handler reconcile), `internal/agent/runner.go` (LogPusher drop marker + bounded flush ctx).
- `internal/config/agent.go` (`minFreeDisk`, `workspaceRetentionDays`), `cmd/agent/main.go` (flags/env).
- New disk-free helper (build-tagged if needed).

## Testing
- Panic: a step that panics → run reported Failed with the panic message in its log; process/siblings survive (a test spawning a panicking parallel step alongside a normal one, asserting the normal one completes and the run is Failed). Outer-guard: a panic in the dispatch path → run Failed, not process death.
- Lost-claim: heartbeat with an active set omitting a Running-assigned run older than grace → that run is failed; a just-claimed run (within grace) is NOT failed; bodyless heartbeat → no reconcile.
- Disk: preflight skips claiming below threshold (fake free-space fn); GC removes an aged job dir, preserves a fresh one, `.ucd-tools`, `wsBase`, and an active-run dir; GC disabled by default.
- LogPusher: N drops then a successful flush emits the marker with the right count; a partition-slow flush is bounded (doesn't hold the lock for the full client timeout — assert with a slow fake client).
- Full suite + generated-artifact no-drift.

## Out of scope
- k8s-side disk preflight/GC (pod volumes, not agent disk).
- Artifact/cache RAM buffering (Branch D).
- Reworking the heartbeat cadence or the reaper's own staleness logic.
