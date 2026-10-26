# Kubernetes job Pod field policy — Design

Date: 2026-10-26
Status: Approved (design); implementation plan in
`docs/superpowers/plans/2026-10-26-k8s-job-pod-field-policy.md`
Tracks: TODO.md item 36 (Critical)

## 1. Problem

On the k8s agent, whatever a job writes in its `podTemplate` reaches the Kubernetes API server.
`internal/k8sagent/podbuilder.go` `BuildPod` decodes an inline `podTemplate.spec` with
`podSpecFromMap` into a full `corev1.PodSpec`. A named agent template's `override` patch goes
through `applyPatch`, which merges whole containers and volumes. No field is filtered. Neither the
controller nor `internal/dsl` checks these fields at apply time, and the only gate is the agent's
own Role, which may create Pods in the job namespace.

Roles are global (`admin`/`developer`/`viewer`, `docs/operator-manual/authorization.md`), so any
`developer` can write a job that does the following:

| Reach | How |
|---|---|
| Any ServiceAccount in the job namespace, and its API permissions | `serviceAccountName`, `automountServiceAccountToken: true`, a projected `serviceAccountToken` volume |
| The node | `hostPath`, `hostNetwork`, `hostPID`, `hostIPC`, `hostPort`, `privileged: true`, `capabilities.add: [SYS_ADMIN]`, `Unconfined` seccomp/AppArmor |
| A chosen node | `nodeName`, which skips the scheduler and taints |
| **Any Kubernetes Secret in the job namespace** | `env[].valueFrom.secretKeyRef`, `envFrom[].secretRef`, `secret` volumes, projected `secret` sources |
| **Another run's exclusive named volume** | a `persistentVolumeClaim` volume naming `<agentName>-<volume>` |

The last two need spelling out because neither needs any cluster misconfiguration:

- **Secrets.** unified-cd fences its own secrets: an agent can fetch only the secrets that the
  run's spec declares. A Kubernetes Secret in the job namespace is outside that fence. In
  particular the artifact sidecar's S3 Secret (`sidecarS3SecretName`) has to live in the job
  namespace. The threat model in `docs/operator-manual/kubernetes-integration.md` says the job
  container never sees it, but today one `secretKeyRef` line reads it.
- **Named volumes.** An exclusive volume is held by one run at a time through the
  `unified-cd/volume-held-by` annotation lock ("Named volumes" in
  `kubernetes-integration.md`). The lock only works because the agent alone mounts the PVC, and an
  inline `persistentVolumeClaim` volume gets around it.

The shipped `manifests/base/namespace/namespace.yaml` `ci` namespace has no Pod Security labels, so
on a cluster with no default admission policy every row above works. Commit 913ea9a documented the
gap ("What a job can put in the Pod") as an interim measure.

## 2. Goals and non-goals

**Goals**

- By default the k8s agent refuses, at claim time, a job whose own Pod content uses any of the
  reaches above. The error names the field path and the agent config key that would allow it.
- An operator can allow each category explicitly in the agent config. Where a category refers to
  named objects (ServiceAccounts, Secrets, PVCs), the operator allows it name by name.
- The allowlist fails closed. A PodSpec field or volume type the policy has not classified is
  denied, and when a Kubernetes library bump adds such a field the unit tests fail until someone
  classifies it.
- Operator-written agent templates (`podTemplates:` in the agent config) keep working unchanged.
- The shipped job namespace enforces Pod Security `baseline` as a second, independent layer.

**Non-goals**

- Per-job or per-team authorization. Roles stay global; this design only limits what a job can
  reach.
- A policy for the host (standard) agent (§9).
- Replacing cluster admission (Pod Security, Kyverno, Gatekeeper, `ValidatingAdmissionPolicy`).
  The agent policy is the unified-cd-side floor. Admission remains the cluster-side ceiling, and
  the documentation keeps recommending it.
- Restricting what an operator's own template may do.

## 3. Rulings

These rulings are binding on the implementation. Each one says what was decided and why.

**R1 — The agent enforces, at claim time, before any Pod, lock or pool work.**
`K8sAgent.executeRun` calls `cfg.JobPodPolicy.CheckJobPodTemplate(cfg.PodTemplates,
c.PodTemplate)` right after the `native: true` check. A violation fails the run through `failRun`,
so the error goes to the run's own log (step -1, "System") and the run finishes `Failed`. The check
comes before the pool, which matters because the pool can hand out an idle Pod without calling
`BuildPod`, so a check inside `BuildPod` alone would miss reuse. It also comes before volume
locking, so a refused run never takes or waits on a volume. `BuildPod` keeps its signature (45 call
sites) and stays a pure builder. Every claim Pod is created from `executeRun`, and scope pods
(`buildScopePod`) carry no job PodSpec content: only an image, literal env and
`resources.limits`.

