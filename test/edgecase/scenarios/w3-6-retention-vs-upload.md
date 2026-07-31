# W3-6 — run retention racing an in-flight artifact upload

**Wave W3, Task 5.** The scenario attacks a **TOCTOU whose own code comment
believes it is closed**, and the finding it is written to produce is not the race
but an **asymmetry**: `deleteRunEverywhere` re-sweeps the log-archive key after
`DeleteRun` to catch a late write — the code names this "race (b)" and closes it
— and does **not** re-sweep `artifacts/<runID>/`.

---

## The boundary, stated before anything else

**This scenario does not re-file the orphan class W3-2 already produced.** W3-2
Part B arm 2 left a live orphaned `runs/<id>/logs.ndjson` by a *different*
production path — a failed compensating `obj.Delete` after `CreateLogArchive`
failed (`FINDINGS.md:1760`, `internal/controller/archiver.go:111-114`) — and its
own entry says explicitly that **W3-6 owns the orphan finding** and that W3-2
"contributes a second, independent production path for the same class". This
runbook's mechanism is the **artifact-prefix TOCTOU**, which is neither the same
code path nor the same key space. **File on the mechanism, cite W3-2's as the
sibling, and do not re-measure it.**

**Two other already-filed items sit close enough to be mistaken for this one.**
W3-2's Part C established the *clean* artifact-failure baseline (a blocked `PUT`
leaves no object and no incomplete multipart upload, `FINDINGS.md:1793`) — that
is the control this scenario's orphan is a delta from, not a competitor. And
**W0-1 / W3-2's startup entries are unrelated**; nothing here restarts a
controller.

**And one constraint that governs how evidence is captured at all.** The agent
destroys every controller error message in transit (`Client.do` →
`"response omitted"`, `internal/agent/client.go:107-108`), so **no agent-side
failure is diagnosable from a run's own logs**. This scenario mostly sidesteps
that by driving the upload itself (§Instrument), but the rule still holds for
anything the real agents do: capture the controller's `slog` container-side, and
capture it **before** any `up -d --force-recreate`.

---

## The instrument, declared up front because it is the least obvious thing here

**The scenario needs an artifact `PUT` against a run that is already terminal.**
`DELETE /api/v1/runs/{id}` refuses a non-terminal run with **409**
(`internal/controller/api_runs.go:427-432`), while a run driven by a real agent
is `Running` for the whole of its `uploadArtifact` step *as long as nothing
interferes*.

**But the two windows DO overlap, and an earlier version of this section was
wrong to say they never do.** `handleCancelRun` marks the row terminal
**synchronously** — `MarkRunFinished(r.Context(), id, api.RunCancelled)`,
`api_runs.go:374` — while the executing agent only learns by **polling**:
`var CancelPollInterval = 5 * time.Second` (`internal/agent/orchestrator.go:37`),
poller at `:122-147`, terminal branch at `:137-146` (which also covers a
stuck-run reap and a failover-driven terminalisation, via `reapedByMaster`). So
**an ordinary agent-driven run is terminal-while-still-uploading for up to 5 s**,
and the manual DELETE is legal for all of it, with a real agent and no
instrument. Whether the in-flight `Put` beats the poller's `cancelRun()` is an
open empirical question — **the 2026-07-31 run did not attempt it**, so that
window is *untested*, not unreachable. **A re-run with time to spare should try
it**: it is the production shape, and producing it once would remove the only
instrument in the scenario.

What makes the scenario constructible *on demand*, with a window as wide as the
payload rather than ≤5 s and no dependence on winning a footrace, is that
**`handleArtifactUpload` accepts uploads for terminal-but-existing runs on
purpose** — `agentRunGuard(..., rejectTerminal=false)`
(`internal/controller/api_artifacts.go:47`,
`internal/controller/agent_guard.go:98-132`), with the reasoning spelled out in
the handler's own comment (`api_artifacts.go:59-61`). So the upload is issued by
a **synthetic agent**: a curl-driven identity that enrolls, claims a run,
finishes it, and then uploads — every step through a documented, production
HTTP route, with no SQL and no product change.

**This is an instrument, and the write-up must say so.** What is synthetic is
*who* holds the credential, not *what code runs*: the `PUT` lands on the same
handler, passes the same guard, and reaches the same `objStore.Put` as an
agent's. The realistic production shape it stands in for is a run that is
terminalised out from under a live agent — by cancel, by the stuck-run reaper,
or by a failover — after which that agent's `uploadArtifact` step still `PUT`s,
is still accepted, and can still be raced by retention. **State that shape and
label the synthetic half; do not present the curl as an agent.**

The window is opened by **feeding the request body slowly** (`curl --limit-rate`)
through the `bigbody` overlay's `proxy_request_buffering off`, which
`nginx-bigbody.conf` records as being there **for this scenario**:

> `proxy_request_buffering off — LOAD-BEARING for W3-6. With buffering on (the
> default) nginx spools the entire body to disk before opening the upstream
> request, which moves the controller's Put entirely outside the wall-clock
> window in which the agent is uploading — and W3-6's TOCTOU width IS that Put.`

**The S3 interposer is deliberately NOT used.** `s3-latency` is not a dependable
width knob at large values (W3-2 measured it *breaking* a `Put` at 30:
`connection refused` is `error`, not `timeout`, so `proxy_next_upstream` never
fired and the request 502'd after 21.037 s —
`test/edgecase/README.md` § "the S3 interposer"), and `pause garage` behind the
interposer — the lever W3-2 replaced it with — **cannot be used here at all**,
because the DELETE this scenario is timing *itself* calls `obj.Delete` and
`obj.List` and would hang alongside the upload. A slow client is a lever that
widens exactly one side of the race, which is what the race needs.

---

## Corrections to inherited facts, established BEFORE execution

