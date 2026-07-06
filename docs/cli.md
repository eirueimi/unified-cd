# CLI Reference

Complete reference for the `unified-cd` command-line tool.

## Table of Contents

- [Global Flags](#global-flags)
- [apply](#apply)
- [jobs](#jobs)
- [run](#run)
- [logs](#logs)
- [secret](#secret)
- [token](#token)
- [artifact](#artifact)
- [export](#export)
- [appsource](#appsource)
- [webhook](#webhook)
- [login](#login)
- [agent](#agent)
- [Configuration precedence](#configuration-precedence)
- [Configuration File](#configuration-file)
- [Resource Kinds Accepted by apply](#resource-kinds-accepted-by-apply)

---

## Global Flags

These flags apply to all subcommands.

```
unified-cd [GLOBAL FLAGS] <subcommand>

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
UNIFIED_SERVER=http://localhost:9 unified-cd --server http://localhost:8080 jobs list
# => (lists jobs — the --server flag beat both UNIFIED_SERVER and the config file)
```

---

## apply

Apply a YAML resource definition to the controller. Creates or updates the resource.

```
unified-cd apply -f <file>

  -f, --file  string   Path to YAML file (required)
```

**Supported resource kinds:** `Job`, `Schedule`, `WebhookReceiver`, `GitCredential`, `AppSource`

Multi-document YAML (separated by `---`) is supported — all resources in the file are applied.

```bash
# Apply a single resource
unified-cd apply -f job.yaml

# Apply multiple resources in one file
unified-cd apply -f all-resources.yaml

# Apply from stdin
cat job.yaml | unified-cd apply -f -
```

---

## jobs

Manage registered jobs.

### jobs list

```
unified-cd jobs list
```

Lists all registered jobs.

```
hello          (2026-06-01)
build          (2026-06-15)
ci-pipeline    (2026-06-20)
```

### jobs get

```
unified-cd jobs get <name>
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
unified-cd jobs show-yaml <name>
```

Prints the job's YAML definition as stored on the controller.

```bash
unified-cd jobs show-yaml build > build.yaml
```

### jobs delete

```
unified-cd jobs delete <name>
```

Deletes a job and all its run history (steps, logs, artifacts).

```bash
unified-cd jobs delete old-job
# => job "old-job" deleted
```

---

## run

Trigger and manage job runs.

### run trigger

```
unified-cd run trigger <job-name> [--param key=value ...]

  --param  string   Input parameter in key=value format (repeatable)
```

Triggers a run of the specified job and prints the run ID.

```bash
# Trigger with no parameters
unified-cd run trigger hello

# Trigger with parameters
unified-cd run trigger build --param image=myapp --param tag=v1.0

# Capture the run ID
RUN_ID=$(unified-cd run trigger build --param image=myapp)
echo "Run started: $RUN_ID"
```

### run cancel

```
unified-cd run cancel <run-id>
```

Cancel a run that is Pending, Queued, or Running. The agent interrupts the
in-flight step (reported as `Cancelled`), `finally:` steps still execute,
and the run finishes as `Cancelled`.

```bash
unified-cd run cancel run-abc123
# => run "run-abc123" cancelled
```

### run list

```
unified-cd run list --job <job-name>

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
unified-cd run list-active
```

Lists all runs in Pending, Queued, or Running state across all jobs.

```
run-abc123   build    Running   2026-06-20 11:00   manual
run-def456   deploy   Queued    2026-06-20 11:01   webhook:github-push
```

### run outputs

```
unified-cd run outputs <run-id>
```

Shows run-level outputs reported by the job, one `key=value` per line
(sorted by key).

```bash
unified-cd run outputs run-abc123
# => imageDigest=sha256:abcd...
# => version=1.4.2
```

### run show-yaml

```
unified-cd run show-yaml <run-id>
```

Prints the YAML definition the run was executed with (the job spec snapshot
taken at trigger time).

```bash
unified-cd run show-yaml run-abc123 > run-spec.yaml
```

### run approvals

```
unified-cd run approvals <run-id>
```

Lists the run's approval gates and their state
(`Pending` / `Approved` / `Rejected` / `TimedOut`).

```
[2]   deploy-gate   Approved   by alice at 2026-06-20 11:05:00
[4]   step[4]       Pending
```

Use [`approve` / `reject`](#run) (`unified-cd approve <run-id> <step-index>`)
to decide a pending gate.

### run delete

```
unified-cd run delete <run-id>
```

Deletes a run that has reached a terminal state (Succeeded, Failed, or Cancelled).

```bash
unified-cd run delete run-abc123
# => run "run-abc123" deleted
```

---

## logs

Stream or retrieve logs for a run.

```
unified-cd logs [-f] <run-id>

  -f, --follow   Follow log output until the run completes (polls every 300ms)
```

```bash
# Print all available logs and exit
unified-cd logs run-abc123

# Follow live output until completion
unified-cd logs -f run-abc123

# Common pattern: trigger then follow
RUN_ID=$(unified-cd run trigger build --param image=myapp)
unified-cd logs -f "$RUN_ID"
```

Secret values that appear in output are automatically masked as `***`.

---

## secret

Manage encrypted secrets stored on the controller.

### secret set

```
unified-cd secret set <name> [value]

  -f, --file  string   Read value from file instead of argument or stdin
```

Creates or updates a secret (idempotent).

```bash
# Value as argument
unified-cd secret set DB_PASSWORD "mysecret"

# Value from file (SSH keys, certificates, multiline values)
unified-cd secret set DEPLOY_KEY -f ~/.ssh/id_rsa

# Value from stdin (avoids shell history)
echo -n "mysecret" | unified-cd secret set DB_PASSWORD

# Interactive (hidden input)
read -s SECRET && echo -n "$SECRET" | unified-cd secret set DB_PASSWORD
```

**Naming rules:** alphanumerics and underscores only; must start with a letter or `_`.
Hyphens are not allowed (template engine cannot parse them).

### secret list

```
unified-cd secret list
```

Lists secret names and creation dates. Values are never shown.

```
DATABASE_URL    (2026-06-01)
DEPLOY_KEY      (2026-06-01)
API_KEY_PROD    (2026-06-10)
```

### secret delete

```
unified-cd secret delete <name>
```

```bash
unified-cd secret delete OLD_SECRET
# => secret "OLD_SECRET" deleted
```

---

## token

Manage Personal Access Tokens (PATs) for authentication.

### token create

```
unified-cd token create <name> [--expires-in duration]

  --expires-in  string   Token expiry duration (e.g. "720h", "8760h"). No expiry if omitted.
```

Generates a new PAT. The token value is shown only once.

```bash
unified-cd token create ci-bot
# => Token created (shown only once):
# =>
# =>   exc_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
# =>
# => Name: ci-bot  ID: 08c1779c-812f-4d68-9971-25ff3467637e

# With expiry
unified-cd token create deploy-bot --expires-in 8760h
```

Use the printed token as a bearer token in CLI, API calls, or agent configuration.

### token list

```
unified-cd token list
```

Lists all tokens (IDs and names only; values are never retrievable).

```
a7c3fec8-1922-4278-b7ca-1ec489839a61   env:UNIFIED_TOKEN   (2026-07-03)   ← bootstrap token from UNIFIED_TOKEN env var
08c1779c-812f-4d68-9971-25ff3467637e   ci-bot               (2026-07-04)
```

### token delete

```
unified-cd token delete <id>
```

Revokes a token immediately.

```bash
unified-cd token delete 08c1779c-812f-4d68-9971-25ff3467637e
# => token "08c1779c-812f-4d68-9971-25ff3467637e" revoked
```

---

## artifact

List and download artifacts uploaded by a run's steps (via `uploadArtifact`).
Artifacts are stored on the controller as tar+zstd archives; `artifact download`
fetches and extracts the archive for you.

### artifact list

```
unified-cd artifact list <run-id>
```

Lists artifact names produced by the run.

```bash
unified-cd artifact list run-abc123
# => cli-art
```

### artifact download

```
unified-cd artifact download <run-id> <name> [--dest DIR]

  --dest  string   Destination directory (default: current directory)
```

Downloads the named artifact and extracts its tar+zstd archive into `--dest`
(or the current directory if `--dest` is omitted).

```bash
# Extract into the current directory
unified-cd artifact download run-abc123 cli-art
# => extracted cli-art of run run-abc123 to .

# Extract into a specific directory
unified-cd artifact download run-abc123 cli-art --dest ./out
# => extracted cli-art of run run-abc123 to ./out
```

---

## export

Export all resources (Jobs, Schedules, WebhookReceivers, GitCredentials,
AppSources) as one YAML file per resource:

```bash
unified-cd export -o ./exported/
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
  re-create them with `unified-cd secret set` after a restore.
- Output is regenerated from the stored spec: comments and key order of the
  originally applied YAML are not preserved.

---

## appsource

Manage GitOps [AppSources](resources.md#appsource). Create/update an AppSource
with `apply`; the commands below operate on existing ones.

### appsource sync

```
unified-cd appsource sync <name>
```

Forces a re-sync: resets the AppSource's `lastCommit` so the next reconciler
tick (≤30s) re-syncs from Git. Returns immediately — it does not wait for the
sync to finish. This is the CLI equivalent of `POST /api/v1/appsources/<name>/sync`.

```bash
unified-cd appsource sync my-pipelines
# => appsource sync scheduled: my-pipelines
```

### appsource list

```
unified-cd appsource list
```

```bash
unified-cd appsource list
# => my-pipelines   repo=https://github.com/acme/pipelines rev=main lastCommit=abc1234
```

### appsource get

```
unified-cd appsource get <name>
```

Shows repoURL, targetRevision, path, last synced commit, and last sync time.

### appsource delete

```
unified-cd appsource delete <name>
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
unified-cd webhook list
```

Lists registered webhook receivers.

```
github-push    (2026-06-01)
gitlab-push    (2026-07-01)
```

### webhook delete

```
unified-cd webhook delete <name>
```

Deletes a webhook receiver. Payloads sent to its `/webhook/<name>` URL will
return 404 afterwards.

```bash
unified-cd webhook delete github-push
# => webhook receiver "github-push" deleted
```

---

## login

Authenticate using OIDC (SSO) device flow and save the token to the config file.
`--server` is required (or set `UNIFIED_SERVER`).

```
unified-cd login --server <url>

  --server     string   Controller server URL (required)
  --issuer     string   OIDC issuer URL (auto-discovered from server if omitted)
  --client-id  string   OIDC device flow client ID (auto-discovered if omitted)
```

```bash
unified-cd login --server http://unified-cd.example.com
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
unified-cd agent install --server <url> --token <token> --id <id> [OPTIONS]

  --server  string   Controller URL (required)
  --token   string   Agent bearer token (required)
  --id      string   Agent identifier (required)
  --label   string   Agent label (repeatable, e.g. --label kind:linux)
  --dir     string   Installation directory (default: ~/.unified-cd)
```

Installs the agent as a system service:
- **Linux**: writes a `systemd` unit file to `<dir>/systemd/unified-cd-agent.service`
- **macOS**: writes a `launchd` plist to `<dir>/launchd/dev.unified-cd.agent.plist`
- **Windows**: prints manual Task Scheduler instructions

```bash
unified-cd agent install \
  --server http://unified-cd.example.com \
  --token my-agent-token \
  --id worker-01 \
  --label kind:linux \
  --label env:prod \
  --dir /opt/unified-cd
```

### agent list

```
unified-cd agent list
```

Lists all registered agents and their status.

```
agent-1    ci-worker-01   linux   kind:linux,env:ci   2026-06-20 10:55
agent-2    ci-worker-02   linux   kind:linux,env:ci   2026-06-20 10:54
k8s-1      k8s-node-1     linux   kind:k8s            2026-06-20 10:55
```

### agent get

```
unified-cd agent get <agent-id>
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
unified-cd agent runs <agent-id>
```

Lists recent runs claimed by the agent (most recent 50).

```
run-abc123   build    Succeeded   2026-06-20 10:00   manual
run-def456   deploy   Running    2026-06-20 11:00   schedule:nightly
```

---

## Configuration File

Default path: `~/.config/unified-cd/config.yaml`

```yaml
server: http://localhost:8080
token: dev-secret
agentId: ""
```

`token` holds either a manually-configured bearer token or the id_token written by
`login` (OIDC device flow or PAT prompt) — both use the same field. `agentId` is
populated only when this config file was written by `agent install`.

Override the path with `--config /path/to/config.yaml`.

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
