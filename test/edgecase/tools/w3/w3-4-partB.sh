#!/usr/bin/env sh
# W3-4 Part B driver: trigger edge-logburst, arm the truncate fault across the
# burst, clear it, and record every instant with a host timestamp.
#
# Run from test/ha/ with COMPOSE_FILES and SCRATCH exported and
# MSYS_NO_PATHCONV=1 set. Usage: w3-4-partB.sh <attempt-number> [arm-delay-s] [hold-s] [timeout]
set -eu

N="${1:?attempt number required}"
ARM_DELAY="${2:-5}"
HOLD="${3:-8}"
TMO="${4:-200ms}"
SCRATCH="${SCRATCH:?SCRATCH required}"
TOOLS="$(cd "$(dirname "$0")" && pwd)"
OUT="$SCRATCH/partB-attempt$N.txt"

api() { curl -sS -H "Authorization: Bearer ha-admin-token" "$@"; }
ts() { date -u +%FT%T.%3NZ; }

{
  echo "=== Part B attempt $N  arm_delay=${ARM_DELAY}s hold=${HOLD}s timeout=$TMO ==="
  echo "$(ts) clear+probe (pre)"
  sh "$TOOLS/w3-4-logfault.sh" clear >/dev/null
  sh "$TOOLS/w3-4-logfault.sh" probe

  echo "$(ts) TRIGGER"
  body=$(api -H 'Content-Type: application/json' -d '{"jobName":"edge-logburst"}' \
         http://localhost:18080/api/v1/runs)
  echo "$body"
  RID=$(printf '%s' "$body" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  echo "RUNID=$RID"
  echo "$RID" > "$SCRATCH/partB-attempt$N.runid"

  sleep "$ARM_DELAY"
  echo "$(ts) ARM truncate $TMO"
  sh "$TOOLS/w3-4-logfault.sh" truncate "$TMO"
  sh "$TOOLS/w3-4-logfault.sh" probe

  sleep "$HOLD"
  echo "$(ts) CLEAR"
  sh "$TOOLS/w3-4-logfault.sh" clear
  sh "$TOOLS/w3-4-logfault.sh" probe
  echo "$(ts) window closed"
} 2>&1 | tee "$OUT"
