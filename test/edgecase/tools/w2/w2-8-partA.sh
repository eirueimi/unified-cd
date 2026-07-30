#!/usr/bin/env bash
# W2-8 Part A. Usage (from test/ha): w2-8-partA.sh <scratch> <trial> <api|cli|none>
# Natural window. Trigger so timeout_at lands ~10s after a reaper sweep
# cluster (grid phase = (epoch mod 60) ~= 5.10s), wait for run=Failed +
# approval=Pending, then fire ONE decision.
set -u
export MSYS_NO_PATHCONV=1
# Run from test/ha (this script does not cd for you).
[ -f docker-compose.ha.yaml ] || { echo "run me from test/ha" >&2; exit 1; }
SCRATCH="$1"; TRIAL="$2"; DECIDE_MODE="$3"   # DECIDE_MODE = api | cli | none
DC="docker compose ${COMPOSE_FILES:--f docker-compose.ha.yaml}"
TOK="Authorization: Bearer ha-admin-token"
BASE=http://localhost:18080
psq(){ $DC exec -T postgres psql -U unified -tAc "$1"; }

# --- aim: trigger at (epoch mod 60) == 44.7 so timeout_at ~= :15 ---
now=$(date -u +%s.%N)
phase=$(awk -v n="$now" 'BEGIN{printf "%.3f", n - int(n/60)*60}')
tgt=$(awk -v n="$now" 'BEGIN{b=int(n/60)*60+44.7; if(b<=n+1.0) b+=60; printf "%.3f", b}')
echo "PLAN trial=$TRIAL now=$now phase=$phase trigger_target=$tgt (grid phase 5.10)"
while :; do c=$(date -u +%s.%N); done_=$(awk -v c="$c" -v t="$tgt" 'BEGIN{print (c>=t)?1:0}'); [ "$done_" = 1 ] && break; sleep 0.05; done

T_POST=$(date -u +%s.%N)
RESP=$(curl -sS -H "$TOK" -H "Content-Type: application/json" -d '{"jobName":"edge-approval-short"}' "$BASE/api/v1/runs")
T_POST_END=$(date -u +%s.%N)
RID=$(echo "$RESP" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
echo "TRIGGER host_before=$T_POST host_after=$T_POST_END runID=$RID"
echo "TRIGGER_BODY $RESP"

DECIDED=0
for i in $(seq 1 90); do
  TS=$(date -u +%s.%N); TSH=$(date -u +%H:%M:%S.%3N)
  ROW=$(psq "SELECT (SELECT status FROM runs WHERE id='$RID')||'|'||coalesce((SELECT updated_at::text FROM runs WHERE id='$RID'),'-')||'|'||coalesce((SELECT status FROM run_approvals WHERE run_id='$RID'),'-')||'|'||coalesce((SELECT decided_by FROM run_approvals WHERE run_id='$RID'),'-')||'|'||coalesce((SELECT timeout_at::text FROM run_approvals WHERE run_id='$RID'),'-')||'|'||coalesce((SELECT claimed_by FROM runs WHERE id='$RID'),'-')||'|db='||clock_timestamp()::text")
  echo "SAMPLE $i host=$TS ($TSH) $ROW"
  RS=$(echo "$ROW" | cut -d'|' -f1); AS=$(echo "$ROW" | cut -d'|' -f3)
  if [ "$DECIDED" = 0 ] && [ "$RS" = "Failed" ] && [ "$AS" = "Pending" ]; then
    echo "=== VULNERABLE STATE CONFIRMED (run=$RS, approval=$AS) at host=$TS ==="
    D0=$(date -u +%s.%N)
    if [ "$DECIDE_MODE" = "api" ]; then
      CODE=$(curl -sS -o "$SCRATCH/$TRIAL-decide-body.txt" -w '%{http_code} %{time_total}' -H "$TOK" -H "Content-Type: application/json" \
        -d '{"decision":"approve","comment":"w2-8 '"$TRIAL"'"}' "$BASE/api/v1/runs/$RID/approvals/1")
      D1=$(date -u +%s.%N)
      echo "DECIDE_API host_before=$D0 host_after=$D1 code_time=$CODE body=$(cat "$SCRATCH/$TRIAL-decide-body.txt")"
    elif [ "$DECIDE_MODE" = "cli" ]; then
      OUT=$(UNIFIED_SERVER=http://localhost:18080 UNIFIED_TOKEN=ha-admin-token "${UNIFIED_CLI:?set UNIFIED_CLI to a built unified-cli binary for DECIDE_MODE=cli}" approve "$RID" 1 --comment "w2-8 cli" 2>&1; echo "rc=$?")
      D1=$(date -u +%s.%N)
      echo "DECIDE_CLI host_before=$D0 host_after=$D1 out<<<$OUT>>>"
    else
      echo "DECIDE_NONE (control: let the reaper have it)"
    fi
    DECIDED=1
    LIMIT=$((i+25))
  fi
  if [ "$DECIDED" = 1 ] && [ "$i" -ge "${LIMIT:-999}" ]; then break; fi
done
echo "RUNID=$RID"
