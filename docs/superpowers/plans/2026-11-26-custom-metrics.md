# Job-Defined Custom Metrics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Jobs and steps declare Argo-style `metrics:`. The controller
evaluates them when the run or step reaches a terminal status and serves the
results on `/metrics` as `unifiedcd_job_*`.

**Architecture:**
- Counters and histograms live in each replica's memory. The replica that
  handles the terminal transition creates one dynamic vec per name, with its
  own series map for the series cap and the TTL.
- Gauges and realtime gauges live in two new tables (migration 035). An
  unchecked scrape-time collector exports them, de-duplicated.
- Templates use the run-metadata types (`dsl.RunData`, `dsl.JobData`),
  evaluated in the controller.

**Tech Stack:** Go, `github.com/prometheus/client_golang`, PostgreSQL, Go
`text/template`.

**Spec:** `docs/superpowers/specs/2026-11-26-custom-metrics-design.md`

**Prerequisite:** branch `feat/run-metadata` is merged to `main`. Rebase
`feat/custom-metrics` onto `main` before Task 1. Migration 034 and the
`dsl.RunData`, `dsl.JobData` and `api.Run.Trigger` / `JobLabels` /
`JobAnnotations` fields exist.

## Global Constraints

- English only, no PII. Commit with
  `git -c user.name=eirueimi -c user.email="128969149+eirueimi@users.noreply.github.com" commit`,
  ending every message with:
  - `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`
  - `Claude-Session: https://claude.ai/code/session_01G3NWwwXnjYtSfzJjZYTrnV`
- Exposed name: `unifiedcd_job_<name>`. A counter gets `_total` appended
  unless `<name>` already ends in `_total`. A histogram also exposes
  `_bucket`, `_sum` and `_count`.
- Name and label key regex: `^[a-zA-Z_][a-zA-Z0-9_]*$`.
- Rejected label keys: `job`, `instance`, `le`, `quantile`.
- Limits: at most 50 metrics per block and 10 labels per metric.
- Defaults: `customMetrics.enabled=true`, `customMetrics.maxSeriesPerMetric=1000`,
  `customMetrics.ttl=24h`.
- Realtime is allowed only on a gauge whose value is exactly
  `{{ .Run.Duration }}` (run-level) or `{{ .Step.Duration }}` (step-level).
- Error counter: `unifiedcd_custom_metric_errors_total{metric, reason}`, with
  `reason` one of `template`, `value`, `conflict`, `series_cap`, `db`.
- Apply-time conflict error, exact text:
  `metric %q conflicts with job %q: %s differ`. The `%s` is one of `type`,
  `labels`, `help`, `buckets`, `realtime` or `family`.
- Never use `prometheus.MustRegister`, `WithLabelValues` or
  `MustNewConstMetric` on anything a job defines. Use `Register`,
  `GetMetricWithLabelValues` and `NewConstMetric`, and handle their errors.
- An emission error never fails a run, a step or a request.
- Do not hand-edit generated schema or reference files. Run `go generate ./...`.

## Review Focus

1. Two concurrent runs of one job, both with a realtime gauge with equal
   labels, must not fail the scrape (Task 4,
   `TestDBCollector_DedupesRealtime`).
2. A duplicate terminal step report, the agent retrying the POST, counts
   once (Task 5, `TestStepReport_DuplicateTerminalEmitsOnce`).
3. A value template that renders `abc` or `NaN` must drop the sample and
   count `reason="value"` without panicking (Task 4,
   `TestEmitter_BadValue`).
4. Re-applying a job that changes a metric's labels, after its old series
   expired, must work. While they are still live, the new samples must be
   refused with `conflict` (Task 4, `TestEmitter_ShapeChange`).
5. A run cancelled while queued emits run-level metrics with
   `.Run.Duration = 0` and creates no realtime row (Task 5,
   `TestRunMetrics_CancelledWhileQueued`).

---

### Task 1: DSL — `metrics:` types, validation, families and shape

**Files:**
- Create: `internal/dsl/metrics_types.go` (types; schemagen scans `*_types.go`), `internal/dsl/metrics_validate.go`
- Modify: `internal/dsl/types.go`: add `Metrics []MetricDef \`yaml:"metrics,omitempty"\`` to `Spec`, `StepEntry` and `Step`
- Modify: `internal/dsl/parse.go:83`: call validation from `Parse`
- Test: `internal/dsl/metrics_validate_test.go`

