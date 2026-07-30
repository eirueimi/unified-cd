import io, re, sys, collections, datetime

LOGFILE = sys.argv[1]
OUT = sys.argv[2]

IPS = {'172.20.0.3': 'controller1', '172.20.0.4': 'controller2', '172.20.0.5': 'controller3'}
KEYS = {
    1702388580: '01 scheduler',
    1819240289: '02 log-archiver',
    1667326824: '03 cache-cleanup',
    1634759286: '04 approval-reaper',
    1937012075: '05 stuckrun-reaper',
    1903519093: '06 queuedrun-reaper',
    1937337955: '07 appsource-sync-reaper',
    1635083380: '08 audit-retention',
    1920103534: '09 run-retention',
    1819570797: '10 log-trim',
    1634758771: '11 appsource-reconciler',
}

line_re = re.compile(r'^(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d\.\d+) UTC \[(\d+)\] h=(\S*)\s+(LOG|DETAIL):\s+(.*)$')

events = []          # (ts, pid, host, verb, key)
other = collections.Counter()   # (statement-tag, host)
pending = {}         # pid -> verb awaiting its DETAIL line
first_ts = last_ts = None

def parse_ts(s):
    return datetime.datetime.strptime(s, '%Y-%m-%d %H:%M:%S.%f')

with io.open(LOGFILE, encoding='utf-8', errors='replace') as fh:
    for raw in fh:
        m = line_re.match(raw.rstrip('\n'))
        if not m:
            continue
        ts, pid, host, kind, body = m.group(1), int(m.group(2)), m.group(3), m.group(4), m.group(5)
        t = parse_ts(ts)
        if first_ts is None:
            first_ts = t
        last_ts = t
        if kind == 'LOG':
            if 'pg_try_advisory_lock($1)' in body:
                pending[pid] = ('try', t, host)
            elif 'pg_advisory_unlock($1)' in body:
                pending[pid] = ('unlock', t, host)
            else:
                pending.pop(pid, None)
                if 'DELETE FROM agents WHERE last_seen_at' in body:
                    other[('DeleteStaleAgents', host)] += 1
                elif 'DELETE FROM oidc_states' in body:
                    other[('DeleteExpiredOIDCStates', host)] += 1
                elif 'DELETE FROM agents WHERE id' in body:
                    other[('agent deregister', host)] += 1
                elif 'FROM runs' in body and "status = 'Pending'" in body and 'ORDER BY created_at' in body:
                    other[('ListPendingRuns(git resolver)', host)] += 1
        else:  # DETAIL
            p = pending.pop(pid, None)
            if not p:
                continue
            mm = re.search(r"\$1 = '(-?\d+)'", body)
            if not mm:
                continue
            key = int(mm.group(1))
            events.append((p[1], pid, p[2], p[0], key))

events.sort(key=lambda e: (e[0], e[1]))

out = []
def w(s=''):
    out.append(s)

w('window: %s .. %s  (%.1fs)' % (first_ts, last_ts, (last_ts - first_ts).total_seconds()))
w('advisory events parsed: %d' % len(events))
w('')

# --- Q1: did every replica contend? ---
w('=== Q1: pg_try_advisory_lock attempts per (key, replica) ===')
attempts = collections.Counter()
for t, pid, host, verb, key in events:
    if verb == 'try':
        attempts[(key, host)] += 1
for key in sorted(set(k for k, _ in attempts)):
    name = KEYS.get(key, 'UNKNOWN(%d)' % key)
    row = []
    for ip in sorted(IPS):
        row.append('%s=%d' % (IPS[ip], attempts.get((key, ip), 0)))
    w('%-24s %-12d %s' % (name, key, '  '.join(row)))
missing = [KEYS[k] for k in KEYS if not any((k, ip) in attempts for ip in IPS)]
w('')
w('keys with ZERO acquisition attempts in this window: %s' % (', '.join(sorted(missing)) or 'none'))
w('')

# --- Q2: hold intervals + overlap check ---
w('=== Q2: hold intervals per key (paired try->unlock on the same pid) ===')
by_pk = collections.defaultdict(list)
for e in events:
    by_pk[(e[1], e[4])].append(e)

