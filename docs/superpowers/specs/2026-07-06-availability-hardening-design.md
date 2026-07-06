# Availability Hardening: k8s-agent Redundancy + Alerting — Design

Date: 2026-07-06
Status: Approved

## Background

Availability review (2026-07-06) left two actionable gaps. (1) The k8s agent
ships with `replicas: 1`; during agent downtime the queued-run reaper fails
waiting runs after a hardcoded 5 minutes. (2) `/metrics` exists but nothing
alerts on it. Postgres and the object store move to managed services and are
out of scope here.

Code review confirmed active-active k8s agents are already safe: run claiming
uses `FOR UPDATE SKIP LOCKED` (no double claim), the Deployment injects a
unique per-pod agent ID via the Downward API (`UNIFIED_K8S_AGENT_ID` =
`metadata.name`), scope pods use `GenerateName`, and pod GC only touches pods
of terminal/absent runs. No leader election is needed or built.

## Part 1 — k8s-agent redundancy

- `manifests/base/k8s-agent/deployment.yaml`: `replicas: 1` → `2`. Mirror the
  same change in the flat manifests that embed the k8s-agent Deployment:
  `manifests/install.yaml`, `manifests/core-install.yaml`,
  `manifests/agent-only.yaml`. Controller/postgres/garage replica counts are
  untouched.
- Queued-run reaper grace becomes configurable:
  `UNIFIED_QUEUED_RUN_GRACE` (Go duration string, e.g. `5m`, `20m`), default
  `5m` — parsed in `cmd/controller/main.go` following the existing
  `UNIFIED_AUDIT_RETENTION_DAYS` pattern (warn + default on invalid values).
  Rationale: with both replicas down (node-pool upgrades), queued runs must
  not be failed before the operator-chosen window.
- Docs: `docs/high-availability.md` (Agent Redundancy section) gains a
  paragraph stating k8s-agent active-active is supported, why it is safe
  (claim atomicity, unique IDs, GenerateName, GC scope), and how the grace
  interacts with full-agent outages. `docs/kubernetes-integration.md` notes
  the new default of 2 replicas.

## Part 2 — Alerting

Two deliverables, complementary; neither adds built-in notification code
(the project direction is notifications via `uses:` templates only).

### 2a. Prometheus alert rules (for the future managed monitoring)

`deployments/observability/prometheus-alerts.yaml` with four rules over the
existing metrics, each with `for:` windows and description annotations:

1. `UnifiedCDNoAliveAgents` — `max(unifiedcd_agents{state="alive"}) == 0`
   for 5m (the comparison yields a series only when true, so the alert fires
   on result presence).
2. `UnifiedCDQueueBacklog` —
   `max(unifiedcd_runs_current{status="Pending"}) + max(unifiedcd_runs_current{status="Queued"}) > 20` for 10m.
3. `UnifiedCDHighFailureRate` —
   `sum(rate(unifiedcd_runs_finished_total{status="Failed"}[30m])) / clamp_min(sum(rate(unifiedcd_runs_finished_total[30m])), 1e-10) > 0.5` for 15m,
   gated on there being actual finishes:
   `and sum(rate(unifiedcd_runs_finished_total[30m])) > 0`.
4. `UnifiedCDScrapeCollectorErrors` —
   `sum(rate(unifiedcd_scrape_collector_errors_total[10m])) > 0` for 10m
   (controller cannot read its own DB gauges).

`docs/operations.md` Metrics section links to the file.

### 2b. Self-monitoring job (works today, no external monitoring)

An example Job + Schedule pair under `examples/self-monitoring/`:

- `Job` `ops/self-monitor`: one `run:` step curls
  `$CONTROLLER_URL/metrics` (params: `controller_url`, thresholds), parses
  with awk/grep, and sets step outputs `alive_agents`, `queue_backlog`,
  `alert_message` (empty when healthy). A second step posts the message via
  the repo's existing Slack template, registered as a job and invoked with
  `call: { job: slack-notify, with: ... }` (per `templates/README.md`; the
  README tells the operator to `unified-cd apply templates/slack-notify.yaml`
  first). It runs with an `if:` condition on the first step's
  `alert_message` output; the webhook URL comes from a unified-cd secret.
- Checks only point-in-time signals: alive agents == 0 and queue backlog >
  threshold. Failure *rate* is intentionally left to the Prometheus rules —
  a stateless scrape cannot compute rates.
- `Schedule` resource triggering it every 5 minutes.
- README in the same directory: setup steps (secret, apply, schedule),
  explicit limitation: **self-monitoring cannot detect a full controller
  outage** — that class needs the external Prometheus rules (2a) or an
  uptime check on `/healthz`.

## Error handling

- Invalid `UNIFIED_QUEUED_RUN_GRACE` → warn log + 5m default (never fatal).
- Self-monitor job failing to reach the controller: the step fails, the run
  is marked Failed and is itself visible in the UI/metrics; the README calls
  this out as a weak self-signal, not a substitute for 2a.

## Testing

- `main.go` grace parsing: unit test following the audit-retention pattern
  (valid, invalid → default, unset → default).
- Manifests: `python -c "import yaml,sys;[yaml.safe_load_all(open(f)) ...]"`
  structural parse of the four changed manifests; grep asserts exactly the
  k8s-agent Deployments changed to `replicas: 2`.
- Alert rules file: YAML parse + promtool syntax check if available
  (documented as optional).
- Self-monitoring job YAML: `unified-cd apply --dry-run`-equivalent parse via
  the dsl package if a parse test exists for examples; otherwise YAML parse.
- No new Go behavior beyond the env parsing — existing suites must stay green.
