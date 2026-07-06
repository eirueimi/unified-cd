# Release Docker Matrix + Native ARM Runners Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rewrite `.github/workflows/release-docker.yml` so the four release images build in an 8-way native matrix (amd64 on `ubuntu-latest`, arm64 on `ubuntu-24.04-arm`), followed by a 4-way manifest-merge stage.

**Architecture:** Two job kinds — `build` pushes single-arch images by digest (no tags), `merge` (`needs: build`) assembles the multi-arch manifests and applies the `latest` + tag-name tags via `docker buildx imagetools create`. Digests travel between jobs as artifacts.

**Tech Stack:** GitHub Actions, docker/build-push-action@v6, docker/setup-buildx-action@v3, actions/upload-artifact@v4 / download-artifact@v4, GHCR.

Spec: `docs/superpowers/specs/2026-07-06-release-docker-matrix-design.md`

## Global Constraints

- Trigger unchanged: `on.push.tags: 'v*'`. Tags unchanged: `latest` + `${{ github.ref_name }}` per image.
- Images and Dockerfiles unchanged: `controller`, `agent`, `k8s-agent`, `runner` ↔ `docker/<image>.Dockerfile`; image names `ghcr.io/eirueimi/unified-cd-<image>`.
- No QEMU step anywhere (all-native builds).
- Permissions per job: `contents: read`, `packages: write`.
- English only in the file (comments included).

---

### Task 1: rewrite the release workflow

**Files:**
- Modify: `.github/workflows/release-docker.yml` (full replacement)

**Interfaces:**
- Consumes: existing Dockerfiles under `docker/`, GHCR login via `GITHUB_TOKEN`.
- Produces: the final workflow — no other task depends on it.

- [ ] **Step 1: Replace the workflow file with the matrix version**

Full new content of `.github/workflows/release-docker.yml`:

```yaml
name: Release Docker Images

on:
  push:
    tags:
      - 'v*'

jobs:
  # Build each image natively per architecture and push it by digest only
  # (no tags yet). arm64 uses GitHub's free native ARM runners for public
  # repos — no QEMU anywhere.
  build:
    name: build (${{ matrix.image }}, ${{ matrix.arch }})
    runs-on: ${{ matrix.runner }}
    permissions:
      contents: read
      packages: write
    strategy:
      fail-fast: false
      matrix:
        image: [controller, agent, k8s-agent, runner]
        arch: [amd64, arm64]
        include:
          - arch: amd64
            runner: ubuntu-latest
            platform: linux/amd64
          - arch: arm64
            runner: ubuntu-24.04-arm
            platform: linux/arm64
    steps:
      - name: Checkout
        uses: actions/checkout@v4

      - name: Set up Docker Buildx
        uses: docker/setup-buildx-action@v3

      - name: Log in to GHCR
        uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - name: Build and push by digest
        id: build
        uses: docker/build-push-action@v6
        with:
          context: .
          file: docker/${{ matrix.image }}.Dockerfile
          platforms: ${{ matrix.platform }}
          outputs: type=image,name=ghcr.io/eirueimi/unified-cd-${{ matrix.image }},push-by-digest=true,name-canonical=true,push=true

      - name: Export digest
        run: |
          mkdir -p /tmp/digests
          digest="${{ steps.build.outputs.digest }}"
          touch "/tmp/digests/${digest#sha256:}"

      - name: Upload digest
        uses: actions/upload-artifact@v4
        with:
          name: digests-${{ matrix.image }}-${{ matrix.arch }}
          path: /tmp/digests/*
          if-no-files-found: error
          retention-days: 7

  # Assemble the multi-arch manifest per image and apply the release tags.
  # Runs only when the entire build matrix succeeded, so a failed build
  # never moves any tag.
  merge:
    name: merge (${{ matrix.image }})
    runs-on: ubuntu-latest
    needs: build
    permissions:
      contents: read
      packages: write
    strategy:
      fail-fast: false
      matrix:
        image: [controller, agent, k8s-agent, runner]
    steps:
      - name: Download digests
        uses: actions/download-artifact@v4
        with:
          pattern: digests-${{ matrix.image }}-*
          path: /tmp/digests
          merge-multiple: true

      - name: Set up Docker Buildx
        uses: docker/setup-buildx-action@v3

      - name: Log in to GHCR
        uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - name: Create multi-arch manifest and push tags
        working-directory: /tmp/digests
        env:
          TAG: ${{ github.ref_name }}
        run: |
          docker buildx imagetools create \
            -t ghcr.io/eirueimi/unified-cd-${{ matrix.image }}:latest \
            -t "ghcr.io/eirueimi/unified-cd-${{ matrix.image }}:$TAG" \
            $(printf 'ghcr.io/eirueimi/unified-cd-${{ matrix.image }}@sha256:%s ' *)

      - name: Inspect manifest
        env:
          TAG: ${{ github.ref_name }}
        run: docker buildx imagetools inspect "ghcr.io/eirueimi/unified-cd-${{ matrix.image }}:$TAG"
```

- [ ] **Step 2: Validate the workflow file**

Run (worktree root `C:/Users/arimax/unified-cd-project/unified-cd-ci-arm`):

```bash
# Structural YAML check (always available)
python -c "import yaml,sys; yaml.safe_load(open('.github/workflows/release-docker.yml')); print('yaml ok')"

# Semantic check if actionlint is installed (skip without failing if absent)
actionlint .github/workflows/release-docker.yml || which actionlint || echo "actionlint not installed - YAML check only"
```

Expected: `yaml ok`; actionlint (if present) reports no findings.

Manual review checklist against the file (each item must hold):
- Every `matrix.image` value has a matching `docker/<image>.Dockerfile` (`ls docker/`).
- `build` has no QEMU step.
- `outputs:` line contains `push-by-digest=true,name-canonical=true,push=true` and NO `tags:` key in the build step.
- Artifact name `digests-<image>-<arch>` matches the download `pattern: digests-<image>-*`.
- Both jobs declare `contents: read` / `packages: write`.

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/release-docker.yml
git commit -m "ci: parallelize release images across native amd64/arm64 runners

4 images x 2 architectures build concurrently by digest (arm64 on
ubuntu-24.04-arm - no QEMU), then a merge stage assembles the
multi-arch manifests. No tags move unless every build succeeds."
```

---

## Verification after merge to main

Real timing can only be measured on an actual `v*` tag push. After the branch
lands, compare the Actions run duration of the next release against the last
pre-change release run. If arm64 runner queue times turn out to be a
bottleneck, the fallback is cross-compilation (spec's rejected Approach B).
