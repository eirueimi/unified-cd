# Migrating to bounded, failure-honest `finally:` semantics

`spec.finally` runs through its own pipeline, separate from the main `steps`
DAG. Several features were implemented and validated only on the main path, so
inside `finally:` they were accepted and then silently ignored at runtime —
no error, no warning, no status. Four of them are now fixed together, because
they share one cause and one file.

Two of the four change what an existing job does: a cleanup phase that used to
run forever now ends at a deadline, and a cancelled run whose cleanup broke now
finishes `Failed` instead of `Cancelled`. Read those two sections before
upgrading a fleet. The other two only start doing something that was already
documented, and cannot break a job that was working.

There is **no DSL change** and therefore no validation error to look for: no
field was added, renamed, or removed, and every job YAML that applied before
applies unchanged.

| Before | After |
|---|---|
| A `finally:` step with no `timeoutMinutes:` could run forever. `call:` was the sharpest case — the child-run poll's only bound is its context, and the cleanup phase had none — but a plain `run:` was unbounded the same way. | Each cleanup phase is bounded by the agent's `finallyTimeout` (default **10m**). A step still running at the deadline is interrupted, reported `Failed`, and the run finishes `Failed`. |
| A `post:`/`cache:` hook drain had no timeout either (`RunPostHook` takes none), so a hanging cleanup hook pinned the run indefinitely. | The same `finallyTimeout` bounds each of the two drains, and scope/claim-pod teardown. |
| — | A phase that hits the budget is recorded on the run itself, on its **System** stream, naming the phase and the budget. A truncated drain does not change the run's status, so that line is the only thing standing between a cut-off `cache:` save and a silently-lost cache. |
| A `finally:` step that exited non-zero on a **cancelled** run was reported `Cancelled`, and the run finished `Cancelled`. The failure was discarded. | The step is reported `Failed` and the run finishes `Failed` — as `spec.finally`'s documented contract already said. A `finally:` block that succeeds still leaves a cancelled run `Cancelled`. |
| `retry:` on a `finally:` step degraded to a single attempt on a cancelled run, and a genuine exec error left the step's log empty. | The step keeps its full attempt budget, and the `failed to execute: …` diagnostic is written to its log. (A main-DAG step's retry loop still stops on cancellation.) |
| An output declared in `spec.params.outputs` and set by a `finally:` step reached that step's outputs and stopped there — `SetRunOutputs` never carried it, so a parent `call:` step read nothing. | The value is promoted to the run's outputs like any other. |

---

## Change 1: `finally:` is no longer unbounded

**What changes.** The cleanup phase now carries a deadline. It is applied per
phase, not as one shared total for the run, because the phases are separated by
the main DAG, which may legitimately run for hours.

**The number to plan for.** Four phases carry the budget, in this order:

1. the `post:`/`cache:` hook drain that follows the main `steps` DAG
2. the `finally:` pipeline
3. the `post:`/`cache:` hook drain that follows `finally:`
4. scope/claim-pod teardown

So the worst case a run can spend after its DAG finishes is **four ×
`finallyTimeout` — 40 minutes at the default**, not 10. If you size a
rollout's `terminationGracePeriodSeconds`, a node-drain timeout, or an
alerting threshold against this setting, size it against that number. It is a
ceiling and not an expectation — reaching it means four separate phases each
wedged — but it is the bound, and it is the one an operator has to plan for.

The one post-DAG phase deliberately left **unbounded** is the final report to
the controller (`FinishRun`/`SetRunOutputs`), which retries until it lands. It
runs no job code; it stops at once on any 4xx (a run the controller already
reaped or deleted); and if the agent gave up on a persistent 5xx the run would
stay `Running` at the controller forever, because the stuck-run reaper keys on
**agent** liveness and an agent that gave up is still heartbeating. In a full
controller outage the agent's own heartbeats fail too, so it goes stale and the
reaper does take over.

