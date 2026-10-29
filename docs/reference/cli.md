# CLI Reference

Complete reference for the `unified-cli` command-line tool.

## Global Flags

These flags apply to all subcommands.

```
unified-cli [GLOBAL FLAGS] <subcommand>

  --config  string   Config file path (default: ~/.config/unified-cd/config.yaml)
  --server  string   Controller server URL (env: UNIFIED_SERVER)
  --token   string   Bearer token (env: UNIFIED_TOKEN)
```

**Resolution order** (highest priority first): `--flag` > environment variable > config file.
See [Configuration precedence](#configuration-precedence) below for details.

---

## Configuration precedence

Values are resolved in this order (highest wins):

1. Command-line flags (`--server`, `--token`)
2. Environment variables (`UNIFIED_SERVER`, `UNIFIED_TOKEN`)
3. Config file `~/.config/unified-cd/config.yaml` (written by `login`)

```bash
# Config file has server: http://localhost:8080
# The env var points at a bad port, but the flag wins, so this succeeds:
UNIFIED_SERVER=http://localhost:9 unified-cli --server http://localhost:8080 jobs list
# => (lists jobs — the --server flag beat both UNIFIED_SERVER and the config file)
```

---

## version

Print the unified-cli version (stamped at build time from the git tag; `dev`
for un-stamped local builds).

```bash
unified-cli version
```

---

## apply

Apply a YAML resource definition to the controller. Creates or updates the resource.

```
unified-cli apply -f <file> [--dry-run]

  -f, --file  string   Path to YAML file (required)
      --dry-run        Validate the YAML locally without applying it to the server
```

**Supported resource kinds:** `Job`, `Schedule`, `WebhookReceiver`, `GitCredential`, `AppSource`

Multi-document YAML (separated by `---`) is supported — all resources in the file are applied.

With `--dry-run` each document is parsed and validated locally (no server call,
no changes) and reported as valid, or the command exits non-zero on the first
error — useful as a lint gate in pull-request CI.

```bash
# Apply a single resource
unified-cli apply -f job.yaml

# Apply multiple resources in one file
unified-cli apply -f all-resources.yaml

# Validate without applying (e.g. in PR CI)
unified-cli apply -f job.yaml --dry-run

# Apply from stdin
cat job.yaml | unified-cli apply -f -
```

---

## jobs

Manage registered jobs.

### jobs list

```
unified-cli jobs list
```

Lists all registered jobs.

```
hello          (2026-06-01)
build          (2026-06-15)
ci-pipeline    (2026-06-20)
```

### jobs get

```
unified-cli jobs get <name>
```

Shows details of a job, including its input parameter definitions.

```
Name:        build
ID:          08c1779c-812f-4d68-9971-25ff3467637e
APIVersion:  unified-cd/v1
Updated:     2026-06-15 10:00:00
Inputs:
  image                (string, required)  container image name
  tag                  (string, default=latest)
```

### jobs show-yaml

```
unified-cli jobs show-yaml <name>
```

Prints the job's YAML definition as stored on the controller.

```bash
unified-cli jobs show-yaml build > build.yaml
```

### jobs delete

```
unified-cli jobs delete <name>
```

Deletes a job and all its run history (steps, logs, artifacts).

```bash
unified-cli jobs delete old-job
# => job "old-job" deleted
```

---

## run

Trigger and manage job runs.

### run trigger

```
unified-cli run trigger <job-name> [--param key=value ...] [--param-file FILE] [--wait] [--follow] [--timeout DURATION] [--output KEY ...]

  --param   string     Input parameter in key=value format (repeatable)
  --param-file FILE    File of key=value lines to use as params (--param overrides on conflict)
  --wait               Block until the run finishes; exit non-zero if it did not succeed
  --follow             Stream step logs while waiting (implies --wait)
  --timeout DURATION   Max time to wait, e.g. 30m (default: no timeout)
  --output KEY         After a successful wait, print this run output's value (repeatable; implies --wait)
```

Triggers a run of the specified job and prints the run ID. With `--wait` (or
`--follow`), the command then blocks until the run reaches a terminal state and
maps the outcome to its exit code, so it fits directly in a CI pipeline.

`--param-file` loads parameters from a `key=value` file (blank lines and `#`
comments are skipped); explicit `--param` flags override file values on conflict.

`--output KEY` prints a run-level output's value to stdout after the run
succeeds — handy for scripting (`URL=$(unified-cli run trigger deploy --output url)`).
When `--output` is used the run ID is written to stderr instead of stdout, so
stdout carries only the captured value(s).

**Exit codes (when waiting):** `0` succeeded · `1` failed · `2` cancelled ·
`124` wait timed out.

```bash
# Trigger with parameters
unified-cli run trigger build --param image=myapp --param tag=v1.0

# Capture the run ID
RUN_ID=$(unified-cli run trigger build --param image=myapp)

# CI one-liner: trigger, stream logs, and fail the step if the run fails
unified-cli run trigger build --param image=myapp --follow --timeout 30m

# Fire-and-wait without logs (just the exit code)
unified-cli run trigger deploy --wait
```

### run wait

```
unified-cli run wait <run-id> [--follow] [--timeout DURATION]

  --follow             Stream step logs while waiting
  --timeout DURATION   Max time to wait, e.g. 30m (default: no timeout)
```

Waits for an existing run to finish. Exit codes match `run trigger --wait`
(`0` succeeded · `1` failed · `2` cancelled · `124` timeout). Useful when the run
was started elsewhere (e.g. by a webhook or schedule) and you want to block on it.

```bash
RUN_ID=$(unified-cli run trigger build)
unified-cli run wait "$RUN_ID" --follow --timeout 20m
```

### run replay

```
unified-cli run replay <run-id>
```

Creates a new run from an existing run's **original spec snapshot** and params,
reproducing it exactly even if the job YAML has since been re-applied, and prints
the new run ID. This differs from the web UI's **Rerun** button, which re-triggers
with the job's *current* spec. Pipe the new ID to `run wait` to block on it.

```bash
NEW=$(unified-cli run replay run-abc123)
unified-cli run wait "$NEW" --follow
```

### run cancel

```
unified-cli run cancel <run-id>
```

Cancel a run that is Pending, Queued, or Running. The agent interrupts the
in-flight step (reported as `Cancelled`), `finally:` steps still execute,
and the run finishes as `Cancelled`.

```bash
unified-cli run cancel run-abc123
# => run "run-abc123" cancelled
```

### run list

```
unified-cli run list --job <job-name>

  --job  string   Job name to list runs for (required)
```

Lists recent runs for a job.

```
run-abc123   Succeeded   2026-06-20 10:00   manual
run-def456   Failed      2026-06-20 09:30   schedule:nightly-build
run-ghi789   Running     2026-06-20 11:00   webhook:github-push
```

### run list-active

```
unified-cli run list-active
```

Lists all runs in Pending, Queued, or Running state across all jobs.

```
run-abc123   build    Running   2026-06-20 11:00   manual
run-def456   deploy   Queued    2026-06-20 11:01   webhook:github-push
```

### run show <run-id>

```
unified-cli run show <run-id>
```

Shows the run's status, trigger source, timestamps, and input params, followed
by a per-step table (index, name, status, exit code).

```
ID:          run-abc123
Job:         build
Status:      Succeeded
Triggered:   manual
Created:     2026-06-20 10:00:00
Updated:     2026-06-20 10:02:15
Params:
  image=myapp
  tag=v1.0
Steps:
  [0] checkout             Succeeded (exit 0)
  [1] build                Succeeded (exit 0)
  [2] deploy-gate          Approved
```

### run outputs

```
unified-cli run outputs <run-id>
```

Shows run-level outputs reported by the job, one `key=value` per line
(sorted by key).

```bash
unified-cli run outputs run-abc123
# => imageDigest=sha256:abcd...
# => version=1.4.2
```

### run show-yaml

```
unified-cli run show-yaml <run-id>
```

Prints the YAML definition the run was executed with (the job spec snapshot
taken at trigger time).

```bash
unified-cli run show-yaml run-abc123 > run-spec.yaml
```

### run approvals

```
unified-cli run approvals <run-id>
```

Lists the run's approval gates and their state
(`Pending` / `Approved` / `Rejected` / `TimedOut`).

```
[2]   deploy-gate   Approved   by alice at 2026-06-20 11:05:00
[4]   step[4]       Pending
```

Use [`approve` / `reject`](#approve-reject) (`unified-cli approve <run-id> <step-index>`)
to decide a pending gate.

### run delete

```
unified-cli run delete <run-id>
```

Deletes a run that has reached a terminal state (Succeeded, Failed, or Cancelled).

```bash
unified-cli run delete run-abc123
# => run "run-abc123" deleted
```

---

## approve / reject

Decide a pending approval gate on a run (see [`run approvals`](#run-approvals)
for listing pending gates).

```
unified-cli approve <run-id> <step-index> [--comment string]
unified-cli reject <run-id> <step-index> [--comment string]

  --comment  string   Optional comment recorded with the decision
```

```bash
unified-cli approve run-abc123 2
# => approved step 2 of run run-abc123

unified-cli reject run-abc123 2 --comment "needs a second review"
# => rejected step 2 of run run-abc123
```

---

## logs

Stream or retrieve logs for a run.

```
unified-cli logs [-f] [-t] [--step] <run-id>

  -f, --follow       Follow log output until the run completes (polls every 300ms)
  -t, --timestamps   Prefix each line with its local HH:MM:SS timestamp
      --step         Prefix each line with its step name, e.g. [build]
```

`--timestamps` and `--step` combine as `HH:MM:SS [step] line`. With `--step`, the
run-level "System" stream is labelled `[System]` and each sidecar's own output is
labelled with its container name.

```bash
# Print all available logs and exit
unified-cli logs run-abc123

# Follow live output until completion
unified-cli logs -f run-abc123

# Follow with timestamps and per-line step names (steps interleave)
unified-cli logs -f -t --step run-abc123

# Common pattern: trigger then follow
RUN_ID=$(unified-cli run trigger build --param image=myapp)
unified-cli logs -f "$RUN_ID"
```

Secret values that appear in output are automatically masked as `***`.

---

## secret

Manage encrypted secrets stored on the controller.

### secret set

```
unified-cli secret set <name> [value]

  -f, --file  string   Read value from file instead of argument or stdin
```

Creates or updates a secret (idempotent).

```bash
# Value as argument
unified-cli secret set DB_PASSWORD "mysecret"

# Value from file (SSH keys, certificates, multiline values)
unified-cli secret set DEPLOY_KEY -f ~/.ssh/id_rsa

# Value from stdin (avoids shell history)
echo -n "mysecret" | unified-cli secret set DB_PASSWORD

# Interactive (hidden input)
read -s SECRET && echo -n "$SECRET" | unified-cli secret set DB_PASSWORD
```

**Naming rules:** alphanumerics, underscores, and hyphens; must start with a letter or `_`.
Hyphenated names (e.g. `slack-webhook-url`) work with both `{{ secrets.NAME }}` and
`{{ .Secrets.NAME }}` template syntax — see [Secrets Management Guide](../user-guide/secrets.md).

### secret list

```
unified-cli secret list
```

Lists secret names and creation dates. Values are never shown.

```
DATABASE_URL    (2026-06-01)
DEPLOY_KEY      (2026-06-01)
API_KEY_PROD    (2026-06-10)
```

### secret delete

```
unified-cli secret delete <name>
```

```bash
unified-cli secret delete OLD_SECRET
# => secret "OLD_SECRET" deleted
```

---

## gitcredential

Manage Git credentials used for `git://` template URIs and AppSource repos.
There is no `set`/`create` subcommand — GitCredentials are created/updated with
`apply -f` (`kind: GitCredential`); the commands below operate on existing ones.

### gitcredential list

```
unified-cli gitcredential list
```

```bash
unified-cli gitcredential list
# => github-bot   host=github.com type=ssh secretRef=deploy-key
```

### gitcredential delete

```
unified-cli gitcredential delete <name>
```

```bash
unified-cli gitcredential delete github-bot
# => gitcredential deleted: github-bot
```

---

## schedule

Manage [Schedules](../user-guide/resources/schedule.md#schedule). Create/update a schedule with
`apply -f` (`kind: Schedule`); the commands below operate on existing ones.

### schedule list

```
unified-cli schedule list
```

Lists all registered schedules.

```
nightly-build    cron=0 2 * * * job=build
weekly-report    cron=0 9 * * 1 job=report
```

### schedule delete

```
unified-cli schedule delete <name>
```

```bash
unified-cli schedule delete nightly-build
# => schedule deleted: nightly-build
```

---

## token

Manage Personal Access Tokens (PATs) for authentication.

### token create

```
unified-cli token create <name> [--expires-in duration] [--role role] [--permission name]...

  --expires-in  string   Token expiry duration (e.g. "720h", "8760h"). No expiry if omitted.
  --role        string   Role ceiling for the token: admin, developer, or viewer.
                         Must not exceed your own global role; it is refused, never reduced.
  --permission  string   Permission ceiling, e.g. job.read. Repeatable. You must hold each one
                         (globally or on at least one folder). Cannot be combined with --role.
```

Generates a new PAT. The token value is shown only once.

A PAT created by an SSO user records that user as its **owner**. Its permissions are always the
owner's permissions under the current config, intersected with the ceiling; with neither `--role`
nor `--permission` there is no ceiling (the token carries everything its owner has).
`--role` no longer reduces your request to your own role: a role above your own global role is refused with a `400`, so a script
discovers the problem at creation instead of at a later `403`. A user who holds only folder grants
has no global role and uses `--permission`. See [Authorization: Personal access tokens](../operator-manual/authorization.md#personal-access-tokens).

Names equal to `env:UNIFIED_TOKEN` or starting with `env:` are reserved and rejected
(`token name "env:..." is reserved`).

Refusals, printed as `server error: <message>` (the HTTP status in parentheses):

| Message | Status | Cause |
|---|---|---|
| `cannot grant role "developer": it exceeds your global role "viewer"; use --permission to grant folder permissions you hold` | 400 | `--role` above your global role. With no global role the tail reads `it exceeds your global role (none)`. |
| `cannot grant permission "secret.write": you do not hold it` | 400 | A `--permission` you hold neither globally nor on any folder |
| `unknown role "x"` / `unknown permission "x"` | 400 | A misspelled `--role` / `--permission` |
| `role and permissions are mutually exclusive` | 400 | `--role` together with `--permission` |
| `permissions must not be empty` | 400 | An API request with `"permissions": []` |
| `cannot grant permissions: this token has no owner; use --role` | 400 | `--permission` used with the break-glass `UNIFIED_TOKEN` or another role-only PAT |
| `token name "env:x" is reserved` | 400 | A name equal to `env:UNIFIED_TOKEN` or starting with `env:` |

| `cannot create a token: this login predates group-aware tokens; log out and log in again, then retry` | 403 | You are using a browser session created before the upgrade |
| `cannot create a token: your OIDC token carries no "groups" claim; log in again so the token includes it, then retry` | 403 | Your CLI login's id_token has no roles claim while the config has group-keyed grants (log in again with a current CLI, which requests the roles-claim scope the server advertises) (`groups` is the configured `oidc.rolesClaim`) |
| `cannot create a token: your identity has no subject (sub); log in again, then retry` | 403 | Your identity carries no `sub` |
| `forbidden: missing permission "token.create"` | 403 | You do not hold `token.create` (add it in `oidc.permissions.global` for a folder-only user) |

Against a controller that predates `--permission`, the server ignores the field and would create a
full-role token. The CLI detects this (the response carries no `permissions`), revokes the
token immediately without printing it, and fails with
`the server does not support --permission (it ignored the requested permissions); the token was revoked; upgrade the controller`.
If the revoke itself fails, the error prints the token ID and tells you to revoke it manually
(`unified-cd token delete <id>`); the process exits non-zero either way.
If the server instead returns permissions that were not requested, the token is revoked too
(`the server returned permissions that were not requested; the token was revoked`). When you create
a token with a PAT, the server may return fewer permissions than requested (the ones you hold); the
CLI keeps that token and prints `warning: the token was narrowed to the permissions you hold: <list>` to stderr.

```bash
unified-cli token create ci-bot
# => Token created (shown only once):
# =>
# =>   exc_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
# =>
# => Name: ci-bot  ID: 08c1779c-812f-4d68-9971-25ff3467637e  Role: developer
#    (Role is empty for an SSO user's token that has no --role ceiling)

# With expiry
unified-cli token create deploy-bot --expires-in 8760h

# With an explicit role ceiling (cannot exceed the caller's own global role)
unified-cli token create viewer-bot --role viewer

# With a permission ceiling (each one must be held by the caller)
unified-cli token create ci-trigger --permission job.read --permission run.trigger
# => Name: ci-trigger  ID: 5d0e...  Role:
# => Permissions: job.read, run.trigger
```

Use the printed token as a bearer token in CLI, API calls, or agent configuration.

### token list

```
unified-cli token list
```

Lists the tokens you may see (IDs, names, and the ceiling only; values are never retrievable): your own
tokens with `token.create`, every token with `token.manage` (admin). The ceiling column is the role,
or the comma-separated permission list for a permission-ceiling token. A role-only (pre-upgrade)
token has no owner, so a caller without `token.manage` does not see it.

```
a7c3fec8-1922-4278-b7ca-1ec489839a61   env:UNIFIED_TOKEN   admin       (2026-07-03)   ← bootstrap token from UNIFIED_TOKEN env var
08c1779c-812f-4d68-9971-25ff3467637e   ci-bot               developer   (2026-07-04)
```

### token delete

```
unified-cli token delete <id>
```

Revokes a token immediately. With `token.create` you can revoke only your own tokens; any other id,
including a role-only (pre-upgrade) token, answers `server error: token not found`. `token.manage`
(admin) revokes any token.

```bash
unified-cli token delete 08c1779c-812f-4d68-9971-25ff3467637e
# => token "08c1779c-812f-4d68-9971-25ff3467637e" revoked
```

---

## artifact

List and download artifacts uploaded by a run's steps (via `uploadArtifact`).
Artifacts are stored on the controller as tar+zstd archives; `artifact download`
fetches and extracts the archive for you.

### artifact list

```
unified-cli artifact list <run-id>
```

Lists artifact names produced by the run.

```bash
unified-cli artifact list run-abc123
# => cli-art
```

### artifact download

```
unified-cli artifact download <run-id> <name> [--dest DIR]

  --dest  string   Destination directory (default: current directory)
```

Downloads the named artifact and extracts its tar+zstd archive into `--dest`
(or the current directory if `--dest` is omitted).

```bash
# Extract into the current directory
unified-cli artifact download run-abc123 cli-art
# => extracted cli-art of run run-abc123 to .

# Extract into a specific directory
unified-cli artifact download run-abc123 cli-art --dest ./out
# => extracted cli-art of run run-abc123 to ./out
```

---

## export

Export all resources (Jobs, Schedules, WebhookReceivers, GitCredentials,
Vars, AppSources) as one YAML file per resource:

```bash
unified-cli export -o ./exported/
```

- Jobs are written at their qualified path (`team-a/build` → `team-a/build.yaml`)
  so the output directory can be committed to Git and pointed at by an
  AppSource `path` directly — re-importing reproduces the same names.
- Non-Job kinds go under `schedules/`, `webhookreceivers/`,
  `gitcredentials/`, `vars/`, `appsources/`.
- `--unmanaged-only` exports only resources not already managed by an
  AppSource (useful for migrating manually-applied resources to Git).
- `--force` allows writing into a non-empty directory.
- Secret **values** are never exported (they are not retrievable via the API);
  re-create them with `unified-cli secret set` after a restore.
- Output is regenerated from the stored spec: comments and key order of the
  originally applied YAML are not preserved.

---

## appsource

Manage GitOps [AppSources](../user-guide/resources/app-source.md#appsource). Create/update an AppSource
with `apply`; the commands below operate on existing ones.

### appsource sync

```
unified-cli appsource sync <name>
```

Forces a re-sync: resets the AppSource's `lastCommit` so the next reconciler
tick (≤30s) re-syncs from Git. Returns immediately — it does not wait for the
sync to finish. This is the CLI equivalent of `POST /api/v1/appsources/<name>/sync`.

```bash
unified-cli appsource sync my-pipelines
# => appsource sync scheduled: my-pipelines
```

### appsource list

```
unified-cli appsource list
```

```bash
unified-cli appsource list
# => my-pipelines   repo=https://github.com/acme/pipelines rev=main lastCommit=abc1234
```

### appsource get

```
unified-cli appsource get <name>
```

Shows repoURL, targetRevision, path, last synced commit, and last sync time.

### appsource delete

```
unified-cli appsource delete <name>
```

Removes the AppSource. Jobs it previously synced are left in place (they are not
pruned by deletion).

---

## webhook

Manage [WebhookReceivers](../user-guide/resources/webhook-receiver.md#webhookreceiver). Create/update a
receiver with `apply -f` (`kind: WebhookReceiver`); the commands below
operate on existing ones.

### webhook list

```
unified-cli webhook list
```

Lists registered webhook receivers.

```
github-push    (2026-06-01)
gitlab-push    (2026-07-01)
```

### webhook delete

```
unified-cli webhook delete <name>
```

Deletes a webhook receiver. Payloads sent to its `/webhook/<name>` URL will
return 404 afterwards.

```bash
unified-cli webhook delete github-push
# => webhook receiver "github-push" deleted
```

---

## audit

View the audit log. Requires the `audit.read` permission (admin bundle only); other callers get `403 forbidden: missing permission "audit.read"`.

### audit list

```
unified-cli audit list [--limit int]

  --limit  int   Max number of entries to show (server default: 100)
```

Prints a table of time, actor, action, resource, status, permission, and
grantedBy, newest first. `permission` is the permission that was checked and
`grantedBy` the grant that allowed it; both show `-` when not recorded, and
`grantedBy` is `-` on a denied request.

```bash
unified-cli audit list --limit 5
# => 2026-07-04T09:12:00Z   alice   token.create   token/ci-bot    200   token.create   role:admin (pat)
# => 2026-07-04T09:10:41Z   bob     run.cancel     run/run-abc123  200   run.cancel     role:developer (pat)
```

---

## authz

Inspect the authorization policy. Requires the `authz.read` permission, which
only the `admin` bundle holds (it can also be granted on its own in
`oidc.permissions.global`); other callers get
`403 forbidden: missing permission "authz.read"`.

### authz show

```
unified-cli authz show [-o table|yaml|json]
```

Prints the policy as the controller resolved it at startup, never the raw
config file: the built-in role bundles, every `roleMap` / `userMap` entry and
`defaultRole` with the role it resolves to (a value that is not a known role is
marked `ignored` for `roleMap`, where it never matches, and `denies login` for `userMap` or `defaultRole`, where the user is denied), and every `oidc.permissions` entry in file order with the
grant as written and its expansion at that scope. It contains no secrets and no
per-user data. The default `-o table` has the sections `BUNDLES`, `LEGACY`, `GLOBAL`
and `FOLDER <name>` with the columns `SUBJECT  NAME  GRANT  PERMISSIONS`; `-o json` and
`-o yaml` print the server's answer. Any other format is refused before a request is
made: `unsupported output format "x" (use table, yaml or json)`. Without OIDC only the
bundles are listed. Entries keep their file order, with group entries before user entries
within a scope; the `roleMap` / `userMap` entries are sorted by key.

### authz explain

```
unified-cli authz explain (--user string | --group string...) [--job string | --folder string] [-o table|yaml|json]

  --user    string   User to explain; matched as both email and sub
  --group   string   Group of the identity (repeatable); when given it is used as the group list
  --job     string   Explain at this job (it need not exist)
  --folder  string   Explain at this folder, as a job directly inside it would be
```

Prints the permissions the identity holds at the scope and every grant that
gives each one (joined with `; `), using the same `grantedBy` strings as the
audit log. Without `--group`, the groups of the user's latest browser login are
used (`groups (login)`); with none stored the group list is empty
(`groups (none)`). `--job` and `--folder` are mutually exclusive; with neither
the scope is global. `explain` describes an identity, not a token: a PAT's
ceiling is not applied.

```bash
unified-cli authz explain --user dev@example.com --group my-org:team-a --job team-a/build
# => identity       dev@example.com
# => groups (request)  my-org:team-a
# => scope          job team-a/build
# => global role    -
# => login allowed  true
# =>
# => PERMISSION   GRANTED BY
# => job.read     folder:team-a (groups["my-org:team-a"])
# => job.configure  folder:team-a (groups["my-org:team-a"])
# => ...
```

At least one of `--user` / `--group` is required (`user or groups is required`), and
`--job` / `--folder` cannot be combined (`job and folder are mutually exclusive`). A
`--folder` is validated like a config folder key. The table also prints `global role`
(the legacy `roleMap` / `userMap` / `defaultRole` result, `-` if it denies) and
`login allowed`, the login rule from [Authorization](../operator-manual/authorization.md#how-a-callers-permissions-are-resolved).
Within a scope, group grants are listed before user grants.

The same data is available as `GET /api/v1/authz/policy` and
`POST /api/v1/authz/explain` (body `{"user", "groups", "job", "folder"}`).
Allowed calls are not written to the audit log; denied calls are.

---

## login

Authenticate using OIDC (SSO) device flow and save the token to the config file.
`--server` is required (or set `UNIFIED_SERVER`).

```
unified-cli login --server <url>

  --server     string   Controller server URL (required)
  --issuer     string   OIDC issuer URL (auto-discovered from server if omitted)
  --client-id  string   OIDC device flow client ID (auto-discovered if omitted)
```

```bash
unified-cli login --server http://unified-cd.example.com
# => Open the following URL in your browser:
# =>   https://...
# =>
# => Waiting (user code: XXXX-XXXX)...
# =>
# => Logged in. Token saved to ~/.config/unified-cd/config.yaml
# => Expires: 2026-07-05T14:12:38Z
```

On success, the verifiable id_token (JWT) from the device flow is written to the
`token` field of `~/.config/unified-cd/config.yaml`, along with its expiry printed
to stdout (Dex default: ~24 hours). Re-run `login` when your token expires. If the
server has no SSO configured, `login` instead prompts for an existing PAT
(see [`token create`](#token-create)) and stores that instead.

See the [Authentication Guide](../operator-manual/authentication.md) for full SSO setup details.

---

## agent

Manage agents: issue enrollment credentials, and list, inspect, and monitor
registered agents. The agent itself runs as a separate binary
(`unified-cd-agent`), started directly or as a hand-configured system
service — see [Agent Identity and Enrollment](../operator-manual/agents.md#agent-identity-and-enrollment)
and [Configuration Reference: Agent](configuration.md#agent) for how to run
it. `--id` is optional; it defaults to the identity bound to a valid enrollment
token or persisted credential. `--credential-file` always defaults to
`$HOME/.unified-cd/<agent-id>/credential.json`. A valid explicit enrollment
token supplies the effective ID before local discovery. Without a token or
`--id`, the agent discovers exactly one ID-scoped default credential; if
multiple exist, select one explicitly with `--id` or `--credential-file`.
The former shared `$HOME/.unified-cd/credential.json` path is ignored unless
explicitly selected with `--credential-file`. See [the migration guide](../operator-manual/migrations/agent-id-scoped-credentials.md)
for existing installations.

An explicit VM `--agent-id` must use lowercase ASCII letters and digits with
internal `.`, `_`, or `-`, start and end with a letter or digit, and avoid
Windows reserved names such as `con` and `com1`. This applies to IDs issued for
the literal default credential directory. A legacy non-portable ID remains
usable when its credential is selected with an explicit `--credential-file`.

### agent enrollment, identity, and enrollment-policy

Administrators create or revoke VM enrollment credentials; viewers may list
metadata without plaintext values:

```bash
unified-cli agent enrollment create --agent-id worker-01 \
  --label kind:linux \
  --output-file /var/lib/unified-cd-agent/enrollment.token
unified-cli agent enrollment list
unified-cli agent enrollment revoke <enrollment-id>
```

`enrollment create` prints the exact `unified-cd-agent --server ... --id ...
--enrollment-token-file ...` (or, without `--output-file`, `--enrollment-token
<value>`) command to run on the target host once the token is in place —
there is no separate install step. An enrolled agent's labels come from
`--label` on `enrollment create` and are fixed for that agent's lifetime; the
agent process's own `--labels` flag is ignored for every agent. Capabilities
are not set at enrollment: the agent auto-detects and self-reports them
(`native` always, plus `container` when it can reach a container runtime) on
every registration — see [Capabilities and routing](../operator-manual/agents.md#capabilities-and-routing).

By default (no `--output-file`), `enrollment create` prints the token to
stdout once and embeds it inline in the suggested run command via
`--enrollment-token <value>` — convenient, but the token is then visible in
shell history and `ps`. `--output-file <path>` instead writes the token to a
new owner-only (`0600`) file and suggests `--enrollment-token-file <path>`;
prefer this on shared hosts. `--quiet` prints *only* the raw token (no other
output) and is mutually exclusive with `--output-file`, for piping straight
into the agent:

```bash
unified-cli agent enrollment create --agent-id agent-1 --label kind:linux --quiet \
  | unified-cd-agent --server https://ci.example.com --enrollment-token -
```

`--id` is optional on `unified-cd-agent`: when omitted (as above), the agent
adopts the identity bound to the enrollment token / persisted credential
instead of requiring it up front; when supplied, it is asserted against that
resolved identity.

On the agent side, `--enrollment-token-file <path>` and `--enrollment-token
<value>` (also settable via `UNIFIED_AGENT_ENROLLMENT_TOKEN`, or
`--enrollment-token -` to read the value from stdin, as above) are mutually
exclusive; the agent exits with an error if both are set.

Manage an identity with `agent identity get|enable|disable|revoke-credentials
<agent-id>`. Use `agent enrollment-policy create|update|get|list|delete` for
Kubernetes workload enrollment. Create/update accepts `--cluster`, repeatable
`--namespace` and `--service-account`, repeatable `--allowed-label` and
`--required-label`, `--access-token-ttl` (5 minutes to 4 hours), and
`--enabled`; it never accepts kubeconfig contents. Capabilities are not part
of the policy: each enrolled pod's agent auto-detects and self-reports its
own capabilities on registration.

### agent list

```
unified-cli agent list
```

Lists all registered agents and their status.

```
agent-1    ci-worker-01   linux   kind:linux,env:ci   2026-06-20 10:55
agent-2    ci-worker-02   linux   kind:linux,env:ci   2026-06-20 10:54
k8s-1      k8s-node-1     linux   kind:kubernetes     2026-06-20 10:55
```

### agent get

```
unified-cli agent get <agent-id>
```

Shows details of a registered agent.

```
ID:         agent-1
Hostname:   ci-worker-01
OS:         linux
Labels:     kind:linux,env:ci
Version:    1.2.3
LastSeenAt: 2026-06-20 10:55:12
Env:
  REGION=us-east
```

### agent runs

```
unified-cli agent runs <agent-id>
```

Lists recent runs claimed by the agent (most recent 50).

```
run-abc123   build    Succeeded   2026-06-20 10:00   manual
run-def456   deploy   Running    2026-06-20 11:00   schedule:nightly
```

---

## Configuration File

There are two separate config files — do not confuse them.

**CLI config** — `~/.config/unified-cd/config.yaml`, written by [`login`](#login)
(or hand-edited). Read by every `unified-cli` invocation via the global
`--config`/`--server`/`--token` flags.

```yaml
server: http://localhost:8080
token: dev-secret
```

`token` holds either a manually-configured bearer token or the id_token written by
`login` (OIDC device flow or PAT prompt) — both use the same field. There is no
`agentId` field in this file.

Override the path with `--config /path/to/config.yaml`.

**Agent runtime config** — an optional YAML file (e.g. `unified-agent.yaml`)
passed to the `unified-cd-agent` binary with `-f`, hand-written by whoever
deploys the agent. Read by the `unified-cd-agent` process, not by any
`unified-cli` command above. See [Configuration Reference: Agent Config
File](configuration.md#agent-config-file) for the full field list, flag/env
equivalents, and defaults — including `credentialFile`, which defaults to
`$HOME/.unified-cd/<agent-id>/credential.json` when left unset.

---

## Resource Kinds Accepted by apply

| Kind | Description |
|---|---|
| `Job` | Job definition (steps, params, concurrency, etc.) |
| `Schedule` | Cron-based trigger for a job |
| `WebhookReceiver` | Webhook endpoint configuration |
| `GitCredential` | Git authentication for private repos / template URIs |
| `AppSource` | GitOps-style sync of job definitions from a Git repository |

All resources use `apiVersion: unified-cd/v1`.
