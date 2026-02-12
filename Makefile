.PHONY: all build build-linux test test-race test-short lint vet fmt fmt-check \
       mock clean deps install setup-dev setup-ch check ci coverage cloc verify help

REPO_PATH := github.com/projecteru2/cocoon
REVISION := $(shell git rev-parse HEAD || echo unknown)
BUILTAT := $(shell date +%Y-%m-%dT%H:%M:%S)
VERSION := $(shell git describe --tags $(shell git rev-list --tags --max-count=1) 2>/dev/null || echo dev)
GO_LDFLAGS ?= -X $(REPO_PATH)/version.REVISION=$(REVISION) \
              -X $(REPO_PATH)/version.BUILTAT=$(BUILTAT) \
              -X $(REPO_PATH)/version.VERSION=$(VERSION)

ifneq ($(KEEP_SYMBOL), 1)
	GO_LDFLAGS += -s
endif

# --- Primary targets ---

all: deps lint test build ## Run deps, lint, test, and build

ci: fmt-check vet lint test build ## Run all CI checks

verify: lint fmt-check ## Verify code is lint-clean and formatted
	@git diff --exit-code || { echo "Files modified after verify; run 'make fmt' and commit."; exit 1; }

# --- Dependencies ---

deps: ## Tidy Go modules
	go mod tidy

# --- Build ---

build: ## Build cocoon binary
	CGO_ENABLED=0 go build -ldflags "$(GO_LDFLAGS)" -o cocoon ./cmd/cocoon/

build-linux: ## Cross-compile for linux/amd64
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(GO_LDFLAGS)" -o cocoon-linux-amd64 ./cmd/cocoon/

install: ## Install cocoon binary to GOPATH/bin
	go install -ldflags "$(GO_LDFLAGS)" ./cmd/cocoon/

# --- Testing ---

test: vet ## Run tests with race detection and coverage
	go test -race -timeout 120s -count=1 -cover -coverprofile=coverage.out ./...

test-race: ## Run tests with race detector only
	go test -race -timeout 120s -count=1 ./...

test-short: ## Run short tests (skip long-running tests)
	go test -short ./...

coverage: test ## Generate and display coverage report
	go tool cover -func=coverage.out
	@echo ""
	@echo "To view HTML coverage report: go tool cover -html=coverage.out"

# --- Code quality ---

vet: ## Run go vet
	go vet ./...

lint: ## Run golangci-lint
	golangci-lint run

fmt: ## Format code with gofumpt and goimports
	gofumpt -l -w .
	goimports -l -w .

fmt-check: ## Check formatting (fails if files need formatting)
	@test -z "$$(gofumpt -l .)" || { echo "Files need formatting (gofumpt):"; gofumpt -l .; exit 1; }
	@test -z "$$(goimports -l .)" || { echo "Files need formatting (goimports):"; goimports -l .; exit 1; }

check: vet lint test ## Run vet, lint, and test

# --- Code generation ---

mock: ## Generate mock implementations
	mockery --dir lock --output lock/mocks --name Locker
	mockery --dir hypervisor --output hypervisor/mocks --name Client
	mockery --dir storage --output storage/mocks --name ReferenceCounter
	mockery --dir storage --output storage/mocks --name COWManager
	mockery --dir storage --output storage/mocks --name GarbageCollector
	mockery --dir vm --output vm/mocks --name Manager
	mockery --dir image --output image/mocks --name Manager

# --- Maintenance ---

clean: ## Remove build artifacts, coverage files, and test cache
	rm -f cocoon cocoon-linux-* cocoon-darwin-*
	rm -rf bin/ dist/
	rm -f coverage.out coverage.html coverage.txt
	go clean -testcache

setup-dev: ## Install development tools and dependencies
	bash scripts/setup-dev.sh

setup-ch: ## Install Cloud Hypervisor and firmware (Linux only)
	bash scripts/init-cloud-hypervisor.sh

cloc: ## Count lines of code (requires cloc)
	cloc --exclude-dir=vendor,mocks,dist --exclude-ext=json .

# --- Help ---

help: ## Show this help message
	@echo "Cocoon Makefile targets:"
	@echo ""
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'
	@echo ""
