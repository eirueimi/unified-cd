# Batch log ingestion — Design

Date: 2026-08-25
Status: Approved (design); implementation plan to follow

## 1. Purpose

A load test on GKE — two controllers, two agents, fifty concurrent runs, a
Cloud SQL instance with two vCPUs — measured log ingestion at **about 76 lines
per second**. A dedicated benchmark against the same instance with nothing else
running reached 248. A batched insert against that same instance reached
**50,000**.

The difference is not the database. It is the write pattern, and the gap is
roughly two hundredfold.

The consequence is not slowness. A job producing 200,000 log lines takes about
**44 minutes**, and holds an execution slot for all of it. In the load test
roughly one run in fifty was such a job; within about thirty minutes every one
of the fifty slots was occupied by one, and **every other job queued forever**.
Because the jobs carried no `timeoutMinutes`, nothing rescued them.

One talkative job can take the whole fleet hostage.

## 2. Where the cost is

Three sources, measured and then confirmed in the code.

**Two round trips per line.** `Postgres.AppendLog`
(`internal/store/postgres.go:1019-1038`) runs an `INSERT ... SELECT ... WHERE
NOT EXISTS (SELECT 1 FROM run_log_archives ...) RETURNING seq`, then a second,
separate `SELECT pg_notify($1, $2)`. Two hundred thousand lines is four hundred
thousand round trips.

**The agent already batches; the controller unbatches.** `AppendLogBulk`
(`internal/agent/client.go`) posts many lines in one request, and
`handleAgentLogBulk` (`internal/controller/api_agent.go:733-741`) receives them
and loops, calling `AppendLog` once per line. The batch arrives and is
immediately taken apart. This is the highest-value line in the whole change.

**Read amplification per notify.** The SSE handler listens on
`log_appended:{runID}` and, on every notification, issues `TailLogs` and
`GetRun` (`internal/controller/sse.go:117-125`). With K viewers on a run, one
log line costs 2K additional queries.

The notification's payload is never read — the callback takes `payload string`
and uses `lastSeq` instead. **The notify is a wake signal, nothing more**, and
that is what makes coalescing it safe rather than a trade-off.

## 3. Scope

In scope:

- A batch write on the `Store` interface, and `handleAgentLogBulk` using it.
- One notification per batch per run, instead of one per line.
- Keeping the single-line `AppendLog`, which has callers that legitimately
  write one line (a claim failure's stderr, the queued-run reaper, the
  scheduler).

Out of scope:

- **A default job timeout.** It is the change that actually breaks the
  deadlock, and it deserves its own design — see §7. This one makes the fleet
  fast; that one makes it survivable. They are not the same decision and should
  not ride together.
- **SSE debounce.** Coalescing notifies at the write side already removes most
  of the amplification. A time-based debounce on the read side is a further
  step with its own latency trade-off, and it should be measured after this
  lands rather than guessed at now.
- **A larger database, or AlloyDB.** Rejected on evidence, not preference: the
  batched benchmark hit 50,000 lines per second *on the instance already in
  use*. The hardware is not the constraint; the round trips are. And `NOTIFY`
  serialises through a single global queue in PostgreSQL, which AlloyDB
  inherits — so the one mechanism most likely to contend does not improve by
  changing engines.

## 4. The change

### 4.1 A batch method on the Store

```go
// AppendLogs stores multiple log lines in one round trip. The returned slice
// is parallel to lines: element i is the seq assigned to lines[i], or 0 if
// that line was dropped because its run is sealed.
AppendLogs(ctx context.Context, lines []LogAppend) ([]int64, error)
```

`LogAppend` carries what `AppendLog` takes today: run ID, step index, stream,
timestamp, line.

Returning a parallel slice with 0 for a dropped line preserves the convention
`handleAgentLogBulk` already uses — it counts zeros and warns once about the
sealed run.

### 4.2 The insert

A multi-row `INSERT ... SELECT ... FROM unnest($1, $2, ...)` keeping the
existing `WHERE NOT EXISTS (SELECT 1 FROM run_log_archives ...)` predicate, so
the sealed rule stays exactly what it is today and costs a join rather than a
round trip.

**`pgx.CopyFrom` is rejected**, despite matching the batched insert's throughput
in the benchmark, because it cannot return the assigned seqs. The seqs are not
decoration: they are how a dropped line is distinguished from a written one,
and dropping that distinction breaks §5's first two invariants.

