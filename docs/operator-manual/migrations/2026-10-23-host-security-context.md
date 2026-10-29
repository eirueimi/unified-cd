# Host agents honour `securityContext`

Standard (host) agents now apply a `podTemplate`'s `securityContext` identity
fields to the claim pod: `runAsUser`, `runAsGroup`, `runAsNonRoot`,
`supplementalGroups`, `fsGroup`, `fsGroupChangePolicy` and
`supplementalGroupsPolicy: Merge`, and the flag-only hardening fields
(`allowPrivilegeEscalation: false`, `capabilities`, `readOnlyRootFilesystem`,
`seccompProfile: RuntimeDefault`). Previously the host silently dropped them, so
the controller pinned every job that set any `securityContext` to a Kubernetes
agent. The pin is now narrower, which changes routing, and the isolated
workspace mode changes with it.

This is a **routing behaviour change**: a job that was pinned to a Kubernetes
agent only because of `securityContext` can now run on a standard agent. It also
changes the permissions of every isolated host workspace, whether or not the job
sets a `securityContext`.

| | Before | After |
|---|---|---|
| Required capabilities of a job whose `podTemplate` sets only host-honoured `securityContext` fields | `[pod]` (Kubernetes agent only) | `[container, securitycontext]` (a Kubernetes agent, or a standard agent that advertises `securitycontext`) |
| `securityContext` using a field outside the honoured set | `[pod]` | unchanged, still `[pod]` |
| Hardening fields only (`allowPrivilegeEscalation: false`, `capabilities.drop` / `add: [NET_BIND_SERVICE]`, `readOnlyRootFilesystem`, `seccompProfile: RuntimeDefault`) | `[pod]` | `[container, securitycontext]`; the standard agent passes `--security-opt no-new-privileges`, `--cap-drop`, `--cap-add`, `--read-only` to the container that sets them (not to the pause container), and a job that relied on a writable `/tmp` under `readOnlyRootFilesystem` fails on both agents, as on Kubernetes |
| New capability string | none | `securitycontext` |
| Isolated host workspace root (`working<N>/<job>`) | `0755`, owned by the agent user | `0777`, like a Kubernetes `emptyDir`; its parent `working<N>` becomes `0700` (also for native jobs) so other local users cannot reach it |
| Files the agent writes into the workspace (artifact download, cache restore) | owned by the agent user | owned by the pod-level `runAsUser`/`runAsGroup`; an unset pod-level ID is `0`, so the owner is `0:0` when only a container sets `runAsUser` or only the pod sets `fsGroup`; only the extracted entries are re-owned |
| Rootless runtimes | rootless podman was the recommended runtime: a root container maps to the agent user, and a non-root UID maps to a subordinate UID that the root cleanup fallback removes | the same, plus an ID above the mapped range now fails the claim pod (see below) |
| A securityContext job in a fleet with no Kubernetes agent | stays `Queued` forever | runs on a standard agent that passed its startup probe |