**Why it did not have one.** `finally:` deliberately ignores run cancellation;
that is the entire reason it exists. The mechanism for that
(`context.WithoutCancel`) also discards the job-level `spec.timeoutMinutes`
deadline, and yields a context whose `Done()` channel is `nil` — so every
`select` on it blocked forever rather than firing. Nothing else caught it: the
controller's parent-finish cascade only runs once the parent reaches
`FinishRun`, which cannot happen until `finally:` returns, so a cancelled
parent and a never-claimed child could deadlock each other; and the stuck-run
reaper keys on **agent** liveness, which stayed healthy because the agent kept
heartbeating throughout.

**Why not `spec.timeoutMinutes`.** Reusing the job budget would give a six-hour
job a six-hour `finally:`, which bounds nothing in the case that matters.

**Why not a DSL field.** Per-step `timeoutMinutes:` already works inside
`finally:` and is the precise control. What was missing was only a ceiling for
steps that set nothing.

**The setting.**

| Agent | Config key | Env | Flag | Default |
|---|---|---|---|---|
| standard | `finallyTimeout` | `UNIFIED_AGENT_FINALLY_TIMEOUT` | `--finally-timeout` | `10m` |
| Kubernetes | `finallyTimeout` | `UNIFIED_K8S_FINALLY_TIMEOUT` | — | `10m` |

A **non-positive** value falls back to `10m`. "Unbounded" is deliberately not
expressible — it is the defect this closes.

An **unparseable** value does *not* fall back — check this before a cutover,
because it decides whether a typo is silent or loud:

| Where | Unparseable value (e.g. `10 minutes`) |
|---|---|
| k8s agent, `finallyTimeout:` in the config file | `Config.Validate` rejects it, the agent logs `invalid config: finallyTimeout "10 minutes": …` and **exits 1**. It never starts on a defaulted budget. |
| standard agent, `finallyTimeout:` in the config file | YAML decode fails, the agent logs `config file: … cannot unmarshal !!str` and **exits 1**. |
| standard agent, `--finally-timeout` flag | `flag` rejects it and the agent **exits 2**. |
| standard agent, `UNIFIED_AGENT_FINALLY_TIMEOUT` | **Ignored**, leaving the 10m default. This is the one place a typo is silent, and it matches every other env duration override on that agent. |

**What you will see when a job hits it.** The step's own log gains

```
unified-cd: step "<name>" failed to execute: context deadline exceeded
```

on stream `stderr`, and the step is reported `Failed`. Separately, the run's
own **System** stream (stepIndex `-1`) gains a line naming the phase and the
budget:

```
unified-cd: the finally: phase did not finish: it hit the 10m cleanup budget (finallyTimeout) and was stopped. Work still in flight was interrupted and anything not yet started was skipped.
```

That record is per phase, and what it means for the run's status differs by
phase:

| Phase truncated | Run status |
|---|---|
| the `finally:` pipeline | **`Failed`** — the phase did not finish, so the run did not fully succeed. This holds even when every step in the block is `continueOnError: true`: that flag scopes one step's failure; whether the phase finished is not that step's to suppress. |
| either `post:`/`cache:` hook drain | **unchanged** — a post hook has never changed a run's status, whatever it fails on, and singling out "failed because the budget expired" while "failed because the object store was down" stayed benign would be an incoherent rule. |
| scope/claim-pod teardown | **unchanged**, and not recorded on the run: it happens after the run is already terminal, and a leaked container is an agent-host fact. It is logged on the agent as `scope/pod teardown truncated by its finallyTimeout budget`. |

**Watch the middle row.** It is the case where the old promise that post-hooks
"always wait for completion to preserve data" (the standard agent's
`--drain-timeout` help text said exactly that; it has been corrected) no longer
holds: a multi-gigabyte `cache:` save that runs past the budget is cut off, not
saved, on a run reported `Succeeded`. Nothing else about that run says so. If
cache correctness matters to your fleet, alert on the System line.

**What to do.**

1. Grep your jobs for `finally:` blocks containing a `call:`, or any step that
   waits on something external, and give those steps an explicit
   `timeoutMinutes:`. That is the right control, and it was always available.
2. If a fleet's teardown genuinely runs longer than ten minutes (large cache
   saves, slow rollbacks), raise `finallyTimeout` for that fleet.
3. If a `finally:` step starts failing at the deadline and you did not know it
   was slow, that is the fix working: before this change it was not finishing,
   it was hanging.

