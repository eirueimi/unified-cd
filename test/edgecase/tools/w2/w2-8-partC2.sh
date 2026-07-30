#!/usr/bin/env bash
# Part C (v2): phase-lock a decision POST onto the approval reaper's 60s sweep
# grid, firing with fire.py (sub-ms accuracy) and refining the predicted sweep
# instant S from the cluster one period earlier (drift ~12 ms/min).
# usage (from test/ha): w2-8-partC2.sh <scratch> <label> <offset_ms>
set -u
export MSYS_NO_PATHCONV=1
# Run from test/ha (this script does not cd for you).
[ -f docker-compose.ha.yaml ] || { echo "run me from test/ha" >&2; exit 1; }
SCRATCH="$1"; LBL="$2"; OFF_MS="$3"
DC="docker compose ${COMPOSE_FILES:--f docker-compose.ha.yaml}"
TOK="Authorization: Bearer ha-admin-token"; BASE=http://localhost:18080
PERIOD=60.012; LEAD=50.25

lastcluster(){ $DC logs --no-log-prefix postgres --since "$1" 2>/dev/null > "$SCRATCH/.c2.log"
  awk -f "$(dirname "$0")/w2-8-grid2.awk" "$SCRATCH/.c2.log" \
  | awk '{ts=$1" "$2; split($2,a,":"); s=a[1]*3600+a[2]*60+a[3]; if(prev=="" || s-prev>5) last=ts; prev=s} END{print last}'; }

REF=$(lastcluster 4m); [ -z "$REF" ] && { echo "ATTEMPT $LBL ABORT no-ref"; exit 1; }
REF_E=$(date -u -d "$REF UTC" +%s.%N); NOW=$(date -u +%s.%N)
S=$(awk -v r="$REF_E" -v p="$PERIOD" -v n="$NOW" -v l="$LEAD" 'BEGIN{k=1; while(r+p*k < n+l+3.0) k++; printf "%.3f", r+p*k}')
TRIG=$(awk -v s="$S" -v l="$LEAD" 'BEGIN{printf "%.3f", s-l}')
echo "ATTEMPT $LBL offset_ms=$OFF_MS ref=$REF S_coarse=$S trigger_at=$TRIG"
python -c "
import time,sys
t=float('$TRIG')
while True:
  d=t-time.time()
  if d<=0: break
  time.sleep(d-0.02 if d>0.05 else 0)
"
T0=$(date -u +%s.%N)
RESP=$(curl -sS -H "$TOK" -H "Content-Type: application/json" -d '{"jobName":"edge-approval-short"}' "$BASE/api/v1/runs")
RID=$(echo "$RESP" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
echo "ATTEMPT $LBL trigger_host=$T0 runID=$RID"

# refine S from the cluster exactly one period before it
python -c "
import time
t=float('$S')-24.0
while True:
  d=t-time.time()
  if d<=0: break
  time.sleep(d-0.02 if d>0.05 else 0)
"
REF2=$(lastcluster 90s); REF2_E=$(date -u -d "$REF2 UTC" +%s.%N)
S2=$(awk -v r="$REF2_E" -v p="$PERIOD" 'BEGIN{printf "%.3f", r+p}')
FIRE=$(awk -v s="$S2" -v o="$OFF_MS" 'BEGIN{printf "%.4f", s+o/1000.0}')
echo "ATTEMPT $LBL ref2=$REF2 S_refined=$S2 fire_at=$FIRE (offset ${OFF_MS}ms)"

OUT=$(python "$(dirname "$0")/w2-8-fire.py" "$FIRE" "$BASE/api/v1/runs/$RID/approvals/1" '{"decision":"approve","comment":"w2-8 '"$LBL"'"}')
echo "ATTEMPT $LBL $OUT"

sleep 6
echo "ATTEMPT $LBL approval_row=$($DC exec -T postgres psql -U unified -tAc "SELECT status||'|'||coalesce(decided_by,'-')||'|'||coalesce(decided_at::text,'-')||'|'||timeout_at::text FROM run_approvals WHERE run_id='$RID';")"
echo "ATTEMPT $LBL run_row=$($DC exec -T postgres psql -U unified -tAc "SELECT status||'|'||updated_at::text FROM runs WHERE id='$RID';")"
$DC logs --no-log-prefix postgres --since 150s 2>/dev/null > "$SCRATCH/.c2post.log"
awk -f "$(dirname "$0")/w2-8-grid2.awk" "$SCRATCH/.c2post.log" | tail -6 | sed "s/^/ATTEMPT $LBL stmt /"
$DC logs --no-log-prefix controller1 controller2 controller3 --since 150s 2>/dev/null | grep -F "marked timed-out approvals" | sed "s/^/ATTEMPT $LBL reaperlog /"
$DC logs --no-log-prefix agent1 agent2 --since 90s 2>/dev/null | grep -F "$RID" | grep -F "approval timed out" | sed "s/^/ATTEMPT $LBL agentlog /"
echo "ATTEMPT $LBL END runID=$RID"
