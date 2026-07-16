# Job Template Collection

A collection of reusable **`kind: JobTemplate`** resources for `unified-cd`. A `uses:` step
inlines a template's steps into YOUR run (sharing its pod, workspace, and secrets), fetching
it via a `git://` URI:

```yaml
# Inline a template into this run
steps:
  - name: notify
    uses:
      job: git://github.com/your-org/ci-templates/slack-notify.yaml@v1
      with:
        status: success
        job_name: my-job
        # with: values are Go templates expanded against the calling job's
        # .Params and .Steps. There is NO variable for the caller's own run
        # ID, so pass a static identifier here.
        run_id: "my-job"
```

`uses:` targets **must be `kind: JobTemplate`** — a strict schema containing only what
inlining can honor: `description`, `params`, `shell`, `podTemplate.spec.containers`/`volumes`,
`steps`, and `finally` (spliced into the *caller's* finally phase, not run standalone —
see [docs/jobs.md](../docs/jobs.md#template-finally-splice-into-the-caller)). Anything else
(`agentSelector`, `concurrency`, `native`, other podTemplate fields) is rejected at run
creation. A template's `podTemplate` containers and volumes are merged into the caller's pod
automatically (the caller's own same-name definition wins; the reserved names `job`,
`unified-artifact`, `ucd-shim`, `workspace`, `ucd-tools` cannot be injected, and every
container/volume name must be a valid DNS-1123 label). See [docs/jobs.md](../docs/jobs.md)
for the full contract.

**Want a child run instead of inlining?** `call:` runs a REGISTERED `kind: Job` by name (its
own pod/agent/run). A `JobTemplate` cannot be registered with `apply` — if you need a template
as a callable job, write a thin `kind: Job` wrapper in your own repo that `uses:` the template
and declares the run-level fields (`agentSelector`, etc.) yourself, then `apply` + `call:` that
wrapper. The one exception in this collection is `buildkit-rootless-build-push.yaml`, which
stays `kind: Job` (it replaces the primary `job` container in its own pod — something `uses:`
deliberately forbids) and is meant to be applied and invoked via `call:` directly.

## Template list

| Template | Purpose | Required tools | Recommended agent label | Secrets used |
|---|---|---|---|---|
| `git-checkout.yaml` | Clone/checkout a Git repository (supports HTTPS/SSH, LFS, sparse checkout, submodules) | git, (git-lfs) | `git:true` | Specified via `token_secret` (e.g. github-token) / `ssh_key_secret` |
| `slack-notify.yaml` | Notify a Slack Incoming Webhook | curl | - | `slack-webhook-url` |
| `github-commit-status.yaml` | Update a GitHub commit status | curl | - | `github-token` |
| `notify-webhook.yaml` | Send a generic JSON POST notification to a webhook | curl | - | Specified via `url_secret` (plain `url` if omitted) |
| `notify-email.yaml` | Send email notifications via SMTP | curl (SMTP(S)-enabled build) | - | Specified via `smtp_url_secret`, `username_secret`, `password_secret` |
| `github-pr-comment.yaml` | Post a comment on a GitHub PR/Issue | curl | - | `token_secret` (default: `github-token`) |
| `gitlab-commit-status.yaml` | Update a GitLab commit status | curl | - | `token_secret` (default: `gitlab-token`) |
| `docker-build-push.yaml` | Build & push a Docker image (supports buildx multi-platform) | docker, (docker buildx) | `docker:true` | Specified via `username_secret` / `password_secret` (optional) |
| `buildkit-rootless-build-push.yaml` | Build & push an image with rootless BuildKit — no privileged; native multi-platform. Auto-pins to a Kubernetes agent | (bundles its own buildkitd sidecar) | auto (k8s) | Specified via `username_secret` / `password_secret` (optional) |
| `setup-go.yaml` | Set up Go module/build cache | go | `go:true` | none |
| `setup-node.yaml` | Set up Node.js dependency cache (npm ci) | node, npm | `node:true` | none |
| `github-release.yaml` | Create a GitHub release & upload assets (curl only, no gh required) | curl | - | `token_secret` (default: `github-token`) |
| `semver-bump.yaml` | Compute the next version based on Conventional Commits | git | `git:true` | none |
| `k8s-deploy.yaml` | Apply Kubernetes manifests & wait for rollout | kubectl | `kubectl:true` | Specified via `kubeconfig_secret` |
| `helm-upgrade.yaml` | Helm upgrade --install | helm, kubectl | `helm:true`, `kubectl:true` | Specified via `kubeconfig_secret` |
| `rsync-deploy.yaml` | Remote deploy via rsync | rsync, ssh | `rsync:true` | Specified via `ssh_key_secret` |
| `s3-sync.yaml` | Sync to S3-compatible object storage (AWS/MinIO/Garage) | aws (AWS CLI v2) | `aws:true` | Specified via `access_key_secret` / `secret_key_secret` |
| `smoke-check.yaml` | Smoke test via URL polling after deployment | curl | - | none |
| `unity-build.yaml` | Unity batch-mode build (Android/iOS/WebGL, etc.) | Unity Editor | `unity:true` | Specified via `license_*_secret` (optional) |
| `fastlane-upload.yaml` | Run a fastlane lane (supports App Store Connect API key) | fastlane, bundler, Xcode | `macos:true`, `fastlane:true` | Specified via `asc_*_secret` (optional) |
| `google-play-upload.yaml` | Upload an AAB to Google Play (fastlane supply) | fastlane | `fastlane:true` | Specified via `service_account_json_secret` |

## Conventions

Each template follows the house style below (`git-checkout.yaml` / `slack-notify.yaml` are the prototypes):

- `apiVersion: unified-cd/v1`, `kind: JobTemplate` (the standalone-job exception is
  `buildkit-rootless-build-push.yaml`, which is `kind: Job` — see above). Declare `name` /
  `type` / `required` / `default` / `description` under `spec.params.inputs`. Write
  `description` in English. `internal/dsl`'s `TestTemplatesParse` gate-checks every file in
  this directory against its intended schema.
- In the header comment at the top of the file, state the template's purpose, the required secrets (with an example
  `unified-cd secret set ...` invocation), and a usage example for referencing it as a `git://` template.
- Document tool prerequisites (docker, kubectl, helm, aws CLI, fastlane, Unity Editor, etc.) in the header comment.
  The agent labels required to run it (e.g. `docker:true`, `kubectl:true`, `unity:true`) listed in this README's table
  are **a naming convention, not something enforced as `agentSelector`** (each user sets `agentSelector` on their own job).
- Map parameters and secrets into `env:` first, then use them from a POSIX `sh` script (`set -eu`, no bashisms).
  Do not interpolate them directly into shell code.
- Indirect reference pattern for optional secrets:
  `"{{ if .Params.token_secret }}{{ index .Secrets .Params.token_secret }}{{ end }}"`
- When writing sensitive information such as private keys or tokens to a file, create a temp file with `mktemp`,
  `chmod 600` it, and clean it up with `trap ... EXIT`.
- The `path` / `key` / `restoreKeys` fields of a `cache:` step all expand template expressions (use
  `{{ hashFile "path/glob" }}` for the key; note that no function named `checksum` exists despite what some docs
  suggest) — see `executeCacheStep` in `internal/agent/agent.go` and the equivalent in `internal/k8sagent/agent.go`.
  A `cache:` step may only target a single `path`, though, so when you need to cache more than one directory (e.g.
  Go's module cache and build cache), split it into separate steps per target as in `setup-go.yaml`.
- For `type: array` input parameters, the value passed as a YAML array is delivered to the job at runtime as a
  newline-separated string in the environment variable (see `sparse_paths` in `git-checkout.yaml`). Declare the
  default value as an empty string `default: ""` rather than an empty array (using a string default even for
  array-typed params is the existing convention).
- **Isolation and native jobs.** Jobs are isolated by default: an unmarked job (or a job that calls/inlines a
  template without `native: true`) runs its steps in a container, so a template can only use tools the runner
  image (or the job's own `podTemplate`) actually provides. Templates that need a host toolchain —
  `fastlane-upload.yaml`, `google-play-upload.yaml`, `unity-build.yaml`, `docker-build-push.yaml` — must be
  called from a `native: true` job; note that requirement in the template's own header comment alongside its
  other tool prerequisites. Container-friendly templates (curl/git/go/node-based ones) run fine in the default
  isolated job and need no `native: true`. When a template's steps target a specific `podTemplate` container,
  use the flat `container:` field — step-level `runsIn:` was removed (see the
  [job-isolation migration guide](../docs/migration-2026-07-job-isolation.md)).
