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

**W4 is complete — see `FINDINGS.md` §"Checkpoint: W4 complete" for the wave's
tally, its two false premises, the RBAC blind spot, and what survives the rig.**
Five directories of raw captures are archived at
`<project parent>/edgecase-evidence/w4/`; **their names follow the TASK, not the
scenario** — `w4-2/` is the **rig**, `w4-2b/` is scenario W4-2, and `w4-1/` holds
both W4-0's spike and W4-1 — so read that archive's own README trap table before
resolving a `w4-*` citation.

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

The bypass is sound because **nothing on the request path compares
`enrollment_method`** — `agent_auth.go:38-100` checks only the token, its kind,
status, expiry and hash, and `GetAgentCredentialForAuth`
(`postgres_agent_auth.go:272-276`) does not even select the column. It is
**compared** in exactly two places, both credential issuance
(`postgres_agent_auth.go:193`, `:526`); it is *selected* by the identity
getters and surfaced read-only by the enrollment API and CLI, but never used to
gate a request. Verified live across register / heartbeat / claim / log-bulk /
finish / refresh (`w4-rig.md` §Step 2).

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
§Step 1). **W4-2 ran its RBAC-denial arm host-side** — a token-scoped kubeconfig
for the `w4-2-reuse-denied` ServiceAccount is enough to deny a verb, and that
arm produced the wave's headline violation (`FINDINGS.md:2393`). What still
needs an in-cluster Deployment is narrower: **running an agent whose identity is
the shipped `manifests/base/k8s-agent/` Role**, which no wave has done
(`FINDINGS.md` checkpoint §(c)(i)). *(Corrected at the branch review — this
paragraph previously asserted that the denial arm "needs an in-cluster
Deployment", which is a false technical requirement and was already false when
written.)*

