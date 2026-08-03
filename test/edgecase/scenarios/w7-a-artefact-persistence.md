# W7-A — do the substituted artefact bytes reach PERSISTENT state?

**Follow-up Arm A.** Discharges campaign carry-forward **7(ii)**
(`FINDINGS.md:3107`) and the "what would flip it to `critical`, and was NOT
measured" clause of `FINDINGS.md:3014`.

W5-2 established the premise and stopped at the workspace by design: a v0.4.0
agent silently ignores `downloadArtifact.runId`, downloads the **current** run's
artefact instead of the named run's, and reports every step and the run
`Succeeded` (`FINDINGS.md:3004`). `:3014` names exactly what was not measured:

> a demonstration that the substituted bytes reach *persistent* product state —
> an artefact republished from the wrong input, a cache entry saved over it —
> which would turn selection into corruption

This scenario takes **both** named routes in one run, and then asks the question
the two of them only make sense together with: **does the wrong content survive
the agent upgrade and reach an agent that has none of the defect?**

---

## The contracts this scenario is measured against

Unchanged from W5-2 and re-stated because the band argument turns on the
invariant set's silence, not on its text. `docs/jobs.md:1305-1331` documents
`downloadArtifact.runId` with no version qualification and closes with
*"`runId` works on both the standard and Kubernetes agents"*; `docs/jobs.md:1274-1276`
states the feature. The mixed fleet is the one `docs/operations.md:189-195`
prescribes — *"Upgrade order: controller first, then agents"*, `:194` *"Agents —
upgrade standard agents after the controller is on the new version"* — **with no
bound stated on how long that window may be**.

**No invariant reaches this, and that is a recorded coverage gap rather than a
stretch.** I4 (`spec:51`) is about log line counts, duplication/ordering and
archive readability; **it has no provenance clause**, which is the campaign's
third gap of that kind (`FINDINGS.md:3006`, carry-forward 5). The result below
is the case the campaign's own prescribed amendment
(`FINDINGS.md:3188`: *the object served is the object named*) exists for.

**Nothing is injected anywhere in this scenario.** There is no fault, no
partition, no throttle, no killed process. Four ordinary runs on an ordinary
rig, and the only "arm" is which agent service is running — which is the state
the upgrade procedure itself produces.

---

## Rig

```bash
# the v0.4.0 agent image must be BUILT, not pulled (FINDINGS.md:3046)
git worktree add ../wt-v040 v0.4.0
cd ../wt-v040 && MSYS_NO_PATHCONV=1 docker build \
  -t unified-cd-agent:v0.4.0-local -f docker/agent.Dockerfile .

cd test/ha
HA_LB_PORT=18091 docker compose -p edge-arma \
  -f docker-compose.ha.yaml \
  -f ../edgecase/compose/mixedver.override.yaml \
  -f ../edgecase/compose/altports.override.yaml up -d --build
```

`compose/altports.override.yaml` is new here. It exists because Arm C needed a
3-hour undisturbed window on its own project while this scenario came up and
went down on another; `ports:` is a sequence and Compose **appends** overrides,
so it uses `!override` and the overlay's header says to verify that with
`docker compose config`. Nothing inside the stack sees the published port.

**The bootstrap-PAT race fired again on the cold `up`** — `controller1` exited
with `create bootstrap pat: duplicate key value violates unique constraint
"pats_token_hash_key"`, exactly the shape `FINDINGS.md:2270` records, and
`docker compose up -d controller1` recovered it. That is 4 up / 3 down across
the campaign's cold bring-ups now; **verify all three are `Up` before starting**.

## Fixtures

Two, both new, both validated through `tools/w3/fixcheck` before an API call was
spent (`w7-producer.yaml`, `w7-poison.yaml`, plus their `.payload.json`).

**`w7-poison.yaml` deliberately carries NO `agentSelector`, and that is a
departure from W5-2's controlled-pair shape that has to be defended.** W5-2's
shape is a *pair* of jobs differing in the selector line. It cannot answer this
question, by construction: the agent cache key is
`caches/<b64url(sha256(jobName))>/<b64url(sha256(key))>.tar.zst` — **the job
name is in the key** — and two jobs cannot share a name, so a pair can never
collide in the cache and the propagation limb would be unmeasurable. So this
scenario uses **one** job and steers by which agent services are running:

| phase | services running | who can claim |
|---|---|---|
| 1 | `agentold` only | the v0.4.0 agent |
| 2 | `agent1`, `agent2` only | HEAD agents |

That is a rig control, not an injection, and it models a controller-first
upgrade with agents replaced one at a time more directly than a label split
does.

