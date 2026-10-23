# Disk-headroom gating on both agents — Design

Date: 2026-10-23
Status: Draft for review (design only; no code written)

Builds on v0.10.0: agent storage metrics (PR #177, rulings in
`docs/superpowers/decisions/2026-10-21-agent-storage-metrics-rulings.md`) and agent-owned named
volumes (PR #176, `docs/superpowers/specs/2026-10-20-agent-named-volumes-design.md`).

## 1. Purpose

Jobs fail because storage runs out. v0.10.0 made the headroom *visible* (Prometheus series). It did
not make the agents *avoid* the failure, and the two agent types protect very differently:

- Host agent: `minFreeDisk` exists but is a byte count that defaults to `0` = **disabled**, and it
  gates the workspace filesystem only.
- Kubernetes agent: nothing. The workspace is an `emptyDir` with no size limit and no
  `ephemeral-storage` request, and the claim loop never looks at a PVC's headroom.

Goal: **an agent stops claiming work it is likely to fail for lack of disk, by default, on both agent
types, and when it does (or when a pod is evicted for disk) the operator and the job author can see
why without reading source.**

## 2. Goals, non-goals, anti-goals

**Goals**

- Host: threshold expressible as a percentage; check on by default; existing byte configs keep working.
- Host: named-volume filesystems are gated too, so a full volume stops the runs that need it.
- Kubernetes (a): the workspace `emptyDir` can carry a size limit and the pod an `ephemeral-storage`
  request, so the scheduler places it on a node with room and an overrun stops the pod with a readable
  reason.
- Kubernetes (b): before claiming a run that mounts named volumes, consult the last known headroom of
  those volumes and do not claim if below threshold.
- A gated agent / run is noticeable from logs, metrics and docs.
- The same job YAML behaves the same on both agents; remaining differences are listed (section 6).

**Non-goals**

- Deleting anything to make room. Gating is never destructive (unchanged from today).
- A new job-level field. Size limits are an operator concern (named-volumes spec section 2: "Operator-only
  concerns stay out of the job definition").
- Hard enforcement on the host. The host agent cannot cap a run's disk use; it only declines to start
  the next one.
- Controller-side gating logic. The controller keeps its "holds no volume state" stance for claiming;
  the agent decides, the controller already filters on a list the agent sends.
- Per-volume thresholds (open question Q6).

**Anti-goals**

- A gate that can wedge an agent forever on a measurement failure. Every unknown fails **open** with a
  log line (as the workspace gate already does: `internal/agent/agent.go:548-553`).
- A gate that blocks unrelated runs. A full volume must not stop jobs that do not mount it.
- A gate on the heartbeat goroutine, or any inline `statfs` on a named volume (see D5).
- Silently changing what an existing byte value means.

## 3. Current behaviour (cited)

| Fact | Where |
|---|---|
| `MinFreeDisk uint64`, 0 disables, host-only | `internal/agent/agent.go:104-107`; `internal/config/agent.go:119-122` |
| Env parsed with `ParseUint`; a malformed value is **silently ignored** | `internal/config/agent.go:225-229` |
| File value merged only when `!= 0`, so an explicit `minFreeDisk: 0` cannot override a non-zero default | `internal/config/agent.go:311-313` |
| Flag is `Uint64`, default from the merged config | `cmd/unified-cd-agent/main.go:115, 242` |
| Gate: per slot, inline `statfs(wsBase)` every loop, below -> log WARN, `blockedSlots++`, sleep 2s | `internal/agent/agent.go:546-565`; `internal/agent/diskfree.go:5` |
| `statfs` failure or zero total -> log + claim anyway | `internal/agent/agent.go:548-553`; rulings #11 |
| Only `wsBase` is gated; volumes are not (`storagePaths` merely *measures* them) | `internal/agent/agent.go:176-185` |
| Detached slots run the same gate | `internal/agent/agent.go:546` (loop is shared) |
| Sampler: background statfs per path, 15s default interval, reading dropped after 2 intervals, one in-flight per path | `internal/agent/storage_sampler.go:12-18, 101-112`; rulings #8 |
| Heartbeat reports `MinFreeDiskBytes` (non-nil, 0 = disabled) and `ClaimBlockedByDisk` | `internal/agent/agent.go:202-209` |
| Claim body carries `BusyVolumes`; the controller excludes runs whose every candidate for some volume entry is busy or undeclared | `internal/api/types.go:152-159`; `internal/controller/api_agent.go:237`; `internal/store/postgres.go:866-885` |
| Host `Busy()` = held exclusive volumes + reserved absolute paths | `internal/agent/volumes.go:729-751` |
| `ListUnclaimableQueuedRuns` / schedulability is label/cap/declaration-only, "whether a volume is currently free is the agent's business" | `internal/controller/schedulability.go:23-27` |
| K8s workspace: `buildWorkspaceVolume()` = `emptyDir{}` (no `SizeLimit`); `injectWorkspace` **replaces** any user volume named `workspace` | `internal/k8sagent/podbuilder.go:663-672, 582-597` |
| No `ephemeral-storage` / `SizeLimit` anywhere in `internal/k8sagent` | grep, 2026-10-23 |
| `PodTemplateNeedsKubernetes`: any limits key other than string `cpu`/`memory`, incl. `ephemeral-storage`, pins the job to a Kubernetes agent | `internal/dsl/podtemplate.go:101-122`; `docs/operator-manual/kubernetes-integration.md:58-70` |
| K8s volume headroom is measured in-pod (`ucd-sh statfs`) before and after each run, cached in memory, never cleared; a restart loses it | `internal/k8sagent/volumes.go:813-885`; `internal/k8sagent/agent.go:540-560`; rulings #9 |
| K8s claim request builds `BusyVolumes` from live pods, degrading to empty on error | `internal/k8sagent/volumes.go:499-521` |
| Pod startup failures are explained (`podStartDiagnostic`); a pod that dies **after** Running is not: `execArgv` returns `(1, streamErr)` and the step fails with a raw stream error | `internal/k8sagent/podmanager.go:106-150`; `internal/k8sagent/executor.go:57-88` |
| Metrics: `min_free_disk_bytes` ("0 = disabled"), `claim_blocked_by_disk`, per-volume free/size/measured | `internal/metrics/collector.go:50-54, 123-155` |
| Storage reading is persisted as one JSON blob in `agents.storage` | `internal/store/postgres.go:1700-1712` |

## 4. Decisions

### D1. One field, `minFreeDisk`, now a string

`minFreeDisk` accepts one of:

| Form | Meaning |
|---|---|
| `"1%"`, `"0.5%"`, `"1.5 %"` | percent of the **total size of the filesystem being checked**; `0 < p < 100` |
| `"10GiB"`, `"500MB"`, `"10Gi"`, `"5G"` | absolute bytes (IEC and SI suffixes; k8s-style `Gi`/`G` also accepted because `internal/agent/volumes.go:116` already uses `resource.ParseQuantity` for volume sizes) |
| `5368709120` / `"5368709120"` | absolute bytes, **the legacy form, unchanged meaning** |
| `0`, `"0"`, `"off"`, `"disabled"` | check disabled |
| unset | default `1%` |

Reasons:

- One field keeps flag, env, yaml and the metrics named the same, and the byte-only configs
  (`minFreeDisk: 5368709120`, `UNIFIED_AGENT_MIN_FREE_DISK=5368709120`, `--min-free-disk 5368709120`)
  parse unchanged. A separate `minFreePercent` would need a precedence rule between two knobs and a
  second flag/env/yaml triple on each agent, for no gain.
- Implementation: a new type `config.DiskThreshold{Percent float64; Bytes uint64; Set, Off bool}` with
  `UnmarshalYAML` (int **or** string scalar), `Parse(string)`, `String()`, and
  `Below(free, total uint64) bool` / `Resolve(total) uint64`. Both agents import it from
  `internal/config`.
- The flag changes from `flag.Uint64` to `flag.String` (still accepts `--min-free-disk 5368709120`).
  `--help` text changes.
- `Set` exists because of a latent bug that default-on would expose: today a file `minFreeDisk: 0`
  merges as "unset" (`internal/config/agent.go:311`). With a non-zero default, `0` in a file must mean
  "off" and win. The merge must test `Set`, not `!= 0`.
- **A malformed value is a startup error**, not silently ignored (today's env path ignores it,
  `internal/config/agent.go:225-229`). With a default-on check, silently falling back would mean an
  operator who typed `1 %%` believes they configured something. Cost: an agent with a garbage env var
  that used to start now refuses to; it previously had *no* protection while looking configured. Called
  out in the migration note.

### D2. Default: on, `1%`

Assumed per the brief, and I found no strong reason against it:

- `Bavail`/`GetDiskFreeSpaceEx` free already excludes the root reserve, so "1% of total" is 1% of
  space an unprivileged job can actually write.
- The check only declines to *start* a run; it never fails, kills or deletes. The worst outcome of a
  wrong default is a queue, with a log line and a metric explaining it.
- It fails open on every measurement problem.

Honest limits, documented rather than hidden:

- 1% is a **floor, not a safety margin**: on a 2 TB disk it is 20 GB, on a 100 GB disk 1 GB. A
  workload that writes 50 GB per run needs an absolute `minFreeDisk: 60GiB`. The docs say to set it
  from the workload, and that `1%` only catches "already nearly full".
- A host whose workspace filesystem is **already below 1% free** will stop claiming after upgrade
  (section 10).

### D3. What "percent of total" is measured against

| Check | Filesystem | Total from |
|---|---|---|
| Host workspace gate | filesystem containing `wsBase` | the same `diskStats(wsBase)` call that gives `free` |
| Host volume gate | filesystem containing each volume's resolved path (`Volumes.PathOf`) | the sampler reading for that path |
| K8s volume gate | the PVC's filesystem | the in-pod `ucd-sh statfs` reading (`FilesystemStats.TotalBytes`) |

A volume on the same filesystem as the workspace gets the same answer twice; harmless. A volume on its
own mount (the 200 GB Unity case) is judged against its own size: 1% of 200 GB = 2 GB.

One global threshold applies to the workspace and all volumes (Q6 discusses per-volume).

**Absolute threshold larger than a volume's total.** A global `10GiB` on a 2 GiB volume is
unsatisfiable. For *volumes*, when `Resolve(total) >= total` the gate is skipped for that volume and
the agent logs one WARN per volume per process ("minFreeDisk 10GiB is not smaller than volume X (2GiB);
not gating it"). The workspace keeps today's behaviour (it blocks): an operator who sets a threshold above
the workspace disk has made a mistake that should be loud, and there is only one workspace.

### D4. Host: volumes are gated too; gating a volume excludes only the runs that need it

On the host the workspace gate stays "stop claiming entirely": every run needs a workspace, so there is
no run it could still safely take.

For a volume below threshold the agent keeps claiming but tells the controller not to hand it runs that
need that volume. **No controller change and no new wire field**: the agent appends the gated volume
names to the existing `ClaimRequest.BusyVolumes` list.

Why reuse `BusyVolumes`:

- The claim SQL already does exactly the right thing: a run is excluded only if some volume entry has
  no candidate that is both advertised and not in the list (`internal/store/postgres.go:874-880`).
  A run with `name: [unity-a, unity-b]` still claims if `unity-b` is fine, and runs with no volumes
  are untouched.
- It works against an older controller, so agent and controller can upgrade in either order. A new
  field would be silently ignored by an old controller, turning the gate off with no signal.
- Its documented meaning is already "an optimisation; the agent's own reservation is what enforces
  exclusion" (`internal/api/types.go:152-159`). That wording needs one extra sentence ("or unavailable").

Races accepted: a run claimed in the instant before a volume dips is not recalled (the same as the
existing workspace gate); candidate choice in `assignLocked` is not changed to avoid a gated candidate
(Q7 notes the cheap preference tweak if wanted).

A run that needs a gated volume stays `Queued`; if no other agent declares the volume it waits until
space is freed. That is the intended behaviour, but it is visible only through metrics/logs (D8, Q5).

### D5. Host: how the gate reads headroom

- **Workspace**: unchanged mechanism, inline `statfs(wsBase)` per slot per loop. Local disk, already
  shipped, tests exist (`internal/agent/diskfree_claimgate_test.go`). Percent comes from the total in
  the same call.
- **Volumes**: read the **sampler** (`sampler.read(path)`), never `statfs` inline. A dead hard-mounted NFS
  export blocks `statfs` in the kernel uninterruptibly (rulings #8); an inline call in `runLoop` would
  wedge a claim slot (and, shared across slots, the whole claim loop) on a volume that no unrelated run
  uses. The sampler already measures every volume every 15s. Cost: the gate sees numbers up to ~30s old
  (2 intervals). Acceptable: it is a coarse floor, and the alternative is a hang.
- A volume with no fresh sample (never measured, failed, stale) is **not gated** (fail open) and logs at
  most once per transition, not every 2s.
- The slot loop computes the list once per iteration (`gatedVolumes()`: names whose fresh reading is
  below threshold) and passes `append(a.Volumes.Busy(), gated...)` into `ClaimRequest`.
- Hysteresis: none. The sampler interval already damps flapping, and a flap only causes a claim to be
  skipped or allowed one tick early.

### D6. Kubernetes (b): gate on the last known volume reading

In `claimRequest` (`internal/k8sagent/volumes.go:499-521`), after building `BusyVolumes`, append every
declared volume whose cached reading (`a.volumeStats`) is below threshold. Same mechanism and same
"only runs that need it are skipped" semantics as D4. The threshold is the agent's own `minFreeDisk`
(same grammar, default `1%`, yaml `minFreeDisk`, env `UNIFIED_K8S_MIN_FREE_DISK`); on Kubernetes it
applies to **volumes only** (there is no workspace gate, see D7 for what protects the workspace).

**Never measured** (fresh agent, restarted pod, volume not yet mounted): not gated, claim proceeds. The
alternative (refuse until measured) is a deadlock: only a run that mounts the volume can measure it.
The first run measures it pre-run, so the second claim is already informed.

**Staleness, and the lockout problem.** This is the one place the k8s design is *not* a mirror of the
host. The reading is only refreshed by a run that mounts the volume. Once a volume is gated, **no run
mounts it, so it is never re-measured, so the gate can never lift** — even after the operator frees
space or grows the PVC. A gate with no way out is worse than none. Resolution:

1. A reading **older than `volumeGateMaxAge` (default 10 minutes, constant, not configurable in v1)
   is ignored by the gate** (treated as unknown, claim allowed). The next run that mounts the volume
   then measures it pre-run and re-arms the gate if still low. Net effect: at most one run per
   10 minutes is let through a still-full volume as a probe. The exported metric and timestamp are
   unaffected by this (they keep their own staleness semantics, rulings #7/#10).
2. A probe run that starts on a still-low volume gets a run-log warning from the existing pre-run
   measurement point (`internal/k8sagent/agent.go:~551`, `warnRun`): "volume X has 1.2GiB free (0.6%),
   below minFreeDisk 1%; this run may fail for lack of space". The job author sees it in the run itself.
3. The cache is in memory, so restarting the agent also clears it (documented as the manual reset).

Replicas share PVCs but not caches; replica B can hold an older, optimistic reading after replica A
filled the volume. The gate is best-effort across replicas, the same as the existing busy list's
"optimisation, correctness is elsewhere" stance. Documented.

Why not an active probe pod: it is the only way to get a fresh reading of an idle PVC, but it costs a
pod per volume per interval, extra RBAC and scheduling load, for a gate that is already a coarse
floor. Rejected for v1.

### D7. Kubernetes (a): workspace `emptyDir` size limit + `ephemeral-storage` request

**Source of the value** (operator-only, no job field):

1. Agent config `workspaceSizeLimit` (Quantity string, e.g. `20Gi`; env `UNIFIED_K8S_WORKSPACE_SIZE_LIMIT`).
2. Per named agent template override `podTemplates.<name>.workspaceSizeLimit` (so a "big" template for
   heavy jobs does not force a global raise).

Jobs cannot set it: an inline `podTemplate` that defines its own `workspace` volume is *already* replaced
by `injectWorkspace` (`podbuilder.go:587-597`), and I keep that. A job that needs more than the
operator allows uses a named template or a named volume — which is the model the volumes spec chose.

**What is applied** (to the pod after `BuildPod`, see "podbuilder overlap" below):

- `workspace` volume: `emptyDir.sizeLimit = <limit>`. The kubelet enforces this by **evicting** the pod
  when usage exceeds it (periodic check, so a fast writer can overshoot briefly; it is not a hard quota
  unless the cluster enables fs-quota monitoring).
- Primary `job` container: `resources.requests["ephemeral-storage"] = <limit>`, **only if no container in
  the pod already requests `ephemeral-storage`**. Request only, no `limits`: the request is what the
  scheduler sums to find a node with room; adding a container *limit* would make the kubelet evict on
  total container ephemeral usage (writable layer + logs + every emptyDir), which is a different and
  surprising cap.
- Anything the operator's own pod template already says wins: an existing `ephemeral-storage` request
  or limit on any container, and an existing `sizeLimit` on `workspace` (not possible today since the
  volume is replaced, but the helper must not clobber a value if that ever changes).

**Interaction with `PodTemplateNeedsKubernetes`.** None. That function inspects the *job's* podTemplate
at schedule time; the agent-side injection happens after the run is claimed by a Kubernetes agent and
does not add anything to the job spec. A job that sets `resources.limits.ephemeral-storage` itself is
still pinned to Kubernetes exactly as documented (`kubernetes-integration.md:58-70`) and its value wins
over the injected request.

**Other pods.** The `uses:` scope pod has its own scratch `emptyDir` named `workspace`
(`internal/k8sagent/scopepod.go:29`); apply the same helper there (it is also node-ephemeral storage).
The `/.ucd` tools volume and the sidecar volumes are small and left alone.

**Default: unset (no limit, no request).** Unlike the host claim gate, an `emptyDir` limit is
destructive: a run that exceeds it is evicted mid-step. Enabling a default would break any existing
workload that quietly writes more than the chosen number to its workspace (a Unity job unpacking
packages into `/workspace` is exactly that), with no escape except a config change after the first
failures. The motivating 200 GB `Library/` lives in a named volume, which the limit does not touch, but
other heavy jobs may not. So: ship the knob, document it prominently next to `minFreeDisk`, log a one-time
INFO at startup when unset ("workspaceSizeLimit is unset: a run can fill the node's ephemeral storage"),
and revisit a default after a release (Q3). This is the one deliberate asymmetry with the host default.

**What the user sees when the limit is hit.** Today: nothing helpful. After the kubelet evicts the pod,
`ExecStep` returns `(1, streamErr)` (`executor.go:82-88`), the step fails with a raw stream error and no
mention of disk. New behaviour (this also helps with node-pressure evictions and works with no limit
set): when `ExecStep`/`ExecStepArgv` returns an infrastructure error, the backend does one
`Get pod` (bounded, 5s) and, if the pod is `Failed` with `status.reason == "Evicted"` (or any terminal
`status.reason`/`message`), wraps the error:

```
k8s: pod ucd-run-abc was evicted: Usage of EmptyDir volume "workspace" exceeds the limit "20Gi".
The workspace emptyDir is limited to 20Gi by the agent's workspaceSizeLimit; use a larger podTemplate
(podTemplates.<name>.workspaceSizeLimit), or keep large data in a named volume (spec.volumes).
```

The kubelet's own message goes first and verbatim; the unified-cd hint is appended only when the message
mentions `ephemeral`/`EmptyDir` and a limit is configured. The wrapped text becomes the step's error and
therefore appears in the run log and the run's failure reason, grep-able in troubleshooting.

### D8. How an operator or job author notices a gate

| Channel | Host workspace | Host volume | K8s volume |
|---|---|---|---|
| Agent log | existing WARN `free disk space below minimum, skipping claim` (add `thresholdBytes` and `percent` attrs) | new WARN on transition: `volume below minFreeDisk, not claiming runs that mount it` (volume, free, total, threshold), logged on entering/leaving, not every 2s | same message on transition, from `claimRequest` |
| Metric | `claim_blocked_by_disk` (existing, `1` = all claims stopped) | new `unifiedcd_agent_volume_claim_blocked{agent,agent_name,volume}` 0/1 | same new series |
| Threshold metric | `min_free_disk_bytes` now = threshold **resolved to bytes** for the workspace (`Resolve(total)`); `0` still means disabled | - | omitted (no workspace gate; a volume's threshold is derivable from `volume_filesystem_size_bytes`) |
| Run log | n/a (never claimed) | n/a | probe-run warning (D6.2) |
| Troubleshooting | existing page updated + new "run Queued because its volume is below minFreeDisk" entry | same entry | same entry + "pod evicted" entry |

`min_free_disk_bytes` semantics: today the pointer is always non-nil on a host agent and `0` means
disabled (`internal/agent/agent.go:192-209`). With a percent threshold, the byte value needs the workspace
total; if the workspace is not measurable this beat the series is **omitted** (nil) rather than
published as `0`, because `0` is the "disabled" alarm. Absent still never means "fine".

Schedulability / JobDetail banner: not in scope for v1 (Q5). `EvaluateSchedulability` deliberately holds
no volume state and the evidence for "queued because gated" lives in agent heartbeats, not in the job
spec. A follow-up could join `agents.storage` into the Queued-run reason.

### D9. New wire / API surface (small)

- `api.VolumeStats` gains `ClaimBlocked bool json:"claimBlocked,omitempty"`. Storage is stored as one JSON
  blob (`postgres.go:1700-1712`), so **no DB migration**. An old controller drops the unknown key; an
  old agent simply never sets it. No new claim-request field (D4).
- `AgentStorage.MinFreeDiskBytes` semantics clarified (above); no type change.
- `ClaimRequest.BusyVolumes` doc comment gains "or unavailable (below the agent's minFreeDisk)".

## 5. Overlap with concurrent work (`podbuilder.go`)

I deliberately avoid editing `BuildPod`, `injectWorkspace`, `resolveWorkspaceConfig` and
`buildWorkspaceVolume`, which another agent is changing (lone `workspace.mountPath`). The size limit is
applied by a **new function in a new file**, `internal/k8sagent/workspace_limit.go`:

```go
func applyWorkspaceLimit(pod *corev1.Pod, limit resource.Quantity)
```

called from the single place that creates run pods (`buildAndCreate`, `internal/k8sagent/volumes.go` /
`agent.go`) **after** `BuildPod` returns, and from `createScopePod`. It finds the volume named
`workspace` and the container named `job` (`primaryContainerName`) by name, so it does not depend on how
`BuildPod` computed the mount path. The only coupling is the literal names `workspace` and `job`, which
`podbuilder.go` already treats as fixed. Conflict surface with the other agent: one added call line in
`buildAndCreate`, none inside `podbuilder.go`. A host `securityContext` design touches the host claim-pod
builder (`internal/agent/claim_pod.go`), not these files; this design touches only
`internal/agent/agent.go` (claim loop, storageReading) and `internal/agent/volumes.go` (a read-only
helper), which that design is unlikely to need, but the `Agent` struct and `runLoop` are shared hot
files, so the host tasks should merge after, or rebase onto, any change there.

## 6. Parity: what is the same, what is not

| Aspect | Host agent | Kubernetes agent |
|---|---|---|
| Threshold grammar / default | `minFreeDisk`, `1%` | `minFreeDisk`, `1%` (same type) |
| Named volume below threshold -> runs needing it not claimed, others unaffected | yes (sampler reading, <=30s old) | yes (last in-run reading) |
| Workspace gated at claim time | **yes** (workspace fs, stops all claiming) | **no** (nothing to measure; the pod's `emptyDir` is protected by D7 instead) |
| Workspace hard cap | no (cannot cap a native or container run) | optional `workspaceSizeLimit`: eviction + clear message; **default unset** |
| Freshness of a volume reading | continuous (15s) | only as of the last run that mounted it; ignored after 10 min so a gated volume can be probed again |
| Unmeasured volume | not gated | not gated |
| Recovery after operator frees space | automatic, within ~30s | automatic after <=10 min (next probe run), or immediately on agent restart |
| A run claimed just before the dip | still runs | still runs |
| Replicas | n/a | caches are per replica; best effort |
| Metric for the gate | `claim_blocked_by_disk`, `volume_claim_blocked` | `volume_claim_blocked` only (omits `claim_blocked_by_disk` / `min_free_disk_bytes`, as in v0.10.0) |
| Job YAML | identical: `spec.volumes` and nothing disk-related | identical; no new fields on either |

The deliberate remaining differences are: (1) the workspace is gated on the host and capped on
Kubernetes, because that is the only protection each platform can offer; (2) a Kubernetes volume
reading is lazier; (3) the default of the Kubernetes workspace cap is off.

## 7. Open questions (each with a recommendation)

**Q1. Default `1%`: confirm?** Recommend yes, with the "floor, not margin" wording in the docs and an
example of an absolute value for the Unity workload (`minFreeDisk: 60GiB`).

**Q2. Should a malformed `minFreeDisk` env value fail startup?** Today it is ignored
(`config/agent.go:225-229`). Recommend **fail startup**: with a default-on check, silent fallback hides
a typo. Alternative: log ERROR and use the default; rejected because that is the same invisibility.

**Q3. Kubernetes `workspaceSizeLimit` default: unset (recommended) or e.g. `20Gi`?** Recommend unset
now + startup INFO line + docs, and reconsider after one release based on eviction reports. Setting a
default is a behaviour change that kills running work, unlike every other change in this design.

**Q4. K8s staleness window for ignoring a reading: 10 min constant (recommended) or configurable?**
Recommend constant for v1; the number only trades probe frequency against how fast an operator's fix
takes effect, and restarting the agent is the immediate escape hatch. Make it `volumeGateMaxAge` yaml
only if someone asks.

**Q5. Surface "Queued because gated" in the controller (schedulability reason / JobDetail banner)?**
Recommend **defer**. It needs the controller to join `agents.storage` into the queued-run reason and
reverses the "controller holds no volume state" line in `schedulability.go`. v1 relies on metrics
(`volume_claim_blocked`), the agent log and the troubleshooting page. Revisit if users report
"Queued with no reason" in practice.

**Q6. Per-volume `minFreeDisk` in the agent's `volumes:` entries?** Recommend **no** in v1. One global
threshold plus the rule "skip gating a volume whose size is not above the threshold" (D3) covers mixed
sizes if the operator uses percent. Add `volumes[].minFreeDisk` later without breaking anything (it would
override the global).

**Q7. Make the agent prefer a non-gated candidate in `assignLocked`/`ChooseVolumes`?** Recommend
**yes only if cheap**: wrap `free()` so gated names are treated as "free but last resort". If it complicates
the search (`maxVolumeSearchNodes` bound, `volumes.go:489`), skip it; the controller already steers the
claim away when all candidates are gated, and the race is narrow.

**Q8. Percent granularity / rounding: `Resolve` uses `uint64(float64(total) * p / 100)`.** Recommend
that, rounded down, with a unit test at 1% of a 1 TiB total; precision loss only matters beyond 2^53
bytes (8 PiB).

**Q9. Should `workspaceSizeLimit` also be a `podTemplates.<name>` field that jobs can pick by name only,
or should jobs be allowed `podTemplate.workspace.sizeLimit`?** Recommend operator-only (named template).
A job field would put an operator concern in the job definition and would not be portable to the host.

## 8. Risks

- Turning the host check on can leave a nearly-full agent idle after upgrade. Mitigated by startup log
  of the effective threshold, the existing blocked metric, and the migration note (section 10).
- The Kubernetes gate uses data that may be minutes old and per-replica; documented as best effort.
- Eviction messaging relies on the pod still existing when the step errors; if the pod was already
  deleted the wrap falls back to the original error (no worse than today).
- `emptyDir.sizeLimit` is eviction-based and can overshoot; the docs must not call it a quota.

## 9. Documentation to change

`docs/reference/configuration.md` (flag table, env table, yaml example, the `minFreeDisk` paragraph, k8s
config fields incl. `minFreeDisk` and `workspaceSizeLimit`); `docs/operator-manual/operations.md`
(Workspace hygiene bullet, "Watching agent disk headroom" including the "default is now 1%" trap text and
the PromQL examples that assume `0 = disabled`); `docs/operator-manual/kubernetes-integration.md`
(workspace section ~361, volumes section ~390-430, resource requests ~58-70, new "Disk limits and
headroom"); `docs/operator-manual/agents.md` (~687 workspace section); `docs/user-guide/writing-jobs/volumes.md`
(a job author's view: "a run waits when its volume is full"); `docs/troubleshooting/*`; a migration note
`docs/operator-manual/migrations/2026-10-23-disk-gating-defaults.md`; `examples/config/agent.yaml`
(the `minFreeDisk` comment is now false); the k8s agent example config / manifests if they list agent
config; `README.md` only if it advertises the setting (it does not today). `TODO.md` has no entry for
this work; nothing to add or remove (verify with `grep -n -i disk TODO.md`). Generated artifacts
(`field-reference.md`, JSON schema) are for the Job DSL and are unaffected: no DSL field is added.

## 10. Migration impact of default-on

- Host agents with no `minFreeDisk` start enforcing `1%` of the workspace filesystem on upgrade. An agent
  already below 1% free stops claiming immediately; its runs were about to fail. The agent logs the
  effective threshold once at startup (new INFO) and the existing WARN names it per skip.
- Agents with `minFreeDisk: <bytes>` behave exactly as before.
- Agents with `minFreeDisk: 0` (explicit) stay disabled (this requires the `Set` fix in D1).
- Monitoring that treats `unifiedcd_agent_min_free_disk_bytes == 0` as "unprotected" keeps working; the
  value is now non-zero for default agents and equals the resolved bytes.
- Host agents with named volumes gain volume gating: a volume that is already below 1% free will keep
  runs that mount it `Queued`. Same for Kubernetes agents once they have a reading.
- A garbage `UNIFIED_AGENT_MIN_FREE_DISK` stops the agent starting (Q2).
- Mixed-version fleets: old agents ignore everything new; an old controller ignores `claimBlocked`; the
  gate itself needs no controller support (D4).
