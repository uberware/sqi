# Diagnostic Logs & Production Observability — Design

**Date:** 2026-06-17
**Status:** Approved (design); implementation plan pending
**Author:** Robin Scher (with Claude)

## Problem

When a task fails for reasons that occur *before or around* the task process
itself, operators have no practical way to diagnose it in production.

The motivating case: incomplete OpenJD support caused tasks to fail with a
"process not found" error. The executor behaved correctly — `cmd.Start()`
failed, the worker logged a rich `executor: task process error` line **to its
stderr**, and published a terminal `failed` status carrying
`reason: "process error: start \"...\": executable file not found"`. The result
for the operator:

- The **task log** was empty — correct, because the process never ran and
  produced no stdout/stderr.
- The **web UI** showed only the short failure reason.
- The **diagnostic detail** lived in the worker process's stderr, visible only
  by tailing the terminal on the worker host — impractical in production.

There are two distinct gaps:

1. **Operator/diagnostic logs** from `sqi-server` and `sqi-worker` (the `slog`
   lines throughout the code) are not centrally visible. They go only to each
   process's stderr.
2. **Task-level diagnosis**: when a task fails with no process output, the UI
   gives an operator nothing to work with beyond a one-line reason.

## Current state (what already exists)

- **Task logs** (the process's own stdout/stderr): `internal/worker/logstreamer`
  batches lines → NATS JetStream `task.logs.<taskID>` → server ingest
  (`internal/scheduler/logingest.go`) → SQLite `task_log` + WebSocket fan-out →
  GUI viewer. Durable, replayable, live.
- **Diagnostic/operational logs**: `internal/log` builds an `slog` logger
  (JSON default, or text) that writes to stderr. The worker has its own
  equivalent. These are **not** collected anywhere.
- **Metrics + health**: Prometheus `/metrics` and `/healthz`/`/readyz` on the
  worker's loopback obs server (`internal/worker/obs`, default
  `127.0.0.1:9091`).
- **Correlation fields**: `slog` call sites already carry `task_id`,
  `attempt_id`, `job_id`, `worker_id`, `session_id` in many places — strong
  raw material for cross-component tracing.

## Goals

- **Small farms / quick glance:** see recent server and worker diagnostic logs
  directly in the web UI, with no external infrastructure and no host shell
  access. Works even when the worker has since crashed or disconnected.
- **Advanced operators:** plug `sqi` into their existing log stack
  (journald / Docker / Loki / ELK) the standard way, with documented structure
  and reliable correlation fields. They can disable the in-UI buffer to avoid
  spending resources on something they access out-of-band.
- **Fix the motivating bug:** a task that failed with an empty task log should
  be self-diagnosable in the UI.

## Non-goals

- A searchable, durable, long-retention log archive in `sqi` (explicitly
  rejected in favor of a bounded recent buffer).
- File-based log output with rotation (deferred; out of scope).
- OpenTelemetry / OTLP export (possible later phase; out of scope).
- Surviving a **server** restart with the in-UI buffer intact (accepted
  tradeoff of the ephemeral design — see below).

## Key decisions

| Decision | Choice | Rationale |
|---|---|---|
| Consumption model | **Hybrid** | External tooling for advanced ops; in-UI buffer for small farms and quick glances. |
| In-UI depth | **Recent ring buffer, server-held** | "Quick glance at the last bit", and it survives worker crashes because the server holds it. Disableable. |
| Transport | **A1 — ephemeral core NATS pub/sub** | Workers are outbound-only; push mirrors the existing `task.logs` pipeline. No JetStream stream = zero added storage. Best-effort recent history. |
| Server-restart durability | **Not preserved** | Accepted tradeoff of A1. Revisit (A2: JetStream stream) only if it bites. |
| External path | **Docs + correlation polish only** | Structured JSON + correlation fields already exist; document and audit rather than build. |
| Task-detail fallback | **In scope** | Directly resolves the motivating bug. |

### Approaches considered for transport

- **A — push over NATS into a server-side ring buffer (chosen).** A `slog`
  fan-out handler on the worker writes to stderr as today *and* publishes each
  record to `worker.diag.<workerID>`. The server subscribes; its own logs feed
  the same buffer in-process.
  - **A1 — ephemeral core NATS pub/sub (chosen):** no stream, no acks, no
    storage; best-effort; buffer lost on *server* restart.
  - **A2 — JetStream stream** (small `MaxAge`/`MaxMsgs`): survives server
    restart, replayable, but adds stream storage + ack machinery. Deferred.
- **B — server pulls from worker obs endpoint.** Rejected: breaks the
  outbound-only worker model; the obs server is loopback-only and unreachable
  from the server.
