"""Fire one HTTP POST at an absolute epoch instant with ms accuracy.
usage: fire.py <target_epoch> <url> <json_body>
Prints: FIRE t_send=<epoch> t_recv=<epoch> status=<code> body=<text>
"""
import os, sys, time, json, urllib.request, urllib.error
# Admin PAT: UNIFIED_TOKEN, else the cache written by bootstrap_admin_token (lib/admin-token.sh).
ADMIN_PAT = os.environ.get("UNIFIED_TOKEN") or open(os.path.join(os.environ.get("TMPDIR", "/tmp"), "unified-ha-admin-token")).read().strip()
tgt = float(sys.argv[1]); url = sys.argv[2]; body = sys.argv[3].encode()
# coarse sleep, then spin the last 15 ms
while True:
    d = tgt - time.time()
    if d <= 0: break
    time.sleep(d - 0.015 if d > 0.030 else 0)
req = urllib.request.Request(url, data=body, method="POST",
    headers={"Authorization": "Bearer " + ADMIN_PAT, "Content-Type": "application/json"})
t0 = time.time()
try:
    with urllib.request.urlopen(req) as r:
        code, txt = r.status, r.read().decode(errors="replace")
except urllib.error.HTTPError as e:
    code, txt = e.code, e.read().decode(errors="replace")
t1 = time.time()
print("FIRE t_send=%.6f t_recv=%.6f status=%d body=%s" % (t0, t1, code, txt.strip().replace("\n", " ")))
