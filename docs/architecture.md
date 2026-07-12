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
| Worker protocol | `internal/worker/protocol` | Shared worker wire-protocol types (the rest of `internal/worker` is the sqi-worker binary; the server-side status/log ingestion lives in `internal/scheduler`) |
| Config | `internal/config` | Typed config struct, layered loader (defaults → file → env → flags) |
| Middleware | `internal/middleware` | Recovery, CORS, request ID, gzip, structured-logging |
| Metrics | `internal/metrics` | Prometheus counter, gauge, and histogram definitions |
| Health | `internal/health` | `/healthz` (liveness) and `/readyz` (readiness) handlers |
| Discovery | `internal/discovery` | mDNS `_sqi._tcp` responder |
| Preset library | `internal/presetlib` | Fetches and caches the remote preset index; verifies SHA-256 on install |
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
REST handler (internal/api/jobs.go)
  │
  ├─ Parse body → raw template bytes + detected content-type
  ├─ openjd.Parse(template)          → structured JobTemplate
  ├─ openjd.Validate(template)       → []ValidationError (reject if non-empty)
  ├─ openjd.ExpandParameterSpace(...) → []TaskParams (one per parameter combination)
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
and again via the scheduler's `handleTaskTerminal` → `propagateStepDependencies`
path whenever a task reaches a terminal state.

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

**Compute-location registry.** sqi maintains a curated, auto-populated catalog
of named compute locations (`compute_locations` table; served at
`GET /api/v1/compute-locations`). The catalog is informational only — it does
not gate scheduling. The matcher (`internal/scheduler/matcher.go`) keys
directly on the raw `step.ComputeLocation` string (promoted from the step's
`attr.worker.computelocation` host requirement) and on the worker's
`ComputeLocation` field; it is unchanged by the registry feature. A worker
whose location name does not appear in the registry is fully eligible for
matching tasks, and a registry entry with no matching workers simply reports
`worker_count: 0`. See [`docs/compute-locations.md`](compute-locations.md).

