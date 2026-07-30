# W3-4 — bulk log append: partial commit + lost ack ⇒ permanent duplication

> **CORRECTED AFTER EXECUTION — READ THIS BEFORE ANYTHING ELSE.**
> **The hypothesis held in full and the I4 attribution survived review of the
> measurement; nothing in the Invariants block below is withdrawn.** What
> execution changed is four *procedural* claims, and one of them makes a step as
> written impossible. Superseded text is kept in place and marked, per house
> style, because the reason it was wrong is the deliverable.
>
> 1. **Part D1 as written cannot work.** It says to attach SSE to the LB before
>    triggering. `worker_shutdown_timeout 1s` — added deliberately in
>    `nginx-logfault.conf` to answer W2-5's reload lesson — **also severs every
>    in-flight connection through nginx on each reload**, including the SSE
>    stream and the agents' long-poll claims. The first live capture died after
>    exactly one event. **Attach SSE directly to a controller instead**
>    (`docker compose exec -T nginx curl -sSN … http://controller1:8080/…`),
>    which is immune to the reloads; it is the same handler and the same
>    LISTEN/NOTIFY path. See Part D, corrected in place.
> 2. **The truncate arm needs no timing luck, which the runbook did not
>    predict.** A 200 ms `proxy_read_timeout` **self-selects by batch size**:
>    a 1-line batch completes in ~5 ms and returns 204, while the 421-line and
>    1579-line batches take 423 ms and 1594 ms and are cut mid-loop every time.
>    Part B's "cap the attempts" discipline was still honoured (4 attempts of a
>    cap of 10, 3 hits), but the one miss was an **operator timing error**
>    — the arm landed after the burst had already been delivered — not a race.
> 3. **W2-5's failure mode did not recur, and the runbook should not be read as
>    having merely got lucky.** Every arm took effect on the agent's very next
>    flush, on an already-established connection. That is attributable to
>    mitigation 1 of §"Why nginx" (`worker_shutdown_timeout`), and it is the
>    same directive that caused correction 1 — the fix and the side effect are
>    the same line of config.
> 4. **An unanticipated blast radius, which is an artifact and not a finding.**
>    Three consecutive upstream timeouts trip `max_fails=1` on all three
>    `test/ha/nginx.conf` upstreams at once, briefly ejecting the entire
>    controller pool and returning `502 no live upstreams` to unrelated agent
>    traffic. Expect it, do not file it, and do not let it contaminate the
>    per-request bracket (it is distinguishable: `ustatus=502 rt≈0.000`).
>
> **The scenario yields ONE major (I4) and ONE observation (minor).** Do not
> split the truncate and lostack arms into two findings — they share the same
> root cause (§"Verified mechanism" facts 1-4).

**Wave W3, Task 1. Runs on today's `test/ha` rig with no infrastructure change** —
the wave's other five scenarios need object storage that Task 3 adds; this one
does not. **One consequence is load-bearing and is stated up front rather than
discovered in Part E: with no object store configured the log archiver never
starts** (`cmd/controller/main.go:399`, `if obj != nil`), so **nothing on this
rig can seal or archive a run's logs**. Every archive claim in this runbook is
therefore **code-read only**, explicitly labelled, and must never be written up
as measured. See Part E.

---

## Step 1 — the open question the plan flagged, settled before anything else

`docs/superpowers/plans/2026-07-30-edge-case-campaign-w3.md` § "Facts NOT
established" asks: *"Whether the `logs.seq` column is a shared sequence (global)
or per-run. Read the migration."* The W3-4 facts block leans on the answer twice
(the `seq > lastSeq` SSE filter, and "duplicates differ only in `seq`").

**Answer: `logs.seq` is ONE GLOBAL SEQUENCE shared by every run in the
installation. It is not per-run, and there is no per-run counter anywhere.**
Four lines of `internal/store/migrations/001_init.up.sql` settle it:

| Line | Text | What it establishes |
|---|---|---|
| `001_init.up.sql:90` | `seq bigint NOT NULL,` | plain column on `public.logs`, no partition/grouping |
| `001_init.up.sql:103-109` | `CREATE SEQUENCE public.logs_seq_seq START WITH 1 INCREMENT BY 1 NO MINVALUE NO MAXVALUE CACHE 1;` | **exactly one** sequence object, installation-wide |
| `001_init.up.sql:320` | `ALTER TABLE ONLY public.logs ALTER COLUMN seq SET DEFAULT nextval('public.logs_seq_seq'::regclass);` | every row of every run draws from that one sequence |
| `001_init.up.sql:384` | `ADD CONSTRAINT logs_pkey PRIMARY KEY (run_id, seq)` | uniqueness is on the **pair**, so `seq` alone is not the identity |

Also `001_init.up.sql:115` (`ALTER SEQUENCE public.logs_seq_seq OWNED BY
public.logs.seq`) and the covering index `logs_run_idx ON public.logs (run_id,
seq)` at `:557`. There is no second `CREATE SEQUENCE` for logs anywhere in
`internal/store/migrations/` (`grep -in seq` over the whole directory returns
only the lines above plus `run_log_archives.max_seq`).

**Three consequences this runbook depends on.**

1. **`seq` is monotonic within a run but not dense** — a run's seqs are strictly
   increasing in commit order, with gaps wherever a concurrent run's line took a
   value. So `TailLogs`'s `WHERE run_id = $1 AND seq > $2` (`postgres.go:942`) is
   sound as a resume cursor, exactly as the facts block assumed, but **a seq
   delta is not a line count** and must never be used as one.
2. **`seq` is assigned at INSERT, so a re-sent duplicate always gets a HIGHER
   `seq` than the original** — which is what makes the duplicate invisible to
   every `seq`-based dedupe in the system, and what makes it appear *after*
   later lines. This is the reordering limb of I4, not merely the duplicate limb.
3. **The discriminator for "same line, sent twice" is `(line, ts)`, not `seq`.**
   `Timestamp` is stamped once when the batch is built (`internal/agent/runner.go:376`,
   `now := time.Now().UTC()` outside the per-line loop) and re-used verbatim on
   every retry of that batch, so the two copies are byte-identical in `line`,
   `step_index`, `stream` and `ts`, and differ **only** in `seq`. Every
   duplication query below groups on `(line, ts)` for that reason.

