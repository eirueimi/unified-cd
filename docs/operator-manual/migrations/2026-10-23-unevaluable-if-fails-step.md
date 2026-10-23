# Migrating: an `if:` that cannot be evaluated now fails the step

A step whose `if:` condition **cannot be evaluated at run time** used to
**run anyway** ("fail-open"). It now **does not run**: the step is reported
**Failed**, the error is written to the run's own log, and the run fails.

```yaml
steps:
  - name: build
    run: ./build.sh            # never declares an output named "result"
  - name: deploy
    if: 'steps.build.outputs.result == "ok"'
    run: ./deploy.sh --prod
```

**Before**, `steps.build.outputs.result` raised `no such key`, `EvalCondition`
returned `(true, nil, err)` ("fail-safe: run the step"), and `deploy` ran on
every trigger. For a deploy gate that is the worst direction to fail in. The
only trace was a `running step` warning in the agent's process log.

**After**, `deploy` is never executed, is reported **Failed** (not Skipped: a
silent skip would hide a broken gate), the run's own log (the `System` stream)
gets a line such as

```
unified-cd: step "deploy": if: expression "steps.build.outputs.result == \"ok\"" eval error: no such key: result — the condition could not be evaluated, so the step did NOT run and is reported Failed (fail closed)
```

and later steps are skipped by the normal implicit-success rule.

| Before | After |
|---|---|
| A compile, type (non-bool) or evaluation error in `if:` ran the step. | The step does not run; it is reported **Failed**; the run fails. |
| The reason was in the agent's slog only, plus a run-log line saying the step "RAN (fail-safe)". | The run-log line says the step did NOT run and is reported Failed. |
| `EvalCondition` returned `true` with the error. | `EvalCondition` returns `false` with the error, so a caller that ignores the error still cannot run the step. |

## What reaches this path

- **`steps` references** that name a step or output that is not set at the
  time (`steps.NAME.outputs.KEY`). `steps` deliberately keeps CEL's `no such
  key` error; undefined `params`, `vars` and `secrets` keys are **unchanged**:
  they read as `""` with a warning and the step is skipped or run on what the
  condition says (see [the params note](params-undefined-key-is-empty.md)).
- An `if:` that does not compile or is not boolean but got past
  [apply-time validation](apply-time-if-validation.md), for example one that
  only appears when a `uses:` template is resolved at trigger time.

## Rulings on the edge cases

- **`continueOnError: true`** on the gated step is honoured as for any failure
  of that step: reported Failed, work not run, but the run is **not** failed
  and later steps proceed.
- **`finally:` steps** (no implicit success): a finally step whose `if:` cannot
  be evaluated does not run its work and is Failed, which fails the run. The
  other `finally:` steps still run.
- **`retry:`** has nothing to retry: `if:` is evaluated once before any
  attempt, so the step fails immediately.
- **`matrix:`/`foreach:`**: each variant evaluates the condition and fails
  separately; the run's log gets one line per step name, not per variant.
- **`post:` and `cache:` hooks** do not evaluate `if:` themselves and are only
  registered by a step that ran, so a step that failed on its condition
  registers none.
- The standard and Kubernetes agents share one orchestration loop; behaviour is
  identical.

## What to do before upgrading

Find conditions that may error today and were silently running the step:

- Search for `steps.<name>.outputs.<key>` in `if:` and confirm that step really
  sets that output on every path (including when it is skipped).
- Make the producing step set the output on every path; a gate can no longer
  fall through to "run" when the output is missing.
- Check the run logs of existing jobs for `if: condition eval failed, running
  step` lines in the agent log: each is a gate that has been failing open.

## Upgrade order

The change lives entirely in the agent's orchestration loop and
`dsl.EvalCondition`; there is no wire-format or controller change. During a
rolling upgrade, which behaviour a run gets depends only on which agent version
claims it.

## Related

- [Steps: When an `if:` cannot be evaluated](../../user-guide/writing-jobs/steps.md#when-an-if-cannot-be-evaluated)
- [Apply-Time `if:` Validation](apply-time-if-validation.md)
- [Undefined `params:` key is empty in `if:`](params-undefined-key-is-empty.md)
