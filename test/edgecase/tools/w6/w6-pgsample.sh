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
# -p SAMPLES /readyz ON THE SAME GRID, AND WHY THAT IS A SEPARATE FEATURE.
# The W6-infra observation (`FINDINGS.md:2517`) reports that `/readyz` was 200
# on all three replicas while Postgres was refusing every new connection — but
# the only reading behind that sentence was taken ~3 MINUTES AFTER the load
# window, because backend count and health status were sampled by two different
# ad-hoc commands at two different times. There is no in-window `/readyz`
# sample in the whole W6-1 archive, so what the health surface does DURING
# saturation is still unmeasured. `-p` closes that: the health probe runs
# inside the same loop iteration as the `pg_stat_activity` query, so every
# backend count carries the `/readyz` and `/healthz` codes read beside it and
# the pairing is a measurement rather than a juxtaposition of two captures.
#
# It needs `compose/ctrlports.override.yaml` (the direct controller ports); the
# probe deliberately does NOT go through the LB, for the same reason nothing
# else rate-bearing here does. `curl` gets `--max-time 2` so a hung controller
# cannot silently stretch the grid, and a probe that fails or times out is
# recorded as code `000` — never dropped, because "the controller stopped
# answering" is exactly the signal this option exists to catch.
#
# Usage:
#   w6-pgsample.sh [-i INTERVAL_S] [-d DURATION_S] [-o OUT.csv] [-r RAW.txt] [-l LABEL]
#                  [-p PORTS]
#     -p  comma-separated host ports to probe /readyz and /healthz on each
#         sample, e.g. -p 18081,18082,18083. Written to <OUT>.health.csv when
#         -o is given, and echoed on every console line.
# Env:
#   COMPOSE_FILES  compose args (default: -f docker-compose.ha.yaml). Run from test/ha/.
#   PGUSER_        psql user (default unified)
#   PGDB_          database  (default unified)
#   HEALTH_HOST    host for -p probes (default localhost)
#
# Example (60 s at 1 Hz while an SSE fan-out is held open):
#   cd test/ha
#   export COMPOSE_FILES="-f docker-compose.ha.yaml -f ../edgecase/compose/ctrlports.override.yaml"
#   ../edgecase/tools/w6/w6-pgsample.sh -i 1 -d 60 -o /path/pg.csv -l sse-10
#
# Example (connection pressure WITH the health surface, which is what W6-S1
# wants and what W6 Task 1 could not produce):
#   ../edgecase/tools/w6/w6-pgsample.sh -i 1 -d 60 -o /path/pg.csv \
#     -p 18081,18082,18083 -l saturation
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
ports=""
while getopts "i:d:o:r:l:p:h" opt; do
  case "${opt}" in
    i) interval="${OPTARG}" ;;
    d) duration="${OPTARG}" ;;
    o) out="${OPTARG}" ;;
    r) raw="${OPTARG}" ;;
    l) label="${OPTARG}" ;;
    p) ports="${OPTARG}" ;;
    *) sed -n '/^# Usage:/,/^set -euo/p' "$0" >&2; exit 2 ;;
  esac
done

COMPOSE_FILES="${COMPOSE_FILES:--f docker-compose.ha.yaml}"
PGUSER_="${PGUSER_:-unified}"
PGDB_="${PGDB_:-unified}"
HEALTH_HOST="${HEALTH_HOST:-localhost}"
health_out=""
IFS=',' read -r -a PORTS <<< "${ports}"
if [ -n "${ports}" ]; then
  command -v curl >/dev/null 2>&1 || { echo "w6-pgsample: -p needs curl on PATH" >&2; exit 2; }
  echo "w6-pgsample: health probe on ${HEALTH_HOST}:{${ports}} — /readyz and /healthz, same grid as the backend count"
fi

# probe_health echoes "PORT:readyz=CODE,healthz=CODE ..." for every -p port.
# A failed or timed-out request yields 000 rather than an empty field, so a
# controller that stops answering is a value in the series, not a gap in it.
probe_health() {
  local p code_r code_h
  for p in "${PORTS[@]}"; do
    [ -n "${p}" ] || continue
    # `|| true`, NOT `|| echo 000`: on a refused connection curl exits nonzero
    # but has ALREADY printed 000, so the fallback concatenated and the field
    # came out as `000000`. Measured, not reasoned — see the README row.
    code_r=$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "http://${HEALTH_HOST}:${p}/readyz" 2>/dev/null || true)
    code_h=$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "http://${HEALTH_HOST}:${p}/healthz" 2>/dev/null || true)
    printf '%s:readyz=%s,healthz=%s ' "${p}" "${code_r:-000}" "${code_h:-000}"
  done
}
dc() { MSYS_NO_PATHCONV=1 docker compose ${COMPOSE_FILES} "$@"; }
psql_() { dc exec -T postgres psql -U "${PGUSER_}" -d "${PGDB_}" -At -F',' -c "$1" | tr -d '\r'; }

