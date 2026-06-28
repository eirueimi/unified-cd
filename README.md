# unified-cd

An open-source CI/CD tool (Jenkins alternative) written in Go.

**GitHub:** https://github.com/eirueimi/unified-cd

**Key features:** YAML-defined jobs · DAG step execution · Multi-platform agents (Linux, macOS, Windows, Kubernetes) · Secrets management · Webhook and cron triggers · High availability · Web UI · OIDC SSO

---

## Installation

### Docker (recommended for production)

```bash
# Controller
docker pull ghcr.io/eirueimi/unified-cd-controller:latest

# Kubernetes agent
docker pull ghcr.io/eirueimi/unified-cd-k8s-agent:latest
```

Images are published to [GitHub Container Registry](https://github.com/eirueimi/unified-cd/pkgs/container/unified-cd-controller) on every `v*` tag for `linux/amd64` and `linux/arm64`.

### Kubernetes

```bash
# Full install (controller + k8s-agent + PostgreSQL)
kubectl apply -f https://raw.githubusercontent.com/eirueimi/unified-cd/main/manifests/install.yaml

# k8s-agent only (connect to existing controller)
kubectl apply -f https://raw.githubusercontent.com/eirueimi/unified-cd/main/manifests/agent-only.yaml
```

### Binaries

Pre-built binaries for Linux, macOS, and Windows (amd64/arm64) are available on the [Releases page](https://github.com/eirueimi/unified-cd/releases):

```bash
# Example: Linux amd64
curl -L https://github.com/eirueimi/unified-cd/releases/latest/download/unified-cli_linux_amd64.tar.gz | tar xz
sudo mv unified-cli /usr/local/bin/
```

---

## Quick Start

### Requirements

- Go 1.26+
- Docker (for PostgreSQL)

### Build and run

```bash
# 1. Build
make build

# 2. Start PostgreSQL
docker compose -f docker-compose.dev.yaml up -d

# 3. Start controller (terminal 1)
UNIFIED_DB_DSN="postgres://unified:unified@localhost:5432/unified?sslmode=disable" \
UNIFIED_TOKEN="dev-secret" \
./bin/unified-cli-controller --addr :8080

# 4. Start agent (terminal 2)
UNIFIED_SERVER="http://localhost:8080" \
UNIFIED_AGENT_TOKEN="dev-secret" \
UNIFIED_AGENT_ID="agent-1" \
./bin/unified-cli-agent

# 5. Configure CLI
mkdir -p ~/.config/unified-cd
cat > ~/.config/unified-cd/config.yaml <<EOF
server: http://localhost:8080
token: dev-secret
EOF

# 6. Run your first job
cat > /tmp/hello.yaml <<EOF
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: hello
spec:
  steps:
    - name: greet
      run: echo hello-from-unified-cd
EOF

./bin/unified-cli apply -f /tmp/hello.yaml
RUN_ID=$(./bin/unified-cli run trigger hello)
./bin/unified-cli logs -f "$RUN_ID"
```

### Tests

```bash
make test        # full test suite (requires Docker)
make test-short  # skip integration tests
```

---

## Architecture

```
CLI / Browser / Webhook
        │
        ▼
┌─────────────────┐     ┌───────────────┐
│   Controller    │────►│  PostgreSQL   │  jobs, runs, queue, secrets, sessions
│   (stateless)   │     └───────────────┘
│   N replicas    │     ┌───────────────┐
│   behind LB     │────►│  S3 / Garage  │  log archives, artifacts, git template cache
└────────┬────────┘     └───────────────┘
         │ HTTP long-poll
         ▼
┌────────────────────────────────────────┐
│  Agents                                │
│  ┌──────────┐  ┌──────────┐  ┌──────┐ │
│  │  Linux   │  │  Windows │  │  k8s │ │  execute job steps
│  └──────────┘  └──────────┘  └──────┘ │
└────────────────────────────────────────┘
```

- **Controller** — stateless HTTP server; schedules and dispatches jobs; manages all resources
- **Agent** — connects to controller via long-polling; executes job steps in a workspace directory
- **k8s-agent** — Kubernetes-native agent; creates a Pod per job and exec's steps inside it
- **CLI** — `unified-cd` — apply YAML, trigger runs, stream logs, manage secrets and tokens

---

## Documentation

### Getting Started
- **[Getting Started Guide](docs/getting-started.md)** — installation, first job, parameters, secrets, schedules, webhooks

### Core References
- **[Job Reference](docs/jobs.md)** — complete Job YAML guide: steps, DAG, conditions, concurrency, artifacts, cache, templates
- **[Resource Reference](docs/resources.md)** — schema for all resource kinds: Job, Schedule, WebhookReceiver, GitCredential, AppSource
- **[CLI Reference](docs/cli.md)** — all commands and flags
- **[Configuration Reference](docs/configuration.md)** — all environment variables and config file options for controller, agent, and k8s-agent
- **[Field Reference](docs/field-reference.md)** — auto-generated field-level schema reference

### Feature Guides
- **[Authentication Guide](docs/authentication.md)** — static tokens, PATs, OIDC SSO (Dex), CLI login
- **[Secrets Management Guide](docs/secrets.md)** — create, reference, and encrypt secrets; log masking
- **[Agent Labels and Routing](docs/agents.md)** — agentSelector, hostname labels, Windows agents
- **[Kubernetes Integration Guide](docs/kubernetes-integration.md)** — k8s-agent setup, podTemplate patterns, RBAC
- **[High Availability Guide](docs/high-availability.md)** — controller redundancy, leader election, rolling deploys
- **[Frontend Development Guide](docs/frontend-development.md)** — Svelte + Vite setup, hot reload, routing

### Infrastructure
- **[Kubernetes Manifests](manifests/README.md)** — install manifests for production and evaluation
- **[VS Code Extension](editors/vscode/README.md)** — YAML completion and validation for unified-cd files

---

## Resource Kinds

| Kind | Description |
|---|---|
| `Job` | Defines a pipeline: steps, parameters, concurrency rules, agent routing |
| `Schedule` | Triggers a job on a cron schedule |
| `WebhookReceiver` | Accepts HTTP webhooks (GitHub, HMAC, or unauthenticated) to trigger a job |
| `GitCredential` | Stores Git authentication for private repo access |
| `AppSource` | GitOps sync: automatically applies Job YAML files from a Git repository |

```yaml
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: example
spec:
  params:
    inputs:
      - name: env
        type: string
        default: staging
  agentSelector:
    - kind:linux
  steps:
    - name: build
      run: make build
    - name: test
      needs: [build]
      run: make test
    - name: deploy
      needs: [test]
      if: '{{ eq .Params.env "production" }}'
      run: make deploy
```

---

## Development

```bash
make build          # build all binaries
make test           # full test suite (requires Docker)
make test-short     # unit tests only
make dev-go         # hot-reload controller with air
make dev-ui         # Vite dev server for frontend
make ui-build       # build Svelte frontend assets (served by controller at runtime)
make manifests      # regenerate Kubernetes install manifests
make vscode-package # build VS Code extension .vsix
```
