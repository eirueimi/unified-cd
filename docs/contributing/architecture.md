# Architecture

This page exists to get a new contributor oriented in about fifteen minutes:
what the pieces are, how a run flows through them, and which directory to open
for a given question.

It is deliberately a *map*, not a specification. Where a detail is load-bearing
and easy to break by accident, it is called out in [Invariants](invariants.md)
rather than buried here.

## The system in one picture

```text
                    +-------------------------------------------+
   unified-cli ---->|                CONTROLLER                 |
   Web UI      ---->|  HTTP API - scheduler - background        |
   webhooks    ---->|  workers - SSE log streaming              |
                    +-------+---------------------------+-------+
                            |                           |
                   +--------v--------+         +--------v--------+
                   |   PostgreSQL    |         |  Object store   |
                   | jobs, runs,     |         | (S3-compatible) |
                   | logs, secrets,  |         | log archives,   |
                   | agent registry  |         | artifacts,cache |
                   +--------^--------+         +--------^--------+
                            |                           |
                    +-------+---------------------------+-------+
                    |                 AGENTS                    |
                    |  claim runs, execute steps, ship logs     |
                    |                                           |
                    |  host agent            Kubernetes agent   |
                    |  (containers on        (Pods in a         |
                    |   the agent's host)     cluster)          |
                    +-------------------------------------------+
```

**The controller never executes user code.** It stores manifests, decides what
should run, and hands work to agents.

**Agents pull; the controller does not push.** An agent polls for work it is
eligible to claim. This is why an agent behind a firewall works, and why the
controller has no agent inventory beyond what agents tell it.

**Agents never talk to each other.** All coordination goes through the
controller's API and Postgres.

## The life of a run

This is the single most useful thing to hold in your head. Every state below is
a real value of `api.RunStatus` (`internal/api/types.go`).

```text
  Pending ---> Queued ---> Running ---> Succeeded
     |            |           |      \-> Failed
     |            |           |      \-> Cancelled
     |            |           \--- agent executes steps, ships logs
     |            \--- an agent claims it
     \--- git templates resolved, concurrency gates evaluated
```

1. **Created** — by `unified-cli`, a `Schedule`, a `WebhookReceiver`, or a
   `call:` step in another run. The run stores a *snapshot* of the job spec, so
   editing the job afterwards does not change a run that already exists.

2. **Pending** — the controller resolves what must be resolved before an agent
   can be offered the run: `uses:` git templates are fetched and inlined,
   concurrency groups and mutexes are evaluated. Driven by `RunGitResolver` and
   `RunScheduler` in `internal/controller/`.

3. **Queued** — eligible for claiming. An agent polls; `ClaimNextRun` hands it
   the run atomically. Selection matches agent labels and capabilities. A run
   no agent can claim is eventually failed by `RunQueuedRunReaper`, with a log
   line saying why.

4. **Running** — the agent executes the step DAG and streams logs back, while
   heartbeating. If it stops heartbeating, `RunStuckRunReaper` fails the
   orphaned run.

5. **Terminal** — after the main DAG the agent runs the `finally:` pipeline,
   drains `post:` and `cache:` hooks, and tears down scopes. Only then does it
   report the final status.

Afterwards, background workers archive the run's logs to the object store, trim
the database rows, and eventually delete expired runs entirely.

## Where things live

The top level splits by *what kind of thing it is*, not by feature.

| Path | What it is |
|---|---|
| `cmd/` | One directory per binary. Thin `main`s — logic lives in `internal/`. |
| `internal/` | All the actual code. Not importable from outside the module, by design. |
| `web/` | The Svelte Web UI, built and served by the controller. |
| `docs/` | This site. `mkdocs build --strict` must pass. |
| `schemas/` | The **generated** JSON Schema for manifests. Never hand-edited. |
| `examples/` | Real manifests, validated by tests against that schema. |
| `manifests/`, `deployments/` | Kubernetes install bundles and observability config. |
| `test/e2e/` | End-to-end tests that drive real binaries. |

### `cmd/` — the binaries

