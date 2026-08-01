# W4-3 — `podStartTimeout`: a run Pod that never becomes ready

**Wave W4, Task 3.** The scenario attacks the one bound the Kubernetes agent
places on Pod start: `podStartTimeout`. Under `RestartPolicy: Never` a Pod that
cannot be scheduled — an unsatisfiable `nodeSelector`, an `ImagePullBackOff` —
**never transitions to `Failed` on its own**, so without a bound the agent's
wait would sit forever and the run would sit `Running` behind it. PR #51 added
the bound. This scenario measures whether it actually fires, whether the
failure is *reported* as well as bounded, whether the pooled-pod arm shares it,
and whether the config surface around it means what the docs say it means.

**Invariant attacked: I5.** Quoted verbatim from
`docs/superpowers/specs/2026-07-29-edge-case-testing-design.md:49`:

> | I5 | **Bounded recovery** — after fault injection the system returns to
> steady state within documented bounds (leader re-election ≤ seconds; stuck-run
> reap ≤ staleAfter 90s + interval 30s; the bounds in `docs/high-availability.md`
> are the contract) |

---

## Disclosure: the enrollment path under this scenario is bypassed

**Stated once, plainly.** Kubernetes agent enrollment does not work at HEAD and
cannot be made to work without a product-code change (`w4-0-enrollment-spike.md`:
`internal/controller/agent_enrollment_kubernetes.go:84-87` reads a TokenReview
`extra` key Kubernetes never populates). Every W4 scenario, this one included,
therefore runs against a controller whose **`POST /api/v1/agents/enroll` is
answered by test infrastructure** — an interposer that mints a credential
through the product's ordinary `"enrollment"` method instead. See
`scenarios/w4-rig.md` for what was built and what was measured about it.

**Consequence for this scenario's findings:** nothing here says anything about
the Kubernetes enrollment path beyond what `w4-0` already records. Everything
this scenario touches — claim, Pod create, the `awaitPodRunning` wait, `failRun`,
`FinishRun`, the pool, Pod deletion — is the real product path, unmodified.
`w4-rig.md` §Step 2 verified live that no request path reads `enrollment_method`.

---

## Corrections to inherited facts, established BEFORE execution

Per the W1/W2/W3 carry-forward rule, the plan's "Verified code facts" block
(`docs/superpowers/plans/2026-08-01-edge-case-campaign-w4.md:71-84`) is a set of
**claims**. Every one was re-read at this branch's HEAD. The `file:line` claims
all hold. **One mechanism claim does not, and it is the most consequential one
in the block.**

### CORRECTION 1 — "Both behaviours are documented at `docs/configuration.md:456`" is **false**, and the docs state the *opposite* of the shipped boot behaviour

The plan (`:74`) and this task's brief both assert a "deliberate asymmetry" in
which `Validate` **rejects** an unparseable `podStartTimeout` at boot while
`PodStartTimeoutDuration()` **falls back to 5m** for the same input, and that
**both** behaviours are documented at `docs/configuration.md:456`.

The two code behaviours are real (`config.go:215-219` and `config.go:142-151`,
verbatim below). **The documentation claim is not.** `docs/configuration.md:456`
reads, in full, on the fallback:

> Unset, unparseable, or non-positive values fall back to the default.

That sentence is the **only** operator-facing statement about invalid values,
and for the *unparseable* case it is **wrong**: an unparseable value does not
fall back to anything, it **refuses to boot**
(`cmd/k8s-agent/main.go:42-45` → `os.Exit(1)`). The boot rejection is documented
**nowhere**. A docs survey for the knob returns **4 hits outside
`docs/superpowers/`** — `docs/configuration.md:368` (the annotated sample
config), `:456` (the field table row), `:463` (the env-override sentence), and
`docs/kubernetes-integration.md:207` (the k8s field table row) — and **not one
of the four mentions that an unparseable value is rejected at boot.** (Hit count
reported, not truncated, per the recording rules; the full survey is in
`w4-3/docsurvey.txt`.)

So the honest shape of the asymmetry is not "two documented behaviours" but
**one documented behaviour that only two of its three stated inputs actually
get**. Filed — see Part D.

