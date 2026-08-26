# Operations Guide

This guide covers what operators need to run unified-cd day to day: where state lives, how to back it up and recover it, and what to monitor.

---

## State Layout

unified-cd's controller is stateless; all durable state lives in two external stores (see the [architecture diagram](../getting-started/concepts.md) and `docker-compose.yaml`'s `UNIFIED_DB_DSN`/`UNIFIED_S3_*` env vars):

| Store | Contents |
|---|---|
| **PostgreSQL** (`UNIFIED_DB_DSN`) | Jobs, runs/steps, queue state, schedules, webhooks, PATs/sessions, agents, per-agent identities and credential hashes, encrypted secrets, `controller_settings` |
| **S3-compatible object store** (`UNIFIED_S3_*`, Garage in dev) | Log archives, artifacts, cache entries, git-template cache |

Losing the PostgreSQL database loses run history and every registered resource (jobs, schedules, webhooks, secrets, GitCredentials, AppSources). Agents are **not** lost from an operator's point of view: both the standard agent and the k8s-agent upsert their row on every claim, so once the DB is restored (or a fresh one is stood up) and an agent's process is still running, it reappears in `agent list` on its own. Everything else must be re-applied:

- Re-`apply` job/schedule/webhook/AppSource/GitCredential YAML.
- Re-`secret set` every secret — secret values are never recoverable from a backup of anything other than the DB itself, and are not retrievable via the API even when present (see [Secrets Management Guide](../user-guide/secrets.md#security-model)).

---

Per-agent credentials are stored as hashes in PostgreSQL. After restoring a
fresh database, issue new one-time enrollments and re-enroll the agents rather
than attempting to reuse lost credential state. During a database outage,
agent authentication fails closed with `authentication unavailable` or
`enrollment unavailable`.

## Backup

### PostgreSQL

Back up with `pg_dump` on a regular schedule:

```bash
docker compose exec -T postgres pg_dump -U unified unified > unified-cd-backup.sql
```

(Verified in the dev stack: `docker compose exec -T postgres pg_dump --version` reports `pg_dump (PostgreSQL) 16.14`.) Restore into a fresh `unified` database with `psql` before starting the controller — the migration chain is idempotent and replays forward from whatever version the dump is on, so restoring an older dump and letting the controller migrate forward on next startup is expected to work. Two caveats, both covered under [Upgrades](#upgrades): migrations are **not** all additive, so a dump on migration `014` or earlier loses **every secret and every session** when it is migrated forward; and a dump that **predates the migrations-001-017 squash** is not migrated at all (see the Upgrades exception below and [Troubleshooting](../troubleshooting/controller-and-database.md#schema-drift-migration-renumbering)).

### S3 / object store

Artifacts, cache entries, and log archives live in the configured bucket. Use your S3 provider's bucket versioning and/or cross-region replication for durability — unified-cd itself does not replicate object data. For Garage in production, run distributed mode with `replication_factor >= 2` (see [High Availability Guide](high-availability.md#s3-object-store)).

**Streaming uploads.** Artifact and cache uploads are streamed end-to-end (tar+zstd is produced and sent incrementally) rather than buffered whole in RAM, so the agent's peak memory during an upload is bounded — a compression window plus one object-store multipart part — instead of scaling with the archive size; large artifacts and caches no longer risk OOMing the agent. The agent→controller artifact PUT uses chunked transfer-encoding (no `Content-Length`), so a length-sensitive proxy sitting in front of the controller must be configured to allow chunked request bodies. Trade-off: because the upload body is streamed and not rewindable, a mid-upload network failure fails that upload outright rather than being transparently retried internally — the step's normal retry/`continueOnError` semantics still apply on top of that.

**Run retention.** By default unified-cd keeps every run forever: `runs` rows, log rows, archived logs, and artifacts all accumulate. Note that log archival only *copies* logs to the object store — database log rows are never trimmed by it. Set `--run-retention-days` (env `UNIFIED_RUN_RETENTION_DAYS`) to enable an hourly, leader-elected sweep that deletes terminal runs older than N days, including their archived logs and artifacts. Audit logs have their own independent setting (`--audit-retention-days`).

**Tiered log storage.** Even before run retention fires, `--log-trim-days` (env `UNIFIED_LOG_TRIM_DAYS`) can reclaim the largest table: N days after a run's logs are archived to the object store, an hourly leader-elected sweep deletes the run's `logs` rows and marks the archive record. All log reads for such runs are then served from the archive — the WebUI viewer, CLI, and SSE work unchanged, with a small first-view latency (one object fetch; up to 128 MiB of raw archive bytes are cached in memory — the decoded, in-memory representation can be larger). The sweeper verifies the archive object exists before trimming and never trims unarchived runs. Runs whose logs exceed the 1,000,000-line archive cap, or that received log lines after archival, are never trimmed either — the sweeper detects incomplete archive coverage and skips them. Runs archived before this feature was deployed (records without coverage data) are automatically re-archived once and then trimmed on a later sweep. Typical setup: `--log-trim-days` a few days, `--run-retention-days` much larger.

**Log sealing.** Log sealing is active whenever an object store is configured—even when `--log-trim-days` is 0/disabled—because the archiver runs regardless. Once a run's logs are archived (~30 seconds after it finishes), the archive becomes the sealed source of truth. Log lines arriving after archival are discarded with a controller warning (`dropping log line for sealed run`) to keep the archive consistent; storing them would make the run untrimmable and, after trim, invisible. See Troubleshooting for common causes (agent retries after network partition, late buffer flushes).

**Sweep failure backoff.** The log archiver, run-retention sweeper, and git resolver retry a persistently failing candidate with exponential backoff (1 min doubling to 1 h) instead of letting it occupy the head of every oldest-first batch — a handful of broken runs can no longer starve archival, deletion, or resolution for everything newer. The backoff state is held by the current leader only and resets on failover (each problem candidate is retried once, then re-excluded).

### The controller's master key (critical)

This is the master key (KEK) used to encrypt secrets (AES-256-GCM, see [Secrets Management Guide](../user-guide/secrets.md#security-model)). The controller refuses to start unless it is given one, via exactly one of:

- `UNIFIED_CONTROLLER_KEY_FILE` — path to a file containing 64 hex characters (`unified-cli keygen --out /etc/unified-cd/kek`). The supported way to run in production; a file (not an env var) is used deliberately, since env vars leak into `docker inspect`, process listings, crash dumps, and child processes.
- `UNIFIED_KMS_URI` — an external KMS. `hashivault://[<mount>/]<key>` wraps the key with
  HashiCorp Vault or OpenBao Transit; see [Secrets Management Guide: Using Vault or OpenBao
  (Transit)](../user-guide/secrets.md#using-vault-or-openbao-transit).
- `UNIFIED_DEV_MODE=1` — generates an ephemeral in-memory key for local development only. Secrets become unreadable the moment the process restarts.

Back the key file up wherever you manage secrets (vault, KMS, sealed file, offline copy) — **independently** of the DB dump, and **never** in the same place/backup as the PostgreSQL dump it protects:

- If the key is lost, every secret encrypted with it becomes permanently undecryptable, even if the PostgreSQL dump is fully intact. There is no recovery path other than re-`secret set`-ing every value.
- Unlike earlier versions, the controller does **not** escrow a copy of the key into the database. Losing the key file (with no independent backup) is unrecoverable even if the database is perfectly intact — there is no fallback copy to fall back to.

---

## Recovery Runbook

| Situation | Action |
|---|---|
| A run is stuck (e.g. no agent can claim it, or it's hung) | `unified-cli run cancel <run-id>` — moves the run to `Cancelled`. Verified live: triggering a `sleep 30` job and running `run cancel <id>` immediately transitioned it to status `Cancelled` in `run list`. |
| An agent dies mid-run | No action needed. The stuck-run reaper detects the stale heartbeat and fails the run automatically — see [High Availability Guide: Orphaned-Run Recovery](high-availability.md#orphaned-run-recovery) for the full heartbeat/staleness/grace timings. In short: heartbeat every 15s, a run is eligible for reaping once its agent's heartbeat is >90s stale, with a 60s grace window after claim, and the run is marked `Failed` (never re-queued, since re-running partially-executed steps can duplicate side effects). |
| An agent claimed a run but the claim response was lost (agent process never learned it owns the run) | No action needed — this now self-heals without waiting for the reaper's stale-heartbeat check. Every agent heartbeat carries the set of run IDs it currently considers active; if the controller has a `Running` run assigned to that agent that is absent from the reported set and has sat claimed for more than ~60s (a grace window protecting a claim whose heartbeat simply hasn't landed yet), it fails that run as orphaned on the *next* heartbeat — typically within a few heartbeat intervals, well before the reaper's 90s-stale-heartbeat path would ever trigger. A legacy agent (built before this feature) sends a bodyless heartbeat and is unaffected — no reconcile runs for it, so it falls back to the existing stale-heartbeat reaper. See [Troubleshooting: a run failed by heartbeat reconcile](../troubleshooting/runs-and-scheduling.md#run-marked-failed-by-heartbeat-reconcile-after-a-lost-claim). |
| Leftover `ucd-run-*` pods on Kubernetes | No action needed in the common case — the k8s-agent's pod GC sweeps every ~1 minute and deletes pods whose run has reached a terminal state. A manual `kubectl delete pod ucd-run-...` is safe if you want it gone immediately; it will not resurrect or affect the run's recorded status. |
| PostgreSQL restored from a backup | Start the controller against it; migrations run automatically (see [Upgrades](#upgrades)). Re-apply any resources created after the backup was taken, and confirm the key file at `UNIFIED_CONTROLLER_KEY_FILE` (or the `UNIFIED_KMS_URI` key) matches what was in use when secrets were encrypted — there is no database copy of the key to fall back on. |

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
Reference: Agent Config File](../reference/configuration.md#agent-config-file) for the
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
  - **Do not enable with multiple agent processes sharing one workspace
    base.** The active-run protection above is per-agent-process only — a
    sweep only ever sees its own process's in-flight claims, so if two or
    more agent processes point at the same `wsBase` (workspace directory),
    one process's GC can remove a `working<slot>/<job>` directory a *different*
    process currently has a run in, causing data loss mid-run. Only enable
    this GC when each agent process has an exclusive workspace base.
  - **Default is off (`0`).** Persistent per-job workspaces are a feature —
    they act as an inter-run build/dependency cache — so sweeping them away
    is opt-in, not automatic. Enable it once you've confirmed the disk-usage
    growth from stale workspaces outweighs the cache benefit for your job mix
    (e.g. many distinct/short-lived job names sharing a host), and pick a
    retention window comfortably longer than your slowest job's normal
    re-run cadence.

---

## Monitoring Points

- **`/healthz`** — liveness endpoint; returns `200` while the process is running and `503` while draining/shutting down. Use it only to decide whether the process must be restarted.
- **`/readyz`** — readiness endpoint; also acquires an API-pool connection and pings PostgreSQL. Use it for Docker/Kubernetes readiness and load-balancer routing. A `503 db unavailable` response means the controller must not receive traffic even if `/healthz` is still `200`.
- **Agent freshness** — `unified-cli agent list` prints each agent's `last_seen_at` (refreshed by the 15s heartbeat) as the last column. An agent whose timestamp stops advancing is not accepting new claims and any run it's holding is on the clock toward the reaper's 90s staleness threshold.
- **Runs stuck in `Running`** — periodically check for runs that have been `Running` far longer than the job normally takes (`unified-cli run list --job <job-name>`). This can indicate a hung step even before the reaper's agent-liveness check would kick in, since the reaper only acts on a *dead* agent, not a live one stuck in a bad step.
- **Controller logs: AppSource sync failures** — the AppSource reconciler runs on the leader replica only and logs a `WARN` when it fails to sync a Git repo (auth failure, unreachable host, malformed YAML). Watch controller logs for these if you rely on GitOps-style job sync.
- **Approval-gate backlog** — visible via `unifiedcd_steps_completed_total{status="WaitingApproval"}`; a growing rate indicates approval gates are piling up faster than they're being actioned.

### PostgreSQL connection budgeting

The controller isolates four PostgreSQL pools so advisory locks and SSE
listeners cannot consume API capacity:

| Pool | Default maximum | Purpose |
|---|---:|---|
| API | 128 | HTTP APIs, authentication, agents, metrics, bootstrap |
| Background | 32 | Scheduler, reapers, archivers, retention, reconcilers |
| Lock | 16 | Session-level advisory locks |
| Listen | 128 | SSE PostgreSQL `LISTEN` sessions |

The repository's Docker Compose PostgreSQL service sets
`max_connections=1000`. For an external database, configure a limit satisfying:

```text
controller replicas × (API + background + lock + listen)
  + migration and administrative reserve
  < PostgreSQL max_connections
```

Pool maxima are capacity limits, not startup allocation: pgx opens connections
as demand grows. Leave the minimum at zero unless the database is sized for
every replica's pre-opened connections.

---

## Upgrades

> **⚠ Upgrading across database migrations `015`/`016` deletes every secret and
> every login session.** Plan for it before you start, not after.
>
> Two migrations destroy data unconditionally, with no `WHERE` clause and no prompt:
>
> | Migration | Statement | Effect |
> |---|---|---|
> | `015_secrets_v2.up.sql:13` | `DELETE FROM public.sessions;` | **Every login session is cleared.** Refresh tokens were stored in plaintext and are replaced by envelope-encrypted columns; rows in the old format cannot be re-encrypted. Every user must log in again. |
> | `016_drop_secret_scope.up.sql:13` | `DELETE FROM public.secrets;` | **Every stored secret is deleted.** Dropping `scope`/`scope_ref` changes the AES-GCM additional authenticated data, so *no* existing ciphertext — global rows included — can be authenticated afterwards. |
>
> **A PostgreSQL backup does not save you.** Restoring a pre-upgrade dump brings
> the secret rows back, but the new controller still cannot decrypt them: the
> binding they were sealed under no longer exists. There is no in-place recovery
> path. Recovery means running `unified-cli secret set` again for every secret,
> from your own source of truth — unified-cd has never been able to hand a
> value back to you (see [Secrets Management Guide](../user-guide/secrets.md#security-model)).
>
> **Before starting an upgrade that crosses `015`/`016`, confirm you can still
> obtain the plaintext of every secret you have set.** Run `unified-cli secret
> list` to enumerate the names; the values must come from wherever you
> originally got them.
>
> Migration `016`'s own comments (`016_drop_secret_scope.up.sql:7-11`) justify the
> deletion by asserting there are "no production secrets to preserve." That was a
> statement about the project at the time the migration was written. It is not a
> claim about *your* database, and it is not a safe assumption for anyone
> upgrading an installation that has been in use.

Upgrade order: **controller first, then agents.**

1. **Controller** — database migrations run automatically at startup (`internal/store`, via `golang-migrate` against the embedded migration set). Roll controller replicas one at a time in an HA deployment; the new version's migrations apply once.

   **Do not assume a migration is backward-compatible.** unified-cd's migration history is *not* uniformly additive. Of the 17 embedded migrations, **5 are backward-incompatible** — `003`, `005`, `014`, `015`, `016`. Between them they drop **6 columns** and **5 constraints**, set **3 columns** `NOT NULL` and then `DROP DEFAULT`, destroy data in **3 places** (2 of them unqualified `DELETE FROM`), and tighten **4 constraints** that appear in the DDL only as `ADD`s: two widened primary keys (`005`), one new `UNIQUE` (`016`), and one narrowed `CHECK` (`014`). The tightened-constraint and `DROP DEFAULT` cases are the dangerous shape, because an older binary still **starts** cleanly against the migrated schema and only fails later, at write time — an `INSERT` that omits the newly-mandatory column raises a `NOT NULL` violation, and rows that were legal under the old primary key now collide.

   **How far back this bites.** During a rolling deploy the exposure is to a controller **two releases behind (N-2), not N-1.** `v0.4.0` already embeds `001`–`016`, and `017` is purely additive, so a `v0.4.0` controller runs correctly against a schema migrated by the current tree. A `v0.3.0` controller embeds only `001`–`012` and would be running against `014`/`015`/`016`. Migrations `003` and `005` are not reachable from any released binary at all — `v0.0.1` already embeds `001`–`007`. So: **rolling one release forward is safe; running binaries more than one release apart against a shared database is not supported.** Before any upgrade that spans more than one release, take the controllers down rather than rolling them.

   **Exception:** a database provisioned before the migrations-001-017 squash (commit `79c1074`) is **not** upgraded correctly by this automatic `migrate up` — the new migration chain's version numbering starts below where such a database already is, so the migration runner treats it as already up to date and silently applies nothing. This leaves newer columns/tables (e.g. `role`, `managed_resources`, `audit_logs`, `sync_status`) missing. See [Troubleshooting: `column "..." does not exist` after upgrading](../troubleshooting/controller-and-database.md#schema-drift-migration-renumbering) for the supported fresh-init/manual-bridge paths.
2. **Agents** — upgrade standard agents after the controller is on the new version.
3. **k8s-agent + sidecar image** — the k8s-agent and its auto-injected `unified-artifact` sidecar communicate over a binary exec protocol and **must be upgraded in lockstep**: an old sidecar image paired with a new agent (or vice versa) is incompatible even if the image pulls successfully. Pin `sidecarImage` in the k8s-agent config to the same release as the agent binary on every upgrade (see [Kubernetes Integration Guide: Sidecar image](kubernetes-integration.md#sidecar-image)).
4. **Default runner/pause image digest pin** — see [Rotating the default runner/pause image digests](#rotating-the-default-runnerpause-image-digests) below. This step is easy to forget because the build succeeds either way; forgetting it just means agents keep pulling the old image forever.

### Checking which version is running

An upgrade is a fleet mid-way between two versions, so every unified-cd
binary reports the release tag it was built from. Released images are stamped
from the git tag by `.github/workflows/release-docker.yml`; anything built
another way (including a plain `go build`) reports `dev`.

| Component | How to read its version |
| --- | --- |
| Controller | First line of its log (`"unified-cd controller starting" version=...`), the `unifiedcd_build_info{version}` gauge on `/metrics`, or `docker run <controller-image> --version` |
| Agent | `version` field in `GET /api/v1/agents` and on the web UI's Agents page, or `unified-cd-agent --version` |
| k8s-agent | Same `GET /api/v1/agents` field, or `kubectl exec <pod> -- /k8s-agent --version` |
| Artifact sidecar | `kubectl exec <pod> -c unified-artifact -- unified-sidecar version` — the only way to verify the lockstep pin in step 3 above |
| CLI | `unified-cd version` |
| Runner image | No unified-cd binary and therefore no version; identified by its digest pin (see [Rotating the default runner/pause image digests](#rotating-the-default-runnerpause-image-digests)) |

**Nothing enforces these versions.** The controller does not compare its
version against an agent's and will never refuse an old agent — compatibility
is decided by capabilities (see [Agents: Capability-based
routing](agents.md#agent-version)), which is deliberately permissive so a
rolling upgrade does not strand runs. These values exist so an operator can
*see* the fleet's state; treat a mismatch as information, not as an error the
system has already handled.

---

### Rotating the default runner/pause image digests

The fleet-wide default images — the primary container for isolated jobs that
don't supply their own `podTemplate` job container, the claim pod's pause
(netns-holder) container, and the k8s-agent's auto-injected artifact
sidecar — are referenced by **digest**, not by mutable tag:

- Host agent: `defaultRunnerImage` / `defaultPauseImage` constants in `cmd/unified-cd-agent/main.go`
- k8s-agent: `defaultPodImage` constant in `internal/k8sagent/config.go` (also consumed by the fallback in `internal/k8sagent/podbuilder.go`'s `defaultPodSpec`)
- k8s-agent artifact sidecar: `defaultSidecarImage` constant in `internal/k8sagent/config.go` (fleet-wide default for `Config.SidecarImage`, auto-injected into every k8s-agent pod by `internal/k8sagent/podbuilder.go`'s `buildArtifactSidecarContainer` — not job-author-controlled, and this sidecar holds long-lived, bucket-scoped static S3 credentials, so treat its pin with at least the same care as the runner/pod image)

This is deliberate: a bare tag like `v0.0.3` or `1.36` looks immutable but
isn't — whoever controls that registry repository can force-push the tag and
execute code in the primary container of every isolated job on every agent
that lacks its own `podTemplate` job container. Pinning to
`repo:tag@sha256:<digest>` keeps the tag for human readability while the
digest is what actually gets pulled and enforced.

**The cost of this is that the pin rots silently.** When the runner image is
rebuilt and re-tagged (e.g. a new `unified-cd-runner` release), agents keep
pulling the *old* image at the *old* digest until someone manually updates
the constant. There is no automated check that the pinned digest still
matches the tag's current content. Treat updating the pin as a **required
step of every runner-image release**, not an optional follow-up:

1. After publishing the new `unified-cd-runner` image (and whenever bumping
   the pinned `busybox` version), resolve the new manifest-list digest:

   ```bash
   docker buildx imagetools inspect ghcr.io/eirueimi/unified-cd-runner:vX.Y.Z
   docker buildx imagetools inspect busybox:1.36
   docker buildx imagetools inspect ghcr.io/eirueimi/unified-cd-artifact-sidecar:vX.Y.Z
   ```

   Use the **top-level `Digest:`** from the multi-arch index output, not one
   of the per-platform manifest digests listed under `Manifests:` — a
   per-platform digest would break agents on other architectures.

2. Update, in the same commit:
   - `cmd/unified-cd-agent/main.go`: `defaultRunnerImage` and/or `defaultPauseImage`
   - `internal/k8sagent/config.go`: `defaultPodImage` and/or `defaultSidecarImage`
   - Bump the tag portion of the string (e.g. `v0.0.3` → `vX.Y.Z`) alongside the digest so the two never drift apart. **Exception: `defaultSidecarImage`** is intentionally pinned as `...:latest@sha256:<digest>` — there is no `vX.Y.Z` release tag to bump for this image, so only its digest changes on rotation; the tag portion (`latest`) stays as-is.

3. Record both old and new digests in the commit message so the pin change
   is auditable in `git log`/`git blame`.

4. Run `go test ./cmd/... ./internal/agent/ ./internal/k8sagent/ -count=1` —
   `TestDefaultImagesAreDigestPinned` (`cmd/unified-cd-agent/main_test.go`),
   `TestDefaultPodImageIsDigestPinned`, and `TestDefaultSidecarImageIsDigestPinned`
   (both in `internal/k8sagent/config_test.go`) only assert the string is a
   well-formed `@sha256:<64 hex chars>` pin, so they will **not** catch a
   stale-but-still-pinned digest; a full pull/smoke-test of the new image on
   at least one host and one k8s agent is the real verification that the
   rotation took effect.

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
| `unifiedcd_build_info{version}` | gauge | Always `1`; the controller's build version is the label. See [Checking which version is running](#checking-which-version-is-running) |
| `unifiedcd_run_time_to_claim_seconds` | histogram | Time from run creation to an agent claiming it. Spans Pending (git template resolution, concurrency gating) as well as Queued — it is the wait a CI user actually experiences, not pure queue time |
| `unifiedcd_background_task_runs_total{task,outcome}` | counter | Background worker passes, by outcome (`success` / `error`) |
| `unifiedcd_background_task_duration_seconds{task}` | histogram | Background worker pass duration |
| `unifiedcd_background_task_items_total{task,result}` | counter | Items a worker acted on, by per-item result (`ok` / `error`) |
| `unifiedcd_log_lines_ingested_total{result}` | counter | Log lines received from agents (`accepted` / `dropped`; dropped means the run was sealed) |
| `unifiedcd_log_bytes_ingested_total` | counter | Bytes of log content received, accepted or not |
| `unifiedcd_db_pool_connections{pool,state}` | gauge | Postgres pool connections by pool (`api`, `background`, `lock`, `listen`) and state (`acquired` / `idle` / `total`) |
| `unifiedcd_db_pool_max_connections{pool}` | gauge | Configured ceiling per pool |
| `unifiedcd_db_pool_empty_acquires_total{pool}` | counter | Acquires that found no free connection and had to wait |
| `unifiedcd_db_pool_canceled_acquires_total{pool}` | counter | Acquires abandoned because the caller's context ended while waiting |
| `go_*`, `process_*` | various | Standard Go runtime and process collectors (goroutines, heap, GC pause, RSS, CPU, open FDs) |

### Watching the background workers

Nine background workers run on tickers with no caller waiting on them, so a
worker that fails every pass has nothing else to surface it. `outcome="error"`
catches a pass that failed outright.

`result="error"` catches the subtler case. Several of these workers iterate a
batch and deliberately swallow per-item failures so one bad item cannot abort
the sweep — the log archiver logs a run it could not archive and moves on,
returning no error. Pass-level outcome therefore reports **success** for a
worker whose every single item failed, and without the per-item counter a
dashboard cannot tell "nothing to archive" from "nothing archivable".

Instrumented tasks: `log_archiver`, `log_trim`, `run_retention`,
`audit_retention`, `cache_cleanup`, `approval_reaper`, `stuck_run_reaper`,
`queued_run_reaper`, `appsource_sync_reaper`.

Not instrumented: the scheduler, the git resolver, and the AppSource
reconciler. Their loop bodies carry leader state inline rather than through a
single pass function, so there is no one place to time. The scheduler is still
observable indirectly — `unifiedcd_runs_current{status="Pending"}` climbing
with `unifiedcd_run_time_to_claim_seconds` is the signal that it has stopped.

### Watching the connection pools

The four pools are separately bounded (`api` 128, `background` 32, `lock` 16,
`listen` 128 by default) so background work cannot starve the API. A **bounded**
pool under pressure does not error — it queues — so `empty_acquires_total` is
the only signal that a pool is too small, and the acquired/max ratio is the
only way to tell whether the isolation the split exists for is holding.

With multiple controller replicas, gauges report identical values on every
replica (aggregate with `max()`); counters count only events the scraped
replica processed (aggregate with `sum(rate(...))`).

`unifiedcd_build_info` is the exception to the "gauges report identical
values on every replica" rule: each replica reports its own build, so during
a rolling controller upgrade `count(count by (version) (unifiedcd_build_info))`
is greater than 1. That is the intended signal — an alert on it staying above
1 catches a rollout that stalled half-finished.

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

# A background worker whose items all fail while its passes report success
sum(rate(unifiedcd_background_task_items_total{result="error"}[15m])) by (task)

# A background worker that has stopped running at all (alert if this drops to 0)
sum(rate(unifiedcd_background_task_runs_total[15m])) by (task)

# Connection pool saturation, per pool
max(unifiedcd_db_pool_connections{state="acquired"}) by (pool)
  / max(unifiedcd_db_pool_max_connections) by (pool)

# Requests waiting on a connection — a bounded pool's only failure signal
sum(rate(unifiedcd_db_pool_empty_acquires_total[5m])) by (pool)

# p95 wait before a run starts executing
histogram_quantile(0.95, sum(rate(unifiedcd_run_time_to_claim_seconds_bucket[1h])) by (le))

# Goroutine leak
max(go_goroutines) by (instance)
```

Ready-made Prometheus alerting rules for these metrics live in
[`deployments/observability/prometheus-alerts.yaml`](https://github.com/eirueimi/unified-cd/blob/main/deployments/observability/prometheus-alerts.yaml)
(no alive agents, queue backlog, high failure rate, collector errors).

---

See also: [Secrets Management Guide](../user-guide/secrets.md) · [High Availability Guide](high-availability.md) · [Kubernetes Integration Guide](kubernetes-integration.md) · [Troubleshooting](../troubleshooting/index.md)