- `tools/w4/w4-k8s-inject.sh` — the first k8s fault tooling here. `inject.sh`'s
  verbs are useless for it: they take compose **service names** and hardcode
  `unified-cd-ha_default` / `unified-cd-ha-$svc-1`.

  | Command | Effect |
  |---|---|
  | `pods` | list this agent's run Pods (`app=unified-cd-agent`) |
  | `delete-pod <runId\|latest>` | delete by the `unified-cd/runId` label, not by the truncated pod name |
  | `annotations [pod]` | read `unified-cd/pool-status`, `pool-key`, `pool-run-id`, `pool-template` |
  | `block reset` / `unblock` | **one-way agent→controller partition, one agent wide.** Measured: 40 s armed → the run stayed `Queued`, no pod, 56 blocked requests, while `agent1`/`agent2` kept heartbeating; cleared → claimed and `Succeeded` in 6 s |
  | `block <status>` | controller-shaped rejection. Measured: `block 503` → `http_code=503` at the probe while the direct `:18080` control stayed 200 |
  | `block hang` | accept and never answer. Measured (`w4-2-fixes/f5-hang.txt`): probe consumes its full deadline (`curl_exit=28`), agent sees `context deadline exceeded`, run `Queued` 40 s with no pod. **Recovery is slow** — `unblock` does not sever hanging requests, so the agent waits out its own client timeout (~24 × 2 s samples, vs `reset`'s 6 s). Do not use it where the window must close sharply |
  | `show` / `probe` | arm state, and one request through the proxy |

  **`block` asserts its own arm.** It probes through the interposer and exits
  non-zero unless the probe failed in the shape the mode promises; `unblock`
  requires a 200 back. Verified in all three modes and against a negative
  control (an interposer started without `-block-file` ignores the arm file —
  the verb now fails instead of printing "ARMED"). Before this, `probe_proxy`
  ended in a no-op `if … then : fi`: dead code shaped like a verification.

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
  interposer's intercept count, per log file. **Read `0` correctly: it means no
  enrollment was intercepted while these logs were being written, NOT that the
  bypass was not in effect.** The agent enrolls once at startup and then not
  again for ~40-45 min, so a short session legitimately reports 0; the
  supporting evidence is the `INTERCEPT` line at the agent's own startup. It is
  unsupported only if no log in the directory carries one. `w4-up.sh` **rotates**
  `enrollproxy.log` rather than truncating it, so a proxy restarted under a
  running agent does not destroy that line — truncation is what produced the
  misleading `0 enrollment exchange(s)` in `w4-2/step8-teardown.txt`.

**What a later wave inherits from this rig, stated once so it is not
re-derived.** Reusable as-is, all committed: the kind wiring (there is **no kind
CLI and none is needed** — Docker Desktop's Kubernetes *is* kind, node
`desktop-control-plane` on a Docker bridge literally named `kind`, and `ci` +
`unified-cd` both already exist); the controller-side config file and its
least-privilege RBAC (`compose/controller-k8senroll.yaml`,
`compose/k8senroll.override.yaml`, `k8s/w4-spike-controller-rbac.yaml` — exactly
`create tokenreviews` + `get pods`, which is also the minimum a real deployment
needs) and its kubeconfig **generator** (`k8s/make-spike-kubeconfig.sh`; the
kubeconfig itself is gitignored credential material and is regenerated, never
archived); the two-way network bridge (`docker network connect` onto the `kind`
network **in addition to** `default` — naming any network replaces implicit
default membership and would cut the controllers off from postgres/garage/nginx,
and **no `insecure-skip-tls-verify` is required** because the node cert's SANs
cover the name the kubeconfig uses); the enrollment interposer
(`tools/w4/enrollproxy`, 427 lines, stdlib only); the k8s fault verbs
(`tools/w4/w4-k8s-inject.sh`, the table above); and **seven** fixtures
(`workloads/w4-tick`, `w4-pending`, `w4-reuse`, `w4-longpod`,
`w4-pending-reuse`, `w4-poolkey-b`, `w4-poolkey-c` — each with both a `.yaml` and
a `.payload.json`, both validated through `tools/w3/fixcheck`) plus **five**
agent configs under `k8s/` (`w4-agent-config.yaml`, the W4-0
`.template.yaml`, and W4-2's `-pooldefault`/`-poolevict`/`-restricted`; the
count read "four" until the branch review — the template was omitted), the `w4-2-reuse-denied` Role and its restricted-kubeconfig
generator (`k8s/make-w4-2-restricted-kubeconfig.sh`, which reads the server URL
out of the developer's own kubeconfig — Docker Desktop publishes the apiserver on
a **dynamic** host port, so a hardcoded one would break on the next restart).

**Three bring-up gotchas that cost time once each and will cost it again.**
(i) **The three bring-up commands are ordered and skipping one fails silently**:
the overlay bind-mounts `compose/kubeconfig-k8senroll.yaml`, and if it does not
exist Docker creates a **directory** at that path, **all three controllers
exit(1)** on `is a directory`, and the empty directory persists to poison the
next `up` until it is `rmdir`ed. That looks like the bootstrap-PAT race and is
not — the race kills **one** replica, this kills three. If you do not need the
403 control, use the plain `test/ha` compose file, which is what W4-1, W4-2 and
W4-3 all did. (ii) **The bootstrap-PAT race is a race, not a certainty** — 3 up /
2 down across the wave's five cold bring-ups (`FINDINGS.md:2270`). Verify all
three are `Up`; do not assume failure either. (iii) **The primary container must
be named `job`** — a `podTemplate` supplying its own `containers:` without one
parses, validates, schedules and builds a Pod, then cannot execute a step
(`FINDINGS.md:2220`). Also carried forward: labels come from the **enrollment
token**, not the agent config; a `podTemplate` does **not** imply the `pod`
capability; and under `reuse` a Pod's name and `unified-cd/runId` label name the
**first** run forever (`FINDINGS.md:2246`).

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

### The W6 harnesses (scale / abuse)

W6 is instrument work first: every W6 scenario was blocked on measurement tools
that did not exist. They live in `tools/w6/` and are listed below **with the
capture that proves each one works** — the campaign has shipped two arms inert
while they passed every one of their own state checks, so "the code is present"
is never the evidence.

Build the two Go tools first (never `go run`; see `w6-build.sh`):

```bash
test/edgecase/tools/w6/w6-build.sh      # -> tools/w6/bin/{loadgen,ssehold}, gitignored
```

Most of them want the **direct controller ports**, and two also want the
`logfault` access-log format:

```bash
cd test/ha
export COMPOSE_FILES="-f docker-compose.ha.yaml \
  -f ../edgecase/compose/logfault.override.yaml \
  -f ../edgecase/compose/ctrlports.override.yaml"
docker compose $COMPOSE_FILES up -d --build
```

| Harness | What it does | The capture that proves it |
|---|---|---|
| `w6-synth-agent.sh` | curl-driven synthetic agent: `enroll` / `register` / `heartbeat` / `trigger` / `claim` / `own` / `run-once` / `lines` / `push-bulk` / `finish` / `token` / `forget`. Agent id, label, job and server are all parameters | `w6-1/step1-synth-agent.txt` — enroll → register(204) → `own edge-w6-probe` → run reads back `Running`, 5 lines pushed through the real bulk route and read back through the admin API → `finish` → `Succeeded` |
| `bin/loadgen` | N genuinely concurrent in-flight requests at ONE named controller; per-request start/end to CSV; `maxInFlight` swept from those timestamps | `w6-1/step2-loadgen.txt` — `-c 20 -mode burst` → `maxInFlight=20`, and the CSV shows all 20 sharing one start timestamp; the `-insecure-serial` control over the same 20 requests → `maxInFlight=1`, each start equal to the previous end. Both guards are proven by `loadgen/main_test.go` (`go test -buildvcs=false ./test/edgecase/tools/w6/loadgen/`), which is mutation-checked: disabling the over-report comparison makes `TestWarnsOnOverReport` fail with the pre-fix silent output |
| `bin/ssehold` | S SSE streams held for a measured window against ONE controller; per-stream status, connect latency, event counts, alive-at-end | `w6-1/step3-4-sse-pg.txt` — 6 streams on `:18083` held 35 s, `aliveAtEnd=6 diedEarly=0`, 30 events each. **The targeting is proven by a delta, not by a snapshot**: `controller3` has **no `listen` row at all in sample 1** and exactly **6 from sample 2 onward**, when the streams opened. `controller1` carries a **flat `listen` peak=8 across every sample including sample 1** — stale listen connections an earlier run left behind, which the pool had not released across this capture's window; that is the same **non-prompt-release** mechanism the `W6-infra` entry describes — **not** "the pools only grow", which is a mechanism claim corrected below (pgxpool applies 30 m idle / 1 h lifetime defaults the product leaves unset) — showing up as free corroboration. So: 6 new `listen` connections, all on the targeted replica, none on the other two |
| `w6-pgsample.sh` | `pg_stat_activity` on a grid, per replica and per **derived** pool, with a peak summary; `-p` also probes `/readyz` and `/healthz` on the same grid | same capture — the `listen` **delta** tracked the stream count exactly, and `w6-1/unplanned-pg-saturation.txt` shows the same instrument reading the saturated case. The `-p` health probe post-dates that capture and is proven off-rig: against a live throwaway port and a dead one it returns real codes and `000` respectively, and its summary was exercised on saturated-and-200, saturated-and-not-200 and never-saturated inputs |
| `w6-reqshape.sh` | per-request shape recorder off nginx's `logfault` access log, folded into buckets; plus `counter` and `contrast` verbs | `w6-1/step5-reqshape.txt` — 60 bulk requests resolved individually inside a 75 ms window (median gap 1 ms) that a 2 s grid would report as one number; `w6-1/step5-reqshape-b.txt` — a real agent's 15 auto-flush ticks at a measured 2.000 s median spacing; `w6-1/step5-reqshape-c.txt` — `contrast` and the combined-format guard |
| `w6-idleload.sh` + `w6-idleanalyze.py` | arms Postgres statement logging, captures a **bounded** window, always reverts, and reports q/s in total, per replica and per statement class. Despite the name it is a **generic window recorder** — W6-2a drove four loaded arms through it | `w6-1/step8-idlebaseline.txt` (arm, revert and read-backs) and `w6-1/step8-idle-report.txt` (the numbers below); **and `w6-2a/harnessfix/verify.txt` for the capture-leak fix — see the warning below** |
| `w6-2b-fault.sh` + `compose/nginx-w62b.conf` + `compose/w62b.override.yaml` | **W6-2b's fault**, URI-scoped to the agent log-bulk endpoint, in three arms no earlier tool provides: `outage` (dead upstream, instant 502, **nothing reaches a controller**), `flap` (per-`$request_id` `split_clients`, half the requests fail), `hang` (**the black hole** — a sink that accepts TCP and never answers). Plus `clear`, `probe`, `hangprobe` | `w6-2b/arm1/` — `outage` gave **11,326** 502s in a 300 s window against a predicted 11,325, in 151 flush passes of sizes 1, 1, 2, …, 150; `w6-2b/arm3/` — `flap` measured at **47.0 %** failure (135 of 287) and **287** requests over the same 300 s, a 39.5x difference from the same fault duration; `w6-2b/arm4/` — `hang` produced **`status=499 rt=60.000`** three times, i.e. the agent's own 60 s client timeout ending a request nginx never answered. **Read the two warnings below before reusing any of it** |
| `workloads/w6-chatty.yaml` | the first fixture in the tree that can reach the 1 MiB pending cap: **wide** lines (default 1 KiB) so ~1,024 lines fill it, plus a per-line heartbeat appended to a `/data` bind that **does not pass through the `LogPusher`** | `w6-2b/arm2b/` — 1.33 MiB emitted under outage produced a drop of exactly **263** lines and a marker reading exactly 263; `w6-2b/arm4/heartbeat.log` — the heartbeat's **176.3 s gap** against a 0.205 s median cadence is what proves the step itself stalled, measured off the logging path |
| `w6-build.sh` | builds `loadgen` and `ssehold` into a gitignored `bin/` | — |

**`w6-synth-agent.sh` is the promotion the README asked for.** `w3-5/synth.sh`
and `w3-6/synth.sh` were the same instrument twice, both session artefacts with
hardcoded absolute scratchpad paths, and neither was committed; the note above
("**one promotion is worth a W4 task**") is now discharged. It pairs with
`workloads/w6-probe`, whose `kind:w6synth` selector no real agent carries.
**Two traps it now encodes, both hit while building it:** the register route is
`POST /api/v1/agents/register` — a **collection** route with the id in the body
(`server.go:493`), not `/agents/{id}/register`, which 404s with no hint; and the
script deliberately does **not** export `MSYS_NO_PATHCONV=1`, because on Git
Bash that hands a native `curl.exe` unconverted `/tmp/...` paths and every
`-o` / `--data-binary @file` fails with exit 23. The rule is per-docker-call, not
per-script.

**`loadgen` treats "were they actually concurrent?" as a measurement.**
`tools/bulk-submit.sh` produces *depth* and no *rate* — it is a serial loop, and
a generator that was secretly serial would not fail, it would quietly produce a
smaller number. So `loadgen` sweeps the 2N start/end events and reports
`maxInFlight`. Ties are broken **end-first**:
Go's clock on Windows is coarse enough that a request's end and the next one's
start share a timestamp, and breaking ties start-first made the serial control
report `maxInFlight=2` — an instrument inventing overlap that provably did not
exist.

**It warns in BOTH directions, and the second guard is the one that matters.**
An under-report (`maxInFlight < -c`) means the rig serialised you. An
**over-report** (`maxInFlight > -c`) is *impossible* for a bounded worker pool —
a worker cannot start request k+1 before request k returned — so it is never a
product fact and always an instrument fault. Only the under-report was guarded
originally, and the consequence is the reason this paragraph exists: the
pre-tie-break build printed `maxInFlight=19` for `-c 8`, said nothing, and that
19 reached the `W6-infra` entry in `FINDINGS.md` and survived until review.
Recomputing the archived `w6-1/step2-sustained8.csv` gives **8 exactly** on both
the millisecond and the microsecond columns. **When an instrument's own number
is the evidence, guard both tails.**

**Aim rate-bearing harnesses at a controller, never at the LB, and that needs
`compose/ctrlports.override.yaml`** (18081/18082/18083 → controller1/2/3). The
base compose publishes only `18080` on nginx, and `test/ha/nginx.conf` has no
upstream `keepalive`, leaves `worker_connections` at 512, and can turn one
client request into three upstream requests via `proxy_next_upstream_tries 3`.
Through the LB you measure the rig. Confirmed the three ports are three distinct
processes (`w6-1/step-ctrlports-verify.txt`): each replica owns its own registry
(`internal/metrics/metrics.go:34`), and the evidence is the line
`metrics_families=128 / 112 / 113` with `http_requests_total_series=8 / 7 / 7` —
three ports, three different registry contents, on the same request.
**The capture's "three distinct registries" section below that line is empty**
(three blank lines: the per-port self-count command produced nothing), so cite
the `metrics_families` line and not that section.

**The request-shape recorder: why the access log and not the counter.** W1-6
derived its request numbers from `unifiedcd_agent_auth_events_total`, which
worked only because every request was *rejected*. Under W6's faults the requests
succeed, so that counter is useless — and
`unifiedcd_http_requests_total{route=".../logs/bulk"}` is the wrong instrument
for three further reasons, all now measured rather than argued:

- **Resolution.** A counter has no timestamp; a 2 s scrape grid yields a delta
  per cell and can never separate two requests 40 ms apart. The LogPusher's tick
  IS 2 s (`internal/agent/runner.go:211`), so a 2 s grid has the same period as
  the quantity being measured. The `logfault` format leads with `$msec`;
  measured, 60 requests inside 75 ms came out as 60 rows with a **1 ms median
  gap**.
- **Blindness.** W6's faults are injected at nginx. `contrast` fires five
  oversize requests that nginx answers itself with 413: **5 access-log rows, and
  a counter delta of exactly 6 — which is the six `/metrics` scrapes the verb
  itself made, and none of the five requests** (`w6-1/step5-reqshape-c.txt`).
- **Self-perturbation.** Scraping three controllers every 2 s is itself load
  that lands in the counter being read (`/metrics` goes through
  `metricsMiddleware`). Reading a log perturbs nothing.

The counter is still the right instrument for "how many did the controller
*serve*", which is why `counter` is a verb rather than deleted. `shape`
**exits 4** if the access log is in nginx's stock `combined` format (1 s
resolution, no `arm=` stamp) rather than silently producing a blunt curve, so it
requires `compose/logfault.override.yaml`. One limit worth knowing: a **413 is
logged with `arm=` empty**, because nginx rejects an oversize body before the
request enters the location that sets `$logfault_arm`.