### CORRECTION 2 — the brief's suspected env-override defect does **not** exist

The brief asks whether `UNIFIED_K8S_POD_START_TIMEOUT` "may reach
`PodStartTimeoutDuration()` without passing `Validate`, in which case an
unparseable env value silently becomes 5m while an unparseable config value
refuses to boot". **It does not.** The env override is applied **inside**
`Validate`, at its very top, *before* the parse check further down the same
function:

```go
// internal/k8sagent/config.go:167-170
func (c *Config) Validate() error {
	if v := os.Getenv("UNIFIED_K8S_POD_START_TIMEOUT"); v != "" {
		c.PodStartTimeout = v
	}
	...
// internal/k8sagent/config.go:215-219
	if c.PodStartTimeout != "" {
		if _, err := time.ParseDuration(c.PodStartTimeout); err != nil {
			return fmt.Errorf("podStartTimeout %q: %w", c.PodStartTimeout, err)
		}
	}
```

An unparseable env value therefore takes **exactly the same** boot rejection as
an unparseable config value. `cmd/k8s-agent/main.go` has the only production
call site of `Validate` (`:42`), and it is unconditional and precedes every use
of the config. Part D confirms this **live**, in both directions, rather than
resting on the read. **Recorded as a refutation, not quietly dropped** — a
negative result on the brief's most-suspected defect is a result.

### Facts that held, re-read at HEAD

- `podStartTimeout` is `Config.PodStartTimeout` (`config.go:44`); env override
  `UNIFIED_K8S_POD_START_TIMEOUT` (`config.go:168-170`); **no CLI flag** —
  `cmd/k8s-agent/main.go:20-21` defines only `--config` and `--log-level`.
- Default **5m** (`defaultPodStartTimeout`, `config.go:138`); unset /
  unparseable / non-positive → 5m at the accessor (`config.go:142-151`).
- `awaitPodRunning` (`agent.go:415`) wraps `pm.WaitForPodRunning`
  (`podmanager.go:96-115`, a 500 ms `Get` poll) in `context.WithTimeout`. On
  timeout `executeRun` (`agent.go:340-347`) calls `failRun` (`:392-404`), which
  writes the reason into the run's own log at `stepIndex -1` via `AppendLogBulk`
  and then `RetryUntilSuccess(FinishRun(RunFailed))`.
- **Second exit:** a concurrent goroutine (`agent.go:426-446`) polls `GetRun` at
  `agentlib.CancelPollInterval` and cancels the wait if the controller marks the
  run terminal, returning `masterTerminal=true`; the caller then **abandons
  without writing status** (`:341-344`).
- **The pooled arm shares the timeout — there is no second one.**
  `awaitPodRunning` is called **once**, at `agent.go:340`, after the
  pooled/fresh branch converges at `:279-338`. The difference is cleanup only,
  via the `podReady` bool (`:292`, set at `:349`): a never-ready pooled pod is
  **deleted, not released to the pool** (`:307-319`). Both defers use
  `context.Background()`.
