# Hardening F1–F7 — Design

**Date:** 2026-07-15
**Status:** Approved (design review done in session)
**Source:** 2026-07-15 same-class defect audit (findings F1–F7; F8 fixed by #46, F9 deferred).

## Problems

- **F1** — `auditLogMiddleware` buffers request bodies with
  `io.LimitReader(r.Body, 64KiB+1)` and hands the truncated buffer to the
  downstream handler. Job YAML or secrets larger than 64 KiB are silently
  cut: confusing parse 400s at best, silently-corrupted stored specs/secrets
  at worst.
- **F2** — `handleAgentSetStepOutputs`, `handleAgentSetRunOutputs`,
  `handleAgentSidecarStatus`, and `handleAgentCreateApproval` accept writes
  for terminal runs (late/duplicate agent reports mutate finished runs; an
  approval created on a terminal run with timeout 0 shows Pending forever).
  `handleAgentStepReport`/`handleAgentFinishRun` already guard — the
  pattern exists but was not applied to these four.
- **F3** — no agent-facing handler compares the `{agentId}` path param to
  `runs.claimed_by`; with the shared agent token, any agent (e.g. a stale
  pre-restart process) can write into any run.
- **F4/F5** — the archiver (`ListRunsNeedingArchival`, oldest-first LIMIT
  20) and the retention sweeper (`ListExpiredRuns`, oldest-first LIMIT 100
  + zero-progress break) can be starved by permanently-failing "poison"
  candidates filling every batch — the same wedge class fixed in log-trim,
  unfixed in its siblings.
- **F6** — the git resolver treats non-deterministic resolve errors as
  transient forever: runs pointing at permanently-unreachable repos stay
  Pending eternally AND (oldest-first LIMIT 50) starve resolution for newer
  runs.
- **F7** — `cache.Save` Puts `<key>.tar.zst` then `<key>.meta`; if the meta
  Put fails, the archive object is orphaned forever (GC and lookup iterate
  `.meta` only). No compensating delete.

## Decisions (from design review)

| Question | Decision |
|---|---|
| Delivery | One branch (`hardening`), one spec, one plan; each fix group an independent task |
| F3 mode | **Enforce immediately (403)** — all clients verified to send their own agent ID; agent+controller ship from one repo |
| F3 hot path | `claimed_by` is immutable once set → controller-side bounded cache (runID → claimed_by, non-NULL only) makes per-line ownership checks memory-cheap |
| F2 response | Terminal writes are no-ops returning **200 `{alreadyFinalized: true}`** (mirrors `handleAgentStepReport`); agents need no changes |
| F4/F5/F6 starvation | Shared in-memory **failure backoff** (leader-local), excluded IDs passed into the candidate SQL — no schema change; resets on failover (each poison retried once, then re-excluded) |
| F6 immortal runs | Resolve failures on runs older than a **deadline (default 1 h, env `UNIFIED_GIT_RESOLVE_DEADLINE`, Go duration)** fail the run with a system log line |
| F7 | Compensating best-effort delete of the just-written archive object when the meta Put fails (mirrors the log archiver's compensation) |

## Design

### F1 — audit middleware body pass-through

In `auditLogMiddleware` (`internal/controller/audit.go`), keep the 64 KiB
peek for audit-name extraction but reconstruct the downstream body as
`io.NopCloser(io.MultiReader(bytes.NewReader(reqBody), origBody))` where
`origBody` is the original (not fully consumed) `r.Body`. Handlers see the
full body; the audit record's body-derived resource name still works for
normal-sized envelopes (a >64 KiB body may yield an empty resource name —
unchanged from today and acceptable). Test: a middleware-level test posts a
>64 KiB body through the middleware to a recording handler and asserts
byte-for-byte receipt; a second case asserts the audit name extraction still
works for small bodies.

### F2 + F3 — agent run-write guard

New helper in `internal/controller` (one place, one policy):

```go
type runWriteVerdict int
const (
    runWriteOK runWriteVerdict = iota
    runWriteNotFound  // 404
    runWriteNotOwned  // 403
    runWriteTerminal  // 200 {"alreadyFinalized": true}
)
func (s *Server) agentRunGuard(ctx, agentID, runID string, rejectTerminal bool) (runWriteVerdict, error)
```

- Resolves `claimed_by` through a bounded LRU cache (non-NULL values only —
  `claimed_by` never changes once set; cache size ~10k, evict oldest).
  Cache miss → `GetRun` → populate.
- `run` missing → notFound. `claimed_by` NULL (write before claim) or
  ≠ agentID → notOwned. Terminal status and `rejectTerminal` → terminal.
- Store lookup errors propagate as 500 at the call site.

Application matrix (all under the agent bearer token):

| Endpoint | ownership (403) | terminal (no-op 200) |
|---|---|---|
| `handleAgentSetStepOutputs` / `SetRunOutputs` | ✓ | ✓ |
| `handleAgentSidecarStatus` | ✓ | — (ownership-only; see note below) |
| `handleAgentCreateApproval` | ✓ | ✓ |
| `handleAgentLogAppend` / `LogBulk` | ✓ | — (the archival seal governs logs) |
| `handleAgentFinishRun` / `StepReport` | ✓ (added) | existing CAS/guard unchanged |

Notes:
- Artifact upload stays out of F3 (no `{agentId}` in its route; protocol
  change out of scope). Its existence check landed in #46.
- 403 body names the mismatch (`run %s is claimed by another agent`) —
  grep-able for troubleshooting.
- The log endpoints keep their current success shape (204 / drop warning);
  ownership rejection is the only added branch.
- `handleAgentSidecarStatus` is deliberately ownership-only
  (`rejectTerminal=false`), not part of the terminal no-op set above: both
  agents stop their sidecar pumps via a deferred `CloseScopes` that runs
  *after* `FinishRun`, so the final `reportStatus(..., "exited", exitCode)`
  call always arrives once the run is already terminal. Rejecting it would
  permanently strand every completed run's sidecar display at its last
  pre-exit phase. `UpsertSidecarStatus` is a display-only upsert keyed by
  `(run, index)`, so the late write is harmless.
- Outputs reported after a run is terminal are intentionally **not**
  recorded (the existing `runWriteTerminal` no-op above is left as-is) —
  this includes run/step outputs set by `finally:` steps of a run that was
  cancelled, since cancellation marks the run terminal before those
  `finally:` steps execute. This is consistent with `handleAgentStepReport`,
  which already no-op'd step status writes on terminal runs before this
  branch, and it prevents a run's recorded outputs from mutating after the
  run has been reported complete to callers.

### F4/F5/F6 (starvation) — shared failure backoff

New `internal/controller/failure_backoff.go`:

```go
type failureBackoff struct { ... mutex, map[id]{failures int, retryAt time.Time}, cap int }
func newFailureBackoff(base, max time.Duration, cap int) *failureBackoff
func (b *failureBackoff) Excluded(now time.Time) []string // ids with retryAt > now
func (b *failureBackoff) Failure(id string, now time.Time) // failures++, retryAt = now + min(base<<n, max)
func (b *failureBackoff) Success(id string)                // delete entry
```

Defaults: base 1 min, doubling, max 1 h, cap 10 000 entries (oldest evicted
past cap). Leader-local by design: a failover/restart clears it; each poison
costs one retry before re-exclusion.

Wiring (each loop owns one instance):
- **Archiver**: `ListRunsNeedingArchival(ctx, limit, excluded []string)`
  gains `AND id != ALL($2)`; `archiveRunLogs` failure → `Failure(runID)`,
  success → `Success(runID)`.
- **Retention sweeper**: `ListExpiredRuns(ctx, cutoff, limit, excluded)`
  gains the same clause; `deleteRunEverywhere` failure → `Failure`, success
  → `Success`. The zero-progress break stays (it now only fires when
  genuinely nothing is deletable this tick).
- **Git resolver**: its pending-run candidate query gains the clause;
  transient resolve failure → `Failure(runID)` (plus the F6 deadline below),
  success or deterministic failure (run already Failed) → `Success`.

### F6 (deadline) — resolve deadline

In the git resolver: when a resolve attempt fails with a transient error AND
`now - run.created_at > deadline`, append a system log line
(`git template resolution failed for more than <deadline>: <last error>`)
and `MarkRunFinished(runID, Failed)`. Deadline default **1 h**, overridable
via `UNIFIED_GIT_RESOLVE_DEADLINE` (Go duration string; invalid/unset →
default; `0` keeps the default rather than disabling — disabling returns to
immortal-Pending, which F6 exists to remove). Deterministic resolve errors
keep failing the run immediately (unchanged).

### F7 — cache compensating delete

In `internal/cache/cache.go` `Save`: if the `.meta` Put fails after the
`.tar.zst` Put succeeded, best-effort `Delete` the archive object (warn on
cleanup failure) before returning the error — no orphan survives a partial
save. Mirrors `archiveRunLogs`'s compensation.

### Configuration & docs

- `docs/configuration.md`: `UNIFIED_GIT_RESOLVE_DEADLINE` (env-only; no
  flag — resolver tuning is niche).
- `docs/troubleshooting.md`: entries for `run %s is claimed by another
  agent` (403) and the resolve-deadline failure line.
- `docs/operations.md`: one paragraph on sweep failure backoff (poison
  candidates are retried with exponential backoff up to 1 h and no longer
  block their batch; state is per-leader and resets on failover).

## Out of scope

- F9 (agent-side LogPusher 1 MiB drop) — deferred by user decision.
- Ownership for artifact upload (no agent identity in the route).
- Persisting backoff state in the DB (schema churn not justified; failover
  cost is one retry per poison).
- Auditing agent endpoints; Prometheus counters for drops/backoffs.

## Testing

- **F1**: middleware test — >64 KiB body reaches a recording handler intact
  while the request is still audited; small-body name extraction unchanged.
- **F2/F3**: guard unit tests (all four verdicts, cache hit path, NULL
  claimed_by); per-endpoint HTTP matrix tests — wrong agent → 403 + no
  mutation; terminal run → 200 alreadyFinalized + no mutation (outputs /
  sidecar / approval); correct agent + live run unchanged; finish/stepReport
  keep their existing terminal semantics with ownership added.
- **F4/F5**: backoff unit tests (exponential schedule, cap eviction,
  Success clears); store tests for the `excluded` param (empty slice = no
  filter; excluded IDs absent from results); loop tests — a poison candidate
  fails once, is excluded next tick, batch proceeds to other candidates.
- **F6**: resolver test — transient failure on a young run leaves it
  Pending; on an old run (backdated `created_at`) fails it with the system
  log line; deterministic errors still fail immediately.
- **F7**: cache test — meta Put failure deletes the archive object (fake
  object store failing only the second Put); successful save unchanged.
- Full `go test ./internal/...` green.
