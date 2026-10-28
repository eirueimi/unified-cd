# Matrix authorization (fine-grained, per-folder permissions) — Design

Date: 2026-10-28
Status: Draft for review. Direction and every former open question decided by
the owner (§4); the designer's calls (§5) await review. No code, no plan.

## 1. Intent and why

unified-cd authorizes every human caller with one of three hierarchical roles
(`viewer < developer < admin`), resolved once per request and enforced by
`requireMinRole` on each route (`internal/controller/rbac.go`,
`internal/controller/server.go`). The role is global: a developer can apply,
trigger, cancel, replay, delete and approve **every** job in the deployment.

unified-cd is expected to be deployed once per project, not shared by many
unrelated organisations the way a hosted Buildkite is, so it does not need
tenant-grade isolation. It is still common for one project to have several
teams (an application team, a platform team, an SRE rota) that should not all
be able to touch everything. The maintainer wants the operator to be able to
say "which team can touch how far" inside one deployment.

The model is Jenkins' matrix authorization: a grid of **(OIDC group or user) ×
fine-grained permission**, plus per-folder overrides like Jenkins'
project-based matrix authorization. In unified-cd the folder is not a new
object: job names are already qualified (`team-a/build`), so a folder is a
path prefix of the qualified job name.

## 2. Goals, non-goals, anti-goals

**Goals**

- A permission catalogue in which every human-facing API route maps to
  exactly one named permission, and a test that fails when a route is added
  without one.
- Grants of permissions (individually or as role bundles) to OIDC groups and
  users, globally and per job folder, configured in the controller config file
  next to `oidc.roleMap` / `userMap` / `defaultRole`.
- Folder grants inherit downward (`team-a` covers `team-a/sub/x`), and all
  grants combine by union.
- The API hides what a caller cannot read (lists, run reads, logs, SSE,
  artifacts), and the Web UI hides or disables actions the caller lacks —
  with every action still enforced by the server (§5.16).
- The audit log records which permission an audited request was checked
  against and the outcome.
- PATs keep working, and a PAT can never exceed its creator.

**Non-goals**

- Issuing OIDC tokens to jobs for cloud IAM. Separate effort.
- Policy sourced from Git / an AppSource, and editing policy in the Web UI.
  The maintainer explicitly dropped both; the config file is the only source.
- Regex or glob role patterns (in claim values, user names or folder keys).
- Multi-tenant isolation. In particular this design does **not** confine
  what *job code* can do (see §5.9): a job in `team-a/` can still reference any
  secret by name, `call:` any job, target any agent labels, download artifacts
  of any run by `runId`, and inline any `uses:` template. Folder permissions
  govern what **humans** may do through the API and UI. `job.configure` on
  any folder is therefore, in practice, the ability to read every secret.
- Policing the contents of Git repositories. An AppSource's repository runs
  under the Git host's own permissions; anyone who can push to it can place
  jobs in **any** folder, and that is managed on the Git side (§4 item 13).
- Explicit deny rules, `inherit: false`, and an `approvers:` field on the
  `approval:` step.
- Folder-limited PAT ceilings, and per-folder WebhookReceivers (both later).
- Hot reload of the authorization config. Like the rest of the controller
  config it is read at startup.

**Anti-goals** — outcomes that make this a failure even if it ships:

- Anything that makes the single-team default setup harder than today. A
  config with only `roleMap` / `userMap` / `defaultRole`, or no OIDC at all,
  must need no edit and must behave exactly as before. The deliberate
  exceptions — the token-management narrowing (§5.6) and the PAT-creation
  refusals of §5.14 / §5.15 — are each called out and listed in the
  migration guide.
- Silently widening anyone's access on upgrade. No existing PAT, session or
  OIDC user may gain a permission it did not have before the operator wrote
  new config.
- A grant that looks folder-scoped but is in fact global (for example a folder
  entry that quietly grants `secret.write` deployment-wide). Such a config must
  fail at startup, not be "helpfully" interpreted.
- A second, divergent permission check in the Web UI that drifts from the
  server's. The UI may only hide; the server stays the authority.
- An unclassified route. A new route that nobody mapped to a permission must
  fail a test, not default to "any authenticated human".

## 3. Constraints

- **Identity resolution is fixed.** OIDC callers are identified by the values
  of `oidc.rolesClaim` (default `groups`), their `email`, and their `sub`
  (`extractRoleValues`, `resolveRole` in `rbac.go`). The new grants match on
  exactly these and nothing else.
- **Legacy resolution order must be preserved bit-for-bit.** `resolveRole`
  is `userMap` (email, then sub) → `roleMap` (highest rank) → `defaultRole`
  (`""`/`deny` ⇒ deny). Note that a `userMap` hit *replaces* group matches, so
  `userMap` can demote a user below their groups. That stays true for the
  global role.
- **Sessions store a role, not claims.** `sessions.role` is resolved at login
  (`auth_oidc.go`) and is not re-resolved on silent refresh. Groups are not
  stored anywhere today.
- **PATs store a role, not an owner.** `pats.role` is clamped to the creator's
  role at creation (`handleCreateToken`), but who created the PAT is not
  recorded. `GET /api/v1/tokens` and `DELETE /api/v1/tokens/{id}` operate on
  every PAT in the store for any developer, including other users' PATs and
  the bootstrap `env:UNIFIED_TOKEN` row.
- **Break-glass.** The PAT named `env:UNIFIED_TOKEN` (`BootstrapPATName`) is
  always admin (`ServerAuth`), checked before OIDC resolution. Must remain so.
- **Agents are a different kind of principal.** `AgentPrincipal` carries no
  role (`docs/contributing/auth-implementation.md`). Agent routes are declared
  in `agentRouteIdentityMatrix`; agent authorization (path identity binding,
  `claimed_by`, secret-fetch fence) is unchanged by this design.
- **Maps are config-file only.** `roleMap`/`userMap` have no env var
  (`internal/config/controller.go`); the new structure is nested maps and has
  none either.
- **Qualified names** (`internal/dsl/qualname.go`):
  `QualifyName(path, name)` trims `/` from both and joins them with `/`; the
  `path` is the reserved `metadata.annotations.path`. On direct apply
  (`storeJob`) each non-empty `path` segment must be DNS-1123 and repeated
  slashes are collapsed. On AppSource sync (`applyResource`,
  `appsource_reconciler.go`) the controller **overwrites** `annotations.path`
  with the file's directory relative to `spec.path` (`relDir`), and those
  directory names are *not* DNS-validated. The leaf (`metadata.name`) is always
  DNS-1123. A name without `/` is a root-level job. `SplitQualifiedName` splits
  on the last `/`.
- **Runs follow the job name.** `runs.job_name` and `schedules.job_name` are
  foreign keys to `jobs.name` with `ON DELETE CASCADE`, so deleting a job
  deletes its run history and schedules; there are no runs of a deleted job.
  `RenameJob` (used only by the AppSource legacy bare→qualified upgrade)
  repoints `runs.job_name`. Moving a job file between directories in an
  AppSource repo is *not* a rename: it creates a new qualified name, and the
  old one is pruned (with `prune: true`) or left behind.
- **Schedules have global names** but target one job (`spec.job`).
  WebhookReceivers have global names and target one job or one AppSource.
- **`uses:` templates are not controller resources.** A `JobTemplate` lives in
  Git and is fetched at run creation (`docs/user-guide/resources/job-template.md`);
  there is no API route that edits one.
