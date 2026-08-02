#!/usr/bin/env bash
# Sample `pg_stat_activity` on a fixed grid and write one CSV row per
# (sample, client, derived pool, state) plus a per-sample total, so connection
# pressure can be read as a time series instead of as one lucky snapshot.
# W6-S1's core metric; nothing in the campaign produced it before.
#
# THE PLAN ASKED FOR A BREAKDOWN "BY POOL / APPLICATION". APPLICATION IS NOT
# AVAILABLE — it is empty for every controller connection, because the product
# never sets it: `grep -rn 'application_name\|ApplicationName' internal/ cmd/`
# returns ZERO hits, no DSN in `test/ha/docker-compose.ha.yaml` carries the
# parameter, and pgx does not default one. Measured, not merely code-read:
# see the `application_name` column in this tool's own output — every
# controller row is blank and only `psql` (this script) names itself.
#
# POOL ATTRIBUTION IS THEREFORE **DERIVED**, and the derivation is stated here
# so no number taken from this tool is over-read. The controller opens four
# pgxpools over one DSN (`internal/store/postgres.go:64-73`): api (128),
# background (32), lock (16), listen (128). Postgres cannot see that
# structure. What it can see is the LAST statement each backend ran, which is
# retained in `pg_stat_activity.query` after the backend goes idle, and two of
# the four pools have a signature statement:
#
#   listen   `LISTEN "..."` — `ListenForNotify` (`postgres.go:1665-1677`) is the
#            only caller, it runs LISTEN once and then blocks in
#            `WaitForNotification` for the life of the stream, so the retained
#            query stays LISTEN for exactly as long as the connection is a
#            subscriber's. This attribution is SOUND.
#   lock     `SELECT pg_try_advisory_lock($1)` / `pg_advisory_unlock($1)` —
#            `AcquireAdvisoryLock` (`postgres.go:1424-1446`) is the only caller
#            and holds the connection for the lock's lifetime. SOUND while
#            held; a released lock connection returns to the pool still showing
#            the unlock, which is still unambiguous.
#   api vs background
#            NOT separable. Both run ordinary CRUD over the same tables. They
#            are reported together as `query`, and any claim that splits them
#            is unsupported by this instrument.
#
# The client column IS authoritative: each controller container has its own IP,
# and this script resolves those IPs to compose service names at startup, so
# per-replica counts are measured rather than inferred.
#
# Usage:
#   w6-pgsample.sh [-i INTERVAL_S] [-d DURATION_S] [-o OUT.csv] [-r RAW.txt] [-l LABEL]
# Env:
#   COMPOSE_FILES  compose args (default: -f docker-compose.ha.yaml). Run from test/ha/.
#   PGUSER_        psql user (default unified)
#   PGDB_          database  (default unified)
#
# Example (60 s at 1 Hz while an SSE fan-out is held open):
#   cd test/ha
#   export COMPOSE_FILES="-f docker-compose.ha.yaml -f ../edgecase/compose/ctrlports.override.yaml"
#   ../edgecase/tools/w6/w6-pgsample.sh -i 1 -d 60 -o /path/pg.csv -l sse-10
#
# GIT BASH: MSYS_NO_PATHCONV=1 is set PER DOCKER CALL, never exported. The SQL
# below contains `client_addr::text`, which MSYS would otherwise try to convert
# as a path list; but exporting it globally breaks any `curl -o`/`@file` in the
# same shell (measured while building the W6 harnesses).
set -euo pipefail

interval=2
duration=60
out=""
raw=""
label=""
while getopts "i:d:o:r:l:h" opt; do
  case "${opt}" in
    i) interval="${OPTARG}" ;;
    d) duration="${OPTARG}" ;;
    o) out="${OPTARG}" ;;
    r) raw="${OPTARG}" ;;
    l) label="${OPTARG}" ;;
    *) sed -n '/^# Usage:/,/^set -euo/p' "$0" >&2; exit 2 ;;
  esac
done

COMPOSE_FILES="${COMPOSE_FILES:--f docker-compose.ha.yaml}"
PGUSER_="${PGUSER_:-unified}"
PGDB_="${PGDB_:-unified}"
dc() { MSYS_NO_PATHCONV=1 docker compose ${COMPOSE_FILES} "$@"; }
psql_() { dc exec -T postgres psql -U "${PGUSER_}" -d "${PGDB_}" -At -F',' -c "$1" | tr -d '\r'; }

# --- resolve container IPs to service names once, so client_addr is readable ---
declare -A IPNAME
while IFS=$'\t' read -r name ip; do
  [ -n "${ip}" ] && IPNAME["${ip}"]="${name}"
