#!/usr/bin/env bash
# Build the W6 Go harnesses into a gitignored bin/ next to this script.
#
# NEVER `go run`: it leaves the real process as a CHILD of the wrapper, so a
# kill on the wrapper orphans the harness — and the campaign rule is that every
# background process is killed and its final output captured. Same reason
# `tools/w4/w4-up.sh` builds first.
#
# `-buildvcs=false` is REQUIRED in this worktree: plain `go build` fails with
# "error obtaining VCS status: exit status 128" (recorded in W4).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "${here}/../../../.." && pwd)"
bin="${here}/bin"
mkdir -p "${bin}"
cd "${repo}"

for cmd in loadgen ssehold; do
  go build -buildvcs=false -o "${bin}/${cmd}" "./test/edgecase/tools/w6/${cmd}"
  echo "built ${bin}/${cmd}"
done
