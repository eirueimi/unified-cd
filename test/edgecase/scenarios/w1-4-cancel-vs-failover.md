# W1-4 — run cancellation racing controller failover

- **Invariants:** I1 (run accounting), I3 (mutex/lock release integrity)
- **Stack:** plain `test/ha` compose, no overlay.
- **Workload:** `test/edgecase/workloads/longrun.payload.json` (job
  `edge-longrun`, native step `tick`: `for i in $(seq 1 300); do echo "tick
  $i"; sleep 1; done` — 300 `tick N` lines at ~1/s, so a triggered run stays
  `Running` for ~5 minutes, plenty of headroom to land an injection mid-step).
  For the mutex variant, a small inline job applied ad hoc (see Race A, Part
  2 below) — `edge-longrun` itself carries no `spec.mutex`, so it cannot
  exercise I3 on its own.
- **Verified API/mechanism (do not re-derive):**
  - Cancel is `POST /api/v1/runs/{id}/cancel` -> transitions the run to
    `Cancelled` via `MarkRunFinished` (`internal/controller/api_runs.go:372`,
    `handleCancelRun`), and cascades to any `call:` descendant runs.
  - The agent has **no dedicated cancel endpoint or push channel**. It polls
    `GET /api/v1/runs/{id}` on a fixed interval
    (`agent.CancelPollInterval = 5 * time.Second`, `internal/agent/orchestrator.go:37`)
    and, when that poll observes `status: "Cancelled"`, cancels its local
    `runCtx` (any other terminal status is treated as `reapedByMaster`, a
    different code path). This poller runs independently of whatever the
    controller side is doing, so it is the mechanism this scenario is built
    to stress: does the *status write* survive an immediate controller
    kill, and does the *agent's own poller* — whose poll attempts were
    themselves failing during the outage — pick the change up once a
    controller is reachable again.
  - Mutex holder bookkeeping: `mutex_holders` (PK `mutex_name`, one row per
    currently-held job-level mutex, `run_id` FK `ON DELETE CASCADE`) and
    `named_lock_slots` (PK `(pool_name, slot_id)`, `run_id` FK
    `ON DELETE SET NULL`) — `internal/store/migrations/001_init.up.sql`. A
    `Cancelled` run must not leave a row behind in either table (I3).

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

Apply the job, trigger a run, and confirm it is actually `Running`/ticking
before relying on the baseline for either race:

```bash
curl -fsS -X POST localhost:18080/api/v1/jobs \
  -H "Authorization: Bearer ha-admin-token" -H "Content-Type: application/json" \
  --data-binary @../edgecase/workloads/longrun.payload.json

curl -fsS -X POST localhost:18080/api/v1/runs \
  -H "Authorization: Bearer ha-admin-token" -H "Content-Type: application/json" \
  -d '{"jobName":"edge-longrun"}'
# capture .id as RUN_ID

curl -fsS "localhost:18080/api/v1/runs/<RUN_ID>" -H "Authorization: Bearer ha-admin-token"
# expect status:"Running"
curl -N --max-time 8 "localhost:18080/api/v1/runs/<RUN_ID>/events" -H "Authorization: Bearer ha-admin-token"
# expect live "tick N" lines arriving during the 8s window
```

If baseline is broken (no leader, readyz not 200, or the run never reaches
`Running`/ticking), STOP and report BLOCKED with the evidence above rather
than proceeding to injection.

## Race A — cancel commits, then all controllers die within ~1s

Goal: prove the `Cancelled` write survives an immediate full-controller
kill, and that the agent (whose 5s poll was failing throughout the outage)
detects `Cancelled` and stops the step once a controller is reachable
again — not that the run silently resumes/completes despite a committed
cancellation.

Using the baseline run's `RUN_ID` (still `Running`, mid-`tick`), issue the
cancel and the triple kill-hard as **one compound command** so the kill
follows the 2xx as tightly as possible:

```bash
date
curl -fsS -o /tmp/cancel_resp -w '%{http_code}\n' -X POST "localhost:18080/api/v1/runs/<RUN_ID>/cancel" \
  -H "Authorization: Bearer ha-admin-token" \
  && ../edgecase/tools/inject.sh kill-hard controller1 \
  && ../edgecase/tools/inject.sh kill-hard controller2 \
  && ../edgecase/tools/inject.sh kill-hard controller3
cat /tmp/cancel_resp
# record: cancel HTTP status (expect 2xx), wall-clock time of the POST and
# of the third kill-hard (expect well under 1s apart)
```

