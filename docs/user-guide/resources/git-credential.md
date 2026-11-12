# GitCredential

## GitCredential

Stores Git authentication credentials for private repositories, used with `git://` template URIs and AppSource.

```yaml
apiVersion: unified-cd/v1
kind: GitCredential
metadata:
  name: <string>                  # required
  annotations:                    # optional
    <key>: <value>
spec:
  host: <string>                  # required — hostname to apply credentials to
  type: token | sshKey | githubApp  # required
  secretRef: <string>             # required — name of StoredSecret containing the credential (for githubApp: the PEM private key)
  knownHosts: <string>            # optional, sshKey only — OpenSSH known_hosts lines pinning the server host key
  controllerUse: <bool>           # optional, default true — may the controller use this credential for its own git fetches
  githubApp:                      # required for, and only valid with, type githubApp
    appId: <int>                  # required — GitHub App ID
    installationId: <int>         # optional — 0/omitted: looked up from the repository owner
    apiURL: <string>              # optional — https URL; default https://api.github.com (GHES: https://HOST/api/v3)
  jobAccess:                      # optional, githubApp only — absent: jobs cannot use this credential
    folders: [<string>]           # job folders allowed to use it; "*" = every job
    repositories: [<string>]      # "owner/name", "owner/*" or "owner/prefix*"; one owner only
    permissions:                  # GitHub App permission name -> highest level (read | write | admin)
      <name>: <level>
```

### Fields

| Field | Type | Required | Description |
|---|---|---|---|
| `metadata.name` | string | Yes | Unique credential name |
| `metadata.annotations` | map[string]string | No | Arbitrary key/value metadata |
| `spec.host` | string | Yes | Hostname to apply the credential to (e.g. `github.com`, `gitlab.example.com`) |
| `spec.type` | string | Yes | `token` for HTTP PAT/OAuth token, `sshKey` for SSH private key, `githubApp` for a GitHub App (the controller mints short-lived installation tokens from its private key) |
| `spec.secretRef` | string | Yes | Name of a StoredSecret holding the actual credential value; for `githubApp`, the App's PEM private key (PKCS#1 or PKCS#8) |
| `spec.controllerUse` | bool | No | Default `true`. When `false`, the controller skips this credential when it resolves a host for AppSource syncs and `git://` templates. Allowed for every type. |
| `spec.githubApp` | object | `githubApp` only | `appId` (required, positive), `installationId` (optional, not negative) and `apiURL` (optional, `https` URL without credentials, query or fragment). Not allowed for other types; `knownHosts` is not allowed with `githubApp`. |
| `spec.jobAccess` | object | No | `githubApp` only. Declares which jobs (`folders`), which repositories (`repositories`, a single owner) and which GitHub App permissions (`permissions`, at most the given level) a job-side token may reach. All three lists are required and non-empty when `jobAccess` is set. |
| `spec.knownHosts` | string | No | `sshKey` only, at most 64 KiB. One or more OpenSSH `known_hosts` lines (`<host> <key-type> <base64-key>`) that pin the server's SSH host key. See [SSH host key verification](#ssh-host-key-verification). |

### SSH host key verification

For `sshKey` credentials, the controller always verifies the SSH server's host key when it fetches over SSH (AppSource syncs and `git://` templates); host key checking is never turned off there. Only a pinned `knownHosts` gives full protection: trust on first use cannot tell a genuine first connection from an attacker's, and `accept-new` also accepts an additional key type for a host it already knows.

- **`knownHosts` set (recommended):** ssh runs with `StrictHostKeyChecking=yes` against exactly the keys you pinned. Any other key fails the fetch. Get the lines with `ssh-keyscan -t ed25519 gitlab.example.com` and compare the fingerprint with the one your Git host publishes before pinning. For a non-default port use the `[host]:port` form.
- **`knownHosts` empty:** trust on first use (`StrictHostKeyChecking=accept-new`). The first key a host presents is recorded in the controller's persistent known_hosts file (`--git-known-hosts-file` / `UNIFIED_GIT_KNOWN_HOSTS_FILE`, default `<data-dir>/.ssh/known_hosts`, else a private per-process temporary directory that is lost on restart). The directory must be owned by the controller's user and not group- or other-writable (Unix), and the path must not contain a double quote; the controller refuses to start otherwise) and a key that later changes is rejected. If the file is not on persistent storage, or you run several controller replicas, each starts with an empty trust store, so the first connection after a restart is trusted again; pin `knownHosts` to avoid that.

