# Audit Log

unified-cd records a persistent audit trail of state-changing API operations and of refused reads: who did what, on
which resource, when, with what result, and which permission was checked and which grant allowed it. This complements the existing per-run approval
audit trail (`run_approvals` — see [Job Reference](../user-guide/writing-jobs/index.md) for the `approval` step type), which
only covered approve/reject decisions.

> The CLI is referred to as `unified-cd` throughout. This is the built binary (`./bin/unified-cd`);
> source is under `cmd/unified-cli`. During development you can also use `go run ./cmd/unified-cli ...`.

## What is recorded

Every **state-changing** (`POST`, `PUT`, `DELETE`) call to the controller's human-facing API is
recorded, allowed or denied, for the following actions. A **denied read** is recorded too (see
[Denied reads](#denied-reads)):

| Action | Route |
|---|---|
| `job.apply` | `POST /api/v1/jobs` |
| `job.delete` | `DELETE /api/v1/jobs/{name}` |
| `run.trigger` | `POST /api/v1/runs` |
| `run.cancel` | `POST /api/v1/runs/{id}/cancel` |
| `run.replay` | `POST /api/v1/runs/{id}/replay` |
| `run.delete` | `DELETE /api/v1/runs/{id}` |
| `run.approval.decide` | `POST /api/v1/runs/{runID}/approvals/{stepIndex}` |
| `secret.set` | `POST /api/v1/secrets` |
| `secret.delete` | `DELETE /api/v1/secrets/{name}` |
| `gitcredential.upsert` | `POST /api/v1/gitcredentials` |
| `gitcredential.delete` | `DELETE /api/v1/gitcredentials/{name}` |
| `token.create` | `POST /api/v1/tokens` |
| `token.delete` | `DELETE /api/v1/tokens/{id}` |
| `webhook.apply` | `POST /api/v1/webhooks` |
| `webhook.delete` | `DELETE /api/v1/webhooks/{name}` |
| `vars.apply` | `POST /api/v1/vars` |
| `vars.delete` | `DELETE /api/v1/vars/{name}` |
| `schedule.apply` | `POST /api/v1/schedules` |
| `schedule.delete` | `DELETE /api/v1/schedules/{name}` |
| `appsource.apply` | `POST /api/v1/appsources` |
| `appsource.delete` | `DELETE /api/v1/appsources/{name}` |
| `appsource.sync` | `POST /api/v1/appsources/{name}/sync` |
| `agent.enrollment.create` / `agent.enrollment.revoke` | `POST` / `DELETE /api/v1/agent-enrollments...` |
| `agent.policy.create` / `agent.policy.update` / `agent.policy.delete` | enrollment-policy write routes |
| `agent.identity.enable` / `agent.identity.disable` / `agent.credentials.revoke` | agent identity lifecycle routes |

Each recorded entry contains:

| Field | Description |
|---|---|
| `occurredAt` | Timestamp of the request |
| `actor` | The authenticated caller: PAT name, OIDC email/subject, or session identity — whatever `ServerAuth` resolved as the `Principal` for the request (the same identity source used for `run_approvals.decided_by`) |
| `method` | HTTP method (`POST`/`PUT`/`DELETE`) |
| `path` | Request path |
| `action` | Short classification from the table above |
| `resource` | Best-effort name/id of the target (e.g. job name, secret name, token id) |
| `status` | HTTP response status code the client actually saw |
| `permission` | The permission the request was checked against (for example `run.trigger`); empty when no decision applied |
| `scope` | The job name for a folder-scopable decision; empty for a global decision and for a refusal before any job was looked at |
| `grantedBy` | A short description of the first grant that allowed the request; empty on a denial |

### Permission, scope and grantedBy

`permission`, `scope` and `grantedBy` record the authorization decision. The `grantedBy` string
names the first grant that allowed the request (global grants are listed before folder grants,
shallower folders before deeper ones). The forms are:

| `grantedBy` | Meaning |
|---|---|
| `role:developer (roleMap["my-org:devs"])` | The global role came from a `roleMap` entry (also `userMap["alice@example.com"]`, `userMap["<sub>"]` or `defaultRole`) |
| `global (groups["my-org:engineering"])` / `global (users["alice@example.com"])` | An `oidc.permissions.global` entry |
| `folder:team-a (groups["my-org:team-a"])` / `folder:team-a (users["contractor@example.com"])` | An `oidc.permissions.folders` entry |
| `role:developer (pat)` | A role-only PAT (no owner) with that stored role |
| `role:developer (session)` | A session created before the upgrade (no stored claims) |
| `... & pat-ceiling` | The grant above, as narrowed by an owner PAT's ceiling |
| `break-glass` | The `UNIFIED_TOKEN` token |

`unified-cli authz explain` prints the same strings, so you can ask why a call was allowed.
`scope` is empty for global decisions (secrets, tokens, agents, ...) and also when a request is
refused by the cheap pre-check ("you hold this permission nowhere") before the job is known.

### Denied reads

Allowed reads are never recorded (log tailing and list polling would flood the table). Every
**denied** read is: a `403`, and a request answered as hidden because the caller lacks `job.read`
(the client saw `404`, but the row is recorded as a denial). The row's `status` is the status the
client actually saw: a hidden resource is answered `404`, `204` (deleting a hidden schedule), or
`400` (applying a schedule whose target is hidden), not `403`. For a denied read or an unclassified
write the row's `action` is the permission that was checked (for example `job.read`) and `resource`
is the scope. Denied calls to `GET /api/v1/authz/policy` and `POST /api/v1/authz/explain` are
recorded this way (`authz.read`); allowed calls are not.

Denied reads are written synchronously, so an authenticated caller who holds no permissions can
grow `audit_logs` at request rate. Size the database for that and keep
[retention](#retention) (`--audit-retention-days`) enabled; with `0` the rows are kept forever.

**Secrets are special-cased:** for `secret.set`, only the secret's `name` is ever read out of the
request body — the `value` field is never inspected, logged, or stored anywhere in the audit
trail. `secret.delete` uses the URL path parameter, so no body is involved at all.

## What is excluded

The following are deliberately **not** recorded:

- **Agent-facing endpoints** (`/api/v1/agents/register`, `/heartbeat`, `/claim`, `/steps`,
  `/logs`, `/finish`, `/outputs`, `/secrets/fetch`, agent-created approvals, etc.) — these use
  a per-agent principal, not a human `Principal`, and would otherwise flood
  the audit log with routine bookkeeping.
- **Credential plaintext and hashes** — enrollment creation is audited by its
  metadata/action only. Access, refresh, and enrollment credentials are never
  audit fields, response-captured values, or metric labels.
- **Webhook payload ingress** (`POST /webhook/{name}`) — this is an unauthenticated-by-identity
  ingress path (verified by HMAC signature, not a human principal); the run it triggers is
  already visible via `triggeredBy` on the run itself.
- **Auth / OIDC endpoints** (`/api/v1/auth/*`, login/callback/logout) — these are the
  authentication mechanism itself, not a state-changing operation on a managed resource.
- **Log-append-style / high-volume routes** (agent log/step/output submission) — see
  "agent-facing endpoints" above; these are the same set.
- **Allowed `GET` requests** — read-only calls are audited only when authorization refuses them
  (see [Denied reads](#denied-reads)).
- **Refusals that involve no human authorization decision** — an agent principal calling a
  human-only route, `401 unauthorized` responses, and `403 forbidden: unclassified route` are not
  recorded.
- **Artifact upload and allowed downloads** — upload is agent-authenticated and excluded along with the
  rest of the agent-facing surface; a download is a read, so only a refused one (no `job.read` on the
  run's job) is recorded.

Allowed state-changing calls are recorded only for routes explicitly present in the middleware's
action table (an allow-list, not a deny-list) — a newly added human-facing route must be
deliberately classified before it starts appearing in the audit log. A request that authorization
refused is recorded even when its route is not in the table.

## Storage

Audit entries are stored in the `audit_logs` table (migration `004_audit_logs`):

```sql
CREATE TABLE audit_logs (
    id bigserial PRIMARY KEY,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    actor text NOT NULL,
    method text NOT NULL,
    path text NOT NULL,
    action text NOT NULL,
    resource text NOT NULL DEFAULT '',
    status integer NOT NULL
);
CREATE INDEX idx_audit_logs_occurred_at ON audit_logs (occurred_at);
```

Migration `024_matrix_authorization` adds the nullable columns `permission`, `scope` and `granted_by`.
They are `NULL` for rows written before the upgrade.

## API

```
GET /api/v1/audit?limit=N&offset=M
```

- Requires the `audit.read` permission, which only the `admin` bundle holds (see
  [Authorization](authorization.md)). Other callers get `403 forbidden: missing permission "audit.read"`.
- `limit` — default `100`, capped at `1000`.
- `offset` — default `0`.
- Results are ordered **newest first** (`occurred_at DESC`).

Example:

```bash
curl -H "Authorization: Bearer $TOKEN" \
  "https://controller.example.com/api/v1/audit?limit=20"
```

```json
[
  {
    "id": 42,
    "occurredAt": "2026-07-05T10:30:00Z",
    "actor": "alice",
    "method": "DELETE",
    "path": "/api/v1/secrets/AWS_KEY",
    "action": "secret.delete",
    "resource": "AWS_KEY",
    "status": 204,
    "permission": "secret.write",
    "grantedBy": "role:admin (roleMap[\"unified-admins\"])"
  }
]
```

## CLI

```
unified-cli audit list [--limit N]
```

Prints a table (time, actor, action, resource, status, permission, grantedBy), newest first.
A column with no value shows `-`:

```
$ unified-cli audit list --limit 5
2026-07-05T10:30:00Z	alice	secret.delete	AWS_KEY	204	secret.write	role:admin (roleMap["unified-admins"])
2026-07-05T10:28:11Z	bob	run.cancel	run-abc123	403	run.cancel	-
2026-07-05T10:15:02Z	alice	token.create	ci-bot	200	token.create	role:admin (roleMap["unified-admins"])
...
```

Requires the `audit.read` permission, same as the API endpoint.

## Retention

Old audit rows are periodically deleted by a leader-only background task (advisory-lock
election, same pattern as the approval reaper / stuck-run reaper), controlled by:

| Flag | Env var | Default | Notes |
|---|---|---|---|
| `--audit-retention-days` | `UNIFIED_AUDIT_RETENTION_DAYS` | `90` | `0` disables cleanup — audit rows are kept forever |

The cleanup task runs hourly and deletes rows with `occurred_at` older than the configured
retention window. In a multi-replica (HA) deployment, only the elected leader performs the
deletion; all replicas can serve `GET /api/v1/audit` regardless of leadership.
