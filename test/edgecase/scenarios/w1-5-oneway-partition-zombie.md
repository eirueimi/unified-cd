# W1-5 — one-way agent->controller partition: zombie executor

- **Invariants:** I2 (at-most-once side effects), I3 (no lock leaks),
  I6 (zombie containment — *measure and document*, explicitly not pass/fail
  per the campaign spec)
- **Stack:** `test/ha` compose **plus** the W1-5 overlay. Every compose and
  `inject.sh` call in this runbook uses:

  ```bash
  cd test/ha
  export COMPOSE_FILES="-f docker-compose.ha.yaml -f ../edgecase/compose/oneway.override.yaml"
  docker compose $COMPOSE_FILES up -d --build
  ```

  The overlay (`test/edgecase/compose/oneway.override.yaml`) does two things:
  swaps nginx's config for `nginx-edge.conf` (identical to `test/ha/nginx.conf`
  plus `include /etc/nginx/blocklist/*.conf;` inside the `location /` block,
  backed by a named volume so `inject.sh` can write into it at runtime), and
  bind-mounts `test/edgecase/sideeffect-data` into **both** agents at `/data`
  so the side-effect log is observable from the host.
- **Workload:**
  - `test/edgecase/workloads/sideeffect.payload.json` — job `edge-sideeffect`,
    `spec.concurrency.mutex: edge-mutex`, one native step `append`:
    `for i in $(seq 1 120); do echo "run,$i,$(date -u +%H:%M:%S)" >> /data/sideeffect.log; sleep 1; done`
    — 120 append-only lines at ~1/s, each carrying its own UTC wall-clock
    timestamp. The step runs ~120s and, critically, **has no dependency on
    controller reachability**: it keeps appending whether or not the agent can
    talk to anything.
  - `test/edgecase/workloads/mutex-successor.payload.json` — job
    `edge-mutex-successor`, same `spec.concurrency.mutex: edge-mutex`, one
    native step `probe`: `echo acquired-mutex-ok`. This is the I3 probe: it can
    only run if the partitioned run's mutex was actually released.
  - **Known workload caveat:** `UNIFIED_RUN_ID` is **not** injected into native
    steps, so the first CSV field in `sideeffect.log` is the literal string
    `run`, not a run id. Correlating log lines to a run therefore relies on the
    per-line `HH:MM:SS` timestamp plus the invariant that only one
    `edge-sideeffect` run exists in the whole scenario (verified explicitly in
    the I2 accounting step below), **not** on the id field.
