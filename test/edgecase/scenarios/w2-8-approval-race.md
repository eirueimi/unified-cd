# W2-8 — approval decision racing the timeout boundary

- **Invariants** (quoted verbatim from
  `docs/superpowers/specs/2026-07-29-edge-case-testing-design.md:48-54`):
  - **I7 (state display consistency)** — "run status, approval status, and audit
    rows never contradict each other or reality" (`:54`). This is the primary
    invariant and the fit is unusually literal: the three nouns I7 names are
    exactly the three artifacts this scenario puts into contradiction —
    `runs.status = 'Failed'`, `run_approvals.status = 'Approved'` with a named
    human in `decided_by`, and an `audit_logs` row
    `action=run.approval.decide … status=204` against that same run id. No
    interpretive stretch is needed; state the three values from one capture and
    the contradiction is on its face.
  - **I1 (run accounting)** — "every API-accepted run reaches exactly one
    terminal state; no phantom runs from duplicate fires/webhooks" (`:48`).
    **Almost certainly NOT violated here, and must not be claimed.** The run
    reaches exactly one terminal state (`Failed`) and stays there; the approval
    decision does not resume it, does not create a second run, and does not
    re-open the run's status. Check this rather than assume it (Part A gate A4
    re-reads `runs.status` after the decision), and if it holds, say so
    explicitly as a null result. W2-7 had to argue an I1 fit carefully because
    its terminal state was *false*; here the run's terminal state is *correct* —
    the gate really did fail — so I1 is the wrong home and I7 is the right one.
  - **I5 (bounded recovery)** — "after fault injection the system returns to
    steady state within documented bounds (leader re-election ≤ seconds;
    stuck-run reap ≤ staleAfter 90s + interval 30s; the bounds in
    `docs/high-availability.md` are the contract)" (`:52`). **In scope only for
    one narrow number**: `docs/jobs.md:1740-1744` promises the approval reaper
    reconciles an expired `Pending` row "within roughly one minute". Measure the
    `timeout_at` → `TimedOut` latency and compare. But note this scenario injects
    **no fault** — the window is produced by ordinary API use — so if the latency
    is inside the bound, that is an I5 null result and not evidence of anything.
    Report it as a measured number against the `docs/jobs.md` sentence, not as
    an I5 pass/fail.
  - **NOT I2, NOT I3, NOT I4** for Parts A-E. No side-effect log, no mutex, no log
    integrity claim. W2-5 was corrected for relabelling I3 and W2-7 for stretching
    a zombie limb; do not invent either here.
  - **I6 (zombie containment) — CORRECTED after Part F. The original text of this
    bullet said "NOT I6 … nothing keeps executing after the terminal write (the
    approval step's 'work' is a poll loop that has already returned)", and that is
    FALSE as a general statement.** It is true only when the decision lands after
    the agent has exited (the timeout path) or after it has already detected the
    cancel (Part D, +9.19 s) — which was every trial the first execution ran. When
    the decision commits **inside** the agent's cancel-detection fence
    (`CancelPollInterval = 5 s`, `internal/agent/orchestrator.go:37`),
    `WaitForApproval` returns **`true`** and the **post-gate step body executes** on
    a `Cancelled` run — 4/4 in Part F. So I6 **is** engaged, as the
    measured-and-documented (explicitly not pass/fail) invariant it is; and
    `docs/jobs.md:1775-1777`'s "an in-flight step is interrupted" is contradicted.
    **I2 is still NOT violated even then** — the step executes exactly *once*, so
    the defect is zero-vs-once, not once-vs-twice, and I2's text is "at most once".
    **I1 is still NOT violated** — one terminal state, no phantom run. If a later
    author is tempted to file the Part F shape as I2, re-read I2's wording first.
    Part A gate A5 and Part F's step-2 checks are what make these claims measured
    rather than assumed.
- **Stack:** plain `test/ha`, **no overlay**. Nothing here needs a shared volume,
  a second agent id, or nginx surgery. Every compose call is:

  ```bash
  cd test/ha
  export COMPOSE_FILES="-f docker-compose.ha.yaml"
  export MSYS_NO_PATHCONV=1          # Git Bash rewrites container paths (W2-5)
  docker compose $COMPOSE_FILES up -d --build
  ```

  Throughout, `psql` means:

  ```bash
  docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "<sql>"
  ```

  and `API` means `curl -sS -H "Authorization: Bearer ha-admin-token"` against
  `http://localhost:18080`.

- **Both agents stay up.** Unlike W2-7, attribution is free here: the agent's
  `"approval timed out"` line carries `runID=<id>` (`internal/agent/approval.go:64-67`),
  so `docker compose logs agent1 agent2 | grep <runID>` attributes unambiguously
  and two agents means two 30 s gates can be in flight at once. Record which
  agent claimed each run anyway (`runs.claimed_by`), because the clock-skew
  measurement in Part B is **per-agent-container** and mixing two containers'
  clocks into one number would be wrong.
- **Workload:** `approval-short.payload.json` (`edge-approval-short`: `before` →
  `gate` with `timeoutMinutes: 0.5` → `after`). Applied with
  `POST /api/v1/jobs`; triggered with `POST /api/v1/runs` body
  `{"jobName":"edge-approval-short"}`. `approval.payload.json`
  (`edge-approval`, 10-minute gate) is the **control** workload for Part E.

## Verified mechanism (read before running; do not re-derive)

### (1) Why this needs no race at all

The plan's Task 9 premise and W1-3's carry-forward lead agree, and the code
confirms it: the vulnerable state **arises by itself and persists**.

