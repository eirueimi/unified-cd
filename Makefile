.PHONY: build test test-short ha-test lint clean fmt dev dev-go dev-ui ui-build embed-shim check-embed-clean manifests vscode-build vscode-package

GO ?= go
GOFLAGS ?= -trimpath
# Host build tooling reports the current machine's GOARCH; the shim always
# targets linux (job containers share the host arch, not the host OS — see
# docs/superpowers/specs/2026-07-12-step-shell-shim-design.md, Component 2).
# Uses `go env`, not `uname`, so this works from Windows git-bash too.
HOSTARCH ?= $(shell $(GO) env GOARCH)

ui-build:
	cd web && npm install && npm run build

# Builds a linux ucd-sh for the HOST arch only and overwrites the committed
# empty placeholder at internal/shim/embedded/ucd-sh-$(HOSTARCH) so
# cmd/agent's arch-tagged go:embed (embed_amd64.go / embed_arm64.go) picks
# up the real binary. Local dev only ever needs the host arch's shim — the
# other arch's placeholder stays empty and is excluded from compilation
# anyway via its go:build tag. Must run before building cmd/agent. Do not
# commit the result — see the package doc comment in
# internal/shim/embedded/embed.go. (Release builds for both arches are
# handled by scripts/build-shims.sh via .goreleaser.yaml's before.hooks.)
embed-shim:
	GOOS=linux GOARCH=$(HOSTARCH) $(GO) build $(GOFLAGS) -o internal/shim/embedded/ucd-sh-$(HOSTARCH) ./cmd/ucd-sh

build: ui-build embed-shim
	$(GO) build $(GOFLAGS) -o bin/unified-cd-controller ./cmd/controller
	$(GO) build $(GOFLAGS) -o bin/unified-cd-agent ./cmd/agent
	$(GO) build $(GOFLAGS) -o bin/unified-cli ./cmd/unified-cli

# Guards against accidentally committing a real ucd-sh binary in place of
# either empty placeholder after a local `make build`/`make embed-shim`
# run. Checks both the working-tree diff (catches an uncommitted local
# build artifact) and the git-blob size at HEAD (catches one that was
# already committed) — the latter mirrors the CI guard in
# .github/workflows/ci.yml so a local `make check-embed-clean` and CI agree.
check-embed-clean:
	git diff --stat --exit-code internal/shim/embedded/ucd-sh-amd64 internal/shim/embedded/ucd-sh-arm64
	test "$$(git cat-file -s HEAD:internal/shim/embedded/ucd-sh-amd64)" = "0"
	test "$$(git cat-file -s HEAD:internal/shim/embedded/ucd-sh-arm64)" = "0"

test:
	$(GO) test ./... -race -count=1

test-short:
	$(GO) test ./... -short -race -count=1

# Level-2 HA failover driver (build-tagged `ha`; requires Docker). Slow: builds images.
ha-test:
	cd test/ha && $(GO) test -tags ha -v -timeout 20m ./...

fmt:
	$(GO) fmt ./...

lint:
	$(GO) vet ./...

clean:
	rm -rf bin/

dev-go:
	air

dev-ui:
	cd web && npm run dev

dev:
	@echo "Run in separate terminals:"
	@echo "  make dev-go   # Go hot-reload on :8080"
	@echo "  make dev-ui   # Svelte HMR on :5173  (proxy /api → :8080)"

manifests:
	kubectl kustomize manifests/core-install > manifests/core-install.yaml
	kubectl kustomize manifests/install > manifests/install.yaml
	kubectl kustomize manifests/agent-only > manifests/agent-only.yaml

vscode-build:
	cd editors/vscode && npm install && npm run build

vscode-package:
	cd editors/vscode && npm install && npm run package
