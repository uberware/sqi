# `internal/`

Server-internal Go packages. Go's `internal` rule makes these importable only by code in this module — they are not part of any public API and may change without notice. Anything that needs to be consumed from outside the module belongs in `pkg/` instead.

## Dependency direction

`cmd/` → `internal/server` → (`internal/api`, `internal/scheduler`) →
(`internal/store`, `internal/bus`). `internal/server` is the only wiring layer:
it constructs the store, bus, scheduler, hub, auth chain and router and hands
them to each other. Leaf packages (`internal/store`, `internal/bus`,
`internal/log`, `internal/health`, `internal/metrics`, `internal/version`)
import no other `internal` package. Two near-leaves are worth naming:
`internal/diag` imports only `internal/log`, and `internal/config` imports
`internal/auth/oidc`, `internal/auth/policy`, and `internal/auth/rolemap` to
validate auth settings.

**Concrete types never cross a package boundary.** Handlers and the scheduler
receive the `store.Store` *interface*, never `*sqlite.Store`, so tests inject
`internal/store/fake`. `internal/store/sqlite` is imported only by
`internal/server` and `cmd/`.

**`internal/openjd` imports `internal/store`, so `internal/store` can never
import `internal/openjd`.** That is why the task state machine lives in
`internal/store/statemachine.go` while the step state machine lives in
`internal/openjd/statemachine.go`. It is also why the worker binary — which must
not link `internal/store` — cannot import `internal/openjd`, and why the shared
expression leaves `internal/openjd/expr` and `internal/openjd/intrange` exist.

## Packages

| Package | Purpose |
|---|---|
| `internal/api` | REST surface: chi router, handlers, request/response wire types, error shape |
| `internal/auth` | Opt-in authentication and authorization: local passwords, sessions, API keys, RBAC policy, role mapping, LDAP/AD and OIDC/SSO |
| `internal/bus` | Embedded NATS broker (JetStream streams for task status/logs/cancel and worker registration/heartbeat/deregister; plain core NATS for `work.lease.<worker>.<queue>` request/reply and `worker.diag.<workerID>`) and the typed client wrapper over it |
| `internal/config` | Layered runtime configuration (defaults → file → env → flags) and validation |
| `internal/diag` | Bounded in-memory ring buffer of diagnostic (operational) log records from the server and connected workers |
| `internal/discovery` | mDNS responder that advertises the running server on the local network |
| `internal/fsutil` | Filesystem helpers shared across packages (AppleDouble sidecar filtering) |
| `internal/health` | Liveness (`/healthz`, no checkers, always 200) and readiness (`/readyz`, runs the registered `sqlite` and `nats` checkers concurrently under a 5 s deadline; 503 `degraded` on any failure) |
| `internal/log` | `slog`-based structured logging setup and helpers |
| `internal/metrics` | Prometheus metric definitions and registration |
| `internal/middleware` | `net/http` middleware (request logging, recovery, etc.) |
| `internal/openjd` | OpenJD template parsing, validation, parameter-space expansion, path mapping |
| `internal/presetgen` | Generates the shipped preset catalog artifacts from the reference preset definitions |
| `internal/presetlib` | Fetches and caches the remote preset index; verifies SHA-256 on install |
| `internal/product` | Product catalog: embedded built-ins overlaid on stored custom/installed products |
| `internal/scheduler` | Lease handler and scheduling policy, worker registry, heartbeat/retention/unschedulable sweeps, cross-job dependency reconcile, and the server-side NATS consumers for task status, task logs, and worker diagnostics |
| `internal/server` | Component lifecycle: starting and stopping the server's subsystems |
| `internal/store` | Storage interface, SQLite-backed implementation, migrations, domain types, fake |
| `internal/ui` | Serves the embedded web UI bundle with single-page-application fallback routing |
| `internal/version` | Build-time metadata injected by the release toolchain |
| `internal/worker` | `sqi-worker` agent internals (see subpackages below) |
| `internal/ws` | WebSocket envelope, subscription hub, and fanout to web clients |

### `internal/worker/` subpackages

The worker agent is decomposed into focused subpackages: `capabilities` and
`registration` (self-reporting and registering with the server), `discovery`
(locating the server via mDNS), `natsclient` and `protocol` (NATS transport and
message types), `lease` (the long-poll work-lease loop over core NATS),
`heartbeat`, `executor` and `session` (OpenJD session lifecycle and bare-metal
process execution), `isolation` (run-as-user execution, one Provider per GOOS
plus a fake), `pathmap` (storage-location path resolution), `staging`
(stage_locally copy in/out via the operator's sync command), `openjd` and
`fmtres` (worker-side OpenJD format-string and EXPR phase-3 resolution),
`envutil` (environment filtering), `logstreamer` and `status` (streaming task
output and status back to the server), `cancel` (task/Session cancellation),
`diaglog` (the diagnostic-log sink that publishes to `worker.diag.<workerID>`),
`config`, `metrics`, and `obs` (observability).
