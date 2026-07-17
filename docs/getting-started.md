# Getting Started with unified-cd

This guide walks you through installing unified-cd, running your first job, and exploring the core features.

## Prerequisites

- **Go 1.24+** — to build from source
- **Docker** — to run PostgreSQL locally
- **Git** — for source checkout

Separately, jobs are isolated by default: an unmarked job runs its steps inside a container, so
the **agent host** also needs a container runtime (docker, podman, or nerdctl) to run jobs —
unless a job opts out with `spec.native: true`. See [Job Isolation: `native` and the claim
pod](jobs.md#job-isolation-native-and-the-claim-pod) and the [job-isolation migration
guide](migration-2026-07-job-isolation.md) for the full model. This guide's examples use
`native: true` so you can follow along without installing a runtime first — see the callout in
step 6.

---

## 1. Build

```bash
git clone https://github.com/your-org/unified-cd
cd unified-cd
make build
# Produces:
#   bin/unified-cd-controller
#   bin/unified-cd-agent
#   bin/unified-cli           (CLI)
#
# The k8s-agent is not built by `make build`; build it separately:
#   go build -o bin/unified-cd-k8s-agent ./cmd/k8s-agent
# or use its Docker image.
```

---

## 2. Start the Stack (Docker Compose)

The repo ships a full hot-reload stack (`docker-compose.yaml`) with PostgreSQL, Garage
(S3-compatible storage), the controller (via `air`), Vite, Dex, and an agent:

```bash
cp .env.example .env
docker compose up -d
```

This brings up PostgreSQL at `localhost:5432` (database/user/password: `unified`) along with
everything else needed to run the controller. If you'd rather build and run the controller and
agent from source manually (steps 3-4 below), you only need PostgreSQL from this stack —
run `docker compose up -d postgres` instead.

---

## 3. Start the Controller

```bash
UNIFIED_DB_DSN="postgres://unified:unified@localhost:5432/unified?sslmode=disable" \
UNIFIED_TOKEN="dev-secret" \
./bin/unified-cd-controller --addr :8080
```

The controller runs database migrations on startup. When ready you'll see:

```
INFO  server listening  addr=:8080
```

**Key configuration options** (see [Configuration Reference](configuration.md) for the full list):

| Environment variable | Description | Required |
|---|---|---|
| `UNIFIED_DB_DSN` | PostgreSQL connection string | Yes |
| `UNIFIED_TOKEN` | Static bearer token for the admin CLI | Yes (when SSO is not configured) |
| `UNIFIED_CONTROLLER_KEY` | 32-byte hex key for secret encryption | Recommended |
| `UNIFIED_S3_ENDPOINT` | S3/Garage endpoint for log archival and artifacts | Optional |

---

## 4. Start an Agent

Open a second terminal:

```bash
agent_state_dir="$HOME/.local/state/unified-cd-agent"
install -d -m 0700 "$agent_state_dir"

UNIFIED_SERVER="http://localhost:8080" \
UNIFIED_TOKEN="dev-secret" \
./bin/unified-cli agent enrollment create \
  --agent-id agent-1 \
  --label kind:linux \
  --output-file "$agent_state_dir/enrollment.token"

UNIFIED_SERVER="http://localhost:8080" \
UNIFIED_AGENT_ID="agent-1" \
UNIFIED_AGENT_CREDENTIAL_FILE="$agent_state_dir/credentials.json" \
UNIFIED_AGENT_ENROLLMENT_TOKEN_FILE="$agent_state_dir/enrollment.token" \
./bin/unified-cd-agent
```

The enrollment command uses the controller's **administrator CLI credential**
only to create a one-time VM enrollment file. It does not configure that token
on the agent. For a non-local deployment, use an HTTPS controller URL and a
securely supplied admin PAT instead of the development value shown here.
`install -d -m 0700` creates the private parent directory required by the
enrollment writer; it is safe to rerun and keeps the credential out of process
arguments.

The agent registers itself with the controller and starts polling for jobs.

**Key configuration options:**

| Environment variable | Description |
|---|---|
| `UNIFIED_SERVER` | Controller URL |
| `UNIFIED_AGENT_CREDENTIAL_FILE` | Private file where the agent keeps its rotating VM refresh credential |
| `UNIFIED_AGENT_ENROLLMENT_TOKEN_FILE` | Private one-time enrollment credential file, required only before first enrollment |
| `UNIFIED_AGENT_TOKEN` | Legacy shared-token migration only; never set it from `UNIFIED_TOKEN` for a new agent |
| `UNIFIED_AGENT_ID` | Unique agent identifier |
| `UNIFIED_AGENT_LABELS` | Comma-separated labels, e.g. `kind:linux,env:prod` |

