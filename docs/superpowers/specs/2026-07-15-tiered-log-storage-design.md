# Tiered Log Storage (DB Log Trim) — Design

**Date:** 2026-07-15
**Status:** Approved (design review done in session)
**Depends on:** run-retention branch (PR #44) — reuses `run_log_archives`, the
retention sweeper pattern, and `runLogArchiveKey`.

## Problem

The log archiver only *copies* a terminal run's logs to the object store
(`runs/<runID>/logs.ndjson`); the `logs` rows stay in PostgreSQL forever, so
the DB grows unbounded even though a durable copy exists. This is the missing
second half of the original tiered-storage intent: the 2026-07-07
windowed-log-viewer design explicitly deferred "reading from the archive"
because DB rows were never deleted. Run retention (PR #44) deletes runs
wholesale at N days, but users may want log rows out of the DB much earlier
while keeping runs browsable.

## Decisions (from design review)

| Question | Decision |
|---|---|
| Trim timing | Age flag `--log-trim-days` / `UNIFIED_LOG_TRIM_DAYS`, default **0 = disabled** (same pattern as run retention) |
| Read fidelity after trim | **Full compatibility**: stats/range/search, CLI `GET /logs?after=N`, and SSE backfill all served from the archive with identical contracts; zero client changes |
| Fallback location | Controller layer (Approach A) — the store stays PostgreSQL-only |
| Rejected alternatives | Store-level transparent fallback (couples `internal/store` to `internal/objectstore`); on-demand rehydration into the DB (write amplification, races with retention) |

## Design

### Schema (migration 012)

`ALTER TABLE run_log_archives ADD COLUMN trimmed_at timestamptz;`
(null = rows still in the DB). `trimmed_at` is the single source of truth for
"serve this run's logs from the archive" — it avoids the ambiguity between
"genuinely empty logs" and "trimmed". Down migration drops the column.
`store.LogArchive` gains `TrimmedAt *time.Time`.

### Trim sweeper

New file `internal/controller/log_trim.go`, mirroring `run_retention.go`:

- `RunLogTrim(ctx, st, obj, interval, trimDays)` started from
  `cmd/controller/main.go`; hourly; returns immediately when `st == nil ||
  obj == nil || trimDays <= 0` (no object store → nothing was archived →
  never trim).
- Leader election via new advisory lock key `logTrimLockKey =
  int64(0x6C74726D) // 'ltrm'` (added to the key-inventory comments).
- Candidates: new store method `ListTrimCandidates(ctx, cutoff, limit)
  ([]string, error)` — run IDs from `run_log_archives` where `trimmed_at IS
  NULL AND archived_at < cutoff`, oldest first, LIMIT (batch 100, same
  full-batch/zero-progress loop semantics as the retention sweeper).
- Per run, in order:
  1. **Verify the archive object exists** via `obj.List(runLogArchiveKey(runID))`
     returning the key — trimming is irreversible, so never trust the DB
     record alone. Missing object → log a warning and skip (the archiver's
     compensation may have removed it; `ListRunsNeedingArchival` will not
     re-archive while the record exists, so also delete the stale
     `run_log_archives` record in this case so the archiver re-creates the
     archive on its next tick, and trim happens on a later sweep).
  2. New store method `TrimRunLogs(ctx, runID) (int64, error)` — one
     transaction: `DELETE FROM logs WHERE run_id = $1` + `UPDATE
     run_log_archives SET trimmed_at = NOW() WHERE run_id = $1`. Returns
     rows deleted.
- Failures are logged and skipped; retried next tick.

#### Archive coverage verification

Verifying the archive *object* exists is not enough: the archiver's
`TailLogs(ctx, runID, 0, 1_000_000)` caps at one million lines, and nothing
stops an agent flushing more log lines after archival (`ListRunsNeedingArchival`
never re-archives once a `run_log_archives` record exists). Either case would
let the sweeper irreversibly delete rows no archive object has a copy of.

`run_log_archives` carries `line_count bigint NOT NULL DEFAULT 0` and
`max_seq bigint NOT NULL DEFAULT 0`, set by `CreateLogArchive` to exactly
what the archive object covers (`len(lines)` and the last line's `seq`).
DEFAULT 0 is the safe direction — an existing record with unknown coverage
never qualifies for trimming while any logs remain.

`TrimRunLogs` checks coverage IN THE SAME TRANSACTION as the delete, after
the `trimmed_at` guard succeeds: `SELECT COUNT(*), COALESCE(MAX(seq), 0)
FROM logs WHERE run_id = $1`, compared against the record's `line_count` /
`max_seq`. If the live table has more rows or a higher seq than the archive
claims, the whole transaction rolls back (including the `trimmed_at` mark)
and `store.ErrArchiveIncomplete` is returned. Doing this inside the
transaction — rather than as a pre-check before it — closes the race against
a late agent flush landing between the sweeper's earlier checks and the
trim itself.

The sweeper treats `errors.Is(err, store.ErrArchiveIncomplete)` as warn +
skip: no progress, and — unlike the missing-object case — no record
deletion, since deleting the record would just make the archiver re-archive
(and still under-cover) the same oversized run forever.

### Archive-backed log reader

New file `internal/controller/archived_logs.go`:

- `archivedLogs` type owning the object store handle and a **byte-bounded LRU
  cache** (`archivedLogsCacheBytes = 128 MiB` constant) of parsed
  `[]api.LogLine` per run ID. Entries are immutable (runs are terminal), so
  caching is safe; an archive larger than the cache cap is decoded per
  request and not cached.
- `load(ctx, runID)` — `obj.Get(runLogArchiveKey(runID))`, ndjson-decode into
  `[]api.LogLine` (the archive preserves seq/stepIndex/stream/timestamp/line,
  so all contracts below are exactly reproducible).
- Contract methods over the in-memory slice, byte-compatible with the store:
  - `tail(afterSeq int64, limit int)` — mirrors `TailLogs` (`seq > afterSeq`,
    ascending, LIMIT).
  - `tailRecent(limit int)` — mirrors `TailLogsRecent` (last N ascending).
  - `count(steps []int)` — mirrors `CountLogs` (count, minSeq, maxSeq over
    the step-filtered view; zeros for an empty view).
  - `logRange(steps []int, offset, limit int)` — mirrors `ListLogsRange`
    (step filter, seq order, offset/limit).
  - `search(steps []int, q string, capN int)` — mirrors `SearchLogs`:
    row numbers are 0-based positions in the step-filtered view **before**
    the match filter; matching is case-insensitive literal substring
    (the Go equivalent of `escapeILIKE` + `ILIKE`); results seq-ordered,
    capped at `capN` with the uncapped total returned.

### Handler fallback wiring

Each log read path branches at the top on one cheap PK lookup
(`GetLogArchive`): if the record exists **and** `TrimmedAt != nil`, serve
from `archivedLogs`; otherwise the existing DB path runs unchanged.

- `handleTailLogs` (`GET /runs/{id}/logs?after=N`) — CLI `logs`, `run wait
  --follow`.
- `handleLogStats`, `handleLogRange`, `handleLogSearch` — WebUI windowed
  viewer.
- SSE (`internal/controller/sse.go`) backfill — replace the
  `TailLogsRecent` call's result with `tailRecent(sseBackfillLimit+1)` for
  trimmed runs; the run is terminal so no live lines follow, and the rest of
  the SSE flow (truncated marker, stream lifecycle) is unchanged.
- `handleLogsArchive` is already archive-backed; unchanged.
- Error mapping: object-store failure or missing object on a trimmed run
  returns 503 with a distinct message (`log archive unavailable`) so
  operators can tell storage outages from bugs; WebUI surfaces its normal
  fetch-error state.

### Configuration

- `--log-trim-days` / `UNIFIED_LOG_TRIM_DAYS`, default 0 = never trim,
  resolver mirroring `runRetentionDaysDefault()`.
- Startup logs enabled/disabled. If both this and `--run-retention-days` are
  set and `logTrimDays >= runRetentionDays`, log a startup **warning** (trim
  would never fire — retention deletes the runs first); not an error.

### Interaction with run retention (PR #44)

- `deleteRunEverywhere` needs no changes: for a trimmed run the `logs`
  cascade is a no-op and the archive object/record deletion already works.
- Retention deleting a run evicts nothing from the reader cache eagerly;
  a subsequent read 404s at `GetRun`/`GetLogArchive` before touching the
  cache, and the LRU eventually drops the entry. Bound: one stale slice per
  deleted run until evicted — acceptable.
- The trim sweeper and retention sweeper never contend destructively: both
  are idempotent and `TrimRunLogs` on a retention-deleted run is a 0-row
  no-op (the archive record is already gone, so it won't even be a
  candidate).

### Docs

- `docs/configuration.md`: new flag/env, relationship to
  `--run-retention-days`.
- `docs/operations.md`: extend the "Run retention" paragraph into the full
  tiering story (hot DB rows → warm archive reads → deleted at retention);
  note the first-view latency for trimmed runs and the 128 MiB reader cache.
- `docs/high-availability.md`: leader table row + advisory-key list.
- No DSL change; `field-reference.md`, `schemas/`, `examples/`, `templates/`
  untouched.

## Out of scope

- Keeping archives *past* run retention (retention still deletes archives
  with the run; archive-only long-term tiers would be a separate feature).
- Changing the archive format (single ndjson object; no per-run index or
  chunking — a 1M-line archive decodes per request or caches whole).
- WebUI changes of any kind (full read compatibility is the point).
- pg_trgm search indexing, regex search (unchanged from the viewer design).
- Trimming logs of runs that were never archived (no object store, archiver
  failure): their rows stay until run retention deletes the run.

## Testing

- Store: `TrimRunLogs` transactionality (rows gone + `trimmed_at` set
  atomically; 0-row no-op on unknown run), `ListTrimCandidates` (cutoff
  boundary, `trimmed_at IS NULL` filter, LIMIT, oldest-first), migration 012
  sentinel.
- Reader: golden-parity tests — for a seeded log set, every contract method
  must return byte-identical results to the store methods over the same
  data (including ILIKE-escape cases: queries containing `%`, `_`, `\`,
  mixed case; step filters; offset/limit boundaries; afterSeq paging).
  Cache: LRU eviction by bytes, oversized-archive bypass.
- Sweeper: mirror run_retention tests (follower no-op, disabled, batch
  draining, zero-progress stop, missing-object skip + stale-record cleanup).
- Handlers: HTTP tests with a trimmed run served from `LocalObjectStore` —
  stats/range/search/tail identical before and after trim (trim in the test,
  compare responses); SSE backfill for a trimmed run; 503 on object-store
  failure.
- Regression: retention deleting a trimmed run still removes the archive
  object (existing tests cover; add one combined trim→retention test).
