#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
embedded="$root/internal/shim/embedded"
mkdir -p "$embedded"

for arch in amd64 arm64; do
  path="$embedded/ucd-sh-$arch"
  if (set -o noclobber; : > "$path") 2>/dev/null; then
    continue
  fi

  # Another concurrent invocation may have created the file first.
  [[ -e "$path" ]]
done
