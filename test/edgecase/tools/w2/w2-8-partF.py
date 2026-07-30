"""W2-8 Part F — cancel a WaitingApproval run, then approve INSIDE the agent's
cancel-detection fence, aiming so that the agent's 3s GetApproval poll observes
Approved BEFORE its 5s cancel poll observes Cancelled.

usage: partF.py <label>
Reads the two live poll grids from the controller HTTP logs, predicts them
forward, and fires cancel + approve at a computed instant.
"""
import json, subprocess, sys, time, urllib.request, urllib.error, re, os

API = "http://localhost:18080"
TOK = "ha-admin-token"
# Run from test/ha, or point HA_DIR at it. COMPOSE_FILES overrides CF.
HA = os.environ.get("HA_DIR", os.getcwd())
CF = os.environ.get("COMPOSE_FILES", "-f docker-compose.ha.yaml").split()
label = sys.argv[1]


def req(method, path, body=None):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(API + path, data=data, method=method,
                               headers={"Authorization": "Bearer " + TOK,
                                        "Content-Type": "application/json"})
    t0 = time.time()
    try:
        with urllib.request.urlopen(r) as resp:
            code, txt = resp.status, resp.read().decode(errors="replace")
    except urllib.error.HTTPError as e:
        code, txt = e.code, e.read().decode(errors="replace")
    t1 = time.time()
    return code, txt, t0, t1


def ctrl_logs(since):
    p = subprocess.run(["docker", "compose"] + CF + ["logs", "--no-log-prefix",
                        "--since", since, "controller1", "controller2", "controller3"],
                       cwd=HA, capture_output=True, text=True,
                       env={**os.environ, "MSYS_NO_PATHCONV": "1"})
    return p.stdout


def parse_ts(s):
    # 2026-07-30T05:12:43.376498Z -> epoch float
    m = re.match(r"(\d{4})-(\d\d)-(\d\d)T(\d\d):(\d\d):(\d\d)\.(\d+)Z", s)
    frac = float("0." + m.group(7))
    tm = (int(m.group(1)), int(m.group(2)), int(m.group(3)),
          int(m.group(4)), int(m.group(5)), int(m.group(6)), 0, 0, 0)
    import calendar
    return calendar.timegm(tm) + frac


out = []
def say(*a):
    line = " ".join(str(x) for x in a)
    print(line, flush=True)
    out.append(line)


say("=== ATTEMPT %s ===" % label)

# 1. trigger; the POST /api/v1/runs log line gives us host<->controller offset
code, txt, t0, t1 = req("POST", "/api/v1/runs", {"jobName": "edge-approval"})
say("trigger status=%d t_send=%.6f t_recv=%.6f body=%s" % (code, t0, t1, txt.strip()))
runID = json.loads(txt)["id"]
say("runID=%s" % runID)
trig_mid = (t0 + t1) / 2.0

# 2. wait for step 1 WaitingApproval
t_wait = None
for _ in range(200):
    c, b, _, _ = req("GET", "/api/v1/runs/%s/steps" % runID)
    if c == 200:
        try:
            steps = json.loads(b)
        except Exception:
            steps = []
        for s in steps or []:
            if s.get("index") == 1 and s.get("status") == "WaitingApproval":
                t_wait = time.time()
                break
    if t_wait:
        break
    time.sleep(0.4)
if not t_wait:
    say("FAIL: never reached WaitingApproval")
    sys.exit(1)
say("WaitingApproval observed at host %.6f" % t_wait)

# 3. let both grids lay down samples, then read them
time.sleep(13.5)
logs = ctrl_logs("70s")
t_logread = time.time()
appr, canc, offs = [], [], []
for ln in logs.splitlines():
    if '"msg":"http request"' not in ln or runID not in ln:
        continue
    try:
        j = json.loads(ln.strip().rstrip(","))
    except Exception:
        j = None
        m = re.search(r'"time":"([^"]+)".*?"method":"([^"]+)","path":"([^"]+)"', ln)
        if not m:
            continue
        j = {"time": m.group(1), "method": m.group(2), "path": m.group(3)}
    ts = parse_ts(j["time"])
    if j["method"] == "GET" and j["path"].endswith("/approvals/1") and "/agents/" in j["path"]:
        appr.append(ts)
    elif j["method"] == "GET" and j["path"] == "/api/v1/runs/" + runID:
        canc.append(ts)
    elif j["method"] == "POST" and j["path"] == "/api/v1/runs":
        offs.append(ts)
