# `internal/`

Server-internal Go packages. Go's `internal` rule makes these importable only by code in this module — they are not part of any public API and may change without notice. Anything that needs to be consumed from outside the module belongs in `pkg/` instead.

## Packages

| Package | Purpose |
|---|---|
| `internal/api` | REST surface: chi router, handlers, request/response wire types, error shape |
| `internal/bus` | Embedded NATS JetStream broker and the typed client wrapper over it |
| `internal/config` | Layered runtime configuration (defaults, file, env) and validation |
| `internal/discovery` | mDNS responder that advertises the running server on the local network |
| `internal/health` | Liveness (`/healthz`) and readiness (`/readyz`) checks |
| `internal/log` | `slog`-based structured logging setup and helpers |
| `internal/metrics` | Prometheus metric definitions and registration |
| `internal/middleware` | `net/http` middleware (request logging, recovery, etc.) |
| `internal/openjd` | OpenJD template parsing, validation, parameter-space expansion, path mapping |
| `internal/scheduler` | Assignment loop, worker registry, and scheduling policy |
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
message types), `pull` (the assignment pull loop), `heartbeat`, `executor` and
`session` (OpenJD session lifecycle and bare-metal process execution), `pathmap`
(storage-location path resolution), `logstreamer` and `status` (streaming task
output and status back to the server), `cancel` (task/Session cancellation),
`config`, `metrics`, and `obs` (observability).
