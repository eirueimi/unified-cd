# CLI Reference

Complete reference for the `unified-cli` command-line tool.

## Table of Contents

- [Global Flags](#global-flags)
- [apply](#apply)
- [jobs](#jobs)
- [run](#run)
- [approve / reject](#approve--reject)
- [logs](#logs)
- [secret](#secret)
- [gitcredential](#gitcredential)
- [schedule](#schedule)
- [token](#token)
- [artifact](#artifact)
- [export](#export)
- [appsource](#appsource)
- [webhook](#webhook)
- [audit](#audit)
- [login](#login)
- [agent](#agent)
- [Configuration precedence](#configuration-precedence)
- [Configuration File](#configuration-file)
- [Resource Kinds Accepted by apply](#resource-kinds-accepted-by-apply)

---

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

Use [`approve` / `reject`](#approve--reject) (`unified-cli approve <run-id> <step-index>`)
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
`{{ .Secrets.NAME }}` template syntax — see [Secrets Management Guide](secrets.md).

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

Manage [Schedules](resources.md#schedule). Create/update a schedule with
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
unified-cli token create <name> [--expires-in duration] [--role role]

  --expires-in  string   Token expiry duration (e.g. "720h", "8760h"). No expiry if omitted.
  --role        string   Role for the token: admin, developer, or viewer
                         (default: your own role; capped at your own role)
```

Generates a new PAT. The token value is shown only once.

```bash
unified-cli token create ci-bot
# => Token created (shown only once):
# =>
# =>   exc_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
# =>
# => Name: ci-bot  ID: 08c1779c-812f-4d68-9971-25ff3467637e  Role: developer

# With expiry
unified-cli token create deploy-bot --expires-in 8760h

# With an explicit role (cannot exceed the caller's own role)
unified-cli token create viewer-bot --role viewer
```

Use the printed token as a bearer token in CLI, API calls, or agent configuration.

### token list

```
unified-cli token list
```

Lists all tokens (IDs, names, and roles only; values are never retrievable).

```
a7c3fec8-1922-4278-b7ca-1ec489839a61   env:UNIFIED_TOKEN   admin       (2026-07-03)   ← bootstrap token from UNIFIED_TOKEN env var
08c1779c-812f-4d68-9971-25ff3467637e   ci-bot               developer   (2026-07-04)
```

### token delete

```
unified-cli token delete <id>
```

Revokes a token immediately.

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
AppSources) as one YAML file per resource:

```bash
unified-cli export -o ./exported/
```

- Jobs are written at their qualified path (`team-a/build` → `team-a/build.yaml`)
  so the output directory can be committed to Git and pointed at by an
  AppSource `path` directly — re-importing reproduces the same names.
- Non-Job kinds go under `schedules/`, `webhookreceivers/`,
  `gitcredentials/`, `appsources/`.
- `--unmanaged-only` exports only resources not already managed by an
  AppSource (useful for migrating manually-applied resources to Git).
- `--force` allows writing into a non-empty directory.
- Secret **values** are never exported (they are not retrievable via the API);
  re-create them with `unified-cli secret set` after a restore.
- Output is regenerated from the stored spec: comments and key order of the
  originally applied YAML are not preserved.

---

## appsource

Manage GitOps [AppSources](resources.md#appsource). Create/update an AppSource
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

Manage [WebhookReceivers](resources.md#webhookreceiver). Create/update a
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

View the audit log. Admin only — the server returns 403 for non-admin tokens.

### audit list

```
unified-cli audit list [--limit int]

  --limit  int   Max number of entries to show (server default: 100)
```

Prints a table of time, actor, action, resource, and status, newest first.

```bash
unified-cli audit list --limit 5
# => 2026-07-04T09:12:00Z   alice   token.create   token/ci-bot        200
# => 2026-07-04T09:10:41Z   bob     run.cancel     run/run-abc123      200
```

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

See the [Authentication Guide](authentication.md) for full SSO setup details.

---

## agent

Manage agents and install them as system services.

### agent install

```
unified-cli agent install --server <url> --id <id> [OPTIONS]

  --server  string   Controller URL (required)
  --credential-file string   Protected VM refresh-credential file (default: $HOME/.unified-cd/<id>/credential.json)
  --enrollment-token-file string  One-time enrollment-token file (required until first enrollment)
  --id      string   Agent identifier (required)
  --label   string   Agent label (repeatable, e.g. --label kind:linux)
  --dir     string   Installation directory (default: ~/.unified-cd)
```

Installs the agent as a system service:
- **Linux**: writes a `systemd` unit file to `<dir>/systemd/unified-cd-agent.service`
- **macOS**: writes a `launchd` plist to `<dir>/launchd/dev.unified-cd.agent.plist`
- **Windows**: prints manual Task Scheduler instructions

```bash
unified-cli agent install \
  --server https://controller.example.invalid \
  --id worker-01 \
  --credential-file /var/lib/unified-cd-agent/credentials.json \
  --enrollment-token-file /var/lib/unified-cd-agent/enrollment.token \
  --label kind:linux \
  --label env:prod \
  --dir /opt/unified-cd
```

Create the one-time enrollment file first with `unified-cli agent enrollment
create --agent-id worker-01 --output-file /var/lib/unified-cd-agent/enrollment.token`.
The file is shown only once and the service rotates its VM refresh credential
without placing plaintext credentials in its command line. `--token` is not an
`agent install` option; `UNIFIED_AGENT_TOKEN` is reserved for explicitly
enabled legacy shared-token migration.

When `--credential-file` is omitted, both `agent install` and the agent runtime
default it to `$HOME/.unified-cd/<id>/credential.json`, so a fresh host only
needs `--server`, `--id`, and `--enrollment-token-file`.

### agent uninstall

```
unified-cli agent uninstall [OPTIONS]

  --dir     string   Installation directory (default: ~/.unified-cd)
  --id      string   Agent identifier (required with --purge-credentials)
  --purge-credentials  bool  Also delete the default $HOME/.unified-cd/<id>/ credential directory
```

Removes the files written by `agent install` (the `agent.yaml` and the
platform service file under `<dir>`) and prints the commands to disable and
remove an already-enabled service (`systemctl --user disable --now
unified-cd-agent` on Linux, `launchctl unload ...` on macOS). Credentials are
left in place unless `--purge-credentials --id <id>` is given, since they are
independently revocable and deleting them is irreversible.

```bash
unified-cli agent uninstall --dir /opt/unified-cd
# Fully remove, including the persisted refresh credential:
unified-cli agent uninstall --id worker-01 --purge-credentials
```

### agent enrollment, identity, and enrollment-policy

Administrators create or revoke VM enrollment credentials; viewers may list
metadata without plaintext values:

```bash
unified-cli agent enrollment create --agent-id worker-01 \
  --label kind:linux --capability container \
  --output-file /var/lib/unified-cd-agent/enrollment.token
unified-cli agent enrollment list
unified-cli agent enrollment revoke <enrollment-id>
```

Manage an identity with `agent identity get|enable|disable|revoke-credentials
<agent-id>`. Use `agent enrollment-policy create|update|get|list|delete` for
Kubernetes workload enrollment. Create/update accepts `--cluster`, repeatable
`--namespace` and `--service-account`, repeatable labels/capabilities,
`--access-token-ttl` (5 minutes to 4 hours), and `--enabled`; it never accepts
kubeconfig contents. See [Migration: agent authentication](migration-agent-auth.md)
for the complete rollout and API paths.

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

**Agent service config** — `~/.unified-cd/agent.yaml` (or `<dir>/agent.yaml` if
`--dir` was passed), written by [`agent install`](#agent-install). Read by the
`agent` service process, not by the `unified-cli` CLI commands above.

```yaml
server: http://localhost:8080
agentId: worker-01
credentialFile: /var/lib/unified-cd-agent/credentials.json
enrollmentTokenFile: /var/lib/unified-cd-agent/enrollment.token
binPath: /usr/local/bin/unified-cli
labels:
  - kind:linux
  - env:prod
```

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
