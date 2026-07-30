# W2 scenario drivers

The arms these scripts drive are described in `test/edgecase/scenarios/w2-*.md`.
Each script is the *procedure*, not the analysis: read the runbook first, run the
script from `test/ha`, and interpret the output with the runbook's deliverable
list.

They were written during the 2026-07-29/30 execution and lived only in the
campaign evidence root until now, which meant several arms — W2-4's Part B
backlog (the one that found its major), W2-9's Part D, W2-8's whole Part F
toolchain — could not be re-run from a checkout alone.

## Conventions

Every script is run **from `test/ha`** and reads two environment variables:

    export SCRATCH=<scratchpad>/w2-N        # capture directory; mkdir -p first
    export COMPOSE_FILES="-f docker-compose.ha.yaml ..."   # the scenario's stack

`COMPOSE_FILES` defaults to the scenario's own invocation, so it only has to be
set when overriding. On Windows also `export MSYS_NO_PATHCONV=1` (W2-5).

A few scripts address a container by name for a `docker kill` / `docker start`
whose round-trip is itself being measured — going through `docker compose` would
add a second layer of latency to the number under test. Those read a
`COMPOSE_PROJECT` prefix, default `unified-cd-ha`.

The only credential any of these use is `ha-admin-token`, the `test/ha` fixture
token already in `docker-compose.ha.yaml`. **Do not add real agent credentials
here** — W2-7 kept its enrollment tokens in uncommitted scratch files and that is
the standing rule.

## Contents

| Script | Scenario | Arm |
|---|---|---|
| `w2-3-armD1.sh` | W2-3 | Arm D1 — race `failOrphanedRun`'s 1 ms window (0/10; see the runbook before spending time on it) |
| `w2-4-partB-backlog.sh` | W2-4 | Part B5 — amplify the reaper's SELECT→UPDATE window with a backlog. **This is the arm that found W2-4's major.** |
| `w2-4-partB-phase.sh` | W2-4 | Part B — phase-lock a trial onto the sweep grid, then call `partB-trial` |
| `w2-4-partB-trial.sh` | W2-4 | Part B — one boundary trial at `created_at + grace + offset` |
| `w2-4-partC.sh` | W2-4 | Part C — the `created_at` clock, via a legitimately mutex-blocked run |
| `w2-4-sample.sh` | W2-4 | generic in-container run sampler |
| `w2-5-partB-inject2.sh` | W2-5 | Part B — steplock injection, polling for a child **newer than the parent** |
| `w2-6-detector.sh` | W2-6 | the divergence detector (gate G5 and every arm) |
| `w2-6-armA.sh` | W2-6 | Part B Arm A — aim a SIGKILL at the leader's check instant (0/20; widen the window with Part C instead) |
| `w2-6-partC.sh` | W2-6 | Part C — hold the schedules row locked, kill the blocked leader |
| `w2-6-partD.sh` | W2-6 | Part D — fail exactly one `last_fired_at` write with a one-shot trigger |
| `w2-6-fires.py` | W2-6 | reduce a `log_statement='all'` capture to schedule fires + the INSERT→UPDATE window |
| `w2-8-partA.sh` | W2-8 | Part A — the natural timeout window, aimed at the reaper grid |
| `w2-8-partC2.sh` | W2-8 | Part C — phase-lock a decision onto the approval reaper's sweep |
| `w2-8-partF.py` | W2-8 | Part F — approve **inside** the agent's cancel-detection fence |
| `w2-8-partF-control.py` | W2-8 | Part F — the mandatory control, approve outside the fence |
| `w2-8-partF-capture.sh` | W2-8 | Part F — capture the five artifacts per attempt |
| `w2-8-fire.py` | W2-8 | fire one POST at an absolute epoch (sub-ms; a shell busy-wait cannot do this) |
| `w2-8-grid2.awk` | W2-8 | reduce a Postgres log to `run_approvals` REAPER/DECIDE events |
| `w2-9-partA.sh` | W2-9 | Part A — hog, saturate, probe, poll |
| `w2-9-partB.sh` | W2-9 | Part B — falsify the threshold by cancelling one at a time |
| `w2-9-partD.sh` | W2-9 | Part D — post-promotion tick vs `docs/high-availability.md:163`. **Run this first.** |
| `w2-1-analyze.py` | W2-1 | reduce a `log_statement='all'` capture to per-key advisory-lock events |
