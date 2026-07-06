# Controller Prometheus Metrics — Design

Date: 2026-07-06
Status: Approved

## Goal

Expose operational metrics from the controller so operators can alert on run
failure rates, queue backlog, agent liveness, and API/webhook health using a
standard Prometheus scrape. Scope is the `/metrics` endpoint only — no bundled
Prometheus/Grafana stack (operators bring their own).

## Non-goals

- Agent-side metrics (host agent / k8s agent). Future work.
- A bundled monitoring stack (Prometheus, Grafana, dashboards, alert rules)
  in `deployments/`. Only a scrape-config example in the docs.
- Authentication on `/metrics` (see Security below).

## Endpoint & Security

- `GET /metrics` served by the controller router, registered alongside
  `/healthz` and `/readyz` as an unauthenticated route.
- Rendered by `promhttp.HandlerFor` bound to a **per-Server
  `prometheus.NewRegistry()`** (no global default registry) so parallel tests
  can construct multiple `Server` instances without duplicate-registration
  panics.
- Security posture: the endpoint is unauthenticated by decision. The
  operations guide must state that when the controller is reachable from the
  internet (e.g. for webhook ingress), `/metrics` must be blocked at the load
  balancer / firewall layer.

## Metric Inventory

Prefix: `unifiedcd_`.

### DB-backed gauges (custom collector)

Collected at scrape time via new store methods, each bounded by a 3-second
timeout. The database is the source of truth, so every controller replica
reports identical values.

| Metric | Labels | Meaning |
|---|---|---|
| `unifiedcd_runs_current` | `status` = `Pending`, `Queued`, `Running` | Current number of non-terminal runs per state (the non-terminal values of `api.RunStatus`). Queue backlog = `Pending` + `Queued`. |
| `unifiedcd_agents` | `state` = `alive`, `stale` | Registered agents partitioned by heartbeat freshness, using the same staleness threshold as the stuck-run reaper. |

Note: `WaitingApproval` is a step-report status, not a run status, so it does
not appear in `runs_current`; approval backlog is visible through
`unifiedcd_steps_completed_total{status="WaitingApproval"}` instead.

### In-process counters & histograms

| Metric | Labels | Instrumented at |
|---|---|---|
| `unifiedcd_runs_created_total` | `trigger` = `webhook`, `schedule`, `api` | Successful `CreateRun` (trigger derived from the `triggeredBy` prefix). (call-created child runs arrive via the API and fold into `api`) |
| `unifiedcd_runs_finished_total` | `status` = `Succeeded`, `Failed`, `Cancelled` | Successful `MarkRunFinished` / `FinishRun` transition |
| `unifiedcd_steps_completed_total` | `status` | Successful `UpsertStepReport` with a terminal step status |
| `unifiedcd_step_duration_seconds` (histogram) | `status` | Same site, observed as `endedAt - startedAt` when both are present |
| `unifiedcd_webhook_events_total` | `name`, `outcome` = `accepted`, `rejected`, `filtered`, `error` | `handleWebhookIngress` (`rejected` = auth failure, `filtered` = filters evaluated false, `error` = internal failure) |
| `unifiedcd_http_requests_total` | `method`, `route`, `code` | chi middleware |
| `unifiedcd_http_request_duration_seconds` (histogram) | `method`, `route` | chi middleware |

Cardinality control:

- `route` uses the chi route **pattern** (e.g. `/api/v1/runs/{id}`), never the
  raw path. Requests that match no route are recorded as `route="unmatched"`.
- `webhook_events_total{name}` uses the receiver name only when the receiver
  exists; unknown names are recorded as `name="unknown"` so probes cannot mint
  unbounded label values.
- Histogram buckets: default Prometheus buckets for HTTP; wider buckets
  (`1s … 2h`) for `step_duration_seconds` since steps include long builds.

## Architecture

Three small units, all under `internal/controller` unless noted:

1. **`internal/metrics` package** — owns the registry, metric definitions, and
   the DB-backed collector. Exposes a `Metrics` struct with typed increment /
   observe methods so call sites never touch raw Prometheus types. The
   collector takes a narrow interface (`MetricsCounts`) implemented by the
   store, not the full `store.Store`.
2. **Store decorator** — `metricsStore` embeds `store.Store` and overrides
   exactly `CreateRun`, `MarkRunFinished`, `FinishRun`, and `UpsertStepReport`
   to count successful transitions. Wired once in `cmd/controller/main.go`
   when constructing the server. Reaper-driven failures are therefore counted
   with no changes to reaper code.
3. **HTTP middleware** — one chi middleware registered after `RealIP`,
   recording count + duration using the matched route pattern. The SSE
   endpoint is included; its duration is observed when the stream closes.

New `store.Store` methods (Postgres + any test fakes):

- `CountRunsByStatus(ctx) (map[api.RunStatus]int, error)` — single GROUP BY.
- `CountAgentsByLiveness(ctx, staleAfter time.Duration) (alive, stale int, err error)`.

## Multi-controller Semantics

- Gauges: identical on every replica — aggregate with `max()` in PromQL.
- Counters/histograms: each replica counts the events it processed — aggregate
  with `sum(rate(...))`.
- The docs section includes example queries for failure rate, queue backlog,
  and p95 step duration under a 2-replica deployment.

## Error Handling

- Collector DB errors: log at warn, export
  `unifiedcd_scrape_collector_errors_total`, and omit the affected family from
  that scrape (never fail the whole `/metrics` response).
- The decorator never alters store results: metric work happens only after a
  successful underlying call, and increments cannot fail.

## Testing

- `internal/metrics`: collector unit test against a fake `MetricsCounts`;
  bucket/label assertions via `prometheus/testutil`.
- Store decorator: each overridden method increments on success and does not
  increment on error; pass-through of return values.
- Webhook counter: extend the existing `api_webhooks_test.go` table with
  outcome assertions.
- Endpoint: unauthenticated `GET /metrics` returns 200 and contains the
  expected families; route-pattern label test via a `{id}` route.
- Store: `CountRunsByStatus` / `CountAgentsByLiveness` covered in the existing
  Postgres test suite.

## Documentation

- `docs/operations.md`: new "Metrics" section — endpoint, security note,
  scrape config example (`bearer_token` not required), PromQL examples,
  multi-replica aggregation guidance.
