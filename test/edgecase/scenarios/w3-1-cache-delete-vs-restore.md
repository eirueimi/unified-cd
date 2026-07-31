# W3-1 — a cache object deleted while a restore is streaming it

**Wave W3, Task 7 — the wave's last scenario.** The scenario attacks the one
concurrency window that actually exists in `internal/cache/`: a `Restore` holds
an open object-store GET and extracts it **incrementally into the workspace**,
while any `Delete` of that key — the cleanup sweeper's, or anything else's — can
land in the middle. There is **no lease, no refcount, no generation and no ETag
guard anywhere in the package.**

---

## The premise this scenario was chartered with is wrong, and the amendment is the point

The campaign spec chartered W3-1 as a **"cache TTL expiry racing a restore"**
with a DB/S3 ordering race in it. **That race does not exist, because the cache
has no database presence at all.**

- `internal/cache/cache.go` takes an `objectstore.ObjectStore` and nothing else.
  There is no `store.Store` parameter anywhere in the package, no `caches`
  table, no migration. Two objects per entry and that is the entire model:
  `caches/<b64url(sha256(jobName))>/<b64url(sha256(key))>.tar.zst` and the same
  prefix `+ ".meta"` (`cache.go:49-53`, `base64.RawURLEncoding` — unpadded,
  URL-safe, over the **raw** digest, not the hex).
- **The TTL is a JSON field inside the `.meta` object** —
  `ExpiresAt: time.Now().Add(time.Duration(ttlDays) * 24 * time.Hour)`
  (`cache.go:90`). Not a column, not S3 object metadata, not a bucket lifecycle
  rule.

So there is no "row committed before the object" and no "object deleted before
the row". **What is left is a read-during-delete**, and it is a real window
because `Restore` opens the stream at `cache.go:142` and `extract` (`:265-313`)
writes files to disk as it walks the tar, one entry at a time.

**Everything below tests that window.** The scenario id, the invariant and the
recording rules are unchanged; only the mechanism is.

---

## The scheduler lever does not exist either — drive the delete by hand

**`RunCacheCleanup` is the only background loop in the controller that does not
take an interval.** Verified by enumeration, not by impression — every
`func Run*(ctx …)` in `internal/controller/`:

| Function | Interval parameter |
|---|---|
| `RunApprovalReaper` (`approval_reaper.go:21`) | `interval time.Duration` |
| `RunAppSourceReconciler` (`appsource_reconciler.go:56`) | `tick time.Duration` |
| `RunAppSourceSyncReaper` (`appsource_sync_reaper.go:24`) | `interval, staleAfter` |
| `RunLogArchiver` (`archiver.go:19`) | `interval time.Duration` |
| `RunAuditRetention` (`audit_retention.go:21`) | `interval time.Duration` |
| `RunLogTrim` (`log_trim.go:28`) | `interval time.Duration` |
| `RunQueuedRunReaper` (`queuedrun_reaper.go:29`) | `interval, minAge, staleAfter` |
| `RunRunRetention` (`run_retention.go:26`) | `interval time.Duration` |
| `RunScheduler` (`scheduler.go:23`) | `tick time.Duration` |
| `RunGitResolver` (`scheduler.go:262`) | `tick, deadline` |
| `RunStuckRunReaper` (`stuckrun_reaper.go:22`) | `interval, staleAfter, grace` |
| **`RunCacheCleanup` (`scheduler.go:219`)** | **none — `time.NewTicker(24 * time.Hour)` is written inline at `:223`** |

