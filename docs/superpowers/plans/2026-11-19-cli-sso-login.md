# CLI SSO Login Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let `unified-cli login` work with IdPs that need a client secret (Google) without handing that secret to clients, make CLI tokens refresh themselves, add `login --browser`, and add `logout`.

**Architecture:**
- The controller gains stateless relay endpoints under `/api/v1/auth/cli/` that add the CLI client's secret server-side for the device flow, refresh and revocation.
- For browser login, a CLI-flavoured OIDC state goes through the existing web login and callback and ends in a PKCE-bound one-time code. The code is stored envelope-encrypted, with migration 032.
- The CLI stores the refresh token and refreshes inside the single `resolve()` choke point, under a lock file.

**Tech Stack:** Go, `golang.org/x/oauth2`, `github.com/coreos/go-oidc/v3`, pgx/PostgreSQL, cobra.

**Spec:** `docs/superpowers/specs/2026-11-19-cli-sso-login-design.md`. It is binding, including its "Designer's calls" section.

## Global Constraints

- **Repo rules.** English only and no PII (AGENTS.md). Commit with `git -c user.name=eirueimi -c user.email="128969149+eirueimi@users.noreply.github.com" commit`. Every message ends with:
  ```
  Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01G3NWwwXnjYtSfzJjZYTrnV
  ```
- **The device client secret stays on the controller.** `deviceClientSecret` never appears in any HTTP response body, header, log line or error string. Neither do tokens, device codes or refresh tokens.
- **The relay calls only the controller's own discovered endpoints.** These are the issuer's device authorization, token and revocation endpoints, obtained through `s.oidcProvider(ctx, r.Host)`, so `IssuerInternal` rewriting applies. No URL is ever taken from the request.
- **The bearer audience is an allow-list.** Accept `deviceClientId` (when set) and `clientId`. Never use `SkipClientIDCheck`.
- **Expiry means the id_token's `exp`.** It is never the OAuth access-token expiry.
- **The loopback redirect must be exact.**
  - The scheme is `http`.
  - The host is exactly `127.0.0.1` or `[::1]`.
  - An explicit port is required, with no userinfo, no query and no fragment.
  - The path is exactly `/callback`.
- **One-time codes:**
  - 32 random bytes, base64url;
  - stored as SHA-256 hex;
  - TTL 2 minutes;
  - single use, deleted on any redeem attempt, including a wrong verifier;
  - S256 PKCE.
- **Exact user-facing strings** (from the spec):
  - `token refresh failed: <reason>; run "unified-cli login" again`
  - `the identity provider issued no refresh token; you will need to log in again when the token expires`
  - `browser login requires the controller key manager` (HTTP 503)
  - `this server does not support browser login for the CLI; upgrade the controller or use the device flow`
- **Compatibility.**
  - With no new setting, behaviour is unchanged, apart from the CLI refreshing when a refresh token exists.
  - An old controller (no `deviceRelay` in `oidc-config`, 404 on `/cli/*`) makes the new CLI fall back.
- **Refresh trigger and scope.** The CLI refreshes when the stored id_token expires within 60 s. It never refreshes a token that came from `--token` or `UNIFIED_TOKEN`.
- **Config file safety.** The config file is written atomically (temp file, then rename) with mode `0600`. The lock file is `<config>.lock`.

## Review Focus

1. **Two CLI commands refreshing at once.** An IdP that rotates refresh tokens must not end with a stored refresh token that has already been used. Task 4 adds a two-goroutine test against a rotating fake IdP.
2. **The IdP returns `invalid_grant` on refresh** (revoked or expired). The command still runs with the old token and prints exactly one warning, and the config is not corrupted. Covered in Task 4.
3. **Loopback redirect lookalikes.** Each of these must be refused (Task 3 table test):
   - `http://127.0.0.1.evil.com:1234/callback`
   - `http://localhost:1234/callback`
   - `https://127.0.0.1:1234/callback`
   - `http://127.0.0.1/callback` (no port)
   - `http://user@127.0.0.1:1/callback`
   - `http://127.0.0.1:1/callback?x=1`
4. **An old controller.** `oidc-config` has no `deviceRelay`, and `/api/v1/auth/cli/browser/start` returns 404. `login` uses today's direct device flow, and `login --browser` prints the upgrade message (Task 5).
5. **The relay under an IdP error that carries the secret in its body or URL.** The error string returned to the client is the OAuth `error` and `error_description` only, never the raw upstream body (Task 2).

