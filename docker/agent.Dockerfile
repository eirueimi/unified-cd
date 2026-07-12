# Stage 1: build the agent binary
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Two-stage build: cmd/agent go:embeds internal/shim/embedded/ucd-sh-<arch>
# (see docs/superpowers/specs/2026-07-12-step-shell-shim-design.md,
# Component 2/3, and the arch-tagged embed_amd64.go / embed_arm64.go) to
# inject into every job container it creates at /.ucd/ucd-sh. The files
# checked into git are intentional empty placeholders, so the one matching
# this builder's own arch must be overwritten with a real linux ucd-sh
# binary before `go build ./cmd/agent` runs, or the agent ships with no
# shim. This builder stage runs at the image's target arch (no cross-arch
# build here), so `go env GOARCH` picks the right file.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o internal/shim/embedded/ucd-sh-$(go env GOARCH) ./cmd/ucd-sh
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /agent ./cmd/agent

# Stage 2: runtime with a shell (bash) for run: steps and git for uses: templates
FROM alpine:3.20
RUN apk add --no-cache bash coreutils git ca-certificates docker-cli
COPY --from=build /agent /usr/local/bin/agent
ENTRYPOINT ["agent"]