`time.NewTicker` does not fire immediately, so **a freshly started controller's
first cache cleanup is at t+24 h**, and a rig that is torn down between
scenarios never reaches one. A repo-wide grep for a lever
(`CACHE_CLEANUP|cache-cleanup|cacheCleanupInterval|CacheCleanup`, excluding this
campaign's own files) returns **7** hits and **not one is a flag, an env var or
a config field**: the call site (`cmd/controller/main.go:401`), the doc comment
and the function (`scheduler.go:216`, `:219`), the leader helper (`:231`,
`:235`), one docs row (`docs/high-availability.md:83`) and two unit tests.

**So do not wait for the sweeper, and do not try to make it fire.** Compute the
object key from `cache.go:49-53` and delete the payload with `mc` while a
restore is streaming it. That exercises the identical `objectstore.Delete` the
sweeper would call (`cache.go:204`), at a chosen instant, with no code change,
no backdating and no 24 h wait. **The hardcoded interval is recorded as a
finding in its own right, not worked around silently.**

---

## Corrections to inherited facts, established BEFORE execution

Per the W1/W2/W3 carry-forward rule, the plan's "Verified code facts" block
(`docs/superpowers/plans/2026-07-30-edge-case-campaign-w3.md:54-65`) is a set of
**claims**. Every one was re-read at this branch's HEAD.

- **CORRECTION 1 — SUBSTANTIVE, AND IT INVERTS THE SCENARIO'S EXPECTED
  OUTCOME. A torn restore does NOT fail the step.** The plan (`:61`) states:
  "the error surfaces as `tar next: %w` (`:286`) → non-`ErrCacheMiss` → step
  fails → `markFailed` (`orchestrator.go:390`)". The first two links are right
  and the last two are wrong. The error does reach
  `executeCacheStep`, but the cache path is **lenient by explicit policy**:

  > `// Cache stays warn+skip on error (lenient policy): a restore/save problem`
  > `// should not fail the step, unlike artifact upload/download.`
  > — `internal/agent/orchestrator.go:969-971`

  and the code under it is
  `if hit, err := b.CacheRestore(...); err != nil { slog.Warn("cache restore error", ...) }`
  (`:972-973`) — **no return, no error propagated**. `executeCacheStep` then
  falls through to `client.ReportStep(..., Status: "Succeeded")` (`:1004-1006`).
  `markFailed` at `orchestrator.go:390` is only reachable when
  `executeCacheStep` returns a **non-nil** error, and the only three ways it
  does are a key-template failure (`:923`), a path-template failure (`:927`) and
  a rejected/escaping cache path (`:960`). A restore error is not among them.

  **This makes the scenario sharper, not weaker.** The predicted outcome was a
  failed step (loud, safe). The actual outcome is a **`Succeeded` step on top of
  a half-populated workspace**, which is the shape the task brief called
  "materially worse than a clean miss" — and it is the shape Part A must
  measure.

- **CORRECTION 2 — the leniency is DOCUMENTED, so lead with the sanction.**
  `docs/jobs.md:1399`: "Restore is best-effort (a miss or error never fails the
  step)". `docs/kubernetes-integration.md:446`: "**Cache** is best-effort: a
  `cache:` step restores at step time if a matching key exists, but a miss or
  restore error never fails the step." Both are unambiguous and both are
  correct at HEAD. **An entry that files "the step did not fail" as the defect
  is wrong and will be caught.** The finding is about *what is left on disk*
  and *what happens to it next*, neither of which any doc addresses.

- **CORRECTION 3 — the plan's account of why a `.meta`-without-payload orphan is
  benign is only true for ONE of its producers.** The plan (`:59`) says the
  orphan is "expired by construction → clean `ErrCacheMiss`" because
  `findBestMatch` skips expired entries (`cache.go:244`). That reasoning holds
  only when the orphan came from an interrupted `DeleteExpired`, which by
  definition only deletes entries already past `ExpiresAt`. An orphan produced
  any other way (an out-of-band delete, a partial failure elsewhere) carries a
  **live** `ExpiresAt`, so `findBestMatch` **does** select it — and the miss then
  comes from the *next* line instead: the fallback `store.Get(fallbackKey +
  ".tar.zst")` returns `ErrNotFound` and `Restore` converts it to `ErrCacheMiss`
  (`cache.go:162-167`). Same clean outcome, different code path. **Part C must
  say which path it actually exercised.**

- **CORRECTION 4 — "payload without `.meta` leaks forever" UNDERSTATES it, and
  the stronger statement is the one to test.** The plan (`:59`) says such a
  payload is "invisible to both `findBestMatch` and `DeleteExpired`". True — both
  `continue` on non-`.meta` keys (`:188`, `:232`). But it is **not** invisible to
  `Restore`: the exact-key path is `store.Get(oKey + ".tar.zst")` at
  `cache.go:142` and **never reads `.meta` at all**. So a payload with no `.meta`
  is not merely dead storage — **it is still served**, on every exact-key hit,
  with no TTL that anything can ever enforce against it. Part C tests the serve,
  not just the leak.

- **CORRECTION 5, derived from 4 and testable on its own — `ExpiresAt` is
  ADVISORY ON THE READ PATH.** `Restore`'s exact-key branch consults no
  metadata, so an entry whose `.meta` says it expired last year is served in
  full as long as its archive object is still there. The only two things that
  ever read `ExpiresAt` are `DeleteExpired` (`:202`) — 24 h ticker, first fire at
  t+24 h — and `findBestMatch` (`:244`), which is the `restoreKeys` **fallback**
  path only. **Part C plants an expired `.meta` and confirms the hit.**

