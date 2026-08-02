# W6-2b — the `LogPusher` request-count curve under outage, and the disk-spill gate

**Wave W6, Task 3.** The agent side of the same pipe W6-2a measured from the
controller side, and the carrier of the campaign's oldest deferred decision:
`FINDINGS.md:496` gates the `LogPusher` **disk-spill** design candidate
(`docs/superpowers/specs/2026-07-29-edge-case-testing-design.md:199-207`) on
"W6-S2 measurement". **A recommendation is this scenario's deliverable**, and it
is written in the `THE DISK-SPILL GATE` section at the end, stated as a
recommendation with its cost and its non-fixes.

The mechanism this scenario measures — `flushLocked` re-issuing one
`AppendLogBulk` per pending batch per tick, so tick *k* costs *k* requests — is
**already filed** as the W1-6 amplification observation (`FINDINGS.md:465`),
derived there from a model that reconciled to within 1 request of a measured
20,447. **This scenario does not re-file it.** What it adds is the thing that
model could not produce: the curve measured directly, in **four different fault
regimes**, of which two have never been run in this campaign.

**Invariants attacked: I4, I5 — and only if contradicted by their own text.**
Verified at HEAD, `docs/superpowers/specs/2026-07-29-edge-case-testing-design.md`:

- **I4 is line 51**, verbatim: *"**Log/artifact integrity** — a Succeeded run's
  log line count matches what the workload emitted; no duplicates, no
  reordering; archives stay readable"*.
- **I5 is line 52**, verbatim: *"**Bounded recovery** — after fault injection
  the system returns to steady state within documented bounds (leader
  re-election ≤ seconds; stuck-run reap ≤ staleAfter 90s + interval 30s; the
  bounds in `docs/high-availability.md` are the contract)"*.

**The brief's `:44-55` is a range, not a citation, and the range is right while
the line numbers inside it are not what a reader would guess**: the table
header is at `:46-47`, so I1..I7 occupy `:48-54` and `:55` is blank. Both
invariants quoted above are inside the brief's range. Recorded because three W4
runbooks got this citation wrong and one quoted a clause that is not in the
spec; this one quotes both invariants verbatim so a triager never has to trust
the line number.

**Direction check, applied before anything was filed.** I5's own text bounds
*recovery after* a fault, against bounds published in
`docs/high-availability.md`. Every cost measured here is incurred **while the
fault is ongoing**, which is a different property — the same reasoning
`FINDINGS.md:473` already applied to the W1-6 entry. So I5 is **not** available
as a violation limb for the request-count results, and this scenario says so
rather than stretching it. I4 *is* available and is checked in every arm.

---

## Corrections to inherited facts, established BEFORE execution

Per the standing carry-forward rule the brief's mechanism block is a set of
**claims**. All of them were re-read at this branch's HEAD.
**The pattern held for an eighth consecutive wave: every `file:line` claim is
correct.** The mechanism claims are correct too — with three amendments, one of
which is the load-bearing prediction for Arm 4.

### The `file:line` claims — every one holds

| Claim | Verified at HEAD |
|---|---|
| `LogPusher` is `internal/agent/runner.go:207-446` | **HOLDS** as a region. `:207` is the doc comment on `logPusherAutoFlushEvery`; `:446` is the closing brace of `pendingSizeBytes`. The struct itself is `:228-245` |
| `flushLocked` `:358-366` issues one `AppendLogBulk` per pending batch per tick | **HOLDS, with a precision.** `func (p *LogPusher) flushLocked` is line **358** and its closing brace is line **425**; `:358-366` names the *pending-resend loop* (`for _, b := range p.pending` at `:361`, `AppendLogBulk` at `:362`, `p.pending = stillPending` at `:366`), not the function. Cite `:360-366` for the loop and `:358-425` for the function |
| Partial-progress accounting exists at **batch** granularity, absent **within** a batch, and there is no coalescing | **HOLDS.** `:363` appends only the failing batch to `stillPending`; a batch that returns `nil` is dropped. There is no code path anywhere in the file that merges two `pendingBatch` values, and `AppendLogBulk` (`client.go:300-304`) is one POST per call |
| Pending capped at 1 MiB of **line text only** (`:256`), `pendingSizeBytes` `:438-446` sums `len(r.Line)` and nothing else | **HOLDS.** `maxPendingBytes: 1 << 20` at `:256`; `total += len(r.Line)` at `:443` — no timestamp, no run id, no step index, no stream, no JSON framing |
| Drop-oldest, and only while `len(p.pending) > 1` | **HOLDS.** `:431`: `for len(p.pending) > 1 && p.pendingSizeBytes() > p.maxPendingBytes`. The newest batch is retained unconditionally |
| Auto-flush every 2 s (`:211`), plain `time.NewTicker`, no backoff (`:272-287`) | **HOLDS.** `logPusherAutoFlushEvery = 2 * time.Second` at `:211`; `t := time.NewTicker(every)` at `:274`; the `select` at `:277-284` has exactly two cases, `ctx.Done()` and `t.C`. No sleep, no jitter, no error inspection — `flushCompleteLinesLocked` returns nothing |
| Write-path flush at 4 KiB (`:255`), bounded by 5 s (`:219`) | **HOLDS.** `flushBytes: 4 << 10` at `:255`; `logPusherWriteFlushTimeout = 5 * time.Second` at `:219`, applied at `:324` |
| Step-end `Flush` `:335-353`, ≤3 retries 1 s apart, 5 s total | **HOLDS.** `context.WithTimeout(context.Background(), 5*time.Second)` at `:343`; `for i := 0; i < 3 && len(p.pending) > 0; i++` at `:345` |
| The drop marker `:401-424` is attempted **only** when `len(p.pending) == 0` | **HOLDS.** `if len(p.pending) == 0 && p.droppedLines > 0` at `:401`. It is step 3 of `flushLocked`, so it is evaluated after every flush and skipped whenever any backlog survives |
| Agent client timeout is `internal/agent/client.go:53` | **HOLDS.** `httpClient = &http.Client{Timeout: 60 * time.Second}` is exactly line 53 |
| `StartAutoFlush` passes the step context with no per-flush deadline (`:282`) | **HOLDS, and the call site confirms the provenance.** `backend_host.go:378` builds `flushCtx, stopAutoFlush := context.WithCancel(ctx)` — `WithCancel`, not `WithTimeout` — and hands `flushCtx` to both pushers at `:379-380`. `:282` passes that same ctx straight into `flushCompleteLinesLocked` |

### The observability enumeration — re-verified rather than inherited, and it is complete

The brief instructed this be re-run rather than inherited. All three limbs hold,
and the third is stronger than the brief states.

- **Zero `slog`/`log` statements in the whole `LogPusher`.** `runner.go:207-446`
  contains no `slog.`, no `log.`, no `fmt.Print*`. The only `fmt` use in the
  region is `fmt.Sprintf` building the marker's text at `:404`. A flush failure
  is therefore invisible in the agent log: `flushLocked` discards every `err`
  into a boolean.
- **`droppedLines` (`:244`) is unexported with no accessor.** `grep -rn
  "droppedLines" --include=*.go .` returns **17** matching lines: 7 in `runner.go` and 10
  in `runner_test.go`. No getter, no metric, no log line. The count is legible
  **only** through the marker line itself — i.e. only in the case where it is
  already known to have been delivered.