- **The audit middleware is an allow-list** (`classifyAudit` in `audit.go`),
  wraps the role check, records only `POST`/`PUT`/`DELETE`, and already
  records `403`s for classified routes. `POST /runs/{id}/replay` is currently
  unclassified (TODO item 24).
- **The Web UI does no role gating today.** It calls `/api/v1/auth/me`, which
  returns only `sub`/`name`/`email`, and renders every action.

## 4. Decided parts (by the owner)

1. **Model:** Jenkins-style matrix — (OIDC group or user) × fine-grained
   permission — with per-folder grants like project-based matrix
   authorization.
2. **Folder = job-name path prefix.** No new folder object.
3. **Source of truth:** the controller config file only, next to
   `oidc.roleMap` / `userMap` / `defaultRole`. No Git/AppSource-sourced policy,
   no Web UI editor.
4. **Backward compatibility:** existing `roleMap` / `userMap` / `defaultRole`
   configs keep working unchanged; `admin` / `developer` / `viewer` remain, as
   named bundles of permissions.
5. **Break-glass** `UNIFIED_TOKEN` PAT stays always-admin.
6. **PATs** must interact with matrix permissions without exceeding their
   creator (rule designed in §5.5).
7. **Non-goals** as listed in §2: job OIDC tokens for cloud IAM, Git/AppSource
   policy, Web UI editing, regex role patterns, multi-tenant isolation.

Decided by the owner on review (these were the open questions of the first
draft):

8. **Job code is not confined by folder** (former Q1). Per-folder secret
   allow-lists, `call:` targets and agent-selector limits are non-goals. The
   warning stays: `job.configure` anywhere ≈ reading every secret (§5.9).
9. **Inheritance is union only** (former Q2). No `inherit: false`, no
   `approvers:` field. Keeping a team out of its own production approvals is
   done with sibling folders (`team-a/`, `team-a-prod/`), not nesting (§5.2).
10. **PAT folder limits: later** (former Q3). v1 ceilings are a role or a
    permission list; a `folders:` ceiling remains a compatible extension.
11. **Developers list and delete only their own PATs; `token.manage` is
    admin-only** (former Q4, §5.6).
12. **Per-folder WebhookReceivers: later** (former Q5). They stay global
    `webhook.*` resources in v1.
13. **No AppSource folder limit** (former Q6, dropped). Principle: Git
    repositories run under their own (Git host) permissions, so unified-cd does
    not police repository contents. Jobs synced by an AppSource are trusted as
    the repository writers' decision; folder grants govern only actions taken
    through unified-cd's own Web UI and API. Anyone who can push to an
    AppSource's repository can place jobs in any folder (the folder is the
    file's directory, §3), and controlling that is the Git host's job —
    branch protection, CODEOWNERS, review rules.
14. **One `job.read`** (former Q7) covers the definition, YAML, runs, logs,
    events, outputs, approvals list and artifacts. Splitting it later stays
    backward compatible (bundles and existing grants would imply the new
    permissions).
15. **The Web UI's permission handling must be solid** (owner emphasis):
    every UI action is enforced server-side, never by UI-only hiding; the UI
    hides or disables actions from the `/auth/me` grants, with a fixture shared
    by the server and web tests; unreadable jobs and runs are absent from
    lists, `404` on direct access and excluded from SSE streams; every
    decision is audited. Specified in §5.16.

## 5. Designer's calls

Each item below is the designer's call, with the reason, unless it says it
records an owner decision.

### 5.1 A 24-permission catalogue, `<resource>.<verb>`, one per route

Designer's call. Names follow the audit actions' `resource.verb` style
(`job.apply`, `run.trigger`, ...) so the two vocabularies read alike. Nine are
folder-scopable, fifteen are global-only (§7). The verbs are kept to what a
team would plausibly separate: Jenkins' `Job/Read`, `Job/Configure`,
`Job/Delete`, `Job/Build`, `Job/Cancel`, `Run/Replay`, `Run/Delete` all have a
counterpart; approval and schedule are unified-cd-specific.

- **One read permission per folder (`job.read`)** covers the definition, the
  YAML, runs, steps, logs, SSE, outputs, approvals list and artifacts.
  Splitting logs or artifacts off is a compatible later change (the bundles
  would include the new permission). Today every role can read all of these,
  so nobody needs the split to keep current behavior. Confirmed by the owner
  (§4 item 14).
- **`job.configure` is create-or-update.** Apply is an upsert; splitting create
  from update would add a race-prone existence check for no stated need.
- **`run.replay` is separate from `run.trigger`.** Replay executes the run's
  *snapshot* spec, i.e. a definition that may since have been fixed (a secret
  reference removed, an `if:` tightened). That is closer to Jenkins'
  `Run/Replay` than to `Job/Build`.
- **`job.delete` is separate from `job.configure`**, and the bundle doc says
  plainly that deleting a job deletes its run history and schedules (cascade).

### 5.2 Grants are unions; no deny; inheritance is downward and unconditional

Designer's call. A caller's effective permissions on job `J` are

    effective(J) = global ∪ ⋃ { folder(F) : F is a proper ancestor folder of J }

Union-only makes the grid monotone: adding a line can only grant, so reading
one entry tells you a lower bound, and "who can approve `team-a/prod/deploy`"
is answered by reading `global` plus the entries for `team-a` and
`team-a/prod`. An explicit deny or a "do not inherit" flag would make every
answer depend on the whole file, and Jenkins' matrix has no deny either. The
cost — that `team-a` developers cannot be kept out of `team-a/prod` approvals
while they hold `developer` on `team-a` — is real; the owner accepted it (§4
item 9): such jobs go in a sibling folder (`team-a-prod/`) instead.

### 5.3 Folder matching is segment-wise on the qualified name

Designer's call.

- Folder `F` applies to job `J` iff `J` starts with `F + "/"`. So `team-a`
  covers `team-a/build` and `team-a/sub/build`; it does **not** cover
  `team-ab/build`, and it does **not** cover a root-level job named exactly
  `team-a` (that job has no folder).
- A job's folder is a pure function of its name (`SplitQualifiedName`), so
  authorization never needs the job row and works identically for a run whose
  job row is mid-rename.
- Root-level jobs are covered by global grants only. There is no `""` / `/`
  folder key; use `global`.
- The check is always on the **resulting** qualified name: a caller with
  `job.configure` on `team-a` who applies a job whose `annotations.path` is
  `team-b` is checked against `team-b/<name>` and denied.

### 5.4 Config shape: legacy keys untouched, one new additive `oidc.permissions` block

Designer's call. `roleMap` / `userMap` / `defaultRole` keep their exact types
and semantics and still produce one **global role** per OIDC caller. A new
optional `oidc.permissions` block adds grants on top, by union:

