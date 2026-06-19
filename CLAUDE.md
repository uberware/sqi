# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`sqi` (pronounced "sky") is a distributed task and render farm manager. Two Go binaries: `sqi-server` (scheduler + REST API + WebSocket + embedded NATS JetStream + SQLite + embedded React web UI) and `sqi-worker` (lease-based task executor). Jobs use the OpenJD (Open Job Description) format natively. AGPL-3.0 licensed, currently in Phase 1 (core scheduler, workers, basic web UI).

## Commands

```sh
make build            # Build both binaries into ./bin/ (builds web UI first)
make run              # Build then run sqi-server (serve on :8080, NATS on :4222)
make test             # All Go tests, race detector on (RACE=off to disable)
make test-cover       # Tests + coverage; fails below COVERAGE_MIN (default 60)
make test-integration # Integration tests (build tag 'integration', in ./test/)
make smoke            # E2E smoke test against real binaries (REST + WebSocket)
make lint             # golangci-lint (make lint-fix to auto-fix)
make fmt              # gofumpt + goimports
make ci               # Full local CI: fmt-check vet lint test-cover
```

Single test: `go test -race -run TestScheduler_LicenseGating ./internal/scheduler/...`

Web UI (run from `web/`):

```sh
npm run dev           # Vite dev server on :5173, proxies /api to localhost:8080
npm run test:watch    # Vitest watch mode
# Full pre-PR gate (matches CI):
npm run format:check && npm run typecheck && npm run lint && npm run test:coverage
```

Never run bare `go test ./...` or lint with `./...` from the repo root — `web/node_modules/` contains third-party Go files. The Makefile filters them (`GO_PKGS`, `LINT_PKGS`); use make targets or explicit package paths.

## Architecture

Full detail with diagrams: `docs/architecture.md`. The short version:

**Job lifecycle:** `POST /api/v1/jobs` with an OpenJD template → `internal/openjd` parses/validates and expands the parameter space into tasks → stored in SQLite (one transaction: job, steps, tasks) → tasks sit `ready` until a worker requests work via core-NATS `work.lease.<queue>` → the scheduler (`internal/scheduler`) computes free cores (`CPUCount − committed`), selects a priority-ordered batch that fits, atomically transitions it `ready → assigned` (stamping `assigned_at` now, when a real worker took the work), and replies → worker executes, streams logs to `task.logs.<task>` and status to `task.status.<job>` → server-side NATS consumers (`internal/worker`) write back to the store and fan out to WebSocket clients via `internal/ws`.

**Task state machine:** pending → ready → assigned → running → succeeded/failed/canceled. Transitions are validated in `store.TransitionTask`; only those arrows are legal. Step dependencies gate pending→ready.

