# Stage 1: build the agent binary
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /agent ./cmd/agent

# Stage 2: runtime with a shell (bash) for run: steps and git for uses: templates
FROM alpine:3.20
RUN apk add --no-cache bash coreutils git ca-certificates docker-cli
COPY --from=build /agent /usr/local/bin/agent
ENTRYPOINT ["agent"]
