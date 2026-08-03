# W5-2 — a v0.4.0 agent against HEAD controllers

**Wave W5, Task 2. The wave's ONE executed scenario.** Everything else in W5 is
a code-read audit (`w5-1-migration-compat-audit.md`), because the wave's other
sub-scenarios were answered by reading. This one survives the cut because it
targets a **silent wrong-data** divergence rather than a loud failure: at HEAD,
`downloadArtifact.runId` selects which run's artefact a step fetches, and a
v0.4.0 agent — which predates the field — is expected to ignore it, download
the *current* run's artefact instead, and report the step `Succeeded`.

Mixed-version pair: **agent v0.4.0, controllers HEAD.** Nothing else is
versioned down. §"Why only this pair" records why.

---

## The contracts this scenario is measured against

**Lead with the documented statement, not with an invariant** (the W4-1 shape).
`docs/jobs.md:1305-1330` documents `downloadArtifact.runId` in a dedicated
section with no version qualification anywhere in it, and closes with:

> `runId` works on both the standard and Kubernetes agents.

and `docs/jobs.md:1274-1276`:

> By default artifacts are scoped to the current run; a `downloadArtifact` step
> can fetch from another run — most usefully a `call:` child run — with `runId:`

Per the Global Constraints, a **violation** is a contradiction of an invariant
**by its own text** (`FINDINGS.md:1509`) **or** of a statement in `docs/*.md`.
The `docs/jobs.md` passage above is the contract Part A attacks.

The three invariants the plan nominated are each checked against their **own
text** (`docs/superpowers/specs/2026-07-29-edge-case-testing-design.md`), and
two of the three do not reach:

- **I1** (`:48`): *"Run accounting — every API-accepted run reaches exactly one
  terminal state; no phantom runs from duplicate fires/webhooks."* Part B's
  question — does a `call:` step on an old agent orphan a child run, hang the
  parent, or fail it cleanly — is squarely inside this text if a run is created
  and never reaches a terminal state, or if a run is created that nobody
  accounts for. Cited by Part B, subject to measurement.
- **I4** (`:51`): *"Log/artifact integrity — a Succeeded run's log line count
  matches what the workload emitted; no duplicates, no reordering; archives
  stay readable."* **This text does NOT reach Part A.** Every clause in it is
  about log line counts, duplication/ordering, and archive readability. It says
  nothing about an artefact's *provenance* — about whether the bytes that
  landed in the workspace came from the run the job named. Downloading the
  wrong run's artefact violates none of its three clauses. **Recorded as an
  invariant-set coverage gap, not stretched into a citation.** The campaign has
  already recorded two such gaps (no secret-store integrity clause, no
  cache-integrity clause); this is a third of the same kind — *no artefact
  provenance clause*.
- **I7** (`:54`): *"State display consistency — run status, approval status,
  and audit rows never contradict each other or reality."* **Judged not to
  reach either, and the judgement is stated rather than assumed.** The run does
  execute every step without error, so `Succeeded` is not contradicted by the
  audit rows or by the step reports; what is wrong is the *content of a file in
  the workspace*, which is not a status, an approval, or an audit row. Reading
  "reality" broadly enough to cover it would make I7 cover everything and
  therefore mean nothing. Part A is filed against `docs/jobs.md`, not I7.

---

## Why only this pair, and why the agent side only

Established by the wave plan's reconnaissance and re-checked here:

- **A v0.3.0 controller cannot use this rig unmodified** — it predates
  `UNIFIED_CONTROLLER_KEY_FILE` and reads only `UNIFIED_CONTROLLER_KEY`, while
  `test/ha/docker-compose.ha.yaml:84` supplies the former.
- **A v0.3.0 agent cannot authenticate against a HEAD controller at all** —
  legacy agent auth was removed in `4e8f315`.
- A second **controller** version costs a second cold image build (npm ci +
  vite + Go) with no layer sharing, because `controller1: &ctrl` carries
  `build:` and `controller2`/`controller3` are anchor aliases of it
  (`docker-compose.ha.yaml:79-81`, `:103-104`).

So **v0.4.0-agent ↔ HEAD-controller is the only viable mixed pair**, and it is
drop-in: v0.4.0's agent already accepts `--credential-file` and
`--enrollment-token-file` — verified **at the tag**, not assumed
(`v0.4.0:cmd/agent/main.go:66-67`, and `agent --help` inside the built image
lists both).

