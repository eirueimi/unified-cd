# Authorization (RBAC and matrix permissions)

unified-cd authorizes every human caller with a set of **permissions** drawn from a fixed catalogue of 30. The three roles — **admin**, **developer**, **viewer** — still exist and still work exactly as before, but they are now *named bundles* of permissions. For a single-team deployment nothing has to change: `roleMap` / `userMap` / `defaultRole` keep their meaning and produce one global role per caller.

To let several teams share one deployment without touching each other's jobs, add the optional `oidc.permissions` block. It grants permissions (individually or as role bundles) to OIDC groups and users, globally or **per job folder**, in the style of Jenkins' project-based matrix authorization. A folder is just a path prefix of the qualified job name: `team-a` covers `team-a/build` and `team-a/sub/build`.

Every route is classified: the API refuses an action the caller has no permission for, and the Web UI additionally hides or disables controls the caller lacks. The server is always the authority; hiding a control is a convenience, not a guard.

## Permission catalogue

There are 30 permissions, written `<resource>.<verb>`. Nine are **folder-scopable**: they can be granted globally or on a folder and are checked against the job a request targets. Twenty-one are **global-only**: they can only be granted in `permissions.global` (or through a role).

### Folder-scopable (9)

| Permission | Meaning | viewer | developer | admin |
|---|---|:--:|:--:|:--:|
| `job.read` | See a job's definition, YAML and schedulability, and its runs: run detail, YAML snapshot, steps, logs (tail, archive, range, search, stats), the SSE event stream, outputs, the approvals list and artifacts (list, download). Also decides which jobs and runs appear in lists and which schedules are listed. | yes | yes | yes |
| `job.configure` | Create or update a job (`apply`; an upsert). | | yes | yes |
| `job.delete` | Delete a job. **This also deletes its run history and its schedules** (foreign-key cascade). | | yes | yes |
| `run.trigger` | Start a run of a job, including "Rerun" in the Web UI (which is a trigger with the old run's parameters). | | yes | yes |
| `run.replay` | Re-run a run's stored *snapshot* spec (`run replay`). Separate from `run.trigger` because the snapshot may be a definition that has since been fixed. | | yes | yes |
| `run.cancel` | Cancel a run. | | yes | yes |
| `run.delete` | Delete a run. | | yes | yes |
| `run.approve` | Approve or reject a waiting `approval` step. | | yes | yes |
| `schedule.configure` | Create, re-target or delete a Schedule that targets the job. | | yes | yes |

### Global-only (21)

| Permission | Meaning | viewer | developer | admin |
|---|---|:--:|:--:|:--:|
| `secret.list` | List secret names (values are never returned by the API). | | yes | yes |
| `secret.write` | Set or delete a secret. | | | yes |
| `gitcredential.read` | List GitCredentials. | | | yes |
| `gitcredential.write` | Create, update or delete a GitCredential. | | | yes |
| `vars.read` | List `Vars` manifests (plain-text values). | yes | yes | yes |
| `vars.write` | Apply or delete a `Vars` manifest. | | | yes |
| `agentvolume.read` | List `AgentVolume` manifests. | yes | yes | yes |
| `agentvolume.write` | Apply or delete an `AgentVolume` manifest. | | | yes |
| `account.manage` | Manage accounts (internal accounts and service accounts). | | | yes |
| `serviceaccount.token` | Create, list and revoke personal access tokens of service accounts other than `admin`. A holder can mint PATs carrying any such account's grants, so this permission is effectively the union of all service-account grants. | | | yes |
| `webhook.read` | List WebhookReceivers. | yes | yes | yes |
| `webhook.write` | Apply or delete a WebhookReceiver. | | | yes |
| `appsource.read` | List and get AppSources. | yes | yes | yes |
| `appsource.write` | Apply, delete or force-sync an AppSource. | | | yes |
| `agent.read` | List and get agents, an agent's recent runs (rows are additionally filtered by `job.read`), enrollments, enrollment policies and agent identities. | yes | yes | yes |
| `agent.manage` | Create and revoke enrollments; create, update and delete enrollment policies; enable and disable identities; revoke agent credentials. | | | yes |
| `agent.enroll` | Create host-agent enrollment tokens, limited to the labels bound to the caller's PAT (see [Enrollment-only PATs](#enrollment-only-pats)). | | | yes |
| `token.create` | Create PATs (never above your own permissions); list and delete **your own** PATs. | | yes | yes |
| `token.manage` | List and delete **every** PAT. | | | yes |
| `audit.read` | Read the [audit log](audit.md). | | | yes |
| `authz.read` | Read the resolved policy and explain any identity's permissions ([below](#inspecting-the-policy-authz-show-and-authz-explain)). | | | yes |

A permission is checked against exactly the route that needs it; the table in [Route to permission](#route-to-permission) lists every route. Agent credentials are not part of this model (see [Agent authorization boundaries](#agent-authorization-boundaries)).

### Role bundles

A role used anywhere a permission can be named expands to its bundle:

| Bundle | Permissions |
|---|---|
| `viewer` | `job.read`, `vars.read`, `webhook.read`, `appsource.read`, `agent.read`, `agentvolume.read` |
| `developer` | `viewer` plus `job.configure`, `job.delete`, `run.trigger`, `run.replay`, `run.cancel`, `run.delete`, `run.approve`, `schedule.configure`, `secret.list`, `token.create` |
| `admin` | all 30 (`authz.read` is only in this bundle) |

A role resolved by `roleMap` / `userMap` / `defaultRole`, a PAT's `--role` ceiling and a role named in a service account's `permissions` are bundles; a role in `roleMap` / `userMap` / `defaultRole` is a *global* grant of its bundle. A bundle granted on a **folder** contributes only its folder-scopable members: `admin` on `team-a` means "every one of the nine folder-scopable permissions under `team-a/`", never `secret.write`.

### Route to permission

| Route | Permission | Scope |
|---|---|---|
| `GET /api/v1/jobs` | `job.read` | rows filtered by job |
| `GET /api/v1/jobs/{name}`, `/yaml`, `/schedulability` | `job.read` | the job |
| `POST /api/v1/jobs` | `job.configure` | the resulting qualified name |
| `DELETE /api/v1/jobs/{name}` | `job.delete` | the job |
| `POST /api/v1/runs` | `run.trigger` | the job in the body |
| `POST /api/v1/runs/{id}/replay` | `run.replay` | the run's job |
| `GET /api/v1/runs?jobName=` | `job.read` | the job |
| `GET /api/v1/runs/active` | `job.read` | rows filtered by job |
| `GET /api/v1/runs/{id}` and its `/outputs`, `/yaml`, `/steps`, `/events`, `/logs`, `/logs/archive`, `/logs/stats`, `/logs/range`, `/logs/search`, `/approvals` | `job.read` | the run's job |
| `POST /api/v1/runs/{id}/cancel` | `run.cancel` | the run's job |
| `DELETE /api/v1/runs/{id}` | `run.delete` | the run's job |
| `POST /api/v1/runs/{runID}/approvals/{stepIndex}` | `run.approve` | the run's job, checked when you decide |
| `GET /api/v1/runs/{runID}/artifacts` and `/artifacts/{name}` | `job.read` | the run's job |
| `GET`/`POST`/`DELETE /api/v1/secrets` | `secret.list` / `secret.write` / `secret.write` | global |
| `GET`/`POST`/`DELETE /api/v1/gitcredentials` | `gitcredential.read` / `gitcredential.write` / `gitcredential.write` | global |
| `GET`/`POST`/`DELETE /api/v1/vars` | `vars.read` / `vars.write` / `vars.write` | global |
| `GET`/`POST`/`DELETE /api/v1/agent-volumes` | `agentvolume.read` / `agentvolume.write` / `agentvolume.write` | global |
| `GET`/`POST`/`DELETE /api/v1/webhooks` | `webhook.read` / `webhook.write` / `webhook.write` | global |
| `GET`/`POST`/`DELETE /api/v1/appsources`, `POST .../sync` | `appsource.read` / `appsource.write` | global |
| `POST /api/v1/tokens` | `token.create` | global |
| `GET /api/v1/tokens`, `DELETE /api/v1/tokens/{id}` | `token.create` (your own) **or** `token.manage` (everyone's) | global |
| `GET /api/v1/schedules` | `job.read` | rows filtered by target job |
| `POST /api/v1/schedules` | `schedule.configure` | the target job, **and** the current target if the name exists |
| `DELETE /api/v1/schedules/{name}` | `schedule.configure` | the schedule's current target job |
| `GET /api/v1/agents`, `/agents/{id}`, `/agents/{id}/runs`, enrollments, enrollment policies, identities | `agent.read` | global |
| `POST /api/v1/agent-enrollments` | `agent.manage` **or** `agent.enroll` | global |
| other enrollment, policy and identity writes | `agent.manage` | global |
| `GET /api/v1/audit` | `audit.read` | global |
| `GET /api/v1/authz/policy`, `POST /api/v1/authz/explain` | `authz.read` | global |

Rows that earlier versions of this page omitted: **Vars**, **replay**, **run delete** and **artifacts**. Artifacts used to be open to "any authenticated human"; they now need `job.read` on the run's job, which every role holds globally, so role-based callers see no difference.

## Which jobs a caller can see

Authorization never needs the job row: a job's folder is a pure function of its name. A caller's effective permissions on job `J` are

    effective(J) = global  ∪  every grant on a proper ancestor folder of J

- Grants only add. There is no deny rule and no "do not inherit" flag, so reading one entry tells you a lower bound and adding a line can never remove access.
- Folder `team-a` applies to `team-a/build` and `team-a/sub/build`. It does **not** apply to `team-ab/build`, and it does **not** apply to a root-level job named exactly `team-a` (a job at the root is covered by global grants only; there is no `/` folder key).
- The check is on the **resulting** qualified name. A caller with `job.configure` on `team-a` who applies a job whose `metadata.annotations.path` is `team-b` is checked against `team-b/<name>` and refused. Direct apply does not collapse repeated slashes (`team-a//sub` is stored as `team-a//sub/<name>`), and authorization runs on exactly the stored string.

### What the caller sees when it is refused

| Situation | Response |
|---|---|
| The caller cannot read the job (no `job.read` on it) and asks for it, or for one of its runs by ID | The same `404` a missing job or run gets. The API does not confirm that a hidden job exists. |
| The caller can read the job but lacks the action | `403 forbidden: missing permission "run.trigger" on job "team-a/build"` (the permission and job names vary) |
| A global-only route | `403 forbidden: missing permission "secret.write"` |
| The caller holds no grant at all | `403 forbidden: no permissions` (session or OIDC bearer), `403 forbidden: token owner has no permissions` (a PAT whose ceiling leaves nothing) |
| The caller's account holds no grants: a human the OIDC policy no longer admits, or a service account with no `permissions` | `403 forbidden: no role` |
| The caller's account is disabled (or is a service account missing from `serviceAccounts`, or the built-in `admin` while it is disabled) | `403 forbidden: account disabled` |

Hidden jobs and runs are also absent from lists (`GET /jobs`, `GET /runs/active`, an agent's recent runs, `GET /schedules`), and the SSE stream `GET /runs/{id}/events` refuses at connect. Two routes answer a hidden resource in their own idiom, indistinguishable from a missing one: `POST /schedules` for a hidden target job answers `400 job not found: <name>`, and `DELETE /schedules/{name}` for a hidden schedule answers `204` without deleting anything (exactly what deleting a missing schedule does).

!!! warning "Job names are not confidential"
    Job and schedule *names* remain visible in global resources: a WebhookReceiver's `trigger.job`, an AppSource's managed-resource list, and the text of authorization errors. What is hidden is a job's contents, its runs and its logs. A schedule re-target that is refused on the **old** target answers `403 forbidden: missing permission "schedule.configure" on job "<old target>"`, which names that job.

!!! warning "Folders are not a secret boundary: `job.configure` on any folder is roughly reading every secret"
    Folder permissions govern what **humans** may do through the API and the UI. They do not confine what a job does once it runs. A job under `team-a/` can still reference any secret by name (secrets are global names, guarded only by the run's own declared secret list), `call:` any job, target any agent labels, download artifacts of any run by ID and inline any `uses:` template. A caller who may create or update a job in *any* folder can therefore write a job that reads *any* secret value, exactly as a `developer` can today. If teams must not see each other's secrets, run separate deployments.

!!! warning "Anyone who can push to an AppSource repository can place jobs in any folder"
    An AppSource's repository runs under the Git host's own permissions. The reconciler writes synced jobs as the system, into whatever folders the repository layout produces (a job's folder is its file's directory), and no folder grant is consulted. Branch protection, CODEOWNERS and review rules on the Git side are what govern who may write which directory. Folder grants cover only actions taken through unified-cd's own API and Web UI. See [AppSource](../user-guide/resources/app-source.md#managed-resource-protection).

Secret *values* are never returned by the API (names only). Treat creating a job as running code with the agent's reach. On a Kubernetes agent, what a job can reach through its Pod (a ServiceAccount, host access, Kubernetes Secrets, PVCs) is limited by the agent's `jobPodPolicy`, which refuses those fields by default until the operator allows them, plus the job namespace's admission policy (the shipped `ci` namespace enforces Pod Security `baseline`). Permissions do not change what that policy allows; see [Job Pod field policy](kubernetes-integration.md#job-pod-field-policy).

## Configuring grants: `oidc.permissions`

The block is optional, additive and **config-file only** (there is no environment variable for it, as for `roleMap` / `userMap`). Like the rest of the controller configuration it is read at startup; changing it needs a restart. It requires `oidc.issuer` (every grant matches an OIDC identity).

```yaml
oidc:
  issuer:     https://idp.example.com
  clientId:   unified-cd
  rolesClaim: groups

  # Unchanged. Produces the caller's global role (bundle).
  roleMap:
    "my-org:platform": admin
  userMap:
    oncall-lead@example.com: admin
  defaultRole: deny                     # nobody reads everything by default

  # New, optional, additive. A value is a role name, a permission name,
  # or a list mixing both.
  permissions:
    global:
      groups:
        "my-org:engineering": [agent.read, appsource.read, webhook.read, vars.read, token.create]
        "my-org:auditors":    [viewer, audit.read]
    folders:
      team-a:
        groups:
          "my-org:team-a": developer
      team-a/prod:
        groups:
          "my-org:sre": [job.read, run.approve]
      team-b:
        groups:
          "my-org:team-b": developer
        users:
          contractor@example.com: [job.read, run.trigger]
```

Rules:

- `groups` keys are compared to the values of `rolesClaim`. `users` keys are compared to the token's `email` **and** `sub` (both, not first-hit; `users` is additive, unlike `userMap`). When the token reports `email_verified: false` (boolean or the string `"false"`), the email is not used for matching, only `sub` is; see [Identity providers](identity-providers.md#how-unified-cd-reads-identity-what-every-idp-must-satisfy).
- A role name in a grant expands to its bundle; on a folder only the bundle's folder-scopable permissions apply.
- Naming a **global-only** permission in a folder entry is a startup error: the operator would believe it is scoped when it cannot be.
- `permissions.global` takes global-only permissions as well, for example `token.create` for everyone.
- Folder keys are literal job-name prefixes. Leading and trailing `/` are trimmed; segments are not DNS-validated (AppSource-derived folders are directory names).
- There are no custom role definitions. YAML anchors cover reuse (`&team-dev [job.read, run.trigger]`).

### Startup validation

The block is strictly validated; the controller refuses to start and logs the first problem on the `config file` error. The strings, so you can grep your logs:

| Condition | Error |
|---|---|
| unknown permission or role | `oidc.permissions.folders["team-a"].groups["my-org:team-a"]: unknown permission or role "jobs.read"` |
| global-only permission on a folder | `oidc.permissions.folders["team-a"].groups["my-org:team-a"]: permission "secret.write" is global-only and cannot be granted on a folder` |
| bad folder segment | `oidc.permissions.folders["team-a//x"]: invalid folder: segments must be non-empty and must not be "." or ".."` |
| wildcard in folder | `oidc.permissions.folders["team-*"]: invalid folder: folders are literal job-name prefixes, "*", "?", "[" are not allowed` |
| whitespace in folder | `oidc.permissions.folders["team a"]: invalid folder: whitespace is not allowed` |
| duplicate after normalisation | `oidc.permissions.folders: "team-a" and "/team-a/" are the same folder` |
| empty folder entry | `oidc.permissions.folders["team-a"]: no groups or users` |
| empty grant | `oidc.permissions.global.groups["my-org:x"]: empty grant` |
| unknown key | `oidc.permissions: unknown field "deny"` |
| no issuer | `oidc.permissions is set but oidc.issuer is not` |
| a `roleMap` value that is not a role, with a `permissions` block | `oidc.roleMap["my-org:devs"]: unknown role "develper" is ignored (valid: admin, developer, viewer)` |

Without a `permissions` block, an unknown `roleMap` role is only a **startup warning** (the controller keeps booting, because refusing to start on upgrade would break existing setups); it is an error as soon as the block exists. Only `roleMap` is checked: an unknown `userMap` role denies that user, it is not ignored. Two more startup warnings are described in [Operations](operations.md#authorization-startup-warnings).

## Role resolution settings (the legacy keys)

These keys produce the caller's **global role** and are unchanged:

```yaml
oidc:
  issuer:      https://...
  clientId:    unified-cd
  clientSecret: ...
  rolesClaim:  groups                 # ID-token claim to read group values from (default "groups")
  roleMap:                            # claim value -> role
    unified-admins:  admin
    unified-devs:    developer
    unified-viewers: viewer
  userMap:                            # email or sub -> role (individual override; IdPs without groups)
    alice@example.com:      admin
    breakglass@example.com: admin
  defaultRole: viewer                 # role when nothing matches; "" or "deny" => no global role
```

Scalars `rolesClaim` and `defaultRole` can also come from `UNIFIED_OIDC_ROLES_CLAIM` / `UNIFIED_OIDC_DEFAULT_ROLE`. **`roleMap`, `userMap` and `permissions` are maps and can only be set in the config file** (there is no env var for them). The requested OIDC scopes can be overridden with `oidc.scopes` / `UNIFIED_OIDC_SCOPES` (see [Identity providers](identity-providers.md#how-unified-cd-reads-identity-what-every-idp-must-satisfy)).

> **Verified email only.** `userMap` and `permissions.*.users` match an email only when the IdP does not report it as unverified. If the token carries `email_verified: false` (the JSON boolean or the string `"false"`), the email is ignored and matching falls back to `sub`. An absent `email_verified` claim leaves matching unchanged. Sessions and PATs created before an upgrade keep their stored email; see the upgrade note under [Identity providers](identity-providers.md#how-unified-cd-reads-identity-what-every-idp-must-satisfy) (delete such PATs with `unified-cli token delete`; users log out and in again for sessions).

> `groups` is NOT a standard OIDC claim — whether it exists and what it contains is entirely up to the IdP. `rolesClaim` lets you point at whatever claim your IdP emits (`groups`, `roles`, a namespaced claim, a nested claim such as `realm_access.roles`, or even `email`). The name is tried as an exact top-level key first, then as a `.`-separated path.

## How a caller's permissions are resolved

For an OIDC caller (browser session or CLI bearer token):

1. **Global role** from the legacy keys, in the unchanged order: `userMap` (email, then sub) → `roleMap` (the highest-ranked match wins) → `defaultRole` (`""` or `deny` means no role). A `userMap` hit *replaces* group matches, so `userMap` can demote a user below their groups for the **global role**.
2. **Union** with every `oidc.permissions` entry that matches (`groups` against the claim values, `users` against email and sub). `userMap` demotion does **not** reach these: grants from `permissions.global` and `permissions.folders` still union in.
3. **Per request**, the effective permissions on job `J` are the global set plus the folder sets of `J`'s ancestor folders.

**The login rule.** A login is refused only when the legacy resolution denies **and** no `permissions` entry matches the identity: browser login answers `403 forbidden: no role assigned`, a bearer token `403 forbidden: no role`. A user who matches only a folder grant can log in; they hold no global role and see only that folder's jobs. With no `permissions` block this is exactly the old rule. To keep folders private, set `defaultRole: deny` (or `viewer` only if everyone may read everything).

A browser session stores the caller's claim values (not a role) and recomputes the permissions from them and the **current** config on every request, so editing `roleMap` takes effect for live sessions at the next restart without a re-login. Claim values are refreshed at login and on silent refresh.

There is no credential that skips this resolution: even the built-in `admin` account is a service account whose PATs expire and can be disabled; see [Built-in admin account and recovery](#built-in-admin-account-and-recovery).

### Service accounts

A **service account** is a non-human account for CI and other automation. It is declared in the controller config (top-level `serviceAccounts`, independent of `oidc:`), so it works the same with or without SSO, and its permissions come only from the config file, read at startup.

```yaml
serviceAccounts:
  admin:                 # built-in: only `enabled` may be set
    enabled: true
  ci-deploy:
    permissions:
      global: [viewer, token.create]   # token.create lets CI rotate its own PAT
      folders:
        team-a: developer
        team-a/prod: [job.read, run.trigger]
```

- The grant syntax and validation are those of one `users:` entry of `oidc.permissions`: role names, permission names or lists of them; a global-only permission on a folder is an error.
- Names are DNS labels and must not start with `env:` or `system:`.
- An entry removed from the file is disabled at the next start (the row stays for the audit log); adding it back re-enables it.
- A service account with no `permissions` authenticates but holds nothing (`403 forbidden: no role`).
- Issue its PAT with `unified-cli token create <name> --account <service-account>`. That needs `token.manage` or `serviceaccount.token`, and is refused for `admin`. For the service account to rotate its own PAT it also needs `token.create`.
- The validation errors, so you can grep your logs: `serviceAccounts["x"]: invalid name: must be a DNS label and must not start with "env:" or "system:"`, `serviceAccounts["admin"]: only "enabled" may be set for the built-in admin account`, `serviceAccounts["x"].permissions: unknown field "y"`, `serviceAccounts["x"].permissions.global: unknown permission or role "jobs.read"`, `serviceAccounts["x"].permissions.folders["f"]: permission "secret.write" is global-only and cannot be granted on a folder`, `serviceAccounts["x"].permissions.folders["f"]: empty grant`.

### Human accounts

A **human account** is created the first time a person signs in through SSO (browser login, CLI device flow or a bearer id_token) **and** passes the admission check above; a person refused with `forbidden: no role assigned` / `forbidden: no role` leaves no row. Accounts are matched by the IdP's `sub`, and each login refreshes the stored email and group snapshot. An admin with `account.manage` can disable and re-enable a human account (`unified-cli account disable <id>`, or the Accounts page of the web UI). Disabling refuses all of its PATs and sessions at once, and refuses a new SSO sign-in with `forbidden: account disabled`. The person's CLI id_token is refused immediately on the controller replica that handled the disable and within 1 minute on the others; sessions and PATs are refused immediately everywhere.

If you switch to an IdP with different subjects, the people sign in as new human accounts; the old accounts keep their PATs until you disable them.

### Personal access tokens

Every PAT belongs to an account: a human's, or a service account's. A PAT's effective permissions are

    matrix(account, current config)  ∩  ceiling

recomputed on every request.

- **Account.** A PAT can never exceed its account, globally or on any folder. Removing the person from `roleMap` / `userMap` / `permissions` (or a service account from `serviceAccounts`) takes effect for all of its PATs at the next controller start with no re-issue; group changes in the IdP are picked up when the person next logs in through the browser. Disabling the account stops every PAT at once.
- **Ceiling.** Optional: either a role (`--role`) or a list of permissions (`--permission`, repeatable). No ceiling means "everything the account has". A ceiling above what the creator holds is refused with a `400`; nothing is ever silently reduced.
- **Lifetime.** Every new PAT expires: `--expires-in` or `tokens.defaultLifetime` (90 days by default), capped by `tokens.maxLifetime` (365 days by default; the built-in `admin` account is always capped at 720h). See [Configuration: tokens](../reference/configuration.md#tokens-pat-lifetimes).
- **PAT created by a PAT.** A PAT may create PATs only for its own account, within its own ceiling and the lifetime limits; this is how CI rotates its token without an admin, and it needs `token.create` on the service account. Creating a PAT for another account needs `token.manage` or `serviceaccount.token` and never targets `admin`. A child's ceiling is the parent's ceiling intersected with the requested one.
- A PAT whose ceiling leaves nothing, or whose account is gone, answers `403` on every route (`forbidden: token owner has no permissions`, `forbidden: no role` or `forbidden: account disabled`), so the failure is diagnosable.
- `GET` and `DELETE /api/v1/tokens` act on your own PATs with `token.create`, on everyone's with `token.manage` (admin only), and on those of service accounts other than `admin` with `serviceaccount.token`. `token list` labels each PAT's account: the person's email (or `sub`), or `sa:<name>` for a service account.
- Creating a PAT is refused for an OIDC bearer id_token without the roles claim while the config has group-keyed grants, with the strings listed in [CLI: token create](../reference/cli.md#token-create) and in [Troubleshooting](../troubleshooting/controller-and-database.md#token-creation-is-refused-cannot-create-a-token).

See [Authentication](authentication.md) for sessions, silent refresh and PAT accounts.

### Enrollment-only PATs

An enrollment-only PAT is a PAT whose only permission is [`agent.enroll`](#permission-catalogue). It can create host-agent enrollment tokens (`POST /api/v1/agent-enrollments`) and nothing else: it cannot list agents, read jobs or manage anything. It is meant for automation that provisions hosts, such as a VM bootstrap script, so that the credential handed to that automation cannot do more than enroll agents.

```bash
unified-cli token create provisioner \
  --permission agent.enroll \
  --enroll-label kind:linux --enroll-label pool:build
```

- **Label bound.** `--enroll-label` (repeatable; API field `enrollLabels`) restricts the labels of the enrollment tokens the PAT may create. It is valid only together with `agent.enroll`, entries must be non-empty and unique, and a PAT created without it has no bound. The bound is stored with the PAT, shown by `token list`, and echoed by `token create`. The controller enforces it when an enrollment token is created with such a PAT: every requested label must exactly match a bound entry (an unbounded PAT can request no labels), and the agent ID cannot be chosen, it is generated. See [Provisioning many VMs](agents.md#provisioning-many-vms-an-enrollment-only-pat).
- **Who may mint one.** A creator must hold `agent.enroll` or `agent.manage`. Unless the creator holds `agent.manage`, it can only grant labels within its own enrollment-label bound: the requested `enrollLabels` must be a subset of the creating token's (`enrollLabels must be a subset of the creating token's enrollLabels`, 403), and a creator with no bound, such as a session whose role grants `agent.enroll` or a PAT minted without `--enroll-label`, can grant no labels at all. When a bounded parent asks for none it inherits the parent's bound rather than becoming unbounded. A creator that holds `agent.manage` is not restricted.
- **Account-bound.** An enrollment-only PAT belongs to an account like every PAT (there are no ownerless PATs): its permissions are the account's intersected with the ceiling `[agent.enroll]`. A service account that should provision hosts therefore needs `agent.enroll` (or `agent.manage`) in its `permissions`, and the PAT is created with `--account <service-account>`. An account that holds `agent.manage` but not `agent.enroll` is treated as holding `agent.enroll` for this purpose.

The refusals are listed in [CLI: token create](../reference/cli.md#token-create) and in [Troubleshooting](../troubleshooting/controller-and-database.md#token-creation-is-refused-cannot-create-a-token).

## Separating teams: a recipe

1. Move team groups out of `roleMap` (or leave them out of it) and set `defaultRole: deny`.
2. Add one folder entry per team; give each team the `developer` bundle on its folder.
3. Grant the global-only permissions the teams still need in `permissions.global`: typically `agent.read`, `webhook.read`, `appsource.read`, `vars.read` and `token.create` (without `token.create` a folder-only user cannot create PATs).
4. Restart the controller. Users log in again so their sessions store their groups.
5. **Re-issue team PATs.** A PAT is bounded by its account's current permissions, so PATs of people moved to folder grants lose their global role at the next restart. Give automation a [service account](#service-accounts) with folder grants instead of a person's PAT.
6. Check the result with `unified-cli authz explain`.

Because grants are unions, a team cannot be kept out of a *nested* folder it already covers. To keep a team out of its own production approvals, use **sibling folders** rather than nesting:

```yaml
oidc:
  defaultRole: deny
  permissions:
    folders:
      team-a:                       # builds and tests: the team has full control
        groups:
          "my-org:team-a": developer
      team-a-prod:                  # production: the team can look and trigger, SRE approves
        groups:
          "my-org:team-a": [job.read, run.trigger]
          "my-org:sre":    developer
```

With `team-a/prod` as a child of `team-a`, the `developer` grant on `team-a` would also cover `team-a/prod/deploy` and the team could approve its own deployments.

## Inspecting the policy: `authz show` and `authz explain`

Both commands need the `authz.read` permission (admin bundle only; it can be granted on its own in `permissions.global`) and print what the controller resolved at startup, never the raw file.

```bash
# The resolved policy: bundles, roleMap/userMap/defaultRole, every permissions entry.
unified-cli authz show

# Why does this identity hold what it holds?
unified-cli authz explain --user dev@example.com --group my-org:team-a --job team-a/build
```

`explain` lists every held permission in catalogue order with **all** the grants that give it, using the same strings the [audit log](audit.md#permission-scope-and-grantedby) records. It describes an identity, not a token: a PAT's ceiling is not applied. Within one scope, group entries are listed before user entries. See [CLI reference](../reference/cli.md#authz) for flags and output formats.

## GitHub App credentials and `jobAccess`

`gitcredential.write` (global only) governs everything about a [GitHub App credential](../user-guide/resources/git-credential.md#github-app-credentials), including `jobAccess`: which job folders may receive a token, which repositories it reaches and its permission ceiling. No job-level or folder-level permission lets a caller change it, so a job author cannot widen a credential's reach. `gitcredential.read` lists credentials, with `jobAccess` shown but never key material. A GitCredential applied through an AppSource can set `jobAccess` as well; whoever can push to that repository can therefore grant jobs access, which is the trust boundary of [AppSource](../user-guide/resources/app-source.md).

`jobAccess.folders` uses the same folder rule as the folder-scopable permissions (`team-a` covers `team-a/build` but not a job named `team-a`; a root-level job is covered only by `"*"`). It is checked **at fetch time**, when the run's agent requests its secrets, against the credential as it is then; changing `jobAccess` affects runs that are already queued. A refused request is answered with `403` and audited (`gitcredential.token_issue`, see [Audit Log](audit.md#github-app-job-tokens)). The agent's own authorization (the run's `claimed_by` check and the "secret not needed by this run" check) applies first.

## Agent authorization boundaries

An agent credential is bound to one immutable `agentId`. The controller
rejects a differing route/body ID with `agent identity mismatch` (403), and
preserves the run's immutable `claimed_by` check for writes. An agent cannot
grant itself scheduling labels or capabilities. Administrators manage
one-time VM enrollments, enrollment policies (Kubernetes, AWS, GCP and Azure), and identity enable,
disable, and credential revocation through the agent lifecycle API/CLI.

**Every write to a run is guarded by the same per-run ownership check,
applied uniformly to every bearer (enrolled) agent.** Step reports, log
lines, step/run outputs, sidecar status, finishing the run, and artifact
uploads are all checked against that run's `claimed_by`. A write to a run
the caller did not claim under the agent ID it presents gets `403 run <id>
is claimed by another agent`. The artifact-upload route has no per-agent
path segment, so the ownership guard is applied against the `agentId` carried
by the caller's own credential: an agent whose ID does not match the run's
`claimed_by` gets `403`, exactly like every other run-write path. Secret
fetch is fenced the same way for every principal: an agent can only request
secret names the run's own spec declares, or it gets `403 secret not needed
by this run` (see [Secrets: Access control](../user-guide/secrets.md#access-control)).

Agent principals are untouched by permissions. On the agent-or-human routes (`GET /runs/{id}`, `/runs/{id}/outputs`, run artifacts, `GET /agents/{agentId}` and its `/runs`) an agent skips the permission check and only the human branch is checked. An agent credential presented to any other human route is refused with `403 forbidden: missing permission "<permission>"`.

Several things act as the system, with no human in the request, so no permission is consulted when they fire: a Schedule, a WebhookReceiver, an AppSource sync, and the child run a `call:` step creates (created by the agent principal, and free to target any job). The authorization happens when the trigger is *configured*: creating or re-targeting a Schedule needs `schedule.configure` on the target job's folder, while WebhookReceivers and AppSources are global, admin-bundle resources. The folder of a `call:` child decides who can see it, so a team may see the parent run but get `404` following the child run's ID.

## SSO topologies

For per-IdP setup steps (app registration, claim configuration, value shapes) see [Identity provider setup](identity-providers.md).

unified-cd is a generic OIDC relying party and points at exactly **one** issuer. Both setups below use the same `roleMap`/`userMap`/enforcement — only `issuer`/`clientId`/`rolesClaim` and the presence of Dex connectors differ.

### (A) Dex as broker (recommended for multiple sources / GitHub / uniform CLI device flow)

unified-cd → Dex; add a Dex connector per identity source.

```yaml
# unified-cd
oidc:
  issuer:         https://<controller>/dex
  clientId:       unified-cd
  clientSecret:   unified-cd-secret
  deviceClientId: unified-cd-cli
  rolesClaim:     groups
  roleMap:
    "my-org:platform": admin       # value shape depends on the connector
    "mygroup/devs":    developer
  userMap:
    alice@example.com: admin
  defaultRole:    viewer
```

```yaml
# Dex connectors (add only what you need)
connectors:
  - type: github            # groups: "org:team" — GitHub is non-OIDC, so this connector is REQUIRED for GitHub
    id: github
    config: { clientID: ..., clientSecret: ..., redirectURI: https://<dex>/callback, orgs: [{name: my-org}], teamNameField: slug }
  - type: gitlab            # groups: "group/subgroup"
    id: gitlab
    config: { baseURL: https://gitlab.example.com, clientID: ..., clientSecret: ... }
  - type: oidc              # Entra / Auth0 / generic — passes upstream claims through
    id: entra
    config: { issuer: https://login.microsoftonline.com/<tenant>/v2.0, clientID: ..., clientSecret: ..., insecureEnableGroups: true, scopes: [openid, profile, email] }
# local static users (no groups → use userMap by email):
# enablePasswordDB: true
# staticPasswords: [{ email: alice@example.com, hash: "...", username: alice }]
```

Groups are fetched at login by the single connector the user authenticates through and baked into the id_token; unified-cd reads the claim from the token (it does not query Dex per request). Group changes take effect on next login / token refresh.

### (B) Direct to an IdP (no Dex)

Point `oidc.issuer` straight at the IdP; `roleMap` and enforcement are identical to (A). The claim must be a flat top-level claim in the ID token, because unified-cd reads neither `userinfo` nor the access token and does not resolve nested claim paths. Step-by-step setups with ready-to-use `oidc:` snippets for Microsoft Entra ID, Okta, Keycloak, Google Workspace, GitHub (via Dex), GitLab and Auth0 are in [Identity provider setup](identity-providers.md).

> **GitHub cannot be used directly** — it is not an OIDC provider (no id_token; org/team membership is only available via its REST API). Use topology (A) with the Dex `github` connector.

### Who produces the group values

| IdP (setup) | who builds the group/role claim | rolesClaim | roleMap value shape |
|---|---|---|---|
| local Dex (static) | (no groups) | `email` | use `userMap` (email→role) |
| Dex → GitHub | the Dex `github` connector (calls the GitHub API) | `groups` | `org:team` |
| Dex → GitLab | GitLab (or Dex `gitlab` connector) | `groups` | group path |
| Entra ID | Entra itself (App Roles or groups claim) | `roles` / `groups` | app-role value / group GUID |
| Auth0 | Auth0 (Post-Login Action) | namespaced claim | Auth0 role name |

## Built-in admin account and recovery

There is no standing admin token. The strongest identity is the built-in service account `admin`, which always exists, holds the admin bundle globally and cannot be given other permissions. Its only configurable setting is `enabled`:

```yaml
serviceAccounts:
  admin:
    enabled: false   # default true
```

How you get an `admin` PAT:

- **Initial token (non-SSO installs).** On the first start where the bootstrap is not done and SSO is not configured, the controller generates a one-time initial admin token (`uci_` plus 40 hex characters) and writes it to `initial-admin-token` (mode `0600`) in the config file's directory, or the data directory when there is no config file. It logs the path, never the value. Exchange it once with `unified-cli bootstrap`; the response is an `admin` PAT valid for 30 days. The token is useless afterwards, the file is deleted, and the endpoint is rate limited (5 attempts per minute) and answers `invalid or already used initial token` for both a wrong and a used value. To choose the value yourself (automated installs), set `UNIFIED_INITIAL_ADMIN_TOKEN` on the controller; the same one-time rules apply. With SSO configured no initial token is generated: the first admin comes from `oidc.userMap` / `roleMap`.
- **Recovery command.** `unified-controller admin recover [--ttl 1h] [--enable-admin]` prints an `admin` PAT straight from the database. It needs database access with the controller's configuration, which is the gate (as with Jenkins' `$JENKINS_HOME` or GitLab's rake tasks); run it with `docker compose exec` or `kubectl exec` where the controller runs. The default lifetime is 1 hour and the maximum 24 hours. If the config disables `admin`, pass `--enable-admin`: `admin` is then enabled until the PAT expires (a lease that replaces any earlier one), and the config remains the long-term authority. It writes an audit row with actor `recovery`.
- **Nobody can mint an `admin` PAT through the token API** (`PATs for the built-in admin account can only be obtained through the initial token or the recovery command`), except an `admin` PAT creating its own successor, which is still capped at 30 days.

`admin` PATs always expire (at most 720h, whatever `tokens.maxLifetime` says), so use the account to set up SSO or service accounts and then stop using it.

When an OIDC issuer is configured and `admin` is still enabled, the controller keeps booting but logs this warning at startup:

```
the built-in admin service account is enabled while SSO is configured; set serviceAccounts.admin.enabled: false once SSO works
```

Disable `admin` once an SSO admin can sign in; `admin recover --enable-admin` is the way back if the IdP is unavailable or misconfigured. The `env:` and `system:` name prefixes are reserved: `token create` (and `POST /api/v1/tokens`) rejects a PAT name starting with either with `400 token name "..." is reserved`, and service-account names may not start with them.

See [Migrating: internal accounts](migrations/2026-11-27-internal-accounts.md) for the upgrade from `UNIFIED_TOKEN`.

## Migration / backward compatibility

- A config with only `roleMap` / `userMap` / `defaultRole` keeps resolving people's permissions as before. No existing user gains a permission until you write `oidc.permissions`.
- PATs without an owner (role-only PATs, `UNIFIED_TOKEN`) no longer exist: the internal-accounts upgrade deletes them, and the controller ignores `UNIFIED_TOKEN`. See [Migrating: internal accounts](migrations/2026-11-27-internal-accounts.md).
- The deliberate differences of the matrix-authorization release (token list and delete narrowing, over-high `--role` refused instead of silently weakened) are listed with exact strings in [Migrating: matrix authorization](migrations/2026-10-28-matrix-authorization.md).
- The controller boots without OIDC when the initial admin token can be generated (or was provided) or the bootstrap is already done. "SSO required" is an operational policy plus always-on authorization, not a hard boot gate.
- When first enabling OIDC, start with `defaultRole: viewer`, populate `roleMap`/`userMap`, then tighten to `defaultRole: deny`.
