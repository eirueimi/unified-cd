#!/usr/bin/env bash
# W4 fault-injection helpers for the Kubernetes rig. This is the FIRST k8s
# fault tooling in this repo; tools/inject.sh is useless here by construction —
# its verbs take compose SERVICE names and hardcode `unified-cd-ha_default` /
# `unified-cd-ha-$svc-1`, and the k8s agent is neither a compose service nor a
# container on that network.
#
# Usage: w4-k8s-inject.sh <command> [args]
#
#   pods                      list this agent's run Pods (label app=unified-cd-agent)
#   delete-pod <runId|latest> delete a run's Pod by its unified-cd/runId label
#   annotations [pod]         dump the pool annotations (pool-status/key/run-id)
#   block [mode]              arm the one-way agent->controller partition
#   unblock                   clear it
#   show                      print the current arm state
#   probe                     measure the arm: one request through the proxy
#
# ARM RULE (README "a verb is verified when SOME capture measures its effect"):
# `block` and `unblock` end by probing THROUGH the interposer and printing the
# result, and `delete-pod` prints the pod list before and after. W3-1 found
# `s3-slow` emitting a directive nginx silently ignored while every
# state-inspection check passed, so state inspection is not confirmation here.
#
# Env: W4_NAMESPACE (default ci), W4_LISTEN (default 127.0.0.1:18099),
#      W4_LOG_DIR (default <repo>/.w4run)
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "${here}/../../../.." && pwd)"
ns="${W4_NAMESPACE:-ci}"
listen="${W4_LISTEN:-127.0.0.1:18099}"
logdir="${W4_LOG_DIR:-${repo}/.w4run}"
arm="${logdir}/block.arm"
sel="app=unified-cd-agent"

cmd="${1:?usage: w4-k8s-inject.sh <pods|delete-pod|annotations|block|unblock|show|probe> [args]}"

list_pods() { kubectl -n "${ns}" get pods -l "${sel}" -o wide --no-headers 2>/dev/null || true; }

# probe_proxy issues ONE request through the interposer and prints what came
# back. curl's exit code matters as much as the status: `block reset` produces
# NO status line (exit 52/56), which is the point — it is a transport failure,
# not a controller answer.
probe_proxy() {
  out=$(curl -s -o /dev/null --max-time 5 -w 'http_code=%{http_code} time=%{time_total}s' \
    "http://${listen}/healthz" 2>&1) && rc=0 || rc=$?
  echo "  probe GET /healthz via ${listen}: ${out} curl_exit=${rc}"
  if [ "${rc}" -eq 0 ] && [ "${out#*http_code=}" = "200 time=${out##*time=}" ]; then :; fi
}

case "${cmd}" in
  pods)
    list_pods
    ;;

  delete-pod)
    # The k8s agent labels every run Pod `unified-cd/runId=<runId>`
    # (internal/k8sagent/podbuilder.go:78-81) and names it
    # `ucd-run-<first 16 chars of runId>`. Deleting by LABEL, not by name, so a
    # scenario can act on a run id it already has without re-deriving the
    # truncation.
    target="${2:?usage: w4-k8s-inject.sh delete-pod <runId|latest>}"
    echo "-- before --"; list_pods
    if [ "${target}" = "latest" ]; then
      pod=$(kubectl -n "${ns}" get pods -l "${sel}" \
        --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1:].metadata.name}')
      [ -n "${pod}" ] || { echo "w4-k8s-inject: no run pod to delete" >&2; exit 1; }
      kubectl -n "${ns}" delete pod "${pod}" --wait=false
    else
      kubectl -n "${ns}" delete pods -l "unified-cd/runId=${target}" --wait=false
    fi
    echo "-- after --"; list_pods
    ;;

  annotations)
    # internal/k8sagent/pool.go:20-31 — annoPoolKey/annoPoolStatus/annoPoolRunID
    # ("unified-cd/pool-key", "-status", "-run-id"), plus the human-readable
    # "unified-cd/pool-template" which is NOT the pool index.
    pod="${2:-}"
    if [ -n "${pod}" ]; then pods="${pod}"; else
      pods=$(kubectl -n "${ns}" get pods -l "${sel}" -o jsonpath='{.items[*].metadata.name}')
    fi
    [ -n "${pods}" ] || { echo "(no run pods)"; exit 0; }
    for p in ${pods}; do
      echo "== ${p} =="
      kubectl -n "${ns}" get pod "${p}" -o jsonpath=\
'  pool-status  = {.metadata.annotations.unified-cd/pool-status}{"\n"}'\
'  pool-key     = {.metadata.annotations.unified-cd/pool-key}{"\n"}'\
'  pool-run-id  = {.metadata.annotations.unified-cd/pool-run-id}{"\n"}'\
'  pool-template= {.metadata.annotations.unified-cd/pool-template}{"\n"}'\
'  label runId  = {.metadata.labels.unified-cd/runId}{"\n"}'\
'  phase        = {.status.phase}{"\n"}'
    done
    ;;

  block)
    # One-way agent -> controller partition, scoped to THIS agent.
    #
    # WHY NOT inject.sh's nginx-block, and why not "kill all three
    # controllers": nginx-block resolves an agent's source IP with
    # `docker inspect` on a compose container, and a HOST-RUN agent has no
    # compose container — its traffic arrives at nginx from the Docker host
    # address it shares with every curl the scenario itself makes, so an IP
    # deny would also cut the instrument. Killing all three controllers is
    # blunt in the other direction: it stops the controller's own timers
    # (reapers, archiver, scheduler), which are exactly what W4-1 needs to keep
    # running while the agent is isolated.
    #
    # The interposer already sits alone in front of this one agent, so arming
    # it is both surgical and one-way: the controller keeps running and keeps
    # serving agent1/agent2 and the admin API.
    mode="${2:-reset}"
    mkdir -p "${logdir}"
    printf '%s\n' "${mode}" > "${arm}"
    echo "w4-k8s-inject: agent->controller partition ARMED (mode=${mode})"
    probe_proxy
    echo "  control (must still work — the controller is NOT down):"
    curl -s -o /dev/null --max-time 5 -w '    direct GET http://localhost:18080/healthz -> %{http_code}\n' \
      http://localhost:18080/healthz
    ;;

  unblock)
    rm -f "${arm}"
    echo "w4-k8s-inject: partition CLEARED"
    probe_proxy
    ;;

  show)
    if [ -f "${arm}" ]; then echo "armed: mode=$(cat "${arm}")"; else echo "unarmed"; fi
    probe_proxy
    ;;

  probe)
    probe_proxy
    ;;

  *) echo "unknown command: ${cmd}" >&2; exit 2 ;;
esac
