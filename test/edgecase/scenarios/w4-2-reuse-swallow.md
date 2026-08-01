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

Executed 2026-08-01 against the rig described above. Raw captures are in the
session scratchpad under `w4-2b/`; every number below traces to one of them.

> **Two clocks, as `w4-rig.md` warns.** The agent and the interposer log local
> time (`+09:00`, e.g. `17:26:32`); the controller, the database and every
> capture script log UTC (`08:26:32Z`). **Local = UTC + 9 h.** Every figure in
> this section is stated in **UTC** unless it is quoted verbatim from an agent
> log line.

**Rig bring-up, and one deviation from `w4-rig.md`'s record.** Plain
`test/ha/docker-compose.ha.yaml`, cold, `up -d --build`. **The W4-0
bootstrap-PAT race FIRED this time**: `controller3` exited(1) at `08:20:54.5598Z`
with `create bootstrap pat: ERROR: duplicate key value violates unique
constraint "pats_token_hash_key" (SQLSTATE 23505)`, while controllers 1 and 2
came up. It was restarted with `up -d controller3` and joined cleanly
(`stack-up.txt`). That makes the running tally **3/4 cold bring-ups survived** —
the first observed failure since `FINDINGS.md:2270` recorded 3/3, and it
confirms as a measurement what that entry could only assert: it is a **race**.
Recorded as a frequency datapoint on the existing entry, not re-filed.

Then `test/edgecase/tools/w4/w4-up.sh`; agent `k8s-agent-w4` registered at
`08:22:25.7780Z` (`rig-up.txt`). **The interposer answered the enrollment, so
the bypass was in effect and the disclosure above applies** — the `INTERCEPT`
line for this session is in `.w4run/enrollproxy.log`, and `w4-down.sh`'s count
is reported in §Teardown with the semantics `w4-rig.md` §Teardown defines.

**One rig side effect, disclosed.** Namespace `ci` was cleared with
`kubectl delete pod -n ci --all` before Part A so the pool would start empty.
That also removed **W4-0's `w4-spike-agent-pod`**, which W4-0 and W4-1 had both
left in place. It is recreated from its committed manifest
(`test/edgecase/k8s/w4-spike-identity.yaml`) at teardown; see §Teardown.

**Fixtures.** `edge-w4-reuse` plus the two new pool-key fixtures created
`200/200/200`, and all three were validated through the real `dsl.Parse` on the
`.yaml` **and** on the YAML re-extracted from the `.payload.json`, per the README
rule (`fixtures.txt`).

### Verdict summary

| Part | Result |
| --- | --- |
| A. Control — does reuse happen here? | **YES, unambiguously.** Three runs, one Pod, marker `MISS`→`HIT`→`HIT`, `pool-status` caught mid-flight going `idle`→`in-use`→`idle`, and **zero** `pool` log lines |
| B0. Is the denial real? | **YES** — `update` and `patch` both `no`; a live `PUT` and a live `PATCH` both refused; `delete` granted |
| B1. Cold pool, restricted credential | **Reuse totally defeated, both runs `Succeeded`.** Two distinct Pods, `MARKER=MISS` twice. **Complete** log enumeration for the window: 6 lines, 3 distinct messages, of which the only abnormal one is `WARN "pool: failed to mark pod idle, deleting"` ×2. No `k8s: failed to release Pod`. **Zero** controller-side signal of any kind |
| B2. Warm pool (CORRECTION 1) | **Confirmed live** — `Restore` re-adopts the idle Pod with no `Update`, and the next `ClaimPod` reports the **403 as a "claim conflict"**. The brief's "this branch is never reached" was wrong |
| B3. Any metric anywhere? | **No.** 53 files, 0 matches — enumeration verified, not repeated. `.Patch(` and `.Watch(` are 0: two of the seven granted verbs are dead |
| C. The contract | **1 violation, and it is bigger than the one predicted.** `docs/kubernetes-integration.md:491-521` publishes, as "Minimum permissions required for k8s-agent to operate", a Role **byte-identical** to the one Part B just proved defeats reuse — on the same page that promises reuse at `:293` and `:341` |
| D. `poolIdleTimeout: 0` | see §Part D |

