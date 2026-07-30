# W2-5 — a `call:` child whose parent link is never written, and the orphan it leaves behind

- **Invariants** (corrected after the 2026-07-29 run — the original draft of
  this header cited **I3** for the consequence limb, which is wrong: campaign I3
  is *no lock leaks* (`docs/superpowers/specs/2026-07-29-edge-case-testing-design.md:50`)
  and no mutex, semaphore or named-lock slot is involved in either workload here):
  - **I1 (run accounting)** is cited as the **closest fit only**, not as the
    limb that fails. The orphan does reach exactly one terminal state, so I1's
    literal text is not contradicted; what I1 is stretched to cover is that the
    run reaches it with no accounting edge to the parent. **The violation must
    rest on published documentation, not on I1** — see §6.
  - **I6 (zombie containment — *measure, don't judge*, per the design spec's own
    caveat at `:53`)** is the consequence limb: a child that survives its
    parent's cancellation keeps executing real side effects — here appends to
    `/data/child.log` — after the operator has been told the work is cancelled.
    Record the measured lifetime and side-effect count; do **not** score it
    pass/fail.
  - **I7 (state display consistency)** covers the reads: the child's
    `GET /api/v1/runs/{id}` carries no `calledBy` and the parent's run detail
    carries no call step at all, both of which contradict the reality that the
    parent created that child.
- **Stack:** `test/ha` + `oneway.override.yaml` (the `/data` bind mount both
  fixtures write to, plus the shared nginx IP blocklist) +
  `steplink.override.yaml` (this scenario's overlay: `nginx-steplink.conf`,
  which gives each agent's step-report endpoint its own runtime-writable
  include directory). Every compose call is:

  ```bash
  cd test/ha
  export COMPOSE_FILES="-f docker-compose.ha.yaml -f ../edgecase/compose/oneway.override.yaml -f ../edgecase/compose/steplink.override.yaml"
  docker compose $COMPOSE_FILES up -d --build
  ```

- **Workloads:** `call-parent.payload.json` (`edge-call-parent`: a 20 s
  `prelude` writing `/data/parent.log`, then a `call:` step invoking
  `edge-call-child`) and `call-child.payload.json` (`edge-call-child`: ~90 s
  emitting timestamped markers to `/data/child.log`).
- **Instrumentation:** psql sampling; `docker compose logs nginx` for the HTTP
  status of individual agent requests (nginx logs access lines to stdout, so a
  403 on the link report and a 200 on the late self-heal attempt are both
  visible with timestamps); the agent's own log for
  `permanent error, giving up retry`.

Throughout, `psql` means:

```bash
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "<sql>"
```

## Verified mechanism (read before running; do not re-derive)

### (1) The window: two HTTP round trips, two transactions, one discarded error

`ExecuteCallStep` (`internal/agent/callstep.go:33-107`) does this, in order:

| # | Line | Call | Failure handling |
|---|---|---|---|
| 1 | `orchestrator.go:427` | `ReportStep{Status:"Running"}` for the call step (no `ChildRunID` — it does not exist yet) | `_ =`, discarded |
| 2 | `callstep.go:50` | `CreateChildRun` → `POST /api/v1/agents/{id}/runs/{runId}/children` | error fails the step, `childRunID` stays `""` |
| 3 | `callstep.go:62-66` | `ReportStep{ChildRunID: childRun.ID, CallJobName: ...}` → `POST /api/v1/agents/{id}/steps` | **`_ = client.ReportStep(...)` — discarded, and `Client.ReportStep` (`client.go:207-210`) is a bare `c.do`, so there is no retry at all: one attempt, any error, link gone** |

Steps 2 and 3 are separate HTTP requests to separate controller replicas'
separate transactions. **The child run therefore exists before anything
records that it is a child**, and the record is a single fire-and-forget
request whose failure is invisible to the agent, the controller, and the
operator.

`handleAgentCreateChildRun` (`internal/controller/api_agent.go:780-806`) uses
`parentRunID` only for the ownership guard (`:783`) and then calls
`createRunFromJob(ctx, jobName, params, "agent:"+agentID)` (`:800`) — the
parent run id is **not** passed and **not** persisted. The child's only trace
of its origin is `runs.triggered_by = 'agent:agent1'`, which names the *agent*,
not the run.

