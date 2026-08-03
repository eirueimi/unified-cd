# Stage 1: Build Svelte frontend
FROM node:22-alpine AS node-build
WORKDIR /src
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# Stage 2: Build Go binary
FROM golang:1.26-alpine AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# VERSION stamps internal/controller.Version, reported in the startup log line
# and as the unifiedcd_build_info{version} gauge on /metrics.
# .github/workflows/release-docker.yml passes the release tag as a build arg.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-X github.com/eirueimi/unified-cd/internal/controller.Version=${VERSION}" \
    -o /controller ./cmd/controller

# Stage 3: Runtime image.
# alpine (not distroless) because the AppSource reconciler shells out to the
# git CLI at runtime (internal/gittemplate: git ls-remote / fetch) to resolve
# and read repo contents. distroless has no git, causing "exec: git: not found".
FROM alpine:3.20
RUN apk add --no-cache git ca-certificates
COPY --from=go-build /controller /controller
COPY --from=node-build /src/dist /ui
ENV UNIFIED_WEB_DIR=/ui
ENTRYPOINT ["/controller"]
