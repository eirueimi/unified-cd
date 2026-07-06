# Run Detail: Show Executing Agent — Design

Date: 2026-07-06
Status: Approved

## Goal

Show, on the Run Detail page in the Web UI, which agent claimed and executed a
run, linked to that agent's existing detail page.

## Background

The `runs` table already records `claimed_by` (the claiming agent's ID) and
`claimed_at`, set atomically at claim time (`internal/store/postgres.go:530`).
This data is never surfaced: `GetRun` does not select it, the `api.Run` struct
has no field for it, and the UI therefore cannot display it. The `/agents/:id`
detail page already exists and is linked from the Agents monitor. This change
threads the existing value through to the UI — no new columns, no new
endpoints.

## Non-goals

- A run-list Agent column (Run Detail only, per scope decision).
- Showing `claimed_at` as a separate field (out of scope; the value exists but
  is not requested).
- Real-time updates of the agent field mid-run (it is set once at claim and
  does not change; the page's normal run refresh already carries it).

## Design

Data path, three edits plus tests:

1. **API type** — `internal/api/types.go`, `Run` struct: add
   `ClaimedBy string ` + json tag `claimedBy,omitempty`. Empty string means the
   run was never claimed (Pending/Queued, or failed before claim).

2. **Store** — `internal/store/postgres.go`, `GetRun` (line 197): add
   `claimed_by` to the SELECT column list; scan it via `sql.NullString` (the
   column is NULL until claim) and copy `.String` into `r.ClaimedBy` when
   valid. No other `GetRun` behavior changes.

3. **UI** — `web/src/routes/RunDetail.svelte`, metadata block (near the
   "Triggered by" field, ~line 498): add an "Agent" metadata row guarded by
   `{#if run.claimedBy}`, rendering a link to the agent detail page in the same
   style as the existing `calledBy` breadcrumb:
   `<a href="#/agents/{run.claimedBy}">{run.claimedBy} ↗</a>`.

## Edge cases

- **Unclaimed run**: `claimedBy` empty → the Agent row is not rendered.
- **Agent row GC'd**: `claimed_by` is stored as a plain string on the run, so
  it survives stale-agent garbage collection. The ID still displays; the link
  may 404, which the AgentDetail page already handles with a not-found state.
  Surfacing the ID retains audit value even when the agent is gone.

## Testing

- **Store**: extend the Postgres-backed run tests — a claimed run returns its
  `ClaimedBy`; an unclaimed run returns `""`. Use the existing
  `NewTestPostgres` clone pattern and the claim path that already sets
  `claimed_by`.
- **UI**: in `web/src/routes/RunDetail.test.js`, assert the Agent link renders
  (with the correct `#/agents/<id>` href) when the mocked run has `claimedBy`,
  and is absent when it does not.