### (2) The one persisted edge, and both directions that read it

`step_reports.child_run_id` is the whole edge. Both traversals read it:

- **parent → children:** `ListChildRunIDs` (`internal/store/postgres.go:311-328`)
  = `SELECT child_run_id::text FROM step_reports WHERE run_id = $1 AND child_run_id IS NOT NULL`.
  This is what `cancelDescendantRuns` walks.
- **child → parent:** `GetRunParent` (`postgres.go:859-878`) =
  `SELECT sr.run_id, r.job_name, sr.step_name FROM step_reports sr JOIN runs r ON r.id = sr.run_id WHERE sr.child_run_id = $1`,
  called from `handleGetRun` (`internal/controller/api_runs.go:183`) to fill
  `api.Run.CalledBy`, which the WebUI renders as the “Called by …” link
  (`web/src/routes/RunDetail.svelte:1148`).

`UpsertStepReport` uses `COALESCE(EXCLUDED.child_run_id, step_reports.child_run_id)`
(`postgres.go:803`) so a link once written is never erased — **the failure mode
is never-written, not overwritten.**

**There is no `runs.parent_run_id` column** (`internal/store/migrations/001_init.up.sql:213-226`,
and no migration adds one — `002`…`017` contain no `ALTER TABLE runs ADD …
parent`), and `api.Run` has no `ParentRunID` field: `types.go:65` is
`api.CalledBy.ParentRunID`, a *nested, read-time-derived* value. Settling this
is one of the scenario's deliverables (see Part B step B6) — the plan's “Facts
NOT established” entry names `api.Run.ParentRunID` and is a misattribution.

### (3) The blast radius: 4 direct call sites, 7 entry points, all blind

`cancelDescendantRuns` (`api_runs.go:390-412`) is BFS over `ListChildRunIDs`
with a `visited` set. Direct call sites (verified by grep at W2-2 execution —
**not** the five the plan originally listed):

| Direct call site | Trigger |
|---|---|
| `internal/controller/api_runs.go:382` | public cancel endpoint (`POST /api/v1/runs/{id}/cancel`) |
| `internal/controller/api_agent.go:626` | the agent's own “parent run finished Failed/Cancelled” report |
| `internal/controller/queuedrun_reaper.go:77` | queued-run reaper |
| `internal/controller/stuckrun_reaper.go:88` | inside `failOrphanedRun` |

Expanding `failOrphanedRun`'s four callers (`stuckrun_reaper.go:58`,
heartbeat reconcile `api_agent.go:114`, claim-build failure `api_agent.go:188`,
startup reconcile `api_agent.go:824`) gives **seven entry points**. Every one
of them reaches the child only through `ListChildRunIDs`, so an unlinked child
is invisible to all seven — plus to the WebUI navigation in both directions.

### (4) Why the in-source “self-heals” comment does not save it

`callstep.go:56-61` says: *“The terminal report re-sends the link, so a report
lost here self-heals; failure to send is non-fatal.”* The terminal report does
carry `ChildRunID` (`orchestrator.go:653-668`) and does go through
`retryUntilSuccess`. It nevertheless cannot heal the case this scenario
produces, for three independent reasons:

1. **It only exists if the agent survives to send it.** The step's terminal
   report is emitted after the poll loop returns; an agent killed inside the
   call step never sends one.
2. **`retryUntilSuccess` abandons on any `< 500`** (`internal/agent/retry.go:33-37`)
   — the established W1-5 major. A 403/404/409 on that request is permanent,
   not delayed.
3. **Even a delivered terminal report is discarded once the parent run is
   terminal.** `handleAgentStepReport` re-reads the run and, for
   `Succeeded/Failed/Cancelled`, returns **200 with `{"alreadyFinalized":true}`
   before touching `step_reports`** (`api_agent.go:506-521`). So in exactly the
   situation the link matters — the parent has been cancelled and the cascade
   has already run — the “self-heal” is answered `200 OK` and writes nothing.
   The agent cannot tell the difference.

### (5) Injection: a URI-scoped 403, and why not a timing race

The brief's first choice is `inject.sh nginx-block agent1` timed to land
between calls 2 and 3. **That window is 1-5 ms wide** (two consecutive HTTP
round trips on a container network), which is the same wall W2-3's Arm D1 (a
1 ms window, 0 hits in 10 attempts) and W2-4's Part B (a 4 ms window,
unhittable) both hit. Container-level injection cannot resolve it, and ten
failed attempts would produce no evidence.

