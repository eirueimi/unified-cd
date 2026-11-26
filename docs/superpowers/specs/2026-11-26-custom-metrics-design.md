# Job-Defined Custom Metrics — Design

Status: draft for review
Date: 2026-11-26
Depends on: `2026-11-26-run-metadata-design.md` (the `runs.trigger`,
`job_labels` and `job_annotations` columns, and the `dsl.RunData`,
`dsl.JobData` and `dsl.StepFailure` types).

**Build order:** the run metadata design is built and merged first. This
design's plan starts from that branch. Nothing here re-implements it.

## Intent

Users want to follow runs from their metrics: counts, durations and their
own numbers, labelled by job, trigger, status and job labels. They want this
without running an exporter of their own. Argo Workflows does this with a
`metrics:` block in the workflow and on each template. The controller
evaluates the block when a template or the workflow finishes and serves the
result on its Prometheus endpoint. This design brings the same feature to
unified-cd, keeping Argo's shape so that its users recognise it.

## Goals

- A `metrics:` block on the job (`spec.metrics`), emitted when the run
  reaches a terminal status. The same block on a step, emitted when the step
  reaches a terminal status. A matrix or foreach step emits once per
  combination.
- Counter, gauge and histogram metrics, with templated label values, a
  templated value and an optional `when` condition.
- Realtime gauges for run and step duration while they are running.
- Correct values with several controller replicas behind a load balancer.
- Bounded cardinality and memory: a series cap, and a TTL for series that
  stop updating.

## Non-goals

- Pushing to OTLP, StatsD or a Pushgateway. Prometheus scraping of
  `/metrics` only.
- Metrics emitted from inside a step's process. Users who need that keep
  using their own SDKs, and the run metadata design gives them labels.
- Summaries (quantiles).
- `metrics:` in a `kind: JobTemplate`. It is rejected when the template is
  parsed (`metrics: is not supported in a JobTemplate; define metrics on the
  Job`), and `uses:` expansion refuses it too, so metrics never travel through
  template inlining in v1. Define them on the Job.
- Exposing a running step's live outputs. Values are read when the step
  finishes.

## Anti-goals

- A job that can break or flood `/metrics` for everyone. The controller
  only uses the non-panicking client_golang calls (`Register`,
  `GetMetricWithLabelValues`), never the `Must*` variants, on anything a job
  defines. Invalid
  definitions fail at apply time. Runtime errors drop the sample and are
  counted. Nothing panics the registry.
- Double counting in HA. Counters and histograms are counted exactly once,
  by the replica that handles the terminal transition.
- Collisions with built-in metric names.

## Approaches considered (where values live)

- **A. Everything in each replica's memory.** This is the simplest
  approach. Gauges break in HA: each replica keeps its own "last value", and
  a query cannot tell which one is newest. A restart also loses gauges.
- **B. Everything in the database.** Every replica serves the same values.
  Counters are then duplicated on every replica, so `sum()` double counts.
  It also costs a write on every event.
- **C. Counters and histograms in memory, gauges in the database (chosen).**
  Counters and histograms add up correctly with `sum()`, and `rate()`
  handles resets after a restart. Gauges have a single latest value and
  survive restarts. Realtime gauges are computed at scrape time from
  database rows, so any replica can answer. This mirrors the controller's
  existing in-memory event counters and its DB-backed scrape-time gauges in
  `internal/metrics/collector.go`.

The user chose C.

## DSL

```yaml
spec:
  metrics:                       # run-level, emitted on run terminal status
    - name: deploy_runs          # becomes unifiedcd_job_deploy_runs_total
      help: Deploy runs by outcome
      labels:
        - key: status
          value: "{{ .Run.Status }}"
        - key: team
          value: "{{ .Job.Labels.team }}"
      counter:
        value: "1"
    - name: deploy_duration_seconds
      help: Deploy duration
      histogram:
        value: "{{ .Run.Duration }}"
        buckets: [30, 60, 120, 300, 600]
    - name: deploy_running_seconds
      help: Elapsed time of the running deploy
      gauge:
        value: "{{ .Run.Duration }}"
        realtime: true
  steps:
    - name: test
      run: make test
      outputs: { coverage: ... }
      metrics:                   # step-level, emitted on step terminal status
        - name: coverage_ratio
          help: Test coverage
          when: '{{ eq .Step.Status "Succeeded" }}'
          gauge:
            value: "{{ .Outputs.coverage }}"
            operation: Set       # Set (default) | Add | Sub
```

