#!/usr/bin/env bash
# W2-6 Arm A: aim a SIGKILL at the leader's own schedule-check instant, hoping to
# land inside the 1-2 ms gap between CreateRun (scheduler.go:189) and
# UpdateScheduleLastFiredAt (:194).
#
# Aiming: the check instants of a leader are promotion + 60k (scheduler.go:71,
# lastScheduleCheck is process-local and zero at promotion, so the first check is
# at promotion itself). Sweep a lead time across attempts to cover docker's own
# kill-delivery latency.
#
# usage (from test/ha): w2-6-armA.sh <first_attempt> <last_attempt>
#
# EXPECT THIS TO MISS. 20 attempts on the 2026-07-30 run landed between
# -0.515 s and +0.037 s of the predicted check and hit 0 times against a
# measured 1-3 ms window. Widen the window with w2-6-partC.sh instead of
# spending more attempts here.
set -u
# Run from test/ha (this script does not cd for you).
[ -f docker-compose.ha.yaml ] || { echo "run me from test/ha" >&2; exit 1; }
export MSYS_NO_PATHCONV=1
DC="docker compose ${COMPOSE_FILES:--f docker-compose.ha.yaml}"

psql() { $DC exec -T postgres psql -U unified -tA -c "$1" 2>&1 | tr -d '\r'; }

# lead times (seconds) swept across attempts
LEADS=(0.05 0.10 0.15 0.20 0.25 0.30 0.35 0.40 0.45 0.50 0.55 0.60 0.12 0.18 0.22 0.28 0.32 0.38 0.42 0.48)

for n in $(seq "$1" "$2"); do
  echo "=================== attempt $n ==================="
  ip=$(psql "SELECT a.client_addr::text FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid WHERE l.locktype='advisory' AND l.objid=1702388580 LIMIT 1;")
  ip=${ip%%/*}   # client_addr is inet, e.g. 172.20.0.3/32
  case "$ip" in
    # IP->service map for THIS rig's compose network. Verify it before trusting
  # the result: `client_addr` is `inet` and renders as 172.20.0.3/32 (hence the
  # ${ip%%/*} strip), and a stale map falls through to "no leader" silently.
  # Rebuild with: docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' <cid>
  172.20.0.4) svc=controller1 ;;
    172.20.0.3) svc=controller2 ;;
    172.20.0.5) svc=controller3 ;;
    *) echo "attempt $n: NO LEADER (ip='$ip') — restoring and skipping"; $DC start controller1 controller2 controller3 >/dev/null 2>&1; sleep 8; continue ;;
  esac
  # promotion instant of the current leader (RFC3339 from docker logs -t)
  t0=$($DC logs -t --no-log-prefix --tail 20000 "$svc" 2>/dev/null | tr -d '\r' | grep "scheduler became leader" | tail -1 | awk '{print $1}')
  if [ -z "$t0" ]; then echo "attempt $n: no promotion line for $svc — skipping"; sleep 5; continue; fi
  t0e=$(date -u -d "$t0" +%s.%N)
  now=$(date -u +%s.%N)
  lead=${LEADS[$(( (n-1) % ${#LEADS[@]} ))]}
  # smallest promotion+60k that is at least 4s away
  target=$(python -c "
import sys
t0=float('$t0e'); now=float('$now')
k=0
while t0+60*k < now+4: k+=1
print('%.3f' % (t0+60*k))")
  sleepfor=$(python -c "print('%.3f' % max(0.0, float('$target')-float('$lead')-float(__import__('time').time())))")
  echo "attempt $n leader=$svc($ip) promoted=$t0 predicted_check=$(date -u -d @$target +%H:%M:%S.%3N) lead=${lead}s sleep=${sleepfor}s"
  sleep "$sleepfor"
  pre=$(date -u +%H:%M:%S.%3N)
  $DC kill -s SIGKILL "$svc" >/dev/null 2>&1
  post=$(date -u +%H:%M:%S.%3N)
  echo "attempt $n KILL $svc issued_pre=$pre issued_post=$post predicted_check=$(date -u -d @$target +%H:%M:%S.%3N) lead=${lead}"
  sleep 3
  $DC start "$svc" >/dev/null 2>&1
  echo "attempt $n restored $svc at $(date -u +%H:%M:%S.%3N)"
  psql "SELECT to_char(NOW(),'HH24:MI:SS.MS')||' lfa='||to_char(last_fired_at,'HH24:MI:SS')||' n='||(SELECT count(*) FROM runs WHERE triggered_by='schedule:edge-every-minute') FROM schedules WHERE name='edge-every-minute';"
  sleep 6
done
