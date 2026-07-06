# Release Docker Workflow: Matrix + Native ARM Runners — Design

Date: 2026-07-06
Status: Approved

## Goal

Cut the `v*` tag release time in `.github/workflows/release-docker.yml` from a
sequential 4-image build with QEMU-emulated arm64 to a fully parallel,
all-native build. Expected wall-clock: roughly the slowest single native image
build (~3-6 min) plus ~30 s of manifest merging, instead of the current
sequential sum with 5-10x QEMU penalties on the Go and npm stages.

## Non-goals

- Dockerfile changes (cross-compilation via `TARGETARCH` was considered and
  rejected in favor of native runners — zero Dockerfile churn).
- Atomicity across the four images (today a mid-run failure can also leave
  some images tagged and others not; unchanged).
- A `workflow_dispatch` dry-run trigger (add later if needed).
- The `artifact-sidecar` and dev/vite images (not released by this workflow
  today; unchanged).
- `type=gha` build-cache (`cache-from`/`cache-to`) was evaluated and dropped.
  This workflow only runs on tag refs, and each release is a new, distinct
  ref; GitHub Actions cache scopes are ref-isolated, so a tag-ref run can
  only restore a cache written by its own ref or by the default branch — and
  nothing on `main` ever runs this workflow to warm those scopes. The cache
  could therefore never hit across releases, while `mode=max` export still
  costs build time and consumes the repo's shared 10GB Actions cache quota
  (risking eviction of unrelated caches, e.g. CI's `setup-go` cache). If
  caching is wanted later, the way to do it is a `main`-branch warming build
  that writes the same cache scopes the release build reads from.

## Design

One workflow file rewrite, two job kinds. The repo is public, so
`ubuntu-24.04-arm` runners are free.

### `build` job — 8-way matrix

Matrix dimensions:

- `image`: `controller`, `agent`, `k8s-agent`, `runner` (Dockerfile =
  `docker/<image>.Dockerfile`)
- `platform`: `linux/amd64` (runs-on `ubuntu-latest`), `linux/arm64`
  (runs-on `ubuntu-24.04-arm`) — mapped via `include`, native on both, so the
  QEMU setup step is deleted entirely.

Each job: checkout → setup-buildx → GHCR login → `docker/build-push-action@v6`
with:

- `platforms:` the single matrix platform
- `outputs: type=image,name=ghcr.io/eirueimi/unified-cd-<image>,push-by-digest=true,name-canonical=true,push=true`
  (no tags at this stage)
- Export the image digest as a job artifact
  (`digests-<image>-<arch>` containing an empty file named by digest —
  the standard docker-docs multi-platform pattern)

### `merge` job — 4-way matrix, `needs: build`

Per image: download that image's digest artifacts → GHCR login →
`docker buildx imagetools create` with tags
`ghcr.io/eirueimi/unified-cd-<image>:latest` and
`...:${{ github.ref_name }}`, listing both per-arch digests → optional
`imagetools inspect` sanity check.

The merge stage runs only when the whole `build` matrix succeeded
(`needs: build` standard semantics), so a failed build never produces a
partial multi-arch manifest — and no tags move at all on any failure.

### Permissions / triggers

Unchanged: `on.push.tags: v*`; `contents: read, packages: write` (needed by
both job kinds).

## Failure modes

- Any build fails → ALL merge jobs are skipped and no tags move (stricter
  than today's sequential flow, which can leave earlier images tagged and
  later ones not — this is an atomicity improvement, not a regression).
  `fail-fast: false` still lets the remaining builds finish so their digest
  artifacts are available if only some jobs need retrying.
- Digest artifact lost between jobs → merge fails loudly; re-run the workflow.

## Operations note: GHCR untagged images are not orphans

Push-by-digest plus provenance attestations means every release leaves
several untagged package versions per image in GHCR (the per-arch digest
images and their attestation manifests) alongside the tagged multi-arch
index. These look orphaned in the GHCR UI because they carry no tag, but
they are referenced by the tagged multi-arch index (`latest` and the
release tag) — deleting them breaks pulls for one or more architectures.
GHCR never garbage-collects on its own, so untagged versions accumulate
release over release. Any future "delete untagged versions" cleanup
(scripted or via a GitHub Action) must exclude digests referenced by a
tagged index, or it will silently break `latest`/version pulls for at
least one architecture.

## Verification

- `actionlint` (or careful YAML review if unavailable) on the rewritten file.
- Real-world timing measured on the next `v*` tag push; compare the Actions
  run summary against the previous release run.
