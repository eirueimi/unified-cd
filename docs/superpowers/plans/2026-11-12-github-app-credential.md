# GitHub App Credentials Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `type: githubApp` GitCredential whose private key stays on the controller, which issues short-lived installation tokens for controller git access (AppSource, `uses:`) and hands them to jobs allowed by `jobAccess`, under the credential's name in `.Secrets`.

**Architecture:** A new leaf package `internal/gittoken` signs App JWTs, looks up installations, expands repository patterns and caches tokens per replica. One controller-side `GitCredentialResolver` replaces the two duplicate secret-to-credential functions and serves AppSource sync, `uses:` resolution and the agent secrets fetch. Storage gains a `config jsonb` column (`githubApp` + `jobAccess`), `controller_use`, and an audit `details` column (migration 029).

**Tech Stack:** Go 1.26.10, PostgreSQL (pgx, golang-migrate), go-jose v4 (`github.com/go-jose/go-jose/v4`, `.../v4/jwt`) for RS256, `net/http/httptest` TLS servers for GitHub fakes, dockertest via `storetest.NewTestPostgres`, Svelte + vitest for the UI list.

**Spec:** `docs/superpowers/specs/2026-11-12-github-app-credential-design.md`

## Global Constraints

- All repository text (code, comments, docs, commit messages, error strings) is English.
- Commit with `git -c user.name=eirueimi -c user.email="128969149+eirueimi@users.noreply.github.com" commit -F <msgfile>`. Every message ends with a blank line and then these two trailers:
  ```
  Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01G3NWwwXnjYtSfzJjZYTrnV
  ```
- Never `git add` anything under `.superpowers/`. Stage explicit paths, never `git add -A`. Never push.
- No new module dependencies. `golang.org/x/sync` is indirect in go.mod; do not promote it (use a mutex instead of singleflight).
- The private key, the App JWT and installation tokens never appear in logs, errors, audit rows, API responses or metric labels. Errors from GitHub carry the HTTP status and GitHub's `message` only.
- The private key never leaves the controller: not in a claim response, not in an agent secrets fetch (see Task 5, Step 1, `TestAgentSecretsFetch_PrivateKeyNeverDelivered`).
- Only `gitcredential.write` (global) changes `jobAccess`; nothing on the job side widens it.
- TDD: each code step is preceded by a failing test that is run and seen failing.
- Docs that describe behaviour changed by a task are updated in that same task. `docs/reference/field-reference.md` and `schemas/unified-cd.schema.json` are generated (`go generate ./internal/dsl/`); never hand-edit them.
- Pinned values: JWT `iat` = now − 60 s, `exp` = now + 9 min; job token handed out only with ≥ 50 min left; controller token reused until < 5 min left; owner → installation cache 1 h; HTTPS user `x-access-token` for `githubApp`, `oauth2` stays for `token`; permission levels `read < write < admin`; default API URL `https://api.github.com`.
- Postgres tests need Docker (running on this host). Linux-only tests (real `sh` + `git`) run in a container:
  `docker run --rm -v "$(pwd -W 2>/dev/null || pwd)":/mnt/src:ro golang:1.26.10 sh -c 'cp -r /mnt/src /src && cd /src && go test <pkg> -run <name> -count=1 -v'`

## Review Focus

1. An admin narrows `jobAccess` (or changes App ID / key ref) while a token is cached: the next hand-out must reflect the new reach, not reuse the wider cached token. Pinned by `TestIssuer_CacheKeyIncludesConfigAndRequest` (Task 3) and `TestAgentSecretsFetch_NarrowedJobAccessGetsNewToken` (Task 5).
2. A job names the App's private-key StoredSecret (`{{ .Secrets.github-app-key }}`) directly: it must be refused, not delivered. Pinned by `TestAgentSecretsFetch_PrivateKeyNeverDelivered` (Task 5).
3. A private `uses:` template fetched with a token for an allowed run is in the template cache; a run that `jobAccess` does not allow must not get it from the cache. Pinned by `TestResolveGitPendingRuns_TemplateCacheDoesNotLeakAcrossJobAccess` (Task 4).
4. A pattern (`acme/infra-*`) matches no repository of the installation: the request must fail, never be sent with an empty `repositories` list (which GitHub reads as the whole installation). Pinned by `TestIssuer_PatternWithNoMatchFails` (Task 3).
5. Folder and owner matching edge cases: `team-a` must not cover `team-ab/x`; a root-level job is covered only by `"*"`; `Acme/App` matches `acme/app` (GitHub names are case-insensitive). Pinned by `TestJobAccessCovers` and `TestGitCredResolver_UsesJobAccess` (Task 4) and `TestMatchRepositoryName` (Task 2).

---

### Task 1: Store, migration 029 and audit details

**Files:**
- Create: `internal/store/migrations/029_github_app_credentials.up.sql`, `internal/store/migrations/029_github_app_credentials.down.sql`
- Modify: `internal/store/store.go:146-151` (AuditEntry), `:205-223` (GitCredential, PendingRun), `:588-591` (interface)
- Modify: `internal/store/postgres.go:2945-3040` (git credential methods, InsertAuditLog, ListAuditLogs), `:3048-3067` (ListPendingRuns)
- Modify: `internal/store/verify.go` (sentinel 29)
- Modify: `internal/api/types.go` (new `GitHubAppConfig`, `GitJobAccess`; `AuditLog.Details`)
- Modify callers so the tree compiles: `internal/controller/api_gitcredential.go:44`, `internal/controller/appsource_apply.go:93`, test callers in `internal/controller/api_gitcredential_test.go`, `audit_test.go`, `managed_guard_test.go`
- Test: `internal/store/postgres_gitcredentials_test.go`, `internal/store/postgres_audit_test.go` (create if no audit store test file exists; otherwise extend the existing one)

**Interfaces:**
- Consumes: nothing new.
- Produces:
  - `api.GitHubAppConfig{AppID int64 \`json:"appId"\`; InstallationID int64 \`json:"installationId,omitempty"\`; APIURL string \`json:"apiURL,omitempty"\`}`
  - `api.GitJobAccess{Folders []string \`json:"folders"\`; Repositories []string \`json:"repositories"\`; Permissions map[string]string \`json:"permissions"\`}`
  - `api.AuditLog.Details json.RawMessage \`json:"details,omitempty"\``
  - `store.GitCredential` gains `ControllerUse bool`, `GitHubApp *api.GitHubAppConfig`, `JobAccess *api.GitJobAccess`
  - `store.AuditEntry` gains `Details map[string]any` (nil or empty → SQL NULL)
  - `store.PendingRun` gains `JobName string`
  - `Store.UpsertGitCredential(ctx context.Context, gc GitCredential) error` (ID, CreatedAt, UpdatedAt ignored)
  - `Store.GetGitCredential(ctx context.Context, name string) (*GitCredential, error)` — `(nil, nil)` when absent
  - `Store.GetGitCredentialByHost` now returns only rows with `controller_use = true`

- [ ] **Step 1: Write the failing store tests**

