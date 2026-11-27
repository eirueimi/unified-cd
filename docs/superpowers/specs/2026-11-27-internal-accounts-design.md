# Internal Accounts and the End of the Always-Admin Token — Design

Status: draft for review.

## 1. Intent

`UNIFIED_TOKEN` is a static credential that holds every permission, bypasses
every folder grant, never expires, belongs to nobody, and is re-registered on
every controller start. Without SSO it is the only way in, so the controller
refuses to start without it. PATs created with it are ownerless admin tokens
that survive its rotation.

The owner wants that kind of token gone, in both SSO and non-SSO setups. No
comparable product keeps an always-on admin token as normal practice: Jenkins,
GitLab and Argo CD use a one-time initial secret; Argo Workflows and Tekton
delegate to Kubernetes; emergency access is "someone who can touch the server
or the database runs a command". unified-cd should follow that model.

The deeper gap, compared with Jenkins or GitLab, is that unified-cd has no
account records. A PAT stores its owner's OIDC `sub`, email and group snapshot
inline, or has no owner at all. This design introduces internal accounts so
that every credential belongs to an account, and SSO becomes one way of
signing in to an account.

## 2. Goals

- No credential that is always admin, never expires and bypasses
  authorization.
- Every PAT and every session belongs to an account. There are no ownerless
  PATs after migration.
- Two kinds of account: **human** (created on first SSO sign-in) and
  **service** (declared in the config file). Service accounts work the same
  with or without SSO.
- Non-SSO setups keep working: the first admin gets in with a one-time initial
  token; daily use is service-account PATs.
- Emergency access without the IdP and without a standing token: a controller
  command that needs database access.
- Disabling an account stops all of its PATs and sessions at once.
- Works the same for the binary, Docker Compose and Kubernetes installs.

## 3. Non-goals

- Password sign-in for humans. Humans without SSO use a service account's PAT
  (one service account per person if they want separation).