So this scenario keeps the *mechanism* of the brief's preferred injection —
**a fast nginx 403, which rides the W1-5 4xx-abandonment major and makes the
loss permanent rather than merely delayed, which is the realistic production
path** — and removes the timing race by scoping the deny **by URI instead of
by instant**:

```
location = /api/v1/agents/agent1/steps { include /etc/nginx/steplock/agent1/*.conf; ... }
```

(`test/edgecase/compose/nginx-steplink.conf`, armed with
`inject.sh steplock agent1`). `POST .../agents/agent1/steps` → 403;
`POST .../agents/agent1/runs/{runId}/children` → unaffected, because an
nginx exact-match location wins over the prefix location and the child-create
URI is a different path. Call 2 succeeds and call 3 is refused, deterministically,
with no timing precision required at all. **Precisely: the substitution is
deterministic *per URI*, but its *activation instant* is not** — the arm takes
effect via `nginx -s reload`, and the 2026-07-29 run contains a counter-example
where an already-connected agent's step report still succeeded inside a
nominally armed window (see the execution notes below). Always probe-confirm the
armed state from the agent's own traffic, not only from a host-side `curl`.

**Be honest about the two costs of this substitution.**

- It refuses *every* step report from that agent while armed, not only call 3.
  The arm therefore also loses the `prelude` step's terminal report and the
  call step's own `Running` report (call 1). Part B records exactly which
  reports were lost, from the agent's `permanent error, giving up retry`
  lines, and keeps the window short (armed mid-prelude, cleared as soon as the
  child run exists) so that everything after the child's creation is
  uninjected.
- A 403 from a reverse proxy is a *stand-in* for the real production losses of
  that one request: a WAF/LB rule, a controller restart mid-request, a
  connection reset, a request timeout, or the agent process dying in the
  window. All of them reach the same state because call 3 has no retry and its
  error is discarded. What the injection does **not** claim to reproduce is a
  specific observed production incident.

### (6) What the docs promise (search these before filing a violation)

**RESOLVED by the 2026-07-29 run — use these three verdicts, do not re-derive
them, and note that the obvious-looking citation is the wrong one:**

- **`docs/jobs.md:671-673` is the PRIMARY citation and it holds without
  qualification.** "Once the child run exists, the call step's status badge …
  **remains accurate if the parent agent stops before it can submit a final step
  report**." The child exists, the final report never lands, and there is no
  step-1 row at all — so there is no badge to remain accurate.
- **`docs/jobs.md:678` is NOT the contract, and citing it as one is a scoping
  error.** Its "the child run is cancelled" clause lives inside the
  `timeoutMinutes` sentence (`:675-678`), and this scenario's workload
  (`test/edgecase/workloads/call-parent.payload.json`) sets **no**
  `timeoutMinutes`. The sentence that governs the unset case (`:679-680`)
  promises only that the *wait* ends, not that the child is cancelled. Quote
  both and disclose the scoping; cite `:678` as evidence of *intent*.
  `docs/high-availability.md:415-416` is likewise intent-corroboration only —
  it is scoped to the startup/heartbeat reconcile endpoint, not to the human
  cancel `POST /api/v1/runs/{id}/cancel` this scenario drives.
- **`docs/jobs.md:707-708` says the *opposite* and must be adjudicated inside
  the entry, not filed as a separate passing remark.** "Cancelling the parent
  releases its slot, after which the child completes" describes exactly the
  buggy outcome. Resolve it in place: the code cascades (`api_runs.go:382` under
  the `:378-381` comment), a `Queued` descendant is cancelled just as readily as
  a `Running` one, and both controls measured the cascade at 3.6-3.8 ms — so
  `:707` is the stale sentence.

- `docs/troubleshooting.md` — grep for `call:`/child/descendant/cascade before
  filing; record what is and is not said about a child outliving its parent.
- `docs/high-availability.md` — the reaper contracts; the descendant cascade is
  described as the mechanism that stops children, so a child that survives it
  is a contradiction of that description if the docs state it.
