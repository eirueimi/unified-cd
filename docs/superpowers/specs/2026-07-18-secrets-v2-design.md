# Design: secrets v2 — versioned envelope format, AAD binding, and file/KMS key supply

**Date:** 2026-07-18
**Status:** Approved (pending implementation plan)
**Base commit:** `fda4bf3` — all file:line citations below were verified against
this revision.

## Problem

Three defects in the current secrets subsystem, all reachable in a default
deployment:

**1. The KEK can end up in the same database as the data it protects.** KEK
resolution (`cmd/controller/main.go:249-264`) is: config file → env var →
persisted DB value → *generate one and persist it to the DB*. In the default
configuration (nothing set), the controller generates a KEK and writes it in
plaintext to `controller_settings.controller_key_hex`
(`internal/store/postgres.go:1900-1907`, column at
`internal/store/migrations/001_init.up.sql:49`). The wrapped DEKs and ciphertext
live in the same database (`secrets` table,
`001_init.up.sql:247-256`). A database dump therefore contains both the
ciphertext and the key that decrypts it, and envelope encryption provides no
protection at all against the threat it exists to address.

**2. Ciphertext is not bound to the secret it belongs to.** `aesGCMEncrypt`
passes `nil` as the AES-GCM additional authenticated data
(`internal/secrets/keymanager.go:69`), and `Encrypt`/`Decrypt`
(`internal/secrets/crypto.go:13`, `:37`) take no identity parameter. Nothing ties
a ciphertext to its `(name, scope, scope_ref)`. An attacker or a bug with write
access to the `secrets` table can swap one secret's ciphertext for another's and
the swap decrypts cleanly — a job asking for `staging-token` can be served the
bytes of `prod-token`.

**3. OIDC refresh tokens are stored in plaintext.** The `sessions` table stores
`token_hash` (hashed, correct) alongside `refresh_token text` — in the clear
(`001_init.up.sql:263-272`). PATs are hashed
(`001_init.up.sql:159-165`); this column is the outlier.

Two structural problems compound these:

- **The ciphertext format carries no version byte.** Blobs are
  `nonce ‖ ciphertext` with nothing identifying the scheme, so any future change
  to the algorithm, the AAD contents, or the key-wrapping provider is another
  breaking change.
- **There is no key rotation.** No re-wrap path exists for either the KEK or the
  DEKs.

## Goal

Close all three defects in a single change, and spend the one breaking change we
are permitted on making future changes *non*-breaking: introduce a versioned
ciphertext format, bind ciphertext to its identity with AAD, remove the DB as a
key store, and settle the key-supply configuration surface for both local keys
and external KMS.

## Non-goals

- **Implementing a KMS backend.** This design settles the `UNIFIED_KMS_URI`
  configuration surface, validates it, and errors clearly on unsupported
  schemes. The Vault/OpenBao Transit `KeyManager` implementation is a separate
  change.
- **Implementing key rotation.** The version byte and provider-tagged wrapped
  DEKs are the groundwork; the rewrap command is future work.
- **Data migration.** There are no deployments whose secrets must be preserved
  (confirmed with the user). Existing encrypted data is not readable by v2 and
  is not migrated. Operators recreate their database and re-enter secrets.

## Context and constraints

- **Breaking changes are permitted, and existing data may be discarded.** This
  is what makes it possible to change the ciphertext format and drop columns
  without a migration path.
- **Small and single-machine deployments are in scope.** This rules out making
  an external KMS mandatory: requiring an operator to run and unseal
  Vault/OpenBao in order to run a CI tool is too heavy, and pushes users toward
  running a KMS badly (dev mode in production), which is worse than a
  well-handled local key. A local key remains a first-class, explicitly
  configured option; KMS is recommended and opt-in.
- **The `KeyManager` interface is already the correct seam**
  (`internal/secrets/keymanager.go:14`) — two methods, `EncryptKey` /
  `DecryptKey`, with `LocalKeyManager` as the only implementation. Nothing about
  the envelope layer needs to change to add a KMS provider later.

