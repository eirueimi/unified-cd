# Stage 1: build the agent binary
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# cmd/unified-cd-agent go:embeds internal/shim/embedded/ucd-sh-<arch> (see
# docs/superpowers/specs/2026-07-12-step-shell-shim-design.md, Component
# 2/3, and the arch-tagged embed_amd64.go / embed_arm64.go) to inject into
# every job container it creates at /.ucd/ucd-sh. The committed
# ucd-sh-<arch> bytes are already in the build context (COPY . . above), so
# `go build ./cmd/unified-cd-agent` embeds them directly — no pre-build step.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /agent ./cmd/unified-cd-agent

# Stage 2: runtime with a shell (bash) for run: steps and git for uses: templates
FROM alpine:3.20
RUN apk add --no-cache bash coreutils git ca-certificates docker-cli
COPY --from=build /agent /usr/local/bin/agent
ENTRYPOINT ["agent"]
