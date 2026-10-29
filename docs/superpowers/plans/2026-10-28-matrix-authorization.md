# Matrix Authorization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the global `viewer < developer < admin` role check with a Jenkins-style matrix of 25 named permissions, grantable to OIDC groups and users globally and per job folder, enforced by the server on every human route and reflected (never decided) by the Web UI.

**Architecture:** A new dependency-free package `internal/authz` owns the catalogue, role bundles, folder rule, grant sets and the compiled `oidc.permissions` policy. The controller computes one `authz.Grants` per request in `ServerAuth` (from the legacy role plus matching policy entries, intersected with a PAT ceiling) and a single route table (`internal/controller/authz_routes.go`) maps every registered `(method, pattern)` to a permission and scope; one inline middleware (`(*Server).authorize`) enforces it, with handler-level checks where the job name is only known after parsing the body. A golden test of today's role decisions lands in Task 2 and guards every later task; the switch from `requireMinRole` to the table happens in Task 6.

**Tech Stack:** Go 1.x, chi v5, pgx v5 / PostgreSQL (golang-migrate), gopkg.in/yaml.v3, testify; Svelte 5 + Vitest + @testing-library/svelte; cobra for the CLI.

**Spec:** `docs/superpowers/specs/2026-10-28-matrix-authorization-design.md` (binding; §-references below point into it). Read it fully before any task.

## Global Constraints

- All code, comments, commit messages, docs and error strings are English; no personal names, personal emails or home paths anywhere (AGENTS.md). Example identities use `example.com`.
- "A config with only `roleMap` / `userMap` / `defaultRole`, or no OIDC at all, must need no edit and must behave exactly as before. The deliberate exceptions — the token-management narrowing (§5.6) and the PAT-creation refusals of §5.14 / §5.15 — are each called out and listed in the migration guide."
- "No existing PAT, session or OIDC user may gain a permission it did not have before the operator wrote new config."
- "A grant that looks folder-scoped but is in fact global ... must fail at startup, not be 'helpfully' interpreted."
- "The UI may only hide; the server stays the authority."
- "A new route that nobody mapped to a permission must fail a test, not default to 'any authenticated human'."
- Identity matching uses exactly the values of `oidc.rolesClaim` (default `groups`), `email` and `sub` (`extractRoleValues`, `resolveRole`). `resolveRole` order is preserved bit-for-bit: `userMap` (email, then sub) → `roleMap` (highest rank) → `defaultRole` (`""`/`deny` ⇒ deny).
- Break-glass: the PAT named `env:UNIFIED_TOKEN` (`BootstrapPATName`) is always admin (all 25 permissions globally), checked before OIDC resolution.
- Agent principals (`AgentPrincipal`), `agentRouteIdentityMatrix` path binding, `claimed_by` and the secret-fetch fence are unchanged. Agent-or-human routes keep the agent bypass; only the human branch is checked.
- `oidc.permissions` is config-file only (no env var), read at startup (no hot reload).
- Out of scope (§2): explicit deny, `inherit: false`, `approvers:`, regex/glob patterns, folder-limited PAT ceilings, per-folder WebhookReceivers, Git/AppSource-sourced policy, UI policy editing, confining job code.
- Catalogue (25, §7.1, §5.18). Folder-scopable (9): `job.read`, `job.configure`, `job.delete`, `run.trigger`, `run.replay`, `run.cancel`, `run.delete`, `run.approve`, `schedule.configure`. Global-only (16): `secret.list`, `secret.write`, `gitcredential.read`, `gitcredential.write`, `vars.read`, `vars.write`, `webhook.read`, `webhook.write`, `appsource.read`, `appsource.write`, `agent.read`, `agent.manage`, `token.create`, `token.manage`, `audit.read`, `authz.read`.
- Bundles (§7.3): `viewer` = `job.read`, `vars.read`, `webhook.read`, `appsource.read`, `agent.read`; `developer` = viewer + `job.configure`, `job.delete`, `run.trigger`, `run.replay`, `run.cancel`, `run.delete`, `run.approve`, `schedule.configure`, `secret.list`, `token.create`; `admin` = all 25 (only bundle with `authz.read`).
- Folder rule (§5.2, §5.3): `effective(J) = global ∪ ⋃ folder(F) for every proper ancestor folder F of J`; `F` covers `J` iff `J` starts with `F + "/"`; byte-exact comparison; root-level jobs are covered by global grants only.
- One forward migration, no backfill (§7.5); `pats.role` and `sessions.role` lose `DEFAULT 'admin'` and stay `NOT NULL`.
- Exact strings (grep-able; never reword):
  - startup: the ten rows of the §5.4 table, e.g. `oidc.permissions.folders["team-a"].groups["my-org:team-a"]: unknown permission or role "jobs.read"`, `...: permission "secret.write" is global-only and cannot be granted on a folder`, `oidc.permissions.folders["team-a//x"]: invalid folder: segments must be non-empty and must not be "." or ".."`, `oidc.permissions.folders["team-*"]: invalid folder: folders are literal job-name prefixes, "*", "?", "[" are not allowed`, `oidc.permissions.folders["team a"]: invalid folder: whitespace is not allowed`, `oidc.permissions.folders: "team-a" and "/team-a/" are the same folder`, `oidc.permissions.folders["team-a"]: no groups or users`, `oidc.permissions.global.groups["my-org:x"]: empty grant`, `oidc.permissions: unknown field "deny"`, `oidc.permissions is set but oidc.issuer is not`; legacy: `oidc.roleMap["my-org:devs"]: unknown role "develper" is ignored (valid: admin, developer, viewer)`.
  - request: `forbidden: missing permission "run.trigger" on job "team-a/build"`, `forbidden: missing permission "secret.write"`, `forbidden: no role`, `forbidden: no role assigned`, `forbidden: token owner has no permissions`, `forbidden: no permissions`.
  - PAT creation: `cannot create a token: this login predates group-aware tokens; log out and log in again, then retry`; `cannot create a token: your OIDC token carries no "groups" claim; log in again so the token includes it, then retry` (claim name = configured `rolesClaim`); `cannot grant role "developer": it exceeds your global role "viewer"; use --permission to grant folder permissions you hold` (empty role: `it exceeds your global role (none)`); `cannot grant permission "secret.write": you do not hold it`.
  - explain: `user or groups is required`, `job and folder are mutually exclusive`.
  - break-glass left enabled (startup warning, §5.4): `UNIFIED_TOKEN is set while SSO is configured: the env:UNIFIED_TOKEN token is always admin and bypasses oidc.permissions; unset it once SSO works and keep it only as a break-glass credential`.
- Commit template (every commit step uses it; do not add `-s`, the message must end with exactly these two lines):

```bash
git -c user.name="<name given in the dispatch>" -c user.email="<email given in the dispatch>" commit -F - <<'EOF'
<subject line given in the step>

<one short paragraph: what and why>

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01G3NWwwXnjYtSfzJjZYTrnV
EOF
```

- Controller and store tests need a running Docker daemon (`store.NewTestPostgres`); never push.

## Review Focus

