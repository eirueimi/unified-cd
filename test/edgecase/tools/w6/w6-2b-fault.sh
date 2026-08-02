#!/usr/bin/env sh
# W6-2b injector: fault the agent log-bulk endpoint only, in ways that leave
# the controller UNTOUCHED.
#
# ============ WHY THIS EXISTS AND w3-4-logfault.sh DOES NOT SUFFICE =========
# tools/w3/w3-4-logfault.sh has two arms and both are PARTIAL-COMMIT faults by
# design, because W3-4 was measuring duplication:
#
#   truncate  nginx 504s MID-LOOP, so the prefix of the batch is already
#             committed upstream when the agent sees the failure.
#   lostack   the request is MIRRORED to a real controller, which commits the
#             WHOLE batch, and only the client-facing leg fails.
#
# W6-2b measures the RESEND CURVE — how many requests the LogPusher issues as
# its backlog grows. Under either W3-4 arm every resend also inserts rows, so
# the curve would be a duplicate-insert curve and the run's line accounting
# would be meaningless. W6-2b needs failures in which NOTHING reaches a
# controller. Hence three new arms. `clear` is W3-4's, verbatim in effect.
#
# Requires compose/w62b.override.yaml (i.e. compose/nginx-w62b.conf). It is a
# NO-OP against test/ha/nginx.conf, nginx-edge.conf, or nginx-logfault.conf —
# `flap` and `hang` reference an upstream/variable those files do not define,
# so nginx -t FAILS LOUDLY there rather than arming inert. `outage` and `clear`
# would silently work against nginx-logfault.conf; ALWAYS probe-confirm anyway
# (W2-5 lesson: two verbs have shipped inert in this campaign).
#
# Run from test/ha/ with COMPOSE_FILES exported.
#
# Usage: w6-2b-fault.sh <clear|outage|flap|hang|show|probe|hangprobe> [arg]
#   clear         restore test/ha parity (300s read timeout, 2s connect,
#                 3-try next_upstream) and arm=clear so the access log says so
#   outage        route the bulk URI to the dead `blackhole` upstream. Instant
#                 502, NO mirror, so no controller ever sees the body. This is
#                 "the controller is unreachable" with nothing committed.
#   flap [50]     route the bulk URI to $w62b_split, which nginx-w62b.conf maps
#                 per-$request_id to blackhole or controllers. The ratio is
#                 COMPILED INTO nginx-w62b.conf (50/50) and the argument is
#                 accepted only to be REJECTED if it disagrees — split_clients
#                 is http-level and cannot be set from this include, and an
#                 arm that silently ignored its own ratio argument is exactly
#                 the class of inert instrument this campaign keeps shipping.
#   hang          route the bulk URI to `hangsink`, which accepts the TCP
#                 connection and never answers, with proxy_read_timeout 600s so
#                 nginx does not answer on the agent's behalf. The agent's own
#                 60s http.Client timeout (internal/agent/client.go:53) is what
#                 ends each request. THIS IS THE BLACK-HOLE ARM.
#   show          print the include file currently in the nginx container
#   probe         issue an unauthenticated request to a bulk URI and print the
#                 X-Logfault-Arm response header and status. Proves the regex
#                 location matched and which arm a FRESH connection gets. It
#                 does NOT prove anything about the agent's existing keepalive
#                 connection — for that, read the access log's arm= column for
#                 the agent's own requests.
#   hangprobe     probe variant for `hang`: `probe` would BLOCK for 600s. This
#                 uses --max-time 12 and asserts curl exit 28 (timeout), which
#                 is the positive evidence that the arm hangs rather than
#                 fast-failing. A fake black hole that is really a fast-fail is
#                 the specific failure this verb exists to rule out.
set -eu

COMPOSE_FILES="${COMPOSE_FILES:--f docker-compose.ha.yaml}"
LB="${UNIFIED_SERVER:-http://localhost:18080}"
BULK="/api/v1/agents/probe/runs/00000000-0000-0000-0000-000000000000/steps/0/logs/bulk"
dc() { docker compose $COMPOSE_FILES "$@"; }

mode="${1:?usage: w6-2b-fault.sh <clear|outage|flap|hang|show|probe|hangprobe> [arg]}"

case "$mode" in
  show)
    dc exec -T nginx sh -c 'cat /etc/nginx/logfault/fault.conf 2>/dev/null || echo "(no fault.conf)"'
    exit 0
    ;;
  probe)
    printf '%s probe ' "$(date -u +%FT%T.%3NZ)"
    curl -sS -o /dev/null -D - -X POST -H 'Content-Type: application/json' -d '[]' \
      "${LB}${BULK}" | tr -d '\r' | grep -iE '^(HTTP/|X-Logfault-Arm)' | tr '\n' ' '
    echo
    exit 0
    ;;
  hangprobe)
    printf '%s hangprobe ' "$(date -u +%FT%T.%3NZ)"
    set +e
    body=$(curl -sS --max-time 12 -o /dev/null -w '%{http_code} %{time_total}' \
      -X POST -H 'Content-Type: application/json' -d '[]' "${LB}${BULK}" 2>&1)
    rc=$?
    set -e
    if [ "$rc" -eq 28 ]; then
      echo "PASS curl exit 28 (timeout after 12s) -> the endpoint HANGS, it does not fast-fail"
      exit 0
    fi
    echo "FAIL curl exit ${rc} out='${body}' -> the endpoint answered; this is NOT a black hole"
    exit 1
    ;;
  clear)
    body='set $logfault_arm clear;
proxy_connect_timeout 2s;
proxy_read_timeout 300s;
proxy_next_upstream error timeout http_502 http_503 http_504;
proxy_next_upstream_tries 3;
proxy_next_upstream_timeout 8s;'
    ;;
  outage)
    body='set $logfault_arm outage;
set $logfault_target blackhole;
proxy_connect_timeout 2s;
proxy_read_timeout 300s;
proxy_next_upstream off;'
    ;;
  flap)
    want="${2:-50}"
    if [ "$want" != "50" ]; then
      echo "w6-2b-fault.sh: flap ratio is compiled into compose/nginx-w62b.conf as 50/50." >&2
      echo "  split_clients is an http-level directive and cannot be written from this" >&2
      echo "  location include. Refusing '${want}' rather than arming 50 and reporting" >&2
      echo "  ${want}. Edit nginx-w62b.conf's split_clients block and reload to change it." >&2
      exit 2
    fi
    body='set $logfault_arm flap;
set $logfault_target $w62b_split;
proxy_connect_timeout 2s;
proxy_read_timeout 300s;
proxy_next_upstream off;'
    ;;
  hang)
    body='set $logfault_arm hang;
set $logfault_target hangsink;
proxy_connect_timeout 2s;
proxy_read_timeout 600s;
proxy_next_upstream off;'
    ;;
  *) echo "unknown mode: $mode" >&2; exit 2 ;;
esac

printf '%s\n' "$body" | dc exec -T nginx sh -c \
  'mkdir -p /etc/nginx/logfault && cat > /etc/nginx/logfault/fault.conf && nginx -t && nginx -s reload'
echo "w6-2b-fault: $mode written+reloaded at $(date -u +%FT%T.%3NZ)"
