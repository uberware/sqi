# sqi-server Architecture

This document describes the internal component layout of `sqi-server` and
traces the complete data flow for a job's lifecycle — from API submission
through scheduling, worker execution, and final state.

---

## Component overview

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              sqi-server process                             │
│                                                                             │
│  ┌──────────────┐   HTTP/WS    ┌──────────────────────────────────────────┐ │
│  │   CLI client │ ──────────► │  REST API + WebSocket gateway            │ │
│  │   Web UI     │             │  (chi router, middleware stack)           │ │
│  │   sqi-sdk │ ◄────────── │  /api/v1/…  /api/v1/ws  /metrics        │ │
│  └──────────────┘             └───────────────┬──────────────────────────┘ │
│                                               │                             │
│                                    ┌──────────▼──────────┐                 │
│                                    │     Scheduler        │                 │
│                                    │  lease handler       │                 │
│                                    │  worker registry     │                 │
│                                    │  heartbeat sweep     │                 │
│                                    │  usage pool gating   │                 │
│                                    └──────┬───────────────┘                 │
│                                           │                                 │
│             ┌─────────────────────────────▼──────────────────────────────┐ │
│             │           embedded NATS (JetStream + core NATS)            │ │
│             │                                                            │ │
│             │  work.lease.<queue>    task.status.<job>                   │ │
│             │  task.logs.<task>      worker.heartbeat                    │ │
│             │  worker.register                                           │ │
│             └────────┬────────────────────────────────────────────────┬─┘ │
│                      │                                                │    │
│          ┌───────────▼──────────┐                   ┌────────────────▼──┐ │
│          │   SQLite state store  │                   │  WebSocket fanout  │ │
│          │  jobs  tasks  workers │                   │  per-client subs   │ │
│          │  farms queues pools   │                   │  backpressure      │ │
│          │  audit_log  storage   │                   └───────────────────┘ │
│          └──────────────────────┘                                          │
└─────────────────────────────────────────────────────────────────────────────┘
                   ▲                              ▲
                   │ NATS                         │ NATS
        ┌──────────┴────────┐         ┌──────────┴────────┐
        │    sqi-worker A   │  . . .  │    sqi-worker N   │
        │  task executor    │         │  task executor    │
        │  log streamer     │         │  log streamer     │
        └───────────────────┘         └───────────────────┘
```

### Key packages

| Package | Path | Role |
|---|---|---|
| REST + WebSocket | `internal/server`, `internal/ws` | HTTP router, middleware, WebSocket upgrade and subscription hub |
| Scheduler | `internal/scheduler` | Assignment loop, worker registry, heartbeat sweep, usage pool gating |
| NATS bus | `internal/bus` | Typed JetStream client wrapper; stream, subject, and consumer definitions |
| Store | `internal/store` | `Store` interface + SQLite implementation; migrations |
| OpenJD | `internal/openjd` | Template parser, validator, parameter-space expansion |
| Worker protocol | `internal/worker` | Server-side handlers for registration, heartbeat, status, log ingestion |
| Config | `internal/config` | Typed config struct, layered loader (defaults → file → env → flags) |
| Middleware | `internal/middleware` | Recovery, CORS, request ID, gzip, structured-logging |
| Metrics | `internal/metrics` | Prometheus counter, gauge, and histogram definitions |
| Health | `internal/health` | `/healthz` (liveness) and `/readyz` (readiness) handlers |
| Discovery | `internal/discovery` | mDNS `_sqi._tcp` responder |
| UI | `internal/ui` | Embeds `web/dist`; SPA fallback handler |
| Version | `internal/version` | Build metadata (version, commit, date, Go version) |

---

## Startup sequence

```
main()
  └─ cobra: serve subcommand
       1. Load and validate configuration (config.Load + config.Validate)
       2. Initialize slog structured logger
       3. Open SQLite, run pending migrations
       4. Start embedded NATS JetStream server
       5. Create in-process NATS client (internal/bus)
       6. Create Store (internal/store/sqlite)
       7. Create Scheduler (internal/scheduler) — starts goroutine pool
       8. Create WebSocket hub (internal/ws)
       9. Build chi router, mount middleware and route handlers
      10. Register NATS consumers (worker registration, heartbeat, status, logs)
      11. Start mDNS responder (if discovery.enabled)
      12. Start HTTP server
      13. Block on SIGINT / SIGTERM
      14. Graceful shutdown:
            a. Stop accepting new HTTP connections
            b. Drain in-flight HTTP requests
            c. Stop Scheduler
            d. Drain NATS in-flight messages, flush JetStream
            e. Close NATS server
            f. Run final SQLite WAL checkpoint
            g. Close SQLite
