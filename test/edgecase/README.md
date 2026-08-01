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
  Referenced from `scenarios/w3-4-bulk-append-duplication.md` §Part B (it was
  committed with no runbook pointing at it until the branch review).

**WHICH DRIVERS ARE COMMITTED AND WHICH ARE EVIDENCE — the rule, stated because
W3 shipped one of each and neither said which it was.** A driver belongs in
`tools/w3/` and is **committed** when it is parameterised and re-runnable by
anyone: `w3-4-logfault.sh` and `w3-4-partB.sh` qualify. A driver that hardcodes
a session scratchpad path, a captured token file, or a specific run id is a
**session artefact**; it is archived in the evidence root, cited by the entry
that used it, and is **not** re-runnable as-is. W3's session artefacts are
`w3-2/partB-pause.sh` and `partB-arm.sh`, `w3-5/synth.sh` and `partB.sh`, and
`w3-6/synth.sh`, `partA-race.sh` and `partC-asymmetry.sh` — each cited from its
entry's Repro line, and none of them committed, which is correct rather than an
omission. **One promotion is worth a W4 task:** the two `synth.sh` files are the
same instrument (a curl-driven synthetic agent that enrolls, claims an
otherwise-unclaimable job, finishes the run and then acts on it), the checkpoint
already tells W4+ to reuse that instrument, and promoting it means removing two
hardcoded absolute paths and taking the agent id and job name as arguments.
Until that is done, **a re-runner of W3-5 or W3-6 must rebuild the helper from
the runbook's own steps** — both runbooks specify every call it makes.
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
  | `inject.sh s3-slow <bytes/s>` | throttle response bodies (holds a cache-restore stream open). **Emits `proxy_buffering on` + `proxy_limit_rate`, not `limit_rate`** — W3-1 measured `limit_rate` doing *nothing* under the server-level `proxy_buffering off` (a 10 s bounded read through the armed proxy returned the **whole** 16909275 B object, byte-identical to the direct-to-Garage control — `w3-1/partA-arm.txt:33-36`; ~~`rt=0.060`~~ was **not** an access-log field and is withdrawn, see `FINDINGS.md:2035`), and `proxy_limit_rate` additionally keeps the **upstream** GET in flight rather than draining it into nginx's buffers. Verified **through the verb** at 270312 B/s (2703126 B in a 10 s bounded read) against an armed 262144 B/s, versus the full 16909275 B direct to Garage in the same 10 s |
  | `inject.sh s3-clear` / `s3-show` / `s3-probe [METHOD] [/bucket/key]` | clear, dump, confirm. **`s3-probe` now EXITS 4 if it gets no HTTP status line back** — it used to swallow that with `|| true`, so the arm-confirmation helper could never fail |

  **Choose the block status deliberately:** minio-go retries 429/500/502/503/504
  internally with backoff, so a `503` arm produces a slow retried failure
  (realistic, but it moves the timing) while `403` fails immediately.

  **`s3-latency` is per request, not per operation** — measured, `mc cat` of a
  6-byte object went 0.022 s → **9.034 s** under `s3-latency 3`, because mc
  issues three requests for that one read.

  **`s3-latency` is NOT dependable at large values — W3-2 measured it failing.**
  The arm works by letting a connect to `192.0.2.1` (TEST-NET-1) time out and
  falling through to `backup` via `proxy_next_upstream timeout`. That requires
  the black hole to **hang**; if the host's network answers with an ICMP
  unreachable instead, nginx sees `connect() failed (111: Connection refused)`,
  which is `error` and **not** `timeout`, so `proxy_next_upstream` never fires
  and the request **502s** rather than being delayed. Measured under
  `s3-latency 30`: a log-archive `Put` got `502 … rt=21.037`, breaking the
  operation instead of widening it (`w3-2/partB-arm1.txt`). The 3-second
  measurements above are not withdrawn — at 3 s the connect deadline expires
  first — but **a scenario that needs a wide window should use
  `inject.sh pause garage` behind the interposer instead**: nginx accepts,
  proxies to the paused container and waits on `proxy_read_timeout 300s`, which
  is a genuine hang, and the window is then bounded only by minio-go's 60 s
  `ResponseHeaderTimeout` (`minio-go/v7@v7.2.0/transport.go:52`). W3-2's Part B
  hit both of its arms on the first attempt with that lever.

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
  doubt. ~~"So `s3-latency` is a usable width knob for W3-6 — at small values
  only, see the large-value caveat above"~~ — **CORRECTED, and this measurement is
  not withdrawn: it answers a narrower question than the sentence claimed.** What
  is measured is that `s3-latency 3` *delays* a 64 MiB artifact `PUT` to 9.702 s
  rather than breaking it, at ~3 s of
  widening per armed second. **W3-6 as executed REJECTED it** and used neither it
  nor the interposer at all — see the width-knob rule below. Three cautions: the
  width scales with the *request
  count*, so a payload large enough to be split into more parts widens more
  than linearly in size; `s3-latency` does **not** reach the `mc`
  container, whose alias points at `garage:3900` directly and bypasses the
  interposer — use a job, not `mc`, to measure an arm; and at large values the arm
  **breaks** the operation instead of widening it (the caveat above; W3-2 measured
  a log-archive `Put` 502-ing at `s3-latency 30`).

  **THE WIDTH-KNOB RULE FOR W4-W6 — read this before reaching for `s3-latency`,
  because it is the arm the README used to recommend and W3-6 rejected it.**
  Pick the knob by which side of the race you need to widen, and check that the
  knob does not also hang your *instrument*:

  | you need to widen | use | why not `s3-latency` |
  |---|---|---|
  | a controller-side S3 call you are **not** also measuring through S3 | `inject.sh pause garage` behind the interposer | a genuine hang bounded by minio-go's 60 s `ResponseHeaderTimeout`; `s3-latency` at large values 502s instead (W3-2) |
  | the **client→controller** leg of an upload (W3-6's artifact TOCTOU) | `curl --limit-rate <R>` against `proxy_request_buffering off` from `bigbody` | `pause` would hang the `DELETE` being timed, because `deleteRunEverywhere` itself calls `obj.Delete`/`obj.List`; `s3-latency` widens the wrong side and is undependable at the values needed |
  | a **response-body** stream (a cache restore) | `inject.sh s3-slow <bytes/s>` | `s3-latency` is per request, not per byte |
  | the delete loop inside `deleteRunEverywhere` | pad the artifact prefix — 400 objects → 0.493 s, 10,000 → 12.739 s, ~1.2 ms/object, serial | not an S3-timing problem at all |

  **W3-6's own note is the one to copy: `--limit-rate` + `proxy_request_buffering
  off` "is a better width knob than anything in `inject.sh` for this race", and it
  is fully deterministic** — a 32 s client-side upload produced a controller
  `duration_ms` of 32374 (`scenarios/w3-6-retention-vs-upload.md` §Instrument and
  execution note 3). **And pair whatever knob you pick with a positive in-flight
  signal rather than a sleep** — W3-6 fired on `mc ls --incomplete`, W3-2 on three
  consecutive granted `pg_locks` samples; both hit on the first attempt.
  Consistent with the arm rule below, none of this is verified by a comment: each
  row above is backed by a capture that measured the knob's effect.

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

  **But a LOADED arm can still be a no-op, and only an EFFECT measurement
  catches that** — W3-1 found `s3-slow` emitting `limit_rate`, which does
  nothing under `proxy_buffering off`, while `s3-show`, `s3-probe` and the
  `arm=` stamp all passed. **The rule for W4+: a verb is verified when SOME
  capture measures its effect, not when its comment carries a measurement.**
  On that test nothing in the table above is unverified today — `s3-block`
  effect-probes itself per arm (method, prefix, plus a control that must not
  match; confirmed live in W3-2), `s3-latency` carries its measurement in its
  comment, `s3-slow` now carries a post-fix through-the-verb measurement, and
  `steplock` — whose own comment warns it is a silent no-op against
  `nginx-edge.conf` — was effect-confirmed by execution in W2-5. **Every new or
  changed arm must ship with an effect measurement; "the config is present and
  `nginx -t` passed" is never one.**

- `compose/bigbody.override.yaml` + `compose/nginx-bigbody.conf` — **required by
  every scenario that uploads a non-trivial artifact.** `test/ha/nginx.conf`
  inherits nginx's default `client_max_body_size 1m`, so a 64 MiB upload dies
  at the LB with **413** and the run Fails before a controller sees a byte
  (measured; filed in `FINDINGS.md`). The overlay gives artifact URIs their own
  location with `client_max_body_size 0` and `proxy_request_buffering off` —
  the second is load-bearing for W3-6, since buffering would spool the whole
  body before opening the upstream request and move the controller's `Put`
  outside the window the agent is uploading in. **It does not stack with
  `logfault.override.yaml`, `steplink.override.yaml` or `oneway.override.yaml`**
  — all **four** replace the same `/etc/nginx/nginx.conf` mount on the `nginx`
  service and the last file listed silently wins. (`oneway` was missing from an
  earlier version of this list and from `FINDINGS.md:2117`; it is still in the
  tree and is the base every `nginx-block` scenario needs.) **The one deliberate
  pairing is `-f oneway -f steplink`, in that order** — `steplink` is designed to
  override `oneway`'s `nginx.conf` while inheriting its `blocklist` volume. It
  *does* stack with `s3proxy.override.yaml` (different service).

### The W4 Kubernetes rig, and the enrollment bypass it rests on

**Read `scenarios/w4-rig.md` before running any W4 scenario.** Summary, because
this one caveat qualifies every W4 finding:

> Kubernetes agent enrollment is **unconditionally broken** — the verifier reads
> the ServiceAccount UID from a TokenReview extra key the API server never
> populates, so every k8s enrollment is 403 (W4-0), and PR #75 removed the
> static-token alternative. W4 therefore runs behind an **enrollment
> interposer**: `tools/w4/enrollproxy` forwards everything except
> `POST /api/v1/agents/enroll`, which it answers with a **real
> controller-issued** `uca_` obtained through the product's ordinary
> `"enrollment"` method. **No W4 finding speaks to the enrollment path** beyond
> what `scenarios/w4-0-enrollment-spike.md` records. Everything downstream of
> authentication is the unmodified product path.

The bypass is sound because **nothing on the request path reads
`enrollment_method`** — `agent_auth.go:38-116` checks only the token, its kind,
status, expiry and hash; the column is read in exactly three issuance sites
(`postgres_agent_auth.go:193`, `:237`, `:526`) and nowhere else. Verified live
across register / heartbeat / claim / log-bulk / finish / refresh
(`w4-rig.md` §Step 2).

Bring up / tear down:

```bash
docker compose -f test/ha/docker-compose.ha.yaml \
               -f test/edgecase/compose/k8senroll.override.yaml up -d --build
test/edgecase/tools/w4/w4-up.sh          # mints a credential, starts proxy + agent
test/edgecase/tools/w4/w4-down.sh        # SIGTERM both, print their final output
docker compose -f test/ha/docker-compose.ha.yaml \
               -f test/edgecase/compose/k8senroll.override.yaml down -v
```

The `k8senroll` overlay is optional for the rig itself — it exists so the
"same request, 403 direct / 200 through the interposer" control can be taken.
The agent runs **on the host** (route decision and its cost, `w4-rig.md`
§Step 1); **W4-2's RBAC-denial arm needs an in-cluster Deployment and is
deferred to the W4-2 task, not dropped.**

- `tools/w4/w4-k8s-inject.sh` — the first k8s fault tooling here. `inject.sh`'s
  verbs are useless for it: they take compose **service names** and hardcode
  `unified-cd-ha_default` / `unified-cd-ha-$svc-1`.

  | Command | Effect |
  |---|---|
  | `pods` | list this agent's run Pods (`app=unified-cd-agent`) |
  | `delete-pod <runId\|latest>` | delete by the `unified-cd/runId` label, not by the truncated pod name |
  | `annotations [pod]` | read `unified-cd/pool-status`, `pool-key`, `pool-run-id`, `pool-template` |
  | `block [reset\|hang\|<status>]` / `unblock` | **one-way agent→controller partition, one agent wide.** Measured: 40 s armed → the run stayed `Queued`, no pod, 56 blocked requests, while `agent1`/`agent2` kept heartbeating; cleared → claimed and `Succeeded` in 6 s |
  | `show` / `probe` | arm state, and one request through the proxy |

  **`block` shipped inert once and only an effect measurement caught it** — the
  W3-1 `s3-slow` lesson, repeated. Every state check passed while the agent
  claimed and ran a job 17 s into the armed window, because its 30 s claim long
  poll had entered the handler *before* the arm. Fixed by severing live
  connections on the arm transition (`ConnState` + `BLOCK-ARM severed N`);
  re-measured before being believed.

  **Why not `nginx-block`:** it denies an agent's source IP resolved via
  `docker inspect` on a compose container. A host-run agent has none, and its
  traffic shares the Docker-host address with every `curl` the scenario makes,
  so an IP deny would cut the instrument too. **Why not "SIGKILL all three
  controllers":** that also stops the reapers, archiver and scheduler, which
  W4-1 needs running while the agent is isolated.

- `tools/w4/w4-mint-credential.sh` / `w4-up.sh` / `w4-down.sh` — credential mint
  (product enrollment path), rig up, rig down. `w4-down.sh` reports the
  interposer's intercept count; **0 means the bypass was never in effect.**

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
and `docker/agent.Dockerfile:17` installs `docker-cli`.
**The ordering, with the cites, because a scenario author will need to defend
it:** in `runClaim` (`internal/agent/orchestrator.go`) a `post:` hook's log
writers are closed by `finishPostLogs(hookCtx)` at **`:706`**, the whole
`finally` pipeline runs at **`:727`** — both inside the main body — and
`FinishRun` is only called afterwards, at **`:787-788`**, wrapped in
`retryUntilSuccess`. There is no agent-side hook that runs *after* that call.

**SUPERSEDED BY W3-5 — do not seal by hand.** This section used to say that any
scenario needing a post-`FinishRun` log flush "must seal by hand
(`INSERT INTO run_log_archives`)". **W3-5 found two better routes and one factual
error, all executed or code-read at HEAD:**

- **A real seal with a synthetic sender is strictly stronger than a hand-planted
  row** — a hand-planted row only proves the guard reads a row; letting the real
  archiver seal proves it plants one the guard then reads. Cost: the same five
  API calls as W3-6's instrument (enroll → exchange → trigger → claim → finish),
  then push after the seal. See `scenarios/w3-5-seal-vs-flush.md` Part A.
- **A fully natural race needs no instrument at all** and hit on the first
  attempt: `partition <agent>` → `POST /runs/{id}/cancel` (terminal
  **synchronously**, `api_runs.go:374`, while the agent only polls every 5 s) →
  wait for the seal → `heal`. The agent's `p.pending` backlog is then re-offered
  into a sealed run. ~95 s per attempt. This is the cause
  `docs/troubleshooting.md:865` names verbatim. See Part B.
- **The sidecar window is NOT "structural, every run"**, contrary to the W3
  plan (`:107-115`). `CloseScopes` follows `FinishRun` by *microseconds to
  milliseconds*, while the archiver can only seal on its next **30 s** tick
  after that same commit (`cmd/controller/main.go:400`) — so the sidecar flush
  wins essentially always. A future sidecar-capable rig should not expect the
  structural shape.

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
| `artifact-large.payload.json` | `edge-artifact-large` | the **artifact** fixture (W3-2, W3-6): builds a `/dev/urandom` blob (`size_mb` param, default **64**) and uploads it. The upload duration IS W3-6's TOCTOU width (`api_artifacts.go:55` GetRun → `:79` Put, nothing between). Measured: 64 MiB → `upload_blob` **0.749 s** (`step5-bigbody-and-latency-recapture.txt`), 256 MiB → **3.060 s** (`step5-baseline.txt:89-94`) — ≈12 ms/MiB. **`s3-latency 3` widens the 64 MiB Put to 9.702 s** and does not break it (see the interposer notes above) — but ~~"which is the knob W3-6 should reach for first"~~ is **CORRECTED: W3-6 as executed rejected `s3-latency` and did not use the interposer at all.** The width knob that worked, and the one to reach for first here, is **payload size plus `curl --limit-rate` against `bigbody`'s `proxy_request_buffering off`** (32 MiB at `--limit-rate 1M` → controller `duration_ms=32374`), fired on a **positive in-flight signal** (`mc ls --incomplete`) rather than a sleep. Pausing the store behind the interposer is unusable for this race specifically, because the `DELETE` being timed itself calls `obj.Delete`/`obj.List` and would hang alongside the upload; and `s3-latency` at large values **breaks** the `Put` (W3-2, `502 … rt=21.037` at `s3-latency 30`) rather than widening it. See §"the width-knob rule for W4-W6" above. **Needs `compose/bigbody.override.yaml`** — required for **both** of its settings, the size cap and `proxy_request_buffering off`; without it the LB 413s anything over 1 MiB and the window does not exist at all. Random, not zeros, because the payload is compressed on the way out |
| `cache-user.payload.json` | `edge-cache-user` | the **cache** fixture (W3-1): `wipe` → `cache:` (`ttlDays: 1`, the real floor — `0` is silently rewritten to 30 at `orchestrator.go:980-982`) → `use_deps`, printing `CACHE-HIT`/`CACHE-MISS` plus the marker's plant timestamp. **The `wipe` step must stay first**: the host agent keeps a persistent per-job workspace, so without it a second run would find `deps/` still present and a "hit" would prove nothing. Verified end to end — run 1 on agent2 planted `01:54:14.857`, run 2 on **agent1** restored that same timestamp, so the hit crossed agents and can only have come from the object store |
| `cache-torn.payload.json` | `edge-cache-torn` | the **tearable** cache fixture (W3-1): same `wipe` → `cache:` (`ttlDays: 1`) → inspect shape as `cache-user`, but the payload is **256 x 65536 bytes of `/dev/urandom`** plus a last-sorting `zz-COMPLETE` sentinel, so "how far did the extract get" is a file count, a byte total and a truncated-file size rather than an impression. `cache-user`'s ~200-byte archive is delivered in one segment and **cannot** be torn. Unthrottled the restore takes ~100 ms; under `inject.sh s3-slow 262144` it is a ~64 s stream. **`inspect_deps` regenerates only on a COMPLETE miss** (zero entries) — on a torn restore it leaves the debris exactly as `extract` left it, which is what makes the deferred save's re-archival of that debris measurable |
| `secret-user.payload.json` | `edge-secret-user` | the **secrets** fixture (W3-3): step `env` references `{{ .Secrets.EDGE_KEK_PROBE }}`, which is what makes the claim response carry a non-empty `SecretsNeeded` and the agent take the `FetchSecrets` path (`internal/agent/orchestrator.go:161`). Prints only the secret's **length** (`secret-len=<n>`), never its value. `secret-user.yaml` is the same job in plain YAML. **The secret must be registered first** — `POST /api/v1/secrets/` (trailing slash required) with `{"name":..., "value":...}`, `204` on success |
| `w36-probe.payload.json` | `edge-w36-probe` | the **unclaimable** fixture (W3-6): `agentSelector: [kind:w36probe]` matches neither `agent1` nor `agent2`, so a run of it sits Queued until a curl-driven **synthetic agent** claims it. Its one step never executes — the run is terminalised through `POST /api/v1/agents/{id}/runs/{runId}/finish` — so the fixture exists to give an instrument a run it exclusively owns |
| `w35-probe.payload.json` | `edge-w35-probe` | the same shape for W3-5, with its own selector `kind:w35probe` so the two scenarios' instruments cannot collide. Used to put a genuinely `Succeeded` run in front of the **real** archiver and then push log lines at it after the seal (`scenarios/w3-5-seal-vs-flush.md` Part A/C) |
| `w4-tick.payload.json` | `edge-w4-tick` | the W4 **baseline** fixture: `agentSelector: [kind:kubernetes]`, one `echo` plus `hostname`. Trigger→`Succeeded` in **4 s**, pod gone within 6 s |
| `w4-pending.payload.json` | `edge-w4-pending` | a **copy** of `podcap-job` with the selector moved to `kind:kubernetes`, keeping the pod-level `nodeSelector: disktype: ssd` that no node satisfies, so the pod sticks `Pending` — W4-3's trigger. Deliberately renamed to `edge-w4-pending`: reusing `metadata.name: edge-podcap-job` would **overwrite the job row** and break W2-4 Part D's premise, which copying the file alone does not prevent |
| `w4-pending-reuse.payload.json` | `edge-w4-pending-reuse` | `w4-pending` plus `podTemplate.reuse: true` — the **pooled-pod not-ready** fixture (W4-3 Part B). Sends the claim down `pool.ClaimPod` and then wedges the pod `Pending`, so the arm can show that the pooled branch shares the **same** `podStartTimeout` (measured 30.193 s vs the fresh branch’s 30.198 s) and differs only in cleanup: a never-ready pooled pod is **deleted**, never released to the pool with `pool-status = idle`. Verified post-terminal by pod list and by pool annotation |
| `w4-reuse.payload.json` | `edge-w4-reuse` | the **`podTemplate.reuse`** fixture — nothing else in the repo exercises it. Prints `hostname` and reads/writes a `/workspace` marker, so reuse is observable two ways. Verified: three runs, one pod, marker carried across all three |
| `w4-longpod.payload.json` | `edge-w4-longpod` | 120 s of 1 Hz self-indexing ticks in a pod — the fixture for anything that must act on a run **while** its pod is alive (pod deletion, partition, podStartTimeout) |

**W4 fixture traps, both measured:** (1) `dsl.RequiredCaps` returns `pod` only
when `PodTemplateNeedsKubernetes` is true — a bare `containers:` list yields
**`container`**, so three of the four W4 fixtures are capability-schedulable on
the Linux agents and are kept off them *only by the label*. (2) The primary
container **must be named `job`** (`dsl.PrimaryContainerName`); a podTemplate
whose containers do not include one parses, validates and builds a Pod, then
fails every step with `container job is not valid for pod`.
`podcap-job.payload.json` has that shape and has never been executed.

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
`git clean -fdx`. It holds ~110 MB of container logs, psql output, nginx and S3
access logs, API reads and metrics scrapes — too bulky and too raw to commit, but
it is what every numeric claim in `FINDINGS.md` is derived from. See its own
`README.md` for per-wave coverage; coverage is uneven, and the entries that rest on
un-captured observations say so inline.

**Directory layout, and the one resolution wrinkle.** W0 and W1 sit directly under
the root (`w01/`, `w02/`, `w1/`, `w1-5/`, `w1-6/`). W2 and W3 each sit one level
down, so a W3 entry's `w3-4/partB-dup.txt` resolves to
`edgecase-evidence/w3/w3-4/partB-dup.txt`:

| Dir | Contents |
|---|---|
| `w3/w3-1/` … `w3/w3-6/` | one directory per W3 scenario |
| `w3/w3-infra/` | the three `W3-infra` entries plus the Task 3 rig build-out (Garage, the S3 interposer, the 413, the sidecar and artifact-format probes, W3-4's archive re-run) |

W3 totals ~4.9 MB across those seven directories, all verified byte-identical
against the session scratchpad with `diff -r` at the wave checkpoint. Two things to
know before citing from it: the wave's one Postgres statement log is stored gzipped
(`w3/w3-4/partB-pglog-raw.txt.gz`; the entry cites the uncompressed name and its
71,852 lines / 4,964,400 bytes, which `gunzip -c` reproduces exactly), and agent
credentials were scrubbed in place at the checkpoint — the prefixes the entries cite
survive, the full tokens do not.

While running a scenario, capture to the session scratchpad (fast, disposable)
and copy the wave's directory into the evidence root at the wave checkpoint.
**Sweep the whole wave for `uca_`/`uce_` before copying** — W3-5 left a full agent
credential in a dotfile, W3-1 found it, and W3-6 then repeated the mistake in three
files that nobody caught until the checkpoint swept everything.

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
