# Volumes: Keeping Data Between Runs

A job's workspace is **ephemeral on every agent**. It is empty when a run starts and is discarded
when the run ends, whether the job lands on a standard (host) agent or a Kubernetes agent, and
whether it is `native: true` or isolated. Nothing a run writes to its workspace is visible to the
next run.

The one way to keep data between runs is a **named volume**. A volume belongs to an *agent*, not
to a job:

- The **operator** declares it in the agent's own config file: its name, its mode, and (the
  operator's business only) its size, storage class and location on disk. See
  [Agent named volumes](../../operator-manual/agents.md#named-volumes) and, for the Kubernetes
  agent, [Named volumes on Kubernetes](../../operator-manual/kubernetes-integration.md#named-volumes).
- The **job** names a volume and says where it appears, with `spec.volumes`. It does not say
  where the data lives or how large it is, so the same job YAML runs unchanged on a host agent and
  a Kubernetes agent.

This is the answer for a Unity project whose `Library/` takes tens of minutes to regenerate, or a
checkout with large LFS objects: the data stays on a volume, and the S3-backed
[`cache:`](artifacts-and-cache.md) step (which tars the directory up and down on every run) is
reserved for what is cheap to move.

```yaml
spec:
  volumes:
    - name: go-cache
      mountPath: .ucd-cache/go
  steps:
    - name: build
      run: |
        export GOCACHE="$UNIFIED_VOLUME_GO_CACHE/build"
        export GOMODCACHE="$UNIFIED_VOLUME_GO_CACHE/mod"
        go build ./...
```

---

## `spec.volumes` fields

| Field | Type | Required | Description |
|---|---|---|---|
| `name` | string, list of strings, or template | Yes | The volume to mount. A list is a set of *candidates*: the agent picks a free one. A name may contain `{{ .Params.X }}`, expanded when the run is queued. |
| `as` | string | When `name` is a list or a template | The stable handle steps use to reach the volume. Defaults to the name when it is one literal string. |
| `mountPath` | string | Yes | Where the volume appears. Relative paths are relative to the workspace and `"."` is the workspace root. An absolute path is a path outside the workspace. |

A job can mount several volumes. Two entries must not use the same `mountPath`, and one
`mountPath` must not sit inside another's (`"."` may be combined with any other entry).

### Candidate lists and the alias

```yaml
spec:
  volumes:
    - name: [go-cache-1, go-cache-2, go-cache-3]
      as: go-cache
      mountPath: .ucd-cache/go
```

The agent holds each *exclusive* volume for one run at a time, so a list of identical copies is how
you get parallelism without a network filesystem: three copies serve three concurrent runs, and a
fourth run waits for a free one. `as` is required for a list (or a template) because the volume
the run is handed is not known when the job is written, and the steps need a name that does not
change. See [the environment variable](#reaching-a-volume-from-a-step) below.

Two rules that are easy to trip over:

- **Two entries carrying the same multi-candidate list resolve to two distinct volumes** when two
  are free. If you want *one* volume at two mount paths, write the same *single* name twice.
  When a group of entries lists fewer distinct volumes between them than there are entries in the
  group, the agent's search for distinct volumes is capped, and the run waits instead of starting
  (the agent logs `volume assignment search abandoned`; see
  [Troubleshooting](../../troubleshooting/steps-and-execution.md#a-run-waits-and-the-agent-logs-volume-assignment-search-abandoned)).
  Ordinary jobs, including one with a dozen entries over a dozen declared volumes, never reach the
  cap.
- **A name containing anything other than letters, digits, `-` and `_` needs `as`.** The
  environment variable is built from the alias, so a volume named `unity.main` cannot be its own
  alias. The agent config allows such a name (it rejects only `.`, `..` and names with a path
  separator), so the job has to supply a clean alias:
  `` spec.volumes[0]: the alias "unity.main" (`as`, or the name when there is no `as`) cannot be part of an environment variable name (UNIFIED_VOLUME_UNITY.MAIN): use letters, digits, '-' and '_' ``.
  A name that is a DNS label (lower-case letters, digits, `-`) is always fine, and is the only
  form a Kubernetes agent accepts.

A template name is expanded against the run's parameters when the scheduler queues the run, so
the controller knows the concrete candidates before any agent claims it:

```yaml
spec:
  volumes:
    - name: "unity-{{ .Params.PLATFORM }}"
      as: unity
      mountPath: "."
```

---

## Modes

The mode is set by the operator in the agent's config, not by the job.

| Mode | Behaviour |
|---|---|
| `exclusive` | At most one run on the agent holds the volume at a time. A second run that needs it **waits** (holding its concurrency slot) until the holder finishes; the run's own log says `waiting for volumes ["unity-main"] and paths [], which another run on this agent is using` (host agent) or `waiting for volumes [["unity-main"]], which another run is using` (Kubernetes agent). |
| `shared` | Any number of runs may hold the volume at once. unified-cd makes **no write-safety guarantee**: concurrent-write correctness is the job's problem. That suits Go's build and module caches and git object stores, which are safe for concurrent use. |

A `shared` volume **cannot be mounted at `"."`**: several runs would check out over each other
with no exclusion. The agent rejects it when it mounts, because only the agent knows the mode:

```
volume mount failed: spec.volumes[0]: volume "go-cache" is shared and cannot be mounted at the workspace root (mountPath "."): several runs would work in one checkout with no exclusion. Declare it exclusive in the agent config, or mount it at a subdirectory
```

---

## Mount paths

- **Relative** (`.ucd-cache/go`, or `"."`): inside the workspace. `"."` makes the volume the
  workspace itself, so the checkout and everything else in it persist. A relative path must not
  climb out of the workspace (`..`).
- **Absolute** (`/root/.cache/go`): a path outside the workspace, for toolchains that keep their
  caches elsewhere or reject relative paths. An absolute path that lands *inside* the workspace is
  rejected: write it relative.
- **A POSIX path, always.** A `mountPath` is written with `/` on every agent, Windows agents
  included. A Windows drive-qualified path (`C:/cache`, `d:\cache`) is rejected when the job is
  applied (`mountPath must be a POSIX path`), because it would otherwise be mistaken for a relative
  path and fail much later with an opaque `mkdir` error.

How the volume is realised depends on where the run executes, but your YAML does not change:

| Where | What a relative path becomes | What an absolute path becomes |
|---|---|---|
| Isolated job, host agent | A bind mount into the claim pod | A bind mount at that container path |
| Isolated job, Kubernetes agent | The PVC mounted at that path in every container of the pod | The same |
| `native: true` job (host agent only) | A symlink inside the workspace, removed when the run ends | A symlink at that **host** path pointing into the volume, removed when the run ends |

Three consequences for `native: true` jobs, which have no container to bind-mount into:

1. **Nothing is clobbered.** If something already exists at the absolute path and the agent did not
   create it as a volume link, the run fails rather than replace it:
   `volume mount failed: <path> already exists and was not created by this agent as a volume link, so it will not be replaced; remove or rename it, or choose a different mountPath`.
2. **An absolute path is an agent-wide exclusive resource.** The host path is global to the
   machine, so two native runs cannot point it at two volumes. A run that needs it waits for the
   run holding it, even when the volume is `shared` and even when you list several candidates;
   candidate lists give parallelism on a native run only for *relative* mount paths.
   The reservation is made **by the agent, after it has claimed the run**; the controller cannot
   filter on it (it routes on volume names, and an absolute path is not one). So with
   `--max-concurrent 4`, four queued runs that all mount the same absolute path are all claimed,
   one runs, and the other three sit in a concurrency slot waiting. A job in that shape occupies one
   agent slot per queued run, whatever the volume's mode. A relative `mountPath` avoids this: the
   controller then routes around a busy exclusive volume instead of letting runs queue up in slots.
3. **On Windows**, creating a symlink needs administrator rights or Developer Mode. The agent falls
   back to a directory junction, which needs an NTFS volume and a path without `cmd.exe`
   metacharacters; if both fail the run fails with
   `volume mount failed: cannot link <link> to <target> on Windows: creating a symlink failed (...) and so did creating a directory junction (...)`.

Volume mount paths deliberately do **not** go through the escape check that artifact and cache
paths do (which rejects every absolute path). The operator declared which volumes exist and a job
can only name one of them, so what an absolute `mountPath` can reach is bounded by the agent's own
config; and a native job can already run any command as the agent user.

### What the job cannot combine with volumes

- **`spec.detached: true`** is rejected at apply time: a detached run's workspace is deleted when
  the run finishes, which contradicts a persistent volume.
- **`podTemplate.reuse: true`** is rejected at apply time: a pooled Kubernetes Pod keeps the
  volumes it was created with, but each run may need different ones.

---

## Reaching a volume from a step

Every step receives `UNIFIED_VOLUME_<ALIAS>`, the absolute path at which *that step* sees the
volume. `<ALIAS>` is the entry's `as` (or its single literal name) upper-cased with `-` written as
`_`: `as: go-cache` gives `UNIFIED_VOLUME_GO_CACHE`. It is named by the alias and never by the
volume the run happened to be handed, which is how a job using a candidate list or a template
keeps working. Use it to point a tool at a volume you mounted at a relative path:

```yaml
steps:
  - run: |
      export GOCACHE="$UNIFIED_VOLUME_GO_CACHE/build"
      export GOMODCACHE="$UNIFIED_VOLUME_GO_CACHE/mod"
      go build ./...
```

Three places where the variable is **not** what you might expect:

- **A scoped step sees it unset.** A step with `runsIn.image` runs in its own scope container,
  which mounts no volume, so `UNIFIED_VOLUME_*` is unset there and the volume's contents are not
  present. The `GOCACHE` example above is exactly the shape that breaks in a scoped step: it
  exports a path rooted in an empty variable. Scoped steps cannot use a volume.
- **A step's own `env:` overrides it**, as it does `UNIFIED_WORKSPACE`. The variable prefix is
  reserved for [`Vars`](../variables.md), so a variable cannot shadow it, but an `env:` entry on the
  step can.
- **`post:` hooks receive none of these variables.** A hook's environment is only its own `env:`.
  A cache-saving hook is the natural consumer of a volume path, so it must hard-code the path or
  recompute it. A hook runs with the workspace as its working directory, so a volume mounted at
  a relative `mountPath` (`.ucd-cache/go`) can be reached by that same relative path, and a volume
  at an absolute `mountPath` by that absolute path. With a candidate list or a template name the
  alias still gives a stable *mount path*, which is what the hook should use, not the volume name.

---

## Wiping a volume: `cleanWorkspace`

The agent never empties a volume on its own initiative, and removing a volume from the agent's
config deletes nothing. The one deliberate way to destroy a volume's data is the job asking:

```yaml
spec:
  cleanWorkspace: true     # empty the volume mounted at "." before this run
```

`spec.cleanWorkspace` and `podTemplate.cleanWorkspace` do the same thing and either one is
enough (the result is their OR). `spec.cleanWorkspace` exists because `native: true` is
incompatible with `podTemplate`, so a native job, which is the usual home of a large persistent
workspace, could not otherwise reach the escape hatch.

- It applies to a volume **at the workspace root** (`mountPath: "."`), which *is* the workspace.
  A volume at any other path is left alone, and that includes one mounted *below* the root
  (`"."` for the checkout plus `.ucd-cache/go` for a cache): the checkout is emptied, the cache
  keeps every byte, on host and Kubernetes agents alike.
- It empties the volume's contents, not the mount point, and falls back to a throwaway root
  container when a rootful container runtime left root-owned files behind, or when a job that
  set `securityContext.runAsUser` left files owned by another UID (a subordinate UID under
  rootless podman).
- An ordinary workspace is always emptied, so the flag changes nothing for a job with no volume at
  `"."`. The agent-wide `--clean-workspace` flag no longer does anything at all and never wipes a
  volume.
- It is a *run-level* setting: leaving it `true` wipes the volume on every run, which defeats the
  volume. Set it for one run to recover a poisoned volume, then remove it.

### A mode flip fails the run instead of wiping

On a host agent, the agent records whether each volume that serves as a workspace was last used by
a `native` or an isolated run, in its own state directory (never inside the volume). If a job's
mode changes, the run **fails** rather than resetting the directory, because resetting would
delete the volume:

```
prepare workspace failed: volume "unity-main" was last used by a native run and this run is isolated: the agent will not reset a volume itself, because that would delete its contents. To keep the data, run the job as native again. To wipe the volume once and continue as isolated, set spec.cleanWorkspace: true on the job. The agent records the mode in <agent state>/.ucd-volume-modes/unity-main; deleting that file also lets the run proceed without wiping
```

Run the job in the mode it last used, set `cleanWorkspace: true` once, or delete the named marker
file to accept the volume as it is.

---

## Artifacts and caches that reach a volume

`uploadArtifact`, `downloadArtifact` and `cache:` paths resolve through the mount table, so a path
*inside* a volume works as you would expect: `uploadArtifact` of `.ucd-cache/go/build` archives the
volume's real contents.

An artifact or cache path that is an **ancestor** of a volume's mount point is different. Archiving
`.` or `.ucd-cache` would otherwise tar tens of gigabytes of volume contents into every artifact.
**A volume's contents are excluded from an archive of an ancestor path**, on both agent types,
and the exclusion is announced in the run log:

```
artifact path "." excludes volume "go-cache" mounted at ".ucd-cache/go": the contents of a volume are never archived as part of a parent directory; name the volume's mount path to archive it
```

(`cache path ...` for a cache step; one line per excluded volume per archive.) The mount point
itself is omitted too; a parent directory that existed only to hold it stays as an empty directory.
To archive a volume deliberately, name the volume's own mount path.

The rule holds in the other direction as well. **`downloadArtifact` and `cache:` restore never
extract into a volume through an ancestor path.** An artifact or cache entry packed before a
directory became a volume's mount point still holds files there; extracting them would write into
the volume on Kubernetes, through the mount link into the volume on a native host, and into a
directory the container cannot see (swept when the run ends) on an isolated host. Instead they are
skipped on every agent, and the run log says so:

```
artifact download path "." excludes volume "go-cache" mounted at ".ucd-cache/go": files of the artifact that lie inside a volume are not extracted into it; name the volume's mount path as the destination to extract into it
```

To fill a volume from an artifact on purpose, make the volume's own mount path the `destDir`.

---

## Scheduling and diagnosing a run that does not start

The controller records which volume names each agent advertises, but holds no volume definitions and
no occupancy state (those live in the agent), so an unknown volume name is **not** an apply-time error. The
run is queued and stays `Queued`, and the job page shows an unschedulable banner naming the
volume. Routing is an AND of ORs: for every `spec.volumes` entry, the agent must declare at least
one of its candidates, and the whole set must be declared by **one** agent, because a run is
claimed by exactly one agent.

| Reason | Meaning |
|---|---|
| `no agent declares volume "unity-main"` | No registered agent advertises that single name. |
| `no agent declares any of the volumes ["a" "b"]` | No registered agent advertises any candidate of the list. |
| `no single agent that provides capability [...] and matches labels [...] declares all of the volumes [...]` | Every candidate set is declared somewhere, but never all by one agent that also fits the capability and labels. |

See [Run stays Queued: no agent declares volume](../../troubleshooting/runs-and-scheduling.md#job-stays-queued-no-agent-declares-volume)
for the fixes, including the `volumesDependOnParams` case where a templated name cannot be judged
until the run is triggered.

A volume that is declared but **busy** is not a scheduling failure: the run is claimed and waits,
holding a concurrency slot (see [Modes](#modes)).

---

## How full is a volume?

A job cannot see how much room its volume has, and `size` is the operator's setting, not yours. On a
host agent it is **advisory**: the agent warns when a volume grows past it but never stops a run, so
it does not tell you how much space is actually left. What does protect a volume is the agent's
`minFreeDisk` (on by default, `1%` of the volume's filesystem): **if an agent's volume is below its
free-space threshold, runs that mount it stay `Queued`; other jobs are unaffected.** The run starts
once space is freed (or another agent that declares the volume claims it). A cleanup job, or
`cleanWorkspace: true`, that mounts the gated volume waits as well, so it cannot free the space
itself; the operator has to. See [Run stays Queued:
the volume it mounts is below
`minFreeDisk`](../../troubleshooting/runs-and-scheduling.md#run-stays-queued-the-volume-it-mounts-is-below-minfreedisk).
The real number is on the controller's
`/metrics` as `unifiedcd_agent_volume_filesystem_free_bytes{agent_name,volume}`, next to
`unifiedcd_agent_volume_measured_timestamp_seconds`, which says how old it is. On a Kubernetes agent
the figure is refreshed only when a run mounts the volume. Ask your operator to alert on it; the
queries, and what a missing series means, are in [Operations: Watching agent disk
headroom](../../operator-manual/operations.md#watching-agent-disk-headroom).

---

## Volumes belong to one agent

Two agents declaring the same volume name hold **two independent copies**. A run goes to any agent
with a free copy; there is no affinity for the agent that ran the job last. In steady state every
copy stays warm, but a job that relies on the *same* data being there must be pinned to one agent
with `agentSelector`, or the volume name must be declared by exactly one agent.

A volume is never shared across agents. The controller stores only the names each agent advertises
and each run's candidate lists; it holds no volume definitions (mode, size, location are the agent's
config) and no occupancy state (which run holds a volume is tracked by the agent).
