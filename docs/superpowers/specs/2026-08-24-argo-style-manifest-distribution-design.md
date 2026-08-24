# Argo-Style Manifest Distribution — Design

Date: 2026-08-24
Status: Approved (design); implementation plan to follow

## 1. Purpose

The Kubernetes manifests ship three generated bundles committed to the
repository — `manifests/core-install.yaml`, `manifests/install.yaml`, and
`manifests/agent-only.yaml` — and the documentation tells operators to fetch
them from `main`:

```
kubectl apply -f https://raw.githubusercontent.com/eirueimi/unified-cd/main/manifests/install.yaml
```

Three problems follow from that arrangement.

- **`core-install.yaml` invites credentials into git.** It carries two Secrets
  whose values are `REPLACE_WITH_*` placeholders, and the documented flow is
  "edit them, then apply". An operator who does that and keeps the manifest
  under version control has committed live credentials.
- **The images are `:latest`.** Releases are cut as `v*` tags and images are
  published per tag, but the bundles pin nothing, so an install gets whatever
  `latest` currently points at.
- **The documented URL serves `main`, not a release.** Anyone following the
  install instructions gets unreleased changes.

Adopt the arrangement Argo Workflows uses: keep only the kustomize sources in
the repository, generate the bundles at release time with the release tag
injected, publish them as release assets, and keep credentials out of the
production bundle entirely.

Fold in one related change to the controller: make the capabilities that
silently switch themselves off visible at startup.

## 2. Scope

In scope:

- Delete the three committed bundles; generate them in the release workflow.
- Inject the release tag into the image references at generation time.
- Remove both Secrets from the `core-install` overlay; document
  `kubectl create secret` as the way to supply them.
- Change the documented distribution URLs to release assets.
- Add a startup summary to the controller naming every degraded capability.
- A migration guide under `docs/operator-manual/migrations/`.

Out of scope:

- **Helm.** Considered and rejected — see section 3.
- **GCS S3-interoperability documentation.** Blocked on a factual question:
  whether log archival actually works against `storage.googleapis.com` with
  HMAC keys. Until someone runs it, there is nothing to document and no bug to
  report.
- **A liveness probe for the k8s-agent.** Investigated separately and
  deliberately not added; see section 3.
- `install.yaml`, the development bundle, keeps its Secrets and their
  development-default values. That is the point of having two bundles.

## 3. Prior art, and two rejected options

**Argo Workflows** was read directly rather than from memory. Its released
`install.yaml` for v4.1.2 pins both images to the release version
(`quay.io/argoproj/argocli:v4.1.2`, `quay.io/argoproj/workflow-controller:v4.1.2`)
and contains **no Secret resources at all** — the artifact-repository
documentation instructs operators to run `kubectl create secret generic ...`
themselves and reference the Secret by name. Its `quick-start-*` bundles do
carry Secrets, with obvious throwaway values (`secret: "shh!"`). The repository
contains no `manifests/install.yaml`; the bundle exists only as a release
asset, built from the kustomize sources at release time.

That split — a production bundle with no credentials, a quick-start bundle with
junk ones — is the arrangement this design adopts, and it already matches how
`core-install` and `install` divide the work here.

**Woodpecker CI** distributes through a Helm chart whose image tag defaults to
the chart's `appVersion`, and whose `resources:` is `{}` with a comment saying
defaults are deliberately left to the user.

**Helm was considered and rejected.** It would solve credential delivery, tag
pinning, and resource defaults in one move, and a values layer is the honest
answer to "how should an operator configure this". It was rejected on
maintenance grounds: the repository has effectively one author, and carrying
both kustomize sources and a chart is how the two drift apart. Adopting Helm
only as a replacement — generating the raw bundles with `helm template` — was
also considered and is a reasonable future direction, but it is a larger
project than this one and would change the install flow a second time for
operators who have just been asked to change it once.

One Helm-specific hazard is recorded here because it will matter if the idea
returns: the encryption key must never be generated inside a chart template.
`randAlphaNum` regenerates on every `helm upgrade`, and `lookup` returns
nothing under `helm template` and `--dry-run`. The existing `kek-job.yaml`
already solves this correctly with `kubectl create` idempotency, and any Helm
migration must port that Job as a `pre-install` hook and nothing else.

**A k8s-agent liveness probe was investigated and rejected.** Every failure
mode in the agent's loops — controller unreachable, credential rejected, claim
error — is logged and retried forever without exiting
(`internal/k8sagent/agent.go:225-236`, `internal/agent/heartbeat.go:39-55`), so
a process-based probe would report a fully wedged agent as healthy. The
controller is already the authority on agent liveness: a silent agent stops
heartbeating, its runs are failed by the stuck-run reaper, and the agent record
is dropped after five minutes. Restarting the pod adds nothing. A real
`/healthz` reporting time-since-last-successful-controller-contact would be
meaningful, but it duplicates the controller's own detection and is not worth
the code today. If a blocking call without a context timeout is ever added to
either loop, that reasoning expires, because the controller's authority depends
on the heartbeat goroutine still running.

