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
simply stays `Queued`: the queued-run reaper deliberately leaves a
capability-unschedulable run alone rather than auto-failing it (see [Job
stays Queued / unschedulable
warning](../../troubleshooting/runs-and-scheduling.md#job-stays-queued-unschedulable-warning)),
so nothing *automatic* moves it out of `Queued`. A manual cancel does, and so
does a Kubernetes agent registering later and claiming it — see [the two ways
out](#the-two-ways-out) below.

Both are true at once: the run was silently not getting what it asked for
before, and it now stops scheduling instead. This is a **breaking change**
for any affected job in an environment with no Kubernetes agent.

| Before | After |
|---|---|
| A `podTemplate` container with `resources.requests` (and no other host-unsupported field) was classified host-runnable. | The same container requires the `pod` capability and is pinned to a Kubernetes agent. |
| A `podTemplate` container with an `env` entry using `valueFrom` (and no other host-unsupported field) was classified host-runnable. | The same container requires the `pod` capability and is pinned to a Kubernetes agent. |
| On a standard agent, `resources.requests` was dropped with one WARN log; `resources.limits` still applied. | The run never reaches a standard agent (\*), so `resources.limits` and `resources.requests` are both honoured on the Kubernetes agent that claims it. |
| On a standard agent, an `env` entry's `valueFrom` was dropped with one WARN log per offending entry; the variable was simply absent from the container's environment. | The run never reaches a standard agent (\*), so `valueFrom` resolves normally on the Kubernetes agent that claims it. |
| A run using either field scheduled on any agent reporting `container` capability. | A run using either field requires an agent reporting `pod` capability. Where none is registered, the run simply stays `Queued`. |

(\*) Except mid rolling-upgrade. An agent that has not re-registered since
being upgraded to a binary older than capability routing reports no
`capabilities` at all; `ClaimNextRun`'s capability check is deliberately
skipped for such an agent (see [Capabilities and
routing](../agents.md#capabilities-and-routing)) so the upgrade itself
doesn't strand runs an old agent could otherwise still run correctly. Until
that agent restarts and re-registers on the new binary, it can still claim
and silently degrade one of these runs exactly as before this change.

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
grep -rn -E "(requests|valueFrom):" <your job definitions>
```

Do not anchor this to the start of the line (no `^[[:space:]]*`). Flow-style
YAML — `resources: {requests: {cpu: 500m}}`, or an inline `env` entry like
`{name: TOKEN, valueFrom: {secretKeyRef: {...}}}` — puts the key you're
looking for after other text on the same line, which an anchored pattern
misses outright. This project's own docs use exactly that flow style (see
`valueFrom: { secretKeyRef: ... }` in the [HA
guide](../high-availability.md)), so treat it as something your job authors
plausibly do too. The unanchored pattern does not pick up `storageRequest:`
(`workspace.pvc.storageRequest` is a different field, and the substring
doesn't match — `requests:` needs the trailing `s`), so it stays clean
against this repo's own `examples/` and `docs/`.

Any match worth confirming as under a `podTemplate:` is a job whose podTemplate
now requires the `pod` capability where it previously did not. Confirm you
have a Kubernetes agent registered before upgrading, or plan one of the two
ways out below.

A job whose `spec` already carries `agentSelector` (a top-level `spec` field,
not a `podTemplate` field) pinning it to a Kubernetes agent (e.g.
`kind:kubernetes`) is unaffected either way — it was already routed there.

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

## A stuck run holds its concurrency locks

If the affected run has a `concurrency` block (`mutex` or a named-lock
`semaphores` pool), staying `Queued` under this change is not inert.
`tryQueueRun` (`internal/store/postgres.go`) acquires the run's mutex/
named-lock-slot rows in the same transaction that marks it `Queued`, and only
`MarkRunFinished` (`internal/store/postgres.go`) releases them — on success,
failure, *or* cancellation, since it isn't gated on which terminal status it
transitions to.

A run that stays `Queued` forever therefore holds those locks forever too.
Every other run sharing that mutex or named-lock pool queues up behind it —
in `Pending`, not `Queued`, because they're blocked on the lock rather than
on agent capability. `Pending` shows no unschedulable banner, and there is no
troubleshooting entry keyed to a stuck `Pending` run, so from the outside it
looks like an unrelated job simply stopped firing, with nothing to search
for.

This failure mode isn't new — a `native` job with no matching agents online
hits it too — but this change enlarges the population of runs that can
trigger it, which is what this guide is for.

**Recourse:** cancel the stuck run (`unified-cli run cancel <run-id>`).
Cancelling works from any non-terminal state, including `Queued`, and
releases both the mutex and the named-lock slot as part of the same
transaction that marks the run `Cancelled` — freeing every sibling run that
was waiting behind it.

## The two ways out

There are exactly two:

1. **Run a Kubernetes agent.** Register a k8s-agent so a `pod`-capable agent
   is available to claim the run — see the [Kubernetes Integration
   Guide](../kubernetes-integration.md). The full `podTemplate`, including
   `resources.requests` and `valueFrom`, is then honoured as originally
   written.

2. **Remove the part the host cannot honour.** Use `resources.limits` instead
   of `resources.requests`, or a literal `value` instead of `valueFrom`, so
   the job can still run on a standard agent. If you go this route, keep
   `resources.limits` to `cpu`/`memory` spelled as YAML strings: the host
   maps only those two keys and silently drops anything else in `limits`
   (an extended resource, `resources.claims`, or a bare numeric value) —
   which as of this same change also now requires a Kubernetes agent rather
   than being dropped.

   Removing it means the job never actually had that guarantee in the first
   place — the request or the injected value was being silently dropped
   before this change. Choosing this path is choosing to keep running without
   something the job was already not getting.