**Interfaces:**
- Produces:
  ```go
  type MetricDef struct {
      Name      string           `yaml:"name"`
      Help      string           `yaml:"help"`
      Labels    []MetricLabel    `yaml:"labels,omitempty"`
      When      string           `yaml:"when,omitempty"`
      Counter   *CounterMetric   `yaml:"counter,omitempty"`
      Gauge     *GaugeMetric     `yaml:"gauge,omitempty"`
      Histogram *HistogramMetric `yaml:"histogram,omitempty"`
  }
  type MetricLabel struct{ Key string `yaml:"key"`; Value string `yaml:"value"` }
  type CounterMetric struct{ Value string `yaml:"value,omitempty"` } // "" means "1"
  type GaugeMetric struct {
      Value     string `yaml:"value"`
      Realtime  bool   `yaml:"realtime,omitempty"`
      Operation string `yaml:"operation,omitempty"` // "", "Set", "Add", "Sub"
  }
  type HistogramMetric struct {
      Value   string    `yaml:"value"`
      Buckets []float64 `yaml:"buckets,omitempty"`
  }
  const MetricNamePrefix = "unifiedcd_job_"
  func (m MetricDef) Kind() string              // "counter" | "gauge" | "histogram"
  func (m MetricDef) ExposedName() string       // with the counter _total rule
  func (m MetricDef) Families() []string        // every exposed family name
  func (m MetricDef) Shape() MetricShape
  type MetricShape struct { Kind, Help string; LabelKeys []string /* sorted */; Buckets []float64; Realtime bool }
  func (a MetricShape) Diff(b MetricShape) string // "" if equal, else the first of "type","labels","help","buckets","realtime"
  func (s MetricShape) Hash() string
  func ValidateMetrics(defs []MetricDef, level string) error // level: "run" | "step"
  ```
  `ValidateMetrics` also rejects two definitions in one job with the same
  name but a different shape, and a `metrics` block on a `call:` step.
  Implement that by calling it from a job-wide validator that sees every
  block.

- [ ] **Step 1: Write the failing tests**, one table test
  `TestValidateMetrics` with one case per Global Constraints rule:
  - a bad name;
  - a reserved label `job`;
  - empty help;
  - 51 metrics, and 11 labels;
  - zero types, and two types;
  - non-increasing buckets;
  - realtime on a counter;
  - realtime with the value `{{ .Outputs.x }}`;
  - a run-level realtime with `{{ .Step.Duration }}`;
  - realtime labels that use `.Run.Status`;
  - `operation: Mul`;
  - `operation` on a counter;
  - the same name with different labels within one job;
  - `metrics` on a `call:` step.

  Each case asserts an error substring. Also add a valid case per type.
  Add `TestMetricDef_ExposedNameAndFamilies`: counter `x` gives
  `unifiedcd_job_x_total`, counter `x_total` gives `unifiedcd_job_x_total`,
  and histogram `h` gives the four families.
- [ ] **Step 2: Run** `go test ./internal/dsl/ -run 'ValidateMetrics|ExposedName'`. Expected: FAIL.
- [ ] **Step 3: Implement.** For the realtime label rule, scan each label
  template's text for `.Run.Status`, `.Run.Duration`, `.Outputs`,
  `.Step.Status`, `.Step.ExitCode`, `.Step.Duration` and `.Step.Attempts`,
  and reject any match.
- [ ] **Step 4: Run** `go test ./internal/dsl/`, then `go generate ./...`, and commit the generated diff. Expected: PASS.
- [ ] **Step 5: Commit** `feat(dsl): metrics: blocks on jobs and steps`.

### Task 2: Store — migration 035, gauges, realtime rows, first-terminal step reports, variant outputs

**Files:**
- Create: `internal/store/migrations/035_custom_metrics.up.sql` and `.down.sql`
- Modify: `internal/store/store.go`, `internal/store/postgres.go`, `internal/metrics/store.go` (wrapper)
- Test: `internal/store/postgres_custommetrics_test.go`

