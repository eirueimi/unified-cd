# Minimal image with the tools needed to transfer artifacts between the pod
# workspace volume and the controller's artifact endpoint.
FROM alpine:3.20
RUN apk add --no-cache tar zstd curl ca-certificates
