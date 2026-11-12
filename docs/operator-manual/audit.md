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

The agent secrets fetch also writes one row per GitHub App job token it hands out or refuses, and
one per refused request for an App's private key (`gitcredential.token_issue`, see [GitHub App job tokens](#github-app-job-tokens)); it is the only
agent route besides credential exchange and refresh that is audited.

Each recorded entry contains:

| Field | Description |
|---|---|
| `occurredAt` | Timestamp of the request |
| `actor` | Display name of the authenticated caller: PAT name, OIDC email/subject, or session identity — whatever `ServerAuth` resolved as the `Principal` for the request (the same identity source used for `run_approvals.decided_by`). **Not unique and not trustworthy on its own**: a PAT name is chosen by whoever creates the token, so two PATs can share a name, or imitate another user's PAT or the bootstrap token. Use `actorId` to tell callers apart |
| `actorId` | Stable identity of the caller; see [Actor identity](#actor-identity). Empty for rows written before the field existed |
| `actorSub` | OIDC subject (`sub`) of the user behind the caller, when there is one (owner PATs, OIDC bearer tokens, sessions); empty for role-only PATs, the bootstrap token and agents |
| `method` | HTTP method (`POST`/`PUT`/`DELETE`) |
| `path` | Request path |
| `action` | Short classification from the table above |
| `resource` | Best-effort name/id of the target (e.g. job name, secret name, token id). For `agent.enrollment.create` it is `<token id> agent=<agent id>`, followed by ` labels=` and the comma-joined labels when any were requested, so a create can be matched to the `agent.enrollment.revoke` entry for the same token ID |
| `status` | HTTP response status code the client actually saw |
| `permission` | The permission the request was checked against (for example `run.trigger`); empty when no decision applied |
| `scope` | The job name for a folder-scopable decision; empty for a global decision and for a refusal before any job was looked at |
| `grantedBy` | A short description of the first grant that allowed the request; empty on a denial |
| `details` | Optional structured context (a JSON object). Only `gitcredential.token_issue` rows carry it today, see [GitHub App job tokens](#github-app-job-tokens); absent on every other row |

### Actor identity

`actor` is a label; `actorId` is the identity. It is derived from the authenticated credential, never
from anything the caller can choose:

| `actorId` | Caller |
|---|---|
| `break-glass` | The `UNIFIED_TOKEN` bootstrap credential. A PAT that merely carries the bootstrap token's name gets its own `pat:<id>` instead |
| `pat:<pat id>` | Any other PAT (role-only, owner or enrollment). `<pat id>` is the PAT's primary key; unlike the name it is unique |
| `oidc:<sub>` | An OIDC bearer token or a browser session; `<sub>` is the subject claim (also in `actorSub`) |
| `agent:<agent id>` | An agent credential exchange or refresh (`agent.enrollment.exchange`, `agent.refresh`) or a GitHub App job token hand-out (`gitcredential.token_issue`); other agent rows carry no `actorId` |

Two PATs with the same name therefore produce rows with the same `actor` and different `actorId`
values. `pats.name` has no `UNIQUE` constraint (existing databases may already hold duplicates), so
`actorId` is the field to filter on when investigating who did something.

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

### GitHub App job tokens

When a run's agent fetches secrets and a requested name is a `githubApp` GitCredential with
`jobAccess`, the controller answers with an installation token instead of a stored secret and
writes an audit row directly (agent routes are outside the audit middleware):

| Field | Value |
|---|---|
| `action` | `gitcredential.token_issue` |
| `actor` / `actorId` | `agent` / `agent:<agent id>` |
| `method` / `path` | `POST` / `/api/v1/agents/<agent id>/secrets/fetch` |
| `resource` | The GitCredential name (for reason `private_key`, the `githubApp` credential whose private key was requested) |
| `status` | `200` the token was handed out; `403` the run's job is not covered by `jobAccess.folders`, or the fetch asked for the App's private key; `502` GitHub refused or failed to issue the token; `500` the controller could not issue (token issuing not configured, or the run could not be loaded) |
| `details` | See below |

`details` keys:

| Key | Present | Value |
|---|---|---|
| `runId` | always | The run the agent fetched for |
| `jobName` | except for reasons `not_configured`, `run` and `private_key` | The run's qualified job name, as checked against `jobAccess.folders` |
| `repositories` | except for reason `private_key` | The credential's `jobAccess.repositories` patterns (as configured, not expanded) |
| `permissions` | except for reason `private_key` | The credential's `jobAccess.permissions` map |
| `expiresAt` | `200` only | The token's expiry (RFC 3339, UTC) |
| `secretRef` | reason `private_key` only | The requested secret name (the App's `secretRef`); never its value |
| `reason` | non-`200` only | `folder` (403), `private_key` (403), `issue` (502), `not_configured` or `run` (500) |

A `200` row is written only when the fetch response actually carries the token: if another name in
the same fetch fails, the request is answered with that error and no `200` row is written for the
token. The token, the App's private key and the App JWT are never recorded. The jobAccess shown is
the credential as it was at fetch time, which is also what was checked. A request for the App's
private key (the credential's `secretRef`) is refused with `403` before any token is issued and
writes one row with reason `private_key`; the key is never read for it.

### Failed authentication

A request that is rejected with `401` (no credentials, unknown or expired token, failed session
refresh, failed OIDC `id_token` verification) is logged and audited:

- The controller logs a structured warning, `authentication failed`, with `remote` (the peer
  address, not `X-Forwarded-For`), `method` (`bearer`, `session`, `none`, `agent-access`,
  `oidc-callback`), `reason` and `path`. The presented token, or any part of it, is never logged.
- An audit row with `action` `auth.failed`, `status` `401` and `actor` `unknown` is written, except
  for `agent-access` failures (an invalid `uca_` agent credential) and `none` (no credentials at all),
  which are only logged. `reason` is a short fixed code such as `invalid_token`, `lookup_failed`
  (the credential store was unreachable) or `invalid_or_expired_session`. Its `resource` is
  `method=<method> reason=<reason> remote=<address>`.
- Reporting is rate limited per controller process and per `method` (about 5 per second after a burst of 20), so a flood of one method does not hide failures of another,
  so a client cannot grow the log or `audit_logs` at request rate. Dropped reports are counted and the
  next report that gets through carries `suppressed=<n>` in the log line. Treat a `suppressed` value
  as evidence of a flood; the audit rows only sample it.

## What is excluded

The following are deliberately **not** recorded:

- **Agent-facing endpoints** (`/api/v1/agents/register`, `/heartbeat`, `/claim`, `/steps`,
  `/logs`, `/finish`, `/outputs`, `/secrets/fetch`, agent-created approvals, etc.) — these use
  a per-agent principal, not a human `Principal`, and would otherwise flood
  the audit log with routine bookkeeping. The exceptions are agent credential exchange and
  refresh, and GitHub App job token hand-outs from `/secrets/fetch`
  ([GitHub App job tokens](#github-app-job-tokens)); an ordinary stored-secret fetch is not recorded.
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
  human-only route and `403 forbidden: unclassified route` are not recorded. Failed
  authentication (`401`) is recorded separately, see [Failed authentication](#failed-authentication).
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

Migration `027_audit_actor_identity` adds the nullable columns `actor_id` and `actor_sub`. They are
`NULL` for rows written before the upgrade, which therefore cannot be attributed beyond the
(non-unique) `actor` name. The change is additive: no existing API field or column changed.

Migration `029_github_app_credentials` adds the nullable `details jsonb` column, returned as the
`details` field of the API. It is `NULL` (and the field absent) for every row that has no details.

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
    "actorId": "oidc:5f2c1a0e-0000-4000-8000-000000000001",
    "actorSub": "5f2c1a0e-0000-4000-8000-000000000001",
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

Prints a table (time, actor, action, resource, status, permission, grantedBy, actorId), newest first.
A column with no value shows `-`:

```
$ unified-cli audit list --limit 5
2026-07-05T10:30:00Z	alice	secret.delete	AWS_KEY	204	secret.write	role:admin (roleMap["unified-admins"])	oidc:5f2c1a0e-0000-4000-8000-000000000001
2026-07-05T10:28:11Z	bob	run.cancel	run-abc123	403	run.cancel	-	pat:3b1e7c52-0000-4000-8000-0000000000aa
2026-07-05T10:15:02Z	alice	token.create	ci-bot	200	token.create	role:admin (roleMap["unified-admins"])	oidc:5f2c1a0e-0000-4000-8000-000000000001
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