**The PG sampler cannot give you "by application", and says so.** The plan asked
for a breakdown by pool/application; `application_name` is **empty for every
controller connection** — `grep -rn 'application_name\|ApplicationName'
internal/ cmd/` returns **zero** hits, no DSN sets it, and pgx does not default
one. Pool attribution is therefore **derived from the retained last-statement
text** and is honest about how far that goes: `listen` (only
`ListenForNotify` runs `LISTEN`, and it holds the connection for the stream's
life) and `lock` (only `AcquireAdvisoryLock` runs `pg_try_advisory_lock`) are
sound; **api and background are not separable** and are reported together as
`query`. The `listen` class was confirmed by effect — 6 SSE streams produced
exactly **6 new** `listen` rows on the replica they were opened against, absent
in the sample before they opened. Read that as a **delta**: `controller1` was
already carrying a flat `listen` peak=8 from an earlier run's connections that
had not been released across that window, so an absolute count over-reads.

**`-p` puts `/readyz` on the same grid, and it exists because of a specific
failure.** The `W6-infra` entry reports that `/readyz` was 200 while Postgres
refused every connection — but the backend count and the health status were
taken by two different commands about three minutes apart, so **no in-window
`/readyz` sample exists anywhere in the W6-1 archive** and what the health
surface does *during* saturation is still unmeasured. With
`-p 18081,18082,18083` the probe runs inside the same loop iteration as the
`pg_stat_activity` query, codes land in `<OUT>.health.csv` (separate file: an
HTTP status in the `count` column would print `peak=200` in the peak table), a
refused probe is recorded as `000` rather than dropped, and the closing summary
answers the question directly — of the samples at or above
`max_connections - 3`, how many still read 200. **Any wave asserting something
about the health surface under load should use it rather than a snapshot taken
afterwards.**