- `metrics` is allowed on `spec`, on steps and parallel sub-steps (both
  `StepEntry` and `Step`), and on `finally:` steps. It is not allowed on
  `call:` steps; use the called job's own `spec.metrics`.
- Exactly one of `counter`, `gauge` or `histogram`. `counter.value` defaults
  to `"1"`.
- Templates use the existing Go template engine and functions, not Argo's
  `{{status}}` syntax. `when` is a template. The sample is emitted only if
  the template renders `true` after trimming spaces. Any other output skips
  the sample. A template error counts as a runtime error.
- **Exposed name:** `unifiedcd_job_<name>`. Counters get a `_total` suffix
  unless the name already ends in `_total`. The prefix keeps user metrics
  apart from built-in `unifiedcd_*` metrics and makes them easy to select.

### Template context (evaluated in the controller)

| Field | Run-level | Step-level |
|---|---|---|
| `.Run.ID`, `.Run.Trigger`, `.Run.TriggeredBy`, `.Run.DisplayName`, `.Run.CreatedAt`, `.Run.ScheduledAt` | yes | yes |
| `.Run.Status` | terminal status | empty (not populated); use `.Step.Status` |
| `.Run.Duration` | seconds from claim to the terminal transition | seconds from claim to now |
| `.Job.Name`, `.Job.Labels.<k>`, `.Job.Annotations.<k>` | yes | yes |
| `.Params.<k>` | yes | yes |
| `.Outputs.<k>` | run outputs | this step's outputs |
| `.Step.Name`, `.Step.Status`, `.Step.ExitCode`, `.Step.Duration`, `.Step.Attempts`, `.Step.MatrixKey`, `.Step.Stage` | no | yes |

Durations: `.Run.Duration` is the transition time minus `claimed_at`, in
seconds. A run that never reached an agent, for example one cancelled while
queued, has `.Run.Duration = 0` and gets no realtime row. `.Step.Duration`
is `endedAt - startedAt`, or 0 if either is missing.

Label values are plain strings. Invalid UTF-8 is replaced with U+FFFD, and a
value longer than 1024 bytes is truncated to 1024 bytes on a UTF-8 boundary. An
empty value is exported as an empty label, which Prometheus treats as the
label being absent.

`.Step.Attempts` needs a new optional `attempts` field on
`StepReportRequest`, set by the agent's retry loop. An old agent leaves it
at 0. `.Run.Duration` and `.Step.Duration` are float seconds.

## Validation (apply time)

- `name` and label `key` must match `^[a-zA-Z_][a-zA-Z0-9_]*$`. The exposed
  name must be a valid Prometheus name.
- Label keys `job`, `instance`, `le` and `quantile`, and any key starting
  with `__`, are rejected. Prometheus attaches or reserves them.
- `help` is required and non-empty.
- At most 50 metrics per block, and at most 10 labels per metric.
- `histogram.buckets` is optional (default: Prometheus `DefBuckets`); if given it must be finite (no NaN or infinities) and strictly increasing.
- `realtime: true` is only allowed on a gauge whose `value` is exactly
  `{{ .Run.Duration }}` (run-level) or `{{ .Step.Duration }}` (step-level).
  Its labels may use only fields that are known when the run or step starts:
  `.Run.*` except `Status` and `Duration`, `.Job.*`, `.Params.*`, `.Step.Name`,
  `.Step.MatrixKey` and `.Step.Stage`.
- `operation` is only valid on gauges.
- **Exposed-family collisions.** The check covers every family a definition
  exposes: `<name>_total` for counters, and `<name>_bucket`, `<name>_sum` and
  `<name>_count` for histograms. A family that collides with any other
  job's family, or with a built-in `unifiedcd_*` family, is rejected. This
  stops a counter `x` and a gauge `x_total` from both producing
  `unifiedcd_job_x_total`.
