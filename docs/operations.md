# Operations Guide

This guide covers what operators need to run unified-cd day to day: where state lives, how to back it up and recover it, and what to monitor.

---

## State Layout

unified-cd's controller is stateless; all durable state lives in two external stores (see the [README architecture diagram](../README.md#architecture) and `docker-compose.yaml`'s `UNIFIED_DB_DSN`/`UNIFIED_S3_*` env vars):

| Store | Contents |
|---|---|
| **PostgreSQL** (`UNIFIED_DB_DSN`) | Jobs, runs/steps, queue state, schedules, webhooks, PATs/sessions, agents, encrypted secrets, `controller_settings` |
| **S3-compatible object store** (`UNIFIED_S3_*`, Garage in dev) | Log archives, artifacts, cache entries, git-template cache |

Losing the PostgreSQL database loses run history and every registered resource (jobs, schedules, webhooks, secrets, GitCredentials, AppSources). Agents are **not** lost from an operator's point of view: both the standard agent and the k8s-agent upsert their row on every claim, so once the DB is restored (or a fresh one is stood up) and an agent's process is still running, it reappears in `agent list` on its own. Everything else must be re-applied:

- Re-`apply` job/schedule/webhook/AppSource/GitCredential YAML.
- Re-`secret set` every secret — secret values are never recoverable from a backup of anything other than the DB itself, and are not retrievable via the API even when present (see [Secrets Management Guide](secrets.md#security-model)).

---

## Backup

### PostgreSQL

Back up with `pg_dump` on a regular schedule:

```bash
docker compose exec -T postgres pg_dump -U unified unified > unified-cd-backup.sql
```

(Verified in the dev stack: `docker compose exec -T postgres pg_dump --version` reports `pg_dump (PostgreSQL) 16.14`.) Restore into a fresh `unified` database with `psql` before starting the controller — migrations are additive and idempotent (see [Upgrades](#upgrades)), so restoring an older dump and letting the controller migrate forward on next startup is expected to work, **unless the dump predates the migrations-001-017 squash** (see the Upgrades exception below and [Troubleshooting](troubleshooting.md#controller-fails-with-column--does-not-exist-after-upgrading)).

### S3 / object store

Artifacts, cache entries, and log archives live in the configured bucket. Use your S3 provider's bucket versioning and/or cross-region replication for durability — unified-cd itself does not replicate object data. For Garage in production, run distributed mode with `replication_factor >= 2` (see [High Availability Guide](high-availability.md#s3--object-store)).

**Run retention.** By default unified-cd keeps every run forever: `runs` rows, log rows, archived logs, and artifacts all accumulate. Note that log archival only *copies* logs to the object store — database log rows are never trimmed by it. Set `--run-retention-days` (env `UNIFIED_RUN_RETENTION_DAYS`) to enable an hourly, leader-elected sweep that deletes terminal runs older than N days, including their archived logs and artifacts. Audit logs have their own independent setting (`--audit-retention-days`).

**Tiered log storage.** Even before run retention fires, `--log-trim-days` (env `UNIFIED_LOG_TRIM_DAYS`) can reclaim the largest table: N days after a run's logs are archived to the object store, an hourly leader-elected sweep deletes the run's `logs` rows and marks the archive record. All log reads for such runs are then served from the archive — the WebUI viewer, CLI, and SSE work unchanged, with a small first-view latency (one object fetch; up to 128 MiB of raw archive bytes are cached in memory — the decoded, in-memory representation can be larger). The sweeper verifies the archive object exists before trimming and never trims unarchived runs. Runs whose logs exceed the 1,000,000-line archive cap, or that received log lines after archival, are never trimmed either — the sweeper detects incomplete archive coverage and skips them. Runs archived before this feature was deployed (records without coverage data) are automatically re-archived once and then trimmed on a later sweep. Typical setup: `--log-trim-days` a few days, `--run-retention-days` much larger.

**Log sealing.** Log sealing is active whenever an object store is configured—even when `--log-trim-days` is 0/disabled—because the archiver runs regardless. Once a run's logs are archived (~30 seconds after it finishes), the archive becomes the sealed source of truth. Log lines arriving after archival are discarded with a controller warning (`dropping log line for sealed run`) to keep the archive consistent; storing them would make the run untrimmable and, after trim, invisible. See Troubleshooting for common causes (agent retries after network partition, late buffer flushes).

**Sweep failure backoff.** The log archiver, run-retention sweeper, and git resolver retry a persistently failing candidate with exponential backoff (1 min doubling to 1 h) instead of letting it occupy the head of every oldest-first batch — a handful of broken runs can no longer starve archival, deletion, or resolution for everything newer. The backoff state is held by the current leader only and resets on failover (each problem candidate is retried once, then re-excluded).

### `UNIFIED_CONTROLLER_KEY` (critical)

This is the master key used to encrypt secrets (AES-256-GCM, see [Secrets Management Guide](secrets.md#security-model)). Back it up wherever you manage secrets (vault, KMS, sealed file) — **independently** of the DB dump:

- If the key is lost, every secret encrypted with it becomes permanently undecryptable, even if the PostgreSQL dump is fully intact. There is no recovery path other than re-`secret set`-ing every value.
- If `UNIFIED_CONTROLLER_KEY` is left unset, the controller generates an ephemeral key on first startup and persists it to the `controller_settings` table in the same database — in that case the key is only as durable as your PostgreSQL backup, and is lost if you ever restore into a different/empty database while the old one is gone. Setting the env var explicitly (and backing it up) avoids that coupling.

---

## Recovery Runbook

| Situation | Action |
|---|---|
| A run is stuck (e.g. no agent can claim it, or it's hung) | `unified-cli run cancel <run-id>` — moves the run to `Cancelled`. Verified live: triggering a `sleep 30` job and running `run cancel <id>` immediately transitioned it to status `Cancelled` in `run list`. |
| An agent dies mid-run | No action needed. The stuck-run reaper detects the stale heartbeat and fails the run automatically — see [High Availability Guide: Orphaned-Run Recovery](high-availability.md#orphaned-run-recovery) for the full heartbeat/staleness/grace timings. In short: heartbeat every 15s, a run is eligible for reaping once its agent's heartbeat is >90s stale, with a 60s grace window after claim, and the run is marked `Failed` (never re-queued, since re-running partially-executed steps can duplicate side effects). |
| An agent claimed a run but the claim response was lost (agent process never learned it owns the run) | No action needed — this now self-heals without waiting for the reaper's stale-heartbeat check. Every agent heartbeat carries the set of run IDs it currently considers active; if the controller has a `Running` run assigned to that agent that is absent from the reported set and has sat claimed for more than ~60s (a grace window protecting a claim whose heartbeat simply hasn't landed yet), it fails that run as orphaned on the *next* heartbeat — typically within a few heartbeat intervals, well before the reaper's 90s-stale-heartbeat path would ever trigger. A legacy agent (built before this feature) sends a bodyless heartbeat and is unaffected — no reconcile runs for it, so it falls back to the existing stale-heartbeat reaper. See [Troubleshooting: a run failed by heartbeat reconcile](troubleshooting.md#run-marked-failed-by-heartbeat-reconcile-after-a-lost-claim). |
| Leftover `ucd-run-*` pods on Kubernetes | No action needed in the common case — the k8s-agent's pod GC sweeps every ~1 minute and deletes pods whose run has reached a terminal state. A manual `kubectl delete pod ucd-run-...` is safe if you want it gone immediately; it will not resurrect or affect the run's recorded status. |
| PostgreSQL restored from a backup | Start the controller against it; migrations run automatically (see [Upgrades](#upgrades)). Re-apply any resources created after the backup was taken, and confirm `UNIFIED_CONTROLLER_KEY` matches what was in use when secrets were encrypted. |

---

## Workspace and Claim-Container Hygiene

Two pieces of standard-agent state accumulate on the agent host over time and, unlike the
k8s-agent's pod GC (see the Recovery Runbook above), are **not** cleaned up automatically by
default:

- **Per-job workspace directories.** Every job gets its own subdirectory under each concurrency
  slot (`wsBase/working<N>/<job-name>`, where `wsBase` is `--workspace-dir` /
  `UNIFIED_AGENT_WORKSPACE_DIR` / the `workspaceDir` config key), and a directory persists for
  every distinct job name ever run in that slot. Disk usage is an operator responsibility —
  include `wsBase` in your normal disk-usage monitoring/cleanup (see [Agent Labels and Routing:
  Workspace lifecycle](agents.md#workspace-lifecycle)).
- **Crash-orphaned claim containers.** If the standard agent process exits ungracefully
  mid-claim (killed, OOM, host reboot), the claim pod's pause and sidecar containers are left
  running (`/.ucd/ucd-sh pause`) instead of being torn down. The host agent has no automatic
  container GC for these — periodically prune claim-pod-shaped containers on agent hosts (see
  [Agent Labels and Routing: Crash-orphaned claim
  containers](agents.md#crash-orphaned-claim-containers)).

Two agent config knobs give operators direct levers over the first item — a
preflight to stop the bleeding, and an opt-in sweep to reclaim space — without
requiring an external cron job. Both are host-agent only (the k8s-agent's
workspaces are pod volumes, reclaimed with the pod). See [Configuration
Reference: Agent Config File](configuration.md#agent-config-file) for the
full flag/env/yaml forms.

- **`minFreeDisk` (`--min-free-disk` / `UNIFIED_AGENT_MIN_FREE_DISK`) — preflight lever.**
  When set, each concurrency slot checks free space on the workspace
  filesystem before claiming a run; below the threshold it skips claiming and
  backs off briefly instead. This is **not an error and not destructive** —
  it never deletes anything and never fails a run — it simply stops that
  agent from making the disk problem worse until space frees up (an operator
  clears old workspaces, an unrelated process on the host frees space, or the
  opt-in GC below runs). Watch the agent log for `free disk space below
  minimum, skipping claim` to know when the lever is engaged. `0` (default)
  disables the check, matching prior behavior.
- **`workspaceRetentionDays` (`--workspace-retention-days` /
  `UNIFIED_AGENT_WORKSPACE_RETENTION_DAYS`) — opt-in GC.** When set to a
  positive number of days, the agent runs a sweep at startup and then hourly
  that removes any `working<slot>/<job>` directory whose modification time is
  older than the retention window. It is deliberately conservative about what
  it will ever touch:
  - **Deletes:** only inactive `wsBase/working<slot>/<job>` directories aged
    past retention.
  - **Protects, always:** `wsBase` itself; `working<slot>` directories
    themselves; any dot-prefixed entry (in particular `.ucd-tools`, the
    `ucd-sh` shim directory); and any `working<slot>/<job>` directory that
    belongs to a run this agent process currently has in flight (checked
    against its live active-claim set on every sweep tick, so a long-running
    job's workspace is never pulled out from under it).
  - **Default is off (`0`).** Persistent per-job workspaces are a feature —
    they act as an inter-run build/dependency cache — so sweeping them away
    is opt-in, not automatic. Enable it once you've confirmed the disk-usage
    growth from stale workspaces outweighs the cache benefit for your job mix
    (e.g. many distinct/short-lived job names sharing a host), and pick a
    retention window comfortably longer than your slowest job's normal
    re-run cadence.

---

## Monitoring Points

- **`/healthz`** — liveness endpoint; returns `200` when up, `503` while draining/shutting down. Verified live: returns `200` on the dev stack. Use as the load balancer / uptime check target ([High Availability Guide](high-availability.md#health-check-endpoints) also documents `/readyz`, which additionally checks DB connectivity).
- **Agent freshness** — `unified-cli agent list` prints each agent's `last_seen_at` (refreshed by the 15s heartbeat) as the last column. An agent whose timestamp stops advancing is not accepting new claims and any run it's holding is on the clock toward the reaper's 90s staleness threshold.
- **Runs stuck in `Running`** — periodically check for runs that have been `Running` far longer than the job normally takes (`unified-cli run list --job <job-name>`). This can indicate a hung step even before the reaper's agent-liveness check would kick in, since the reaper only acts on a *dead* agent, not a live one stuck in a bad step.
- **Controller logs: AppSource sync failures** — the AppSource reconciler runs on the leader replica only and logs a `WARN` when it fails to sync a Git repo (auth failure, unreachable host, malformed YAML). Watch controller logs for these if you rely on GitOps-style job sync.
- **Approval-gate backlog** — visible via `unifiedcd_steps_completed_total{status="WaitingApproval"}`; a growing rate indicates approval gates are piling up faster than they're being actioned.

---

## Upgrades

Upgrade order: **controller first, then agents.**

1. **Controller** — database migrations run automatically at startup (`internal/store`, via `golang-migrate` against the embedded migration set). Roll controller replicas one at a time in an HA deployment; the new version's migrations apply once, and old and new controller binaries can both be running against the already-migrated schema during a rolling deploy as long as the migration is backward-compatible (additive columns/tables — this is the norm for unified-cd's migration history).

   **Exception:** a database provisioned before the migrations-001-017 squash (commit `79c1074`) is **not** upgraded correctly by this automatic `migrate up` — the new migration chain's version numbering starts below where such a database already is, so the migration runner treats it as already up to date and silently applies nothing. This leaves newer columns/tables (e.g. `role`, `managed_resources`, `audit_logs`, `sync_status`) missing. See [Troubleshooting: `column "..." does not exist` after upgrading](troubleshooting.md#controller-fails-with-column--does-not-exist-after-upgrading) for the supported fresh-init/manual-bridge paths.
2. **Agents** — upgrade standard agents after the controller is on the new version.
3. **k8s-agent + sidecar image** — the k8s-agent and its auto-injected `unified-artifact` sidecar communicate over a binary exec protocol and **must be upgraded in lockstep**: an old sidecar image paired with a new agent (or vice versa) is incompatible even if the image pulls successfully. Pin `sidecarImage` in the k8s-agent config to the same release as the agent binary on every upgrade (see [Kubernetes Integration Guide: Sidecar image](kubernetes-integration.md#sidecar-image)).

---

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
| `unifiedcd_steps_completed_total{status}` | counter | Step reports received with a non-Running status |
| `unifiedcd_step_duration_seconds{status}` | histogram | Step wall-clock duration |
| `unifiedcd_webhook_events_total{name,outcome}` | counter | Webhook ingress outcomes |
| `unifiedcd_http_requests_total{method,route,code}` | counter | API traffic by chi route pattern |
| `unifiedcd_http_request_duration_seconds{method,route}` | histogram | HTTP request duration by method and chi route pattern |
| `unifiedcd_scrape_collector_errors_total` | counter | Errors collecting DB-backed gauges (`unifiedcd_runs_current`, `unifiedcd_agents`) at scrape time |

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

# No alive agents (alert if this returns a result for 5m)
max(unifiedcd_agents{state="alive"}) == 0

# p95 step duration
histogram_quantile(0.95, sum(rate(unifiedcd_step_duration_seconds_bucket[1h])) by (le))
```

Ready-made Prometheus alerting rules for these metrics live in
[`deployments/observability/prometheus-alerts.yaml`](../deployments/observability/prometheus-alerts.yaml)
(no alive agents, queue backlog, high failure rate, collector errors).

---

See also: [Secrets Management Guide](secrets.md) · [High Availability Guide](high-availability.md) · [Kubernetes Integration Guide](kubernetes-integration.md) · [Troubleshooting](troubleshooting.md)
