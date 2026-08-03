# Edge-Case Campaign: Wave W5 (Mixed-Version Upgrade) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Execute Wave W5 — the campaign's last — recording what breaks when unified-cd runs at two
versions at once. **Most of the wave is a code-read audit; exactly one scenario is executed.**

**Architecture:** Same recording pattern as W0-W4 and W6. Findings appended to
`test/edgecase/FINDINGS.md`, one runbook under `test/edgecase/scenarios/`, raw captures to the
session scratchpad and archived at the checkpoint.

## Read this before planning anything: the wave is deliberately small

Reconnaissance (read-only, 2026-08-01) established that **three of the design doc's four W5
sub-scenarios are already answered by reading**. Running them would produce confirmations, not
findings, and would cost two full controller image builds (npm + vite + Go) on a laptop. The user
designated W5 the campaign's lowest priority. **This plan therefore replaces most of the wave with a
code-read audit and keeps one executed scenario.**

Each claim below is code-read unless labelled otherwise. Six consecutive waves say the `file:line`
claims will hold and the *mechanism* claims may not — **treat all of it as claims to check.**

### 1. There is no version boundary — only a version label nobody reads

The whole lifecycle: the agent self-reports `Version` (`internal/agent/version.go:5` →
`AgentRegisterRequest.Version`, `internal/api/types.go:89` → `internal/controller/api_agent.go:60` →
the `agents.version` column → the list-agents response). **The controller has no version variable at
all** — no `internal/controller/version.go`, and it never reports a version on any wire.

Untruncated negative surveys, repo-wide: `User-Agent|UserAgent|X-Agent-Version|agent_version|
AgentVersion` → **0 matches**. `semver|version\.Compare|MinVersion|version mismatch|incompatible`
(case-insensitive) → 63 hits, **not one a version comparison** (Go module `+incompatible` suffixes,
semver-*tag* caching in `internal/gittemplate`, a job template, DSL messages, prose). `/api/v1` is a
static prefix (`internal/controller/server.go:355`) that has never changed.

The real compatibility mechanism is **capabilities**: an agent reporting no capabilities is
capability-agnostic and matches on labels alone, explicitly so rolling upgrades don't strand runs
(`docs/agents.md:316-322`); an *unknown* capability string is rejected 400 (`:324-326`).

### 2. The documented rolling-upgrade guarantee is unfounded — this is the wave's main finding

`docs/operations.md:191` claims old and new controller binaries can both run against the
already-migrated schema *"as long as the migration is backward-compatible (additive columns/tables —
**this is the norm for unified-cd's migration history**)"*.

**Four of the last five migrations are backward-incompatible:**

| Migration | `file:line` | Operation |
|---|---|---|
| 003 | `003_appsource_managed_resources.up.sql:11` | `DROP COLUMN managed_jobs` |
| 005 | `005_matrix_variant.up.sql:2,6` | `DROP CONSTRAINT step_reports_pkey` / `step_outputs_pkey` |
| 014 | `014_agent_enrollment_policies.up.sql:23` | `DROP COLUMN access_token_ttl` |
| 014 | `:35-36` | `SET NOT NULL` **and `DROP DEFAULT`** on `access_token_ttl_seconds` |
| 015 | `015_secrets_v2.up.sql:11` | `DROP COLUMN controller_key_hex` (+ data destruction) |
| 015 | `:13`, `:15` | `DELETE FROM sessions`; `DROP COLUMN refresh_token` |
| 015 | `:16-19` | new NOT NULL columns, then **`DROP DEFAULT`** |
| 016 | `016_drop_secret_scope.up.sql:13` | `DELETE FROM secrets` (all of them) |
| 016 | `:15-17` | drop unique constraint, `DROP COLUMN scope`, `DROP COLUMN scope_ref` |

Only 017 is clean. All paths are under `internal/store/migrations/` — **write the full path**; a W6
entry was corrected for omitting it.

**Second limb:** `internal/store/verify.go:25-27` states a rule — *"A later migration must never drop
or rename a sentinel object"* — that **no test enforces**; `TestSchemaSentinelsCoverAllMigrations`
only checks one-sentinel-per-migration (`verify.go:21-23`).

**Checked and refuted as a scenario:** the sentinel guard does **not** fire for the tag pairs
available. `v0.3.0`'s sentinel list (1-12) survives migrations 013-017 intact, so **a v0.3.0 binary
boots against a HEAD schema and `verifySchema` passes.** The false-drift hazard is latent, not live —
do not plan a scenario around it. What breaks instead is runtime SQL (`42703 undefined_column`),
code-read-provable.

### 3. One real mixed-version-only availability defect, reachable only by reading

