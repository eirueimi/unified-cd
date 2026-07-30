#!/usr/bin/env sh
# Arm D1: race the failOrphanedRun window (MarkRunFinished COMMIT -> the first
# cancelDescendantRuns SELECT), measured at 1 ms in Arm D0.
#
# Targeting: the stuck-run sweep fires in a tight cluster every 30.000 s with a
# fixed phase (D0: 8 clusters, within-cluster spread 2-11 ms, between-cluster
# gap 29.987-30.001 s). controller1 won the lock in 8/8 clusters and is always
# the first winner, so it is the kill target; controllers 2 and 3 stay alive so
# their tickers keep phase. One kill per attempt, aimed at
# (predicted sweep + ~5 ms), the offset from D0's SELECT->COMMIT measurement.
#
# Usage: armD1.sh <first> <last> <phase_seconds_within_minute>
set -u
SCRATCH="${SCRATCH:?run from test/ha with SCRATCH exported, e.g. export SCRATCH=<scratchpad>/w2-3}"
S="$SCRATCH"
CF="${COMPOSE_FILES:--f docker-compose.ha.yaml -f ../edgecase/compose/oneway.override.yaml}"
# Addressed by container name, not `docker compose kill`: the kill round trip is
# itself the number under test (T0/T1 below) and compose adds a second layer.
PROJ="${COMPOSE_PROJECT:-unified-cd-ha}"
export COMPOSE_FILES="$CF"
PSQL() { docker compose $CF exec -T postgres psql -U unified -tAc "$1"; }
OUT="$S/armD1-attempts.txt"
PHASE="$3"          # seconds-within-a-30s-cycle at which the sweep fires
KILLLAT="${KILLLAT:-0.30}"   # measured docker-kill round trip, subtracted from the aim point

for N in $(seq "$1" "$2"); do
  echo "=================== ATTEMPT $N $(date -u +%FT%T.%3NZ)" | tee -a "$OUT"
  sh ../edgecase/tools/inject.sh nginx-unblock x >/dev/null 2>&1
  for i in $(seq 1 40); do
    C=$(PSQL "SELECT count(*) FROM agents;"); [ "$C" = "2" ] && break; sleep 2
  done

  P=$(curl -fsS -X POST localhost:18080/api/v1/runs -H "Authorization: Bearer ha-admin-token" \
      -H 'Content-Type: application/json' -d '{"jobName":"edge-call-parent"}' \
      | sed -E 's/.*"id":"([^"]+)".*/\1/')
  AG=""; CH=""
  for i in $(seq 1 45); do
    L=$(PSQL "SELECT coalesce(r.claimed_by,'-')||'~'||coalesce((SELECT child_run_id::text FROM step_reports WHERE run_id='$P' AND child_run_id IS NOT NULL LIMIT 1),'none') FROM runs r WHERE r.id='$P';")
    case "$L" in *none*) sleep 2 ;; *) AG=$(echo "$L"|cut -d'~' -f1); CH=$(echo "$L"|cut -d'~' -f2); break ;; esac
  done
  if [ -z "$CH" ]; then echo "ATTEMPT $N SKIPPED: no link" | tee -a "$OUT"; continue; fi
  echo "parent=$P agent=$AG child=$CH" | tee -a "$OUT"

  for i in $(seq 1 90); do
    A=$(PSQL "SELECT round(EXTRACT(EPOCH FROM (NOW()-claimed_at))::numeric,1) FROM runs WHERE id='$P';")
    [ "$(awk -v a="$A" 'BEGIN{print (a>=50)?"y":"n"}')" = "y" ] && break
    sleep 2
  done
  sh ../edgecase/tools/inject.sh nginx-block "$AG" >/dev/null 2>&1
  PSQL "DELETE FROM agents WHERE id='$AG';" >/dev/null
  echo "injected claim_age=$A at $(date -u +%FT%T.%3NZ)" | tee -a "$OUT"

  # Sleep until the next predicted sweep boundary, minus the docker-kill latency.
  W=$(awk -v ph="$PHASE" -v kl="$KILLLAT" -v now="$(date -u +%s.%N)" \
      'BEGIN{c=now%30; d=ph-c; while(d<3) d+=30; printf "%.3f", d-kl}')
  echo "  sleeping ${W}s to the predicted sweep" | tee -a "$OUT"
  sleep "$W"
  T0=$(date -u +%FT%T.%3NZ)
  docker kill -s SIGKILL "$PROJ-controller1-1" >/dev/null 2>&1
  T1=$(date -u +%FT%T.%3NZ)
  docker start "$PROJ-controller1-1" >/dev/null 2>&1
  echo "  kill issued=$T0 returned=$T1" | tee -a "$OUT"

  sleep 45
  R=$(PSQL "SELECT (SELECT status FROM runs WHERE id='$P')||'|'||(SELECT status FROM runs WHERE id='$CH')||'|'||(SELECT updated_at FROM runs WHERE id='$P')||'|'||(SELECT updated_at FROM runs WHERE id='$CH');")
  echo "RESULT attempt=$N $R" | tee -a "$OUT"
  case "$R" in
    Failed\|Cancelled*) echo "  -> miss (cascade completed)" | tee -a "$OUT" ;;
    Failed\|*)          echo "  -> ***HIT CANDIDATE*** parent Failed, child not Cancelled" | tee -a "$OUT" ;;
    *)                  echo "  -> inconclusive (parent not Failed)" | tee -a "$OUT" ;;
  esac
done