**W5-2's discrimination instrument is kept and is load-bearing.** The consumer
publishes its **own** artefact under the **same name** `w7payload` first, then
clears the workspace, and only then downloads with `runId:`. Both candidates
therefore exist under one name at download time, each carrying a nonce naming
its origin, so the read-out separates *ignored the field* from *nothing was
there*. Dropping it turns the result into a null.

---

## Prediction, recorded before the rig came up

Written into the session's `PREDICTIONS.md` before the schedule was created and
before any of this ran:

- **A1 republish → YES**: `uploadArtifact` is a plain upload of workspace bytes.
- **A2 cache → YES**: the deferred save archives whatever is in `paths:`.
- **A3 propagation → "YES *if* the cache key namespace is reachable from a
  HEAD-claimable job"**, flagged as the load-bearing uncertainty, with the worry
  that a per-job `<jobhash>` would confine it.
- **Band: "to be argued from `:6-8` only after the measurement, not now."**

## Delta

A1 and A2 landed as predicted. **A3's stated worry was misdirected and the
answer is worse than the prediction**: the per-job hash does not confine the
blast radius, it *is* the propagation path — the same job, which is what an
operator re-runs, is exactly what collides. Two further results were not
predicted at all: the poisoned entry is **re-archived by the HEAD run that
consumes it**, with its TTL refreshed, so it never expires; and the HEAD run's
**own** download in the same run is correct, which is what proves the wrong
bytes came from the store and not from anything the HEAD agent did.

---

## Execution

All timestamps are DB/host clock, 2026-08-03. Captures: `w7/w7-a/step0-producer.txt`,
`step1-phase1-v040.txt`, `step2-persistent-state.txt`, `step3-cache-and-phase2.txt`,
`step4-controls.txt`, `step5-signals-and-census.txt`, plus the four objects pulled
out of band (`cache-entry.tar.zst`, `cache-entry-after-head.tar.zst`,
`cache-entry-control.tar.zst`, `w7republished-from-api.tar.gz`).

**One capture gap, annotated rather than asserted** (`FINDINGS.md:3015`'s
precedent). A separate full agent-log dump was attempted, the command failed,
and the failure was not noticed until after `down -v`. What survives is the
`agent-side signal search` section of `step4-controls.txt`, holding
**`agentold`'s complete log** — enrol, `cache enabled`, `running`, `cache miss`,
`cache saved`, drain — and **not** `agent1`/`agent2`'s. So "no WARN or ERROR on
any agent" is measured for `agentold` and **unmeasured** for the two HEAD
agents. Nothing in the finding depends on it.

### Step 0 — the producer (HEAD fleet)

`edge-w7-producer` run `1b8fd335-0ac2-4c8f-9966-6a1e8f5b8320`, claimed by
`agent2` (HEAD), `Succeeded` at `11:58:07`, artefact `w7payload` carrying
`W7-ARTIFACT-SOURCE=producer` / `W7-NONCE=a1b2c3d4e5f67001`.

### Step 1 — phase 1, the v0.4.0 agent

`stop agent1 agent2`; `GET /api/v1/agents` then returns `agentold` alone. Run
`02b24f18-0edc-46c4-9cb7-d846cc8d78c4`, `claimedBy: agentold`, **`Succeeded`**,
all nine steps `Succeeded` with `exitCode 0`. Its log:

```
seq 11  W7-CACHE=MISS
seq 12  W7-CACHE-STAGED=yes
seq 14  W7-ARTIFACT-SOURCE=consumer-self     <- what `fetch_other_run` delivered
seq 15  W7-NONCE=9f8e7d6c5b4a7002
seq 17  W7-ARTIFACT-SOURCE=consumer-self     <- what the deferred cache save will archive
```

W5-2's defect, reproduced on a fresh rig with a fresh fixture.

### Step 2 — the persistent state, read OUT OF BAND

Not through a job, so nothing the agent does can be mistaken for the answer:

```
artifacts/1b8fd335-…/w7payload.tar.gz        187B   (producer, control)
artifacts/02b24f18-…/w7payload.tar.gz        192B   (the consumer's own)
artifacts/02b24f18-…/w7republished.tar.gz    192B   <- the republish route
caches/oebhFROq…/Cbf4JOX…tar.zst             192B   <- the cache route
caches/oebhFROq…/Cbf4JOX….meta               119B
```

`GET /api/v1/runs/02b24f18-…/artifacts/w7republished` → **HTTP 200, 192 bytes**;
decompressed (the payload is zstd under a `.tar.gz` key — that is
`FINDINGS.md:1702`, already recorded, not re-filed) it is one file,
`marker.txt`, reading

```
W7-ARTIFACT-SOURCE=consumer-self
W7-NONCE=9f8e7d6c5b4a7002
```

