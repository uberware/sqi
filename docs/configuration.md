# sqi-server Configuration Reference

`sqi-server` is configured through four layers applied in order, with later
layers overriding earlier ones:

1. **Built-in defaults** — sensible values for local development.
2. **Config file** — YAML or JSON; searched in `./config/sqi-server.yaml`,
   `~/.sqi/sqi-server.yaml`, and `/etc/sqi/sqi-server.yaml` by default. Pass
   an explicit path with `--config /path/to/file`.
3. **Environment variables** — prefixed `SQI_`, e.g. `SQI_HTTP_ADDR`.
4. **CLI flags** — highest priority; available on the `serve` subcommand.

Print the effective merged configuration at any time with:

```sh
sqi-server config print
```

A fully commented example file is at
[`config/sqi-server.example.yaml`](../config/sqi-server.example.yaml).

Duration values use Go syntax: `30s`, `1m30s`, `500ms`, `2h`, etc.

---

## `http` — REST API and WebSocket listener

### `http.addr`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"0.0.0.0:8080"` |
| **Env var** | `SQI_HTTP_ADDR` |
| **CLI flag** | `--http-addr` |

TCP address the HTTP server listens on. Use `127.0.0.1:8080` to restrict to
loopback only.

```yaml
http:
  addr: "0.0.0.0:8080"
```

---

### `http.enable_pprof`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `false` |
| **Env var** | `SQI_HTTP_ENABLE_PPROF` |
| **CLI flag** | *(none — set via config file or env var)* |

Expose Go runtime profiling endpoints at `/debug/pprof/`. Profiling data
reveals memory layout, goroutine stacks, and CPU hotspots — never enable
this on a server accessible to untrusted networks. Enable temporarily on a
loopback-only instance for performance diagnosis.

```yaml
http:
  enable_pprof: false
```

---

## `nats` — Embedded NATS JetStream broker

### `nats.addr`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"0.0.0.0:4222"` |
| **Env var** | `SQI_NATS_ADDR` |

TCP address the embedded NATS server binds to. Defaults to all interfaces so
that workers which discover the server over mDNS can connect to NATS at the
advertised LAN host. Set this to `"127.0.0.1:4222"` to restrict NATS to loopback
(single-machine only). The broker is currently unauthenticated; authentication
arrives in phase 3.

```yaml
nats:
  addr: "0.0.0.0:4222"
```

---

### `nats.data_dir`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"data/nats"` |
| **Env var** | `SQI_NATS_DATA_DIR` |

Directory used by JetStream for file-backed stream persistence. Created at
startup if it does not exist. Relative paths are resolved from the working
directory at the time `sqi-server` starts. For production, use an absolute path
on fast local storage.

```yaml
nats:
  data_dir: "/var/lib/sqi/nats"
```

---

### `nats.max_store_mb`

| | |
|---|---|
| **Type** | `int` (megabytes) |
| **Default** | `1024` |
| **Env var** | `SQI_NATS_MAX_STORE_MB` |

Maximum disk space JetStream may use. When the limit is reached, older messages
are evicted per stream retention policy. Increase this on farms with many active
jobs or high log volume.

```yaml
nats:
  max_store_mb: 4096
```

---

## `store` — SQLite state store

### `store.sqlite_path`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"sqi.db"` |
| **Env var** | `SQI_STORE_SQLITE_PATH` |

Path to the SQLite database file. Created at startup if it does not exist.
Schema migrations run automatically at startup. For production, use an absolute
path on a local SSD.

```yaml
store:
  sqlite_path: "/var/lib/sqi/sqi.db"
```

---

### `store.checkpoint_interval`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `"5m"` |
| **Env var** | `SQI_STORE_CHECKPOINT_INTERVAL` |

How often the background goroutine runs `PRAGMA wal_checkpoint(TRUNCATE)` to
fold committed WAL frames back into the main database file. Without periodic
checkpointing the WAL grows unboundedly under write load. A final checkpoint
always runs on clean shutdown regardless of this setting.

Set to a large value (e.g. `"24h"`) to disable periodic checkpointing while
keeping the shutdown checkpoint. Must be `> 0`.

```yaml
store:
  checkpoint_interval: "10m"
```

---

## `log` — Structured logging

