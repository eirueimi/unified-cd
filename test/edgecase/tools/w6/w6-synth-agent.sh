#!/usr/bin/env bash
# A curl-driven SYNTHETIC AGENT: enrolls a real agent identity, claims a run of
# an otherwise-unclaimable job, pushes log lines at it, and terminalises it —
# all through documented production HTTP routes. No SQL, no product change.
#
# PROVENANCE. This is the promotion of `w3-5/synth.sh` and `w3-6/synth.sh`,
# which were session artefacts with hardcoded absolute scratchpad paths and
# were never committed (`test/edgecase/README.md` §"which drivers are committed
# and which are evidence"). Both were the same instrument. Everything is now a
# parameter: agent id, label, job name, server, and state directory. There are
# no absolute paths in this file.
#
# WHY A SYNTHETIC AGENT AT ALL. The rig has exactly two agent identities and
# executes exactly two concurrent real runs (2 agents x MaxConcurrent default 1,
# `internal/agent/agent.go:218-221`). Anything that needs more agent identities
# than that, or needs a run whose lifecycle the harness controls exactly
# (claim held, finish deferred, logs pushed after a seal), cannot use them.
# Paired with an unclaimable fixture (a selector no real agent carries, e.g.
# `workloads/w35-probe` / `w36-probe` / `w6-trickle`'s `kind:w6synth` sibling),
# this gives a harness a run it exclusively owns.
#
# WHAT MAKES IT LEGITIMATE. `enroll` walks the product's own two-step
# enrollment path — POST /api/v1/agent-enrollments (admin PAT) -> one-time
# `uce_`, then POST /api/v1/agents/enroll (Bearer uce_) -> `uca_`/`ucr_`. The
# identity row it creates has enrollment_method = 'enrollment' and nothing on
# the request path ever consults that column (`internal/controller/
# agent_auth.go:38-100`; see README §"the enrollment bypass").
#
# LABELS COME FROM THE ENROLLMENT, NOT FROM THE AGENT. handleAgentClaim uses
# `principal.AuthorizedLabels` (`api_agent.go:143`), i.e. the labels baked into
# the credential here — a job's `agentSelector` must match THIS script's
# --label, not anything sent at register. `register` is therefore optional and
# is provided only because capabilities are the agent's own self-report
# (`api_agent.go:39-55`).
#
# Usage:
#   w6-synth-agent.sh enroll                       mint uce_ -> uca_/ucr_
#   w6-synth-agent.sh register [caps]              optional; default native,container
#   w6-synth-agent.sh heartbeat [runId...]  ALWAYS pass every run this identity
#                                           still owns — a heartbeat that omits
#                                           one FAILS it as orphaned (see the
#                                           verb body)
#   w6-synth-agent.sh trigger <job>                -> run id on stdout
#   w6-synth-agent.sh claim [timeout]              -> claimed run id on stdout ("" if none)
#   w6-synth-agent.sh finish <runId> [status]      default Succeeded
#   w6-synth-agent.sh lines <runId> <step> <prefix> <count>   bulk JSON body -> stdout
#   w6-synth-agent.sh push-bulk <runId> <step> <file>         POST that body
#   w6-synth-agent.sh own <job> [claimTimeout]     trigger + claim, -> run id
#   w6-synth-agent.sh run-once <job>               trigger + claim + finish, -> run id
#   w6-synth-agent.sh token                        print the access token (NOT redacted)
#   w6-synth-agent.sh forget                       delete the state dir
#
# Env:
#   UNIFIED_SERVER  base URL for BOTH admin and agent calls (default http://localhost:18080)
#   UNIFIED_TOKEN   admin PAT (default ha-admin-token)
#   W6_AGENT_ID     default w6-synth
#   W6_LABEL        default kind:w6synth
#   W6_STATE_DIR    default <repo>/.w6run/<agent id>
#
# The state dir holds live `uca_`/`ucr_` material and is gitignored. Every
# stdout line this script prints is redacted to a 4-char prefix except the
# explicit `token` verb, which exists so a caller can export it.
#
# NOTE FOR GIT BASH ON WINDOWS: this script deliberately does NOT export
# MSYS_NO_PATHCONV=1. The campaign's standing rule applies to `docker`/`compose`
# invocations with colon-bearing arguments; this script calls neither. Setting
# it here breaks every `curl -o` and `--data-binary @file`, because MSYS would
# then hand a native Windows curl.exe an unconverted `/tmp/...` path — measured
# as `curl: (23) Failure writing output to destination` while building this.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "${here}/../../../.." && pwd)"