## 4. Stop committing generated bundles

`manifests/core-install.yaml`, `manifests/install.yaml`, and
`manifests/agent-only.yaml` are deleted. The repository keeps
`manifests/base/`, `manifests/core-install/`, `manifests/install/`, and
`manifests/agent-only/`.

`make manifests` is kept but writes to `dist/manifests/`, which is gitignored.
Its purpose becomes local inspection, not producing a committed artifact.

This removes a class of defect rather than fixing an instance of it: with no
committed bundle, a bundle cannot drift from its sources, and the rule in
`manifests/README.md` requiring all three to be regenerated and committed
together stops being load-bearing.

## 5. Secret delivery

Both Secrets are removed from the `core-install` overlay. The controller
Deployment continues to reference them by their existing fixed names, so an
operator creates them before applying the bundle:

```bash
kubectl create namespace unified-cd

kubectl create secret generic unified-cd-controller -n unified-cd \
  --from-literal=UNIFIED_DB_DSN='postgres://...' \
  --from-literal=UNIFIED_TOKEN='...' \
  --from-literal=UNIFIED_S3_ENDPOINT='...' \
  --from-literal=UNIFIED_S3_BUCKET='...' \
  --from-literal=UNIFIED_S3_KEY='...' \
  --from-literal=UNIFIED_S3_SECRET='...'

unified-cli keygen --out ./kek
kubectl create secret generic unified-cd-controller-kek -n unified-cd \
  --from-file=kek=./kek
```

The KEK uses `--from-file`, not `--from-literal`: `keygen --out` already writes
a file, and passing the key as a literal would put it in `argv` and the shell
history. The documentation offers `--from-env-file` for the other six for the
same reason.

Names are unchanged (`unified-cd-controller`, `unified-cd-controller-kek`), and
the consumption mechanisms are unchanged: `envFrom` for the first, a volume
mount with `defaultMode: 0400` for the second. The separation is deliberate and
must survive — `envFrom` projects every key of the Secret it names, so moving
the KEK into the first Secret would put the encryption key into the process
environment, which is exactly what the file mount avoids.

### 5.1 The failure mode this introduces

The placeholder flow had an accidental virtue: all six keys were visible at
once, so they were hard to forget. Creating the Secret by hand is not.

A Secret that exists but lacks the four `UNIFIED_S3_*` keys does **not** stop
the controller. `envFrom` is satisfied by the Secret's existence, the pod
starts, and object storage is simply absent. The selection logic at
`cmd/controller/main.go:329-338` falls through S3, then `UNIFIED_DATA_DIR`,
then logs `no object store configured — log archival disabled` at warn level
and continues. There is no fallback to the Garage instance —
`core-install.yaml` contains no Garage at all, and the development bundle
reaches it only because it explicitly sets `UNIFIED_S3_ENDPOINT` to the
in-cluster service.

Section 6 addresses the visibility half of this. The documentation half:
the `kubectl create secret` example is always shown with all six keys, the
consequence of omitting the S3 four is stated directly beneath it, and the log
line to look for is quoted verbatim.

A missing Secret, by contrast, is a loud failure: the pod stays in
`CreateContainerConfigError` until it appears. Both orderings converge —
namespace, Secrets, bundle is the documented order, but applying the bundle
first and creating the Secrets after also works, because the Deployment retries.

### 5.2 Relationship to the KEK warning annotation

A recent change added `unified-cd.io/kek-warning` to the base KEK Secret so the
warning would survive `kubectl kustomize` into `core-install.yaml`. This design
removes that Secret from the bundle, so the annotation goes with it. The
warning's content moves into the documentation, directly above the
`kubectl create secret` command. The annotation is correct for the current
arrangement and should not be reverted in advance; this change supersedes it.

## 6. Controller startup summary

The controller logs each subsystem's state as it initializes — more than twenty
lines between `unified-cd controller starting` and `controller listening`.
Nothing summarizes which capabilities ended up off.

Emit a consolidated summary immediately before `controller listening`. The
handler is a JSON `slog` handler, so this is structured records, not a banner:

- One `slog.Info("startup summary", ...)` always, carrying the resolved state
  of each optional capability — object store (`s3` / `local` / `none`), secret
  encryption key (`file` / `kms` / `ephemeral`), SSO, web UI.
- One `slog.Warn` per degraded capability, naming what is unavailable and the
  setting that restores it.

Capabilities to cover, each confirmed against the code during implementation:

