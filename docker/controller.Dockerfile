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

# Stage 3: Minimal runtime image
FROM gcr.io/distroless/static-debian12
COPY --from=go-build /controller /controller
COPY --from=node-build /src/dist /ui
ENV UNIFIED_WEB_DIR=/ui
ENTRYPOINT ["/controller"]
