# Helm Chart Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `charts/unified-cd`, a Helm chart that installs the controller, the k8s agent, or both (with or without bundled PostgreSQL/Garage), make it the single source of the three release bundles, and delete the Kustomize overlays.

**Architecture:** The chart's templates are ports of `manifests/base/*` and the `install` overlay, with hard-coded `unified-cd`/`ci` replaced by `.Release.Namespace` / `jobNamespace.name`. A small Go package `internal/helmchart` shells out to `helm template` and returns parsed objects; chart tests and the existing manifest-reading Go tests use it. `scripts/build-manifests.sh` renders the bundles with `helm template`; CI lints, renders and kind-installs the chart; the release workflow pushes it to `oci://ghcr.io/eirueimi/charts`.

**Tech Stack:** Helm 3 (pinned `v3.16.4`), Go tests (`gopkg.in/yaml.v3`, testify), GitHub Actions (`azure/setup-helm@v4`, `helm/kind-action@v1`).

**Spec:** `docs/superpowers/specs/2026-10-30-helm-chart-design.md` (read §2, §5 and §7 before any task — §7 holds binding decisions).

## Global Constraints

- English only in code, docs, commits; no personal names or emails (AGENTS.md).
- Resource names are fixed, not release-prefixed: `unified-cd-controller`, `unified-cd-controller-config`, `unified-cd-controller-kek`, `unified-cd-k8s-agent`, `unified-cd-k8s-agent-config`, `unified-cd-kek-init`, `postgres`, `postgres-data`, `garage`, `garage-data`, `garage-config`, ClusterRole/Binding `unified-cd-controller-kubernetes-enrollment`, enrollment policy `unified-cd-k8s-agents`.
- Every namespaced object uses `{{ .Release.Namespace }}`, except the agent's job-namespace Role/RoleBinding which use `{{ .Values.jobNamespace.name }}`. No template contains the literal `unified-cd` as a namespace or `ci`.
- The chart never renders a Namespace for the release namespace and never uses `lookup`.
- First-party image tags default to `.Chart.AppVersion`; checked-in `Chart.yaml` is `version: 0.0.0-dev`, `appVersion: latest`.
- Third-party images stay pinned exactly: `postgres:16-alpine`, `dxflrs/garage:v2.3.0`, `alpine/k8s:1.31.2`; the agent's `podImage` default stays `golang:1.24-alpine`.
- Trial credentials are exactly today's: DSN `postgres://unified:unified@postgres.<ns>.svc.cluster.local:5432/unified?sslmode=disable`, `UNIFIED_TOKEN=dev-token-change-me`, S3 endpoint `garage.<ns>.svc.cluster.local:3900`, bucket `unified-cd-logs`, key `garageadmin`, secret `garageadmin12345`, `GARAGE_RPC_SECRET` 64 zeros.
- The KEK Secret is only ever created by the Job's `kubectl create` (create-only); never a Helm-rendered Secret.
- Helm version everywhere: `v3.16.4`.

## Values contract (all tasks use these keys)

