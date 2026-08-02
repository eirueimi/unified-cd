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
# `block` probes THROUGH the interposer and then ASSERTS the probe failed in
# the shape the mode promises, exiting non-zero if it did not; `unblock`
# asserts the proxy answers 200 again; `delete-pod` prints the pod list before
# and after. W3-1 found `s3-slow` emitting a directive nginx silently ignored
# while every state-inspection check passed, so state inspection is not
# confirmation here — and printing a probe without asserting on it is state
# inspection wearing a measurement's clothes.
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

# probe_proxy issues ONE request through the interposer, prints what came back,
# and leaves it in PROBE_CODE / PROBE_RC for the caller to ASSERT on. curl's
# exit code matters as much as the status: `block reset` produces NO status
# line (exit 52/56), which is the point — it is a transport failure, not a
# controller answer. `block hang` produces no answer at all (exit 28).
PROBE_CODE=""
PROBE_RC=0
probe_proxy() {
  out=$(curl -s -o /dev/null --max-time 5 -w '%{http_code} %{time_total}' \
    "http://${listen}/healthz" 2>&1) && PROBE_RC=0 || PROBE_RC=$?
  PROBE_CODE="${out%% *}"
  echo "  probe GET /healthz via ${listen}: http_code=${PROBE_CODE} time=${out##* }s curl_exit=${PROBE_RC}"
}

# assert_armed turns the probe into a VERIFICATION rather than a printout.
# Without it, an interposer started with no -block-file ignores the arm file
# completely and `block` would still print "ARMED" and exit 0 — exactly the
# class of silently-inert verb the W3-1 s3-slow lesson is about.
assert_armed() {
  case "$1" in
    hang)
      # accepted and never answered: curl gives up at --max-time, exit 28.
      if [ "${PROBE_RC}" -ne 28 ]; then
        echo "w4-k8s-inject: FAILED to arm 'hang' — probe returned http_code=${PROBE_CODE} curl_exit=${PROBE_RC}, expected curl_exit=28 (timeout). Is the interposer running with -block-file ${arm}?" >&2
        exit 1
      fi
      ;;
    [1-5][0-9][0-9])
      if [ "${PROBE_CODE}" != "$1" ]; then
        echo "w4-k8s-inject: FAILED to arm '$1' — probe returned http_code=${PROBE_CODE} curl_exit=${PROBE_RC}. Is the interposer running with -block-file ${arm}?" >&2
        exit 1
      fi
      ;;
    *) # reset
      if [ "${PROBE_RC}" -eq 0 ]; then
        echo "w4-k8s-inject: FAILED to arm 'reset' — probe still answered http_code=${PROBE_CODE}. Is the interposer running with -block-file ${arm}?" >&2
        exit 1
      fi
      ;;
  esac
  echo "  ARM VERIFIED (mode=$1)"
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
    # Settle past ONE watchArm tick (200 ms) before probing. The interposer
    # severs every live connection on the arm TRANSITION, and a probe issued
    # inside that window is severed too — MEASURED: `block hang` probed
    # immediately returns curl_exit=52 at 0.106 s (the sever), not the
    # curl_exit=28 the mode promises. Probing after the transition measures the
    # steady-state arm, which is what the mode's contract is about.
    sleep 0.5
    echo "w4-k8s-inject: agent->controller partition ARMED (mode=${mode})"
    probe_proxy
    assert_armed "${mode}"
    echo "  control (must still work — the controller is NOT down):"
    curl -s -o /dev/null --max-time 5 -w '    direct GET http://localhost:18080/healthz -> %{http_code}\n' \
      http://localhost:18080/healthz
    ;;

  unblock)
    rm -f "${arm}"
    echo "w4-k8s-inject: partition CLEARED"
    probe_proxy
    if [ "${PROBE_CODE}" != "200" ]; then
      echo "w4-k8s-inject: WARNING the proxy did not answer 200 after unblock (http_code=${PROBE_CODE} curl_exit=${PROBE_RC})" >&2
      exit 1
    fi
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
