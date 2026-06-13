# sqi-server REST API

This document describes how to use the `sqi-server` REST API, with worked
examples for the most common operations. The machine-readable contract is the
OpenAPI 3.1 specification served by a running server at:

```
GET /api/v1/openapi.yaml
```

You can browse it with any OpenAPI viewer (e.g. Swagger UI, Redoc, or
[`scalar`](https://scalar.com/)).

---

## Base URL

All REST endpoints share the prefix `/api/v1`. A locally-running server
listens on `http://localhost:8080` by default.

```
BASE=http://localhost:8080/api/v1
```

---

## Conventions

### Content-Type

Submit requests as `application/json` or `application/x-yaml` (for OpenJD
templates). Responses are always `application/json`.

### Error responses

All errors use the [RFC 7807](https://www.rfc-editor.org/rfc/rfc7807)
problem-details format with `Content-Type: application/problem+json`:

```json
{
  "type": "about:blank",
  "title": "Not Found",
  "status": 404,
  "detail": "job not found",
  "instance": "a1b2c3d4e5f60708"
}
```

The `instance` field contains the request ID, which also appears in the
`X-Request-Id` response header — useful when correlating with server logs.

### Rate limiting

`/api/v1` requests are rate-limited per client IP (default token bucket: 20
requests per second sustained, burst of 40). Exceeding the limit returns a
`429 Too Many Requests` problem-details body and a delta-seconds
`Retry-After` header indicating when the next request will be accepted:

```
HTTP/1.1 429 Too Many Requests
Content-Type: application/problem+json
Retry-After: 1
```

Clients should back off for at least the advertised duration before retrying.

### Pagination

List endpoints accept `limit` (default 50, max 1000) and `offset`
(zero-based) query parameters and return a wrapper object:

```json
{
  "items": [ ... ],
  "total": 1234,
  "limit": 50,
  "offset": 0
}
```

### Versioning

The URL prefix is the API contract version, and every response under
`/api/v1` carries a matching `X-API-Version: 1` header. This contract major
is deliberately decoupled from the product release version (e.g. sqi 0.1,
0.2, … all serve contract `1`). Additive changes — new endpoints, new
optional fields, new enum values — do not change it; clients must tolerate
unknown fields and values.

A breaking change ships as a new URL prefix (`/api/v2/…`) with
`X-API-Version: 2`. During the migration window the old prefix keeps working
and its responses carry the [RFC 8594](https://www.rfc-editor.org/rfc/rfc8594)
deprecation headers:

| Header | Value |
|---|---|
| `Deprecation` | HTTP-date the deprecation was declared, or `true` |
| `Sunset` | HTTP-date the endpoint will be removed (optional) |
| `Link` | `<url>; rel="deprecation"` — migration documentation (optional) |

Clients should warn when they see a `Deprecation` header or an
`X-API-Version` major newer than the one they were written against.

---

## Worked examples

The examples below use `curl`. Replace `$BASE` with `http://localhost:8080/api/v1`
or export it:

```sh
export BASE=http://localhost:8080/api/v1
```

---

### Submit a job

`POST /api/v1/jobs`

The body is a raw OpenJD template in YAML or JSON. Required query parameters:

| Parameter | Description |
|---|---|
| `farm_id` | UUID of the target farm |
| `queue_id` | UUID of the target queue |

Optional query parameters: `owner`, `priority` (default 50), `project`.

```sh
curl -s -X POST "$BASE/jobs?farm_id=FARM_ID&queue_id=QUEUE_ID&owner=alice" \
  -H "Content-Type: application/x-yaml" \
  --data-binary @job.yaml | jq .
```

Successful response — `201 Created`:

```json
{
  "id": "018f1a2b-3c4d-7e5f-a6b7-c8d9e0f12345",
  "farm_id": "...",
  "queue_id": "...",
  "name": "My Render Job",
  "owner": "alice",
  "submitter": "",
  "priority": 50,
  "status": "pending",
  "template_format": "yaml",
  "created_at": "2026-01-15T10:00:00Z",
  "updated_at": "2026-01-15T10:00:00Z"
}
```

Validation failure — `422 Unprocessable Entity`:

```json
{
  "type": "about:blank",
  "title": "Unprocessable Entity",
  "status": 422,
  "detail": "step 'Render' references undefined parameter 'Frames'",
  "instance": "a1b2c3d4e5f60708"
}
```

---

### List jobs

`GET /api/v1/jobs`

Filter parameters (all optional):

| Parameter | Values | Description |
|---|---|---|
| `status` | `pending`, `running`, `completed`, `failed`, `canceled`, `paused` | Filter by job status |
| `queue_id` | UUID | Filter by queue |
| `farm_id` | UUID | Filter by farm |
| `owner` | string | Filter by owner |
| `project` | string | Filter by project |
| `sort_by` | `created_at`, `priority`, `status`, `updated_at`, `name` | Sort field (default: `created_at`) |
| `sort_dir` | `asc`, `desc` | Sort direction (default: `asc`) |
| `limit` | 1–1000 | Page size (default: 50) |
| `offset` | ≥0 | Page offset (default: 0) |

```sh
# List running jobs, newest first
curl -s "$BASE/jobs?status=running&sort_by=created_at&sort_dir=desc" | jq .

# Page through all failed jobs
curl -s "$BASE/jobs?status=failed&limit=10&offset=0" | jq .items[].id
```

---

### Get job detail

`GET /api/v1/jobs/{id}`

Returns the job with its steps and per-status task counts.

```sh
JOB_ID=018f1a2b-3c4d-7e5f-a6b7-c8d9e0f12345

curl -s "$BASE/jobs/$JOB_ID" | jq '{name, status, task_counts}'
```

```json
{
  "name": "My Render Job",
  "status": "running",
  "task_counts": {
    "total": 100,
    "pending": 0,
    "ready": 10,
    "assigned": 5,
    "running": 5,
    "succeeded": 80,
    "failed": 0,
    "canceled": 0
  }
}
```

---

### List tasks for a job

`GET /api/v1/jobs/{id}/tasks`

Accepts `status` (`pending`, `ready`, `assigned`, `running`, `succeeded`, `failed`, `canceled`),
`limit`, `offset`, `sort_by` (`created_at`, `status`, `updated_at`, `name`), and `sort_dir` filters.

```sh
# Show failed tasks for a job
curl -s "$BASE/jobs/$JOB_ID/tasks?status=failed" | jq .items[]
```

---

### Cancel a job

`DELETE /api/v1/jobs/{id}`

Cancels the job and propagates cancel signals to all assigned workers.

```sh
curl -s -X DELETE "$BASE/jobs/$JOB_ID"
# Returns 204 No Content on success
```

---

### Pause and resume a job

`PATCH /api/v1/jobs/{id}`

```sh
# Pause
curl -s -X PATCH "$BASE/jobs/$JOB_ID" \
  -H "Content-Type: application/json" \
  -d '{"action": "pause"}'

# Resume
curl -s -X PATCH "$BASE/jobs/$JOB_ID" \
  -H "Content-Type: application/json" \
  -d '{"action": "resume"}'

# Raise priority
curl -s -X PATCH "$BASE/jobs/$JOB_ID" \
  -H "Content-Type: application/json" \
  -d '{"priority": 90}'
```

---

### Tail task logs (polling)

`GET /api/v1/tasks/{id}/logs`

Logs are returned as a paginated list of timestamped chunks. Advance the
cursor using `after_nats_seq` from each response.

| Parameter | Default | Description |
|---|---|---|
| `limit` | 100 | Chunks per page |
| `after_nats_seq` | 0 | Return only chunks with NATS sequence > this value |

```sh
TASK_ID=<task-uuid>
CURSOR=0

# Poll loop
while true; do
  RESP=$(curl -s "$BASE/tasks/$TASK_ID/logs?limit=100&after_nats_seq=$CURSOR")
  echo "$RESP" | jq -r '.items[].data'
  CURSOR=$(echo "$RESP" | jq -r '.after_nats_seq')
  sleep 1
done
```

Each item in `.items[]` has:

```json
{
  "id": "...",
  "task_id": "...",
  "attempt_id": "...",
  "seq_num": 42,
  "nats_seq": 1001,
  "stream": "stdout",
  "data": "Frame 0042 rendered in 3.2s\n",
  "at": "2026-01-15T10:05:42.123Z",
  "received_at": "2026-01-15T10:05:42.130Z"
}
```

---

### Tail task logs (WebSocket)

For live streaming, use the WebSocket endpoint instead of polling:

```
GET /api/v1/ws   (Upgrade: websocket)
```

See the [WebSocket subscription protocol](#websocket-subscriptions) section
below for message format details.

Quick example using [`websocat`](https://github.com/vi/websocat):

```sh
TASK_ID=<task-uuid>

websocat ws://localhost:8080/api/v1/ws <<EOF
{"type":"subscribe","subject":"tasks/$TASK_ID/logs","since_seq":0}
EOF
```

---

### Retry a failed task

`POST /api/v1/tasks/{id}/retry`

Creates a new attempt for a task in `failed` or `canceled` state.

```sh
curl -s -X POST "$BASE/tasks/$TASK_ID/retry" | jq .
```

---

### Worker endpoints

```sh
# List workers
curl -s "$BASE/workers" | jq '.items[] | {id, hostname, status}'

# Disable a worker (drains current task, stops new assignments)
curl -s -X POST "$BASE/workers/$WORKER_ID/disable"

# Re-enable
curl -s -X POST "$BASE/workers/$WORKER_ID/enable"
```

---

### Farm, queue, and resource CRUD

Each resource supports `GET /` (list), `POST /` (create), `GET /{id}`,
`PUT /{id}` (full replace), and `DELETE /{id}`.

```
/api/v1/farms
/api/v1/queues
/api/v1/storage-locations
/api/v1/license-pools
```

Example — create a farm, then a queue inside it:

```sh
FARM=$(curl -s -X POST "$BASE/farms" \
  -H "Content-Type: application/json" \
  -d '{"name":"studio-a","description":"Studio A render farm"}')
FARM_ID=$(echo $FARM | jq -r .id)

curl -s -X POST "$BASE/queues" \
  -H "Content-Type: application/json" \
  -d "{\"farm_id\":\"$FARM_ID\",\"name\":\"renders\",\"priority\":50,\"max_concurrent_tasks\":200}"
```

---

## WebSocket subscriptions

### Upgrade

```
GET /api/v1/ws
Upgrade: websocket
```

### Message envelope

All frames — in both directions — are JSON objects with this shape:

```json
{
  "type": "subscribe | unsubscribe | ping | push | ack | error | pong",
  "subject": "<subject-string>",
  "payload": { ... },
  "seq": 42
}
```

Message types by direction:

| Type | Direction | Purpose |
|---|---|---|
| `subscribe` | client → server | Register for a subject's pushes (payload = `{since_seq}`) |
| `unsubscribe` | client → server | Cancel a subscription |
| `ping` | client → server | Application-level keep-alive (server replies `pong`) |
| `ack` | server → client | Confirms a `subscribe`/`unsubscribe` (payload = `{client_seq, error}`) |
| `push` | server → client | A subscribed event (payload shape varies by subject) |
| `error` | server → client | Protocol-level error (payload = `{code, message}`) |
| `pong` | server → client | Reply to a client `ping` |

`seq` on a server `push` is a per-subject, hub-assigned counter (starts at 1,
increases globally across connections); store the last value you received and
pass it as `since_seq` when reconnecting. On client messages `seq` is a
client-chosen value the server echoes back in the `ack`'s `client_seq` so you
can correlate replies.

### Subscribe

Send a `subscribe` frame with the resume cursor inside `payload` as `since_seq`.
The server replies with an `ack`, then immediately replays buffered events with
`seq` greater than `since_seq`, then streams new events live:

```json
{"type": "subscribe", "subject": "tasks/{task-id}/logs", "payload": {"since_seq": 0}, "seq": 1}
```

The acknowledgement (sent before any replayed pushes):

```json
{"type": "ack", "payload": {"client_seq": 1, "error": ""}}
```

A non-empty `ack.error` (e.g. an unknown subject) means the subscription was
rejected; treat it like an `error` frame.

### Available subjects

| Subject | Description | `push` payload fields |
|---|---|---|
| `jobs` | Aggregate job summary changes | `job_id, name, owner, queue_id, status, updated_at` |
| `jobs/{job-id}/tasks` | Task-level state transitions for the given job | `job_id, task_id, name, status, worker_id, updated_at` |
| `tasks/{task-id}/logs` | Live log chunks for the given task attempt | `task_id, attempt_id, seq_num, stream, data, at` |
| `workers` | Worker registration and heartbeat events | `worker_id, hostname, farm_id, status` |

A `tasks/{task-id}/logs` push carries a `seq_num` and the chunk content but —
unlike the REST `GET /tasks/{id}/logs` chunks — omits `id`, `nats_seq`, and
`received_at`.

Example `push` frame:

```json
{
  "type": "push",
  "subject": "tasks/{task-id}/logs",
  "payload": {
    "task_id": "...",
    "attempt_id": "...",
    "seq_num": 42,
    "stream": "stdout",
    "data": "Frame 0042 rendered in 3.2s\n",
    "at": "2026-01-15T10:05:42.123Z"
  },
  "seq": 42
}
```

### Unsubscribe

```json
{"type": "unsubscribe", "subject": "tasks/{task-id}/logs"}
```

### Keep-alive

The server sends a WebSocket-level ping every 30 seconds (handled transparently
by any compliant WebSocket client). Connections idle for more than 5 minutes
without any frame are closed with status 1001 (Going Away). Reconnect, re-send
your `subscribe` frames, and pass the last received `seq` as `since_seq` to
resume without missing events. (A client may also send an application-level
`ping` frame at any time; the server replies with `pong`.)

---

## Health and observability endpoints

| Endpoint | Description |
|---|---|
| `GET /healthz` | Liveness — returns `200 OK` if the process is alive |
| `GET /readyz` | Readiness — returns `200 OK` only when SQLite and NATS are reachable |
| `GET /metrics` | Prometheus metrics (text format) |
| `GET /api/v1/openapi.yaml` | OpenAPI 3.1 specification |

```sh
curl -sf http://localhost:8080/healthz && echo "alive"
curl -sf http://localhost:8080/readyz  && echo "ready"
```