```yaml
controller:
  enabled: true
  image: {repository: ghcr.io/eirueimi/unified-cd-controller, tag: "", pullPolicy: IfNotPresent}
  replicas: 1
  existingSecret: ""          # "" + bundledStores.enabled → chart renders the trial Secret; otherwise this name (default "unified-cd-controller") is referenced, not created
  kek:
    secretName: unified-cd-controller-kek
    generate: null            # null → follows bundledStores.enabled; true/false overrides
    job: {helmHook: true, image: alpine/k8s:1.31.2}
  config: {}                  # deep-merged over the chart-computed controller config (agentAuth defaults)
  service: {type: ClusterIP, port: 80}
  resources: {requests: {cpu: 100m, memory: 256Mi}, limits: {memory: 512Mi}}
  nodeSelector: {}
  tolerations: []
  affinity: {}
  podAnnotations: {}
agent:
  enabled: true
  image: {repository: ghcr.io/eirueimi/unified-cd-k8s-agent, tag: "", pullPolicy: IfNotPresent}
  replicas: 2
  server: ""                  # "" → in-cluster http://unified-cd-controller.<ns>.svc.cluster.local + allowInsecureHTTP: true (requires controller.enabled); otherwise must start with https://
  config: {}                  # deep-merged over the chart-computed agent config
  resources: {requests: {cpu: 50m, memory: 128Mi}, limits: {memory: 256Mi}}
  nodeSelector: {}
  tolerations: []
  affinity: {}
  podAnnotations: {}
jobNamespace:
  create: true
  name: ci
  podSecurity: {enforce: baseline, audit: restricted}
bundledStores:
  enabled: true
  postgres: {image: postgres:16-alpine, storage: 5Gi, storageClassName: ""}
  garage: {image: dxflrs/garage:v2.3.0, storage: 10Gi, storageClassName: ""}
ingress:
  enabled: false
  className: ""
  annotations: {}
  hosts: []                   # [{host: cd.example.com, paths: [{path: /, pathType: Prefix}]}]
  tls: []                     # [{secretName: ..., hosts: [...]}]
```

Computed defaults (helpers in `templates/_helpers.tpl`):
- Controller config default = today's `manifests/base/controller/config-configmap.yaml` `agentAuth` with `namespaces: [<release ns>]`, plus — only when `bundledStores.enabled` — today's `kubernetesStoreCredentialPolicies` with `namespaces: [<jobNamespace.name>]`, `serviceAccounts: [default]`.
- Agent config default = today's base agent config with `namespace: <jobNamespace.name>`, the server per `agent.server`, and — only when `bundledStores.enabled` — `sidecarS3SecretMode: broker`.

Render-time failures (`fail`), each with a message naming the value to change:
1. `controller.enabled=false` and `agent.enabled=false` → `nothing to install: enable controller.enabled or agent.enabled`.
2. `bundledStores.enabled=true` and `controller.enabled=false` → `bundledStores.enabled requires controller.enabled`.
3. `agent.server=""` and `controller.enabled=false` → `agent.server is required when controller.enabled is false`.
4. `agent.server` set and not `https://` → `agent.server must be an https:// URL`.
5. Trial Secret rendered and `ingress.enabled=true` → `ingress.enabled with the bundled trial credentials would expose dev-token-change-me: set controller.existingSecret to a Secret you created, or disable ingress`.

## Review Focus

1. A user overrides one key in `controller.config` / `agent.config` (e.g. `agent.config.maxConcurrent: 10`) and expects every other default key to survive — deep merge, not replace (Task 2 and Task 3 tests).
2. `helm install` into a namespace other than `unified-cd` (e.g. `cd-system`) must produce a working install: enrollment policy namespace, SA subjects, in-cluster server URL, DSN and S3 hosts all follow the release namespace (Task 3 and Task 4 tests).
3. `helm upgrade` after a values change must not fail on the KEK Job's immutable spec and must not touch the KEK (Task 4 hook test, Task 7 kind assertion).
4. A user sets `jobNamespace.name: builds` — the agent Role/RoleBinding, the Namespace (with PSA labels), the agent config `namespace:` and the store-credential policy must all move together (Task 3 test).
5. A user enables Ingress for production with `bundledStores.enabled=false` and an existing Secret — must render, with no trial Secret anywhere in the output (Task 5 test).

---

### Task 1: `internal/helmchart` render helper and chart skeleton

**Files:**
- Create: `internal/helmchart/render.go`, `internal/helmchart/render_test.go`
- Create: `charts/unified-cd/Chart.yaml`, `charts/unified-cd/values.yaml` (full values contract above), `charts/unified-cd/templates/_helpers.tpl`, `charts/unified-cd/.helmignore`

