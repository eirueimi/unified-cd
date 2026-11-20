# Migrating: host agent claim-pod behaviour

The host agent's claim pod (the pause, sidecar and job containers of an isolated job) now follows the
same lifecycle rules as the Kubernetes agent's pod. Several behaviours change for host agents; the
Kubernetes agent is not affected.

| | Before | Now |
|---|---|---|
| Image pull | An image already on the host was used as it was, including `:latest` and untagged ones. A pull failed the run only when the image was missing. | Every image of the claim pod (job, sidecar, runner and pause) whose reference has **no digest and either no tag or the tag `latest`** is pulled **once per claim**, even when it is present locally. Other references are pulled only when missing. |
| Pull failure | The container runtime's own error, from `docker run` | `claim pod image pull failed: pull image "<image>": <runtime error>`, or `claim pod image pull failed: image "<image>" is not present after pull` |
| Pull duration | Unbounded | Bounded by `podPullTimeout` (default `30m`): `claim pod image pull exceeded podPullTimeout (30m0s)` |
| Start duration | Unbounded | Bounded by `podStartTimeout` (default `5m`): `claim pod start exceeded podStartTimeout (5m0s)` |
| `podTemplate.reuse: true` | Ignored: the claim pod was removed when the run ended | Honoured: the pod is kept idle and handed to the next run of the same job on the same `maxConcurrent` slot |
| Container labels | None | Every claim-pod and scope container carries `unified-cd/agent-id`, and `unified-cd/claim-pod` or `unified-cd/scope` |
| Containers left by a crash | Stayed on the host until removed by hand | Removed by a startup sweep, for containers carrying the labels |
| A run cancelled while its images are pulling or its pod is starting | The run was not interrupted until the pod was up | Interrupted: the agent removes the partial pod and sends no `FinishRun` (the controller already holds the final status) |

## Images are pulled on every claim when they are mutable

`:latest` and untagged references are treated as mutable, so the agent pulls them at the start of
every claim. This applies to the job container, to sidecars, and to a `runnerImage` or `pauseImage`
you set to such a reference. The default pause image is pinned by digest and is never re-pulled;
the case that most often bites is the runner `:latest` of a locally built agent with a local runner
image. It is intended: a mutable tag should resolve to what the registry has now.

You see the effect in three places:

- **Local-only images.** An image that exists only on the host (built or loaded with `docker build`
  or `docker load`, never pushed) and is referenced as `myapp` or `myapp:latest` can no longer be
  used: the pull fails and the run ends with
  `claim pod image pull failed: pull image "myapp:latest": ...`.
- **Air-gapped hosts, or a registry that is down.** The same failure, for every `:latest` or
  untagged image, even one already on disk.
- **Registry rate limits.** Sidecars are often written without a tag (`redis`, `postgres`). Each
  claim now makes one pull request per such image, which counts against Docker Hub's anonymous
  and free-account limits.

A run that cannot pull fails before any step starts. The message is in the run log (step -1), and
a pull that does not finish within `podPullTimeout` fails with
`claim pod image pull exceeded podPullTimeout (30m0s)`.

**What to do.** For each image that fails or that you want to stop re-pulling:

1. Use a tag other than `latest` (`redis:7.4`, `myapp:2026-11-20`). An image with such a tag is
   pulled only when it is not on the host.
2. Or pin it by digest (`redis@sha256:<digest>`), which also makes the run reproducible.
3. For an image that is only on the host, push it to a registry the agent can reach, or to a mirror,
   and reference it there with a non-`latest` tag.
4. On a rate-limited registry, authenticate the container runtime (`docker login`), point it at a
   mirror, or use a tag so it is pulled once and kept.

If the pull is only slow, raise `podPullTimeout`.

## `podTemplate.reuse: true` is honoured

A job with `podTemplate.reuse: true` used to get a fresh claim pod on every run on a host agent. It
now keeps the pod after the run (up to `poolMaxIdle` idle pods, each removed after
`poolIdleTimeout`) and gives it to the next run of the **same job on the same slot** whose pod
definition is unchanged. Only the workspace is emptied between runs.

What persists between runs of that job on the slot: everything in the containers outside the
workspace. That includes the files in `$HOME`, so **credentials a step writes there**
(`docker login`, a git credential store, a cloud CLI profile) are visible to the next run, whoever
triggered it, as are `/tmp`, installed tools, background processes and the state of sidecars. Step
environment variables and secrets are passed per step and do not persist.

If you set `reuse: true` for the Kubernetes agent and did not expect it on host agents:

- To opt a job out, set `reuse: false` (or remove the field) and re-apply the job.
- To turn pooling off for a host agent, set `--pool-max-idle 0` (`poolMaxIdle: 0`,
  `UNIFIED_AGENT_POOL_MAX_IDLE=0`).
- Have steps remove credentials they create, or do not enable `reuse` on jobs whose runs should not
  see each other's container state.

`reuse` together with `spec.volumes` fails the run, as on the Kubernetes agent:
`spec.volumes cannot be combined with podTemplate.reuse: ...`. Full behaviour is in
[Claim-pod pool](../agents.md#claim-pod-pool-podtemplatereuse).

## New labels and the startup sweep

Every claim-pod and scope container the agent creates now carries `unified-cd/agent-id=<agent ID>`
plus `unified-cd/claim-pod=true` or `unified-cd/scope=true`. When the agent starts, before it takes
any claim, it removes the labelled containers of its own ID that a previous process left behind, and
logs `removed orphaned containers from a previous agent process`.

Two consequences:

- **Containers from before the upgrade carry no labels and are never swept.** Remove them once by
  hand after upgrading. See
  [Crash-orphaned claim containers](../agents.md#crash-orphaned-claim-containers) for the commands.
- **Each agent process needs its own agent ID, and only one process may run per ID.** Two live
  processes sharing an ID delete each other's running claim containers at startup.

## New settings

| Flag | Config key | Env | Default |
|---|---|---|---|
| `--pod-pull-timeout` | `podPullTimeout` | `UNIFIED_AGENT_POD_PULL_TIMEOUT` | `30m` (non-positive or unset: default) |
| `--pod-start-timeout` | `podStartTimeout` | `UNIFIED_AGENT_POD_START_TIMEOUT` | `5m` (non-positive or unset: default) |
| `--pool-idle-timeout` | `poolIdleTimeout` | `UNIFIED_AGENT_POOL_IDLE_TIMEOUT` | `30m`; `0` never evicts by age |
| `--pool-max-idle` | `poolMaxIdle` | `UNIFIED_AGENT_POOL_MAX_IDLE` | `4`; `0` disables pooling |

A negative `poolIdleTimeout` or `poolMaxIdle` is refused at startup.

See also [Agents and Enrollment troubleshooting](../../troubleshooting/agents-and-enrollment.md).
