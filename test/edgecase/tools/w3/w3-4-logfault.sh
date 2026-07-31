#!/usr/bin/env sh
# W3-4 injector: fault the agent log-bulk endpoint only.
# Requires the compose/logfault.override.yaml overlay (nginx-logfault.conf);
# it is a NO-OP against test/ha/nginx.conf or nginx-edge.conf, so ALWAYS
# probe-confirm with `probe` after arming (W2-5 lesson).
#
# Run from test/ha/ with COMPOSE_FILES exported, e.g.
#   COMPOSE_FILES="-f docker-compose.ha.yaml -f ../edgecase/compose/logfault.override.yaml"
#
# Usage: w3-4-logfault.sh <clear|truncate|lostack|show|probe> [timeout]
#   clear             restore test/ha parity (300s read timeout, 3-try next_upstream)
#   truncate [200ms]  cut proxy_read_timeout so nginx 504s MID-LOOP; the closed
#                     upstream connection cancels the controller's request
#                     context, leaving the prefix already committed
#   lostack           mirror the request to a real controller (commits the whole
#                     batch) while the client-facing leg goes to a dead upstream
#                     (502 to the agent)
#   show              print the include file currently in the nginx container
#   probe             issue an unauthenticated request to a bulk URI and print
#                     the X-Logfault-Arm response header; this proves BOTH that
#                     the regex location matched and which arm the worker
#                     serving a FRESH connection has loaded. It does NOT prove
#                     anything about the agent's existing keepalive connection —
#                     for that, read the nginx access log for the agent's own
#                     requests (nginx-logfault.conf's log_format is the bracket).
set -eu

COMPOSE_FILES="${COMPOSE_FILES:--f docker-compose.ha.yaml}"
dc() { docker compose $COMPOSE_FILES "$@"; }

mode="${1:?usage: w3-4-logfault.sh <clear|truncate|lostack|show|probe> [timeout]}"

case "$mode" in
  show)
    dc exec -T nginx sh -c 'cat /etc/nginx/logfault/fault.conf 2>/dev/null || echo "(no fault.conf)"'
    exit 0
    ;;
  probe)
    url="${2:-http://localhost:18080/api/v1/agents/probe/runs/00000000-0000-0000-0000-000000000000/steps/0/logs/bulk}"
    printf '%s probe ' "$(date -u +%FT%T.%3NZ)"
    curl -sS -o /dev/null -D - -X POST -H 'Content-Type: application/json' -d '[]' "$url" \
      | tr -d '\r' | grep -iE '^(HTTP/|X-Logfault-Arm)' | tr '\n' ' '
    echo
    exit 0
    ;;
  clear)
    body='proxy_read_timeout 300s;
proxy_next_upstream error timeout http_502 http_503 http_504;
proxy_next_upstream_tries 3;
proxy_next_upstream_timeout 8s;'
    ;;
  truncate)
    t="${2:-200ms}"
    body="set \$logfault_arm truncate;
proxy_read_timeout $t;
proxy_next_upstream off;"
    ;;
  lostack)
    body='set $logfault_arm lostack;
set $logfault_target blackhole;
mirror /_logfault_mirror;
mirror_request_body on;
proxy_read_timeout 300s;
proxy_next_upstream off;'
    ;;
  *) echo "unknown mode: $mode" >&2; exit 2 ;;
esac

printf '%s\n' "$body" | dc exec -T nginx sh -c \
  'mkdir -p /etc/nginx/logfault && cat > /etc/nginx/logfault/fault.conf && nginx -t && nginx -s reload'
echo "logfault: $mode written+reloaded at $(date -u +%FT%T.%3NZ)"
