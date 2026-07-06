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
    semaphores:
      - pool: <string>
        capacity: <int>
    orLocks:
      - name: <string>
        in:                       # list of candidate values, or a $param expression
          - <string>
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
      runsIn:                     # see "runsIn" below
        image: <string>
        container: <string>
        resources:
          requests: { cpu: <string>, memory: <string> }
          limits: { cpu: <string>, memory: <string> }
      continueOnError: <bool>     # default: false
      timeoutMinutes: <number>
    - parallel:                   # OR: a group of steps that run concurrently; see jobs.md
        - name: <string>          # ("Concurrent Steps (parallel)")
          run: <shell script>
```

### `runsIn`

`runsIn` declares an isolated execution context for a step. `image` and
`container` are mutually exclusive; a step with no `runsIn` (or `runsIn: null`)
runs in the default/shared environment (the host agent process, or the
default pod container on k8s).

| Field | Behavior |
|---|---|
| `runsIn.image` | Run in a fresh, isolated environment from this image: the standard agent runs `<runtime> run --rm`, the k8s agent spins up a throwaway pod. This environment does **not** share the job workspace — it is a "pure function" call. Pass inputs via `with:`/`env`, and read outputs via `outputs:`/stdout. |
| `runsIn.container` | Exec into a pre-provisioned, named container (k8s pod only — a hard error on the standard agent). |
| `runsIn.resources` | Optional CPU/memory `requests`/`limits` (Kubernetes quantity strings, e.g. `"500m"`, `"1"`, `"256Mi"`, `"1Gi"`) applied to a `runsIn.image` step's container. |

`runsIn` can be set on a plain step or on a `uses` step; the two placements
have different meanings — see the next section.

#### Step-level vs. uses-level `runsIn.image`

- **Step-level** `runsIn.image` (on a `run` step): unchanged, single throwaway
  isolated call as described above. No artifact/cache support — it is a pure
  function with no persistent filesystem.
- **Uses-level** `runsIn.image` (on a `uses:` step): **scope mode**. The whole
  inlined template runs inside **one** isolated environment (a "scope") that
  stays alive for all of the template's steps, instead of each inlined step
  getting its own independent throwaway environment.
- **Uses-level** `runsIn.container`: unchanged — exec into a named
  pre-provisioned container; not scope mode.
- A `uses` step without `runsIn`: unchanged current inlining behavior.

#### Uses-level scope: artifacts & cache in the isolated environment

When a `uses:` step declares `runsIn.image`, every step inlined from that
template shares one isolated scope environment (one container on the standard
agent, one dedicated pod on k8s). `cache`, `uploadArtifact`, and
`downloadArtifact` steps inside that scope operate on **the scope's own
filesystem**, not the outer job workspace — so a template that builds
something in its isolated environment can save/restore that output as an
artifact or cache entry without ever touching the outer workspace.

The scope does not share the outer job workspace; it starts from a fresh,
empty filesystem:

- **Inputs** enter the scope via `with:` (environment variables, as with any
  `uses`) and `downloadArtifact` (which writes into the scope's filesystem).
- **Outputs** leave the scope via `uploadArtifact` (pushed to the run's
  artifact store) and `outputs:`/stdout.

Because artifacts are keyed by run, not by workspace path, they cross the
isolation boundary naturally — on Kubernetes this means no shared
`ReadWriteMany` volume is required for a scoped `uses`.

```yaml
steps:
  - name: build-in-container
    uses:
      job: git://github.com/my-org/ci-templates/jobs/build.yaml@v1
      with:
        target: ./cmd/server
      runsIn:
        image: golang:1.22
```

If `build.yaml` contains a `download-deps` step with `cache:`, a `compile`
step with `run:`, and a `save-binary` step with `uploadArtifact:`, all three
run inside the same `golang:1.22` scope: the cache is restored into and saved
from the scope's filesystem, the compile step writes into that same
filesystem, and the artifact upload reads the compiled binary from it — none
of it touches the outer job's workspace.

Under `matrix`/`foreach`, each variant of a scoped `uses` step gets its own,
independent scope instance (its own container/pod), so matrix variants never
share isolated state.

**Validation — the following are parse errors inside a scoped `uses` (a `uses`
whose own `runsIn.image` is set), because they are incompatible with holding
one isolated environment open for the template's steps:**

| Inside a scoped `uses` | Why it's a parse error |
|---|---|
| A nested `runsIn.image` or `runsIn.container` on an inlined step | A scope must be homogeneous — one environment for the whole template, not a per-step override. |
| An `approval:` step | An approval pause would hold the scope's container/pod alive across a human wait (up to the approval timeout), wasting resources and risking a k8s pod deadline killing it mid-wait. |
| A `call:` step | `call:` spawns a separate child run on another agent/workspace that cannot see the scope's isolated filesystem — undefined semantics inside a scope. |

These checks apply to both concrete steps and members of a `parallel:` block,
and are inert outside scope mode — a plain `uses` or a `uses` with
`runsIn.container` still allows `approval:`/`call:` unchanged. `parallel:`
sub-steps inside a scoped `uses` execute concurrently, but all still target
the same shared scope environment.

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
| `spec.syncPolicy.interval` | string | No | How often to check for changes (e.g. `5m`, `1h`). Default: `5m`, minimum: `1m` |
| `spec.syncPolicy.prune` | bool | No | If `true`, resources that are removed from the repo are deleted from the controller. Default: `false` |
| `spec.syncPolicy.allowManualOverride` | bool | No | If `true`, disables managed-resource protection for this AppSource's resources (direct apply/delete is allowed). Default: `false` |

### Managed-resource protection

Resources synced by an AppSource (listed in its managed resources) are
protected from direct modification: `unified-cd apply` and REST API
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

1. `unified-cd export -o ./exported --unmanaged-only`
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
2. All `.yaml` files under `path` are scanned recursively.
3. AppSource syncs `Job`, `Schedule`, `WebhookReceiver`, `GitCredential`, and `AppSource` documents found (recursively) under `spec.path`. Files of other kinds, or files that fail to parse, are skipped with a per-file warning; the rest of the sync continues.
4. Files are applied in two passes — GitCredentials and Jobs first, then Schedules, WebhookReceivers, and AppSources — so cross-references (e.g. a Schedule's `job`) resolve on the first sync. Within each pass, files are processed in sorted path order. If two files declare the same kind and name, the first (lexicographically earliest path) wins and the rest are skipped with a warning.
5. If `prune: true`, resources that were previously managed by this AppSource but no longer appear in the repo are deleted. Pruning a nested `AppSource` removes only that AppSource; the resources it managed are left in place (non-cascading, matching Argo CD's default).

Do not manage the same resource from two AppSources — the last sync wins.

**Private repositories:** authentication is resolved automatically by matching the host of `spec.repoURL` against a registered [GitCredential](#gitcredential) (`spec.host`). There is no per-AppSource credential field — register a `GitCredential` for the repo's host and it applies to every AppSource (and `git://` template) using that host.

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
