# W4-2 — the reuse swallow: a totally defeated feature, announced by one `WARN` that reads like a hiccup

**Wave W4, Task 5.** `podTemplate.reuse: true` promises that a Pod is returned
to a pool after a run and handed to the next one. The pool marks a finished Pod
idle by **updating its annotations**. If that single Kubernetes `Update` is
refused, `PodPool.ReleasePod` deletes the Pod instead, the pool stays empty, and
reuse never happens again for the life of the process — while every run keeps
reporting `Succeeded`. This scenario measures what an operator can actually see
when that happens, against what the documentation promises.

It also settles a second question that needs no cluster at all: what the
**default** `poolIdleTimeout` does, and whether `docs/configuration.md:458`'s
one-word gloss of it is true.

---

## First: the re-charter, recorded before anything else

**What the campaign spec chartered.** `docs/superpowers/plans/2026-08-01-edge-case-campaign-w4.md`
chartered W4-2 as "verify the known missing `pods update/patch` RBAC verbs — is
reuse still silently degraded?"

**That mechanism is dead, and here is the proof.**
`manifests/base/k8s-agent/rbac.yaml:7-13` grants
`create, get, list, delete, watch, update, patch` on `pods`, `pods/exec` and
`pods/log`, with a comment at `:9-12` naming pod reuse as the reason:

> update/patch are required by pod reuse (podTemplate.reuse): the pool marks
> a finished pod idle by updating its annotations (PodManager.UpdatePodAnnotations,
> PodPool.ReleasePod). Without update, that call is forbidden and the pool
> falls back to deleting the pod every run — reuse silently never happens.

The gap was real once and was closed in **PR #50 (`6b0bf8f`, 2026-07-15)**. All
generated bundles match. Running the scenario as chartered would consist of
reading four YAML files and confirming a fix.

**The substitute, holding the scenario ID and every recording rule fixed.**
W4-2 stops being an audit of the shipped manifests and becomes an experiment on
the *behaviour the manifests' comment describes*: **grant the pre-#50 Role to a
live agent and measure what a defeated reuse feature looks like from outside.**
The manifests are not modified, not proposed to be modified, and not the
subject. What is under test is the product's own observability and the
documented contract around `reuse`.

### The campaign-process correction, in one paragraph

The premise that motivated this scenario came from a cross-session memory note
that carries a **`RESOLVED`** header. The header did not travel with the claim.
The stale premise reached the **W4 exploration brief**, which chartered W4-2
around it; it then reappeared **inside Task 2's rig record** (`w4-rig.md`),
which asserted that "the shipped `manifests/` Roles lack `pods update/patch`, so
reuse is broken there" — an unevidenced claim, false at HEAD, attributed to
W4-0, which records nothing of the kind. Task 2's fix wave caught and corrected
it (`w4-rig.md` §Review corrections, F7; `FINDINGS.md:2260` carries the
retraction). So the same dead fact was published twice, in two documents, by two
different tasks, in one wave. **The lesson is not "check the memory notes"; it
is that a premise's provenance has to travel with the premise.** A claim
inherited from a note is worth exactly the freshness of the note, and a briefing
that restates it without its `file:line` at current HEAD converts a dated
observation into an apparent fact. Every `file:line` in this runbook was re-read
at this branch's HEAD before it was written down, and the corrections that
produced are recorded below rather than silently absorbed.

---

## Disclosure: the enrollment path under this scenario is bypassed

**Stated once, plainly, as every W4 scenario must.** Kubernetes agent
enrollment does not work at HEAD and cannot be made to work without a
product-code change (`w4-0-enrollment-spike.md`). Every W4 scenario, this one
included, runs against a controller whose **`POST /api/v1/agents/enroll` is
answered by test infrastructure** — an interposer that mints a credential
through the product's ordinary `"enrollment"` method instead. What was built,
and what was measured about it, is in `scenarios/w4-rig.md`, which carries the
standing form of this disclosure.

