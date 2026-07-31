#!/usr/bin/env sh
# Fault-injection helpers for the edge-case campaign (W1+).
# Usage: inject.sh <command> <service> [args]
# Run from test/ha/ (paths are relative to it). COMPOSE_FILES may add overlays:
#   COMPOSE_FILES="-f docker-compose.ha.yaml -f ../edgecase/compose/oneway.override.yaml"
set -eu

COMPOSE_FILES="${COMPOSE_FILES:--f docker-compose.ha.yaml}"
dc() { docker compose $COMPOSE_FILES "$@"; }

cmd="${1:?usage: inject.sh <kill-soft|kill-hard|pause|unpause|partition|heal|nginx-block|nginx-unblock|steplock|steplock-clear|s3-block|s3-latency|s3-slow|s3-clear|s3-show|s3-probe> [service|args]}"
# nginx-unblock and steplock-clear clear the whole blocklist and take no
# service argument; the s3-* family takes fault parameters rather than a
# service name; every other command needs a service.
case "$cmd" in
  nginx-unblock|steplock-clear)                       svc="${2:-}" ;;
  s3-block|s3-latency|s3-slow|s3-clear|s3-show|s3-probe) svc="" ;;
  *)                                                  svc="${2:?service name required}" ;;
esac

# --- S3 interposer helpers (see compose/nginx-s3.conf) --------------------
# All of these need the compose/s3proxy.override.yaml overlay in
# COMPOSE_FILES; without it there is no `s3proxy` service and they fail loudly
# (which is the intent — a silent no-op is what W2-5 warned about).
S3FAULT_DIR=/etc/nginx/s3fault

# s3_reload — validate then reload, and ABORT LOUDLY if the arm file does not
# parse. Learned the hard way: an arm file whose directive duplicates one
# already present in `location /` makes `nginx -t` fail with
# `[emerg] ... directive is duplicate`, nginx keeps serving the OLD config, and
# without this check the caller cheerfully prints "armed". That is precisely
# the silent-no-op class the W2-5 lesson is about. `set -e` alone is not
# enough — `docker compose exec`'s exit status is not reliably the inner
# command's here — so the status is captured and checked explicitly.
s3_reload() {
  if ! dc exec -T s3proxy sh -c "nginx -t 2>&1 && nginx -s reload 2>&1" > /tmp/.s3reload.$$ 2>&1; then
    echo "inject.sh: FATAL — nginx refused the arm; the OLD config is still live:" >&2
    cat /tmp/.s3reload.$$ >&2
    rm -f /tmp/.s3reload.$$
    exit 3
  fi
  if grep -q '\[emerg\]' /tmp/.s3reload.$$; then
    echo "inject.sh: FATAL — nginx -t emitted [emerg]; the OLD config is still live:" >&2
    cat /tmp/.s3reload.$$ >&2
    rm -f /tmp/.s3reload.$$
    exit 3
  fi
  rm -f /tmp/.s3reload.$$
}

# s3_probe <METHOD> <PATH> — re-evaluates the LIVE arm files against one
# (method, key) pair through the same include the real traffic uses, and
# prints the status line plus X-S3-Arm. 200 = that pair passes, 5xx = blocked.
# This is the per-arm confirmation the W2-5 lesson demands: never assume a
# reload took.
s3_probe() {
  _m="${1:-GET}"; _p="${2:-/}"
  dc exec -T s3proxy sh -c \
    "wget -S -q -O- 'http://127.0.0.1:3900/_s3probe?m=$_m&p=$_p' 2>&1 | \
     grep -E 'HTTP/|X-S3-Arm|s3probe' || true"
}

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
  steplock)
    # Surgical injection (W2-5): refuse ONLY this agent's step-report endpoint
    # (POST /api/v1/agents/<id>/steps) with 403, leaving every other agent API
    # — child-run creation, claim, heartbeat, logs, finish — working. Requires
    # the steplink.override.yaml overlay (nginx-steplink.conf); it is a no-op
    # against nginx-edge.conf, so check the response code after arming.
    dc exec -T nginx sh -c \
      "mkdir -p /etc/nginx/steplock/$svc && echo 'deny all;' > /etc/nginx/steplock/$svc/deny.conf && nginx -s reload"
    echo "steplock armed for $svc (POST /api/v1/agents/$svc/steps -> 403)"
    ;;
  steplock-clear)
    dc exec -T nginx sh -c "rm -f /etc/nginx/steplock/*/deny.conf && nginx -s reload"
    echo "steplock cleared for all agents"
    ;;

  s3-block)
    # s3-block <METHOD|ANY> [keyPrefix] [status]
    # Fails the matching S3 requests at the interposer while everything else
    # keeps working. keyPrefix has NO leading slash and INCLUDES the bucket,
    # because the bucket is the first path segment of a path-style S3 request
    # and is therefore also the side selector:
    #   unified-cd-logs/artifacts/   controller, artifact upload/download
    #   unified-cd-logs/runs/        controller, log archival
    #   unified-cd-cache/caches/     AGENT, cache save/restore
    # Examples:
    #   inject.sh s3-block PUT unified-cd-logs/artifacts/
    #   inject.sh s3-block ANY unified-cd-cache/
    #   inject.sh s3-block DELETE '' 403
    #
    # STATUS MATTERS: minio-go retries 429/500/502/503/504 internally (up to
    # 10 attempts with backoff), so a 503 arm produces a slow, retried failure
    # — realistic, but it moves the timing. Pass 403 for an immediate,
    # non-retried failure when the scenario is measuring a window rather than
    # a retry policy.
    #
    # ONE BLOCK ARM AT A TIME. Every s3-block writes the same single file
    # ($S3FAULT_DIR/10-block.conf, truncating `>`), so a second s3-block
    # silently REPLACES the first rather than adding to it — there is no
    # "block PUT on the cache AND DELETE on artifacts" state reachable by
    # calling this verb twice. (s3-block composes with s3-latency/s3-slow,
    # which write 20-latency.conf / 30-slow.conf; it does not compose with
    # itself.) A scenario needing two simultaneous block arms must either
    # widen one regex to cover both pairs, or write a second numbered file
    # into $S3FAULT_DIR by hand and reload — s3-clear removes all of them.
    meth="${2:?usage: inject.sh s3-block <METHOD|ANY> [keyPrefix] [status]}"
    pfx="${3:-}"
    status="${4:-503}"
    case "$meth" in
      ANY|any|'*') methre='[A-Z]+' ;;
      *)           methre="$meth" ;;
    esac
    dc exec -T s3proxy sh -c "mkdir -p $S3FAULT_DIR && cat > $S3FAULT_DIR/10-block.conf <<'EOF'