**Interfaces:**
- Produces:
  ```go
  type MetricGauge struct { Name, Help string; Labels map[string]string; Value float64; UpdatedAt time.Time }
  type MetricRealtime struct { RunID, StepKey, Name, Help string; Labels map[string]string; StartedAt time.Time }
  // ErrSeriesCap is returned when a new labels_hash would exceed maxSeries for that name.
  var ErrSeriesCap = errors.New("custom metric series cap reached")
  ApplyMetricGauge(ctx context.Context, g MetricGauge, labelsHash, op string, maxSeries int) error // op: "Set"|"Add"|"Sub"
  ListMetricGauges(ctx context.Context, since time.Time) ([]MetricGauge, error)
  DeleteMetricGaugesBefore(ctx context.Context, before time.Time) (int64, error)
  PutMetricRealtime(ctx context.Context, r MetricRealtime) error
  DeleteMetricRealtime(ctx context.Context, runID, stepKey string) error // stepKey "" deletes the run-level rows only
  ListMetricRealtime(ctx context.Context) ([]MetricRealtime, error)
  DeleteMetricRealtimeForTerminalRuns(ctx context.Context) (int64, error)
  UpsertStepReport(...same args...) (firstTerminal bool, err error)
  GetStepVariantOutputs(ctx context.Context, runID string, stepIndex int, variant string) (map[string]string, error)
  ```
  Table definitions come from the spec. `custom_metric_gauges` has primary
  key `(name, labels_hash)`. `custom_metric_realtime` has primary key
  `(run_id, step_key, name)`, and `run_id` references `runs(id)` with
  `ON DELETE CASCADE`.

  `firstTerminal` is true only when this call changed the row's status
  from a non-terminal value, or from no row, to one of `Succeeded`,
  `Failed`, `Cancelled` or `Skipped`. Compute it in the same statement:
  `INSERT ... ON CONFLICT DO UPDATE ... RETURNING` with the old status
  taken from a CTE or `xmax` check.

- [ ] **Step 1: Write the failing tests.**
  - `TestApplyMetricGauge_SetAddSub`: Set 5, Add 2, Sub 1 gives 6.
  - `TestApplyMetricGauge_SeriesCap`: with maxSeries 2, a third hash returns `ErrSeriesCap`, and updating an existing hash still works.
  - `TestListMetricGauges_Since`: rows older than `since` are excluded.
  - `TestMetricRealtime_PutListDelete`: put, list and delete, and step rows survive a run-level delete.
  - `TestDeleteMetricRealtimeForTerminalRuns`: removes only the rows of terminal runs.
  - `TestUpsertStepReport_FirstTerminal`: the sequence Running, Failed, Failed returns false, true, false.
  - `TestGetStepVariantOutputs`: two variants keep separate maps.
