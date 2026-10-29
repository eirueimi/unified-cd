# Migrating to matrix authorization

unified-cd now authorizes every human caller with fine-grained **permissions**, and the three roles
(`admin`, `developer`, `viewer`) became named bundles of them. A deployment that changes no
configuration keeps its behavior: no existing PAT, session or user gains a permission, and a config
with only `roleMap` / `userMap` / `defaultRole` (or no OIDC at all) needs no edit.

A few things do change on upgrade, deliberately. Each one is a narrowing or a refusal that replaces
a silent behavior, and each is listed below with the exact text you will see. The new capability
(per-folder grants for separating teams) is opt-in and is described in
[Authorization](../authorization.md).

## What changed

| # | Before | After |
|---|---|---|
| 1 | Any `developer` could list **every** PAT (`token list`, `GET /api/v1/tokens`) and delete any of them, including an admin's automation token and the `env:UNIFIED_TOKEN` row. | `token.create` (in `developer`) lists and deletes only **your own** PATs. `token.manage` (admin only) lists and deletes everyone's. A PAT created before the upgrade has no owner, so a developer no longer sees or deletes it. Deleting an id you may not touch answers `404 token not found`. |
| 2 | `--role` above your own role was silently **clamped** to your role: the token was weaker than you asked for, and your script found out later at a `403`. | It is refused, nothing is written: `400 cannot grant role "developer": it exceeds your global role "viewer"; use --permission to grant folder permissions you hold`. For a user who holds only folder grants the tail reads `it exceeds your global role (none)`. |
| 3 | A browser session created before the upgrade could create PATs. | Until the user logs in again, creating a PAT from that session is refused: `403 cannot create a token: this login predates group-aware tokens; log out and log in again, then retry`. Pre-upgrade sessions last at most 24 hours, so this disappears by itself. |
| 4 | A CLI login (device flow) could create PATs whatever its id_token carried. | If the id_token has no roles claim (`oidc.rolesClaim`, default `groups`) **and** the config has any group-keyed grant (any `roleMap` entry or any `permissions.*.groups` entry), creation is refused: `403 cannot create a token: your OIDC token carries no "groups" claim; log in again so the token includes it, then retry`. With a config that has no group-keyed grants (for example `userMap` only), creation still works and the token records no groups. The controller now advertises the roles-claim scope to the CLI, so a current `unified-cli login` requests it; a CLI built before this change does not, so upgrade the CLI and log in again. |
| 5 | A PAT whose identity has no `sub` could be created. | `403 cannot create a token: your identity has no subject (sub); log in again, then retry`. |
| 6 | A browser session stored a **role** fixed at login. | A session stores the caller's group claim values and recomputes its permissions from them and the **current** config on every request. Editing `roleMap` / `userMap` now affects live sessions after a controller restart, without a re-login. (Sessions created before the upgrade stay role-only until they expire.) |
| 7 | `GET /api/v1/auth/me` was open and answered only for a PAT (with the PAT name as `sub`) or a session (`sub`, `email`); an OIDC bearer got `401`. | It runs behind the normal authentication and answers for any authenticated caller with `name`, `kind`, `role`, `permissions` (`global` and `folders`) and, when known, `sub` and `email`. A role-only PAT or pre-upgrade session whose role is empty now gets `403 forbidden: no role` instead of a `200`. The Web UI uses it to hide or disable controls. |
| 8 | Artifact list and download (`GET /api/v1/runs/{runID}/artifacts[/{name}]`) were open to any authenticated human, whatever the role. | They require `job.read` on the run's job, which every role holds globally, so role-based callers see no change. A role-only PAT whose stored role is empty now gets `403 forbidden: no role` on these routes (as on every other route). |
| 9 | Run replay (`POST /api/v1/runs/{id}/replay`) was not audited, and the audit log recorded only successful-or-failed state changes. | Replay is audited as `run.replay`. Every audit row also records the checked `permission`, its `scope` and `grantedBy`, and **denied reads** (including a hidden-job `404`) are recorded. See [Audit log](../audit.md). |
| 10 | Starting with SSO and `UNIFIED_TOKEN` both set logged nothing. | The controller logs `UNIFIED_TOKEN is set while SSO is configured: the env:UNIFIED_TOKEN token is always admin and bypasses oidc.permissions; unset it once SSO works and keep it only as a break-glass credential`. |
| 11 | `pats.role` and `sessions.role` defaulted to `admin`. | Migration `024_matrix_authorization` drops that default (every insert states its role) and adds the nullable owner, claims, ceiling and audit columns. Existing rows are untouched. |

Everything else is unchanged: break-glass is still the configured `UNIFIED_TOKEN`, always admin;
agents are a different kind of principal and are not affected; job code is not confined by folders
(see the warnings in [Authorization](../authorization.md)).

## Before you upgrade

1. **Search your scripts for `token create --role`.** Any script run by a user who asks for a role
   above their own now fails with the row 2 message instead of getting a weaker token. Ask for a role
   you hold, or use `--permission`.
