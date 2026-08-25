# Migrating podTemplate sub-field routing

The controller's routing predicate now looks inside two `podTemplate` container
fields instead of only checking their names. A `podTemplate` declaring
`resources.requests`, or an `env` entry that sets `valueFrom` instead of a
literal `value`, now requires a Kubernetes agent — the same as any other
Kubernetes-only field.

**Before**, a container carrying only the unsupported half of `resources` or
`env` was classified host-runnable by field name alone. It scheduled on a
standard agent, which mapped `resources.limits` but silently dropped
`resources.requests`, and honoured a literal `env` value but silently dropped
`valueFrom` — the job ran, but without the resource guarantee or the injected
value its author asked for.

**After**, such a job requires the `pod` capability and is pinned to a
Kubernetes agent, where the full template is honoured. Where no Kubernetes
agent is registered, the run leaves `Pending` for `Queued` as normal and then
simply stays `Queued` — nothing later transitions it to any other state.

Both are true at once: the run was silently not getting what it asked for
before, and it now stops scheduling instead. This is a **breaking change**
for any affected job in an environment with no Kubernetes agent.

| Before | After |
|---|---|
| A `podTemplate` container with `resources.requests` (and no other host-unsupported field) was classified host-runnable. | The same container requires the `pod` capability and is pinned to a Kubernetes agent. |
| A `podTemplate` container with an `env` entry using `valueFrom` (and no other host-unsupported field) was classified host-runnable. | The same container requires the `pod` capability and is pinned to a Kubernetes agent. |
| On a standard agent, `resources.requests` was dropped with one WARN log; `resources.limits` still applied. | The run never reaches a standard agent, so `resources.limits` and `resources.requests` are both honoured on the Kubernetes agent that claims it. |
| On a standard agent, an `env` entry's `valueFrom` was dropped with one WARN log; the variable was simply absent from the container's environment. | The run never reaches a standard agent, so `valueFrom` resolves normally on the Kubernetes agent that claims it. |
| A run using either field scheduled on any agent reporting `container` capability. | A run using either field requires an agent reporting `pod` capability. Where none is registered, the run simply stays `Queued`. |

## Who is affected, and how to find out before upgrading

The symptom below appears only *after* a run is stuck — by then you're
debugging, not planning. Search your job definitions first. Matching on
`podTemplate:` and a fixed number of lines after it is not reliable: a
container commonly lists `name`, `image`, `command`, `args`, and `env` ahead
of `resources`, which can push the `requests:` key well past a short
line-count window and produce a clean, wrong "not affected" result on a file
that *is* affected. Search for the keys themselves instead, and confirm each
hit sits under a `podTemplate:` container's `resources:` or `env:` block —
a handful of false positives to dismiss by eye beats a false negative you
never see:

```bash
grep -rn -E "^[[:space:]]*(requests|valueFrom):" <your job definitions>
```

Any match worth confirming as under a `podTemplate:` is a job whose podTemplate
now requires the `pod` capability where it previously did not. Confirm you
have a Kubernetes agent registered before upgrading, or plan one of the two
ways out below.

A job whose `podTemplate` already carries `agentSelector` pinning it to a
Kubernetes agent (e.g. `kind:kubernetes`) is unaffected either way — it was
already routed there.

## The symptom

A run that used to schedule on a standard agent instead sits `Queued` —
permanently, not just for a while. The job's page in the Web UI shows a
warning banner:

```
⚠ This job can't be scheduled right now: no registered agent provides
capability [pod]. Runs will stay Queued until a matching agent registers.
```

See [Runs and Scheduling: Job stays Queued / unschedulable
warning](../../troubleshooting/runs-and-scheduling.md#job-stays-queued-unschedulable-warning)
for the full cause and fix.

## The two ways out

There are exactly two:

1. **Run a Kubernetes agent.** Register a k8s-agent so a `pod`-capable agent
   is available to claim the run — see the [Kubernetes Integration
   Guide](../kubernetes-integration.md). The full `podTemplate`, including
   `resources.requests` and `valueFrom`, is then honoured as originally
   written.

2. **Remove the part the host cannot honour.** Use `resources.limits` instead
   of `resources.requests`, or a literal `value` instead of `valueFrom`, so
   the job can still run on a standard agent.

   Removing it means the job never actually had that guarantee in the first
   place — the request or the injected value was being silently dropped
   before this change. Choosing this path is choosing to keep running without
   something the job was already not getting.
