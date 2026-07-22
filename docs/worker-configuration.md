# sqi-worker Configuration Reference

`sqi-worker` is configured through four layers applied in order, with later
layers overriding earlier ones:

1. **Built-in defaults** — sensible values for local development.
2. **Config file** — YAML or JSON; searched in `./config/sqi-worker.yaml`,
   `~/.sqi/sqi-worker.yaml`, and `/etc/sqi/sqi-worker.yaml` by default. Pass
   an explicit path with `--config /path/to/file`.
3. **Environment variables** — prefixed `SQI_WORKER_`, e.g.
   `SQI_WORKER_NATS_URL`. (Exceptions: `diagnostics.enabled` uses
   `SQI_DIAGNOSTICS_ENABLED` and `staging.defaults` uses
   `SQI_STAGING_DEFAULTS`, both with no `WORKER` infix — see the
   `diagnostics` and `staging` sections.)
4. **CLI flags** — highest priority; available on the `start` subcommand.

Print the effective merged configuration at any time with:

```sh
sqi-worker config print
```

Validate configuration without connecting to the server:

```sh
sqi-worker start --dry-run
```

A fully commented example file is at
[`config/sqi-worker.example.yaml`](https://github.com/uberware/sqi/blob/main/config/sqi-worker.example.yaml).

Duration values use Go syntax: `30s`, `1m30s`, `500ms`, `2h`, etc.

---

## `nats` — Remote NATS connection

### `nats.url`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` (empty — use mDNS discovery) |
| **Env var** | `SQI_WORKER_NATS_URL` |

URL of the NATS server embedded in `sqi-server`. Required when
`discovery.enable_mdns` is `false`. Must not be empty unless mDNS discovery
is enabled. Example: `nats://sqi-server.local:4222`.

```yaml
nats:
  url: "nats://sqi-server.example.com:4222"
```

---

### `nats.tls_cert_file`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` (disabled) |
| **Env var** | `SQI_WORKER_NATS_TLS_CERT_FILE` |

Path to the client TLS certificate (PEM-encoded). Required for mutual TLS
when the server demands client certificates. Must be set together with
`nats.tls_key_file`.

```yaml
nats:
  tls_cert_file: "/etc/sqi/client.crt"
  tls_key_file:  "/etc/sqi/client.key"
```

---

### `nats.tls_key_file`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` (disabled) |
| **Env var** | `SQI_WORKER_NATS_TLS_KEY_FILE` |

Path to the client TLS private key (PEM-encoded). Must be set together with
`nats.tls_cert_file`.

---

### `nats.tls_ca_file`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` (use system CA pool) |
| **Env var** | `SQI_WORKER_NATS_TLS_CA_FILE` |

Path to the CA certificate used to verify the NATS server's TLS certificate
(PEM-encoded). Leave empty to use the system certificate pool.

```yaml
nats:
  tls_ca_file: "/etc/sqi/ca.crt"
```

---

### `nats.insecure_skip_verify`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `false` |
| **Env var** | `SQI_WORKER_NATS_INSECURE_SKIP_VERIFY` |
| **CLI flag** | `--nats-insecure-skip-verify` |

Skip TLS certificate verification. **Never set this to `true` in production**
— it defeats the purpose of TLS. Use only in development environments where a
self-signed certificate is acceptable.

---

### `nats.max_reconnect_attempts`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `-1` (retry indefinitely) |
| **Env var** | `SQI_WORKER_NATS_MAX_RECONNECT_ATTEMPTS` |

Maximum number of reconnection attempts before giving up and exiting. `-1`
means retry indefinitely — the recommended setting for long-running workers.
Reconnect attempts use exponential backoff with jitter starting at
`nats.reconnect_wait`.

```yaml
nats:
  max_reconnect_attempts: -1
```

---

### `nats.reconnect_wait`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `"2s"` |
| **Env var** | `SQI_WORKER_NATS_RECONNECT_WAIT` |

Base wait duration between reconnection attempts. The actual delay uses
exponential backoff with ±20% jitter; this is the floor of that range. Must
be `> 0`.

```yaml
nats:
  reconnect_wait: "2s"
```

---

## `worker` — Identity and runtime behavior

### `worker.name`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `os.Hostname()` |
| **Env var** | `SQI_WORKER_NAME` |

Human-readable label for this worker shown in the `sqi-server` web UI and
logs. Defaults to the machine hostname. Use a descriptive name on farms with
many workers of the same type, e.g. `render-gpu-01`.

```yaml
worker:
  name: "render-gpu-01"
```

---

### `worker.farm_id`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` (no farm) |
| **Env var** | `SQI_WORKER_FARM_ID` |

Farm this worker belongs to. When set, the worker only receives tasks
belonging to that farm. When empty (the default), the worker is unaffiliated
and accepts tasks from any farm — suitable for single-farm or development
setups. Set this when running workers across multiple farms to prevent
cross-farm task assignment.

```yaml
worker:
  farm_id: "studio-a"
```

---

### `worker.data_dir`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `~/.sqi/worker` (Linux/macOS); `%USERPROFILE%\.sqi\worker` (Windows) |
| **Env var** | `SQI_WORKER_DATA_DIR` |

Directory used to persist the worker ID file (`worker.id`) ONLY. Created
automatically on first start, and never widened for run-as-user traversal —
it stays private (0700) for as long as the worker exists.

The worker ID file ensures the server can correlate this worker across
restarts. Do not delete `worker.id` unless you intend to re-register as a new
worker. For production, use an absolute path on a fast local SSD.

Each worker instance needs its own `data_dir`: two workers sharing one would
load the same `worker.id` and collide on the server. This is the key setting
when [running multiple workers on one host](#running-multiple-workers-on-one-host).

Session working directories are a SEPARATE location — see
[`worker.session_dir`](#workersession_dir) below — not a child of `data_dir`
as they were before run-as-user isolation existed.

```yaml
worker:
  data_dir: "/var/lib/sqi-worker"
```

---

### `worker.session_dir`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` → resolved at startup (see below) |
| **Env var** | `SQI_WORKER_SESSION_DIR` |

Directory under which session working directories are created
(`<session_dir>/<sessionID>/`). Deliberately separate from `data_dir`: this
is ephemeral scratch that, when run-as-user isolation (`isolation:` in the
config file — see `config/sqi-worker.example.yaml`) is in use, must be
traversable by whichever run-as-user identity a session resolves to, while
`data_dir` holds the persistent worker-id and must stay private.

Left unset, the effective value is resolved at startup:

- **Running as root** — `/var/lib/sqi-worker-sessions`, created traversable
  (`0711`) from birth. Deliberately a SIBLING of, never a descendant of,
  `data_dir`'s own HOME-unset fallback (`/var/lib/sqi-worker`): nesting the
  two would make `LoadOrCreateWorkerID`'s own `0700` `data_dir` an ancestor
  of the session root, and the boot-time traversal check below would then
  refuse to start over a directory sqi itself just created. Its ancestors
  (`/var`, `/var/lib`) are `0755` on every real Linux/macOS installation, so
  nothing needs to be created or widened specifically for this.
- **Otherwise** — `<data_dir>/sessions`, created at `0750` (the location and
  mode used before this split existed). Real run-as-user isolation cannot
  function without root regardless of directory permissions, so there is
  nothing to protect by moving it, or widening it, for a worker that can
  never use it anyway.

```yaml
worker:
  session_dir: "/var/lib/sqi-worker-sessions"
```

> **Never silently widened.** Unlike an earlier implementation, sqi will not
> `chmod` an existing directory to make it traversable — a directory is
> either created fresh at the correct mode, or the worker refuses to start
> with an actionable error naming the exact ancestor that needs
> `chmod o+x` and why (see `internal/worker/isolation.ValidateTraversable`).
> At **boot**, this check runs only when `isolation.required: true` — an
> operator who never sets that on a worker that happens to run as root (but
> was never actually going to receive an isolated assignment) must still be
> able to start with a deliberately restricted `staging.scratch_dir` or
> `session_dir`. Otherwise, the identical check (same actionable message)
> runs the moment an assignment that actually carries run-as-user isolation
> arrives, failing that one task rather than the whole worker.

---

### `worker.compute_location`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` (none) |
| **Env var** | `SQI_WORKER_COMPUTE_LOCATION` |

Named compute location for this worker. When non-empty, `sqi-server`
auto-registers an entry for this name in the compute-location registry if one
does not already exist — you do not need to pre-create the entry.
The value is used for two purposes:

- **Storage-location path mapping** — the server resolves `loc://` URI
  references using the root keyed by this name in each storage location (see
  [`docs/storage-locations.md`](storage-locations.md)).
- **Step affinity matching** — steps that declare
  `attr.worker.computelocation: [<name>]` in their OpenJD host requirements
  are only assigned to workers whose `compute_location` matches.

Leave empty if you are not using named storage locations and no steps declare
a compute-location requirement.

```yaml
worker:
  compute_location: "nas-studio-a"
```

See [`docs/compute-locations.md`](compute-locations.md) for the full guide,
including the auto-registration model, non-blocking deletes, and the
relationship to storage-location roots.

---

### `worker.capability_tags`

| | |
|---|---|
| **Type** | `[]string` |
| **Default** | `[]` (empty) |
| **Env var** | `SQI_WORKER_CAPABILITY_TAGS` (comma-separated) |

Arbitrary capability tags merged with auto-detected capabilities at
registration time. Use these to annotate software or hardware features that
the auto-detector cannot discover. Tags listed here overwrite any
auto-detected tag with the same key.

```yaml
worker:
  capability_tags:
    - maya-2025
    - arnold-7
    - gpu
    - highram
```

```sh
SQI_WORKER_CAPABILITY_TAGS=maya-2025,arnold-7,gpu sqi-worker start
```

See [`docs/worker-capabilities.md`](worker-capabilities.md) for the full
reference including auto-detected tags.

---

### `worker.heartbeat_interval`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `"15s"` |
| **Env var** | `SQI_WORKER_HEARTBEAT_INTERVAL` |

How often the worker publishes a heartbeat message to `sqi-server`. The
server uses heartbeat gaps to detect stale workers — this value should be
shorter than the server's `scheduler.heartbeat_timeout` (default 30 s). At
the default of 15 s the server receives two heartbeats per timeout window.
Must be `> 0`.

```yaml
worker:
  heartbeat_interval: "15s"
```

---

### `worker.shutdown_grace_period`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `"30s"` |
| **Env var** | `SQI_WORKER_SHUTDOWN_GRACE_PERIOD` |

Maximum time the worker waits for in-flight tasks to finish after receiving
SIGINT or SIGTERM before force-killing them. Tasks that do not complete within
this window receive SIGTERM then SIGKILL and are reported as `failed`. Set
this to match your longest expected task duration on rolling-restart workers.
Must be `> 0`.

```yaml
worker:
  shutdown_grace_period: "5m"
```

---

### `worker.allow_root`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `false` |
| **Env var** | `SQI_WORKER_ALLOW_ROOT` |

Allow the worker to run as the root user on Linux and macOS. Disabled by
default because executing render processes as root is a security risk. Enable
only when running inside a container where root is expected (e.g., as the
container's only user), or when you understand and accept the risk.

```yaml
worker:
  allow_root: false
```

---

### `worker.keep_failed_sessions`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `false` |
| **Env var** | `SQI_WORKER_KEEP_FAILED_SESSIONS` |

Retain a session's working directory after a failed session (task
cancellation, non-zero exit code, or environment setup error). Useful for
post-mortem inspection of partial outputs and environment state on a specific
worker. Disable in production to avoid filling the data directory on busy
workers.

```yaml
worker:
  keep_failed_sessions: false
```

---

### `worker.queue_ids`

| | |
|---|---|
| **Type** | `[]string` |
| **Default** | `[]` (all queues) |
| **Env var** | `SQI_WORKER_QUEUE_IDS` (comma-separated) |

Restrict this worker to serving specific queue IDs. The worker keeps one
outstanding lease request per listed queue (`work.lease.<queueID>`). When
empty (the default), the worker issues a single lease request using an empty
queue ID, which the server treats as a wildcard. Set this on heterogeneous
farms where some workers specialise in a subset of queues.

```yaml
worker:
  queue_ids:
    - gpu-renders
    - cpu-preview
```

---

### `worker.pull_idle_backoff`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `"2s"` |
| **Env var** | `SQI_WORKER_PULL_IDLE_BACKOFF` |

> **Deprecated.** This field is accepted by the config loader for backwards
> compatibility but has no effect in the current lease-based worker. Idle
> backoff is no longer needed: the worker's lease request long-polls on the
> server (~30 s hold) and re-issues immediately on return — there is no tight
> polling loop to throttle.

---

### `worker.pull_nack_delay`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `"5s"` |
| **Env var** | `SQI_WORKER_PULL_NACK_DELAY` |

> **Deprecated.** This field is accepted by the config loader for backwards
> compatibility but has no effect in the current lease-based worker.
> Pre-execution NACKs no longer apply: the server validates eligibility before
> leasing, so the worker runs what it is given.

---

## `capabilities` — Software auto-detection

Configures the built-in DCC detectors (Maya, Nuke, Houdini, Blender) that run
automatically at startup and advertise a `key=true` tag with no per-worker
configuration, plus any custom detectors for in-house tools. Full reference,
including the detector schema and the tag/version model:
[`docs/worker-capabilities.md`](worker-capabilities.md#capability-auto-detection-built-in-dcc-detectors).

### `capabilities.detect`

| | |
|---|---|
| **Type** | `[]Detector` |
| **Default** | `[]` (empty — only the built-ins run) |
| **Env var** | — (config file only) |

Custom detectors, in the same schema as the built-ins. Structured detectors
have no environment-variable form — cloud fleets bake them into the worker
config file shipped in the image. Each entry is validated at config-load
time; an invalid detector (missing `tag`, zero `checks`, more than one check
primitive set, a bad `os` value, or a non-compiling regex) fails worker
startup with a descriptive error.

```yaml
capabilities:
  detect:
    - tag: mytool
      checks:
        - exe: mytool
        - path_glob: "/opt/mytool*/bin/mytool"
      version:
        from: "mytool(?P<v>[0-9.]+)"
```

### `capabilities.disable`

| | |
|---|---|
| **Type** | `[]string` |
| **Default** | `[]` (empty — all built-ins run) |
| **Env var** | `SQI_WORKER_CAPABILITIES_DISABLE` (comma-separated, appended to any config-file entries) |

Built-in tag names to turn off, by exact tag (`maya`, `nuke`, `houdini`,
`blender`). Use this when a built-in misfires on a nonstandard host layout —
typically paired with a `capabilities.detect` entry for the same tag that
supplies more specific checks.

```yaml
capabilities:
  disable: [blender]
```

```sh
SQI_WORKER_CAPABILITIES_DISABLE=blender,nuke sqi-worker start
```

Run `sqi-worker capabilities` to print every tag the worker would currently
advertise, with its source (`auto`, `builtin:<tag>`, `custom`, or `manual`) —
the fastest way to confirm a detector is (or isn't) firing.

---

## `staging` — Local path staging (`stage_locally` delivery)

Used by jobs that run a `stage_locally` delivery of the `SQI_PATH_TRANSLATION`
extension. `scratch_dir` and `sync_command` are operator-owned and have no
environment-variable form (config file only); cloud fleets bake them into the
worker config shipped in the image. `defaults` (with its `SQI_STAGING_DEFAULTS`
env var) controls whether an otherwise-unconfigured worker still runs
`stage_locally` jobs — see below.

### `staging.scratch_dir`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` → `<os.TempDir()>/sqi-staging` whenever unset |
| **Env var** | — (config file only) |

Base directory for per-attempt staged copies. Leave unset to use the
platform temp directory (e.g. `/tmp/sqi-staging` on Linux/macOS,
`%TEMP%\sqi-staging` on Windows) — convenient for local/dev workers, but a
persistent, purpose-built scratch volume is recommended for production. This
fallback applies whenever `scratch_dir` is unset, independent of
`staging.defaults`; what `staging.defaults` controls is whether staging is
allowed to proceed at all on an otherwise-unconfigured worker (see below).

### `staging.sync_command`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` → built-in copy when unset and `staging.defaults` is true |
| **Env var** | — (config file only) |

Command template invoked per path, with `{src}`, `{dest}`, and optional
`{object_type}` placeholders (e.g. `rsync -a {src} {dest}`). The same template
serves copy-in and copy-out.

Leave unset, or set explicitly to the sentinel value `builtin`, to use sqi's
built-in cross-platform copy instead of shelling out — sqi then copies bytes
itself (single file or recursive directory tree, preserving file mode but not
ownership/xattrs) rather than invoking an external command. Any other value is
treated as a shell command template exactly as before.

Explicitly setting `sync_command: builtin` selects the built-in copy — and,
via the TEMP-scratch fallback above, lets staging proceed with `scratch_dir`
left unset — even when `staging.defaults` is `false`. It's an intentional
per-worker opt-in, distinct from the automatic fallback described under
`staging.defaults`, so it does not log the one-time WARN either.

> **The built-in copy only moves bytes the worker can already reach** (local
> disk or a filesystem already shared/mounted on that worker). It is a
> local/dev convenience, not a substitute for remote transfer. A farm whose
> workers span multiple compute locations (see
> [`docs/compute-locations.md`](compute-locations.md)) needs a real
> `sync_command` (`rsync`, `aws s3 cp`, etc.) to move data between them —
> configure one explicitly rather than relying on the built-in copy.

> **`sync_command` MUST NOT create hardlinks into scratch** when run-as-user
> isolation is in use — e.g. `cp -al`, or `rsync --link-dest`. A hardlink IS
> the file: it shares one inode with whatever it links to, so chowning the
> scratch copy to the target run-as-user identity chowns the ORIGINAL file
> too, and no permission fix-up sqi applies afterward can separate the two —
> there is nothing left to distinguish. `rsync -a` (no `--link-dest`) and the
> built-in copy above are both safe: they always copy bytes into a fresh
> inode.

### `staging.defaults`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `true` |
| **Env var** | `SQI_STAGING_DEFAULTS` |

When true (the default), a worker that hasn't configured `scratch_dir` and
`sync_command` at all still runs `stage_locally` jobs: it falls back to the
TEMP scratch directory and the built-in copy described above, logging a
one-time WARN the first time it does so. Set `staging.defaults: false` to
disable that automatic fallback — a worker with neither key set then fails
`stage_locally` jobs immediately with a pre-execution error, as every worker
did before this setting existed. (An explicit `sync_command: builtin` still
works even with `defaults: false` — see above.)

```yaml
staging:
  scratch_dir: /scratch/sqi
  sync_command: "rsync -a {src} {dest}"
  defaults: true
```

> **Behavior change on upgrade.** Before this setting existed, any worker that
> ran a `stage_locally` job without `staging.scratch_dir` and
> `staging.sync_command` configured failed the task immediately with a
> staging error. `staging.defaults` now defaults to `true`, so those same
> workers instead run the job using TEMP scratch and the built-in copy. Set
> `staging.defaults: false` (or `SQI_STAGING_DEFAULTS=false`) to restore the
> old fail-hard behavior.

---

## `diagnostics` — Diagnostic-log publishing

### `diagnostics.enabled`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `true` |
| **Env var** | `SQI_DIAGNOSTICS_ENABLED` (note: no `WORKER` infix) |

When enabled (the default) the worker publishes its own `slog` output to the
ephemeral core-NATS subject `worker.diag.<workerID>`, which the server ingests
into its diagnostics ring buffer and surfaces in the web UI. Set to `false` to
suppress publishing (the worker still logs to stderr). This is the worker
counterpart to the server's `diagnostics.buffer_size` knob.

```yaml
diagnostics:
  enabled: true
```

```sh
SQI_DIAGNOSTICS_ENABLED=false sqi-worker start
```

---

## `log` — Structured logging

### `log.level`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"info"` |
| **Accepted values** | `debug`, `info`, `warn`, `error` |
| **Env var** | `SQI_WORKER_LOG_LEVEL` |
| **CLI flag** | `--log-level` |

Minimum log level to emit. Use `debug` during initial deployment to verify
registration and task flow. Switch to `info` or `warn` in production.

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
| **Env var** | `SQI_WORKER_LOG_FORMAT` |
| **CLI flag** | `--log-format` |

Log output format. `json` is structured and machine-parseable — use it in
production so log aggregators (Loki, Datadog, Splunk, etc.) can index fields.
`text` is human-readable with aligned columns — use it during local
development.

```yaml
log:
  format: "json"
```

---

## `metrics` — Local HTTP server

The worker exposes a small HTTP server on loopback for container orchestration
health probes and Prometheus metrics. This server is not exposed to the
network by default.

| Path | Purpose |
|---|---|
| `/healthz` | Liveness probe — always returns 200 when the process is running |
| `/readyz` | Readiness probe — returns 503 when the NATS connection is not connected |
| `/metrics` | Prometheus metrics endpoint |
| `/debug/pprof/` | Go profiling endpoints (only when `metrics.enable_pprof: true`) |

### `metrics.addr`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"127.0.0.1:9091"` |
| **Env var** | `SQI_WORKER_METRICS_ADDR` |

TCP address the local HTTP server listens on. Use `0.0.0.0:9091` to expose
the endpoints to Prometheus scrapers on the network (ensure the port is
firewalled appropriately).

When [running multiple workers on one host](#running-multiple-workers-on-one-host),
give each instance a distinct port — otherwise the second worker fails to bind.

```yaml
metrics:
  addr: "127.0.0.1:9091"
```

---

### `metrics.enable_pprof`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `false` |
| **Env var** | `SQI_WORKER_METRICS_ENABLE_PPROF` |

Expose Go runtime profiling endpoints at `/debug/pprof/`. Profiling data
reveals memory layout, goroutine stacks, and CPU hotspots — enable
temporarily for performance diagnosis, never in long-term production.

```yaml
metrics:
  enable_pprof: false
```

---

## `discovery` — mDNS server auto-discovery

### `discovery.enable_mdns`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `true` |
| **Env var** | `SQI_WORKER_DISCOVERY_ENABLE_MDNS` |

Enable mDNS-based `sqi-server` auto-discovery. When `true` and `nats.url` is
empty, the worker browses for `_sqi._tcp` services on the local network.
Disable on networks that prohibit multicast — most cloud VPCs, VLANs, and
container networks.

```yaml
discovery:
  enable_mdns: true
```

---

### `discovery.mdns_timeout`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `"5s"` |
| **Env var** | `SQI_WORKER_DISCOVERY_MDNS_TIMEOUT` |

Maximum time to wait for an mDNS discovery result before giving up. Increase
if the server is slow to respond on a congested network. Only relevant when
`discovery.enable_mdns` is `true`.

```yaml
discovery:
  mdns_timeout: "5s"
```

---

## `log_streamer` — Log chunk publisher

Controls how task process stdout/stderr is batched and streamed to
`sqi-server` via NATS JetStream. Tune these values to balance NATS message
overhead against how live the web UI log viewer feels.

### `log_streamer.max_lines_per_chunk`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `50` |
| **Env var** | `SQI_WORKER_LOG_STREAMER_MAX_LINES_PER_CHUNK` |

Maximum number of output lines batched into a single NATS message. A flush is
triggered immediately when the buffer reaches this count. Increase for very
verbose processes to reduce per-message overhead; decrease for better
granularity when watching live log output.

```yaml
log_streamer:
  max_lines_per_chunk: 50
```

---

### `log_streamer.max_bytes_per_chunk`

| | |
|---|---|
| **Type** | `int` (bytes) |
| **Default** | `16384` (16 KB) |
| **Env var** | `SQI_WORKER_LOG_STREAMER_MAX_BYTES_PER_CHUNK` |

Maximum total byte count of line content in a single NATS message. A flush is
triggered when the accumulated bytes reach this limit after adding a line.
Guards against a single very long line producing an oversized message.

```yaml
log_streamer:
  max_bytes_per_chunk: 16384
```

---

### `log_streamer.flush_interval`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `"500ms"` |
| **Env var** | `SQI_WORKER_LOG_STREAMER_FLUSH_INTERVAL` |

Maximum time a line may sit in the buffer before being flushed regardless of
the chunk size thresholds. Smaller values make the web UI log viewer feel
more live at the cost of more frequent small NATS publishes on slowly printing
processes.

```yaml
log_streamer:
  flush_interval: "500ms"
```

---

## Quick reference table

| Key | Type | Default | Env var | CLI flag |
|---|---|---|---|---|
| `nats.url` | string | `""` | `SQI_WORKER_NATS_URL` | — |
| `nats.tls_cert_file` | string | `""` | `SQI_WORKER_NATS_TLS_CERT_FILE` | — |
| `nats.tls_key_file` | string | `""` | `SQI_WORKER_NATS_TLS_KEY_FILE` | — |
| `nats.tls_ca_file` | string | `""` | `SQI_WORKER_NATS_TLS_CA_FILE` | — |
| `nats.insecure_skip_verify` | bool | `false` | `SQI_WORKER_NATS_INSECURE_SKIP_VERIFY` | `--nats-insecure-skip-verify` |
| `nats.max_reconnect_attempts` | int | `-1` | `SQI_WORKER_NATS_MAX_RECONNECT_ATTEMPTS` | — |
| `nats.reconnect_wait` | duration | `2s` | `SQI_WORKER_NATS_RECONNECT_WAIT` | — |
| `worker.name` | string | hostname | `SQI_WORKER_NAME` | — |
| `worker.farm_id` | string | `""` | `SQI_WORKER_FARM_ID` | — |
| `worker.data_dir` | string | `~/.sqi/worker` (Linux/macOS); `%USERPROFILE%\.sqi\worker` (Windows) | `SQI_WORKER_DATA_DIR` | — |
| `worker.session_dir` | string | `""` → resolved at startup | `SQI_WORKER_SESSION_DIR` | — |
| `worker.compute_location` | string | `""` | `SQI_WORKER_COMPUTE_LOCATION` | — |
| `worker.capability_tags` | []string | `[]` | `SQI_WORKER_CAPABILITY_TAGS` | — |
| `worker.heartbeat_interval` | duration | `15s` | `SQI_WORKER_HEARTBEAT_INTERVAL` | — |
| `worker.shutdown_grace_period` | duration | `30s` | `SQI_WORKER_SHUTDOWN_GRACE_PERIOD` | — |
| `worker.allow_root` | bool | `false` | `SQI_WORKER_ALLOW_ROOT` | — |
| `worker.keep_failed_sessions` | bool | `false` | `SQI_WORKER_KEEP_FAILED_SESSIONS` | — |
| `worker.queue_ids` | []string | `[]` | `SQI_WORKER_QUEUE_IDS` | — |
| `worker.pull_idle_backoff` | duration | `2s` | `SQI_WORKER_PULL_IDLE_BACKOFF` | — (deprecated, no effect) |
| `worker.pull_nack_delay` | duration | `5s` | `SQI_WORKER_PULL_NACK_DELAY` | — (deprecated, no effect) |
| `capabilities.detect` | `[]Detector` | `[]` | — (config file only) | — |
| `capabilities.disable` | `[]string` | `[]` | `SQI_WORKER_CAPABILITIES_DISABLE` | — |
| `staging.scratch_dir` | string | `""` | — (config file only) | — |
| `staging.sync_command` | string | `""` | — (config file only) | — |
| `staging.defaults` | bool | `true` | `SQI_STAGING_DEFAULTS` | — |
| `diagnostics.enabled` | bool | `true` | `SQI_DIAGNOSTICS_ENABLED` | — |
| `log.level` | string | `info` | `SQI_WORKER_LOG_LEVEL` | `--log-level` |
| `log.format` | string | `json` | `SQI_WORKER_LOG_FORMAT` | `--log-format` |
| `metrics.addr` | string | `127.0.0.1:9091` | `SQI_WORKER_METRICS_ADDR` | — |
| `metrics.enable_pprof` | bool | `false` | `SQI_WORKER_METRICS_ENABLE_PPROF` | — |
| `discovery.enable_mdns` | bool | `true` | `SQI_WORKER_DISCOVERY_ENABLE_MDNS` | — |
| `discovery.mdns_timeout` | duration | `5s` | `SQI_WORKER_DISCOVERY_MDNS_TIMEOUT` | — |
| `log_streamer.max_lines_per_chunk` | int | `50` | `SQI_WORKER_LOG_STREAMER_MAX_LINES_PER_CHUNK` | — |
| `log_streamer.max_bytes_per_chunk` | int | `16384` | `SQI_WORKER_LOG_STREAMER_MAX_BYTES_PER_CHUNK` | — |
| `log_streamer.flush_interval` | duration | `500ms` | `SQI_WORKER_LOG_STREAMER_FLUSH_INTERVAL` | — |

---

## Worked example: GPU render farm node

A node in a GPU render farm running Houdini 20.5 with a single NVIDIA GPU:

```yaml
# /etc/sqi/sqi-worker.yaml

nats:
  url: "nats://render-server.studio.local:4222"

discovery:
  enable_mdns: false

worker:
  name: "gpu-node-04"
  farm_id: "studio-main"
  data_dir: "/var/lib/sqi-worker"
  compute_location: "nas-studio"
  capability_tags:
    - houdini-20.5
    - karma-renderer
    - gpu
  heartbeat_interval: "15s"
  shutdown_grace_period: "10m" # Houdini frames can take several minutes

log:
  level: "info"
  format: "json"

metrics:
  addr: "0.0.0.0:9091"        # expose to Prometheus scraper
```

---

## Running multiple workers on one host

A single worker executes tasks in parallel — the server leases as many tasks as
fit the worker's CPU cores (see [CPU reservations in OpenJD
templates](openjd-submission.md#5-cpu-reservations)). The worker runs whatever
it is leased; concurrency is gated entirely by the server's CPU-core accounting
(`amount.worker.vcpu`). There is no local task-count cap.

Run *separate* worker processes when you want distinct worker identities:
independent heartbeats and registrations, different capability sets, or
separate queue assignments — useful for local farm simulation and testing the
scheduler. `sqi-worker` dials out to the server's NATS (it binds no inbound
ports except the metrics server), so multiple instances coexist on one host as
long as three things differ per instance:

| Setting | Env var | Why it must differ |
|---|---|---|
| [`worker.data_dir`](#workerdata_dir) | `SQI_WORKER_DATA_DIR` | Holds the persistent `worker.id` UUID; a shared dir means a duplicate identity on the server. |
| [`metrics.addr`](#metricsaddr) | `SQI_WORKER_METRICS_ADDR` | The local health/metrics HTTP server; a second instance on the same port fails to bind. |
| [`worker.name`](#workername) | `SQI_WORKER_NAME` | Cosmetic only — defaults to the hostname, so instances would otherwise share a label in the web UI. |

Everything else (NATS URL, discovery, capability tags) can be shared or vary
as you like.

For local development the `make run-workers` target wires all of this up for
you — see
[Running multiple workers locally](development.md#running-multiple-workers-locally)
in the development guide. A manual example starting three workers:

```sh
for i in 1 2 3; do
  SQI_WORKER_NAME="worker-$i" \
  SQI_WORKER_DATA_DIR="$HOME/.sqi/worker-$i" \
  SQI_WORKER_METRICS_ADDR="127.0.0.1:$((9090 + i))" \
    sqi-worker start &
done
```

---

## See also

- [`config/sqi-worker.example.yaml`](https://github.com/uberware/sqi/blob/main/config/sqi-worker.example.yaml) — Fully commented example with every option.
- [`docs/worker-capabilities.md`](worker-capabilities.md) — Auto-detected capability tags and how to override them.
- [`docs/worker-deployment.md`](worker-deployment.md) — systemd, launchd, and Windows service installation.
- [`docs/configuration.md`](configuration.md) — `sqi-server` configuration reference.
