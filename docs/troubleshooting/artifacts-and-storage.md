# Artifacts and Storage

## k8s pod `ImagePullBackOff` on `unified-artifact`

**Symptom**

The job's pod is stuck in `ImagePullBackOff` or `ErrImagePull`, and
`kubectl describe pod` shows the failing container is `unified-artifact` (the
auto-injected workspace sidecar), not one of the job's own containers.

**Cause**

The k8s-agent injects a sidecar container named `unified-artifact` into every
pod to handle `uploadArtifact` / `downloadArtifact` / `cache` transfers. Its
image is set by the agent's `sidecarImage` config field
(default `ghcr.io/eirueimi/unified-cd-artifact-sidecar:latest`) — if that tag
isn't pullable from the node (private registry without imagePullSecrets, typo
in the tag, or the tag was deleted), the pod can never become Ready.

**Fix**

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

