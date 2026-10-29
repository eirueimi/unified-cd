# Controller and Database

## `token name "env:..." is reserved` / startup warning about impersonating tokens

**Symptom**

- `unified-cli token create env:something` (or `POST /api/v1/tokens`) fails with `token name "env:something" is reserved`.
- The controller logs `removed personal access tokens named "env:UNIFIED_TOKEN" that do not match the configured UNIFIED_TOKEN (a stale row after token rotation, or a user-created token)` at startup, with `count`, `ids` and `roles` attributes.

**Cause**

The `env:` name prefix is reserved for the token synced from `UNIFIED_TOKEN`. Admin (break-glass) status is granted by matching the configured `UNIFIED_TOKEN` hash, never by name. At startup the controller deletes every PAT named `env:UNIFIED_TOKEN` whose hash differs from the configured token's (in the same transaction that upserts the real bootstrap row). Two causes produce the warning:

- **Token rotation**: you changed `UNIFIED_TOKEN`, so the previously synced row no longer matches. That row has role `admin` and was created by the controller at a previous startup.
- **A user-created token**: someone created a PAT with the reserved name (possible on versions before this fix). Its role is usually below `admin`, and it was not created by a controller startup (check the `ids` against earlier `token list` output and its creator).

**Fix**

Choose a token name that does not start with `env:`. If the warning appears without a rotation you performed, a user-created PAT was removed; review who created it and rotate any credentials it could have reached.

## Controller refuses to start with an `oidc.permissions` error

**Symptom**

The controller exits at startup and logs `config file` with one of these errors (the first problem found is reported):

| Error | Cause |
|---|---|
| `oidc.permissions.folders["team-a"].groups["my-org:team-a"]: unknown permission or role "jobs.read"` | A grant names something that is not one of the 25 permissions or `viewer` / `developer` / `admin` (a typo) |
| `oidc.permissions.folders["team-a"].groups["my-org:team-a"]: permission "secret.write" is global-only and cannot be granted on a folder` | A global-only permission appears in a folder entry |
| `oidc.permissions.folders["team-a//x"]: invalid folder: segments must be non-empty and must not be "." or ".."` | An empty, `.` or `..` segment in a folder key |
| `oidc.permissions.folders["team-*"]: invalid folder: folders are literal job-name prefixes, "*", "?", "[" are not allowed` | A wildcard in a folder key |
| `oidc.permissions.folders["team a"]: invalid folder: whitespace is not allowed` | Whitespace in a folder key |
| `oidc.permissions.folders: "team-a" and "/team-a/" are the same folder` | Two keys that are the same folder after trimming `/` |
| `oidc.permissions.folders["team-a"]: no groups or users` | A folder entry with neither |
| `oidc.permissions.global.groups["my-org:x"]: empty grant` | A key with no value (or `[]`) |
| `oidc.permissions: unknown field "deny"` | An unknown key; only `global` and `folders` exist (and `groups` / `users` below them) |
| `oidc.permissions is set but oidc.issuer is not` | The block needs an OIDC issuer; every grant matches an OIDC identity |
| `oidc.roleMap["my-org:devs"]: unknown role "develper" is ignored (valid: admin, developer, viewer)` | With a `permissions` block present, a `roleMap` value that is not a role is an error |

Structural mistakes give `oidc.permissions...: must be a mapping`, `...: duplicate key "<key>"`, `...: keys must be scalars` or `...: grant must be a role or permission name or a list of them`.

**Fix**

