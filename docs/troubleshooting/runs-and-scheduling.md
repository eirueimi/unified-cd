# Runs and Scheduling

## Run stays `Queued` forever

**Symptom**

A triggered run never leaves the `Queued` status, even though agents are connected:

```
ID:          17c9e93a-7c33-48be-831c-d7b9098ba887
Job:         my-job
Status:      Queued
```

**Cause**

No connected agent satisfies the job's `agentSelector`, or every agent that
does is already at its concurrency limit. Claiming only happens when an
agent's label set is a superset of `agentSelector` (AND match) **and** the
agent has a free concurrency slot.

> **"Forever" is conditional.** A run that no *live* agent's labels can
> satisfy is auto-failed by the queued-run reaper once it is older than
> `UNIFIED_QUEUED_RUN_GRACE` (default `5m`, plus up to one 30s sweep), with
> the reason written to the run's own log — see [Run failed with "no eligible
> agent available to claim it"](#run-failed-with-no-eligible-agent-available-to-claim-it)
> below. A run stays `Queued` indefinitely only while some live agent *does*
> match its labels but cannot run it (capability mismatch, or every slot
> busy). If the run is still `Queued` well past the grace, the cause is the
> latter.

**Fix**

Check which agents are connected and what labels they advertise:

```bash
unified-cli agent list
# docker-agent-1   c1e136ded609   linux         hostname:c1e136ded609,kind:docker,pool:default   2026-07-04 04:54
# k8s-agent-1      DESKTOP-EMUF6H6 windows/k8s   kubernetes,kind:kubernetes,pool:default,hostname:...    2026-07-04 04:54
```

- Compare the job's `agentSelector` against the label sets above — every
  selector entry must have an exact match on some agent.
- If the labels match but the run is still stuck, the matching agent(s) may
  already be running `--max-concurrent` jobs; start another agent in the pool
  or wait for a slot to free up.