**R2 — No apply-time rejection in the controller or the DSL.** Every category can be opted into
per agent. A job's `agentSelector` can match several agents with different policies, and the
controller does not know which agent will claim the run. So nothing is "never allowable" at apply
time except what R6 and R7 forbid outright, and those depend on agent-local facts (the sidecar
Secret name, `agentName`, the names of injected volumes) that the controller never sees. The cost
is late feedback: the run fails at claim, not at `apply`. The error message carries the fix, and
a run that fails at claim has executed nothing. A later improvement could have the agent advertise
its policy in the heartbeat so that `apply` can warn; that is out of scope here.

**R3 — Trust boundary.** Agent-side named templates are operator-written and trusted, and they are
not checked. Everything a job supplies is checked:

| Job-supplied input | Reaches the Pod via | Checked |
|---|---|---|
| `podTemplate.spec` (inline), including containers and volumes merged in from `uses:` templates (`internal/gittemplate/resolve.go` `mergeContribution`) | `BuildPod` inline branch | Yes, the whole decoded PodSpec |
| `podTemplate.override.containers` / `.volumes` | `applyPatch` | Yes, each patch container and volume |
| An override container's `volumeMounts` / `volumeDevices` naming a **template** volume | the template volume is mounted into a job container | Yes, the referenced template volume is checked as if the job had declared it (§5.6) |
| `podTemplate.name` | selects an operator template | No; choosing among the operator's templates is allowed |
| `podTemplate.workspace.mountPath` | `injectWorkspace` (an `emptyDir`) | No; it is a path for an agent-built `emptyDir`, and Kubernetes rejects a duplicate mount path |
| `podTemplate.reuse`, `cleanWorkspace` | pool and wipe behaviour | No; no Pod field |
| `spec.volumes` (named volumes) | `injectVolumes` (agent-owned PVCs, behind the lock) | No; agent-mediated |
| `runsIn.image`, `runsIn.resources.limits` | `buildScopePod` | No; image and limits only |
| `podTemplate.spec` when `podTemplate.name` is also set | ignored by `BuildPod`'s named branch | No; it never reaches the Pod |

The last row reflects how `BuildPod` behaves today: with a name set, the `Spec` map is dropped,
including `uses:` contributions. The policy checks exactly what `BuildPod` consumes, so nothing
ignored can fail a run. Whether `uses:` contributions should merge into a named template is a
separate question (§11).

**R4 — Check the decoded, typed form.** The checker decodes job content with the same functions
`BuildPod` uses (`podSpecFromMap`, `containersFromMaps`, `volumesFromMaps`), then reads typed fields
and also the JSON key set of the re-encoded typed values. An unknown or misspelled key that the
decode drops never reaches the API server, so it is not a policy matter, and existing jobs with
harmless typos keep working. If decoding fails, the checker returns nil and leaves the error to
`BuildPod`, so today's `parse inline pod spec: ...` messages stay the same.

**R5 — Report every violation in one error, in a deterministic order.** A job author should not
have to fix one field per run attempt. The order is the walk order: PodSpec keys sorted, then
containers, init containers and volumes by index, and fields in a fixed order within each.

**R6 — Some names can never be opted into, and the agent refuses to start if the config lists
them.**
- `allowSecrets` may not name `sidecarS3SecretName`, because the sidecar's credentials must stay
  out of job containers, which is the property the threat model states.
- `allowPersistentVolumeClaims` may not name `<agentName>-<volume>` for any configured volume.
  Named volumes are reachable only through `spec.volumes`, behind the lock.
- There are no wildcards (`"*"`). Each name is listed on purpose, which keeps an operator from
  allowing the sidecar Secret or a named-volume PVC by accident. A namespace shared by two agents
  holds both agents' PVCs, and this agent can recognise only its own (the docs warn about this).

**R7 — Job containers never mount the agent-injected credential volumes.** In `file` mode the
agent adds a Secret volume `sidecar-s3-credentials` to the sidecar, and in `broker` mode a
projected token volume `sidecar-store-credential-token`. A job container's `volumeMounts` or
`volumeDevices` entry naming either one would read the sidecar's credentials. The agent always
refuses such a mount, and no setting allows it.

**R8 — Where Pod Security `baseline` permits something by design, the default policy permits it
too.** The default policy is meant to be a subset of `baseline` for job content, plus the
unified-cd-specific categories that `baseline` does not cover (ServiceAccounts, Secrets, image pull
Secrets, PVCs, `nodeName`, the field allowlists). It does not go beyond `baseline` on container
hardening:
- `allowPrivilegeEscalation` is allowed whatever its value. Unset already means `true` in practice,
  so denying an explicit `true` would change nothing, and `baseline` allows it. (`restricted`
  forbids it, and operators who want that set `restricted` on the namespace.)
- A `capabilities.add` entry from `baseline`'s default set (`AUDIT_WRITE`, `CHOWN`,
  `DAC_OVERRIDE`, `FOWNER`, `FSETID`, `KILL`, `MKNOD`, `NET_BIND_SERVICE`, `SETFCAP`, `SETGID`,
  `SETPCAP`, `SETUID`, `SYS_CHROOT`) is allowed, because the runtime grants these already. Any
  other capability counts as privilege. `capabilities.drop` is always allowed.
- Running as root (`runAsUser: 0`) is allowed.

