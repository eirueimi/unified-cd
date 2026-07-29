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
- **Repro:** `test/edgecase/scenarios/w0-1-pgbouncer-txn-pooling.md`, probe section (steps 1-2). Baseline (no overlay) confirmed healthy first: single leader (`controller2`), 2 runs in ~90s from a 1/min schedule, SSE streaming live `tick N` events. Probe stacked `../edgecase/compose/pgbouncer.override.yaml` (all three controllers' `UNIFIED_DB_DSN` routed through `pgbouncer` in `POOL_MODE: transaction`) onto the same compose file and repeated the job+schedule apply.
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
  Run count grew from 0 to 26 in ~10 minutes (expected-healthy ~10). No `advisory`/`lock`/`unlock`-related log line ever appeared on any controller — `grep -i "advisory\|lock\b\|unlock"` over all three logs matched nothing but the routine `"postgres pool configuration"` line. `pg_locks` (queried directly against `postgres`, bypassing the pooler) showed a stable set of 6 granted advisory locks held by only 3 distinct Postgres backend pids (68, 69, 282) throughout — i.e. PgBouncer's small pool of *real* server-side sessions is shared/multiplexed across the three controllers' logical connections, so a lock "held" by one controller's client connection is actually attached to a shared backend session another controller's transaction can just as legitimately borrow next. That is the mechanism behind the split-brain: it produces zero Postgres-level lock errors because, from Postgres's point of view, every acquire/release is happening on a real, valid session — the pooling layer is what silently reassigns "the same" session identity to different application-level leaders. Restarting `controller1` (kill+recreate) and waiting 60s did not strand any lock (still 6 granted, no dead/orphaned pids) and did not stop the split-brain — it simply re-joined and re-claimed leadership on its own 90s later, unprompted. Killing the apparent last-elected leader (`controller3`) did not halt scheduling either: runs kept accumulating afterward (26 -> 32 over ~2 minutes) at roughly the reduced (2-leader) duplicate rate, driven by the two survivors.
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
