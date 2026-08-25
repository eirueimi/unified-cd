# `finally:` post-hooks and cache saves were silently dropped

Branch: `fix/finally-post-hooks`

## Summary

`RunClaim` (`internal/agent/orchestrator.go`) drained its two deferred-hook
slices - `postHooks` (cache saves, appended by `executeCacheStep`) and
`hookStack` (`post:` hooks, appended by `makeStepRunner`) - exactly once,
between the main DAG and the `finally` pipeline, and never again. Anything a
`finally:` step registered landed on a stack nobody popped: the `post:` script
never ran, the `cache:` save never happened, and nothing reported it. Not the
step status (the step still reported `Succeeded`), not the run status, not even
an agent-side `slog` line.

Both agents share this one loop - the k8s agent calls `agentlib.RunClaim` - so
the defect and the fix are backend-independent.

The fix: `drainHooks` is now a closure called **twice**, once after the main
DAG (exactly where the old single drain was) and once after the `finally`
pipeline. Each call detaches both slices under `postHooksMu` and resets them to
`nil` in the same critical section, so calling it repeatedly can never re-run a
hook.

A second, related change fell out of proving the bug (see "Reachability"
below): the DSL parser used to *reject* `cache:` and `post:` in `finally:`
steps, with the now-obsolete orchestrator behaviour as the stated reason. That
restriction is lifted. `approval:` in `finally:` stays rejected.

## Proving it first

`internal/agent/orchestrator_finally_hooks_test.go` was written before any
production change and confirmed failing against `main`:

- `TestExecuteRun_FinallyStepPostHook_Runs` - the hook's marker file was never
  created (`The system cannot find the file specified`).
- `TestExecuteRun_FinallyStepCacheSave_Runs` - `cache.Restore` returned
  `hit == false`; the deferred save never ran. The two hook slices are
  independent mechanisms, so both were checked separately, as asked.
- `TestExecuteRun_HookDrainOrder_MainBeforeFinally_LIFOWithinEach` - observed
  `[main-post-2, main-post-1, finally-body-1, finally-body-2]`, i.e. the main
  batch drained correctly and in LIFO order while the finally batch never
  drained at all.

The parity case was likewise confirmed failing on **both** drivers with the
second `drainHooks()` call temporarily removed:

```
--- FAIL: TestParity_HostAgent/finally-post-hook-runs
    finally-post-hook-runs: failed to read marker file ... (no post: hook ever ran?)
--- FAIL: TestParity_K8sAgent/finally-post-hook-runs
    finally-post-hook-runs: postExec invocation order = [], want [echo finally-post-2 ..., echo finally-post-1 ...]
```

## The four questions

### 1. Ordering - two drains, and the drain is idempotent

Three options were on the table: move the existing drain past `finally`, add a
second drain, or make one idempotent drain that runs twice. The answer is "add
a second drain, and make the drain idempotent" - those are complementary, not
alternatives.

**Not "move it".** A step's `post:` hook is *that step's* cleanup. Moving the
single drain to after `finally` would make every normal step's cleanup wait on
the job-level `finally` block, which can be long-running. It would also change
already-observable behaviour: a `finally:` step would begin executing while a
main step's container teardown (the canonical `post:` example in the docs) had
not yet run, so `finally:` would observe a different world than it does today.
The existing ordering is load-bearing and stays exactly as it was.

**Two drains, not "one drain called twice by accident".** Drain #1 sits where
it always sat; drain #2 sits immediately after the `finally` pipeline and
before the deferred `b.CloseScopes`, so a scoped `finally:` step's hook still
finds its scope container alive. Drain #2 is deliberately *outside* the
`RunPipeline` error check: a structural error in the finally pipeline does not
un-register hooks its steps already pushed, and skipping them would reintroduce
the exact silent-skip bug.

**Idempotence is what makes two drains safe.** `drainHooks` detaches both
slices and nils them under the mutex, so each hook is handed to exactly one
drain. This is not a nicety - without it, a second call would re-run every
main-DAG hook (a container teardown twice; a cache save of a workspace that
`finally:` may have just mutated). It also means the call is cheap and total:
it can be placed anywhere without an "is there anything to do?" guard.

The resulting ordering, pinned by
`TestExecuteRun_HookDrainOrder_MainBeforeFinally_LIFOWithinEach`:

```
main DAG bodies -> main cache saves -> main post: hooks (LIFO)
  -> finally bodies -> finally cache saves -> finally post: hooks (LIFO)
```

### 2. Failure and cancellation

`finally:` exists for the case where things went wrong, so its hooks matter
most exactly when the machinery is winding down. Three properties make that
work, and all three are asserted:

- **Context.** Both drains use the same `hookCtx = context.WithoutCancel(ctx)`.
  A cancelled, timed-out, or reaped run does not skip cleanup. This mirrors
  `finally` itself, which already runs on a `WithoutCancel` context.
- **Position.** Drain #2 is before the `reapedByMaster` early return and before
  the deferred `CloseScopes`/poller teardown, so no wind-down path can outrun
  it. `TestExecuteRun_FinallyStepPostHook_RunsWhenCancelled` drives a real
  controller-side cancellation and asserts the hook still ran (skipped on
  Windows for the same pre-existing reason `TestExecuteRun_CancelPropagation`
  is - `retryUntilSuccess` on a `WithoutCancel` context keeps retrying after
  the test server closes).
- **Registration.** A `post:` hook is registered only when its owning step
  reached `Succeeded` - unchanged, and correct: a `finally:` step that itself
  failed did not necessarily create the resource its hook would tear down.
  Cache saves register on the cache step's success and then always drain, so a
  later failure does not lose the save.
  `TestExecuteRun_FinallyStepPostHook_RunsWhenMainFailed` covers the
  main-DAG-already-failed case, and the parity case builds on it too.

A hook that *itself* fails is logged (`slog.Warn("post step failed")`) and does
not change the run's status. That was already true for main-DAG hooks and is
now identically true for finally hooks - one rule, not two. Making a finally
hook's failure fail the run would have been a second, unrelated behaviour
change smuggled in under a bug fix.

### 3. Concurrency

Both production backends report `Concurrent` (the k8s agent since `07b9d0d`),
and `finally:` runs through the very same `RunPipeline`/`makeStepRunner`, so
several `finally:` steps in a `parallel:` group register hooks from concurrent
goroutines. Every append was already under `postHooksMu`; the drain now takes
the same lock rather than relying on "the pipeline has returned, so no lock is
needed". That is strictly stronger, and it is what makes the detach-and-reset
atomic with respect to any late append.

`TestExecuteRun_ParallelFinallyPostHooks_ConcurrentAppendIsSafe` - the finally
twin of the existing `TestExecuteRun_ParallelPostHooks_ConcurrentAppendIsSafe` -
drives 8 concurrent `finally:` members each with its own `post:` hook and
checks **every** marker file, so it catches a lost append as well as a race.
Clean under `-race`.

### 4. LIFO

The promise, as encoded by the existing `post-hooks-lifo` parity case and its
two drivers, is: `post:` hooks run in reverse *registration* order - the
last-registered hook runs first. Cache saves are separate and run in
registration order, before any `post:` hook in the same drain.

That promise is now explicitly a **within-batch** guarantee. A strict whole-run
LIFO would require finally-registered hooks (the last registered) to run
*first*, which is impossible: they do not exist until `finally` has run, and by
then the main batch has already drained. Documented as such in the code and in
`docs/user-guide/writing-jobs/steps.md`. The existing case still passes
unchanged, and the new ordering test pins both halves in one sequence.

## Reachability: the parser was gating this, and leaking

While checking the docs I found `docs/user-guide/writing-jobs/approval-and-finally.md`
documented the defect as a limitation, and `internal/dsl/parse.go` enforced it:
`cache: is not supported in finally steps` / `post: is not supported in finally
steps`, with the comment *"pass false for finally entries because the agent
drains postHooks/hookStack BEFORE running finally"*.

That gate was never airtight. `uses:` **is** documented as supported in
`finally:`, a template's own `spec.steps` may legally carry `post:`/`cache:`,
and after `gittemplate` inlining the resolved spec is **not** re-validated -
`resolveGitPendingRuns` (`internal/controller/scheduler.go`) only re-checks
container references. So a `uses:` cleanup template with a `post:` hook,
referenced from `finally:`, reached the agent and had its hook silently
dropped. The bug was user-reachable through a documented path.