## Design

### 1. Key supply

The key may be supplied exactly one way, plus a dev escape hatch:

| Variable | Purpose |
|---|---|
| `UNIFIED_CONTROLLER_KEY_FILE` | Local key mode. Path to a file containing 64 hex characters. |
| `UNIFIED_KMS_URI` | KMS mode, e.g. `hashivault://unified-cd-kek`. |
| `UNIFIED_DEV_MODE` | When `1`, and neither of the above is set, generate an ephemeral in-memory key. |

**Removed** (breaking):

- env `UNIFIED_CONTROLLER_KEY` (`internal/config/controller.go:160`)
- config-file field `controllerKey` (`internal/config/controller.go:103`)
- `EnsureControllerKey` (`internal/store/store.go:455`,
  `internal/store/postgres.go:1893-1907`)
- column `controller_settings.controller_key_hex`
  (`internal/store/migrations/001_init.up.sql:49`)

A file is the only supported way to supply a local key because env vars leak
into `docker inspect`, process listings, crash dumps, and child processes, and
because a file is the idiom that Docker secrets and Kubernetes Secret mounts
already produce. The codebase has precedent: the k8s agent reads its token from
a mounted file (`internal/k8sagent/config.go:15`).

**Resolution order at startup:**

1. Both `UNIFIED_KMS_URI` and `UNIFIED_CONTROLLER_KEY_FILE` set → fatal error.
   Ambiguity about which key is in effect is not tolerable for a key store.
2. `UNIFIED_KMS_URI` set → parse and validate the URI. Because no KMS provider
   is implemented in this change (see Non-goals), a syntactically valid URI
   produces a fatal error stating that the named provider is not implemented in
   this build; a malformed URI or unknown scheme produces a fatal error naming
   the schemes the configuration surface accepts. Setting `UNIFIED_KMS_URI`
   therefore cannot start the controller yet — it exists so the configuration
   shape is settled and so the follow-up change adds a provider without
   redefining configuration.
3. `UNIFIED_CONTROLLER_KEY_FILE` set → read, trim surrounding whitespace,
   validate 64 hex characters, warn if the file is group- or world-readable
   (skipped on Windows), construct `LocalKeyManager`.
4. `UNIFIED_DEV_MODE=1` → generate an ephemeral key, and log a warning stating
   explicitly that secrets will not be decryptable after a restart.
5. Otherwise → fatal error instructing the operator to run
   `unified-cd keygen --out <path>` and set `UNIFIED_CONTROLLER_KEY_FILE`.

Whitespace is trimmed because editors and `echo` append newlines. There is no
fixed development key anywhere in the repository: the repo already demonstrates
how such values escape into real deployments (`garageadmin` /
`garageadmin12345`, `dev-token-change-me`), and the ephemeral dev key avoids
creating another one.

### 2. `keygen` CLI command

New subcommand:

```
unified-cd keygen --out /etc/unified-cd/kek
```

It writes the key file itself with mode `0600` rather than printing to stdout for
shell redirection. `unified-cd keygen > file` would create the file under the
caller's umask — commonly `0644` — leaving the key world-readable. Writing the
file directly removes that footgun. Printing to stdout remains available when
`--out` is omitted, for operators piping into their own secret tooling.

### 3. Ciphertext format

```
ciphertext    : 0x02 ‖ nonce(12) ‖ AES-256-GCM(dek, plaintext, aad = Binding)
encrypted_dek : 0x02 ‖ <opaque bytes from the KeyManager>
```

The leading byte is the **envelope format version**. `0x02` denotes v2; v1 blobs
(no version byte) are not readable by design.

The version lives inside the blob rather than in a separate column so it cannot
desynchronise from the data it describes. Rotation tooling can scan and decode
the header; if filtering by key version later becomes a performance concern, a
`kek_id` column can be added **without a breaking change**, which is precisely
what the version byte buys.

The two existing columns (`encrypted_dek`, `ciphertext`) are retained. Merging
them into a single self-describing blob was considered and rejected: it requires
a schema change and discards a working separation for no benefit here.

