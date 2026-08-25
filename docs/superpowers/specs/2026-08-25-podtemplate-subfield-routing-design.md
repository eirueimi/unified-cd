# Sub-field granularity in podTemplate routing — Design

Date: 2026-08-25
Status: Approved (design); implementation plan to follow

## 1. Purpose

A job whose `podTemplate` declares `resources.requests`, or an `env` entry with
`valueFrom` instead of a literal `value`, is routed to a standard agent, where
that part of the template is dropped with a warning and the job runs anyway.
The author asked for a resource guarantee, or for an injected environment
variable, and silently did not get one.

This is not the documented trade-off. The documentation presents both as
deliberate host limitations — docker and podman have no request concept, and
the host cannot resolve a `valueFrom` reference. Both are true of the *host
backend*. What is not intended is that such a job reaches a host agent at all.

## 2. The defect

The controller already infers which agent capability a run needs.
`dsl.RequiredCaps` (`internal/dsl/capabilities.go:24-32`) returns `pod` for a
podTemplate the host cannot honour, and the agent-side capability match keeps
the run away from standard agents. The predicate behind it is
`PodTemplateNeedsKubernetes` (`internal/dsl/podtemplate.go:27-60`), which walks
each container's fields and returns true for any field outside
`HostSupportedContainerFields` (`internal/dsl/podtemplate.go:13-16`):

```go
var HostSupportedContainerFields = map[string]bool{
	"name": true, "image": true, "env": true, "resources": true,
	"command": true, "args": true,
}
```

The predicate compares field *names*. It cannot see that the host honours
`resources.limits` but drops `resources.requests`, or that it honours a literal
`env` value but drops `valueFrom`. Both fields are marked supported, so a
container carrying only the unsupported half is classified host-runnable,
`RequiredCaps` yields `container`, and the run is free to land on a standard
agent — where it is degraded:

```
podTemplate container resources.requests is not supported on the host agent
(docker/podman have no request concept) and is ignored; use resources.limits
or route to a Kubernetes agent
```
(`internal/agent/claim_pod.go:92-96`)

```
podTemplate container env without a literal value is ignored on the host agent
```
(`internal/agent/claim_pod.go:71-73`)

The first warning names the remedy — route to a Kubernetes agent — at a point
where routing has already been decided.

**The granularity is the bug.** The field-level set is the right shape for every
other field. `resources` and `env` are the two whose sub-keys straddle the
backend boundary, and the predicate has no way to express that.

The `env` case is the more dangerous of the two. A dropped `resources.requests`
costs a scheduling guarantee; a dropped `valueFrom` referencing a
`secretKeyRef` removes a credential from the container's environment, and what
the step then does depends entirely on how it handles an unset variable.

## 3. The change

`PodTemplateNeedsKubernetes` gains two sub-field rules:

- A container whose `resources` map contains a non-empty `requests` forces `pod`.
- A container with an `env` entry that has no literal `value` key forces `pod`.

The second rule must match the host builder's own test exactly. The host treats
"no `value` key present" as unresolvable (`claim_pod.go:69`, `rawVal, present := e["value"]`),
which is not the same as an empty-string value — `value: ""` is a literal empty
string the host honours. The predicate must split the same way, or the two will
disagree about a template and the warning becomes reachable again for a case the
predicate thought it had routed away.

`HostSupportedContainerFields` keeps both entries. Removing them would be wrong:
the host really does map `resources.limits` (`claim_pod.go:86-91`) and literal
`env` values, and dropping the fields from the set would push jobs onto
Kubernetes agents they do not need. The set stays the field-level source of
truth; the two sub-field rules sit beside it as explicit, commented exceptions,
with a note that they exist because these fields' sub-keys split across the
boundary.

The host-side warnings stay as defence in depth. They become unreachable through
the normal path, and their comments should say so — a warning that cannot fire
otherwise looks like dead code to the next reader, and someone will delete it.

## 4. What an operator experiences

Before: the run schedules on a standard agent and executes without the requested
resources, or without the injected environment variable.

