# Batch Log Ingestion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the controller's one-round-trip-per-log-line write path with a batched insert, so a bulk log request from an agent costs a constant number of database round trips instead of two per line.

**Architecture:** A new `AppendLogs` method on the `Store` interface takes a slice of lines and groups them by run. Each run gets one multi-row `INSERT … SELECT … FROM unnest(…) WITH ORDINALITY`, keeping the existing sealed-run `WHERE NOT EXISTS` guard, and one `pg_notify` — instead of two statements per line. `handleAgentLogBulk` calls it once per request. The single-line `AppendLog` stays for the callers that legitimately write one line.

**Tech Stack:** Go, pgx v5, PostgreSQL. Integration tests use `NewTestPostgres(t)`, which requires Docker and is skipped under `-short`.

**Spec:** `docs/superpowers/specs/2026-08-25-batch-log-ingestion-design.md`

## Global Constraints

- **Do not change `AppendLog`.** It keeps its exact signature and behaviour. Its remaining callers — the claim-failure stderr path, the queued-run reaper, the scheduler — legitimately write one line, and the spec keeps it in scope explicitly.
- **A sealed run's lines are dropped and return `seq == 0`.** A run with a row in `run_log_archives` accepts no appends. Real seqs start at 1, so 0 is unambiguous.
- **No notification for dropped lines.** A run whose lines were all dropped must produce no `pg_notify` for that run. SSE clients stay consistent with what readers can actually see.
- **`seq` must be monotonic in input order.** `TailLogs(afterSeq)` pages on it; out-of-order seqs make an SSE client skip or repeat lines.
- **A failed notification does not fail the append.** Today's `_, _ = p.pool.Exec(...)` discards the error deliberately. Keep that, and keep a comment saying why — silent error discard otherwise reads as a bug.
- **Do not add a capability clause to `ListUnclaimableQueuedRuns`.** Unrelated to this change, but the file carries an explicit comment saying so; leave it alone.

---

## Resolved design question

The spec's §4.2 left one mechanism open: when part of a batch is dropped, `RETURNING seq` yields rows for inserted lines only, so the returned seqs no longer align positionally with the input. The spec required the plan to pick a mechanism and prove it.

**Decision: group the batch by run before inserting.**

Sealed-ness is a property of the *run*, not the line, and `run_log_archives` does not change within a single statement's snapshot. So every line of a given run in a given batch shares one verdict. Insert one run's lines per statement and the returned row count is unambiguous:

- **0 rows** — the run is sealed; every one of its lines gets `seq = 0`; no notify.
- **exactly `len(lines)` rows** — the run is live; pair the ascending seqs with that run's lines in input order; notify once.

There is no third outcome, so there is no positional-alignment problem to solve and no check-then-insert race to guard against. The `WHERE NOT EXISTS` predicate stays inside the INSERT, which is what actually enforces the seal.

Cost: one INSERT plus one notify per *distinct run* in the batch. A bulk request from one agent is almost always one run, so the common case is two round trips per request instead of two per line.

`ORDER BY ord` in the INSERT's SELECT fixes the order in which rows are produced, and therefore the order seqs are assigned. Task 1 Step 7 proves this by reading the rows back and asserting the stored line at each seq matches the input at that position — rather than assuming it.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/store/store.go` | `LogAppend` struct and the `AppendLogs` interface method + doc comment |
| `internal/store/postgres.go` | `AppendLogs` implementation: group by run, one INSERT + one notify per run |
| `internal/store/postgres_log_batch_test.go` | **new** — integration tests for the batch path |
| `internal/controller/queuedrun_reaper_test.go` | the fake store gains `AppendLogs` to keep satisfying the interface |
| `internal/controller/api_agent.go` | `handleAgentLogBulk` makes one batch call |
| `internal/controller/api_agent_logbulk_test.go` | **new** — asserts one batch call per request, with a counting fake |

---

### Task 1: The batch write

**Files:**
- Modify: `internal/store/store.go` (the `Store` interface, near the existing `AppendLog` declaration at :244-247)
- Modify: `internal/store/postgres.go` (add `AppendLogs` immediately after `AppendLog`, which ends at :1038)
- Modify: `internal/controller/queuedrun_reaper_test.go` (its fake store implements `Store`)
- Test: `internal/store/postgres_log_batch_test.go` (new)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `store.LogAppend` and `Store.AppendLogs(ctx, []LogAppend) ([]int64, error)`. Task 2 consumes both.

- [ ] **Step 1: Add the type and the interface method**