---

## Step 1 — obtaining a v0.4.0 agent, and what it cost

**The pull route does not exist, and the reason is itself a finding.**

| attempt | result |
|---|---|
| `docker pull ghcr.io/eirueimi/unified-cd-agent:v0.4.0` | `not found` |
| `docker pull ghcr.io/eirueimi/unified-cd-agent:v0.3.0` | **succeeds unauthenticated** — `sha256:fc6178f2b85d0ee09b66afa46c1fbd77f7ba53741295887cf84ae62cca88c2c5` |
| `docker manifest inspect …:v0.4.0` for **all five** images (`controller`, `agent`, `k8s-agent`, `runner`, `artifact-sidecar`) | **TAG MISSING** for all five |

So the packages are public — the plan's "whether the packages are public is not
resolvable read-only" is resolved: **they are public**. What is missing is the
tag, and the cause is in the release run:

- GitHub Actions run **29796662231**, `Release Docker Images`, tag `v0.4.0`,
  **failure** in 1m47s (2026-07-21).
- Failing legs: `build (k8s-agent, amd64)` and `build (k8s-agent, arm64)`, both
  with
  `internal/shim/embedded/embed_amd64.go:11:12: pattern ucd-sh-amd64: no matching files found`
  from `docker/k8s-agent.Dockerfile:7`.
- The other **eight** legs (`controller`, `agent`, `runner`,
  `artifact-sidecar` × amd64/arm64) **all succeeded and pushed by digest**.
- The tag-applying job is `merge: … needs: build`
  (`.github/workflows/release-docker.yml:70-73`), whose own comment reads
  *"Runs only when the entire build matrix succeeded, so a failed build never
  moves any tag."* It never ran.

**Consequence, and it is the interesting part:** one broken image in a
five-image matrix left the whole v0.4.0 release untagged. Every
`ghcr.io/eirueimi/unified-cd-*:latest` still points at v0.3.0, and there is no
tagged v0.4.0 image of any kind, even though four of the five built fine. The
all-or-nothing gate is *deliberate* (the comment says so) — the finding is not
that the gate exists but that **the repository has shipped a `v*` git tag whose
container release silently does not exist**, with no other trace than a red run
13 days old.

**The build route, which is what this scenario used:**

```bash
git worktree add ../wt-v040 v0.4.0
cd ../wt-v040
MSYS_NO_PATHCONV=1 docker build -t unified-cd-agent:v0.4.0-local \
  -f docker/agent.Dockerfile .
```

Cost: **the two Go build layers took 5.3 s and 11.0 s and the export 1.7 s**;
`golang:1.26-alpine`, `go mod download` and the runtime stage's `apk add` were
all **CACHED** from this machine's existing HEAD agent image, so a cold machine
pays materially more than the ~19 s of uncached work measured here. Resulting
image `unified-cd-agent:v0.4.0-local`,
`sha256:bc3b433380e6f844666295f062cfa4cd579f7bc3b53a0654fa3acda457dc936d`,
40,894,945 bytes. Raw capture: `w5-2/step1-build-v040-agent.txt`.

**Note the asymmetry with the release failure:** `docker/agent.Dockerfile` at
v0.4.0 *does* build `./cmd/ucd-sh` into `internal/shim/embedded/` before
building the agent (`agent.Dockerfile:17-18`), which is exactly the step
`k8s-agent.Dockerfile` was missing. The agent image was never the broken one.

---

## Step 2 — the overlay

`compose/mixedver.override.yaml`, following `compose/dupagent.override.yaml:67-88`:
a **new** service, sharing the `agent-credentials` named volume, no existing
service mutated. Two services:

- `agentold-enroll` — one-shot, mirrors the base file's `agent-enroll` for a
  single id, mints an enrollment token for `agentold` with label `kind:old`
  through the product's ordinary enrollment API against a **HEAD** controller.
- `agentold` — `image: unified-cd-agent:v0.4.0-local`, same four
  `UNIFIED_CACHE_*` vars as `agent1`, same `--credential-file` /
  `--enrollment-token-file` flag shape.

**`image:` and not `build:`, deliberately.** The build context is a sibling
worktree outside this repository, and Compose resolves a build context relative
to the **project directory** (`test/ha/`), so a `build:` block would hard-code
one machine's worktree layout into a committed file. A service that has no
`build:` at all does not hit the anchor trap the plan warns about (that trap is
about merging `image:` **onto** an existing `build:`).

