.PHONY: build test test-short ha-test lint clean fmt dev dev-go dev-ui ui-build generate manifests vscode-build vscode-package docs-build docs-serve

GO ?= go
GOFLAGS ?= -trimpath
# Stamped into every binary's version variable (unified-cli's `version`
# command / `--version` flag, the controller's `--version` and startup log
# line, the agent's registered version). Falls back to "dev" outside a git
# checkout or when no tag exists yet.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

ui-build:
	cd web && npm install && npm run build

build: ui-build
	$(GO) build $(GOFLAGS) -ldflags "-X github.com/eirueimi/unified-cd/internal/controller.Version=$(VERSION)" -o bin/unified-cd-controller ./cmd/controller
	$(GO) build $(GOFLAGS) -ldflags "-X github.com/eirueimi/unified-cd/internal/agent.Version=$(VERSION)" -o bin/unified-cd-agent ./cmd/unified-cd-agent
	$(GO) build $(GOFLAGS) -ldflags "-X github.com/eirueimi/unified-cd/internal/cli.version=$(VERSION)" -o bin/unified-cli ./cmd/unified-cli

generate:
	$(GO) generate ./...

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
	scripts/build-manifests.sh manifests

vscode-build:
	cd editors/vscode && npm install && npm run build

vscode-package:
	cd editors/vscode && npm install && npm run package

# Documentation site (MkDocs). Python-only; no Go or web-UI target depends on
# this. Install once with: python -m pip install -r docs/requirements.txt
docs-build:
	python -m mkdocs build --strict

docs-serve:
	python -m mkdocs serve