```

---

## Job lifecycle data flow

### 1. Submission

A client sends `POST /api/v1/jobs` with a raw OpenJD template (YAML or JSON).

```
client
  │
  │  POST /api/v1/jobs (OpenJD YAML/JSON body)
  ▼
REST handler (internal/server/api/jobs.go)
  │
  ├─ Parse body → raw template bytes + detected content-type
  ├─ openjd.Parse(template)          → structured JobTemplate
  ├─ openjd.Validate(template)       → []ValidationError (reject if non-empty)
  ├─ openjd.ExpandTasks(template)    → []TaskSpec (one per parameter combination)
  │
  ├─ store.CreateJob(template, steps, tasks)
  │     Writes in a single transaction:
  │       jobs row (status=pending, template verbatim)
  │       steps rows (one per step)
  │       tasks rows (one per expanded task, status=pending or ready)
  │
  └─ HTTP 201 Created  { id, name, status, step_count, task_count }
```

### 2. Task readiness

After a job is created, the store marks tasks `ready` when their step's
dependencies are satisfied. For jobs with no step dependencies (the common
case), all tasks of the first step are immediately `ready`. Tasks in later
steps become `ready` only after all tasks in their dependency steps have
reached `succeeded`.

This evaluation runs inside the `CreateJob` transaction for the initial set,
and again inside `RecordTaskTerminal` whenever a task reaches a terminal state.

### 3. Assignment (lease-on-request)

Ready tasks stay `ready` until a worker asks for work. Workers keep exactly one
outstanding lease request per queue on `work.lease.<queue>` (core-NATS
request/reply). When a request arrives the server:

```
handleLeaseRequest(queueID, workerID)
  │
  ├─ store.CommittedCores(workerID)   → committed (Σ required_cores of assigned+running tasks)
  ├─ free = worker.CPUCount − committed
  │     If free ≤ 0: park request in the per-queue waiter registry (~30 s hold)
  │
  ├─ store.ListReadyTasks(queue, …)   → candidates (priority-ordered)
  │
  │  First-fit walk over candidates:
  ├─ WorkerEligible(task, worker)     → bool (capability/queue/farm/location/amounts match)
  ├─ effectiveCost = task.RequiredCores ?? worker.CPUCount
  ├─ If effectiveCost fits free:
  │     store.LeaseReadyTask(taskID, workerID, now)
  │           Atomically: status ready→assigned, stamp assigned_at,
  │                       create running attempt, apply policy gates,
  │                       TryClaimSlots (usage pools)
  │     Decrement free; add to batch
  │
  └─ bus.Reply(batch []AssignMsg)
         Each AssignMsg includes: resolved command, args, env, path map, session_id
```

A parked request is woken when new work becomes available for that queue (job
submitted, task becomes `ready`, task completes freeing cores, or stale-task
reaper reclaims a task). Unfulfillable requests time out after ~30 s and return
an empty batch; the worker immediately re-requests.

### 4. Worker execution

```
sqi-worker
  │
  ├─ Keeps one outstanding lease request per queue (work.lease.<queue>)
  │     Long-poll (~35 s timeout); re-issues immediately on return
  ├─ Receives batch of AssignMsgs from server
  ├─ Executes each task (spawns child process, manages lifecycle)
  ├─ Streams log chunks → NATS task.logs.<task_id>
  └─ Reports status changes → NATS task.status.<job_id>
         { task_id, attempt_id, status, exit_code, timestamp }
```

### 5. Status ingestion

```
NATS consumer (internal/worker/status_handler.go)
  │
  ├─ Receive task.status message
  ├─ store.UpdateTaskAttempt(attempt_id, status, exit_code, end_time)
  ├─ If terminal (succeeded/failed/canceled):
  │     store.UpdateTask(task_id, status)
  │     usagePool.ReleaseClaim(task_id)
  │     store.EvaluateStepReadiness(job_id)   ← marks successor tasks ready
  │     bus.PublishTaskStatus(job_id, summary) ← triggers WebSocket fanout
  └─ ack message