1. **Encoded or suffixed job paths** (`/api/v1/jobs/team-b%2Fx`, `/jobs/team-b/x/yaml`, `/jobs/team-a/x/schedulability`): authorization must check exactly the job name the handler then serves — pinned by `TestAuthorize_JobPathUsesHandlerNameDerivation` (Task 6).
2. **Existence oracle through run IDs**: for every job-from-run GET route, a hidden run must answer byte-for-byte like a nonexistent run (status and body), including a malformed run ID — pinned by `TestAuthorize_HiddenRunIndistinguishableFromMissing` (Task 6).
3. **`annotations.path` normalisation on apply** (`/team-b/`, `team-a//sub`, an empty path): the permission check must run on the same qualified name that `storeJob` writes, so a leading slash cannot escape a folder and repeated slashes cannot dodge it — pinned by `TestApplyJob_ChecksStoredQualifiedName` (Task 7).
4. **Case and look-alike folder names** (AppSource directories are not DNS-validated: `Team-A/x`, `team-a/x`): matching is byte-exact, a `team-a` grant does not cover `Team-A/x` — pinned by fixture cases `case-sensitive-folder-*` in `web/src/lib/authz-cases.json`, consumed by both `TestCanFixture` (Task 1) and `can() fixture` (Task 11).
5. **Bursts of 403s in a long-open tab** (several parallel requests failing after a config change): the UI must re-fetch `/auth/me` at most once per burst and never loop when the re-fetch itself fails — pinned by `apiFetch: concurrent 403s trigger a single /auth/me refetch` (Task 11).

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/authz/catalogue.go` (new) | `Permission`, the 25 constants, `All`, `Parse`, `FolderScopable`, `Bundle`, `RoleNames` |
| `internal/authz/set.go` (new) | `Set` bitset over the catalogue |
| `internal/authz/folder.go` (new) | `NormalizeFolder`, `Covers`, `Ancestors` |
| `internal/authz/grants.go` (new) | `Grants` (global + per-folder sets with provenance), `Decide`, `Explain`, `View`, `FromView`, `Restrict` |
| `internal/authz/policy.go` (new) | `PolicyConfig` YAML (strict, order-preserving), `Compile`, `Policy.Match`, `Policy.HasGroupKeyedGrants`, `Policy.HasFolderGrants` |
| `internal/authz/describe.go` (new, Task 10) | `Policy.Describe`, `ExplainIdentity` |
| `web/src/lib/authz-cases.json` (new) | Shared `(grants, permission, job) → bool` fixture (Go + Vitest) |
| `web/src/lib/ui-actions.json` (new) | Inventory of every state-changing UI call site |
| `internal/config/controller.go` | `ControllerOIDCConfig.Permissions`, compile in `ControllerEffective` |
| `internal/store/migrations/024_matrix_authorization.{up,down}.sql` (new) | Columns of §7.5 |
| `internal/store/store.go`, `postgres.go`, `verify.go` | PAT owner/ceiling, session claims, audit columns, lookups |
| `internal/api/types.go` | Wire fields for PATs, audit, `/auth/me`, policy view |
| `internal/controller/authz_routes.go` (new) | `routeTable`, `routeScope`, `lookupRoute` |
| `internal/controller/authz_decide.go` (new) | `authorize` middleware, `requireJob`, decision context, messages |
| `internal/controller/rbac.go` | `resolveRoleSource`, `identityGrants`; `requireMinRole` deleted in Task 6 |
| `internal/controller/auth.go`, `auth_oidc.go` | Principal grants, admission rules, claims on login/refresh, `/auth/me` |
| `internal/controller/api_tokens.go` | Owner/ceiling creation rules, own/all listing |
| `internal/controller/audit.go` | Decision fields, denied-read recording, `run.replay` |
| `internal/controller/api_authz.go` (new, Task 10) | `GET /authz/policy`, `POST /authz/explain` |
| `internal/cli/token.go`, `internal/cli/authz.go` (new) | `--permission`; `authz show` / `authz explain` |
| `web/src/lib/authz.js` (new), `web/src/lib/api.js` | `can`, `canAnywhere`, `ApiError`, `refreshMe`, 403 re-fetch |
| `web/src/App.svelte`, `web/src/routes/*.svelte` | Hide/disable controls, not-found view |

---

### Task 1: `internal/authz` core — catalogue, bundles, folders, grants (M)

**Files:**
- Create: `internal/authz/catalogue.go`, `internal/authz/set.go`, `internal/authz/folder.go`, `internal/authz/grants.go`, `web/src/lib/authz-cases.json`
- Test: `internal/authz/catalogue_test.go`, `internal/authz/folder_test.go`, `internal/authz/grants_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Permission string`; constants `JobRead`, `JobConfigure`, `JobDelete`, `RunTrigger`, `RunReplay`, `RunCancel`, `RunDelete`, `RunApprove`, `ScheduleConfigure`, `SecretList`, `SecretWrite`, `GitCredentialRead`, `GitCredentialWrite`, `VarsRead`, `VarsWrite`, `WebhookRead`, `WebhookWrite`, `AppSourceRead`, `AppSourceWrite`, `AgentRead`, `AgentManage`, `TokenCreate`, `TokenManage`, `AuditRead`, `AuthzRead` with the dotted names of Global Constraints.
  - `func All() []Permission` (catalogue order: the 9 folder-scopable then the 16 global-only, as listed in Global Constraints); `func Parse(name string) (Permission, bool)`; `func FolderScopable(p Permission) bool`; `var RoleNames = []string{"admin", "developer", "viewer"}`; `func Bundle(role string) (Set, bool)`.
  - `type Set uint32`; methods `Has(Permission) bool`, `With(...Permission) Set`, `Union(Set) Set`, `Intersect(Set) Set`, `FolderPart() Set`, `IsEmpty() bool`, `Names() []string` (sorted ascending); `func AllSet() Set`.
  - `func NormalizeFolder(key string) (string, error)` — trims leading/trailing `/`; errors (no prefix): `invalid folder: whitespace is not allowed`, `invalid folder: folders are literal job-name prefixes, "*", "?", "[" are not allowed`, `invalid folder: segments must be non-empty and must not be "." or ".."` (checked in that order; an all-slash or empty key is the segments error).
  - `func Covers(folder, job string) bool`; `func Ancestors(job string) []string` (`"a/b/c"` → `["a", "a/b"]`; `"x"` → `nil`).
  - `type Grants struct` (unexported fields) with `func (g *Grants) Add(folder string, perms Set, by string)` (`folder == ""` means global; a folder bundle must already be `FolderPart()`-reduced by the caller), `func (g Grants) Union(o Grants) Grants`, `Can(p Permission, job string) bool`, `Decide(p Permission, job string) (ok bool, grantedBy string)` (first source in insertion order, global sources before folder sources, outer folders before inner), `Explain(p Permission, job string) []string` (all sources), `HeldAnywhere(p Permission) bool`, `IsEmpty() bool`, `Restrict(ceiling Set, suffix string) Grants` (intersects global and every folder set; appends `suffix` to every grantedBy when non-empty), `View() GrantsView`.
  - `type GrantsView struct { Global []string \`json:"global"\`; Folders map[string][]string \`json:"folders"\` }` (never nil slices/maps; folders with an empty set are omitted); `func FromView(v GrantsView) (Grants, error)` (source string `view`).
  - `func RoleGrants(role, by string) Grants` (global bundle of `role`; empty for unknown/empty role); `func AllGrants(by string) Grants`.

- [ ] **Step 1: Write the failing tests**

`catalogue_test.go`:
```go
func TestCatalogueHas25PermissionsInOrder(t *testing.T) {
    all := All()
    require.Len(t, all, 25)
    folder := 0
    for i, p := range all { if FolderScopable(p) { folder++; require.Less(t, i, 9) } }
    require.Equal(t, 9, folder)
    require.Equal(t, AuthzRead, all[24])
}
func TestBundles(t *testing.T) {
    v, _ := Bundle("viewer"); d, _ := Bundle("developer"); a, _ := Bundle("admin")
    require.ElementsMatch(t, []string{"agent.read","appsource.read","job.read","vars.read","webhook.read"}, v.Names())
    require.Equal(t, v, d.Intersect(v)) // developer ⊇ viewer
    require.Len(t, d.Names(), 15)
    require.Equal(t, AllSet(), a)
    require.False(t, d.Has(AuthzRead)); require.False(t, d.Has(TokenManage)); require.False(t, d.Has(SecretWrite))
    _, ok := Bundle("deny"); require.False(t, ok)
}
func TestParseRejectsUnknown(t *testing.T) { _, ok := Parse("jobs.read"); require.False(t, ok) }
```
`folder_test.go`: `TestNormalizeFolder` table — `"/team-a/"→"team-a"`, `"team-a/sub"→"team-a/sub"`, `"Team_A"→"Team_A"` (no DNS validation), and the three errors for `"team-a//x"`, `"a/../b"`, `"."`, `"/"`, `"team-*"`, `"t?"`, `"t[1]"`, `"team a"`, `"team\ta"` (exact error text). `TestCovers`: `Covers("team-a","team-a/build")`, `Covers("team-a","team-a/sub/build")` true; `Covers("team-a","team-ab/build")`, `Covers("team-a","team-a")`, `Covers("team-a","Team-A/x")` false. `TestAncestors`.

`grants_test.go`:
```go
func TestCanFixture(t *testing.T) {
    b, err := os.ReadFile("../../web/src/lib/authz-cases.json"); require.NoError(t, err)
    var cases []struct{ Name string; Grants GrantsView; Permission string; Job string; Want bool }
    require.NoError(t, json.Unmarshal(b, &cases)); require.NotEmpty(t, cases)
    for _, c := range cases { g, err := FromView(c.Grants); require.NoError(t, err, c.Name)
        require.Equal(t, c.Want, g.Can(Permission(c.Permission), c.Job), c.Name) }
}
func TestDecideReportsFirstSourceAndExplainAll(t *testing.T) {
    var g Grants
    g.Add("", Set(0).With(JobRead), `role:viewer (roleMap["my-org:all"])`)
    g.Add("team-a", Set(0).With(JobRead, RunTrigger), `folder:team-a (groups["my-org:team-a"])`)
    ok, by := g.Decide(JobRead, "team-a/x"); require.True(t, ok); require.Equal(t, `role:viewer (roleMap["my-org:all"])`, by)
    require.Len(t, g.Explain(JobRead, "team-a/x"), 2)
    ok, _ = g.Decide(RunTrigger, "team-b/x"); require.False(t, ok)
    require.True(t, g.HeldAnywhere(RunTrigger)); require.False(t, g.Can(RunTrigger, ""))
}
func TestRestrictIntersectsEveryScopeAndSuffixes(t *testing.T) { /* Restrict(Set(0).With(JobRead), " & pat-ceiling") keeps job.read on team-a, drops run.trigger, grantedBy ends with " & pat-ceiling" */ }
func TestViewOmitsEmptyFoldersAndSortsNames(t *testing.T) { /* View() of g above: Global ["job.read"], Folders {"team-a": ["job.read","run.trigger"]}; empty Grants View is {Global: [], Folders: {}} */ }
```
`authz-cases.json` — at least these cases (name: grants → permission, job → want): `folder-covers-child` (team-a job.read; job.read, team-a/build → true), `folder-covers-grandchild` (team-a/sub/build → true), `folder-not-prefix-sibling` (team-ab/build → false), `folder-not-root-namesake` (team-a → false), `root-job-global-only` (global job.read; x → true), `root-job-folder-grant` (team-a job.read; x → false), `nested-folder` (team-a/prod run.approve; team-a/prod/deploy → true; team-a/deploy → false), `union-global-and-folder` (global job.read + team-a run.trigger; run.trigger team-a/x → true), `global-only-job-empty` (team-a job.read; job.read, "" → false), `case-sensitive-folder-miss` (team-a job.read; Team-A/x → false), `case-sensitive-folder-hit` (Team-A job.read; Team-A/x → true), `absent-permission` (team-a job.read; run.cancel team-a/x → false), `empty-grants` (no grants; job.read, x → false).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/authz/ -count=1`
Expected: FAIL — package has no non-test Go files / undefined: `All`.

- [ ] **Step 3: Implement the files listed under Interfaces.** `Set` bit `i` is `All()[i]`. `Grants` keeps `global Set`, `folders map[string]Set`, and an ordered `[]source{folder string; perms Set; by string}` for `Decide`/`Explain`. `Can(p, job)` checks global, then every `Ancestors(job)` key.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/authz/ -count=1 -race && go vet ./internal/authz/`
Expected: `ok  github.com/eirueimi/unified-cd/internal/authz`

- [ ] **Step 5: Commit** — `git add internal/authz web/src/lib/authz-cases.json`; subject `feat(authz): add the permission catalogue, bundles and folder grant sets`.

---

### Task 2: Route table, two-way classification test, legacy golden tests (M)

**Files:**
- Create: `internal/controller/authz_routes.go`
- Test: `internal/controller/authz_routes_test.go`, `internal/controller/authz_legacy_golden_test.go`

**Interfaces:**
- Consumes: `authz.Permission` constants, `authz.Bundle` (Task 1).
- Produces:
  - `type routeScope uint8` with constants `scopeGlobal`, `scopeJobPath`, `scopeJobRunID`, `scopeJobQuery`, `scopeJobBody`, `scopeScheduleBody`, `scopeSchedulePath`, `scopeFiltered`, `scopePublic`, `scopeSelf`, `scopeAgent`.
  - `type routeRule struct { Method, Pattern string; Perm authz.Permission; Scope routeScope; RunParam string; AgentBypass bool; FiltersRows bool }` — `Method` may be `"*"` (any method, for `HandleFunc` routes `/dex`, `/dex/*`, `/device/callback`, `/ui`, `/ui/*`); `RunParam` is `"id"` or `"runID"` for `scopeJobRunID`; `FiltersRows` marks `GET /api/v1/agents/{agentId}/runs` (global + filtered).
  - `var routeTable []routeRule` — one row per route of spec §7.2 (human, public/self, agent), patterns exactly as `chi.RouteContext(r.Context()).RoutePattern()` reports them (no trailing `/`, e.g. `/api/v1/secrets`, `/api/v1/jobs/*`). `GET /api/v1/jobs/*` covers job, `/yaml` and `/schedulability`; `GET /api/v1/runs` is `scopeJobQuery`.
  - `func lookupRoute(method, pattern string) (routeRule, bool)` (exact method first, then `"*"`).
  - `func normalizeRoutePattern(p string) string` — what `chi.Walk` reports, normalised the way `RoutePattern()` does (trim one trailing `/` unless the pattern is `/`).

- [ ] **Step 1: Write the failing tests**

`authz_routes_test.go`:
```go
func walkRoutes(t *testing.T) map[string]bool // chi.Walk over a server built with newTestServerWithConfig(t, Config{WebDir: t.TempDir()}); key "METHOD normalizedPattern"
func TestEveryRegisteredRouteIsClassified(t *testing.T) {
    for key := range walkRoutes(t) { m, p, _ := strings.Cut(key, " ")
        _, ok := lookupRoute(m, p)
        require.Truef(t, ok, "%s has no permission: classify it in the route table", key) }
}
func TestEveryRouteTableEntryIsRegistered(t *testing.T) { /* each row (Method "*" matched by any method) appears in walkRoutes */ }
func TestRouteTableShape(t *testing.T) {
    // human rows (not public/self/agent) carry a known Perm; folder scopes carry a FolderScopable Perm;
    // scopeGlobal rows carry a global-only Perm except GET /api/v1/agents/{agentId}/runs (FiltersRows);
    // scopeJobRunID rows set RunParam; no duplicate (Method, Pattern).
}
```
`authz_legacy_golden_test.go`:
```go
// Frozen from today's requireMinRole/viewerOrAgent registrations (server.go). "" = any authenticated human.
var legacyMinRole = map[string]string{ "GET /api/v1/audit": "admin", "POST /api/v1/jobs": "developer", /* ... every human row of spec §7.2 "Today" column ... */ "GET /api/v1/runs/{runID}/artifacts": "" }
// Permitted differences (spec §7.2, §5.6, §5.14, §5.15).
var legacyExempt = map[string]bool{"POST /api/v1/tokens": true, "GET /api/v1/tokens": true, "DELETE /api/v1/tokens/{id}": true}
// Routes that did not exist before this work (§5.18); Task 10 adds its two rows here.
var legacyNewRoutes = map[string]bool{}

