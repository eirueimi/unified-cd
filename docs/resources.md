# Resource Reference

Complete schema reference for all unified-cd resource kinds.

All resources use `apiVersion: unified-cd/v1` and are applied with `unified-cd apply -f <file>`.

## Table of Contents

- [Job](#job)
- [Schedule](#schedule)
- [WebhookReceiver](#webhookreceiver)
- [GitCredential](#gitcredential)
- [AppSource](#appsource)

---

## Job

The primary unit of work. See [Job Reference](jobs.md) for the full feature guide.

```yaml
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: <string>                  # required
  labels:                         # optional
    <key>: <value>
spec:
  params:
    inputs:
      - name: <string>            # required
        type: string | bool | int # required
        required: <bool>
        default: <any>
        description: <string>
    outputs:
      - name: <string>
        type: string | bool | int | artifact
  agentSelector:
    - <label>                     # e.g. "kind:linux"
  concurrency:
    mutex: <string>
    namedLocks:
      - pool: <string>
        capacity: <int>
    orLocks:
      - name: <string>
        candidates:
          - <string>
  failFast: <bool>                # default: true
  timeoutMinutes: <number>
  podTemplate:                    # k8s-agent only — see Kubernetes Integration Guide
    name: <string>
    spec: <PodSpec map>
    workspace:
      mountPath: <string>
      pvc:
        claimName: <string>
        storageClassName: <string>
        storageRequest: <string>
        accessMode: ReadWriteOnce | ReadOnlyMany | ReadWriteMany
    reuse: <bool>
    cleanWorkspace: <bool>
    override:
      containers: [<map>]
      volumes: [<map>]
  steps:
    - name: <string>              # required, unique within the job
      if: <CEL expression>        # e.g. params.env == "production"; see jobs.md
      needs: [<step-name>, ...]
      env:
        <KEY>: <value>            # supports {{ secrets.NAME }} and {{ .Params.X }}
      run: <shell script>
      outputs:
        <key>: <template expression>
      call:
        job: <job-name>
        with:
          <key>: <value>
      uses:
        job: git://<host>/<owner>/<repo>/<path>@<ref>
        with:
          <key>: <value>
      cache:
        path: <string>
        key: <string>
        restoreKeys: [<string>, ...]
        ttlDays: <int>            # default: 30
      uploadArtifact:
        name: <string>
        path: <string>
      downloadArtifact:
        name: <string>
        destDir: <string>         # default: current directory
      post:
        run: <shell script>
        env:
          <KEY>: <value>
      container: <string>         # k8s multi-container: target container name
      continueOnError: <bool>     # default: false
      timeoutMinutes: <number>
```

---

## Schedule

Triggers a job on a cron schedule.

```yaml
apiVersion: unified-cd/v1
kind: Schedule
metadata:
  name: <string>                  # required
spec:
  cron: <string>                  # required — 5-field cron expression
  job: <string>                   # required — name of the Job to trigger
  params:                         # optional — parameters passed to the triggered run
    <key>: <value>
```

### Fields

| Field | Type | Required | Description |
|---|---|---|---|
| `metadata.name` | string | Yes | Unique schedule name |
| `spec.cron` | string | Yes | 5-field cron expression: `min hour day month weekday` |
| `spec.job` | string | Yes | Name of the registered Job to trigger |
| `spec.params` | map[string]string | No | Input parameters to pass to the triggered run |

### Cron expression format

```
┌─ minute        (0-59)
│  ┌─ hour       (0-23)
│  │  ┌─ day     (1-31)
│  │  │  ┌─ month (1-12)
│  │  │  │  ┌─ weekday (0-6, 0=Sunday)
│  │  │  │  │
*  *  *  *  *
```

| Example | Meaning |
|---|---|
| `0 2 * * *` | Every day at 02:00 UTC |
| `*/15 * * * *` | Every 15 minutes |
| `0 9 * * 1-5` | Weekdays at 09:00 UTC |
| `0 0 1 * *` | First day of every month |

If the controller is down during a scheduled fire time, the fire is caught up within 1 hour after restart.

Apply a Schedule the same way as any other resource:

```bash
unified-cd apply -f schedule.yaml
```

Runs triggered by a Schedule show up with `triggeredBy: schedule:<name>`.

### Example

```yaml
apiVersion: unified-cd/v1
kind: Schedule
metadata:
  name: nightly-build
spec:
  cron: "0 2 * * *"
  job: build
  params:
    tag: nightly
    deploy_env: staging
```

---

## WebhookReceiver

Accepts incoming HTTP webhooks and triggers a job.

```yaml
apiVersion: unified-cd/v1
kind: WebhookReceiver
metadata:
  name: <string>                  # required
spec:
  trigger:                        # exactly one of job / appSource
    job: <string>                 # trigger a Job (creates a Run)
    appSource: <string>           # OR force a GitOps re-sync of an AppSource
  auth:
    type: none | hmac-sha256 | github | token
    secretRef: <string>           # name of StoredSecret (required unless type is none)
    header: <string>              # token type only — header to compare (default X-Gitlab-Token)
  filters:                        # optional — all must match for the job to trigger
    - <template expression>
  paramsMapping:                  # optional — map webhook payload fields to job inputs
    <param-name>: <template expression>
```

### Fields

| Field | Type | Required | Description |
|---|---|---|---|
| `metadata.name` | string | Yes | Unique receiver name. Also the URL path segment: `POST /webhook/<name>` |
| `spec.trigger.job` | string | Cond. | Name of the Job to trigger. Exactly one of `job` / `appSource` is required |
| `spec.trigger.appSource` | string | Cond. | Name of an AppSource to force-sync (resets its `lastCommit` so the next reconciler tick re-syncs). Exactly one of `job` / `appSource` is required |
| `spec.auth.type` | string | Yes | Authentication method (see below) |
| `spec.auth.secretRef` | string | No | Name of a StoredSecret containing the HMAC key (required for `hmac-sha256` and `github`) |
| `spec.filters` | []string | No | Template expressions that must all evaluate to `true` for the trigger to fire (applies to both `job` and `appSource` triggers) |
| `spec.paramsMapping` | map[string]string | No | Maps payload fields to job input parameter names. Ignored for `appSource` triggers |

### Authentication types

| Type | Description |
|---|---|
| `none` | No signature verification. Use only for trusted internal sources. |
| `hmac-sha256` | Verifies `X-Signature: sha256=<hex hmac>` (or GitHub-compatible `X-Hub-Signature-256`) over the raw request body using the secret from `secretRef` |
| `github` | Verifies GitHub's `X-Hub-Signature-256` header using the secret from `secretRef` |
| `token` | Verifies a plaintext shared-secret token sent in a header (default `X-Gitlab-Token`, configurable via `auth.header`) by constant-time comparison against `secretRef`. Use for GitLab and other services that send a raw token header instead of an HMAC signature. |

### Delivery responses

| Result | HTTP status |
|---|---|
| Run created (`job` trigger) | `200` + run JSON |
| AppSource re-sync scheduled (`appSource` trigger) | `202` + `{"appSource","status"}` |
| Filters did not match (no run / no sync) | `204` |
| Signature invalid or missing | `401` |
| Required job param not produced by `paramsMapping`, or `appSource` not found | `400` (body names the cause) |

### Webhook endpoint

```
POST http://<controller>/webhook/<receiver-name>
```

This endpoint takes no bearer token; it is authenticated by the `auth` check
alone. The request body must be **raw JSON** (`Content-Type: application/json`) —
it is parsed directly as the `.Payload`. Form-encoded bodies
(`application/x-www-form-urlencoded`, which GitHub sends as `payload=<json>`)
fail JSON parsing and return `400`. For GitHub, set the webhook's **Content
type** to `application/json`; see the [Getting Started webhook
walkthrough](getting-started.md#configuring-the-webhook-on-github).

### Template variables in filters and paramsMapping

| Variable | Type | Description |
|---|---|---|
| `.Payload` | map | The parsed JSON webhook body |
| `.Headers` | map | Request headers |

### Examples

```yaml
---
# GitHub push webhook: trigger build on push to main
apiVersion: unified-cd/v1
kind: WebhookReceiver
metadata:
  name: github-push
spec:
  trigger:
    job: build
  auth:
    type: github
    secretRef: GITHUB_WEBHOOK_SECRET
  filters:
    - '{{ eq .Payload.ref "refs/heads/main" }}'
  paramsMapping:
    image: myapp
    tag: "{{ .Payload.after }}"    # commit SHA

---
# Generic HMAC webhook
apiVersion: unified-cd/v1
kind: WebhookReceiver
metadata:
  name: deploy-trigger
spec:
  trigger:
    job: deploy
  auth:
    type: hmac-sha256
    secretRef: WEBHOOK_SECRET
  paramsMapping:
    env: "{{ .Payload.environment }}"
    version: "{{ .Payload.version }}"

---
# No auth (internal use only)
apiVersion: unified-cd/v1
kind: WebhookReceiver
metadata:
  name: internal-trigger
spec:
  trigger:
    job: cleanup
  auth:
    type: none

---
# GitLab push webhook: verify the X-Gitlab-Token secret token
apiVersion: unified-cd/v1
kind: WebhookReceiver
metadata:
  name: gitlab-push
spec:
  trigger:
    job: build
  auth:
    type: token
    secretRef: GITLAB_WEBHOOK_TOKEN
  filters:
    - '{{ eq .Payload.ref "refs/heads/main" }}'
  paramsMapping:
    git_ref: "{{ .Payload.checkout_sha }}"

---
# GitHub push webhook: force a GitOps re-sync of an AppSource on push to main
apiVersion: unified-cd/v1
kind: WebhookReceiver
metadata:
  name: gitops-sync
spec:
  trigger:
    appSource: my-pipelines      # instead of job — force-syncs this AppSource
  auth:
    type: github
    secretRef: github-webhook-secret
  filters:
    - '{{ eq .Payload.ref "refs/heads/main" }}'
```

An `appSource` trigger resets the AppSource's `lastCommit`, so the next
reconciler tick (≤30s) re-syncs from Git — turning the otherwise poll-only
[AppSource](#appsource) into a push-driven sync. It does not wait for the sync
to finish; it responds `202` immediately.

---

## GitCredential

Stores Git authentication credentials for private repositories, used with `git://` template URIs and AppSource.

```yaml
apiVersion: unified-cd/v1
kind: GitCredential
metadata:
  name: <string>                  # required
spec:
  host: <string>                  # required — hostname to apply credentials to
  type: token | sshKey            # required
  secretRef: <string>             # required — name of StoredSecret containing the credential
```

### Fields

| Field | Type | Required | Description |
|---|---|---|---|
| `metadata.name` | string | Yes | Unique credential name |
| `spec.host` | string | Yes | Hostname to apply the credential to (e.g. `github.com`, `gitlab.example.com`) |
| `spec.type` | string | Yes | `token` for HTTP PAT/OAuth token, `sshKey` for SSH private key |
| `spec.secretRef` | string | Yes | Name of a StoredSecret holding the actual credential value |

### Credential matching

When resolving a `git://` URI or AppSource `repoURL`, the controller looks up a GitCredential whose `spec.host` matches the URI's hostname. This allows job definitions to reference private templates without embedding credentials.

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
  secretRef: GITHUB_TOKEN        # created with: unified-cd secret set GITHUB_TOKEN ghp_...

---
# GitLab SSH key
apiVersion: unified-cd/v1
kind: GitCredential
metadata:
  name: gitlab-internal
spec:
  host: gitlab.example.com
  type: sshKey
  secretRef: GITLAB_SSH_KEY      # created with: unified-cd secret set GITLAB_SSH_KEY -f ~/.ssh/id_ed25519
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

## AppSource

GitOps-style automatic synchronization of resource definitions from a Git repository.
When applied, the controller periodically clones the repository and upserts the supported resource kinds found at the specified path.

```yaml
apiVersion: unified-cd/v1
kind: AppSource
metadata:
  name: <string>                  # required
spec:
  repoURL: <string>               # required — Git repository URL (HTTPS or SSH)
  targetRevision: <string>        # required — branch, tag, or commit SHA
  path: <string>                  # required — directory path inside the repo
  gitCredentialRef: <string>      # optional — GitCredential name for private repos
  syncPolicy:
    interval: <duration>          # polling interval (default: 5m, minimum: 1m)
    prune: <bool>                 # delete resources from DB when removed from the repo (default: false)
```

### Fields

| Field | Type | Required | Description |
|---|---|---|---|
| `metadata.name` | string | Yes | Unique AppSource name |
| `spec.repoURL` | string | Yes | Git repository URL |
| `spec.targetRevision` | string | Yes | Branch name, tag, or full commit SHA |
| `spec.path` | string | Yes | Directory within the repo to scan for YAML files (recursive) |
| `spec.gitCredentialRef` | string | No | Name of a GitCredential resource for private repository access |
| `spec.syncPolicy.interval` | string | No | How often to check for changes (e.g. `5m`, `1h`). Default: `5m`, minimum: `1m` |
| `spec.syncPolicy.prune` | bool | No | If `true`, resources that are removed from the repo are deleted from the controller. Default: `false` |

### Sync behavior

1. The controller clones or fetches the repository at every `interval`.
2. All `.yaml` files under `path` are scanned recursively.
3. AppSource syncs `Job`, `Schedule`, `WebhookReceiver`, `GitCredential`, and `AppSource` documents found (recursively) under `spec.path`. Files of other kinds, or files that fail to parse, are skipped with a per-file warning; the rest of the sync continues.
4. Files are processed in sorted path order. If two files declare the same kind and name, the first (lexicographically earliest path) wins and the rest are skipped with a warning.
5. If `prune: true`, resources that were previously managed by this AppSource but no longer appear in the repo are deleted. Pruning a nested `AppSource` removes only that AppSource; the resources it managed are left in place (non-cascading, matching Argo CD's default).

Do not manage the same resource from two AppSources — the last sync wins.

`secretRef` fields (on `GitCredential`/`WebhookReceiver`) reference a `StoredSecret` by name. Secret values are never stored in Git; create them with `unified-cd secret set` before syncing.

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
   [WebhookReceiver](#webhookreceiver) whose `trigger.appSource` names this
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
# Private repository with GitCredential
apiVersion: unified-cd/v1
kind: AppSource
metadata:
  name: private-pipelines
spec:
  repoURL: https://github.com/my-org/private-ci
  targetRevision: production
  path: pipelines/
  gitCredentialRef: github-org    # references GitCredential named "github-org"
  syncPolicy:
    interval: 10m
    prune: true                   # delete jobs removed from the repo
```
