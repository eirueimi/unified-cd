# W1-2 — PostgreSQL outage/restart during a running job

- **Invariants:** I1 (run accounting), I5 (bounded recovery)
- **Stack:** plain `test/ha` compose, no overlay.
- **Workload:** `test/edgecase/workloads/longrun.payload.json` (job
  `edge-longrun`, native step: `for i in $(seq 1 300); do echo "tick $i"; sleep 1; done`
  — ~300s of execution, one `tick N` log line per second).
- **Caveat — this is outage+restart, not true primary/standby failover.**
  `test/ha/docker-compose.ha.yaml` runs a single `postgres` service; there is
  no replica to fail over to. This scenario kills that single instance and
  brings the *same* container back up, exercising the controllers'
  reconnect/re-elect path and the agent's tolerance of a DB-side (as opposed
  to controller-side, cf. W1-1) outage. It does **not** exercise a Patroni-style
  primary+standby switchover, connection-string change, or data-loss-on-failover
  scenario — those are out of scope for this compose file. Treat findings here
  as "controllers survive Postgres being unreachable for a while and coming
  back," not as validation of any specific HA-Postgres topology.
- **Mechanism under test:** while the step is mid-execution on the agent,
  `postgres` is killed (both a clean `SIGTERM` and a `SIGKILL` variant are
  run, back to back as scenario A and B) and brought back with
  `docker compose up -d postgres`. Every controller's `/readyz` pings the DB
  directly (`internal/controller/server.go`: `s.store.Ping(pingCtx)`, 3s
  timeout) so `/readyz` should flip to 503 while Postgres is down and back to
  200 once it recovers and the controller's pool reconnects. Scheduler leader
  election and the SSE log-streaming path both depend on a Postgres session
  (advisory lock for leader election, `LISTEN`/`NOTIFY` for SSE fan-out per
  `docs/high-availability.md` §External Dependency Redundancy: "After a
  PostgreSQL failover, controllers reconnect automatically and leader
  election re-runs.") — this scenario checks whether that promise holds for
  both the leader-election path and the LISTEN/NOTIFY path, and whether the
  agent's local execution + buffered reporting (same `LogPusher` mechanism as
  W1-1) is enough to let the run finish untouched by a DB-only outage.

## Baseline (healthy stack, no overlay)

```bash
cd test/ha
docker compose -f docker-compose.ha.yaml up -d --build
curl -fsS localhost:18080/readyz            # expect: ok (retry until up)
```

BASELINE GATE — confirm before injecting anything:

```bash
# exactly one leader elected:
for c in controller1 controller2 controller3; do
  echo "== $c"; docker compose -f docker-compose.ha.yaml logs $c 2>/dev/null | grep -c "scheduler became leader"
done
curl -s -o /dev/null -w '%{http_code}\n' localhost:18080/readyz   # expect 200
```

If baseline is broken (no leader, readyz not 200), STOP and report BLOCKED
with the evidence above rather than proceeding to injection.

## Probe

Apply the job and trigger a run:

```bash
curl -fsS -X POST localhost:18080/api/v1/jobs \
  -H "Authorization: Bearer ha-admin-token" -H "Content-Type: application/json" \
  --data-binary @../edgecase/workloads/longrun.payload.json

curl -fsS -X POST localhost:18080/api/v1/runs \
  -H "Authorization: Bearer ha-admin-token" -H "Content-Type: application/json" \
  -d '{"jobName":"edge-longrun"}'
# capture .id from the JSON response as RUN_ID
```

Wait until the run is `Running` and ticking (~30s), then attach SSE to
confirm live delivery before injecting:

```bash
for i in $(seq 1 4); do
  date; curl -fsS "localhost:18080/api/v1/runs/<RUN_ID>" -H "Authorization: Bearer ha-admin-token"
  sleep 10
done
curl -N --max-time 15 "localhost:18080/api/v1/runs/<RUN_ID>/events" -H "Authorization: Bearer ha-admin-token"
```

Record the pre-injection `"scheduler became leader"` counts per controller
(baseline gate output above) — these are the "before" counts for the leader-
thrash check after each variant.

### Injection variant A — hard kill (`SIGKILL`)

```bash
date
../edgecase/tools/inject.sh kill-hard postgres
```

Observe for ~45s: per-controller `/readyz` should turn 503 (each controller
pings the DB directly), agent report retries should be visible in the agent
logs, and SSE should stall or drop:

```bash
for i in $(seq 1 5); do
  date
  for c in controller1 controller2 controller3; do
    printf '%s: ' "$c"
    docker compose -f docker-compose.ha.yaml exec -T "$c" wget -qO- -S http://localhost:8080/readyz 2>&1 | head -1
  done
  sleep 9
done

docker compose -f docker-compose.ha.yaml logs agent1 agent2 --tail 50
```

Bring postgres back:

```bash
date
docker compose -f docker-compose.ha.yaml up -d postgres
```

Observe recovery — poll each controller's `/readyz` until it returns 200 and
record the wall-clock time it flips (per controller, so a difference between
replicas is visible):

```bash
for i in $(seq 1 12); do
  date
  for c in controller1 controller2 controller3; do
    printf '%s: ' "$c"
    docker compose -f docker-compose.ha.yaml exec -T "$c" wget -qO- -S http://localhost:8080/readyz 2>&1 | head -1
  done
  sleep 5
done
```

