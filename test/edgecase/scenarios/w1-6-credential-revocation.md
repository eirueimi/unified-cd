# W1-6 — agent credential revocation mid-run

- **Invariants:** I6 (zombie containment — *measure and document*, explicitly
  not pass/fail per the campaign spec), I7 (state display consistency).
  I1 (run accounting) is watched as a pass/fail side condition: a run left
  `Running` forever after its agent is de-credentialled is a major I1
  violation.
- **Stack:** plain `test/ha` compose, **no overlay** — the fault is injected
  through the product's own admin API, not through infrastructure. Every
  compose call in this runbook uses:

  ```bash
  cd test/ha
  docker compose -f docker-compose.ha.yaml up -d --build
  ```

- **Workload:** `test/edgecase/workloads/longrun.payload.json` — job
  `edge-longrun`, one native step `tick`:
  `for i in $(seq 1 300); do echo "tick $i"; sleep 1; done` — 300 lines at
  ~1/s over ~300s. Two properties matter here: the step is long enough to
  still be executing through a full reap cycle plus a restart, and it **has
  no dependency on controller reachability** — it keeps ticking whether or
  not the agent can authenticate to anything. Unlike W1-5's `sideeffect`
  workload it writes to stdout, so the log pipeline (and therefore the
  agent's authenticated `logs/bulk` calls) is exercised continuously.
  - **Known workload caveat (same as W1-5):** `UNIFIED_RUN_ID` is **not**
    injected into native steps, so nothing inside the step output identifies
    the run. Correlation is by run id from the API plus wall-clock time.

- **Injection mechanism:** the `unified-cli` admin verbs, run inside the
  `agent-enroll` service (it is the only service in this compose that has
  both the Go toolchain and `UNIFIED_SERVER`/`UNIFIED_TOKEN` in its
  environment). `-buildvcs=false` is required — this is a linked git
  worktree and `go run` otherwise fails the VCS stamp:

  ```bash
  docker compose -f docker-compose.ha.yaml run --rm agent-enroll \
    go run -buildvcs=false ./cmd/unified-cli agent identity revoke-credentials agent1
  docker compose -f docker-compose.ha.yaml run --rm agent-enroll \
    go run -buildvcs=false ./cmd/unified-cli agent identity disable agent2
  docker compose -f docker-compose.ha.yaml run --rm agent-enroll \
    go run -buildvcs=false ./cmd/unified-cli agent identity get agent1
  ```

  The first `run --rm` pays a one-off ~60s Go module download; budget for it
  before starting a timed observation window.

## Verified API/mechanism (do not re-derive)

Read this section before running the scenario — the task brief's stated
premise for Part A is **contradicted by the code**, and the runbook is
written to test the premise, not to assume it.

- **Brief's premise (the hypothesis under test):** "access-token TTL 1h with
  lazy refresh 15 min before expiry → revocation does NOT bite a live agent
  quickly", predicting a ~45-minute blind window during which a revoked
  agent keeps working on its cached token.
- **What the code actually says.** The two halves of that premise are both
  true in isolation but do not combine into a blind window, because the
  controller does **not** trust the token's own expiry — it re-reads the
  credential row from Postgres on *every* agent API request:
  - `agentAccessTTL = time.Hour`, `agentRefreshTTL = 30 * 24 * time.Hour`,
    `agentRefreshOverlap = 5 * time.Minute`
    (`internal/controller/api_agent_enrollment.go:21-23`); the agent's
    `tokenRefreshLeadTime = 15 * time.Minute` plus up to
    `maxCredentialJitter = 5 * time.Minute`
    (`internal/agent/credentials.go:27-28`), and `Token()` returns the cached
    access token while `now + lead + jitter` is still before `accessExpires`
    (`credentials.go:140`). So the *client* really does hold a token for
    ~40-45 min before refreshing.
  - But `agentAuth` (`internal/controller/agent_auth.go:62-77`) calls
    `store.GetAgentCredentialForAuth(parsed.ID)` per request and rejects on
    `credential.Status != "active" || credential.RevokedAt != nil ||
    !credential.ExpiresAt.After(time.Now())`. There is no in-memory
    credential cache on the server side (`s.credentialTouches` only rate-limits
    the `last_used_at` write, not the auth lookup).
  - `RevokeAgentIdentityCredentials`
    (`internal/store/postgres_agent_auth.go:467-489`) is
    `UPDATE agent_credentials SET revoked_at = COALESCE(revoked_at, NOW())
    WHERE identity_id = $1` — **every** credential of the identity, access and
    refresh alike, in one transaction.
  - `SetAgentIdentityEnabled(..., false)`
    (`postgres_agent_auth.go:437-465`) sets `agent_identities.status =
    'disabled'` **and** runs the same blanket credential revoke.
  - **Therefore the predicted behavior is the opposite of the brief's:**
    revocation should bite on the agent's very next authenticated call
    (sub-second at a 15s heartbeat cadence), not in ~45 minutes. Record what
    actually happens; if the observation matches the code, the brief's
    premise is disproved and the "45-minute blind window" observation must
    **not** be written up as if it had been seen.
- **Status codes the agent should see, and where they differ.** This is the
  Part C question, and the difference is *not* where the brief expects it:
  - **Agent API path** (heartbeat, claim, logs, step/finish report — anything
    under `agentAuth`): a revoked credential surfaces as
    `ErrAgentCredentialNotFound` and a disabled identity as
    `ErrAgentIdentityDisabled` (`postgres_agent_auth.go:286-291`), and
    `agent_auth.go:64-70` maps **both** to a bare **`401 unauthorized`**. So
    revoke and disable are *indistinguishable* on this path.
  - **Credential path** (`POST /api/v1/agents/token/refresh`, i.e. what a
    restarting or refreshing agent calls): `RotateAgentRefresh` checks
    identity status *before* the credential (`postgres_agent_auth.go:349-352`),
    so a **disabled** identity yields `ErrAgentIdentityDisabled` ->
    `respondAgentCredentialError` -> **`403 "agent identity disabled"`**
    (`internal/controller/api_agent_enrollment.go:496-500`), whereas a merely
    **revoked** credential yields `ErrAgentCredentialNotFound` -> **`401
    unauthorized`** (`api_agent_enrollment.go:508-512`). The
    `403 agent identity disabled` string the brief asks for exists, but only
    on the refresh/enroll endpoint — so **Part C's difference is only
    observable on restart**, not while the agent is live.
- **What the agent does with those codes.**
  - `Client.do` mints an `HTTPError` for **any** status `>= 400`
    (`internal/agent/client.go:107-108`), and `retryUntilSuccess` treats every
    `HTTPError` with `StatusCode < 500` as permanent and returns without
    retrying (`internal/agent/retry.go:33-37`). A `401` therefore abandons
    `ReportStep` / `SetRunOutputs` / `FinishRun` outright. **This is the same
    `if` W1-5 already filed a major against** — see the overlap note below.
  - There is a 401-triggered token invalidation path
    (`client.go:96-105`: on a `GET` that 401s, invalidate the token source and
    replay once), but it is guarded by a type assertion to the unexported
    `invalidatingTokenSource` interface (`client.go:37-42`) and
    **`*CredentialManager` does not implement `Invalidate()`** — `grep -rn
    "Invalidate()" internal/agent` matches only `client.go` and
    `client_test.go`'s fake. So for the real agent this path is dead code and
    a live 401 never causes a re-authentication attempt. Verify this claim
    against the live agent log rather than asserting it from the grep alone.
  - On **restart**, `main` builds a `CredentialManager` and calls
    `EnsureIdentity` first (`cmd/unified-cd-agent/main.go:158-172`); with
    `--id agent1` set and a credential file present this resolves locally with
    no network call. The first network call is `Agent.Run`'s `Register`,
    wrapped in `retryUntilSuccess` (`internal/agent/agent.go:185-195`). A
    non-retryable credential error there sets `registerErr`, `Run` returns it,
    and `main` does `slog.Error("agent run", ...); os.Exit(1)`
    (`main.go:219-221`). `docker-compose.ha.yaml` sets **no** `restart:` policy
    on `agent1`/`agent2`, so the expected shape is a **single clean exit(1),
    not a crash loop** — confirm which it is.
- **Who fails the run after the agent stops heartbeating.** Same two
  mechanisms W1-5 mapped, unchanged:
  - **Stuck-run reaper** (`internal/controller/stuckrun_reaper.go`, wired at
    `cmd/controller/main.go:404` as
    `RunStuckRunReaper(ctx, st, 30*time.Second, 90*time.Second, 60*time.Second)`):
    ticks every 30s, `ListStuckRunIDs(staleAfter=90s, grace=60s)`
    (`internal/store/postgres.go:1216`) matches `status='Running' AND
    claimed_at < NOW()-60s AND (agent missing OR last_seen_at < NOW()-90s)`,
    and each hit is `failOrphanedRun` -> `MarkRunFinished(id, Failed)`.
    **Failed, never re-queued.** Expected reap ≈ T_revoke + 90s .. +120s.
  - **Heartbeat reconcile** (`internal/controller/api_agent.go:99-124`) needs a
    *successful, authenticated* heartbeat to fire — which a de-credentialled
    agent cannot make — so it is out of play here until/unless the agent is
    re-enrolled. Note this is the opposite of W1-5's addendum, where the agent
    healed and the reconcile is what settled the run.
- **Overlap with W1-5 — read before filing anything.** W1-5 already filed a
  **major** against `internal/agent/retry.go:33-36` +
  `internal/agent/client.go:107-108` (see FINDINGS.md, "W1-5 — a transient 4xx
  from an intermediary makes the agent permanently abandon step/finish
  reporting"). Part B/C here drive the **same `if`**, but with a
  **legitimately permanent, controller-originated** 401/403 rather than
  W1-5's illegitimate intermediary-minted 403. **Do not re-file the retry.go
  classification bug.** W1-6's job is the complementary half: given the 401 is
  genuinely permanent, is abandoning the report the *right* behavior, and what
  run/agent state does it leave behind? Cross-reference the W1-5 entry
  explicitly.

## Baseline (plain stack, before any injection)

```bash
cd test/ha
docker compose -f docker-compose.ha.yaml up -d --build
curl -fsS localhost:18080/readyz            # expect: ok (retry until up)
```

BASELINE GATE — confirm all three before injecting anything:

```bash
curl -s -o /dev/null -w '%{http_code}\n' localhost:18080/readyz     # expect 200
for c in controller1 controller2 controller3; do
  echo -n "$c: "
  docker compose -f docker-compose.ha.yaml logs $c 2>/dev/null | grep -c "scheduler became leader"
done                                                                 # expect exactly one controller with >=1
curl -fsS localhost:18080/api/v1/agents -H "Authorization: Bearer ha-admin-token"
# expect agent1 AND agent2, both with a recent lastSeenAt
```

This scenario needs **two** healthy agents: Part A/B burn `agent1`, Part C
burns `agent2`, and each part needs its victim to be the claiming agent.
If baseline is broken, STOP and report BLOCKED with the evidence above.

Apply the job and confirm the identity is readable:

```bash
curl -fsS -X POST localhost:18080/api/v1/jobs \
  -H "Authorization: Bearer ha-admin-token" -H "Content-Type: application/json" \
  --data-binary @../edgecase/workloads/longrun.payload.json      # expect 200

docker compose -f docker-compose.ha.yaml run --rm agent-enroll \
  go run -buildvcs=false ./cmd/unified-cli agent identity get agent1
# expect: Status: active   (this also warms the Go module cache)
```

## Part A — is revocation lazy? (bounded observation, ~5 minutes)

Trigger a run, find its claiming agent, revoke that agent's credentials, and
watch for 5 minutes. **5 minutes is deliberately enough**: the brief's
hypothesis predicts nothing changes for ~45 min, the code predicts breakage
within one heartbeat interval (15s). Five minutes discriminates the two by a
factor of 20. **Do not soak for 45 minutes** — if the agent is still healthy
at +5 min, say so and extrapolate the TTL math explicitly rather than waiting
it out.

```bash
SCRATCH=/path/to/scratchpad/w1-6
mkdir -p "$SCRATCH"

date -u +%FT%T.%3NZ
curl -fsS -X POST localhost:18080/api/v1/runs \
  -H "Authorization: Bearer ha-admin-token" -H "Content-Type: application/json" \
  -d '{"jobName":"edge-longrun"}'
# capture .id as RUN_ID

curl -fsS "localhost:18080/api/v1/runs/$RUN_ID" -H "Authorization: Bearer ha-admin-token"
# record: status ("Running"), claimedBy, claimedAt
```

If `claimedBy` is not `agent1`, either re-trigger until it is or swap the
victim/control agent names throughout — Part C needs the *other* agent intact.

```bash
date -u +%FT%T.%3NZ     # T_revoke
docker compose -f docker-compose.ha.yaml run --rm agent-enroll \
  go run -buildvcs=false ./cmd/unified-cli agent identity revoke-credentials agent1 \
  2>&1 | tee "$SCRATCH/revoke.txt"
# expect: agent identity "agent1" revoke-credentials
```

Confirm the injection is real from the agent's own side before trusting the
timeline (a no-op revoke would silently produce a "nothing happened" result
that looks exactly like the brief's predicted blind window):

```bash
docker compose -f docker-compose.ha.yaml logs --since 60s agent1 | tail -30
# look for: heartbeat / claim / log-push failures citing http 401
```

Observation loop — keep the raw output; every number in the findings must come
from it:

```bash
for i in $(seq 1 20); do
  echo "=== $(date -u +%FT%T.%3NZ)"
  curl -s "localhost:18080/api/v1/runs/$RUN_ID" -H "Authorization: Bearer ha-admin-token" \
    | tr ',' '\n' | grep -E '"status"|"updatedAt"|"claimedBy"'
  curl -s "localhost:18080/api/v1/runs/$RUN_ID/logs/stats" -H "Authorization: Bearer ha-admin-token"
  echo
  curl -s localhost:18080/api/v1/agents -H "Authorization: Bearer ha-admin-token" \
    | tr ',' '\n' | grep -E '"id"|lastSeenAt'
  sleep 15
done 2>&1 | tee "$SCRATCH/partA-timeline.txt"
```

Record, on that timeline:

1. **T_revoke** and the first `401` in `agent1`'s log (revocation latency =
   difference; expect ≲ one 15s heartbeat interval).
2. **Whether `lastSeenAt` for `agent1` freezes** — it can only advance on a
   successful authenticated heartbeat, so a frozen `lastSeenAt` is the direct
   proof that revocation bit.
3. **Whether `logs/stats` keeps growing** — this is the honest test of "keeps
   reporting successfully". If `count` freezes, the cached token is *not*
   buying the agent a blind window.
4. **Reap** — `"stuck-run reaper: failed orphaned run (agent lost)"` with the
   matching `runId`, and the run's own flip to `Failed`:

   ```bash
   docker compose -f docker-compose.ha.yaml logs controller1 controller2 controller3 \
     > "$SCRATCH/controllers.log"
   grep -n "stuck-run reaper" "$SCRATCH/controllers.log"
   ```

   Compute reap latency from T_revoke and compare against the 90s + 30s bound.
5. **Zombie window (I6)** — whether the `tick` step is still executing after
   the run reads `Failed`, and for how long:

   ```bash
   docker compose -f docker-compose.ha.yaml exec -T agent1 ps -ef | grep -c "seq 1 300"
   ```

   The step should keep running to its full ~300s regardless, because nothing
   can reach the agent to stop it and the cancel poller is itself 401ing.
   Capture the check to a file — an interactive-only `ps` read has been called
   out as non-reverifiable in earlier findings.

## Part B — revocation at restart: who fails the run, and when?

Restart the de-credentialled agent. On startup it must obtain a token, which
means a refresh exchange against a revoked refresh credential.

```bash
date -u +%FT%T.%3NZ     # T_restart
docker compose -f docker-compose.ha.yaml restart agent1
sleep 20
docker compose -f docker-compose.ha.yaml ps -a agent1        # Up? Exited(1)? Restarting?
docker compose -f docker-compose.ha.yaml logs --since 3m agent1 | tail -40
```

Record precisely:

- the exact startup failure text and status code (expect `401` on
  `/api/v1/agents/token/refresh`, surfaced as
  `"permanent credential error, giving up retry"` from `retry.go:38-42`, or as
  `"resolve agent identity"` if `EnsureIdentity` is what fails first);
- **crash loop vs clean exit vs busy retry** — check `ps -a` for the container
  state and the restart count, and `logs` for repeated startup attempts.
  Compose sets no `restart:` policy, so a loop here would mean the agent
  process itself is looping internally;
- the **run's fate**: the restart abandoned the in-flight run (the process is
  gone), and startup-reconcile (`Client.ReconcileRuns`,
  `internal/agent/agent.go:204-208`) cannot authenticate — so the only actor
  left that can settle the run is the stuck-run reaper. Confirm it does, and
  when:

  ```bash
  grep -n "stuck-run reaper" "$SCRATCH/controllers.log"
  curl -s "localhost:18080/api/v1/runs/$RUN_ID" -H "Authorization: Bearer ha-admin-token"
  ```

  **A run left `Running` forever here is a major I1 violation** — that is the
  pass/fail this part exists for.
- the **step_reports** row for the run, which W1-5 already established goes
  stale on every orphan-fail path:

  ```bash
  docker compose -f docker-compose.ha.yaml exec -T postgres psql -U unified -c \
    "SELECT run_id, step_index, step_name, status, exit_code, started_at, ended_at FROM step_reports;" \
    > "$SCRATCH/db-final.txt" 2>&1
  docker compose -f docker-compose.ha.yaml exec -T postgres psql -U unified -c \
    "SELECT id, job_name, status, claimed_by, claimed_at, updated_at FROM runs ORDER BY created_at;" \
    >> "$SCRATCH/db-final.txt" 2>&1
  ```

## Part C — `disable` vs `revoke-credentials`

Repeat Part B's restart probe against a **fresh** agent and run, using
`disable` instead of `revoke-credentials`. Per the mechanism notes above the
two are identical on the agent API path (both `401`) and differ only on the
refresh endpoint (`403 "agent identity disabled"` vs `401 unauthorized`), so
the restart is the part that actually discriminates them.

```bash
date -u +%FT%T.%3NZ
curl -fsS -X POST localhost:18080/api/v1/runs \
  -H "Authorization: Bearer ha-admin-token" -H "Content-Type: application/json" \
  -d '{"jobName":"edge-longrun"}'
# capture .id as RUN2_ID; confirm claimedBy == agent2

date -u +%FT%T.%3NZ     # T_disable
docker compose -f docker-compose.ha.yaml run --rm agent-enroll \
  go run -buildvcs=false ./cmd/unified-cli agent identity disable agent2 \
  2>&1 | tee "$SCRATCH/disable.txt"

docker compose -f docker-compose.ha.yaml logs --since 60s agent2 | tail -20   # live-path status code
date -u +%FT%T.%3NZ
docker compose -f docker-compose.ha.yaml restart agent2
sleep 20
docker compose -f docker-compose.ha.yaml ps -a agent2
docker compose -f docker-compose.ha.yaml logs --since 3m agent2 | tail -40    # restart-path status code
```

Also read the identity back both ways, so the operator-visible difference is
on the record:

```bash
docker compose -f docker-compose.ha.yaml run --rm agent-enroll \
  go run -buildvcs=false ./cmd/unified-cli agent identity get agent1   # revoked  -> Status: ?
docker compose -f docker-compose.ha.yaml run --rm agent-enroll \
  go run -buildvcs=false ./cmd/unified-cli agent identity get agent2   # disabled -> Status: ?
```

Record the API rejection text difference on both paths, and whether an
operator can tell revoked-from-disabled by (a) the agent's own log, (b)
`agent identity get`, (c) `GET /api/v1/agents`.

## Recording (severity guidance)

- Run stuck `Running` forever (no reap, no reconcile, nothing settles it) =
  **major (I1)**. This is the primary pass/fail.
- Run status / approval status / audit row contradiction = **major (I7)**.
- The agent's *live* behavior after revocation (does it keep executing? for
  how long? does it keep side-effecting?) = **measured observation (I6)** —
  the campaign spec is explicit that I6 is document-don't-judge.