**Wrapped DEKs are self-describing per provider.** Each `KeyManager` tags its own
output — `LocalKeyManager` prefixes `local:`; Vault Transit already returns
`vault:v1:…`. Without this, opening local-wrapped data while configured for KMS
produces an opaque GCM authentication failure. With it, the error states that the
DEK was wrapped by `local` while the configured provider is `hashivault`. This
also lays the groundwork for a future local→KMS migration.

### 4. AAD binding

`Binding` is constructed only through typed constructors, so an empty or
malformed AAD cannot be built by accident:

```go
func SecretBinding(name, scope, scopeRef string) Binding
func SessionRefreshBinding(sessionID string) Binding
```

The canonical encoding is **length-prefixed**:

```
kind ‖ uint32(len(f1)) ‖ f1 ‖ uint32(len(f2)) ‖ f2 ‖ …
```

Naive concatenation would collide: `name="a", scope="bc"` and
`name="ab", scope="c"` produce identical bytes, which would permit exactly the
ciphertext substitution the AAD exists to prevent. Length prefixing is not
optional.

**API:**

```go
func Encrypt(ctx, km, plaintext []byte, b Binding) (encryptedDEK, ciphertext []byte, err error)
func Decrypt(ctx, km, encryptedDEK, ciphertext []byte, b Binding) ([]byte, error)
```

### 5. `sessions.refresh_token`

- Remove: `refresh_token text`
- Add: `refresh_token_dek bytea`, `refresh_token_ct bytea`
- Binding: `SessionRefreshBinding(sessionID)`

The same envelope scheme is used as for secrets, rather than a second scheme.
The extra KMS round-trip this implies is acceptable: session *validation* uses
`token_hash`, so the refresh token is decrypted only when refreshing an OIDC
token, which is rare.

Refresh tokens must be **encrypted, not hashed** — unlike PATs and session
tokens, the original value is replayed to the identity provider and must be
recoverable.

### 6. Missing or undecryptable secrets must fail the run

Secrets are silently dropped in **two** places today:

1. **Controller side** — `handleFetchSecrets` skips any secret it cannot load and
   omits it from the response (`internal/controller/api_secrets.go:114-118`):

   ```go
   stored, err := s.store.GetSecret(r.Context(), name, "global", "")
   if err != nil {
       // Skip if not found.
       continue
   }
   ```

2. **Agent side** — a failed fetch is non-fatal; the agent logs a warning and
   proceeds with an empty map (`internal/agent/orchestrator.go:162-165`).

Either path yields a step such as `curl -H "Authorization: Bearer $TOKEN"`
running with an empty token: the run then fails confusingly, or succeeds having
done the wrong thing.

Both change to **failing the run**, with the reason surfaced in the run log. The
controller returns an explicit error identifying the missing or undecryptable
secret by name rather than omitting it; the agent treats a fetch or decrypt
failure as fatal for the run.

This is included here rather than deferred because it is the same defect in a
different layer. Adding AAD makes tampering *detectable*; leaving the detection
swallowed by a `continue` and a warning would make the detection pointless.

### 7. Error handling

Startup errors are fatal and state the next action: which variable to set, which
schemes are supported, the expected key length against the actual, and the exact
`keygen` invocation.

Runtime decrypt failures are distinguished into three cases, because they mean
different things to an operator:

| Case | Meaning |
|---|---|
| Provider mismatch | Data was wrapped by a different `KeyManager` than the one configured |
| Unknown version byte | Data written by a newer build than this one |
| **AAD mismatch** | **Security-relevant**: ciphertext substitution, tampering, or corruption |

An AAD mismatch is logged distinctly rather than folded in with generic decrypt
errors. Log output carries identifiers only — never the secret value or the AAD
contents.

## Schema change

A new paired migration, `015_secrets_v2.up.sql` / `.down.sql`:

- Drop `controller_settings.controller_key_hex`
- Drop `sessions.refresh_token`; add `sessions.refresh_token_dek bytea` and
  `sessions.refresh_token_ct bytea`

