#!/usr/bin/env bash
# Fail a CI job whose real-runtime tests passed by skipping.
#
# `go test` reports a skipped test as ok, so a leg whose runtime, cluster or shim
# image silently went missing would go green having tested nothing. This reads
# the `go test -json` stream and fails when:
#   - a test whose name matches FILTER was skipped and is not matched by ALLOW
#     (an explicit allowlist, e.g. the host-only cases on Kubernetes), or
#   - the number of passed subtests under a prefix differs from the expected
#     count given as PREFIX=COUNT.
#
# usage: check-test-json.sh RESULTS.json FILTER_REGEX ALLOW_SKIP_REGEX [PREFIX=COUNT ...]
set -euo pipefail

results=$1 filter=$2 allow=$3
shift 3
status=0

skipped=$(jq -r --arg f "$filter" --arg a "$allow" \
  'select(.Action=="skip" and .Test != null and (.Test|test($f)) and ((.Test|test($a))|not)) | .Test' "$results" | sort -u)
if [ -n "$skipped" ]; then
  echo "::error::tests were skipped, which a required CI leg must not do:"
  echo "$skipped"
  status=1
fi

for spec in "$@"; do
  prefix=${spec%=*} want=${spec##*=}
  got=$(jq -r --arg p "$prefix" 'select(.Action=="pass" and .Test != null and (.Test|startswith($p))) | .Test' "$results" | sort -u | wc -l)
  if [ "$got" -ne "$want" ]; then
    echo "::error::expected $want passed subtests under $prefix, saw $got"
    status=1
  fi
done
exit $status