- **CONFIRMED, not corrected:** the two object keys and their encoding
  (`cache.go:49-53`); `ExpiresAt` computed at save time (`:90`); `Save`'s
  best-effort compensating `Delete` on a `.meta` put failure (`:98-107`);
  `DeleteExpired` deleting archive (`:204`) then `.meta` (`:207`) with `continue`
  on either failure and no transaction; `extract` writing entries as it walks
  (`:280-311`); `tar next: %w` at `:286`; the agent's own S3 client
  (`internal/config/agent.go:152-155` → `cmd/unified-cd-agent/main.go:204-217`),
  separate from the controller's; the 24 h hardcoded ticker (`scheduler.go:223`).

---

## Invariants

Quoted verbatim from `docs/superpowers/specs/2026-07-29-edge-case-testing-design.md`.

- **I4 (log/artifact integrity)** — `:51`:

  > "**Log/artifact integrity** — a Succeeded run's log line count matches what
  > the workload emitted; no duplicates, no reordering; archives stay readable"

  **Argue this clause by clause and expect the honest answer to be "not
  contradicted".** The W3-5 precedent read *"archives stay readable"* narrowly,
  and the campaign's rule is that an invariant must be contradicted **by its own
  text, not its spirit** (`FINDINGS.md:1509`).
  - *"a Succeeded run's log line count matches what the workload emitted"* —
    **not touched.** Nothing here loses or adds a log line. Every line the
    fixture emits is stored.
  - *"no duplicates, no reordering"* — **not touched.**
  - *"archives stay readable"* — **the clause a careless entry would stretch, and
    it must not be stretched.** The subject of I4 is titled "Log/**artifact**
    integrity"; a cache entry is neither a log nor an artifact (artifacts are the
    `artifacts/<runID>/` prefix and a distinct product concept with its own
    handlers). **If the honest reading is that I4 does not reach a cache
    archive, say so plainly and rest the entry on the contract limb below.** Do
    not manufacture an I4 violation because the scenario's plan line says "I4".
- **I2 (at-most-once side effects)** — `:49`:

  > "**At-most-once side effects** — step side effects execute at most once
  > (detected via an append-only side-effect log on a shared volume, closing the
  > gap `ha_test.go` documents: upserted step reports cannot reveal
  > re-execution)"

  **Considered and expected NOT to be reached.** The question the brief poses is
  whether a later step *observes half-restored inputs and acts on them* — it
  does, and that is the finding, but **I2 is an at-most-once bound and a step
  that runs exactly once on bad input does not violate it.** A zero-vs-once
  shape does not violate I2 either (the W2 ruling). State this and move on.
- **NOT I1.** Every run must reach exactly one terminal state. **Prove it with a
  census**, do not assert it.
- **NOT I3.** No mutex, semaphore or concurrency slot in any fixture here.
- **NOT I5.** Nothing is injected that the system is expected to recover *from*
  — an object is deleted and stays deleted. A permanently corrupted cache entry
  is a data-integrity fact, not a recovery-bound one; W1-6 and W3-5 both drew
  that line and this scenario stays on the same side of it.
- **NOT I6.** No run is terminalised out from under a still-executing agent.
- **I7 (state display consistency)** — `:54`: in scope **only** if a surface is
  found that lies. Candidate to decide deliberately: the run's own log says
  `cache hit` (`orchestrator.go:975`) — **decide whether a `cache hit` INFO line
  after a torn extract is a lying surface, or simply a true statement about the
  lookup**. Read `Restore`'s return values before ruling: on an extract failure
  it returns `(false, err)` (`cache.go:146`), so the orchestrator takes the
  `err != nil` branch and logs `cache restore error`, **not** `cache hit`. Verify
  live before writing either way.

### The contract limbs — surveyed in full, counts printed, before anything is filed

**Three candidate limbs, and they are not equally strong.**

1. **`docs/jobs.md:1407` — the storage-bound promise, and the strongest limb for
   the orphan:**

   > "`ttlDays` is capped at 365 (jobs asking for a longer TTL fail validation at
   > parse time) so a single entry cannot pin itself, and the storage it
   > occupies, indefinitely."

   A payload object with no `.meta` pins its storage **indefinitely** — no
   sweeper can see it, no lookup can expire it, and the exact-key read path will
   keep serving it. Read the surrounding section (`docs/jobs.md:1401-1407`) before
   citing, per the W2-7 lesson.
2. **`docs/high-availability.md:83`** — "Only the leader deletes expired cache
   entries". This is an **arbitration** row in a leader-election table, not a
   latency or completeness promise. **Weak limb; do not lead with it.**
