# Custom Metrics

A job can publish its own Prometheus metrics: a counter of deploys by outcome, a histogram of build time, a gauge fed from a step output. You declare them in `metrics:` blocks; the controller evaluates them and serves them on its `/metrics` endpoint next to the built-in metrics. Nothing runs on the agent and no push gateway is needed.

For the operator side (names in the metrics table, limits, HA behavior) see [Operations: Metrics](../../operator-manual/operations.md#metrics). For the switches (`customMetrics.enabled`, `maxSeriesPerMetric`, `ttl`) see [Configuration](../../reference/configuration.md). A runnable example is in `examples/jobs/custom-metrics/`.

## Declaring metrics

`metrics:` is allowed in two places:

- **`spec.metrics`** (run level) is evaluated once, when the run reaches a terminal status.
- **A step's `metrics:`** (step level) is evaluated once, when that step reaches a terminal status (`Succeeded`, `Failed`, `Cancelled` or `Skipped`). It works on steps, on sub-steps inside `parallel:`, and on `finally:` steps. It is not allowed on `call:` steps; put `metrics:` in the called job instead.

```yaml
spec:
  metrics:
    - name: deploy_runs               # exposed as unifiedcd_job_deploy_runs_total
      help: Deploy runs by outcome
      labels:
        - key: status
          value: "{{ .Run.Status }}"
        - key: team
          value: "{{ .Job.Labels.team }}"
      counter: {}                     # increments by 1 (counter.value defaults to "1")
    - name: deploy_duration_seconds   # exposed as unifiedcd_job_deploy_duration_seconds
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
      outputs:
        coverage: "{{ .Params.coverage }}"
      metrics:
        - name: coverage_ratio
          help: Test coverage
          when: '{{ eq .Step.Status "Succeeded" }}'
          gauge:
            value: "{{ .Outputs.coverage }}"
            operation: Set            # Set (default) | Add | Sub
```

Every definition has `name`, `help` (required), optional `labels` and `when`, and exactly one of `counter`, `gauge` or `histogram`.

| Type | Fields | Behavior |
|---|---|---|
| `counter` | `value` (optional, default `1`) | Adds `value` to the series. A negative value is rejected. |
| `gauge` | `value`, `operation` (`Set` default, `Add`, `Sub`), `realtime` | Sets, adds to or subtracts from the series' last value. |
| `histogram` | `value`, `buckets` (optional) | Observes `value`. Without `buckets` the Prometheus default buckets are used; when given they must be strictly increasing. |

`value`, label values and `when` are Go templates, rendered by the controller with the same functions as other templates. `value` must render to a finite number.

## Naming

The exposed name is `unifiedcd_job_` plus your `name`. Counters get a `_total` suffix unless the name already ends in `_total`. A histogram also exposes `_bucket`, `_sum` and `_count`. So `deploy_runs` as a counter becomes `unifiedcd_job_deploy_runs_total`, and `deploy_duration_seconds` as a histogram becomes `unifiedcd_job_deploy_duration_seconds_bucket` and friends.

Rules checked when you apply the job:

- `name` and label keys match `^[a-zA-Z_][a-zA-Z0-9_]*$`.
- Label keys `job`, `instance`, `le` and `quantile` are reserved, as is any key starting with `__`. Keys may not repeat.
- Histogram `buckets` must be finite numbers (no `NaN` or infinities) and strictly increasing.
- `metrics:` is not supported in a JobTemplate. A template step that declares it is rejected, so define the metrics on the Job that uses the template.
- At most 50 metrics per block and 10 labels per metric.
- `operation` is only valid on a gauge.

Metric names are shared across all jobs. Two definitions with the same `name`, in this job or another one, must have the same type, label keys, help text, buckets and `realtime` setting, and no two names may expose the same family. A family may not collide with a built-in `unifiedcd_*` metric either. Violations are rejected at apply time; the exact messages are in [Troubleshooting](../../troubleshooting/controller-and-database.md#applying-a-job-fails-with-a-metric-conflict).

## Template context

Metric templates see these fields. Run-level metrics run when the run finishes; step-level metrics run when the step finishes.

| Field | Run-level | Step-level |
|---|---|---|
| `.Run.ID`, `.Run.Trigger`, `.Run.TriggeredBy`, `.Run.DisplayName`, `.Run.CreatedAt`, `.Run.ScheduledAt` and the other [run metadata](run-metadata.md) fields | yes | yes |
| `.Run.Status` | the terminal status | empty (not populated); use `.Step.Status` |
| `.Run.Duration` | seconds from claim to the terminal transition | seconds from claim to now |
| `.Job.Name`, `.Job.Labels.<k>`, `.Job.Annotations.<k>` | yes | yes |
| `.Params.<k>` | yes | yes |
| `.Outputs.<k>` | the run's outputs | this step's outputs |
| `.Step.Name`, `.Step.Status`, `.Step.ExitCode`, `.Step.Duration`, `.Step.Attempts`, `.Step.MatrixKey`, `.Step.Stage` | no | yes |

Notes:

- Durations are float seconds. `.Run.Duration` counts from the moment an agent claimed the run. A run that never reached an agent (for example cancelled while queued) has `.Run.Duration` of `0` and gets no realtime row. `.Step.Duration` is the step's end time minus its start time, or `0` when either is missing.
- A missing key (for example an unset label or output) renders as an empty string. An empty label value is exported as an empty label, which Prometheus treats as the label being absent. Label values longer than 1024 bytes are truncated to 1024 bytes (on a UTF-8 character boundary). A value template that renders empty or non-numeric is counted as an error, see below.
- `.Step.Attempts` is `1` unless the step was retried and the agent reported the attempt count.
- Steps that were interrupted (for example Failed by the controller after an agent was lost) do **not** emit step-level metrics. The run's own `spec.metrics` still emit when the run finishes.

## `when`

`when` is a template. The sample is emitted only if the rendered text, trimmed of spaces, is exactly `true`. Any other result skips the sample silently. A template error is counted in `unifiedcd_custom_metric_errors_total{reason="template"}`.

```yaml
when: '{{ eq .Step.Status "Succeeded" }}'
```

Terminal `Skipped` and `Cancelled` steps also emit, so use `when` to restrict a step metric to the statuses you care about.

## Realtime gauges

A gauge with `realtime: true` shows a value while the run or step is still going, for example "how long has this deploy been running". Instead of storing a number, the controller stores the start time and computes the elapsed seconds at every scrape. When the run or step finishes, the series disappears.

Realtime is restricted so that it is computable at scrape time:

- The gauge `value` must be exactly `{{ .Run.Duration }}` at run level or `{{ .Step.Duration }}` at step level.
- Labels may only use fields known when the run or step starts: `.Run.*` except `.Run.Status` and `.Run.Duration`, `.Job.*`, `.Params.*`, `.Step.Name`, `.Step.MatrixKey` and `.Step.Stage`. Using `.Outputs`, `.Step.Status`, `.Step.ExitCode`, `.Step.Duration` or `.Step.Attempts` in a label is rejected at apply time.
- A realtime and a non-realtime gauge cannot share a name.

Run-level realtime rows appear when an agent claims the run. Step-level rows appear when the step reports `Running`. Rows of runs that ended abnormally are cleaned up by a background sweep.

## Reading the metrics with several controller replicas

Counters and histograms are kept in memory on the replica that produced the sample, so each replica holds only part of the total. Run-level samples are produced on the replica that finished the run; step-level samples are produced on the replica that received the step's report. Gauges are stored in the database, so every replica shows the same value. Therefore:

- **Scrape every controller replica separately.** Do not scrape through a load balancer, which would hit a different replica on each scrape.
- **Counters and histograms:** aggregate with `sum(rate(...))`, for example `sum by (status) (rate(unifiedcd_job_deploy_runs_total[5m]))`. A replica restart resets its counters; `rate()` handles that.
- **Gauges:** every replica exposes the same series, so a plain `sum()` multiplies the value by the replica count. Use `max by (...)`, for example `max by (team) (unifiedcd_job_coverage_ratio)`.

## Cardinality

Each distinct combination of label values is a separate series. Keep label values to a small, bounded set: a status, a team, an environment, a matrix key.

- **Never use `.Run.ID`** (or other per-run values such as timestamps, display names or free-form params) as a label. Every run would create a new series.
- Per metric name and replica, at most `customMetrics.maxSeriesPerMetric` series are kept (default 1000). A sample that would create a new series over that limit is dropped and counted in `unifiedcd_custom_metric_errors_total{reason="series_cap"}`; series that already exist keep updating.
- A series that has not been updated for `customMetrics.ttl` (default 24h) is dropped.

## Errors at run time

Problems that only appear when a sample is produced do not fail the run. They increment `unifiedcd_custom_metric_errors_total{metric,reason}` with `reason` one of `template`, `value`, `conflict`, `series_cap` and `db`; see [Operations: Metrics](../../operator-manual/operations.md#metrics) for what each means.

## Rolling upgrades

Runs and steps that finish on a controller replica that does not yet have this feature are not counted, and neither are their gauge updates. Realtime rows such a replica leaves behind are hidden from scrapes once the run is terminal and are removed by the hourly sweep. Metrics are complete again once every replica runs the new version.

## Changing a metric's shape

If you change a metric's type, labels, help or buckets, apply the change to every job that uses the name. Other controller replicas notice the new shape within about 30 seconds; until then they may count the old shape's samples as `conflict` errors.