Per the W1 carry-forward rule the plan's "Verified code facts" block is a set of
**claims**. Every row of §"Verified mechanism" was re-read at this branch's HEAD.
Two citations are off and one previously-open question is settled; **no
conclusion changes**.

- **CORRECTION 1 — the chunked-upload citation is `internal/agent/client.go:349-371`, not `:352-360`.**
  The plan's W3-6 block (`docs/superpowers/plans/2026-07-30-edge-case-campaign-w3.md:125`)
  cites `client.go:352-360` for "the upload is chunked with no Content-Length".
  `func (c *Client) UploadArtifact` starts at **`:349`** and returns at **`:371`**;
  no `Content-Length` is set anywhere in it. **W3-2 already filed this same
  correction** (its §"Corrections to inherited facts", CORRECTION 2) — recorded
  again only because this scenario's fixture header
  (`test/edgecase/workloads/artifact-large.yaml`) still carries the old span.
  **The fact is correct; the span is wrong.**
- **CORRECTION 2 — the guard comment spans `api_artifacts.go:57-61`, and the plan's `:60-61` names only its last clause.**
  The plan (`:126`) cites `:60-61` for "terminal-but-existing runs still accept
  uploads". At HEAD the comment runs `:57-61`; `:57-59` is the orphan warning
  ("A late upload for a deleted run would create an orphaned object nothing ever
  cleans up") and `:59-61` is the terminal-run allowance. **Both halves are the
  same comment**, which matters for §Invariants' contract question — quote it
  whole.
- **SETTLED — the plan's open question at `:144`, "whether `handleArtifactList`'s
  route group enforces run existence", is answered NO.** The route group is
  `s.r.Route("/api/v1/runs/{runID}/artifacts", ...)` at
  `internal/controller/server.go:496-499` and carries only `s.agentOrServerAuth`;
  the handler (`api_artifacts.go:120-147`) never calls `GetRun` and answers
  purely from `objStore.List(prefix)`. **Part B turns this from a code read into
  a measurement**, and it is the reason Part B must not assume a 404 (W3-2
  already found `GET /api/v1/runs/{id}/logs` returning **200** for a deleted
  run, `FINDINGS.md:1760`).
- **CONFIRMED, not corrected:** `run_retention.go:131-173` is exactly
  `deleteRunEverywhere`'s span, the five steps are at the lines the plan gives,
  and `DELETE /api/v1/runs/{id}` (`server.go:375`) calls it synchronously at
  `api_runs.go:433` behind a terminal check at `:427-432`.

---

## Invariants

Quoted verbatim from `docs/superpowers/specs/2026-07-29-edge-case-testing-design.md`.

- **I4 (log/artifact integrity)** — `:51`:

  > "**Log/artifact integrity** — a Succeeded run's log line count matches what
  > the workload emitted; no duplicates, no reordering; archives stay readable"

  **This is the scenario's primary invariant and its fit must be argued, not
  assumed** — the natural temptation is to stretch "archives stay readable" over
  an orphan, and W3-2 already ruled that it does not:

  > "**The reverse state — an object with no record — does NOT break this
  > clause.** It is an orphan, not an unreadable archive."
  > (`w3-2-s3-outage.md` §Invariants)

  **That ruling is binding on this branch and this scenario inherits it.** So the
  I4 question here is narrower and sharper: an artifact object that survives the
  deletion of its own run is **not** an unreadable archive — it is a *readable*
  one that should not exist. Judge it against I4's exact wording and say plainly
  which clause it does and does not touch. If the honest answer is "no clause of
  I4 is contradicted", write that and rest the entry on the contract limb below.
- **I7 (state display consistency)** — `:54`:

  > "**State display consistency** — run status, approval status, and audit rows
  > never contradict each other or reality"

  **In scope only if Part B finds a surface that lies.** `GET /api/v1/runs/{id}`
  → 404 while `GET /api/v1/runs/{id}/artifacts` → 200 with contents is two API
  surfaces disagreeing about whether a run exists. Decide deliberately whether
  that is "contradicting reality" or merely two routes with different
  preconditions, and note that W3-2 already recorded the same shape on the logs
  route without filing it as an I7 violation. **Consistency with that call is
  required; if this scenario departs from it, say why.**
- **NOT I1.** I1 is "every API-accepted run reaches exactly one terminal state;
  no phantom runs from duplicate fires/webhooks" (`:48`). Every run here reaches
  exactly one terminal state — including the synthetic ones, which are finished
  through `POST /agents/{id}/runs/{runId}/finish`. **State that I1 held, with the
  census**, and note that a *deleted* run is not a run that failed to reach a
  terminal state; it reached one and was then removed.
- **NOT I2.** I2 is "step side effects execute at most once" (`:49`). Nothing is
  executed twice. Per the W2 correction a zero-vs-once shape does not violate
  at-most-once, and a *never-collected* object is not a side effect executed
  twice either.
- **NOT I3.** No mutex, semaphore or concurrency slot in any fixture used. The
  retention sweeper's advisory lock (`runRetentionLockKey = 0x7272746E`) is not
  I3's subject — I3 is about *workload* locks verified via `mutex_holders` /
  `named_lock_slots` — and in any case the manual DELETE route does not take it.
- **NOT I5.** Nothing is injected and nothing has to recover. **Do not stretch
  "bounded recovery" over "the object is never collected"** — there is no fault
  to return to steady state from.
- **NOT I6.** No run is terminalised out from under a running agent and no agent
  is fenced. (The scenario *describes* that shape as the realistic production
  route into its own precondition — see §Instrument — but does not produce it.)

### The contract question, decided in advance and argued in the entry

**The guard's comment is an inline comment on an exported handler, not an
unexported helper's doc comment.** The campaign's classification rule
disqualifies only the latter (`FINDINGS.md:479`, the W1 clarification: the
"documented contract" limb "requires a *published* promise — an exported API
field, a schema column, or a statement in `docs/`" and "does **not** include an
unexported helper's own doc comment"). `handleArtifactUpload` is exported and
routed; `deleteRunEverywhere` is **unexported**.

**Decide it this way and justify it in the entry:** an inline comment inside a
function body is not a *published* promise either — it is not an API field, not
a schema column and not a statement in `docs/`. The exported-vs-unexported
distinction the W1 rule draws is about *doc comments* on identifiers a caller can
see, and neither `api_artifacts.go:57-61` nor `run_retention.go:111-130` is one.
**So the comments are evidence of intent, not contract**, and the entry rests on
`docs/*.md` plus the invariants. Their value is different and greater: they show
the authors knew about this exact hazard and closed half of it.

### Contract limb — survey `docs/*.md` IN FULL and print the hit count

W3-3 lost a contract violation to a `head`-truncated survey (plan `:88`) and the
method rule that came out of it is binding. Run these, capture the output
**untruncated**, and print `| wc -l` next to each:

    grep -rn -iE "retention|delete|deletion|purge|orphan|reclaim|garbage" docs/*.md
    grep -rn -iE "artifact" docs/*.md
    grep -rn "UNIFIED_RUN_RETENTION_DAYS|run-retention-days" docs/*.md
    grep -rn -iE "object store|object storage|S3|garage|bucket|storage" docs/*.md

**Read the surrounding section before citing anything** (the W2-7 lesson), and
**check whether this branch has already ruled on the passage** (the Task-3
lesson: its 413 finding was reclassified for reading
`docs/high-availability.md:253-254` broadly when `FINDINGS.md:1563` had already
read it narrowly). Before filing:

    grep -rn "<the doc file>:<the line>" test/edgecase/

and record the result. If the surveys turn up nothing covering "what a run
deletion promises about its objects", write **"silent, not contradicted"** and
rest on the invariants and on the code's own stated intent.

---

## Verified mechanism — read this before designing anything

Every row re-read at this branch's HEAD; the `file:line` is the claim.

| # | Fact | Site |
|---|---|---|
| 1 | `deleteRunEverywhere` has **five** object-side steps and the DB delete is **fourth**: (1) record-based archive delete, (2) deterministic archive-key delete, (3) `List("artifacts/"+runID+"/")` then delete each, (4) `st.DeleteRun`, (5) the deterministic archive key **again**, best-effort | `internal/controller/run_retention.go:131-173`; steps at `:133-141`, `:145-147`, `:148-156`, `:158-160`, `:167-170` |
| 2 | **Step 5 exists to catch a write that lands during the deletion window** — the comment names it "race (b)" and says "We close (b) here … once more after DeleteRun succeeds, to catch an archiver that wrote the object during our own deletion window" | `run_retention.go:120-130`, `:162-166` |
| 3 | **There is no equivalent re-sweep of `artifacts/<runID>/`.** Step 3 runs once, before `DeleteRun`, and `obj.List` snapshots the key set before the loop | `run_retention.go:148-156` |
| 4 | The manual `DELETE /api/v1/runs/{id}` calls the **identical** helper synchronously, after a terminal check (409 otherwise), behind `requireMinRole("developer")` | `internal/controller/server.go:375`; `api_runs.go:427-432`, `:433` |
| 5 | The sweeper's candidate set is driven off **`runs` rows** — `SELECT id FROM runs WHERE status IN (…) AND updated_at < $1 …` — so a deleted run can never be a candidate again | `internal/store/postgres.go:1494-1502` |
| 6 | **There is no orphan-object reconciler anywhere in the tree.** Every object-store `List` call site is either a per-run prefix or `caches/`: `cache.go:182` (`caches/`), `cache.go:223` (`jobPrefix`), `api_artifacts.go:127` (`artifacts/<runID>/`), `log_trim.go:76` (`runLogArchiveKey(id)`), `run_retention.go:148` (`artifacts/<runID>/`). **Five, and that is all of them** | `grep -rn "\.List(ctx" --include=*.go internal/ cmd/` |
| 7 | **`handleArtifactUpload` checks run existence at `:55` and `Put`s at `:79` with no lock, lease or re-check between**, and its own comment names the hazard: "A late upload for a deleted run would create an orphaned object nothing ever cleans up (deleteRunEverywhere already ran its prefix delete). Terminal-but-existing runs are still accepted: their objects stay referenced and are removed with the run." | `internal/controller/api_artifacts.go:55-67`, comment at `:57-61`, `Put` at `:79` |
| 8 | **The window is as wide as the payload transfer.** `size := r.ContentLength; if size < 0 { size = -1 }` and the body is streamed straight into `Put` (`:75-79`); the agent's own client sets no `Content-Length` at all | `api_artifacts.go:75-79`; `internal/agent/client.go:349-371` (**not** `:352-360`, Correction 1) |
| 9 | **Terminal-but-existing runs still accept uploads** — the guard is called with `rejectTerminal=false`, so the terminal check at `agent_guard.go:129-131` is skipped; ownership (`run.ClaimedBy == agentID`) is still enforced at `:121-127` | `api_artifacts.go:47`; `agent_guard.go:98-132` |
| 10 | **`handleArtifactList` does not check run existence** — no `GetRun`, and the route group carries only `agentOrServerAuth` | `api_artifacts.go:120-147`; `server.go:496-499` |
| 11 | The artifact key is `artifacts/{runID}/{name}.tar.gz`, validated per segment | `internal/artifact/…:115-123` (`ArtifactKey`) |
| 12 | The retention sweeper's interval is **hardcoded** `time.Hour` at the call site and `time.NewTicker` does not fire immediately, so a fresh controller's first sweep is at **t+1h**; `UNIFIED_RUN_RETENTION_DAYS` unset ⇒ **0** ⇒ the goroutine returns immediately | `cmd/controller/main.go:426`, `:47-58`; `run_retention.go:26-33` |
| 13 | Artifacts have **no DB record of any kind** — no table, no `CreateArtifact` — so there is nothing to reconcile a bucket against even in principle | W3-2 §Verified mechanism 16; `api_artifacts.go:120-147` |

**The single sentence the scenario tests:** the deletion path was written by
someone who knew a late write could orphan an object, added a post-`DeleteRun`
re-sweep to catch one, and applied it to only one of the two key spaces it
deletes — and nothing anywhere will ever find what escapes through the other.

---

## Stack

```bash
cd test/ha
export MSYS_NO_PATHCONV=1          # Git Bash rewrites container paths (W2-5)
export COMPOSE_FILES="-f docker-compose.ha.yaml \
  -f ../edgecase/compose/bigbody.override.yaml"
docker compose $COMPOSE_FILES up -d --build
```

- **`bigbody.override.yaml` is REQUIRED, for two independent reasons.**
  `client_max_body_size 0` (without it `test/ha/nginx.conf` inherits nginx's
  1 MiB default and the upload dies at the LB with 413 before a controller sees
  a byte — Task 3 measured exactly that, `FINDINGS.md:1655`); and
  `proxy_request_buffering off`, **which is what makes the window exist at all**
  (§Instrument).
- **The three nginx overlays do NOT stack** (`bigbody`, `logfault`, `steplink`
  all replace `/etc/nginx/nginx.conf`; the last listed silently wins). Only
  `bigbody` is needed.
- **`s3proxy` is deliberately absent** — see §Instrument for why the interposer
  is the wrong tool for this particular race.
- **`down -v`, not `down`, at the end and before any re-run.** `ha-garagedata` is
  persistent, and this scenario's entire deliverable is *an object that is still
  there*; a stale one from a previous attempt would be indistinguishable from a
  fresh hit.

Throughout: `psql` means
`docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "<sql>"`,
`API` means `curl -sS -H "Authorization: Bearer ha-admin-token"` against
`http://localhost:18080`, and `mc` means
`docker compose $COMPOSE_FILES exec -T mc mc` — which talks to `garage:3900`
**directly**, bypassing the LB and any interposer, and is therefore the
out-of-band surface for every object claim in this runbook.

**Workloads.** `test/edgecase/workloads/w36-probe.payload.json` (job
`edge-w36-probe`, `agentSelector: [kind:w36probe]`) — a job **no real agent can
claim**, so the synthetic identity is the only possible claimant and agent1 /
agent2 are never involved. `tick.payload.json` (`edge-tick`) for the baseline
gate. `artifact-large.payload.json` is **not** used: this scenario drives the
upload itself.

---

## BASELINE GATE — do not proceed past a failing check

Write every gate output to `$SCRATCH/gate.txt` unless a per-check file is named.

```bash
SCRATCH="<scratchpad>/w3-6" ; mkdir -p "$SCRATCH"
```

1. **G0 — worktree.** `git rev-parse --show-toplevel` is `.../wt-edge-spec`,
   branch `plan/edge-case-w3`. `docker compose ls` shows the developer stack
   (project `unified-cd`) present and untouched. **STOP** if the toplevel is the
   main checkout.
2. **G1 — Garage is there and the controllers are using it.** All three
   controllers log `using S3-compatible object store` with `endpoint` and
   `bucket`; **none** logs `no object store configured`. **STOP on any mismatch**
   — against a store-less rig every step below is a silent no-op.
   → `$SCRATCH/gate-g1-objstore.txt`.
3. **G2 — stack health.** Three controllers up, `API /readyz` → 200,
   `GET /api/v1/agents` lists agent1 and agent2. (They take no part in the
   scenario; the check exists so a broken rig is not mistaken for a finding.)
4. **G3 — clean slate, object side included.**
   `SELECT count(*) FROM runs WHERE status IN ('Pending','Queued','Running');` → 0;
   `mc ls --recursive garage/unified-cd-logs/` → **empty**. **If it is not empty
   the previous teardown was not `-v`** and every orphan claim below is void.
   → `$SCRATCH/gate-g3-clean.txt`.
5. **G4 — `bigbody` took, both halves.**
   `docker compose $COMPOSE_FILES exec -T nginx grep -c "proxy_request_buffering off" /etc/nginx/nginx.conf`
   is non-zero, likewise `client_max_body_size 0`, and `nginx -t` is ok. **STOP
   otherwise** — with buffering on, Part A measures nginx's spool time and not
   the controller's `Put`, and the race cannot be hit at all.
   → `$SCRATCH/gate-g4-bigbody.txt`.
6. **G5 — retention is OFF, which is a precondition and not an accident.** All
   three controllers must log `run retention disabled (keep forever)`
   (`cmd/controller/main.go:424`). **STOP if any logs `run retention enabled`** —
   a live sweeper would delete runs out from under the measurements, and Part D
   depends on knowing this is off. → `$SCRATCH/gate-g5-retention.txt`.
7. **G6 — the synthetic agent works end to end, before anything is raced.**
   Enroll, exchange, claim, finish, upload a **small** artifact to the finished
   run, and confirm `GET /api/v1/runs/{id}/artifacts` lists it and `mc` shows the
   object. **STOP if the upload to a terminal run is refused** — fact 9 would be
   wrong, and that refutation is itself the finding (report it and stop).
   → `$SCRATCH/gate-g6-synthagent.txt`.
8. **G7 — the archiver's positive control**, needed only by Part C: trigger
   `edge-tick`, wait for `Succeeded`, confirm a `run_log_archives` row and the
   object `runs/<id>/logs.ndjson` within ~60 s. → `$SCRATCH/gate-g7-archive.txt`.
9. **G8 — API 500s.** The API on this rig has been intermittently returning 500s.
   Record, for every gate and every request, how many attempts it took. A 500 on
   a trigger is **not** a finding of this scenario; a 500 mid-measurement
   invalidates that attempt, which is discarded and re-run, not reasoned around.

---

## Part A — the TOCTOU, on the production path

**Deliverable:** an object under `artifacts/<runID>/` whose `runs` row was
deleted while the upload that wrote it was in flight, with a timeline that shows
the DELETE landing **after** `handleArtifactUpload`'s `GetRun` and **before** its
`Put` completed, each instant from a named capture.

**A1 — build the synthetic agent.** Record every response.
→ `$SCRATCH/partA-synthagent.txt`.

1. `API -X POST /api/v1/agent-enrollments -d '{"agentId":"w36-probe","labels":["kind:w36probe"],"expiresIn":"2h"}'`
   → 201, capture `.token` (`internal/controller/api_agent_enrollment.go:42-88`).
2. `curl -X POST -H "Authorization: Bearer <enrollment token>" /api/v1/agents/enroll`
   → 200, capture `.accessToken` (`uca_…`, TTL **1 h**,
   `api_agent_enrollment.go:21`, `:260-305`). **Note the expiry in the capture**;
   if the scenario runs long, re-enroll rather than refreshing, so nothing
   rotates under a live credential.
3. `API -X POST /api/v1/jobs` with `w36-probe.payload.json`, then
   `API -X POST /api/v1/runs -d '{"jobName":"edge-w36-probe"}'` → run id.
4. `curl -H "Authorization: Bearer <access token>" -X POST "/api/v1/agents/w36-probe/claim?timeout=10s"`
   → the run, now `Running`, `claimed_by = w36-probe`
   (`internal/controller/api_agent.go:127-175`; the claim upserts the agent
   registration itself, `:150`, so no separate `/register` is needed).
5. `curl -H "Authorization: Bearer <access token>" -X POST /api/v1/agents/w36-probe/runs/<id>/finish -d '{"status":"Succeeded"}'`
   → 204. **Confirm `GET /api/v1/runs/<id>` is `Succeeded`** and
   `SELECT claimed_by FROM runs WHERE id=…` is `w36-probe`. Both are
   preconditions of the race: terminal makes DELETE legal, ownership makes the
   upload legal.

**A2 — measure the width before racing it.** Upload `<N>` MiB at
`--limit-rate <R>` to a *different* terminal synthetic run and record: the wall
time of the `PUT`, and the controller access-log `duration_ms` for it. **The
controller's `duration_ms` is the window** — it starts before `GetRun` and ends
after `Put`. Target **≥ 20 s**, adjust `N`/`R`, and record the pair actually
used. → `$SCRATCH/partA-width.txt`.

**A3 — the race.** On a fresh synthetic terminal run:

1. Start the slow upload in the background, stamping the start instant:
   `date -u +%FT%T.%3NZ` before `curl -T -` and after it returns, both into the
   capture.
2. **Confirm the request is actually in the controller, not still in the client.**
   Poll `mc ls --recursive garage/unified-cd-logs/` for a multipart upload
   (`mc ls --incomplete`) **or** the controller access log for an in-flight
   request; do **not** fire on a fixed sleep alone. Record which signal fired and
   at what instant. (A fixed sleep with the signal recorded *alongside* it is
   acceptable; a fixed sleep with no signal at all is not — the whole claim is
   about ordering.)
3. Fire `API -X DELETE /api/v1/runs/<id>`, stamping the instant before and
   after. Expect **204**. A **409** means the run was not terminal; a **404**
   means it was already gone; a **500** is a G8 discard.
4. Let the upload finish. Record its HTTP status.
5. → `$SCRATCH/partA-race.txt`.

**A4 — the four claims, each from that capture.**
1. `SELECT count(*) FROM runs WHERE id='<id>'` → **0**.
2. `mc ls --recursive garage/unified-cd-logs/artifacts/<id>/` → **the object**,
   with its size.
3. The DELETE's window lies inside the upload's window — quote all four
   instants.
4. The upload's own status code: **the interesting case is 204.** If the `Put`
   returns 204 *after* the run row is gone, the client believes it succeeded.
   Record what it actually was.

**A5 — attempt discipline.** **Cap: 6 attempts.** Record the count reached
**either way**. If the race is not hit, file the code-read argument (facts 7 + 8)
with an explicit **"not reproduced live"** label — the W2-3 Arm D precedent,
accepted at 0/10 — and **do not write that the window is unreachable**.

**Falsification.** If the DELETE returns 204 and the object is nonetheless
**absent** afterwards, something re-sweeps the artifact prefix and fact 3 is
wrong: that is the finding, and it must be reported as a correction to the plan
rather than buried. If the upload returns a 404, the DELETE landed before
`GetRun` and the attempt is void, not a result — retry with a later fire.

---

## Part B — prove what can and cannot reach the orphan

**Deliverable:** a per-surface table for the Part A object — which surfaces can
see it, which cannot, and the argument (with its code reads labelled) that no
automated path will ever collect it.

**B1 — the negative surfaces.** For the deleted run id: `runs` rows **0**;
`logs` rows **0** (FK cascade); `run_log_archives` rows **0**;
`GET /api/v1/runs/<id>` → **404**. → `$SCRATCH/partB-surfaces.txt`.

**B2 — the surfaces that may NOT be negative, checked honestly rather than
assumed.** W3-2 found `GET /api/v1/runs/{id}/logs` answering **200** for a
deleted run, so a 404 here is a hypothesis, not a given (fact 10 predicts a
**200 with contents**):
- `GET /api/v1/runs/<id>/artifacts` — record the **status and the body**.
- `GET /api/v1/runs/<id>/artifacts/<name>` — record status and byte count.
- `GET /api/v1/runs/<id>/logs`, `/logs/stats`, `/steps` — record each.

**If the list route returns the orphan, say so plainly and do not soften it:**
the object is not merely uncollected garbage, it is **still served by the API
after its run was deleted**, and that is a materially different fact from
"wasted storage". It is also what makes the I7 question in §Invariants real.

**B3 — the reachability argument, with every limb labelled.**
- **Live:** the object is present out of band (`mc`), and no run row exists.
- **Code-read:** `ListExpiredRuns` selects `FROM runs` (fact 5), so the sweeper
  can never nominate this id again — *and the sweeper is off on this rig anyway*
  (G5), which must be stated so nobody reads "it was never collected" as a
  measured negative over a live sweeper.
- **Code-read, and this is the strong limb because it is exhaustive:** enumerate
  **every** object-store `List` call site in the tree (fact 6) and show that none
  of the five enumerates anything but a per-run prefix or `caches/`. **Paste the
  grep and its full output** — an exhaustive enumeration is only worth anything
  if the reader can see it was exhaustive.
- **Do not write "never" for a window this runbook ended itself.** The rig is
  torn down with `-v`; the correct statement is that no code path exists that
  *would* have collected it, not that an infinite wait was observed.

---

## Part C — the asymmetry, produced live on one run in one window

**This is the part that makes the scenario a finding rather than a race report.**
Part A shows an artifact escaping. Part C shows a **log archive planted in the
same deletion window being caught**, on the same run, by the same DELETE — so the
asymmetry is a measurement, not an argument.

**How the window is opened.** `deleteRunEverywhere`'s step 3 lists
`artifacts/<runID>/` **once** and then deletes each key **serially**
(`run_retention.go:148-156`). So the window's width is a knob: **pad the prefix
with many small objects out of band**, and the DELETE spends measurable time
inside step 3, before `DeleteRun`. Planting during that window puts an object
after step 2 and before step 5 — which is exactly where race (b) lives.

- **C1 — calibrate.** On a terminal synthetic run, plant `<M>` tiny objects under
  `artifacts/<id>/pad-*.tar.gz` with `mc cp --recursive`, then time
  `API -X DELETE /api/v1/runs/<id>`. Raise `<M>` until the DELETE takes **≥ 10 s**.
  Record `<M>` and the duration. → `$SCRATCH/partC-calibrate.txt`.
- **C2 — the two-plant experiment.** On a fresh terminal synthetic run padded to
  `<M>`:
  1. Fire the DELETE in the background, stamping the instant.
  2. After a delay well inside the measured window, plant **both** out of band
     with `mc`, stamping each: `runs/<id>/logs.ndjson` (the log-archive key —
     `runLogArchiveKey`, the key step 5 re-sweeps) and
     `artifacts/<id>/late-blob.tar.gz` (a key step 3 has already snapshotted
     past).
  3. **Immediately after the plant and BEFORE the DELETE returns, `mc ls
     runs/<id>/` and `mc ls artifacts/<id>/late-blob.tar.gz`, and record both.**
     This is the step the 2026-07-31 run omitted, and its absence is the one
     inference that entry has to declare: without it, a post-DELETE `[count=0]`
     on the log-archive key is consistent with "step 5 deleted it" *and* with
     "the plant never wrote it" — and note 1's silently-empty upload is proof
     that this rig can produce the second. One `mc ls` closes it.
  4. Wait for the DELETE to return. Record its status.
  5. `mc ls --recursive garage/unified-cd-logs/` and report **which of the two
     survived**.
  → `$SCRATCH/partC-asymmetry.txt`.
- **C3 — the expected result, stated in advance so a surprise is legible.** The
  log archive is **deleted** by step 5 (`run_retention.go:167-170`); the late
  artifact **survives**, because step 3 already ran and nothing re-lists the
  prefix. **If both survive**, step 5 did not fire in this window — check the
  plant instant against the DELETE's return before concluding anything, and if
  the plant landed after step 5 rather than before it, that is a void attempt and
  not a result. **If both are deleted**, fact 3 is wrong and that is the finding.
- **C4 — attempt discipline.** **Cap: 6 attempts.** Report the count either way.
- **C5 — label the instrument.** `mc` planting is **not** a production path;
  what it stands in for is the archiver's `Put` (which W3-2 produced live, in the
  same window shape, at `FINDINGS.md:1760`) and a late `handleArtifactUpload`
  `Put` (which Part A produced live). **C2 is a controlled demonstration that
  the two key spaces are treated differently by one execution of one function;
  it is not a claim that objects appear by magic.** Say exactly that.

---

## Part D — the sweeper path

**SKIPPED, deliberately, and the reason is recorded rather than omitted.**

The sweeper's interval is **hardcoded `time.Hour`** at the call site
(`cmd/controller/main.go:426`) with no flag and no env override, and
`time.NewTicker` does not fire immediately — so even with
`UNIFIED_RUN_RETENTION_DAYS=1` and a backdated `updated_at`, the first tick is at
**t+1h**, and a controller restart re-arms it at t+1h rather than firing at once
(fact 12). That is an hour of wall clock for a path that is **the identical
function**: `DELETE /api/v1/runs/{id}` calls `deleteRunEverywhere` synchronously
(fact 4), so Parts A and C exercise every line the sweeper would.

**What is therefore NOT established by this scenario, stated explicitly:** that
the sweeper reaches `deleteRunEverywhere` in a live deployment; that
`ListExpiredRuns`' predicate selects what it appears to; and that the
leader-election / backoff wrapper around it behaves as read. All three are
**code-read only** here. The entry must say so rather than implying the sweeper
was exercised.

---

## Teardown

```bash
# 1. show the stack healthy and enumerate what is left in the bucket
docker compose $COMPOSE_FILES ps
docker compose $COMPOSE_FILES exec -T mc mc ls --recursive garage/unified-cd-logs/
# 2. down, WITH -v
docker compose $COMPOSE_FILES down -v
```

- **Cancel every surviving run before teardown** and confirm zero non-terminal
  runs in a census. The synthetic identity's runs count.
- **Revoke the synthetic agent's credential** (`DELETE /api/v1/agents/w36-probe`)
  or record that the `down -v` disposes of it. Either is fine; asserting neither
  is not.
- **`down -v`, not `down`.** `ha-garagedata` is persistent and this scenario
  deliberately leaves orphans in it.
- **Kill every background sampler and *capture* that, do not assert it.** Part A
  runs a background `curl` and Part C a background DELETE. Keep PIDs in
  `$SCRATCH/samplers.pid`, `kill` them explicitly, then show `jobs` empty and
  `ps -W | grep -iE "curl|psql|mc"` matching nothing — **and check inside the
  containers too**, because a `docker compose exec` sampler outlives the shell
  that launched it and appears in neither (the W3-4 lesson, which caught a live
  one). → `$SCRATCH/teardown.txt`.
- Copy `$SCRATCH` into the campaign evidence root at the wave checkpoint
  (`test/edgecase/README.md` § "Raw evidence").

---

## Recording rules

- **An orphaned object no code path can ever reach ⇒ judge against I4's exact
  wording, quoted**, and against W3-2's binding ruling that an object without a
  record is an orphan and not an unreadable archive. If no I4 clause is
  contradicted, **say so** and rest the entry on the contract survey and on the
  asymmetry.
- **The TOCTOU itself ⇒ judge on whether the guard's comment is a contract.**
  The decision is made in §Invariants (it is not: an inline comment is not a
  published promise, even on an exported handler) — **restate the argument in the
  entry rather than the conclusion alone**, because the campaign's rule names
  only the unexported-doc-comment case and this is a deliberate extension of it.
- **Cross-reference W3-2's orphan, do not double-file it.** W3-2's entry
  (`FINDINGS.md:1760`) already states that W3-6 owns the class and that the two
  "should be one finding about 'no orphan-object reconciler exists' with two
  producers, not two findings". **Honour that: one entry, this one, naming both
  producers.**
- **The API-serves-a-deleted-run's-artifacts result ⇒ decide I7 explicitly** and
  reconcile with W3-2's `GET /runs/{id}/logs` → 200 observation, which was
  recorded without an I7 filing. Departing from that call requires a stated
  reason.
- **Observation entries say "observation" in the title** and repeat it in the
  Severity line as `minor (observation)` (`FINDINGS.md:481`).
- **Every number cites a `$SCRATCH` filename whose time window covers it.**
  Derived figures say "derived"; code-read figures say "code-read"; uncaptured
  live observations say `(observed live, raw output not captured to scratchpad)`.
  **Do not present verbatim-looking log text that was not captured**, and do not
  call an attribution a "bracket" unless a capture covers that specific request.
- **Report every capped arm's attempt count either way**, with the cap stated.
- **Label the synthetic agent and the `mc` plants as instruments every time they
  carry a claim**, not once in a preamble.

---

## Execution notes — 2026-07-31 run (read before re-running)

Executed on branch `plan/edge-case-w3`, **`04:36:29Z – 04:57:27Z`**, on the single
stack §Stack specifies (`test/ha` + `bigbody`, no interposer), torn down with
`down -v` (`w3-6/teardown.txt`). **Three `FINDINGS` entries: 1 violation (minor,
documented contract), 1 observation (minor), and — added during review, from a
code read rather than from this run — 1 further violation (major, documented
contract) that was NOT executed**, the job-delete orphan producer of note 8. No
branch-internal asset bug.
The developer stack (`docker compose ls`, project `unified-cd`) was running and
untouched at both ends (`w3-6/gate.txt`, `w3-6/teardown.txt`).

**Six runs, all reaching exactly one terminal state (`Succeeded`)**; three were
then deleted by the scenario. Enumerable from the audit log's nine
`run.trigger` / `run.delete` rows over six distinct ids (`w3-6/census.txt`) --
**count from there, not from what survives in `runs`**, because half the runs of
this scenario are deliberately destroyed by it.

| Part | Arm | Result |
|---|---|---|
| G6 | none | a `PUT` to a **terminal** run is accepted, and this row is **two uploads, not one**: the first (`gate-blob`) returned 204 while writing **0 B** (the MSYS trap, note 1) and is what the `GET /runs/{id}/artifacts` → `[{"name":"gate-blob"}]` listing was taken against; the **4.0 KiB** object is the post-fix re-upload (`gate-blob2`). Either way fact 9 is confirmed live and the scenario is constructible |
| G7 | none | archival works: `a50b9031` archived at **3036 B / 30 lines** (`w3-6/census.txt`) |
| A2 | none | 32 MiB at `--limit-rate 1M`, chunked → controller `duration_ms=32374`. **The window is the payload transfer** |
| A3 | DELETE fired on an `mc ls --incomplete` signal | **hit on attempt 1 of a cap of 6.** DELETE **204**, measured by two different instruments and kept apart: **client-side, `time_total=0.013043` (13 ms) from `curl`** (`w3-6/partA-race.txt`, bracketed `04:45:28.842Z` → `04:45:28.918Z`); **server-side, `"duration_ms":10` stamped by `accessLogMiddleware` on completion at `04:45:28.89119652Z`** (`w3-6/partA-bracket.txt`). ~~"204 in 13 ms at `04:45:28.891Z`"~~ fused the two — the 13 ms is curl's total and the instant is the controller's completion stamp for a 10 ms handler; the 3 ms difference is the client leg, not error. `PUT` handler started `04:45:28.2998Z` (derived) and finished `04:46:00.586Z` with **204**. Run row gone, **32 MiB object left** |
| B | the orphan's surfaces | 2 of 7 say 404; 5 answer 200; **2 of those serve its data** (list + a 33554432-byte download), for a server token **and** for an agent credential |
| C1 | 400 pads, no race | DELETE 0.493 s, **all 400 deleted**, prefix empty — the positive control |
| C2 | 10,000 pads, two plants ~3 s into a 12.739 s DELETE | **the asymmetry, live**: the late `runs/<id>/logs.ndjson` was **deleted** by step 5, the late `artifacts/<id>/late-blob.tar.gz` **survived** |
| D | — | **skipped**, see §Part D; the sweeper was never exercised and every claim about it is code-read |

**Nine things a re-run should know.**

1. **The MSYS path trap cost one silent zero-byte capture and would have cost a
   finding.** With `MSYS_NO_PATHCONV=1` exported (needed for `docker compose
   exec` container paths), mingw `curl` cannot open `@/c/Users/...` — it uploads
   an **empty body and still returns 204**. G6's first upload produced a **0 B**
   object and looked like a success (`w3-6/gate-g6-synthagent.txt`). Use a
   Windows-form path (`C:/Users/...`) for every `curl` file argument, and
   **check `%{size_upload}` against the object's size**, not the status code.
2. **The synthetic agent buys a WIDE window, not the only window — and the
   original claim here that it was "the only way in" was false.** The route does
   need a terminal run (409 otherwise), but `handleCancelRun` marks the row
   terminal synchronously (`api_runs.go:374`) while the agent only notices on a
   5 s poll (`orchestrator.go:37`, `:137-146`), so an ordinary agent-driven run
   is terminal-while-still-uploading for up to 5 s and the DELETE is legal for
   all of it. **That window was never attempted here.** The synthetic agent was
   chosen because it makes the window as wide as the payload and repeatable;
   building it costs five API calls and no SQL (`w3-6/synth.sh`).
3. **`--limit-rate` + `proxy_request_buffering off` is a better width knob than
   anything in `inject.sh` for this race**, and it is fully deterministic: the
   controller's `duration_ms` came out at 32374 for a 32 s client-side upload.
   `bigbody` is required for both of its settings, not just the size cap.
4. **Fire the DELETE on `mc ls --incomplete`, not on a sleep.** An incomplete
   multipart upload under `artifacts/<runID>/` means the controller is *inside*
   `objStore.Put` right now. It appeared **577 ms** after the upload started.
5. **`accessLogMiddleware` stamps on completion**, so a handler's start instant
   is `time - duration_ms`. That is what brackets the race, and it cross-checks
   against the client's own stamp to within ~55 ms. Say "derived" when you use it.
6. **Padding the artifact prefix is a clean, linear window knob inside
   `deleteRunEverywhere`.** 400 objects → 0.493 s, 10,000 → 12.739 s
   (~1.2 ms per object; the deletes are serial, `run_retention.go:152-156`).
   Generating the pads inside the `mc` container and `mc cp --recursive`-ing
   them takes ~7 s for 10,000.
7. **The whole event is silent.** Zero controller `WARN` and zero `ERROR` lines
   **across the two windows in which an orphan was created** — the captures are
   headed "in the window" (Part A) and "in the last 6 minutes" (Part C) and
   together do **not** span `04:36:29Z – 04:57:27Z`, so do not restate this as a
   session-wide negative; the load-bearing claim is that nothing was recorded at
   the moments the orphans were created, and that is what the captures cover.
   The artifact `PUT` has **no audit row at all** (the route lives outside the
   `/api/v1` audit group); the audit log's only word on the raced run is
   `run.delete` → 204.
8. **Do not re-file W3-2's orphan — and know that the class has THREE
   producers, not two.** `FINDINGS.md:1760` is the sibling on the log-archive
   key space and its own text hands this class to W3-6; the W3-6 violation
   entry names both **race-window** producers and scopes its completeness claim
   to exactly those. The third was found in this scenario's own docs survey and
   is filed as a separate, **unexecuted** entry (last in `FINDINGS.md`):
   `DELETE /api/v1/jobs/{name}` calls only `store.DeleteJob`
   (`api_jobs.go:220-231` → `postgres.go:2168-2171`), never reaches
   `deleteRunEverywhere`, and cascades the `runs` rows away — so every artifact
   **and** every log archive of every run of a deleted job survives with no
   window at all. **A re-run of this scenario should measure it**: trigger,
   archive, upload, `DELETE /api/v1/jobs/{name}`, then `mc ls --recursive` both
   prefixes. It is minutes of work on this rig.
9. **The rig's intermittent-500 allowance (G8) never had to be spent** — no
   step of this scenario was retried for a 500. **That is an absence of trouble,
   not a measurement: no capture counts 500s across the session, so the earlier
   "zero API 500s across the whole session" claim is withdrawn** and appears in
   no `FINDINGS` entry. A re-run should neither assume the rig is clean nor cite
   this note as evidence that it was.

**Sampler hygiene was captured, not asserted.** The two background jobs (Part
A's slow upload, Part C's DELETE) were `wait`ed on in their own drivers; both
PIDs were recorded in `$SCRATCH/samplers.pid` and re-checked at teardown, `jobs`
was empty, a host `ps -W` for `curl|psql|mc` matched nothing, and — per the W3-4
lesson that a `docker compose exec` sampler outlives its shell — an in-container
`ps` on `postgres` and `nginx` showed nothing left. **The `mc` image has no `ps`**,
which the capture records rather than hides (`w3-6/teardown.txt`).

**No Postgres statement logging was armed**, so there was nothing to revert.
Said explicitly because the wave budgets for it elsewhere.

**Two runbook steps were not followed as written, and both are recorded rather
than quietly dropped.** (i) Part A's `$SCRATCH/.delbody.N` capture of the
DELETE's response body was never written, for the same MSYS path reason as
note 1 — the DELETE returned 204 with an empty body, so nothing was lost, but
the file named in the driver does not exist. (ii) Gate G7's capture
(`gate-g7-archive.txt`) holds only the trigger; the archive **confirmation**
(`run_log_archives` row, 3036 B / 30 lines, object present) is in
`w3-6/census.txt`, taken at the end of the session rather than within 60 s of
the run.