| Binary | Role |
|---|---|
| `controller` | The server: API, scheduler, background workers, Web UI. |
| `unified-cd-agent` | The **host agent**. Runs steps as containers on its own host. |
| `k8s-agent` | The **Kubernetes agent**. Runs steps as Pods in a cluster. |
| `unified-cli` | The user's CLI (`apply`, `trigger`, `export`, ...). |
| `unified-sidecar` | Runs *inside* a job Pod; moves artifacts and cache to and from the object store. |
| `ucd-sh` | A small shell shim injected into job containers so steps are interpreted the same way everywhere. |
| `shimgen`, `schemagen`, `docgen` | Generators. See [Generated artifacts](#generated-artifacts). |

### `internal/` — where to look for what

Sorted roughly by how often you will need them.

| Package | Open it when you are asking... |
|---|---|
| `controller/` | "What does the server do when...?" HTTP handlers (`api_*.go`), the scheduler, every background worker, SSE. The largest package. |
| `agent/` | "How does a run actually execute?" The orchestrator, step pipeline, retry, `finally:`, hooks, log shipping. **Also the host backend.** |
| `k8sagent/` | The Kubernetes backend: Pod building, scopes, the sidecar pump. |
| `dsl/` | The manifest language: types, parsing, validation, CEL `if:` conditions, template expansion. The authority on what a valid manifest is. |
| `store/` | Postgres: queries, migrations, and the `Store` interface. |
| `api/` | Wire types shared by controller, agents and CLI. Changing these is a compatibility decision. |
| `cli/` | `unified-cli` command implementations. |
| `objectstore/` | S3-compatible storage and credential providers. |
| `secrets/` | Encryption at rest, and the **log masker**. |
| `gittemplate/` | Fetching and inlining `uses:` templates from git. |
| `metrics/` | The Prometheus registry, recorders, and scrape-time collectors. |
| `config/` | Config-file parsing for controller and agents. |
| `runtime/`, `shim/` | Container-runtime abstraction, and the `ucd-sh` payload. |
| `paritycases/` | **Shared** scenarios proving both agents execute the DSL identically. |
| `schemakinds/` | Root manifest kinds, derived from the generated schema. |

!!! note "`internal/agent` is not only shared code"
    This is the most common orientation mistake. `internal/agent` holds *both*
    the backend-independent orchestrator *and* the host backend
    (`hostBackend`). The Kubernetes backend lives in `internal/k8sagent` and
    imports `internal/agent` for the shared half. See
    [The ExecBackend seam](#the-execbackend-seam).

## The seams that matter

### The ExecBackend seam

The single most important structural fact in the codebase.

```text
    internal/agent/orchestrator.go     <- shared: DAG order, retry, finally,
    internal/agent/pipeline.go            hooks, masking, log shipping
                  |
                  | reaches the machine only through...
                  v
    internal/agent/backend.go          <- the ExecBackend interface
                  |
        +---------+----------+
        v                    v
   hostBackend          k8sBackend
   (internal/agent)     (internal/k8sagent)
```

Everything about *how a job behaves* — the order steps run in, what `if:`
means, when `finally:` fires, how retries are counted — lives **above** the
interface, and is therefore identical on both backends by construction.
Everything about *where a process runs* lives below it.

When adding behaviour, ask first: **is this a property of the job, or of the
machine?** Job properties belong above the seam. Putting one below it means
implementing it twice, and the two copies will drift.

`internal/paritycases` exists to hold that line: one `Case` is run against both
backends by a driver in each package.

### The credential provider seam

`objectstore.S3Config.Creds` is an optional `*credentials.Credentials`. When it
is nil, `NewS3ObjectStore` falls back to a static key pair — which is exactly
what the controller and the host agent construct. That nil-fallback is what
lets the Kubernetes sidecar grow refreshable credentials without either of them
changing.

### The store interface

`internal/controller` talks to `store.Store`, an interface. `*store.Postgres`
implements it; `metrics.InstrumentedStore` decorates it to count run and step
transitions. Tests substitute behaviour by embedding the interface and
overriding one method — follow that pattern rather than writing a whole fake.

## Generated artifacts

Several committed files are build products. Editing them by hand is always
wrong; regenerate instead.

| Artifact | Generated from | Guard |
|---|---|---|
| `schemas/unified-cd.schema.json` | `internal/dsl` structs, via `cmd/schemagen` | `TestSchemaIsUpToDate` |
| `docs/reference/field-reference.md` | the same, via `cmd/docgen` | the same run |
| `internal/shim/embedded/ucd-sh-*` | `cmd/ucd-sh`, via `cmd/shimgen` | a source-hash check |
| `web/dist` | the Svelte app | the build |

Regenerate the schema and field reference with:

```bash
go generate ./internal/dsl/
```

!!! warning "Regenerate the shim on Linux only"
    The shim binaries are not byte-reproducible across host operating systems —
    the same source produces different bytes on Windows, macOS and Linux.
    Regenerating on the wrong platform produces a diff that looks like a real
    change and is not. See the package doc in
    `internal/shim/embedded/embed.go`.

**How schema `required` is decided.** `schemagen` derives it from the struct
tag: a field without `omitempty`, and not a pointer, becomes required. A new
optional field therefore needs `omitempty` — otherwise every existing manifest
becomes invalid in editors. That has already happened once, with `spec.params`.

## Database migrations

Numbered pairs in `internal/store/migrations/` (`NNN_name.up.sql` and
`.down.sql`), applied in order at controller startup. Each also gets a sentinel
in `internal/store/verify.go`, so a partially-migrated database is detected
rather than limped along. Copy the most recent migration as a template; both
directions must work.

## Where to go next

- [Invariants](invariants.md) — the rules that are load-bearing, and what broke
  when each was violated. **Read this before changing agent or controller
  internals.**
- [Frontend development](frontend-development.md) — the Svelte app.
- `docs/superpowers/specs/` — per-feature design documents, with the reasoning
  behind each decision.