- **Verified API/mechanism (do not re-derive):**
  - **Claiming agent** is exposed as `claimedBy` on `GET /api/v1/runs/{id}`
    (`internal/api/types.go:59`: `ClaimedBy string \`json:"claimedBy,omitempty"\``
    — "Claiming agent's ID; empty until claimed"). In this compose the agent IDs
    are exactly the compose service names, `agent1` / `agent2`
    (`test/ha/docker-compose.ha.yaml`, `--id agent1` / `--id agent2`), so the
    value of `claimedBy` can be passed straight to `inject.sh nginx-block`.
  - **Partition mechanism.** The agent image has **no `iptables` and no
    `NET_ADMIN`**, so a real packet-level partition cannot be injected from
    inside the container. Instead `inject.sh nginx-block <svc>` resolves that
    container's IP and writes `deny <ip>;` into the nginx blocklist include,
    then `nginx -s reload`. Because unified-cd's agent protocol is **strictly
    agent-polls-controller** (claim long-poll, heartbeat, log push, step/finish
    reports, cancel poll — all agent-initiated; the controller never dials the
    agent), denying the agent at the single ingress is equivalent to a full
    one-way agent->controller partition.
    **Caveat to state in the findings:** nginx answers a denied request with a
    fast `403`, so the agent observes *prompt permanent-looking errors* rather
    than the TCP black-hole (connect timeouts, half-open sockets, retransmits)
    a real network partition would produce. Anything in the agent that behaves
    differently on "fast 4xx" vs "hung socket" is therefore **not** exercised by
    this scenario.
  - **Stuck-run reaper** (`internal/controller/stuckrun_reaper.go`, wired in
    `cmd/controller/main.go:404` as
    `RunStuckRunReaper(ctx, st, 30*time.Second, 90*time.Second, 60*time.Second)`):
    leader-elected via advisory lock `stuckRunReaperLockKey` (`0x7374756B`),
    ticks every **30s**, and calls
    `ListStuckRunIDs(staleAfter=90s, grace=60s)` — `internal/store/postgres.go:1216`:

    ```sql
    SELECT r.id FROM runs r LEFT JOIN agents a ON r.claimed_by = a.id
    WHERE r.status = 'Running'
      AND r.claimed_at IS NOT NULL
      AND r.claimed_at < NOW() - grace          -- 60s
      AND (a.id IS NULL OR a.last_seen_at < NOW() - staleAfter)   -- 90s
    ```

    Each hit goes through `failOrphanedRun` -> `store.MarkRunFinished(id, Failed)`
    (+ cascade-cancel of `call:` descendants). **Failed, never re-queued** — the
    stated reason (`stuckrun_reaper.go:72-73`) is precisely that re-running a
    partially-executed run could duplicate side effects, i.e. I2 is a design
    intent here, not just a campaign invariant.
  - **Expected reap window.** Heartbeat interval is 15s
    (`docs/high-availability.md` §Agent heartbeat), so at block time T0 the
    agent's `last_seen_at` is somewhere in `[T0-15s, T0]`. The run becomes
    eligible at `max(claimed_at + 60s, last_seen_at + 90s)`, and is acted on at
    the next 30s reaper tick — i.e. **reap lands roughly T0+90s .. T0+120s** for
    a block issued shortly after the claim. The step itself ends ~120s after it
    starts. **Block as early as possible after the claim** — every second of
    delay eats directly into the observable zombie window, and if the reap lands
    after the step has already finished on its own the zombie window is zero and
    the scenario measures nothing.
  - **Mutex release** rides on `MarkRunFinished`, which runs
    `DELETE FROM mutex_holders WHERE run_id = $1` and
    `UPDATE named_lock_slots SET run_id = NULL, acquired_at = NULL WHERE run_id = $1`
    (`internal/store/postgres.go:769`, `:774`) in the same call — hence the
    reaper's comment that it must fail runs one-by-one rather than with a bulk
    `UPDATE`.
  - **What the healed agent's buffered writes hit** (all in
    `internal/controller/api_agent.go`, policy centralised in
    `agentRunGuard`/`respondRunWriteVerdict`, `internal/controller/agent_guard.go`):
    - `POST .../runs/{id}/finish` — guard runs with `rejectTerminal` semantics
      via the store CAS; an already-terminal run matches no rows and the handler
      answers **`200` with `{"alreadyFinalized":true}`**, *deliberately not 409*,
      because the agent's HTTP client treats `>=400` as an error
      (`api_agent.go:575-586`).
    - `POST .../steps` (step report) — explicit `GetRun` pre-check; if the run
      is `Succeeded`/`Failed`/`Cancelled` the handler returns **`200` with
      `alreadyFinalized:true`** and does **not** upsert the step
      (`api_agent.go:505-525`).
    - `POST .../logs` / `logs/bulk` — the guard is called with
      `rejectTerminal=false`, so **log lines for a terminal run are still
      accepted and appended** (`204`); only a *sealed* (archived) run drops
      them, and that drop is a `204` too, with a controller-side
      `"dropping log line for sealed run"` warning (`api_agent.go:539-570`).
      Expect the zombie's buffered log tail to land on the Failed run.
    - `403` is what a write from the **wrong** agent gets
      (`runWriteNotOwned`), which is *not* what a healed zombie produces (it
      still owns `claimed_by`) — so do not expect 403s here; record whatever
      actually appears.
  - **Heartbeat reconcile** (`api_agent.go:99-124`): a live agent posts
    `{"activeRunIds":[...]}` with every heartbeat, and the controller fails any
    of that agent's `Running` runs absent from the reported set (past a 60s
    grace). It only selects `Running` runs, so it is a no-op against a run the
    reaper already Failed — noted so it is not mistaken for a second reap.

