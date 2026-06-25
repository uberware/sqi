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
# Raise in 5-point increments as new test suites land.
# 2026-06-13: measured 74.5% (race) after the phase-1 unit-test backfill;
# gate set ~5 points below for headroom against per-platform fluctuation.
COVERAGE_MIN ?= 70

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

# The web bundle is a prerequisite of build-server (not just build) so the
# embedded web/dist/ is rebuilt from current source on every server build,
# including under `make -j` and via run-server.
#
# npm ci is the slow step (it wipes and reinstalls node_modules), so it is
# gated on a stamp file keyed to the npm manifests and only re-runs when
# dependencies change. The vite build itself is sub-second and always runs,
# so web/dist/ always matches current source without make having to track
# individual web source files. npm ci deletes the stamp along with
# node_modules, so an interrupted install re-runs from scratch.
web/node_modules/.make-stamp: web/package.json web/package-lock.json
	cd web && npm ci
	touch $@

.PHONY: build-web
build-web: web/node_modules/.make-stamp ## Build the web UI bundle (web/dist/) embedded by sqi-server
	cd web && npm run build

.PHONY: build-server
build-server: build-web ## Build sqi-server binary into ./bin/
	@mkdir -p $(BUILD_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) $(CMD_DIR)

.PHONY: build-worker
build-worker: ## Build sqi-worker binary into ./bin/
	@mkdir -p $(BUILD_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(WORKER_BINARY) $(WORKER_CMD_DIR)

.PHONY: build-all
build-all: build-web ## Cross-compile both binaries for linux/darwin/windows × amd64/arm64
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

# run-workers spins up several sqi-worker instances on this one host to simulate
# a multi-worker farm locally. Each instance gets a unique identity so they do
# not collide: its own data dir (which holds the persistent worker.id UUID and
# session dirs), its own metrics/health port, and a distinct UI name. They all
# discover/connect to the same sqi-server (run `make run` in another terminal).
# Inherited SQI_WORKER_* env vars still apply, so e.g.
#   SQI_WORKER_NATS_URL=nats://127.0.0.1:4222 make run-workers N=5
# overrides discovery and starts five workers. Ctrl-C stops all of them.
# Number of workers to start, and a short `N=` alias so `make run-workers N=5` works.
WORKERS                  ?= 3
N                        ?= $(WORKERS)
# First worker's metrics/health port; each subsequent worker increments by one.
WORKER_METRICS_BASE_PORT ?= 9091
# Per-instance state (worker.id UUID + session dirs) lives under here, one subdir
# per worker, so instances never share an identity.
WORKER_DATA_ROOT         ?= ./.run/workers

.PHONY: run-workers
run-workers: build-worker ## Spin up N sqi-worker instances locally (N=3 default; Ctrl-C stops all)
	@echo "Starting $(N) workers (Ctrl-C to stop all)..."
	@pids=""; \
	trap 'echo; echo "Stopping workers..."; kill $$pids 2>/dev/null; wait $$pids 2>/dev/null; exit 0' INT TERM; \
	for i in $$(seq 1 $(N)); do \
	  port=$$(( $(WORKER_METRICS_BASE_PORT) + i - 1 )); \
	  data_dir="$(WORKER_DATA_ROOT)/worker-$$i"; \
	  mkdir -p "$$data_dir"; \
	  echo "  worker-$$i  data_dir=$$data_dir  metrics=127.0.0.1:$$port"; \
	  SQI_WORKER_NAME="worker-$$i" \
	  SQI_WORKER_DATA_DIR="$$data_dir" \
	  SQI_WORKER_METRICS_ADDR="127.0.0.1:$$port" \
	    $(BUILD_DIR)/$(WORKER_BINARY) start & \
	  pids="$$pids $$!"; \
	done; \
	wait

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

.PHONY: smoke
smoke: build-server build-worker ## Run the end-to-end smoke test against the built binaries
	SQI_SERVER_BIN=$(BUILD_DIR)/$(BINARY) SQI_WORKER_BIN=$(BUILD_DIR)/$(WORKER_BINARY) \
	  bash scripts/smoke.sh

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

# actionlint version, run via `go run` so no global install is required.
# Files are passed explicitly (rather than relying on actionlint's directory
# auto-discovery) to skip macOS AppleDouble sidecar files (._*.yml) that appear
# when the repo lives on a non-APFS volume; CI never sees those.
ACTIONLINT_VERSION := v1.7.12

.PHONY: lint-actions
lint-actions: ## Lint GitHub Actions workflows with actionlint (via go run; no install)
	go run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION) \
	  $$(find .github/workflows -maxdepth 1 -type f \( -name '*.yml' -o -name '*.yaml' \) ! -name '._*')

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
# web/dist is intentionally not cleaned: web/dist/index.html is git-tracked so
# //go:embed in web/embed.go always has a file (see .gitignore), and build-web
# regenerates the bundle on every build anyway.

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

# ── Python client (clients/python) ────────────────────────────────────────────
# The sqi-sdk library lives in clients/python with its own toolchain (ruff,
# mypy, pytest). It is developed in an isolated virtualenv at clients/python/.venv
# so its dependencies never leak into the system interpreter. Targets cd into the
# project so ruff/mypy/pytest discover the pyproject.toml config there.
PY_DIR  := clients/python
PY_VENV := $(PY_DIR)/.venv

.PHONY: py-install
py-install: ## Create clients/python/.venv and install sqi-sdk editable with all extras
	python3 -m venv $(PY_VENV)
	$(PY_VENV)/bin/python -m pip install --upgrade pip
	$(PY_VENV)/bin/python -m pip install -e '$(PY_DIR)[yaml,ws,dev]'

.PHONY: py-fmt
py-fmt: ## Format the Python client with ruff
	cd $(PY_DIR) && .venv/bin/ruff format .

.PHONY: py-lint
py-lint: ## Lint the Python client with ruff
	cd $(PY_DIR) && .venv/bin/ruff check .

.PHONY: py-typecheck
py-typecheck: ## Type-check the Python client with mypy (strict; src at 3.9, tests at 3.13)
	cd $(PY_DIR) && .venv/bin/mypy src \
	  && .venv/bin/mypy --python-version=3.13 tests

.PHONY: py-test
py-test: ## Run the Python client unit tests with coverage
	cd $(PY_DIR) && .venv/bin/pytest

.PHONY: py-test-integration
py-test-integration: build-server build-worker ## Run the Python client integration tests against freshly-built binaries
	cd $(PY_DIR) && .venv/bin/pytest -m integration --no-cov

.PHONY: py-check
py-check: ## Full Python client gate (check-only): ruff format, ruff check, mypy, pytest
	cd $(PY_DIR) && .venv/bin/ruff format --check . \
	  && .venv/bin/ruff check . \
	  && .venv/bin/mypy src \
	  && .venv/bin/mypy --python-version=3.13 tests \
	  && .venv/bin/pytest

.PHONY: py-build
py-build: ## Build the Python client sdist + wheel into clients/python/dist/
	$(PY_VENV)/bin/python -m pip install --quiet --upgrade build
	cd $(PY_DIR) && .venv/bin/python -m build

# ── CI convenience target ─────────────────────────────────────────────────────

.PHONY: ci
ci: fmt-check vet lint test-cover ## Run the full CI check suite locally
