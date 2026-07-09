# sqi Observability Guide

This document covers the two distinct log channels in sqi, the in-UI diagnostic
panels, the REST and WebSocket APIs for consuming diagnostic logs, how to wire
diagnostic output to external systems (journald, Docker, Loki, ELK), and the
full configuration reference.

---

## Two log channels

sqi produces two distinct kinds of log output; they serve different purposes
and follow different flows.

| | Task logs | Diagnostic logs |
|---|---|---|
| **What** | stdout/stderr of a task process running under a worker | sqi-server's and sqi-worker's own structured `slog` output |
| **Who writes** | The task's child process | The sqi binaries themselves |
| **Transport** | NATS JetStream `task.logs.<taskID>` | Core NATS `worker.diag.<workerID>` (best-effort); server logs go in-process |
| **Persistence** | Durable; retained ~96 h in JetStream, also written to SQLite | In-memory ring buffer on the server; lost on server restart |
| **Where to read** | Task detail → log tab in the web UI | Worker detail panel, Admin → Server log, or REST/WS API |
| **Disable** | Not configurable (always streamed for running tasks) | Server: `SQI_DIAGNOSTICS_BUFFER_SIZE=0`; worker: `SQI_DIAGNOSTICS_ENABLED=false` |

### Task logs

When a worker executes a task it captures the process's stdout and stderr
through a `logstreamer` and publishes them in chunks to the JetStream subject
`task.logs.<taskID>`. The server's consumer writes each chunk to SQLite and
fans it out over WebSocket so the task log page updates live. Because JetStream
retains messages, you can reload the log page and still see all output.

### Diagnostic logs