# THE SAMPLER USED TO DIE OF THE THING IT MEASURES. `set -euo pipefail` plus a
# bare `rows=$(psql_ ...)` means that the first `FATAL: sorry, too many clients
# already` ends the script — so a capture aimed at Postgres exhaustion stopped
# at the sample BEFORE the exhaustion and produced nothing about it. Measured in
# W6-1: a 130 s grid returned 2 samples and exited when 40 SSE streams pinned
# `max_connections`, and the `-p` health series — the entire reason `-p` was
# added — stopped with it. `psql_ok` keeps the loop alive, records the count as
# `unavailable`, and lets the health probe (plain curl, unaffected by Postgres)
# carry on. A sampler that cannot outlive the fault is not an instrument.
psql_ok() {
  local v
  if v=$(psql_ "$1" 2>/dev/null); then printf '%s' "${v}"; else printf 'unavailable'; fi
}

# --- resolve container IPs to service names once, so client_addr is readable ---
declare -A IPNAME
while IFS=$'\t' read -r name ip; do
  [ -n "${ip}" ] && IPNAME["${ip}"]="${name}"
done < <(dc ps -q | while read -r cid; do
  [ -n "${cid}" ] || continue
  MSYS_NO_PATHCONV=1 docker inspect -f '{{index .Config.Labels "com.docker.compose.service"}}	{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${cid}"
done)

# The preamble reads these ONCE, and it is the second place the sampler used to
# die of the thing it measures: a run started while Postgres is ALREADY at
# `max_connections` never reached its own loop, so the state that most needs a
# health series was the one state in which none could be taken. Env overrides
# (`PG_MAXCONN_`/`PG_RESERVED_`) let a capture be started INTO an existing
# saturation with the two settings supplied from a healthier moment.
maxconn=$(psql_ok "select current_setting('max_connections')")
reserved=$(psql_ok "select current_setting('superuser_reserved_connections')")
[ "${maxconn}" = "unavailable" ] && maxconn="${PG_MAXCONN_:-unavailable}"
[ "${reserved}" = "unavailable" ] && reserved="${PG_RESERVED_:-3}"
if [ "${maxconn}" = "unavailable" ]; then
  echo "w6-pgsample: WARNING Postgres refused this script's own connection at startup." >&2
  echo "w6-pgsample:   That is itself the saturation signal. Sampling continues; backend" >&2
  echo "w6-pgsample:   counts will read 'unavailable' until it clears. Set PG_MAXCONN_ to" >&2
  echo "w6-pgsample:   get the budget comparison in the closing summary." >&2
fi
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
  # A SEPARATE file, deliberately: the health codes share the grid but not the
  # schema, and folding an HTTP status into the `count` column would poison the
  # peak table at the bottom of this script with "peak=200".
  if [ -n "${ports}" ]; then
    health_out="${out}.health.csv"
    printf 'sample,ts_utc,elapsed_s,label,port,readyz,healthz,total_backends,db_backends,max_connections\n' > "${health_out}"
  fi
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
  rows=$(psql_ok "${Q}")
  tot=$(psql_ok "${TOT}")
  dbtot=$(psql_ok "${DBTOT}")
  [ "${rows}" = "unavailable" ] && rows=""
  # Probed in the SAME iteration as the counts above, which is the whole point:
  # a health code and a backend count from two different commands minutes apart
  # is what left the W6-infra entry unable to say anything about the health
  # surface in-window.
  health=""
  [ -n "${ports}" ] && health=$(probe_health)
  if [ -n "${raw}" ]; then
    { echo "== sample ${i} ${ts} elapsed=${elapsed}s total_backends=${tot} db_backends=${dbtot}${health:+ health: ${health}}"; printf '%s\n' "${rows}"; } >> "${raw}"
  fi
  printf '%s sample=%-4d total_backends=%-4s db_backends=%-4s of max_connections=%s%s\n' "${ts}" "${i}" "${tot}" "${dbtot}" "${maxconn}" "${health:+  ${health}}"
  if [ -n "${health_out}" ]; then
    for hp in ${health}; do
      port="${hp%%:*}"; rest="${hp#*:}"
      rz="${rest%%,*}"; rz="${rz#readyz=}"
      hz="${rest#*,}"; hz="${hz#healthz=}"
      printf '%d,%s,%d,%s,%s,%s,%s,%s,%s,%s\n' "${i}" "${ts}" "${elapsed}" "${label}" "${port}" "${rz}" "${hz}" "${tot}" "${dbtot}" "${maxconn}" >> "${health_out}"
    done
  fi
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
# `unavailable` rows are samples in which Postgres refused this script's own
# connection. They are COUNTED and reported, never parsed as a number and never
# dropped: a window in which the sampler could not connect is the window the
# reader most needs told about, and silently averaging over the samples that
# happened to succeed would understate the peak.
unavail = 0
with open(sys.argv[1]) as f:
    for r in csv.DictReader(f):
        if not r["count"].lstrip("-").isdigit():
            unavail += 1
            continue
        if r["service"] == "TOTAL":
            tot.append(int(r["count"]))
            continue
        peak[(r["service"], r["pool_derived"], r["state"])] = max(
            peak[(r["service"], r["pool_derived"], r["state"])], int(r["count"]))
