# Migrating to version-derived default images

The default container images an agent pulls used to be pinned by digest
(`repo:tag@sha256:...`) in the agent's source, and had to be rotated by hand on
every release. A digest can be recorded only after its image is pushed, so each
pin could only ever name the *previous* release, and an agent that was not
rotated kept pulling an old image without any error.

Those defaults are now **derived from the agent binary's own stamped version**:
a `v0.7.0` agent asks for `:v0.7.0`, an image built from the same git tag and
the same commit, so a release's agent pulls that release's images by
construction. There is nothing to rotate. The price is that **the default is a
tag, and a tag is mutable**: the immutability the digest gave you is gone from
the default and moves into configuration, where you can restore it.

| # | Before | After |
|---|---|---|
| 1 | `runnerImage` (host agent) and `podImage` (Kubernetes agent) defaulted to `unified-cd-runner:v0.0.3@sha256:...`. | They default to `unified-cd-runner:<agent version>`, no digest. |
| 2 | `sidecarImage` defaulted to `unified-cd-artifact-sidecar:latest@sha256:...`. | It defaults to `unified-cd-artifact-sidecar:<agent version>`, no digest. |
| 3 | `shimImage` defaulted to `unified-cd-k8s-agent:latest`. | It defaults to `unified-cd-k8s-agent:<agent version>`. |
| 4 | `pauseImage` defaulted to `busybox:1.36@sha256:...`. | **Unchanged.** It is a third-party image with no release identity here, so it stays digest-pinned. |
| 5 | A release required a hand-written "rotate the digests" commit; forgetting it left agents on the old images with no error. | No such step. A release that did not publish its images makes the agent fail with `ImagePullBackOff` instead. |

`<agent version>` is the version stamped into the binary: `v0.7.0` becomes
`:v0.7.0`; goreleaser's `0.7.0` (stamped without the `v`) is normalised to
`:v0.7.0`; a pre-release such as `v0.7.0-rc1` becomes `:v0.7.0-rc1`. A version
that does not name a published release (`dev`, a `make` build's
`v0.6.0-3-gabc123-dirty`, a bare commit SHA) falls back to `:latest`.

Nothing is rejected at startup and no job YAML changes. The effects are all at
image pull time, listed below.

## Who is affected