| State | What is lost |
|---|---|
| No object store configured | Log archival and artifacts |
| `logTrimDays > 0` with no object store | The setting is inert (see below) |
| `UNIFIED_DEV_MODE=1` | Every secret becomes unreadable after a restart |
| OIDC not configured | SSO, and the CLI device flow |
| `--web-dir` empty | `/ui/*` returns 404 |

The log-trim row is a second instance of the same problem, found while writing
this design. `RunLogTrim` returns immediately when the object store is nil
(`internal/controller/log_trim.go:29-31`), so there is no data-loss risk — but
startup still logs `log trim enabled trimDays=N`, advertising a sweeper that
will never run.

**Startup does not fail.** Running without an object store is a legitimate
configuration for a deployment that does not use artifacts, and refusing to
start would break existing installations. The goal is that an operator can see
what is off, not that they are prevented from choosing it.

Tests install a recording `slog` handler and assert that each branch emits the
expected records.

## 7. Release workflow and distribution

A job is added to `.github/workflows/release-docker.yml` that runs on `v*`
tags and `needs` the image-publishing job. Ordering matters: a bundle attached
before its images exist advertises image references that cannot be pulled.

The job, operating on the CI checkout only and never committing:

1. `kustomize edit set image` for both first-party images, to the release tag.
2. `kubectl kustomize` for each of the three overlays.
3. Attach the three files to the GitHub Release.

Note that `kubectl` embeds kustomize for `kubectl kustomize` but does **not**
expose `kustomize edit`, so the job installs the standalone `kustomize` CLI for
step 1. The alternative — adding an `images:` block with a placeholder tag to
each overlay's `kustomization.yaml` and rewriting it in the workflow — avoids
that dependency but puts a value in the repository that is meaningless outside
CI. Prefer the standalone CLI; fall back to the `images:` block only if
installing it in the release job proves awkward.

Third-party images (`postgres:16-alpine`, `dxflrs/garage:v2.3.0`) are already
pinned in the base and are not touched.

Documented URLs become:

```
# Pinned — the primary form in the documentation
https://github.com/eirueimi/unified-cd/releases/download/v0.5.0/install.yaml

# Always the newest release — offered as a convenience
https://github.com/eirueimi/unified-cd/releases/latest/download/install.yaml
```

Local development uses `kubectl apply -k manifests/install`, which replaces
applying a committed file and loses nothing.

## 8. Migration

For an operator already running from `core-install.yaml` with filled-in
Secrets, **no action is required**. `kubectl apply -f` does not prune, so the
Secrets already in the cluster survive a bundle that no longer contains them,
and the names are unchanged, so the Deployment's references still resolve. What
changes for them is where they fetch the bundle, and knowing that it no longer
carries the Secrets.

The break is elsewhere: **automation pointing at the `raw.githubusercontent.com`
URLs stops working** when the files are deleted. This is accepted rather than
mitigated. A transition release that keeps deprecated bundles committed was
considered and rejected — it extends by one release exactly the drift this
change removes. A 404 is an immediate, unambiguous failure rather than a silent
wrong result, and the migration guide gives the replacement URL.

Per `AGENTS.md`'s breaking-change rule, a guide is added under
`docs/operator-manual/migrations/`, following the pattern of the existing
ID-scoped credential guide: a before/after table, the exact commands, and the
exact error strings an operator will see.

## 9. Verification

The change is complete when:

1. A `v*` tag produces a release carrying all three bundles, and each pins both
   first-party images to that tag.
2. The three bundles no longer exist in the repository, and `make manifests`
   writes to a gitignored directory.
3. `core-install.yaml` as released contains no Secret resource;
   `install.yaml` still contains its development-default Secrets.
4. Applying the released `core-install.yaml` to a cluster with the two Secrets
   pre-created brings the controller up; applying it without them leaves the
   pod in `CreateContainerConfigError` and nothing worse.
5. Starting a controller with no object store configured emits both the summary
   record and a warn record naming log archival and artifacts; starting one
   with `logTrimDays > 0` and no object store additionally warns that the
   setting is inert.
6. Every documentation location that told an operator to edit placeholders now
   describes the `kubectl create secret` flow, and every `raw.githubusercontent`
   manifest URL is replaced.
7. `go build ./...` and `go test ./... -short -count=1` pass.

## 10. Staging

1. **Controller startup summary.** Independent of everything else, and useful
   on its own. Ships first so the visibility exists before the flow that needs
   it.
2. **Secrets out of `core-install`.** The overlay change plus every
   documentation location, plus the migration guide.
3. **Release-time generation.** The workflow job and tag injection, with the
   bundles still committed, so the generated output can be compared against the
   committed one and proven identical apart from the image tags.
4. **Delete the committed bundles.** Only after stage 3 has proven the
   generated output is right. Documentation URLs change here.
