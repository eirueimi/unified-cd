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

**Docs survey, untruncated, with hit counts.** `pod GC|podgc|orphan-pod|orphan
pod|orphaned Pod` across `docs/` returns **37** hits, of which **7** are outside
`docs/superpowers/`: `docs/high-availability.md:351`, `:425`, `:428`, `:444`,
and `docs/operations.md:79`, `:87`. (`:428` is the passage above; `:425` is its
heading.) `docs/operations.md:79` is the operator-facing restatement — "the
k8s-agent's pod GC sweeps every ~1 minute and deletes pods whose run has reached
a terminal state" — and note that it states **L1 and half of L2 only**: it does
not mention the 404 branch, and it does not mention L3 or L4. Full survey in
`w4-1/docsurvey.txt`.

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

*(To be filled in after execution. Committed before execution so the
plan-versus-outcome delta is visible.)*
