# W4-1 — the orphan-pod GC racing a live run: skip, don't delete

**Wave W4, Task 1.** The Kubernetes agent sweeps run Pods roughly once a minute
and deletes the ones whose backing Run is finished or gone. The sweep's
dangerous edge is the one where it *cannot tell*: if `GetRun` fails for a
reason that is not a definitive "this run does not exist", the Pod it is
looking at may still be executing a live run, and deleting it kills that run.
`internal/k8sagent/podgc.go` is written to skip in that case, and
`docs/high-availability.md` promises it in as many words. This scenario
measures whether the shipped agent does what that passage says, and — because
"the Pod survived" is also what a sweep that never ran looks like — measures
the opposite branch on the same rig with the same instrument, so the result is
a discrimination rather than a null.

**This scenario leads with a documented contract, not with an invariant.**
`docs/high-availability.md:428-433` states all three limbs of the GC's decision
rule in one passage, and `:351-352` leans on the same rule to justify the
shipped `replicas: 2`. The campaign invariants are cited only where their own
text applies:

- **I1**, quoted verbatim from
  `docs/superpowers/specs/2026-07-29-edge-case-testing-design.md:45`:

  > | I1 | **Run accounting** — every run reaches exactly one terminal state,
  > and that state matches what actually happened |

- **I2**, `:46`:

  > | I2 | **At-most-once side effects** — a step's side effects are never
  > duplicated by a failover, retry, or reconnect |

Per `FINDINGS.md:1509`, an invariant must be contradicted by **its own text**,
not its spirit. A pod GC that spuriously kills a live run would make the run's
terminal state not match what actually happened, which is I1's text; nothing in
this scenario duplicates a side effect, so I2 is a null limb and is recorded as
one rather than stretched.

---

## Disclosure: the enrollment path under this scenario is bypassed

**Stated once, plainly.** Kubernetes agent enrollment does not work at HEAD and
cannot be made to work without a product-code change (`w4-0-enrollment-spike.md`).
Every W4 scenario, this one included, therefore runs against a controller whose
**`POST /api/v1/agents/enroll` is answered by test infrastructure** — an
interposer that mints a credential through the product's ordinary
`"enrollment"` method instead. What was built, and what was measured about it,
is in `scenarios/w4-rig.md`, which carries the standing form of this disclosure.

**Consequence for this scenario's findings:** nothing below says anything about
the Kubernetes enrollment path beyond what `w4-0` already records. Everything
this scenario touches — claim, Pod create, the GC sweep, `GetRun`, `DeletePod`,
the reuse pool — is the real product path, unmodified.

---

## Corrections to inherited facts, established BEFORE execution

Per the W1/W2/W3 carry-forward rule, this task's brief carries a "verified code
facts" block, and four consecutive waves have had such a block corrected by
execution. Every claim was re-read at this branch's HEAD. **All the `file:line`
claims hold.** Three statements need adjusting, and one of them sharpens the
scenario's central constraint rather than softening it.

### The facts that held, re-read at HEAD

- `go a.runPodGC(runCtx, time.Minute)` is at `internal/k8sagent/agent.go:139`,
  with the `<= 0 → time.Minute` guard at `podgc.go:102-104`.
- **Not leader-elected, no advisory lock** — `grep -rn "leader" internal/k8sagent/`
  returns **0** hits, and `manifests/base/k8s-agent/deployment.yaml:9` sets
  `replicas: 2`.
- `listRunPods` (`podgc.go:125-141`) lists label `app=unified-cd-agent` in
  `cfg.Namespace` via `PodManager.ListPods` (`podmanager.go:69-73`), which is
  namespace-scoped and carries no field selector, no owner filter and no
  agent-id predicate — **the sweep is not scoped to this agent's own runs**.
- `podGCDecision` (`podgc.go:19-24`) is `if poolManaged { return false }` then
  `return !found || isTerminalRunStatus(runStatus)`; `poolManaged` is
  `pod.Annotations[annoPoolStatus] != ""` (`podgc.go:137`), pool-*status* and
  deliberately not pool-template (rationale `:119-124`); `isTerminalRunStatus`
  is `Succeeded|Failed|Cancelled` (`:27-34`).
- Unresolvable run → skip and retry next sweep (`podgc.go:76-81`). Only a
  definitive HTTP 404 counts as gone: `isRunNotFound` (`:57-60`) requires
  `errors.As(err, &*agentlib.HTTPError)` **and** `StatusCode == 404`.
- The only signals are `slog`: `podgc.go:79` (skip, `WARN`), `:87` (delete
  failed, `WARN`), `:90` (deleted, `INFO`, carrying `runFound`), plus `:114`
  (list failed, `ERROR`). `grep -rnE "prometheus|promauto|metrics\.|Metric"
  internal/k8sagent/` returns **0**.
- `internal/agent/client.go:107-108` mints
  `&HTTPError{StatusCode: …, Body: "response omitted"}` for every status ≥ 400,
  so the status survives to `isRunNotFound` but the controller's error body does
  not survive to anything.

### CORRECTION 1 — "the only *non-test* call site" understates it: `runPodGC` has **no test call site at all**

`grep -rn "runPodGC(" --include="*.go" .` returns exactly two lines: the
definition at `podgc.go:101` and the goroutine at `agent.go:139`. The ticker
loop, and therefore the interval and the `<= 0` guard at `:102-104`, are
**never exercised by any test in the tree** — `podgc_test.go` has three tests
and they cover `podGCDecision`, `runPodGCOnce` and `listRunPods`, all of which
are the interval-free inner layers. The guard is unreachable code at HEAD.

This matters to the scenario as a cost, not as a defect: **the one minute is a
literal at a call site with no seam**, so unlike `internal/agent`'s
`workspaceGCInterval` (a package-level `var` at `agent.go:528`, which
`workspace_gc_test.go:145-147` overrides to 5 ms), there is nothing a test or an
operator can turn down. Every trial in this scenario costs up to a full minute
and the sweep phase cannot be commanded.

### CORRECTION 2 — the no-metrics enumeration is over **53** files, not 60

`ls -1 internal/k8sagent/ | wc -l` and `find internal/k8sagent -type f | wc -l`
both return **53** (the package is flat — no subdirectories). The claim itself
holds: 0 matches for `prometheus|promauto|metrics\.|Metric` across all 53. The
count is corrected because the recording rules require an enumeration claim to
be verified rather than repeated.

### CORRECTION 3 — the contract passage carries a fourth limb the brief did not quote, and a second passage rests on it

