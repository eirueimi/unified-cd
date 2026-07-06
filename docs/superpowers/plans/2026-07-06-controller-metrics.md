# Controller Prometheus Metrics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expose an unauthenticated `GET /metrics` Prometheus endpoint on the controller covering run/step/webhook/HTTP counters plus DB-backed gauges for queue backlog and agent liveness.

**Architecture:** A new `internal/metrics` package owns a per-server Prometheus registry, a scrape-time DB collector (narrow `Counts` interface), and a `store.Store` decorator that counts run/step transitions. The controller wires them in `cmd/controller/main.go` and adds one chi middleware + one route.

**Tech Stack:** Go 1.26, `github.com/prometheus/client_golang`, chi v5, pgx v5, testify, real-Postgres tests via `store.NewTestPostgres`.

Spec: `docs/superpowers/specs/2026-07-06-controller-metrics-design.md`

## Global Constraints

- All code, comments, commit messages, and docs in English (AGENTS.md).
- Metric name prefix: `unifiedcd_`.
- `/metrics` is unauthenticated; docs must tell operators to block it at the LB when internet-facing.
- Per-Server `prometheus.NewRegistry()` — never the global default registry.
- HTTP `route` label uses the chi route pattern, never the raw path; unmatched requests use `route="unmatched"`.
- Collector DB budget: 3-second timeout; scrape errors increment `unifiedcd_scrape_collector_errors_total` and omit the family, never fail the response.
- `trigger` label values: `webhook`, `schedule`, `api` (spec's `call` value is dropped — call-created child runs arrive through the API and are not distinguishable at `CreateRun`; Task 8 updates the spec).
- Tests: `go test ./internal/metrics/... ./internal/store/... ./internal/controller/...` must pass. Postgres-backed tests follow the existing `store.NewTestPostgres(t)` pattern (requires Docker).
- Run `gofmt -w` on every file you touch before committing.

---

### Task 1: metrics package core (counters, histograms, handler)

**Files:**
- Modify: `go.mod` / `go.sum` (add dependency)
- Create: `internal/metrics/metrics.go`
- Test: `internal/metrics/metrics_test.go`

**Interfaces:**
- Consumes: nothing (leaf package).
- Produces: `metrics.New() *Metrics`, `(*Metrics).Handler() http.Handler`, and typed recorders used by every later task:
  `RunCreated(triggeredBy string)`, `RunFinished(status string)`, `StepCompleted(status string)`, `StepDuration(status string, seconds float64)`, `WebhookEvent(name, outcome string)`, `HTTPRequest(method, route string, code int, seconds float64)`.

- [ ] **Step 1: Add the dependency**

```bash
cd /path/to/unified-cd-metrics
go get github.com/prometheus/client_golang@latest
go mod tidy
```

- [ ] **Step 2: Write the failing test**

`internal/metrics/metrics_test.go`:

```go
package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTriggerLabel(t *testing.T) {
	assert.Equal(t, "webhook", triggerLabel("webhook:push"))
	assert.Equal(t, "schedule", triggerLabel("schedule:nightly"))
	assert.Equal(t, "api", triggerLabel("api"))
	assert.Equal(t, "api", triggerLabel("some-user@example.com")) // principal names fold into api
}

func TestRecorders(t *testing.T) {
	m := New()
	m.RunCreated("webhook:push")
	m.RunFinished("Failed")
	m.StepCompleted("Succeeded")
	m.StepDuration("Succeeded", 12.5)
	m.WebhookEvent("push", "accepted")
	m.HTTPRequest("GET", "/api/v1/runs/{id}", 200, 0.05)

	assert.Equal(t, 1.0, testutil.ToFloat64(m.runsCreated.WithLabelValues("webhook")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.runsFinished.WithLabelValues("Failed")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.stepsCompleted.WithLabelValues("Succeeded")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.webhookEvents.WithLabelValues("push", "accepted")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.httpRequests.WithLabelValues("GET", "/api/v1/runs/{id}", "200")))
}

func TestHandlerServesTextFormat(t *testing.T) {
	m := New()
	m.RunCreated("api")
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	require.Equal(t, 200, rec.Code)
	assert.True(t, strings.Contains(rec.Body.String(), "unifiedcd_runs_created_total"))
}

func TestTwoInstancesDoNotCollide(t *testing.T) {
	// Per-instance registries: constructing two must not panic on duplicate registration.
	_ = New()
	_ = New()
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/metrics/ -v`
Expected: FAIL — `undefined: New`, `undefined: triggerLabel`.

- [ ] **Step 4: Write the implementation**

`internal/metrics/metrics.go`:

```go
// Package metrics owns the controller's Prometheus registry, metric
// families, and the store decorator / DB collector that feed them.
package metrics

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds one registry per controller instance. Call sites go through
// typed recorder methods so handlers never touch raw Prometheus types.
type Metrics struct {
	reg *prometheus.Registry

	runsCreated     *prometheus.CounterVec
	runsFinished    *prometheus.CounterVec
	stepsCompleted  *prometheus.CounterVec
	stepDuration    *prometheus.HistogramVec
	webhookEvents   *prometheus.CounterVec
	httpRequests    *prometheus.CounterVec
	httpDuration    *prometheus.HistogramVec
	collectorErrors prometheus.Counter
}

// New builds a Metrics with its own registry (never the global default, so
// multiple Server instances can coexist in tests).
func New() *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		runsCreated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "unifiedcd_runs_created_total",
			Help: "Runs created, by trigger source (webhook, schedule, api).",
		}, []string{"trigger"}),
		runsFinished: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "unifiedcd_runs_finished_total",
			Help: "Runs transitioned to a terminal status.",
		}, []string{"status"}),
		stepsCompleted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "unifiedcd_steps_completed_total",
			Help: "Step reports received with a non-Running status.",
		}, []string{"status"}),
		stepDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "unifiedcd_step_duration_seconds",
			Help:    "Step wall-clock duration reported by agents.",
			Buckets: []float64{1, 5, 15, 60, 300, 900, 1800, 3600, 7200},
		}, []string{"status"}),
		webhookEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "unifiedcd_webhook_events_total",
			Help: "Webhook ingress events, by receiver name and outcome (accepted, rejected, filtered, error).",
		}, []string{"name", "outcome"}),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "unifiedcd_http_requests_total",
			Help: "HTTP requests, by method, chi route pattern, and status code.",
		}, []string{"method", "route", "code"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "unifiedcd_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds, by method and chi route pattern.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
		collectorErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "unifiedcd_scrape_collector_errors_total",
			Help: "Errors while collecting DB-backed gauges at scrape time.",
		}),
	}
	m.reg.MustRegister(m.runsCreated, m.runsFinished, m.stepsCompleted,
		m.stepDuration, m.webhookEvents, m.httpRequests, m.httpDuration,
		m.collectorErrors)
	return m
}

// Handler serves the registry in the Prometheus text exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// RunCreated records a successful run creation. triggeredBy is the raw store
// value ("webhook:<name>", "schedule:<name>", "api", or a principal name).
func (m *Metrics) RunCreated(triggeredBy string) {
	m.runsCreated.WithLabelValues(triggerLabel(triggeredBy)).Inc()
}

// RunFinished records a run's actual transition into a terminal status.
func (m *Metrics) RunFinished(status string) {
	m.runsFinished.WithLabelValues(status).Inc()
}

// StepCompleted records a step report with a non-Running status.
func (m *Metrics) StepCompleted(status string) {
	m.stepsCompleted.WithLabelValues(status).Inc()
}

// StepDuration records a completed step's wall-clock duration.
func (m *Metrics) StepDuration(status string, seconds float64) {
	m.stepDuration.WithLabelValues(status).Observe(seconds)
}

// WebhookEvent records one webhook ingress outcome.
func (m *Metrics) WebhookEvent(name, outcome string) {
	m.webhookEvents.WithLabelValues(name, outcome).Inc()
}

// HTTPRequest records one served HTTP request.
func (m *Metrics) HTTPRequest(method, route string, code int, seconds float64) {
	m.httpRequests.WithLabelValues(method, route, strconv.Itoa(code)).Inc()
	m.httpDuration.WithLabelValues(method, route).Observe(seconds)
}

// triggerLabel folds the free-form triggeredBy store value into a bounded
// label set. Principal names (manual API triggers) fold into "api".
func triggerLabel(triggeredBy string) string {
	switch {
	case strings.HasPrefix(triggeredBy, "webhook:"):
		return "webhook"
	case strings.HasPrefix(triggeredBy, "schedule:"):
		return "schedule"
	default:
		return "api"
	}
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/metrics/ -v`
Expected: PASS (4 tests).

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/metrics/
git commit -m "feat(metrics): registry, counters, and histograms for controller metrics"
```

---

### Task 2: store count queries (`CountRunsByStatus`, `CountAgentsByLiveness`, `FinishRun` on the interface)

**Files:**
- Modify: `internal/store/store.go` (interface additions)
- Modify: `internal/store/postgres.go` (two new methods; `FinishRun` already exists)
- Test: `internal/store/postgres_metrics_test.go`

**Interfaces:**
- Consumes: existing `CreateRun`, `MarkRunRunning`, `UpsertAgent` for test setup.
- Produces (all on `store.Store`, implemented by `*Postgres`):
  - `FinishRun(ctx context.Context, runID string, status api.RunStatus) (updated bool, err error)` — already implemented on `*Postgres` at `internal/store/postgres.go:571`; this task only adds it to the interface.
  - `CountRunsByStatus(ctx context.Context) (map[api.RunStatus]int, error)`
  - `CountAgentsByLiveness(ctx context.Context, staleAfter time.Duration) (alive, stale int, err error)`

- [ ] **Step 1: Write the failing test**

`internal/store/postgres_metrics_test.go`:

```go
package store

import (
	"context"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCountRunsByStatus(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	// Two Pending (CreateRun default), one of them moved to Running,
	// one finished (must not be counted).
	r1, err := pg.CreateRun(ctx, "job-a", nil, []byte(`{}`), nil, "api")
	require.NoError(t, err)
	_, err = pg.CreateRun(ctx, "job-a", nil, []byte(`{}`), nil, "api")
	require.NoError(t, err)
	r3, err := pg.CreateRun(ctx, "job-a", nil, []byte(`{}`), nil, "api")
	require.NoError(t, err)
	require.NoError(t, pg.MarkRunRunning(ctx, r1.ID))
	require.NoError(t, pg.MarkRunFinished(ctx, r3.ID, api.RunSucceeded))

	counts, err := pg.CountRunsByStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, counts[api.RunPending])
	assert.Equal(t, 1, counts[api.RunRunning])
	assert.Equal(t, 0, counts[api.RunSucceeded]) // terminal statuses excluded
}

func TestCountAgentsByLiveness(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	require.NoError(t, pg.UpsertAgent(ctx, "agent-fresh", "h1", "linux", "v1", nil, nil))
	require.NoError(t, pg.UpsertAgent(ctx, "agent-stale", "h2", "linux", "v1", nil, nil))
	_, err := pg.pool.Exec(ctx,
		`UPDATE agents SET last_seen_at = NOW() - interval '10 minutes' WHERE id = $1`,
		"agent-stale")
	require.NoError(t, err)

	alive, stale, err := pg.CountAgentsByLiveness(ctx, 90*time.Second)
	require.NoError(t, err)
	assert.Equal(t, 1, alive)
	assert.Equal(t, 1, stale)
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/store/ -run 'TestCountRunsByStatus|TestCountAgentsByLiveness' -v`
Expected: FAIL — `pg.CountRunsByStatus undefined`, `pg.CountAgentsByLiveness undefined`.

- [ ] **Step 3: Implement the Postgres methods**

Append to `internal/store/postgres.go` (near the other run queries):

```go
// CountRunsByStatus returns the number of non-terminal runs per status.
// Terminal statuses are excluded so the result stays a bounded gauge input.
func (p *Postgres) CountRunsByStatus(ctx context.Context) (map[api.RunStatus]int, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT status, COUNT(*) FROM runs
		 WHERE status IN ('Pending','Queued','Running')
		 GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[api.RunStatus]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[api.RunStatus(status)] = n
	}
	return out, rows.Err()
}

// CountAgentsByLiveness partitions registered agents by heartbeat freshness:
// alive = last_seen_at within staleAfter, stale = older than staleAfter.
func (p *Postgres) CountAgentsByLiveness(ctx context.Context, staleAfter time.Duration) (alive, stale int, err error) {
	err = p.pool.QueryRow(ctx,
		`SELECT COUNT(*) FILTER (WHERE last_seen_at >= NOW() - make_interval(secs => $1)),
		        COUNT(*) FILTER (WHERE last_seen_at <  NOW() - make_interval(secs => $1))
		 FROM agents`, staleAfter.Seconds()).Scan(&alive, &stale)
	return alive, stale, err
}
```

- [ ] **Step 4: Add the three methods to the `store.Store` interface**

In `internal/store/store.go`, directly under the existing `MarkRunFinished` line (line 124):

```go
	// FinishRun is like MarkRunFinished but reports whether the run actually
	// transitioned (false when it was already terminal).
	FinishRun(ctx context.Context, runID string, status api.RunStatus) (updated bool, err error)
	// CountRunsByStatus returns the number of non-terminal runs per status.
	CountRunsByStatus(ctx context.Context) (map[api.RunStatus]int, error)
	// CountAgentsByLiveness partitions registered agents by heartbeat freshness.
	CountAgentsByLiveness(ctx context.Context, staleAfter time.Duration) (alive, stale int, err error)
```

- [ ] **Step 5: Run the tests, then build the whole module**

Run: `go test ./internal/store/ -run 'TestCountRunsByStatus|TestCountAgentsByLiveness' -v`
Expected: PASS.
Run: `go build ./...`
Expected: no errors (`*Postgres` already satisfies the widened interface; nothing else implements `store.Store`).

- [ ] **Step 6: Commit**

```bash
git add internal/store/store.go internal/store/postgres.go internal/store/postgres_metrics_test.go
git commit -m "feat(store): run/agent count queries and FinishRun on the Store interface"
```

---

### Task 3: DB-backed collector

**Files:**
- Create: `internal/metrics/collector.go`
- Test: `internal/metrics/collector_test.go`

**Interfaces:**
- Consumes: `Metrics.reg`, `Metrics.collectorErrors` (Task 1); `api.RunStatus` constants.
- Produces: `type Counts interface { CountRunsByStatus(ctx context.Context) (map[api.RunStatus]int, error); CountAgentsByLiveness(ctx context.Context, staleAfter time.Duration) (alive, stale int, err error) }` (satisfied by `*store.Postgres` from Task 2) and `(*Metrics).RegisterDBCollector(c Counts, staleAfter time.Duration)`.

- [ ] **Step 1: Write the failing test**

`internal/metrics/collector_test.go`:

```go
package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeCounts struct {
	runs map[api.RunStatus]int
	alive, stale int
	err  error
}

func (f *fakeCounts) CountRunsByStatus(ctx context.Context) (map[api.RunStatus]int, error) {
	return f.runs, f.err
}

func (f *fakeCounts) CountAgentsByLiveness(ctx context.Context, staleAfter time.Duration) (int, int, error) {
	return f.alive, f.stale, f.err
}

func TestDBCollectorExportsGauges(t *testing.T) {
	m := New()
	m.RegisterDBCollector(&fakeCounts{
		runs:  map[api.RunStatus]int{api.RunPending: 2, api.RunRunning: 1},
		alive: 3, stale: 1,
	}, 90*time.Second)

	expected := `
# HELP unifiedcd_agents Registered agents, by heartbeat liveness.
# TYPE unifiedcd_agents gauge
unifiedcd_agents{state="alive"} 3
unifiedcd_agents{state="stale"} 1
# HELP unifiedcd_runs_current Current number of non-terminal runs, by status.
# TYPE unifiedcd_runs_current gauge
unifiedcd_runs_current{status="Pending"} 2
unifiedcd_runs_current{status="Queued"} 0
unifiedcd_runs_current{status="Running"} 1
`
	require.NoError(t, testutil.GatherAndCompare(m.reg, strings.NewReader(expected),
		"unifiedcd_runs_current", "unifiedcd_agents"))
}

func TestDBCollectorErrorsIncrementCounterAndOmitFamilies(t *testing.T) {
	m := New()
	m.RegisterDBCollector(&fakeCounts{err: errors.New("db down")}, 90*time.Second)

	families, err := m.reg.Gather()
	require.NoError(t, err) // scrape itself must not fail
	for _, f := range families {
		assert.NotEqual(t, "unifiedcd_runs_current", f.GetName())
		assert.NotEqual(t, "unifiedcd_agents", f.GetName())
	}
	assert.Equal(t, 2.0, testutil.ToFloat64(m.collectorErrors)) // one per failed query
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/metrics/ -run TestDBCollector -v`
Expected: FAIL — `m.RegisterDBCollector undefined`.

- [ ] **Step 3: Write the implementation**

`internal/metrics/collector.go`:

```go
package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/eirueimi/unified-cd/internal/api"
)

// Counts is the narrow store surface the scrape-time collector needs.
// *store.Postgres implements it.
type Counts interface {
	CountRunsByStatus(ctx context.Context) (map[api.RunStatus]int, error)
	CountAgentsByLiveness(ctx context.Context, staleAfter time.Duration) (alive, stale int, err error)
}

// collectTimeout bounds the DB work done per scrape.
const collectTimeout = 3 * time.Second

var (
	runsCurrentDesc = prometheus.NewDesc("unifiedcd_runs_current",
		"Current number of non-terminal runs, by status.", []string{"status"}, nil)
	agentsDesc = prometheus.NewDesc("unifiedcd_agents",
		"Registered agents, by heartbeat liveness.", []string{"state"}, nil)
)

type dbCollector struct {
	counts     Counts
	staleAfter time.Duration
	errors     prometheus.Counter
}

// RegisterDBCollector registers the scrape-time DB gauges on m's registry.
// staleAfter is the agent-liveness window (matches the stuck-run reaper).
func (m *Metrics) RegisterDBCollector(c Counts, staleAfter time.Duration) {
	m.reg.MustRegister(&dbCollector{counts: c, staleAfter: staleAfter, errors: m.collectorErrors})
}

func (d *dbCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- runsCurrentDesc
	ch <- agentsDesc
}

// Collect queries the DB with a bounded timeout. On error it increments the
// error counter and omits the affected family — the scrape never fails.
func (d *dbCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	if counts, err := d.counts.CountRunsByStatus(ctx); err != nil {
		d.errors.Inc()
		slog.Warn("metrics: count runs by status", "error", err)
	} else {
		for _, st := range []api.RunStatus{api.RunPending, api.RunQueued, api.RunRunning} {
			ch <- prometheus.MustNewConstMetric(runsCurrentDesc, prometheus.GaugeValue,
				float64(counts[st]), string(st))
		}
	}

	if alive, stale, err := d.counts.CountAgentsByLiveness(ctx, d.staleAfter); err != nil {
		d.errors.Inc()
		slog.Warn("metrics: count agents by liveness", "error", err)
	} else {
		ch <- prometheus.MustNewConstMetric(agentsDesc, prometheus.GaugeValue, float64(alive), "alive")
		ch <- prometheus.MustNewConstMetric(agentsDesc, prometheus.GaugeValue, float64(stale), "stale")
	}
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/metrics/ -v`
Expected: PASS (all tests including Task 1's).

- [ ] **Step 5: Commit**

```bash
git add internal/metrics/collector.go internal/metrics/collector_test.go
git commit -m "feat(metrics): scrape-time DB collector for run and agent gauges"
```

---

### Task 4: instrumented store decorator

**Files:**
- Create: `internal/metrics/store.go`
- Test: `internal/metrics/store_test.go`

**Interfaces:**
- Consumes: `store.Store` (with Task 2's additions), recorders from Task 1.
- Produces: `metrics.NewInstrumentedStore(s store.Store, m *Metrics) *InstrumentedStore` — an embedding decorator overriding exactly `CreateRun`, `FinishRun`, `MarkRunFinished`, `UpsertStepReport`.

- [ ] **Step 1: Write the failing test**

`internal/metrics/store_test.go` (uses the real Postgres test harness, the repo's standard pattern):

```go
package metrics

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstrumentedStoreCountsTransitions(t *testing.T) {
	m := New()
	st := NewInstrumentedStore(store.NewTestPostgres(t), m)
	ctx := context.Background()

	run, err := st.CreateRun(ctx, "job-a", nil, []byte(`{}`), nil, "webhook:push")
	require.NoError(t, err)
	assert.Equal(t, 1.0, testutil.ToFloat64(m.runsCreated.WithLabelValues("webhook")))

	// First finish counts; a second finish attempt is CAS-rejected and must not.
	require.NoError(t, st.MarkRunFinished(ctx, run.ID, api.RunFailed))
	require.NoError(t, st.MarkRunFinished(ctx, run.ID, api.RunSucceeded))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.runsFinished.WithLabelValues("Failed")))
	assert.Equal(t, 0.0, testutil.ToFloat64(m.runsFinished.WithLabelValues("Succeeded")))

	// Step reports: Running is not counted; terminal is; duration observed
	// only when both timestamps are present.
	started := time.Now().Add(-30 * time.Second)
	ended := time.Now()
	require.NoError(t, st.UpsertStepReport(ctx, run.ID, 0, 0, "build", "", "Running", nil, &started, nil, "", ""))
	require.NoError(t, st.UpsertStepReport(ctx, run.ID, 0, 0, "build", "", "Succeeded", nil, &started, &ended, "", ""))
	assert.Equal(t, 0.0, testutil.ToFloat64(m.stepsCompleted.WithLabelValues("Running")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.stepsCompleted.WithLabelValues("Succeeded")))
}

