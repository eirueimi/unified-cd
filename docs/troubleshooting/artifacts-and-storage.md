# Artifacts and Storage

## Kubernetes: a transfer fails with `requires UNIFIED_CONTROLLER_URL and UNIFIED_POD_TOKEN_FILE`, `403 forbidden` or `409 binding-pending`

**Symptom**

An artifact step fails, or a cache step logs `cache not restored`, with one of:

```
artifact requires UNIFIED_CONTROLLER_URL and UNIFIED_POD_TOKEN_FILE (set by the k8s agent)
cache requires UNIFIED_CONTROLLER_URL and UNIFIED_POD_TOKEN_FILE (set by the k8s agent)
presign artifact/get: 403 forbidden: this namespace or service account may not request object URLs
presign artifact/put: 409 binding-pending: no pod is bound to this run yet; retry
presign artifact/get: 403 forbidden: the presented pod is not the one bound to this run
artifact upload failed: presign artifact/put: 503 unavailable: kubernetes identity unavailable
```

**Cause**

The Kubernetes sidecar holds no store credentials. It asks the controller for presigned URLs with its projected ServiceAccount token (see [How the sidecar reaches the object store](../operator-manual/kubernetes-integration.md#how-the-sidecar-reaches-the-object-store)), so each message points at one link of that chain:

- `requires UNIFIED_CONTROLLER_URL and UNIFIED_POD_TOKEN_FILE`: the sidecar container was started without the two variables the k8s-agent sets, usually because the sidecar image and the agent are different releases or the Pod was not built by this agent.
- `403 forbidden: this namespace or service account may not request object URLs`: the job Pod's namespace and ServiceAccount are not in the controller's `agentAuth.kubernetesStoreCredentialPolicies` for this cluster. The Helm chart renders a policy for the job namespace and the `default` ServiceAccount; a job that runs under another ServiceAccount needs its own entry.
- `503 unavailable: kubernetes identity unavailable`: the controller could not verify the Pod's token. The usual cause is missing RBAC: the controller reads the job Pod in the Pod's own namespace, so its ServiceAccount needs `get` on `pods` there, and without it the Pod lookup is refused (`403`). The chart grants this (Role `unified-cd-controller-job-pods`) from v0.21.1 on; v0.21.0 and earlier granted it only in the release namespace, so every job in another namespace (the default `ci`) failed this way. Other causes are an unreachable cluster API or a TokenReview timeout. The controller logs the real cause as a `level=WARN` line, `kubernetes identity unavailable for object request` with `cluster=<name>` and `error=...` (for example `pods "ucd-run-abc" is forbidden: User "system:serviceaccount:unified-cd:unified-cd-controller" cannot get resource "pods" in API group "" in the namespace "ci"`). To check the permission:

  ```bash
  kubectl auth can-i get pods -n <job-namespace> --as=system:serviceaccount:<release-namespace>:unified-cd-controller
  ```

  It must answer `yes`.
- `409 binding-pending`: the controller has not yet been told which Pod runs this run. The sidecar retries for up to 30 seconds, so the error means the agent's heartbeat carrying the Pod binding did not arrive in that time; check that the agent can reach the controller.
- `409 run-finished`: the run has already finished (succeeded or failed, or cancelled and its Pod is no longer bound or more than an hour has passed), so its Pod no longer gets object URLs. The sidecar does not retry. `finally:` and hook transfers of a cancelled run keep working for up to an hour from Pods that were bound before the cancel (a scope Pod started after the cancel is never bound and gets `403`); if one fails with this error, its cleanup ran longer than that. For any other run, check why the run ended.
- `403 forbidden: the presented pod is not the one bound to this run`: the token belongs to a different Pod than the one the agent reported for the run.

**Fix**

For `503 kubernetes identity unavailable`, upgrade the chart to v0.21.1 or later, or create a Role with `get` on `pods` and a RoleBinding to the controller's ServiceAccount in each job namespace (for a policy on another cluster, grant it to the controller's identity in that cluster). See [the controller needs `pods get` in every job namespace](../operator-manual/kubernetes-integration.md#how-the-sidecar-reaches-the-object-store).

Upgrade the sidecar image together with the k8s-agent (the default `sidecarImage` follows the agent's version), add the job ServiceAccount to `kubernetesStoreCredentialPolicies`, and check the agent's connection to the controller. Nothing needs to be configured for the object store itself, and there is no Secret to create.

## Kubernetes: the sidecar fails with `artifact requires S3 configuration (UNIFIED_S3_*)` after an upgrade

**Symptom**

After upgrading the controller, steps on the Kubernetes agent fail with:

```
artifact requires S3 configuration (UNIFIED_S3_*): store credentials broker <url>: store credentials request failed: 401 Unauthorized: unauthorized
```

(`cache requires S3 configuration ...` for a cache step; `<url>` is the controller URL.)

**Cause**

The job Pod runs an old `unified-artifact` sidecar image. It still asks the controller for store credentials at the old broker endpoint, which the controller no longer serves. The old sidecar sends its token in the request body with no `Authorization` header, so the controller's API authentication answers `401` before routing can say the route is gone. The `UNIFIED_S3_*` in the message is the old sidecar's own wording; there is nothing to set. The sidecar image must match the k8s-agent release.

**Fix**

Upgrade the k8s-agent (its default `sidecarImage` is the matching tag) or set `sidecarImage` to the same release. Pooled Pods built from the old image are replaced as they age out; to replace them at once, restart the agent.

## Kubernetes: the agent refuses to start with `sidecarS3SecretName and sidecarS3SecretMode are no longer supported`

**Symptom**

The k8s-agent exits at startup with:

```
k8s-agent config: sidecarS3SecretName and sidecarS3SecretMode are no longer supported; the artifact sidecar uses controller-issued URLs (see docs/operator-manual/migrations/2026-11-20-run-scoped-object-access.md)
```

**Cause**

The config file sets `sidecarS3SecretName` or `sidecarS3SecretMode`, or the environment sets `UNIFIED_K8S_SIDECAR_S3_SECRET_NAME` or `UNIFIED_K8S_SIDECAR_S3_SECRET_MODE`. The sidecar no longer reads store credentials from a Secret.

**Fix**

Remove those keys (and the variables) and restart. If you set `agent.config.sidecarS3SecretMode` in the Helm values, remove it there; the chart no longer sets it. The Secret can be deleted once every agent is upgraded.

## A host agent refuses to start: the cache store flags are no longer supported

**Symptom**

The host agent exits at startup with:

```
agent config: the cache store flags (--cache-endpoint, --cache-key, --cache-secret, --cache-bucket) are no longer supported; caches use controller-issued URLs (see docs/operator-manual/migrations/2026-11-20-run-scoped-object-access.md)
```

**Cause**

One of the removed cache store settings is still set: a `--cache-*` flag, a `UNIFIED_CACHE_ENDPOINT` / `UNIFIED_CACHE_KEY` / `UNIFIED_CACHE_SECRET` / `UNIFIED_CACHE_BUCKET` variable (check the service unit or environment file too), or a `cacheEndpoint` / `cacheKey` / `cacheSecret` / `cacheBucket` key in the agent config file. The agent no longer holds a store key; the controller issues a URL per transfer.

**Fix**

Remove every one of them and restart. Caches need no agent configuration. If the agent cannot reach the object store endpoint the controller names, set `objectTransfer: controller` instead (see [Agents: Object transfer](../operator-manual/agents.md#object-transfer-objecttransfer)).

## A transfer fails with `multipart-unavailable`

**Symptom**

A log line or step error contains:

```
presign artifact/multipart-start: 501 multipart-unavailable: the object store does not accept presigned multipart uploads; use a single put
```

**Cause**

The controller's store cannot presign multipart upload parts (GCS's S3-compatible API is treated this way; see [Object store compatibility](../operator-manual/operations.md#object-store-compatibility)). The client normally handles this itself: it spools the stream to a temporary file and sends one presigned PUT. If you see the error as a failure, the single PUT that followed failed or the temporary directory had no room for the whole archive.

**Fix**

Make sure the client's temp directory can hold the largest archive it uploads on such a store, and look at the error that follows for the real cause (for example a store endpoint the client cannot reach).

## A download fails with `object changed during download`

**Symptom**

An artifact or cache download fails with:

```
object changed during download (at byte 1073741824)
```

**Cause**

The client resumes an interrupted download with a `Range` request and a conditional header that names the object version it started with. The object was replaced (for example the artifact was uploaded again, or the cache entry was overwritten) between the first part and the resume, so the store answered `412` and the client refuses to join bytes of two versions.

**Fix**

Retry the step. A run's artifact name is normally written once; if a job re-uploads the same artifact while another run is downloading it, order the two so the download starts after the upload finishes.

## Job pods never start with `CreateContainerConfigError`

**Symptom**

A job on the k8s-agent never starts. The run sits for the full `podStartTimeout` (default 5m) and then
fails. The run's failure message names the container and the kubelet's reason:

```
k8s: run pod did not become ready: waiting for Pod ucd-run-abc123 to become
Running: context deadline exceeded (container "job" is waiting:
CreateContainerConfigError: secret "db-password" not found)
```

**Cause**

A container references a Secret that does not exist **in the job Pod's namespace**, for example a
`secretKeyRef` or `envFrom.secretRef` in the job's `podTemplate` (permitted through
`jobPodPolicy.allowSecrets`) or in an agent template. The reference is a `LocalObjectReference` and has no
namespace field, so Kubernetes always resolves it in the Pod's own namespace (the agent's `namespace:`
field, `ci` in the shipped manifests); a Secret one namespace over is invisible. The artifact sidecar
itself uses no Secret.

**Fix**

```bash
# Which namespace do job pods run in? The agent config's `namespace:` field.
kubectl -n ci get secret db-password
```

If it is not there, create it there (copying it from another namespace is fine).

## Kubernetes run fails: pod was evicted: Usage of EmptyDir volume "workspace" exceeds the limit

**Symptom**

A run on the Kubernetes agent fails part-way through, and its error starts with the kubelet's own
words:

```
k8s: pod <pod name> was evicted: Usage of EmptyDir volume "workspace" exceeds the limit "20Gi".
The workspace emptyDir is limited to 20Gi by the agent workspaceSizeLimit; use a larger podTemplate (podTemplates.<name>.workspaceSizeLimit), or keep large data in a named volume (spec.volumes).
(exec error: ...)
```

The second line (the hint) appears only when the agent has a `workspaceSizeLimit` configured and the kubelet message mentions `emptyDir` or ephemeral storage. The
cluster shows the eviction: `kubectl -n ci get events --field-selector reason=Evicted`.

**Cause**

The agent caps the workspace `emptyDir` with `workspaceSizeLimit`, and this run wrote more than the
cap, so the kubelet evicted the pod. The cap exists so one runaway run cannot fill the node's disk
and evict unrelated pods. With no limit configured (the default) the same eviction can still happen
for node-pressure reasons; the kubelet message is then shown without the hint.

**Fix**

Pick whichever fits the job:

1. **Raise the limit for the template this job uses**: `podTemplates.<name>.workspaceSizeLimit`
   (a Kubernetes quantity such as `80Gi`) overrides the global value. Restart the agent after
   changing its config.
2. **Raise the global `workspaceSizeLimit`** (or `UNIFIED_K8S_WORKSPACE_SIZE_LIMIT`) if every job needs
   more room.
3. **Keep the large data out of the workspace**: put it in a [named
   volume](../user-guide/writing-jobs/volumes.md) (`spec.volumes`), which is a PVC and not subject to
   the `emptyDir` cap.

Check the key spelling in the agent config: it ignores unknown keys, so a misspelled
`workspaceSizeLimit` leaves the workspace uncapped (or at the old cap) without any error. A pod
template that already sets an `ephemeral-storage` request or limit, or a `sizeLimit` on the
workspace, is left as you wrote it and `workspaceSizeLimit` does not apply to it. See [Kubernetes
Integration: Disk limits and
headroom](../operator-manual/kubernetes-integration.md#disk-limits-and-headroom).

## k8s pod `ImagePullBackOff` on `unified-artifact`

**Symptom**

The job's pod is stuck in `ImagePullBackOff` or `ErrImagePull`, and the failing
container is `unified-artifact` (the auto-injected workspace sidecar), not one
of the job's own containers. (With the default images the same symptom can show
on the `ucd-shim` init container or on `job`, for the same reason; see the
release-tag cause below.)

You no longer need `kubectl describe pod` to find that out: the run's own
failure message carries the kubelet's reason and message, e.g.

```
k8s: run pod did not become ready: waiting for Pod ucd-run-abc123 to become
Running: context deadline exceeded (container "unified-artifact" is waiting:
ImagePullBackOff: Back-off pulling image
"ghcr.io/…/unified-cd-artifact-sidecar:v0.7.0")
```

The agent also logs the reason as soon as it appears, rather than only when
`podStartTimeout` expires. It deliberately keeps waiting out the timeout
anyway: an image pull recovers on its own the moment the image becomes
pullable, so failing early would turn a transient registry problem into a hard
run failure.

**Cause**

The k8s-agent injects a sidecar container named `unified-artifact` into every
pod to handle `uploadArtifact` / `downloadArtifact` / `cache` transfers. Its
image is set by the agent's `sidecarImage` config field (default
`ghcr.io/eirueimi/unified-cd-artifact-sidecar:<agent version>`, so a `v0.7.0`
agent asks for `:v0.7.0`; `:latest` for a locally built agent) — if that tag
isn't pullable from the node (private registry without imagePullSecrets, a
mirror that does not carry the tag, typo in the tag, or the tag was deleted),
the pod can never become Ready.

One cause is specific to the version-derived default: **the agent's release
never published its images.** The git tag exists, so the agent binary was built
and deployed, but `release-docker.yml` failed (its `verify` job reports it), so
`:v0.7.0` does not resolve. Check with
`docker manifest inspect ghcr.io/eirueimi/unified-cd-artifact-sidecar:v0.7.0`.
See the [migration
note](../operator-manual/migrations/2026-10-22-version-derived-default-images.md#a-release-whose-images-did-not-publish).

**Fix**

- If the tag is the agent's own version and does not resolve in the registry,
  re-push the release tag so `release-docker.yml` publishes the images; if you
  mirror images, copy that tag into the mirror.
- Confirm the configured `sidecarImage` is pullable from the cluster's nodes:
  `docker pull <sidecarImage>` from a node, or check for image pull secrets if
  the registry is private.
- The sidecar image **must match the agent's release** — it runs the
  `unified-sidecar` binary via `exec`, using a binary protocol; an
  older/mismatched image is incompatible even if it happens to pull
  successfully. Pin `sidecarImage` to the same version as the k8s-agent.
  Verify the two actually match by asking each binary:
  `kubectl exec <pod> -c unified-artifact -- unified-sidecar version` and
  `kubectl exec <pod> -- /k8s-agent --version`. Both print `dev` unless the
  image was built from a release tag.
- See [Kubernetes Integration Guide](../operator-manual/kubernetes-integration.md) for the full
  sidecar contract.

## A sidecar failed to start

**Symptom**

A step that talks to a `podTemplate` sidecar (a database, a tool container)
fails or hangs — connection refused, timeout, or similar — and it's unclear
whether the sidecar's own process ever came up.

**Cause**

There are no readiness probes for `podTemplate` sidecars (see [Job Isolation:
`native` and the claim pod](../user-guide/writing-jobs/isolation-and-containers.md#job-isolation-native-and-the-claim-pod)),
so a step can easily run before its sidecar is ready — or the sidecar's
process may have failed to start at all (bad config, missing env var, crash
on boot).

**Fix**

Open the run in the Web UI and select the sidecar in the **Sidecars** group
in the step sidebar (a separate group from "Steps") to read its own
stdout/stderr — this is the sidecar's own process output (e.g. `mysqld`'s
startup log), not step output. The status dot and label next to its name
show whether it's still `running` or has `exited N`; a non-zero `N` is the
container's exit code and points straight at why it never came up. This
works even after the run finishes and the pod/container is torn down — the
sidecar's log lines persist in the run's log store. See [Job Reference:
Sidecar container logs](../user-guide/writing-jobs/isolation-and-containers.md#sidecar-container-logs) for the full
behavior.

## Artifact step fails `no such file`

**Symptom**

```
upload-artifact "missing-artifact": tar walk "/root/workspace/working0/does-not-exist.txt": lstat /root/workspace/working0/does-not-exist.txt: no such file or directory
```

**Cause**

A relative `path:` in `uploadArtifact` (or `destDir:` in `downloadArtifact`)
resolves against the **run workspace** — the same directory `run:` steps
execute in — on every agent type (standard and Kubernetes). This error means
the file genuinely isn't there at that location: a common cause is a step
that wrote the file using its own working-directory assumption (e.g.
`cd subdir && make build` writing to `subdir/out/report.txt`, then a later
step referencing `out/report.txt` relative to the workspace root instead of
`subdir/out/report.txt`), or a step that wrote outside the workspace entirely
(e.g. an absolute path like `/tmp/report.txt`).

**Fix**

- Double check the exact path the producing step wrote to, relative to the
  run workspace root — add a debugging `run: ls -la` or `find . -name
  '<file>'` step before the `uploadArtifact` step if unsure.
- If a step intentionally `cd`s into a subdirectory, reference the artifact
  path relative to the workspace root, not the step's `cd` target.
- Artifact/cache paths must be **workspace-relative**: an absolute `path:`/
  `destDir:` (or one that escapes the workspace via `..`) is now rejected
  outright — see [Step fails with `artifact/cache path ... escapes the
  workspace`](#step-fails-with-artifactcache-path-escapes-the-workspace). Have
  the producing step write the file inside the workspace instead.
- See [Job Reference: Artifacts](../user-guide/writing-jobs/artifacts-and-cache.md#artifacts) for the full path
  resolution rules.

## `artifact download` fails

**Symptom**

`unified-cli artifact download <run-id> <name>` errors instead of extracting a
file.

**Cause**

Either the run ID is wrong/belongs to a different job than expected, or the
artifact `name` doesn't match what `uploadArtifact` used for that run (names
are case-sensitive and must match exactly).

**Fix**

Always list the run's artifacts first to get the exact name:

```bash
unified-cli artifact list <run-id>
# app-binary
# test-report

unified-cli artifact download <run-id> test-report --dest ./out
```

If the list is empty, the run never reached (or failed before) its
`uploadArtifact` step — check `unified-cli logs <run-id>` for the upload step's
outcome.

## Step fails with `artifact/cache path ... escapes the workspace`

**Symptom**

A step fails with `artifact/cache path "<p>" escapes the workspace`.

**Cause**

An `uploadArtifact`/`downloadArtifact`/`cache` step used an absolute path or a `..` path that leaves the run workspace. This is rejected to keep steps from reading or writing files outside the workspace (on Kubernetes the artifact sidecar is more privileged than the job container).

**Fix**

Use a path relative to the workspace (e.g. `dist`, not `/workspace/dist` or `../dist`). `$UNIFIED_WORKSPACE` names the workspace root if you need an absolute base.

**Behavior change (2026-07 path hardening)**

Before the 2026-07 path hardening, an absolute artifact/cache path was not
rejected — it was silently re-rooted under the workspace (on Kubernetes) or
resolved against the host filesystem (on the standard agent), so a step
could appear to work while quietly reading or writing the wrong location.
Absolute paths now hard-fail with the error above instead. This is
intentional: if a step that used to "work" with an absolute path now fails
here, it was previously relying on the silent-reroot/host-resolve behavior —
switch it to a workspace-relative path per the Fix above.

## An artifact or cache path leaves a volume's contents out

**Symptom**

An `uploadArtifact` or `cache:` step succeeds, but the archive does not contain the files that live
on a mounted volume, or a `downloadArtifact` step succeeds but files of the artifact are missing
under a volume's mount path, and the owning step's log has a line like:

```
artifact path "." excludes volume "go-cache" mounted at ".ucd-cache/go": the contents of a volume are never archived as part of a parent directory; name the volume's mount path to archive it
```

(`cache path ...` for a cache step; one line per excluded volume.) For a download the line reads:

```
artifact download path "." excludes volume "go-cache" mounted at ".ucd-cache/go": files of the artifact that lie inside a volume are not extracted into it; name the volume's mount path as the destination to extract into it
```

**Cause**

The step's `path` (or `destDir`) is an **ancestor** of a volume's mount point (`.`, or `.ucd-cache`
when a volume is at `.ucd-cache/go`). A volume can hold tens of gigabytes, so its contents are
deliberately never swept into the archive of a parent directory, and an archive is never extracted
into a volume through a parent directory either (it would write files into a persistent volume the
job did not ask to touch, or, on an isolated host agent, into a directory the container cannot
see). The rule is the same on the host and Kubernetes agents, and the mount point itself is omitted
too.

**Fix**

Name the volume's own mount path when you do want its contents (`path: .ucd-cache/go`, or
`destDir: .ucd-cache/go` for a download), or accept the exclusion. This is an announcement, not an
error.

## Artifact upload fails or a cache is never restored: `flag provided but not defined: -exclude`

**Symptom**

Only for jobs that archive an ancestor of a volume's mount point (or download an artifact into one),
on a Kubernetes agent: the artifact step fails and the cache step reports "not restored", and the
`unified-artifact` sidecar
(visible in the **Sidecars** group of the run) printed:

```
flag provided but not defined: -exclude
```

and exited with status 2.

**Cause**

The Kubernetes agent passes the repeatable `--exclude <path>` flag to the sidecar (`artifact upload`,
`artifact download`, `cache save`, `cache restore`) so a volume's contents stay out of the archive
and an archive is not extracted into a volume, and it passes it **only** when a job archives (or
extracts into) an ancestor of a volume. A sidecar image built before the flag exists
rejects it. The agent and its sidecar image must be rolled together; jobs that never archive an
ancestor of a volume are unaffected, which is why it can pass unnoticed until one does.

**Fix**

Update the sidecar image to the build that matches the agent (`sidecarImage` in the agent config,
or leave it unset: the default is derived from the agent's version, so a released agent gets its own
release's sidecar; see [Default images and the pause image
digest](../operator-manual/operations.md#default-images-and-the-pause-image-digest)).
`unified-sidecar version` inside the container prints its build, so you can compare it with the
agent's.

## Every artifact and cache step fails: `flag provided but not defined: -root` or `--root is required`

**Symptom**

On a Kubernetes agent, every artifact upload/download and cache save/restore step fails (a cache
restore reports "not restored"), and the `unified-artifact` sidecar (visible in the **Sidecars**
group of the run) printed one of:

```
flag provided but not defined: -root
```

```
cache restore: --root is required
```

(`artifact upload: --root is required` and the other subcommands read the same.) The sidecar exited
with status 2.

**Cause**

The agent and its sidecar image are on different versions. The agent passes `--root <workspace
mount>` on every artifact and cache invocation to confine the sidecar to the workspace. A sidecar
older than the agent does not know the flag (`flag provided but not defined: -root`); a sidecar newer
than the agent requires it and the agent does not send it (`--root is required`). Unlike
`-exclude`, this affects every artifact and cache step, not only those on a volume ancestor.

**Fix**

Upgrade the k8s-agent and the sidecar image in lockstep, as described in [Operations:
Upgrades](../operator-manual/operations.md#upgrades). Leave `sidecarImage` unset so the default
follows the agent's version, or bump the override to the agent's release.