intervals = collections.defaultdict(list)   # key -> [(start, end, pid, host)]
unreleased = []                             # (key, pid, host, start)
losses = collections.Counter()              # (key, host)
for (pid, key), evs in by_pk.items():
    open_try = None
    for t, _pid, host, verb, _k in evs:
        if verb == 'try':
            if open_try is not None:
                losses[(key, open_try[1])] += 1
            open_try = (t, host)
        else:
            if open_try is not None:
                intervals[key].append((open_try[0], t, pid, open_try[1]))
                open_try = None
    if open_try is not None:
        unreleased.append((key, pid, open_try[1], open_try[0]))

for key in sorted(intervals):
    ivs = sorted(intervals[key])
    durs = [(b - a).total_seconds() for a, b, _, _ in ivs]
    w('%-24s holds=%d  min=%.4fs  median=%.4fs  max=%.4fs' % (
        KEYS.get(key, key), len(ivs), min(durs), sorted(durs)[len(durs) // 2], max(durs)))
    holders = collections.Counter(IPS.get(h, h) for _, _, _, h in ivs)
    w('%-24s   winners by replica: %s' % ('', dict(holders)))

w('')
w('unreleased (still-held or lost) trailing acquisitions:')
for key, pid, host, start in sorted(unreleased, key=lambda x: x[3]):
    w('  %-24s pid=%d %s at %s' % (KEYS.get(key, key), pid, IPS.get(host, host), start))

w('')
w('=== OVERLAP CHECK (two replicas holding the same key at once) ===')
viol = 0
for key in sorted(intervals):
    ivs = sorted(intervals[key])
    for i in range(len(ivs) - 1):
        a_s, a_e, a_pid, a_h = ivs[i]
        b_s, b_e, b_pid, b_h = ivs[i + 1]
        if b_s < a_e:
            viol += 1
            w('  OVERLAP %s: pid %d (%s) [%s..%s] vs pid %d (%s) [%s..%s]' % (
                KEYS.get(key, key), a_pid, a_h, a_s, a_e, b_pid, b_h, b_s, b_e))
w('  overlaps found: %d' % viol)

# --- Q3: one winner per tick cluster ---
w('')
w('=== Q3: tick clusters (trys grouped within 1s) and winners per cluster ===')
for key in sorted(set(k for k, _ in attempts)):
    trys = sorted([(t, host) for t, pid, host, verb, k in events if verb == 'try' and k == key])
    if key == 1702388580:
        w('%-24s skipped (200ms poll, not a tick cluster; see scheduler note)' % KEYS[key])
        continue
    clusters = []
    for t, host in trys:
        if clusters and (t - clusters[-1][-1][0]).total_seconds() <= 1.0:
            clusters[-1].append((t, host))
        else:
            clusters.append([(t, host)])
    winners_per_cluster = []
    ivs = sorted(intervals.get(key, []))
    for c in clusters:
        lo, hi = c[0][0], c[-1][0]
        n = sum(1 for s, e, _, _ in ivs if lo - datetime.timedelta(seconds=1) <= s <= hi + datetime.timedelta(seconds=1))
        winners_per_cluster.append(n)
    sizes = collections.Counter(len(c) for c in clusters)
    w('%-24s clusters=%d  cluster sizes=%s  winners/cluster=%s' % (
        KEYS.get(key, key), len(clusters), dict(sizes), dict(collections.Counter(winners_per_cluster))))

# --- unlocked jobs ---
w('')
w('=== Unlocked jobs: statements per replica ===')
tags = sorted(set(tag for tag, _ in other))
for tag in tags:
    row = ['%s=%d' % (IPS.get(ip, ip), other.get((tag, ip), 0)) for ip in sorted(IPS)]
    extra = {h: c for (tg, h), c in other.items() if tg == tag and h not in IPS}
    w('%-32s %s %s' % (tag, '  '.join(row), ('other=%s' % extra) if extra else ''))

io.open(OUT, 'w', encoding='utf-8', newline='\n').write('\n'.join(out) + '\n')
print('\n'.join(out))
