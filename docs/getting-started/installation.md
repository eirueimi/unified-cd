# Installation

## Docker (recommended for production)

```bash
# Controller
docker pull ghcr.io/eirueimi/unified-cd-controller:latest

# Kubernetes agent
docker pull ghcr.io/eirueimi/unified-cd-k8s-agent:latest
```

Images are published to [GitHub Container Registry](https://github.com/eirueimi/unified-cd/pkgs/container/unified-cd-controller) on every `v*` tag for `linux/amd64` and `linux/arm64`.

A ready-to-run stack using these published images (controller + PostgreSQL + Garage + a Docker agent) lives at [`deployments/docker/docker-compose.yaml`](https://github.com/eirueimi/unified-cd/blob/main/deployments/docker/docker-compose.yaml). Unlike the repo-root `docker-compose.yaml` (source build with hot reload, for development), this one pulls the release images:

```bash
cp .env.example .env    # set UNIFIED_TOKEN
# Generate a persistent secret-encryption key (or skip this and keep the
# default UNIFIED_DEV_MODE=1 for a throwaway key — secrets won't survive a restart):
unified-cli keygen --out ./kek
# In .env set UNIFIED_CONTROLLER_KEY_FILE=/run/secrets/kek, then add this volume
# mount under the controller service in deployments/docker/docker-compose.yaml:
#   volumes:
#     - ./kek:/run/secrets/kek:ro
docker compose --env-file .env -f deployments/docker/docker-compose.yaml up -d
```

Pin a release by setting `UNIFIED_CD_VERSION` (e.g. `v0.0.3`) in `.env`.

The repository-root Compose files are development-only and do not define a
production security boundary. Production controllers require HTTPS. Per-agent
credential enrollment is implemented; mTLS agent certificates are future work,
not a current deployment feature.

## Kubernetes

The install bundles below are published as GitHub Release assets starting
with **v0.6.0**; earlier releases carry only the Go binary archives, so an
older release's asset list will not include `install.yaml` or
`agent-only.yaml`.

```bash
# Full install (controller + k8s-agent + PostgreSQL)
# Pinned to a release
kubectl apply -f https://github.com/eirueimi/unified-cd/releases/download/v0.6.0/install.yaml
# Or always the newest release
kubectl apply -f https://github.com/eirueimi/unified-cd/releases/latest/download/install.yaml

# k8s-agent only (connect to existing controller)
# Pinned to a release
kubectl apply -f https://github.com/eirueimi/unified-cd/releases/download/v0.6.0/agent-only.yaml
# Or always the newest release
kubectl apply -f https://github.com/eirueimi/unified-cd/releases/latest/download/agent-only.yaml
```

## Binaries

Pre-built binaries for Linux, macOS, and Windows (amd64/arm64) are available on the [Releases page](https://github.com/eirueimi/unified-cd/releases):

```bash
# Example: Linux amd64, pinned to the latest tagged release. Archive names
# are versioned (goreleaser's name_template), so update the version below to
# match whichever release you're installing.
curl -L https://github.com/eirueimi/unified-cd/releases/download/v0.5.0/unified-cd_0.5.0_linux_amd64.tar.gz | tar xz
sudo mv unified-cli /usr/local/bin/
```

Or install from source with Go:

```bash
go install github.com/eirueimi/unified-cd/cmd/unified-cd-agent@latest    # → $GOBIN/unified-cd-agent
go install github.com/eirueimi/unified-cd/cmd/controller@latest  # controller
go install github.com/eirueimi/unified-cd/cmd/unified-cli@latest # CLI
```
