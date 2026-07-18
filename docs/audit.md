# Audit Log

unified-cd records a persistent audit trail of state-changing API operations: who did what, on
which resource, when, and with what result. This complements the existing per-run approval
audit trail (`run_approvals` — see [Job Reference](jobs.md) for the `approval` step type), which
only covered approve/reject decisions.

> The CLI is referred to as `unified-cd` throughout. This is the built binary (`./bin/unified-cd`);
> source is under `cmd/unified-cli`. During development you can also use `go run ./cmd/unified-cli ...`.

## Table of Contents

- [What is recorded](#what-is-recorded)
- [What is excluded](#what-is-excluded)
- [Storage](#storage)
- [API](#api)
- [CLI](#cli)
- [Retention](#retention)

## What is recorded

Every **state-changing** (`POST`, `PUT`, `DELETE`) call to the controller's human-facing API is
recorded, for the following actions:

| Action | Route |
|---|---|
| `job.apply` | `POST /api/v1/jobs` |
| `job.delete` | `DELETE /api/v1/jobs/{name}` |
| `run.trigger` | `POST /api/v1/runs` |
| `run.cancel` | `POST /api/v1/runs/{id}/cancel` |
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
| `status` | HTTP response status code |

**Secrets are special-cased:** for `secret.set`, only the secret's `name` is ever read out of the
request body — the `value` field is never inspected, logged, or stored anywhere in the audit
trail. `secret.delete` uses the URL path parameter, so no body is involved at all.

## What is excluded

The following are deliberately **not** recorded:

- **Agent-facing endpoints** (`/api/v1/agents/register`, `/heartbeat`, `/claim`, `/steps`,
  `/logs`, `/finish`, `/outputs`, `/secrets/fetch`, agent-created approvals, etc.) — these use
  a per-agent principal (or temporary legacy compatibility token), not a human
  `Principal`, and would otherwise flood the audit log with routine bookkeeping.
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
- **All `GET` requests** — read-only calls are never audited, regardless of route.
- **Artifact upload/download** — agent-authenticated (upload) or shared with agents (download);
  excluded along with the rest of the agent-facing surface.

Only routes explicitly present in the middleware's action table are recorded (an allow-list, not
a deny-list) — a newly added human-facing route must be deliberately classified before it starts
appearing in the audit log.

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

## API

```
GET /api/v1/audit?limit=N&offset=M
```

- **Admin role only** — same RBAC enforcement (`requireMinRole`) as other admin-only endpoints
  (see [Authorization](authorization.md)). Non-admin callers get `403 Forbidden`.
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
    "status": 204
  }
]
```

## CLI

```
unified-cli audit list [--limit N]
```

Prints a table (time, actor, action, resource, status), newest first:

```
$ unified-cli audit list --limit 5
2026-07-05T10:30:00Z	alice	secret.delete	AWS_KEY	204
2026-07-05T10:28:11Z	bob	job.apply	deploy-prod	200
2026-07-05T10:15:02Z	alice	token.create	ci-bot	200
...
```

Requires an admin-role token/session, same as the API endpoint.

## Retention

Old audit rows are periodically deleted by a leader-only background task (advisory-lock
election, same pattern as the approval reaper / stuck-run reaper), controlled by:

| Flag | Env var | Default | Notes |
|---|---|---|---|
| `--audit-retention-days` | `UNIFIED_AUDIT_RETENTION_DAYS` | `90` | `0` disables cleanup — audit rows are kept forever |

The cleanup task runs hourly and deletes rows with `occurred_at` older than the configured
retention window. In a multi-replica (HA) deployment, only the elected leader performs the
deletion; all replicas can serve `GET /api/v1/audit` regardless of leadership.
