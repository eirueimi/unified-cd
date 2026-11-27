# Internal Accounts Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the always-admin `UNIFIED_TOKEN` with internal accounts (human and service), a one-time initial admin token, an offline recovery command and bounded PAT lifetimes.

**Architecture:** A new `accounts` table owns every PAT and session. Humans are created on admitted SSO sign-in. Service accounts come from a new `serviceAccounts` config block whose grants are compiled at startup, like `oidc.permissions`. The built-in `admin` service account is reached only through the initial token exchange or the `unified-controller admin recover` command. The schema change is done in two migrations, 036 (add and backfill) and 037 (drop the old owner columns), so every task leaves the build green.

**Tech Stack:** Go 1.26 (chi, pgx, golang-migrate), PostgreSQL, Svelte web UI (vitest), Helm chart (tests in `internal/helmchart`), mkdocs.

**Spec:** `docs/superpowers/specs/2026-11-27-internal-accounts-design.md`. Read it before any task. Where this plan and the spec disagree, the spec wins; record the conflict in the task report.

## Global Constraints

- PAT lifetime: default `2160h` (90 days), maximum `8760h` (365 days), config `tokens.defaultLifetime` / `tokens.maxLifetime`; `tokens.maxLifetime: 0` allows non-expiring PATs. PATs of the built-in `admin` account are capped at `720h` (30 days) regardless.
- Recovery PAT: default TTL `1h`, maximum `24h`.
- Expiry warning window: 7 days; warning logged once a day.
- Built-in service account name: `admin`. Only `enabled` may be set for it; it holds the admin bundle globally.
- Reserved name prefixes for new PAT names and service-account names: `env:` and `system:`. Service-account names follow DNS-label rules (`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`).
- New permissions, appended at the end of the catalogue: `account.manage`, then `serviceaccount.token` (both in the admin bundle only).
- Initial token file name: `initial-admin-token`, mode `0600`. Env var to provide the value: `UNIFIED_INITIAL_ADMIN_TOKEN`. The log states the path, never the value.
- Initial token prefix `uci_` + 40 hex characters (same generator shape as `generatePAT`).
- Audit actor ids: `recovery` (recovery command), `bootstrap` (initial-token exchange). Service-account PAT rows also carry `details.account = "sa:<name>"`. `break-glass` is no longer produced.
- Controller `UNIFIED_TOKEN` / `--token` / `token:` and `UNIFIED_ALLOW_DEFAULT_TOKEN` / `--allow-default-token` / `allowDefaultToken:` are removed. A set `UNIFIED_TOKEN` is ignored with an error log that names `docs/operator-manual/migrations/2026-11-27-internal-accounts.md`. A leftover `token:` or `allowDefaultToken:` key in the config file must not abort startup: accept it and log the same error.
- The client-side CLI `UNIFIED_TOKEN` (the PAT the CLI sends) is unchanged.
- English only in the repo. Every user-visible string a test asserts is quoted in this plan; copy it verbatim.
- Commits: `git -c user.name=eirueimi -c user.email="128969149+eirueimi@users.noreply.github.com" commit`, ending with the two trailer lines `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>` and `Claude-Session: https://claude.ai/code/session_01G3NWwwXnjYtSfzJjZYTrnV`.
- Windows checkout: check gofmt on committed content with `git show HEAD:<file> | gofmt -l`. If `go generate` rewrites the shim binaries, revert them.

## Review Focus

1. A human account is disabled while it holds a live session cookie, a PAT and a CLI id_token. All three must be refused on the next request, not at the next sign-in. Test pinned in Task 3.
2. Two replicas start on a fresh database at the same time. Exactly one initial token hash is stored and the other replica does not overwrite it. Test pinned in Task 5.
3. The same initial token is exchanged twice, concurrently. Exactly one exchange succeeds; the other gets the same 401 as a wrong value. Test pinned in Task 5.
4. A service account is removed from the config while its PAT is in use. After a restart the PAT is refused; re-adding the entry makes the same PAT work again. Test pinned in Task 3.
5. A PAT tries to create a PAT for another account, for `admin`, or beyond its lifetime cap. All are refused. An `admin` PAT minting its own successor gets at most 720h. Test pinned in Task 4.

## Facts for implementers

- Store interface: `internal/store/store.go:283`. Implementation: `internal/store/postgres.go`. Test DB: `storetest.NewTestPostgres(t)`. Upgrade tests: `storetest.NewEmptyTestDatabase(t)` + `MigrateTo`; see `internal/store/postgres_matrix_authz_test.go`.
- Every migration needs a `schemaSentinels` entry in `internal/store/verify.go`, or `TestSchemaSentinelsCoverAllMigrations` fails.
- New routes need a `routeTable` row (`internal/controller/authz_routes.go`), an `auditActionTable` entry or an `auditExemptWriteRoutes` exemption (`internal/controller/audit.go`), and possibly an update to `TestRouteTableShape`.
- Central API test helper: `newTestServer` (`internal/controller/api_jobs_test.go:23`) seeds an ownerless admin PAT with the value `secret`. test/e2e uses `mustSeedBootstrapPAT` (`test/e2e/seed_helper_test.go:15`).
- `LoadController` uses `KnownFields(true)`, so new config blocks must be added to `ControllerConfig`. `ControllerEffective` returns before `authz.Compile` when no config file is given; service-account compilation must run in both paths.
- `flag.Parse` stops at the first non-flag argument, so the subcommand must be dispatched before flags are registered.

---

### Task 1: Permissions and config blocks

**Files:**
- Modify: `internal/authz/catalogue.go`, `internal/authz/catalogue_test.go`, `internal/authz/policy.go`
- Create: `internal/authz/serviceaccounts.go`, `internal/authz/serviceaccounts_test.go`
- Modify: `internal/config/controller.go`, `internal/config/config_test.go` (or a new `internal/config/accounts_test.go`)
- Modify: `web/src/lib/authz-cases.json` only if `internal/authz/grants_test.go` requires it