- `docs/configuration.md` — whether anything documents the link's durability.
- **`callstep.go:56-61` is an unexported function's own comment and is NOT a
  documented contract** under this campaign's rule (`FINDINGS.md:476-514`). It
  establishes intent — which is what makes “the link is never written” a defect
  rather than a design choice — but the *violation* has to rest on an invariant
  or on published documentation.

## Baseline gate

Confirm all of these before recording anything. If any fails, STOP and report
BLOCKED with the evidence.

```bash
SCRATCH=<scratchpad>/w2-5 ; mkdir -p "$SCRATCH"
cd test/ha
export COMPOSE_FILES="-f docker-compose.ha.yaml -f ../edgecase/compose/oneway.override.yaml -f ../edgecase/compose/steplink.override.yaml"
docker compose $COMPOSE_FILES up -d --build

curl -s -o /dev/null -w 'readyz=%{http_code}\n' localhost:18080/readyz
docker compose $COMPOSE_FILES ps --format '{{.Service}} {{.State}}'
curl -fsS localhost:18080/api/v1/agents -H "Authorization: Bearer ha-admin-token" | tee "$SCRATCH/agents.json"

for f in call-parent call-child; do
  curl -fsS -X POST localhost:18080/api/v1/jobs \
    -H "Authorization: Bearer ha-admin-token" -H 'Content-Type: application/json' \
    --data-binary @../edgecase/workloads/$f.payload.json -o /dev/null -w "$f=%{http_code}\n"
done                                                            # expect 200 x2
```

Four gates specific to this scenario:

```bash
# G1. The overlay's nginx.conf actually replaced oneway's. Compose merges
#     `volumes` entries by target path; verify rather than assume.
docker compose $COMPOSE_FILES config | grep -A3 'nginx.conf'
docker compose $COMPOSE_FILES exec -T nginx grep -c steplock /etc/nginx/nginx.conf   # expect 3
# On Git Bash prefix the whole command with MSYS_NO_PATHCONV=1, or the
# container path is rewritten to C:/Program Files/Git/etc/nginx/nginx.conf.

# G2. Unarmed, the step-report endpoint is reachable (a bare POST with no body
#     must NOT be 403 — 400/401 is fine, it proves the request reached a
#     controller rather than being denied at nginx).
curl -s -o /dev/null -w 'unarmed steps=%{http_code}\n' -X POST localhost:18080/api/v1/agents/agent1/steps

# G3. Armed, it is 403 — and the child-create path on the SAME agent is not.
sh ../edgecase/tools/inject.sh steplock agent1
curl -s -o /dev/null -w 'armed steps=%{http_code}\n'    -X POST localhost:18080/api/v1/agents/agent1/steps
curl -s -o /dev/null -w 'armed children=%{http_code}\n' -X POST localhost:18080/api/v1/agents/agent1/runs/00000000-0000-0000-0000-000000000000/children
sh ../edgecase/tools/inject.sh steplock-clear
curl -s -o /dev/null -w 'cleared steps=%{http_code}\n'  -X POST localhost:18080/api/v1/agents/agent1/steps
```

Expect `armed steps=403` and `armed children` ≠ 403 (401/404 both prove the
request was proxied). **If `armed steps` is not 403 the overlay did not take and
the whole scenario is invalid — STOP.**

```bash
# G4. Host <-> DB clock skew, since the parent's terminal timestamp is a DB
#     column and the child's markers are host-side `date -u` inside a container.
psql "SELECT NOW();" ; date -u +%FT%T.%3NZ
docker compose $COMPOSE_FILES exec -T agent1 date -u +%FT%T.%3NZ

# G5. Clean slate for the observable side effect.
rm -f ../edgecase/sideeffect-data/child.log ../edgecase/sideeffect-data/parent.log
```

## Part A — control: the link is written, and the cascade works

Runs **first**, uninjected. Without it, “the child was not cancelled” is
ambiguous between a missing link and a cascade that never worked.