- [ ] **Step 2: Run** `go test ./internal/store/ -run 'MetricGauge|MetricRealtime|FirstTerminal|VariantOutputs'`. Expected: FAIL.
- [ ] **Step 3: Implement.** Update every `UpsertStepReport` caller so it builds; callers ignore the bool for now.
- [ ] **Step 4: Run** `go build ./... && go test ./internal/store/ ./internal/metrics/`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(store): custom metric tables and first-terminal step reports (migration 035)`.

### Task 3: Templates and the controller-side sample context

**Files:**
- Modify: `internal/dsl/template.go` (new data type and expander, reusing `funcMap`)
- Test: `internal/dsl/metric_template_test.go`

**Interfaces:**
- Produces:
  ```go
  type MetricStepData struct {
      Name, Status, MatrixKey string
      ExitCode, Attempts, Stage int
      Duration float64
  }
  type MetricTemplateData struct {
      Run     RunData           // Duration for metrics is set via RunDuration below
      Job     JobData
      Params  map[string]string
      Outputs map[string]string
      Step    MetricStepData
  }
  func ExpandMetricTemplate(tpl string, d MetricTemplateData) (string, error)
  func MetricWhen(tpl string, d MetricTemplateData) (bool, error) // "" -> true; else trimmed == "true"
  func MetricValue(tpl string, d MetricTemplateData) (float64, error) // error on non-numeric, NaN or ±Inf
  ```
  `dsl.RunData.Duration` from run metadata is `int64` seconds. Metrics need
  float seconds. Add `RunDurationSeconds float64` to `MetricTemplateData`,
  and in `ExpandMetricTemplate` make `{{ .Run.Duration }}` render it: wrap
  `RunData` in a struct whose `Duration` field is a float64 and shadows the
  embedded one.

- [ ] **Step 1: Write the failing tests.**
  - `TestMetricValue`: `"1"` gives 1, `"{{ .Step.Duration }}"` with 2.5 gives 2.5, and `abc`, `NaN` and `+Inf` give an error.
  - `TestMetricWhen`: `""` gives true, `'{{ eq .Step.Status "Succeeded" }}'` follows the status, and `" true\n"` gives true.
  - `TestExpandMetricTemplate_Fields`: `.Job.Labels.team`, `.Params.env` and `.Outputs.coverage` all render.
- [ ] **Step 2: Run** `go test ./internal/dsl/ -run 'MetricValue|MetricWhen|ExpandMetricTemplate'`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./internal/dsl/`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(dsl): metric template context and value parsing`.

### Task 4: Metrics package — emitter (counters/histograms) and DB collector (gauges/realtime)

**Files:**
- Create: `internal/metrics/custom.go` (emitter), `internal/metrics/custom_collector.go` (unchecked collector)
- Modify: `internal/metrics/metrics.go`: register the error counter and expose `(*Metrics).Custom() *CustomEmitter`
- Test: `internal/metrics/custom_test.go`, `internal/metrics/custom_collector_test.go`

**Interfaces:**
- Consumes: Task 1 `MetricDef` and `MetricShape`. Task 2 store methods. Task 3 `MetricValue`.
- Produces:
  ```go
  type CustomConfig struct { Enabled bool; MaxSeriesPerMetric int; TTL time.Duration }
  type CustomGaugeStore interface {
      ApplyMetricGauge(ctx context.Context, g store.MetricGauge, labelsHash, op string, maxSeries int) error
      ListMetricGauges(ctx context.Context, since time.Time) ([]store.MetricGauge, error)
      ListMetricRealtime(ctx context.Context) ([]store.MetricRealtime, error)
  }
  type CustomEmitter struct{ /* reg, cfg, vecs map[string]*customVec, mu, errors *prometheus.CounterVec, gauges CustomGaugeStore */ }
  func NewCustomEmitter(reg *prometheus.Registry, cfg CustomConfig, gauges CustomGaugeStore, errs *prometheus.CounterVec) *CustomEmitter
  // Emit records one sample. labels are already rendered. value is already parsed.
  func (e *CustomEmitter) Emit(ctx context.Context, def dsl.MetricDef, labels map[string]string, value float64)
  func (e *CustomEmitter) Sweep(now time.Time) // TTL for in-memory series; called once a minute
  func (e *CustomEmitter) CountError(metric, reason string)
  func RegisterCustomCollector(reg *prometheus.Registry, cfg CustomConfig, gauges CustomGaugeStore, shapes func(name string) (dsl.MetricShape, bool), errs *prometheus.CounterVec, now func() time.Time) error
  func BuiltinFamilyNames() []string // every family name registered by New(), for the apply-time check
  ```
  The emitter keys vecs by exposed name. If a vec exists with a different
  `Shape().Hash()`:
  - with zero tracked series, `Unregister` it and recreate it;
  - otherwise `CountError(name, "conflict")` and drop the sample.

  The series map entry is `{labelValues []string; lastUpdate time.Time}`.
  The cap is checked before `GetMetricWithLabelValues`. Gauges go to
  `gauges.ApplyMetricGauge`; `store.ErrSeriesCap` maps to `series_cap` and
  any other error maps to `db`. Realtime gauges are never passed to
  `Emit`, because Task 5 writes their rows.

  The collector's `Describe` sends nothing. `Collect`:
  - lists gauges since `now - TTL` and all realtime rows;
  - skips rows whose sorted label keys differ from `shapes(name)`, counting
    `conflict`; a missing shape also counts as `conflict`;
  - de-duplicates by name and sorted labels, with max value for realtime;
  - emits `NewConstMetric(GaugeValue)` with help from the row.

  The realtime value is `now - StartedAt` in seconds. It uses a 3 s
  timeout, as `dbCollector` does.

- [ ] **Step 1: Write the failing tests**, using a fake `CustomGaugeStore` and `prometheus/testutil`:
  - `TestEmitter_CounterAndHistogram`: two counter samples give 2, and a histogram sample lands in the right bucket.
  - `TestEmitter_SeriesCap`: with cap 2, the third label set is dropped and `series_cap` is counted.
  - `TestEmitter_TTLSweep`: after `Sweep(now+TTL+1s)` the series is gone.
  - `TestEmitter_ShapeChange`: live series plus a new shape gives `conflict`. After the sweep, the new shape registers.
  - `TestEmitter_BadValue`: `CountError` with `value` does not panic. This is the plumbing test; the parsing happens in Task 5's caller. Also, `Emit` with a wrong label count never panics.
  - `TestEmitter_GaugeOps`: Set, Add and Sub reach the store with the right op, and `ErrSeriesCap` is counted as `series_cap`.
  - `TestDBCollector_DedupesRealtime`: two realtime rows with the same name and labels give one series with the max value, and `testutil.GatherAndCount` does not error.
  - `TestDBCollector_SkipsShapeMismatch`.
  - `TestDBCollector_TTLFilter`.
- [ ] **Step 2: Run** `go test ./internal/metrics/ -run 'Emitter|DBCollector_'`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test -race ./internal/metrics/`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(metrics): custom metric emitter and DB-backed gauge collector`.

### Task 5: Controller — config, apply-time conflicts, run and step emission, realtime rows, sweeps

**Files:**
- Modify: `internal/config/controller.go` (`CustomMetrics` section: YAML `customMetrics`, env `UNIFIED_CUSTOM_METRICS_ENABLED`, `_MAX_SERIES_PER_METRIC`, `_TTL`, and flags following the existing pattern)
- Modify: `cmd/controller/main.go` (build `CustomEmitter`, register the collector, start the sweeps)
- Create: `internal/controller/custom_metrics.go`, containing:
  - the sample builder;
  - the step-spec resolver;
  - `checkMetricConflicts`;
  - `RunCustomMetricsSweeper`.
- Modify:
  - `internal/controller/api_jobs.go:78` and `appsource_apply.go:62`: conflict check before upsert;
  - `api_agent.go`: claim path writes run-level realtime rows; `handleAgentStepReport` handles a step `Running` report (realtime rows) and the first terminal report (step emission);
  - `buildClaimResponse`: uses the shared resolver;
  - `internal/metrics/store.go`: run-level hook on `FinishRun` and `FinishRunIfStatus` when `updated`.
- Modify: `internal/store/postgres.go` around the internal `MarkRunFinished` calls (`:703,723,837`): expose the finished run IDs so the hook can run for them, or route them through the decorator.
- Modify: `internal/api/types.go` `StepReportRequest`: add `Attempts int \`json:"attempts,omitempty"\``. Modify `internal/agent/orchestrator.go`, where the report is built near `:1036`: set `Attempts` to the last `try`.
- Test: `internal/controller/custom_metrics_test.go`, `internal/config/controller_custommetrics_test.go`