**Labels are the steering mechanism.** Agent labels come from the enrollment
token, not from `--labels`. `agentold` alone carries `kind:old`; `agent1` and
`agent2` alone carry `kind:linux`. Every arm below is therefore a **pair of
otherwise-identical jobs differing in one selector line**, so "old agent vs
HEAD agent" is a controlled comparison and not a race for the claim.

Bring-up (from `test/ha`):

```bash
COMPOSE_FILES="-f docker-compose.ha.yaml -f ../edgecase/compose/mixedver.override.yaml"
docker compose $COMPOSE_FILES up -d --build
docker compose $COMPOSE_FILES down -v      # MANDATORY between scenarios
```

Fixtures (each with a `.yaml` and a `.payload.json`, **both** validated through
`tools/w3/fixcheck` — 8 files, 8 `OK`): `w5-oldtick`, `w5-producer`,
`w5-consumer-old`, `w5-consumer-head`, `w5-call-parent-old`, `w5-call-child`,
`w5-detached-old`, `w5-detached-head`.

---

## Step 3 — baseline (GATES EVERYTHING BELOW)

The old agent must enroll against a HEAD controller, register, claim
`edge-w5-oldtick` and finish `Succeeded`. **If it cannot, the wave's executed
half is blocked and that is the finding** — no workaround is to be attempted.

---

## Part A — the silent wrong artefact

