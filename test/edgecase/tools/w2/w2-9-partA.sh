#!/usr/bin/env sh
# W2-9 Part A driver. Run from test/ha with MSYS_NO_PATHCONV=1.
# NOTE: Part D (w2-9-partD.sh) is the strongest and cheapest limb and only
# needs this script's A1+A2 as setup — consider running it before A4.
set -u
SCRATCH="${SCRATCH:?run from test/ha with SCRATCH exported, e.g. export SCRATCH=<scratchpad>/w2-9}"
S="$SCRATCH"
CF="${COMPOSE_FILES:--f docker-compose.ha.yaml}"
API="http://localhost:18080"
AUTH="Authorization: Bearer ha-admin-token"
psql() { docker compose $CF exec -T postgres psql -U unified -tAc "$1"; }
trig() { curl -sS -X POST "$API/api/v1/runs" -H "$AUTH" -H "Content-Type: application/json" -d "{\"jobName\":\"$1\"}"; }
idof() { printf '%s' "$1" | tr ',' '\n' | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1; }
stat() { curl -sS -H "$AUTH" "$API/api/v1/runs/$1" | tr ',' '\n' | sed -n 's/.*"status"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1; }

# ---------- A1: the holder ----------
{
echo "=== A1 hog trigger $(date -u +%FT%T.%3NZ) ==="
B=$(trig edge-mutex-hog); echo "body: $B"
HOG=$(idof "$B"); echo "HOG=$HOG"
i=0
while [ $i -lt 30 ]; do
  s=$(stat "$HOG"); echo "$(date -u +%H:%M:%S.%3N) hog=$s"
  [ "$s" = "Running" ] && break
  i=$((i+1)); sleep 1
done
echo "--- mutex_holders ---"
psql "SELECT mutex_name, run_id, acquired_at FROM mutex_holders;"
echo "--- hog row (t0 = claimed_at) ---"
psql "SELECT id, status, claimed_by, created_at, claimed_at FROM runs WHERE id='$HOG';"
} 2>&1 | tee "$S/partA-hog.txt"

HOG=$(grep '^HOG=' "$S/partA-hog.txt" | head -1 | cut -d= -f2)
if ! grep -q "edge-mutex|$HOG" "$S/partA-hog.txt"; then
  echo "ABORT: hog does not hold edge-mutex" | tee "$S/partA-ABORT.txt"; exit 1
fi

# ---------- A2: saturate ----------
{
echo "=== A2 bulk-submit start $(date -u +%FT%T.%3NZ) ==="
} 2>&1 | tee "$S/partA-bulk-timing.txt"
UNIFIED_SERVER="$API" UNIFIED_TOKEN=ha-admin-token \
  sh ../edgecase/tools/bulk-submit.sh edge-sideeffect 55 \
  > "$S/partA-blocked-ids.txt" 2> "$S/partA-bulk-stderr.txt"
RC=$?
{
echo "bulk-submit rc=$RC end $(date -u +%FT%T.%3NZ)"
echo "ids submitted: $(wc -l < "$S/partA-blocked-ids.txt")"
cat "$S/partA-bulk-stderr.txt"
} 2>&1 | tee -a "$S/partA-bulk-timing.txt"

# ---------- A3: census pre-probe ----------
{
echo "=== A3 census pre-probe $(date -u +%FT%T.%3NZ) ==="
psql "SELECT status, count(*) FROM runs GROUP BY status ORDER BY status;"
echo "--- pending ordering (pos|id|job|status|created_at) ---"
psql "SELECT row_number() OVER (ORDER BY created_at) AS pos, id, job_name, status, created_at FROM runs WHERE status='Pending' ORDER BY created_at;"
} 2>&1 | tee "$S/partA-census-pre.txt"

# ---------- A4: the probe ----------
{
echo "=== A4 probe trigger $(date -u +%FT%T.%3NZ) ==="
B=$(trig edge-unrelated-probe); echo "body: $B"
PROBE=$(idof "$B"); echo "PROBE=$PROBE"
echo "--- probe position among Pending ---"
psql "SELECT pos, id, job_name, created_at FROM (SELECT row_number() OVER (ORDER BY created_at) AS pos, id, job_name, created_at FROM runs WHERE status='Pending') q WHERE id='$PROBE';"
echo "--- total Pending now ---"
psql "SELECT count(*) FROM runs WHERE status='Pending';"
} 2>&1 | tee "$S/partA-probe-trigger.txt"
PROBE=$(grep '^PROBE=' "$S/partA-probe-trigger.txt" | head -1 | cut -d= -f2)

# ---------- A5: poll 180s ----------
{
echo "=== A5 probe poll (5s x 36 = 180s) probe=$PROBE ==="
i=1
while [ $i -le 36 ]; do
  api=$(stat "$PROBE")
  db=$(psql "SELECT status||'|pend='||(SELECT count(*) FROM runs WHERE status='Pending')||'|run='||(SELECT count(*) FROM runs WHERE status='Running')||'|q='||(SELECT count(*) FROM runs WHERE status='Queued') FROM runs WHERE id='$PROBE';")
  echo "$(date -u +%H:%M:%S.%3N) sample=$i api=$api db=$db"
  i=$((i+1)); sleep 4
done
} 2>&1 | tee "$S/partA-probe-poll.txt"

# ---------- A6: nothing else running ----------
{
echo "=== A6 running rows $(date -u +%FT%T.%3NZ) ==="
psql "SELECT id, job_name, status, claimed_by, claimed_at FROM runs WHERE status IN ('Running','Queued');"
echo "--- census post ---"
psql "SELECT status, count(*) FROM runs GROUP BY status ORDER BY status;"
echo "--- scheduler enqueued lines (expect 0 after the hog) ---"
docker compose $CF logs --no-log-prefix controller1 controller2 controller3 --since 300s 2>/dev/null | grep -c "scheduler enqueued"
} 2>&1 | tee "$S/partA-running.txt"

echo "PARTA_DONE HOG=$HOG PROBE=$PROBE" | tee "$S/partA-summary.txt"