2. **Tell developers that `token list` now shows their own tokens only.** Admins keep the full view.
3. **If automation creates PATs from a CLI login on a deployment that maps groups** (`roleMap` or
   `permissions.*.groups`), upgrade the CLI and log in again (row 4); a current CLI requests the
   roles-claim scope the server advertises.
4. **Upgrade order:** controller first, then the CLIs, then (once folder grants are configured)
   re-issue team PATs. A new CLI against an old controller still works: the old controller sends no
   scopes, so the CLI falls back to `openid email profile`. The one exception is
   `token create --permission`: an old controller ignores the field and would mint a full-role
   token, so the CLI refuses it, revokes the token at once and exits non-zero with
   `the server does not support --permission (it ignored the requested permissions); the token was revoked; upgrade the controller`.
5. Nothing else needs doing. Run the upgrade; read the startup log for the warning in row 10.

## Separating teams

Folder grants are opt-in. To let teams work in one deployment without touching each other's jobs:

1. Put each team's jobs under its own folder (`metadata.annotations.path: team-a`, or a directory
   per team in the AppSource repository). A job `team-a/build` is covered by folder `team-a`.
2. In the controller config file, move team groups out of `roleMap` (or leave them out) and set
   `defaultRole: deny`. Add one folder entry per team, and the global-only permissions every team
   still needs:

   ```yaml
   oidc:
     defaultRole: deny
     roleMap:
       "my-org:platform": admin
     permissions:
       global:
         groups:
           "my-org:engineering": [agent.read, appsource.read, webhook.read, vars.read, token.create]
       folders:
         team-a:
           groups:
             "my-org:team-a": developer
         team-b:
           groups:
             "my-org:team-b": developer
   ```

   `token.create` is global-only: without it a folder-only user cannot create PATs.
3. Restart the controller (it refuses to start on a malformed block; see
   [Troubleshooting](../../troubleshooting/controller-and-database.md#controller-refuses-to-start-with-an-oidcpermissions-error)).
   Users log in again so their sessions store their groups.
4. Check the result: `unified-cli authz show`, and for any user
   `unified-cli authz explain --user <email> --job team-a/build`.
5. Keep a team out of its own production approvals with a **sibling** folder (`team-a-prod`), not a
   nested one: grants are unions, so a `developer` grant on `team-a` also covers `team-a/prod`.

## Re-issue team PATs after the switch

A PAT created before the upgrade is **role-only**: it carries a global role and has no owner, so
folder grants do not narrow it. When folder grants are configured the controller logs, at startup,
`role-only personal access tokens with a role above viewer bypass folder grants until revoked` with
the `count`. After you introduce folders:

1. Have each team member create their team's automation tokens again while logged in through SSO,
   with the narrowest ceiling the automation needs. The token is bounded by its owner's grants and
   the ceiling, and recomputed against the current config on every request:

   ```bash
   unified-cli token create team-a-ci --permission job.read --permission run.trigger --expires-in 2160h
   ```

2. Switch the automation to the new token.
3. As an admin, list and revoke the old role-only tokens:

   ```bash
   unified-cli token list
   unified-cli token delete <id>
   ```

Until you do, those old tokens keep exactly the access they had before; that is the point of the
upgrade being non-widening, and also why they are the next thing to retire.

## Reference: exact strings

| Where | Text |
|---|---|
| `POST /api/v1/tokens`, role above your own | `cannot grant role "developer": it exceeds your global role "viewer"; use --permission to grant folder permissions you hold` |
| `POST /api/v1/tokens`, permission you do not hold | `cannot grant permission "secret.write": you do not hold it` |
| `POST /api/v1/tokens`, pre-upgrade session | `cannot create a token: this login predates group-aware tokens; log out and log in again, then retry` |
| `POST /api/v1/tokens`, bearer without the roles claim | `cannot create a token: your OIDC token carries no "groups" claim; log in again so the token includes it, then retry` |
| `POST /api/v1/tokens`, identity without `sub` | `cannot create a token: your identity has no subject (sub); log in again, then retry` |
| `POST /api/v1/tokens`, empty list | `permissions must not be empty` |
| `POST /api/v1/tokens`, role and permissions together | `role and permissions are mutually exclusive` |
| `DELETE /api/v1/tokens/{id}`, not yours | `token not found` (404) |
| Role-only PAT or pre-upgrade session with an empty role | `forbidden: no role` (403) |
| Caller with a missing permission | `forbidden: missing permission "run.trigger" on job "team-a/build"` or `forbidden: missing permission "secret.write"` (403) |
| Caller whose grants are all gone | `forbidden: no permissions` / `forbidden: token owner has no permissions` (403) |

See [Authorization](../authorization.md), [Authentication](../authentication.md),
[CLI: token](../../reference/cli.md#token) and [Troubleshooting](../../troubleshooting/controller-and-database.md).