func TestLegacyEquivalence_RouteTableAndBundles(t *testing.T) {
    // every human row is in legacyMinRole or legacyNewRoutes; for each non-exempt row and role in
    // viewer/developer/admin: bundle(role).Has(row.Perm) == (roleRank(role) >= roleRank(min))
}
func TestLegacyEquivalence_HTTP(t *testing.T) {
    // newTestServer + makeRolePAT for each role; for each non-exempt legacyMinRole row build a request
    // with placeholder params ({id},{runID} -> "00000000-0000-0000-0000-000000000000", {name},{agentId} -> "x",
    // {stepIndex} -> "0", "*" -> "x"), body `{}` for POST/PUT;
    // require.Equal(t, roleRank(role) < roleRank(min), rec.Code == http.StatusForbidden, key+" as "+role)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/controller/ -run 'TestEveryRegistered|TestEveryRouteTable|TestRouteTableShape|TestLegacyEquivalence' -count=1`
Expected: FAIL — undefined: `lookupRoute`.

- [ ] **Step 3: Implement `internal/controller/authz_routes.go`** with the table and helpers above. Nothing consumes the table at runtime yet.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/controller/ -run 'TestEveryRegistered|TestEveryRouteTable|TestRouteTableShape|TestLegacyEquivalence' -count=1 -race`
Expected: `ok  github.com/eirueimi/unified-cd/internal/controller`

- [ ] **Step 5: Commit** — `git add internal/controller/authz_routes.go internal/controller/authz_routes_test.go internal/controller/authz_legacy_golden_test.go`; subject `test(controller): classify every route and freeze today's role decisions`.

---

### Task 3: `oidc.permissions` config, compile and startup validation (L)

**Files:**
- Create: `internal/authz/policy.go`
- Modify: `internal/config/controller.go:97-117` (field), `:275-308` (merge + compile), `internal/controller/server.go:108-121` (`OIDCConfig.Policy`), `cmd/controller/main.go:414-427` (wire + log warnings), `examples/config/controller.yaml` (commented example)
- Test: `internal/authz/policy_test.go`, `internal/config/controller_permissions_test.go`

**Interfaces:**
- Consumes: Task 1 (`Parse`, `Bundle`, `NormalizeFolder`, `Grants.Add`, `Set.FolderPart`).
- Produces:
  - `type PolicyConfig struct` with `func (c *PolicyConfig) UnmarshalYAML(n *yaml.Node) error` — walks the node by hand so that unknown keys produce `<path>: unknown field "<key>"` (paths `oidc.permissions`, `oidc.permissions.global`, `oidc.permissions.folders["<raw key>"]`), keeps file order of folders, groups and users, accepts a grant as a scalar or a sequence of scalars, rejects duplicate mapping keys (`<path>: duplicate key "<key>"`) and any other node kind (`<path>: grant must be a role or permission name or a list of them`).
  - `type Legacy struct { RolesClaim string; RoleMap, UserMap map[string]string; DefaultRole string }`
  - `func Compile(cfg *PolicyConfig, legacy Legacy, issuer string) (*Policy, []string, error)` — returns a non-nil `*Policy` even when `cfg == nil` (empty policy); `warnings` holds the legacy unknown-role lines when `cfg == nil`; the same lines become the returned error when `cfg != nil` (first one, in sorted key order).
  - `type Identity struct { Values []string; Email, Sub string }`
  - `func (p *Policy) Match(id Identity) (authz.Grants, bool)` — nil-safe; `matched` is true when any entry matched, even if it grants nothing new. Sources: `global (groups["<k>"])`, `global (users["<k>"])`, `folder:<f> (groups["<k>"])`, `folder:<f> (users["<k>"])`. `users` keys match email **and** sub (both).
  - `func (p *Policy) Configured() bool`, `HasGroupKeyedGrants() bool` (any `permissions.*.groups` entry), `HasFolderGrants() bool`.
  - `config.ControllerOIDCConfig` gains `Permissions *authz.PolicyConfig \`yaml:"permissions"\``, `Policy *authz.Policy \`yaml:"-"\``, `PolicyWarnings []string \`yaml:"-"\``.
  - `controller.OIDCConfig` gains `Policy *authz.Policy`.

- [ ] **Step 1: Write the failing tests**

`policy_test.go` — `TestCompileStartupErrors`, a table with one YAML snippet per §5.4 row (decoded with `yaml.v3` into `struct{ Permissions *PolicyConfig \`yaml:"permissions"\` }` with `KnownFields(true)`, then `Compile(cfg, Legacy{}, issuer)`) asserting `require.EqualError` / `ErrorContains` with the exact string of Global Constraints for: unknown permission `jobs.read`; `secret.write` on folder `team-a`; folder `team-a//x`; `team-*`; `team a`; `team-a` and `/team-a/` duplicates; `team-a: {}`; `global.groups["my-org:x"]: []`; top-level `deny:`; issuer `""`. Plus:
```go
func TestCompileBundleOnFolderIsFolderPartOnly(t *testing.T) { /* folders.team-a.groups.g: admin → Match({Values:["g"]}) grants on team-a == authz.AllSet().FolderPart(); global empty */ }
func TestCompileExplicitAuthzReadOnFolderIsError(t *testing.T) { /* "authz.read" on a folder -> global-only error */ }
func TestMatchUsersMatchesEmailAndSubAdditively(t *testing.T) { /* users: {a@example.com: [job.read], sub-1: [run.trigger]} → identity {Email:a@example.com, Sub:sub-1} gets both */ }
func TestMatchUnmatchedIsFalse(t *testing.T) { g, ok := pol.Match(Identity{Email: "x@example.com"}); require.False(t, ok); require.True(t, g.IsEmpty()) }
func TestCompileLegacyUnknownRole(t *testing.T) {
    // cfg nil: no error, warnings == []string{`oidc.roleMap["my-org:devs"]: unknown role "develper" is ignored (valid: admin, developer, viewer)`}
    // cfg non-nil (any valid block): EqualError with the same string
}
func TestCompileNilConfigIsEmptyPolicy(t *testing.T) { p, w, err := Compile(nil, Legacy{}, ""); require.NoError(t, err); require.Empty(t, w); require.False(t, p.Configured()) }
```
`controller_permissions_test.go`:
```go
func TestControllerEffective_PermissionsBlockCompiles(t *testing.T) { /* the §5.4 example YAML -> cfg.OIDC.Policy.Configured(), HasFolderGrants() */ }
func TestControllerEffective_PermissionsWithoutIssuerFails(t *testing.T) { /* oidc: {permissions: {...}} only -> ErrorContains "oidc.permissions is set but oidc.issuer is not" */ }
func TestControllerEffective_IssuerFromEnvSatisfiesPermissions(t *testing.T) { /* t.Setenv("UNIFIED_OIDC_ISSUER", ...) + file with permissions only -> no error */ }
func TestControllerEffective_UnknownPermissionsFieldFails(t *testing.T) { /* ErrorContains `oidc.permissions: unknown field "deny"` */ }
func TestControllerEffective_LegacyOnlyConfigUnchanged(t *testing.T) { /* existing TestControllerEffective_RoleMapping YAML -> no error, PolicyWarnings empty, Policy not Configured */ }
```
`cmd/controller/main_test.go` (break-glass left enabled, spec §5.4):
```go
func TestBootstrapTokenWarning(t *testing.T) { /* (issuer "https://idp", token "t") -> the exact §5.4 string; (issuer "", token "t") -> ""; (issuer "https://idp", token "") -> "" */ }
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/authz/ ./internal/config/ ./cmd/controller/ -count=1`
Expected: FAIL — undefined: `PolicyConfig`, `Compile`, `bootstrapTokenWarning`.

- [ ] **Step 3: Implement** `policy.go`; in `ControllerEffective` copy `file.OIDC.Permissions` when non-nil, then after the OIDC merge (and before `validateControllerAgentAuth`) call `authz.Compile(eff.OIDC.Permissions, authz.Legacy{...}, eff.OIDC.Issuer)` when `eff.OIDC != nil` and store `Policy`/`PolicyWarnings`; return the error unwrapped. In `main.go` pass `Policy: eff.OIDC.Policy` into `controller.OIDCConfig`, `slog.Warn` each `PolicyWarnings` line verbatim, and when `Policy.Configured()` but `eff.OIDC.ClientID == ""` log `slog.Warn("oidc.permissions is set but oidc.clientId is not; no OIDC caller can authenticate")`. Add `func bootstrapTokenWarning(issuer, token string) string` in `cmd/controller/main.go` returning `UNIFIED_TOKEN is set while SSO is configured: the env:UNIFIED_TOKEN token is always admin and bypasses oidc.permissions; unset it once SSO works and keep it only as a break-glass credential` when both are non-empty and `""` otherwise, and `slog.Warn` it at startup when non-empty. Add a commented `permissions:` block (the §5.4 example) under the commented `oidc:` block of `examples/config/controller.yaml`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/authz/ ./internal/config/ ./cmd/controller/ -count=1 -race`
Expected: three `ok` lines.

- [ ] **Step 5: Commit** — `git add internal/authz/policy.go internal/authz/policy_test.go internal/config internal/controller/server.go cmd/controller/main.go cmd/controller/main_test.go examples/config/controller.yaml`; subject `feat(config): parse and validate oidc.permissions at startup`.

---

### Task 4: Migration 024 and store support (M)

**Files:**
- Create: `internal/store/migrations/024_matrix_authorization.up.sql`, `.down.sql`
- Modify: `internal/store/verify.go:31-55` (sentinel), `internal/store/store.go:97-104` (`PAT`), `:470-516` (interface), `:594-605` (`Session`), `internal/store/postgres.go:2411-2500`, `:2621-2660`, `:2723-2760`, `internal/api/types.go:383-405,600-609`, all `CreateSession` / `InsertAuditLog` call sites (`internal/controller/auth_oidc.go:273`, `audit.go:224`, `internal/controller/audit_passthrough_test.go:23`, store tests)
- Test: `internal/store/postgres_matrix_authz_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces (store):
  - `type PATOwner struct { Sub, Email string; Claims []string }`; `PAT` gains `Owner *PATOwner` (nil ⇔ `owner_sub IS NULL`) and `Permissions []string` (nil ⇔ NULL).
  - `type NewOwnedPAT struct { Name, TokenHash, Role string; Owner PATOwner; Permissions []string; ExpiresAt *time.Time }`; `CreateOwnedPAT(ctx context.Context, in NewOwnedPAT) (*PAT, error)` (nil `Owner.Claims` is stored as `'{}'`, never NULL).
  - `ListPATsByOwner(ctx context.Context, ownerSub string) ([]PAT, error)`; `DeleteOwnedPAT(ctx context.Context, id, ownerSub string) (bool, error)`; `UpdatePATOwnerSnapshot(ctx context.Context, ownerSub, email string, claims []string) (int64, error)`; `CountRoleOnlyPATsAboveViewer(ctx context.Context, excludeName string) (int, error)`.
  - `CreateSession(ctx, tokenHash, sub, email, role string, claims []string, refreshDEK, refreshCT []byte, expiresAt time.Time) (*Session, error)` (nil claims ⇒ NULL); `Session.Claims []string` (nil ⇔ NULL); `UpdateSessionClaims(ctx context.Context, id string, claims []string) error`.
  - `RunJobName(ctx context.Context, runID string) (job string, found bool, err error)` — a malformed UUID (SQLSTATE `22P02`) is `found=false, err=nil`.
  - `ScheduleJobName(ctx context.Context, name string) (job string, found bool, err error)`.
  - `type AuditEntry struct { Actor, Method, Path, Action, Resource string; Status int; Permission, Scope, GrantedBy string }`; `InsertAuditLog(ctx context.Context, e AuditEntry) error` (empty strings stored as NULL for the three new columns).
- Produces (api): `AuditLog` gains `Permission, Scope, GrantedBy string` (`json:"permission,omitempty"` etc.); `CreatePATRequest` and `CreatePATResponse` gain `Permissions []string \`json:"permissions,omitempty"\``; `PATMeta` gains `Owner string \`json:"owner,omitempty"\`` (owner email, else sub) and `Permissions []string \`json:"permissions,omitempty"\``.

Migration up (exact):
```sql
ALTER TABLE pats ADD COLUMN owner_sub text, ADD COLUMN owner_email text,
  ADD COLUMN owner_claims text[], ADD COLUMN permissions text[];
ALTER TABLE pats ALTER COLUMN role DROP DEFAULT;
CREATE INDEX pats_owner_sub_idx ON pats (owner_sub) WHERE owner_sub IS NOT NULL;
ALTER TABLE sessions ADD COLUMN claims text[];
ALTER TABLE sessions ALTER COLUMN role DROP DEFAULT;
ALTER TABLE audit_logs ADD COLUMN permission text, ADD COLUMN scope text, ADD COLUMN granted_by text;
```
Down reverses it (`SET DEFAULT 'admin'`, drop index and columns). Header comment explains: no backfill, `owner_sub IS NULL` ⇔ role-only, `claims IS NULL` ⇔ pre-upgrade. Sentinel: `{version: 24, migration: "024_matrix_authorization", table: "audit_logs", column: "granted_by"}`.

- [ ] **Step 1: Write the failing tests** (`postgres_matrix_authz_test.go`, all with `NewTestPostgres`):
```go
func TestMigration024DropsRoleDefaults(t *testing.T) { /* information_schema.columns column_default IS NULL for pats.role and sessions.role; is_nullable = 'NO' */ }
func TestOwnedPATRoundTrip(t *testing.T) { /* CreateOwnedPAT(Role:"", Owner{Sub:"s1",Email:"a@example.com",Claims:nil}, Permissions:["job.read"]) -> GetPATByHash: Owner.Claims == []string{} (non-nil), Permissions == ["job.read"], Role == "" */ }
func TestLegacyPATHasNoOwner(t *testing.T) { /* CreatePAT(...) -> Owner == nil, Permissions == nil */ }
func TestDeleteOwnedPATOnlyDeletesOwn(t *testing.T) { /* other owner -> false and row kept; same owner -> true; role-only row with ownerSub "" -> false */ }
func TestUpdatePATOwnerSnapshot(t *testing.T) { /* two PATs of s1, one of s2 -> returns 2; email and claims updated */ }
func TestCountRoleOnlyPATsAboveViewer(t *testing.T) { /* role-only viewer, developer, admin, bootstrap admin, owned developer -> excludeName bootstrap => 2 */ }
func TestSessionClaimsNullVsEmpty(t *testing.T) { /* nil -> Claims == nil; []string{} -> non-nil empty; UpdateSessionClaims sets ["g1"] */ }
func TestRunJobName(t *testing.T) { /* existing run -> job,true; unknown uuid -> false,nil; "not-a-uuid" -> false,nil */ }
func TestScheduleJobName(t *testing.T) { /* existing -> target,true; missing -> false,nil */ }
func TestAuditLogDecisionColumns(t *testing.T) { /* InsertAuditLog with Permission/Scope/GrantedBy -> ListAuditLogs returns them; empty -> "" */ }
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/store/ -run 'Migration024|OwnedPAT|LegacyPATHasNoOwner|DeleteOwnedPAT|UpdatePATOwner|CountRoleOnly|SessionClaims|RunJobName|ScheduleJobName|AuditLogDecision|TestSchemaSentinelsCoverAllMigrations' -count=1`
Expected: FAIL — compile errors (undefined `CreateOwnedPAT`, ...).

- [ ] **Step 3: Implement** the migration, sentinel, types and methods; update every caller of the two changed signatures to pass `nil` claims / an `AuditEntry` with only the old fields, so behaviour is unchanged.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go test ./internal/store/ ./internal/controller/ -count=1 -race`
Expected: `ok` for both packages (existing controller tests unchanged).

- [ ] **Step 5: Commit** — `git add internal/store internal/api/types.go internal/controller`; subject `feat(store): record PAT owners, session claims and audit decisions`.

---

### Task 5: Principals carry grants; admission, claims capture, `/auth/me` (L)

**Files:**
- Modify: `internal/controller/auth.go:21-178`, `internal/controller/rbac.go`, `internal/controller/auth_oidc.go:124-147,178-266,302-361`, `internal/controller/server.go:124-160` (hook field), `:606` (`/auth/me` under `ServerAuth`)
- Test: `internal/controller/auth_grants_test.go`, `internal/controller/authz_testutil_test.go` (new helper), existing `auth_oidc_test.go` (`TestHandleMe_*` keep passing)

**Interfaces:**
- Consumes: `authz.Grants`, `RoleGrants`, `AllGrants`, `Policy.Match`, `HasGroupKeyedGrants` (Tasks 1, 3); `PAT.Owner`, `Session.Claims`, `UpdateSessionClaims`, `UpdatePATOwnerSnapshot`, `CreateSession(... claims ...)` (Task 4).
- Produces:
  - `Principal` gains `Sub, Email string`, `Claims []string`, `ClaimsKnown bool`, `Grants authz.Grants`, `Ceiling *authz.Set` (nil = none), `OwnerPATID string` (set for owner PATs), `BreakGlass bool`. `Role` stays the legacy global role (may be `""`).
  - `func resolveRoleSource(values []string, email, sub string, m RoleMapping) (role, source string, ok bool)` — `resolveRole` becomes a wrapper (unchanged results; `TestResolveRole` untouched). Sources: `userMap["<email>"]`, `userMap["<sub>"]`, `roleMap["<value>"]`, `defaultRole`.
  - `func (s *Server) identityGrants(values []string, email, sub string) (role string, g authz.Grants, admitted bool)` — `g = RoleGrants(role, "role:<role> (<source>)") ∪ policy.Match(...)`; `admitted = legacyOK || matched`.
  - `func ceilingSet(role string, perms []string) *authz.Set` — `perms != nil` → that set; else `role != ""` → bundle; else nil.
  - Server field `verifyIDTokenHook func(ctx context.Context, raw, audience string) (map[string]any, error)`; when nil, the real go-oidc verification runs. Used by the bearer branch, the login callback and silent refresh.
  - `func (s *Server) completeOIDCLogin(ctx context.Context, claims map[string]any, refreshToken string) (sessionToken string, expiresAt time.Time, status int, msg string)` — extracted from `handleOIDCCallback` after claims parsing; stores `claims` as `extractRoleValues(...)` normalised to non-nil `[]string{}`; calls `UpdatePATOwnerSnapshot(sub, email, values)`; refuses with `403 forbidden: no role assigned` only when not admitted.
  - `func (s *Server) refreshedClaims(ctx context.Context, rawIDToken string) ([]string, bool)` — verifies the refreshed id_token (browser `ClientID` audience); `ok=false` when absent, unverifiable, or the `rolesClaim` key is missing. `refreshSession` returns `([]string, bool, error)` and calls `UpdateSessionClaims` when `ok`.
  - `/auth/me` response (JSON): `{"sub","email","name","kind","role","permissions": authz.GrantsView}`; `sub`/`email` omitted when empty.

Admission rules (spec §5.15, exact): break-glass → `AllGrants("break-glass")`, `Role: "admin"`, `BreakGlass: true`. Role-only PAT (`Owner == nil`): `roleRank(pat.Role) == 0` → `403 forbidden: no role`; else `RoleGrants(pat.Role, "role:<r> (pat)")`, `ClaimsKnown false`. Owner PAT: `identityGrants(owner.Claims, owner.Email, owner.Sub)` then `Restrict(*ceiling, " & pat-ceiling")` when a ceiling exists, `Role` = owner's legacy role clamped to the ceiling role's rank when the ceiling is a role and `""` when it is a permission list; admitted whatever `pat.Role` is. Bearer: claims via hook/verifier; `values := extractRoleValues(...)`; `ClaimsKnown = values != nil || !(policy.HasGroupKeyedGrants() || len(RoleMap) > 0)`; not admitted → `403 forbidden: no role`. Session with `Claims != nil`: `identityGrants(sess.Claims, sess.Email, sess.Sub)` (role recomputed from stored claims and current config), admitted even when `sess.Role == ""`; with `Claims == nil`: role-only from `sess.Role` (`"role:<r> (session)"`), `roleRank == 0` → `403 forbidden: no role`. `ServerAuth(st, nil)` keeps working (role-only principals only).

- [ ] **Step 1: Write the failing tests**

`authz_testutil_test.go` (shared by later tasks):
```go
// newAuthzTestServer builds a KM-enabled test server with OIDC configured (issuer https://idp.example.com,
// clientId unified-cd, rolesClaim groups), the given legacy maps and permissions YAML compiled via authz.Compile,
// and verifyIDTokenHook accepting raw tokens "idtoken:<email>|<g1,g2>" (claims sub=email, email, groups;
// "idtoken:<email>|-" omits the groups claim).
func newAuthzTestServer(t *testing.T, legacy authz.Legacy, permissionsYAML string) (*Server, store.Store)
func bearer(email string, groups ...string) string
func doAs(t *testing.T, s *Server, method, path, token string, body []byte) *httptest.ResponseRecorder
```
`auth_grants_test.go`:
```go
func TestServerAuth_BreakGlassHoldsAll25(t *testing.T)            // principal.Grants.Can(AuthzRead,"") && HeldAnywhere for all 25
func TestServerAuth_RoleOnlyPATEmptyRoleIsNoRole(t *testing.T)   // CreatePAT role "" -> 403 body "forbidden: no role\n"
func TestServerAuth_OwnerPATEmptyRoleAdmitted(t *testing.T)      // CreateOwnedPAT role "" owner matched by folder grant -> admitted, Grants.Can(JobRead,"team-a/x")
func TestServerAuth_OwnerPATCeilingRestricts(t *testing.T)       // owner developer via roleMap, ceiling ["job.read"] -> !Can(RunTrigger,"x"); grantedBy ends " & pat-ceiling"
func TestServerAuth_OwnerPATFollowsCurrentConfig(t *testing.T)   // same PAT row, two servers: owner group in roleMap -> developer; removed -> IsEmpty()
func TestServerAuth_SessionWithClaimsEmptyRoleAdmitted(t *testing.T)
func TestServerAuth_SessionWithoutClaimsEmptyRoleIsNoRole(t *testing.T) // 403 "forbidden: no role"
func TestServerAuth_BearerDeniedByLegacyButMatchedByFolderIsAdmitted(t *testing.T) // defaultRole deny + folder grant -> Role "" admitted
func TestServerAuth_BearerMatchedByNothingIsNoRole(t *testing.T)
func TestServerAuth_BearerClaimsKnown(t *testing.T) // groups absent + roleMap entry -> ClaimsKnown false; groups absent + userMap-only -> true, Claims == []string{}
func TestIdentityGrants_UserMapDemotionDoesNotSuppressPermissions(t *testing.T) // userMap viewer + roleMap admin group + folder grant on group -> Role viewer, folder grant present
func TestCompleteOIDCLogin_StoresClaimsAndRefreshesPATOwners(t *testing.T) // session.Claims == ["g1"]; owned PAT of same sub has Owner.Claims ["g1"]
func TestCompleteOIDCLogin_DeniedOnlyWhenNothingMatches(t *testing.T) // status 403, msg "forbidden: no role assigned"
func TestRefreshedClaims_UpdatesOnlyWhenClaimPresent(t *testing.T)
func TestHandleMe_ReturnsCallerGrantsOnly(t *testing.T) // folder-only bearer: permissions.folders has "team-a" only, not "team-b"; kind "oidc"; role ""
func TestHandleMe_LegacyPATShape(t *testing.T) // viewer PAT: role "viewer", global == viewer bundle names, folders {}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/controller/ -run 'TestServerAuth_|TestIdentityGrants_|TestCompleteOIDCLogin_|TestRefreshedClaims_|TestHandleMe_' -count=1`
Expected: FAIL — undefined: `newAuthzTestServer` / `Principal.Grants`.

- [ ] **Step 3: Implement** the interfaces above. `requireMinRole` still enforces routes, so admitted role-less principals are still refused by it (403) until Task 6.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/controller/ -count=1 -race`
Expected: `ok` (includes `TestLegacyEquivalence_HTTP` and all existing auth tests).

- [ ] **Step 5: Commit** — `git add internal/controller`; subject `feat(controller): compute permission grants for every authenticated principal`.

---

### Task 6: Switch-over — `authorize` middleware and per-route enforcement (L)

**Files:**
- Create: `internal/controller/authz_decide.go`
- Modify: `internal/controller/server.go:375-394,448-561,606-619` (every `requireMinRole`/`viewerOrAgent` replaced by `s.authorize`; `requiredRole` removed from `agentIdentityRoute`), `internal/controller/agent_auth.go:136-150` (delete `viewerOrAgent`), `internal/controller/rbac.go:94-109` (delete `requireMinRole`), tests that referenced them (`rbac_test.go` keeps `TestRoleRank`)
- Test: `internal/controller/authz_enforcement_matrix_test.go`, `internal/controller/authz_decide_test.go`

**Interfaces:**
- Consumes: `routeTable`, `lookupRoute` (Task 2); `Principal.Grants` (Task 5); `RunJobName`, `ScheduleJobName` (Task 4).
- Produces:
  - `type authzDecision struct { Permission authz.Permission; Scope, GrantedBy string; Denied bool; Status int }`; `func withDecisionSlot(r *http.Request) (*http.Request, *authzDecision)`; `func recordDecision(r *http.Request, d authzDecision)` (no-op without a slot).
  - `func (s *Server) authorize(next http.Handler) http.Handler` — inline (`r.With(...)`) middleware; looks up `lookupRoute(r.Method, chi.RouteContext(r.Context()).RoutePattern())`; unclassified → `slog.Error` + `403 forbidden: unclassified route` (fail closed).
  - `func (s *Server) requireJob(w http.ResponseWriter, r *http.Request, perm authz.Permission, job string, hide func(http.ResponseWriter)) bool` — no `job.read` on `job` → record deny, call `hide` (when nil, answer like a readable job: 403); has `job.read` but not `perm` → record deny, `403 forbidden: missing permission "<p>" on job "<job>"`; else record allow with `Decide`'s grantedBy.
  - `func (s *Server) canReadJob(r *http.Request, job string) bool` — true for agent principals.
  - `func jobNameFromWildcard(r *http.Request) string` — the single derivation shared by the middleware and `handleGetJobOrYAML`/`handleDeleteJob` (strip leading `/`, then a trailing `/schedulability` or `/yaml` for GET).
  - Messages: `func missingPerm(p authz.Permission) string`, `func missingPermOnJob(p authz.Permission, job string) string`; consts `msgNoPermissions = "forbidden: no permissions"`, `msgTokenOwnerNoPermissions = "forbidden: token owner has no permissions"`.
  - Hidden bodies: runs → `store.ErrRunNotFound.Error()` (404); jobs by path → exactly the body `serveJob` writes for a missing job (404).

`authorize` algorithm (the part signatures do not determine):
```
rule := lookupRoute(...)
if agent principal: rule.AgentBypass ? next : 403 missingPerm(rule.Perm)
p := principal; if p.Grants.IsEmpty(): 403 (OwnerPATID != "" ? msgTokenOwnerNoPermissions : msgNoPermissions)
if !p.Grants.HeldAnywhere(rule.Perm): record deny; 403 missingPerm(rule.Perm)     // cheap pre-check, before any body/DB read
switch rule.Scope:
  scopeGlobal:        Decide(perm,"") or 403 missingPerm
  scopeJobPath:       requireJob(perm, jobNameFromWildcard(r), hideJob)
  scopeJobQuery:      job := query "jobName"; job == "" ? next (handler 400) : requireJob(perm, job, hideJob)
  scopeJobRunID:      global fast path when Grants.Can(perm,"") && Grants.Can(job.read,""):
                        GET → next without lookup; other methods → best-effort RunJobName for Scope, then next
                      else job, found, err := RunJobName(param); err → 500; !found → 404 run-not-found; requireJob(perm, job, hideRun)
  scopeSchedulePath:  global fast path when Can(perm,""); else ScheduleJobName: !found → next (idempotent delete); found → requireJob(perm, job, nil)
  scopeJobBody, scopeScheduleBody, scopeFiltered: next (handlers decide; Task 7)
```

- [ ] **Step 1: Write the failing tests**

`authz_enforcement_matrix_test.go` (uses `newAuthzTestServer`; seeds jobs `team-a/x`, `team-a/sub/x`, `team-b/x`, `team-ab/x`, `x`, one terminal run each, schedule `sch` → `team-a/x`; a `requestFor(rule, job, runID)` table builds path and body per row):
```go
func TestEveryHumanRouteRefusesCallerWithoutItsPermission(t *testing.T) {
    // for each human row: principal "not-<perm>@example.com" with all 25 except rule.Perm globally ->
    // refused: 403, or 404 whose body is the hidden-run/job body when rule.Perm == job.read on a folder scope
}
func TestEveryHumanRouteAdmitsCallerWithItsPermission(t *testing.T) {
    // principal "only-<perm>@example.com" with rule.Perm globally (+ job.read for folder-scopable perms
    // other than job.read, because §5.7 makes job.read a precondition) -> not 403 and not the hidden 404 body
}
func TestFolderGrantMatrix(t *testing.T) {
    // for every folder-scopable path/query/run-ID row: grant developer on team-a only ->
    // team-a/x and team-a/sub/x admitted; team-b/x, team-ab/x, x -> 404 (job.read missing)
    // grant [job.read] on team-a only + action row -> 403 with exact missingPermOnJob text
}
func TestAuthorize_HiddenRunIndistinguishableFromMissing(t *testing.T) {
    // for every scopeJobRunID GET row: team-a reader asking about team-b's run and about a random UUID and
    // about "not-a-uuid" -> identical status and body  (Review Focus 2)
}
func TestAuthorize_JobPathUsesHandlerNameDerivation(t *testing.T) {
    // team-a reader: GET /api/v1/jobs/team-b/x/yaml -> 404; /jobs/team-b%2Fx -> 404;
    // /jobs/team-a/x/schedulability -> 200; DELETE /jobs/team-b/x by team-a developer -> 404  (Review Focus 1)
}
func TestAuthorize_EmptyGrantSetMessages(t *testing.T) {
    // owner PAT whose owner matches nothing -> 403 "forbidden: token owner has no permissions\n" on GET /api/v1/jobs and GET /api/v1/secrets;
    // session with claims matching nothing -> "forbidden: no permissions\n"
}
func TestAuthorize_GlobalOnlyMessage(t *testing.T) { /* developer PAT POST /api/v1/secrets -> body `forbidden: missing permission "secret.write"` */ }
func TestAuthorize_AgentBypassUnchanged(t *testing.T) { /* agent principal GET /api/v1/runs/{id}, /outputs, /artifacts -> not 403; agent on /api/v1/jobs -> 401 as today */ }
func TestAuthorize_ApprovalCheckedAgainstCurrentJobName(t *testing.T) { /* RenameJob x -> team-a/x; team-a developer can decide; team-b developer -> 404 */ }
```
`authz_decide_test.go`: `TestAuthorize_UnclassifiedRouteFailsClosed` (a router built in the test with one unlisted route wrapped in `s.authorize` → 403 `forbidden: unclassified route`).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/controller/ -run 'TestEveryHumanRoute|TestFolderGrantMatrix|TestAuthorize_' -count=1`
Expected: FAIL — undefined: `(*Server).authorize`.

- [ ] **Step 3: Implement `authz_decide.go` and switch every registration in `server.go`.** Order on each route: `ServerAuth` (or `agentOrServerAuth`) → existing `auditLogMiddleware` where present → `s.authorize` → handler. Remove `requiredRole`; agent-or-human matrix rows get `s.authorize` (the table marks them `AgentBypass`). Make `handleGetJobOrYAML` and `handleDeleteJob` use `jobNameFromWildcard`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go vet ./internal/controller/ && go test ./internal/controller/ -count=1 -race`
Expected: `ok` — including `TestLegacyEquivalence_HTTP` and `TestLegacyEquivalence_RouteTableAndBundles` unchanged, and `grep -rn "requireMinRole\|viewerOrAgent" internal/` prints nothing.

- [ ] **Step 5: Commit** — `git add internal/controller`; subject `feat(controller): enforce the route permission table instead of minimum roles`.

---

### Task 7: Body-scoped checks and list filtering (M)

**Files:**
- Modify: `internal/controller/api_jobs.go:22-62,104-125`, `internal/controller/api_runs.go:22-42,271-300`, `internal/controller/api_schedules.go:14-85`, `internal/controller/api_agents.go:36-47`
- Test: `internal/controller/authz_body_filter_test.go`

**Interfaces:**
- Consumes: `requireJob`, `canReadJob` (Task 6); `ScheduleJobName` (Task 4).
- Produces: `func (s *Server) filterRuns(r *http.Request, runs []api.Run) []api.Run`; `storeJob` takes the qualified name computed once by the caller: `func (s *Server) storeJob(ctx context.Context, job *dsl.Job, name string) (*api.Job, error)`.

Rules: **apply job** — parse → `name := job.Metadata.QualifiedName()` → `requireJob(job.configure, name, nil)` (403 regardless of readability; §5.3) → `guardManagedResource` → `storeJob(ctx, job, name)`. **trigger** — decode → `requireJob(run.trigger, req.JobName, hide = 404 "job not found: <name>")`. **schedule apply** — parse → `requireJob(schedule.configure, sc.Spec.Job, hide = 400 "job not found: <job>")` → if `ScheduleJobName(name)` found and differs → `requireJob(schedule.configure, old, nil)` → guard → existing logic. **lists** — `GET /jobs`, `GET /runs/active`, `GET /agents/{agentId}/runs` (`filterRuns`), `GET /schedules` (by `JobName`) drop rows the caller cannot read; agent principals see everything.

- [ ] **Step 1: Write the failing tests**
```go
func TestApplyJob_ChecksStoredQualifiedName(t *testing.T) {
    // team-a developer: path "team-a//sub" -> 200 and the stored name is covered by team-a;
    // path "/team-b/" -> 403 `forbidden: missing permission "job.configure" on job "team-b/<name>"`;
    // no path annotation -> 403 on root "<name>"  (Review Focus 3)
}
func TestApplyJob_PermissionBeforeManagedGuard(t *testing.T) {
    // managed job team-b/x (AppSource "pipelines" lists it): team-a developer -> 403 and body has no "pipelines" or repo URL;
    // team-b developer -> 409 with the existing guard string; allowManualOverride -> 200
}
func TestDeleteJob_ManagedHiddenIs404WithoutRepoURL(t *testing.T)
func TestTriggerRun_HiddenJobLooksMissing(t *testing.T) // team-a dev on team-b/x and on "nope" -> both 404 "job not found: ..." same text pattern
func TestSchedule_RetargetNeedsBothFolders(t *testing.T) // sch -> team-b/x existing; team-a dev re-targets to team-a/x -> 403 on "team-b/x"; team-b+team-a dev -> 200
func TestSchedule_DeleteChecksCurrentTarget(t *testing.T) // team-a dev deleting team-b's schedule -> 403; missing schedule -> 204
func TestFilter_ListJobs(t *testing.T)        // team-a reader sees team-a/x, team-a/sub/x only
func TestFilter_ActiveRuns(t *testing.T)
func TestFilter_AgentRuns(t *testing.T)
func TestFilter_Schedules(t *testing.T)
func TestRunsByJobQuery_Hidden404(t *testing.T) // GET /api/v1/runs?jobName=team-b/x by team-a reader -> 404
```
Extend `TestRouteTableShape` (Task 2 file): every `scopeFiltered` or `FiltersRows` row maps to one of these `TestFilter_*` functions in a `filterTests map[string]func(*testing.T)` (compile-time reference).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/controller/ -run 'TestApplyJob_|TestDeleteJob_Managed|TestTriggerRun_Hidden|TestSchedule_|TestFilter_|TestRunsByJobQuery|TestRouteTableShape' -count=1`
Expected: FAIL — e.g. `TestApplyJob_ChecksStoredQualifiedName`: expected 403, got 200.

- [ ] **Step 3: Implement** the rules above.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/controller/ -count=1 -race`
Expected: `ok`.

- [ ] **Step 5: Commit** — `git add internal/controller`; subject `feat(controller): check body-scoped jobs and filter lists by job.read`.

---

### Task 8: PAT creation, ownership and ceilings; CLI `--permission`; startup warning (L)

**Files:**
- Modify: `internal/controller/api_tokens.go:27-108`, `internal/cli/token.go:25-110`, `cmd/controller/main.go` (call the warning after the store opens)
- Create: `internal/controller/pat_warning.go`
- Test: `internal/controller/api_tokens_owner_test.go`, `internal/controller/api_tokens_role_test.go` (replace `TestCreateToken_ClampsToCreatorRole` with `TestCreateToken_OverHighRoleIs400`), `internal/cli/token_test.go` (new)

**Interfaces:**
- Consumes: `Principal` (Task 5), `CreateOwnedPAT`, `ListPATsByOwner`, `DeleteOwnedPAT`, `CountRoleOnlyPATsAboveViewer` (Task 4), `authz.Parse`, `Policy.HasFolderGrants`.
- Produces:
  - `func validateCeiling(creator Principal, role string, perms []string) (status int, msg string)` — legacy `role` above `creator.Role` → 400 role string (with `(none)` tail for empty role); unknown role → `400 unknown role "<r>"`; each `perms` entry unknown → `400 unknown permission "<p>"`, not `HeldAnywhere` in `creator.Grants` → 400 `cannot grant permission "<p>": you do not hold it`; for a PAT creator the bound is its own (already restricted) `Grants`.
  - `func WarnRoleOnlyPATs(ctx context.Context, st store.Store, pol *authz.Policy, log *slog.Logger) error` — when `pol.HasFolderGrants()`, logs `slog.Warn("role-only personal access tokens with a role above viewer bypass folder grants until revoked", "count", n)` if `n > 0` (excluding `BootstrapPATName`).
  - CLI: repeatable `--permission <name>` on `token create`; the `--role` help text becomes `role ceiling for the token: admin, developer, or viewer (must not exceed your global role)`; output prints `Permissions: a, b` when present.

Creation rules (`POST /tokens`, order): decode → name required → creator kind: break-glass or role-only PAT (no owner) → `perms` present → 400 `cannot grant permissions: this token has no owner; use --role`; else role-only PAT with role `req.Role` or the creator's role, `validateCeiling` on the role → `CreatePAT`. Session with `Claims == nil` → 403 session string. Bearer with `!ClaimsKnown` → 403 bearer string (claim name from `rolesClaim`, default `groups`). Otherwise owner = creator's `Sub/Email/Claims` (owner PAT creator: its owner snapshot); ceiling = requested (`role` or `perms`, not both → `400 role and permissions are mutually exclusive`), PAT-from-PAT with no requested ceiling inherits the parent's ceiling (`pat.Role`/`pat.Permissions`), with a requested ceiling stores `requested ∩ parent ceiling` as a permission list; `validateCeiling` → `CreateOwnedPAT`. Never clamp silently.
List: `token.manage` → `ListPATs`; else `ListPATsByOwner(principal.Sub)` (empty `Sub` → empty list). Delete: `token.manage` → `DeletePAT`; else `DeleteOwnedPAT(id, Sub)`; `false` → `404 token not found`.

- [ ] **Step 1: Write the failing tests** (`api_tokens_owner_test.go`):
```go
func TestCreateToken_PreUpgradeSessionRefused(t *testing.T)   // 403 exact session string; ListPATs unchanged
func TestCreateToken_BearerWithoutGroupsRefusedWhenGroupKeyed(t *testing.T) // 403 exact bearer string with "groups"
func TestCreateToken_BearerWithoutGroupsUserMapOnly(t *testing.T) // 200; Owner.Claims == []string{}
func TestCreateToken_OverHighRoleIs400(t *testing.T)          // viewer PAT --role admin -> 400 `cannot grant role "admin": it exceeds your global role "viewer"; use --permission to grant folder permissions you hold`
func TestCreateToken_FolderOnlyUserRoleIs400None(t *testing.T) // `... it exceeds your global role (none); ...`
func TestCreateToken_UnheldPermissionIs400(t *testing.T)      // `cannot grant permission "secret.write": you do not hold it`
func TestCreateToken_FolderOnlyUserNoCeiling(t *testing.T)    // 200, Role "", Owner set; the new PAT can read team-a/x and not team-b/x
func TestCreateToken_PATFromPATBoundByParent(t *testing.T)    // parent ceiling [job.read]; child asks [run.trigger] -> 400; asks none -> inherits [job.read]
func TestCreateToken_BreakGlassCreatesRoleOnly(t *testing.T)  // Owner nil, Role admin; perms -> 400 no-owner string
func TestTokens_DeveloperListsAndDeletesOwnOnly(t *testing.T) // dev session sees own PAT only; deleting an admin's / role-only PAT -> 404, row kept
func TestTokens_ManageListsAndDeletesAll(t *testing.T)
func TestWarnRoleOnlyPATs(t *testing.T)                       // captured slog handler: count=2 message exact; no folder grants -> no log
```
`internal/cli/token_test.go`: `TestTokenCreateSendsPermissions` — `captureTransport` records method `POST`, path `/api/v1/tokens`, body `{"name":"ci","permissions":["job.read","run.trigger"]}`; server 400 body is returned as `server error: <body>`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/controller/ -run 'TestCreateToken_|TestTokens_|TestWarnRoleOnlyPATs' -count=1 && go test ./internal/cli/ -run TestTokenCreateSendsPermissions -count=1`
Expected: FAIL — e.g. `TestCreateToken_OverHighRoleIs400`: expected 400, got 200.

- [ ] **Step 3: Implement** the rules; wire `controller.WarnRoleOnlyPATs` in `main.go` after `SetOIDCConfig` (log an error return, do not exit).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/controller/ ./internal/cli/ ./cmd/controller/ -count=1 -race`
Expected: three `ok` lines; `TestLegacyEquivalence_*` still pass (token rows are exempt).

- [ ] **Step 5: Commit** — `git add internal/controller internal/cli cmd/controller`; subject `feat(tokens): record PAT owners, enforce ceilings and per-owner management`.

---

### Task 9: Audit the decision; denied reads; `run.replay` (M)

**Files:**
- Modify: `internal/controller/audit.go:15-236`, `internal/controller/server.go` (wrap top-level human routes — `/runs/{id}/events`, `/runs/active`, `/runs/{id}`, `/runs/{id}/outputs`, `/agents`, artifact routes, agent-or-human matrix rows — with `auditLogMiddleware`), `internal/cli/audit.go:63-66` (print `permission` and `grantedBy` columns), `TODO.md` (remove item 24)
- Test: `internal/controller/audit_decision_test.go`, existing `audit_test.go` (`TestAudit_GetRequestsNotRecorded` keeps passing for allowed GETs)

**Interfaces:**
- Consumes: `withDecisionSlot`, `recordDecision`, `authzDecision` (Task 6); `store.AuditEntry` (Task 4).
- Produces: `auditLogMiddleware(st interface{ InsertAuditLog(ctx context.Context, e store.AuditEntry) error })`. For `GET`/`HEAD`: no response wrapper; after the handler, insert only when the slot is `Denied`, with `Action = string(decision.Permission)`, `Resource = decision.Scope`, `Status = decision.Status`. For `POST`/`PUT`/`DELETE` on classified routes: today's row plus `Permission`, `Scope`, `GrantedBy` (empty `GrantedBy` on a deny). Denied `POST`/`PUT`/`DELETE` on routes absent from `auditActionTable` (e.g. `POST /api/v1/authz/explain`) are recorded the GET way. New table entries: `"POST /api/v1/runs/{id}/replay": "run.replay"`, resource param `"id"`. The `auditActionTable` doc comment is corrected: routes absent from the table are not recorded (allow-list), except denials.

- [ ] **Step 1: Write the failing tests**
```go
func TestAudit_ReplayRecorded(t *testing.T)               // action "run.replay", resource = source run ID, actor, method POST, path, status 200, permission "run.replay", scope = job name
func TestAudit_AllowedWriteRecordsGrant(t *testing.T)     // folder dev triggers team-a/x: permission "run.trigger", scope "team-a/x", grantedBy `folder:team-a (groups["my-org:team-a"])`
func TestAudit_LegacyRoleGrantedBy(t *testing.T)          // dev PAT: grantedBy "role:developer (pat)"; break-glass: "break-glass"
func TestAudit_DeniedWriteRecorded(t *testing.T)          // viewer POST /api/v1/jobs: status 403, permission "job.configure", grantedBy ""
func TestAudit_DeniedReadRecordedEvenAs404(t *testing.T)  // team-a reader GET /api/v1/runs/<team-b run>/logs and /events -> row action "job.read", status 404, scope "team-b/x"
func TestAudit_AllowedReadNotRecorded(t *testing.T)       // GET /api/v1/jobs by viewer -> no row
func TestAudit_SecretBodyNeverRecorded(t *testing.T)      // existing secret special case unchanged with the new columns
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/controller/ -run 'TestAudit_' -count=1`
Expected: FAIL — `TestAudit_ReplayRecorded`: no audit row.

- [ ] **Step 3: Implement**; remove the `### 24.` section from `TODO.md` (the file keeps active work only; its "Done when" is met by `TestAudit_ReplayRecorded` plus the `audit.md` update in Task 13).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/controller/ ./internal/cli/ -count=1 -race`
Expected: `ok` for both.

- [ ] **Step 5: Commit** — `git add internal/controller internal/cli/audit.go TODO.md`; subject `feat(audit): record the checked permission, scope and deciding grant`.

---

### Task 10: Admin policy view — `authz.read` endpoints and `unified-cd authz` (L)

**Files:**
- Create: `internal/authz/describe.go`, `internal/controller/api_authz.go`, `internal/cli/authz.go`
- Modify: `internal/controller/server.go` (register under the `/api/v1` group), `internal/controller/authz_routes.go` (two rows), `internal/controller/authz_legacy_golden_test.go` (`legacyNewRoutes`), `internal/store/store.go` + `postgres.go` (`LatestSessionClaims`), `internal/api/types.go`, `internal/cli/root.go:79` (register `newAuthzCmd`)
- Test: `internal/authz/describe_test.go`, `internal/controller/api_authz_test.go`, `internal/cli/authz_test.go`

**Interfaces:**
- Consumes: `Policy`, `Grants.Explain`, `NormalizeFolder` (Tasks 1, 3); `resolveRoleSource`, `identityGrants` (Task 5); route table (Task 2); `authorize` (Task 6).
- Produces:
  - api: `AuthzPolicy{ OIDC bool; RolesClaim string; Bundles map[string][]string; Legacy AuthzLegacy; Global []AuthzGrantEntry; Folders []AuthzFolderEntry }`, `AuthzLegacy{ RoleMap, UserMap []AuthzLegacyEntry; DefaultRole AuthzLegacyEntry }`, `AuthzLegacyEntry{ Key, Role string; Ignored bool; Permissions []string }`, `AuthzGrantEntry{ Subject, Name string; Grant, Permissions []string }`, `AuthzFolderEntry{ Folder string; Entries []AuthzGrantEntry }`, `AuthzExplainRequest{ User string; Groups []string; Job, Folder string }` (`groups` omitted vs `[]` distinguished with `*[]string`), `AuthzExplainResponse{ Identity{User; Groups; GroupsSource}; Scope{Kind, Name}; GlobalRole string; LoginAllowed bool; Permissions []AuthzExplainedPermission{Permission string; GrantedBy []string} }` — JSON names in lowerCamelCase as in spec §5.18.
  - `func (p *Policy) Describe(legacy Legacy) api.AuthzPolicy` (file order preserved; legacy maps sorted by key).
  - store: `LatestSessionClaims(ctx context.Context, user string) (claims []string, found bool, err error)` — newest `sessions` row with `sub = $1 OR email = $1` and `claims IS NOT NULL`.
  - `GET /api/v1/authz/policy` → `(*Server).handleAuthzPolicy`; `POST /api/v1/authz/explain` → `(*Server).handleAuthzExplain`; both rows `authz.read`, `scopeGlobal`; both registered under the existing `/api/v1` group (audit middleware present, no `auditActionTable` entry, so only denials are recorded).
  - CLI: `newAuthzCmd(resolve)` / `newAuthzCmdWithClient(resolve, *http.Client)` with `show` and `explain`; flags `-o/--output` (`table` default, `yaml`, `json`), `--user`, repeatable `--group`, `--job`, `--folder` (`MarkFlagsMutuallyExclusive("job","folder")`). Table output is tab-separated like `audit list`: `show` prints sections `BUNDLES`, `LEGACY`, `GLOBAL`, `FOLDER <name>` with rows `SUBJECT  NAME  GRANT  PERMISSIONS`; `explain` prints a header (`identity`, `groups (<source>)`, `scope`, `global role`, `login allowed`) and rows `PERMISSION  GRANTED BY` (multiple grants joined with `; `).

- [ ] **Step 1: Write the failing tests**

`describe_test.go`: `TestDescribeListsEveryEntryInFileOrder` (the §5.4 example: two global groups, three folders in order, `team-b` users entry, bundle on folder expands to folder part), `TestDescribeMarksUnknownLegacyRoleIgnored`, `TestDescribeHasNoSecretFields` (marshal to JSON; must not contain `clientSecret`, `token`, `@` outside configured user keys).
`api_authz_test.go`:
```go
func TestAuthzRead_AdminOnlyAmongBundles(t *testing.T) // viewer/developer PAT -> 403 `forbidden: missing permission "authz.read"`; admin and break-glass -> 200; permissions.global users grant [authz.read] -> 200
func TestAuthzPolicy_NoOIDC(t *testing.T)              // oidc false, bundles present, legacy/global/folders empty
func TestAuthzExplain_RequestGroupsWin(t *testing.T)   // stored login claims ["g-old"], request groups ["my-org:team-a"] -> groupsSource "request"
func TestAuthzExplain_LoginGroupsUsed(t *testing.T)    // no groups -> groupsSource "login", groups == stored claims
func TestAuthzExplain_NoSnapshot(t *testing.T)         // groupsSource "none", groups []
func TestAuthzExplain_FolderScopeEqualsJobInside(t *testing.T) // folder "team-a/prod" == job "team-a/prod/<leaf>"
func TestAuthzExplain_GrantedByMatchesAudit(t *testing.T) // explain run.trigger on team-a/x for the identity, then that identity triggers: audit grantedBy == explain GrantedBy[0]
func TestAuthzExplain_Errors(t *testing.T)             // {} -> 400 "user or groups is required"; job+folder -> 400 "job and folder are mutually exclusive"; folder "team-*" -> 400 `invalid folder: folders are literal job-name prefixes, "*", "?", "[" are not allowed`
func TestAuthzEndpoints_AllowedCallsNotAudited(t *testing.T) // no audit rows after allowed show/explain; a denied explain -> one row, action "authz.read", status 403
```
`internal/cli/authz_test.go`: `TestAuthzShowRequest` (method `GET`, path `/api/v1/authz/policy`, `Authorization: Bearer tok`), `TestAuthzExplainRequest` (method `POST`, path `/api/v1/authz/explain`, body `{"user":"a@example.com","groups":["g1","g2"],"job":"team-a/x"}`; without `--group` the body has no `groups` key), `TestAuthzOutputFormats` (`-o json` round-trips into `api.AuthzPolicy`; `-o yaml` parses; default table contains `BUNDLES`; `-o xml` → error `unsupported output format "xml" (use table, yaml or json)`), `TestAuthzExplainJobFolderExclusive`, `TestAuthzServerErrorVerbatim` (403 body printed as `server error: <body>`).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/authz/ ./internal/controller/ ./internal/cli/ -run 'Describe|TestAuthz' -count=1`
Expected: FAIL — undefined: `Describe`, `handleAuthzPolicy`, `newAuthzCmd`.

- [ ] **Step 3: Implement**; add both routes to `legacyNewRoutes` in the golden test.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/authz/ ./internal/store/ ./internal/controller/ ./internal/cli/ -count=1 -race`
Expected: four `ok` lines (classification and golden tests included).

- [ ] **Step 5: Commit** — `git add internal/authz internal/store internal/api internal/controller internal/cli`; subject `feat(authz): add the admin policy view and unified-cd authz show/explain`.

---

### Task 11: Web — `can()`, shared fixture, `/auth/me` grants, 403 re-fetch (M)

**Files:**
- Create: `web/src/lib/authz.js`
- Modify: `web/src/lib/api.js:6,43-106`
- Test: `web/src/lib/authz.test.js`, `web/src/lib/api.test.js`

**Interfaces:**
- Consumes: `/auth/me` shape (Task 5); `web/src/lib/authz-cases.json` (Task 1).
- Produces:
  - `export function can(grants, permission, jobName)` — `grants` is the `/auth/me` `permissions` object; global, then every proper ancestor folder of `jobName`; byte-exact.
  - `export function canAnywhere(grants, permission)`; `export function allows(grants, permission, jobName)` — `grants == null` → `true` (not yet known: render as today, the server decides), else `jobName === undefined ? canAnywhere(...) : can(...)`.
  - `export function requiresText(permission, jobName)` → `requires run.approve on team-a/prod/deploy` / `requires secret.write`.
  - api.js: `export class ApiError extends Error { status }` thrown by `apiFetch`/`apiFetchText` for non-OK responses (message = body text, as today); `export const grants = derived(currentUser, u => u ? u.permissions ?? null : null)`; `export async function refreshMe()` (fetches `/api/v1/auth/me`, sets `currentUser`, de-duplicated: concurrent callers share one in-flight promise); on a `403`, `apiFetch` calls `refreshMe()` once (shared promise; a failed re-fetch is swallowed) and still throws the `ApiError`.

- [ ] **Step 1: Write the failing tests**
```js
// authz.test.js
import cases from './authz-cases.json';
describe('can() fixture', () => { it.each(cases)('$name', c => expect(can(c.grants, c.permission, c.job)).toBe(c.want)); });
it('allows() is permissive only while grants are unknown', () => { expect(allows(null, 'secret.write')).toBe(true); expect(allows({global: [], folders: {}}, 'secret.write')).toBe(false); });
it('requiresText', () => { expect(requiresText('run.approve', 'team-a/prod/deploy')).toBe('requires run.approve on team-a/prod/deploy'); expect(requiresText('secret.write')).toBe('requires secret.write'); });
// api.test.js
it('apiFetch throws ApiError with status on 404', ...);           // e instanceof ApiError, e.status === 404, e.message === body
it('apiFetch: a 403 refetches /auth/me once', ...);               // fetch mock: 1 call to /auth/me
it('apiFetch: concurrent 403s trigger a single /auth/me refetch', ...); // Promise.allSettled of 3 failing calls -> 1 /auth/me call  (Review Focus 5)
it('apiFetch: a failing /auth/me refetch does not loop', ...);     // /auth/me returns 403 -> still 1 call, original ApiError thrown
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd web && npx vitest run src/lib/authz.test.js src/lib/api.test.js`
Expected: FAIL — `Failed to resolve import "./authz.js"`.

- [ ] **Step 3: Implement** `authz.js` and the `api.js` changes.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd web && npx vitest run`
Expected: all test files pass.

- [ ] **Step 5: Commit** — `git add web/src/lib`; subject `feat(web): evaluate the caller's grants with the shared can() rule`.

---

### Task 12: Web — hide or disable controls, not-found view, UI action inventory (L)

**Files:**
- Create: `web/src/lib/ui-actions.json`, `web/src/lib/ui-actions.test.js`, `internal/controller/ui_actions_test.go`
- Modify: `web/src/App.svelte:61-90`, `web/src/routes/RunDetail.svelte:659-665,1127-1140,1260-1320`, `web/src/routes/JobDetail.svelte:29-36,77,86`, `web/src/routes/JobRun.svelte:141-148`, `web/src/routes/TokenList.svelte`, `web/src/routes/AppSourceList.svelte:78-80`
- Test: `web/src/App.test.js`, `web/src/routes/RunDetail.test.js`, `web/src/routes/JobDetail.test.js`, `web/src/routes/JobRun.test.js`, new `web/src/routes/TokenList.test.js`, `web/src/routes/AppSourceList.test.js`

**Interfaces:**
- Consumes: `grants`, `allows`, `requiresText`, `ApiError` (Task 11); `lookupRoute` (Task 2); `newAuthzTestServer` (Task 5).
- Produces: `ui-actions.json` — array of `{ "file": "routes/RunDetail.svelte", "action": "cancel", "method": "POST", "route": "/api/v1/runs/{id}/cancel", "permission": "run.cancel" }`, one entry per call site: RunDetail cancel / rebuild (`POST /api/v1/runs`, `run.trigger`) / approval decide (`run.approve`); JobRun trigger; TokenList create (`token.create`) and delete (`DELETE /api/v1/tokens/{id}`, `token.create`); AppSourceList sync (`appsource.write`); App logout (`POST /api/v1/auth/logout`, `"permission": null`).

UI rules (spec §5.16): nav **hidden** when not `allows(...)` anywhere — Agents (`agent.read`), Schedules (`job.read`), Webhooks (`webhook.read`), GitCredentials (`gitcredential.read`), AppSources (`appsource.read`), Secrets (`secret.list`), Tokens (`token.create` or `token.manage`). Per-job controls **disabled** with `title={requiresText(p, run.jobName)}`: RunDetail Cancel (`run.cancel`), Rerun (`run.trigger`), Approve/Reject (`run.approve`); JobDetail "▶ Run" tab rendered as a disabled span; JobRun submit. AppSource Sync is global-only → hidden. TokenList: role options limited to roles not above `$currentUser.role`; a permission checklist of permissions held anywhere; heading "All tokens" plus an Owner column only with `token.manage`, else "Your tokens". RunDetail/JobDetail: an `ApiError` with status 404 from the main load renders `<div class="error">Run not found</div>` / `Job not found` for both missing and hidden. Every action still calls the server and renders its error text.

- [ ] **Step 1: Write the failing tests**
```js
// ui-actions.test.js — reads every web/src/**/*.svelte (fs), counts /method:\s*["'](POST|PUT|DELETE|PATCH)["']/g per file,
// expects counts to equal ui-actions.json entries per file; a file with calls but no entries fails with
// "<file> has an unlisted state-changing call: add it to web/src/lib/ui-actions.json"
// App.test.js
it('hides global-only navigation the caller lacks', ...) // permissions {global:['job.read'],folders:{}} -> no "Secrets", "Webhooks", "GitCredentials", "AppSources"; "Jobs" present
it('shows all navigation while grants are unknown', ...)
// RunDetail.test.js
it('disables Approve with a tooltip naming the permission', ...) // folders {"team-a":["job.read"]}, run.jobName "team-a/prod/deploy" -> disabled, title "requires run.approve on team-a/prod/deploy"
it('enables Cancel when run.cancel is held on the run\'s folder', ...)
it('renders the not-found view for a 404', ...)                 // GET /api/v1/runs/<id> -> 404 "run not found" -> text "Run not found"
// JobRun.test.js: submit disabled without run.trigger on the job; JobDetail.test.js: Run tab disabled; 404 -> "Job not found"
// TokenList.test.js: viewer role -> role select has only "viewer" and default; "All tokens"/Owner column only with token.manage
// AppSourceList.test.js: Sync hidden without appsource.write, shown with it
```
`internal/controller/ui_actions_test.go`:
```go
func TestUIActionInventoryIsEnforced(t *testing.T) {
    // read ../../web/src/lib/ui-actions.json; for each entry: lookupRoute(method, route) exists and rule.Perm == entry permission
    // (permission null -> scopePublic); for non-null entries an HTTP call by a principal holding every other permission
    // (and job.read for folder-scopable ones) is refused with 403/404 — no action is protected only by hiding
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd web && npx vitest run && cd .. && go test ./internal/controller/ -run TestUIActionInventoryIsEnforced -count=1`
Expected: FAIL — `ui-actions.json` missing / controls not disabled.

- [ ] **Step 3: Implement** the inventory and the component changes.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd web && npx vitest run && npm run build && cd .. && go test ./internal/controller/ -run TestUIActionInventoryIsEnforced -count=1`
Expected: all Vitest files pass, `vite build` succeeds, Go `ok`.

- [ ] **Step 5: Commit** — `git add web internal/controller/ui_actions_test.go`; subject `feat(web): hide or disable actions the caller lacks and keep a UI action inventory`.

---

### Task 13: Documentation, migration guide, final sweep (L)

**Files:**
- Modify: `docs/operator-manual/authorization.md`, `docs/operator-manual/authentication.md`, `docs/operator-manual/audit.md`, `docs/reference/configuration.md`, `docs/reference/cli.md`, `docs/contributing/auth-implementation.md:129-155`, `docs/contributing/invariants.md`, `docs/contributing/frontend-development.md`, `docs/troubleshooting/controller-and-database.md`, `docs/user-guide/resources/app-source.md`, `README.md`, `docs/getting-started/quickstart.md` (check only; edit if a step now fails)
- Create: `docs/operator-manual/migrations/2026-10-28-matrix-authorization.md`

**Interfaces:**
- Consumes: every exact string and behaviour of Tasks 1–12.
- Produces: docs only.

Content (spec §10 plus §5.18): authorization.md — the 25-permission catalogue, bundles, `oidc.permissions` reference and §5.4 example, resolution order with union and the login rule, the PAT rule (owner snapshot ∩ ceiling), the three warning boxes ("job names are not confidential", "folders are not a secret boundary: `job.configure` on any folder ≈ reading every secret", "anyone who can push to an AppSource repository can place jobs in any folder"), the sibling-folder recipe, `unified-cd authz show` / `explain`, and the rows today's matrix misses (Vars, replay, run delete, artifacts). authentication.md — sessions store claims, silent refresh, PAT owners and ceilings, PAT owner refresh at browser login. audit.md — `permission`/`scope`/`grantedBy`, the `grantedBy` forms, denied reads, `run.replay` row, allow-list wording. configuration.md — `oidc.permissions` (config-file only, no env var). cli.md — `token create --permission`, `--role` no longer clamps, `authz show` / `authz explain` with flags and output formats, server error strings. auth-implementation.md — "Adding an endpoint" step 3 becomes "add the route to `routeTable` in `internal/controller/authz_routes.go` with a permission and scope", `viewerOrAgent`/`requireMinRole` rows replaced by `authorize` + `AgentBypass`, "Where to look" gains `internal/authz`. invariants.md — "every route is classified (`TestEveryRegisteredRouteIsClassified`)". frontend-development.md — `can()`/`allows()`, the shared fixture, hide-vs-disable, the UI action inventory, a UI control is never the only enforcement. troubleshooting — one symptom entry per startup string (including the break-glass-left-enabled warning), `403 forbidden: missing permission ...`, unexpected 404 on a readable-looking job, `forbidden: no permissions` / `token owner has no permissions`, the two PAT-creation refusals, the role-only PAT startup warning. app-source.md — folder grants do not apply to synced jobs; the Git host governs who writes them. Migration guide — before→after table (developer token list/delete narrowing; over-high `--role` 400 instead of clamp; pre-upgrade session PAT creation 403; `/auth/me` behind `ServerAuth`; role-less role-only PAT now `403 forbidden: no role` on artifact routes) with exact strings, the team-separation recipe, "re-issue team PATs after the switch". README — one feature-summary line.

- [ ] **Step 1: Write the docs.**

- [ ] **Step 2: Sweep for stale or now-wrong statements**

Run: `grep -rn "requireMinRole\|viewerOrAgent\|capped at it\|clamp" docs/ examples/ templates/ README.md internal/ cmd/ | grep -v "docs/superpowers/"`
Expected: no hits except intentional historical mentions in the migration guide.
Run: `grep -rn "24 permissions\|developer can list\|any developer" docs/ README.md | grep -v docs/superpowers/` — Expected: no hits.
Run: `grep -n "^### 24\." TODO.md` — Expected: no output.

- [ ] **Step 3: Full verification**

Run: `make lint && make test && (cd web && npx vitest run && npm run build) && go test ./internal/dsl/ -run 'Templates|Examples' -count=1`
Expected: `go vet` clean; every Go package `ok`; Vitest all passed; build succeeds.

- [ ] **Step 4: Commit** — `git add docs README.md`; subject `docs: document matrix authorization, the policy view and the upgrade path`.

---

## Self-review (applied)

1. **Spec coverage.** §5.1/§7.1/§7.3 → T1; §5.13/§9 classification and golden → T2; §5.4 config and every startup string, legacy roleMap warning → T3; §7.5 data → T4; §5.5 evaluation, §5.10, §5.15 admission, §5.11 `/auth/me` → T5; §5.7, §5.8 (jobs path), §7.2 rules, SSE at connect, approvals at decision time → T6; §5.3 resulting-name check, §5.8/§5.17 apply ordering, §7.4 lists and schedule re-target → T7; §5.5 creation, §5.6, §5.14, §5.15 ceilings, §7.4 CLI, §9 step 3 startup warning → T8; §5.12, §5.16 rule 6, TODO item 24 → T9; §5.18 → T10; §5.16 rules 2–5 and §5.11 shared fixture → T11–T12; §10 → T13. §5.9 (job code not confined, system triggers unchecked) needs no code; T13 documents it.
2. **Step scan.** Each code step names files, signatures and pinned values; the only algorithm written out is `authorize` (its fast path and 404/403 split are not implied by the signature). No "TBD" or unnamed helpers remain.
3. **Type consistency.** `authz.Grants`/`Set`/`GrantsView`, `Principal.Grants`, `requireJob(w, r, perm, job, hide)`, `RunJobName`/`ScheduleJobName` returning `(string, bool, error)`, `store.AuditEntry` and `InsertAuditLog(ctx, e)` are used with the same names in every task that consumes them.
4. **Review Focus.** Five lines, each with a named test in its owning task (T6 ×2, T7, T1/T11 fixture, T11).
5. **Proportion.** Code blocks are test names and assertions, the migration SQL and one algorithm; handler bodies are left to the implementer.
