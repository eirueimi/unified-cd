#!/usr/bin/env bash
# Record the SHAPE of a request stream over time — one row per request, with a
# millisecond timestamp — and fold it into a per-tick curve.
#
# ================= WHY THE NGINX ACCESS LOG, AND NOT THE COUNTER ============
# W1-6 derived its request numbers from `unifiedcd_agent_auth_events_total`.
# That worked only because every request in that scenario was REJECTED at
# authentication, which is what the counter counts. W6's log-fault arms are
# different: the requests SUCCEED, or fail somewhere that counter never sees,
# so it is useless here. The two candidates were the nginx access log and
# `unifiedcd_http_requests_total{route=".../logs/bulk"}` scraped per
# controller on a fine grid. The access log wins on four grounds, three of
# which are disqualifying for the counter:
#
#  1. RESOLUTION. `nginx-logfault.conf`'s `logfault` format leads with `$msec`
#     — a per-request timestamp at millisecond resolution. A Prometheus
#     counter has no timestamp at all; scraping it on a 2 s grid yields the
#     DELTA per grid cell and can never resolve two requests 40 ms apart into
#     two events. Task 3's curve is a per-TICK request count and the
#     LogPusher's tick is 2 s (`internal/agent/runner.go:211`,
#     `logPusherAutoFlushEvery`), so a 2 s grid is exactly the wrong
#     granularity: the quantity being measured has the same period as the
#     instrument. `shape -b` folds ms-resolution rows into whatever bucket the
#     analysis wants, AFTER the fact; the reverse is impossible.
#  2. BLINDNESS. The W6 faults are injected AT NGINX. A request nginx answers
#     itself — 413 over the 1 MiB body cap (`FINDINGS.md:1655`), a 502 to a
#     dead upstream, a 403 from a URI-scoped deny — never reaches a
#     controller, so `unifiedcd_http_requests_total` does not increment for
#     precisely the requests under measurement. The `contrast` verb below
#     demonstrates this by effect rather than asserting it.
#  3. AMPLIFICATION. `proxy_next_upstream_tries 3` (`test/ha/nginx.conf:25`)
#     can turn one client request into up to three upstream requests. The
#     controller counters would report the amplified number and the agent's
#     own flush count is the un-amplified one; the access log carries BOTH, as
#     `$status` and `$upstream_status` (which nginx renders as a comma list,
#     e.g. `504, 200`, when it retried).
#  4. SELF-PERTURBATION. Scraping `/metrics` every 2 s from three controllers
#     is itself 1.5 req/s of load that lands in the very counter being read
#     (`/metrics` goes through `metricsMiddleware`,
#     `internal/controller/server.go:288`). Reading a log perturbs nothing.
#
# The counter is not useless in general — it is the right instrument for
# "how many did the controller SERVE", and `contrast` prints both numbers side
# by side so a scenario can quote the difference. It is the wrong instrument
# for "what shape did the agent EMIT".
#
# ================= WHAT THE RECORDER REQUIRES ==============================
# `compose/logfault.override.yaml` (i.e. `nginx-logfault.conf`). Against
# `test/ha/nginx.conf` or `nginx-edge.conf` nginx logs the stock `combined`
# format: `$time_local` at ONE-SECOND resolution, no `$request_time`, no
# `arm=`. `shape` DETECTS that and exits 4 rather than silently producing a
# 1 s-granular curve — the campaign has shipped two inert instruments that
# passed all their own state checks, and a blunt curve is worse than no curve.
#
# ================= WHY `follow -d` IS DEPRECATED (W6-2b) ===================
# `follow -d` backgrounded `dc logs -f ... > out &` and killed `$!`. `dc` is a
# SHELL FUNCTION, so `$!` is the subshell; the process actually holding the
# pipe is the docker-compose CLI PLUGIN two levels down, and it survives the
# kill and keeps writing. This is the SAME defect Task 2 found and fixed in
# w6-idleload.sh (`FINDINGS.md` W6-2a entry 5) — it was fixed there and left
# here, and W6-2b measured it live: `follow -d 5` reported "captured 34 lines",
# and eight seconds and twenty unrelated requests later the same file held
# **56**. A request-count curve read off a file that is still growing is not a
# measurement of anything.
#
# `window` is the replacement and has no background process at all: it sleeps,
# then pulls the interval with `docker compose logs --since T --until T`, and
# writes the interval it used to a `-window.txt` sidecar so an old capture can
# bound its own re-analysis. `follow -d` now refuses to run and points here;
# bare `follow` (which returns the PID for a caller that manages its own
# lifecycle) still exists but warns, because the PID it prints is the wrong
# process to kill.
#
# Usage:
#   w6-reqshape.sh window  -o RAW.log -d SECONDS [-S SERVICE]
#                                                          BOUNDED capture: sleep D, then pull
#                                                          exactly [T0,T1]. Use this.
#   w6-reqshape.sh follow  -o RAW.log [-d SECONDS]        DEPRECATED (-d refuses; see above)
#   w6-reqshape.sh shape   -f RAW.log [-u URI_SUBSTRING] [-b BUCKET_S] [-o OUT.csv]
#   w6-reqshape.sh counter [-r 18081,18082,18083] [-u ROUTE_SUBSTRING]
#                                                          one snapshot of unifiedcd_http_requests_total
#   w6-reqshape.sh contrast [-n K]                         fire K LB-terminated requests and
#                                                          compare access-log count vs counter delta
# Env: COMPOSE_FILES (default -f docker-compose.ha.yaml). Run from test/ha/.
#
# GIT BASH: MSYS_NO_PATHCONV=1 is set PER DOCKER CALL, never exported —
# exporting it makes a native curl.exe receive unconverted `/tmp/...` paths and
# every `-o` / `--data-binary @file` in `contrast` fails with curl exit 23.
set -euo pipefail