**Interfaces:**
- Produces:
  - `func ChartDir() string` — absolute path of `charts/unified-cd` resolved from this source file (`runtime.Caller`).
  - `func Available() bool` — `helm` on PATH.
  - `type Object map[string]any` with `func (o Object) Kind() string`, `Name() string`, `Namespace() string`.
  - `func Render(t testing.TB, namespace string, set ...string) []Object` — runs `helm template unified-cd <ChartDir()> --namespace <ns> --set <each>` (skips the test via `t.Skip` when `!Available()`, fails the test on non-zero exit with stderr), splits documents, drops empty ones.
  - `func RenderErr(namespace string, set ...string) (stdout string, stderr string, err error)` — same invocation without a `testing.TB`, for expected-failure tests.
  - `func Find(objs []Object, kind, name string) Object` — nil when absent.
  - Helpers `unified-cd.namespace`, `unified-cd.controllerImage`, `unified-cd.agentImage`, `unified-cd.labels` (standard `app.kubernetes.io/*` + `helm.sh/chart`) in `_helpers.tpl`, plus the five render-time failures as `unified-cd.validate` included from every top-level template.

- [ ] **Step 1: Write failing tests** in `render_test.go`: `TestRenderEmptyChartSucceeds` (render with defaults returns no error); `TestValidateFailures` table over the five failure rules — each `RenderErr` returns a non-nil error and stderr contains the exact message substring from the contract.
- [ ] **Step 2: Run** `go test ./internal/helmchart/ -count=1` — expect FAIL (package/chart missing).
- [ ] **Step 3: Implement** the helper, `Chart.yaml` (`apiVersion: v2`, `name: unified-cd`, `type: application`, `version: 0.0.0-dev`, `appVersion: latest`, `kubeVersion: ">=1.25.0-0"`), full `values.yaml` with a comment per key, `_helpers.tpl`. Until later tasks add templates, put the `include "unified-cd.validate"` in a `templates/validate.yaml` that renders nothing.
- [ ] **Step 4: Run** `go test ./internal/helmchart/ -count=1` and `helm lint charts/unified-cd` — PASS, lint `0 chart(s) failed`.
- [ ] **Step 5: Commit** `feat(chart): add unified-cd chart skeleton and render helper`.

### Task 2: Controller templates

**Files:**
- Create: `charts/unified-cd/templates/controller/{serviceaccount,rbac,configmap,service,deployment}.yaml`
- Test: `internal/helmchart/controller_test.go`
- Modify: `internal/config/config_test.go:117-135` (`TestDefaultControllerManifestConfiguresKubernetesEnrollment` reads the ConfigMap from `helmchart.Render(t, "unified-cd")` instead of `manifests/base/...`)

**Interfaces:**
- Consumes: Task 1 helpers.
- Produces: ConfigMap `unified-cd-controller-config` key `controller-config.yaml`; Deployment env `UNIFIED_CONTROLLER_KEY_FILE=/etc/unified-cd/kek`, `envFrom` Secret = `controller.existingSecret` or `unified-cd-controller`; KEK volume from `controller.kek.secretName`; helper `unified-cd.controllerSecretName`, `unified-cd.trialSecret` (bool: `bundledStores.enabled && controller.existingSecret == ""`).

