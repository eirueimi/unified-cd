#!/usr/bin/env bash
# usage (from test/ha): w2-8-partF-capture.sh <label> <runID> <hhmm-prefix-for-log-grep>
# Captures every artifact Part F keys on, for one attempt.
set -u
LBL="$1"; RID="$2"; TP="${3:-}"
SCRATCH="${SCRATCH:?run from test/ha with SCRATCH exported, e.g. export SCRATCH=<scratchpad>/w2-8}"
export MSYS_NO_PATHCONV=1
# Run from test/ha (this script does not cd for you).
[ -f docker-compose.ha.yaml ] || { echo "run me from test/ha" >&2; exit 1; }
CF="${COMPOSE_FILES:--f docker-compose.ha.yaml}"
PSQL="docker compose $CF exec -T postgres psql -U unified"
OUT="$SCRATCH/partF-$LBL-evidence.txt"
{
echo "################ PART F attempt $LBL — run $RID ################"
echo "captured $(date -u +%FT%T.%NZ) (host clock)"
echo
echo "=== 1. run status + timeline ==="
$PSQL -c "SELECT id, job_name, status, created_at, claimed_at, claimed_by, updated_at FROM runs WHERE id='$RID';"
echo "=== 2. run_approvals ==="
$PSQL -c "SELECT step_index, step_name, status, decided_by, decided_at, comment, created_at, timeout_at, timeout_at - now() AS expires_in FROM run_approvals WHERE run_id='$RID';"
echo "=== 3. step_reports (ALL steps — note whether a row exists for step 2 'after') ==="
$PSQL -c "SELECT step_index, step_name, status, started_at, ended_at, exit_code FROM step_reports WHERE run_id='$RID' ORDER BY step_index;"
echo "=== 4. logs table — did the post-gate step produce OUTPUT? ==="
$PSQL -c "SELECT step_index, stream, ts, line FROM logs WHERE run_id='$RID' ORDER BY step_index, ts;"
echo "=== 5. audit_logs for this run ==="
$PSQL -c "SELECT id, occurred_at, actor, method, path, action, status FROM audit_logs WHERE resource='$RID' ORDER BY id;"
echo "=== 6. decision-vs-cancel ordering, one clock (Postgres) ==="
$PSQL -c "SELECT (SELECT updated_at FROM runs WHERE id='$RID') AS run_cancelled_at, (SELECT decided_at FROM run_approvals WHERE run_id='$RID' AND step_index=1) AS approval_decided_at, (SELECT decided_at FROM run_approvals WHERE run_id='$RID' AND step_index=1) - (SELECT updated_at FROM runs WHERE id='$RID') AS decided_minus_cancelled, (SELECT ts FROM logs WHERE run_id='$RID' AND step_index=2 ORDER BY ts LIMIT 1) AS post_gate_step_output_at, (SELECT ts FROM logs WHERE run_id='$RID' AND step_index=2 ORDER BY ts LIMIT 1) - (SELECT updated_at FROM runs WHERE id='$RID') AS output_minus_cancelled;"
echo
echo "=== 7. controller HTTP: every request naming this run (agent + human), time-sorted ==="
docker compose $CF logs --no-log-prefix --since 10m controller1 controller2 controller3 \
  | grep "$RID" | grep '"msg":"http request"' \
  | sed 's/.*"time":"\([^"]*\)".*"method":"\([^"]*\)","path":"\([^"]*\)","status":\([0-9]*\).*/\1 \2 \4 \3/' | sort
echo
echo "=== 8. controller HTTP: POST /api/v1/agents/*/steps  (ReportStep — path carries NO runID, so it must be"
echo "        matched by time window, not by run id.  status 204 = persisted, 200 = alreadyFinalized no-op) ==="
docker compose $CF logs --no-log-prefix --since 10m controller1 controller2 controller3 \
  | grep -E '"path":"/api/v1/agents/[^"]*/steps"' \
  | sed 's/.*"time":"\([^"]*\)".*"method":"\([^"]*\)","path":"\([^"]*\)","status":\([0-9]*\).*/\1 \2 \4 \3/' | sort | grep -E "T${TP}"
echo
echo "=== 9. agent container logs mentioning this run ==="
docker compose $CF logs --no-log-prefix --since 10m agent1 agent2 | grep "$RID"
echo "=== 9b. agent container logs: any cancel/interrupt line in the window ==="
docker compose $CF logs --no-log-prefix --since 10m agent1 agent2 | grep -E "cancellation|interrupting|reported run terminal|approval" | grep -E "T${TP}"
} > "$OUT" 2>&1
echo "wrote $OUT"