- **Three distinct sites share the one knob**, and this scenario must not
  conflate them: (1) the run pod's cold start, (2) the pooled pod that never
  becomes ready — same call site as (1) — and (3) the `uses:`-scope pod's wait
  (`backend.go:167`, added by PR #90). `config.go:133-137` says the sharing is
  deliberate. **Site (3) is out of scope here** and is named only so a later
  reader does not mistake this scenario's numbers for its bound.

---

## Rig and configuration

Bring-up exactly as `w4-rig.md` §Bring-up: `test/ha/docker-compose.ha.yaml`
(the `k8senroll` overlay is **not** used — this scenario needs no 403 control),
then `test/edgecase/tools/w4/w4-up.sh`.

**The one deviation from the rig's defaults:** the agent is started with
`UNIFIED_K8S_POD_START_TIMEOUT=30s` exported into `w4-up.sh`'s environment, so
the agent inherits it. `test/edgecase/k8s/w4-agent-config.yaml` carries
`podStartTimeout: 60s`; the env override wins (`config.go:168-170`), and using
it rather than editing the file means **the override path is exercised by the
scenario's own main line**, not only by Part D. 30 s collapses the 5-minute
product default so each trial resolves inside a capture window.

Fixtures: `w4-pending.payload.json` (`edge-w4-pending`, pod-level
`nodeSelector: disktype: ssd` that no node in the single-node
`docker-desktop` cluster satisfies) for Parts A and C, and a new
`w4-pending-reuse.payload.json` (`edge-w4-pending-reuse`, the same
`nodeSelector` plus `podTemplate.reuse: true`) for Part B. Both verified
through the real `dsl.Parse` twice — source YAML and YAML re-extracted from the
payload — per the README rule (`w4-3/fixcheck.txt`).

---

## Part A — cold start: does the bound fire, and is the failure reported?

**Method.** Trigger `edge-w4-pending`. Sample `kubectl get pods -n ci` and the
run's status once a second from the trigger. Record: the Pod's creation, that it
stays `Pending`, the wall time from Pod creation to the run reaching `Failed`
against the configured 30 s, the presence of a `stepIndex -1` line in the run's
own log carrying the reason, and the Pod's deletion.

**What would be a violation.** The run sitting `Running` past the bound with no
terminal status; or a terminal status with **no** reported reason (the campaign
has already recorded one such case — the pod-deleted 137 in `w4-rig.md` §Step 7
— and a second, on a path whose whole purpose is to report, would be worse).

**Result:** see §Results below.

## Part B — the pooled arm: same bound, different cleanup

**Method.** Trigger `edge-w4-pending-reuse`. `podTemplate.reuse: true` sends the
claim down `pool.ClaimPod` (`agent.go:294-302`) instead of `BuildPod`+`CreatePod`.
Measure the same elapsed window as Part A and compare. Then check, **by pod name
and by pool annotation**, that the wedged pod is gone rather than idle:
`test/edgecase/tools/w4/w4-k8s-inject.sh pods` and `... annotations` read
`unified-cd/pool-status` directly (`pool.go:20-31`).

**What would be a major finding.** A wedged pod **returned to the pool**
(`pool-status = idle`) rather than deleted: the pool would then hand a pod that
has never once been schedulable to the next run, turning one unschedulable job
into a persistent, cross-job failure.

**A naming trap carried forward from `w4-rig.md` §Step 5:** under `reuse`, a
pod's name and its `unified-cd/runId` label carry the **first** run's id forever.
It does not bite here — this pod is created fresh for this run and never
released — but the check is written on `pool-status` and on the pod list, not on
a label lookup, so that it stays correct if it ever does.

**Result:** see §Results below.

## Part C — the second exit: cancel while the Pod is still Pending

**Method.** Trigger `edge-w4-pending`, wait for the Pod to exist and be
`Pending`, then cancel the run through the API well inside the 30 s bound.
Expect `awaitPodRunning`'s concurrent `GetRun` poller (`agent.go:426-446`) to
observe the terminal status, cancel the wait, and return `masterTerminal=true`;
and `executeRun` (`:341-344`) to log
`k8s: run became terminal before pod ready; abandoning` and **return without
calling `failRun`**.

**This is documented behaviour** (`docs/kubernetes-integration.md:207`: "The wait
also aborts early (without overriding the controller's status) if the run is
already terminal at the controller"), so the expected filing is **conformance**,
not a finding. Record precisely: the run's final status, its step rows, whether
any `stepIndex -1` line was written, and whether the pod was cleaned up (the
deferred `DeletePod` runs on `context.Background()`, so it must survive the
cancellation).

**Result:** see §Results below.

## Part D — the config surface: boot rejection vs runtime fallback

Four live boots of the real `k8s-agent` binary, each with the rest of the rig
untouched, against `test/edgecase/k8s/w4-agent-config.yaml`:

| # | Input | Predicted from code |
|---|---|---|
| D1 | config file `podStartTimeout: not-a-duration` | `Validate` error → `os.Exit(1)`, message `invalid config … podStartTimeout "not-a-duration"` |
| D2 | env `UNIFIED_K8S_POD_START_TIMEOUT=not-a-duration`, config file valid | **same rejection** — refuting the brief's hypothesis that the env path bypasses `Validate` |
| D3 | env `UNIFIED_K8S_POD_START_TIMEOUT=-1s` (parseable, non-positive) | boots fine; `PodStartTimeoutDuration()` silently yields **5m** |
| D4 | D3's agent left running against `edge-w4-pending` | the run does **not** fail at 30 s or 60 s — it waits the full 5m default, proving the silent substitution is the one in effect |

D4 is the part that makes D3 a measurement rather than a code reading: a boot
that succeeds proves only that `Validate` accepted the value, not which duration
the runtime then used.

**Discoverability is the question Part D actually answers.** Both behaviours are
defensible in isolation. What matters to an operator is whether the pairing is
*findable*: whether `docs/*.md` says which inputs are rejected and which are
silently substituted, and whether the running agent tells you which duration it
ended up with. See Correction 1 above for the docs half, and the Results for
what the agent logs.

**Result:** see §Results below.

---

## Results

Executed 2026-08-01 against the rig described above: `test/ha/docker-compose.ha.yaml`
brought up cold (**all three controllers came up** — the W4-0 bootstrap-PAT race
did not fire, consistent with `w4-rig.md`'s correction that it is a race and not
a certainty), then `w4-up.sh` with `UNIFIED_K8S_POD_START_TIMEOUT=30s` exported.
The interposer answered **2** enrollment exchanges over the session
(`w4-3/teardown.txt`), so the bypass was in effect and the disclosure above
applies. Raw captures are in the session scratchpad under `w4-3/`; every number
below traces to one of them.

### Verdict summary

| Part | Result |
| --- | --- |
| A. Cold start | **CONFORMANT** — pod created and `Pending` throughout; run `Failed` after a **30.198 s** wait against a 30 s bound; reason written at `stepIndex -1`; pod deleted |
| B. Pooled arm | **CONFORMANT** — **30.193 s**, 5 ms from Part A: the same bound, not a second one. Wedged pod **deleted**, not returned to the pool |
| C. Second exit | **CONFORMANT** — agent abandoned 220 ms after the controller wrote `Cancelled`; no status written by the agent, no `stepIndex -1` line, pod still cleaned up |
| D. Config surface | **1 violation + 2 observations** — the docs promise a fallback the product does not perform; the brief's suspected env defect is **refuted** |

**Three of the four parts found the product behaving exactly as designed and
documented, and that is recorded as conformance rather than dressed up.** The
one contradiction is in Part D, and it is against `docs/`, not against I5.

### I5 was attacked and **held**; this scenario files nothing on it

Parts A and B are the I5 arm, and neither misses a documented bound: the run
reached a terminal state within **0.7 %** of the configured `podStartTimeout`
(30.198 s and 30.193 s against 30 s), the failure was reported into the run's
own log, and the pod was reclaimed. **No entry below is filed on I5**, per the
rule at `FINDINGS.md:1509` that an invariant must be contradicted by its own
text. The Part D entries are filed on the documented-contract limb and as
observations.

### Part A — cold start (run `9262f762-7bf3-45f1-8144-7d601057cc8a`)

Capture `w4-3/partA.txt`, `w4-3/partA-agentlog.txt`.

```
TRIGGER edge-w4-pending at 06:51:03.724 runId=9262f762-...
06:51:04.156 t=0.24s  status=Queued   pods=ucd-run-9262f762-7bf3-45 0/2 Pending
06:51:05.591 t=1.69s  status=Running  pods=ucd-run-9262f762-7bf3-45 0/2 Pending
 ... 20 further 1 Hz samples, all `Running` / `0/2 Pending` ...
06:51:34.090 t=30.16s status=Running  pods=ucd-run-9262f762-7bf3-45 0/2 Pending
06:51:35.513 t=31.61s status=Failed   pods=<none>
```

**The measurement window and what it bounds.** The 1 Hz sampler brackets the
transition to `[30.16 s, 31.61 s]`, which is too coarse to state as a result.
The precise figure is the agent's own pair of timestamps:

| Event | Time (UTC) | Source |
| --- | --- | --- |
| run claimed | `06:51:04.070214` | `runs.claimed_at` |
| `k8s: executing Run` | `06:51:04.0744` | agent log |
| `k8s: run pod did not become ready: …` | `06:51:34.2723` | agent log |
| `stepIndex -1` log row lands | `06:51:34.272826` | `logs` table |
| run reaches `Failed` | `06:51:34.384859` | `runs.updated_at` |

**Wait window = 30.198 s against a configured 30 s** — an overshoot of 198 ms,
which itself *contains* `BuildPod` + `CreatePod`, since `executing Run` precedes
them and the `context.WithTimeout` only starts inside `awaitPodRunning`
(`agent.go:340`, `:416`). The true overshoot on the bound is smaller than
198 ms. From claim to terminal status: **30.315 s**.

**This is also the live confirmation that the env override reaches the
runtime.** `w4-agent-config.yaml` carries `podStartTimeout: 60s`; the wait was
30.2 s. The file's value lost to `UNIFIED_K8S_POD_START_TIMEOUT`, exactly as
`config.go:168-170` says.

**Reported, not merely bounded.** One row exists in `logs` for this run and it
is `failRun`'s:

```
step_index | stream | ts                            | line
        -1 | stderr | 2026-08-01 06:51:34.272826+00 | k8s: run pod did not become ready: failed to get Pod
                                                      ucd-run-9262f762-7bf3-45: client rate limiter Wait
                                                      returned an error: context deadline exceeded
```

`step_reports` has **zero** rows — no step ever started, which is correct: the
run failed before `RunClaim` was entered. The pod was absent from the first
sample after the transition and from all **3** cleanup samples over the
following 9 s.

**The reason text is the one thing here worth filing.** It names neither the
timeout, nor its value, nor the Pod's `Pending` phase, nor the scheduling
failure that caused it — it surfaces client-go's rate-limiter error, because
`WaitForPodRunning`'s deadline is nearly always consumed inside the in-flight
`Pods().Get` (`podmanager.go:98-100`, wrapped `failed to get Pod %s: %w`) rather
than at the loop's own `ctx.Done()` check (`:108-110`), which would have
returned a clean `context.DeadlineExceeded`. Filed as an observation.

### Part B — the pooled arm (run `38b62b88-fb4a-45d3-9b7a-52d05b332895`)

Capture `w4-3/partB.txt`, `w4-3/partB-midflight.txt`.

**Mid-flight (t ≈ 9 s), proving the claim really went through the pool** —
`w4-k8s-inject.sh annotations`, reading `unified-cd/pool-*` (`pool.go:20-31`):

```
== ucd-run-38b62b88-fb4a-45 ==
  pool-status  = in-use
  pool-key     = a0e28a12adce9064a42fa3daff719ed8
  label runId  = 38b62b88-fb4a-45d3-9b7a-52d05b332895
  phase        = Pending

Events:
  Warning  FailedScheduling  8s  default-scheduler  0/1 nodes are available:
    1 node(s) didn't match Pod's node affinity/selector. …
```

`pool-key` is set, so this pod came from `createPoolPod` (`pool.go:228`), not
from `BuildPod` — the `usePool` branch at `agent.go:294-302` was taken. The
`FailedScheduling` event is the fault itself: the single-node `docker-desktop`
cluster carries no `disktype: ssd` label.

**Same bound, to within 5 ms:**

| | Part A (fresh) | Part B (pooled) |
| --- | --- | --- |
| `k8s: executing Run` | `06:51:04.0744` | `06:53:00.5030` |
| failure logged | `06:51:34.2723` | `06:53:30.6959` |
| **wait window** | **30.198 s** | **30.193 s** |

**There is no second timeout.** The two arms differ by 5 ms on a 30 s bound,
which is the expected result of one `context.WithTimeout` at one call site
(`agent.go:340`) reached by both branches. The reason string, the `stepIndex -1`
row, the `Failed` status and the empty `step_reports` are identical to Part A.

**The wedged pod was deleted, not pooled — checked two ways, immediately after
the terminal status:**

```
$ w4-k8s-inject.sh pods
(no output — no run pods in ci)
$ w4-k8s-inject.sh annotations
(no run pods)
```

No pod named `ucd-run-38b62b88-fb4a-45` survives, and **no pod carries
`pool-status = idle`**. So the `!podReady` defer (`agent.go:307-313`) ran and
took the delete branch rather than `ReleasePod`. The failure mode the brief
flagged as major — a never-schedulable pod handed to the next run — **does not
occur**. The agent logged nothing about the deletion, which is correct: that
defer logs only on failure (`:310-312`).

### Part C — the second exit (run `a2483a09-3edf-409d-951f-89776153d03b`)

Capture `w4-3/partC.txt`. `POST /api/v1/runs/{id}/cancel` → **204** at
`06:54:27.66`, with the pod `Pending` and ~20 s of the 30 s bound still unspent.

| Event | Time (UTC) |
| --- | --- |
| `k8s: executing Run` | `06:54:17.8620` |
| controller writes `Cancelled` (`runs.updated_at`) | `06:54:27.706825` |
| agent: `k8s: run became terminal before pod ready; abandoning` | `06:54:27.9271` |
| pod absent | by `06:54:29.177` (next sample) |

**Conformance, and it matches `docs/kubernetes-integration.md:207` exactly.**
The agent noticed **220 ms** after the controller's write — inside one
`agentlib.CancelPollInterval` tick (`orchestrator.go:37`, **5 s**), which is the
bound on this path; 220 ms is a lucky tick alignment, not a guarantee, and a
re-runner should expect anything up to ~5 s plus one `GetRun` round trip.

What the run looks like afterwards, stated as the brief asked:

- **final status `Cancelled`** — the controller's, untouched. `failRun` was not
  called, so `FinishRun(RunFailed)` never raced it.
- **zero rows in `logs`** — no `stepIndex -1` line. The whole run produced no
  log output at all, which is the honest consequence of abandoning without
  writing status: an operator inspecting a cancelled run sees nothing about the
  pod that was created and destroyed on its behalf. **Recorded, not filed:**
  this is the documented behaviour working, and `agent.go:341-344`'s
  `slog.Info` gives the *agent's* operator the record. Nothing in `docs/`
  promises the run's log carries it.
- **zero rows in `step_reports`.**
- **the pod was still cleaned up**, because the deferred `DeletePod` runs on
  `context.Background()` (`agent.go:307-337`) and so survives the cancellation
  of the claim context.

### Part D — the config surface

Captures `w4-3/partD-boot.txt`, `w4-3/partD-d3.txt`, `w4-3/partD-d4.txt`,
`w4-3/docsurvey.txt`.

**D1 — unparseable value in the config file.** Real binary, real config path,
env cleared:

```
{"level":"ERROR","msg":"invalid config","error":"podStartTimeout \"not-a-duration\": time: invalid duration \"not-a-duration\""}
D1 EXIT CODE = 1
```

**D2 — the same value in `UNIFIED_K8S_POD_START_TIMEOUT`, with a valid
(`60s`) config file.** This is the brief's suspected defect, and the result
**refutes it**:

```
{"level":"ERROR","msg":"invalid config","error":"podStartTimeout \"not-a-duration\": time: invalid duration \"not-a-duration\""}
D2 EXIT CODE = 1
```

Byte-identical message, same exit code. The env override does **not** slip past
`Validate` into `PodStartTimeoutDuration()`; it is applied at `config.go:168-170`,
*above* the parse check at `:215-219` in the same function, so it inherits the
rejection. **A negative result on the most-suspected defect, recorded as a
result.** (One small consequence of the shared message: it quotes the *value*
but not its *source*, so an operator who set the env cannot tell from the log
whether the file or the environment was the culprit. Noted, not filed — the
value is quoted, which is the load-bearing half.)

**D3 — a parseable but non-positive env value (`-1s`).** The agent **booted
normally** and registered. `Validate` has no positivity check; only the accessor
does.

**D4 — which duration did D3's agent actually use?** `edge-w4-pending` triggered
against it, sampled at 1 Hz for the full window:

```
06:56:16.532 t=30.65s  status=Running  pods=… 0/2 Pending    <- not the 30s the earlier agent used
06:56:46.568 t=60.73s  status=Running  pods=… 0/2 Pending    <- not the 60s in the config FILE
06:57:46.477 t=120.63s status=Running  pods=… 0/2 Pending
06:59:46.493 t=240.64s status=Running  pods=… 0/2 Pending
07:00:47.622 t=301.78s status=Failed   pods=<none>
```

Agent timestamps: `executing Run` `06:55:46.0689` → failure `07:00:46.4180` =
**300.349 s**. The resolved duration was **5m — the compiled-in default**.

**The sharp edge D4 exposes, which the code read alone does not.** The config
file said `podStartTimeout: 60s` and was valid. The env said `-1s`. The result
was neither: `Validate` **overwrites the field** with the env string
(`config.go:169`) and `PodStartTimeoutDuration()` then falls back to the
*global* default rather than to the file's value (`config.go:147-150`). **A
non-positive env value therefore silently discards a valid operator-set
config-file value and quintuples the bound**, from the 60 s the operator wrote
to 5 m. Filed as an observation.

**Discoverability — the question Part D exists to answer.** Two answers, both
negative:

1. **The docs describe the runtime fallback and never mention the boot
   rejection.** The full survey (`w4-3/docsurvey.txt`, reported untruncated:
   **28** hits across `docs/`, of which **4** are outside `docs/superpowers/`)
   is `docs/configuration.md:368`, `:456`, `:463` and
   `docs/kubernetes-integration.md:207`. `:456` says "Unset, unparseable, or
   non-positive values fall back to the default" — true for two of its three
   inputs and **false for the third**. No operator-facing document says an
   unparseable value refuses to boot. Filed as a violation on the
   documented-contract limb. *(Enumeration checked, not assumed: the sibling
   rows for `drainTimeout` and `poolIdleTimeout` at `:457-458` make no
   equivalent unparseable-falls-back claim, even though `DrainTimeoutDuration`
   has the same shape — so this is one wrong sentence, not a pattern across the
   table.)*
2. **The running agent never states the duration it resolved.** The entire D3/D4
   agent log is **4 lines** — the `sidecarS3SecretName` warning, `k8s agent
   registered`, `executing Run`, and the failure — and none of them carries the
   timeout. A `grep` for it over that log returns **0**. There is no way, short
   of triggering an unschedulable job and timing it (which is what D4 had to
   do), for an operator to learn which value is in effect.

**One doc inaccuracy noted and deliberately not filed.**
`docs/superpowers/specs/2026-07-15-k8s-agent-resilience-design.md:27` and `:96`
say "`Validate` clamps non-positive to the 5m default". It does not — `Validate`
leaves the value alone and the *accessor* substitutes. The observable result is
the same, and a design spec under `docs/superpowers/` is a plan record, not the
operator-facing contract that the recording rules put behind the
documented-contract limb. Recorded here so the next reader of that spec is not
misled.

## Findings filed

**1 violation + 2 observations.** All three are Part D; Parts A, B and C are
conformance and are filed as nothing.

| # | Kind | Title (see `FINDINGS.md`) |
| --- | --- | --- |
| 1 | **violation** (contract limb, minor) | `docs/configuration.md:456` promises an unparseable `podStartTimeout` falls back to the default; the agent refuses to boot instead |
| 2 | observation (minor) | the pod-start timeout reports a client-go rate-limiter error, naming neither the timeout, its value, nor the Pod's `Pending` state |
| 3 | observation (minor) | a non-positive `UNIFIED_K8S_POD_START_TIMEOUT` silently discards a valid config-file value and substitutes 5m; nothing logs the resolved duration |

`FINDINGS.md` was grepped for the findings themselves before filing, not merely
for the doc text: `podStartTimeout` / `PodStartTimeout` /
`UNIFIED_K8S_POD_START_TIMEOUT` return **0** hits in the file at the time of
writing, and `unparseable` returns 2, both W4-0 entries about
`providerConfig`. Nothing here is a re-filing.

## Teardown

`w4-down.sh` (agent SIGTERMed first; both processes' final output captured),
then `docker compose … down -v`. No run pods remain in `ci`; W4-0's
`w4-spike-agent-pod` is left in place, as W4-0 left it. Every capture was swept
for `uca_` / `ucr_` / `uce_` material followed by a credential body — **0
matches**; the interposer log carries id prefixes only.
