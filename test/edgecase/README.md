# Edge-Case Test Campaign

Exploratory testing of unified-cd's distributed-systems edge cases.
Spec: `docs/superpowers/specs/2026-07-29-edge-case-testing-design.md`
(waves W0-W6, invariants I1-I7, findings workflow).

## Layout

- `FINDINGS.md` — one entry per invariant violation or notable observation.
- `scenarios/` — one runbook per scenario (`w<wave>-<n>-<slug>.md`).
- `compose/` — overlay files stacked onto `test/ha/docker-compose.ha.yaml`.
- `workloads/` — job/schedule YAML (and pre-encoded JSON API payloads).
- Scheduler/timing probes live next to the code they probe (e.g.
  `internal/controller/`), gated by the `edgeprobe` build tag — not a
  standalone `probes/` directory; see "Running probe tests" below.

## Tools

- `tools/inject.sh <cmd> <service>` — fault injection (kill/pause/partition/
  nginx-block/steplock, plus the `s3-*` family — see "The object store, and the
  S3 interposer" below). Run from `test/ha/`.

  `steplock <agent>` / `steplock-clear` are **URI-scoped**: they deny only
  `POST /api/v1/agents/<agent>/steps` (403) and leave every other endpoint for
  that agent working — notably
  `POST /api/v1/agents/<agent>/runs/<runId>/children`. They require the
  `compose/steplink.override.yaml` overlay (`nginx-steplink.conf`, which gives
  each agent's step-report path an exact-match `location` with its own include
  dir) and are a no-op against `nginx-edge.conf`, so always check the response
  code after arming. Introduced by W2-5 to reach a sub-10 ms code window
  deterministically instead of racing it; the locations are per-agent-id and
  must be extended by hand for a third agent.
- `tools/bulk-submit.sh <job-name> <count>` — submits `count` runs of an
  already-applied job and prints one run id per line (progress on stderr, so
  `| tee ids.txt` captures ids only). Honors `UNIFIED_SERVER`
  (default `http://localhost:18080`) and `UNIFIED_TOKEN` (default
  `ha-admin-token`). Used by W2-9 to push >50 runs into `Pending`.

  The trigger endpoint is `POST /api/v1/runs` with body `{"jobName":"..."}`
  (`internal/controller/server.go:370` → `handleTriggerRun`,
  `api_runs.go:22-42`), which returns the full `api.Run` JSON — **there is no
  `/api/v1/jobs/<job>/trigger` route**.

- `tools/w3/w3-4-logfault.sh <clear|truncate|lostack|show|probe>` — URI-scoped
  fault on the agent **log-bulk** endpoint only
  (`POST /api/v1/agents/<id>/runs/<runId>/steps/<n>/logs/bulk`). `truncate [t]`
  cuts `proxy_read_timeout` so nginx 504s mid-loop and the closed upstream
  connection cancels the controller's request context, leaving a **committed
  prefix**; `lostack` mirrors the request to a real controller (which commits
  the whole batch) while the client-facing leg goes to a dead upstream (502).
  Requires `compose/logfault.override.yaml` (`nginx-logfault.conf`) and is a
  **no-op** against `test/ha/nginx.conf` or `nginx-edge.conf`, so always
  `probe` after arming — it prints the `X-Logfault-Arm` header.

  **The probe proves the arm only for a NEW connection.** The authoritative
  bracket is `nginx-logfault.conf`'s custom `logfault` access-log format, which
  stamps `arm=` and the status onto **every** request, so armed/cleared claims
  are made per request rather than per wall-clock window (the W2-5 lesson).
  `worker_shutdown_timeout 1s` bounds how long an already-connected agent keeps
  the old config — **and, as a side effect, severs in-flight SSE streams and
  long-poll claims on every reload**; run SSE captures straight against a
  controller, not through the LB.

  **`worker_shutdown_timeout` is carried by `nginx-logfault.conf` ONLY**
  (`nginx-logfault.conf:35`; `grep -rn worker_shutdown_timeout test/edgecase
  test/ha --include=*.conf` matches only `nginx-logfault.conf` — the directive
  at `:35` and its own explanatory comment at `:23`, nothing else).
  `nginx-edge.conf` (used by `inject.sh
  nginx-block`) and `nginx-steplink.conf` (used by `inject.sh steplock`) do
  **not** have it, so those two injectors **still carry the original W2-5
  trap**: after `nginx -s reload` an already-connected agent may keep being
  served by an old worker with the old config, and a request can succeed
  inside a nominally-armed window. Do not generalise the logfault overlay's
  clean arm behaviour to them — either add the directive to the config you are
  using, or bracket every claim per-request from an access log.
- `tools/w3/w3-4-partB.sh <attempt-n> [arm-delay-s] [hold-s] [timeout]` — the
  W3-4 Part B driver: clear+probe, trigger `edge-logburst`, arm `truncate`
  across the burst, clear+probe, with a host timestamp on every step.
- `compose/mixedkek.override.yaml` — no injector script; the fault **is** the
  configuration. Gives **controller3 alone** a different local KEK
  (`compose/kek-b`, a committed throwaway test key like `ha-admin-token`), by
  adding a second mount at a **new** target and repointing
  `UNIFIED_CONTROLLER_KEY_FILE` — not by re-pointing the existing
  `/run/secrets/kek` mount, so the divergence shows as two distinct lines in
  `docker compose config` instead of relying on Compose's silent
  merge-by-target-path. `controller2`/`controller3` are `*ctrl` aliases of
  `controller1`'s `&ctrl` anchor (`docker-compose.ha.yaml:15`, `:30-31`), so
  this cannot be expressed inside `test/ha/` without editing it.

  **A wrong key is still a *valid* key file** (64 hex chars after `TrimSpace`,
  `internal/config/keysource.go:208-211`), so controller3 starts normally,
  passes `/readyz`, and logs an `encryption key loaded` line that differs from
  the healthy replicas' only by the file path. **Check key length inside the
  container, never on the host** — `test/ha/kek` is CRLF on a Windows checkout
  (66 bytes) and `kek-b` is LF (65); both `TrimSpace` to 64.
  **A decrypt 500 is not an nginx failure**: `http_500` is absent from
  `proxy_next_upstream` (`test/ha/nginx.conf:24`), so it is neither retried
  against a healthy replica nor counted toward `max_fails=1` — W3-3 drove nine
  consecutive 500s across all three upstreams with zero ejection, which is the
  opposite of W3-4's experience with 504s under the same config.

### The object store, and the S3 interposer (W3+)

`test/ha/docker-compose.ha.yaml` now runs **Garage** and points the
controllers (`UNIFIED_S3_*` → `unified-cd-logs`) and the agents
(`UNIFIED_CACHE_*` → `unified-cd-cache`) at it. That turns on the log
archiver, cache cleanup, artifact upload and the agent cache, all of which were
silently off before. **Consequences you must plan for:**

- **`docker compose down -v` between scenarios is now MANDATORY**, not hygiene.
  Cache entries, artifacts and run-log archives live in the dedicated
  `ha-garagedata` volume and survive a plain `down`; a leftover
  `caches/<jobhash>/<keyhash>` object turns a cache-miss expectation into a hit.
- **A controller that starts while Garage is unreachable CRASHLOOPS.**
  `NewS3ObjectStore` calls `BucketExists` eagerly
  (`internal/objectstore/s3.go:41-49`) and `cmd/controller/main.go:311-313`
  does `os.Exit(1)`. This constrains injection ordering for any scenario that
  both faults the store and restarts a replica.
- **All four `UNIFIED_CACHE_*` vars are required and none has a default.**
  `cmd/unified-cd-agent/main.go:204` gates the cache store on
  `endpoint != "" && key != "" && secret != "" && bucket != ""`. A missing one
  disables the cache **silently** — the only trace is the absence of the
  `cache enabled` INFO line at `:215`. Verified live: with the bucket unset and
  everything else identical, agent1 registered normally and logged no cache
  line at all.

- **Out-of-band object manipulation goes through the `mc` service.** Garage's
  own CLI cannot delete an S3 object — `garage bucket` (v2.3.0) offers
  `inspect-object` and no rm/delete verb. The `mc` container idles with the
  alias preconfigured:
  ```bash
  docker compose $COMPOSE_FILES exec -T mc mc ls --recursive garage/unified-cd-logs/
  docker compose $COMPOSE_FILES exec -T mc mc rm garage/unified-cd-logs/<key>
  ```
  It idles rather than one-shotting because `exec` costs milliseconds where a
  `compose run` costs container start, and injection timing matters.

- `compose/s3proxy.override.yaml` + `compose/nginx-s3.conf` — the **S3
  interposer**, an nginx on port 3900 between the unified-cd processes and
  Garage. Both sides are repointed at it, and **side selection is by bucket**,
  which is the first path segment of a path-style S3 request:
  `unified-cd-logs/runs/` and `unified-cd-logs/artifacts/` are the controller's,
  `unified-cd-cache/caches/` is the agent's. Verbs:

  | Command | Effect |
  |---|---|
  | `inject.sh s3-block <METHOD\|ANY> [keyPrefix] [status]` | fail matching requests; verified method- **and** prefix-selective |
  | `inject.sh s3-latency <seconds>` | fixed delay **per HTTP request** before Garage is reached |
  | `inject.sh s3-slow <bytes/s>` | throttle response bodies (holds a cache-restore stream open) |
  | `inject.sh s3-clear` / `s3-show` / `s3-probe [METHOD] [/bucket/key]` | clear, dump, confirm |

  **Choose the block status deliberately:** minio-go retries 429/500/502/503/504
  internally with backoff, so a `503` arm produces a slow retried failure
  (realistic, but it moves the timing) while `403` fails immediately.

  **`s3-latency` is per request, not per operation** — measured, `mc cat` of a
  6-byte object went 0.022 s → **9.034 s** under `s3-latency 3`, because mc
  issues three requests for that one read.

  **It DELAYS a large artifact PUT; it does not fail one — verified, because
  the mechanism gave real grounds to doubt it.** The arm works by letting a
  connect to a black hole time out and falling through to `backup` via
  `proxy_next_upstream timeout`, while `nginx-s3.conf:118` sets
  `proxy_request_buffering off` — and nginx cannot pass a request to the next
  upstream once it has *sent* any of an unbuffered body. It is sound here only
  because a **connect** timeout means nothing was sent yet. Measured on the
  `bigbody + s3proxy` stack with the 64 MiB `edge-artifact-large` fixture
  (`w3-infra/step5-bigbody-and-latency-recapture.txt`, phase 3):

  | arm | `upload_blob` | controller `PUT …/artifacts` |
  |---|---|---|
  | none | 0.753 s | `status:204 duration_ms:747` |
  | `s3-latency 3` | **9.702 s** | `status:204 duration_ms:9696` |

  Both runs `Succeeded` and both objects are in Garage at 64 MiB. The
  interposer's access log shows the arithmetic exactly: a 64 MiB Put is
  **3** S3 requests (`POST ?uploads=`, `PUT ?partNumber=1`, `POST ?uploadId=`),
  each logged `ustatus=504, 200` — black hole timed out, `backup` served it —
  so 3 × 3 s ≈ the 9 s of added width. The body-bearing request carries
  `reqlen=67203925` and still falls through, which is the point that was in
  doubt. **So `s3-latency` is a usable width knob for W3-6**, at ~3 s of
  widening per armed second. Two cautions: the width scales with the *request
  count*, so a payload large enough to be split into more parts widens more
  than linearly in size; and `s3-latency` does **not** reach the `mc`
  container, whose alias points at `garage:3900` directly and bypasses the
  interposer — use a job, not `mc`, to measure an arm.

  **On the two reload lessons, and why this overlay resolves them differently
  from `nginx-logfault.conf`:** it uses `keepalive_timeout 0`, **not**
  `worker_shutdown_timeout 1s`. On reload nginx's old workers close their
  listening sockets immediately, so only *established* connections carry stale
  config — and with `keepalive_timeout 0` this proxy never has an established
  idle connection, so the request after a reload is deterministically served by
  a new worker. `worker_shutdown_timeout` would get there by **killing**
  in-flight requests, which is exactly wrong on a path carrying multi-megabyte
  artifact PUTs and cache-restore GET streams. W3-4's separate warning (that
  `worker_shutdown_timeout` severs SSE and long-poll claims) does not apply
  here at all: this nginx carries S3 traffic only, while SSE
  (`/api/v1/runs/{id}/events`, `internal/controller/server.go:337`) and the
  agent long-poll claim go to the controller LB on `nginx:8080` and never
  traverse port 3900.

  **Probe anyway.** Every arming verb ends by printing a probe, and `s3-block`
  additionally prints a *control* probe for a pair it must NOT match. The
  authoritative bracket is `nginx-s3.conf`'s `s3fault` access-log format, which
  stamps `arm=` onto every request. `s3_reload` aborts with exit 3 if
  `nginx -t` rejects an arm — building this caught exactly that failure, where
  a duplicate directive made nginx keep the OLD config while the script
  reported "armed".

