# sqi-client (Python client API)

`sqi-client` is the pure-Python library for driving a `sqi-server` instance from
scripts and pipeline tooling — the same operations the web UI performs, over the
REST API (with an optional WebSocket extra for live events). Per the roadmap
([`../ROADMAP.md`](../ROADMAP.md)) it is the foundation for the Phase 2 DCC
submitters and for pipeline automation.

This is the reference document. For the project overview and install, see
[`clients/python/README.md`](../clients/python/README.md); the authoritative wire
contract is the OpenAPI spec described in [`api.md`](./api.md).

- Import name: `sqi_client` · distribution: `sqi-client`
- Python 3.9+ · sync-only in Phase 1 · core dependency `httpx` only
- Optional extras: `yaml` (PyYAML), `ws` (websockets)

## Contents

- [Installation](#installation)
- [Client construction and configuration](#client-construction-and-configuration)
- [Error handling](#error-handling)
- [Pagination: `Page` vs `iter_*`](#pagination-page-vs-iter_)
- [Submitting jobs](#submitting-jobs)
- [Querying and managing jobs](#querying-and-managing-jobs)
- [Tasks and logs](#tasks-and-logs)
- [Workers](#workers)
- [Farm, queue, and resource CRUD](#farm-queue-and-resource-crud)
- [Live events (WebSocket, `ws` extra)](#live-events-websocket-ws-extra)
- [Conveniences](#conveniences)

## Installation

```sh
pip install sqi-client            # core (httpx only)
pip install 'sqi-client[yaml]'    # + PyYAML
pip install 'sqi-client[ws]'      # + websockets (live event streaming)
```

Until the package is published to PyPI, install the wheel attached to a GitHub
release — see [`clients/python/README.md`](../clients/python/README.md#installation).

## Client construction and configuration

`SqiClient` is constructed from the server's root URL (no `/api/v1` suffix — the
client adds it). It owns an `httpx.Client` connection pool and works as a context
manager or with an explicit `close()`.

```python
from sqi_client import SqiClient

with SqiClient("http://localhost:8080") as sqi:
    print(sqi.ping())   # True if /healthz is OK
    print(sqi.ready())  # True if /readyz is OK (SQLite + NATS reachable)
```

Constructor options:

| Argument | Default | Purpose |
|---|---|---|
| `base_url` | — | Server root, e.g. `http://localhost:8080`. A subpath is preserved (reverse-proxy friendly). |
| `timeout` | `30.0` | Per-request timeout in seconds. |
| `headers` | `None` | Extra default headers merged into every request — the Phase 3 auth hook. Caller headers win on conflict. |
| `max_attempts` | `3` | Total attempts for a retried (idempotent GET) request; `1` disables retries. |
| `retry_backoff` | `0.5` | Base seconds for exponential backoff between retries. |
| `retry_backoff_max` | `30.0` | Cap on the computed backoff delay. |
| `retry_jitter` | `True` | Randomize each backoff delay in `[0, computed]`. |

Only idempotent `GET` requests are retried (on connection errors, 5xx, and 429 —
honoring `Retry-After`). `POST`/`PATCH`/`PUT`/`DELETE` are never retried. Each
request logs at `DEBUG` and each retry at `WARNING` on the `sqi_client` logger
(no handler is attached — standard library etiquette).

```python
sqi = SqiClient(
    "https://farm.studio.example",
    timeout=15.0,
    headers={"Authorization": "Bearer …"},  # forward-compatible auth hook
    max_attempts=5,
)
```

## Error handling

Every error derives from `SqiError`, so a single `except` can catch any client
failure:

```
SqiError
├── SqiConnectionError      # server unreachable (DNS, refused, reset)
├── SqiTimeoutError         # request timed out / wait_for_job deadline
└── APIError                # any non-2xx response
    ├── BadRequestError     # 400
    ├── NotFoundError       # 404
    ├── ConflictError       # 409
    ├── ValidationError     # 422
    ├── RateLimitError      # 429 (see .retry_after)
    └── ServerError         # 5xx
```

`APIError` carries `status`, `title`, `detail`, and `request_id` (parsed from the
RFC 7807 `application/problem+json` body or the `X-Request-Id` header). `str(exc)`
includes the status, detail, and request ID so failures are diagnosable in logs.

```python
from sqi_client import NotFoundError, ValidationError

try:
    job = sqi.get_job("does-not-exist")
except NotFoundError as exc:
    print(exc.status, exc.detail, exc.request_id)

try:
    sqi.submit_job(bad_template, farm_id=f, queue_id=q)
except ValidationError as exc:
    print(exc.detail)  # e.g. "step 'Render' references undefined parameter 'Frames'"
```

## Pagination: `Page` vs `iter_*`

List endpoints come in two flavors:

- `list_*(...)` returns a single `Page[T]` mirroring the server's
  `{items, total, limit, offset}` wrapper.
- `iter_*(...)` returns a lazy iterator that walks every page for you, advancing
  `offset` until the result set is exhausted.

```python
page = sqi.list_jobs(status="running", limit=50)
print(page.total, len(page.items))

for job in sqi.iter_jobs(status="failed"):   # fetches pages on demand
    print(job.id, job.name)
```

Status filters accept either the enum or its wire string:
`sqi.list_jobs(status=JobStatus.RUNNING)` and `sqi.list_jobs(status="running")`
are equivalent. `None` filters are omitted from the request entirely.

> **Note:** farms, storage locations, and usage pools are returned by the
> server as bare arrays (no pagination), so their `list_*` methods return a plain
> `list[T]`. Only queues, jobs, tasks, and workers are paginated.

## Submitting jobs

`submit_job(template, *, farm_id, queue_id, owner=None, priority=None, project=None) -> Job`

The template may be a `str` (sent verbatim), a `pathlib.Path` (read from disk),
or a `dict` (serialized to JSON). The `Content-Type` is chosen automatically —
`application/json` for dicts and JSON-parseable strings/files, `application/x-yaml`
otherwise (and always for `.yaml`/`.yml` paths). Serializing a dict needs only
the standard library; the `yaml` extra is **not** required to submit.

```python
from pathlib import Path

# From a file on disk:
job = sqi.submit_job(Path("render.yaml"), farm_id=farm_id, queue_id=queue_id)

# From a dict built in code:
job = sqi.submit_job(
    {"specificationVersion": "jobtemplate-2023-09", "name": "My Job", "steps": [...]},
    farm_id=farm_id, queue_id=queue_id, owner="alice", priority=90,
)
```

A `422` from the server (OpenJD validation failure) is raised as
`ValidationError` with the server's `detail` preserved verbatim, so a submitter
can show it to the artist.

## Querying and managing jobs

| Method | Description |
|---|---|
| `list_jobs(*, status, farm_id, queue_id, owner, project, sort_by, sort_dir, limit, offset) -> Page[Job]` | One page of jobs. |
| `iter_jobs(...) -> Iterator[Job]` | Same filters, auto-paged. |
| `get_job(job_id) -> Job` | Detailed job with `steps` and `task_counts`; 404 → `NotFoundError`. |
| `pause_job(job_id) -> Job` / `resume_job(job_id) -> Job` | Pause/resume; returns the updated job. |
| `set_job_priority(job_id, priority) -> Job` | Validates `priority >= 1` client-side before sending. |
| `cancel_job(job_id) -> None` | POST `/jobs/{id}/cancel`; returns `None` on `204`. |
| `retry_job(job_id) -> RetryJobResult` | POST `/jobs/{id}/retry`; revives all `failed`/`canceled` tasks; returns `RetryJobResult(job_id, retried)`. |
| `delete_job(job_id) -> None` | DELETE; permanently deletes the job and all data (cancels active tasks first); returns `None` on `204`. |

```python
job = sqi.get_job(job_id)
print(job.status, job.task_counts.succeeded, "/", job.task_counts.total)

sqi.pause_job(job_id)
sqi.set_job_priority(job_id, 75)
sqi.resume_job(job_id)
sqi.cancel_job(job_id)   # soft-cancel; job data is retained
result = sqi.retry_job(job_id)   # revive all failed/canceled tasks; result.retried = count
sqi.delete_job(job_id)   # hard-delete; all data permanently removed
```

## Tasks and logs

| Method | Description |
|---|---|
| `list_job_tasks(job_id, *, status, sort_by, sort_dir, limit, offset) -> Page[Task]` | One page of a job's tasks. |
| `iter_job_tasks(job_id, ...) -> Iterator[Task]` | Auto-paged companion. |
| `get_task(task_id) -> Task` | A single task. |
| `retry_task(task_id) -> RetryResult` | Retry a failed/canceled task; revives the task, its step, and the job (when terminal); response `status` is `ready` or `pending` depending on step dependencies; invalid state → `ConflictError`. |
| `cancel_task(task_id) -> CancelResult` | Cancel a non-terminal task; terminal task → `ConflictError`. |
| `get_task_logs(task_id, limit=100, after_nats_seq=0) -> LogPage` | One page of log chunks plus the cursor. |
| `tail_task_logs(task_id, poll_interval=1.0, from_seq=0, follow=True) -> Iterator[LogChunk]` | Polling log tail. |

`get_task_logs` returns a `LogPage` whose `after_nats_seq` is the cursor to pass
on the next call to fetch only newer chunks:

```python
cursor = 0
while True:
    page = sqi.get_task_logs(task_id, after_nats_seq=cursor)
    for chunk in page.items:
        print(chunk.data, end="")
    if not page.items:
        break
    cursor = page.after_nats_seq
```

`tail_task_logs` does this for you, yielding each chunk in order and sleeping
`poll_interval` between empty polls. With `follow=True` (the default) it stops
once the task reaches a terminal state and the final chunks have been drained;
with `follow=False` it stops at the current end of the log.

```python
for chunk in sqi.tail_task_logs(task_id, follow=True):
    print(chunk.data, end="")
```

## Workers

| Method | Description |
|---|---|
| `list_workers(*, farm_id, queue_id, compute_location, status, sort_by, sort_dir, limit, offset) -> Page[Worker]` | One page of workers. |
| `iter_workers(...) -> Iterator[Worker]` | Auto-paged companion. |
| `get_worker(worker_id) -> Worker` | Worker detail, including `current_tasks`. |
| `disable_worker(worker_id) -> WorkerAction \| None` | Drain and stop new assignments. |
| `enable_worker(worker_id) -> WorkerAction \| None` | Re-enable a disabled worker. |

```python
for worker in sqi.iter_workers(status="online"):
    print(worker.hostname, worker.status)

action = sqi.disable_worker(worker_id)   # WorkerAction(id=..., status=DISABLED)
sqi.enable_worker(worker_id)
```

`disable_worker`/`enable_worker` return a lightweight `WorkerAction` (`id`,
`status`) — the server's response shape — rather than a full `Worker`.

## Farm, queue, and resource CRUD

Each resource family exposes the standard create/list/get/update/delete set.
`update_*` is a full replace (PUT semantics): a field left at its default is
reset, so pass every field you want to keep.

| Family | Create | List | Get / Update / Delete |
|---|---|---|---|
| Farms | `create_farm(*, name, description=None, max_concurrent_tasks=0)` | `list_farms() -> list[Farm]`, `iter_farms()` | `get_farm`, `update_farm`, `delete_farm` |
| Queues | `create_queue(*, farm_id, name, description=None, priority=0, max_concurrent_tasks=0, paused=False)` | `list_queues(*, farm_id, paused, sort_by, sort_dir, limit, offset) -> Page[Queue]`, `iter_queues(...)` | `get_queue`, `update_queue`, `delete_queue` |
| Storage locations | `create_storage_location(*, name, type, description=None, roots=None)` | `list_storage_locations() -> list[StorageLocation]`, `iter_storage_locations()` | `get_storage_location`, `update_storage_location`, `delete_storage_location` |
| Usage pools | `create_usage_pool(*, name, max_concurrent, server_hint=None)` | `list_usage_pools() -> list[UsagePool]`, `iter_usage_pools()` | `get_usage_pool`, `update_usage_pool`, `delete_usage_pool` |

```python
farm = sqi.create_farm(name="studio-a", description="Studio A render farm")
queue = sqi.create_queue(farm_id=farm.id, name="renders", priority=50, max_concurrent_tasks=200)
loc = sqi.create_storage_location(
    name="project-root", type="filesystem", roots={"on-prem": "/mnt/farm"}
)
pool = sqi.create_usage_pool(name="arnold-pool", max_concurrent=10)
```

`create_usage_pool`/`update_usage_pool` validate `max_concurrent >= 1`
client-side (raising `ValueError`) before sending. Every `UsagePool` response
also carries read-only, server-computed `in_use` (active claims) and
`available` (`max(max_concurrent - in_use, 0)`) fields for live utilization.

## Live events (WebSocket, `ws` extra)

With `pip install 'sqi-client[ws]'`, `SqiClient.events()` opens a live event
stream over `GET /api/v1/ws`. The stream is a context manager; subscribe to one
or more subjects and iterate `Event` objects. It tracks the last `seq` per
subject and auto-reconnects + resubscribes (resuming from that `seq`) across an
idle disconnect or server restart.

```python
with sqi.events() as stream:
    stream.subscribe("workers")
    stream.subscribe(f"jobs/{job_id}/tasks")
    for event in stream:
        print(event.subject, event.seq, event.payload)
```

Subjects: `jobs`, `jobs/{job-id}/tasks`, `tasks/{task-id}/logs`, `workers`
(see [`api.md`](./api.md#available-subjects) for payload shapes). A failed
subscription or server `error` frame is raised as `SqiError`.

`tail_task_logs_live(task_id, from_seq=0) -> Iterator[LogChunk]` is the
WebSocket-backed counterpart to `tail_task_logs`:

```python
for chunk in sqi.tail_task_logs_live(task_id):
    print(chunk.data, end="")
```

Calling `events()`/`tail_task_logs_live` without the `ws` extra installed raises
`ImportError` naming the exact remedy (`pip install 'sqi-client[ws]'`). The live
`tasks/{id}/logs` payload omits `id`/`nats_seq`/`received_at`, so those are
zero-valued on the yielded `LogChunk`; `seq_num` and the content fields are set.

## Conveniences

`wait_for_job(job_id, poll_interval=2.0, timeout=None) -> Job` polls `get_job`
until the job reaches a terminal status (`completed`, `failed`, `canceled`),
returning the final `Job`. It raises `SqiTimeoutError` if `timeout` elapses
first. Note that `paused` is **not** terminal — pass a `timeout` if a job might
be paused indefinitely.

`submit_and_wait(template, *, farm_id, queue_id, owner=None, priority=None, project=None, poll_interval=2.0, timeout=None) -> Job`
composes `submit_job` + `wait_for_job` for the simplest pipeline script:

```python
from pathlib import Path

job = sqi.submit_and_wait(
    Path("render.yaml"), farm_id=farm_id, queue_id=queue_id, timeout=3600
)
if job.status != JobStatus.COMPLETED:   # compare by value, not identity
    raise SystemExit(f"job {job.id} ended as {job.status}")
```
