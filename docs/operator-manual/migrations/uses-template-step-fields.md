# Migrating templated `approval:`, `retry:`, `matrix:` and `foreach:`

`uses:` inlines a `kind: JobTemplate`'s steps into the caller's run. Four
fields declared on a template's own steps were being dropped during that
inlining — silently, with no error, no warning and no log line: `approval:`,
`retry:`, `matrix:` and `foreach:`. They are now carried through.

This is **not a breaking change for these four fields** — nothing you can
write today stops working because `approval:`/`retry:`/`matrix:`/`foreach:`
are now honored. It is here because it changes what an *already-applied* job
does at runtime, and one of the four fields is a human gate. **If any
`kind: JobTemplate` you `uses:` declares `approval:` on a step, that gate was
not gating.** Read [Who is affected](#who-is-affected-and-how-to-find-out)
before your next production run.

A related pair of fields, `ScopeID`/`ScopeImage`, was dropped by the same bug
and is restored by the same fix — but unlike the four above, **that one case
actually can break a run that depended on the old behaviour.** It has its own
section below: [A related fix: nested `runsIn.image` now actually
isolates](#a-related-fix-nested-runsinimage-now-actually-isolates).

**Before**, the inliner rebuilt each concrete (non-`parallel:`) template step
from a hand-maintained list of fields to keep. The four fields were added to
the step schema after that list was written and never added to it, so they
were dropped on the way in. Steps inside a template's `parallel:` block were
never affected — that path copied the whole step — so the same template could
lose a field on one step and keep it on the step beside it.

**After**, every field a template step declares survives inlining. The
inliner now copies the whole step and rewrites only what must change (the
uses-prefixed name, the combined `if:`, and `.Params.`/`.Steps.` references),
so a field added to the step schema in future cannot go missing the same way.

| Field on a template step | Before (inlined into the caller) | After |
|---|---|---|
| `approval:` | Dropped. The step inlined with **no action at all** (`run: ""`), so the agent ran an empty script, reported `Succeeded`, and the run continued. | The gate is inlined and the run pauses for a human decision. |
| `retry:` | Dropped. One attempt, then failure. | The declared `attempts`/`backoff` apply. |
| `matrix:` | Dropped. One un-expanded copy of the step ran; a template promising a build across five platforms built one and reported success. | The step fans out to one variant per combination. |
| `foreach:` | Dropped, same as `matrix:`. | The step fans out to one variant per item. |

Every other step field (`run:`, `if:`, `env:`, `outputs:`, `cache:`, `post:`,
`call:`, `uploadArtifact:`, `downloadArtifact:`, `container:`, `shell:`,
`timeoutMinutes:`, `continueOnError:`) was already carried across and is
unchanged.

## A related fix: nested `runsIn.image` now actually isolates

The same hand-written field list also dropped two fields that aren't
user-authored: `ScopeID` and `ScopeImage`, which the inliner itself stamps
onto a template step when *that step's own* `uses:` carries
[`runsIn.image`](../../user-guide/writing-jobs/templates-and-reuse.md#uses-level-runsinimage-scope)
(a "scope"). They only come into play for a specific nested shape — a
template calling another template into an isolated scope — but where that
shape exists, restoring them **can change a run's outcome**, not just its
audit trail. Unlike the four fields above, treat this one as a genuine
compatibility risk to check for, not just a fixed silent failure.

The shape: a caller job has a plain (non-scope) `uses:` on template **T**. One
of T's own steps has a `uses:` of its own, targeting template **N**, with
`runsIn: { image: X }` on that inner `uses:` step. N has concrete
(non-`parallel:`) steps.

- **Before:** inlining T dropped the `ScopeID`/`ScopeImage` that resolving
  the inner `uses:` had already stamped onto N's steps (the hand-written
  literal never listed those two fields, on top of never listing the four
  above). N's steps ran **unscoped** — sharing the caller's own workspace and
  exec target — and image `X` was never used at all.
- **After:** `ScopeID`/`ScopeImage` survive the outer inlining, so N's steps
  run the way the `runsIn: { image: X }` on T's step originally asked: inside
  their **own isolated scope container**, on image `X`, starting from an
  empty `/workspace` — not the caller's checkout.

This is a fix in the same direction as the other four: it restores the
isolation a template author explicitly asked for with `runsIn.image`, and is
arguably its own silent-isolation-bypass — a `runsIn.image` that silently
failed to isolate is not so different from an `approval:` that silently
failed to gate. But it is fair to call it a breaking change in this one
shape: a step in N that used to read a file the caller had checked out, or
that wrote a file a later caller step expected to find, ran on the shared
workspace only because of the bug, and now runs against an empty
`/workspace` instead. Move data like that through `uploadArtifact:` /
`downloadArtifact:` (or `cache:`) instead of relying on a shared filesystem.

## Who is affected, and how to find out

Only jobs whose steps use `uses:` with a `kind: JobTemplate` target are
involved. Check the templates you reference, not your jobs — the fields are
declared in the template. Grep the pinned ref your job actually resolves
(see below), not the working tree:

```bash
git -C <your template repository> grep -n -E "(approval|retry|matrix|foreach):" <pinned-ref>
```

A hit on a step **outside** a `parallel:` block is a field that was being
dropped. Do not anchor the pattern to the start of the line: a flow-style
step (`- {name: gate, approval: {message: ok?}}`) puts the key after other
text on the same line, which an anchored pattern misses outright.

**For the `ScopeID`/`ScopeImage` case above**, search the same pinned ref for
a template step that itself declares `runsIn:`:

```bash
git -C <your template repository> grep -n -E "runsIn:" <pinned-ref>
```

A hit means one of that template's own steps calls another template into a
scope. Check whether any step downstream of it (inside the *called*
template, N in the description above) ever assumed it could see the outer
caller's workspace, or a file a caller step wrote — that assumption now
breaks, because the step runs isolated instead of on the shared workspace.

Remember that `uses:` targets are **pinned** (`@v1.2.3`, `@a1b2c3d4`), so the
template version your jobs actually resolve is the one at that ref, not
whatever is on the template repository's default branch today.

## The symptom of the old behaviour

There was no error to search for — that is what made this worth a guide. What
the run history shows instead, for `approval:`:

- The gate step finished `Succeeded`, in well under a second.
- Its log output is empty.
- No approval was ever recorded against the run, and nobody was asked.

For `matrix:`/`foreach:`, the step ran exactly once where the template
declared several variants, and the run reported success on that single copy.
For `retry:`, a failing step failed on its first attempt with no retry
recorded.

## What to expect after upgrading

- **Runs will now stop and wait.** A templated approval gate blocks until a
  human approves or the approval times out (`timeoutMinutes`, default 60).
  Pipelines that appeared to run unattended will now need an approver. This is
  the behaviour the template always asked for. See [Approval Step
  (`approval`)](../../user-guide/writing-jobs/approval-and-finally.md#approval-step-approval)
  for how a run is approved or rejected.
- **Step counts change.** A templated `matrix:`/`foreach:` step now expands to
  one variant per combination instead of a single copy, so run duration,
  concurrency and agent load all go up accordingly.
- **Failures may take longer to surface.** A templated `retry:` now really
  retries before the step fails.
- **A nested `runsIn.image` now actually isolates.** See [A related fix:
  nested `runsIn.image` now actually
  isolates](#a-related-fix-nested-runsinimage-now-actually-isolates) above —
  in that one specific shape, a step that used to (accidentally) share the
  caller's workspace now runs against an empty one, and can fail if it
  depended on that.

Nothing needs to be re-applied or edited: the change is in how a run is
compiled, so it takes effect for new runs as soon as the controller is
upgraded. If a gate is now blocking a pipeline you want unattended, remove
`approval:` from the template — deliberately, this time.

## Two things that stay as they were

- **Scope mode still rejects `approval:`.** Inside a `uses:` step carrying
  [`runsIn.image`](../../user-guide/writing-jobs/templates-and-reuse.md#uses-level-runsinimage-scope),
  `approval:` (and `call:`, and step-level `container:`) remain hard errors,
  because a human wait would hold the isolated scope environment open. That
  rejection was already loud and is unchanged. `retry:` and `matrix:` are
  legal there and now work — the scope environment is keyed per matrix
  variant, so variants get independent environments.
- **`approval:` is still rejected in a template's `finally:`**, exactly as in
  a job's own [`finally:`](../../user-guide/writing-jobs/approval-and-finally.md#finally-block-finally)
  — the approval would have nothing left to gate.

## One consequence worth knowing about

If a template declares an output in `spec.params.outputs` and the step
producing it now carries `matrix:`, that output becomes per-variant: the
template's output capture reads a map keyed by matrix combination rather than
a single value. This is the same rule that has always applied to referencing a
matrix step's outputs in a hand-written job (see [Matrix and Foreach
Steps](../../user-guide/writing-jobs/steps.md#matrix-and-foreach-steps)) — it
just was not reachable through `uses:` while `matrix:` was being dropped.
Declare a job-level output from a non-matrix step if you need a single scalar.
