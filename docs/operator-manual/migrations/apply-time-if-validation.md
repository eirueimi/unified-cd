# Migrating `if:` expressions validated at apply time

`unified-cli apply` now compiles every step's `if:` expression before storing
the job. A condition that does not compile is **rejected**. Previously it was
accepted, and failed at run time — where a compile failure is deliberately
**fail-open**: the step *runs*.

**Before**, `if: '{{ eq .Params.env "production" }}'` applied without complaint.
On every run the expression failed to compile, the orchestrator logged a
warning, and the production-only step ran on every trigger, with the author's
intent inverted and nothing in the run to say so.

**After**, the same job fails `apply` with the compile error, while its author
is present to read it. That is the point of the change and it is not going
away.

But the change is a **compatibility break for jobs already in your system**,
and for a Git-managed job the consequence is worse than the fail-open it fixes.
Read [What happens to a Git-managed job](#what-happens-to-a-git-managed-job)
before you upgrade.

| Before | After |
|---|---|
| An `if:` that does not compile applied cleanly. | `apply` fails with `if: expression "..." does not compile`. |
| At run time the condition failed open — the step ran regardless of what the condition said. | The job never reaches an agent with an uncompilable condition, so the gate means what it says. |
| An AppSource re-synced such a job normally. | The reconciler skips the file. With `syncPolicy.prune: true` the job is **deleted**; without prune it silently stops being updated from Git. |

## What is now rejected

Only what `cel-go` refuses to compile: a syntax error, an unknown identifier, or
a type mismatch. The check runs against the **same environment** the agent uses,
so it cannot reject a condition that works today — anything it refuses would
have hit the identical compile error at run time and failed open.

The two shapes that exist in real job files:

**Go-template syntax in an `if:`.** `if:` is [CEL](https://github.com/google/cel-go),
not a Go template — unlike `run:`, `env:` and `outputs:` in the same file.

```
spec.steps[0] (deploy): if: expression "{{ eq .Params.env \"production\" }}" does not compile
(if: is a CEL expression, not a Go template): ERROR: <input>:1:19: Syntax error: missing ':' at '"production"'
 | {{ eq .Params.env "production" }}
 | ..................^
```

**An identifier that was never bound.** The `if:` environment declares exactly
four variables — `params`, `vars`, `steps`, `secrets` — plus the zero-argument
functions `always()`, `failure()` and `success()`. Anything else is undeclared.
`matrix` and `foreach` are the common mistakes, because both exist as `{{ }}`
template variables and neither is bound in CEL:

```
spec.steps[0] (deploy): if: expression "matrix.os == \"linux\"" does not compile
(if: is a CEL expression, not a Go template): ERROR: <input>:1:1: undeclared reference to 'matrix' (in container '')
 | matrix.os == "linux"
 | ^
```

What is **not** rejected: an expression that compiles but errors during
evaluation. The one case left in `conditionVars` that can still do this is a
`steps.NAME` reference that names a step the job does not have — `params`,
`vars` and `secrets` no longer raise here; an undefined key on any of those
three reads as the empty string instead (see
[the params-undefined-key migration
note](params-undefined-key-is-empty.md)). See
[Steps: Conditional Execution](../../user-guide/writing-jobs/steps.md#conditional-execution-if)
for the full undefined-key table.

## What happens to a Git-managed job

This is the part to plan around. An AppSource reconcile that cannot parse a
file takes the skip-one-file branch: it logs a warning and moves on, so one bad
file never aborts a whole sync. That was a reasonable rule when the only way to
reach it was a genuinely malformed file. This change makes previously-valid
files reach it.

**With `syncPolicy.prune: true`, the job is deleted.** The skip happens before
the resource is recorded as still-present in this sync, and the prune step then
deletes every previously-managed resource that is not in that set — it cannot
distinguish "removed from Git" from "present in Git but skipped". The file is
still in your repository. The job is gone from the controller, along with
anything pointing at it: a `Schedule` targeting it stops firing, a
`WebhookReceiver` triggering it starts failing, and its run history is no longer
reachable by job name.

**Without prune, the job silently stops being updated.** The old stored spec
stays live and keeps running on every trigger. Commits to that file have no
effect — no error surfaces anywhere an author looks, because the author's
feedback loop is the Git push, and the push succeeded.

The only trace either way is in the **controller's** log:

```
appsource reconciler: failed to apply resource, skipping  appsource=my-src file=jobs/legacy.yaml kind=Job resource=legacy error=...
appsource reconciler: some resources failed to apply and were skipped  appsource=my-src skipped=1 applied=12
```

If you upgrade and something stops running, that is the line to search for.

## Find affected jobs before upgrading

Do not grep for this. `if:` values appear in flow style, in `finally:` blocks,
inside `parallel:` groups and inside `uses:` templates, and the thing you are
looking for — "does this compile as CEL?" — is not a text pattern.

Run the real validator instead. `unified-cli apply --dry-run` parses and
validates locally and **does not contact the server**, so it is safe to run
against production job files from anywhere (the CLI still needs its usual
configuration to start):

```bash
# Every job file in your AppSource repository, on the NEW CLI binary.
find . -name '*.yaml' -print0 | while IFS= read -r -d '' f; do
  unified-cli apply --dry-run -f "$f" >/dev/null || echo "AFFECTED: $f"
done
```

Jobs that were applied manually and are not in Git need to be pulled out of the
controller first:

```bash
unified-cli export -o ./exported          # on the OLD binary, before upgrading
# then run the loop above over ./exported
```

Two things the dry run cannot see:

- **A `uses:` template's `if:`.** Templates are fetched from Git and resolved
  when a run is created, not when the calling job is applied, so a bad condition
  inside a template is invisible here. It surfaces later as a **failed run**
  (see below). Dry-run the template repository's files too.
- **Jobs whose stored spec differs from the file** — which, if you already have
  a job in the skipped-and-not-updated state, is exactly the situation. Trust
  `export` over the repository for those.

## Jobs that call a `uses:` template

A template's steps are validated by the same check when the template is parsed
at resolve time. A template with an uncompilable `if:` therefore makes the
**run fail** rather than fail open — immediately if the resolver classifies the
failure as deterministic, otherwise once the run passes the resolution
deadline. The reason is written into the run's own log.

This is a visible failure with a real error message, which is a better outcome
than the silent wrong-gate it replaces — but it is still a behaviour change:
runs that used to complete now fail until the template is fixed. Fix the
template repository before or at the same time as the upgrade.

## The fix

Rewrite the condition as CEL. There are no `{{ }}` delimiters and no leading
dot, and the variables are lowercase:

```yaml
# wrong — Go template, rejected at apply
if: '{{ eq .Params.env "production" }}'

# right
if: params.env == "production"
```

```yaml
# wrong — matrix is not bound in CEL
if: matrix.os == "linux"

# right — pass the dimension in as a parameter, or gate with a var
if: params.os == "linux"
if: vars.TARGET_OS == "linux"
```

| Template form | CEL form |
|---|---|
| `{{ .Params.NAME }}` | `params.NAME` |
| `{{ .Vars.NAME }}` | `vars.NAME` |
| `{{ .Steps.NAME.Outputs.KEY }}` | `steps.NAME.outputs.KEY` |
| `{{ secrets.NAME }}` | `secrets.NAME` |
| (no template form) | `always()`, `failure()`, `success()` |

See [Expressions and Conditions](../../user-guide/writing-jobs/expressions.md)
for the full reference, and
[Steps: Conditional Execution](../../user-guide/writing-jobs/steps.md#conditional-execution-if)
for the CEL variable table and the undefined-key rules.

## Recovering a job that was already pruned

If you upgraded before reading this and a job was deleted by prune:

1. Fix the `if:` in the repository and push. The next reconcile re-applies the
   file and the job comes back under the same name.
2. Run history recorded against that job name is reachable again once the job
   row exists.
3. Check any `Schedule` or `WebhookReceiver` that targets it — those were not
   deleted, and they resume working as soon as the job exists again.

If you have not upgraded yet, setting `syncPolicy.prune: false` on the
AppSource for the duration of the upgrade turns the deletion into the milder
stops-being-updated case, at the cost of leaving genuinely-removed resources
behind until you turn it back on.