for k in sorted(peak):
    print("  %-14s %-8s %-22s peak=%d" % (k[0], k[1], k[2], peak[k]))
if tot:
    print("  TOTAL backends: min=%d max=%d mean=%.1f over %d samples" % (min(tot), max(tot), sum(tot)/len(tot), len(tot)))
if unavail:
    print("  UNAVAILABLE: %d sample rows in which Postgres refused this sampler's own"
          " connection ('too many clients already'). That is a saturation reading, not a gap."
          % unavail)
PY
fi

if [ -n "${health_out}" ]; then
  echo "--- /readyz on the same grid (in-window; this is the pairing W6-infra could not make) ---"
  python - "${health_out}" "${reserved}" <<'PY'
import csv, sys, collections
rows = list(csv.DictReader(open(sys.argv[1])))
reserved = int(sys.argv[2]) if len(sys.argv) > 2 and sys.argv[2].isdigit() else 3
if not rows:
    print("  no health samples")
    raise SystemExit
per = collections.defaultdict(list)
for r in rows:
    per[r["port"]].append(r)
for port in sorted(per):
    rs = per[port]
    codes = collections.Counter(r["readyz"] for r in rs)
    hcodes = collections.Counter(r["healthz"] for r in rs)
    print("  port %s  readyz %s   healthz %s   over %d samples" % (
        port,
        " ".join("%s=%d" % kv for kv in sorted(codes.items())),
        " ".join("%s=%d" % kv for kv in sorted(hcodes.items())),
        len(rs)))
# The specific question: did the health surface report anything while the
# server was at or near its connection ceiling? Answered per sample, not by
# eye, and stated in BOTH directions so a passing readyz is as visible as a
# failing one.
#
# IT COMPARES CLIENT BACKENDS, NOT `total_backends`. `pg_stat_activity` includes
# Postgres's own background workers, which carry a NULL `datname` and consume no
# `max_connections` slot, so a `total_backends` test over-reads saturation — the
# caveat `README.md` records, and this summary was on the wrong side of it:
# W6-1's 40-stream sweep printed "AT/NEAR max_connections in 156 of 180 samples"
# from `total_backends=98` while client backends were 93 of a 97-slot budget.
# The honest comparison is db_backends against max_connections - reserved.
# `unavailable` is the STRONGEST saturation reading there is — psql itself was
# refused — so it counts as saturated rather than being skipped.
def saturated(r):
    d = r["db_backends"]
    if d == "unavailable":
        return True
    return (r["max_connections"].isdigit() and d.isdigit()
            and int(d) >= int(r["max_connections"]) - reserved)
sat = [r for r in rows if saturated(r)]
if sat:
    ok = sum(1 for r in sat if r["readyz"] == "200")
    print("  AT/NEAR the client-backend budget (max_connections - superuser_reserved=%d)"
          " in %d of %d samples:" % (reserved, len(sat), len(rows)))
    # ASCII only in this print: the console encoding on the machine this was
    # built on is cp932, and an em dash here raised UnicodeEncodeError and
    # killed the summary after the per-port table.
    print("    readyz still 200 in %d of those %d - the health surface %s the degradation."
          % (ok, len(sat), "did NOT report" if ok else "reported"))
else:
    print("  no sample reached the client-backend budget; this window says nothing about the saturated case.")
PY
fi