**Interfaces:**
- Produces:
  - `authz.AccountManage Permission = "account.manage"`, `authz.ServiceAccountToken Permission = "serviceaccount.token"` (catalogue indices 28 and 29).
  - `type ServiceAccountPermissions struct { Global []string; Folders []FolderGrant }`, `type FolderGrant struct { Folder string; Grants []string }`, with an `UnmarshalYAML` that accepts the same grant forms as one `users:` entry (scalar or list of role or permission names).
  - `func CompileServiceAccount(name string, p *ServiceAccountPermissions) (Grants, error)`. Errors are prefixed `serviceAccounts["<name>"].permissions`. A global-only permission on a folder gives the same "is global-only and cannot be granted on a folder" error as `oidc.permissions`. `by` is recorded as `sa:<name>`.
  - `const BuiltinAdminAccount = "admin"` (in `internal/authz` so config, store and controller share it).
  - `config.ServiceAccountConfig { Enabled *bool `yaml:"enabled"`; Permissions *authz.ServiceAccountPermissions `yaml:"permissions"` }`.
  - `ControllerConfig.ServiceAccounts map[string]ServiceAccountConfig `yaml:"serviceAccounts"``.
  - `ControllerConfig.Tokens TokensConfig `yaml:"tokens"`` with `DefaultLifetime, MaxLifetime *time.Duration`, plus accessors `TokenDefaultLifetime() time.Duration` (default 2160h) and `TokenMaxLifetime() time.Duration` (default 8760h, 0 = unlimited).
  - `ControllerConfig.UI UIConfig `yaml:"ui"`` with `AllowTokenEntry bool `yaml:"allowTokenEntry"``.
  - Compiled result `ControllerConfig.ServiceAccountGrants map[string]authz.Grants `yaml:"-"`` (no `admin` key) and `ControllerConfig.AdminAccountEnabled() bool` (default true).
  - Env overrides: `UNIFIED_TOKENS_DEFAULT_LIFETIME`, `UNIFIED_TOKENS_MAX_LIFETIME`, `UNIFIED_UI_ALLOW_TOKEN_ENTRY`.

- [ ] **Step 1: Write failing tests**
  - `TestCatalogueHas30PermissionsInOrder`: replaces the 28-permission test. `all[28] == AccountManage`, `all[29] == ServiceAccountToken`, and `Bundle("admin")` contains both while `developer` contains neither.
  - `TestCompileServiceAccount`:
    - `Global: ["viewer"]`, `Folders: [{team-a, [developer]}]` → `Can(JobRead, "x")`, `Can(RunTrigger, "team-a/job")`, `!Can(RunTrigger, "team-b/job")`.
    - Folder grant `[secret.write]` → error containing `is global-only and cannot be granted on a folder`.
    - Unknown name → error containing `unknown permission or role`.
  - `TestServiceAccountsConfig` (YAML through `LoadController` and `ControllerEffective`):
    - `ci-deploy` with permissions compiles into `ServiceAccountGrants["ci-deploy"]`.
    - `admin: {enabled: false}` → `AdminAccountEnabled() == false`.
    - `admin: {permissions: {global: [viewer]}}` → error `serviceAccounts["admin"]: only "enabled" may be set for the built-in admin account`.
    - Names `Bad_Name`, `env:x` and `system:x` → error `serviceAccounts["<name>"]: invalid name: must be a DNS label and must not start with "env:" or "system:"`.
    - Env-only configuration with no file (`UNIFIED_TOKENS_MAX_LIFETIME=0`) → `TokenMaxLifetime() == 0`.
    - `tokens.defaultLifetime` greater than a non-zero `maxLifetime` → error `tokens.defaultLifetime must not exceed tokens.maxLifetime`.
