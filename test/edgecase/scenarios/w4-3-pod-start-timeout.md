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

*(Filled in after execution — see the commit that follows this one.)*

## Findings filed

*(Filled in after execution.)*
