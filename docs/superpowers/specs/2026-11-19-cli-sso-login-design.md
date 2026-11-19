# CLI SSO login: secrets stay on the controller, refresh, browser login — design

Status: agreed in conversation (2026-11-19); written spec awaiting approval

## Why

Using `unified-cli login` against Google Workspace without Dex is impossible today, and
every other IdP makes users log in again once a day.

- **No client secret is ever sent.** The CLI only speaks one OAuth shape: a public client
  running the device flow (RFC 8628).
  - Google's device flow uses a "TVs and limited-input devices" client that requires its
    client secret at the token endpoint.
  - So the only way to use the CLI with Google is to put Dex in front.
- **Nothing is refreshed.** The CLI saves the id_token only.
  - The user must run `login` again when it expires: about 24 h on Dex, and 1 h on
    Google.

The user wants the CLI to cover the parts of OAuth it does not implement yet. These are:

- sending a client secret;
- refreshing tokens;
- a browser login;
- revoking tokens on logout.

The device flow stays the default.

## Goals

1. **Client secrets never leave the controller.**
   - A new optional controller setting holds the CLI client's secret: `oidc.deviceClientSecret`,
     or the env var `UNIFIED_OIDC_DEVICE_CLIENT_SECRET`.
   - When it is set, the controller **relays** the CLI's device-flow token requests to the
     IdP and adds the secret itself.
   - No endpoint ever returns the secret.
2. **The CLI refreshes automatically.**
   - `login` stores the refresh token when the IdP issues one.
   - Before every command, the CLI checks the saved id_token. If it expires within 60
     seconds, the CLI refreshes it first, then runs the command with the new token.
3. **Browser login with `unified-cli login --browser`.**
   - The CLI opens the browser on the controller.
   - The controller runs its existing browser SSO with the existing web client.
   - The controller hands the result back to the CLI through a loopback redirect, using a
     one-time code bound by PKCE.
   - No new IdP client registration is needed.
4. **`unified-cli logout`**
   - It deletes the saved token and refresh token.
   - It asks the IdP to revoke the refresh token when the IdP advertises a
     `revocation_endpoint` (RFC 7009).
5. **Defaults stay the same.**
   - `login` without flags still runs the device flow.
   - With no new setting, every current deployment behaves exactly as today, except that
     the CLI now refreshes when the IdP issues refresh tokens.

## Non-goals

- Groups for Google Workspace without Dex.
- Changing how the controller authenticates CLI requests. It still verifies an id_token
  bearer, and it still does no per-request lookup.
- Storing CLI refresh tokens on the controller. The controller only relays them.
- OS keychain storage for the CLI config. It stays a file with mode `0600`.

## Anti-goals

- **A secret delivered to clients or exposed by any endpoint.** This includes
  `oidc-config`, error bodies and logs.
- **A relay that becomes an open proxy.** The relay endpoints talk only to the configured
  issuer's own endpoints, with the configured client IDs. They are rate-limited, and they
  never log tokens, device codes or refresh tokens.
- **A loopback redirect to anything but loopback.** The redirect target must be
  `http://127.0.0.1:<port>/...` or `http://[::1]:<port>/...`. Anything else is refused.
- **A one-time code that works twice, works after its short TTL, or works without the
  matching PKCE verifier.**
- **Breaking an existing CLI or controller combination.**
  - An old CLI against a new controller keeps working: the direct device flow is
    unchanged when no secret is set.
  - A new CLI against an old controller keeps working: it falls back to today's behaviour
    when `oidc-config` lacks the new fields.

## Decided design

### Controller configuration

- **New: `oidc.deviceClientSecret`** (YAML) / `UNIFIED_OIDC_DEVICE_CLIENT_SECRET` (env).
  - It follows the same precedence and loading as `clientSecret`.
  - It is optional.
  - Setting it without `deviceClientId` is a startup error.