- [ ] **Step 2: Run them and verify they fail**: `go test ./internal/authz/ ./internal/config/`.
- [ ] **Step 3: Implement.** Reuse the existing grant decoding in `policy.go` (export a small helper if needed); do not duplicate validation rules. Compile service accounts in `ControllerEffective` on both the file and env-only paths.
- [ ] **Step 4: Run** `go test ./internal/authz/ ./internal/config/ ./internal/controller/ -run 'Catalogue|ServiceAccount|Config|Authz'`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(authz,config): account permissions and serviceAccounts, tokens, ui config`.

### Task 2: Accounts schema and store (migration 036)

**Files:**
- Create: `internal/store/migrations/036_accounts.up.sql`, `036_accounts.down.sql`, `internal/store/postgres_accounts.go`, `internal/store/postgres_accounts_test.go`, `internal/store/migration_036_test.go`
- Modify: `internal/store/store.go`, `internal/store/postgres.go` (PAT and session scan/insert), `internal/store/verify.go`

**Interfaces:**
- Consumes: `authz.BuiltinAdminAccount` (Task 1).
- Produces:
  - `type Account struct { ID, Kind, Name, OIDCSub, OIDCIssuer, Email string; Claims []string; DisabledAt *time.Time; CreatedAt time.Time; LastSeenAt *time.Time }`, with `const AccountKindHuman = "human"` and `AccountKindService = "service"`.
  - `UpsertHumanAccount(ctx, sub, issuer, email string, claims []string) (*Account, error)`: inserts, or updates email, claims and `last_seen_at`. Never clears `disabled_at`.
  - `GetAccount(ctx, id string) (*Account, error)` and `GetHumanAccountBySub(ctx, sub string) (*Account, error)`. Both return `nil, nil` when missing.
  - `SyncServiceAccounts(ctx, names []string) error`:
    - ensures a row for every name, plus `admin`;
    - clears `disabled_at` on the listed names;
    - sets `disabled_at` on service rows that are not listed, except `admin`.
  - `ListAccounts(ctx) ([]Account, error)` and `SetAccountDisabled(ctx, id string, disabled bool) error`.
  - `type NewPAT struct { Name, TokenHash, AccountID, Role string; Permissions, EnrollLabels []string; ExpiresAt *time.Time; CreatedBy string }`, plus `CreateAccountPAT(ctx, in NewPAT) (*PAT, error)`. The new `PAT.AccountID string` is filled by `scanPAT`.
  - `ListPATsByAccount(ctx, accountID string) ([]PAT, error)` and `DeleteAccountPAT(ctx, id, accountID string) (bool, error)`.
  - `Session.AccountID string`, and `CreateAccountSession(ctx, accountID, tokenHash, sub, email, role string, claims []string, refreshDEK, refreshCT []byte, expiresAt time.Time) (*Session, error)`.
  - Bootstrap state on the `controller_settings` id=1 row (new columns `initial_token_hash text`, `bootstrap_done_at timestamptz`, `admin_enabled_until timestamptz`; the migration inserts the row if missing):
    - `GetBootstrapState(ctx) (BootstrapState, error)`, where `BootstrapState{ Done bool; InitialTokenHash string; AdminEnabledUntil *time.Time }`;
    - `ClaimInitialToken(ctx, hash string) (stored bool, err error)`: sets the hash only when it is NULL and bootstrap is not done, in a single UPDATE … WHERE;
    - `ConsumeInitialToken(ctx, hash string) (ok bool, err error)`: a single UPDATE that matches the hash, then sets `bootstrap_done_at = now()` and the hash to NULL;
    - `SetAdminEnabledUntil(ctx, t time.Time) error`.
  - Old methods (`CreatePAT`, `CreateOwnedPAT`, `CreateEnrollPAT`, `UpdatePATOwnerSnapshot`, `*BootstrapPAT*`, `CreateSession`) stay until Task 9.
- Migration 036, in order:
  1. Create `accounts` (unique partial index on `oidc_sub` where kind = 'human'; unique partial index on `name` where kind = 'service').
  2. Insert the `admin` service row.
  3. Add nullable `pats.account_id` and `sessions.account_id`, both `uuid REFERENCES accounts(id) ON DELETE CASCADE`.
  4. Create one human account per distinct `pats.owner_sub` and `sessions.sub`. For email and claims, take the most recent session row, else the most recent PAT row. Then set `account_id`.
  5. For every PAT with no owner, insert an `audit_logs` row (actor `migration`, action `token.delete`, details `{"patId","name","role","createdBy","reason":"ownerless PAT removed by the internal-accounts migration"}`; verify the column list against migrations 001, 024, 027 and the one that adds `details`), then delete the PAT.
  6. Delete sessions with `claims IS NULL`.
  7. Add the `controller_settings` columns.
  The down migration reverses the schema only. Deleted rows are not restored; say so in a comment.

- [ ] **Step 1: Write failing tests**
  - `TestMigration036Backfill` (upgrade test from version 35), seeded with:
    - an owner PAT for sub `s1` and a session for `s1`;
    - a role-only admin PAT;
    - an ownerless enroll PAT;
    - an `env:UNIFIED_TOKEN` row;
    - a session with NULL claims for sub `s2`.

    After migrating, assert:
    - exactly one human account `s1`, with both its PAT and its session pointing at it;
    - the three ownerless PATs are gone, with three audit rows whose actor is `migration`;
    - the NULL-claims session is gone, and there is no account for `s2`;
    - the `admin` service account exists.
  - `TestSyncServiceAccounts`: sync `[a, b]`, then `[b]` → `a` disabled, `b` enabled, `admin` enabled. Sync `[a, b]` again → `a` re-enabled with the same id.
  - `TestInitialTokenClaimAndConsume`:
    - two concurrent `ClaimInitialToken` calls with different hashes → exactly one `stored == true`;
    - two concurrent `ConsumeInitialToken` calls with the right hash → exactly one `ok == true`;
    - consuming the wrong hash → `ok == false`;
    - after a successful consume, `Done == true` and `InitialTokenHash == ""`, and a further `ClaimInitialToken` returns `false`.
  - `TestUpsertHumanAccountKeepsDisabled`: disable, upsert again → still disabled; claims are updated.
- [ ] **Step 2: Run them and verify they fail**: `go test ./internal/store/ -run 'Migration036|ServiceAccounts|InitialToken|HumanAccount'`.
- [ ] **Step 3: Implement** the migration, the sentinel (`accounts` table, version 36) and the store methods.
- [ ] **Step 4: Run** `go test ./internal/store/`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(store): accounts table, account-owned PATs and sessions (migration 036)`.

### Task 3: Account-based authentication

**Files:**
- Modify:
  - `internal/controller/auth.go`, `auth_oidc.go`, `auth_cli_browser.go`, `rbac.go`, `server.go` (Config, NewServer), `audit.go`
  - `pat_warning.go`: delete the file and its call site.
  - `internal/controller/api_jobs_test.go`: the `newTestServer` helper and its variants in `api_secrets_test.go` and `api_webhooks_test.go`.
  - `test/e2e/seed_helper_test.go`
  - Every controller test that asserts break-glass or role-only behaviour. Rewrite each one for accounts or delete it; list the deleted tests in the report.
- Create: `internal/controller/auth_accounts_test.go`

**Interfaces:**
- Consumes: Task 1 config accessors, Task 2 store methods.
- Produces:
  - `Principal` gains `AccountID, AccountKind, AccountName string`. `BreakGlass` is removed.
  - `controller.Config` gains `ServiceAccountGrants map[string]authz.Grants`, `AdminEnabled bool`, `TokenDefaultLifetime, TokenMaxLifetime time.Duration` and `AllowTokenEntry bool`. `Token` is removed.
  - `(s *Server) accountGrants(a *store.Account) (authz.Grants, bool)`:
    - human → `identityGrants(a.Claims, a.Email, a.OIDCSub)`;
    - service `admin` → `authz.AllGrants("sa:admin")`, but only if `s.adminEnabled(ctx)`;
    - other service account → `ServiceAccountGrants[name]`;
    - disabled, or a service account missing from config → `false`.
  - `(s *Server) adminEnabled(ctx) bool` is `cfg.AdminEnabled || now < AdminEnabledUntil`. Cache the DB read for at most 10 seconds.
  - Error strings:
    - a disabled or unknown account → 403 `forbidden: account disabled`;
    - an account that has no grants → 403 `forbidden: no role`.
  - `AuditActorID`: `pat:<id>` for PATs, `oidc:<sub>` for sessions and id_tokens. When `AccountKind == service`, the audit middleware adds `details.account = "sa:<name>"`.
