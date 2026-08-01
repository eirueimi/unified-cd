#!/usr/bin/env bash
# W4 rig: bring up the host-run Kubernetes agent behind the enrollment
# interposer. Assumes the HA compose stack is already up (see
# scenarios/w4-rig.md §Bring-up for the exact compose invocation).
#
# Steps, in order:
#   1. build k8s-agent + enrollproxy into a gitignored bin/ (never `go run`:
#      `go run` leaves the real process as a CHILD, so a kill on the wrapper
#      orphans the agent — the campaign rule is that every background process
#      is killed and its final output captured)
#   2. mint a real `uca_`/`ucr_` pair through the product's enrollment path
#   3. write the dummy ServiceAccount-token file the agent insists on reading
#   4. start the interposer, wait for it to answer
#   5. start the agent, wait for `k8s agent registered`
#
# Logs go to $W4_LOG_DIR (default ./.w4run), PIDs alongside them. Tear down
# with w4-down.sh, which captures each process's final output.
#
# Usage:  test/edgecase/tools/w4/w4-up.sh
# Env:    UNIFIED_SERVER (default http://localhost:18080)
#         UNIFIED_TOKEN  (default ha-admin-token)
#         W4_AGENT_ID    (default k8s-agent-w4)
#         W4_LABEL       (default kind:kubernetes)
#         W4_LISTEN      (default 127.0.0.1:18099)
#         W4_LOG_DIR     (default <repo>/.w4run)
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "${here}/../../../.." && pwd)"
cd "${repo}"

server="${UNIFIED_SERVER:-http://localhost:18080}"
agent_id="${W4_AGENT_ID:-k8s-agent-w4}"
label="${W4_LABEL:-kind:kubernetes}"
listen="${W4_LISTEN:-127.0.0.1:18099}"
logdir="${W4_LOG_DIR:-${repo}/.w4run}"
bindir="${here}/bin"
mkdir -p "${logdir}" "${bindir}"

echo "== w4-up: 1/5 build =="
# -buildvcs=false is REQUIRED in this worktree: plain `go build` fails with
# "error obtaining VCS status: exit status 128" (recorded in w4-0 step 4).
go build -buildvcs=false -o "${bindir}/k8s-agent" ./cmd/k8s-agent
go build -buildvcs=false -o "${bindir}/enrollproxy" ./test/edgecase/tools/w4/enrollproxy
echo "   built ${bindir}/k8s-agent and ${bindir}/enrollproxy"

echo "== w4-up: 2/5 mint credential (product enrollment path) =="
UNIFIED_SERVER="${server}" "${here}/w4-mint-credential.sh" "${agent_id}" "${label}"

echo "== w4-up: 3/5 dummy ServiceAccount token file =="
# Not credential material. readProjectedServiceAccountToken only requires the
# file to exist and be non-empty; the interposer never inspects the Bearer.
printf 'w4-rig-dummy-service-account-token-not-a-credential\n' > "${here}/w4-sa-token"

echo "== w4-up: 4/5 start interposer =="
"${bindir}/enrollproxy" \
  -listen "${listen}" \
  -upstream "${server}" \
  -credentials "${here}/w4-agent-credentials.json" \
  -block-file "${logdir}/block.arm" \
  > "${logdir}/enrollproxy.log" 2>&1 &
echo $! > "${logdir}/enrollproxy.pid"
for _ in $(seq 1 30); do
  if curl -s -o /dev/null --max-time 2 "http://${listen}/healthz"; then break; fi
  sleep 0.5
done
code=$(curl -s -o /dev/null -w '%{http_code}' "http://${listen}/healthz" || true)
echo "   interposer pid=$(cat "${logdir}/enrollproxy.pid") passthrough /healthz -> ${code}"
if [ "${code}" != "200" ]; then
  echo "w4-up: interposer is not forwarding; see ${logdir}/enrollproxy.log" >&2
  exit 1
fi

echo "== w4-up: 5/5 start k8s agent =="
"${bindir}/k8s-agent" --config ./test/edgecase/k8s/w4-agent-config.yaml --log-level debug \
  > "${logdir}/k8s-agent.log" 2>&1 &
echo $! > "${logdir}/k8s-agent.pid"
for _ in $(seq 1 60); do
  if grep -q 'k8s agent registered' "${logdir}/k8s-agent.log" 2>/dev/null; then break; fi
  if ! kill -0 "$(cat "${logdir}/k8s-agent.pid")" 2>/dev/null; then break; fi
  sleep 0.5
done
if grep -q 'k8s agent registered' "${logdir}/k8s-agent.log" 2>/dev/null; then
  grep -m1 'k8s agent registered' "${logdir}/k8s-agent.log"
  echo "w4-up: OK — agent ${agent_id} is up. Logs in ${logdir}/."
else
  echo "w4-up: agent did NOT register; last lines:" >&2
  tail -20 "${logdir}/k8s-agent.log" >&2
  exit 1
fi
