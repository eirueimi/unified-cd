# Migration: `uses:` targets are now `kind: JobTemplate`

This release gives `uses:` an explicit contract: **a `uses:` step must point
at a `kind: JobTemplate` document** — a dedicated resource whose strict schema
contains only what inlining into the caller's run can honor. Pointing `uses:`
at a `kind: Job` file (the previous convention) now fails at run creation.
This is a **breaking change** for every existing `uses:` template.

Why: previously `uses:` fetched a full `kind: Job` but silently honored only
its `steps`/`params`/`shell` — the template's `podTemplate`, `agentSelector`,
`finally`, and every other field were dropped without any error, and a
template that targeted its own `container:` failed opaquely at exec time. The
new schema makes the supported surface explicit, and the template's
`podTemplate.spec.containers`/`volumes` are now actually **merged into the
caller's pod**. See [Job Reference](jobs.md) and
[Resource Reference — JobTemplate](resources.md#jobtemplate) for the full
contract.

## Before → after

| Before | After |
|---|---|
| **`uses:` template file with `kind: Job`** (steps/params/shell only) | Change `kind: Job` → `kind: JobTemplate`. Nothing else needed — steps, params, and shell carry over unchanged. |
| **Template declaring `agentSelector`, `concurrency`, `timeoutMinutes`, or `native`** (previously silently ignored under `uses:`) | Remove the field from the template — it never had any effect under `uses:`. If the logic genuinely needs its own agent/run semantics, keep it a `kind: Job` and invoke it with `call:` instead. |
| **Template declaring `finally`** (previously silently ignored under `uses:` — this row used to say "remove the field") | **Now supported, with different semantics than a `kind: Job`'s own finally phase**: `finally:` on a `JobTemplate` splices its (renamed, ref-rewritten) steps into the *caller's* `spec.finally`, appended after the caller's own finally steps — it does not run in a phase of its own. Nothing to remove; just double-check the carried-over steps still make sense running as part of the caller's finally rather than the old `kind: Job`'s own. Rejected (run-creation error) if the `uses:` step uses scope mode (`runsIn.image`). See [Job Reference — Template `finally:`](jobs.md#template-finally-splice-into-the-caller). |
| **Template declaring `podTemplate` containers/volumes** (previously silently dropped — `container:` steps failed at exec) | Keep them: they are now merged into the caller's pod (gap-fill; the caller's own same-name definition wins if identical, differing definitions fail the run). Reserved names cannot be injected: containers `job`/`unified-artifact`/`ucd-shim`, volumes `workspace`/`ucd-tools`. Other `podTemplate` fields (`reuse`, `workspace`, `override`, pod-level spec keys) are not in the schema — remove them or use `call:`. |
| **Dual-use file** (same YAML both `apply`-registered for `call:` AND fetched via `uses:`) | Split it: the `uses:` side becomes a `kind: JobTemplate`; for `call:`, register a thin `kind: Job` wrapper that `uses:` the template and declares the run-level fields itself (see `examples/jobs/call-template.yaml`). |
| **Pinned refs (`@v1.2.3` / `@<sha>`) pointing at pre-migration commits** | Those commits contain `kind: Job` forever, so the kind gate rejects them. Re-pin to a tag/SHA at or after the template's migration commit. |
| **`uses:` step inside a `parallel:` block** (previously accepted by validation but never resolved — failed opaquely at exec) | Now rejected at parse time: move the `uses:` step to a top-level step (an expanded template is a sequence and cannot occupy one parallel slot). |

## Errors you will see

- Pointing `uses:` at a `kind: Job`:
  `uses: targets must be kind: JobTemplate (got kind: Job); convert the template, or invoke the job with call:`
- A template field outside the JobTemplate schema (strict decode), e.g.:
  `field agentSelector not found in type dsl.JobTemplateSpec`
- A step referencing an undefined container (now caught at run creation
  instead of failing at exec):
  `step "x" references container "y", which is not defined in the job's podTemplate`
- `uses:` inside `parallel:`:
  `uses: is not supported inside parallel: (a uses template expands to a sequence of steps); move it to a top-level step`

## Also in this change

- `uses:` inside `finally:` is now resolved (previously it silently reached
  the agent unresolved and failed at execution).
- The bundled `templates/` collection has been migrated to
  `kind: JobTemplate` (exception: `buildkit-rootless-build-push.yaml`, which
  replaces the primary `job` container in its own pod and therefore remains a
  standalone `kind: Job` for `apply` + `call:`).