- **The host agent exposes no metrics endpoint — and in fact binds no HTTP
  listener at all.** Stronger than "no metrics": `grep -rn
  "ListenAndServe\|net.Listen\|http.Handle\|NewServeMux" --include=*.go
  cmd/unified-cd-agent internal/agent` returns **85** hits and **every one is in
  a `_test.go` file** (hit count reported, not truncated;
  `w6-2b/codesurvey.txt`). `grep -rc promhttp internal/agent` is **0**. So there
  is no surface on which a `droppedLines` gauge could be exposed without adding
  a server first.

### AMENDMENT 1 — the two regimes are mutually exclusive, and the charter assumed they were one workload

The spec's W6-S2 line asks for "drop-marker frequency and lost-line counts
under sustained load" as a single quantity. It is two, and the 1 MiB **line
text** cap is what separates them:

- **Chatty regime.** The write path flushes at 4 KiB (`:255`), so each pending
  batch holds ~4 KiB of line text. 1 MiB / 4 KiB = **~256 batches** before
  drop-oldest engages. Reachable in seconds. This regime yields the drop marker
  and a **plateau** in per-flush request cost — not a triangle.
- **Sparse regime.** A 1 line/s trickle of ~45-byte lines makes each 2 s tick's
  batch ~90 bytes, so the cap is ~**11,650 batches ≈ 6.5 hours** of outage. Any
  realistic outage is **purely quadratic and never drops** — exactly what W1-6
  observed (`grep -rc "dropped"` = 0 across its whole evidence set).

**No single workload produces both numbers**, and this is stated as a result
rather than worked around. Arm 1 measures the curve; Arm 2 measures the drop.

### AMENDMENT 2 — the quadratic regime needs a *total* outage, and the arithmetic says a partial one is bounded at a small constant

This is derived before Arm 3 rather than after it, so the arm is a test of a
prediction and not a description of whatever happened.

Let `p_k` be the pending batch count at tick *k* and `s` the per-request success
probability. Each tick retries all `p_k` batches independently, then appends one
new batch which itself fails with probability `1-s`:

```
E[p_{k+1}] = (1-s)·p_k + (1-s)  =>  fixed point  p* = (1-s)/s
```

- `s = 0` (total outage): no fixed point, `p_k = k`. **Quadratic.**
- `s = 0.5` (Arm 3): `p* = 1`. Per-tick request cost `~2`. **Bounded.**
- `s = 0.1` (a 90%-failing intermediary): `p* = 9`. Still **bounded**.

**The quadratic is a property of the total outage, not of the retry policy
under stress**, and the recommendation has to be argued on that basis rather
than on the shape alone. Arm 3 tests `s = 0.5` against `p* = 1`.

### AMENDMENT 3 — `logPusherWriteFlushTimeout` cannot bound the write path's stall, and this is Arm 4's prediction

`logPusherWriteFlushTimeout`'s own doc comment (`:213-219`) states the mitigation
it provides: *"bounds how long a synchronous flush triggered from Write (on
crossing the flushBytes threshold) may block holding `p.mu`. Without a bound, a
controller partition could stall the writer (and thus the running step) for as
long as the underlying HTTP client takes to give up."*

Read against the code, the bound is applied **inside** the critical section:

```go
func (p *LogPusher) Write(b []byte) (int, error) {
        p.mu.Lock()                      // :320  <- the unbounded wait is HERE
        defer p.mu.Unlock()
        n, _ := p.buf.Write(b)
        if p.buf.Len() >= p.flushBytes {
                fctx, cancel := context.WithTimeout(context.Background(), logPusherWriteFlushTimeout)
                p.flushLocked(fctx)      // :325  <- and the bound is HERE
                cancel()
        }
```

So it bounds a flush `Write` **itself** starts, and does nothing about `Write`
waiting for `p.mu` while the **auto-flush goroutine** holds it. That goroutine's
`flushLocked` runs under `flushCtx` — `context.WithCancel`, no deadline
(`backend_host.go:378`) — and issues `len(p.pending)` sequential
`AppendLogBulk` calls, each bounded only by the client's own 60 s
(`client.go:53`).

**Prediction: against a controller that accepts the connection and never
answers, one auto-flush tick holds `p.mu` for `len(p.pending) × 60 s`, every
`Write` blocks for that whole time, the agent stops draining the step's stdout
pipe, and once the 64 KiB pipe buffer fills the step's own process stalls.**
The bound named in the comment does not apply on that path. Arm 4 measures it.

**This is not filed as a violation even if it reproduces**, and the reason is
the campaign's own classification rule (`FINDINGS.md:479`): the promise is made
in the doc comment of an **unexported package-level var**, which is not a
published contract. It is an observation.

---

## The rig

`test/ha` plus two overlays:

```bash
cd test/ha
export COMPOSE_FILES="-f docker-compose.ha.yaml \
  -f ../edgecase/compose/w62b.override.yaml \
  -f ../edgecase/compose/ctrlports.override.yaml"
docker compose $COMPOSE_FILES up -d --build
```

- **`compose/w62b.override.yaml` is new**, and it is a rig overlay, not a
  product change. It does three things: swaps `test/ha/nginx.conf` for
  `compose/nginx-w62b.conf`, adds the `logsink` service Arm 4 needs, and binds
  `../edgecase/sideeffect-data:/data` into both agents (the same bind
  `oneway.override.yaml:11,14` provides) so a fixture can record its own
  progress on a channel that does not pass through the `LogPusher`.
- **`compose/nginx-w62b.conf` is new and is a derivative of
  `compose/nginx-logfault.conf`**, not a replacement for it. It keeps that
  file's `logfault` access-log format byte-for-byte (so
  `tools/w6/w6-reqshape.sh` works unchanged and does not exit 4), keeps the
  URI-scoped bulk location and its runtime-writable include, and adds exactly
  what W6-2b's four arms need and W3-4's two arms do not: a `split_clients`
  map for the flapping arm, a `hangsink` upstream for the black-hole arm, and
  it moves `proxy_connect_timeout` out of the bulk location so an arm can set
  it. **`nginx-logfault.conf` is left untouched so W3-4 stays reproducible.**
- `ctrlports.override.yaml` publishes 18081/18082/18083. Used for the health
  and connection checks only; every fault here is URI-scoped at the LB, so the
  agents keep using `http://nginx:8080` exactly as the base rig configures them.

### The fault, and why the existing W3-4 verbs are not sufficient

`tools/w3/w3-4-logfault.sh` was assessed first, as the brief instructed. Its
two arms are **wrong for W6-2b, and would have produced a contaminated curve**:

- `truncate` cuts `proxy_read_timeout` so nginx 504s **mid-loop**, leaving the
  prefix of the batch **already committed** upstream.
- `lostack` **mirrors the request to a real controller**, which commits the
  **whole** batch, and only the client-facing leg fails.

Both are deliberately *partial-commit* faults, because W3-4 was measuring
duplication. W6-2b needs the opposite: a failure in which **nothing reaches a
controller**, so that the resend curve is a curve of resends and not a curve of
duplicate inserts. Neither verb provides it, so this scenario adds
`tools/w6/w6-2b-fault.sh` with three new arms — `outage`, `flap`, `hang` —
writing into the same `/etc/nginx/logfault/fault.conf` include. It reuses
W3-4's `clear` semantics verbatim and carries W3-4's own probe discipline.

