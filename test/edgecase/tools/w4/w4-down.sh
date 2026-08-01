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
# is the bypass's own evidence.
#
# READ IT CORRECTLY, because 0 is ambiguous and reporting it as a failure was a
# real bug in the first version of this script. The agent enrolls ONCE at
# startup and then not again until its cached access token is inside its
# refresh lead time — 15 min plus up to 5 min of jitter before a 1 h expiry, so
# roughly every 40-45 min (internal/k8sagent/credentials.go:83-84). A short
# session, or a proxy restarted underneath a still-running agent, legitimately
# shows 0. The count is over every enrollproxy.log* in the log dir so a restart
# during the session is still covered.
#
# 0 means "no enrollment was intercepted while these logs were being written" —
# NOT "the bypass was not in effect". The claim that rests on the bypass is
# supported by the INTERCEPT line at the agent's own startup, wherever that
# landed. If no log in the directory carries one, the rig was never brought up
# by w4-up.sh and any claim about it IS unsupported.
n=$(cat "${logdir}"/enrollproxy.log* 2>/dev/null | grep -c 'INTERCEPT #')
echo "w4-down: ${n} enrollment exchange(s) intercepted across ${logdir}/enrollproxy.log*"
if [ "${n}" -eq 0 ]; then
  echo "w4-down: NOTE 0 is expected for a session shorter than the ~40 min refresh"
  echo "         interval, or when the proxy was restarted under a running agent."
fi

if [ -n "${capture}" ]; then
  mkdir -p "${capture}"
  cp -f "${logdir}"/*.log "${capture}/" 2>/dev/null
  echo "w4-down: copied logs to ${capture}/"
fi
