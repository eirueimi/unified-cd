#!/usr/bin/env bash
# Builds the three install bundles from the Helm chart in charts/unified-cd.
#
# Usage: scripts/build-manifests.sh <output-dir> [image-tag]
#
# With an image tag, the two first-party images are pinned to it — this is what
# the release workflow does, so a released bundle names the images that release
# published. Without one, the chart's own default (its appVersion) is used,
# which is what a local build wants. Third-party images (postgres, garage) are
# pinned in the chart's values and are never rewritten here.
#
# With an image tag the chart is also rendered from a temporary copy whose
# Chart.yaml says `version: <tag without v>` and `appVersion: <tag>`, so the
# bundle's helm.sh/chart and app.kubernetes.io/version labels name the release
# instead of the checked-in 0.0.0-dev / latest. This is safe for in-place
# `kubectl apply` upgrades only because no immutable field (a Job pod template
# or a Deployment selector) carries those labels.
#
# Each bundle is the `unified-cd` Namespace followed by
# `helm template unified-cd charts/unified-cd --namespace unified-cd` with the
# matching values file from charts/unified-cd/bundles/.
set -euo pipefail

out_dir=${1:?usage: build-manifests.sh <output-dir> [image-tag]}
image_tag=${2:-}

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
chart_dir="$repo_root/charts/unified-cd"
mkdir -p "$out_dir"

bundles=(core-install install agent-only)

command -v helm >/dev/null || {
  echo "helm not found; it is required to render the manifests" >&2
  exit 1
}

set_args=()
if [ -n "$image_tag" ]; then
  set_args=(--set "controller.image.tag=$image_tag,agent.image.tag=$image_tag")
  tmp_chart=$(mktemp -d)
  trap 'rm -rf "$tmp_chart"' EXIT
  cp -R "$chart_dir" "$tmp_chart/unified-cd"
  chart_dir="$tmp_chart/unified-cd"
  sed -i.bak -e "s/^version:.*/version: ${image_tag#v}/" -e "s/^appVersion:.*/appVersion: \"$image_tag\"/" "$chart_dir/Chart.yaml"
  rm -f "$chart_dir/Chart.yaml.bak"
fi

for bundle in "${bundles[@]}"; do
  {
    # The chart never renders the release namespace; the bundle must create it.
    printf 'apiVersion: v1\nkind: Namespace\nmetadata:\n  name: unified-cd\n---\n'
    helm template unified-cd "$chart_dir" --namespace unified-cd \
      -f "$chart_dir/bundles/$bundle.values.yaml" ${set_args[@]+"${set_args[@]}"}
  } > "$out_dir/$bundle.yaml"
  echo "wrote $out_dir/$bundle.yaml"
done
