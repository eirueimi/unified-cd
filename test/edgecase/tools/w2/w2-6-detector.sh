#!/usr/bin/env bash
# W2-6 divergence detector: one row every ~2s. Usage (from test/ha):
#   sh ../edgecase/tools/w2/w2-6-detector.sh > "$SCRATCH/detector.txt" &
#   echo $! >> "$SCRATCH/samplers.pid"     # MUST be killed explicitly before
#                                          # teardown: on the 2026-07-30 run it
#                                          # outlived `down -v` and appended 847
#                                          # error rows to its own capture.
# The advisory key below is the SCHEDULER key in decimal. Cross-check it with
#   SELECT objid FROM pg_locks WHERE locktype='advisory';
# a wrong conversion reads 0 holders, which looks exactly like a dead scheduler.
# cols: t | last_fired_at | newest_run_created_at | d=newest_run-lfa | n_fires | leader_ip
# Run from test/ha (this script does not cd for you).
[ -f docker-compose.ha.yaml ] || { echo "run me from test/ha" >&2; exit 1; }
export MSYS_NO_PATHCONV=1
SQL="SELECT to_char(NOW(),'HH24:MI:SS.MS') AS t,
       coalesce(to_char(s.last_fired_at,'HH24:MI:SS.MS'),'NULL') AS lfa,
       coalesce(to_char(r.created_at,'HH24:MI:SS.MS'),'NULL') AS newest_run,
       coalesce(round(EXTRACT(EPOCH FROM (r.created_at - s.last_fired_at))::numeric,3)::text,'NULL') AS d,
       (SELECT count(*) FROM runs WHERE triggered_by='schedule:edge-every-minute') AS n,
       coalesce((SELECT a.client_addr::text FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid
                 WHERE l.locktype='advisory' AND l.objid=1702388580 LIMIT 1),'NONE') AS leader
FROM schedules s
LEFT JOIN (SELECT created_at FROM runs WHERE triggered_by='schedule:edge-every-minute'
           ORDER BY created_at DESC LIMIT 1) r ON true
WHERE s.name='edge-every-minute';"
for i in $(seq 1 3000); do
  docker compose ${COMPOSE_FILES:--f docker-compose.ha.yaml} exec -T postgres psql -U unified -tA -F'|' -c "$SQL" 2>&1 | tr -d '\r'
  sleep 1.5
done