- **C — durable SQLite history.** Rejected: contradicts the bounded ring-buffer
  goal; storage growth, pruning, paging.

## Architecture

```
sqi-worker                                  sqi-server
----------                                  ----------
slog ──► fan-out handler ──► stderr         slog ──► fan-out handler ──► stderr
                │                                          │
                ▼ (sink, if enabled)                       ▼ (sink, in-process)
        DiagLogMsg JSON                              ┌──────────────┐
                │                                    │ ring buffer  │
                ▼                                    │ (internal/   │
   core NATS publish                                 │   diag)      │
   subject worker.diag.<workerID> ───────────────────►  per-comp.   │
                                  (server subscriber)│  bounded     │
                                                     └──────┬───────┘
                                                            │
                                        REST GET /api/v1/diagnostics/logs
                                        WS live-tail topic
                                                            │
                                                            ▼
                                                       Web UI panels
```

### 1. Fan-out `slog` handler (`internal/log`)

A `slog.Handler` that wraps the existing JSON/text handler. On each record it:

1. Delegates to the wrapped handler (writes to stderr exactly as today).
2. Passes the record to a configured **sink** (an interface), if any.

Critical constraints:

- **No recursion.** The sink must never emit `slog` records itself. Its own
  errors are written directly to stderr (or dropped), never routed back through
  `slog`, or it would feed itself in a loop.
- **Cheap when disabled.** When no sink is configured, behavior is byte-for-byte
  identical to today.
- Respects the configured level (a record filtered out for stderr is not sent
  to the sink either).

The worker uses the same handler with a NATS-publishing sink; the server uses it
with an in-process sink that writes straight into the ring buffer.

### 2. Worker publish side (`internal/worker/diaglog`)

Mirrors `internal/worker/logstreamer`. The sink serializes a compact message:

```go
type DiagLogMsg struct {
    Ts        time.Time         // record time
    Level     string            // "DEBUG"|"INFO"|"WARN"|"ERROR"
    Msg       string            // record message
    Attrs     map[string]string // flattened attributes, incl. correlation keys
}
```

Published with core NATS (not JetStream) to `worker.diag.<workerID>` (new
subject constant + helper in `internal/bus`). **Best-effort:** publish errors
are dropped; publishing never blocks task execution and never fails a task.
Disabled by config → sink is a no-op and only stderr is used.

### 3. Server ring buffer (`internal/diag`)

A bounded, in-memory, concurrency-safe buffer.

- **Keyed per component:** `"server"` and `"worker:<workerID>"`.
- **Bounded:** configurable cap per component (e.g. last N lines and/or last X
  minutes) plus a global ceiling; eviction drops oldest (ring semantics).
- **Two feeders:**
  1. the server's own fan-out sink (in-process, no NATS round trip);
  2. a `worker.diag.>` core-NATS subscriber that decodes `DiagLogMsg`.
- **Backpressure:** a bounded channel in front of the buffer with a `dropped`
  counter (exposed as a metric) so a log storm cannot OOM the server.
- Stored record: `{Ts, Level, Component, Msg, Attrs}`.

This is a standalone injected service, **not** part of `store.Store` — so there
are no SQLite migrations, no schema, no fake-store stubs for persistence. It
exposes a small interface (with a fake) for handler tests.

### 4. REST + WS surface (`internal/api`)

- `GET /api/v1/diagnostics/logs` with query params:
  `component`, `level` (minimum level), `task_id`, `since` (timestamp/cursor),
  `limit`. Returns recent records from the buffer, newest-last.
- A WebSocket topic for **live tail** of new records, via the existing
  `internal/ws` hub (same pattern as task-log live updates).
- OpenAPI spec entry in `internal/api/openapi.yaml`; wire types added to
  `web/src/api/types.ts`.
- Authorization: same posture as other read endpoints.

### 5. Web UI (`web/`)

All three surfaces must be designed and verified in **both light and dark
themes**. Use only `src/styles/tokens.css` variables — never hardcoded colors.
Log levels map onto existing themed severity tokens:

| Level | Token |
|---|---|
| ERROR | `--color-error` / `--color-error-muted` |
| WARN  | `--color-warning` / `--color-warning-muted` |
| INFO  | `--color-info` / `--color-info-muted` |
| DEBUG | `--color-text-muted` |

Surfaces:

1. **Worker detail — Diagnostics panel.** Recent worker log lines, level-colored
   and monospace, live-updating over WebSocket. Level filter.
