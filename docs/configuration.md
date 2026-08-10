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
[`config/sqi-server.example.yaml`](https://github.com/uberware/sqi/blob/main/config/sqi-server.example.yaml).

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

### `http.cors_origins`

| | |
|---|---|
| **Type** | `[]string` |
| **Default** | `[]` (empty — treated as `["*"]`) |
| **Env var** | `SQI_HTTP_CORS_ORIGINS` (comma-separated) |
| **CLI flag** | `--http-cors-origins` |

Browser origins the CORS middleware allows. Only relevant to a
**separately-hosted web UI** calling this server cross-origin; the normal
same-origin deployment (where `sqi-server` serves the embedded UI itself)
needs none of this.

Each entry must be `scheme://host[:port]`, or the wildcard `"*"`. A trailing
slash, a path, a query, a fragment, or embedded whitespace is rejected at
startup with an `http.cors_origins` validation error — go-chi/cors could
never match such a value, so a typo fails loudly at boot rather than
silently at request time.

**With `auth.enabled=true` a wildcard is dropped at startup** (and an error
is logged): browsers reject `Access-Control-Allow-Credentials` combined with
`*`. An empty list defaults to `["*"]` and so is dropped too — meaning a
separately-hosted UI must name its origin explicitly here for credentialed
cross-origin requests to work at all. See
[auth.md § CSRF & CORS](auth.md#csrf--cors).

```yaml
http:
  cors_origins:
    - "https://ui.example.com"
    - "http://localhost:5173"
```

```sh
SQI_HTTP_CORS_ORIGINS="https://ui.example.com,http://localhost:5173"
sqi-server serve --http-cors-origins=https://ui.example.com
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
(single-machine only). **Broker authentication does not exist**: any host
that can reach this port can register as a worker and receive task
assignments, regardless of `auth.enabled` — see
[Known gaps](auth.md#known-gaps). Deferred to Phase 4 hardening.

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

> **Reserved — not yet wired.** This key is parsed and validated but the
> scheduler does not currently consume it; the assignment loop runs on a fixed
> 1s interval (`AssignInterval`). Setting it has no effect today. Must be `> 0`.

Intended meaning: how often the assignment loop wakes to match ready tasks to
idle workers.

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

> **Reserved — not yet wired.** This key is parsed and validated but the
> scheduler does not currently consume it; task concurrency is governed by
> CPU-core commitment (`CPUCount − committed`), not a per-worker task cap.
> Setting it has no effect today. Must be `≥ 1`.

Intended meaning: the maximum number of tasks simultaneously assigned to a
single worker.

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

### `scheduler.job_retention`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `168h` (7 days) |
| **Env var** | `SQI_SCHEDULER_JOB_RETENTION` |

How long a terminal job is retained before the retention sweep hard-deletes it
and all of its data (steps, tasks, attempts, logs). The sweep runs on the
heartbeat-sweep tick and removes completed and canceled jobs whose completion
time is older than this window; failed jobs are governed by
`job_retention_include_failed`. Active jobs are never auto-deleted. Set to `0`
to disable automatic deletion (jobs can still be deleted manually).

```yaml
scheduler:
  job_retention: "168h"
```

---

### `scheduler.job_retention_include_failed`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `false` |
| **Env var** | `SQI_SCHEDULER_JOB_RETENTION_INCLUDE_FAILED` |

When `true`, the retention sweep also removes failed jobs older than
`job_retention`. Default keeps failed jobs for post-mortem debugging. No effect
when `job_retention` is `0`.

```yaml
scheduler:
  job_retention_include_failed: false
```

---

### `scheduler.unschedulable_grace`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `30s` |
| **Env var** | `SQI_SCHEDULER_UNSCHEDULABLE_GRACE` |

How long a `ready` task may wait with no eligible online worker before it is
flagged "unschedulable" (surfaced in the API/UI so operators can spot stuck
work rather than assume the scheduler is silently making progress). Set to `0`
to disable the sweep entirely.

```yaml
scheduler:
  unschedulable_grace: "30s"
```

See
[Why isn't my job running? — Unschedulable tasks](observability.md#why-isnt-my-job-running--unschedulable-tasks)
for what the flag means, where it surfaces (task `unschedulable_reason`, job
`task_counts.unschedulable`, the job-detail badge), and how it clears.

---

## Retry & failure limits

Worker-reported task failures auto-retry with backoff up to a per-task attempt
ceiling, and a job auto-parks (pauses) once its cumulative genuine failures
reach a failure limit. Three server keys set the farm-wide defaults for this
policy:

### `scheduler.default_max_attempts`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `3` |
| **Env var** | `SQI_SCHEDULER_DEFAULT_MAX_ATTEMPTS` |

Farm-wide default number of genuine attempts a task may make before it goes
terminal-`failed`. Must be `≥ 1`; `1` disables auto-retry (a single failure is
immediately terminal, matching pre-retry behavior).

```yaml
scheduler:
  default_max_attempts: 3
```

### `scheduler.retry_delay`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `"30s"` |
| **Env var** | `SQI_SCHEDULER_RETRY_DELAY` |

Backoff applied before a failed task re-enters the `ready` queue for
re-lease. `0` means immediate re-queue.

```yaml
scheduler:
  retry_delay: "30s"
```

### `scheduler.default_failure_limit`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `0` |
| **Env var** | `SQI_SCHEDULER_DEFAULT_FAILURE_LIMIT` |

Farm-wide default ceiling on a job's cumulative genuine task failures. Once
reached, the job is auto-parked (`status=paused`, with a `park_reason` such as
`"failure limit reached (N)"`). `0` disables the limit (off — a job never
auto-parks on failure count alone).

```yaml
scheduler:
  default_failure_limit: 0
```

### Precedence: Server → Farm → Queue → Job

The effective value for each of the three knobs above is resolved
independently as the **first non-null** of, in order: **Job → Queue → Farm →
server default**. A farm sets a studio-wide policy; a queue can narrow it for
a class of work; a job can override it for one submission. Any tier left
unset (`null`) falls through to the next.

Farm and queue overrides are set via the farm/queue REST resources'
`max_attempts` / `retry_delay_seconds` / `failure_limit` fields (and the
matching Python SDK keyword arguments on `create_farm`/`update_farm` and
`create_queue`/`update_queue`). A job's overrides use the same three names,
but on `POST /api/v1/jobs` they travel as **query parameters** — alongside
`priority` — not as a body field, since the body is the raw OpenJD template:

```sh
curl -X POST "http://localhost:8080/api/v1/jobs?max_attempts=5&retry_delay_seconds=10&failure_limit=20" \
  -H 'Content-Type: application/yaml' \
  --data-binary @job.yaml
```

Overrides are validated at the API boundary: `max_attempts` must be >= 1,
`retry_delay_seconds` and `failure_limit` must be >= 0 (an explicit
`failure_limit` of 0 disables an inherited limit). Out-of-range or
non-integer values are rejected with `400` rather than stored or silently
ignored. The job detail response (`GET /api/v1/jobs/{id}`) reports both the
configured per-job overrides and `effective_retry` — the fully resolved
policy the job actually runs under.

A manual `POST /api/v1/tasks/{id}/retry` or `POST /api/v1/jobs/{id}/retry`
resets a task/job's failure counters, independent of this policy. See
[the task state machine](architecture.md#state-machine-task-status) for how
retry, exhaustion, and auto-park interact, and
[Retry and auto-park metrics](observability.md#retry-and-auto-park-metrics)
for the Prometheus counters.

> **Behavior change on upgrade.** The default `scheduler.default_max_attempts`
> is `3`, so farms that upgrade into this feature will see transient task
> failures auto-retry (up to 2 extra attempts) where they previously went
> straight to terminal-`failed`. To restore the prior no-auto-retry behavior,
> set `scheduler.default_max_attempts: 1` (or
> `SQI_SCHEDULER_DEFAULT_MAX_ATTEMPTS=1`).

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

Must not be empty, and this is checked **regardless of `discovery.enabled`** —
explicitly setting it to `""` fails config validation and the server will not
start, even with discovery switched off. Leave the default in place rather than
blanking it to disable advertisement; use `discovery.enabled: false` for that.

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
counts, and host-requirement counts — **plus one sqi-defined limit the OpenJD
specification does not itself set: a maximum of 100 steps per job template**.
A template exceeding 100 steps is rejected with an error at `/steps`. Set to
`false` only in operator environments that predate strict limit enforcement
and cannot yet update all templates — including templates with more than 100
steps; resource-exhaustion guards (the per-range value cap and the per-step
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
> rather than buffer) — see [Worker configuration](#worker-configuration) below.

See [`docs/observability.md`](observability.md) for the full diagnostics guide.

---

## `preset_library` — Remote preset catalog

### `preset_library.url`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"https://uberware.github.io/sqi-presets/index.json"` |
| **Env var** | `SQI_PRESET_LIBRARY_URL` |

URL of the preset library's JSON index. The default points to the official community
preset library hosted on GitHub Pages.

Set to an empty string `""` to **disable** the feature. When disabled, the Preset
Library browse page shows a "not configured" empty state and all `/api/v1/presets`
endpoints return 503.

```yaml
preset_library:
  url: "https://uberware.github.io/sqi-presets/index.json"
```

To use a private or self-hosted library, provide any accessible HTTP or HTTPS URL
that serves the index JSON. See [`docs/preset-library.md`](preset-library.md) for the
index format and full integration guide.

---

## `auth` — Authentication (Phase 3, opt-in)

### `auth.enabled`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `false` |
| **Env var** | `SQI_AUTH_ENABLED` |
| **CLI flag** | `--auth-enabled` |

The single switch for sqi's authentication gate. Default `false` — the server
is open on a trusted local network and every request is served as an anonymous
superuser.

As of component A1, this is a live gate: setting it to `true` requires every
REST request and the WebSocket upgrade to carry a valid session, backed by
local accounts (see below and [`docs/auth.md`](auth.md)). Role-based
authorization is not enforced yet (component B1) — see the interim gap
documented in [`docs/auth.md`](auth.md#local-accounts).

```yaml
auth:
  enabled: false
```

---

### `auth.validate_job_owner`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `true` |
| **Env var** | `SQI_AUTH_VALIDATE_JOB_OWNER` |
| **CLI flag** | `--auth-validate-job-owner` |

Rejects a job submission whose `owner` (set via a `jobs.submit_as` override)
names no known user, with `400`. Default `true` — it keeps `Job.Owner` a
trustworthy key for owner-scoped visibility: the `user` role's job listings are
filtered by owner, so a typo'd owner yields a job invisible to the person who
actually owns it and missed by an admin filtering on them. Disable it when
owners come from a directory that has not yet provisioned local records.

```yaml
auth:
  validate_job_owner: true
```

---

### `auth.session.ttl`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `168h` (7 days) |
| **Env var** | `SQI_AUTH_SESSION_TTL` |

Absolute lifetime of a session created by `POST /api/v1/auth/login`, from
creation. There is no sliding/idle renewal — using a session does not extend
it. Must be `> 0` when `auth.enabled` is `true`; ignored (no validation) when
auth is disabled.

```yaml
auth:
  session:
    ttl: "168h"
```

---

### `auth.session.cookie_name`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"sqi_session"` |
| **Env var** | `SQI_AUTH_SESSION_COOKIE_NAME` |

Name of the session cookie set by login and cleared by logout. Change this
only if `sqi_session` collides with another cookie on the same domain (e.g.
sqi served under a shared reverse-proxy host alongside another app).

Must not be empty when `auth.enabled` is true — the server fails to start
otherwise. The name is deliberately required rather than defaulted at the point
of use: the CSRF middleware reads the cookie by name, and an empty name would
make every mutating request take the "no session cookie" exempt path, silently
disabling CSRF protection.

```yaml
auth:
  session:
    cookie_name: "sqi_session"
```

---

### `auth.session.cookie_secure`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"auto"` |
| **Accepted values** | `auto`, `true`, `false` |
| **Env var** | `SQI_AUTH_SESSION_COOKIE_SECURE` |

Controls the session cookie's `Secure` attribute. `"auto"` sets `Secure` when
the request arrived over TLS or carries `X-Forwarded-Proto: https` — correct
behind a TLS-terminating proxy. sqi's default deployment posture is a
trusted, plain-HTTP LAN, so `"false"` lets an operator force `Secure` off
explicitly on such a deployment rather than rely on `"auto"` guessing right;
`"true"` forces it on. Any other value fails config validation when
`auth.enabled` is `true`.

```yaml
auth:
  session:
    cookie_secure: "auto"
```

---

### `auth.bootstrap.username` / `auth.bootstrap.password`

| | |
|---|---|
| **Type** | `string` / `string` |
| **Default** | `""` / `""` |
| **Env var** | `SQI_AUTH_BOOTSTRAP_USERNAME` / `SQI_AUTH_BOOTSTRAP_PASSWORD` |

Seed credentials for the first admin account, applied once at startup only
when `auth.enabled` is `true` **and** the `users` table is empty; the account
is created with `role: "admin"`. Idempotent — once any user exists, these
values have no further effect and never overwrite an existing password.
Leaving both empty is valid: an auth-enabled server with no users just logs a
`WARN` and boots without an admin, until an operator sets both and restarts.
Setting only one of the two is a startup validation error (guards against a
typo'd env var name).

`auth.bootstrap.password` is **redacted** (`<redacted>`) in
`sqi-server config print` and any other re-marshaled dump of the config — it
never appears in that output, even though the loaded value is used normally.

```yaml
auth:
  bootstrap:
    username: "admin"
    password: "change-me-after-first-login"
```

See [`docs/auth.md`](auth.md) for the full authentication model, the local
account model, and the interim authorization gap before role enforcement
(component B1).

---

### `auth.ldap.*`

Directory (LDAP / Active Directory) authentication, component C1. Every key
below sits under `auth.ldap`. **No `ldap.*` key has a CLI flag** — these are
file- or environment-configured only. `role_map` is additionally **file-only:
it has no environment form**, because a list of group→role pairs has no
sensible flat encoding.

The whole block is inert while `auth.ldap.enabled` is false: the server never
builds a verifier and never contacts a directory, whatever else is set here.
Validation of the keys below only runs when `auth.ldap.enabled` is true.

Setting `auth.ldap.enabled: true` while `auth.enabled` is false is **not** a
silent no-op — it is a configuration error, and the server refuses to start:

```
auth.ldap.enabled: requires auth.enabled=true; LDAP without the auth gate authenticates nobody
```

Enable both together, or neither. You cannot stage an LDAP block behind a
closed auth gate.

| Key | Type | Default | Env var |
|---|---|---|---|
| `auth.ldap.enabled` | bool | `false` | `SQI_AUTH_LDAP_ENABLED` |
| `auth.ldap.url` | string | `""` | `SQI_AUTH_LDAP_URL` |
| `auth.ldap.start_tls` | bool | `false` | `SQI_AUTH_LDAP_START_TLS` |
| `auth.ldap.tls_skip_verify` | bool | `false` | `SQI_AUTH_LDAP_TLS_SKIP_VERIFY` |
| `auth.ldap.ca_file` | string | `""` | `SQI_AUTH_LDAP_CA_FILE` |
| `auth.ldap.timeout` | duration | `10s` | `SQI_AUTH_LDAP_TIMEOUT` |
| `auth.ldap.bind_dn` | string | `""` | `SQI_AUTH_LDAP_BIND_DN` |
| `auth.ldap.bind_password` | string | `""` | `SQI_AUTH_LDAP_BIND_PASSWORD` |
| `auth.ldap.base_dn` | string | `""` | `SQI_AUTH_LDAP_BASE_DN` |
| `auth.ldap.user_filter` | string | `(sAMAccountName=%s)` | `SQI_AUTH_LDAP_USER_FILTER` |
| `auth.ldap.nested_groups` | bool | `false` | `SQI_AUTH_LDAP_NESTED_GROUPS` |
| `auth.ldap.user_dn_template` | string | `""` | `SQI_AUTH_LDAP_USER_DN_TEMPLATE` |
| `auth.ldap.username_attr` | string | `sAMAccountName` | `SQI_AUTH_LDAP_USERNAME_ATTR` |
| `auth.ldap.display_name_attr` | string | `displayName` | `SQI_AUTH_LDAP_DISPLAY_NAME_ATTR` |
| `auth.ldap.unique_id_attr` | string | *(none — required)* | `SQI_AUTH_LDAP_UNIQUE_ID_ATTR` |
| `auth.ldap.role_source` | string | `directory` | `SQI_AUTH_LDAP_ROLE_SOURCE` |
| `auth.ldap.role_map` | list | `[]` | *(file only — no env form)* |
| `auth.ldap.default_role` | string | `read-only` | `SQI_AUTH_LDAP_DEFAULT_ROLE` |

**Transport.** `url` must be `ldap://…` or `ldaps://…` and is required when
enabled. `start_tls` upgrades a plain `ldap://` connection and is rejected
against `ldaps://`, which is already TLS. `ca_file` is a PEM bundle used to
verify the directory's certificate; an unreadable file **aborts boot** rather
than yielding a server that looks healthy but can authenticate nobody.
`tls_skip_verify` disables certificate verification entirely — a MITM can
then harvest every password that crosses the connection — so it is logged as
a `WARN` at boot and should never be set outside a lab. `timeout` must be
`> 0`; it bounds the TCP connect and each request leg, and is the *only*
bound on a hung directory (see
[A hung directory](auth.md#a-hung-directory)).

**Bind mode** — mutually exclusive, one is required:

- **Search-then-bind:** set `base_dn` (required) and usually `bind_dn` +
  `bind_password`. `user_filter` must contain `%s`, the placeholder for the
  escaped username. Leaving `bind_dn` empty selects **anonymous search**,
  which is valid; setting `bind_password` *without* `bind_dn` is a validation
  error, because the password would be silently discarded.
- **Template bind:** set `user_dn_template`, which must contain `%s` (e.g.
  `uid=%s,ou=people,dc=example,dc=com`). Combining it with
  `bind_dn`/`base_dn` is a validation error.

`nested_groups` requires search mode — template bind reads the flat
`memberOf` attribute, so the combination is rejected at boot rather than
silently ignored.

**Roles.** `role_source` must be `directory` or `local` (see
[`role_source`](auth.md#role_source--who-owns-a-users-role) for what each
means and why one value drives both the login re-sync and the API's 409).
`role_map` is ordered and **first match wins**; each entry needs a non-empty
`group` and a `role` that is one of `admin`, `operator`, `user`, `read-only`
— an unknown role fails validation rather than falling through to
`default_role`. `default_role` accepts those same four values **or empty**,
where empty means reject any login that matched no group.

`bind_password` is **redacted** (`<redacted>`) in `sqi-server config print`,
the same as `auth.bootstrap.password`. Prefer `SQI_AUTH_LDAP_BIND_PASSWORD`
over writing it into a config file regardless — a secret that never lands on
disk cannot leak from one.

```yaml
auth:
  enabled: true
  ldap:
    enabled: true
    url: "ldaps://dc01.example.com:636"
    timeout: "10s"
    bind_dn: "CN=sqi-svc,OU=Service Accounts,DC=example,DC=com"
    # bind_password via SQI_AUTH_LDAP_BIND_PASSWORD
    base_dn: "DC=example,DC=com"
    user_filter: "(sAMAccountName=%s)"
    username_attr: "sAMAccountName"
    display_name_attr: "displayName"
    unique_id_attr: "objectGUID"   # "entryUUID" on OpenLDAP; no default, always required
    nested_groups: true
    role_source: "directory"
    role_map:
      - group: "CN=Farm Admins,OU=Groups,DC=example,DC=com"
        role: admin
      - group: "CN=Farm Operators,OU=Groups,DC=example,DC=com"
        role: operator
    default_role: "read-only"
```

**`unique_id_attr` is a breaking change for an existing enabled-LDAP
deployment.** It has no default anywhere — not in the defaults struct, not in
the loader, not in the environment — because no single value is correct on both
Active Directory (`objectGUID`) and RFC 4530 servers (`entryUUID`), and
guessing on a server exposing both would silently pick the wrong one. A config
with `auth.ldap.enabled: true` and no `unique_id_attr` **fails validation and
the server does not boot**. Accounts provisioned before this key existed carry
an empty identifier and must be recreated — see
[Upgrading from an earlier sqi](auth.md#upgrading-from-an-earlier-sqi).

See [`docs/auth.md`](auth.md#ldap--active-directory) for the model behind
these keys: why LDAP attaches at login rather than per request, how
just-in-time provisioning works, why accounts match on a stable identifier
rather than a username, why a local admin account is required in `directory`
mode, and the revocation-lag and timing caveats.

---

### `auth.oidc.*`

OAuth2/OIDC single sign-on, component C2. Every key below sits under
`auth.oidc`. **No `oidc.*` key has a CLI flag** — these are file- or
environment-configured only. `role_map` is additionally **file-only: it has
no environment form**, for the same reason as `auth.ldap.role_map` — a list of
group→role pairs has no sensible flat encoding.

The whole block is inert while `auth.oidc.enabled` is false: the server never
builds the SSO route, whatever else is set here. Validation of the keys below
only runs when `auth.oidc.enabled` is true.

Setting `auth.oidc.enabled: true` while `auth.enabled` is false is **not** a
silent no-op — it is a configuration error, and the server refuses to start:

```
auth.oidc.enabled: requires auth.enabled=true; SSO without the auth gate authenticates nobody
```

Enable both together, or neither. Unlike LDAP, `issuer` discovery happens lazily on
first use, not at boot — a briefly unreachable provider must not stop the
scheduler from starting.

Deliberately a single provider block, not a list: almost every organization
has one identity provider.

| Key | Type | Default | Env var |
|---|---|---|---|
| `auth.oidc.enabled` | bool | `false` | `SQI_AUTH_OIDC_ENABLED` |
| `auth.oidc.issuer` | string | `""` | `SQI_AUTH_OIDC_ISSUER` |
| `auth.oidc.client_id` | string | `""` | `SQI_AUTH_OIDC_CLIENT_ID` |
| `auth.oidc.client_secret` | string | `""` | `SQI_AUTH_OIDC_CLIENT_SECRET` |
| `auth.oidc.redirect_url` | string | `""` | `SQI_AUTH_OIDC_REDIRECT_URL` |
| `auth.oidc.scopes` | []string | `[openid, profile, email]` | `SQI_AUTH_OIDC_SCOPES` (comma-separated) |
| `auth.oidc.username_claim` | string | `preferred_username` | `SQI_AUTH_OIDC_USERNAME_CLAIM` |
| `auth.oidc.display_name_claim` | string | `name` | `SQI_AUTH_OIDC_DISPLAY_NAME_CLAIM` |
| `auth.oidc.groups_claim` | string | `groups` | `SQI_AUTH_OIDC_GROUPS_CLAIM` |
| `auth.oidc.role_source` | string | `directory` | `SQI_AUTH_OIDC_ROLE_SOURCE` |
| `auth.oidc.role_map` | list | `[]` | *(file only — no env form)* |
| `auth.oidc.default_role` | string | `read-only` | `SQI_AUTH_OIDC_DEFAULT_ROLE` |
| `auth.oidc.reauth_mode` | string | `after_logout` | `SQI_AUTH_OIDC_REAUTH_MODE` |
| `auth.oidc.logout_mode` | string | `local` | `SQI_AUTH_OIDC_LOGOUT_MODE` |
| `auth.oidc.post_logout_redirect_url` | string | `""` | `SQI_AUTH_OIDC_POST_LOGOUT_REDIRECT_URL` |
| `auth.oidc.button_label` | string | `Sign in with SSO` | `SQI_AUTH_OIDC_BUTTON_LABEL` |

**Endpoint.** `issuer`, `client_id`, `client_secret`, and `redirect_url` are
all required when enabled — none has a safe empty meaning. `issuer` and
`redirect_url` must each be an absolute URL. `redirect_url` must resolve to
this server's `/api/v1/auth/oidc/callback`; `post_logout_redirect_url` is
only read (and only required) when `logout_mode` is `"provider"`.

**Claims.** `scopes` must be non-empty and include `"openid"`; note that
`"groups"` is *not* standard OIDC — whether group membership needs a scope, a
provider-side mapper, or both varies by provider. `username_claim` and
`display_name_claim` carry defaults, so an empty value means the operator
explicitly cleared it (same reasoning as `auth.ldap.username_attr` /
`display_name_attr`); `groups_claim` feeds `role_map` the same way.

**Roles.** `role_source` must be `directory` or `local`, mirroring
`auth.ldap.role_source` but tracked separately — an operator may trust one
provider's groups and not the other's. `role_map` is ordered and **first
match wins**; each entry needs a non-empty `group` and a `role` that is one
of `admin`, `operator`, `user`, `read-only` — an unknown role fails
validation rather than falling through to `default_role`. `default_role`
accepts those same four values **or empty**, where empty means reject any
login that matched no group.

**Reauth and logout.** `reauth_mode` is one of `after_logout` (re-prompt only
on the login following an explicit logout), `always` (re-prompt every
login), or `never` (silent re-login always permitted). `logout_mode` is
`local` (end only the sqi session) or `provider` (also end the session at the
identity provider, signing the user out of every tool that trusts it — off
by default because of that blast radius).

`client_secret` is **redacted** (`<redacted>`) in `sqi-server config print`,
the same as `auth.ldap.bind_password`. Prefer `SQI_AUTH_OIDC_CLIENT_SECRET`
over writing it into a config file regardless — a secret that never lands on
disk cannot leak from one.

```yaml
auth:
  enabled: true
  oidc:
    enabled: true
    issuer: "https://login.microsoftonline.com/<tenant>/v2.0"
    client_id: "sqi"
    # client_secret via SQI_AUTH_OIDC_CLIENT_SECRET
    redirect_url: "https://sqi.example.com/api/v1/auth/oidc/callback"
    scopes: ["openid", "profile", "email"]
    username_claim: "preferred_username"
    display_name_claim: "name"
    groups_claim: "groups"
    role_source: "directory"
    role_map:
      - group: "sqi-farm-admins"
        role: admin
      - group: "sqi-farm-operators"
        role: operator
    default_role: "read-only"
    reauth_mode: "after_logout"
    logout_mode: "local"
```

The mode constants (`ReauthAfterLogout`/`ReauthAlways`/`ReauthNever`,
`LogoutLocal`/`LogoutProvider`, `RoleSourceDirectory`/`RoleSourceLocal`) live
in `internal/auth/oidc`, not `internal/config` — `internal/api` reads them
directly and must not import the config loader, the same boundary that
`toLDAPConfig` exists to keep for LDAP.

See [`docs/auth.md`](auth.md#oidc--sso) for the model behind these keys: the
login flow and its defenses, why accounts match on the `sub` claim, how
`reauth_mode` and `logout_mode` differ, and the limits — revocation lag, roles
applying at next login only, what `logout_mode: provider` actually does on
Keycloak, and which providers CI does and does not cover.

---

## Queue identity: `run_as_user` / `run_as_group` (task isolation)

Unlike everything else in this reference, `run_as_user` and `run_as_group`
are not `sqi-server` config-file keys — there is no server-wide or
farm-wide default for either. They are set per queue, via the queue REST
resource only (`POST /api/v1/queues`, `PUT /api/v1/queues/{id}`) — there is
no web UI field or Python SDK field for either yet — and are documented here
because, like the retry overrides above, they are queue-level settings that
change worker behavior.

| Field | Type | Default | Effect |
|---|---|---|---|
| `run_as_user` | `string \| null` | `null` | OS username tasks (and OpenJD environment `onEnter`/`onExit` actions) in this queue execute as. `null` — the default — means no isolation: tasks run as the worker daemon's own account, identical to a worker with no isolation feature at all. |
| `run_as_group` | `string \| null` | `null` | OS group for the same tasks. `null` means the target user's primary group. |

Both fields are deliberately **excluded** from the Farm → server-default
cascade that the retry-policy fields above use: a farm-wide default would
silently apply an OS identity to a queue whose owner never configured one.
Every queue's isolation setting is explicit or absent — never inherited.

**Permission.** Setting either field — including sending an explicit `null`
to clear a previously-set value — requires the `isolation.manage`
permission, held only by the `admin` role; `infra.manage` (which `operator`
holds, and which gates every other queue field) is not sufficient. **Omitting
both keys from a `PUT` body** preserves whatever is currently stored and
requires no permission — this is a deliberate exception to `PUT`'s normal
full-replace semantics, so that an `operator` without `isolation.manage` can
still edit a queue's priority or concurrency limit without silently clearing
an `admin`'s isolation configuration on the same request. See
[Task isolation](auth.md#task-isolation) in the auth guide for the full
reasoning, including why enabling isolation raises the *worker daemon's*
privilege even though it lowers an individual *task's*.

**Validation.** `run_as_group` requires `run_as_user` to also be set (in the
same request, or already stored and preserved by an omitted key) — the
scheduler only gates isolation on `run_as_user`, so a group with no user
would select no OS identity at all and be silently ignored. Both `POST` and
`PUT` reject that combination with `400 Bad Request`.

The scheduler places only the resolved **username** in the task assignment
sent to the worker over NATS — never a credential — because worker↔server
transport carries no authentication at all (see
[Known gaps](auth.md#known-gaps)). The worker resolves that username to a
real OS credential itself. This mechanism runs on both POSIX (Linux/macOS)
and Windows (the worker must run as a LocalSystem service, or hold
`SeAssignPrimaryTokenPrivilege`, to assume another account's identity); see
[`docs/worker-configuration.md`](worker-configuration.md#isolation--run-as-user-task-execution)
for the full worker-side `isolation` config block, the environment allowlist,
and the per-platform requirements.

### Important: Worker upgrade required

**`run_as_user` is only enforced by workers that support task isolation.** A
worker binary built before isolation support shipped will silently ignore the
`isolation` field in its task assignments and run job code as the worker
daemon's own OS user. The scheduler does not filter assignments by worker
capability, so an admin who sets `run_as_user` on a queue while even one
un-upgraded worker remains in the farm gets silent, partial enforcement —
some tasks isolated, some not, with no indication which.

This is the asymmetry worth understanding:

- **A worker that *supports* isolation but is misconfigured fails closed and
  loudly**: it refuses to start, or fails the individual task with an
  actionable error message.
- **An old worker fails open and silently**: it accepts the assignment,
  executes the task unisolated, and reports success. There is no error
  anywhere.

**Guidance:** upgrade all workers to a binary that supports task isolation
before enabling `run_as_user` on any queue. Do not mix binary versions.

A proper solution would require workers to advertise isolation capability and
the scheduler to refuse isolation-required tasks to workers lacking it — a
protocol change deferred as a future improvement. For now, the only way to
ensure consistent enforcement is to roll the farm forward in lockstep.

---

## Worker configuration

Worker configuration applies to `sqi-worker` instances, not the server. Workers
load configuration from the same layered sources as the server (defaults → file →
environment → flags), except that environment variables use the `SQI_WORKER_`
prefix and the config file is named `sqi-worker.yaml`.

The full worker key reference — NATS connection, identity, metrics, discovery, log
streaming, and more — lives in
[`docs/worker-configuration.md`](worker-configuration.md), and a fully commented
example is at
[`config/sqi-worker.example.yaml`](https://github.com/uberware/sqi/blob/main/config/sqi-worker.example.yaml).
The keys below are summarized here for operators configuring a worker alongside
the server; the same keys are covered in full in that reference. `staging`
bridges server storage configuration to worker execution, `diagnostics` mirrors
the server-side diagnostics setting, and `capabilities` configures the worker's
software auto-detection.

### Staging (`stage_locally` path delivery)

Used by workers that run jobs declaring the `stage_locally` delivery of
`SQI_PATH_TRANSLATION`.

| Key | Type | Description |
|---|---|---|
| `staging.scratch_dir` | string | Base directory for per-attempt staged copies. Defaults to `<os.TempDir()>/sqi-staging` when unset. |
| `staging.sync_command` | string | Command template invoked per path, with `{src}`, `{dest}`, and optional `{object_type}` placeholders (e.g. `rsync -a {src} {dest}`). The same template serves copy-in and copy-out. Left unset (or set to `builtin`), sqi copies the bytes itself instead of shelling out. |
| `staging.defaults` | bool | Default `true`. When true, an otherwise-unconfigured worker still runs `stage_locally` jobs via the TEMP scratch dir and built-in copy above (one-time WARN logged). Set `false` to make an unconfigured worker fail `stage_locally` jobs immediately instead. |

```yaml
staging:
  scratch_dir: "/tmp/sqi-staging"
  sync_command: "rsync -a {src} {dest}"
  defaults: true
```

Full detail, including the built-in copy's local/dev caveat and the
`staging.defaults` behavior change, is in
[Worker configuration → `staging`](worker-configuration.md#staging--local-path-staging-stage_locally-delivery).

### Diagnostics (`diagnostics.enabled`)

Controls whether the worker mirrors its own structured (`slog`) output to
`sqi-server` over core NATS, where it appears in the web UI alongside server logs.
This is the worker agent's operational log — distinct from task process output,
which is always streamed. This is the worker's counterpart to the server's
`diagnostics.buffer_size` (workers publish; the server buffers).

| Key | Type | Default | Env var | Description |
|---|---|---|---|---|
| `diagnostics.enabled` | bool | `true` | `SQI_DIAGNOSTICS_ENABLED` | When `true`, the worker's diagnostic-log records are published to `sqi-server` in addition to local stderr. Set `false` to keep them local only. |

Note the env var is `SQI_DIAGNOSTICS_ENABLED` (not `SQI_WORKER_…`), matching the
server-side diagnostics naming.

```yaml
diagnostics:
  enabled: true
```

See [`docs/observability.md`](observability.md) for the full diagnostics guide.

### Capability auto-detection (`capabilities.detect` / `capabilities.disable`)

Controls the worker's software capability auto-detection: built-in detectors
for Maya, Nuke, Houdini, and Blender run automatically at startup and
advertise a tag (e.g. `maya`, plus `maya-2025`) with value `"true"`, with no
per-worker configuration — enough on its own to satisfy the reference DCC
products/presets' `key=true` requirement. See
[`docs/products.md`](products.md) and
[`docs/dcc-submitters.md`](dcc-submitters.md) for how these auto-detected
tags relate to the reference DCC products/presets.

| Key | Type | Default | Env var | Description |
|---|---|---|---|---|
| `capabilities.detect` | `[]Detector` | `[]` | — (config file only) | Custom detectors for in-house tools, same schema as the built-ins. |
| `capabilities.disable` | `[]string` | `[]` | `SQI_WORKER_CAPABILITIES_DISABLE` (comma-separated, appended) | Turn off a built-in detector by tag name, e.g. `[blender]`. |

```yaml
capabilities:
  detect:
    - tag: mytool
      checks:
        - exe: mytool
  disable: [blender]
```

See [`docs/worker-capabilities.md`](worker-capabilities.md#capability-auto-detection-built-in-dcc-detectors)
for the full auto-detection guide (how it runs, the tag/version model, the
`sqi-worker capabilities` command) and
[`docs/worker-capabilities.md`](worker-capabilities.md#writing-custom-detectors)
for the detector schema reference.

---

## Quick reference table

| Key | Type | Default | Env var | CLI flag |
|---|---|---|---|---|
| `http.addr` | string | `0.0.0.0:8080` | `SQI_HTTP_ADDR` | `--http-addr` |
| `http.enable_pprof` | bool | `false` | `SQI_HTTP_ENABLE_PPROF` | — |
| `http.cors_origins` | []string | `[]` (= `*`) | `SQI_HTTP_CORS_ORIGINS` | `--http-cors-origins` |
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
| `scheduler.offline_worker_retention` | duration | `24h` | `SQI_SCHEDULER_OFFLINE_WORKER_RETENTION` | — |
| `scheduler.job_retention` | duration | `168h` | `SQI_SCHEDULER_JOB_RETENTION` | — |
| `scheduler.job_retention_include_failed` | bool | `false` | `SQI_SCHEDULER_JOB_RETENTION_INCLUDE_FAILED` | — |
| `scheduler.unschedulable_grace` | duration | `30s` | `SQI_SCHEDULER_UNSCHEDULABLE_GRACE` | — |
| `scheduler.default_max_attempts` | int | `3` | `SQI_SCHEDULER_DEFAULT_MAX_ATTEMPTS` | — |
| `scheduler.retry_delay` | duration | `30s` | `SQI_SCHEDULER_RETRY_DELAY` | — |
| `scheduler.default_failure_limit` | int | `0` | `SQI_SCHEDULER_DEFAULT_FAILURE_LIMIT` | — |
| `discovery.enabled` | bool | `true` | `SQI_DISCOVERY_ENABLED` | — |
| `discovery.instance_name` | string | `sqi-server` | `SQI_DISCOVERY_INSTANCE_NAME` | — |
| `openjd.enforce_limits` | bool | `true` | `SQI_OPENJD_ENFORCE_LIMITS` | `--openjd-enforce-limits` |
| `diagnostics.buffer_size` | int | `1000` | `SQI_DIAGNOSTICS_BUFFER_SIZE` | — |
| `preset_library.url` | string | `https://uberware.github.io/sqi-presets/index.json` | `SQI_PRESET_LIBRARY_URL` | — |
| `auth.enabled` | bool | `false` | `SQI_AUTH_ENABLED` | `--auth-enabled` |
| `auth.validate_job_owner` | bool | `true` | `SQI_AUTH_VALIDATE_JOB_OWNER` | `--auth-validate-job-owner` |
| `auth.session.ttl` | duration | `168h` | `SQI_AUTH_SESSION_TTL` | — |
| `auth.session.cookie_name` | string | `sqi_session` | `SQI_AUTH_SESSION_COOKIE_NAME` | — |
| `auth.session.cookie_secure` | string | `auto` | `SQI_AUTH_SESSION_COOKIE_SECURE` | — |
| `auth.bootstrap.username` | string | `""` | `SQI_AUTH_BOOTSTRAP_USERNAME` | — |
| `auth.bootstrap.password` | string | `""` | `SQI_AUTH_BOOTSTRAP_PASSWORD` | — |
| `auth.ldap.enabled` | bool | `false` | `SQI_AUTH_LDAP_ENABLED` | — |
| `auth.ldap.url` | string | `""` | `SQI_AUTH_LDAP_URL` | — |
| `auth.ldap.start_tls` | bool | `false` | `SQI_AUTH_LDAP_START_TLS` | — |
| `auth.ldap.tls_skip_verify` | bool | `false` | `SQI_AUTH_LDAP_TLS_SKIP_VERIFY` | — |
| `auth.ldap.ca_file` | string | `""` | `SQI_AUTH_LDAP_CA_FILE` | — |
| `auth.ldap.timeout` | duration | `10s` | `SQI_AUTH_LDAP_TIMEOUT` | — |
| `auth.ldap.bind_dn` | string | `""` | `SQI_AUTH_LDAP_BIND_DN` | — |
| `auth.ldap.bind_password` | string | `""` | `SQI_AUTH_LDAP_BIND_PASSWORD` | — |
| `auth.ldap.base_dn` | string | `""` | `SQI_AUTH_LDAP_BASE_DN` | — |
| `auth.ldap.user_filter` | string | `(sAMAccountName=%s)` | `SQI_AUTH_LDAP_USER_FILTER` | — |
| `auth.ldap.nested_groups` | bool | `false` | `SQI_AUTH_LDAP_NESTED_GROUPS` | — |
| `auth.ldap.user_dn_template` | string | `""` | `SQI_AUTH_LDAP_USER_DN_TEMPLATE` | — |
| `auth.ldap.username_attr` | string | `sAMAccountName` | `SQI_AUTH_LDAP_USERNAME_ATTR` | — |
| `auth.ldap.display_name_attr` | string | `displayName` | `SQI_AUTH_LDAP_DISPLAY_NAME_ATTR` | — |
| `auth.ldap.unique_id_attr` | string | *(none — required)* | `SQI_AUTH_LDAP_UNIQUE_ID_ATTR` | — |
| `auth.ldap.role_source` | string | `directory` | `SQI_AUTH_LDAP_ROLE_SOURCE` | — |
| `auth.ldap.role_map` | list | `[]` | — | — |
| `auth.ldap.default_role` | string | `read-only` | `SQI_AUTH_LDAP_DEFAULT_ROLE` | — |
| `auth.oidc.enabled` | bool | `false` | `SQI_AUTH_OIDC_ENABLED` | — |
| `auth.oidc.issuer` | string | `""` | `SQI_AUTH_OIDC_ISSUER` | — |
| `auth.oidc.client_id` | string | `""` | `SQI_AUTH_OIDC_CLIENT_ID` | — |
| `auth.oidc.client_secret` | string | `""` | `SQI_AUTH_OIDC_CLIENT_SECRET` | — |
| `auth.oidc.redirect_url` | string | `""` | `SQI_AUTH_OIDC_REDIRECT_URL` | — |
| `auth.oidc.scopes` | []string | `[openid, profile, email]` | `SQI_AUTH_OIDC_SCOPES` | — |
| `auth.oidc.username_claim` | string | `preferred_username` | `SQI_AUTH_OIDC_USERNAME_CLAIM` | — |
| `auth.oidc.display_name_claim` | string | `name` | `SQI_AUTH_OIDC_DISPLAY_NAME_CLAIM` | — |
| `auth.oidc.groups_claim` | string | `groups` | `SQI_AUTH_OIDC_GROUPS_CLAIM` | — |
| `auth.oidc.role_source` | string | `directory` | `SQI_AUTH_OIDC_ROLE_SOURCE` | — |
| `auth.oidc.role_map` | list | `[]` | — | — |
| `auth.oidc.default_role` | string | `read-only` | `SQI_AUTH_OIDC_DEFAULT_ROLE` | — |
| `auth.oidc.reauth_mode` | string | `after_logout` | `SQI_AUTH_OIDC_REAUTH_MODE` | — |
| `auth.oidc.logout_mode` | string | `local` | `SQI_AUTH_OIDC_LOGOUT_MODE` | — |
| `auth.oidc.post_logout_redirect_url` | string | `""` | `SQI_AUTH_OIDC_POST_LOGOUT_REDIRECT_URL` | — |
| `auth.oidc.button_label` | string | `Sign in with SSO` | `SQI_AUTH_OIDC_BUTTON_LABEL` | — |

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

- [`config/sqi-server.example.yaml`](https://github.com/uberware/sqi/blob/main/config/sqi-server.example.yaml) — Fully
  commented example with every option.
- [`docs/architecture.md`](architecture.md) — Component layout and how configuration values are consumed.
- [`docs/operations.md`](operations.md) — Install, upgrade, backup, and log rotation.
- [`docs/observability.md`](observability.md) — In-UI diagnostics, REST/WS log API, and external log wiring.
