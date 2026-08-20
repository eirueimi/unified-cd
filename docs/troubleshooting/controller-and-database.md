# Controller and Database

## Controller logs `dropping log line for sealed run`

**Symptom**

The controller logs `dropping log line for sealed run` (or `dropping log lines for sealed run`).

**Cause**

An agent sent log lines for a run whose logs were already archived (~30 s after the run finished). Log sealing is active whenever an object store is configured, independent of the `--log-trim-days` setting. The archive is the sealed source of truth, so the lines were discarded — storing them would make the run untrimmable and, after a trim, invisible anyway.

**Fix**

Occasional occurrences are expected noise. Common causes include:

- An agent retrying after a network partition (the run was finalized by the stuck-run reaper meanwhile)
- Teardown/buffer flushes arriving later than the archiver delay

If you see a sustained stream of these warnings for the same run, the agent may be stuck and worth restarting.

## A run's log shows `[N log line(s) dropped: controller unreachable]`

**Symptom**

A run's log (on the `stderr` stream, wherever the agent's log pusher for that
step/run was flushing) contains a synthetic line like:

```
[42 log line(s) dropped: controller unreachable]
```

This is distinct from [`dropping log line for sealed
run`](#controller-logs-dropping-log-line-for-sealed-run) above — that one is
logged by the *controller* and means lines arrived too late (after
archival); this one is written into the *run's own log stream* by the
*agent* and means lines never arrived at all.

**Cause**

The agent buffers log lines locally and ships them to the controller in
batches; if the controller is unreachable for a sustained stretch (network
partition, controller restart/outage), the agent keeps the batches it
couldn't send in a bounded in-memory queue (capped at 1MiB). Once that cap
is exceeded, the **oldest** queued batches are evicted to make room for
newer ones — at least the single most recent batch is always kept, even if
it alone exceeds the cap. Rather than let that gap in the run's log pass
silently, the agent counts every discarded line and, on the next flush that
successfully reaches the controller, emits this one synthetic line
reporting exactly how many lines were lost. The counter then resets — if a
second partition causes more drops later in the same run, a second marker
with the new count appears when connectivity next recovers.

**Fix**

The marker itself needs no action — it is a visibility feature, not an
error to fix. It tells you a real gap exists in that step's log around the
time the marker appears (the step almost certainly still ran; only the log
of it is incomplete):

- If the gap coincides with a known controller restart/outage or network
  maintenance window, this is expected — no data beyond the log lines
  themselves was lost (the step's actual execution, its status reports, and
  its artifacts/outputs are unaffected; only the buffered stdout/stderr
  text for that window is gone).
- If you see this marker frequently with no obvious controller-side cause,
  check connectivity and latency between the affected agent and the
  controller — a marginal or high-latency link between them, not the
  controller's own health, is the more likely culprit.
- There is no way to recover the specific dropped lines after the fact; the
  count in the marker is the only remaining record that they existed.

## Controller `/readyz` returns `503 db unavailable`

**Symptom**

`/healthz` returns `200`, but `/readyz` returns:

```text
db unavailable
```

Run, job, agent, claim, or heartbeat requests may stall or fail while the
container still appears alive.

**Cause**

The process is running, but PostgreSQL is unavailable or the API connection
pool cannot provide a connection before the readiness deadline. Pool
exhaustion can result from sizing all controller replicas above PostgreSQL's
`max_connections`; network and database outages produce the same readiness
signal.

**Diagnosis**

Check PostgreSQL activity by state and wait event:

```sql
SELECT state, wait_event_type, wait_event, count(*)
FROM pg_stat_activity
WHERE datname = current_database()
GROUP BY state, wait_event_type, wait_event
ORDER BY count(*) DESC;
```

Compare the controller settings
`UNIFIED_DB_API_MAX_CONNS`, `UNIFIED_DB_BACKGROUND_MAX_CONNS`,
`UNIFIED_DB_LOCK_MAX_CONNS`, and `UNIFIED_DB_LISTEN_MAX_CONNS` across all
replicas with PostgreSQL `max_connections`. The bundled Compose stack uses
`128/32/16/128` per controller and sets the server limit to `1000`.

**Fix**

Restore PostgreSQL connectivity first. If the server is at its connection
limit, reduce per-replica pool maxima or increase `max_connections` with
memory and replica count included in the capacity calculation documented in
[Operations: PostgreSQL connection budgeting](../operator-manual/operations.md#postgresql-connection-budgeting).
Do not rely on `/healthz` for traffic routing; use `/readyz`.

## Controller fails at startup with `schema drift: ... does not exist`

**Symptom**

After upgrading the controller binary/image against an existing database, the
controller **fails fast at startup** (it never finishes booting) with an
error such as:

```
schema drift: schema_migrations.version=7 claims 007_step_call_link is applied,
but step_reports.child_run_id does not exist; migration files were likely
renumbered after this database was migrated - see docs/troubleshooting.md
("Schema drift") for recovery
```

**Cause**

After running `golang-migrate`'s `Up()`, the controller calls `verifySchema()`
(`internal/store/postgres.go`, `internal/store/verify.go`), which cross-checks
`schema_migrations.version` against a sentinel object (a table, column, or
index) for every migration it claims is applied. `golang-migrate` only
compares version *numbers* — if migration files were renumbered (e.g. an
old incremental series was squashed/re-sequenced) after a database was already
migrated to the old numbering, `migrate up` treats that database as fully
migrated and silently skips the renumbered files, even though their schema
objects were never created. `verifySchema()` catches this by probing for the
actual objects and fails startup immediately with a "schema drift" error
instead of letting the controller boot with a stale schema and fail later,
per-request, with errors like `column "role" does not exist`.

This is exactly the same class of drift described in
[Schema drift (migration renumbering)](#schema-drift-migration-renumbering)
below — see that section for full diagnosis and recovery steps (apply the
missing migration's `.up.sql` by hand, then restart so `verifySchema()`
re-checks and confirms the fix).

## Dev stack: controller container unhealthy, `vendor/modules.txt` errors

**Symptom**

`docker compose up` starts the controller container but it never becomes
healthy, and its logs show something like:

```
inconsistent vendoring in /app:
	github.com/some/module@v1.2.3: is explicitly required in go.mod, but not marked as explicit in vendor/modules.txt

	go mod vendor
to sync the vendor directory.
```

**Cause**

The dev `docker-compose` stack's `air` hot-reload containers mount the repo
into the container (`/app`), including the git-ignored local `vendor/`
directory. If `go.mod`/`go.sum` changed (e.g. after a `git pull` or branch
switch) but the local `vendor/` wasn't regenerated, the in-container Go build
fails with this inconsistency error and the controller never passes its
health check.

**Fix**

```bash
go mod vendor
docker compose restart controller agent
```

## Local Kubernetes won't start (`kubelet is not healthy`)

**Symptom**

Docker Desktop's Kubernetes (kind mode) never comes up, and its logs show:

```
The kubelet is not healthy after 4m0s
```

with the hint `required cgroups disabled` in the underlying error.

**Cause**

The Kubernetes node runs inside WSL2, and the kubelet requires cgroup v2.
WSL2 defaults to cgroup v1 on some installs/kernels, so the kubelet fails
its startup health check.

**Fix**

Edit (or create) `%UserProfile%\.wslconfig` and add the kernel command line
switch to force cgroup v2 — put it on its own line, one key per line:

```ini
[wsl2]
kernelCommandLine = cgroup_no_v1=all systemd.unified_cgroup_hierarchy=1
```

Then restart WSL2 so the change takes effect:

```bash
wsl --shutdown
```

Restart Docker Desktop and re-enable Kubernetes. Verify cgroup v2 is active
from inside any WSL2 distro:

```bash
test -f /sys/fs/cgroup/cgroup.controllers && echo "cgroup v2 active"
```

## Schema drift (migration renumbering)

**Symptom:** the controller exits at startup with an error like:

```
schema drift: schema_migrations.version=7 claims 007_step_call_link is applied,
but step_reports.child_run_id does not exist; migration files were likely
renumbered after this database was migrated - see docs/troubleshooting.md
("Schema drift") for recovery
```

**Cause:** migration files were renumbered (typically when parallel branches
merged) after this database had already been migrated. golang-migrate compares
only version numbers, so a database whose recorded version matches an older
numbering silently skips the current file with that number.

**Diagnosis:** compare `SELECT version FROM schema_migrations;` against
`internal/store/migrations/`. The startup error names the first migration
whose objects are missing; later ones may be missing too.

**Recovery:**

1. For each missing migration (start with the one named in the error), apply
   its `.up.sql` statements manually, e.g.:

   ```
   psql "$DSN" -f internal/store/migrations/007_step_call_link.up.sql
   ```

2. Leave `schema_migrations.version` as-is when it already equals the highest
   migration number; only the schema objects were missing.
3. Restart the controller. Startup verification re-runs and confirms the fix.

**If the error says `schema_migrations is dirty` instead:**
Either a previous migration attempt crashed midway, leaving the dirty flag set, or another replica's migration is currently in flight (this can happen transiently during a mixed-version rolling deploy). Restart the controller first; if the error persists across restarts, it is the crashed-migration case, not an in-flight one — inspect which statements of the named version actually applied to the schema, repair them manually to a consistent state, then clear the flag with golang-migrate's `force` command or `UPDATE schema_migrations SET dirty = false` once the schema matches the version number. Then restart the controller.