```go
// postgres_gitcredentials_test.go (update TestPostgres_GitCredentialKnownHosts to the new signature, then add:)
func TestPostgres_GitCredentialGitHubAppRoundTrip(t *testing.T) {
	pg := NewTestPostgres(t); ctx := t.Context()
	in := GitCredential{Name: "gh-app", Host: "github.com", CredType: "githubApp", SecretRef: "github-app-key", ControllerUse: true,
		GitHubApp: &api.GitHubAppConfig{AppID: 123456, InstallationID: 7890123, APIURL: "https://api.github.com"},
		JobAccess: &api.GitJobAccess{Folders: []string{"team-a"}, Repositories: []string{"acme/app", "acme/infra-*"},
			Permissions: map[string]string{"contents": "write"}}}
	require.NoError(t, pg.UpsertGitCredential(ctx, in))
	got, err := pg.GetGitCredential(ctx, "gh-app")
	require.NoError(t, err)
	assert.Equal(t, in.GitHubApp, got.GitHubApp)
	assert.Equal(t, in.JobAccess, got.JobAccess)
	assert.True(t, got.ControllerUse)
	list, _ := pg.ListGitCredentials(ctx)
	assert.Equal(t, in.JobAccess, list[0].JobAccess)
}
func TestPostgres_GetGitCredentialMissingIsNil(t *testing.T) // got == nil, err == nil
func TestPostgres_GetGitCredentialByHostSkipsControllerUseFalse(t *testing.T) {
	// "older" (controllerUse true) upserted first, "newer" (controllerUse false) second, same host:
	// GetGitCredentialByHost returns "older"; after "older" is updated to controllerUse false too, it returns nil.
}
func TestPostgres_GitCredentialTokenTypeDefaults(t *testing.T) // token row: GitHubApp == nil, JobAccess == nil, ControllerUse as given
func TestPostgres_GitCredentialCheckAcceptsGitHubAppRejectsOther(t *testing.T) // CredType "bogus" → error from the CHECK
func TestPostgres_ListPendingRunsCarriesJobName(t *testing.T) // CreateRun(ctx, "team-a/build", ...) → ListPendingRuns()[0].JobName == "team-a/build"

// audit
func TestPostgres_AuditLogDetailsRoundTrip(t *testing.T) {
	// InsertAuditLog with Details{"runId": "r1", "repositories": []string{"app"}} → ListAuditLogs()[0].Details JSON-equals
	// {"runId":"r1","repositories":["app"]}; a second row without Details → Details == nil (no "details" key in JSON).
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/store/ -run 'TestPostgres_GitCredential|TestPostgres_GetGitCredential|TestPostgres_ListPendingRunsCarriesJobName|TestPostgres_AuditLogDetails' -count=1`
Expected: build failure (`too many arguments` / `GetGitCredential undefined` / `unknown field Details`).

- [ ] **Step 3: Write migration 029 and its sentinel**

Up (exact):
```sql
ALTER TABLE public.git_credentials DROP CONSTRAINT git_credentials_cred_type_check;
ALTER TABLE public.git_credentials ADD CONSTRAINT git_credentials_cred_type_check
    CHECK (cred_type = ANY (ARRAY['token'::text, 'sshKey'::text, 'githubApp'::text]));
ALTER TABLE public.git_credentials ADD COLUMN controller_use boolean NOT NULL DEFAULT true;
ALTER TABLE public.git_credentials ADD COLUMN config jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE public.audit_logs ADD COLUMN details jsonb;
```
Down: `DELETE FROM public.git_credentials WHERE cred_type = 'githubApp';`, restore the two-value CHECK, drop `config`, `controller_use`, `audit_logs.details` (all `IF EXISTS`).
Sentinel: `{version: 29, migration: "029_github_app_credentials", table: "git_credentials", column: "config"}`.

- [ ] **Step 4: Implement the store changes in `internal/store/store.go` and `internal/store/postgres.go`**

`config` holds `{"githubApp": {...}, "jobAccess": {...}}` (keys omitted when nil); marshal with `encoding/json` from an unexported `gitCredentialConfig{GitHubApp *api.GitHubAppConfig \`json:"githubApp,omitempty"\`; JobAccess *api.GitJobAccess \`json:"jobAccess,omitempty"\`}` and scan it back in all three readers through one `scanGitCredential(row pgx.Row) (*GitCredential, error)` helper. `GetGitCredentialByHost` adds `AND controller_use` to the WHERE clause, keeping `ORDER BY updated_at DESC LIMIT 1`. `InsertAuditLog` writes `details` as `$12::jsonb`, NULL when `len(e.Details) == 0`; `ListAuditLogs` selects it into `api.AuditLog.Details`. `ListPendingRuns` also selects `job_name`. Update the two non-test callers to build a `store.GitCredential` from their existing fields with `ControllerUse: true` (Task 2 replaces this).

- [ ] **Step 5: Run the store tests and the sentinel gate**

Run: `go test ./internal/store/ -count=1`
Expected: PASS (includes `TestSchemaSentinelsCoverAllMigrations` and the existing migrate up/down tests).

- [ ] **Step 6: Build and run the packages that use the store**

Run: `go build ./... && go vet ./internal/... && go test ./internal/controller/ -run 'GitCredential|Audit|ManagedGuard' -count=1`
Expected: PASS.

- [ ] **Step 7: Commit**

`git add internal/store internal/api/types.go internal/controller/api_gitcredential.go internal/controller/appsource_apply.go internal/controller/*_test.go` (only the files touched), message `feat(store): store GitHub App git credentials and audit details (migration 029)`.

---

### Task 2: DSL, API, CLI, GitOps and UI for the new fields

**Files:**
- Modify: `internal/dsl/gitcredential_types.go`, `internal/dsl/gitcredential_parse.go`
- Create: `internal/dsl/gitcredential_access.go` (repository pattern helpers)
- Modify: `internal/api/types.go:563-581` (UpsertGitCredentialRequest, GitCredentialMeta)
- Modify: `internal/controller/api_gitcredential.go`, `internal/controller/appsource_apply.go:88-96`, `internal/controller/api_secrets.go:17-45` (collision on secret set)
- Modify: `internal/cli/apply.go:97-120`, `internal/cli/export.go:181-199`, `internal/cli/gitcredential.go:56-62`
- Modify: `web/src/routes/GitCredentialList.svelte`
- Regenerate: `schemas/unified-cd.schema.json`, `docs/reference/field-reference.md`
- Modify docs: `docs/reference/cli.md` (gitcredential list output, apply of the new fields)
- Test: `internal/dsl/gitcredential_parse_test.go` (create or extend), `internal/controller/api_gitcredential_test.go`, `internal/controller/appsource_apply_test.go` (extend the existing GitOps apply test file), `internal/controller/api_secrets_test.go`, `internal/cli/export_roundtrip_test.go`, `internal/cli/request_contract_test.go`, `web/src/routes/GitCredentialList.test.js` (create)

**Interfaces:**
- Consumes: Task 1 `api.GitHubAppConfig`, `api.GitJobAccess`, `store.GitCredential`, `Store.GetGitCredential`, `Store.UpsertGitCredential(ctx, gc)`.
- Produces:
  - `dsl.GitCredentialSpec` gains `ControllerUse *bool \`yaml:"controllerUse,omitempty"\``, `GitHubApp *GitHubAppSpec \`yaml:"githubApp,omitempty"\``, `JobAccess *GitJobAccessSpec \`yaml:"jobAccess,omitempty"\``; `Type` tag becomes `schema:"enum:token,sshKey,githubApp"`
  - `dsl.GitHubAppSpec{AppID int64 \`yaml:"appId"\`; InstallationID int64 \`yaml:"installationId,omitempty"\`; APIURL string \`yaml:"apiURL,omitempty"\`}`
  - `dsl.GitJobAccessSpec{Folders []string \`yaml:"folders"\`; Repositories []string \`yaml:"repositories"\`; Permissions map[string]string \`yaml:"permissions"\`}`
  - `func (s GitCredentialSpec) ControllerUseOrDefault() bool` — nil → true
  - `func ParseRepositoryPatterns(patterns []string) (owner string, names []string, err error)` — `"acme/app"` → `"app"`, `"acme/infra-*"` → `"infra-*"`, `"acme/*"` → `"*"`
  - `func MatchRepositoryName(pattern, name string) bool` — case-insensitive; `"*"` matches all; `"p*"` is a prefix match; otherwise equality
  - `func RepositoryAllowed(patterns []string, owner, repo string) bool` — owner compared case-insensitively
  - `api.UpsertGitCredentialRequest` gains `ControllerUse *bool \`json:"controllerUse,omitempty"\``, `GitHubApp *GitHubAppConfig \`json:"githubApp,omitempty"\``, `JobAccess *GitJobAccess \`json:"jobAccess,omitempty"\``
  - `api.GitCredentialMeta` gains `ControllerUse bool \`json:"controllerUse"\``, `GitHubApp *GitHubAppConfig \`json:"githubApp,omitempty"\``, `JobAccess *GitJobAccess \`json:"jobAccess,omitempty"\``
  - controller: `func gitCredentialFromDSL(gc *dsl.GitCredential) (store.GitCredential, error)` — validates and normalizes `jobAccess.folders` with `authz.NormalizeFolder` (`"*"` kept as-is); shared by the API handler and GitOps apply
  - controller: `func checkSecretNamespaceCollision(ctx context.Context, st store.Store, gc store.GitCredential) error` (used by the API handler and by GitOps `applyResource`) and the reverse check in `handleSetSecret`; both wrap `errNameCollision`, which the API maps to HTTP 409

