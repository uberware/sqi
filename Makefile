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
#
# GO_PKGS shells out, so it is assigned with the deferred `=` described under
# "Deferred shell-outs" below rather than `:=`.
GO_PKGS    = $(eval GO_PKGS := $(shell go list ./... | grep -v '/node_modules/'))$(GO_PKGS)
LINT_PKGS := ./cmd/... ./internal/... ./pkg/... ./test/... ./web
FMT_DIRS  := ./cmd ./internal ./pkg ./test web/embed.go

MODULE              := github.com/uberware/sqi
BINARY              := sqi-server
CMD_DIR             := ./cmd/sqi-server
WORKER_BINARY       := sqi-worker
WORKER_CMD_DIR      := ./cmd/sqi-worker
BUILD_DIR           := ./bin

# ── Deferred shell-outs ───────────────────────────────────────────────────────
# Every $(shell ...) below runs a POSIX one-liner, and make runs it through its
# shell — cmd.exe on Windows, which understands none of them (no grep, no awk,
# no /dev/null, and its internal DATE command prompts for a new system date).
# With `:=` all of them run at parse time, on *every* make invocation, so
# `make test-isolation-windows` — the one target meant to be run from a Windows
# shell, and one that references none of these — printed four unrelated shell
# errors before doing anything. `=` defers each value until a recipe actually
# references it, which keeps the errors on the targets that genuinely need a
# POSIX shell.
#
# The `$(eval X := ...)` wrapper caches the result on first reference so each
# command still runs at most once per make run, as `:=` did. That is not just
# an optimization: without it BUILD_DATE would be re-evaluated per reference
# and sqi-server and sqi-worker could be stamped with different timestamps.
#
# Version embedding — use git tag if available, fall back to "dev"
VERSION      = $(eval VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev"))$(VERSION)
COMMIT       = $(eval COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown"))$(COMMIT)
BUILD_DATE   = $(eval BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ))$(BUILD_DATE)
GO_VERSION   = $(eval GO_VERSION := $(shell go version | awk '{print $$3}'))$(GO_VERSION)

# Deferred (`=`) so it does not force the four variables above to expand at
# parse time, which would defeat the deferral described above.
LDFLAGS = -s -w \
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

.PHONY: test-conformance
test-conformance: ## Run the official OpenJD conformance suite (needs the pinned submodule)
	go test $(TEST_FLAGS) -tags conformance -v ./test/conformance/

# The OpenJD expression-language reference implementation, pinned. It ships as
# the openjd.expr namespace of openjd-model, which is a thin re-export layer
# over a compiled Rust crate rather than a Python implementation. Pinned
# because it is Beta (0.x, breaking changes permitted in minor bumps), so an
# unpinned upgrade could turn the differential test red without a single sqi
# commit — and because a divergence report is meaningless without knowing
# which build of the reference produced it.
OPENJD_MODEL_VERSION ?= 0.11.1
ORACLE_VENV := .venv-oracle

.PHONY: expr-oracle-venv
expr-oracle-venv: ## Create the venv holding the pinned OpenJD reference implementation
	@python3 -m venv $(ORACLE_VENV)
	@$(ORACLE_VENV)/bin/python3 -m pip install --quiet --upgrade pip
	@$(ORACLE_VENV)/bin/python3 -m pip install --quiet "openjd-model==$(OPENJD_MODEL_VERSION)"
	@echo "reference implementation ready: openjd-model $(OPENJD_MODEL_VERSION) in $(ORACLE_VENV)"

# Differential test against the reference implementation. Like test-isolation,
# this exits 0 when its dependency is absent, so A LOCAL PASS PROVES NOTHING
# ON ITS OWN — look for the "--- PASS: TestExprOracle" line. CI asserts it by
# name for that reason.
.PHONY: test-expr-oracle
test-expr-oracle: ## Differential-test the EXPR evaluator against the OpenJD reference (needs python3)
	@if [ ! -x "$(ORACLE_VENV)/bin/python3" ] && [ -z "$$SQI_EXPR_ORACLE_PYTHON" ]; then \
	  if ! command -v python3 >/dev/null 2>&1; then \
	    echo "python3 unavailable — skipping the expression oracle"; exit 0; fi; \
	  echo "no $(ORACLE_VENV) — creating it (run 'make expr-oracle-venv' to do this explicitly)"; \
	  $(MAKE) --no-print-directory expr-oracle-venv || \
	    { echo "could not install the reference implementation — skipping the expression oracle"; exit 0; }; \
	fi
	go test $(TEST_FLAGS) -tags oracle -run 'TestExprOracle' -v -timeout 5m ./test/oracle/

.PHONY: test-ldap
test-ldap: ## Run the LDAP tests against a real directory in a container (needs Docker)
	go test $(TEST_FLAGS) -tags integration -run 'TestLDAP_' -v -timeout 15m ./test/integration/

.PHONY: test-oidc
test-oidc: ## Run the SSO tests against a real Keycloak in a container (needs Docker)
	go test $(TEST_FLAGS) -tags integration -run 'TestOIDC_' -v -timeout 15m ./test/integration/

# Unlike test-ldap/test-oidc (which run natively on the host and connect OUT to
# a container), test-isolation must run the go test binary ITSELF as root
# inside the container: the whole point is exercising real setuid/setgid
# transitions, real directory permission bits, and a real symlink-preserving
# rsync against real unprivileged accounts, none of which a fake Provider can
# see (internal/worker/isolation/fake.go).
#
# The image is built from a STAGED COPY of the repo (rsync'd into a scratch
# directory, filtered by test/integration/isolation/.dockerignore, then passed
# to `docker build` as the context) rather than either (a) bind-mounting the
# repo at `docker run` time, or (b) using the repo root directly as the build
# context. (a) broke outright on this project's own dev machines: Colima
# (common on macOS) only virtiofs-shares $HOME by default, so a repo living
# elsewhere (e.g. /Volumes/...) resolves to an EMPTY bind mount and `go test`
# fails with "go.mod file not found" before a single test runs — `docker
# build`, by contrast, has no such dependency, since the CLI reads its context
# from wherever it runs and streams it to the daemon regardless of what the
# daemon's host shares. (b) doesn't work either: the repo-root .dockerignore
# (shared with deploy/docker/Dockerfile's production build) excludes test/
# entirely, and this image needs test/integration/**; the classic
# (non-BuildKit) builder this project's Docker install runs has no per-
# Dockerfile ignore-file override to give this build its own rules on that
# same context. A staged copy sidesteps both problems at once — PROVIDED the
# repo-root .dockerignore is not itself staged into the copy: rsync -a copies
# dotfiles, so a naive staged copy carries the repo-root .dockerignore along
# to $ctx/.dockerignore, and Docker auto-discovers a context-root
# .dockerignore from a directory the same way regardless of which Dockerfile
# is building it — silently re-excluding test/ from the staged copy exactly as
# it would from the repo root directly. The recipe below excludes every
# .dockerignore from the rsync and then places
# test/integration/isolation/.dockerignore at the staged root explicitly, so
# Docker's own (real, no-trick) context-root ignore-file discovery sees only
# this image's small, correct exclusion list.
#
# --init runs a real init (tini) as container PID 1: without it, the `go
# test` process itself is PID 1, which never reaps re-parented grandchildren
# after a process-group kill — a container-hygiene artifact of the TEST
# HARNESS, not of isolation.Apply, but one that produces a false failure in
# TestIsolation_ProcessGroupKillReapsPrivilegeDroppedGrandchild without it.
.PHONY: test-isolation
test-isolation: ## Run run-as-user isolation tests as root against real OS accounts in a container (needs Docker)
	@if ! docker info >/dev/null 2>&1; then \
	  echo "docker unavailable — skipping isolation integration tests"; exit 0; fi
	@ctx=$$(mktemp -d) && trap 'rm -rf "$$ctx"' EXIT && \
	rsync -a --exclude-from=test/integration/isolation/.dockerignore --exclude='.dockerignore' "$(CURDIR)/" "$$ctx/" && \
	cp test/integration/isolation/.dockerignore "$$ctx/.dockerignore" && \
	docker build -q -t sqi-isolation-test -f test/integration/isolation/Dockerfile "$$ctx" && \
	docker run --rm --init sqi-isolation-test \
	  go test $(TEST_FLAGS) -tags integration -run 'TestIsolation_' -v -timeout 15m ./test/integration/

.PHONY: test-isolation-windows
test-isolation-windows: ## Run windows run-as-user isolation tests as SYSTEM against real local accounts (needs an elevated shell)
	@powershell -NoProfile -ExecutionPolicy Bypass -File scripts/test-isolation-windows.ps1

.PHONY: bench
bench: ## Run benchmarks
	go test -bench=. -benchmem $(GO_PKGS)

.PHONY: smoke
smoke: build-server build-worker ## Run the end-to-end smoke test against the built binaries
	SQI_SERVER_BIN=$(BUILD_DIR)/$(BINARY) SQI_WORKER_BIN=$(BUILD_DIR)/$(WORKER_BINARY) \
	  bash scripts/smoke.sh

.PHONY: auth-demo
auth-demo: build-server build-worker ## Run the auth surface demo on a live local farm (KEEP=1 to leave it running)
	SQI_SERVER_BIN=$(BUILD_DIR)/$(BINARY) SQI_WORKER_BIN=$(BUILD_DIR)/$(WORKER_BINARY) \
	SQI_AUTH_DEMO_KEEP=$(if $(KEEP),$(KEEP),0) \
	  bash scripts/auth-demo.sh

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

# ── Docs site (MkDocs) ────────────────────────────────────────────────────────
# The public documentation site (docs/ + mkdocs.yml), published to GitHub Pages
# on release. Uses a local virtualenv at .venv-docs so it never touches system
# Python. Run docs-site-install once, then docs-site / docs-site-serve.
DOCS_VENV := .venv-docs
# Strip macOS AppleDouble sidecars (._*) that some network/exFAT mounts create
# on every write; jinja2 chokes on them when loading theme templates. No-op on
# clean filesystems (Linux CI, local APFS) where the find matches nothing.
DOCS_CLEAN := find $(DOCS_VENV) -name '._*' -delete 2>/dev/null || true

.PHONY: docs-site-install
docs-site-install: ## Create .venv-docs and install pinned MkDocs dependencies
	@test -d $(DOCS_VENV) || python3 -m venv $(DOCS_VENV)
	$(DOCS_VENV)/bin/python -m pip install -r requirements-docs.txt

.PHONY: docs-site
docs-site: ## Build the docs site with strict checks (the CI gate)
	@$(DOCS_CLEAN)
	$(DOCS_VENV)/bin/mkdocs build --strict

.PHONY: docs-site-serve
docs-site-serve: ## Serve the docs site locally with live reload
	@$(DOCS_CLEAN)
	$(DOCS_VENV)/bin/mkdocs serve

# ── Release ───────────────────────────────────────────────────────────────────

.PHONY: changelog
changelog: ## Regenerate CHANGELOG.md from Conventional Commits (VERSION=x.y.z tags unreleased commits)
	@which git-cliff > /dev/null 2>&1 || { echo "git-cliff not found — install: https://git-cliff.org/docs/installation"; exit 1; }
	git-cliff $(if $(VERSION),--tag v$(VERSION),) --output CHANGELOG.md
	@echo "Wrote CHANGELOG.md$(if $(VERSION), (unreleased commits tagged v$(VERSION)),)"

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