**`compose/maxconcurrent.override.yaml` — and the caveat that travels with it.**
The rig runs exactly 2 concurrent real runs (2 agents × `MaxConcurrent` default
1, `internal/agent/agent.go:218-221`). **There is no environment variable for
`MaxConcurrent`** — `internal/config/agent.go:147-186` enumerates every env var
the agent reads and it is not among them; `UNIFIED_AGENT_MAX_DETACHED` exists for
the *separate* detached pool and is easy to mistake for it. An env-only overlay
would be **silently inert**, so this one replaces `command:`. Verified by
effect (`w6-1/step7-maxconcurrent.txt`): with `W6_MAX_CONCURRENT=4`, 10
triggered `edge-longrun` runs gave **8 simultaneously `Running`** (2 `Queued`)
where the stock rig gives 2, and each agent container had created
`working0..working3`. **Every number taken under this overlay must say "N runs
across 2 agent processes", not "N agents"**: they share one process's CPU, one
HTTP client and one workspace filesystem. The one place it cuts the other way is
`p.mu`, which is per `LogPusher` per step — a stalled flush stalls one step's
stdout pipe, not all N.

**`w6-reqshape.sh follow -d` is GONE, and the reason generalises.** It had the
identical unstoppable-capture defect Task 2 fixed in `w6-idleload.sh` — `dc` is
a shell function, so the killed `$!` is the subshell and the docker-compose
plugin keeps the pipe. Measured in W6-2b before fixing: `follow -d 5` printed
`captured 34 lines`, and eight seconds later the same file held **56**. Use
**`window -o RAW.log -d SECONDS`**, which sleeps and then pulls exactly
`[T0,T1]` with `--since/--until`, with no background process and a
`-window.txt` sidecar so an old capture can bound its own re-analysis.
`follow -d` now exits 5 and names the replacement. Verified live: two
consecutive 12 s windows against a 1 Hz probe are disjoint (`06:05:57.426`
against `06:05:58.512`) and window 1 was byte-identical before and after window
2 ran.

