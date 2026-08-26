# Approval and Finally

## Approval Step (`approval`)

An approval step pauses the run and waits for a human decision before continuing.
The agent is held (blocked) until the step is approved, rejected, or it times out.

```yaml
spec:
  steps:
    - name: build
      run: ./build.sh

    - name: gate-deploy
      approval:
        message: "Approve deployment to production?"
        timeoutMinutes: 30   # optional; default 60

    - name: deploy
      run: ./deploy.sh
```

### How to approve or reject

Any authenticated user can make a decision through the CLI, the Web UI, or the API:

**CLI:**

```bash
unified-cli approve <run-id> <step-index>
unified-cli reject  <run-id> <step-index> [--comment "reason"]
```

**API:**

```
POST /api/v1/runs/{runID}/approvals/{stepIndex}
```

Body: `{"decision": "approved"}` or `{"decision": "rejected", "comment": "reason"}`

**Web UI:** Approve / Reject buttons appear on the run detail page while the step is waiting.

### Behavior

- An **approval** allows the run to continue with the next step.
- A **rejection** fails the approval step immediately; the run fails and the `finally` block runs.
- A **timeout** also fails the step (the agent fails the step after `timeoutMinutes`); the run fails
  and the `finally` block runs.
- The identity of the decider is recorded (`decidedBy`) in the audit record.

### `approval` fields

| Field | Type | Required | Description |
|---|---|---|---|
| `message` | string | No | Human-readable prompt shown to approvers in the UI and CLI. |
| `timeoutMinutes` | number | No | Minutes to wait before the step is failed automatically. Default: 60. |

### Constraints and v1 limitations

- `approval` is **not allowed** in a `finally` block.
- The agent is held while waiting. Prefer short timeouts or set `timeoutMinutes` explicitly to
  avoid blocking the agent for an extended period.
- When the step times out, the agent fails the step itself, so the run is correctly marked as
  Failed. The approval audit row in `run_approvals` is reconciled separately: a leader-elected
  controller reaper marks any expired `Pending` row as `TimedOut` (with `decidedBy` = `system`)
  within roughly one minute. The reaper only fixes the audit row — it never changes run status.
- **A gate stops accepting decisions once it can no longer affect the run.** The approve/reject
  endpoint refuses, with `409`, when
  - the run has already reached a terminal state (`409 run is already terminal; approvals are no
    longer accepted`), or
  - the gate is past its `timeout_at` (`409 approval window has expired; the step already timed
    out`) — i.e. in the window before the reaper has relabelled the row `TimedOut`.

  Both checks are enforced in the same statement that writes the decision, so a run terminalizing
  concurrently with a decision cannot slip between them. This is why `run_approvals` can be read as
  an audit record: a row reading `Approved` by a named principal means the decision was accepted
  while the gate was genuinely open. It also means a decision can no longer cause the post-gate step
  to execute on a run the operator has already cancelled.

  The Web UI still renders Approve/Reject on a gate step whose run has since gone terminal (the step
  row keeps its `WaitingApproval` status); clicking it now returns the `409` above instead of
  recording a decision.

---

## Finally Block (`finally`)

Steps under `spec.finally` run **after the main `steps` DAG completes** —
whether it succeeded, failed, or was cancelled. Use it for notifications,
cleanup, or rollback.

```yaml
spec:
  steps:
    - name: deploy
      run: ./deploy.sh
  finally:
    - name: notify          # no if: → always runs
      run: ./notify.sh "{{ .Params.env }}"
    - name: rollback
      if: failure()         # only when a step failed
      run: ./rollback.sh
```

- `finally` uses the same structure as `steps` (stages + `parallel`).
- A `finally` step with no `if:` always runs.
- All `finally` steps run to completion; a `finally` step that fails marks the
  run **Failed**.
- On cancellation, `finally` still runs, but `failure()` is `false`.
- **A `finally` step that fails on a cancelled run still fails the run.** The
  cancellation ends the main `steps` DAG; it does not excuse cleanup. A
  `finally` step is reported `Failed` (not `Cancelled`) when it exits non-zero,
  and the run finishes `Failed` rather than `Cancelled` — so an operator who
  cancels a deploy and whose rollback then breaks sees the broken rollback
  instead of a clean cancellation. A `finally` block that cleans up
  successfully leaves a cancelled run `Cancelled`, as before.
- **`retry:` keeps its full attempt budget inside `finally`, even on a
  cancelled run** — for the same reason. (In the main DAG a cancellation still
  stops the retry loop at the current attempt: cancelling a run means stopping
  it.)
- `cache:` and `post:` **are** supported in `finally` steps. Their deferred
  hooks (the cache save, the post script) run after the whole `finally` block
  completes, in their own drain pass — a normal step's `post:` hook still runs
  before `finally` starts, so it never waits on cleanup. Within each pass,
  `post:` hooks run last-registered-first.
- `approval:` is **not** supported in `finally` steps: the cleanup phase has to
  run unattended after a failure or a cancellation, so it must never block on a
  human.
- [`uses:`](templates-and-reuse.md#git-template-inlining-uses) is supported in `finally` steps and
  is resolved the same way as in `steps:` — a common pattern is a `uses:`
  notification/cleanup template that only needs to run on completion.
- Both the standard and Kubernetes agents detect mid-run cancellation: an
  in-flight step is interrupted, `finally` still runs (with `failure()` false),
  and the run finishes as `Cancelled` — unless a `finally` step itself fails,
  in which case the run finishes `Failed` (see above).

### Job outputs from a `finally` step

A `finally` step is an ordinary step, so it can set a value declared in
[`spec.params.outputs`](parameters.md#output-fields), and that value **is** promoted to the
run's outputs — which is what a parent [`call:`](templates-and-reuse.md#calling-other-jobs-call) step
reads back.

```yaml
spec:
  params:
    outputs:
      - name: report_url
  steps:
    - name: test
      run: ./test.sh
  finally:
    - name: publish-report
      run: ./publish.sh
      outputs:
        report_url: "{{ .Stdout }}"
```

If a main-DAG step and a `finally` step both set the same declared output name,
**the `finally` value wins**. This is the same "the step that ran last wins"
rule that already applies between two main-DAG steps, and it is the useful
direction: a teardown step recording what was actually left in place should
override a provisional value from the main DAG.

### How long `finally` may run

`finally` deliberately ignores run cancellation — that is the entire point of
the block. Because of that it also cannot inherit the job-level
`spec.timeoutMinutes` deadline, so the agent applies its own ceiling instead:
**`finallyTimeout`, 10 minutes by default**, configurable per agent
(`--finally-timeout` / `UNIFIED_AGENT_FINALLY_TIMEOUT` on the standard agent,
`finallyTimeout` / `UNIFIED_K8S_FINALLY_TIMEOUT` on the Kubernetes agent — see
the [Configuration Reference](../../reference/configuration.md)).

The ceiling applies separately to each cleanup phase: the `finally` pipeline
itself, and each `post:`/`cache:` hook drain. A step still running when the
budget expires is interrupted, reported `Failed`, and the run finishes
`Failed`.

Per-step `timeoutMinutes:` works inside `finally` and is the precise tool —
set it on any cleanup step that talks to something that can hang (a `call:`
to a teardown job, a webhook, a cloud API). The budget is only the backstop
for steps that set nothing.

---