done < <(dc ps -q | while read -r cid; do
  [ -n "${cid}" ] || continue
  MSYS_NO_PATHCONV=1 docker inspect -f '{{index .Config.Labels "com.docker.compose.service"}}	{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${cid}"
done)

maxconn=$(psql_ "select current_setting('max_connections')")
reserved=$(psql_ "select current_setting('superuser_reserved_connections')")
echo "w6-pgsample: max_connections=${maxconn} superuser_reserved=${reserved} interval=${interval}s duration=${duration}s label=${label:-none}"
echo "w6-pgsample: known clients: $(for k in "${!IPNAME[@]}"; do printf '%s=%s ' "${IPNAME[$k]}" "$k"; done)"

Q="
select
  -- host(), not client_addr::text: the latter renders '172.20.0.6/32' and the
  -- /32 makes the docker-inspect IP lookup miss, so every row came back
  -- unresolved the first time this ran.
  host(client_addr),
  coalesce(nullif(application_name,''),'(unset)'),
  case
    when query like 'LISTEN %' then 'listen'
    when query like '%advisory_lock%' or query like '%advisory_unlock%' then 'lock'
    when query is null or query = '' then 'unknown'
    else 'query'
  end,
  state,
  count(*)
from pg_stat_activity
where datname = '${PGDB_}'
group by 1,2,3,4
order by 5 desc;
"
TOT="select count(*) from pg_stat_activity;"
DBTOT="select count(*) from pg_stat_activity where datname = '${PGDB_}';"

if [ -n "${out}" ]; then
  printf 'sample,ts_utc,elapsed_s,label,client_addr,service,application_name,pool_derived,state,count\n' > "${out}"
fi
[ -n "${raw}" ] && : > "${raw}"

start=$(date +%s)
i=0
while :; do
  now=$(date +%s)
  elapsed=$(( now - start ))
  [ "${elapsed}" -ge "${duration}" ] && break
  i=$(( i + 1 ))
  ts=$(date -u +%FT%T.%3NZ)
  rows=$(psql_ "${Q}")
  tot=$(psql_ "${TOT}")
  dbtot=$(psql_ "${DBTOT}")
  if [ -n "${raw}" ]; then
    { echo "== sample ${i} ${ts} elapsed=${elapsed}s total_backends=${tot} db_backends=${dbtot}"; printf '%s\n' "${rows}"; } >> "${raw}"
  fi
  printf '%s sample=%-4d total_backends=%-4s db_backends=%-4s of max_connections=%s\n' "${ts}" "${i}" "${tot}" "${dbtot}" "${maxconn}"
  if [ -n "${out}" ]; then
    # `client_addr` is NULL (empty in -At output) for connections over the unix
    # socket — i.e. psql inside the container, including this script's own.
    # Indexing a bash associative array with an empty subscript is a hard error
    # under `set -e`, which killed the sampling loop after one sample the first
    # time this ran; resolve the key before the lookup.
    printf '%s\n' "${rows}" | while IFS=, read -r addr app pool state cnt; do
      [ -n "${cnt}" ] || continue
      if [ -n "${addr}" ]; then
        svc="${IPNAME[${addr}]:-${addr}}"
      else
        addr="unix-socket"; svc="unix-socket(psql)"
      fi
      printf '%d,%s,%d,%s,%s,%s,%s,%s,%s,%s\n' "${i}" "${ts}" "${elapsed}" "${label}" "${addr}" "${svc}" "${app}" "${pool}" "${state}" "${cnt}" >> "${out}"
    done
    printf '%d,%s,%d,%s,-,TOTAL,-,-,-,%s\n' "${i}" "${ts}" "${elapsed}" "${label}" "${tot}" >> "${out}"
  fi
  sleep "${interval}"
done

echo "w6-pgsample: ${i} samples over ${duration}s${out:+ -> ${out}}${raw:+, raw -> ${raw}}"
if [ -n "${out}" ]; then
  echo "--- peak per service/pool over the window (derived; api and background are NOT separable) ---"
  python - "${out}" <<'PY'
import csv, sys, collections
peak = collections.defaultdict(int)
tot = []
with open(sys.argv[1]) as f:
    for r in csv.DictReader(f):
        if r["service"] == "TOTAL":
            tot.append(int(r["count"]))
            continue
        peak[(r["service"], r["pool_derived"], r["state"])] = max(
            peak[(r["service"], r["pool_derived"], r["state"])], int(r["count"]))
for k in sorted(peak):
    print("  %-14s %-8s %-22s peak=%d" % (k[0], k[1], k[2], peak[k]))
if tot:
    print("  TOTAL backends: min=%d max=%d mean=%.1f over %d samples" % (min(tot), max(tot), sum(tot)/len(tot), len(tot)))
PY
fi