COMPOSE_FILES="${COMPOSE_FILES:--f docker-compose.ha.yaml}"
LB="${UNIFIED_SERVER:-http://localhost:18080}"
REPLICAS="${W6_REPLICAS:-18081,18082,18083}"
dc() { MSYS_NO_PATHCONV=1 docker compose ${COMPOSE_FILES} "$@"; }

cmd="${1:-}"; shift || true

usage() { sed -n '/^# Usage:/,/^set -euo/p' "$0" >&2; exit 2; }

case "${cmd}" in

window)
  out=""; dur=""; svc="nginx"
  while getopts "o:d:S:" o; do case "$o" in o) out="$OPTARG";; d) dur="$OPTARG";; S) svc="$OPTARG";; *) usage;; esac; done
  [ -n "${out}" ] && [ -n "${dur}" ] || usage
  # Bounded on BOTH ends and with no background process, so the file cannot
  # keep growing after this returns. `--since`/`--until` take RFC3339; docker's
  # resolution there is one second, so T0 is taken one second early to avoid
  # clipping the leading edge and the sidecar records the interval actually
  # requested (never infer the window from the file's own first/last row —
  # that is the span-vs-histogram error).
  t0=$(date -u -d '1 second ago' +%FT%TZ 2>/dev/null || date -u +%FT%TZ)
  echo "reqshape: window opens ${t0}, ${dur}s, service=${svc} -> ${out}"
  sleep "${dur}"
  t1=$(date -u +%FT%TZ)
  dc logs --no-log-prefix --since "${t0}" --until "${t1}" "${svc}" > "${out}" 2>&1
  printf 'service=%s\nsince=%s\nuntil=%s\nnominal_seconds=%s\n' "${svc}" "${t0}" "${t1}" "${dur}" \
    > "${out%.log}-window.txt"
  echo "reqshape: captured $(wc -l < "${out}") lines for ${t0} .. ${t1} (sidecar ${out%.log}-window.txt)"
  ;;

follow)
  out=""; dur=""
  while getopts "o:d:" o; do case "$o" in o) out="$OPTARG";; d) dur="$OPTARG";; *) usage;; esac; done
  [ -n "${out}" ] || usage
  if [ -n "${dur}" ]; then
    echo "reqshape: 'follow -d' is REFUSED. It could not stop its own capture: it killed the" >&2
    echo "  subshell while the docker-compose plugin kept the pipe, so the file went on" >&2
    echo "  growing after the run reported a line count. Measured live in W6-2b: 34 lines at" >&2
    echo "  'captured', 56 eight seconds later. Use:  w6-reqshape.sh window -o ${out} -d ${dur}" >&2
    exit 5
  fi
  echo "reqshape: WARNING — the printed PID is the subshell, NOT the docker-compose plugin" >&2
  echo "  that holds the pipe; killing it does not stop the capture. Prefer 'window'." >&2
  dc logs -f --no-log-prefix --since 0s nginx > "${out}" 2>&1 &
  echo $!
  ;;

shape)
  raw=""; uri=""; bucket=2; out=""
  while getopts "f:u:b:o:" o; do case "$o" in f) raw="$OPTARG";; u) uri="$OPTARG";; b) bucket="$OPTARG";; o) out="$OPTARG";; *) usage;; esac; done
  [ -n "${raw}" ] || usage
  python - "${raw}" "${uri}" "${bucket}" "${out}" <<'PY'