```

### 6. Log ingestion

```
NATS consumer (internal/worker/log_handler.go)
  │
  ├─ Receive task.logs message (chunk: seq, timestamp, data)
  ├─ store.InsertLogChunk(attempt_id, seq, timestamp, data)
  └─ bus.PublishLogChunk(task_id, chunk)   ← triggers WebSocket fanout for live tail
```

### 7. Real-time delivery to clients

```
bus fanout goroutine (internal/ws/hub.go)
  │
  ├─ NATS subscriber on task.status.*, task.logs.*, worker.heartbeat
  ├─ For each received message:
  │     Look up subscribed WebSocket connections
  │     Enqueue to per-client send channel (drops on overflow / backpressure)
  └─ WebSocket write loop drains the send channel to the client
```

The `jobs` WebSocket subject carries normal job-status updates and also a
synthetic `removed` status event when a job is hard-deleted — either manually
via `DELETE /api/v1/jobs/{id}` or by the retention sweep. Clients that display
a job list (including the web UI `JobList`) should remove the row when they
receive `status: "removed"` for a job ID. The retention sweep runs on the same
heartbeat-sweep tick that handles offline-worker cleanup, controlled by
`scheduler.job_retention` and `scheduler.job_retention_include_failed`.

---

## State machine: task status

```
                     ┌─────────────────┐
                     │     pending     │  (dependency not yet met)
                     └────────┬────────┘
                              │ step dependencies satisfied
                              ▼
                     ┌─────────────────┐
                     │      ready      │  (eligible for assignment)
                     └────────┬────────┘
                              │ worker requests work; scheduler leases task
                              ▼
                     ┌─────────────────┐
                     │    assigned     │  (worker leased; brief window before running)
                     └────────┬────────┘
                              │ worker picks up task
                              ▼
                     ┌─────────────────┐
                     │     running     │  (child process active on worker)
                     └──┬──────────┬───┘
              exit 0    │          │ exit ≠ 0      │ cancel received
                        ▼          ▼               ▼
               ┌──────────┐ ┌──────────┐  ┌──────────────┐
               │succeeded │ │  failed  │  │   canceled   │
               └──────────┘ └──────────┘  └──────────────┘