### Part A — the control: reuse happens on this rig

Capture `partA.txt`, `partA3-transition.txt`. Config: the committed
`w4-agent-config.yaml`, unmodified.

Three runs of `edge-w4-reuse`, all `Succeeded`, **all in one Pod**:

```
08:23:58.7  A1  38dc9437-…  W4-REUSE-HOSTNAME=ucd-run-38dc9437-37b8-4d  MARKER=MISS  planted 08:24:03Z
08:24:05.9  A2  fa01696d-…  W4-REUSE-HOSTNAME=ucd-run-38dc9437-37b8-4d  MARKER=HIT   planted=08:24:03Z
08:24:26.6  A3  0533d690-…  W4-REUSE-HOSTNAME=ucd-run-38dc9437-37b8-4d  MARKER=HIT   planted=08:24:07Z
```

Both observation channels agree: **pod-name identity** and **marker
persistence**. The `unified-cd/pool-key` annotation read
`3c1d2612f14e2dd35233f878a28578b5` throughout — the same value `w4-rig.md`
§Step 5 recorded for this fixture on a different session, so the pod-shape hash
is stable across processes, which is what `Restore` depends on.

**The annotation transition was caught in flight, not inferred.** A3 was sampled
at 0.7 s intervals (`partA3-transition.txt`):

```
08:24:26.706  ucd-run-38dc9437-37b8-4d status=idle    runid=-
08:24:27.647  ucd-run-38dc9437-37b8-4d status=in-use  runid=0533d690
08:24:28.579  ucd-run-38dc9437-37b8-4d status=idle    runid=-
```

`ClaimPod`'s `Update` to `in-use` (`pool.go:194-197`) and `ReleasePod`'s back to
`idle` (`pool.go:253-255`) are both visible, ~0.9 s apart.

**And the log said nothing, which is the point.** Every line the agent emitted
across A1 and A2 containing the string `pool`: **none**. The complete
`(level,msg)` enumeration for that window is `INFO k8s: executing Run` ×2 and
`INFO running` ×2. **A working reuse feature and a totally defeated one are
therefore distinguished by exactly one `WARN` line — everything else in the log
is identical.** That is Part B's finding stated from the control side.

### Part B0 — the denial is real, proven three ways

Capture `partB0.txt`.

```
-- kubectl auth can-i pods -n ci, as system:serviceaccount:ci:w4-2-reuse-denied --
   get yes | list yes | create yes | delete yes | watch yes | update NO | patch NO

-- auth whoami THROUGH the minted kubeconfig --
   Username  system:serviceaccount:ci:w4-2-reuse-denied
   Groups    [system:serviceaccounts system:serviceaccounts:ci system:authenticated]

-- reads work through it --
   NAME                       READY   STATUS    RESTARTS   AGE
   ucd-run-38dc9437-37b8-4d   2/2     Running   0          86s

-- a live PUT (kubectl replace) = the 'update' verb ReleasePod uses --
   Error from server (Forbidden): pods "ucd-run-38dc9437-37b8-4d" is forbidden:
   User "system:serviceaccount:ci:w4-2-reuse-denied" cannot update resource "pods"
   in API group "" in the namespace "ci"

-- a live PATCH (kubectl annotate), for contrast --
   ... cannot patch resource "pods" ...

-- delete is GRANTED, so ReleasePod's fallback delete will succeed --
   yes
```

The route the brief flagged as its one known risk did **not** bite. The brief
predicted `https://desktop-control-plane:6443` would fail to resolve from a host
process and budgeted an hour for it. The generator reads the server URL out of
the developer's own current kubeconfig instead — Docker Desktop publishes the
apiserver on a **dynamic** host port (`https://127.0.0.1:65224` this session),
and the node certificate's SANs include `IP Address:127.0.0.1`, so no
`insecure-skip-tls-verify` was needed and no hour was spent. **That is the
committed generator's default, not a one-off**, precisely so the next task does
not hit the hardcoded-port version of the same problem.

### Part B1 — a cold pool under the restricted credential

Capture `partB1.txt`, `partB-controller.txt`, `agent-logs/k8s-agent-partB1-restricted.log`.

