#!/usr/bin/env bash
# W2-6 Part C: widen the scheduler.go:189->:194 window with a held row lock on
# the schedules row, then SIGKILL the blocked leader so its successor re-fires the
# SAME occurrence. Deterministic version of Arm A.
set -u
# Run from test/ha (this script does not cd for you).
[ -f docker-compose.ha.yaml ] || { echo "run me from test/ha" >&2; exit 1; }
export MSYS_NO_PATHCONV=1
DC="docker compose ${COMPOSE_FILES:--f docker-compose.ha.yaml}"
SCRATCH="${SCRATCH:?run from test/ha with SCRATCH exported, e.g. export SCRATCH=<scratchpad>/w2-6}"
S="$SCRATCH"

psql() { $DC exec -T postgres psql -U unified -tA -c "$1" 2>&1 | tr -d '\r'; }
psqlt() { $DC exec -T postgres psql -U unified -c "$1" 2>&1 | tr -d '\r'; }

ip=$(psql "SELECT a.client_addr::text FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid WHERE l.locktype='advisory' AND l.objid=1702388580 LIMIT 1;")
ip=${ip%%/*}
case "$ip" in
  # IP->service map for THIS rig's compose network. Verify it before trusting
  # the result: `client_addr` is `inet` and renders as 172.20.0.3/32 (hence the
  # ${ip%%/*} strip), and a stale map falls through to "no leader" silently.
  # Rebuild with: docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' <cid>
  172.20.0.4) svc=controller1 ;;
  172.20.0.3) svc=controller2 ;;
  172.20.0.5) svc=controller3 ;;
  *) echo "no leader (ip='$ip') — abort"; exit 1 ;;
esac
t0=$($DC logs -t --no-log-prefix --tail 20000 "$svc" 2>/dev/null | tr -d '\r' | grep "scheduler became leader" | tail -1 | awk '{print $1}')
t0e=$(date -u -d "$t0" +%s.%N)
target=$(python -c "
t0=float('$t0e'); import time; now=time.time()
k=0
while t0+60*k < now+14: k+=1
print('%.3f' % (t0+60*k))")
echo "leader=$svc($ip) promoted=$t0 predicted_check=$(date -u -d @"$target" +%H:%M:%S.%3N)"

# --- take the row lock ~10s before the predicted check, hold it 70s
sleepfor=$(python -c "import time; print('%.3f' % max(0.0, float('$target')-10.0-time.time()))")
echo "sleeping ${sleepfor}s before taking the row lock"
sleep "$sleepfor"
echo "LOCK issued at $(date -u +%H:%M:%S.%3N)"
$DC exec -T postgres psql -U unified > "$S/partC-lock.txt" 2>&1 <<'SQL' &
\timing on
BEGIN;
SELECT NOW() AS lock_taken, name, last_fired_at FROM schedules WHERE name='edge-every-minute' FOR UPDATE;
SELECT pg_sleep(70);
ROLLBACK;
SELECT NOW() AS lock_released;
SQL
LOCKPID=$!
sleep 3
psqlt "SELECT to_char(NOW(),'HH24:MI:SS.MS') AS t, name, last_fired_at FROM schedules WHERE name='edge-every-minute';"

# --- wait for the leader's UPDATE to block on that row
echo "--- waiting for the blocked UPDATE (poll 0.5s) ---"
for i in $(seq 1 60); do
  out=$(psql "SELECT to_char(NOW(),'HH24:MI:SS.MS')||' pid='||pid||' ip='||coalesce(client_addr::text,'?')||' waiting='||coalesce(wait_event_type,'-')||'/'||coalesce(wait_event,'-')||' state='||state FROM pg_stat_activity WHERE query ILIKE '%UPDATE schedules SET last_fired_at%' AND pid <> pg_backend_pid();")
  if [ -n "$out" ]; then echo "BLOCKED: $out"; break; fi
  sleep 0.5
done
echo "--- state with the window held open ---"
psqlt "SELECT to_char(NOW(),'HH24:MI:SS.MS') AS t, to_char(last_fired_at,'HH24:MI:SS') AS lfa,
       (SELECT count(*) FROM runs WHERE triggered_by='schedule:edge-every-minute') AS n_fires FROM schedules WHERE name='edge-every-minute';"
psqlt "SELECT id, status, to_char(created_at,'HH24:MI:SS.MS') AS created FROM runs WHERE triggered_by='schedule:edge-every-minute' ORDER BY created_at DESC LIMIT 3;"

# --- scheduler-stall probe: a manually triggered run must stay Pending
echo "--- stall probe: manual trigger while the scheduler is blocked ---"
date -u +%FT%T.%3NZ
curl -fsS -X POST localhost:18080/api/v1/runs -H "Authorization: Bearer ha-admin-token" \
  -H 'Content-Type: application/json' -d '{"jobName":"edge-tick"}' | tr -d '\r'; echo

# --- kill the blocked leader
echo "KILL $svc at $(date -u +%H:%M:%S.%3N)"
$DC kill -s SIGKILL "$svc" >/dev/null 2>&1
echo "kill returned $(date -u +%H:%M:%S.%3N)"

# --- wait for the successor's UPDATE to block on the same row
echo "--- waiting for the SUCCESSOR's blocked UPDATE ---"
for i in $(seq 1 60); do
  out=$(psql "SELECT to_char(NOW(),'HH24:MI:SS.MS')||' pid='||pid||' ip='||coalesce(client_addr::text,'?')||' waiting='||coalesce(wait_event_type,'-')||'/'||coalesce(wait_event,'-') FROM pg_stat_activity WHERE query ILIKE '%UPDATE schedules SET last_fired_at%' AND pid <> pg_backend_pid();")
  if [ -n "$out" ]; then echo "SUCCESSOR BLOCKED: $out"; break; fi
  sleep 0.5
done
psqlt "SELECT id, status, to_char(created_at,'HH24:MI:SS.MS') AS created, triggered_by FROM runs ORDER BY created_at DESC LIMIT 5;"
psqlt "SELECT to_char(NOW(),'HH24:MI:SS.MS') AS t, to_char(last_fired_at,'HH24:MI:SS') AS lfa FROM schedules WHERE name='edge-every-minute';"

echo "--- waiting for the lock to be released (pg_sleep 70) ---"
wait $LOCKPID
cat "$S/partC-lock.txt"
sleep 5
psqlt "SELECT to_char(NOW(),'HH24:MI:SS.MS') AS t, to_char(last_fired_at,'HH24:MI:SS') AS lfa FROM schedules WHERE name='edge-every-minute';"
psqlt "SELECT id, status, to_char(created_at,'HH24:MI:SS.MS') AS created, claimed_by, triggered_by FROM runs ORDER BY created_at DESC LIMIT 8;"
$DC start "$svc" >/dev/null 2>&1
echo "restored $svc at $(date -u +%H:%M:%S.%3N)"
