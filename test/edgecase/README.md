# Edge-Case Test Campaign

Exploratory testing of unified-cd's distributed-systems edge cases.
Spec: `docs/superpowers/specs/2026-07-29-edge-case-testing-design.md`
(waves W0-W6, invariants I1-I7, findings workflow).

## Layout

- `FINDINGS.md` — one entry per invariant violation or notable observation.
- `scenarios/` — one runbook per scenario (`w<wave>-<n>-<slug>.md`).
- `compose/` — overlay files stacked onto `test/ha/docker-compose.ha.yaml`.
- `workloads/` — job/schedule YAML (and pre-encoded JSON API payloads).
- Scheduler/timing probes live next to the code they probe (e.g.
  `internal/controller/`), gated by the `edgeprobe` build tag — not a
  standalone `probes/` directory; see "Running probe tests" below.

## Tools

- `tools/inject.sh <cmd> <service>` — fault injection (kill/pause/partition/
  nginx-block/steplock). Run from `test/ha/`.

  `steplock <agent>` / `steplock-clear` are **URI-scoped**: they deny only
  `POST /api/v1/agents/<agent>/steps` (403) and leave every other endpoint for
  that agent working — notably
  `POST /api/v1/agents/<agent>/runs/<runId>/children`. They require the
  `compose/steplink.override.yaml` overlay (`nginx-steplink.conf`, which gives
  each agent's step-report path an exact-match `location` with its own include
  dir) and are a no-op against `nginx-edge.conf`, so always check the response
  code after arming. Introduced by W2-5 to reach a sub-10 ms code window
  deterministically instead of racing it; the locations are per-agent-id and
  must be extended by hand for a third agent.
- `tools/bulk-submit.sh <job-name> <count>` — submits `count` runs of an
  already-applied job and prints one run id per line (progress on stderr, so
  `| tee ids.txt` captures ids only). Honors `UNIFIED_SERVER`
  (default `http://localhost:18080`) and `UNIFIED_TOKEN` (default
  `ha-admin-token`). Used by W2-9 to push >50 runs into `Pending`.

  The trigger endpoint is `POST /api/v1/runs` with body `{"jobName":"..."}`
  (`internal/controller/server.go:370` → `handleTriggerRun`,
  `api_runs.go:22-42`), which returns the full `api.Run` JSON — **there is no
  `/api/v1/jobs/<job>/trigger` route**.

## Workload fixtures

Every `*.payload.json` is the pre-encoded `{"yaml":"..."}` body for
`POST /api/v1/jobs`. All are `agentSelector: [kind:linux]`, and all are
`native: true` **except `podcap-job.payload.json`**, which carries a
Kubernetes-only `podTemplate` so its inferred capability is `pod` (see the
table).

| File | Job | Purpose |
|---|---|---|
| `tick.payload.json` | `edge-tick` | trivial run |
| `longrun.payload.json` | `edge-longrun` | long-lived run for reaper timing |
| `approval.payload.json` | `edge-approval` | approval gate, 10-minute timeout |
| `sideeffect.payload.json` | `edge-sideeffect` | mutex `edge-mutex` holder, writes `/data/sideeffect.log` |
| `mutex-successor.payload.json` | `edge-mutex-successor` | mutex `edge-mutex` successor probe (I3) |
| `schedule-every-minute.payload.json` | — | schedule fixture |
| `call-parent.payload.json` | `edge-call-parent` | 20s `prelude` then a `call:` step invoking `edge-call-child` (W2-2, W2-5) |
| `call-child.payload.json` | `edge-call-child` | ~90s child, timestamped markers to `/data/child.log` so an orphaned child stays observable after its parent dies |
| `approval-short.payload.json` | `edge-approval-short` | `before` → `gate` (`timeoutMinutes: 0.5` = **30s**) → `after` (W2-8) |
| `mutex-hog.payload.json` | `edge-mutex-hog` | mutex `edge-mutex` lock holder, sleeps 600s (W2-9) |
| `unrelated-probe.payload.json` | `edge-unrelated-probe` | **no mutex**, `echo probe-ran` — the W2-9 starvation probe |
| `podcap-job.payload.json` | `edge-podcap-job` | `podTemplate` with a pod-level `nodeSelector`, so `dsl.RequiredCaps` infers **`pod`** — label-claimable (`kind:linux`) but capability-unschedulable, because the `test/ha` agents report `["native","container"]` (W2-4 Part D) |

### Fractional `timeoutMinutes` — verified, do not re-derive

`approval.timeoutMinutes` is `float64` (`internal/dsl/types.go:341`) and
**fractional values round-trip end to end**; `approval-short.payload.json`
therefore uses `0.5`, giving a **30-second** approval window (not 60s):

- Parse + `Validate()` accept it — nothing in `internal/dsl` constrains the
  value (no minimum, no integer check); `dsl.Parse` decodes `0.5` to `0.5`.
- `buildClaimResponse` (`internal/controller/api_agent.go:436-440`) only
  substitutes the default when `timeout == 0`, so `0.5` passes through.
- The agent computes `time.Duration(timeoutMin*60) * time.Second`
  (`internal/agent/approval.go:48`) → exactly 30s.
- The controller computes `time.Now().Add(time.Duration(req.TimeoutMinutes *
  float64(time.Minute)))` (`internal/controller/api_approvals.go:87`) → also
  exactly 30s, so `timeout_at` and the agent deadline agree.

Caveat: the agent's `time.Duration(timeoutMin*60)` truncates to whole
seconds, so values finer than 1/60 minute lose resolution. `0.5` is exact.
The controller's approval reaper still ticks at **1 minute**
(`cmd/controller/main.go:403`), so a row can sit `Pending` up to ~60s past a
30s `timeout_at` — the agent-local deadline is the one that fires first.

## Raw evidence

`FINDINGS.md` cites captures by relative name (`w1-5/agent1.log`,
`w1-6/metrics.txt`, ...). Those names resolve against the campaign's evidence
root, which is **not in this repository**:

    <project parent>/edgecase-evidence/

i.e. a sibling of the checkout, so it survives worktree removal and
`git clean -fdx`. It holds ~9 MB of container logs, psql output, API reads and
metrics scrapes — too bulky and too raw to commit, but it is what every
numeric claim in `FINDINGS.md` is derived from. See its own `README.md` for
per-wave coverage; coverage is uneven, and the entries that rest on
un-captured observations say so inline.

While running a scenario, capture to the session scratchpad (fast, disposable)
and copy the wave's directory into the evidence root at the wave checkpoint.

## Running a compose scenario

Each runbook lists its exact stack invocation. The general shape:

    docker compose -f test/ha/docker-compose.ha.yaml \
      -f test/edgecase/compose/<overlay>.yaml up -d --build

The LB is `http://localhost:18080`, admin token `ha-admin-token`
(both inherited from the test/ha stack).

## Running probe tests

Scheduler/timing boundary probes are observational Go tests excluded from
normal builds by the `edgeprobe` build tag. They live next to the code they
probe (they call unexported functions), e.g.
`internal/controller/edgeprobe_scheduler_test.go`:

    go test -tags edgeprobe ./internal/controller -run TestEdgeProbe -v

Probes PASS unless infrastructure breaks; their `t.Logf` output is the
result. Copy notable output into `FINDINGS.md`.

## Rules

- Phase 1 is exploration: record findings, do NOT fix production code.
- Findings are reported in one batch after the final wave.
- Every scenario names the invariants (I1-I7) it attacks.
