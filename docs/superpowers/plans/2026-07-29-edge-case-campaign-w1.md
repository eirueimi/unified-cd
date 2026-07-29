# Edge-Case Campaign: Wave W1 (Recovery / Failover) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Execute Wave W1 (recovery/failover scenarios) of the edge-case campaign: build the fault-injection tooling, then run six live scenarios against the test/ha compose stack, recording findings.

**Architecture:** Per `docs/superpowers/specs/2026-07-29-edge-case-testing-design.md` and the W0-validated pattern: per-scenario runbooks under `test/edgecase/scenarios/`, compose overlays under `test/edgecase/compose/`, findings appended to `test/edgecase/FINDINGS.md` (classification rule stated in the W0 checkpoint). One-way agent→controller partition is implemented at nginx (all agent traffic transits it and the system is strictly agent-polls-controller, so a per-agent nginx block IS a full one-way partition; no image rebuild or NET_ADMIN needed).

**Tech Stack:** docker compose (test/ha stack + overlays), nginx deny toggling via a blocklist include, curl against LB `localhost:18080` (token `ha-admin-token`), `unified-cli agent identity` commands, psql for lock inspection.

## Global Constraints

- All committed text is English (AGENTS.md).
- Work on branch `plan/edge-case-w1` in worktree `wt-edge-spec` — never commit on the main checkout.
- **No production-code changes** (spec §8). Test-only files under `test/edgecase/` allowed; `test/ha/` files are NOT modified (overlays only).
- Findings record problems; they do not fix them. Classification rule (from the W0 checkpoint in FINDINGS.md): violation = observed behavior contradicts an invariant or documented contract; observation = behavior matched expectations but reveals a risk.
- Scenario execution tasks follow the W0-1 pattern: write runbook → commit → execute → record findings → commit findings separately.
- Docker running; stack builds take minutes — Bash timeouts up to 600000 ms. Tear every stack down with `-v` after each scenario.
- Verified API facts used throughout (do not re-derive): cancel = `POST /api/v1/runs/{id}/cancel`, agent cancel-poll = `GET /api/v1/runs/{id}` every 5s; approvals = `GET /api/v1/runs/{id}/approvals` and `POST /api/v1/runs/{id}/approvals/{stepIndex}` with body `{"decision":"approve"|"reject"}` (default timeout 60 min, applied at claim build; controller approval-reaper ticks every 1 min under advisory lock and only marks rows TimedOut — the agent's WaitForApproval holds an independent deadline); credential revocation = `unified-cli agent identity revoke-credentials <agent-id>` / `disable <agent-id>` — **effectively immediate**: `agentAuth` re-reads the credential row from Postgres on every agent API request (`internal/controller/agent_auth.go:62-77`), so the first agent call after the `UPDATE` commits is rejected (measured 1.5-5.0s in W1-6, entirely the agent's own poll cadence). The 1h access-token TTL (`internal/controller/api_agent_enrollment.go:21`) and the agent's 15-min lazy-refresh lead plus jitter (`internal/agent/credentials.go:27-28,140`) are two different sides of the same exchange and govern only how often the **client** re-exchanges its refresh token; they create **no** server-side blind window; job-level mutex = `spec.concurrency.mutex: <name>` (`internal/dsl/types.go:86`) — **not** `spec.mutex:`, which `dec.KnownFields(true)` (`internal/dsl/parse.go:94`) makes a hard 400.

---

### Task 1: Spec amendments (pause does not exist; W1-7 deferred)

**Files:**
- Modify: `docs/superpowers/specs/2026-07-29-edge-case-testing-design.md`

**Interfaces:**
- Produces: the corrected W1 scenario table that Tasks 3-8 implement.

- [ ] **Step 1: Fix W1-3** — replace the table row:

```markdown
| W1-3 (#8) | Controller restart/failover while a run is paused or awaiting an approval gate; approval-reaper timeout behavior across the restart | I1, I7 |
```

with:

```markdown
| W1-3 (#8) | Controller restart/failover while a run is awaiting an approval gate; approval-reaper timeout behavior across the restart. (Run pause/resume does not exist in unified-cd — earlier drafts assumed it did; an approval gate is the only wait-state a run can be in.) | I1, I7 |
```

- [ ] **Step 2: Defer W1-7** — replace the table row:

```markdown
| W1-7 (C13) | AppSource reconciler crash mid apply/prune: mixed git-generation window observable by schedules/webhooks; verify next cycle heals | I1, I7 |
```

with:

```markdown
| W1-7 (C13) | DEFERRED past W3: AppSource reconciler crash mid apply/prune. No existing e2e exercises AppSource against real git (reconciler tests are fully mocked), and `file://` remotes are rejected by design (`dsl.ValidateGitRepoURL`), so this scenario needs a git-over-HTTP server container (dumb protocol: bare repo + `git update-server-info` + static file server) — too expensive to bolt onto W1 | I1, I7 |
```

- [ ] **Step 3: Fix the DSL feature list** — in the spec's architecture summary (§ describing DSL features), remove the words `pause/resume, ` from the feature enumeration (the feature does not exist).

- [ ] **Step 4: Commit**

```bash
git add docs/superpowers/specs/2026-07-29-edge-case-testing-design.md
git commit -m "docs(spec): W1-3 approval-only (no pause feature exists), defer W1-7 AppSource

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: Fault-injection tooling + W1 workloads

**Files:**
- Create: `test/edgecase/tools/inject.sh`
- Create: `test/edgecase/compose/nginx-edge.conf`
- Create: `test/edgecase/compose/oneway.override.yaml`
- Create: `test/edgecase/workloads/longrun.payload.json`
- Create: `test/edgecase/workloads/sideeffect.payload.json`
- Create: `test/edgecase/workloads/mutex-successor.payload.json`
- Create: `test/edgecase/workloads/approval.payload.json`