Both binaries write their own operational output (scheduler decisions, worker
registration, errors, etc.) using Go's structured `slog`. This output is
**always** written to **stderr** in JSON format by default (see
[Log format](#log-format-and-level)). When diagnostics are enabled (the
default), the logs are also forwarded to the server's in-memory ring buffer and
surfaced in the web UI — so operators do not need to shell into a host just to
see why a worker is misbehaving.

**Transport detail:** each worker runs a fan-out sink that publishes every log
record as a JSON `DiagLogMsg` to the core-NATS subject
`worker.diag.<workerID>`. The server subscribes to `worker.diag.>` and stores
arriving records in its buffer. The server's own slog records bypass NATS and
feed the same buffer in-process via a fan-out handler. Core NATS is used (not
JetStream) because diagnostic records are best-effort — a brief network blip
simply means some lines are missing from the UI, which is acceptable for
operational logs.

---

## Task-state health signals

### `assigned` as an anomaly

Under the lease model, `assigned` is a brief handoff window: the worker that
just requested work is expected to transition the task to `running` within
seconds. A task that lingers in `assigned` is a genuine anomaly — the worker
leased it but never reported `running`.

The scheduler's **stale-assigned reaper** runs on every heartbeat-sweep tick
and returns tasks that have been `assigned` for longer than
`scheduler.assigned_task_timeout` (default **30 s**) to the `ready` queue.
If the task's job is now idle (no other tasks in flight), the job is demoted
from `running` back to `pending` so it does not appear stuck.

Frequent reaper activity on a particular worker is a sign that the worker is
taking leases but failing to start them — look for WARN-level lines in the
worker's diagnostic panel (see [Worker diagnostics panel](#worker-diagnostics-panel)).

### Per-worker health from heartbeat + running state

The server infers worker health from two signals:

1. **Heartbeat staleness.** Heartbeats are retained (busy workers holding
   tasks do not issue lease requests, so the long-poll itself is not a
   liveness signal). A worker whose last heartbeat is older than
   `scheduler.heartbeat_timeout` (default 30 s) is marked offline and its
   tasks are reclaimed.

2. **Missing `running` within the tight window.** A task in `assigned` that
   does not transition to `running` within `assigned_task_timeout` is reclaimed
   by the stale-assigned reaper. No worker self-report is needed — the
   server's ledger is authoritative. The only possible divergence is a
   server-side phantom (the server assigned the task but the reply was lost),
   which the tight reaper window already catches.

### Why isn't my job running? — Unschedulable tasks

Unlike `assigned` lingering (an anomaly on an otherwise-progressing task),
"unschedulable" answers the more common operator question: a `ready` task
just never gets picked up. It means no currently-online worker can satisfy
that task's requirements — a farm/queue/compute-location mismatch, an unmet
hardware amount or capability-attribute requirement, or a usage pool at
capacity — and it has been waiting long enough that this isn't just a
worker-hasn't-polled-yet timing gap.

The scheduler's **unschedulable sweep** runs on every heartbeat-sweep tick
(the same tick as the stale-assigned reaper above) and reuses the same
worker-eligibility check the assignment path uses to hand out leases — it is
read-only and never changes scheduling, only the annotation described below.
A `ready` task is flagged once it has waited longer than
`scheduler.unschedulable_grace` (default **30 s**) with no eligible online
worker; setting the grace period to `0` disables the sweep entirely. See
[`scheduler.unschedulable_grace`](configuration.md#schedulerunschedulable_grace)
for the config knob.

Where it surfaces:

- **Task**: the `unschedulable_reason` field on `GET /api/v1/tasks/{id}` (and
  in task list/detail responses) — a human-readable string such as
  `no online workers` or `no eligible online worker: <reason>`, where
  `<reason>` is one of `farm mismatch`, `queue affinity`,
  `compute location mismatch`, `amount requirement not met`,
  `attribute requirement not met`, or `usage pool at capacity`. Empty/absent
  means schedulable.
- **Job**: `task_counts.unschedulable` — the number of `ready` tasks currently
  flagged, a subset of `ready`, not an additional task status.
- **Web UI**: an "Unschedulable" badge on the job detail page (per affected
  task) and a matching count on the job's row in the job list.

It is a continuous, self-clearing signal rather than a one-shot alert: every
sweep tick re-evaluates already-flagged tasks, so the reason string clears the
moment a matching worker comes online, or the task leaves `ready` (assigned to
a worker or canceled) — no manual acknowledgement is needed. If a job's badge
won't clear, check the [Worker diagnostics panel](#worker-diagnostics-panel)
and the task's reason string together: a `queue affinity` or
`compute location mismatch` reason usually points at a worker configuration
mismatch rather than a capacity shortfall.

---

## In-UI diagnostics

### Worker diagnostics panel

Navigate to **Workers** → click any worker → the **Worker diagnostics** panel
appears below the task list. It shows the most recent log records emitted by
that worker process, newest last. Use this to diagnose connection issues, task
assignment problems, or executor errors without leaving the UI.

### Admin → Server log

The top-level **Admin** nav item opens the management hub; its **Server Log**
card opens the server-log page (`/server-log`) showing `sqi-server`'s own
diagnostic records (component `server`). This is where you
look for scheduler errors, NATS broker issues, database connection problems, and
startup warnings.

### Failed-task fallback (the "process not found" case)

When a task finishes in the `failed` state but produced no task output — for
example because the worker could not find the configured executable — the task
detail page shows a fallback panel labelled **Worker diagnostics for this
task**. This panel queries the diagnostic buffer filtered by the task's ID
(`task_id` correlation key) and shows the worker log lines that accompanied the
failure, typically something like:

```
WARN  executor: process not found  path=/usr/local/bin/houdini-20.5
```

Without this fallback, an empty task log gives no indication of what went
wrong. The fallback only appears when the task is failed and the task log is
empty.

### Disabling in-UI diagnostics

Set `SQI_DIAGNOSTICS_BUFFER_SIZE=0` on the server to turn off the ring buffer and
the `worker.diag.>` subscription (a single knob: `0` = off, a positive value = the
per-component buffer capacity):

```sh
SQI_DIAGNOSTICS_BUFFER_SIZE=0 sqi-server serve --config /etc/sqi/sqi-server.yaml
```

When server diagnostics are disabled (`buffer_size: 0`):

- No in-memory buffer is allocated.
- The REST endpoint returns **503 Service Unavailable**.
- The worker diagnostics panel and Admin → Server log show nothing.

Workers have a separate boolean toggle: set `SQI_DIAGNOSTICS_ENABLED=false` on a
worker so it logs to stderr only and does not publish `worker.diag` messages
(workers publish rather than buffer, so there is no size to configure there).

In this mode, operational logs are available only out-of-band (journald, Docker,
file forwarding — see [Out-of-band wiring](#out-of-band-wiring)).

> **Buffer lifetime:** the ring buffer is **in-memory only** and is lost on
> server restart. If you need durable operational logs beyond `buffer_size`
> records per component, ship them out-of-band as well.

---

## REST API

### `GET /api/v1/diagnostics/logs`

Returns records from the in-memory diagnostic log buffer.

**Query parameters:**

| Parameter | Type | Description |
|---|---|---|
| `component` | string | Exact component match: `server` or `worker:<workerID>`. Omit to return all components. |
| `level` | string | Minimum level: `DEBUG`, `INFO`, `WARN`, `ERROR`. Omit for all levels. |
| `task_id` | string | Only records whose `attrs.task_id` equals this value. |
| `since` | string | RFC3339Nano lower bound (exclusive). Records with `ts > since` are returned. |
| `limit` | int | Maximum records to return (newest kept). Default and maximum: 1000. |

**Response (200 OK):**

```json
{
  "records": [
    {
      "ts":        "2026-01-15T10:23:45.000000001Z",
      "component": "worker:a1b2c3d4",
      "level":     "WARN",
      "msg":       "executor: process not found",
      "attrs": {
        "path":      "/usr/local/bin/houdini-20.5",
        "task_id":   "tsk_abc123",
        "worker_id": "a1b2c3d4"
      }
    }
  ]
}
```

When diagnostics are disabled the endpoint returns **503** with a problem-detail
body instead.

**Example — tail recent WARN and above from a specific worker:**

```sh
WORKER_ID="a1b2c3d4"
curl -s "http://localhost:8080/api/v1/diagnostics/logs?component=worker:${WORKER_ID}&level=WARN&limit=50" \
  | jq '.records[] | "\(.ts) [\(.level)] \(.msg)"'
```

**Example — fetch all records for a failed task:**

```sh
curl -s "http://localhost:8080/api/v1/diagnostics/logs?task_id=tsk_abc123" \
  | jq '.records'
```

**Example — poll for new records since a known timestamp:**

```sh
SINCE="2026-01-15T10:23:45.000000001Z"
curl -s "http://localhost:8080/api/v1/diagnostics/logs?since=${SINCE}" \
  | jq '.records'
```

---

## WebSocket live tail

Subscribe to the `diagnostics` subject over the existing WebSocket endpoint
(`/api/v1/ws`) to receive records as they are emitted:

```json
{ "type": "subscribe", "subject": "diagnostics" }
```

Each push frame carries a `DiagnosticsPush` payload:

```json
{
  "type":    "push",
  "subject": "diagnostics",
  "payload": {
    "component": "worker:a1b2c3d4",
    "level":     "INFO",
    "msg":       "task assigned",
    "attrs": {
      "task_id":   "tsk_abc123",
      "job_id":    "job_xyz",
      "worker_id": "a1b2c3d4"
    },
    "at": "2026-01-15T10:23:45.000000001Z"
  }
}
```

> Note: the WebSocket payload uses the key `at` for the timestamp; the REST
> response uses `ts`. Both carry the same RFC3339Nano value.

Clients filter by component or level client-side — the server broadcasts all
enabled records to all `diagnostics` subscribers.

---

## Diagnostic log schema

### JSON fields

| Field | Type | Description |
|---|---|---|
| `ts` / `at` | string (RFC3339Nano) | Record timestamp in UTC. `ts` in REST responses; `at` in WS push payloads. |
| `component` | string | `"server"` for sqi-server records; `"worker:<workerID>"` for worker records. |
| `level` | string | `"DEBUG"`, `"INFO"`, `"WARN"`, or `"ERROR"`. |
| `msg` | string | Log message text. |
| `attrs` | object (string→string) | Structured attributes. Omitted when empty. |

### Levels

| Level | Use |
|---|---|
| `DEBUG` | Verbose internals — scheduler tick details, NATS message counts. Not emitted at the default `info` level. |
| `INFO` | Normal operational events — worker registration, task assignment, startup. |
| `WARN` | Recoverable problems — missed heartbeat, executor warning, retried operation. |
| `ERROR` | Failures that required operator attention — database errors, NATS disconnection. |

### Correlation keys

The following attribute keys appear on relevant log lines and can be used to
correlate records across components:

| Key | Value | Present on |
|---|---|---|
| `task_id` | Task identifier (e.g. `tsk_abc123`) | Worker executor logs, failed-task lookup |
| `attempt_id` | Attempt identifier within a task | Worker executor logs |
| `job_id` | Job identifier | Scheduler and worker logs |
| `worker_id` | Worker identifier | Worker logs, server ingestion |
| `session_id` | Worker session identifier | Worker registration and heartbeat logs |

---

## Log format and level

Both binaries share the same `log.*` configuration keys:

```sh
# JSON (default, production-recommended)
SQI_LOG_FORMAT=json SQI_LOG_LEVEL=info sqi-server serve

# Human-readable text (local development)
SQI_LOG_FORMAT=text SQI_LOG_LEVEL=debug sqi-server serve
```

Or via config file:

```yaml
log:
  format: json    # json | text
  level:  info    # debug | info | warn | error
```

The stderr JSON lines (with `SQI_LOG_FORMAT=json`) look like:

```json
{"time":"2026-01-15T10:23:45.000Z","level":"INFO","msg":"server started","addr":"0.0.0.0:8080"}
```

Use `text` only in development; log aggregators expect structured JSON.

---

## Out-of-band wiring

Both binaries always write structured JSON to **stderr**. The in-UI buffer
complements out-of-band log forwarding — it does not replace it. Ship stderr to
your log platform for durable, searchable operational history beyond what the
ring buffer retains.

### journald (systemd)

The systemd unit files in [`docs/operations.md`](operations.md) and
[`docs/worker-deployment.md`](worker-deployment.md) already route stderr to
journald via `StandardError=journal`. No additional configuration is needed.

Tail live:

```sh
journalctl -u sqi-server -f
journalctl -u sqi-worker -f
```

Query as JSON (compatible with `jq`):

```sh
# All WARN+ from sqi-server in the last hour
journalctl -u sqi-server --since "1 hour ago" -o json \
  | jq 'select(.PRIORITY <= "4") | .MESSAGE | fromjson?'
```

Filter by a correlation key — journald stores the slog JSON inside the
`MESSAGE` field, so pipe through `jq`:

```sh
# Find all log lines for a specific task
journalctl -u sqi-worker --since "1 day ago" -o json \
  | jq -r '.MESSAGE | fromjson? | select(.task_id == "tsk_abc123")
           | "\(.time) [\(.level)] \(.msg)"'
```

Tune retention in `/etc/systemd/journald.conf`:

```ini
[Journal]
SystemMaxUse=2G
MaxRetentionSec=30day
```

### Docker

Both official images write JSON to stderr by default. Pass `--log-driver` to
route to any Docker logging plugin:

```sh
# Default json-file driver (logs in /var/lib/docker/containers/<id>/<id>-json.log)
docker run -d --name sqi-server \
  -e SQI_LOG_FORMAT=json \
  ghcr.io/uberware/sqi/sqi-server:latest serve

# Read live
docker logs -f sqi-server

# Pipe to jq for field filtering
docker logs sqi-server 2>&1 | jq 'select(.level == "WARN" or .level == "ERROR")'
```

For centralised collection, use the `fluentd` or `gelf` driver:

```sh
docker run -d --name sqi-server \
  --log-driver fluentd \
  --log-opt fluentd-address=localhost:24224 \
  --log-opt tag="sqi.server" \
  ghcr.io/uberware/sqi/sqi-server:latest serve
```

### Loki / Promtail

If you are running sqi under systemd, point Promtail at the journald source:

```yaml
# promtail-config.yaml
scrape_configs:
  - job_name: sqi
    journal:
      labels:
        job: sqi
    pipeline_stages:
      - json:
          expressions:
            level:   level
            msg:     msg
            task_id: task_id
      - labels:
          level:
          task_id:
```

For Docker deployments, use the Docker service discovery driver or mount the
container log files and parse the JSON lines directly:

```yaml
scrape_configs:
  - job_name: sqi-docker
    docker_sd_configs:
      - host: unix:///var/run/docker.sock
        filters:
          - name: name
            values: [sqi-server, sqi-worker]
    pipeline_stages:
      - json:
          expressions:
            level:     level
            component: component
      - labels:
          level:
          component:
```

### ELK / Filebeat

If sqi runs under systemd and journald writes to files under
`/var/log/journal/`, point Filebeat at the journal input:

```yaml
# filebeat.yml
filebeat.inputs:
  - type: journald
    id: sqi
    include_matches:
      - SYSLOG_IDENTIFIER=sqi-server
      - SYSLOG_IDENTIFIER=sqi-worker

output.elasticsearch:
  hosts: ["https://elasticsearch:9200"]
  index: "sqi-logs-%{+yyyy.MM.dd}"

processors:
  - decode_json_fields:
      fields: ["message"]
      target: ""
      overwrite_keys: true
```

For file-based deployments (stdout/stderr redirected to files), use the
`log` input type and set `json.keys_under_root: true` to promote top-level
JSON fields so you can filter on `level`, `msg`, `task_id`, etc. directly in
Kibana.

---

## Configuration reference

### Server (`sqi-server`)

| YAML key | Env var | Type | Default | Description |
|---|---|---|---|---|
| `diagnostics.buffer_size` | `SQI_DIAGNOSTICS_BUFFER_SIZE` | int | `1000` | Per-component ring-buffer capacity (`server`, each worker). `0` disables diagnostics (no buffer, no `worker.diag.>` subscription, REST 503); positive = capacity; negative is rejected. |

Example:

```yaml
diagnostics:
  buffer_size: 2000 # 0 to disable
```

### Worker (`sqi-worker`)

| YAML key | Env var | Type | Default | Description |
|---|---|---|---|---|
| `diagnostics.enabled` | `SQI_DIAGNOSTICS_ENABLED` | bool | `true` | Publish slog records to the server over `worker.diag.<workerID>`. When false, logs go to stderr only. |

Example:

```yaml
diagnostics:
  enabled: true
```

The `SQI_LOG_FORMAT` and `SQI_LOG_LEVEL` env vars (and their `log.format` /
`log.level` YAML equivalents) apply to both binaries and control what reaches
stderr regardless of whether diagnostics are enabled. See
[`docs/configuration.md`](configuration.md) for the full log config reference.

---

## Security considerations

Diagnostic log lines carry operational metadata: file paths, command names,
error strings, and correlation keys such as `task_id` and `worker_id`. They
do **not** carry secret values: the worker executor logs attribute keys but
not environment variable values, and secret injection in OpenJD tasks keeps
secrets out of the task's structured log output.

Operators should still follow standard practices:

- Do not set `SQI_LOG_LEVEL=debug` in production unless actively diagnosing a
  problem. Debug output is more verbose and may log additional internal state.
- The `GET /api/v1/diagnostics/logs` endpoint sits under the same authorization
  as all other sqi read endpoints. Restrict network access to the sqi-server
  port accordingly.
- If you forward diagnostic logs to a third-party log aggregator, ensure that
  aggregator's access controls are appropriate for operational metadata.

---

## See also

- [`docs/operations.md`](operations.md) — Server install, upgrade, and log
  routing under systemd.
- [`docs/worker-deployment.md`](worker-deployment.md) — Worker install and
  log routing.
- [`docs/configuration.md`](configuration.md) — Full configuration reference
  including `log.level` and `log.format`.
- [`docs/architecture.md`](architecture.md) — NATS subject layout and the
  task log pipeline.
