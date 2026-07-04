# Getting Started with unified-cd

This guide walks you through installing unified-cd, running your first job, and exploring the core features.

## Prerequisites

- **Go 1.24+** — to build from source
- **Docker** — to run PostgreSQL locally
- **Git** — for source checkout

---

## 1. Build

```bash
git clone https://github.com/your-org/unified-cd
cd unified-cd
make build
# Produces:
#   bin/unified-cd-controller
#   bin/unified-cd-agent
#   bin/unified-cd-k8s-agent
#   bin/unified-cd            (CLI)
```

---

## 2. Start PostgreSQL

```bash
docker compose -f docker-compose.dev.yaml up -d
```

The dev compose file starts a PostgreSQL instance at `localhost:5432` with:
- database: `unified`
- user/password: `unified` / `unified`

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
UNIFIED_SERVER="http://localhost:8080" \
UNIFIED_AGENT_TOKEN="dev-secret" \
UNIFIED_AGENT_ID="agent-1" \
./bin/unified-cd-agent
```

The agent registers itself with the controller and starts polling for jobs.

**Key configuration options:**

| Environment variable | Description |
|---|---|
| `UNIFIED_AGENT_SERVER` | Controller URL |
| `UNIFIED_AGENT_TOKEN` | Bearer token (must match controller's `UNIFIED_TOKEN` or a PAT) |
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
./bin/unified-cd jobs list
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
  steps:
    - name: greet
      run: echo "Hello from unified-cd!"
    - name: info
      needs: [greet]
      run: uname -a
```

Apply it, trigger a run, and follow the logs:

```bash
./bin/unified-cd apply -f hello.yaml

RUN_ID=$(./bin/unified-cd run trigger hello)
./bin/unified-cd logs -f "$RUN_ID"
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
      needs: [build]
      run: echo "Pushing {{ .Steps.build.Outputs.image_ref }}"
```

```bash
./bin/unified-cd apply -f build.yaml
./bin/unified-cd run trigger build --param image=myapp --param tag=v1.0
```

---

## 8. Routing Jobs to Specific Agents

Use `agentSelector` to route jobs to agents with particular labels.

Start a second agent with different labels:

```bash
UNIFIED_SERVER="http://localhost:8080" \
UNIFIED_AGENT_TOKEN="dev-secret" \
UNIFIED_AGENT_ID="docker-agent" \
UNIFIED_AGENT_LABELS="kind:docker,env:ci" \
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
./bin/unified-cd secret set REGISTRY_PASS "s3cr3t"
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
./bin/unified-cd apply -f nightly.yaml
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
./bin/unified-cd secret set GITHUB_WEBHOOK_SECRET "$(openssl rand -hex 20)"
./bin/unified-cd apply -f webhook.yaml
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
| Authentication (static token, PAT, OIDC SSO) | [Authentication Guide](authentication.md) |
| Agent labels and routing | [Agent Labels and Routing](agents.md) |
| Secrets management and encryption model | [Secrets Management Guide](secrets.md) |
| Kubernetes pod-based agents | [Kubernetes Integration Guide](kubernetes-integration.md) |
| High availability and rolling deploys | [High Availability Guide](high-availability.md) |
| Frontend development | [Frontend Development Guide](frontend-development.md) |
| VS Code YAML completion extension | [VS Code Extension](../editors/vscode/README.md) |
| Kubernetes install manifests | [Kubernetes Manifests](../manifests/README.md) |
| Field-level schema reference (auto-generated) | [Field Reference](field-reference.md) |
