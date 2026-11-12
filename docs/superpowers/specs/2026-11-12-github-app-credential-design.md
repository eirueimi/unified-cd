# GitHub App credentials — design

Status: agreed with the user (2026-11-12), ready for planning

## Intent

Today every private GitHub repository is read with a long-lived secret: a PAT
(`type: token`) or a deploy SSH key (`type: sshKey`) stored as a StoredSecret
and referenced from a GitCredential. Jobs that talk to GitHub (`git-checkout`,
`github-commit-status`, `github-pr-comment`, `github-release`) use a stored
PAT through `.Secrets`.

The user's goal is security: stop keeping long-lived, person-bound tokens.
A GitHub App gives installation tokens that expire after one hour, are not
tied to a person, and can be narrowed to chosen repositories and permissions
when issued.

This adds a GitCredential `type: githubApp`. The controller holds the App's
private key, issues installation tokens, uses them for its own git access,
and hands tokens to jobs that are allowed to have them. Jobs use the token
exactly like a stored secret, by name.

The model follows Jenkins (GitHub Branch Source "GitHub App" credential): the
key stays on the server, the credential decides which jobs may use it and how
far its token reaches.

## Goals

1. `type: githubApp` GitCredential: App ID, optional installation ID, private
   key by secret name, optional API URL for GitHub Enterprise Server.
2. The controller uses it for AppSource sync and `uses:` git:// template
   fetch, with a token narrowed to the one repository and `contents: read`.
3. A job uses the token by referencing the GitCredential's name where it
   would reference a secret: `{{ .Secrets.<credential-name> }}`, or
   `token_secret: <credential-name>` for the existing templates. No new job
   syntax. The token is delivered, masked and kept out of storage like a
   secret.
4. The GitCredential decides which job folders may use it and the ceiling of
   its token (repositories, permissions). A credential without `jobAccess` is
   never handed to a job.
5. Every hand-out of a token to a run is audited.

## Non-goals

- A plugin mechanism. The user chose an in-tree `TokenSource` interface with
  `githubApp` as its only implementation; later providers (Azure DevOps,
  workload identity) are in-tree additions. No exec or out-of-process plugins.
- Jobs holding the private key; no template that mints tokens in a job.
- Per-job narrowing. A job gets the credential's full ceiling. To give
  different jobs different reach, an admin creates several GitCredentials
  (as with several PATs today).
- Tokens that outlive GitHub's one hour; revoking a token at run end (tokens
  are shared between runs, see below). Both are Open items.
- Changing how `token` and `sshKey` credentials behave, apart from the
  optional `controllerUse` field.

## Anti-goals

- The private key never leaves the controller: not in a claim response, an
  agent fetch, logs, audit rows, API responses or errors.
- Only a holder of `gitcredential.write` (global) changes who may use a
  credential or how far it reaches. A job author cannot widen it.
- No path, including `uses:`, lets a job read a repository through the App
  that its credential's `jobAccess` does not allow.

## Decided by the user

- Key on the controller; jobs get issued tokens delivered like secrets.
- In-tree `TokenSource` only.
- Folders decide which jobs may use a credential; the credential sets the
  reach; no `jobAccess` means no job use.
- Same job-side syntax as existing credentials: the GitCredential name is
  used as a secret name. No `gitTokens` declaration.
- `controllerUse: false` lets a credential opt out of controller-side use.
- `uses:` is subject to `jobAccess`; when it does not allow the fetch, the
  controller fetches anonymously instead of failing outright, so public
  templates keep working.
- Tokens are cached per credential and shared between runs; a token with
  less than 50 minutes left is not handed to a run; no revoke at run end.

## Resource shape

```yaml
apiVersion: unified-cd/v1
kind: GitCredential
metadata:
  name: gh-app-release
spec:
  host: github.com
  type: githubApp
  secretRef: github-app-key        # StoredSecret holding the PEM private key
  controllerUse: true              # optional, default true; all types
  githubApp:
    appId: 123456
    installationId: 7890123        # optional, see "Installation lookup"
    apiURL: https://api.github.com # optional; GHES: https://ghe.example.com/api/v3
  jobAccess:                       # optional; absent = jobs cannot use this credential
    folders: [team-a, platform/tools]   # authz folder rules; "*" = every job
    repositories: [acme/app, acme/infra-*]  # "owner/name", "owner/*", or a trailing-* prefix
    permissions:                   # GitHub App permission names -> highest level
      contents: write
      pull_requests: write
      statuses: write
```

