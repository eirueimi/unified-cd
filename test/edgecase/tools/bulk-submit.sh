#!/usr/bin/env sh
# Bulk run submission for the edge-case campaign (W2-9 needs >50 Pending runs).
# Usage: bulk-submit.sh <job-name> <count>
# Prints one run id per line on stdout (so the caller can `tee ids.txt`);
# progress and errors go to stderr. The job must already be applied.
#   UNIFIED_SERVER=http://localhost:18080 UNIFIED_TOKEN=ha-admin-token \
#     ./bulk-submit.sh edge-mutex-hog 55 | tee /tmp/hog-ids.txt
set -eu

SERVER="${UNIFIED_SERVER:-http://localhost:18080}"
TOKEN="${UNIFIED_TOKEN:-ha-admin-token}"

job="${1:?usage: bulk-submit.sh <job-name> <count>}"
count="${2:?usage: bulk-submit.sh <job-name> <count>}"

i=1
while [ "$i" -le "$count" ]; do
  # POST /api/v1/runs {"jobName":"..."} is the trigger endpoint
  # (internal/controller/server.go:370 -> handleTriggerRun); it returns the
  # full api.Run JSON, whose "id" is the run id.
  body=$(curl -fsS -X POST "$SERVER/api/v1/runs" \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -d "{\"jobName\":\"$job\"}") || {
    echo "bulk-submit: trigger $i/$count failed for job $job" >&2
    exit 1
  }
  # Extract .id without depending on jq being present in the container/host.
  # Split on commas first so the (greedy, POSIX) sed cannot skip past the
  # top-level "id" to a later one; api.Run declares "id" first
  # (internal/api/types.go:52), so the first match is the run id.
  id=$(printf '%s' "$body" | tr ',' '\n' |
    sed -n 's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
  if [ -z "$id" ]; then
    echo "bulk-submit: no id in response for trigger $i/$count: $body" >&2
    exit 1
  fi
  echo "$id"
  i=$((i + 1))
done

echo "bulk-submit: submitted $count runs of $job" >&2