func TestInstrumentedStoreDoesNotCountFailures(t *testing.T) {
	m := New()
	st := NewInstrumentedStore(store.NewTestPostgres(t), m)

	// Finishing a nonexistent run errors and must not count.
	err := st.MarkRunFinished(context.Background(), "00000000-0000-0000-0000-000000000000", api.RunFailed)
	_ = err // MarkRunFinished's error contract is the store's own; the metric is what we assert.
	assert.Equal(t, 0.0, testutil.ToFloat64(m.runsFinished.WithLabelValues("Failed")))
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/metrics/ -run TestInstrumentedStore -v`
Expected: FAIL — `undefined: NewInstrumentedStore`.

- [ ] **Step 3: Write the implementation**

`internal/metrics/store.go`:

```go
package metrics

import (
	"context"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
)

// InstrumentedStore decorates a store.Store, counting run and step state
// transitions. Every other method passes through via embedding, so reapers
// and handlers are instrumented with a single wrap in main.go.
type InstrumentedStore struct {
	store.Store
	m *Metrics
}

func NewInstrumentedStore(s store.Store, m *Metrics) *InstrumentedStore {
	return &InstrumentedStore{Store: s, m: m}
}

func (s *InstrumentedStore) CreateRun(ctx context.Context, jobName string, params map[string]string, spec []byte, agentSelector []string, triggeredBy string) (*api.Run, error) {
	run, err := s.Store.CreateRun(ctx, jobName, params, spec, agentSelector, triggeredBy)
	if err == nil {
		s.m.RunCreated(triggeredBy)
	}
	return run, err
}

func (s *InstrumentedStore) FinishRun(ctx context.Context, runID string, status api.RunStatus) (bool, error) {
	updated, err := s.Store.FinishRun(ctx, runID, status)
	if err == nil && updated {
		s.m.RunFinished(string(status))
	}
	return updated, err
}

// MarkRunFinished routes through FinishRun so a transition is only counted
// when the run actually left a non-terminal state (the underlying CAS
// silently ignores finish-after-finish).
func (s *InstrumentedStore) MarkRunFinished(ctx context.Context, runID string, status api.RunStatus) error {
	_, err := s.FinishRun(ctx, runID, status)
	return err
}

func (s *InstrumentedStore) UpsertStepReport(ctx context.Context, runID string, stepIndex, stageIndex int, stepName, variant, status string, exitCode *int, startedAt, endedAt *time.Time, childRunID, callJobName string) error {
	err := s.Store.UpsertStepReport(ctx, runID, stepIndex, stageIndex, stepName, variant, status, exitCode, startedAt, endedAt, childRunID, callJobName)
	if err == nil && status != "Running" {
		s.m.StepCompleted(status)
		if startedAt != nil && endedAt != nil {
			s.m.StepDuration(status, endedAt.Sub(*startedAt).Seconds())
		}
	}
	return err
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/metrics/ -v`
Expected: PASS. Note: `MarkRunFinished` on a nonexistent run — check the actual behavior of `FinishRun` (it may return `updated=false, err=nil`); the metric assertion holds either way.

- [ ] **Step 5: Commit**

```bash
git add internal/metrics/store.go internal/metrics/store_test.go
git commit -m "feat(metrics): store decorator counting run and step transitions"
```

---

### Task 5: /metrics route and HTTP middleware on the Server

**Files:**
- Modify: `internal/controller/server.go` (field, setter, middleware, route)
- Test: `internal/controller/metrics_http_test.go`

**Interfaces:**
- Consumes: `metrics.New()`, `(*Metrics).Handler()`, `(*Metrics).HTTPRequest(...)` (Task 1).
- Produces: `(*Server).SetMetrics(m *metrics.Metrics)` — nil-safe: without it, the middleware no-ops and `/metrics` returns 404 (same pattern as `dexProxy`).

- [ ] **Step 1: Write the failing test**

`internal/controller/metrics_http_test.go`:

```go
package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/metrics"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMetricsEndpointWithoutSetMetricsIs404(t *testing.T) {
	s, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestMetricsEndpointServesUnauthenticated(t *testing.T) {
	s, _ := newTestServer(t)
	s.SetMetrics(metrics.New())

	rec := httptest.NewRecorder()
	// No Authorization header on purpose.
	s.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, strings.Contains(rec.Body.String(), "unifiedcd_"))
}

func TestHTTPMiddlewareUsesRoutePattern(t *testing.T) {
	s, _ := newTestServer(t)
	s.SetMetrics(metrics.New())

	// Hit a parameterized route (auth fails with 401 — still recorded).
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/some-id", nil)
	s.Router().ServeHTTP(httptest.NewRecorder(), req)

	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	assert.True(t, strings.Contains(body, `route="/api/v1/runs/{id}"`),
		"expected chi route pattern label, got:\n"+body)
	assert.False(t, strings.Contains(body, `route="/api/v1/runs/some-id"`),
		"raw path must never appear as a label")
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/controller/ -run 'TestMetricsEndpoint|TestHTTPMiddleware' -v`
Expected: FAIL — `s.SetMetrics undefined`.

- [ ] **Step 3: Implement the Server changes**

In `internal/controller/server.go`:

(a) Add to imports: `"github.com/eirueimi/unified-cd/internal/metrics"`.

(b) Add a field to the `Server` struct, after `uiProxy`:

```go
	metrics *metrics.Metrics // nil = middleware no-ops and /metrics returns 404.
```

(c) Add the setter next to `SetKeyManager`:

```go
// SetMetrics enables the /metrics endpoint and HTTP request instrumentation.
func (s *Server) SetMetrics(m *metrics.Metrics) { s.metrics = m }
```

(d) Add the middleware next to `accessLogMiddleware`:

```go
// metricsMiddleware records request count and duration per chi route
// pattern (never the raw path, to keep label cardinality bounded).
// No-op until SetMetrics is called.
func (s *Server) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.metrics == nil {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		route := chi.RouteContext(r.Context()).RoutePattern()
		if route == "" {
			route = "unmatched"
		}
		code := ww.Status()
		if code == 0 {
			code = http.StatusOK
		}
		s.metrics.HTTPRequest(r.Method, route, code, time.Since(start).Seconds())
	})
}
```

(e) In `routes()`, register the middleware after `accessLogMiddleware` and the route next to `/healthz`:

```go
	s.r.Use(s.metricsMiddleware)