```bash
# A1. Trigger the parent and note which agent claims it.
curl -fsS -X POST localhost:18080/api/v1/runs -H "Authorization: Bearer ha-admin-token" \
  -H 'Content-Type: application/json' -d '{"jobName":"edge-call-parent"}' | tee "$SCRATCH/armA-trigger.json"
PARENT_A=<id>
psql "SELECT id, status, claimed_by, claimed_at, created_at FROM runs WHERE id='$PARENT_A';"

# A2. Wait out the 20s prelude, then confirm the link row exists.
psql "SELECT run_id, step_index, step_name, status, child_run_id FROM step_reports WHERE run_id='$PARENT_A' ORDER BY step_index;" \
  | tee "$SCRATCH/armA-link.txt"
CHILD_A=<child_run_id>
curl -fsS "localhost:18080/api/v1/runs/$CHILD_A" -H "Authorization: Bearer ha-admin-token" \
  | tee "$SCRATCH/armA-child-get.json"        # expect a populated "calledBy"

# A3. Cancel the parent and time the cascade.
date -u +%FT%T.%3NZ
curl -s -o /dev/null -w 'cancel=%{http_code}\n' -X POST "localhost:18080/api/v1/runs/$PARENT_A/cancel" \
  -H "Authorization: Bearer ha-admin-token"
psql "SELECT id, job_name, status, updated_at FROM runs WHERE id IN ('$PARENT_A','$CHILD_A');" \
  | tee "$SCRATCH/armA-cascade.txt"
```

**Record:** the `child_run_id` value; `calledBy` present in the child's API
read; both runs' `updated_at` (the cascade latency, DB clock); and the last
`/data/child.log` marker, to show the child's side effects stop.

## Part B — the injected arm: the link is never written and the child outlives the cancel

```bash
# B1. Trigger the parent; capture created_at and the claiming agent.
curl -fsS -X POST localhost:18080/api/v1/runs -H "Authorization: Bearer ha-admin-token" \
  -H 'Content-Type: application/json' -d '{"jobName":"edge-call-parent"}' | tee "$SCRATCH/armB-trigger.json"
PARENT=<id>
psql "SELECT NOW(), id, status, claimed_by, claimed_at FROM runs WHERE id='$PARENT';" | tee "$SCRATCH/armB-claim.txt"
AG=<claimed_by>          # arm the steplock for THIS agent, whichever it is

# B2. Arm mid-prelude (~10s in, ~10s before the call step). Record the instant.
date -u +%FT%T.%3NZ | tee "$SCRATCH/armB-arm.txt"
sh ../edgecase/tools/inject.sh steplock "$AG" | tee -a "$SCRATCH/armB-arm.txt"

# B3. Poll at 0.2s for the child run to appear (created via the UNBLOCKED
#     children endpoint), then clear the steplock immediately so nothing after
#     the child's creation is injected.
#
#     The predicate MUST be `created_at > <this parent's created_at>`, not
#     "the newest edge-call-child". A bare ORDER BY created_at DESC LIMIT 1
#     matches a PREVIOUS trial's child and clears the steplock seconds before
#     this trial's call step even starts, producing a normally-linked child and
#     a void trial. That cost one trial on the 2026-07-29 run; the corrected
#     driver is `test/edgecase/tools/w2/w2-5-partB-inject2.sh`.
PCREATED=$(psql "SELECT created_at FROM runs WHERE id='$PARENT';")
docker compose $COMPOSE_FILES exec -T postgres sh -c "
  for i in \$(seq 1 300); do
    psql -U unified -tAc \"SELECT NOW(), id, status, claimed_by, triggered_by, created_at FROM runs WHERE job_name='edge-call-child' AND created_at > '$PCREATED' ORDER BY created_at DESC LIMIT 1;\"
    sleep 0.2
  done" | tee "$SCRATCH/armB-childwait.txt"
CHILD=<id>
date -u +%FT%T.%3NZ | tee "$SCRATCH/armB-clear.txt"
sh ../edgecase/tools/inject.sh steplock-clear | tee -a "$SCRATCH/armB-clear.txt"
```

```bash
# B4. Confirm the unlinked state — the scenario's precondition.
psql "SELECT run_id, step_index, step_name, status, child_run_id FROM step_reports WHERE run_id='$PARENT' ORDER BY step_index;" \
  | tee "$SCRATCH/armB-steprows.txt"
psql "SELECT child_run_id FROM step_reports WHERE run_id='$PARENT' AND child_run_id IS NOT NULL;" \
  | tee "$SCRATCH/armB-nolink.txt"          # expect ZERO rows == ListChildRunIDs' exact query
