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
| `golangci-lint` ≥ 2.13.0 (see the version floor below) | Linter suite | [golangci-lint.run/usage/install](https://golangci-lint.run/usage/install/) |
| `lefthook` | Git hook runner | `go install github.com/evilmartians/lefthook@latest` |
| `pkgsite` | Local pkg.go.dev docs server | `go install golang.org/x/pkgsite/cmd/pkgsite@latest` |
| `ffmpeg` (with `ffprobe`) | Required by `make test`, `make test-cover`, `make ci` and `make test-preset-harness`: the preset validation registry's `tier2` blocks require the real-ffmpeg cases to run, and a skip on a required platform is a failure | `apt-get install ffmpeg` / `choco install ffmpeg` / `brew install ffmpeg` |
| Python 3 (`python3` or `python` on `PATH`) | Required by the same four targets for the `python-sequence` Tier-2 case. Some `internal/openjd/expr` path tests compare against `python3`'s `pathlib` and skip without it. `make test-expr-oracle`, `make docs-site-install` and the `py-*` targets build their virtualenvs from it | [python.org](https://www.python.org/downloads/) |
| Docker (optional) | Build and run the container image. Also runs the real-directory LDAP tests (`make test-ldap`), the real-provider SSO tests (`make test-oidc`), the real-root run-as-user isolation tests (`make test-isolation`) and the PostgreSQL tests (`make test-postgres`), all of which skip without it | [docs.docker.com](https://docs.docker.com/get-docker/), or `brew install colima docker && colima start` |

`gofumpt`, `goimports`, and `golangci-lint` are required at commit time via
pre-commit hooks. Install them before running `make hooks`.

**`golangci-lint` 2.13.0 is the minimum.** `.golangci.yml` excludes
`errors.AsType` from `errcheck` by function name. Before 2.13.0, `errcheck`
cannot resolve a generic function's name, so the exclusion never matches and
`make lint` reports every `errors.AsType` call site as `Error return value is
not checked`.

CI pins v2.13.1 (`.github/workflows/ci.yml`) so a new golangci-lint release
cannot change its result. 2.13.0 also lints this repo clean, so the local floor
sits one patch below the pin. Raise both together.

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
| `make build-worker` | Build `sqi-worker` only |
| `make build-web` | Build the web UI bundle into `web/dist/` (`npm ci` runs only when npm manifests change) |
| `make build-all` | Cross-compile both binaries for linux/darwin/windows × amd64/arm64 |
| `make run` | Build then run `sqi-server` with default config (`make run-server` is the same) |
| `make run-worker` | Build then run a single `sqi-worker` with default config |
| `make run-workers` | Build then run N `sqi-worker` instances locally (`N=3` default); Ctrl-C stops all |
| `make test` | Run all tests with the race detector enabled |
| `make test-cover` | Run tests and print coverage; fails below 70% (the `COVERAGE_MIN` default in the Makefile) |
| `make test-cover-html` | Open an HTML coverage report in the browser |
| `make test-integration` | Run integration tests (build tag `integration`) |
| `make test-dcc` | Run the preset harness with Blender, Nuke and Houdini **required**, against the real applications (`DCC_APPS=blender,nuke,houdini` by default; narrow it to the ones you have). A missing application **fails** instead of skipping. See [Running the preset validation harness](#running-the-preset-validation-harness) |
| `make test-ldap` | Run the LDAP tests against a real OpenLDAP directory in a container (needs Docker; **skips** without it) |
| `make test-oidc` | Run the SSO tests against a real Keycloak in a container (needs Docker; **skips** without it) |
| `make test-postgres` | Run the PostgreSQL-backed tests against a throwaway PostgreSQL in a container (`PG_VERSION=16` or `18`, default 18; needs Docker; **skips** without it). See [Testing against PostgreSQL](#testing-against-postgresql) |
| `make test-isolation` | Run run-as-user task-isolation tests as real root against real OS accounts in a container (needs Docker; **skips** without it) |
| `make test-isolation-windows` | Run the Windows run-as-user isolation tests against real local accounts, tier 1 as the elevated administrator and tier 2 as SYSTEM. Needs an **elevated** shell on a real Windows host (no container); exits 0 with a message when not elevated |
| `make test-service-windows` | Drive `sqi-server` and `sqi-worker` through the real Windows service manager. It installs, starts, stops and deletes real `sqi-test-*` services and creates and deletes a throwaway `sqisvc-*` local account. Needs an **elevated** shell on a real Windows host (no container); exits 0 having done nothing when not elevated |
| `make test-discovery` | Run the mDNS discovery tests over real multicast (no container; **fails rather than skips** when multicast is unavailable) |
| `make test-conformance` | Run the official OpenJD conformance suite against the vendored `third_party/` fixtures (build tag `conformance`) |
| `make test-expr-oracle` | Differential-test the EXPR evaluator against the OpenJD reference implementation (needs `python3`; **skips** without it) |
| `make test-preset-harness` | Run the whole preset validation harness (Tiers 1 to 3 and the registry check) in one `go test` process. See [Running the preset validation harness](#running-the-preset-validation-harness) |
| `make test-preset-library` | Validate the published preset library against the validator in your tree (needs network; **skips** when the library is unreachable, **fails** when it is reachable but invalid) |
| `make expr-oracle-venv` | Create `.venv-oracle/` with the pinned reference implementation (`make test-expr-oracle` does this on demand) |
| `make smoke` | End-to-end smoke test against the real binaries (REST + WebSocket) |
| `make auth-demo` | Run the auth-surface demo against a live local farm (`KEEP=1` leaves it running) |
| `make bench` | Run benchmarks |
| `make lint` | Run `golangci-lint` |
| `make lint-fix` | Run `golangci-lint --fix` |
| `make lint-actions` | Lint the GitHub Actions workflows with `actionlint` (via `go run`; no install needed) |
| `make fmt` | Format all Go files with `gofumpt` and `goimports` |
| `make fmt-check` | Check formatting without modifying files (the `make ci` gate) |
| `make vet` | Run `go vet` over the filtered package list (excludes `web/node_modules/`) |
| `make generate` | Run `go generate` across the module |
| `make docs` | Serve Go package docs at `localhost:8080` via `pkgsite` |
| `make docs-site-install` | Create `.venv-docs/` and install the pinned MkDocs dependencies |
| `make docs-site` | Build the MkDocs documentation site with `--strict` (the CI gate) |
| `make docs-site-serve` | Serve the documentation site locally with live reload |
| `make changelog` | Regenerate `CHANGELOG.md` from Conventional Commits via `git-cliff` (`VERSION=x.y.z` tags the pending release) |
| `make hooks` | Install git hooks via `lefthook` |
| `make release` | Build a release with goreleaser |
| `make release-snapshot` | Build a local snapshot release (no publish, no git tag required) |
| `make docker-build` | Build the `sqi-server` Docker image |
| `make docker-run` | Run the `sqi-server` Docker image with default config |
| `make deps` | Download and tidy Go module dependencies |
| `make deps-upgrade` | Upgrade all Go dependencies to the latest minor/patch versions |
| `make clean` | Remove build artifacts and `coverage.out` |
| `make ci` | Run the full CI sequence (fmt-check, vet, lint, test-cover) |

The `py-*` targets for the Python client are listed under
[sqi-sdk (Python) development](#sqi-sdk-python-development).

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
go test -race -run TestPolicyGate_QueueAtCapacity ./internal/scheduler/...

# With verbose output
go test -race -v ./internal/openjd/...

# Integration tests (require the integration build tag)
make test-integration

# LDAP tests against a real directory in a container (needs Docker)
make test-ldap

# SSO tests against a real Keycloak in a container (needs Docker)
make test-oidc

# PostgreSQL-backed tests against a throwaway PostgreSQL 16 or 18 in a
# container (needs Docker)
make test-postgres PG_VERSION=16

# Run-as-user task-isolation tests as real root against real OS accounts in a
# container (needs Docker)
make test-isolation

# Windows service tests against the real service manager (elevated shell on a
# real Windows host; installs and deletes real services)
make test-service-windows

# Fuzz targets (-fuzz takes one target per run; each runs for 30 seconds)
go test -fuzz=FuzzParseYAML         -fuzztime=30s ./internal/openjd/
go test -fuzz=FuzzSubmitJobPayload  -fuzztime=30s ./internal/api/
```

The other fuzz targets are `FuzzParseJSON` (`internal/openjd`),
`FuzzPatchJobPayload` (`internal/api`), `FuzzDecodeAssignMsg`
(`internal/worker/protocol`) and `FuzzHandleLine` (`internal/worker/openjd`).

### Testing against a real directory or identity provider

Two suites in `test/integration/` talk to a real external identity source
rather than a fake: `ldap_test.go` (`make test-ldap`) and `oidc_test.go`
(`make test-oidc`). Both are behind the `integration` build tag, both boot a
throwaway container, and **both skip when Docker is unavailable**, so a green
run proves nothing until you confirm it executed. Check for `--- PASS` lines,
not just a zero exit code; `GOFLAGS=-count=1` stops Go replaying a cached
result.

Most LDAP tests drive a fake connection. That covers sqi's own logic but not
how it uses go-ldap on the wire: a wrong search scope, a misnamed attribute, a
filter a real server rejects. `make test-ldap` boots a throwaway OpenLDAP
container and drives the whole login path against it in every supported bind
mode. CI runs it on every change (the `ldap-integration` job) on both amd64 and
arm64, because the image's variants do not behave identically. Run it before
touching anything under `internal/auth/ldap/`.

To point it at a directory you already have, including a real Active Directory,
set `SQI_TEST_LDAP_URL` and no container is started:

```sh
SQI_TEST_LDAP_URL=ldap://dc01.example.com:389 make test-ldap
```

That directory must already hold the fixture tree; see the `seedLDIF` constant
in `test/integration/ldap_test.go` for the exact users, groups, and passwords.

**Reproducing an architecture-specific failure.** The image's variants differ:
for example, the memberof module registers its schema later on x86 than on
arm64, so a fixture can fall inside that window on one architecture only. CI
reports which architecture broke; this reproduces it locally:

```sh
SQI_TEST_LDAP_PLATFORM=linux/amd64 make test-ldap
```

It runs under emulation, so expect it to be several times slower than native.

The fixture directory needs two properties:

- **`memberOf` must be populated.** AD does this natively; OpenLDAP needs the
  `memberof` overlay, and the overlay's `olcMemberOfGroupOC` must match the
  objectClass your groups actually use. Get it wrong and every user
  authenticates successfully and silently lands on `default_role`.
- **The service account must be able to read other entries.** Template bind
  works without this (sqi binds *as* the user and reads that user's own entry),
  so a directory that fails only in search mode is the usual symptom.

The fixture checks both up front, as the service account, and fails naming the
cause instead of letting it surface later as wrong roles.

### Testing SSO against a real identity provider

The SSO unit tests drive a fake provider that signs real tokens, so
token-validation mistakes surface. A fake cannot show what a real provider
omits: Keycloak emits no group membership unless a protocol mapper is
configured, and a token with no groups still validates, so every user silently
lands on `default_role`. `make test-oidc` catches that class of bug.

```sh
make test-oidc
```

It boots a throwaway Keycloak container, seeds a realm, and drives the whole
browser-shaped flow: the authorization-code round trip, group → role mapping, a
rename at the provider keeping the same account, state-mismatch refusal,
`prompt=login` forcing re-authentication, and the RP-initiated logout behavior
documented in [`docs/auth.md`](auth.md#provider-logout-is-weaker-than-it-looks).
CI runs it on every change (the `oidc-integration` job) on one architecture,
since none of the HTTP or token behavior it asserts on depends on architecture.

It skips when Docker is unavailable, and a skip verifies nothing. Run it, and
confirm it ran, before touching anything under `internal/auth/oidc/` or the SSO
routes in `internal/api/`: use `GOFLAGS=-count=1 make test-oidc` so Go cannot
replay a cached result, and check for `--- PASS` on each `TestOIDC_` test.

To point it at a realm you already have instead of starting a container, set
`SQI_TEST_OIDC_ISSUER`:

```sh
SQI_TEST_OIDC_ISSUER=https://keycloak.example.com/realms/farm make test-oidc
```

That realm must already hold the fixture's client, users, groups, and the
`groups` protocol mapper; see the seeding code in
`test/integration/oidc_test.go` for exactly what. Any test that **mutates** the
fixture through the admin API (the rename-at-the-provider case) skips under this
variable rather than edit your realm, so an external issuer covers less than
the container does.

Use `https://` for a remote issuer. Keycloak always marks its session cookies
`Secure`. Go's cookie jar exempts loopback from that rule, so the container
fixture works over plain HTTP on `127.0.0.1`, but a non-loopback issuer over
`http://` would hide the provider's session from the test client.

The Keycloak image tag is pinned because the tests scrape Keycloak's login and
logout-confirmation markup. It appears in two places that must match:
`keycloakImage` in `test/integration/oidc_test.go` and the `docker pull` in
`.github/workflows/ci.yml`.

### Testing the Windows service against the real service manager

The unit tests in `internal/winsvc` drive the `service` commands through a fake
SCM. That cannot show what the real Service Control Manager, the LSA and NTFS do
with a registration, so `test/winservice` builds both binaries and installs,
starts, stops and removes real services with them:

```powershell
make test-service-windows
```

It must run from an **elevated** shell on a real Windows host, and it is not
sandboxed: it **really installs, starts, stops and deletes services** named
`sqi-test-*`, and it creates and deletes a throwaway local account
(`sqisvc-*`) and its profile. The profile outlives the run: Windows loads it to
start the `--user` service and keeps it loaded until reboot (the User Profile
Service holds its hives open after the service is stopped and deleted), so
`UserAccount` notes it and leaves it, and the first run after a reboot deletes
every such profile whose account is gone. It builds the binaries into
`%ProgramData%\sqi winservice bin NNN` and works in `%ProgramData%\sqi winservice NNN`
directories, and it writes logs to `%ProgramData%\sqi\logs\sqi-test-*.log`. Six
tests, `TestWinService_ServerLifecycle`, `InstallRefusesExisting`,
`WorkerLifecycle`, `StartupFailureLeavesTrace`, `HandRegisteredNewService` and
`UserAccount`, cover the install, start, status, stop and uninstall round trip
for both binaries, the refusal of an existing name, a service that fails at
startup and leaves a trace line, a hand-registered service, and `--user`.

Not elevated, it prints `winservice: not elevated — the service suite needs an
elevated (Administrator) shell` and **exits 0 without doing anything**. A skip
verifies nothing, so confirm all six `--- PASS: TestWinService_` lines (the
target passes `-count=1`, so a cached result cannot stand in). CI runs it as the
`winservice-integration` job on an elevated `windows-latest` runner, on one
architecture, and fails the job on that message or when any of the six does not
pass.

Two things make a local run fail for reasons outside sqi:

- **Machine-level `SQI_*` environment variables** reach the services the suite
  installs, because a service sees the machine environment. Unset them first.
- **`services.msc` holding a handle** on a service makes the check that it is
  gone after `uninstall` fail: a service with an open handle is only *marked for
  deletion*. Close it.

**Cleaning up after a killed run.** Every test registers its cleanup, but a
killed run (or Ctrl-C) skips it. Unique names mean a leftover never breaks the
next run, but it stays on the machine until you remove it from an elevated
PowerShell:

```powershell
# Leftover services: list them, then stop (if running) and delete each
sc.exe query state= all | findstr sqi-test
$name = 'sqi-test-server-xxxxxxxx'   # a name from the list above
sc.exe stop $name
sc.exe delete $name

# A leftover account, in this order: undo what the --user install did to the
# machine while the account can still be looked up, then delete it
icacls "$env:ProgramData\sqi\logs" /remove:g sqisvc-xxxxxxxx
# secpol.msc -> Local Policies -> User Rights Assignment -> Log on as a service:
#   remove sqisvc-xxxxxxxx
net user sqisvc-xxxxxxxx /delete
# Its profile cannot be deleted until a reboot; the next suite run removes it.

# Leftover files
Remove-Item -Recurse -Force "$env:ProgramData\sqi winservice*"
Remove-Item "$env:ProgramData\sqi\logs\sqi-test-*"
```

The suite also creates `%ProgramData%\sqi` and `%ProgramData%\sqi\logs` when they
are missing, as the installer does, and never removes them; `logs` keeps the
protected permissions the installer gives it. To return the host to how it was,
delete them by hand once you have checked that nothing else uses them.

### Testing mDNS discovery over real multicast

`make test-discovery` runs the discovery suite over a real mDNS round trip: a
real responder advertises on a real interface and a real browser finds it. It
needs no container.

It covers what unit tests of each half cannot: the server derives its TXT
records from its config, they cross the wire, and the worker parses them and
acts on them, so a record one side advertises and the other never reads (such
as `nats_tls`) fails here. In `TestDiscovery_RealBinaryFindsItsServerOverMDNS`
a real `sqi-worker` subprocess with no `nats.url` has to find its server, learn
from the advertisement that the broker needs TLS, and register.

#### It does not advertise on your network

Every advertisement these tests make is restricted to loopback. A browser
listening on all interfaces still receives it, so coverage is unaffected and a
test run never announces a service on your LAN. Where loopback cannot carry
multicast the tests refuse to run rather than fall back to a real interface.

Linux loopback needs two settings that macOS `lo0` has by default, and the tests
name each one that is missing:

```bash
sudo ip link set lo multicast on      # Linux `lo` ships without the flag
sudo ip -6 addr add fe80::1/64 dev lo # and with no non-loopback address
```

zeroconf builds the advertisement's address records from the advertising
interface's addresses and discards loopback ones. Linux `lo` carries only
`127.0.0.1` and `::1`, so without the second command registration fails with
"Could not determine host IP addresses" even with the multicast flag set. macOS
`lo0` already carries `fe80::1`. The link-local address is scoped to the link
and not routed. The `discovery-integration` CI job runs both commands.

#### The one test that opens a listener

`TestDiscovery_RealBinaryFindsItsServerOverMDNS` binds the test broker to all
interfaces for about ten seconds, so it runs only under `make test-discovery`;
`make test-integration` skips it. The advertisement carries this machine's
hostname, so the worker dials that name whatever interface the announcement
went out on, and a loopback-bound broker would be unreachable. Broker
authentication and TLS are both on, so enrolling still requires a join token.

#### Other farms and skips

- **It refuses to run next to a real farm.** The tests advertise only on
  loopback but browse on every interface, as the production worker does. If
  anything else already advertises `_sqi._tcp` they skip rather than risk
  discovering another server, because a test cannot tell another broker from
  its own before connecting to it.
- **A skip is a failure here.** `SQI_TEST_REQUIRE_MULTICAST=1`, which the target
  sets, turns the capability skip into a failure, so the target cannot pass
  while running nothing. The same tests run under `make test-integration`,
  where they skip on a host without loopback multicast.

---

### Testing against PostgreSQL

Plain `make test` runs every store test against SQLite and the in-memory fake,
and needs no Docker. The PostgreSQL store is tested by `make test-postgres`:

```sh
make test-postgres                 # PostgreSQL 18 (the default)
make test-postgres PG_VERSION=16   # PostgreSQL 16, the oldest supported
```

`scripts/test-postgres.sh` starts a throwaway PostgreSQL container (the
pinned image tag for `PG_VERSION`, data on tmpfs), points
`SQI_TEST_POSTGRES_DSN` at it, sets `SQI_TEST_REQUIRE_POSTGRES=1`, and runs
three passes:

1. the store, scheduler, API, server, restore and `cmd` packages, with the race
   detector (`RACE=off` drops it);
2. the store packages again with `SQI_TEST_PG_FORCE_TX_RETRY=1`, which runs
   every transaction once, rolls it back and runs it again. The store retries
   aborted transactions, so every body must be safe to re-run, and one that
   keeps state from a failed attempt fails a test here. It prints
   `== BEGIN FORCED-RETRY PHASE` and `== END FORCED-RETRY PHASE` around this
   pass, and CI checks between them that its `/postgres` subtests passed and
   none was skipped;
3. the integration suite (`-tags integration`, `./test/integration/`) with the
   server on PostgreSQL (`SQI_TEST_STORE_DRIVER=postgres`), skipping the LDAP,
   OIDC and isolation tests, which have targets of their own.

It ends with `POSTGRES TESTS PASSED (PG_VERSION=…)`. The variables it uses can
also be set by hand:

| Variable | Effect |
|---|---|
| `SQI_TEST_POSTGRES_DSN` | An existing server to test against instead of a container (a `postgres://` URL for a role that can create databases). Each test gets a fresh database, cloned from a migrated template and dropped when the test ends. |
| `SQI_TEST_REQUIRE_POSTGRES=1` | A PostgreSQL test **fails** instead of skipping when no DSN is set. |
| `SQI_TEST_PG_FORCE_TX_RETRY=1` | Rehearse every transaction once (the second pass above). The forced-interleaving race tests skip under it. |
| `SQI_TEST_STORE_DRIVER=postgres` | Run the `test/integration` harness's server on a fresh PostgreSQL database instead of SQLite; any value but `sqlite` or `postgres` fails. |
| `SQI_TEST_POSTGRES_IMAGE` | Override the container image (CI passes the image it pulled). |

**Without Docker the target prints `docker unavailable — skipping PostgreSQL
tests` and exits 0, and without `SQI_TEST_POSTGRES_DSN` every PostgreSQL test
skips. A skip verifies nothing.** Confirm the run with `--- PASS` lines for
`/postgres` subtests and the final `POSTGRES TESTS PASSED` line. CI's
`postgres-integration` job runs the target on PostgreSQL 16 and 18. It fails
when Docker is unavailable; when any of its named tests does not pass (the
`TestPGRace_G1_…` to `TestPGRace_G12_…` race tests, the schema parity,
atomic-submission, readiness and backup round-trip tests, the PostgreSQL
stress test, the EXPR end-to-end job, and the restore, store copy and upgrade
tests); when any `/postgres` subtest is skipped; and when none passed, over the
whole run or within the forced-retry pass. The 18 leg also runs `make smoke`
with the server on PostgreSQL.

A local `PG_VERSION=16` run needs a PostgreSQL 16 `pg_dump` and `pg_restore`
first on `PATH`. The backup round trip restores a dump into the test server,
and a newer client's dump carries settings (such as `SET transaction_timeout`)
that a PostgreSQL 16 server rejects. CI installs the matching client tools for
each leg.

**Every cross-backend test runs three ways.** The store packages' `newStores`,
the scheduler's `raceBackends` and the API's `seedBackends` return the fake
and SQLite always, plus PostgreSQL when `SQI_TEST_POSTGRES_DSN` is set (or
`SQI_TEST_REQUIRE_POSTGRES=1`), keyed by subtest name (`.../fake`,
`.../sqlite`, `.../postgres`). A test written over one of them needs no change
to run on PostgreSQL. Both SQL stores refuse a transaction that locks anchor
rows out of the [lock order](architecture.md#lock-order), so an inversion fails
the test on SQLite as well.

**Concurrency tests force their interleaving.** A race that SQLite's single
writer cannot produce is tested on PostgreSQL with the pause hook in
`internal/store/sqlstore/pause_test.go`. `pgRace(t)` opens a PostgreSQL store
whose dialect parks the first transaction that reaches an armed point:
`p.atStatement(sql)` arms the first dynamic statement whose text contains
`sql` (a prepared statement was rebound when it was prepared and never fires),
and `p.atAnchor(table, id)` arms the lock of one anchor row, before the lock
is taken. `interleave(t, p, paused, competitor)` runs `paused` until it parks,
runs `competitor` until it either finishes or shows in `pg_stat_activity` as
waiting on a lock, then resumes `paused`, and reports both errors and whether
`competitor` had to wait. The race tests are `TestPGRace_*` in
`internal/store/sqlstore/pgrace_test.go`; CI asserts the
`TestPGRace_G<n>_…` ones by name. To add one:

1. Write the test first and run it against the code **without** the fix: it
   must fail on every run, for the reason it names. Also assert on
   `p.matched()`, so the test checks that it parked at the statement it meant,
   not at another that contains the armed text.
2. Make the fix, and run it green.

The hook runs on PostgreSQL only, so these tests skip without a DSN, and
under `SQI_TEST_PG_FORCE_TX_RETRY=1`, where the pause would fire in the
rehearsal that is rolled back.

### Differential-testing EXPR against the reference implementation

`make test-expr-oracle` evaluates a corpus of expressions with **both** sqi's Go
evaluator (`internal/openjd/expr`) and the implementation the EXPR spec names as
its reference, then compares the results.

It catches a misreading of the spec applied consistently, which sqi's own tests
cannot see because they encode the same misreading. It is not a conformance
check (`make test-conformance` runs the official fixture suite); it helps
explain why a fixture disagrees, and it reaches expressions no fixture
contains.

```sh
make test-expr-oracle        # (re)installs .venv-oracle/ when missing or off-pin, then compares
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

**The oracle grades the same on every host** because of two pins.
`scripts/expr-oracle.py` reads and writes UTF-8 explicitly; otherwise Python
uses the locale encoding, which on Windows is the ANSI code page, and the
reference receives every non-ASCII corpus case mangled. It also passes
`path_format=PathFormat.POSIX` instead of the specification's host-native
default, because sqi's `expr.WithPathFormat` defaults to POSIX and every path
case would otherwise diverge on a Windows host. Both are no-ops on Linux and
macOS. The Makefile resolves the venv's interpreter per host (`bin/python3` or
`Scripts/python.exe`).

**The reference is not the authority.** Although it lives in the `openjd.expr`
Python namespace, it is a thin re-export layer over a compiled Rust crate
(`openjd-expr`, in `OpenJobDescription/openjd-rs`), and that crate is Beta: on
the 0.x line, breaking changes are permitted in minor releases. The
specification outranks it. When the two disagree, read
`third_party/openjd-specifications/` and decide; do not change sqi to match the
reference. In most baselined entries the reference is the one that is wrong.
`test/oracle/baseline.txt` gives the reasoning for each ruling, and divergences
in the reference are reported upstream.

**The oracle does not cover the Windows path flavor.** Every case is evaluated
with `path_format=PathFormat.POSIX`, so `test/oracle/corpus.txt` has no
Windows-flavor case, and the Windows expectations in `internal/openjd/expr` are
pinned by that package's own unit tests against CPython's `PureWindowsPath`.
URI behavior is covered, because a URI is detected under the POSIX format too.

`OPENJD_MODEL_VERSION` in the `Makefile` pins the reference, so an upstream
release cannot change the result without an sqi commit and every divergence is
tied to a known build. The Makefile passes the pin to the test as
`SQI_EXPR_ORACLE_EXPECT_VERSION`, and the test fails when the reference that
answered is a different version. With `SQI_EXPR_ORACLE_PYTHON` set, no version
is expected.

**A skip proves nothing.** The target exits 0 when no interpreter has the
package importable, and the test skips, as `make test-isolation` does without
Docker. Confirm it ran by looking for
`--- PASS: TestExprOracle_MatchesReferenceImplementation`. CI (`expr-oracle`)
asserts that line by name.

---

### Running the preset validation harness

Every shipped preset and built-in product has a Tier-1 argv golden (a reviewed
snapshot of the exact command line the template resolves to) and a Tier-3 case
that runs the real submit → assign → resolve pipeline against a recording stub
in place of the vendor executable. The five ffmpeg presets also have a Tier-2
case that runs real ffmpeg end to end, `python-sequence` has one that runs
real Python, and the Blender, Nuke and Houdini presets have ones that render
with the real application. See [`docs/preset-library.md`](preset-library.md#validation-tiers)
for what each tier does and does not prove.

```sh
# Run the whole harness: Tier 1 (argv goldens), Tier 2 (real ffmpeg and
# Python), Tier 3 (stub execution), and the registry check that all three ran.
# They must share one go test invocation: TestZZPresetTierRegistrySatisfied
# reads an in-process outcome sink, so splitting the tiers across invocations
# makes it fail with "never reported" in both.
make test-preset-harness

# Just Tier 1, verbose, for one preset
go test ./test/integration/ -run TestPresetTier1Argv/maya-layer-render -v
```

`make test-preset-harness` needs ffmpeg and Python 3 on `PATH`: the
registry's `tier2` blocks require those cases to run, and a skip on a required
platform is a registry failure. Each registry entry's `required_on` names the
platforms its cases must run on. Most name Linux, macOS and Windows; these name
fewer:

- Linux and macOS: `script`, `command-sequence`, `ffmpeg-segment-transcode-bash`
- Windows: `script-powershell`, `command-sequence-powershell`,
  `ffmpeg-segment-transcode-powershell`, `3dsmax-render`,
  `after-effects-render-windows`, `after-effects-queue-render-windows`
- macOS: `after-effects-render-macos`, `after-effects-queue-render-macos`

A pass on one OS proves nothing about another OS's entries, so CI runs the
whole harness natively on Linux (the `test` job) and on Windows
(`preset-harness-windows`). No CI job runs it on macOS.

The Blender, Nuke and Houdini cases (`TestPresetDCC_*`) are different: they need
the application, so `make test-preset-harness` skips them when it is absent.
`make test-dcc` is the same harness with `SQI_TEST_REQUIRE_APPS` set, which makes
a skip a failure. Each application must be installed where the worker's detector
finds it (or its command on `PATH`) and licensed for command-line use:

- **Blender:** free, no license. CI runs these cases in the `blender-presets` job
  against a pinned Blender.
- **Nuke:** the free Non-commercial license works and needs `--nc` on every
  command-line run, so the tests pass `SQI_TEST_NUKE_ARGS` (default `--nc`) to
  Nuke and to the worker's `nuke` command. Set it empty for a commercial or Indie
  license. The license is activated by signing in once through Foundry.
- **Houdini:** the free Apprentice license works once activated through the
  License Administrator, and `hython` then runs headless.

Use `make test-dcc DCC_APPS=blender` to require only the ones you have installed.

To regenerate goldens after an intentional change to a preset's template or to
expansion/resolution behavior:

```sh
go test ./test/integration/ -run TestPresetTier1Argv -preset-update
```

**Review the diff of a regenerated golden.** `-preset-update` rewrites each
golden to whatever the pipeline now produces, regressions included, so the test
passes right after regeneration whatever changed. `git diff` the golden and
check the new argv against the vendor's documented command-line flags before
staging it.

Adding a new preset to the harness (fixture → golden → registry entry) is
covered step by step in
[`docs/preset-library.md`](preset-library.md#adding-a-preset-to-the-harness).

---

## Code layout

```
sqi/
├── cmd/
│   ├── sqi-server/        Entry point, Cobra subcommands (serve, migrate, backup, restore, store, config, worker, tls, version; service on Windows)
│   └── sqi-worker/        Worker entry point
├── internal/
│   ├── api/               HTTP router, REST handlers, WebSocket upgrade, OpenAPI spec
│   ├── auth/              Auth gate, accounts, sessions, API keys, RBAC policy, LDAP, OIDC
│   ├── brokerauth/        NATS broker credential primitives shared by server and worker
│   ├── bus/               Typed NATS JetStream client wrapper
│   ├── certgen/           Farm CA, server and client certificate generation (sqi-server tls)
│   ├── config/            Typed config struct, layered loader
│   ├── diag/              In-memory diagnostic-log ring buffer
│   ├── discovery/         mDNS _sqi._tcp responder
│   ├── fslock/            Exclusive OS file lock (the SQLite single-server lock)
│   ├── fsutil/            Small shared filesystem helpers
│   ├── health/            /healthz and /readyz handlers
│   ├── log/               slog setup and the fan-out handler
│   ├── metrics/           Prometheus metric definitions
│   ├── middleware/        HTTP middleware (auth, CSRF, rate limiting, logging, metrics, versioning)
│   ├── openjd/            OpenJD parser, validator, parameter-space expansion
│   ├── pgdsn/             Reads and redacts PostgreSQL DSNs
│   ├── presetgen/         Generates the preset-library index.json + definition files
│   ├── presetlib/         Client for a community preset library index
│   ├── presettest/        Preset validation harness
│   ├── product/           Products/presets, the catalog layer over OpenJD templates
│   ├── restore/           Puts a backup in place and fences the attempts it records as open
│   ├── scheduler/         Lease handling, worker registry, status and log ingestion, periodic sweeps
│   ├── secretin/          Reads one line of secret input from stdin
│   ├── server/            Process boot, graceful shutdown orchestration
│   ├── store/             Store interface; sqlstore (shared SQL implementation), sqlite and postgres (the two databases), backend (opens the configured one), fake; migrations (migrations/postgres for PostgreSQL)
│   ├── tlsutil/           TLS loading primitives shared by server and worker
│   ├── ui/                Embeds web/dist; SPA fallback handler
│   ├── version/           Build metadata (version, commit, date, Go version)
│   ├── winsvc/            Runs sqi-server and sqi-worker as Windows services
│   ├── worker/            sqi-worker binary internals (executor, lease, heartbeat, capabilities, …); only worker/protocol is shared with the server
│   └── ws/                WebSocket hub, subscription management, scheduler-driven fanout
├── pkg/                   Public Go API (currently empty; see pkg/doc.go)
├── api/                   Reserved for non-Go API contract artifacts (currently empty; the OpenAPI spec is internal/api/openapi.yaml)
├── clients/               Python packages: clients/python (sqi-sdk), clients/submitter (sqi-submitter)
├── presets/               Reference (presets/sqi) and test (presets/testing) preset YAML
├── third_party/           Vendored OpenJD specifications submodule (specs, RFCs, and conformance fixtures)
├── web/                   Frontend source; web/dist is embedded
├── config/                Example config files
├── deploy/                Docker, Compose and systemd manifests
├── docs/                  Human-readable documentation
├── scripts/               Developer and CI scripts (smoke test, PostgreSQL and Windows test runners, oracle glue, signing hook)
└── test/                  Integration, conformance, oracle, preset-library and Windows-service test suites
```

### Key conventions

**Imports follow the dependency direction.** `cmd` → `internal/server` →
`internal/api`, `internal/scheduler` → `internal/store`, `internal/bus`. A
higher-level package imports lower-level ones (`internal/api` imports
`internal/auth`, `internal/openjd`, `internal/scheduler`, `internal/store`,
`internal/ws` and more), never the reverse.

**Interfaces over concrete types at package boundaries.** Handlers and the
scheduler receive the `store.Store` interface, never `*sqlite.Store` or
`*postgres.Store`, so tests can inject a fake.

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

**Changelog.** `CHANGELOG.md` is generated; do not edit it by hand.
[git-cliff](https://git-cliff.org) derives it from the Conventional Commit
history (config in `cliff.toml`), so the way to change an entry is to change the
commit message. `feat`/`fix`/`perf`/`refactor`/`build`/`ci`/`docs`/`test`
commits become changelog entries grouped by type; `chore` and `release` commits
are omitted, and so is a commit that doesn't parse as a Conventional Commit.
Regenerate with:

```sh
make changelog                 # refresh, leaving pending commits under [Unreleased]
make changelog VERSION=0.3.0   # roll pending commits into a dated 0.3.0 section
```

Install git-cliff first (`brew install git-cliff`, or see
<https://git-cliff.org/docs/installation>). The release workflow also runs
git-cliff at tag time and bundles the result into the release archives, but
does not commit that copy back, so the tracked `CHANGELOG.md` is refreshed by
hand during release prep (see `docs/release-runbook.md`).

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
            writeProblem(w, r, http.StatusNotFound, "job not found")
            return
        }
        h.logger.ErrorContext(ctx, "list steps", slog.String("job_id", jobID), slog.Any("err", err))
        writeProblem(w, r, http.StatusInternalServerError, "internal error")
        return
    }

    writeJSON(w, http.StatusOK, stepListResponse{
        Items: stepsToResponse(steps),
        Total: len(steps),
    })
}
```

### Step 2 — Wire the route in the router

Open `internal/api/router.go`. Routes are grouped by the permission they need
(each group calls `g.Use(az.require(policy.<Permission>))`), and a route on one
job also takes `az.requireJobAccessByJobID()` so the job-owner rules apply. Add
the new route to the matching group, next to the related routes:

```go
// inside the jobs.read group:
g.With(az.requireJobAccessByJobID()).Get("/jobs/{id}/steps", jobs.listSteps)
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

Then implement it in `internal/store/sqlstore/step.go` using a prepared
statement. Write the SQL with `?` placeholders and run it through the
`dbPool`/`dbTx`/`dbStmt` wrappers, never directly on `*sql.DB` or `*sql.Tx`;
`TestNoRawStatementCalls` enforces this.

**The same SQL runs on SQLite and PostgreSQL, so keep it portable.** The
dialect supplies what differs: `?` placeholders are rewritten to `$1, $2, …`
on PostgreSQL (`TestSQLConstants_NoQuestionMarkInLiteralsOrComments` keeps any
`?` out of string literals and comments, where the rewrite would corrupt it),
the wrappers bind Go `time.Time` and `bool` arguments through the dialect
(`BindTime`, `BindBool`; a time is stored as `TimeText`'s 30-character form),
booleans are scanned through `dbBool`, and a case-insensitive match is
`Dialect.CIEqual` or `Dialect.CILike`. Any `ORDER BY` behind a `LIMIT` or a
page ends with an `id` tiebreaker, and a nullable sort column states where its
NULLs go (`(col IS NULL) DESC, col ASC`, as `orderByClause` renders it), since
the two databases put NULLs at opposite ends by default. Build a query by
concatenation only from constants and the audited fragments
`TestSQL_ConcatenatesOnlyAuditedPieces` allows. A new method
needs nothing PostgreSQL-specific beyond that: it runs on both through the
shared layer, and its tests must pass three ways (the fake, SQLite and
PostgreSQL; see [Testing against PostgreSQL](#testing-against-postgresql)).
A method that reads rows to decide a write takes its anchor rows first, in
the [global lock order](architecture.md#lock-order) (see
[Store invariants](architecture.md#store-invariants)).

**A new table needs a migration, and every migration ships as a pair.** Add a
numbered SQL file to `internal/store/migrations/` (the next number in the
sequence, e.g. `00039_my_feature.sql`) **and** one with the same number to
`internal/store/migrations/postgres/`, each with `+goose Up`/`+goose Down`
sections following the pattern of the surrounding files. A new column or
index on an existing table needs a pair too. `TestSchemaParity`
(`internal/store/postgres`, run by `make test-postgres` and in CI) migrates a
fresh database of each kind and fails when their newest versions, tables,
columns, indexes or foreign keys differ, so a change made on one backend only
fails it. Write the PostgreSQL file natively: every `TEXT` column `COLLATE
"C"`, every timestamp column of the `ts` domain, integers `BIGINT`, flags
`BOOLEAN NOT NULL DEFAULT false`, and a case-insensitive unique as a unique
index on `lower(col)` (see [Two backends](architecture.md#two-backends)). A
new method on an *existing* table (like `ListStepsForJob` above, assuming
`steps` already exists) needs no schema change at all.

SQL that stamps a time itself must write the store's 30-character form:
`strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000000Z'` on SQLite, and on
PostgreSQL, where the `ts` domain's `CHECK` refuses any other form,
`to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"000Z"')` (see
"Timestamps" under the SQLite schema overview in `docs/architecture.md`).
`TestMigrations_StampTimesInTheStoredForm` fails on a SQLite migration whose
SQL, comments aside, uses strftime's `%SZ` or `%fZ`.

Finally, add the method to the in-memory fake so existing tests keep
compiling. The fake has one file per aggregate, mirroring
`internal/store/sqlstore/`: the `StepStore` methods above go in
`internal/store/fake/step.go`, and a wholly new aggregate gets a new file
(`internal/store/fake/workercredential.go` is a complete example).

> **Not every new store method is REST-triggered.** `RecordTaskFailure`,
> `RequeueTaskForRetry` (`internal/store/sqlstore/task.go`), and `ParkJob`
> (`internal/store/sqlstore/job.go`) exist only for the scheduler's auto-retry
> and failure-limit handling (`handleTaskFailed` in
> `internal/scheduler/failure.go`); no handler in `internal/api/` calls them.
> They still follow the same shape as
> REST-triggered methods: declared on the relevant sub-interface
> (`TaskStore`/`JobStore`), implemented in `internal/store/sqlstore/`, and
> stubbed in `internal/store/fake/` so scheduler tests can inject the fake.
> Layered policy resolution, such as picking the effective retry policy from
> Job → Queue → Farm → server default, is a plain package-level helper rather
> than a store method: see `ResolveRetryPolicy` in
> `internal/scheduler/retrypolicy.go`. Follow this pattern for
> scheduler-internal state changes that need no REST surface of their own.

> **Every terminal non-success must leave a reason.** `task_attempts.message`
> is the per-attempt reason (next to `exit_code`); `tasks.failure_reason`
> denormalizes the latest terminal reason onto the task (mirroring
> `unschedulable_reason`), cleared on retry. The reason is written by the same
> guarded store operation that moves the task to its terminal status, never by
> a separate call afterwards: `CompleteTaskAttempt` stamps a worker report's
> `FailureReason`, and `CancelJobExecution`, `CancelTaskExecution`,
> `CancelPendingStep` and `CancelBlockedJob` stamp their `reason` argument only
> on a task that has none yet, so a cascade-cancel's more specific reason
> always wins regardless of ordering. `FailureReasonSummary(ctx, jobID)`
> aggregates a job's failed tasks by reason (`FailedCount`, `DominantReason`,
> `DistinctReasons`) for the job-detail failure banner. **The rule for new
> code:** any new code path that drives a task to a terminal `failed` or
> `canceled` stamps its reason inside the write that moves the task, and only
> on a task with no reason when a more specific one may already be set. A
> separate setter call after the status write is a second write that can land
> on a task another writer has moved in between, which the store's
> [invariants](architecture.md#store-invariants) rule out.
> There is no standalone setter for the reason on `store.Store`; a test that
> needs a task with a `FailureReason` seeds it in its `storetest.Submit` call or
> drives the real failure or cancel write. See
> [the durable-failure-reason table](architecture.md#5-status-ingestion) for
> every existing path and its reason string.

### Step 3b — Gate it with a permission (if the route is not public)

A new permission must be added in **two** places:

1. **Server** — add the permission constant and its role grants to
   `internal/auth/policy/policy.go`, then mount the route behind the matching
   authorization middleware in `internal/api/router.go`.
2. **Web** — add the same string to the `Permission` union **and** the
   `PERMISSION_SET` literal in `web/src/auth/policy.ts`. The
   `Record<Permission, true>` type on `PERMISSION_SET` makes an omission a
   typecheck failure, and `web/src/auth/policy.test.ts` mirrors the server
   grants against `ALL_PERMISSIONS`, so a server-only permission fails a test
   rather than shipping with no client-side gate.

Then gate the UI affordance itself with `can(principal, '<permission>')` and,
for a whole route, wrap it in `<RequireRole permission="…">`; see
[`web-development.md`](web-development.md#role-gating-can-requirerole-navcard-filtering).

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

**Seed store state through the production writes.** A test that needs jobs,
steps or tasks in some state builds them with `internal/store/storetest`, which
is written against `store.Store` so the same test runs on the SQLite store, the
in-memory fake and, when it is configured, the PostgreSQL store:

- `storetest.Submit` creates a whole job (job, `DependsOn` edges, steps and
  tasks) in one `CreateJobSubmission`, the call production makes. Put every
  status and field the test needs in that one submission, except a task's
  `assigned` and `running`, which are leased (next bullet).
  `storetest.NewJob(id, farm, queue)` builds that submission by chaining
  `.As`, `.Step` and `.Task`, then writes it with `.Submit` or
  `.SubmitLeasing`.
- `storetest.SubmitLeasing` is `Submit` for a job with work in flight: each task
  asking to be `assigned` or `running` is submitted ready and then leased (and
  started) for real. `storetest.Lease`, `storetest.Start` and
  `storetest.Running` do the same for one task, through `LeaseTask` and
  `StartTaskAttempt`, so the attempt and its usage-pool claims are real.
- To test what happens after a transition, drive it rather than writing the
  resulting state: `storetest.Complete` and `storetest.FailAndRequeue` cover the
  common worker reports; call `CancelJobExecution`, `OfflineWorker` and so on
  for the rest.
- `storetest.InjectAttempt` and `storetest.InjectClaim` write a row with no
  state checks, through injectors that live on the concrete stores, not on
  `store.Store` (`storetest.InjectorFor` returns them, for a test that asserts
  on an injector's error). They are for states production cannot reach (an
  open attempt on a terminal task, a claim on a closed attempt). Never use one
  to build a state a production write can produce. A wrapper that embeds
  `store.Store` does not expose them, and the test fails naming the wrapper
  type; pass the store the wrapper wraps.
- `storetest.ClaimViolations(t, st)` runs the concrete store's check of
  invariant I3 (no active usage claim on a closed attempt or a terminal task)
  and returns the offending claim IDs. Assert it is empty after any write that
  should release claims.
- Two reads exist only for tests, because nothing in production makes them.
  `storetest.ActiveWorkerCredential(ctx, st, workerID)` finds a worker's
  active credential in `ListActiveWorkerCredentials`, the set the broker's keys
  are rebuilt from. `ListAuditEntries` lives on the concrete stores (the
  `storetest.AuditReader` interface), not on `store.Store`. Do not add a read
  method to `store.Store` that only tests call: every backend would have to
  implement it.

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
it via `openjd.Parse` + `openjd.ValidateWithBudget`), and panics if any file is
malformed. To add a new built-in:

1. Create `internal/product/builtins/<name>.yaml` using the definition file
   format — metadata fields (`name`, `title`, `description`, `category`,
   `version`) plus an inline `template:` key containing a full
   `jobtemplate-2023-09` OpenJD template.
2. Stamp the file with the SPDX comment header:
   ```
   # SPDX-License-Identifier: AGPL-3.0-or-later
   ```
3. Run `go test ./internal/product/...`: `TestBuiltins_LoadValidateAndStamp`
   validates every built-in definition and catches template errors.
4. Built-in names are reserved; `POST /api/v1/products` rejects names that
   shadow a built-in.
5. If the product requires a software capability tag, add a matching built-in
   detector in `internal/worker/capabilities/builtins/` (see
   [Adding a built-in DCC detector](#adding-a-built-in-dcc-detector) below).

### Custom products

Custom products are created at runtime through the REST API and stored in the
state store's `products` table. They are mutable (PUT/DELETE) and appear in
`GET /api/v1/products` merged with the built-ins.

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
| `SQI_STORE_DRIVER` | `postgres` to run against PostgreSQL (with `SQI_STORE_POSTGRES_DSN`) |
| `SQI_STORE_POSTGRES_DSN` | The PostgreSQL database to use when the driver is `postgres` |
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

A failing hook blocks the commit and prints a hint. To bypass for a WIP commit:
`git commit --no-verify`.

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

**`make lint` reports `Error return value is not checked` on `errors.AsType`**
— your `golangci-lint` predates 2.13.0. Check with `golangci-lint version` and
upgrade (`brew upgrade golangci-lint`); see the version floor noted under
[Prerequisites](#prerequisites).

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

To rebuild both binaries before starting the worker:

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

To simulate a multi-worker farm on one machine, for exercising the scheduler,
capability matching or queue routing, use the `run-workers` target. With
`sqi-server` running (`make run` in another terminal), start N workers:

```sh
make run-workers          # 3 workers (the default)
make run-workers N=5      # 5 workers
```

Each instance gets a distinct identity so they don't collide: a unique name
(`worker-1`, `worker-2`, …), its own data dir under `./.run/workers/worker-<i>`
(holding a separate `worker.id` UUID), and its own metrics port (`9091`,
`9092`, …). Ctrl-C stops them all. Inherited `SQI_WORKER_*` env vars still
apply, so you can layer on shared config, for example an explicit NATS URL
instead of mDNS discovery:

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
lease loop via the `lease.Dispatcher` interface and into the heartbeat
publisher via `heartbeat.StateSource`. To change how tasks execute, implement a
new type that satisfies these interfaces.

**The relevant interfaces** (defined in `internal/worker/lease/lease.go` and
`internal/worker/heartbeat/heartbeat.go`):

```go
// lease.Dispatcher — called by the lease loop for each incoming assignment.
type Dispatcher interface {
    Dispatch(ctx context.Context, m *protocol.AssignMsg) error
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
   `lease.Dispatcher` and `heartbeat.StateSource`. It also needs to
   implement `cancel.TaskCanceler` if you want NATS-driven cancellation:

   ```go
   // cancel.TaskCanceler
   type TaskCanceler interface {
       Cancel(taskID string) bool
   }
   ```

   To take part in the server's attempt reconciliation, also implement the
   optional `heartbeat.AttemptSource` (`ActiveAttemptIDs() []string`) and
   `Hold(attemptID string)`. The lease loop calls `Hold` for every assignment
   in a batch before dispatching any of it; the attempt must stay reported until
   its terminal status has been published (or `Dispatch` fails before running
   it). A source without `AttemptSource` sends heartbeats without
   `attempts_reported`, and the server never reconciles that worker's attempts.

3. **Publish status messages** — inject `*status.Publisher` and call
   `Running`, `Terminal`, and (on shutdown) `ShutdownFailed`. Match the
   existing executor's publish points:
   - `Running` immediately after the workload starts.
   - `Terminal` ("succeeded", "failed", "canceled") after it exits.

4. **Wire it in `cmd/sqi-worker/start.go`** — replace the `executor.New(...)`
   call with your constructor, passing the same status publisher
   (`statusPub`), session manager (`sessionMgr`), metrics and OpenJD output
   interceptor (`openjdInterceptor`).

5. **Add unit tests** — create `executor_test.go` asserting at minimum:
   - A `Dispatch` that fails before the task starts publishes a terminal
     `failed` status and returns the error.
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
       Arch() string
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
hardware tags above. See
[`docs/worker-capabilities.md`](worker-capabilities.md#capability-auto-detection-built-in-dcc-detectors)
for the full picture (how detection runs, the tag/version model, precedence)
and the
[Writing custom detectors](worker-capabilities.md#writing-custom-detectors)
reference for the detector schema.

**Paired-change convention:** a new built-in product or reference preset that
requires a software capability tag ships a matching built-in detector in
`internal/worker/capabilities/builtins/`. For reference presets this is
enforced: `TestBuiltinDetectors_CoverPresets` in
[`internal/worker/capabilities/builtins_test.go`](https://github.com/uberware/sqi/blob/main/internal/worker/capabilities/builtins_test.go)
checks every `attr.worker.tag.<x>` required by `presets/sqi/*.yaml` against the
tags the built-in detectors emit. It does not scan `internal/product/builtins/`,
so apply the convention to a new built-in product by hand. The one existing
exception is the `container` built-in's `docker` tag, which has no detector and
is set manually.

**Steps to add a new built-in detector:**

1. Create `internal/worker/capabilities/builtins/<name>.yaml` with the
   detector schema: `tag`, `checks` (each check exactly one of
   `exe`/`path_glob`/`env`/`registry`, with an optional per-check `os` gate),
   and an optional `version.from` regex. Use the existing built-ins
   (`maya.yaml`, `nuke.yaml`, `houdini.yaml`, `blender.yaml`) as reference;
   they use the same schema as a custom detector.
2. Stamp the file with the SPDX comment header:
   ```
   # SPDX-License-Identifier: AGPL-3.0-or-later
   ```
3. Run `go test ./internal/worker/capabilities/...`:
   `TestBuiltinDetectors_CoverPresets` and the built-in load/validate tests
   catch a malformed detector or an uncovered preset tag.
4. `make fmt && make lint`.

## sqi-sdk (Python) development

The Python client lives at `clients/python/` (import name `sqi_client`). It has
its own toolchain, virtualenv, dependencies and Make targets, separate from the
Go code.

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
it (not bare `pytest`) from the repo root, or `cd clients/python` first: pytest
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

1. **Transport** — use the shared helpers rather than calling `httpx`
   directly: `_request` / `_request_json` in `client.py` (path joining under
   `/api/v1`, `None`-param dropping, typed-error mapping, GET-only retry),
   `parse_page` in `models.py` for paginated responses, and the generic
   `_CrudResource` in `client.py` for standard create/get/update/delete
   resources.
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
is a second Python package that depends on `sqi-sdk`. It has its own
virtualenv, `pyproject.toml` and gate, independent of both the Go build and
`clients/python`. Full user-facing reference: [`docs/dcc-submitters.md`](dcc-submitters.md).

```
clients/submitter/
  pyproject.toml            sqi-submitter; requires-python >=3.9; deps: sqi-sdk
  src/sqi_submitter/
    core/                   no Qt, no DCC imports — session, form model, pre-fill, HostAdapter
    qt/                      Qt only, imported lazily (PySide6, falls back to PySide2)
    hosts/{maya,houdini,nuke,max,vred}/  adapter + launch glue (in-process hosts)
    hosts/{blender,cinema4d,aftereffects,unreal,katana,keyshot,silhouette,fusion,modo}/
                                         one self-contained host file each, plus a
                                         startup hook or kit where needed (handoff hosts)
    cli.py, install.py                   sqi-submit, sqi-submit install-host
  tests/                     pytest; tests/integration/ is env-gated (see below)
presets/sqi/                 the forty-two reference preset YAML fixtures (twenty-nine DCC render
                              presets, eight standalone renderer presets, five ffmpeg
                              transcode presets), validated by
                              internal/product/sqipresets_test.go against a per-preset
                              `want` map of expected category/params/extensions
presets/testing/             the four test/QA preset fixtures (test-render, test-steps;
                              bash + PowerShell), validated by
                              internal/product/testingpresets_test.go
```

Gate command line (from `clients/submitter`, with `sqi-sdk` resolvable; run
`pip install -e ../python` first if it is not installed):

```sh
ruff format --check . && ruff check . && mypy && mypy --python-version=3.13 tests && pytest -q
```

`tests/integration/` is skipped unless `SQI_TEST_SERVER_URL` points at a live
`sqi-server`; the Blender render smoke inside it additionally requires
`SQI_TEST_BLENDER=1` and a local Blender install. That smoke test renders
through the `blender-batch-render` preset; it does not exercise the Blender
add-on, which is covered by the manual checklist in `docs/dcc-submitters.md`.

Adding an in-process DCC adapter is a `HostAdapter` subclass plus launch glue,
walked through in
[Writing a new adapter](dcc-submitters.md#writing-a-new-adapter) in
`docs/dcc-submitters.md`. A host that cannot run the Qt dialog writes a context
file instead; see [The handoff contract](dcc-submitters.md#the-handoff-contract).
