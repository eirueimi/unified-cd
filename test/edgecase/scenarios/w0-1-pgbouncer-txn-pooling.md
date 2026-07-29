# W0-1 — PgBouncer in transaction-pooling mode

- **Invariants:** I1 (run accounting), I5 (bounded recovery — here: is the
  failure mode diagnosable?)
- **Stack:** test/ha compose + `compose/pgbouncer.override.yaml`
- **Docs contract:** `docs/high-availability.md` requires session pooling;
  transaction pooling "breaks advisory locks and NOTIFY". This scenario
  documents the actual failure mode an operator would see.

## Baseline (healthy stack, no overlay)

```bash
cd test/ha
docker compose -f docker-compose.ha.yaml up -d --build
curl -fsS localhost:18080/readyz            # expect: ok (retry until up)
curl -fsS -X POST localhost:18080/api/v1/jobs \
  -H "Authorization: Bearer ha-admin-token" -H "Content-Type: application/json" \
  --data-binary @../edgecase/workloads/tick.payload.json
curl -fsS -X POST localhost:18080/api/v1/schedules \
  -H "Authorization: Bearer ha-admin-token" -H "Content-Type: application/json" \
  --data-binary @../edgecase/workloads/schedule-every-minute.payload.json
```

Wait ~90s, then record the baseline:

```bash
# exactly one leader elected:
for c in controller1 controller2 controller3; do
  echo "== $c"; docker compose -f docker-compose.ha.yaml logs $c 2>/dev/null | grep -c "scheduler became leader"
done
# schedule fired (>=1 run):
curl -fsS "localhost:18080/api/v1/runs?jobName=edge-tick" -H "Authorization: Bearer ha-admin-token"
# SSE delivers log lines (replace <RUN_ID> from the previous output; expect
# "tick N" events streaming; Ctrl-C after a few):
curl -N "localhost:18080/api/v1/runs/<RUN_ID>/events" -H "Authorization: Bearer ha-admin-token"
```

Tear down INCLUDING volumes (schedule state must not leak into the probe):

```bash
docker compose -f docker-compose.ha.yaml down -v
```

## Probe (with PgBouncer overlay)

```bash
docker compose -f docker-compose.ha.yaml -f ../edgecase/compose/pgbouncer.override.yaml up -d --build
curl -fsS localhost:18080/readyz
```

Repeat the same job+schedule apply as the baseline. Then observe for ~5
minutes:

1. **Leader election:** same `grep -c "scheduler became leader"` loop as the
   baseline, plus `grep -i "advisory\|lock"` over all controller logs.
   Questions to answer: do multiple controllers claim leadership? Does
   leadership flap? Any unlock warnings ("you don't own a lock")?
2. **Scheduling:** does `edge-every-minute` fire at all / once per minute /
   multiple times per minute?
   `curl -fsS "localhost:18080/api/v1/runs?jobName=edge-tick" -H "Authorization: Bearer ha-admin-token"`
   Count runs after 5 minutes; expected-healthy would be ~5.
3. **SSE:** attach to a running run's `/events` — do log lines arrive
   (NOTIFY works) or does the stream stall?
4. **Advisory locks in PG:** which backend sessions hold them, and do they
   leak after a controller restart?
   ```bash
   docker compose -f docker-compose.ha.yaml exec postgres \
     psql -U unified -c "SELECT locktype, classid, objid, pid, granted FROM pg_locks WHERE locktype='advisory';"
   docker compose -f docker-compose.ha.yaml restart controller1
   # wait 60s, re-run the pg_locks query: are old locks still granted to dead sessions?
   ```
5. **Kill the apparent leader** (`docker compose ... kill controller<N>` for
   whichever logged leadership last): does another replica EVER become
   leader, or is the lock stranded on a PgBouncer server connection forever
   (scheduling halted = the split-brain-adjacent outcome the docs warn
   about)?

## Recording

FINDINGS entries (severity guidance): silent scheduling halt or duplicate
fires = **major**; noisy-but-clear errors pointing at pooling mode =
**minor** (docs-confirming). Also record whether any phantom/duplicate runs
appeared (I1) and the exact log lines an operator could alert on.

## Teardown

```bash
docker compose -f docker-compose.ha.yaml -f ../edgecase/compose/pgbouncer.override.yaml down -v
```