The honoured set is the identity fields listed above plus the hardening fields
`allowPrivilegeEscalation: false`, `capabilities.drop` (with `capabilities.add`
limited to `NET_BIND_SERVICE`), `readOnlyRootFilesystem`, `privileged: false`,
`procMount: Default` and `seccompProfile` / `appArmorProfile` of type
`RuntimeDefault`. These still pin the job to `pod`: `sysctls`, `seLinuxOptions`,
`windowsOptions`, `supplementalGroupsPolicy: Strict`, `seccompProfile` /
`appArmorProfile` of type `Unconfined` or `Localhost`, `privileged: true`,
`capabilities.add` of anything but `NET_BIND_SERVICE`, a `procMount` other than
`Default`, and any key not listed. A
`securityContext` that mixes honoured and pinned values is pinned as a whole. See
[Capabilities and routing](../agents.md#capabilities-and-routing).

## Who is affected, and how to find out before upgrading

A job moves if its required capabilities change from `["pod"]` to
`["container","securitycontext"]` **and** its `agentSelector` is empty or matches
the labels of some standard agent.

This query lists candidates, the jobs whose stored `podTemplate` carries a
`securityContext`:

```sql
SELECT name FROM jobs
WHERE jsonb_path_exists(spec, '$.PodTemplate.spec.securityContext')
   OR jsonb_path_exists(spec, '$.PodTemplate.spec.containers[*].securityContext');
```

The job spec is stored without a JSON tag on that field, so the key is
`PodTemplate` with a capital P. Confirm that on your database with
`SELECT spec FROM jobs LIMIT 1` before trusting an empty result.

For each candidate, ask the controller what the job requires:

```bash
curl -s -H "Authorization: Bearer $TOKEN" \
  https://<controller>/api/v1/jobs/<name>/schedulability
```

Look at `requiredCaps`. After the upgrade, `["container","securitycontext"]`
means the job can leave Kubernetes; `["pod"]` means it stays (it uses a pinned
field).

**Jobs whose podTemplate comes from a git `uses:` template are not visible to
the SQL.** That `podTemplate` is not stored on the job; it is resolved when a run
is triggered. Check the same endpoint on those jobs by hand, or inspect the runs
they create.

In this repository no shipped example or template moves:
`templates/buildkit-rootless-build-push.yaml` sets `seccompProfile` and
`appArmorProfile` to `Unconfined`, which stay pinned.

## What can break for a job that moves

Everything the routing predicate can see behaves the same on both agent types.
What it cannot see is an *implicit* dependency on running inside the cluster:

- cluster DNS names (`*.svc.cluster.local`);
- a mounted ServiceAccount token;
- access that a NetworkPolicy gates.

The job never declared these, so before this change it only worked on Kubernetes
by accident. Remedy: pin the job to a Kubernetes agent explicitly. Every
Kubernetes agent carries the `kubernetes` label:

```yaml
spec:
  agentSelector: [kubernetes]
```

## What starts working

A job with a host-honoured `securityContext` in a fleet that has no Kubernetes
agent no longer stays `Queued` forever. It runs on a standard agent whose
startup probe passed.

## Upgrade order: controller first

Upgrade the controller before any agent, as usual.

- **New host agent, old controller.** The old controller does not know the
  `securitycontext` capability, so registration is rejected with
  `400 unknown capability: securitycontext`. A 4xx is a permanent error: the
  agent logs `permanent error, giving up retry` with `status=400`, stops
  registering and exits (the log does not show the response text; the
  `unknown capability` message appears only in the HTTP response body, and the
  controller does not log it). Upgrade the controller, then restart the agent, or rely on your service
  manager's restart loop.
- **Kubernetes agents keep claiming, old or new.** A Kubernetes agent advertises
  `[pod, container]` before and after this change; it never reports
  `securitycontext` itself. The controller normalises every stored capability set so
  that `pod` implies `securitycontext`, so these jobs reach it whatever its version, and a
  new Kubernetes agent started against an old controller registers without error.
- **Old host agents never receive these jobs.** An old host agent advertises
  `[native, container]`, and an agent that registered with no capabilities at all
  counts as that legacy set. Neither includes `securitycontext`, so neither can
  claim such a run and silently drop the fields.
- **Queued runs keep their old routing.** A run keeps the required capabilities
  computed when it was created, so a run already `Queued` at upgrade time stays
  `[pod]` and waits for a Kubernetes agent. A replay recomputes the routing.

## A host agent advertises `securitycontext` only if its probe passes

At startup (after the container runtime is detected and `ucd-sh` is installed)
the agent runs a probe: it starts a root helper and a container with a
non-root user and supplementary group against a scratch directory under the
workspace base, and checks that identity, supplementary groups, `fsGroup`
ownership and setgid inheritance behave as Kubernetes would. The probe runs once
at startup and its cached result is reused on every claim poll. Only if all checks
pass does it advertise `securitycontext` next to `native` and `container`.
Otherwise it logs one warning and does not advertise it:

```
container runtime cannot honour securityContext (<reason>); jobs that set securityContext will not be routed to this agent
```

Docker Desktop on Windows and macOS often fails the probe: the setgid
inheritance check fails on a file-sharing mount, because the layer between the
host and the VM does not honour setgid inheritance (observed on Docker Desktop
for Windows). The reason then reads, for example, `a file created in the
workspace has group <n>, want 54321: setgid inheritance did not give it the
fsGroup`. Such an agent keeps running jobs without `securityContext`; it just
never receives the ones that need it. The probe runs once per agent process, so
restart the agent after fixing the cause.

## Apply-time validation of `securityContext` values

`securityContext` values are now checked when a job is applied. Previously a value
of the wrong type was accepted at apply time and either silently dropped by a
standard agent or rejected by the Kubernetes API server at run time. A job that
carries such a value is rejected when you apply it again; jobs already stored are
not re-validated until then. Messages (the path differs for a container-level
context: `podTemplate.spec.containers[<i>].securityContext`):

```
podTemplate.spec.securityContext.runAsUser: must be an integer between 0 and 2147483647 (got "1000")
podTemplate.spec.securityContext.runAsGroup: must be an integer between 0 and 2147483647 (got -1)
podTemplate.spec.securityContext.fsGroup: must be an integer between 0 and 2147483647 (got "100")
podTemplate.spec.securityContext.supplementalGroups: must be a list of integers (got 5)
podTemplate.spec.securityContext.supplementalGroups[1]: must be an integer between 0 and 2147483647 (got "x")
podTemplate.spec.securityContext.fsGroupChangePolicy: must be one of Always, OnRootMismatch (got "Sometimes")
podTemplate.spec.securityContext.supplementalGroupsPolicy: must be one of Merge, Strict (got "x")
podTemplate.spec.securityContext: must be a map (got "x")
```

The usual cause is a quoted number (`runAsUser: "1000"`); write it unquoted. A
`podTemplate` that comes from a git `uses:` template is resolved when a run is
triggered and is not checked at apply time.

## Workspace permissions: every isolated workspace is `0777`

The workspace root of **every** isolated host job (with or without a
`securityContext`) is now created with mode `0777`, as a Kubernetes `emptyDir`
is, so a container running as any UID can write to it. `native: true` jobs and
workspaces backed by a [named volume](../agents.md#named-volumes) are not
changed. What keeps other users on the host out is the slot directory above the
workspace: the agent creates `working<N>` with mode `0700` and tightens one an
older agent left at `0755`, when it starts and again before each run. Containers
do not need to traverse it (the runtime bind-mounts the job directory itself),
and native jobs run as the agent user. Also keep the workspace base directory
(`wsBase`) at `0750` or stricter. If you run something other than the agent
user over `working<N>` (a backup job, a monitoring script), it loses access.

Files the agent writes for the job (an artifact download, a cache restore) are
owned by the pod-level identity, as the Kubernetes artifact sidecar does: an
unset pod-level `runAsUser` or `runAsGroup` means ID `0`, so the owner is `0:0`
when only a container sets `runAsUser` or only the pod sets `fsGroup`. Only
the entries the extraction wrote, and directories it newly created, are
re-owned; anything already at the destination keeps its owner. Workspace
cleanup and garbage collection fall back to a root helper container (the pinned image
`docker.io/library/busybox:1.36@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662`; an air-gapped host must have it pulled or loaded beforehand), so files
owned by other UIDs do not block them. When an artifact upload or cache save cannot read a
file a container wrote as another UID, the retry copies it out of the claim pod into a
private directory under `wsBase` (`.ucd-root-copy-<random>`), so budget disk there.
When a pod sets `fsGroup`, each volume (the workspace and every named volume, including one
mounted at a relative path below the workspace) is regrouped on its own, and the helper's
per-root summary, including a count of failed entries, is written to the run log; only the
first 20 failures are listed.

## Rootless runtimes

Rootless docker and podman map only a range of container IDs. A job that asks
for `runAsUser`, `runAsGroup`, `supplementalGroups` or `fsGroup` above that range
cannot be honoured, so the claim pod fails to start with an explicit error of
this form (the field, ID and range vary):

```
runAsUser 70000 is not mapped by this agent's rootless container runtime (mapped container IDs 0-65535); use an ID within that range or run the job on a Kubernetes agent
```

Rootless podman is still the recommended runtime (files a root container writes
belong to the agent user, and files owned by a non-root UID that maps to a
subordinate UID are removed by the root cleanup fallback), but it is no longer
unrestricted: an ID above its mapped range is rejected. Use an ID within the
range, or add `agentSelector: [kubernetes]`.