- Managing permissions in the database or from the UI. Permissions stay in the
  config file, read at startup (owner's decision).
- SCIM or other IdP push provisioning. The account table is shaped so it can
  be added later.
- Changing agent authentication (`uce_`/`uca_`/`ucr_`). Agents are not
  accounts.
- Changing the CLI's client-side `UNIFIED_TOKEN` variable. It keeps meaning
  "the PAT this CLI sends".

## 4. Anti-goals

- A replacement that is just as strong under another name, for example an
  initial token that stays valid, or a built-in admin whose PATs never expire
  and that cannot be turned off.
- Upgrades that lock operators out: an upgraded deployment must always have a
  documented way back in (SSO, the initial token of 7.3, or the recovery
  command). Automation that used ownerless PATs is expected to break and be
  re-issued (owner's decision, section 7).
- A service account becoming a way for humans to skip SSO in the web UI.

## 5. Approaches considered

| | Approach | Trade-off | Outcome |
|---|---|---|---|
| A | Service accounts + one-time initial token + offline recovery command | Removes the standing token, gives CI a non-human identity; existing ownerless PATs must be migrated | **Chosen**, extended with internal accounts |
| B | Local users with passwords (Jenkins style) | Per-person sign-in without SSO; adds password storage, lockout, reset and a login form | Rejected: too much for setups that do not run SSO |
| C | Keep `UNIFIED_TOKEN` but valid only until the first admin PAT exists, plus a recovery command | Cheap; ownerless admin PATs remain, so the gap with other products stays | Rejected |

On top of A the owner chose to keep accounts internally (an account table),
because it makes SSO and non-SSO consistent, and chose to keep permissions in
the config file (option 1) rather than in the database.

## 6. Decisions

### 6.1 Accounts

A new `accounts` table is the registry of who exists. It holds identity and
state, not permissions.

| Column | Meaning |
|---|---|
| `id` | uuid |
| `kind` | `human` or `service` |
| `name` | service: the config name (unique among service accounts); human: display email or `sub` |
| `oidc_sub` | human only; unique. unified-cd supports exactly one issuer, so `sub` alone identifies a person |
| `oidc_issuer` | human only; informational, the issuer configured when the account was created or migrated (empty when SSO was not configured) |
| `email`, `claims` | human only; last known email and group-claim snapshot (moved here from `pats` and `sessions`) |
| `disabled_at` | non-null means disabled |
| `created_at`, `last_seen_at` | |

- A **human account** is created the first time a person signs in through SSO
  (browser login, CLI device flow, or a bearer id_token). Every browser login
  refreshes `email` and `claims` (today this refreshes the snapshot on every
  PAT of that `sub`; it now refreshes the account once).
- A **service account** row is created at startup for each entry in the
  config's `serviceAccounts` block. An entry removed from the config is marked
  disabled at the next start (the row is kept for audit). Re-adding it
  re-enables it.
- An admin can disable or re-enable a human account with a new permission
  `account.manage` (admin bundle). Disabled means: PATs rejected, sessions
  rejected, new SSO sign-in refused with a clear message.

### 6.2 Permissions

Permissions keep coming from the config file only.

- Humans: unchanged. `oidc.roleMap`, `oidc.userMap`, `oidc.defaultRole` and
  `oidc.permissions` are evaluated against the account's `email`, `sub` and
  `claims`, exactly as they are today against the PAT owner snapshot.
- Service accounts: a new top-level block, independent of `oidc:` (because
  `oidc.permissions` is rejected without `oidc.issuer`, and service accounts
  must work without SSO):

```yaml
serviceAccounts:
  admin:                 # built-in, see 6.3; only `enabled` may be set
    enabled: true
  ci-deploy:
    permissions:
      global: [viewer]
      folders:
        team-a: developer
        team-a/prod: [job.read, run.trigger]
```

  The grant syntax and validation are the same as one `users:` entry of
  `oidc.permissions` (role names, permission names, lists; global-only
  permissions rejected on folders). Service-account names follow DNS-label
  rules and must not start with `env:` or `system:`.
- A PAT's permissions are its account's permissions intersected with the
  PAT's optional ceiling (`--role` / `--permission`), as owner PATs work
  today. No PAT exceeds its account.

### 6.3 The built-in `admin` service account

- Always present in the registry, holds the admin bundle globally, and cannot
  be given other permissions.
- `serviceAccounts.admin.enabled: false` disables it (Argo CD's
  `admin.enabled`). The startup warning that today recommends unsetting
  `UNIFIED_TOKEN` once SSO works now recommends disabling `admin`.
- Its PATs can only be obtained through the initial token (6.4) or the
  recovery command (6.6). Nobody can mint an `admin` PAT through the normal
  token API, so the strongest identity is never handed out casually.
- Its PATs always expire (6.5).

### 6.4 The initial token

- Generated once, at the first start where the bootstrap has not yet been
  done (a flag in `controller_settings`) and SSO is not configured. With SSO
  configured the first admin comes from `oidc.userMap` / `roleMap`, so no
  initial token is generated; the operator can still use the recovery command.
- Written only to a file, mode 0600, at `<config dir>/initial-admin-token` when
  a config file is used, else `<data dir>/initial-admin-token`; the log states
  the path, never the value (logs are often shipped elsewhere).
- Instead of generating, the operator can provide the value with
  `UNIFIED_INITIAL_ADMIN_TOKEN` (for automated installs, like GitLab's
  `GITLAB_ROOT_PASSWORD`). The same one-time rules apply.
- Exchanged once with `unified-cli bootstrap --initial-token <value>`
  (`POST /api/v1/bootstrap`). The response is a PAT of the built-in `admin`
  account. After the exchange the flag is set, the stored hash is erased, the
  file is deleted if the controller can delete it, and the value is useless.
- Only one replica generates it: the generation is a database-guarded insert;
  other replicas log that another replica generated it.
- The endpoint is rate-limited and returns the same error for "wrong value"
  and "already used".

### 6.5 PAT lifetime

- Every new PAT has an expiry. The default is 90 days and the maximum is 365
  days, configurable with `tokens.defaultLifetime` and `tokens.maxLifetime`.
  `tokens.maxLifetime: 0` allows non-expiring PATs for operators who
  explicitly accept that.
- PATs of the built-in `admin` account are capped at 30 days regardless of
  `tokens.maxLifetime`.
- `token list` shows the expiry; the controller logs a daily warning for PATs
  that expire within 7 days and exports a gauge of them.

### 6.6 Recovery command

- `unified-controller admin recover [--ttl 1h] [--enable-admin]` connects to
  the database with the controller's normal configuration (config file, env,
  flags) and prints a PAT of the built-in `admin` account, default lifetime 1
  hour, maximum 24 hours.
- `--enable-admin` also re-enables `admin` when the config disabled it; the
  enablement lasts until the PAT expires, so the config remains the long-term
  authority.
- Runs wherever the controller runs: on the host, `docker compose exec
  controller unified-controller admin recover`, or `kubectl exec`. Needing
  database credentials is the gate, like Jenkins' `$JENKINS_HOME` or GitLab's
  rake task.
- Writes an audit row with actor `recovery`.
- The controller binary gains subcommands for this; running it with no
  subcommand keeps starting the server as today.

### 6.7 Web UI token entry

- When browser SSO is configured (OIDC with a client secret), the web UI hides
  the "enter token manually" form and the server rejects PAT bearer credentials
  on requests that carry browser fetch metadata (`Sec-Fetch-Site` present;
  Node's fetch sends `Sec-Fetch-Mode` only and is not affected, and browsers
  send no `Sec-Fetch-*` headers to plain-http non-localhost origins). Humans use
  the UI through SSO; CLI and API clients keep using PATs.
- This is a policy for the web UI, not a security boundary: a person holding a
  service-account PAT can still call the API with curl. The audit log, PAT
  expiry and `last_used_at` are the controls for that.
- `ui.allowTokenEntry: true` turns the form back on while SSO is configured,
  for example during an IdP outage.
- Without browser SSO (no OIDC, or OIDC without a client secret) the form
  stays, as today.

### 6.8 Startup rule

The controller no longer requires `UNIFIED_TOKEN`. It starts when:

- SSO is configured, or
- the bootstrap is done, or
- it can generate (or was given) the initial token.

`--token` / `UNIFIED_TOKEN` on the controller are removed. If set, the
controller logs an error that names the migration guide and keeps starting
(see 7), so an upgrade does not turn into an outage.

### 6.9 Audit identities

| Principal | `actorId` |
|---|---|
| human account (session, id_token, or its PAT) | `oidc:<sub>` for sessions and id_tokens, `pat:<id>` for PATs, as today |
| service account PAT | `pat:<id>`; the row's details also carry `account: sa:<name>` |
| recovery command | `recovery` |
| initial token exchange | `bootstrap` |

`break-glass` stops appearing in new rows; old rows keep it.

### 6.10 Details settled after review (author's calls)

- **Changing the IdP.** Accounts are matched by `sub`. Switching to an IdP
  with different subjects creates new human accounts on first sign-in; the old
  ones keep their PATs until an admin disables them. Documented in the
  migration guide and in authentication.
- **When a human account is written.** Only after the existing admission
  check succeeds; a person who is refused ("forbidden: no role") leaves no
  row. On the bearer id_token path the account is upserted at most once per
  minute per `sub` (an in-memory cache), so `last_seen_at` and `claims` do not
  add a write to every request.
- **PAT-from-PAT.** A PAT may create PATs only for its own account, within its
  own ceiling and the lifetime limits; this is how CI rotates its token without
  an admin. This includes the built-in `admin` account (an `admin` PAT can mint
  a successor, still capped at 30 days). Creating a PAT for another account
  needs `token.manage` or `serviceaccount.token`, and never targets `admin`.
- **`--enable-admin` lease.** Stored as `admin_enabled_until` in
  `controller_settings`. Startup config sync does not override an unexpired
  lease, and the request path treats `admin` as enabled while
  `now < admin_enabled_until`. A recovery PAT's expiry never exceeds the lease.
- **Recovery command configuration.** The controller's config loading (file,
  env, flags) is factored out of `main` so `admin recover` uses the same
  effective configuration, including `serviceAccounts`, `tokens` and the
  database settings.
