# Campaign Findings

One entry per invariant violation or notable observation. Reported as one
batch at the end of the campaign; the operator prioritizes.

Severity: **critical** (data loss / silent corruption / security),
**major** (incorrect visible behavior, unbounded recovery),
**minor** (diagnosability, docs gap, cosmetic).

Entry template:

    ## <scenario-id> — <one-line title>
    - **Invariant:** I<n> (<name>)
    - **Severity:** critical | major | minor
    - **Repro:** <commands / probe name>
    - **Observed:** <what happened, with log/query excerpts>
    - **Expected:** <what the docs/spec promise>
    - **Notes:** <fix ideas, related known issues>

---

## W0-1 — PgBouncer transaction pooling causes silent leader-election split-brain and duplicate schedule fires
- **Invariant:** I1 (run accounting)
- **Severity:** major
- **Repro:** `test/edgecase/scenarios/w0-1-pgbouncer-txn-pooling.md`, probe section (observation steps 1-5). Baseline (no overlay) confirmed healthy first: single leader (`controller2`), 2 runs in ~90s from a 1/min schedule, SSE streaming live `tick N` events. Probe stacked `../edgecase/compose/pgbouncer.override.yaml` (all three controllers' `UNIFIED_DB_DSN` routed through `pgbouncer` in `POOL_MODE: transaction`) onto the same compose file and repeated the job+schedule apply.
- **Observed:** All three controllers independently logged `"scheduler became leader"` within an 11-minute window, with no corresponding "lost leadership"/"stepped down" message ever logged by any of them:
  ```
  controller1-1 | {"time":"2026-07-29T01:47:53.589Z","level":"INFO","msg":"scheduler became leader"}
  controller2-1 | {"time":"2026-07-29T01:57:06.415Z","level":"INFO","msg":"scheduler became leader"}
  controller3-1 | {"time":"2026-07-29T01:57:09.477Z","level":"INFO","msg":"scheduler became leader"}
  controller1-1 | {"time":"2026-07-29T02:05:18.889Z","level":"INFO","msg":"scheduler became leader"}   # re-claimed, no restart/kill involved
  ```
  From the moment all three had claimed leadership, `edge-every-minute` fired **3x per cron tick** instead of once (`GET /api/v1/runs?jobName=edge-tick` sample, one minute's worth):
  ```
  createdAt: 2026-07-29T01:57:06.418461Z  triggeredBy: schedule:edge-every-minute
  createdAt: 2026-07-29T01:57:09.479238Z  triggeredBy: schedule:edge-every-minute
  createdAt: 2026-07-29T01:57:54.540495Z  triggeredBy: schedule:edge-every-minute
  ```
  Run count grew from 0 to 26 in ~10 minutes (expected-healthy ~10). No `advisory`/`lock`/`unlock`-related log line ever appeared on any controller — `grep -i "advisory\|lock\b\|unlock"` over all three logs matched nothing but the routine `"postgres pool configuration"` line. `pg_locks` (queried directly against `postgres`, bypassing the pooler) showed a stable set of 6 granted advisory locks held by only 3 distinct Postgres backend pids (68, 69, 282) throughout (observed interactively; raw query output not captured — re-capture when this scenario is promoted to a permanent test) — i.e. PgBouncer's small pool of *real* server-side sessions is shared/multiplexed across the three controllers' logical connections, so a lock "held" by one controller's client connection is actually attached to a shared backend session another controller's transaction can just as legitimately borrow next. That is the mechanism behind the split-brain: it produces zero Postgres-level lock errors because, from Postgres's point of view, every acquire/release is happening on a real, valid session — the pooling layer is what silently reassigns "the same" session identity to different application-level leaders. Restarting `controller1` (kill+recreate) and waiting 60s did not strand any lock (still 6 granted, no dead/orphaned pids) and did not stop the split-brain — it simply re-joined and re-claimed leadership on its own 90s later, unprompted. Killing the apparent last-elected leader (`controller3`) did not halt scheduling either: runs kept accumulating afterward (26 -> 27 -> 28 -> 28 -> 30 -> 30 over ~106s) at roughly the reduced (2-leader) duplicate rate, driven by the two survivors.
- **Expected:** `docs/high-availability.md` requires session pooling and states that transaction pooling "breaks advisory locks and NOTIFY." The expected failure mode per the docs would be lock breakage; what we could not tell from the docs alone is whether that breakage is loud (errors an operator could alert on) or silent. It is the latter: no error, warning, or lock-contention message of any kind was logged anywhere in the stack; the only externally visible symptom is the schedule firing 2-3x too often and multiple `"scheduler became leader"` lines across replicas that a log-scraping alert would need to specifically watch for.
- **Notes:** Recommend the docs/runbook explicitly call out that the failure signature is "duplicate `scheduler became leader"` lines across >1 controller ID" and "run count exceeding schedule cadence," since there is no explicit split-brain or lock-loss error to grep for. A cheap mitigating fix: emit a "scheduler lost leadership" (or periodic leader-lock renewal failure) log line distinct from "became leader," so flapping is visible without cross-referencing run timestamps. Also worth an invariant check (I1) at the run-creation layer: reject/dedupe schedule-triggered runs for the same schedule tick within a short window, independent of leader-election correctness, as defense in depth.

## W0-1 — Controllers crash-loop with no DB-connect retry at startup; pgbouncer overlay reliably triggers it via a boot race
- **Invariant:** I5 (bounded recovery — diagnosability)
- **Severity:** minor
- **Repro:** Same probe as above, `docker compose -f docker-compose.ha.yaml -f ../edgecase/compose/pgbouncer.override.yaml up -d --build` cold start (not a steady-state observation — this happened on the very first `up`).
- **Observed:** Two of three controllers exited immediately on first boot because they queried Postgres (via `pgbouncer`) before PgBouncer's listener/DNS record was fully live, and neither retries:
  ```
  controller2-1 | {"level":"ERROR","msg":"store init","error":"ping postgres api pool: failed to connect ... connection refused"}
  controller3-1 | {"level":"ERROR","msg":"store init","error":"ping postgres api pool: ... lookup pgbouncer on 127.0.0.11:53: no such host"}
  ```
  Both processes then exited(1) rather than retrying, which in turn made `nginx` (whose upstream config lists all three controllers by DNS name at startup) fail to start at all, since Docker's embedded DNS had already dropped the dead containers' records:
  ```
  nginx-1 | 2026/07/29 01:47:53 [emerg] 1#1: host not found in upstream "controller2:8080" in /etc/nginx/nginx.conf:7
  ```
  This cascaded to `agent-enroll` looping on `dial tcp: lookup nginx ...: no such host` for its full 60-retry budget and exiting with `controller never became ready for agent1`, leaving the compose `up -d --build` command itself reporting a failure even though `postgres`, `pgbouncer`, and `controller1` were all healthy. Manually re-running `docker compose up -d controller2 controller3 nginx` (letting them retry against an already-warm pgbouncer) then `up -d agent-enroll agent1 agent2` recovered the stack fully with no further errors.
- **Expected:** A transient dependency race at boot (a sibling container's DNS/listener not yet ready) is the kind of condition a clustered controller should retry through, not crash-exit on — especially since `postgres`/`pgbouncer` already gate `depends_on: condition: service_healthy` for the DB but there is no equivalent healthcheck/retry for the controller-to-pgbouncer hop or for nginx-to-controller DNS resolution.
- **Notes:** Not unique to transaction-pooling mode — a plain extra network hop (any TCP proxy in front of Postgres) would likely reproduce it — but the pgbouncer overlay is what surfaced it here since it's the first scenario in this campaign to insert a proxy between controller and Postgres. Recommend either a startup retry loop on `store init` (bounded, with backoff) or a `depends_on: condition: service_healthy` + healthcheck on the `pgbouncer` service itself so controllers don't race it.

## W0-1 — SSE/NOTIFY log streaming keeps working under transaction pooling, despite the docs' blanket claim
- **Invariant:** I5 (bounded recovery — diagnosability / docs accuracy)
- **Severity:** minor
- **Repro:** Same probe stack (pgbouncer overlay active, split-brain already underway). Attached to a currently-running run's SSE endpoint: `curl -N --max-time 15 "localhost:18080/api/v1/runs/cc467bdc-8853-48b8-9973-b4e5ec609ada/events" -H "Authorization: Bearer ha-admin-token"`.
- **Observed:** Log lines streamed live and in order, same shape as the healthy baseline — no stall, no gap, no reconnect needed:
  ```
  data: {"type":"log","seq":541,"stepIndex":0,"stream":"stdout","line":"tick 1","timestamp":"2026-07-29T02:02:57.998135Z"}
  data: {"type":"log","seq":542,"stepIndex":0,"stream":"stdout","line":"tick 2","timestamp":"2026-07-29T02:02:57.998135Z"}
  data: {"type":"log","seq":543,"stepIndex":0,"stream":"stdout","line":"tick 3","timestamp":"2026-07-29T02:02:59.99823Z"}
  ...
  data: {"type":"log","seq":552,"stepIndex":0,"stream":"stdout","line":"tick 12","timestamp":"2026-07-29T02:03:08.005493Z"}
  ```
  (full capture not committed; `seq` increments continuously, `curl` received bytes throughout the 15s window rather than blocking until timeout.)
- **Expected:** `docs/high-availability.md` states transaction pooling "breaks advisory locks and NOTIFY," which reads as a blanket claim covering both mechanisms. Advisory locks did break (see the split-brain entry above), but NOTIFY-driven SSE delivery did not visibly break in this run — an operator reading the docs literally would expect log streaming to also stall or go silent under this misconfiguration, and it didn't.
- **Notes:** Either the controller's SSE/log-delivery path doesn't actually depend on LISTEN/NOTIFY the way the docs imply (e.g. it falls back to polling, or the connection happened to land on a session that kept a live LISTEN registration for the run's duration), or NOTIFY breakage requires conditions this run didn't hit (e.g. longer runs, more concurrent listeners, a pooler reconnect mid-stream). Recommend either correcting the docs to be precise about which mechanism breaks (advisory locks, confirmed) versus which is untested/unconfirmed (NOTIFY), or a follow-up scenario specifically designed to stress LISTEN/NOTIFY under transaction pooling (e.g. a long-running job with a mid-run pgbouncer server-connection cycle) before asserting NOTIFY breakage as fact.

## W0-2 — TZ-divergent replicas disagree on whether a schedule is due for the same physical instant
- **Invariant:** I1 (run accounting)
- **Severity:** major
- **Repro:** `go test -tags edgeprobe ./internal/controller -run TestEdgeProbe_TZDivergentLeaders -v` (`internal/controller/edgeprobe_scheduler_test.go`).
- **Observed:** A UTC-context check and a JST-context check of the exact same schedule (`"0 9 * * *"`) at the exact same physical instant (00:05 UTC vs 00:06 UTC — one minute apart, but the schedule's own `next`/due computation is evaluated against `now.In(<replica's Location>)`) reach opposite due/not-due conclusions:
  ```
  edgeprobe_scheduler_test.go:134: UTC-leader fired=0, JST-takeover fired=1, last_fired_at=2026-07-29 09:00:00 +0900 JST — TZ divergence causes skip/dup? utc=0 jst=1
  ```
  The UTC-context leader sees the schedule as ~9h from due (`fired=0`); the JST-context leader, checking essentially the same moment, sees `09:06 JST` and fires it (`fired=1`). Nothing in the run-creation path detects or reconciles this — whichever replica happens to be leader when it evaluates `now` in its own Location wins, silently.
- **Expected:** Cron due-ness should be a property of the physical instant plus the schedule definition, not of which `time.Location` happens to be attached to the `now` value a given leader passes into `dsl.NextCronTime`/`checkAndFireSchedules`. A schedule should fire exactly once per intended occurrence regardless of which replica (or which replica's local/env TZ) is currently leader.
- **Notes:** This is the most concerning result from the W0-2 batch (per Task 5's analysis) because it is a live architectural landmine, not a rare boundary condition: any HA deployment where replica hosts/containers don't have `TZ` pinned identically (or where `now` is constructed via `time.Now()` on hosts with different local zones, or passed through `.In(...)` inconsistently anywhere in the call chain) is exposed to this on every leader handoff, producing either a duplicate fire (if the new leader's Location makes an already-fired occurrence look due again) or a skipped fire (the reverse). Recommend: (1) normalize `now` to UTC at every entry point that computes cron due-ness, never trusting an arbitrary Location on the caller-supplied timestamp; (2) add a startup/health check that asserts all controller replicas report the same effective TZ; (3) treat this as a strong argument for pinning `TZ=UTC` in the HA compose/deployment manifests explicitly rather than leaving it to host defaults.

## W0-2 — Catch-up window boundary: an occurrence missed by one cron interval (5 min) past the 1h window is silently and permanently dropped
- **Invariant:** I1 (run accounting), I5 (bounded recovery — diagnosability)
- **Severity:** major
- **Repro:** `go test -tags edgeprobe ./internal/controller -run TestEdgeProbe_CatchupWindowBoundary -v` (`internal/controller/edgeprobe_scheduler_test.go`).
- **Observed:** The closed lower edge of the catch-up window (`next == now-60m` exactly) correctly fires:
  ```
  edgeprobe_scheduler_test.go:81: boundary next==now-1h: fired=1 last_fired_at=2026-07-29 20:00:00 +0900 JST
  ```
  But one cron interval (5 minutes, the `*/5` schedule's own granularity) further out (`next == now-65m`, from a `last_fired_at` seeded at `now-66m` = 19:54 JST), the occurrence is dropped with zero signal of any kind:
  ```
  edgeprobe_scheduler_test.go:89: just outside window: new-runs=0 (silent skip? true) last_fired_at=2026-07-29 19:55:00 +0900 JST
  ```
  No run is created, and there is no log line, error, warning, or metric distinguishing "occurrence intentionally outside catch-up window, dropped" from "nothing was due yet." `last_fired_at` IS advanced to the skipped occurrence (19:55 JST, i.e. the `next` value) — `internal/controller/scheduler.go:197-201` takes this path unconditionally when `next < windowStart`, so the run is dropped but the bookkeeping still moves on. The only trace that an occurrence was silently lost is the absence of a run row a human would have to know to go looking for; nothing distinguishes this `last_fired_at` advance from the one that happens on a normal fire.
- **Expected:** A 1-hour catch-up window is a reasonable, presumably intentional bound (I5 — "bounded recovery"), but a schedule occurrence that falls outside it should be a detectable event, not indistinguishable from normal steady-state operation. An operator recovering from an outage that lasted a few minutes longer than the window gets no indication that a tick was lost.
- **Notes:** Argued as **major** primarily because the missed occurrence is *permanently* lost, not merely delayed — no catch-up ever happens for it, unlike the backward-clock-step or DST-gap entries below where the affected occurrence is late but still eventually fires. A scheduled run that was due and never executes at all is incorrect behavior in its own right (silent data loss against I1, run accounting), independent of how loud or quiet the failure path is. The total silence (no log line, error, warning, or metric distinguishing this from steady-state) is an aggravating factor that compounds the severity — it means operators cannot even detect after the fact that an occurrence was dropped — but is not itself the primary basis for the major rating; on diagnosability alone this would only be minor per this file's own severity rubric. Not critical, because it's a single bounded occurrence rather than ongoing/systemic corruption, and the window's existence is itself a documented, intentional design choice. Per a reading of `scheduler.go:197` (`next < windowStart → too old to fire`), this silent-drop path applies to any occurrence outside the window by any margin, not just the 5-minute case the probe exercised — that broader claim is inferred from the code, not directly demonstrated by this probe. Recommend at minimum a `WARN`-level log line (`"schedule occurrence outside catch-up window, skipped"`, with schedule ID and how far outside) whenever this path is taken.

## W0-2 — observation: backward clock step causes a bounded, self-healing stall equal to the skew magnitude
- **Invariant:** I1 (run accounting)
- **Severity:** minor
- **Repro:** `go test -tags edgeprobe ./internal/controller -run TestEdgeProbe_BackwardClockStep -v` (`internal/controller/edgeprobe_scheduler_test.go`).
- **Observed:**
  ```
  edgeprobe_scheduler_test.go:151: with future last_fired_at: fired=0 (every-minute schedule silent until wall clock catches up: true)
  ```
  A `last_fired_at` stamped 10 minutes in the future (e.g. from a leader with a fast/skewed clock) causes an every-minute schedule to stop firing when checked against the real, correct `now`. This is bounded and self-healing: because the scheduler only fires when the computed `next` time is after `last_fired_at`, the schedule resumes on its own — no operator action, no restart, no manual reset — as soon as real wall-clock `now` catches up past the erroneous future stamp. The stall duration equals the skew magnitude (~11 minutes for the tested 10-minute-ahead skew, given the every-minute cadence); no occurrence is permanently lost, and the schedule is back to normal cadence immediately once the clock passes the bad timestamp. Structurally this is the same recovery shape as the DST-gap entry below (single bounded delay, automatic resumption, no data loss), not an unbounded one.
- **Expected:** A future `last_fired_at` from clock skew is a fault condition; recovery should be bounded once the underlying skew is no longer ahead of real time. Observed behavior matches this: the delay is bounded by, and directly proportional to, the size of the skew, with no manual intervention required.
- **Notes:** Rated minor rather than major, consistent with the DST-gap entry's rating for the same recovery shape. Caveat/extrapolation beyond what was directly observed: the probe only exercised a 10-minute skew, and nothing in the observed logic caps how large a skew can be written to `last_fired_at` in the first place — a much larger erroneous future stamp (from a badly corrupted clock, NTP misconfiguration, or a bug elsewhere writing a bogus timestamp) would plausibly produce a proportionally longer stall of the same bounded/self-healing shape, but potentially large in wall-clock terms, and it would be silent and undetectable for its entire duration (no metric or log flags "schedule N minutes overdue due to a future last_fired_at"). This is inferred, not tested — the probe does not establish how large a skew the system will tolerate before writing `last_fired_at`, if there is any limit at all. If large-skew robustness is a real operational concern, recommend clamping/rejecting `last_fired_at` writes that are ahead of the server's own `now` (`pg.UpdateScheduleLastFiredAt`) as defense in depth, and/or a health check comparing each schedule's expected next-fire time to wall clock — a hardening suggestion, not evidence of an unbounded defect as tested.

## W0-2 — DST spring-forward gap: a daily schedule silently skips the transition day instead of firing near the gap
- **Invariant:** I1 (run accounting)
- **Severity:** minor
- **Repro:** `go test -tags edgeprobe ./internal/controller -run TestEdgeProbe_DSTGap -v` (`internal/controller/edgeprobe_scheduler_test.go`).
- **Observed:**
  ```
  edgeprobe_scheduler_test.go:46: DST gap: next('30 2 * * *') after 2026-03-08 01:00:00 -0500 EST = 2026-03-09 02:30:00 -0400 EDT (skipped? true)
  ```
  `02:30` does not exist on 2026-03-08 in `America/New_York` (clocks jump 02:00 -> 03:00). `dsl.NextCronTime` does not fast-forward to the nearest valid time on the same day (e.g. 03:30) or fire once at the gap boundary — it skips the entire remainder of March 8th and returns March 9th's `02:30 EDT` instead, a ~24h delay versus the schedule's nominal daily cadence.
- **Expected:** Not explicitly documented behavior either way, but an operator reading `"30 2 * * *"` as "runs daily at 2:30am" would not expect a full calendar day to be skipped on the two-per-year DST transition dates without any indication.
- **Notes:** Kept at minor rather than major because the recovery is bounded and predictable (exactly one missed occurrence, resuming automatically the next day at the correct time — this is standard, if under-documented, behavior for cron libraries using wall-clock arithmetic over a `Location`), and it affects at most one occurrence twice a year for daily-or-coarser schedules in DST-observing zones. Worth a one-line callout in scheduling docs ("schedules that land in the spring-forward gap skip to the next valid occurrence rather than firing early/late on the same day") so this isn't a support-ticket surprise.

## W0-2 — observations (no violation)
- **Invariant:** I1 (run accounting)
- **Severity:** minor (observation — confirms designed/expected behavior, no defect)
- **Repro:** `go test -tags edgeprobe ./internal/controller -run 'TestEdgeProbe_DSTFold|TestEdgeProbe_BacklogDrainRate' -v` (`internal/controller/edgeprobe_scheduler_test.go`).
- **Observed:**
  - **DST fold** (ambiguous wall-clock hour repeats): calling `NextCronTime("30 1 * * *", ...)` twice in sequence across the November fold returns two distinct instants exactly one real hour apart, both labeled "01:30" in local wall-clock terms:
    ```
    edgeprobe_scheduler_test.go:60: DST fold: first=2026-11-01 01:30:00 -0400 EDT (2026-11-01 05:30:00 +0000 UTC) second=2026-11-01 01:30:00 -0500 EST (2026-11-01 06:30:00 +0000 UTC) — fires twice on the fold night? true
    ```
    This is the mirror image of the spring-forward gap finding above (ambiguous instant walked through as two firings instead of a nonexistent instant being skipped) and is standard `time.Location`-based wall-clock cron semantics, not a scheduler-specific defect — noted here as a known, low-probability (twice a year, one hour, DST-observing zones only) duplicate-fire source for the record.
  - **Backlog drain rate**: with a 30-minute backlog (6 due `*/5` occurrences queued) and 8 sequential calls to `checkAndFireSchedules`, the total run count climbs exactly 1-per-call for the first 6 calls and then flattens once the backlog is exhausted:
    ```
    edgeprobe_scheduler_test.go:107: call 1: total runs=1
    edgeprobe_scheduler_test.go:107: call 2: total runs=2
    edgeprobe_scheduler_test.go:107: call 3: total runs=3
    edgeprobe_scheduler_test.go:107: call 4: total runs=4
    edgeprobe_scheduler_test.go:107: call 5: total runs=5
    edgeprobe_scheduler_test.go:107: call 6: total runs=6
    edgeprobe_scheduler_test.go:107: call 7: total runs=6
    edgeprobe_scheduler_test.go:107: call 8: total runs=6
    ```
    Confirms the drain rate is exactly one backlog occurrence per `checkAndFireSchedules` call, matching the brief's prediction — at the production cadence of one check per minute, a 30-minute scheduler outage takes ~6 real minutes to fully drain. This is expected, bounded, and self-terminating behavior; no defect.
- **Expected:** Both match the behavior predicted going into the probe design (DST fold as documented cron-library wall-clock semantics; drain rate as a direct consequence of the one-occurrence-per-call design). Included for completeness/record, not as findings.
- **Notes:** No action needed. DST fold could be paired with the spring-forward gap callout in scheduling docs if that doc gets written.

## Checkpoint: W0 complete
- Scenarios run: W0-1 (pgbouncer transaction-pooling overlay, 3 findings entries), W0-2 (6 scheduler clock/TZ boundary probes: DST gap, DST fold, catch-up window boundary, backlog drain rate, TZ-divergent leaders, backward clock step)
- Classification rule: a FINDINGS entry is a **violation** when observed behavior contradicts an invariant (I1-I7) or a documented contract; it is an **observation** when behavior matched expectations but reveals a risk. This rule is applied consistently — later waves should count entries the same way.
- Violations: 6 (W0-1 split-brain [major], W0-1 crash-loop-no-retry [minor], W0-1 NOTIFY-docs-overclaim [minor], W0-2 TZ-divergent leaders [major], W0-2 catch-up-window silent-and-permanent skip [major], W0-2 DST-gap day-skip [minor]) — 3 from W0-1 + 3 from W0-2 (3 major: W0-1 split-brain, W0-2 TZ-divergent, W0-2 catch-up-window; 3 minor: W0-1 crash-loop-no-retry, W0-1 NOTIFY-docs-overclaim, W0-2 DST-gap). Observations: 2 (W0-2 backward-clock-step bounded stall — its own Expected confirms behavior matched expectations, no invariant violated; W0-2 combined DST-fold + backlog-drain-rate entry).
- Impact on later waves: The `edgeprobe`-tagged, build-tag-gated unit-test probe pattern established here (drive `checkAndFireSchedules`/`NextCronTime` directly with constructed `now` values, since libfaketime/time-namespace clock injection is not viable against static Go binaries) is directly reusable for W2's boundary scenarios (W2-6 scheduler-crash-mid-fire, W2-8 approval-timeout clock skew) without needing container-level clock manipulation. The compose-overlay pattern from W0-1 (stack a `.override.yaml` onto the base HA compose file to inject a single infra misconfiguration, diff against a confirmed-healthy baseline) is validated and ready for W3's cache/restore-race scenarios. Also carrying forward from W0-1: the docs' NOTIFY-breaks-under-transaction-pooling claim needs a caveat (advisory locks confirmed broken; NOTIFY/SSE was not observed to break in this probe) — flagged as a docs-follow-up candidate, not yet filed.

## W1-1 — observation: all-controller restart during a 300s run survives with zero log loss, zero duplication, and no run-completion delay
- **Invariant:** I1 (run accounting), I4 (log completeness/ordering), I5 (bounded recovery)
- **Severity:** minor (observation — confirms designed/expected behavior, no defect)
- **Repro:** `test/edgecase/scenarios/w1-1-all-controller-restart.md`. Baseline gate confirmed first (single leader `controller1`, `readyz` 200). Applied `edge-longrun` (`test/edgecase/workloads/longrun.payload.json`, 300 `tick N` lines at ~1/s), triggered run `39d87dcb-113c-4c80-aa43-c454734e8c4d` (claimed by `agent2`), confirmed live SSE delivery through `tick 54`, then `../edgecase/tools/inject.sh kill-hard` against `controller1`, `controller2`, `controller3` in sequence (04:54:07-04:54:09 UTC), confirmed `readyz` returned 502 for the full outage window, then `docker compose -f docker-compose.ha.yaml up -d controller1 controller2 controller3` (04:55:28-04:55:30 UTC).
- **Observed:**
  - **Outage window:** all three controllers down 04:54:09Z -> 04:55:30Z (~81s); `readyz` polled every 10s during this window returned `502` on every check, never `200`.
  - **Agent behavior during outage:** `agent2`'s claim/heartbeat/cancel-poller calls failed continuously with `http 502` (e.g. `{"msg":"claim","error":"http 502: response omitted","detached":true}`, `{"msg":"agent heartbeat failed", ...}`), but the agent process did not crash or abandon the run — it kept retrying in a loop, consistent with `LogPusher`'s buffer-and-retry design (`internal/agent/runner.go`).
  - **Recovery timing (I5):** `docker compose up -d controller1 controller2 controller3` was issued and completed within the `04:55:28Z`-`04:55:30Z` window. `controller1` re-acquired leadership (`"scheduler became leader"`) at `04:55:30.339751164Z` — within ~0.3-2.3s of that `up -d` window (leader-election-from-restart figure). The first successful post-recovery agent report (a `logs/bulk` flush, HTTP 204) landed on `controller1` at `04:55:33.144550456Z` — ~2.8s after leader re-election, and ~3-5s after the `up -d` window (first-report-from-restart figure). No unbounded stall.
  - **Run completion timing:** the run reached `Succeeded` at `04:58:05.740150Z` (per the `finish` call at `04:58:05.742860762Z` on `controller2`). The run was triggered at `04:53:04.683801Z` and the step began executing at `04:53:05.08Z`; 300 one-second ticks from that start predicts completion at ~`04:58:05Z` — i.e. the measured completion time matches the *uninterrupted* baseline expectation almost exactly. The ~81s controller outage added no visible delay to wall-clock run completion: the agent kept executing the local shell step throughout the outage and only the *reporting* was delayed/buffered, not the *execution*.
  - **Line accounting (I4):** fetched via `GET /api/v1/runs/<id>/logs?after=0` (`handleTailLogs` — sufficient in one call since total lines were well under the 1000-line page size; no trim occurred so the archive route was not needed) and cross-checked against `GET /api/v1/runs/<id>/logs/stats`, which reported `{"count":300,"minSeq":1,"maxSeq":300}`. Parsed the full log body: exactly 300 lines, all `tick 1`..`tick 300`, `seq` 1-300 strictly increasing with no gaps and no duplicates, tick numbers 1-300 with zero missing and zero extra values. No `[N log line(s) dropped: controller unreachable]` marker appeared anywhere in the log.
  - **SSE re-attach:** `curl -N .../events` against the completed run replayed the full 300-line backlog in order (`seq 1` = `tick 1` ... ) and closed cleanly; no stall, no error.
  - **I1 (run accounting):** `GET /api/v1/runs?jobName=edge-longrun` after the probe shows exactly one run, `Succeeded` — no duplicate or phantom runs from the outage/recovery.
  - **Leader-election note (not a violation):** during the kill sequence, `controller3` briefly claimed leadership at `04:54:07.528423572Z` — i.e. in the sub-second gap between `kill-hard controller1` (the prior leader) and the subsequent `kill-hard controller3` commands, before `controller3` itself was killed moments later. This is sequential failover (old leader's Postgres advisory-lock session dying releases the lock, a survivor grabs it), not overlapping/split-brain leadership like the W0-1 pgbouncer finding: `grep -c "scheduler became leader"` afterward shows `controller1: 2, controller3: 1` with no time overlap between any two claims, and no duplicate runs resulted. `grep -i "lock\|advisory"` across all controller logs matched nothing (as in the healthy W0-1 baseline).
- **Expected:** Per the failsafe-audit's LogPusher pending-buffer design (`docs/superpowers/plans/2026-07-16-resilience-2.md`), a bounded controller outage should be absorbed by client-side buffering with zero log loss and the run should still reach a terminal state once connectivity returns; this scenario's outage (~81s, ~300 tiny log lines, well under the 1MiB `maxPendingBytes` cap) is squarely inside that bound, and behavior matched exactly: zero loss, zero duplication, correctly ordered logs, one run record, bounded recovery.
- **Notes:** This run's outage produced far too little buffered data (a few KB of `tick N` lines) to exercise the `droppedLines`/marker eviction path (`appendPendingLocked`, 1MiB cap) — that path remains untested by this probe; a follow-up scenario with either a much longer outage or a much higher-throughput log producer would be needed to observe the drop-marker mechanism in action (tracked as a gap, not a defect). Also worth carrying into W2: the sub-second `controller3` leadership blip during the kill sequence is an artifact of injecting the three `kill-hard` calls sequentially rather than simultaneously — a scenario that kills all three truly atomically (e.g. `docker kill` in one shell command against all three at once) would remove even that transient handover and might be a cleaner variant if W2 wants a strictly zero-leader-transition version of this probe.