Editing `001_init.up.sql` in place was considered first, reasoning that a change
with no upgrade path does not need a migration. That reasoning was withdrawn on
inspecting the repository: migrations run to `014`, and every one carries a
matching `.down.sql`. The project maintains a disciplined incremental chain;
rewriting its first link would break that convention, force every developer to
drop their database, and leave `002`–`014` applying on top of an altered `001`.
Following the existing chain costs nothing here.

Rows already in `secrets` keep their columns but were written under v1 and
cannot be decrypted by v2, so operators re-enter their secrets. Dropping the
refresh-token column invalidates existing sessions and users log in again —
the intended handling for tokens that were stored in the clear.

## Testing

**Unit — no container.** A fake in-memory `KeyManager` is used. It is defined in
test code only and is not constructible from any configuration value, so it
cannot become an accidental production code path.

- `Binding` canonical encoding **collision test** (`"a"|"bc"` vs `"ab"|"c"`) — written RED first
- Round-trip encrypt/decrypt with a matching `Binding`
- **Decrypt with a different `Binding` must fail** — the central property of this design
- Unknown version byte, and provider mismatch, each produce their distinct error
- Key resolution: each source; both-set is an error; unset is an error; dev mode yields an ephemeral key; trailing newline tolerated; wrong length rejected; permission warning emitted

**Integration — dockertest Postgres.**

- Secret round-trip through the store
- **The plaintext refresh token does not appear in any `sessions` column** — written RED first; this is the test that demonstrates defect 3 is fixed

**Repository hygiene.**

- A test asserting the removed identifiers (`UNIFIED_CONTROLLER_KEY`,
  `controllerKey`, `controller_key_hex`) appear nowhere in the repository.
  Without it, stale references to a deleted key source will survive and mislead
  operators.

**CI.** Run `TZ=UTC go test` on Linux before pushing. The `internal/controller`
package has already produced a Linux-only, timezone-dependent failure that
Windows runs did not reproduce (PR #53).

## Documentation

Removing a documented environment variable makes every reference to it wrong, so
documentation is part of this change, not a follow-up.

| File | Change |
|---|---|
| `README.md:41` | Quickstart: `keygen --out` + `_FILE` |
| `docs/getting-started.md:76` | Same |
| `docs/configuration.md:54,91,476` | Variable table, config sample, `openssl rand -hex 32` examples |
| `docs/high-availability.md:202-226,462` | **Rewrite** — see below |
| `manifests/README.md:36` | Remove "auto-generates and persists a key to the DB" |

Config and sample surfaces: `.env.example:19`,
`examples/config/controller.yaml:14`, `docker-compose.yaml:78`,
`deployments/docker/docker-compose.yaml:81`,
`manifests/base/controller/secret.yaml:10`, `manifests/core-install.yaml:162`,
`manifests/install.yaml:175`, `manifests/install/secret-patch.yaml:10`,
`test/ha/docker-compose.hardfail.yaml:34`, `test/ha/docker-compose.ha.yaml:22`.

### HA semantics change

`docs/high-availability.md:202-226` currently documents that omitting
`UNIFIED_CONTROLLER_KEY` is safe in HA **because all replicas share a database**
and therefore converge on the same auto-persisted key. Removing DB key storage
**removes that guarantee**. Every replica must now be given the same key file, or
the same `UNIFIED_KMS_URI`.

This is an operational change, not merely a documentation correction. KMS mode
resolves it naturally — every replica points at the same Vault — so the rewritten
guide should route HA operators toward KMS.

## Consequences

- Operators must generate and distribute a key before the controller will start.
  This is the intended outcome: the previous behaviour started successfully while
  providing no real protection.
- HA deployments must distribute the key themselves (see above).
- Existing encrypted data becomes unreadable. Migration `015` applies to an
  existing database, but the secrets it contains must be re-entered and users
  must log in again.
- Future changes to the algorithm, AAD contents, or key provider can ship without
  another breaking change.