- [ ] **Step 1: Write the failing DSL validation tests**

```go
func TestGitCredentialValidate_GitHubApp(t *testing.T) {
	ok := `...full resource from the spec's "Resource shape" section...`
	_, err := ParseGitCredential(strings.NewReader(ok)); require.NoError(t, err)
	cases := []struct{ name, mutate, wantErr string }{
		{"type enum", `type: bogus`, `spec.type must be "token", "sshKey" or "githubApp", got "bogus"`},
		{"githubApp required", `(githubApp removed)`, `spec.githubApp is required for type "githubApp"`},
		{"githubApp only for githubApp", `(type: token, githubApp kept)`, `spec.githubApp is only valid for type "githubApp"`},
		{"appId positive", `appId: 0`, `spec.githubApp.appId must be a positive integer`},
		{"installationId non-negative", `installationId: -1`, `spec.githubApp.installationId must not be negative`},
		{"apiURL https", `apiURL: http://ghe.example.com/api/v3`, `spec.githubApp.apiURL must be an https URL without credentials, query or fragment`},
		{"knownHosts not with githubApp", `knownHosts: ...`, `spec.knownHosts is only valid for type "sshKey"`},
		{"jobAccess only githubApp", `(type: token + jobAccess)`, `spec.jobAccess is only valid for type "githubApp"`},
		{"folders required", `folders: []`, `spec.jobAccess.folders must not be empty`},
		{"repositories required", `repositories: []`, `spec.jobAccess.repositories must not be empty`},
		{"permissions required", `permissions: {}`, `spec.jobAccess.permissions must not be empty`},
		{"single owner", `repositories: [acme/app, widget/lib]`, `spec.jobAccess.repositories must name a single owner, got "acme" and "widget"`},
		{"pattern shape", `repositories: [acme/*app]`, `spec.jobAccess.repositories[0] "acme/*app": want "owner/name", "owner/*" or "owner/prefix*"`},
		{"level", `permissions: {contents: maintain}`, `spec.jobAccess.permissions.contents must be "read", "write" or "admin", got "maintain"`},
		{"permission name", `permissions: {"Contents!": read}`, `spec.jobAccess.permissions: invalid permission name "Contents!"`},
		{"name vs secret pattern", `metadata.name: 1app`, `metadata.name "1app" must match the secret-name pattern ^[A-Za-z_][A-Za-z0-9_-]*$ when spec.jobAccess is set`},
	}
	// each case: ParseGitCredential(mutated) error contains wantErr
}
func TestGitCredentialControllerUseDefault(t *testing.T) // absent → ControllerUseOrDefault() == true; "controllerUse: false" → false; allowed for token and sshKey
func TestParseRepositoryPatterns(t *testing.T) // [acme/app acme/infra-*] → ("acme", [app infra-*]); [Acme/x acme/y] → owner "Acme", no error (case-insensitive same owner)
func TestMatchRepositoryName(t *testing.T) {
	assert.True(t, MatchRepositoryName("*", "anything"))
	assert.True(t, MatchRepositoryName("infra-*", "Infra-DB"))
	assert.False(t, MatchRepositoryName("infra-*", "infra"))   // prefix "infra-" required
	assert.True(t, MatchRepositoryName("App", "app"))
	assert.False(t, MatchRepositoryName("app", "app2"))
	assert.True(t, RepositoryAllowed([]string{"acme/app"}, "ACME", "App"))
	assert.False(t, RepositoryAllowed([]string{"acme/app"}, "widget", "app"))
}
```

Allowed permission-name pattern: `^[a-z][a-z_]*$`. Repository segment charset: `[A-Za-z0-9._-]+`; `*` only as the whole name or its last character.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/dsl/ -run 'TestGitCredential|TestParseRepositoryPatterns|TestMatchRepositoryName' -count=1`
Expected: FAIL (unknown field `githubApp` / undefined functions).

- [ ] **Step 3: Implement the DSL types, `Validate` additions and `gitcredential_access.go`**

