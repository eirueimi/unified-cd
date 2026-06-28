# Stage 1: Build Go binary
FROM golang:1.26-alpine AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /k8s-agent ./cmd/k8s-agent

# Stage 2: Minimal runtime image
FROM gcr.io/distroless/static-debian12
COPY --from=go-build /k8s-agent /k8s-agent
ENTRYPOINT ["/k8s-agent"]
