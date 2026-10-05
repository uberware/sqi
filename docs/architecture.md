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
│             │  work.lease.<worker>.<queue>   task.status.<worker>.<job>  │ │
│             │  task.logs.<worker>.<task>     task.cancel.<task>          │ │
│             │  worker.register.<worker>      worker.heartbeat.<worker>   │ │
│             │  worker.deregister.<worker>    worker.diag.<worker>        │ │
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
| REST + WebSocket | `internal/api`, `internal/ws` | chi router, REST handlers, OpenAPI spec, WebSocket upgrade and subscription hub |
| Boot orchestration | `internal/server` | Process startup and graceful shutdown; wires the store, bus, scheduler, auth, and router together |
| Auth | `internal/auth` | Opt-in authentication and authorization: `password` (argon2id local login), `session` (server-side cookie sessions), `apikey`, `policy` (RBAC), `rolemap`, `ldap`, `oidc`, plus the `Authenticator` interface and `chain` that select among them (see [`docs/auth.md`](auth.md)) |
| Scheduler | `internal/scheduler` | Assignment loop, worker registry, heartbeat sweep, usage pool gating |
| NATS bus | `internal/bus` | Typed JetStream client wrapper; stream, subject, and consumer definitions |
| Store | `internal/store` | `Store` interface + SQLite implementation; migrations |
| Diagnostics | `internal/diag` | Bounded, per-component in-memory ring buffer of server + worker operational log records; backs `GET /api/v1/diagnostics/logs` and the `diagnostics` WebSocket subject (see [`docs/observability.md`](observability.md)) |
| Logging | `internal/log` | `slog` setup plus the fan-out `Handler`/`Sink` (`NewWithSink`) that tees every record to stderr *and* to a sink — the diag buffer on the server, the `worker.diag.<workerID>` publisher on a worker |
| Products | `internal/product` | Product/preset catalog above OpenJD: embedded built-ins overlaid on stored `custom`/`installed` products |
| OpenJD | `internal/openjd` | Template parser, validator, parameter-space expansion |
| Worker protocol | `internal/worker/protocol` | Shared worker wire-protocol types (the rest of `internal/worker` is the sqi-worker binary; the server-side status/log ingestion lives in `internal/scheduler`) |
| Config | `internal/config` | Typed config struct, layered loader (defaults → file → env → flags) |
| Middleware | `internal/middleware` | Recovery, CORS, request ID, gzip, structured-logging, auth (see [`docs/auth.md`](auth.md)) |
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
       3. Open SQLite, run pending migrations (internal/store/sqlite) and
          register it as the "sqlite" readiness checker (Store.Ping, read pool)
       4. Start the background WAL checkpointer (store.checkpoint_interval)
       5. Start the expired-session sweeper (no-op when auth is disabled)
       6. Seed a default farm + queue on first start (no-op once any farm exists)
       7. Start embedded NATS JetStream server, provision streams, and register
          it as the "nats" readiness checker
       8. Create in-process NATS client (internal/bus)
       9. Create WebSocket hub (internal/ws) — before the scheduler, so it can be
          passed in as the notifier — then wire the diagnostic buffer's notify
          callback to it
      10. Create and run Scheduler (internal/scheduler). Scheduler.Run is what
          registers every NATS consumer: worker registration/heartbeat/deregister,
          task status, task logs and the core-NATS worker.diag.> subscriber. It
          then finalizes, once, any step an earlier release left stuck and
          releases any pending step a retry reset but never released
          (reconcileStuckSteps and reconcilePendingSteps, see "Store
          invariants"), and only after that subscribes the core-NATS
          work.lease.> request/reply handler and starts the heartbeat sweep
      11. Wire auth (internal/server wireAuthDeps) — skipped to the anonymous
          superuser when auth.enabled is false, so auth-off boot is unchanged:
            a. Bootstrap the first admin account (no-op once any user exists)
            b. Select the authenticator chain (API key → session cookie)
            c. Build the LDAP verifier    (if auth.ldap.enabled)
            d. Build the OIDC provider    (if auth.oidc.enabled; issuer
               discovery is lazy — a brief provider outage must not block boot)
      12. Build chi router, mount middleware and route handlers
      13. Start HTTP server
      14. Start mDNS responder (if discovery.enabled)
      15. Block on SIGINT / SIGTERM
      16. Graceful shutdown (30 s deadline, server.ShutdownTimeout):
            a. Stop the mDNS responder (goodbye packets first)
            b. Stop accepting new HTTP connections and drain in-flight requests
            c. Stop Scheduler
            d. Drain NATS in-flight messages, flush JetStream
            e. Close NATS server
            f. Final WAL checkpoint in TRUNCATE mode (checkpointer goroutine, on
               context cancel) then close both SQLite pools
```

---

## Job lifecycle data flow

Every `/api/v1` REST call below passes through the auth middleware first
(anonymous superuser by default, auth off); `/api/v1/ws` is gated by its own
upgrade hook instead, and `/healthz`, `/readyz`, `/metrics`, and
`/api/v1/openapi.yaml` stay public regardless. See [`docs/auth.md`](auth.md).

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
  │
  ├─ openjd.Submitter.Submit(...)
  │     openjd.Parse(template)           → structured JobTemplate
  │     openjd.Validate(template)        → []ValidationError (reject if non-empty)
  │     openjd.ExpandParameterSpace(...) → []TaskParams (one per parameter combination)
  │       Expansion runs to completion in memory first: a template that cannot
  │       expand never reaches the store.
  │
  │     store.CreateJobSubmission(job, dependsOn, steps, tasks)
  │       Writes in a single transaction:
  │         jobs row (status=pending or blocked, template verbatim)
  │         job_dependencies rows (one per cross-job dependency edge)
  │         steps rows (one per step)
  │         tasks rows (one per expanded task, status=pending or ready)
  │
  └─ HTTP 201 Created  { id, name, status, step_count, task_count }
```

That single write is **load-bearing, not incidental**. Submission used to write
those rows through separate store calls, which left two defects: a failure
partway through stranded a `pending` job that nothing reaps, and a *store*
failure on a later step left the earlier steps persisted while that step's row
was lost entirely — producing a job that `checkJobCompletion`, which derives job
status from the steps that *exist*, would later mark `completed` having silently
lost work. (An *expansion* failure produced only the first: the step row was
written before its tasks were expanded, so the job kept all its steps and simply
hung `pending`.) Both are properties of partial creation, so splitting the write
back up reintroduces both.

**It holds the write connection for its full duration — which is why reads run
on a second connection pool.** The write pool is `SetMaxOpenConns(1)`
(`internal/store/sqlite/store.go`), so one submission owns it from `BeginTx` to
`Commit`. Anything queuing behind it queues in Go's `database/sql` pool, which
is not `SQLITE_BUSY` and which `busy_timeout` does not affect — nothing surfaces
it as a lock error, it simply stalls. When reads shared that pool, `GET /readyz`
stalled with them: its `sqlite` checker is `Store.Ping`, and `internal/health`
gives all checkers a **5 s** budget per request, so a submission over that
budget returned HTTP 503 `degraded` — endpoint removal under an orchestrator, at
~65k tasks, which is 6.5% of one step's legal maximum. Measured on an M-series
Mac (single step, one `CreateJobSubmission` call, `/readyz` issued 50 ms in):

| tasks | transaction | `/readyz`, shared pool | `/readyz`, split pools |
|---|---|---|---|
| 1,000 | 57 ms | ok | ok |
| 10,000 | 683 ms | ok | ok |
| 25,000 | 1.73 s | ok | ok |
| 50,000 | 3.53 s | ok | ok |
| 75,000 | 5.40 s | **503** (`context deadline exceeded`) | ok |

WAL mode lets readers proceed alongside a writer, so `Store` now opens a second
pool over the same file for `SELECT`s only, and `Store.Ping` — the readiness
checker — deliberately uses it. Measured the same way, `Ping` returns in **tens
of microseconds** while a 75,000- or 100,000-task submission is mid-flight. See
the Concurrency section of `internal/store/sqlite`'s package doc for how a
statement is routed and why the classification is by SQL verb rather than by Go
method.

**Other writes still queue**, by design: lease transitions, task-status writes
and the sweeps all go through the one write connection, and a long submission
delays them exactly as before. Only reads were freed. The stall is also not
*new* cost for the inserts themselves — batching them is faster than the per-row
path (measured 7.4 s versus 8.8 s for 100,000 tasks) — but the window during
which other writers wait is one contiguous transaction instead of N gaps.
`GET /healthz` (liveness) registers no checkers and was never affected.

**Correctness is not an accident of that single write connection.** Every rule
that spans more than one row (a usage claim exists only while its attempt is
open, a step finalizes only when every task is terminal, a pool never exceeds
its cap) is enforced by a single store operation. On SQLite the write
connection provides the serialization those operations rely on, and the
anchor-lock hook most of them call (`lockAnchors`) is a no-op. A PostgreSQL
store must take the anchor locks instead, and [Store invariants](#store-invariants)
lists the operations that name no anchor yet and what the anchor table does
not close.

**Cross-job dependencies (`depends_on`).** A submission — raw `POST /api/v1/jobs`
or `POST /api/v1/products/{name}/jobs`, from the REST API, the web UI, or the
Python SDK (`submit_job`/`submit_and_wait`/`submit_product_job`) — may include
`depends_on`, a list of upstream job IDs that must all reach `completed` before
this job's work may run. The upstreams must already exist and be in the same
farm as the new job (cross-farm dependencies are not supported); this is
validated at submit time and recorded as edges in `job_dependencies`. The
submitter's check runs before the write, so `CreateJobSubmission` re-checks
every upstream inside its own transaction: one that failed, was canceled or
was deleted in between makes it refuse the whole submission
(`store.ErrDependencyUnsatisfiable`, HTTP 422, worded exactly as the
submitter's own pre-check words the same cause), while one that completed in
between is left to the sweep, which releases the job on its next tick. A job
with a non-empty `depends_on` is created in a new `blocked` status instead of
`pending`, and — unlike a normal submission — **every** step and task is
written `pending` up front, even steps with no step-level dependencies that
would otherwise be immediately `ready`; the job is held entirely until it is
released. Dependencies work **across queues** within a farm — readiness is
farm-wide and queue affinity is only applied when a worker leases work, so a
released job's tasks are immediately leasable regardless of which queue they
or their upstreams belong to.

A `blocked` job is reconciled whenever an upstream job reaches a terminal
status: if every upstream in its `depends_on` list is now `completed`, the job
is released to `pending` and `openjd.ResolveDependencies` re-runs to promote
its no-dependency steps' tasks to `ready`, same as a fresh submission; if any
upstream is `failed`, `canceled`, or deleted, the blocked job is itself
canceled with an upstream-failed reason, and the cancellation cascades to any
of *its* own dependents in turn. This reconcile is primarily **event-driven** —
triggered from the same choke point that detects job completion
(`checkJobCompletion` in `internal/scheduler/taskstatus.go`) and from the job
cancel and delete paths — with the periodic heartbeat sweep acting as a backstop pass over
all `blocked` jobs to catch anything missed by the event-driven path. Both
moves are single guarded store writes: `store.ReleaseBlockedJob` re-checks
every upstream inside the `UPDATE` that releases the job, and neither it nor
`store.CancelBlockedJob` touches a job that is no longer `blocked`, so a
reconcile can never undo a user's cancel.

### 2. Task readiness

After a job is created, the store marks tasks `ready` when their step's
dependencies are satisfied. For jobs with no step dependencies (the common
case), all tasks of the first step are immediately `ready`. Tasks in later
steps become `ready` only after all tasks in their dependency steps have
reached `succeeded`. A `blocked` job's steps and tasks skip this evaluation at
submit time and are all held `pending` regardless of step dependencies, until
the job is released and this same evaluation runs (see above).

For the initial set this evaluation happens **before** the write, not inside it:
`buildStepWithTasks` decides each step's and task's starting status while
expanding the template in memory, and the `CreateJobSubmission` transaction only
persists the statuses it already chose. It runs again via the scheduler's
`handleTaskTerminal` → `propagateStepDependencies` path whenever a task reaches a
terminal state, once its step has been finalized (`store.FinalizeStep`). Each
release is `store.ReleaseStep`, which moves a step that is still `pending`, and
its pending tasks, to `ready` in one guarded transaction; a failed or canceled
upstream step instead cancels its dependents through `store.CancelPendingStep`,
with the same guard plus a second one: the cancel re-checks, inside its own
transaction, that one of the step's upstream steps is still `failed` or
`canceled`. A retry that revives the upstream between the cascade's read of the
step list and its write therefore leaves the downstream step `pending`, to run
once the retried upstream completes. A release needs no such re-check, because
a `completed` step never reopens.

### 3. Assignment (lease-on-request)

Ready tasks stay `ready` until a worker asks for work. Workers keep exactly one
outstanding lease request per queue on `work.lease.<worker>.<queue>` (core-NATS
request/reply). When a request arrives the server:

```
handleLeaseRequest(workerID, queueID)
  │
  ├─ Refuse (empty batch, held ~1 s so the worker's loop cannot spin) if the worker is
  │     disabled, or the request comes from a worker process whose registration has not
  │     landed (its instance_id differs from the stored one, both non-empty)
  ├─ store.CommittedCores(workerID, worker.CPUCount) → committed (Σ required_cores of assigned+running tasks)
  ├─ free = worker.CPUCount − committed
  │     If free ≤ 0: park request in the per-queue waiter registry (~30 s hold)
  │
  ├─ store.ListReadyTasks(farmID, now, batchSize) → candidates, farm-wide, ordered
  │     job priority DESC, job created_at ASC, step order ASC, task created_at ASC
  │     (queue affinity is applied per-candidate by WorkerEligible, not by this query)
  │
  │  First-fit walk over candidates:
  ├─ effectiveCost = task.RequiredCores ?? worker.CPUCount; skip if it does not fit free
  ├─ Early filters, on values read a moment ago (cheap; not the decision):
  │     job not paused or terminal, policyGate (queue/farm MaxConcurrentTasks),
  │     WorkerEligible(task, worker) → capability/queue/farm/location/amounts and
  │     usage-pool room
  ├─ store.LeaseTask(task, worker, attempt ID, now, usage claims)
  │     One transaction (the decision):
  │       guarded ready→assigned (task still leasable), stamp assigned_at,
  │       re-check the queue's and farm's MaxConcurrentTasks,
  │       insert the running attempt (attempt_number = MAX + 1),
  │       per usage pool: re-read its max_concurrent, count, insert the claim
  │     Any outcome but Leased (Lost, QueueFull, FarmFull, PoolFull) writes
  │     nothing: the task is skipped and stays ready for the next request
  ├─ Leased: decrement free; add to batch
  │
  └─ bus.Reply(batch []AssignMsg)
         Each AssignMsg includes: task/job/step/attempt IDs, resolved OnRun action
         (command, args, timeout), embedded files, ordered environments, job and
         task parameters, path map + path deliveries, compute location, and the
         isolation identity (username only — see docs/auth.md#task-isolation).
         It does NOT carry a session_id: the session is created worker-side and
         travels back on the task-status message.
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
matching worker appears or the task leaves `ready`. The reason is written only
while the task is still `ready`, a condition evaluated inside the write itself,
so a task leased between the sweep's read and its write is never stamped. See
[`docs/observability.md`](observability.md#why-isnt-my-job-running--unschedulable-tasks)
for the operator-facing view and
[`scheduler.unschedulable_grace`](configuration.md#schedulerunschedulable_grace)
for the config knob.

### 4. Worker execution

```
sqi-worker
  │
  ├─ Keeps one outstanding lease request per queue (work.lease.<worker>.<queue>)
  │     Long-poll (~35 s timeout); re-issues immediately on return
  ├─ Receives batch of AssignMsgs from server
  ├─ Executes each task (spawns child process, manages lifecycle)
  ├─ Streams log chunks → NATS task.logs.<worker_id>.<task_id>
  └─ Reports status changes → NATS task.status.<worker_id>.<job_id>
         { task_id, attempt_id, status, exit_code, timestamp }
```

### 5. Status ingestion

```
NATS consumer (internal/scheduler/taskstatus.go)
  │
  ├─ Receive task.status message   { …, message }  ← worker's human-readable reason, if any
  ├─ Check the attempt exists, belongs to the task and is held by the subject's worker
  ├─ If running:
  │     store.StartTaskAttempt(attempt, task, session_id)  ← one transaction: acts only while the attempt is
  │                                           still running and is the task's latest and the task is
  │                                           assigned or running; moves assigned → running, records
  │                                           session_id. A stale report is acked and emits no event
  │     store.PromoteJobRunning(job_id)            ← only a pending job; never un-pauses or revives one
  ├─ If failed with message "worker_shutdown": store.ReclaimTaskAttempt(...)  ← a reclaim, not a failure:
  │                                           the task goes back to ready, no retry is consumed
  ├─ If failed: store.RecordTaskFailure(...), then retry, park or go terminal (see Auto-retry below)
  ├─ If terminal (succeeded/failed/canceled):
  │     store.CompleteTaskAttempt(report)   ← one transaction: close the attempt if it is still
  │                                           running, release its usage claims, then, only if
  │                                           it is still the task's latest attempt and the task is
  │                                           assigned or running, move the task (state-machine
  │                                           guarded) and stamp failure_reason
  │     notifier.NotifyTask(...)                          ← triggers WebSocket fanout
  │     checkStepCompletion: store.FinalizeStep → propagateStepDependencies → store.FinalizeJob
  └─ ack message (a refused transition is acked too; its claims were still released)
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
| Worker-reported failure/cancel | the worker's `Message` verbatim | `handleTaskTerminal`, stamped by `store.CompleteTaskAttempt` in the same transaction that moves the task, and only if the task ends up in the reported status |
| Failed with no worker message | `"failed (exit N)"` or `"failed"` | `handleTaskTerminal` fallback |
| Worker reclaimed (heartbeat timeout or graceful deregister) | `"worker went offline"` | `store.OfflineStaleWorker` / `store.OfflineWorker` — set on the **attempt** `message` only; reclaim is not a task failure, so `tasks.failure_reason` is left untouched |
| Worker restarted (a new process registers under the same worker ID) | `"worker restarted"` | `store.RegisterWorker`, in the registration's own transaction — attempt `message` only, as above |
| Worker shut down (a `failed` report whose message is `worker_shutdown`) | `"worker shut down"` | `store.ReclaimTaskAttempt` — attempt `message` only, as above |
| Cascade-canceled (an upstream step failed or was canceled, or a blocked job's upstream job failed, was canceled or was deleted) | `"canceled: upstream step failed"` | stamped inside the same UPDATE that cancels the tasks: `store.CancelPendingStep` (called by `openjd.CancelDependents`) for a step, `store.CancelBlockedJob` for a blocked job |
| User-initiated cancel | `"canceled by user"` | stamped inside the UPDATE that cancels the tasks: `store.CancelJobExecution` for `CancelJob`, `store.CancelTaskExecution` for `CancelTask` |

Both server-originated task reasons (the two cancel rows) are stamped only on a
task that has no reason yet, so a cascade-cancel's more specific reason always
wins regardless of ordering. No reason is a separate write after the status
change: each is part of the guarded write that moves the task (or, for the
reclaims, closes the attempt).

These server-originated reason strings are shared constants in `internal/store`
(`FailureReasonCanceledByUser`, `FailureReasonUpstreamFailed`,
`FailureReasonWorkerOffline`, `FailureReasonWorkerRestarted`,
`FailureReasonWorkerShutdown`) — `FailureReasonSummary` groups by exact string,
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
  ├─ Receive task.logs message on a JetStream push-consumer over SQI_LOGS
  ├─ store.CreateTaskLog(store.TaskLog{…})   ← the NATS stream sequence is
  │     persisted as the chunk's pagination cursor for the logs REST endpoint
  └─ notifier.NotifyLog(task_id, chunk)      ← triggers WebSocket fanout for live tail
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

The diagram shows the happy path only. The complete permitted set
(`internal/store/statemachine.go`) is:

| From | To | When |
|---|---|---|
| `pending` | `ready` | dependency resolution: all dependency steps completed |
| `pending` | `canceled` | job canceled before step dependencies were satisfied |
| `ready` | `assigned` | scheduler leases the task to a worker |
| `ready` | `canceled` | task canceled while waiting for a worker |
| `assigned` | `running` | worker confirms execution started |
| `assigned` | `ready` | reclaim: the assigned worker went offline, restarted or shut down, or the stale-assigned reaper fired |
| `assigned` | `canceled` | task canceled after assignment, before confirmation |
| `assigned` | `succeeded` / `failed` | the worker's `running` publish was lost (see below) |
| `running` | `succeeded` | worker reports clean exit (exit code 0) |
| `running` | `failed` | worker reports non-zero exit or a fatal error |
| `running` | `ready` | reclaim (worker offline, restarted or shut down) or auto-retry re-queue |
| `running` | `canceled` | task canceled while executing |

`succeeded`, `failed`, and `canceled` are terminal — no outgoing transitions.

Transitions are validated by `store.ValidateTaskTransition`
(`internal/store/statemachine.go`) and enforced, in both store implementations,
by the two writes that move a task on a worker's report: `StartTaskAttempt`
(the `running` report, which writes only `assigned` → `running`) and
`CompleteTaskAttempt` (a terminal report). Both are a compare-and-set: the
store reads the current status, checks the arrow, and writes the new status
only while the row still holds the status it read. `CompleteTaskAttempt`
validates the arrow against the table and re-reads a bounded number of times
if another writer moved the row in between. The check therefore cannot race a
concurrent writer on any store, not only under SQLite's single write
connection; the in-memory fake does the same under its mutex. A transition
outside the permitted set returns `store.ErrInvalidTransition` (a `Rejected`
result, from `CompleteTaskAttempt`) and leaves the task row unchanged.

**A worker's report moves a task only from the attempt the server still
considers live.** `StartTaskAttempt` acts only while the report's attempt is
still `running` and is the task's latest and the task is `assigned` or
`running`; otherwise it reports `started = false`, nothing is written, and the
consumer acks the message and emits no event. `CompleteTaskAttempt` always
closes the attempt and releases its claims, but moves the task only when the
attempt is the task's latest **and** the task is `assigned` or `running` (a
task already holding the reported status is a no-op). A task that went back to
`ready` or `pending`, or reached another terminal status, refuses the move, so
a canceled task's late `canceled` echo cannot re-cancel a task that was
retried in the meantime. `RequeueTaskForRetry` (the failure fork's requeue)
likewise acts only while its attempt is the task's latest, so a reclaim and a
new lease landing between `RecordTaskFailure` and the requeue leave the new
lease alone. `UpdateTaskStatus`, which these reports used to go through, is no
longer part of `store.Store`; both stores keep it only as a test fixture.

The one path the table does not describe is a manual retry: `RetryTasks`
revives `failed` and `canceled` tasks in its own guarded SQL, to `pending`, or
to `ready` when the task's step is itself `ready` (a sibling still in flight;
nothing would otherwise release a `pending` task from a step that is already
released). A step still recorded as the legacy `running` status is treated as
`ready` here.

**There are two state machines, in two packages, with two sentinel errors.**
The **task** machine is `store.ValidateTaskTransition` /
`store.ErrInvalidTransition` (`internal/store/statemachine.go`), enforced on
every task status write above. The **step** machine is
`openjd.ValidateStepTransition` / `openjd.ErrInvalidTransition`
(`internal/openjd/statemachine.go`), and beside the task machine sits a **job**
table, `store.JobTransitions`. Unlike the task machine, the step and job tables
are not consulted at run time: they are specifications that tests check. Every
step and job status write is a named store operation (`FinalizeStep`,
`ReleaseStep`, `CancelPendingStep`, `FinalizeJob`, `PromoteJobRunning`,
`PauseJob` and the rest) whose precondition is in its own SQL, and a
table-driven test per table asserts that each operation's from-states are legal
arrows there. Steps have no `running` status: nothing writes it, so a step goes
`pending` → `ready` → `completed`/`failed`/`canceled`, or straight from
`pending` to `canceled` when an upstream step fails or is canceled
(`CancelPendingStep`), its blocked job is canceled (`CancelBlockedJob`) or its
job is canceled (`CancelJobExecution`), and back to `pending` only on a retry.
There is deliberately no `pending` → `failed` arrow: the only way a pending
step could be judged failed is a retry interrupted before dependency
resolution. When the server stops in that window, the start-up reconcile
releases the step first (see *Upgrade repair* under
[What changed for operators](#what-changed-for-operators)); when only the store
call fails and the server stays up, the window stays open until the next start
(see [Known gaps](#known-gaps)). The value survives in the enum
and wire types, and a real running step would be a separate, user-visible
feature. The
task machine lives in `store` and not in `openjd` for a hard reason: `openjd`
imports `store`, so `store` can never import `openjd` back. Do not merge the
two sentinels — `errors.Is` against the wrong one silently stops matching.

Two rules keep enforcement safe given that task status arrives over JetStream
(at-least-once delivery):

- **Writing a task's current status is a no-op, not an error** — a redelivered
  message must not fail.
- **The consumer acks an invalid transition instead of Nak'ing it.** A message
  describing a state the task has already left cannot become legal on
  redelivery, so Nak'ing would loop forever. It is discarded with a warning,
  the same treatment a malformed payload gets.

Cancellation follows the same principle. `CancelTask` is one store call,
`store.CancelTaskExecution`, whose `UPDATE` cancels the task only while it is
non-terminal. A task that finished first, before the call or during it, is
reported as not canceled and nothing is written: canceling a completed task is
not an error, regardless of which side of the race the caller landed on. Other
store failures still propagate. A single-task cancel keeps the task's
`assigned_worker_id`, so a canceled task still shows the worker that held it; a
job-wide cancel (`store.CancelJobExecution`) clears it.

**A cancel finishes the work it leaves behind.** After `CancelTaskExecution`
reports a task canceled, `Scheduler.CancelTask` drives the same completion path
a terminal worker report does (`checkStepCompletion`: finalize the step,
propagate dependencies, finalize the job, reconcile cross-job dependents), so
canceling a step's last open task finishes the step and, if it was the last,
the job. The cancel has already committed, so an error from that path is
logged at Warn and not returned: the caller never sees a 500 for a cancel that
happened, and the start-up reconcile is the backstop. A job-wide cancel
finalizes in the store instead: `CancelJobExecution` finalizes every open step
of the job in the same transaction, last, after the tasks, attempts and claims
(a pending step, or one with no tasks, becomes `canceled`; any other gets
`FinalizeStep`'s outcome rule), and then cancels the job row itself, under the
same guard as `CancelJobStatus` (a completed, failed or already canceled job is
left as it is). A job cancel is therefore one write: no server stop, store error
or dropped HTTP request between two writes can leave a job live with all of its
work canceled, and a worker's late `canceled` echo finds the job already
terminal instead of finalizing it `failed` ahead of the cancel. The REST
handler's `CancelJobStatus` after it is an idempotent confirmation that still
reports a job that completed or failed first. After a job cancel every step is
terminal, so `RetryJob` on a canceled job works: the revived tasks sit under a
step that `RetryTasks` resets and `ResolveDependencies` releases.

Two arrows deserve note. `assigned` → `succeeded`/`failed` is permitted even
though it appears to skip `running`: the worker publishes `running` first, but
that publish is best-effort and gives up after `MaxRetries`, so it can be lost
while the task still runs to completion. Rejecting the terminal message would
strand finished work. Separately, the auto-retry re-queue described below
(`running` → `ready` on a transient failure) is a **policy-driven store call**
(`RequeueTaskForRetry`) with its own guarded SQL, as are the other paths that
move tasks for the server's own reasons: `LeaseTask`, `RetryTasks`,
`ReleaseStep` / `CancelPendingStep`, `CancelBlockedJob`, `CancelJobExecution` /
`CancelTaskExecution`, and the reclaim operations (`ReclaimStaleAssignedTasks`,
`OfflineStaleWorker`, `OfflineWorker`, `ReclaimTaskAttempt`, and the restart
reclaim inside `RegisterWorker`). None of them route through
`UpdateTaskStatus`, and each writes only the rows its own `WHERE` still
matches (see [Store invariants](#store-invariants)).

### Auto-retry on worker-reported failure

A worker-reported `failed` status no longer routes straight to a terminal
state. The scheduler (`internal/scheduler/failure.go`) resolves the effective
[retry policy](configuration.md#retry--failure-limits) (Job → Queue → Farm →
server default) and records the genuine failure via
`store.RecordTaskFailure`, then picks one of three outcomes:

- **Retry.** The task's genuine-failure count is still below its resolved
  `max_attempts` and the job has not hit its `failure_limit` — the attempt
  closes as failed, usage-pool claims are released, and the task re-enters
  `ready` (`store.RequeueTaskForRetry`) stamped with a `retry_after` backoff
  timestamp (`now + retry_delay`). The requeue acts only while the failed
  attempt is still the task's latest, so a reclaim and a new lease landing
  between the two writes are left alone. It does **not** go through the
  terminal / step-completion path, since the step isn't actually done.
- **Exhausted.** The task's genuine-failure count reaches `max_attempts` with
  no failure limit tripped — the task goes terminal-`failed`, cascading to its
  step and job exactly as before this feature.
- **Auto-park.** The *job's* cumulative genuine-failure count reaches its
  resolved `failure_limit` — the job is parked first (`store.ParkJob`, sets
  `status=paused` and a `park_reason` like `"failure limit reached (3)"`),
  then the tripping task still goes terminal-`failed` so the step/job
  completion cascade runs normally rather than leaving the job half-running.

**Lost/reclaimed work is separate and uncapped.** A task reclaimed because its
worker went offline (heartbeat timeout or graceful deregister), because its
worker process restarted and registered again, because its worker reported
`failed` with the message `worker_shutdown`, or because it lingered in
`assigned` past the stale-assigned reaper's timeout is simply returned to
`ready` — it never consumes any of the task's `max_attempts` and never counts
toward the job's `failure_limit` (a `worker_shutdown` report is routed to
`ReclaimTaskAttempt` before the failure policy is consulted). Only a *genuine*
worker-reported failure counts. A rolling restart of the farm's workers
therefore cannot park jobs, and a worker's `worker_shutdown` report and its
deregister end in the same task, claim and counter state whichever the server
applies first (only the closed attempt's `message` differs: `worker shut down`
or `worker went offline`, depending on which landed first).

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
`leaseGatesPass` step and, decisively, inside `store.LeaseTask`, whose guarded
`ready` → `assigned` write applies the same predicate as the list (backoff
elapsed, queue unpaused, job neither paused nor terminal), so a job paused
between the list and the lease leaves its task unleased. It also fixed
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
met land in `ready` immediately. The exception is a task whose step is still
`ready` (a sibling is in flight, so the step is not reset and nothing would
release the task from `pending`): it is revived straight to `ready`. A manual retry also resets the task's and
job's genuine-failure counters, independent of the automatic retry policy
above, and clears the task's `failure_reason` — both the automatic and manual
retry paths above leave a fresh task with no stale reason attached. See
[Durable failure reason](#5-status-ingestion) for how `failure_reason` is set
in the first place.

---

## Store invariants

Five rules hold across the store's rows. Each is enforced by **one store
operation**: the scheduler and the REST handlers never assemble one out of
several store calls, because a decision made on one call's answer can be stale
by the time the next call writes. They are written down here because a store
with genuinely concurrent writers, such as the planned PostgreSQL backend, has
to uphold them with row locks, where SQLite gets most of them from its single
write connection.

- **I1. No blind status writes.** Every status change states its precondition
  in its own `WHERE`. Zero rows affected means the world moved on: the
  operation reports a typed no-op (or `store.ErrConflict` where the API needs
  one, such as a pause of a job that has finished) and never overwrites. It is
  the rule task status already followed — writing a task's current status is a
  no-op, not an error — and a task status write driven by a worker report is a
  compare-and-set on the status read in the same transaction.
- **I2. A set-returning operation returns exactly the rows it changed.** It
  uses `UPDATE … RETURNING`. Where the caller needs the *pre*-update value,
  which SQLite's `RETURNING` cannot report (a job cancel clears the worker it
  must then signal), the transaction keeps a `SELECT` and takes its anchor lock
  before it.
- **I3. An active usage claim exists if and only if its attempt is open.**
  Whatever closes an attempt releases its claims in the same transaction: a
  terminal report (`CompleteTaskAttempt`), a recorded failure
  (`RecordTaskFailure`), a cancel (`CancelJobExecution`, `CancelTaskExecution`),
  a reap (`ReclaimStaleAssignedTasks`), a shutdown reclaim
  (`ReclaimTaskAttempt`) and an offline or restart reclaim
  (`OfflineStaleWorker`, `OfflineWorker`, `RegisterWorker`). A claim is inserted only by
  `LeaseTask`, in the transaction that assigns the task and opens the attempt,
  and a lease that does not complete writes nothing at all, so there is no
  rollback path to leak from.
- **I4. A derived decision is made inside the statement that writes it.** Step
  and job finalization (`FinalizeStep`, `FinalizeJob`), blocked-job release
  (`ReleaseBlockedJob`), the step-failure cascade (`CancelPendingStep`, which
  re-checks that an upstream step is failed or canceled), stalled-job demotion
  (`DemoteStalledJobs`) and the last-admin guard (`UpdateUserKeepingAdmin`, `DeleteUser`) compute their
  condition in the `UPDATE` or `DELETE` itself and report what they decided;
  `FinalizeStep`, which runs on the single write connection for every terminal
  report, answers its check of the step's tasks from the
  `tasks (step_id, status)` index (migration `00032`) rather than the task rows.
- **I5. A capacity check and its write are atomic.** `LeaseTask` re-checks the
  queue's and the farm's `max_concurrent_tasks` and each usage pool's
  `max_concurrent` at write time, against values read in its own transaction.
  The scheduler's earlier checks (`policyGate`, `WorkerEligible`) are cheap
  filters, not the decision.

Single-row rules are guarded the same way: `SetTaskUnschedulableReason` writes
only while the task is `ready` (otherwise a no-op), `PauseJob` only while the job is `pending` or `running`, and
`DeleteWorkerIfRemovable` only while the worker is still removable **and holds
no task**: the in-flight check (no task `assigned` or `running` on the worker)
is part of the `DELETE` statement itself.

### Anchor rows

Under PostgreSQL's default READ COMMITTED isolation, I1 and I2 hold as
written, because a guarded single-row `UPDATE` re-checks its predicate against
the row it locks. I3, I4 and I5 do not: their conditions read *other* rows.
Each operation that guards one of them therefore names **anchor rows** to lock
before it reads, by calling `lockAnchors` (`internal/store/sqlite/anchor.go`).

**On SQLite, I3–I5 hold because the write pool's single connection serializes
every write transaction; `lockAnchors` is a no-op there. A PostgreSQL store
must take the anchor locks to provide the same guarantee.** The anchors each
operation passes today:

| Operation | Anchor rows, in lock order | Notes |
|---|---|---|
| `LeaseTask` | the task's queue row (only if it has a cap), its farm row (only if it has a cap), then each requested usage-pool row, sorted by id | The queue and farm caps that decide whether those rows are anchored are read before the lock. No job row and no worker row (see below). |
| `CompleteTaskAttempt`, `RecordTaskFailure` | the task's job row | |
| `StartTaskAttempt`, `ReclaimTaskAttempt` | the task's job row | A PostgreSQL store must `SELECT … FOR UPDATE` the task row after the job anchor and **before** the latest-attempt check, as for `CompleteTaskAttempt` (see below). `ReclaimTaskAttempt` takes no worker row: it neither reads nor writes one. |
| `RequeueTaskForRetry` | none today (one guarded `UPDATE` whose predicate includes the latest-attempt check); a PostgreSQL store takes the job row first | Same: lock the task row `FOR UPDATE` before the latest-attempt check. |
| `CancelJobExecution`, `CancelTaskExecution` | the job row (the task's job, for a single task) | `CancelJobExecution` writes the job's steps after the tasks, attempts and claims, and the job row itself **last**; the step write touches only `steps`, which no lease or report writes, and the job row is the anchor this transaction already holds, so neither adds a lock-order inversion. |
| `RetryTasks` | the job row | |
| `FinalizeStep`, `FinalizeJob` | the job row (the step's job, for a step) | The current status, used only to report "already terminal", is read before the lock. |
| `ReleaseStep`, `CancelPendingStep` | the step's job row | |
| `ReleaseBlockedJob`, `CancelBlockedJob` | the job row, then each upstream job row sorted by id, the upstream rows shared (`FOR SHARE`) | The anchor type carries no lock mode yet; the call sites document the shared one. |
| `CreateJobSubmission` (dependency re-check) | each upstream job row, sorted by id, shared | As above. |
| `DeleteJob` | the job row | |
| `DeleteTerminalJobsBefore` (retention) | each candidate's job row, taken just before that job's eligibility re-check | Candidates come in the eligibility query's order, unsorted, and every lock is held to the sweep's one commit. |
| `ReclaimStaleAssignedTasks` | none today; a PostgreSQL store must take each candidate's job row, sorted by id, **before** the `UPDATE` (see below) | |
| `OfflineStaleWorker`, `OfflineWorker` | the worker row, then the job row of each of the worker's assigned or running tasks, sorted by id | |
| `RegisterWorker` | the worker row, then, only when the registration is from a new process, the job row of each of the worker's assigned or running tasks, sorted by id | `offlineWorker`'s order, so it inherits the gap described below (`LeaseTask` takes no worker row). |
| `UpdateUserKeepingAdmin`, `DeleteUser` (last-admin guard) | every enabled admin's user row, sorted by id | The admin set is read before the lock. |
| `DemoteStalledJobs` | **left for the PostgreSQL store to decide**: every lease takes the job row (contention on large jobs), or demotion goes per job under the job lock, or it stays cosmetic and self-heals on the job's next `running` report | |

### Statement order

Inside these transactions **the order of the statements is part of the
contract** on PostgreSQL. Every operation that cancels or reclaims tasks writes
the task rows **first**, and only then closes their attempts and releases
their claims: `CancelJobExecution`, `CancelTaskExecution`,
`ReclaimStaleAssignedTasks`, `ReclaimTaskAttempt`, `OfflineStaleWorker`,
`OfflineWorker` and the restart reclaim inside `RegisterWorker`. A
concurrent writer already holding one of those task rows then makes the task
`UPDATE` wait, and once that writer commits, READ COMMITTED gives the later
statements a fresh snapshot, so they see the attempt and claims it wrote.
Closing attempts first would miss an attempt a lease created in between, or
let a worker's report move a task after its attempt had been closed.

Whether an in-flight writer is waited for depends on the predicate. The
`UPDATE` waits only when its predicate matches the row as that writer found
it. A cancel matches any non-terminal task, so it waits for a lease moving the
task from `ready` to `assigned` and then cancels what the lease committed. The
reaper (`status = 'assigned'`, assigned before the cutoff) and the offline
reclaim (the worker's `assigned` or `running` tasks) do not match a `ready`
row, so they **skip** a task a lease is moving out of `ready` rather than wait
for it; what they wait for is a worker's report on a task they do match.

`CompleteTaskAttempt` and `RecordTaskFailure` run the other way round (close
the attempt, release its claims, then move the task; `CompleteTaskAttempt`
first checks that the attempt is still the task's latest and refuses the move
when it is not), so the claims are freed even when the task move is refused.
The two orders coexist only because these
operations lock the task's job row first, so they cannot interleave on the same
job; the reaper does not take that lock yet (see below). `LeaseTask` writes its
task row before any count, so that row lock orders it against a cancel of the
same task and every count after it includes the task itself; it inserts the
attempt before the claims that reference it.

### What the anchor table does not close yet

None of the following can happen on SQLite, where the single write connection
serializes every writer. A PostgreSQL store has to close each of them:

- **`LeaseTask` takes neither the task's job row nor the worker row.**
  `CancelJobExecution` and `CancelTaskExecution` read the tasks they report,
  with the worker each one held, under only the job-row anchor. A lease that
  commits between that read and the cancel's `UPDATE` is canceled, with its
  attempt and claims closed, so I3 holds, but it is not reported, and its
  worker never gets a cancel signal. Either `LeaseTask` also anchors the job
  row (per-job lease contention: the same trade-off as `DemoteStalledJobs`),
  or the reported set comes from the `UPDATE` itself. Likewise
  `OfflineStaleWorker`, `OfflineWorker` and `RegisterWorker`'s restart reclaim
  read the worker's in-flight jobs
  after locking the worker row, yet a lease can still hand that worker a task
  afterwards: if it commits before the reclaim, the reclaim takes the task
  under a job row it never locked; if it is still in flight, the reclaim skips
  the task and only the stale-assignment reaper recovers it. Either `LeaseTask`
  takes the worker row `FOR SHARE`, or the offline operation re-reads that set
  until it is stable.
- **`DeleteJob` and retention lock the job row, but a lease and a log append
  (`CreateTaskLog`, which runs without a transaction) do not.** Either can
  insert a child row after its table was cleared, so a later `DELETE` in the
  cascade (claims, logs, attempts, tasks, steps, dependency edges, then the
  job row) fails on a foreign key. The PostgreSQL store must retry the cascade
  on a foreign-key or deadlock error.
- **`ReclaimStaleAssignedTasks` must take its anchors before its `UPDATE`,
  without locking a task row first**: read the candidate tasks and their job
  IDs unlocked, lock the job rows sorted by id, then run the `UPDATE` re-guarded
  on the candidate IDs, `status = 'assigned'` and the cutoff, with
  `RETURNING`. Selecting the tasks `FOR UPDATE` first, or anchoring after the
  `RETURNING`, locks a task row before its job row, the reverse of every
  job-level operation. Without the job-row lock the reaper's task-then-attempt
  order can also deadlock against `CompleteTaskAttempt`'s attempt-then-task
  order.
- **Values read before the lock must be re-read under it**: `LeaseTask`'s queue
  and farm caps (a row read as uncapped is not anchored at all), the
  last-admin guard's admin set, and the status `FinalizeStep` and `FinalizeJob`
  report as "already terminal".
- **The latest-attempt check** of `CompleteTaskAttempt`, `StartTaskAttempt`,
  `ReclaimTaskAttempt` and `RequeueTaskForRetry` reads the task's attempts
  before anything holds the task row, and `LeaseTask` does not take the job
  row, so a lease committing between that check and the write would make a
  superseded report look current. Lock the task row (after the job row)
  before the check; for `RequeueTaskForRetry`, which takes no anchor on SQLite,
  take the job row first as well.
- **Upstream job rows want a shared lock**, which the anchor type cannot
  express yet, and **retention should lock its candidates in id order** (or
  commit per job).

### What changed for operators

The invariants changed some behaviour beyond fixing the races they close, and
the lifecycle and report fixes that followed them (H4a2) changed more. The
list is in two groups; the first is the invariants', the second H4a2's.

- A worker's late, echoed or redelivered report never rewrites an attempt that
  is already closed, whether the server closed it (a cancel, a reap, an
  offline reclaim) or an earlier delivery of the same report did. The attempt
  keeps the `status`, `ended_at` and `message` it was closed with, and its
  `exit_code` and `session_id` are not overwritten either. A report the state
  machine refuses still releases the attempt's usage claims, and the consumer
  acks it.
- A worker's late terminal report for an attempt that is no longer the task's
  latest (the attempt was reaped or its worker taken offline, and the task was
  leased again) is refused and acked the same way. It still closes that
  attempt and releases its claims, but it no longer ends the task under the new
  lease (v0.3.0 completed or canceled it while the new attempt was running).
- A submission with `depends_on` whose upstream job fails, is canceled or is
  deleted between the server's dependency check and the write is now rejected
  with 422, as one whose upstream had already ended that way is; v0.3.0 created
  it `blocked` and canceled it later through the dependency sweep.
- `SetTaskUnschedulableReason` on a task that is no longer `ready` is a no-op,
  not an error, so a task leased while the unschedulable sweep was deciding is
  never stamped. It reports `written=false`, and the sweep sends no task event
  for it.

And from the lifecycle and report fixes:

- Canceling a job's last open task, canceling a job, and retrying a failed
  task while a sibling is still in flight (its step is `ready`) all finish the
  step and job now, and
  `RetryJob` after a cancel runs the tasks again (see Cancellation above).
  Jobs already canceled get their open steps finalized on the first start
  after upgrade (migration `00033`).
- **Disabled is a flag, not a status.** `workers.status` is liveness only
  (`online`/`offline`), written by registration, deregistration and the
  heartbeat sweep; `workers.disabled` is the operator's decision, written only
  by the enable/disable endpoints (migration `00036` moved existing
  `disabled` rows to `online` with the flag set). Until then one column held
  both, and registration, deregistration and the sweep each overwrote
  `disabled` with `online` or `offline`. The API, the `status` filter and the
  worker gauge report the effective status (`disabled` while the flag is set);
  offline-worker retention never deletes a disabled worker.
- A **disabled** worker finishes the tasks it holds and is leased nothing new:
  its lease requests are parked for `leaseHoldTimeout` (30 s), as an idle
  worker's are, and answered with an empty batch, so a worker left disabled for
  days neither spins its request loop nor polls about once a second per queue.
  Enabling it (`POST /workers/{id}/enable`) wakes the parked requests through
  `Scheduler.WakeWorker`, and the woken request is leased work at once. Because
  no liveness write touches the flag, it stays disabled across reconnects,
  restarts and a graceful deregister. If it dies, the heartbeat sweep takes it
  offline and reclaims its tasks like any other worker, and it stays disabled.
  A worker is removable when it is offline, disabled or not;
  `DELETE /workers/{id}` also refuses a worker that still holds an assigned or
  running task with the same 409 as any worker that is not removable.
- A worker process that restarts within the heartbeat timeout no longer leaves
  its previous process's tasks `running`. Each worker process sends a random
  `instance_id` in every registration (additive and optional: no
  `ProtocolVersion` bump; an older worker sends none and is treated as before).
  When the stored one is non-empty and differs from a non-empty incoming one,
  the registration transaction reclaims the worker's assigned and running
  tasks. Migration `00034` adds the column, empty on existing rows, so the
  first registration after upgrade reclaims nothing. The consequence: a worker
  upgraded by `kill -9` or a crash (so no deregister) while it holds running
  tasks, and back within the heartbeat timeout, leaves those tasks `running`
  this one time, the very bug this fixes. An orderly SIGTERM upgrade is fine,
  because the old build deregisters and the deregister reclaims its tasks, so
  drain or stop workers gracefully before upgrading them. The lease request
  carries the same optional `instance_id`, and the scheduler refuses to serve a
  request whose instance differs from the stored non-empty one until the
  registration has landed (the registration travels through JetStream, the
  lease is core NATS request/reply, so a restarted process can ask for work
  first, and a task leased to it before its registration landed would be
  reclaimed by that registration while the process runs it). The refusal is
  held for `leaseRefusalDelay` (1 s), about as long as a registration takes to
  land.
- A late or redelivered deregister can no longer take a restarted worker
  offline. The deregister carries the same optional `instance_id`, and
  `OfflineWorker` writes nothing when it and the stored one are both non-empty
  and differ: the message comes from a process a later registration replaced,
  and applying it would mark the live process offline and reclaim the tasks it
  is running (the worker would then stay `offline`, since heartbeats never
  restore `online`, while it went on leasing). The server acks it and emits no
  event. An older worker sends no `instance_id`, and its deregister applies as
  before.
- A task interrupted by its worker shutting down (`failed` with the message
  `worker_shutdown`) no longer consumes a retry or counts toward the job's
  failure limit, whichever of the report and the deregister the server sees
  first.
- A late or duplicate worker report can no longer move a task that was
  reclaimed, re-leased, canceled or retried: a `running` report from a
  superseded or closed attempt is acked and discarded (no event, no job
  promotion), a terminal report moves the task only while it is assigned or
  running, and the failure fork's requeue acts only for the task's latest
  attempt.
- Orphaned attempts and their usage claims are closed on the first start after
  upgrade (migration `00035`).
- A submission that loses its `depends_on` upstream mid-submit gets the same
  422 message as one that fails the pre-check (`openjd: submit: depends_on job
  "<id>" already terminated unsuccessfully (<status>)` or `openjd: submit:
  depends_on job "<id>" not found`).

Timestamps are unchanged from v0.3.0, though the writes that stamp them moved.
A released claim's `released_at` is always server time. A terminal report
applied through `CompleteTaskAttempt` stamps the task row's `updated_at` with
server time and the attempt's `ended_at` with the worker's reported time. The
failure path still uses the worker's reported time: `RecordTaskFailure` stamps
the attempt's `ended_at` and the task's and job's `updated_at` with it, an
auto-retry requeue stamps the task's `updated_at` with it and computes
`retry_after` from it, and an auto-park stamps the job's `updated_at` with it.

**Upgrade repair.** A v0.3.0 database can hold damage these invariants now
prevent, and fixing the code does not undo it, so it is repaired in four
parts:

- Migration `00031_release_leaked_claims` releases every active claim whose
  attempt is missing or no longer running, or whose task is terminal (the I3
  checker's predicate), stamping `released_at` from SQLite's own clock. A
  healthy database comes out byte-for-byte unchanged, and its `Down` is a
  documented no-op, because a repair is not reversible.
- Migration `00033_finalize_canceled_job_steps` finalizes the open steps of
  every **terminal** job whose tasks are all terminal, by the rule
  `CancelJobExecution` now applies (a pending step, or one with no tasks,
  becomes `canceled`; any other gets `FinalizeStep`'s outcome). Until H4a2 a
  job cancel wrote its tasks and its job row but never its steps. A step with a
  task still in flight is never touched, steps of live jobs are left to the
  start-up reconcile below, and `Down` is a no-op.
- Migration `00035_close_orphan_attempts` closes, as `failed` with server time,
  every `running` attempt whose task is not in flight (finished, or back in
  `ready` or `pending`) or that is not its task's latest attempt, then
  releases every active claim whose attempt is no longer running (`00031`
  repaired claims only and could not reach these). A healthy database is
  unchanged and `Down` is a no-op.
- Every scheduler start runs two reconcile passes before the lease subscriber
  starts, both synchronous and with no bound on how much they repair, so a
  first start on a database with a large backlog does that work before it
  leases anything. On a healthy farm neither writes anything (the first is one
  query; the second also reads each job it lists).
  - `reconcileStuckSteps`: `store.ListStuckSteps` lists each non-terminal step
    that has at least one task, no non-terminal task, **and a job that is not
    itself terminal**, and each one goes through the normal completion path:
    `FinalizeStep`, dependency propagation, `FinalizeJob`, cross-job dependents
    and WebSocket events. That finalizes steps left stuck by the old 1,000-task
    page limit on step completion, and steps stranded by older releases when a
    single-task cancel took a job's last open task and no worker report
    followed. The job condition is what keeps a healthy farm free of writes:
    terminal jobs are the migration's, not this pass's.
  - `reconcilePendingSteps`: `store.ListJobIDsWithPendingSteps` lists every
    non-terminal, non-`blocked` job with at least one `pending` step, and each
    goes through `ResolveDependencies` (release what is releasable) and
    `CancelDependents` (cancel what can never run), then `FinalizeJob` if that
    left every step terminal. A retry commits its revived tasks and the step
    reset before dependency resolution runs, so a server stop in between used
    to leave a `pending` step nothing would ever release. A `blocked` job is
    excluded because its steps wait on another job, not on their own upstream
    steps.

### Known gaps

The invariants above and the lifecycle fixes after them do not close these.
The first group predates both (v0.3.0) and is left for a later change; the
second is residue of the lifecycle fixes themselves.

Pre-existing in v0.3.0:

- **An `offline` worker that keeps asking for work is still leased to.** The
  lease handler refuses a `disabled` worker and a restarted process whose
  registration has not landed, but it looks at no other status, so a worker
  the server has declared offline (a missed heartbeat window) that is in fact
  alive and keeps requesting work is served. Refusing it would starve it for
  good, since heartbeats never bring a worker back `online`. One case is
  closed: a request that parked while its worker was online, and is woken after
  the worker went offline (a graceful deregister or the heartbeat sweep), is
  answered empty rather than leased a task, so a departed process's last
  request no longer takes a task into a dead inbox for the stale-assignment
  reaper to recover. A live worker that hits this is refused once, and its
  next request is served.
- **Heartbeat timestamps compare as text.** SQLite stores timestamps as
  RFC3339Nano text, which mis-orders within a second (`"…:05Z"` sorts after
  `"…:05.5Z"`), so its heartbeat-staleness comparison is wrong below one second
  while the in-memory fake compares exactly. The fake's `ListStaleWorkers` also
  lists a worker that has never heartbeated as stale, where SQLite never does
  (pinned by `TestListStaleWorkers`; `OfflineStaleWorker` follows SQLite on
  both). Both are fixed-width-timestamp work, not part of the lifecycle fixes.
- **A coarse wall clock fails a store test.** On a host whose clock resolution
  is coarse (observed on Windows),
  `TestJobStore_CreateJobSubmission_StampsDistinctRowTimestamps` fails because
  two rows stamped in one tick are not distinct. It fails the same way at
  `main`.
- **A `LeaseTask` that commits is not always delivered.** If building the
  assignment payload fails after `LeaseTask` committed (a deterministic error:
  the job's template no longer parses, or its step is gone), that task stays
  `assigned` without being in the batch, and only the stale-assignment reaper
  returns it to `ready`. A failure of `LeaseTask` itself no longer has this
  effect: the tasks leased earlier in the batch are still delivered.
- **A job whose steps are all terminal but whose own status is not**, left by
  a server stop between a cancel cascade's commit (`CancelDependents`
  canceling the last open steps after an upstream step failed) and the
  `FinalizeJob` that follows it, is repaired by neither start-up reconcile
  pass: `reconcileStuckSteps` lists non-terminal steps and
  `reconcilePendingSteps` lists `pending` ones, and this job has neither.
  There is still no start-up repair for it. The paths that can leave it are
  those whose triggering request is not redelivered: REST `CancelTask` (which
  logs a completion error and moves on), the REST retry, and the start-up
  `reconcilePendingSteps` pass itself. When the cascade arrives through a
  worker's terminal report, the JetStream message is redelivered after the
  stop, and re-running `checkStepCompletion` (which propagates whenever the
  step is terminal) reaches `FinalizeJob` and repairs it. A user's job cancel
  no longer leaves this shape: `CancelJobExecution` cancels the job row in the
  same transaction that cancels its tasks and finalizes its steps.

Left by the lifecycle fixes:

- **A stale redelivered registration can reclaim a live process's tasks.** A
  registration message that is Nak'd and redelivered after the worker has
  restarted again can flip the stored `instance_id` back to the earlier
  process's value; the next registration, from the live process, then reads
  that as a restart and reclaims the live process's tasks. Closing it needs
  ordering metadata on the registration (a sequence or a timestamp), which the
  message does not carry.
- **A refused process re-requests about once a second per queue.** The refusal
  of an unregistered process's lease request is held for `leaseRefusalDelay`
  (1 s) precisely so the worker's lease loop does not spin, but the loop does
  ask again as soon as the empty reply arrives, so a worker whose registration
  never lands (the message was discarded, or two live processes share a worker
  ID) asks about once a second per queue for as long as that lasts, and gets no
  work. Such a process is no longer silent: once it has been refused 30 times
  in a row the server logs a Warn naming the worker and both instance IDs, at
  most once every 5 minutes per process. The request for an unknown worker ID
  is answered at once and has always had no such backoff. A disabled worker no
  longer does this: its requests are parked for the full lease hold.
- **A retry whose dependency resolution fails with the server up can still
  finalize a pending step `failed`.** The start-up reconcile closes the window
  for a server stop between `RetryTasks` and `ResolveDependencies`, but
  `RetryJob`/`RetryTask` return an error if `ResolveDependencies` fails after
  `RetryTasks` committed, leaving the step `pending` with a revived task and
  nothing to release it until the next start. Until then, canceling that task
  drives step completion, and `FinalizeStep`'s guard admits `pending`, so a
  step whose other task is still `failed` is finalized `failed` directly from
  `pending`, an arrow the step table deliberately lacks. Closing it fully means
  `FinalizeStep` refusing a `pending` step.
- **Removing a busy worker revokes its credential first.** `DELETE
  /workers/{id}` revokes the worker's broker credential before the guarded
  delete, deliberately (a decommissioned machine loses broker access along with
  its record), and the in-flight check lives in the delete. Going offline
  reclaims a worker's tasks in the same transaction, so this needs an offline
  worker that was leased a task afterwards (the lease path does not check
  liveness); such a worker loses its credential and receives the 409.

---

## NATS subjects and streams

| Subject pattern | Transport | Direction | Purpose |
|---|---|---|---|
| `work.lease.<worker_id>.<queue>` | Core NATS request/reply | worker → server (request); server → worker (reply) | Worker requests a batch of tasks; server replies with assignments or empty on timeout |
| `task.status.<worker_id>.<job_id>` | JetStream (`SQI_TASK`, MaxAge 24 h) | worker → server | Terminal and intermediate status updates |
| `task.logs.<worker_id>.<task_id>` | JetStream (`SQI_LOGS`, MaxAge 96 h) | worker → server | Log chunk delivery |
| `task.cancel.<task_id>` | JetStream (`SQI_CANCEL`, MaxAge 5 min) | server → worker | Cancellation signal; the worker holding the task interrupts the process |
| `worker.register.<worker_id>` | JetStream (`SQI_WORKER`, MaxAge 2 min) | worker → server | Registration at startup and on reconnect; carries the worker process's `instance_id` (a changed one reclaims the previous process's tasks) |
| `worker.heartbeat.<worker_id>` | JetStream (`SQI_WORKER`, MaxAge 2 min) | worker → server | Liveness heartbeat |
| `worker.deregister.<worker_id>` | JetStream (`SQI_WORKER`, MaxAge 2 min) | worker → server | Graceful departure; marks the worker offline without waiting for heartbeat timeout (a disabled worker stays disabled) and reclaims its tasks. Ignored when its `instance_id` names a process a later registration replaced |
| `worker.diag.<workerID>` | Core NATS (best-effort) | worker → server | Diagnostic log records |

Every worker → server subject carries the publishing worker's ID directly after
its class prefix. NATS permissions are static per credential and JetStream does
not stamp publisher identity onto a message, so this placement is what lets the
broker restrict a worker to its own traffic, and what lets the server recover
who published a message it received (`bus.ParseWorkerSubject`).

A queue-unaffiliated worker leases on the reserved queue token
`work.lease.<worker_id>._any` (`bus.WildcardQueueToken`).

**Transport security.** Every subject above crosses the broker's client
listener, which is **plaintext unless `nats.tls` is configured** — see
[`docs/tls.md`](tls.md). That matters most for `work.lease.*`, whose reply
carries the whole `AssignMsg`: command lines, embedded file contents, job
parameters, environment and the run-as-user name. Broker *authentication*
(`nats.auth`, [`docs/auth.md`](auth.md)) is a separate setting: it decides who
may connect and constrains which subjects they may use, but it does not encrypt
anything. The server's own connections to the embedded broker never touch this
listener at all — they run in-process over a pipe.

JetStream streams use file-backed storage with configurable size limits.
`work.lease.<worker_id>.<queue>` uses core NATS request/reply — no stream is created for
it. The server holds an unfulfillable request in memory for up to 30 s before
replying with an empty batch; the worker re-requests immediately. A disabled
worker's request is held the same way (enabling the worker wakes it). A request
from a worker process whose registration has not landed yet is refused with an
empty batch, but only after holding it for about a second, so the worker's
request loop cannot spin.

---

## SQLite schema overview

Full DDL lives in `internal/store/migrations/`. The primary tables are:

| Table | Purpose |
|---|---|
| `farms` | Top-level organizational unit; holds default scheduling policy |
| `queues` | Belongs to a farm; jobs are submitted to a queue |
| `jobs` | One row per submitted job; holds verbatim OpenJD template |
| `job_dependencies` | Cross-job `depends_on` edges; gates dependents in `blocked` |
| `steps` | One row per step in a job; tracks dependency graph |
| `tasks` | Atomic work unit; one per expanded parameter combination |
| `task_attempts` | One row per execution attempt; holds exit code, timing, session_id |
| `task_logs` | Persisted task process output |
| `workers` | Registered workers with capabilities and status |
| `products` | Product/preset catalog above OpenJD (built-in, installed, custom) |
| `compute_locations` | Named compute-location registry (curated catalog; auto-populated from worker registrations) |
| `storage_locations` | Named storage locations with per-compute-location root mappings |
| `usage_pools` | Named concurrency limits (usage pools) with `max_concurrent` cap |
| `usage_claims` | Active usage claims tied to task attempts |
| `audit_log` | Append-only log of state-changing API operations |
| `users` | Local and external accounts: role, `auth_source`, `external_id` (opt-in auth only) |
| `sessions` | Server-side browser sessions backing the session cookie |
| `api_keys` | Hashed API-key credentials with owner and revocation state |

The last three exist regardless of whether auth is enabled — the migrations
always run — but stay empty and unread until `auth.enabled` is true. See
[`docs/auth.md`](auth.md).

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
- **Authentication.** `SqiClient` takes a `token` argument, falling back to
  `$SQI_TOKEN` then `$SQI_API_KEY`, and sends it as `Authorization: Bearer` on
  every request and on the WebSocket upgrade. A `headers` mapping is still
  accepted and is merged in afterwards, so an explicit header wins over the
  resolved token — useful for a gateway that expects its own scheme. A 401 or
  403 raises `SqiAuthError`. The credential to use is an API key issued from the
  web UI or `POST /api/v1/api-keys`; see [`docs/auth.md`](auth.md).

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
