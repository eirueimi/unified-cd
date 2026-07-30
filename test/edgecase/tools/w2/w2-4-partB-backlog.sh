#!/usr/bin/env sh
# W2-4 Part B5 — amplify the reaper's SELECT->UPDATE window with a backlog.
#
# runQueuedRunReaperOnce lists ALL reapable runs in one SELECT and then loops
# AppendLog + MarkRunFinished per run (queuedrun_reaper.go:59-79). The list is
# never re-validated, so with a backlog of N the last entry is failed roughly
# N x 4ms after the liveness check that justified failing it (4ms measured for
# a single-run batch: partB-ph5a-window.txt). Bringing an agent back inside
# that stretch should produce a run that is claimed and Running when the
# reaper's stale list reaches it.
#
# Usage (from test/ha): partB-backlog.sh <count> <tag>
set -eu
COUNT="$1"; TAG="$2"
LEAD=0.400        # measured docker start -> agents row insert (partB-calibration2.txt)
SWEEPPHASE=29.05  # measured sweep grid position, (epoch mod 30) (partB-sweeps.txt)
INTO=0.25         # how far into the sweep loop we want registration to land
SCRATCH="${SCRATCH:?run from test/ha with SCRATCH exported, e.g. export SCRATCH=<scratchpad>/w2-4}"
S="$SCRATCH"
CF="${COMPOSE_FILES:--f docker-compose.ha.yaml -f ../edgecase/compose/oneway.override.yaml -f ../edgecase/compose/queuedgrace.override.yaml}"
PROJ="${COMPOSE_PROJECT:-unified-cd-ha}"
dcpsql() { docker compose $CF exec -T postgres psql -U unified -tAF'|' -c "$1"; }

echo "agents_before=$(dcpsql 'SELECT count(*) FROM agents;')"
MARK=$(dcpsql "SELECT EXTRACT(EPOCH FROM NOW());")
echo "batch_mark_epoch=$MARK"
sh ../edgecase/tools/bulk-submit.sh edge-tick "$COUNT" > "$S/partB-$TAG-ids.txt" 2>"$S/partB-$TAG-submit.err"
echo "submitted=$(wc -l < "$S/partB-$TAG-ids.txt")"
dcpsql "SELECT count(*), min(created_at), max(created_at) FROM runs WHERE created_at > to_timestamp($MARK);"

MINC=$(dcpsql "SELECT EXTRACT(EPOCH FROM min(created_at)) FROM runs WHERE created_at > to_timestamp($MARK);")
H0=$(date +%s.%N); DBNOW=$(dcpsql "SELECT EXTRACT(EPOCH FROM NOW());"); H1=$(date +%s.%N)

# First sweep instant strictly after the earliest run's reapability boundary.
PLAN=$(awk -v m="$MINC" -v p="$SWEEPPHASE" -v i="$INTO" -v l="$LEAD" -v d="$DBNOW" -v a="$H0" -v b="$H1" 'BEGIN{
  bnd=m+30;
  k=int((bnd-p)/30)+1; s=p+k*30;
  while (s<=bnd) s+=30;
  issue=s+i-l;
  mid=(a+b)/2;
  printf "%.3f %.3f %.3f", s, issue, (issue-d)-(b-mid);
}')
SWEEP=$(echo "$PLAN" | cut -d' ' -f1); ISSUE=$(echo "$PLAN" | cut -d' ' -f2); SLEEPFOR=$(echo "$PLAN" | cut -d' ' -f3)
echo "min_created_epoch=$MINC target_sweep_epoch=$SWEEP planned_issue_epoch=$ISSUE sleep_for=$SLEEPFOR"

docker compose $CF exec -T postgres sh -c "for i in \$(seq 1 300); do psql -U unified -tAF'|' -c \"SELECT NOW(), count(*) FILTER (WHERE status='Queued'), count(*) FILTER (WHERE status='Failed'), count(*) FILTER (WHERE status='Running'), (SELECT count(*) FROM agents) FROM runs WHERE created_at > to_timestamp($MARK);\"; sleep 0.2; done" > "$S/partB-$TAG-poll.txt" 2>&1 &
POLLPID=$!

sleep "$SLEEPFOR"
T0=$(date -u +%FT%T.%3NZ); docker start "$PROJ-agent1-1" >/dev/null; T1=$(date -u +%FT%T.%3NZ)
echo "start_issued_host=$T0 start_returned_host=$T1"
wait $POLLPID
docker stop "$PROJ-agent1-1" >/dev/null

echo "=== DETECTOR: runs the reaper failed that had already been claimed ==="
dcpsql "SELECT r.id, r.status, r.claimed_by, r.claimed_at, r.updated_at,
        round(EXTRACT(EPOCH FROM (r.updated_at - r.claimed_at))::numeric,3) AS claim_to_fail
        FROM runs r
        WHERE r.created_at > to_timestamp($MARK)
          AND r.claimed_by IS NOT NULL
          AND r.status = 'Failed'
        ORDER BY r.updated_at;"
echo "=== ... and which of those carry the reaper's reason line ==="
dcpsql "SELECT l.run_id, l.ts, l.line FROM logs l JOIN runs r ON r.id=l.run_id
        WHERE r.created_at > to_timestamp($MARK) AND r.claimed_by IS NOT NULL
          AND l.step_index = -1 ORDER BY l.ts;"
echo "=== batch outcome ==="
dcpsql "SELECT status, count(*) FROM runs WHERE created_at > to_timestamp($MARK) GROUP BY status ORDER BY status;"
echo "=== agent row insert vs sweep ==="
dcpsql "SELECT NOW(), id, last_seen_at FROM agents;"
