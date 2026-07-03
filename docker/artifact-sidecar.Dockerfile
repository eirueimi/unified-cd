# Minimal image with the tools needed to transfer artifacts between the pod
# workspace volume and the controller's artifact endpoint.
# bash is required because the agent's ExecStep runs `bash -lc <script>`
# (see internal/k8sagent/executor.go buildShellCommand) — alpine has no bash by default.
FROM alpine:3.20
RUN apk add --no-cache bash tar zstd curl ca-certificates