In `internal/store/store.go`, directly above the `AppendLog` declaration, add the struct:

```go
// LogAppend is one line for the batch append path. The fields mirror
// AppendLog's parameters exactly.
type LogAppend struct {
	RunID     string
	StepIndex int
	Stream    string
	Timestamp time.Time
	Line      string
}
```

and directly below `AppendLog`, add to the `Store` interface:

```go
	// AppendLogs stores multiple log lines and notifies SSE listeners once
	// per run that had at least one line written. The returned slice is
	// parallel to lines: element i is the seq assigned to lines[i], or 0 if
	// that line was DROPPED because its run is sealed — the same convention
	// AppendLog uses. Lines for different runs may be mixed freely.
	AppendLogs(ctx context.Context, lines []LogAppend) ([]int64, error)
```

Check that `time` is already imported in `store.go`; add it if not.

- [ ] **Step 2: Write the failing tests**

Create `internal/store/postgres_log_batch_test.go`:

```go
package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRun creates a job and a run, returning the run ID.
func newRun(t *testing.T, pg *Postgres, job string) string {
	t.Helper()
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, job, "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, job, nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	return run.ID
}

// TestPostgres_AppendLogs_MixedSealedAndLive is the core mapping test the
// design calls for: a batch straddling a sealed run and a live one must
// return zeros for the sealed run's lines and ascending seqs for the live
// one, positionally aligned with the input.
func TestPostgres_AppendLogs_MixedSealedAndLive(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	live := newRun(t, pg, "live")
	sealed := newRun(t, pg, "sealed")

	// Seal the second run.
	seq, err := pg.AppendLog(ctx, sealed, 0, "stdout", time.Now(), "before seal")
	require.NoError(t, err)
	require.NoError(t, pg.CreateLogArchive(ctx, sealed, "runs/"+sealed+"/logs.ndjson", 1, 1, seq))

	now := time.Now().UTC()
	// Interleave the two runs so a per-run grouping bug shows up as a
	// misalignment rather than a clean split.
	lines := []LogAppend{
		{RunID: live, StepIndex: 0, Stream: "stdout", Timestamp: now, Line: "live-1"},
		{RunID: sealed, StepIndex: 0, Stream: "stdout", Timestamp: now, Line: "dropped-1"},
		{RunID: live, StepIndex: 0, Stream: "stdout", Timestamp: now, Line: "live-2"},
		{RunID: sealed, StepIndex: 0, Stream: "stdout", Timestamp: now, Line: "dropped-2"},
		{RunID: live, StepIndex: 1, Stream: "stderr", Timestamp: now, Line: "live-3"},
	}

	seqs, err := pg.AppendLogs(ctx, lines)
	require.NoError(t, err)
	require.Len(t, seqs, len(lines))

	assert.Zero(t, seqs[1], "sealed run's line must be dropped")
	assert.Zero(t, seqs[3], "sealed run's line must be dropped")
	assert.Positive(t, seqs[0])
	assert.Positive(t, seqs[2])
	assert.Positive(t, seqs[4])
	assert.Less(t, seqs[0], seqs[2], "seqs must ascend in input order")
	assert.Less(t, seqs[2], seqs[4], "seqs must ascend in input order")

	// Nothing landed in the sealed run beyond its pre-seal line.
	count, _, _, err := pg.CountLogs(ctx, sealed, nil)
	require.NoError(t, err)
	assert.EqualValues(t, 1, count)

	// The live run has exactly the three lines, in order.
	got, err := pg.TailLogs(ctx, live, 0, 100)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, "live-1", got[0].Line)
	assert.Equal(t, "live-2", got[1].Line)
	assert.Equal(t, "live-3", got[2].Line)
	assert.EqualValues(t, 1, got[2].StepIndex)
	assert.Equal(t, "stderr", got[2].Stream)
}

// TestPostgres_AppendLogs_AllSealed: a batch whose every line belongs to a
// sealed run returns all zeros and stores nothing.
func TestPostgres_AppendLogs_AllSealed(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	run := newRun(t, pg, "j")

	seq, err := pg.AppendLog(ctx, run, 0, "stdout", time.Now(), "before seal")
	require.NoError(t, err)
	require.NoError(t, pg.CreateLogArchive(ctx, run, "runs/"+run+"/logs.ndjson", 1, 1, seq))

	seqs, err := pg.AppendLogs(ctx, []LogAppend{
		{RunID: run, StepIndex: 0, Stream: "stdout", Timestamp: time.Now().UTC(), Line: "a"},
		{RunID: run, StepIndex: 0, Stream: "stdout", Timestamp: time.Now().UTC(), Line: "b"},
	})
	require.NoError(t, err)
	assert.Equal(t, []int64{0, 0}, seqs)

	count, _, _, err := pg.CountLogs(ctx, run, nil)
	require.NoError(t, err)
	assert.EqualValues(t, 1, count)
}

// TestPostgres_AppendLogs_Empty: an empty batch is a no-op, not an error and
// not a round trip. handleAgentLogBulk can receive an empty array.
func TestPostgres_AppendLogs_Empty(t *testing.T) {
	pg := NewTestPostgres(t)
	seqs, err := pg.AppendLogs(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, seqs)
}

// TestPostgres_AppendLogs_OrderIsProvenNotAssumed inserts a large batch and
// reads it back, asserting the line stored at the i-th ascending seq is the
// i-th input line. This is what proves `ORDER BY ord` actually fixes the
// order seqs are assigned in — the plan's one load-bearing assumption.
func TestPostgres_AppendLogs_OrderIsProvenNotAssumed(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	run := newRun(t, pg, "j")

	const n = 500
	lines := make([]LogAppend, n)
	now := time.Now().UTC()
	for i := range lines {
		lines[i] = LogAppend{
			RunID: run, StepIndex: 0, Stream: "stdout",
			Timestamp: now, Line: fmt.Sprintf("line-%03d", i),
		}
	}

	seqs, err := pg.AppendLogs(ctx, lines)
	require.NoError(t, err)
	require.Len(t, seqs, n)
	for i := 1; i < n; i++ {
		require.Less(t, seqs[i-1], seqs[i], "seq must ascend at index %d", i)
	}

	got, err := pg.TailLogs(ctx, run, 0, n+10)
	require.NoError(t, err)
	require.Len(t, got, n)
	for i := range got {
		require.Equal(t, lines[i].Line, got[i].Line, "line at position %d", i)
		require.Equal(t, seqs[i], got[i].Seq, "seq at position %d", i)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/store/ -run AppendLogs -count=1`