set \$s3_arm \"block[$meth /$pfx -> $status]\";
if (\$s3_sel ~ \"^$methre /$pfx\") { return $status; }
EOF"
    s3_reload
    echo "s3-block armed: $meth /$pfx -> $status"
    echo "-- probe (should be blocked):"; s3_probe "$meth" "/$pfx"
    echo "-- probe (control, must NOT be blocked):"; s3_probe GET /_control_never_matched_
    ;;

  s3-latency)
    # s3-latency <seconds> — adds a fixed pre-request delay to EVERY S3 call
    # through the interposer by routing it first at a black-holed upstream and
    # letting proxy_connect_timeout expire before falling back to Garage.
    # Widens Put/Get windows (W3-6's TOCTOU is bounded by Put duration).
    # Composes with s3-block: they are separate include files.
    #
    # VERIFIED ON A LARGE PUT, not just on a small GET: 64 MiB
    # edge-artifact-large went upload_blob 0.753 s unarmed -> 9.702 s under
    # `s3-latency 3`, Succeeded both times, object present in Garage both
    # times. A 64 MiB Put is 3 S3 requests, hence ~3x the armed seconds; width
    # scales with REQUEST COUNT, so a bigger payload split into more parts
    # widens more than linearly. Does NOT reach the `mc` container, whose
    # alias points straight at garage:3900 — measure an arm with a job.
    secs="${2:?usage: inject.sh s3-latency <seconds>}"
    dc exec -T s3proxy sh -c "mkdir -p $S3FAULT_DIR && cat > $S3FAULT_DIR/20-latency.conf <<'EOF'
set \$s3_arm \"\${s3_arm}+latency[${secs}s]\";
set \$s3_target garage_delayed;
proxy_connect_timeout ${secs}s;
proxy_next_upstream timeout;
proxy_next_upstream_tries 2;
proxy_next_upstream_timeout 0;
EOF"
    s3_reload
    echo "s3-latency armed: +${secs}s per request"
    echo "-- probe:"; s3_probe GET /
    ;;

  s3-slow)
    # s3-slow <bytes-per-second> — throttles RESPONSE bodies (limit_rate), so
    # a cache-restore GET stream stays open long enough to race an out-of-band
    # delete against it (W3-1). Request bodies are unaffected; use a larger
    # payload for a wider Put window.
    rate="${2:?usage: inject.sh s3-slow <bytes-per-second, e.g. 65536>}"
    dc exec -T s3proxy sh -c "mkdir -p $S3FAULT_DIR && cat > $S3FAULT_DIR/30-slow.conf <<'EOF'
set \$s3_arm \"\${s3_arm}+slow[${rate}B/s]\";
limit_rate ${rate};
EOF"
    s3_reload
    echo "s3-slow armed: responses capped at ${rate} B/s"
    echo "-- probe:"; s3_probe GET /
    ;;

  s3-clear)
    dc exec -T s3proxy sh -c "rm -f $S3FAULT_DIR/*.conf"
    s3_reload
    echo "s3 faults cleared"
    echo "-- probe:"; s3_probe GET /
    ;;

  s3-show)
    dc exec -T s3proxy sh -c "ls -l $S3FAULT_DIR/ 2>/dev/null; echo '--- contents ---'; cat $S3FAULT_DIR/*.conf 2>/dev/null || echo '(unarmed)'"
    ;;

  s3-probe)
    # inject.sh s3-probe [METHOD] [/bucket/key]
    s3_probe "${2:-GET}" "${3:-/}"
    ;;

  *) echo "unknown command: $cmd" >&2; exit 2 ;;
esac
