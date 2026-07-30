#!/usr/bin/env sh
# W2-4 sampler. Usage (from test/ha): w2-4-sample.sh <runid> <iterations> <sleep-seconds>
# Runs the poll loop INSIDE the postgres container so each sample is a local
# psql invocation rather than a docker exec round trip.
#   COMPOSE_FILES - optional; defaults to W2-4's stack.
set -eu
RUN="$1"; N="$2"; SLEEP="$3"
CF="${COMPOSE_FILES:--f docker-compose.ha.yaml -f ../edgecase/compose/oneway.override.yaml -f ../edgecase/compose/queuedgrace.override.yaml}"
docker compose $CF exec -T postgres sh -c "
for i in \$(seq 1 $N); do
  psql -U unified -tAF'|' -c \"SELECT NOW(), r.id, r.status, r.created_at, r.updated_at, r.claimed_by, r.claimed_at,
      round(EXTRACT(EPOCH FROM (NOW() - r.created_at))::numeric,3) AS age,
      (SELECT count(*) FROM agents) AS agent_rows,
      (SELECT max(last_seen_at) FROM agents) AS newest_hb
    FROM runs r WHERE r.id = '$RUN';\"
  sleep $SLEEP
done"
