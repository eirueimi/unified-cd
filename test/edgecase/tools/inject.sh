#!/usr/bin/env sh
# Fault-injection helpers for the edge-case campaign (W1+).
# Usage: inject.sh <command> <service> [args]
# Run from test/ha/ (paths are relative to it). COMPOSE_FILES may add overlays:
#   COMPOSE_FILES="-f docker-compose.ha.yaml -f ../edgecase/compose/oneway.override.yaml"
set -eu

COMPOSE_FILES="${COMPOSE_FILES:--f docker-compose.ha.yaml}"
dc() { docker compose $COMPOSE_FILES "$@"; }

cmd="${1:?usage: inject.sh <kill-soft|kill-hard|pause|unpause|partition|heal|nginx-block|nginx-unblock> [service]}"
# nginx-unblock clears the whole blocklist and takes no service argument;
# every other command needs one.
case "$cmd" in
  nginx-unblock) svc="${2:-}" ;;
  *)             svc="${2:?service name required}" ;;
esac

case "$cmd" in
  kill-soft)  dc kill -s SIGTERM "$svc" ;;
  kill-hard)  dc kill -s SIGKILL "$svc" ;;
  pause)      dc pause "$svc" ;;
  unpause)    dc unpause "$svc" ;;
  partition)  docker network disconnect unified-cd-ha_default "unified-cd-ha-$svc-1" ;;
  heal)       docker network connect    unified-cd-ha_default "unified-cd-ha-$svc-1" ;;
  nginx-block)
    # One-way agent->controller partition: deny this agent's IP at nginx.
    # Strictly agent-polls-controller, so this is a full one-way partition.
    ip=$(docker inspect "unified-cd-ha-$svc-1" \
      --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')
    dc exec -T nginx sh -c "echo 'deny $ip;' > /etc/nginx/blocklist/deny.conf && nginx -s reload"
    echo "blocked $svc ($ip) at nginx"
    ;;
  nginx-unblock)
    dc exec -T nginx sh -c ": > /etc/nginx/blocklist/deny.conf && nginx -s reload"
    echo "unblocked all at nginx"
    ;;
  *) echo "unknown command: $cmd" >&2; exit 2 ;;
esac
