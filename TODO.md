# Active Backlog

This file contains active work only. Completed history is available in Git.
Last audited: 2026-07-26 against `1e46459`.

## Critical

### 13. Verify that the default Kubernetes sidecar image is anonymously pullable

- **Status:** Needs verification
- **Impact:** If the version-derived default tag is unavailable without registry credentials, installations that omit `sidecarImage` cannot start the sidecar and Kubernetes jobs remain blocked.
- **Evidence:** `internal/k8sagent/config.go` `defaultSidecarImage()` derives the default from the agent's stamped version (`:vX.Y.Z` for a release, `:latest` otherwise) and it is consumed by the rendered `install.yaml` bundle (built from `manifests/install` and published as a release asset); `internal/k8sagent/config_test.go` `TestDefaultImagesCarryNoDigest` and `TestDefaultImagesResolveThroughVersionedImage` check only its shape, never that the tag is anonymously pullable.
- **Done when:** A credential-isolated Docker pull of the image derived from `defaultSidecarImage` exits zero within 120 seconds, and a temporary Kubernetes namespace with a ServiceAccount that has no image pull secrets starts that exact image with pull policy `Always` and reaches `Ready` within 120 seconds with no pull waiting reason and an image ID matching the digest the registry reports for that tag; every external Docker and Kubernetes setup, pull, wait, inspection, and namespace-deletion subprocess is bounded, safe-target checks guard cleanup, any cleanup failure fails the experiment and is combined with rather than hidden by any primary failure, and all three documents state the verified version-derived default.

## High

### 12. Define and align full database-loss agent recovery behavior

- **Status:** Partial
- **Impact:** After complete controller database loss, existing agents cannot authenticate to claim work until they are re-enrolled, despite operations guidance also promising automatic inventory recovery.
- **Evidence:** `internal/controller/api_agent.go` `handleAgentClaim` calls `internal/store/postgres.go` `UpsertAgentOnClaim`, but `internal/controller/agent_auth.go` authenticates first through `GetAgentCredentialForAuth`; `internal/controller/api_agent_test.go` `TestAgentAPI_Claim_UpsertsUnregisteredAgent` covers only a missing inventory row, and `docs/operator-manual/operations.md` states conflicting full-loss recovery expectations.
- **Done when:** The supported full-database-loss workflow has one explicit runtime and documentation contract, and an integration test starting from absent inventory and credential records proves either bounded automatic recovery or the documented re-enrollment requirement before the agent reappears and can claim work.

### 33e. Verify that the published quickstart controller image passes its health check

- **Status:** Needs verification
- **Impact:** If the published default controller image lacks the `wget` used by the quickstart health check, the controller remains unhealthy and dependent quickstart services do not start.
- **Evidence:** `deployments/docker/docker-compose.yaml` defines the controller image and `wget` health check, while `docker/controller.Dockerfile` uses an Alpine runtime that supplies BusyBox `wget`; source inspection cannot establish the contents of the published default image.
- **Done when:** A credential-isolated pull of the image derived from the Compose controller image expression completes within 120 seconds, then a uniquely named Compose project starts PostgreSQL, Garage, and the controller without building and reports the controller `healthy` within 120 seconds; every Docker subprocess is bounded, prior version environment state is restored, project containers, volumes, and orphans plus the isolated Docker configuration are always cleaned up, and any failure is corrected and the experiment repeated.

## Medium

### 37. Ship a Helm chart for installing unified-cd

- **Status:** Open
- **Impact:** Operators who standardise on Helm must translate the Kustomize manifests into their own chart, and every release can drift from what that hand-made chart deploys.
- **Evidence:** Installation is Kustomize only: `manifests/base`, `manifests/core-install`, `manifests/agent-only` and `manifests/install`, rendered into the `install.yaml` release asset (`docs/getting-started/installation.md`). No chart exists in the repository; `templates/helm-upgrade.yaml` is a job template that runs `helm` from a step, not an installer.
- **Done when:** A chart built from the same resources as the Kustomize bundles installs the controller, the k8s agent, or both, with values for image tags (defaulting to the chart's app version), namespaces, external PostgreSQL and S3, and the job namespace's Pod Security labels; CI renders it and checks that the default values produce the same objects as `install.yaml`, and installs it into kind; the release workflow publishes it to an OCI registry; and the installation guide documents it next to `install.yaml`.

## Low

No active items.