Leader re-election — exactly one re-claim expected, no thrash:

```bash
for c in controller1 controller2 controller3; do
  echo "== $c"; docker compose -f docker-compose.ha.yaml logs $c 2>/dev/null | grep "scheduler became leader"
done
```

LISTEN/NOTIFY resubscribed — attach SSE *after* `/readyz` is back to 200 and
confirm live `tick` lines arrive (not just a backlog replay):

```bash
curl -N --max-time 15 "localhost:18080/api/v1/runs/<RUN_ID>/events" -H "Authorization: Bearer ha-admin-token"
```

Wait for the run to finish, then apply the same line-accounting check as
W1-1 (I4, shared approach):

```bash
for i in $(seq 1 10); do
  date; curl -fsS "localhost:18080/api/v1/runs/<RUN_ID>" -H "Authorization: Bearer ha-admin-token"
  sleep 15
done

curl -fsS "localhost:18080/api/v1/runs/<RUN_ID>/logs?after=0" \
  -H "Authorization: Bearer ha-admin-token" > /path/to/scratchpad/w1/w1-2/run-a-logs.json

curl -fsS "localhost:18080/api/v1/runs/<RUN_ID>/logs/stats" \
  -H "Authorization: Bearer ha-admin-token"

# small python counter in scratchpad is fine here too, mirroring W1-1:
# count total tick lines, check for duplicates, check seq is contiguous 1..300
```

### Injection variant B — soft kill (`SIGTERM`, clean shutdown)

**Caveat — SIGTERM alone will NOT reliably bring postgres down here, and the
probe will stall if you wait on it.** `docker compose kill -s SIGTERM
postgres` puts Postgres into "Smart Shutdown": it immediately refuses *new*
connections but waits indefinitely for existing sessions to close on their
own before the postmaster exits. In this compose stack the controllers hold
persistent pooled connections that are never voluntarily closed, so
`/readyz` (which pings the DB over an already-open pooled connection) keeps
returning `200` and the postgres container stays `Up ... (unhealthy)`
forever — confirmed here over a 3+ minute observation window with zero
progress toward the container actually stopping. Do not treat a stuck
`Up (unhealthy)` state as "still shutting down, give it more time"; it will
not resolve on its own with this pool configuration. Escalate to a hard kill
to actually force the outage before proceeding to the recovery steps below:

```bash
date
../edgecase/tools/inject.sh kill-soft postgres
# observe for ~1-2 minutes: readyz stays 200 on all controllers, postgres
# container reports "Up ... (unhealthy)" and pg_isready reports "rejecting
# connections" but never actually exits — this is expected, not a bug in
# unified-cd (it's standard PostgreSQL SIGTERM/Smart-Shutdown semantics
# colliding with a connection pool that doesn't evict idle connections).
# Once confirmed stuck, force it down to continue the scenario:
date
../edgecase/tools/inject.sh kill-hard postgres
```

(An alternative that avoids the container-level signal ambiguity entirely,
not required but worth considering for a future revision: run
`docker compose -f docker-compose.ha.yaml exec postgres pg_ctl stop -m fast`
inside the container instead of a compose-level `kill`, which is closer to a
"true" clean-shutdown probe since `-m fast` still terminates existing
sessions immediately rather than waiting for them.)

Use the same observation loops (per-controller `/readyz`, agent logs, bring
postgres back with `up -d postgres`, leader re-election grep, SSE re-attach,
line-accounting on the new RUN_ID). The point of variant B was originally to
compare controller reconnect latency (time from `up -d postgres` to
`/readyz` 200, per controller) against variant A's hard-kill figures — in
practice, once both are forced into a genuine outage this way, the more
interesting difference observed was *not* reconnect latency (which was
comparable) but whether the outage happened to still be ongoing at the exact
moment the running step finished locally (see FINDINGS.md for what that
exposes).

## Recording

FINDINGS entries (severity guidance):
- Run failed or was lost because of the DB blip (never reaches `Succeeded`,
  or the controller has no record of it post-recovery) = **major** (I1) —
  the docs promise controllers reconnect automatically and in-progress runs'
  agent-side execution + buffered reporting should carry the run to
  completion once Postgres is back.
- Leader thrash (multiple `"scheduler became leader"` lines per controller,
  or overlapping claims across controllers) after a single outage/recovery
  cycle = **minor**.
- SSE not delivering live lines after recovery until the *client*
  reconnects = expected client-side behavior, not itself a violation — the
  docs' contract is about the *server* reconnecting to Postgres and
  resubscribing LISTEN, not about a single already-open HTTP connection
  surviving a DB outage. Record precisely what was observed (did a **new**
  SSE connection opened after `/readyz` returned 200 receive live ticks
  promptly, or did it also stall/error) and judge violation-vs-observation
  against that server-side-reconnect contract, not against "the original
  curl session kept streaming."
- Missing or duplicated `tick` lines, or lines out of seq order = **major**
  (I4-equivalent check, shared with W1-1).
- Unbounded or unexpectedly long `/readyz` recovery time, or asymmetry
  between hard-kill and soft-kill reconnect latency large enough to suggest
  the pool doesn't detect a dead connection promptly = evaluate against I5;
  note the measured durations either way.

## Teardown

```bash
docker compose -f docker-compose.ha.yaml down -v
```

Verify nothing is left running:

```bash
docker compose -f docker-compose.ha.yaml ps -a
```