Expected: FAIL — `pg.AppendLogs undefined (type *Postgres has no field or method AppendLogs)`.

If the package does not compile at all because the interface method has no implementation, that is the same failure and is fine.

- [ ] **Step 4: Implement `AppendLogs`**

In `internal/store/postgres.go`, immediately after `AppendLog`:

```go
// AppendLogs stores many log lines with a constant number of round trips per
// run, instead of AppendLog's two per line. It is the hot path for an agent's
// bulk log upload; AppendLog remains for the callers that write a single line.
//
// The batch is grouped by run because SEALING IS A PROPERTY OF THE RUN, not of
// the line: run_log_archives does not change within one statement's snapshot,
// so every line of a run in a batch shares one verdict. That makes the row
// count returned by each INSERT unambiguous — zero (the run is sealed) or all
// of them — which is what lets the returned seqs be mapped back to the input
// positionally without relying on any ordering guarantee across runs.
//
// ORDER BY ord fixes the order rows are produced, and therefore the order the
// seq sequence is drawn in, so the ascending seqs pair with that run's lines in
// input order. postgres_log_batch_test.go proves this by reading the rows back
// rather than assuming it.
func (p *Postgres) AppendLogs(ctx context.Context, lines []store.LogAppend) ([]int64, error) {
	if len(lines) == 0 {
		return nil, nil
	}

	// Group line positions by run, preserving input order within each run.
	order := make([]string, 0, 4)
	byRun := make(map[string][]int, 4)
	for i, l := range lines {
		if _, ok := byRun[l.RunID]; !ok {
			order = append(order, l.RunID)
		}
		byRun[l.RunID] = append(byRun[l.RunID], i)
	}

	const q = `
		INSERT INTO logs(run_id, step_index, stream, ts, line)
		SELECT $1::uuid, s.step_index, s.stream, s.ts, s.line
		FROM unnest($2::int[], $3::text[], $4::timestamptz[], $5::text[])
			WITH ORDINALITY AS s(step_index, stream, ts, line, ord)
		WHERE NOT EXISTS (SELECT 1 FROM run_log_archives WHERE run_id = $1::uuid)
		ORDER BY s.ord
		RETURNING seq;
	`

	out := make([]int64, len(lines))
	for _, runID := range order {
		idx := byRun[runID]
		steps := make([]int32, len(idx))
		streams := make([]string, len(idx))
		stamps := make([]time.Time, len(idx))
		texts := make([]string, len(idx))
		for j, i := range idx {
			steps[j] = int32(lines[i].StepIndex)
			streams[j] = lines[i].Stream
			stamps[j] = lines[i].Timestamp
			texts[j] = lines[i].Line
		}

		rows, err := p.pool.Query(ctx, q, runID, steps, streams, stamps, texts)
		if err != nil {
			return nil, fmt.Errorf("append logs for run %s: %w", runID, err)
		}
		seqs := make([]int64, 0, len(idx))
		for rows.Next() {
			var seq int64
			if err := rows.Scan(&seq); err != nil {
				rows.Close()
				return nil, fmt.Errorf("append logs for run %s: %w", runID, err)
			}
			seqs = append(seqs, seq)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("append logs for run %s: %w", runID, err)
		}

		// Zero rows means the run is sealed and every one of its lines was
		// dropped; out[i] is already 0 for those. Any other short count would
		// mean the per-run invariant above is wrong, so say so loudly rather
		// than returning a silently misaligned mapping.
		if len(seqs) == 0 {
			continue
		}
		if len(seqs) != len(idx) {
			return nil, fmt.Errorf("append logs for run %s: inserted %d of %d lines", runID, len(seqs), len(idx))
		}
		for j, i := range idx {
			out[i] = seqs[j]
		}

		// One notification per run that had a line written. The payload keeps
		// AppendLog's shape (the highest seq); no reader parses it — the SSE
		// handler uses its own lastSeq — but changing a wire format for no
		// gain is worse than leaving it. The error is discarded deliberately,
		// exactly as in AppendLog: the lines are written, which is what
		// matters, and a failed wake-up costs a delayed refresh, not data.
		_, _ = p.pool.Exec(ctx, "SELECT pg_notify($1, $2)",
			"log_appended:"+runID, fmt.Sprintf("%d", seqs[len(seqs)-1]))
	}
	return out, nil
}
```

