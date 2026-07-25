# Detached runs: exempt lightweight orchestrators from MaxConcurrent

Date: 2026-07-25
Status: Approved (design)

## Decisions (locked)

- **Opt-in flag:** `spec.detached: true` (top-level boolean; default `false`).
- **Budget:** agent `MaxDetachedConcurrent` (`--max-detached-concurrent` /
  `UNIFIED_AGENT_MAX_DETACHED`); default `16`, `0` = off, `-1` = unlimited.
- **`native: true` + `detached: true`:** allowed — `native` (execution mode) and
  `detached` (concurrency accounting) are orthogonal. A native host orchestrator
  that mostly `call:`s is a valid, important case (it is exactly the kind of run
  that must be exempted to fix the deadlock on a host/native fleet). Heavy work
  under `detached` is an author misuse regardless of `native`, so there is no
  native-specific rejection; this is a documentation caution, not a hard error.
- **k8s pod handling:** Approach A — detached changes only claim accounting, not
  pod allocation. Detached runs still allocate a pod on k8s exactly as today; we
  do NOT lazily skip the pod (Approach B) in this version. Because a detached
  parent holds its pod idle for the whole `call:` wait, `podTemplate.reuse` gives
  no benefit for detached jobs and is documented as "do not combine." Skipping
  the pod for pure orchestrators (Approach B) is deferred to future work — it
  only helps when k8s *cluster* capacity (not the agent semaphore) is the
  bottleneck, and orchestrators on host agents have no pod at all.

## Problem

An agent runs up to `MaxConcurrent` runs at once. On the host agent this is
`MaxConcurrent` slot goroutines, each executing one run to completion and each
owning a slot-keyed workspace (`working<slot>/<job>`). On the k8s agent it is a
semaphore of size `MaxConcurrent` gating the claim loop
(`internal/k8sagent/agent.go`), each run holding a token for its whole lifetime.

A `call:` step launches a child run and **polls until the child completes**,
holding its slot/token the entire wait. If the pool has no free capacity for the
child (classically a single-slot agent, or a scarce pinned agent such as one
macOS builder shared by parent and child), the child stays `Queued` forever
while the parent stays `Running` — a slot deadlock. The current mitigation is
operational only (`max-concurrent >= 2`, or route the child to a different pool
via `agentSelector`), which does not scale to nested calls.

## Goal

Break the `call:` slot deadlock by letting a **lightweight orchestrator run**
(one that mostly issues `call:` steps and waits) run **without consuming a
normal execution slot**, so the scarce slot it would otherwise occupy stays free
for the real worker child.

## Design

### Opt-in flag

A new per-job boolean marks the job's runs as detached:

```yaml
spec:
  detached: true
```

`detached` is an **explicit author opt-in** (not inferred from `native`/
`agentSelector`). The author asserts the job is a lightweight orchestrator. A
detached run:

1. **Does not consume the agent's `MaxConcurrent` budget.** It is claimed into a
   separate, bounded detached budget instead.
2. **Gets an independent, per-run workspace** (host agent), decoupled from the
   slot-keyed workspace pool.

Normal (non-detached) runs are completely unchanged: they keep the
`MaxConcurrent` slot/semaphore model, so the invariant "at most `MaxConcurrent`
normal runs execute at once" is untouched.

### Why this avoids the resume over-subscription problem

An earlier "yield the slot during the call: wait, re-acquire on resume" idea was
rejected because when the child finished the parent would resume into a state
with more than `MaxConcurrent` runs actively executing. Detached avoids this
entirely: a detached run is **never** in the normal budget — not before, during,
or after the wait — so it can never push normal execution over `MaxConcurrent`.
The cost is that detached runs are *assumed* lightweight (author opt-in) and are
bounded by their own separate limit rather than by `MaxConcurrent`.

### Agent behavior

**Host agent.** In addition to the `MaxConcurrent` normal slot goroutines, run a
separate pool of up to `MaxDetachedConcurrent` detached-claim goroutines. A
detached-claim goroutine claims only detached runs, executes them with a fresh
per-run workspace (`detached/<runID>` under the workspace base rather than
`working<slot>/<job>`), and hands that directory to the existing workspace GC
(`workspace_gc.go`) for age-based cleanup.

**k8s agent.** The concurrency exemption is identical: a detached run is claimed
outside the `MaxConcurrent` semaphore, against the separate detached budget, so
its token stays free for the child. The **independent-workspace** aspect is a
no-op on k8s — each run already gets its own pod and `/workspace` volume, so
there is nothing slot-shared to decouple. Note a detached run on k8s still runs
an (idle) pod while it waits; detached frees the agent's claim/semaphore slot,
not the cluster pod. Skipping the pod entirely for pure orchestrators is a
future optimization (out of scope).

### Controller / claim changes