- `compose/bigbody.override.yaml` + `compose/nginx-bigbody.conf` — **required by
  every scenario that uploads a non-trivial artifact.** `test/ha/nginx.conf`
  inherits nginx's default `client_max_body_size 1m`, so a 64 MiB upload dies
  at the LB with **413** and the run Fails before a controller sees a byte
  (measured; filed in `FINDINGS.md`). The overlay gives artifact URIs their own
  location with `client_max_body_size 0` and `proxy_request_buffering off` —
  the second is load-bearing for W3-6, since buffering would spool the whole
  body before opening the upstream request and move the controller's `Put`
  outside the window the agent is uploading in. **It does not stack with
  `logfault.override.yaml` or `steplink.override.yaml`** — all three replace
  the same `/etc/nginx/nginx.conf` mount and the last file listed silently
  wins. It *does* stack with `s3proxy.override.yaml` (different service).

- `tools/w3/fixcheck` — parses YAML fixtures through the real `dsl.Parse`
  (`KnownFields(true)` + `Job.Validate`) and prints what the controller would
  see, offline. Run it on both the `.yaml` **and** the YAML re-extracted from
  the `.payload.json` before spending an API call: W1 shipped two payloads that
  400'd on a wrong key path.

**Sidecars are NOT available on this rig, by two independent mechanisms.** The
campaign envelope is `native: true`, which leaves `pod == nil`
(`internal/agent/agent.go:593-623`), and `hostBackend.SetMasker` creates the
sidecar log pump only when `b.pod != nil`
(`internal/agent/backend_host.go:362-368`). Dropping `native: true` does not
help: the claim then needs a container runtime and the agent containers have no
Docker socket. Note the agents nonetheless *advertise* `["native","container"]`
— `Available()` is a binary-on-PATH check (`internal/runtime/ocicli.go:42-45`)
and `docker/agent.Dockerfile:17` installs `docker-cli`. Any scenario needing a
post-`FinishRun` log flush must seal by hand
(`INSERT INTO run_log_archives`) against a post-hook or `finally` flush, and
must say that those flush **before** `FinishRun`, so the structural window is
absent and the result is a hand-timed demonstration, not a natural race.
**The ordering, with the cites, because a scenario author will need to defend
it:** in `runClaim` (`internal/agent/orchestrator.go`) a `post:` hook's log
writers are closed by `finishPostLogs(hookCtx)` at **`:706`**, the whole
`finally` pipeline runs at **`:727`** — both inside the main body — and
`FinishRun` is only called afterwards, at **`:787-788`**, wrapped in
`retryUntilSuccess`. There is no agent-side hook that runs *after* that call.