**Instrumented so the wrong artefact is provably the wrong one, not merely a
missing one.** The naive shape ("download from another run and see if it
fails") cannot distinguish *ignored the field* from *the artefact was not
there*, because a v0.4.0 agent that ignores `runId` and finds no artefact in
the current run would simply fail the step — which looks like a loud, safe
failure and is the opposite of the claim.

So the consumer job **publishes its own artefact under the same name first**:

1. `make_self_marker` writes `out/marker.txt` = `W5-ARTIFACT-SOURCE=consumer-self`
   + nonce `9f8e7d6c5b4a0002`.
2. `publish_self` uploads it as artefact `w5payload` **of the current run**.
3. `clear_local_copy` deletes `out/` so nothing on disk can be mistaken for the
   download.
4. `fetch_other_run` is `downloadArtifact { name: w5payload, destDir: dl,
   runId: <producer run id> }`, where the producer run wrote
   `W5-ARTIFACT-SOURCE=producer` + nonce `a1b2c3d4e5f60001`.
5. `show` prints the downloaded tree and the file contents.

Both candidate artefacts therefore exist under the same name, and the marker
file names which run it came from. The read-out is unambiguous:

| what `dl/…/marker.txt` says | conclusion |
|---|---|
| `W5-ARTIFACT-SOURCE=producer` / `a1b2c3d4e5f60001` | `runId` honoured |
| `W5-ARTIFACT-SOURCE=consumer-self` / `9f8e7d6c5b4a0002` | **`runId` silently ignored — the wrong artefact, provably** |
| step Failed | a third outcome, recorded as such |

`edge-w5-consumer-head` is the same job with `agentSelector: [kind:linux]`, run
on a HEAD agent as the control. The producer run id is substituted into both
payloads at apply time (the committed fixture carries
`PRODUCERRUNIDPLACEHOLDER`); a run id is a UUID and matches HEAD's
`artifactRunIDRe` `^[A-Za-z0-9_-]{1,64}$` (`internal/agent/orchestrator.go:834`).

**Code-read mechanism, both tags.** HEAD's `executeDownloadArtifact` computes
`targetRunID := runID`, overrides it from `da.RunID` after template expansion
and regex validation, and passes `targetRunID` to the backend
(`internal/agent/orchestrator.go:842-882`). v0.4.0's function has **no
`targetRunID` at all** and passes the claim's own `runID` unconditionally
(`v0.4.0:internal/agent/orchestrator.go:830-851`). The controller sends the
field regardless — `internal/controller/api_agent.go:417` copies
`entry.DownloadArtifact.RunID` into every claim step — and the download route
`GET /api/v1/runs/{runID}/artifacts/{name}` is `agentOrServerAuth` with **no
per-run ownership check** (`internal/controller/server.go:496-499`), so
cross-run download is permitted for both agents.

**Prediction:** the old agent downloads the consumer run's own artefact
(`consumer-self` / `9f8e7d6c5b4a0002`) and reports the run `Succeeded`; the
HEAD agent downloads the producer's (`producer` / `a1b2c3d4e5f60001`).

---

## Part B — the moved `call:` child-run endpoint

v0.4.0's agent creates a `call:` child with
`POST /api/v1/runs` (`v0.4.0:internal/agent/client.go:215-219`). At HEAD that
route lives inside the `/api/v1` group behind `ServerAuth` +
`requireMinRole("developer")` (`internal/controller/server.go:355-370`), and
`ServerAuth` accepts only a PAT, an OIDC bearer, or a session cookie
(`internal/controller/auth.go:67-112`) — never a `uca_` agent access token.
HEAD's own client comment states the same conclusion in as many words: *"the
human POST /api/v1/runs is not reachable with a uca_ credential"*
(`internal/agent/client.go:234-236`). HEAD posts
`/api/v1/agents/{agentId}/runs/{runId}/children` instead
(`client.go:251`), a route registered in `agentRouteIdentityMatrix` at
`internal/controller/server.go:252` and **absent from v0.4.0's server**.

**The 401/403 prediction is INFERRED — routes read, request never made.** It is
measured here. The three outcomes are tracked separately and the child is
followed in each:

- parent fails cleanly, no child row → clean N-1 break, no I1 limb;
- parent hangs → I1 (a run that never reaches a terminal state) and the
  bounded-recovery question;
- **a child run is created but not linked to the parent** → the worst case, and
  the one that would matter: `ListChildRunIDs` (`api_runs.go:396`) drives the
  parent-cancel cascade and `stuckrun_reaper.go:87` drives the orphan cascade,
  both keyed on the parent link. An unlinked child would survive both.

`edge-w5-call-child` selects `kind:linux`, so if a child run *is* created it is
claimable by a HEAD agent and its fate is observable rather than merely
pending. Run accounting is read from the API (`GET /api/v1/runs?job=…`) and
from `run_relations`/`step_reports` in Postgres directly.

---

## Part C — what else diverges, enumerated

The plan says the v0.4.0→HEAD wire delta is "`internal/api/types.go` +3 lines"
and names two edges. **The enumeration is verified rather than accepted**, by
diffing every file that defines the wire: the API types, the agent's HTTP
client, and the controller's route table.

```
git diff v0.4.0..HEAD --stat -- internal/api/            # 2 files, +8 -6
git diff v0.4.0..HEAD -- internal/agent/client.go        # +44 -5
git diff v0.4.0..HEAD -- internal/controller/server.go   # route table + Config
```

Findings are recorded in §Results. **A third and a fourth edge exist that the
plan does not list** — see Results; naming them here would pre-empt the
measurement.

---

## Results

Executed 2026-08-03, 02:56-03:05 UTC, on the `test/ha` stack plus
`compose/mixedver.override.yaml`. Raw captures: `w5-2/` in the evidence root.

### Rig notes taken during bring-up

- **The bootstrap-PAT race fired.** `controller1` exited(1) on
  `create bootstrap pat: ERROR: duplicate key value violates unique constraint
  "pats_token_hash_key" (SQLSTATE 23505)`; `controller2`/`controller3` came up.
  This is the known race (`FINDINGS.md:2270`, README §"Three bring-up
  gotchas" (ii)) and **not** a mixed-version effect — the overlay adds no
  controller. Recovered with `docker compose … up -d controller1`; all three
  ran for the rest of the session. Recorded because the README asks for the
  3-up/2-down tally to be kept: **this bring-up was 2 up / 1 down.**
- `agentold` logged `cache enabled` and `agent registered` 17 ms apart and
  needed no retry.

### Step 3 — baseline: **PASS. The gate is open.**

`edge-w5-oldtick` → run `e3f6036a-…`, `claimedBy: agentold`, **Succeeded** in
~1.1 s (created 02:57:36.49, updated 02:57:37.57), two stdout lines including
`Linux 85701cb9c37a … x86_64 GNU/Linux`. **A v0.4.0 agent enrolls, registers,
claims and finishes a run against HEAD controllers with no modification to
either side.** So the wave's executed half is not blocked, and the plan's
"drop-in" claim holds.

**And one thing the baseline shows that no code read would have:** the
list-agents response gives **`"version": "dev"` for all three agents** — the two
HEAD agents *and* the v0.4.0 one (`step3`/agents capture). See §Unplanned
finding 1.

### Part A — **CONFIRMED, exactly as predicted, and provably.**

| | run | claimedBy | run status | `fetch_other_run` step | `dl/marker.txt` |
|---|---|---|---|---|---|
| producer (control input) | `d8eb76f5-…` | `agent1` (HEAD) | Succeeded | — | published `W5-ARTIFACT-SOURCE=producer` / `a1b2c3d4e5f60001` |
| consumer, **v0.4.0 agent** | `2c682686-…` | **`agentold`** | **Succeeded** | **Succeeded** | **`W5-ARTIFACT-SOURCE=consumer-self` / `9f8e7d6c5b4a0002`** |
| consumer, HEAD agent | `faecbba3-…` | `agent2` (HEAD) | Succeeded | Succeeded | `W5-ARTIFACT-SOURCE=producer` / `a1b2c3d4e5f60001` |

Both jobs carry the identical `runId: d8eb76f5-acd7-4eac-be49-baa0831e5df7`;
they differ in **one line** (`agentSelector: kind:old` vs `kind:linux`).

**The wrong artefact is provably the wrong one, not a missing one.** Both
candidates existed under the name `w5payload` at download time — the consumer
run published its own first (step 1, `Succeeded`, `kind: uploadArtifact`) and
then deleted the local copy (step 2 log: `workspace cleared; only dl/ remains`,
followed by an `ls -la` showing only `.ucd-mode` and an empty `dl/`). So the
old agent did not fall back to a leftover file and did not fail to find the
named run's artefact: it fetched a **different, existing** artefact and
returned it under the name the job asked for.

**Nothing anywhere signals it.** All five steps report `Succeeded` with
`exitCode: 0` on both runs; `agentold`'s container log for that run is a single
`"running"` line with no warning; the controller emits nothing. The only
difference visible anywhere in the product is the *content of a file in the
workspace*.

Filed as a violation of `docs/jobs.md:1305-1330` — see `FINDINGS.md` §W5-2a.

### Part B — the 401 prediction **HOLDS**; the break is clean.

Parent `9005f151-…`, `claimedBy: agentold`:

- `prelude` **Succeeded** (`exitCode 0`), one log line.
- `invoke_child` **Failed**, `kind: call`. `agentold`'s log:
  `{"level":"ERROR","msg":"call step failed","step":"invoke_child","error":"create child run for job \"edge-w5-call-child\": http 401: response omitted"}`
- `after_call` **Skipped**.
- Parent run **Failed**, 0.5 s after claim. **One** terminal state.

**The child was followed, and there is none.** `SELECT … FROM runs WHERE
job_name='edge-w5-call-child'` → **0 rows**; `SELECT count(*) FROM step_reports
WHERE child_run_id IS NOT NULL` → **0**. No orphan, no phantom, no hang.

**Measured, not inferred: 401, not 403.** The request never reaches
`requireMinRole("developer")` — `ServerAuth` rejects the `uca_` credential
first, because it matches no PAT hash, no OIDC bearer and no session cookie
(`internal/controller/auth.go:67-112`). The plan's "401/403" is resolved to
**401**.

**I1 is a null limb and is recorded as one.** *"every API-accepted run reaches
exactly one terminal state; no phantom runs"* — the parent reached exactly one
(`Failed`), and no run was created that anything failed to account for. **I3 is
also null**: no `mutex:`/`concurrency:` was in play in this job at all, so the
plan's I3 citation has nothing to attach to; recorded rather than stretched.

One diagnosability point survives: **`http 401: response omitted`** is the
whole message an operator gets. It does not name the URL, does not say the
route moved, and deliberately drops the body. On a fleet mid-upgrade this is
the only symptom of "your agent is older than your controller".

### Part C — the enumeration, verified, and it is **not closed at two edges**

Route-surface diff, both tags, normalised so group-relative registrations are
comparable (`paths-head.txt` / `paths-v040.txt`, 75 vs 74 distinct path
literals in `internal/controller/server.go`):

```
ONLY AT HEAD:    /api/v1/agents/{agentId}/runs/{runId}/children
ONLY AT v0.4.0:  (none)
```

**Exactly one route added, zero removed.** So a v0.4.0 agent never calls a path
that has disappeared; every request it makes still resolves. The `call:`
failure is an **authorization** outcome on a route that still exists, not a
404. *(A first pass grepping only for absolute `"/api/v1…"` literals reported
three added routes; two of those — `/runs/active`, `/runs/{id}/outputs` — are
group-relative at v0.4.0 and the grep missed them. The normalised survey
above is the one to cite.)*

Wire-type diff:

| file | delta | agent-visible? |
|---|---|---|
| `internal/api/types.go` | +3 | **yes** — `DownloadArtifactStep.RunID` only. Edge 1. |
| `internal/api/agent_auth.go` | +5 −6 | no — `AgentEnrollmentPolicyRequest.Capabilities` and `CreateAgentEnrollmentRequest.Capabilities` removed, `AgentID` now `omitempty`. These are the **admin/CLI** enrollment-policy types; the agent never sends either. |
| `internal/agent/client.go` | +44 −5 | **yes**, two call sites. Edges 2 and 3. |

**The class is NOT closed at the plan's two edges. There is a third.**

**Edge 3 — detached runs are unclaimable by a v0.4.0 agent.** HEAD's client
gained `ClaimDetached`, which appends `&kind=detached` to the claim
(`internal/agent/client.go:184-200`), read by the controller at
`internal/controller/api_agent.go:131`; the pool defaults to 16 slots
(`internal/agent/agent.go:315-317`), and migration `017_run_detached` and
`dsl.Spec.Detached` (`internal/dsl/types.go:47`) both post-date v0.4.0. A
v0.4.0 agent has no such loop, so it never asks. **Measured:**

| job | selector | outcome |
|---|---|---|
| `edge-w5-detached-old` | `kind:old` | **`Queued` for the whole 317 s window, which I ended.** `updated_at` never moved past 02:59:48.66 — 0.1 s after creation, i.e. **no state transition at all** |
| `edge-w5-detached-head` | `kind:linux` | `Succeeded`, claimed by `agent1`, within 6 s |

Per the campaign rule, **"never" is not written for a window I ended myself**:
what is measured is 317 s with zero transitions, against a 6 s control.

**Edge 4 — the enrollment-policy wire lost `Capabilities`** (table above). Not
reachable by the agent, so it is recorded as part of the enumeration and not as
a scenario. A v0.4.0 **`unified-cli`** posting `capabilities` to a HEAD
controller would have the field silently dropped; not measured.

So the enumeration, and how I know it is complete: the agent↔controller wire is
exactly (a) the shared types in `internal/api/`, (b) the request the agent's
`Client` builds, and (c) the routes the controller registers. All three were
diffed in full at both tags, non-test files included; the totals are +8/−6
lines of API types, +44/−5 lines of client, and +1/−0 routes.

### Unplanned finding 1 — every shipped agent reports `version: "dev"`

`GET /api/v1/agents` returned `"version": "dev"` for `agent1`, `agent2` **and**
`agentold`. `internal/agent/version.go:5` is `var Version = "dev"`, set only by
`-ldflags`, and **no Dockerfile passes `-ldflags` at all**: `grep -rc ldflags
docker/` returns `0` for all **seven** Dockerfiles at HEAD and matches nothing
at v0.4.0. The repository's only `-ldflags` is `Makefile:16`, which targets
`internal/cli.version` — the CLI, not the agent.

This compounds W5-1's result rather than duplicating it. W5-1 establishes that
**nothing reads** the version. This establishes that **nothing writes a real
one either**: a v0.4.0 agent and a HEAD agent are, on every surface the product
exposes, indistinguishable. An operator cannot answer "which of my agents are
old?" from the product at all.

### Unplanned finding 2 — the v0.4.0 container release does not exist

Detailed in §Step 1. Summary of what was measured: the packages are **public**
(v0.3.0 pulls unauthenticated); **all five** v0.4.0 tags are missing; the
release run failed on `k8s-agent` only; the tag-applying `merge` job is
`needs: build`. Root cause, pinned: **`internal/shim/embedded/ucd-sh-amd64` is
not a tracked file at v0.4.0 at all** — `git ls-tree v0.4.0
internal/shim/embedded/` lists four `.go` files and no binaries — while
`embed_amd64.go` carries `//go:embed ucd-sh-amd64`. `agent.Dockerfile:17`
*creates* it before building, so the agent image builds; `k8s-agent.Dockerfile`
never did. That Dockerfile is **byte-identical at v0.4.0 and HEAD**
(`git diff v0.4.0..HEAD -- docker/k8s-agent.Dockerfile` is empty) — the
build only works at HEAD because the real binaries were later checked in
(`ucd-sh-amd64`, 4,977,841 bytes). Consequence: every
`ghcr.io/eirueimi/unified-cd-*:latest` still resolves to **v0.3.0** (2026-07-16),
which predates the entire v0.4.0 auth rewrite, and `README.md:30-33` tells a
new user to pull exactly those tags.

---

## Corrections to the plan's reconnaissance

Seven consecutive waves have had the plan's "verified code facts" corrected by
execution, with the pattern that `file:line` claims hold and *mechanism* claims
fail. **The pattern held for a seventh wave, and in both directions.**

**`file:line` claims that held, re-checked at both tags:**
`internal/api/types.go:357-359` (`RunID`, `omitempty`), v0.4.0's
`internal/agent/client.go:218` (`POST /api/v1/runs`), HEAD's `client.go:251`
(the children route), `internal/controller/server.go:355-370`
(`ServerAuth` + `requireMinRole("developer")`), `v0.4.0:cmd/agent/main.go:66-67`
(`--credential-file` / `--enrollment-token-file`),
`test/ha/docker-compose.ha.yaml:79-81` and `:103-104` (the `&ctrl` anchor).

**Corrections:**

1. **"HEAD is 79+ commits ahead of v0.4.0" — wrong.** `git rev-list --count
   v0.4.0..main` is **56**; including this branch's three W5 commits,
   `v0.4.0..HEAD` is **64**. Neither is 79.
2. **"images for v0.0.1-v0.4.0 *should* exist … whether the packages are public
   is not resolvable read-only" — resolved, and half of it is false.** The
   packages **are** public. The v0.4.0 tags **do not exist**, for any of the
   five images. §Step 1.
3. **"the GHCR images exist and are publicly pullable" (listed as a hypothesis)
   — split verdict**: publicly pullable **yes** for v0.0.1-v0.3.0, **no image
   at all** for v0.4.0.
4. **"a v0.4.0 agent's `POST /api/v1/runs` gets 401/403" — resolved to 401**,
   and the parent fails cleanly rather than hanging or orphaning.
5. **"the v0.4.0→HEAD wire delta is small … two sharp edges" — the count is
   wrong.** There are **three** agent-visible edges (the third is detached-run
   claiming, measured) and a fourth on the enrollment-policy wire. The
   *size* claim (`types.go` +3) is exactly right; the *enumeration* was not.
6. **The plan's invariant nominations do not survive contact with their own
   text.** I4 does not cover artefact provenance; I7 does not cover workspace
   file contents; I3 has nothing to attach to in Part B. Only I1 applies, and
   it is a null limb. Part A is filed against `docs/*.md` instead.

## What was NOT measured

- Whether any reaper eventually terminalizes the stranded detached run. The
  observation window is 317 s and I ended it.
- Whether a v0.4.0 `unified-cli` (as opposed to agent) breaks against a HEAD
  controller — edge 4 is code-read only.
- The cost of a cold v0.4.0 agent build. The measured 5.3 s + 11.0 s + 1.7 s is
  with `golang:1.26-alpine`, `go mod download` and `apk add` all CACHED.
- Anything about the k8s-agent at v0.4.0: it does not build, which is finding 2.

## Teardown, archival and the credential sweep

`docker compose … down -v` ran clean (all volumes and the network removed, no
`unified-cd-ha` container left). The 38 capture files are archived at
`<project parent>/edgecase-evidence/w5/w5-2/` and verified with `diff -r`
(identical). The `../wt-v040` worktree and the `unified-cd-agent:v0.4.0-local`
image are left in place — a re-run needs both, and rebuilding costs ~19 s only
while the base layers stay cached.

**Sweep note for the checkpoint, so the number is not re-derived.** A sweep over
the **whole** evidence root for `uc[aer]_[A-Za-z0-9_-]{20,}` returns **0**, and
for `BEGIN … PRIVATE KEY` **0**, matching W6's checkpoint. A *looser*
JWT-shaped probe (`eyJ[A-Za-z0-9_-]{10,}`, no dot-separator requirement)
returns **12** hits — **all false positives**: every one is a base64-encoded
job spec in a `jobs-apply`/`trigger` capture that decodes to
`{"Steps": [{"If": …`, not a token. The two `client-key-data` hits are
`edgecase-evidence/README.md`'s own prose describing the sweep. W6's narrower
"0 JWT-shaped strings" claim stands; this wave adds nothing to scrub.