- **`UNIFIED_TOKEN` after the upgrade.** The startup sync and delete of the
  `env:UNIFIED_TOKEN` row are removed; a set `UNIFIED_TOKEN` (any value) is
  ignored with an error log naming the migration guide and never creates a
  row. `CheckAdminToken` and `UNIFIED_ALLOW_DEFAULT_TOKEN` are removed (the
  latter is ignored with a warning). The reserved name prefixes `env:`,
  and `system:` are rejected for new PAT and service-account names.
- **Old sessions without claims** are deleted by the migration; those users
  sign in again (as with the matrix-authorization upgrade).
- **Web UI and Sec-Fetch.** Only PAT bearer credentials are refused on
  browser-originated requests; session cookies and OIDC id_tokens are
  unaffected, and so is the CLI.

## 7. Migration (one release, breaking)

The owner accepted a breaking upgrade with no compatibility period, and an
upgrade that stops every controller replica first.

**Upgrade procedure.** Stop every controller replica, upgrade, start. Old and
new replicas never run against the same database at the same time; the
migration does not keep old columns for them. Agents are not affected.

A migration and the first start of the new version do the following.

1. **Human owners.** For each distinct `owner_sub` in `pats` and `sub` in
   `sessions`, create a human account with the latest email and claims, and
   point the PATs and sessions at it (`account_id`, NOT NULL afterwards). The
   old owner columns are dropped in the same migration.
