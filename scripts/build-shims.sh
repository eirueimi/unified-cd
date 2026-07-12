#!/bin/sh
# Builds both linux ucd-sh shim binaries (amd64 and arm64) that
# internal/shim/embedded's arch-tagged go:embed directives
# (embed_amd64.go / embed_arm64.go) pick up, overwriting the committed
# empty placeholders at internal/shim/embedded/ucd-sh-amd64 and
# ucd-sh-arm64. Run before goreleaser's build matrix compiles cmd/agent for
# every goos/goarch combination — see .goreleaser.yaml's before.hooks and
# the package doc comment in internal/shim/embedded/embed.go.
#
# Assumes a POSIX sh and a `go` toolchain on PATH. Release CI runs on
# ubuntu-latest; this script is not exercised from a Windows dev machine
# (goreleaser isn't run locally there).
set -eu

GOOS=linux GOARCH=amd64 go build -trimpath -o internal/shim/embedded/ucd-sh-amd64 ./cmd/ucd-sh
GOOS=linux GOARCH=arm64 go build -trimpath -o internal/shim/embedded/ucd-sh-arm64 ./cmd/ucd-sh
