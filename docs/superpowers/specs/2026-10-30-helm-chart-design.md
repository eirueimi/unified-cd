# Helm Chart — Design

Date: 2026-10-30
Status: Draft (design); implementation plan to follow
Backlog: TODO 37

## 1. Intent

A general user should be able to install unified-cd onto any conformant
Kubernetes cluster (managed EKS/GKE/AKS, on-prem, kind/k3s) with `helm install`,
and manage upgrades with Helm. Today the only Kubernetes path is the Kustomize
overlays under `manifests/`, rendered into the `install.yaml`,
`core-install.yaml` and `agent-only.yaml` release assets; Helm users must
hand-translate them and drift on every release.

## 2. Decisions made with the user

1. **Audience:** general users, any Kubernetes. No cloud- or ingress-controller-
   specific defaults.
2. **Zero-config install is a working trial.** `helm install unified-cd <chart>`
   with no values brings up the controller, the k8s agent, and in-cluster
   PostgreSQL and Garage (S3), exactly like today's `install.yaml`: usable at
   once, explicitly not for production.
3. **Exposure follows the Argo CD / Argo Workflows convention.** The controller
   Service is `ClusterIP` and the trial is reached with `kubectl port-forward`.
   An Ingress is opt-in through values (`enabled`, `className`, `hosts`,
   `annotations`, `tls[].secretName`). The chart never installs cert-manager or
   creates certificates itself; cert-manager is used through annotations.
4. **The chart becomes the single source.** The three release bundles are
   rendered from the chart with `helm template`. The Kustomize sources in
   `manifests/` are removed; Kustomize is no longer hand-maintained.

## 3. Goals

- One chart, `charts/unified-cd`, that installs the controller, the k8s agent,
  or both (component toggles), with or without bundled PostgreSQL/Garage.
- Values for: image repository/tag per component (tag defaulting to the chart's
  `appVersion`), external PostgreSQL DSN and S3 settings via existing Secrets,
  the job namespace and its Pod Security labels, the agent's controller URL and
  enrollment policy, resources, nodeSelector/tolerations/affinity, Ingress.
- The three release bundles keep their names, contents and roles (see §5.4), so
  `kubectl apply -f install.yaml` users are unaffected.
- CI lints and renders the chart, checks the rendered bundles, and installs the
  chart into kind and proves the controller becomes ready and the agent
  enrolls.
- The release workflow publishes the chart to an OCI registry
  (`oci://ghcr.io/eirueimi/charts/unified-cd`) with chart `version` and
  `appVersion` equal to the release tag (`version` without the leading `v`).
- `docs/getting-started/installation.md` documents `helm install` next to
  `install.yaml`.

## 4. Non-goals / anti-goals

- No Bitnami (or other) subchart dependencies for PostgreSQL/Garage: the bundled
  stores stay the same simple single-replica trial Deployments the Kustomize
  base has, rendered by this chart. Production uses external stores.
- No HA/production-grade bundled PostgreSQL, no backup tooling.
- No cert-manager installation, no certificate generation, no cloud-specific
  load-balancer annotations as defaults.
- Not a failure mode to accept: **a production-looking install that silently
  uses the trial credentials.** See §5.3.
- Not a failure mode to accept: **`helm upgrade` or a re-render rotating the
  KEK.** Rotating it makes every stored secret permanently unreadable.

## 5. Design

### 5.1 Layout

`charts/unified-cd/` with `Chart.yaml` (`apiVersion: v2`, `type: application`),
`values.yaml`, `values.schema.json` (validates types and the required/forbidden
combinations below), `templates/`, `templates/NOTES.txt`, and a `README.md`
generated or hand-written from the values table. Resource names keep today's
fixed names (`unified-cd-controller`, `unified-cd-k8s-agent`, `postgres`,
`garage`, `unified-cd-controller-kek`, …) rather than release-prefixed names,
because the controller config, the enrollment policy and the agent config refer
to each other by these names; consequently one release per namespace (documented,
and enforced only by name collision).

> **Correction (2026-10-30, final review).** "One release per namespace" was
> wrong. The `unified-cd-controller-kubernetes-enrollment` ClusterRole and
> ClusterRoleBinding have fixed cluster-scoped names, and the default job
> namespace `ci` is cluster-wide, so the real constraint is one controller
> release per cluster. A second release (for example agent-only) must set a
> different `jobNamespace.name`; a second controller is blocked by the
> cluster-scoped RBAC names.

### 5.2 Components and values (shape; exact keys are the builder's call)

- `controller.enabled` (default true), `agent.enabled` (default true),
  `bundledStores.enabled` (default true → PostgreSQL + Garage + the bucket/key
  setup the Kustomize `install` overlay does today).
- `controller.config` — the controller config file content (today's
  ConfigMap), with the trial's `agentAuth.kubernetesClusters`,
  `kubernetesEnrollmentPolicies` and `kubernetesStoreCredentialPolicies`
  defaults derived from the release namespace, the agent ServiceAccount name
  and the job namespace instead of the hard-coded `unified-cd` / `ci`.
