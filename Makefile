BINARY := dyne
GOLANGCI_LINT_VERSION := v2.11.4
GOLANGCI_LINT_VERSION_NUMBER := $(patsubst v%,%,$(GOLANGCI_LINT_VERSION))
DOCKER_CONTEXT ?= colima-codex-k8s
IMAGE ?= coding-agent:local

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show development commands
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\nTargets:\n"} /^[a-zA-Z0-9_-]+:.*##/ {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build dyne
	go build -o $(BINARY) ./cmd/dyne

.PHONY: run
run: build ## Run dyne with ARGS="..."
	./$(BINARY) $(ARGS)

.PHONY: test
test: ## Run all Go tests
	go test ./...

.PHONY: test-race
test-race: ## Run all Go tests with the race detector
	go test -race ./...

.PHONY: integration-test
integration-test: ## Run live Kubernetes tests with KUBERNETES_INTEGRATION_CONTEXT
	@test -n "$(KUBERNETES_INTEGRATION_CONTEXT)" || { printf 'KUBERNETES_INTEGRATION_CONTEXT is required\n'; exit 1; }
	KUBERNETES_INTEGRATION_CONTEXT="$(KUBERNETES_INTEGRATION_CONTEXT)" go test -cover -tags=integration -count=1 ./internal/kubernetes

.PHONY: e2e-test
e2e-test: image ## Run coding-session journeys on an isolated Kubernetes context
	@test -n "$(KUBERNETES_INTEGRATION_CONTEXT)" || { printf 'KUBERNETES_INTEGRATION_CONTEXT is required\n'; exit 1; }
	KUBERNETES_INTEGRATION_CONTEXT="$(KUBERNETES_INTEGRATION_CONTEXT)" E2E_IMAGE="$(IMAGE)" go test -tags=integration -count=1 -timeout=20m ./test/e2e

.PHONY: coverage
coverage: ## Write coverage.out
	go test -race -coverprofile=coverage.out ./...

.PHONY: fmt
fmt: require-linter ## Format Go source and imports
	golangci-lint fmt

.PHONY: fmt-check
fmt-check: require-linter ## Check Go formatting without changing files
	@diff="$$(golangci-lint fmt --diff)" || exit $$?; if [ -n "$$diff" ]; then printf '%s\n' "$$diff"; exit 1; fi

.PHONY: lint
lint: require-linter ## Run configured Go linters
	golangci-lint run

.PHONY: lint-fix
lint-fix: require-linter ## Apply safe linter and formatter fixes
	golangci-lint run --fix

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: tidy
tidy: ## Update go.mod and go.sum
	go mod tidy

.PHONY: mod-check
mod-check: ## Verify module checksums and tidy state
	go mod verify
	go mod tidy -diff

.PHONY: check
check: fmt-check mod-check vet lint test-race build ## Run all required local checks

.PHONY: image
image: ## Build the coding-agent image in the selected Docker context
	env -u DOCKER_HOST docker --context $(DOCKER_CONTEXT) build -f container/Dockerfile -t $(IMAGE) .

.PHONY: tools
tools: ## Install the pinned development tools
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: doctor
doctor: ## Check required and optional development tools
	@for tool in go git golangci-lint; do command -v $$tool >/dev/null || { printf 'required tool not found: %s\n' $$tool; exit 1; }; done
	@for tool in docker colima; do command -v $$tool >/dev/null || printf 'optional local-runtime tool not found: %s\n' $$tool; done
	@go version
	@golangci-lint version

.PHONY: clean
clean: ## Remove generated local artifacts
	rm -f $(BINARY) coverage.out

.PHONY: require-linter
require-linter:
	@command -v golangci-lint >/dev/null || { printf 'golangci-lint is required; run make tools\n'; exit 1; }
	@version="$$(golangci-lint version 2>/dev/null)"; case "$$version" in *"version $(GOLANGCI_LINT_VERSION_NUMBER) "*) ;; *) printf 'golangci-lint $(GOLANGCI_LINT_VERSION_NUMBER) is required; run make tools\n'; exit 1;; esac
