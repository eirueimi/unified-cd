# Security Hardening Wave — Agent Env Isolation, Authz Residuals, Webhook Injection, Cache/Shim/Image Provenance — Design

**Status:** Approved 2026-07-18. Scope and all breaking-change decisions confirmed by the user ("破壊的変更になっていいので" — breaking changes are acceptable; take the strictest safe option throughout).

**Baseline:** main @ `fda4bf3` (after PR #63 "per-agent enrollment and short-lived credentials").

**Goal:** Close the remaining provenance/authorization gaps found in the 2026-07-18 trust-boundary audit. The audit's two original Criticals were largely closed by PR #63; this wave fixes what #63 did not cover, plus five independent findings of the same class (*mutable or externally-controlled input crosses a trust boundary and reaches execution without verification*).

## Baseline correction (why this spec is not the raw audit)

The audit ran against `49355fa`. PR #63 then landed and changed the picture. Re-verified against `fda4bf3`:

- **Closed by #63:** the agent-token → all-secrets chain. `handleAgentSecretsFetch` now requires an authenticated non-`legacy` principal, requires `runId`, and passes `agentRunGuard`. Artifact upload now resolves a principal and applies `agentRunGuard` for non-legacy auth.
- **Still open:** everything in this spec.

Two of the audit's suggested fix directions were found infeasible or imprecise on inspection and are corrected here — see H-6 (shim) and A-3 (SecretsNeeded).

## Threat model note

A `developer`-role job author can obviously execute code *inside their own job*. That is trusted by design. Every item below is a case where such an author (or an outside party) reaches **beyond** that boundary: onto the agent host's credentials, into another job's workspace/artifacts/secrets, or onto the whole fleet.

---

## A. Agent credential isolation

### A-1 (Critical). Host agent leaks its entire process environment into every native step

**Code:** `internal/agent/runner.go:103, 133, 159` — `cmd.Env = append(os.Environ(), extraEnv...)`. When `extraEnv` is empty, `cmd.Env` stays nil and `os/exec` inherits the parent environment anyway, so the leak happens on **every** path. `internal/agent/backend_host.go` passes env through unscrubbed.

**Why it matters:** the agent's environment is where credentials live (`internal/config/agent.go:68-77`): `UNIFIED_AGENT_TOKEN`, `UNIFIED_CACHE_KEY`, `UNIFIED_CACHE_SECRET`. Any `run:` step can `echo $UNIFIED_CACHE_SECRET`. The cache credentials grant **direct S3 write access to the shared cache and artifact buckets, bypassing every controller-side check** — #63's short-lived agent credentials do not mitigate that at all.

**The correct pattern already exists in-repo:** the k8s agent's `imageStepEnv` (`internal/k8sagent/agent.go:485-487`) builds a *fresh* map from `step.Env` only. The host backend simply never got the same treatment.

**Design (allowlist — user-selected, breaking):**
- Build step environment from an explicit baseline instead of `os.Environ()`:
  - A minimal OS baseline required for a shell to function: `PATH`, `HOME`, `PWD`, plus platform essentials (`SystemRoot`, `TEMP`, `TMP` on Windows; `TMPDIR`, `LANG`, `TZ` on unix). The exact list is defined once in a single exported helper so host and k8s agree.
  - Plus the names listed in the existing `AgentConfig.ExposeEnv` allowlist (`internal/agent/agent.go:65`, flag `--expose-env`, env `UNIFIED_AGENT_EXPOSE_ENV`). **Note:** `ExposeEnv` currently feeds only `collectEnv` for the registration report (`agent.go:176`); this wave makes it the allowlist for step execution too, which is its natural meaning.
  - Plus `extraEnv` (the orchestrator's already-expanded step env), which wins over the baseline.
- `cmd.Env` is set **unconditionally** (never left nil), so no path silently inherits.
- Applies to all three call sites in `runner.go` and to the host backend's scoped/container paths.

**Breaking:** jobs relying on an inherited host variable now must name it in `--expose-env`. Requires a migration note and a clear failure mode (the variable is simply absent, not a crash).

**Never expose, even via `ExposeEnv`:** a denylist of credential names (`UNIFIED_AGENT_TOKEN`, `UNIFIED_CACHE_KEY`, `UNIFIED_CACHE_SECRET`, `UNIFIED_TOKEN`, `UNIFIED_CONTROLLER_KEY`, and any per-agent credential #63 introduced) is enforced *on top of* the allowlist, so an operator cannot foot-gun them into steps.

### A-2. Legacy shared-token auth bypasses per-run guards at five sites

**Code:** `internal/controller/agent_guard.go:104, 119`; `agent_auth.go:123`; `api_agent.go:36, 154`; `api_artifacts.go:30` — each gates its check on `principal.AuthMethod != "legacy"`. `api_secrets.go:90` is the one site that **rejects** legacy (fail-closed) and is correct as-is.

**Why it matters:** while compatibility mode is configured (`agentAuth.legacySharedToken` / `UNIFIED_AGENT_LEGACY_TOKEN`), one stolen shared token bypasses run-ownership everywhere except secrets. Combined with A-1 (which is how that token gets stolen), this reopens most of what #63 closed.

**Design (breaking, user-approved):**
- Apply `agentRunGuard` to legacy principals too: a legacy caller may only write to a run it actually claimed. Legacy agents keep working; they simply lose the ability to act on *other* runs.
- Make legacy compatibility **opt-in and off by default**. It already is effectively off when unset (`LegacySharedToken` defaults to the empty env var); this wave keeps that and adds an explicit, louder startup warning plus documentation that it is a migration-only affordance.
- `api_secrets.go`'s fail-closed treatment of legacy stays unchanged.
- The `agentId` path binding (`agent_auth.go:123`) is enforced for legacy as well, so a legacy caller cannot impersonate another agent's ID in the path.

### A-3. Secrets fetch is not constrained to the run's declared `SecretsNeeded`

**Code:** `internal/controller/api_secrets.go` — after the (now correct) guard, the handler loops over caller-supplied `req.Names` and decrypts each one. Nothing checks them against the run's needs.

**Audit correction:** the audit proposed "recompute or persist at claim time". Verified: `SecretsNeeded` is computed in the claim handler (`internal/controller/api_agent.go:269`) and returned in the claim response (`internal/api/types.go:101`) — it is **not persisted**. Therefore:

**Design:** recompute the allowed set at fetch time from the run's stored spec using the same helper the claim path uses (`collectSecretNames`), and reject any requested name outside it with 403. No schema change, no migration. Residual severity today is Medium (requires a valid credential *and* a claimed run), but this restores the documented guarantee in `docs/secrets.md:231` ("fetch only the secrets needed for a run").

### A-4. Artifact upload residuals

**Code:** `internal/controller/api_artifacts.go:30, 40`.

**Design:**
- The guard becomes unconditional via A-2 (legacy included).
- Build the object key with `artifactKey()` (`internal/artifact/store.go:115-123`) instead of raw `fmt.Sprintf`, restoring the `isSafeArtifactPathSegment` defense that the #26 traversal fix added everywhere else. Not currently exploitable (chi's `{name}` does not match `/`), but this is the one network-facing path with the defense missing.

---

## B. Webhook ingress

### B-1. Unauthenticated by default

**Code:** `internal/dsl/webhook_parse.go:42` maps an omitted `auth.type` to `"none"`; `internal/controller/api_webhooks.go:112` skips verification for it; the route (`internal/controller/server.go:350`) sits outside all auth middleware.

**Design (breaking, user-approved):** an **omitted** `auth` block becomes a parse error. `type: none` remains expressible but only together with an explicit `allowUnauthenticated: true`, so an unauthenticated public trigger is always a deliberate, greppable choice. Existing receivers that omitted `auth:` must be updated — migration note required.

### B-2. Payload interpolated into step shell text with no validation

**Flow:** payload → `paramsMapping` → `ExpandWebhookTemplate` (`api_webhooks.go:176-184`) → `resolveParams` → run params → `dsl.ExpandTemplate(step.Run, …)` (`internal/agent/orchestrator.go:441`) → `sh -lc` (`internal/agent/runner.go:103`).

**Why it matters even with valid HMAC:** a signature proves origin, not content. Any outside contributor who can open a PR controls `.Payload.pull_request.head.ref`. This is the GitHub-Actions script-injection class.

**Design (pattern validation — user-selected):**
- Add `pattern:` (a regex) to `params.inputs` in the DSL.
- Enforce it in `resolveParams` for **every** param source (webhook mapping, CLI `--param`, `call:` `with:`, schedule params) — not only webhooks, since the injection sink is shared.
- Params that are mapped from a webhook payload **must** be validated: the job's corresponding `params.inputs` entry must declare either `pattern:` or the explicit escape hatch `unvalidated: true`. A payload-mapped param with neither is a parse-time error on the receiver, so the risk is always an on-purpose, greppable declaration rather than an omission.
- Ship a sane default pattern suggestion in docs (e.g. `^[A-Za-z0-9._/-]+$`) rather than silently applying one, so the constraint is visible in the job spec.

`spec.Filters` are unchanged: they restrict *which* events trigger and sanitize nothing.

---

## C. Shared-resource provenance

### C-1. Cache is one flat global namespace; `restoreKeys` allows cross-job hijack

**Code:** `internal/cache/cache.go:35-38` (`objectKey` = `sha256(key)`, no owner component) and `:172-208` (`findBestMatch` scans every `.meta`, selects by prefix on the attacker-writable `OriginalKey`, tie-broken by **longest remaining TTL** — so the attacker, who controls `ttlDays`, always wins).

**Attack:** job A saves `key: "deps-pwned", ttlDays: 3650`; job B declares `restoreKeys: ["deps-"]`; B restores A's archive into its workspace and executes it in B's secret context.

**Design:**
- Namespace the object key by the **qualified job name** (`dsl.QualifiedName`, i.e. the same `team-a/build` identity the store and AppSource already key on, hashed): `caches/<sha256(qualifiedJobName)>/<sha256(key)>`. Job identity — not run ID — is the right granularity, since the whole point of a cache is reuse across runs of the same job.
- Constrain `restoreKeys` prefix matching to the caller's own namespace: `findBestMatch` lists only within that prefix, so it can never even see another job's entry.
- Record the owning job in `Meta` and verify it on restore (defense in depth against a stale/incorrectly-keyed object).
- Bound `ttlDays` with a maximum (currently only checked non-negative, `internal/dsl/parse.go:563`), so a single entry cannot pin itself forever.
- **Cache-layout change:** existing entries miss and regenerate. Acceptable (caches are derived data); documented.
- Deliberate cross-job cache sharing, if wanted later, becomes an explicit opt-in — out of scope here.

### C-2. `ucd-sh` shim is writable by native steps (audit direction corrected)

**Code:** `internal/agent/agent.go:693-701` writes `<wsBase>/.ucd-tools/ucd-sh` (0755). It is the default shell **and** keep-alive entrypoint for every container step (`internal/agent/claim_pod.go:254, 260`).

**Attack:** a native step (running as the agent user, cwd inside `wsBase`) does `cp evil ../../.ucd-tools/ucd-sh`, persistently backdooring the shell of every *containerized* job later run on that agent — jobs that chose container isolation specifically to avoid the host.

**Audit correction — the proposed fix would break dind.** The audit suggested moving `toolsDir` outside `wsBase`. The code documents at `internal/agent/agent.go:651-667` why that is invalid: with a **remote** container runtime (`DOCKER_HOST=tcp://…`, the dind setup), only `wsBase` is a shared mount between agent and daemon. A `toolsDir` outside it bind-mounts an *empty* directory at `/.ucd` with no error, and every container's entrypoint then fails with `exit status 127`. `toolsDir` **must** stay under `wsBase`.

**Design (verify-and-repair instead of relocate):**
- Hash the on-disk shim against the `go:embed` bytes and rewrite it if it differs — at agent startup **and immediately before each claim's container-creating path**. This bounds tampering to within a single run rather than persisting across runs, while fully respecting the shared-mount invariant.
- Log loudly (Error, with the agent and previous run) when a mismatch is repaired: shim tampering is a strong compromise signal, not routine.
- Additionally tighten mode to 0555 where the platform allows (the agent rewrites it as needed, so read-only is sufficient) — best-effort, not the primary control, since the agent user may own the file regardless.

**Honest scope:** a native step already runs with agent-user privileges and could tamper with the agent binary itself; the gain here is preventing native→container isolation escape and preserving the option of sandboxing native steps later.

### C-3. Default runner and pause images are pinned by mutable tag

**Code:** `cmd/agent/main.go:73-74` — `busybox:1.36`, `ghcr.io/eirueimi/unified-cd-runner:v0.0.3`. No digest anywhere in `internal/` and no attestation check (`internal/runtime/runtime.go:101` `Pull(ctx, image)` takes a bare string).

**Impact:** whoever controls the runner image repository can force-push `:v0.0.3` and execute code in the primary container of every isolated job lacking a `podTemplate` job container (`claimNeedsRunnerImage`, `internal/agent/claim_pod.go:190`). Structurally identical to the tracked `uses:` git-tag finding.

**Design:**
- Ship digest-pinned defaults: `…/unified-cd-runner:v0.0.3@sha256:<digest>` and a digest-pinned `busybox`. The tag stays for readability; the digest is what is enforced.
- Apply the same to the k8s-agent defaults (`cmd/k8s-agent/`, `internal/k8sagent/`).
- Document a digest-rotation procedure tied to the release process, otherwise the pin rots silently.

**Explicitly out of scope:** job-author `image:` values. A job author can already run arbitrary code in their own job, so tag mutability adds no privilege there; adding friction would be pure cost.

---

## Out of scope

- `uses:` git-template SHA pinning — tracked separately (its own TODO); same class, independent change.
- AppSource fetching by ref name after resolving `headSHA` (`appsource_reconciler.go:143-152`) — Low; trusted-by-design GitOps, only the audit-record accuracy is wrong. Revisit later.
- Replacing the shared object-store credential model, per-run credentials, or sandboxing native steps — larger redesigns.
- Job-author-controlled `image:` values (see C-3).

## Testing

Each item ships regression tests that fail before the fix:

- **A-1:** a native step cannot see `UNIFIED_AGENT_TOKEN` / `UNIFIED_CACHE_SECRET`; a variable named in `ExposeEnv` *is* visible; a denylisted name stays hidden even when listed in `ExposeEnv`; `extraEnv` still wins; the shell still functions (PATH resolves a binary).
- **A-2:** a legacy principal cannot write to a run it did not claim (per guarded route); it *can* write to one it did claim; legacy still cannot fetch secrets.
- **A-3:** a name outside the run's `SecretsNeeded` is rejected 403; names inside succeed.
- **A-4:** upload rejected for an unclaimed run; key built via `artifactKey()` (a traversal-shaped name is rejected).
- **B-1:** an omitted `auth` block fails to parse; `type: none` without `allowUnauthenticated` fails; with it, parses.
- **B-2:** a param value containing shell metacharacters is rejected by its `pattern:`; a webhook-mapped param without `pattern:` fails at parse; valid values still expand.
- **C-1:** job B cannot restore job A's entry via a crafted `restoreKeys` prefix; same-job round-trip works; `ttlDays` above the cap is rejected.
- **C-2:** a modified on-disk shim is detected and repaired before the next claim; container steps still get a working shell/pause entrypoint (this must be exercised against the real dind path, not just unit-mocked).
- **C-3:** defaults contain a digest; an isolated job runs end-to-end against the digest-pinned image.
- Full `go test ./...` green; generated artifacts drift-free.

## Verification beyond unit tests

The compose stack (`docker-compose.yaml`: controller + Linux agent + dind + Garage) must run a real isolated job, a cache save/restore, and an artifact round-trip after the change. C-2 and C-3 in particular can pass unit tests while breaking the real dind path.