- `secretRef` keeps its meaning (the StoredSecret with the credential
  material). For `githubApp` it holds a PEM RSA key (PKCS#1 or PKCS#8),
  parsed when a token is first needed.
- `githubApp` is required for and only allowed with `type: githubApp`;
  `knownHosts` is not allowed with it. `jobAccess` is only allowed with
  `type: githubApp` in this version; with `jobAccess`, `repositories` and
  `permissions` are required and non-empty.
- `jobAccess.repositories` must name a single owner (one installation). A
  credential that needs two organizations is two credentials.
- Permission levels: `read < write < admin`. Names are passed through to
  GitHub; the controller keeps no list of them.
- `controllerUse` applies to every type. Lookup by host for AppSource and
  `uses:` skips credentials with `controllerUse: false`. Several credentials
  per host stay allowed (host is not unique today); among the eligible ones
  the most recently updated still wins, as now.

### Name rules

A GitCredential with `jobAccess` shares the `.Secrets` namespace, so:

- its name must match the secret-name pattern `^[A-Za-z_][A-Za-z0-9_-]*$`;
- upserting it fails if a StoredSecret has that name, and setting a
  StoredSecret fails if such a GitCredential has that name (both directions,
  API and GitOps apply).

## Token issuing (controller)

A new package (name is the builder's call) defines:

```go
type TokenRequest struct {
    Repositories []string          // repo names within one owner; empty = installation's
    Permissions  map[string]string // name -> read|write|admin; empty = installation's
}
type Token struct { Value string; ExpiresAt time.Time }
type TokenSource interface {
    Token(ctx context.Context, req TokenRequest) (Token, error)
}
```

The `githubApp` implementation:

- Signs an RS256 JWT with go-jose (already in go.mod): `iss` = App ID,
  `iat` = now − 60 s, `exp` = now + 9 min.
- `POST {apiURL}/app/installations/{id}/access_tokens` with `repositories`
  (names without owner) and `permissions`.
- Pattern expansion: GitHub accepts only exact names. When `jobAccess`
  contains `owner/*` or a trailing-`*` prefix, the controller lists the
  installation's repositories (`GET /installation/repositories`, paginated)
  and sends the matching names. `owner/*` alone may send no `repositories`
  (the whole installation).
- In-memory cache per replica, keyed by credential name and request. Job
  tokens: one cached token per credential, re-issued when less than 50
  minutes remain. Controller tokens (one repo, `contents: read`): reused
  until 5 minutes remain.
- HTTP client as in cloud enrollment (`internal/controller/agent_enrollment_*.go`):
  injectable `*http.Client`, default transport (so `HTTPS_PROXY` and the
  system CA pool apply), fixed timeout. No private-CA flag in this version.
- Errors carry GitHub's status and `message`, never the JWT or a token.

### Installation lookup

If `installationId` is set, it is used. Otherwise the controller looks up
the installation from a repository
(`GET /repos/{owner}/{repo}/installation`) and caches owner → installation
for 1 hour. For job tokens the owner comes from `jobAccess.repositories`; for
controller use, from the repository being fetched.

## Controller-side use

- `resolveCredential` (appsource_reconciler.go) and the `credFn` in
  `resolveGitPendingRuns` (scheduler.go) duplicate the secret → credential
  logic; both move to one shared function with the `githubApp` case.
- `gittemplate.CredentialFunc` is `func(ctx, host)` today
  (`internal/gittemplate/resolve.go:25`, called at `:179`). It must also
  receive owner/repo (to narrow the token) and, for `uses:`, the run's
  qualified job name (to apply `jobAccess`). Every caller changes. `credFn`
  runs before the template cache lookup, so a cache hit still asks for a
  credential; the token cache keeps that cheap.
- AppSource: token for that one repository, `contents: read`. `jobAccess`
  does not apply (an admin configured the AppSource).
- `uses:`: if the credential has `jobAccess` and it covers the run's job
  folder and the repository, use a token for that one repository,
  `contents: read`. Otherwise fetch anonymously (the existing empty
  credential path, `scheduler.go` "public repo"). A public template is then
  read as before; a private one fails in git, and the error adds that the
  GitCredential does not allow this job or repository. A `githubApp`
  credential without `jobAccess` is used by `uses:` for no job.
- Git transport: unchanged `GIT_ASKPASS` path
  (`internal/gittemplate/fetch.go`). For `githubApp` the HTTPS user is
  `x-access-token`, as GitHub documents for installation tokens; `token`
  keeps `oauth2`. Confirm against a real installation in an integration test.

## Job-side use

- A job references the token by the credential name:
  `{{ .Secrets.gh-app-release }}`, `index .Secrets "gh-app-release"`, or a
  template param such as `token_secret: gh-app-release`. Name extraction
  (`internal/dsl/secretrefs.go`) is unchanged; `.Params`-resolved names work
  as they do for secrets today.
- Issuing happens in `handleAgentSecretsFetch`
  (`internal/controller/api_secrets.go`). After the existing agent and run
  checks and the "needed by this run" check (`secretNamesForRun`, which reads
  the run's spec snapshot), a requested name that is a `githubApp`
  GitCredential with `jobAccess` is answered with a token instead of a
  StoredSecret value. The run's qualified job name must be covered by
  `jobAccess.folders` (`authz.Covers`; root-level jobs only by `"*"`). The
  check uses the credential as it is at fetch time. A failure is a 403 naming
  the credential and the rule, and the run fails as with a missing secret.
- The token is the credential's cached job token (shared between runs, at
  least 50 minutes left when handed out).
- Agents and the masker need no change: the token arrives in the same
  `AgentFetchSecretsResponse` map and is masked like any secret. The k8s
  agent uses the same fetch.
- `github-commit-status.yaml` hard-codes `{{ .Secrets.github-token }}`.
  Either add a secret-name param to it (like `git-checkout`'s
  `token_secret`), or document that the credential must be named
  `github-token`. The builder adds the param; the default stays
  `github-token`.

## Several organizations in one job (helper template)

An installation token covers one installation, which is one organization, so
a job that fetches private repositories of two organizations (for example
`go mod tidy` with modules from `acme` and `widget`) needs two GitCredentials
and must route each organization to its own token.

Decided by the user: route with a git credential helper that reads the token
from an environment variable. Do not put tokens in URLs (`insteadOf` with
`https://x-access-token:<token>@...`): that writes the token to a config
file (on a native job, the agent user's `~/.gitconfig`, shared across runs),
passes it in the argv of `git-remote-https` (visible to other processes), and
can surface it in error output written to artifacts or caches.

A new template (name is the builder's call, for example `git-auth-run`):

- Params: a list of GitCredential names, and the command to run.
- For each credential it sets an environment variable with the token
  (`{{ index .Secrets <name> }}`) and adds, through `GIT_CONFIG_COUNT` /
  `GIT_CONFIG_KEY_<n>` / `GIT_CONFIG_VALUE_<n>` (git 2.31+), so nothing is
  written to disk:
  - `credential.useHttpPath=true`
  - `credential.https://<host>/<owner>.helper=` a shell function that prints
    `username=x-access-token` and `password=$<VAR>`
  The owner comes from the credential's `jobAccess.repositories` (single
  owner, see Resource shape). The template receives it as a param next to
  each credential name, because templates cannot read GitCredentials.
- Then it runs the command in the same step, so the variables exist only in
  that step. unified-cd has no mechanism to carry environment variables from
  one step to the next, and an isolated job's `$HOME` is fresh per run, so a
  configure-once step would not reach later steps.
- The builder confirms with a real git that a path-scoped
  `credential.<url>.helper` with `useHttpPath` selects the right helper per
  organization, and that `go` (which runs git) picks it up, in a test.

Prior art, for the docs: the `gh` CLI's `gh auth setup-git` registers
`gh auth git-credential` as a helper that reads `GH_TOKEN`; it is per host,
so one token for all of github.com, and does not split organizations. Git
Credential Manager is aimed at interactive sign-in and stores credentials in
the OS keychain; neither mints App tokens.

## Audit and metrics

- `audit_logs` has no details column today. Migration 029 adds one (for
  example `details jsonb`), and `store.AuditEntry` / `InsertAuditLog` gain it.
- An audit row per hand-out to a run: action `gitcredential.token_issue`,
  actor `agent:<id>`, resource the credential name, details with run ID, job
  name, repositories, permissions and the token's `expires_at`. Denials are
  audited too. Agent routes are outside `auditLogMiddleware`, so the handler
  writes the row directly, as `auditAgentCredentialAs` does. The token is
  never recorded.
- Controller-side issuance is not audited per fetch; it is counted.
- Metric: tokens issued and handed out, by credential, purpose
  (`controller`/`job`) and result.

## Storage, API, CLI, UI

- Migration 029 also allows `githubApp` in the `git_credentials.cred_type`
  CHECK and adds `controller_use boolean NOT NULL DEFAULT true` and a
  `config jsonb NOT NULL DEFAULT '{}'` for `githubApp` and `jobAccess` (the
  builder may choose columns). `UpsertGitCredential` and
  `GetGitCredentialByHost` change accordingly.
- Every hard-coded `token|sshKey` list gets `githubApp`: the DSL schema tag
  and `Validate`, the API handler, the DB CHECK, the two `switch`
  statements. Also check `schemas/` and `web/`.
- `GET /gitcredentials` and `unified-cd gitcredential list` show type, App
  ID, installation ID, API URL, `controllerUse` and `jobAccess`; never key
  material.
- GitOps apply and export carry the new fields. A GitCredential synced from
  an AppSource can set `jobAccess`; that is the trust boundary documented in
  #225.

## Known drawbacks (to document)

- Rate limits: issuing calls are subject to GitHub's secondary limits; the
  shared job token keeps issuing to about one call per credential per 10
  minutes.
- API calls made with the token count against the installation's hourly
  limit (from 5,000), shared by every job and controller fetch using that
  App. Use separate Apps per purpose (sync, release, CI) to isolate them.
- A run gets a token with 50 to 60 minutes left; a run that uses it after
  that fails.
- A job always gets the credential's full reach; fine-grained reach means
  more GitCredentials.
- GitCredential and StoredSecret names share one namespace for
  `jobAccess` credentials.

## Docs to update

- `docs/user-guide/resources/git-credential.md` (new type, `controllerUse`,
  `jobAccess`, GHES, installation lookup, drawbacks)
- `docs/user-guide/secrets.md` (credential names in `.Secrets`, name rules)
- `docs/reference/field-reference.md`, `json-schema.md`, `cli.md`
- `docs/operator-manual/audit.md` (new action and details), `authorization.md`
- `templates/README.md`, the GitHub templates' header comments, and the new
  param on `github-commit-status.yaml`
- A migration note only if a behaviour of existing setups changes (none is
  expected; `controllerUse` defaults to today's behaviour)
- `templates/README.md` entry for the helper template
- `TODO.md` (Open items)

## Open items (not in this version)

- Tokens for runs longer than one hour (re-fetch on expiry, or per step).
- Revoking tokens (needs per-run tokens).
- Per-job narrowing of repositories and permissions.
- A private CA flag for GHES.
- Other `TokenSource` providers (Azure DevOps, workload identity).

## Left to the builder

- Package and type names, cache structure, metric names.
- `config` jsonb versus dedicated columns.
- Exact error wording, within the rules above.

## Rulings made while planning

Recorded by the controller session; the plan implements them.

- The `uses:` template cache is partitioned by credential, and its keys move
  to a new prefix, so a run that `jobAccess` does not allow never reads a
  template cached by an allowed run.
- The agent secrets fetch refuses any name that is the `secretRef` of a
  `githubApp` credential, so a job cannot read the private key as a secret.
- The token cache key includes the credential's App ID, installation ID, API
  URL, key reference and the normalized request, so narrowing `jobAccess`
  takes effect on the next hand-out.
- Installation lookup without a concrete repository uses
  `/orgs/{owner}/installation`, then `/users/{owner}/installation`.
- Pattern expansion lists repositories with a listing-only token
  (`metadata: read`) that is never cached or handed out. No match is an
  error; the request is never sent with an empty list. More than 500 matches
  is an error.
- `uses:` needs `contents` of at least `read` in `jobAccess.permissions`;
  otherwise the fetch is anonymous.
- `jobAccess.folders` must be non-empty.
- Default API URL: `https://api.github.com` for `github.com`, otherwise
  `https://<host>/api/v3`; `apiURL` must be https.
- The helper template has four fixed credential/owner slots, and it clears
  inherited helpers for each owner before adding its own.
- The helper test proves per-organization helper selection with git over a
  TLS test server; that `go` picks it up follows from `go` running git with
  the same environment, which the docs state.
- `gittemplate.Credential` gains `Username` so `githubApp` can send
  `x-access-token`.
- If a StoredSecret and a `jobAccess` credential end up with one name (only
  through a race), the credential wins in the fetch.
- An AppSource with an SSH `repoURL` and a `githubApp` credential is an error
  that asks for an https `repoURL`.