## Baseline (overlay stack, before any injection)

```bash
cd test/ha
export COMPOSE_FILES="-f docker-compose.ha.yaml -f ../edgecase/compose/oneway.override.yaml"

# start from a clean side-effect log — I2 accounting counts raw lines
: > ../edgecase/sideeffect-data/sideeffect.log

docker compose $COMPOSE_FILES up -d --build
curl -fsS localhost:18080/readyz            # expect: ok (retry until up)
```

BASELINE GATE — confirm all three before injecting anything:

```bash
# exactly one leader elected:
for c in controller1 controller2 controller3; do
  echo "== $c"
  docker compose $COMPOSE_FILES logs $c 2>/dev/null | grep -c "scheduler became leader"
done
curl -s -o /dev/null -w '%{http_code}\n' localhost:18080/readyz   # expect 200
# both agents registered and heartbeating:
curl -fsS localhost:18080/api/v1/agents -H "Authorization: Bearer ha-admin-token"
# expect agent1 and agent2, both with a recent lastSeenAt
```

If baseline is broken (no leader, `readyz` not 200, or only one agent
registered — this scenario needs **two** so the successor has an unblocked
agent to land on), STOP and report BLOCKED with the evidence above rather than
proceeding to injection.

Apply both jobs:

```bash
curl -fsS -X POST localhost:18080/api/v1/jobs \
  -H "Authorization: Bearer ha-admin-token" -H "Content-Type: application/json" \
  --data-binary @../edgecase/workloads/sideeffect.payload.json
curl -fsS -X POST localhost:18080/api/v1/jobs \
  -H "Authorization: Bearer ha-admin-token" -H "Content-Type: application/json" \
  --data-binary @../edgecase/workloads/mutex-successor.payload.json
# both expect 200. If either 400s, read the error body and check
# internal/dsl/types.go — `mutex` lives under spec.concurrency, and
# internal/dsl/parse.go:93 sets dec.KnownFields(true) so any stray field is a
# hard 400 rather than an ignored key.
```

## Injection — block the claiming agent at nginx

Trigger the run, then find the claiming agent and block it **immediately**
(see "Expected reap window" above for why the delay matters):

```bash
date -u +%H:%M:%S
curl -fsS -X POST localhost:18080/api/v1/runs \
  -H "Authorization: Bearer ha-admin-token" -H "Content-Type: application/json" \
  -d '{"jobName":"edge-sideeffect"}'
# capture .id as RUN_ID

# poll until claimedBy is populated (usually within ~1-2s)
curl -fsS "localhost:18080/api/v1/runs/$RUN_ID" -H "Authorization: Bearer ha-admin-token"
# record: status ("Running"), claimedBy ("agent1" or "agent2"), claimedAt

date -u +%H:%M:%S     # T0
../edgecase/tools/inject.sh nginx-block <claimedBy>       # e.g. agent2
# prints: blocked <svc> (<ip>) at nginx
```

Confirm the block is real from the agent's own side before trusting the
timeline (the deny is IP-based, so a mis-resolved IP would silently no-op):

```bash
docker compose $COMPOSE_FILES logs --since 30s <claimedBy-service> | tail -20
# expect heartbeat / claim / log-push failures citing http 403
```

## Observation timeline (poll ~every 15s, ~4 minutes total)

Run a single polling loop and keep its raw output — every number in the
findings must come from it:

```bash
SCRATCH=/path/to/scratchpad/w1-5
mkdir -p "$SCRATCH"
for i in $(seq 1 16); do
  echo "=== $(date -u +%H:%M:%S)"
  curl -s "localhost:18080/api/v1/runs/$RUN_ID" -H "Authorization: Bearer ha-admin-token" \
    | tr ',' '\n' | grep -E '"status"|"updatedAt"|"claimedBy"'
  echo "sideeffect lines: $(wc -l < ../edgecase/sideeffect-data/sideeffect.log)"
  sleep 15
done 2>&1 | tee "$SCRATCH/timeline.txt"
```