```yaml
oidc:
  issuer:     https://idp.example.com
  clientId:   unified-cd
  rolesClaim: groups

  # Unchanged. Produces the caller's global role (bundle), resolved exactly as today.
  roleMap:
    "my-org:platform": admin
  userMap:
    oncall-lead@example.com: admin
  defaultRole: deny                     # nobody reads everything by default

  # New, optional, additive. Values are a role name, a permission name,
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

- **Matching** reuses identity resolution: `groups` keys are compared to the
  values of `rolesClaim`; `users` keys are compared to the token's `email` and
  `sub` (both, not first-hit — `users` is additive, unlike `userMap`).
- **A role name used as a grant** expands to its bundle (§7.3). In a folder
  entry only the bundle's folder-scopable permissions apply (`admin` on a
  folder means "every folder-scopable permission there"). This is documented,
  and it is not the anti-goal of §2 because a bundle on a folder can never
  reach a global permission.
- **Naming a global-only permission explicitly in a folder entry** is a
  startup error, because that is the anti-goal case: the operator believes it
  is scoped and it cannot be.
- **`defaultRole` interplay.** `defaultRole` stays a *global* role for users
  nothing in `roleMap`/`userMap` matches. To make folders private, an operator
  sets `defaultRole: deny` (or `viewer` only if everyone may read everything).
  Login is denied only when the legacy resolution denies **and** no
  `permissions` entry matches the identity; a user who matches only a folder
  grant can log in and sees only that folder. With no `permissions` block this
  reduces to exactly today's rule.
- **`userMap` demotion does not reach `permissions`.** A `userMap` entry
  replaces group matches *for the global role only*; grants from
  `permissions.global` / `permissions.folders` still union in. Documented.
- **No custom role definitions** in v1. YAML anchors cover reuse
  (`&team-dev [job.read, run.trigger]`); a `roles:` map can be added later
  without changing any existing key.
- **No env vars.** Like `roleMap`, the block is config-file only, and the
  configuration reference says so.
- **`oidc.permissions` without `oidc.issuer` is a startup error**: every
  grant matches an OIDC identity, so it would be dead config.

Startup validation (strict decode; the controller refuses to start). Exact
strings, so they can be grepped from logs and listed in troubleshooting:

| Condition | Error |
|---|---|
| unknown permission or role | `oidc.permissions.folders["team-a"].groups["my-org:team-a"]: unknown permission or role "jobs.read"` |
| global-only permission on a folder | `oidc.permissions.folders["team-a"].groups["my-org:team-a"]: permission "secret.write" is global-only and cannot be granted on a folder` |
| bad folder segment | `oidc.permissions.folders["team-a//x"]: invalid folder: segments must be non-empty and must not be "." or ".."` |
| wildcard in folder | `oidc.permissions.folders["team-*"]: invalid folder: folders are literal job-name prefixes, "*", "?", "[" are not allowed` |
| whitespace in folder | `oidc.permissions.folders["team a"]: invalid folder: whitespace is not allowed` |
| duplicate after normalisation | `oidc.permissions.folders: "team-a" and "/team-a/" are the same folder` |
| empty entry | `oidc.permissions.folders["team-a"]: no groups or users` |
| empty grant | `oidc.permissions.global.groups["my-org:x"]: empty grant` |
| unknown key | `oidc.permissions: unknown field "deny"` (strict decode) |
| no issuer | `oidc.permissions is set but oidc.issuer is not` |

Folder keys are normalised by trimming leading/trailing `/`. Segments are
otherwise not DNS-validated, because AppSource-derived folders (directory
names) are not either (§3).

**Legacy maps.** Today a `roleMap` value that is not a known role (a typo
such as `develper`) is silently never matched. Designer's call: keep booting
but log a warning (`oidc.roleMap["my-org:devs"]: unknown role "develper" is
ignored (valid: admin, developer, viewer)`) when there is no `permissions`
block, and make it a startup error when there is one — at that point the
operator is actively editing authorization and failing loudly is cheap; before
that, refusing to boot on upgrade would violate the first anti-goal.

### 5.5 PATs: owner snapshot ∩ ceiling, evaluated per request

Designer's call. This is the PAT rule.

- **At creation** the PAT records its **owner**: the creating principal's
  `sub`, `email` and the `rolesClaim` values it presented (a snapshot), plus an
  optional **ceiling**: either a role (`role`, as today) or a list of
  permissions (`permissions`). No ceiling means "everything the owner has".
- **At use** the PAT's effective permissions are
  `matrix(owner snapshot, current config) ∩ ceiling`, recomputed on every
  request. So:
  - a PAT can never exceed its owner, globally or on any folder;
  - removing the owner from `roleMap`/`userMap`/`permissions` in config
    takes effect for all their PATs at the next controller start, with no
    re-issue;
  - group changes in the IdP are seen when the owner next logs in through
    the browser: an OIDC login refreshes the group snapshot on every PAT whose
    owner `sub` matches (bounded staleness, the same class as sessions today).
- **Creation checks.** `token.create` is required, the creator's identity
  must be known (§5.14), and the ceiling must lie within the owner's grants
  (§5.15). Both are refusals with a `4xx`, never a silent clamp.
- **PAT created by a PAT** inherits the parent's owner snapshot; its ceiling is
  the parent's ceiling ∩ the requested ceiling.
- **Role-only PATs.** A PAT with no recorded owner — every PAT created before
  this change, and any PAT created by the break-glass token or by another
  role-only PAT — is evaluated as today: the global bundle of `pats.role`.
  This is what makes the upgrade non-widening and non-narrowing for existing
  tokens.
- **Break-glass** short-circuits to all 24 permissions globally, before any of
  the above, exactly where `ServerAuth` short-circuits today.
- A PAT whose owner now resolves to nothing authenticates but gets `403
  forbidden: token owner has no permissions` on every route, so the failure is
  diagnosable rather than looking like a bad token.

Why not "a PAT carries a role bundle, clamped at creation" (today's model
extended)? Because a role is global and a folder-only user has no global role:
the PAT would either be unusable or would have to be granted a global role the
owner does not have. Why not "a PAT stores a frozen copy of the owner's
effective permissions"? Because then removing a team from config would leave
its PATs working until each is found and revoked; evaluating against the
current config is what the operator expects when they edit the file.

### 5.6 Token management becomes per-owner (one deliberate narrowing)

Designer's proposal, confirmed by the owner (§4 item 11). Today any developer can list
and delete every PAT, including admins' and the bootstrap row. With owners
recorded:

- `token.create` (in `developer`) — create PATs, list and delete **own** PATs.
- `token.manage` (in `admin`) — list and delete **everyone's** PATs.

Together with the PAT-creation refusals of §5.14 / §5.15, this is where a
deployment that changes no config sees different behavior: a developer stops seeing other people's tokens, and can no
longer delete role-only (pre-upgrade) PATs, which have no owner. It is a
narrowing, never a widening, and it closes a hole the matrix would otherwise
make worse (a folder-limited developer could revoke an admin's automation
token). It is listed in the migration guide.

### 5.7 Unreadable is 404; readable-but-not-allowed is 403

Designer's call. If the caller lacks `job.read` on the job a request targets
(by job name or via a run ID), the response is the same `404` a missing job or
run gets, so the API does not confirm existence of hidden jobs through run
IDs. If the caller can read it but lacks the action, the response is `403
forbidden: missing permission "run.trigger" on job "team-a/build"` — naming
the permission makes misconfiguration self-diagnosing and reveals nothing the
caller cannot already read. Global-only routes keep `403 forbidden: missing
permission "secret.write"`.

Job *names* are not treated as confidential. They remain visible in global
resources (WebhookReceiver `trigger.job`, AppSource managed-resource lists,
Schedules are filtered — see §7.4). What is hidden is job contents, runs and
logs. The authorization page says this explicitly.

### 5.8 Authorization runs before the managed-resource guard

Designer's call. On `POST`/`DELETE /jobs` the permission check runs first and
`guardManagedResource` second, so a caller without `job.configure` gets a
permission error rather than a message naming the managing AppSource and its
repo URL.

### 5.9 Job code is not confined; non-human triggers act as the system

Designer's call, confirmed by the owner (§4 items 8 and 13). Folder permissions decide
what a human may do through the API and UI. They do not change what a job,
once it runs, may reach:

- **Secrets** stay global names; a run may fetch any secret its spec declares
  (the existing fetch fence). Therefore `job.configure` on *any* folder is, in
  practice, the ability to read *any* secret value, exactly as `developer` is
  today. The authorization page states this in a warning box, so nobody
  mistakes folders for a secret boundary. A per-folder secret allow-list
  ("jobs under `team-a/` may only declare `TEAM_A_*`") would be enforceable at
  run creation (one choke point, `createRunFromJob`, covers trigger, replay,
  schedule, webhook and `call:`), but it is a confinement-of-job-code feature
  and it changes the "secrets are global" contract; the owner made it a
  non-goal.
- **`call:`** child runs are created by the agent principal (`agent:<id>`),
  with no human in the request, and may target any job. An apply-time check
  would be incomplete (the call can come from a Git `uses:` template, which
  the controller never sees at apply time), so v1 adds none.
- **AppSource-synced jobs** are the repository writers' decision (§4 item
  13). The reconciler writes them as the system, into whatever folders the
  repository layout produces; no folder grant is consulted.
- **Schedules, WebhookReceivers and AppSource syncs** fire as the system. The
  authorization happens when the trigger is *configured*: creating or
  re-targeting a schedule needs `schedule.configure` on the target job's
  folder (§7.4); WebhookReceivers and AppSources are global, admin-bundle
  resources.
- **`uses:` templates** are edited in Git, governed by the Git host's
  permissions, and pinned by `@tag`/`@sha` in callers. No unified-cd
  permission covers them, and none can: the controller has no write path for
  them. GitCredentials (which grant the controller read access to template
  repos) stay global `gitcredential.*`.
- **Agent principals** are untouched. Agent-or-human routes keep the agent
  bypass; only the human branch is checked against permissions.

### 5.10 Sessions and OIDC bearers carry claims, not a role

Designer's call. A session stores the `rolesClaim` values (new nullable
column) captured at login and updated on silent refresh when the refreshed
id_token carries them; the grant set is computed from the stored claims and
the current config on every request (cheap: a map lookup per claim value per
ancestor folder). A session created before the upgrade has no stored claims
and is evaluated as role-only from `sessions.role`, i.e. exactly as today; it
expires within the 24 h session lifetime. An OIDC bearer (CLI device flow)
already presents its claims on every request.

### 5.11 The Web UI gets the caller's grants from `/api/v1/auth/me`

Designer's call. `GET /api/v1/auth/me` additionally returns the caller's
effective grants — only the caller's, never the config:

```json
{
  "name": "dev@example.com",
  "kind": "session",
  "role": "",
  "permissions": {
    "global":  ["agent.read", "appsource.read", "token.create", "vars.read", "webhook.read"],
    "folders": { "team-a": ["job.configure", "job.delete", "job.read", "run.approve",
                            "run.cancel", "run.delete", "run.replay", "run.trigger",
                            "schedule.configure"] }
  }
}
```

The UI evaluates `can(permission, jobName)` with the same union/prefix rule
(§5.2, §5.3). Rejected alternative: per-resource capability flags on every job
and run response — they touch every handler and every list, and still need a
global answer for the navigation. To stop the JS rule drifting from the Go
rule (anti-goal), both sides are tested against one shared fixture file of
`(grants, permission, jobName) → bool` cases (§9). Folder keys appear only if a
grant there matched the caller, so the response does not enumerate other
teams' folders.

### 5.12 Audit records the permission and the deciding grant

Designer's call. Each audited row gains `permission` (e.g. `run.trigger`),
`scope` (the job name for folder-scoped routes, empty for global) and
`grantedBy` — a short, stable description of the first grant that allowed it
(`role:developer (roleMap["my-org:devs"])`, `folder:team-a
(groups["my-org:team-a"])`, `pat-ceiling`, `break-glass`) or empty on a deny.
The allow-list nature and the secret-body special case are unchanged; the
"no GETs" rule is relaxed to record denied reads only (§5.16 rule 6). `run.replay` is classified at the same time (it is TODO item 24 and
the route table touches it anyway).

### 5.13 Route classification lives in one table

Designer's call. Human routes move to a declarative table analogous to
`agentRouteIdentityMatrix`: `{method, pattern, permission, scope}` where scope
is `global` or a job resolver (`jobFromBody`, `jobFromPath`, `jobFromRunID`,
`jobFromScheduleBody`, `jobFromSchedulePath`, `filtered`). Middleware does a
cheap "holds this permission anywhere" pre-check (so a caller with no
`run.trigger` at all is refused before the body is read); the exact per-job
check happens where the job name becomes known. Every non-human route is also
listed, as `public`, `self` (authenticates itself, e.g. `/auth/me`,
`/agents/enroll`, `/store-credentials`, `/webhook/{name}`) or `agent`, so the
classification test (§9) can be exhaustive in both directions. `requireMinRole`
and `viewerOrAgent` are retired in favour of `requirePermission` and
`requirePermissionOrAgent`.

### 5.14 A PAT is not created from an unknown identity

Designer's call. A PAT's owner snapshot is only as good as the claims the
creator presented. Two creators present none we can trust:

- a **pre-upgrade session** (`sessions.claims IS NULL`), whose groups were
  never stored;
- an **OIDC bearer** (CLI device flow) whose id_token has no `rolesClaim`
  claim at all, while the config contains group-keyed grants (any `roleMap`
  entry or any `permissions.*.groups` entry). Typical cause: the device flow
  did not request the scope that makes the IdP emit groups. When the config
  has no group-keyed grants, groups cannot affect any decision, so an absent
  claim is recorded as the empty set — it is then exact, not a guess — and
  userMap-only setups (for example local Dex static users, which have no
  groups) keep creating PATs.

For these, `POST /api/v1/tokens` is refused with `403` and no PAT is written:

- session: `cannot create a token: this login predates group-aware tokens; log out and log in again, then retry`
- bearer: `cannot create a token: your OIDC token carries no "groups" claim; log in again so the token includes it, then retry`
  (the claim name is the configured `rolesClaim`).

Rejected alternatives: (a) create a role-only PAT from `sessions.role` — that
is a PAT which bypasses folder grants for as long as it lives, i.e. a widening
the moment the operator introduces folders; (b) record the owner with an empty
claim set — that silently narrows the PAT to the owner's `userMap`/default
grants, which looks like a broken token rather than a stale login.

Rollout window: pre-upgrade sessions last at most 24 h (`sessionDuration`),
and silent refresh stores claims when the refreshed id_token carries them, so
the session case disappears within 24 h of the upgrade without any action;
users who hit it sooner log in again. The bearer case is a configuration issue
of the IdP/device-flow scopes and is listed in troubleshooting with the exact
string. PAT creation by break-glass and by role-only PATs is unaffected (it
already has no claims and produces role-only PATs, §5.5); those creators are
admins or pre-upgrade tokens by construction.

### 5.15 Folder-only principals, empty roles, and what `ServerAuth` admits

Designer's call. A user matched only by folder grants has an empty global
role. That must be representable end to end without reopening "unknown or
empty role means denied" for principals that have no grants.

**Storage.** `pats.role` and `sessions.role` stay `NOT NULL`, and may be `''`
when the row carries an owner (`pats.owner_sub IS NOT NULL`) or claims
(`sessions.claims IS NOT NULL`). The migration **drops the `DEFAULT 'admin'`**
on both columns (added by `002_add_role`): every insert already passes the role
explicitly, and a default of admin is the wrong failure mode for a row whose
writer forgot it. Existing rows are untouched. `''` on a row without owner or
claims is never written by new code.

**Ceiling rules** (all `400`, no PAT written):

- legacy `role` above the creator's global role:
  `cannot grant role "developer": it exceeds your global role "viewer"; use --permission to grant folder permissions you hold`
  (for an empty global role the tail reads `it exceeds your global role (none)`).
  This replaces today's silent clamp for every creator. Silent clamping hands
  the caller a token weaker than they asked for and lets their script discover
  it later at a 403; once folder-only users exist it would clamp every request
  to nothing.
- a `permissions` ceiling must be a subset of the owner's effective grants,
  i.e. every entry is held globally or on at least one folder:
  `cannot grant permission "secret.write": you do not hold it`.
- for a PAT created by a PAT, the bound is the parent's effective grants
  (owner ∩ parent ceiling).

**`ServerAuth` changes** (`internal/controller/auth.go`):

- **PAT branch (line 80, `role := pat.Role`).** After the break-glass
  short-circuit: if the PAT has an owner, build the principal's grants from
  the owner snapshot and the current config, intersected with the ceiling, and
  pass it on **whatever `pat.Role` is, including `''`**. If it has no owner
  (role-only), keep today's semantics: `roleRank(pat.Role) == 0` is rejected
  in `ServerAuth` with `403 forbidden: no role` (today the same token is
  refused later by `requireMinRole`; moving the refusal up is needed because
  `requireMinRole` goes away).
- **Session branches (lines 133 and 141, `Role: sess.Role`)** — both the
  refresh and the normal path: if `sess.Claims != nil`, build grants from the
  stored claims and the session's email/sub and pass the principal on even
  when `sess.Role == ''`. If `sess.Claims == nil` (pre-upgrade), keep
  role-only semantics and reject `roleRank(sess.Role) == 0` with `403
  forbidden: no role`.
- **OIDC bearer branch (line 100) and browser login (`auth_oidc.go:236`).**
  `resolveRole` failing no longer refuses on its own: the caller is refused
  with the existing `forbidden: no role` / `forbidden: no role assigned` only
  when the legacy resolution denies **and** no `permissions` entry matches
  (§5.4). Otherwise the principal carries role `''` and its grants.
- An admitted principal whose grant set is empty (owner or claims present,
  but nothing in the current config matches) reaches the permission check and
  gets `403 forbidden: token owner has no permissions` (PAT) or `403
  forbidden: no permissions` (session/bearer) on every route.

### 5.16 Web UI enforcement: the server decides, the UI only reflects

Records the owner's emphasis (§4 item 15); the mechanisms are designer's calls.

**Inventory.** The UI's state-changing calls today are: trigger
(`JobRun.svelte`, `POST /runs`), rebuild (`RunDetail.svelte`, also `POST /runs`
with the run's params — a trigger, not a replay), cancel (`POST
/runs/{id}/cancel`), approve/reject (`POST /runs/{runID}/approvals/{i}`),
AppSource sync (`POST /appsources/{name}/sync`), and token create/delete
(`TokenList.svelte`). The UI has no job edit or delete action. Every one of
these is a row of the route table (§7.2) and is decided by the server like any
API call; the inventory is kept as a test fixture so that a new UI action
without a classified route fails a test.

**Rules.**

1. **Server-side first.** No action is protected only by hiding. A hidden
   control that is called anyway (devtools, stale page, crafted request) gets
   the same `403`/`404` as the CLI would. The UI renders that response as an
   error and does not retry.
2. **Hide or disable from `/auth/me`.** The page loads the caller's grants
   once (§5.11) and evaluates `can(permission, jobName)` per control. Rule of
   thumb: a control for an action the caller could never take anywhere is
   **hidden** (navigation to Secrets, GitCredentials, AppSources, Webhooks,
   Vars, audit; the token page's "all tokens" view); a per-job control the
   caller lacks on *this* job is **disabled** with a tooltip naming the
   permission (`requires run.approve on team-a/prod`), so a user who expects
   the button can tell why it is greyed out instead of filing a bug. The
   token page offers only ceilings the caller holds.
3. **One rule, tested on both sides.** `can()` in `web/src/lib` and the Go
   evaluator are tested against the same fixture file (§9); the fixture is
   the spec of the prefix/union rule.
4. **Unreadable means absent.** Job lists, the active-runs view, agent run
   history and schedule lists are rendered from server responses that are
   already filtered (§7.4), so unreadable jobs and runs never reach the
   browser. A direct link to an unreadable job or run gets `404` and renders
   the ordinary not-found view. SSE: `GET /runs/{id}/events` is per run and
   refuses at connect (`404`); there is no multi-run stream today, and any
   future one must filter per event with the same evaluator — the route table
   entry for such a stream is `filtered`, which the classification test
   requires to have a per-item filter test.
5. **Stale grants.** Grants change only at controller restart or re-login.
   On any `403` the UI re-fetches `/auth/me` once and re-renders, so a page
   left open across a config change converges without a manual reload.
6. **Every decision audited.** Designer's call on what "every" means: every
   state-changing request is recorded, allowed or denied, with `permission`,
   `scope` and `grantedBy` (§5.12) — the UI's actions are all
   state-changing, so all are covered. Additionally, every **denied read**
   (a `403`, or a `404` caused by missing `job.read` — recorded as a deny even
   though the client saw `404`) is recorded. Allowed reads are not: log
   tailing and list polling would flood the table and the audit trail would
   stop being readable, which defeats it.

### 5.17 AppSource-managed jobs edited through the API or UI

Investigated, and the code already does the right thing; no new rule is
needed. Designer's call only on ordering (§5.8) and wording below.

- `handleApplyJob` and `handleDeleteJob` (`internal/controller/api_jobs.go`)
  call `guardManagedResource` (`managed_guard.go`) on the qualified name. If
  an AppSource lists the job in its managed resources and its
  `syncPolicy.allowManualOverride` is not `true`, the write is refused with
  `409`:
  `resource Job "team-a/build" is managed by AppSource "pipelines"; update it in Git (<repoURL with credentials redacted>), or set syncPolicy.allowManualOverride: true on the AppSource`.
  The guard fails closed on a store error. The same guard covers Schedules,
  WebhookReceivers, GitCredentials, Vars and AppSources.
- With `allowManualOverride: true` an edit is accepted and is overwritten on
  the next sync that changes the file (the reconciler upserts unconditionally)
  — that is the documented incident escape hatch, not a gap.
- The Web UI has no job edit or delete control, so the UI path does not
  exist; run-level actions (trigger, cancel, approve) on managed jobs are not
  edits and are governed only by folder permissions.
- With this design: the permission check runs first (§5.8), so a caller
  without `job.configure`/`job.delete` on that folder gets `403`/`404`, never
  the repo URL; a caller with it gets the existing `409`. Should a job edit
  control ever be added to the UI, it must be disabled for managed jobs with
  the same message (the job response would then need a `managedBy` field);
  that is noted for whoever adds it, not built here.

## 6. Open questions

None. The seven questions of the first draft were answered by the owner and
are recorded in §4 items 8–14.

## 7. The design

### 7.1 Permission catalogue

**Folder-scopable (9)** — granted globally or on a folder; checked against
the job a request targets.

| Permission | Meaning |
|---|---|
| `job.read` | See a job's definition, YAML and schedulability, and its runs: run detail, YAML snapshot, steps, logs (tail, archive, range, search, stats), SSE events, outputs, approval list, artifacts (list, download). Also governs which jobs/runs appear in lists and which schedules are listed. |
| `job.configure` | Create or update a job (apply). |
| `job.delete` | Delete a job. Cascades to its run history and schedules. |
| `run.trigger` | Start a run of a job. |
| `run.replay` | Re-run a run's snapshot spec. |
| `run.cancel` | Cancel a run. |
| `run.delete` | Delete a run. |
| `run.approve` | Approve or reject a waiting approval step. |
| `schedule.configure` | Create, re-target or delete a schedule targeting the job. |

**Global-only (15)**

| Permission | Meaning |
|---|---|
| `secret.list` | List secret names (never values). |
| `secret.write` | Set or delete a secret. |
| `gitcredential.read` | List GitCredentials. |
| `gitcredential.write` | Create, update or delete a GitCredential. |
| `vars.read` | List `Vars` manifests (plain-text values). |
| `vars.write` | Apply or delete a `Vars` manifest. |
| `webhook.read` | List WebhookReceivers. |
| `webhook.write` | Apply or delete a WebhookReceiver. |
| `appsource.read` | List and get AppSources. |
| `appsource.write` | Apply, delete or force-sync an AppSource. |
| `agent.read` | List and get agents, an agent's recent runs (filtered by `job.read`), enrollments, enrollment policies and agent identities. |
| `agent.manage` | Create/revoke enrollments; create/update/delete enrollment policies; enable/disable identities; revoke agent credentials. |
| `token.create` | Create PATs (≤ own permissions); list and delete own PATs. |
| `token.manage` | List and delete every PAT. |
| `audit.read` | Read the audit log. |

### 7.2 Route → permission

Every route registered in `server.go` (including `agentRouteIdentityMatrix`).
"Today" is the current minimum role.

**Human (and agent-or-human) routes**

| Method | Route | Today | Permission | Scope |
|---|---|---|---|---|
| GET | `/api/v1/audit` | admin | `audit.read` | global |
| POST | `/api/v1/jobs` | developer | `job.configure` | job from body (qualified name after `annotations.path`) |
| GET | `/api/v1/jobs` | viewer | `job.read` | filtered |
| GET | `/api/v1/jobs/*` (job, `/yaml`, `/schedulability`) | viewer | `job.read` | job from path |
| DELETE | `/api/v1/jobs/*` | developer | `job.delete` | job from path |
| POST | `/api/v1/runs` | developer | `run.trigger` | job from body `jobName` |
| POST | `/api/v1/runs/{id}/replay` | developer | `run.replay` | job from run |
| GET | `/api/v1/runs?jobName=` | viewer | `job.read` | job from query |
| GET | `/api/v1/runs/active` | viewer | `job.read` | filtered |
| GET | `/api/v1/runs/{id}` | viewer or agent | `job.read` (agent bypass) | job from run |
| GET | `/api/v1/runs/{id}/outputs` | viewer or agent | `job.read` (agent bypass) | job from run |
| GET | `/api/v1/runs/{id}/yaml` | viewer | `job.read` | job from run |
| POST | `/api/v1/runs/{id}/cancel` | developer | `run.cancel` | job from run |
| DELETE | `/api/v1/runs/{id}` | developer | `run.delete` | job from run |
| GET | `/api/v1/runs/{id}/events` (SSE) | viewer | `job.read` | job from run, checked at connect |
| GET | `/api/v1/runs/{id}/logs` | viewer | `job.read` | job from run |
| GET | `/api/v1/runs/{id}/steps` | viewer | `job.read` | job from run |
| GET | `/api/v1/runs/{id}/logs/archive` | viewer | `job.read` | job from run |
| GET | `/api/v1/runs/{id}/logs/stats` | viewer | `job.read` | job from run |
| GET | `/api/v1/runs/{id}/logs/range` | viewer | `job.read` | job from run |
| GET | `/api/v1/runs/{id}/logs/search` | viewer | `job.read` | job from run |
| GET | `/api/v1/runs/{runID}/approvals` | viewer | `job.read` | job from run |
| POST | `/api/v1/runs/{runID}/approvals/{stepIndex}` | developer | `run.approve` | job from run, at decision time |
| GET | `/api/v1/runs/{runID}/artifacts` | any human or agent | `job.read` (agent bypass) | job from run |
| GET | `/api/v1/runs/{runID}/artifacts/{name}` | any human or agent | `job.read` (agent bypass) | job from run |
| POST | `/api/v1/secrets` | admin | `secret.write` | global |
| GET | `/api/v1/secrets` | developer | `secret.list` | global |
| DELETE | `/api/v1/secrets/{name}` | admin | `secret.write` | global |
| POST | `/api/v1/gitcredentials` | admin | `gitcredential.write` | global |
| GET | `/api/v1/gitcredentials` | admin | `gitcredential.read` | global |
| DELETE | `/api/v1/gitcredentials/{name}` | admin | `gitcredential.write` | global |
| POST | `/api/v1/tokens` | developer | `token.create` | global |
| GET | `/api/v1/tokens` | developer | `token.create` (own) / `token.manage` (all) | global |
| DELETE | `/api/v1/tokens/{id}` | developer | `token.create` (own) / `token.manage` (any) | global |
| POST | `/api/v1/agent-enrollments` | admin | `agent.manage` | global |
| GET | `/api/v1/agent-enrollments` | viewer | `agent.read` | global |
| DELETE | `/api/v1/agent-enrollments/{id}` | admin | `agent.manage` | global |
| POST | `/api/v1/agent-enrollment-policies` | admin | `agent.manage` | global |
| GET | `/api/v1/agent-enrollment-policies` | viewer | `agent.read` | global |
| GET | `/api/v1/agent-enrollment-policies/{name}` | viewer | `agent.read` | global |
| PUT | `/api/v1/agent-enrollment-policies/{name}` | admin | `agent.manage` | global |
| DELETE | `/api/v1/agent-enrollment-policies/{name}` | admin | `agent.manage` | global |
| GET | `/api/v1/agent-identities/{agentId}` | viewer | `agent.read` | global |
| POST | `/api/v1/agent-identities/{agentId}/enable` | admin | `agent.manage` | global |
| POST | `/api/v1/agent-identities/{agentId}/disable` | admin | `agent.manage` | global |
| POST | `/api/v1/agent-identities/{agentId}/credentials/revoke` | admin | `agent.manage` | global |
| GET | `/api/v1/agents` | viewer | `agent.read` | global |
| GET | `/api/v1/agents/{agentId}` | viewer or agent | `agent.read` (agent bypass, path-bound) | global |
| GET | `/api/v1/agents/{agentId}/runs` | viewer or agent | `agent.read` (agent bypass, path-bound); rows filtered by `job.read` | global + filtered |
| POST | `/api/v1/webhooks` | admin | `webhook.write` | global |
| GET | `/api/v1/webhooks` | viewer | `webhook.read` | global |
| DELETE | `/api/v1/webhooks/{name}` | admin | `webhook.write` | global |
| POST | `/api/v1/vars` | admin | `vars.write` | global |
| GET | `/api/v1/vars` | viewer | `vars.read` | global |
| DELETE | `/api/v1/vars/{name}` | admin | `vars.write` | global |
| POST | `/api/v1/schedules` | developer | `schedule.configure` | job from body `spec.job`, **and** the existing schedule's current target if the name exists |
| GET | `/api/v1/schedules` | viewer | `job.read` | filtered by target job |
| DELETE | `/api/v1/schedules/{name}` | developer | `schedule.configure` | the schedule's current target job |
| POST | `/api/v1/appsources` | admin | `appsource.write` | global |
| GET | `/api/v1/appsources` | viewer | `appsource.read` | global |
| GET | `/api/v1/appsources/{name}` | viewer | `appsource.read` | global |
| DELETE | `/api/v1/appsources/{name}` | admin | `appsource.write` | global |
| POST | `/api/v1/appsources/{name}/sync` | admin | `appsource.write` | global |

The legacy-equivalence test (§9) proves that, with no `permissions` block,
each row's decision for each role is unchanged — except `GET`/`DELETE
/api/v1/tokens` (§5.6) and `POST /api/v1/tokens`, which now refuses an
over-high `role` (§5.15) and a creator without known claims (§5.14) instead of
clamping. The artifact rows move from "any authenticated human"
to `job.read`, which every legacy role holds globally, so they are unchanged
too.

**Public and self-authenticating routes** (classified, no permission):
`GET /healthz`, `GET /readyz`, `GET /metrics`, `GET /api/v1/auth/oidc-config`,
`GET /api/v1/ui-config`, `GET /api/v1/auth/oidc-login`,
`GET /api/v1/auth/oidc-callback`, `POST /api/v1/auth/logout`,
`GET /api/v1/auth/me` (self), `/dex`, `/dex/*`, `/device/callback`,
`POST /webhook/{name}` (HMAC/token), `POST /api/v1/agents/enroll`,
`POST /api/v1/agents/token/refresh`, `POST /api/v1/store-credentials`
(TokenReview), `/ui`, `/ui/*`, `GET /`.

**Agent-only routes** (classified `agent`, unchanged): `POST
/api/v1/agents/register` and every `agentAuth` entry of
`agentRouteIdentityMatrix` — heartbeat, deregister, claim, steps, logs,
reconcile, finish, children (`call:`), step outputs, run outputs, bulk logs,
sidecars, secrets fetch, approval create/get, and `PUT
/api/v1/runs/{runID}/artifacts/{name}`.

### 7.3 Role bundles

| Bundle | Permissions |
|---|---|
| `viewer` | `job.read`, `vars.read`, `webhook.read`, `appsource.read`, `agent.read` |
| `developer` | `viewer` + `job.configure`, `job.delete`, `run.trigger`, `run.replay`, `run.cancel`, `run.delete`, `run.approve`, `schedule.configure`, `secret.list`, `token.create` |
| `admin` | all 24 |

A role resolved by `roleMap` / `userMap` / `defaultRole`, a role-only PAT's
role, and a pre-upgrade session's role are global grants of these bundles. A
bundle granted on a folder contributes only its folder-scopable members.

### 7.4 Effects beyond the per-route check

- **Lists.** `GET /jobs`, `GET /runs/active`, `GET /agents/{id}/runs` and
  `GET /schedules` are filtered in the handler after the store query, using
  the caller's precomputed grant set (sizes are already bounded: run lists cap
  at 50, job and schedule lists are whole-table reads today). `GET
  /runs?jobName=` is a single check.
- **SSE.** `GET /runs/{id}/events` checks `job.read` at connect. Grants only
  change at controller restart, which ends every stream, so a per-event
  re-check buys nothing.
- **Schedules.** Applying a schedule whose name already exists checks
  `schedule.configure` on both the old and the new target, so a team cannot
  take over another team's schedule by reusing its global name.
- **Web UI.** §5.16.
- **CLI.** `unified-cli token create` gains a repeatable
  `--permission <name>` alongside `--role`. Error messages are the server's
  (§5.7), so no CLI logic changes.
- **Audit.** §5.12.

### 7.5 Data changes

One forward migration, no backfill:

- `pats`: `owner_sub text NULL`, `owner_email text NULL`,
  `owner_claims text[] NULL`, `permissions text[] NULL` (ceiling list; `NULL`
  means the `role` column is the ceiling, or no ceiling when an owner exists
  and `role` is empty). `owner_sub IS NULL` ⇔ role-only PAT.
- `sessions`: `claims text[] NULL` (`NULL` ⇔ pre-upgrade, role-only).
- `pats.role`, `sessions.role`: `DROP DEFAULT` (stay `NOT NULL`; `''` allowed
  only alongside an owner / claims, §5.15).
- `audit_logs`: `permission text NULL`, `scope text NULL`,
  `granted_by text NULL`.

`Principal` gains a precomputed grant set (`global` set, plus a map of the
folders whose grants matched); `Principal.Role` remains as the legacy global
role and may be empty.

## 8. Edge cases

| Case | Behavior |
|---|---|
| AppSource moves a job file `team-a/x.yaml` → `team-b/x.yaml` | A new job `team-b/x` is upserted; `team-a/x` is pruned (`prune: true`, cascading its runs and schedules — existing behavior) or left behind. Permissions follow the names: old runs stay under `team-a`'s grants. Nothing new to authorize; the sync is a system action. |
| AppSource legacy upgrade `build` → `team-a/build` (`RenameJob`) | Runs are repointed to `team-a/build`, so they move from global-only visibility to global ∪ `team-a`. That is a widening for `team-a` grantees only, and only for jobs that are `team-a`'s — intended. |
| Runs of a deleted job | Do not exist (FK cascade). The run→job helper resolves the folder from `runs.job_name` text and never needs the job row, so a concurrent delete yields 404, not a panic or an unscoped allow. |
| Approval of a run whose job moved | `run.approve` is checked at decision time against the run's current `job_name`. For a rename that is the new folder; for an AppSource move the run belongs to the old job and stays under the old folder. |
| Replay | `run.replay` on the source run's job (current `job_name`). The replayed run is created under that same job. |
| Direct apply into another folder via `annotations.path` | Checked on the resulting qualified name (§5.3). |
| Direct apply or delete of an AppSource-managed job (API/CLI) | Permission check first, then the existing managed-resource guard: `409` pointing to the AppSource and repo, unless `allowManualOverride` (§5.8, §5.17). |
| Someone pushes a job into another team's folder via an AppSource repo | Synced; not a unified-cd decision (§4 item 13). Visible to that folder's readers like any other job. |
| Schedule fires / webhook fires / AppSource syncs | System principal; no permission check at fire time. The configuring action was checked (§5.9, §7.4). |
| `call:` child run | Created by the agent principal; unaffected (§5.9). The child run's visibility follows the child job's folder, so a team may see its parent run but not the child. The parent run's step shows the child run ID; following it returns 404 for a caller without `job.read` there. |
| `uses:` template edited in Git | Outside unified-cd authorization (§5.9). |
| Agent principal on agent-or-human routes | Bypass unchanged; path identity binding unchanged. |
| Break-glass | All permissions, globally, before any lookup. |
| PAT requested from a pre-upgrade session (`claims IS NULL`) | `403 cannot create a token: this login predates group-aware tokens; ...`; no row written. Gone within 24 h of upgrade (§5.14). |
| PAT requested by an OIDC bearer with no `rolesClaim` claim, config has group-keyed grants | `403 cannot create a token: your OIDC token carries no "groups" claim; ...`; no row written. |
| Same, but config has no group-keyed grants (userMap-only) | PAT created; owner claims recorded as empty (exact). |
| Folder-only user creates a PAT with no ceiling or a `permissions` ceiling they hold | PAT created with `role = ''` and an owner; it authenticates and is evaluated by owner ∩ ceiling. |
| Folder-only user requests `--role developer` | `400 cannot grant role "developer": it exceeds your global role (none); use --permission ...`. |
| Viewer requests `--role admin` (no `permissions` block) | `400` naming the role — previously silently clamped to `viewer` (§5.15). |
| Ceiling permission held nowhere | `400 cannot grant permission "<p>": you do not hold it`. |
| Session with claims and `role = ''` | Admitted by `ServerAuth`; decided by the permission check. |
| Session without claims (pre-upgrade) and `role = ''`, or role-only PAT with `role = ''` | Rejected in `ServerAuth`: `403 forbidden: no role`. |
| Owner/claims present but nothing in current config matches | Admitted; every route `403 forbidden: token owner has no permissions` / `forbidden: no permissions`. |
| Folder-only user creates a PAT | Needs `token.create`, which is global-only: grant it in `permissions.global` (see the example in §5.4). Safe, because the PAT is bounded by the owner's grants. |
| User in no `roleMap` group, `defaultRole: deny`, matched only by a folder grant | Logs in; sees only that folder's jobs; global pages hidden. |
| OIDC not configured | `oidc.permissions` cannot be set (startup error); everything is PAT-based and role-only, exactly as today. |

## 9. Migration, rollout and tests

**Migration and rollout**

1. Upgrade with no config change. Behavior is identical except token
   management (§5.6) and PAT creation (an over-high `--role` is a `400`
   instead of a clamp; a pre-upgrade session must log in again, for at most
   24 h — §5.14, §5.15), which the migration guide lists with a before→after
   table and the exact error strings. Existing PATs and sessions are role-only.
2. Optionally add `permissions.global` grants (e.g. `token.create` for
   everyone) — purely additive.
3. To separate teams: move team groups out of `roleMap` (or demote them to no
   global role), set `defaultRole: deny`, add `permissions.folders` entries.
   Users re-login (sessions pick up claims); PATs created before step 1 stay
   role-only and keep their global role until revoked, so the guide says to
   re-issue team PATs after the switch. The controller logs at startup the
   number of role-only PATs with a role above `viewer` when folder grants are
   configured, so the operator knows what still bypasses folders.

**Tests**

- **Exhaustive classification (two-way).** Walk the router with `chi.Walk`
  (including mounted sub-routers) and assert every `(method, pattern)` is in
  the route table, and every table entry is registered — the same shape as
  `TestEveryDefaultImageIsClassified` in the host-security-context work. A new
  route fails with "`POST /api/v1/foo` has no permission: classify it in the
  route table". The `/jobs/*` sub-dispatch (job, `/yaml`, `/schedulability`)
  is enumerated explicitly.
- **Per-permission table test.** For every row: a principal holding exactly
  that permission globally is not refused; one holding every *other*
  permission is refused. For folder-scopable rows: a grant on `team-a`
  allows `team-a/x` and `team-a/sub/x` and refuses `team-b/x`, `team-ab/x` and
  root `x`; unreadable targets return 404, readable-but-not-allowed 403.
- **Legacy equivalence (golden).** A frozen table of today's `route → min
  role` (captured from the current `requireMinRole` registrations) is checked
  against the new evaluation for each of `viewer`/`developer`/`admin` with no
  `permissions` block. The only permitted differences are the token rows
  (§5.6, §5.14, §5.15), listed in the test by name.
- **Resolution.** `userMap` demotion still demotes the global role and does
  not suppress `permissions`; login denial only when both legacy and new
  grants are empty; every startup error in §5.4 by exact string.
- **PATs.** Ceiling ∩ owner; config removal reduces a PAT's access without
  re-issue; login refreshes owner claims; PAT-from-PAT (bound by the parent's
  effective grants); role-only PATs; break-glass.
- **PAT creation refusals (§5.14, §5.15).** Exact status and string for: a
  pre-upgrade session (`claims IS NULL`) → `403`, no row written; an OIDC
  bearer without the `rolesClaim` claim when group-keyed grants exist → `403`;
  the same bearer with a userMap-only config → created with empty owner
  claims; `--role` above the global role (including empty global role) →
  `400` naming the role; unheld `permissions` entry → `400`.
- **`ServerAuth` admission (§5.15).** Session with claims and `role ''` →
  admitted; session without claims and `role ''` → `403 forbidden: no role`;
  owner PAT with `role ''` → admitted; role-only PAT with `role ''` → `403
  forbidden: no role`; OIDC bearer denied by `resolveRole` but matched by a
  folder grant → admitted; admitted principal with an empty grant set → `403`
  `no permissions` on every route; the migration leaves no `DEFAULT` on
  `pats.role` / `sessions.role`.
- **Filtering.** Lists, SSE connect, artifacts, `agents/{id}/runs`, schedule
  list and re-target.
- **Audit.** Allowed and denied state-changing requests and denied reads
  (including a hidden-job `404`) record `permission`, `scope` and
  `grantedBy`; allowed reads are not recorded; `run.replay` is recorded.
- **UI (§5.16).** One shared fixture (`(grants, permission, jobName) →
  bool`) consumed by a Go test and a Vitest test of the client `can()`; a Go
  test that every action in the UI inventory is a classified route whose
  server check refuses a caller without the permission (no action is
  protected only by hiding); component tests that each control is hidden or
  disabled for a caller lacking its permission and shown otherwise; list views
  contain no unreadable job; a direct link to an unreadable run renders the
  not-found view; a `403` triggers one `/auth/me` re-fetch.
- **AppSource-managed jobs (§5.17).** API apply and delete of a managed job
  return the existing `409` string after the permission check passes; a
  caller without `job.configure` / `job.delete` gets `403`/`404` without the
  repo URL; with `allowManualOverride` the write succeeds.

## 10. Documentation to update with the implementation

- `docs/operator-manual/authorization.md` — replace the role matrix with the
  catalogue, bundles, `oidc.permissions` reference and examples, the "job
  names are not confidential", "folders are not a secret boundary" and
  "anyone who can push to an AppSource repository can place jobs in any
  folder" warnings, the sibling-folder recipe, resolution order including union and login rule, PAT rule. Also
  fixes the existing gaps (Vars, replay, run delete, artifacts are missing
  from today's matrix).
- `docs/operator-manual/authentication.md` — sessions store claims; PAT
  owners and ceilings.
- `docs/operator-manual/audit.md` — new fields; `run.replay` row.
- `docs/reference/configuration.md` — `oidc.permissions`, config-file only.
- `docs/reference/cli.md` — `token create --permission`; error messages.
- `docs/contributing/auth-implementation.md` — "Adding an endpoint" step 3
  becomes "add the route to the route table with a permission and scope";
  the `viewerOrAgent` row changes.
- `docs/contributing/invariants.md` — "every route is classified" invariant.
- `docs/troubleshooting/` — a new authorization entry (or a section in
  `controller-and-database.md`) for every startup error string and for
  `403 forbidden: missing permission ...` / unexpected 404.
- `docs/operator-manual/migrations/` — token-management narrowing, the
  PAT-creation refusals with their exact strings, the team-separation recipe.
- `examples/config/controller.yaml` — a commented `permissions` example.
- `README.md` — feature summary (fine-grained per-folder permissions).
- `docs/user-guide/resources/app-source.md` — managed-resource protection
  section: folder grants do not apply to synced jobs; the Git host governs
  who can write them.
- `docs/contributing/frontend-development.md` — the `can()` helper, the shared
  fixture, hide-vs-disable, and the rule that a UI control is never the only
  enforcement.
- `TODO.md` — item 24 (replay audit) is closed by this work.