- If the ~45-min blind window is **observed**, it is an observation (known TTL
  design) but must be recorded prominently as the fleet's revocation SLA. If
  it is **disproved** — the expected outcome given `agent_auth.go:62-77` —
  record that instead, with the measured revocation latency, and state plainly
  that the brief's premise did not hold. Do **not** write up an unobserved
  45-minute window as if it had been measured.
- `retry.go:33-36` abandoning the report on a 401 = **already filed by W1-5**;
  cross-reference, do not re-file. Anything *new* about the state that
  abandonment leaves behind under a legitimate revocation is fair game.
- A stale `step_reports` row under a terminal run = **already filed by W1-5**
  as minor I7; note recurrence as corroboration only.
- An agent that crash-loops (rather than exiting once) after revocation,
  producing unbounded log/CPU churn or unbounded failed-auth load on the
  controller, = **minor (I5, diagnosability/bounded recovery)** unless the
  volume is severe enough to constitute a self-inflicted DoS, in which case
  argue the upgrade explicitly.
- Revocation that does **not** bite at all (agent keeps reporting
  successfully indefinitely) = **major security-adjacent finding**; a
  revocation verb that does not revoke is a broken contract regardless of TTL
  design. Only file this if actually observed.

## Execution notes (added after the 2026-07-29 run — read before re-running)