Correct the key named in the error and restart. `unified-cli authz show` prints the resolved result once the controller is up. The reference and examples are in [Authorization](../operator-manual/authorization.md#configuring-grants-oidcpermissions).

## Startup warning: unknown role in `roleMap` is ignored

**Symptom**

The controller logs `oidc.roleMap["my-org:devs"]: unknown role "develper" is ignored (valid: admin, developer, viewer)` at startup and users in that group get the `defaultRole` (or nothing).

**Cause and fix**

A `roleMap` value that is not `admin`, `developer` or `viewer` never matches. Without an `oidc.permissions` block this is only a warning (refusing to start would break existing setups); with the block it is a startup error. Fix the spelling. Only `roleMap` is checked: an unknown `userMap` role denies that user.

## Startup warning: `UNIFIED_TOKEN is set while SSO is configured`

**Symptom**

The controller logs `UNIFIED_TOKEN is set while SSO is configured: the env:UNIFIED_TOKEN token is always admin and bypasses oidc.permissions; unset it once SSO works and keep it only as a break-glass credential`.

**Cause and fix**

Both an OIDC issuer and the bootstrap token (`--token`, `UNIFIED_TOKEN`, or `token` in the config file) are set. The bootstrap token holds all 25 permissions and ignores every folder grant. That is its purpose as a break-glass credential, so the controller keeps booting. Once SSO works, either unset `UNIFIED_TOKEN` or keep it stored securely and unused. Without an issuer the token is the only login and nothing is logged.

## Startup warning: role-only personal access tokens bypass folder grants

**Symptom**

With folder grants configured, the controller logs `role-only personal access tokens with a role above viewer bypass folder grants until revoked` with a `count` attribute.

**Cause and fix**

PATs created before matrix authorization have no owner; they carry a global role (`developer` or `admin`) that applies everywhere, so folder grants do not narrow them. The break-glass row is not counted. List them with `unified-cli token list` (as an admin) and re-issue them as owned tokens, ideally with `--permission` ceilings, then revoke the old ones.

## Startup warning: `oidc.permissions is set but oidc.clientId is not`

**Symptom**

The controller logs `oidc.permissions is set but oidc.clientId is not; no OIDC caller can authenticate`.

**Cause and fix**

OIDC is only active when both `oidc.issuer` and `oidc.clientId` are set, so the grants are dead configuration. Set the client ID (and secret for browser SSO).

## `403 forbidden: missing permission "..."`

**Symptom**

A call fails with `403 forbidden: missing permission "run.trigger" on job "team-a/build"` (a folder-scopable permission) or `403 forbidden: missing permission "secret.write"` (a global-only one).

**Cause**

The caller can read the target (or the permission is global-only) but does not hold the named permission, globally or on an ancestor folder of that job. Folder `team-a` covers `team-a/x` and `team-a/sub/x`, not `team-ab/x` and not a root-level job.

**Fix**

Ask which permission the caller holds where: `unified-cli authz explain --user <email> --job <job>` (needs `authz.read`). Grant the permission in `oidc.permissions` and restart. The denial is also in the [audit log](../operator-manual/audit.md#denied-reads) with `permission` and `scope`.

## A job or run returns 404 although the caller expects to read it

**Symptom**

A job that exists, or a run ID that is correct, returns `404` for one caller and works for another. It is absent from that caller's job list.

**Cause**

The caller has no `job.read` on that job, and a hidden job answers exactly like a missing one so existence is not leaked. A new team member with only global `viewer` permissions on a deployment that uses `defaultRole: deny` sees the same.

**Fix**

Grant `job.read` (or a bundle containing it) on the job's folder. Check with `unified-cli authz explain --user <email> --job <job>`; the audit log records the hidden `404` as a denied `job.read`.

## `403 forbidden: no permissions` / `token owner has no permissions`

**Symptom**

Every call from a caller that authenticates fine returns `403 forbidden: no permissions` (browser session or CLI login) or `403 forbidden: token owner has no permissions` (an owned PAT).

**Cause**

The identity authenticated, but nothing in the **current** config grants it anything: it was removed from `roleMap` / `userMap` / `permissions`, its groups changed, or (for a PAT) its owner's grants are gone. Permissions are recomputed on every request.

**Fix**

Restore the grant, or have the user log in again if their groups changed at the IdP (a browser login refreshes the group snapshot of their PATs). Check with `unified-cli authz explain`.

## `403 forbidden: no role` / `forbidden: no role assigned`

**Symptom**

An SSO login ends in `403 forbidden: no role assigned`, a CLI bearer token gets `403 forbidden: no role`, or a role-only PAT or pre-upgrade session gets `403 forbidden: no role`.

**Cause**

For SSO: nothing in `userMap` / `roleMap` / `defaultRole` produced a role **and** no `oidc.permissions` entry matched the identity. For a role-only PAT or a pre-upgrade session: its stored role is empty or unknown. A role-only PAT with an empty role also meets this on the artifact routes, which now require `job.read`.

**Fix**

Map the user's group or email to a role or a grant, or set `defaultRole`. Re-create a role-less PAT with a role.

## Token creation is refused: `cannot create a token`

**Symptom**

`unified-cli token create` (or the Tokens page) fails with one of:

- `cannot create a token: this login predates group-aware tokens; log out and log in again, then retry` (403)
- `cannot create a token: your OIDC token carries no "groups" claim; log in again so the token includes it, then retry` (403; the claim name is your configured `oidc.rolesClaim`)
- `cannot create a token: your identity has no subject (sub); log in again, then retry` (403)
- `cannot grant role "developer": it exceeds your global role "viewer"; use --permission to grant folder permissions you hold` (400; `(none)` instead of the quoted role for a user with only folder grants)
- `cannot grant permission "secret.write": you do not hold it` (400)

**Cause**

A PAT records the creator's identity and groups so that it can never exceed them. The first message is a browser session created before the upgrade (it has no stored groups); log in again. The second is a CLI login whose id_token does not carry the roles claim while the config has group-keyed grants. The CLI device flow requests only the `openid`, `email` and `profile` scopes, so an IdP that emits groups only on request (Dex does) omits them; with a config that has no group-keyed grants an absent claim is simply recorded as no groups and creation works. The 400s are deliberate, nothing is silently reduced: ask for a role or permissions you hold.

**Fix**

Create the token from the Web UI Tokens page (a browser session carries the groups), or log in again; use `--permission` instead of `--role` when you have no global role. For a folder-only user, `token.create` must be granted in `oidc.permissions.global`.

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
renumbered after this database was migrated - see docs/troubleshooting/controller-and-database.md
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
renumbered after this database was migrated - see docs/troubleshooting/controller-and-database.md
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
