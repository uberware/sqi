# `internal/`

Server-internal Go packages. Go's `internal` rule makes these importable only by code in this module — they are not part of any public API and may change without notice.

Expected children (created as the corresponding tasks land):

- `internal/config` — layered configuration loading (tasks 16–19)
- `internal/log` — slog setup and request middleware (tasks 20–21)
- `internal/store` — SQLite-backed state store, migrations, fake (tasks 25–32)
- `internal/bus` — typed NATS JetStream client wrapper (tasks 36–39)
- `internal/openjd` — OpenJD parser, validator, expansion (tasks 40–45)
- `internal/scheduler` — assignment loop, worker registry, policy (tasks 46–55)
- `internal/api` — REST handlers and routing (tasks 66–80)
- `internal/ws` — WebSocket upgrade, subscriptions, fanout (tasks 81–85)
- `internal/ui` — embedded SPA hosting (tasks 86–88)
- `internal/discovery` — mDNS responder (tasks 89–90)

Anything that needs to be consumed from outside the module belongs in `pkg/` instead.
