# Edge-Case Test Campaign — Design

Date: 2026-07-29
Status: Approved (design); implementation plan to follow

## 1. Purpose and approach

Systematically probe unified-cd's distributed-systems edge cases — failover
windows, reaper timing boundaries, storage races, scale limits — to find real
defects before users do.

The campaign is **exploratory first, automation second**:

1. **Phase 1 (explore):** run scripted scenarios against live stacks, record
   every invariant violation in a findings document. No fixes during this
   phase — findings are reported in one batch at the end, and the operator
   decides priorities.
2. **Phase 2 (automate):** only scenarios that found defects (regression
   tests shipped with the fix) or that guard fragile timing contracts get
   promoted to permanent tests. Structurally-safe scenarios keep their
   runbook only (YAGNI).

Rejected alternatives: automation-first (writes assertions against behavior
we do not understand yet, and inverts the explore-then-automate decision);
pure chaos injection (poor reproducibility and diagnosis for targeted
scenarios — may be added later as an extra wave once the targeted list is
exhausted).

## 2. Scope

- **Environments:** docker compose (extension of `test/ha/`) and kind
  (k8s-agent scenarios).
- **In scope:** recovery/failover, reaper/timing boundaries, storage/key
  handling, k8s agent, scale/abuse limits (connection counts, rate-limit
  absence, NOTIFY hotspots).
- **Out of scope:** multi-tenancy / RBAC granularity for ~100-user
  operation (separate initiative: namespaces, secret scoping, per-job
  approvers, queue fairness *features*); fixing defects mid-campaign;
  mixed-version upgrade testing beyond the single lowest-priority W5
  scenario.

## 3. Invariants (defect criteria)

Every scenario names the invariants it attacks. A violation is a finding.

| ID | Invariant |
|----|-----------|
| I1 | **Run accounting** — every API-accepted run reaches exactly one terminal state; no phantom runs from duplicate fires/webhooks |
| I2 | **At-most-once side effects** — step side effects execute at most once (detected via an append-only side-effect log on a shared volume, closing the gap `ha_test.go` documents: upserted step reports cannot reveal re-execution) |
| I3 | **No lock leaks** — mutex/semaphore/concurrency slots are released when the holder reaches a terminal state (verified by a successor run acquiring the lock AND by direct inspection of `mutex_holders` / `named_lock_slots`) |
| I4 | **Stored-state integrity and provenance** *(AMENDED post-campaign — the original text is quoted and the amendment argued in §9 at the end of this file; entries filed before the amendment quote the original and are correct as of their filing)* — for each object class the system persists on a run's behalf — **log rows, log archives, artifacts, cache entries, secrets** — (a) what is stored matches what the run wrote: no loss, no duplication, no reordering; (b) it stays readable, and decryptable, for as long as the product still serves it; and (c) the object served for a request is the object that request named. A Succeeded run's log line count matching what the workload emitted is clause (a) applied to log rows |
| I5 | **Bounded recovery** — after fault injection the system returns to steady state within documented bounds (leader re-election ≤ seconds; stuck-run reap ≤ staleAfter 90s + interval 30s; the bounds in `docs/high-availability.md` are the contract) |
| I6 | **Zombie containment** — after the controller fails a run, observed agent-side behavior is *measured and documented* (not pass/fail: the architecture has no hard fencing; the operator judges acceptability) |
| I7 | **State display consistency** — run status, approval status, and audit rows never contradict each other or reality |

## 4. Waves and scenarios

Waves run in order (cheap-setup and high-yield first). Codex-suggested
scenarios (C-prefixed) came from an independent code-grounded review and are
merged into the waves below. Scenario counts are a ceiling — waves may be
trimmed at the checkpoint after each wave if yield is low.

### W0 — Environment-config probes (compose config changes only)

| ID | Scenario | Invariants |
|----|----------|-----------|
| W0-1 (C8) | PgBouncer in transaction-pooling mode: verify advisory locks / LISTEN visibly break (docs warn this; confirm failure mode is diagnosable, not silent split-brain) | I1, I5 |
| W0-2 (C14) | Scheduler clock/TZ boundaries — TZ mismatch between replicas, DST gap/fold, one-hour catch-up window edges, backward clock step — probed via observational unit tests (build tag `edgeprobe`) driving `checkAndFireSchedules` with constructed `now` values. libfaketime cannot intercept static Go binaries (vDSO time calls) and Linux time namespaces do not cover CLOCK_REALTIME, so container-level skew injection is not implementable | I1 |