**Consequence for this scenario's findings:** nothing below says anything about
the Kubernetes enrollment path beyond what `w4-0` already records. Everything
this scenario touches — claim, Pod create, `PodPool.ClaimPod`, `ReleasePod`,
`Restore`, `StartEviction`, the pod GC — is the real product path, unmodified.

**A second, scenario-specific disclosure.** The credential the agent presents to
the **Kubernetes API server** and the credential it presents to the
**controller** are independent. Part B replaces only the former. The enrollment
bypass is untouched by it, and no measurement below conflates the two.

---

## Corrections to inherited facts, established BEFORE execution

Per the W1/W2/W3/W4 carry-forward rule, this task's brief carries a verified
code-facts block, and every wave so far has had one corrected by execution. All
of it was re-read at HEAD. **The load-bearing claims hold.** Five statements
need adjusting, and one of them adds a whole arm to Part B.

### The facts that held, re-read at HEAD

- `buildRestConfig` (`cmd/k8s-agent/main.go:96-105`) passes `cfg.Kubeconfig`
  (`internal/k8sagent/config.go:36`) straight to
  `clientcmd.BuildConfigFromFlags`. `Config.Validate` (`config.go:166-226`)
  **never touches the field** — no default, no existence check, no format check.
- `main.go:66-68` hands one client to `NewPodManager`, `NewExecutor` **and
  `NewPodPool`**, so the pool's `Update` goes through exactly that credential.
- `ReleasePod` (`pool.go:241-270`): on `Update` failure at `:255` it logs
  `slog.Warn("pool: failed to mark pod idle, deleting", …)` at `:257` and
  **returns `p.pm.DeletePod(…)`** at `:258`. The Pod is never appended to
  `p.pods`.
- `agent.go:316-318`'s `slog.Warn("k8s: failed to release Pod", …)` fires only
  when `ReleasePod` returns non-nil — i.e. only when the *fallback delete* also
  fails. On the RBAC path `delete` is granted, so the delete succeeds and this
  line does **not** fire.
- `agent.go:301-304` and `:327-331` are the contrast: `ClaimPod` and `CreatePod`
  errors **do** call `a.failRun`. `ReleasePod`'s does not.
- `podmanager.go:91-92` returns `Update`'s error **bare**, unwrapped, so the
  `error` field carries the raw `apierrors` 403 string.
- `pool.go:246-249`: a `Get` failure in `ReleasePod` short-circuits to
  `DeletePod` **with no log line at all** — a second, quieter swallow.
- **`patch` is a dead grant** and **`watch` is unexercised**:
  `grep -rn "\.Patch(\|\.Watch(" internal/k8sagent/` (untruncated, tests
  included) returns **0**. The three writers all use `Update` —
  `podmanager.go:91`, `pool.go:255`, `pool.go:336`.
- **`internal/k8sagent/` emits zero Prometheus metrics.** Re-verified as an
  enumeration, not a repetition, in §Part B3.

### CORRECTION 1 — `ClaimPod`'s misattributed log IS reachable, and Part B gains an arm for it

The brief's reconnaissance concluded that `ClaimPod`'s
`"pool: pod claim conflict, creating new pod"` (`pool.go:202`) is **never
reached** under an RBAC denial, because `ReleasePod` never appends to `p.pods`
so `pool.go:188-189`'s `if len(idle) > 0` is never true. **That is correct for a
process that started with an empty pool, and wrong in general.**

