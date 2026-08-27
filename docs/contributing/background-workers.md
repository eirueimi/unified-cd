# Background workers

The controller runs twelve goroutines on tickers, launched from
`cmd/controller/main.go`. None of them has a caller waiting on a result, which
is what makes them worth a page of their own: **a worker that fails every pass
has nothing to surface it** except the metrics and log lines it emits itself.

## The twelve

| Worker | File | Interval | Leader-elected |
|---|---|---|---|
| `RunScheduler` | `scheduler.go` | 200ms | yes |
| `RunGitResolver` | `scheduler.go` | 200ms | **no** — see below |
| `RunCacheCleanup` | `scheduler.go` | 24h | yes |
| `RunLogArchiver` | `archiver.go` | 30s | yes |
| `RunLogTrim` | `log_trim.go` | 1h | yes |
| `RunRunRetention` | `run_retention.go` | 1h | yes |
| `RunAuditRetention` | `audit_retention.go` | 1h | yes |
| `RunApprovalReaper` | `approval_reaper.go` | 1m | yes |
| `RunStuckRunReaper` | `stuckrun_reaper.go` | 30s | yes |
| `RunQueuedRunReaper` | `queuedrun_reaper.go` | 30s | yes |
| `RunAppSourceSyncReaper` | `appsource_sync_reaper.go` | 30s | yes |
| `RunAppSourceReconciler` | `appsource_reconciler.go` | 30s | yes |

Intervals are the values `main.go` passes; several are configurable, and a
worker whose feature is disabled (`retentionDays <= 0`, a nil object store)
returns immediately instead of spinning.

## The shape they share

Eleven of the twelve follow the same skeleton:

```go
func RunX(ctx, st, ...) {
    ticker := time.NewTicker(interval)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
        }
        metrics.ObservePass("x", func() (int, int, error) { return runXOnce(...) })
    }
}

func runXOnce(...) (ok, failed int, err error) {
    release, err := st.AcquireAdvisoryLock(ctx, xLockKey)
    if err != nil { ...; return 0, 0, err }
    if release == nil { return 0, 0, nil }   // another replica is leader
    defer release()
    ...
}
```

Two things to notice.

**Leadership is per worker, not per process.** Each holds its own Postgres
advisory lock, with its own key (`xLockKey`, a four-byte mnemonic — `'loga'`,
`'ltrm'`, `'stuk'` and so on, all declared near the top of their files). So one
replica can be leader for archiving while another leads trimming. There is no
single "leader replica".

**A non-leader pass is a success with zero items**, not an error. Each replica
exports its own metrics; summing across replicas gives the fleet's real
throughput.

## `RunGitResolver` is the exception

It takes **no advisory lock**. Every replica resolves Pending runs' `uses:`
git templates independently.

This is worth knowing before you touch it: with several replicas, the same run
can be fetched and resolved more than once, and each replica writes the
resolved spec with `UpdateRunSpec`. The writes agree, so the outcome is the
same — but the git fetches are duplicated.

If you change this worker, decide deliberately whether that is still the
intent, and do not assume the leader-election skeleton above applies to it.

## Instrumentation, and why the per-item counts exist

Nine of the twelve report through `metrics.ObservePass`, which records pass
duration, pass outcome, and **per-item results**:

- `unifiedcd_background_task_runs_total{task,outcome}` — outcome is `success`
  or `error`
- `unifiedcd_background_task_duration_seconds{task}`
- `unifiedcd_background_task_items_total{task,result}` — result is `ok` or
  `error`

The per-item split is not redundancy. Several of these workers iterate a batch
and deliberately swallow item failures so one bad item cannot abort the sweep —
the log archiver logs a run it could not archive and moves to the next,
returning `nil`.

**A pass in which every single item failed therefore reports `success`.** That
is precisely the silent breakage an operator needs to see, and
`rate(unifiedcd_background_task_items_total{result="error"}[15m])` is the only
query that distinguishes "nothing to archive" from "nothing archivable".

### The three that are not instrumented

`RunScheduler`, `RunGitResolver` and `RunAppSourceReconciler` carry their
leader state and cursor position inline in the loop body rather than in a
single pass function, so there is no one place to time without restructuring
them. Restructuring a leader-elected loop to add a metric is the wrong trade.

The scheduler stays observable indirectly: `unifiedcd_runs_current{status="Pending"}`
climbing together with `unifiedcd_run_time_to_claim_seconds` is the signal that
it has stopped.

## The recorder seam

`metrics.ObservePass` reads a package-level `atomic.Pointer[Metrics]` set by
`metrics.NewForController`, not a parameter threaded through every worker.

That is deliberate and worth understanding before "cleaning it up": these are
package-level functions with roughly forty call sites, nearly all tests that
have no opinion about metrics. Threading a recorder through every signature
would churn those tests to express something none of them assert. The recorder
is nil until startup wires it, and `ObservePass` tolerates nil, so every test
keeps working untouched.

Wiring it **at construction** rather than through a separate
`SetBackgroundMetrics` call is also deliberate: it means a controller that has
metrics has instrumented workers, instead of depending on a line `main.go`
could omit with nothing to notice. See
[Invariants — a guard is not trusted until it has been seen to fail](invariants.md).

## Adding a worker

1. Follow the skeleton: ticker, `select` on `ctx.Done()`, a `runXOnce` that
   takes an advisory lock with a **new** key and returns
   `(ok, failed int, err error)`.
2. Wrap the call in `metrics.ObservePass("your_task", ...)`. Use a fixed task
   name — it is a Prometheus label, so a value derived from input is a
   cardinality leak.
3. If the pass iterates a batch and continues past failures, count those
   failures. Returning `nil` with everything failed is the defect this is here
   to prevent.
4. Launch it from `cmd/controller/main.go` with `go`, and return early inside
   the worker when its feature is disabled rather than gating the launch.
5. Document its interval and lock key here.