Record, on that timeline:

1. **T0** — the block.
2. **Heartbeat failure onset** — first failing heartbeat in the blocked agent's
   log (`docker compose $COMPOSE_FILES logs <agent> | grep -n "heartbeat"`).
3. **Reap** — the controller log line
   `"stuck-run reaper: failed orphaned run (agent lost)"` with the matching
   `runId`, and the run's own flip to `status:"Failed"`. Capture both:

   ```bash
   docker compose $COMPOSE_FILES logs controller1 controller2 controller3 \
     > "$SCRATCH/controllers.log"
   grep -n "stuck-run reaper" "$SCRATCH/controllers.log"
   ```

   Compute observed reap latency = reap timestamp − T0, and compare against the
   90s+30s bound (I5-adjacent sanity check; the primary invariants here are
   I2/I3/I6).
4. **Zombie window (I6)** — `wc -l` of `sideeffect-data/sideeffect.log` keeps
   growing *after* the run reads `Failed`. The exact end of execution is
   readable from the file itself, since each line carries its own timestamp:

   ```bash
   tail -3 ../edgecase/sideeffect-data/sideeffect.log
   cp ../edgecase/sideeffect-data/sideeffect.log "$SCRATCH/sideeffect.log"
   ```

   **Zombie duration = (timestamp of the last appended line) − (reap
   timestamp)**, and the number of side-effect lines produced after the run was
   already `Failed` = count of lines whose timestamp is > reap timestamp.
   Expected shape: it keeps going until the 120-iteration loop simply ends,
   because nothing in the architecture can reach the agent to stop it — there
   is no fencing token, no lease check inside the step, and the agent's only
   inbound signal (the cancel poller) is blocked by the same partition.

## I3 — successor must acquire the mutex immediately after the reap

Trigger the successor **as soon as** the reap is observed (this is the real
I3 test: the reaper's `MarkRunFinished` is supposed to have deleted the
`edge-mutex` row even though the holder is still executing):

```bash
date -u +%H:%M:%S
curl -fsS -X POST localhost:18080/api/v1/runs \
  -H "Authorization: Bearer ha-admin-token" -H "Content-Type: application/json" \
  -d '{"jobName":"edge-mutex-successor"}'
# capture .id as SUCC_ID

for i in $(seq 1 20); do
  date -u +%H:%M:%S
  curl -s "localhost:18080/api/v1/runs/$SUCC_ID" -H "Authorization: Bearer ha-admin-token" \
    | tr ',' '\n' | grep -E '"status"|"claimedBy"'
  sleep 3
done 2>&1 | tee "$SCRATCH/successor.txt"
```

Required outcome: it must Queue, claim on the **other** (unblocked) agent,
acquire `edge-mutex`, and reach `Succeeded`. If it sits `Queued` for >60s while
a free, healthy agent exists, the mutex leaked — **major (I3)**.

Direct cross-check against the lock tables (redirect to a file; interactive-only
psql output has been called out as non-reverifiable in earlier findings):

```bash
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c \
  "SELECT * FROM mutex_holders;" > "$SCRATCH/mutex_holders.txt" 2>&1
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c \
  "SELECT * FROM named_lock_slots;" >> "$SCRATCH/mutex_holders.txt" 2>&1
cat "$SCRATCH/mutex_holders.txt"
```

Expect: no `edge-mutex` row attributable to the reaped run once it is `Failed`
(the successor's own row may legitimately be present while *it* is running —
re-check after it finishes for a clean empty-table read).

## Heal — what the zombie does when it can talk again

```bash
date -u +%H:%M:%S     # T_heal
../edgecase/tools/inject.sh nginx-unblock <any-service-arg>   # unblocks all
```

