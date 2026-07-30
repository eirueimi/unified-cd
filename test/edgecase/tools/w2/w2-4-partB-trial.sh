#!/usr/bin/env sh
# W2-4 Part B trial. Usage (from test/ha): w2-4-partB-trial.sh <offset-seconds> <tag>
# Returns the agent at created_at + 30s + OFFSET, where the deadline is computed
# on the DB clock and the host sleep is corrected for host<->DB skew.
# LEAD (0.400s) is the measured `docker start` -> agents-row-insert latency
# (partB-calibration2.txt); the trial reports the MEASURED registration offset,
# not the intended one.
set -eu
OFFSET="$1"; TAG="$2"
LEAD=0.400
GRACE=30
SCRATCH="${SCRATCH:?run from test/ha with SCRATCH exported, e.g. export SCRATCH=<scratchpad>/w2-4}"
S="$SCRATCH"
CF="${COMPOSE_FILES:--f docker-compose.ha.yaml -f ../edgecase/compose/oneway.override.yaml -f ../edgecase/compose/queuedgrace.override.yaml}"
PROJ="${COMPOSE_PROJECT:-unified-cd-ha}"
dcpsql() { docker compose $CF exec -T postgres psql -U unified -tAF'|' -c "$1"; }

echo "### trial=$TAG offset=${OFFSET}s"
echo "agents_before=$(dcpsql 'SELECT count(*) FROM agents;')"

RESP=$(curl -fsS -X POST localhost:18080/api/v1/runs -H "Authorization: Bearer ha-admin-token" \
  -H 'Content-Type: application/json' -d '{"jobName":"edge-tick"}')
RUN=$(printf '%s' "$RESP" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
echo "run=$RUN"
echo "$RESP" > "$S/partB-$TAG-trigger.json"

# Start the 0.2s sampler in the background FIRST (its `docker compose exec`
# startup is ~10s, which the >=25s pre-return wait absorbs).
docker compose $CF exec -T postgres sh -c "for i in \$(seq 1 420); do psql -U unified -tAF'|' -c \"SELECT NOW(), r.status, r.created_at, r.updated_at, r.claimed_by, r.claimed_at, round(EXTRACT(EPOCH FROM (NOW()-r.created_at))::numeric,3), (SELECT count(*) FROM agents), (SELECT max(last_seen_at) FROM agents) FROM runs r WHERE r.id='$RUN';\"; sleep 0.2; done" > "$S/partB-$TAG-poll.txt" 2>&1 &
POLLPID=$!

H0=$(date +%s.%N)
ROW=$(dcpsql "SELECT EXTRACT(EPOCH FROM (SELECT created_at FROM runs WHERE id='$RUN')), EXTRACT(EPOCH FROM NOW());")
H1=$(date +%s.%N)
CREATED=$(printf '%s' "$ROW" | cut -d'|' -f1)
DBNOW=$(printf '%s' "$ROW" | cut -d'|' -f2)
echo "created_epoch=$CREATED db_now_epoch=$DBNOW host_t0=$H0 host_t1=$H1"

SLEEPFOR=$(awk -v c="$CREATED" -v g="$GRACE" -v o="$OFFSET" -v l="$LEAD" -v d="$DBNOW" -v a="$H0" -v b="$H1" \
  'BEGIN{target=c+g+o-l; mid=(a+b)/2; now=b; printf "%.3f", (target-d)-(now-mid)}')
echo "sleep_for=$SLEEPFOR"
sleep "$SLEEPFOR"

T0=$(date -u +%FT%T.%3NZ); docker start "$PROJ-agent1-1" >/dev/null; T1=$(date -u +%FT%T.%3NZ)
echo "start_issued_host=$T0 start_returned_host=$T1"

wait $POLLPID
docker stop "$PROJ-agent1-1" >/dev/null
echo "agents_after_stop=$(dcpsql 'SELECT count(*) FROM agents;')"

echo "--- transitions ---"
awk -F'|' '{if($2!=p){print; p=$2}}' "$S/partB-$TAG-poll.txt"
echo "--- agent row appears ---"
awk -F'|' '$8>0{print "first_row_sample="$0; exit}' "$S/partB-$TAG-poll.txt"
echo "--- final ---"
dcpsql "SELECT id,status,created_at,updated_at,claimed_by,claimed_at, round(EXTRACT(EPOCH FROM (updated_at-created_at))::numeric,3) AS age_at_final FROM runs WHERE id='$RUN';"
dcpsql "SELECT step_index, ts, line FROM logs WHERE run_id='$RUN' ORDER BY ts;"