`golang-migrate v4.19.1`'s `m.lock()` covers the whole `Up()` (`migrate.go:265-283`), and the
postgres driver's `Lock()` is `SELECT pg_advisory_lock($1)` keyed identically across replicas,
session-scoped and blocking. **Three replicas booting simultaneously serialize safely — do not run
that scenario.**

But `verifySchema` runs **after** `m.Up()` returns, i.e. **after the advisory lock is released**
(`internal/store/postgres.go:137` then `:142`). So: replica A finishes `Up()` and unlocks → replica B
acquires the lock and sets `dirty=true` → replica A's `verifySchema` reads `dirty=true`
(`verify.go:55`, hard-fails `:62-68`) → `cmd/controller/main.go:258-259` → **`os.Exit(1)`**. There is
**no retry**, and `test/ha/docker-compose.ha.yaml` sets **no `restart:` policy** on the controllers.
The window opens **only during a mixed-version boot**.

**Do not build a rig to measure it.** Record it as a code-read finding with the `file:line` chain and
an explicit caveat that the window width was not measured. If it is ever to be measured, it belongs
in a Postgres-backed unit test (two `Migrate()` calls against one test DB with different embedded
migration sets), not a compose scenario.

### 4. What actually differs across the tags

Tags: v0.0.1/v0.0.2 (7/6), v0.0.3 (7/7), v0.1.0 (7/9), v0.2.0 (7/13), v0.2.1 (7/14), v0.3.0 (7/16),
v0.4.0 (7/20). **HEAD is 79+ commits ahead of v0.4.0.**

- **v0.2.1 → v0.3.0: additive only.** A v0.2.1 agent silently ignores `retry:` — a behaviour delta.
- **v0.3.0 → v0.4.0: the breaking release.** Auth replaced wholesale (migrations 013/014, new
  `api_agent_enrollment.go`); secrets scope removed from the wire; KEK moved out of the DB. **Cleanest
  wire-level N-1 break:** `AgentFetchSecretsRequest` gained a required `RunID`
  (`internal/api/types.go:268`), enforced at `internal/controller/api_secrets.go:90-91` (400 if
  empty) — a v0.3.0 agent sends none, so **every secret fetch is a hard 400.**
