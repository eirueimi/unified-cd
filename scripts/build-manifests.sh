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

# kubectl embeds kustomize for `kubectl kustomize`; every path below needs it.
command -v kubectl >/dev/null || {
  echo "kubectl not found; it is required to render the manifests" >&2
  exit 1
}

if [ -n "$image_tag" ]; then
  # kubectl's embedded kustomize does not expose `kustomize edit`, so instead
  # of shelling out to the standalone CLI, append the `images:` block it would
  # have written directly. `kubectl kustomize` honours an `images:` block the
  # same way the standalone CLI does, so this needs no CLI at all.
  for overlay in "${overlays[@]}"; do
    kfile="$repo_root/manifests/$overlay/kustomization.yaml"
    if grep -q '^images:' "$kfile"; then
      echo "$kfile already has an images: block; refusing to append a second one" >&2
      exit 1
    fi
    cat >>"$kfile" <<EOF
images:
  - name: ghcr.io/eirueimi/unified-cd-controller
    newTag: $image_tag
  - name: ghcr.io/eirueimi/unified-cd-k8s-agent
    newTag: $image_tag
EOF
  done
fi

for overlay in "${overlays[@]}"; do
  kubectl kustomize "$repo_root/manifests/$overlay" > "$out_dir/$overlay.yaml"
  echo "wrote $out_dir/$overlay.yaml"
done