Immediately re-check the run's status through a still-live path if
possible (it likely won't be — all controllers are now down); the
authoritative check happens after restart. Hold the outage ~30s, polling
`/readyz` to make the down window visible:

```bash
for i in $(seq 1 3); do
  date; curl -s -o /dev/null -w '%{http_code}\n' localhost:18080/readyz
  sleep 10
done
# expect 502/503 (or connection error) throughout
```

Restart all three and record the moment:

```bash
date   # T_restart
docker compose -f docker-compose.ha.yaml up -d controller1 controller2 controller3
```

Poll `/readyz` until 200, then immediately re-fetch the run and confirm it
committed `Cancelled` **before** the kill (i.e. it must already read
`Cancelled`, not something the recovered controller retroactively decided):

```bash
for i in $(seq 1 8); do
  date; curl -s -o /dev/null -w '%{http_code}\n' localhost:18080/readyz
  sleep 5
done

date
curl -fsS "localhost:18080/api/v1/runs/<RUN_ID>" -H "Authorization: Bearer ha-admin-token"
# expect status:"Cancelled" immediately on first successful read after
# readyz flips to 200 — record the timestamp/updatedAt on the row
```

Detection latency and process-kill evidence — capture agent logs and grep
for the cancellation detection and the point the step's shell process is
actually torn down:

```bash
mkdir -p /path/to/scratchpad/w1/w1-4
docker compose -f docker-compose.ha.yaml logs agent1 agent2 \
  > /path/to/scratchpad/w1/w1-4/race-a-agents.log
docker compose -f docker-compose.ha.yaml logs controller1 controller2 controller3 \
  > /path/to/scratchpad/w1/w1-4/race-a-controllers.log

grep -n "cancel\|Cancelled\|reapedByMaster" /path/to/scratchpad/w1/w1-4/race-a-agents.log
```

From the logs, compute: **detection latency** = timestamp of the agent's
first post-recovery successful poll that observes `Cancelled` (or its
resulting cancel-runCtx log line) minus `T_restart`. This is expected to be
bounded by `CancelPollInterval` (5s) plus controller-boot-to-ready latency
(observed ~30-35s in W1-1/W1-2/W1-3 for this same compose file) — i.e. the
poller was failing throughout the outage (`http 502/503` on every attempt)
and only succeeds once readyz is back, at which point it should pick up
`Cancelled` within one more poll interval.

Confirm the step's shell process was actually killed, not just abandoned
by the agent process (which would leave an orphaned `sleep`/`tick` loop
running against nothing): check that no further `tick N` lines appear in
the run's log after the point of cancellation, and — if the agent
container is still reachable — that no stray `sh -c ... tick` process
remains:

```bash
curl -fsS "localhost:18080/api/v1/runs/<RUN_ID>/logs?after=0" -H "Authorization: Bearer ha-admin-token" \
  | python3 -c "import json,sys; lines=[json.loads(l)['line'] for l in sys.stdin if l.strip()]; print('last tick line:', lines[-1] if lines else None); print('count:', len(lines))"
# the last "tick N" should correspond to wall-clock time at/before the kill,
# not continue climbing toward 300 after recovery

docker compose -f docker-compose.ha.yaml exec -T agent1 sh -c "ps aux | grep -i tick || true"
docker compose -f docker-compose.ha.yaml exec -T agent2 sh -c "ps aux | grep -i tick || true"
# expect no matching process on whichever agent claimed <RUN_ID>
```

### Race A, Part 2 — mutex release under the same race (I3)

`edge-longrun` carries no `spec.mutex`, so Part 1 alone cannot exercise I3.
Run a cheap, deliberately small variant with a mutex-bearing job so a
cancel-vs-failover race has a lock to actually release. Apply this inline
job (a short sleep, long enough to guarantee the mutex is held at cancel
time, short enough not to balloon scope):

```bash
cat > /tmp/w1-4-mutex.payload.json <<'EOF'
{"yaml":"apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: edge-mutex-cancel\nspec:\n  native: true\n  mutex: edge-mutex-cancel\n  agentSelector:\n    - kind:linux\n  steps:\n    - name: hold\n      run: sleep 60\n"}
EOF

curl -fsS -X POST localhost:18080/api/v1/jobs \
  -H "Authorization: Bearer ha-admin-token" -H "Content-Type: application/json" \
  --data-binary @/tmp/w1-4-mutex.payload.json

curl -fsS -X POST localhost:18080/api/v1/runs \
  -H "Authorization: Bearer ha-admin-token" -H "Content-Type: application/json" \
  -d '{"jobName":"edge-mutex-cancel"}'
# capture .id as MUTEX_RUN_ID
```

Confirm the mutex row exists once the run is `Running`:

```bash
curl -fsS "localhost:18080/api/v1/runs/<MUTEX_RUN_ID>" -H "Authorization: Bearer ha-admin-token"
# expect status:"Running"
docker compose -f docker-compose.ha.yaml exec -T postgres psql -U unified -c \
  "SELECT * FROM mutex_holders WHERE mutex_name='edge-mutex-cancel';"
# expect exactly one row, run_id = MUTEX_RUN_ID
```

Cancel it (no kill this time — this part isolates the mutex-release path
on an ordinary cancel; Part 1 already covers the controller-kill race for
run-status commit):

```bash
curl -fsS -o /tmp/cancel_resp2 -w '%{http_code}\n' -X POST "localhost:18080/api/v1/runs/<MUTEX_RUN_ID>/cancel" \
  -H "Authorization: Bearer ha-admin-token"
cat /tmp/cancel_resp2
# expect 2xx
```

Poll until the run reads `Cancelled`, then immediately re-check both lock
tables:

```bash
for i in $(seq 1 10); do
  date
  curl -fsS "localhost:18080/api/v1/runs/<MUTEX_RUN_ID>" -H "Authorization: Bearer ha-admin-token"
  echo
  sleep 2
done
# stop once status:"Cancelled"

docker compose -f docker-compose.ha.yaml exec -T postgres psql -U unified -c \
  "SELECT * FROM mutex_holders WHERE mutex_name='edge-mutex-cancel';"
docker compose -f docker-compose.ha.yaml exec -T postgres psql -U unified -c \
  "SELECT * FROM named_lock_slots WHERE run_id='<MUTEX_RUN_ID>';"
# expect zero rows in both once the run is Cancelled
```

If `mutex_holders` still shows a row for `edge-mutex-cancel` after the run
reads `Cancelled`, that is the I3 violation this part hunts for (a leaked
lock that would permanently starve any successor job sharing the same
`spec.mutex`, since nothing else releases it).

## Race B — cancel attempted during the outage itself

Goal: the opposite ordering from Race A — the cancel POST itself hits a
down LB, so nothing can have committed; confirm the LB fails loudly
(502/503, not a false 2xx) and that a retried cancel after recovery works
normally.

Trigger a fresh `edge-longrun` run and confirm it is ticking:

```bash
curl -fsS -X POST localhost:18080/api/v1/runs \
  -H "Authorization: Bearer ha-admin-token" -H "Content-Type: application/json" \
  -d '{"jobName":"edge-longrun"}'
# capture .id as RUN_ID2

curl -fsS "localhost:18080/api/v1/runs/<RUN_ID2>" -H "Authorization: Bearer ha-admin-token"
# expect status:"Running"
```

Kill all three controllers hard, back to back:

```bash
date
../edgecase/tools/inject.sh kill-hard controller1
../edgecase/tools/inject.sh kill-hard controller2
../edgecase/tools/inject.sh kill-hard controller3
```

Confirm the outage is live, then attempt the cancel through the LB while
down and record the **exact** HTTP status (or curl connection-error code):

```bash
curl -s -o /dev/null -w '%{http_code}\n' localhost:18080/readyz
# expect 502/503/000 (connection error)

curl -sS -o /tmp/cancel_down_resp -w 'HTTP_CODE:%{http_code}\n' -X POST "localhost:18080/api/v1/runs/<RUN_ID2>/cancel" \
  -H "Authorization: Bearer ha-admin-token"
cat /tmp/cancel_down_resp
# record exact code (expect 502/503, or a curl exit/connect error if nginx
# itself has no live upstream at all)
```

Restart all three controllers:

```bash
date   # T_restart2
docker compose -f docker-compose.ha.yaml up -d controller1 controller2 controller3
for i in $(seq 1 8); do
  date; curl -s -o /dev/null -w '%{http_code}\n' localhost:18080/readyz
  sleep 5
done
```

Retry the identical cancel once `/readyz` is 200:

```bash
date
curl -fsS -o /tmp/cancel_retry_resp -w '%{http_code}\n' -X POST "localhost:18080/api/v1/runs/<RUN_ID2>/cancel" \
  -H "Authorization: Bearer ha-admin-token"
cat /tmp/cancel_retry_resp
# expect 2xx this time
```

Poll until the run settles and confirm normal cancellation (agent
detection latency here should look like ordinary steady-state cancel —
no outage was in progress when the cancel actually landed):

```bash
for i in $(seq 1 10); do
  date
  curl -fsS "localhost:18080/api/v1/runs/<RUN_ID2>" -H "Authorization: Bearer ha-admin-token"
  echo
  sleep 3
done
# expect status:"Cancelled" within roughly one CancelPollInterval (5s) of
# the retried POST
```

## Recording (severity guidance)

- Run resumes executing to completion, or reaches `Succeeded`, despite a
  `Cancelled` status that was committed before the controller kill (Race
  A) = **major** (I1) — the committed status must be authoritative.
- Cancel POST returns 2xx (Race A or the Race B retry) but the run never
  actually transitions to `Cancelled` = **major** (I1).
- Agent step process (`tick`/`sleep`) survives materially longer than one
  `CancelPollInterval` + recovery time after the agent's own logs show it
  detected `Cancelled` = record precisely with timestamps (I6-adjacent —
  measure, don't presume a fixed severity without the numbers).
- `mutex_holders` or `named_lock_slots` still holds a row for the
  cancelled run's mutex/pool after the run reads `Cancelled` = **major**
  (I3) — a leaked lock permanently starves any successor sharing that
  `spec.mutex`.
- Race B cancel-while-down returns something other than a clear
  502/503/connection-error (e.g. a false 2xx that isn't backed by any
  live controller, or a 5xx that isn't retriable) = record precisely;
  severity depends on what actually happened.
- Clean run: cancel commits before the kill and survives it, mutex
  releases cleanly on an ordinary cancel, cancel-while-down fails loudly
  and a retry after recovery succeeds normally = **observation**.

## Teardown

```bash
docker compose -f docker-compose.ha.yaml down -v
```

Verify nothing is left running:

```bash
docker compose -f docker-compose.ha.yaml ps -a
```
