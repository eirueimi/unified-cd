# Design: Vault/OpenBao Transit KeyManager

**Date:** 2026-07-18
**Status:** Approved (pending implementation plan)
**Base commit:** `df28d1e` — file:line citations verified against this revision.
**Follows:** `2026-07-18-secrets-v2-design.md`, which established the `KeySource` /
`KeyManager` seam and deliberately left the KMS provider unimplemented.

## Problem

secrets v2 removed the controller's key-encryption key (KEK) from the database
and settled the configuration surface for an external KMS, but implemented no
provider. Today `KeySource.Resolve()` returns an error for any `UNIFIED_KMS_URI`
(`internal/config/keysource.go:58-59`, `kmsError` at `:85-99`), so the only
working production key source is a local file.

That leaves three gaps:

- **The KEK is still held by the controller.** A local key file must be
  distributed to every replica and backed up independently; whoever holds it
  holds every secret.
- **There is no key rotation.** secrets v2 shipped a version byte and
  provider-tagged wrapped DEKs as groundwork, but no mechanism uses them.
- **There is no audit trail for key use.** Nothing records that a DEK was
  unwrapped, so a compromise leaves no evidence of what was decrypted.

## Goal

Implement a `KeyManager` backed by HashiCorp Vault's (or OpenBao's) Transit
secrets engine, so the KEK never leaves the KMS and the controller holds only a
revocable credential. Support both ends of the deployment range the project
targets: a single-machine operator with a static token, and a Kubernetes
deployment that authenticates with no distributed secret at all.

## Non-goals

- **AppRole authentication.** Deferred. See "Why not AppRole first" below — it is
  roughly thirty lines on the abstraction this design establishes, and its
  security advantage over a periodic token only materialises with orchestration
  machinery this change will not build.
- **Key rotation / rewrap.** Transit versions its own ciphertext (`vault:v1:…`)
  and keeps decrypting older versions after a rotation, so an operator can rotate
  the Transit key without any change here. A `rewrap` pass is only needed to
  retire old key versions, and is future work.
- **Cloud-native KMS providers** (AWS KMS, GCP KMS, Azure Key Vault). The URI
  scheme surface accommodates them; Vault runs in all those environments, so
  Transit alone already covers the portability requirement.
- **Bundling a production Vault deployment.** See "Production topology".

## Context and constraints

- **The seam already exists and does not change.** `secrets.KeyManager` is two
  methods, `EncryptKey` / `DecryptKey` (`internal/secrets/keymanager.go:14`).
  Transit's encrypt/decrypt map onto them directly.
- **Provider tagging already works.** secrets v2 made `LocalKeyManager` prefix
  its wrapped DEKs with `local:`; Transit ciphertext is natively self-describing
  as `vault:v1:…`, so the two are distinguishable with no new mechanism.
- **The default developer experience does not change.** `UNIFIED_DEV_MODE=1`
  keeps using an ephemeral local key. Vault is opt-in, which is what makes it
  acceptable for the compose overlay to carry a fixed dev token.
- **Startup is synchronous.** `Resolve()` is called from `cmd/controller/main.go`
  before the server starts, so whatever it does determines whether a Vault
  outage prevents the controller from starting.

### Verified external facts

These shaped the design and were confirmed against vendor documentation rather
than assumed:

