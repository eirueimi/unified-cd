# shellcheck shell=sh
# Shared admin credential for the edgecase tools. Source it, then call
#
#   TOKEN=$(bootstrap_admin_token [server])
#
# The HA rig has no standing admin token. Its controllers are started with the
# fixed one-time initial admin token below (UNIFIED_INITIAL_ADMIN_TOKEN in
# test/ha/docker-compose.ha.yaml), which can be exchanged exactly once for an
# admin PAT (POST /api/v1/bootstrap). The agent-enroll one-shot does that
# exchange at startup and keeps the PAT at /state/admin-token in the admin-state
# volume, so the function looks in this order and caches the result in
# ${TMPDIR:-/tmp}/unified-ha-admin-token:
#
#   1. the cache file, if the PAT in it is still accepted by the server;
#   2. the admin-state volume of the test/ha stack;
#   3. a fresh exchange of the initial token (for rigs without agent-enroll).
#
# A cached PAT from an earlier stack is rejected by step 1 and replaced. The PAT
# lasts 30 days. Nothing here prints a credential except the final PAT on
# stdout.

: "${HA_INITIAL_ADMIN_TOKEN:=uci_ha00000000000000000000000000000000000000}"

_admin_token_json_field() {
  # Print .token from a JSON object on stdin (jq if present, else sed).
  if command -v jq >/dev/null 2>&1; then
    jq -r '.token // empty'
  else
    sed -n 's/.*"token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'
  fi
}

bootstrap_admin_token() {
  _bat_server="${1:-${UNIFIED_SERVER:-http://localhost:18080}}"
  _bat_cache="${TMPDIR:-/tmp}/unified-ha-admin-token"
  _bat_repo="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"

  if [ -s "$_bat_cache" ]; then
    _bat_tok="$(cat "$_bat_cache")"
    _bat_code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
      -H "Authorization: Bearer $_bat_tok" "$_bat_server/api/v1/agents" || true)"
    if [ "$_bat_code" = "200" ]; then
      printf '%s\n' "$_bat_tok"
      return 0
    fi
    rm -f "$_bat_cache"
  fi

  _bat_tok=""
  if command -v docker >/dev/null 2>&1 && [ -f "$_bat_repo/test/ha/docker-compose.ha.yaml" ]; then
    _bat_tok="$(docker compose -f "$_bat_repo/test/ha/docker-compose.ha.yaml" run --rm --no-deps -T \
      --entrypoint cat agent-enroll /state/admin-token 2>/dev/null | tr -d '\r\n' || true)"
  fi
  if [ -z "$_bat_tok" ]; then
    _bat_tok="$(curl -fsS --max-time 10 -X POST -H 'Content-Type: application/json' \
      -d "{\"initialToken\":\"$HA_INITIAL_ADMIN_TOKEN\"}" \
      "$_bat_server/api/v1/bootstrap" | _admin_token_json_field)" || _bat_tok=""
  fi
  if [ -z "$_bat_tok" ]; then
    echo "bootstrap_admin_token: could not obtain an admin PAT from $_bat_server" >&2
    return 1
  fi
  ( umask 077; printf '%s\n' "$_bat_tok" > "$_bat_cache" )
  printf '%s\n' "$_bat_tok"
}
