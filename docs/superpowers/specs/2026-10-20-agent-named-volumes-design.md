# Agent-owned named volumes — Design

Date: 2026-10-20
Status: Approved (design); implementation plan to follow

## 1. Purpose

Kubernetes support exists so that operators can hand node supply to a cluster
autoscaler instead of managing hosts by hand. In practice the standard (host)
agent and the k8s-agent behave differently enough that the *job author* has to
know which one their job will land on. The goal of this project:

> An isolated job (`native: false`) behaves the same way on any agent.

Exact parity is not the goal (see §9). The concrete motivation is a Unity
project with two things that must survive between runs:

- `Library/` — tens of GB, tens of minutes to regenerate.
- The git checkout plus LFS objects — too heavy to re-clone every run.

The S3-backed `cache:` mechanism does not fit: tarring tens of GB up and down
per run costs more than it saves. The data has to stay on a volume.

## 2. Goals, non-goals, anti-goals

**Goals**

- Workspace persistence behaves identically on host agents and k8s-agents.
- Persistence is explicit, named, and *guaranteed* — not a function of which
  concurrency slot or which agent happened to pick the run up.
- Operator-only concerns (size, storage class, on-disk location) stay out of
  the job definition.
- The Unity case works: a run finds the same `Library/` and the same checkout
  it left behind.

**Non-goals**

- Full host/k8s parity (§9 tiers 2 and 3).
- Cross-agent shared storage. A volume belongs to one agent.
- Controller-side volume state. The controller stores nothing about volumes.
- Per-directory disk quota enforcement on host agents (not achievable across
  operating systems without root).

**Anti-goals** — outcomes that make this a failure even if it ships:

- A job that reads as portable but silently behaves differently per agent type.
- Persistence that is probabilistic ("you will usually get your old workspace").
- unified-cd deleting tens of GB of a user's data on its own initiative.

## 3. Current behaviour

| | Host agent | k8s-agent |
|---|---|---|
| Default workspace | `<workspaceDir>/working<slot>/<job>` (`claimWorkDir`, `internal/agent/workspace.go:66`). Survives runs. | `emptyDir` (`buildWorkspaceVolume`, `internal/k8sagent/podbuilder.go:627`). Dies with the pod. |
| `workspace.pvc.claimName` | Bind mount replacing that directory | An existing PVC, shared by every run (RWO contends) |
| `workspace.pvc` without `claimName` | Bind mount replacing that directory | Generic ephemeral volume (10Gi/RWO default) — **deleted with the pod** |
| `podTemplate.spec.volumes`, `volumeMounts`, `override` | Unsupported. `PodTemplateNeedsKubernetes` (`internal/dsl/podtemplate.go:27`) pins the run to k8s; with no k8s-agent it sits in `Queued`. | Honoured |

Two facts that shaped the design:

**Host workspaces are keyed by slot *and* job.** `claimWorkDir(wsBase, slot,
jobName)` yields `working<slot>/<job>`, so a run never inherits another job's
leftovers. What is unpredictable is the *slot*: each slot runs an independent
claim loop (`runLoop`, `internal/agent/agent.go:404`) with no job-to-slot or
job-to-agent affinity. With `maxConcurrent: N` a job accumulates up to N copies
of its own workspace per agent and lands on a random one.