**R9 — Configuration lives in a file block, with no environment variables.**
`podTemplates:` and `volumes:` are file-only, and this policy is a list-shaped structure that would
be awkward in environment variables. A misspelled key inside `jobPodPolicy:` is a startup error.
The top-level config decode is lenient, so without that check a typo such as `allowHostAcess`
would be silently ignored. The failure would be safe (the field stays denied) but confusing.

**R10 — The default ServiceAccount token is left alone.** A Pod that sets no `serviceAccountName`
runs as the namespace's `default` ServiceAccount, and Kubernetes automounts that account's token
unless the account or the Pod opts out. This design does not force
`automountServiceAccountToken: false` onto job Pods, because jobs that rely on the `default`
account's token today would break silently. The checks apply only to what a job *chooses*:
naming an account, setting `automountServiceAccountToken: true`, or projecting a token. The docs
recommend `automountServiceAccountToken: false` on the job namespace's `default` ServiceAccount.
See §11, question 1.

**R11 — The shipped `ci` namespace enforces Pod Security `baseline` and audits `restricted`.**
The labels are `pod-security.kubernetes.io/enforce: baseline` and
`pod-security.kubernetes.io/audit: restricted`. There is no `warn: restricted`, because every
agent-built Pod (shim init container, sidecar) violates `restricted` (no `runAsNonRoot`, no
`capabilities.drop: [ALL]`, no `seccompProfile`). client-go prints admission warnings, so the
agent log would get a warning line on every Pod create. Audit annotations cost nothing and give an
operator data for a later move to `restricted`. No version labels are set, so the level follows
the cluster's own `latest`. The `unified-cd` namespace (controller, agent, PostgreSQL, Garage) is
left unchanged; this item is about job Pods.

**R12 — Pooled Pods.** The pool key already includes the job's `podTemplate` and the agent
template, so an idle Pod is only ever matched to claims with identical content, and R1 checks that
content before `ClaimPod`. An idle Pod built before the upgrade from content that is now denied is
never handed out again. It lingers until `poolIdleTimeout` or until it is deleted. The migration
note says how to remove such Pods.

## 4. Configuration

A new top-level block in the k8s agent config, named like the existing camelCase keys
(`podTemplates`, `sidecarS3SecretMode`). Every entry is optional, and an absent block is the most
restrictive policy.

```yaml
# k8s-agent-config.yaml
jobPodPolicy:
  # Named objects: list each name; no wildcards.
  allowServiceAccounts: []        # serviceAccountName / serviceAccount values a job may set; also
                                  # gates automountServiceAccountToken: true and projected
                                  # serviceAccountToken / podCertificate sources (checked against
                                  # the Pod's effective account, "default" when none is named)
  allowSecrets: []                # Secrets a job may read: env valueFrom.secretKeyRef,
                                  # envFrom.secretRef, secret volumes, projected secret sources.
                                  # May not name sidecarS3SecretName.
  allowImagePullSecrets: []       # imagePullSecrets a job may name (lets the kubelet pull with the
                                  # Secret's credentials; does not let the job read the Secret)
  allowPersistentVolumeClaims: [] # claimName values of persistentVolumeClaim volumes. May not name
                                  # this agent's own named-volume claims (<agentName>-<volume>).
  # Categories:
  allowHostAccess: false          # hostPath volumes; hostNetwork, hostPID, hostIPC; container
                                  # ports[].hostPort / hostIP; probe/lifecycle httpGet.host and
                                  # tcpSocket.host
  allowPrivileged: false          # privileged: true; capabilities.add outside the baseline set;
                                  # procMount other than Default; seLinuxOptions outside the
                                  # baseline set; sysctls outside the safe set;
                                  # windowsOptions.hostProcess: true
  allowUnconfinedProfiles: false  # seccompProfile.type / appArmorProfile.type: Unconfined
  allowNodeName: false            # nodeName
  # Escape hatches for fields that are neither always allowed nor in a category:
  allowPodSpecFields: []          # e.g. [runtimeClassName, priorityClassName, shareProcessNamespace]
  allowVolumeTypes: []            # e.g. [nfs, csi, ephemeral]
```

`allowUnconfinedProfiles` is separate from `allowPrivileged` because the case that motivates it,
rootless BuildKit (`templates/buildkit-rootless-build-push.yaml`), needs unconfined profiles and
nothing else. Folding the two together would make operators grant `privileged` to get rootless
builds.

**Validation at startup** (`Config.Validate`; a failure makes `cmd/k8s-agent` exit 1, like other
config errors):

1. Every name in the four name lists is a DNS-1123 subdomain
   (`k8s.io/apimachinery/pkg/util/validation.IsDNS1123Subdomain`). `"*"` gets a dedicated
   message: `wildcards are not supported; list each name`.
