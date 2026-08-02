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
#   * IT ALWAYS REVERTS. `ALTER SYSTEM RESET` for both settings, a reload, and
#     a fresh-session read-back, on a trap so it happens even on interrupt.
#     `log_statement='all'` left on will fill the container's log driver.
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

echo "== capture ${dur}s (LEAVE THE STACK ALONE — this window IS the baseline) =="
dc logs -f --no-log-prefix --since 0s postgres > "${raw}" 2>&1 &
lpid=$!
t0=$(date -u +%FT%T.%3NZ)
sleep "${dur}"
t1=$(date -u +%FT%T.%3NZ)
kill "${lpid}" 2>/dev/null || true
wait "${lpid}" 2>/dev/null || true
echo "   window ${t0} .. ${t1}; raw -> ${raw} ($(wc -l < "${raw}") lines)"

revert

echo "== analysis =="
python - "${raw}" "${mapfile}" "${dur}" "${label}" <<'PY' | tee "${rep}"
import re, sys, collections
raw, mapfile, dur, label = sys.argv[1], sys.argv[2], float(sys.argv[3]), sys.argv[4]

ip2svc = {}
for line in open(mapfile):
    p = line.split()
    if len(p) == 2:
        ip2svc[p[1]] = p[0]

# %m [%p] host=%h  LOG:  statement: <sql>
LINE = re.compile(r'^(?P<ts>\d{4}-\d\d-\d\d \d\d:\d\d:\d\d\.\d+ \S+) \[(?P<pid>\d+)\] host=(?P<host>\S*)\s+LOG:\s+statement:\s+(?P<sql>.*)$')

per_host = collections.Counter()
per_stmt = collections.Counter()
per_host_stmt = collections.Counter()
first = last = None
n = 0
buf_sql = None
buf_key = None

def norm(sql):
    s = ' '.join(sql.split())
    s = re.sub(r"'[^']*'", "'?'", s)
    s = re.sub(r'\$\d+', '$?', s)
    return s[:90]

for line in open(raw, errors='replace'):
    line = line.rstrip('\r\n')
    m = LINE.match(line)
    if not m:
        continue
    n += 1
    ts = m.group('ts')
    if first is None:
        first = ts
    last = ts
    host = m.group('host') or '(local)'
    svc = ip2svc.get(host, host)
    key = norm(m.group('sql'))
    per_host[svc] += 1
    per_stmt[key] += 1
    per_host_stmt[(svc, key)] += 1

print("w6-idleload report  label=%s" % label)
print("  window          %s .. %s   (nominal %.0f s)" % (first, last, dur))
print("  statements      %d   => %.2f queries/s across the stack" % (n, n / dur))
print()
print("  --- per replica ---")
for svc, c in per_host.most_common():
    print("  %-14s %7d   %6.2f q/s" % (svc, c, c / dur))
print()
print("  --- per statement class (top 20) ---")
for k, c in per_stmt.most_common(20):
    print("  %7d  %6.2f/s  %s" % (c, c / dur, k))
print()
print("  --- ListPendingRuns (the git resolver's only statement; FINDINGS.md:563 measured 5.006 q/s per replica) ---")
hit = [(k, c) for k, c in per_stmt.items() if k.startswith('SELECT id, spec, created_at FROM runs')]
if not hit:
    print("  (no match — check the statement text against internal/controller/scheduler.go)")
for k, c in hit:
    print("  total %d  %.3f q/s across the stack" % (c, c / dur))
    for svc in sorted(per_host):
        cc = per_host_stmt.get((svc, k), 0)
        if cc:
            print("    %-14s %6d  %.3f q/s" % (svc, cc, cc / dur))
print()
print("  --- advisory-lock traffic (the per-tick 'leader election', FINDINGS.md:515) ---")
adv = [(k, c) for k, c in per_stmt.items() if 'advisory' in k]
for k, c in sorted(adv, key=lambda x: -x[1]):
    print("  %7d  %6.2f/s  %s" % (c, c / dur, k))
PY
echo "== done: report -> ${rep} =="
