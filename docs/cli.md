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
- [login](#login)
- [agent](#agent)
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
# => Token (shown once): exc_xxxxxxxxxxxxxxxx
#    Name: ci-bot  ID: tok-abc123

# With expiry
unified-cd token create deploy-bot --expires-in 8760h
```

Use the printed token as a bearer token in CLI, API calls, or agent configuration.

### token list

```
unified-cd token list
```

Lists all tokens (names and IDs only; values are never retrievable).

```
tok-abc123   ci-bot           (2026-06-01)
tok-def456   env:UNIFIED_TOKEN  (2026-05-01)   ← bootstrap token from UNIFIED_TOKEN env var
```

### token delete

```
unified-cd token delete <id>
```

Revokes a token immediately.

```bash
unified-cd token delete tok-abc123
# => token "tok-abc123" revoked
```

---

## login

Authenticate using OIDC (SSO) device flow and save the token to the config file.

```
unified-cd login --server <url>

  --server     string   Controller server URL (required)
  --issuer     string   OIDC issuer URL (auto-discovered from server if omitted)
  --client-id  string   OIDC device flow client ID (auto-discovered if omitted)
```

```bash
unified-cd login --server http://unified-cd.example.com
# => Open this URL in your browser: https://...
# => Waiting for authentication...
# => Logged in. Token saved to ~/.config/unified-cd/config.yaml
```

After login, the id_token is stored in the config file and used automatically for subsequent commands.
id_tokens expire (Dex default: ~24 hours). Re-run `login` when your token expires.

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

---

## Configuration File

Default path: `~/.config/unified-cd/config.yaml`

```yaml
server: http://localhost:8080
token: dev-secret

# Token from OIDC login is also stored here:
# id_token: eyJhbGci...
```

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
