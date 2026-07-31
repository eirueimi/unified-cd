# W3-2 — S3 outage during log archival and artifact upload

> **CORRECTED AFTER EXECUTION — READ THIS BEFORE ANYTHING ELSE.**
> **Every part produced its deliverable and no invariant attribution below is
> withdrawn.** What execution changed is four *procedural* claims, one of which
> made a step as written impossible and one of which invalidated two of this
> runbook's own captures. Superseded text is kept in place and struck through,
> per house style, because the reason it was wrong is the deliverable.
>
> 1. **THIS RUNBOOK SHIPPED A WRONG CONSTANT AND IT VOIDED TWO CAPTURES.**
>    §"Verified mechanism" fact 1 said `logArchiverLockKey = 0x6C6F6761`
>    ~~"(= **1819242081** decimal, which is what `pg_locks.objid` shows)"~~.
>    **`0x6C6F6761` is 1819240289, not 1819242081** (`printf '%d' 0x6C6F6761`).
>    Gate G5 and the first Part-A lock census were both run with the wrong
>    literal, both returned 0 granted samples out of 60 and 75, and both were
>    about to be written up as "the lock is never held" — the exact opposite of
>    the truth. `$SCRATCH/gate-g5-leader.txt` and `$SCRATCH/partA-lockhold.txt`
>    are **VOID** and are kept only as the counter-example. Every lock number in
>    the findings comes from `partA-lockhold-1s.txt` / `partA2-stall.txt`, which
>    use the correct value. **A capture that returns a clean negative is exactly
>    the shape that hides a typo'd predicate; print the constant you are
>    querying with, in the capture, next to the result.**
> 2. **`inject.sh s3-latency 30` did NOT widen the window — it broke the Put**,
>    which is the opposite of what `test/edgecase/README.md` records as verified
>    (measured there at `s3-latency 3`). The black hole `192.0.2.1:3900` answered
>    **`connect() failed (111: Connection refused)`** instead of hanging, so
>    `proxy_next_upstream timeout` never fired — refused is `error`, not
>    `timeout` — and nginx returned **502 after 21.037 s** instead of falling
>    through to `backup` (`$SCRATCH/partB-arm1.txt`). Part B's lever was replaced
>    with **`pause garage` behind the interposer**, which is a genuine hang
>    (nginx accepts, proxies, and waits on `proxy_read_timeout 300s`) and gives a
>    window bounded only by minio-go's 60 s `ResponseHeaderTimeout`. Both Part B
>    arms then hit on the first attempt. **A re-runner should not assume
>    `s3-latency` is reliable at large values on every host** — see the note
>    added to §Part B.
> 3. **Part A's premise about what bounds an S3 hang was answered from the wrong
>    layer, and the answer is a third-party default.** §Invariants asked "is
>    there any timeout at all on the controller's side?". There is none in
>    unified-cd; the bound is **minio-go's `ResponseHeaderTimeout: time.Minute`**
>    (`minio-go/v7@v7.2.0/transport.go:52`), measured as ~65 s of continuously
>    held archiver lock per attempt.
> 4. **Part C was executed BEFORE Part B**, not in the runbook's order. Part B
>    deletes `runs` rows with raw SQL, which would have polluted Part C's run
>    census; the swap costs nothing and the captures are independent.
>
> **The scenario yields ONE violation (minor, I5) and THREE observations
> (minor).** Do not split Part A's arms into separate findings — `kill-hard` and
> `pause` share one mechanism and differ only in which error text surfaces.

**Wave W3, Task 4. The first scenario in the campaign that can fault object
storage at all**, because Task 3 put Garage into `test/ha` and built the S3
interposer. Everything below is on the **controller side of the socket**.

---

## The boundary, stated before anything else

**This scenario does NOT re-measure agent-side log loss.** W1-6 owns that
(`FINDINGS.md:169`, `:311`, `:378`, `:391`), W1-2 owns the 5 s step-end `Flush`
budget, F9 owns the 1 MiB `p.pending` byte cap, and W3-4 owns the duplicate
surplus. Those four are all failures of the agent→controller hop. **W3-2 is the
controller→object-store hop**: what happens when the *controller's* S3 client
cannot reach the bucket. If a run in this scenario also loses a log tail,
attribute that half to W1-6 and cite it; do not re-derive it.

**And one constraint that governs how evidence is captured at all.** The agent
destroys every controller error message in transit: `Client.do` replaces the
body of **every** response with status >= 400 by the literal string
`"response omitted"` (`internal/agent/client.go:107-108`). So **no agent-side
failure in this scenario is diagnosable from the run's own logs** — the real
message exists only in the controller's `slog`. Capture it container-side, and
capture it **before** any `up -d --force-recreate`, which discards that
container's log history. (Part C adds a second, independent instance of the same
class on a path that does *not* go through `c.do` at all — see §Part C.)

---

## Corrections to inherited facts, established BEFORE execution

Per the W1 carry-forward rule, the plan's "Verified code facts" block is a set of
**claims**. Every row of §"Verified mechanism" below was re-read at this
branch's HEAD. Three citations are off; **no conclusion changes**, and they are
recorded so nobody re-derives them.

- **CORRECTION 1 — the bare artifact-upload error is at `internal/agent/client.go:369`, not `:367-368`.**
  `docs/superpowers/plans/2026-07-30-edge-case-campaign-w3.md:36` cites
  `client.go:367-368` for `return fmt.Errorf("upload artifact http %d", resp.StatusCode)`.
  At HEAD that statement is on **line 369** (`grep -n "upload artifact http"`),
  inside `if resp.StatusCode >= 400 {` at `:368`. The **fact** the plan states —
  that this is a bare `fmt.Errorf` and **not** an `*HTTPError`, so
  `retryUntilSuccess`'s `errors.As` probe would not match it — is correct and is
  re-confirmed here.
- **CORRECTION 2 — the chunked-upload citation is `client.go:349-371`, not `:352-360`.**
  The plan's W3-6 block (`:125`) cites `client.go:352-360` for "the upload is
  chunked with no Content-Length". `func (c *Client) UploadArtifact` starts at
  **`:349`** and returns at **`:371`**; the request is built at `:354` and no
  `Content-Length` is ever set. Fact correct, span wrong.
- **CORRECTION 3 — `runArchiveAsLeader`'s loop line.** The plan asserts (`:34`)
  that `bo := newFailureBackoff(...)` at `archiver.go:28` sits "**before** the
  loop at `:29`". Confirmed exactly: `:28` is the `bo :=` and `:29` is `for {`.
  **This one is CONFIRMED, not corrected** — and Part A is the arm that tests
  what it implies.

---

## Invariants

Quoted verbatim from `docs/superpowers/specs/2026-07-29-edge-case-testing-design.md`.