`PodPool.Restore` (`pool.go:279-354`) runs at every agent start
(`agent.go:84`). Its `poolStatusIdle` branch (`:317-325`) re-adopts an idle
pooled Pod into `p.pods` **by appending directly — it makes no `Update` call at
all.** So an agent that starts under the restricted credential in a namespace
that already contains an idle pooled Pod (left by any earlier agent, or by an
earlier build, or by this scenario's own Part A) has a **non-empty pool on its
first claim**. That claim pops the Pod, calls
`PodManager.UpdatePodAnnotations` (`podmanager.go:77-93`) — Get, then
`Update` — is refused, and takes `pool.go:198-205`: it logs a **403 as a
"claim conflict"** and deletes the Pod.

This is a strictly worse message than `ReleasePod`'s, because "conflict" names
optimistic-concurrency contention — a condition that is genuinely transient and
genuinely self-healing — for a permission error that is neither. **Part B2
exists to measure it**, and it costs one agent restart. It also means the two
messages are ordered: the *first* run after such a restart emits the `ClaimPod`
line, and every run thereafter emits the `ReleasePod` line.

### CORRECTION 2 — `Restore`'s own `Update` failure is quieter than the brief says, and is a THIRD swallow

The brief states that `Restore`'s `Update` (`pool.go:336`) has its error
"only `slog.Warn`ed, at `agent.go:85`". **It is not `slog.Warn`ed anywhere.**
`agent.go:84`'s Warn fires on `Restore`'s **returned** error, and `Restore`
returns non-nil only from the `List` at `:280-285`. The `Update` at `:336` is
inside `if err == nil { … continue }` (`:337-346`); when it fails, control falls
through to `:349`:

```go
slog.Info("pool: deleting orphaned in-use pod", "pod", pod.Name, "run", runID)
```

— an **INFO**-level line whose text describes a *deliberate* cleanup of an
orphan, emitted for a Pod that is neither orphaned nor being cleaned up for the
reason stated, with the 403 **discarded entirely** (no `error` field, no `err`
variable use). That is the third swallow on this path and the only one that
loses the error object. It is reachable on the restricted credential whenever an
agent restarts holding an **in-use** pooled Pod whose run has since finished.

### CORRECTION 3 — an unparseable `poolIdleTimeout` cannot reach the accessor

The brief states `PoolIdleTimeoutDuration()` "returns 0 for unset, unparseable
and literal `"0"` alike". The accessor does (`config.go:121-131`), but the
**unparseable case is unreachable in the shipped binary**: `Validate` at
`config.go:210-214` rejects it and the agent exits before the accessor is ever
called. Only **unset** and a value that parses to zero (`"0"`, `"0s"`) reach it.
This does not weaken Part D at all — Part D is about the **default**, which is
unset — and it is corrected because `FINDINGS.md:2294` (W4-3) already established
this exact boot-rejection behaviour for all three duration fields, and a runbook
that re-asserted the opposite would contradict the campaign's own record.

### CORRECTION 4 — the `poolIdleTimeout` docs survey is 6 hits, not 9

`grep -rn "poolIdleTimeout" docs/` returns **6** hits in **3** files
(the brief said 9). Excluding `docs/superpowers/`: **2 hits in 1 file** —
`docs/configuration.md:397` and `:458`. The brief's substantive point is
confirmed exactly: **`docs/configuration.md` is the only operator-facing page
that mentions the field at all**, and `docs/kubernetes-integration.md` — the
page that documents pod reuse — never mentions it.

The `reuse` survey reproduces the brief's numbers exactly: **176** hits in
**65** files case-insensitively across `docs/`; **25** in **8** files excluding
`docs/superpowers/` (`agents.md`, `configuration.md`, `field-reference.md`,
`high-availability.md`, `jobs.md`, `kubernetes-integration.md`, `operations.md`,
`resources.md`). Full survey in `w4-2b/docsurvey.txt`.

### CORRECTION 5 — line-number drift, listed so no citation below is off by one

`main.go:69-70` (not `:69-71`) is the `SetIdleTimeout` guard; `agent.go:84` (not
`:85`) is the `Restore` call. `pool.go:130-133` and `config.go:121-131` hold as
cited.

### Confirmed refutation, carried forward

**`docs/field-reference.md:342` is not a contract and is not cited.** Its
Description column is empty, as are all neighbouring PodTemplate rows
(`:339-344`). It is a generated schema listing.

---

## The documented contract — the limbs this scenario is against

Quoted verbatim, each re-read at HEAD.

**L1 — `docs/kubernetes-integration.md:293`**, the strongest:

> With `reuse: true`, the Pod is returned to a pool after the run and reused by the next run.

**L2 — `docs/kubernetes-integration.md:341`**, a *mechanism* claim inside the
numbered "how a run executes" list, arguably the sharpest because it presents
"return to pool" as an unconditional consequence:

> 4. After all steps complete, delete the Pod (or return to pool if `reuse: true`)

**L3 — `docs/jobs.md:1657`**, the field table:

> | `reuse` | bool | Return pod to a pool after run and reuse it for subsequent runs |

**L4 — supporting limbs**, weaker but consistent: `docs/jobs.md:1623`
("`reuse: false` # keep the pod alive after run; reuse for next run"),
`docs/kubernetes-integration.md:323` ("combine with `reuse: true` for persistent
cache"), and the architecture diagram at `:25-27` ("PodPool (when `reuse: true`)
… existing Pods pooled for reuse").

**L5 — `docs/configuration.md:458`**, the Part D limb, quoted with its columns:

> | `poolIdleTimeout` | string | No | `0` (no reuse) | Go duration an idle pooled Pod is kept for reuse before teardown (e.g. `10m`) |

**The gap every limb of L1-L4 shares:** not one qualifies the promise. None says
reuse can fail, none says it depends on the agent's `pods update` permission,
and none says the run still succeeds when it doesn't. **L5 is a different kind of
defect** — not an unqualified promise but an affirmatively wrong one, about the
default.

---

## Method

### Rig

Bring-up exactly as `w4-rig.md` §Bring-up: the plain
`test/ha/docker-compose.ha.yaml` (the `k8senroll` overlay is **not** used —
this scenario needs no 403 control, and the overlay's missing-kubeconfig gotcha
kills all three controllers), then `test/edgecase/tools/w4/w4-up.sh`. Check all
three controllers are `Up`; the W4-0 bootstrap-PAT race is a race, not a
certainty.

Parts B and D start the agent **by hand** against an alternate config rather
than through `w4-up.sh`, which hardcodes `w4-agent-config.yaml`. The interposer
`w4-up.sh` started is reused unchanged, so the enrollment bypass and its
`INTERCEPT` evidence are identical across every arm.

Four configs, all committed, each differing from `w4-agent-config.yaml` in
exactly one field:

| config | the one change | used by |
| --- | --- | --- |
| `k8s/w4-agent-config.yaml` | — (rig default; `poolIdleTimeout: 5m`) | Part A |
| `k8s/w4-2-agent-config-restricted.yaml` | `kubeconfig:` → the restricted file | Part B |
| `k8s/w4-2-agent-config-pooldefault.yaml` | `poolIdleTimeout` **absent** (product default) | Part D |
| `k8s/w4-2-agent-config-poolevict.yaml` | `poolIdleTimeout: 40s` | Part D control |

Fixtures: `w4-reuse.payload.json` (`edge-w4-reuse`, already committed and
already validated through the real `dsl.Parse`, `w4-rig.md` §Step 4) for Parts A
and B; two new fixtures for Part D, `w4-poolkey-b` (image `busybox:1.36`) and
`w4-poolkey-c` (image `alpine:3.20` plus a distinct `env`), whose only purpose is
to produce **three distinct `poolKey` values** — `poolKey` (`pool.go:76-110`)
hashes the whole effective pod shape, so a different image and a different `env`
each yield a different pool bucket. Both are validated through the real
`dsl.Parse` on the `.yaml` **and** on the YAML re-extracted from the
`.payload.json`, per the README rule.

### The instrument problem

Every signal in this scenario is a **log line**; `internal/k8sagent/` emits no
metrics, and the pool exposes no API. Worse, the interesting outcomes are
*absences*: a pool that never fills, an eviction goroutine that never starts.
Three devices handle that.

1. **Part A is a control, and it is not optional.** Without demonstrating that
   reuse actually happens on this rig with an unrestricted credential, "reuse did
   not happen" under the restricted one is indistinguishable from "reuse never
   works here". Part A and Part B run the same fixture, on the same cluster, in
   the same namespace, minutes apart, differing in one config field.
2. **Two independent observation channels per arm**, as `w4-rig.md` §Step 5
   established for this fixture: **pod-name identity** (`W4-REUSE-HOSTNAME`) and
   **marker persistence** (`/workspace/w4-reuse-marker`, `HIT` vs `MISS`). A
   third, **`unified-cd/pool-status` annotation**, is read live with
   `w4-k8s-inject.sh annotations`.
3. **Part D has a positive control of its own** (`poolevict`), for exactly the
   reason Part A is Part B's: "no `pool: evicting idle pod` line ever appeared"
   at the default must be discriminated from "this rig cannot produce that line".

**On the pooled-Pod naming trap** (`FINDINGS.md:2246`): under reuse a Pod's name
and its `unified-cd/runId` label permanently name the **first** run that used
it. No arm below locates a Pod by run id; Pods are located by the `pool-status`
annotation and by `kubectl get pods -n ci` in full.

---

## Part A — the control: reuse actually happens here

**Method.** With the rig default config (privileged kubeconfig,
`poolIdleTimeout: 5m`), on a namespace whose only pod is W4-0's spike pod:

1. Record `kubectl get pods -n ci` before anything.
2. Trigger `edge-w4-reuse`, wait for terminal, capture its logs and the pod list.
3. Read the released Pod's annotations with `w4-k8s-inject.sh annotations`.
4. Trigger `edge-w4-reuse` again, same captures.
5. Enumerate **every** line in the agent log containing `pool` for the window.

**Predicted, from `pool.go:241-270` and `:182-214`.** Run 1 creates a Pod and
`ReleasePod` updates it to `pool-status: idle`, appending it to `p.pods`. Run 2's
`ClaimPod` finds `len(idle) > 0`, updates the annotation to `in-use` with the
run id, and executes in the same Pod. So: **one** Pod name for both runs,
`W4-REUSE-MARKER=MISS` then `HIT`, `pool-status` reading `idle` between the runs
and `in-use` during run 2, and **zero** `pool: failed to mark pod idle` /
`pool: pod claim conflict` lines.

**What this part cannot show, stated up front.** It is a control, not a probe.
A conformant result files nothing.

**Result:** see §Results.

## Part B — deny the verb

**B0 — prove the denial, before running anything through it.** Apply
`k8s/w4-2-reuse-denied-rbac.yaml`, mint the kubeconfig with
`k8s/make-w4-2-restricted-kubeconfig.sh`, and record: `kubectl auth can-i` for
`get list create delete update patch watch` on `pods` as that SA; `kubectl auth
whoami` through the minted file; a successful `get pods` through it; and a
**live refused write** through it. Without B0 the whole part could be measuring
a broken kubeconfig.

**B1 — a cold pool.** Delete every pooled Pod so the restricted agent starts
with `p.pods` empty and `Restore` adopts nothing. Start the agent against
`w4-2-agent-config-restricted.yaml`. Run `edge-w4-reuse` **twice**. Measure:

- Do the two runs execute in **different** Pods (`W4-REUSE-HOSTNAME`), and is
  the marker `MISS` both times?
- Does each run still report **`Succeeded`**?
- What is the **complete** set of agent log lines for the window? The prediction
  is exactly one `WARN "pool: failed to mark pod idle, deleting"` per run,
  carrying the raw 403 in its `error` field, and **no**
  `k8s: failed to release Pod` line — because `DeletePod` succeeds, so
  `ReleasePod` returns nil. **This enumeration is to be verified, not asserted:**
  the capture takes the full agent log for the window, and the runbook reports
  every distinct message in it.
- Is there **any** metric, anywhere? (§B3.)
- Capture the raw 403 text.

**B2 — the warm pool, per CORRECTION 1.** Restart the restricted agent while an
**idle** pooled Pod exists in `ci`, so `Restore` re-adopts it (`pool.go:317-325`,
no `Update` involved) and the next claim pops it into the denied
`UpdatePodAnnotations`. Predicted: one
`WARN "pool: pod claim conflict, creating new pod"` naming a **403**, then a
fresh Pod, then the ordinary `ReleasePod` warning at the end of that same run.
**A 403 reported as a conflict** is the specific thing being measured.

**B3 — the observability enumeration, verified.** `grep -rnE
"prometheus|promauto|metrics\.|Metric" internal/k8sagent/` with the file count
that makes it an enumeration (`find internal/k8sagent -type f | wc -l`), plus a
grep for `\.Patch(` / `\.Watch(` to confirm the two dead grants at HEAD.

**What would be a violation.** Reuse silently not happening while the run
reports `Succeeded`, with no operator-facing surface naming the cause, against
L1/L2/L3 — none of which qualifies the promise.

**Result:** see §Results.

## Part C — the contract

Judge whether silent non-reuse contradicts L1-L4, and file accordingly.

**`FINDINGS.md` is grepped for the FINDING, not merely the doc text**, before
anything is appended — a prior wave re-filed an existing entry because its
already-ruled check covered doc passages only. Terms: `ReleasePod`, `PodPool`,
`StartEviction`, `pool claim conflict`, `failed to mark pod idle`,
`poolIdleTimeout`, `podTemplate.reuse`, `no reuse`.

**Invariant limbs.** No invariant I1-I7 is expected to apply by its own text.
I1 ("every run reaches exactly one terminal state, and that state matches what
actually happened", `docs/superpowers/specs/2026-07-29-edge-case-testing-design.md:45`)
is **not** contradicted: the runs succeed and they did succeed. Filing I1 for a
lost optimisation would be the stretch `FINDINGS.md:1509` forbids. The limbs are
recorded as null rather than stretched.

## Part D — `poolIdleTimeout: 0` — the default disables **eviction**, not **reuse**

**The code trace, all at HEAD.**

1. `PoolIdleTimeoutDuration()` (`config.go:121-131`) returns `0` when the field
   is unset — the default.
2. `main.go:69-70` is `if d := cfg.PoolIdleTimeoutDuration(); d > 0 {
   pool.SetIdleTimeout(d) }`, so at the default `SetIdleTimeout` is **never
   called** and `p.idleTimeout` keeps its zero value.
3. `StartEviction` (`pool.go:130-133`) returns immediately at `<= 0`. **No
   eviction goroutine ever starts.**
4. `idleTimeout` is read **nowhere else**: `grep -rn "idleTimeout"
   internal/k8sagent/` returns 7 lines, all in `pool.go`, all inside
   `SetIdleTimeout`, `StartEviction`, `evictExpired` or the struct comment.
   `ClaimPod` and `ReleasePod` never consult it.

**So `0` disables eviction and leaves reuse fully on — and
`docs/configuration.md:458` says the opposite of the truth, about the default.**

**The consequence, which is why this is not merely a doc gloss.** With
`idleTimeout` 0, nothing removes an idle pooled Pod:

- the **pod GC declines** — `podGCDecision` (`podgc.go:19-24`) returns `false`
  unconditionally for pool-managed Pods, keyed on the `pool-status` annotation
  (`podgc.go:137`), deliberately deferring to *"the pool's own idle-timeout/Restore
  logic"* (`podgc.go:99-100`), which at 0 does nothing. **Measured already**, on
  this rig: `w4-1-podgc-skip-not-delete.md` §Part C counted **8** GC evaluations
  of one pooled Pod by two agents, with zero deletions and zero log lines;
- the **pool re-adopts across restarts** — `Restore`'s idle branch
  (`pool.go:317-325`) appends the Pod back into `p.pods` rather than deleting it,
  so a rollout does not clear the leak either;
- `docs/high-availability.md:431` states the GC half as a *feature* — "Pods still
  owned by the Pod-reuse pool are never touched" — without noting that at the
  default nothing else touches them.

**Idle pooled Pods therefore accumulate without bound, and survive agent
restarts, at the default configuration.** The bound is one Pod per distinct
`poolKey` — and `poolKey` (`pool.go:76-110`) hashes the *entire effective pod
shape*, so the number of buckets is the number of distinct pod shapes any job in
the fleet has ever asked for, which nothing caps.

**This is measured live, not left as a code read.** A code-read leak and a
measured leak are different findings.

**D1 — accumulation.** Start the agent against
`w4-2-agent-config-pooldefault.yaml`. Run `edge-w4-reuse`, `edge-w4-poolkey-b`
and `edge-w4-poolkey-c` once each — three distinct pod shapes, hence three
distinct pool keys. Predicted: **three** Pods left `Running` in `ci`, all with
`pool-status: idle`, all with **distinct** `pool-key` annotations, and all still
present after a hold long enough for any eviction to have fired.

**D2 — the leak survives a restart.** SIGTERM the agent, restart it against the
same config, and confirm three `pool: restored idle pod` lines (`pool.go:324`)
and three Pods still `Running`.

**D3 — the positive control.** Restart against
`w4-2-agent-config-poolevict.yaml` (`poolIdleTimeout: 40s`), same rig, same
Pods. Predicted: `pool: evicting idle pod (timeout)` (`pool.go:169`) for each,
and the Pods gone. `StartEviction` floors the check interval at 30 s
(`pool.go:134-137`), so the first eviction lands at roughly 60 s. **Without D3,
D1's "nothing evicted them" is indistinguishable from "this rig cannot evict".**

**D4 — reuse still works at the default.** Run `edge-w4-reuse` a second time
under `pooldefault` and confirm the same Pod and `MARKER=HIT`. This is the direct
refutation of "(no reuse)": the arm the doc says produces no reuse is the arm in
which reuse demonstrably happens.

**Severity is judged on the evidence gathered, and is not taken from the
briefing.** The judgement, and the reasoning behind the band chosen, is recorded
in §Results.

**Weaker secondary hit, filed subordinate if at all.**
`docs/configuration.md:395-396`'s "Unset = no reuse window" is ambiguous rather
than false — "reuse window" can be read as "the window before teardown", which
is genuinely absent — and is recorded as a note on the primary entry.

**Relationship to what is already on file, checked before filing.**
`FINDINGS.md:2282-2295` (W4-3) is about the **same table row** `:458` but a
**different column and a different claim**: that its Description column omits
the boot rejection of an unparseable value. This entry is about its **Default**
column asserting something false about what the default *does*. The two do not
overlap and neither subsumes the other; the relationship is stated explicitly in
the filed entry so a reader of either finds the other.

---

## Recording rules this scenario is bound by

- A **violation** contradicts an invariant (I1-I7) **or a statement in
  `docs/*.md`**. An inline comment inside a function body is **not** a contract —
  which matters acutely here, because `manifests/base/k8s-agent/rbac.yaml:9-12`
  and `pool.go:52`'s `// 0 = no eviction` both state the truth plainly. **The
  filings rest on `docs/kubernetes-integration.md`, `docs/jobs.md` and
  `docs/configuration.md`, not on those comments.** (`pool.go:52`'s comment is
  quoted only to show the code's own author knew what 0 means.)
- An **observation** says "observation" in its **title** and repeats it in its
  **Severity** line.
- **An invariant must be contradicted by its own text, not its spirit**
  (`FINDINGS.md:1509`).
- **Do not inflate, and do not deflate.** Three of W2's nine scenarios produced
  only observations and that was right each time.
- **Never `head` a docs survey; always report the hit count.**
- **When a class is claimed fully enumerated, verify the enumeration.**
- Every number traces to a capture whose window covers it; derived, inferred and
  code-read values are labelled.
- **"Never" is not written for a window this scenario ended itself.** Part D's
  natural phrasing — "the pooled Pods are never evicted" — is the wrong one; what
  is measured is that they survived a specific, bounded hold, and that is what
  is written.
- Every W4 scenario discloses the enrollment bypass, referencing
  `scenarios/w4-rig.md`. Done above.
- **All committed text is English. No product-code changes.** `manifests/`,
  `test/ha/` and `test/edgecase/workloads/podcap-job.payload.json` are not
  modified.
- **Scrub credentials.** The restricted kubeconfig is gitignored; the committed
  artifact is its generator.
- Kill every background process started and capture its final output; tear the
  rig down; leave the tree clean; remove the scenario's own cluster objects.

---

## Results

*(Filled in after execution.)*
