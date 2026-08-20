# Templates and `uses`

## `uses:` run fails with `uses: targets must be kind: JobTemplate`

**Symptom**

A run using `uses: git://...` fails at creation with:

```
uses: targets must be kind: JobTemplate (got kind: Job); convert the template, or invoke the job with call:
```

or a strict-decode error naming a field, e.g. `field agentSelector not found in type dsl.JobTemplateSpec`.

**Cause**

`uses:` targets must be `kind: JobTemplate` — a strict schema holding only
what inlining can honor. The fetched file is either a pre-migration
`kind: Job` template, or declares a field outside the JobTemplate schema.
Note that pinned refs (`@v1.2.3` / `@<sha>`) pointing at commits from before
the template's migration keep fetching the old `kind: Job` content forever.

**Fix**

Convert the template (`kind: Job` → `kind: JobTemplate`, drop unsupported
fields) and re-pin tags/SHAs to a commit that uses the `JobTemplate` kind. If
the target genuinely needs its own pod/agent/run semantics, keep it a
`kind: Job` and invoke it with `call:` instead.

## Scoped `uses` step can't find workspace files

**Symptom**

A `uses:` step with `runsIn.image` set (a [scope](../user-guide/writing-jobs/templates-and-reuse.md#uses-level-runsinimage-scope))
runs a template step that expects a file produced earlier in the outer job —
`cat`, a build tool, a script — and the file is simply not there (`no such
file or directory` or equivalent), with no error from the framework itself
pointing at the cause.

**Cause**

A `uses:` step with a uses-level `runsIn.image` runs the **entire inlined
template** in one isolated environment (one container on the standard agent,
one dedicated pod on Kubernetes) that starts from a **fresh, empty
filesystem** and never shares the outer job's workspace. Files written by
steps before the `uses:` step — via `run:`, `uploadArtifact`, or anything
else — are silently absent inside the scope; there is no error at scope
start, because from the scope's point of view there was never anything to
find. This is easy to miss because a **non-scoped** `uses:` step (no
`runsIn`) inlines into the caller's own workspace and does not have this
problem.

**Fix**

Treat the scope like a separate machine and cross the boundary explicitly:

- Pass inputs in via `with:` (environment variables) or `downloadArtifact`
  (pulls a previously uploaded artifact into the scope's filesystem).
- Get outputs back out via `uploadArtifact` (pushes to the run's artifact
  store, retrievable outside the scope) or `outputs:`/stdout.

See [Uses-level `runsIn.image` (scope)](../user-guide/writing-jobs/templates-and-reuse.md#uses-level-runsinimage-scope)
for the full model.

## Job fails apply with a dangling `container:` reference

**Symptom**

Applying (or triggering) a job that has no `uses:` step anywhere in
`steps:`/`finally:` now fails immediately with:

```
step "x" references container "y", which is not defined in the job's podTemplate
```

where this previously either passed apply and failed later at run creation,
or (for a job with `runsIn`-style scoping unrelated to `uses:`) failed
opaquely when the agent tried to exec into a nonexistent container.

**Cause**

A step's `container:` field (or a `uses:`-inlined step's inherited
`container:`) must name either the reserved primary container (`job`), one
of the other reserved names, or a container actually declared in
`spec.podTemplate.spec.containers`. For a **plain job** — no `uses:` step in
`steps:` or `finally:`, and no named agent-side `podTemplate.name` (whose
containers live in agent config and aren't visible here) — this is now
checked at **apply time** via `internal/dsl.Job.Validate` /
`ValidateContainerReferences` (`internal/dsl/container.go`), instead of only
at run creation or step-exec time. A job that carries any `uses:` step still
defers this check to the controller's post-resolution sweeper, because the
template's pod-shape merge may supply the missing container at resolution
time; a named agent-side `podTemplate.name` defers to pod build on the
agent.

**Fix**

- Add the missing container to `spec.podTemplate.spec.containers`, or
- Fix the typo in the step's `container:` field, or
- Remove `container:` from the step to use the primary container instead.

See [Job Reference — `container:`](../user-guide/writing-jobs/isolation-and-containers.md#container-targeting-a-podtemplate-container)
and [`ValidateContainerReferences`](https://github.com/eirueimi/unified-cd/blob/main/internal/dsl/container.go).

## `podTemplate` container/volume name rejected as an invalid DNS-1123 label

**Symptom**

Applying a `Job` or a `JobTemplate` (or resolving a `uses:` template whose
`podTemplate` merges into the caller) fails with an error like:

```
podTemplate container name "My_Tools" is not a valid DNS-1123 label (lowercase alphanumerics and '-', must start/end alphanumeric)
podTemplate volume name "Cache Vol" is not a valid DNS-1123 label (lowercase alphanumerics and '-', must start/end alphanumeric)
```

or `podTemplate container name is required` / `... exceeds 63 characters` for
an empty or overlong name.

**Cause**

Every `podTemplate.spec.containers[].name` and `podTemplate.spec.volumes[].name`
must be a valid Kubernetes DNS-1123 label — lowercase alphanumerics and `-`
only, starting and ending with an alphanumeric character, 63 characters or
fewer (`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`). This is now checked at **apply
time** by `internal/dsl.ValidateDNS1123Label` (`internal/dsl/container.go`),
for both `Job.Validate` and `JobTemplate.Validate` — previously an invalid
name (uppercase, underscores, spaces, a leading/trailing `-`, a `.`, or an
empty/too-long name) went unvalidated until the pod was actually built,
surfacing as an opaque Kubernetes/container-runtime API error much later.
This also closes a case/whitespace evasion of the reserved-name checks
(`job`/`unified-artifact`/`ucd-shim` for containers,
`workspace`/`ucd-tools` for volumes): those checks normalize
(trim+lowercase) before comparing, but a variant like `" Job "` is now
rejected by shape validation before it would even reach that comparison.

**Fix**

Rename the container/volume to a valid DNS-1123 label, e.g. `my-tools`
instead of `My_Tools`, `cache-vol` instead of `Cache Vol`. See [Job
Reference — Kubernetes Pod Template
(`podTemplate`)](../user-guide/writing-jobs/isolation-and-containers.md#kubernetes-pod-template-podtemplate) for the full
rule and the reserved-name list.

## `uses: git://...` job fails to resolve with invalid characters

**Symptom**

A `uses: git://...` job fails to resolve with `git URI ref "..." contains invalid characters`.

**Cause**

The `@ref` portion contains characters outside `[A-Za-z0-9._/+-]` or starts with `-` (blocked to prevent git option injection).

**Fix**

Reference a normal branch, tag, or SHA. Relative ref syntax like `HEAD~1`, `HEAD^`, or `main@{upstream}` is intentionally rejected; use a plain branch name, tag, or full SHA instead.

## Run fails with log line `git template resolution failed for more than 1h0m0s`

**Symptom**

a run fails with log line `git template resolution failed for more than 1h0m0s: ...`.

**Cause**

the job references a `git://` template whose host stayed unreachable (or credentials stayed invalid) past `UNIFIED_GIT_RESOLVE_DEADLINE`.

**Fix**

fix the repository URL/credentials and re-trigger the run.