| You | What changes |
|---|---|
| Run a **released** agent and set none of the image fields | Your agent now pulls the images of its own release, not a fixed digest. You gain lockstep and lose immutability. Read [Restoring a digest pin](#restoring-a-digest-pin) if the second matters to you, and [Mirrored and air-gapped registries](#mirrored-and-air-gapped-registries) if you mirror images. |
| Already set `runnerImage`, `pauseImage`, `podImage`, `sidecarImage` and `shimImage` explicitly | Nothing, for the fields you set. An explicit value always wins over the default. Fields you did not set follow the rules above. |
| Run a **locally built** agent (`make`, `go build`) | See [Locally built agents still skew](#locally-built-agents-still-skew). |

## Restoring a digest pin

Set the image explicitly to a digest. These are existing fields; no new
configuration surface was added.

**Host agent** (flag or config key; a flag overrides the config file):

| Image | Flag | Config key |
|---|---|---|
| Runner | `--runner-image` | `runnerImage` |
| Pause | `--pause-image` | `pauseImage` |

```yaml
# agent.yaml
runnerImage: ghcr.io/eirueimi/unified-cd-runner@sha256:<digest>
```

`pauseImage` is already digest-pinned by default; set it only to substitute
your own image.

**Kubernetes agent.** The Kubernetes agent has **no image flags** and no image
environment variables (its only flags are `--config`, `--log-level` and
`--version`), so every image is set in the agent's config file, typically the
ConfigMap:

```yaml
# k8s-agent-config.yaml
podImage: ghcr.io/eirueimi/unified-cd-runner@sha256:<digest>
sidecarImage: ghcr.io/eirueimi/unified-cd-artifact-sidecar@sha256:<digest>
shimImage: ghcr.io/eirueimi/unified-cd-k8s-agent@sha256:<digest>
```

Resolve each digest from the release you deploy, and use the **top-level
`Digest:`** of the multi-arch index, not a per-platform one:

```bash
docker buildx imagetools inspect ghcr.io/eirueimi/unified-cd-runner:vX.Y.Z
docker buildx imagetools inspect ghcr.io/eirueimi/unified-cd-artifact-sidecar:vX.Y.Z
docker buildx imagetools inspect ghcr.io/eirueimi/unified-cd-k8s-agent:vX.Y.Z
```

A digest you set no longer follows the agent. **Update it on every agent
upgrade**, or the agent runs an old runner, shim or sidecar. The sidecar fails
visibly when it is stale, because it and the k8s-agent speak a binary exec
protocol that must be upgraded in lockstep; the stale runner or shim fails
silently. See [Kubernetes Integration: Shim image, pin it in
production](../kubernetes-integration.md#shim-image-pin-it-in-production) for
the exposure each image carries.

## The pull policy depends on the tag

Nothing in this tree sets `imagePullPolicy`. The Kubernetes agent builds its
containers (the `job` container of the default pod spec, the `unified-artifact`
sidecar and the `ucd-shim` init container) without one; a search of the Go
source for `pullpolicy` (case-insensitive) finds no occurrence at all. That is
why Kubernetes' own default applies, and that default depends on the tag:

| Image reference | Kubernetes' default `imagePullPolicy` |
|---|---|
| `:latest`, or no tag | `Always` |
| Any other tag (`:v0.7.0`), or a digest | `IfNotPresent` |

(That table is Kubernetes behaviour, not something this repository implements.)
The consequence is a behaviour difference between build paths that did not
exist when every default was a digest:

- A **released** agent asks for `:v0.7.0` and gets `IfNotPresent`: a node keeps
  the images it has pulled and does not contact the registry on later pods.
  This is a speed-up, and it means the node's copy of a tag is not refreshed if
  the tag is moved afterwards.
- A **locally built** agent asks for `:latest` and gets `Always`: every pod
  start re-pulls, needs the registry to be reachable, and counts against
  registry rate limits.

A `podTemplate` you write is passed through as written, so a container in it
can set its own `imagePullPolicy`; the three containers above cannot be set,
because they come from the agent.

**Host agents** are not affected by this flip. The host agent never calls the
runtime's pull operation and passes no `--pull` option, so `docker`, `podman`
or `nerdctl` applies its own default when it creates the container: pull the
image if it is absent locally, and otherwise use what is there. A host that is
on `:latest` therefore keeps whatever copy it first pulled.

## A release whose images did not publish

`release-docker.yml` builds five images from a `v*` tag, and its `merge` job
runs only if every build succeeded, so a failed build leaves **every** image
untagged for that release, including the ones whose own build passed. The git
tag and the binaries built from it still exist.

An agent from that release asks for `:vX.Y.Z`, which does not resolve. Pods land
in `ImagePullBackOff`, and the run fails with the kubelet's reason once
`podStartTimeout` expires (see [the troubleshooting
entry](../../troubleshooting/artifacts-and-storage.md#k8s-pod-imagepullbackoff-on-unified-artifact)
for the exact message). That is loud. Before this change the old digest kept
working and the agent silently ran one release behind.

The `verify` job in `release-docker.yml` reports the failure
(`Release incomplete` or `Release tags missing`), and `:latest` still points at
the previous release. **Recovery is to fix the build failure and re-push the
tag**, which is what the workflow's own error message says. Until that is done,
roll the agent back to the previous release, or set the image fields explicitly
to the previous release's images.

Check a release's images before rolling it out:

```bash
docker manifest inspect ghcr.io/eirueimi/unified-cd-runner:vX.Y.Z
```

If nodes already pulled a tag that was later moved by a re-push, an
`IfNotPresent` node keeps its old copy; delete the image on the node or use a
digest.

## Mirrored and air-gapped registries

A pinned digest stayed valid across restarts and upgrades. A version-derived tag
changes every release, so if you mirror images, **the new release's tags must
be in the mirror before its agents start**. This is a new step of every
upgrade. Copy these for each release:

- `unified-cd-runner:vX.Y.Z` (host agent runner and Kubernetes `podImage`)
- `unified-cd-artifact-sidecar:vX.Y.Z` (Kubernetes agents)
- `unified-cd-k8s-agent:vX.Y.Z` (Kubernetes agents; also the shim image)

And, because the agents ask for a name under `ghcr.io/eirueimi`, either point
them at the mirror by setting the image fields explicitly, or arrange for your
registry to serve that name. Setting the fields (see [Restoring a digest
pin](#restoring-a-digest-pin)) is the supported route, and it also takes the
image out of the version-derivation entirely: you then own the update step.
`busybox` (the pause image) is not on this list; it does not change with
unified-cd releases.

## Locally built agents still skew

This change fixes **released** agents. A binary from `make` or `go build`
reports `dev` or a `git describe` string such as `v0.6.0-3-gabc123-dirty`, which
does not name a published tag, so it asks for `:latest` and gets whichever
release is newest. A locally built agent can therefore still run images from a
different commit than itself, which is exactly the skew this change removes for
released agents. If you build agents yourself, pin the image fields as above.

A related case: a goreleaser **snapshot** build stamps
`<version>-SNAPSHOT-<short sha>`. That string looks like a pre-release, so the
agent accepts it and requests an image tag that was never published. Nothing in
this repository configures snapshot builds, so this is a note and not a defect;
if you run snapshot builds, set the image fields.

## What the operations guide says now

The "Rotating the default runner/pause image digests" section of the
[operations guide](../operations.md#default-images-and-the-pause-image-digest)
now covers only the `busybox` pause image, and the upgrade checklist no longer
asks for a rotation. The old guidance, to bump the digest in the source on every
runner-image release, is obsolete for the three unified-cd images.