psql "SELECT id, job_name, status, triggered_by, created_at FROM runs ORDER BY created_at;" \
  | tee "$SCRATCH/armB-runs.txt"
curl -fsS "localhost:18080/api/v1/runs?jobName=edge-call-child" -H "Authorization: Bearer ha-admin-token" \
  | tee "$SCRATCH/armB-childlist.json"      # the child IS listed: it exists, it is just unattributed
curl -fsS "localhost:18080/api/v1/runs/$CHILD" -H "Authorization: Bearer ha-admin-token" \
  | tee "$SCRATCH/armB-child-get.json"      # expect NO "calledBy"

# B5. Which reports were actually lost (be precise; do not infer).
docker compose $COMPOSE_FILES logs "$AG" | grep -n "permanent error, giving up retry\|call: child run created" \
  | tee "$SCRATCH/armB-agentlog.txt"
docker compose $COMPOSE_FILES logs nginx | grep "agents/$AG/steps" | tee "$SCRATCH/armB-nginx-steps.txt"
```

```bash
# B6. Settle the ParentRunID question definitively, on the live schema and row.
psql "SELECT column_name FROM information_schema.columns WHERE table_name='runs' ORDER BY ordinal_position;" \
  | tee "$SCRATCH/armB-runs-columns.txt"
psql "SELECT count(*) FROM information_schema.columns WHERE table_name='runs' AND column_name LIKE '%parent%';"
```

Report: whether the column exists at all; what `api.Run`'s JSON actually
carries for a child (`calledBy`, nested) in **both** the Part A control and this
arm; and therefore whether the field is *ever* populated and from where.

```bash
# B7. Cascade-cancel the parent through the public endpoint (call site
#     api_runs.go:382) and record the parent's terminal timestamp.
curl -s -o /dev/null -w 'cancel=%{http_code}\n' -X POST "localhost:18080/api/v1/runs/$PARENT/cancel" \
  -H "Authorization: Bearer ha-admin-token" | tee "$SCRATCH/armB-cancel.txt"
psql "SELECT NOW(), id, job_name, status, updated_at FROM runs WHERE id IN ('$PARENT','$CHILD');" \
  | tee -a "$SCRATCH/armB-cancel.txt"
docker compose $COMPOSE_FILES logs controller1 controller2 controller3 | grep -i "cascade cancel" \
  | tee "$SCRATCH/armB-cascade-log.txt"     # expect nothing: there is nothing to walk

# B8. The consequence. Sample the child until terminal; the child log is the
#     side effect that must be shown continuing past the parent's terminal ts.
#     Poll by `$CHILD`'s id — same trap as B3, and here the id is already known,
#     so there is no excuse for "the newest edge-call-child".
docker compose $COMPOSE_FILES exec -T postgres sh -c "
  for i in \$(seq 1 400); do
    psql -U unified -tAc \"SELECT NOW(), id, status, claimed_by, updated_at FROM runs WHERE id='$CHILD';\"
    sleep 0.5
  done" | tee "$SCRATCH/armB-childpoll.txt"
tail -20 ../edgecase/sideeffect-data/child.log | tee "$SCRATCH/armB-childlog-tail.txt"
psql "SELECT id, status, created_at, updated_at FROM runs WHERE id='$CHILD';" | tee "$SCRATCH/armB-childfinal.txt"
```

```bash
# B9. The late self-heal attempt. When the agent notices the parent is
#     Cancelled it terminates the call step and sends the terminal report WITH
#     ChildRunID through an unblocked nginx. Capture its status code and prove
#     the link is still absent afterwards (mechanism note 4, reason 3).
docker compose $COMPOSE_FILES logs nginx | grep "agents/$AG/steps" | tee "$SCRATCH/armB-selfheal-nginx.txt"
psql "SELECT run_id, step_index, step_name, status, child_run_id FROM step_reports WHERE run_id='$PARENT' ORDER BY step_index;" \
  | tee "$SCRATCH/armB-steprows-after.txt"