- `agent.config` — the k8s agent config (server URL, enrollment policy, labels,
  job namespace, maxConcurrent, sidecar settings). When the controller is in the
  same release, the agent's `server` defaults to the in-cluster controller
  Service (as today's `install` overlay does); in agent-only mode `agent.server`
  is required and must be `https://`.
- `jobNamespace.create` (default true), `jobNamespace.name` (default `ci`),
  `jobNamespace.podSecurity` (default enforce `baseline`, audit `restricted` —
  today's labels).
- Images: `controller.image`, `agent.image`, plus the sidecar/shim/runner image
  overrides the agent config already supports; first-party tags default to
  `.Chart.AppVersion`; third-party images (postgres, garage, the KEK-init image)
  stay pinned as in today's base.
- `ingress.*` per §2.3, for the controller Service only.

### 5.3 Secrets and the two modes

- **Trial mode** (`bundledStores.enabled: true`): the chart creates the
  `unified-cd-controller` Secret with the same development defaults
  `install.yaml` ships today (DSN to the bundled PostgreSQL, `UNIFIED_TOKEN`,
  Garage key/secret) and the agent reaches the in-cluster controller over HTTP
  (`allowInsecureHTTP: true`) the way the trial bundle does. NOTES.txt prints a prominent "trial only, do not
  expose" warning and the port-forward command.
- **Guard:** rendering fails (template `fail`) when trial-mode defaults would be
  exposed: `ingress.enabled: true` while the trial `UNIFIED_TOKEN` is in use, unless
  the user supplies their own controller Secret. The message tells the user what
  to set.
- **Production mode** (`bundledStores.enabled: false`): the chart creates no
  credential Secret. The user names existing Secrets
  (`controller.existingSecret`, default `unified-cd-controller`, holding the
  same keys core-install documents today: `UNIFIED_DB_DSN`, `UNIFIED_TOKEN`,
  `UNIFIED_S3_*`) and the KEK Secret (`controller.kek.existingSecret`). Missing
  Secrets surface as Pod start failures exactly as core-install does today; the
  chart does not `lookup`.
- **KEK:** in trial mode the KEK is generated in-cluster once by the same
  create-only Job pattern as `manifests/install/kek-job.yaml` (never `apply`,
  never a Helm-managed Secret with a random value — `helm template` and upgrades
  would rotate it). The Job's RBAC stays namespace-scoped. Whether the Job is a
  plain resource or a Helm hook is the builder's call, as long as: a re-install,
  `helm upgrade`, and `kubectl apply -f install.yaml` re-runs never replace an
  existing KEK, and `helm uninstall` does not delete the KEK Secret (document
  that it survives and how to remove it deliberately).

### 5.4 Release bundles from the chart

`scripts/build-manifests.sh <out-dir> [tag]` keeps its interface and becomes
`helm template` with fixed release name/namespace (`unified-cd`) and
per-bundle value files kept in the chart directory or `scripts/`:

| Bundle | Values |
|---|---|
| `install.yaml` | defaults (trial) |
| `core-install.yaml` | `bundledStores.enabled=false`, no Secrets, the example-invalid HTTPS controller URL placeholder as today |
| `agent-only.yaml` | `controller.enabled=false`, `bundledStores.enabled=false`, the example-invalid `server` URL as today |

Because `helm template` emits no Namespace for the release namespace, the
script (or a template gated by a value) must still include the
`unified-cd` Namespace object in the bundles so `kubectl apply -f` works on an
empty cluster as it does today; the Helm install path uses
`--create-namespace` instead. The release workflow's tag-injection check
(`release-docker.yml` "Assert the built bundle carries the release tag") keeps
working against the new output. Behavioural equivalence of the new bundles with
the v0.11.0 Kustomize output is checked once during implementation (a reviewed
`diff` of normalized objects, differences limited to labels/annotations Helm
adds and deliberate changes listed in the PR); after that the Kustomize output
no longer exists to compare against.

### 5.5 Removing Kustomize

Delete `manifests/base`, `manifests/core-install`, `manifests/install`,
`manifests/agent-only`. Keep `manifests/README.md` updated to point at the
chart (or move its content into the docs and leave a pointer). Every consumer of
the overlays moves to the chart: `scripts/build-manifests.sh`,
`internal/k8sagent/credentials_test.go` (renders `manifests/<overlay>` with
`kubectl kustomize` — switch to `helm template`, skipping when `helm` is absent
exactly as it skips today when `kubectl` is absent), comments that cite
`manifests/base/...` paths in Go code (`internal/api/types.go`,
`internal/k8sagent/podbuilder.go`, …), and docs. Repo-local `kubectl apply -k
manifests/install` instructions are replaced by `helm install` from the chart
directory or the release `install.yaml`. This is a breaking change for anyone
applying the overlays from a checkout: write
`docs/operator-manual/migrations/2026-10-30-helm-chart.md` (Kustomize overlay →
chart values mapping; existing `kubectl apply` installs can switch to Helm by
adopting the fixed-name resources — document the `meta.helm.sh/release-name`
/ `release-namespace` annotations and `app.kubernetes.io/managed-by` label
needed for adoption, or recommend a fresh install for trials).

### 5.6 CI

- `helm lint` and `helm template` for the default values and each bundle's
  values, plus a schema-violation case and the §5.3 guard case (render must
  fail), in the existing CI workflow.
- The rendered bundles pass `kubectl apply --dry-run=server` or kubeconform
  (builder's call) against the kind cluster's API.
- A kind job installs the chart with default values using locally built
  controller and agent images loaded into kind (the CI already builds and loads
  images this way), waits for the controller Deployment to be Available and the
  agent to report enrolled (`unified-cli agent list` through a port-forward, or
  the controller API), then runs one trivial job end to end if that fits in the
  job's time budget; `helm upgrade` with a changed value and asserts the KEK
  Secret's data is unchanged; `helm uninstall` and asserts the KEK Secret
  remains.

### 5.7 Release

The release workflow packages the chart with `version`/`appVersion` set from
the tag and `helm push`es it to `oci://ghcr.io/eirueimi/charts`, after the image
verify step (same ordering rationale as the bundle job). The chart package
must be anonymously pullable like the images (verify once after the first
release, as TODO 13 did for the sidecar).

## 6. Documentation

- `docs/getting-started/installation.md`: Helm section first for Kubernetes
  (trial: `helm install unified-cd oci://ghcr.io/eirueimi/charts/unified-cd
  --version <v> -n unified-cd --create-namespace`, then port-forward;
  production: external stores + existing Secrets + Ingress values), the
  `install.yaml` path kept next to it.
- `docs/operator-manual/kubernetes-integration.md`, `docs/reference/configuration.md`,
  `docs/operator-manual/operations.md`, `README.md`, `manifests/README.md`: replace
  overlay paths, add the chart values reference (or link the chart README).
- Migration guide per §5.5; TODO 37 removed.

## 7. Settled details (designer's calls, not discussed with the user)

1. **Guard rule (§5.3).** Decided on values alone: the chart renders its own
   trial `unified-cd-controller` Secret iff `bundledStores.enabled: true` and
   `controller.existingSecret` is unset. Render fails when that trial Secret
   would be rendered and `ingress.enabled: true`. The trial Secret carries
   every trial credential (DSN password, `UNIFIED_TOKEN`, Garage key/secret), so
   one rule covers them all. A hand-made Secret holding dev values cannot be
   detected and is not attempted; the docs say so.
2. **KEK Job.** A Helm hook (`post-install,post-upgrade`,
   `hook-delete-policy: before-hook-creation,hook-succeeded`) for Helm
   installs, which sidesteps Job-spec immutability on upgrade; the bundles are
   rendered with a value (e.g. `kek.job.helmHook=false`) that emits the same
   Job without hook annotations, as a plain resource, as today. The KEK Secret
   is created by the Job's `kubectl create`, so it is never Helm-owned and
   survives `helm uninstall`. The Job image stays pinned (`alpine/k8s:1.31.2`)
   but is overridable (`kek.job.image`) for air-gapped clusters. Its script
   uses the release namespace, not a hard-coded `unified-cd`.
3. **Namespaces.** The chart never creates the release namespace (Helm users
   pass `--create-namespace`). `scripts/build-manifests.sh` prepends the
   `unified-cd` Namespace object to each bundle, first in the file, as today.
   The job namespace is chart-created when `jobNamespace.create: true` and is
   annotated `helm.sh/resource-policy: keep`, so `helm uninstall` never
   deletes it or running job Pods. In the bundles it is emitted the same way.
4. **Versions and tags.** The checked-in `Chart.yaml` has `version: 0.0.0-dev`
   and `appVersion: latest`, so a source-checkout render names `:latest`
   images exactly like today's overlays; `build-manifests.sh <dir> <tag>` and
   the release job set both from the tag. The CI kind install passes the
   locally built and kind-loaded image tags with `--set`.
5. **Helm in CI/release.** Pin Helm with `azure/setup-helm` (a current 3.x
   release) in both workflows. Plain `helm push`; no signing/provenance in this
   iteration. `credentials_test.go` skips when `helm` is absent, as it does
   for `kubectl` today.
6. **Equivalence baseline (§5.4).** Render the current Kustomize tree before
   deleting it and diff normalized objects against the chart's bundles.
   Acceptable differences: Helm/chart labels and annotations, the hook-less
   KEK Job shape, and object ordering inside the file (the Namespace stays
   first because the script prepends it; `kubectl apply -f` handles the rest).
   Everything else must match or be listed in the PR.
7. **Adoption (§5.5).** The migration guide recommends a fresh install for
   trial installs, and documents the adoption labels/annotations for existing
   `kubectl apply` installs with a note to delete the completed KEK Job first
   (its spec is immutable); adoption is documented, not CI-tested.
8. **CI end-to-end job run (§5.6)** is a separate step that may be skipped if
   it does not fit the kind job's time budget; the readiness, enrollment, KEK
   upgrade and uninstall assertions are required.

## 8. Left to the builder

Exact value key names and nesting, helper template structure, whether the KEK
Job is a hook, kubeconform vs server dry-run, how NOTES.txt is worded, whether
the chart README is generated (helm-docs) or hand-written, and how the bundle
value files are organized — provided the behaviour above holds.