Note the parameter type is `store.LogAppend` only if `postgres.go` is in a different package than `store.go`. **Check first:** `postgres.go` starts with `package store`, in which case the type is written bare as `LogAppend` and there is no import to add. Use whichever matches the file.

- [ ] **Step 5: Add `AppendLogs` to the fake store**

`internal/controller/queuedrun_reaper_test.go` holds a fake that implements `Store`. Find its `AppendLog` method and add beside it:

```go
func (f *fakeStore) AppendLogs(ctx context.Context, lines []store.LogAppend) ([]int64, error) {
	out := make([]int64, len(lines))
	for i, l := range lines {
		seq, err := f.AppendLog(ctx, l.RunID, l.StepIndex, l.Stream, l.Timestamp, l.Line)
		if err != nil {
			return nil, err
		}
		out[i] = seq
	}
	return out, nil
}
```

Use the fake's actual receiver name and type; `fakeStore` above is a placeholder for whatever it is called. If the fake embeds `store.Store` rather than implementing every method, no change is needed — check before editing.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/store/ -run AppendLogs -count=1`
Expected: PASS, 4 tests. Docker must be running; `NewTestPostgres` needs it.

- [ ] **Step 7: Run the existing seal and log tests unchanged**

Run: `go test ./internal/store/ -run 'Log|Seal' -count=1`
Expected: PASS. In particular `TestPostgres_AppendLog_SealedAfterArchive` must be untouched and still green — `AppendLog` did not change.

Then: `go build ./... && go test ./... -short -count=1`
Expected: build clean, short suite green.

- [ ] **Step 8: Commit**

```bash
git add internal/store/store.go internal/store/postgres.go internal/store/postgres_log_batch_test.go internal/controller/queuedrun_reaper_test.go
git commit -m "feat(store): add AppendLogs, a batched log write grouped by run"
```

---

### Task 2: The caller

**Files:**
- Modify: `internal/controller/api_agent.go:729-751` (the append loop inside `handleAgentLogBulk`)
- Test: `internal/controller/api_agent_logbulk_test.go` (new)

**Interfaces:**
- Consumes: `store.LogAppend` and `Store.AppendLogs` from Task 1.
- Produces: nothing later tasks depend on.

**Do not touch the ownership-guard pass** at the top of `handleAgentLogBulk`. It deliberately runs in its own loop over distinct run IDs so a mixed-ownership batch is rejected before any line lands, and its comment says so. Only the append loop below it changes.

- [ ] **Step 1: Write the failing test**