- [ ] **Step 1: Write failing tests:** `TestControllerObjectsMatchBase` — default render contains Deployment/Service/ServiceAccount/ClusterRole/ClusterRoleBinding/Role/RoleBinding/ConfigMap with the fixed names; Deployment fields equal today's base (image `ghcr.io/eirueimi/unified-cd-controller:latest`, args `["-f","/etc/unified-cd/controller-config.yaml"]`, port 8080, probes `/healthz` `/readyz`, resources, KEK mount `defaultMode: 0400` = 256); Service `ClusterIP` port 80 → 8080. `TestControllerConfigFollowsNamespace` — render in `cd-system`: policy `namespaces: [cd-system]`, ClusterRoleBinding subject namespace `cd-system`. `TestControllerConfigDeepMerge` — `--set controller.config.server.listen=:9090` (any new map key) keeps the computed `agentAuth` and adds the key; a user-supplied `controller.config.agentAuth.kubernetesEnrollmentPolicies` list replaces the computed list (Helm merges maps, not lists — document this in values.yaml). `TestControllerImageTag` — `--set controller.image.tag=v9.9.9` renders that tag. `TestControllerDisabled` — `controller.enabled=false,bundledStores.enabled=false,agent.server=https://cd.example.com` renders no controller objects.
- [ ] **Step 2: Run** `go test ./internal/helmchart/ ./internal/config/ -count=1` — FAIL.
- [ ] **Step 3: Implement** the templates as ports of `manifests/base/controller/*` (keep their explanatory comments as template comments `{{- /* */}}` where they explain non-obvious choices) and the config deep merge with `mergeOverwrite (deepCopy computedDefault) .Values.controller.config`.
- [ ] **Step 4: Run** the same tests — PASS; `helm lint charts/unified-cd` clean.
- [ ] **Step 5: Commit** `feat(chart): render the controller`.

### Task 3: k8s agent and job namespace templates

**Files:**
- Create: `charts/unified-cd/templates/agent/{serviceaccount,rbac,configmap,deployment}.yaml`, `charts/unified-cd/templates/job-namespace.yaml`
- Test: `internal/helmchart/agent_test.go`
- Modify: `internal/k8sagent/credentials_test.go:270-357` — `TestKubernetesCredentialManifestsUseProjectedEnrollmentIdentity` reads its objects from `helmchart.Render` (default render) instead of `manifests/base/...`, keeping every assertion. `TestKubernetesCredentialRenderedProductionManifestsUseHTTPS` still renders the Kustomize overlays in this task; Task 6 switches it.

**Interfaces:**
- Consumes: Task 1/2 helpers (`unified-cd.trialSecret` decides `sidecarS3SecretMode: broker`).
- Produces: ConfigMap `unified-cd-k8s-agent-config` key `k8s-agent-config.yaml`; Namespace `<jobNamespace.name>` (when `create`) with labels `pod-security.kubernetes.io/enforce|audit` from values and annotation `helm.sh/resource-policy: keep`.