**Interfaces:**
- Consumes: `test/ha/docker-compose.ha.yaml` service names and `test/ha/nginx.conf` (as the template for nginx-edge.conf — read it before writing; do not modify it).
- Produces: `inject.sh <cmd> <svc>` helper and payload files used verbatim by Tasks 3-8. Compose project name is `unified-cd-ha` (from the ha compose file's `name:`), so containers are `unified-cd-ha-<svc>-1`.

- [ ] **Step 1: Create `test/edgecase/tools/inject.sh`**

```bash
#!/usr/bin/env sh
# Fault-injection helpers for the edge-case campaign (W1+).
# Usage: inject.sh <command> <service> [args]
# Run from test/ha/ (paths are relative to it). COMPOSE_FILES may add overlays:
#   COMPOSE_FILES="-f docker-compose.ha.yaml -f ../edgecase/compose/oneway.override.yaml"
set -eu

COMPOSE_FILES="${COMPOSE_FILES:--f docker-compose.ha.yaml}"
dc() { docker compose $COMPOSE_FILES "$@"; }

cmd="${1:?usage: inject.sh <kill-soft|kill-hard|pause|unpause|partition|heal|nginx-block|nginx-unblock> <service>}"
svc="${2:?service name required}"

case "$cmd" in
  kill-soft)  dc kill -s SIGTERM "$svc" ;;
  kill-hard)  dc kill -s SIGKILL "$svc" ;;
  pause)      dc pause "$svc" ;;
  unpause)    dc unpause "$svc" ;;
  partition)  docker network disconnect unified-cd-ha_default "unified-cd-ha-$svc-1" ;;
  heal)       docker network connect    unified-cd-ha_default "unified-cd-ha-$svc-1" ;;
  nginx-block)
    # One-way agent->controller partition: deny this agent's IP at nginx.
    # Strictly agent-polls-controller, so this is a full one-way partition.
    ip=$(docker inspect "unified-cd-ha-$svc-1" \
      --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')
    dc exec -T nginx sh -c "echo 'deny $ip;' > /etc/nginx/blocklist/deny.conf && nginx -s reload"
    echo "blocked $svc ($ip) at nginx"
    ;;
  nginx-unblock)
    dc exec -T nginx sh -c ": > /etc/nginx/blocklist/deny.conf && nginx -s reload"
    echo "unblocked all at nginx"
    ;;
  *) echo "unknown command: $cmd" >&2; exit 2 ;;
esac
```

- [ ] **Step 2: Create `test/edgecase/compose/nginx-edge.conf`** — copy `test/ha/nginx.conf` VERBATIM, then make exactly two changes: (a) inside the `server { ... }` block, as the first directives of `location /` (and of every other `location` block if the file has more than one), insert:

```
            include /etc/nginx/blocklist/*.conf;
```

(b) add a comment at the top: `# Edge-campaign variant of test/ha/nginx.conf: adds a runtime-writable blocklist include (see tools/inject.sh nginx-block). Keep in sync with the base file.`

Note: `deny` is valid inside `location`; denied clients get 403. Verify your resulting file with `docker run --rm -v "$PWD/test/edgecase/compose/nginx-edge.conf:/etc/nginx/nginx.conf:ro" nginx:1.27-alpine nginx -t` — it must print `syntax is ok` (the blocklist dir may warn if missing; create the include dir in the overlay as below, and for the syntax check pre-create it with `--tmpfs /etc/nginx/blocklist`).

- [ ] **Step 3: Create `test/edgecase/compose/oneway.override.yaml`**

```yaml
# W1-5 overlay: nginx with a runtime-writable blocklist (per-agent one-way
# partition via tools/inject.sh nginx-block), plus a shared host bind mount
# for the side-effect workload so re-execution is observable from the host.
services:
  nginx:
    volumes:
      - ../edgecase/compose/nginx-edge.conf:/etc/nginx/nginx.conf:ro
      - blocklist:/etc/nginx/blocklist
  agent1:
    volumes:
      - ../edgecase/sideeffect-data:/data
  agent2:
    volumes:
      - ../edgecase/sideeffect-data:/data
volumes:
  blocklist:
```

Also create the (git-ignored at runtime, committed empty) data dir with a keeper: `test/edgecase/sideeffect-data/.gitkeep`, and add `test/edgecase/sideeffect-data/*` + `!test/edgecase/sideeffect-data/.gitkeep` to a new `test/edgecase/.gitignore`.

- [ ] **Step 4: Create the four payload files** (single-line JSON, byte-exact):

`test/edgecase/workloads/longrun.payload.json` (300 numbered ticks, 1/s — I4 line accounting):

```json
{"yaml":"apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: edge-longrun\nspec:\n  native: true\n  agentSelector:\n    - kind:linux\n  steps:\n    - name: tick\n      run: for i in $(seq 1 300); do echo \"tick $i\"; sleep 1; done\n"}
```

`test/edgecase/workloads/sideeffect.payload.json` (holds mutex `edge-mutex`, appends to /data 1/s for 120s — I2/I3):

```json
{"yaml":"apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: edge-sideeffect\nspec:\n  native: true\n  mutex: edge-mutex\n  agentSelector:\n    - kind:linux\n  steps:\n    - name: append\n      run: for i in $(seq 1 120); do echo \"$UNIFIED_RUN_ID,$i,$(date -u +%H:%M:%S)\" >> /data/sideeffect.log; sleep 1; done\n"}
```

(If `$UNIFIED_RUN_ID` is not an injected env var — verify with `grep -rn "UNIFIED_RUN_ID" internal/agent internal/api` before committing — substitute the literal `run` and rely on the per-run line count instead; note the substitution in the commit message.)

`test/edgecase/workloads/mutex-successor.payload.json` (same mutex, instant — probes lock release, I3):

```json
{"yaml":"apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: edge-mutex-successor\nspec:\n  native: true\n  mutex: edge-mutex\n  agentSelector:\n    - kind:linux\n  steps:\n    - name: probe\n      run: echo acquired-mutex-ok\n"}
```

`test/edgecase/workloads/approval.payload.json` (10-min gate between two steps — W1-3):

```json
{"yaml":"apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: edge-approval\nspec:\n  native: true\n  agentSelector:\n    - kind:linux\n  steps:\n    - name: before\n      run: echo before-gate\n    - name: gate\n      approval:\n        message: edge-campaign gate\n        timeoutMinutes: 10\n    - name: after\n      run: echo after-gate\n"}
```

- [ ] **Step 5: Verify** — `sh -n test/edgecase/tools/inject.sh` (syntax), the nginx -t check from Step 2, and merged-config render:

```bash
cd test/ha && docker compose -f docker-compose.ha.yaml -f ../edgecase/compose/oneway.override.yaml config --quiet && echo MERGE-OK
```

Paste all three outputs into your report.

- [ ] **Step 6: Commit**

```bash
git add test/edgecase/tools test/edgecase/compose test/edgecase/workloads test/edgecase/.gitignore test/edgecase/sideeffect-data/.gitkeep
git commit -m "test(edgecase): add W1 fault-injection tooling and workloads

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: W1-1 — all-controller restart during a long run

**Files:**
- Create: `test/edgecase/scenarios/w1-1-all-controller-restart.md`
- Modify: `test/edgecase/FINDINGS.md`

**Interfaces:** Consumes Task 2's `longrun.payload.json`. Invariants I1, I4, I5.

- [ ] **Step 1: Write the runbook** with exactly this content skeleton (flesh each bullet into the W0-1 runbook style — Invariants/Stack/steps with exact commands/Recording/Teardown):
  - Stack: plain ha compose (no overlay). Apply `edge-longrun` via `curl -fsS -X POST localhost:18080/api/v1/jobs -H "Authorization: Bearer ha-admin-token" -H "Content-Type: application/json" --data-binary @../edgecase/workloads/longrun.payload.json`; trigger via `POST /api/v1/runs` body `{"jobName":"edge-longrun"}`; capture run id.
  - Wait until the run is Running and ticking (~30s), attach SSE (`curl -N --max-time 15 .../runs/<id>/events`).
  - Inject: `../edgecase/tools/inject.sh kill-hard controller1` then same for controller2, controller3 (all three down ~60s: verify `curl -s -o /dev/null -w '%{http_code}' localhost:18080/readyz` returns 502/503). Then `docker compose -f docker-compose.ha.yaml up -d controller1 controller2 controller3`.
  - Observe: run keeps executing on the agent during the outage (docker logs agent); after recovery the run reaches Succeeded; **line accounting**: fetch full logs and count `tick` lines — must be exactly 300, no duplicates/reordering (I4; this exercises the LogPusher pending buffer + marker path from the failsafe audit); SSE re-attach works; time from controller start to first successful agent report (I5).
  - Recording guidance: missing/duplicated tick lines = major (I4); a `[N log line(s) dropped: controller unreachable]` marker with matching count = expected-mechanism observation, record count; run lost/failed = major (I1).
- [ ] **Step 2: Commit the runbook** (`test(edgecase): add W1-1 all-controller-restart runbook`, Co-Authored-By trailer).
- [ ] **Step 3: Execute it** end-to-end (baseline health check first: one leader, readyz 200). Capture bulky output to the session scratchpad `w1/` dir, not the repo.
- [ ] **Step 4: Record findings** in FINDINGS.md per the template + classification rule; teardown `down -v`; commit (`test(edgecase): record W1-1 findings`, trailer).

---

### Task 4: W1-2 — PostgreSQL outage/restart mid-run

**Files:**
- Create: `test/edgecase/scenarios/w1-2-postgres-restart.md`
- Modify: `test/edgecase/FINDINGS.md`

**Interfaces:** Consumes `longrun.payload.json`. Invariants I1, I5. (Compose runs a single postgres; primary/standby failover is out of compose scope — this scenario is outage+restart, and the runbook must say so.)

- [ ] **Step 1: Write the runbook**: start stack, trigger `edge-longrun`, wait Running; `inject.sh kill-hard postgres`; observe for ~45s: `/readyz` per controller turns 503 (DB ping), agent report retries in agent logs, SSE behavior; `docker compose -f docker-compose.ha.yaml up -d postgres`; observe recovery: controllers reconnect (readyz 200 within — record time), scheduler leader re-elected exactly once (grep "scheduler became leader" counts before/after), LISTEN/NOTIFY resubscribed (attach SSE after recovery, confirm live lines), run completes with 300 lines (share the I4 check with W1-1). Injection variant B: repeat with `kill-soft` (clean FIN) and note any difference in reconnect latency.
  - Recording: run failed due to the DB blip = major (I1); leader thrash (multiple became-leader lines per controller) = minor; SSE dead after recovery until client reconnect = record as observation with the exact behavior (server-side LISTEN reconnect is the contract in docs/high-availability.md §External Dependency Redundancy).
- [ ] **Step 2: Commit runbook.** **Step 3: Execute.** **Step 4: Findings + teardown `-v` + commit.** (Same commit-message pattern as Task 3, scenario id w1-2.)

---

### Task 5: W1-3 — controller restart while a run awaits an approval gate

**Files:**
- Create: `test/edgecase/scenarios/w1-3-approval-gate-restart.md`
- Modify: `test/edgecase/FINDINGS.md`

**Interfaces:** Consumes `approval.payload.json`. Invariants I1, I7. Verified API: `GET /api/v1/runs/{id}/approvals`; `POST /api/v1/runs/{id}/approvals/{stepIndex}` body `{"decision":"approve"}`; reaper ticks 1/min (marks TimedOut only — the agent holds an independent deadline).

- [ ] **Step 1: Write the runbook**, two parts:
  - **Part A (approve across a restart):** apply+trigger `edge-approval`; wait until `GET .../approvals` shows the gate Pending (record `stepIndex` and `timeoutAt`); kill-hard ALL controllers; 60s outage; restart; verify the approval row survived (same timeoutAt — I7); approve via API; verify the run resumes and Succeeds with `after-gate` in logs. Record time from approve to step start (agent approval-poll latency).
  - **Part B (timeout race across a restart):** re-trigger; this time edit nothing and let the gate sit; kill all controllers at T+~8min (2 min before the 10-min timeout); keep them down past the timeout (restart at T+~11min); observe: who times the gate out (controller reaper on restart vs agent's independent deadline — compare timestamps in both logs), the approval row's final status, the run's final status, and whether they agree (I7: an Approved/TimedOut row disagreeing with the run status is the C11-class violation this part hunts).
  - Recording: approval row lost across restart = major (I1); row/run status disagreement = major (I7); double-timeout (both clocks fire, conflicting writes) = record precisely with timestamps.
- [ ] **Step 2: Commit runbook.** **Step 3: Execute** (Part B takes ~12 min wall clock — poll, don't idle-sleep). **Step 4: Findings + teardown `-v` + commit** (scenario id w1-3).

---

### Task 6: W1-4 — cancel racing controller failover

**Files:**
- Create: `test/edgecase/scenarios/w1-4-cancel-vs-failover.md`
- Modify: `test/edgecase/FINDINGS.md`

**Interfaces:** Consumes `longrun.payload.json`. Invariants I1, I3. Verified: cancel = `POST /api/v1/runs/{id}/cancel`; the agent polls `GET /api/v1/runs/{id}` every 5s and cancels its runCtx on `Cancelled`.

- [ ] **Step 1: Write the runbook**, two races:
  - **Race A (cancel then immediate controller death):** trigger `edge-longrun`; once Running, `POST .../cancel` (expect 2xx) and within 1s kill-hard ALL controllers; restart after 30s. Observe: run stays `Cancelled` (the status was committed before the kill); the agent — whose 5s poll failed during the outage — detects Cancelled after recovery and stops the step (record detection latency from controller recovery); step process actually killed (agent logs).
  - **Race B (cancel during the outage):** trigger again; kill all controllers; attempt cancel through the LB while down (expect 502/503 — record exact code); restart controllers; retry the same cancel (expect 2xx); observe normal cancellation.
  - Recording: run resumes executing to completion despite committed Cancelled = major (I1); cancel 2xx but run never transitions = major; agent step process survives >30s after detection = record (I6-adjacent); anything leaking `mutex_holders` rows after Cancelled (psql check) = major (I3).
- [ ] **Step 2: Commit runbook.** **Step 3: Execute.** **Step 4: Findings + teardown `-v` + commit** (scenario id w1-4).

---

### Task 7: W1-5 — one-way partition: zombie executor

**Files:**
- Create: `test/edgecase/scenarios/w1-5-oneway-partition-zombie.md`
- Modify: `test/edgecase/FINDINGS.md`

**Interfaces:** Consumes Task 2's `oneway.override.yaml`, `nginx-edge.conf`, `inject.sh nginx-block/nginx-unblock`, `sideeffect.payload.json`, `mutex-successor.payload.json`. Invariants I2, I3, I6. Reaper facts (docs/high-availability.md): heartbeat 15s, staleAfter 90s, reaper interval 30s, claim grace 60s.

- [ ] **Step 1: Write the runbook:**
  - Stack: `COMPOSE_FILES="-f docker-compose.ha.yaml -f ../edgecase/compose/oneway.override.yaml"` for every compose/inject call. Clear `test/edgecase/sideeffect-data/sideeffect.log` before starting.
  - Apply both jobs; trigger `edge-sideeffect`; find the claiming agent via `GET /api/v1/runs/{id}` (`claimedBy` field — verify exact field name in the response JSON and note it in the runbook); `inject.sh nginx-block <that-agent-service>`.
  - Observe on a timeline (poll every ~15s, ~4 min total): T0 block; heartbeats start failing (agent logs); at ~90-120s the stuck-run reaper fails the run (controller logs + run status Failed); **zombie window**: `wc -l` of `sideeffect-data/sideeffect.log` keeps growing after the run is Failed — record for how long (I6; expected: until the 120s step simply finishes, since nothing can tell the agent to stop);
  - Immediately after the reap: trigger `edge-mutex-successor` — it must Queue, claim on the OTHER (unblocked) agent, acquire `edge-mutex`, and Succeed (I3: reap released the mutex; if it sits Queued>60s on a free agent, the mutex leaked = major). psql cross-check: `SELECT * FROM mutex_holders;` empty for edge-mutex.
  - `inject.sh nginx-unblock`; observe the healed agent: its buffered reports/finish for the dead run get rejected (403/conflict per the CAS + seal semantics — record exact responses in agent logs), the dropped-lines marker path, and whether the agent re-registers cleanly and claims new work.
  - I2 accounting: total lines in sideeffect.log for that run id must equal the seconds the step actually ran — no duplicates (the run was never re-queued; confirm run count for edge-sideeffect is exactly 1).
  - Recording: mutex leak = major (I3); any re-execution/duplicate side effects = major (I2); zombie duration and post-heal behavior = measured observation (I6 is explicitly document-don't-judge per the spec).
- [ ] **Step 2: Commit runbook.** **Step 3: Execute.** **Step 4: Findings + teardown `-v` (also delete sideeffect-data contents) + commit** (scenario id w1-5).

---

### Task 8: W1-6 — credential revocation mid-run

**Files:**
- Create: `test/edgecase/scenarios/w1-6-credential-revocation.md`
- Modify: `test/edgecase/FINDINGS.md`

**Interfaces:** Consumes `longrun.payload.json`; `unified-cli agent identity revoke-credentials|disable <agent-id>` (run via `docker compose ... run --rm agent-enroll go run -buildvcs=false ./cmd/unified-cli agent identity ...` — the enroll service has the toolchain and UNIFIED_SERVER/TOKEN env). Invariants I6, I7. Verified: access-token TTL 1h with lazy refresh 15 min before expiry → revocation does NOT bite a live agent quickly.

- [ ] **Step 1: Write the runbook:**
  - **Part A (revocation is lazy — bounded observation):** trigger `edge-longrun`; identify claiming agent; `agent identity revoke-credentials <agent-id>`; observe 5 min: the agent keeps executing AND keeps reporting successfully on its cached token (record: heartbeats still accepted, logs still flowing — this is the ~45-min blind window in action). Record as an observation with the exact TTL math (60m TTL − 15m lead) unless something disproves it.
  - **Part B (revocation bites at refresh/restart):** `docker compose restart <agent-svc>` — on startup the agent must refresh → 401; record its exact behavior (crash loop? clean error? retry storm?) and the run's fate: the restart abandoned the run; startup-reconcile can't authenticate — so who fails the run, and when? (Expect the stuck-run reaper at ~90-120s; a run stuck Running forever = major I1.)
  - **Part C (disable vs revoke):** repeat Part B's restart probe with a fresh agent+run using `agent identity disable` instead; record the API rejection text difference (`403 agent identity disabled`).
  - Recording: run stuck Running forever = major (I1); status/audit contradiction = major (I7); the 45-min blind window = observation (known TTL design), but record it prominently — it is the fleet's revocation SLA.
- [ ] **Step 2: Commit runbook.** **Step 3: Execute.** **Step 4: Findings + teardown `-v` + commit** (scenario id w1-6).

---

### Task 9: W1 checkpoint

**Files:**
- Modify: `test/edgecase/FINDINGS.md`

- [ ] **Step 1:** Append `## Checkpoint: W1 complete` following the W0 checkpoint's format and its classification rule: scenarios run (w1-1..w1-6; w1-7 deferred per spec), violations vs observations with severity breakdown, impact on later waves (at minimum: whether the reaper/lock-release behavior observed here changes W2-3's boundary-timing plan, and whether the nginx-block tooling suffices for W2's partition needs).
- [ ] **Step 2:** Commit (`test(edgecase): record W1 checkpoint`, trailer).
- [ ] **Step 3:** Report the checkpoint to the operator in chat (wave summary; fixes still wait for the final batch).
