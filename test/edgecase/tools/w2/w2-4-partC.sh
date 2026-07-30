#!/usr/bin/env sh
# W2-4 Part C — the created_at clock. A run that spends the grace period
# legitimately blocked on a mutex enters Queued already past minAge, so the
# 30s grace buys it nothing. No DB mutation anywhere: the mutex is released by
# the product's own stuck-run reaper failing the holder.
# Usage (from test/ha): w2-4-partC.sh <age-seconds-before-kill>
set -eu
AGE="${1:-120}"
SCRATCH="${SCRATCH:?run from test/ha with SCRATCH exported, e.g. export SCRATCH=<scratchpad>/w2-4}"
S="$SCRATCH"
CF="${COMPOSE_FILES:--f docker-compose.ha.yaml -f ../edgecase/compose/oneway.override.yaml -f ../edgecase/compose/queuedgrace.override.yaml}"
PROJ="${COMPOSE_PROJECT:-unified-cd-ha}"
dcpsql() { docker compose $CF exec -T postgres psql -U unified -tAF'|' -c "$1"; }

echo "== C1: agents up =="
docker start "$PROJ-agent1-1" "$PROJ-agent2-1" >/dev/null
sleep 6
dcpsql "SELECT NOW(), id, last_seen_at FROM agents ORDER BY id;"

echo "== C1: trigger the mutex hog =="
HOG=$(curl -fsS -X POST localhost:18080/api/v1/runs -H "Authorization: Bearer ha-admin-token" \
  -H 'Content-Type: application/json' -d '{"jobName":"edge-mutex-hog"}' | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
echo "hog=$HOG"
sleep 8
dcpsql "SELECT NOW(), id, status, claimed_by, claimed_at FROM runs WHERE id='$HOG';"
dcpsql "SELECT * FROM mutex_holders;"

echo "== C2: trigger the victim (same mutex) =="
VIC=$(curl -fsS -X POST localhost:18080/api/v1/runs -H "Authorization: Bearer ha-admin-token" \
  -H 'Content-Type: application/json' -d '{"jobName":"edge-sideeffect"}' | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
echo "victim=$VIC"
sleep 5
dcpsql "SELECT NOW(), id, status, created_at FROM runs WHERE id='$VIC';"   # expect Pending

echo "== letting the victim age ${AGE}s past creation while it stays Pending =="
sleep "$AGE"
dcpsql "SELECT NOW(), id, status, round(EXTRACT(EPOCH FROM (NOW()-created_at))::numeric,3) AS age FROM runs WHERE id='$VIC';"

echo "== C3: kill both agents hard (rows survive, last_seen_at freezes) =="
sh ../edgecase/tools/inject.sh kill-hard agent1
sh ../edgecase/tools/inject.sh kill-hard agent2
echo "kill_host=$(date -u +%FT%T.%3NZ)"
dcpsql "SELECT NOW(), id, last_seen_at FROM agents ORDER BY id;"

echo "== C4: 0.2s sampler across the whole chain (hog reap -> queue -> victim reap) =="
docker compose $CF exec -T postgres sh -c "for i in \$(seq 1 1100); do psql -U unified -tAF'|' -c \"SELECT NOW(),
   (SELECT status FROM runs WHERE id='$HOG') AS hog_status,
   (SELECT updated_at FROM runs WHERE id='$HOG') AS hog_updated,
   (SELECT status FROM runs WHERE id='$VIC') AS vic_status,
   (SELECT updated_at FROM runs WHERE id='$VIC') AS vic_updated,
   round(EXTRACT(EPOCH FROM (NOW()-(SELECT created_at FROM runs WHERE id='$VIC')))::numeric,3) AS vic_age,
   (SELECT count(*) FROM mutex_holders) AS mutex_rows,
   (SELECT count(*) FROM agents) AS agent_rows,
   (SELECT round(EXTRACT(EPOCH FROM (NOW()-max(last_seen_at)))::numeric,3) FROM agents) AS hb_age;\"; sleep 0.2; done" \
   > "$S/armC-poll.txt" 2>&1
echo "== C5: results =="
echo "hog=$HOG victim=$VIC" | tee "$S/armC-ids.txt"
dcpsql "SELECT id, job_name, status, created_at, updated_at, claimed_by, claimed_at FROM runs WHERE id IN ('$HOG','$VIC');"
dcpsql "SELECT run_id, step_index, ts, line FROM logs WHERE run_id IN ('$HOG','$VIC') AND step_index=-1 ORDER BY ts;"
dcpsql "SELECT count(*) FROM mutex_holders;"
echo "--- victim status transitions (first sample of each) ---"
awk -F'|' '{if($4!=p){print; p=$4}}' "$S/armC-poll.txt"
echo "--- hog status transitions ---"
awk -F'|' '{if($2!=p){print; p=$2}}' "$S/armC-poll.txt"