2. **Ownerless PATs are deleted.** Every PAT without an owner is removed: the
   `env:UNIFIED_TOKEN` row, role-only PATs (created before owners existed, with
   `UNIFIED_TOKEN`, or with another role-only PAT), and ownerless
   enrollment-only PATs. The migration writes one audit row per deleted PAT
   (id, name, role, `created_by`) and the first start logs the list, so
   operators can see what to re-issue.
3. **Bootstrap on upgrade.** If, after step 2, SSO is not configured, the
   bootstrap flag is not set, so the first start generates an initial token
   exactly as for a fresh install (6.4). An upgraded non-SSO install is
   therefore re-bootstrapped: read the file, run `unified-cli bootstrap`,
   declare service accounts, issue new PATs.
4. Owned PATs that survive keep their current expiry, including none; only
   new PATs get the lifetime limits of 6.5. The migration guide recommends
   re-issuing them.

There are no `legacy-*` accounts and no account grants stored in the
database: every service account's permissions come from `serviceAccounts`.

## 8. Changes outside the controller

- Helm chart: the trial Secret no longer ships `dev-token-change-me`; it sets
  `UNIFIED_INITIAL_ADMIN_TOKEN` to a value generated at install time (stored
  in a Secret created once, like the KEK). `NOTES.txt` shows how to read it
  and run `unified-cli bootstrap`.
- `deployments/docker`: `.env.example` and the compose file replace
  `UNIFIED_TOKEN` with `UNIFIED_INITIAL_ADMIN_TOKEN` and a bootstrap step.
- Dev mode (`UNIFIED_DEV_MODE=1`) and the test harnesses (`test/ha`,
  `test/edgecase`, integration tests): a fixed `UNIFIED_INITIAL_ADMIN_TOKEN`
  plus one bootstrap call in the setup scripts. Dev mode does not get a
  standing admin token.
- CLI: `unified-cli bootstrap`, `account list|disable|enable`,
  `serviceaccount list`, `token create --account <sa>` (needs `token.manage`
  or the new permission `serviceaccount.token`; never for `admin`).
- Web UI: an accounts page (list, disable/enable humans, service accounts and
  their PATs).
- Docs: authentication, authorization (break-glass section rewritten),
  installation, quickstart, chart README, troubleshooting, a migration guide
  under `docs/operator-manual/migrations/`.

## 9. Left to the builder

- Exact table and column names, indexes, and how the `owner_*` columns on
  `pats` and the identity columns on `sessions` are dropped (same migration or
  the next release).
- How `Principal` is reshaped (an `Account` reference replacing `Sub`/`Email`/
  `Claims` and `BreakGlass`).
- API paths and payloads for accounts and service accounts, following the
  existing `/api/v1/tokens` style.
- The exact log and error strings, written so they can be grepped and listed
  in Troubleshooting.
- Splitting the work into tasks; the migration (7) and the startup rule (6.8)
  must land together.
