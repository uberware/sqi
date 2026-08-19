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
4. **CLI flags** — highest priority. `--config`/`-c`, `--log-level` and
   `--log-format` are root flags available on every subcommand; `--dry-run` and
   `--nats-insecure-skip-verify` belong to `start` only.

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

Directory used to persist the worker ID file (`worker.id`), and on Windows the
DPAPI-encrypted run-as-user credential store (`<data_dir>\isolation\`). Created
automatically on first start, and never widened for run-as-user traversal —
it stays private (0700) for as long as the worker exists.

The worker ID file ensures the server can correlate this worker across
restarts. Do not delete `worker.id` unless you intend to re-register as a new
worker. For production, use an absolute path on a fast local SSD.

Each worker instance needs its own `data_dir`: two workers sharing one would
load the same `worker.id` and collide on the server. This is the key setting
when [running multiple workers on one host](#running-multiple-workers-on-one-host).

Session working directories have their own setting — see
[`worker.session_dir`](#workersession_dir) below. They are moved out from under
`data_dir` for any worker that could actually use run-as-user isolation (a root
POSIX worker, or any Windows worker); a non-root POSIX worker with
`session_dir` unset still keeps them at `<data_dir>/sessions`, the pre-split
location, because isolation cannot function there anyway.

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

- **Running as root (POSIX)** — `/var/lib/sqi-worker-sessions`, created traversable
  (`0711`) from birth. Deliberately a SIBLING of, never a descendant of,
  `data_dir`'s own HOME-unset fallback (`/var/lib/sqi-worker`): nesting the
  two would make `LoadOrCreateWorkerID`'s own `0700` `data_dir` an ancestor
  of the session root, and the boot-time traversal check below would then
  refuse to start over a directory sqi itself just created. Its ancestors
  (`/var`, `/var/lib`) are `0755` on every real Linux/macOS installation, so
  nothing needs to be created or widened specifically for this.
- **Windows (any account)** — `%ProgramData%\sqi\worker\sessions`, chosen
  regardless of privilege: a worker running as LocalSystem resolves its data
  directory under `System32\config\systemprofile`, which is the wrong place for
  render scratch. Directory modes are inert on Windows; a session directory's
  real protection is the protected NTFS DACL applied beneath this root (see
  [Windows](#windows) below).
- **Otherwise (non-root POSIX)** — `<data_dir>/sessions`, created at `0750`
  (the location and mode used before this split existed). Real run-as-user
  isolation cannot function without root regardless of directory permissions,
  so there is nothing to protect by moving it, or widening it, for a worker
  that can never use it anyway.

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

> **Upgrade note: a root worker's sessions relocate automatically, with no
> config change.** The root/non-root branch above is decided purely by the
> worker's effective uid at startup, not by whether any queue actually
> configures `run_as_user` — a worker that has always run as root (for
> whatever reason, isolation-related or not) and never set `session_dir`
> moves from the pre-split `<data_dir>/sessions` to
> `/var/lib/sqi-worker-sessions` the moment it is upgraded to a version that
> contains this split, with no config change and no explicit opt-in. This is
> intentional, not a regression to work around by setting `session_dir` back
> to the old path: keeping sessions under `data_dir` would eventually demand
> `chmod o+x` on `data_dir` itself for the first isolated assignment, widening
> the `0700` directory that holds the worker-id file — exactly what this
> split exists to prevent. Two concrete consequences to plan for before
> upgrading a root worker:
>
> - Any directories preserved under the old `<data_dir>/sessions` by
>   `worker.keep_failed_sessions` are orphaned — they still exist on disk for
>   post-mortem inspection, but the worker will never look there again after
>   the upgrade. Copy out anything you still need before upgrading, or check
>   both paths for a time.
> - The new default, `/var/lib/sqi-worker-sessions`, sits on the root
>   filesystem, not necessarily the volume `data_dir` points at. A
>   deployment that intentionally placed `data_dir` on a large/fast dedicated
>   volume (the `data_dir` doc above recommends "an absolute path on a fast
>   local SSD") gets session scratch on `/var` instead unless `session_dir` is
>   set explicitly to point back at that volume.

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
empty (the default), the worker issues a single lease request on the reserved
subject `work.lease._any` — an empty leaf would produce the invalid subject
`work.lease.` with no responders. The server selects tasks farm-wide for that
token and gates by worker eligibility, so a queue-unaffiliated worker is
matched to any queue's ready work. Set this on heterogeneous farms where some
workers specialise in a subset of queues.

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

## `isolation` — Run-as-user task execution

Runs job-supplied task and environment actions under a queue-configured OS
user (`run_as_user`/`run_as_group` on the queue — see
[`docs/configuration.md`](configuration.md#queue-identity-run_as_user--run_as_group-task-isolation)
and [`docs/auth.md`](auth.md#task-isolation)) instead of the worker daemon's
own account. Supported on POSIX (Linux/macOS) and on Windows via the
`logon_user` provider — see [Windows](#windows) below for the
platform-specific setup and requirements. With no queue configured for
isolation anywhere, a worker's behavior is byte-for-byte unchanged whether or
not this section is present.

> **Enabling this RAISES the worker daemon's own privilege.** The only
> mechanism sqi has for a POSIX process to become another OS user
> (`setuid`/`setgid`/`setgroups`) itself requires starting as root:
> `isolation.Provider.Capable()` fails unless the worker's effective uid is 0.
> If you run `sqi-worker` unprivileged today, turning isolation on is **not**
> a pure reduction in privilege — it is a trade. The **daemon** gains root so
> that individual **tasks** can lose it. Do not enable `isolation.required` or
> point any queue's `run_as_user` at this worker expecting the daemon itself
> to end up with less access than it has now; it ends up with more.

### `isolation.required`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `false` |
| **Env var** | `SQI_WORKER_ISOLATION_REQUIRED` |

Exit at boot if this worker cannot assume another OS identity (POSIX: not
running as root). Separate from a per-assignment credential-resolution
failure, which fails only that one task: `required: true` says "this worker
is misconfigured" — a problem the worker can only detect at boot, since it
learns queue identities solely from each assignment, never from its own
config. Default `false`, so a worker that never expects an isolated
assignment keeps starting normally even if it happens to run as root.

`isolation.required: true` is a contradiction with `worker.allow_root: false`
(the default) on POSIX: the POSIX provider can only assume another identity
from root, but `worker.allow_root: false` refuses to let the worker start as
root at all. Config validation catches this combination explicitly at load
time with a message naming both keys, rather than letting it surface as a
confusing "cannot start as root" error that never mentions isolation. Set
`worker.allow_root: true` together with `isolation.required: true`, or leave
`isolation.required: false`. Windows is exempt from this check: its
credential mechanism does not require the worker process itself to run
privileged.

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

Selects the Windows credential mechanism. **Ignored on POSIX** — setting it
on a Linux/macOS worker has no effect at all, since the POSIX provider has no
concept of a selectable mechanism. `logon_user` is the only supported value;
`s4u` is refused explicitly (see [Windows](#windows) below).

### `isolation.env_passthrough`

| | |
|---|---|
| **Type** | `[]string` |
| **Default** | `[]` (empty — only the minimal base is inherited) |
| **Env var** | `SQI_WORKER_ISOLATION_ENV_PASSTHROUGH` (comma-separated) |

Daemon environment variable **name** patterns (`filepath.Match` globs, e.g.
`FLEXLM_*`) that an isolated task's environment inherits from the worker
daemon's own process, in addition to a minimal base (`PATH`,
`HOME`/`USERPROFILE`, `TMPDIR`, and a few others rewritten to the target
user rather than left pointing at the daemon's). Each pattern is validated as
a glob at config-load time; an invalid one (e.g. an unbalanced `[`) fails
worker startup naming the exact field, `isolation.env_passthrough[N]`.

**This governs only what an isolated task inherits from the daemon's own
environment.** Anything the job itself supplies — an OpenJD
`Environment.variables` block, an `openjd_env` export, a task template
variable — always reaches the task unfiltered, regardless of this setting;
those are the job's own data, not daemon leakage, and filtering them would
silently break jobs whose variable names no operator allowlist could
anticipate.

A render farm's licensing model is the realistic reason this exists — most
license managers are configured via environment variable on the render node,
and that variable has to survive into the isolated task:

```yaml
isolation:
  env_passthrough:
    - "foundry_LICENSE"     # Nuke
    - "ARNOLD_LICENSE"      # Arnold (Maya/Houdini)
    - "solidangle_LICENSE"  # Arnold, older variable name
```

> **A broad glob defeats the allowlist entirely.** `env_passthrough: ["*"]`
> re-opens the exact daemon-environment leak this filter exists to close, and
> so does anything that reads narrower but matches broadly in practice, like
> `"*KEY*"` — the daemon's own credentials, tokens, and internal service
> addresses all match a wildcard just as easily as a license variable does.
> Write the actual variable name. A task failing on a missing license
> variable is the expected, correct symptom of an allowlist that is too
> tight — the fix is to add that one name, not to widen the pattern.

### Windows

Task isolation is implemented on Windows via the `logon_user` provider. The
full suite — `make test-isolation-windows`, which exercises real local
accounts, real NTFS ACLs, DPAPI credential storage, and a child process
actually launched under the target account's token — has now run and passes on
a real elevated host (tier 1 as elevated Administrator, tier 2 as SYSTEM via a
scheduled task). See [Known gaps](auth.md#known-gaps) in `docs/auth.md` for the
caveats that remain.

**The worker must run as a service under LocalSystem**, or as an account
granted `SeAssignPrimaryTokenPrivilege`. Windows requires that privilege to
start a process under another account's token, and an elevated Administrator
does **not** hold it by default — this is the single most common cause of
`isolation: worker cannot assume another OS identity` on Windows. `Capable()`
reports it at boot with the fix named.

**Each run-as-user account needs the "Log on as a batch job" right**
(`SeBatchLogonRight`). The provider logs the account on with
`LOGON32_LOGON_BATCH` — the correct logon type for a service doing work on a
user's behalf, since unlike an interactive logon it does not require "Allow log
on locally", and unlike an S4U logon it does carry network credentials. Default
workstation policy grants that right to Administrators, Backup Operators and
Performance Log Users only, so a purpose-made standard account does **not**
hold it until you say so, and credential resolution fails with:

    isolation: logon "render-svc": LogonUserW: Logon failure: the user has not
    been granted the requested logon type at this computer.

Grant it in `secpol.msc` under **Local Policies → User Rights Assignment → Log
on as a batch job**. To script it, or on an edition that ships no `secpol.msc`
(Windows Home), call `LsaAddAccountRights` with the account's SID —
`scripts/test-isolation-windows.ps1` does precisely that for its throwaway
accounts and is a working reference.

**Provisioning credentials.** Each run-as-user account needs its password
stored on every worker that serves a queue configured for it. From an elevated
shell:

    sqi-worker isolation set-credential render-svc

The secret is read from stdin — never pass it as an argument, where it is
visible to every process on the host. It is encrypted with the machine DPAPI
key and written under `<data_dir>\isolation\` with an ACL granting only SYSTEM
and Administrators.

> **Machine-scope encryption means the file ACL is the real boundary.**
> Anything running on the host that can *read* the file can decrypt it. Do not
> relax the ACL, and treat a backup that copies it elsewhere on the same
> machine as a credential disclosure.

**Session directories** are secured with a protected NTFS DACL granting the
target account, SYSTEM, and Administrators — and nothing else. Inheritance is
stripped, so a session directory does not carry whatever `BUILTIN\Users` grant
its parent had. Ownership is deliberately *not* transferred to the target
account: a Windows owner implicitly holds `WRITE_DAC` and could otherwise
re-open its own session to other accounts.

**Session root** defaults to `%ProgramData%\sqi\worker\sessions`, not the
worker data directory — as LocalSystem, the data directory resolves under
`System32\config\systemprofile`, which is the wrong place for render scratch.
Override with `worker.session_dir`.

**Bypass traverse checking.** Windows grants `SeChangeNotifyPrivilege` to
`Everyone` by default, which is why sqi does not check ancestor permissions
the way it does on POSIX. If your environment strips that privilege, isolation
is refused at credential-resolution time with a message naming the policy —
sqi will not widen the directories above a session root for you.

**The `s4u` provider is not implemented** and is refused explicitly. An S4U
logon needs no password, but its token carries no network credentials: any UNC
path, mapped drive, or authenticated licence server fails as `ANONYMOUS`
inside the task.

**Process containment.** Windows task processes run inside a job object, so a
grandchild orphaned before a cancel or timeout is still reaped. One
consequence applies to *every* Windows worker, isolated or not: a running task
is terminated when the worker process exits, including on a graceful service
restart. Previously such processes were orphaned while the task was reclaimed
and re-run elsewhere.

> **Open gap: session-directory TOCTOU on Windows.** See [Known
> gaps](auth.md#known-gaps) in `docs/auth.md` for the staging race this
> enables.

### Privileged accounts and groups are refused outright

Before any stripping or filtering happens, `isolation.Provider.Resolve`
refuses several requests outright:

- A `run_as_user` naming a known-privileged account (`root`, `Administrator`,
  `SYSTEM`, ...) or resolving to **uid 0**, regardless of name.
- An explicit `run_as_group` naming a known-privileged group (`root`,
  `wheel`, `admin`, `sudo`, `sudoers`, `adm`, `docker`, `disk`, `shadow`,
  `staff`, `administrators`) — a check on the group you asked FOR, distinct
  from the target account's ambient memberships covered below.
- A `run_as_user` account whose **primary** group is gid 0 — refused even
  with no `run_as_group` set at all, since gid 0 is refused unconditionally
  regardless of which name a platform gives it (`root` on Linux, `wheel` on
  macOS/BSD).

### A target account's existing group memberships are not stripped

Isolation strips gid 0 (`root`) from a target account's supplementary groups
unconditionally. It does **not** strip any other, named group the account
already belongs to — `docker`, `disk`, `shadow`, or an in-house group. This
is deliberate: those memberships are the account's own pre-existing access,
typically how a render-farm account reaches project storage, and stripping
them "to be safe" would silently break exactly that access. The operator's
responsibility, correspondingly: **do not point a queue's `run_as_user` at an
account that belongs to `docker`** (a well-known one-step escalation to root)
**or `disk`/`shadow`.** Check with `id <user>` before assigning an account to
an isolated queue, the same as you would before granting it any other kind of
OS-level access.

### The NSS (directory-backed account) fallback, and its caveats

`sqi-worker` ships with `CGO_ENABLED=0`, so Go's `os/user` package reads only
`/etc/passwd` and `/etc/group` directly and never consults NSS — an account
that only exists via a directory service (LDAP/AD, `sssd`, `winbind`) would
not resolve at all. To make `run_as_user` usable against such an account, sqi
falls back to shelling out to `id`/`getent` when the direct read fails, and
parses their output.

Two honest caveats, not resolved by testing that exists today:

- **The `id`/`getent` output parsers have only ever been exercised against
  canned, hand-written output.** They have never been run against a real
  directory server. Verify a directory-backed `run_as_user` account resolves
  correctly in your own environment before relying on it in production —
  there is no equivalent of `make test-ldap`/`make test-oidc` for this path.
- **A worker sandboxed by systemd (or similar) in a way that blocks `exec`
  breaks this fallback outright.** If your unit file restricts syscalls or
  denies executing external binaries, `id`/`getent` cannot run, and
  directory-backed accounts fail to resolve even though the account itself is
  valid.

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

Five keys bound what **one assignment** may spend evaluating OpenJD `EXPR`
expressions **on this host**, at task-execution time — after the server has
already accepted the job. They are separate from the server's own
submission-time limits
([`openjd.expr_*`](configuration.md#expr-expression-limits--read-this-before-changing-any-of-them)):
each worker reads its own, which is what lets a heterogeneous farm size them
to each host's real memory.

These are **always on**. `0` is not "unlimited" — it is out of range, and an
out-of-range value is a **startup failure, not a clamp**.

> **These keys are live.** Earlier revisions carried a "`EXPR` is not accepted
> yet" callout here: the extension's registry status was *in-progress*, so no
> template declaring `extensions: [EXPR]` could be submitted and no assignment
> ever reached these limits. **That is no longer true** — `EXPR` is a supported
> extension, EXPR jobs are submitted, dispatched and executed, and every one of
> these keys now meters real work on this host. See
> [`docs/openjd-extensions/expr.md`](openjd-extensions/expr.md).

### Four caveats before you change any of them

**1. Tightening is not free here the way it is on the server.** Every one of
these five is compared against a server-side limit metering the same values
one phase earlier, and this worker must not be **tighter** than the server it
reports to:

| This worker's key | must be ≥ the server's |
|---|---|
| `expr.operation_limit` | `openjd.expr_operation_limit` |
| `expr.memory_limit` | `openjd.expr_memory_limit` |
| `expr.assignment_positions` | `openjd.expr_template_positions` |
| `expr.assignment_retained_bytes` | `openjd.expr_template_retained_bytes` |
| `expr.let_retained_bytes` | `openjd.expr_template_retained_bytes` |

The last row pairs a **per-table** limit with a **template-wide** one, because
the server meters no per-table scope. A whole template's retained bytes is a
valid upper bound on any one of its tables, so the comparison is conservative
— see [`expr.let_retained_bytes`](#exprlet_retained_bytes), which is where the
consequence of this key's low floor is spelled out.

This worker advertises all five when it registers, and the server **refuses
to dispatch EXPR jobs to it while any of them is short** — it does not accept
the job and then fail it here, once per task, naming a budget the submitter
never saw. So the cost of tightening past the server is that this host stops
being offered EXPR work; it keeps running **everything else**. The server logs
the reason when this worker registers, and flags any task no capable worker
exists for — the latter only while the server's unschedulable sweep is on
(`scheduler.unschedulable_grace > 0`, the default). With that sweep off, such
a task simply waits `ready` with nothing written on it, and the one-off
registration log line is the only signal — and because the *server* emits it,
it lands in the server's own log (Admin → Server log, component `server`), not
in this worker's diagnostics.

The relation is **necessary, not sufficient.** Phase 3 evaluates concrete
values where the server had placeholders, so the same expression can
legitimately cost more here than it did at submit — which is exactly why the
shipped defaults are 100x the server's operation budget and 20x its memory
budget rather than equal to them. Matching the server is the floor, not a
guarantee: an accepted job can still exhaust a worker that passes every
comparison, and no configuration on either side makes that impossible.

**Two of the five defaults have zero headroom against the server's, so raise
the workers first.** `expr.assignment_positions` (10,000) is exactly the
server's `openjd.expr_template_positions` default, and `expr.let_retained_bytes`
(10,000,000) is exactly its `openjd.expr_template_retained_bytes` default.
Raising either of those two server keys by any amount therefore withholds EXPR
work from *every* worker still on the shipped defaults, immediately and
farm-wide. The other three ship with real headroom (100x the server's operation
budget, 20x its memory budget, 2x its template-retained-bytes budget for
`expr.assignment_retained_bytes`). Roll the worker value out first, confirm the
registration `WARN` is gone, then raise the server.

**2. `operation_limit` and `assignment_positions` multiply.** The cumulative
operation ceiling for one assignment is their product — 10¹⁰ at the defaults
(1,000,000 x 10,000), and 10¹² if both are raised to their maxima
(10,000,000 x 100,000), **100x**. Nothing counts operations cumulatively; the
product is a derived upper bound, not a measurement. `memory_limit` is not a
third multiplier on it: raising that lets one evaluation hold a larger value,
and measured on this branch it does **not** change what an operation costs in
time.

**3. None of these bounds wall-clock time.** Specification section 1.3.10
rule 3 prices a string operation at the value's length divided by 256, so
byte-heavy work is charged almost nothing:
`("x" * 900000).upper()` and `("x" * 900000).title()` are charged **the same
7,034 operations** and differ by 9x in CPU (~6 ms vs ~58 ms), and a
regex over the same string is charged **half** as much for nearly the slowest
time — a ~17x spread in cost per operation. Raising a limit lengthens the
worst assignment this host can be asked to resolve, roughly in proportion, and
no value makes a slow one impossible. Unlike the server's, this cost is
charged to the task slot the assignment already occupies; see
[the server's caveat 2](configuration.md#2-none-of-these-limits-bounds-wall-clock-time)
for the measured numbers, which apply per position here too.

**4. The byte dimensions count cumulative allocation, not peak live
retention.** A session that never holds more than a few MB live at once is
still charged the sum of every environment it enters. Sizing host RAM against
`assignment_retained_bytes` **over-provisions**; sizing that key against an
observed RSS **under-bounds** it. For real RAM, the number that matters is
`50 x memory_limit` — the structural ceiling of a single `let:` block, 1 GB at
the default — per concurrently-resolving task slot, not `memory_limit` itself.

### `expr.operation_limit`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `1000000` |
| **Range** | `10000` – `10000000` |
| **Env var** | `SQI_WORKER_EXPR_OPERATION_LIMIT` |

Operations (specification section 1.3.10) **one** host-side expression
evaluation may spend. The floor is the server's own *default* per-evaluation
operation budget: below that, this worker would reject at execution what the
server type-checked at submit.

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

Live bytes (specification section 1.3.9) **one** host-side expression
evaluation may hold. See caveat 4: the per-`let:`-block structural ceiling is
50x this number, so size RAM against that product, not against this value.

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

Expression **positions** (the command, one args entry, one embedded file, one
variable value) one assignment may resolve, summed across the task's own
symbol table and every environment the session enters. Charged at environment
**entry** only — teardown gets a fresh allowance per evaluation so a budget
that cannot avert the memory it charges for can never silently skip an
`onExit` (license check-ins, unmounts).

**Must not be lower than the server's `openjd.expr_template_positions`.** An
assignment's positions are a subset of its template's, so a lower value here
cannot run a job the server accepted; the server sees that from this worker's
registration and withholds EXPR work rather than failing it here. The floor of
2,000 sits just above a worked count of 1,841 positions for a generous session
(one task action plus 50 entered environments).

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

Bytes every `let:` block in one assignment may **cumulatively** retain, summed
across the task's symbol table and every environment's. The default is exactly
2x `expr.let_retained_bytes` — sized for a task plus one environment, each
near its own per-table ceiling.

Cumulative allocation, not peak live retention — see caveat 4.

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

Bytes **one** symbol table may hold live. Measured across the *whole* table,
not just its `let:` bindings — so a job whose own parameters are large spends
budget its `let:` block never asked for. A block that would push the table
past this stops there with an error rather than spending a fresh evaluation
budget per remaining binding.

It bounds **one table**, a scope the server's template-wide walk does not
meter separately — but it is advertised and compared all the same, against
`openjd.expr_template_retained_bytes` (caveat 1's last row). A whole
template's retained bytes is a valid upper bound on any one of its tables, so
the comparison is conservative in one direction only: it can withhold EXPR
work from a worker whose per-table limit was never going to be the binding
constraint. It is **not** sufficient in the other — see below.

> **Read this before lowering it.** The floor of `1000000` is a **tenth** of
> `openjd.expr_template_retained_bytes`' default. A worker set anywhere below
> the server's value for that key is offered **no EXPR work at all** until one
> of the two moves. That is the intended outcome, and it is the visible one —
> a registration `WARN` and an `unschedulable_reason` naming both keys. What
> it replaced is the invisible one: before this comparison existed, such a
> worker accepted the assignment and failed **every task of the job**, once
> per task, over a `let:` block the server had already accepted.

Even with the comparison satisfied, this key can still reject an accepted
job — the accounting measures the *whole table*, and phase 3 binds concrete
values the server only had placeholders for (`"x" * Task.Param.N` costs
nothing at submit). Sizing it at exactly the server's value is the floor, not
a guarantee.

It may legally be set higher than `expr.assignment_retained_bytes`; the
tighter of the two simply becomes the effective one.

```yaml
expr:
  let_retained_bytes: 10000000
```

---

## `capabilities` — Software auto-detection

Configures the eight built-in software detectors (Maya, Nuke, Houdini,
Blender, Mistika Boutique/Ultima, Mistika VR, Mistika Workflows, and ffmpeg)
that run automatically at startup and advertise a `key=true` tag with no
per-worker configuration, plus any custom detectors for in-house tools. Full
reference, including the detector schema and the tag/version model:
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
`blender`, `ffmpeg`, `mistika`, `mistikavr`, `mistikaworkflows`). Use this
when a built-in misfires on a nonstandard host layout — typically paired with
a `capabilities.detect` entry for the same tag that supplies more specific
checks.

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

> **`sync_command` MUST NOT dereference symlinks at either end**, on stage-in
> or stage-out. sqi validates the scratch-side path before invoking the
> command (regular file, single hardlink, contained in scratch), but that
> check cannot see what the command itself does once invoked: `rsync -a`
> preserves a symlink, `rsync -aL` or plain `cp` follow it. A command that
> follows a symlink a task planted at its declared output path hands the
> daemon's `sync_command` process — running as root — whatever that symlink
> points to, on stage-out, or writes through a symlink planted at the real
> destination path, on stage-in. This is entirely a property of the command
> template an operator chooses and is outside anything sqi can inspect or
> enforce.
>
> **Be aware this residue also includes a race, and it is structurally
> unclosable for `sync_command` specifically.** sqi hands `sync_command` a
> path string, not an open file descriptor, so there is nothing for sqi to
> pin between validating the scratch-side path and the command actually
> opening it — a task whose process group is still alive after its own
> successful exit (nothing kills it on success; only the timeout/cancel
> paths do) can still own a scratch subdirectory and swap a hardlink into it
> after sqi's check passes and before the external command reads the file.
> The built-in copy (`sync_command` unset or `builtin`) does not have this
> gap: it re-validates the file on the descriptor it actually opened, which
> pins the inode and cannot be swapped out from under it. That re-validation
> has no equivalent for an external command sqi merely invokes with strings —
> there is no descriptor to hand it and no way to make its own `open()` call
> observe what sqi already checked. Treat a configured `sync_command` as
> carrying this residual risk permanently when run-as-user isolation is in
> use, not as a gap that a smarter check could close.

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
- [`docs/auth.md`](auth.md#task-isolation) — the task-isolation model: `isolation.manage`, `run_as_user` update semantics, and why isolation trades daemon privilege for task privilege.