Namespace cleared, agent restarted against
`w4-2-agent-config-restricted.yaml`; `Restore` adopted nothing (**zero** pool
lines at startup, verified). Two runs of `edge-w4-reuse`:

```
08:26:28.1  B1-1  c7031bdb-…  Succeeded  HOSTNAME=ucd-run-c7031bdb-9ec9-43  MARKER=MISS
08:26:32.9  B1-2  4fc8b36a-…  Succeeded  HOSTNAME=ucd-run-4fc8b36a-d518-4f  MARKER=MISS
```

**Two different Pods. `MISS` twice. Both runs `Succeeded`.** The `/workspace`
build cache that `docs/kubernetes-integration.md:294` says "can accumulate" did
not survive a single run.

**The complete log for the window — this is an enumeration, and it was
verified rather than asserted.** Six lines, three distinct messages:

```
INFO   2  k8s: executing Run
INFO   2  running
WARN   2  pool: failed to mark pod idle, deleting
```

The two `WARN`s, verbatim (local `+09:00`):

```
{"time":"2026-08-01T17:26:32.4317826+09:00","level":"WARN","msg":"pool: failed to mark pod idle, deleting",
 "pod":"ucd-run-c7031bdb-9ec9-43",
 "error":"pods \"ucd-run-c7031bdb-9ec9-43\" is forbidden: User \"system:serviceaccount:ci:w4-2-reuse-denied\" cannot update resource \"pods\" in API group \"\" in the namespace \"ci\""}
{"time":"2026-08-01T17:26:37.4320373+09:00","level":"WARN","msg":"pool: failed to mark pod idle, deleting",
 "pod":"ucd-run-4fc8b36a-d518-4f",
 "error":"pods \"ucd-run-4fc8b36a-d518-4f\" is forbidden: … cannot update resource \"pods\" …"}
```

**Every prediction the brief made about this window holds, and the enumeration
adds one thing it did not.**

- The raw `apierrors` 403 does survive into the `error` field, unwrapped
  (`podmanager.go:91-92` returns it bare). An operator who *reads this line* can
  diagnose it in one step. That is the one genuinely good property of this
  failure.
- **`k8s: failed to release Pod` (`agent.go:317`) did not fire**, exactly as
  predicted: `DeletePod` succeeds, so `ReleasePod` returns `nil`.
- **What the enumeration adds:** the *only* difference between this log and Part
  A's is two `WARN` lines. There is no summary, no count, no "reuse disabled"
  line, nothing at agent start, and nothing that says the condition is
  **permanent**. The word "deleting" describes the action; nothing describes the
  consequence.

**And the message's wording is the finding.** "failed to mark pod idle,
deleting" reads as a transient hiccup with a sensible fallback. It is neither:
the credential does not change, so this fires on **every** run for the life of
the process, and the fallback is not a recovery — it is the permanent
substitution of pod-per-run for pod reuse.

**Nothing outside the agent process saw anything.** Measured, not assumed
(`partB-controller.txt`):

- `runs`: all six runs of the session `Succeeded`, `claimed_by k8s-agent-w4`.
- `step_reports`: 12 rows, every one `Succeeded` with `exit_code 0`.
- Controller replicas: a case-insensitive grep for `forbidden|403|pool|reuse`
  over all three returns 14 lines, and **every one is incidental** — `postgres
  pool configuration` at boot, and `http request` lines whose *path* contains the
  substring `403` because run id `4fc8b36a-d518-4f7a-bd0d-403760f192b2` does.
  **Zero controller lines about the denial, the pool, or reuse.**
- `GET /api/v1/runs/{id}` for a restricted run returns
  `{"status":"Succeeded", … ,"claimedBy":"k8s-agent-w4"}` and nothing else.

So the total operator-facing evidence that a feature is completely dead is **one
`WARN` per run in one process's stdout**, on a Kubernetes agent whose stdout is
behind `kubectl logs` and a Pod that restarts on crash.

### Part B2 — the warm pool: a 403 reported as a "claim conflict"

Capture `partB2.txt`, `agent-logs/k8s-agent-partB2-warm.log`. **This arm exists
because reconnaissance corrected the brief, and the correction was right.**