**Caveat discovered on execution — check this before looking for replays.**
The nginx deny answers with `403`, and `retryUntilSuccess`
(`internal/agent/retry.go:33-36`) classifies **every** `HTTPError` with
`StatusCode < 500` as *permanent* and returns without retrying. So the agent's
step report and `FinishRun` for the partitioned run are abandoned outright at
the moment the step ends — **before** the heal — and are never replayed
afterwards. Grep for this first; if it is present, "what the buffered writes
get answered with at heal" is unanswerable in this variant because no buffered
write is ever attempted again:

```bash
grep -n "permanent error, giving up retry" "$SCRATCH/agent1.log"
# expect two lines (step report + finish), timestamped at step-end, status 403
```

Then capture the healed agent's full log and classify every buffered write it
replays:

```bash
docker compose $COMPOSE_FILES logs <claimedBy-service> > "$SCRATCH/agent-blocked.log"
grep -nE "403|alreadyFinalized|dropped|reaped|finish|heartbeat" "$SCRATCH/agent-blocked.log" | tail -40
```

Record specifically:

- the exact status codes the buffered step report / finish call receive
  (expected `200 alreadyFinalized` per the mechanism notes above, **not** 403 —
  the zombie still legitimately owns `claimed_by`);
- whether the buffered log tail lands on the now-`Failed` run (`204`, lines
  visible via `GET /runs/$RUN_ID/logs?after=0`) or is dropped, and whether a
  `[N log line(s) dropped: controller unreachable]` marker appears;
- whether the agent re-registers cleanly and claims new work afterwards — prove
  it by triggering one more `edge-mutex-successor` run after the heal and
  checking it can land on the previously-blocked agent.

## I2 accounting — no re-execution, no duplicate side effects

```bash
curl -fsS "localhost:18080/api/v1/runs?jobName=edge-sideeffect" \
  -H "Authorization: Bearer ha-admin-token" > "$SCRATCH/runs-sideeffect.json"
python -c "import json,sys; d=json.load(open(r'$SCRATCH/runs-sideeffect.json')); \
  rs=d if isinstance(d,list) else d.get('runs',d.get('items',[])); \
  print('run count:', len(rs)); [print(r['id'], r['status'], r.get('claimedBy')) for r in rs]"

wc -l < ../edgecase/sideeffect-data/sideeffect.log
awk -F, '{print $2}' ../edgecase/sideeffect-data/sideeffect.log | sort -n | uniq -d
# expect: exactly 1 run (Failed); exactly 120 lines; NO duplicated iteration
# numbers (the uniq -d output must be empty)
```

The run was Failed, never re-queued, so `edge-sideeffect` must show **exactly
one** run and the side-effect log must contain each iteration number exactly
once. Any duplicated iteration number, or a second `edge-sideeffect` run, is
re-execution = **major (I2)**.

## Addendum (added during execution) — 4xx-permanent abandonment with no reap

The main probe's partition outlasts the reaper, so the reaper's `Failed` masks
what the agent's own abandoned reports would have done. This cheap addendum
isolates that: partition a run whose step finishes **well inside** the reaper's
eligibility window, then heal **before** the run could ever be reaped, and see
what terminal status the control plane ends up with for a step that actually
succeeded.

```bash
cat > /tmp/w1-5-short.payload.json <<'EOF'
{"yaml":"apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: edge-short\nspec:\n  native: true\n  agentSelector:\n    - kind:linux\n  steps:\n    - name: quick\n      run: sleep 25; echo done-short\n"}
EOF
curl -fsS -X POST localhost:18080/api/v1/jobs \
  -H "Authorization: Bearer ha-admin-token" -H "Content-Type: application/json" \
  --data-binary @/tmp/w1-5-short.payload.json

# trigger, poll for claimedBy, block that agent immediately (same shape as above)
# capture .id as SHORT_ID
```

Timing that makes this work, and why each bound matters:

- the step ends at `claimed_at + ~28s` (`sleep 25` + startup), so the step's
  report/finish attempts happen **while blocked**;
