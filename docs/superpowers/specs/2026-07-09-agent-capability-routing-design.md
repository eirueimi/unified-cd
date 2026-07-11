# Agent capability routing and unschedulable-job warnings

- Date: 2026-07-09
- Status: Design approved (implementation plan pending)
- Related: [2026-07-08-job-isolation-design.md](2026-07-08-job-isolation-design.md)
  (native/isolated/claim-pod; this design fixes how those jobs are ROUTED)

## Background & motivation

After job-isolation shipped, running the example jobs on a mixed fleet (one
standard/Docker agent + one Kubernetes agent) surfaced two routing problems:

1. **Native jobs are silently claimed by the k8s agent and rejected.** A
   `native: true` job with no `agentSelector` can be claimed by either agent.
   When the k8s agent claims it, it fails fast with "native: true jobs are
   host-only" — a job that should have run on the standard agent instead just
   fails. The author has to hand-write an `agentSelector` that excludes k8s,
   but the positive-only label match (`agent must have ALL selector labels`)
   cannot express "not a k8s agent."

2. **podTemplate jobs were force-pinned to Kubernetes** (host claim pod
   unreachable). This half was already fixed in parallel by commit `8ca1567`
   ("only pin podTemplate runs to k8s when host can't run them"), which added
   `dsl.PodTemplateNeedsKubernetes(pt)` + `dsl.HostSupportedContainerFields`
   and made the `kubernetes`-label pin conditional. This design REUSES that
   predicate and folds it into a single capability model (below); it does not
   redefine it. The still-open half is the native case:
   `native: true` jobs are not covered by `8ca1567` and are still silently
   claimed and rejected by the k8s agent.

**Goal:** the controller infers what kind of agent a job needs from its spec
and routes it to a matching agent automatically — no hand-written selector,
no silent native-rejection, and host-runnable podTemplates reach the host
claim pod. When NO registered agent can satisfy a job's needs, surface a
clear "unschedulable" warning in the WebUI instead of leaving it silently
Queued forever.

The user chose **capability routing + unschedulable warning** over passive
warnings-only and over true runtime fallback (claim → detect → requeue),
which is racy and costly.

## Scope and non-goals

- **In scope:** an agent `capabilities` field (native/container/pod);
  trigger-time requirement inference; a `capabilities ⊇ requiredCaps` claim
  filter; a schedulability evaluation + WebUI warning; replacing the blanket
  podTemplate→kubernetes pin.
- **Out of scope (YAGNI):** a general YAML linter / best-practice advisories
  (set -e, unused outputs, deprecations); true runtime fallback; user-defined
  capabilities beyond the fixed three; auto-scaling / provisioning agents to
  satisfy an unschedulable job.

## Confirmed decisions

1. **Capabilities are a new typed field** on agent registration
   (`capabilities []string`), distinct from free-form `labels` (labels =
   author/topology; capabilities = machine-determined execution ability).
2. **Fixed vocabulary of three:** `native` (run a step as a host process),
   `container` (run a step in an isolated container), `pod` (build a
   Kubernetes Pod).
3. **Agents self-advertise:** the standard agent always reports `native`, and
   adds `container` when a container runtime is detected; the k8s agent
   reports `["pod","container"]`.
4. **Requirement inference replaces the blanket podTemplate pin** at trigger
   time; a run carries `requiredCaps`.
5. **Match is `agent.capabilities ⊇ run.requiredCaps`**, ANDed with the
   existing author `agentSelector` label match. Conflicts need no special
   handling — they reduce to "no agent satisfies it" and are reported by the
   warning.
6. **Legacy agents (no capabilities reported) skip the cap check** and match
   by labels only, so a rolling upgrade does not strand runs.
7. **Warning fires only when genuinely unschedulable** (requiredCaps + author
   labels satisfiable by no currently-registered agent).

## A. Capability model (agent side)

- `api.AgentRegisterRequest` gains `Capabilities []string` (`json:"capabilities,omitempty"`).
  `api.AgentInfo` gains the same for read-back/UI.
