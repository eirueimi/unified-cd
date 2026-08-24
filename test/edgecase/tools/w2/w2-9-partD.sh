#!/usr/bin/env sh
# W2-9 Part D: does the post-promotion tick process "any accumulated Pending Runs"?
# docs/operator-manual/high-availability.md:163 says it does. Test it with >50 accumulated.
set -u
SCRATCH="${SCRATCH:?run from test/ha with SCRATCH exported, e.g. export SCRATCH=<scratchpad>/w2-9}"
S="$SCRATCH"
CF="${COMPOSE_FILES:--f docker-compose.ha.yaml}"
LEADER="${LEADER:-}"     # scheduler leader to kill; auto-detected below if unset
psql() { docker compose $CF exec -T postgres psql -U unified -tAc "$1"; }

# ---- preconditions, OUTSIDE the tee'd block so a failure really does abort ----
if [ -z "$LEADER" ]; then
  # `docker compose logs` prefixes each line with "<service>-<n>  | ...", and
  # "scheduler became leader" (scheduler.go:55) is the ONLY leadership line in
  # the system. The last one wins.
  LEADER=$(docker compose $CF logs controller1 controller2 controller3 --since 30m 2>&1 \
           | grep -i "scheduler became leader" | tail -1 | sed -E 's/^([a-z0-9_-]+)-[0-9]+ *\|.*/\1/')
fi
case "$LEADER" in
  controller1|controller2|controller3) : ;;
  *) echo "ABORT: could not identify the scheduler leader (got '$LEADER'). Set LEADER= explicitly." >&2; exit 1 ;;
esac
# The contract at docs/operator-manual/high-availability.md:163 is only contradicted ABOVE the
# scheduler.go:58 limit of 50, so a smaller backlog measures nothing.
PEND=$(psql "SELECT count(*) FROM runs WHERE status='Pending';" | tr -d '\r')
case "$PEND" in
  ''|*[!0-9]*) echo "ABORT: could not read the Pending count (got '$PEND')." >&2; exit 1 ;;
esac
if [ "$PEND" -le 50 ]; then
  echo "ABORT: only $PEND Pending runs; Part D needs >50 accumulated." >&2; exit 1
fi

{
echo "=== Part D start $(date -u +%FT%T.%3NZ) ==="
echo "LEADER=$LEADER accumulated_pending=$PEND"
echo "--- pre-state ---"
psql "SELECT status, count(*) FROM runs GROUP BY status ORDER BY status;"
psql "SELECT count(*) AS pending FROM runs WHERE status='Pending';"
psql "SELECT mutex_name, left(run_id::text,8) FROM mutex_holders;"
echo "--- current scheduler leader (from logs) ---"
docker compose $CF logs controller1 controller2 controller3 --since 30m 2>&1 | grep -i "scheduler became leader"
echo "--- arm log_statement ---"
docker compose $CF exec -T postgres psql -U unified -c "ALTER SYSTEM SET log_statement='all';" >/dev/null
docker compose $CF exec -T postgres psql -U unified -c "SELECT pg_reload_conf();" >/dev/null
echo "SHOW log_statement (fresh session): $(docker compose $CF exec -T postgres psql -U unified -tAc 'SHOW log_statement;')"
sleep 3
echo "--- SIGKILL the scheduler leader $LEADER at $(date -u +%FT%T.%3NZ) ---"
docker compose $CF kill -s SIGKILL "$LEADER"
echo "killed at $(date -u +%FT%T.%3NZ)"
i=0
while [ $i -lt 40 ]; do
  L=$(docker compose $CF logs controller1 controller3 --since 3m 2>&1 | grep -i "scheduler became leader" | tail -1)
  if [ -n "$L" ]; then echo "PROMOTED: $L (seen at $(date -u +%FT%T.%3NZ))"; break; fi
  i=$((i+1)); sleep 2
done
sleep 6
echo "--- post-promotion state $(date -u +%FT%T.%3NZ) ---"
psql "SELECT status, count(*) FROM runs GROUP BY status ORDER BY status;"
} 2>&1 | tee "$S/partD-promotion.txt"

docker compose $CF logs --no-log-prefix postgres --since 120s > "$S/partD-pglog-raw.txt" 2>&1
{
echo "=== disarm $(date -u +%FT%T.%3NZ) ==="
docker compose $CF exec -T postgres psql -U unified -c "ALTER SYSTEM RESET log_statement;"
docker compose $CF exec -T postgres psql -U unified -c "SELECT pg_reload_conf();"
echo "SHOW log_statement (fresh session): $(docker compose $CF exec -T postgres psql -U unified -tAc 'SHOW log_statement;')"
} 2>&1 | tee "$S/partD-disarm.txt"

# per-tick candidate counts across the promotion
awk '
/FROM runs WHERE status = .Pending. ORDER BY created_at LIMIT/ { if (tick!="") printf "tick %s host=%s : %d candidates\n", tick, host, n; tick=$1" "$2; host=$5; n=0; next }
/execute stmtcache_[0-9a-f]+: SELECT status FROM runs WHERE id = \$1 FOR UPDATE/ { want=1; next }
want && /DETAIL:  parameters: \$1 = / { n++; want=0 }
END { if (tick!="") printf "tick %s host=%s : %d candidates\n", tick, host, n }
' "$S/partD-pglog-raw.txt" > "$S/partD-tick-candidates.txt"
{
echo "=== per-tick candidate counts across the promotion ==="
cat "$S/partD-tick-candidates.txt"
} 2>&1 | tee "$S/partD-analysis.txt"

echo "--- restart controller2 ---"
docker compose $CF start "$LEADER" 2>&1 | tee -a "$S/partD-promotion.txt"