### W1 — Recovery / failover (compose)

| ID | Scenario | Invariants |
|----|----------|-----------|
| W1-1 (#1) | All-controller restart during a long-running run: agent report retries, buffered log delivery, SSE reconnect, final state consistency | I1, I4, I5 |
| W1-2 (#7) | PostgreSQL failover mid-run: reconnect, advisory-lock re-election, LISTEN/NOTIFY re-subscription | I1, I5 |
| W1-3 (#8) | Controller restart/failover while a run is awaiting an approval gate; approval-reaper timeout behavior across the restart. (Run pause/resume does not exist in unified-cd — earlier drafts assumed it did; an approval gate is the only wait-state a run can be in.) | I1, I7 |
| W1-4 (#11) | Run cancellation racing controller failover | I1, I3 |
| W1-5 (C1) | One-way agent→controller partition (nginx blocklist): reaper fails the run and releases locks while the unfenced agent keeps executing — measure zombie duration and side effects | I2, I3, I6 |
| W1-6 (C9) | Agent credential revocation while its run executes: 4xx-permanent report abandonment vs run reap; measure what the unauthenticated process keeps doing | I6, I7 |
| W1-7 (C13) | DEFERRED past W3: AppSource reconciler crash mid apply/prune. No existing e2e exercises AppSource against real git (reconciler tests are fully mocked), and `file://` remotes are rejected by design (`dsl.ValidateGitRepoURL`), so this scenario needs a git-over-HTTP server container (dumb protocol: bare repo + `git update-server-info` + static file server) — too expensive to bolt onto W1 | I1, I7 |

### W2 — Reaper / timing boundaries (compose)

| ID | Scenario | Invariants |
|----|----------|-----------|
| W2-1 (#3) | Two-controller operation: leader exclusion verified for EVERY background job (scheduler, archiver, cache cleanup, all reapers, retention, trim), not just the scheduler | I1 |
| W2-2 (#4) | Agent replacement (same ID re-registers) with orphaned runs; `call:` descendant cascade-cancel | I1, I3 |
| W2-3 (#5) | Stuck-run reaper boundary timing: heartbeat staleness straddling 90s, claims straddling the 60s grace; lock release on reap | I1, I3 |
| W2-4 (#9) | Queued-run reaper grace boundary during full agent outage, racing agent recovery at the edge | I1 |
| W2-5 (C2) | `call:` child creation succeeds but the response/parent-link is lost: unlinked child undiscoverable by cascade cancellation | I1, I3 |
| W2-6 (C3) | Scheduler crash between run creation and `last_fired_at` update: duplicate fire by the next leader | I1, I2 |
| W2-7 (C4) | Two live agent processes sharing one agent ID: mutual orphan-classification failing each other's runs | I1 |
| W2-8 (C11) | Approval decision racing the timeout boundary (clock skew between deadline checks): Approved audit row + Failed run contradiction | I7 |
| W2-9 (C15) | Pending-snapshot starvation (NOT claim-query head-of-line blocking). A mutex-blocked run stays `Pending`: `tryQueueRun` (`postgres.go:482+`) hits the `mutex_holders` unique violation and rolls back, leaving status untouched, so it never reaches the `Queued` claim query at all. The starvation is one phase earlier — `TransitionPendingToQueued` (`postgres.go:437-475`) snapshots only the **50 oldest** `Pending` runs (`LIMIT 50` from `scheduler.go:58`) each 200ms tick, so ≥51 runs blocked on one held mutex saturate every snapshot and a newer, unblocked, runnable run at position 51+ is never examined. Git-unresolved runs consume batch slots identically (`postgres.go:513-518`) | I1, I5 |

### W3 — Storage / keys (compose + Garage)

| ID | Scenario | Invariants |
|----|----------|-----------|
| W3-1 (#2) | Cache TTL expiry racing a restore: cleanup leader deletes the entry/S3 object mid-restore | I4 |
| W3-2 (#6) | S3 outage during log archival and artifact upload: retry behavior, no collateral run failure, archiver behavior in the S3-write→record-create gap | I4, I5 |
| W3-3 (#10) | Mixed-KEK replicas (one controller with a wrong key): behavior of secret-using runs routed to the bad replica | I1, I7 |
| W3-4 (C5) | Bulk log append partial commit + lost ack: duplicate committed prefix on retry, SSE ordering | I4 |
| W3-5 (C10) | Archive seal racing the final log flush: terminal-status publication vs delayed agent flush → permanently incomplete archive (late lines 204-dropped) | I4 |
| W3-6 (C12) | Run retention racing an in-flight artifact upload: orphan object recreated under a deleted run's prefix | I4 |

### W4 — Kubernetes agent (kind)

| ID | Scenario | Invariants |
|----|----------|-----------|
| W4-1 | Pod GC racing a live run (transient controller blip during the GC resolve → skip-not-delete contract) | I1, I2 |
| W4-2 | Pod-reuse RBAC: verify the known missing `update`/`patch` verbs finding — is reuse still silently degraded to delete-every-run? | I5 |
| W4-3 | `podStartTimeout` behavior (fixed in PR #51): pod stuck Pending → bounded failure, pooled-pod not-ready handling | I5 |

kind wiring: ~~k8s-agent runs in kind; the controller stays on the compose stack, reachable from kind via host networking — only the enrollment URL changes, avoiding a second controller deployment.~~
**CORRECTED AFTER EXECUTION (W4, 2026-08-01) — "only the enrollment URL changes" is FALSE on four counts, each verified before the wave began and restated with its `file:line` at `docs/superpowers/plans/2026-08-01-edge-case-campaign-w4.md:13-17`:** no static token exists for the k8s agent (PR #75 removed it, `enrollmentPolicy` is mandatory); enrollment is **bidirectional** — the controller must reach into the cluster for `TokenReviews().Create` and `Pods().Get`, so it needs a kubeconfig and RBAC; the compose controllers load **no config file**, so the verifier map is empty and enrollment fails closed with 503; and HTTPS is enforced agent-side unless `allowInsecureHTTP` or a loopback host.
**What it actually cost, and what survives it, is recorded at `test/edgecase/FINDINGS.md` §"Checkpoint: W4 complete" and in `test/edgecase/scenarios/w4-0-enrollment-spike.md`. The controller did stay on the compose stack — that half is right — but enrollment itself is unconditionally broken (W4-0), so every W4 scenario ran behind an enrollment interposer.**

### W5 — Mixed-version rolling upgrade (lowest priority, only if time permits)

| ID | Scenario | Invariants |
|----|----------|-----------|
| W5-1 (C6) | Old/new controllers sharing one schema during rolling upgrade; concurrent migration startup; dirty-migration recovery; controller↔agent wire compat | I1 |

### W6 — Scale / abuse (compose + load generator)

Measurement-centric (documented limits, like I5) rather than pass/fail.

| ID | Scenario | Focus |
|----|----------|-------|
| W6-S1 | Connection pressure: ~30 claim long-polls + ~50 SSE subscriptions; measure PG connection count, behavior at `max_connections` (what breaks first, does `/readyz` degrade) — yields sizing guidance for ~100-user operation | I5 |
| W6-S2 | Log/NOTIFY hotspot: many concurrent high-frequency-log runs; measure log throughput, SSE latency, archival lag. Piggybacked measurement: LogPusher drop-marker frequency and lost-line counts under sustained load/partition (data to decide the disk-spill design, §6) | I4, I5 |
| W6-S3 | Trigger flood with one PAT: demonstrate the absence of rate limiting; observe scheduler/queue behavior compounding with W2-9 starvation | I5 |
| W6-S4 (C7) | Webhook duplicate delivery (no persisted delivery ID) creating multiple runs per external event; flood tolerance of the HMAC-verified endpoint | I1 |

## 5. Execution mechanics

### Layout

```
test/edgecase/
  README.md            # campaign overview, how to run
  FINDINGS.md          # one entry per violation (see format below)
  scenarios/           # one runbook per scenario: w1-5-oneway-partition.md ...
  compose/
    <scenario>.override.yaml   # per-scenario overlays stacked onto the
                                # test/ha stack (e.g. pgbouncer.override.yaml)
  workloads/           # job YAMLs (below)
  tools/
    inject.sh          # fault-injection helpers
```

### Runbook format (per scenario)

Target invariants / required stack / step-by-step commands / expected
behavior / observation points (log greps, psql queries, API calls). The
runbook doubles as the test spec if the scenario is promoted in Phase 2.

### Fault-injection helpers (`tools/inject.sh`)

`kill-soft` / `kill-hard` (SIGTERM/SIGKILL), `partition` / `heal`
(`docker network disconnect/connect`), `nginx-block` / `nginx-unblock`
(per-agent deny at the nginx LB — a full one-way agent→controller partition
since all agent traffic transits nginx and the system is strictly
agent-polls-controller; W1-5), `pause` / `unpause` (SIGSTOP: alive but
unresponsive — distinct failure mode from kill and partition),
(clock-skew injection was dropped: libfaketime cannot intercept static Go
binaries and time namespaces do not virtualize CLOCK_REALTIME — W0-2 covers
clock boundaries via `edgeprobe` unit probes instead).

### Workloads (`workloads/`)

1. `longrun.yaml` — 5–10 min, echoes a sequence number every second (I4
   line accounting + W1-1 target)
2. `sideeffect.yaml` — appends `run_id,step,timestamp` to a shared volume
   (I2 re-execution detection)
3. `mutex-pair.yaml` — two jobs contending one mutex (I3 leak detection)
4. `cache-roundtrip.yaml` — cache save/restore (W3-1)
5. `call-parent.yaml` — `call:` sub-run (W2-2/W2-5)
6. `approval.yaml` — approval gate (W1-3/W2-8)

### Observation

API-first; direct psql for lock residue, `last_fired_at`, archive records.
Each runbook carries its queries inline.

### Findings format (`FINDINGS.md`)

Per entry: scenario ID, reproduction steps, violated invariant, severity,
log/query excerpts. Reported as one batch after the final wave; the
operator prioritizes fixes and Phase-2 promotions.

## 6. Known-gap handling (do not re-discover)

Verified current state (2026-07-29):

- **Artifact & cache uploads stream** (`artifact.StreamTarZstd`, io.Pipe →
  chunked HTTP → streaming S3 put). The former RAM-buffering gap is fixed;
  io.Pipe backpressure means no buffer to overflow. W6 only sanity-checks
  controller-side behavior under parallel uploads.
- **LogPusher** retains the 1 MiB drop-oldest pending cap but now emits a
  `[N log line(s) dropped: controller unreachable]` marker (no longer
  silent). Remaining design candidate: disk spill with a cap + a
  flush-before-finish barrier. **Decision gated on W6-S2 measurements** —
  note the hard bound: after archive seal, late lines are dropped (204)
  regardless, so spill only helps partitions shorter than the run's
  remaining lifetime unless the finish barrier is added.

Both go into the findings report as design candidates with measured data,
prioritized alongside new findings.

## 7. Phase 2 — automation

Selection per scenario after the findings review:

1. **Found a defect** → regression test ships with the fix (highest
   priority).
2. **No defect, but fragile timing/contract** → promotion candidate.
3. **Structurally safe** → runbook retained, no automation.

Forms:

- **Level-1 (preferred):** deterministic Postgres-backed unit tests in
  `internal/store` / `internal/controller` for transaction-split and
  boundary-timing scenarios (W2-6, W2-8, W2-9, W2-3 class). Fast, runs in
  CI always.
- **Level-2:** compose-driven tests following the `test/ha` pattern under a
  new build tag `edge`, only where process kill/partition is essential.
  **Initial cap: ~5 tests** (maintenance cost); more require case-by-case
  approval at the findings review. Not in default CI — manual/nightly.

## 8. Process rules

- All campaign artifacts (runbooks, compose, scripts, findings) are
  committed in English, authored in a worktree (AGENTS.md).
- No production-code changes during Phase 1.
- After each wave: a short checkpoint summary (scenarios run, findings so
  far, anything invalidating later waves).

## 9. Post-campaign amendment: I4 restated as stored-state integrity

Added 2026-08-03, after the campaign closed and after the calibration
settlement. **This section changes the invariant set, no product code and no
evidence.** It is recorded here because §3 is where a future reader meets I4.

**The original text, quoted so the amendment can be checked against it:**

> | I4 | **Log/artifact integrity** — a Succeeded run's log line count matches
> what the workload emitted; no duplicates, no reordering; archives stay
> readable |

**Why it was amended.** The campaign's summary records four coverage gaps in
I1-I7, **three of one kind**: three real defects each had to reach for a
`docs/` sentence — the documented-contract limb — because no invariant's
*subject* reached the object that was damaged. No secret-store integrity
clause (W3-3, `test/edgecase/FINDINGS.md:1620`), no cache-integrity clause
(W3-1, `:1938`), no artefact-provenance clause (W5-2, `:3004`). I4 reads
"Log/**artifact** integrity" by its own text, which is exactly why a
propagating cache corruption and a wrong-artefact download both fell outside
it. Three of a kind is a fact about the invariant set, not three separate
observations, so the fix is **one generalised clause and not one clause per
gap** — a per-gap fix leaves the next object class uncovered by the identical
argument.

**The fourth gap is a different kind and this clause deliberately does not
reach it.** W4's `:2383` and `:2401` are about Kubernetes Pods and pool
entries the product provisions on a run's behalf outside its own storage and
then does not reclaim. Nothing about them is *stored wrong*; they are simply
not released. That is a lifecycle gap whose nearest invariant is **I3**, whose
subject is *locks*. It is open, it is recorded at `FINDINGS.md:2473` with its
own prescribed amendment (generalise I3's subject to resources the system
acquires or provisions on a run's behalf), and **stretching this clause to
cover it would be the exact move `FINDINGS.md:1509` forbids.**

**Why the amended text is checkable — the property the campaign requires is
that an invariant be contradicted by its own text, not by its spirit
(`FINDINGS.md:1509`), and a clause loose enough to cover everything decides
nothing.** Two devices keep it tight:

1. **The subject is a closed enumeration, not "state".** Five object classes,
   all of them things the product writes and later serves back. It therefore
   does **not** reach run/step/approval/audit rows (I7's subject), lock rows
   (I3's), execution counts (I2's), terminality (I1's), the agent's scratch
   workspace, or any externally-provisioned execution resource. A scenario
   that wants I4 must name which of the five it damaged.
2. **The predicate is three testable conjuncts, each with an instrument.**
   (a) compares stored bytes/rows against what the run wrote — countable;
   (b) is a read-back — the object is fetched and, for secrets, decrypted;
   (c) compares the identity in the request against the identity of what came
   back — both are recorded. Clause (b) is bounded by "for as long as the
   product still serves it", so an object the operator removed through a
   documented route is not a breach, only one the product still advertises.

**Worked check, against entries that exist rather than against hypotheticals
— each is contradicted or not on the amended text alone:**

| Entry | Clause | Contradicted? |
|---|---|---|
| W1-2 `FINDINGS.md:179` — 38 log lines dropped at step end, no marker | (a) log rows | yes |
| W3-4 `:1521` — 2,002 emitted lines stored and served as 8,493, reordered | (a) log rows | yes |
| W4-1 `:2334` — 21 lines of a Succeeded run lost, contiguous `seq` | (a) log rows | yes |
| W3-3 `:1620` — secret still listed, permanently undecryptable on all replicas | (b) secrets | yes |
| W3-1 `:1938` — a good 16 MiB cache entry overwritten with 1.9 MiB of debris | (a) cache entries | yes |
| W5-2 `:3004` — `downloadArtifact.runId` ignored; another run's artefact served | (c) artifacts | yes |
| W3-1 `:1986` — cache payload orphaned from its `.meta`: unexpirable, still served | — | **no** — the payload matches what was written, is readable, and the exact key served the object it named. **Reclamation is not integrity.** Stays on the contract limb |
| W4 `:2383`, `:2401` — unreclaimed Pods and pool entries | — | **no** — not an object class the product persists, and nothing about them is stored wrong. The I3 gap, still open |

**Consequences, executed in the same change as this amendment.** The four
entries the campaign named were re-classified (`:1938`, `:1620`, `:1986`,
`:3004`) and **three of the four moved**; the published limb split was
re-derived rather than taken from the campaign's prediction. Both are recorded
at `## Invariant amendment` at the end of `test/edgecase/FINDINGS.md`, which is
the authority on entry dispositions.
