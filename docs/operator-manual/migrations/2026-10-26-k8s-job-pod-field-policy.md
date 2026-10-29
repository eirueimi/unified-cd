# Migrating: the Kubernetes agent filters a job's Pod fields, and no step can run in the sidecar

Two breaking changes land together.

1. **The k8s-agent now refuses, at claim time, a job whose own `podTemplate` reaches a
   ServiceAccount, the node, a Kubernetes Secret, a PersistentVolumeClaim, or a field it has not
   classified.** Before, whatever a job wrote in `podTemplate.spec` or `podTemplate.override`
   reached the Kubernetes API server unfiltered, so any `developer` could run a Pod as any
   ServiceAccount in the job namespace, mount `hostPath`, run `privileged`, or read any Kubernetes
   Secret there (including the artifact sidecar's S3 Secret). Each category now needs an explicit
   opt-in in the agent's `jobPodPolicy:` block. The shipped `ci` namespace also enforces Pod
   Security `baseline`.
2. **A step can no longer have `container: unified-artifact` or `container: ucd-shim`**, on any
   agent type. Before, `container: unified-artifact` ran job code inside the artifact sidecar,
   the container that holds the object-store credentials. That step needed no `podTemplate` at
   all.

The policy and the tables are described in [Job Pod field policy](../kubernetes-integration.md#job-pod-field-policy).

## The job Pod field policy

Agent templates (`podTemplates:` in the k8s-agent config) are operator-written and are **not**
filtered. Only what a job supplies in its own `podTemplate` is: an inline `spec` (including
containers and volumes merged in from `uses:` templates), an `override` patch, and a template
volume that an override container mounts.

| A job's own `podTemplate` (inline or override) that... | Fails with `k8s: job pod policy: ...` unless |
|---|---|
| sets `serviceAccountName` / `serviceAccount` | `allowServiceAccounts: [<name>]` |
| sets `automountServiceAccountToken: true`, or projects a `serviceAccountToken` / `podCertificate` | the Pod's effective ServiceAccount is in `allowServiceAccounts` |
| reads a Secret (`secretKeyRef`, `envFrom.secretRef`, a `secret` volume, a projected `secret`) | `allowSecrets: [<name>]` |
| names `imagePullSecrets` | `allowImagePullSecrets: [<name>]` |
| mounts a `persistentVolumeClaim` | `allowPersistentVolumeClaims: [<claim>]` (never a named-volume claim; use `spec.volumes`) |
| uses `hostPath`, `hostNetwork`, `hostPID`, `hostIPC`, `hostPort` / `hostIP`, or a probe / lifecycle `host` | `allowHostAccess: true` |
| uses `privileged`, a capability outside the `baseline` set, `procMount`, non-baseline SELinux options, an unsafe sysctl, or `hostProcess` | `allowPrivileged: true` |
| uses `Unconfined` seccomp or AppArmor | `allowUnconfinedProfiles: true` |
| sets `nodeName` | `allowNodeName: true` |
| sets an opt-in PodSpec field (`runtimeClassName`, `priorityClassName`, `shareProcessNamespace`, `hostAliases`, `dnsPolicy`, `dnsConfig`, ...) | `allowPodSpecFields: [<field>]` |
| uses an opt-in volume type (`ephemeral`, `csi`, `nfs`, ...) | `allowVolumeTypes: [<type>]` |
| adds an override container that mounts an agent-template volume of one of the above kinds | the matching setting |
| mounts `sidecar-s3-credentials` / `sidecar-store-credential-token` | nothing; remove the mount |
| sets `workspace.mountPath`, or an absolute `spec.volumes` `mountPath`, that is relative, unclean, `/`, or on, inside or above `/usr`, `/bin`, `/sbin`, `/lib*`, `/etc`, `/proc`, `/sys`, `/dev`, `/run`, `/var/run`, `/.ucd`, `/usr/local/bin` or a sidecar credential mount | nothing; choose another path (`apply` also rejects the workspace case) |

Moving the content into an agent template is always an alternative to opting in.

**Unchanged:** jobs with no `podTemplate`, named templates without an override, and inline specs
that use only always-allowed fields (containers with images, commands, literal and
`configMapKeyRef` / `fieldRef` env, resources, probes without `host`, `securityContext` hardening
such as `runAsUser`, `fsGroup`, `readOnlyRootFilesystem`, `emptyDir` / `configMap` / `downwardAPI`
/ `projected` volumes, `nodeSelector`, `tolerations`, `affinity`). Every file in `examples/jobs/`
falls in this group. `shareProcessNamespace` is opt-in because it lets the job container read the
sidecar's `/proc`, which defeats the boundary that keeps the S3 credentials out of the job.

### The error

A refused run fails immediately with the error as the first line of its log (the "System" entry).
No Pod was created and nothing ran. Every violation is listed, in a fixed order, each with the
setting that allows it:

```
k8s: job pod policy: the job's podTemplate sets 2 field(s) this agent does not allow: podTemplate.spec.containers[1].securityContext.privileged (container "docker"): privileged: true (allow with jobPodPolicy.allowPrivileged: true in the k8s-agent config); podTemplate.spec.volumes[0].hostPath (volume "root"): a hostPath volume ("/" on the node) (allow with jobPodPolicy.allowHostAccess: true in the k8s-agent config). Agent-side podTemplates (podTemplates: in the k8s-agent config) are not subject to this policy; see "Job Pod field policy" in docs/operator-manual/kubernetes-integration.md
```

A violation that nothing can allow ends with `(no k8s-agent setting allows this)`. See
[Run fails immediately on Kubernetes: "k8s: job pod policy:
..."](../../troubleshooting/steps-and-execution.md#run-fails-immediately-on-kubernetes-k8s-job-pod-policy).

### Opting in

Add a `jobPodPolicy:` block to the k8s-agent config file (file-only, no environment variables) and
restart the agent. Allow only what your jobs need, name by name:

```yaml
jobPodPolicy:
  allowServiceAccounts: [deployer]
  allowSecrets: [db-password]
  allowPrivileged: false
```

The agent refuses to start on the following. Each is reported as `invalid config: jobPodPolicy: ...`,
except the first, which is a `load config` error:

- an unknown key under `jobPodPolicy:` (for example a misspelled `allowHostAcess`);
- a name that is not a valid Kubernetes object name, including any wildcard:
  `allowSecrets: "*": wildcards are not supported; list each name`;
- `allowSecrets` naming `sidecarS3SecretName`:
  `allowSecrets: "<name>" is sidecarS3SecretName, the artifact sidecar's credentials, which job containers must never read`;
- `allowPersistentVolumeClaims` naming a named-volume claim of this agent:
  `allowPersistentVolumeClaims: "<agentName>-<volume>" is this agent's named volume "<volume>"; jobs reach it only through spec.volumes, which holds its lock`;
- an `allowPodSpecFields` / `allowVolumeTypes` entry that is always allowed
  (`"<x>" is always allowed; remove it`), belongs to a category
  (`"<x>" is governed by jobPodPolicy.<key>; use that instead`), or is unknown
  (`"<x>" is not an opt-in PodSpec field; valid entries: ...`).

At startup the agent logs `k8s: job pod policy` with the effective opt-ins (`allowed=none` when
there are none).

### Find the jobs that will be refused

Run this over your stored job definitions before upgrading. It over-matches on purpose (a
`secretKeyRef` in a template you wrote yourself is fine); read each hit:

```
grep -rnE "container: *[\"']?(unified-artifact|ucd-shim)|hostAliases|dnsPolicy|dnsConfig|serviceAccount|automountServiceAccountToken|hostPath|hostNetwork|hostPID|hostIPC|hostPort|privileged|capabilities|procMount|Unconfined|sysctls|seLinuxOptions|nodeName|secretKeyRef|secretRef|secretName|imagePullSecrets|persistentVolumeClaim|runtimeClassName|priorityClassName|shareProcessNamespace|ephemeral|csi:|nfs:" <your job definitions>
```

### `hostAliases`, `dnsPolicy` and `dnsConfig` now need an opt-in

These are not filtered by the policy's always-allowed class any more. They are
Pod-wide, so they apply to the artifact sidecar too: a job that sets them controls where the
sidecar's connections to the store-credentials broker and the object store go. A job that sets any
of them now fails with
`podTemplate.spec.hostAliases: ... (allow with jobPodPolicy.allowPodSpecFields: [hostAliases] in the k8s-agent config)`.
To restore the old behaviour for jobs you trust, list the fields:

```yaml
jobPodPolicy:
  allowPodSpecFields: [hostAliases, dnsPolicy, dnsConfig]
```

Or move the setting into an agent template, which is not filtered.

This opt-in only keeps a job from setting these PodSpec fields. It does not stop a job container
that runs as root from editing the Pod's shared `/etc/hosts`, or from listening or sniffing in the
Pod's shared network namespace (`baseline` allows root and `NET_RAW`). The mitigations that do work
are TLS for the controller / broker URL (the shipped install reaches it over plain HTTP,
`manifests/install/agent-config-patch.yaml`; use `https://`) and running job containers non-root
with `readOnlyRootFilesystem: true` and `NET_RAW` dropped, set in your agent templates.

### Mount paths over system directories are refused

A job-chosen `podTemplate.workspace.mountPath` and every absolute `spec.volumes[].mountPath` is
mounted into every container, the artifact sidecar included. The agent therefore refuses, with no
setting to allow it, a path that is relative, unclean, `/`, or on, inside or above one of `/usr`,
`/bin`, `/sbin`, `/lib`, `/lib32`, `/lib64`, `/libx32`, `/etc`, `/proc`, `/sys`, `/dev`, `/run`,
`/var/run`, `/.ucd`, `/usr/local/bin` or the sidecar's credential mounts. The error is
`k8s: job pod policy: ... podTemplate.workspace.mountPath: mountPath "/usr/local/bin" is inside the protected container directory "/usr" (no k8s-agent setting allows this)`.
`apply` rejects the workspace case up front (`spec.podTemplate.workspace: mountPath "..." ...`). Any
path outside those directories, such as `/workspace` or `/home/runner/work`, is unaffected. The
agent now also starts and execs the sidecar as `/usr/local/bin/unified-sidecar`, so run the sidecar
image from the same release as the agent (the shipped image installs the binary there).

A `mountPath` that contains a backslash is now rejected, in `spec.volumes` and in
`podTemplate.workspace`: `spec.volumes[0]: mountPath "\usr\local\bin" contains a backslash; mountPath
must be a POSIX path (relative to the workspace, or absolute and starting with "/")` (for the
workspace: `spec.podTemplate.workspace: mountPath "..." contains a backslash; mountPath must be a
POSIX path`). The Kubernetes agent used to turn a backslash into a `/` when it mounted a volume, so
`\usr\local\bin` landed on `/usr/local/bin`. Write the path with `/`; this was already the rule
for every agent (see [Volumes](../../user-guide/writing-jobs/volumes.md)). A job stored before the
upgrade is refused by the agent the same way as any other protected path, after the backslashes are
treated as separators.

## The artifact sidecar only reads and writes inside the workspace

The sidecar used to resolve the paths of `uploadArtifact`, `downloadArtifact` and `cache` steps in
its own filesystem and follow symlinks there, so a job could make it archive its own credential
mount through a symlink, or write over its own files on a download or restore. It now opens the
workspace as a confined root (the agent passes `--root` with every invocation) and never follows a
symlink or `..` out of it.

| A job that... | Now |
|---|---|
| uploads, downloads or caches a path through a symlink whose target is outside the workspace (`ln -s /etc/unified-cd l` then `path: l/s3-credentials`) | The step fails (upload, download) or does nothing (cache, which is best-effort). The target is never read or written. |
| archives a directory that contains a symlink | The link is stored as a link entry and its target is not read. A symlink whose target was non-empty used to fail the upload. A download of an archive that holds a link entry still fails with `unsupported symlink entry`, as before. |
| restores a cache whose archive holds symlinks or hardlinks | The Kubernetes sidecar skips link entries (it never recreated them), still reports a hit, and now prints one `cache restore warning: skipped N symlink/hardlink entries; ...` line to the step log. The restored tree may be incomplete: tools such as `.venv` or `node_modules/.bin` may need re-creating. |
| uses symlinks that stay inside the workspace | Unchanged as path components. |

Upgrade the agent and the sidecar image together: a sidecar that is older than the agent does not
know `--root` and fails every artifact and cache step, and a newer sidecar with an older agent
fails with `--root is required`. The host agent and the CLI are not affected.

## The `ci` namespace now enforces Pod Security `baseline`

`manifests/base/namespace/namespace.yaml` labels `ci` with
`pod-security.kubernetes.io/enforce: baseline` and `pod-security.kubernetes.io/audit: restricted`.
There is no `warn` label (every agent-built Pod violates `restricted`, so each Pod create would log
a warning), and no version label, so the level follows the cluster's `latest`. This is the
cluster-side layer. It applies to **operator templates too**, whatever `jobPodPolicy` says.

| Before | After |
|---|---|
| The shipped `ci` namespace had no Pod Security labels; a Pod with `privileged: true`, `hostPath` or `hostNetwork` was created. | The API server refuses it: `k8s: failed to create Pod: pods "ucd-run-..." is forbidden: violates PodSecurity "baseline:latest": ...` |
| The `golang` template in `examples/config/k8s-agent.yaml` (Docker-in-Docker, `privileged: true`) ran. | It fails to create its Pod unless the namespace admits `privileged`. |
| `templates/buildkit-rootless-build-push.yaml` (`Unconfined` seccomp / AppArmor) ran. | It needs `jobPodPolicy.allowUnconfinedProfiles: true` **and** a namespace that admits `Unconfined` (Pod Security `privileged`). |

Only installs that apply the updated manifests are affected. Existing Pods are not evicted.

Apply the label to an existing namespace. Check first: with `--dry-run=server` the API server
warns about every existing Pod that would violate `baseline`, so you see the violators before
anything changes:

```bash
kubectl label --dry-run=server --overwrite namespace ci pod-security.kubernetes.io/enforce=baseline
kubectl label --overwrite namespace ci \
  pod-security.kubernetes.io/enforce=baseline \
  pod-security.kubernetes.io/audit=restricted
```

If a template or job needs `privileged`, `hostPath` or `Unconfined`, either keep that namespace
at `enforce=privileged` or, better, run those jobs from a separate agent with its own namespace and
keep `ci` strict. See [Run fails: "k8s: failed to create Pod: ... violates PodSecurity
"baseline:latest""](../../troubleshooting/steps-and-execution.md#run-fails-k8s-failed-to-create-pod-violates-podsecurity-baselinelatest).

Also consider switching off the `default` ServiceAccount's token in the job namespace. The policy
does not force it, because jobs that rely on it would break silently; see [Job Pod field
policy](../kubernetes-integration.md#job-pod-field-policy).

## Idle pooled Pods built before the upgrade

The pool key includes the job's `podTemplate`, and the policy is checked before the pool is
consulted, so an idle Pod is only ever handed to a claim whose content passes. A Pod that was
built before the upgrade from content that is now refused is simply never handed out again; it
lingers until `poolIdleTimeout` or until it is deleted. To remove such Pods at once, scale the
agent down first (or it may re-pool them), then:

```bash
kubectl -n ci delete pods -l app=unified-cd-agent
```

## Reserved step targets

Only `job` among the reserved container names is a valid step `container:` target.
`unified-artifact` holds the object-store credentials, and `ucd-shim` is an init container that
has exited before any step runs. This applies to **every** agent type (the host agent's claim pod has no
such container, so such a step always failed there at exec with `container "unified-artifact" is
not defined in the job's podTemplate`).

| A job that... | Now |
|---|---|
| has a step, a `finally:` step, a `parallel:` sub-step, a `uses:` entry, or a `kind: JobTemplate` step with `container: unified-artifact` or `container: ucd-shim` (case and surrounding whitespace do not matter) | `apply` fails: `spec.steps[0] (upload): container "unified-artifact" is reserved for the agent's artifact sidecar, which holds the object-store credentials; steps cannot run in it. Use the primary container "job" or a container declared in podTemplate`. For `ucd-shim` the reason reads `is the agent's ucd-sh installer init container, which has exited before any step runs`. Nothing restores it: use `job` or a declared container. |
| gets such a step from a `uses:` template at trigger time | The controller fails the run when it resolves the template, with the same message in the run's System log. |
| was stored before the upgrade with such a step | A k8s-agent fails the claim before creating a Pod: `k8s: step "upload" targets container "unified-artifact", which is reserved for the agent (the artifact sidecar holds the object-store credentials; ucd-shim has exited before steps run); no step may run in it. Re-apply the job with container: "job" or a podTemplate container`. A host agent fails the step at exec, as it always did (`container "unified-artifact" is not defined in the job's podTemplate`). |

The k8s-agent also refuses the exec itself as a last check, so no path that skips validation can
reach the sidecar. Re-apply any stored job that still has such a step with `container: job` (or a
declared container). `post:` hooks have no `container:` of their own; they run in the owning
step's container.

The sidecar is also kept out of reach of job-supplied Pod settings: no job container can mount its
credential volumes, `allowSecrets` cannot name `sidecarS3SecretName`, no job-chosen mount path may
cover its binaries or credential mounts, and name resolution (`hostAliases`, `dnsPolicy`,
`dnsConfig`) is opt-in. The remaining ways to reach the sidecar from a job are opt-ins:
`allowPodSpecFields: [shareProcessNamespace]` lets a job container read the sidecar's `/proc`, and
`[hostAliases, dnsPolicy, dnsConfig]` let it set Pod-wide name resolution that redirects the
sidecar's connections. Do not set them on an agent that runs untrusted jobs. Leaving them out is not
enough on its own: a root job container can still edit the shared `/etc/hosts`, so also use TLS for
the broker and run job containers non-root with `readOnlyRootFilesystem`.