**This also disposes of a tempting but wrong measurement.** Because the sequence
is global, `max(seq) - min(seq) + 1` for a run over-counts by however many lines
other runs interleaved. Count rows, never seq spans.

---

## Corrections to inherited facts, established BEFORE execution

These are checks made at the point of use, per the W1 carry-forward rule that a
"verified facts" block is a set of claims. Two were wrong.

- **CORRECTION 1 — `sideeffect.payload.json` emits ZERO log lines, not 120.**
  The Task 1 brief and `test/edgecase/README.md`'s table both describe it as the
  chatty workload. Its step is
  `for i in $(seq 1 120); do echo "run,$i,$(date -u +%H:%M:%S)" >> /data/sideeffect.log; sleep 1; done`
  — **the `echo` is redirected to a file**, so the step's stdout is empty and its
  `logs` row count is 0. It is a *side-effect* fixture (I2), not a log fixture.
  `tick.payload.json` (30 lines, 1/s) and `longrun.payload.json` (300 lines, 1/s)
  are the only stdout-producing fixtures, and at 1 line/s neither ever reaches
  `LogPusher`'s 4 KiB `flushBytes` threshold (`runner.go:255`), so both produce
  **~2-line batches** from the 2 s auto-flush (`runner.go:211`) — far too small
  to make a mid-batch truncation observable. **This scenario therefore adds one
  fixture**, `workloads/logburst.payload.json` (§Stack).
- **CORRECTION 2 — the bulk URI is not `POST /api/v1/agents/*/logs/bulk`.**
  The brief's per-URI nginx idiom has to match the real route, which is
  `POST /api/v1/agents/{agentId}/runs/{runId}/steps/{stepIndex}/logs/bulk`
  (`internal/controller/server.go:255`; client side
  `internal/agent/client.go:301`). An exact-match `location =` (the W2-5
  steplink idiom) is impossible here because the URI embeds a run id and a step
  index, so `nginx-logfault.conf` uses a **regex** location instead. Regex
  locations still outrank the prefix `location /`, so the surgical property is
  preserved.
- **CONFIRMED, not corrected — the facts block's guess about `seq` was right**
  ("It appears to be **global, not per-run**"). Step 1 upgrades it from
  inference to migration evidence. Recorded so nobody re-derives it.

---

## Invariants

Quoted verbatim from `docs/superpowers/specs/2026-07-29-edge-case-testing-design.md`.

- **I4 (log/artifact integrity)** — `:51`:

  > "**Log/artifact integrity** — a Succeeded run's log line count matches what
  > the workload emitted; no duplicates, no reordering; archives stay readable"

  **This is the primary and only claimed invariant, and the fit is direct on
  three of its four clauses.** The scenario produces a run that reaches
  `Succeeded` (log-append failures never fail a step — `handleAgentLogBulk`'s
  error is invisible to the orchestrator, and `LogPusher` swallows it into
  `p.pending`, `runner.go:390-392`), whose `logs` row count **exceeds** what the
  workload emitted, which contains **exact duplicates**, and in which those
  duplicates are **reordered** relative to the lines they follow (Step 1
  consequence 2). Check the fit before leaning on it:
  - *"a Succeeded run's"* — the run must actually be `Succeeded`. Gate G8 and
    every part below assert this explicitly; a `Failed` run would put the
    measurement outside I4's stated scope and the entry would have to be
    re-attributed. **This is the clause most likely to break the attribution, so
    it is checked first, not last.**
  - *"log line count matches what the workload emitted"* — the workload emits a
    known, exact number of lines (`logburst` = 2002: `burst-begin`, `burst-1`
    … `burst-2000`, `burst-end`). The comparison is against that constant, not
    against another run.
  - *"no duplicates"* — measured by `GROUP BY line, ts HAVING count(*) > 1`.
  - *"no reordering"* — measured by checking whether the burst indices are
    monotonic in `seq` order.
  - *"archives stay readable"* — **NOT exercised.** No object store on this rig
    (§Part E). Do not claim this clause.
- **NOT I1.** I1 is "every API-accepted run reaches exactly one terminal state;
  no phantom runs from duplicate fires/webhooks" (`:48`). Every run here reaches
  exactly one terminal state, and the word "duplicate" in I1 is scoped to *runs*
  (duplicate fires/webhooks), not to log rows. W2-9 had an I1 attribution
  withdrawn in review for stretching this clause; do not stretch it the other
  way. State that I1 **held**.
- **NOT I2.** I2 is "step side effects execute at most once" (`:49`). Nothing is
  re-executed here: the step body runs exactly once and the duplication is
  entirely in the transport of its output. **This distinction is the whole
  point** — a reviewer who sees "duplicate" and reaches for I2 has mis-read the
  mechanism. `logburst` deliberately writes no side-effect file so there is
  nothing to confuse the two.
- **NOT I5.** I5 is "after fault injection the system returns to steady state
  within documented bounds … the bounds in `docs/high-availability.md` are the
  contract" (`:52`). Two reasons it does not fit. (i) The system **does** return
  to steady state promptly once the fault is cleared — the pending batch drains
  on the next 2 s flush. (ii) `docs/high-availability.md` carries **no** bound on
  log delivery; grep it. The damage is not a slow recovery, it is that the
  recovery itself writes the duplicate. **Recording I5 as an explicit null result
  is part of the deliverable**, not an omission.
- **NOT I6.** No zombie: the agent is never fenced, never partitioned from its
  own execution, and no run is terminalised out from under it.
- **NOT I7.** I7 is "run status, approval status, and audit rows never
  contradict each other or reality" (`:54`). The run status is `Succeeded` and
  the run **did** succeed; `step_reports` is accurate; no audit row is implicated.
  The *log content* is wrong, which is I4's department, not I7's. Note in the
  entry that I7 **held**, because "the run looks fine" is exactly why this is
  hard to notice.
