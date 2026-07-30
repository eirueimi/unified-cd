#!/usr/bin/env sh
# W2-5 Part B (v2): trigger edge-call-parent, arm the steplock mid-prelude for
# the CLAIMING agent, clear it as soon as a child row NEWER than the parent
# appears. v1 broke because the child-row poll matched a previous trial's child.
set -eu
SCRATCH="${SCRATCH:?run from test/ha with SCRATCH exported, e.g. export SCRATCH=<scratchpad>/w2-5}"
COMPOSE_FILES="${COMPOSE_FILES:--f docker-compose.ha.yaml -f ../edgecase/compose/oneway.override.yaml -f ../edgecase/compose/steplink.override.yaml}"
psql() { docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "$1"; }

echo "=== B1 trigger $(date -u +%FT%T.%3NZ)"
curl -fsS -X POST localhost:18080/api/v1/runs -H "Authorization: Bearer ha-admin-token" \
  -H 'Content-Type: application/json' -d '{"jobName":"edge-call-parent"}'
echo
sleep 2
PARENT=$(psql "SELECT id FROM runs WHERE job_name='edge-call-parent' ORDER BY created_at DESC LIMIT 1;" | tr -d '\r')
AG=$(psql "SELECT claimed_by FROM runs WHERE id='$PARENT';" | tr -d '\r')
PCREATED=$(psql "SELECT created_at FROM runs WHERE id='$PARENT';" | tr -d '\r')
echo "PARENT=$PARENT AG=$AG PCREATED=$PCREATED"
psql "SELECT NOW(), id, status, claimed_by, claimed_at, created_at FROM runs WHERE id='$PARENT';"
printf '%s\n' "$PARENT" > "$SCRATCH/partB-parent-id.txt"
printf '%s\n' "$AG"     > "$SCRATCH/partB-agent.txt"

# B2. Arm mid-prelude: the prelude runs 20 s from the claim, so ~10 s in.
sleep 8
echo "=== B2 arm at $(date -u +%FT%T.%3NZ)"
psql "SELECT NOW() AS db_arm_instant;"
sh ../edgecase/tools/inject.sh steplock "$AG"
curl -s -o /dev/null -w "armed-probe steps=%{http_code} at $(date -u +%FT%T.%3NZ)\n" \
  -X POST "localhost:18080/api/v1/agents/$AG/steps"

# B3. Poll for a child row created AFTER the parent, then clear immediately.
echo "=== B3 waiting for a child row newer than the parent"
CHILD=""
i=0
while [ $i -lt 400 ]; do
  CHILD=$(psql "SELECT id FROM runs WHERE job_name='edge-call-child' AND created_at > '$PCREATED' ORDER BY created_at DESC LIMIT 1;" | tr -d '\r')
  if [ -n "$CHILD" ]; then break; fi
  i=$((i+1))
done
echo "=== B3 child row seen at $(date -u +%FT%T.%3NZ) after $i polls: CHILD=$CHILD"
psql "SELECT NOW() AS db_child_seen;"
sh ../edgecase/tools/inject.sh steplock-clear
echo "=== B3 cleared at $(date -u +%FT%T.%3NZ)"
psql "SELECT NOW() AS db_clear_instant;"
curl -s -o /dev/null -w "cleared-probe steps=%{http_code}\n" \
  -X POST "localhost:18080/api/v1/agents/$AG/steps"
printf '%s\n' "$CHILD" > "$SCRATCH/partB-child-id.txt"
psql "SELECT id, job_name, status, claimed_by, triggered_by, created_at FROM runs WHERE id='$CHILD';"
echo "=== B3 parent step rows immediately after:"
psql "SELECT run_id, step_index, step_name, status, child_run_id FROM step_reports WHERE run_id='$PARENT' ORDER BY step_index;"