`ClaimNextRun` must be able to hand out **detached vs normal** runs selectively,
so a detached-claim goroutine picks up only detached runs and a normal slot
picks up only normal runs (otherwise a normal slot could grab a detached run and
lose the exemption, or vice versa). Add a claim-kind filter:

- Extend the claim request with a kind selector (e.g. `?kind=detached|normal`),
  and filter `ClaimNextRun`'s query on the run's `detached` column.
- The run's `detached` flag is derived from its stored spec at run-creation time
  and persisted on the run row (a new `detached bool` column) so the claim query
  can filter without parsing the spec.

This is backend-agnostic and serves both agent types identically.

### Bounds / configuration

- Agent config `MaxDetachedConcurrent` / flag `--max-detached-concurrent` / env
  `UNIFIED_AGENT_MAX_DETACHED`, mirroring the existing `MaxConcurrent` plumbing.
- Recommended default: a modest positive cap (e.g. 16). `0` disables detached
  claiming on that agent (detached runs are simply not claimed there — they wait
  for an agent that has detached capacity), matching the "0 = off" convention of
  the disk/GC knobs. A negative value = unlimited (mirrors the k8s
  `MaxConcurrent` convention), for pure-orchestrator fleets.
- Each detached claim still holds a goroutine, a controller connection, and (on
  the host) an independent workspace, so the cap plus the existing `MinFreeDisk`
  preflight bound the resource footprint.

### Validation

- `detached` is a spec-level boolean; default `false` (fully backward
  compatible, opt-in).
- `native: true` + `detached: true` is **allowed** (orthogonal — see Decisions).
  detached is an author assertion that the run is a lightweight orchestrator;
  marking a heavy job detached (native or not) can over-subscribe the host /
  cluster and is a documented misuse, not a validation error.
- `detached` composes with `agentSelector` and concurrency groups normally: a
  detached run may still target a pool and still participate in
  `mutex`/`semaphores`/`orLocks` serialization.

### Interaction with pod reuse (k8s)

A pooled pod is claimed exclusively: `ClaimPod` only ever reuses an **idle**
(`available`) pod and immediately stamps it `in_use` with the run's ID
(`annoPoolRunID`); it is returned to the pool by `ReleasePod` only in the
post-run teardown, **after** the run's execution fully returns. A `call:` wait
happens mid-run, so a detached parent's pod is **never released or reassigned to
another run during the wait** — no other job lands on it.

The flip side is a cost, not a correctness bug: with `podTemplate.reuse`, a
detached parent holds its pooled pod idle-but-`in_use` for the whole wait, so it
is unavailable to others (pool starvation), worsened by long orchestrator waits.
This is the same "idle pod held" cost noted above; the eventual fix is the
future pure-orchestrator no-pod optimization. Until then, prefer NOT enabling
`podTemplate.reuse` for detached orchestrator jobs, or size the pool for it.

### Interaction with existing features

- **Heartbeat / stuck-run reaper:** detached runs are reported in the heartbeat
  `activeRunIds` like any other in-flight run, so lost-claim reconcile and the
  stuck-run reaper work unchanged.
- **Concurrency groups (mutex/semaphore/orLock):** unaffected — those serialize
  by lock key at `Pending -> Queued`, orthogonal to slot accounting.
- **agentSelector unschedulable banner / queued-run reaper:** unchanged; a
  detached run with an unsatisfiable selector is still surfaced/failed the same
  way.

## Testing

- DSL: `detached: true` parses; `detached: true` + `native: true` is rejected (or
  warns) per the decision above.
- Store: `CreateRun` persists the `detached` column from the spec;
  `ClaimNextRun` with a kind filter returns only matching runs (a detached run is
  not handed to a normal claim and vice versa).
- Host agent: a detached run is claimed while all `MaxConcurrent` normal slots
  are busy (the exemption), gets a `detached/<runID>` workspace, and the child of
  a detached `call:` parent is claimable while the parent waits (deadlock gone).
- k8s agent: a detached run is claimed outside the `MaxConcurrent` semaphore.
- `MaxDetachedConcurrent` bounds concurrent detached claims; `0` disables.

## Documentation

- `docs/jobs.md`: a "Detached runs" section explaining the deadlock, the opt-in
  flag, that detached runs must be lightweight orchestrators, the separate
  `MaxDetachedConcurrent` budget, and the `native` restriction. Update the
  existing `call:` slot-deadlock warning to point at `detached` as the fix.
- `docs/configuration.md` / operations docs: the new agent knob.

## Migration / compatibility

Fully backward compatible: `detached` defaults to `false`, so existing jobs and
agents behave exactly as today. Adopting it is per-job opt-in plus setting
`MaxDetachedConcurrent` on the agents that should host orchestrators.

## Out of scope (future)

- Skipping the pod entirely for a pure-orchestrator detached run on k8s (it
  currently still runs an idle pod while waiting).
- Any change to how `call:` polls the child (covered by the separate call-step
  timeout work).
