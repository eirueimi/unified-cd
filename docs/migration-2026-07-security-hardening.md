# Migration: security hardening (agent env, authz residuals, webhook injection, cache/shim/image provenance)

This release closes seven security gaps found in an internal audit of the
agent execution path, the legacy-agent compatibility mode, webhook ingress,
and the cache/shim/image supply chain. Most of the individual fixes are
backward compatible in spirit (they remove an unintended capability rather
than change an intended one), but several are enforced as hard failures, so
read this guide before upgrading a fleet that has been running any of the
old behavior.

Related, narrower migration guides this release also touches or depends on:
[Migration: agent authentication](migration-agent-auth.md) (per-agent
enrollment vs. legacy shared tokens — several fixes below tighten what the
legacy mode is still allowed to do) and
[Operations Guide: Rotating the default runner/pause image
digests](operations.md#rotating-the-default-runnerpause-image-digests) (item
7 below).

## Summary

| # | Change | Symptom if you're affected | Fix |
|---|---|---|---|
| 1 | Step environment is an allowlist | A `run:` script that used to read a host env var now finds it empty | Add the variable name to `--expose-env` / `UNIFIED_AGENT_EXPOSE_ENV` / `exposeEnv` |
| 2 | Legacy shared-token agents get per-run ownership checks | `403 run <id> is claimed by another agent`; legacy artifact upload always `403` and fails the step/run | Migrate the agent to per-agent enrollment |
| 3 | Secret fetch limited to the run's declared secrets | `403 secret not needed by this run` | Reference the secret in the job spec (`{{ secrets.NAME }}`) |
| 4 | `WebhookReceiver.spec.auth` is required | `apply`/GitOps sync fails to parse | Declare a real `auth.type`, or `type: none` + `allowUnauthenticated: true` |
| 5 | Payload-mapped params must declare `pattern:` or `unvalidated: true` | Webhook ingress `400`, names the param | Add `pattern: '^[A-Za-z0-9._/-]+$'` (or looser, as appropriate) to the input, or `unvalidated: true` |
| 6 | Cache is namespaced per job; `ttlDays` capped at 365 | One-time cache miss after upgrade; `restoreKeys` no longer match other jobs' entries; `apply` fails if `ttlDays > 365` | Nothing required — caches regenerate. Lower `ttlDays` if it exceeds 365 |
| 7 | Default runner/pause/sidecar images are digest-pinned | None by default; matters only when you rotate the image | Follow the rotation procedure in `docs/operations.md` when publishing a new image |

---

## 1. Step environment is now an allowlist

**What changed.** Before this release, every native `run:` step inherited the
agent's **entire process environment** — including `UNIFIED_AGENT_TOKEN`,
`UNIFIED_CACHE_KEY`, and `UNIFIED_CACHE_SECRET`. Any job author could read
those values with `echo $UNIFIED_AGENT_TOKEN` and, via the cache
credentials, write directly to the shared object store, bypassing every
controller-side check. Steps now get a minimal, explicit environment: an OS
baseline (just enough for a shell to function — `PATH`, `HOME`, etc.), plus
whatever the agent operator names in the `ExposeEnv` allowlist, plus the
orchestrator's own step env (`env:`, secrets, etc.).