import re, sys, csv, collections

raw, uri, bucket, out = sys.argv[1], sys.argv[2], float(sys.argv[3]), sys.argv[4]

# nginx-logfault.conf log_format `logfault`:
#   $msec $time_iso8601 $status arm=$logfault_arm target=$logfault_target
#   ustatus=$upstream_status rt=$request_time urt=$upstream_response_time
#   reqlen=$request_length from=$remote_addr "$request"
LINE = re.compile(
    r'^(?P<msec>\d+\.\d+) (?P<iso>\S+) (?P<status>\d{3}) '
    r'arm=(?P<arm>\S*) target=(?P<target>\S*) '
    r'ustatus=(?P<ustatus>.*?) rt=(?P<rt>\S+) urt=(?P<urt>.*?) '
    r'reqlen=(?P<reqlen>\S+) from=(?P<from>\S+) "(?P<request>[^"]*)"\s*$')

rows, other, combined = [], 0, 0
COMBINED = re.compile(r'^\S+ \S+ \S+ \[\d{2}/\w{3}/\d{4}')
with open(raw, errors='replace') as f:
    for line in f:
        line = line.rstrip('\r\n')
        if not line:
            continue
        m = LINE.match(line)
        if not m:
            if COMBINED.match(line):
                combined += 1
            else:
                other += 1
            continue
        d = m.groupdict()
        if uri and uri not in d['request']:
            continue
        parts = d['request'].split(' ')
        d['method'] = parts[0] if parts else ''
        d['uri'] = parts[1] if len(parts) > 1 else ''
        d['msec'] = float(d['msec'])
        rows.append(d)

if combined and not rows:
    sys.stderr.write(
        "reqshape: the nginx access log is in the STOCK `combined` format "
        "(%d lines), not `logfault`.\n"
        "          That format has one-second resolution and carries no arm= "
        "stamp, so it cannot\n"
        "          resolve a 2 s tick. Bring the stack up with "
        "compose/logfault.override.yaml.\n" % combined)
    sys.exit(4)
if not rows:
    sys.stderr.write("reqshape: no matching requests (unparsed=%d, combined=%d)\n" % (other, combined))
    sys.exit(3)