Part A left one idle pooled Pod in `ci`. The agent was stopped (SIGTERM,
graceful) and restarted against the restricted config. `Restore`'s idle branch
(`pool.go:317-325`) re-adopted the Pod — **with no `Update` call, so the
restricted credential is not exercised at all here**:

```
{"time":"2026-08-01T17:25:55.5055937+09:00","level":"INFO","msg":"pool: restored idle pod",
 "pod":"ucd-run-38dc9437-37b8-4d","poolKey":"3c1d2612f14e2dd35233f878a28578b5","template":""}
```

The pool was therefore **non-empty on the first claim**, `pool.go:188-189`'s
`if len(idle) > 0` was true, and 1.02 s later:

```
{"time":"2026-08-01T17:25:56.5304005+09:00","level":"WARN","msg":"pool: pod claim conflict, creating new pod",
 "pod":"ucd-run-38dc9437-37b8-4d",
 "error":"pods \"ucd-run-38dc9437-37b8-4d\" is forbidden: User \"system:serviceaccount:ci:w4-2-reuse-denied\" cannot update resource \"pods\" in API group \"\" in the namespace \"ci\""}
```

**A permission error announced as an optimistic-concurrency conflict.** The two
conditions could not be less alike: a claim conflict means another actor touched
the Pod and retrying is correct; a 403 means retrying is futile forever. The
same run then produced the ordinary `ReleasePod` warning 3.51 s later, so **the
first run after such a restart emits both messages and the ordering is fixed**.

**The brief's reconnaissance said this branch is "never reached". It is reached
on any agent restart that inherits an idle pooled Pod** — which, per Part D, is
the state the *default* configuration leaves the namespace in permanently. The
brief was right about a cold process and wrong in general, and the difference is
one restart.

**A detail neither the brief nor the runbook predicted, read off the live
annotations:** the replacement Pod carried `pool-status: in-use` right up to its
deletion and **never** transitioned to `idle` — because the transition *is* the
denied write. So `kubectl` shows a namespace of Pods permanently annotated
`in-use` with an empty `pool-run-id`, which is also what an in-flight run looks
like. The annotation is not a reliable readout of pool state under this failure.

### Part B3 — the observability enumeration, verified

Capture `partB3-docsurvey.txt`.

```
internal/k8sagent/  : 53 files (ls -1 = 53, find -type f = 53), find -type d = 1 (flat, no subdirs)
grep -rlE 'prometheus|promauto|metrics\.|Metric'  -> 0 files, 0 matches
grep -rn '\.Patch('                               -> 0
grep -rn '\.Watch('                               -> 0
the three Update writers: podmanager.go:91, pool.go:255, pool.go:336
```

**The 53 confirms `w4-1-podgc-skip-not-delete.md`'s CORRECTION 2 and refutes the
brief's "60" a second time.** And two of the seven verbs the shipped Role grants
— `patch` and `watch` — are **exercised by nothing in the package at HEAD**.
That is not a defect (a Role may grant headroom), but it is worth stating next to
a finding about a missing verb: the shipped Role over-grants two verbs while the
documented Role under-grants the one that matters.

### Part C — the contract, and the finding the scenario did not go looking for

Captures `partC-rbac-docs.txt`, `partC-blame.txt`, `partB3-docsurvey.txt`,
`findings-grep.txt`.

**Docs surveys, untruncated, with hit counts.** `reuse` (case-insensitive)
across `docs/`: **176 hits in 65 files**; excluding `docs/superpowers/`,
**25 hits in 8 files** (`agents.md`, `configuration.md`, `field-reference.md`,
`high-availability.md`, `jobs.md`, `kubernetes-integration.md`, `operations.md`,
`resources.md`). Both numbers reproduce the brief exactly.

**The predicted finding holds.** Silent non-reuse contradicts L1
(`kubernetes-integration.md:293`), L2 (`:341`) and L3 (`jobs.md:1657`): all
three state the pooling as an unconditional consequence of `reuse: true`, and
under Part B's credential the Pod is *deleted* every run while the run reports
`Succeeded`. L2 is the sharpest — its numbered step 4 says "delete the Pod (or
return to pool if `reuse: true`)", and `pool.go:255-259` turns the parenthesis
straight back into "delete the Pod" on any `Update` error.