**Symptom.** A job that relied on reading some host environment variable
(e.g. `GOPATH`, an ambient proxy variable, a tool's config-path variable)
now finds that variable empty or unset inside the step, with no error — the
step just behaves as if the variable was never set.

**Fix.** Name the variable in one of (in priority order: flag > config file >
env var, same as every other agent setting):

```bash
./bin/unified-cd-agent --expose-env GOPATH,HTTPS_PROXY
```

```bash
UNIFIED_AGENT_EXPOSE_ENV="GOPATH,HTTPS_PROXY" ./bin/unified-cd-agent
```

```yaml
# unified-agent.yaml
exposeEnv:
  - GOPATH
  - HTTPS_PROXY
```

**Agent credentials can never be exposed this way, by design.** A small
denylist (`UNIFIED_AGENT_TOKEN`, `UNIFIED_CACHE_KEY`, `UNIFIED_CACHE_SECRET`,
`UNIFIED_TOKEN`) is dropped unconditionally even if
an operator explicitly names one in `ExposeEnv` — there is no override, flag,
or config setting that will make a step see these values. If a job needs a
credential, pass it as a job-level secret (`{{ secrets.NAME }}`) instead of
relying on agent-host environment inheritance.

This does not affect containerized/isolated steps (Docker/Kubernetes) — those
never inherited the host environment in the first place; only native
(`spec.native: true`) steps and post-hooks are affected.

See [Configuration Reference: Agent Environment
Variables](configuration.md#agent-environment-variables) for the full
`exposeEnv`/`UNIFIED_AGENT_EXPOSE_ENV` reference.

---

## 2. Legacy shared-token agents are now subject to per-run ownership

**What changed.** [Per-agent enrollment](migration-agent-auth.md) (PR #63)
already enforced that a run's writes (step reports, log lines, outputs,
sidecar status, finishing the run) could only come from the agent that
claimed it — but the artifact-upload handler explicitly **skipped** that
check for legacy shared-token principals. A stolen or leaked shared token
could write artifacts to any run in the system, regardless of who claimed
it. That exemption is removed: every write path, including artifact upload,
now checks run ownership for every principal, legacy or not.

**Symptom.** An agent (legacy or enrolled) writing to a run it did not claim
under the agent ID it presents now gets:

```
403 run <id> is claimed by another agent
```

**The sharp edge:** the artifact-upload route (`PUT
/api/v1/runs/{runID}/artifacts/{name}`) has no `{agentId}` path segment for a
legacy caller to present an identity on — a legacy shared token can
therefore never match a run's `claimed_by` on that route. **Every legacy
artifact upload is now unconditionally rejected with `403`, regardless of
which run it targets.** This is not a silent skip: a `403` here is treated as
a step error, so any job with an `uploadArtifact:` step run by a legacy
agent **fails that step and the run** (unless the step sets
`continueOnError`).

**Fix.** Migrate the affected agent to per-agent enrollment — see [Enroll a
VM agent](migration-agent-auth.md#enroll-a-vm-agent) / [Enroll Kubernetes
agents](migration-agent-auth.md#enroll-kubernetes-agents) — **before**
upgrading if that agent runs any job with an `uploadArtifact:` step. There is
no configuration flag to widen legacy mode's reach here; the fix is always
enrollment.

---

## 3. Secret fetch is limited to the run's declared secrets

**What changed.** The agent secrets-fetch endpoint already required a valid
per-run credential, but it previously decrypted whatever secret names the
caller supplied in the request body — an agent holding a valid credential
for a run it owns could request the name of **any** secret in the store, not
just the ones that run's job spec actually references. The controller now
recomputes the run's declared secret set from its stored spec and rejects
any requested name outside it, before attempting to decrypt anything.

**Symptom.**

```
403 secret not needed by this run
```

The message is deliberately generic — it doesn't echo the requested name or
confirm/deny that the secret exists, so the endpoint can't be used to
enumerate the secret store.

**Fix.** Reference the secret in the job spec so it's part of what the
controller computes as `SecretsNeeded` for that run — e.g.:

```yaml
steps:
  - name: deploy
    env:
      API_KEY: "{{ secrets.API_KEY_PROD }}"
    run: ./deploy.sh
```

There's nothing to declare separately; the controller scans `env:`/`run:`
strings for `secrets.NAME` at dispatch time. If you were fetching a secret
through some path other than a plain job-spec reference (e.g. a custom
integration calling the secrets-fetch API directly with names not in the
job), that pattern no longer works — reference the secret from the job spec
instead.

See [Secrets Management Guide: Security Model](secrets.md#security-model)
and [Troubleshooting](secrets.md#troubleshooting).

---

## 4. `WebhookReceiver.spec.auth` is now required

**What changed.** Previously, an omitted `auth:` block silently defaulted to
`type: none` — meaning forgetting the `auth:` block published an
unauthenticated remote job trigger with no warning. `spec.auth` is now a
required field: a `WebhookReceiver` with no `auth:` block fails to parse.
Additionally, `type: none` by itself is no longer sufficient — it now
requires an explicit `allowUnauthenticated: true` alongside it, so an
unauthenticated webhook is always a deliberate, greppable choice rather than
an oversight.

**Symptom.** `unified-cli apply` (or GitOps `AppSource` sync) of a
`WebhookReceiver` manifest fails with:

```
webhook receiver "<name>": spec.auth is required (use type: hmac-sha256, github, or token; or type: none with allowUnauthenticated: true)
```

or, if `auth.type: none` is present without the flag:

```
webhook receiver "<name>": auth.type "none" requires allowUnauthenticated: true — an unauthenticated webhook lets anyone trigger this job
```

**Fix.** Declare a real auth type with a `secretRef` (preferred):

```yaml
spec:
  auth:
    type: hmac-sha256
    secretRef: WEBHOOK_SECRET
```

Or, if the receiver genuinely must stay unauthenticated (e.g. a trusted
internal-only trigger), opt in explicitly:

```yaml
spec:
  auth:
    type: none
    allowUnauthenticated: true
```

**Fail-closed at ingress too, not just at parse time.** A `WebhookReceiver`
row that predates this release — stored with `type: none` and no
`allowUnauthenticated` flag (because the old parser silently defaulted to
`none` and never set the flag) — is **rejected at webhook-ingress time**,
not just re-validated at the next `apply`:

```
401 webhook receiver "<name>" has auth.type "none" without allowUnauthenticated: true — declare allowUnauthenticated: true to accept unauthenticated requests, or adopt a real auth type (hmac-sha256, github, token)
```

No Run is created for a request rejected this way. **This means every
existing unauthenticated `WebhookReceiver` must be re-applied** (with
`allowUnauthenticated: true` added) after upgrading, or its webhook stops
firing — the old stored row is not good enough on its own even though it was
never touched.

See [Resources Reference: WebhookReceiver](resources.md#webhookreceiver) for
the full field reference and auth-type table.

---

## 5. Payload-mapped params must declare `pattern:` or `unvalidated: true`

**What changed.** A valid webhook signature (HMAC, GitHub, or token) proves
who sent the request — it says nothing about whether the request's
*content* is benign. Because param values are interpolated directly into
step shell text, an unconstrained `paramsMapping` entry that reads from
`.Payload` (e.g. `.Payload.pull_request.head.ref`, which an outside
contributor controls by opening a PR) is a command-injection vector — the
same class of bug as GitHub Actions script injection. Any job input targeted
by a payload-mapped param must now declare either a `pattern:` regex or an
explicit `unvalidated: true` opt-out.

**Symptom.** Webhook ingress (not `apply`) fails with `400`, naming the
receiver, the param, and the job:

```
400 webhook receiver "wh": param "ref" is mapped from the request payload but job "build" declares no pattern for it (add pattern: to the input, or unvalidated: true to accept it explicitly)
```

This check runs live, at ingress time, against the target job's *current*
spec — not at receiver-apply time — so it can surface even for a receiver
that was applied successfully before the job was created or edited. A
literal `paramsMapping` value that never references `.Payload` (e.g. `image:
myapp`) is author-controlled and is not affected.

If a value *does* reach a step but fails its declared pattern, that's a
separate, later failure:

```
param "<name>" does not match required pattern "<pattern>"
```

(the rejected value is never echoed in this error, since it may itself carry
the injection payload).

**Fix.** Add a `pattern:` to the job input the payload maps into:

```yaml
params:
  inputs:
    - name: ref
      type: string
      pattern: '^[A-Za-z0-9._/-]+$'   # suggested starting point for branch/tag/SHA-shaped values
```

Or, only when the value is genuinely free-form and never reaches a shell,
opt out explicitly:

```yaml
params:
  inputs:
    - name: ref
      type: string
      unvalidated: true
```

See [Job Reference: Input fields](jobs.md#input-fields) and [Resources
Reference: Payload-mapped params must be
validated](resources.md#payload-mapped-params-must-be-validated).

---

## 6. Cache is namespaced per job; `ttlDays` capped at 365

**What changed.** Cache entries used to live in one flat global namespace,
keyed only by `sha256(key)`. `restoreKeys` prefix matching scanned every
job's cache entries, tie-broken by attacker-controlled `ttlDays` — so one job
could plant an entry under a key crafted to match a *different* job's
`restoreKeys` prefix, and that job would restore and execute the planted
archive in its own secret context. Cache entries are now namespaced by
qualified job name (storage key incorporates `sha256(jobName)`), and
`restoreKeys` matching only ever scans the same job's own entries. `ttlDays`
is capped at 365 so a single entry can no longer pin itself (and its
storage) indefinitely via the TTL tie-break.

**Symptom.**

- A one-time cache miss for every job on its first run after upgrading — this
  is expected and not an error. Every cache entry saved before this change is
  orphaned under the new key layout (no job can address it) and is simply
  abandoned; the job's cache regenerates from scratch on that first miss.
  No migration or manual cleanup is needed.
- `restoreKeys` no longer matches entries saved by a different job — if you
  were relying on cross-job cache sharing via a shared `restoreKeys` prefix,
  that no longer works (it was the vulnerability, not a supported feature).
- `unified-cli apply` (or job parse) now fails if a job's `cache.ttlDays`
  exceeds 365:

  ```
  step "<name>": cache.ttlDays <N> exceeds the maximum of 365
  ```

**Fix.** No action needed for the namespacing change — caches simply
regenerate. If a job spec sets `ttlDays` above 365, lower it to 365 or less.

If you run the k8s-agent, the artifact-transfer sidecar's cache commands now
require `--job <qualifiedJobName>`; **upgrade the k8s-agent and its sidecar
image together** (they already have to be upgraded in lockstep for other
reasons — see [Operations Guide: Upgrades](operations.md#upgrades)). An old
sidecar paired with a new agent (or vice versa) does not understand the new
argument.

See [Job Reference: Cache entries are namespaced per
job](jobs.md#cache-entries-are-namespaced-per-job) and [Kubernetes
Integration: Artifacts and Cache](kubernetes-integration.md#artifacts-and-cache).

---

## 7. Default runner/pause/sidecar images are digest-pinned

**What changed.** The fleet-wide default images — the host agent's primary
container image for isolated jobs without their own `podTemplate` container,
the claim pod's pause (netns-holder) container, the k8s-agent's fallback pod
image, and the artifact-transfer sidecar image — were previously referenced
by mutable tag (`ghcr.io/eirueimi/unified-cd-runner:v0.0.3`, `busybox:1.36`,
`ghcr.io/eirueimi/unified-cd-artifact-sidecar:latest`). Whoever controls
those registry repositories could force-push the tag and execute code (or,
for the sidecar, exfiltrate its long-lived S3 credentials) in every isolated
job on every agent using the default. All four are now pinned to a specific
digest in `repo:tag@sha256:<digest>` form — the tag stays human-readable, but
the digest is what's actually pulled and enforced.

**Symptom.** None for a normal upgrade — the pinned digests correspond to
the same image content the previous tags pointed to at pin time. This only
matters when **you** (or the project) later publish a new runner/sidecar
image: the pin does not move automatically, so agents keep pulling the old
pinned image forever until the pin is deliberately updated. Job-author
`podTemplate`/container images are untouched by this change — pinning there
would add friction without security value, since a job author can already
run arbitrary code in their own job.

**Fix / ongoing requirement.** Treat updating the digest pin as a required
step of every runner/pause/sidecar image release, not an optional follow-up
— see [Operations Guide: Rotating the default runner/pause image
digests](operations.md#rotating-the-default-runnerpause-image-digests) for
the full procedure (resolving the manifest-list digest via `docker buildx
imagetools inspect`, which three constants to update, and the
`@sha256:[0-9a-f]{64}$`-anchored test guard that keeps a future edit from
regressing to a mutable tag).

---

## Rollout order

None of these seven changes depend on each other for rollout purposes, but
if you're also mid-migration on [per-agent
enrollment](migration-agent-auth.md), finish that first — items 2 and 3
above assume the ownership/secrets-fetch machinery from PR #63 is already in
place, and legacy mode remains connectivity-only compatibility during that
migration, not a place to accumulate new exceptions.

1. Upgrade the controller and agents together (this is a single release,
   not phased infrastructure like agent auth).
2. Re-apply every `WebhookReceiver` that relies on `type: none` with
   `allowUnauthenticated: true` added (item 4) — do this before or
   immediately after the controller upgrade, since ingress starts
   fail-closing on old rows immediately.
3. Add `pattern:`/`unvalidated:` to any job input targeted by a
   payload-mapped webhook param (item 5) — apply will otherwise start
   failing at the next webhook delivery, not at `apply` time, so this is
   easy to miss until a webhook fires.
4. Add any host env vars steps still need to `exposeEnv` (item 1).
5. Expect (and don't chase) a one-time cache miss per job (item 6).
6. If running legacy shared-token agents that upload artifacts, migrate them
   to per-agent enrollment before their next run with an `uploadArtifact:`
   step (item 2).
