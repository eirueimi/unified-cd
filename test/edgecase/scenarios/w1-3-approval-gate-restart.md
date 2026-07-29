# W1-3 — controller restart while a run awaits an approval gate

- **Invariants:** I1 (run accounting), I7 (approval/gate integrity — a
  human-decision row and the run's own status must agree, and a pending
  decision must not be lost or silently re-decided by infrastructure churn)
- **Stack:** plain `test/ha` compose, no overlay.
- **Workload:** `test/edgecase/workloads/approval.payload.json` (job
  `edge-approval`, three native steps: `before` (`echo before-gate`), `gate`
  (an `approval` step, `timeoutMinutes: 10`, message `edge-campaign gate`),
  `after` (`echo after-gate`)). Pause/resume does not exist in unified-cd —
  an approval gate is the only wait-state a run can sit in, which is why
  this scenario (unlike W1-1/W1-2's mid-execution kill) times the controller
  outage around the gate instead of around a running step.
- **Verified API (do not re-derive):**
  - `GET /api/v1/runs/{id}/approvals` -> array of
    `{runId, stepIndex, stepName, message, status, createdAt, timeoutAt,
    decidedBy, decidedAt}`; `status` is `Pending|Approved|Rejected|TimedOut`.
  - `POST /api/v1/runs/{id}/approvals/{stepIndex}` body
    `{"decision":"approve"}` -> `204` on success, `404` if there is no
    pending row for that step, `409` if it was already decided.
  - `GET /api/v1/runs/{id}/logs` for the `after-gate` line once the run
    resumes.
- **Mechanism under test — two independent clocks, not one:**
  - `internal/controller/approval_reaper.go` (`RunApprovalReaper`) ticks
    once a minute under the `approvalReaperLockKey` advisory lock (leader-
    only) and calls `store.MarkExpiredApprovalsTimedOut`, which flips any
    `Pending` row whose `timeoutAt` has passed to `TimedOut`. It only
    touches the approval audit row — it does **not** itself fail the run.
    This can only run while at least one controller is up and holds the
    lock.
  - `internal/agent/approval.go` (`WaitForApproval`) computes its own
    `deadline := time.Now().Add(timeoutMin * time.Minute)` locally on the
    agent, once, right after the `CreateApproval` call returns (so it is a
    few ms *after*, and therefore a few ms *later* than, the server's own
    `timeoutAt`, which the controller stamps at `time.Now()` inside
    `handleAgentCreateApproval` when it receives that same request). The
    agent polls `GetApproval` on a fixed interval and independently checks
    `time.Now().After(deadline)` every loop — this check does not require
    the controller to be reachable at all, so it keeps ticking through a
    controller-side outage. If the agent's own deadline elapses before it
    ever observes `Approved`, `WaitForApproval` returns `false` and the
    orchestrator reports the step `Failed` (`orchestrator.go:362-368`) and
    marks the run for eventual `Failed` completion via `recordFailure()` —
    entirely independent of whether the controller-side reaper has (or even
    can, mid-outage) marked the row `TimedOut` yet.
  - The step-Failed `ReportStep` call on the approval-timeout path
    (`orchestrator.go:363-366`) is a **single best-effort attempt**
    (`_ = client.ReportStep(...)`, no `retryUntilSuccess` wrapping), unlike
    the run's final `FinishRun` call, which *is* wrapped in the unbounded
    `retryUntilSuccess` (`orchestrator.go:787-789`). So if the agent's local
    timeout fires while controllers are down, the step-level "Failed" report
    may be dropped, but the run-level terminal status is still guaranteed to
    land eventually once a controller is reachable again.
  - Net effect Part B is built to expose: the run's *authoritative* fate is
    decided by the agent's independent deadline, not by the reaper — the
    reaper is a housekeeping pass over the audit table, not the trigger for
    run failure. If the outage straddles both clocks, the row can still say
    `Pending` for a while after the run has already effectively failed
    (agent-side), and the row only catches up to `TimedOut` whenever a
    controller restarts and its reaper's next tick fires. **This part hunts
    for the row settling to something other than `TimedOut` (e.g. staying
    `Pending` forever, or racing to `Approved`) while the run is `Failed`,
    or vice versa** — that disagreement is the I7/C11-class violation.

## Baseline (healthy stack, no overlay)

```bash
cd test/ha
docker compose -f docker-compose.ha.yaml up -d --build
curl -fsS localhost:18080/readyz            # expect: ok (retry until up)
```

BASELINE GATE — confirm before injecting anything:

```bash
# exactly one leader elected:
for c in controller1 controller2 controller3; do
  echo "== $c"; docker compose -f docker-compose.ha.yaml logs $c 2>/dev/null | grep -c "scheduler became leader"
done
curl -s -o /dev/null -w '%{http_code}\n' localhost:18080/readyz   # expect 200
```

Apply the job and trigger a run, then confirm the gate is reachable
(`Pending`) before relying on the baseline for either part:

```bash
curl -fsS -X POST localhost:18080/api/v1/jobs \
  -H "Authorization: Bearer ha-admin-token" -H "Content-Type: application/json" \
  --data-binary @../edgecase/workloads/approval.payload.json

curl -fsS -X POST localhost:18080/api/v1/runs \
  -H "Authorization: Bearer ha-admin-token" -H "Content-Type: application/json" \
  -d '{"jobName":"edge-approval"}'
# capture .id as RUN_ID

for i in $(seq 1 10); do
  date
  curl -fsS "localhost:18080/api/v1/runs/<RUN_ID>/approvals" -H "Authorization: Bearer ha-admin-token"
  echo
  sleep 3
done
# expect one element, status:"Pending", once `before` has run
```

If baseline is broken (no leader, readyz not 200, or the gate never reaches
`Pending`), STOP and report BLOCKED with the evidence above rather than
proceeding to injection. This baseline run can double as the start of Part A
if the gate is confirmed `Pending`; otherwise re-trigger fresh for Part A.

## Part A — approve across a full-controller restart

Using the baseline run's `RUN_ID` (or a fresh trigger), poll until the gate
shows `Pending` and record `stepIndex` and `timeoutAt` verbatim:

```bash
curl -fsS "localhost:18080/api/v1/runs/<RUN_ID>/approvals" -H "Authorization: Bearer ha-admin-token"
# record: .[0].stepIndex, .[0].timeoutAt, .[0].status (expect "Pending")
```

Kill all three controllers hard, back to back, and confirm the LB has no
live upstream:

```bash
date
../edgecase/tools/inject.sh kill-hard controller1
../edgecase/tools/inject.sh kill-hard controller2
../edgecase/tools/inject.sh kill-hard controller3

for i in $(seq 1 6); do
  date; curl -s -o /dev/null -w '%{http_code}\n' localhost:18080/readyz
  sleep 10
done
# expect 502/503 (or curl connection error) throughout this loop (~60s outage)
```

Restart all three:

```bash
date
docker compose -f docker-compose.ha.yaml up -d controller1 controller2 controller3
```

Poll `/readyz` until 200, then re-fetch the approval row and diff against
the recorded `stepIndex`/`timeoutAt` (I7 — the row must survive the outage
unchanged, not be recreated or have its deadline shifted):

```bash
for i in $(seq 1 8); do
  date; curl -s -o /dev/null -w '%{http_code}\n' localhost:18080/readyz
  sleep 5
done

curl -fsS "localhost:18080/api/v1/runs/<RUN_ID>/approvals" -H "Authorization: Bearer ha-admin-token"
# compare .[0].stepIndex and .[0].timeoutAt against the pre-outage values;
# status should still be "Pending" (outage was well under timeoutMinutes)
```

Approve, and time the resume:

```bash
date
curl -fsS -X POST "localhost:18080/api/v1/runs/<RUN_ID>/approvals/<STEP_INDEX>" \
  -H "Authorization: Bearer ha-admin-token" -H "Content-Type: application/json" \
  -d '{"decision":"approve"}'
# expect 204

for i in $(seq 1 10); do
  date
  curl -fsS "localhost:18080/api/v1/runs/<RUN_ID>" -H "Authorization: Bearer ha-admin-token"
  echo
  sleep 3
done
# note the wall-clock time `after` step starts / run reaches Succeeded,
# relative to the approve POST above (agent approval-poll latency)
```

Verify final state — run `Succeeded`, `after-gate` present in the log, and
the approval row settled `Approved`:

```bash
curl -fsS "localhost:18080/api/v1/runs/<RUN_ID>/logs?after=0" -H "Authorization: Bearer ha-admin-token" \
  | grep -o '"line":"after-gate"'

curl -fsS "localhost:18080/api/v1/runs/<RUN_ID>/approvals" -H "Authorization: Bearer ha-admin-token"
# expect status:"Approved", decidedBy set
```

## Part B — timeout race across a restart (no approve)

Re-trigger a fresh run of the same job and record `T0` (trigger time),
`stepIndex`, and `timeoutAt`:

```bash
date   # T0
curl -fsS -X POST localhost:18080/api/v1/runs \
  -H "Authorization: Bearer ha-admin-token" -H "Content-Type: application/json" \
  -d '{"jobName":"edge-approval"}'
# capture .id as RUN_ID2

curl -fsS "localhost:18080/api/v1/runs/<RUN_ID2>/approvals" -H "Authorization: Bearer ha-admin-token"
# record .[0].stepIndex and .[0].timeoutAt (should read ~T0 + 10min)
```

Let the gate sit — **do not approve**. Poll in a bounded loop (visible
progress, no long idle sleep) until it is close to T+8min:

```bash
for i in $(seq 1 16); do
  date
  curl -fsS "localhost:18080/api/v1/runs/<RUN_ID2>/approvals" -H "Authorization: Bearer ha-admin-token"
  echo
  sleep 30
done
```

At approximately T+8min (2 minutes before the recorded `timeoutAt`), kill
all three controllers hard:

```bash
date   # should read ~T0+8min
../edgecase/tools/inject.sh kill-hard controller1
../edgecase/tools/inject.sh kill-hard controller2
../edgecase/tools/inject.sh kill-hard controller3
```

Keep them down *past* the recorded `timeoutAt` (i.e. past T+10min) — poll
`/readyz` in a bounded loop to confirm the outage is still live and to make
the ~3min hold visible, without idle-sleeping the whole window in one call:

```bash
for i in $(seq 1 20); do
  date; curl -s -o /dev/null -w '%{http_code}\n' localhost:18080/readyz
  sleep 10
done
# expect 502/503 (or connection error) throughout; this loop alone covers
# ~200s — pad with a second identical loop if T+11min hasn't been reached yet
```

Restart at approximately T+11min:

```bash
date   # should read ~T0+11min
docker compose -f docker-compose.ha.yaml up -d controller1 controller2 controller3
```

Capture container logs for both controllers and agents into the scratchpad
for offline diffing of the two clocks (do not commit these):

```bash
mkdir -p /path/to/scratchpad/w1/w1-3
docker compose -f docker-compose.ha.yaml logs controller1 controller2 controller3 \
  > /path/to/scratchpad/w1/w1-3/controllers.log
docker compose -f docker-compose.ha.yaml logs agent1 agent2 \
  > /path/to/scratchpad/w1/w1-3/agents.log
```

From the captured logs, find:

- **Agent clock:** grep `agents.log` for `"approval timed out"` (the
  `WaitForApproval` local-deadline log line, `internal/agent/approval.go:65`)
  and its timestamp — this fires independent of controller reachability, so
  expect it around T0+10min *even if that falls inside the outage window*.
- **Controller clock:** grep `controllers.log` for
  `"approval reaper: marked timed-out approvals"` (`approval_reaper.go:56`)
  and its timestamp — this can only fire after a controller has restarted
  and the reaper's next ~1/min tick lands, so expect it at-or-after the
  T+11min restart, not at T+10min.

```bash
grep -n "approval timed out" /path/to/scratchpad/w1/w1-3/agents.log
grep -n "approval reaper: marked timed-out approvals" /path/to/scratchpad/w1/w1-3/controllers.log
```

Poll until both the approval row and the run settle to a terminal state,
then record the final values of both:

```bash
for i in $(seq 1 12); do
  date
  curl -fsS "localhost:18080/api/v1/runs/<RUN_ID2>/approvals" -H "Authorization: Bearer ha-admin-token"
  echo
  curl -fsS "localhost:18080/api/v1/runs/<RUN_ID2>" -H "Authorization: Bearer ha-admin-token"
  echo
  sleep 15
done
```

Compare: approval row final `status` vs run final `status`. Per I7 these
must agree (`TimedOut` row + `Failed` run is the expected pairing; anything
else — e.g. row still `Pending`, or row `Approved`/`TimedOut` while the run
disagrees — is the C11-class contradiction this part hunts). Also confirm
via `GET .../logs` that `after-gate` never appears (the run must not have
resumed past the gate).

## Recording

FINDINGS entries (severity guidance):

- Approval row lost across the restart in Part A (row missing, or
  `timeoutAt`/`stepIndex` changed from the pre-outage values) = **major**
  (I1).
- Row/run status disagreement at the end of Part B (e.g. row `Pending`
  forever, or row status contradicting the run's terminal status) =
  **major** (I7).
- Double-timeout or conflicting writes (both clocks fire and leave
  inconsistent state, e.g. a decidedBy/decidedAt that doesn't match either
  expected actor) = record precisely with timestamps from both logs; severity
  per the actual inconsistency observed (at least major, given I7).
- Clean survival + approve in Part A (same `timeoutAt`, run resumes and
  reaches `Succeeded` with `after-gate` in the log, bounded approve-to-resume
  latency) = **observation**.
- Clean agreement in Part B (agent's local deadline logs `approval timed
  out` around T+10min even mid-outage, reaper's row-level `TimedOut` lands
  only after restart, run's final status is `Failed`, row's final status is
  `TimedOut`, no `after-gate` in the log) = **observation** — this is the
  two-independent-clocks design working as intended, not a violation, since
  nothing documents the row and the run settling in lockstep; the invariant
  is that they must not *disagree* once both are settled, not that they
  settle simultaneously.

## Teardown

```bash
docker compose -f docker-compose.ha.yaml down -v
```

Verify nothing is left running:

```bash
docker compose -f docker-compose.ha.yaml ps -a
```
