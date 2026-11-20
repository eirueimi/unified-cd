FROM golang:1.26.11-alpine
RUN apk add --no-cache bash git ca-certificates docker-cli && \
    go install github.com/air-verse/air@latest
WORKDIR /app