**One thing the plan must nail down and test.** `RETURNING seq` yields rows for
inserted lines only, so when part of a batch is dropped the returned seqs no
longer align positionally with the input. The mapping from input line to seq
must be explicit and covered by a test that feeds a batch mixing a sealed run
and a live one and asserts the exact returned slice. Two mechanisms are
plausible — evaluating sealed-ness per distinct run first and then inserting
only live rows, or carrying an ordinality through the statement — and the plan
picks one and proves it rather than relying on PostgreSQL returning rows in
source order, which is not guaranteed.

### 4.3 The notification

One `pg_notify` per run per batch, after the insert, naming only runs that had
at least one line written. Its payload keeps today's shape; no reader parses
it, but changing it gains nothing and an unparsed payload is not a reason to
churn a wire format.

## 5. Invariants that must not break

These come from the existing behaviour and are the reason this change is
delicate rather than mechanical.

1. **A sealed run's lines are dropped.** A run with a row in `run_log_archives`
   accepts no appends. Today this returns `seq == 0` and `handleAgentLogBulk`
   counts those and logs `dropping log lines for sealed run`.
2. **No notification for dropped lines.** The existing comment says why: SSE
   clients stay consistent with what readers can actually see. A batch that
   wrote nothing for a run must not notify for that run.
3. **`seq` is monotonic.** `TailLogs(afterSeq)` pages on it. Out-of-order seqs
   make an SSE client skip or repeat lines.
4. **A failed notification does not fail the append.** Today's
   `_, _ = p.pool.Exec(...)` discards the error deliberately — the line is
   written and that is what matters. The batch version keeps that, and says so
   in a comment, because silent error discard otherwise reads as a bug.
5. **Log trim and archival keep working.** `internal/controller/log_trim.go`,
   `internal/controller/archived_logs.go`, and `api_logwindow_test.go` all
   depend on seq and window semantics.

## 6. Verification

1. Existing tests pass untouched — in particular
   `internal/store/postgres_log_seal_test.go` (the sealed-drop behaviour),
   `internal/controller/api_logwindow_test.go`, and
   `internal/controller/log_trim_test.go`.
2. A batch mixing a sealed run and a live run returns the exact expected
   parallel slice: zeros for the sealed run's lines, ascending seqs for the
   live one.
3. A batch produces exactly one notification per run that had a line written,
   and none for a run whose lines were all dropped.
4. `handleAgentLogBulk` calls the batch method once per request, not once per
   line. This is worth asserting directly with a counting fake, because it is
   the specific regression this change exists to prevent recurring.
5. The single-line `AppendLog` still works for its remaining callers.
6. `go build ./...` and `go test ./... -short -count=1` pass. The full suite
   (Docker required) covers the store integration tests.
7. **A measured before/after.** The benchmark harness lives outside this
   repository, in `gcp/yuichiro.arima/loadtest/`. The acceptance bar stated by
   the load test is a 200,000-line job completing in seconds to minutes rather
   than 44, with the fifty slots not exhausting. This design cannot verify that
   from here; the plan records it as an owner-run step.

## 7. What this does not fix

The deadlock had two causes, and this addresses one of them.

Making ingestion two hundred times faster shortens the talkative job from 44
minutes to seconds. It does not bound it. A job producing ten million lines
still occupies its slot for as long as it takes, and nothing stops it — because
the jobs in the load test carried no `timeoutMinutes` and nothing supplied a
default.

**The change that guarantees the fleet survives is a default timeout**, not
this one. It is out of scope here because it changes the behaviour of every
existing job that omits `timeoutMinutes` — a run that used to take as long as
it needed would start failing — and that is a breaking change deserving its own
design, its own decision about the default value, and its own migration note.

Recording it here so the sequencing is deliberate: this change removes the
cause; the timeout change limits the damage when something else causes it
again. Both are wanted. Only the second one is a guarantee.

## 8. Staging

1. **The batch method.** `LogAppend`, the `Store` interface addition, the
   Postgres implementation, and the fake in
   `internal/controller/queuedrun_reaper_test.go` that must follow the
   interface. Tests for the seal mapping and the notification count. Nothing
   calls it yet.
2. **The caller.** `handleAgentLogBulk` switches to one batch call, keeping the
   dropped-line counting and its warning. This is the stage that delivers the
   improvement.
3. **Measurement.** The owner runs the load-test harness and records the
   before/after, since the harness is not in this repository.
