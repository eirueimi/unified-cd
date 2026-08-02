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
  "droppedLines" --include=*.go .` returns **17** hits: 8 in `runner.go` and 9
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

*(filled in after execution)*
