#!/usr/bin/env bash
# Builds the three install bundles from the kustomize overlays.
#
# Usage: scripts/build-manifests.sh <output-dir> [image-tag]
#
# With an image tag, the two first-party images are pinned to it — this is what
# the release workflow does, so a released bundle names the images that release
# published. Without one, the overlays' own values are used, which is what a
# local build wants. Third-party images (postgres, garage) are already pinned
# in the base and are never rewritten here.
set -euo pipefail

out_dir=${1:?usage: build-manifests.sh <output-dir> [image-tag]}
image_tag=${2:-}

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
mkdir -p "$out_dir"

overlays=(core-install install agent-only)

if [ -n "$image_tag" ]; then
  # kubectl embeds kustomize for `kubectl kustomize` but does not expose
  # `kustomize edit`, so the standalone CLI is required for this branch.
  command -v kustomize >/dev/null || {
    echo "kustomize CLI not found; it is required to pin an image tag" >&2
    exit 1
  }
  for overlay in "${overlays[@]}"; do
    (
      cd "$repo_root/manifests/$overlay"
      kustomize edit set image \
        "ghcr.io/eirueimi/unified-cd-controller=ghcr.io/eirueimi/unified-cd-controller:$image_tag" \
        "ghcr.io/eirueimi/unified-cd-k8s-agent=ghcr.io/eirueimi/unified-cd-k8s-agent:$image_tag"
    )
  done
fi

for overlay in "${overlays[@]}"; do
  kubectl kustomize "$repo_root/manifests/$overlay" > "$out_dir/$overlay.yaml"
  echo "wrote $out_dir/$overlay.yaml"
done