```

```go
	// Prometheus metrics (no auth by design — block at the LB / firewall
	// when the controller is internet-facing). 404 until SetMetrics.
	s.r.Get("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if s.metrics == nil {
			http.NotFound(w, r)
			return
		}
		s.metrics.Handler().ServeHTTP(w, r)
	})
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/controller/ -run 'TestMetricsEndpoint|TestHTTPMiddleware' -v`
Expected: PASS.

- [ ] **Step 5: Run the full controller test package (middleware touches every route)**

Run: `go test ./internal/controller/`
Expected: PASS — no existing test may break.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/server.go internal/controller/metrics_http_test.go
git commit -m "feat(controller): /metrics endpoint and per-route HTTP instrumentation"
```

---

### Task 6: webhook ingress outcome counter

**Files:**
- Modify: `internal/controller/api_webhooks.go` (`handleWebhookIngress`, lines 71–191)
- Test: extend `internal/controller/api_webhooks_test.go`

**Interfaces:**
- Consumes: `(*Metrics).WebhookEvent(name, outcome string)` via a nil-safe helper.
- Produces: outcomes `accepted` (run created / AppSource sync scheduled), `rejected` (auth failure or unknown receiver — the latter with `name="unknown"`), `filtered` (a filter evaluated false), `error` (every other failure path).