Quoted from the file, `docs/high-availability.md:427-433`, verbatim and in full:

> The k8s-agent additionally leaks `ucd-run-*` Pods if not cleaned up, since
> pod-per-run does not reuse Pods across Runs. `internal/k8sagent/podgc.go`
> sweeps run Pods every **~1 minute** and deletes a Pod when its backing Run is
> terminal (`Succeeded` / `Failed` / `Cancelled`) or definitively gone
> (`GetRun` returns HTTP 404) — so it naturally cleans up Pods for runs the
> stuck-run reaper just Failed. Pods still owned by the Pod-reuse pool are
> never touched. Any other error resolving the Run (a transient
> controller/network blip) causes that Pod to be skipped for the cycle rather
> than deleted, since deleting the Pod for a Run that's actually still live
> would spuriously kill it.

Four limbs, all testable: **(L1)** ~1 minute sweep; **(L2)** terminal-or-404 →
delete; **(L3)** pool-owned → never touched; **(L4)** any other error → skip for
the cycle. The threshold row at `:444` repeats L1 (`| k8s pod GC sweep interval
| ~1m | How often orphaned ucd-run-* Pods are cleaned up |`).

The brief did not mention `docs/high-availability.md:351-352`, which makes the
GC's decision rule load-bearing for the shipped replica count:

> This is safe without leader election because run claiming is atomic
> (`FOR UPDATE SKIP LOCKED`), each pod registers under its own agent ID …, scope
> pods use `generateName`, and pod GC only touches pods whose runs are terminal
> or absent.

That is the sentence Part D is against.

**Docs survey, untruncated, with hit counts.** Case-**insensitive**
`pod GC|podgc|orphan-pod|orphan pod|orphaned Pod` across `docs/` returns **62**
hits, of which **8** are outside `docs/superpowers/`:

```
docs/agents.md:513
docs/high-availability.md:351  :425  :428  :444
docs/operations.md:79  :87
docs/troubleshooting.md:223
```

*(An earlier, case-**sensitive** pass of the same pattern returned 37/7 and
missed `docs/agents.md:513` and `docs/troubleshooting.md:223` — both of which
turn out to carry a claim worth checking, see the Addendum in §Results. Recorded
because "never truncate a docs survey" is worth nothing if the pattern is the
thing that truncates it.)* Full survey in `w4-1/docsurvey.txt`.

`:425` is the passage's heading and `:428` is the passage itself.
`docs/operations.md:79` is the operator-facing restatement — "the k8s-agent's
pod GC sweeps every ~1 minute and deletes pods whose run has reached a terminal
state" — and note that it states **L1 and half of L2 only**: it does not mention
the 404 branch, and it does not mention L3 or L4.

### Settled by the rig, cited not re-litigated

`w4-rig.md` §Step 7 measured what a run does when its Pod is deleted mid-flight:
it reaches **`Failed`** through the ordinary step-error path, essentially
immediately, with the step row reading `status=Failed exit_code=137` (128 +
SIGKILL) and the run's `updated_at` **98 ms** after step end. The 90 s stuck-run
reaper is uninvolved. Two limits travel with that result and are carried here:
**n = 1**, and the delete marker's **1 s resolution**. Its caveat also stands —
the recorded evidence is indistinguishable from a self-inflicted 137, with no
error field, no stderr row, and nothing logged by the agent.

**Consequence for this scenario's design:** an arm that expects a run to *hang*
after losing its Pod is wrong. Part A is built around "a spurious delete kills
the run", and Part B2 is the arm that actually performs that kill, on purpose,
to measure it.

---

## The instrument problem, and how this scenario solves it

Three properties of the GC make it awkward to observe, and the method below is
shaped entirely by them.

1. **A sweep that decides "leave everything alone" logs nothing.** `podgc.go`
   logs on skip, on delete, and on failure — never on entry, never on a clean
   pass. So the sweep is invisible unless it acts.
2. **The phase cannot be commanded** (Correction 1). The ticker fires at
   `agentStart + 60k` and nothing exposes k.
3. **`kubectl` sees the effect but not the actor.** A pod that vanishes could
   have been deleted by the GC, by `executeRun`'s deferred cleanup, or by the
   pool's idle timeout.

**The beacon.** Every arm below plants a *synthetic orphan Pod*: an ordinary
`busybox` pod in namespace `ci` carrying exactly the two labels `listRunPods`
selects on — `app=unified-cd-agent` and `unified-cd/runId=<a fresh random
UUID>` — and no pool annotations. The controller genuinely 404s that UUID
(`handleGetRun`, `api_runs.go:167-181`, returns 404 only for
`store.ErrRunNotFound` and 500 for anything else), so the GC's own predicate
resolves it as "definitively gone" and deletes it. Nothing else in the system
touches it: it backs no run, so no `executeRun` defer knows about it, and it
carries no pool annotation, so the pool ignores it.

That single device does three jobs:

- it **is Part B1** — a definitive 404 on the real path, with no fault
  injection anywhere;
- it **times the sweep**, converting an invisible tick into a logged
  `pod GC deleted orphaned pod` line with a `slog` timestamp, which is how the
  phase is learned and how each subsequent arm is scheduled;
- it is a **positive control inside every other arm** — when a beacon planted
  alongside the arm's subject is deleted in the same sweep, "the subject
  survived" cannot be confused with "the sweep never ran".

**Cost, stated up front.** Each arm needs its subject alive across a sweep whose
phase is known only from the previous beacon, so each attempt costs up to
60 seconds and can miss. **The attempt count for every arm, successful and
failed, is reported in the Results.**

---

## Rig and configuration

Bring-up exactly as `w4-rig.md` §Bring-up: the plain
`test/ha/docker-compose.ha.yaml` (the `k8senroll` overlay is **not** used — this
scenario needs no 403 control, and the overlay's missing-kubeconfig gotcha kills
all three controllers), then `test/edgecase/tools/w4/w4-up.sh`.

**No deviation from the rig's defaults for Parts A-C.** `w4-agent-config.yaml`
is used as committed: `namespace: ci`, `maxConcurrent: 1`, `poolIdleTimeout: 5m`
(comfortably longer than any window here, so the pool's own eviction cannot be
mistaken for a GC delete), `podStartTimeout: 60s`.

Fixtures, both already committed and already validated through the real
`dsl.Parse` (`w4-rig.md` §Step 4): `w4-longpod.payload.json`
(`edge-w4-longpod`, 120 × 1 s ticks — long enough to stay live across two
sweeps) for Parts A and B2, and `w4-reuse.payload.json` (`edge-w4-reuse`,
`podTemplate.reuse: true`) for Part C. **No new fixture is needed and none is
added.**