**Unschedulable detection.** On every heartbeat-sweep tick (the same tick that
reaps stale-`assigned` tasks and reclaims stale workers), the scheduler also
re-evaluates each `ready` task against the current online-worker set using the
same `WorkerEligible` check used above for lease assignment — read-only, it
never changes scheduling, only an annotation. A task that has waited longer
than `scheduler.unschedulable_grace` with no eligible online worker is flagged
with a human-readable `unschedulable_reason`, cleared automatically once a
matching worker appears or the task leaves `ready`. See
[`docs/observability.md`](observability.md#why-isnt-my-job-running-unschedulable-tasks)
for the operator-facing view and
[`scheduler.unschedulable_grace`](configuration.md#schedulerunschedulable_grace)
for the config knob.

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
NATS consumer (internal/scheduler/taskstatus.go)
  │
  ├─ Receive task.status message   { …, message }  ← worker's human-readable reason, if any
  ├─ store.UpdateTaskAttempt(attempt_id, status, exit_code, end_time, message)
  ├─ If terminal (succeeded/failed/canceled):
  │     store.TransitionTask(task_id, status)
  │     If failed/canceled: store.SetTaskFailureReason(task_id, reason)  ← see below
  │     usagePool.ReleaseClaim(claim_id)
  │     checkStepCompletion → propagateStepDependencies   ← marks successor tasks ready
  │     notifier.NotifyTask(...)                          ← triggers WebSocket fanout
  └─ ack message
```

**Durable failure reason.** Every `task_attempts` row carries a `message`
column next to `exit_code` — the worker's human-readable reason for that
attempt, sent in `TaskStatusMsg.Message` on every failure path (pre-exec/
staging error, `openjd_fail`, timeout, process error, plain non-zero exit).
`tasks.failure_reason` denormalizes the *latest terminal* reason onto the task
itself (mirroring `unschedulable_reason`), so the REST layer and web UI don't
need to join into attempts to explain a failure. It is set only when a task
reaches terminal `failed`/`canceled` and cleared on retry (auto or manual).
The reason is worker-reported where available, or synthesized by the
scheduler for the paths that have none:

| Path | Reason | Where |
|---|---|---|
| Worker-reported failure/cancel | the worker's `Message` verbatim | `handleTaskTerminal` |
| Failed with no worker message | `"failed (exit N)"` or `"failed"` | `handleTaskTerminal` fallback |
| Worker reclaimed (heartbeat timeout) | `"worker went offline"` | stale-worker sweep — set on the **attempt** `message` only; reclaim is not a task failure, so `tasks.failure_reason` is left untouched |
| Cascade-canceled (upstream step failed) | `"canceled: upstream step failed"` | stamped by `openjd.CancelDependents` inside the same UPDATE that cancels the tasks (`store.TransitionStepPendingTasks`) |
| User-initiated cancel | `"canceled by user"` | `CancelJob` stamps it inside the bulk-cancel UPDATE (`store.CancelJobTasks`); `CancelTask` uses `store.SetTaskFailureReasonIfEmpty`. Both are only-if-empty, so a cascade-cancel's more specific reason always wins regardless of ordering |

These server-originated reason strings are shared constants in `internal/store`
(`FailureReasonCanceledByUser`, `FailureReasonUpstreamFailed`,
`FailureReasonWorkerOffline`) — `FailureReasonSummary` groups by exact string,
so producers must not drift.

**Attempt history.** Each attempt's `message` — previously visible only by
reading the raw `task_attempts.message` column, with no REST surface — is now
served directly: `GET /api/v1/tasks/{id}/attempts` (backed by
`store.ListTaskAttempts`) returns the task's full attempt history, oldest
first:

```json
{
  "items": [
    {
      "attempt_number": 1,
      "status": "failed",
      "worker_id": "worker-abc",
      "exit_code": 1,
      "message": "openjd_fail: ...",
      "started_at": "2026-07-10T12:00:00Z",
      "ended_at": "2026-07-10T12:00:05Z"
    }
  ]
}
```

`status` is the attempt's own terminal/in-flight status
(`running`/`succeeded`/`failed`/`canceled`) — independent of the task's
current status, since a retried task's latest attempt may be `running` while
earlier attempts read `failed`. `worker_id`, `exit_code`, `message`, and
`ended_at` are omitted rather than sent empty (an in-flight attempt has no
`exit_code`/`ended_at`; a synthesized reclaim has no `worker_id`). Returns
`404` for an unknown task id, and `{"items":[]}` (not `404`) for a task with
no attempts yet.

This closes the gap left by `failure_reason`'s clear-on-retry behavior: a
**mid-retry task** — one that failed, was retried, and is now `running` or
`ready` again — has an empty task-level `failure_reason`, but the attempt row
where the earlier failure actually happened still carries its `message`, so
the reason is never lost, only relocated to the attempt it belongs to.

`GET /api/v1/jobs/{id}` (job detail) additionally exposes a `failure_summary`
(`failed_count`, `dominant_reason`, `distinct_reasons`), aggregated across the
job's failed tasks by `store.FailureReasonSummary`, and powers the web UI's
job-level failure banner. See
[`docs/observability.md`](observability.md#why-did-my-task-fail) for the
operator-facing view.

### 6. Log ingestion

```
NATS consumer (internal/scheduler/logingest.go)
  │
  ├─ Receive task.logs message (chunk: seq, timestamp, data)
  ├─ store.InsertLogChunk(attempt_id, seq, timestamp, data)
  └─ notifier.NotifyLog(task_id, chunk)   ← triggers WebSocket fanout for live tail
```

### 7. Real-time delivery to clients

```
WebSocket hub (internal/ws/hub.go)
  │
  ├─ The scheduler calls the hub's Notifier methods (NotifyTask/NotifyLog/
  │     NotifyJob/NotifyWorker) after it ingests each NATS message — the hub
  │     itself is not a NATS subscriber
  ├─ For each notification:
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
transition returns an error from `store.TransitionTask`. The auto-retry
re-queue described below (`running` → `ready` on a transient failure) is a
**separate, policy-driven store call** (`RequeueTaskForRetry`), not a
`TransitionTask` arrow — the diagram above still reflects every state a task
can be validated *into* via `TransitionTask`; auto-retry sends a task back to
`ready` directly instead of landing it on `failed` at all.

### Auto-retry on worker-reported failure

A worker-reported `failed` status no longer routes straight to a terminal
state. The scheduler (`internal/scheduler/failure.go`) resolves the effective
[retry policy](configuration.md#retry-failure-limits) (Job → Queue → Farm →
server default) and records the genuine failure via
`store.RecordTaskFailure`, then picks one of three outcomes:

- **Retry.** The task's genuine-failure count is still below its resolved
  `max_attempts` and the job has not hit its `failure_limit` — the attempt
  closes as failed, usage-pool claims are released, and the task re-enters
  `ready` (`store.RequeueTaskForRetry`) stamped with a `retry_after` backoff
  timestamp (`now + retry_delay`). It does **not** go through the terminal /
  step-completion path, since the step isn't actually done.
- **Exhausted.** The task's genuine-failure count reaches `max_attempts` with
  no failure limit tripped — the task goes terminal-`failed`, cascading to its
  step and job exactly as before this feature.
- **Auto-park.** The *job's* cumulative genuine-failure count reaches its
  resolved `failure_limit` — the job is parked first (`store.ParkJob`, sets
  `status=paused` and a `park_reason` like `"failure limit reached (3)"`),
  then the tripping task still goes terminal-`failed` so the step/job
  completion cascade runs normally rather than leaving the job half-running.

**Lost/reclaimed work is separate and uncapped.** A task reclaimed because its
worker went offline (heartbeat timeout) or because it lingered in `assigned`
past the stale-assigned reaper's timeout is simply returned to `ready` — it
never goes through `handleTaskFailed`, so it does not consume any of the
task's `max_attempts` and never counts toward the job's `failure_limit`. Only
a *genuine* worker-reported failure counts.

**A parked job is not stuck forever.** Parking only sets `status=paused`; it
does not force every other task in the job to a terminal state. If the job
still has non-terminal (`pending`/`ready`) tasks elsewhere — the common case,
since a job usually parks with work still in flight — it simply stays
`paused` until an operator resumes it (`PATCH /api/v1/jobs/{id}` with the
existing resume action), because job completion is still evaluated by "have
all steps reached a terminal state," which naturally stays false while
non-terminal work remains. But if the tripping task was the job's last
non-terminal work, the completion check finalizes the job to `failed`
immediately — a parked job is not exempted from normal completion.

**Un-parking re-arms the failure limit.** Both recovery paths clear the park
state (`store.ResumeJob` for the resume action, the retry reset inside
`store.RetryTasks` for a manual retry): `park_reason` is cleared and the
job's `failed_attempts` is reset to zero — without the reset, the very next
genuine failure would compare against the already-tripped counter and
instantly re-park the job. A *manually* paused job (empty `park_reason`)
keeps its accumulated `failed_attempts` across pause/resume, and a manual
pause is never overridden by a task retry.

**Two ready-selection gates support this.** `store.ListReadyTasks` (used by
the lease-assignment path) now excludes:

1. **Backoff gate** — tasks whose `retry_after` is in the future are skipped
   until the timestamp passes.
2. **Job-status gate** — tasks belonging to a `paused` or terminal
   (`completed`/`failed`/`canceled`) job are skipped.

The job-status gate is defense-in-depth, checked again at the lease-time
`leaseGatesPass` step to close the read-list→lease race window. It also fixed
a **pre-existing gap**: before this feature, a job that was *manually* paused
via the REST API had no gate stopping its `ready` tasks from still being
leased and run by a worker — pausing only stopped new *scheduling* decisions
downstream, not in-flight `ready` dispatch. The same gate now protects both
manually-paused and auto-parked jobs.

`failed` and `canceled` tasks can also be retried manually — individually via
`POST /api/v1/tasks/{id}/retry` or in bulk via `POST /api/v1/jobs/{id}/retry`.
On retry the task resets to `pending`, and its step and the job also reset to
`pending` when they were terminal; the scheduler's step-dependency propagation
then re-gates `pending`→`ready`, so tasks whose step dependencies are already
met land in `ready` immediately. A manual retry also resets the task's and
job's genuine-failure counters, independent of the automatic retry policy
above, and clears the task's `failure_reason` — both the automatic and manual
retry paths above leave a fresh task with no stale reason attached. See
[Durable failure reason](#5-status-ingestion) for how `failure_reason` is set
in the first place.

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
| `compute_locations` | Named compute-location registry (curated catalog; auto-populated from worker registrations) |
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

`sqi-sdk` (the `sqi-sdk` box in the component overview, per the [roadmap](roadmap.md))
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

The **preset library** (`internal/presetlib`) extends the product catalog with a
remote index of ready-made product definitions. It exposes `/api/v1/presets`
endpoints for browsing and installing presets; installed presets become products with
`source: installed`. See [`docs/preset-library.md`](preset-library.md).

---

## Further reading

- [`docs/configuration.md`](configuration.md) — Every configuration option with defaults and environment variable names.
- [`docs/api.md`](api.md) — REST API reference with worked examples.
- [`docs/python-client.md`](python-client.md) — Python client (`sqi-sdk`) reference.
- [`docs/development.md`](development.md) — Local setup, test commands, adding a new endpoint.
- [`docs/compute-locations.md`](compute-locations.md) — Compute-location registry: auto-registration, curation, step affinity, and storage-location roots.
- [`internal/store/migrations/`](https://github.com/uberware/sqi/tree/main/internal/store/migrations) — Full schema DDL.