### `log.level`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"info"` |
| **Accepted values** | `debug`, `info`, `warn`, `error` |
| **Env var** | `SQI_LOG_LEVEL` |
| **CLI flag** | `--log-level` |

Minimum log level to emit. `debug` includes verbose request tracing and
scheduler internals — useful during development but noisy in production.

```yaml
log:
  level: "info"
```

---

### `log.format`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"json"` |
| **Accepted values** | `json`, `text` |
| **Env var** | `SQI_LOG_FORMAT` |
| **CLI flag** | `--log-format` |

Log output format. `json` is structured and machine-parseable — use it in
production so log aggregators (Loki, Datadog, Splunk, etc.) can index fields.
`text` is human-readable with aligned columns — use it during local development.

```yaml
log:
  format: "json"
```

---

## `scheduler` — Task assignment loop

### `scheduler.heartbeat_timeout`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `"30s"` |
| **Env var** | `SQI_SCHEDULER_HEARTBEAT_TIMEOUT` |

Duration after which a worker that has not sent a heartbeat is declared offline.
Its in-flight tasks are reclaimed and re-queued for assignment. For high-latency
networks or heavily loaded worker hosts, increase to `60s` or more. Must be
`> 0`.

```yaml
scheduler:
  heartbeat_timeout: "45s"
```

---

### `scheduler.tick_interval`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `"500ms"` |
| **Env var** | `SQI_SCHEDULER_TICK_INTERVAL` |

How often the assignment loop wakes to match ready tasks to idle workers. Lower
values reduce assignment latency at the cost of more CPU. Most farms work well
at the default. Reduce to `100ms` only for workloads with sub-second task
bursts. Must be `> 0`.

```yaml
scheduler:
  tick_interval: "500ms"
```

---

### `scheduler.max_tasks_per_worker`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `1` |
| **Env var** | `SQI_SCHEDULER_MAX_TASKS_PER_WORKER` |

Maximum number of tasks simultaneously assigned to a single worker. The default
of `1` is appropriate for rendering workloads that saturate CPU or GPU.
Increase for lightweight tasks (transcoding short clips, small script jobs) that
benefit from in-process concurrency on the worker side. Must be `≥ 1`.

```yaml
scheduler:
  max_tasks_per_worker: 1
```

---

### `scheduler.offline_worker_retention`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `24h` |
| **Env var** | `SQI_SCHEDULER_OFFLINE_WORKER_RETENTION` |

How long a worker may remain offline before the retention sweep hard-deletes its
record, bounding the growth of the worker list on farms with ephemeral nodes
(e.g. cloud workers that spin up for a burst of work and are then destroyed). The
sweep runs on the heartbeat-sweep tick and only removes workers in the `offline`
state — `online` and administratively `disabled` workers are never auto-removed.
A worker that reconnects after removal simply re-registers. Set to `0` to disable
automatic removal entirely (workers can still be removed manually from the web
UI).

```yaml
scheduler:
  offline_worker_retention: "24h"
```

---

## `discovery` — mDNS service advertisement

### `discovery.enabled`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `true` |
| **Env var** | `SQI_DISCOVERY_ENABLED` |

When `true`, `sqi-server` broadcasts a `_sqi._tcp` mDNS record so that workers
and the `sqi` CLI can discover it automatically on the local network without any
manual address configuration. Disable in environments that prohibit multicast —
most cloud VPCs, VLANs, and container networks fall into this category.

```yaml
discovery:
  enabled: true
```

---

### `discovery.instance_name`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"sqi-server"` |
| **Env var** | `SQI_DISCOVERY_INSTANCE_NAME` |

The mDNS service instance name advertised on the network. Each `sqi-server` on
the same subnet should use a distinct name to avoid collisions when running
multiple farms on the same local network.

```yaml
discovery:
  instance_name: "studio-farm-primary"
```

---

## `openjd` — OpenJD submission and validation

### `openjd.enforce_limits`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `true` |
| **Env var** | `SQI_OPENJD_ENFORCE_LIMITS` |
| **CLI flag** | `--openjd-enforce-limits` |

When `true`, submitted job templates are checked against the OpenJD quantitative
limits — maximum name lengths, parameter-definition counts, per-parameter value
counts, and host-requirement counts. Set to `false` only in operator
environments that predate strict limit enforcement and cannot yet update all
templates; resource-exhaustion guards (the per-range value cap and the per-step
task-count cap) always apply regardless of this setting.