## Workload fixtures

Every `*.payload.json` is the pre-encoded `{"yaml":"..."}` body for
`POST /api/v1/jobs` — **except `schedule-every-minute.payload.json`, which is a
`kind: Schedule` and must go to `POST /api/v1/schedules`.** Posting it to
`/api/v1/jobs` returns **400** `invalid yaml: ... field cron not found in type
dsl.Spec` (verified live during W2-6): the jobs handler unmarshals the body into
`dsl.Spec`, which has no `cron`/`job` fields. `POST /api/v1/schedules` accepts it
and returns `200` with the schedule JSON. All the *job* fixtures are
`agentSelector: [kind:linux]`, and all are `native: true` **except
`podcap-job.payload.json`**, which carries a Kubernetes-only `podTemplate` so
its inferred capability is `pod` (see the table).

| File | Job | Purpose |
|---|---|---|
| `tick.payload.json` | `edge-tick` | trivial run |
| `longrun.payload.json` | `edge-longrun` | long-lived run for reaper timing |
| `approval.payload.json` | `edge-approval` | approval gate, 10-minute timeout |
| `sideeffect.payload.json` | `edge-sideeffect` | mutex `edge-mutex` holder, writes `/data/sideeffect.log`. **Emits ZERO log lines** — its `echo` is redirected to the file, so its `logs` row count is 0. It is a side-effect (I2) fixture, never a log fixture (W3-4) |
| `mutex-successor.payload.json` | `edge-mutex-successor` | mutex `edge-mutex` successor probe (I3) |
| `schedule-every-minute.payload.json` | — | schedule fixture (`edge-every-minute`, `cron: "* * * * *"`, job `edge-tick`) — **`POST /api/v1/schedules`**, not `/api/v1/jobs` |
| `call-parent.payload.json` | `edge-call-parent` | 20s `prelude` then a `call:` step invoking `edge-call-child` (W2-2, W2-5) |
| `call-child.payload.json` | `edge-call-child` | ~90s child, timestamped markers to `/data/child.log` so an orphaned child stays observable after its parent dies |
| `approval-short.payload.json` | `edge-approval-short` | `before` → `gate` (`timeoutMinutes: 0.5` = **30s**) → `after` (W2-8) |
| `mutex-hog.payload.json` | `edge-mutex-hog` | mutex `edge-mutex` lock holder, sleeps 600s (W2-9) |
| `unrelated-probe.payload.json` | `edge-unrelated-probe` | **no mutex**, `echo probe-ran` — the W2-9 starvation probe |
| `podcap-job.payload.json` | `edge-podcap-job` | `podTemplate` with a pod-level `nodeSelector`, so `dsl.RequiredCaps` infers **`pod`** — label-claimable (`kind:linux`) but capability-unschedulable, because the `test/ha` agents report `["native","container"]` (W2-4 Part D) |
| `logburst.payload.json` | `edge-logburst` | the **chatty** fixture (W3-4): emits exactly **2002** stdout lines — `burst-begin`, `burst-1`…`burst-2000` written as fast as the shell can, then `burst-end` after a 30 s quiet window. Line contents are self-indexing so duplicates and reordering are measurable without joining anything. `sleep 8` before the burst gives a window to arm a fault against an already-connected agent |
| `artifact-large.payload.json` | `edge-artifact-large` | the **artifact** fixture (W3-2, W3-6): builds a `/dev/urandom` blob (`size_mb` param, default **64**) and uploads it. The upload duration IS W3-6's TOCTOU width (`api_artifacts.go:55` GetRun → `:79` Put, nothing between). Measured: 64 MiB → `upload_blob` **0.863 s**, 256 MiB → **3.060 s** (≈12 ms/MiB). **Needs `compose/bigbody.override.yaml`** — without it the LB 413s anything over 1 MiB. Random, not zeros, because the payload is compressed on the way out |
| `cache-user.payload.json` | `edge-cache-user` | the **cache** fixture (W3-1): `wipe` → `cache:` (`ttlDays: 1`, the real floor — `0` is silently rewritten to 30 at `orchestrator.go:980-982`) → `use_deps`, printing `CACHE-HIT`/`CACHE-MISS` plus the marker's plant timestamp. **The `wipe` step must stay first**: the host agent keeps a persistent per-job workspace, so without it a second run would find `deps/` still present and a "hit" would prove nothing. Verified end to end — run 1 on agent2 planted `01:54:14.857`, run 2 on **agent1** restored that same timestamp, so the hit crossed agents and can only have come from the object store |
| `secret-user.payload.json` | `edge-secret-user` | the **secrets** fixture (W3-3): step `env` references `{{ .Secrets.EDGE_KEK_PROBE }}`, which is what makes the claim response carry a non-empty `SecretsNeeded` and the agent take the `FetchSecrets` path (`internal/agent/orchestrator.go:161`). Prints only the secret's **length** (`secret-len=<n>`), never its value. `secret-user.yaml` is the same job in plain YAML. **The secret must be registered first** — `POST /api/v1/secrets/` (trailing slash required) with `{"name":..., "value":...}`, `204` on success |