After: the run requires the `pod` capability. Where a Kubernetes agent exists, it
schedules there and the template is honoured — the outcome the author asked for.
Where one does not, the run leaves `Pending` for `Queued` as normal and then simply
stays `Queued`. It is never auto-failed: `ListUnclaimableQueuedRuns`
(`internal/store/postgres.go:1402-1412`) is label-only by design and
deliberately omits the capability clause `ClaimNextRun` itself ANDs in, so a
run that is label-claimable but capability-unschedulable is intentionally left
`Queued` rather than terminalized by the queued-run reaper
(`internal/controller/queuedrun_reaper.go:27-37`). The job's page instead shows
the existing, documented unschedulable banner
(`docs/troubleshooting/runs-and-scheduling.md`, "Job stays Queued /
unschedulable warning").

**This is a breaking change**, and the failure is the good kind: a job that was
silently not getting what it asked for now says so. But it turns a running job
into a permanently `Queued` one, which an operator experiences as a regression
until they read why.

## 5. Migration

Per `AGENTS.md`'s breaking-change rule, a guide goes under
`docs/operator-manual/migrations/`, following the shape of the existing
ID-scoped credential guide: what changed, a before/after table, the exact
symptom string, and the ways out.

The ways out are the whole content, and there are only two per case: run a
Kubernetes agent, or remove the part the host cannot honour — use
`resources.limits` instead of `requests`, or a literal `value` instead of
`valueFrom`. The guide should not invent a third.

It must also say how to find affected jobs *before* upgrading, since the symptom
appears only after: search job definitions for `requests:` and for `valueFrom:`
beneath a `podTemplate`. A job that already pins itself to a Kubernetes agent
with `agentSelector` is unaffected either way.

## 6. Verification

1. `dsl` tests cover the predicate at sub-field granularity:
   - `resources.requests` present yields `pod`; `resources.limits` only yields
     `container`; both yield `pod`; an empty `requests` map yields `container`,
     because an empty map requests nothing.
   - an `env` entry without a `value` key yields `pod`; `value: "x"` yields
     `container`; `value: ""` yields `container`, because an empty string is a
     literal the host honours; a container mixing a literal entry and a
     `valueFrom` entry yields `pod`.
2. The existing tests for `PodTemplateNeedsKubernetes` pass unchanged — no field
   outside `resources` and `env` changes classification.
3. `go build ./...` and `go test ./... -short -count=1` pass.
4. `docs/operator-manual/kubernetes-integration.md`'s intentional-differences
   entries for requests and for host-unsupported fields become routing
   statements rather than "ignored with a WARN".

## 7. Out of scope

- **Teaching the host agent to honour requests or resolve `valueFrom`.** Docker
  and podman have no request concept, and a `valueFrom` reference is resolvable
  only against a Kubernetes API. These are capability boundaries, not
  implementation gaps.
- **Rejecting these templates at apply time.** Considered: it fails earlier and
  more loudly. Rejected because such a job is valid — it simply needs a
  Kubernetes agent — and apply-time rejection would refuse a correct job on a
  cluster that has one.
- **Auditing the remaining fields in `HostSupportedContainerFields`.**
  `name`, `image`, `command`, and `args` have no sub-keys that split across the
  boundary; `command`/`args` reached parity in an earlier change and their truth
  table is documented in `kubernetes-integration.md`.

## 8. How this was found, and what it implies

The `resources.requests` case was found by reading the routing predicate during
a survey of intentional host/Kubernetes differences. The `env` case was found
while writing this document, by checking a claim made in an earlier draft's
out-of-scope section — the draft asserted that non-literal `env` already forced
`pod` through another path, and it does not.

Two instances of the same defect, one of them found only because a sentence
about it was written down and then checked, is weak evidence that the field-level
set is the wrong abstraction rather than merely incomplete. It is not strong
enough to justify redesigning it here. It is strong enough to justify the
comment the change adds: any future addition to `HostSupportedContainerFields`
must state whether the host honours the whole field or only part of it.
