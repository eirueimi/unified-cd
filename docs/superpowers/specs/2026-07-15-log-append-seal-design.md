# Log Append Seal (Reject Agent Writes After Archival) — Design

**Date:** 2026-07-15
**Status:** Approved (design review done in session)
**Depends on:** run retention (#44) and tiered log storage (#45), both on main.

## Problem

Agent-facing write endpoints validate nothing about the target run. For log
appends (`POST /agents/{agentId}/logs` and `.../logs/bulk`), lines that arrive
after the run's logs were archived are never captured by the archive:

- they permanently disqualify the run from log trimming (the coverage filter
  silently excludes it, so its DB log rows are never reclaimed);
- appended after trimming, they become invisible ghost rows (all reads serve
  the archive) that waste space until run retention deletes the run;
- they make a completed run's visible log mutable (minor auditability issue).

Realistic sources: agent retries after a network partition (the stuck-run
reaper already finalized the run), buffered teardown lines from cancelled
runs, lagging sidecar log pumps.

Separately, `PUT /runs/{id}/artifacts/{name}` accepts uploads for nonexistent
runs; a late upload after `deleteRunEverywhere` recreates an orphaned
`artifacts/<runID>/...` object that nothing will ever delete (flagged in the
run-retention final review).

## Decisions (from design review)

| Question | Decision |
|---|---|
| Seal boundary | **Archival**: appends are rejected once a `run_log_archives` record exists. The archiver runs ~30 s after a run turns terminal, so legitimate teardown/flush lines land before the seal and are captured by the archive (and counted in its coverage). No new config, no wall-clock grace. |
| Rejection response | **Silently drop**: 204, line not stored, `slog.Warn` for observability. Mirrors `handleAgentFinishRun`'s `alreadyFinalized` design — agents treat >= 400 as errors, so an error status would cause retry storms on unmodified agents. Dropped lines are exactly the lines no reader would ever see, so there is no real information loss. |
| Scope | Log append + bulk (seal guard) and artifact upload (404 for nonexistent runs). Step reports, outputs, sidecar status, and finish are untouched — they have legitimate late deliveries (`finish` already handles its race via CAS + `alreadyFinalized`). |
| Guard mechanism | **In the INSERT statement** (Approach B), not a separate lookup — log append is the hottest write path in the system and a per-request `GetLogArchive` (Approach A) would double its DB round trips. |

## Design

### Store: guarded insert

`AppendLog` (`internal/store/postgres.go`) becomes a conditional insert —
one statement, zero extra round trips, atomic with respect to the archiver:

```sql
INSERT INTO logs(run_id, step_index, stream, ts, line)
SELECT $1, $2, $3, $4, $5
WHERE NOT EXISTS (SELECT 1 FROM run_log_archives WHERE run_id = $1)
RETURNING seq;
```

- Signature stays `(int64, error)`; **`(0, nil)` now means "dropped: run is
  sealed"** (real seqs are a Postgres sequence starting at 1, so 0 is
  unambiguous). The doc comment states this contract.
- No row inserted → the `log_appended` notification path never fires → SSE
  clients see nothing, consistent with readers.
- Internal callers are unaffected: `tryQueueRun`'s system messages target
  Pending runs, which can never have an archive record.
- Race window: a line that lands between the archiver's `TailLogs` read and
  its `CreateLogArchive` still inserts (record doesn't exist yet) and is not
  in the archive — unchanged from today, and the tiered-storage coverage
  check already keeps such runs untrimmed. The seal shrinks the exposure
  from "forever after archival" to that few-second window.

### Handlers: drop accounting

`handleAgentLogAppend`: when `AppendLog` returns `(0, nil)`, respond 204 and
`slog.Warn("dropping log line for sealed run", "run", req.RunID)`.

`handleAgentLogBulk`: loops `AppendLog` per line (existing structure); count
lines with `seq == 0` and, if any, emit ONE
`slog.Warn("dropping log lines for sealed run", "run", ..., "dropped", n)`
per request (not per line). Still 204.

### Artifact upload: existence check

`handleArtifactUpload` (`internal/controller/api_artifacts.go`): before
reading the body, `GetRun(ctx, runID)`; `store.ErrRunNotFound` → 404. Other
statuses are accepted unchanged — a late upload to a terminal-but-existing
run is referenced by the run and cleaned up by `deleteRunEverywhere`'s
prefix delete when the run is eventually removed; only uploads to *deleted*
runs create unreachable orphans, and those now 404. The agent-side artifact
uploader treats >= 400 as a step failure, which is correct here: the run is
gone, the upload is pointless.

### Docs

- `docs/troubleshooting.md`: entry for the grep-able warning
  `dropping log line for sealed run` — meaning (line arrived after the log
  archive was written; it was discarded), common causes (agent retry after
  partition, teardown flush later than the ~30 s archiver delay), and that
  it is expected noise in small quantities.
- `docs/operations.md`: one sentence in the tiered-log paragraph: log lines
  arriving after archival are discarded (warn-logged) rather than stored
  where no reader would see them.

## Out of scope

- Guards on step reports, outputs, sidecar status (legitimate late writes;
  no data-loss or leak consequence).
- Auditing agent endpoints (the audit middleware covers human-facing routes
  only; unchanged).
- A Prometheus counter for dropped lines (add later if the warn proves
  noisy enough to need graphing).
- Agent-side protocol changes (410 handling) — the silent-drop contract
  requires none.

## Testing

- Store (`postgres_log_seal_test.go` or appended to an existing logs test
  file): `AppendLog` to a run WITHOUT an archive record inserts and returns
  seq > 0; to a run WITH a record returns `(0, nil)` and inserts nothing
  (assert via `CountLogs`); internal system-message path (Pending run)
  unaffected.
- Handler: append to a sealed run → 204 and no rows; bulk with a sealed run
  → 204, zero rows, single warn; append to a live run still works end to
  end (SSE/notification behavior implicitly covered by existing tests).
- Artifact upload: nonexistent run → 404 body mentions the run; existing
  run upload round-trip still passes (existing test).
- Regression: a run that receives NO post-archival appends still trims
  normally (existing log-trim tests cover; no change expected).
