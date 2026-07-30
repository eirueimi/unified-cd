#!/usr/bin/env python3
"""W2-6: extract every schedule fire from a Postgres log with log_statement='all'.

Emits one line per event of interest:
  LIST  <ts> h=<host> pid=<pid>            ListSchedules (a schedule check happened)
  INS   <ts> h=<host> pid=<pid>            INSERT INTO runs ... triggered_by='schedule:...'
  UPD   <ts> h=<host> pid=<pid> occ=<...>  UPDATE schedules SET last_fired_at=$1
and a paired summary table with the INSERT->UPDATE window in ms.
"""
import re
import sys
from datetime import datetime

TS = re.compile(r"^(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d\.\d+) UTC \[(\d+)\] h=(\S+) (LOG|DETAIL):\s+(.*)$")


def parse(path):
    """Merge continuation lines into their log record first, then classify.

    Necessary because the INSERT statement is logged across several physical
    lines and only the first carries the timestamp prefix.
    """
    recs = []
    with open(path, encoding="utf-8", errors="replace") as fh:
        for line in fh:
            line = line.rstrip("\n")
            m = TS.match(line)
            if m:
                recs.append(list(m.groups()))
            elif recs:
                recs[-1][4] += " " + line.strip()
    events = []
    cur = None
    for ts, pid, host, kind, body in recs:
        t = datetime.strptime(ts, "%Y-%m-%d %H:%M:%S.%f")
        if kind == "LOG":
            if "FROM schedules ORDER BY name" in body:
                cur = {"k": "LIST", "t": t, "pid": pid, "h": host}
                events.append(cur)
            elif "INSERT INTO runs" in body:
                cur = {"k": "INS", "t": t, "pid": pid, "h": host, "trig": None}
                events.append(cur)
            elif "UPDATE schedules SET last_fired_at" in body:
                cur = {"k": "UPD", "t": t, "pid": pid, "h": host, "occ": None}
                events.append(cur)
            else:
                cur = None
        elif kind == "DETAIL" and cur is not None:
            if cur["k"] == "INS":
                mm = re.search(r"\$6 = '([^']*)'", body)
                if mm:
                    cur["trig"] = mm.group(1)
            elif cur["k"] == "UPD":
                mm = re.search(r"\$1 = '([^']*)'", body)
                if mm:
                    cur["occ"] = mm.group(1)
    return [e for e in events if e["k"] != "INS" or (e["trig"] or "").startswith("schedule:")]


def main():
    events = parse(sys.argv[1])
    for e in events:
        ts = e["t"].strftime("%H:%M:%S.%f")[:-3]
        if e["k"] == "UPD":
            print(f"UPD   {ts} h={e['h']} pid={e['pid']} occ={e['occ']}")
        elif e["k"] == "INS":
            print(f"INS   {ts} h={e['h']} pid={e['pid']} trig={e['trig']}")
        else:
            print(f"LIST  {ts} h={e['h']} pid={e['pid']}")
    print()
    print("paired INSERT -> next UPDATE (same pid), window in ms:")
    print("  n  insert_ts     update_ts     win_ms  host          pid    occurrence")
    n = 0
    for i, e in enumerate(events):
        if e["k"] != "INS":
            continue
        upd = None
        for f in events[i + 1:]:
            if f["k"] == "UPD" and f["pid"] == e["pid"]:
                upd = f
                break
            if f["k"] == "INS" and f["pid"] == e["pid"]:
                break
        n += 1
        if upd is None:
            print(f"  {n:<3}{e['t'].strftime('%H:%M:%S.%f')[:-3]}  {'NO UPDATE':<13} {'-':>6}  {e['h']:<13} {e['pid']:<6} ORPHANED FIRE")
            continue
        win = (upd["t"] - e["t"]).total_seconds() * 1000
        print(f"  {n:<3}{e['t'].strftime('%H:%M:%S.%f')[:-3]}  {upd['t'].strftime('%H:%M:%S.%f')[:-3]}  {win:6.1f}  {e['h']:<13} {e['pid']:<6} {upd['occ']}")
    print()
    occs = {}
    for e in events:
        if e["k"] == "UPD":
            occs.setdefault(e["occ"], []).append(e["t"].strftime("%H:%M:%S.%f")[:-3])
    dup = {k: v for k, v in occs.items() if len(v) > 1}
    print(f"distinct occurrences written: {len(occs)}; REPEATED occurrence binds: {len(dup)}")
    for k, v in dup.items():
        print(f"  REPEATED occ={k} written at {v}")


if __name__ == "__main__":
    main()