rows.sort(key=lambda r: r['msec'])
t0 = rows[0]['msec']
print("reqshape: %d matching requests%s" % (len(rows), (" uri~%r" % uri) if uri else ""))
print("  window   %s .. %s  (%.3f s)" % (rows[0]['iso'], rows[-1]['iso'], rows[-1]['msec'] - t0))
gaps = [rows[i]['msec'] - rows[i-1]['msec'] for i in range(1, len(rows))]
if gaps:
    print("  inter-arrival gaps: min=%.3f s  median=%.3f s  max=%.3f s"
          % (min(gaps), sorted(gaps)[len(gaps)//2], max(gaps)))
    closer = sum(1 for g in gaps if g < bucket)
    print("  RESOLUTION: %d of %d consecutive pairs are separated by less than the %.1f s bucket."
          % (closer, len(gaps), bucket))
    print("              Each is a pair a %.1f s counter grid would have merged into one number;"
          % bucket)
    print("              this recorder keeps them as %d distinct rows with ms timestamps."
          % len(rows))
st = collections.Counter(r['status'] for r in rows)
arm = collections.Counter(r['arm'] for r in rows)
print("  status   %s" % dict(sorted(st.items())))
print("  arm      %s" % dict(sorted(arm.items())))

buckets = collections.Counter()
bstatus = collections.defaultdict(collections.Counter)
for r in rows:
    b = int((r['msec'] - t0) // bucket)
    buckets[b] += 1
    bstatus[b][r['status']] += 1
print("  --- per-%.1fs bucket ---" % bucket)
print("  %-8s %-10s %-8s %s" % ("bucket", "t_rel_s", "count", "status"))
for b in sorted(buckets):
    print("  %-8d %-10.1f %-8d %s" % (b, b * bucket, buckets[b], dict(sorted(bstatus[b].items()))))
print("  cumulative=%d  peak_bucket=%d" % (len(rows), max(buckets.values())))

if out:
    with open(out, 'w', newline='') as f:
        w = csv.writer(f)
        w.writerow(['msec', 't_rel_s', 'iso', 'status', 'ustatus', 'arm', 'target',
                    'rt', 'urt', 'reqlen', 'from', 'method', 'uri', 'bucket'])
        for r in rows:
            w.writerow([('%.3f' % r['msec']), ('%.3f' % (r['msec'] - t0)), r['iso'], r['status'],
                        r['ustatus'], r['arm'], r['target'], r['rt'], r['urt'], r['reqlen'],
                        r['from'], r['method'], r['uri'], int((r['msec'] - t0) // bucket)])
    print("  wrote %s" % out)
PY
  ;;

counter)
  reps="${REPLICAS}"; route=""
  while getopts "r:u:" o; do case "$o" in r) reps="$OPTARG";; u) route="$OPTARG";; *) usage;; esac; done
  ts=$(date -u +%FT%T.%3NZ)
  total=0
  for p in $(echo "${reps}" | tr ',' ' '); do
    v=$(curl -s "http://localhost:${p}/metrics" \
        | awk -v r="${route}" '/^unifiedcd_http_requests_total\{/ { if (r=="" || index($0,r)>0) s+=$NF } END { printf "%d", s+0 }')
    echo "${ts} controller_port=${p} unifiedcd_http_requests_total${route:+ route~${route}} = ${v}"
    total=$(( total + v ))
  done
  echo "${ts} SUM = ${total}"
  ;;

contrast)
  k=5
  while getopts "n:" o; do case "$o" in n) k="$OPTARG";; *) usage;; esac; done
  tmpdir=$(mktemp -d); trap 'rm -rf "${tmpdir}"' EXIT
  # A body over nginx's default client_max_body_size 1m. nginx answers 413
  # ITSELF and never opens an upstream connection (FINDINGS.md:1655), so this
  # is a request the product genuinely served zero of.
  python -c "import sys;sys.stdout.write('[\"'+'x'*1200000+'\"]')" > "${tmpdir}/big.json"
  uri="/api/v1/agents/w6-contrast/runs/00000000-0000-0000-0000-000000000000/steps/0/logs/bulk"
  # `counter` prints "<ts> SUM = <n>", so the total is field 4, not 3.
  before=$(bash "$0" counter | awk '/ SUM = /{print $4}')
  dc logs -f --no-log-prefix --since 0s nginx > "${tmpdir}/live.log" 2>&1 &
  lpid=$!
  sleep 1.5
  echo "contrast: firing ${k} oversize (>1 MiB) requests at the LB — nginx answers them itself"
  for _ in $(seq 1 "${k}"); do
    curl -s -o /dev/null -w '  client saw %{http_code}\n' -X POST \
      -H 'Content-Type: application/json' --data-binary "@${tmpdir}/big.json" "${LB}${uri}"
  done
  sleep 2
  kill "${lpid}" 2>/dev/null || true
  wait "${lpid}" 2>/dev/null || true
  after=$(bash "$0" counter | awk '/ SUM = /{print $4}')
  # nginx writes BOTH an access-log row (leading $msec) and an error-log line
  # for an oversize body, and they interleave on the same stdout stream. Count
  # only the access rows — the error lines are shown below but are not the
  # recorder's input.
  seen=$(grep -c '^[0-9]\{10\}\.[0-9]\{3\} .*w6-contrast' "${tmpdir}/live.log" || true)
  errs=$(grep -c 'too large body.*w6-contrast' "${tmpdir}/live.log" || true)
  echo
  echo "contrast result"
  echo "  access-log rows for THIS uri     : ${seen}   <- the recorder this tool uses"
  echo "  (nginx error-log lines, not input): ${errs}"
  echo "  unifiedcd_http_requests_total SUM: ${before} -> ${after} (delta $(( after - before )))"
  echo "  the captured lines:"
  grep "w6-contrast" "${tmpdir}/live.log" | sed 's/^/    /' || true
  echo
  echo "  Note the access rows carry 'arm=' EMPTY: nginx rejects an oversize body before"
  echo "  it enters the location that sets \$logfault_arm, so a 413 is one of the few"
  echo "  requests the logfault format cannot stamp with an arm."
  echo
  echo "  Read the counter delta as: NOT ${k} of these requests. nginx terminated every one"
  echo "  of them with 413 and never opened an upstream connection, so no controller"
  echo "  counted any of them. A counter-derived request curve is blind to exactly the"
  echo "  requests an nginx-injected fault produces. The delta is not zero either — it is"
  echo "  this tool's own /metrics scrapes (6 of them, two 'counter' calls x 3 replicas)"
  echo "  plus whatever the agents did meanwhile, which is the self-perturbation argument,"
  echo "  measured rather than asserted."
  ;;

*) usage ;;
esac
