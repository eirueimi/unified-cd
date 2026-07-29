# W2-5 — a `call:` child whose parent link is never written, and the orphan it leaves behind

- **Invariants:**
  - **I1 (run accounting)** is the pass/fail limb: every run must be reachable
    from the system's own bookkeeping. A `call:` child's *only* persisted edge
    to its parent is one column in one row; if that row is never written the
    child is unreachable in both directions and no cascade, reaper, or UI can
    attribute it to anything.
  - **I3 (no orphaned side effects)** is the consequence limb: a child that
    survives its parent's cancellation keeps executing real side effects — here
    appends to `/data/child.log` — after the operator has been told the work is
    cancelled.
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
   before touching `step_reports`** (`api_agent.go:506-520`). So in exactly the
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
with no timing precision required at all.

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
docker compose $COMPOSE_FILES exec -T nginx grep -c 'steplock' /etc/nginx/nginx.conf   # expect 4

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
docker compose $COMPOSE_FILES exec -T postgres sh -c '
  for i in $(seq 1 300); do
    psql -U unified -tAc "SELECT NOW(), id, status, claimed_by, triggered_by, created_at FROM runs WHERE job_name='"'"'edge-call-child'"'"' ORDER BY created_at DESC LIMIT 1;"
    sleep 0.2
  done' | tee "$SCRATCH/armB-childwait.txt"
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
docker compose $COMPOSE_FILES exec -T postgres sh -c '
  for i in $(seq 1 400); do
    psql -U unified -tAc "SELECT NOW(), id, status, claimed_by, updated_at FROM runs WHERE job_name='"'"'edge-call-child'"'"' ORDER BY created_at DESC LIMIT 1;"
    sleep 0.5
  done' | tee "$SCRATCH/armB-childpoll.txt"
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
  completion = major (I1)**: the run reached a terminal state with no edge to
  the only parent it ever had, so it is unreachable from every one of the seven
  `cancelDescendantRuns` entry points and from both directions of the WebUI
  navigation. Cite the invariant *and* whatever `docs/` says about the cascade;
  if the docs are silent, say so and rest the entry on I1.
- **I3 is the second limb** if `/data/child.log` grows past the parent's
  terminal timestamp: side effects continued after the operator was told the
  work was cancelled. Quantify with marker timestamps, not adjectives.
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

## Execution notes

(Added after the run — see the section appended at the bottom of this file.)