2. **System/server log view — under an Admin section.** The master's own recent
   diagnostic logs live on an **Admin** page reached from a new "Admin" item in
   the nav. The server log is the first and (for now) only admin section, but
   the page is built as an extensible container (e.g. a sectioned/tabbed Admin
   layout) so future admin tools can be added without restructuring. Same
   component treatment as the worker panel.
3. **Task-detail fallback (resolves the motivating bug).** When a task is in a
   `failed` state **and** has no task-log output, the task log view falls back to
   showing that attempt's worker diagnostic events (filtered by `attempt_id`),
   plus the full terminal failure `reason` shown prominently. So "process not
   found" becomes self-diagnosable in the UI.

All UI data goes through `src/api/` (`apiFetch` → TanStack Query hooks) and the
single WebSocket (`useWebSocket`); no `fetch` from components. CSS Modules
co-located with components.

### 6. Configuration

Both binaries gain a diagnostics section (layered defaults → file → env →
flags, per `internal/config`):

- `diagnostics.enabled` — **default on** with a modest cap. Worker: when off,
  the sink is a no-op (stderr only). Server: when off, no buffer is kept and the
  `worker.diag.>` subscription is not started.
- Capacity / retention knobs (per-component line cap and/or max age, global
  ceiling).

Advanced operators set `diagnostics.enabled=false` and rely on the out-of-band
path.

### 7. External / advanced path (docs + correlation polish)

- **Audit** `slog` call sites (executor, scheduler, bus, api, worker
  subpackages) for **consistent correlation keys**: `worker_id`, `job_id`,
  `task_id`, `attempt_id`, `session_id`. Fill gaps so cross-component tracing
  works in any aggregator.
- **New doc** `docs/observability.md` (cross-linked from `operations.md` and
  `worker-deployment.md`): the JSON log schema, field/level reference, the
  in-UI diagnostics feature and how to disable it, and worked examples of wiring
  stdout/stderr to journald, Docker logging drivers, Loki, and ELK.
- **Confirm** JSON is the production default for both binaries; document `text`
  as the dev convenience.

## Error handling

- **Worker publish failure:** dropped, best-effort. Never blocks or fails a
  task. The diag sink's own errors go to stderr, never back through `slog`.
- **Ring buffer overflow:** drop oldest (ring).
- **Backpressure / log storm:** bounded ingest channel; excess dropped with a
  `dropped` counter/metric.
- **Server restart:** buffer empties. Documented and accepted (A1 tradeoff).
- **NATS unavailable:** worker diag publishes fail silently; server subscriber
  reconnects with the existing NATS client; task execution and existing
  pipelines are unaffected.

## Security / privacy

- Diagnostic lines carry paths, command names, and error strings — **not** secret
  values. Existing `openjd_redacted_env` scrubbing keeps secrets out of task
  output; diag lines log attribute *keys*, not secret values.
- `docs/observability.md` will state explicitly: do not log secret values.
- Diagnostics endpoints sit under the same authorization posture as other read
  endpoints.

## Testing (test-first)

- **Fan-out handler:** record reaches both stderr and sink; level filtering
  honored; sink errors do not recurse; no-op when no sink.
- **Ring buffer:** capacity eviction, per-component keying, filtering by
  level/`task_id`/`since`, concurrent access under `-race`, dropped-counter on
  overflow.
- **NATS round-trip:** worker publishes `DiagLogMsg`; server subscriber decodes
  into the buffer (integration test against the broker).
- **API handler:** query params, paging/cursor, empty cases, injected fake
  service.
- **Config:** enable/disable gating on both binaries.
- **Web:** panel renders records; live update via mocked WS; task-detail
  fallback shows worker diagnostics + reason when task log is empty and failed;
  **visual check in both light and dark themes**. Vitest + RTL, query by
  role/label/text, mock at the network boundary.

## Build sequence (high level)

1. `internal/diag` ring buffer + interface + fake (TDD).
2. Fan-out `slog` handler in `internal/log` (TDD).
3. `internal/bus` subject + helper for `worker.diag.<id>`.
4. Worker `diaglog` sink + wiring into the worker logger (TDD).
5. Server: wire fan-out sink + `worker.diag.>` subscriber into boot
   (`internal/server`).
6. REST endpoint + OpenAPI + WS topic (TDD).
7. Config on both binaries (TDD).
8. Web: API hooks + WS subscription + three UI surfaces (both themes) (TDD).
9. Docs + correlation-field audit.

## Open questions / future

- If "buffer lost on server restart" proves painful, promote transport to **A2**
  (JetStream stream with small retention) — additive, no UI/API change.
- File output with rotation and OTLP export remain candidates for a later phase.