- **One shape per name across all jobs.** The shape is type, label key set,
  help, buckets and `realtime`, so a plain gauge and a realtime gauge cannot
  share a name. Two definitions with the same
  `name` must agree on type, label key set, help and buckets. The check runs
  against every stored job at apply time. Apply and AppSource apply both
  fail with
  `metric %q conflicts with job %q: <type|labels|help|buckets> differ`.
  A runtime guard catches races between two concurrent applies (see
  Runtime errors).

## Runtime

### Where samples are produced

- **Run-level:** a hook in `metrics.InstrumentedStore` that runs after a
  successful terminal transition. It covers `FinishRun` and
  `FinishRunIfStatus` (agent finish, cancel, reapers, scheduler). The hook
  loads the run, its spec snapshot and its outputs.
  `TransitionPendingToQueued`'s internal `MarkRunFinished` bypasses the
  decorator today, so the builder must route it through the hook or cover
  that case explicitly.
- **Step-level:** `handleAgentStepReport`, on the **first** terminal report
  for a `(run, stageIndex, stepIndex, variant)`. `UpsertStepReport` gains a
  return value that says whether this call moved the row from non-terminal
  to terminal: a compare on the previous status in the same statement.
  Duplicate or retried POSTs then emit nothing. To keep the hot path cheap,
  only a terminal report (`Succeeded`, `Failed`, `Cancelled`, `Skipped`) takes
  a transaction with a per-step advisory lock and reads the previous status.
  `Running` reports and the child-run link call use the single guarded
  statement and return `firstTerminal = false`.
  - Terminal statuses are `Succeeded`, `Failed`, `Cancelled` and `Skipped`.
    All of them emit, and users filter with `when`.
  - Steps that `MarkRunStepsInterrupted` flips to Failed do not emit step
    metrics. The run-level metrics of that run still emit. Document this.
  - The agent posts step outputs before the terminal report, so outputs are
    present.
  - A new variant-aware getter,
    `GetStepVariantOutputs(runID, stepIndex, variant)`, supplies
    `.Outputs`. The existing `GetStepOutputs` merges variants, so it cannot
    be used here.
  - The report's `(stageIndex, stepIndex)` is resolved to the spec entry by
    a helper that walks stages, parallel sub-steps and `finally:` in the
    same order and with the same numbering as `buildClaimResponse`. Both
    call that one helper, so the two numberings cannot drift apart.
    `.Step.Stage` is the report's `stageIndex`.
- Only the replica whose transition or report succeeded emits. A
  compare-and-swap miss (`updated == false`) or a duplicate terminal report
  emits nothing, so counters are counted once.

### Counters and histograms (in memory)

- Each exposed name gets one dynamic `CounterVec` or `HistogramVec`,
  created on first use on the controller's existing registry and keyed by
  name and shape hash. When a definition's shape differs from the
  registered vec's:
  - if the vec has no live series (all expired by TTL), it is unregistered
    and recreated with the new shape;
  - otherwise the sample is dropped with `reason="conflict"`.
  A deleted job's series therefore disappear by TTL, and the name then
  becomes free.
- Prometheus vecs record neither the last update time nor the series count.
  The emitter keeps its own map from label hash to label values and last
  update, under a mutex. The TTL sweep and the series cap use that map, and
  removal goes through `DeleteLabelValues` with the stored values.
- The value template must render a finite float. Counters must be ≥ 0.
- **Series cap:** at most 1000 label combinations per name per replica. The
  operator can change it with the new controller setting
  `customMetrics.maxSeriesPerMetric`. A new combination over the cap is
  dropped.
- **TTL:** a series not updated for `customMetrics.ttl` (default `24h`) is
  deleted by a once-a-minute sweep. `0` disables the sweep.

### Gauges (database)

- New table `custom_metric_gauges`, with these columns and primary key
  `(name, labels_hash)`:
  - `name text`
  - `labels jsonb`
  - `labels_hash text`
  - `help text`
  - `value double precision`
  - `updated_at timestamptz`
- `Set` upserts the value. `Add` and `Sub` upsert `value = value ± $v`
  atomically.
