# Migrating to enforced `runsIn.resources`

`runsIn.resources` (the uses-level scope's CPU/memory bounds) has always
parsed and validated. Until this release it did nothing else: both execution
backends silently dropped it. An `apply` that set
`runsIn.resources.limits.memory: 512Mi` succeeded, and the scope it described
ran with no memory limit at all. This release wires the field through to both
backends and, at the same time, closes the sibling gap of `requests` being
accepted and dropped. That is **two** breaking changes in one release, and
they behave differently enough to need separate treatment.

| Before | After |
|---|---|
| `runsIn.resources.requests` parsed and validated (quantity syntax only); `apply` succeeded. | `runsIn.resources.requests` is a parse error; `apply` fails immediately, naming the field. |
| `runsIn.resources.limits` parsed and validated; `apply` succeeded; neither backend applied it. The scope container/pod ran with no CPU/memory bound. | `runsIn.resources.limits` is applied on both backends: the standard agent maps it to the container runtime's `--cpus`/`--memory`, the Kubernetes agent sets it on the scope pod's `step` container. |

## Breaking change 1: `runsIn.resources.requests` now fails `apply`

A job whose YAML sets `runsIn.resources.requests` on any `uses:` step now
fails to apply, with:

```
runsIn.resources.requests is not supported (the host agent has no request concept, and this scope has no pod to route to Kubernetes); use podTemplate.spec.containers[].resources.requests instead, which is honored on a Kubernetes agent
```

This is the easier of the two changes to reason about: the field was already
inert (no backend read `requests` at all), so nothing that was working stops
working. What changes is that a job carrying it can no longer be applied —
CI pipelines that `apply` on every push, or GitOps controllers that
reconcile from a Git-tracked copy of these job files, will start failing on
any job that has this field, whether or not anyone touches it in this
release.

**Fix:** remove `runsIn.resources.requests`, or move the whole
resource block to `podTemplate.spec.containers[].resources`, which already
supports `requests` and already requires (and gets, via capability routing)
a Kubernetes agent — see [Migrating podTemplate sub-field
routing](podtemplate-subfield-routing.md).

## Breaking change 2: `runsIn.resources.limits` now actually limits

This is the one to read carefully, because it is easy to misdiagnose.

**Nothing about the affected job's YAML needs to change for this to bite.** A
job that has set `runsIn.resources.limits.memory: 512Mi` for weeks, applied
cleanly, and run successfully every time, gets a real memory limit for the
first time the moment the agent claiming it is upgraded to this release. If
that job's actual memory usage is, and always was, above 512Mi — it was just
never being stopped from exceeding it — it now gets killed partway through a
step, typically visible as the container runtime or Kubernetes reporting an
out-of-memory kill (exit code 137 on the standard agent; `OOMKilled` as the
container status reason on the Kubernetes agent) rather than the step's own
command failing.

From the seat of whoever is on call, this looks exactly like a regression:
"a job that was fine yesterday is failing today, and I didn't touch it." It
is worth saying plainly that **this is the correct outcome, not a bug** — the
author of that job asked for a 512Mi limit, and up to this release the
platform was silently not giving it to them. The job was never actually
guaranteed 512Mi; it was one memory spike away from this failure the entire
time, on any host that happened to be tight on RAM. This release makes that
guarantee real, which means a job whose usage was already over the number it
declared surfaces now instead of at some unpredictable future point (or
never, if it happened to keep running on hosts with room to spare). The
platform did not start killing a healthy job — it started enforcing a bound
the job's own author wrote down.

CPU limits fail more gently by comparison — a CPU limit throttles rather than
kills, so a job that was quietly relying on unthrottled CPU degrades to
slower rather than dying outright, which is far less likely to page anyone
but is worth knowing about if a job that runs fine now takes longer after
this release.

### Finding jobs at risk before upgrading

Search job definitions for the field itself, not just `runsIn:`, since a
`runsIn:` block commonly has `image` and other keys ahead of `resources`:

```bash
grep -rn "resources:" <your job definitions>
```

Confirm each hit sits under a `uses:` step's `runsIn:` block (as opposed to a
`podTemplate` container's `resources:`, which is unaffected by this change —
see [Migrating podTemplate sub-field routing](podtemplate-subfield-routing.md)
for that field's own history). Flow-style YAML —
`resources: { limits: { memory: 512Mi } }` — puts the keys you're looking for
on one line; don't anchor the search to the start of a line or to a fixed
number of lines after `runsIn:`.

For each job that matches, you cannot know from the YAML alone whether its
real memory usage is under the declared limit — that requires knowing how
the job actually behaves, which this repository has no record of. Treat
every match as a candidate to check, not a confirmed problem:

- If you have visibility into past runs' actual memory usage (host
  monitoring, `docker stats` history, a cluster metrics stack), compare it
  against the declared `limits.memory` before upgrading.
- If you don't, the safe default is to raise the limit generously before
  upgrading, watch the job's first few runs after, and tighten it once you
  have real numbers — rather than leaving the original number in place and
  finding out it was too low from a failed run.

### What to do about a job that starts failing

There are exactly two ways out, and they are not equivalent — pick based on
whether the job actually needs the memory it's using:

1. **Raise (or remove) the limit.** If the job's real usage is legitimately
   at or above what was declared, `runsIn.resources.limits.memory` was wrong
   from the start; correct it to reflect what the job actually needs, or
   remove the `limits` block entirely to go back to unbounded (the pre-this-
   release behavior, now explicit instead of accidental).
2. **Fix the job.** If the usage is a leak or an avoidable spike rather than
   a real requirement, this is the signal that was previously suppressed —
   use it.

Either way, this does not require an `agentSelector`/capability change:
`runsIn.resources.limits` works on both backends without pinning the job to
a particular agent kind.

## Who is unaffected

A job with no `runsIn.resources` at all is untouched by either change. A job
using only `runsIn.resources.limits` with a value its real usage already
respects sees no behavior change — the limit was always correct, it's just
enforced now. Only `requests` users (breaking change 1) and `limits` users
whose declared number was already too low for actual usage (breaking change
2) are affected.
