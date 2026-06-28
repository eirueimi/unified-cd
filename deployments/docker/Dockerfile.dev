FROM golang:1.26-alpine
RUN apk add --no-cache bash git ca-certificates && \
    go install github.com/air-verse/air@latest
WORKDIR /app