### Fractional `timeoutMinutes` — verified, do not re-derive

`approval.timeoutMinutes` is `float64` (`internal/dsl/types.go:341`) and
**fractional values round-trip end to end**; `approval-short.payload.json`
therefore uses `0.5`, giving a **30-second** approval window (not 60s):

- Parse + `Validate()` accept it — nothing in `internal/dsl` constrains the
  value (no minimum, no integer check); `dsl.Parse` decodes `0.5` to `0.5`.
- `buildClaimResponse` (`internal/controller/api_agent.go:436-440`) only
  substitutes the default when `timeout == 0`, so `0.5` passes through.
- The agent computes `time.Duration(timeoutMin*60) * time.Second`
  (`internal/agent/approval.go:48`) → exactly 30s.
- The controller computes `time.Now().Add(time.Duration(req.TimeoutMinutes *
  float64(time.Minute)))` (`internal/controller/api_approvals.go:87`) → also
  exactly 30s, so `timeout_at` and the agent deadline agree.

Caveat: the agent's `time.Duration(timeoutMin*60)` truncates to whole
seconds, so values finer than 1/60 minute lose resolution. `0.5` is exact.
The controller's approval reaper still ticks at **1 minute**
(`cmd/controller/main.go:403`), so a row can sit `Pending` up to ~60s past a
30s `timeout_at` — the agent-local deadline is the one that fires first.