server="${UNIFIED_SERVER:-http://localhost:18080}"
admin="${UNIFIED_TOKEN:-ha-admin-token}"
agent_id="${W6_AGENT_ID:-w6-synth}"
label="${W6_LABEL:-kind:w6synth}"
state="${W6_STATE_DIR:-${repo}/.w6run/${agent_id}}"
cred="${state}/credentials.json"

mkdir -p "${state}"

die() { echo "w6-synth-agent: $*" >&2; exit 1; }
jget() { python -c 'import json,sys;d=json.load(sys.stdin);print(d.get(sys.argv[1],"") if not isinstance(d,list) else "")' "$1"; }

need_cred() {
  [ -s "${cred}" ] || die "no credential at ${cred}; run 'enroll' first"
  ACCESS="$(python -c 'import json,sys;print(json.load(open(sys.argv[1]))["accessToken"])' "${cred}")"
}

adm() { curl -sS -H "Authorization: Bearer ${admin}" "$@"; }
agt() { curl -sS -H "Authorization: Bearer ${ACCESS}" "$@"; }

cmd="${1:-}"; shift || true

case "${cmd}" in

enroll)
  tmp="$(mktemp -d)"; trap 'rm -rf "${tmp}"' EXIT
  code=$(curl -sS -o "${tmp}/enr.json" -w '%{http_code}' -X POST "${server}/api/v1/agent-enrollments" \
    -H "Authorization: Bearer ${admin}" -H 'Content-Type: application/json' \
    -d "{\"agentId\":\"${agent_id}\",\"labels\":[\"${label}\"],\"expiresIn\":\"2h\"}")
  case "${code}" in 200|201) ;; *) cat "${tmp}/enr.json" >&2; die "enrollment create failed (${code})";; esac
  uce="$(jget token < "${tmp}/enr.json")"
  [ -n "${uce}" ] || die "enrollment response carried no token"

  code=$(curl -sS -o "${cred}.new" -w '%{http_code}' -X POST "${server}/api/v1/agents/enroll" \
    -H "Authorization: Bearer ${uce}" -H 'Content-Type: application/json' -d '{}')
  case "${code}" in 200) ;; *) cat "${cred}.new" >&2; rm -f "${cred}.new"; die "enrollment exchange failed (${code})";; esac
  mv "${cred}.new" "${cred}"; chmod 600 "${cred}" 2>/dev/null || true
  python - "${cred}" "${agent_id}" "${label}" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
print("enrolled  agentId=%s label=%s" % (sys.argv[2], sys.argv[3]))
print("  accessToken  = %s...(redacted)  expires %s" % (d["accessToken"][:4], d.get("accessExpiresAt")))
print("  refreshToken = %s...(redacted)" % d["refreshToken"][:4])
PY
  echo "  state dir    = ${state}"
  ;;

register)
  need_cred
  caps="${1:-native,container}"
  capjson=$(python -c 'import json,sys;print(json.dumps([c for c in sys.argv[1].split(",") if c]))' "${caps}")
  # The route is POST /api/v1/agents/register — a COLLECTION route, not
  # /agents/{agentId}/register (`internal/controller/server.go:493`); the id
  # travels in the body and must equal the principal's or the handler 403s
  # (`api_agent.go:34-38`). Getting that wrong yields a bare 404 with no hint.
  agt -o /dev/null -w 'register http_code=%{http_code}\n' -X POST -H 'Content-Type: application/json' \
    -d "{\"agentId\":\"${agent_id}\",\"labels\":[\"${label}\"],\"capabilities\":${capjson}}" \
    "${server}/api/v1/agents/register"
  ;;