```

Transitions are validated — only the arrows above are permitted. Any other
transition returns an error from `store.TransitionTask`.

`failed` and `canceled` tasks can be retried — individually via
`POST /api/v1/tasks/{id}/retry` or in bulk via `POST /api/v1/jobs/{id}/retry`.
On retry the task resets to `pending`, and its step and the job also reset to
`pending` when they were terminal; `ResolveDependencies` then re-gates
`pending`→`ready`, so tasks whose step dependencies are already met land in
`ready` immediately.

---

## NATS subjects and streams

| Subject pattern | Transport | Direction | Purpose |
|---|---|---|---|
| `work.lease.<queue>` | Core NATS request/reply | worker → server (request); server → worker (reply) | Worker requests a batch of tasks; server replies with assignments or empty on timeout |
| `task.status.<job_id>` | JetStream (`TASK_STATUS`) | worker → server | Terminal and intermediate status updates |
| `task.logs.<task_id>` | JetStream (`TASK_LOGS`) | worker → server | Log chunk delivery |
| `worker.heartbeat` | JetStream (`WORKER_HB`) | worker → server | Liveness heartbeat |
| `worker.register` | JetStream (`WORKER_REG`) | worker → server | Registration at startup |
| `worker.diag.<workerID>` | Core NATS (best-effort) | worker → server | Diagnostic log records |

JetStream streams use file-backed storage with configurable size limits.
`work.lease.<queue>` uses core NATS request/reply — no stream is created for
it. The server holds an unfulfillable request in memory for up to 30 s before
replying with an empty batch; the worker re-requests immediately.

---

## SQLite schema overview

Full DDL lives in `internal/store/migrations/`. The primary tables are:

| Table | Purpose |
|---|---|
| `farms` | Top-level organizational unit; holds default scheduling policy |
| `queues` | Belongs to a farm; jobs are submitted to a queue |
| `jobs` | One row per submitted job; holds verbatim OpenJD template |
| `steps` | One row per step in a job; tracks dependency graph |
| `tasks` | Atomic work unit; one per expanded parameter combination |
| `task_attempts` | One row per execution attempt; holds exit code, timing, session_id |
| `workers` | Registered workers with capabilities and status |
| `storage_locations` | Named storage locations with per-compute-location root mappings |
| `usage_pools` | Named concurrency limits (usage pools) with `max_concurrent` cap |
| `usage_claims` | Active usage claims tied to task attempts |
| `audit_log` | Append-only log of state-changing API operations |

WAL mode is always enabled (`PRAGMA journal_mode=WAL`). Foreign keys are
enforced (`PRAGMA foreign_keys=ON`). A busy timeout of 5 s prevents immediate
SQLITE_BUSY errors under concurrent write load.

**Sessions are not a first-class table.** A session is an ephemeral,
worker-side runtime grouping of task attempts (one setup/teardown environment
reused across attempts). It is identified only by the `session_id` carried on
`task_attempts` rows and on the status and log messages a worker publishes.
Persisting sessions as their own table would add no scheduling value, so
attempts reference a session by ID alone.

---

## sqi-sdk (Python library)

`sqi-sdk` (the `sqi-sdk` box in the component overview, per [`../ROADMAP.md`](../ROADMAP.md))
is a pure-Python client library that talks to `sqi-server` over the same public
surface as the web UI: the REST API for everything, plus the WebSocket gateway
for live events. It lives in the repository at `clients/python/` (import name
`sqi_client`) and is versioned and released alongside the binaries.

**Module layout** (`clients/python/src/sqi_client/`):

| Module | Role |
|---|---|
| `client.py` | `SqiClient` — the HTTP transport core (default headers, `/api/v1` prefix, typed-error mapping, GET retry/backoff, health probes) plus every resource method (submit, query, manage, CRUD) and the conveniences. |
| `models.py` | Frozen dataclasses and status enums mirroring the OpenAPI component schemas, with a tolerant `from_dict` parsing layer and the `Page`/`iter_pages`/`parse_page` pagination primitives. |
| `errors.py` | The `SqiError` exception hierarchy and RFC 7807 problem-details parsing. |
| `events.py` | The optional WebSocket event stream (`SqiEventStream`, `Event`), imported lazily so the core needs no `websockets`. |

**Design notes:**

- **Sync-first.** Phase 1 ships a synchronous client only (backed by
  `httpx.Client`). The transport is isolated so an `AsyncSqiClient` can be added
  later without breaking the public API.
- **Minimal dependencies.** The only required runtime dependency is `httpx`, so
  the library can be embedded in DCC Python environments (Maya, Houdini, Nuke).
  `PyYAML` and `websockets` are optional extras (`sqi-sdk[yaml]`,
  `sqi-sdk[ws]`), imported lazily and never required by the core.
- **Phase 3 auth extension point.** `SqiClient` accepts a `headers` mapping
  merged into every request (and carried onto the WebSocket upgrade); this is the
  forward-compatible hook for injecting authentication tokens once Phase 3 lands,
  with no change to the public method signatures.

See [`docs/python-client.md`](python-client.md) for the full client reference.

---

## OpenJD extensions

sqi validates template `extensions` against a registry and supports a vendor
(`SQI_`) namespace for sqi-defined extensions, with a promotion path to upstream.
See `docs/openjd-extensions.md` for the registry, namespacing convention, and the
contribution-doc pattern.

## Product catalog

Products are the catalog layer over OpenJD templates: a named, versioned wrapper
(metadata + stored template) that lets clients submit jobs without handling raw
template files. The `internal/product` package overlays embedded built-ins on
stored `custom`/`installed` products; the REST surface is at `/api/v1/products`.
See [`docs/products.md`](products.md) for the full reference.

---

## Further reading

- [`docs/configuration.md`](configuration.md) — Every configuration option with defaults and environment variable names.
- [`docs/api.md`](api.md) — REST API reference with worked examples.
- [`docs/python-client.md`](python-client.md) — Python client (`sqi-sdk`) reference.
- [`docs/development.md`](development.md) — Local setup, test commands, adding a new endpoint.
- [`internal/store/migrations/`](../internal/store/migrations) — Full schema DDL.