**Diagnostic logs** (the binaries' own operational `slog` output — a separate channel from task process logs): a fan-out `slog.Handler` (`internal/log` `NewWithSink` + `Sink`) tees each record to a sink in addition to stderr. The worker sink (`internal/worker/diaglog`) publishes to **core NATS** (not JetStream) subject `worker.diag.<workerID>`; the server subscribes (`internal/scheduler/diagingest.go`) and, together with its own in-process logs, fills a bounded in-memory ring buffer (`internal/diag`, keyed by component `server`/`worker:<id>`, capped per-component + a global component ceiling, lost on restart by design). Served by `GET /api/v1/diagnostics/logs` and WebSocket subject `diagnostics`; shown in the web `DiagnosticsPanel` (worker detail, Admin page, failed-task fallback). Server `diagnostics.buffer_size` is the single knob (0 = off → no buffer/subscription/REST 503; default 1000 = per-component capacity); workers have a separate boolean `diagnostics.enabled` (publish or not). Full guide: `docs/observability.md`.

**Key packages** (under `internal/`): `server` (boot/shutdown orchestration), `api` (chi router, REST handlers, OpenAPI spec), `scheduler`, `store` (Store interface + SQLite impl + migrations + in-memory fake), `bus` (typed NATS wrapper, stream/subject definitions), `openjd`, `worker` (server-side worker protocol handlers), `ws` (WebSocket hub), `diag` (in-memory diagnostic-log ring buffer), `log` (slog setup + fan-out handler), `config` (layered: defaults → file → env → flags). The worker binary's internals live in `internal/worker/` subpackages: `executor`, `lease`, `heartbeat`, `capabilities`, `cancel`, `status`, `diaglog`.

**Dependency direction:** `cmd` → `internal/server` → `internal/api`, `internal/scheduler` → `internal/store`, `internal/bus`. No cross-imports between same-level internal packages. Handlers receive the `store.Store` interface, never `*sqlite.Store` — tests inject the fake from `internal/store/fake/`.

**Web UI** (`web/`, React 19 + TypeScript + Vite): built bundle in `web/dist/` is embedded into `sqi-server` via `web/embed.go` + `internal/ui`. All server access goes through `src/api/` (`apiFetch` → TanStack Query hooks); never call `fetch` from a component. Live updates via a single WebSocket (`src/ws/`, `useWebSocket` hook). Wire types in `src/api/types.ts` mirror the OpenAPI spec (`internal/api/openapi.yaml`), which is authoritative.

## Conventions

- **SPDX header on every source file** (Go, TS, YAML, Python), before package/imports — exact template in `docs/spdx-header.md`.
- **Conventional Commits** enforced by commit-msg hook: `type(scope)?: description`, types `feat fix docs style refactor test chore build ci perf revert`.
- Pre-commit hooks (lefthook, install via `make hooks`) run gofumpt, goimports (`-local github.com/uberware/sqi`), go vet, golangci-lint --fix. Bypass with `--no-verify` for WIP only.
- Table-driven Go tests (`[]struct{...}` + `t.Run`). One file per REST route group in `internal/api/`.
- Tests that open SQLite must use a unique temp path: `db := t.TempDir() + "/test.db"`.
- New REST endpoint = handler in `internal/api/<resource>.go` + route in `router.go` + Store interface method (sub-interface file in `internal/store/`) + SQLite impl + fake stub + OpenAPI spec entry + tests. Walkthrough in `docs/development.md`. (Exception: read-only endpoints backed by an in-memory service, like diagnostics, inject that service into `api.Deps` instead of touching `store.Store`.)
- **Bus transport:** task status/logs/heartbeat/registration go through JetStream (`bus.Client.publish`). Two channels use **core NATS** (no stream): (1) work leases — `work.lease.<queue>` request/reply (`bus.Client.RequestLease` on the worker, `Client.SubscribeLease` on the server) — the server gates CPU capacity and replies with a batch of `AssignMsg`; (2) ephemeral worker diagnostic logs — `worker.diag.<workerID>` (`Client.PublishWorkerDiag`/`SubscribeWorkerDiag`) — best-effort, nothing retained. The stable `workerID` from `workerconfig.LoadOrCreateWorkerID(DataDir)` is the same value used as the registration id, the task's `assigned_worker_id`, and the subject leaf for both core-NATS channels.
- **Lint is strict — run `make lint`, not just `go vet`:** `errcheck` rejects blank-assigned errors (`_ = f()`); either handle the error or annotate `//nolint:errcheck // <reason>` (precedent: `internal/middleware`, `internal/health`). `cyclop` caps function complexity (~15) — extract helpers. gofumpt wants a blank line between consecutive methods. Run `make ci` before declaring done (integration tests behind the `integration` build tag are vetted too, so a changed exported signature must update those callers).
- Web wire types: a task's worker is `assigned_worker_id` (not `worker_id`); the `Task` type has no failure-reason field.
- Web: strict TS (`noUncheckedIndexedAccess`, `exactOptionalPropertyTypes`), no `any` (narrow `unknown` with type guards), `@/` path alias not `../../`, CSS Modules co-located with components, all colors/spacing from `src/styles/tokens.css`, accessibility baseline in `docs/web-accessibility.md`. Component tests with Vitest + React Testing Library: query by role/label/text, mock at the network boundary.

## Key docs

`ROADMAP.md` (product spec, technical architecture, phases, and design), `docs/development.md` (setup, adding endpoints, worker executors, capability tags), `docs/architecture.md` (data flow, NATS subjects, schema), `docs/web-development.md` (dev-server workflow, routes, query hooks, WS subscriptions), `docs/configuration.md`.