Fault injection is `test/edgecase/tools/w4/w4-k8s-inject.sh block <mode>`, the
one-way agent→controller partition described and measured in `w4-rig.md` §Step 5.
Two modes are used, and the pair is the scenario's central discrimination:

| mode | what the agent's `GetRun` gets | `isRunNotFound` | predicted GC action |
| --- | --- | --- | --- |
| `reset` | transport failure, no status (`curl_exit=52`) | false | **skip** (L4) |
| `404` | `HTTPError{StatusCode: 404}` | **true** | **delete** (L2) |

`w4-rig.md` §Step 5 records two properties of this verb that shape the schedule:
`block` now **asserts** its arm (probe must fail in the mode's shape) rather
than printing one, and `hang` is **not** used here because `unblock` does not
sever hanging requests, costing ~24 × 2 s samples of recovery against `reset`'s
6 s. Every armed window is kept **under 90 s** so the controller's stuck-run
reaper (`staleAfter` 90 s) cannot fail the live run out from under the arm and
turn a skip test into a terminal-status test.

---

## Part A — the contract: a non-404 error during a sweep, over a live run

**Method.** With the sweep phase known from a beacon:

1. Trigger `edge-w4-longpod` and wait for its Pod to be `Running`.
2. Plant a second beacon (fresh UUID) alongside it.
3. Arm `block reset` a few seconds before the predicted sweep, and hold it
   ~20 s past the sweep — a window well under the 90 s reaper threshold.
4. `unblock`, and let the following (unarmed) sweep run with the longpod run
   still live.

**Predicted, from `podgc.go:76-81`.** During the armed sweep, `GetRun` fails
with a transport error for *both* pods; `isRunNotFound` is false for both; both
are skipped with a `WARN "k8s: pod GC skipping pod (run status unknown)"` naming
the pod and runId. At the next sweep, the beacon's 404 resolves and it is
deleted — that is L4's "retries next sweep", measured on a subject whose correct
disposition is known — while the longpod's `GetRun` returns `Running`, which is
not terminal, so its Pod is correctly left alone and the run finishes normally.

**What would be a violation.** The live run's Pod deleted during the armed
sweep — a direct contradiction of `docs/high-availability.md:431-433`, and
(given the rig's Step 7 result) an I1 violation as well, because the run would
reach `Failed` while nothing about the job actually failed.

**What is measured, not assumed.** The skip lines are the evidence that the
sweep ran *and* took the skip branch; the beacon's survival across the armed
sweep and its deletion at the next one is the evidence that the sweep was live
and that the skip is per-cycle rather than permanent; and the run's own terminal
status is the evidence that nothing killed it.

**Result:** see §Results below.

## Part B — the 404 branch: does a definitive not-found actually delete?

Without this, Part A proves nothing: a Pod that survives a sweep is
indistinguishable from a Pod that no sweep ever looked at.

**B1 — the clean control, no fault injection.** Plant one beacon, wait, and
confirm it is deleted by a `pod GC deleted orphaned pod` line carrying
`runFound=false`. The 404 here is minted by the real controller from
`store.ErrRunNotFound`, on the real route the agent's `GetRun` uses
(`GET /api/v1/runs/{id}`, `server.go:352`). B1 also establishes the sweep phase
every other arm is scheduled against, and confirms L1's ~1 minute by the
interval between consecutive beacon deletions.

**B2 — the same lever as Part A, one status code apart.** Arm `block 404` over a
sweep while `edge-w4-longpod` is live. Prediction from `isRunNotFound`
(`podgc.go:57-60`): the interposer's 404 is `HTTPError{StatusCode: 404}` and is
therefore **indistinguishable from the controller's own** "run not found", so
the GC deletes a Pod backing a live run — and per `w4-rig.md` §Step 7 the run
then fails with exit code 137.

**B2 is deliberately a harm demonstration, and it is what makes Part A a
discrimination.** Both arms use the same instrument, the same fixture and the
same window; the only difference is the shape of the failure the agent sees.
`reset` → skipped and the run lives. `404` → deleted and the run dies. If B2
produces the delete, then Part A's survival is a decision, not an absence.

**B2's second question, which is the finding candidate.** The 404 in B2 is
minted by an **intermediary**, not by the controller — which is exactly what a
misrouted ingress, a path-rewriting proxy, or a controller rolled back past a
route would produce. `client.go:107-108` discards the response body before
`isRunNotFound` ever sees it, so the agent has no way to tell the two apart.
Whether that is worth filing depends on what B2 measures; it is stated here as a
prediction so the Results can confirm or refute it. (Related but **not** the
same as the campaign's existing `agent 4xx report abandonment` note, which is
about the *report* path treating any <500 as permanent; this is the *GC* path
treating one specific status as authority to delete.)

**Result:** see §Results below.

## Part C — pool-managed Pods are never touched

**Method.** Run `edge-w4-reuse` once and let it release its Pod to the pool. The
released Pod is then in exactly the state the GC would otherwise collect: its
`unified-cd/runId` label names a run that is **`Succeeded`**, i.e. terminal, so
`podGCDecision`'s second clause is true and only the `poolManaged` guard stands
between it and deletion. Plant a beacon, and let ≥ 1 sweep pass.

**The annotation is verified during the window, not assumed.** `poolManaged` is
`pod.Annotations["unified-cd/pool-status"] != ""` and nothing else. A fixture
that failed to set it would produce a pod the GC deletes for the ordinary
terminal-run reason, and the arm would look like a *pass* if read only as "did
the pod survive". So `w4-k8s-inject.sh annotations` (`pool.go:20-31`) is run
against the pooled pod while the sweep window is open, and its `pool-status`
recorded.

**Also recorded:** that the pooled pod's run is genuinely terminal (from the
`runs` row), so the arm is testing the guard rather than testing a run that
simply had not finished yet.

**Predicted.** Beacon deleted; pooled Pod untouched; **no log line at all** for
the pooled Pod — `podGCDecision` returning false takes `continue` at
`podgc.go:84` with no `slog` call, so silence is the expected signal and the
beacon is the only proof the sweep happened.

**What would be a violation.** The pooled Pod deleted — a direct contradiction
of `docs/high-availability.md:431` ("Pods still owned by the Pod-reuse pool are
never touched"), and a warm pool that empties itself once a minute.

**Result:** see §Results below.

## Part D — two unsynchronised sweeps

`docs/high-availability.md:351-352` justifies the shipped `replicas: 2` in part
because "pod GC only touches pods whose runs are terminal or absent". With no
leader election and no advisory lock, the shipped default is **two agent
processes each running an independent, unsynchronised sweep over the same
namespace**, and `listRunPods` is namespace-scoped — neither agent's sweep is
limited to its own runs.

**This is run as a measurement, not a code read.** The rig runs one agent by
default, but a second is cheap on this route: mint a second credential for a
different agent id through the same product path, start a second interposer on
`127.0.0.1:18098` with those credentials, and start a second `k8s-agent` against
a copy of `w4-agent-config.yaml` whose only edit is `server:`. Both agents then
sweep namespace `ci` with independent 60 s tickers at unrelated phases. **If
that bring-up fails, this Part is downgraded to an explicitly labelled code-read
observation rather than reported as a measurement.**

**Method.** With both agents up and registered, plant **two** beacons and watch
both agents' logs across ≥ 2 sweeps each. Measure:

1. Does each agent's sweep evaluate Pods it did not create? (Both beacons were
   created by neither agent, so any action on them answers this.)
2. What does the loser of a delete race log — `pod GC delete failed` with a
   Kubernetes `NotFound`, or nothing at all because the pod was already gone
   from its own `ListPods`?
3. How many `GET /api/v1/runs/{id}` requests does one orphan Pod cost per minute
   at two replicas, and how does that scale?

**What would be a violation.** A pod belonging to a *live* run deleted as a
result of two agents sweeping — which would contradict `:351-352`'s safety
argument directly. **What is more likely, and is still worth recording:** a
benign but noisy delete race, and a per-Pod controller request rate that is
linear in replica count with no coordination.

**Result:** see §Results below.

---

## Recording rules this scenario is bound by

- A **violation** contradicts an invariant (I1-I7) or a statement in `docs/*.md`.
  An inline comment in a function body is not a contract — which matters
  particularly here, because `podgc.go:50-53` and `:19-24` carry doc comments
  that state the skip rule as clearly as the docs do. **The filing rests on
  `docs/high-availability.md:428-433`, not on those comments.**
- An **observation** says "observation" in its title and repeats it in its
  Severity line.
- `FINDINGS.md` is grepped for the **finding**, not merely for the doc text,
  before anything is appended.
- Every number traces to a capture whose window covers it; derived, inferred and
  code-read values are labelled as such.
- **"Never" is not written for a window this scenario ended itself.** The
  natural phrasing of Part A's result — "the Pod was never deleted" — is the
  wrong one; what is measured is that it survived a specific, bounded number of
  sweeps, and that is what is written.

---

## Results

Executed 2026-08-01 against the rig described above: plain
`test/ha/docker-compose.ha.yaml` brought up cold (**all three controllers came
up** — the W4-0 bootstrap-PAT race did not fire; third consecutive 3/3, so
`w4-rig.md`'s correction that it is a race and not a certainty continues to
hold), then `w4-up.sh` with no deviation from the committed
`w4-agent-config.yaml`. Agent A (`k8s-agent-w4`) registered at **07:41:29.3989**
UTC. The interposer answered **4** enrollment exchanges across the session's
proxy logs (`teardown-agents.txt`), so the bypass was in effect and the
disclosure above applies. Raw captures are in the session scratchpad under
`w4-1/`; every number below traces to one of them.

> **Two clocks, as `w4-rig.md` warns.** The agents and the interposer log local
> time (`+09:00`, e.g. `16:44:29`); the controller, the database and every
> capture script log UTC (`07:44:29Z`). **Local = UTC + 9 h.** Every figure in
> this section is stated in **UTC**.

### Verdict summary

| Part | Result |
| --- | --- |
| A. Non-404 error during a sweep, over a live run | **CONFORMANT** — both Pods skipped with the `WARN`, the live run's Pod untouched, the run `Succeeded`; the beacon deleted at the *next* sweep, which is L4's "retries next cycle" measured |
| B1. Definitive 404, no fault injection | **CONFORMANT** — deleted, `runFound=false`. This is the discrimination that makes A a decision rather than an absence |
| B2. An **intermediary**-minted 404 | **1 observation, major** — the GC deleted the Pod of a live `Running` run, and the run ended `Failed` with a NULL exit code, no reason in its own log, and its terminal status written by the controller's heartbeat reconcile rather than by the agent. Not a violation: `:428-430` says a 404 means "definitively gone", so the code does what is published |
| C. Pool-managed Pod | **CONFORMANT** — survived **8** GC evaluations across two agents with zero GC log lines, then was removed by the **pool's own** idle timeout, which is the lifecycle owner `podgc.go:99-100` names |
| D. Two unsynchronised sweeps | **measured, not code-read** — agent B deleted two Pods it did not create; the loser of the race logs nothing; per-Pod controller load is **exactly linear** in replica count (1/min → 2/min) |
| L1 (~1 minute interval) | **CONFORMANT** — 12 consecutive ticks over 720.019 s = **60.0016 s/tick** |
| Spun out of Part A | **1 violation** — the k8s agent's **stdout** log path has no buffer, no retry and no drop marker; 21 of 122 lines permanently lost to a 22.0 s blip, contradicting `docs/troubleshooting.md:889-898` |

**Attempt count, reported honestly because the brief required it: 5 timed
trials, 5 successes, 0 failed attempts.** That is *not* a claim that the 60 s
phase is easy to race — it is the beacon paying off. The first beacon (Part B1)
converted an invisible tick into a `slog`-timestamped line, after which every
armed window was **scheduled against a known phase** rather than raced against
an unknown one. The measured phase held to ±2 ms over twelve minutes, so a
±8 s arming margin was ample. **A scenario that skipped the phase-finding step
would have faced a genuine one-in-three-per-attempt race**, which is what the
brief anticipated.

### The four contract limbs, and where each was measured

| Limb (`docs/high-availability.md`) | Arm | Verdict |
| --- | --- | --- |
| L1 — sweeps every ~1 minute (`:428`, `:444`) | every capture | **holds**, 60.0016 s/tick |
| L2 — terminal or definitively-gone (404) → delete (`:428-430`) | B1, B2, C | **holds** |
| L3 — pool-owned Pods never touched (`:431`) | C | **holds** across 8 evaluations |
| L4 — any other error → skip for the cycle, not delete (`:431-433`) | A | **holds** |

**No limb of the documented contract was contradicted, and that is the
scenario's headline.** Both findings below are about the *edges* of a rule the
product implements exactly as written: what a 404 is allowed to mean, and what
happens to the log of the run the GC is trying not to kill.

### I1 and I2 — the invariant limbs, adjudicated

**I2 is a null limb and is recorded as one.** No side effect is duplicated
anywhere in this scenario; no fixture here carries I2's append-only detector.
Nothing is filed on it.

**I1 was attacked in Part B2 and is *not* filed on, after argument.** The
temptation is real: a healthy 120 s job was destroyed at t≈45 s by its own
agent's GC and recorded `Failed`. But I1's text is "every run reaches exactly
one terminal state, and that state matches what actually happened", and the run
*did* fail — its container was SIGKILLed and it never produced its remaining
output. Exactly one terminal state was reached. Filing I1 here would be the
stretch `FINDINGS.md:1509` forbids, of the same kind W2-5 and W2-7 were
corrected for. **What is wrong in B2 is not the arithmetic of the status; it is
that the agent chose to destroy the run on evidence it could not attribute**,
and that is filed on its own terms.

### L1 — the interval, measured over 12 consecutive ticks

Every logged sweep instant for agent A (UTC, converted from the agent's local
`+09:00`), from `final-logs/k8s-agent.log`:

```
07:42:29.4276   deleted beacon-1
07:44:29.4110   skipped ucd-run-2f1491cf-895b-41   (+2 ticks)
07:44:29.4121   skipped w4-1-beacon-2
07:45:29.4341   deleted beacon-2                   (+1 tick)
07:50:29.4241   deleted ucd-run-e20e9975-c031-45   (+5 ticks)
07:50:29.4342   deleted beacon-3
07:54:29.4463   deleted beacon-4                   (+4 ticks)
```

Span `07:42:29.4276` → `07:54:29.4463` = **720.0187 s over 12 ticks =
60.00156 s/tick**. First tick landed **60.0287 s** after `k8s agent registered`
(`07:41:29.3989`), consistent with `time.NewTicker` starting at `Run` entry
(`agent.go:139`). Sweeps that acted on nothing logged nothing, exactly as
predicted — the gaps above are not missed ticks, and the controller's own access
log confirms it: `GET /api/v1/runs/0fe86020-…` (the pooled Pod's runId) appears
at `07:54:29`, `07:55:29`, `07:56:29`, `07:57:29`, `07:58:29` — the five silent
A-sweeps in that stretch, each resolving that Pod and deciding to leave it.

### Part A — the contract (attempt 1 of 1; run `2f1491cf-895b-4146-ac42-973c62408c44`)

Captures `partA-attempt1.txt`, `partA-outcome.txt`.

```
07:43:44.829  TRIGGER edge-w4-longpod  runId=2f1491cf-…
07:43:45.219  BEACON w4-1-beacon-2 planted  runId=411aa927-…
07:44:24.193  ARMING block reset
07:44:24.232  ARM VERIFIED (mode=reset)  — probe curl_exit!=0, direct :18080 control still 200
07:44:29.411  *** SWEEP, INSIDE THE ARMED WINDOW ***
07:44:45.160  UNBLOCKING  → proxy answers 200 again
07:45:29.434  *** SWEEP, UNARMED ***
07:45:49.911  run reaches Succeeded
```

**The armed sweep, verbatim** (`final-logs/k8s-agent.log`, local `+09:00`):

```
{"time":"2026-08-01T16:44:29.4110442+09:00","level":"WARN","msg":"k8s: pod GC skipping pod (run status unknown)",
 "pod":"ucd-run-2f1491cf-895b-41","runId":"2f1491cf-…","error":"Get \"http://127.0.0.1:18099/api/v1/runs/2f1491cf-…\": EOF"}
{"time":"2026-08-01T16:44:29.4121029+09:00","level":"WARN","msg":"k8s: pod GC skipping pod (run status unknown)",
 "pod":"w4-1-beacon-2","runId":"411aa927-…","error":"Get \"http://127.0.0.1:18099/api/v1/runs/411aa927-…\": EOF"}
```

**Both** Pods were skipped — the live run's and the beacon's — in **1.06 ms**,
and neither was deleted. The error is a transport failure with no status
(`EOF`), so `isRunNotFound` (`podgc.go:57-60`) is false and `podgc.go:76-81`
takes the `continue`. `kubectl` sampling at 2 s through the armed window shows
both Pods `Running` at every sample.

**The next sweep is what makes "skip for the cycle" a measurement rather than a
tautology.** At `07:45:29.4341`, unarmed, the GC resolved the *same two* Pods
again and split them:

```
{"…16:45:29.4341486+09:00","level":"INFO","msg":"k8s: pod GC deleted orphaned pod",
 "pod":"w4-1-beacon-2","runId":"411aa927-…","runFound":false}
```

The beacon — whose correct disposition is "delete", established independently in
B1 — was deleted on the retry. The live run's Pod was resolved (`GetRun` → 200,
`Running`), found non-terminal, and left alone with no log line. **So the skip
was per-cycle, not permanent, and the retry produced the right answer for each
Pod.**

**The run finished normally.** `runs`: `Succeeded`, `claimed_at
07:43:45.553269`, `updated_at 07:45:49.911182`. `step_reports`: one row,
`status=Succeeded exit_code=0`, `07:43:49.5885` → `07:45:49.803917` —
**120.215 s**, the fixture's own 120 × 1 s loop, so the partition cost the run
nothing in wall time.

**Stated precisely, because the natural phrasing is the wrong one:** the live
Pod was **not** "never deleted". It survived **two** GC evaluations while its run
was live — one armed, one not — and was then removed by `executeRun`'s ordinary
deferred cleanup after the run succeeded. That is the window this scenario
measured, and it is the window this scenario ended.

### Part B1 — the 404 branch, no fault injection (attempt 1 of 1)

Capture `partB1-probe.txt`, `partB1.txt`.

The controller's own answer on the route the agent's `GetRun` uses
(`GET /api/v1/runs/{id}`, `server.go:352` → `handleGetRun`,
`api_runs.go:167-181`):

```
GET /api/v1/runs/{random-uuid} -> 404
body: run not found: d5aadebd-c1dd-42db-8523-fa74a6c6b4ee
```

Beacon-1 planted `07:42:14.438`; deleted at the sweep `07:42:29.4276`, **14.99 s
later**, with `runFound=false`. `kubectl` saw it `Running` at `07:42:27.814`,
`Terminating` at `07:42:30.030`, `NotFound` at `07:42:32.268`.

**This is the load-bearing control for Part A.** The same GC, the same pod-shape,
the same namespace: a 404 deletes. Part A's survivor was therefore skipped, not
overlooked. Note also what B1 proves about the *lister*: `listRunPods` selects on
two labels and nothing else — no owner reference, no `ucd-run-` name check, no
agent-id predicate — so a Pod this agent never created, in a namespace it shares,
is a deletion candidate. That is the property Part D turns into a measurement.

### Part B2 — an intermediary-minted 404 over a live run (attempt 1 of 1; run `e20e9975-c031-45b1-829f-59812503cab8`)

Captures `partB2-attempt1.txt`, `partB2-detail.txt`, `partB2-tail.txt`,
`partB2-attribution.txt`.

```
07:49:44.320  TRIGGER edge-w4-longpod  runId=e20e9975-…
07:49:45.176  claimed_at
07:49:48.204  step "tick" started_at
07:50:23.694  ARMING block 404
07:50:23.733  ARM VERIFIED (mode=404) — probe http_code=404, direct :18080 control still 200
07:50:29.424  *** SWEEP: the live run's Pod is DELETED ***
07:50:44.682  UNBLOCKING
07:50:59.431  run reaches Failed
```

The sweep, verbatim — note that the run was `Running` at the controller
throughout, verified by this scenario's own polls against the **direct**
`:18080` control at `07:50:44`, `07:50:48`, `07:50:51`, `07:50:55` and
`07:50:58`:

```
{"…16:50:29.4240822+09:00","level":"INFO","msg":"k8s: pod GC deleted orphaned pod",
 "pod":"ucd-run-e20e9975-c031-45","runId":"e20e9975-…","runFound":false}
{"…16:50:29.4342474+09:00","level":"INFO","msg":"k8s: pod GC deleted orphaned pod",
 "pod":"w4-1-beacon-3","runId":"d89f3d6f-…","runFound":false}
```

**The GC destroyed a healthy run 41.2 s into a 120 s job**, because a 404 minted
by something that is not the controller is byte-indistinguishable, at
`isRunNotFound`, from the controller's own "run not found" — and
`client.go:107-108` has already replaced the body with `"response omitted"`
before anything could tell them apart. The interposer's own log names the
request it answered:

```
2026/08/01 16:50:29 BLOCK #53 GET /api/v1/runs/e20e9975-… mode=404
```

**What the run looks like afterwards — four separate degradations, all measured:**

1. **Terminal status written by the controller, not the agent.** `runs.updated_at
   = 07:50:59.430803`. The only heartbeat that reached the controller in
   `[07:50:40, 07:51:09]` is `POST /api/v1/agents/k8s-agent-w4/heartbeat → 204`
   at `07:50:59.433999` — 3.2 ms later, i.e. the same request. `claimed_at +
   heartbeatReconcileGrace(60 s)` expired at `07:50:45.175696`, so the run was
   **14.255 s past grace** at the first post-grace heartbeat. **Zero**
   `stuck-run reaper` or `agent reconcile` lines exist in any controller log for
   the whole session. This is the heartbeat-reconcile signature W1-5 and W2-2
   established (a path that logs nothing on success), and the attribution is
   **derived** from those four facts rather than from a log line, because no log
   line exists.
2. **The agent gave up on reporting.** Two
   `{"level":"ERROR","msg":"permanent error, giving up retry","status":404}`
   lines at `16:50:29.4462` and `16:50:29.4479` — the step report and the run
   finish, abandoned 22 ms after the delete. **This is a live reproduction of
   the already-filed `FINDINGS.md:305` (W1-5 4xx-permanent abandonment), on a
   different agent implementation; it is corroboration and is deliberately not
   re-filed.** The agent logged **nothing at all** after `16:50:44.415`; its log
   ends there.
3. **`step_reports.exit_code` is NULL.** Contrast `w4-rig.md` §Step 7, where an
   externally-issued `kubectl delete` produced `exit_code=137`. Here the 137
   existed but its report was abandoned at (2), so the row carries `Failed` with
   no exit code at all — *less* forensic evidence than the manual-delete case.
4. **37 log rows of an expected 122**, and no drop marker. See the spin-out
   below; the loss mechanism is the same one Part A exposed.

**The one thing that is *better* here than in `w4-rig.md` §Step 7:** this kill
is greppable. `pod GC deleted orphaned pod` names the Pod and the runId, so an
operator who thinks to read the *agent's* log can find it. That line is the only
record anywhere; nothing reaches the run's own log, and the controller sees only
a heartbeat.

**Scope of the precondition, stated so the finding is not inflated.** B2 requires
something other than the controller to answer `GET /api/v1/runs/{id}` with a
404. On this rig that was the interposer. In a real deployment the candidates are
ordinary: an ingress or service-mesh route that stops matching after a path
rewrite, a controller rolled back past a route the agent still calls, a proxy
answering 404 for an unknown upstream during a rolling upgrade, or an auth
gateway that 404s rather than 401s to avoid leaking existence. Every one of them
is a *transient* condition that the GC's own design principle — skip when you
cannot tell — is meant to survive, and 404 is the single status excluded from
that principle.

### Part C — pool-managed Pods (attempt 1 of 1; run `0fe86020-e240-4386-a913-d4f0ec1567d2`)

Capture `partC.txt`, `partD-requestrate.txt`.

`edge-w4-reuse` ran and released its Pod to the pool. The state under test,
read live with `w4-k8s-inject.sh annotations` (`pool.go:20-31`) **before** the
sweep window opened:

```
== ucd-run-0fe86020-e240-43 ==
  pool-status  = idle
  pool-key     = 3c1d2612f14e2dd35233f878a28578b5
  pool-run-id  =
  label runId  = 0fe86020-e240-4386-a913-d4f0ec1567d2
  phase        = Running
```

and the run that label names is **terminal**: `runs` reads
`0fe86020-…|Succeeded|07:53:56.143493`. **So `podGCDecision`'s second clause was
true and only `poolManaged` stood between this Pod and deletion** — the arm
tests the guard, not a run that merely had not finished.

**The annotation was verified present, not assumed** — that check is the whole
point, since a fixture that failed to set `pool-status` would be deleted for the
ordinary terminal-run reason and the arm would still *look* like a pass. It read
`idle` before the window and `idle` after it, unchanged.

**How many sweeps it survived, counted rather than asserted.** The controller's
access log records one `GET /api/v1/runs/0fe86020-…` per sweep per agent, and
they land exactly on the two agents' tick instants:

```
07:54:29  07:55:29  07:56:05  07:56:29  07:57:05  07:57:29  07:58:05  07:58:29
   (A)       (A)       (B)       (A)       (B)       (A)       (B)       (A)
```

**8 GC evaluations, by two independent agents, zero deletions and zero log
lines about this Pod** — silence being the predicted signal, since
`podGCDecision` returning false takes `continue` at `podgc.go:84` with no `slog`
call. Beacon-4, planted alongside at `07:53:58.491`, was deleted in the *first*
of those eight sweeps (`07:54:29.4463`), so every one of them was a live sweep
that saw this Pod and declined it.

**The Pod's actual lifecycle owner then removed it, on schedule.** At
`07:58:59.366` (local `16:58:59.3656`) the agent logged
`pool: evicting idle pod (timeout)` with `idleSince 07:53:56.157` — exactly
`poolIdleTimeout: 5m` later. That is `podgc.go:99-100`'s "Pods managed by the
reuse pool are left to the pool's own idle-timeout/Restore logic" happening
verbatim, and it is a stronger result than the Pod merely persisting: the Pod
was not forgotten, it was owned.

### Part D — two unsynchronised sweeps (attempt 1 of 1)

Captures `partD-bringup.txt`, `partD.txt`, `partD-requestrate.txt`.

**This is a measurement, not a code read.** A second agent was brought up on the
same route: a second credential minted for `k8s-agent-w4b` through the product's
own enrollment path, a second interposer on `127.0.0.1:18098`, and a second
`k8s-agent` against a copy of `w4-agent-config.yaml` whose only edit is
`server:`. Both registered, both `[pod container]`, both `kind:kubernetes`, both
sweeping namespace `ci`:

```
k8s-agent-w4   registered 07:41:29.3989  → sweeps at 07:5X:29.4
k8s-agent-w4b  registered 07:55:05.3611  → sweeps at 07:5X:05.4
```

**The two tickers are 35.96 s out of phase and nothing brings them together** —
no leader election, no lock, no jitter, no shared state. That is the shipped
`replicas: 2` shape reproduced.

**1. Each agent acts on Pods it did not create — confirmed by effect.**
Beacons 5 and 6 were planted at `07:55:29.471` and `07:55:29.876` by this
scenario, not by either agent. **Agent B deleted both**, at its own tick:

```
{"…16:56:05.3897829+09:00","…","msg":"k8s: pod GC deleted orphaned pod","pod":"w4-1-beacon-5",…,"runFound":false}
{"…16:56:05.3981137+09:00","…","msg":"k8s: pod GC deleted orphaned pod","pod":"w4-1-beacon-6",…,"runFound":false}
```

An agent 71 seconds old, which had claimed nothing and created nothing, deleted
two Pods on its second sweep. `listRunPods`'s two-label selector is the whole
authorisation.

**2. The loser of the race logs nothing, because there is no race to lose.**
Agent A's next tick was `07:56:29.4`, **24.0 s** after B's. By then the Pods were
gone from A's own `ListPods`, so A produced **zero** GC lines in the window —
not a `pod GC delete failed`, not anything. A genuine `DeletePod` collision would
need the two ticks inside the ~10 ms it takes one sweep to run, which at 60 s
periods and independent phases is a ~0.017 % coincidence per sweep pair. **So the
predicted "benign but noisy delete race" does not occur; the failure mode is
quieter than predicted, and the practical consequence is that neither agent's log
tells you the other one exists.**

**3. Per-Pod controller load is exactly linear in replica count.** Counting
`GET /api/v1/runs/0fe86020-…` (the surviving pooled Pod, resolved by every sweep
of every agent) per minute across the natural one-agent → two-agent transition:

| minute (UTC) | agents live | GC resolves |
| --- | --- | --- |
| 07:54 | A | 1 |
| 07:55 | A | 1 |
| 07:56 | A + B | 2 |
| 07:57 | A + B | 2 |
| 07:58 | A + B | 2 |

**No coordination and no de-duplication: cost is `pods × replicas` requests per
minute, forever, against the controller's `GET /api/v1/runs/{id}`.** At the
shipped `replicas: 2` with a busy namespace this is small; it is recorded
because it is unbounded in both factors and nothing in the code caps it.

**4. `docs/high-availability.md:351-352` holds.** No Pod belonging to a live run
was deleted as a result of two agents sweeping. The safety argument — "pod GC
only touches pods whose runs are terminal or absent" — is exactly what was
observed, including B correctly declining A's pooled Pod on four consecutive
sweeps. **No violation is filed on Part D**, and the costs above are recorded as
an observation.

### Spin-out of Part A — the k8s agent's stdout log path has no buffer, no retry and no marker

Captures `partA-logloss.txt`, `partA-logloss-detail.txt`, `partB2-detail.txt`.

Part A's fixture emits `W4-LONGPOD-BEGIN`, `TICK 0`…`TICK 119`, `W4-LONGPOD-END`
— **122 lines**. The run reached `Succeeded` with `exit_code=0`. Its stored log
holds **101** rows, all `stdout`, all distinct. **Ticks 35-55 — 21 lines — are
permanently absent**, and the loss is invisible in every column but the text:

```
seq | ts                            | line
 36 | 2026-08-01 07:44:23.670044+00 | W4-LONGPOD-TICK 34
 37 | 2026-08-01 07:44:45.693951+00 | W4-LONGPOD-TICK 56
```

`seq` is **contiguous across the gap** (the controller assigns it at insert, so
lines that never arrived burn no sequence number), and `count(*) where line like
'%dropped%'` is **0** — no `[N log line(s) dropped: controller unreachable]`
marker. The hole is `07:44:23.670` → `07:44:45.694` = **22.024 s**, against an
armed window of `~07:44:23.7` → `07:44:45.16`. **The partition ended 82 s before
the step did**, so nothing about step-end timing is involved. Part B2 reproduced
it independently: 37 rows of an expected 122, last delivered line `TICK 35` at
`07:50:23.326`, arm at `07:50:23.694`.

**Mechanism, code-read and unambiguous.** The k8s agent splits its two streams:
`StepLogWriters` (`internal/k8sagent/backend.go:406-421`) gives **stderr** a real
`agentlib.NewLogPusher` with `StartAutoFlush`, and gives **stdout** a
`logLineWriter` (`internal/k8sagent/agent.go:458-493`) whose entire delivery
strategy is, per line:

```go
_ = lw.client.AppendLog(context.Background(), lw.agentID, api.LogAppendRequest{…})
```

One synchronous POST per line, on `context.Background()`, **with the error
discarded**. No batching, no pending queue, no cap, no `droppedLines` counter, no
retry, and nothing that could ever emit the marker. `Client.AppendLog`
(`internal/agent/client.go:213-216`) is a bare `c.do` and adds no retry of its
own. **Every stdout line whose single POST fails is gone, silently, at any
volume and any outage length.**

**This contradicts a published document.** `docs/troubleshooting.md:889-898`
describes the behaviour as a property of "the agent", unconditionally, and the
`Fix` section at `:908-912` states that after such a gap "only the buffered
stdout/stderr text for that window is gone" — i.e. it tells the operator that
stdout is buffered and that a gap will be reported. For the Kubernetes agent's
stdout, none of the mechanism it describes exists. Filed as a violation.

**Relationship to what is already on file, checked before filing.** This is
**not** W1-2's step-end-`Flush` loss (`FINDINGS.md:179`) — that is a `LogPusher`
whose bounded 5 s exit-time retry expires; here there is no `LogPusher` on the
path at all, and the outage ended long before the step did. It is **not** F9 /
the 1 MiB cap — 21 short lines are five orders of magnitude below it. And it is
the direct counter-example to W1-1's "zero log loss" result
(`FINDINGS.md:159`), which measured an **81 s** outage on the **host** agent and
lost nothing: the two agents do not share this code path, and the difference is
not a matter of degree.

### Addendum — "the cluster's own pod garbage collection" is not what reaps these Pods

The docs survey turned up two passages outside the GC's own section that
attribute the cleanup to Kubernetes rather than to unified-cd.
`docs/agents.md:513-514`: "Unlike the Kubernetes agent, where an orphaned pod is
eventually reaped by the cluster's own pod garbage collection, **the host agent
has no automatic container GC**"; `docs/troubleshooting.md:222-224` repeats it
almost verbatim. Both use the claim as the *contrast* that justifies telling
host-agent operators to prune by hand.

**Bounded measurement, and its bound is stated.** Beacon-7 was planted at
`07:59:22.699`; both agents were stopped at `07:59:24` (`teardown-agents.txt`).
The Pod was still `1/1 Running` at `08:00:17` and again at `08:04:08` — **4 m
46 s with no unified-cd agent process in existence** (verified: `ps` shows no
`k8s-agent` and no `enrollproxy`). Kubernetes did not reap it during that
window. This is a *bounded* observation: it shows the Pod survives that window
unattended, not that Kubernetes would never act on a longer horizon. **Recorded here rather than filed**, because the passage's operational
advice (host agents need manual pruning, k8s ones do not) is correct — every
sweep in this scenario cleaned up exactly as promised — and only its attribution
of the mechanism is wrong. A reader who believes it will look for the wrong knob
when the sweep stops happening: the sweep lives in `podgc.go` and dies with the
agent process.

### What the whole scenario says in one paragraph

The pod GC does what `docs/high-availability.md:428-433` says it does, on all
four limbs, measured on a live rig: it sweeps on a 60.0016 s tick, it deletes
terminal and definitively-gone Pods, it never touches pooled Pods, and when it
cannot resolve a Run it skips that Pod and retries next cycle — demonstrated by
a live run's Pod surviving an armed sweep in the same 1.06 ms window in which a
beacon that *should* be deleted was also spared, and by that beacon being
deleted on the very next sweep. The two findings sit either side of that
correct behaviour: the GC's one exception to "skip when uncertain" is a bare
status-code test on a response the agent has already stripped of provenance, and
the stdout stream of the run it is trying not to kill has no delivery guarantee
at all.

## Findings filed

**1 violation + 3 observations.** Parts A, B1, C and the L1/L2/L3/L4 limbs are
conformance and are filed as nothing. **The one violation is not against the pod
GC at all** — it is against `docs/troubleshooting.md`, and it was found because
Part A's own fault-injection window happened to be a mid-step controller outage
over a chatty run. Every finding the GC itself produced is an observation,
because on all four documented limbs it did exactly what it says it does.

| # | Kind | Title (see `FINDINGS.md`) |
| --- | --- | --- |
| 1 | **violation** (contract limb, major) | the Kubernetes agent's **stdout** log path is one un-retried POST per line with the error discarded — 21 lines lost to a 22.0 s blip on a `Succeeded` run, no marker, while `docs/troubleshooting.md:889-898` promises buffering and a drop marker |
| 2 | observation (major) | a 404 the controller did not mint makes the pod GC delete a live run's Pod; the run dies with a NULL exit code and no reason on any run-scoped surface |
| 3 | observation (minor) | the GC's Pod set is "two labels in a namespace" — no owner reference, no agent-id predicate — so any agent sweeps every other agent's Pods, and per-Pod controller load is `pods × replicas` per minute with no coordination |
| 4 | observation (minor) | the sweep interval is a literal at `agent.go:139` with no seam: no flag, env var, config field or test call site, and `runPodGC`'s own `<= 0` guard is unreachable |

`FINDINGS.md` was grepped for the **findings**, not merely the doc text, before
appending: `logLineWriter` **0** hits, `podGCDecision` **0**, `isRunNotFound`
**0**, `runPodGC` **0**, `listRunPods` **0**, `orphan-pod` **0**, `replicas: 2`
**0**, and `pod GC` **1** — which is `FINDINGS.md:1502`, a W3→W4 carry-forward
note about zombie budgeting, not a finding about the GC. Nothing here is a
re-filing. The two entries this scenario deliberately **corroborates without
re-filing** are `FINDINGS.md:305` (4xx-permanent abandonment, reproduced in B2)
and `FINDINGS.md:2233` (`w4-rig` §Step 7's indistinguishable 137, which B2
extends by showing the exit code can be lost entirely).

## Teardown

Agent B and its interposer were SIGTERMed (then SIGKILL-escalated) with their
final output captured; agent A was stopped through `w4-down.sh`, which SIGTERMs
it first so the graceful-drain path runs, prints each process's final lines, and
reports **4** intercepted enrollment exchanges across the rotated proxy logs
(1 + 2 + 1). `scratchpad/w4-1/final-logs/` holds both agents' complete logs.
The compose stack was taken down with `down -v` (mandatory between scenarios,
per the Garage volume rule). Every remaining `w4-1-beacon-*` Pod was removed by
hand — they are scenario instruments, not product artifacts, and none may
outlive the scenario. W4-0's `w4-spike-agent-pod` is left in place, as W4-0 left
it. Captures were swept for `uca_`/`ucr_`/`uce_` material followed by a
credential body; the mint script and both interposers log a 4-character kind
prefix or a UUID prefix only.