2. `allowSecrets` does not contain `sidecarS3SecretName` (R6).
3. `allowPersistentVolumeClaims` contains no `<agentName>-<volume>` (R6).
4. Every `allowPodSpecFields` entry is a PodSpec field classified **opt-in** (§5.1), and every
   `allowVolumeTypes` entry is a volume type classified opt-in (§5.3). An always-allowed name gets
   "`%q` is always allowed; remove it". A category name gets "`%q` is governed by
   `jobPodPolicy.<key>`; use that instead". An unknown name gets an error listing the valid
   opt-in names.
5. An unknown key under `jobPodPolicy:` is rejected (`LoadConfig`, strict decode of just that
   block; R9).

At startup the agent logs one line, `k8s: job pod policy`, with the effective opt-ins
(`allowed=none` when there are none), so an operator can see what the agent will admit.

## 5. The policy

The tables below are the contract. They classify every JSON field of the vendored
`k8s.io/api` v0.36.1 types they cover. A completeness test (§7) fails if the vendored struct has a
field the table does not list, or if the table lists a field the struct does not have.

### 5.1 PodSpec top-level fields

| Class | Fields | Rule |
|---|---|---|
| **Always allowed** (nested content checked where noted) | `containers`*, `initContainers`*, `volumes`*, `securityContext`*, `restartPolicy`, `terminationGracePeriodSeconds`, `activeDeadlineSeconds`, `dnsPolicy`, `dnsConfig`, `nodeSelector`, `affinity`, `tolerations`, `topologySpreadConstraints`, `hostname`, `subdomain`, `setHostnameAsFQDN`, `hostAliases`, `enableServiceLinks`, `os`, `hostUsers`, `resources` | Allowed |
| **Category** | `serviceAccountName`, `serviceAccount` (deprecated alias) | value ∈ `allowServiceAccounts` |
| | `automountServiceAccountToken` | `false` always allowed; `true` requires the effective account ∈ `allowServiceAccounts` |
| | `nodeName` | `allowNodeName` |
| | `hostNetwork`, `hostPID`, `hostIPC` | `true` requires `allowHostAccess` (`false` is omitted on re-encode) |
| | `imagePullSecrets` | each name ∈ `allowImagePullSecrets` |
| **Opt-in** (`allowPodSpecFields`) | `shareProcessNamespace`, `runtimeClassName`, `priorityClassName`, `priority`, `preemptionPolicy`, `schedulerName`, `readinessGates`, `overhead`, `schedulingGates`, `resourceClaims`, `hostnameOverride`, `workloadRef`, `schedulingGroup` | denied unless listed |
| **Never** | `ephemeralContainers` | The API server rejects it on Pod create anyway; denied with "cannot be set when a Pod is created" |
| **Unclassified** (a field added by a future library bump) | — | denied, "not classified by this agent's job pod policy"; the completeness test catches it before a release |

Reasons for the less obvious ones:

- `tolerations`, `nodeSelector`, `affinity` are allowed. Dedicated CI node pools need them.
  They choose among the nodes the scheduler offers and override no isolation. Operators who need
  to restrict placement use the `PodNodeSelector` / `PodTolerationRestriction` admission plugins.
- `hostAliases` only writes the Pod's own `/etc/hosts`, and `hostUsers: false` tightens isolation.
- `shareProcessNamespace` is opt-in, not allowed. It lets the job container read
  `/proc/<pid>/environ` and `/proc/<pid>/root` of the sidecar, which defeats the container boundary
  that keeps the sidecar's S3 credentials out of the job (the same property R6 and R7 protect).
- `runtimeClassName` can select a weaker runtime as easily as a stronger one, `priorityClassName`
  and `preemptionPolicy` can preempt other workloads, and `schedulerName` bypasses the default
  scheduler. Each has legitimate uses, so each is opt-in by name.

### 5.2 Containers (`containers`, `initContainers`, and override containers)

Every current `corev1.Container` field is allowed as a field: `name`, `image`, `command`, `args`,
`workingDir`, `ports`, `envFrom`, `env`, `resources`, `resizePolicy`, `restartPolicy`,
`restartPolicyRules`, `volumeMounts`, `volumeDevices`, `livenessProbe`, `readinessProbe`,
`startupProbe`, `lifecycle`, `terminationMessagePath`, `terminationMessagePolicy`,
`imagePullPolicy`, `securityContext`, `stdin`, `stdinOnce`, `tty`. A future field is unclassified
and therefore denied (there is no container escape hatch, because no current field needs one).
Value rules:

| Field | Rule | Category |
|---|---|---|
| `ports[].hostPort` ≠ 0, `ports[].hostIP` ≠ "" | denied | `allowHostAccess` |
| `env[].valueFrom.secretKeyRef.name` | ∈ `allowSecrets` | secrets |
| `envFrom[].secretRef.name` | ∈ `allowSecrets` | secrets |
| `env[].valueFrom.configMapKeyRef`, `fieldRef`, `resourceFieldRef`, `fileKeyRef`; `envFrom[].configMapRef` | allowed | — |
| `livenessProbe` / `readinessProbe` / `startupProbe` `.httpGet.host`, `.tcpSocket.host` ≠ "" | denied; the kubelet sends the request from the node's network position | `allowHostAccess` (matches `baseline`'s probe-host rule) |
| `lifecycle.postStart` / `preStop` `.httpGet.host`, `.tcpSocket.host` ≠ "" | same | `allowHostAccess` |
| `volumeMounts[].name`, `volumeDevices[].name` = `sidecar-s3-credentials` or `sidecar-store-credential-token` | denied, nothing allows it (R7) | — |
| `volumeMounts[].name`, `volumeDevices[].name` naming an agent **template** volume (override only) | the template volume is checked under §5.3 | that volume's category |
| `securityContext` | §5.4 | — |