- **New: `oidc.authParams`**
  - A map of extra query parameters for the browser authorization request, used by both
    the web login and `--browser`.
  - It exists because Google issues a refresh token to a web client only with
    `access_type=offline` (and `prompt=consent` for repeat logins). Without that, Google
    browser sessions and `--browser` logins get no refresh token.
  - The parameters `client_id`, `redirect_uri`, `response_type`, `scope`, `state`,
    `code_challenge`, `code_challenge_method` and `nonce` are reserved. Setting any of
    them is a startup error.
- **Documented, not new:** `oidc.scopes` already lets operators add `offline_access`.
  Okta, Entra ID and Keycloak issue refresh tokens only for that scope.

### `GET /api/v1/auth/oidc-config` (unauthenticated, additive fields)

| New field | Type | Meaning |
|---|---|---|
| `deviceRelay` | bool | True when `deviceClientSecret` is set. The CLI must use the relay endpoints for the device flow and for refreshing device-flow tokens. |
| `revocationEndpoint` | string | The issuer's `revocation_endpoint` from discovery, when it has one. |

The existing `browserSSOEnabled` field gates `--browser`; see "Designer's calls" below.

The secret is never included.

### Relay endpoints

All are unauthenticated and `POST` with JSON bodies. Each one is rate-limited per client
IP with the existing limiter (`newRateLimiter`); the builder picks the rate and the burst.
They return the IdP's OAuth error codes unchanged, for example `authorization_pending`,
`slow_down`, `expired_token` and `invalid_grant`, so the CLI's polling logic stays standard.

| Endpoint | Does |
|---|---|
| `/api/v1/auth/cli/device/start` | Calls the issuer's device authorization endpoint with `deviceClientId` (and the secret where the IdP accepts it there) and the configured scopes, and returns the IdP response. Used only when `deviceRelay`. |
| `/api/v1/auth/cli/device/token` | Body `{deviceCode}`. Calls the token endpoint once with `grant_type=urn:ietf:params:oauth:grant-type:device_code`, `deviceClientId` and `deviceClientSecret`, and returns `{idToken, refreshToken?, expiry}` or the OAuth error. |
| `/api/v1/auth/cli/browser/exchange` | Body `{code, codeVerifier}`. Redeems a browser-login one-time code; see below. |
| `/api/v1/auth/cli/refresh` | Body `{refreshToken, client}`, where `client` is `device` or `browser`. Refreshes with the matching client credentials: `deviceClientId` and `deviceClientSecret`, or `clientId` and `clientSecret`. Returns `{idToken, refreshToken, expiry}`. If the IdP did not rotate the refresh token, the old one is echoed back. A refresh response without an `id_token` is an error. |
| `/api/v1/auth/cli/revoke` | Body `{refreshToken, client}`. Calls the `revocation_endpoint` with the matching client credentials. If the issuer has none, it returns 404. |

The relay is stateless. Nothing is stored except the browser-login one-time codes.

### Browser login (`--browser`)

1. **The CLI starts listening.** It listens on `127.0.0.1:0`, with the path
   `/callback`, and creates a PKCE verifier and its S256 challenge.
2. **The CLI opens the controller's login page.** It opens
   `GET /api/v1/auth/cli/browser/start?redirect=http://127.0.0.1:<port>/callback&challenge=<S256>`
   in the default browser, and prints the URL as well.
3. **The controller checks the request and starts its normal login.**
   - The redirect must be a loopback address with an explicit port and the path
     `/callback`, and the challenge must be present; otherwise the request is refused.
   - The controller saves an OIDC state that records "CLI login", the redirect and the
     challenge. It extends the existing `oidc_states` row, or uses a sibling table; the
     builder decides.
   - It then sends the browser to the IdP exactly as the web login does.
4. **The callback becomes a one-time code.** The existing `/api/v1/auth/oidc-callback`
   handles a CLI state differently from a web login:
   - It runs the same token exchange and id_token verification.
   - It does not create a session or set a cookie.
   - It creates a **one-time code**: 32 random bytes, stored hashed, with a 2-minute TTL.
   - It stores the id_token and the refresh token with that code, encrypted with the
     existing envelope encryption (`secrets.Encrypt` with a binding to the code row, as
     sessions do).
   - It redirects the browser to the loopback `redirect` with `?code=<one-time code>`.