- [ ] **Step 1: Write the failing test**

Append to `internal/controller/api_webhooks_test.go`:

```go
func TestWebhookIngressMetricOutcomes(t *testing.T) {
	s, pg := newTestServer(t)
	m := metrics.New()
	s.SetMetrics(m)

	// Receiver with no auth and a filter that only passes ref==main.
	applyWebhookReceiver(t, s, `
apiVersion: unified-cd/v1
kind: WebhookReceiver
metadata:
  name: wh-metrics
spec:
  trigger:
    job: wh-metrics-job
  filters:
    - '{{ eq .Payload.ref "main" }}'
`)
	applyJob(t, s, pg, "wh-metrics-job")

	post := func(path, body string) int {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		return rec.Code
	}

	assert.Equal(t, http.StatusOK, post("/webhook/wh-metrics", `{"ref":"main"}`))
	assert.Equal(t, http.StatusNoContent, post("/webhook/wh-metrics", `{"ref":"dev"}`))
	assert.Equal(t, http.StatusBadRequest, post("/webhook/wh-metrics", `not-json`))
	assert.Equal(t, http.StatusNotFound, post("/webhook/no-such-receiver", `{}`))

	get := func(name, outcome string) float64 {
		return testutil.ToFloat64(m.WebhookEventsForTest(name, outcome))
	}
	assert.Equal(t, 1.0, get("wh-metrics", "accepted"))
	assert.Equal(t, 1.0, get("wh-metrics", "filtered"))
	assert.Equal(t, 1.0, get("wh-metrics", "error"))
	assert.Equal(t, 1.0, get("unknown", "rejected"))
}
```

