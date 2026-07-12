# Job Template Collection

A collection of reusable `Job` templates for `unified-cd`. Register them with `unified-cd apply -f templates/<file>.yaml`
and use them either by calling them via `call:`, or by inlining them via a `git://` URI with `uses:`.

```yaml
# When calling a registered job
steps:
  - name: notify
    call:
      job: slack-notify
      with:
        status: success
        job_name: my-job
        # Call params can only reference .Params and .Steps of the calling
        # job — there is NO template variable for the caller's own run ID,
        # so pass a static identifier here.
        run_id: "my-job"

# When inlining as a git template
steps:
  - name: notify
    uses:
      job: git://github.com/your-org/ci-templates/slack-notify.yaml@v1
      with:
        status: success
        job_name: my-job
        run_id: "my-job"
```

> **Note:** `with:` values are Go templates expanded against the calling
> job's `.Params` and `.Steps` (for `call:`; `uses:` additionally sees the
> usual step context). For `call:` steps, a reference to a non-existent
> field (such as the `{{ .RunID }}` these examples used to show) fails the
> step with a `call param ... template` error before any child run is
> created.

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

- `apiVersion: unified-cd/v1`, `kind: Job`. Declare `name` / `type` / `required` / `default` / `description`
  under `spec.params.inputs`. Write `description` in English.
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