5. **The CLI redeems the code.**
   - The CLI calls `/api/v1/auth/cli/browser/exchange` with the code and the verifier.
   - The controller checks S256(verifier) against the stored challenge, deletes the row
     in the same transaction (single use), and returns the tokens.
   - A wrong verifier also deletes the row.
6. **The controller accepts the new audience.** The bearer verifier accepts id_tokens
   whose audience is either `deviceClientId` or `clientId`. Today it accepts only the
   former, when set.
7. **Housekeeping and feedback.**
   - Expired one-time codes are purged by the existing expired-row cleanup, or a new one
     if none fits.
   - The loopback page tells the user they can close the tab.
   - The CLI times out after 5 minutes.

A new migration (`032_...`) adds the one-time-code storage and any `oidc_states` columns.

### CLI storage and refresh

- **New fields in `Config`, all `omitempty` and written with mode `0600`:**
  - `refreshToken`
  - `tokenExpiry` (RFC 3339)
  - `loginClient` (`device` or `browser`)
  - `loginRelay` (bool)
- **When the CLI refreshes.** `resolve()` in `internal/cli/root.go` is the single place
  that loads the config for every command. Before it returns, it refreshes when all of
  these hold:
  - the token came from the config file, not `--token` or `UNIFIED_TOKEN`;
  - a refresh token is stored;
  - the token expires within 60 s.
- **Which path it uses.**
  - Direct to the IdP's token endpoint, as a public client, when `loginClient=device` and
    no relay is in use.
  - Otherwise through `/api/v1/auth/cli/refresh`.
- **Concurrent commands.**
  - The CLI takes an exclusive lock file next to the config, `config.yaml.lock`, before it
    reads, refreshes and writes.
  - After taking the lock it re-reads the file. If another process already refreshed, it
    uses that token.
  - The config file is written atomically: a temporary file, then a rename.
  - The builder picks the lock mechanism. It must work on Linux, macOS and Windows. Avoid
    a new dependency if a small portable implementation suffices.
- **When a refresh fails.** The CLI keeps the old token and prints one warning to stderr:
  `token refresh failed: <reason>; run "unified-cli login" again`. The command still runs,
  and the server's 401 is the final word.
- **Without a refresh token** the behaviour is today's.

### `unified-cli logout`

- **Revoke.** When a refresh token is stored and a revocation endpoint is advertised, the
  CLI revokes it:
  - directly, for the direct device path;
  - otherwise through `/api/v1/auth/cli/revoke`.
- **Clear.** It always clears `token`, `refreshToken`, `tokenExpiry`, `loginClient` and
  `loginRelay` from the config. It keeps `server` and `headers`.
- **When revocation fails or is unavailable,** it prints a warning and still clears the
  local tokens.
- **Without SSO tokens** (for example a PAT login), it clears the token only.

### Login command surface

- `unified-cli login` runs the device flow (the default).
  - With `deviceRelay`, it goes through the relay.
  - Otherwise it goes direct to the IdP, as today.
- `unified-cli login --browser` runs the browser login. It is refused with a clear message
  when the server reports `browserSSOEnabled: false`.
- **Manual flags.** The existing `--issuer` and `--client-id` keep working for the direct
  device flow only. With `--browser` they are rejected.

### Documentation

- **Operator guides.**
  - `docs/operator-manual/identity-providers.md`:
    - Google section: the TV client plus `deviceClientSecret` replaces "use Dex for the
      CLI"; document `authParams: {access_type: offline, prompt: consent}`.
    - Okta, Entra and Keycloak: add `offline_access` to `scopes`.
    - The Dex section is unchanged.
  - `docs/operator-manual/authentication.md`: CLI token lifetime, refresh, `--browser`,
    `logout`, and remove the "refresh not supported" note.
- **CLI reference.** `docs/reference/cli.md`: `login --browser` and `logout`, plus the
  updated token-expiry wording.
- **Configuration reference.** `docs/reference/configuration.md`: the two new settings.
- **Troubleshooting.** New symptoms with the exact error strings: refresh failed, loopback
  refused, one-time code expired or reused.
- **Release note.** This is not a breaking change, so there is no migration guide.