That is exactly the per-job workspace-copy scheme (Jenkins' `@2` suffix) that
this project explicitly rejects: unbounded disk growth plus unpredictable cold
starts. It is already shipped on host agents — implicitly, and without a name.

**`orLocks` resolves before claim.** Locks are taken when the scheduler moves a
run `Pending` → `Queued` (`tryQueueRun`, `internal/store/postgres.go`), before
any agent claims it, and the winning value is merged into the run's parameters
as `<NAME>_LOCK_VALUE`. Locks are global to the controller.

## 4. Design

### 4.1 The default workspace is ephemeral everywhere

A workspace with no volume declared is freshly created for each run and
discarded afterwards — on host agents and k8s-agents, for `native: true` and
`native: false` alike. This matches today's k8s `emptyDir` semantics and makes
the host agent's slot lottery disappear.

Host agents clean the workspace **both before and after** the run. The
before-run clean is the guarantee: a run always starts empty, even if the agent
died mid-run last time. The after-run clean is disk reclamation, and it is
best-effort — a failure is a warning, not a failed run, because the guarantee
does not depend on it. Before-run alone would hold the previous run's output
until that job's next run, which is not what `emptyDir` does and is the whole
point of calling this ephemeral. `prepareWorkspace`
(`internal/agent/workspace.go:92`) already implements before-run cleaning and
the root-owned-leftover fallback; this change makes that path unconditional
rather than opt-in and adds the after-run sweep.

This is a breaking change to host-agent behaviour. unified-cd has no users yet,
so no compatibility shim is provided. A migration guide is still required
(§12).

**The unconditional clean must never reach a volume.** `prepareWorkspace` does
two things besides cleaning: it writes `modeMarkerFile` (`.ucd-mode`) into the
workspace, and it `RemoveAll`s the workspace when the recorded native/isolated
mode flips. Applied to a workspace backed by a volume, a mode flip on the
Unity job would delete 200GB, which is an anti-goal (§2), and the marker file
would be written into the user's checkout. Therefore:

- The mode marker for a volume-backed workspace lives **outside** the volume,
  in the agent's own state directory, keyed by volume name.
- A mode flip on a volume-backed workspace **fails the run** with an error
  naming the volume and the two modes. It does not wipe. The operator clears
  the volume deliberately (`cleanWorkspace`, §4.10) or routes the job to a
  different volume. The original reason for the reset — root-owned leftovers
  from a rootful container breaking a later native run — is still real, so
  failing loudly is correct; silently proceeding is not.

On Kubernetes, `podTemplate.reuse: true` draws from a pod pool
(`PodPool.ClaimPod`, `internal/k8sagent/agent.go:356-373`) and already clears
the workspace between runs on a reused pod. That clearing must honour §4.1 for
the ephemeral case. A pooled pod cannot carry per-run volume sets, because the
pod's volumes are fixed when it is created and successive runs need different
ones, so **`volumes` combined with `podTemplate.reuse: true` is rejected at
apply time**. Widening the pool to key on volume set is deliberately out of
scope.

### 4.2 Named volumes are owned by the agent

A volume is declared in the **agent's** configuration, not in a job. The agent
creates the backing store, enforces exclusion, and owns the lifecycle. A host
agent is one machine; a k8s-agent is a "virtual agent". A volume name exists
once per agent — two agents declaring the same name hold two independent
copies.

The controller holds no volume state.

### 4.3 Agent configuration

The agent config file (`unified-agent.yaml`, `internal/config/agent.go`) gains
a `volumes` list. A list of structs has precedent in the controller config
(`kubernetesClusters` and friends, `internal/config/controller.go:16`). This
field is file-only; it has no flag or env-var form.

Host agent:

```yaml
volumes:
  - name: unity-main
    mode: exclusive
    path: /var/lib/unified-cd/volumes/unity-main   # optional
    size: 200Gi                                    # advisory
  - name: go-cache
    mode: shared
```

`path` defaults to `<workspaceDir>/volumes/<name>`. `size` is advisory: the
agent measures usage and warns on overrun but does not enforce it, because
per-directory quotas are not portable without root.

**No volume is ever swept by the workspace GC.** `gcWorkspaces`
(`internal/agent/workspace_gc.go:46`) globs `wsBase/working*` only, so the
default volume path is already outside its reach — but by the spelling of a
glob, not by intent. The volume manager therefore refuses any resolved `path`
that is, or sits under, a `working*` directory, and a test pins that the
retention sweep cannot reach a volume. A volume deleted because it went unused
for `workspaceRetentionDays` would destroy exactly the data the operator
declared in order to keep.

**On a host agent, an isolated run's volume contents end up owned by root**
when the container runtime is rootful. Three consequences: `cleanWorkspace` on
such a volume must fall back to the throwaway-container cleanup that
`containerCleanup` (`internal/agent/workspace.go`) already provides for the
workspace; the advisory usage measurement must tolerate permission errors and
warn rather than fail the run; and an operator reclaiming the directory by hand
needs root. The last one belongs in the operations documentation.

k8s-agent:

```yaml
volumes:
  - name: unity-main
    mode: exclusive
    size: 200Gi            # required
    storageClassName: gp3  # optional
```

The k8s-agent creates an ordinary (non-ephemeral) PVC per volume via dynamic
provisioning, named with the agent name as a prefix so that several k8s-agents
in one namespace stay independent. `mode: exclusive` requests RWO;
`mode: shared` requests RWX, which only succeeds if the operator's
StorageClass supports it.

The split is deliberate: `name` and `mode` are the **contract**, visible to
jobs and identical on both agent types. `path`, `size`, and `storageClassName`
are the **implementation**, operator-only, and may differ.

### 4.4 Job-facing contract

A job names a volume and where it should appear.

```yaml
spec:
  volumes:
    - name: unity-main
      mountPath: "."              # the workspace root
    - name: go-cache
      mountPath: .ucd-cache/go
```

There is no `spec.workspace.volume` shorthand. `WorkspaceConfig` is reachable
only under `spec.podTemplate.workspace`, and `spec.native: true` is
incompatible with `spec.podTemplate` (`internal/dsl/parse.go:206`), so a
shorthand there could never serve a native job — which is the case that needs
it most. `mountPath: "."` is the only spelling, and it reads the same on every
agent type.

`name` may be a list of candidates. The agent picks a free one:

```yaml
spec:
  volumes:
    - name: [go-cache-1, go-cache-2, go-cache-3]
      mountPath: .ucd-cache/go
```

`name` supports template expansion, as volume names must be composable with
`vars` and parameters.

An entry may carry an `as:` alias, which is the stable handle a step uses to
reach the volume (§4.8). It is required whenever `name` is a list or a
template, because the chosen name is not known when the job is written:

```yaml
spec:
  volumes:
    - name: [go-cache-1, go-cache-2, go-cache-3]
      as: go-cache
      mountPath: .ucd-cache/go
```

When `name` is a single literal, `as` defaults to it.

**Where names are resolved.** Template expansion happens on the controller when
the scheduler moves the run `Pending` → `Queued`, alongside the existing
`dsl.ExpandConcurrency`. The controller therefore knows the concrete candidate
set before any agent claims, and can filter agents by it. Expanding on the
agent instead would leave the controller unable to route the run at all.

This makes capability matching an AND of ORs: the run needs, for *every*
entry, *some* candidate the agent advertises. `dsl.RequiredCaps` produces a flat
AND-set today and has to grow that shape.

An unknown volume name is therefore **not** an apply-time error — the
controller holds no volume state (§4.2) and cannot know which names exist. It
surfaces as the run staying `Queued` with a schedulability reason naming the
volume. The apply-time check is limited to syntax: a missing `as` where one is
required, a malformed name, a duplicate mount path.

**Volume selection does not use `orLocks`.** The handoff proposal was to select
with `orLocks` and expand `{{ .Params.<NAME>_LOCK_VALUE }}` into the volume
name. That under-counts: `orLocks` locks are global to the controller while
volumes are per-agent, so two agents each holding `a` and `b` offer four copies
but `orLocks` caps concurrency at two. Since the agent already tracks volume
occupancy (§4.7), the agent is the right place to choose. `orLocks` remains for
resources that genuinely are fleet-wide — licence seats, physical devices.

### 4.5 Modes

`exclusive` — at most one run per agent may hold the volume at a time.
Kubernetes RWO is a *node*-level constraint, not a pod-level one, so access
mode is not relied upon for exclusion; unified-cd enforces it. Example:
Unity's `Library/`.

`shared` — several runs may hold it concurrently. unified-cd makes **no**
write-safety guarantee; concurrent-write correctness is the job's
responsibility. This is sufficient in practice for the intended uses: Go's
build cache and module cache are documented as safe for concurrent use by
multiple processes, and git and git-LFS object stores are content-addressed and
written atomically.

Operator guidance to document: on Kubernetes, `shared` requires an RWX-capable
StorageClass, which in practice means a network filesystem (NFS, CephFS, EFS,
Azure Files, Longhorn). Those are slow for many-small-file workloads — exactly
what a Go module cache or a Unity `Library/` is. Where RWX is unavailable or
too slow, declaring several `exclusive` volumes and letting the agent pick a
free one (§4.4) gives the same parallelism without RWX.

### 4.6 Mount paths

`mountPath` is relative to the workspace. `"."` is the workspace root.

A relative, non-root `mountPath` on a `native: true` run is a symlink inside
the workspace, created before the run and removed after it. Because the
workspace is per-run, nothing is global about it, and concurrent native runs do
not contend (unlike the absolute case below).

**Absolute `mountPath` is allowed**, because many toolchains keep their caches
outside the workspace and some (`GOCACHE`) reject relative paths outright. On
Kubernetes and in host-agent isolated jobs an absolute path is a container
path: well-defined, portable between the two, and with no effect on the host.

On `native: true` runs there is no container. An absolute path is realised as a
symlink at that host path pointing into the volume, because a bind mount is
unavailable (root-only on Linux, absent on macOS). Three rules follow:

1. **Never clobber.** If the target path exists and is not a symlink this agent
   created, the run fails with a clear error. unified-cd does not move or
   delete what it did not create.
2. **An absolute path on a native run is an agent-wide exclusive resource.**
   The host path is global to the machine, so two concurrent native runs cannot
   point it at two different volumes. The agent treats the absolute path like
   an occupied exclusive volume and does not claim a second run that needs it
   (§4.7). Runs wait; they do not fail. A consequence worth documenting: the
   candidate-list trick in §4.4 does not produce parallelism on native runs
   when `mountPath` is absolute, only when it is relative.
3. The symlink is removed when the run ends, and stale symlinks are swept at
   agent startup.

This is an explicit exception to an existing invariant. `ContainWithinOS` and
`ContainWithinSlash` (`internal/agent/pathguard.go`) reject absolute paths for
artifact and cache paths, precisely to stop a crafted path from escaping the
workspace. Volumes differ because the operator declares them and the job can
only name one the operator already created; the escape is bounded by the
agent's own configuration. The reasoning must be stated in the code and in the
docs, not left implicit, and volume mount paths must not be routed through the
existing guards.

A native job can already run arbitrary commands as the agent user, so an
absolute mount path grants it no privilege it did not have. No operator opt-in
is required.

### 4.7 Dispatch

- The agent advertises its volume names as capabilities at registration, the
  same channel `dsl.RequiredCaps` already feeds.
- A run that declares `volumes` requires an agent advertising at least one
  candidate from every entry's `name` set.
- On claim, the agent reports the exclusive volumes and native absolute mount
  paths it is currently holding. The controller filters out runs that need
  them.

The claim-time report is an **optimisation**, not the correctness boundary: it
stops an agent from taking a run it would then sit on while occupying a
concurrency slot. Correctness lives in the agent's reservation, taken after the
run arrives, and the reservation must be atomic across slots.

**Losing the race.** Two slots on one agent can both report a volume free before
either reserves it. There is no controller endpoint that returns a claimed run
to `Queued`, and adding one is new controller, store, and run-state work that
this project does not take on. The loser therefore **waits locally** for the
volume, holding its slot. The cost is bounded and the case is rare, because the
controller-side filter prevents the common collision; the race window is one
claim round-trip. A release endpoint is the cleaner answer and is deliberately
deferred — record it as a follow-up, and revisit if the held slot shows up in
practice.

**Where occupancy lives differs by agent type.** Nothing prevents two agent
processes from registering under one agent ID: `agents.id`
(`internal/store/migrations/001_init.up.sql:18`) is one row per ID, which two
processes simply upsert in turn. A host agent is one machine and in-process
state is authoritative there; a lock file inside the volume directory is a
diagnostic that warrants a warning, not an authority. A k8s-agent can have two
processes live at once during a rollout, so in-process state is **not**
sufficient: the k8s-agent derives occupancy from the live claim pods that mount
each PVC, labelled by volume. That is the same list `PodPool.Restore`
(`internal/k8sagent/agent.go:99`) already walks at startup, so it is both
replica-safe and restart-safe.

### 4.8 Environment

The agent exposes each mounted volume's absolute path to steps as
`UNIFIED_VOLUME_<ALIAS>`, following the existing `UNIFIED_WORKSPACE` /
`UNIFIED_AGENT_OS` convention (`EnvWorkspace`, `internal/agent/stepenv.go:114`).
`<ALIAS>` is the entry's `as` value (§4.4) upper-cased with `-` replaced by
`_` — **not** the resolved volume name, which a job using a candidate list or a
template cannot predict. This is how a job points a tool at a volume it mounted
at a relative path:

```yaml
steps:
  - run: |
      export GOCACHE="$UNIFIED_VOLUME_GO_CACHE/build"
      export GOMODCACHE="$UNIFIED_VOLUME_GO_CACHE/mod"
      go build ./...
```

`internal/dsl/vars_parse.go` carries a hand-maintained list of synthesised
variable names (`UNIFIED_WORKSPACE`, `UNIFIED_AGENT_OS`, …). `UNIFIED_VOLUME_*`
is a prefix, not a fixed name, so it needs prefix handling there rather than a
new list entry. Hand-maintained lists that must track a second source are this
repository's most-repeated defect class; add a test that fails when the two
disagree.

### 4.9 Validation rules settled here

These change the semantics rather than the wording, so they are decided in the
design. Everything else about validation is the builder's (§11).

- **`mode: shared` at `mountPath: "."` is rejected** — but by the *agent*, at
  mount time, not at apply time. A shared workspace root means several runs
  checking out over each other with no exclusion, which defeats the point of
  persisting a workspace. It cannot be an apply-time rule because `mode` lives
  in the agent's config and the job never sees it; the same job YAML is valid
  against an agent that declares the volume `exclusive`.
- **`mountPath: "."` may be combined with other entries**, as long as no two
  mount paths collide or nest. A persistent workspace plus a shared cache
  subdirectory is a legitimate shape.
- **`volumes` is rejected on a detached run** (`spec.detached`). Detached runs
  get a per-run workspace that is deleted when they finish
  (`detachedWorkDir`, `internal/agent/workspace.go:78`); a volume contradicts
  that lifecycle.
- **`volumes` with `podTemplate.reuse: true` is rejected** (§4.1).
- **`as` is required** when `name` is a list or contains a template (§4.4).

### 4.10 `cleanWorkspace`

`podTemplate.cleanWorkspace: true` wipes whatever the workspace is — including a
volume mounted at `"."`. This is the necessary escape hatch for a poisoned
persistent workspace, and it is the **only** path by which unified-cd destroys
volume data on purpose, so it must be called out in the volume documentation and
not only under `cleanWorkspace`.

`spec.cleanWorkspace` is **added by this project**. Before it, `CleanWorkspace`
lived on `PodTemplate` only (`internal/dsl/types.go:542`), and
`internal/dsl/parse.go:204-207` rejects `spec.native: true` together with
`spec.podTemplate` — so a native job could not reach the only escape hatch
there was. That is precisely the Unity configuration this project exists for: a
`native: true` macOS build with its checkout and `Library/` on a persistent
volume. Its only remedy would have been an operator deleting tens of gigabytes
by hand on the agent host. The field also belongs at the spec level on its own
merits, because it is a workspace concern and `native` lives there too, while
`podTemplate` is the Kubernetes-flavoured corner. Both spellings are valid and
`clean` is the OR of the two.

**The agent-level `--clean-workspace` flag does not wipe a volume**, and after
§4.1 it does nothing at all. Its purpose was to stop a job inheriting another
run's leftovers, which the unconditional cleaning of every ordinary workspace
now guarantees — while applied to a volume it would destroy, on every run, the
data the job's own config declared as the thing to keep. An operator setting a
fleet-wide hygiene default has not knowingly agreed to that; a job asking for
`cleanWorkspace` has. The flag is kept so existing agent configs still start,
its help text says it has no effect, and removing it is a follow-up rather than
part of this project.

## 5. `workspace.pvc` is removed

`WorkspaceConfig.PVC` (`internal/dsl/types.go:544-553`) is deleted, both the
`claimName` form and the ephemeral form. It overlaps named volumes, means
something different on each agent type, and keeping two mechanisms for one job
has no payoff with no users to carry. `WorkspaceConfig.MountPath` stays.

This is the mechanism Argo Workflows offers (a hand-created PVC referenced by
name, with a separate `synchronization` semaphore to stop runs colliding on
it). Its two problems are the ones named above: the job YAML carries a
cluster-specific resource name, and exclusion lives in a second mechanism that
must be kept in step by hand. Tekton's `workspaces` — a name and a mount path
in the Task, bound to concrete storage at run time — is the closer precedent
for §4.3's two-layer split, and GitLab Runner's `builds_dir`/`cache_dir` is the
closer precedent for agent ownership.

## 6. Decisions taken during design

Recorded as decisions, not as open questions. Each was made in this design and
should be revisited by the builder only with a reason.

1. **Dropping a volume from the agent config does not delete data.** The agent
   stops advertising it and logs the orphan. PVCs are left in place. Removing a
   line from a config file must not destroy tens of GB.
2. **No agent affinity.** When several host agents declare the same volume
   name, a run goes to any agent with a free copy; the agent that ran the job
   last is not preferred. In steady state every copy stays warm, so the lottery
   only hurts when copies outnumber the workload — and affinity would require
   the controller to hold volume state, which §4.2 rules out.
3. **Host agents clean before AND after the run**; the after-run sweep is
   best-effort (§4.1).
4. **`cleanWorkspace` wipes volume contents** (§4.10).
5. **Host-agent `size` is advisory** (§4.3).
6. **Absolute mount paths need no operator opt-in** (§4.6).
7. **A native/isolated mode flip on a volume fails the run; it never wipes**
   (§4.1).
8. **Volume names resolve on the controller at queue time**, so capability
   matching becomes an AND of ORs and an unknown name is an unschedulable run,
   not an apply error (§4.4).
9. **The claim race loser waits locally** rather than returning the run; no
   release endpoint is built (§4.7).
10. **The k8s-agent derives occupancy from live pods, not process state**, so
    two replicas under one agent ID stay correct (§4.7).

Items 7–10 came out of the builder check and were settled in the design rather
than referred back.

## 7. Restart recovery

A host agent treats all of its volumes as free at startup: it has lost every
run it was executing, and the controller's lost-claim reconcile handles those
runs. A stale lock file inside the volume directory is not treated as
authoritative.

A k8s-agent is different — its claim pods outlive a restart of the agent
process, and it already adopts them: `PodPool.Restore`
(`internal/k8sagent/agent.go:99`) runs at startup. Occupancy is read from the
live pods that mount each volume's PVC (§4.7), so restart needs no separate
recovery path — the pod list *is* the state. The builder should confirm that
`Restore` covers non-pooled per-run claim pods as well as pooled ones, and
extend it if it does not.

## 8. Worked examples

Unity, native, whole workspace persisted:

```yaml
# unified-agent.yaml (macOS host)
volumes:
  - name: unity-main
    mode: exclusive
    path: /var/lib/unified-cd/volumes/unity-main
    size: 200Gi
```

```yaml
spec:
  native: true
  volumes:
    - name: unity-main
      mountPath: "."       # checkout and Library/ both persist; pull, not clone
```

Go, ephemeral workspace, shared cache:

```yaml
# unified-agent.yaml
volumes:
  - name: go-cache
    mode: shared
```

```yaml
spec:
  volumes:
    - name: go-cache
      mountPath: .ucd-cache/go
  steps:
    - run: |
        export GOCACHE="$UNIFIED_VOLUME_GO_CACHE/build"
        export GOMODCACHE="$UNIFIED_VOLUME_GO_CACHE/mod"
        go build ./...
```

Go on Kubernetes without RWX — three exclusive copies, agent picks a free one:

```yaml
spec:
  volumes:
    - name: [go-cache-1, go-cache-2, go-cache-3]
      as: go-cache
      mountPath: .ucd-cache/go
```

## 9. Out of scope

Three tiers were agreed; this project is tier 1 only.

1. **Behavioural divergence** — workspace persistence and `workspace.pvc`.
   This project.
2. **Host-reproducible k8s features** — additional `emptyDir` volumes in
   `podTemplate`, parts of `securityContext`, ignoring `resources.requests`.
   A later project. Note that an ephemeral workspace already gives a job
   scratch space; the genuinely missing case is `medium: Memory` (tmpfs).
3. **Inherently Kubernetes-only** — `nodeSelector`, `affinity`, `tolerations`,
   `serviceAccountName`, `env.valueFrom`, existing-PVC `claimName`. These keep
   pinning the run to Kubernetes. Ideally a job would be visibly marked
   "Kubernetes-only" at apply time.

## 10. Separate defect (fix independently)

`buildWorkspaceVolume` (`internal/k8sagent/podbuilder.go:650-656`) builds the
ephemeral PVC with:

```go
sc := wsCfg.PVC.StorageClassName
// ...
StorageClassName: &sc,
```

The pointer is never nil, so an unset `storageClassName` yields
`storageClassName: ""`, which in Kubernetes means *do not use a StorageClass* —
the cluster default is not applied and the PVC stays `Pending` unless a
matching static PV exists. The field must be left nil when the value is empty.

This code is removed by §5, so the fix is only worth landing separately if §5
does not ship first. Confirm against a cluster either way, and add a
troubleshooting entry with the grep-able symptom.

## 11. Left to the builder

- The wire format for advertising volumes and for the claim-time occupancy
  report, and the shape of the AND-of-ORs eligibility filter in the controller
  (§4.4).
- The local reservation primitive, atomic across slots, and how a waiting
  loser is woken (§4.7).
- PVC naming and creation in the k8s-agent, and whether `PodPool.Restore`
  already covers non-pooled claim pods (§7).
- Whether host-agent volume usage measurement runs per-run or on a timer, and
  what the warning looks like.
- Error strings for every rule in §4.9, plus: absolute `mountPath` that lands
  inside the workspace, nested mount paths, a candidate list where only some
  entries are known to the agent.
- Windows: `os.Symlink` requires privilege; fall back to a directory junction.
  Decide whether an unprivileged Windows agent without junction support
  refuses absolute native mount paths at startup or at run time.
- A follow-up, not this project: a controller endpoint that returns a claimed
  run to `Queued`, which would replace the local wait in §4.7.

## 12. Documentation obligations

Per `AGENTS.md`, this is not done until all of the following agree with the
code. The `kind: JobTemplate` migration is the cautionary precedent.

- `docs/user-guide/writing-jobs/job-structure.md`, `docs/user-guide/resources/job.md`,
  `docs/operator-manual/agents.md`, `docs/reference/configuration.md`,
  `docs/operator-manual/kubernetes-integration.md`.
- Narrative docs: `docs/getting-started/quickstart.md` (does it still work end
  to end?), `docs/troubleshooting/controller-and-database.md` (new failure
  modes with exact error strings: occupied volume, clobber refusal, RWX
  unavailable, Windows symlink), `docs/operator-manual/operations.md` (volume
  disk hygiene is a new operator responsibility).
- Generated: no new root kind, so `cmd/schemagen/main.go` roots and
  `cmd/docgen/main.go` rootKinds are unchanged — but run `go generate ./...`
  and commit the `docs/reference/field-reference.md` and
  `schemas/unified-cd.schema.json` diff.
- `examples/**` and `templates/**`: every `workspace.pvc` use must go. Re-run
  `go test ./internal/dsl/ -run 'Templates|Examples'`.
- A migration guide under `docs/operator-manual/migrations/` covering both
  breaking changes: the workspace is no longer persistent by default, and
  `workspace.pvc` is gone. Before/after table plus the exact validation errors.
- `README.md` feature summary.
- Final sweep: `grep -rn "workspace.pvc\|claimName" docs/ examples/ templates/ README.md`
  and `grep -rn "pvc:" examples/ templates/` — zero hits outside historical
  specs and plans.