---

## 5. Configure the CLI

```bash
mkdir -p ~/.config/unified-cd
cat > ~/.config/unified-cd/config.yaml <<EOF
server: http://localhost:8080
token: dev-secret
EOF
```

Verify the connection:

```bash
./bin/unified-cli jobs list
# (empty — no jobs yet)
```

---

## 6. Your First Job

Create a job file:

```yaml
# hello.yaml
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: hello
spec:
  native: true   # see callout below
  steps:
    - name: greet
      run: echo "Hello from unified-cd!"
    - name: info
      run: uname -a
```

> **Why `native: true`?** Jobs are isolated by default — steps run inside a container (a
> per-claim "claim pod" on the standard agent, mirrored by a real Pod on the k8s-agent).
> `native: true` runs steps directly on the agent host instead, which is what lets this
> quickstart work without a container runtime installed. Remove `native: true` (and install
> docker/podman/nerdctl) to get the default isolated behavior — see [Job Isolation: `native` and
> the claim pod](jobs.md#job-isolation-native-and-the-claim-pod).

Steps run sequentially in the order listed. To run steps concurrently, group them under a
`parallel:` block instead (see [Concurrent Steps (`parallel`)](jobs.md#concurrent-steps-parallel)).

Apply it, trigger a run, and follow the logs:

```bash
./bin/unified-cli apply -f hello.yaml

RUN_ID=$(./bin/unified-cli run trigger hello)
./bin/unified-cli logs -f "$RUN_ID"
```

Expected output:

```
Hello from unified-cd!
Linux ... (your kernel info)
```

---

## 7. Job with Parameters

Jobs can declare typed input parameters and pass values between steps.

```yaml
# build.yaml
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: build
spec:
  native: true   # as in step 6 — runs without a container runtime
  params:
    inputs:
      - name: image
        type: string
        required: true
      - name: tag
        type: string
        default: latest
    outputs:
      - name: image_ref
        type: string
  steps:
    - name: build
      run: |
        echo "Building {{ .Params.image }}:{{ .Params.tag }}"
      outputs:
        image_ref: "{{ .Params.image }}:{{ .Params.tag }}"

    - name: push
      run: echo "Pushing {{ .Steps.build.Outputs.image_ref }}"
```

```bash
./bin/unified-cli apply -f build.yaml
./bin/unified-cli run trigger build --param image=myapp --param tag=v1.0
```

---

## 8. Routing Jobs to Specific Agents

Use `agentSelector` to route jobs to agents with particular labels.

Start a second agent with different labels:

```bash
agent_state_dir="$HOME/.local/state/unified-cd-agent-docker"
install -d -m 0700 "$agent_state_dir"

UNIFIED_SERVER="http://localhost:8080" \
UNIFIED_TOKEN="dev-secret" \
./bin/unified-cli agent enrollment create \
  --agent-id docker-agent \
  --label kind:docker --label env:ci \
  --output-file "$agent_state_dir/enrollment.token"

UNIFIED_SERVER="http://localhost:8080" \
UNIFIED_AGENT_ID="docker-agent" \
UNIFIED_AGENT_LABELS="kind:docker,env:ci" \
UNIFIED_AGENT_CREDENTIAL_FILE="$agent_state_dir/credentials.json" \
UNIFIED_AGENT_ENROLLMENT_TOKEN_FILE="$agent_state_dir/enrollment.token" \
./bin/unified-cd-agent
```

Define a job that requires those labels:

```yaml
# docker-build.yaml
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: docker-build
spec:
  native: true   # as in step 6 — runs without a container runtime
  agentSelector:
    - kind:docker
  steps:
    - name: build
      run: docker build -t myapp .
```

Only agents with the `kind:docker` label will claim this job.

---

## 9. Secrets

Store sensitive values server-side and reference them in jobs:

```bash
./bin/unified-cli secret set REGISTRY_PASS "s3cr3t"
```

```yaml
# deploy.yaml
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: deploy
spec:
  steps:
    - name: login
      env:
        REGISTRY_PASS: "{{ secrets.REGISTRY_PASS }}"
      run: |
        echo "$REGISTRY_PASS" | docker login registry.example.com --password-stdin -u ci
```

Secrets are encrypted at rest (AES-256-GCM) and automatically masked in logs.

---

## 10. Scheduled Jobs

Run a job on a cron schedule:

```yaml
# nightly.yaml
apiVersion: unified-cd/v1
kind: Schedule
metadata:
  name: nightly-build
spec:
  cron: "0 2 * * *"   # 02:00 UTC every day
  job: build
  params:
    tag: nightly
```

```bash
./bin/unified-cli apply -f nightly.yaml
```

---

## 11. Webhook Triggers

Trigger jobs from external events (e.g. GitHub pushes):

```yaml
# webhook.yaml
apiVersion: unified-cd/v1
kind: WebhookReceiver
metadata:
  name: github-push
spec:
  trigger:
    job: build
  auth:
    type: github
    secretRef: GITHUB_WEBHOOK_SECRET
  filters:
    - '{{ eq .Payload.ref "refs/heads/main" }}'
  paramsMapping:
    tag: "{{ .Payload.after }}"
```

Store the shared secret first (any random string; it must match what you enter
on GitHub), then apply:

```bash
./bin/unified-cli secret set GITHUB_WEBHOOK_SECRET "$(openssl rand -hex 20)"
./bin/unified-cli apply -f webhook.yaml
# Webhook endpoint: POST http://localhost:8080/webhook/github-push
```

The `/webhook/<name>` endpoint takes no bearer token — it is authenticated by
the signature check alone, so it is safe to expose publicly.

### Configuring the webhook on GitHub

In the repository, open **Settings → Webhooks → Add webhook** and set:

| Field | Value |
|---|---|
| **Payload URL** | `https://<your-controller>/webhook/github-push` — the path segment is the receiver's `metadata.name` |
| **Content type** | `application/json` — **required** (see note below) |
| **Secret** | the exact same string you passed to `secret set GITHUB_WEBHOOK_SECRET` |
| **SSL verification** | leave enabled when serving over HTTPS |
| **Which events** | pick the events you want (e.g. "Just the `push` event") |

> **Content type must be `application/json`.** The receiver verifies GitHub's
> `X-Hub-Signature-256` over the raw request body and then parses that body as
> JSON. With `application/x-www-form-urlencoded`, GitHub sends the payload as
> `payload=<url-encoded JSON>` instead of raw JSON; the signature still matches,
> but JSON parsing fails and the delivery returns **400 `invalid JSON payload`**.

**Reachability:** GitHub's servers must be able to reach the Payload URL, so a
bare `http://localhost:8080` will not work. Expose the controller behind an
HTTPS reverse proxy / load balancer, or for local testing tunnel it with
something like `ngrok http 8080` and use the public URL ngrok prints.

**Verify the delivery:** GitHub's webhook page has a **Recent Deliveries**
section showing each attempt's request and response, with a **Redeliver**
button to retry. The controller's response codes:

| Response | Meaning |
|---|---|
| `200` + run JSON | run created |
| `204` | signature OK but a `filters` expression did not match — no run (e.g. a push to a non-`main` branch) |
| `400 invalid JSON payload` | body is not JSON — usually the wrong **Content type** (see note above) |
| `400 missing required param` | `paramsMapping` did not produce a required job input |
| `401 signature verification failed` | wrong **Secret**, or signature sent in an unexpected header |

See the [WebhookReceiver reference](resources.md#webhookreceiver) and
[Troubleshooting](troubleshooting.md#webhook-returns-401) for the full field
and error tables.

---

## 12. Web UI

Open the Web UI in your browser:

```
http://localhost:8080/ui/
```

The UI lets you:
- Browse jobs and their run history
- Trigger runs with a form-based UI (for jobs that declare `inputs`)
- Stream live logs
- View job YAML definitions
- Monitor agent status

---

## Next Steps

| Topic | Document |
|---|---|
| Complete Job YAML reference (all fields, concurrency, DAG, artifacts, cache) | [Job Reference](jobs.md) |
| CLI commands and flags | [CLI Reference](cli.md) |
| Environment variables and startup flags | [Configuration Reference](configuration.md) |
| Authentication (human PATs/SSO and per-agent credentials) | [Authentication Guide](authentication.md) |
| Shared-agent-token migration | [Migration: agent authentication](migration-agent-auth.md) |
| Agent labels and routing | [Agent Labels and Routing](agents.md) |
| Secrets management and encryption model | [Secrets Management Guide](secrets.md) |
| Kubernetes pod-based agents | [Kubernetes Integration Guide](kubernetes-integration.md) |
| High availability and rolling deploys | [High Availability Guide](high-availability.md) |
| Frontend development | [Frontend Development Guide](frontend-development.md) |
| VS Code YAML completion extension | [VS Code Extension](../editors/vscode/README.md) |
| Kubernetes install manifests | [Kubernetes Manifests](../manifests/README.md) |
| Field-level schema reference (auto-generated) | [Field Reference](field-reference.md) |
