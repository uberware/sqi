# sqi Development Guide

This document covers local setup, the test and lint workflow, code layout
conventions, a step-by-step walkthrough for adding a new REST endpoint, and
guides for extending the worker.

---

## Prerequisites

| Tool | Purpose | Install |
|---|---|---|
| Go ≥ 1.23 (see `go.mod` for the pinned toolchain) | Build and test | [go.dev/dl](https://go.dev/dl/) |
| Node.js ≥ 24 with npm ≥ 11 (see `.nvmrc` and `web/package.json` `engines`) | Build the web UI bundle embedded in `sqi-server` (`make build` runs it) | [nodejs.org](https://nodejs.org/) or `nvm use` |
| `gofumpt` | Stricter formatter (superset of `gofmt`) | `go install mvdan.cc/gofumpt@latest` |
| `goimports` | Import organiser | `go install golang.org/x/tools/cmd/goimports@latest` |
| `golangci-lint` | Linter suite | [golangci-lint.run/usage/install](https://golangci-lint.run/usage/install/) |
| `lefthook` | Git hook runner | `go install github.com/evilmartians/lefthook@latest` |
| `pkgsite` | Local pkg.go.dev docs server | `go install golang.org/x/pkgsite/cmd/pkgsite@latest` |
| Docker | Build and run the container image | [docs.docker.com](https://docs.docker.com/get-docker/) |

`gofumpt`, `goimports`, and `golangci-lint` are required at commit time via
pre-commit hooks. Install them before running `make hooks`.

---

## First-time setup

```sh
git clone https://github.com/uberware/sqi.git
cd sqi

# Install git hooks (gofumpt, goimports, go vet, golangci-lint on every commit)
make hooks

# Build both binaries
make build

# Start sqi-server with default config (SQLite: sqi.db, NATS: 127.0.0.1:4222)
make run
```

The server is now reachable at `http://localhost:8080`. Confirm it is healthy:

```sh
curl -sf http://localhost:8080/healthz && echo OK
curl -sf http://localhost:8080/readyz  && echo OK
```

---

## Makefile targets

Run `make` (no arguments) to see all available targets with descriptions.

| Target | Description |
|---|---|
| `make build` | Build `sqi-server` and `sqi-worker` into `./bin/` (builds the web UI first) |
| `make build-server` | Build `sqi-server` only (builds the web UI first) |
| `make build-web` | Build the web UI bundle into `web/dist/` (`npm ci` runs only when npm manifests change) |
| `make run` | Build then run `sqi-server` with default config |
| `make test` | Run all tests with the race detector enabled |
| `make test-cover` | Run tests and print coverage; fails below 35% |
| `make test-cover-html` | Open an HTML coverage report in the browser |
| `make test-integration` | Run integration tests (build tag `integration`) |
| `make bench` | Run benchmarks |
| `make lint` | Run `golangci-lint` |
| `make lint-fix` | Run `golangci-lint --fix` |
| `make fmt` | Format all Go files with `gofumpt` and `goimports` |
| `make vet` | Run `go vet ./...` |
| `make docs` | Serve Go package docs at `localhost:8080` via `pkgsite` |
| `make hooks` | Install git hooks via `lefthook` |
| `make clean` | Remove build artifacts and `coverage.out` |
| `make ci` | Run the full CI sequence (build, vet, lint, test-cover) |

Override the race detector: `make test RACE=off`

Override the coverage threshold: `make test-cover COVERAGE_MIN=50`

---

## Running tests

```sh
# All tests, race detector on (default)
make test

# Specific package
go test -race ./internal/scheduler/...

# Single test function
go test -race -run TestScheduler_LicenseGating ./internal/scheduler/...

# With verbose output
go test -race -v ./internal/openjd/...

# Integration tests (require the integration build tag)
make test-integration

# Fuzz targets (run for 30 seconds each)
go test -fuzz=FuzzParse         -fuzztime=30s ./internal/openjd/...
go test -fuzz=FuzzRESTPayloads  -fuzztime=30s ./internal/api/...
```

---

## Code layout

```
sqi/
├── cmd/
│   ├── sqi-server/        Entry point, Cobra subcommands (serve, migrate, backup, config, version)
│   └── sqi-worker/        Worker entry point
├── internal/
│   ├── api/               HTTP router, REST handlers, WebSocket upgrade, OpenAPI spec
│   ├── bus/               Typed NATS JetStream client wrapper
│   ├── config/            Typed config struct, layered loader
│   ├── discovery/         mDNS _sqi._tcp responder
│   ├── health/            /healthz and /readyz handlers
│   ├── log/               slog helpers
│   ├── metrics/           Prometheus metric definitions
│   ├── middleware/         HTTP middleware (logging, metrics, versioning)
│   ├── openjd/            OpenJD parser, validator, parameter-space expansion
│   ├── scheduler/         Assignment loop, worker registry, heartbeat sweep
│   ├── server/            Process boot, graceful shutdown orchestration
│   ├── store/             Store interface + SQLite implementation + migrations
│   ├── ui/                Embeds web/dist; SPA fallback handler
│   ├── version/           Build metadata (version, commit, date, Go version)
│   ├── worker/            Server-side handlers for the worker wire protocol
│   └── ws/                WebSocket hub, subscription management, NATS fanout
├── pkg/                   Public Go API (empty in Phase 1; see pkg/doc.go)
├── api/                   Source-of-truth specs: OpenAPI 3.1, JSON schemas
├── web/                   Frontend source (Phase 2); web/dist is embedded
├── config/                Example config files
├── deploy/                Docker and infrastructure manifests
├── docs/                  Human-readable documentation
├── scripts/               Utility scripts (SBOM, signing, etc.)
└── test/                  Integration test harness (Phase 2)
```

### Key conventions

**No cross-imports between internal packages at the same level.** The
dependency direction is:
`cmd` → `internal/server` → `internal/api`, `internal/scheduler` → `internal/store`, `internal/bus`

**Interfaces over concrete types at package boundaries.** Handlers receive the
`store.Store` interface, not `*sqlite.Store`, so tests can inject a fake.

**One file per route group.** `internal/api/jobs.go`, `internal/api/tasks.go`,
`internal/api/workers.go`, etc. Each file owns its handler struct, wire-format
types, and route-mounting helper.

**Table-driven tests.** Tests use `[]struct{ name, input, want }` slices with
`t.Run(tc.name, ...)` rather than separate test functions per case.

**SPDX header on every source file.**
```go
// SPDX-License-Identifier: AGPL-3.0-only
```

**Conventional Commits.** The `commit-msg` hook enforces the format:
`type(scope)?: description`. Valid types: `feat fix docs style refactor test chore build ci perf revert`.

---

## Adding a new REST endpoint

This walkthrough adds a hypothetical `GET /api/v1/jobs/{id}/steps` endpoint as
a concrete example. Follow the same pattern for any new route.

### Step 1 — Add the handler method

Open (or create) the relevant file in `internal/api/`. For a new route on an
existing resource, add a method to the existing handler struct. For a wholly
new resource, create `internal/api/<resource>.go` following the pattern in
`jobs.go`.

```go
// internal/api/steps.go

// SPDX-License-Identifier: AGPL-3.0-only

package api

// GET /api/v1/jobs/{id}/steps
func (h *jobHandler) listSteps(w http.ResponseWriter, r *http.Request) {
    jobID := chi.URLParam(r, "id")
    ctx := r.Context()

    steps, err := h.store.ListStepsForJob(ctx, jobID)
    if err != nil {
        if errors.Is(err, store.ErrNotFound) {
            writeError(w, r, http.StatusNotFound, "job not found")
            return
        }
        h.logger.ErrorContext(ctx, "list steps", slog.String("job_id", jobID), slog.Any("err", err))
        writeError(w, r, http.StatusInternalServerError, "internal error")
        return
    }

    writeJSON(w, http.StatusOK, stepListResponse{
        Items: stepsToResponse(steps),
        Total: len(steps),
    })
}
```

### Step 2 — Wire the route in the router

Open `internal/api/router.go` and find the `/api/v1` sub-router block. Add
the new route alongside the related routes:

```go
// inside the r.Route("/api/v1", ...) block:
r.Get("/jobs/{id}/steps", jobs.listSteps)
```

### Step 3 — Add or extend the Store interface

The `Store` interface is composed from per-aggregate sub-interfaces. Each
sub-interface lives in its own file under `internal/store/` (e.g. `job.go`,
`task.go`, `step.go`). Add the new method to the relevant sub-interface file:

```go
// internal/store/step.go — add inside the StepStore interface
// ListStepsForJob returns all steps for the given job, ordered by step_order.
// Returns ErrNotFound when no job with that ID exists.
ListStepsForJob(ctx context.Context, jobID string) ([]Step, error)
```

Then implement it in `internal/store/sqlite/step.go` using a prepared
statement, and add a corresponding stub to the in-memory fake in
`internal/store/fake/store.go` so existing tests keep compiling.

### Step 4 — Update the OpenAPI spec

Add the new path to `internal/api/openapi.yaml`:

```yaml
  /jobs/{id}/steps:
    parameters:
      - $ref: "#/components/parameters/id"
    get:
      tags: [jobs]
      operationId: listSteps
      summary: List steps for a job
      responses:
        "200":
          description: Ordered list of steps with status.
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/StepList"
        "404":
          $ref: "#/components/responses/NotFound"
```

### Step 5 — Write tests

Add a test file (or extend an existing one) in `internal/api/`:

```go
// internal/api/steps_test.go

func TestListSteps(t *testing.T) {
    cases := []struct {
        name   string
        jobID  string
        want   int // expected HTTP status
    }{
        {"job exists",     existingJobID, http.StatusOK},
        {"job not found",  "nonexistent", http.StatusNotFound},
    }

    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            rec := httptest.NewRecorder()
            req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+tc.jobID+"/steps", nil)
            router.ServeHTTP(rec, req)
            if rec.Code != tc.want {
                t.Fatalf("got %d, want %d", rec.Code, tc.want)
            }
        })
    }
}
```

Run the tests: `go test -race ./internal/api/...`

### Step 6 — Run lint and format

```sh
make fmt
make lint
```

Fix any issues before committing.

---

## Local docs

Serve Go package docs locally with `pkgsite`:

```sh
make docs
# Opens http://localhost:8080 with pkg.go.dev-style rendering
```

---

## Environment variables for local overrides

| Variable | Purpose |
|---|---|
| `SQI_HTTP_ADDR` | Override the listen address (e.g. `127.0.0.1:9090`) |
| `SQI_STORE_SQLITE_PATH` | Point at a specific database file |
| `SQI_LOG_LEVEL` | Set to `debug` for verbose output |
| `SQI_LOG_FORMAT` | Set to `text` for human-readable logs |
| `SQI_HTTP_ENABLE_PPROF` | Set to `true` to enable profiling endpoints |

Example — run with debug logging against a test database:

```sh
SQI_LOG_LEVEL=debug SQI_LOG_FORMAT=text SQI_STORE_SQLITE_PATH=/tmp/test.db \
  ./bin/sqi-server serve
```

---

## Pre-commit hooks

The hooks installed by `make hooks` run automatically on every `git commit`:

1. **gofumpt** — formats staged Go files.
2. **goimports** — organises imports (stdlib / external / internal groups).
3. **go vet** — basic correctness checks.
4. **golangci-lint** — full linter suite with auto-fix.

A failing hook blocks the commit and prints the fix command. To bypass for a
WIP commit: `git commit --no-verify`.

To skip the slow `golangci-lint` step locally while keeping the others, add
a `lefthook-local.yml` (not committed):

```yaml
pre-commit:
  commands:
    golangci-lint:
      skip: true
```

---

## Troubleshooting

**Build fails with "toolchain not found"** — the `go.mod` file pins an exact
Go toolchain. Install the matching version from [go.dev/dl](https://go.dev/dl/)
or run `go install golang.org/dl/go1.26.3@latest && go1.26.3 download`.

**Tests fail with "database is locked"** — SQLite WAL mode is used, but
concurrent tests that share the same file can still conflict. Ensure each test
that opens a database uses a unique temporary path:
```go
db := t.TempDir() + "/test.db"
```

**`golangci-lint` not found** — install it following the
[official instructions](https://golangci-lint.run/usage/install/). The
`go install golangci-lint` method is not supported by the project.

**`gofumpt` or `goimports` not found after installing** — ensure `$(go env GOPATH)/bin`
is on your `$PATH`:
```sh
export PATH="$PATH:$(go env GOPATH)/bin"
```

---

## sqi-worker development

### Running a worker locally against a dev server

Start `sqi-server` in one terminal:

```sh
make run
# or: ./bin/sqi-server serve
```

The server exposes NATS on `127.0.0.1:4222` by default. In a second terminal,
start the worker pointing at it with debug logging:

```sh
SQI_WORKER_NATS_URL=nats://127.0.0.1:4222 \
SQI_WORKER_DISCOVERY_ENABLE_MDNS=false \
SQI_WORKER_LOG_FORMAT=text \
SQI_WORKER_LOG_LEVEL=debug \
  ./bin/sqi-worker start
```

The worker logs its worker ID, connected NATS URL, and detected capabilities at
startup. It will appear under **Workers** in the web UI at
`http://localhost:8080` within a few seconds.

To validate configuration without connecting:

```sh
SQI_WORKER_NATS_URL=nats://127.0.0.1:4222 \
  ./bin/sqi-worker start --dry-run
```

To run both components together in a single shell session:

```sh
# Terminal 1
make run

# Terminal 2
make build && \
SQI_WORKER_NATS_URL=nats://127.0.0.1:4222 \
SQI_WORKER_DISCOVERY_ENABLE_MDNS=false \
SQI_WORKER_LOG_FORMAT=text \
  ./bin/sqi-worker start
```

Worker tests:

```sh
# Unit tests for all worker packages
go test -race ./internal/worker/...

# Specific package
go test -race ./internal/worker/executor/...

# Integration test: boots a full server + worker binary
make test-integration
```

---

### Writing a new executor type

The task executor lives in `internal/worker/executor`. It is wired into the
pull loop via the `pull.TaskDispatcher` interface and into the heartbeat
publisher via `heartbeat.StateSource`. To change *how* tasks execute, you
implement a new type that satisfies these interfaces.

**The relevant interfaces** (defined in `internal/worker/pull/pull.go` and
`internal/worker/heartbeat/heartbeat.go`):

```go
// pull.TaskDispatcher — called by the pull loop for each incoming assignment.
type TaskDispatcher interface {
    Dispatch(ctx context.Context, msg *protocol.AssignMsg) error
}

// heartbeat.StateSource — queried by the heartbeat publisher on each tick.
type StateSource interface {
    ActiveTaskCount() int
    ActiveTaskIDs() []string
    LastAssignmentAt() *time.Time
}
```

**Steps to add a new executor type:**

1. **Create the executor package** — add a new file or sub-package under
   `internal/worker/executor/`, e.g.
   `internal/worker/executor/container/container.go`.

2. **Implement the interfaces** — your type must implement at minimum
   `pull.TaskDispatcher` and `heartbeat.StateSource`. It also needs to
   implement `cancel.TaskCanceler` if you want NATS-driven cancellation:

   ```go
   // cancel.TaskCanceler
   type TaskCanceler interface {
       Cancel(taskID string) bool
   }
   ```

3. **Publish status messages** — inject `*status.Publisher` and call
   `Running`, `Terminal`, and (on shutdown) `ShutdownFailed`. Match the
   existing executor's publish points:
   - `Running` immediately after the workload starts.
   - `Terminal` ("succeeded", "failed", "canceled") after it exits.

4. **Wire it in `cmd/sqi-worker/start.go`** — replace the `executor.New(...)`
   call with your constructor. Wire the same `statusPub`, `sessionMgr`,
   `metrics`, and `logPub` dependencies.

5. **Add unit tests** — create `executor_test.go` asserting at minimum:
   - Dispatch returns an error when at capacity.
   - Status messages are published for success and failure.
   - `DrainAndShutdown` (or equivalent) blocks until all goroutines exit.

The existing bare-metal executor (`internal/worker/executor/`) is the
canonical reference implementation.

---

### Adding a new capability tag to auto-detection

Auto-detected tags are produced by the `internal/worker/capabilities` package.
Detection is abstracted behind the `Probe` interface, with platform-specific
implementations in `probe_linux.go`, `probe_darwin.go`, `probe_windows.go`,
and `probe_other.go`.

**Steps to add a new auto-detected tag:**

1. **Add a method to the `Probe` interface** in
   `internal/worker/capabilities/capabilities.go`:

   ```go
   type Probe interface {
       OS() string
       OSVersion() string
       CPUCount() int
       RAMMb() int
       GPUInfo() GPUInfo
       // Add your new method:
       IsNVLink() bool
   }
   ```

2. **Implement it on `*defaultProbe` in the appropriate platform file(s):**

   - If the detection works identically on all platforms (e.g., a `runtime`
     package call), add a single implementation in `probe_default.go`.
   - If it is platform-specific, add the real implementation in the relevant
     file and a `false`/zero stub in the others:

   ```go
   // probe_linux.go
   func (*defaultProbe) IsNVLink() bool { return linuxIsNVLink() }

   // probe_darwin.go, probe_windows.go, probe_other.go
   func (*defaultProbe) IsNVLink() bool { return false }
   ```

3. **Consume the result in `Detect`** in `capabilities.go`. If the tag is a
   simple presence flag, add it to `c.Tags`:

   ```go
   func Detect(p Probe) Capabilities {
       // ... existing fields ...
       if p.IsNVLink() {
           c.Tags["nvlink"] = ""
       }
       return c
   }
   ```

   For a key=value tag:

   ```go
   if v := p.SomeString(); v != "" {
       c.Tags["some_key"] = v
   }
   ```

4. **Update the test probe** in `detect_test.go` — add the new method to the
   `fakeProbe` struct used in table-driven tests:

   ```go
   type fakeProbe struct {
       // ... existing fields ...
       isNVLink bool
   }
   func (f fakeProbe) IsNVLink() bool { return f.isNVLink }
   ```

   Add table rows covering the `true` and `false` cases.

5. **Document the new tag** in
   [`docs/worker-capabilities.md`](worker-capabilities.md) under the
   "Auto-detected tags" section.

6. **Run and format:**

   ```sh
   go test -race ./internal/worker/capabilities/...
   make fmt && make lint
   ```