Note: this test references two helpers. If `api_webhooks_test.go` already has equivalents for applying a receiver and a job (check the top of the file — existing tests apply receivers via `/api/v1/webhooks` with `Authorization: Bearer secret`), reuse those instead of writing `applyWebhookReceiver`/`applyJob`. Match the file's existing helper names; only add these thin wrappers if none exist. Add the imports the file lacks (`metrics`, `testutil`).

Also add the test-only accessor to `internal/metrics/metrics.go` (exported counters would invite direct use; a narrow test hook is cleaner):

```go
// WebhookEventsForTest returns the underlying counter for label assertions
// in tests. Not for production use.
func (m *Metrics) WebhookEventsForTest(name, outcome string) prometheus.Counter {
	return m.webhookEvents.WithLabelValues(name, outcome)
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/controller/ -run TestWebhookIngressMetricOutcomes -v`
Expected: FAIL (compile error on the missing helper, then zero counts once wired incorrectly).

- [ ] **Step 3: Instrument the handler**

In `internal/controller/api_webhooks.go`, add a nil-safe helper above `handleWebhookIngress`:

```go
// countWebhookEvent records a webhook ingress outcome when metrics are enabled.
func (s *Server) countWebhookEvent(name, outcome string) {
	if s.metrics != nil {
		s.metrics.WebhookEvent(name, outcome)
	}
}
```