### 5.3 Volumes

Each volume's source is the single non-name key of its re-encoded JSON.

| Class | Types | Rule |
|---|---|---|
| **Always allowed** | `emptyDir`, `configMap`, `downwardAPI`, `image`, `projected`* | `projected` sources are checked one by one (below) |
| **Category** | `hostPath` | `allowHostAccess` |
| | `secret` | `secretName` ∈ `allowSecrets` |
| | `persistentVolumeClaim` | `claimName` ∈ `allowPersistentVolumeClaims` |
| **Opt-in** (`allowVolumeTypes`) | `ephemeral`, `csi`, `nfs`, `iscsi`, `fc`, `rbd`, `cephfs`, `glusterfs`, `flexVolume`, `gitRepo`, `cinder`, `flocker`, `azureFile`, `azureDisk`, `awsElasticBlockStore`, `gcePersistentDisk`, `vsphereVolume`, `quobyte`, `photonPersistentDisk`, `portworxVolume`, `scaleIO`, `storageos` | denied unless listed |

`projected` sources:

| Source | Rule |
|---|---|
| `configMap`, `downwardAPI`, `clusterTrustBundle` | allowed |
| `secret` | `name` ∈ `allowSecrets` |
| `serviceAccountToken`, `podCertificate` | the Pod's effective ServiceAccount ∈ `allowServiceAccounts`. Both mint a credential for that account. |
| unclassified | denied |

Reasons: `image` mounts an OCI image read-only, which a container can already run, so it adds no
reach. `ephemeral` provisions storage and `csi` can surface arbitrary driver content (for example
the Secrets Store CSI driver). Network volume types mount from the node with the node's
credentials and network position. `gitRepo` runs `git` on the node as root (it is deprecated and
has a history of host-escape CVEs). That puts all of these in the opt-in class.

### 5.4 Security contexts

Pod-level (`spec.securityContext`):

| Field | Rule | Category |
|---|---|---|
| `runAsUser`, `runAsGroup`, `runAsNonRoot`, `supplementalGroups`, `supplementalGroupsPolicy`, `fsGroup`, `fsGroupChangePolicy`, `seLinuxChangePolicy` | allowed | — |
| `seccompProfile.type: Unconfined`, `appArmorProfile.type: Unconfined` | denied (`RuntimeDefault`, `Localhost` allowed) | `allowUnconfinedProfiles` |
| `seLinuxOptions` with `user` or `role` set, or `type` ∉ {"", `container_t`, `container_init_t`, `container_kvm_t`, `container_engine_t`} | denied | `allowPrivileged` |
| `sysctls[].name` ∉ safe set | denied | `allowPrivileged` |
| `windowsOptions.hostProcess: true` | denied | `allowPrivileged` |

The safe sysctls are `kernel.shm_rmid_forced`, `net.ipv4.ip_local_port_range`,
`net.ipv4.ip_unprivileged_port_start`, `net.ipv4.tcp_syncookies`, `net.ipv4.ping_group_range`,
`net.ipv4.ip_local_reserved_ports`, `net.ipv4.tcp_keepalive_time`, `net.ipv4.tcp_fin_timeout`,
`net.ipv4.tcp_keepalive_intvl` and `net.ipv4.tcp_keepalive_probes`. This is the Pod Security
Standards `baseline` list as of Kubernetes v1.34, and a unit test pins it.

Container-level (`containers[].securityContext`):

| Field | Rule | Category |
|---|---|---|
| `runAsUser`, `runAsGroup`, `runAsNonRoot`, `readOnlyRootFilesystem`, `allowPrivilegeEscalation` | allowed (R8) | — |
| `privileged: true` | denied | `allowPrivileged` |
| `capabilities.add[]` outside the baseline set (R8), compared upper-cased with any `CAP_` prefix removed | denied | `allowPrivileged` |
| `capabilities.drop` | allowed | — |
| `procMount` set and ≠ `Default` | denied | `allowPrivileged` |
| `seLinuxOptions`, `windowsOptions.hostProcess` | as pod-level | `allowPrivileged` |
| `seccompProfile`, `appArmorProfile` `Unconfined` | as pod-level | `allowUnconfinedProfiles` |

Both security-context types also carry the unclassified-field rule: a key that is not in their
table is denied.

### 5.5 The effective ServiceAccount

For an inline spec, the effective ServiceAccount is `serviceAccountName`, else `serviceAccount`,
else `default`. For an override, the policy uses the **agent template's** account, because the
override cannot change it (patches carry only containers and volumes). That means
`allowServiceAccounts: [deployer]` lets a job project a token only in a Pod that really runs as
`deployer`, whether the job named that account itself or the operator's template did.