**So the wrong bytes are downloadable through the product's own artefact API as
a first-class artefact of a `Succeeded` run.**

The cache object's sidecar:

```json
{"originalKey":"edge-w7-poison-v1","expiresAt":"2026-08-04T11:58:44.160776272Z",
 "size":192,"ownerJob":"edge-w7-poison"}
```

and its content is the same wrong `marker.txt`.

### Step 3 — phase 2, a HEAD agent, same job, same cache key

`stop agentold`, `start agent1 agent2`. Run
`ea2c1a2f-27ae-4bc5-bca2-a06f0eb3bc0c`, `claimedBy: agent1` (**HEAD**),
**`Succeeded`**:

```
seq 27  W7-CACHE=HIT
seq 29  W7-ARTIFACT-SOURCE=consumer-self   <- restored from the object store
seq 30  W7-NONCE=9f8e7d6c5b4a7002
seq 32  W7-CACHE-STAGED=no (restored entry kept)
seq 34  W7-ARTIFACT-SOURCE=producer        <- this agent's OWN download, CORRECT
seq 35  W7-NONCE=a1b2c3d4e5f67001
seq 37  W7-ARTIFACT-SOURCE=consumer-self   <- what it re-archives
```

**`seq 34` is the control that makes this a discrimination rather than a
coincidence**: the same run, the same `runId:` literal, on a HEAD agent, fetched
the *correct* artefact. The only wrong bytes in that run came from the shared
cache.

### Step 4 — two controls

**(a) The poisoned entry survives the run that consumed it, and is renewed.**
After the HEAD run the object is re-written at `12:00:31` (from `11:58:44`),
content-identical and byte-differing (191 B vs 192 B — the archive is
re-created, not copied), and by the third write its `.meta` reads
`expiresAt: 2026-08-04T12:01:33`. **The 1-day TTL is refreshed by every
consumer, so the entry never expires while the job keeps running.**

**(b) Deleting the object out of band restores correct behaviour.** `mc rm` of
the `.tar.zst` and the `.meta`, then the same job on the same HEAD fleet, run
`ef86f2c0-…`, `claimedBy: agent2`:

```
seq 47  W7-CACHE=MISS
seq 48  W7-CACHE-STAGED=yes
seq 50  W7-ARTIFACT-SOURCE=producer
seq 53  W7-ARTIFACT-SOURCE=producer     <- what it archives: correct
```

So nothing regenerates the wrong content; the cache object **is** what carries
it, and an out-of-band delete is what clears it.

### Step 5 — what the product said about any of it

- **4 runs, all `Succeeded`. 29 `step_reports`, all `Succeeded`.**
- **`audit_logs`: 16 rows, and the `action` histogram is
  `agent.enrollment.exchange 5 / run.trigger 4 / agent.enrollment.create 3 /
  agent.refresh 2 / job.apply 2`. Zero rows for any artifact upload or
  download** — the live confirmation of W5-2's code read
  (`api_artifacts.go` has no `slog` site; `audit.go` matches `artifact` and
  `MethodGet` zero times).
- **Controller lines matching `cache`: 0.** Lines matching `artifact`: **11**,
  every one of them an `accessLogMiddleware` `http request` line carrying
  method/path/status/duration/remoteAddr and nothing else. The `GET` that
  delivered the wrong artefact and the `GET` that delivered the right one are
  the same line shape with different run ids.
- **Agent-side**: `agentold` logged `cache miss` and `cache saved` at INFO, and
  nothing else about any of it. No WARN, no ERROR, on any agent, in the whole
  session.

---

## Findings

| # | FINDINGS.md | Kind | Severity | Subject |
|---|---|---|---|---|
| 1 | W7-A entry | violation | **critical** | the substituted bytes reach the object store as an artefact AND as a shared cache entry, propagate to a fully-upgraded agent, and are renewed rather than repaired |

**One entry, not three.** The republish route, the cache route and the
propagation are one defect measured at three depths; splitting them would file
one contract three times, which is the test `FINDINGS.md:2961` applies to its
own Class D and this scenario applies here.

## What this scenario does NOT establish

- **The `deployed` limb was not measured.** The charter's disjunct was
  "published, cached, or deployed"; there is no deployment surface on this rig
  and none was faked.
- **Whether a `uses:` template or a `call:` child re-exports the poisoned entry
  further** was not measured. The cache key contains the job name, so a second
  job would need the same name, which is impossible — but a `call:` child runs
  under its own job name and was not tested.
- **The cache-key hash inputs were read off `cache-user.yaml`'s header and
  confirmed by the observed key**, not re-derived from `internal/cache/`.
- **Nothing here re-measures the v0.4.0 download defect itself**; that is
  `FINDINGS.md:3004` and it is cited, not re-filed.
