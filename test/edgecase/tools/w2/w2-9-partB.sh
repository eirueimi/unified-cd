#!/usr/bin/env sh
# W2-9 Part B. Usage (from test/ha): w2-9-partB.sh <probe-run-id>
# Falsification — cancel one at a time, find the real threshold.
set -u
SCRATCH="${SCRATCH:?run from test/ha with SCRATCH exported, e.g. export SCRATCH=<scratchpad>/w2-9}"
S="$SCRATCH"
CF="${COMPOSE_FILES:--f docker-compose.ha.yaml}"
API="http://localhost:18080"
AUTH="Authorization: Bearer ha-admin-token"
PROBE="$1"
psql() { docker compose $CF exec -T postgres psql -U unified -tAc "$1"; }
census() { psql "SELECT (SELECT count(*) FROM runs WHERE status='Pending')||'|probe='||(SELECT status FROM runs WHERE id='$PROBE')||'|q='||(SELECT count(*) FROM runs WHERE status='Queued')||'|r='||(SELECT count(*) FROM runs WHERE status='Running');"; }

{
echo "=== Part B start $(date -u +%FT%T.%3NZ) probe=$PROBE ==="
echo "$(date -u +%H:%M:%S.%3N) BEFORE-ANY-CANCEL pending|probe|queued|running = $(census)"
n=0
armed=0
# cancel the oldest Pending edge-sideeffect runs one at a time
while [ $n -lt 12 ]; do
  victim=$(psql "SELECT id FROM runs WHERE status='Pending' AND job_name='edge-sideeffect' ORDER BY created_at LIMIT 1;")
  victim=$(printf '%s' "$victim" | tr -d '\r')
  [ -z "$victim" ] && { echo "no victim left"; break; }
  # arm the statement log one cancel before the predicted transition (pending==51)
  pend=$(psql "SELECT count(*) FROM runs WHERE status='Pending';" | tr -d '\r')
  if [ "$pend" = "51" ] && [ "$armed" = "0" ]; then
    echo "--- arming log_statement at pending=51, $(date -u +%FT%T.%3NZ) ---"
    docker compose $CF exec -T postgres psql -U unified -c "ALTER SYSTEM SET log_statement='all';" >/dev/null
    docker compose $CF exec -T postgres psql -U unified -c "SELECT pg_reload_conf();" >/dev/null
    echo "SHOW log_statement (fresh session): $(docker compose $CF exec -T postgres psql -U unified -tAc 'SHOW log_statement;')"
    armed=1
  fi
  n=$((n+1))
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$API/api/v1/runs/$victim/cancel" -H "$AUTH")
  echo "$(date -u +%H:%M:%S.%3N) cancel#$n victim=$victim http=$code  t+0 => $(census)"
  sleep 2
  c=$(census)
  echo "$(date -u +%H:%M:%S.%3N) cancel#$n                                        t+2 => $c"
  probe_status=$(printf '%s' "$c" | cut -d'|' -f2 | sed 's/probe=//')
  if [ "$probe_status" != "Pending" ]; then
    echo "*** TRANSITION at cancel#$n : probe=$probe_status, line above carries the Pending count ***"
    break
  fi
done
echo "--- 6 further samples after the transition, 2s apart ---"
i=0
while [ $i -lt 6 ]; do
  echo "$(date -u +%H:%M:%S.%3N) post => $(census)"
  i=$((i+1)); sleep 2
done
echo "=== Part B cancel series end $(date -u +%FT%T.%3NZ) ==="
} 2>&1 | tee "$S/partB-cancel-series.txt"

# capture the statement log across the transition, then disarm
docker compose $CF logs --no-log-prefix postgres --since 60s > "$S/partB-pglog-raw.txt" 2>&1
{
echo "=== disarm $(date -u +%FT%T.%3NZ) ==="
docker compose $CF exec -T postgres psql -U unified -c "ALTER SYSTEM RESET log_statement;"
docker compose $CF exec -T postgres psql -U unified -c "SELECT pg_reload_conf();"
echo "SHOW log_statement (fresh session): $(docker compose $CF exec -T postgres psql -U unified -tAc 'SHOW log_statement;')"
} 2>&1 | tee "$S/partB-pglog-disarm.txt"

# B3: did it actually run?
{
echo "=== B3 probe outcome $(date -u +%FT%T.%3NZ) ==="
psql "SELECT id, job_name, status, claimed_by, created_at, claimed_at, updated_at FROM runs WHERE id='$PROBE';"
echo "--- probe logs ---"
psql "SELECT step_index, stream, line FROM logs WHERE run_id='$PROBE' ORDER BY ts;"
echo "--- step_reports ---"
psql "SELECT step_index, step_name, status, started_at, ended_at FROM step_reports WHERE run_id='$PROBE' ORDER BY step_index;"
echo "--- census ---"
psql "SELECT status, count(*) FROM runs GROUP BY status ORDER BY status;"
} 2>&1 | tee "$S/partB-probe-outcome.txt"