| Step | Code | Effect |
|---|---|---|
| agent creates the row | `internal/agent/approval.go:33-38` → `api_approvals.go:69-93` → `CreatePendingApproval` (`postgres.go:2430-2437`) | `run_approvals` row `status='Pending'`, `timeout_at = controller_now + 30 s` |
| agent's local deadline | `approval.go:48` (`deadline := time.Now().Add(...)`), polled every `ApprovalPollInterval = 3 s` (`agent.go:27`) | on expiry logs `"approval timed out"` (`:64`) and returns **false** |
| step + run fail | `orchestrator.go:362-367` reports the step `Failed`, `recordFailure()` | run reaches `Failed` |
| the row is **not** touched | `approval.go:17-20` — "On timeout the agent has no decision endpoint (decisions are human-only), so the controller-side `run_approvals` row stays `Pending`" | row still `Pending`, `decided_by` NULL |
| the only healer | `RunApprovalReaper` (`approval_reaper.go:22-39`), **1-minute** tick (`cmd/controller/main.go:403`), advisory lock `0x61707276` | marks it `TimedOut`/`system` on its next tick after `timeout_at` |

So between the run's terminal write and the reaper's next tick there is a window
— **0 to ~60 s wide, expected mean ~30 s** — in which the row is `Pending` and
the run is `Failed`. Nothing in `DecideApproval` (`postgres.go:2439-2450`) or
`handleDecideApproval` (`api_approvals.go:13-54`) consults the run:

```sql
UPDATE run_approvals
SET status = $3, decided_by = $4, comment = $5, decided_at = now()
WHERE run_id = $1 AND step_index = $2 AND status = 'Pending';
```

No join to `runs`, no run-status check, **no `timeout_at` check**. The handler
never calls `GetRun`. `changed == true` ⇒ `204 No Content`.

**Consequence for the runbook: Part A is not a race and must not be written as
one.** Wait for the natural state, verify it with a read, then fire one POST.
Only Part C is a race, and it races the *reaper*, not the timeout.

### (2) The two clocks, and what a naive measurement would get wrong

Two independent `time.Now()` calls in two different containers:

- **controller clock** — `timeout_at = time.Now().Add(TimeoutMinutes × time.Minute)`
  at `api_approvals.go:86-89`, evaluated in whichever controller replica served
  the agent's `CreateApproval` POST.
- **agent clock** — `deadline = time.Now().Add(time.Duration(timeoutMin*60) * time.Second)`
  at `approval.go:48`, evaluated in the agent container.

The plan lists "direction and magnitude of clock skew between the agent's
`WaitForApproval` deadline and the controller's `timeout_at`" as **unresolved**
(`plans/2026-07-30-edge-case-campaign-w2.md:92`). Settling it needs three
quantities kept apart, because the observable difference is a **sum**, not the
skew:

```
(agent log ts of "approval timed out")  −  timeout_at
   =  clock_skew(agent → controller)                    ← the unknown
    + code_gap                                          ← CreateApproval RTT + ReportStep RTT, :33-38 → :48
    + poll_granularity                                  ← 0 … ApprovalPollInterval (3 s), see below
```

`poll_granularity` is not zero-mean and is worth reading off the loop
(`approval.go:50-72`): the deadline check is *after* the `GetApproval` call and
*before* the ticker wait, so expiry is detected on the first tick at or after
`deadline`, i.e. the log line lands in `[deadline, deadline + 3 s)`. It can
therefore only push the difference **up**, never down.

**So measure the skew directly and use the log line as a cross-check, not as the
primary instrument.** Direct measurement: read `date -u +%s.%N` inside the agent
container and `SELECT NOW()` (and `date -u` in the controller container) as close
together as the harness allows, repeatedly, and report the offset with its own
sampling error. Docker containers on one host share the kernel clock, so the
honest expected answer is "no skew beyond sampling noise" — **which is a real
answer to the open question and must be reported as such**, together with the
sign of the *observable* difference, which the code path guarantees is positive
(agent deadline later) regardless of skew.

### (3) The reaper race (Part C) is a two-writer CAS on one row

Both writers are single autocommit statements guarded on the same predicate:

| Writer | Statement | Guard |
|---|---|---|
| human decision | `DecideApproval`, `postgres.go:2439-2450` | `run_id=$1 AND step_index=$2 AND status='Pending'` |
| reaper | `MarkExpiredApprovalsTimedOut`, `postgres.go:2473-2484` | `status='Pending' AND timeout_at IS NOT NULL AND timeout_at < now()` |

Note the reaper's guard has a third conjunct, `timeout_at IS NOT NULL`, which the
plan's summary at `:81` elides. Harmless here (the row always has one) but quote
the statement, not the summary.

Exactly one can match a row. Postgres row-level locking makes the outcome
well-defined but not predictable from outside: the loser's `UPDATE` matches 0
rows, and for `DecideApproval` that means `changed == false`, which
`handleDecideApproval:44-52` turns into **409 "already decided"** (because
`GetApproval` still finds the row — the 404 branch needs the row to be absent
entirely). So the observable per-attempt outcome is a clean binary:

- **human wins** → `204`, `status='Approved'`, `decided_by=<actor>`
- **reaper wins** → `409`, `status='TimedOut'`, `decided_by='system'`

and the reaper's win is independently visible in the controller log:
`"approval reaper: marked timed-out approvals" count=N` (`approval_reaper.go:56`,
guarded by `if n > 0`, so it fires only on a tick that actually reaped).