- **The Part A hypothesis was disproved, as the mechanism section predicted.**
  Revocation is effectively immediate: `agent_credentials.revoked_at` and the
  agent's first `401` were 4.95s apart for `revoke-credentials` and 1.51s
  apart for `disable`, and both intervals are the agent's own poll cadence,
  not a server-side grace. `lastSeenAt` froze *before* the revocation instant
  and never advanced again. Do not re-run this expecting a 45-minute window;
  the 5-minute observation is more than sufficient and could safely be cut to
  ~3 minutes (long enough to cover the reap at +90-120s).
- **Victim agents are whichever agent claims — do not assume.** In this run
  the first run landed on `agent2`, so Part A/B burned `agent2` and Part C
  burned `agent1`, the reverse of the order written above. Everything works
  either way; just keep one healthy agent for Part C.
- **The restart failure lands in `EnsureIdentity`, not in `Agent.Run`'s
  `Register`.** The runbook's mechanism section predicted `Register`, on the
  reasoning that `--id` plus a persisted credential resolves the identity
  locally. It does not: the `enrollment.token` file written by `agent-enroll`
  is still present, so `Token()` takes the enroll-first branch
  (`internal/agent/credentials.go:143-170`), gets `401` on
  `/api/v1/agents/enroll`, logs `WARN "enrollment token rejected (expired or
  already consumed); continuing with the existing credential"`, then fails the
  refresh. Net observable output is exactly two lines and `os.Exit(1)`; the
  `WARN` is a red herring and should not be read as the cause.
