#!/usr/bin/env bash
# W4 rig teardown: stop the agent and the interposer, and CAPTURE each
# process's final output before it goes away (campaign rule: "Kill every
# background process you start and capture its final output").
#
# The agent is stopped with SIGTERM first so its graceful-drain path runs
# (agentlib.ShutdownContext; a second signal forces immediate shutdown and
# reconciles in-flight runs). Compose is NOT touched — the stack is torn down
# separately, see scenarios/w4-rig.md §Teardown.
#
# Usage:  test/edgecase/tools/w4/w4-down.sh [capture-dir]
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "${here}/../../../.." && pwd)"
logdir="${W4_LOG_DIR:-${repo}/.w4run}"
capture="${1:-}"

stop() {
  name="$1"; pidfile="${logdir}/${name}.pid"
  [ -f "${pidfile}" ] || { echo "w4-down: no ${name}.pid"; return 0; }
  pid="$(cat "${pidfile}")"
  if kill -0 "${pid}" 2>/dev/null; then
    kill -TERM "${pid}" 2>/dev/null
    for _ in $(seq 1 20); do
      kill -0 "${pid}" 2>/dev/null || break
      sleep 0.5
    done
    if kill -0 "${pid}" 2>/dev/null; then
      echo "w4-down: ${name} (pid ${pid}) ignored SIGTERM; SIGKILL"
      kill -KILL "${pid}" 2>/dev/null
    fi
    echo "w4-down: stopped ${name} (pid ${pid})"
  else
    echo "w4-down: ${name} (pid ${pid}) was already gone"
  fi
  rm -f "${pidfile}"
  echo "--- ${name} final output (last 25 lines of ${logdir}/${name}.log) ---"
  tail -25 "${logdir}/${name}.log" 2>/dev/null
}

stop k8s-agent
stop enrollproxy

# The interposer logs one INTERCEPT line per intercepted enrollment. That count
# is the bypass's own evidence: if it is 0, the agent never went through the
# interposer and any claim about the rig is unsupported.
n=$(grep -c '^.*INTERCEPT #' "${logdir}/enrollproxy.log" 2>/dev/null || echo 0)
echo "w4-down: interposer answered ${n} enrollment exchange(s) this session"

if [ -n "${capture}" ]; then
  mkdir -p "${capture}"
  cp -f "${logdir}"/*.log "${capture}/" 2>/dev/null
  echo "w4-down: copied logs to ${capture}/"
fi
