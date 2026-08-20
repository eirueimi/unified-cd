# AppSource

## AppSource

GitOps-style automatic synchronization of resource definitions from a Git repository.
When applied, the controller periodically clones the repository and upserts the supported resource kinds found at the specified path.

```yaml
apiVersion: unified-cd/v1
kind: AppSource
metadata:
  name: <string>                  # required
  annotations:                    # optional
    <key>: <value>
spec:
  repoURL: <string>               # required — Git repository URL (https://, http://, ssh://, or scp-like git@host:path)
  targetRevision: <string>        # required — branch, tag, or commit SHA (must start alphanumeric)
  path: <string>                  # required — directory path inside the repo
  syncPolicy:
    interval: <duration>          # polling interval (default: 5m, minimum: 1m)
    prune: <bool>                 # delete resources from DB when removed from the repo (default: false)
```

### Fields

| Field | Type | Required | Description |
|---|---|---|---|
| `metadata.name` | string | Yes | Unique AppSource name |
| `metadata.annotations` | map[string]string | No | Arbitrary key/value metadata |
| `spec.repoURL` | string | Yes | Git repository URL |
| `spec.targetRevision` | string | Yes | Branch name, tag, or full commit SHA |
| `spec.path` | string | Yes | Directory within the repo to scan for `.yaml`/`.yml` files (recursive) |
| `spec.syncPolicy.interval` | string | No | How often to check for changes (e.g. `5m`, `1h`). Default: `5m`, minimum: `1m` |
| `spec.syncPolicy.prune` | bool | No | If `true`, resources that are removed from the repo are deleted from the controller. Default: `false` |
| `spec.syncPolicy.allowManualOverride` | bool | No | If `true`, disables managed-resource protection for this AppSource's resources (direct apply/delete is allowed). Default: `false` |

### Git field constraints

`spec.repoURL`, `spec.targetRevision`, and `spec.path` flow into a `git`
subprocess argv, so they are restricted to prevent git option injection and
unsupported/dangerous transports. Validation runs twice: once at apply time
(`unified-cli apply` / REST API writes), and again by the AppSource
reconciler every time it reads a spec back out of the store before using it
to build a `git` command — so rows written before this validation existed,
or inserted directly against the store, are still caught before they ever
reach a git-exec site.

- **`spec.repoURL`** must use `https://`, `http://`, `ssh://`, or the
  scp-like `git@host:` form. A schemeless URL, a `-`-leading URL, or a
  `file://`/`ext::` transport is rejected:
  - `repo URL %q must not start with '-'`
  - `repo URL %q must use https://, http://, ssh://, or scp-like user@host: form`

  For the scp-like form, the host component must also start alphanumeric —
  a dash-leading host (e.g. `git@-x:...`) is rejected so it can't smuggle an
  ssh option such as `-oProxyCommand=...`.

- **`spec.targetRevision`** must match the ref charset `[A-Za-z0-9._/+-]`
  and start with an alphanumeric character. This allows branch names, tag
  names, and full commit SHAs, but rejects relative-ref syntax (`HEAD~1`,
  `@{upstream}`) and anything `-`-leading (which could be parsed as a git
  option):
  - `git ref %q contains invalid characters (must start alphanumeric; allowed: A-Z a-z 0-9 . _ / + -)`

- **`spec.path`** must not start with `-`:
  - `spec.path %q must not start with '-'`

### Managed-resource protection

Resources synced by an AppSource (listed in its managed resources) are
protected from direct modification: `unified-cli apply` and REST API
writes/deletes targeting them are rejected with **409 Conflict**, keeping Git
the source of truth. The error names the managing AppSource and its repoURL.

To edit such a resource, change it in the Git repository and let the AppSource
sync it. To intentionally allow manual overrides (e.g. during an incident),
set on the AppSource:

```yaml
spec:
  syncPolicy:
    allowManualOverride: true
```

Notes:

- Matching is exact on `{kind, qualified name}`.
- An AppSource that manages **itself** (app-of-apps root) can always be
  re-applied directly, so a broken Git state stays repairable.
- The guard fails closed: if the controller cannot check the management state
  (DB error), the write is rejected.

### Migrating manually-applied resources to Git

1. `unified-cli export -o ./exported --unmanaged-only`
2. Commit the directory to a Git repository.
3. Apply an AppSource whose `path` points at the exported directory.
4. On the first sync each resource is upserted under its existing name and
   recorded as managed — no manual deletion is needed, and from then on the
   resources are protected from direct writes. Within a sync, Jobs and
   GitCredentials are applied before Schedules and WebhookReceivers, so a
   Schedule or WebhookReceiver that references a Job by name resolves
   correctly on the very first sync regardless of file path order.

### Sync behavior

1. The controller clones or fetches the repository at every `interval`.
2. All `.yaml`/`.yml` files under `path` are scanned recursively.
3. AppSource syncs `Job`, `Schedule`, `WebhookReceiver`, `GitCredential`, and `AppSource` documents found (recursively) under `spec.path`. Files of other kinds, or files that fail to parse, are skipped with a per-file warning; the rest of the sync continues.
4. Files are applied in two passes — GitCredentials and Jobs first, then Schedules, WebhookReceivers, and AppSources — so cross-references (e.g. a Schedule's `job`) resolve on the first sync. Within each pass, files are processed in sorted path order. If two files declare the same kind and name, the first (lexicographically earliest path) wins and the rest are skipped with a warning.
5. If `prune: true`, resources that were previously managed by this AppSource but no longer appear in the repo are deleted. Pruning a nested `AppSource` removes only that AppSource; the resources it managed are left in place (non-cascading, matching Argo CD's default).

Do not manage the same resource from two AppSources — the last sync wins.

**Private repositories:** authentication is resolved automatically by matching the host of `spec.repoURL` against a registered [GitCredential](git-credential.md#gitcredential) (`spec.host`). There is no per-AppSource credential field — register a `GitCredential` for the repo's host and it applies to every AppSource (and `git://` template) using that host.

`secretRef` fields (on `GitCredential`/`WebhookReceiver`) reference a `StoredSecret` by name. Secret values are never stored in Git; create them with `unified-cli secret set` before syncing.

`spec.syncPolicy.interval` has a minimum of `1m`; values below that are rejected.

### Triggering a sync out of band

AppSource is poll-driven, but two mechanisms let you force a sync between ticks
(both reset `lastCommit` so the next reconciler tick re-syncs; neither waits for
the sync to complete):

```bash
# 1. Manual sync via the CLI (requires the bearer token)
unified-cli appsource sync my-pipelines

#    …or the equivalent raw API call
curl -X POST http://localhost:8080/api/v1/appsources/my-pipelines/sync \
  -H "Authorization: Bearer $TOKEN"
```

2. **Push-driven sync via a webhook** — point a Git provider's webhook at a
   [WebhookReceiver](webhook-receiver.md#webhookreceiver) whose `trigger.appSource` names this
   AppSource. This needs no admin token (it is authenticated by signature) and
   is the recommended way to make GitOps sync react to pushes instead of waiting
   for the poll interval.

### Examples

```yaml
---
# Public repository, track main branch
apiVersion: unified-cd/v1
kind: AppSource
metadata:
  name: team-pipelines
spec:
  repoURL: https://github.com/my-org/cd-definitions
  targetRevision: main
  path: jobs/
  syncPolicy:
    interval: 5m
    prune: false

---
# Private repository — auth resolved via a GitCredential whose spec.host matches github.com
apiVersion: unified-cd/v1
kind: AppSource
metadata:
  name: private-pipelines
spec:
  repoURL: https://github.com/my-org/private-ci
  targetRevision: production
  path: pipelines/
  syncPolicy:
    interval: 10m
    prune: true                   # delete jobs removed from the repo
```
