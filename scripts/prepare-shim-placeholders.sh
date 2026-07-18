#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
embedded="$root/internal/shim/embedded"
mkdir -p "$embedded"

for arch in amd64 arm64; do
  path="$embedded/ucd-sh-$arch"
  if [[ ! -e "$path" ]]; then
    : > "$path"
  fi
done
