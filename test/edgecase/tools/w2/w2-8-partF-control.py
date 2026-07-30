"""Part F control: identical procedure, but the approve is fired OUTSIDE the
agent's cancel-detection fence (cancel + 8 s > CancelPollInterval = 5 s).
Expect: no post-gate execution.  usage: partF-control.py <label> <delay_s>"""
import json, sys, time, urllib.request, urllib.error, os
API="http://localhost:18080"; TOK="ha-admin-token"
label=sys.argv[1]; delay=float(sys.argv[2])
def req(m,p,b=None):
    d=json.dumps(b).encode() if b is not None else None
    r=urllib.request.Request(API+p,data=d,method=m,headers={"Authorization":"Bearer "+TOK,"Content-Type":"application/json"})
    t0=time.time()
    try:
        with urllib.request.urlopen(r) as x: c,t=x.status,x.read().decode(errors="replace")
    except urllib.error.HTTPError as e: c,t=e.code,e.read().decode(errors="replace")
    return c,t,t0,time.time()
out=[]
def say(*a):
    s=" ".join(str(x) for x in a); print(s,flush=True); out.append(s)
say("=== CONTROL %s (approve at cancel + %.1f s, fence = CancelPollInterval 5 s) ===" % (label, delay))
c,t,_,_=req("POST","/api/v1/runs",{"jobName":"edge-approval"})
rid=json.loads(t)["id"]; say("runID=%s"%rid)
for _ in range(200):
    c,b,_,_=req("GET","/api/v1/runs/%s/steps"%rid)
    if c==200 and any(s.get("index")==1 and s.get("status")=="WaitingApproval" for s in json.loads(b) or []):
        break
    time.sleep(0.4)
say("WaitingApproval at host %.6f"%time.time())
time.sleep(6.0)
c1,b1,s1,_=req("POST","/api/v1/runs/%s/cancel"%rid); say("CANCEL status=%d t_send=%.6f"%(c1,s1))
time.sleep(delay)
c2,b2,s2,_=req("POST","/api/v1/runs/%s/approvals/1"%rid,{"decision":"approve","comment":"w2-8 partF control "+label})
say("APPROVE status=%d t_send=%.6f (%.3f s after cancel) body=%s"%(c2,s2,s2-s1,b2.strip()))
time.sleep(20)
for p in ["/api/v1/runs/%s/steps"%rid,"/api/v1/runs/%s/approvals"%rid]:
    c,b,_,_=req("GET",p); say("%s -> %d %s"%(p,c,b.strip()))
say("RUNID_FOR_SQL %s"%rid)
outdir=os.environ.get("SCRATCH") or os.path.dirname(os.path.abspath(__file__))
open(os.path.join(outdir,"partF-%s.txt"%label),"w").write(chr(10).join(out)+chr(10))