## Designer's calls (settled after the builder check)

- **Bearer audience.**
  - The verifier checks the id_token audience against an explicit allow-list:
    `deviceClientId` (when set) and `clientId`.
  - It never uses `SkipClientIDCheck`. An id_token with any other audience is rejected.
  - Accepting `clientId` is intended. Only the controller can redeem web-client codes,
    because only it holds `clientSecret`, so web-audience id_tokens reach a CLI only
    through `--browser`.
- **Expiry means the id_token's `exp`.**
  - The relay responses and the CLI's `tokenExpiry` use the `exp` claim of the returned
    id_token. They do not use the OAuth access-token expiry.
  - The direct device path does the same.
- **The browser login admits users like the web login.**
  - The CLI branch of the callback runs the same admission check as `completeOIDCLogin`
    (`identityGrants`), without creating a session.
  - A user with no role gets the same 403 "no role assigned" page, and no code is issued.
- **The browser login needs a key manager.** Browser login requires `s.km`, because the
  code row is envelope-encrypted. When the key manager is missing, `/cli/browser/start`
  returns 503 with `browser login requires the controller key manager`.
- **No new `browserLogin` field.**
  - The CLI gates `--browser` on the existing `browserSSOEnabled`, which is true when
    `ClientSecret` is set. That field already exists on older controllers.
  - An older controller lacks `/cli/browser/start`. The CLI turns a 404 there into
    `this server does not support browser login for the CLI; upgrade the controller or use the device flow`.
- **Rate limits.**
  - Each relay endpoint gets its own per-client-IP limiter, separate from the enrollment
    limiter, with defaults sized so that about 50 users behind one IP can log in and
    refresh at once.
  - The builder sets the exact numbers. Device-token polling needs at least one request
    per 5 s per user.
  - Docs note that behind a proxy, `trustedProxies` must be set, or all users share one
    bucket.
- **Relay destinations.**
  - The relay calls only the endpoints from the controller's own discovery of the
    configured issuer: device authorization, token and revocation. It never takes a URL
    from the request.
  - It uses the same HTTP client and context as `oidcProvider()`, so `IssuerInternal`
    host rewriting applies.
- **Login without a refresh token.** A `--browser` or device login that yields no refresh
  token succeeds and prints one warning, as follows.
  - Text: `the identity provider issued no refresh token; you will need to log in again when the token expires`.
  - For Google, the warning adds a hint to set `oidc.authParams`.
- **`authParams` also changes the web login.** It applies to the existing web login's
  `AuthCodeURL`. This is intended, because it fixes refresh for Google browser sessions.
  It is a new setting, so no existing deployment changes.
- **Refresh does not re-check anything.**
  - The relay returns the IdP's id_token unmodified and does not re-check admission on
    refresh.
  - The controller keeps checking each request against the id_token's claims, as it does
    for device-flow tokens today.
- **Migration number.** It is `032`. No other open branch claims it, and the builder
  re-checks at merge time.

## Left to the builder

- Rate-limit values, the one-time-code table shape, and whether the CLI state lives in
  `oidc_states` or in a sibling table.
- The lock-file implementation.
- **Tests.** At least:
  - the secret never appears in any response;
  - a non-loopback redirect is refused;
  - a missing or wrong PKCE verifier fails;
  - a one-time code is single-use and expires;
  - the relay passes OAuth errors through;
  - a refresh response with no `id_token` is an error;
  - the bearer audience accepts `clientId`;
  - concurrent refreshes from two processes leave one valid token;
  - `--token` and `UNIFIED_TOKEN` bypass refresh;
  - an old controller (no new `oidc-config` fields) makes the new CLI fall back;
  - `logout` clears the tokens when revocation fails.

## Unverified assumptions (flag in docs where they matter)

- Google's device-flow token endpoint returns a refresh token and an id_token for a
  limited-input client with the `openid email profile` scopes.
- Google returns a refresh token to a web client only with `access_type=offline`.
- Google, Okta, Entra ID and Dex return a new id_token on refresh.

These come from general knowledge; none has been tested against the real IdPs.
