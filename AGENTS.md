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

**Always create a git worktree before committing anything.** Never commit from the main working tree — cut an isolated worktree on a dedicated branch first, make your commits there, then integrate. This applies to every change that ends in a commit, not just large features.

```bash
# Create an isolated worktree BEFORE making any commit
git worktree add ../unified-cd-<change-name> -b <change-name>

# Work and commit inside the worktree
cd ../unified-cd-<change-name>
# ... make changes ...
git add -A && git commit -m "..."

# When done, merge and clean up
git worktree remove ../unified-cd-<change-name>
```

This keeps the main working tree clean and allows parallel development without branch-switching side effects.

## Docs, Examples, and Templates Hygiene

**After implementing any change to behavior, the DSL schema, CLI flags, or configuration, update `examples/` and `templates/` with the same rigor as `docs/` — a feature change is NOT done until all three are consistent with the code.** Docs that contradict the code are worse than no docs, and examples/templates ARE docs (they are the first thing users copy). The `kind: JobTemplate` migration (PR #54) shipped with `docs/` updated but every file in `templates/` still `kind: Job` and therefore broken — do not repeat that class of miss. Check each of these:

1. **`docs/*.md`** — grep for every field/flag/behavior you added, renamed, or removed. Beyond the obvious reference pages (`jobs.md`, `resources.md`, `agents.md`, `configuration.md`, `kubernetes-integration.md`), always check the narrative docs: **`getting-started.md`** (do the quickstart steps still work end-to-end for a new user?), **`troubleshooting.md`** (add symptom entries for any new failure mode, with the exact grep-able error string), and **`operations.md`** (any new operator responsibility, e.g. disk/container hygiene).
2. **Generated artifacts** — `docs/field-reference.md` and `schemas/unified-cd.schema.json` are BOTH generated; never hand-edit either. A new root resource kind must be added to `cmd/schemagen/main.go`'s `roots` AND `cmd/docgen/main.go`'s `rootKinds`, and its type definitions must live in `internal/dsl/types.go` or a `*_types.go` file (schemagen scans only those). Then run `go generate ./...` and commit the diff.
3. **`examples/**`** — every YAML must parse under the current parser for its `kind:` AND still make semantic sense under current defaults (a parsing example that fails at runtime is still broken). `internal/dsl`'s `TestExamplesParse` gate-checks parsing; semantics (e.g. an example instructing `apply` of something no longer registrable, or `call:`ing a job that no longer exists) you must check by reading. Also `examples/config/*.yaml` and every example README: check for removed/renamed fields, stale setup instructions, and stale comments.
4. **`templates/`** — files here are `kind: JobTemplate` (`uses:` targets; the documented standalone exception list lives in `internal/dsl/templates_parse_test.go`). `TestTemplatesParse` gate-checks every file against its intended schema — extend that test's expectations, never bypass it. Check template header comments (usage instructions) and `templates/README.md` against any schema/behavior change, and remember external references are PINNED (`@tag`/`@sha` point at old commits): a breaking template change needs a new tag and updated example refs.
5. **Breaking changes** — add or update a `docs/migration-*.md` guide with a before→after table and the exact validation-error strings users will see.
6. **`README.md`** — keep the feature summary in lockstep with the DSL.

A cheap final sweep: `grep -rn "<removed-field>" docs/ examples/ templates/ README.md` for every removed or renamed identifier — zero hits (outside historical specs/plans) before you finish. For a renamed/removed KIND or contract change, also `grep -rn "kind: <old>" examples/ templates/` and re-run `go test ./internal/dsl/ -run 'Templates|Examples'`.

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
