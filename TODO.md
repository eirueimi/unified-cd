# Active Backlog

This file contains active work only. Completed history is available in Git.
Last audited: 2026-07-26 against `1e46459`.

## Critical

## High

## Medium

### 37. Ship a Helm chart for installing unified-cd

- **Status:** Open
- **Impact:** Operators who standardise on Helm must translate the Kustomize manifests into their own chart, and every release can drift from what that hand-made chart deploys.
- **Evidence:** Installation is Kustomize only: `manifests/base`, `manifests/core-install`, `manifests/agent-only` and `manifests/install`, rendered into the `install.yaml` release asset (`docs/getting-started/installation.md`). No chart exists in the repository; `templates/helm-upgrade.yaml` is a job template that runs `helm` from a step, not an installer.
- **Done when:** A chart built from the same resources as the Kustomize bundles installs the controller, the k8s agent, or both, with values for image tags (defaulting to the chart's app version), namespaces, external PostgreSQL and S3, and the job namespace's Pod Security labels; CI renders it and checks that the default values produce the same objects as `install.yaml`, and installs it into kind; the release workflow publishes it to an OCI registry; and the installation guide documents it next to `install.yaml`.

## Low

No active items.