**The blast radius, enumerated instead of dated — and it is zero.** The previous
version of this paragraph said "any W6 number taken with `follow -d` **before
2026-08-02** should be re-derived", which **protected nothing**: every W6
capture in the campaign was taken *on* 2026-08-02 and the fix landed the same
day, so the cutoff excluded the entire exposed set. Replaced with the
enumeration. `grep -rc reqshape scenarios/` returns **0** for
`w6-2a-log-write-amplification.md` (W6-2a used `w6-idleload.sh`, fixed
separately by Task 2 — **untouched by this defect**), **1** for
`w6-1-connection-pressure.md` and **5** for `w6-2b-logpusher-curve.md`. Of those
two: **W6-2b's arms are all bounded by `window` sidecars** (`-window.txt` in all
six evidence dirs), and **W6-1's three `step5-reqshape*.txt` captures are
self-bounding** — 60 of 60 deliberately fired requests, 15 of 15 ticks, filtered
by URI, so a longer file cannot change the count. **No published number moves;
nothing needs re-deriving.** The record is kept because the instrument was wrong,
not because a result was.

**The leak still exists in bare `follow`, which is warned about rather than
refused.** Only `follow -d` exits 5 (`w6-reqshape.sh:126-131`); plain `follow`
prints a WARNING that the PID it echoes is the subshell and not the
docker-compose plugin (`:133-134`) and then starts the same unstoppable capture.
That is deliberate — a caller who wants a live stream and tears down the stack
afterwards is fine — but **a bare `follow` capture is not window-bounded and
must not be treated as one.** Use `window` for anything a number will be read
off.

**A verification that is not idempotent can consume the thing it verifies —
re-probe at the point of use.** W6-2b's `hang` arm passed `hangprobe` at setup
(`curl` exit 28, a real hang) and **fast-failed with a 502 in 2.5 ms** forty
minutes later when a scenario actually armed it. The sink was
`while true; do tail -f /dev/null | nc -l -p 8080 >/dev/null; done`: plain
`nc -l` is single-shot and the `tail -f` never exits, so the loop never
iterates — **the sink served exactly one connection in its life and the
verification probe was the customer.** The black-hole arm had silently degraded
into the outage arm, and only the in-line re-probe caught it (`w6-2b/arm4-void/`
keeps the void run; its numbers are used for nothing). Fixed to
`nc -lk -p 8080 -e sleep 3600`, which has no per-connection state to exhaust.
This sharpens the campaign's standing rule rather than replacing it: "an arm is
verified when some capture measures its effect" was satisfied here and was still
not enough.

**One nginx-config trap worth knowing before writing another fault into
`compose/`.** `nginx-logfault.conf` sets `proxy_connect_timeout 2s` **inside**
the bulk location, so a runtime include cannot set its own without nginx failing
`-t` on a duplicate directive — which rules out every connect-level arm.
`nginx-w62b.conf` moves it out to the include for exactly that reason, and both
files say so in their headers. `nginx-w62b.conf` is a **derivative**, not a
replacement: it keeps the `logfault` log_format byte-for-byte (so
`w6-reqshape.sh shape` does not exit 4) and leaves `nginx-logfault.conf`
untouched so W3-4 stays reproducible.

#### The idle floor (measure every W6 number net of this)

Measured on the **plain `test/ha` rig**, no overlays, three controllers, two
agents, **zero jobs and zero runs**. **Two different windows, and the table says
which:** the statement rates come from the 300 s untouched capture at
**04:16:20-04:21:21** (`w6-1/step8-idle-report.txt`), the connection count from a
separate **33 s** sample at **04:25** on the same rig
(`w6-1/step8-idle-connections.txt`). They are not one measurement.

| Quantity | Idle value | Window |
|---|---|---|
| Total query rate | **21633 statements / 300 s = 72.11 q/s** across the stack | 300 s (04:16-04:21) |
| Per replica | 24.10 / 23.68 / 24.32 q/s (controller1/2/3) | 300 s |
| Postgres backends | **73-74 of `max_connections` 100** — about **26 free slots at rest** | **33 s (04:25), 7 samples** |
| Largest single consumer | `ClaimNextRun` (the agent claim long-poll), 10574 = **35.25 q/s**, i.e. **49% of the whole idle floor**, from just two agents | 300 s |
| Git resolver `ListPendingRuns` | 1508 per replica = **5.027 q/s per replica**, 15.08 q/s total (`internal/store/postgres.go:2064-2067` ← `internal/controller/scheduler.go:291`, inside `resolveGitPendingRuns`) | 300 s |
| Scheduler's `TransitionPendingToQueued` | 1508 on **controller1 only** (the lock holder) = 5.027 q/s. **Not `ListPendingRuns`** — it is `internal/store/postgres.go:437-440`, called at `internal/controller/scheduler.go:58`. The two rows are different statements against the same table; naming both `ListPendingRuns` made one call look both leader-gated and not | 300 s |
| `pg_try_advisory_lock` | 3181 = 10.60 q/s, split **55 / 1563 / 1563** — the leader retries far less because it already holds the key | 300 s |
| `DeleteStaleAgents` | 15 in 300 s = 0.05 q/s across the stack. **"1/min per replica" is an ASSUMPTION, not a measurement** — the analyser prints no per-replica split for this class (it is outside the top 8), so the ÷3 is unverified | 300 s |

**The denominator is nominal.** Every `/s` above divides by **300 s** while the
analyser's own printed window is **04:16:20.006 .. 04:21:21.487 = 301.5 s** — it
extends past the nominal end to swallow the `ALTER SYSTEM RESET` revert
statements. The rates are therefore **~0.5% high**: 72.11 q/s becomes 71.76 q/s
on the true window. Below the noise for every use here, and stated so nobody
re-derives it and thinks they have found a discrepancy.