A rejected key fails with an error containing `SSH host key verification failed`. If the key changed legitimately, update `knownHosts` (or delete the stale line from the controller's known_hosts file).

**Not covered:** the shipped `templates/git-checkout.yaml` and `templates/rsync-deploy.yaml` run on agents, not in the controller, and they do **not** verify host keys (they pass `StrictHostKeyChecking=no` and discard known_hosts). This field does not affect them; see TODO.md for the follow-up.

### Name rules for `jobAccess`

A GitCredential with `jobAccess` shares the secrets namespace, so its `metadata.name` must match the secret-name pattern `^[A-Za-z_][A-Za-z0-9_-]*$`, and it cannot have the same name as a StoredSecret. Both directions are enforced, through the API and through GitOps apply:

- Upserting such a credential fails (HTTP 409) with `name "<name>" is already used by a StoredSecret; a GitCredential with jobAccess shares the secrets namespace`.
- Setting a secret fails (HTTP 409) with `name "<name>" is already used by a GitCredential with jobAccess`.

`jobAccess.folders` entries are normalized like authorization folders (leading and trailing `/` removed); `"*"` is kept.

### Credential matching

When resolving a `git://` URI or AppSource `repoURL`, the controller looks up a GitCredential whose `spec.host` matches the URI's hostname. This allows job definitions to reference private templates without embedding credentials. An AppSource `repoURL` that embeds credentials (`https://user:pass@host/...`) is rejected; a GitCredential is the only way to authenticate an AppSource.

For a `githubApp` credential the controller fetches with a short-lived installation token narrowed to the one repository being fetched and `contents: read`, sent over HTTPS with the user `x-access-token`:

- **AppSource:** always uses the token; `jobAccess` does not apply. The `repoURL` must be `https://<host>/<owner>/<repo>[.git]`; an SSH `repoURL` fails the sync with `GitCredential "<name>" (githubApp) needs an https repoURL`.
- **`git://` templates (`uses:`):** the token is used only when `jobAccess` covers the run's job folder (`"*"` covers every job, including root-level ones) and the template repository, and `jobAccess.permissions.contents` is at least `read`. Otherwise the template is fetched anonymously: a public template still resolves, and a private one fails with git's error followed by `GitCredential "<name>" does not allow job "<job>" to read <owner>/<repo> through the GitHub App (jobAccess)` (or, without `jobAccess`, `GitCredential "<name>" has no jobAccess, so no job can read <owner>/<repo> through the GitHub App`).

The controller's template cache (fixed refs only) is partitioned by the credential a template was fetched with, so a run fetching anonymously never reads a template cached by a run that the credential allowed.

### GitHub App credentials

`type: githubApp` lets unified-cd authenticate to GitHub as a [GitHub App](https://docs.github.com/en/apps) instead of with a long-lived personal access token. The `secretRef` StoredSecret holds the App's PEM private key (PKCS#1 or PKCS#8); the key stays on the controller. The controller signs a short-lived App JWT, exchanges it for an installation access token (valid for one hour) and uses that token in place of a stored one. Three questions decide how a credential is used:

- **Does the controller use it for its own fetches?** `controllerUse`.
- **Which jobs may receive a token?** `jobAccess.folders`.
- **How far does that token reach?** `jobAccess.repositories` and `jobAccess.permissions`.

```yaml
apiVersion: unified-cd/v1
kind: GitCredential
metadata:
  name: gh-app-release
spec:
  host: github.com
  type: githubApp
  secretRef: github-app-key        # StoredSecret holding the PEM private key
  controllerUse: true              # optional, default true; allowed for every type
  githubApp:
    appId: 123456
    installationId: 7890123        # optional, see "Installation lookup"
    apiURL: https://api.github.com # optional; GHES: https://ghe.example.com/api/v3
  jobAccess:                       # optional; absent = jobs cannot use this credential
    folders: [team-a, platform/tools]        # authz folder rules; "*" = every job
    repositories: [acme/app, acme/infra-*]   # "owner/name", "owner/*", or "owner/prefix*"
    permissions:                   # GitHub App permission names -> highest level
      contents: write
      pull_requests: write
      statuses: write
```

#### `controllerUse`

Applies to every credential type. The controller looks a credential up by host for AppSource syncs and `git://` (`uses:`) templates; a credential with `controllerUse: false` is skipped. Several credentials may share a host (host is not unique); among the eligible ones the most recently updated credential wins. Set `controllerUse: false` on a credential that exists only for jobs, so the controller never picks it for a sync.

#### `jobAccess`: which jobs and how far

`jobAccess` is what lets a **job** use the credential. Without it, a `githubApp` credential is never handed to any job and is never used for `uses:` (the controller can still use it for AppSource syncs).

| Field | Meaning |
|---|---|
| `folders` | Required, non-empty. Authorization folder rules (the same rule as `authz` folder grants): `team-a` covers `team-a/build` and `team-a/sub/job`, not a job literally named `team-a`. `"*"` covers every job. A job at the root (no folder) is covered only by `"*"`. |
| `repositories` | Required, non-empty. `owner/name`, `owner/*` or `owner/prefix*` (a trailing `*` only). **All entries must name one owner**, because an installation token belongs to one installation; a credential that needs two organizations is two credentials. |
| `permissions` | Required, non-empty. GitHub App permission names (for example `contents`, `pull_requests`, `statuses`) mapped to the highest level a token may carry: `read < write < admin`. Names are passed through to GitHub; unified-cd keeps no list of them. The App itself must have been granted at least these permissions. |

A job always receives the credential's full reach: every repository and permission in `jobAccess`. For narrower access, create more credentials.

Only a holder of the global `gitcredential.write` permission changes `jobAccess`; a job author cannot widen it. A GitCredential applied through an AppSource can set `jobAccess` too, so a repository that an AppSource syncs is a trust boundary: anyone who can push to it can grant jobs access to the App's repositories.

#### Using the token in a job

A job references the token by the credential name, exactly like a secret:

```yaml
steps:
  - name: open-pr
    env:
      GH_TOKEN: "{{ .Secrets.gh-app-release }}"   # or: index .Secrets "gh-app-release"
    run: |
      curl -fsS -X POST -H "Authorization: Bearer ${GH_TOKEN}" \
        https://api.github.com/repos/acme/app/pulls -d @pr.json
```

Template parameters such as `token_secret: gh-app-release` (for example on `github-commit-status.yaml`) work the same way. When the agent fetches the run's secrets, the controller checks, at that moment and against the credential as it is now, that the run's job is covered by `jobAccess.folders`, and answers with the token. A job outside the folders gets HTTP 403 `gitcredential "<name>": job "<job>" is not covered by jobAccess.folders` and the run fails like a run with a missing secret. Every hand-out and every denial is written to the [audit log](../../operator-manual/audit.md#github-app-job-tokens) (`gitcredential.token_issue`) and counted in `unifiedcd_git_credential_tokens_total`.

The token is the credential's cached job token, shared between runs and reissued when less than 50 minutes of its hour remain, so a run receives a token with 50 to 60 minutes left. It is masked in logs like any secret. The private key (`secretRef`) is never delivered: requesting it as a secret fails with `secret "<name>" is the private key of GitHub App credential "<credential>" and is never delivered to jobs`.

#### Controller use

The controller uses a `githubApp` credential for its own fetches with a token narrowed to the one repository being fetched and `contents: read` (see [Credential matching](#credential-matching)). When `uses:` is not allowed by `jobAccess`, the fetch is anonymous so public templates keep working; the controller does not fail outright. `jobAccess.permissions` needs `contents` of at least `read` for `uses:` to use the token.

#### GitHub Enterprise Server

Set `spec.host` to the GHES host. `apiURL` defaults to `https://api.github.com` for `github.com` and to `https://<host>/api/v3` for any other host; set `apiURL` to override it. It must be an `https` URL. The controller uses the system certificate pool; there is no flag for a private CA in this version, so install the CA into the controller image or host trust store.

#### Installation lookup

If `githubApp.installationId` is set, it is used. Otherwise the controller looks up the installation of the repository owner (`GET /repos/{owner}/{repo}/installation`, or `/orgs/{owner}/installation` then `/users/{owner}/installation` when no concrete repository is known, as for a pattern) and caches owner to installation for one hour. For job tokens the owner comes from `jobAccess.repositories`; for controller use, from the repository being fetched. An App installed in several organizations needs `installationId` or one credential per organization.

A `jobAccess.repositories` pattern (`acme/*`, `acme/infra-*`) is expanded by listing the installation's repositories with a listing-only `metadata: read` token that is never cached or handed out. No match is an error (`no repository of the installation matches ...`) and more than 500 matches is an error. `owner/*` alone asks GitHub for the whole installation.

#### Known drawbacks

- **Rate limits.** Token issuing is subject to GitHub's secondary rate limits; the shared job token keeps issuing to about one call per credential per 10 minutes. API calls made with a token count against the installation's hourly limit (from 5,000 requests), shared by every job and controller fetch that uses the same App. Use separate Apps per purpose (sync, release, CI) to isolate them.
- **Token lifetime.** A run gets a token with 50 to 60 minutes left. A run that keeps using it after that fails; re-fetching for runs longer than one hour is not supported yet (see `TODO.md`). Tokens are not revoked when a run ends.
- **No per-job narrowing.** A job gets the credential's full `jobAccess` reach.
- **Shared namespace.** A GitCredential with `jobAccess` and a StoredSecret cannot share a name (see [Name rules](#name-rules-for-jobaccess)).
- **No private CA flag for GHES** in this version.

#### Several organizations in one job

An installation token covers one installation, which is one organization. A job that fetches private repositories of two organizations (for example `go mod tidy` with modules from `acme` and `widget`) needs two credentials and must send each organization's requests with its own token. The `git-auth-run.yaml` template does this with a git credential helper that reads each token from an environment variable, set only for the one step; nothing is written to disk and no token is placed in a URL or in argv.

```yaml
# Two credentials, one per organization
apiVersion: unified-cd/v1
kind: GitCredential
metadata: { name: acme-app }
spec:
  host: github.com
  type: githubApp
  secretRef: acme-app-key
  controllerUse: false
  githubApp: { appId: 111111 }
  jobAccess:
    folders: [team-a]
    repositories: ["acme/*"]
    permissions: { contents: read }
---
apiVersion: unified-cd/v1
kind: GitCredential
metadata: { name: widget-app }
spec:
  host: github.com
  type: githubApp
  secretRef: widget-app-key
  controllerUse: false
  githubApp: { appId: 222222 }
  jobAccess:
    folders: [team-a]
    repositories: ["widget/*"]
    permissions: { contents: read }
```

```yaml
# In a job under team-a/
steps:
  - name: tidy
    uses:
      job: git://github.com/your-org/ci-templates/git-auth-run.yaml@v1
      with:
        credential_1: acme-app
        owner_1: acme
        credential_2: widget-app
        owner_2: widget
        command: "GOPRIVATE=github.com/acme,github.com/widget go mod tidy"
```

The template takes up to four credential/owner pairs, needs git 2.31 or later, and runs `command` in the same step (unified-cd does not carry environment variables between steps). Owner matching follows git's URL matching, so `owner_N` is compared as a case-sensitive path prefix and must be spelled as it appears in the repository URL. Because `go` runs git with the same environment, the routing applies to module downloads too. A slot with only one of `credential_N` and `owner_N` set fails with `git-auth-run: slot N needs both credential_N and owner_N`.

### Examples

```yaml
---
# GitHub PAT
apiVersion: unified-cd/v1
kind: GitCredential
metadata:
  name: github-org
spec:
  host: github.com
  type: token
  secretRef: GITHUB_TOKEN        # created with: unified-cli secret set GITHUB_TOKEN ghp_...

---
# GitLab SSH key
apiVersion: unified-cd/v1
kind: GitCredential
metadata:
  name: gitlab-internal
spec:
  host: gitlab.example.com
  type: sshKey
  secretRef: GITLAB_SSH_KEY      # created with: unified-cli secret set GITLAB_SSH_KEY -f ~/.ssh/id_ed25519
  knownHosts: |                  # optional: pin the host key (ssh-keyscan -t ed25519 gitlab.example.com)
    gitlab.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI...
```

```yaml
---
# GitHub App with job access
apiVersion: unified-cd/v1
kind: GitCredential
metadata:
  name: gh-app-release
spec:
  host: github.com
  type: githubApp
  secretRef: github-app-key       # StoredSecret holding the App's PEM private key
  controllerUse: true
  githubApp:
    appId: 123456
    installationId: 7890123       # optional
  jobAccess:
    folders: [team-a, platform/tools]
    repositories: [acme/app, acme/infra-*]
    permissions:
      contents: write
      pull_requests: write
```

Then reference in a job:

```yaml
steps:
  - name: build
    uses:
      job: git://github.com/my-private-org/ci-templates/jobs/build.yaml@v1.0.0
      with:
        target: ./cmd/server
# Credentials for github.com are resolved automatically via the GitCredential above
```

---