- **v0.4.0 → HEAD: tiny surface, two sharp edges.**
  1. **`DownloadArtifactStep.RunID` added, `omitempty`** (`internal/api/types.go:357-359`). A v0.4.0
     agent **ignores it and downloads from the current run instead of the specified one — silently,
     no error, run `Succeeded`.** A **silent wrong-data** divergence: the most valuable class in the
     wave.
  2. **The `call:` child-run endpoint moved** — v0.4.0 posts `POST /api/v1/runs`
     (`v0.4.0:internal/agent/client.go:218`); HEAD posts `.../runs/{runId}/children`
     (`internal/agent/client.go:251`, route absent from v0.4.0's server). At HEAD
     `POST /api/v1/runs` sits behind `ServerAuth` + `requireMinRole("developer")`
     (`server.go:355-370`). *(401/403 is inferred — routes read, request not made.)*
  3. **Legacy agent auth removed entirely** (`4e8f315`) — a HEAD controller cannot authenticate any
     pre-v0.4.0 agent at all.

### 5. Rig feasibility — why only the agent pair is executed

Compose **builds from source**: `controller1: &ctrl` with `build:`
(`test/ha/docker-compose.ha.yaml:79-81`), and `controller2`/`controller3` are **anchor aliases of the
same build** (`:103-104`). A second controller version = a second cold build with no layer sharing
(npm ci + vite + Go) — *inferred* 10-20 min. The agent image is much cheaper (single Go build).

`.github/workflows/release-docker.yml` pushes `ghcr.io/eirueimi/unified-cd-<image>:<tag>` on every
`v*` tag, so images for v0.0.1-v0.4.0 **should** exist — but whether those runs succeeded and whether
the packages are public is **not resolvable read-only**. Try the pull; fall back to building.

**Overlay pattern fits, with one trap.** `test/edgecase/compose/dupagent.override.yaml:67-88` is the
precedent: a brand-new service with `profiles:`, its own `build:`, sharing the credentials volume.
**Add a new service; do not mutate `controller3`** — it carries `build:` via the anchor, and Compose
merging an overlay's `image:` onto an existing `build:` builds-and-tags rather than switching to a
pull.

**Two constraints that bite:** a **v0.3.0 controller cannot use this rig unmodified** (it predates
`UNIFIED_CONTROLLER_KEY_FILE` and reads only `UNIFIED_CONTROLLER_KEY`, while the rig supplies the
former at `docker-compose.ha.yaml:84`); and a **v0.3.0 agent cannot enroll against a HEAD controller
at all**. So **the only viable mixed pair is v0.4.0-agent ↔ HEAD-controller** — and it is drop-in,
because v0.4.0's agent already accepts `--credential-file` and `--enrollment-token-file`
(`v0.4.0:cmd/agent/main.go:66-67`), the exact flags the rig passes.

## Explicitly inferred, not verified — treat as hypotheses

- That each migration file executes as one implicit Postgres transaction (depends on pgx-stdlib's
  protocol choice for a zero-arg `Exec`; pgx source not read). If it does not, a crash mid-file
  leaves a partially-applied migration plus `dirty=true`.
- That a v0.3.0 controller in this rig exits at startup on `EnsureControllerKey`.
- That a v0.4.0 agent's `POST /api/v1/runs` gets 401/403 from a HEAD controller.
- That the GHCR images exist and are publicly pullable.
- Build-time estimates for a second controller image.

## Global Constraints

- All committed text is English (AGENTS.md).
- Work on branch `plan/edge-case-w5` in worktree `wt-edge-spec` — never commit on the main checkout.
- **No production-code changes.** Test-only files under `test/edgecase/` and docs. Do not modify
  `manifests/`, `test/ha/`, or `test/edgecase/workloads/podcap-job.payload.json`. Rig changes are
  `test/edgecase/compose/` overlays.
- A **violation** contradicts an invariant (I1-I7) **by its own text** (`FINDINGS.md:1509`) **or** a
  statement in `docs/*.md`. The table is at
  `docs/superpowers/specs/2026-07-29-edge-case-testing-design.md:48-54`; I1 = `:48`, I4 = `:51`,
  I5 = `:52`. NOT contracts: an inline comment inside a function body, an unexported helper's doc
  comment, anything under `docs/superpowers/`. An **observation** says "observation" in its **title**
  and repeats it in its **Severity** line.
- **Severity must be argued from `FINDINGS.md:6-8`'s own text, never by analogy to a sibling entry.**
  W6 re-banded four entries under this rule; it is the campaign's most-enforced.
- Every number traces to a capture whose window covers it. Label derived / inferred / **code-read**.
  **Do not write "never" for a window you ended yourself.**
- **Never `head` a survey; always report the hit count.**
- **When you claim a class is fully enumerated, verify the enumeration.**
- **Before filing, grep `FINDINGS.md` for the finding itself**, not just the doc text.
- **An arm is verified when some capture measures its effect.**
- Scrub credentials from every capture. Kill every background process and capture its final output.
- **`FINDINGS.md` self-citations must keep resolving.** Appending is safe; if you edit above the
  append point, re-check every affected `FINDINGS.md:NNNN` across `test/edgecase/` and `docs/`.

---

### Task 1: The migration backward-compatibility audit — code-read, no rig

**Highest yield-per-minute in the wave. Run it first.**

**Files:** Create `test/edgecase/scenarios/w5-1-migration-compat-audit.md`; Modify
`test/edgecase/FINDINGS.md`

- [ ] **Step 1: Verify every row of the destructive-migration table above** against
      `internal/store/migrations/`, and **complete the enumeration** — the table lists the last five
      migrations plus 003/005; sweep **all 17** and report the full count of backward-incompatible
      operations, by class (column drop, constraint drop, NOT NULL without default, data deletion).
- [ ] **Step 2: Verify the contract.** Quote `docs/operations.md:191` verbatim from the file. Run an
      **untruncated** survey of `docs/` (excluding `docs/superpowers/`) for rolling-upgrade,
      compatibility and version-skew statements, and **report the hit count**. Reconciliation
      established 20 hits of which 8 are substantive — confirm or refute that.
- [ ] **Step 3: File the violation.** The documented rolling-upgrade guarantee is contradicted by the
      repository's own migration history. Argue the band from `FINDINGS.md:6-8`. Label the entry
      **code-read, not executed** — precedent for that filing exists at `FINDINGS.md:1864` (W3-6).
- [ ] **Step 4: File the unenforced-rule limb** (`verify.go:25-27` vs
      `TestSchemaSentinelsCoverAllMigrations`) — judge whether it is a second violation, an
      observation, or a Notes clause on the first, and say why.
- [ ] **Step 5: File the `verifySchema`-outside-the-lock race** as a code-read finding with the full
      `file:line` chain (`postgres.go:137` → `:142`, `verify.go:55`/`:62-68`, `main.go:258-259`, and
      the absent `restart:` policy). **State explicitly that the window width was not measured**, and
      record the Postgres-backed unit test as the right way to measure it if the campaign ever wants
      that. **Verify the "no `restart:` policy" claim by enumeration** — do not assert it.
- [ ] **Step 6: Record the refuted scenarios as results**, not as omissions: concurrent migration
      startup is safe by construction (advisory lock over the whole `Up()`); the sentinel guard does
      not fire for any available tag pair; and a v0.3.0 controller alongside HEAD was deliberately not
      built. Each with its `file:line`.
- [ ] **Step 7: Commit.**

---

### Task 2: Scenario W5-2 — a v0.4.0 agent against HEAD controllers

**The wave's one executed scenario. It targets silent wrong behaviour, which is why it survives the
cut.**

**Files:** Create `test/edgecase/scenarios/w5-2-mixed-agent.md`,
`test/edgecase/compose/mixedver.override.yaml`, fixtures as needed; Modify
`test/edgecase/FINDINGS.md`, `test/edgecase/README.md`

**Invariants:** I1 (`:48`), I4 (`:51`), I7 (`:54`) — each only if contradicted by its **own text**.

- [ ] **Step 1: Get a v0.4.0 agent.** Try `docker pull ghcr.io/eirueimi/unified-cd-agent:v0.4.0`
      first; if the package is private or the tag is missing, build from a `git worktree add
      ../wt-v040 v0.4.0`. **Record which route worked and what it cost** — the reconnaissance could
      not resolve this read-only and a future wave will want the answer.
- [ ] **Step 2: Write the overlay** as a **new service** (`agent-old`) following
      `compose/dupagent.override.yaml:67-88`'s shape, sharing the credentials volume. Do **not**
      mutate an existing service. v0.4.0's agent accepts `--credential-file` and
      `--enrollment-token-file`, the flags the rig already passes — verify that at the tag rather than
      trusting it.
- [ ] **Step 3: Baseline.** The old agent enrolls, claims, and runs a trivial job to `Succeeded`
      against HEAD controllers. **If this fails, the wave's executed half is blocked — say so
      immediately** rather than working around it.
- [ ] **Step 4: Part A — the silent wrong artefact.** `DownloadArtifactStep.RunID`
      (`internal/api/types.go:357-359`) is `omitempty` and a v0.4.0 agent ignores it. Build a job that
      downloads an artefact from a **named other run**, run it on the old agent and on a HEAD agent,
      and compare what lands in the workspace. **Predicted: the old agent silently downloads the
      current run's artefact and reports `Succeeded`.** Attacks **I4** (artifact integrity) and
      **I7** (state display consistency) — but check each against its own text before citing it.
- [ ] **Step 5: Part B — the moved `call:` endpoint.** A v0.4.0 agent posts `POST /api/v1/runs`; at
      HEAD that route requires `developer` role under `ServerAuth`. Run a `call:` job on the old
      agent. **Does the parent fail cleanly, hang, or orphan the child?** Attacks **I1** (run
      accounting) and **I3** (lock release). The 401/403 prediction is *inferred* — measure it.
- [ ] **Step 6: Part C — what else diverges.** The v0.4.0→HEAD wire delta is small
      (`internal/api/types.go` +3 lines). **Enumerate it and verify the enumeration**; report anything
      beyond the two known edges, or state that the class is closed and how you know.
- [ ] **Step 7: Findings, teardown, commit.** Every entry must state which side ran which version.

---

### Task 3: The W5 checkpoint — and the campaign's closing summary

**Files:** Modify `test/edgecase/FINDINGS.md`, `test/edgecase/README.md`,
`<project parent>/edgecase-evidence/README.md`

- [ ] **Step 1: Append `## Checkpoint: W5 complete`** in the W3/W4/W6 format. State:
      (a) the wave's tally, derived from the file;
      (b) **why W5 was reduced**, with the three refuted sub-scenarios named and their `file:line`
      evidence — this is the wave's methodological result and it should read as a decision, not an
      apology;
      (c) whether the "verified code facts" mechanism-vs-`file:line` pattern held for a **seventh**
      consecutive wave;
      (d) carry-forwards still open;
      (e) whether W5 changes the **zero-criticals calibration** (the escalation set stands at six,
      hubbed on `FINDINGS.md:179`). Do not manufacture a critical; do not avoid one.
- [ ] **Step 2: Hunt inconsistencies across the wave's documents and report every one** — never
      resolve one silently. This has found real defects in each of the last four checkpoints.
- [ ] **Step 3: Append `## Campaign summary`** — **this is the campaign's last wave, and the
      operator's entry point to ~145 entries across seven waves.** State the campaign-wide totals by
      severity and limb; the escalation set and why it is six; the invariant-set coverage gaps found
      (no secret-store integrity clause, no cache-integrity clause); the zero-criticals calibration
      as a **disclosed decision, not a measurement**; and the highest-value follow-ups in priority
      order. **Keep it short enough to be read** — a summary nobody finishes is a summary nobody uses.
- [ ] **Step 4: Archive** to `<project parent>/edgecase-evidence/w5/`, verify with `diff -r`, run a
      **fresh credential sweep over the whole evidence root** (not just this wave's), and update both
      READMEs.
- [ ] **Step 5: Commit** (`test(edgecase): record W5 checkpoint and campaign summary`).