**Interfaces:**
- Consumes: Tasks 1–4.
- Produces:
  ```go
  // Shared by buildClaimResponse and step emission so indices cannot drift.
  func resolveClaimStep(spec dsl.Spec, stageIndex, stepIndex int) (dsl.Step, bool)
  func checkMetricConflicts(ctx context.Context, st store.Store, jobName string, spec dsl.Spec) error
  type customMetrics struct{ emitter *metrics.CustomEmitter; st store.Store; cfg metrics.CustomConfig }
  func (c *customMetrics) onRunFinished(ctx context.Context, runID string, status api.RunStatus)
  func (c *customMetrics) onStepTerminal(ctx context.Context, run api.Run, spec dsl.Spec, req api.StepReportRequest)
  func (c *customMetrics) onRunClaimed(ctx context.Context, run api.Run, spec dsl.Spec)
  func (c *customMetrics) onStepRunning(ctx context.Context, run api.Run, spec dsl.Spec, req api.StepReportRequest)
  func RunCustomMetricsSweeper(ctx context.Context, st store.Store, e *metrics.CustomEmitter, ttl time.Duration)
  ```
  - `RunCustomMetricsSweeper` calls `e.Sweep` every minute on every
    replica. Under an advisory lock, as `RunApprovalReaper` does, it runs
    `DeleteMetricGaugesBefore(now-ttl)` and
    `DeleteMetricRealtimeForTerminalRuns` every hour.
  - `checkMetricConflicts` compares every definition in the new spec with
    the definitions in every other stored job. It compares by family name
    for the `family` reason, and by name for the shape reasons. It also
    compares against `metrics.BuiltinFamilyNames()`.
  - Sample flow:
    1. render `when`; a false result skips the sample;
    2. render each label value, replacing invalid UTF-8 with U+FFFD;
    3. parse the value with `MetricValue`;
    4. call `Emit`.

    A template or value failure calls `CountError(name, "template" | "value")`
    and logs a warning with the run ID. With `Enabled=false`, every
    `on*` call is a no-op.
  - `.Run.Duration` is the transition time minus `claimed_at`, or 0 if the
    run was never claimed. `.Step.Duration` is `EndedAt - StartedAt`, or 0.
  - `.Outputs`: run-level uses `GetRunOutputs`; step-level uses
    `GetStepVariantOutputs`.
  - `MarkRunStepsInterrupted` steps do not emit.
  - Realtime `StepKey` is `fmt.Sprintf("%d/%d/%s", stageIndex, stepIndex, variant)`.

