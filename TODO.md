# Active Backlog

This file contains active work only. Completed history is available in Git.
Last audited: 2026-07-26 against `1e46459`.

## Critical

No active items.

## High

### 36. Make the GHCR chart package public after the first chart release and verify anonymous pull

- **Status:** Needs action after the first chart release
- **Impact:** Until `ghcr.io/eirueimi/charts/unified-cd` is public, every documented `helm install oci://ghcr.io/eirueimi/charts/unified-cd` fails for users without credentials, because a package first pushed by a workflow is private by default.
- **Evidence:** `.github/workflows/release-docker.yml` pushes the chart with `helm push` using `GITHUB_TOKEN`; `docs/getting-started/installation.md`, `charts/unified-cd/README.md` and `README.md` document the anonymous `oci://` install.
- **Done when:** The package visibility is set to public in the GitHub package settings, and a credential-isolated `helm pull oci://ghcr.io/eirueimi/charts/unified-cd --version <released version>` (no prior `helm registry login`) exits zero within 120 seconds.

## Medium

No active items.

## Low

No active items.
