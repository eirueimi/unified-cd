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

(Verified in the dev stack: `docker compose exec -T postgres pg_dump --version` reports `pg_dump (PostgreSQL) 16.14`.) Restore into a fresh `unified` database with `psql` before starting the controller — migrations are additive and idempotent (see [Upgrades](#upgrades)), so restoring an older dump and letting the controller migrate forward on next startup is expected to work.

### S3 / object store

Artifacts, cache entries, and log archives live in the configured bucket. Use your S3 provider's bucket versioning and/or cross-region replication for durability — unified-cd itself does not replicate object data. For Garage in production, run distributed mode with `replication_factor >= 2` (see [High Availability Guide](high-availability.md#s3--object-store)).

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
| Leftover `ucd-run-*` pods on Kubernetes | No action needed in the common case — the k8s-agent's pod GC sweeps every ~1 minute and deletes pods whose run has reached a terminal state. A manual `kubectl delete pod ucd-run-...` is safe if you want it gone immediately; it will not resurrect or affect the run's recorded status. |
| PostgreSQL restored from a backup | Start the controller against it; migrations run automatically (see [Upgrades](#upgrades)). Re-apply any resources created after the backup was taken, and confirm `UNIFIED_CONTROLLER_KEY` matches what was in use when secrets were encrypted. |

---

## Monitoring Points

- **`/healthz`** — liveness endpoint; returns `200` when up, `503` while draining/shutting down. Verified live: returns `200` on the dev stack. Use as the load balancer / uptime check target ([High Availability Guide](high-availability.md#health-check-endpoints) also documents `/readyz`, which additionally checks DB connectivity).
- **Agent freshness** — `unified-cli agent list` prints each agent's `last_seen_at` (refreshed by the 15s heartbeat) as the last column. An agent whose timestamp stops advancing is not accepting new claims and any run it's holding is on the clock toward the reaper's 90s staleness threshold.
- **Runs stuck in `Running`** — periodically check for runs that have been `Running` far longer than the job normally takes (`unified-cli run list --job <job-name>`). This can indicate a hung step even before the reaper's agent-liveness check would kick in, since the reaper only acts on a *dead* agent, not a live one stuck in a bad step.
- **Controller logs: AppSource sync failures** — the AppSource reconciler runs on the leader replica only and logs a `WARN` when it fails to sync a Git repo (auth failure, unreachable host, malformed YAML). Watch controller logs for these if you rely on GitOps-style job sync.

---

## Upgrades

Upgrade order: **controller first, then agents.**

1. **Controller** — database migrations run automatically at startup (`internal/store`, via `golang-migrate` against the embedded migration set). Roll controller replicas one at a time in an HA deployment; the new version's migrations apply once, and old and new controller binaries can both be running against the already-migrated schema during a rolling deploy as long as the migration is backward-compatible (additive columns/tables — this is the norm for unified-cd's migration history).
2. **Agents** — upgrade standard agents after the controller is on the new version.
3. **k8s-agent + sidecar image** — the k8s-agent and its auto-injected `unified-artifact` sidecar communicate over a binary exec protocol and **must be upgraded in lockstep**: an old sidecar image paired with a new agent (or vice versa) is incompatible even if the image pulls successfully. Pin `sidecarImage` in the k8s-agent config to the same release as the agent binary on every upgrade (see [Kubernetes Integration Guide: Sidecar image](kubernetes-integration.md#sidecar-image)).

---

See also: [Secrets Management Guide](secrets.md) · [High Availability Guide](high-availability.md) · [Kubernetes Integration Guide](kubernetes-integration.md) · [Troubleshooting](troubleshooting.md)