heartbeat)
  # A HEARTBEAT WITH NO RUN IDS KILLS THIS AGENT'S OWN RUNS. Measured in W6-1:
  # a 25 s keepalive loop calling `heartbeat` with the old `-d '{}'` body
  # terminalised the very run the scenario was holding open, 4 s into the first
  # arm, and the arm then measured an already-terminal run without saying so.
  # `handleAgentHeartbeat` gates reconcile on BODY PRESENCE, not on the decoded
  # slice (`internal/controller/api_agent.go:88-101`, comment verbatim: "gated
  # on BODY PRESENCE (r.ContentLength != 0), not on the decoded slice being
  # non-nil"), so `{}` is ContentLength=2 and reports an EMPTY active set —
  # which is exactly the "the agent restarted and forgot its runs" signal, and
  # every reconcilable run of this agent is failed as orphaned.
  # So: pass every run this identity still owns, on every beat.
  need_cred
  ids=""
  for rid in "$@"; do
    [ -n "${ids}" ] && ids="${ids},"
    ids="${ids}\"${rid}\""
  done
  agt -o /dev/null -w 'heartbeat http_code=%{http_code}\n' -X POST -H 'Content-Type: application/json' \
    -d "{\"activeRunIds\":[${ids}]}" "${server}/api/v1/agents/${agent_id}/heartbeat"
  ;;

trigger)
  job="${1:?usage: trigger <job>}"
  adm -X POST -H 'Content-Type: application/json' -d "{\"jobName\":\"${job}\"}" \
    "${server}/api/v1/runs" | jget id
  ;;

claim)
  need_cred
  t="${1:-10s}"
  agt -X POST "${server}/api/v1/agents/${agent_id}/claim?timeout=${t}" | jget runId
  ;;

finish)
  need_cred
  rid="${1:?usage: finish <runId> [status]}"; st="${2:-Succeeded}"
  agt -o /dev/null -w "finish ${rid} ${st} http_code=%{http_code}\n" -X POST \
    -H 'Content-Type: application/json' -d "{\"status\":\"${st}\"}" \
    "${server}/api/v1/agents/${agent_id}/runs/${rid}/finish"
  ;;

lines)
  rid="${1:?usage: lines <runId> <step> <prefix> <count>}"; step="${2:?}"; pref="${3:?}"; n="${4:?}"
  python - "${rid}" "${step}" "${pref}" "${n}" <<'PY'
import sys, json, datetime
rid, step, pref, n = sys.argv[1], int(sys.argv[2]), sys.argv[3], int(sys.argv[4])
ts = datetime.datetime.now(datetime.timezone.utc).isoformat().replace('+00:00', 'Z')
print(json.dumps([{"runId": rid, "stepIndex": step, "stream": "stdout",
                   "timestamp": ts, "line": "%s-%d" % (pref, i)} for i in range(1, n + 1)]))
PY
  ;;

push-bulk)
  need_cred
  rid="${1:?usage: push-bulk <runId> <step> <bodyfile>}"; step="${2:?}"; body="${3:?}"
  curl -sS -o /dev/null -w '%{http_code} size_upload=%{size_upload} time_total=%{time_total}\n' \
    -H "Authorization: Bearer ${ACCESS}" -H 'Content-Type: application/json' \
    -X POST --data-binary "@${body}" \
    "${server}/api/v1/agents/${agent_id}/runs/${rid}/steps/${step}/logs/bulk"
  ;;

own|run-once)
  need_cred
  job="${1:?usage: ${cmd} <job> [claimTimeout]}"; t="${2:-15s}"
  rid=$(adm -X POST -H 'Content-Type: application/json' -d "{\"jobName\":\"${job}\"}" \
        "${server}/api/v1/runs" | jget id)
  [ -n "${rid}" ] || die "trigger of ${job} returned no run id"
  claimed=$(agt -X POST "${server}/api/v1/agents/${agent_id}/claim?timeout=${t}" | jget runId)
  [ "${claimed}" = "${rid}" ] || die "claimed ${claimed:-<nothing>}, expected ${rid} (another agent got there first, or the selector matches a real agent)"
  if [ "${cmd}" = "run-once" ]; then
    agt -o /dev/null -X POST -H 'Content-Type: application/json' -d '{"status":"Succeeded"}' \
      "${server}/api/v1/agents/${agent_id}/runs/${rid}/finish"
  fi
  echo "${rid}"
  ;;

token)
  need_cred; printf '%s\n' "${ACCESS}"
  ;;

forget)
  rm -rf "${state}"; echo "removed ${state}"
  ;;

*)
  sed -n '/^# Usage:/,/^# Env:/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' >&2
  exit 2
  ;;
esac
