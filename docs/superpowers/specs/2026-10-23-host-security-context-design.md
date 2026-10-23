# Host-agent `securityContext` — Design

Date: 2026-10-23
Status: Draft (design); open questions in §13 need answers before the plan is executed

Line references are to the tree as of 2026-10-23 (main). "Upstream k8s" means
kubelet/containerd behaviour that this repo does not vendor; every such claim is
listed in §11 with the test that verifies it.

## 1. Purpose

The project goal (`docs/superpowers/specs/2026-10-20-agent-named-volumes-design.md` §1):

> An isolated job (`native: false`) behaves the same way on any agent.

That spec put "parts of `securityContext`" in tier 2, "host-reproducible k8s
features, a later project" (§9). This is that project.

Today, if a job sets any `securityContext`, it can only run on Kubernetes. The routing
predicate treats any pod-level key other than `containers` as host-unsupported
(`internal/dsl/podtemplate.go:40-46`, comment at :43 "securityContext,
serviceAccountName, ... — all host-unsupported"). It does the same for any container
field outside `HostSupportedContainerFields` (`podtemplate.go:13-16`, :53-56). Such a
run requires the `pod` capability (`internal/dsl/capabilities.go:40-41`), and only the
k8s-agent advertises that (`internal/k8sagent/agent.go:92`).

The host runtime layer has no notion of a user at all. `crt.CreateSpec`
(`internal/runtime/runtime.go:28-59`) has no user or group field, and
`ociCLI.createArgs` (`internal/runtime/ocicli.go:101-144`) never emits `--user`.
`ociCLI.Exec` (`ocicli.go:162-185`) never emits `-u`. So every claim-pod container
runs as its image's default user, which is usually root.

The pain this removes: one root container in a claim pod leaves root-owned files in
the bind-mounted workspace or in a named volume. The next non-root step, or the
agent's own cleanup, then fails with `Permission denied`. Named volumes persist across
runs (`internal/agent/volumes.go`), so wrong ownership builds up from run to run.
Kubernetes avoids this with pod-level `runAsUser` and `fsGroup`. Host agents cannot
express either today.

## 2. Goals, non-goals, anti-goals

**Goals**

- The host claim pod honours the identity part of `securityContext`, and such jobs are
  no longer pinned to Kubernetes. The identity part is: pod- and container-level
  `runAsUser`, `runAsGroup` and `runAsNonRoot`, plus pod-level `supplementalGroups`,
  `fsGroup` and `fsGroupChangePolicy`.
- The observable result matches Kubernetes: `id -u`, `id -g`, `id -G` in every step;
  the owner, group and mode of the workspace root and of each named-volume root; the
  owner and group of files a step creates; the owner of files the platform writes on
  the job's behalf (artifact download, cache restore).
- Correct on rootful and rootless docker, podman and nerdctl, with **one** code path
  and no per-mode branching (§5.4).
- An agent that cannot deliver these semantics does not advertise that it can (§7).
  Being unable to deliver must never mean silently dropping the fields.

**Non-goals**

- Changing the UID of jobs that set no `securityContext` (user decision 1). They keep
  running as the image's default user, as on Kubernetes.
- Fields that widen privilege beyond the runtime default, or that name node-local
  resources (§4). These stay pinned to Kubernetes.
- Making Kubernetes storage honour `fsGroup` where its CSI driver does not (§6.5).
- `runsIn.image` scope containers. Neither backend applies the job's `securityContext`
  to them: host `scope.go:94`, k8s `buildScopePod` (`internal/k8sagent/scopepod.go:48`).
  That stays symmetric.

**Anti-goals**

- A job that sets `runAsUser: 1000` runs as 1000 on Kubernetes and as root on some
  host. This is the failure mode a rolling upgrade invites; §7.2 closes it.
- A run that passes or fails depending on whether the host runtime is rootful or
  rootless.
- Agent cleanup that a non-root job can break: files left by other UIDs that the next
  run cannot get past.

## 3. Current behaviour

| | Host agent | k8s-agent |
|---|---|---|
| Process identity | The image default, for every claim container. `claimPodManager.Start` (`internal/agent/claim_pod.go:328-377`) builds `CreateSpec` without a user. Exec inherits it (`ocicli.go:162`). | The kubelet applies the pod and container `securityContext`, decoded verbatim by `podSpecFromMap` (`podbuilder.go`). The pod-level context also applies to the injected `unified-artifact` sidecar and the `ucd-shim` init container, which set none of their own (`buildArtifactSidecarContainer`, `injectUcdShim`). |
| `securityContext` in a job | Never reaches a host. It pins the run to `pod` (`podtemplate.go:40-56`). If a host is reached anyway (a legacy run), `parseContainerDef` logs a WARN and drops the field (`claim_pod.go:49-54`). | Honoured. |
| Ephemeral workspace root | `os.MkdirAll(workDir, 0o755)` (`workspace.go:131`), subject to umask, owned by the agent user. | `emptyDir`: mode 0777, root-owned. With `fsGroup` it becomes group `fsGroup` with setgid, i.e. 02777 (upstream kubelet). |
| Named-volume root | `os.MkdirAll(v.path, 0o755)` at agent start (`volumes.go:142`), owned by the agent user. | A PVC, usually root-owned 0755 (`docs/operator-manual/kubernetes-integration.md:452`). It is chgrp'd to `fsGroup` only if the CSI driver supports ownership management (§6.5). |
| Files written on the job's behalf | Written by the agent process, as the agent user. Artifact download: `backend_host.go:286-303`. Cache restore: `backend_host.go:216-244`. | Written by the artifact sidecar, as the pod-level `runAsUser`, or root when that is unset (the sidecar image is `distroless/static`, root by default, `docker/artifact-sidecar.Dockerfile:22`). |
| Removing other UIDs' files | `wipeDir` (`workspace.go:188-217`) retries a failed `RemoveAll` through `containerCleanup` (`workspace.go:221-250`): a `busybox` container, root by image default, running `rm -rf /w/*...`. The workspace GC uses a bare `os.RemoveAll` with no fallback (`workspace_gc.go:98`). | Pod deletion. A reused pod is cleared by `clearWorkspaceScript` (`internal/k8sagent/workspace_clear.go:42`), executed in the job container **as the job's user**. |

Three facts shape the design.

**The agent never tells the runtime which user to use.** So the ownership problems
reported today come only from image `USER` lines and image-default root. On rootless
docker or podman, a container that runs as image `USER 1000` already writes files
owned by a subordinate UID. The agent cannot remove those files itself and needs the
`containerCleanup` fallback. That fallback works there by accident: container root
holds `CAP_DAC_OVERRIDE` over every mapped UID inside its user namespace. The docs
presented rootless podman as the way around this (`docs/operator-manual/operations.md:185-188`,
`docs/troubleshooting/steps-and-execution.md:115-116`). That holds only while every
container runs as root. Once jobs can run as non-root UIDs, rootless podman no longer
removes every ownership problem, and those docs must say so.

**The host workspace is less permissive than `emptyDir`.** An image with a non-root
`USER` cannot write the workspace root on a host today. The root is 0755 and owned by
the agent user, or by container root under rootless. The same image works on
Kubernetes, where the root is 0777. That is a parity bug independent of
`securityContext`, and this design fixes it (§6.1).

**Steps run through `exec`, not through the entrypoint.** Every step is
`docker exec <job> /.ucd/ucd-sh -c ...` (`claim_pod.go:410-421`). So the identity that
matters is the one an exec process gets. Docker and podman give an exec process the
container's configured user and additional groups. This needs verification on each
runtime (§11, V3).

## 4. Field decisions

Rule: **honour a field when it sets identity or only restricts the process. Keep
pinning a field that widens privilege beyond the runtime default, or that refers to
node-local state.** A host agent has no admission control: no Pod Security Admission
and no node-level policy. So "honour `privileged: true`" would mean root on the host
machine with nothing the operator opted into. Fields are split into tier A (this
project's core) and tier B (flag-only hardening; see Open question Q1).

### 4.1 Pod level (`podTemplate.spec.securityContext`)

| Field | Decision | Tier | Reason |
|---|---|---|---|
| `runAsUser` | honour | A | identity |
| `runAsGroup` | honour | A | identity |
| `runAsNonRoot` | honour (pre-start check, §5.3) | A | a restriction; pinning a safety check would be perverse |
| `supplementalGroups` | honour | A | identity |
| `fsGroup` | honour (§6) | A | the core of the user's problem |
| `fsGroupChangePolicy` | honour `Always` (default) and `OnRootMismatch` | A | without `OnRootMismatch`, a 200 GB Unity volume is re-chowned every run |
| `supplementalGroupsPolicy` | honour `Merge` (the default); **pin** `Strict` | A | docker, podman and nerdctl always merge `/etc/group` memberships; `Strict` has no mapping |
| `seccompProfile` | honour `type: RuntimeDefault`; **pin** `Unconfined` and `Localhost` | B | RuntimeDefault is what docker, podman and nerdctl already apply; Unconfined widens; Localhost names a node file |
| `appArmorProfile` | honour `RuntimeDefault`; **pin** others | B | same reasoning |
| `seLinuxOptions`, `seLinuxChangePolicy` | **pin** | — | relabelling semantics differ (`:z`/`:Z`); node-local policy |
| `sysctls` | **pin** | — | node-scoped |
| `windowsOptions` | **pin** | — | no Linux mapping |

### 4.2 Container level (`containers[].securityContext`)

A container-level value overrides the pod-level one, exactly as in Kubernetes.

| Field | Decision | Tier | Mapping |
|---|---|---|---|
| `runAsUser`, `runAsGroup`, `runAsNonRoot` | honour | A | §5 |
| `allowPrivilegeEscalation` | honour (`false` → `--security-opt no-new-privileges`, `true` → no flag) | B | restriction |
| `capabilities.drop` | honour (`--cap-drop`, `ALL` included) | B | restriction |
| `capabilities.add` | honour only `NET_BIND_SERVICE` (the one capability Pod Security Standards "restricted" allows); **pin** anything else | B | widening |
| `privileged` | honour `false` (no flag); **pin** `true` | B | widening |
| `readOnlyRootFilesystem` | honour (`--read-only`; podman also needs `--read-only-tmpfs=false`, see §11 V7) | B | restriction |
| `procMount` | honour `Default`; **pin** `Unmasked` | B | widening |
| `seccompProfile`, `appArmorProfile` | as at pod level | B | |
| `seLinuxOptions`, `windowsOptions` | **pin** | — | |

Any key not listed is pinned. The allowlist approach matches `containerNeedsKubernetes`
(`podtemplate.go:84-144`).

If tier B is deferred, every tier-B field moves to "pin" and nothing else in this
design changes.

### 4.3 Malformed values

Kubernetes rejects a non-integer `runAsUser`, a negative one, or one above 2147483647
when it decodes the PodSpec. For the fields in §4.1 and §4.2, the design moves that
rejection to **apply time** (`internal/dsl/parse.go`). Both backends then fail
identically and early. This follows the precedent in `claim_pod.go:87-91`, which
matches k8s on a malformed `env` value instead of degrading. The values arrive as
`int` from YAML and as `float64` after the JSON round trip through the store and the
claim, and the decoder must accept both.

## 5. Process identity on the host

### 5.1 Resolution (pure, shared)

A new pure function in `internal/dsl` resolves the effective identity of one
container: `ResolveSecurity(podSC, containerSC map[string]any) (EffectiveSecurity, error)`.
The routing predicate and the host builder both call it, so "which fields the host
honours" is decided in one place. That is the single-source-of-truth pattern
`HostSupportedContainerFields` already follows (`podtemplate.go:3-12`).

```
UID  = container.runAsUser  ?? pod.runAsUser  ?? unset
GID  = container.runAsGroup ?? pod.runAsGroup ?? unset
Supp = pod.supplementalGroups ∪ {pod.fsGroup}            (pod-level only, as in k8s)
NonRoot = container.runAsNonRoot ?? pod.runAsNonRoot ?? false
```

### 5.2 Runtime flags (identical rootful and rootless)

`crt.CreateSpec` gains `User string` and `GroupAdd []string`. Tier B adds
`CapDrop`, `CapAdd`, `NoNewPrivileges` and `ReadOnlyRootfs`. `ociCLI.createArgs`
emits them before the image.

| UID | GID | Flag | Primary group the process gets |
|---|---|---|---|
| unset | unset | none | image default (decision 1) |
| set | set | `--user U:G` | G |
| set | unset | `--user U` | the runtime looks up U's group in the image `/etc/passwd`, falling back to 0. containerd CRI does the same (§11 V2) |
| unset | set | `--user <imageUser>:G` | G. `<imageUser>` is the user part of the image's `Config.User`, or `0` when empty. This needs the image config (§5.3) |

Each `Supp` entry becomes `--group-add <gid>`. docker, podman and nerdctl also add the
user's `/etc/group` memberships on their own, as containerd CRI does (§11 V2).

These flags go on **every claim-pod container**: the primary `job` container and every
user sidecar. That matches Kubernetes, where the pod-level context applies to all
containers. The **pause** container is the exception. It runs as `--user 0:0` (§6.2)
because it is the agent's privileged helper. On k8s the sandbox container is
invisible to the job, and the same holds here: nothing execs into pause on the job's
behalf.

Exec: `ociCLI.Exec` keeps passing no `-u`. The exec process inherits the container's
configured user and its `--group-add` groups. This is load-bearing, and §11 V3
verifies it on each runtime.

Apple `container` (`internal/runtime/apple.go`) cannot back a claim pod at all.
`Create` with `NetworkContainer` returns an error (`apple.go:108-110`), and Apple is
excluded from auto-detection (`runtime.go:124-129`, `docs/reference/configuration.md:362`).
Its `createArgs` must **return an error** when `User` or `GroupAdd` is set, not ignore
them, so the field can never be silently dropped. In practice an Apple-runtime agent
never advertises the capability (§7.1). wslc shares the `ociCLI` driver; its flag
support is unverified, and the probe decides.

### 5.3 `runAsNonRoot` and the image user

Kubernetes refuses to start a container with `runAsNonRoot: true` when the effective
UID is 0. When `runAsUser` is unset and the image `USER` is a name rather than a
number, it refuses with "image has non-numeric user (…), cannot verify user is
non-root". The host mirrors this **before** creating the container. A sidecar running
its own entrypoint must never start as root first.

The image config comes from an optional runtime interface,
`crt.ImageInspector{ ImageUser(ctx, image) (string, error) }`. On docker, podman and
nerdctl it runs `<bin> image inspect --format '{{.Config.User}}'`, after a `Pull` when
the image is absent. It is optional, checked by type assertion, so the roughly ten
`ContainerRuntime` test fakes are not churned. A runtime without it fails the run with
an actionable message. That only happens when `runAsNonRoot`, or `runAsGroup` without
`runAsUser`, needs it.

The failure is a claim-pod start failure through the existing `failRun` path
(`agent.go:875-879`), with a message that mirrors kubelet's.

### 5.4 Why there is no rootful/rootless branch

The decisive fact is that **every** ownership operation in §6 is performed by a
container running as UID 0 *inside the runtime's user namespace*, never by the agent
process.

- **Rootful.** Container UID N is host UID N. Container root is host root. Container
  root can chown and remove anything.
- **Rootless docker, podman or nerdctl.** Container root is the agent's host user.
  Container UID N (N ≥ 1) is subordinate UID `subuid_start + N - 1`. Container root
  holds `CAP_CHOWN`, `CAP_FOWNER` and `CAP_DAC_OVERRIDE` over every mapped ID. So it
  can chgrp to `fsGroup` (mapped), chown to `runAsUser` (mapped), and remove files
  owned by any mapped subordinate ID, which the agent cannot.
- **podman `--userns=keep-id`** (if an operator set it globally in `containers.conf`).
  The agent UID maps to itself and container root maps to a subordinate ID. Every
  inode in the workspace is still mapped, so the same operations still succeed. Note
  that keep-id also changes the *default* user of every container to the agent's UID.
  That already violates decision 1 today. The probe (§7.1) detects it and refuses to
  advertise.

The design therefore needs **no** rootless detection for correctness. The agent still
records the mode for diagnostics and for one limit. The probe reads
`/proc/self/uid_map` and `/proc/self/gid_map` inside the root helper, which yields both
"rootless" (the map is not `0 0 4294967295`) and the highest mappable ID. podman also
reports `podman info --format '{{.Host.Security.Rootless}}'`, and docker and nerdctl
list `name=rootless` in `info .SecurityOptions`. The uid_map read is the
runtime-agnostic way to get this.

**Limit, rootless only.** A UID, GID, `fsGroup` or `supplementalGroups` entry above the
mapped range (typically 65536) cannot be represented. The agent fails such a run at
claim-pod start with
`runAsUser 200000 is not mapped by this agent's rootless container runtime (mapped container IDs 0-65536); ...`.
That is a divergence from Kubernetes, which accepts up to 2³¹-1. See Q5.

## 6. `fsGroup` and workspace ownership

### 6.1 What Kubernetes does (the target)

Upstream kubelet, `SetVolumeOwnership` / emptyDir setup:

- An `emptyDir` is created mode **0777**, root-owned, whether or not `fsGroup` is set.
- With `fsGroup` set, for every inode under the volume root: `lchown(path, -1, fsGroup)`.
  Then, unless the inode is a symlink, `chmod(mode | 0660)`, and for directories also
  `| 0110 | setgid`. With `fsGroupChangePolicy: OnRootMismatch`, the walk is skipped
  when the root already has group `fsGroup`, the setgid bit, and group rwx.
- The process gets `fsGroup` as a supplementary group (§5.2).
- Files created later inherit group `fsGroup` through the setgid directory, by plain
  kernel semantics.

The implementer must re-read upstream `pkg/volume/volume_linux.go` at the k8s version
CI's kind cluster runs and copy its exact masks and symlink handling into the new shim
subcommand (§6.3). The k8s-tagged parity test (§10) pins the result.

### 6.2 Who does what on the host

| When | What | Who / privilege | Applies to |
|---|---|---|---|
| `prepareWorkspace`, isolated mode, ephemeral workspace (`workspace.go:126-135`) | `os.Chmod(workDir, 0o777)` after `MkdirAll`. This is an explicit chmod, so umask cannot narrow it. | The agent process, which owns the directory it just created | **Every** isolated claim, with or without `securityContext`: the `emptyDir` parity fix from §3 (Q2). Not native claims. Not a volume used as the workspace (a volume is a PVC on k8s, not an `emptyDir`). |
| `claimPodManager.Start`, after the pause container and **before** any job or sidecar container | `ucd-sh fsgroup --policy=<Always\|OnRootMismatch> <fsGroup> <path>...` exec'd in pause. The paths are the workspace mount and every `volumeMounts` entry; `/.ucd` is excluded. | pause, `--user 0:0`, root in the runtime's userns (§5.4) | Claims whose pod sets `fsGroup` |
| After a non-scoped `DownloadArtifact` or `CacheRestore` into the workspace or a volume (`backend_host.go:216-226`, :286-290) | `ucd-sh chown-as <U>:<G> <path>...` exec'd in pause: re-owns what the agent wrote as if the k8s artifact sidecar had written it (§6.4). | pause, root | Claims with any honoured identity field |
| A non-scoped `UploadArtifact` or `CacheSave` fails with `fs.ErrPermission` | Retry the read through `CopyOut` from pause into a temp dir, then archive that | pause, root | Any isolated claim. This is the read-side twin of the existing `wipeDir` fallback |
| Workspace sweep, before and after the run (`wipeDir`) | Unchanged: `RemoveAll`, falling back to `containerCleanup`. `containerCleanup` gains an explicit `User: "0:0"` so its root-ness no longer depends on the `busybox` image default | the cleanup container, root | Ephemeral workspaces, and volumes with `cleanWorkspace` |
| Workspace GC of aged directories (`workspace_gc.go:98`) | Switch from `os.RemoveAll` to `wipeDir`, so it gets the same fallback | cleanup container | Crashed-run leftovers |

**Pause gains the claim's mounts.** It currently mounts only `/.ucd` (`claim_pod.go:338`).
It now mounts the workspace and every volume, at the **same container paths** the job
containers use. So one path string means the same inode in every container of the
claim. Nothing else changes about pause: it still owns the netns and is still removed
last (`claim_pod.go:436-441`). Pause is used instead of a new one-shot container
because it already exists for the whole claim. That makes each later fixup an exec,
about 50-100 ms, rather than a container start. Pause must be root regardless of
`pauseImage`: an operator who sets `registry.k8s.io/pause` gets an image whose `USER`
is 65535. Hence the explicit `--user 0:0`.

**No new image dependency.** The helpers are subcommands of the `ucd-sh` shim, which is
already bind-mounted read-only into every container (`claim_pod.go:316-323`) and is
mode 0755/0555 (`agent.go:1069-1093`, :1210-1215), so any UID can execute it. The
`pauseImage` stays free to be a shell-less image.

### 6.3 New shim subcommands

These are added to `cmd/ucd-sh/main.go`, next to `pause`, `statfs` and `--install`
(`cmd/ucd-sh/main.go:28-78`). The logic lives in `internal/shim`, unit-testable with
injected `lchown`, `chmod` and `lstat` functions.

- `ucd-sh fsgroup [--policy=Always|OnRootMismatch] <gid> <dir>...` implements kubelet's
  walk exactly. It never follows symlinks (`filepath.WalkDir` plus `Lchown`). It
  prints one summary line per root: `fsgroup <dir>: <n> entries, <dur>`.
- `ucd-sh chown-as <uid>:<gid> <path>...` makes each path and its descendants look as
  if a process with euid U and egid G had created them. Owner becomes U. Group becomes
  the parent directory's group if the parent is setgid, otherwise G. A directory under
  a setgid parent keeps setgid. Symlinks get `Lchown` only.
- `ucd-sh secctx-probe <dir>` is used only by §7.1. It prints `uid= gid= groups=` for
  itself and, for a file it creates in `<dir>`, `file_uid= file_gid= dir_mode=`. Run as
  root, it also prints `uid_map=` and `gid_map=`.

Changing the shim means regenerating the committed binaries
(`internal/shim/embedded/ucd-sh-{amd64,arm64}`, via `go generate ./internal/shim/embedded/`;
see `internal/shim/embedded/embed.go:14-24`). The k8s side gets the new shim with the
k8s-agent image. The k8s side never calls the new subcommands.

### 6.4 Files the platform writes on the job's behalf

On Kubernetes, artifact download and cache restore are done by the `unified-artifact`
sidecar. It runs as the **pod-level** `runAsUser` and `runAsGroup`, or as root:root
when those are unset. It holds `fsGroup` as a supplementary group, and it writes into
setgid directories when `fsGroup` is set. So:

- Pod-level `runAsUser: 1000`, `fsGroup: 1000`: downloaded files are `1000:1000`. The
  job can modify them.
- Only the job **container** sets `runAsUser: 1000`: downloaded files are root-owned,
  and the job **cannot** modify them. That is real k8s behaviour.

The host reproduces both by running `chown-as <podUID or 0>:<podGID or 0>` after the
agent writes (Q6). The alternative is to chown to the job container's identity. That
is friendlier, but a job that works on hosts would then fail on k8s, which is the
anti-goal. The docs instead say: **set `runAsUser`/`fsGroup` at pod level**.

The fixup is skipped when the claim sets no identity field. The agent then keeps
writing as itself, exactly as today, so decision 1 holds. Whatever mismatch that
leaves with the k8s sidecar (agent user versus root) exists today and is out of scope.

### 6.5 Named volumes

- A host volume root is created 0755 and owned by the agent user (`volumes.go:142`).
  On k8s, a fresh PVC is typically root-owned 0755. With `fsGroup`, both converge on
  `group=fsGroup, mode 02775`, plus whatever the walk does to the contents.
- **Ownership drift is repaired by `fsGroup` under `Always`, on both backends.** Take a
  previous root container (no `securityContext`) that left `root:root 0644` files in
  `go-cache`. The next run with `fsGroup: 1000` regroups them to 1000 and adds g+rw,
  and a `runAsUser: 1000` step can then write them. That is the user's original
  problem, solved the same way k8s solves it.
- **Cost.** `Always` walks the whole volume on every run, as k8s does. For tens of GB,
  the docs recommend `fsGroupChangePolicy: OnRootMismatch`, as k8s docs do. The shim
  prints a duration, and the agent copies it into the run log (step -1, as `warnRun`
  does) so a slow start is explained.
- `mode: shared` volumes held by concurrent runs may be walked concurrently. That is
  harmless: the operations are idempotent. RWX on k8s behaves the same.
- **A k8s-side difference that this design does not fix.** Whether a PVC is regrouped
  at all depends on the CSI driver's `fsGroupPolicy`: `File` always, `ReadWriteOnceWithFSType`
  only with an fsType, `None` never. In-tree `hostPath` and the `local-path`
  provisioner never regroup. The host implements the documented `fsGroup` contract.
  A cluster whose storage ignores `fsGroup` is a k8s storage configuration problem. It
  gets its own troubleshooting entry, extending
  `docs/troubleshooting/steps-and-execution.md:383-418`.

### 6.6 The shim mount, `/.ucd`

On the host, `/.ucd` is a read-only bind of `<wsBase>/.ucd-tools`
(`claim_pod.go:316-323`, `agent.go:1031-1093`). It is world-readable and executable,
so a non-root process can run `ucd-sh pause` as PID 1 and `ucd-sh -c` as its exec
shell. It is excluded from the `fsgroup` walk. On k8s, `/.ucd` is an `emptyDir`
(`injectUcdShim`). It *is* regrouped by `fsGroup`, and the `ucd-shim` init container
writes it as the pod `runAsUser`. The only observable difference is
`stat -c %g /.ucd`, which no job should depend on. It is listed as a documented
difference rather than emulated: regrouping the agent's shared, read-only tools
directory for one claim would affect every concurrent claim.

### 6.7 Sidecars

- **User sidecars** (podTemplate containers other than `job`) get the same `--user`
  and `--group-add` as `job`, from their own container-level and pod-level values.
  This is what fixes "one root container in the mix": with a pod-level `runAsUser`,
  no claim container runs as root unless one explicitly sets `runAsUser: 0`.
- **The artifact sidecar** has no host container. Its work is done by the agent
  process plus the §6.4 and §6.2 fixups. The workspace stays readable and writable for
  it because (a) the agent writes as itself and then re-owns via pause, and (b) reads
  fall back to pause `CopyOut` on EACCES.

### 6.8 Windows and macOS hosts

On a Windows or macOS agent, isolated jobs run in Docker Desktop's (or podman
machine's) Linux VM. Bind mounts of host paths go through virtiofs, gRPC-FUSE or 9p.
How these handle `chown`, `chmod` and setgid is **unverified** and varies by backend:
NTFS paths through WSL2 drvfs typically drop ownership changes, while paths inside
the WSL2 ext4 filesystem keep them. `os.Chmod(0o777)` on Windows only toggles the
read-only attribute.

The design does not model this. It relies on the probe (§7.1). An agent whose bind
mounts do not keep ownership fails the probe, does not advertise `securitycontext`,
and keeps receiving only the jobs it can run correctly. The 0777 chmod in §6.2 is
best-effort on these platforms. It does not change routing, because jobs without
`securityContext` never depended on it.

## 7. Advertising and routing

### 7.1 A startup probe decides, not an allowlist

The outcome depends on the runtime (docker, podman, nerdctl or wslc), the mode
(rootful, rootless, userns-remap, keep-id), the host OS, SELinux and the
file-sharing backend. That matrix is too large to encode correctly from what can be
verified here. So the agent **measures** it once, at startup, after the runtime is
detected and the shim is installed (`cmd/unified-cd-agent/main.go:166`):

1. Create `<wsBase>/.ucd-probe-<rand>`. It is dot-prefixed, so it is out of GC and job
   reach (`agent.go:1049-1054` explains the convention).
2. Start pause with `--user 0:0`, mounting the probe dir at `/p` plus `/.ucd`. Exec
   `ucd-sh fsgroup 54321 /p` and `ucd-sh secctx-probe /p`, which records the uid_map.
3. Create a container from `pauseImage` with `--user 54322:54323 --group-add 54321`.
   Exec `ucd-sh secctx-probe /p`.
4. Expect `uid=54322 gid=54323 groups∋54321`, the new file `54322:54321` (setgid
   inheritance), and `dir_mode` with setgid and group rwx. The root helper must also
   report `uid=0` when run **without** `--user`. That catches the keep-id and default
   user override from §5.4.
5. Remove everything through the root helper, then remove the dir.

If every check passes, the agent advertises the new capability `securitycontext` next
to `native` and `container` (`agentCapabilities`, `agent.go:241-247`). Otherwise it
logs one WARN that names the failed check and does not advertise it. This mirrors the
existing "no container runtime" registration warning (`agent.go:257-268`). The probe
costs two container starts at agent start. Like runtime detection, it runs once per
process (`agent.go:229-237`). IDs 54321-54323 sit inside the default 65536 rootless
range.

### 7.2 Routing

- `dsl.CapSecurityContext = "securitycontext"` is added to the vocabulary
  (`capabilities.go:6-27`).
- `PodTemplateNeedsKubernetes` allows the pod-level key `securityContext`, and the
  container field `securityContext` joins `HostSupportedContainerFields`. A
  sub-key check in `containerNeedsKubernetes` (and a pod-level twin) pins any value §4
  marks "pin". The comment at `podtemplate.go:82-83` requires the whole/part statement,
  and this design supplies it.
- `RequiredCaps` returns `[container, securitycontext]` for a host-runnable podTemplate
  that has any `securityContext` key at pod or container level. It returns `[pod]` when
  something else pins it, and `[container]` otherwise.
- **Rolling upgrade, host side.** An old host agent advertises
  `[native, container]`, or nothing at all, in which case the legacy baseline is the
  same (`capabilities.go:20-22`). Under the claim rule
  `required_caps <@ COALESCE(a.capabilities, legacy)` (`internal/store/postgres.go:871`),
  neither can claim a `securitycontext` run. The anti-goal is therefore closed by
  construction: no old agent can drop the fields.
- **Rolling upgrade, k8s side.** An old k8s-agent advertises `[pod, container]`
  (`k8sagent/agent.go:92`). Without help it would stop claiming securityContext jobs
  until it is upgraded, and those jobs used to run there. Two changes fix this. The new
  k8s-agent advertises `securitycontext` explicitly. And the controller **normalizes**
  every stored capability set so that `pod` implies `securitycontext`. That is one
  helper, `dsl.NormalizeCapabilities`, applied where register and claim store caps
  (`internal/controller/api_agent.go:46`, :219) and in `EvaluateSchedulability`
  (`internal/controller/schedulability.go:35`).
- **Upgrade order** stays "controller first, then agents"
  (`docs/operator-manual/operations.md:281`). A new host agent registering against an
  old controller is rejected with `400 unknown capability: securitycontext`
  (`api_agent.go:51-55`). A 4xx is a permanent error: `retryUntilSuccess`
  (`internal/agent/retry.go:33-37`) logs `permanent error, giving up retry` with
  `status=400`, `Run` returns the registration error, and the agent exits. The
  `unknown capability` text is in the HTTP response, not in the agent log. Restart the
  agent (or let the service manager do so) after upgrading the controller. The
  migration note must say this.
- Runs already created keep the `required_caps` stored at `CreateRun`
  (`postgres.go:250-285`). `UpdateRunSpec` recomputes only Pending runs
  (`postgres.go:2805-2815`). So a run that is Queued at upgrade time stays on `[pod]`.
  A replay recomputes (`api_runs.go:166`).

## 8. Interaction with existing mechanisms

- **Shim exec shell.** `/.ucd/ucd-sh -c` runs as the container user. That needs
  nothing beyond §6.6. `readOnlyRootFilesystem` (tier B) does not affect `/.ucd`,
  which is a mount, or the workspace.
- **`ucd-sh pause` as PID 1 under a non-root UID** behaves the same: zombie reaping
  and SIGTERM handling need no privilege.
- **Volume mode marker and native/isolated flip** (`workspace.go:137-158`). These are
  unchanged. A native run on a volume after a `runAsUser: 1000` rootless isolated run
  meets subordinate-UID-owned files. The existing flip guard already fails that run
  loudly.
- **Volume usage measurement** (`volumes.go:757-814`) skips unreadable entries and
  already reports "the figure is a lower bound" (`docs/operator-manual/agents.md:859`).
  Non-root UIDs under rootless make that more frequent. The behaviour is unchanged,
  and the docs need a sentence.
- **k8s reuse pool clear.** `clearWorkspaceScript` runs as the job user, so it cannot
  remove a file a container-level `runAsUser: 0` sidecar wrote 0644 into a 0755
  directory. The pod is then deleted and the run fails (Ruling 30b in
  `docs/superpowers/decisions/2026-10-20-agent-named-volumes-rulings.md:56`). The
  host's root fallback succeeds there. `reuse` is a host no-op hint, so this
  divergence is k8s-only and is recorded, not fixed here.
- **k8s `runAsNonRoot` at pod level without pod `runAsUser`** blocks the injected
  root-image `unified-artifact` sidecar and the `ucd-shim` init container on k8s
  (upstream kubelet check, §11 V8). The host has neither container and does **not**
  emulate a failure that comes from unified-cd's own plumbing. This is a k8s-agent
  defect to fix separately: give the injected containers an explicit non-root
  `runAsUser` when the pod requests `runAsNonRoot` (Q8).

## 9. Observable behaviour, host versus Kubernetes

| Observation | k8s | Host after this design |
|---|---|---|
| `id -u` / `id -g` / `id -G` in a step | from `securityContext` | same (V2, V3) |
| `stat -c '%a %g' $UNIFIED_WORKSPACE` with no `fsGroup` | `777 0` | `777 <agent gid / 0>`. Owner and group may differ; mode and writability match |
| same, with `fsGroup: F` | `2777 F` | `2777 F` |
| volume root with `fsGroup: F` | `2775 F` (CSI-dependent, §6.5) | `2775 F` |
| owner of a file a step creates | `U:F` (setgid) or `U:G` | same |
| owner of a downloaded artifact | `podU-or-0 : F or podG-or-0` | same (§6.4) |
| step can delete a downloaded artifact | iff pod-level identity permits | same |
| UID above 65536 on a rootless host | runs | run fails with an explicit error (Q5) |
| `stat /.ucd` group | F | the agent's group (§6.6) |
| agent cleanup of other UIDs' files | n/a (pod deleted) | always succeeds via the root fallback |

## 10. Tests

**Unit, no runtime.**

- `internal/dsl/podtemplate_needs_k8s_test.go` gains table rows: every tier-A field
  host-OK; every "pin" value from §4 pins; unknown sub-key pins; a mixture pins.
  `capabilities_test.go`: `RequiredCaps` returns `[container, securitycontext]`;
  `NormalizeCapabilities` adds `securitycontext` for `pod`.
- `ResolveSecurity` precedence table (container over pod; supplementary groups pod-only;
  `float64` and `int` inputs).
- `internal/runtime/ocicli_test.go`: `createArgs` emits `--user` and `--group-add`
  before the image, with order pinned; tier B flags; podman `--read-only-tmpfs=false`.
  `apple_test.go`: `User` or `GroupAdd` set returns an error.
- `internal/shim`: the `fsgroup` walk against a fake FS reproduces kubelet's masks,
  symlink skip and `OnRootMismatch` skip; `chown-as` group inheritance.
- `internal/agent/claim_pod_test.go`: `Start` gives pause `User: "0:0"` and the
  workspace and volume mounts; it gives `job` and sidecars `User` and `GroupAdd`;
  it execs `fsgroup` before creating `job` (order assertion on `podFakeRT`).
  `runAsNonRoot` with a root image fails before any job or sidecar `Create`.
- `internal/agent/parity_host_test.go` (fake runtime): no new shared case. A fake shell
  cannot observe UIDs, so the identity golden lives in the real-runtime suites below.

**Shared golden, real runtimes.** A new `internal/paritycases/securitycontext.go`
exports `SecurityContextCases()`. These cases are kept separate from `Cases()`
(`scenarios.go:11-34`), which the fake drivers run. Each case is a claim whose step
prints, in a fixed format:
`id -u; id -g; id -G | tr ' ' '\n' | sort -n | tr '\n' ' '; stat -c '%a %g' "$UNIFIED_WORKSPACE"; touch f && stat -c '%u %g' f`,
plus a case that downloads an artifact and `stat`s it, and a volume case. `Expectation`
reuses `LogMustContain`, so the golden is the shared expected stdout.

- Host driver: `internal/agent/secctx_parity_integration_test.go`, `//go:build integration`.
  It uses the real runtime chosen by `UCD_TEST_CONTAINER_RUNTIME` and the pattern of
  `claim_pod_integration_test.go:124-150`.
- k8s driver: `internal/k8sagent/secctx_parity_k8s_test.go`, `//go:build k8s`, on the
  kind cluster CI already starts (`.github/workflows/ci.yml:81-128`).
- **CI gap.** Today no CI job runs `-tags integration`. The CI integration job runs
  `go test ./... -race` without tags (`ci.yml:36-39`). The plan adds a job matrix:
  ubuntu-latest **rootful docker** (preinstalled) and ubuntu-latest **rootless podman**
  as the runner user (podman is preinstalled on the image; rootless is the default
  for a non-root user). **Rootless docker** is a third leg via
  `dockerd-rootless-setuptool.sh`, or a documented manual run if that proves flaky.
  Only this matrix turns the "unverified" items in §11 into facts.

## 11. Unverified claims and how each is verified

| # | Claim | Verified by |
|---|---|---|
| V1 | kubelet `emptyDir` is 0777, and `fsGroup` gives 02777 / 02775 with exact masks | k8s-tagged golden (`stat -c %a`) |
| V2 | `--user U` without G picks the `/etc/passwd` group, falling back to 0, and adds `/etc/group` memberships, on docker, podman and nerdctl as in containerd CRI | golden case with an image that has a passwd entry for U and one that does not |
| V3 | `docker exec` / `podman exec` / `nerdctl exec` inherit `--user` **and** `--group-add` | golden `id -G` printed from an exec'd step |
| V4 | rootless container root can chgrp a bind-mounted agent-owned dir to a mapped gid and remove subordinate-UID files | rootless podman CI leg; rootless docker leg |
| V5 | `docker cp` / `podman cp` / `nerdctl cp` read files under a bind mount of a running container | integration test of the upload fallback with a 0600 file owned by U |
| V6 | nerdctl supports `--group-add` and `--security-opt no-new-privileges` | manual run on a nerdctl host; there is no CI leg, so a failure demotes nerdctl via the probe |
| V7 | podman `--read-only` adds tmpfs on /tmp, /run and /var/tmp unless `--read-only-tmpfs=false` | podman CI leg (`touch /tmp/x` must fail, as on k8s) |
| V8 | pod-level `runAsNonRoot` blocks a root-image sidecar or init container on k8s | k8s-tagged test (documents Q8) |
| V9 | Docker Desktop (Windows/macOS) bind mounts' ownership behaviour | manual probe run; the result goes in `docs/reference/configuration.md` |

## 12. Migration impact

This is a **routing behaviour change**. A job whose `podTemplate` is pinned to `pod`
*only* because of `securityContext` fields from the honoured set becomes
`[container, securitycontext]`. It may then land on a standard agent that passed the
probe.

**In this repository: zero jobs move.**

- `templates/buildkit-rootless-build-push.yaml:105-111` sets
  `seccompProfile: Unconfined` and `appArmorProfile: Unconfined`. Both stay pinned
  (§4), so the template stays on k8s, and the header comment at :14-16 stays true
  except for its wording.
- `examples/config/k8s-agent.yaml` is an agent-side named template. A named template
  always pins (`podtemplate.go:32-34`).
- No other file in `examples/` or `templates/` sets `securityContext`
  (`grep -rln securityContext examples templates`).

**In an operator's database**, the affected set is computable before upgrading:

```sql
-- candidates: jobs whose podTemplate carries securityContext
SELECT name FROM jobs
WHERE jsonb_path_exists(spec, '$.PodTemplate.spec.securityContext')
   OR jsonb_path_exists(spec, '$.PodTemplate.spec.containers[*].securityContext');
```

`dsl.Spec.PodTemplate` has no json tag (`internal/dsl/types.go:39`), so the stored key
is `PodTemplate`. The migration note must tell the reader to confirm this with
`SELECT spec FROM jobs LIMIT 1`. For each candidate,
`GET /api/v1/jobs/<name>/schedulability` (`api_jobs.go:152`) shows `requiredCaps`.
A job moves iff that changes from `["pod"]` to `["container","securitycontext"]`
**and** its `agentSelector` is empty or matched by some host agent's labels. Jobs
resolved from git templates at trigger time (`uses:`) have no stored podTemplate. The
SQL misses them, so the same endpoint has to be checked on their resolved runs.

**What can break for a job that moves.** Everything the routing predicate *can* see
behaves the same (§9). What it cannot see is an **implicit** dependency on being
inside the cluster: cluster DNS names (`*.svc.cluster.local`), a mounted
ServiceAccount token, NetworkPolicy-gated access. Such a job never declared that
dependency. Before this change it only ran on k8s by accident. Remedy: add
`agentSelector: [kubernetes]` (every k8s-agent carries the `kubernetes` label,
`internal/k8sagent/agent.go:109`).

**What starts working.** A securityContext job in a fleet with no k8s-agent no longer
sits `Queued` forever.

## 13. Open questions (with recommendations)

1. **Tier B hardening fields: include them in this release, or pin them for now?**
   Recommendation: **include**, as the last two plan tasks. Copy-pasted PSS-"restricted"
   templates set `allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]` and
   `seccompProfile: RuntimeDefault` together with `runAsNonRoot`. Pinning those keeps
   most real non-root templates on k8s and leaves the pain where it is. They are
   flag-only, with no ownership semantics. Drop the two tasks if you want the minimum.
2. **Make the ephemeral isolated workspace 0777 for every claim (emptyDir parity),
   including jobs without `securityContext`?** Recommendation: **yes**. It fixes
   today's non-root-image-`USER` failure on hosts. The exposure is limited by `wsBase`
   permissions, and the docs will recommend `wsBase` 0750 or stricter.
3. **Gate advertisement on a startup probe, or on a static runtime/OS allowlist?**
   Recommendation: **probe**. It is the only option that is correct for Docker
   Desktop, wslc, userns-remap, keep-id and SELinux without guessing.
4. **Capability shape:** `[container, securitycontext]`, with the controller
   normalizing `pod ⇒ securitycontext`. Recommendation: **yes**. It keeps old k8s
   agents claiming these jobs during a rolling upgrade. The name `securitycontext`
   is open to bikeshedding.
5. **A UID/GID above a rootless agent's mapped range:** fail the run, or make it
   unroutable? Recommendation: **fail at claim-pod start with an explicit message**.
   Expressing ranges in capabilities is over-design for an ID range almost no job
   uses. Revisit if it shows up in practice.
6. **Owner of agent-written files (download, cache restore):** the k8s sidecar
   identity (pod-level U or 0), or the job container's identity? Recommendation:
   **sidecar identity**, for exact k8s parity. Document "set identity at pod level".
7. **Read fallback through pause `CopyOut` makes the host more permissive than k8s**
   for files unreadable by the pod-level user (k8s's sidecar would fail; the host
   succeeds). Accept? Recommendation: **accept, and document it**. The failure
   direction is "host succeeds where k8s fails", never the reverse, and the
   alternative would be an exec-as-user read path.
8. **k8s `runAsNonRoot` versus the injected root-image sidecar and shim init.**
   Recommendation: **the host does not emulate that failure**. File a separate
   k8s-agent fix that gives the injected containers `runAsUser: 65532` when the pod
   asks for `runAsNonRoot` and sets no `runAsUser`.
9. **`fsGroup` on host volumes:** always apply (CSI `fsGroupPolicy: File` semantics)
   even though some k8s storage ignores it? Recommendation: **yes**, implement the
   documented contract and document the k8s storage caveat.
10. **Operator opt-out flag** (`--security-context=off`) for host agents?
    Recommendation: **no**. Operators who want these jobs on k8s use `agentSelector`.
    Add the flag only if someone asks.
11. **Auto-pin jobs that moved?** Recommendation: **no auto-pin**. Rely on the
    migration note: the SQL, the schedulability check, and the `agentSelector: [kubernetes]`
    remedy for jobs with implicit cluster dependencies.