Then add one call before each return in `handleWebhookIngress` (current line numbers in parentheses):

| Return site | Insert before it |
|---|---|
| `GetWebhookReceiver` error → 404 (l.75) | `s.countWebhookEvent("unknown", "rejected")` |
| invalid receiver spec → 500 (l.81) | `s.countWebhookEvent(name, "error")` |
| body read error → 400 (l.88) | `s.countWebhookEvent(name, "error")` |
| auth verification failed → 401 (l.103) | `s.countWebhookEvent(name, "rejected")` |
| invalid JSON payload → 400 (l.110) | `s.countWebhookEvent(name, "error")` |
| filter template error → 400 (l.120) | `s.countWebhookEvent(name, "error")` |
| filtered out → 204 (l.125) | `s.countWebhookEvent(name, "filtered")` |
| appSource not found → 400 (l.135) | `s.countWebhookEvent(name, "error")` |
| `ResetAppSourceCommit` error → 500 (l.139) | `s.countWebhookEvent(name, "error")` |
| appSource sync scheduled → 202 (l.142) | `s.countWebhookEvent(name, "accepted")` |
| `paramsMapping` error → 400 (l.153) | `s.countWebhookEvent(name, "error")` |
| job not found → 400 (l.161) | `s.countWebhookEvent(name, "error")` |
| `resolveParams` error → 400 (l.172) | `s.countWebhookEvent(name, "error")` |
| `agentSelector` error → 400 (l.177) | `s.countWebhookEvent(name, "error")` |
| `CreateRun` error → 500 (l.187) | `s.countWebhookEvent(name, "error")` |
| success → 200 (l.190) | `s.countWebhookEvent(name, "accepted")` |

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/controller/ -run 'TestWebhook' -v`
Expected: PASS — the new test and all existing webhook tests.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/api_webhooks.go internal/controller/api_webhooks_test.go internal/metrics/metrics.go
git commit -m "feat(controller): count webhook ingress outcomes"
```

