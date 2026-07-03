# Build the unified-sidecar binary (cache + artifact transfer, direct to S3).
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/unified-sidecar ./cmd/unified-sidecar

# Minimal runtime: just the static binary + CA certs. The k8s agent execs the
# binary via argv (no shell), so no bash/curl/tar/zstd are needed. The
# distroless static image has no shell and no `sleep` binary, so the pod
# keeps the sidecar container alive with `unified-sidecar idle` instead of
# `sleep infinity`.
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/unified-sidecar /usr/local/bin/unified-sidecar
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
ENTRYPOINT ["/usr/local/bin/unified-sidecar"]