```yaml
openjd:
  enforce_limits: true
```

---

## `diagnostics` — In-UI diagnostic log buffer

### `diagnostics.buffer_size`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `1000` |
| **Env var** | `SQI_DIAGNOSTICS_BUFFER_SIZE` |

Maximum diagnostic records retained **per component** (`server` plus each
connected worker) in `sqi-server`'s in-memory ring buffer. This single value is
also the on/off switch:

- **`0`** — diagnostics are **disabled**: no buffer is allocated, the server
  does not subscribe to `worker.diag.>`, and the REST diagnostics endpoint
  returns 503.
- **positive** — the per-component capacity. When a component's buffer is full
  the oldest records are evicted. The buffer is in-memory only and is cleared on
  server restart.

Negative values are rejected. The buffer feeds the web UI (Admin → Server log
and each worker's detail page).

```yaml
diagnostics:
  buffer_size: 2000 # or 0 to disable
```

> Workers have their own separate `diagnostics.enabled` toggle (they publish
> rather than buffer) — see the worker configuration guide.

See [`docs/observability.md`](observability.md) for the full diagnostics guide.

---

## Quick reference table

| Key | Type | Default | Env var | CLI flag |
|---|---|---|---|---|
| `http.addr` | string | `0.0.0.0:8080` | `SQI_HTTP_ADDR` | `--http-addr` |
| `http.enable_pprof` | bool | `false` | `SQI_HTTP_ENABLE_PPROF` | — |
| `nats.addr` | string | `0.0.0.0:4222` | `SQI_NATS_ADDR` | — |
| `nats.data_dir` | string | `data/nats` | `SQI_NATS_DATA_DIR` | — |
| `nats.max_store_mb` | int | `1024` | `SQI_NATS_MAX_STORE_MB` | — |
| `store.sqlite_path` | string | `sqi.db` | `SQI_STORE_SQLITE_PATH` | — |
| `store.checkpoint_interval` | duration | `5m` | `SQI_STORE_CHECKPOINT_INTERVAL` | — |
| `log.level` | string | `info` | `SQI_LOG_LEVEL` | `--log-level` |
| `log.format` | string | `json` | `SQI_LOG_FORMAT` | `--log-format` |
| `scheduler.heartbeat_timeout` | duration | `30s` | `SQI_SCHEDULER_HEARTBEAT_TIMEOUT` | — |
| `scheduler.tick_interval` | duration | `500ms` | `SQI_SCHEDULER_TICK_INTERVAL` | — |
| `scheduler.max_tasks_per_worker` | int | `1` | `SQI_SCHEDULER_MAX_TASKS_PER_WORKER` | — |
| `discovery.enabled` | bool | `true` | `SQI_DISCOVERY_ENABLED` | — |
| `discovery.instance_name` | string | `sqi-server` | `SQI_DISCOVERY_INSTANCE_NAME` | — |
| `openjd.enforce_limits` | bool | `true` | `SQI_OPENJD_ENFORCE_LIMITS` | `--openjd-enforce-limits` |
| `diagnostics.buffer_size` | int | `1000` | `SQI_DIAGNOSTICS_BUFFER_SIZE` | — |

---

## Minimal production example

```yaml
# /etc/sqi/sqi-server.yaml

http:
  addr: "0.0.0.0:8080"

nats:
  data_dir: "/var/lib/sqi/nats"
  max_store_mb: 4096

store:
  sqlite_path: "/var/lib/sqi/sqi.db"

log:
  level: "info"
  format: "json"

scheduler:
  heartbeat_timeout: "60s"
  tick_interval: "500ms"
  max_tasks_per_worker: 1

discovery:
  enabled: false       # disable multicast in a server environment
  instance_name: "sqi-prod"
```

---

## See also

- [`config/sqi-server.example.yaml`](../config/sqi-server.example.yaml) — Fully
  commented example with every option.
- [`docs/architecture.md`](architecture.md) — Component layout and how configuration values are consumed.
- [`docs/operations.md`](operations.md) — Install, upgrade, backup, and log rotation.
- [`docs/observability.md`](observability.md) — In-UI diagnostics, REST/WS log API, and external log wiring.
