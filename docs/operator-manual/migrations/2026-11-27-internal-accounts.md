# Migrating: internal accounts, the initial admin token and recovery

The controller no longer has an always-admin `UNIFIED_TOKEN`. Every credential
now belongs to an **account**: a *human* account (created the first time a
person signs in with SSO) or a *service* account (declared in the controller
config, for CI and other automation). A non-SSO install gets in once with a
one-time **initial admin token**, and a lost admin credential is replaced with
`<controller binary> admin recover`.

**You are affected if** you run any controller that used `UNIFIED_TOKEN` (every
non-SSO install, and SSO installs that kept it as a fallback), if your
automation uses a PAT created before owners existed or with `UNIFIED_TOKEN`, or
if you set `UNIFIED_ALLOW_DEFAULT_TOKEN`.

This is a breaking upgrade with no compatibility period. Allow a short outage:
all controller replicas are stopped before the upgrade (step 3).

## 1. What breaks

| Before | After |
|---|---|
| The controller authenticates `UNIFIED_TOKEN` (or `--token`, or `token:` in the config file) as an admin PAT named `env:UNIFIED_TOKEN`. | The controller ignores it and logs `UNIFIED_TOKEN is no longer used by the controller and is ignored; see docs/operator-manual/migrations/2026-11-27-internal-accounts.md`. It never creates a PAT row from it. |
| Without SSO the controller refuses to start when `UNIFIED_TOKEN` is unset. | The controller starts when SSO is configured, when the bootstrap is done, or when it can generate (or was given) the initial admin token. |
| `UNIFIED_ALLOW_DEFAULT_TOKEN`, `--allow-default-token` and the refusal of the published default token. | Gone. The variable and flag are accepted and ignored; there is no default token to refuse. |
| PATs without an owner exist: role-only PATs, enrollment-only PATs without an owner, and anything created with `UNIFIED_TOKEN`. | **Every ownerless PAT is deleted by the migration.** Automation that used one gets `401` until you re-issue its token. |
| PATs and sessions store the owner's `sub`, email and groups inline. | They point at an account. Humans get an account from their existing PAT owners and sessions. |
| Sessions from before the matrix-authorization upgrade (no stored claims). | Deleted; those users sign in again. |
| A new PAT could be created without an expiry. | Every new PAT expires (default 90 days, maximum 365 days; see step 8). PATs that survive the migration keep their current expiry. |
| `token list` printed ID, name, ceiling and creation date; `break-glass` appeared as an audit actor. | `token list` is a table with the columns `ID NAME ACCOUNT CEILING EXPIRES CREATED`. New audit rows never use `break-glass`; old rows keep it. |
| An owner PAT whose owner lost every permission answered `forbidden: token owner has no permissions`. | Accounts add two refusals: `forbidden: account disabled` and `forbidden: no role` (see [Troubleshooting](../../troubleshooting/controller-and-database.md#accounts-and-the-initial-admin-token)). |

Owned PATs (created by SSO users) keep working. A PAT is still bounded by its
owner's current permissions, and a person's account now also lets an admin
disable them (`unified-cli account disable`), which stops all of their PATs and
sessions at once.

## 2. Find the deleted PATs

The migration writes one audit row per deleted PAT: action `token.delete`,
actor and `actorId` `migration`, the PAT name as the resource, and the PAT's id,
role and `createdBy` in `details`. Use them to decide what to re-issue.

```bash
unified-cli audit list --limit 1000 | grep migration
```

For the details (`patId`, `role`, `createdBy`), query the database:

```sql
SELECT occurred_at, resource AS name, details->>'role' AS role,
       details->>'createdBy' AS created_by, details->>'patId' AS pat_id
FROM audit_logs
WHERE actor_id = 'migration' AND action = 'token.delete'
ORDER BY occurred_at;
```

`audit list` needs the `audit.read` permission, so run it after step 4 (or with
an SSO admin), and shows at most the newest 1000 rows (use the SQL if the
upgrade is further back). The rows are written when the migration runs, so the
list is complete even if you look later, but audit rows are deleted after
`--audit-retention-days` (default 90): save the list now.

## 3. Upgrade: stop every replica first

Old and new controller replicas must never run against the same database: the
migration drops the old owner columns that old replicas read. Stop **all**
replicas, upgrade, then start.

- **Binary / systemd:** stop every controller process, install the new binary, start them.
- **Docker Compose:** `docker compose down` (without `-v`), pull or rebuild the new image, then set `UNIFIED_INITIAL_ADMIN_TOKEN` in `.env` (see step 4) and `docker compose up -d`.
- **Kubernetes (Helm):** scale the controller to zero and wait until its Pods have exited, then upgrade:

  ```bash
  kubectl -n unified-cd scale deployment/unified-cd-controller --replicas=0
  kubectl -n unified-cd wait --for=delete pod -l app=unified-cd-controller --timeout=120s
  helm upgrade unified-cd oci://ghcr.io/eirueimi/charts/unified-cd --version <new-version> -n unified-cd <your values>
  kubectl -n unified-cd get deployment unified-cd-controller   # replicas must be back at controller.replicas
  ```

  If Helm leaves the Deployment at zero replicas, scale it back to `controller.replicas`.

- **Kubernetes (release bundle, `kubectl apply`):** scale to zero, wait, apply the new bundle, then check the replicas:

  ```bash
  kubectl -n unified-cd scale deployment/unified-cd-controller --replicas=0
  kubectl -n unified-cd wait --for=delete pod -l app=unified-cd-controller --timeout=120s   # "no matching resources found" is fine: no Pods are left
  kubectl apply -f https://github.com/eirueimi/unified-cd/releases/download/<new-version>/install.yaml
  kubectl -n unified-cd get deployment unified-cd-controller
  ```

  `kubectl apply` merges against the last applied manifest, which declared the same replica count as the new one, so the live count of 0 can survive. If the Deployment stays at zero, run `kubectl -n unified-cd scale deployment/unified-cd-controller --replicas=<n>`. The same applies to `core-install.yaml`.

Agents are not affected and reconnect on their own.

## 4. Re-bootstrap a non-SSO install

If SSO is **not** configured, the first start after the upgrade behaves like a
fresh install: the controller makes (or accepts) a one-time initial admin token.
Exchange it once for an admin PAT with `unified-cli bootstrap`. The PAT is a PAT
of the built-in `admin` account and lasts 30 days. The initial token works only
once and is useless afterwards.

### Binary

The controller writes the token to `initial-admin-token` (mode `0600`) in the
config file's directory when you pass `-f`, otherwise in the data directory
(`--data-dir`), and logs the path, never the value. If neither directory is
writable the controller refuses to start (`cannot write the initial admin token:
no writable config or data directory; set UNIFIED_INITIAL_ADMIN_TOKEN`).

```bash
export UNIFIED_SERVER=https://cd.example.com
unified-cli bootstrap --initial-token-file /etc/unified-cd/initial-admin-token
# prints the admin PAT on stdout; keep it
```

To choose the value yourself, set `UNIFIED_INITIAL_ADMIN_TOKEN` on the
controller before its first start and pass the same value to `unified-cli
bootstrap` (`--initial-token` or the same environment variable). A provided value
needs at least 24 characters, and the fixed value from the repo-root
`docker-compose.yaml` is refused unless `UNIFIED_DEV_MODE` is set. The file is
deleted after a successful exchange.

### Docker Compose

Set `UNIFIED_INITIAL_ADMIN_TOKEN` in `.env` (`echo "uci_$(openssl rand -hex 20)"`)
and remove `UNIFIED_TOKEN` from it. The production compose refuses to start
without it.

- **New install:** the `agent-enroll` one-shot exchanges the token, stores the
  admin PAT in the `admin-state` volume and uses it to enroll the agent. Read
  the PAT with
  `docker compose -f deployments/docker/docker-compose.yaml run --rm --no-deps --entrypoint cat agent-enroll /state/admin-token`.
- **Upgraded install:** the agent already has credentials, so `agent-enroll`
  skips and **never bootstraps**. Run the exchange yourself, once, with the value
  from `.env`:

  ```bash
  export UNIFIED_SERVER=http://localhost:8080
  export UNIFIED_INITIAL_ADMIN_TOKEN=<the value in .env>
  unified-cli bootstrap
  ```

### Kubernetes (Helm chart)

On upgrade, the chart's create-once Job (`unified-cd-initial-token-init`)
generates the Secret `unified-cd-initial-admin-token` (key
`UNIFIED_INITIAL_ADMIN_TOKEN`) if it does not exist, and the controller Pod
waits for it. Exchange it:

```bash
kubectl -n unified-cd port-forward svc/unified-cd-controller 8080:80 &
export UNIFIED_SERVER=http://localhost:8080
export UNIFIED_INITIAL_ADMIN_TOKEN="$(kubectl -n unified-cd get secret unified-cd-initial-admin-token -o jsonpath='{.data.UNIFIED_INITIAL_ADMIN_TOKEN}' | base64 -d)"
unified-cli bootstrap
```

To supply the value yourself, set `controller.initialAdminToken.generate=false`
and create the Secret (same name and key) before the upgrade. An SSO-only
install can also set `generate=false` and skip the Secret.

### If the exchange fails

- `invalid or already used initial token` (401): the value is wrong, or the token was already exchanged.
- `too many bootstrap attempts` (429): the endpoint allows 5 attempts per minute per client address; wait and retry.
- `the built-in admin account is disabled; enable it or use the recovery command` (409): `serviceAccounts.admin.enabled` is `false` and no recovery lease is running. The initial token is not consumed; enable `admin` (restart) or use `<controller binary> admin recover --enable-admin`.
- If the exchange consumed the token but the PAT could not be created (the controller logs `bootstrap: create PAT (the initial token is already consumed; use '<controller binary> admin recover')`), or you lost the admin PAT, use [`admin recover`](#7-admin-recover).

## 5. Declare service accounts for CI

Automation gets a **service account**: a named, non-human account declared in
the controller config. It works the same with or without SSO, and its
permissions come only from the config.

```yaml
serviceAccounts:
  ci-deploy:
    permissions:
      global: [viewer, token.create]   # token.create lets CI rotate its own PAT
      folders:
        team-a: developer
        team-a/prod: [job.read, run.trigger]
```

Names are DNS labels and must not start with `env:` or `system:`. `permissions`
accepts the same grants as one `users:` entry of `oidc.permissions`. A name
removed from the file is disabled at the next controller start; adding it back
re-enables it. Restart the controller after editing the file. On Kubernetes set
`controller.serviceAccounts` in the chart values.

Then issue its PAT as a caller who holds `token.manage` or
`serviceaccount.token` (the admin bundle has both). The PAT is bounded by the
service account's permissions, and by `--role` / `--permission` if you pass them:

```bash
unified-cli token create ci-deploy-token --account ci-deploy --expires-in 2160h
```

A service account's PAT can create its own successor (a PAT may mint PATs for
its own account, within its own ceiling), which is how CI rotates its token
without an admin. The service account needs `token.create` for that, as in the
example above. A caller who holds `serviceaccount.token` can also list and
delete the PATs of service accounts (not `admin`, not humans), and can mint PATs
carrying any non-admin service account's grants, so that permission is
effectively the union of all service-account grants. PATs for the built-in `admin` account cannot be minted
through the API (`PATs for the built-in admin account can only be obtained
through the initial token or the recovery command`).

Re-issue every deleted PAT from step 2 this way, and re-issue surviving owned
PATs too if you want them to carry the new lifetime limits.

## 6. Disable `admin` under SSO

With SSO configured no initial token is generated: the first admin comes from
`oidc.userMap` / `oidc.roleMap`. A value you provide with
`UNIFIED_INITIAL_ADMIN_TOKEN` is still honoured under SSO (Compose and Helm
set one), so under SSO either set `controller.initialAdminToken.generate=false`
(Helm) or do not provide one, or exchange the token once and then set
`serviceAccounts.admin.enabled: false`. The built-in `admin` account still exists, so
the controller logs `the built-in admin service account is enabled while SSO is
configured; set serviceAccounts.admin.enabled: false once SSO works`. Once an
SSO admin can sign in, turn it off:

```yaml
serviceAccounts:
  admin:
    enabled: false
```

While `admin` is disabled its PATs are refused with `forbidden: account
disabled`. `admin recover --enable-admin` brings it back temporarily (step 7).

## 7. `admin recover`

`<controller binary> admin recover` prints a short-lived PAT of the built-in
`admin` account. Its only gate is access to the database with the controller's
configuration. It never starts the HTTP server and writes an audit row with
actor `recovery`. Run it with the same configuration as the server (the same
`-f` file and the same environment), or it may reach another database or take a
different `admin` decision.

The controller binary is called `controller` in the release archives and in the
container image (`/controller`), and `bin/unified-cd-controller` after `make
build`. This guide writes `<controller binary>` for whichever applies.

```bash
<controller binary> admin recover [-f config] [--dsn dsn] [--ttl 1h] [--enable-admin]
```

- `--ttl` defaults to `1h`; the maximum is `24h` (`--ttl must not exceed 24h`).
- If the config disables `admin`, the command refuses (`the built-in admin account is disabled in the configuration; pass --enable-admin`). `--enable-admin` enables `admin` until the PAT expires and replaces any earlier lease; the config stays the long-term authority.
- The PAT goes to stdout, everything else to stderr.

Run it where the controller runs:

```bash
# release archive (use ./bin/unified-cd-controller after make build)
./controller admin recover -f /etc/unified-cd/controller.yaml
# Docker Compose
docker compose -f deployments/docker/docker-compose.yaml exec controller /controller admin recover
# Kubernetes
kubectl -n unified-cd exec deploy/unified-cd-controller -- /controller admin recover -f /etc/unified-cd/controller-config.yaml
```

Use the PAT to sign in with the CLI, create what you need (a service account
PAT, or fix the SSO mapping), and let it expire.

## 8. PAT lifetimes

Every new PAT expires. Defaults and limits are controller config
(see [Configuration](../../reference/configuration.md#tokens-pat-lifetimes)):

```yaml
tokens:
  defaultLifetime: 2160h   # 90 days, used when --expires-in is omitted
  maxLifetime: 8760h       # 365 days
```

- `unified-cli token create --expires-in 720h` asks for a lifetime. Above `tokens.maxLifetime` it is refused: `expiresIn exceeds tokens.maxLifetime (<max>)`.
- PATs of the built-in `admin` account are capped at 720h (30 days) whatever `tokens.maxLifetime` says: `expiresIn exceeds the 720h limit for the built-in admin account`.
- **Opt out of the cap** with `tokens.maxLifetime: 0` (or `UNIFIED_TOKENS_MAX_LIFETIME=0`). New PATs then have no upper bound on `--expires-in`, and you accept PATs that live as long as you ask. `defaultLifetime` must still be positive.
- `unified-cli token list` shows the expiry. The controller logs a daily warning `personal access tokens expire within 7 days` and exports `unifiedcd_pats_expiring_soon`; alert on it so CI tokens are rotated in time.

## 9. The web UI token form under SSO

When browser SSO is configured (OIDC with a client secret), the web UI no longer
offers "enter token manually", and the controller refuses a PAT on requests that
carry browser fetch metadata (`Sec-Fetch-Site`): `personal access tokens are not
accepted from the browser while SSO is configured; sign in with SSO` (401). The
CLI and API clients are unaffected, and so are session cookies and OIDC
id_tokens. Node.js `fetch` sends `Sec-Fetch-Mode` but not `Sec-Fetch-Site`, so it
is not treated as a browser. Browsers send no `Sec-Fetch-*` headers to
plain-http, non-localhost origins, so the policy does not apply there. Without
browser SSO (no OIDC, or OIDC without a client secret) the form stays.

This is a policy for the web UI, not a security boundary: someone holding a
service-account PAT can still call the API with `curl`. Expiry, `last_used_at`
and the audit log are the controls for that.

To turn the form back on while SSO is configured (for example during an IdP
outage), set `ui.allowTokenEntry: true` (`UNIFIED_UI_ALLOW_TOKEN_ENTRY=1`;
chart value `controller.ui.allowTokenEntry`).

## Behaviour changes to know about

- **Disabling a person.** `unified-cli account disable <id>` (permission `account.manage`) refuses all of their PATs and sessions immediately, on every replica. Their CLI id_token is refused immediately on the replica that handled the disable and within 1 minute on the others. A disabled person who tries to sign in gets `forbidden: account disabled`. Service accounts are not toggled this way: remove them from `serviceAccounts` (`service accounts are enabled and disabled in serviceAccounts`).
- **Changing the IdP.** Accounts are matched by `sub`. Switching to an IdP with different subjects creates new human accounts on first sign-in; the old ones keep their PATs until an admin disables them.
- **Who gets an account.** A person gets a human account only after the normal admission check succeeds; someone refused with `forbidden: no role` leaves no row.
- **Permissions.** The catalogue now has 30 permissions (it had 28): `account.manage` and `serviceaccount.token` are new and belong to the admin bundle.

## Troubleshooting

| Symptom | Fix |
|---|---|
| `UNIFIED_TOKEN is no longer used by the controller and is ignored` in the log | Remove `UNIFIED_TOKEN`, `--token` and the `token:` key from the controller's environment and config; use a service account PAT in clients. |
| `401 unauthorized` from CI after the upgrade | The PAT was ownerless and deleted (step 2). Issue a service-account PAT (step 5). |
| Controller does not start: `cannot write the initial admin token: no writable config or data directory; set UNIFIED_INITIAL_ADMIN_TOKEN` | Pass `-f` with a writable directory, set `--data-dir`, or set `UNIFIED_INITIAL_ADMIN_TOKEN`. |
| `invalid or already used initial token` | Already exchanged or wrong value. Use `admin recover` (step 7). |
| Compose: `agent-enroll` skipped and no admin PAT | Upgraded installs skip it. Run `unified-cli bootstrap` by hand (step 4). |

The full list of messages is in [Troubleshooting](../../troubleshooting/controller-and-database.md#accounts-and-the-initial-admin-token).