- Store: add a `capabilities text[]` column to the agents table (nullable;
  NULL = legacy agent). `UpsertAgent` / `UpsertAgentOnClaim` persist it.
  Claim-time upsert must not clobber a previously-registered non-null
  capabilities set with an empty one (mirror the existing hostname/OS
  clobber-guard in `handleAgentClaim`).
- Agent side:
  - `internal/agent` (standard agent): compute at registration —
    `caps := []string{"native"}`; if `a.containerRuntime()` succeeds, append
    `"container"`. (Detection already exists; call it once at startup.)
  - `internal/k8sagent`: report `[]string{"pod","container"}`.
- Validation: the controller rejects unknown capability strings at
  registration (only native/container/pod allowed) with a 400, so a typo
  can't create an agent that silently matches nothing.

## B. Requirement inference (controller, trigger time)

New pure function in `internal/dsl` (or a small controller helper):

```
requiredCaps(spec dsl.Spec) []string
  spec.Native == true                    -> ["native"]
  spec.PodTemplate == nil                -> ["container"]   // isolated default
  spec.PodTemplate != nil && needsK8s    -> ["pod"]
  spec.PodTemplate != nil && !needsK8s   -> ["container"]
```

The `needsK8s` branch **reuses the existing `dsl.PodTemplateNeedsKubernetes(pt)`**
(added in `8ca1567`, backed by `dsl.HostSupportedContainerFields`) — this
design does NOT reimplement it. That predicate already returns true for a
named agent-template / `Override` patch, `Reuse`, a pod-spec key beyond
`containers`, or a container field outside `HostSupportedContainerFields`
(and treats `workspace.pvc`/`mountPath` as host-OK).

Wire-through: `api.ClaimResponse` already carries what the agent needs; the
`requiredCaps` live on the RUN row for claim matching, not in ClaimResponse.
`CreateRun` gains a `requiredCaps []string` parameter (persisted on the run);
`handleTriggerRun` and the call-step child-run path both compute it via
`requiredCaps(spec)` and pass it in.

**Relation to `8ca1567`:** today `api_runs.go` expresses the k8s-only case by
appending the `kubernetes` *label*
(`if dsl.PodTemplateNeedsKubernetes(spec.PodTemplate) { appendLabelIfMissing(..., "kubernetes") }`).
This design supersedes that label append with the `pod` *capability*
(`needsK8s → requiredCaps ["pod"]`), so routing goes through one mechanism
(capabilities) instead of two. The k8s agent already advertises `pod`, so the
behavior is equivalent; removing the label append also stops it from
polluting the author-visible selector. Keep `PodTemplateNeedsKubernetes`
itself unchanged — only its consumer in `api_runs.go` moves from label to cap.

## C. Matching (claim time)

- Store: add `required_caps text[]` to the runs table (persisted by
  `CreateRun`).
- `ClaimNextRun` reads the claiming agent's capabilities from the agents
  table (join on `agent_id`), so the claim payload does not need a new field
  and there is one source of truth (the row upserted at register/claim). The
  claim SQL's WHERE adds, alongside the existing
  `agent_labels @> run.agent_selector` label check:
  `(agent.capabilities IS NULL  OR  agent.capabilities @> run.required_caps)`
  i.e. legacy agents (null caps) skip the check; capability-reporting agents
  must be a superset of the run's requiredCaps. An empty/null
  `required_caps` on a run matches everything (older runs created before this
  change, and any future run with no inferred requirement).
- The claim upsert (`UpsertAgentOnClaim`) still records labels as today; it
  must not overwrite a registered capabilities set — so the claim path either
  omits capabilities (leaving the registered value) or applies the same
  clobber guard used for hostname/OS.

## D. Schedulability evaluation & WebUI warning

