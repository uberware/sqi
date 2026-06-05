# sqi Makefile
# Targets: build, test, lint, run, clean, release, docs

# ── Variables ────────────────────────────────────────────────────────────────

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
# Current coverage is ~40%.  Start the CI gate at 35 to establish a floor
# and avoid flapping on minor test changes.  Raise in 5-point increments as
# new test suites land (integration tests in task 111 should push this well
# above 60).
COVERAGE_MIN ?= 35

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
	go test $(TEST_FLAGS) ./...

.PHONY: test-cover
test-cover: ## Run tests and emit coverage report
	go test $(TEST_FLAGS) -coverprofile=$(COVERAGE_OUT) -covermode=atomic ./...
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
	go test -bench=. -benchmem ./...

# ── Lint and Vet ─────────────────────────────────────────────────────────────

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint (install: https://golangci-lint.run/usage/install/)
	golangci-lint run ./...

.PHONY: lint-fix
lint-fix: ## Run golangci-lint with --fix for auto-correctable issues
	golangci-lint run --fix ./...

# ── Formatting ────────────────────────────────────────────────────────────────

.PHONY: fmt
fmt: ## Format code with gofumpt and goimports
	gofumpt -l -w .
	goimports -l -w .

.PHONY: fmt-check
fmt-check: ## Check formatting without modifying files (used in CI)
	@unformatted=$$(gofumpt -l .); \
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
