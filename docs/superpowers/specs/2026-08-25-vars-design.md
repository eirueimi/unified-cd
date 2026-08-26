# Variables: global `Vars` and job-level `spec.vars` — Design

Date: 2026-08-25
Status: Approved (design); implementation plan to follow

## 1. Purpose

There is no way to say a value once and use it everywhere. A registry host, a
language version, an application name — anything shared across jobs, or across
the steps of one job — must be repeated in every `run:` that needs it, or
smuggled in as a secret, which is the wrong tool for a value that is not
sensitive.

Add two layers of plain-text variables:

- **Global** — a `kind: Vars` manifest, applied and synced like a Job, so the
  values live in Git.
- **Job-level** — `spec.vars` on a Job, shared by every step in it.

Variables arrive two ways at once, because both are useful and neither
subsumes the other: as environment variables on every step, and as
`{{ .Vars.KEY }}` in templates. This mirrors GitLab CI's `variables:`.

## 2. Scope

In scope:

- `kind: Vars`, applied with `unified-cli apply` and synced by AppSource.
- `spec.vars` on a Job.
- Injection into every step's environment, and `{{ .Vars.KEY }}` in templates.
- Validation at apply time: key syntax, cross-manifest collisions, reserved
  names.
- A parity scenario, so the feature cannot silently diverge between backends.
- User documentation, including when to use a variable and when to use a
  secret.

Out of scope:

- **Secret values.** Variables are plain text, stored and returned in the
  clear, and printed in logs like any other environment variable. Secrets keep
  their existing path. The documentation must say this in the first paragraph a
  reader sees, not in a footnote.
- **Templated variable values.** `REGISTRY: {{ .Params.x }}` is not resolved in
  v1. Values are literals. Nothing about this design forecloses adding it later.
- **Namespacing or path-scoped variables.** unified-cd has no real namespaces —
  `team-a/build` is a qualified name derived from a file path, not an isolation
  boundary. Scoping variables to a path prefix would invent a boundary the rest
  of the system does not have.
- **Web UI surfacing.** Not requested. Listing variables, or showing which jobs
  reference them, is its own task.

## 3. The two manifests

A global manifest, applied and synced like any other resource:

```yaml
kind: Vars
metadata:
  name: org-defaults
spec:
  vars:
    REGISTRY: ghcr.io/myorg
    GO_VERSION: "1.24"
```

And the job-level layer:

```yaml
kind: Job
spec:
  vars:
    APP_NAME: myapp
  steps:
    - name: build
      run: docker build -t {{ .Vars.REGISTRY }}/$APP_NAME .
```

The example is deliberate: `{{ .Vars.REGISTRY }}` and `$APP_NAME` are the same
value reaching the step by the two different routes.

### 3.1 Precedence

Nearest scope wins:

```
step env:  >  job spec.vars:  >  global Vars
```

A job overriding a global key is normal and silent — that is what "nearest
wins" means, and warning about it would train operators to ignore warnings.

## 4. Validation

All of this is rejected at apply time, where the author is present to read the
error, rather than at claim time, where nobody is.

**Key syntax.** `[A-Za-z_][A-Za-z0-9_]*`. The values become environment
variables; a key that is not a valid variable name has no useful behaviour to
fall back on.

**Cross-manifest collisions.** Two `Vars` manifests defining the same key is an
error naming both manifests. The alternative is a last-writer-wins rule whose
outcome depends on apply order, which is a debugging problem disguised as a
feature. Note this is a *global-versus-global* rule; a job overriding a global
is the precedence rule above and is fine.

**Reserved names.** Two sets are refused:

- The agent's own credentials, already listed in `internal/agent/stepenv.go`'s
  `stepEnvDenied`: `UNIFIED_CACHE_KEY`, `UNIFIED_CACHE_SECRET`,
  `UNIFIED_TOKEN`, `UNIFIED_AGENT_CREDENTIAL_FILE`,
  `UNIFIED_AGENT_ENROLLMENT_TOKEN_FILE`. That list exists because leaking them
  lets a job author act as the agent.
- `PATH` and `HOME`. This is a judgment call and not in the original sketch, so
  the reasoning is worth stating: a global `Vars` manifest applies to *every
  step of every job*, and a `PATH` that shadows the agent's baseline breaks all
  of them at once, in a way whose cause is not obvious from the failure. The
  cost of refusing is that nobody can set `PATH` globally, which they should
  not want to do; the cost of allowing it is a footgun with the widest possible
  blast radius.