- Behaviour:
  - **PAT branch:** load the account and compute `accountGrants ∩ ceiling`. A PAT whose `AccountID` is empty is refused.
  - **id_token branch:** after admission, call `UpsertHumanAccount` at most once per minute per sub. Use an in-memory cache keyed by sub that stores the account and the time it was fetched. Refuse when the account is disabled; the disabled check uses the cached row, refreshed at least every minute.
  - **Session branch:** load the session's account and refuse it if disabled.
  - **`admitOIDCClaims`:** run the admission first. Only when admitted, call `UpsertHumanAccount`. If the account is disabled, return 403 `forbidden: account disabled`. Remove `UpdatePATOwnerSnapshot` from this path.
  - **`completeOIDCLogin`:** call `CreateAccountSession`.
  - **cmd/controller/main.go:** call `SyncServiceAccounts` after migrate, and pass the Task 1 config into `controller.Config`. Leave the `UNIFIED_TOKEN` startup logic for Task 5; it simply has no effect on authentication any more.

- [ ] **Step 1: Write failing tests** in `auth_accounts_test.go`:
  - `TestDisabledHumanRefusedEverywhere`: create a human account with a session cookie, a PAT and a stubbed id_token principal (follow the existing OIDC tests' verifier stub). `SetAccountDisabled(true)`. All three requests to `GET /api/v1/jobs` return 403 with body `forbidden: account disabled`. For the id_token, the test may advance the cache clock instead of sleeping.
  - `TestServiceAccountRemovedFromConfig`:
    - with `ServiceAccountGrants{"ci": viewer}` and a PAT for `ci`, `GET /api/v1/jobs` returns 200;
    - rebuild the server without `ci` and call `SyncServiceAccounts` without it → 403 `forbidden: account disabled`;
    - re-add `ci` and sync → the same PAT returns 200.
  - `TestAdminAccountDisabled`: an admin PAT returns 200 with `AdminEnabled: true` and 403 with `false`. With `false` but `AdminEnabledUntil = now+1h`, it returns 200.
  - `TestOwnerlessPATRefused`: a PAT row with NULL `account_id` (inserted with raw SQL) → 403.
  - `TestRefusedLoginWritesNoAccount`: a login whose claims are not admitted → 403 `forbidden: no role assigned`, and `ListAccounts` shows no human.
  - `TestServiceAccountAuditDetail`: a write made with a `ci` PAT produces an audit row with `details.account == "sa:ci"`.
- [ ] **Step 2: Run them and verify they fail**: `go test ./internal/controller/ -run 'Disabled|ServiceAccount|AdminAccount|OwnerlessPAT|RefusedLogin'`.
- [ ] **Step 3: Implement.** Rewrite `newTestServer` so it configures `ServiceAccountGrants{"test-admin": admin bundle}`, runs `SyncServiceAccounts([test-admin])`, and creates a `test-admin` PAT whose hash is `HashToken("secret")`. The existing tests keep sending `Bearer secret`. Do the same for `mustSeedBootstrapPAT`, renaming it `mustSeedAdminPAT`.
- [ ] **Step 4: Run** `go test ./internal/controller/ ./internal/store/ ./cmd/controller/` and `go vet ./...`. Expected: PASS. test/e2e must compile: `go vet -tags e2e ./test/e2e/` (check the build tag in the package first).
- [ ] **Step 5: Commit** `feat(controller): authenticate through accounts; remove break-glass`.

### Task 4: Token API on accounts, with lifetimes

**Files:**
- Modify: `internal/controller/api_tokens.go`, `internal/api/types.go`, `internal/controller/api_tokens*_test.go`, `internal/controller/audit_actor_test.go`

**Interfaces:**
- Consumes: Task 3 `Principal.Account*`, Task 2 `CreateAccountPAT`, `ListPATsByAccount`, `DeleteAccountPAT`.
- Produces:
  - `api.CreatePATRequest` gains `Account string json:"account,omitempty"` (a service-account name).
  - `api.PATMeta` gains `Account string json:"account"` (`sa:<name>` or the human's email, else sub) and keeps `ExpiresAt`.
- Rules:
  - **Target account.** With no `Account`, the PAT is created for the caller's own account. This works for every principal kind, including a PAT creating a PAT (CI rotation) and an `admin` PAT minting its successor.
  - **Ceilings.** The ceiling rules are today's owner-PAT rules: role XOR permissions, and `validateCeiling`. A PAT-from-PAT intersects its parent's ceiling.
  - **Creating for another service account.** `Account: "<sa>"` needs `token.manage` or `serviceaccount.token`. A named account that is unknown or disabled → 400 `unknown service account "<sa>"`. `Account: "admin"` → 400 `PATs for the built-in admin account can only be obtained through the initial token or the recovery command`. Naming a human account is not supported in v1.
  - **Lifetime.** An empty `expiresIn` takes `TokenDefaultLifetime`; when that is 0, the PAT has no expiry. An `expiresIn` above a non-zero `TokenMaxLifetime` → 400 `expiresIn exceeds tokens.maxLifetime (<max>)`. For the `admin` account, the lifetime is `min(requested or default, 720h)`, and a request above 720h → 400 `expiresIn exceeds the 720h limit for the built-in admin account`.
  - **Enroll-only PATs** (`permissions == [agent.enroll]`) are now owned by the target account, like any other PAT.
  - **List and delete.** A caller sees and deletes the PATs of its own account. With `token.manage`, a caller sees and deletes everyone's.
  - **Reserved names.** The `env:` and `system:` name prefixes are rejected with the existing message `token name "<name>" is reserved`.

- [ ] **Step 1: Write failing tests**
  - `TestTokenDefaultAndMaxLifetime`:
    - no `expiresIn` → `ExpiresAt` is within ±1 minute of now+2160h;
    - `expiresIn: 9000h` → 400 with the exact message, max `8760h0m0s` (use `Duration.String()`);
    - with `TokenMaxLifetime: 0` and no `expiresIn`, still the default 2160h; with `TokenDefaultLifetime: 0` too → `ExpiresAt == nil`.
  - `TestAdminSuccessorCapped`: an `admin` PAT creating a PAT without `expiresIn` → expiry of now+720h; with `expiresIn: 1000h` → 400.
  - `TestPATCannotTargetOtherAccounts`: a `ci` PAT (viewer plus `token.create`) with `Account: "deploy"` → 403 from the permission check (`serviceaccount.token` missing); with `Account: "admin"`, a `token.manage` caller → 400 with the exact admin message.
  - `TestServiceAccountTokenPermission`: a caller with only `serviceaccount.token` creates a PAT for `deploy`; `token list` as that caller does not show deploy's PATs unless it also holds `token.manage`.
  - Update the existing owner, enroll, audit and role tests to the account model. Behaviour that no longer exists (role-only creators) is deleted, with the reason given in the report.
- [ ] **Step 2: Run them and verify they fail.**
- [ ] **Step 3: Implement.** `CreatedBy` stays `creator.AuditActorID()`.
- [ ] **Step 4: Run** `go test ./internal/controller/ ./internal/api/ ./internal/cli/`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(controller): account-owned PATs with default and maximum lifetimes`.

### Task 5: Initial token, bootstrap endpoint, startup rule

**Files:**
- Create: `internal/controller/bootstrap.go`, `internal/controller/bootstrap_test.go`, `internal/cli/bootstrap.go`, `internal/cli/bootstrap_test.go`, `cmd/controller/initial_token.go`, `cmd/controller/initial_token_test.go`
- Modify:
  - `cmd/controller/main.go`: remove the `--token` and `--allow-default-token` flags, `CheckAdminToken`, the sync/delete block, `bootstrapTokenWarning` and its test.
  - `internal/config/controller.go`: remove `Token` and `AllowDefaultToken`, accept the legacy keys as described in Global Constraints.
  - `internal/config/defaulttoken.go`: delete it, together with its tests.
  - `internal/controller/authz_routes.go`, `audit.go`, `server.go`, `internal/cli/root.go`

**Interfaces:**
- Consumes: Task 2 `ClaimInitialToken`, `ConsumeInitialToken`, `GetBootstrapState`, `CreateAccountPAT`; the `admin` account.
- Produces:
  - `POST /api/v1/bootstrap`. It is registered as `unauthenticated(mPost, "/api/v1/bootstrap")` and added to `auditExemptWriteRoutes`, because the handler writes its own audit row.
    - Request: `{"initialToken": "...", "name": "optional"}`.
    - Success: 201 with `api.CreatePATResponse`, carrying a PAT of the `admin` account. The name defaults to `bootstrap`, `expiresIn` is 720h and `CreatedBy` is `bootstrap`.
    - Failure: 401 `invalid or already used initial token`, for every case.
    - Rate limit: 5 requests per minute per controller process (`rate.NewLimiter`). Over the limit → 429 `too many bootstrap attempts`.
    - The handler writes an audit row itself: actor `bootstrap`, ActorID `bootstrap`, action `bootstrap.exchange`, details `{"patId"}`.
  - `func prepareInitialToken(ctx context.Context, st store.Store, sso bool, provided string, dirs []string) (path string, err error)` in `cmd/controller/initial_token.go`.
    - **Value provided** (`UNIFIED_INITIAL_ADMIN_TOKEN`): `ClaimInitialToken(HashToken(provided))` whether or not SSO is configured. It writes no file. If a different hash is already stored, it logs a warning `UNIFIED_INITIAL_ADMIN_TOKEN is ignored: an initial admin token was already issued`.
    - **Otherwise**, when `!sso`, bootstrap is not done and no hash is stored: generate a token, `ClaimInitialToken`, and write it to the first writable directory in `dirs`. `dirs` is the config file directory, then the data dir. If the claim lost the race, no file is written; log `the initial admin token was generated by another controller replica`. On success, log `initial admin token written to <path>; exchange it with "unified-cli bootstrap"`.
    - **Bootstrap already done:** if an `initial-admin-token` file exists in any of `dirs`, delete it and log `removed the used initial admin token file <path>`. This covers spec 6.4 "the file is deleted if the controller can delete it". The bootstrap handler also tries to delete the file in the replica's own dirs after a successful exchange.
  - **No directory writable:** return an error. main exits with `cannot write the initial admin token: no writable config or data directory; set UNIFIED_INITIAL_ADMIN_TOKEN`.
  - **Startup rule (main):** if `!sso` and bootstrap is not done and `prepareInitialToken` failed → exit 1. Otherwise start.
    - A set controller `UNIFIED_TOKEN` (env, flag or file) → `slog.Error("UNIFIED_TOKEN is no longer used by the controller and is ignored; see docs/operator-manual/migrations/2026-11-27-internal-accounts.md")`.
    - When SSO is configured and `AdminAccountEnabled()`, log once `the built-in admin service account is enabled while SSO is configured; set serviceAccounts.admin.enabled: false once SSO works`.
  - CLI `unified-cli bootstrap [--initial-token <value> | --initial-token-file <path>] [--name <n>]`: needs `--server` (or its usual resolution), does not need a token, and prints only the PAT on stdout. When neither flag is given, it reads the value from `UNIFIED_INITIAL_ADMIN_TOKEN`.

- [ ] **Step 1: Write failing tests**
  - `TestBootstrapExchangeOnce`: claim a hash; 20 concurrent POSTs with the right value → exactly one 201 and 19 × 401 `invalid or already used initial token` (raise the limiter in the test through a server option). The returned PAT authenticates as `admin`, and `ExpiresAt` is within ±1 minute of now+720h.
  - `TestBootstrapWrongValue` → 401, same body. `TestBootstrapRateLimit`: the 6th request in a minute → 429.
  - `TestPrepareInitialTokenRace`: two concurrent `prepareInitialToken` calls on a fresh DB with two temp dirs → exactly one file exists, its mode is 0600 (skip the mode assertion on Windows), and the stored hash matches the file's contents.
  - `TestPrepareInitialTokenSkipsWithSSO`: with `sso == true` and nothing provided → no file and no hash.
  - `TestPrepareInitialTokenProvided`: with the value provided and `sso == true` → hash stored, no file.
  - `TestPrepareInitialTokenNoDir`: with only unwritable or empty dirs → error.
  - `TestPrepareInitialTokenRemovesUsedFile`: bootstrap is done and a leftover file exists → the file is deleted and no error is returned.
  - cmd/controller: `TestLegacyTokenKeysAccepted`: a config file with `token:` and `allowDefaultToken:` loads without error.
  - CLI: `TestBootstrapCommand` against an httptest server → prints only the token, and sends no Authorization header.
- [ ] **Step 2: Run them and verify they fail.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./cmd/controller/ ./internal/controller/ ./internal/config/ ./internal/cli/`. Expected: PASS.
- [ ] **Step 5: Commit** `feat: one-time initial admin token and bootstrap exchange; drop controller UNIFIED_TOKEN`.

### Task 6: `unified-controller admin recover`

**Files:**
- Create: `cmd/controller/recover.go`, `cmd/controller/recover_test.go`
- Modify: `cmd/controller/main.go` (subcommand dispatch, and extraction of config loading into `loadRuntimeConfig`)

**Interfaces:**
- Consumes: Task 2 `CreateAccountPAT`, `SetAdminEnabledUntil`, `InsertAuditLog`; Task 1 config.
- Produces:
  - `func loadRuntimeConfig(args []string, fs *flag.FlagSet) (*config.ControllerConfig, error)`: the config file, env and flags loading that `main` uses today, made reusable. The server path keeps its current flags and behaviour.
  - Dispatch: `os.Args[1] == "admin"` → `runAdmin(os.Args[2:])`, before any global flag registration. With no subcommand, the server starts as today.
  - `unified-controller admin recover [-f config] [--dsn ...] [--ttl 1h] [--enable-admin]`:
    - connects with the same database settings and runs migrations;
    - creates an `admin` PAT named `recovery-<UTC timestamp>` with `CreatedBy` `recovery`;
    - writes the audit row (actor `recovery`, ActorID `recovery`, action `admin.recover`, details `{"patId","ttl","enableAdmin"}`);
    - prints only the token on stdout, and a one-line note on stderr.
  - `--ttl` above 24h → exit 2, `--ttl must not exceed 24h`.
  - When the config disables `admin` and `--enable-admin` is not given → exit 1, `the built-in admin account is disabled in the configuration; pass --enable-admin`.
  - `--enable-admin` → `SetAdminEnabledUntil(now + ttl)`, and the PAT expiry equals that lease.
- [ ] **Step 1: Write failing tests**
  - `TestRecoverPrintsWorkingAdminPAT`: against a test DB, call `runAdmin` with a DSN and capture stdout. The token authenticates as `admin` in a `NewServer` built from the same DB. Expiry is now+1h ±1m. The audit row exists.
  - `TestRecoverTTLCap`: `--ttl 25h` → exit code 2 with the exact message.
  - `TestRecoverDisabledAdmin`: without `--enable-admin` → exit 1 with the exact message. With it → `GetBootstrapState().AdminEnabledUntil` equals the PAT's `ExpiresAt`.
  - `TestServerStartArgsUnchanged`: `loadRuntimeConfig` with today's typical flags (`-f`, `--dsn`, `--addr`) yields the same values as before the refactor.
- [ ] **Step 2: Run them and verify they fail.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./cmd/controller/` and `go build ./...`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(controller): admin recover subcommand`.

### Task 7: Accounts API, CLI and expiry warnings

**Files:**
- Create: `internal/controller/api_accounts.go`, `internal/controller/api_accounts_test.go`, `internal/cli/account.go`, `internal/cli/account_test.go`, `internal/controller/pat_expiry.go`, `internal/controller/pat_expiry_test.go`
- Modify: `internal/api/types.go`, `internal/controller/server.go`, `authz_routes.go`, `audit.go`, `internal/cli/root.go`, `internal/cli/token.go` (`--account` flag; `list` shows account and expiry), `internal/metrics/metrics.go`, `cmd/controller/main.go` (start the daily loop)

**Interfaces:**
- Produces:
  - `GET /api/v1/accounts` (`account.manage`) returns `[]api.Account{ID, Kind, Name, Email, Disabled bool, CreatedAt, LastSeenAt, PATCount int}`. Query `?kind=service|human`.
  - `POST /api/v1/accounts/{id}/disable` and `/enable` (`account.manage`, audited as `account.disable` / `account.enable`). On a service account → 400 `service accounts are enabled and disabled in serviceAccounts`.
  - CLI:
    - `unified-cli account list [--kind human|service]`, printing the columns ID, KIND, NAME, EMAIL, DISABLED, LAST SEEN;
    - `account disable <id>` and `account enable <id>`;
    - `serviceaccount list`, an alias for `account list --kind service`;
    - `token create --account <name>`;
    - `token list` adds the columns ACCOUNT and EXPIRES.
  - Gauge `unifiedcd_pats_expiring_soon` (help `Personal access tokens that expire within 7 days`), refreshed by the daily loop. Each day the loop logs `slog.Warn("personal access tokens expire within 7 days", "count", n, "ids", ids)` when n > 0. The loop runs once at startup and then every 24h, so it works without cross-replica coordination; every replica logs.
- [ ] **Step 1: Write failing tests**
  - `TestAccountsListAndDisable`: an admin lists the accounts (`admin`, `test-admin` and one human); disabling the human → its PAT gets 403; enabling it → 200. A `developer` caller → 403. Disabling `admin` → 400 with the exact message.
  - `TestPATExpiryWarning`: with PATs expiring in 3 days, in 10 days, and never → the gauge is 1, and the log contains the warning with count 1.
  - CLI tests for `account list`, `token create --account`, and the new `token list` columns (httptest).
- [ ] **Step 2: Run them and verify they fail.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./internal/controller/ ./internal/cli/ ./internal/metrics/`. Expected: PASS.
- [ ] **Step 5: Commit** `feat: accounts API and CLI, PAT expiry warning`.

### Task 8: Web UI and browser PAT policy

**Files:**
- Modify: `internal/controller/auth.go` (Sec-Fetch rule), `internal/controller/api_uiconfig.go`, `web/src/lib/api.js`, `web/src/components/AuthSetup.svelte` (+ `AuthSetup.test.js`), `web/src/routes/TokenList.svelte` (+ test), `web/src/App.svelte`, `web/src/lib/ui-actions.json` if `ui_actions_test.go` requires it
- Create: `web/src/routes/AccountList.svelte` and `web/src/routes/AccountList.test.js`

**Interfaces:**
- Produces:
  - **Server.** A PAT bearer on a request that carries `Sec-Fetch-Mode` → 401 `personal access tokens are not accepted from the browser while SSO is configured; sign in with SSO`. This applies only when OIDC is configured and `AllowTokenEntry` is false. Session cookies and id_tokens are unaffected.
  - `GET /api/v1/ui-config` adds `allowTokenEntry` (true when SSO is not configured, or when `ui.allowTokenEntry` is set).
  - **AuthSetup.** When `browserSSOEnabled && !allowTokenEntry`, the manual token form and the "or enter token manually" line are not rendered.
  - **TokenList.** Shows the Account and Expires columns. The create form gets an "Account" field, shown only to callers with `token.manage` or `serviceaccount.token`.
  - **AccountList** at `/accounts`, linked in the nav when `allows($grants, 'account.manage')`. It lists accounts and has Disable/Enable buttons for humans only.
- [ ] **Step 1: Write failing tests**
  - Go: `TestBrowserPATRefusedUnderSSO`. A PAT with `Sec-Fetch-Mode: cors` → 401 with the exact message. The same request without the header → 200. With `AllowTokenEntry: true` → 200. Without OIDC → 200.
  - vitest:
    - AuthSetup hides the form when ui-config says `allowTokenEntry: false` and SSO is enabled, and shows it otherwise;
    - TokenList renders the Account and Expires columns;
    - AccountList's disable button POSTs to `/api/v1/accounts/<id>/disable`.
- [ ] **Step 2: Run them and verify they fail.** Run `go test ./internal/controller/ -run Browser`, then, in `web/`, `npx vitest run`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the same commands, plus `npm run build` in `web/`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(web): accounts page; no manual token entry under SSO`.

### Task 9: Drop the old owner columns (migration 037) and dead code

**Files:**
- Create: `internal/store/migrations/037_accounts_cleanup.up.sql` and `.down.sql`
- Modify: `internal/store/verify.go`, `store.go`, `postgres.go` and tests; `internal/controller/auth.go` (remove `BootstrapPATName`); `internal/config/stale_refs_test.go`

**Interfaces:**
- Migration 037:
  - `pats.account_id` and `sessions.account_id` become NOT NULL.
  - Drop `pats.owner_sub`, `owner_email` and `owner_claims`, together with the index `pats_owner_sub_idx`.
  - Keep `sessions.sub`, `email` and `claims`: they are still used for refresh. Note this in the migration comment.
  - Update the sentinels: the v24 sentinel must not reference dropped columns. Follow the existing pattern for sentinels of later-dropped columns, or change it to an index-absent sentinel. Add a v37 sentinel.
- Remove from Store and Postgres: `CreatePAT`, `CreateOwnedPAT`, `CreateEnrollPAT`, `UpdatePATOwnerSnapshot`, `CountRoleOnlyPATsAboveViewer`, `UpsertBootstrapPAT`, `SyncBootstrapPAT`, `DeleteBootstrapPATByName`, `ListPATsByOwner`, `DeleteOwnedPAT`, `CreateSession`; and `PATOwner`, `NewOwnedPAT`, `NewEnrollPAT`, `PAT.Owner`. Rename `CreateAccountPAT` to `CreatePAT` and `CreateAccountSession` to `CreateSession`, and update every caller.
- `stale_refs_test.go`: extend `TestNoStaleControllerKeyReferences` so that `UNIFIED_ALLOW_DEFAULT_TOKEN`, `allowDefaultToken`, `BootstrapPATName` and `dev-token-change-me` fail the test outside `docs/superpowers/`, `docs/operator-manual/migrations/` and `CHANGELOG`-like files. The guard is written now; Tasks 10 and 11 make it pass. Until then, skip it with `t.Skip("enabled in Task 11")` and record the skip in the report.
- [ ] **Step 1: Write failing tests**
  - `TestMigration037`: after migrating, inserting a PAT with NULL `account_id` fails, and the column `owner_sub` does not exist.
  - Run `TestSchemaSentinelsCoverAllMigrations`.
- [ ] **Step 2: Run them and verify they fail.**
- [ ] **Step 3: Implement**, including the renames.
- [ ] **Step 4: Run** `go test ./...` (in short mode if the full run needs Docker; state which) and `go vet ./...`. Expected: PASS.
- [ ] **Step 5: Commit** `refactor(store): drop PAT owner columns and ownerless PAT code (migration 037)`.

### Task 10: Deployments, chart, CI and test harnesses

**Files:**
- Modify:
  - Chart: `charts/unified-cd/templates/controller/trial-secret.yaml`, `templates/controller/kek-job.yaml` (or a sibling job), `templates/controller/deployment.yaml`, `templates/NOTES.txt`, `templates/_helpers.tpl`, `values.yaml`, `values.schema.json`, `templates/controller/configmap.yaml` (template `serviceAccounts`, `tokens` and `ui` from values)
  - Chart tests: `internal/helmchart/*_test.go`
  - CI: `.github/workflows/ci.yml`
  - Compose: `deployments/docker/docker-compose.yaml`, `deployments/docker/.env.example`, root `docker-compose.yaml`, `.env.example`
  - test/ha: `docker-compose.ha.yaml`, `docker-compose.hardfail.yaml`, `ha_test.go`, `hardfailure_test.go`
  - test/edgecase: the `compose/` overrides and `tools/` scripts listed in the fact sheet
  - Reserved env names: `internal/agent/stepenv.go` and `internal/dsl/vars_parse.go` add `UNIFIED_INITIAL_ADMIN_TOKEN` and keep `UNIFIED_TOKEN`; update their tests.

**Interfaces:**
- **Chart.** The create-once Job (the same pattern as the KEK) also creates the Secret `unified-cd-initial-admin-token` with key `UNIFIED_INITIAL_ADMIN_TOKEN`, as a random value of 40 hex characters with the `uci_` prefix. The controller Deployment reads it with `optional: true`. This applies to both trial and production. `NOTES.txt` prints how to read the Secret and run `unified-cli bootstrap`.
- **Trial Secret.** It no longer has `UNIFIED_TOKEN` or `UNIFIED_ALLOW_DEFAULT_TOKEN`. The `_helpers.tpl` ingress guard keeps refusing ingress with trial credentials, but its message no longer mentions `dev-token-change-me`. New text: `ingress.enabled with the bundled trial credentials would expose the trial database password; use the production values`. Update the chart tests accordingly.
- **Compose.**
  - In production compose, `UNIFIED_INITIAL_ADMIN_TOKEN: ${UNIFIED_INITIAL_ADMIN_TOKEN:?set UNIFIED_INITIAL_ADMIN_TOKEN in .env (uci_ + openssl rand -hex 20)}` replaces `UNIFIED_TOKEN`.
  - In the root dev compose, a fixed `UNIFIED_INITIAL_ADMIN_TOKEN` default of `uci_dev0000000000000000000000000000000000000` (exactly 40 characters after `uci_`).
  - The `agent-enroll` one-shot bootstraps once and stores the PAT in a named volume, `/state/admin-token`. On later runs it reuses the file.
- **CI** (`ci.yml`, the kind job): read the Secret with `kubectl get secret unified-cd-initial-admin-token -o jsonpath=...`, run the bootstrap exchange with curl (`POST /api/v1/bootstrap`), and use the returned PAT where the job used `dev-token-change-me`.
- **test/ha and test/edgecase.**
  - Set `UNIFIED_INITIAL_ADMIN_TOKEN: uci_ha00000000000000000000000000000000000000`.
  - The Go tests exchange it once in `TestMain`, or with `sync.Once`, and use the PAT.
  - The shell tools call a shared `bootstrap_admin_token` function that caches the PAT in `${TMPDIR:-/tmp}/unified-ha-admin-token`.
  - `w7b-upgrade.yaml` keeps `UNIFIED_TOKEN` on the old-image services only, and adds `UNIFIED_INITIAL_ADMIN_TOKEN` to the new-image services. Update the scenario note to say that an upgraded non-SSO install re-bootstraps.
- [ ] **Step 1: Write failing tests**
  - helmchart: the trial render has no `dev-token-change-me` and no `UNIFIED_ALLOW_DEFAULT_TOKEN`; the Deployment references the Secret `unified-cd-initial-admin-token` with `optional: true`; the ingress guard message is the new text.
  - stepenv and vars: `UNIFIED_INITIAL_ADMIN_TOKEN` is reserved.
- [ ] **Step 2: Run them and verify they fail**: `go test ./internal/helmchart/ ./internal/agent/ ./internal/dsl/`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./internal/helmchart/ ./internal/agent/ ./internal/dsl/`, `helm lint charts/unified-cd`, `docker compose -f deployments/docker/docker-compose.yaml config -q` (with a dummy `.env` in a temp dir), and `go vet ./test/...` with the HA build tag. Expected: PASS. CI runs the kind and HA jobs after the push; say in the report which jobs could not run locally.
- [ ] **Step 5: Commit** `chore: initial admin token in chart, compose, CI and test harnesses`.

### Task 11: Documentation

**Files:**
- Create: `docs/operator-manual/migrations/2026-11-27-internal-accounts.md`
- Modify:
  - `mkdocs.yml`: add `"Internal Accounts": operator-manual/migrations/2026-11-27-internal-accounts.md` after the run-metadata entry.
  - `README.md`, `charts/unified-cd/README.md`
  - Getting started: `docs/getting-started/installation.md`, `docs/getting-started/quickstart.md`
  - Operator manual: `docs/operator-manual/authentication.md` (rewrite "PAT owners and ceilings" and the static-token sections), `docs/operator-manual/authorization.md` (replace "Break-glass" with "Built-in admin account and recovery"), `docs/operator-manual/audit.md` (actor ids), `docs/operator-manual/high-availability.md`, `docs/operator-manual/operations.md`, `docs/operator-manual/migrations/2026-11-09-production-compose-hardening.md` (add a forward pointer only)
  - Reference: `docs/reference/cli.md`, `docs/reference/configuration.md`
  - Troubleshooting: `docs/troubleshooting/controller-and-database.md`, `docs/troubleshooting/index.md`
  - `docs/user-guide/variables.md`, `docs/contributing/auth-implementation.md`, `examples/config/controller.yaml`
  - `internal/config/stale_refs_test.go`: remove the `t.Skip`.

**Interfaces:**
- The migration guide covers, in this order:
  1. what breaks: ownerless PATs are deleted, the controller `UNIFIED_TOKEN` is ignored, and `UNIFIED_ALLOW_DEFAULT_TOKEN` is gone;
  2. how to find the deleted PATs, from the audit rows with actor `migration`;
  3. the stop-all-replicas upgrade procedure;
  4. re-bootstrap for non-SSO installs, with each install type: binary, Docker Compose and Kubernetes;
  5. declaring `serviceAccounts` for CI;
  6. disabling `admin` under SSO;
  7. `admin recover`;
  8. PAT lifetimes, and how to opt out with `tokens.maxLifetime: 0`;
  9. the web UI token form under SSO, and `ui.allowTokenEntry`.
- Troubleshooting lists every error string defined in Tasks 3 to 8, verbatim.
- [ ] **Step 1: Find every reference** with `git grep -n -E 'UNIFIED_TOKEN|break-glass|Break-glass|dev-token-change-me|allowDefaultToken|ALLOW_DEFAULT_TOKEN|env:UNIFIED_TOKEN|role-only' -- docs README.md charts examples`. Classify each hit as rewrite, keep-as-history (dated migration guides) or client-side CLI `UNIFIED_TOKEN` (keep), and put the classification in the report.
- [ ] **Step 2: Write the guide and update the pages.**
- [ ] **Step 3: Run** `mkdocs build --strict` (from `docs/requirements.txt`; if mkdocs is not installed locally, say so) and `go test ./internal/config/ -run Stale`. Expected: PASS.
- [ ] **Step 4: Commit** `docs: internal accounts, initial admin token and recovery`.