- If the job is called via a `call:` step from another run, check for the
  slot-deadlock case first — see [Calling Other Jobs (`call`)](../user-guide/writing-jobs/templates-and-reuse.md#calling-other-jobs-call).
  A parent run holding its only agent slot while waiting on a same-pool child
  looks identical to this symptom but requires raising `--max-concurrent`
  instead of relabeling agents.

Cancel a run stuck this way with `unified-cli run cancel <run-id>`.

## Run failed with "no eligible agent available to claim it"

**Symptom**

A `Queued` run is failed automatically, and its own log (not just the
controller log) carries a single line:

```
run failed: no eligible agent available to claim it (requires agent labels: kind:linux)
```

with `WARN queued-run reaper: failed unclaimable queued run` on the leader.

**Cause**

The run sat `Queued` longer than `UNIFIED_QUEUED_RUN_GRACE` (default `5m`,
measured from the run's `created_at`) while no live agent's labels satisfied
its `agentSelector` — typically a full agent outage. The reaper fails such
runs rather than leaving them "in progress" forever.

Note the parenthetical names the selector the run *requires*; it is not a
claim that the selector was the thing that was wrong. When every agent is
gone, nothing matches, whatever the selector says.

**Fix**

- Bring the agent pool back, then re-trigger the run.
- If the outage was planned and longer than the grace, raise
  `UNIFIED_QUEUED_RUN_GRACE` before the next one. The failure lands in
  `[grace, grace + 30s)` after `created_at`, so size the window against
  `grace`, and remember the clock starts at run creation — a run that spent
  the grace blocked on a mutex enters `Queued` with none of it left.

A run that a returning agent claims before the reaper writes is left alone:
the reaper re-checks each run at the moment it fails it and logs
`INFO queued-run reaper: run left the Queued state before it could be
failed; skipping`. Seeing that line means the race was caught, not that
anything went wrong.

## Job stays Queued / unschedulable warning

**Symptom**

A run stays `Queued` and never gets claimed, and the job's page in the Web
UI shows a warning banner near the top:

```
⚠ This job can't be scheduled right now: no registered agent provides
capability [pod]. Runs will stay Queued until a matching agent registers.
```

**Cause**

This is a stronger, more specific version of the generic ["Run stays Queued
forever"](#run-stays-queued-forever) symptom above. Every job now has an
inferred capability requirement — `native`, `container`, or `pod` — derived
from its spec (see [Capabilities and
routing](../operator-manual/agents.md#capabilities-and-routing)), on top of any hand-written
`agentSelector`. The banner means the controller checked the **current
agent inventory** via `GET /api/v1/jobs/{name}/schedulability` and found
that no registered agent satisfies **both**: capabilities ⊇ the job's
required capability, AND labels ⊇ the job's `agentSelector`. Unlike the
generic Queued symptom (which can also just mean "every matching agent is
busy"), this banner only fires when the mismatch is structural — no
currently-connected agent could ever claim this run, busy or not.

The `Reason` field (and banner text) tells you which half failed:

- `no registered agent provides capability [...]` — no connected agent
  reports the needed capability at all (e.g. a `podTemplate` that needs
  Kubernetes, but no k8s-agent is registered; or a `native: true` job with
  only k8s-agents online). For `[container]`, check the standard agents' logs
  for `no container runtime`: an agent that started before docker/podman was
  running, or without it on `PATH`, registers as native-only and stays that
  way until it is restarted. The same happens silently to a single host when
  other agents still provide `container`, so that host's isolated jobs go to
  the others or wait.
- `no registered agent matches labels [...]` — at least one agent has the
  right capability, but none also carries every label in `agentSelector`.
- `no agent declares volume "..."`, `no agent declares any of the volumes [...]`,
  or `no single agent that provides capability [...] and matches labels [...]
  declares all of the volumes [...]` — the job mounts a named volume no
  suitable agent declares. See [Job stays Queued: no agent declares
  volume](#job-stays-queued-no-agent-declares-volume).

If `agentSelector` contains a `{{ .Params.X }}` expression, the label half
can't be evaluated from the job definition alone (it only resolves at
trigger time with real parameter values); the banner is suppressed for the
label part and the API response sets `selectorDependsOnParams: true` —
schedulability isn't falsely reported just because a selector is
parameterized.

**Fix**

- Register (or start) an agent that reports the missing capability — a
  standard agent reports `native` (+ `container` with a runtime installed),
  a Kubernetes agent reports `pod` + `container`. See [Capabilities and
  routing](../operator-manual/agents.md#capabilities-and-routing) for the full model.
- Or adjust the job: drop an `agentSelector` label that no connected agent
  carries, or change `native`/`podTemplate` so the job's inferred
  requirement matches an agent you actually have (e.g. remove a
  Kubernetes-only `podTemplate` feature so the job infers `container`
  instead of `pod`, letting it run on a standard agent too).
- Legacy agents (pre-upgrade binaries reporting no `capabilities`) are
  counted as standard host agents advertising `native` and `container`, same
  as the claim itself does: they satisfy a `native` or `container` job (by
  label) without being re-registered, but never a `pod` job. If the missing
  capability is `pod` and your Kubernetes agent is running, check that it is
  on a current build: an agent that sends no capability set on its claim
  polls cannot recover a record that was re-created without one until it
  restarts.
- Once a satisfying agent registers, the banner disappears on the job page's
  next load and the run is claimed on the matching agent's next poll — no
  need to re-trigger it.

## Job stays Queued: no agent declares volume

**Symptom**

A job that mounts a [named volume](../user-guide/writing-jobs/volumes.md) (`spec.volumes`) is
applied without error, but its runs stay `Queued`, and the job's page in the Web UI shows an
unschedulable banner. `GET /api/v1/jobs/{name}/schedulability` returns `"satisfiable": false`
with one of three reasons:

```
no agent declares volume "unity-main"
```

```
no agent declares any of the volumes ["go-cache-1" "go-cache-2"]
```

```
no single agent that provides capability [native] and matches labels [macos] declares all of the volumes [["unity-main"] ["go-cache-1" "go-cache-2"]]
```

**Cause**

A volume is declared in an *agent's* own config, never in the job, and the controller records
only the names agents advertise (no definitions, no occupancy), so applying a job that names a
volume nobody declares is not an error: the run is queued and simply never claimed. Routing is an AND of ORs. For every `spec.volumes` entry the
agent must declare at least one candidate name, and one agent must satisfy every entry, because a
run is claimed by exactly one agent.

- The first form: no registered agent advertises that single name.
- The second form: the entry lists several candidate names and no registered agent advertises
  any of them.
- The third form: every candidate set *is* declared somewhere, but never all by one agent that
  also has the job's capability and carries every `agentSelector` label. The volumes are on
  different agents (or on an agent the selector excludes). Two agents declaring the same name
  hold two independent copies, which is fine; the failure is that no *one* agent holds them all.

A volume that is declared but currently **busy** is not this: that run is claimed and waits (see
[A run waits for a volume another run holds](steps-and-execution.md#a-run-waits-for-a-volume-another-run-holds)).

**Fix**

- Check what the agents actually advertise. The volume names an agent declares are in the
  `volumes` field of `GET /api/v1/agents` and `GET /api/v1/agents/{agentId}` (`unified-cli agent
  list` does not print them).
- A name that is simply not declared: add it to an agent's `volumes:` list and restart that agent
  (the list is read at startup; there is no flag or environment variable form). On a Kubernetes
  agent the config also needs `agentName` and a `size` for each volume.
- A name that differs by spelling or case from the declared one: names are matched exactly.
- The third form: declare all the volumes on one agent, relax the job's `agentSelector`, or
  mount fewer volumes.
- Once a satisfying agent registers the run is claimed on its next poll; no need to re-trigger.

**`volumesDependOnParams`.** If a candidate name contains a `{{ .Params.X }}` expression, it only
resolves when the run is queued, so the controller cannot judge it from the job definition. That
entry is left out of the check, no banner is raised for it, and the schedulability response sets
`"volumesDependOnParams": true`, exactly as `selectorDependsOnParams` does for a parameterised
`agentSelector` (see the section above). The consequence is that a templated volume name which
*resolves* to something nobody declares gives a run that stays `Queued` with no banner. Read the
resolved name from the run's params, and compare it with the agents' `volumes`.

## Run stays Queued: the volume it mounts is below `minFreeDisk`

**Symptom**

A job that mounts a [named volume](../user-guide/writing-jobs/volumes.md) stays `Queued` while jobs
that do not mount it run normally on the same agents. The job page shows no unschedulable banner
(the volume is declared). The agent that declares the volume logs, once when the volume enters the
gate:

```
level=WARN msg="volume below minFreeDisk, not claiming runs that mount it" volume=unity-main freeBytes=3221225472 totalBytes=214748364800 thresholdBytes=21474836480
```

and `/metrics` shows:

```
unifiedcd_agent_volume_claim_blocked{agent="build-agent-01",agent_name="build-host",volume="unity-main"} 1
```

When the volume recovers the agent logs `volume no longer below minFreeDisk (or no longer measured),
claiming runs that mount it again` (`or its reading is too old to trust` on a Kubernetes agent).

**Cause**

The free space on the filesystem backing that volume is below the agent's `minFreeDisk` (default
`1%`), so the agent leaves the volume out of what it will claim. Only runs that mount it wait; the
agent keeps claiming everything else, and another agent that declares the volume can still claim the
run. Nothing is deleted and no run fails. This is different from the workspace gate, which stops all
claiming ([`free disk space below minimum, skipping
claim`](agents-and-enrollment.md#a-host-agent-is-not-claiming-free-disk-space-below-minimum-skipping-claim)).

On a **host agent** the reading comes from the agent's 15-second background sample, so the gate
follows the volume within about that time. A volume with no fresh reading is not gated. On a
**Kubernetes agent** a volume is measured only by a run that mounts it, so the gate rests on the
reading from the last such run: after 10 minutes the reading no longer counts, one probe run is let
through and measures the volume again (its run log warns `volume ... has ... free (...), below
minFreeDisk ...; this run may fail for lack of space` if it is still low), and the gate re-arms if
so. The `volume_claim_blocked` series reads `0` once a reading is more than 10 minutes old.
Each Kubernetes replica keeps its own readings.

**Fix**

- Free space on that volume's filesystem (clear old build output inside the volume, expand the PVC
  through your StorageClass, or move the volume to a bigger disk). A host agent lifts the gate on the
  next sample; a Kubernetes agent lifts it when the stale reading expires, or at once if you restart
  the agent (`kubectl rollout restart deployment/unified-cd-k8s-agent`), which clears its readings.
- If the threshold is simply too high for this volume, lower `minFreeDisk` in the agent's config
  (for example `"500MiB"`, or `0` to disable it) and restart the agent. See [Operations: Choosing a
  threshold](../operator-manual/operations.md#choosing-a-threshold).
- To see which volumes are gated across the fleet:
  `max by (agent_name, volume) (unifiedcd_agent_volume_claim_blocked) == 1`.
- If the log instead says `minFreeDisk is not smaller than the volume, so the volume is not gated`,
  the threshold is at least as big as the volume, the volume is *not* the reason a run is queued, and
  the threshold is wrong for it.

## Run marked `Failed` with "agent lost"

**Symptom**

A run that was `Running` flips to `Failed` with no step-level error, and the
controller log shows:

```
stuck-run reaper: failed orphaned run (agent lost)
```

**Cause**

The line carries a `reason` field naming which of the two paths fired.

- `reason=agent heartbeat stale` — the agent that claimed the run stopped
  sending heartbeats (crashed, was killed, or lost network connectivity) and
  never resumed. This is the common case: investigate agent health.
- `reason=agent inventory row absent for longer than the staleness window` —
  the claiming agent has no row in `agents` at all, and did not get one back
  across the confirmation window. Look for what *removed* the row rather than
  for a sick agent: `DeleteStaleAgents` (the agent was already dead for five
  minutes), a `DELETE /api/v1/agents/{id}`, or an agent process deregistering
  at the end of its drain under an agent ID another process is also using. In
  that last case the controller will also have logged
  `WARN agent heartbeat re-created a missing inventory row` for the live
  process — see [duplicate agent IDs](../operator-manual/agents.md).

The controller's orphaned-run reaper detects a `Running` run whose claiming
agent has gone away by either route and fails the run rather than leaving it
stuck forever. It fails (never re-queues) the run, since re-running
partially-executed steps risks duplicating side effects like deploys.

**Fix**

This is expected recovery behavior, not a bug to work around — the run
genuinely needs to be re-triggered once the underlying agent problem is
fixed:

- Confirm the agent is back and healthy: `unified-cli agent list`.
- Re-trigger the job once the agent (or a replacement in the same pool) is
  available.
- On Kubernetes, the run's `ucd-run-*` pod is garbage-collected separately;
  no manual pod cleanup is required.
- See [High Availability Guide: Orphaned-Run Recovery](../operator-manual/high-availability.md#orphaned-run-recovery)
  for the full heartbeat/reaper timing and design.

## Run marked `Failed` by heartbeat reconcile after a lost claim

**Symptom**

A run flips from `Running` to `Failed` unusually fast — well under the
stuck-run reaper's ~90s-stale-heartbeat-plus-grace window above — with no
step ever having reported progress, and `unified-cli agent list` shows the
run's claiming agent as perfectly healthy the whole time (`last_seen_at`
kept advancing normally; the agent never went stale). The controller log
does **not** show `stuck-run reaper: failed orphaned run (agent lost)` for
this run — that line is specific to the slower stale-heartbeat path.

**Cause**

Every agent heartbeat now reports the run IDs it currently considers active
(`activeRunIds`). This particular run's claim never made it into that set —
typically because the HTTP response to a successful `Claim` call never
reached the agent process (a network blip right after claiming, or the
agent restarted in the instant between claiming and starting the run) — so
the agent itself never learned it owned the run and correctly never reports
it as active. The controller cross-checks this on every heartbeat: a
`Running` run assigned to that agent, absent from its self-reported active
set, and claimed more than ~60s ago (a grace window so a claim whose
heartbeat simply hasn't landed yet isn't reaped prematurely) is failed as
orphaned right there — instead of waiting for that agent to look dead via
the much slower stale-heartbeat path. The run is failed, never re-queued,
for the same reason the stuck-run reaper doesn't re-queue: re-running
partially-executed steps can duplicate side effects.

**Fix**

This is expected self-healing, not a bug to work around — the run genuinely
needs to be re-triggered, same as any other orphaned run:

- Re-trigger the job; the agent itself is healthy and will claim it normally.
- If this recurs frequently for one agent, suspect network reliability
  between that agent and the controller — a lost claim response is the
  underlying trigger, not agent instability.
- A legacy agent (built before this feature) sends a bodyless heartbeat and
  never participates in this reconcile path; it relies solely on the
  stuck-run reaper above for a lost-claim recovery, which takes longer but
  still eventually fails the run.

## Approve/reject returns 409 `run is already terminal` or `approval window has expired`

**Symptom**

`unified-cli approve <run-id> <step-index>` (or the Approve button in the Web
UI) fails with one of:

```
run is already terminal; approvals are no longer accepted
approval window has expired; the step already timed out
```

**Cause**

The gate is no longer decidable. Either the run finished, failed, or was
cancelled while the gate was pending, or the gate passed its
`timeoutMinutes` deadline (the agent has already failed the step locally; the
approval reaper relabels the row `TimedOut`/`system` within about a minute).

Both checks are enforced inside the statement that writes the decision, so
this is not a transient race you can retry past. It is distinct from
`409 already decided`, which means somebody else decided this gate first.

The Web UI still shows Approve/Reject buttons on a gate step belonging to a
run that has since gone terminal — the step row keeps its `WaitingApproval`
status. The buttons are inert: they produce this 409.

**Fix**

Re-trigger the job. The decision cannot be applied retroactively: a recorded
approval would be a false audit entry, and (when the run was cancelled) could
otherwise let the post-gate step execute after the cancel.

