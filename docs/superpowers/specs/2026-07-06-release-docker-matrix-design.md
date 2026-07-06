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
- `cache-from`/`cache-to`: `type=gha` with `scope=<image>-<arch>` and
  `mode=max`, so Go module download / npm ci layers carry across releases
- Export the image digest as a job artifact
  (`digests-<image>-<arch>` containing an empty file named by digest —
  the standard docker-docs multi-platform pattern)

### `merge` job — 4-way matrix, `needs: build`

Per image: download that image's digest artifacts → GHCR login →
`docker buildx imagetools create` with tags
`ghcr.io/eirueimi/unified-cd-<image>:latest` and
`...:${{ github.ref_name }}`, listing both per-arch digests → optional
`imagetools inspect` sanity check.

Because merge only runs when all its image's builds succeeded, a failed arch
never produces a partial multi-arch manifest or moves the tags.

### Permissions / triggers

Unchanged: `on.push.tags: v*`; `contents: read, packages: write` (needed by
both job kinds).

## Failure modes

- One arch fails → that image's merge is skipped, tags untouched; other
  images proceed independently (same blast radius as today, minus QEMU
  flakiness).
- GHA cache miss/eviction → build falls back to a full native build; caches
  are best-effort only.
- Digest artifact lost between jobs → merge fails loudly; re-run the workflow.

## Verification

- `actionlint` (or careful YAML review if unavailable) on the rewritten file.
- Real-world timing measured on the next `v*` tag push; compare the Actions
  run summary against the previous release run.