docker compose $COMPOSE_FILES logs "$AG" | tail -40 | tee "$SCRATCH/armB-agentlog-tail.txt"
```

**Numbers to compute, all from captures:**

- `t_child_created` (`runs.created_at` of the child) and `t_arm` / `t_clear`,
  to show the child was created inside the armed window and everything after
  was not.
- `t_parent_terminal` = the parent's `updated_at` at cancel.
- `t_child_terminal` = the child's `updated_at`, and
  `orphan_lifetime = t_child_terminal − t_parent_terminal`.
- The `/data/child.log` marker timestamps that fall **after**
  `t_parent_terminal` (count and last value) — the side effect, on the agent's
  clock, corrected for the G4 skew.
- The HTTP status nginx recorded for the late terminal step report.

## Recording (severity guidance)

- **An orphan child that survives its parent's cancellation and runs to
  completion = major**: the run reached a terminal state with no edge to
  the only parent it ever had, so it is unreachable from every one of the seven
  `cancelDescendantRuns` entry points and from both directions of the WebUI
  navigation. **Rest the entry on the published `docs/jobs.md:671-673` promise
  (see §6), with I1 cited as closest fit only** — the orphan does reach exactly
  one terminal state, so I1's literal text is not what is contradicted. Do not
  rest a major on I1 alone here.
- **I6 is the second limb** if `/data/child.log` grows past the parent's
  terminal timestamp: side effects continued after the operator was told the
  work was cancelled. Quantify with marker timestamps, not adjectives — and per
  I6's *measure, don't judge* caveat, report the lifetime and marker count
  rather than scoring it. **Note the marker log is second-granular**, so derived
  offsets from it carry ±1 s and must not be quoted to three decimals; only
  DB-clock-to-DB-clock figures (e.g. child `Succeeded` minus parent terminal)
  are exact. **Not I3** — I3 is *no lock leaks* and no lock is involved here.
- **The `alreadyFinalized` 200 on the late self-heal report is a separate
  finding** — an observation at minimum (the agent is told its report
  succeeded when it was dropped), and it is what makes the
  “a report lost here self-heals” comment false in exactly the case that
  matters. If the injected arm shows it live, record the status code and the
  step-row state.
- **If the link turns out to be written transactionally after all** — i.e. the
  child cannot exist without its link row — that is a finding in the opposite
  direction: record it, correct the Verified-facts block in
  `docs/superpowers/plans/2026-07-30-edge-case-campaign-w2.md:68`, and drop the
  major.
- Do **not** re-file the two already-recorded neighbours: the dangling
  `Running` step row under a terminal run (`FINDINGS.md:587`, `:330`) and the
  4xx abandonment in `retryUntilSuccess` (W1-5). Cross-reference them; this
  entry's payload is the *missing edge* and the orphan it produces.
- Every uncaptured live observation must carry
  `(observed live, raw output not captured to scratchpad)`.

## Teardown

```bash
sh ../edgecase/tools/inject.sh steplock-clear || true
sh ../edgecase/tools/inject.sh nginx-unblock  || true   # no service argument
docker compose $COMPOSE_FILES down -v
docker compose $COMPOSE_FILES ps -a
rm -f ../edgecase/sideeffect-data/child.log ../edgecase/sideeffect-data/parent.log
```

## Execution notes (added after the 2026-07-29 run — read before re-running)

- **Outcome: the major reproduced on the first valid trial, and the axis was
  the right one.** Parent `b556c457` / child `a833b977`; the child ran all 90
  iterations and reached `Succeeded` **38.840 s** after the parent's terminal
  timestamp. Full numbers in `FINDINGS.md:885`.
- **The URI-scoped injection works exactly as designed and needs no timing.**
  Armed, `POST /api/v1/agents/agent2/steps` → 403 while
  `POST /api/v1/agents/agent1/steps` → 401 and
  `POST /api/v1/agents/agent2/runs/{id}/children` → 401 (proxied, not denied).
  Three 403s landed on the blocked path at `23:44:16`, and the child was
  created at `23:44:16.589078` in the middle of them.
- **Trap that cost one trial: “the newest `edge-call-child` run” is not the
  child you are waiting for.** v1 of the poll script matched a *previous*
  arm's child and cleared the steplock ~7 s before the call step even started,
  so that trial produced a normally-linked child. Poll for
  `created_at > <parent.created_at>`; the fixed script is
  `w2-5/partB-inject2.sh`. The void trial was not wasted — it became the
  **second control** (parent on `agent2`, child on `agent1`, cascade 3.628 ms,
  `w2-5/armA2-control2.txt`), which is worth keeping deliberately since the
  first control had the agents the other way round. **But it is not an
  *uninjected* control** — see the next note.
- **`steplock` arms via `nginx -s reload`, and a host-side probe does NOT prove
  the arm is in force for an already-connected agent.** This run contains a
  direct counter-example: the lock was re-armed for `agent2` at `23:42:44.459Z`
  (`w2-5/partB-rearm.txt`) and not cleared until `23:44:48.865Z`
  (`w2-5/partB-b4-unlinked.txt`), yet `POST /api/v1/agents/agent2/steps`
  **succeeded** inside that window twice — `200 87` at `23:43:19`
  (`w2-5/nginx-full.log:451`) and `204` at `23:43:56` (`:498`). The captured
  nginx exec PIDs are `43, 51, 59, 67, 75,
  91, 105, 113, 121` — stride 8 everywhere, so **`83` is missing** and one nginx
  exec in that interval went uncaptured. The plausible mechanism is that a
  reload leaves the agent's established keepalive upstream connection served by
  an old worker with the old config; either way, **treat "armed" as unproven
  until you see a denial on traffic originating from the agent's own IP**
  (`172.20.0.x`, `Go-http-client/1.1`), not just from a `curl` on
  `172.20.0.1`.
  - **Consequences for how the controls may be described:** control 1
    (`09b5013b`, `23:40:52.085023` → `23:41:27.415940`) is fully clean — the
    lock was cleared at `23:40:26` and not re-armed until `23:42:27` — and it
    alone is sufficient to establish that the cascade works when the link
    exists. Control 2 (`6eebe7ba`) must be described as *"the injection did not
    cover its link-report window"* — its link WAS written at `23:42:35` while
    clear (`nginx-full.log:335,336,338`, three `204`s) — and **not** as
    uninjected.
  - **Part B is unaffected and its window is independently verified from
    inside:** the arm at `23:44:08.510227` was probe-confirmed `403` at
    `23:44:09.321` and is bracketed by three logged agent-originated denials at
    `23:44:16` (`nginx-full.log:539-545`); the clear is likewise confirmed by
    the post-cancel `200 87` at `23:45:11` (`:672`) on the same path.
- **A tight `docker compose exec … psql` poll loop inside a `tee`d pipeline
  died silently mid-run** (the tool call returned with the loop's output
  truncated after the arm step). The steplock was still armed and the child had
  been created, so the arm was recoverable by hand — but **do not depend on a
  long-lived poll loop to also perform the injection teardown.** Split the
  “detect” and “clear” steps, or re-check the armed state before recording. The
  actual clear happened at `23:44:49.8`, ~33 s after the child appeared rather
  than the ~1 s the runbook intends; harmless here (everything after the
  child's creation was uninjected either way) but it must be reported as the
  measured window, not the intended one.
- **Git Bash rewrites container paths.** `docker compose exec -T nginx grep -c
  steplock /etc/nginx/nginx.conf` becomes
  `C:/Program Files/Git/etc/nginx/nginx.conf`. Export `MSYS_NO_PATHCONV=1` for
  the whole scenario.
- **Compose merged the two nginx.conf mounts by target path, so
  `steplink.override.yaml` cleanly replaced `oneway.override.yaml`'s bind** —
  verified in `docker compose config` (one `target: /etc/nginx/nginx.conf`,
  source `nginx-steplink.conf`). Gate G1 is still worth keeping: it is one
  command and the whole scenario is void if the merge ever behaves differently.
- **`nginx-steplink.conf` must be extended by hand for a third agent.** The
  locations are exact-match per agent id (`agent1`, `agent2`), which is what
  makes the scoping airtight; a regex location would need the blocklist include
  to be per-agent some other way.
- **nginx access logs are the right instrument for “was this one request
  accepted?”** Second precision only, but they distinguish `204` (step report
  written), `200 87` (`alreadyFinalized`, dropped) and `403` (denied), which no
  other surface does — the agent logs nothing for a 2xx and the controller logs
  nothing for either.
- **Budget ~12 minutes of wall time** for a full pass on a warm stack: baseline
  gate ~1 min, Part A control ~1.5 min, Part B ~2.5 min (20 s prelude + 90 s
  child), plus a spare trial. The `up -d --build` from cold dominates
  everything else.
