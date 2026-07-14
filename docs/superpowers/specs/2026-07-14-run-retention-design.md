# Run Retention (TTL) — Design

**Date:** 2026-07-14
**Status:** Approved (design review done in session)

## Problem

Terminal runs accumulate forever. The audit log has age-based cleanup
(`audit_retention.go`), but runs have none: `runs` rows, their cascaded child
rows (`logs`, `step_reports`, `run_outputs`, `step_outputs`, `run_approvals`,
`run_log_archives`, `sidecar_status`), archived log objects
(`runs/<runID>/logs.ndjson`), and artifact objects (`artifacts/<runID>/...`)
all grow without bound. Note that the log archiver only *copies* logs to the
object store — `logs` rows are never trimmed — so run retention is the only
mechanism that reclaims DB log space.

Additionally, the existing manual `DELETE /api/v1/runs/{id}` deletes only the
DB row; it leaks the run's log-archive and artifact objects in the object
store. This design fixes that leak as a side effect.

## Decisions (from design review)

| Question | Decision |
|---|---|
| Retention criterion | Age in days only (no keep-last-N) |
| Deletion scope | DB rows **and** object-store objects (log archive + artifacts) |
| Configuration | Controller-global flag/env only; no per-job DSL override |
| Default | `0` = disabled (opt-in); deleting run history is irreversible (including the spec snapshot `run replay` uses) |
| Mechanism | Same pattern as audit retention: hourly ticker, advisory-lock leader election |

## Design

### Configuration

- Flag `--run-retention-days` on the controller, env `UNIFIED_RUN_RETENTION_DAYS`.
- Default resolution mirrors `auditRetentionDaysDefault()` in
  `cmd/controller/main.go`, except the fallback default is **0 (disabled)**.
- `<= 0` disables the sweeper entirely (keep forever). Startup logs whether
  retention is enabled, same as the audit branch.

### Sweeper

New file `internal/controller/run_retention.go`:

- `RunRunRetention(ctx, st, objStore, interval, retentionDays)` goroutine,
  started from `cmd/controller/main.go` next to `RunAuditRetention`.
  Interval defaults to 1 hour.
- Leader election via a new advisory lock key `runRetentionLockKey =
  int64(0x7272746E) // 'rrtn'`. Add it to the existing key-inventory comments
  (audit_retention.go and any other file that lists the keys).
- Each tick, the leader:
  1. `ListExpiredRuns(ctx, cutoff, 100)` — new store method returning IDs of
     runs with `status IN ('Succeeded','Failed','Cancelled') AND
     updated_at < now() - retentionDays` (oldest first, LIMIT).
  2. Deletes each run via the shared helper (below). Per-run failures are
     logged and skipped; the run is retried on a later tick.
  3. If the batch was full (100 rows), immediately fetches the next batch
     within the same tick; stops when a batch comes back short or empty.
     A batch that deletes **zero** runs also stops the tick — failed runs
     stay in the result set (oldest first), so continuing would refetch the
     same IDs and spin forever.

### Per-run deletion helper

`deleteRunEverywhere(ctx, st, objStore, runID)` in `internal/controller`,
used by **both** the sweeper and `handleDeleteRun`. Order is the invariant —
objects first, DB row last, so a surviving DB row always still references any
surviving objects (no orphaned objects, ever):

1. If a `run_log_archives` record exists, `objStore.Delete(archive.ObjectKey)`.
2. `objStore.List("artifacts/<runID>/")`, then `Delete` each key.
3. `st.DeleteRun(runID)` — FK `ON DELETE CASCADE` removes all child rows.

Error handling:

- Any object-deletion failure aborts before step 3; the sweeper logs and
  moves on (retry next tick), the API handler returns 500 (client retries).
- `objStore == nil` (object store not configured): skip steps 1–2 and delete
  the DB row. Nothing was ever uploaded in such deployments.
- `ObjectStore.Delete` returns nil for missing keys, so partial retries are
  idempotent.

**Archiver race.** The log archiver and this deletion path walk terminal runs
independently, often on different replicas, so their steps can interleave
around a single run: (a) we find no `run_log_archives` record, then the
archiver `Put`s the object and its `CreateLogArchive` fails on the now-gone
run's FK, orphaning the object; (b) the archiver's `Put`+`CreateLogArchive`
completes *after* our lookup found nothing but *before* our `DeleteRun` runs,
so the cascade removes the record we never saw. We close (a) in the archiver
(delete the object it just wrote if `CreateLogArchive` fails) and (b) here by
always deleting the deterministic `runLogArchiveKey(runID)` unconditionally —
both before and, best-effort, once more after `DeleteRun` — instead of
relying solely on the archive record.

### Manual DELETE API

`handleDeleteRun` keeps its existing 404/409 (non-terminal) checks and calls
`deleteRunEverywhere` instead of bare `st.DeleteRun`. This fixes the current
object leak for manual deletes.

### Docs

- `docs/configuration.md`: document the new flag/env.
- `docs/operations.md`: retention is the only DB/object space reclamation;
  note default-off and the irreversibility (replay of a deleted run is gone).
- `docs/high-availability.md`: add the new advisory-lock key to the leader
  table.
- No DSL/schema change, so `field-reference.md`, `schemas/`, `examples/`,
  `templates/` are untouched.

## Out of scope

- Keep-last-N-per-job (count-based) retention.
- Per-job DSL retention override.
- Tiered log storage — trimming `logs` rows after archival while keeping the
  archive longer than the run. That would require the API/WebUI to fall back
  to reading `runs/<runID>/logs.ndjson`, which does not exist today
  (deliberately deferred by the 2026-07-07 windowed-log-viewer design).
- Retention for anything other than runs (audit already has its own; jobs,
  schedules, etc. are user-managed resources).

## Testing

- `run_retention_test.go` (mirror `audit_retention_test.go`):
  - disabled (`days <= 0`) never touches the store;
  - leader loses the advisory lock → no deletion;
  - fake object store: objects deleted before the DB row; an object-delete
    failure leaves the DB row intact (retried next tick);
  - `objStore == nil` still deletes the DB row.
- Store test for `ListExpiredRuns`: cutoff boundary, non-terminal statuses
  excluded, LIMIT respected, oldest-first ordering.
- Regression test: `DELETE /api/v1/runs/{id}` removes the run's log-archive
  and artifact objects from a fake object store.
