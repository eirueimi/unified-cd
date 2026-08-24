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

```bash
# Full install (controller + k8s-agent + PostgreSQL)
kubectl apply -f https://raw.githubusercontent.com/eirueimi/unified-cd/main/manifests/install.yaml

# k8s-agent only (connect to existing controller)
kubectl apply -f https://raw.githubusercontent.com/eirueimi/unified-cd/main/manifests/agent-only.yaml
```

## Binaries

Pre-built binaries for Linux, macOS, and Windows (amd64/arm64) are available on the [Releases page](https://github.com/eirueimi/unified-cd/releases):

```bash
# Example: Linux amd64
curl -L https://github.com/eirueimi/unified-cd/releases/latest/download/unified-cli_linux_amd64.tar.gz | tar xz
sudo mv unified-cli /usr/local/bin/
```

Or install from source with Go:

```bash
go install github.com/eirueimi/unified-cd/cmd/unified-cd-agent@latest    # → $GOBIN/unified-cd-agent
go install github.com/eirueimi/unified-cd/cmd/controller@latest  # controller
go install github.com/eirueimi/unified-cd/cmd/unified-cli@latest # CLI
```