Create `internal/controller/api_agent_logbulk_test.go`. It needs the package's existing test-server helper — find how a neighbouring test in `internal/controller` builds a `Server` with a fake store and an authenticated agent, and follow that exact pattern rather than inventing one.

The assertion that matters:

```go
// TestHandleAgentLogBulk_OneBatchCall is the specific regression this change
// exists to prevent recurring: the agent batches, and the controller must not
// unbatch. A counting fake proves the handler makes ONE store call for a
// request carrying many lines, not one per line.
func TestHandleAgentLogBulk_OneBatchCall(t *testing.T) {
	// ... build the server with a store fake that counts AppendLogs calls,
	// records the slice it received, and returns ascending seqs ...

	const n = 25
	// ... POST n lines for one run to the bulk endpoint ...

	assert.Equal(t, 1, fake.appendLogsCalls, "handler must batch, not loop")
	assert.Equal(t, 0, fake.appendLogCalls, "handler must not use the single-line path")
	assert.Len(t, fake.lastBatch, n)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}
```

Add a second test for the sealed case:

```go
// TestHandleAgentLogBulk_SealedRunWarnsOnce: when the store reports every line
// dropped, the handler still returns 204 and counts the drops.
func TestHandleAgentLogBulk_SealedRunDropped(t *testing.T) {
	// ... fake returns all zeros ...
	assert.Equal(t, http.StatusNoContent, rec.Code)
	// nothing is asserted about the log line itself; the point is that an
	// all-dropped batch is not an error
}
```

And a third for the empty body, since the handler can receive `[]`:

```go
func TestHandleAgentLogBulk_EmptyBatch(t *testing.T) {
	// POST `[]`
	assert.Equal(t, http.StatusNoContent, rec.Code)
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/controller/ -run HandleAgentLogBulk -count=1`
Expected: FAIL — `appendLogsCalls` is 0 and `appendLogCalls` is 25, because the handler still loops.

- [ ] **Step 3: Replace the append loop**

In `handleAgentLogBulk`, replace everything from `dropped := 0` through the `for _, req := range lines { ... }` loop with:

```go
	batch := make([]store.LogAppend, len(lines))
	for i, req := range lines {
		if req.Timestamp.IsZero() {
			req.Timestamp = time.Now().UTC()
		}
		batch[i] = store.LogAppend{
			RunID:     req.RunID,
			StepIndex: req.StepIndex,
			Stream:    req.Stream,
			Timestamp: req.Timestamp,
			Line:      req.Line,
		}
	}
	seqs, err := s.store.AppendLogs(r.Context(), batch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// A zero seq means the line was dropped because its run is sealed. Count
	// them and warn once, as the per-line loop this replaced did.
	dropped := 0
	var droppedRun string
	for i, seq := range seqs {
		if seq == 0 {
			dropped++
			droppedRun = batch[i].RunID
		}
	}
	if dropped > 0 {
		slog.Warn("dropping log lines for sealed run", "run", droppedRun, "dropped", dropped)
	}
```

Leave the trailing `w.WriteHeader(http.StatusNoContent)` as it is.

Check that `internal/store` is imported in `api_agent.go`; add it if not.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/controller/ -run HandleAgentLogBulk -count=1`
Expected: PASS, 3 tests.

- [ ] **Step 5: Run the log-related controller tests unchanged**

Run: `go test ./internal/controller/ -run 'Log|Trim|Archive' -count=1`
Expected: PASS. `api_logwindow_test.go` and `log_trim_test.go` depend on seq and window semantics and must be untouched.

Then: `go build ./... && go vet ./... && go test ./... -short -count=1`
Expected: all clean.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/api_agent.go internal/controller/api_agent_logbulk_test.go
git commit -m "perf(controller): append a bulk log request in one batch, not one line at a time"
```

---

## Not a task: the measurement

The spec's stage 3 is a before/after measurement using the load-test harness in `gcp/yuichiro.arima/loadtest/`, which is **not in this repository**. It cannot be a task here.

The acceptance bar the load test stated: a 200,000-line job completing in seconds to minutes rather than 44, with the fifty execution slots not exhausting. Record it as an owner-run step after the branch merges.

## Not a task: the default timeout

Making ingestion faster shortens the talkative job; it does not bound it. The change that guarantees the fleet survives is a default `timeoutMinutes`, which is out of scope here by design — it changes the behaviour of every existing job that omits one, and needs its own design, its own decision about the default value, and its own migration note.

Nothing in this plan should try to sneak it in.
