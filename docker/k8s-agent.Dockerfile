# Stage 1: Build Go binary
FROM golang:1.26-alpine AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /k8s-agent ./cmd/k8s-agent
# /ucd-sh ships alongside the agent binary so the k8s init container (this
# same image, see docs/superpowers/specs/2026-07-12-step-shell-shim-design.md
# Component 3) can self-install it into the shared /.ucd emptyDir via
# `/ucd-sh --install /.ucd/ucd-sh` — no go:embed needed on this binary.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /ucd-sh ./cmd/ucd-sh

# Stage 2: Minimal runtime image
FROM gcr.io/distroless/static-debian12
COPY --from=go-build /k8s-agent /k8s-agent
COPY --from=go-build /ucd-sh /ucd-sh
ENTRYPOINT ["/k8s-agent"]