### 5.6 Override patches against a template

For `podTemplate.name` plus `override`:

1. Every patch volume is checked under §5.3, with the path `podTemplate.override.volumes[i]`.
2. Every patch container is checked under §5.2, with the path `podTemplate.override.containers[i]`.
3. For each `volumeMounts`/`volumeDevices` entry of a patch container, the checker resolves the
   name against the volumes the Pod will have. A name the patch itself supplies is already covered
   by step 1. A name that resolves to an **agent template** volume (one the patch does not
   replace) gets that template volume checked under §5.3, with the path
   `podTemplate.override.containers[i].volumeMounts[j]` and the wording "mounts agent template
   volume %q, which is ...". Without this step a job could add a container that mounts the
   template's `hostPath` or `secret` volume, which the operator meant only for the template's own
   container. A name that resolves to nothing is left for the API server to reject, as today. Agent-injected names
   (`workspace`, `ucd-tools`, named volumes) are mounted into every container by the agent anyway.
   The two credential volumes fall under R7.
4. Template containers are not checked, even when the patch replaces a template volume they mount.
   The replacement volume is job content and is checked in step 1.

### 5.7 Error shape

The grep-able prefix is `k8s: job pod policy:`. Example (one line in the run log; wrapped here for
reading):

```
k8s: job pod policy: the job's podTemplate sets 2 field(s) this agent does not allow:
podTemplate.spec.containers[1].securityContext.privileged (container "docker"): privileged: true
(allow with jobPodPolicy.allowPrivileged: true in the k8s-agent config);
podTemplate.spec.volumes[0].hostPath (volume "root"): a hostPath volume
(allow with jobPodPolicy.allowHostAccess: true in the k8s-agent config).
Agent-side podTemplates (podTemplates: in the k8s-agent config) are not subject to this policy;
see "Job Pod field policy" in docs/operator-manual/kubernetes-integration.md
```

A violation that nothing can allow ends with `(no k8s-agent setting allows this)` instead. A
violation that needs a name lists the exact value to add, for example
`(allow with jobPodPolicy.allowSecrets: ["db-password"] in the k8s-agent config)`.

The error is a typed `*JobPodPolicyError` carrying `[]PodPolicyViolation{Path, What, Allow}`, so
tests assert on structure, not on wording.

## 6. Pod Security on the shipped namespace

`manifests/base/namespace/namespace.yaml` gains, on `ci`:

```yaml
  labels:
    pod-security.kubernetes.io/enforce: baseline
    pod-security.kubernetes.io/audit: restricted
```

**Agent-built Pods comply with `baseline`.** From `podbuilder.go` and `scopepod.go`: the
`ucd-shim` init container, the `unified-artifact` sidecar and the scope `step` container set no
`securityContext`, no host namespaces and no host ports. The agent adds only `emptyDir` volumes
(`workspace`, `ucd-tools`), a `secret` volume (`file` mode), a `projected` `serviceAccountToken`
volume (`broker` mode) and `persistentVolumeClaim` volumes (named volumes), all of which `baseline`
permits. Plan Task 6 adds a `-tags k8s` test that creates real agent-built Pods in all three
sidecar modes with server-side dry-run in a namespace labelled `enforce: baseline` (Pod Security
admission runs on dry-run), so this claim is checked against a real admission controller in CI.

**The kind CI job is unaffected.** `.github/workflows/ci.yml`'s `k8s` job applies no manifests;
the `-tags k8s` tests create their own unlabelled namespaces (`newTestNamespace` in
`internal/k8sagent/testutil_k8s_test.go`), and no existing test Pod uses a field `baseline` forbids.

**What `baseline` breaks that the agent policy alone would not.** These are operator-trusted
templates that `baseline` rejects at the API server, whatever the agent config says:

- `examples/config/k8s-agent.yaml`'s `golang` template, which has a `docker:24-dind` container with
  `privileged: true`.
- `templates/buildkit-rootless-build-push.yaml`, which uses `Unconfined` seccomp and AppArmor. It
  is job content, so it also needs `allowUnconfinedProfiles`.

Operators who need either one relabel the job namespace (`enforce: privileged`) or, better, give
those jobs a separate agent with its own namespace. The docs, the example and the template header
say so.

## 7. Testing

Unit tests (no cluster), in `internal/k8sagent`:

1. **Completeness.** Reflection over the JSON tags of `corev1.PodSpec`, `Container`,
   `VolumeSource`, `VolumeProjection`, `PodSecurityContext` and `SecurityContext`. Each field
   appears in exactly one class of the matching table, and every table entry exists on the struct.
   A library bump that adds a field fails this test with the field's name.
2. **Every denied field, inline.** A table of cases, one per row of §5.1–§5.4 that can deny, each
   built as an inline `podTemplate.spec`. Each case asserts a `*JobPodPolicyError` with exactly
   one violation, its path and its `Allow` key.