---

### Task 1: Controller settings, oidc-config fields, bearer audience allow-list

**Files:**
- Modify:
  - `internal/config/controller.go` (`ControllerOIDCConfig`, env loading near line 316, file merge near line 414, validation)
  - `internal/controller/server.go` (OIDC config struct near line 141)
  - `internal/controller/auth_oidc.go` (`oidcProvider` and `AuthCodeURL` options, `bearerAudience`, `verifyOIDCBearer`, `verifyBearerClaims`)
  - `internal/controller/api_tokens.go:344` (`handleOIDCConfig`)
- Test: the existing tests next to each file, i.e. `internal/config/controller_test.go` and the `auth_oidc` and `api_tokens` tests in `internal/controller/`.
- Docs: `docs/reference/configuration.md` (both settings, with env names).

**Interfaces:**
- Produces:
  - `ControllerOIDCConfig.DeviceClientSecret string` (yaml `deviceClientSecret`, env `UNIFIED_OIDC_DEVICE_CLIENT_SECRET`).
  - `ControllerOIDCConfig.AuthParams map[string]string` (yaml `authParams`, config file only).
  - The same two fields on the controller's OIDC config struct.
  - `func (s *Server) bearerAudiences() []string`, which returns `[deviceClientId?, clientId]` with duplicates and empties removed.
  - `func (s *Server) authCodeOptions() []oauth2.AuthCodeOption`, built from `AuthParams`.
  - New `oidc-config` fields: `deviceRelay` (bool) and `revocationEndpoint` (string, omitted when the issuer has none; read from the provider's discovery claims).

- [ ] **Step 1: Write failing tests**
  - **Config:**
    - `deviceClientSecret` is loaded from YAML and from env, with env precedence matching `clientSecret`.
    - `deviceClientSecret` without `deviceClientId` fails with an error mentioning `deviceClientId`.
    - `authParams` containing any of `client_id, redirect_uri, response_type, scope, state, code_challenge, code_challenge_method, nonce` fails, naming the key.
  - **oidc-config:**
    - `deviceRelay` is true only when the secret is set.
    - The response body never contains the secret value. Assert with `strings.Contains` on the raw body.
    - `revocationEndpoint` is present when the fake issuer's discovery has one.
  - **Bearer:**
    - An id_token with `aud=clientId` is accepted when `deviceClientId` differs.
    - An id_token with `aud=other` is rejected.
  - **AuthCodeURL:** the web login redirect URL carries `access_type=offline` when `authParams: {access_type: offline}`.
- [ ] **Step 2: Run them and see them fail.** Expected: FAIL.

  ```bash
  go test ./internal/config/ ./internal/controller/ -run 'OIDC|Bearer|AuthParams|DeviceClientSecret'
  ```
- [ ] **Step 3: Implement.**
  - Multi-audience verification: verify with `oidc.Config{SkipClientIDCheck: false}` per audience, or check `idToken.Audience` against `bearerAudiences()` after verifying with one config. Pick one that keeps signature and expiry checks.
  - Apply `authCodeOptions()` in `handleOIDCLogin`.
- [ ] **Step 4: Run Step 2 again, plus `go vet ./...`.** Expected: PASS.
- [ ] **Step 5: Update `docs/reference/configuration.md`.**
- [ ] **Step 6: Commit** with the message `feat(controller): device client secret, authParams and multi-audience CLI bearer`.

### Task 2: Relay endpoints (device start/token, refresh, revoke)

**Files:**
- Create: `internal/controller/auth_cli_relay.go` and `internal/controller/auth_cli_relay_test.go`
- Modify:
  - `internal/api/types.go` (new types)
  - `internal/controller/server.go:646-656` (routes)
  - `internal/controller/authz_routes.go:179-183` (`unauthenticated(...)` entries)
  - `internal/controller/audit_routes_test.go:20` (exempt entries with reasons)

**Interfaces:**
- Consumes: Task 1's `DeviceClientSecret` and `bearerAudiences()`.
- Produces, in `internal/api`:
  - `CLIDeviceStartResponse{DeviceCode, UserCode, VerificationURI, VerificationURIComplete string; ExpiresIn, Interval int}`, with JSON tags `deviceCode, userCode, verificationUri, verificationUriComplete, expiresIn, interval`.
  - `CLIDeviceTokenRequest{DeviceCode string}` (`deviceCode`).
  - `CLIRefreshRequest{RefreshToken, Client string}` (`refreshToken`, `client`, where `client` is `"device"` or `"browser"`).
  - `CLITokenResponse{IDToken, RefreshToken string; Expiry time.Time}` (`idToken`, `refreshToken`, `expiry`).
  - `CLIOAuthError{Error, ErrorDescription string}` (`error`, `errorDescription`).
- Produces, routes (all `POST`, unauthenticated, each with its own limiter):

  | Route | Status codes | Limiter |
  |---|---|---|
  | `/api/v1/auth/cli/device/start` | 200; 404 when `deviceRelay` is off | `newRateLimiter(now, time.Second, 20)` |
  | `/api/v1/auth/cli/device/token` | 200; 400 with `CLIOAuthError` (`authorization_pending`, `slow_down`, `expired_token`, `access_denied`) | `newRateLimiter(now, 100*time.Millisecond, 50)` |
  | `/api/v1/auth/cli/refresh` | 200; 400 with `CLIOAuthError` | `newRateLimiter(now, 200*time.Millisecond, 50)` |
  | `/api/v1/auth/cli/revoke` | 204; 404 when there is no revocation endpoint | `newRateLimiter(now, time.Second, 20)` |

  - An exhausted limiter returns 429.
  - `refresh` and `revoke` with `client=device` use `deviceClientId` and `deviceClientSecret`. With `client=browser` they use `clientId` and `clientSecret`.
  - Each successful response has its returned id_token verified against `bearerAudiences()`, and `Expiry` set from its `exp`.
  - A refresh response without an `id_token` returns 400 `{"error":"invalid_grant","errorDescription":"identity provider returned no id_token on refresh"}`.
  - A response with no rotated refresh token echoes the request's refresh token.

- [ ] **Step 1: Write failing tests** against an `httptest` fake IdP that serves discovery, device, token and revocation endpoints:
  - A start call reaches the device endpoint with the device client ID, and the token call sends `client_secret`.
  - `authorization_pending` is passed through as 400 `{"error":"authorization_pending"}`.
  - The upstream error body contains the secret. The response contains only `error` and `errorDescription`, and `strings.Contains(body, secret)` is false (Review Focus 5).
  - A refresh with `client=browser` uses the web client credentials.
  - A refresh with no `id_token` returns `invalid_grant` with the exact description.
  - A non-rotating refresh echoes the refresh token.
  - `Expiry` equals the id_token's `exp`, not `expires_in`.
  - `device/start` returns 404 when the secret is unset.
  - `revoke` returns 404 without a revocation endpoint, and 204 with one.
  - The 21st rapid `device/start` from one IP returns 429.
  - A log capture during a successful token call contains neither the secret nor the tokens.
- [ ] **Step 2: Run and see them fail.** Expected: FAIL.

  ```bash
  go test ./internal/controller/ -run CLIRelay
  ```
- [ ] **Step 3: Implement.** Use `oauth2.Config` with `AuthStyleInParams` (or the provider's detected style) and `DeviceAuth` / `DeviceAccessToken` or a single token POST. The handler must make exactly one upstream token call per request: no polling loop on the server.
- [ ] **Step 4: Run Step 2 again, plus `go test ./internal/controller/ -run 'Audit|Authz'`.** Expected: PASS.
- [ ] **Step 5: Commit** with the message `feat(controller): CLI token relay for device flow, refresh and revocation`.

### Task 3: Controller browser login for the CLI (start, callback branch, exchange)

**Files:**
- Create:
  - `internal/store/migrations/032_cli_login_codes.up.sql`
  - `internal/store/migrations/032_cli_login_codes.down.sql`
  - `internal/controller/auth_cli_browser.go`
  - `internal/controller/auth_cli_browser_test.go`
- Modify:
  - `internal/store/verify.go` (the sentinel entry for 032)
  - `internal/store/store.go` and `internal/store/postgres.go` (OIDC state, code storage)
  - `internal/secrets/binding.go` (new binding)
  - `internal/controller/auth_oidc.go` (callback branch; admission extracted from `completeOIDCLogin`)
  - routes and authz entries as in Task 2
  - `audit_routes_test.go`
- Test: `internal/store/` (code storage), `internal/controller/auth_cli_browser_test.go`

**Interfaces:**
- Consumes:
  - Task 1's `authCodeOptions()` and `bearerAudiences()`.
  - Task 2's `api.CLITokenResponse` and `api.CLIOAuthError`.
- Produces:
  - **Migration 032.**
    - `oidc_states` gains `cli_redirect text NOT NULL DEFAULT ''` and `cli_challenge text NOT NULL DEFAULT ''`.
    - New table `cli_login_codes`, with columns:
      - `id uuid PK DEFAULT gen_random_uuid()`
      - `code_hash text UNIQUE NOT NULL`
      - `challenge text NOT NULL`
      - `tokens_dek bytea NOT NULL`
      - `tokens_ct bytea NOT NULL`
      - `expires_at timestamptz NOT NULL`
      - `created_at timestamptz NOT NULL DEFAULT now()`
  - **Store.**
    - `CreateCLIOIDCState(ctx, state, cliRedirect, cliChallenge string, expiresAt time.Time) (*OIDCState, error)`.
    - `OIDCState` gains `CLIRedirect` and `CLIChallenge`, and `GetAndDeleteOIDCState` returns them.
    - `CreateCLILoginCode(ctx, codeHash, challenge string, dek, ct []byte, expiresAt time.Time) (id string, err error)`. It also deletes expired rows.
    - `ConsumeCLILoginCode(ctx, codeHash string) (*CLILoginCode, error)`. It deletes the row and returns it, or returns nil when the row is missing or expired.
    - `type CLILoginCode struct{ ID, Challenge string; DEK, CT []byte }`.
  - **Secrets:** `secrets.CLILoginCodeBinding(id string) Binding` (kind `cli_login_code`).
  - **Controller:**
    - `func (s *Server) admitOIDCClaims(ctx context.Context, claims map[string]any) (status int, msg string)`. It is extracted from `completeOIDCLogin` (the PAT owner snapshot plus `identityGrants` admission) and reused by both paths.
    - `GET /api/v1/auth/cli/browser/start?redirect=&challenge=`:
      - 302 to the IdP on success;
      - 400 for a bad redirect or a missing challenge;
      - 404 when there is no `clientSecret`;
      - 503 `browser login requires the controller key manager` when `s.km == nil`.
    - `POST /api/v1/auth/cli/browser/exchange` with body `{code, codeVerifier}`:
      - 200 `api.CLITokenResponse` on success;
      - 400 `{"error":"invalid_grant"}` for unknown, expired or reused codes and for a wrong verifier.
      - Limiter: `newRateLimiter(now, time.Second, 20)`.
    - The encrypted payload is JSON `{"idToken":..., "refreshToken":...}`.
    - The callback redirects to `<cli_redirect>?code=<base64url code>`.

- [ ] **Step 1: Write failing tests.**
  - **Redirect validation:** a table test of the redirects listed in Review Focus 3 (all 400), plus `http://127.0.0.1:5555/callback` and `http://[::1]:5555/callback` (both accepted).
  - **Full flow against a fake IdP:**
    - `start` returns 302 to the IdP with `authParams`.
    - The callback with that state returns 302 to the loopback with `code`.
    - `exchange` with the right verifier returns 200 with `idToken`.
    - A second `exchange` with the same code returns 400 `invalid_grant`.
  - **Wrong verifier:** returns 400, and a following correct verifier also returns 400 (the row was deleted).
  - **Expiry:** a code redeemed after 2 minutes (inject the clock or set `expires_at` in the past) returns 400.
  - **Admission:** a user that `identityGrants` would deny gets 403 `forbidden: no role assigned` at the callback, and no code row is created.
  - **No key manager:** `km == nil` makes `start` return 503 with the exact string.
  - **The web login is unchanged:** a non-CLI state still creates a session cookie.
  - **Store:**
    - Create, then consume, returns the row once. A second consume returns nil.
    - An expired row returns nil.
    - Migration 032 applies and rolls back.
- [ ] **Step 2: Run and see them fail.** Expected: FAIL.

  ```bash
  go test ./internal/store/ -run 'CLILogin|OIDCState|Migration'
  go test ./internal/controller/ -run CLIBrowser
  ```
- [ ] **Step 3: Implement.** Compare the verifier with `subtle.ConstantTimeCompare` on base64url(SHA-256(verifier)) against the stored challenge.
- [ ] **Step 4: Run Step 2 again, plus `go test ./internal/controller/ -run 'OIDC|Audit|Authz'`.** Expected: PASS.
- [ ] **Step 5: Commit** with the message `feat(controller): browser login for the CLI via PKCE-bound one-time codes`.

### Task 4: CLI token storage and automatic refresh

**Files:**
- Create:
  - `internal/cli/filelock.go`
  - `internal/cli/filelock_test.go`
  - `internal/cli/oidc_refresh.go`
  - `internal/cli/oidc_refresh_test.go`
- Modify:
  - `internal/cli/config.go` (`Config` fields, atomic `SaveConfig`)
  - `internal/cli/root.go:30-50` (`resolveWith` calls refresh)
  - `internal/cli/login.go` (the device flow stores the refresh token and expiry, and uses the relay when `deviceRelay`)

**Interfaces:**
- Consumes: Task 2's routes and `api` types. The `oidc-config` fields `deviceRelay`, `deviceClientId`, `tokenEndpoint` and `revocationEndpoint`.
- Produces:
  - **`Config` fields:**
    - `RefreshToken string` (`refreshToken,omitempty`)
    - `TokenExpiry time.Time` (`tokenExpiry,omitempty`)
    - `LoginClient string` (`loginClient,omitempty`: `device` or `browser`)
    - `LoginRelay bool` (`loginRelay,omitempty`)
  - **`func lockFile(path string) (unlock func(), err error)`.**
    - It creates `path` with `O_CREATE|O_EXCL`.
    - It retries every 50 ms for up to 10 s, and removes a lock older than 30 s.
    - It uses no new dependency.
  - **`func refreshIfNeeded(ctx context.Context, path string, c Config, now time.Time) (Config, error)`.**
    - It returns `c` unchanged unless `RefreshToken != ""` and `TokenExpiry` is before `now + 60s`.
    - Otherwise it locks, re-reads the file, and refreshes if the token is still stale:
      - **direct:** the `tokenEndpoint` from `oidc-config` (or discovery), with `deviceClientId`, as a public client;
      - **relay:** `/api/v1/auth/cli/refresh`, when `LoginClient == "browser"` or `LoginRelay`.
    - It then saves and unlocks.
  - **`func idTokenExpiry(raw string) (time.Time, error)`.** It parses `exp` without verifying, for scheduling only.
  - **`func saveLogin(path string, c Config, tok api.CLITokenResponse, client string, relay bool) error`.** It is shared with Task 5.

- [ ] **Step 1: Write failing tests**, each with an `httptest` controller and IdP and a temp config path:
  - **Not expired:** a token that is not expired is not refreshed (zero IdP calls).
  - **Direct refresh:** an expired direct device token is refreshed through the IdP token endpoint without `client_secret`, and the new token, refresh token and expiry are saved.
  - **Relay refresh:**
    - An expired `LoginRelay` token is refreshed through `/api/v1/auth/cli/refresh` with `client=device`.
    - An expired `LoginClient=browser` token uses `client=browser`.
  - **Overrides:** `--token` and `UNIFIED_TOKEN` skip refresh even when the file holds a stale refresh token.
  - **`invalid_grant`:** the old token is returned, stderr has exactly `token refresh failed: invalid_grant; run "unified-cli login" again`, and the file is unchanged and still parses (Review Focus 2).
  - **Concurrent refresh:** two goroutines call `refreshIfNeeded` on the same file against a rotating fake IdP that rejects a reused refresh token. Exactly one IdP refresh call is made, and both get the new token (Review Focus 1).
  - **Saved file:** `SaveConfig` leaves mode `0600` (skip the mode assert on Windows) and no `.tmp` file.
  - **Device login:**
    - With `deviceRelay: true`, login uses `/cli/device/start` and `/cli/device/token` and stores `LoginRelay=true`.
    - With an old `oidc-config` (no `deviceRelay`), login uses the direct device flow (Review Focus 4, first half).
    - A token response with no refresh token prints the exact no-refresh-token warning.
- [ ] **Step 2: Run and see them fail.** Expected: FAIL.

  ```bash
  go test ./internal/cli/ -run 'Refresh|Lock|Login|SaveConfig'
  ```
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run Step 2 again, plus `go test ./internal/cli/` and `go vet ./...`.** Expected: PASS.
- [ ] **Step 5: Commit** with the message `feat(cli): store refresh tokens and refresh before each command`.

### Task 5: `login --browser` and `logout`

**Files:**
- Create: `internal/cli/login_browser.go`, `internal/cli/logout.go` and their tests
- Modify:
  - `internal/cli/login.go` (the `--browser` flag)
  - `internal/cli/root.go` (register `newLogoutCmd()`)

**Interfaces:**
- Consumes: Task 3's routes, and Task 4's `saveLogin`, `lockFile` and `Config` fields.
- Produces:
  - **`login --browser`:**
    - It listens on `127.0.0.1:0` with path `/callback`, and makes a 32-byte verifier with its S256 challenge.
    - It opens `<server>/api/v1/auth/cli/browser/start?redirect=...&challenge=...` with the OS browser opener (`rundll32 url.dll,FileProtocolHandler` on Windows, `open` on macOS, `xdg-open` otherwise), and always prints the URL.
    - It waits up to 5 minutes, exchanges the code, and saves with `LoginClient=browser`.
    - The loopback page reads `Login complete. You can close this tab.`
  - **`--browser` errors:**
    - With `--issuer` or `--client-id`, it is rejected: `--browser cannot be combined with --issuer or --client-id`.
    - When `browserSSOEnabled` is false, it fails with `browser login is not configured on this server`.
    - When `start` returns 404, it prints the exact upgrade message.
  - **`logout`:**
    - It loads the config under the lock.
    - When `RefreshToken != ""` and `revocationEndpoint` is set, it revokes:
      - directly for direct device logins;
      - otherwise through `/api/v1/auth/cli/revoke`.
    - On revoke failure it prints `warning: could not revoke the refresh token: <reason>`.
    - It clears `Token`, `RefreshToken`, `TokenExpiry`, `LoginClient` and `LoginRelay`, keeps `Server` and `Headers`, and prints `Logged out.`

- [ ] **Step 1: Write failing tests.**
  - **Browser flow:** drive it end to end with a fake controller that redirects to the loopback URL it receives (inject the browser opener as a func that performs the GET), and assert the saved config.
  - **Flag combination:** `--browser --issuer x` returns the exact error.
  - **Old controller:** a 404 from `start` gives the upgrade message (Review Focus 4, second half).
  - **No browser SSO:** `browserSSOEnabled: false` gives the exact message.
  - **Timeout:** injectable for tests (e.g. 100 ms); it returns an error mentioning `timed out`.
  - **`logout`:**
    - With a revocation endpoint, it revokes and clears.
    - When revocation returns 500, it warns and still clears.
    - With a PAT-only config, it clears `Token` only and makes no network call.
- [ ] **Step 2: Run and see them fail.** Expected: FAIL.

  ```bash
  go test ./internal/cli/ -run 'Browser|Logout'
  ```
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run `go test ./internal/cli/`, then `go build ./... && go vet ./...`.** Expected: PASS.
- [ ] **Step 5: Commit** with the message `feat(cli): login --browser and logout`.

### Task 6: Documentation

**Files:**
- Modify:
  - `docs/operator-manual/identity-providers.md`
    - Google section: TV client plus `deviceClientSecret`, `authParams: {access_type: offline, prompt: consent}`, and an unverified-assumptions note.
    - Okta, Entra and Keycloak: add `offline_access` to `scopes`.
    - Table near lines 28-31: Google CLI login is no longer "Dex only"; groups still are.
  - `docs/operator-manual/authentication.md`
    - CLI token lifetime and refresh.
    - `--browser`, `logout`, and the relay.
    - `trustedProxies` for the relay rate limits.
    - Remove "Automatic refresh via refresh token is not currently supported" (around line 297) and fix line 339.
  - `docs/reference/cli.md`: `login --browser`, `logout`, and the token-expiry text around line 965.
  - `docs/troubleshooting/` (the auth page that exists; `grep -rln "unified-cli login" docs/troubleshooting`): symptoms with the exact strings from Global Constraints, the 400 `invalid_grant` on exchange, and a refused loopback redirect.
  - `docs/getting-started/quickstart.md`, if it describes CLI login.
  - `README.md`, if its feature list mentions CLI login.
- Check: `grep -rn "not currently supported\|Dex only\|re-run .*login\|~24" docs/ README.md`. Every stale hit outside `docs/superpowers/` is fixed.

- [ ] **Step 1: Update the docs above.** Every config key, env var, flag, endpoint and message string must match Tasks 1-5 exactly.
- [ ] **Step 2: Build the docs.** Expected: success with no warnings.

  ```bash
  python -m venv .venv-docs
  .venv-docs/Scripts/pip install -r docs/requirements.txt   # or requirements file the repo uses
  .venv-docs/Scripts/mkdocs build --strict
  ```

  Every new page is in the `mkdocs.yml` nav. Links to `examples/` are absolute GitHub URLs.
- [ ] **Step 3: Commit** with the message `docs: CLI SSO relay, refresh, browser login and logout`.