- A **periodic token**'s TTL resets to its configured period on every renewal, and
  "as long as successfully renewed within each period, they will never expire" —
  so a correctly issued token needs *renewal*, not *rotation*. An
  `explicit_max_ttl` overrides this and revokes the token regardless. The system
  max TTL defaults to **32 days**. **Batch tokens cannot be renewed at all.** Only
  the *initial* root token generated at initialization has no expiry.
  ([Vault — Tokens](https://developer.hashicorp.com/vault/docs/concepts/tokens))
- **AppRole** logs in at `POST /v1/auth/approle/login` with `role_id` +
  `secret_id`, returning `auth.client_token` and `auth.lease_duration`. The
  documented production pattern uses a **trusted orchestrator delivering the
  SecretID via response wrapping**, so the secret never appears in logs or
  configuration.
  ([Vault — AppRole](https://developer.hashicorp.com/vault/docs/auth/approle))
- **Vault HA is active/standby and "does not enable increased scalability"** —
  standby nodes forward requests to the active node over mutually-authenticated
  TLS rather than serving them. HA provides failover, not throughput.
  ([Vault — High Availability](https://developer.hashicorp.com/vault/docs/concepts/ha))
- **Concourse CI**, a direct peer, supports periodic token / userpass / AppRole /
  cert, configures them through a generic `CONCOURSE_VAULT_AUTH_BACKEND` +
  `CONCOURSE_VAULT_AUTH_PARAM` pair rather than per-method flags, and renews "at
  half of the token's lease duration".
  ([Concourse — Vault](https://concourse-ci.org/docs/operation/creds/vault/))

## Design

### 1. Authentication

Two methods ship: **token** and **kubernetes**.

```go
type vaultAuth interface {
    login(ctx context.Context) (token string, ttl time.Duration, renewable bool, err error)
}
```

- `staticToken` — reads the token from a file or environment variable and reports
  Vault's own view of its renewability.
- `kubernetesAuth` — posts the pod's projected ServiceAccount JWT to
  `auth/kubernetes/login`.

**A single renewal loop is shared by every method**, because the expensive part
is common: both produce a token with a TTL that must be kept alive. What differs
per method is only how a fresh token is obtained — one HTTP call. Adding AppRole
later is one `login` implementation, not a second lifecycle.

The loop renews at **half the remaining lease**, following Concourse. When a token
reports `renewable: false` (a batch token, or a static token Vault will not
renew), the loop does not attempt renewal and instead re-logs-in when the token
stops working.

#### Why not AppRole first

Naively deployed, AppRole replaces one long-lived secret (a token in a file) with
another (a SecretID in a file), so its security advantage is small. Its real value
comes from the response-wrapping pattern the documentation recommends, which
requires a trusted orchestrator this change will not build.

Kubernetes auth, by contrast, eliminates the distributed secret outright: the
ServiceAccount token is already projected into the pod by the kubelet and rotated
without operator involvement. For a project that ships Kubernetes manifests and
introduced per-agent Kubernetes enrollment in #63, it is both the higher-value
method and the more consistent one.

### 2. Configuration

Following Concourse's shape, so adding a method never changes the schema:

| Variable | Purpose |
|---|---|
| `UNIFIED_KMS_URI` | Existing. `hashivault://<key>` or `hashivault://<mount>/<key>` |
| `UNIFIED_VAULT_ADDR` | Vault/OpenBao address |
| `UNIFIED_VAULT_AUTH` | `token` (default) or `kubernetes` |
| `UNIFIED_VAULT_AUTH_PARAM` | Method-specific parameters, comma-separated `key=value` pairs |
| `UNIFIED_VAULT_TOKEN_FILE` | Token method: path to a file holding the token |
| `VAULT_TOKEN` | Token method: fallback, matching Vault's ecosystem convention |

**URI form.** `hashivault://unified-cd-kek` names the key on the default `transit`
mount. `hashivault://kms-transit/unified-cd-kek` names an alternate mount, for
operators who mount the engine elsewhere. A URI with more than two path segments
is a configuration error rather than a guess.

**`UNIFIED_VAULT_AUTH_PARAM`** is parsed as comma-separated `key=value` pairs
(`role=unified-cd,mount=kubernetes`). The `kubernetes` method requires `role`;
`mount` is optional and defaults to the method name. An unrecognised key is a
startup error, not silently ignored — a typo in a security-relevant parameter
must not fail open.

**A file is preferred over the environment variable** for the same reason the KEK
is: environment values leak into `docker inspect`, process listings, crash dumps,
and child processes, and the controller spawns `git`. Reading from a file also
lets an operator replace a token **without restarting the controller**, which
matters for the non-periodic tokens that must genuinely be rotated.

Unlike the KEK, both paths are supported rather than one. A Vault token is
materially less catastrophic than the KEK — it is renewable, revocable, and
scoped by policy — and `VAULT_TOKEN` is the convention every Vault operator
already knows.

### 3. Client library

The official `hashicorp/vault/api` client, which OpenBao's API compatibility
lets us use for both.

secrets v2's reasoning ("two methods, hand-rolled HTTP is enough") applied when
the scope was encrypt/decrypt. Adding authentication and token lifetime
management changes the calculus: renewal is fiddly, easy to get subtly wrong, and
sits directly under the encryption of every secret. The dependency is worth not
hand-rolling that.

### 4. Availability

**Startup fails fast.** `Resolve()` attempts a login; if it fails, the controller
exits. At startup a transient outage and a misconfiguration are indistinguishable,
and a controller running with a broken key manager fails every secret operation
anyway. This matches the existing behaviour for an unresolvable key
(`cmd/controller/main.go:247-251` logs and calls `os.Exit(1)`). Kubernetes
back-off retries and recovers; compose surfaces the error immediately.

**Runtime is resilient.** A running controller is not killed by a Vault blip: the
renewal loop re-authenticates, and individual operations fail and are retried by
whatever requested them. Startup is strict; steady state is patient.

**DEK cache.** Transit is called once per secret read, so every job claim carrying
secrets hits Vault. An LRU cache of unwrapped DEKs, keyed by the wrapped-DEK bytes
(themselves ciphertext, not secret), bounded by size and a short TTL, with the
plaintext DEK **zeroed on eviction** — matching the zeroing discipline already in
`internal/secrets/crypto.go`.

The TTL is what makes a Transit key rotation or revocation take effect within a
bounded window rather than never.

**The cache is not an HA substitute, and the documentation must say so.** It only
helps for a recently-used secret; a job requesting an uncached secret while Vault
is unreachable still fails. Combined with the verified fact that Vault HA is
active/standby and does not add throughput, the honest statement is: HA shortens
unplanned outages, the cache absorbs brief blips and reduces load on the single
active node, and neither replaces the other.

Relative to local-key mode, where the KEK sits in process memory for the lifetime
of the controller, a few minutes of cached DEKs is a **stricter** posture, not a
new class of exposure.

### 5. Error handling

Startup errors are fatal and distinguish causes, because different causes belong
to different people:

| Cause | Message names |
|---|---|
| Address unreachable | `UNIFIED_VAULT_ADDR` and the network path |
| Authentication failed | The method, and what to check for it |
| Transit key missing | The key name and the command to create it |
| Permission denied | The exact capabilities the policy needs |

Conflating "unreachable" with "permission denied" sends an operator to the wrong
team.

At runtime:

- **Unreachable** — an *availability* event. Logged distinctly from
  `ErrBindingMismatch`, which is a *security* event. The two must not share a log
  line or severity.
- **Token expired or revoked** — re-login, then retry the operation once.
- **Permission denied** — do not retry. A policy problem does not resolve by
  waiting.

`VaultKeyManager.DecryptKey` mirrors `LocalKeyManager`: a blob that does not begin
with `vault:` yields `ErrProviderMismatch`, so opening local-wrapped data under a
Vault configuration reports precisely instead of as an opaque authentication
failure.

### 6. Production topology

**Vault is operator-provided.** The controller treats it as an external dependency
it authenticates to, exactly as it already treats Postgres and object storage. In
production that means either an existing organisational Vault, or a deployment
made with the official HashiCorp/OpenBao Helm chart.

This follows the convention the project already documents for its other external
dependencies in `docs/high-availability.md`: managed Postgres ("Cloud SQL, Amazon
RDS/Aurora, or a Patroni primary+standby", `:299`) and managed S3 (`:307`). The
bundled single-node manifests exist to get started, not to run production.

**No Vault HA manifests are bundled.** Hand-writing a Raft StatefulSet with
auto-unseal would duplicate a maintained upstream Helm chart, and would ship an
artifact CI never exercises — the same failure mode as the stale runbook secrets
v2 had to repair. What is documented instead is what unified-cd genuinely owns:

- The **Vault policy** the controller needs: `update` on
  `transit/encrypt/<key>` and `transit/decrypt/<key>`. Token auth additionally
  *wants* `read` on `auth/token/lookup-self`, so the renewal loop can learn the
  token's real TTL and renewability instead of the values a bare token carries
  (`TTL: 0, Renewable: false`). This capability is **optional**: a token
  minted without it still works — the controller logs a warning at startup and
  falls back to re-authenticating on its normal cadence instead of renewing,
  rather than failing to start.
- The **Transit key setup**: enabling the engine and creating the key.
- The **HA implications for unified-cd**: every replica points at the same Vault;
  because standbys forward, the address is the Vault service rather than a
  specific node; a Vault outage during a rollout crash-loops pods, which is the
  intended fail-closed behaviour; the DEK cache absorbs failover but does not
  replace HA; and **auto-unseal is a prerequisite for unattended HA**, since
  otherwise every node restart requires manual unsealing.

A `docs/high-availability.md` Vault section is added in the same shape as the
existing Postgres and S3 sections.

### 7. Development environment

An opt-in `docker-compose.openbao.yml`, following the existing
`docker-compose.sso.yml` precedent. It runs OpenBao in dev mode (in-memory),
enables Transit, creates the key, and points the controller at it.

The default stack is unchanged and keeps using `UNIFIED_DEV_MODE=1`.

Because the overlay is opt-in and OpenBao dev mode holds nothing real and dies
with the container, a fixed dev token is acceptable here — with a
`${VAR:-default}` escape hatch on every credential and an explicit "not for
production" banner. The repository has twice shipped fixed credentials that
escaped into deployments (`garageadmin`, `dev-token-change-me`); the escape hatch
and the banner are what distinguish this from those.

Kubernetes gets documentation rather than manifest changes: the ServiceAccount
token is projected by default, so configuring `kubernetes` auth is only a matter
of setting environment variables.

## Testing

**Unit — no container.** A fake Vault over `httptest`:

- Login for each method; renewal at half the remaining lease; re-login after a
  403; **no retry** on permission denied.
- `renewable: false` tokens are never sent to the renew endpoint.
- Provider mismatch on a non-`vault:` blob.
- Cache hit, miss, TTL expiry, LRU eviction, and zeroing of evicted DEKs.

**The renewal loop takes an injected clock.** It must not be tested with
`time.Sleep`. This repository has repeatedly shipped timing- and
race-dependent CI failures — the `fix/ci-test-races` series, and a Linux-only
timezone failure in PR #53 that Windows runs did not reproduce. A renewal loop
tested against wall-clock time would join that list.

**Integration — real OpenBao** via dockertest, following the existing shared-
Postgres pattern in `internal/store/testutil.go`. Gated behind a build tag or
environment variable so the default suite stays fast, and wired into CI as its
own job or an addition to the existing integration job.

**Cross-checks.** A local-mode round trip must still work unchanged, and a
`local:`-wrapped DEK opened under a Vault configuration must produce
`ErrProviderMismatch` rather than a generic failure.

## Consequences

- Operators gain a deployment where the controller never holds the KEK, and where
  key use is audited by Vault.
- Vault becomes a startup dependency for deployments that choose it: the
  controller will not start without it. This is deliberate.
- A single-machine operator is unaffected unless they opt in; the local key file
  remains fully supported.
- Adding AppRole, or a cloud-native KMS provider, later requires no configuration
  redesign.
