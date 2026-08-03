# Analyse a Postgres statement-log capture taken by w6-idleload.sh.
#
# Kept as a separate file rather than a heredoc inside the shell script for one
# reason: it can be re-run against an ALREADY CAPTURED log. The first version of
# this analyser under-counted a 300 s window by 28x (see the STMT comment), and
# re-capturing would have cost another five minutes of a stack that must be
# untouched while it records. An analyser that can be fixed and re-run against
# the raw evidence is worth more than one that is welded to the capture.
#
#   python w6-idleanalyze.py RAW.log IPMAP.txt DURATION_SECONDS LABEL
#
# IPMAP.txt is "<service> <ip>" per line, as w6-idleload.sh writes it from
# `docker inspect`.
import re
import sys
import collections

raw, mapfile, dur, label = sys.argv[1], sys.argv[2], float(sys.argv[3]), sys.argv[4]

ip2svc = {}
for line in open(mapfile):
    p = line.split()
    if len(p) == 2:
        ip2svc[p[1]] = p[0]

# A record starts with the log_line_prefix w6-idleload.sh arms
# ('%m [%p] host=%h '); SQL text wraps onto unprefixed continuation lines, so
# records are ACCUMULATED rather than matched line by line.
PREFIX = re.compile(
    r'^(?P<ts>\d{4}-\d\d-\d\d \d\d:\d\d:\d\d\.\d+ \S+) \[(?P<pid>\d+)\] host=(?P<host>\S*)\s+(?P<rest>.*)$')

# BOTH forms are required. pgx uses the EXTENDED query protocol with a
# statement cache, so nearly all controller traffic is logged as
# `LOG:  execute stmtcache_<hash>: <sql>`, NOT as `LOG:  statement: <sql>`.
# Matching only the simple form under-counted a 300 s idle window by 28x — 784
# records instead of 21633 — while looking entirely plausible: it still printed
# a report, a per-replica breakdown and a rate. Confirmed against the raw
# capture with `grep -oE 'LOG:  [a-z]+ [^:]*:' RAW | sort | uniq -c`.
# `DETAIL:  parameters:` records carry their own prefix and simply do not match.
STMT = re.compile(r'^LOG:\s+(?:statement:|execute [^:]*:)\s+(?P<sql>.*)$', re.S)

per_host = collections.Counter()
per_stmt = collections.Counter()
per_host_stmt = collections.Counter()
first = last = None
n = 0


def norm(sql):
    s = ' '.join(sql.split())
    s = re.sub(r"'[^']*'", "'?'", s)
    s = re.sub(r'\$\d+', '$?', s)
    return s[:100]


def emit(ts, host, rest):
    global n, first, last
    m = STMT.match(rest)
    if not m:
        return
    n += 1
    if first is None:
        first = ts
    last = ts
    svc = ip2svc.get(host, host or '(local)')
    key = norm(m.group('sql'))
    per_host[svc] += 1
    per_stmt[key] += 1
    per_host_stmt[(svc, key)] += 1


cur = None
for line in open(raw, errors='replace'):
    line = line.rstrip('\r\n')
    m = PREFIX.match(line)
    if m:
        if cur:
            emit(*cur)
        cur = [m.group('ts'), m.group('host'), m.group('rest')]
    elif cur is not None:
        cur[2] += '\n' + line
if cur:
    emit(*cur)

print("w6-idleload report  label=%s" % label)
print("  window          %s .. %s   (nominal %.0f s)" % (first, last, dur))
print("  statements      %d   => %.2f queries/s across the stack" % (n, n / dur))
print()
print("  --- per replica ---")
for svc, c in per_host.most_common():
    print("  %-14s %7d   %6.2f q/s" % (svc, c, c / dur))
print()
print("  --- per statement class (top 25) ---")
for k, c in per_stmt.most_common(25):
    print("  %7d  %7.2f/s  %s" % (c, c / dur, k))
print()
print("  --- ListPendingRuns, the git resolver's statement (FINDINGS.md:563 measured 5.006 q/s per replica) ---")
hit = [(k, c) for k, c in per_stmt.items() if k.startswith('SELECT id, spec, created_at FROM runs')]
if not hit:
    print("  (no match; check the statement text against internal/controller/scheduler.go)")
for k, c in hit:
    print("  total %d  %.3f q/s across the stack" % (c, c / dur))
    for svc in sorted(per_host):
        cc = per_host_stmt.get((svc, k), 0)
        if cc:
            print("    %-14s %6d  %.3f q/s" % (svc, cc, cc / dur))
print()
print("  --- advisory-lock traffic, the per-tick 'leader election' (FINDINGS.md:515) ---")
adv = [(k, c) for k, c in per_stmt.items() if 'advisory' in k]
for k, c in sorted(adv, key=lambda x: -x[1]):
    print("  %7d  %7.2f/s  %s" % (c, c / dur, k))
    for svc in sorted(per_host):
        cc = per_host_stmt.get((svc, k), 0)
        if cc:
            print("      %-12s %6d" % (svc, cc))
print()
print("  --- per-replica split of the top 8 classes ---")
for k, c in per_stmt.most_common(8):
    parts = " ".join("%s=%d" % (s, per_host_stmt.get((s, k), 0)) for s in sorted(per_host))
    print("  %-100s %s" % (k[:100], parts))