3. **Every denied field, via override.** The same cases expressed as `override.containers` /
   `override.volumes` against a benign named template (PodSpec-level fields cannot be expressed in
   an override and are inline-only).
4. **Each opt-in restores.** For every case in 2 and 3, the same input with the matching
   `jobPodPolicy` setting passes. Name-list cases also check that a *different* name still fails.
5. **Named templates are unaffected.** A template containing every denied field, referenced with
   no override, passes, and so does the same template with a benign override.
6. **Template volume through an override mount.** An override container mounting the template's
   `hostPath` / `secret` volume is denied with the "mounts agent template volume" wording, and
   allowed with the matching opt-in.
7. **R7.** Mounting `sidecar-s3-credentials` / `sidecar-store-credential-token` is denied in an
   inline spec and in an override, and nothing allows it.
8. **R8 boundaries.** `allowPrivilegeEscalation: true`, `capabilities.add: [NET_BIND_SERVICE]`
   and `[CAP_CHOWN]`, and `runAsUser: 0` pass. `capabilities.add: [SYS_ADMIN]` and `[cap_net_admin]`
   are denied.
9. **Error shape.** Two violations come out in deterministic order, with the prefix, the per-item
   `allow with ...` text and the final pointer. A decode error returns nil.
10. **Config.** Each validation rule in §4 (good and bad), including R6, the wildcard message and
    the strict decode of the block.
11. **Wiring.** Using the existing `fakePM` + `httptest` pattern (`agent_native_test.go`), a
    denied claim fails the run with the policy error in the step -1 log and **no** `CreatePod`. A
    denied `reuse: true` claim never reaches the pool. An allowed claim creates its Pod. A denied
    claim with `spec.volumes` takes no lock.
12. **Manifest.** `namespace.yaml`'s `ci` namespace carries the two labels (a file-read test in the
    style of `credentials_test.go`).

Integration (`-tags k8s`, kind): §6's baseline dry-run test. The existing k8s integration tests
must stay green. None of them sets a field the default policy denies (checked by grep during
design), and none of them calls `executeRun` with such content.

## 8. Upgrade impact (breaking)

Jobs that **stop working** on upgrade, and the setting that restores each:

| A job's own `podTemplate` (inline or override) that... | Fails with `k8s: job pod policy: ...` unless |
|---|---|
| sets `serviceAccountName` / `serviceAccount` | `allowServiceAccounts: [<name>]` |
| sets `automountServiceAccountToken: true`, or projects a `serviceAccountToken` / `podCertificate` | the effective account is in `allowServiceAccounts` |
| reads a Secret (`secretKeyRef`, `envFrom.secretRef`, `secret` volume, projected `secret`) | `allowSecrets: [<name>]` |
| names `imagePullSecrets` | `allowImagePullSecrets: [<name>]` |
| mounts a `persistentVolumeClaim` | `allowPersistentVolumeClaims: [<claim>]` (never a named-volume claim; use `spec.volumes`) |
| uses `hostPath`, `hostNetwork`, `hostPID`, `hostIPC`, `hostPort`/`hostIP`, a probe/lifecycle `host` | `allowHostAccess: true` |
| uses `privileged`, a non-baseline capability, `procMount`, non-baseline SELinux, an unsafe sysctl, `hostProcess` | `allowPrivileged: true` |
| uses `Unconfined` seccomp or AppArmor | `allowUnconfinedProfiles: true` |
| sets `nodeName` | `allowNodeName: true` |
| sets an opt-in PodSpec field (`runtimeClassName`, `priorityClassName`, `shareProcessNamespace`, ...) | `allowPodSpecFields: [<field>]` |
| uses an opt-in volume type (`ephemeral`, `csi`, `nfs`, ...) | `allowVolumeTypes: [<type>]` |
| adds an override container that mounts a template volume of one of the above kinds | the matching setting |
| mounts `sidecar-s3-credentials` / `sidecar-store-credential-token` | nothing; remove the mount |

Moving the content into an agent template (`podTemplates:`) is always an alternative. Operator
templates are not filtered.

Changes from the shipped `ci` namespace label (only for installs that apply the updated manifests;
existing Pods are not evicted, and `kubectl` warns about existing violators when the label is
applied):

- A Pod that violates `baseline` fails at create with
  `k8s: failed to create Pod: pods "ucd-run-..." is forbidden: violates PodSecurity "baseline:latest": ...`.
  This happens whatever the agent config says, including for operator templates.

Unchanged: jobs with no `podTemplate`, named templates without override, inline specs using only
always-allowed fields (containers with images, commands, literal and `configMapKeyRef`/`fieldRef`
env, resources, probes without `host`, `securityContext` hardening, `emptyDir`/`configMap`/
`downwardAPI`/`projected` configMap and downward volumes, `nodeSelector`, `tolerations`,
`affinity`, `fsGroup`). Every file in `examples/jobs/` falls in this group (checked during design).

## 9. Host agent

