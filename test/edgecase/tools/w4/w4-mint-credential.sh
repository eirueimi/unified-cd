#!/usr/bin/env bash
# W4 rig: mint the agent access credential the enrollment interposer serves.
#
# This is the PRODUCT'S OWN supported enrollment path — exactly what
# test/ha's `agent-enroll` service does for agent1/agent2
# (test/ha/docker-compose.ha.yaml:143-155), just with a different label:
#
#   POST /api/v1/agent-enrollments   (admin PAT)  -> one-time `uce_` token
#   POST /api/v1/agents/enroll       (Bearer uce_) -> `uca_` access + `ucr_` refresh
#
# Nothing about the credential is synthetic. The identity row it creates has
# enrollment_method = 'enrollment', and the controller's agent-auth middleware
# never consults that column (internal/controller/agent_auth.go:38-116), which
# is why the interposer works at all.
#
# CAPABILITIES ARE NOT SET HERE, and cannot be. `capabilities` on the identity
# stays empty: the controller takes an agent's capabilities from its OWN
# self-report at POST /api/v1/agents/register and validates them against
# dsl.ValidCapability (internal/controller/api_agent.go:39-55, "capabilities
# are the agent's own runtime auto-detection ... not an authorization
# boundary"). The k8s agent advertises ["pod","container"] unconditionally
# (internal/k8sagent/agent.go:59), so the right capabilities arrive the moment
# it registers. LABELS are the opposite: handleAgentRegister IGNORES the
# agent's requested labels and uses principal.AuthorizedLabels, so the label
# passed here is the one the job selector must match.
#
# The output file carries live credential material and is gitignored.
#
# Usage:
#   test/edgecase/tools/w4/w4-mint-credential.sh [agent-id] [label]
# Env:
#   UNIFIED_SERVER (default http://localhost:18080)
#   UNIFIED_TOKEN  (default ha-admin-token)
set -euo pipefail

agent_id="${1:-k8s-agent-w4}"
label="${2:-kind:kubernetes}"
server="${UNIFIED_SERVER:-http://localhost:18080}"
admin="${UNIFIED_TOKEN:-ha-admin-token}"

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
out="${here}/w4-agent-credentials.json"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

code=$(curl -s -o "${tmp}/enr.json" -w '%{http_code}' -X POST "${server}/api/v1/agent-enrollments" \
  -H "Authorization: Bearer ${admin}" -H 'Content-Type: application/json' \
  -d "{\"agentId\":\"${agent_id}\",\"expiresIn\":\"10m\",\"labels\":[\"${label}\"]}")
if [ "${code}" != "201" ] && [ "${code}" != "200" ]; then
  echo "w4-mint-credential: enrollment create failed (${code})" >&2
  cat "${tmp}/enr.json" >&2
  exit 1
fi
uce=$(python -c 'import json,sys;print(json.load(open(sys.argv[1]))["token"])' "${tmp}/enr.json")

code=$(curl -s -o "${out}.new" -w '%{http_code}' -X POST "${server}/api/v1/agents/enroll" \
  -H "Authorization: Bearer ${uce}" -H 'Content-Type: application/json' -d '{}')
if [ "${code}" != "200" ]; then
  echo "w4-mint-credential: enrollment exchange failed (${code})" >&2
  cat "${out}.new" >&2
  rm -f "${out}.new"
  exit 1
fi
mv "${out}.new" "${out}"
chmod 600 "${out}" 2>/dev/null || true

python - "${out}" "${agent_id}" "${label}" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
print("w4-mint-credential: wrote %s" % sys.argv[1])
print("  agentId        = %s" % d["agentId"])
print("  label          = %s (authorized on the identity; the job selector must match this)" % sys.argv[3])
print("  accessToken    = %s...(redacted)"  % d["accessToken"][:4])
print("  accessExpires  = %s" % d["accessExpiresAt"])
print("  refreshToken   = %s...(redacted)"  % d["refreshToken"][:4])
print("  capabilities   = %s  <- empty BY DESIGN; the agent self-reports [pod,container] at register" % d.get("capabilities"))
PY
