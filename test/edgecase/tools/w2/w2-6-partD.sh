#!/usr/bin/env bash
# W2-6 Part D: (i) recreate the schedule through the API so it is in its natural
# birth state, (ii) make exactly ONE UpdateScheduleLastFiredAt fail (BEFORE UPDATE
# trigger, armed for a single schedule check), (iii) observe the duplicate fire on
# the next check and then whether the schedule ever fires again.
set -u
# Run from test/ha (this script does not cd for you).
[ -f docker-compose.ha.yaml ] || { echo "run me from test/ha" >&2; exit 1; }
export MSYS_NO_PATHCONV=1
DC="docker compose ${COMPOSE_FILES:--f docker-compose.ha.yaml}"
TOK="Authorization: Bearer ha-admin-token"

psql()  { $DC exec -T postgres psql -U unified -tA -c "$1" 2>&1 | tr -d '\r'; }
psqlt() { $DC exec -T postgres psql -U unified -c "$1" 2>&1 | tr -d '\r'; }
st()    { psqlt "SELECT to_char(NOW(),'HH24:MI:SS.MS') AS t, to_char(last_fired_at,'HH24:MI:SS') AS lfa,
                 (SELECT count(*) FROM runs WHERE triggered_by='schedule:edge-every-minute') AS n_fires
          FROM schedules s WHERE name='edge-every-minute';"; }

echo "########## D0: recreate the schedule through the API (no DB mutation) ##########"
date -u +%FT%T.%3NZ
curl -s -o /dev/null -w 'delete=%{http_code}\n' -X DELETE localhost:18080/api/v1/schedules/edge-every-minute -H "$TOK"
psqlt "SELECT count(*) AS schedules_rows FROM schedules;"
N0=$(psql "SELECT count(*) FROM runs WHERE triggered_by='schedule:edge-every-minute';")
echo "runs so far: $N0"
curl -s -X POST localhost:18080/api/v1/schedules -H "$TOK" -H 'Content-Type: application/json' \
  --data-binary @../edgecase/workloads/schedule-every-minute.payload.json -w "\napply=%{http_code}\n"
date -u +%FT%T.%3NZ
psqlt "SELECT name, cron, job_name, last_fired_at, updated_at, NOW() FROM schedules;"

echo "########## D0b: wait for two natural fires to establish the birth lag ##########"
n=$N0
until [ "$(psql "SELECT count(*) FROM runs WHERE triggered_by='schedule:edge-every-minute';")" -ge $((N0+2)) ]; do sleep 3; done
st

echo "########## D1: arm the trigger just before the next predicted check ##########"
ip=$(psql "SELECT a.client_addr::text FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid WHERE l.locktype='advisory' AND l.objid=1702388580 LIMIT 1;")
ip=${ip%%/*}
case "$ip" in
  # IP->service map for THIS rig's compose network. Verify it before trusting
  # the result: `client_addr` is `inet` and renders as 172.20.0.3/32 (hence the
  # ${ip%%/*} strip), and a stale map falls through to "no leader" silently.
  # Rebuild with: docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' <cid>
  172.20.0.4) svc=controller1 ;; 172.20.0.3) svc=controller2 ;; 172.20.0.5) svc=controller3 ;;
  *) echo "no leader — abort"; exit 1 ;;
esac
t0=$($DC logs -t --no-log-prefix --tail 20000 "$svc" 2>/dev/null | tr -d '\r' | grep "scheduler became leader" | tail -1 | awk '{print $1}')
t0e=$(date -u -d "$t0" +%s.%N)
target=$(python -c "
t0=float('$t0e'); import time; now=time.time()
k=0
while t0+60*k < now+12: k+=1
print('%.3f' % (t0+60*k))")
echo "leader=$svc($ip) promoted=$t0 predicted_check_Tk=$(date -u -d @"$target" +%H:%M:%S.%3N)"
sleep "$(python -c "import time; print('%.3f' % max(0.0, float('$target')-6.0-time.time()))")"
echo "ARM at $(date -u +%H:%M:%S.%3N)"
$DC exec -T postgres psql -U unified <<'SQL' 2>&1 | tr -d '\r'
CREATE OR REPLACE FUNCTION edge_block_sched_update() RETURNS trigger
  LANGUAGE plpgsql AS $$ BEGIN
    RAISE EXCEPTION 'w2-6 injection: schedules UPDATE refused';
  END $$;
CREATE TRIGGER edge_block_sched_update BEFORE UPDATE ON schedules
  FOR EACH ROW WHEN (NEW.name = 'edge-every-minute')
  EXECUTE FUNCTION edge_block_sched_update();
SQL
echo "armed at $(date -u +%H:%M:%S.%3N)"

# wait past Tk
sleep "$(python -c "import time; print('%.3f' % max(0.0, float('$target')+8.0-time.time()))")"
echo "--- state 8s after Tk (run created, last_fired_at must be UNCHANGED) ---"
st
psqlt "SELECT id, status, to_char(created_at,'HH24:MI:SS.MS') AS created FROM runs WHERE triggered_by='schedule:edge-every-minute' ORDER BY created_at DESC LIMIT 3;"
echo "--- the Warn line ---"
$DC logs -t --no-log-prefix --since 60s controller1 controller2 controller3 2>/dev/null | tr -d '\r' | grep -E "last_fired_at|became leader"

echo "########## D2: disarm (one failed update only) ##########"
$DC exec -T postgres psql -U unified -c "DROP TRIGGER edge_block_sched_update ON schedules;" -c "DROP FUNCTION edge_block_sched_update();" 2>&1 | tr -d '\r'
echo "disarmed at $(date -u +%H:%M:%S.%3N)"
st

echo "########## D3: the next check — expect a DUPLICATE fire of the same occurrence ##########"
sleep "$(python -c "import time; print('%.3f' % max(0.0, float('$target')+68.0-time.time()))")"
st
psqlt "SELECT id, status, to_char(created_at,'HH24:MI:SS.MS') AS created FROM runs WHERE triggered_by='schedule:edge-every-minute' ORDER BY created_at DESC LIMIT 4;"

echo "########## D4: does it ever fire again? (4 more checks) ##########"
for k in 2 3 4 5; do
  sleep "$(python -c "import time; print('%.3f' % max(0.0, float('$target')+60.0*$k+8.0-time.time()))")"
  echo "--- after check T+$k ---"
  st
done
echo "--- controller log lines mentioning the schedule during D4 ---"
$DC logs -t --no-log-prefix --since 300s controller1 controller2 controller3 2>/dev/null | tr -d '\r' \
  | grep -icE "schedule|last_fired"
$DC logs -t --no-log-prefix --since 300s controller1 controller2 controller3 2>/dev/null | tr -d '\r' \
  | grep -iE "schedule|last_fired" | tail -20
echo "########## Part D complete at $(date -u +%FT%T.%3NZ) ##########"