- **Take the zombie `ps` check *during* the window, not after.** This run's
  `docker compose exec -T agent2 ps -ef` landed ~60s after the step's loop had
  already ended, so it corroborates nothing on its own. Either poll `ps` on
  the same 15s cadence as the status loop, or rely (as the findings do) on the
  step-end `"permanent error, giving up retry"` timestamps, which pin the end
  of execution precisely.
- **The reap always precedes the restart, so Part B's "who fails the run?"
  is really answered by Part A.** The reaper's clock starts at the last
  successful heartbeat, which stops at revocation, so the run is reaped
  ~90-120s later regardless of when (or whether) the agent is restarted.
  Measured: +106.9s (Part A) and +93.8s (Part C) from the revocation instant.
  If a future variant wants to observe a restart racing a *still-Running* run,
  it must restart within ~90s of revoking.
- **Budget ~12s per `docker compose run --rm agent-enroll` invocation, every
  time.** Each `run --rm` is a fresh container and the Go module cache is not
  on a volume, so the module download repeats on every call — it is not a
  one-off first-run cost. Issue the `date -u` stamp immediately around the
  command and take the authoritative injection instant from
  `agent_credentials.revoked_at` / `agent_identities.disabled_at` in Postgres
  rather than from the CLI's own wall-clock, which is up to ~13s late.
- **Where to read the ground truth.** The three most load-bearing reads, all
  cheap: `SELECT i.agent_id, i.status, c.kind, c.revoked_at FROM
  agent_credentials c JOIN agent_identities i ON i.id=c.identity_id;` for the
  injection instant, `SELECT occurred_at, actor, action, resource, status FROM
  audit_logs ORDER BY occurred_at;` for the `401`/`403` split, and
  `docker compose exec -T <controller> wget -qO- http://localhost:8080/metrics
  | grep agent_auth` for `unifiedcd_agent_auth_events_total`, which is the only
  place `reason="disabled"` is distinguished from `reason="invalid"`.

## Teardown

```bash
docker compose -f docker-compose.ha.yaml down -v
docker compose -f docker-compose.ha.yaml ps -a          # verify nothing left running
```

`-v` matters more than usual here: the `agent-credentials` named volume holds
the per-agent credential files, and both victims' identities are left revoked
/ disabled in Postgres. Leaving either behind makes the next scenario's
`agent-enroll` bootstrap skip re-enrollment (`if [ -f "$DIR/credentials.json" ]`)
and start agents that can never authenticate.