- The scrape-time collector is an **unchecked** collector, a sibling of
  `dbCollector`. Its `Describe` sends nothing, because names and label sets
  are dynamic. It exports every row with `updated_at` newer than the TTL.
  Rows whose label keys do not match the name's current shape (stale rows
  left by a shape change) are skipped and counted with `reason="conflict"`.
  Rows with the same name and label values are de-duplicated in Go before
  they are emitted, because client_golang fails a scrape that contains a
  duplicate series. The leader deletes older rows hourly.
- The series cap is enforced on insert: count the rows for that name, and
  refuse a new `labels_hash` when the cap is reached.
- Every replica exports the same rows, so a series appears once per
  replica. Prometheus must scrape each controller replica as its own
  target. `docs/operator-manual/operations.md` already requires this for
  the built-in counters and gauges. A scrape through the load balancer
  gives wrong counters, with jumps that look like resets. Docs tell users to read gauges with `max by (...)`, the same as
  the existing agent disk gauges.

### Realtime gauges (database, computed at scrape)

- New table `custom_metric_realtime`, keyed by
  `(run_id, step_key, name)`. `step_key` is `""` for run-level and
  `<stageIndex>/<stepIndex>/<matrixKey>` for steps. Other columns:
  - `labels jsonb`
  - `help text`
  - `started_at timestamptz`
- Rows are written with labels already evaluated:
  - run-level, when the run is claimed;
  - step-level, on the step's `Running` report.
- Rows are deleted on the terminal transition or report. The leader removes
  rows whose run is terminal, as a backstop, and the scrape-time read joins
  `runs` and ignores rows of terminal runs, so a leftover row (for example
  from a replica without this feature) is never exposed.
- At scrape, the value is `now - started_at`. Two rows with the same name
  and labels, such as two concurrent runs of one job, are exported as the
  maximum.

### Runtime errors

- `unifiedcd_custom_metric_errors_total{metric, reason}` counts runtime
  failures. `reason` is one of `template`, `value`, `conflict`,
  `series_cap` or `db`. Every error is also logged at warn, with the run ID
  and the metric.
- An error never fails the run, the step or the request.

When `customMetrics.enabled` is false, existing rows stay in the tables but
are not exported. The leader's hourly cleanup keeps running. If no leader
runs cleanup for a while, the TTL filter at export time still hides stale
rows.

## Configuration

Add a new `customMetrics` section to the controller config:
- `enabled`, default `true`;
- `maxSeriesPerMetric`, default `1000`;
- `ttl`, default `24h`.

Each setting has a `UNIFIED_CUSTOM_METRICS_*` env var and a flag, following
the existing pattern. When the section is disabled, definitions still
validate at apply time, but nothing is emitted, and the realtime and gauge
rows are not written.

## Migration

During a rolling upgrade, runs and steps that finish on a replica without this
feature are not counted. Their leftover realtime rows are hidden once the run
is terminal and removed by the hourly sweep.

Migration 035, which runs after the run metadata design's 034, creates the
two tables. There are no changes to existing tables.

## Docs to update in the same change

- New `docs/user-guide/writing-jobs/custom-metrics.md`. It covers the DSL,
  the template context table, the naming rule, HA reading rules (`sum` for
  counters and histograms, `max by` for gauges) and the cardinality advice.
  It must explicitly say not to label with `.Run.ID`.
- `docs/operator-manual/operations.md`, metrics section:
  - the `unifiedcd_job_*` family;
  - `unifiedcd_custom_metric_errors_total`;
  - the config keys;
  - gauge duplication across replicas.
- `docs/reference/configuration.md`: `customMetrics.*`.
- `docs/troubleshooting/controller-and-database.md`: the apply-time
  conflict error, and series-cap drops.
- `examples/custom-metrics/`: one job that uses each metric type and a
  realtime gauge.
- Regenerate the schema and the field reference.

## Builder latitude

- Hash function for `labels_hash`, as long as label order does not matter.
- Whether the run-level hook lives in the store decorator or in a small
  emitter the decorator calls.
- Exact flag spellings, if they follow the existing controller config
  conventions.