Field doc comments on the new types are what the generated field reference shows; write them as user-facing one-liners (from the spec's Resource shape comments).

- [ ] **Step 4: Run the DSL tests, regenerate, and run the generators' gates**

Run: `go test ./internal/dsl/ -count=1 && go generate ./internal/dsl/ && go test ./cmd/docgen/ ./cmd/schemagen/ ./internal/dslschema/ -count=1 && git diff --stat schemas docs/reference/field-reference.md`
Expected: PASS; the diff shows `githubApp` in the type enum and the `controllerUse`, `githubApp`, `jobAccess` fields.

- [ ] **Step 5: Write the failing API, GitOps and collision tests**

```go
func TestAPI_UpsertGitCredential_GitHubApp(t *testing.T) {
	// POST {name gh-app-release, host github.com, credType githubApp, secretRef github-app-key,
	//       githubApp{appId 123456}, jobAccess{folders [team-a], repositories [acme/app], permissions {contents: write}}} → 204
	// GET /api/v1/gitcredentials → entry has CredType githubApp, GitHubApp.AppID 123456, ControllerUse true, JobAccess equal;
	// body does not contain the private key PEM stored under github-app-key (set via mustSetSecret before).
}
func TestAPI_UpsertGitCredential_GitHubAppValidation(t *testing.T) // missing githubApp → 400 with `githubApp is required`; folder "a/../b" → 400 `invalid folder`
func TestAPI_UpsertGitCredential_ControllerUseFalse(t *testing.T) // controllerUse:false on a token credential → listed with controllerUse false
func TestAPI_UpsertGitCredential_OmittedControllerUseDefaultsTrue(t *testing.T) // old-style request without the field → true
func TestAPI_GitCredentialNameCollidesWithSecret(t *testing.T) {
	// secret "gh-app-release" exists → upsert of a jobAccess credential "gh-app-release" → 409,
	//   body `name "gh-app-release" is already used by a StoredSecret; a GitCredential with jobAccess shares the secrets namespace`
	// same name WITHOUT jobAccess → 204 (no shared namespace)
}
func TestAPI_SetSecretCollidesWithJobAccessCredential(t *testing.T) {
	// jobAccess credential "gh-app-release" exists → POST /api/v1/secrets name gh-app-release → 409,
	//   body `name "gh-app-release" is already used by a GitCredential with jobAccess`
	// a credential without jobAccess named "plain" does not block secret "plain"
}
func TestApplyResource_GitCredentialGitHubApp(t *testing.T) // GitOps doc with all new fields → stored row equal; collision with a secret → error contains "already used by a StoredSecret"
```

- [ ] **Step 6: Run to verify they fail**

Run: `go test ./internal/controller/ -run 'TestAPI_UpsertGitCredential|TestAPI_GitCredentialNameCollides|TestAPI_SetSecretCollides|TestApplyResource_GitCredential' -count=1`
Expected: FAIL.

- [ ] **Step 7: Implement the handler, GitOps apply, collision checks and list fields**

`handleUpsertGitCredential` builds a `dsl.GitCredential{APIVersion: dsl.SupportedAPIVersion, Kind: "GitCredential", ...}` from the request, calls `Validate()` (one rule set for API and GitOps), then `gitCredentialFromDSL`, then the collision check, then `guardManagedResource`, then `UpsertGitCredential`. Keep the existing `host, credType, secretRef are required` message for the all-empty case so existing tests stay valid. The collision check runs only when `JobAccess != nil` and uses `GetSecret` (`pgx.ErrNoRows` = free). `handleSetSecret` calls `GetGitCredential(name)` and rejects when the row has `JobAccess != nil`. List maps the three new fields; never the secret value.

- [ ] **Step 8: Write the failing CLI tests, then implement CLI apply, export and list**

```go
// request_contract_test.go: applying the spec's full GitCredential YAML sends UpsertGitCredentialRequest with
//   GitHubApp{AppID:123456, InstallationID:7890123, APIURL:"https://api.github.com"}, JobAccess equal, ControllerUse ptr(true)
// export_roundtrip_test.go: a githubApp meta with controllerUse false exports spec {host,type,secretRef,controllerUse:false,githubApp,jobAccess}
//   and re-parses with dsl.ParseGitCredential; a token meta with controllerUse true exports without the controllerUse key
// gitcredential list: line contains "type=githubApp appId=123456 installationId=7890123 controllerUse=true jobAccess=folders:team-a repos:acme/app,acme/infra-* perms:contents=write"
```
CLI apply parses the document with `dsl.ParseGitCredential` (client-side validation) instead of the local struct. List prints `installationId=auto` when 0 and `apiURL=` only when set.

Run: `go test ./internal/cli/ -count=1`
Expected: PASS after implementation (FAIL before).

- [ ] **Step 9: UI list columns**

`GitCredentialList.svelte` adds columns "Controller use" (yes/no) and "Job access" (folders, repositories; "none" when absent) and shows `App ID` under the type badge for `githubApp`. Test `GitCredentialList.test.js`: mocked `apiFetch` returning one githubApp entry renders `123456`, `team-a`, `acme/app`, and `no` for controllerUse false.
Run: `cd web && npx vitest run src/routes/GitCredentialList.test.js`
Expected: PASS.

- [ ] **Step 10: Update `docs/reference/cli.md` (gitcredential list output example) and run the full affected suites**

Run: `go test ./internal/dsl/ ./internal/cli/ ./internal/controller/ -count=1`
Expected: PASS.

- [ ] **Step 11: Commit**

Message `feat(gitcredential): githubApp type, controllerUse and jobAccess fields`.

---

### Task 3: `internal/gittoken` token source

**Files:**
- Create: `internal/gittoken/gittoken.go` (types, constants), `internal/gittoken/githubapp.go` (JWT, installation lookup, expansion, POST), `internal/gittoken/issuer.go` (caches, locking)
- Create: `internal/gittoken/gittokentest/fake.go` (fake GitHub used by Tasks 3–5)
- Test: `internal/gittoken/githubapp_test.go`, `internal/gittoken/issuer_test.go`

**Interfaces:**
- Consumes: `dsl.MatchRepositoryName` (Task 2).
- Produces:
  ```go
  type TokenRequest struct {
      Owner        string            // repository owner; selects the installation
      Repositories []string          // names within Owner; trailing-"*" prefix patterns allowed; nil or ["*"] = the installation's
      Permissions  map[string]string // name -> read|write|admin; empty = installation's
  }
  type Token struct { Value string; ExpiresAt time.Time }
  type TokenSource interface { Token(ctx context.Context, req TokenRequest) (Token, error) }

  type GitHubApp struct {
      Name           string // credential name: cache key prefix and error context
      AppID          int64
      InstallationID int64  // 0 = look up
      APIURL         string // "" = DefaultAPIURL
      KeyRef         string // StoredSecret name; part of the cache fingerprint
      PrivateKey     func(ctx context.Context) ([]byte, error) // PEM (PKCS#1 or PKCS#8); called only on a cache miss
  }
  func NewIssuer(client *http.Client, now func() time.Time) *Issuer // nil client → &http.Client{Timeout: DefaultHTTPTimeout}; nil now → time.Now
  func (i *Issuer) Source(app GitHubApp, minRemaining time.Duration) TokenSource
  // Issuer.OnRequest func(credential string, err error) — optional hook called once per POST access_tokens (metrics)

  const (
      DefaultAPIURL          = "https://api.github.com"
      DefaultHTTPTimeout     = 30 * time.Second
      JobMinRemaining        = 50 * time.Minute
      ControllerMinRemaining = 5 * time.Minute
      InstallationCacheTTL   = time.Hour
  )
  ```
  - `gittokentest.GenerateKeyPEM(t testing.TB, pkcs8 bool) (*rsa.PrivateKey, []byte)`
  - `gittokentest.NewFakeGitHub(t testing.TB, pub *rsa.PublicKey) *FakeGitHub` — TLS httptest server; fields `URL string`, `InstallationID int64` (default 7890123), `Repos []string`, `TokenTTL time.Duration` (default 1 h), `Now func() time.Time`, `IssueStatus int`/`IssueMessage string` (forced failure); methods `Client() *http.Client`, `AccessTokenRequests() []AccessTokenRequest` (`{InstallationID int64; Repositories []string; Permissions map[string]string; HasRepositoriesKey bool}`), `Paths() []string`, `LastJWT() string`. Tokens are `"ghs_<n>"`. It verifies the JWT signature with `pub` and rejects (401) a bad one.

- [ ] **Step 1: Write the failing tests**

```go
func TestGitHubApp_JWTClaims(t *testing.T) {
	// fixed clock 2026-11-12T10:00:00Z; after one Token() call, parse fake.LastJWT() with go-jose:
	// alg RS256; iss == "123456"; iat == 09:59:00; exp == 10:09:00
}
func TestGitHubApp_IssuesNarrowedToken(t *testing.T) {
	// InstallationID 7890123; req{Owner "acme", Repositories ["app"], Permissions {"contents":"read"}}
	// → one POST /app/installations/7890123/access_tokens with Repositories ["app"], Permissions {"contents":"read"};
	// Token.Value == "ghs_1"; ExpiresAt == fake's expires_at
}
func TestGitHubApp_InstallationLookupByRepoCachedOneHour(t *testing.T) {
	// InstallationID 0; req Repositories ["app"] → GET /repos/acme/app/installation once; second call (different perms) → no lookup;
	// clock +61m and a new request → lookup again
}
func TestGitHubApp_InstallationLookupByOwner(t *testing.T) {
	// Repositories ["*"] → GET /orgs/acme/installation; when that returns 404 → GET /users/acme/installation is used
}
func TestGitHubApp_StarSendsNoRepositories(t *testing.T) // ["*"] → HasRepositoriesKey == false
func TestGitHubApp_PatternExpansion(t *testing.T) {
	// fake.Repos = [infra-a, Infra-B, app, …150 more] (two pages, Link rel="next");
	// req Repositories ["infra-*"] → first POST has Permissions {"metadata":"read"} and no repositories (listing token),
	// GET /installation/repositories?per_page=100 is paged to the end, final POST Repositories == ["infra-a","Infra-B"]
}
func TestIssuer_PatternWithNoMatchFails(t *testing.T) {
	// Repositories ["nomatch-*"] → error contains `no repository of the installation matches "nomatch-*"`;
	// no POST other than the listing token's (no request without repositories for the job token)
}
func TestGitHubApp_ErrorsCarryStatusAndMessageOnly(t *testing.T) {
	// fake.IssueStatus 422, IssueMessage "Bad permissions" → err contains "422" and "Bad permissions";
	// err does not contain fake.LastJWT() nor any line of the PEM
}
func TestGitHubApp_AcceptsPKCS1AndPKCS8(t *testing.T) // both GenerateKeyPEM variants issue a token; garbage PEM → error "parse private key" without key bytes
func TestIssuer_PrivateKeyLoadedOnlyOnMiss(t *testing.T) // two cached calls → PrivateKey loader called once
func TestIssuer_JobTokenReissuedUnderFiftyMinutes(t *testing.T) {
	// minRemaining JobMinRemaining; t0 issue ghs_1 (expires t0+60m); t0+9m → ghs_1; t0+11m → ghs_2
}
func TestIssuer_ControllerTokenReusedUntilFiveMinutes(t *testing.T) // t0+54m → ghs_1; t0+56m → ghs_2
func TestIssuer_CacheKeyIncludesConfigAndRequest(t *testing.T) {
	// same Name: different Permissions → new token; different Repositories → new token; AppID or KeyRef changed → new token;
	// identical config+request (map order differs) → cached token
}
func TestIssuer_ConcurrentMissIssuesOnce(t *testing.T) // 10 goroutines, same key → 1 POST access_tokens (run with -race)
func TestIssuer_OnRequestHook(t *testing.T) // hook called with ("gh-app", nil) once per POST and with a non-nil error on failure
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/gittoken/... -count=1`
Expected: build failure (package empty).

- [ ] **Step 3: Implement `gittoken.go`, `githubapp.go`, `issuer.go` and the fake**

JWT: `jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)` and `jwt.Signed(signer).Claims(jwt.Claims{Issuer: strconv.FormatInt(AppID,10), IssuedAt: jwt.NewNumericDate(now.Add(-60*time.Second)), Expiry: jwt.NewNumericDate(now.Add(9*time.Minute))}).Serialize()`. Key parsing: `x509.ParsePKCS1PrivateKey`, then `x509.ParsePKCS8PrivateKey` (must be `*rsa.PrivateKey`). Requests send `Accept: application/vnd.github+json` and `X-GitHub-Api-Version: 2022-11-28`; JWT requests use `Authorization: Bearer <jwt>`, the repository listing uses `Authorization: token <listing token>`. Installation lookup: a concrete name in `Repositories` → `/repos/{owner}/{name}/installation`; otherwise `/orgs/{owner}/installation`, falling back to `/users/{owner}/installation` on 404; cache key `apiURL + "|" + lower(owner)`. Error form: `github app %q: <operation>: HTTP %d: %s` (GitHub `message`, truncated to 200 bytes). Token cache key: `Name` + SHA-256 of a canonical JSON of `{AppID, InstallationID, APIURL, KeyRef, Owner (lower), sorted Repositories, sorted Permissions}`. A per-key mutex serializes misses. Reject a request with more than 500 expanded names (`GitHub accepts at most 500 repositories per token`).

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/gittoken/... -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

Message `feat(gittoken): GitHub App installation token source with caching`.

---

### Task 4: Controller-side use (AppSource and `uses:`)

**Files:**
- Create: `internal/controller/gitcred_resolver.go`
- Modify: `internal/gittemplate/resolve.go:17-30, 179-186, 381-395`, `internal/gittemplate/cache.go`, `internal/gittemplate/fetch.go:164-190, 618-645` (and the internal helpers that take `token, sshKey`)
- Modify: `internal/controller/appsource_reconciler.go:27-52, 106-130, 302-338` (delete `resolveCredential`), `internal/controller/scheduler.go:305-372`
- Modify: `internal/metrics/metrics.go` (two counters)
- Modify: `cmd/controller/main.go:580-595` (one `gittoken.Issuer` + resolver, wired into Server, RunGitResolver, RunAppSourceReconciler)
- Test: `internal/gittemplate/resolve_test.go`, `cache` tests (new `internal/gittemplate/cache_test.go`), `fetch_argv_test.go`, `internal/controller/gitcred_resolver_test.go`, `internal/controller/scheduler_test.go` (or the existing git-resolver test file), `internal/controller/appsource_reconciler_test.go`, `internal/metrics/metrics_test.go`, `internal/controller/gitcred_github_integration_test.go`

**Interfaces:**
- Consumes: Task 1 store API; Task 2 `dsl.RepositoryAllowed`; Task 3 `gittoken.Issuer`, `GitHubApp`, `TokenRequest`, `ControllerMinRemaining`, `JobMinRemaining`, `DefaultAPIURL`.
- Produces:
  ```go
  // gittemplate
  type Credential struct {
      Token, SSHKey string
      Username      string // HTTPS basic-auth user; "" = "oauth2"
      CacheScope    string // template cache partition; "" = fetched anonymously
      Hint          string // appended to a fetch error (anonymous fallback explanation)
  }
  type CredentialRequest struct { Host, Owner, Repo, JobName string }
  type CredentialFunc func(ctx context.Context, req CredentialRequest) (Credential, error)
  type FetcherInterface interface { Fetch(ctx context.Context, uri URI, cred Credential) ([]byte, error) }
  func (f *Fetcher) ResolveCommitSHA(ctx context.Context, repoURL, ref string, cred Credential) (string, error)
  func (f *Fetcher) FetchDir(ctx context.Context, repoURL, ref, path string, cred Credential) (map[string][]byte, error)
  func (c *Cache) Get(ctx context.Context, uri URI, scope string) ([]byte, bool)
  func (c *Cache) Put(ctx context.Context, uri URI, scope string, data []byte)
  const GitHubAppUser = "x-access-token"

  // controller
  type GitCredentialResolver struct { /* st, km, issuer, metrics */ }
  func NewGitCredentialResolver(st store.Store, km secrets.KeyManager, issuer *gittoken.Issuer, m *metrics.Metrics) *GitCredentialResolver
  func (g *GitCredentialResolver) ForAppSource(ctx context.Context, repoURL string) (gittemplate.Credential, error)
  func (g *GitCredentialResolver) ForUses(ctx context.Context, req gittemplate.CredentialRequest) (gittemplate.Credential, error)
  func (g *GitCredentialResolver) JobToken(ctx context.Context, gc *store.GitCredential) (gittoken.Token, error) // used by Task 5
  func (g *GitCredentialResolver) githubAppSource(gc *store.GitCredential, minRemaining time.Duration) gittoken.TokenSource
  func jobAccessCovers(folders []string, jobName string) bool // "*" covers all; else authz.Covers; root-level only by "*"
  func (s *Server) SetGitCredentialResolver(g *GitCredentialResolver)
  func RunGitResolver(ctx context.Context, st store.Store, resolver *gittemplate.Resolver, creds *GitCredentialResolver, tick, deadline time.Duration)
  func RunAppSourceReconciler(ctx context.Context, st store.Store, fetcher AppSourceFetcher, creds *GitCredentialResolver, tick time.Duration)
  // AppSourceFetcher methods take cred gittemplate.Credential like *Fetcher

  // metrics
  func (m *Metrics) GitCredentialToken(credential, purpose, result string) // unifiedcd_git_credential_tokens_total{credential,purpose,result}; purpose controller|job, result ok|denied|error
  func (m *Metrics) GitHubAppTokenRequest(credential, result string)     // unifiedcd_github_app_token_requests_total{credential,result}; result ok|error
  ```
  A nil `*GitCredentialResolver` returns an empty Credential from `ForAppSource`/`ForUses` (today's nil-km behaviour). API URL default when `GitHubApp.APIURL` is empty: `gittoken.DefaultAPIURL` for host `github.com`, else `https://<host>/api/v3`.

- [ ] **Step 1: Write the failing gittemplate tests**

```go
func TestResolveSpec_CredentialRequestCarriesOwnerRepo(t *testing.T) // credFn records CredentialRequest{Host:"github.com", Owner:"acme", Repo:"ci-templates"} for git://github.com/acme/ci-templates/x.yaml@v1
func TestResolveSpec_FetchErrorIncludesHint(t *testing.T) // credFn returns Hint "GitCredential ..."; fake fetcher errors → err message contains the hint
func TestCache_ScopePartitionsEntries(t *testing.T) {
	// Put(uri@v1, "gh-app", data); Get(uri, "") misses; Get(uri, "gh-app") hits; key has prefix "git_templates/v2/"
}
func TestBuildHTTPSURL_Username(t *testing.T) {
	assert.Equal(t, "https://x-access-token@github.com/acme/app.git", buildHTTPSURL(uri, Credential{Token: "t", Username: GitHubAppUser}))
	assert.Equal(t, "https://oauth2@github.com/acme/app.git", buildHTTPSURL(uri, Credential{Token: "t"}))
}
```
Update every existing caller/test of `Fetch`, `ResolveCommitSHA`, `FetchDir`, `Cache.Get/Put` and `CredentialFunc` to the new signatures in the same step.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/gittemplate/ -count=1`
Expected: build failure on the new signatures.

- [ ] **Step 3: Implement the gittemplate changes**

`resolveSteps` calls `credFn(ctx, CredentialRequest{Host: uri.Host, Owner: uri.Owner, Repo: uri.Repo})`; `Resolver.fetch` uses `cred.CacheScope` for `Get`/`Put`; on fetch error with `cred.Hint != ""` the error becomes `fmt.Errorf("...: %w; %s", err, cred.Hint)`. Cache key: `"git_templates/v2/" + base64url(sha256(scope + "\x00" + host/owner/repo/ref/path))` (the v2 prefix leaves legacy unscoped entries, which may hold PAT-fetched content, unreachable).

- [ ] **Step 4: Run the gittemplate tests**

Run: `go test ./internal/gittemplate/ -count=1`
Expected: PASS.

- [ ] **Step 5: Write the failing resolver, scheduler and metric tests**

```go
func TestJobAccessCovers(t *testing.T) {
	assert.True(t, jobAccessCovers([]string{"team-a"}, "team-a/build"))
	assert.True(t, jobAccessCovers([]string{"platform/tools"}, "platform/tools/x/y"))
	assert.False(t, jobAccessCovers([]string{"team-a"}, "team-ab/x"))
	assert.False(t, jobAccessCovers([]string{"team-a"}, "team-a"))      // a folder covers jobs inside it, not a job of that name
	assert.False(t, jobAccessCovers([]string{"team-a"}, "build"))       // root-level
	assert.True(t, jobAccessCovers([]string{"*"}, "build"))
	assert.False(t, jobAccessCovers(nil, "team-a/build"))
}
func TestGitCredResolver_TokenAndSSHUnchanged(t *testing.T) // token → Credential{Token, Username:"", CacheScope:"<name>"}; sshKey → SSHKey set
func TestGitCredResolver_AppSourceGitHubApp(t *testing.T) {
	// githubApp cred (fake GitHub) for repoURL https://github.com/acme/app.git → Credential.Token "ghs_1",
	// Username "x-access-token", CacheScope "gh-app"; fake saw Repositories ["app"], Permissions {"contents":"read"}; jobAccess not consulted
}
func TestGitCredResolver_AppSourceGitHubAppRequiresHTTPS(t *testing.T) // git@github.com:acme/app.git → error `GitCredential "gh-app" (githubApp) needs an https repoURL`
func TestGitCredResolver_UsesJobAccess(t *testing.T) {
	// table over (jobAccess, job, owner/repo) → token or anonymous:
	//  folders [team-a], repos [acme/*], perms {contents: read}:  team-a/build acme/ci → token (narrowed to ["ci"], contents:read)
	//  same: team-b/x → anonymous, Hint `GitCredential "gh-app" does not allow job "team-b/x" to read acme/ci through the GitHub App (jobAccess)`
	//  same: build (root) → anonymous; folders ["*"]: build → token
	//  same: team-a/build widget/ci → anonymous
	//  perms {statuses: write} (no contents) → anonymous
	//  no jobAccess → anonymous (and no GitHub call at all)
	//  anonymous result: Token "", SSHKey "", CacheScope ""
}
func TestGitCredResolver_ControllerUseFalseIgnored(t *testing.T) // only credential for host has controllerUse false → ForAppSource returns empty Credential
func TestResolveGitPendingRuns_PassesJobName(t *testing.T) // pending run of job "team-a/build" → ForUses sees JobName "team-a/build"
func TestResolveGitPendingRuns_TemplateCacheDoesNotLeakAcrossJobAccess(t *testing.T) {
	// fake Fetcher: succeeds only when cred.Token != ""; a fixed-ref uses: template.
	// run 1 (team-a/build, allowed) resolves and populates the cache; run 2 (team-b/x, same template) must NOT resolve:
	// its fetch is attempted anonymously and fails with the jobAccess hint.
}
func TestGitCredResolver_Metrics(t *testing.T) // controller hand-out → GitCredentialToken("gh-app","controller","ok") == 1; GitHub 500 → result "error"
```
`TestGitCredResolver_*` use `storetest.NewTestPostgres`, `testKeyManager(t)`, `gittokentest`, and a fixed clock.

- [ ] **Step 6: Run to verify they fail**

Run: `go test ./internal/controller/ -run 'TestJobAccessCovers|TestGitCredResolver|TestResolveGitPendingRuns' -count=1`
Expected: FAIL (undefined).

- [ ] **Step 7: Implement `gitcred_resolver.go`, the metric counters, and rewire the reconciler, scheduler and `main.go`**

One unexported `decryptSecret(ctx, name string) ([]byte, error)` serves token/sshKey material and the App private key (passed as `GitHubApp.PrivateKey`). `ForUses` returns anonymous for a `githubApp` credential unless `JobAccess != nil && jobAccessCovers(...) && dsl.RepositoryAllowed(...) && level(JobAccess.Permissions["contents"]) >= read`. `JobToken` builds `TokenRequest` from `dsl.ParseRepositoryPatterns(JobAccess.Repositories)` and `JobAccess.Permissions` with `gittoken.JobMinRemaining`. The scheduler's closure sets `req.JobName = r.JobName` and calls `creds.ForUses`. In `main.go`, create one `gittoken.NewIssuer(nil, nil)` with `OnRequest` feeding `m.GitHubAppTokenRequest`, one resolver, and pass it to `srv.SetGitCredentialResolver`, `RunGitResolver` and `RunAppSourceReconciler`.

- [ ] **Step 8: Add the env-gated real-installation test**

`TestGitHubAppRealInstallation` in `internal/controller/gitcred_github_integration_test.go`: skipped unless `UNIFIED_TEST_GITHUB_APP_ID`, `UNIFIED_TEST_GITHUB_APP_KEY_FILE` and `UNIFIED_TEST_GITHUB_APP_REPO` (`owner/repo`, private) are set. It issues a controller token through `gittoken` and runs `(*gittemplate.Fetcher).ResolveCommitSHA` on `https://github.com/<repo>.git` with `Username: x-access-token`; expects a 40-hex SHA. Without the variables: `go test ./internal/controller/ -run TestGitHubAppRealInstallation -v` prints `SKIP`.

- [ ] **Step 9: Run the controller, gittemplate and metrics suites**

Run: `go build ./... && go test ./internal/gittemplate/ ./internal/metrics/ ./internal/controller/ -count=1`
Expected: PASS.

- [ ] **Step 10: Commit**

Message `feat(controller): use GitHub App tokens for AppSource sync and uses: templates`.

---

### Task 5: Job-side token hand-out in the agent secrets fetch

**Files:**
- Modify: `internal/controller/api_secrets.go:75-143` (handleAgentSecretsFetch)
- Modify: `internal/controller/audit.go` (action constant), `internal/controller/api_agent_enrollment.go:596-615` (factor the row writer so it accepts details)
- Modify docs: `docs/operator-manual/audit.md` (new action, `details` column and its keys)
- Test: `internal/controller/api_secrets_test.go` (new fixture variant), `internal/controller/api_secrets_githubapp_test.go`

**Interfaces:**
- Consumes: Task 1 `GetGitCredential`, `ListGitCredentials`, `AuditEntry.Details`; Task 4 `GitCredentialResolver.JobToken`, `jobAccessCovers`, `Metrics.GitCredentialToken`.
- Produces:
  - `const auditActionGitCredentialTokenIssue = "gitcredential.token_issue"`
  - `func (s *Server) auditAgentEvent(r *http.Request, agentID, action, resource string, status int, details map[string]any)` — `auditAgentCredentialAs` becomes a call to it with nil details
  - Details keys: `runId`, `jobName`, `repositories` (jobAccess patterns), `permissions`, `expiresAt` (RFC 3339, success only), `reason` (denial or error only)
  - test helper `newSecretsFetchFixtureForJob(t *testing.T, jobName, specYAML string) (srv *Server, pg store.Store, agentID, runID string)` with the resolver wired to a `gittokentest.FakeGitHub` and a settable clock

- [ ] **Step 1: Write the failing tests**

```go
func TestAgentSecretsFetch_GitHubAppJobToken(t *testing.T) {
	// job "team-a/build", spec `run: echo {{ .Secrets.gh-app-release }}`; credential gh-app-release with
	// jobAccess{folders [team-a], repositories [acme/app], permissions {contents: write, statuses: write}}
	// → 200, Secrets["gh-app-release"] == "ghs_1"; fake saw Repositories ["app"], Permissions equal.
	// audit row: Action "gitcredential.token_issue", ActorID "agent:"+agentID, Resource "gh-app-release", Status 200,
	// Details {runId, jobName "team-a/build", repositories ["acme/app"], permissions {...}, expiresAt}; no audit column contains "ghs_1"
}
func TestAgentSecretsFetch_ParamSelectedCredentialName(t *testing.T) // `{{ index .Secrets .Params.token_secret }}` with token_secret=gh-app-release → token
func TestAgentSecretsFetch_GitHubAppFolderDenied(t *testing.T) {
	// job "team-b/x" → 403, body `gitcredential "gh-app-release": job "team-b/x" is not covered by jobAccess.folders`;
	// audit row Status 403, Details.reason "folder"; no GitHub POST
}
func TestAgentSecretsFetch_RootLevelJobNeedsStar(t *testing.T) // job "build": folders [team-a] → 403; folders ["*"] → 200
func TestAgentSecretsFetch_CredentialAsOfFetchTime(t *testing.T) // run created while allowed; folders changed to [other] before fetch → 403
func TestAgentSecretsFetch_NarrowedJobAccessGetsNewToken(t *testing.T) {
	// fetch → ghs_1 (perms contents:write); admin narrows to contents:read; second run fetch → ghs_2 with Permissions {"contents":"read"}
}
func TestAgentSecretsFetch_SharedTokenAcrossRuns(t *testing.T) // two runs, same credential, clock +5m → same "ghs_1", one POST; clock +11m → "ghs_2"
func TestAgentSecretsFetch_GitHubAppWithoutJobAccessNeverHandedOut(t *testing.T) // githubApp cred "gh-sync" without jobAccess, no secret of that name → 404 `secret gh-sync not found`; no GitHub call
func TestAgentSecretsFetch_PrivateKeyNeverDelivered(t *testing.T) {
	// spec references {{ .Secrets.github-app-key }} (the credential's secretRef) → 403
	// `secret "github-app-key" is the private key of GitHub App credential "gh-app-release" and is never delivered to jobs`;
	// body contains no "BEGIN" line
}
func TestAgentSecretsFetch_GitHubIssueFailure(t *testing.T) // fake 500 "boom" → 502, body contains "gh-app-release" and "boom", not the JWT; audit Status 502, reason "issue"
func TestAgentSecretsFetch_MixedSecretAndToken(t *testing.T) // NEEDED (StoredSecret) + gh-app-release → both values in one response
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/controller/ -run 'TestAgentSecretsFetch_' -count=1`
Expected: new tests FAIL; existing `TestAgentSecretsFetch_*` still PASS.

- [ ] **Step 3: Implement the hand-out in `handleAgentSecretsFetch`**

Order after the existing `allowed` check: load the githubApp private-key refs once (`ListGitCredentials`, filter `CredType == "githubApp"`, map `SecretRef → name`); for each requested name: (1) a private-key ref → 403 as above; (2) `GetGitCredential(name)` is a `githubApp` row with `JobAccess != nil` → `GetRun(runID).JobName`, `jobAccessCovers`, then `s.gitCreds.JobToken`; (3) otherwise the existing StoredSecret path, unchanged. A jobAccess credential wins over a same-named StoredSecret (only reachable through a create race; the collision rule prevents it otherwise). `s.gitCreds == nil` → 500 `git token issuing is not configured`. Audit every outcome of (2) with `auditAgentEvent` and count `GitCredentialToken(name, "job", ok|denied|error)`. Issue failures are 502 `gitcredential "<name>": <gittoken error>`.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/controller/ -run 'TestAgentSecretsFetch|TestAudit' -count=1`
Expected: PASS.

- [ ] **Step 5: Update `docs/operator-manual/audit.md`**

Add `gitcredential.token_issue` (actor `agent:<id>`, resource = credential name, written for 200, 403 and 502) and document the `details` column and its keys. Run: `grep -n "token_issue" docs/operator-manual/audit.md` → at least one hit.

- [ ] **Step 6: Commit**

Message `feat(controller): hand GitHub App tokens to jobs allowed by jobAccess, with audit`.

---

### Task 6: Templates

**Files:**
- Modify: `templates/github-commit-status.yaml` (param + header), `templates/github-pr-comment.yaml`, `templates/github-release.yaml`, `templates/git-checkout.yaml` (header comments only: mention a GitHub App credential name works as `token_secret`)
- Create: `templates/git-auth-run.yaml`
- Modify: `templates/README.md`, `internal/shim/corpus_test.go:311-312`, `internal/dsl/templates_parse_test.go` (if it lists expected templates)
- Test: `internal/dsl/template_git_auth_run_test.go` (Linux-only, real `sh` and `git`)

**Interfaces:**
- Consumes: Task 5 behaviour (credential name usable as a secret name); nothing from Go code.
- Produces:
  - `github-commit-status` param `token_secret` (string, default `github-token`); env `GITHUB_TOKEN: "{{ index .Secrets .Params.token_secret }}"`
  - `git-auth-run` params: `host` (default `github.com`), `credential_1`..`credential_4` and `owner_1`..`owner_4` (string, default `""`; slot 1 required), `command` (string, required). Fixed slots because `.Secrets` names must be statically resolvable per param (`dsl.ResolveSecretNameParams`).
  - Env per slot n: `GIT_AUTH_TOKEN_n: "{{ if .Params.credential_n }}{{ index .Secrets .Params.credential_n }}{{ end }}"`, `GIT_AUTH_OWNER_n: "{{ .Params.owner_n }}"`

- [ ] **Step 1: Write the failing template test**

```go
func TestGitAuthRunTemplate_RoutesTokenPerOwner(t *testing.T) {
	// skip on windows; skip if `git version` < 2.31
	// httptest.NewTLSServer: every request without Basic auth → 401 + WWW-Authenticate: Basic realm="x";
	//   with auth → record (path prefix "/acme/" or "/widget/", password) and return 404.
	// Parse templates/git-auth-run.yaml with dsl.ParseJobTemplate; take steps[0].run; run it with `sh -c` and env:
	//   GIT_AUTH_HOST=127.0.0.1:<port>, GIT_AUTH_OWNER_1=acme, GIT_AUTH_TOKEN_1=tok-acme,
	//   GIT_AUTH_OWNER_2=widget, GIT_AUTH_TOKEN_2=tok-widget, GIT_SSL_NO_VERIFY=1, HOME=<tempdir>,
	//   GIT_CONFIG_GLOBAL=<tempfile with `[credential] helper = "!f() { echo username=wrong; echo password=global-helper; }; f"`>,
	//   GIT_AUTH_COMMAND="git ls-remote https://127.0.0.1:<port>/acme/app.git; git ls-remote https://127.0.0.1:<port>/widget/lib.git; true"
	// assert recorded["/acme/"] == "tok-acme", recorded["/widget/"] == "tok-widget", no "global-helper" password seen;
	// assert no file under HOME contains "tok-acme" (nothing written to disk)
}
func TestGitAuthRunTemplate_EmptySlotsIgnored(t *testing.T) // only slot 1 set → GIT_CONFIG_COUNT adds entries for one owner only
func TestGitHubCommitStatusTemplate_TokenSecretParam(t *testing.T) // parsed template: param token_secret default "github-token"; env GITHUB_TOKEN references .Params.token_secret
```
The env names the test sets must match what the template's `env:` maps to (`GIT_AUTH_HOST` from `host`, `GIT_AUTH_COMMAND` from `command`).

- [ ] **Step 2: Run to verify it fails (in the Linux container)**

Run (container form from Global Constraints): `... go test ./internal/dsl/ -run 'TestGitAuthRunTemplate|TestGitHubCommitStatusTemplate' -count=1 -v`
Expected: FAIL (`templates/git-auth-run.yaml: no such file`).

- [ ] **Step 3: Write `templates/git-auth-run.yaml` and change `github-commit-status.yaml`**

Script (POSIX sh, interp-compatible; the corpus gate runs it): start `i=${GIT_CONFIG_COUNT:-0}`; append `credential.useHttpPath=true`; for each slot with a non-empty owner and token, append `credential.https://$GIT_AUTH_HOST/$owner.helper` = `""` (resets helpers inherited from system/global config for that URL) and then = `!f() { echo username=x-access-token; echo "password=$GIT_AUTH_TOKEN_n"; }; f` (the value holds the variable name, never the token); export `GIT_CONFIG_COUNT` and each `GIT_CONFIG_KEY_<i>`/`GIT_CONFIG_VALUE_<i>`; finally `exec sh -c "$GIT_AUTH_COMMAND"`. Fail with a clear message when `command` or slot 1 is empty. Header comment: why not `insteadOf` URLs (spec "Several organizations in one job"), git 2.31+ requirement, the `go mod tidy` example with `GOPRIVATE=github.com/acme,github.com/widget`, and prior art (`gh auth setup-git`, Git Credential Manager).

- [ ] **Step 4: Run the template tests and the corpus gate**

Run (container): `... go test ./internal/dsl/ -run 'TestGitAuthRunTemplate|TestGitHubCommitStatusTemplate|TestTemplatesParse' -count=1 -v`
Run (host): `go test ./internal/shim/ -run TestCorpus -count=1`
Expected: template tests PASS; TestCorpus fails with the new totals. Update `wantFilesWithJobDocs` and `wantTotalScripts` to the counts it reports (expected 56 and 96 when the new file has one `run:`), rerun, PASS.

- [ ] **Step 5: Update `templates/README.md`**

Add the `git-auth-run.yaml` row and the GitHub App note (a jobAccess GitCredential name works wherever a token secret name is asked for); change the `github-commit-status.yaml` row's secret column to "`token_secret` (default `github-token`)". Run: `grep -n "git-auth-run\|token_secret" templates/README.md` → hits for both.

- [ ] **Step 6: Commit**

Message `feat(templates): token_secret for github-commit-status and git-auth-run helper`. Note in the body that external references are pinned, so the new template and param arrive with the next tag.

---

### Task 7: User and operator docs, TODO

**Files:**
- Modify: `docs/user-guide/resources/git-credential.md`, `docs/user-guide/secrets.md`, `docs/operator-manual/authorization.md`, `docs/reference/json-schema.md` (only if it lists types), `docs/troubleshooting/templates-and-uses.md`, `docs/troubleshooting/steps-and-execution.md`, `docs/operator-manual/operations.md` (metrics, rate limits), `README.md:210` (GitCredential row), `TODO.md`

**Interfaces:**
- Consumes: the exact field names, error strings, metric names and audit action from Tasks 2–6.
- Produces: documentation only.

- [ ] **Step 1: `git-credential.md`**

New `githubApp` section with the spec's full YAML; `controllerUse` (all types, lookup-by-host semantics, most recently updated eligible credential wins); `jobAccess` (folders as authz folder rules, `"*"`, root-level jobs only by `"*"`, single owner, patterns, permission levels, no `jobAccess` = never handed to jobs); controller use (AppSource and `uses:` with a one-repository `contents: read` token; anonymous fallback for `uses:` and the hint text); GHES (`apiURL`, default `https://<host>/api/v3` for non-github.com hosts, no private-CA flag); installation lookup; the "Known drawbacks" list verbatim in substance; a worked "`go mod tidy` across two organizations" example using two credentials and `git-auth-run`.

- [ ] **Step 2: `secrets.md`, `authorization.md`, troubleshooting, operations, README**

`secrets.md`: credential names in `.Secrets`, the shared namespace and both 409 messages, the private-key secret never being delivered, 50–60 min token lifetime. `authorization.md`: `gitcredential.write` governs `jobAccess`; the folder check at fetch time. Troubleshooting entries with grep-able strings: `is not covered by jobAccess.folders`, `is the private key of GitHub App credential`, `does not allow job`, `no repository of the installation matches`, `is already used by a StoredSecret`. `operations.md`: `unifiedcd_git_credential_tokens_total`, `unifiedcd_github_app_token_requests_total`, rate-limit advice (separate Apps per purpose). README row mentions GitHub App.

- [ ] **Step 3: TODO.md open items**

Add under the fitting priority sections, numbered 57–61, each with Status/Impact/Evidence/Done when: tokens for runs longer than one hour; revoking tokens (needs per-run tokens); per-job narrowing of repositories and permissions; a private CA flag for GHES; other `TokenSource` providers (Azure DevOps, workload identity).

- [ ] **Step 4: Sweep for stale statements**

Run: `grep -rn "token\" or \"sshKey\"\|token | sshKey\|token|sshKey\|\"token\" | \"sshKey\"" docs/ templates/ README.md internal/ --include=*.md --include=*.go --include=*.yaml` and `grep -rn "github-token" docs/ templates/`
Expected: no remaining two-type enumerations outside historical specs/plans; every `github-token` mention for `github-commit-status` says it is the default of `token_secret`. Then `go test ./internal/dsl/ -run 'Templates|Examples' -count=1` → PASS.

- [ ] **Step 5: Final full run**

Run: `go build ./... && go vet ./... && go test ./... -count=1` and `cd web && npx vitest run`
Expected: PASS (Postgres tests via Docker; Linux-only template test skipped on Windows, already run in the container in Task 6).

- [ ] **Step 6: Commit**

Message `docs: GitHub App credentials, jobAccess and git-auth-run`.

---

## Self-review notes

- Spec coverage: Goals 1–5 → Tasks 2/1 (resource), 4 (controller use), 5 (job use), 2+5 (jobAccess ceiling), 5 (audit). Anti-goals → Global Constraints + Review Focus 2 and 3. Token issuing details and caches → Task 3. Installation lookup → Task 3. Controller-side bullets (shared function, CredentialFunc change, cache-hit note, x-access-token, integration test) → Task 4. Job-side bullets (fetch order, folder check at fetch time, 403, shared token, agents unchanged, commit-status param) → Tasks 5–6. Helper template → Task 6. Audit/metrics → Tasks 1, 4, 5. Storage/API/CLI/UI/GitOps → Tasks 1–2. Known drawbacks, Docs to update, Open items → Tasks 2, 5, 6, 7. No migration note: defaults keep existing behaviour; the template-cache key namespace change only costs one refetch.
- Rulings made where the spec is silent are listed in the hand-off report and marked in the steps (installation lookup by owner, `TokenRequest.Owner`, listing token, cache scope, private-key refusal, contents permission for `uses:`, non-empty folders, GHES default API URL, fixed helper slots, Fetcher signature change, credential-wins on a name race, https-only AppSource with githubApp).