**`FINDINGS.md:563`'s 5.006 q/s per replica for the git resolver still holds** —
5.027 measured, 0.4% apart, five waves later. What that entry did **not** say,
and what dominates the floor, is the claim long-poll: at 35.25 q/s it is **7×**
the git resolver. Two cautions on re-use: 300 s is too short to sample the
10-minute `oidc_states` cleanup (it does not appear at all), and the **73-74
connection floor is a settled-state figure, not a startup one** — a freshly
restarted controller set read **19** total backends. **"...and climbed to the
seventies over roughly ten idle minutes" is an INFERENCE across two different
stack instances, not a measured curve, and should be read as one:** the 19 is
the stack restarted at 04:03:31 (`w6-1/unplanned-pg-saturation.txt`), the 73-74
is a stack **torn down and re-`up`ed at ~04:12:16** (`w6-1/step7-maxconcurrent.txt`)
and sampled at 04:25, and **no intermediate sample exists** — the shape of the
climb, and whether it is even monotonic, is unmeasured. **"The pools only grow"
was the mechanism first written here and it is wrong as stated — see the
correction two paragraphs below**: `newPostgresPool` sets no `MaxConnIdleTime`
or `MaxConnLifetime`, but pgxpool supplies defaults when they are unset. What is
code-read is non-release **promptly**, not unbounded accumulation, and neither is
traced. See the W6-infra entry in `FINDINGS.md`. A wave that needs the curve
should take it with `w6-pgsample.sh -i 10 -d 900` from a cold start; it is cheap
and nobody has done it.