---

### Task 7: wire everything in cmd/controller/main.go

**Files:**
- Modify: `cmd/controller/main.go`
- Test: `internal/controller/metrics_integration_test.go`

**Interfaces:**
- Consumes: everything above.
- Produces: a running controller whose `/metrics` includes all families and whose background reapers/scheduler count through the decorator.

- [ ] **Step 1: Write the failing integration test**

`internal/controller/metrics_integration_test.go`:

```go
package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/metrics"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Mirrors the production main.go wiring: collector + decorator + endpoint.
func TestMetricsEndToEndWiring(t *testing.T) {
	pg := store.NewTestPostgres(t)
	m := metrics.New()
	m.RegisterDBCollector(pg, 90*time.Second)
	st := metrics.NewInstrumentedStore(pg, m)

	_, err := pg.UpsertBootstrapPAT(context.Background(), "test-bootstrap", HashToken("secret"))
	require.NoError(t, err)
	s := NewServer(Config{AgentToken: "agent-secret"}, st)
	s.SetMetrics(m)

	run, err := st.CreateRun(context.Background(), "wiring-job", nil, []byte(`{}`), nil, "api")
	require.NoError(t, err)
	require.NoError(t, st.MarkRunFinished(context.Background(), run.ID, api.RunFailed))

	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	for _, want := range []string{
		`unifiedcd_runs_created_total{trigger="api"} 1`,
		`unifiedcd_runs_finished_total{status="Failed"} 1`,
		`unifiedcd_runs_current{status="Pending"} 0`,
		`unifiedcd_agents{state="alive"} 0`,
	} {
		assert.True(t, strings.Contains(body, want), "missing %q in:\n%s", want, body)
	}
}
```

- [ ] **Step 2: Run it to verify it passes already (it uses library wiring, no main.go)**

Run: `go test ./internal/controller/ -run TestMetricsEndToEndWiring -v`
Expected: PASS. This test pins the wiring pattern main.go must follow; it fails if any piece regresses.

- [ ] **Step 3: Wire main.go**

In `cmd/controller/main.go`:

(a) Add import: `"github.com/eirueimi/unified-cd/internal/metrics"`.

(b) After the `pg.Migrate` block, before the server construction, add:

```go
	// Metrics: DB-backed gauges + store decorator counting run/step
	// transitions. staleAfter=90s matches the stuck-run reaper's window.
	m := metrics.New()
	m.RegisterDBCollector(pg, 90*time.Second)
	st := metrics.NewInstrumentedStore(pg, m)
```

