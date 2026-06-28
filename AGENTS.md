# unified-cd — Agent Instructions

## Language

**All code, comments, commit messages, documentation, and any text written in this repository must be in English.** This includes:
- Source code comments and docstrings
- Markdown documentation files
- Commit messages and PR descriptions
- YAML field descriptions and inline comments
- Log messages and error strings in code
- Planning and specification documents

Do not write in Japanese or any other language.

## Feature Development Workflow

**Always use a git worktree when implementing a new feature or fixing a non-trivial bug.**

```bash
# Create an isolated worktree for the feature
git worktree add ../unified-cd-<feature-name> -b <feature-name>

# Work in the worktree
cd ../unified-cd-<feature-name>
# ... implement the feature ...

# When done, merge and clean up
git worktree remove ../unified-cd-<feature-name>
```

This keeps the main working tree clean and allows parallel development without branch-switching side effects.

## Project Overview

unified-cd is an open-source CI/CD tool (Jenkins alternative) written in Go. Key components:

- **controller** — Job scheduling and management server (PostgreSQL-backed)
- **agent** — Worker that executes job steps on various platforms (Linux, macOS, Windows, Kubernetes)
- **CLI** (`unified-cd`) — Apply YAML job definitions, trigger runs, and stream logs
- **Web UI** — Svelte + Vite frontend served by the controller

Jobs are defined in a Kubernetes-style YAML format (`kind: Job`) and applied via `unified-cd apply -f job.yaml`.

## Architecture

- Jobs and Runs persist in PostgreSQL; logs and artifacts in S3-compatible object store (Garage)
- Controller is stateless — multiple replicas behind a load balancer for HA
- Leader election via PostgreSQL advisory locks (`pg_try_advisory_lock`)
- Agent ↔ controller communication via long-polling HTTP; log streaming via SSE (LISTEN/NOTIFY)

## Repository Layout

```
cmd/
  unified-cd-controller/   # controller binary
  unified-cd-agent/        # standard agent
  unified-cd-k8s-agent/    # Kubernetes pod agent
  unified-cli/             # CLI (unified-cd)
internal/
  controller/              # HTTP handlers, scheduler, background workers
  agent/                   # step executor, log pusher
  dsl/                     # YAML DSL types and validation
  store/                   # PostgreSQL data layer
web/                       # Svelte + Vite frontend
editors/vscode/            # VS Code YAML completion extension
manifests/                 # Kubernetes install manifests
docs/                      # User-facing documentation
examples/                  # Example job YAML files
```

## Privacy

**Never include personally identifiable information (PII) in any file in this repository.** This includes:
- Real names (full name, first name, last name, or username) in file paths, comments, or documentation
- Personal email addresses
- Home directory paths containing a real person's name or username (e.g. `/Users/john.doe/...`, `/home/jane/...`)
- GitHub usernames or profile URLs that identify a specific individual

When writing paths in plans, specs, or documentation, use generic placeholders instead:
- Use `/path/to/unified-cd` instead of an absolute path tied to a person's machine
- Use `your-username` or `<username>` when a placeholder username is needed
- Use `example@example.com` for any example email address

## Development

```bash
docker compose -f docker-compose.dev.yaml up -d   # start PostgreSQL
make build                                         # build all binaries
make test                                          # full test suite (requires Docker)
make test-short                                    # skip integration tests
make dev-go                                        # hot-reload controller (requires air)
make dev-ui                                        # Vite dev server for the UI
```
