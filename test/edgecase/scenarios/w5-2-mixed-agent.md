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

*(appended after execution — see the sections below)*