Since the gate's stated reason no longer holds and the gate did not actually
hold either, it is removed rather than left half-enforcing:

- `internal/dsl/parse.go` - the `allowDeferredHooks` parameter is renamed
  `allowApproval` and now gates only `approval:`. `approval:` stays rejected in
  `finally:`: a cleanup phase must run unattended after a failure or a
  cancellation, so it must never block on a human.
- `internal/dsl/parse_test.go` - `TestParse_FinallyRejectsCache/Post` become
  `TestParse_FinallyAcceptsCache/Post`, plus
  `TestParse_FinallyParallelAcceptsPost` for the `parallel:` branch, which
  carried its own copy of the rejection.
- Docs updated in `approval-and-finally.md` (the limitation bullet is replaced
  by the drain-ordering rule plus the `approval:` carve-out) and `steps.md`
  (the `post:` section now states the two-pass ordering).

This is a relaxation: previously-rejected YAML now parses. Nothing that parsed
before stops parsing, so no migration guide is needed. The generated artifacts
(`schemas/unified-cd.schema.json`, `docs/reference/field-reference.md`) are
untouched - the restriction was Go-level validation, never in the schema - and
no example or template referenced it.

## Parity case

Yes: `finally-post-hook-runs`, case 15 in `internal/paritycases/scenarios.go`.
The main DAG fails, then two `finally:` steps each carry a `post:` hook.

It asserts **outcomes only** - step statuses, run status, and each finally
step's own log line - in the shared `Expectation`, plus, per driver, that both
hooks were invoked and in LIFO order. Nothing anywhere asserts on timing or
duration.

The per-driver split mirrors the existing `post-hooks-lifo` case and exists for
the same reason: `parityK8sBackend.RunPostHook` is a fake that records
invocation order without executing the script, so hook output never reaches the
shipped logs on that driver. The host driver observes `$POSTHOOK_MARKER_FILE`
append order; the k8s driver observes the fake's recorded scripts. Both
assertions treat "nothing recorded at all" as a hard failure, which is exactly
how a silently-dropped hook presents. Both drivers' assertion helpers were
generalised (`assertPostHookOrderFromMarkerFile`, `assertK8sPostHookOrder`) so
the old case and the new one share one implementation.

## Verification

```
go build ./...                                                        PASS
go vet ./...                                                          PASS
go test ./... -short -count=1                                         PASS
go test ./internal/agent/... ./internal/k8sagent/... \
        ./internal/paritycases/... -race -count=1                     PASS (no races)
```

Two flakes appeared on the first pass of the full suite and both passed in
isolation and on re-run of their package:
`TestRetry_PerAttemptTimeoutThenSucceeds` and
`TestStartHeartbeat_TicksUntilCtxDone`. Both are wall-clock-sensitive and
neither touches the code changed here. The machine was heavily loaded - see
"Environment note".

Neither of the two known out-of-scope problems was chased. The
`RetryInitialWait`/`RetryMaxWait` race did not fire at all. A stray
`internal/agent/working0/test/.ucd-mode` did appear during a full-suite run; it
was deleted rather than committed, and left to the branch that owns it.

## Environment note

The dev machine had ten orphaned `yes` processes (started about two hours
before this work, almost certainly leaked by an `exec_tree` process-tree-kill
test on another branch) saturating all 8 logical cores, which stretched
`go build ./...` past ten minutes and caused the flakes above. They were
dropped to `Idle` priority rather than killed, so nothing another agent might
still be observing was destroyed. They are still running and worth cleaning up.

## Files

- `internal/agent/orchestrator.go` - `drainHooks` closure; second call after
  `finally`; doc comments.
- `internal/agent/agent.go` - `postHookEntry` doc comment updated for two drains.
- `internal/agent/orchestrator_finally_hooks_test.go` - new; six regression tests.
- `internal/paritycases/scenarios.go` - `finallyPostHookRuns` case and the two
  exported expected-order fixtures.
- `internal/agent/parity_host_test.go`, `internal/k8sagent/parity_k8s_test.go` -
  drivers wired to the new case; assertion helpers generalised.
- `internal/dsl/parse.go`, `internal/dsl/parse_test.go` - validation relaxed.
- `docs/user-guide/writing-jobs/approval-and-finally.md`,
  `docs/user-guide/writing-jobs/steps.md` - documented behaviour updated.
