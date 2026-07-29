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
