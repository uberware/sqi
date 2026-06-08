# sqi Makefile
# Targets: build, test, lint, run, clean, release, docs

# ── Variables ────────────────────────────────────────────────────────────────
# npm places third-party JS packages in web/node_modules/. Some of those
# packages contain Go source files (e.g. flatted) that are not part of the sqi
# codebase. The three variables below exclude that directory from all Go tool
# invocations so that linting, testing, and formatting never touch it.
#
# GO_PKGS: filtered package list used by test, vet, bench.
# LINT_PKGS: explicit directory patterns for golangci-lint (no ./... recursion).
# FMT_DIRS: explicit paths for gofumpt/goimports (web/embed.go is listed
#   individually so ./web does not accidentally recurse into node_modules/).
GO_PKGS   := $(shell go list ./... | grep -v '/node_modules/')
LINT_PKGS := ./cmd/... ./internal/... ./pkg/... ./test/... ./web
FMT_DIRS  := ./cmd ./internal ./pkg ./test web/embed.go

MODULE              := github.com/uberware/sqi
BINARY              := sqi-server
CMD_DIR             := ./cmd/sqi-server
WORKER_BINARY       := sqi-worker
WORKER_CMD_DIR      := ./cmd/sqi-worker
BUILD_DIR           := ./bin

# Version embedding — use git tag if available, fall back to "dev"
VERSION      := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT       := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE   := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
GO_VERSION   := $(shell go version | awk '{print $$3}')

LDFLAGS := -s -w \
  -X $(MODULE)/internal/version.Version=$(VERSION) \
  -X $(MODULE)/internal/version.Commit=$(COMMIT) \
  -X $(MODULE)/internal/version.BuildDate=$(BUILD_DATE) \
  -X $(MODULE)/internal/version.GoVersion=$(GO_VERSION)

# Race detector on by default for tests; override with RACE=off
RACE         ?= on
ifeq ($(RACE),on)
  TEST_FLAGS := -race
else
  TEST_FLAGS :=
endif

COVERAGE_OUT := coverage.out
# Raise in 5-point increments as new test suites land
COVERAGE_MIN ?= 60

# ── Default ───────────────────────────────────────────────────────────────────

.DEFAULT_GOAL := help

# ── Help ──────────────────────────────────────────────────────────────────────

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | sort \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

# ── Build ─────────────────────────────────────────────────────────────────────

.PHONY: build
build: build-server build-worker ## Build both sqi-server and sqi-worker into ./bin/

.PHONY: build-server
build-server: ## Build sqi-server binary into ./bin/
	@mkdir -p $(BUILD_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) $(CMD_DIR)