- [ ] **Step 1: Write the failing tests** (controller test server with real store):
  - `TestApplyJob_MetricConflict`: job A has counter `x` with labels `[a]`. Job B with counter `x` and labels `[b]` is rejected with `metric "x" conflicts with job "A": labels differ`. Job B with gauge `x_total` is rejected with `family differ`.
  - `TestRunMetrics_CounterOnFinish`: the agent finish emits `unifiedcd_job_runs_total{status="Succeeded"} 1`, gathered from the registry.
  - `TestRunMetrics_CancelledWhileQueued`: cancel before claim emits the sample with duration 0, and no realtime row exists.
  - `TestStepReport_DuplicateTerminalEmitsOnce`: two identical Failed reports give a counter of 1.
  - `TestStepMetrics_MatrixPerCombination`: two variants emit two samples, each with its own `.Outputs`.
  - `TestRealtime_RowsLifecycle`: claim creates the run row, a step `Running` report creates the step row, and the terminal transitions delete both.
  - `TestResolveClaimStep_MatchesClaimNumbering`: for a spec with parallel sub-steps and `finally:`, every `(stage, step)` in `buildClaimResponse` resolves to the same step name.
  - `TestCustomMetrics_Disabled`: nothing is emitted and no rows are written.
  - `TestControllerConfig_CustomMetrics`: defaults, plus YAML, env and flag precedence.
- [ ] **Step 2: Run** `go test ./internal/controller/ ./internal/config/ -run 'MetricConflict|RunMetrics_|StepReport_Duplicate|StepMetrics_|Realtime_|ResolveClaimStep|CustomMetrics'`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./internal/controller/ ./internal/config/ ./internal/agent/ ./internal/store/`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(controller): emit job-defined custom metrics`.

### Task 6: Docs, examples, generated artifacts

**Files:**
- Create: `docs/user-guide/writing-jobs/custom-metrics.md`, covering:
  - the DSL;
  - the template context table from the spec;
  - the naming rule;
  - `when`;
  - realtime limits;
  - HA reading rules: `sum(rate())` for counters and histograms, `max by (...)` for gauges, each replica scraped separately;
  - cardinality advice, including "never label with `.Run.ID`";
  - interrupted steps do not emit.
- Modify: `docs/operator-manual/operations.md`, metrics section:
  - the `unifiedcd_job_*` family;
  - `unifiedcd_custom_metric_errors_total` in the metric table;
  - the config keys;
  - gauge duplication across replicas.
- Modify: `docs/reference/configuration.md` (`customMetrics.*`), `docs/troubleshooting/controller-and-database.md` (conflict error text, `series_cap` drops)
- Modify: `docs/user-guide/writing-jobs/job-structure.md` (link to the new page), and the docs nav if pages are listed explicitly
- Create: `examples/custom-metrics/job.yaml` and a README: a counter by status, a duration histogram, a step gauge from an output, and a realtime gauge
- Modify: `README.md` feature list (one line)

- [ ] **Step 1:** Write the docs and the example. Every exposed name and error string must match Global Constraints exactly.
- [ ] **Step 2: Run** `go generate ./... && go test ./internal/dsl/ -run 'Examples|Templates'`. Expected: PASS.
- [ ] **Step 3: Commit** `docs: job-defined custom metrics`.