**Re-confirmed by W6-2a one day later: 72.01 q/s (24.15 / 23.99 / 23.86 per
replica), 0.4 % from the corrected 71.76 above** — both on their true windows.
W6-2a's nominal 150 s window in fact spans `04:46:10.038 .. 04:48:42.077` =
**152.04 s** (`w6-2a/floor/breakdown.txt`, corroborated by
`w6-2a/floor/persec.txt`'s `seconds observed=152`), so its first-reported
divide-by-nominal figure of 72.99 q/s was ~1 % high, exactly as this section's
own denominator caution predicts. **Correcting both sides tightens the agreement
from 1.2 % to 0.4 %** — and two things that arm adds rather than re-derives. The floor's **per-second** shape on one replica is
**median 22, max 61** statements/s (`w6-2a/floor/persec.txt`), which is the
number a peak is meaningful against; and that arm's own class breakdown carries
**zero** log-path statements, so the subtraction of the floor from a loaded arm
is checkable rather than asserted. Its connection samples were 69 at window open
and 74 at close, consistent with the 73-74 above.

**One correction to "the pools only grow", supplied because it is load-bearing
and cheap.** `newPostgresPool` (`internal/store/postgres.go:90-103`) indeed sets
no `MaxConnIdleTime` and no `MaxConnLifetime` — but pgxpool **applies defaults
when they are unset**: `MaxConnLifetime = 1h` and `MaxConnIdleTime = 30m`
(pgx **v5.9.2**, `pgxpool/pool.go:22-23`, reached from `ParseConfig` at
`:417-430`). So the code read does **not** support unbounded accumulation; it
supports "connections are not returned promptly, and are reclaimed on a 30-minute
idle / 1-hour lifetime horizon". W6-2a's own session is consistent with that and
does not test it: backends went 69 → 74 → 75 → 76 → 84 → 90 → 95 → 93 over
~45 minutes of continuous use, with no 30-minute idle period anywhere in it.

#### Two instrument corrections from W6-2a — read these before trusting either tool

**1. `w6-idleload.sh` used to leak its own capture, and it is fixed.** The
capture step was `dc logs -f ... > "${raw}" &` followed by `kill "${lpid}"`, and
**the kill did not stop the capture**: `dc` is a shell function, so `$!` is the
subshell, while the process holding the pipe is the `docker-compose` CLI
**plugin** two levels down. Three arms left three survivors still writing into
three "finished" files. The `floor` capture read **10948** statements when its
own analyser ran and **26523** when re-read after the next arm — and the second
number looks exactly as plausible as the first, which is the whole hazard. The
step is now a **bounded pull**, `docker compose logs --since T0 --until T1`,
with no background process to leak, and it writes the window to a
`-window.txt` sidecar so an already-captured file can bound its own
re-analysis. Verified live (`w6-2a/harnessfix/verify.txt`): two consecutive
25 s windows against a 1 Hz probe captured probes **5-20** and **27-42** —
disjoint, non-empty — and window 1's file was byte-identical before and after
window 2 ran. **Any capture taken before this fix must be analysed with an
explicit window**; W6-2a's `breakdown.py`, archived beside its evidence, is the
model.

**How far back the leak reaches — settled by re-running the analyser, so the
warning does not hang unresolved over the campaign's most-cited number.** The
only pre-fix capture that any published figure rests on is Task 1's 300 s
idle-floor arm, and **it is not inflated at all**. Task 1 ran exactly **one**
`idleload` arm, last in its session, so no later arm could append to it. The
archived raw *is* a superset of what was analysed live — **215,052 lines against
the 214,315 that `w6-1/step8-idlebaseline.txt:16` reports** — but the 737 extra
lines are a shutdown tail (`postgres-1 exited with code 0`) and contain **4**
`statement:` lines, all inside the window. **Re-running
`w6-idleanalyze.py w6-1/w6-idleload-idle-statements.log w6-1/ipmap.txt 300 idle`
on the archived file reproduces the report exactly: same window
`04:16:20.006 .. 04:21:21.487`, `statements 21633`, and the same per-replica
7296 / 7230 / 7104.** Zero drift, not the 2.4× the `floor` capture suffered. And
every W6-2a number is window-bounded by `breakdown.py` by construction. So the
rule above is a standing precaution for future re-analysis, **not** an open
question about any figure this README publishes.

**2. `w6-pgsample.sh`'s `TOTAL backends ... of max_connections=100` line reads
as saturation and is not.** `pg_stat_activity` includes Postgres's own
background workers, which carry a NULL `datname` and **do not consume
`max_connections` slots**. Measured (`w6-2a/post-bs10-connstate.txt`): a line
reading `total_backends=100 of max_connections=100` decomposed as 95 rows for
`datname='unified'`, one further `unified` row, and **4 NULL-`datname` rows** —
i.e. **96 client backends of 100** with `superuser_reserved_connections = 3`,
one free non-superuser slot rather than none. Split by `datname` before claiming
exhaustion.

#### Three more instrument corrections from W6-1 — and one rule that now covers all five

**1. `w6-pgsample.sh` could not survive the saturation it exists to measure.**
`set -euo pipefail` plus a bare `rows=$(psql_ …)` meant the first
`FATAL: sorry, too many clients already` ended the script. A 130 s grid over a
40-stream arm returned **2 samples**, both from *before* the exhaustion, and the
`-p` health series stopped with it — the option added specifically so a scenario
could read `/readyz` **inside** the load window could not survive the load
window. A second copy of the defect sat in the preamble's `max_connections`
read, so a capture *started into* an existing saturation never reached its own
loop. Fixed: `psql_ok` records `unavailable` and keeps sampling,
`PG_MAXCONN_`/`PG_RESERVED_` let a capture be started into saturation, and the
peak analyser counts `unavailable` rows rather than choking on them. **Verified
by effect** (`w6-1s/harnessfix-pgsample.txt`): 5 of 5 samples `unavailable`,
health probed on every one, `UNAVAILABLE: 5 sample rows` in the summary.

**2. The same tool's own saturation test was on the wrong side of the caveat
this README already records.** Its closing summary compared `total_backends` —
which includes Postgres background workers — against `max_connections - 3`, and
printed *"AT/NEAR max_connections in 156 of 180 samples"* for a window whose
**client** backends were 93 of a 97-slot budget. It now compares `db_backends`
against `max_connections - superuser_reserved` and treats `unavailable` as
saturated. **Documenting a caveat does not fix the code that has it.**

**3. `w6-synth-agent.sh heartbeat` terminalised the agent's own runs, and cost
two whole captures.** The verb sent `-d '{}'`. `handleAgentHeartbeat` gates
reconcile on **body presence**, not on the decoded slice
(`internal/controller/api_agent.go:88-91`), so `{}` reports an **empty** active
set — the "the agent restarted and forgot its runs" signal — and every
reconcilable run of that identity is failed as orphaned. A 25 s keepalive loop
killed the long-lived probe run it existed to protect, **4 s into the first
arm**, twice. The verb now takes run ids: `heartbeat <runId>...`. **Always pass
every run the identity still owns.** The product behaviour is correct and its
own comment documents it; the harness was wrong.

**`loadgen` gains `-delay`, and the reason it exists is a correction.**
`FINDINGS.md:2535` proposes settling its own open rate-vs-concurrency question
with "`-c 8` with a per-worker delay, so 8 in flight at ~50 req/s". **That
experiment cannot be run, by arithmetic rather than tooling.** In-flight = rate ×
latency, so 8 in flight at 50 req/s needs 160 ms of server latency and the
endpoint answers in ~2 ms; the same 8 workers paced to 50 req/s hold **~0.15**
in flight. Measured: `-c 8 -delay 400ms` gives `meanInFlight=0.03`. **In-flight
is an OUTCOME; the only controls a client has are worker count and pacing.** So
`-delay` isolates *rate* at a fixed worker count, and the complementary
"concurrency at ~zero rate" corner needs a **latency-bearing** endpoint —
`ssehold`, or the agent claim long-poll. A paced GET cannot do it. The flag
suppresses the under-report guard (a paced run is *meant* to sit below `-c`) and
prints a `PACED:` line instead; the **over-report** guard is untouched.

**The rule that now covers all five instrument defects this wave.** W6-2a: a
capture that would not stop. W6-2b: the same defect in a second tool, plus an arm
whose verification consumed it. W6-1: a sampler that stops when the fault starts,
and a driver that does **not** stop when its session does — a `nohup driver.sh &`
outlived its call by ~6 minutes and, because its stdout was block-buffered, its
log showed only the first of four arms while it silently ran a 200-worker
max-rate arm underneath a "zero-load control" taken on the same rig. That control
appeared to show a stack going 23 → ≥97 backends in 12 s with no load, which
would have been a spectacular false finding. **An instrument's failure mode and
its subject's failure mode must not be the same event, and "the process looks
finished" is never evidence that it is.** Corollary, learned the expensive way:
**any capture whose window overlaps another arm's window is void** — run arms in
the foreground, one per invocation.

#### What W6-1 settled — cite these rather than re-deriving them

- **The SSE ceiling on this rig is 24 concurrent streams, not ~100.** One arm,
  40 streams, one replica, from a 59-backend floor:
  `aliveAtEnd=24 diedEarly=16 non200=0`. And a subscriber is **not** one
  connection: 24 survivors consumed a 38-slot budget, because each also spends
  api-pool connections per NOTIFY wake.
- **The open "200 with backfill then a silently dead stream" question
  (`FINDINGS.md:2537`) is ANSWERED, and the phrasing needed correcting:** the
  stream is **closed**, not dead. 200, complete backfill (14-18 events), then the
  server ends the body. An `EventSource` reconnects into the same refusal rather
  than hanging.
- **Rate versus concurrency (`FINDINGS.md:2535`'s open question): concurrency
  wins, and it is not close.** Same rig, same replica, same session — 8 workers
  at **20 req/s** did **not** saturate (1,200/1,200 `200`, zero 401s); **60
  concurrent claim long-polls at 3 req/s DID**, with every request answered 200.
  Rate is an independent second route (8 workers at 2,451 req/s saturates, 31.8 %
  401 — `:2517` reproduced from a clean floor), but it takes ~800× the rate.
  `:2535`'s own caution — *"Do not read this entry as '8 concurrent requests at
  any rate will do this'"* — is **confirmed**.
- **`/readyz` is 200 in 144 of 145 saturated in-window samples**, and a warm pool
  makes saturation **completely silent server-side**: zero controller `ERROR`
  lines across two fully saturated arms. Background jobs only starve when they
  need a *new* connection, i.e. after a restart.
- **A settled, recreated stack idles at 68 client backends** (59 at `up`, 68 by
  ~7 minutes, flat thereafter, `/readyz` 200 throughout) — the measured version
  of the curve this README says "is cheap and nobody has done it". Note it is
  **client** backends; the 73-74 figure elsewhere is `total`.
- **`docker compose down -v` / `up` re-assigns container IPs**, so per-replica
  attributions are comparable only *within* one stack instance. Totals are
  comparable across recreates; splits are not.

#### The log write path, measured (W6-2a) — do not re-derive

`scenarios/w6-2a-log-write-amplification.md` carries the full derivation. The
numbers a later W6 arm will want:

- **One appended log line costs 2 statements** — `INSERT INTO logs` plus
  `SELECT pg_notify` (`internal/store/postgres.go:918-937`) — with no
  transaction and no multi-row insert, so an N-line request is **2N** sequential
  round trips. Measured exactly, five times: `2N = 4004` for the 2002-line
  `logburst` fixture, `2N = 70200` for 35,100 lines.
- **The batch guard is a constant, not a term.** `handleAgentLogBulk` runs
  `agentRunGuard` once per distinct `runID` and an in-process LRU answers
  thereafter, so the cost is `2N + G` with **G measured at 2** — one `GetRun` per
  replica that saw the run for the first time.
- **Each SSE subscriber multiplies it by 2, and the multiplier lands on the API
  pool, not the listen pool** (`sse.go:120`/`:138` call `s.store`, which
  `cmd/controller/main.go:270`/`:339` make the api-pool store). Measured at
  exactly `2002 × S` for S = 1, 5, 10, alongside exactly S `listen` backends.
- **A released listen-pool connection keeps its `LISTEN`s** (no `UNLISTEN`
  anywhere, and pgxpool's `Release` does no reset), so the real multiplier is
  "streams sitting on connections that carry this run's channel", not S. See the
  W6-2a entries in `FINDINGS.md`.
- **Peak: 11,085 statements/s on one replica** from one 2,002-line burst with ten
  viewers, against that replica's resting median of 22/s — about 504×.
- **Ingest is ~812 lines/s per request** (1.23 ms/line), identical through the LB
  and direct to a controller, so the loop is the cost and nginx contributes
  nothing. A 1 MiB body — the most the reference LB passes — is **7,600 lines,
  15,200 statements and 9.356 s** on one goroutine and one backend.
- **`logburst`'s `burst-end` arrives 30 s after the burst**, so a `first..last`
  span over a run reads as a ~27 s SSE backlog that **is not there**. Read the
  per-second histogram, never the span — this one survived a first review before
  the histogram killed it.

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
| `longrun.payload.json` | `edge-longrun` | long-lived run for reaper timing. **It is also already a slow trickle** — `for i in $(seq 1 300); do echo "tick $i"; sleep 1; done`, i.e. 300 lines at 1 line/s over ~300 s — which the W6 plan missed when it assumed `tick` (30 lines / 30 s) was the longest emitter available. `longrun.yaml` did not exist until W6 and was reconstructed from this payload, byte-verified to round-trip |
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
| `w6-trickle.payload.json` | `edge-w6-trickle` | the **slow-trickle** fixture (W6-S2b Arm 1): params `lines` (default 600) and `interval_s` (default 1), so 10 minutes at 1 line/s out of the box. Deliberately on the sparse side of `LogPusher`'s 4 KiB `flushBytes` threshold, so **every** flush is a 2 s timer flush and pending grows by exactly one batch per tick — the quadratic regime, which by construction **never drops** (the 1 MiB cap counts line text only, so the ceiling is tens of thousands of batches). **Do not use it to look for the drop marker**; that needs the chatty `logburst` and is a different arm |
| `w6-probe.payload.json` | `edge-w6-probe` | the **unclaimable** fixture for `tools/w6/w6-synth-agent.sh` — `agentSelector: [kind:w6synth]` matches neither `agent1` nor `agent2`, so the synthetic identity owns the run's whole lifecycle. Same shape and reason as `w35-probe` / `w36-probe`, with its own selector so a W6 instrument cannot collide with a re-run of either W3 scenario |

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
| `w6/w6-1/` | W6 Task 1, the harness build-out: one capture per harness verification, the 300 s idle-load statement log (12 MB, the wave's largest single file) and the connection-saturation captures behind the `W6-infra` entry. **Read its `NOTES.txt` first** — it flags one superseded analysis file that is kept as evidence of an analyser bug and whose numbers must not be used |

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