Stated per the brief's instruction rather than quietly: **this is a new fault,
not a rebuilt instrument.** Every *measurement* instrument in this scenario is
Task 1's or Task 2's, used as-is.

### Harnesses

- `tools/w6/w6-reqshape.sh` — `follow` to capture, `shape` to fold into a
  per-bucket curve. The request-count curve **is** this scenario's headline, so
  this is the load-bearing instrument. Its `counter` verb is used once as a
  cross-check that the outage arms really did keep every request away from the
  controllers.
- `tools/w6/w6-pgsample.sh -p` — one grid across the two heaviest arms, to
  check the arms do not trip the connection pressure `FINDINGS.md:2517`
  records. **That entry is cited, not re-filed**, and its "TOTAL backends … of
  max_connections" line over-reads because it counts background workers
  (W6-2a's correction) — so any saturation claim is checked against the
  `datname` split first.
- `tools/w6/w6-synth-agent.sh` — not used. Every arm here needs a **real**
  agent, because the quantity under measurement is a real `LogPusher`'s
  behaviour and a synthetic agent has none.

### Fixtures

- **Arm 1 / Arm 3: `workloads/w6-trickle.yaml`** (`edge-w6-trickle`), Task 1's
  fixture, used as-is. Its own header already argues why 1 line/s is the sparse
  regime.
- **Arm 2 / Arm 4 / Part E: `workloads/w6-chatty.yaml`** (`edge-w6-chatty`), new.
  Nothing existing reaches the 1 MiB cap: `logburst` is 2,000 lines of ~10 bytes
  = ~20 KB, i.e. **2 % of the cap**, and `w6-trickle`'s own header says not to
  use it for the drop marker. The new fixture emits **wide** lines (default
  1 KiB) so that ~1,000 lines fill the cap, and — the part Arm 4 needs — it also
  appends each line's index and timestamp to a file under `/data`, a channel
  that does not pass through the `LogPusher` at all. **A gap in that file is a
  stall of the step's own process**, measured independently of anything the
  logging path reports.

---

## Arms, with predictions stated before the first capture

Every arm: trigger, wait for `Running`, arm the fault, hold for the stated
window, clear, let the run finish, then read the curve from the access log and
the line accounting from the API.

### Arm 1 — the quadratic regime (sparse workload, total outage)

`edge-w6-trickle lines=600 interval_s=1`, fault `outage` armed ~30 s in and held
**300 s** = **150 ticks**.

| Quantity | Prediction | Basis |
|---|---|---|
| Requests at tick *k* | *k* | `flushLocked` `:360-366`, one call per pending batch |
| Cumulative over the outage | `150·151/2` = **11,325** | triangular |
| Peak instantaneous rate | 150 requests in one tick = **75 req/s** | derived; compare W1-6's derived ~67 req/s |
| Pending bytes at clear | ~150 × ~90 B ≈ **13.5 KB**, 1.3 % of the cap | `pendingSizeBytes` counts line text only |
| Drops | **0** | cap never approached |
| Drop marker | **never fires** | `droppedLines` stays 0 |
| Recovery | one flush issues ~150 requests and drains to 0 | no coalescing exists |
| I4 at the end | 600 lines + 2, contiguous `seq`, no duplicates | nothing was committed during the outage |

### Arm 2 — the drop regime (chatty workload, total outage)

`edge-w6-chatty lines=2000 pad_bytes=1024 interval_s=0` ≈ **2.05 MiB**, fault
`outage` armed before the burst and cleared **before** the step ends.

| Quantity | Prediction | Basis |
|---|---|---|
| Batch size | ~4 KiB, ~4 lines | write-path flush at `flushBytes` `:255` |
| Batches to fill the cap | **~256** | 1 MiB / 4 KiB |
| Per-flush request cost | rises to ~256 then **plateaus** | drop-oldest holds `len(p.pending)` at ~256 |
| Total batches created | ~512 | 2.05 MiB / 4 KiB |
| Lines dropped | ~(512-256) × 4 ≈ **1,024** | one eviction per new batch past the cap |
| Total requests | `256·257/2 + 256·256` ≈ **98,000** | triangle then rectangle |
| Drop marker | **fires once**, on the first successful flush after clear, carrying the full count | `:401` precondition `len(p.pending)==0` is met after the drain |
| I4 | **violated by design of the fault, not by the product** — lines are lost, and the marker is the product's documented answer to that | the finding, if any, is about whether the marker is *reachable*, which is Part E |

**Watch item:** the write path's flush is bounded at 5 s (`:219`). At ~256
requests per flush the arm needs each request to cost well under 20 ms or the
budget truncates the resend and the plateau is measured short. The access log's
`rt=` column is the check.

### Arm 3 — the flapping fault (the arm closest to a real outage)

`edge-w6-trickle lines=600 interval_s=1`, fault `flap` (per-request 50 %
split on `$request_id`) held **300 s**.

Predicted from Amendment 2: **not quadratic.** Fixed point `p* = (1-s)/s = 1`,
so per-tick cost `~2` requests and cumulative `~300` over the window, against
Arm 1's 11,325 — a **~38× difference from the same outage duration**. Zero
drops, no marker, I4 exact.

**If this holds, it is the single most important input to the gate**, because it
says the quadratic is not what an operator meets during an ordinary rolling
restart or a partial partition; it is what they meet during a total, sustained
outage.

### Arm 4 — the black-hole partition. Never exercised in this campaign

W1-5 and W1-6 both used nginx 403s and W3-4 both of its arms; **all four
fail fast**. This arm makes the bulk endpoint **accept the connection and never
answer**.

**How the black hole is achieved, stated plainly because a fake one is worse
than none.** The agent image has no `iptables` and no `NET_ADMIN` (W1
established this), so no packet-level drop is available. Instead the bulk
location's `proxy_pass` is pointed at `logsink`, a container whose only job is
to hold an accepted TCP connection open and never write to it
(`nc -l` behind a `tail -f /dev/null`), with `proxy_read_timeout 600s` so nginx
does not answer on the agent's behalf.

**What that is equivalent to, and what it is not.** *Equivalent* in everything
this arm measures: the agent's request is accepted, sent, and then never
answered, so `AppendLogBulk` blocks until the client's own 60 s timeout
(`client.go:53`) — which is exactly the condition Amendment 3 is about.
*Not equivalent* in three respects, all of which are stated in the results:
the TCP handshake **succeeds** (a real black hole would hang in `connect`, and
Go's dialer would apply its own timeout there instead), there are no
retransmit-backoff dynamics, and the socket is genuinely alive so TCP keepalive
never fires. **What that leaves unmeasured** is written out in the results
rather than glossed.

Predictions:

| Quantity | Prediction |
|---|---|
| Per-request duration | **~60 s**, the client timeout |
| Access-log signature | `status=499` (client closed) with `rt≈60` — the direct proof the agent held the request 60 s and nginx did not answer it |
| Mutex hold per auto-flush tick | `len(p.pending) × 60 s`, unbounded (Amendment 3) |
| Effect on `Write` | blocks for that entire time; `logPusherWriteFlushTimeout` does **not** apply |
| Effect on the step | the agent stops draining stdout; once the 64 KiB pipe fills, the step's own process stalls — **visible as a gap in the `/data` heartbeat file** |
| Request **count** | **collapses** — roughly one request per 60 s per pending batch. The hang regime is *cheap* for the controller and *expensive* for the step, the exact inverse of Arms 1-2 |

### Part E — the marker's reachability

`edge-w6-chatty` with the `outage` fault armed **through the step's natural
end** and never cleared. Predicted: the step-end `Flush` (`:335-353`) spends its
5 s budget, `p.pending` is still non-empty, the marker's `len(p.pending)==0`
precondition at `:401` is never met, the process discards the pusher, and the
loss is **permanently invisible** while the run reads `Succeeded`.

**This is the W1-2 major (`FINDINGS.md:179`) reached from the other side, and it
is CITED, NOT RE-FILED.** W1-2 lost a tail because the 5 s budget expired; this
part adds the marker's reachability condition to that picture: not only is the
tail lost, the mechanism built to announce the loss is structurally unable to
fire in exactly the case where the loss is largest.

---

## RESULTS

Executed 2026-08-02 `06:02Z – 06:55Z` on branch `plan/edge-case-w6`. Five runs,
five bounded nginx access-log windows, ~141,000 request rows. Raw captures are
under `w6-2b/<arm>/` in the evidence root; every number below traces to a
capture whose window covers it, and **every window is bounded on both ends by
`w6-reqshape.sh window`'s `-window.txt` sidecar** — see the instrument defect
below for why that sentence had to be earned.

### Two instrument defects found and fixed before any number was trusted, and the second one is the more important lesson

**1. `w6-reqshape.sh follow -d` could not stop its own capture.** The same
defect Task 2 found in `w6-idleload.sh` and fixed *there*, still live in the
instrument this scenario's headline rests on. Measured before fixing
(`w6-2b/harness/`): `follow -d 5` printed `captured 34 lines`; eight seconds and
twenty unrelated requests later the same file held **56**, and it never
stopped — the same file was **44,151,788 bytes** when it was archived at the
end of the session, having absorbed every arm of this scenario. `dc` is a shell
function, so the backgrounded `$!` is the subshell and the docker-compose plugin
holding the pipe survives the kill. Replaced with a `window` verb that sleeps
and then pulls exactly `[T0,T1]` with `--since/--until`, no background process
at all. **Verified live**, because "the code is present" is never the evidence:
two consecutive 12 s windows against a 1 Hz probe are disjoint (`06:05:57.426`
against `06:05:58.512`), both non-empty, and window 1's file was byte-identical
before and after window 2 ran. `follow -d` now exits 5 and names the
replacement.

**2. The black hole was fake on its first use, and the arm's own probe is what
had made it fake.** `logsink`'s first form was
`while true; do tail -f /dev/null | nc -l -p 8080 >/dev/null; done`. It passed
`hangprobe` at `06:03:56` — `curl` exit 28, a genuine hang. It then **fast-failed
with a 502 in 2.5 ms** when Arm 4 armed it at `06:42:48`. Plain `nc -l` is
single-shot, and the `tail -f` that was there to hold stdin open never exits, so
the pipeline never completes and the `while` loop never iterates: **the sink
served exactly one connection in its entire life, and the arm-verification probe
was the customer.** Every subsequent connection got `ECONNREFUSED`, so the
black-hole arm had silently degraded into the outage arm.

The first Arm 4 run is kept at `w6-2b/arm4-void/` and its numbers are used for
nothing. **The rule this is evidence for is the campaign's own — an arm is
verified when some capture measures its effect — with a sharpening: a
verification that is not idempotent can consume the thing it verifies, so it
must be re-run at the point of use and not once at setup.** The fix
(`nc -lk -p 8080 -e sleep 3600`) has no per-connection state to exhaust; three
consecutive `hangprobe`s pass (`w6-2b/arm4/`), and Arm 4's own in-line probe at
`06:50:19` passed immediately before the arm was measured.

---

### Arm 1 — the quadratic regime. Predicted 11,325 requests; measured 11,326

Run `840ac81a`, `edge-w6-trickle lines=600 interval_s=1`, `outage` armed
`06:07:29.6` and cleared `06:12:30.5` (**299.6 s** measured request-to-request).
Window `06:07:07Z .. 06:13:49Z`, 23,419 raw rows, 11,527 on the bulk URI.

| Quantity | Predicted | Measured |
|---|---:|---:|
| Requests during the outage | 11,325 | **11,326** (all 502, `arm=outage`) |
| Flush passes | 150 | **151** |
| Pass sizes | 1, 2, 3, … , 150 | **1, 1, 2, 3, … , 150** |
| Pass spacing | 2.000 s | **median 2.000 s** (min 1.455, max 2.002) |
| Peak instantaneous rate | 75 req/s | **150 req/s** — see the correction below |
| Drops | 0 | **0** |
| Drop marker | never fires | **0 marker lines in the completed run's log** |
| I4 at the end | exact | **exact**: 602 entries, `seq` 1-602 contiguous, `trickle 1..600` each once, zero duplicates, emission order preserved, run `Succeeded` |

**`1 + Σ(1..150) = 11,326` exactly.** The leading `1, 1` rather than `1, 2` is
the second tick finding no complete new line in the buffer and taking
`flushCompleteLinesLocked`'s `flushPendingLocked` branch (`:296-298`) — one
retry, no new batch. From tick 3 on it is a perfect triangle, deviation from
`k` never exceeding 1 at any of the 151 passes.

**CORRECTION to the prediction, and it doubles the number that matters.** The
`75 req/s` prediction spread a tick's requests over the 2 s tick. They are not
spread: the fault fails in **median `rt = 0.000 s`, max 0.001 s**, so a pass of
150 requests completes in ~150 ms and the peak *second* carries the whole tick.
Measured peak: **150 req/s at `06:12:29`**, the last outage tick, with
149/148/147 in the preceding seconds. **The instantaneous rate is `k` per
second, not `k/2`.** W1-6's derived `~67 req/s` peak is a factor of two low for
the same arithmetic reason, and is now **corrected in place at
`FINDINGS.md:457`, `:472` and `:495`** — all three sites that carried it —
rather than only here. **The transfer is labelled an inference across rigs and
faults, not a re-measurement of W1-6:** what carries over is the arithmetic
error of dividing a pass across its tick, which is rig-independent; Arm 1's
`rt` median of 0.000 s does **not**, because Arm 1's fault is an nginx 502 to a
dead upstream on the W6 stack whereas W1-6's was a controller-minted 403 whose
per-request time was never captured. Both fail fast, so the direction is not in
doubt, but W1-6's peak would have to be re-measured to be stated as measured.

**Recovery is a spike the same size as the outage's peak, and it lands on the
controllers.** `06:12:31`: **151 requests, all 204, in 0.475 s** — one
`flushLocked` pass draining the entire 150-batch backlog, holding `p.mu` for the
whole 0.475 s, all three replicas via round-robin. Steady state resumes
immediately at **0.514 req/s** (one flush per 2 s tick). *There is no coalescing:
150 batches means 150 HTTP requests and 150 handler invocations, at recovery
just as during the outage.*

**Cap headroom, measured.** Batches were **512-690 bytes on the wire** for
~90 bytes of line text — so the 1 MiB budget, which counts line text only, was
1.3 % consumed after 150 batches while the real wire/heap footprint was ~5-7×
that. The sparse regime cannot reach the cap, exactly as Amendment 1 argued.

### Arm 2 — the drop regime. The plateau is real; its height is not 256, and the reason is a mechanism correction

Run `b4b0bad9`, `edge-w6-chatty lines=1800 pad_bytes=1024 interval_s=0.1`,
`outage` armed `06:27:55.6` and cleared `06:30:06.6` (**130.8 s**), step still
running at the clear and for 90 s after it. Window `06:27:33Z .. 06:31:25Z`,
85,406 raw rows, 42,713 on the bulk URI.

| Quantity | Predicted | Measured |
|---|---:|---:|
| Requests during the outage | — | **42,356**, all 502 |
| Mean rate | — | **323.9 req/s** sustained for 131 s |
| Peak second | — | **880 req/s** (`06:29:35`) |
| Median second | — | **343 req/s** |
| Bytes of line text emitted during the outage | — | ~1.33 MiB (1,245 lines × 1,065 B) |
| Pending batches at the plateau | ~256 | **216-228 at the tail** (noisy estimator: min 1, max 281, median 60, last-third mean 154.1) |
| Lines dropped | ~1,024 | **263** |
| Drop marker | fires once with the full count | **fires exactly once, count exactly right** |
| I4 | count short by the dropped lines | count short by **exactly** the marker's number; zero duplicates; emission order preserved |

**AMENDMENT 1's batch-size arithmetic was wrong, and the correction is
load-bearing for the gate.** The prediction assumed the write path produces
4 KiB batches because `flushBytes` is 4 KiB. It does not. `Write` is called with
whatever the agent's copier read from the step's stdout pipe — up to a full pipe
read — and only *then* tests `p.buf.Len() >= p.flushBytes`. So a batch is "one
pipe read's worth", not "4 KiB". Measured over 42,356 requests:
**median `reqlen` 5,143 B, max 21,990 B**, i.e. ~4.8 lines per batch here and up
to ~35 lines per batch in the faster Part E run (median 13,558 B, max 37,290 B).

The plateau follows directly — **but the divisor must be line-text bytes, not
`reqlen`, and an earlier version of this paragraph conflated them.** The cap
sums `len(r.Line)` and nothing else (`runner.go:438-446`), whereas `reqlen` is
nginx's **wire** length: JSON framing, headers and the request line included.
Written correctly the estimate is `1 MiB ÷ (4.8 lines × 1,065 B of line text) =
205 batches`; the `1 MiB / 5,143 B = 204` an earlier version printed lands in
the same place **only because this fixture's 1 KiB padding dominates the
framing**. That is not general: **Arm 1 measured 512-690 B on the wire for ~90 B
of line text — a wire:text ratio of ~6-7×** — where the `reqlen` form of the
formula would be wrong by that whole factor. Use line-text bytes.

The measured `len(p.pending)` was recovered independently, from the
**periodicity of the `reqlen` sequence**, since a flush pass re-sends the
pending list in order and the sequence therefore repeats with period
`len(p.pending)+1`. **The estimator is noisy and its noise is reported here
rather than smoothed away.** Over the outage it reads `min=1 max=281 median=60`,
with a `last-third mean=154.1` (`arm2b/analysis.txt`); the series is not
monotone — it opens `15, 5, 40, 15, 34, 53, …` — and the
`15 → 68 → 200 → 216 → 228` an earlier version quoted is a **selected monotone
subsequence**, not the series. The tail is what the plateau claim rests on and
it is stable: the last six samples are `216, 216, 216, 216, 228, 152` (the final
152 is the recovery pass, already draining). **The estimator's own maximum is
281, above the 205-batch arithmetic**, and the capture prints it as
`implied cap in batches ~ 281` — a single-sample excursion consistent with a
pass whose period the sampler mis-segmented, disclosed rather than dropped. So
the plateau is stated as **216-228 at the tail, against an estimator spanning
1-281 over the whole outage**. Prediction and measurement agree on the
*mechanism* (the plateau is `cap ÷ batch line-text bytes`) and disagree on the
*number* by the factor the batch-size error introduced.

**The plateau is the whole point for the gate: per-flush request cost is
`len(p.pending)`, and `len(p.pending)` is `cap ÷ batch_bytes`. The cap is the
only thing bounding the request storm.**

**The drop marker works, and it is exact.** 1,800 lines emitted, **1,537
delivered, 263 missing in one contiguous range `211..473`** — drop-oldest, as
designed — and the marker reads:

```
seq 1801  stderr  [263 log line(s) dropped: controller unreachable]
```

263 = 263, to the line. No duplicates, `seq` 603-2142 contiguous, emission order
preserved, run `Succeeded`.

**One usability defect measured while confirming it: the marker does not mark
the gap.** It is emitted on the first successful flush after recovery, so it
sits between `chatty 1460` and `chatty 1461` — **987 lines after the gap it
describes**, which is at `210 → 474`. A reader scrolling the log meets an
unexplained 263-line jump, and the explanation is a thousand lines further
down, carrying no indication of which range it refers to. Filed as an
observation.

### Arm 3 — the flapping fault. The fixed point holds, and the arm found a violation nobody was looking for

Run `c57daa9e`, `edge-w6-trickle lines=400 interval_s=1`, `flap` armed
`06:33:42.9` and cleared `06:38:44.1` (**300.5 s** — deliberately the same
duration as Arm 1's outage). Window `06:33:26Z .. 06:39:07Z`, **921 raw rows**
against Arm 1's 23,419.

| Quantity | Predicted | Measured |
|---|---:|---:|
| Per-request failure probability | 0.50 (nginx `split_clients`) | **0.470** (135 of 287) |
| Fixed point `p* = (1-s)/s` | 1.00 | **0.90** |
| Mean requests per flush pass | 2.00 | **1.901** |
| Max pass | small | **5** (Arm 1's max pass was **150**) |
| Total requests over the window | ~300 | **287** |
| Peak second | ~2-3 | **5 req/s** (Arm 1: 150 req/s) |
| Drops / marker | none | **0 / 0** |

**287 against 11,326 for the same 300 s of fault: a factor of 39.5.** The pass
count is identical in both arms (151 and 151, median spacing 2.000 s) — the
*ticker* is the same; what differs is that a partial fault drains the backlog
geometrically and a total one never drains it at all. **Amendment 2 is
confirmed by measurement, and it is the single most important input to the
gate: the quadratic is a property of a total, sustained outage, not of the
retry policy under stress.**

#### The violation: a partial fault silently REORDERS the stored log

Not predicted, not looked for, and found because I4 was checked in every arm
rather than only in the ones expected to break it.

```
seq 2165 -> 2166 : trickle index 24 -> 21   (emitted 06:33:48.597 -> 06:33:45.587)
seq 2191 -> 2192 : trickle index 50 -> 41   (emitted 06:34:14.684 -> 06:34:05.654)
seq 2309 -> 2310 : trickle index 168 -> 157  (emitted 06:36:13.091 -> 06:36:02.056)
```

**26 inversions; 137 of 400 lines stored out of emission order; maximum
displacement 12 positions.** Count is exact (400/400), duplicates are **zero**,
the run is `Succeeded`.

**Mechanism, and it is plain in the code.** `flushLocked` retries the pending
list first (`:360-366`) and then sends the current buffer (`:369-393`), **in one
pass, continuing past failures**. Under a partial fault an *older* pending batch
can fail in the same pass in which the *newer* buffer batch succeeds. The newer
lines are inserted first and take the lower `seq`; the older lines land on a
later tick with a higher `seq`. Every read path orders by `seq` — `TailLogs`
(`postgres.go:939-943`), the archive read (`:970-972`), search (`:1056-1058`) —
so **every reader gets the wrong order**.

**It is recoverable in principle and by nothing in the product.** The `ts`
column is stamped once per batch at build time (`:376`, `now :=` outside the
loop — 201 distinct values for 400 lines), and re-sorting by `(timestamp, seq)`
does recover emission order exactly, **0 inversions left**. No read path does
that.

**Why this is not W3-4 (`FINDINGS.md:1521`) re-filed.** W3-4's reordering is a
consequence of *duplication*: nginx committed a prefix, the agent resent, and
the surplus rows landed out of order. Here **nothing is committed twice** — the
`outage`/`flap` faults route to a dead upstream with no mirror, and the measured
duplicate count is 0 — and the count is exact. The trigger is different (any
per-request failure ratio, no partial commit required), the mechanism is
different (flush-pass ordering, not retry-after-partial-commit), and the fix is
different (abort the pass on first failure). Filed separately and cross-linked.

### Arm 4 — the black-hole partition. The step stalls for 176.3 seconds, and the request count collapses

Run `9b175a9b`, `edge-w6-chatty lines=400 pad_bytes=1024 interval_s=0.2`,
`hang` armed `06:50:19.1` (probe **passed in line**, curl exit 28) and cleared
`06:53:34.4` — **195.3 s**. Window `06:49:57Z .. 06:54:58Z`, 547 raw rows.

**Six requests. That is the entire log-path cost of a 195-second black hole.**

```
06:50:24  499  rt=4.999   urt=5.000   reqlen=5143    target=hangsink
06:50:29  499  rt=5.001   urt=5.000   reqlen=5143    target=hangsink
06:50:31  499  rt=12.013  urt=12.012  reqlen=212     target=hangsink   <- the hangprobe itself
06:51:29  499  rt=60.000  urt=60.000  reqlen=5143    target=hangsink
06:52:29  499  rt=60.004  urt=60.000  reqlen=29184   target=hangsink
06:53:29  499  rt=60.006  urt=60.001  reqlen=5143    target=hangsink
06:53:35  000  rt=5.926   urt=5.924   reqlen=29184   target=hangsink   <- cut short by the clear
```

**Every prediction in Arm 4's table is confirmed, and the access log states each
one on its own line.**

- **`status=499` with `rt=60.000`** is the direct proof: 499 is nginx's
  client-closed code, so *the agent* ended the request, and it ended it at
  exactly 60.0 s — `internal/agent/client.go:53`'s `Timeout: 60 * time.Second`,
  measured to the millisecond, three times.
- **The two `rt=4.999`/`5.001` rows are the write path**, bounded by
  `logPusherWriteFlushTimeout` (`:219`) exactly as its comment says. **They stop
  after the first 10 seconds and never recur**, which is Amendment 3 confirmed
  by effect: once the auto-flush goroutine held `p.mu`, `Write` could no longer
  reach its own timeout because it could no longer reach the `if` that sets it.
- **A pass costs `len(p.pending) × 60 s`**, and the two distinct `reqlen`s
  (5,143 and 29,184) resolve it: the pass at `06:51:29`+`06:52:29` is one
  two-batch pass taking 120 s, and `06:53:29`+`06:53:35` is the next one. The
  ticker's buffered channel means the next tick is already pending when a pass
  ends, so **`p.mu` is re-acquired immediately and is held essentially
  continuously**.

**And therefore the step itself stops.** Measured on the `/data` heartbeat file,
which does not pass through the `LogPusher`:

```
line 209  06:50:39.238   <- last line written before the stall
line 210  06:53:35.580   <- first line after it
GAP = 176.3 s   against a median inter-line cadence of 0.205 s
```

**One gap ≥ 5 s in the whole 400-line run, and it is 176.3 s.** The stall began
20 s after the arm (the stdout pipe buffer absorbing ~100 lines first) and ended
**1.1 s after the fault was cleared**. The step's own process — not its logging,
its *execution* — was stopped for 90 % of the fault by a log endpoint that
neither answered nor refused.

**I4 held exactly** (403 entries, `seq` 2654-3056 contiguous, `chatty 1..400`
each once, zero duplicates, emission order preserved, zero drop markers, run
`Succeeded`). Nothing was lost: the hang is not a data-loss fault, it is an
availability fault, and that is the axis nothing in this campaign had measured.

**The observability enumeration, confirmed by effect.** Across the 195 s hang
both agent containers logged **66 lines, all `{"level":"ERROR","msg":"claim"}`,
all stamped `06:50:19.92`** — the instant of the arming reload, `EOF` on the
long-poll, i.e. `worker_shutdown_timeout 1s` severing the agents' established
connections. (Which is also the positive evidence that the arm reached the
agent's *existing* keepalive connection and not merely fresh ones — the standing
rule for arms in front of a long-poll endpoint.) **Zero lines anywhere mention
the log path, the flush, or the stall**, against 0 in a clean control window.
A 176-second step stall is invisible in the agent log, invisible in the run
status, and has no metric because the agent binds no HTTP listener at all.

#### What "black hole" means here, and what it leaves unmeasured

Stated plainly rather than glossed, because the campaign has shipped inert arms.
**This is an HTTP-level black hole, not a packet-level one.** The agent image
has no `iptables` and no `NET_ADMIN`; the fault is nginx forwarding to a
container that accepts the TCP connection and never writes to it.

*Equivalent* in everything the arm measures: the request is accepted, sent, and
never answered; `AppendLogBulk` blocks for the full client timeout; `p.mu` is
held; the step stalls. Confirmed to the millisecond by the `rt=60.000` rows.

*Not equivalent*, and therefore **still unmeasured on this rig**:

1. **`connect` never hangs here.** A packet-level drop would stall in the
   dialer, where Go's transport applies its own dial timeout rather than the
   60 s client timeout, so the per-request cost could differ from 60 s.
2. **No retransmit or backoff dynamics**, and no half-open sockets — the socket
   is genuinely alive, so TCP keepalive never fires and no `RST` ever arrives.
   A real partition can leave a socket in states this arm cannot produce.
3. **The heal is instant here** (an nginx reload), so nothing was learned about
   what a *recovering* black hole does, e.g. whether a 60 s-blocked request that
   completes late races the batch that replaced it.

None of the three changes the finding — the finding is that a request the
controller does not answer stalls the step, and that is measured — but a
fix-verification pass on a real partition should re-check (1) specifically.

### Part E — the marker's reachability. A `Succeeded` run with **zero** log lines and no marker

Run `120de00e`, `edge-w6-chatty lines=2000 pad_bytes=1024 interval_s=0` (~2.05
MiB), `outage` armed **before** the trigger and **never cleared** while the run
lived. This arm was Arm 2's first attempt and it turned into Part E by itself:
at full speed the step finished in under 30 s, i.e. **the outage outlived the
step**, which is precisely the W1-2 condition.

```
GET /api/v1/runs/120de00e/logs/stats  ->  {"count":0,"maxSeq":0,"minSeq":0}
run status                            ->  Succeeded
drop markers                          ->  0
lines emitted                         ->  2002
lines delivered                       ->  0
```

**11,015 HTTP requests were spent — peaking at 1,070 req/s, sustained above
1,000 req/s for five seconds — to deliver nothing at all.** The step-end `Flush`
spent its 5 s budget (`:343`) against a fault that was still armed, `p.pending`
was still non-empty, and so the marker's `len(p.pending) == 0` precondition at
`:401` was never met. `droppedLines` was certainly non-zero — 2.05 MiB of line
text against a 1 MiB cap forces eviction — and it is **structurally
unobservable**: unexported, no accessor, no metric, no log line, and the one
surface that would have reported it is gated on the backlog being empty.

**This is W1-2 (`FINDINGS.md:179`) reached from the other side and it is CITED,
NOT RE-FILED.** W1-2 owns "the bounded step-end `Flush` loses the tail". What is
new, and is filed separately, is the *marker's* reachability: W1-2's own entry
records that in its case `droppedLines` was never even incremented, so the
marker's precondition was moot. **This is the first live case in the campaign
where the eviction path certainly fired and the marker still could not**, which
makes the precondition itself the defect rather than a coincidence of the
earlier run.

**The severity of the shape, stated once.** The loss is not a tail. It is
100 % of a `Succeeded` run's output, with the run reading green, the log reading
empty, and every announcement mechanism in the system silent.

### The connection budget, watched as the brief instructed

`FINDINGS.md:2517`'s saturation was **not** reproduced and this scenario does not
contradict it. After the heaviest arms: `/readyz` **200** on the LB and all three
replicas directly; **15 of 15** authenticated LB reads returned 200, zero 401s;
`pg_stat_activity` split by `datname IS NULL` gives **85 client backends and 5
background workers** — i.e. 85 of 100, not the 90 the raw total would suggest, so
the W6-2a correction to `w6-pgsample.sh`'s summary line was applied rather than
re-derived. **Cited, not re-filed.** The difference in shape is the same one
W6-2a recorded: 2517's trigger was concurrent requests, and every arm here is
one agent's serial write path, however fast.

---

## THE DISK-SPILL GATE

`FINDINGS.md:496` gates the `LogPusher` disk-spill candidate
(spec `:199-207`) on W6-S2 measurement. The measurement exists now.

### Recommendation: **do not implement disk spill as specified. Implement four
cheaper changes first, three of which are strictly better value, and re-open
spill only after the first of them lands.**

**The measurement that decides it.** Arm 2 established that per-flush request
cost is `len(p.pending)`, and that `len(p.pending)` at steady state is
`min(cap, line-text bytes emitted during the outage) ÷ batch line-text bytes` —
measured at **216-228 batches at the tail** for a 1 MiB cap and ~5.1 KiB of line
text per batch, producing **324 req/s sustained and 880 req/s peak from a single
step**. Disk spill's *only* effect is to raise the cap, so with `flushLocked`
unchanged, **raising the cap raises the ceiling on the storm**.

**Two labels this paragraph must carry, because neither was measured.**
**(1) The 1:1 relation between cap and per-flush request count is a CODE
INFERENCE, not a measurement — no arm varied `maxPendingBytes`.** All four arms
ran the stock 1 MiB (`runner.go:256`). The relation is read off `flushLocked`
(one request per pending batch, `:358-366`) and `appendPendingLocked` (evict
oldest until `pendingSizeBytes <= maxPendingBytes`, `:429-446`): the cap bounds
the batch count and the batch count *is* the request count, with no coalescing
anywhere between them. What Arm 2 measures is the **left** side — that the
plateau exists and sits where the cap divided by the batch size puts it. An arm
that raised the cap and re-measured the plateau would close this, and it is the
single cheapest follow-up in this scenario. **(2) The "100 MiB ⇒ ~20,000
batches" figure is arithmetic on that inference and is bounded by the `min()`
above, not reached on demand.** The backlog cannot exceed what the step has
actually emitted: Arm 2 emitted ~1.33 MiB of line text in 131 s, so at that rate
a 100 MiB buffer would need **~2.7 hours** of unbroken outage to fill. The
correct statement is that spill removes the only bound on the storm and lets it
grow with outage duration; the 20,000-request tick is the ceiling that bound
would no longer impose, not a rate anything was observed to reach.
`FINDINGS.md:465` warned this in the abstract, and none of it reverses the
recommendation — it sharpens what the recommendation is entitled to claim.

**The second measurement that decides it.** Spill addresses loss at the byte
cap. Of the three loss paths this scenario exercised, **the byte cap is the only
one that already announces itself correctly** — Arm 2 lost 263 lines to eviction
and the marker reported exactly 263. The two losses that are silent are Part E's
(the whole run's output, lost at the step-end budget) and, on the availability
axis, Arm 4's 176-second stall. **Spill fixes the announced loss and neither of
the silent ones.**

### What to do instead, in order, with costs

**R1 — abort a flush pass on its first failure.** *(smallest change here)*
`flushLocked` currently continues through the whole pending list and then sends
the buffer regardless of what failed. Stopping at the first failure **fixes the
Arm 3 I4 reordering violation outright** (a newer batch can no longer overtake
an older one), and under a partial fault it also stops spending requests on
batches that will be re-sent anyway. **Cost:** under a fault that fails *one
specific* batch repeatedly, head-of-line blocking — the rest of the backlog waits.
**Does not fix:** the quadratic, the loss, the stall.

**R2 — give the auto-flush path a per-flush deadline.** `StartAutoFlush` passes
`flushCtx` from `context.WithCancel` (`backend_host.go:378`), so a flush is
bounded only by `60 s × len(p.pending)`. Passing a `context.WithTimeout` per
tick — the write path's own `logPusherWriteFlushTimeout` is the obvious value —
turns Arm 4's **176.3 s step stall into ~5 s per tick**. **Cost:** three lines,
and a fault that is merely slow rather than hung will now abandon flushes it
would have completed, so the deadline must be comfortably above a normal
round trip. **Does not fix:** any data loss; the step still runs slowly under a
hang, it just is not frozen.

**R3 — make the drop marker reachable, and make it say where.** Two changes:
attempt the marker whenever `droppedLines > 0` **and at least one request in
this flush succeeded**, instead of requiring `len(p.pending) == 0` (`:401`); and
carry the covered range in the marker's text, since Arm 2 measured it landing
987 lines after the gap it describes. **Cost:** the marker can now be emitted
while a backlog still exists, so its count is a running total and may be
superseded — which is why the *range* matters. **Does not fix:** Part E, where
no request ever succeeds. For that case the only answer is an agent-side
`slog.Warn` at teardown when `p.pending` is non-empty — currently the whole
`LogPusher` contains **zero** log statements, so a silent teardown is not an
oversight in one place, it is the file's uniform behaviour.

**R4 — coalesce the backlog, in chunks, and only after R1.** Send the pending
list as `ceil(total ÷ K)` requests rather than one per batch. This is the change
that actually flattens the curve: per-flush cost stops being `len(p.pending)`
and becomes `bytes ÷ K`, bounded by the cap rather than by the batch count.
**Three costs, all measured elsewhere in this campaign and all real:**

1. **It collides with nginx's 1 MiB default** (`FINDINGS.md:1655`). Arm 2's
   plateau backlog was ~1 MiB of line text and more on the wire; a literal
   single-request coalesce would have been **413**ed by the reference
   deployment. `K` is not optional.
2. **It collides with the controller's own cost model.** W6-2a measured
   `handleAgentLogBulk` at `2N` statements with no bound on `N`, and 7,600 lines
   in one body holding a Postgres backend for **9.356 s** (`FINDINGS.md:2584`).
   Coalescing moves the cost from many small requests to few enormous ones, which
   is better for the agent and *worse* for one controller goroutine. `K` must be
   chosen against that entry, not against the agent alone.
3. **It must not be generalised to an agent-level batcher without the W3-5 fix
   landing first.** Within one `LogPusher` the body is single-run and the change
   is safe exactly as `FINDINGS.md:474` already states. Merged **across**
   concurrent pushers it puts more than one run id in one body, and
   `handleAgentLogBulk` keeps a single scalar `droppedRun`
   (`api_agent.go:730-733`), so a bulk append spanning two sealed runs
   attributes every drop to whichever run is last in the body — the W3-5
   observation at `FINDINGS.md:1878`/`:1924`. **The two changes must not land in
   either order without the other.** Stated here as the brief required, and it
   is not a hypothetical: R4 is exactly the change whose obvious next
   optimisation is the agent-level one.

### Then, and only then, disk spill

After R4, the request cost of holding a backlog no longer scales with the
backlog, and the argument against spill evaporates. At that point spill is worth
re-opening **together with the flush-before-finish barrier**, which is the half
of the spec's own §6 candidate that addresses the losses actually measured here
— and **the barrier is worth more than the spill on its own evidence**: Part E
lost 100 % of a run's output with the cap irrelevant to the outcome, and Arm 2
lost 263 lines to the cap and announced them correctly.

**What spill still would not fix, whenever it lands:** the spec's own hard bound
(after archive seal, late lines are dropped with a 204 regardless — W3-5, and
W6-2a's C4 met it by accident); Arm 3's reordering; and Arm 4's stall, which is
not a capacity problem at all.

### The one-line answer

**The `LogPusher`'s problem is not that its buffer is too small. Arms 1 and 3
show the request storm is a total-outage phenomenon and disappears under a
partial one; Arm 2 shows the buffer's size is the only thing bounding that
storm; Part E shows the buffer's contents are discarded wholesale at step end
anyway; and Arm 4 shows the failure mode with the worst consequence spends
almost no buffer at all. Spilling to disk makes the one bounded thing unbounded
and leaves every silent failure silent.**

---

## Findings filed

| # | `FINDINGS.md` | Kind | Severity | Subject |
|---|---|---|---|---|
| 1 | `:2630` | **violation, I4 ("no reordering")** | **major** *(raised from minor at review)* | a fault that fails only *some* requests reorders a `Succeeded` run's stored log — 137 of 400 lines out of emission order, count exact, **zero** duplicates. Distinguished from W3-4 on trigger, mechanism and fix — as a *distinction*, not as the severity basis |
| 2 | `:2643` | observation | major | a controller that accepts and never answers stalls the **step itself** for 176.3 s; the auto-flush path has no per-flush deadline and the 5 s bound is applied after `p.mu` is acquired |
| 3 | `:2657` | observation | major | the drop marker's `len(p.pending) == 0` precondition makes it unreachable exactly when the loss is total — a `Succeeded` run, 2,002 lines emitted, **0** delivered, no marker |
| 4 | `:2670` | observation (campaign asset) | minor | `w6-reqshape.sh` had the same unstoppable-capture defect Task 2 fixed elsewhere, and the black-hole arm's own probe destroyed the arm it verified |

**Finding 1's band was raised from minor to major at review, and the reason is
methodological before it is substantive.** The original Severity line calibrated
against W3-4's clause count — argument by analogy, which is the exact move
commit `066e421` disallowed one task earlier in this same wave when it dropped
W6-2a's D2 to minor on the ground that a band must come from `FINDINGS.md:6-8`.
Re-argued from that text alone: **"incorrect visible behavior" fits word for
word** (every read surface orders by `seq`, so every reader is served the wrong
order, permanently, with nothing marking it), **none of "diagnosability / docs
gap / cosmetic" fits** (the stored artifact is wrong rather than
under-observed; no document is wrong; and a log's order is its content, not its
presentation), and **critical is excluded on the limb `:2336` already worked
through** for silent permanent *loss* — nothing is lost here and emission order
is fully recoverable from the stored `ts`. The W3-4 comparison stays in the
entry's Notes as a distinction carrying no severity weight.

**Cited, not re-filed** — four, per the standing rule, and each was grepped for
before this scenario filed anything:

- `FINDINGS.md:465` (W1-6) — the quadratic amplification itself. Arm 1 is its
  first **direct** measurement (11,326 against a predicted 11,325, and 151
  passes of 1, 1, 2, … , 150) where W1-6 had a model reconciling to within 1 of a
  measured total. **One correction is supplied rather than a new entry:** W1-6's
  derived peak of `~67 req/s` divides a tick's requests by the 2 s tick, and the
  requests do not spread — they complete in ~150 ms, so the true peak is
  **`k` per second, not `k/2`**, measured here at **150 req/s**. That figure was
  a factor of two low and has been **corrected in place at `:457`, `:472` and
  `:495`**, the three sites that carried it (`:457` had already been corrected
  once, in the same direction, per the note at `:458`). The correction is
  labelled a **cross-rig inference on the arithmetic**, not a re-measurement of
  W1-6's 403 path.
- `FINDINGS.md:179` (W1-2) — the bounded step-end `Flush` losing the tail. Part E
  reproduces it at 100 % loss; finding 3 above is about the *marker*, not the loss.
- `FINDINGS.md:1521` (W3-4) — duplication-and-reordering under retry. Finding 1
  explains at length why it is not this.
- `FINDINGS.md:2517` (W6-infra) — connection pressure. **Not reproduced**: after
  the heaviest arms, `/readyz` 200 on the LB and all three replicas, **15 of 15**
  authenticated reads 200 with zero 401s, and 85 client backends + 5 background
  workers (the W6-2a `datname` correction applied rather than re-derived).

**Invariant summary.** **I4 violated once** (Arm 3, the "no reordering" clause,
by its own text). **I4 held exactly in Arms 1 and 4** — 602/602 and 400/400
lines, contiguous `seq`, zero duplicates, emission order preserved — and held
*to the line* in Arm 2, where the shortfall equals the marker's own count. **I5
is not reachable by any of these results** and the entries say so: its text
bounds *recovery after* a fault against `docs/high-availability.md`, and every
cost measured here is incurred while the fault is ongoing — the same reading
`FINDINGS.md:473` already applied to the W1-6 entry.
