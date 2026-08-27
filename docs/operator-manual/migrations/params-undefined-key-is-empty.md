# Migrating: an undefined `params` (and `secrets`) key in `if:` now reads as empty

An `if:` that gates on a `params` key which is never actually set used to
**run the step**. It now **evaluates as though the key were the empty
string**, which for the common `== "something"` form means the step is
**skipped** instead.

```yaml
steps:
  - name: deploy-to-prod
    if: 'params.DEPOY == "yes"'   # typo: DEPOY, not DEPLOY
    run: ./deploy.sh --prod
```

**Before**, that typo ran `deploy-to-prod` on *every* run, regardless of what
`DEPLOY` was actually set to — `params.DEPOY` raised `no such key`, an
evaluation error, and `EvalCondition`'s error path is fail-safe: **run the
step**. A gate written to be closed defaulted open, silently, with nothing in
the run's log to say why. That is the worst direction for a deploy gate to
fail in.

**After**, `params.DEPOY` evaluates to `""`, `"" == "yes"` is `false`, and
`deploy-to-prod` is skipped — the outcome the gate's author almost certainly
wanted from a typo, and the same outcome `vars` has given since it was added.
The run's own log gains a `System` line naming the undefined key, same as
`vars` already does.

`secrets` had the identical trap for the identical reason (same
`map(string, string)` shape, same CEL default map semantics) and is fixed the
same way, in the same change. `steps` is **not** — see
[What did not change: steps](#what-did-not-change-steps) below.

| Before | After |
|---|---|
| `params.MISSING` in an `if:` raised `no such key` at run time. `EvalCondition` returned `(true, nil, err)` — fail-safe: the step ran. | `params.MISSING` evaluates to `""`. No error. The step runs or skips based on what the condition actually says with an empty string in place. |
| `secrets.MISSING` in an `if:` raised `no such key` the same way, with the same fail-open outcome. | `secrets.MISSING` evaluates to `""`, same as `params`. |
| Nothing in the run's log explained why a params-gated step ran. | The run's own **System** stream (stepIndex `-1`) gets a line naming the expression and the undefined key, e.g. `if: expression "params.DEPOY == \"yes\"" referenced undefined params.DEPOY — an undefined key reads as the empty string, ...`. |
| `steps.MISSING.outputs.KEY` raised `no such key`, fail-open. | **Unchanged** — still raises, still fails open. See below. |

## Why this is a real behaviour change, not a bug fix in the narrow sense

Every `if:` in a job file today that references a `params` (or `secrets`) key
which is not actually supplied on that run currently **runs its step**. After
upgrading, the same condition **evaluates against `""`** instead, and for
almost every comparison operator that changes the result:

- `params.ENV == "prod"` — was: ran regardless (error → fail-open). Now:
  runs only if `ENV` is genuinely `"prod"`; otherwise skips.
- `params.ENV != "prod"` — was: ran regardless. Now: runs when `ENV` is
  anything other than `"prod"`, **including** when `ENV` is undefined — this
  direction is unaffected in practice, since undefined already isn't `"prod"`.
- `params.ENV in ["staging", "prod"]` — was: ran regardless. Now: skips
  unless `ENV` is genuinely one of those two values.

If a job in your fleet has an `if:` that references a `params` or `secrets`
key which is misspelled, or which is conditionally supplied only on some
triggers (e.g. only a webhook payload sets it, not the schedule that also
runs this job), **that step's behaviour changes on upgrade** — usually from
"always runs" to "runs only when the key is actually supplied and matches."

This was evaluated against two alternatives and this is the one that shipped;
see [What was considered and rejected](#what-was-considered-and-rejected) if
you want the reasoning, not just the outcome.

## Find affected jobs before upgrading

Unlike the [apply-time `if:` validation
migration](apply-time-if-validation.md), there is no `apply --dry-run` check
for this — the old behaviour compiled fine and only differed at evaluation
time, and evaluation depends on which params happen to be set on a given run,
which the CLI cannot know in advance.

Grep your job files for `if:` expressions that mention `params.` or
`secrets.` and, for each one, ask two questions:

1. **Is the key always set?** If it comes from `spec.params.inputs` with a
   `required: true` or a `default:`, it is always defined and this change
   does not affect that condition at all.
2. **Is the key set only sometimes** — a webhook-only field, an optional
   `--param`, a param only some callers of a `uses:` template pass? If so,
   the condition's behaviour when the key is absent is changing from "runs"
   to "evaluates against empty string" on this upgrade. Decide whether that
   is the behaviour you want; if not, add an explicit `default:` under
   `spec.params.inputs` so the key is never actually undefined, or rewrite
   the condition to state the fallback explicitly (e.g.
   `if: params.ENV == "" || params.ENV == "prod"` for "prod is the default").

## What did not change: `steps`

`conditionVars` — the table `params`, `vars`, `steps` and `secrets` are all
declared from — was audited for this same trap while fixing `params`.
`secrets` had it and is fixed alongside `params` in this same change. `steps`
also still has it (`steps.MISSING.outputs.KEY` still raises `no such key` and
still fails open) and is **deliberately left alone**:

- The fix used for `params`/`vars`/`secrets` defaults a missing key to the
  empty *string*, which only works because those three are all
  `map(string, string)`. `steps` is `map(string, dyn)` — the value under a
  step name is shaped like `{outputs: {...}}`, not a string — so defaulting
  a missing step name to `""` would not close the trap, only move it:
  `steps.nope.outputs.ok` would still error, now on "outputs is not a field
  on a string" instead of "no such key." Actually closing this needs a
  differently-shaped default and its own design pass.
- The ambiguity that justifies defaulting `params` to empty (see below) does
  not exist for `steps`: a step name in an `if:` is either one the job
  genuinely declares, or a typo. There is no equivalent of `params`' external
  pass-through paths where an undeclared step reference is a legitimate,
  supported thing to write.

If your fleet has an `if:` referencing a `steps.` name that does not match an
actual step, that condition still fails open today, exactly as before this
change. Double-check step names the same way you always had to.

## What was considered and rejected

**Rejecting an `if:` that names an undeclared `params` key at *apply* time**
was investigated and stays rejected, permanently — not because it wouldn't
have caught the typo above, but because it can't tell that typo apart from
legitimate, documented usage. `resolveParams` (`internal/controller/params.go`)
passes an undeclared param through by design; five separate paths can
introduce one that never appears under `spec.params.inputs`: CLI `--param`,
a re-triggered run, a webhook's `paramsMapping`, a `Schedule`'s `params`, and
a `call:` step's `with:` — and `spec.concurrency.orLocks` synthesizes
`{NAME}_LOCK_VALUE` keys on top of that. `unified-cd run trigger job --param
DEPLOY_TARGET=x` gating on `if: params.DEPLOY_TARGET == "x"` is supported and
works today, with no `spec.params.inputs` entry anywhere. A typo and that
legitimate pass-through reference are statically indistinguishable from the
job manifest alone, so rejecting one at apply time means rejecting the other.

The runtime fix documented on this page does not have that problem: it
changes nothing about which params are allowed to reach a run, only what an
*undefined* one evaluates to inside `if:` — and it brings `if:` in line with
how the same variable already behaves on the template side, where Go's
`missingkey=zero` option already makes `{{ .Params.MISSING }}` expand to
empty in `run:`, `env:` and `outputs:`. `if:` was the one place `params.X`
disagreed with `{{ .Params.X }}` about the same undefined `X`.

## Upgrade order

This lives entirely in the agent's condition evaluation (`internal/dsl`).
There is no wire format or controller-side change, so a mixed fleet during a
rolling upgrade is safe: some runs evaluate `params`/`secrets`-referencing
`if:` conditions with the old fail-open behaviour and some with the new
empty-string behaviour, and which one a given run gets depends only on which
agent version claims it.

## Related

- [Steps: Conditional Execution](../../user-guide/writing-jobs/steps.md#conditional-execution-if)
  — the full undefined-key table for `params`, `vars`, `steps` and `secrets`.
- [Apply-Time `if:` Validation](apply-time-if-validation.md) — the separate,
  earlier change that rejects a malformed `if:` at `apply`; this page is
  about the different case of an `if:` that compiles fine but names a key
  that turns out not to be set.