- **I5 (bounded recovery)** — `:52`:

  > "**Bounded recovery** — after fault injection the system returns to steady
  > state within documented bounds (leader re-election ≤ seconds; stuck-run reap
  > ≤ staleAfter 90s + interval 30s; the bounds in `docs/high-availability.md`
  > are the contract)"

  **This is the scenario's primary invariant.** Three limbs are tested against
  it, and they are not equally strong — say which is which in the entry:
  - *Archival recovery after the store returns* (Part A). The archiver must pick
    the run back up. There is **no documented bound** for archival latency, so
    the measurement is against the code's own 30 s interval plus the backoff
    schedule, not against a published number. Say "no documented bound" out
    loud rather than inventing one.
  - *A hang, not a refusal* (Part A2). If the store accepts the TCP connection
    and never answers, is there any timeout at all on the controller's side?
    **Measure the stall, and measure whether the archiver's advisory lock is
    held for its duration** — a lock held by a stalled leader is a cluster-wide
    stall, not a replica-local one. **Never write "never" or "unbounded" for a
    window this runbook ends itself**; report the window's length and who ended
    it.
  - *Startup* (Part D). A replica that cannot start while the store is down has
    not returned to steady state. Whether it recovers **on its own** once the
    store returns is a property of the restart policy, which this rig sets
    explicitly — read it, do not assume it.
- **I4 (log/artifact integrity)** — `:51`:

  > "**Log/artifact integrity** — a Succeeded run's log line count matches what
  > the workload emitted; no duplicates, no reordering; archives stay readable"

  **Fit is partial and must be scoped honestly, clause by clause**, because the
  natural temptation here is to stretch it:
  - *"a Succeeded run's log line count …"* — Part A asserts the **opposite** of
    a violation: the run is untouched by an archival failure, its `logs` rows are
    all still there, and the count is unchanged. **Recording that I4 HELD on
    Part A is the deliverable**, and it is what makes "the run is unaffected"
    a measurement rather than a restatement of the code.
  - *"archives stay readable"* — this is the clause Part B attacks. An archive
    **record** that points at an object that is not there would break it. The
    code's claimed invariant is that no such state exists (object first, record
    second, compensating delete on record failure). Part B tries to produce it.
  - **The reverse state — an object with no record — does NOT break this
    clause.** It is an orphan, not an unreadable archive. Judge it as an
    orphan-object question and cross-reference W3-6 (below).
  - Part C: an artifact that fails to upload is **not** an integrity violation
    if the run Fails visibly. It becomes one only if the run reports success
    while the artifact is missing. **Check that explicitly; do not assume it.**
- **NOT I1.** I1 is "every API-accepted run reaches exactly one terminal state;
  no phantom runs from duplicate fires/webhooks" (`:48`). Every run here reaches
  exactly one terminal state, including the Part C runs that Fail. Archival is
  strictly post-terminal, so it cannot move a run's state at all. **State that
  I1 held**, with the census.
- **NOT I2.** I2 is "step side effects execute at most once" (`:49`). The
  archiver is idempotent by construction (`CreateLogArchive` is
  `ON CONFLICT (run_id) DO UPDATE`, `internal/store/postgres.go:1523-1525`), so
  a retried archival overwrites rather than duplicates; and the artifact upload
  is executed **once, never retried** (§Verified mechanism 8), which is a
  zero-or-once shape. **Per the W2 correction, a zero-vs-once shape does not
  violate at-most-once** — do not file it here.
- **NOT I3.** No mutex, no semaphore, no concurrency slot in any fixture used.
  **One caveat that looks like I3 and is not:** Part A2 measures a Postgres
  **advisory lock** held across a stalled S3 call. I3's subject is
  "mutex/semaphore/concurrency slots … released when the holder reaches a
  terminal state" — a *workload* lock, verified via `mutex_holders` /
  `named_lock_slots`. The archiver's advisory lock is neither, and the holder
  never reaches a terminal state because it is a live goroutine. **File that
  under I5 (a stall), not I3.**
- **NOT I6.** No zombie: no run is terminalised out from under a running agent,
  and no agent is fenced.
- **NOT I7.** I7 is "run status, approval status, and audit rows never contradict
  each other or reality" (`:54`). Nothing here states an untruth — the Part C run
  really did fail, and the Part A runs really did succeed. **The diagnosability
  gap (an archival failure is invisible on every run-scoped surface) is filed as
  an observation against the *absence* of an I7 hook**, in the W3-3 / W3-4 idiom,
  not as an I7 violation.