3. **`docs/jobs.md:1397`** — "On hit, the cached directory is restored before the
   step runs." Weigh honestly against `:1399`'s explicit best-effort sanction in
   the very next paragraph. An omission is not a contradiction
   (`FINDINGS.md:1688` is the binding precedent on that).

**Run the survey IN FULL and print the hit count next to each — no `head`
(the W3-3 rule).** Capture to `$SCRATCH/partE-docs-survey.txt`:

    grep -rn -iE "cache" docs/*.md | wc -l
    grep -rn -iE "ttl|expir" docs/*.md | wc -l
    grep -rn -iE "cleanup|garbage|sweep|reclaim|orphan" docs/*.md | wc -l
    grep -rn -iE "partial|corrupt|atomic|all-or-nothing|torn" docs/*.md | wc -l

**And run the already-ruled check on both limbs — the doc passage AND the
finding**, because W3-5 filed a re-file of W2-2 by checking doc text only:

    grep -rn "jobs.md:139\|jobs.md:140\|jobs.md:14\|kubernetes-integration.md:44\|high-availability.md:83" test/edgecase/
    grep -n -iE "cache" test/edgecase/FINDINGS.md

---

## Verified mechanism — every row re-read at this branch's HEAD

| # | Fact | Site |
|---|---|---|
| 1 | **The cache is entirely object-resident.** No `store.Store` in the package, no table, no migration | `internal/cache/cache.go` (whole file) |
| 2 | **Two objects per entry**, keyed `caches/<b64url(sha256(job))>/<b64url(sha256(key))>` + `.tar.zst` / `.meta`, `base64.RawURLEncoding` over the raw digest | `cache.go:49-53` |
| 3 | **TTL is a JSON field in `.meta`**, fixed at save time | `cache.go:88-93` (`ExpiresAt` at `:90`) |
| 4 | **`Restore`'s exact-key path opens a stream and never reads `.meta`** — no TTL check, no owner check, no ETag | `cache.go:141-149` |
| 5 | **`Get` is eager**: minio-go defers the HTTP GET until first Read, so `S3ObjectStore.Get` peeks one byte to force it and to translate `NoSuchKey` into `ErrNotFound` **before returning**. So by the time `extract` starts, the GET is already established | `internal/objectstore/s3.go:62-86` |
| 6 | **`extract` writes as it walks** — `os.Create` + `io.Copy` per entry, inside the `tr.Next()` loop; a mid-stream failure returns from inside that loop with everything written so far left on disk | `cache.go:280-311`, error at `:286` |
| 7 | **A restore error is warn+skip and the step still reports `Succeeded`** — see Correction 1. `markFailed` is unreachable from a restore failure | `orchestrator.go:969-978`, `:1004-1006`; contrast `:388-390` |
| 8 | **The cache SAVE is deferred to a post-hook and runs on whatever is on disk at the end of the run**, under the same key, regardless of what the restore did | `orchestrator.go:984-1001`; `RunPipeline`'s post-hook drain `:680-681` |
| 9 | **No lease, refcount, generation or ETag anywhere in the package.** `ObjectStore` is `Put/Get/Delete/List` and nothing else | `internal/objectstore/objectstore.go`; `cache.go` (no conditional request anywhere) |
| 10 | **`DeleteExpired` deletes archive then `.meta`, `continue` on either failure, no transaction** | `cache.go:202-211` (archive `:204`, meta `:207`) |
| 11 | **Both sweep/lookup loops skip non-`.meta` keys**, so a payload with no `.meta` is invisible to both | `cache.go:188`, `:232` |
| 12 | **`Save` compensates for its own `.meta` failure** with a best-effort `Delete` of the archive it just wrote, and says why in its own comment ("it would leak forever") | `cache.go:98-107` |
| 13 | **`RunCacheCleanup` is the only `Run*` loop with no interval parameter**; ticker hardcoded 24 h, inline | `scheduler.go:219`, `:223` (full enumeration in §"The scheduler lever") |
| 14 | **The agent uses its OWN S3 client and its OWN bucket** (`unified-cd-cache`), separate from the controller's (`unified-cd-logs`), so cache faults are injected on the agent's path | `internal/config/agent.go:152-155`; `cmd/unified-cd-agent/main.go:204-217`; `test/ha/docker-compose.ha.yaml` `&agentcache` |
| 15 | **All four `UNIFIED_CACHE_*` are required, none has a default, and a missing one disables the cache SILENTLY** — the only trace is the absence of the `cache enabled` INFO line | `cmd/unified-cd-agent/main.go:204`, `:215`; `test/edgecase/README.md:119-125` |
| 16 | **TTL floor is 1 day via the DSL** — `0` is silently rewritten to 30 | `orchestrator.go:980-982`; `dsl/parse.go:614-619` |

**The single sentence this scenario tests:** an object-store delete landing
inside a cache restore leaves a partially-populated workspace, the step is
reported `Succeeded` on top of it, the next step builds on it, and the deferred
save writes it back under the same key.

---

## Stack

```bash
cd test/ha
export MSYS_NO_PATHCONV=1          # Git Bash rewrites container paths (W2-5)
export COMPOSE_FILES="-f docker-compose.ha.yaml -f ../edgecase/compose/s3proxy.override.yaml"
docker compose $COMPOSE_FILES up -d --build
```

- **`test/ha` + the S3 interposer overlay.** The interposer is required and it
  is required for exactly one thing: **`inject.sh s3-slow <bytes/s>` is the width
  knob.** It is nginx `limit_rate` on the response body, which holds a
  cache-restore GET open for as long as the arm says. **`s3-latency` is NOT the
  knob** — it is a fixed pre-request connect delay (a black-holed primary with
  `garage:3900` as `backup`), so it widens the gap *before* the body and does
  nothing to the body itself. W3-2 also measured it turning one logical Put into
  several delayed requests.
- **`bigbody.override.yaml` is NOT needed.** Nothing in this scenario posts a
  large body **through the controller LB** — the 16 MiB moves agent→Garage, and
  the three nginx overlays do not stack anyway.
- **`down -v`, not `down`.** `ha-garagedata` is persistent and every claim here
  is about what is or is not in it. A stale `caches/<jobhash>/<keyhash>` turns a
  miss expectation into a hit and would make the whole scenario unfalsifiable.
- **The delete must bypass the interposer.** `mc` talks to `garage:3900`
  directly (alias `garage`), so the throttle applies to the agent's restore
  stream and **not** to the delete that tears it. That asymmetry is what makes
  the timing possible; state it in the entry.

Throughout: `psql` means
`docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "<sql>"`,
`API` means `curl -sS -H "Authorization: Bearer ha-admin-token"` against
`http://localhost:18080`, and `mc` means
`docker compose $COMPOSE_FILES exec -T mc mc`.

**Workload.** `test/edgecase/workloads/cache-torn.payload.json`, job
`edge-cache-torn` — `wipe` → `cache:` (`ttlDays: 1`) → `inspect_deps`. 256 files
of exactly 65536 random bytes plus a `zz-COMPLETE` sentinel that sorts last, so
"how far did the extract get" is a file count and a byte total. See the
fixture's own header for why `cache-user.payload.json` cannot be used (a
~200-byte archive has no window in it).

**A Windows/MSYS trap that has already cost this wave two captures.** With
`MSYS_NO_PATHCONV=1` set, mingw `curl` mishandles MSYS-form file paths — W3-6
saw an `@/c/Users/...` upload send an **empty body and still return 204**, and
W3-5 saw `-D`/`-o` with MSYS-form output paths fail to open, exit 23,
`http_code=000`. **Use Windows-form paths (`C:/Users/...`) for every `curl` file
argument and check `%{size_upload}`, never the status alone.**

---

## BASELINE GATE — do not proceed past a failing check

```bash
SCRATCH="<scratchpad>/w3-1" ; mkdir -p "$SCRATCH"
```

1. **G0 — worktree.** `git rev-parse --show-toplevel` is `.../wt-edge-spec`,
   branch `plan/edge-case-w3`. `docker compose ls` shows the developer stack
   (project `unified-cd`) present and untouched. **STOP** if the toplevel is the
   main checkout. → `$SCRATCH/gate.txt`.
2. **G1 — the agent cache is actually ON.** Both agents must log the
   `cache enabled` INFO line (`main.go:215`). **STOP on its absence** — with a
   nil `CacheStore` every cache step is a silent no-op that still reports
   `Succeeded`, and the entire scenario would measure nothing (fact 15).
   → `$SCRATCH/gate-g1-cache.txt`.
3. **G2 — the interposer is live and both sides go through it.** All three
   controllers `UNIFIED_S3_ENDPOINT=s3proxy:3900`, both agents
   `UNIFIED_CACHE_ENDPOINT=s3proxy:3900`; `inject.sh s3-probe` answers.
   → `$SCRATCH/gate-g2-proxy.txt`.
4. **G3 — clean slate, object side included.** `SELECT count(*) FROM runs` → 0;
   `mc ls --recursive garage/unified-cd-cache/` → **empty** (the bucket may not
   exist yet, which is also clean). **If it is not empty the previous teardown
   was not `-v`.** → `$SCRATCH/gate-g3-clean.txt`.
5. **G4 — a REAL cache hit, end to end, before anything is injected.** Post
   `edge-cache-torn`, trigger it twice, and confirm run 2 reports
   `CACHE-PRESENT complete=yes` with `entries=257`, `SHORT-FILES=0` and a
   `planted-at` equal to **run 1's** `planted-at`. **The `wipe` step makes this
   falsifiable** — the directory is removed before the cache step, so a hit can
   only have come from the object store. Confirm both objects exist via `mc` and
   record the exact key. **STOP if run 2 misses** — every arm below is timed
   against a working restore. → `$SCRATCH/gate-g4-hit.txt`.
6. **G5 — record the object key and its size**, computed independently two ways
   and required to agree: (a) `mc ls` of `caches/`, (b)
   `b64url(sha256("edge-cache-torn"))/b64url(sha256("edge-cache-torn-v1"))` per
   `cache.go:49-53`. **A mismatch means the qualified job name is not the bare
   name and every later `mc rm` would hit the wrong key.**
   → `$SCRATCH/gate-g5-key.txt`.
7. **G6 — retention, trim and cleanup are all quiet.** All three controllers log
   `run retention disabled (keep forever)`; confirm **no** cache-cleanup activity
   in the session (fact 13 says the first tick is at t+24 h). **Report this as an
   absence over the session window, never as "never"** — the window is one the
   runbook ends itself. → `$SCRATCH/gate-g6-sweepers.txt`.
8. **G7 — API 500s.** This rig has been intermittently returning 500s. Record,
   per request, how many attempts it took. A 500 on a trigger is **not** a
   finding of this scenario; a 500 mid-measurement invalidates that attempt,
   which is discarded and re-run, not reasoned around.

---

## Part A — the torn restore, and what is left on disk

**Deliverable:** a run whose `restore_deps` step is reported **`Succeeded`** while
its workspace holds a **partial** `deps/`, whose next step reads that partial
directory, and whose deferred save writes it back under the same key.

**A1 — arm the width knob and probe-confirm it.** `inject.sh s3-slow 262144`
(256 KiB/s → a ~16 MiB archive is a ~64 s stream). **Confirm the arm per
request, not per wall clock**: `inject.sh s3-show`, then time an `mc cat` of a
known object **through the proxy** against the same read **direct to Garage**,
and record both durations. An arm that did not take is the W2-5 trap and it has
been caught live twice this wave. → `$SCRATCH/partA-arm.txt`.

**A2 — start the restore.** Trigger `edge-cache-torn`. Record the run id and
which agent claimed it (`SELECT claimed_by`). **Everything below targets that
agent's container.**

**A3 — detect that the extract has actually begun, and do not use a sleep.**
Poll the claiming agent's workspace `deps/` directory
(`docker compose exec <agent> sh -c 'ls -1 <workspace>/deps | wc -l'`; locate the
workspace once with `find / -maxdepth 4 -name edge-cache-torn`). Wait until the
count is comfortably non-zero and still rising — **two consecutive rising
samples, both captured** — which is the proof the extract is mid-flight rather
than done or not started. → `$SCRATCH/partA-progress.txt`.

**A4 — tear it.** `mc rm garage/unified-cd-cache/<archive key>` **direct to
Garage, not through the proxy**. Stamp the instant before and after. Immediately
re-sample the `deps/` count. → `$SCRATCH/partA-delete.txt`.

**A5 — the four measurements, each from a named capture.**
1. **What error surfaced.** The claiming agent's container log for the window:
   `cache restore error` (`orchestrator.go:973`) with the wrapped chain from
   `cache.go:146` / `:286`. **Capture it verbatim; do not paraphrase a log line
   you did not capture.** If instead the log says `cache hit`, that is the
   result and Part B's backend answer explains it.
2. **Whether the step failed.** `SELECT status FROM step_reports WHERE run_id=…`
   → expect `restore_deps` **`Succeeded`** (fact 7, Correction 1). Record the
   run's status too.
3. **Whether the workspace is partially populated — the measurement that
   matters.** `inspect_deps`'s own output: `STATE entries=<n> complete=<yes|no>
   bytes=<b>` plus `SHORT-FILE` lines. A torn extract gives `0 < n < 257`,
   `complete=no`, and typically one truncated file. **This is a later step
   reading half-restored inputs and acting on them**, which is the brief's
   central question, and it is answered by the fixture's own stdout — no
   instrument.
4. **What the deferred save then did.** After the run ends, `mc ls` and
   `mc stat` the archive key. If a new object is there and it is materially
   smaller than G5's, **the cache entry has been silently replaced by the
   partial one**. Trigger a **third** run and record what it reports: a
   `CACHE-PRESENT complete=no` on a clean workspace is persistent corruption
   with no fault injected at all. → `$SCRATCH/partA-poison.txt`.

**Falsification, and take it seriously.** If the restore **completes normally**
after the delete, Part A has no torn state and Part B's backend answer is the
reason. **Do not retry blindly** — read Part B's result first, then either widen
the window (slower `s3-slow`, bigger payload) or record the negative honestly
and say the window is not reachable through this backend. **Cap: 3 attempts.
Report the count either way.**

---

## Part B — what Garage does to an in-flight GET whose key is deleted

**This is listed NOT ESTABLISHED in the plan (`:139`), and the answer is a fact
about the BACKEND, not about unified-cd. Label it that way everywhere it
appears.**

**B1 — the clean measurement, with no unified-cd in it and no proxy in it.**
From the `mc` container, start a throttled read of the cache archive **direct to
Garage** (`mc cp --limit-download 256K garage/unified-cd-cache/<key> /tmp/x`; if
that flag is unavailable on this `mc` build, fall back to reading through the
s3proxy with `s3-slow` armed **and say so**, because nginx then sits in the
path). While it is running, `mc rm` the same key from a second `exec`. Record:
the reader's exit code, the bytes it ended up with versus the object's size, and
any error text. → `$SCRATCH/partB-backend.txt`.

**B2 — state the answer in one sentence, as a backend fact**, in the form:
"Garage v2.3.0, single-node, `--default-bucket`: an object DELETE issued while a
GET of the same key is streaming **does / does not** terminate the in-flight
response; the reader observed *<exact outcome>*." Name the version from the
compose file. **Do not generalise it to S3, to AWS, or to MinIO** — one backend,
one version, one configuration.

**B3 — reconcile it with Part A.** If the backend completes in-flight GETs, then
the `cache.go` window is real in code and **unreachable against this backend**,
and Part A's torn state has to be produced another way or recorded as not
reproduced. **Say which**, with the W2-3 Arm D precedent (a 0/10 negative filed
with an explicit "not reproduced live" label was accepted). If the backend does
sever the stream, Part A's result stands on its own.

---

## Part C — the two half-done states, and the TTL that is not enforced

**Each state is produced by hand with `mc`, then observed through a real run.
Run them one at a time and wipe the cache prefix between them**, or the previous
state's objects answer the next state's lookup.

**C1 — payload with no `.meta` (the leaking state).** Save a fresh entry (a run),
then `mc rm` **only** the `.meta`. Trigger a run.
- Expect **`CACHE-PRESENT complete=yes`** — a hit, from the exact-key path,
  which never reads `.meta` (fact 4, Correction 4).
- Confirm the object is invisible to the other two consumers by inspection of
  their code paths (`cache.go:188`, `:232`) — both `continue` on non-`.meta` —
  and by `mc ls`, which shows the payload alone.
- **The finding is the conjunction**: the entry is served indefinitely, has no
  expiry anything can read, and no code path in the tree can ever delete it.
  → `$SCRATCH/partC-payload-orphan.txt`.

**C2 — `.meta` with no payload (the benign state).** Fresh entry, `mc rm` only
the `.tar.zst`. Trigger a run.
- Expect a **clean miss** — `CACHE-MISS` and a regenerated `deps/`.
- **Say which code path produced the miss**, per Correction 3: with a live
  `ExpiresAt` the miss comes from the fallback `Get`'s `ErrNotFound`
  (`cache.go:162-167`), **not** from `findBestMatch`'s expiry skip (`:244`). The
  fixture has `restoreKeys`, so `findBestMatch` really is entered — confirm from
  the agent log which branch was taken, or state that the two are
  indistinguishable from outside and rest on the code read.
  → `$SCRATCH/partC-meta-orphan.txt`.

**C3 — an expired `.meta` with its payload intact (Correction 5).** Fresh entry;
`mc cp` the real `.meta` out, rewrite **only** `expiresAt` to a past instant,
`mc cp` it back. Trigger a run.
- Expect a **full hit**, because the exact-key path never reads it.
- **This is the sharpest cheap result in Part C**: `ttlDays` does not bound how
  long an entry is served. It bounds only when a 24 h sweeper *would* delete it,
  and that sweeper's first opportunity is t+24 h after a controller starts.
  → `$SCRATCH/partC-expired-meta.txt`.

**C4 — the asymmetry, stated as a table** with, for each of the three states,
what `Restore` exact-key does, what `findBestMatch` does, what `DeleteExpired`
does, and whether anything can ever reclaim it.

---

## Part D — the deletion order

**Code-read plus the end-states Part C already produced. Say so explicitly: the
sweeper never ran on this rig** (fact 13; G6 records the absence over the session
window).

- `DeleteExpired` deletes the **archive first** (`cache.go:204`) and the `.meta`
  **second** (`:207`), `continue` on either failure, no transaction.
- An interruption between them therefore leaves **`.meta` with no payload** —
  Part C2's **benign** state — and never the payload-with-no-`.meta` state Part
  C1 showed is unreclaimable. **The order is the safe one.**
- **Say whether it looks deliberate.** Weigh: the file carries no comment
  justifying the order at `:202-211`, but `Save` **does** carry an explicit one
  for the mirror-image case (`:99-102`: "without its `.meta` it is invisible to
  both lookup and GC (which iterate `.meta` only), so it would leak forever"),
  and it compensates accordingly. **A codebase that reasons about exactly this
  hazard in `Save` and then orders `DeleteExpired`'s two deletes the matching
  way is most naturally read as deliberate-but-undocumented.** Argue it; do not
  assert it. **The inline comment at `:99-102` is not a contract** — the campaign
  rule (`FINDINGS.md:479`) admits an exported API field, a schema column, or a
  statement in `docs/`, and a comment inside a function body is none of those.
  Cite it as evidence of intent only.

---

## Part E — the contract survey

Run §"The contract limbs" in full, capture untruncated with hit counts printed,
and run **both** already-ruled checks (doc passage *and* the finding itself).
→ `$SCRATCH/partE-docs-survey.txt`, `$SCRATCH/partE-already-ruled.txt`.

---

## Teardown

```bash
../edgecase/tools/inject.sh s3-clear
docker compose $COMPOSE_FILES exec -T mc mc ls --recursive garage/unified-cd-cache/
docker compose $COMPOSE_FILES ps
docker compose $COMPOSE_FILES down -v
```

- **Clear every interposer arm first and confirm it** (`s3-show` empty,
  `s3-probe` 200). Leaving `s3-slow` armed would silently throttle the next
  scenario's object store.
- **Cancel every surviving run** and confirm zero non-terminal runs in a census.
- **Kill every background sampler and *capture* that, do not assert it.** Keep
  PIDs in `$SCRATCH/samplers.pid`, `kill` them, show `jobs` empty and
  `ps -W | grep -iE "curl|psql|mc"` matching nothing — **and check inside the
  containers too**, because a `docker compose exec` sampler outlives the shell
  that launched it and appears in neither (the W3-4 lesson). The `mc` image has
  no `ps`; record that rather than hiding it. → `$SCRATCH/teardown.txt`.
- **`down -v`, not `down`.**
- **Scrub any credential from `$SCRATCH` before finishing** — W3-5 left a full
  `uca_` token in a dotfile. Grep for `uca_`, `ha-admin-token` and
  `garageadmin12345` and record the result.
- Copy `$SCRATCH` into the campaign evidence root at the wave checkpoint.

---

## Recording rules

- **Lead with the sanction.** `docs/jobs.md:1399` and
  `docs/kubernetes-integration.md:446` explicitly make restore best-effort. An
  entry that presents "the step did not fail" as the defect is wrong.
- **Judge I4 clause by clause, quoted verbatim, and expect "not contradicted".**
  Do not stretch "archives stay readable" onto a cache archive. If the entry
  rests on the contract limb, say so in the Invariant line itself.
- **Report Part B as a BACKEND fact**, named version, named configuration, not
  generalised.
- **Every number cites a `$SCRATCH` filename whose time window covers it.**
  Derived figures say "derived"; code-read figures say "code-read"; uncaptured
  live observations say `(observed live, raw output not captured to
  scratchpad)`. **Do not present verbatim-looking log text that was not
  captured**, and do not call an attribution a "bracket" unless a capture covers
  that specific request.
- **Report every capped arm's attempt count either way**, with the cap stated.
- **When claiming a class is fully enumerated, paste the enumeration** — the
  §"scheduler lever" table and the three-state table in Part C are both such
  claims (the W3-6 lesson).
- **Observation entries say "observation" in the title** and repeat it in the
  Severity line as `minor (observation)` (`FINDINGS.md:481`).
- **A negative measured over a window the runbook itself ended is an absence,
  not a "never".**
