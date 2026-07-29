# W1-1 — all-controller restart during a long-running run

- **Invariants:** I1 (run accounting), I4 (log completeness/ordering — no
  lost, duplicated, or reordered log lines), I5 (bounded recovery)
- **Stack:** plain `test/ha` compose, no overlay.
- **Workload:** `test/edgecase/workloads/longrun.payload.json` (job
  `edge-longrun`, native step: `for i in $(seq 1 300); do echo "tick $i"; sleep 1; done`
  — ~300s of execution, one `tick N` log line per second).
- **Mechanism under test:** while the step is mid-execution on the agent, all
  three controllers go down together (SIGKILL) and stay down for ~60s before
  being brought back. The agent's `LogPusher` (per `internal/agent/runner.go`)
  keeps buffering log lines locally and executing the step regardless of
  controller reachability; failed flushes queue in a `pending` buffer (cap
  1MiB, drop-oldest with a `[N log line(s) dropped: controller unreachable]`
  marker on overflow — see `docs/troubleshooting.md`). This scenario checks
  that ordinary-duration outages (well under the 1MiB pending cap at this
  workload's line rate) survive with zero loss, and that the run still
  reaches `Succeeded` once the controllers come back.

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

> **Phase dependence — this is what makes or breaks the "zero log loss"
> headline, so do not let it drift.** The sequence above consumes ~55s before
> injection (the 4x10s poll loop plus the 15s SSE check), on top of the ~30s
> startup wait, and the outage below is held ~60-80s — so against a 300s step
> the whole outage necessarily starts *and ends* well before the step reaches
> its natural end. That is the only reason this scenario sees zero loss: the
> 2s auto-flush ticker gets many chances to drain `LogPusher.pending` before
> `finishLogs` ever runs the bounded 5s step-end `Flush`. A re-runner who is
> slower here (longer waits, a longer outage, or a shorter step) can push the
> outage across the step's completion and **will** lose the log tail with no
> drop marker. That is not a contradiction of this scenario's result — it is
> W1-2's major I4 finding reproducing (see FINDINGS.md, "Postgres outage
> overlapping a step's natural completion..."), and it should be attributed
> there rather than re-filed or read as a regression in W1-1.

Inject: kill all three controllers hard, one after another, then confirm the
LB has no live upstream for ~60s:

```bash
../edgecase/tools/inject.sh kill-hard controller1
../edgecase/tools/inject.sh kill-hard controller2
../edgecase/tools/inject.sh kill-hard controller3

for i in $(seq 1 6); do
  date; curl -s -o /dev/null -w '%{http_code}\n' localhost:18080/readyz
  sleep 10
done
# expect 502/503 (or curl connection error) throughout this loop

docker logs unified-cd-ha-agent1-1 --tail 50   # or agent2, whichever claimed the run
docker logs unified-cd-ha-agent2-1 --tail 50
```

Bring all three controllers back:

```bash
date
docker compose -f docker-compose.ha.yaml up -d controller1 controller2 controller3
```

Observe recovery: one leader re-elected, run resumes reporting, reaches
`Succeeded`, SSE re-attaches, timing from controller-up to first successful
agent report (I5):

```bash
for i in $(seq 1 8); do
  date; curl -fsS "localhost:18080/api/v1/runs/<RUN_ID>" -H "Authorization: Bearer ha-admin-token"
  sleep 15
done

curl -N --max-time 15 "localhost:18080/api/v1/runs/<RUN_ID>/events" -H "Authorization: Bearer ha-admin-token"
```

**Line accounting (I4)** — once the run is `Succeeded`, fetch the full log
and count `tick` lines. Try the tail-logs route first (returns up to 1000
lines per call, `after=0` gets everything for a run this size); fall back to
the archive route if the DB copy has already been trimmed
(`internal/controller/api_runs.go`: `GET /runs/{id}/logs` ->
`handleTailLogs`, `GET /runs/{id}/logs/archive` -> `handleLogsArchive`,
`GET /runs/{id}/logs/stats` -> `handleLogStats` for a quick count
cross-check):

```bash
curl -fsS "localhost:18080/api/v1/runs/<RUN_ID>/logs?after=0" \
  -H "Authorization: Bearer ha-admin-token" > /path/to/scratchpad/w1/run-logs.json

curl -fsS "localhost:18080/api/v1/runs/<RUN_ID>/logs/stats" \
  -H "Authorization: Bearer ha-admin-token"

# count/verify tick lines from the captured file (jq or grep -c), check for
# duplicates and monotonic seq/ordering, and grep for the drop marker:
grep -c '"line":"tick ' /path/to/scratchpad/w1/run-logs.json
grep '"line":"tick ' /path/to/scratchpad/w1/run-logs.json | grep -o '"line":"tick [0-9]*"' | sort | uniq -d   # expect empty (no dup ticks)
grep -o '\[.*log line(s) dropped: controller unreachable\]' /path/to/scratchpad/w1/run-logs.json
```

## Recording

FINDINGS entries (severity guidance):
- Missing or duplicated `tick` lines, or lines out of seq order = **major**
  (I4).
- A `[N log line(s) dropped: controller unreachable]` marker whose count
  matches an actual gap = expected-mechanism **observation**, not a
  violation — record the count and whether it's consistent with the outage
  window and pending-buffer cap.
- Run lost (never reaches `Succeeded`, or the controller has no record of it
  post-recovery) = **major** (I1).
- Unbounded or unexpectedly long recovery time (controller-up to first
  successful agent report, or to run reaching `Succeeded`) = evaluate against
  I5; note the measured duration either way.

## Teardown

```bash
docker compose -f docker-compose.ha.yaml down -v
```

Verify nothing is left running:

```bash
docker compose -f docker-compose.ha.yaml ps -a
```