---

## Change 2: a failing `finally:` step now fails a cancelled run

**What changes.** A user cancels a run; a `finally:` step then exits non-zero
because cleanup genuinely broke. Previously the step's `Failed` was rewritten
to `Cancelled` — the rewrite was applied whenever the run had been cancelled,
regardless of which phase the step belonged to — and because the failure was
only recorded for steps whose status stayed `Failed`, the failure was dropped
entirely. The run finished `Cancelled` and the broken teardown was invisible.

Now the rewrite is phase-aware. A `finally:` step's context is never cancelled,
so a non-zero exit there is a real failure: the step is reported `Failed` and
the run finishes `Failed`.

This was already the documented contract in two places — `spec.finally`'s own
description ("a `finally` step that fails marks the run **Failed**") and the
agent's `suppressOnCancel` flag, which is set to "a genuine finally failure
counts even when the run was cancelled". It simply was not implemented for
`run:` steps. `cache:` and artifact steps in `finally:` already behaved this
way, which is why the behaviour looked inconsistent from outside.

**What stays the same.**

- A cancelled run whose `finally:` block succeeds still finishes `Cancelled`.
- The main-DAG step interrupted by the cancellation is still reported
  `Cancelled`, not `Failed`.
- `continueOnError: true` on a `finally:` step still suppresses **that step's**
  failure. It does not suppress a truncated phase: if the budget expires, the
  run finishes `Failed` however the individual steps are marked (see Change 1).

**What to do.** Nothing, unless a dashboard, alert, or automation treats
`Cancelled` and `Failed` differently. If it does, expect a new class of
`Failed` run: cancelled runs whose cleanup was already broken and was being
hidden. Fix the cleanup, or mark the step `continueOnError: true` if its
failure genuinely does not matter.

---

## Change 3: `retry:` in `finally:` keeps its attempts (no action needed)

The attempt loop's "stop on cancellation" check was consulted globally rather
than per phase, so a `retry:` on a `finally:` step ran once and gave up on a
cancelled run — exactly when a flaky teardown most needs its retries. It now
uses its full budget there. A main-DAG step's retry loop still stops on
cancellation: cancelling a run means stopping it.

Related: the `unified-cd: step "<name>" failed to execute: …` diagnostic was
suppressed on a cancelled run, so a `finally:` step whose exec genuinely failed
produced an empty log. It is now written for `finally:` steps.

This can only turn a silent single-attempt failure into either a success or a
properly logged failure. No job that worked before behaves differently.

---

## Change 4: `finally:` step outputs reach the run (no action needed)

The job-output promotion loop scanned the main `steps` DAG only, so a value a
`finally:` step published landed in the step's outputs, the run finished, and
`SetRunOutputs` never carried it — a parent `call:` step reading that output
got nothing. Both phases are scanned now.

**Collision rule.** If a main-DAG step and a `finally:` step both set the same
declared output name, the **`finally:` value wins**. That is the same "the last
step that declares it wins" rule that already applied between two main-DAG
steps, and it is the useful direction: a teardown step recording what was
actually left in place should override a provisional value from the main DAG.

"Last" is **declaration order** — the order the steps are written in the job —
not the order they finished. The promotion loop walks the claim's stages, and
the steps within each stage, exactly as written. For sequential stages the two
orders coincide; inside one `parallel` group they do not, and the winner is
still the step written last, which is the only reproducible answer.

This can only add a value where there was none, or replace a main-DAG value
that the job author explicitly asked a later step to overwrite.

---

## Upgrade order

Nothing here touches the controller, the database, or the wire format — the
whole change lives in the agents' shared orchestration loop. Upgrade agents in
any order; a mixed fleet is safe, it simply means some runs get the old
`finally:` behaviour and some the new until the rollout completes.

If you want the new bound but not the new default, set `finallyTimeout`
explicitly before rolling the agents out.

## Related

- [Approval and Finally](../../user-guide/writing-jobs/approval-and-finally.md#finally-block-finally)
- [Agents: Bounding a run's cleanup phase](../agents.md#bounding-a-runs-cleanup-phase-finallytimeout)
- [Configuration Reference](../../reference/configuration.md)