`stepEnvDenied` remains the runtime backstop regardless of apply-time
validation — a run created before this validation existed must not be able to
inject a credential name.

## 5. How a variable reaches a step

The merge happens once, when the controller builds a claim
(`buildClaimResponse`), and the result travels on the claim as
`Vars map[string]string` — the same shape as the existing `Params`.

From there the agent side is already backend-agnostic, and that is the property
this design is built around:

- **Template data.** `TemplateData` gains `Vars map[string]string`
  (`internal/dsl/template.go`), populated in the shared orchestrator alongside
  `Params`.
- **Step environment.** The shared orchestrator lays the claim's vars down
  first and lets the step's own `env:` overwrite them, which is the precedence
  rule expressed as an ordering.

Both happen in `internal/agent/orchestrator.go`, above the `ExecBackend` seam.
Neither the host backend nor the Kubernetes backend learns anything about
variables. **This is the design property to protect**, and it is why a parity
scenario is part of the work rather than an optional extra: if a future change
moves any of this below the seam, the two agents can diverge, and the parity
suite is what says so.

### 5.1 Timing

The merge is at claim assembly. A change to a `Vars` manifest affects runs
claimed after it, and does not disturb a run already in flight. Deleting a
manifest is the same mechanism: subsequent claims are built without those keys.

## 6. Storage and lifecycle

`Vars` is persisted, so it follows the `Schedule` and `WebhookReceiver`
pattern: a table, a migration, and Upsert/List/Delete in `internal/store`, with
`case "Vars"` added to the CLI's apply dispatch and to the controller's
AppSource apply and delete paths.

It is explicitly **not** modelled on `JobTemplate`, which looks superficially
similar and is not persisted at all — it is resolved at `uses:` time by the git
template layer and never enters the store. Following it would leave `Vars` with
nowhere to live.

One detail that has bitten this codebase before: a new field on `Spec` needs
**both** a `yaml` and a `json` tag. The store persists the spec as JSON and
reads it back, so a yaml-only tag round-trips to nothing. The `Detached` field
carries a comment saying exactly this; `spec.vars` needs the same treatment.

## 7. Verification

1. Applying a `Vars` manifest with an invalid key, a colliding key, or a
   reserved name fails with an error naming the key — and, for a collision,
   both manifests.
2. A step's `run:` sees a global variable as an environment variable, and
   `{{ .Vars.KEY }}` resolves to the same value.
3. Precedence holds in both directions: a job `spec.vars` beats a global of the
   same name, and a step `env:` beats both.
4. A variable named after an agent credential never reaches a step, even if a
   run was created before the apply-time validation existed.
5. The parity scenario passes on both the host and the Kubernetes agent —
   asserting the same expectation, from one `Case`.
6. Deleting a `Vars` manifest removes its keys from subsequently built claims,
   and does not affect a run already claimed.
7. `go generate ./...` regenerates `schemas/unified-cd.schema.json` and the
   field reference with `Vars` present, and the diff is otherwise empty.
8. `go build ./...` and `go test ./... -short -count=1` pass.

## 8. Staging

Ordered so each stage is provable before the next depends on it:

1. **The type and its validation.** `kind: Vars` parsing, `spec.vars` on Job,
   key syntax, collisions, reserved names. No storage, no delivery — pure
   functions with table tests.
2. **Storage.** Table, migration, Upsert/List/Delete, and the apply paths (CLI
   and AppSource).
3. **Delivery.** The claim field, the merge in `buildClaimResponse`, and the
   orchestrator's template data and step env. This is the stage the parity
   scenario belongs to.
4. **Generated artefacts and documentation.** Schema and field-reference
   regeneration, and the user guide.

## 9. Decisions left open, and what was decided about them

- **The kind's name.** `Vars` is what was proposed and not objected to. It
  stays. `Variables` would read slightly better beside `kind: Job`, but the
  field is `vars:` either way and two names for one concept is worse than one
  imperfect name.
- **Deleting a manifest.** Subsequent claims are built without those keys. This
  is not a special rule; it is what falls out of merging at claim assembly, and
  it is recorded here only so nobody implements a tombstone.
- **A job overriding a global.** Silent, per §3.1.