(c) Replace `pg` with `st` in every call whose parameter is `store.Store`, so scheduler- and reaper-driven transitions are counted:

- `controller.NewServer(controller.Config{...}, st)`
- `controller.RunScheduler(ctx, st, 200*time.Millisecond)`
- `controller.RunLogArchiver(ctx, st, obj, 30*time.Second)`
- `controller.RunCacheCleanup(ctx, st, obj)`
- `controller.RunApprovalReaper(ctx, st, time.Minute)`
- `controller.RunStuckRunReaper(ctx, st, 30*time.Second, 90*time.Second, 60*time.Second)`
- `controller.RunQueuedRunReaper(ctx, st, 30*time.Second, 5*time.Minute, 90*time.Second)`
- `controller.RunAppSourceSyncReaper(ctx, st, 30*time.Second, 5*time.Minute)`
- `controller.RunAuditRetention(ctx, st, time.Hour, *auditRetentionDays)`
- `controller.RunGitResolver(ctx, st, resolver, km, 200*time.Millisecond)`
- `controller.RunAppSourceReconciler(ctx, st, fetcher, km, 30*time.Second)`

Keep `pg` for the `*store.Postgres`-specific calls: `pg.Close()`, `pg.Migrate`, `pg.UpsertBootstrapPAT`, `pg.DeleteBootstrapPATByName`, `pg.EnsureControllerKey`, `pg.DeleteExpiredOIDCStates`, `pg.DeleteStaleAgents`.

(d) After `srv := controller.NewServer(...)`, add:

```go
	srv.SetMetrics(m)
```

- [ ] **Step 4: Build and run the full test suite**

Run: `go build ./... && go test ./internal/... ./cmd/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/controller/main.go internal/controller/metrics_integration_test.go
git commit -m "feat(controller): wire metrics registry, DB collector, and instrumented store"
```

---

### Task 8: documentation and spec sync

**Files:**
- Modify: `docs/operations.md` (new "Metrics" section)
- Modify: `docs/superpowers/specs/2026-07-06-controller-metrics-design.md` (drop `call` from the trigger label list)

**Interfaces:** none (docs only).

- [ ] **Step 1: Add the Metrics section to docs/operations.md**

Append:

```markdown
## Metrics

The controller exposes Prometheus metrics at `GET /metrics` when metrics are
enabled (they are wired in by default in `cmd/controller`).

**Security:** `/metrics` is intentionally unauthenticated. If the controller
is reachable from the internet (e.g. for webhook ingress), block `/metrics`
at the load balancer or firewall.

Scrape config:

```yaml
scrape_configs:
  - job_name: unified-cd
    static_configs:
      - targets: ["controller-1:8080", "controller-2:8080"]
```

Key metrics:

| Metric | Type | Meaning |
|---|---|---|
| `unifiedcd_runs_current{status}` | gauge | Non-terminal runs (queue backlog = Pending + Queued) |
| `unifiedcd_agents{state}` | gauge | Agents by heartbeat liveness (alive / stale) |
| `unifiedcd_runs_created_total{trigger}` | counter | Runs created (webhook / schedule / api) |
| `unifiedcd_runs_finished_total{status}` | counter | Terminal run transitions |
| `unifiedcd_step_duration_seconds{status}` | histogram | Step wall-clock duration |
| `unifiedcd_webhook_events_total{name,outcome}` | counter | Webhook ingress outcomes |
| `unifiedcd_http_requests_total{method,route,code}` | counter | API traffic by chi route pattern |

With multiple controller replicas, gauges report identical values on every
replica (aggregate with `max()`); counters count only events the scraped
replica processed (aggregate with `sum(rate(...))`).

Example queries:

```promql
# Run failure rate over 1h, across replicas
sum(rate(unifiedcd_runs_finished_total{status="Failed"}[1h]))
  / sum(rate(unifiedcd_runs_finished_total[1h]))

# Queue backlog
max(unifiedcd_runs_current{status="Pending"})
  + max(unifiedcd_runs_current{status="Queued"})

# No alive agents (alert on > 0 for 5m)
max(unifiedcd_agents{state="alive"}) == 0

# p95 step duration
histogram_quantile(0.95, sum(rate(unifiedcd_step_duration_seconds_bucket[1h])) by (le))
```
```

- [ ] **Step 2: Sync the spec's trigger label list**

In `docs/superpowers/specs/2026-07-06-controller-metrics-design.md`, change the `runs_created_total` row: `` `trigger` = `webhook`, `schedule`, `api`, `call` `` → `` `trigger` = `webhook`, `schedule`, `api` `` and append to that row's cell: "(call-created child runs arrive via the API and fold into `api`)".

- [ ] **Step 3: Full suite + commit**

```bash
go build ./... && go test ./internal/... ./cmd/...
git add docs/operations.md docs/superpowers/specs/2026-07-06-controller-metrics-design.md
git commit -m "docs: metrics endpoint, scrape config, PromQL examples"
```

---

## Post-merge note (not a task)

The main working tree at `/path/to/unified-cd` contains an untracked `vendor/` directory. After merging this branch there, run `go mod vendor` in that tree once so the new `prometheus/client_golang` dependency is vendored for local builds.
