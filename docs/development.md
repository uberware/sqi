# sqi Development Guide

This document covers local setup, the test and lint workflow, code layout
conventions, a step-by-step walkthrough for adding a new REST endpoint, and
guides for extending the worker.

---

## Prerequisites

| Tool | Purpose | Install |
|---|---|---|
| Go ≥ 1.26 (the `go` directive in `go.mod` pins 1.26.3) | Build and test | [go.dev/dl](https://go.dev/dl/) |
| Node.js ≥ 24 with npm ≥ 11 (see `.nvmrc` and `web/package.json` `engines`) | Build the web UI bundle embedded in `sqi-server` (`make build` runs it) | [nodejs.org](https://nodejs.org/) or `nvm use` |
| `gofumpt` | Stricter formatter (superset of `gofmt`) | `go install mvdan.cc/gofumpt@latest` |
| `goimports` | Import organizer | `go install golang.org/x/tools/cmd/goimports@latest` |
| `golangci-lint` | Linter suite | [golangci-lint.run/usage/install](https://golangci-lint.run/usage/install/) |
| `lefthook` | Git hook runner | `go install github.com/evilmartians/lefthook@latest` |
| `pkgsite` | Local pkg.go.dev docs server | `go install golang.org/x/pkgsite/cmd/pkgsite@latest` |
| Docker (optional) | Build and run the container image; also runs the real-directory LDAP tests (`make test-ldap`), the real-provider SSO tests (`make test-oidc`), and the real-root run-as-user isolation tests (`make test-isolation`), all of which skip cleanly without it | [docs.docker.com](https://docs.docker.com/get-docker/), or `brew install colima docker && colima start` |

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

# Start sqi-server with default config (SQLite: sqi.db, NATS: 0.0.0.0:4222)
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
| `make run-worker` | Build then run a single `sqi-worker` with default config |
| `make run-workers` | Build then run N `sqi-worker` instances locally (`N=3` default); Ctrl-C stops all |
| `make test` | Run all tests with the race detector enabled |
| `make test-cover` | Run tests and print coverage; fails below 70% (the `COVERAGE_MIN` default in the Makefile) |
| `make test-cover-html` | Open an HTML coverage report in the browser |
| `make test-integration` | Run integration tests (build tag `integration`) |
| `make test-ldap` | Run the LDAP tests against a real OpenLDAP directory in a container (needs Docker; **skips** without it) |
| `make test-oidc` | Run the SSO tests against a real Keycloak in a container (needs Docker; **skips** without it) |
| `make test-isolation` | Run run-as-user task-isolation tests as real root against real OS accounts in a container (needs Docker; **skips** without it) |
| `make test-expr-oracle` | Differential-test the EXPR evaluator against the OpenJD reference implementation (needs `python3`; **skips** without it) |
| `make expr-oracle-venv` | Create `.venv-oracle/` with the pinned reference implementation (`make test-expr-oracle` does this on demand) |
| `make smoke` | End-to-end smoke test against the real binaries (REST + WebSocket) |
| `make bench` | Run benchmarks |
| `make lint` | Run `golangci-lint` |
| `make lint-fix` | Run `golangci-lint --fix` |
| `make fmt` | Format all Go files with `gofumpt` and `goimports` |
| `make vet` | Run `go vet ./...` |
| `make docs` | Serve Go package docs at `localhost:8080` via `pkgsite` |
| `make changelog` | Regenerate `CHANGELOG.md` from Conventional Commits via `git-cliff` (`VERSION=x.y.z` tags the pending release) |
| `make hooks` | Install git hooks via `lefthook` |
| `make clean` | Remove build artifacts and `coverage.out` |
| `make ci` | Run the full CI sequence (fmt-check, vet, lint, test-cover) |

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

# LDAP tests against a real directory in a container (needs Docker)
make test-ldap

# SSO tests against a real Keycloak in a container (needs Docker)
make test-oidc

# Run-as-user task-isolation tests as real root against real OS accounts in a
# container (needs Docker)
make test-isolation

# Fuzz targets (run for 30 seconds each)
go test -fuzz=FuzzParse         -fuzztime=30s ./internal/openjd/...
go test -fuzz=FuzzRESTPayloads  -fuzztime=30s ./internal/api/...
```

### Testing against a real directory or identity provider

Two suites in `test/integration/` talk to a real external identity source
rather than a fake: `ldap_test.go` (`make test-ldap`) and `oidc_test.go`
(`make test-oidc`). Both are behind the `integration` build tag, both boot a
throwaway container, and **both skip rather than fail when Docker is
unavailable** — which means a green run proves nothing until you have confirmed
it actually executed. Check for `--- PASS` lines, not merely an exit code of
zero; `GOFLAGS=-count=1` defeats a cached result that would otherwise replay a
previous run.

Most LDAP tests drive a fake connection, which covers sqi's own logic but
cannot catch a mistake in how it uses go-ldap *on the wire* — a wrong search
scope, a misnamed attribute, a filter a real server rejects. `make test-ldap`
closes that: it boots a throwaway OpenLDAP container and drives the whole login
path against it in every supported bind mode. It runs in CI on every change
(the `ldap-integration` job), on **both amd64 and arm64** — the container image
is multi-arch and its variants have already proven not to be equivalent.

It **skips**, rather than fails, when Docker is unavailable, so it never blocks
a contributor who has not installed it — but a skip verifies nothing, so run it
before touching anything under `internal/auth/ldap/`.

To point it at a directory you already have, including a real Active Directory,
set `SQI_TEST_LDAP_URL` and no container is started:

```sh
SQI_TEST_LDAP_URL=ldap://dc01.example.com:389 make test-ldap
```

That directory must already hold the fixture tree — see the `seedLDIF` constant
in `test/integration/ldap_test.go` for the exact users, groups, and passwords.

**Reproducing an architecture-specific failure.** The image's variants are not
identical — most visibly, the memberof module registers its schema several
seconds later on x86 than on arm64, a window a fixture can sit inside on one
architecture and not the other. CI covers both, so it will tell you *which*
architecture broke; this reproduces that one locally without pushing:

```sh
SQI_TEST_LDAP_PLATFORM=linux/amd64 make test-ldap
```

It runs under emulation, so expect it to be several times slower than native —
worth it when CI is red and your machine is green.
Two fixture properties are load-bearing and easy to get wrong:

- **`memberOf` must be populated.** AD does this natively; OpenLDAP needs the
  `memberof` overlay, and the overlay's `olcMemberOfGroupOC` must match the
  objectClass your groups actually use. Get it wrong and every user
  authenticates successfully and silently lands on `default_role`.
- **The service account must be able to read other entries.** Template bind
  works without this (sqi binds *as* the user and reads that user's own entry),
  so a directory that fails only in search mode is the usual symptom.

The fixture asserts both up front, as the service account, and fails with a
message naming the cause rather than letting them surface as wrong roles later.

### Testing SSO against a real identity provider

The SSO unit tests drive a fake provider. It signs real tokens, so a validation
mistake surfaces — but a fake returns whatever the test asks for, so it cannot
show what a *real* provider **omits**. Keycloak emits no group membership at all
unless a protocol mapper is configured for it, and a token carrying no groups
still validates: every user then lands on `default_role`, a silent privilege
downgrade with no error anywhere. `make test-oidc` is what makes that class of
bug fail before it merges.

```sh
make test-oidc
```

It boots a throwaway Keycloak container, seeds a realm, and drives the whole
browser-shaped flow: the authorization-code round trip, group → role mapping, a
rename at the provider keeping the same account, state-mismatch refusal,
`prompt=login` forcing re-authentication, and the RP-initiated logout behavior
documented in [`docs/auth.md`](auth.md#provider-logout-is-weaker-than-it-looks).
It runs in CI on every change (the `oidc-integration` job) on **one**
architecture, unlike `ldap-integration` — Keycloak is a JVM application and none
of the HTTP or token behavior this job asserts on is architecture-dependent.

It **skips**, rather than fails, when Docker is unavailable. **A skip verifies
nothing**, so run it — and confirm it ran — before touching anything under
`internal/auth/oidc/` or the SSO routes in `internal/api/`. Go's test cache will
happily replay a previous result; `GOFLAGS=-count=1 make test-oidc` and a check
for `--- PASS` on each `TestOIDC_` is the only honest confirmation.

To point it at a realm you already have instead of starting a container, set
`SQI_TEST_OIDC_ISSUER`:

```sh
SQI_TEST_OIDC_ISSUER=https://keycloak.example.com/realms/farm make test-oidc
```

That realm must already hold the fixture's client, users, groups, and the
`groups` protocol mapper — see the seeding code in
`test/integration/oidc_test.go` for exactly what. Any test that **mutates** the
fixture through the admin API (the rename-at-the-provider case) skips under this
variable rather than editing a realm you did not intend it to, so an external
issuer covers strictly less than the container does.

Use `https://` for a remote issuer. Keycloak marks its session cookies `Secure`
unconditionally; Go's cookie jar exempts loopback from that rule, which is why
the container fixture works over plain HTTP on `127.0.0.1`, but a **non**-loopback
issuer over `http://` would hide the provider's session from the test client.

The Keycloak image tag is **pinned, not floating on `:latest`**, and the pin is
load-bearing: the test scrapes Keycloak's own login and logout-confirmation
markup. It appears in two places that must stay in step — `keycloakImage` in
`test/integration/oidc_test.go` and the `docker pull` in
`.github/workflows/ci.yml`.

---

### Differential-testing EXPR against the reference implementation

`make test-expr-oracle` evaluates a corpus of expressions with **both** sqi's Go
evaluator (`internal/openjd/expr`) and the implementation the EXPR spec names as
its reference, then compares the results.

This catches a class of defect no single-implementation test can reach: a
misreading of the spec applied *consistently*, which looks correct from the
inside because every test encodes the same misreading. It is not a conformance
check — `make test-conformance` is, and it runs the official fixture suite. The
oracle is supplementary: it explains *why* a fixture disagrees, and it reaches
expressions no fixture happens to contain.

```sh
make test-expr-oracle        # creates .venv-oracle/ on first run, then compares
make expr-oracle-venv        # (re)create the venv explicitly
SQI_EXPR_ORACLE_PYTHON=/path/to/python3 make test-expr-oracle
```

Moving parts:

| Path | Role |
|---|---|
| `test/oracle/corpus.txt` | The cases. One per line, `TARGET-TYPE<TAB>EXPRESSION`. |
| `test/oracle/baseline.txt` | Accepted divergences, each with the reasoning above it. |
| `scripts/expr-oracle.py` | Feeds the corpus to the reference over JSON lines. |
| `test/oracle/oracle_test.go` | Runs both sides and compares (build tag `oracle`). |

**The reference is not the authority.** Despite living in the `openjd.expr`
Python namespace, it is a thin re-export layer over a compiled Rust crate
(`openjd-expr`, in `OpenJobDescription/openjd-rs`), and that crate is **Beta** —
on the 0.x line, where breaking changes are permitted in minor bumps. The
specification outranks it. When the two disagree, read
`third_party/openjd-specifications/` and decide; do not change sqi to match the
reference. **Most** baselined entries are cases where **the reference is
wrong** — the corpus currently scores 891/1052 agreeing with 161 baselined
divergences, and each one's reasoning is argued in `test/oracle/baseline.txt`,
which is the authority on any individual ruling (an earlier revision of this
paragraph said "three of the five", a count that went stale several waves ago
and is corrected here rather than re-pinned). Two have been reported upstream:
[openjd-rs#291](https://github.com/OpenJobDescription/openjd-rs/issues/291) (a
`target_type` inherited by operands that must not have it, including the
operand `and`/`or` discards per section 2.1.6) and
[#290 item 6](https://github.com/OpenJobDescription/openjd-rs/issues/290#issuecomment-5125365894)
(a `path` as the **left** operand of an ordering operator inverts the
comparison).

**The oracle cannot see the Windows path flavor at all.** The reference's path
family is POSIX-only — it accepts a backslash in a replacement name and never
switches on a drive letter — so `test/oracle/corpus.txt` carries no
Windows-flavor case, and every Windows expectation in `internal/openjd/expr` is
pinned by that package's own unit tests against CPython's `PureWindowsPath`
instead. URI behavior *is* oracle-measurable, because a URI is detected under
the reference's POSIX format too.

`OPENJD_MODEL_VERSION` in the `Makefile` pins the reference. It is pinned
deliberately: unpinned, an upstream release could turn this red with no sqi
commit behind it, and a divergence report means little without knowing which
build produced it.

**A skip proves nothing.** The target exits 0 when no interpreter has the
package importable, and the test skips — exactly as `make test-isolation` does
without Docker. Confirm it actually ran by looking for
`--- PASS: TestExprOracle_MatchesReferenceImplementation`. CI (`expr-oracle`)
asserts that line by name for this reason.

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
│   ├── worker/            sqi-worker binary internals (executor, lease, heartbeat, capabilities, …); only worker/protocol is shared with the server
│   └── ws/                WebSocket hub, subscription management, scheduler-driven fanout
├── pkg/                   Public Go API (currently empty; see pkg/doc.go)
├── api/                   Source-of-truth specs: OpenAPI 3.1, JSON schemas
├── web/                   Frontend source; web/dist is embedded
├── config/                Example config files
├── deploy/                Docker and infrastructure manifests
├── docs/                  Human-readable documentation
├── scripts/               Utility scripts (SBOM, signing, etc.)
└── test/                  Integration test harness
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
// SPDX-License-Identifier: AGPL-3.0-or-later
```

**Conventional Commits.** The `commit-msg` hook enforces the format:
`type(scope)?: description`. Valid types: `feat fix docs style refactor test chore build ci perf revert`.

**Changelog.** `CHANGELOG.md` is a generated artifact — do not edit it by hand.
[git-cliff](https://git-cliff.org) derives it from the Conventional Commit
history (config in `cliff.toml`), so the way to change an entry is to change the
commit message. `feat`/`fix`/`perf`/`refactor`/`build`/`ci`/`docs`/`test`
commits become changelog entries grouped by type; `chore` and `release` commits
are omitted. A commit that doesn't parse as a Conventional Commit is skipped
entirely, so it never reaches the changelog. Regenerate with:

```sh
make changelog                 # refresh, leaving pending commits under [Unreleased]
make changelog VERSION=0.3.0   # roll pending commits into a dated 0.3.0 section
```

Install git-cliff first (`brew install git-cliff`, or see
<https://git-cliff.org/docs/installation>). The release workflow also runs
git-cliff automatically at tag time and bundles the result into the release
archives — but that copy is not committed back, so the tracked `CHANGELOG.md` is
refreshed by hand during release prep (see `docs/release-runbook.md`).

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

// SPDX-License-Identifier: AGPL-3.0-or-later

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

> **Not every new store method is REST-triggered.** The auto-retry +
> failure-limit feature added `RecordTaskFailure`, `RequeueTaskForRetry`
> (`internal/store/sqlite/task.go`), and `ParkJob`
> (`internal/store/sqlite/job.go`) purely for the scheduler's own internal
> use (`internal/scheduler/failure.go`'s `handleTaskFailed`) — no handler in
> `internal/api/` calls them directly. They still follow the same shape as
> REST-triggered methods: declared on the relevant sub-interface
> (`TaskStore`/`JobStore`), implemented in `internal/store/sqlite/`, and
> stubbed in `internal/store/fake/` so scheduler tests can inject the fake.
> Layered policy resolution that isn't itself a store method — e.g. picking
> the effective retry policy from Job → Queue → Farm → server default — is a
> plain package-level helper next to its caller rather than a store method:
> see `resolveRetryPolicy` in `internal/scheduler/retrypolicy.go`. Follow this
> pattern for scheduler-internal state changes that don't need a REST
> surface of their own.

> **Every terminal non-success must leave a reason.** `task_attempts.message`
> is the per-attempt reason (next to `exit_code`); `tasks.failure_reason`
> denormalizes the latest terminal reason onto the task (mirroring
> `unschedulable_reason`), cleared on retry. Two `TaskStore` methods write it:
> `SetTaskFailureReason(ctx, id, reason)` (unconditional) and
> `SetTaskFailureReasonIfEmpty(ctx, id, reason)` (a no-op, not an error, when
> the task already carries a reason). `FailureReasonSummary(ctx, jobID)`
> aggregates a job's failed tasks by reason (`FailedCount`, `DominantReason`,
> `DistinctReasons`) for the job-detail failure banner. **The rule for new
> code:** any new code path that drives a task to a terminal `failed` or
> `canceled` must call one of the two setters — reach for
> `SetTaskFailureReasonIfEmpty` whenever a more-specific reason may already be
> set by another path (the pattern cascade-cancel and user-cancel use so
> cascade always wins regardless of ordering), and the unconditional
> `SetTaskFailureReason` only when your path is authoritative. See
> [the durable-failure-reason table](architecture.md#5-status-ingestion) for
> every existing path and its reason string.

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

## Adding a product

Products are thin catalog entries that wrap an OpenJD template. See
[`docs/products.md`](products.md) for the full concept and REST surface.

### Built-in products

Built-ins live in `internal/product/builtins/*.yaml` and are compiled into the
binary via `//go:embed` in `internal/product/builtins.go`. At process init,
`loadBuiltins` reads every `.yaml` file in that directory, calls
`ParseDefinition` on each (which re-serializes the inline template and validates
it via `openjd.Parse` + `openjd.ValidateWithOptions`), and panics if any file is
malformed. To add a new built-in:

1. Create `internal/product/builtins/<name>.yaml` using the definition file
   format — metadata fields (`name`, `title`, `description`, `category`,
   `version`) plus an inline `template:` key containing a full
   `jobtemplate-2023-09` OpenJD template.
2. Stamp the file with the SPDX comment header:
   ```
   # SPDX-License-Identifier: AGPL-3.0-or-later
   ```
3. Run `go test ./internal/product/...` — the `TestBuiltins_LoadValidateAndStamp`
   test validates every built-in definition and will catch template errors.
4. Built-in names are reserved; `POST /api/v1/products` rejects names that
   shadow a built-in.
5. New default/built-in product or reference preset requiring a software
   capability tag → add a matching built-in detector in
   `internal/worker/capabilities/builtins/`; the
   `TestBuiltinDetectors_CoverPresets` invariant must stay green (see
   [Adding a built-in DCC detector](#adding-a-built-in-dcc-detector) below).

### Custom products

Custom products are created at runtime through the REST API and stored in
SQLite (the `products` table). They are mutable (PUT/DELETE) and appear in `GET
/api/v1/products` merged with the built-ins.

```sh
curl -X POST http://localhost:8080/api/v1/products \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "my-render",
    "title": "My Renderer",
    "version": "1.0.0",
    "template": "specificationVersion: jobtemplate-2023-09\nname: MyRender\nsteps:\n  - name: Run\n    script:\n      actions:\n        onRun:\n          command: render\n",
    "format": "yaml"
  }'
```

The server validates the template (via `product.ValidateTemplate`) before
writing; a malformed template returns `400 Bad Request`.

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
2. **goimports** — organizes imports (stdlib / external / internal groups).
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

The server exposes NATS on `0.0.0.0:4222` (all interfaces) by default. In a second terminal,
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

### Running multiple workers locally

To simulate a multi-worker farm on one machine — for exercising the scheduler,
capability matching, or queue routing — use the `run-workers` target. With
`sqi-server` running (`make run` in another terminal), start N workers:

```sh
make run-workers          # 3 workers (the default)
make run-workers N=5      # 5 workers
```

Each instance gets a distinct identity so they don't collide: a unique name
(`worker-1`, `worker-2`, …), its own data dir under `./.run/workers/worker-<i>`
(holding a separate `worker.id` UUID), and its own metrics port (`9091`,
`9092`, …). Ctrl-C stops them all. Inherited `SQI_WORKER_*` env vars still
apply, so you can layer on shared config — e.g. point them at an explicit NATS
URL instead of mDNS discovery:

```sh
SQI_WORKER_NATS_URL=nats://127.0.0.1:4222 make run-workers N=5
```

Tunable Makefile variables: `N` (or `WORKERS`), `WORKER_METRICS_BASE_PORT`,
and `WORKER_DATA_ROOT`. See
[Running multiple workers on one host](worker-configuration.md#running-multiple-workers-on-one-host)
for the underlying settings and a manual (non-Make) equivalent.

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

### Adding a built-in DCC detector

This is a separate, declarative detection path from the `Probe`-based
hardware tags above — see
[`docs/worker-capabilities.md`](worker-capabilities.md#capability-auto-detection-built-in-dcc-detectors)
for the full picture (how detection runs, the tag/version model, precedence)
and the
[Writing custom detectors](worker-capabilities.md#writing-custom-detectors)
reference for the detector schema.

**Paired-change convention:** every new default/built-in product or reference
preset that requires a software capability tag ships a matching built-in
detector in `internal/worker/capabilities/builtins/`. For `presets/sqi/*.yaml`
specifically, this is test-enforced: `TestBuiltinDetectors_CoverPresets` in
[`internal/worker/capabilities/builtins_test.go`](https://github.com/uberware/sqi/blob/main/internal/worker/capabilities/builtins_test.go)
cross-checks every `attr.worker.tag.<x>` required by `presets/sqi/*.yaml`
against the tags emitted by the built-in detectors, so a new reference preset
cannot ship without a matching detector or the test fails. (The test currently
only scans `presets/sqi/`, not `internal/product/builtins/`; apply the same
convention by hand to a new built-in *product* until that coverage is
extended.)

**Steps to add a new built-in detector:**

1. Create `internal/worker/capabilities/builtins/<name>.yaml` with the
   detector schema — `tag`, `checks` (each check exactly one of
   `exe`/`path_glob`/`env`/`registry`, with an optional per-check `os` gate),
   and an optional `version.from` regex. Use the existing built-ins
   (`maya.yaml`, `nuke.yaml`, `houdini.yaml`, `blender.yaml`) as reference —
   they use the identical schema as a custom detector.
2. Stamp the file with the SPDX comment header:
   ```
   # SPDX-License-Identifier: AGPL-3.0-or-later
   ```
3. Run `go test ./internal/worker/capabilities/...` —
   `TestBuiltinDetectors_CoverPresets` and the built-in load/validate tests
   will catch a malformed detector or a still-uncovered preset tag.
4. `make fmt && make lint`.

## sqi-sdk (Python) development

The Python client lives at `clients/python/` (import name `sqi_client`). It is a
separate toolchain from the Go code — it has its own virtualenv, dependencies,
and Make targets — so its commands do not overlap with the Go ones above.

### First-time setup

```sh
# Create clients/python/.venv and install sqi-sdk editable with all extras
# (yaml, ws, dev — ruff, mypy, pytest, pytest-cov, pytest-timeout, respx,
# websockets):
make py-install
```

### Day-to-day commands

```sh
make py-fmt              # format with ruff
make py-lint             # ruff lint
make py-typecheck        # mypy (src targets 3.9, tests target 3.13)
make py-test             # unit tests with coverage gate
make py-check            # full check-only gate: ruff format --check, ruff check, mypy, pytest
make py-build            # build sdist + wheel into clients/python/dist/
```

`make py-check` is the pre-PR gate; it matches what CI runs for the client. Run
it (not bare `pytest`) from the repo root, or `cd clients/python` first — pytest
must load `clients/python/pyproject.toml` for the markers, coverage gate, and
`addopts` to apply.

### Integration tests against locally-built binaries

The `@pytest.mark.integration` suite boots the real `sqi-server` and `sqi-worker`
binaries as subprocesses (skipped by default; coverage off because the subset
doesn't meet the unit gate):

```sh
# Builds the binaries first, then runs pytest -m integration:
make py-test-integration

# Or, if the binaries are already current, run directly:
cd clients/python && .venv/bin/pytest -m integration --no-cov
```

The harness finds the binaries at `<repo-root>/bin/sqi-server` and
`bin/sqi-worker` by default; override with `SQI_SERVER_BIN` / `SQI_WORKER_BIN`.
If a binary is missing the whole marker is skipped with a clear message.

### Adding a new endpoint wrapper

To wrap a new server endpoint in the client, mirror the existing layering:

1. **Transport** — reach for the shared helpers in `client.py` rather than
   calling `httpx` directly: `_request` / `_request_json` (path joining under
   `/api/v1`, `None`-param dropping, typed-error mapping, GET-only retry),
   `parse_page` for paginated responses, and the generic `_CrudResource` for
   standard create/get/update/delete resources.
2. **Model** — add or extend a frozen dataclass in `models.py` matching the
   OpenAPI component schema field-for-field, with a tolerant `from_dict`. Export
   it from `__init__.py`'s `__all__`.
3. **Method** — add the public method to `SqiClient` (keyword-only options,
   fully type-annotated, Google-style docstring). Accept status filters as either
   the enum or its wire string via `_enum_value`.
4. **Tests** — add `respx`-mocked unit tests in `tests/` (one file per resource
   group), and extend the integration suite if it exercises a real execution
   path.
5. **Docs** — document the method in
   [`docs/python-client.md`](python-client.md) with a short runnable example.

Run `make py-check` before opening the PR. The authoritative request/response
shape is always the OpenAPI spec (`internal/api/openapi.yaml`); when it and the
server diverge, treat the spec as authoritative and flag the discrepancy.

## The sqi-submitter package

`clients/submitter/` (import name `sqi_submitter`, distribution `sqi-submitter`)
is a second, separate Python package depending on `sqi-sdk` — its own
virtualenv, `pyproject.toml`, and gate, independent of both the Go build and
`clients/python`. Full user-facing reference: [`docs/dcc-submitters.md`](dcc-submitters.md).

```
clients/submitter/
  pyproject.toml            sqi-submitter; requires-python >=3.9; deps: sqi-sdk
  src/sqi_submitter/
    core/                   no Qt, no DCC imports — session, form model, pre-fill, HostAdapter
    qt/                      Qt only, imported lazily (PySide6, falls back to PySide2)
    hosts/{maya,houdini,nuke,blender}/   adapter + launch glue per host
  tests/                     pytest; tests/integration/ is env-gated (see below)
presets/sqi/                 the fourteen reference preset YAML fixtures (nine DCC render
                              presets, five ffmpeg transcode presets), validated by
                              internal/product/sqipresets_test.go against a per-preset
                              `want` map of expected category/params/extensions —
                              not a blanket rule, since not every preset is
                              `Rendering` or declares `TASK_CHUNKING`
presets/testing/             the four test/QA preset fixtures (test-render, test-steps;
                              bash + PowerShell), validated by
                              internal/product/testingpresets_test.go
```

Gate command line (from `clients/submitter`, with `sqi-sdk` resolvable —
`pip install -e ../python` first if not already installed):

```sh
ruff format --check . && ruff check . && mypy src && pytest -q
```

`tests/integration/` is skipped unless `SQI_TEST_SERVER_URL` points at a live
`sqi-server`; the Blender render smoke inside it additionally requires
`SQI_TEST_BLENDER=1` and a local Blender install.

Adding a DCC adapter (a fifth Python-based host) is a `HostAdapter` subclass
plus launch glue — walked through in
[`docs/dcc-submitters.md` §6, Writing a new adapter](dcc-submitters.md#writing-a-new-adapter).