.PHONY: build-worker
build-worker: ## Build sqi-worker binary into ./bin/
	@mkdir -p $(BUILD_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(WORKER_BINARY) $(WORKER_CMD_DIR)

.PHONY: build-all
build-all: ## Cross-compile both binaries for linux/darwin/windows × amd64/arm64
	@mkdir -p $(BUILD_DIR)
	@for bin_name in $(BINARY) $(WORKER_BINARY); do \
	  cmd_dir="./cmd/$$bin_name"; \
	  for os in linux darwin windows; do \
	    for arch in amd64 arm64; do \
	      ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
	      echo "Building $$bin_name $$os/$$arch..."; \
	      GOOS=$$os GOARCH=$$arch go build \
	        -ldflags "$(LDFLAGS)" \
	        -o $(BUILD_DIR)/$$bin_name-$$os-$$arch$$ext \
	        $$cmd_dir; \
	    done; \
	  done; \
	done

# ── Run ───────────────────────────────────────────────────────────────────────

.PHONY: run
run: run-server ## Build then run sqi-server (alias for run-server)

.PHONY: run-server
run-server: build-server ## Build then run sqi-server with default config
	$(BUILD_DIR)/$(BINARY) serve

.PHONY: run-worker
run-worker: build-worker ## Build then run sqi-worker with default config
	$(BUILD_DIR)/$(WORKER_BINARY) start

# ── Test ──────────────────────────────────────────────────────────────────────

.PHONY: test
test: ## Run all tests (race detector on by default; override with RACE=off)
	go test $(TEST_FLAGS) $(GO_PKGS)

.PHONY: test-cover
test-cover: ## Run tests and emit coverage report
	go test $(TEST_FLAGS) -coverprofile=$(COVERAGE_OUT) -covermode=atomic $(GO_PKGS)
	go tool cover -func=$(COVERAGE_OUT) | tail -1
	@cov=$$(go tool cover -func=$(COVERAGE_OUT) | tail -1 | awk '{print int($$3)}'); \
	  echo "Coverage: $$cov% (minimum: $(COVERAGE_MIN)%)"; \
	  if [ $$cov -lt $(COVERAGE_MIN) ]; then \
	    echo "ERROR: coverage below minimum threshold"; exit 1; \
	  fi

.PHONY: test-cover-html
test-cover-html: test-cover ## Open HTML coverage report in the browser
	go tool cover -html=$(COVERAGE_OUT)

.PHONY: test-integration
test-integration: ## Run integration tests (tagged 'integration')
	go test $(TEST_FLAGS) -tags integration ./test/...

.PHONY: bench
bench: ## Run benchmarks
	go test -bench=. -benchmem $(GO_PKGS)

# ── Lint and Vet ─────────────────────────────────────────────────────────────

.PHONY: vet
vet: ## Run go vet
	go vet $(GO_PKGS)

# Lint targets use explicit path patterns rather than ./... so that third-party
# Go code in web/node_modules/ (installed by npm) is not linted.
# ./web (no trailing /...) targets only the web Go package (embed.go).
LINT_PKGS := ./cmd/... ./internal/... ./pkg/... ./test/... ./web

.PHONY: lint
lint: ## Run golangci-lint (install: https://golangci-lint.run/usage/install/)
	golangci-lint run $(LINT_PKGS)

.PHONY: lint-fix
lint-fix: ## Run golangci-lint with --fix for auto-correctable issues
	golangci-lint run --fix $(LINT_PKGS)

# ── Formatting ────────────────────────────────────────────────────────────────

.PHONY: fmt
fmt: ## Format code with gofumpt and goimports
	gofumpt -l -w $(FMT_DIRS)
	goimports -l -w $(FMT_DIRS)

.PHONY: fmt-check
fmt-check: ## Check formatting without modifying files (used in CI)
	@unformatted=$$(gofumpt -l $(FMT_DIRS)); \
	  if [ -n "$$unformatted" ]; then \
	    echo "Unformatted files:"; echo "$$unformatted"; exit 1; \
	  fi

# ── Generate ──────────────────────────────────────────────────────────────────

.PHONY: generate
generate: ## Run go generate across the module
	go generate ./...

# ── Docs ─────────────────────────────────────────────────────────────────────

.PHONY: docs
docs: ## Serve Go package docs locally via pkgsite
	@which pkgsite > /dev/null 2>&1 || go install golang.org/x/pkgsite/cmd/pkgsite@latest
	pkgsite -open .

# ── Release ───────────────────────────────────────────────────────────────────

.PHONY: release
release: ## Build a release with goreleaser (install: https://goreleaser.com)
	GOVERSION=$(GO_VERSION) goreleaser release --clean

.PHONY: release-snapshot
release-snapshot: ## Build a local snapshot release (no publish, no git tag required)
	GOVERSION=$(GO_VERSION) goreleaser release --snapshot --clean

# ── Docker ────────────────────────────────────────────────────────────────────

.PHONY: docker-build
docker-build: ## Build the sqi-server Docker image
	docker build \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  --build-arg BUILD_DATE=$(BUILD_DATE) \
	  -t sqi-server:$(VERSION) \
	  -f deploy/Dockerfile .

.PHONY: docker-run
docker-run: ## Run the sqi-server Docker image with default config
	docker run --rm -p 8080:8080 sqi-server:$(VERSION) serve

# ── Clean ─────────────────────────────────────────────────────────────────────

.PHONY: clean
clean: ## Remove build artifacts and coverage output
	rm -rf $(BUILD_DIR) $(COVERAGE_OUT)

# ── Dependency helpers ────────────────────────────────────────────────────────

.PHONY: deps
deps: ## Download and tidy Go module dependencies
	go mod download
	go mod tidy

.PHONY: deps-upgrade
deps-upgrade: ## Upgrade all Go dependencies to latest minor/patch
	go get -u ./...
	go mod tidy

.PHONY: hooks
hooks: ## Install git hooks via lefthook (install: go install github.com/evilmartians/lefthook@latest)
	lefthook install

# ── CI convenience target ─────────────────────────────────────────────────────

.PHONY: ci
ci: fmt-check vet lint test-cover ## Run the full CI check suite locally
