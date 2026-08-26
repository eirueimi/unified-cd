# Migrating templated `approval:`, `retry:`, `matrix:` and `foreach:`

`uses:` inlines a `kind: JobTemplate`'s steps into the caller's run. Four
fields declared on a template's own steps were being dropped during that
inlining — silently, with no error, no warning and no log line: `approval:`,
`retry:`, `matrix:` and `foreach:`. They are now carried through.

This is **not a breaking change** — nothing you can write today stops working.
It is here because it changes what an *already-applied* job does at runtime,
and one of the four fields is a human gate. **If any `kind: JobTemplate` you
`uses:` declares `approval:` on a step, that gate was not gating.** Read
[Who is affected](#who-is-affected-and-how-to-find-out) before your next
production run.

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

## Who is affected, and how to find out

Only jobs whose steps use `uses:` with a `kind: JobTemplate` target are
involved. Check the templates you reference, not your jobs — the fields are
declared in the template:

```bash
grep -rn -E "(approval|retry|matrix|foreach):" <your template repository>
```

A hit on a step **outside** a `parallel:` block is a field that was being
dropped. Do not anchor the pattern to the start of the line: a flow-style
step (`- {name: gate, approval: {message: ok?}}`) puts the key after other
text on the same line, which an anchored pattern misses outright.

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
