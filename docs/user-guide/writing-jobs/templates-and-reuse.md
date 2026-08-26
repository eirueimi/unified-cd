# Templates and Reuse

## Git Template Inlining (`uses`)

Inline steps from a job definition stored in a Git repository directly into the current run.

```yaml
steps:
  - name: lint
    uses:
      job: git://github.com/my-org/ci-templates/jobs/golangci-lint.yaml@v2.1.0
      with:
        target: ./...

  - name: security-scan
    uses:
      job: git://github.com/my-org/ci-templates/jobs/trivy.yaml@a1b2c3d4
      with:
        severity: HIGH,CRITICAL
```

**URI format:** `git://<host>/<owner>/<repo>/<path>@<ref>`

- `@v1.2.3` — recommended: pinned tag
- `@a1b2c3d4e5f6...` — pinned commit SHA
- `@main` — mutable branch (not cached; use with caution)

The referenced YAML file must be **`kind: JobTemplate`** — a dedicated,
strictly-scoped resource (not a full `Job`; see [The `JobTemplate`
schema](#the-jobtemplate-schema) below). Its steps are inlined at the point of
`uses`, and a non-scope template's `podTemplate` containers/volumes are merged
into the caller's pod (see [Pod-shape merge](#pod-shape-merge-containers-and-volumes)).

For private repositories, create a [GitCredential](../resources/git-credential.md#gitcredential) resource for the host.

`uses:` steps are resolved everywhere steps can appear, including inside
[`finally:`](approval-and-finally.md#finally-block-finally) — a `uses:` step in a `finally:` block is
inlined exactly like one in `steps:`.

An inlined step's `container:` (whether declared on the template's own step,
or inherited from the outer `uses` step) must resolve to a real target — the
primary container, a reserved name, or a container present in the merged
`podTemplate` — or run creation fails immediately with an error naming the
step and the container, instead of the step failing opaquely at exec time.

> **A `uses:`-free job's `container:` references are now validated at
> `apply` time, not just at run creation/exec.** Applying a plain job (no
> `uses:` step anywhere in `steps:`/`finally:`, and no named agent-side
> `podTemplate.name`) whose `container:` names something the job's own
> inline `podTemplate` doesn't define now fails immediately with the same
> `step "x" references container "y", which is not defined in the job's
> podTemplate` error — previously this only surfaced at run creation (for a
> uses-bearing spec) or opaquely at step-exec time. A spec that carries any
> `uses:` step, or a named agent-side `podTemplate.name` (its containers live
> in agent config, invisible at apply time), still defers this check to
> resolution/pod-build, since the template's pod-shape merge or the agent's
> template may supply the reference later. See [Job fails apply with a dangling `container:`
> reference](../../troubleshooting/templates-and-uses.md#job-fails-apply-with-a-dangling-container-reference)
> in Troubleshooting.

### The `JobTemplate` schema

`uses:` targets must be `kind: JobTemplate`. This is a **strict, deliberately
small** schema — only what `uses:` can honor, because a template's steps are
inlined directly into the caller's run and pod. Fields that would shape a
*different* pod, agent, or run (`agentSelector`, `concurrency`,
`timeoutMinutes`, `native`, `podTemplate.reuse` / `podTemplate.workspace` /
`podTemplate.override`, and any other pod-level key besides
`containers`/`volumes`) do not exist on this type at all — any field outside
the schema below is a **run-creation error naming the offending field**
(strict decode, same as a `Job` document). `spec.finally` **is** on the
schema — see [Template `finally:`](#template-finally-splice-into-the-caller)
below; it does not shape a different pod/agent/run the way the others would,
because it splices into the caller's own finally phase rather than running
anywhere of its own.

```yaml
apiVersion: unified-cd/v1
kind: JobTemplate
metadata:
  name: build-with-tools           # required
spec:
  description: builds with tools   # optional
  params:                          # optional: same Params shape as a Job
    inputs:
      - { name: target, type: string, default: all }
  shell: ["/bin/sh", "-c"]         # optional: template-level shell default
  podTemplate:                     # optional: pod-shape contribution only
    spec:
      containers:
        - { name: tools, image: alpine:3 }
      volumes:
        - { name: toolcache, emptyDir: {} }
  steps:                           # required, at least one
    - name: build
      container: tools
      run: make {{ .Params.target }}
  finally:                         # optional: spliced into the CALLER's finally
    - name: cleanup
      run: rm -rf /workspace/tmp
```

- `apiVersion` / `kind: JobTemplate` / `metadata.name` are required.
- `spec.steps` must contain at least one step.
- `spec.podTemplate.spec` accepts **only** `containers` and `volumes` — no
  other pod-level keys; every container/volume `name` must also be a valid
  [DNS-1123 label](isolation-and-containers.md#kubernetes-pod-template-podtemplate) (checked at apply
  time, same as a `Job`'s `podTemplate`).
- A job that needs its own pod, agent routing, or run semantics (its own
  `agentSelector`, `podTemplate.reuse`, etc.) doesn't fit this model — invoke
  it with [`call:`](#calling-other-jobs-call) instead of `uses:`.

A template step is an ordinary step: everything a `Job` step may declare —
including [`approval:`](approval-and-finally.md#approval-step-approval),
[`retry:`](steps.md#retry),
[`matrix:`/`foreach:`](steps.md#matrix-and-foreach-steps), `post:`, `cache:`
and `shell:` — is honored after inlining, subject only to the same rules the
caller's own steps obey (no `approval:` in `finally:`, and none of
`approval:`/`call:`/`container:` under a [`runsIn.image`
scope](#uses-level-runsinimage-scope)). Older controllers dropped `approval:`,
`retry:`, `matrix:` and `foreach:` from inlined steps without reporting it —
if you maintain a template that declares any of them, read [Migrating
templated `approval:`, `retry:`, `matrix:` and
`foreach:`](../../operator-manual/migrations/uses-template-step-fields.md).

A fetched template whose `kind` is `Job` (the old target kind) fails run
creation with:

```
uses: targets must be kind: JobTemplate (got kind: Job); convert the template, or invoke the job with call:
```

### `if:` on a `uses:` step

An `if:` on the `uses:` step itself gates the **whole expansion** — every
step the template inlines, not just the `uses:` step's own conceptual slot:

```yaml
steps:
  - name: notify
    if: failure()
    uses:
      job: git://github.com/my-org/ci-templates/jobs/slack-notify.yaml@v1
      with:
        channel: "#builds"
```

Here, `failure()` gates every step the `slack-notify.yaml` template expands
to — the synthetic inputs step, every inlined body step, every `parallel:`
sub-step, and the output-capture step — so the whole notification only runs
when an earlier step in the job has failed.

- **Combining with the template's own `if:`.** If an inlined step also
  declares its own `if:` (inside the template), the two are AND-combined:
  `(outer) && (inner)`. Both operands keep their own semantics — the outer
  expression is evaluated in the caller's context (unrewritten), the inner
  expression is evaluated after the template's usual `{{ .Params }}` /
  `.Steps.<name>.Outputs` reference rewriting. A step whose template `if:` is
  empty just inherits the outer `if:` verbatim; a `uses:` step with no `if:`
  leaves each inlined step's own `if:` untouched (unchanged from before).
- **Status-function semantics are identical to a plain step's `if:`.** See
  [Status Functions in `if:`](expressions.md#status-functions-in-if): an outer `failure()`
  makes the *combined* expression mention a status function, so the implicit
  `success()` requirement is overridden for the whole expansion, exactly as
  it would be for a single plain step with `if: failure()`.
- Before this, a `uses:` step's `if:` was accepted by the schema but silently
  dropped — the template always expanded unconditionally regardless of the
  `uses:` step's own `if:`. It is now honored.

### Pod-shape merge (containers and volumes)

In **non-scope** mode (no `runsIn.image` on the `uses:` step — see [Uses-level
`runsIn.image` (scope)](#uses-level-runsinimage-scope) below), a template's
`podTemplate.spec.containers` and `podTemplate.spec.volumes` are merged into
the **caller's** `podTemplate` so the inlined steps have somewhere to run:

- **Gap-fill only.** A container/volume name the caller doesn't already
  define is added as-is.
- **Same name, identical definition → deduplicated.** If the caller (or
  another merged `uses:` template) already defines a container/volume with
  the same name and an identical (JSON-equal) definition, the caller's
  definition is kept and nothing changes.
- **Same name, differing definition → run-creation error**, naming the
  container/volume, so the conflict can be resolved by renaming one side or
  aligning the two definitions.
- **Reserved names can never be injected.** A template that defines a
  container named `job` or `unified-artifact`, or a volume named `workspace`
  or `ucd-tools`, fails run creation — those names are owned by the system
  (the primary container, the artifact/cache sidecar, and the injected
  workspace/tools volumes).

In **scope** mode (`runsIn.image` on the `uses:` step), the template runs in
its own throwaway container/pod, not the caller's — so a template `podTemplate`
can't be honored there at all. A scope-mode `uses:` step whose template
declares a `podTemplate` fails run creation with an error rather than silently
dropping it.

### Template `finally:` (splice into the caller)

A `JobTemplate` may declare its own `spec.finally` — cleanup steps that don't
belong in the template's `steps:` DAG. A template's `finally:` steps do
**not** run in their own phase; they are **spliced into the CALLER's own
`finally:` phase**:

```yaml
# templates/with-cleanup.yaml (kind: JobTemplate)
spec:
  steps:
    - { name: work, run: ./work.sh }
  finally:
    - { name: cleanup, run: rm -rf /tmp/scratch }
```

```yaml
# caller
spec:
  steps:
    - name: build
      uses: { job: git://github.com/org/repo/templates/with-cleanup.yaml@v1 }
  finally:
    - name: my-own-cleanup
      run: echo done
```

resolves to a caller `spec.finally` containing, in order: `my-own-cleanup`
(the caller's own finally step, unchanged), then `build__cleanup` (the
template's finally step, renamed with the usual `usesName__` prefix and
ref-rewritten exactly like a body step — including `if:` combined with the
`uses:` step's own outer `if:`, the same way as [`if:` on a `uses:`
step](#if-on-a-uses-step) above).

- **Ordering: caller finally first, template finally appended after.** The
  caller's hand-written `finally:` steps always run (in their declared order)
  before any spliced-in template finally steps.
- **Prefixed like any inlined step**, so two sibling `uses:` steps pointing at
  the same template (e.g. `a:` and `b:` both using the same
  `with-cleanup.yaml`) never collide — they splice in as `a__cleanup` and
  `b__cleanup`.
- **Nested `uses:` bubble with full double-prefixing.** If the template's own
  body or its own `finally:` contains a nested `uses:` step whose target
  template *also* declares `finally:`, the nested finally steps bubble all
  the way up to the caller's `spec.finally`, carrying every prefix level
  applied along the way (e.g. `outer__inner__cleanup`) — no un-prefixed name
  ever leaks into the caller's finally list, and refs inside the bubbled
  steps stay valid at every level.
- **A `uses:` step already inside the caller's own `finally:`** works too: if
  a `uses:` step sitting in the caller's `finally:` block resolves to a
  template that itself declares `finally:`, both the template's body
  expansion and its spliced finally steps land in the caller's
  `spec.finally`.
- **Rejected in scope mode.** A `uses:` step with `runsIn.image` runs the
  template in its own throwaway environment whose lifetime ends with the
  template body — there is no environment left for a `finally:` step to run
  in by the time the caller's finally phase starts. A scope-mode `uses:` step
  whose template declares `finally:` fails run creation instead of silently
  dropping the field:
  ```
  template declares finally:, but this uses: step has runsIn.image (scope mode): the scope pod's lifetime ends with the template body, so its finally cannot be honored
  ```
- **Name collisions still fail loudly.** If a spliced-in name (e.g.
  `build__cleanup`) collides with an existing step name anywhere else in the
  resolved spec (another step, another template's splice, or a hand-written
  caller step), resolution fails naming the colliding name — same
  global-name-collision check that already applies to inlined body steps.
- **Status functions in the outer `if:` evaluate against the finally-phase
  status, not the status at expansion time.** Per [`if:` on a `uses:`
  step](#if-on-a-uses-step), the `uses:` step's outer `if:` is combined
  (`(outer) && (inner)`) onto every inlined step *including* its spliced
  finally steps. A param/steps-based outer condition (e.g.
  `params.env == "production"`) behaves exactly as expected: the spliced
  cleanup is gated the same way the body was. But an outer [status
  function](expressions.md#status-functions-in-if) is re-evaluated at the moment each
  spliced step runs — and by the time the finally phase runs, the run's
  status is frozen at whatever it ended up being, not what it was when the
  template's body executed. Concretely: `if: success()` on a `uses:` step
  whose body already ran successfully can still have its spliced
  `build__cleanup` **skipped**, if some unrelated later step in the job
  failed before the finally phase started — `success()` is now false
  job-wide. An outer `if: failure()` (the common rollback pattern) doesn't
  have this problem — it's evaluated the same way on both sides. If a
  template carries its own `finally:` and the caller wants its cleanup to
  reliably follow the template body's own success/failure regardless of what
  else happens later in the job, gate on a param or step output instead of a
  status function.

### Migrating a `kind: Job` template

Existing `uses:` targets authored as `kind: Job` must be converted:

1. Change `kind: Job` to `kind: JobTemplate`.
2. Drop any field outside the [`JobTemplate` schema](#the-jobtemplate-schema)
   above — most commonly `agentSelector`, `concurrency`, `timeoutMinutes`,
   and `native`, plus `podTemplate.reuse`/`workspace`/`override`. These never
   had meaning for an inlined template (the caller's run/agent/pod already
   governed them); the strict schema rejects them explicitly instead of
   silently ignoring them. `spec.finally` is the one exception: it **is**
   part of the schema now — see [Template
   `finally:`](#template-finally-splice-into-the-caller) above — but note its
   semantics changed from "the old `kind: Job`'s own finally phase" (which
   never ran under `uses:` anyway) to "spliced into the caller's finally
   phase", so re-check that a carried-over `finally:` still makes sense
   running in the caller's context.
3. If `podTemplate.spec` declared anything besides `containers`/`volumes`
   (e.g. a raw pod-level key), remove it — only `containers` and `volumes`
   are supported.
4. If the job genuinely needs its own pod/agent/run semantics (a dedicated
   `agentSelector`, its own `finally:`, pod reuse), it isn't a `uses:`
   candidate — keep it as a `kind: Job` and invoke it with
   [`call:`](#calling-other-jobs-call) instead.

---

## Uses-level `runsIn.image` (scope)

Put `runsIn.image` on a `uses:` step to run the **entire inlined template** in
**one** isolated environment — a "scope" — that stays alive across all of the
template's steps (one container on the standard agent, one dedicated pod on
k8s):

```yaml
steps:
  - name: build
    uses:
      job: git://github.com/my-org/ci-templates/jobs/build.yaml@v1.0.0
      with:
        target: ./cmd/server
    runsIn:
      image: golang:1.22
```

Because the template's steps share one long-lived environment, `cache`,
`uploadArtifact`, and `downloadArtifact` steps inside the template operate on
**the scope's own filesystem**, not the outer job workspace. So if `build.yaml`
restores a dependency cache, compiles, and uploads the resulting binary as an
artifact, all three happen inside the `golang:1.22` scope — a template can build
in its isolated environment and save the result without ever touching the outer
workspace.

The scope starts from a fresh, empty filesystem and never shares the outer job
workspace:

- **Inputs** enter via `with:` (env vars) and `downloadArtifact` (written into
  the scope filesystem).
- **Outputs** leave via `uploadArtifact` (pushed to the run's artifact store)
  and `outputs:`/stdout.

Artifacts are keyed by run, not by workspace path, so they cross the isolation
boundary naturally — on Kubernetes a scoped `uses` needs no shared
`ReadWriteMany` volume. Under `matrix`/`foreach`, each variant of a scoped
`uses` gets its own independent scope (its own container/pod).

A `uses:` step with no `runsIn` keeps its existing (non-scope) inlining
behavior — scope mode is triggered only by a **uses-level `runsIn.image`**.
`runsIn.container` on a `uses:` entry is rejected (a parse error); target a
named container from the template's own steps with `container:` instead.

`runsIn.resources.limits` bounds the scope's CPU/memory on both backends;
`runsIn.resources.requests` is rejected at apply time (use `podTemplate`
instead — it already supports `requests`). See [Uses-level `runsIn.image`
resources](../resources/job.md#uses-level-runsinimage-resources) for the full
behavior.

Scope mode and the [pod-shape merge](#pod-shape-merge-containers-and-volumes)
are mutually exclusive: a scoped `uses:` template runs in its own dedicated
environment, not the caller's pod, so a template `podTemplate` has nowhere to
go there — a scope-mode `uses:` whose template declares a `podTemplate` fails
run creation instead of silently dropping it.

**Not allowed inside a scoped `uses`** (parse errors, because they are
incompatible with holding one isolated environment across the whole template):

- a nested `runsIn:` (any form) on an inlined step — the scope is a single
  homogeneous environment, not a per-step override;
- an `approval:` step — it would pin the isolated container/pod open across a
  human wait;
- a `call:` step — the child run executes elsewhere and cannot see the scope's
  filesystem.

---

## Calling Other Jobs (`call`)

Invoke another registered job as a step. The called job runs with its own DAG on the same or a different agent.

```yaml
steps:
  - parallel:
      - name: build-frontend
        call:
          job: build                # name of another registered Job
          with:
            image: myapp-frontend
            tag: "{{ .Params.tag }}"

      - name: build-backend
        call:
          job: build
          with:
            image: myapp-backend
            tag: "{{ .Params.tag }}"

  - name: deploy
    run: ./deploy.sh
```

`call` steps wait for the called job to complete. The called job's run shares the parent run's context.

Once the child run exists, the call step's status badge in the run detail view
follows the child run's current status. This remains accurate if the parent
agent stops before it can submit a final step report.

By default a `call` step waits **indefinitely** for the child run to finish. To
bound the wait, set the step's `timeoutMinutes`: if the child run has not
reached a terminal state within that many minutes, the call step fails with a
timeout and — once the parent run finalizes — the child run is cancelled. When
`timeoutMinutes` is unset there is no timeout (the call waits until the child
finishes or the parent run is cancelled).

On success the child run's ID is available to later steps as
`{{ .Steps.<call-step-name>.ChildRunID }}` (for matrix call steps, a map keyed
by combination key — `{{ index .Steps.<name>.ChildRunID "linux/amd64" }}`) —
see [Downloading from another run (`runId`)](artifacts-and-cache.md#downloading-from-another-run-runid)
for fetching the child's artifacts. Reference the call step by its name with
dot-notation (e.g. `build_app`); step names are validated as identifiers (see
[Step names](steps.md#step-names)), so a name like `build_app` is always addressable.

> **⚠️ Slot deadlock: the called job needs a *free* agent slot.**
> A `call` step holds the parent run's agent slot while it waits for the called
> job to finish. The called job is a **separate run** that must be claimed by an
> agent — if it can only run on the same agent pool and that pool has no free
> slot, it deadlocks: the child stays `Queued` forever while the parent stays
> `Running`. Unless the call step sets `timeoutMinutes` (which fails the parent
> step and releases its slot after that many minutes), nothing breaks the
> deadlock automatically.
>
> The common trigger is an agent (or pool) with **`max-concurrent: 1`** calling a
> job that targets the same agent: the parent occupies the only slot, so the
> child can never be claimed.
>
> **Requirement:** any agent pool that runs `call` chains must have
> **`max-concurrent` ≥ 2** (and ≥ 1 + the maximum `call` nesting depth for
> nested calls), or route the called job to a *different* agent pool via its
> `agentSelector`, **or mark the orchestrator job `detached: true`** (see below)
> so it never holds a normal slot while it waits. Cancelling the parent releases
> its slot, after which the child completes (and the parent's `finally` block
> still runs).

### Detached runs (`detached`)

A job with `spec.detached: true` is a **lightweight orchestrator**: its runs do
**not** consume an agent's `max-concurrent` budget and are claimed from a
separate `max-detached-concurrent` pool. Use it for jobs that mostly issue
`call:` steps and wait — marking the orchestrator `detached` keeps it from
holding a scarce execution slot while its child runs, which is the cleanest fix
for the call-slot deadlock above.

```yaml
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: release-orchestrator
spec:
  detached: true                 # this run does not occupy a normal slot
  steps:
    - name: build_ios
      call: { job: unity-build-ios }
    - name: build_android
      call: { job: unity-build-android }
```

- **Claimed out of the box.** Every agent hosts up to `max-detached-concurrent`
  detached runs, which **defaults to 16**, so a `detached` job runs without extra
  agent configuration. Set a positive value to change the cap, or a **negative**
  value on agents that must not host detached runs. (If every agent has detached
  disabled, a `detached` job stays `Queued`.)
- **Independent workspace.** On host agents a detached run gets its own per-run
  workspace (removed when the run finishes); on Kubernetes each run already has
  its own pod, so nothing changes there.
- **Orthogonal to `native`.** `detached` (concurrency accounting) and `native`
  (host-process execution) may be combined — a native host orchestrator is a
  valid detached job.
- **Keep them lightweight.** `detached` exempts the run from `max-concurrent`, so
  a *heavy* detached job can over-subscribe the host/cluster. Use it for
  orchestration, not for real compute.
- **Do not combine with `podTemplate.reuse`.** A detached run holds its pod idle
  while it waits, so pooling gives no benefit.

---

