# WebhookReceiver

## WebhookReceiver

Accepts incoming HTTP webhooks and triggers a job.

```yaml
apiVersion: unified-cd/v1
kind: WebhookReceiver
metadata:
  name: <string>                  # required
  annotations:                    # optional
    <key>: <value>
spec:
  trigger:                        # exactly one of job / appSource
    job: <string>                 # trigger a Job (creates a Run)
    appSource: <string>           # OR force a GitOps re-sync of an AppSource
  auth:                           # required — an omitted auth block is a parse error
    type: none | hmac-sha256 | github | token
    secretRef: <string>           # name of StoredSecret (required unless type is none)
    header: <string>              # token type only — header to compare (default X-Gitlab-Token)
    allowUnauthenticated: <bool>  # required (must be true) alongside type: none
  filters:                        # optional — all must match for the job to trigger
    - <template expression>
  paramsMapping:                  # optional — map webhook payload fields to job inputs
    <param-name>: <template expression>
```

### Fields

| Field | Type | Required | Description |
|---|---|---|---|
| `metadata.name` | string | Yes | Unique receiver name. Also the URL path segment: `POST /webhook/<name>` |
| `metadata.annotations` | map[string]string | No | Arbitrary key/value metadata |
| `spec.trigger.job` | string | Cond. | Name of the Job to trigger. Exactly one of `job` / `appSource` is required |
| `spec.trigger.appSource` | string | Cond. | Name of an AppSource to force-sync (resets its `lastCommit` so the next reconciler tick re-syncs). Exactly one of `job` / `appSource` is required |
| `spec.auth` | object | Yes | Authentication config. Omitting this block entirely is a parse error — see [Authentication types](#authentication-types) |
| `spec.auth.type` | string | Yes | Authentication method (see below) |
| `spec.auth.secretRef` | string | No | Name of a StoredSecret; required unless `type` is `none`. Holds the HMAC key or shared token, depending on `type` |
| `spec.auth.allowUnauthenticated` | bool | Cond. | Required (must be `true`) when `type` is `none`; parsing fails otherwise. Makes an unauthenticated public trigger a deliberate, greppable choice rather than an accident |
| `spec.filters` | []string | No | Template expressions that must all evaluate to `true` for the trigger to fire (applies to both `job` and `appSource` triggers) |
| `spec.paramsMapping` | map[string]string | No | Maps payload fields to job input parameter names. Ignored for `appSource` triggers |

### Authentication types

| Type | Description |
|---|---|
| `none` | No signature verification. Requires `allowUnauthenticated: true` alongside it — an unauthenticated webhook lets anyone trigger the job, so it must be a deliberate, greppable opt-in. Use only for trusted internal sources. |
| `hmac-sha256` | Verifies `X-Signature: sha256=<hex hmac>` (or GitHub-compatible `X-Hub-Signature-256`) over the raw request body using the secret from `secretRef` |
| `github` | Verifies GitHub's `X-Hub-Signature-256` header using the secret from `secretRef` |
| `token` | Verifies a plaintext shared-secret token sent in a header (default `X-Gitlab-Token`, configurable via `auth.header`) by constant-time comparison against `secretRef`. Use for GitLab and other services that send a raw token header instead of an HMAC signature. |

### Migration: `spec.auth` is now required

**Breaking change.** Previously, an omitted `auth:` block silently defaulted to
`type: none` — meaning it was possible to publish an unauthenticated remote
trigger for a job just by forgetting the `auth:` block. `spec.auth` is now a
required field: a `WebhookReceiver` document with no `auth:` block fails to
parse with an error naming the fix.

If you have a receiver that intentionally relied on the old implicit
`none` behavior:

- **Preferred:** adopt a real auth type (`hmac-sha256`, `github`, or `token`)
  with a `secretRef`.
- **If the receiver genuinely must stay unauthenticated** (e.g. a trusted
  internal-only trigger), add `allowUnauthenticated: true` alongside
  `type: none`:

  ```yaml
  auth:
    type: none
    allowUnauthenticated: true
  ```

  so the choice is explicit and greppable rather than an accident.

This only affects parsing (`unified-cli apply` / GitOps sync of a
`WebhookReceiver` manifest). It does not change verification behavior for
`hmac-sha256`, `github`, or `token` receivers.

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
walkthrough](../../getting-started/quickstart.md#configuring-the-webhook-on-github).

### Template variables in filters and paramsMapping

| Variable | Type | Description |
|---|---|---|
| `.Payload` | map | The parsed JSON webhook body |

### Payload-mapped params must be validated

A valid webhook signature (HMAC, GitHub, or token) proves who sent the
request — it says nothing about whether the request's *content* is benign.
For a GitHub push or pull_request event, fields like `.Payload.ref` or
`.Payload.pull_request.head.ref` are controlled by whoever pushed the branch
or opened the PR, which can be any outside contributor. This is the same
class of vulnerability as GitHub Actions script injection: because param
values are interpolated directly into step shell text (`sh -lc`), an
unconstrained payload-mapped param is a command-injection vector.

For this reason, any `paramsMapping` entry whose template reads `.Payload`
must target a job input that declares `pattern:` (a regular expression the
resolved value must match), `choices:` (a fixed allow-list of values), or
`unvalidated: true` (an explicit, greppable opt-out for values that are
genuinely free-form and never reach a shell). `choices:` satisfies this gate
on its own, with no `pattern:` needed: it's a strict enumeration of exact
allowed values, strictly stronger than any regex could enforce, since a
regex only constrains a value's syntax while choices constrains its
membership outright. This is enforced live, at webhook-ingress time, against
the target job's *current* spec — not at receiver-apply time — because the
job may not exist yet when the receiver is applied, or may be edited
independently afterward. A receiver whose mapping fails this check is
rejected with `400` and no Run is created; the error names the receiver, the
param, and the job:

```
webhook receiver "wh": param "ref" is mapped from the request payload but job "build" declares no pattern for it (add pattern: to the input, choices: to restrict it to a fixed set of values, or unvalidated: true to accept it explicitly)
```

A literal `paramsMapping` value that never references `.Payload` (e.g.
`image: myapp`) is author-controlled, not attacker-controlled, and is not
subject to this requirement. See [Input fields](../writing-jobs/parameters.md#input-fields) for
`pattern`/`choices`/`unvalidated`; a reasonable starting pattern for most
identifiers (branch names, tags, commit SHAs) is `^[A-Za-z0-9._/-]+$`.

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
    image: myapp                   # literal — no pattern needed
    tag: "{{ .Payload.after }}"    # commit SHA — reads .Payload, so the
                                    # `build` job's `tag` input must declare
                                    # pattern: (or unvalidated: true)

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
    # Both read .Payload, so `deploy`'s `env` and `version` inputs must each
    # declare pattern: (or unvalidated: true) or this webhook is rejected at
    # ingress time.
    env: "{{ .Payload.environment }}"
    version: "{{ .Payload.version }}"

---
# No auth (internal use only) — allowUnauthenticated is mandatory here;
# without it, this receiver fails to parse.
apiVersion: unified-cd/v1
kind: WebhookReceiver
metadata:
  name: internal-trigger
spec:
  trigger:
    job: cleanup
  auth:
    type: none
    allowUnauthenticated: true

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
[AppSource](app-source.md#appsource) into a push-driven sync. It does not wait for the sync
to finish; it responds `202` immediately.

---

