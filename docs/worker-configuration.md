# sqi-worker Configuration Reference

`sqi-worker` is configured through four layers applied in order, with later
layers overriding earlier ones:

1. **Built-in defaults**, suitable for local development.
2. **Config file**, YAML or JSON. Without `--config /path/to/file`, the worker
   uses the first file it finds among `./config/sqi-worker.yaml`,
   `~/.sqi/sqi-worker.yaml` and `/etc/sqi/sqi-worker.yaml`, trying each path
   with a `.json` extension after the `.yaml` one. A Windows service does not
   search: it refuses to start without `--config` (see
   [registering the service by hand](worker-deployment.md#registering-the-service-by-hand)).
3. **Environment variables**, prefixed `SQI_WORKER_`, e.g.
   `SQI_WORKER_NATS_URL`. Two have no `WORKER` infix: `diagnostics.enabled`
   uses `SQI_DIAGNOSTICS_ENABLED` and `staging.defaults` uses
   `SQI_STAGING_DEFAULTS`. A numeric or duration variable that does not parse
   is ignored, leaving the value from the lower layers; a boolean variable
   that does not parse is read as `false`.
4. **CLI flags**, highest priority. `--config`/`-c`, `--log-level` and
   `--log-format` are root flags available on every subcommand; `--dry-run` and
   `--nats-insecure-skip-verify` belong to `start` only.

Print the effective merged configuration at any time with:

```sh
sqi-worker config print
```

Validate configuration and print the capabilities the worker would register,
without connecting to the server:

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

URL of the NATS server embedded in `sqi-server`, e.g.
`nats://sqi-server.local:4222`. Required when `discovery.enable_mdns` is
`false`. When set, mDNS discovery is skipped.

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

### `nats.tls_enabled`

| | |
|---|---|
| **Type** | `string` — `"auto"`, `"true"` or `"false"` |
| **Default** | `"auto"` |
| **Env var** | `SQI_WORKER_NATS_TLS_ENABLED` |

Whether to use TLS on the broker connection. Three-valued, like the server's
[`auth.session.cookie_secure`](configuration.md#authsessioncookie_secure):

| Value | Meaning |
|---|---|
| `"auto"` | Use TLS when any `nats.tls_*` file or `nats.insecure_skip_verify` is set, or when the server discovered over mDNS advertises a TLS-required broker (`nats_tls=1`) |
| `"true"` | Always use TLS. With no `tls_ca_file`, the certificate is verified against the system roots, which works for a publicly trusted broker certificate and fails for a farm CA |
| `"false"` | Never use TLS, even with a CA configured |

The file and the environment also accept `1`/`yes`/`on` for `"true"` and
`0`/`no`/`off` for `"false"`. Any other value fails startup with a
`nats.tls_enabled` validation error.

Under `"auto"`, a worker that discovers a TLS-required broker enables TLS and
logs that it did; if no `tls_ca_file` is configured it also warns that the
system roots cannot verify a farm CA. Under `"false"`, a worker that
discovers a TLS-required broker keeps plaintext and warns that the broker will
refuse the connection, because the broker's own error does not name this
setting.

```yaml
nats:
  tls_enabled: "auto"
```

---

### `nats.tls_ca_file`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` (use system CA pool) |
| **Env var** | `SQI_WORKER_NATS_TLS_CA_FILE` |

Path to the CA certificate (PEM-encoded) that verifies the broker's TLS
certificate, i.e. the one configured as `nats.tls.cert_file` on the server.

Empty means the system certificate pool. When set, only certificates issued by
this CA are accepted. Separate from
[`nats.server_tls_ca_file`](#natsserver_tls_ca_file), which verifies the HTTPS
API on a different port and may have a different issuer.

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

Skip verification of the broker's TLS certificate. Development only: with
verification off, the connection can be intercepted. Under
`nats.tls_enabled: "auto"`, setting this also turns TLS on.

---

### `nats.max_reconnect_attempts`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `-1` (retry indefinitely) |
| **Env var** | `SQI_WORKER_NATS_MAX_RECONNECT_ATTEMPTS` |

Maximum number of reconnection attempts before the worker gives up and shuts
down. `-1` retries indefinitely and is the recommended setting. Delays between
attempts are set by [`nats.reconnect_wait`](#natsreconnect_wait).

```yaml
nats:
  max_reconnect_attempts: -1
```

> **Keep this above `0`.** When broker authentication is on and the server
> rejects this worker's credential mid-run (usually after
> `sqi-server worker revoke` or `DELETE /api/v1/workers/{id}/credential`), the
> worker logs that its credential was rejected and that it must re-enroll. It
> learns the reason from a reconnect attempt, so with `0` it attempts none and
> logs only a generic "connection closed".

---

### `nats.reconnect_wait`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `"2s"` |
| **Env var** | `SQI_WORKER_NATS_RECONNECT_WAIT` |

Delay before the first reconnection attempt. Each later attempt doubles the
delay, up to 2 minutes, and every delay varies randomly by ±20% so workers
that lose the server together do not reconnect in step. Not validated at load;
`0` reconnects with no delay.

```yaml
nats:
  reconnect_wait: "2s"
```

---

### `nats.credential_file`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` (resolves to `<worker.data_dir>/worker.nk`) |
| **Env var** | `SQI_WORKER_NATS_CREDENTIAL_FILE` |

Path to this worker's nkey seed file, which it presents to authenticate to the
broker. Only used when the server's
[`nats.auth.enabled`](configuration.md#natsauthenabled) is `true`. Empty
resolves to `<worker.data_dir>/worker.nk`.

Enrollment and `sqi-worker keygen` both create the file at the resolved path;
`keygen` loads the same configuration layers as `start`, so a non-default
path set here or in `SQI_WORKER_NATS_CREDENTIAL_FILE` applies to both. The
file is written owner-only, and the worker refuses to load one that anyone
else can read (POSIX mode `0600`, an owner-only ACL on Windows). The worker
writes it through a temporary file in the same directory and renames it into
place, so it needs write permission on the directory: a directory set to
`0500` fails the write even when the seed file inside is owner-writable.

```yaml
nats:
  credential_file: "/var/lib/sqi-worker/worker.nk"
```

---

### `nats.join_token`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` |
| **Env var** | `SQI_WORKER_NATS_JOIN_TOKEN` |

A worker enrollment token, used once on first start to obtain a credential
and ignored once `nats.credential_file` exists. Prefer
[`nats.join_token_file`](#natsjoin_token_file), since a token in a config file
is a secret stored at rest.

```yaml
nats:
  join_token: ""
```

---

### `nats.join_token_file`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` |
| **Env var** | `SQI_WORKER_NATS_JOIN_TOKEN_FILE` |

Path to a file containing a join token. Takes precedence over
[`nats.join_token`](#natsjoin_token).

```yaml
nats:
  join_token_file: "/run/secrets/sqi-join-token"
```

---

### `nats.server_url`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` |
| **Env var** | `SQI_WORKER_NATS_SERVER_URL` |

`sqi-server` HTTP base URL used for enrollment, e.g.
`"http://sqi-server.example:8080"`. Enrollment runs over REST because the
broker rejects unauthenticated connections, so a worker cannot get its first
credential over NATS.

mDNS discovery does not supply this value. Set it whenever
[`nats.join_token`](#natsjoin_token) or
[`nats.join_token_file`](#natsjoin_token_file) is configured; a worker that
needs to enroll without it fails with an error naming this field.

When the server has `http.tls` enabled this must be an `https://` URL, and
[`nats.server_tls_ca_file`](#natsserver_tls_ca_file) must name the CA that
signed its certificate. A worker that discovers an HTTPS server over mDNS
warns at startup when this URL is `http://`.

```yaml
nats:
  server_url: "https://sqi-server.example:8080"
```

---

### `nats.server_tls_ca_file`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` (use system CA pool) |
| **Env var** | `SQI_WORKER_NATS_SERVER_TLS_CA_FILE` |

Path to the CA certificate that verifies `sqi-server`'s HTTPS certificate at
[`nats.server_url`](#natsserver_url), for enrollment.

Separate from [`nats.tls_ca_file`](#natstls_ca_file) because the two verify
different certificates on different ports, which can come from different
issuers (for example a publicly issued certificate on the API and a farm CA on
the broker). On a farm set up with `sqi-server tls init` both are the same
`ca.crt`.

Enrollment needs this when the server uses a private CA: without it, a worker
cannot verify the HTTPS server and so cannot obtain its first credential.
Empty means the system roots; when set, only this CA is accepted.

```yaml
nats:
  server_tls_ca_file: "/etc/sqi/ca.crt"
```

---

### `nats.server_tls_insecure_skip_verify`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `false` |
| **Env var** | `SQI_WORKER_NATS_SERVER_TLS_INSECURE_SKIP_VERIFY` |

Skip verification of `sqi-server`'s HTTPS certificate during enrollment.
Development only: it accepts any certificate, so the enrollment request and
its join token can be intercepted.

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
belonging to that farm. When empty (the default), the worker accepts tasks
from any farm, which suits single-farm and development setups. Set it on farms
that must not share workers.

```yaml
worker:
  farm_id: "studio-a"
```

---

### `worker.data_dir`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `~/.sqi/worker` (Linux/macOS); `%USERPROFILE%\.sqi\worker` (Windows); `/var/lib/sqi-worker` when no home directory can be determined |
| **Env var** | `SQI_WORKER_DATA_DIR` |

Directory holding the worker ID file (`worker.id`), the default location of
the nkey seed (`worker.nk`), and on Windows the DPAPI-encrypted run-as-user
credential store (`<data_dir>\isolation\`). Created on first start as a
private (`0700`) directory, and never widened for run-as-user traversal.

The worker ID lets the server recognize this worker across restarts. Do not
delete `worker.id` unless you intend to re-register as a new worker. For
production, use an absolute path on local disk.

Each worker instance needs its own `data_dir`: two workers sharing one load
the same `worker.id` and collide on the server. See
[running multiple workers on one host](#running-multiple-workers-on-one-host).

Session working directories are configured separately by
[`worker.session_dir`](#workersession_dir). With it unset, they live outside
`data_dir` on any worker that can use run-as-user isolation (a root POSIX
worker, or any Windows worker), and at `<data_dir>/sessions` on a non-root
POSIX worker, which cannot isolate.

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
(`<session_dir>/<sessionID>/`). Kept separate from `data_dir` because, under
run-as-user isolation (see [`isolation`](#isolation--run-as-user-task-execution)),
session scratch must be traversable by the run-as-user account, while
`data_dir` holds the worker ID and must stay private. An explicitly set
directory is created at `0711` if it does not exist.

Left unset, the value is resolved at startup:

- **Running as root (POSIX)**: `/var/lib/sqi-worker-sessions`, created at
  `0711`. It sits beside `data_dir`'s no-home fallback (`/var/lib/sqi-worker`)
  rather than inside it, because a session root under the `0700` `data_dir`
  would fail the traversal check described below.
- **Windows (any account)**: `%ProgramData%\sqi\worker\sessions`. A worker
  running as LocalSystem has its home directory under
  `System32\config\systemprofile`, which is unsuitable for render scratch.
  Directory modes have no effect on Windows; each session directory is
  protected by an NTFS DACL instead (see [Windows](#windows) below).
- **Otherwise (non-root POSIX)**: `<data_dir>/sessions`, created at `0750`. A
  non-root worker cannot run tasks as another user, so session scratch needs no
  wider access.

```yaml
worker:
  session_dir: "/var/lib/sqi-worker-sessions"
```

> **Never widened.** sqi does not `chmod` an existing directory to make it
> traversable. A directory is either created at the correct mode, or the
> worker refuses with an error naming the ancestor that needs `chmod o+x`.
> At boot this check runs only when `isolation.required: true`, so a root
> worker that never receives an isolated assignment can still start with a
> restricted `staging.scratch_dir` or `session_dir`. Otherwise the same check
> runs when an assignment with run-as-user isolation arrives, and fails only
> that task.

> **A root worker's default session root is on `/var`.** The root/non-root
> choice above depends only on the worker's effective uid at startup, not on
> whether any queue sets `run_as_user`. If `data_dir` is on a dedicated
> scratch volume, set `session_dir` to a directory on that volume too, but
> not inside `data_dir`: a session root under the `0700` `data_dir` fails the
> traversal check above on the first isolated assignment.

---

### `worker.compute_location`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` (none) |
| **Env var** | `SQI_WORKER_COMPUTE_LOCATION` |

Named compute location for this worker. When set, `sqi-server` registers the
name in its compute-location registry if it is not there already, so the entry
need not be created first. The value is used for:

- **Storage-location path mapping**: the server resolves `loc://` URI
  references using the root keyed by this name in each storage location (see
  [`docs/storage-locations.md`](storage-locations.md)).
- **Step affinity matching**: steps that declare
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

Capability tags merged with auto-detected capabilities at registration. Use
them for software or hardware the detectors cannot discover. An entry is a
bare tag (`gpu`) or a `key=value` pair (`maya=false`), and it overrides any
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

### `worker.commands`

| | |
|---|---|
| **Type** | map of command name → `{path, args}` |
| **Default** | `{}` (none) |
| **Env var** | none (config file only) |

Says where a command a job runs really lives. A job template names the program
it runs (`nuke`, `blender`); this table maps that name to the executable to
start instead, with optional arguments placed **before** the ones the job
supplies.

The worker starts a job's command directly, with no shell, so a shell alias
never resolves, and some software installs under a versioned name (Nuke on
Windows is `Nuke17.1.exe`, never `nuke`). Without an entry the worker looks the
name up on its own `PATH`, exactly as before.

An entry is the first thing consulted. After it, a command of the same name
already on `PATH` is used as is, and only if there is neither does a detector's
default apply: the Nuke built-in supplies one from the newest install it finds,
with no arguments (see [Detected commands](worker-capabilities.md#detected-commands)).
`sqi-worker capabilities` prints what each command resolves to and why.

```yaml
worker:
  commands:
    nuke:
      path: "C:/Program Files/Nuke17.1v2/Nuke17.1.exe"
      args: ["--nc"]            # Nuke's non-commercial license flag
    ffmpeg:
      path: ffmpeg-7            # a bare name is resolved through the worker's PATH
```

- **`path`** is required: an absolute path, or a bare name resolved through the
  worker's `PATH`. A relative path with a directory part is rejected, because a
  task's working directory is a per-session scratch directory. On Windows write
  paths with forward slashes in YAML, or single-quote them.
- **`args`** are literal. They are not format strings, and are placed first, so
  `args: ["--nc"]` runs `Nuke17.1.exe --nc -x -X Write1 …`.
- **The name** must be a bare command name, with no `/` or (on Windows) `\`. A
  job that names a path is naming a file, so a mapping never captures it. On
  Windows names match case-insensitively, as the platform does.
- **Where it applies:** every command a job starts: a task's `onRun`, and an
  environment's `onEnter` and `onExit`.
- **Who controls it:** only the operator. The table is worker configuration;
  nothing a job or the server sends can add to it.
- **Run-as-user isolation:** the mapped path is started under the session's
  account like any other command, so that account must be able to execute it.

The worker logs each mapping at startup, and warns about a mapping whose target
cannot be found on that host, so a mistyped path shows up in the worker log
rather than as a job failure.

---

### `worker.heartbeat_interval`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `"15s"` |
| **Env var** | `SQI_WORKER_HEARTBEAT_INTERVAL` |

How often the worker publishes a heartbeat to `sqi-server`. The server marks a
worker stale when heartbeats stop, so keep this shorter than the server's
`scheduler.heartbeat_timeout` (default 30 s); the default sends two per
timeout window. Must be `> 0`.

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

Maximum time the worker waits for in-flight tasks to finish after SIGINT,
SIGTERM or a Windows service stop. Tasks still running at the end of this
window receive SIGTERM, then SIGKILL a third of this period later, and are
reported as `failed`. On workers you restart while they are busy, set it to
your longest expected task duration. Must be `> 0`.

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

Allow the worker to run as root on Linux and macOS; with `false`, a root
worker refuses to start. Disabled by default because running render processes
as root is a security risk. Enable it inside a container where root is the
only user, or for run-as-user isolation (see
[`isolation.required`](#isolationrequired)). Ignored on Windows.

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

Keep a session's working directory after the session fails (task
cancellation, non-zero exit code, or environment setup error), for inspecting
partial outputs and environment state. Leave it off in production so failed
sessions do not fill [`worker.session_dir`](#workersession_dir) on busy
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

Restrict this worker to specific queue IDs. The worker keeps one outstanding
lease request per listed queue, on `work.lease.<workerID>.<queueID>`. When
empty (the default), it issues a single lease request on
`work.lease.<workerID>._any`, and the server offers it ready work from any
queue it is eligible for. Set this on farms where some workers serve only a
subset of queues.

Each entry becomes one NATS subject token, so an entry that is empty or
contains `.`, whitespace, `*` or `>` is rejected at load with an error
naming it. Such an entry would otherwise produce lease requests that the
broker refuses or the server never answers, with nothing in the worker's log
saying why.

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

> **Deprecated.** Accepted by the config loader but has no effect (a negative
> value still fails validation). The server holds an unfulfilled lease
> request for up to 30 s and the worker re-issues it on return, so there is
> no polling loop to throttle.

---

### `worker.pull_nack_delay`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `"5s"` |
| **Env var** | `SQI_WORKER_PULL_NACK_DELAY` |

> **Deprecated.** Accepted by the config loader but has no effect (a negative
> value still fails validation). The server checks eligibility before
> leasing a task, and the worker runs what it is given.

---

## `isolation` — Run-as-user task execution

Runs job-supplied task and environment actions as the OS user configured on
the queue (`run_as_user`/`run_as_group`; see
[`docs/configuration.md`](configuration.md#queue-identity-run_as_user--run_as_group-task-isolation)
and [`docs/auth.md`](auth.md#task-isolation)) instead of the worker daemon's
own account. Supported on POSIX (Linux/macOS) and on Windows through the
`logon_user` provider; see [Windows](#windows) below for its setup. When no
queue is configured for isolation, this section has no effect.

> **Enabling this raises the worker daemon's own privilege.** On POSIX, sqi
> switches to another user with `setuid`/`setgid`/`setgroups`, which requires
> the daemon to run as root: `isolation.Provider.Capable()` fails unless the
> effective uid is 0. For a worker that runs unprivileged today, isolation
> gives the daemon root so that tasks can run without it.

### `isolation.required`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `false` |
| **Env var** | `SQI_WORKER_ISOLATION_REQUIRED` |

Exit at boot if this worker cannot assume another OS identity (on POSIX, when
it is not running as root). With the default `false`, the worker starts
anyway, and an isolated assignment that it cannot run fails only that task.
The worker learns which queues use isolation only from each assignment, so
this setting is the way to make a misconfigured worker fail at boot.

On POSIX, `isolation.required: true` conflicts with `worker.allow_root: false`
(the default): isolation needs root, and `allow_root: false` refuses to start
as root. Config validation rejects the combination at load with a message
naming both keys. Set `worker.allow_root: true` alongside it, or leave
`isolation.required: false`. The check does not apply on Windows, where
`worker.allow_root` has no effect and the provider checks its own privileges
at boot.

```yaml
isolation:
  required: true
worker:
  allow_root: true
```

### `isolation.provider`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"logon_user"` |
| **Accepted values** | `logon_user` (`s4u` is recognised and refused; any other value fails Windows provider construction) |
| **Env var** | `SQI_WORKER_ISOLATION_PROVIDER` |

Selects the Windows credential mechanism. Ignored on Linux and macOS.
`logon_user` is the only supported value; `s4u` is refused (see
[Windows](#windows) below).

### `isolation.env_passthrough`

| | |
|---|---|
| **Type** | `[]string` |
| **Default** | `[]` (empty — only the minimal base is inherited) |
| **Env var** | `SQI_WORKER_ISOLATION_ENV_PASSTHROUGH` (comma-separated) |

Environment variable name patterns (`filepath.Match` globs, e.g.
`FLEXLM_*`) that an isolated task inherits from the worker daemon's
environment, in addition to a minimal base (`PATH`, `HOME`/`USERPROFILE`,
`TMPDIR` and a few others, rewritten for the target user where they name the
daemon's). Each pattern is validated at config load; an invalid glob (e.g. an
unbalanced `[`) fails startup naming the field, `isolation.env_passthrough[N]`.

This applies only to variables inherited from the daemon. Variables the job
supplies (an OpenJD `Environment.variables` block, an `openjd_env` export, a
task template variable) always reach the task.

The usual reason to set it is licensing: many license managers are configured
by an environment variable on the render node, and that variable must reach
the isolated task:

```yaml
isolation:
  env_passthrough:
    - "foundry_LICENSE"     # Nuke
    - "ARNOLD_LICENSE"      # Arnold (Maya/Houdini)
    - "solidangle_LICENSE"  # Arnold, older variable name
```

> **Avoid broad globs.** `"*"`, or a pattern like `"*KEY*"`, passes the
> daemon's own credentials, tokens and internal service addresses to every
> isolated task. List the exact variable names. When a task fails on a
> missing license variable, add that one name.

### Windows

On Windows, task isolation uses the `logon_user` provider.
`make test-isolation-windows` tests it against real local accounts, NTFS ACLs,
DPAPI credential storage and processes launched under the target account's
token. See [Known gaps](auth.md#known-gaps) in `docs/auth.md` for its
remaining limitations.

**The worker must run as a service under LocalSystem**, or as an account
granted `SeAssignPrimaryTokenPrivilege`. Windows requires that privilege to
start a process under another account's token, and an elevated Administrator
does not hold it by default. A missing privilege is the most common cause
of `isolation: worker cannot assume another OS identity` on Windows;
`Capable()` reports it at boot and names the fix.

`Capable()` also requires `SeIncreaseQuotaPrivilege` (needed by
`CreateProcessAsUser`), and loading the target account's profile needs
`SeBackupPrivilege` and `SeRestorePrivilege`. LocalSystem holds all four. An
account you install the service under with `sqi-worker service install --user …`
holds them only if you grant them (Local Security Policy → User Rights
Assignment: *Replace a process level token*, *Adjust memory quotas for a
process*, *Back up files and directories* and *Restore files and
directories*). Granting them may not be enough: Microsoft documents
`LoadUserProfile` as callable only by an administrator or LocalSystem. Treat
LocalSystem as the supported account for run-as-user isolation. See
[Choosing the account](worker-deployment.md#choosing-the-account) for
installing the worker under another account.

**Each run-as-user account needs the "Log on as a batch job" right**
(`SeBatchLogonRight`). The provider logs the account on with
`LOGON32_LOGON_BATCH`, which does not require "Allow log on locally" and,
unlike an S4U logon, carries network credentials. Default workstation policy
grants that right only to Administrators, Backup Operators and Performance Log
Users, so a purpose-made standard account does not hold it until you grant
it, and credential resolution fails with:

    isolation: logon "render-svc": LogonUserW: Logon failure: the user has not
    been granted the requested logon type at this computer.

Grant it in `secpol.msc` under **Local Policies → User Rights Assignment → Log
on as a batch job**. To script it, or on an edition without `secpol.msc`
(Windows Home), call `LsaAddAccountRights` with the account's SID;
`scripts/test-isolation-windows.ps1` does this for its test accounts and can
serve as a reference.

**Provisioning credentials.** Each run-as-user account needs its password
stored on every worker that serves a queue configured for it. From an elevated
shell:

    sqi-worker isolation set-credential render-svc

The secret is read from stdin; an argument would be visible to every process
on the host. It is encrypted with the machine DPAPI key and written under
`<data_dir>\isolation\` with an ACL granting only SYSTEM and Administrators.

> **Machine-scope encryption means the file ACL is the real boundary.**
> Anything running on the host that can *read* the file can decrypt it. Do not
> relax the ACL, and treat a backup that copies it elsewhere on the same
> machine as a credential disclosure.

**Session directories** are secured with a protected NTFS DACL granting only
the target account, SYSTEM and Administrators. Inheritance is stripped, so a
session directory does not inherit a `BUILTIN\Users` grant from its parent.
Ownership is not transferred to the target account, because a Windows owner
implicitly holds `WRITE_DAC` and could open its session to other accounts.

**Session root** defaults to `%ProgramData%\sqi\worker\sessions` (see
[`worker.session_dir`](#workersession_dir)).

**Bypass traverse checking.** Windows grants `SeChangeNotifyPrivilege` to
`Everyone` by default, so sqi does not check ancestor directory permissions as
it does on POSIX. If your environment removes that privilege from the target
account, credential resolution fails with a message naming the policy; sqi
does not widen the directories above a session root.

**The `s4u` provider is not implemented** and is refused. An S4U logon needs
no password, but its token carries no network credentials: any UNC path,
mapped drive, or authenticated license server fails as `ANONYMOUS` inside the
task.

**Process containment.** Windows task processes run inside a job object, so a
grandchild orphaned before a cancel or timeout is still reaped. This applies
to every Windows worker, isolated or not: a running task is terminated when
the worker process exits, including on a graceful service restart.

> **An isolated task controls its session directory.** The ACL grants the
> target account full access, so the task can replace anything under its
> scratch subdirectory, including swapping a file it wrote for a symlink or a
> directory junction. Stage-out opens every source through an `os.Root`
> rooted at the scratch directory, so the kernel refuses a reparse point or
> an escape at the open, and the built-in copy reads that same descriptor.
> An operator-configured `sync_command` receives path strings instead; see
> [`staging.sync_command`](#stagingsync_command) for the risk that leaves.

### Privileged accounts and groups are refused outright

`isolation.Provider.Resolve` refuses these requests:

- A `run_as_user` naming a known-privileged account (`root`, `Administrator`,
  `SYSTEM`, ...) or resolving to uid 0, whatever its name.
- A `run_as_group` naming a known-privileged group (`root`, `wheel`, `admin`,
  `sudo`, `sudoers`, `adm`, `docker`, `disk`, `shadow`, `staff`,
  `administrators`). This checks the group requested on the queue; the target
  account's existing memberships are covered below.
- A `run_as_user` account whose primary group is gid 0, even with no
  `run_as_group` set. gid 0 is refused whatever the platform calls it (`root`
  on Linux, `wheel` on macOS/BSD).

### A target account's existing group memberships are not stripped

Isolation always strips gid 0 (`root`) from a target account's supplementary
groups. It keeps every other group the account belongs to, such as `docker`,
`disk`, `shadow` or an in-house group, because those memberships are often
how a render account reaches project storage. **Do not point a queue's
`run_as_user` at an account in `docker`** (a one-step escalation to root)
**or `disk`/`shadow`.** Check with `id <user>` before assigning an account to
an isolated queue.

### The NSS (directory-backed account) fallback, and its caveats

`sqi-worker` is built with `CGO_ENABLED=0`, so Go's `os/user` package reads
only `/etc/passwd` and `/etc/group` and never consults NSS. An account that
exists only in a directory service (LDAP/AD, `sssd`, `winbind`) would not
resolve. When the direct read fails, sqi runs `id`/`getent` (from `/usr/bin`
or `/bin`, falling back to a `PATH` search with a warning) and parses their
output.

Two caveats:

- **The `id`/`getent` parsers are tested only against hand-written output,**
  never against a real directory server. Confirm that a directory-backed
  `run_as_user` account resolves correctly in your environment before relying
  on it.
- **A sandbox that blocks `exec` breaks this fallback.** If a systemd unit (or
  similar) restricts syscalls or forbids running external binaries,
  `id`/`getent` cannot run and directory-backed accounts fail to resolve.

```yaml
isolation:
  required: false
  env_passthrough:
    - "foundry_LICENSE"
    - "ARNOLD_LICENSE"
    - "solidangle_LICENSE"
```

---

## `expr` — EXPR expression limits

Five keys bound what one assignment may spend evaluating OpenJD `EXPR`
expressions on this host at task-execution time, after the server has
accepted the job. They are separate from the server's submission-time limits
([`openjd.expr_*`](configuration.md#expr-expression-limits--read-this-before-changing-any-of-them)),
and each worker reads its own, so hosts with different memory can be sized
differently. See [`docs/openjd-extensions/expr.md`](openjd-extensions/expr.md)
for the extension itself.

These limits are always enforced. `0` does not mean unlimited: it is out of
range, and an out-of-range value fails startup rather than being clamped.

### Four caveats before you change any of them

**1. This worker must not be tighter than the server.** Each key is compared
with a server limit that meters the same values at submission:

| This worker's key | must be ≥ the server's |
|---|---|
| `expr.operation_limit` | `openjd.expr_operation_limit` |
| `expr.memory_limit` | `openjd.expr_memory_limit` |
| `expr.assignment_positions` | `openjd.expr_template_positions` |
| `expr.assignment_retained_bytes` | `openjd.expr_template_retained_bytes` |
| `expr.let_retained_bytes` | `openjd.expr_template_retained_bytes` |

The last row compares a per-table limit with a template-wide one,
because the server has no per-table limit. A template's total retained bytes
bounds any one of its tables, so the comparison errs toward withholding work;
see [`expr.let_retained_bytes`](#exprlet_retained_bytes).

The worker advertises all five when it registers, and the server does not
dispatch EXPR jobs to it while any of them is lower than the server's
value. The worker keeps running all other work. The server logs a `WARN` when
the worker registers, in its own log (Admin → Server log, component
`server`), not in this worker's diagnostics. While the server's unschedulable
sweep is on (`scheduler.unschedulable_grace > 0`, the default), it also marks
any task that no worker can run; with the sweep off, such a task waits in
`ready` and the registration warning is the only signal.

Passing the comparison does not guarantee a job will fit. At execution time
expressions see concrete values where the server saw placeholders, so they
can cost more here than at submission, and an accepted job can still exceed
this worker's limits. That is why the operation and memory defaults are well
above the server's.

**Raise worker limits before server limits.** Two defaults equal the server's:
`expr.assignment_positions` (10,000) matches `openjd.expr_template_positions`,
and `expr.let_retained_bytes` (10,000,000) matches
`openjd.expr_template_retained_bytes`. Raising either server key at all
withholds EXPR work from every worker still on the defaults. The other three
defaults have headroom: 100x the server's operation limit, 20x its memory
limit, and 2x its template retained bytes for `expr.assignment_retained_bytes`.
Roll out the new worker value, confirm the registration `WARN` is gone, then
raise the server.

**2. `operation_limit` and `assignment_positions` multiply.** Their product is
the upper bound on operations for one assignment: 10¹⁰ at the defaults
(1,000,000 x 10,000) and 10¹² at the maxima (10,000,000 x 100,000). Nothing
counts operations across an assignment; the product is derived, not
enforced. `memory_limit` does not multiply it: it raises how large a value
one evaluation may hold, not how long an operation takes.

**3. None of these limits bounds wall-clock time.** Specification section
1.3.10 rule 3 charges a string operation its length divided by 256, so
operations on the same string can be charged the same and differ several-fold
in CPU time. Raising a limit lengthens the slowest assignment this host can be
asked to resolve, roughly in proportion. The time is spent in the task slot
the assignment already occupies. See
[the server's caveat 2](configuration.md#2-none-of-these-limits-bounds-wall-clock-time)
for figures, which apply per position here too.

**4. The byte limits count cumulative allocation, not peak live memory.** A
session is charged the sum over every environment it enters, even if it never
holds more than a few MB at once. Sizing host RAM against
`assignment_retained_bytes` over-provisions, and setting that key from an
observed RSS under-sizes it. For host RAM, size against `50 x memory_limit`
(the most a single `let:` block can hold, 1 GB at the default) per task slot
resolving expressions at the same time.

### `expr.operation_limit`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `1000000` |
| **Range** | `10000` – `10000000` |
| **Env var** | `SQI_WORKER_EXPR_OPERATION_LIMIT` |

Operations (specification section 1.3.10) that one expression evaluation
on this host may spend. The floor equals the server's default
`openjd.expr_operation_limit`.

```yaml
expr:
  operation_limit: 1000000
```

### `expr.memory_limit`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `20000000` |
| **Range** | `1000000` – `200000000` |
| **Env var** | `SQI_WORKER_EXPR_MEMORY_LIMIT` |

Live bytes (specification section 1.3.9) that one expression evaluation on
this host may hold. The floor equals the server's default
`openjd.expr_memory_limit`. A `let:` block can hold up to 50x this value, so
size host RAM against that product (caveat 4).

```yaml
expr:
  memory_limit: 20000000
```

### `expr.assignment_positions`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `10000` |
| **Range** | `2000` – `100000` |
| **Env var** | `SQI_WORKER_EXPR_ASSIGNMENT_POSITIONS` |

Expression positions (a command, one args entry, one embedded file, one
variable value) one assignment may resolve, summed across the task and every
environment the session enters. Only environment entry is charged: teardown
is exempt, so an exhausted budget never skips an `onExit` action such as a
license check-in or an unmount.

**Must not be lower than the server's `openjd.expr_template_positions`;**
the server withholds EXPR work from a worker that is (caveat 1). The floor of
2,000 covers a large session of one task action plus 50 entered environments.

```yaml
expr:
  assignment_positions: 10000
```

### `expr.assignment_retained_bytes`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `20000000` |
| **Range** | `2000000` – `200000000` |
| **Env var** | `SQI_WORKER_EXPR_ASSIGNMENT_RETAINED_BYTES` |

Bytes that all `let:` blocks in one assignment may retain in total, summed
across the task's symbol table and every environment's. The default is 2x
`expr.let_retained_bytes`, enough for a task and one environment each near
the per-table limit. This counts cumulative allocation, not peak live memory
(caveat 4).

```yaml
expr:
  assignment_retained_bytes: 20000000
```

### `expr.let_retained_bytes`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `10000000` |
| **Range** | `1000000` – `100000000` |
| **Env var** | `SQI_WORKER_EXPR_LET_RETAINED_BYTES` |

Bytes one symbol table may hold live. The whole table counts, not only
its `let:` bindings, so large job parameters use up part of this budget. A
`let:` block that would push the table past this limit stops with an error at
that binding.

The server has no per-table limit, so this key is compared with
`openjd.expr_template_retained_bytes` (caveat 1's last row). That comparison
can withhold EXPR work from a worker whose per-table limit would never have
been reached.

> **Read this before lowering it.** The floor of `1000000` is a tenth of the
> server's default `openjd.expr_template_retained_bytes`. A worker set below
> the server's value for that key is offered no EXPR work at all until one
> of the two changes, and the server reports this with a registration `WARN`
> and an `unschedulable_reason` naming both keys.

Even at or above the server's value, this key can reject an accepted job: it
measures the whole table, and at execution time bindings hold concrete values
the server saw only as placeholders (`"x" * Task.Param.N` costs nothing at
submission).

It may be set higher than `expr.assignment_retained_bytes`; the lower of the
two then applies.

```yaml
expr:
  let_retained_bytes: 10000000
```

---

## `capabilities` — Software auto-detection

Configures software detection. Nineteen built-in detectors (Maya, Nuke,
Houdini, Blender, Cinema 4D, 3ds Max, After Effects, Mistika Boutique/Ultima,
Mistika VR, Mistika Workflows, ffmpeg, the standalone renderers Arnold kick,
V-Ray, husk, Redshift, RenderMan, 3Delight and Mantra, and oiiotool) run at
startup with no configuration and advertise a `key=true` tag for each tool
found. You can add custom detectors for in-house tools. Full reference,
including the detector schema and the tag/version model:
[`docs/worker-capabilities.md`](worker-capabilities.md#capability-auto-detection-built-in-dcc-detectors).

### `capabilities.detect`

| | |
|---|---|
| **Type** | `[]Detector` |
| **Default** | `[]` (empty — only the built-ins run) |
| **Env var** | — (config file only) |

Custom detectors, in the same schema as the built-ins. They have no
environment-variable form; on cloud fleets, put them in the config file baked
into the image. Each entry is validated at config load, and an invalid one
(missing `tag`, no `checks`, a check without exactly one of
`exe`/`path_glob`/`env`/`registry`, a bad `os` value, or a regex that does not
compile) fails startup with an error naming it.

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

Built-in detectors to turn off, by exact tag: `maya`, `nuke`, `houdini`,
`blender`, `cinema4d`, `3dsmax`, `aftereffects`, `mistika`, `mistikavr`,
`mistikaworkflows`, `ffmpeg`, `arnold`, `vray`, `husk`, `redshift`,
`renderman`, `3delight`, `mantra` or `oiiotool`. Use this when a built-in
misfires on a nonstandard host layout, typically with a `capabilities.detect`
entry for the same tag that has more specific checks.

```yaml
capabilities:
  disable: [blender]
```

```sh
SQI_WORKER_CAPABILITIES_DISABLE=blender,nuke sqi-worker start
```

Run `sqi-worker capabilities` to print every tag the worker would advertise,
with its source (`auto`, `builtin:<tag>`, `custom`, or `manual`), to check
whether a detector fires.

---

## `staging` — Local path staging (`stage_locally` delivery)

Used by jobs that run a `stage_locally` delivery of the `SQI_PATH_TRANSLATION`
extension. `scratch_dir` and `sync_command` can be set only in the config file;
on cloud fleets, bake them into the config shipped in the image. `defaults`
(env var `SQI_STAGING_DEFAULTS`) controls whether a worker without them still
runs `stage_locally` jobs.

### `staging.scratch_dir`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` → `<os.TempDir()>/sqi-staging` whenever unset |
| **Env var** | — (config file only) |

Base directory for per-attempt staged copies. When unset, staging uses
`sqi-staging` under the platform temp directory (e.g. `/tmp/sqi-staging` on
Linux, `%TEMP%\sqi-staging` on Windows), whatever `staging.defaults` says.
That suits development; use a dedicated scratch volume in production.

### `staging.sync_command`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` → built-in copy when unset and `staging.defaults` is true |
| **Env var** | — (config file only) |

Command template run once per path, with `{src}`, `{dest}`, and optional
`{object_type}` placeholders (e.g. `rsync -a {src} {dest}`). The same template
serves copy-in and copy-out. The template is split on whitespace and run
directly, not through a shell: quoting, pipes and redirection are not
interpreted, and each placeholder is substituted within its own argument.

Leave it unset, or set it to `builtin`, to use sqi's built-in cross-platform
copy instead of an external command. The built-in copy handles a single file
or a directory tree and preserves file mode, but not ownership or extended
attributes.

`sync_command: builtin` selects the built-in copy even when
`staging.defaults` is `false`, and works with `scratch_dir` unset (the temp
directory fallback above applies). Because it is an explicit choice, it does
not log the one-time warning described under `staging.defaults`.

> **The built-in copy only moves bytes the worker can already reach** (local
> disk or a filesystem mounted on that worker). It does not transfer data
> between hosts. A farm whose workers span multiple compute locations (see
> [`docs/compute-locations.md`](compute-locations.md)) needs a `sync_command`
> such as `rsync` or `aws s3 cp` to move data between them.

> **sqi refuses to stage a file that has more than one hardlink.** On
> stage-in the built-in copy refuses such an input; on stage-out the check
> runs before any copy, built-in or `sync_command`. The task fails with one
> of two messages, both containing `has more than one hardlink`:
>
> - stage-in: `copy refused: opened "<path>" has more than one hardlink; sqi
>   will not copy a file that may alias content beyond scratch`
> - stage-out: `stage-out refused: "<path>" has more than one hardlink; sqi
>   will not copy a file that may alias content beyond scratch`
>
> The stage-out message gives the path relative to the scratch directory. A
> hardlink shares its inode with the original file, so chowning the staged
> copy to the run-as-user account would chown the original too. Inputs
> delivered by a content-addressed or deduplicating asset store, or by
> `cp -al` / `rsync --link-dest`, are multiply linked and will be refused.
> Deliver them with a real copy (`rsync -a` without `--link-dest`, `cp`
> without `-l`) or configure a `sync_command` that makes one.
>
> This concerns input files that already have extra links. The next note
> concerns a `sync_command` that creates them.
>
> Releases up to v0.3.0 did not enforce this on Windows, so after upgrading a
> mixed farm, a job with multiply linked inputs can start failing on its
> Windows workers only.

> **`sync_command` MUST NOT create hardlinks into scratch** when run-as-user
> isolation is in use (e.g. `cp -al` or `rsync --link-dest`). Chowning the
> scratch copy to the run-as-user account would chown the original file too.
> `rsync -a` without `--link-dest` and the built-in copy are both safe: they
> always write a new inode.

> **`sync_command` MUST NOT dereference symlinks at either end**, on stage-in
> or stage-out. Before running the command on stage-out, sqi opens the source
> through an `os.Root` rooted at the scratch directory, so the kernel enforces
> containment during the open (`OBJ_DONT_REPARSE` on Windows, `openat` with
> per-component `O_NOFOLLOW` on POSIX) and refuses a source reached through a
> symlink or a directory junction. It then checks on the open descriptor that
> the source is a regular file with a single hardlink. sqi cannot control what
> the command does after that: `rsync -a` preserves a symlink, while
> `rsync -aL` and plain `cp` follow it. A command that follows a symlink a task
> planted at its output path gives the `sync_command` process, which runs as
> the worker daemon (root under POSIX isolation), whatever the link points to
> on stage-out, and writes through a symlink planted at the destination on
> stage-in.
>
> **A configured `sync_command` also leaves a race open.** sqi passes it a
> path string, not an open descriptor, so a task process still running after
> its task exits successfully (only timeout and cancel kill it) can swap a
> hardlink into its scratch subdirectory between sqi's check and the
> command's own open. The built-in copy (`sync_command` unset or `builtin`)
> reads the descriptor it validated, so it has no such gap. When run-as-user
> isolation is in use, treat this as a permanent property of any external
> `sync_command`.

### `staging.defaults`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `true` |
| **Env var** | `SQI_STAGING_DEFAULTS` |

When `true` (the default), a worker without `scratch_dir` and `sync_command`
still runs `stage_locally` jobs, using the temp scratch directory and the
built-in copy described above. It logs a one-time `WARN` the first time it
uses the built-in copy with no `scratch_dir` set.

With `false`, a worker runs `stage_locally` jobs only when both `scratch_dir`
and `sync_command` are set, or `sync_command` is `builtin`; otherwise each
such job fails before execution.

```yaml
staging:
  scratch_dir: /scratch/sqi
  sync_command: "rsync -a {src} {dest}"
  defaults: true
```

---

## `diagnostics` — Diagnostic-log publishing

### `diagnostics.enabled`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `true` |
| **Env var** | `SQI_DIAGNOSTICS_ENABLED` (note: no `WORKER` infix) |

When enabled (the default), the worker publishes its own `slog` output on the
core-NATS subject `worker.diag.<workerID>`, which the server keeps in its
in-memory diagnostics buffer and shows in the web UI. Set to `false` to stop
publishing; the worker still logs locally, to stderr or to `log.file`. The
server side is controlled by its `diagnostics.buffer_size`.

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

Log output format. `json` suits production, where log aggregators (Loki,
Datadog, Splunk, etc.) can index its fields. `text` writes human-readable
`key=value` pairs, which suits local development.

```yaml
log:
  format: "json"
```

---

### `log.file`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` (log to stderr) |
| **Env var** | `SQI_WORKER_LOG_FILE` |
| **CLI flag** | — |

Path of a file to write log output to instead of stderr, rotated by size (see
`log.max_size_mb` and `log.max_backups`). Relative paths resolve against the
working directory; a Windows service runs in the directory that holds its
config file. Empty logs to stderr.

The directory must already exist: sqi-worker does not create it, and startup
fails with a `log.file` validation error naming the missing directory. A path
that ends with a separator or names an existing directory also fails
validation.

When `sqi-worker` runs as a Windows service and `log.file` is empty, logs go to
`%ProgramData%\sqi\logs\<service-name>.log` instead, since a service has no
stderr to write to. That default directory is created for you.

```yaml
log:
  file: "D:\\sqi-logs\\sqi-worker.log"
```

---

### `log.max_size_mb`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `100` |
| **Env var** | `SQI_WORKER_LOG_MAX_SIZE_MB` |
| **CLI flag** | — |

Size in megabytes at which `log.file` is rotated. Must be `> 0`. Has no effect
while `log.file` is empty and the worker is not running as a Windows service.

```yaml
log:
  max_size_mb: 100
```

---

### `log.max_backups`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `5` |
| **Env var** | `SQI_WORKER_LOG_MAX_BACKUPS` |
| **CLI flag** | — |

How many rotated files (`<file>.1` through `<file>.N`) are kept. `0` keeps
none: the file is discarded and started fresh when it reaches
`log.max_size_mb`. Must be `>= 0`.

```yaml
log:
  max_backups: 5
```

---

## `metrics` — Local HTTP server

The worker runs a small HTTP server for health probes and Prometheus metrics.
By default it listens on loopback only.

| Path | Purpose |
|---|---|
| `/healthz` | Liveness probe; returns 200 while the process is running |
| `/readyz` | Readiness probe; returns 503 while the NATS connection is down |
| `/metrics` | Prometheus metrics endpoint |
| `/debug/pprof/` | Go profiling endpoints (only when `metrics.enable_pprof: true`) |

### `metrics.addr`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"127.0.0.1:9091"` |
| **Env var** | `SQI_WORKER_METRICS_ADDR` |

TCP address the HTTP server listens on. Use `0.0.0.0:9091` to expose the
endpoints to Prometheus scrapers on the network, and firewall the port.

The listener is **plaintext unless [`metrics.tls.enabled`](#metricstlsenabled)
is set**; the `nats.tls_*` settings cover only the broker connection. It
serves metrics, health and (optionally) pprof, never credentials or job
payloads. Turn TLS on with any address reachable from other hosts.

When [running multiple workers on one host](#running-multiple-workers-on-one-host),
give each instance a distinct port; a second worker on the same port fails to
bind.

```yaml
metrics:
  addr: "127.0.0.1:9091"
```

---

### `metrics.tls.enabled`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `false` |
| **Env var** | `SQI_WORKER_METRICS_TLS_ENABLED` |

Serve HTTPS on `metrics.addr`. As with the server's listeners, the same port
switches to TLS and no plaintext port remains.

Requires `metrics.tls.cert_file` and `metrics.tls.key_file`
(`SQI_WORKER_METRICS_TLS_CERT_FILE` / `SQI_WORKER_METRICS_TLS_KEY_FILE`); the
worker refuses to start without both, or with a pair it cannot load. Use a
certificate issued for the worker's own host, from
`sqi-server tls issue --host <worker-host>` or from a CA your monitoring stack
already trusts.

```yaml
metrics:
  addr: "0.0.0.0:9091"
  tls:
    enabled: true
    cert_file: "/etc/sqi/certs/worker.crt"
    key_file: "/etc/sqi/certs/worker.key"
```

---

### `metrics.enable_pprof`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `false` |
| **Env var** | `SQI_WORKER_METRICS_ENABLE_PPROF` |

Expose Go runtime profiling endpoints at `/debug/pprof/`. Profiles reveal
memory contents, goroutine stacks and CPU hotspots, so enable this only
temporarily, for performance diagnosis.

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

Enable mDNS discovery of `sqi-server`. When `true` and `nats.url` is empty,
the worker browses for `_sqi._tcp` services on the local network and uses the
first server found. Disable it on networks without multicast, which includes
most cloud VPCs and container networks.

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

Maximum time to wait for an mDNS discovery result before startup fails.
Increase it if the server is slow to respond on a congested network. Used only
when `discovery.enable_mdns` is `true` and `nats.url` is empty.

```yaml
discovery:
  mdns_timeout: "5s"
```

---

## `log_streamer` — Log chunk publisher

Controls how task process stdout/stderr is batched and streamed to
`sqi-server` over NATS JetStream. These values trade NATS message overhead
against how quickly output appears in the web UI log viewer. In each key, `0`
selects the default and a negative value fails validation.

### `log_streamer.max_lines_per_chunk`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `50` |
| **Env var** | `SQI_WORKER_LOG_STREAMER_MAX_LINES_PER_CHUNK` |

Maximum number of output lines batched into one NATS message; the buffer is
flushed as soon as it reaches this count. Increase it for very verbose
processes to reduce per-message overhead, or decrease it for finer-grained
live output.

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

Byte threshold for the line content buffered for one NATS message. The buffer
is flushed when, after a line is added, its total reaches this value, so a
message can exceed it by up to one line. Lines are not split or truncated.

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

Maximum time a line may wait in the buffer before it is flushed, whatever the
size thresholds. Smaller values show output sooner, at the cost of more small
NATS publishes from processes that print slowly.

```yaml
log_streamer:
  flush_interval: "500ms"
```

---

## Quick reference table

| Key | Type | Default | Env var | CLI flag |
|---|---|---|---|---|
| `nats.url` | string | `""` | `SQI_WORKER_NATS_URL` | — |
| `nats.tls_enabled` | string | `auto` | `SQI_WORKER_NATS_TLS_ENABLED` | — |
| `nats.tls_cert_file` | string | `""` | `SQI_WORKER_NATS_TLS_CERT_FILE` | — |
| `nats.tls_key_file` | string | `""` | `SQI_WORKER_NATS_TLS_KEY_FILE` | — |
| `nats.tls_ca_file` | string | `""` | `SQI_WORKER_NATS_TLS_CA_FILE` | — |
| `nats.insecure_skip_verify` | bool | `false` | `SQI_WORKER_NATS_INSECURE_SKIP_VERIFY` | `--nats-insecure-skip-verify` |
| `nats.max_reconnect_attempts` | int | `-1` | `SQI_WORKER_NATS_MAX_RECONNECT_ATTEMPTS` | — |
| `nats.reconnect_wait` | duration | `2s` | `SQI_WORKER_NATS_RECONNECT_WAIT` | — |
| `nats.credential_file` | string | `""` (`<worker.data_dir>/worker.nk`) | `SQI_WORKER_NATS_CREDENTIAL_FILE` | — |
| `nats.join_token` | string | `""` | `SQI_WORKER_NATS_JOIN_TOKEN` | — |
| `nats.join_token_file` | string | `""` | `SQI_WORKER_NATS_JOIN_TOKEN_FILE` | — |
| `nats.server_url` | string | `""` | `SQI_WORKER_NATS_SERVER_URL` | — |
| `nats.server_tls_ca_file` | string | `""` | `SQI_WORKER_NATS_SERVER_TLS_CA_FILE` | — |
| `nats.server_tls_insecure_skip_verify` | bool | `false` | `SQI_WORKER_NATS_SERVER_TLS_INSECURE_SKIP_VERIFY` | — |
| `worker.name` | string | hostname | `SQI_WORKER_NAME` | — |
| `worker.farm_id` | string | `""` | `SQI_WORKER_FARM_ID` | — |
| `worker.data_dir` | string | `~/.sqi/worker` (Linux/macOS); `%USERPROFILE%\.sqi\worker` (Windows); `/var/lib/sqi-worker` with no home directory | `SQI_WORKER_DATA_DIR` | — |
| `worker.session_dir` | string | `""` → resolved at startup | `SQI_WORKER_SESSION_DIR` | — |
| `worker.compute_location` | string | `""` | `SQI_WORKER_COMPUTE_LOCATION` | — |
| `worker.capability_tags` | []string | `[]` | `SQI_WORKER_CAPABILITY_TAGS` | — |
| `worker.commands` | map | `{}` | — (config file only) | — |
| `worker.heartbeat_interval` | duration | `15s` | `SQI_WORKER_HEARTBEAT_INTERVAL` | — |
| `worker.shutdown_grace_period` | duration | `30s` | `SQI_WORKER_SHUTDOWN_GRACE_PERIOD` | — |
| `worker.allow_root` | bool | `false` | `SQI_WORKER_ALLOW_ROOT` | — |
| `worker.keep_failed_sessions` | bool | `false` | `SQI_WORKER_KEEP_FAILED_SESSIONS` | — |
| `worker.queue_ids` | []string | `[]` | `SQI_WORKER_QUEUE_IDS` | — |
| `worker.pull_idle_backoff` | duration | `2s` | `SQI_WORKER_PULL_IDLE_BACKOFF` | — (deprecated, no effect) |
| `worker.pull_nack_delay` | duration | `5s` | `SQI_WORKER_PULL_NACK_DELAY` | — (deprecated, no effect) |
| `isolation.required` | bool | `false` | `SQI_WORKER_ISOLATION_REQUIRED` | — |
| `isolation.provider` | string | `logon_user` | `SQI_WORKER_ISOLATION_PROVIDER` | — |
| `isolation.env_passthrough` | []string | `[]` | `SQI_WORKER_ISOLATION_ENV_PASSTHROUGH` | — |
| `expr.operation_limit` | int | `1000000` | `SQI_WORKER_EXPR_OPERATION_LIMIT` | — |
| `expr.memory_limit` | int | `20000000` | `SQI_WORKER_EXPR_MEMORY_LIMIT` | — |
| `expr.assignment_positions` | int | `10000` | `SQI_WORKER_EXPR_ASSIGNMENT_POSITIONS` | — |
| `expr.assignment_retained_bytes` | int | `20000000` | `SQI_WORKER_EXPR_ASSIGNMENT_RETAINED_BYTES` | — |
| `expr.let_retained_bytes` | int | `10000000` | `SQI_WORKER_EXPR_LET_RETAINED_BYTES` | — |
| `capabilities.detect` | `[]Detector` | `[]` | — (config file only) | — |
| `capabilities.disable` | `[]string` | `[]` | `SQI_WORKER_CAPABILITIES_DISABLE` | — |
| `staging.scratch_dir` | string | `""` | — (config file only) | — |
| `staging.sync_command` | string | `""` | — (config file only) | — |
| `staging.defaults` | bool | `true` | `SQI_STAGING_DEFAULTS` | — |
| `diagnostics.enabled` | bool | `true` | `SQI_DIAGNOSTICS_ENABLED` | — |
| `log.level` | string | `info` | `SQI_WORKER_LOG_LEVEL` | `--log-level` |
| `log.format` | string | `json` | `SQI_WORKER_LOG_FORMAT` | `--log-format` |
| `log.file` | string | `""` | `SQI_WORKER_LOG_FILE` | — |
| `log.max_size_mb` | int | `100` | `SQI_WORKER_LOG_MAX_SIZE_MB` | — |
| `log.max_backups` | int | `5` | `SQI_WORKER_LOG_MAX_BACKUPS` | — |
| `metrics.addr` | string | `127.0.0.1:9091` | `SQI_WORKER_METRICS_ADDR` | — |
| `metrics.tls.enabled` | bool | `false` | `SQI_WORKER_METRICS_TLS_ENABLED` | — |
| `metrics.tls.cert_file` | string | `""` | `SQI_WORKER_METRICS_TLS_CERT_FILE` | — |
| `metrics.tls.key_file` | string | `""` | `SQI_WORKER_METRICS_TLS_KEY_FILE` | — |
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

A single worker runs tasks in parallel: the server leases it as many tasks as
fit its CPU cores (see [CPU reservations in OpenJD
templates](openjd-submission.md#5-cpu-reservations)). Concurrency is set by the
server's CPU-core accounting (`amount.worker.vcpu`); the worker has no local
task-count cap.

Run separate worker processes when you want distinct worker identities, each
with its own heartbeats, registration, capability set or queue assignment, for
example to simulate a farm or test the scheduler. `sqi-worker` connects out to
the server's NATS and listens only on its metrics port, so several instances
can share a host as long as these settings differ:

| Setting | Env var | Why it must differ |
|---|---|---|
| [`worker.data_dir`](#workerdata_dir) | `SQI_WORKER_DATA_DIR` | Holds the persistent `worker.id` UUID and, when `nats.credential_file` is left at its default, the worker's nkey seed. Sharing it gives two instances one identity on the server and one credential. |
| [`metrics.addr`](#metricsaddr) | `SQI_WORKER_METRICS_ADDR` | The local health/metrics HTTP server; a second instance on the same port fails to bind. |
| [`worker.name`](#workername) | `SQI_WORKER_NAME` | Display only. It defaults to the hostname, so instances would otherwise share a label in the web UI. |

Other settings (NATS URL, discovery, capability tags) may be shared or differ.
With broker authentication on, each instance that enrolls needs its own join
token: tokens are single-use by default (`nats.auth.join_token_single_use`),
so a second instance redeeming the same token fails enrollment.

For local development, the `make run-workers` target sets all of this up; see
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
- [`docs/auth.md`](auth.md#task-isolation) — The task-isolation model: `isolation.manage`, `run_as_user` update semantics, and why isolation raises the daemon's privilege to lower the task's.
