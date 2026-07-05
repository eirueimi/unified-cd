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
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /controller ./cmd/controller

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