appr.sort(); canc.sort()
offset = (offs[0] - trig_mid) if offs else 0.0   # ctrl_clock - host_clock
say("ctrl-host offset = %+.6f s (from POST /api/v1/runs, %d samples)" % (offset, len(offs)))
say("approval polls (ctrl clock, n=%d): %s" % (len(appr), ["%.3f" % (x % 60) for x in appr[-6:]]))
say("cancel   polls (ctrl clock, n=%d): %s" % (len(canc), ["%.3f" % (x % 60) for x in canc[-6:]]))
if len(appr) < 2 or len(canc) < 1:
    say("FAIL: not enough poll samples to aim")
    sys.exit(1)

# 4. aim.  Work in HOST clock: host = ctrl - offset
a_last = appr[-1] - offset
c_last = canc[-1] - offset
say("last approval poll (host) %.6f ; last cancel poll (host) %.6f" % (a_last, c_last))

# candidate: fire approve LEAD_A seconds before an approval tick, and require the
# cancel fire instant to be at least MIN_C after the previous cancel tick.
LEAD_A = 0.45      # approve commits this long before the agent's approval poll
GAP = float(sys.argv[2]) if len(sys.argv) > 2 else 0.12   # cancel -> approve spacing
MIN_C = 0.35       # cancel must land at least this long AFTER a cancel tick
now = t_logread
best = None
for k in range(1, 200):
    a_tick = a_last + 3.0 * k
    t_appr = a_tick - LEAD_A
    t_canc = t_appr - GAP
    if t_canc < now + 1.0:
        continue
    m = int((t_canc - c_last) // 5.0)
    c_prev = c_last + 5.0 * m
    c_next = c_prev + 5.0
    # the approval tick that will observe Approved must fall BEFORE the next
    # cancel poll, with margin; and the cancel must not land on a cancel tick.
    if (t_canc - c_prev) >= MIN_C and (c_next - a_tick) >= 0.35:
        best = (t_canc, t_appr, a_tick, c_prev, c_next)
        break
if not best:
    say("FAIL: no aim found")
    sys.exit(1)
t_canc, t_appr, a_tick, c_prev, c_next = best
say("AIM: cancel@%.6f approve@%.6f | predicted approval tick %.6f (approve leads by %.3f s)"
    % (t_canc, t_appr, a_tick, a_tick - t_appr))
say("AIM: predicted cancel ticks %.6f (prev) / %.6f (next); next cancel tick is %.3f s after approve"
    % (c_prev, c_next, c_next - t_appr))


def spin_until(tgt):
    while True:
        d = tgt - time.time()
        if d <= 0:
            return
        time.sleep(d - 0.015 if d > 0.030 else 0)


spin_until(t_canc)
c1, b1, s1, e1 = req("POST", "/api/v1/runs/%s/cancel" % runID)
say("CANCEL status=%d t_send=%.6f t_recv=%.6f body=%s" % (c1, s1, e1, b1.strip()))
spin_until(t_appr)
c2, b2, s2, e2 = req("POST", "/api/v1/runs/%s/approvals/1" % runID,
                     {"decision": "approve", "comment": "w2-8 partF " + label})
say("APPROVE status=%d t_send=%.6f t_recv=%.6f body=%s" % (c2, s2, e2, b2.strip()))
say("measured cancel->approve send gap = %.3f ms" % ((s2 - s1) * 1000.0))

# 5. watch the run for 40 s
say("--- post-fire watch (host clock) ---")
t_end = time.time() + 40
seen = set()
while time.time() < t_end:
    # NB: list endpoint, not GET /api/v1/runs/{id} — the latter is exactly the
    # agent cancel poller's path and polling it would pollute that grid in the
    # controller HTTP log we read back for attribution.
    c, b, _, _ = req("GET", "/api/v1/runs?limit=30")
    st = "?"
    if c == 200:
        for r_ in json.loads(b) or []:
            if r_.get("id") == runID:
                st = r_.get("status")
    c2_, b2_, _, _ = req("GET", "/api/v1/runs/%s/steps" % runID)
    sm = ""
    if c2_ == 200:
        try:
            sm = "|".join("%d:%s" % (s["index"], s["status"]) for s in sorted(json.loads(b2_), key=lambda x: x["index"]))
        except Exception:
            sm = b2_[:80]
    key = (st, sm)
    line = "WATCH %.3f run=%s steps=%s" % (time.time(), st, sm)
    if key not in seen:
        seen.add(key)
        say(line + "   <-- CHANGE")
    time.sleep(1.0)
say("--- final ---")
for p in ["/api/v1/runs/%s/steps" % runID,
          "/api/v1/runs/%s/approvals" % runID]:
    c, b, _, _ = req("GET", p)
    say("%s -> %d %s" % (p, c, b.strip()))
say("RUNID_FOR_SQL %s" % runID)

outdir = os.environ.get("SCRATCH") or os.path.dirname(os.path.abspath(__file__))
with open(os.path.join(outdir, "partF-%s.txt" % label), "w") as f:
    f.write("\n".join(out) + "\n")
