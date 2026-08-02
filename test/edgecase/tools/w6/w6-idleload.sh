#!/usr/bin/env bash
# Measure the stack's IDLE query floor: turn on Postgres statement logging,
# capture a window during which nothing is running, and report queries/second
# in total, per replica, and per statement class.
#
# WHY EVERY W6 NUMBER NEEDS THIS. `FINDINGS.md:515` and `:563` establish that
# the background jobs already cost measurable query volume with nothing
# running: ten of the eleven "leader-elected" jobs are per-tick mutexes that
# execute up to N times per nominal interval on N replicas (2.15-2.40x measured
# on 3), three jobs have no lock at all, and the git resolver alone costs
# **5.0 queries/s per replica** — 15/s across the rig — because a 200 ms ticker
# (`cmd/controller/main.go:444`) queries before it knows there is anything to
# do. A W6 measurement that does not subtract this floor attributes the rig's
# resting metabolism to the workload under test.
#
# ARMING DISCIPLINE (the campaign has been bitten by every one of these):
#   * ONE `ALTER SYSTEM` PER `psql -c`. Two statements in one `-c` is an
#     implicit transaction, and `ALTER SYSTEM` is refused inside one —
#     **silently**, while `pg_reload_conf()` still returns `t`. This script
#     issues each on its own connection and then reads the settings back in a
#     FRESH session, because a session that predates the reload keeps the old
#     values and would report success either way.
#   * IT REVERTS ON A TRAP, BUT THE REVERT IS NOT UNCONDITIONAL — it needs a
#     Postgres connection. `ALTER SYSTEM RESET` for both settings, a reload and
#     a fresh-session read-back run on a trap so they happen even on interrupt,
#     BUT if the window you measured exhausted `max_connections` the revert's
#     own `psql` is refused, the script exits non-zero and LEAVES
#     `log_statement='all'` ARMED on the running cluster. W6-3 hit exactly this
#     (exit 2, `FATAL: sorry, too many clients already`) and had to revert by
#     hand. Until that is fixed: after any saturating arm, verify the settings
#     are back before starting the next one. `log_statement='all'` left on will
#     fill the container's log driver. See the W6-3 note in `README.md`.
#
# Usage:
#   w6-idleload.sh [-d SECONDS] [-o OUTDIR] [-l LABEL]
# Env:
#   COMPOSE_FILES  compose args (default -f docker-compose.ha.yaml). Run from test/ha/.
#
# GIT BASH: MSYS_NO_PATHCONV=1 is set per docker call, not exported.
set -euo pipefail

dur=300
outdir="."
label="idle"
while getopts "d:o:l:h" o; do
  case "${o}" in
    d) dur="${OPTARG}" ;;
    o) outdir="${OPTARG}" ;;
    l) label="${OPTARG}" ;;
    *) sed -n '/^# Usage:/,/^set -euo/p' "$0" >&2; exit 2 ;;
  esac
done

COMPOSE_FILES="${COMPOSE_FILES:--f docker-compose.ha.yaml}"
dc() { MSYS_NO_PATHCONV=1 docker compose ${COMPOSE_FILES} "$@"; }
# Each call is its own psql process, hence its own session — which is exactly
# what the read-back needs.
psq() { dc exec -T postgres psql -U unified -d unified -At -c "$1" | tr -d '\r'; }

mkdir -p "${outdir}"
raw="${outdir}/w6-idleload-${label}-statements.log"
rep="${outdir}/w6-idleload-${label}-report.txt"

reverted=0
revert() {
  [ "${reverted}" = "1" ] && return 0
  reverted=1
  echo "== revert =="
  psq "ALTER SYSTEM RESET log_statement" >/dev/null
  psq "ALTER SYSTEM RESET log_line_prefix" >/dev/null
  psq "SELECT pg_reload_conf()" >/dev/null
  sleep 1
  echo "   fresh session now reads: log_statement=$(psq 'SHOW log_statement') log_line_prefix=$(psq 'SHOW log_line_prefix')"
}
trap revert EXIT INT TERM