- **Contract limb — survey `docs/*.md` IN FULL and print the hit count.** W3-3
  lost a contract violation to a `head`-truncated survey (plan `:88`); the method
  rule that came out of it is binding here. The surveys to run and capture, each
  with `| wc -l` next to the output:

      grep -rn -iE "object store|object storage|S3|garage|minio|artifact" docs/*.md
      grep -rn -iE "archiv|log archive|retention|trim" docs/*.md
      grep -rn -iE "crash|exit|restart|start(s|ing|up)|unavailable|outage|degraded" docs/*.md
      grep -rn "UNIFIED_S3" docs/*.md

  **Two passages are known in advance and both must be read in their surrounding
  section before being cited** (the W2-7 lesson, plus the Task-3 lesson that a
  passage this branch has already ruled on narrowly must not be re-read broadly):
  - `docs/high-availability.md:305-310`, the "S3 / Object store" subsection of
    "External Dependency Redundancy": "**The controller starts without S3, but
    log archival is disabled** (`no object store configured — log archival
    disabled`). S3 is required for HA operation." Read the parenthetical: it
    quotes the **unconfigured** branch's warning (`cmd/controller/main.go:321`).
    So the sentence is about `UNIFIED_S3_*` being **absent**, which is true and
    was measured by W3-4. It says nothing about a **configured-but-unreachable**
    store. **Do not file it as contradicted without arguing that scoping
    explicitly.**
  - `docs/high-availability.md:322-325`, the Vault subsection, three paragraphs
    later in the same section: "**The controller fails closed.** It will not
    start if Vault is unreachable, so a Vault outage during a rollout crash-loops
    pods until Vault returns. This is deliberate…". **This is the comparand that
    makes Part D worth writing up**: the same document documents exactly this
    behaviour for one external dependency and not for the other.
  - `docs/configuration.md:68` marks `UNIFIED_S3_ENDPOINT` **Required: No** with
    "Without S3, log archival and artifacts are disabled" — same unconfigured
    scoping.
  If the surveys turn up nothing that covers the configured-but-unreachable case,
  write **"silent, not contradicted"** and rest on **I5**.

---

## Verified mechanism — read this before designing anything

Every row re-read at this branch's HEAD; the `file:line` is the claim.

| # | Fact | Site |
|---|---|---|
| 1 | The archiver is a 30 s ticker guarded by one advisory lock, `logArchiverLockKey = 0x6C6F6761` (~~= **1819242081** decimal~~ **= 1819240289 decimal** — see correction 1 in the box at the top of this file; that is what `pg_locks.objid` shows, with `classid=0` and `objsubid=1`) | `internal/controller/archiver.go:15`, `:19-23`; started at `cmd/controller/main.go:400` |
| 2 | **The lock is acquired per tick and released per tick** — `AcquireAdvisoryLock` then `defer release()` inside `runArchiveAsLeader`, so leadership can move between ticks and is **not** sticky | `archiver.go:39-52` |
| 3 | Candidate set: terminal status, **no `run_log_archives` row**, not in the excluded set, `ORDER BY updated_at LIMIT $1` (limit **20**) | `internal/store/postgres.go:1458-1467`; limit at `archiver.go:55` |
| 4 | **`bo` is one instance per PROCESS, not per tick** — `bo := newFailureBackoff(time.Minute, time.Hour, 10_000)` at `:28`, the loop at `:29`. Its own doc comment says it is "Leader-local by design: a failover or restart clears it, costing one retry per poison before it is re-excluded" | `archiver.go:25-28`; `internal/controller/failure_backoff.go:9-15` |
| 5 | Backoff schedule: `wait = base << (failures-1)` capped at `max` ⇒ **1 min, 2, 4, 8, 16, 32, 60 (capped)** | `failure_backoff.go:37-64` |
| 6 | **The only trace of a failed archival is one `slog.Error`.** No run field, no log row, no status change, no metric named in the handler | `archiver.go:60-63` |
| 7 | Order is **object first, record second**; on a record failure a best-effort `obj.Delete` compensates and a `slog.Warn` fires if *that* fails | `archiver.go:94-116`, delete at `:111`, warn at `:112-114` |
| 8 | **Artifact upload is ONE SHOT.** `b.UploadArtifact` is called once (`orchestrator.go:815`), not wrapped in `retryUntilSuccess`; the caller likewise (`:401`); `Client.UploadArtifact` has no internal retry | `internal/agent/orchestrator.go:815`, `:401-404`; `internal/agent/client.go:349-371` |
| 9 | A failed upload reports the step **Failed** (`:817-820`) and then `markFailed` fails the **run** (`:403`) | `orchestrator.go:817-820`, `:401-404` |
| 10 | A **configured-but-down** store gives **500** (`objStore.Put` error → `http.Error(..., 500)`); an **unconfigured** store gives **503** "object store not configured" | `internal/controller/api_artifacts.go:79-81` vs `:21-24` |
| 11 | The agent's artifact error is a **bare** `fmt.Errorf("upload artifact http %d", ...)` — not an `*HTTPError`, and it carries **no body at all** | `internal/agent/client.go:368-369` (**not** `:367-368`, Correction 1) |
| 12 | **`UNIFIED_S3_*` is read once at startup and `BucketExists` is eager**; failure is `slog.Error("s3 object store init", ...)` + `os.Exit(1)` | `internal/objectstore/s3.go:41-44`; `cmd/controller/main.go:304-314` |
| 13 | The archiver and the cache cleaner are the **only** two goroutines gated on `obj != nil` | `cmd/controller/main.go:399-402` |
| 14 | **Nothing bounds the archiver's S3 calls.** `RunLogArchiver(ctx, …)` gets the process context; no `WithTimeout` anywhere in `archiver.go`, and `obj.Put`/`obj.Delete` inherit it | `archiver.go:19`, `:80-118` |
| 15 | `CreateLogArchive` is `ON CONFLICT (run_id) DO UPDATE` ⇒ re-archival is idempotent | `internal/store/postgres.go:1519-1527` |
| 16 | **Artifacts have no DB record at all** — `handleArtifactList` answers from `objStore.List(prefix)` and reconstructs names from keys, so the object/record question is *inapplicable* to artifacts | `internal/controller/api_artifacts.go:120-147` |
| 17 | `deleteRunEverywhere` deletes the deterministic log-archive key **twice** — once before `DeleteRun` (`:145-147`) and once after, best-effort with a `slog.Warn` (`:167-170`) — deliberately, to close the archiver race the comment calls "(b)" | `internal/controller/run_retention.go:111-130`, `:145-147`, `:161-170` |

**The single sentence the scenario tests:** an object-store outage is invisible
to every run and to every run-scoped API, is retried by a leader-local backoff
whose scope the plan had to correct, produces exactly one class of log line, and
— on the startup path only — is fatal.

---

## Stack, and why it is TWO phases with a `down -v` between them

`docker compose down -v` between phases is **mandatory, not hygiene**: the
`ha-garagedata` volume is persistent, so a leftover `runs/<id>/logs.ndjson` or
`artifacts/<id>/...` from an earlier phase turns a miss expectation into a hit
(`test/edgecase/README.md` § "The object store, and the S3 interposer").

```bash
cd test/ha
export MSYS_NO_PATHCONV=1          # Git Bash rewrites container paths (W2-5)
```

**Phase 1 — Parts A and D. Plain `test/ha`, NO interposer.**

```bash
export COMPOSE_FILES="-f docker-compose.ha.yaml"
docker compose $COMPOSE_FILES up -d --build
```

Rationale, because the choice is load-bearing and a re-runner will want to
collapse the two phases into one: **the interposer changes what a full outage
looks like.** With `s3proxy` in the path, a paused Garage is an nginx
`proxy_read_timeout 300s` (`nginx-s3.conf:166`), so the controller's stall is
bounded by *nginx's* configuration rather than by unified-cd's. Part A2's whole
question is whether unified-cd bounds it at all, so Part A must talk to Garage
directly. Part D likewise: `s3proxy.override.yaml` adds
`depends_on: s3proxy: {condition: service_healthy}`, which would change the
startup failure's shape.

**Phase 2 — Parts B and C. `test/ha` + `s3proxy` + `bigbody`.**

```bash
docker compose $COMPOSE_FILES down -v            # MANDATORY between phases
export COMPOSE_FILES="-f docker-compose.ha.yaml \
  -f ../edgecase/compose/s3proxy.override.yaml \
  -f ../edgecase/compose/bigbody.override.yaml"
docker compose $COMPOSE_FILES up -d --build
```

- **`bigbody.override.yaml` is REQUIRED for Part C.** `test/ha/nginx.conf`
  inherits nginx's default `client_max_body_size 1m`, so a 64 MiB artifact dies
  at the LB with **413** and the run Fails before a controller sees a byte —
  already measured and filed by Task 3. Without it Part C would measure the LB,
  not the object store.
- **The three nginx overlays do NOT stack** (`bigbody`, `logfault`, `steplink`
  all replace `/etc/nginx/nginx.conf` and the last listed silently wins).
  `bigbody` + `s3proxy` **do** stack — different services. Neither `logfault` nor
  `steplink` is needed here.

Throughout, `psql` means
`docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "<sql>"`,
`API` means `curl -sS -H "Authorization: Bearer ha-admin-token"` against
`http://localhost:18080`, and `mc` means
`docker compose $COMPOSE_FILES exec -T mc mc`.

**Workloads.** `tick.payload.json` (`edge-tick`, 30 stdout lines) for every
archival arm — small logs make the archive object small and the `Put` a single
S3 request, which is what makes the request census in Part B legible.
`artifact-large.payload.json` (`edge-artifact-large`, 64 MiB default) for Part C.
**No new fixture is added by this scenario.**

---

## BASELINE GATE — do not proceed past a failing check

Write every gate output to `$SCRATCH/gate.txt` unless a per-check file is named.

```bash
SCRATCH="<scratchpad>/w3-2" ; mkdir -p "$SCRATCH"
```

- **G0 — worktree.** `git rev-parse --show-toplevel` is `.../wt-edge-spec`,
  branch `plan/edge-case-w3`. `docker compose ls` shows the developer stack
  (project `unified-cd`) present and untouched; `test/ha`'s project is
  `unified-cd-ha`. **STOP** if the toplevel is the main checkout.
- **G1 — Garage is actually there and healthy**, and the controllers are
  actually using it. All three controllers must log
  `using S3-compatible object store` with `endpoint` and `bucket`, and **none**
  may log `no object store configured`. → `$SCRATCH/gate-g1-objstore.txt`.
  **STOP on any mismatch** — against a rig with no object store every part below
  is a silent no-op, which is exactly what W3-4 had to write up as code-read.
- **G2 — stack health.** Three controllers up, `API /readyz` → 200,
  `GET /api/v1/agents` lists agent1 and agent2 connected with their labels
  recorded (`kind:linux` is what every fixture selects on).
  → `$SCRATCH/gate-g2-agents.txt`.
- **G3 — clean slate, object side included.**
  `SELECT count(*) FROM runs WHERE status IN ('Pending','Queued','Running');` → 0;
  `SELECT count(*) FROM run_log_archives;` recorded; and
  `mc ls --recursive garage/unified-cd-logs/` recorded. **After a `down -v` the
  last of these must be empty** — if it is not, the volume survived and the
  teardown was not `-v`. → `$SCRATCH/gate-g3-clean.txt`.
- **G4 — the archiver demonstrably works before anything is broken.** Trigger
  `edge-tick`, wait for `Succeeded`, and within ~60 s confirm **all three** of:
  a `run_log_archives` row for it, the object `runs/<id>/logs.ndjson` in Garage
  at the same `size_bytes`, and the `archived Run logs` INFO line naming the
  replica that did it. **STOP if archival does not happen** — every negative
  result in Part A is meaningless without this positive control.
  → `$SCRATCH/gate-g4-archive-baseline.txt`.
- **G5 — the archiver interval and the leader identity, measured not assumed.**
  Over a 2-minute quiet window, record which replica logs
  `archived Run logs` / holds the advisory lock, sampling
  `SELECT pid, objid, granted FROM pg_locks WHERE locktype='advisory';` every
  5 s. **Fact 2 says leadership is per-tick, not sticky** — this is where that is
  confirmed or refuted, and Part A's per-replica attribution depends on it.
  → `$SCRATCH/gate-g5-leader.txt`.
- **G6 — (Phase 2 only) the interposer is live and unarmed.**
  `inject.sh s3-clear` then `inject.sh s3-probe`, and confirm an `X-S3-Arm: none`
  header plus a `s3fault`-format access line in
  `docker compose $COMPOSE_FILES logs s3proxy --since 30s`. **STOP if either is
  absent** — a missing header means the arm files are not being included and
  every injection below is a silent no-op (the W2-5 failure).
  → `$SCRATCH/gate-g6-probe.txt`.
- **G7 — (Phase 2 only) `bigbody` took.**
  `docker compose $COMPOSE_FILES exec -T nginx grep -c client_max_body_size /etc/nginx/nginx.conf`
  is non-zero and `nginx -t` is ok. **STOP otherwise** — a 413 would make Part C
  measure the LB. → `$SCRATCH/gate-g7-bigbody.txt`.
- **G8 — API 500s.** The API on this rig has been intermittently returning 500s.
  Record, for every gate and every trigger, how many attempts it took. A 500 on a
  trigger is **not** a finding of this scenario; a 500 mid-measurement
  invalidates that attempt, which must be discarded and re-run, not reasoned
  around.

---

## Part A — archival under a full outage (Phase 1)

**Deliverable:** for each of two outage shapes, a per-replica, per-run tally of
`failed to archive Run logs` lines with timestamps; proof that the runs
themselves are untouched and stay in the candidate set; and the recovery latency
once the store returns.

### A1 — the refusal shape (`kill-hard`)

- **A1.1.** `inject.sh kill-hard garage`. Confirm the container is gone
  (`docker compose ps garage`) and record the instant.
- **A1.2.** Trigger **2** `edge-tick` runs, wait for both `Succeeded`. Record
  run ids and terminal instants.
- **A1.3 — the run is unaffected, measured on four surfaces**, because "archival
  is post-terminal" is the claim being tested, not an assumption:
  `GET /api/v1/runs/{id}` → `Succeeded`; `GET /api/v1/runs/{id}/steps` → step
  `Succeeded`, exit 0; `SELECT count(*) FROM logs WHERE run_id=…` → equals what
  `edge-tick` emits (**count it from the control run in G4**, do not assume 30);
  `GET /api/v1/runs/{id}/logs/stats`. → `$SCRATCH/partA-runs.txt`.
- **A1.4 — observe for a bounded window and SAY the window.** Hold the outage
  for **8 minutes** from A1.2. Then, **before touching any container**, capture
  all three controllers' logs for the window in full:
  `docker compose $COMPOSE_FILES logs controller1 controller2 controller3 --since <t0>`
  → `$SCRATCH/partA-controllers.txt`.
- **A1.5 — THE MEASUREMENT, and the arm that tests a fact this plan corrected.**
  From that capture build a table with one row per `failed to archive Run logs`
  line: `time`, `replica`, `runId`, `error`. Then answer, in the entry:
  1. **How many attempts did each run get, and from which replicas?** Fact 4
     says `bo` is per-process; fact 2 says leadership moves per tick. So a run
     should be retried on a replica that has not yet excluded it even while
     another replica is still inside its 1-minute window. **Report the observed
     count against the single-process schedule of fact 5** (1 attempt, then
     +1 min, +2, +4, +8, …) and state whether the observed rate exceeds it.
  2. **Did every replica attempt it at least once?** A replica that has never
     been leader has an empty exclusion set, so its first turn always costs one
     attempt per candidate.
  3. **Is the only trace the `slog.Error`?** `grep -c` for it, and grep the same
     window for anything else naming the run id — a WARN, a metric, an audit
     row, a run field. `SELECT * FROM audit_logs WHERE …` if the table names the
     run. → `$SCRATCH/partA-backoff.txt`.
- **A1.6 — the run stays in the candidate set.** Run the eligibility query by
  hand for both runs:
  `SELECT id FROM runs WHERE status IN ('Succeeded','Failed','Cancelled') AND id NOT IN (SELECT run_id FROM run_log_archives) ORDER BY updated_at;`
  → both present. This is what "never marked" means concretely.
- **A1.7 — recovery, which is the I5 limb.** `docker compose up -d garage`, wait
  for healthy, and time how long until both runs have `run_log_archives` rows and
  their objects exist in Garage. **Compare against the 30 s interval plus the
  backoff each replica has accumulated** — the honest expectation is *not* 30 s,
  because a run already excluded on the replica that wins the next tick waits
  out its window there. Record the actual latency and the replica that did it.
  → `$SCRATCH/partA-recovery.txt`.

**Falsification.** If the runs are archived *during* the outage, or if a run's
status or logs change, the scenario's premise is wrong and that is the finding —
report it and stop. If **zero** `failed to archive Run logs` lines appear in
8 minutes, the archiver is not running: re-check G1 and G4 before concluding
anything about backoff.

### A2 — the hang shape (`pause`), and the lock question

`pause` (SIGSTOP) leaves the listening socket in the kernel, so a connect
completes and no response ever comes — the realistic cloud-outage shape, and the
one that exercises timeout paths. Fact 14 says nothing bounds it.

- **A2.1.** With the stack healthy again, trigger 1 `edge-tick` and wait for
  `Succeeded`, but **do not** let it be archived: `pause garage` immediately
  after the run is created, so the archiver's next tick finds the run and stalls.
- **A2.2 — is the archiver stalled, and is the lock held?** Sample
  `SELECT pid, objid, granted, pg_blocking_pids(pid) FROM pg_locks WHERE locktype='advisory';`
  every 5 s for **4 minutes**, alongside
  `docker compose $COMPOSE_FILES logs controller1 controller2 controller3 --since …`.
  Two things to establish: (i) whether `objid` **1819242081** stays granted to
  one pid continuously rather than being taken and released every 30 s (compare
  against G5's healthy baseline — that comparison is what licenses the word
  "stalled"), and (ii) whether **any** replica logs anything at all during the
  stall. → `$SCRATCH/partA2-stall.txt`, sampler script in
  `$SCRATCH/partA2-sampler.sh`.
- **A2.3 — bound the claim honestly.** After 4 minutes, `unpause garage` and
  record what happens. **The write-up must say the window was 4 minutes and that
  this runbook ended it** — the code-read claim that nothing would ever end it
  (fact 14) is a separate, labelled statement.
- **A2.4 — the blast radius of the stall.** While stalled, confirm the rest of
  the system is unaffected: trigger a run and let it complete, read the API,
  check `/readyz` on all three. The point is to establish whether this is a
  *cluster-wide archival* stall (all replicas blocked behind one lock) or a
  *whole-controller* stall. **They are very different findings** — measure which.
  → `$SCRATCH/partA2-blast.txt`.

**Falsification.** If the lock is released every 30 s during the pause, the
archiver is not blocking on S3 and A2's premise is wrong — likelier is that
`minio-go`'s transport has a dial or response-header timeout the code read
missed, which would itself be the correction to record.

---

## Part B — the object/record window (Phase 2)

**Deliverable:** two produced interleavings — (i) `Put` succeeds, record fails,
compensating `Delete` **succeeds**, leaving no trace in Garage; (ii) the same
with the compensating `Delete` **blocked**, leaving an orphan object plus the
`slog.Warn` — and an explicit statement of which further interleavings are
code-read.

**How the window is opened, and why this way.** The window between `obj.Put`
(`archiver.go:96`) and `st.CreateLogArchive` (`:106`) is microseconds for a tiny
log. Two levers widen or trigger it:

- ~~**`inject.sh s3-latency <n>`** routes every S3 request at a black hole first
  and lets `proxy_connect_timeout` expire before falling through to Garage
  (`nginx-s3.conf:107-110`), so a single-request `Put` is delayed by ~`n`
  seconds. Task 3 measured this on a 64 MiB Put (0.753 s → 9.702 s under
  `s3-latency 3`, 3 requests) and confirmed it **delays** rather than **fails**.
  A `edge-tick` archive is a few kilobytes, so it should be **one** request —
  **verify that from the interposer access log rather than assuming it.**~~
  **SUPERSEDED — see correction 2 at the top of this file. `s3-latency 30` broke
  the Put instead of widening it**: the black hole answered `connection refused`
  rather than hanging, `proxy_next_upstream timeout` does not cover a refusal,
  and the request 502'd after 21.037 s (`$SCRATCH/partB-arm1.txt`). Task 3's
  measurement at `s3-latency 3` is not withdrawn — at 3 s the connect deadline
  expires before any ICMP unreachable arrives — but **the verb is not
  dependable at large values**, and Part B needed a wide window.
- **USE `inject.sh pause garage` BEHIND THE INTERPOSER instead.** nginx accepts
  the controller's request, proxies it to the paused container and waits on
  `proxy_read_timeout 300s` (`nginx-s3.conf:166`), which is a genuine hang. The
  window is then bounded only by minio-go's 60 s `ResponseHeaderTimeout`
  (`minio-go/v7@v7.2.0/transport.go:52`) — ample. Unpause once the `runs` row is
  deleted and the in-flight `Put` completes against a live Garage. Measured
  Put durations with this lever: **4.862 s** (arm 1) and **6.267 s** (arm 2),
  both `200` at the interposer.
- **Deleting the `runs` row inside that window** makes `CreateLogArchive` fail on
  the FK, which is the *exact* failure `archiver.go:107-110`'s own comment
  describes.

**METHOD, labelled up front rather than glossed.** The `runs` row is deleted with
**raw SQL**, not through `DELETE /api/v1/runs/{id}`, and the write-up must say
so. Reasons, both of which matter: the API route runs `deleteRunEverywhere`,
whose own object deletes would be caught by arm (ii)'s `DELETE` block and would
entangle two mechanisms in one measurement; and its step-5 re-sweep is **W3-6's**
subject (Task 5), which this scenario must **cross-reference rather than
re-measure**. So Part B produces the *archiver-side* half live and leaves the
retention-side half code-read, with the cite.

**Detecting that the Put is in flight.** Do not guess. `pg_locks` is the
instrument: the archiver holds `objid` **1819242081** for the whole of
`archiveRunLogs` (fact 1 + Part A2), so a lock that has been continuously granted
for longer than the healthy hold time means a `Put` is in flight *right now*.
Poll it every second and fire the SQL delete on that signal.

- **B1 — arm (i), compensation succeeds.**
  1. `inject.sh s3-clear`, then `inject.sh s3-latency 30`, then `s3-probe` —
     record the probe.
  2. Trigger `edge-tick`, wait for `Succeeded`.
  3. Poll `pg_locks` at 1 s. When the archiver lock has been held ≥ 5 s, run
     `DELETE FROM logs WHERE run_id='<id>'; DELETE FROM runs WHERE id='<id>';`
     (or rely on the cascade — **check which** and record it).
  4. When the `Put` completes, expect: `failed to archive Run logs` with an FK
     error, **no** `failed to clean up orphaned log archive object` warn, and
     `mc ls --recursive garage/unified-cd-logs/runs/` showing **no** object for
     that run id.
  → `$SCRATCH/partB-arm1.txt`.
- **B2 — arm (ii), compensation blocked.** Same as B1 with
  `inject.sh s3-block DELETE unified-cd-logs/ 403` armed **as well**
  (`s3-block` and `s3-latency` write different include files and compose;
  **`s3-block` does not compose with itself** — one block arm at a time). Expect:
  the same FK error **plus**
  `failed to clean up orphaned log archive object after CreateLogArchive failure`
  at `WARN`, and the object **present** in Garage with no `runs` row and no
  `run_log_archives` row pointing at it. → `$SCRATCH/partB-arm2.txt`.
  - **Choose the status deliberately**: `403`, not `503`. minio-go retries
    429/500/502/503/504 internally up to ten times with backoff, so a `503` arm
    turns a one-shot failure into a slow retried one and moves the timing. Part B
    is timing a window, so it wants the immediate refusal.
- **B3 — prove the orphan is an orphan, on three surfaces.** For arm (ii)'s
  object: no `runs` row, no `run_log_archives` row, and the API cannot enumerate
  it (`GET /api/v1/runs/{id}` → 404). Then **state the reachability argument and
  cross-reference rather than double-file**: `ListExpiredRuns` is driven off
  `runs` rows (`internal/store/postgres.go:1494-1502`) and the row is gone, and
  `deleteRunEverywhere`'s step-5 re-sweep (`run_retention.go:167-170`) can only
  fire for a run it is *currently deleting*. **W3-6 (Task 5) owns the
  orphan-object finding**; W3-2 contributes a second production path to it and
  must say so in one sentence rather than filing its own.
- **B4 — the state the code says cannot exist.** Confirm, for every run in the
  session, that there is **no** `run_log_archives` row whose object is missing
  from Garage:

  ```sql
  SELECT run_id, object_key, size_bytes, line_count FROM run_log_archives;
  ```

  cross-checked against `mc ls --recursive garage/unified-cd-logs/runs/`.
  **This is I4's "archives stay readable" clause**, and a clean result is a
  *pass* to record, not an absence. Also confirm re-archival idempotency
  (fact 15) by deleting a `run_log_archives` row for a still-existing run and
  letting the next tick rewrite it.
  → `$SCRATCH/partB-integrity.txt`.
- **B5 — attempt discipline.** B1 and B2 each have a **cap of 6 attempts**.
  Record the count reached **either way**. If an arm is not hit, file the
  code-read argument (facts 7 + `archiver.go:107-116`) with an explicit
  **"not reproduced live"** label — the W2-3 Arm D precedent, accepted at 0/10.
  **Do not write that the window is unreachable.**

---

## Part C — artifact upload, one shot (Phase 2)

**Deliverable:** a `Failed` run whose failing step is the artifact upload, with
the interposer access log showing the controller's `Put` attempted and refused,
the controller returning **500**, the agent attempting the upload **exactly
once**, and every operator-visible surface enumerated.

- **C1 — control first.** With no arm, trigger `edge-artifact-large`
  (default 64 MiB) and confirm it `Succeeded`, the object is in Garage at the
  expected size, and `GET /api/v1/runs/{id}/artifacts` lists `big-blob`. Record
  the `upload_blob` step duration — it is the width Part C's fault has to land
  inside, and Task 3 measured it at **0.753 s** for 64 MiB on this stack.
  → `$SCRATCH/partC-control.txt`.
- **C2 — the arm.** `inject.sh s3-block PUT unified-cd-logs/artifacts/ 403`,
  probe, and record both the blocked probe and the control probe the verb prints
  (the pair it must **not** match). Note the prefix includes the bucket, which is
  also the side selector: `unified-cd-logs/` is the controller's, so the agent's
  cache path (`unified-cd-cache/`) is untouched. → `$SCRATCH/partC-arm.txt`.
- **C3 — trigger and measure.** Trigger `edge-artifact-large`. Establish, each
  from a named capture:
  1. **The agent attempted the upload once.** Count
     `PUT /api/v1/runs/<id>/artifacts/big-blob` lines in the **controllers'**
     access logs for the run's window — expect exactly **1**. (The controller's
     own access log is the bracket; the agent's log is not, because the agent
     logs no request line.)
  2. **The controller answered 500**, and its `slog` carries the real message
     that the agent's error text does not. Capture it container-side.
  3. **The interposer saw the `PUT` and refused it**, with `arm=block[...]`
     stamped on the request — this is the per-request bracket, and it is what
     licenses the word "armed" for this specific request rather than for a
     wall-clock window.
  4. **The step is `Failed` and the run is `Failed`** — `GET /runs/{id}/steps`
     and `GET /runs/{id}`.
  5. **What the run's own log says.** Expect `upload-artifact "big-blob": upload
     artifact http 500` and nothing more. **This is a second, independent
     instance of the wave-level "no diagnosable agent-side error" fact, on a path
     that does NOT go through `c.do`** — the message is content-free because
     `UploadArtifact` never reads the body at all (fact 11), not because
     `"response omitted"` overwrote it. Say that distinction explicitly; it is
     the difference between one policy and two.
  → `$SCRATCH/partC-fail.txt`.
- **C4 — is the failure clean?** Confirm no partial object was left in Garage
  under `artifacts/<runID>/`, that `GET /api/v1/runs/{id}/artifacts` returns
  `[]`, and that the run reached exactly one terminal state. **A `Failed` run
  with no artifact is correct behaviour; a `Succeeded` run with no artifact would
  be the I4 violation.** Check which, do not assume.
  → `$SCRATCH/partC-clean.txt`.
- **C5 — the 500-vs-503 contrast.** Fact 10 is the notable part: a
  **configured-but-down** store gives 500, a **missing** one gives 503, and the
  agent's error text differs only in the digits. The 500 is measured in C3.
  **The 503 is CODE-READ on this rig and must be labelled as such** — producing
  it needs a controller with `UNIFIED_S3_*` unset, which this stack does not have
  and which Part D's overlay-free phase cannot supply either. Note that W3-4 ran
  on exactly such a rig and never uploaded an artifact, so the campaign has not
  measured the 503 branch anywhere; say so rather than implying it was checked.
- **C6 — falsification.** If the run **Succeeds** despite the blocked `PUT`,
  check the interposer bracket for that request before concluding anything: the
  likeliest explanation is that the arm did not cover the key (the artifact key
  is `artifacts/<runID>/<name>.tar.gz`, `internal/artifact`), not that the
  product swallowed the error. If the upload is attempted **more than once**,
  fact 8 is wrong and *that* is the finding.

---

## Part D — the fatal-startup limb (Phase 1)

**Deliverable:** a controller that will not start while the object store is
unreachable, the exact log line and exit, the container's post-mortem state, and
a judgement against the documented startup contract.

- **D1 — read the restart policy BEFORE the experiment.** `docker compose config`
  for the controller services: whether they carry a `restart:` policy decides
  whether "crashloop" is even the right word on this rig. **Record it verbatim.**
  On Kubernetes the pod would crashloop; with compose's default the container
  exits **once** and stays exited, which is a *different and arguably worse*
  recovery story. → `$SCRATCH/partD-policy.txt`.
- **D2 — the experiment.** With Garage down (`kill-hard garage`),
  `docker compose $COMPOSE_FILES restart controller1`. Use **`restart`, not
  `up -d --force-recreate`** — a recreate discards the container's log history,
  and the log line is the deliverable. Capture:
  `docker compose $COMPOSE_FILES logs controller1 --since 2m` and
  `docker compose $COMPOSE_FILES ps -a controller1`. Expect
  `{"level":"ERROR","msg":"s3 object store init","error":"bucket exists check: …"}`
  and exit code 1. → `$SCRATCH/partD-crash.txt`.
- **D3 — does it recover on its own?** Bring Garage back and wait **2 minutes**
  without touching controller1. Record whether it comes back by itself. Then
  `up -d controller1` and confirm it starts. **This is the I5 limb**: "returns to
  steady state" is about what happens without an operator.
  → `$SCRATCH/partD-recovery.txt`.
- **D4 — blast radius during D2-D3.** With one of three replicas dead, confirm
  the LB still serves (`proxy_next_upstream` covers a refused connection) and
  runs still execute. Record how many `no live upstreams` / `502` lines nginx
  logged, if any. A single dead replica should be invisible; **measure it**, and
  note explicitly that this is why the finding's severity argument cannot lean on
  "the cluster went down".
- **D5 — the judgement, and the two lessons that govern it.**
  1. **Read the surrounding section, not the line.** `docs/high-availability.md`
     § "External Dependency Redundancy" (`:293-333`) is one section covering
     Postgres, S3 and Vault. Quote `:309-310` and `:322-325` **together**; the
     asymmetry between them is the substance.
  2. **Check whether this branch has already ruled on the passage.** Task 3's
     413 finding was reclassified for reading `docs/high-availability.md:253-254`
     broadly when the same branch had already read it narrowly
     (`FINDINGS.md:1563`). Before filing, `grep -rn "high-availability.md:3" test/edgecase/`
     and record the result.
  3. **Cross-reference, do not double-file.** `FINDINGS.md:43` already carries a
     W0-1 entry — *"Controllers crash-loop with no DB-connect retry at startup"*,
     **I5, minor** — for the identical shape against **Postgres**. W3-2's
     contribution is not "the controller exits on a failed dependency"; it is
     that (a) the dependency in question is documented as **optional**, (b) the
     eager `BucketExists` makes an *optional* subsystem's outage fatal to the
     whole process including every unrelated API, and (c) the same document
     documents this behaviour explicitly for Vault and not for S3. **If that
     delta does not survive scrutiny, fold it into W0-1's entry as a second
     instance instead of filing a new one.**

---

## Teardown

```bash
# 1. clear every interposer arm and PROVE it is clear (Phase 2 only)
../edgecase/tools/inject.sh s3-clear && ../edgecase/tools/inject.sh s3-show \
  && ../edgecase/tools/inject.sh s3-probe
# 2. unpause / restore anything paused or killed, and show the stack healthy
docker compose $COMPOSE_FILES ps
# 3. down
docker compose $COMPOSE_FILES down -v
```

- **Cancel every surviving run before teardown**, and confirm zero non-terminal
  runs in a census.
- **`down -v`, not `down`.** The Garage volume is persistent (§Stack).
- **Kill every background sampler and *capture* that, do not assert it.** Part A2
  and Part B both run polling loops. Keep PIDs in `$SCRATCH/samplers.pid`, `kill`
  them explicitly, then show `jobs` empty and `ps -W | grep -iE "curl|psql|python"`
  matching nothing — **and check inside the containers too**, because a
  `docker compose exec` sampler outlives the shell that launched it and appears
  in neither (the W3-4 lesson, which caught a live one). Two passes: before the
  teardown of each phase. → `$SCRATCH/teardown.txt`.
- Copy `$SCRATCH` into the campaign evidence root at the wave checkpoint
  (`test/edgecase/README.md` § "Raw evidence").

---

## Recording rules

- **A run failed as collateral damage of an archival failure ⇒ major (I4/I5).**
  The code says this cannot happen (archival is post-terminal). **If it does not
  happen, that is a recorded pass, not an omission** — say so with the four
  surfaces from A1.3, because "the run is unaffected" is the load-bearing half of
  why an archival outage is *only* a diagnosability problem.
- **An archival stall that holds the cluster-wide advisory lock ⇒ judge against
  I5 quoted verbatim.** State the observed window and **who ended it**; keep the
  "nothing in the code would end it" claim separate and label it code-read
  (fact 14). Do not write "never" or "unbounded" for a window this runbook
  closed.
- **An orphaned object with no reconciler ⇒ cross-reference W3-6, do not
  double-file.** One sentence naming W3-2 as a second production path for the
  same class is the correct treatment.
- **The crashloop ⇒ judge on the documented startup contract**, per D5, and
  **cross-reference W0-1's Postgres entry**. If `docs/*.md` does not cover the
  configured-but-unreachable case, write **"silent, not contradicted"** and rest
  on I5.
- **The diagnosability gap ⇒ observation.** Entry titles must say
  **"observation"** (`FINDINGS.md:481`) and the Severity line must read
  `minor (observation)`.
- **Every number cites a `$SCRATCH` filename whose time window covers it.**
  Derived figures say "derived"; code-read figures say "code-read"; uncaptured
  live observations say `(observed live, raw output not captured to scratchpad)`.
  **Do not present verbatim-looking log text that was not captured**, and do not
  call an attribution a "bracket" unless a capture covers that specific request.
- **Report every capped arm's attempt count either way**, with the cap stated.
- **Cross-references, not re-filings.** Four already-filed items live near this
  machinery and none of them is this: **W0-1's startup crash** (Postgres, I5,
  minor); **W1-2 / W1-6 / F9** (all agent-side log loss, the other side of the
  socket); **W3-4** (duplicate surplus in `logs`, with an archive-fidelity limb
  measured at `FINDINGS.md:1557`); and **W3-6** (orphan artifacts, Task 5). Name
  them explicitly so triage does not merge them.

---

## Execution notes — 2026-07-31 run (read before re-running)

Executed on branch `plan/edge-case-w3`, **`02:54:55Z – 03:48:14Z`**, in the two
phases §Stack specifies, with a `down -v` between them and after them
(`w3-2/teardown.txt`). **Four `FINDINGS` entries: 1 violation (minor, I5) and 3
observations (minor).** No branch-internal asset bug in the campaign's shipped
assets — but **this runbook itself shipped a wrong constant** (correction 1),
which is recorded here rather than filed, because it was caught and corrected
inside the same session before any finding rested on it. The developer stack
(`docker compose ls`, project `unified-cd`) was running and untouched at both
ends (`w3-2/gate.txt`, `w3-2/teardown.txt`).

**Nine runs, all reaching exactly one terminal state — 8 `Succeeded`, 1
`Failed`** (the Part C armed run, which is *supposed* to fail). Phase 1 ended
with 6 runs / 6 archives / 6 objects; phase 2 ended with 2 surviving runs, 2
archives and 1 orphan object left deliberately by Part B arm 2.

| Part | Arm | Result |
|---|---|---|
| G4 | none | archival works: 3038 B / 30 lines / `maxSeq` 30, `controller1`, 59 s after the run went terminal |
| A1 | `kill-hard garage`, 8 min held | **20** `failed to archive Run logs` lines for **2** runs across **all three** replicas; runs untouched on four surfaces |
| A1.7 | restore | both archived **59.1 s** and **89.1 s** after Garage returned, both by `controller1`, both complete (30/30 lines) |
| A2 | `pause garage`, 7.5 min held | **4** attempts, each ~65 s of continuously-held cluster lock; error text differs from A1 and is misleading |
| A2.4 | during the stall | `/readyz` **200** on all three, LB `/healthz` 200, a fresh run `Succeeded` in 35 s |
| B arm 1 | `pause` + SQL `DELETE FROM runs` mid-`Put` | FK violation on `CreateLogArchive`, compensating `Delete` **204**, **no** object left |
| B arm 2 | same + `s3-block DELETE unified-cd-logs/ 403` | same FK violation, compensating `Delete` **403**, `WARN` fired, **3.0 KiB orphan** left |
| C1 | none | 64 MiB artifact `Succeeded`, `upload_blob` 0.813 s, controller `PUT` 204 in 807 ms, 3 S3 requests |
| C3 | `s3-block PUT unified-cd-logs/artifacts/ 403` | run `Failed`, step `Failed`, **exactly 1** controller `PUT` (500, 160 ms), multipart aborted cleanly |
| D2 | `kill-hard garage` + `restart controller1` | `s3 object store init` ERROR, **exit 1**, `restartCount=0`, container stays `exited` |
| D3 | Garage restored | still `exited` after **2 minutes** with no operator action; `up -d` brings it straight back |

**Ten things a re-run should know.**

1. **Print the constant with the result.** Correction 1 is the whole lesson: two
   captures returned a clean, plausible negative because the `objid` literal was
   mistyped, and a clean negative is the one result nobody re-checks.
   `0x6C6F6761` = **1819240289**.
2. **The archiver lock is transient when healthy and continuous when the store
   is broken, and that contrast IS the instrument.** Under `kill-hard`, 55 of 70
   1-second samples had it granted; under `pause`, 86 of 120 2-second samples.
   The gaps between holds are the backoff windows. It is also the only reliable
   *in-flight* signal for Part B — poll it and fire on it, do not guess.
3. **A failing archival pass is slow, and the lock is held for all of it.**
   Under `kill-hard` each failing `Put` cost ~44 s and a two-run pass ~89 s;
   under `pause`, ~65 s per attempt. The batch limit is **20**
   (`archiver.go:55`), so a full poison batch is arithmetic nobody has done:
   ~15 min of held cluster-wide lock (derived from 20 × 44 s, **not measured**).
4. **The `pause` and `kill-hard` errors are not interchangeable, and only one is
   diagnosable.** `kill-hard` gives `dial tcp: lookup garage … no such host`.
   `pause` gives `readfrom tcp …: http: ContentLength=3047 with Body length 0`,
   which names no outage at all — see the observation entry on the
   non-rewindable buffer.
5. **Leadership rotates per tick and every replica pays its own first attempt.**
   The 20 attempts split 8 / 4 / 8 across `controller1` / `controller2` /
   `controller3`; passes alternated cleanly. Attribute from the log line's
   container prefix, and attribute the *lock* from
   `pg_locks ⋈ pg_stat_activity.client_addr` against the container IPs.
6. **Recovery is governed by the backoff, not by the 30 s interval.** 59.1 s and
   89.1 s, both matching `controller1`'s derived `retryAt` (03:22:37 / 03:23:20)
   to within one tick. Do not quote "30 s" as the recovery bound.
7. **Part B needs `pause`, not `s3-latency`** (correction 2), and **raw SQL for
   the `runs` row** — the API `DELETE` route runs `deleteRunEverywhere`, whose
   own object deletes would collide with arm 2's `DELETE` block and mix two
   mechanisms in one measurement. Label the SQL as the instrument it is.
8. **Arm 2 leaves a real orphan on purpose.** `runs/<id>/logs.ndjson`, 3.0 KiB,
   no `runs` row, no `run_log_archives` row, `GET /api/v1/runs/{id}` → 404. It
   is destroyed by the `down -v`, so re-create it if you need it.
9. **Part C's failure is correct and completely undiagnosable, and the second
   half is the finding.** One `PUT`, 500, step `Failed`, run `Failed`, multipart
   aborted (`DELETE … 204` at the interposer), no partial object — all correct.
   And: **zero** controller `WARN`/`ERROR` lines, **zero** run log lines about
   it, agent error `upload artifact http 500` with no body ever read, and the
   failed step reports `"exitCode":0`.
10. **`restart: "no"` is the rig's policy and it changes the vocabulary.** On
    this stack the controller does not crash-*loop*; it exits once and stays
    exited. Read `partD-policy.txt` before writing "crashloop" — the product
    behaviour under test is `os.Exit(1)` with no in-process retry, and the
    recovery story is entirely the supervisor's.

**Sampler hygiene was captured, not asserted.** All polling loops in this
scenario were foreground `bash` `for` loops that ran to completion, so there was
nothing to kill; both passes recorded `jobs` empty, `ps -W | grep -iE
"curl|psql|python"` empty on the host, and — per the W3-4 lesson that a
`docker compose exec` sampler outlives its shell — an in-container `ps` check on
`nginx`, `s3proxy` and `postgres`, all empty (`w3-2/teardown.txt`). The `mc`
image has no `grep`, which the capture records rather than hides.

**No Postgres statement logging was armed.** Nothing in this scenario needed the
insert-side view, so `log_statement`/`log_line_prefix` were left at their
defaults and there was nothing to revert. Said explicitly because the runbook
budgets for it elsewhere in the wave.

**The rig's intermittent-500 allowance (G8) was never spent** — zero API 500s on
any trigger or gate command across 9 triggers and every gate. A re-run should not
assume that.