- **Contract limb — expect "silent, not contradicted", and say so.** Pre-execution
  doc survey (re-run it at write-up time and capture it):
  `grep -rn -i "duplicat|idempot|exactly once|at least once|atomic" docs/*.md`
  and `grep -rn -i "log line|log lines|bulk|buffered|LogPusher" docs/*.md`.
  What exists: `docs/troubleshooting.md:889-899` describes the agent's batching
  and retry ("the agent keeps the batches it couldn't send in a bounded in-memory
  queue") and `docs/operations.md:51` describes sealing. **Neither promises
  atomicity of a bulk append, nor de-duplication, nor that a failed bulk request
  committed nothing.** `docs/high-availability.md:91-92` uses the word
  "idempotent" only of the git resolver and OIDC cleanup. So: **the docs are
  silent, not contradicted** — read the surrounding section before filing
  anything else (the W2-7 lesson: a cited passage turned out to sanction the
  behaviour). The finding rests on I4.
  - **One doc passage must be read carefully before it is cited as sanctioning
    the behaviour**, because it is the closest thing to an authorisation:
    `docs/troubleshooting.md:889-899` explains that unsendable batches are queued
    and retried. It authorises **retry**; it says nothing about what the
    controller may already have committed from the attempt that "failed". Retry
    is only safe if the endpoint is idempotent, and §"Verified mechanism" (4)
    shows it is not. Quote both halves in the entry rather than only the
    convenient one.

---

## Verified mechanism — read this before designing anything

Every row re-read at this branch's HEAD; the `file:line` is the claim.

| # | Fact | Site |
|---|---|---|
| 1 | Bulk append is a **plain Go loop with no transaction**: one `AppendLog` call per line | `internal/controller/api_agent.go:719-739`, call at `:725` |
| 2 | Each `AppendLog` is a **standalone autocommitted statement** — `p.pool.QueryRow(...)` on the pool, no `tx` | `internal/store/postgres.go:918-926` |
| 3 | A mid-loop error does `http.Error(...); return` **from inside the loop** — no rollback, no compensation, and the 500 body carries **no** indication of how many lines landed | `internal/controller/api_agent.go:726-729` |
| 4 | **No idempotency of any kind**: `api.LogAppendRequest` is `{RunID, StepIndex, Stream, Timestamp, Line}` — no batch id, no nonce; the INSERT has no `ON CONFLICT` and there is no unique constraint on any content column (`logs_pkey` is `(run_id, seq)`, and `seq` is server-assigned) | `internal/api` request type; `postgres.go:919-923`; `001_init.up.sql:384` |
| 5 | `AppendLog` uses **`r.Context()`**, so a client disconnect (nginx giving up on the upstream) cancels the very next line's insert and aborts the loop with the prefix committed | `api_agent.go:725` |
| 6 | The agent **retries the whole batch** on any error: `flushLocked` re-issues `AppendLogBulk` for every entry in `p.pending` before sending the current buffer, and a batch lands in `p.pending` on any error | `runner.go:358-366` (retry loop), `:390-392` (enqueue on error) |
| 7 | **Every** non-2xx is an error to the agent — `c.do` returns an `*HTTPError` for any status ≥ 400 with no permanence classification on this path | `internal/agent/client.go:107-109`, `:300-304` |
| 8 | Retries are stamped with the **original** timestamp: `now` is computed once per batch, outside the per-line loop | `runner.go:376` |
| 9 | Auto-flush cadence is **2 s**; the byte threshold is **4 KiB**; the pending cap is **1 MiB** | `runner.go:211`, `:255`, `:256` |
| 10 | SSE re-reads from the DB by `seq` and does **not** forward what was appended — the NOTIFY payload is the seq but the callback ignores it and re-queries `TailLogs(ctx, id, lastSeq, 10_000)` | `internal/controller/sse.go:117-120` |
| 11 | Therefore a duplicate is delivered to SSE clients **as an ordinary new line** with a strictly higher seq — the `seq > lastSeq` filter dedupes *transport* retries, never *content* duplicates | derived from 10 + Step 1 consequence 2 |
| 12 | The archiver encodes **whatever `TailLogs` returns**, so `line_count`/`max_seq` record the inflated count and log-trim's coverage check still passes ⇒ **the duplication is permanent and survives into the archive** | `internal/controller/archiver.go:81-92`, `:106`; `postgres.go:1519-1528` — **code-read only on this rig, see Part E** |
| 13 | A log-append failure **cannot fail the step**: the orchestrator never sees it, and `LogPusher` returns no error to its `io.Writer` caller | `runner.go:319-329` (`Write` always returns `n, nil`) |

**The single sentence the scenario tests:** facts 1-8 together mean that *any*
failure the agent observes on a bulk append — including one the controller
raised **after** committing part or all of the batch — is answered by re-sending
the identical batch, which the controller then commits **again**, with no
constraint, no dedupe and no ordering repair anywhere downstream.

### The two injection shapes, and why they are one mechanism

| Arm | What the agent sees | What the controller committed | Produces |
|---|---|---|---|
| **truncate** | `504` (nginx `proxy_read_timeout` expired) | a **prefix** — the loop is aborted by fact 5 | partial commit **and** lost ack, in one shot |
| **lostack** | `502` (client-facing leg went to a dead upstream) | the **whole batch** — a mirror subrequest delivered it to a real controller | a clean full-batch lost ack |

Both are the "lost ack" shape the brief names; `truncate` is additionally the
brief's Part B (mid-batch partial commit). **They are not two findings** — the
root cause is facts 1-4 in both cases.

### Why nginx, and how the W2-5 lesson is answered

The natural instrument is the campaign's per-URI nginx idiom, and W2-5 recorded
that **`nginx -s reload` is not guaranteed to take effect on an already-connected
agent** — it has a captured counter-example of a request succeeding inside a
nominally-armed window. After a reload, nginx's *old* workers keep serving
established keepalive connections with the *old* configuration, and the agent's
`http.Client` holds exactly such a connection. Three independent answers, all
required:

1. **`worker_shutdown_timeout 1s;`** (`nginx-logfault.conf`, main context) forces
   old workers to close within ~1 s of a reload, so the agent's next flush opens
   a fresh connection against a new worker. This *bounds* the arm latency; it
   does not prove anything.
2. **A fresh-connection probe** — `w3-4-logfault.sh probe` POSTs to a bulk URI
   and reads the `X-Logfault-Arm` response header, proving both that the regex
   location matched and which arm a new connection gets. **This still says
   nothing about the agent's existing connection**, and the runbook must not
   pretend otherwise.
3. **The bracket that actually carries every claim: the nginx access log.**
   `nginx-logfault.conf` defines a `logfault` log format carrying `$msec`,
   `$status`, `arm=$logfault_arm`, `ustatus=$upstream_status`, `rt=$request_time`
   and `reqlen=$request_length`, and logs **every** request. So each individual
   bulk request the agent made is classified, by nginx, with the arm that was in
   force **for that request**. **All armed/cleared claims in this runbook are
   made per-request from that log, never from the wall-clock window in which the
   arm command was issued.** A control run is called "uninjected" only if every
   one of its bulk requests logged `arm=none` and `status=204`.

---

## Stack

Plain `test/ha` **plus one overlay**, `compose/logfault.override.yaml`, which
only replaces the nginx config and adds a writable include volume. No product
code, no change to `test/ha/` (Task 3 owns the one sanctioned amendment there
this wave).

```bash
cd test/ha
export COMPOSE_FILES="-f docker-compose.ha.yaml -f ../edgecase/compose/logfault.override.yaml"
export MSYS_NO_PATHCONV=1          # Git Bash rewrites container paths (W2-5)
docker compose $COMPOSE_FILES up -d --build
```

Throughout, `psql` means
`docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "<sql>"`,
and `API` means `curl -sS -H "Authorization: Bearer ha-admin-token"` against
`http://localhost:18080`.

**Workload — one new fixture.** `workloads/logburst.payload.json`, job
`edge-logburst`, modelled byte-for-byte on the known-good `tick.payload.json`
(same `apiVersion`/`kind`/`native`/`agentSelector`), differing only in the step
body:

```
echo burst-begin
sleep 8
for i in $(seq 1 2000); do echo "burst-$i"; done
sleep 30
echo burst-end
```

Rationale, point by point:

- **2000 lines emitted as fast as the shell can write them** is what produces
  batches large enough for a mid-batch truncation to be observable. At 1 line/s
  (`tick`, `longrun`) every batch is ~2 lines and `proxy_read_timeout` can only
  ever land between batches. **`edge-logburst` emits exactly 2002 lines**
  (`burst-begin` + 2000 + `burst-end`); that constant is the I4 comparand.
- **Line contents are unique and self-indexing** (`burst-<i>`), so a duplicate is
  identifiable without joining anything, and reordering is a monotonicity check
  on the index.
- **`sleep 8` before the burst** gives a window to arm after the step has started
  and after the agent's connection to nginx already exists — which is the case
  W2-5 says is dangerous, and therefore the case worth testing.
- **`sleep 30` after the burst** is a quiet window in which the only bulk traffic
  is *retries* of the failed batch. This is what makes the retry cadence
  measurable and keeps new output from interleaving with the duplicate.
- **No side-effect file and no mutex**, deliberately: nothing must invite an I2
  or I3 reading (§Invariants).

---

## BASELINE GATE — do not proceed past a failing check

Write every gate output to `$SCRATCH/gate.txt` unless a per-check file is named.

```bash
SCRATCH="<scratchpad>/w3-4" ; mkdir -p "$SCRATCH"
```

- **G0 — worktree.** `git rev-parse --show-toplevel` is `.../wt-edge-spec`,
  branch `plan/edge-case-w3`. `docker compose ls` shows the developer stack
  (project `unified-cd`) untouched; `test/ha`'s project is `unified-cd-ha`, so
  they do not collide. **STOP** if the toplevel is the main checkout.
- **G1 — the overlay actually took.** Compose merges volume entries by target
  path, so the nginx.conf replacement is silent if it fails.
  1. `docker compose $COMPOSE_FILES config | grep -A4 'nginx'` shows
     `nginx-logfault.conf` mounted at `/etc/nginx/nginx.conf`.
  2. `docker compose $COMPOSE_FILES exec -T nginx grep -c logfault /etc/nginx/nginx.conf`
     is **non-zero**.
  3. `docker compose $COMPOSE_FILES exec -T nginx nginx -t` → syntax ok.
  **STOP on any mismatch** — against the stock config every arm below is a
  silent no-op, which is precisely the W2-5 failure.
- **G2 — stack health.** All three controllers up; `API /readyz` → 200;
  `GET /api/v1/agents` lists **agent1 and agent2**, both connected, and record
  their **labels** (`kind:linux` is what `edge-logburst` selects on; a scenario
  that assumed a selector without reading it has been wrong twice in this
  campaign). → `$SCRATCH/gate-g2-agents.txt`.
- **G3 — clean slate.**
  `SELECT count(*) FROM runs WHERE status IN ('Pending','Queued','Running');` → 0,
  and `SELECT count(*) FROM logs;` recorded (it need not be 0, but every query
  below is run-scoped and the starting value must be known, because **`seq` is
  global** — Step 1).
- **G4 — the fault include is in its cleared state, and the location is live.**
  `../edgecase/tools/w3/w3-4-logfault.sh clear`, then
  `w3-4-logfault.sh probe`. The probe must print an `X-Logfault-Arm: none`
  header. **STOP if the header is absent** — that means the regex location did
  not match and nothing below is being injected. → `$SCRATCH/gate-g4-probe.txt`.
  Also `w3-4-logfault.sh show` to record the file contents.
- **G5 — the job applies.** `POST /api/v1/jobs` with
  `workloads/logburst.payload.json` → **200**. A new fixture 400ing is a
  branch-internal asset bug (W1-4 precedent) and must be fixed and recorded
  before any measurement. → `$SCRATCH/gate-g5-job.txt`.
- **G6 — Postgres statement logging, armed and verified in a FRESH session.**
  **One `ALTER SYSTEM` per `psql -c`** — two in one `-c` is an implicit
  transaction which Postgres refuses **silently**, while `pg_reload_conf()` still
  returns `t`, so the broken form is indistinguishable from success (W2-7's
  branch-internal asset bug).

  ```bash
  docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c "ALTER SYSTEM SET log_statement='all';"
  docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c "ALTER SYSTEM SET log_line_prefix='%m [%p] h=%h ';"
  docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c "SELECT pg_reload_conf();"
  docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "SHOW log_statement;"    # must print: all
  docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "SHOW log_line_prefix;"  # must print: %m [%p] h=%h
  ```

  **STOP on either mismatch.** The instrument's job here is to show the *insert
  side* of the truncation — the last `INSERT INTO logs` that committed before the
  abort, and the absence of any `ROLLBACK` — and `h=%h` attributes it to a
  controller replica. **Budget it**: 2000 inserts plus 2000 `pg_notify` calls per
  batch is ~4000 statements per attempt, so arm it only for the specific attempt
  being instrumented and `RESET` immediately after. Record both `SHOW` outputs
  here and again at every revert. → `$SCRATCH/gate-g6-arm.txt`.
- **G7 — the nginx access log is reaching `docker compose logs`.** Make one
  ordinary API call and confirm a `logfault`-format line appears in
  `docker compose $COMPOSE_FILES logs nginx --since 30s`. **STOP if not** — the
  entire bracketing discipline of this scenario rests on that log.
  Note that `docker compose logs --since` lags several seconds (W2-8 note 5):
  wait ≥6 s and widen the window before concluding anything from it.
  → `$SCRATCH/gate-g7-accesslog.txt`.
- **G8 — API 500s.** The API on this rig has been intermittently returning 500s.
  Record, for every gate and every trigger, how many attempts it took. A 500 on a
  trigger is not a finding of this scenario; a 500 mid-measurement invalidates
  that attempt and it must be discarded and re-run, not reasoned around.

---

## Part A — the control: an uninjected `edge-logburst`

**Deliverable:** exactly **2002** `logs` rows for the run, **zero** groups with
`count(*) > 1`, burst indices monotonic in `seq` order, run `Succeeded` — and an
nginx access-log extract proving **every** bulk request in the run's window was
`arm=none` / `status=204`.

- **A1.** With the fault cleared (G4) and confirmed, trigger:
  `API -X POST -d '{"jobName":"edge-logburst"}' /api/v1/runs`. Record
  `runID_ctrl`, the host clock at trigger, and the number of attempts (G8).
- **A2.** Wait for terminal. Record status and the run's timestamps —
  **the `runs` table has `created_at`/`updated_at`, not `started_at`/`ended_at`**
  (an earlier draft of this query errored with `column "started_at" does not
  exist`; the error is in `$SCRATCH/partA-control.txt` and is the only `ERROR:`
  in the whole Postgres capture, so do not misread it as a product fault).
  **STOP and re-run if not `Succeeded`** — I4's scope is a `Succeeded` run.
- **A3 — the three measurements.** → `$SCRATCH/partA-control.txt`

  ```sql
  SELECT count(*) FROM logs WHERE run_id = '<runID_ctrl>';
  SELECT line, ts, count(*), array_agg(seq ORDER BY seq)
    FROM logs WHERE run_id = '<runID_ctrl>'
   GROUP BY line, ts HAVING count(*) > 1;
  -- monotonicity: the burst index must increase with seq
  SELECT count(*) FROM (
    SELECT (regexp_match(line,'^burst-([0-9]+)$'))[1]::int AS i,
           row_number() OVER (ORDER BY seq) AS pos
      FROM logs WHERE run_id='<runID_ctrl>' AND line ~ '^burst-[0-9]+$'
  ) t WHERE i <> pos;
  ```

  Expected: `2002`, **zero rows**, `0`.
- **A4 — the bracket that licenses the word "uninjected".** Extract every
  `logs/bulk` line for this run id from the nginx access log over
  `[trigger, ended_at]` and show that all are `arm=none status=204`.
  → `$SCRATCH/partA-nginx.txt`. **Do not call this run a control on the basis of
  "the fault was cleared at the time"** — the whole point of the W2-5 lesson is
  that arm state and wall clock are different things.
- **A5 — batch-size census, needed to size the injection.** From the same
  extract, tabulate `reqlen` and count per request. This is how many lines a
  single bulk request actually carries on this rig, and it decides whether a
  200 ms `proxy_read_timeout` can land mid-loop at all. **If the census shows
  the burst is split into many small batches, raise the burst size or lower the
  timeout before running Part B, and record that you did.**

**Falsification.** If A3 shows duplicates or a count ≠ 2002 with no injection at
all, this scenario's premise is wrong and *that* is the finding: report the
control numbers, stop, and do not proceed to Part B on a broken baseline.

---

## Part B — the truncate arm: partial commit + lost ack

**Deliverable:** a `Succeeded` run whose `logs` row count **exceeds 2002**, with
a named set of `(line, ts)` groups having `count(*) > 1`, each traceable to a
`504` in the nginx access log; **plus** the exact prefix length committed by the
aborted attempt, and the Postgres-side evidence that the abort left no rollback.

**Attempt discipline.** This is the brief's capped arm. **Cap: 10 attempts.**
Record the count reached **either way**, and if it is not hit, file the code-read
argument (facts 1-5) with an explicit **"not reproduced live"** label — the
W2-3 Arm D precedent, accepted at 0/10. **Do not write that the window is
unreachable.**

- **B1.** Clear + probe. Trigger `edge-logburst`, record `runID_trunc` and the
  trigger instant.
- **B2 — arm during the `sleep 8`, i.e. against an already-connected agent.**
  `w3-4-logfault.sh truncate 200ms`, then **immediately** `w3-4-logfault.sh probe`
  → must print `X-Logfault-Arm: truncate`. Record both instants.
  → `$SCRATCH/partB-arm.txt`.
- **B3 — hold the arm for a bounded window, then clear.** While armed, **every**
  retry of the failed batch is also truncated and commits **another** prefix, so
  the duplication compounds. Hold for **6 s** (≈3 retries at the 2 s cadence),
  then `w3-4-logfault.sh clear` + probe. Record both instants.
  **The window must be ended deliberately and the write-up must say so** — do not
  describe an unbounded arm you closed yourself.
- **B4 — wait for terminal and confirm `Succeeded`.** If the run is not
  `Succeeded`, the I4 attribution does not apply to it; record why and re-run.
- **B5 — measure.** → `$SCRATCH/partB-dup.txt`

  ```sql
  SELECT count(*) FROM logs WHERE run_id = '<runID_trunc>';
  SELECT line, ts, count(*) AS copies, array_agg(seq ORDER BY seq)
    FROM logs WHERE run_id = '<runID_trunc>'
   GROUP BY line, ts HAVING count(*) > 1
   ORDER BY min(seq);
  SELECT copies, count(*) AS lines_with_that_many_copies FROM (
    SELECT count(*) AS copies FROM logs WHERE run_id='<runID_trunc>'
     GROUP BY line, ts) t GROUP BY copies ORDER BY copies;
  ```

  **The discriminator, stated explicitly in the write-up:** duplicates share
  `line` **and** `ts` and differ **only** in `seq`, because the batch timestamp
  is stamped once per batch and re-used on retry (`runner.go:376`), while `seq`
  is server-assigned per INSERT from a global sequence (Step 1). Group on
  `(line, ts)`; never on `seq`.
- **B6 — the prefix length, which is what makes this a *partial commit* and not
  just a duplicate.** From the `copies` histogram, the highest-`copies` group is
  the prefix that every attempt re-committed; the boundary index is the last
  `burst-<i>` with more copies than its successor. Report it as **the number of
  lines the aborted request committed before nginx's timeout cancelled the
  request context**. Cross-check against the Postgres log (G6) for that window:
  the last `INSERT INTO logs` before the gap, and — the point — **no `ROLLBACK`
  covering the committed prefix**, because there was never a transaction (fact 2).
  → `$SCRATCH/partB-pglog.txt`, and `RESET log_statement` immediately after,
  verified in a fresh session.
- **B7 — reordering.** Re-run A3's monotonicity query. A non-zero result is the
  "no reordering" clause of I4. Also show it concretely: the second copy of
  `burst-1` has a `seq` **greater** than the first copy of `burst-<prefix+1>`,
  i.e. the run's log replays its own beginning after having moved past it.
- **B8 — what the operator can see.** Record every surface, because "nothing is
  wrong except the data" is the severity argument:
  `GET /api/v1/runs/{id}` (status `Succeeded`), `GET /api/v1/runs/{id}/steps`
  (step `Succeeded`, exit 0), the controller logs for that window
  (`grep -c "dropping log lines"` → expect 0; the 504s are nginx's, and the
  controller's own error line for the cancelled insert), and whether any
  `[N log line(s) dropped: controller unreachable]` marker appears (it must
  **not** — that marker is gated on `p.droppedLines > 0`, incremented **only** by
  `appendPendingLocked`'s byte-cap eviction, `runner.go:432`, which 1 MiB never
  reaches here). → `$SCRATCH/partB-surfacing.txt`.

**Falsification.** If the armed window produces **no** duplicates, the injection
did not reach the agent's connection — check A4's bracket for this run's window
before concluding anything about the code. If it produces duplicates but the
count is exactly `2002 + 2002` (a whole extra copy), then the timeout landed
between batches and this is the **lostack** shape, not a partial commit: say so
and count it against Part C, not Part B.

---

## Part C — the lostack arm: a whole batch committed behind a 502

**Deliverable:** a `Succeeded` run in which one **entire** batch is duplicated,
with the mirror's own delivery visible controller-side, proving the commit
happened for a request the agent was told had failed.

- **C1.** Clear + probe, trigger `edge-logburst`, record `runID_lost`.
- **C2.** During the `sleep 8`: `w3-4-logfault.sh lostack`, probe → must print
  `X-Logfault-Arm: lostack`. Hold **4 s** (≈2 retries), then clear + probe.
- **C3.** Confirm `Succeeded`, then run B5's queries. Expected shape: whole
  batches duplicated (not prefixes), `copies` = 1 + number of truncated attempts.
- **C4 — prove the mirror delivered.** The client-facing leg logs `502` with
  `target=blackhole`; the mirror subrequest is a *separate* upstream request. Show
  the controller received and committed it: the `logs` rows exist with `ts` equal
  to the failed batch's, and the Postgres log (if armed) shows the inserts inside
  the 502's own `$request_time`. → `$SCRATCH/partC-mirror.txt`.
- **C5 — RISK, and what to do if it materialises.** nginx's `mirror` fires in the
  **precontent** phase, strictly before the content phase's `proxy_pass`, so the
  ordering is a property of nginx's phase engine, not of timing. But mirror
  subrequests are fire-and-forget and their completion is not contractually
  guaranteed once the main request finalises. **If C4 shows the mirror did not
  deliver, do not fake it**: record the negative result, and note that Part B's
  truncate arm already establishes the lost-ack shape (the agent sees a failure
  for a request that committed), of which Part C is only the cleaner
  presentation. Part C is a nice-to-have; Part B is the finding.

---

## Part D — downstream: the duplicate is delivered to SSE

**Deliverable:** a captured SSE stream in which the duplicated lines appear as
ordinary `log` events, in duplicate, with increasing `seq`.

This is what makes W3-4 an I4 finding rather than a database curiosity — the
duplicate is not merely stored, it is **served**.

- **D1 — CORRECTED, see correction 1 in the box at the top of this file.**
  ~~Attach to `http://localhost:18080/api/v1/runs/{id}/events` (the LB) before
  triggering the Part B run, in the background.~~ **That capture dies at the
  first arm**: `worker_shutdown_timeout 1s` closes every connection through
  nginx on reload, and the first attempt recorded exactly one event before
  `curl: (18) transfer closed with outstanding read data remaining`
  (`$SCRATCH/partD-sse.txt`, kept as the counter-example). **Attach directly to
  a controller instead**, from inside the nginx container so no new image is
  needed:

  ```bash
  docker compose $COMPOSE_FILES exec -T nginx curl -sSN -m 90 \
    -H "Authorization: Bearer ha-admin-token" \
    "http://controller1:8080/api/v1/runs/$RID/events" > "$SCRATCH/partD3-sse-live.txt" &
  ```

  This is the same handler and the same `LISTEN "log_appended:"+id` path;
  the LB adds nothing to the test. `GET /api/v1/runs/{id}/events` is registered
  outside the `/api/v1` group with `ServerAuth` + `requireMinRole("viewer")`
  (`internal/controller/server.go:336-337`), so the admin token works either
  way. Record the PID in `$SCRATCH/samplers.pid` — **and note that a
  `docker compose exec` sampler survives the shell that launched it**, which is
  how one was still alive at teardown pass 2 (see the execution notes).
- **D2.** After the run ends, count occurrences of a known-duplicated `burst-<i>`
  in the captured stream. **Expected: the same multiplicity as the DB**, because
  the backfill and the live path both read `TailLogs` by `seq` (fact 10) and the
  duplicate has a higher seq than the cursor (Step 1 consequence 2), so nothing
  filters it.
- **D3 — the negative control that makes D2 mean something.** Do the same count
  for the Part A control run's stream (or replay
  `GET /api/v1/runs/{runID_ctrl}/events`, which terminates immediately for a
  terminal run — `sse.go:104-110`): multiplicity **1** for every line.
- **D4 — and the ordinary log read.** `GET /api/v1/runs/{id}/logs` and
  `/logs/range` show the same duplication. Note whether the WebUI was inspected;
  if not, **say so** rather than claiming "every surface" (the W2-9 lesson).

---

## Part E — the archive limb: CODE-READ ONLY on this rig, and why

**This limb is not measured and must not be written up as if it were.**

`test/ha/docker-compose.ha.yaml` sets only `UNIFIED_DB_DSN`, `UNIFIED_TOKEN` and
`UNIFIED_CONTROLLER_KEY_FILE`, so `cmd/controller/main.go:303-322` takes neither
object-store branch, `obj == nil`, and the controller logs
`no object store configured — log archival disabled`. Consequently the archiver
is **never started** (`main.go:399`, `if obj != nil`), no `run_log_archives` row
is ever written, and no run is ever sealed.

- **E1 — verify the premise live rather than asserting it** (one command, and it
  is the only thing Part E measures): confirm the warning in the controller logs
  and `SELECT count(*) FROM run_log_archives;` → 0.
  → `$SCRATCH/partE-noobjstore.txt`.
- **E2 — the code-read claim, stated as such.** `archiveRunLogs` encodes whatever
  `TailLogs` returns (`archiver.go:81-92`) and records `line_count`/`max_seq`
  from that same slice (`:106` → `postgres.go:1519-1528`). It applies no
  de-duplication and no ordering repair. So a duplicated run archived by a rig
  that *does* have an object store would carry the duplication into
  `runs/<runID>/logs.ndjson`, and the trim-coverage check — which compares the
  recorded `line_count`/`max_seq` against the DB — would still pass, because both
  sides count the same inflated set. **The duplication is therefore permanent**:
  after a trim the archive is the only copy and it is the corrupted one.
- **E3 — the honest option.** Either leave E2 as **code-read only**, or defer the
  measurement to after Task 3 (which adds Garage) and re-run it there. **Say
  which was chosen.** Do not present E2 as measured, and do not let the word
  "permanent" in the finding rest on an unmeasured limb without that label.

---

## Teardown

```bash
# 1. revert the instrument FIRST, verified in a FRESH session
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c "ALTER SYSTEM RESET log_statement;"
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c "ALTER SYSTEM RESET log_line_prefix;"
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c "SELECT pg_reload_conf();"
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "SHOW log_statement;"   # must print: none
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "SHOW log_line_prefix;" # must print: %m [%p]
# 2. clear the fault and prove it is clear
../edgecase/tools/w3/w3-4-logfault.sh clear && ../edgecase/tools/w3/w3-4-logfault.sh probe
# 3. down
docker compose $COMPOSE_FILES down -v
```

- **Cancel every surviving run before teardown.**
- **Kill every background sampler (the SSE captures above) and *capture* that,
  do not assert it.** Keep PIDs in `$SCRATCH/samplers.pid`, `kill` them
  explicitly, then show `jobs` empty and `ps -W | grep -iE "curl|psql|python"`
  matching nothing, on **two** passes: before the revert and immediately before
  `down -v`. → `$SCRATCH/teardown.txt`. (W2-6 left one running; W2-8's first
  session asserted hygiene in prose only.)
- Copy `$SCRATCH` into the campaign evidence root at the wave checkpoint
  (`test/edgecase/README.md` § "Raw evidence").

---

## Recording rules

- **Duplicated committed log content on a `Succeeded` run ⇒ I4 violation**,
  quoting `:51` verbatim and naming which clauses are hit (count, duplicates,
  reordering) and which is **not** exercised (archives readable — Part E).
  Severity argument, stated rather than asserted: no run state is wrong, no side
  effect repeats, nothing is lost — but the run's own log is silently and
  **permanently** wrong, the corruption is served to every reader including SSE,
  there is no marker of any kind, and the trigger is an ordinary transient
  failure of one HTTP request rather than an exotic fault. Judge **major**, not
  critical, unless Part E is actually measured (it is not on this rig): critical
  would need the archive limb demonstrated, and it is code-read.
- **A 500 that commits a prefix with no way for the client to learn how far it
  got** — judge on whether any doc promises atomicity. Per the pre-execution
  survey, **the docs are silent, not contradicted**; say exactly that and rest on
  I4. Read `docs/troubleshooting.md:889-899` in full before citing it: it
  authorises **retry** and says nothing about partial commits, which is a gap,
  not a sanction (contrast W2-7, where the cited passage did sanction the
  behaviour).
- **Do not split Part B and Part C into two findings.** Same root cause (facts
  1-4). If Part C fails to reproduce, that is a note inside the entry, not an
  absence.
- **Report the Part B attempt count either way**, with the cap (10) stated.
- **Never write "never" or "unbounded" for a window this runbook closed itself.**
  The armed windows are 6 s and 4 s by construction; the duplication count is a
  function of that choice and must be reported as such. What is unbounded is
  **code-read**: nothing removes a duplicate once committed, and `flushLocked`
  will keep re-sending for as long as the failure persists.
- Entry titles must say **"observation"** for observation entries
  (`FINDINGS.md:481`) and repeat it in the Severity line as `minor (observation)`.
  A defect in this campaign's own assets gets an explicit `Classification:` line
  and sits outside both tallies (`FINDINGS.md:487`).
- Every number cites a `$SCRATCH` filename whose time window covers it. Derived
  figures say "derived"; code-read figures say "code-read"; uncaptured live
  observations say `(observed live, raw output not captured to scratchpad)`.
- **Cross-references, not re-filings.** Three already-filed items live in this
  same machinery and none of them is this: **F9** (agent `LogPusher` 1 MiB silent
  drop) is *loss at the byte cap*; the **W1-2 major I4** is *loss at the 5 s
  step-end `Flush` budget*; the **W1-6 observation** is *quadratic request
  amplification with no loss*. W3-4 is the fourth, disjoint shape: **no loss at
  all, and a permanent surplus instead**. Say so explicitly so triage does not
  merge them.

---

## Execution notes — 2026-07-30 run (read before re-running)

Executed against `test/ha` + `compose/logfault.override.yaml` on branch
`plan/edge-case-w3`, `23:35:26Z – 23:50:06Z`. **Two `FINDINGS` entries: 1
violation (major, I4) and 1 observation (minor).** No branch-internal asset bug.
The developer stack (`docker compose ls` project `unified-cd`) was untouched
before and after (`w3-4/gate.txt`, `w3-4/teardown.txt`).

**Six runs, all `Succeeded`** (`w3-4/consolidated.txt`). Emitted line count is
2002 for every one of them:

| run | arm | rows | surplus | dup groups | out-of-order |
|---|---|---|---|---|---|
| `a644004c` | none (control) | 2002 | 0 | 0 | 0 |
| `e87666b7` | truncate 200 ms, 8.8 s | 2558 | 556 | 383 | 2383 |
| `16683d6e` | lostack, 5.8 s | **8493** | 6491 | 2000 | 8490 |
| `a0b22da5` | truncate 200 ms, 8.8 s | 2773 | 771 | 400 | 2581 |
| `78788e0d` | none in window (timing miss ⇒ second control) | 2002 | 0 | 0 | 0 |
| `f7d619ac` | truncate 200 ms, 8.9 s | 2626 | 624 | 276 | 2441 |

**Every armed/cleared claim above is per-request from the nginx access log**, not
from the wall clock: `w3-4/partA-nginx.txt`, `w3-4/partB-nginx-attempt1.txt`,
`w3-4/partC-dup.txt`. Both control runs are called uninjected because **all** of
their bulk requests logged `arm=none status=204`, which is the only basis on
which the word is used anywhere in this scenario.

**Instrument hygiene.** `log_statement='all'` was armed once
(`w3-4/gate-g6-arm.txt`) for the Part B attempt-1 window only — 71,852 log lines
in ~90 s — and `RESET` immediately after, verified in a fresh session
(`w3-4/partB-pglog-disarm.txt`: `none`). `log_line_prefix` stayed armed until
teardown and was `RESET` there, verified fresh (`w3-4/teardown.txt`:
`%m [%p]`). **One `ALTER SYSTEM` per `psql -c` throughout**, per W2-7.

**Sampler hygiene was captured, not asserted — and it caught something.** Two
passes, `jobs` plus `ps -W | grep -iE "curl|psql|python"`, both empty on the
host. But the pass-2 check inside the nginx container found a **still-running**
`curl` SSE sampler (pid 169, the Part D2 stream, launched without `-m`), which
was then terminated by `down -v` rather than explicitly killed
(`w3-4/teardown.txt`; the background task exited 137). **Two lessons:** a
`docker compose exec` sampler outlives the shell that launched it and does not
appear in the host's `jobs` or `ps`, so **the container must be checked too**;
and always pass `-m <seconds>` to a `curl -N` sampler.

**Seven things a re-run should know.**

1. **`logs.seq` is one global sequence** — settled from the migration, see Step 1.
   The plan's guess was right; it is now evidence. Consequence that bit twice
   while writing queries: a run's `max(seq)-min(seq)+1` is **not** its line count
   once any other run is active, and the duplicate always sorts *after* the
   original because `seq` is assigned at INSERT.
2. **The lostack (mirror) arm was flagged as a risk and it worked perfectly.**
   nginx's `mirror` runs in the precontent phase and the main request does not
   finalise until the subrequest completes — visible directly in the access log
   as `502 … rt=0.659 urt=0.000`: nginx spent 659 ms on a request whose only
   upstream leg refused instantly, which is the mirror's own duration. The
   controllers logged **204** for all 15 mirrored requests the agent was told had
   failed. It is the strongest single result in the scenario (4.24x inflation)
   and the cheapest to re-run.
3. **Do not use `sideeffect.payload.json` as a chatty workload.** Its `echo` is
   redirected to a file; it emits **zero** log lines (Correction 1 at the top).
   `edge-logburst` exists for this and its 2002 is the comparand.
4. **Batch shapes differ per agent and per run** and must be read off the data,
   not assumed. `agent2` produced 421 + 1579; `agent1` produced 421 + 411 + 541 +
   492 + 135. The `(line, ts)` grouping is what makes this irrelevant — each
   distinct `ts` is exactly one batch (`runner.go:376`).
5. **The controller reports 500 for a request nginx already answered 504**, at
   `duration_ms:200` — exactly the injected timeout. That pair of log lines
   (nginx 504 / controller 500, same instant, same URI) is the cleanest single
   piece of evidence that the ack was lost *after* the commit, and it is what to
   grep for first on a re-run.
6. **Budget ~40 s per run.** Trigger → claim ≈ 2 s, `sleep 8`, burst, `sleep 30`.
   Arm at trigger + 5 s, hold 8 s. Arming later than trigger + 8 s misses the
   burst entirely (that is what run `78788e0d` is).
7. **Part E was left as code-read and must be re-run after Task 3.** No object
   store on this rig: `run_log_archives` = 0 rows, three
   `no object store configured — log archival disabled` warnings
   (`w3-4/consolidated.txt`). The claim that the duplication survives into the
   archive and passes log-trim's coverage check rests entirely on
   `archiver.go:81-92` / `:106` and is labelled as such in the finding. **It is
   the limb that would raise the severity from major to critical**, so it is
   worth ten minutes on the Garage rig.