echo "== before (fresh session) =="
echo "   log_statement=$(psq 'SHOW log_statement') log_line_prefix=$(psq 'SHOW log_line_prefix')"

echo "== arm =="
psq "ALTER SYSTEM SET log_statement = 'all'" >/dev/null
psq "ALTER SYSTEM SET log_line_prefix = '%m [%p] host=%h '" >/dev/null
psq "SELECT pg_reload_conf()" >/dev/null
sleep 1
armed_stmt=$(psq 'SHOW log_statement')
armed_pref=$(psq 'SHOW log_line_prefix')
echo "   fresh session now reads: log_statement=${armed_stmt} log_line_prefix=${armed_pref}"
if [ "${armed_stmt}" != "all" ]; then
  echo "w6-idleload: ARM FAILED — log_statement is '${armed_stmt}', not 'all'. Aborting rather than" >&2
  echo "             producing a capture that would silently under-count." >&2
  exit 4
fi
case "${armed_pref}" in *host=*) ;; *) echo "w6-idleload: ARM FAILED — log_line_prefix has no host= field; per-replica attribution is impossible" >&2; exit 4 ;; esac

# --- map container IPs to compose service names, for per-replica attribution ---
mapfile=$(mktemp)
dc ps -q | while read -r cid; do
  [ -n "${cid}" ] || continue
  MSYS_NO_PATHCONV=1 docker inspect -f '{{index .Config.Labels "com.docker.compose.service"}} {{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${cid}"
done > "${mapfile}"
echo "== container IPs =="
sed 's/^/   /' "${mapfile}"

# THE CAPTURE IS A BOUNDED PULL, NOT A FOLLOW — and that is a bug fix, not a
# style preference. This step used to be
#
#     dc logs -f --no-log-prefix --since 0s postgres > "${raw}" & lpid=$!
#     ... ; kill "${lpid}"
#
# and the kill DID NOT STOP THE CAPTURE. `dc` is a shell function, so `$!` is
# the subshell running it; the process actually holding the pipe is the
# `docker-compose` CLI PLUGIN two levels down, and it survives. W6-2a measured
# the consequence: the `floor` capture read 10948 statements when its own
# analyser ran and 26523 when re-read after the next arm — three plugin
# processes were still writing into three "finished" files, and every one of
# them was silently accumulating later arms. A capture that keeps growing after
# its window is worse than no capture, because it still analyses.
#
# `docker compose logs --since T --until T` takes RFC3339 and returns the same
# bytes with no background process at all, so there is nothing left to leak.
# The window is also written into the report, so a re-analysis of an old file
# can bound itself.
echo "== capture ${dur}s (LEAVE THE STACK ALONE — this window IS the baseline) =="
t0=$(date -u +%FT%T.%3NZ)
sleep "${dur}"
t1=$(date -u +%FT%T.%3NZ)
dc logs --no-log-prefix --since "${t0}" --until "${t1}" postgres > "${raw}" 2>&1
echo "   window ${t0} .. ${t1}; raw -> ${raw} ($(wc -l < "${raw}") lines)"
printf '%s\n%s\n' "${t0}" "${t1}" > "${outdir}/w6-idleload-${label}-window.txt"

revert

echo "== analysis =="
# The analyser lives in its own file so it can be re-run against an ALREADY
# CAPTURED log; the first version of it under-counted by 28x and re-capturing
# would have cost another untouched five minutes of stack time.
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PYTHONIOENCODING=utf-8 python "${here}/w6-idleanalyze.py" "${raw}" "${mapfile}" "${dur}" "${label}" | tee "${rep}"
cp "${mapfile}" "${outdir}/w6-idleload-${label}-ipmap.txt"
echo "== done: report -> ${rep}; re-analyse with:"
echo "   python test/edgecase/tools/w6/w6-idleanalyze.py ${raw} ${outdir}/w6-idleload-${label}-ipmap.txt ${dur} ${label}"