The host (standard) agent is not in scope, and this design changes nothing there. A host claim pod
is built only from `spec.containers[]` fields in `dsl.HostSupportedContainerFields` (`name`,
`image`, `env`, `resources`, `command`, `args`), and `dsl.PodTemplateNeedsKubernetes` pins any
other `podTemplate` content (a named template, an override, any PodSpec key other than
`containers`, any other container field, `valueFrom`, `resources.requests`, non-string limits) to
a Kubernetes agent. None of the reaches in §1 can be expressed in a host claim pod. The host
agent's own trust model (a job runs with the agent host's container runtime) is documented
separately and is unchanged.

**Potential conflict.** A separate, unmerged branch adds host-agent `securityContext` support. It
widens `HostSupportedContainerFields` and stops pinning host-honoured `securityContext` fields to
Kubernetes. This design does not depend on it. Where the two would meet:

- `internal/dsl/podtemplate.go`. This plan does not modify it, but if that branch makes
  `securityContext` host-supported, the k8s policy is still what applies when a k8s agent claims
  such a job. Routing never exempts a run from the claiming agent's policy.
- The docs: `kubernetes-integration.md` (the parity bullets near the top and the rewritten policy
  section), `isolation-and-containers.md` (the `podTemplate` paragraph), and
  `templates/buildkit-rootless-build-push.yaml`'s header, which both branches edit.
- If that branch ever lets a host job request privilege (it currently targets hardening fields
  only), the host agent will need its own opt-in policy along these lines.

## 10. Documentation obligations

- `docs/operator-manual/kubernetes-integration.md`: replace "What a job can put in the Pod" with
  "Job Pod field policy". The new section covers the trust boundary, the tables (in condensed
  form), configuration, the error, the namespace labels and the recommendation for the `default`
  ServiceAccount. Fix the threat-model sentence ("the job container never sees them"), which
  becomes true by default but can be undone by `allowPodSpecFields: [shareProcessNamespace]`, so
  that caveat goes in. Note the `baseline` consequence for privileged templates in the setup
  section's example templates.
- `docs/reference/configuration.md`: add `jobPodPolicy` to the config example and the field table,
  and note that it is file-only.
- `docs/operator-manual/authorization.md`: rewrite the paragraph from 913ea9a. Roles stay global;
  on a Kubernetes agent a job's reach is now limited by the agent's `jobPodPolicy` and the
  namespace's admission. Update the anchor.
- `docs/user-guide/writing-jobs/isolation-and-containers.md`: tell job authors that a Kubernetes
  agent may refuse some fields, and link the policy section.
- `docs/troubleshooting/steps-and-execution.md`: entries for `k8s: job pod policy:` and for the
  `violates PodSecurity "baseline:latest"` create failure.
- `docs/operator-manual/migrations/2026-10-26-k8s-job-pod-field-policy.md`: the §8 table, the
  namespace label, idle pooled Pods (R12) and validation errors.
- `examples/config/k8s-agent.yaml`, `manifests/base/k8s-agent/config-configmap.yaml`: a commented
  `jobPodPolicy` block, and a note on the DinD template and `baseline`.
- `templates/buildkit-rootless-build-push.yaml` header and `templates/README.md`: the two
  requirements (`allowUnconfinedProfiles`, and a namespace that admits `Unconfined`).
- `TODO.md`: remove item 36. Add an item for the adjacent finding in §11.
- `README.md`: check the feature summary for any claim about `podTemplate` being a full PodSpec.

## 11. Out of scope, adjacent findings, open questions

**Adjacent finding (not verified end to end; filed as a TODO item by plan Task 7).**
`dsl.ValidateContainerReferences` accepts every reserved container name as a `container:` target,
and that includes `unified-artifact`. The k8s executor has no visible guard against execing a
`run:` step into the sidecar. If one is missing, `container: unified-artifact` runs job code
inside the container that holds the S3 credentials, which defeats R6 and R7 from another
direction. It needs verification and probably a fix in `internal/dsl` (reject the sidecar as a
step target) plus an agent-side backstop.

**Adjacent observation.** With `podTemplate.name` set, `BuildPod` ignores `podTemplate.spec`, so
containers and volumes contributed by `uses:` templates are silently dropped from a named-template
Pod (R3, last row). It is not a security issue and is not changed here.

**Open questions (the rulings above stand unless overruled):**

1. **R10.** Should the agent force `automountServiceAccountToken: false` on job Pods that do not
   ask for a token? It would be safer, but it would break jobs using the `default` account's
   token, so it is not in this design.
2. **R2.** Should apply-time validation reject the R7 mounts and `ephemeralContainers` now, for
   earlier feedback? It is cheap, but it splits the policy across two components.
3. **R6.** Should `allowPersistentVolumeClaims` also refuse, at claim time, any PVC carrying the
   `unified-cd/volume-owner` label (another agent's named volume in a shared namespace)? That
   needs a `get` per claimed PVC and a Role rule the agent otherwise needs only when `volumes` is
   configured.
4. **R11.** `audit: restricted` and no `warn`, versus also `warn: baseline`. `warn: baseline` adds
   nothing for Pods, because enforce already rejects them, but it would warn on a workload
   controller that someone creates in `ci`.