- heal at `claimed_at + ~54s`, i.e. **before** `claimed_at + 60s`. Note this
  does **not** mean `ListStuckRunIDs`'s grace clause (`claimed_at < NOW() -
  grace`, `internal/store/postgres.go:1216-1224`) stays unmet forever — it
  expires normally at `claimed_at + 60s`, same as any other run, and by the
  time this run settles that boundary has long since passed. What actually
  keeps the run un-reaped is the **other** conjunct: once healed, the agent's
  next successful heartbeat refreshes `last_seen_at` to a fresh value, so
  `a.last_seen_at < NOW() - staleAfter` (90s, `internal/store/postgres.go:1224`)
  never matches again — the run simply stops looking stuck to the reaper,
  independent of the grace clause. (Verify: no `stuck-run reaper` line
  mentioning `SHORT_ID` in `controllers.log`.) **Do not heal later than this**
  (e.g. at `claimed_at + ~58s`) expecting the grace-clause margin to be what's
  protecting the run — the margin that actually matters is against the 90s
  staleness window on the agent's *next* heartbeat after heal, not against the
  60s grace clause, and healing too close to a heartbeat boundary risks a
  stale `last_seen_at` still being read by a reaper tick that lands in between;
- keep polling for ~2 more minutes: the run is now `Running` in the DB with
  nothing left executing, so the only thing that can still settle it is the
  **heartbeat reconcile** (`api_agent.go:99-124`), which fires on the healed
  agent's first heartbeat past `heartbeatReconcileGrace` (60s from
  `claimed_at`).

Record: the run's final status, `runs.updated_at`, `step_reports` for the run,
and `GET /runs/$SHORT_ID/logs/stats`. A step that ran to completion and whose
run is nonetheless recorded terminal-**Failed**, with its stdout absent, is the
outcome this addendum exists to expose.

## Recording (severity guidance)

- `mutex_holders` still holds `edge-mutex` for the reaped run after it reads
  `Failed`, or the successor sits `Queued` >60s with a free agent available =
  **major (I3)** — a leaked lock permanently starves every job sharing that
  `spec.concurrency.mutex`.
- Any duplicate side effect (repeated iteration number in `sideeffect.log`) or
  any second `edge-sideeffect` run = **major (I2)** — the reaper's own contract
  is fail-never-requeue precisely to prevent this.
- Zombie duration, post-reap side-effect line count, and post-heal write
  behavior = **measured observation (I6)**. I6 is explicitly
  *document-don't-judge* in the campaign spec ("not pass/fail: the architecture
  has no hard fencing; the operator judges acceptability"), so report the
  numbers and the mechanism, and do **not** rate a bounded, self-terminating
  zombie as a violation.
- A healed zombie whose buffered writes are *silently* accepted onto a terminal
  run in a way that changes visible state (e.g. a step report flipping a Failed
  run's step to Succeeded) = record precisely; severity depends on what state
  actually changed.
- Addendum: a run whose step **succeeded** ending up permanently recorded
  `Failed` (or any other status inversion) because its report was abandoned =
  **major** — the terminal status is wrong, never self-corrects, and would push
  an operator to re-run work whose side effects already landed. State plainly
  in the finding that the `403` came from the injection's nginx deny, not from
  the controller, and that a real packet-level partition would surface
  *retryable* transport errors instead — the defect is that the agent cannot
  distinguish an intermediary's transient 4xx from the controller's own
  (legitimately permanent) ownership 4xx.
- A terminal run whose `step_reports` row is left `Running` forever = **minor**
  violation of I7: a real and permanent contradiction, but confined to a
  display/audit row under an already-correct terminal run status — no lock
  leak, no scheduling effect, no data loss.
- Clean run: reap within the documented 90s+30s bound, mutex released, successor
  succeeds on the other agent, exactly one run, exactly 120 non-duplicated
  side-effect lines, zombie bounded by the step's own length = **observation**.

## Teardown

```bash
docker compose $COMPOSE_FILES down -v
docker compose $COMPOSE_FILES ps -a          # verify nothing left running
: > ../edgecase/sideeffect-data/sideeffect.log   # leave the shared volume clean
```