- [ ] **Step 1: Write failing tests:** `TestAgentObjectsMatchBase` (replicas 2, image tag `latest`, projected token `audience: unified-cd-agent-enrollment`, `expirationSeconds: 3600`, mount `/var/run/secrets/unified-cd-agent` read-only, env `UNIFIED_K8S_CONFIG=/etc/unified-cd/config.yaml`, Role in job namespace with today's verbs). `TestAgentDefaultServerIsInCluster` — default render: `server: http://unified-cd-controller.unified-cd.svc.cluster.local`, `allowInsecureHTTP: true`, `sidecarS3SecretMode: broker`; in `cd-system` the host follows. `TestAgentOnlyRequiresHTTPS` — agent-only render with `https://cd.example.com` has no `allowInsecureHTTP` and no controller objects. `TestJobNamespaceMovesTogether` — `jobNamespace.name=builds`: Namespace `builds` with `enforce: baseline`, `audit: restricted`, keep annotation; Role/RoleBinding namespace `builds`; agent config `namespace: builds`; controller store-credential policy `namespaces: [builds]`. `TestJobNamespaceCreateFalse` renders no Namespace. `TestAgentConfigDeepMerge` — `--set agent.config.maxConcurrent=10` keeps `enrollmentPolicy`, `labels`, `podImage`.
- [ ] **Step 2: Run** `go test ./internal/helmchart/ ./internal/k8sagent/ -run 'Agent|JobNamespace|KubernetesCredential' -count=1` — FAIL.
- [ ] **Step 3: Implement** templates as ports of `manifests/base/k8s-agent/*`, `manifests/install/agent-config-patch.yaml` and the `ci` Namespace in `manifests/base/namespace/namespace.yaml` (keep its PSA comment).
- [ ] **Step 4: Run** — PASS; `helm lint` clean.
- [ ] **Step 5: Commit** `feat(chart): render the k8s agent and job namespace`.

### Task 4: Bundled stores, trial Secret and KEK Job

**Files:**
- Create: `charts/unified-cd/templates/stores/{postgres,garage}.yaml` (Deployment, Service, PVC; Garage ConfigMap `garage-config` with today's `garage.toml`), `charts/unified-cd/templates/controller/trial-secret.yaml`, `charts/unified-cd/templates/controller/kek-job.yaml`
- Test: `internal/helmchart/stores_test.go`

**Interfaces:**
- Consumes: `unified-cd.trialSecret`, `unified-cd.controllerSecretName`.
- Produces: helper `unified-cd.kekGenerate` (bool: `controller.kek.generate` if non-nil else `bundledStores.enabled`).

- [ ] **Step 1: Write failing tests:** `TestTrialSecretValues` — default render: Secret `unified-cd-controller` `stringData` equals the Global Constraints values exactly (namespace substituted), carries today's `unified-cd.io/kek-provenance` annotation text. `TestTrialFollowsNamespace` — in `cd-system` DSN host `postgres.cd-system.svc.cluster.local`, S3 `garage.cd-system.svc.cluster.local:3900`. `TestStoresMatchBase` — postgres/garage Deployments, Services, PVCs (5Gi/10Gi), env and probes equal today's base; `storageClassName` only rendered when set. `TestNoTrialSecretWithExistingSecret` — `controller.existingSecret=mine` renders no Secret and the Deployment `envFrom` names `mine`. `TestProductionModeRendersNoSecretsOrStores` — `bundledStores.enabled=false`: no Secret, no postgres/garage objects, no KEK Job (generate follows), no `sidecarS3SecretMode`, no store-credential policy. `TestKEKJobHook` — default: Job `unified-cd-kek-init` has annotations `helm.sh/hook: post-install,post-upgrade` and `helm.sh/hook-delete-policy: before-hook-creation,hook-succeeded`; ServiceAccount/Role/RoleBinding carry the same hook annotations with weight lower than the Job (`helm.sh/hook-weight: "-1"` vs `"0"`); the script references `-n <release ns>` and `controller.kek.secretName`, never `unified-cd` literally; with `controller.kek.job.helmHook=false` none of the four objects has a hook annotation. `TestKEKGenerateOverride` — `bundledStores.enabled=false,controller.kek.generate=true` renders the Job.
- [ ] **Step 2: Run** `go test ./internal/helmchart/ -run 'Trial|Stores|Secret|KEK|Production' -count=1` — FAIL.
- [ ] **Step 3: Implement** as ports of `manifests/base/postgres/*`, `manifests/base/garage/*`, `manifests/install/secret.yaml`, `manifests/install/kek-job.yaml` (keep the create-only script logic and its comments verbatim apart from the namespace/secret-name substitution).
- [ ] **Step 4: Run** — PASS; `helm lint` clean.
- [ ] **Step 5: Commit** `feat(chart): bundle trial stores, trial Secret and the create-only KEK Job`.

### Task 5: Ingress, NOTES, schema and chart README

**Files:**
- Create: `charts/unified-cd/templates/ingress.yaml`, `charts/unified-cd/templates/NOTES.txt`, `charts/unified-cd/values.schema.json`, `charts/unified-cd/README.md`
- Test: `internal/helmchart/ingress_test.go`

**Interfaces:**
- Consumes: `unified-cd.trialSecret`, the validate helper (failure 5).

- [ ] **Step 1: Write failing tests:** `TestIngressDisabledByDefault`. `TestIngressRendersHostsAndTLS` — production values (`bundledStores.enabled=false`, `controller.existingSecret=prod`, `ingress.enabled=true`, `ingress.className=nginx`, one host `cd.example.com`, `tls[0].secretName=cd-tls`) render a `networking.k8s.io/v1` Ingress to Service `unified-cd-controller` port 80, `ingressClassName: nginx`, the TLS block, annotations passed through, and no Secret object at all. `TestIngressWithTrialFails` — default + `ingress.enabled=true` fails with failure-5 message. `TestSchemaRejectsBadTypes` — `--set controller.replicas=two` and `--set jobNamespace.podSecurity.enforce=superuser` (enum `privileged|baseline|restricted`) fail. `TestNotesTrialWarning` — `helm template` does not render NOTES, so run `helm install --dry-run=client unified-cd <ChartDir()> -n x` (no cluster needed; skip when helm absent) and assert the output contains `TRIAL ONLY` and `kubectl -n x port-forward svc/unified-cd-controller 8080:80`; with production values it contains neither `TRIAL ONLY` nor `dev-token-change-me`.
- [ ] **Step 2: Run** `go test ./internal/helmchart/ -run 'Ingress|Schema|Notes' -count=1` — FAIL.
- [ ] **Step 3: Implement.** Schema covers every key in the values contract (types, the PSA enum, `agent.server` pattern `^$|^https://`). NOTES prints, in trial mode, a `TRIAL ONLY — do not expose` banner, the port-forward command, the dev token, and how to switch to production; in production mode, how to reach the controller (Ingress host or port-forward) and the Secrets it expects. README: install commands (trial / production / agent-only), values table, the one-release-per-namespace rule, KEK survival on uninstall and how to delete it deliberately (`kubectl -n <ns> delete secret unified-cd-controller-kek`), job namespace kept on uninstall.
- [ ] **Step 4: Run** — PASS; `helm lint --strict charts/unified-cd` clean.
- [ ] **Step 5: Commit** `feat(chart): add ingress, notes, values schema and chart README`.

### Task 6: Bundles from the chart; delete Kustomize

**Files:**
- Create: `charts/unified-cd/bundles/{install,core-install,agent-only}.values.yaml` (add `bundles/` to `.helmignore`)
- Modify: `scripts/build-manifests.sh` (same CLI; uses `helm template`), `internal/k8sagent/credentials_test.go` (render via bundle values files; remove the Task-3 interim `--set`), `internal/api/types.go:676`, `internal/k8sagent/podbuilder.go:99,201,239` (comments point at chart templates)
- Delete: `manifests/base/`, `manifests/core-install/`, `manifests/install/`, `manifests/agent-only/`
- Modify: `manifests/README.md` (becomes a short pointer to the chart and the release bundles)

**Interfaces:**
- Produces: `scripts/build-manifests.sh <out-dir> [tag]` writes `install.yaml`, `core-install.yaml`, `agent-only.yaml`. Bundle values: install = `controller.kek.job.helmHook: false`; core-install = `bundledStores.enabled: false`, `controller.kek.job.helmHook: false`, `agent.server: https://controller.example.invalid`; agent-only = `controller.enabled: false`, `bundledStores.enabled: false`, `agent.server: https://controller.example.invalid`. With a tag, `--set controller.image.tag=<tag>,agent.image.tag=<tag>`. Each file is prefixed with the `unified-cd` Namespace object (`apiVersion: v1`, `kind: Namespace`, `metadata.name: unified-cd`) then `---`, then `helm template unified-cd charts/unified-cd --namespace unified-cd -f <values>`. Script fails if `helm` is missing.

- [ ] **Step 1: Capture the baseline before deleting anything:** `scripts/build-manifests.sh <scratch>/old` on the unmodified script (needs `kubectl`; if absent install it or use `go run sigs.k8s.io/kustomize/kustomize/v5@v5.4.3 build`). Keep the output outside the repo.
- [ ] **Step 2: Rewrite the script, add the values files, and render** `<scratch>/new`. Write a throwaway normalizer (outside the repo) that loads both, drops `metadata.labels`/`metadata.annotations` keys starting with `app.kubernetes.io/`, `helm.sh/`, `meta.helm.sh/`, sorts by kind/namespace/name, and diffs. Allowed differences only: those labels/annotations, Kustomize's `garage-config-<hash>` name vs `garage-config` (and the Deployment volume referencing it), object order. Put the final normalized diff summary in the task report; any other difference is a bug to fix in the chart.
- [ ] **Step 3: Update Go tests:** `credentials_test.go` renders bundles through `helmchart.Render` with `-f charts/unified-cd/bundles/<b>.values.yaml` (add `func RenderFile(t testing.TB, namespace, valuesFile string, set ...string) []Object` to `internal/helmchart` for this) and keeps every existing assertion (`server: http://` absent in core-install/agent-only, `https://controller.example.invalid` present, `allowInsecureHTTP: true` in install).
- [ ] **Step 4: Delete the Kustomize trees, fix the Go comments, rewrite `manifests/README.md`.** Run `git grep -n "manifests/base\|manifests/install\|manifests/core-install\|manifests/agent-only\|kubectl kustomize"` — only docs (Task 8) and historical `docs/superpowers/` files may remain.
- [ ] **Step 5: Verify:** `go test ./internal/helmchart/ ./internal/k8sagent/ ./internal/config/ -count=1` PASS; `scripts/build-manifests.sh dist/m v0.0.0-test` writes three files and `grep -q "ghcr.io/eirueimi/unified-cd-controller:v0.0.0-test" dist/m/core-install.yaml`; `go vet ./...` clean.
- [ ] **Step 6: Commit** `feat!: render the install bundles from the Helm chart and remove the Kustomize overlays`.

### Task 7: CI

**Files:**
- Modify: `.github/workflows/ci.yml`

- [ ] **Step 1:** Add `azure/setup-helm@v4` with `version: v3.16.4` to the unit-test matrix job (all three OSes) and the Linux integration job, before `go test`, so `internal/helmchart` tests run everywhere instead of skipping.
- [ ] **Step 2:** New job `helm` ("Helm chart (lint + kind install)"), `runs-on: ubuntu-latest`, `timeout-minutes: 30`: setup-go, setup-helm, `helm lint --strict charts/unified-cd`, `helm lint --strict` with each bundle values file, `scripts/build-manifests.sh dist/m` then `kubectl apply --dry-run=server -f` each bundle after the kind cluster is up; kind cluster `helm-ci` via `helm/kind-action@v1`; build `docker/controller.Dockerfile` → `unified-cd-controller:ci` and `docker/k8s-agent.Dockerfile` → `unified-cd-k8s-agent:ci` with `VERSION=ci` build-arg, `kind load docker-image` both; `helm install unified-cd charts/unified-cd -n unified-cd --create-namespace --set controller.image.repository=unified-cd-controller,controller.image.tag=ci,agent.image.repository=unified-cd-k8s-agent,agent.image.tag=ci --wait --timeout 10m`.
- [ ] **Step 3:** Assertions in the same job (bash, `set -euo pipefail`, every wait bounded): poll until `secret/unified-cd-controller-kek` exists (≤3m; do not wait on the Job, whose hook-delete policy removes it on success); controller Deployment `Available` (≤5m); port-forward `svc/unified-cd-controller 18080:80` and poll `GET http://127.0.0.1:18080/api/v1/agents` with `Authorization: Bearer dev-token-change-me` until a returned agent carries label `kind:kubernetes` (≤5m). Record `kubectl -n unified-cd get secret unified-cd-controller-kek -o jsonpath='{.data.kek}'`; `helm upgrade unified-cd charts/unified-cd -n unified-cd --reuse-values --set agent.config.maxConcurrent=3 --wait --timeout 10m` must succeed and the KEK data must be byte-identical; `helm uninstall unified-cd -n unified-cd --wait`; the KEK Secret and namespace `ci` must still exist. On failure dump `kubectl get all -A`, controller and agent logs. Optional final step (`continue-on-error: true`, ≤10m): apply a one-step job and trigger it — skip if it needs images not loadable in the budget.
- [ ] **Step 4: Verify locally** what can be: `actionlint` if available (else YAML parse with `python -c 'import yaml,sys; yaml.safe_load(open(sys.argv[1]))' .github/workflows/ci.yml`). The kind job is proven on the PR's CI run.
- [ ] **Step 5: Commit** `ci: lint, render and kind-install the Helm chart`.

### Task 8: Release workflow and documentation

**Files:**
- Modify: `.github/workflows/release-docker.yml` (bundle job: add setup-helm; new `chart` job)
- Modify: `docs/getting-started/installation.md`, `docs/operator-manual/kubernetes-integration.md`, `docs/reference/configuration.md`, `docs/operator-manual/operations.md`, `docs/troubleshooting/agents-and-enrollment.md`, `docs/troubleshooting/steps-and-execution.md`, `examples/config/k8s-agent.yaml`, `README.md`, `mkdocs.yml`, `TODO.md`
- Create: `docs/operator-manual/migrations/2026-10-30-helm-chart.md`

- [ ] **Step 1: Release.** In the `manifests` job add `azure/setup-helm@v4` (`v3.16.4`) before the build step (the script now needs helm). New job `chart` (`needs: [verify]`, `permissions: {contents: read, packages: write}`): checkout, setup-helm, `helm registry login ghcr.io -u ${{ github.actor }} --password-stdin <<< "${{ secrets.GITHUB_TOKEN }}"`, `helm package charts/unified-cd --version "${TAG#v}" --app-version "$TAG" -d dist/chart`, `helm push dist/chart/unified-cd-${TAG#v}.tgz oci://ghcr.io/eirueimi/charts`, then `helm show chart oci://ghcr.io/eirueimi/charts/unified-cd --version "${TAG#v}"` must print `appVersion: $TAG`. Comment that a first-ever push creates a private GHCR package which an owner must make public once (Package settings → Change visibility), and that the TODO-13-style anonymous pull check should be repeated after the first release.
- [ ] **Step 2: Docs.** installation.md: Kubernetes section leads with Helm — trial `helm install unified-cd oci://ghcr.io/eirueimi/charts/unified-cd --version <x.y.z> -n unified-cd --create-namespace` + port-forward + dev token; production (external stores, `controller.existingSecret`, KEK Secret, `ingress.*`, cert-manager via annotations); agent-only (`controller.enabled=false`, `bundledStores.enabled=false`, `agent.server`); `install.yaml` path kept beside it; installing from a checkout uses `charts/unified-cd`. Replace every overlay path / `kubectl apply -k` in the listed docs (`git grep` from Task 6 Step 4). configuration.md: link the chart README values table. Migration guide per spec §5.5 and §7.7 (overlay → values mapping table; fresh install recommended for trials; adoption: `kubectl -n unified-cd delete job unified-cd-kek-init`, then label/annotate every object with `app.kubernetes.io/managed-by=Helm`, `meta.helm.sh/release-name=unified-cd`, `meta.helm.sh/release-namespace=unified-cd`, then `helm install`; note the KEK Secret needs no adoption). Add it to `mkdocs.yml` nav next to the other migration guides. README: Helm install line. Remove `### 37.` from `TODO.md` and put `No active items.` under every now-empty severity heading.
- [ ] **Step 3: Verify:** `"<scratchpad>/mkdocs-venv/Scripts/python" -m mkdocs build --strict -d <tmp>` (path from the controller) passes; YAML-parse `release-docker.yml`; `git grep -n "kubectl apply -k\|manifests/base"` only in historical `docs/superpowers/` and migration guides dated before 2026-10-30.
- [ ] **Step 4: Commit** `docs: document installing with Helm; release the chart to GHCR`.
