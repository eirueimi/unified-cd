# Migration: agent capability routing

This release adds a typed `capabilities` list (`native`/`container`/`pod`)
to agent registration and uses it, instead of a label, to route runs to an
agent that can actually execute them. See [Capabilities and
routing](agents.md#capabilities-and-routing) in the Agent guide for the full
model, and [Job stays Queued / unschedulable
warning](troubleshooting.md#job-stays-queued--unschedulable-warning) for the
new Web UI warning this enables.

## What changed

Commit `8ca1567` ("only pin podTemplate runs to k8s when host can't run
them") made the controller append a `kubernetes` *label* to a run's
`agentSelector` whenever `dsl.PodTemplateNeedsKubernetes(podTemplate)` was
true, so the run could only be claimed by an agent carrying that label. This
release **replaces that label append with a capability requirement**: the
same `PodTemplateNeedsKubernetes` check now yields `requiredCaps: ["pod"]`
on the run instead of mutating its `agentSelector`, and the claim query
matches it against the claiming agent's advertised `capabilities` rather
than its labels.

The same capability mechanism also closes a gap the label pin never
covered: a `native: true` job now gets `requiredCaps: ["native"]` at trigger
time, so it is routed to a host agent automatically instead of being
claimable (and then rejected) by a Kubernetes agent.

| Before | After |
|---|---|
| A `podTemplate` job needing Kubernetes got a `kubernetes` label silently appended to its `agentSelector`, visible to authors inspecting the run and matched against agent **labels**. | The same job gets `requiredCaps: ["pod"]` on the run, matched against agent **capabilities**. `agentSelector` is left exactly as the author wrote it — no synthetic label. |
| A `native: true` job with no `agentSelector` could be claimed by a Kubernetes agent and would then fail immediately ("native: true jobs are host-only"). | The job gets `requiredCaps: ["native"]`; only a `native`-capable (standard) agent can claim it in the first place. |
| No signal when a job could never be scheduled — it just sat `Queued`. | `GET /api/v1/jobs/{name}/schedulability` and a Web UI banner report it explicitly — see [troubleshooting](troubleshooting.md#job-stays-queued--unschedulable-warning). |

## Do I need to change my job YAML?

No. `requiredCaps` is inferred from the same spec fields (`native`,
`podTemplate`) that already drove the old label pin — nothing new to
declare, and any hand-written `agentSelector` you already have keeps working
unchanged (it's still ANDed with the capability check).

## Do I need to upgrade my agents?

Yes, to get the benefit — but it is not a hard cutover.

- **Upgrade both the standard agent and the k8s-agent binaries.** A
  standard agent reports `["native"]` (or `["native","container"]` with a
  runtime detected) on its own at registration; a k8s-agent reports
  `["pod","container"]`. No configuration is required — capabilities are
  self-detected, not something you set with a flag.
- **A pre-upgrade agent reports no `capabilities` at all** (the column is
  `NULL` for it in the `agents` table). Such a **legacy agent skips the
  capability check entirely** and continues to match runs by
  `agentSelector` labels only — exactly like every agent did before this
  release. This means a rolling upgrade is safe: while some agents in your
  fleet are upgraded and some aren't, a run either matches a legacy agent's
  labels (as before) or a capability-reporting agent's capabilities+labels
  (the new, stricter check) — nothing gets stranded mid-rollout.
- Once **all** agents that should route by capability are upgraded, the
  effective behavior is: `podTemplate` jobs needing Kubernetes only reach a
  k8s-agent, `native: true` jobs only reach a standard agent, and
  host-runnable `podTemplate` jobs can reach either — without any
  `kubernetes` label ever being synthesized into `agentSelector`.

## What if I relied on the synthesized `kubernetes` label?

If you had written your own `agentSelector` logic that depended on the
controller's auto-appended `kubernetes` label being present on a
`podTemplate` run (e.g. inspecting run selectors, or a custom tool matching
on it), that label is no longer appended. Depend on `requiredCaps` on the
run (or the job's `/schedulability` capability report) instead — see
[Capabilities and routing](agents.md#capabilities-and-routing).
