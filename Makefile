.PHONY: build test lint clean fmt dev dev-go dev-ui ui-build manifests vscode-build vscode-package

GO ?= go
GOFLAGS ?= -trimpath

ui-build:
	cd web && npm install && npm run build

build: ui-build
	$(GO) build $(GOFLAGS) -o bin/unified-cd-controller ./cmd/controller
	$(GO) build $(GOFLAGS) -o bin/unified-cd-agent ./cmd/agent
	$(GO) build $(GOFLAGS) -o bin/unified-cli ./cmd/unified-cli

test:
	$(GO) test ./... -race -count=1

test-short:
	$(GO) test ./... -short -race -count=1

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