- Helper `EvaluateSchedulability(spec dsl.Spec, agents []api.AgentInfo)
  -> {RequiredCaps []string, Selector []string, Satisfiable bool, Reason string}`.
  Satisfiable = at least one agent in `agents` whose labels ⊇ the job's
  (expanded, param-free where possible) agentSelector AND whose capabilities
  ⊇ requiredCaps (legacy null-caps agents count as satisfying the cap part,
  matching the claim rule). Reason names the missing piece ("no registered
  agent provides capability `native`" / "no agent matches labels [kind:macos]").
- API: `GET /api/v1/jobs/{name}/schedulability` returns that struct (computed
  against the current agent inventory). Alternatively fold the fields into the
  existing job-detail payload; a separate endpoint keeps the job read cheap
  and the evaluation on-demand.
- WebUI `JobDetail.svelte`: fetch schedulability on load; when
  `!Satisfiable`, render a warning banner near the top:
  "⚠ This job requires capability `<cap>` but no registered agent provides it
  (selector: <labels>). Runs will stay Queued." Nothing is shown when the job
  is schedulable.
- Note (agentSelector param templating): a selector that only resolves at
  trigger time (`hostname:{{ .Params.agent }}`) cannot be fully evaluated on
  the job view. Evaluate the capability part always; for label parts
  containing `{{`, skip the label check and note "selector depends on
  run parameters" rather than warning falsely.

## Error handling

| Situation | Behavior |
|---|---|
| Agent registers with an unknown capability string | 400 at registration; agent not recorded with bad caps |
| Rolling upgrade: old agent with no capabilities | Treated as legacy — cap check skipped, matches by labels (today's behavior) |
| Claim upsert would overwrite known caps with empty | Guard preserves the previously-registered set (like hostname/OS today) |
| Job needs `native` but only k8s agents online | Run stays Queued; JobDetail shows the unschedulable warning |
| podTemplate with a PVC on a fleet with no k8s agent | requiredCaps `pod`; unschedulable warning |
| Author agentSelector contradicts the inferred cap | No special case: reduces to unschedulable → warning |
| Run created before this change (null required_caps) | Matches any agent (cap check is a no-op for null requirement) |

## Testing

- **Unit — inference:** `requiredCaps(spec)` for native / isolated /
  host-podTemplate / k8s-podTemplate; `podTemplateNeedsK8s` for each trigger
  (PVC, initContainers, named template, container command, env.valueFrom, ...)
  and for the host-runnable subset (name/image/env-literal/limits → false).
- **Unit — matching:** superset semantics; legacy null-caps agent skips the
  check; null required_caps run matches anyone; the label AND cap AND compose.
- **Unit — schedulability:** satisfiable / unsatisfiable-by-cap /
  unsatisfiable-by-label / param-templated-selector cases; reason strings.
- **Store:** capabilities/required_caps round-trip; claim-upsert clobber guard;
  `ClaimNextRun` picks a cap-matching agent and skips a non-matching one.
- **Controller:** `handleTriggerRun` persists inferred requiredCaps; the
  blanket podTemplate→kubernetes pin is gone; `/schedulability` endpoint.
- **Integration (docker+k8s or fakes):** a native job is claimed by the
  standard agent and never by the k8s agent (regression for the observed
  native-rejection); a host-runnable podTemplate job is claimable by the
  standard agent (regression for the blanket-pin bug).
- **Web:** JobDetail shows the banner when `/schedulability` returns
  unsatisfiable, hides it otherwise.
- **Regression:** existing parity/claim tests; legacy agents (no caps) still
  claim label-matched runs.

## Implementation order (rough)

1. `internal/dsl`: `requiredCaps(spec)` reusing the existing
   `PodTemplateNeedsKubernetes` (+ tests). No new podTemplate predicate.
2. api types + store: `capabilities` on agents, `required_caps` on runs
   (migration), `CreateRun`/`ClaimNextRun`/upsert changes (+ tests).
3. Agents: standard + k8s advertise capabilities at registration.
4. Controller: infer + persist requiredCaps at trigger and in the call-step
   child path; replace `8ca1567`'s `kubernetes`-label append with
   `requiredCaps ["pod"]`; capability validation on registration.
5. `EvaluateSchedulability` + `/api/v1/jobs/{name}/schedulability`.
6. WebUI JobDetail warning banner.
7. Docs: agents.md (capabilities, how routing is inferred), jobs.md
   (no need to hand-write agentSelector for native/podTemplate),
   troubleshooting.md (unschedulable warning), migration note (the
   podTemplate→kubernetes auto-pin is replaced by capability routing).
