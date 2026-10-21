# Migrating to ephemeral workspaces and agent-owned named volumes

Until now the two agent types disagreed about whether a job's workspace survives a run, so a job
author had to know which agent a job would land on. A **host agent** kept
`<workspaceDir>/working<slot>/<job>` between runs; the **Kubernetes agent** gave each Pod an
`emptyDir` that died with the Pod, unless the job used `podTemplate.workspace.pvc`, which each
agent type implemented differently.

That is replaced by one rule. **The undeclared workspace is ephemeral on every agent, and
persistence comes only from a named volume the agent's own config declares.** See [Volumes:
Keeping Data Between Runs](../../user-guide/writing-jobs/volumes.md) for the feature itself.

This release carries several breaking changes that are easy to miss one at a time. Read the table,
then the sections for the ones that apply to you.

| # | Before | After |
|---|---|---|
| 1 | A host agent kept a job's workspace between runs (`working<slot>/<job>`), so a second run saw the first run's files. Which copy a run saw depended on which concurrency slot claimed it. | The workspace is empty at the start of every run and removed at the end, on host and Kubernetes agents, for `native: true` and isolated jobs alike. Nothing carries over. |
| 2 | A Kubernetes Pod with `podTemplate.reuse: true` kept its `/workspace` for the next run (unless `cleanWorkspace` was set). | A reused Pod's workspace is emptied, dot-files included, before every run. `reuse` only saves Pod start-up. |
| 3 | `podTemplate.workspace.pvc` (both `claimName` and the `storageClassName`/`storageRequest`/`accessMode` form) was accepted. | **Removed.** A job or template that carries it is rejected; an agent config that carries it does not start. See [`workspace.pvc` is removed](#workspacepvc-is-removed). |
| 4 | The agent-wide `--clean-workspace` flag (and `cleanWorkspace:` agent config key) wiped every job's directory on every claim. | The flag is accepted and **does nothing**. Every ordinary workspace is already emptied on every run, and the flag never wipes a volume. It exists only so existing agent configs still start. |
| 5 | A workspace that could not be emptied logged a warning and the run **proceeded on the dirty directory**. A reused Pod whose workspace clear failed was silently kept and reused. | The run **fails**. A reused Pod whose clear fails is deleted instead of returned to the pool. |
| 6 | Each host workspace carried a `.ucd-mode` marker file; a native/isolated flip deleted the directory. | Ordinary workspaces have no marker. A volume used as the workspace keeps its marker **outside** the volume, in the agent's state directory, and a flip **fails the run** instead of wiping. |
| 7 | `spec.cleanWorkspace` did not exist; a `native: true` job could not set `podTemplate.cleanWorkspace`. | `spec.cleanWorkspace` is new. It wipes a volume mounted at `"."`. `podTemplate.cleanWorkspace` still works and the two are ORed. |
| 8 | (Kubernetes) The agent's Role needed `create` and `delete` on `persistentvolumeclaims`, which it never used. | With `volumes` configured, the Role needs `get`, `list`, `create` and `update`. See [Kubernetes agents](#kubernetes-agents). |

**Database and upgrade order.** This release adds one migration, `022_agent_volumes`, which is
purely additive: it adds `agents.volumes` (the volume names each agent advertises) and
`runs.required_volumes` (each run's candidate lists, written once when the run is queued), both
`NOT NULL` with defaults. It runs automatically at controller startup, like every migration, and an
older controller binary keeps working against the migrated schema (it just cannot route volume jobs).
The controller therefore stores the names agents advertise and the candidate lists runs need, but
holds **no volume definitions and no occupancy state**: what a volume is (mode, size, location) lives
in the agent's config, and who is holding it right now lives with the agent. **Upgrade the controller
first, then the agents**, so the controller can record the volumes an agent advertises; until it is
upgraded, jobs that mount volumes stay unschedulable. No job YAML that does not use `workspace.pvc`
fails to apply because of this release.

---

## The workspace is ephemeral (rows 1, 2, 4, 5)

**What to look for.** A job that reads, in a later run, a file an earlier run left behind: a
`git-checkout` that expects `.git` to exist, a build directory, a downloaded toolchain, a
`node_modules`, an incremental compiler's output. Before, on a host agent, those worked by accident
whenever the same slot happened to serve the run. After, the job re-creates them every run.

There is no field to search for. Two probes help:

```bash
# jobs that depend on a cache kept in the workspace
grep -rn -E "\.cache|node_modules|\.gradle|Library/|\.git\b|incremental" <your job definitions>

# agent configs that set the now-inert flag
grep -rn -E "clean-workspace|cleanWorkspace" <your agent configs and unit files>
```

**How to keep the data.** Pick by what the data is:

| The data is | Use |
|---|---|
| Cheap to move: a dependency directory of a few hundred MB | [`cache:`](../../user-guide/writing-jobs/artifacts-and-cache.md) (S3-backed; works on every agent, unchanged) |
| Expensive to move: a Unity `Library/`, an LFS checkout, a multi-GB build cache | A named volume (below) |

To move a host job's persistent workspace onto a volume:

1. Declare the volume in the agent's config (file-only; there is no flag):

   ```yaml
   # unified-agent.yaml
   volumes:
     - name: unity-main
       mode: exclusive
       path: /var/lib/unified-cd/volumes/unity-main   # optional
       size: 200Gi                                    # advisory
   ```

2. Mount it in the job. `mountPath: "."` makes the volume the whole workspace, which is what a
   job that used to rely on its per-job directory wants:

   ```yaml
   spec:
     volumes:
       - name: unity-main
         mountPath: "."
   ```

3. The volume starts empty. The first run repopulates it (a fresh clone, a full import); later
   runs find it. Nothing is migrated from the old `working<slot>/<job>` directory, which can be
   deleted.

**Reused Pods.** `podTemplate.reuse: true` combined with `spec.volumes` is rejected, because a
pooled Pod keeps the volumes it was created with. A job that used `reuse` *as* its cache must
choose: keep `reuse` and move the data to `cache:`, or drop `reuse` and use a volume.

**Disk.** Ordinary host workspaces no longer accumulate, since each is removed after its run; the
opt-in `workspaceRetentionDays` GC now reclaims only what an agent crash left behind. Volumes are
the new thing to monitor: they grow without bound and nothing sweeps them. See [Operations:
Named-Volume Hygiene](../operations.md#named-volume-hygiene).

### `--clean-workspace` is a no-op (row 4)

Leaving the flag or `cleanWorkspace: true` in an agent config is harmless: the agent starts. It is
not an alias for anything. Remove it at your convenience. Removing it does not change behaviour,
and setting it does not wipe a volume. The only thing that empties a volume is a job's own
`spec.cleanWorkspace` or `podTemplate.cleanWorkspace`.

### A workspace that cannot be emptied now fails the run (row 5)

Before, if emptying the workspace failed (typically root-owned files that a rootful container left
behind, where even the throwaway root-cleanup container failed), the agent warned and ran on. Now
the run fails before its first step. The messages to search for:

| Where | Message in the run log / agent log |
|---|---|
| Host agent | `prepare workspace failed: clean workspace <dir>: ...` (the run is failed with this reason) |
| Kubernetes agent, reused Pod | `k8s: could not clear the reused Pod's workspace: ...` (the Pod is deleted, not returned to the pool) |

Fix the cause (use rootless podman, or delete the directory by hand on the agent host); re-trigger
the run. See [Workspace lifecycle](../agents.md#workspace-lifecycle).

---

## `workspace.pvc` is removed

`podTemplate.workspace.pvc` is gone, in both forms:

```yaml
# removed form 1: an existing claim
podTemplate:
  workspace:
    pvc:
      claimName: go-build-cache

# removed form 2: a per-Pod ephemeral claim (deleted with the Pod)
podTemplate:
  workspace:
    pvc:
      storageClassName: standard
      storageRequest: 5Gi
      accessMode: ReadWriteOnce
```

`podTemplate.workspace.mountPath` stays.

It overlapped named volumes, meant something different on each agent type (a host agent degraded it
to a bind mount, a Kubernetes agent mounted a real claim) and the second form's claim was deleted
with the Pod. Its replacement keeps the claim name out of the job:

| Before (`pvc`) | After (named volume) |
|---|---|
| The job names a cluster resource: `claimName: go-build-cache` | The job names a *volume*, `go-build-cache`; the Kubernetes agent owns the PVC `<agentName>-go-build-cache` |
| A hand-created PVC; ReadWriteOnce contention between runs was the job's problem | The agent provisions the PVC, and a mode of `exclusive` makes the agent hold it for one run at a time |
| `storageClassName`, `storageRequest`, `accessMode` in the job | `storageClassName`, `size` and `mode` in the agent's config, where the operator owns them |
| An ephemeral `pvc` was deleted with the Pod | A volume is never deleted by the agent |
| An ephemeral `pvc` with no `storageClassName` requested the *empty* class, which Kubernetes reads as "no StorageClass": the cluster default was not applied and the claim stayed `Pending` | An unset `storageClassName` is left unset on the PVC, so the cluster default applies |

### What you will see

A job or JobTemplate that still carries the block is rejected when it is applied (and when an
AppSource syncs it):

```
spec.podTemplate.workspace.pvc: workspace.pvc was removed: the workspace is now ephemeral on every agent, and persistence comes from an agent-owned named volume (declare it in the agent's config and mount it with spec.volumes); see docs/operator-manual/migrations/2026-10-20-named-volumes.md
```

A Kubernetes agent whose config carries it under `podTemplates` **does not start**:

```
<config path>: podTemplates["golang"].workspace.pvc: workspace.pvc was removed: the workspace is now ephemeral on every agent, and persistence comes from an agent-owned named volume (declare it in the agent's config and mount it with spec.volumes); see docs/operator-manual/migrations/2026-10-20-named-volumes.md
```

That agent-config check exists because that file's loader ignores unknown keys, so without it
the block would be dropped silently and a persistent workspace would quietly become ephemeral.

> **A job that was applied before the upgrade is not re-validated.** The controller stores the spec
> and decodes it leniently, so an already-applied job whose podTemplate carries `pvc` keeps
> running, with the `pvc` ignored and an ephemeral workspace. It is rejected the next time it is
> applied. Find them before they surprise you:
>
> ```bash
> grep -rn -E "pvc:|claimName:" <your job definitions>
> ```

### How to migrate a `pvc` job

1. Choose a volume name and add it to the Kubernetes agent's config. `agentName` and `size` are
   required:

   ```yaml
   agentName: ci-main
   volumes:
     - name: go-build-cache
       mode: exclusive          # ReadWriteOnce; "shared" needs an RWX StorageClass
       size: 20Gi
       # storageClassName: gp3  # omit to use the cluster default
   ```

2. Replace the `pvc` block in the job with `spec.volumes`:

   ```yaml
   # before
   podTemplate:
     workspace:
       pvc:
         claimName: go-build-cache
   # after
   volumes:
     - name: go-build-cache
       mountPath: "."        # the volume IS the workspace; use e.g. .ucd-cache/go for a subdirectory
   ```

3. Roll the agent's Role first (see below), then the agent.
4. The new PVC starts empty. The old claim is not read or deleted. Delete it yourself when you
   are done with it.

---

## Kubernetes agents

Roll in this order. Rolling the image first produces an agent that fails at startup on a real
cluster.

1. **The Role, before the agent image.** With `volumes` configured the agent creates and locks
   PVCs. The Role needs `get`, `list`, `create` and `update` on `persistentvolumeclaims`.
   `manifests/base/k8s-agent/rbac.yaml` carries them; if you maintain your own Role, add them.
   The shipped Role previously had only `create` and `delete`. The agent never deletes a volume's
   PVC itself.
2. **`agentName`.** Required once `volumes` is set. It prefixes the PVC names
   (`<agentName>-<volume>`) and **must never change while the volumes hold data**: a new name
   provisions new, empty PVCs and leaves the old ones behind. The enrolled agent ID cannot serve,
   since it contains colons and changes on every restart.
3. **The sidecar and the agent, together.** A new k8s-agent passes `--exclude` to the
   `unified-artifact` sidecar, but only when a job archives (or downloads an artifact into) an ancestor of a volume's mount point.
   A sidecar image that predates the flag exits 2 on it, so that artifact upload fails and that
   cache reads as "not restored". Both are set in the agent's config (`sidecarImage`); if you
   override it, bump it to the build that ships with the agent. See [Rotating the default
   image digests](../operations.md#rotating-the-default-runnerpause-image-digests).
4. **Volume names are DNS labels.** Lower-case letters, digits and `-`, with the claim name
   `<agentName>-<volume>` at most 63 characters. A name that is legal on a host agent can be
   illegal here.

---

## New failure modes

Each has a symptom entry with the exact string to search for:

- A run that stays `Queued`: [no agent declares volume](../../troubleshooting/runs-and-scheduling.md#job-stays-queued-no-agent-declares-volume).
- A run that waits on a held volume: [a run waits for a volume another run holds](../../troubleshooting/steps-and-execution.md#a-run-waits-for-a-volume-another-run-holds), and one whose holder crashed: [a volume stays held after its run crashed](../../troubleshooting/steps-and-execution.md#a-volume-stays-held-after-its-run-crashed).
- A run that fails to mount or prepare a volume: [mode flip](../../troubleshooting/steps-and-execution.md#run-fails-volume-was-last-used-by-a-nativeisolated-run), [clobber refusal](../../troubleshooting/steps-and-execution.md#run-fails-already-exists-and-was-not-created-by-this-agent-as-a-volume-link), [Windows symlink](../../troubleshooting/steps-and-execution.md#run-fails-cannot-link-on-windows), [shared volume at the workspace root](../../troubleshooting/steps-and-execution.md#run-fails-volume-is-shared-and-cannot-be-mounted-at-the-workspace-root).
- A Pod stuck in `Pending` on a shared volume: [RWX unavailable](../../troubleshooting/steps-and-execution.md#a-shared-volume-never-binds-rwx-unavailable).
- A workspace that cannot be emptied: [`prepare workspace failed: clean workspace`](../../troubleshooting/steps-and-execution.md#run-fails-with-prepare-workspace-failed-clean-workspace).
- A Kubernetes agent that will not start: [volume configuration errors](../../troubleshooting/agents-and-enrollment.md#an-agent-fails-at-startup-with-a-volume-configuration-error).
- An artifact step or cache failing against an older sidecar: [`-exclude` flag](../../troubleshooting/artifacts-and-storage.md#artifact-upload-fails-or-a-cache-is-never-restored-flag-provided-but-not-defined-exclude).

---

## Checklist

- [ ] No `pvc:` or `claimName:` under any job, template or agent `podTemplates` (`grep` above).
- [ ] Every job that needs data between runs mounts a volume, or uses `cache:`.
- [ ] Every volume a job names is declared by an agent that fits the job's `agentSelector`; an
      unknown name leaves the run `Queued`.
- [ ] Kubernetes: Role updated, `agentName` set, sidecar image current, volume names are DNS labels.
- [ ] Disk monitoring includes each host agent's volume paths (default `<workspaceDir>/volumes/<name>`).
      The controller exports each volume's free space as `unifiedcd_agent_volume_filesystem_free_bytes`;
      see [Watching agent disk headroom](../operations.md#watching-agent-disk-headroom).
- [ ] Jobs that persisted secrets or credentials in the old workspace are reviewed: a volume
      *does* persist them, and a workspace no longer does.