## Raw evidence

`FINDINGS.md` cites captures by relative name (`w1-5/agent1.log`,
`w1-6/metrics.txt`, ...). Those names resolve against the campaign's evidence
root, which is **not in this repository**:

    <project parent>/edgecase-evidence/

i.e. a sibling of the checkout, so it survives worktree removal and
`git clean -fdx`. It holds ~9 MB of container logs, psql output, API reads and
metrics scrapes — too bulky and too raw to commit, but it is what every
numeric claim in `FINDINGS.md` is derived from. See its own `README.md` for
per-wave coverage; coverage is uneven, and the entries that rest on
un-captured observations say so inline.

While running a scenario, capture to the session scratchpad (fast, disposable)
and copy the wave's directory into the evidence root at the wave checkpoint.

## Running a compose scenario

Each runbook lists its exact stack invocation. The general shape:

    docker compose -f test/ha/docker-compose.ha.yaml \
      -f test/edgecase/compose/<overlay>.yaml up -d --build

The LB is `http://localhost:18080`, admin token `ha-admin-token`
(both inherited from the test/ha stack).

## Running probe tests

Scheduler/timing boundary probes are observational Go tests excluded from
normal builds by the `edgeprobe` build tag. They live next to the code they
probe (they call unexported functions), e.g.
`internal/controller/edgeprobe_scheduler_test.go`:

    go test -tags edgeprobe ./internal/controller -run TestEdgeProbe -v

Probes PASS unless infrastructure breaks; their `t.Logf` output is the
result. Copy notable output into `FINDINGS.md`.

## Rules

- Phase 1 is exploration: record findings, do NOT fix production code.
- Findings are reported in one batch after the final wave.
- Every scenario names the invariants (I1-I7) it attacks.