**Phase-locking is mandatory (W2-4's lesson, `plan:59`).** The reaper's sweep
grid must be measured, not guessed. Its `UPDATE` runs on **every** winning tick
regardless of whether any row matches, so `log_statement='all'` sees the grid
even on an idle stack — this is the one instrument that works here, and it is
better than the stuck-run reaper's because no work needs to exist to see it.
Budget per `plan:47`: **query load at `interval / N`** (expect 2-3 `UPDATE`s per
minute clustered within ~10 ms on this 3-replica rig), **worst-case latency at
the nominal 60 s**.

### (4) What the docs actually say — read this before filing a contract violation

W2-7's cited contract turned out to *sanction* the behaviour it was cited
against. The same trap is set here, so quote and scope carefully.

`docs/jobs.md:1740-1744` (Approval Step → "Constraints and v1 limitations"):

> "When the step times out, the agent fails the step itself, so the run is
> correctly marked as Failed. The approval audit row in `run_approvals` is
> reconciled separately: a leader-elected controller reaper marks any expired
> `Pending` row as `TimedOut` (with `decidedBy` = `system`) within roughly one
> minute. The reaper only fixes the audit row — it never changes run status."

Read it three ways and record which one you rely on:

1. **As a prohibition on post-timeout decisions — it is NOT one.** Nothing in
   this passage, in `docs/jobs.md:1712-1725` ("How to approve or reject" /
   "Behavior"), or in `docs/authorization.md:13` says the decision endpoint stops
   accepting decisions once `timeout_at` has passed or once the run is terminal.
   On this limb the docs are **silent, not contradicted**, and the finding must
   rest on I7.
2. **As a statement of the intended reconciled end state — it is that.** The
   passage tells an operator that a timed-out gate ends up `TimedOut`/`system`.
   The observed end state is `Approved`/`<human>`. But note the escape: the
   reaper's promise is scoped to "any expired **`Pending`** row", and once the
   human's `UPDATE` commits the row is no longer `Pending` — so the *reaper*
   behaves exactly as documented and citing it as a broken promise would repeat
   W2-7's error. Use this limb as **context for severity**, not as the violation.
3. **As designating `run_approvals` an audit record — it does, twice, and this is
   the limb that matters.** `docs/jobs.md:1740` calls it "The approval audit
   row"; `:1722` says "The identity of the decider is recorded (`decidedBy`) in
   the audit record"; `docs/audit.md:4-5` calls `run_approvals` "the existing
   per-run approval audit trail". So the product's own documentation classifies
   the falsified row as an audit artifact, which is what makes I7's "audit rows
   never contradict … reality" bite on it directly rather than by analogy.

Also grep before filing, per house rule:
`grep -rn -i "timeout_at\|after the timeout\|expired approval" docs/` and
`grep -rn -i "approval" docs/audit.md docs/operations.md`.

### (5) Operator surfacing — code-read, then confirm live

Two surfaces, and they disagree about whether the decision is even offerable:

- **Web UI (`web/src/routes/RunDetail.svelte:1230-1273`, `:1278+`)** — the
  Approve/Reject buttons render **only** `{#if s.status === 'WaitingApproval'}`,
  so the UI does **not** offer the post-timeout decision. But the `{:else if
  approval?.decidedBy}` branch renders `Decided by <strong>{decidedBy}</strong>`
  under a step whose badge reads `Failed`, with **no indication** the decision
  arrived after the failure. So the UI cannot *cause* this state but displays it
  as though it were ordinary.
- **CLI (`internal/cli/approvals.go:31-68`)** — `unified-cli approve <run-id>
  <step-index>` performs **no** run-state or approval-state lookup: it POSTs and,
  on any status < 400, prints `approved step <n> of run <id>`. **This is the
  honest trigger to demonstrate** — it needs no crafted curl and no knowledge
  that the run has failed, which is what makes the scenario an ordinary-use
  finding rather than an API-abuse one. Record that the CLI reports success.

Record the mitigation honestly alongside the finding: a UI-only operator cannot
reach this state, so the realistic actor is the CLI, the API, or an automation
holding a `dev`-or-better token (`server.go:383` gates the route with `dev`;
`docs/authorization.md:13` grants approve/reject to dev and above).

## BASELINE GATE — do not proceed past a failing check

Write every gate output to `$SCRATCH/gate.txt`.

```bash
SCRATCH="<scratchpad>/w2-8" ; mkdir -p "$SCRATCH"
```

- **G0 — worktree.** `git rev-parse --show-toplevel` is `.../wt-edge-spec` and
  the branch is `plan/edge-case-w2`. The `test/ha` project name is
  `unified-cd-ha` (`docker-compose.ha.yaml:1`), distinct from the developer
  stack's `unified-cd`, so the two do not collide — but confirm with
  `docker compose ls` that the dev stack is untouched.
- **G1 — stack health.** All three controllers `healthy`; `API /readyz` → 200;
  `GET /api/v1/agents` lists **agent1 and agent2**, both with
  `capabilities` including `native` (W2-4: they advertise
  `["native","container"]`). Capture the full agent list — a scenario that
  assumes a capability without reading this has been wrong twice already.
- **G2 — Postgres statement logging armed, and *verified in a fresh session*.**
  **One `ALTER SYSTEM` per `psql -c`** — two in one `-c` is an implicit
  transaction, Postgres refuses it, and `pg_reload_conf()` still returns `t`, so
  the broken form is indistinguishable from success (established by W2-7,
  `plan:80`).

  ```bash
  docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c "ALTER SYSTEM SET log_statement='all';"
  docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c "ALTER SYSTEM SET log_line_prefix='%m [%p] h=%h ';"
  docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c "SELECT pg_reload_conf();"
  # fresh session — this is the check that matters
  docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "SHOW log_statement;"   # must print: all
  docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "SHOW log_line_prefix;"
  ```

  Record both `SHOW` outputs in the gate. **Revert at teardown and say so in the
  findings** (W2-6 shipped a runbook whose revert could not have worked).
- **G3 — the reaper is actually ticking.** With G2 armed, wait ~150 s and grep
  the Postgres log for the reaper's own statement:

  ```bash
  docker compose $COMPOSE_FILES logs --no-log-prefix postgres --since 3m \
    | grep -n "UPDATE run_approvals" > "$SCRATCH/gate-reaper-grid.txt"
  ```

  Expect clusters of 2-3 statements ~60 s apart. **Zero occurrences is a gate
  failure, not a finding** — it means the instrument or the job is not what this
  runbook assumes, and per `plan:50` a silent job has three possible causes of
  which only one is a defect. Derive the grid's `(epoch mod 60)` phase and record
  it; Part C aims at it.
- **G4 — clock census.** Sample, in one command, `date -u +%s.%N` in agent1,
  agent2, controller1 and postgres containers plus the host, three times a few
  seconds apart. This is the direct instrument for §(2). Record the spread and
  the sampling cost (the `exec` round-trip is the error bar).
- **G5 — job applied and the timeout is really 30 s.** `POST /api/v1/jobs` with
  `approval-short.payload.json` → 200. Then trigger one **throwaway** run, read
  `GET /api/v1/runs/{id}/approvals` and confirm `timeoutAt − createdAt ≈ 30 s`
  from the API body. This checks `plan:90`'s fractional-`timeoutMinutes`
  resolution at the point of use rather than trusting it (the W1 carry-forward
  lesson at `FINDINGS.md:498`). Let this run die naturally; it also warms the
  images so Part A's timings are not first-run timings.
- **G6 — audit log readable.** `API /api/v1/audit?limit=5` → 200 and a JSON
  array. If this 500s (the API has been intermittently 500ing on this rig),
  retry and record how many attempts it took; Part A's strongest evidence
  depends on it.

## Part A — the natural window (no race)

**Deliverable:** a single psql read showing `runs.status='Failed'` and
`run_approvals.status='Approved'` with a named human in `decided_by`, plus the
matching `audit_logs` row at `status=204`.

- **A1.** Trigger `edge-approval-short`; record `runID`, the trigger response
  time (host clock), and `runs.claimed_by`.
- **A2.** Poll `GET /api/v1/runs/{id}/approvals` every 1 s into
  `$SCRATCH/partA-approvals-poll.txt` and `GET /api/v1/runs/{id}` every 1 s into
  `$SCRATCH/partA-run-poll.txt`. These two continuous samplers are what pin the
  window's edges; a point read after the fact cannot. Also capture
  `timeout_at` from the first sample.
- **A3.** Wait for `runs.status = 'Failed'`. Record:
  - `t_timeout_at` (controller clock, from the DB),
  - `t_run_failed` = `runs.updated_at` (DB clock),
  - `t_agent_log` = the agent's `"approval timed out"` line
    (`docker compose logs --no-log-prefix agent1 agent2 | grep <runID>`;
    container clock — save the full grep to `$SCRATCH/partA-agent-log.txt`).
  - **Confirm `run_approvals.status` is still `Pending` at this moment.** This is
    the load-bearing observation of the whole scenario and it must come from a
    sampler line, not from a read taken later.
- **A4.** Fire the decision **once**, from two channels in two separate trials so
  both are on record:
  - trial A-api: `POST /api/v1/runs/{id}/approvals/1` body
    `{"decision":"approve","comment":"w2-8"}` — capture status code and body
    (`-w '%{http_code} %{time_total}'`);
  - trial A-cli: the same via `unified-cli approve <run-id> 1` inside a container
    that has the CLI (the `agent-enroll` service builds from `dev.Dockerfile` and
    mounts the repo, so `go run ./cmd/unified-cli` works there) — capture the
    exact stdout. Expected: it prints success.

  Then **one** psql read capturing both rows together:

  ```sql
  SELECT 'RUN'  AS kind, r.id, r.status, r.updated_at::text, NULL, NULL, NULL
    FROM runs r WHERE r.id = '<runID>'
  UNION ALL
  SELECT 'APPR', a.run_id, a.status, a.decided_at::text, a.decided_by,
         a.timeout_at::text, a.created_at::text
    FROM run_approvals a WHERE a.run_id = '<runID>';
  ```

  → `$SCRATCH/partA-contradiction.txt`. One artifact, both rows, one DB clock.
- **A5 — the null checks that keep the classification honest.** After the
  decision: re-read `runs.status` (expect still `Failed` — I1 intact); read
  `step_reports` for the run and confirm the `after` step never ran and the
  `gate` step is still `Failed`; confirm no new run exists with
  `triggered_by` referencing this one. Capture to
  `$SCRATCH/partA-noresume.txt`. If any of these is false the finding is much
  larger and the classification changes — check, do not assume.
- **A6 — the audit row.** `API /api/v1/audit?limit=20` filtered to this run id →
  `$SCRATCH/partA-audit.txt`. Expect
  `action=run.approval.decide`, `resource=<runID>`, `status=204`,
  `actor=<the token's principal name>`. Written synchronously
  (`audit.go:222-227`), so no polling. Also confirm from the same read that the
  route **is** audited (W2-7 established agent routes are not; `server.go:383`
  sits under the `auditLogMiddleware` mounted at `:357` — verify live, since this
  row is the entry's strongest evidence).
- **A7 — the reaper's non-intervention.** After the decision, wait for the next
  two sweeps and show the row stays `Approved` (the reaper's guard no longer
  matches) and that no `"approval reaper: marked timed-out approvals"` line
  mentions it. Also record the `timeout_at → next sweep` latency for the §(4)
  limb-2 comparison against "roughly one minute".
- **A8 — the display.** Fetch the run detail page's two API reads and record what
  the UI would render per `RunDetail.svelte:1268-1273`: step badge `Failed`,
  caption `Decided by <actor>`. Note explicitly whether **anything** anywhere
  flags the contradiction (expected: nothing).

## Part B — the two clocks

**Deliverable:** a signed, magnitude-bounded answer to `plan:92`, decomposed per
§(2).

- **B1.** From G4's census plus a repeat census taken during Part A, compute
  `offset(agent_container → postgres)` and `offset(agent → controller)` with the
  `exec` round-trip as the error bar. Save raw to `$SCRATCH/partB-clocks.txt`.
- **B2.** From Part A's three timestamps compute the **observable** difference
  `t_agent_log − t_timeout_at` and decompose it: subtract the measured
  `code_gap` (available from the Postgres log — the `INSERT INTO run_approvals`
  statement timestamp is when the controller evaluated `timeout_at`, and the
  following `INSERT INTO step_reports` for the same run is the `ReportStep` at
  `approval.go:39-46`, so the gap between them bounds the RTT term) and report
  the residual against `poll_granularity ∈ [0, 3 s)`.
- **B3.** Repeat over **≥3 runs** so the poll-granularity term is visibly
  variable and the skew term visibly constant. Report direction and magnitude,
  and state plainly if the answer is "skew is below the measurement floor" —
  that resolves the open question as well as a non-zero number would, provided
  the floor is stated.

## Part C — racing the reaper (a distribution, not a result)

**Deliverable:** a per-attempt outcome table over **≥6 phase-locked attempts**,
with the winner attributed by three independent signals (HTTP status, row
contents, controller log line).

- **C1 — measure the grid.** From the Postgres log, extract every
  `UPDATE run_approvals` statement with its `%m` timestamp into
  `$SCRATCH/partC-grid.txt`; compute cluster instants, within-cluster spread,
  between-cluster period, and the stable `(epoch mod 60)` phase. Do this
  **immediately before** the attempts and re-check after — W2-4 measured ~13 ms
  of drift per tick on the 30 s grid, so a 60 s grid over ten minutes may drift
  visibly.
- **C2 — aim.** For a target sweep instant `S`, the row must be `Pending` and
  expired at `S`, so `timeout_at ∈ (S − 60 s, S)`; and to make the race tight,
  fire the POST at `S`. `timeout_at ≈ trigger + (claim latency) + 30 s`, so
  trigger at `S − 30 s − ε` for a small `ε` (a few seconds) and schedule the
  POST for `S` with a busy-wait on the host clock (sleep to `S − 0.5 s`, then
  spin). Record the *intended* and *actual* POST instants for every attempt —
  an attempt whose POST missed the cluster by more than a few tens of ms is a
  **missed aim**, and must be reported separately from a lost race.
- **C3 — sweep the offset.** Do not fire every attempt at exactly `S`. Vary the
  intended offset across `{−200, −50, −10, 0, +10, +50, +200} ms` so the
  distribution has an x-axis; the interesting cell is `|offset| < spread` where
  both writers are genuinely concurrent. Each attempt: fresh run, fresh row.
- **C4 — record per attempt:** intended offset, actual POST instant (host
  clock), HTTP status + `time_total`, resulting `status`/`decided_by`/`decided_at`
  from `run_approvals`, the reaper `UPDATE`'s own log timestamp for that cluster,
  and whether a `"marked timed-out approvals"` line appeared with what `count`.
  Tabulate into `$SCRATCH/partC-attempts.txt`.
- **C5 — report as a distribution.** N attempts, N-human-wins, N-reaper-wins,
  N-missed-aim. **State the sample size next to every ratio** and do not
  generalise a 6-attempt split into a probability. If every attempt is won by one
  side, that is a legitimate finding about the ordering, but say it is 6/6 and
  not "always".
- **C6 — the point that survives whatever the split is.** Either outcome leaves
  a defect: a human win writes `Approved` onto a `Failed` run (Part A's finding),
  and a reaper win means the human's 409 "already decided" is the *only* signal
  they get that their approval had no effect — which is itself a misleading
  message, since nobody decided anything and the run had already failed. Record
  the exact 409 body text.

## Part D — is the window bounded by anything else?

Short, cheap, and it forecloses an obvious reviewer question: *is the exposure
really only ~60 s?*

- **D1 — a `Pending` row on a run that failed for a different reason.** The
  reaper's guard needs `timeout_at < now()`. A gate with a **long** timeout
  (`approval.payload.json`, 10 minutes) on a run that is failed early — cancel it
  via `POST /api/v1/runs/{id}/cancel` — leaves a row that is `Pending` and **not
  yet expired**, so the reaper cannot touch it for ten minutes. Fire an approve
  into that state and record the outcome. If it returns 204, the exposure window
  is **`timeoutMinutes`**, not 60 s, and the default is 60 minutes
  (`docs/jobs.md:1731`, `approval.go:22`) — a materially larger finding than
  Part A's, and one that needs no timeout to occur at all.
- **D2** — confirm from the same read whether the *cancelled* run's approval row
  is ever reconciled by anything (grep the reaper's log lines and re-read after
  two sweeps). If nothing ever reconciles a `Pending` row on a terminal run
  before `timeout_at`, say so with the sample window that supports it.

## Part E — controls

A control is only a control if the injection was demonstrably clear for its whole
window (house rule; W2-4/W2-6 both had to state this).

- **E1 — the happy path.** A run approved **while the step is genuinely
  `WaitingApproval`**: expect 204, `Approved`, run continues, `after` step runs,
  run `Succeeded`, audit row `status=204`. This shows the 204 in Part A is not
  simply "this endpoint always 204s" and that the fixture works.
- **E2 — the already-decided path.** A second approve on E1's row: expect
  **409** and an unchanged `decided_at`. This is the response code the buggy path
  *should* have produced, and having it measured on the same rig is what makes
  "204 is wrong" a comparison rather than an assertion.
- **E3 — the absent-row path.** Approve step index `0` (the `before` step, which
  has no approval row): expect **404 "no pending approval"**. Distinguishes the
  three codes so Part A's 204 cannot be misread.
- **E4 — after the reaper has run.** Approve a row that is already `TimedOut`:
  expect 409. Bounds Part A's window on the far side.

## Part F — the decision inside the agent's cancel-detection fence

**Added after the first execution and after review; this is the part that carries
the *execution* half of the defect.** Parts A-E all fired the decision with the
agent already gone or already past its fence, so every one of them could only
produce a false record. Part F fires it **inside** the fence.

**Why it can work at all — read this before aiming.** Two agent-side pollers race,
and `WaitForApproval` consults the wrong one first:

| Poller | Interval | Code |
|---|---|---|
| cancel poller (`client.GetRun`) | **5 s** — `CancelPollInterval` | `internal/agent/orchestrator.go:37`, loop `:123-149`; only its tick sets `cancelledByMaster` and calls `cancelRun()` |
| approval poller (`GetApproval`) | **3 s** — `ApprovalPollInterval` | `internal/agent/agent.go:27`; loop `internal/agent/approval.go:51-73` |

`WaitForApproval` returns **`true`** on an `Approved` read at `approval.go:55-56`
*before* it ever reaches `case <-ctx.Done()` at `:69`. So if **any** 3 s approval
tick falls between the decision's commit and the next 5 s cancel tick, the gate
succeeds. Nothing downstream stops the next step: `orchestrator.go:357-362`
reports the gate `Succeeded`; `RunPipeline` (`internal/agent/pipeline.go:126-157`)
has **no** `ctx.Err()` check between stages; and `EvalCondition`
(`internal/dsl/condition.go:44`) computes `successVal` from `cancelledByMaster`
(`orchestrator.go:93-99`), still **`false`**. The step body then runs.

- **Workload:** `approval.payload.json` (`edge-approval`, `timeoutMinutes: 10`) —
  a long gate so no timeout can interfere. Its `after` step is `echo after-gate`,
  which is what makes execution *provable*: the string lands in the `logs` table.
- **F1 — aim, do not guess.** Trigger, wait for step 1 `WaitingApproval`, then wait
  ≥13 s so **both** grids have laid down samples, then read them out of the
  controller HTTP log for that run. **The two grids separate by path**: the approval
  grid is `GET /api/v1/agents/{id}/runs/{rid}/approvals/1`, the cancel grid is
  `GET /api/v1/runs/{rid}`. Predict one period forward and fire `cancel` then
  `approve` from **one process** so that the approve commits ~0.45 s before the next
  predicted approval tick while the next cancel tick is still ≥3 s away. Firer:
  `w2-8/partF.py`. Measured aim accuracy: predicted-vs-actual approval tick
  **+1.77 ms / −0.61 ms**.
  - **Do not poll `GET /api/v1/runs/{id}` from the host while doing this** — that is
    the cancel poller's exact path and it pollutes the grid you are about to read
    back. Use `GET /api/v1/runs?limit=N` or `/steps` instead.
- **F2 — capture, per attempt** (`w2-8/partF-capture.sh`). The five artifacts that
  matter, in order of load-bearing-ness:
  1. **`SELECT … FROM logs WHERE run_id=… AND step_index=2`** — the post-gate step's
     own stdout. **This is the proof of execution**; nothing else could write it.
  2. **`step_reports`** — expect **no row for step 2** and step 1 stuck
     `WaitingApproval`. The absence is itself a finding (I7).
  3. **`POST /api/v1/agents/*/steps`** in the controller HTTP log. **This path
     carries NO run id**, so it must be matched by time window, not by run — the
     first version of this capture grepped by run id and saw nothing, which reads
     exactly like "the agent never reported". Status **`204` = persisted, `200` =
     `alreadyFinalized` no-op** (`internal/controller/api_agent.go:513-521`).
  4. **The agent container log.** Expect **no** `"received cancellation signal from
     master"` line at all on a successful attempt — the pipeline finished before the
     poller's next tick.
  5. **One-statement ordering read** on the Postgres clock: `runs.updated_at`,
     `run_approvals.decided_at`, and `min(logs.ts) WHERE step_index=2`.
- **F3 — the control is mandatory, and it is what makes the fence a measurement.**
  Repeat with the approve fired **8 s** after the cancel (outside the 5 s fence).
  Expect: `204` and `Approved` (the Part A defect, unchanged) but **zero** step-2
  log lines, and the agent's `"received cancellation signal from master;
  interrupting run"` line present. Firer: `w2-8/partF-control.py`.
- **F4 — one wide-gap attempt.** Repeat F1 with the cancel→approve gap at ~2.5 s to
  show the window is the whole fence and not just the ~120 ms of the first hits.
- **Recording.** If a post-gate step runs, this is a **separate violation entry**,
  not a note on Part A's: primary **I7** (the `logs`/`step_reports` contradiction),
  **I6** as the measured-not-scored zombie limb, a **published-contract** limb at
  `docs/jobs.md:1775-1777`, and **explicitly NOT I2** (exactly one execution) and
  **NOT I1** (one terminal state). It also requires **editing Part A's entry**:
  its "nothing executes" and "NOT I6" claims must be scoped to Part A-E's aims.
  Report the attempt count either way, and separate *aborted-before-firing* attempts
  from *fired-and-missed* ones — they are not the same result.

## Teardown

```bash
# revert the instrument FIRST, and verify in a fresh session
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c "ALTER SYSTEM RESET log_statement;"
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c "ALTER SYSTEM RESET log_line_prefix;"
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c "SELECT pg_reload_conf();"
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "SHOW log_statement;"   # must print: none
docker compose $COMPOSE_FILES down -v
```

- **Kill every background sampler before teardown and say so in the findings.**
  W2-6 left one running. Keep their PIDs in `$SCRATCH/samplers.pid` and `kill`
  them explicitly, then show the process list is clear.
- Copy `$SCRATCH` into the campaign evidence root at the wave checkpoint
  (`test/edgecase/README.md` § "Raw evidence").

## Recording rules

- **Part A ⇒ major, I7**, if it reproduces: the record contradicts reality on all
  three of I7's nouns at once, and one of them is an *audit* artifact, which is
  the one thing that must not lie. Severity argument, stated rather than
  asserted: the falsified row is durable, is attributed to a **named human**, is
  surfaced in the UI as an ordinary decision, and is reachable by a documented
  CLI command that reports success — but for the Parts A-E aims it does **not** cause
  execution (nothing resumes, no side effect fires), which is why it is major and
  not critical. Say both halves — **and scope the negative half explicitly to the
  aims you actually fired.** Parts A-E all fire with the agent gone or already past
  its cancel-detection fence, so "nothing executes" is a property of the aim, not
  of the system; Part F fires inside the fence and the step **does** execute. Do not
  write "nothing executes" unqualified.
- **Part D ⇒ escalates Part A's scope if it returns 204** — same invariant, same
  entry or a sibling entry, but the window becomes `timeoutMinutes` (default 60
  **minutes**) rather than ≤60 s, and no timeout is required. If it 404s or 409s,
  record that as the bound it is.
- **Part C ⇒ observation**, whichever way the split falls: the two-writer CAS is
  as-designed (both statements are correct in isolation) and the risk it reveals
  is the misleading 409. Do not inflate a lost race into a violation.
- **Part B ⇒ measurement**, not a finding, unless the skew is large enough to
  make `timeout_at` and the agent deadline disagree by more than the poll
  interval — in which case it is a new observation about the fixture's
  assumptions.
- Entry titles must say **"observation"** for observation entries
  (`FINDINGS.md:481`). A defect found in this campaign's own assets gets an
  explicit `Classification:` line and sits outside both tallies (`FINDINGS.md:487`).
- Every number cites a `$SCRATCH` filename whose time window covers it. Derived
  figures say "derived"; code-read figures say "code-read"; uncaptured live
  observations say `(observed live, raw output not captured to scratchpad)`.

## Execution notes — 2026-07-30 run (read before re-running)

Executed against `test/ha` at branch `plan/edge-case-w2`, `03:52:16Z – 04:42:19Z`.
Instrument armed at `03:52:45.9` (verified `log_statement=all` in a fresh session)
and reverted at `04:42:08` (verified `none` in a fresh session, `w2-8/teardown.txt`);
stack torn down with `down -v`. **No background sampler was left running** — every
sampler in this scenario is a bounded foreground loop inside `partA.sh` /
`partC2.sh` / the Part D2 command, `jobs` was empty and no stray `psql`/`curl`
process remained at teardown (that claim was prose-only in this session — it was
actually captured in the Part F session, see below). Three FINDINGS entries from
this session: **1 violation (major, I7) and 2 observations (minor)**; no
branch-internal asset bug. **A fourth entry (major) was added by the Part F session
below**, bringing the scenario to 2 violations + 2 observations.

**Predictions that held.** Part A needed no race, exactly as §(1) argued: the
`Failed`/`Pending` state was on a sampler line within 0.66 s of the terminal
write, and one POST returned `204`. The reaper grid was visible from the first
gate check (§G3 never failed). The `409`/`404` disambiguation in §(3) matched E2
and E3 exactly. The audit row was present with no polling.

**Eight corrections for a re-run:**

1. **Part D was the bigger finding, and the runbook under-weighted it.** It is
   written as a short "forecloses a reviewer question" probe; in fact it removes
   the timeout from the story altogether. A 10-minute gate on a **cancelled** run
   sat `Pending` with **9m48s** left on `timeout_at` and accepted `204`. Run Part
   D **before** Part C on a re-run: the exposure window is `timeoutMinutes`
   (default 60 **minutes**), not the ≤60 s the timeout path gives, and no timeout
   need occur.
2. **And Part D's shape is reachable from the Web UI, which the runbook's §(5)
   said it was not.** §(5) is right for the *timed-out* case (the gate step reads
   `Failed`, so no buttons render) and **wrong** for the cancelled case:
   `GET /runs/{id}/steps` still returns step 1 `WaitingApproval` on a `Cancelled`
   run, so `RunDetail.svelte:1246-1266` renders live Approve/Reject buttons.
   Do not repeat the claim that only the CLI/API can reach this.
3. **A shell busy-wait cannot phase-lock anything on Windows.** The first Part C
   driver looped on `date`+`awk` subprocesses and overshot by **~120 ms**, so
   attempts C1–C3 are missed aims, not lost races. Fire from a single process:
   `w2-8/fire.py` sleeps to an absolute epoch and spins the last 15 ms
   (overshoot 0.33–0.94 ms measured). **But the real floor is the request
   pipeline** — host→`UPDATE` latency is **49.9–64.3 ms** with ~14 ms of jitter,
   so aims inside about −55 ms are a coin toss and −100 ms or wider wins
   reliably. Budget that, not the clock.
4. **The Postgres log parser must handle both statement forms.** `DecideApproval`
   is parameterised, so it logs as `LOG: execute stmtcache_<hash>: …`; the
   reaper's arg-less query logs as `LOG: statement: …`. A parser matching only
   `statement:` sees every sweep and **zero** decisions — the first version of
   this scenario's `grid.awk` did exactly that and looked like it was working.
   Use `w2-8/grid2.awk`, which also reads `DETAIL: parameters: $1 = '<run id>'`.
   Comparing the two statements on the Postgres clock is the right instrument for
   the race anyway: it removes every cross-clock correction.
5. **`docker compose logs --since` lags several seconds.** A read taken 3 s after
   a sweep did not contain that sweep, which looked momentarily like a *skipped*
   sweep (it was not — the row was `TimedOut` at the expected instant). Use
   `--since 150s` and wait ≥6 s before reading, and never conclude "the job did
   not run" from a `logs --since` window alone.
6. **Two Windows path traps.** With `MSYS_NO_PATHCONV=1` set (required, W2-5),
   `curl -o /c/Users/...` fails with "No such file or directory" — write bodies
   to stdout instead — and Windows `python` must be handed a `C:/Users/...` path,
   not `/c/Users/...`. Both cost an attempt here.
7. **The clock instruments the runbook proposed are the wrong ones.** §(2)'s
   `docker exec date` census has a **±280 ms** error bar (exec startup dominates)
   and the *controller* image's `date` has no `%N` at all, so the controller clock
   cannot be read directly. What settles it is **row-internal brackets**:
   `(timeout_at − run_approvals.created_at) − 30 s` bounds controller↔Postgres to
   **<54 µs**, and `step_reports[0].started_at − runs.claimed_at` together with
   `run_approvals.created_at − step_reports[0].ended_at` bracket agent↔Postgres to
   `(−3.54, +4.69) ms`. Use those.
8. **§(2)'s `poll_granularity` term is ~0 for this fixture, not `U(0, 3 s)`.**
   30 s is exactly ten `ApprovalPollInterval`s and the ticker starts with the
   deadline, so the last tick lands on the deadline. Every observed difference was
   6.6–19.5 ms. A `timeoutMinutes` whose truncated whole-second value is not a
   multiple of 3 (e.g. `0.52` → 31 s) would add up to ~2 s — so the fixture, not
   the design, is why this scenario's numbers are milliseconds.

**One measurement to read with care.** The `timeout_at → TimedOut` latencies
(19.056–21.744 s, 11 rows) are **not** a sample of the natural distribution: 10 of
the 11 come from Part C, which deliberately placed `timeout_at` ~20 s ahead of a
sweep. The only unaimed sample is the §G5 throwaway run at **21.744 s**. The
structural bound from the measured 60.012 s grid is `[0, 60.02) s`.

**Invariant citation:** the invariant table is at
`docs/superpowers/specs/2026-07-29-edge-case-testing-design.md:48-54` (I7 = `:54`,
I6 = `:53`). An earlier draft of this file cited `:44-55`.

## Execution notes — Part F, 2026-07-30 second session (read with the above)

Executed against a freshly rebuilt `test/ha` on the same branch,
`05:12:47Z – 05:33:36Z`. Instrument armed at `05:12:47` (verified `all` in a fresh
session) and reverted at `05:30:52`, re-verified `none` in a fresh session at both
`05:30:52` and `05:33:07` (`w2-8/partF-gate.txt`, `w2-8/partF-teardown.txt`); stack
torn down with `down -v`. **Sampler hygiene was captured, not asserted this
time** — `jobs` printed nothing and `ps -W | grep -iE "psql|curl|python"` matched
nothing, on **two** passes (before the revert and immediately before `down -v`).
The earlier session's identical claim was prose-only; the structural reason it held
either way is that every sampler in this scenario is a bounded foreground loop
inside a single script.

**Result: the fence was probed and it does not hold.** 7 runs triggered — **4 aimed
inside the fence, 4 hits**; **1 control aimed outside it, 0 execution**; **2 aborted
at the aiming stage before any cancel/approve pair fired** (harness bugs: the
`/steps` API returns `index`, not `stepIndex`; and a 9 s observation window caught
only one cancel-poll sample, since widened to 13.5 s). Filed as a **new violation
entry (major, I7 primary + I6 measured + a `docs/jobs.md:1775-1777` contract limb)**,
which brings this scenario to **4 entries: 2 violations (both major) and 2
observations (both minor)**.

**Three things a re-run should know.**

1. **`POST /api/v1/agents/{id}/steps` — the `ReportStep` path — carries no run id**
   (`internal/agent/client.go:208`). Grepping the controller log by run id shows
   `logs/bulk` and `finish` but **zero** step reports, which reads exactly like "the
   agent never reported the step". Match it by time window instead, and read the
   status code: **`204` = persisted, `200` = `{"alreadyFinalized":true}` no-op**
   (`internal/controller/api_agent.go:513-521`).
2. **Do not poll `GET /api/v1/runs/{id}` from the host during Part F.** That is the
   agent cancel poller's exact path; host polling pollutes the very grid the aimer
   reads back. Use `GET /api/v1/runs?limit=N` for run status and `/steps` for steps.
3. **The `logs` table has no terminal-run guard, and that is the only reason the
   execution is visible.** Step reports under a terminal run are dropped with `200`,
   but `POST …/steps/{i}/logs/bulk` returned **`204`** and the `after-gate` line
   persisted. Any future scenario asking "did work happen after the terminal
   write?" should key on `logs`, not `step_reports` — the step table is silent by
   design.