**But the survey turned up something far more consequential, and it inverts the
re-charter's own premise.**

`docs/kubernetes-integration.md:491-521` is a section headed **"RBAC example"**,
introduced by the sentence *"Minimum permissions required for k8s-agent to
operate:"*, publishing a `Role` whose rules are:

```yaml
rules:
  - apiGroups: [""]
    resources: ["pods", "pods/exec", "pods/log"]
    verbs: ["create", "get", "list", "delete", "watch"]
  - apiGroups: [""]
    resources: ["persistentvolumeclaims"]
    verbs: ["create", "delete"]
```

**That is byte-identical to the Role this scenario built to defeat reuse.** The
diff is empty — verified mechanically, not by eye (`partC-rbac-docs.txt`, the
`diff -u` of the two blocks prints nothing). `test/edgecase/k8s/w4-2-reuse-denied-rbac.yaml`
was written as "the shipped manifest minus `update` and `patch`"; it landed on
the documented example by construction, because the documented example **is** the
pre-fix verb set.

**Provenance, from `git`** (`partC-blame.txt`):

| line | last touched by | when |
| --- | --- | --- |
| `manifests/base/k8s-agent/rbac.yaml:13` | `6b0bf8f` — *fix(k8s): make podTemplate.reuse actually reuse pods (#50)* | 2026-07-15 |
| `docs/kubernetes-integration.md:504` | `51b148d` — *implement unified-cd* (the initial commit) | 2026-06-28 |

And `git show --stat 6b0bf8f` lists **ten** files: six under `internal/k8sagent/`
and four under `manifests/`. **No documentation file at all.** The fix corrected
every copy of the Role it shipped and left the copy it *published* untouched, on
the very page that promises the feature.

**So the re-charter's premise is exactly half right, and this is the correction
that matters.** "The missing-verbs mechanism is dead" is true of `manifests/` and
**false of `docs/`**. An operator who does what the Kubernetes integration guide
tells them — which is what an operator not using the bundled manifests will do,
and the section exists for precisely that reader — builds a Role under which
`podTemplate.reuse` cannot work, and gets no error, no failed run, and one
`WARN` line per run whose wording implies a transient problem. **Part B is
therefore not a synthetic experiment. It is a live execution of the documented
configuration**, and its result is that the documented configuration silently
breaks a documented feature.

**Was this already on file?** `FINDINGS.md` was grepped for the **findings**,
not the doc text (`findings-grep.txt`): `ReleasePod` **0**, `ClaimPod` **0**,
`PodPool` **0**, `StartEviction` **0**, `evictExpired` **0**, `poolKey` **0**,
`pool: failed to mark pod idle` **0**, `pool claim conflict` **0**, `no reuse`
**0**, `(no reuse)` **0**, `kubernetes-integration.md:293` **0**, `:341` **0**,
`jobs.md:1657` **0**, `configuration.md:458` **0**, `silently never happens`
**0**. The one `update/patch` hit is `FINDINGS.md:2260` — the W4-rig
**retraction** of the false claim about `manifests/`, which is the opposite
claim about a different file. Nothing here is a re-filing.

**Invariant limbs, adjudicated.** **I1 is a null limb and is recorded as one.**
Its text is "every run reaches exactly one terminal state, and that state matches
what actually happened". Every run in Parts A, B and D reached exactly one
terminal state and it was `Succeeded`, which is what happened: the *jobs* did
succeed. What is lost is an optimisation and a build cache, not correctness of
the run record. Filing I1 here would be the stretch `FINDINGS.md:1509` forbids,
of the kind W2-5 and W2-7 were corrected for. **No other invariant applies by its
own text and none is stretched to fit.** This scenario's filings rest entirely on
`docs/*.md`.

### Part D — `poolIdleTimeout: 0`: measured, not code-read

Captures `partD1.txt`, `partD2-hold.txt`, `partD3.txt`,
`agent-logs/k8s-agent-partD-*.log`.

**D4 first, because it is the direct refutation.** Under
`w4-2-agent-config-pooldefault.yaml` — the `poolIdleTimeout` line **absent**,
i.e. the product default `docs/configuration.md:458` calls **"(no reuse)"** —
two runs of `edge-w4-reuse`:

```
08:28:29.5  f0b89939-…  Succeeded  HOSTNAME=ucd-run-f0b89939-bfd5-4f  MARKER=MISS   planted 08:28:33Z
08:28:34.6  62f508da-…  Succeeded  HOSTNAME=ucd-run-f0b89939-bfd5-4f  MARKER=HIT    planted=08:28:33Z
```

**Same Pod. `MARKER=HIT`. Reuse is fully on at the default the documentation says
produces none.** The doc's Default column is not vague, not ambiguous and not a
simplification — on the only operator-facing page that mentions the field, it
says the opposite of what happens.

**D1 — accumulation, three distinct pool keys.** Three fixtures whose only
difference is pod shape (`edge-w4-reuse` = `alpine:3.20`; `edge-w4-poolkey-b` =
`busybox:1.36`; `edge-w4-poolkey-c` = `alpine:3.20` + one `env` entry) produced
three separately-pooled Pods with three distinct `poolKey` hashes, confirming
that `poolKey` (`pool.go:76-110`) buckets by the whole effective shape:

```
ucd-run-f0b89939-bfd5-4f   pool-status=idle   pool-key=3c1d2612f14e2dd35233f878a28578b5
ucd-run-4685a87c-afb9-40   pool-status=idle   pool-key=db63f75ad772fff085fd2b00d7836b73
ucd-run-4f69515d-d2e1-46   pool-status=idle   pool-key=5a495bb77d29c505ff339c7ad7f8c09f
```

**The hold: 7 min 44 s, sampled every 60 s, and its bound is stated.** All three
Pods stayed `2/2 Running` and `pool-status: idle` at every one of the 8 samples,
`08:29:09.9Z` → `08:36:13.3Z`, and the agent emitted **zero** lines matching
`evict`. The window **exceeds** the rig's own `poolIdleTimeout` (5 m) and does
**not** exceed the documented example (10 m). **This is not written as "the Pods
are never evicted"** — it is written as: they survived a 7 m 44 s window that
this scenario ended itself, during which the only component that could have
evicted them was not running, because `StartEviction` returned at `<= 0` before
launching its goroutine.

**And the pod GC saw them and declined, ten times each — corroborating W4-1 on
different Pods.** The controller access log carries exactly **10**
`GET /api/v1/runs/{id}` for each of the three pooled run ids across the hold,
one per sweep per Pod, with no deletion and no GC log line
(`partD2-hold.txt`). That is `podgc.go:99-100`'s deferral to "the pool's own
idle-timeout/Restore logic" measured against a pool whose idle-timeout logic
does not exist.

**D2 — the leak survives a restart.** SIGTERM, then restart against the same
config:

```
{"…17:36:28.6008051+09:00","level":"INFO","msg":"pool: restored idle pod","pod":"ucd-run-4685a87c-afb9-40","poolKey":"db63f75a…"}
{"…17:36:28.6008051+09:00","level":"INFO","msg":"pool: restored idle pod","pod":"ucd-run-4f69515d-d2e1-46","poolKey":"5a495bb7…"}
{"…17:36:28.6008051+09:00","level":"INFO","msg":"pool: restored idle pod","pod":"ucd-run-f0b89939-bfd5-4f","poolKey":"3c1d2612…"}
```

Three Pods before, three re-adopted, three still `Running` after. **A rollout
does not clear the pool; it inherits it.**

**D3 — the positive control, and without it D1 proves nothing.** Same rig, same
three Pods, same fixtures, agent restarted against
`w4-2-agent-config-poolevict.yaml` whose **only** difference is
`poolIdleTimeout: 40s`:

```
08:36:31.2  t=+0s    pods=3  'evicting idle pod' lines=0
08:37:01.4  t=+30s   pods=3  'evicting idle pod' lines=0
08:37:31.7  t=+60s   pods=0  'evicting idle pod' lines=3
```

```
{"…17:37:30.3134338+09:00","level":"INFO","msg":"pool: evicting idle pod (timeout)","pod":"ucd-run-4685a87c-afb9-40","idleSince":"2026-08-01T17:36:30.3255849+09:00"}
… ×3, 6.3 ms apart …
```

Evicted on the second 30 s check, exactly as `pool.go:134-137`'s 30 s floor
predicts. **So the rig can evict. D1's Pods were not evicted because at the
default nothing evicts, not because this rig cannot.**

**One thing D3 exposed that no part of this scenario set out to look for, and it
sharpens the finding.** The `idleSince` in all three eviction lines is
`17:36:30.3255849+09:00` — the **restart instant**, identical to the
`k8s agent registered` timestamp, not the `17:28:33`-ish moment each Pod was
actually released. `Restore` (`pool.go:317-323`) rebuilds each `PooledPod` with
`IdleSince: time.Now()`. **The idle clock therefore resets on every agent
restart**, so an agent that restarts more often than `poolIdleTimeout` never
evicts anything even when the field *is* set. Measured here as a side effect;
the mechanism is code-read at `pool.go:322`.

**Severity, judged on this evidence and argued rather than taken from the
briefing.** Filed **major**, and the reasoning is recorded so a reviewer can
disagree with the band without disagreeing with the facts:

- The false sentence *alone* would be **minor** — `docs/*.md` drift, the band
  `FINDINGS.md:8` names, and the band W4-3's `podStartTimeout` entry
  (`FINDINGS.md:2282`) correctly took for a comparable inversion.
- What lifts it is **what the false sentence conceals plus the absence of any
  second source.** The concealed behaviour is a resource that **no component in
  the product ever reclaims**: the pod GC declines by design
  (`podgc.go:19-24`, measured, 10 sweeps × 3 Pods), the eviction goroutine never
  starts (`pool.go:130-133`), and `Restore` re-adopts across restarts
  (`pool.go:317-325`, measured). That is the "unbounded recovery" half of
  `FINDINGS.md:7` — not slow recovery, no recovery path at all short of manual
  `kubectl delete` or setting a field the operator has been told means the
  opposite. And there is nowhere else to learn the truth:
  `docs/configuration.md` is the **only** operator-facing page that mentions
  `poolIdleTimeout` at all (2 hits, 1 file), and
  `docs/kubernetes-integration.md` — the page that documents pod reuse — never
  mentions it. `docs/high-availability.md:431` states the GC half as a *feature*
  without noting that at the default nothing else touches these Pods.
- **Held below critical**: nothing is lost, corrupted or exposed; no run fails;
  the retained Pods are healthy and reusable, which is the *point* of a pool.
- **The bound, stated so the finding is not inflated.** The leak is **one Pod per
  distinct pod shape ever used with `reuse: true`**, not one per run — three
  fixtures produced three Pods, and running `edge-w4-reuse` twice more produced
  none. It is unbounded in shape diversity and in time, not in run count. It
  also only reaches deployments where some job sets `podTemplate.reuse: true`
  while the agent config never sets `poolIdleTimeout` — which is the default
  configuration, and the two live in different files owned by different people.

**The secondary hit is recorded, not filed.** `docs/configuration.md:395-396`
("Unset = no reuse window") is ambiguous rather than false — "reuse window" can
be read as "the window before teardown", which genuinely is absent at 0 — and it
appears as a note on the primary entry rather than as its own.

**Relationship to `FINDINGS.md:2282-2295` (W4-3), stated in the filed entry.**
That entry is about the **same table row**, `docs/configuration.md:458`, and a
**different column and different claim**: that its Description column omits the
boot rejection of an unparseable value. This one is about its **Default** column
asserting something false about what the default does. Neither subsumes the
other. Worth noting for the campaign's own process: W4-3 read that exact row
closely enough to compare it against its neighbours and did not notice the
Default column was wrong, because it was looking for a different defect.

### What the whole scenario says in one paragraph

`podTemplate.reuse` works exactly as documented when the agent can write Pod
annotations, and is completely and silently defeated when it cannot — the
difference between the two, in the agent's entire log, is one `WARN` line per run
whose wording ("failed to mark pod idle, deleting") describes a transient hiccup
rather than a permanently dead feature, with nothing on any controller-side
surface at all and no metric anywhere in the package. That would be a
diagnosability observation if the restricted credential were exotic. It is not:
`docs/kubernetes-integration.md:491-521` publishes it as the **minimum
permissions required**, byte-identical, on the same page that promises reuse
twice, because PR #50 fixed every Role it *shipped* and none of the one it
*publishes*. And the field an operator would reach for on discovering pooled Pods
piling up is documented, on the only page that mentions it, as meaning the exact
opposite of what it does.

## Findings filed

**2 violations + 1 observation.** Parts A, B0 and D3 are controls and are filed
as nothing; B3's enumeration is evidence, not a finding.

| # | Kind | Title (see `FINDINGS.md`) |
| --- | --- | --- |
| 1 | **violation** (contract limb, major) | the docs' own "Minimum permissions required" RBAC example omits `pods update`, so `podTemplate.reuse` — promised twice on the same page — is silently and permanently defeated, with one `WARN`/run as the entire signal |
| 2 | **violation** (contract limb, major) | `docs/configuration.md:458` says the `poolIdleTimeout` default `0` means "(no reuse)"; reuse is fully on at the default and what `0` disables is *eviction*, leaving idle pooled Pods that nothing in the product ever reclaims and that survive restarts |
| 3 | observation (minor) | `ClaimPod` reports a permanent 403 as `"pool: pod claim conflict"`, reachable on any restart that inherits an idle pooled Pod; plus a third, quieter swallow at `pool.go:336` that discards the error entirely |

**Deliberately corroborated without re-filing:** `FINDINGS.md:2270` (the
bootstrap-PAT cold-start race — this session is the first observed *failure*,
3/4) and `FINDINGS.md:2246` (the pooled-Pod naming trap, seen again in every
capture here).

## Teardown

The agent was stopped with SIGTERM first (so the graceful-drain path runs),
escalating to SIGKILL, at **every** config swap — five swaps, five archived logs
in `w4-2b/agent-logs/`, each with its final lines printed into the capture that
performed it. The interposer was stopped through `w4-down.sh`
(`w4-2b/teardown.txt`, `final-logs/`), which reported **10** intercepted
enrollment exchanges across the rotated proxy logs (6 + 2 + 1 + 1) — so the
bypass was in effect for every arm and the disclosure at the top applies to all
of them. `ps` confirms **no** `k8s-agent` and **no** `enrollproxy` process
survived.

**A rig datapoint worth carrying forward.** `INTERCEPT #2` through `#6` are the
agent's four config swaps plus its restart: the k8s agent re-enrolls on **every
process start**, so a scenario that restarts the agent N times costs N+1
interceptions rather than one per ~40 min. That is consistent with
`w4-rig.md` §Step 3's correction — the refresh interval governs a *long-lived*
agent, not a restarted one — and it is why this session's count is 10 rather
than the 1 a naive reading would predict.

Cluster objects this scenario created were removed: the `w4-2-reuse-denied`
ServiceAccount, Role and RoleBinding in `ci`, and every pooled `ucd-run-*` Pod.
**W4-0's `w4-spike-agent-pod`, which this scenario's namespace clear removed, was
recreated from its committed manifest** (`test/edgecase/k8s/w4-spike-identity.yaml`)
so the next task finds the namespace as W4-0 left it. The restricted kubeconfig
is gitignored and was deleted; the committed artifact is its generator.

The compose stack was taken down with `down -v` (mandatory between scenarios,
per the Garage volume rule); both Garage and agent-credentials volumes were
removed.

**Credential sweep, run over every capture in `w4-2b/`.** `uc[aer]_` appears in
exactly four distinct forms — `uca_`, `ucr_`, `uca_8353b48a` and
`uca_d4af207c` — i.e. the bare kind prefixes and two **8-hex-character
credential-id prefixes**, each immediately followed by the interposer's own `...`
redaction ellipsis. No credential body appears anywhere. A grep for JWT-shaped
ServiceAccount tokens (`eyJ[A-Za-z0-9_-]{20,}`) returns **0**, and no kubeconfig
file was left in the scratchpad. The only other credential-shaped strings are the
ServiceAccount *name* `system:serviceaccount:ci:w4-2-reuse-denied` inside 403
messages and a `credential-id` JTI from `kubectl auth whoami`, neither of which
is secret material.
