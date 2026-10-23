# Artifacts and Storage

## Step fails with `requires S3 configuration (UNIFIED_S3_*)`

**Symptom**

A step fails with one of:

```
artifact requires S3 configuration (UNIFIED_S3_*): ...
cache requires S3 configuration (UNIFIED_S3_*): ...
```

or, on the k8s-agent, the run is failed at claim time — before any pod is
created — with:

```
artifact steps require the unified-artifact sidecar's own S3 credentials, but the
k8s-agent's sidecarS3SecretName config field is not set; affected steps: "publish". ...
```

That preflight only fails a run for a transfer step that would definitely have
run and definitely have failed it. A step carrying `continueOnError: true` is
ignored (its failure is contractually harmless), and a step guarded by an `if:`
produces a warning instead of a failure, because the guard may reference a
prior step's output and cannot be evaluated before the run starts.

This is confusing precisely because **log archiving already works** — S3 is
obviously configured somewhere.

**Cause**

The controller's S3 configuration does not reach the sidecar, and it is not
supposed to. Two different paths carry bytes to the same bucket:

- **Logs** go agent → controller → S3. The *controller's* credentials cover them.
- **Kubernetes artifacts and cache** go from the auto-injected
  `unified-artifact` sidecar **directly** to S3, with no controller in the
  path. They need their *own* credentials, in the job Pod.

So a deployment with S3 credentials only in the controller's Secret archives
logs perfectly and cannot transfer a single artifact.

**Fix**

Create a Secret carrying `UNIFIED_S3_ENDPOINT` / `UNIFIED_S3_BUCKET` /
`UNIFIED_S3_KEY` / `UNIFIED_S3_SECRET` **in the namespace the k8s-agent's
`namespace:` config field points at** — the namespace job Pods run in (`ci` in
the shipped manifests), not the namespace the agent Deployment runs in
(`unified-cd`) — and name it in the agent config:

```yaml
# k8s-agent-config.yaml
namespace: ci                              # job Pods are created here
sidecarS3SecretName: unified-cd-s3-creds   # ...so the Secret must live here too
```

Getting the namespace wrong is a *different* failure with a much louder blast
radius — see [Job pods never start with `CreateContainerConfigError`](#job-pods-never-start-with-createcontainerconfigerror)
below. See [Kubernetes Integration: S3 credentials](../operator-manual/kubernetes-integration.md#s3-credentials-required)
for the full contract.

**Note on cache**

Cache steps never fail the run over this — that is deliberate — but they are no
longer *silent* about it either. A run with cache steps and no
`sidecarS3SecretName` logs one loud warning at claim time, and a restore the
sidecar could not perform is logged as "cache not restored", never as
`cache hit`. If you are chasing a cache that appears to work while build times
never drop, check the agent log for those lines.

## Job pods never start with `CreateContainerConfigError`

**Symptom**

Every job on the k8s-agent fails — including jobs with no artifact or cache
step at all. Runs sit for the full `podStartTimeout` (default 5m) and then
fail. The run's failure message names the container and the kubelet's reason:

```
k8s: run pod did not become ready: waiting for Pod ucd-run-abc123 to become
Running: context deadline exceeded (container "unified-artifact" is waiting:
CreateContainerConfigError: secret "unified-cd-s3-creds" not found)
```

**Cause**

`sidecarS3SecretName` names a Secret that does not exist **in the job Pod's
namespace**. The agent attaches it to the sidecar with `envFrom.secretRef`,
which is not marked `optional: true`, so the kubelet cannot configure the
container and the Pod never reaches `Running` — and since no container in the
Pod starts, no step of any kind runs.

The usual cause is creating the Secret in the *agent's* namespace
(`unified-cd` in the shipped manifests) while job Pods run in the namespace the
agent's `namespace:` field names (`ci`). The reference is a
`LocalObjectReference` and has no namespace field, so Kubernetes always
resolves it in the Pod's own namespace; a Secret one namespace over is
invisible.

Note the asymmetry with the entry above: **unset** `sidecarS3SecretName`
degrades (artifact steps fail, cache no-ops, everything else runs), while
**set-but-missing** breaks every job outright.

**Fix**

```bash
# Which namespace do job pods run in? The agent config's `namespace:` field.
kubectl -n ci get secret unified-cd-s3-creds
```

If it is not there, create it there (copying it from the agent's namespace is
fine), or clear `sidecarS3SecretName` if you do not need sidecar transfers yet.

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
  sidecar contract and `sidecarS3SecretName` configuration.

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
