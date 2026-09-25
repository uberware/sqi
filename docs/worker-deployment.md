# sqi-worker Deployment Guide

This document covers installing and running `sqi-worker` on bare metal (Linux,
macOS, Windows) and in Docker, including how to configure auto-start on boot in
each environment.

---

## Installation

### Pre-built binaries

Download the latest release archive from the GitHub Releases page. One archive
per platform contains **both** `sqi-server` and `sqi-worker`. Archives are named
`sqi_<OS>_<arch>` — for example `sqi_Linux_x86_64.tar.gz`,
`sqi_Linux_arm64.tar.gz`, `sqi_Darwin_arm64.tar.gz`,
`sqi_Windows_x86_64.zip`.

```sh
# Linux (x86_64)
curl -Lo sqi.tar.gz https://github.com/uberware/sqi/releases/latest/download/sqi_Linux_x86_64.tar.gz
tar -xzf sqi.tar.gz sqi-worker
chmod +x sqi-worker
sudo mv sqi-worker /usr/local/bin/
```

Verify the download against the published checksums:

```sh
curl -Lo checksums.txt https://github.com/uberware/sqi/releases/latest/download/checksums.txt
sha256sum --check --ignore-missing checksums.txt
```

> On macOS, extract the matching `sqi_Darwin_<arch>` archive. The binaries are
> signed with an Apple Developer ID and notarized, so Gatekeeper allows them to
> run without the quarantine workaround.

### Build from source

Requirements: Go 1.26.3 or later (the `go` directive in `go.mod` pins the version).

```sh
git clone https://github.com/uberware/sqi.git
cd sqi
make build
# Binary is at ./bin/sqi-worker
```

---

## Configuration

Before running the worker, create a configuration file. The recommended
location for a system-wide installation is `/etc/sqi/sqi-worker.yaml`.

Copy the annotated example and edit it:

```sh
sudo mkdir -p /etc/sqi
sudo cp config/sqi-worker.example.yaml /etc/sqi/sqi-worker.yaml
sudo $EDITOR /etc/sqi/sqi-worker.yaml
```

Minimum settings (if mDNS auto-discovery is not available):

```yaml
nats:
  url: "nats://sqi-server.example.com:4222"

discovery:
  enable_mdns: false

worker:
  data_dir: "/var/lib/sqi-worker"
```

A worker executes as many tasks concurrently as it has CPU cores; the server
gates capacity by CPU-core fit, so there is no per-worker concurrency knob to
set. On a multi-site farm you may also want to declare where this worker runs
so the server can resolve storage paths and honour location affinity:

```yaml
worker:
  compute_location: "onprem"      # matches a storage-location root name
  capability_tags: ["maya-2025", "arnold-7"]
  # queue_ids: ["gpu-renders"]    # restrict to specific queues (optional)
```

See [Compute locations](compute-locations.md) and
[Storage locations](storage-locations.md) for how these names are used.

Validate before deploying:

```sh
sqi-worker start --dry-run --config /etc/sqi/sqi-worker.yaml
```

See [`docs/worker-configuration.md`](worker-configuration.md) for every
available option.

---

## Broker authentication

Broker authentication (`nats.auth.enabled` on the server) is opt-in and off
by default; if the server you are joining does not have it enabled, skip
this section and connect as shown above with no credential. If it does, the
worker needs a credential before it can connect at all — see
[`docs/auth.md`](auth.md#broker-authentication-transport) for the full model
(what it protects, revocation, key rotation). This section walks the two
ways to obtain one.

**Both paths need `nats.server_url` set on the worker to the server's HTTP
base URL** (e.g. `http://sqi-server.example:8080`). Enrollment always runs
over REST, never over NATS — the broker's whole job is to refuse
unauthenticated connections, so it cannot also hand out the first
credential. `nats.server_url` is **not** derived from mDNS discovery even
when `discovery.enable_mdns` is on for locating the NATS broker itself; a
worker with a join token configured and no `server_url` set fails fast,
naming the field, rather than guessing a host.

### Path A — self-service enrollment with a join token

1. On the server host, mint a token:

   ```sh
   sqi-server worker token issue --ttl 1h
   ```

   The raw token prints to stdout exactly once — capture it
   (`TOKEN=$(sqi-server worker token issue)`) or store it securely; only its
   hash is kept in the database and it cannot be displayed again. If
   `auth.enabled` is also on, an operator can mint one over REST instead —
   `POST /api/v1/workers/join-tokens`, gated on the `workers.enroll`
   permission — but the CLI command above always works, whether or not
   `auth.enabled` is on.

2. Hand the token to the worker via a file (preferred — a token in the
   config file itself is a secret at rest) and set the server URL:

   ```yaml
   nats:
     join_token_file: "/run/secrets/sqi-join-token"
     server_url: "http://sqi-server.example:8080"
   ```

3. Start the worker normally. On first boot, finding no credential file at
   `nats.credential_file` (default `<worker.data_dir>/worker.nk`), it
   generates an nkey keypair locally and calls `POST /api/v1/workers/enroll`
   with the token, its worker ID and its new **public** key — the private
   seed never leaves the machine, and nothing from the server's response is
   persisted. Only after the server confirms enrollment does the worker
   write the seed it generated itself to `worker.nk` (mode `0600`) and
   connect with it. The token is single-use by default, so a second worker
   needs its own token.

Every failure past this point is fatal and explicit — an expired, unknown or
already-used token, or a worker ID already bound to a different key — the
worker logs the cause and exits rather than looping in the background with
no visible reason. An unreachable *server* is different: that still falls
back to the ordinary reconnect-with-backoff behavior once a credential
exists.

### Path B — manual pre-provisioning

For a worker that cannot reach the server's REST API (air-gapped hosts), or
a site that wants no self-service enrollment endpoint at all
(`nats.auth.enrollment_endpoint_enabled: false` on the server):

1. On the worker host, generate a keypair without connecting anywhere:

   ```sh
   sqi-worker keygen --data-dir /var/lib/sqi-worker
   ```

   `keygen` loads the worker's own configuration the same way `sqi-worker
   start` does (the root `-c/--config` file, `SQI_WORKER_*` environment
   variables, and built-in defaults), so `--data-dir` here is an explicit
   override of `worker.data_dir` — pass it when generating a keypair before
   the worker's own config is in place, or to target a different directory
   for a one-off run. With the worker's normal config already set up, a bare
   `sqi-worker keygen` resolves the same directory the worker itself uses.

   This writes `/var/lib/sqi-worker/worker.nk` (mode `0600`) and prints the
   worker's public key and the exact command to run next:

   ```
   Public key: UABC...XYZ
   Worker ID: 3f2a... (newly generated; if you expected an existing worker id here, --data-dir/worker.data_dir is probably pointed at the wrong directory)
   On the server, run:
     sqi-server worker enroll --worker-id 3f2a... --public-key UABC...XYZ
   A RUNNING sqi-server will not accept this credential until it restarts; to enroll against a running server, use POST /api/v1/workers/enroll with a join token instead.
   ```

2. Copy that command to the server host (out of band — SSH, a console, a
   provisioning script) and run it there:

   ```sh
   sqi-server worker enroll --worker-id 3f2a... --public-key UABC...XYZ
   ```

3. **Restart `sqi-server`.** `sqi-server worker enroll` writes the database
   from a separate process with no broker handle, and the broker builds its
   authorized-key set once at startup — so a *running* server does not know
   about the credential just written, and a worker presenting it is refused
   with an authorization error (which the worker treats as fatal, by
   design). If restarting the server is not acceptable, enroll over REST
   with a join token instead (Path A), which reloads the running broker in
   the same request.

4. Start the worker. It finds the seed already on disk, skips enrollment
   entirely, and connects directly with the credential.

No join token and no REST call are involved in this path at any point.

### Revoking a worker's credential

- **Immediately**, against a running server: `DELETE
  /api/v1/workers/{id}/credential`. This disconnects the worker inside the
  call — its in-flight leases reclaim through the normal heartbeat-sweep
  path.
- **Offline**, without a running server (or as part of a maintenance
  script): `sqi-server worker revoke <worker-id>`. This writes the database
  directly and takes effect the next time `sqi-server` starts, not before.

### Rotating a compromised or lost key

1. Revoke the current credential — `sqi-server worker revoke <worker-id>`,
   or the REST path above for an immediate disconnect.
2. `sqi-worker keygen --force` on the worker host, with the worker's normal
   config in place (pass `--data-dir` to override `worker.data_dir`
   explicitly for a one-off run against a different directory). This
   overwrites `worker.nk` with a new keypair and prints the new public key,
   whether the worker ID is the existing one or newly generated — a freshly
   generated one here is the signal that the resolved data directory does
   not match the worker being rotated — and the `sqi-server worker enroll`
   command for step 3. It must run *after* step 1, since the worker ID stays
   bound to the old public key on the server until that credential is
   revoked.
3. `sqi-server worker enroll --worker-id <worker-id> --public-key <new-key>`
   on the server host, using the key `keygen` printed.
4. **Restart `sqi-server`**, for the reason given in Path B step 3: this
   offline command cannot reload a running broker's authorized-key set, so
   until the server restarts it still refuses the new key. To rotate
   without a server restart, remove `worker.nk` and re-enroll over REST
   with a fresh join token instead (see the note below).
5. Restart the worker. It finds the new seed `keygen` already wrote and
   connects directly — no join token or REST enrollment call is needed on
   this path.

The worker ID can be reused once its old credential is revoked; the old
public key itself can never be enrolled again, on this worker ID or any
other. To rotate via a fresh join token instead of `keygen`, remove
`worker.nk` before restarting so the worker re-enrolls as it did on first
boot (Path A above).

---

## Linux — systemd

### 1. Create a dedicated user

```sh
sudo useradd --system --no-create-home --shell /bin/false sqiworker
sudo mkdir -p /var/lib/sqi-worker
sudo chown sqiworker:sqiworker /var/lib/sqi-worker
```

### 2. Install the systemd unit file

Ready-to-use unit files ship in the repository at
[`deploy/systemd/`](https://github.com/uberware/sqi/tree/main/deploy/systemd)
(`sqi-worker.service` and the `sqi-worker@.service` template). Copy one into
place, or create `/etc/systemd/system/sqi-worker.service` from the following:

```ini
[Unit]
Description=sqi distributed task worker
Documentation=https://github.com/uberware/sqi
After=network-online.target
Wants=network-online.target
# Optional: wait for sqi-server if it is on the same host
# After=sqi-server.service

[Service]
Type=simple
User=sqiworker
Group=sqiworker
ExecStart=/usr/local/bin/sqi-worker start --config /etc/sqi/sqi-worker.yaml
Restart=on-failure
RestartSec=5s
StandardOutput=journal
StandardError=journal
SyslogIdentifier=sqi-worker

# Worker ID and session working directories
StateDirectory=sqi-worker
StateDirectoryMode=0750

# Security hardening
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/sqi-worker

[Install]
WantedBy=multi-user.target
```

> **Note:** If `ProtectSystem=strict` prevents the worker from executing render
> tools, replace it with `ProtectSystem=full` or remove it. Render workers often
> need read access to shared network mounts.

### 3. Enable and start

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now sqi-worker
```

Check status:

```sh
sudo systemctl status sqi-worker
sudo journalctl -u sqi-worker -f
```

### 4. Upgrade

Replace the binary and restart:

```sh
sudo systemctl stop sqi-worker
sudo cp new-sqi-worker /usr/local/bin/sqi-worker
sudo systemctl start sqi-worker
```

---

## macOS — launchd

### 1. Create a configuration file

```sh
mkdir -p ~/.sqi
cp config/sqi-worker.example.yaml ~/.sqi/sqi-worker.yaml
$EDITOR ~/.sqi/sqi-worker.yaml
```

Set `worker.data_dir` to `~/Library/Application Support/sqi-worker` (or
another persistent path).

### 2. Install the launchd plist

Create `~/Library/LaunchAgents/net.uberware.sqi-worker.plist` for a per-user
agent (runs when the user is logged in):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>net.uberware.sqi-worker</string>

  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/sqi-worker</string>
    <string>start</string>
    <string>--config</string>
    <string>/Users/YOURUSERNAME/.sqi/sqi-worker.yaml</string>
  </array>

  <key>RunAtLoad</key>
  <true/>

  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>

  <key>StandardOutPath</key>
  <string>/tmp/sqi-worker.log</string>

  <key>StandardErrorPath</key>
  <string>/tmp/sqi-worker.log</string>

  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/usr/local/bin:/usr/bin:/bin</string>
  </dict>
</dict>
</plist>
```

Replace `YOURUSERNAME` with your actual macOS username.

For a system-wide daemon (runs without a user logged in), place the plist at
`/Library/LaunchDaemons/net.uberware.sqi-worker.plist` and run it as a
dedicated service account. Update `UserName` and paths accordingly.

### 3. Load and start

```sh
launchctl load ~/Library/LaunchAgents/net.uberware.sqi-worker.plist
launchctl start net.uberware.sqi-worker
```

Check whether it is running:

```sh
launchctl list | grep sqi-worker
```

View logs:

```sh
tail -f /tmp/sqi-worker.log
```

### 4. Stop and unload

```sh
launchctl stop net.uberware.sqi-worker
launchctl unload ~/Library/LaunchAgents/net.uberware.sqi-worker.plist
```

---

## Windows — Windows Service

`sqi-worker` runs as a native Windows service. When the Service Control Manager
(SCM) starts it, a service **Stop** — or a reboot — drains in-flight tasks
exactly as Ctrl-C does in a console: the worker stops taking new assignments,
waits up to `worker.shutdown_grace_period` for running tasks, deregisters and
exits. `sqi-worker service install` registers it. `sqi-server` has the same
command group; see [its operations guide](operations.md#windows-service). The
`service` command exists only in Windows builds.

Every `service` subcommand, `status` included, must run from an **elevated**
(Administrator) PowerShell. From any other shell it fails straight away with
`this command must be run from an elevated (Administrator) shell`.

### 1. Install the binary

Copy `sqi-worker.exe` to a permanent location that only administrators can
write, e.g. `C:\Program Files\sqi\sqi-worker.exe`. `service install` registers
the copy you run it from, and the service runs it as LocalSystem by default, so
a binary that ordinary users can replace is a privilege escalation.

### 2. Create a configuration file

`service install` needs the configuration file to exist — a service cannot
prompt for one — and bakes its absolute path into the service. The default
location is `C:\ProgramData\sqi\sqi-worker.yaml` (`%ProgramData%\sqi\sqi-worker.yaml`);
pass `--config` (`-c`) for another. Start from the effective defaults:

```powershell
New-Item -ItemType Directory -Force C:\ProgramData\sqi | Out-Null
& "C:\Program Files\sqi\sqi-worker.exe" config print > C:\ProgramData\sqi\sqi-worker.yaml
```

> **Create that directory administrators-only.** Any local user can create
> `C:\ProgramData\sqi` first, or read what a service later writes there. See
> [Security notes and known limitations](#security-notes-and-known-limitations)
> for the two-line `icacls` recipe, and run it *before* the commands above.

At minimum set:

```yaml
nats:
  url: "nats://sqi-server.example.com:4222"
discovery:
  enable_mdns: false
worker:
  data_dir: "C:\\ProgramData\\sqi\\worker"
```

Set `worker.data_dir` explicitly: its default is `.sqi\worker` under the running
account's profile, which for LocalSystem is inside
`C:\Windows\System32\config\systemprofile`.

Check the file before installing it with
`sqi-worker.exe start --dry-run --config C:\ProgramData\sqi\sqi-worker.yaml`.
See [`docs/worker-configuration.md`](worker-configuration.md) for every option.

### 3. Install and start the service

From an **elevated** PowerShell:

```powershell
& "C:\Program Files\sqi\sqi-worker.exe" service install --start
```

```text
installed service "sqi-worker"
  command: "C:\Program Files\sqi\sqi-worker.exe" start --config C:\ProgramData\sqi\sqi-worker.yaml
  account: LocalSystem
  log:     C:\ProgramData\sqi\logs\sqi-worker.log (unless log.file is set)
service "sqi-worker" is running (pid 4812)
```

This registers a service named `sqi-worker` (display name *sqi Worker Agent*)
that:

- runs `sqi-worker.exe start --config <absolute path of the config file>`, with
  the binary you ran the installer from. Root flags such as `--log-level` are
  **not** stored in the service; set `log.level` and `log.format` in the config
  file;
- runs as **LocalSystem** unless you pass `--user` (see below);
- runs in the configuration file's directory, so a relative path in the
  configuration resolves beside it rather than in `C:\Windows\System32`;
- starts automatically at boot as an *Automatic (Delayed Start)* service, after
  the other boot-time services. It does not wait for the server: a worker that
  cannot reach it at boot exits with a failure, and the recovery actions below
  try again;
- is restarted by the SCM after a failure — after 5 s, 30 s, then 60 s for the
  first, second and later failures, with the count reset after 24 hours without
  one — including when the worker stops itself with an error rather than
  crashing;
- is given `worker.shutdown_grace_period` + 15 s (45 s at the default) to drain
  when Windows shuts down. This is the service's *PreShutdown* timeout (see
  [Managing the service](#managing-the-service)).

The flags of `service install`:

| Flag | Meaning |
|---|---|
| `--config`, `-c` | The worker's configuration file (a flag of the root command; default `C:\ProgramData\sqi\sqi-worker.yaml`). It must exist. If it does not, the error suggests `sqi-worker config print > <path>`. |
| `--name` | Service name (default `sqi-worker`). See [Multiple workers on one host](#multiple-workers-on-one-host). |
| `--display-name` | Display name (default `sqi Worker Agent`). A service installed under another `--name` defaults to `sqi Worker Agent (<name>)`, because the SCM refuses two services with the same display name. |
| `--user` | Account to run as: `DOMAIN\name`, `.\name`, a bare local name (treated as `.\name`) or a UPN. Default LocalSystem. You are prompted for the password. See [Choosing the account](#choosing-the-account). |
| `--start` | Start the service after installing it and wait (up to 60 s) for it to run. |

`service install` refuses a name that is already registered — straight after the
elevation check, before any password prompt, permission change or other side
effect:

```text
Error: service already exists: sqi-worker; remove it first with: sqi-worker service uninstall --name sqi-worker
```

If `--start` does not find the service running — it stopped during startup, or
was still starting after 60 s — the command prints
`service "sqi-worker" did not start: …` and the last 20 lines of the
[default log file](#logs), and exits with an error. The service stays installed;
fix the problem and run `service start`.

### Choosing the account

The service runs as **LocalSystem** by default. LocalSystem reaches network
shares as the *computer* account (`DOMAIN\HOSTNAME$`), not as a user — if your
jobs read scenes or write renders on SMB shares that only grant users access,
run it as a user account:

```powershell
& "C:\Program Files\sqi\sqi-worker.exe" service install --user STUDIO\render-svc --start
```

The installer then, in this order:

1. refuses an existing service (above);
2. checks the configuration file: it must exist, and the worker reads
   `worker.shutdown_grace_period` from it;
3. prints a warning about run-as-user privileges (below) and prompts for the
   password — or reads it from piped standard input; an empty password is
   refused;
4. grants the account **Log on as a service** (`SeServiceLogonRight`);
5. checks the password with a service-type logon;
6. prepares the log directory's permissions (see [Logs](#logs)); and
7. creates the service.

The right is granted *before* the password is checked because the check needs
it. **A wrong password therefore stops the install at step 5 with the right
still granted; nothing is rolled back.** To take it back, remove the account
under *Local Security Policy* (`secpol.msc`) → *Local Policies* → *User Rights
Assignment* → *Log on as a service* (Windows Home has no `secpol.msc`).

A domain Group Policy that defines "Log on as a service" overwrites local grants
at its next refresh, after which the service fails to start with error **1069**.
The installer cannot fix that; add the account to the policy.

The account must be able to read the configuration file and write
`worker.data_dir`.

**Run-as-user queues need more privileges.** If the farm uses run-as-user queues
([task isolation](worker-configuration.md#windows)), the worker's own account
needs `SeAssignPrimaryTokenPrivilege` — the installer prints a warning naming it
whenever you pass `--user`, because the worker config cannot tell whether a
queue will use isolation — and also `SeIncreaseQuotaPrivilege`, and
`SeBackupPrivilege` and `SeRestorePrivilege` to load the target account's
profile. LocalSystem holds all of them; a user account holds them only if you
grant them. If no queue uses run-as-user, the warning does not apply.

### Paths and drive letters

Services do not see drive letters mapped in a user's logon session. Use UNC
paths (`\\nas\shows\...`) in job parameters, or map them with
[storage locations](storage-locations.md) / path mapping.

### Logs

A service has no console, so it logs to a file. Unless `log.file` is set, that
is `C:\ProgramData\sqi\logs\<service-name>.log` — `<service-name>` being the
name the service was installed under (`sqi-worker.log` by default) — rotated at
`log.max_size_mb` (default 100 MB), keeping `log.max_backups` (default 5) old
files:

```powershell
Get-Content C:\ProgramData\sqi\logs\sqi-worker.log -Wait -Tail 50
```

- **Settings.** [`log.file`](worker-configuration.md#logfile),
  [`log.max_size_mb`](worker-configuration.md#logmax_size_mb) and
  [`log.max_backups`](worker-configuration.md#logmax_backups) (environment
  `SQI_WORKER_LOG_FILE`, `SQI_WORKER_LOG_MAX_SIZE_MB` and
  `SQI_WORKER_LOG_MAX_BACKUPS`) apply to the service like any other run. A
  relative `log.file` resolves against the service's working directory, which is
  the directory that holds its configuration file. An explicit `log.file` needs
  its directory to exist already.
- **The default directory** is created with permissions that grant full control
  to SYSTEM, Administrators and the service's account only, and nothing is
  inherited. `service install` creates it; a hand-registered service creates it
  at its first start. A directory that already exists is left as it is (a
  `--user` service is added to it — see the [known limitations](#security-notes-and-known-limitations)).
- **Rotation** is by size: the file becomes `<file>.1`, `.1` becomes `.2`, and so
  on up to `log.max_backups`, and the oldest is dropped. A rotation that fails
  — most often because something such as `Get-Content -Wait`, an editor or a log
  shipper holds the file open without allowing it to be renamed — is not an
  error and loses no lines: the worker keeps appending to the current file and
  tries again after another `log.max_size_mb` of output, so the file can
  temporarily grow past its limit.
- **Startup failures** are recorded even when the worker never got as far as
  opening its log: a worker that stops with an error (for example an invalid
  config) appends one line to `C:\ProgramData\sqi\logs\<service-name>.log`, such
  as `{"time":"…","level":"ERROR","msg":"service exited with error","error":"…"}`,
  and the service reports exit code `1066 (service-specific 1)`. The write is
  best effort: if that directory cannot be created, only the exit code is left.
- A console `sqi-worker start --dry-run` opens `log.file` when it is set, so it
  can create or append to the configured file; without `log.file` a console run
  logs to stderr.

`service status` and `install --start` always show and tail the **default** log
path. If you set `log.file`, read that file instead.

### Managing the service

From an elevated PowerShell (with `sqi-worker.exe` on your `PATH`, or by its full
path). Each command takes `--name` for a service installed under another name.

```powershell
sqi-worker service status     # state, exit codes, account, start type, command line, log path
sqi-worker service stop       # asks the worker to drain, and waits for the service to stop
sqi-worker service start      # waits (up to 60 s) for the service to run
sqi-worker service uninstall  # stops it (draining first), then removes it; config, data and logs are kept
```

```text
name:       sqi-worker
state:      running
pid:        4812
exit codes: 0 (service-specific 0)
account:    LocalSystem
start type: automatic (delayed)
command:    "C:\Program Files\sqi\sqi-worker.exe" start --config C:\ProgramData\sqi\sqi-worker.yaml
log:        C:\ProgramData\sqi\logs\sqi-worker.log (unless log.file is set)
```

A service that stopped because of an error shows
`exit codes: 1066 (service-specific 1)`; one that was stopped on request shows
`0 (service-specific 0)`. That includes a Stop, or a shutdown, that arrives
while the worker is still starting up (say, still looking for the server): it is
a clean stop, never a failure the recovery actions would answer.

`Get-Service sqi-worker` (which needs no elevation), `Start-Service`,
`Stop-Service` and `services.msc` work too. To upgrade, `service stop`, replace
`sqi-worker.exe`, and `service start`; the registration keeps its path and
settings.

**How long each command waits.** `start` waits up to 60 s. `stop` and
`uninstall` wait for the service to stop for up to its PreShutdown timeout plus
15 s, and never less than 60 s (60 s at the default grace period); after that
they fail with `service sqi-worker still stop pending after …`, and the worker
carries on draining — run `service status` until it shows `stopped`.

**The PreShutdown timeout is fixed at install time.** It is
`worker.shutdown_grace_period` + 15 s (the server's is 45 s), worked out once by
`service install` from the configuration file *and* the `SQI_WORKER_*`
variables set in the shell you install from, and registered with the service.
Changing `worker.shutdown_grace_period` afterwards, in the file or in the machine
environment, does not change it: run `service uninstall` and `service install`
again. Note the running service sees the *machine* environment, not your shell's,
so a `SQI_WORKER_*` variable set only in your shell shapes the timeout but not
the worker; put settings in the configuration file.

### Registering the service by hand

`service install` is a convenience; a service registered any other way works
too, for example:

```powershell
New-Service -Name "sqi-worker" -DisplayName "sqi Worker Agent" -StartupType Automatic `
  -BinaryPathName '"C:\Program Files\sqi\sqi-worker.exe" start --config "C:\ProgramData\sqi\sqi-worker.yaml"'
Start-Service sqi-worker
```

Quote the executable and the config path inside the single-quoted string, as
above, whenever they contain spaces, and use absolute paths. Such a service
still answers the SCM natively, drains on Stop and on shutdown, runs in the
config file's directory and logs to the default file. What only `service
install` configures is missing: delayed start, the recovery actions and the
PreShutdown timeout, so on a reboot Windows may stop the worker before its drain
completes. Set recovery yourself with `sc.exe` (not `sc`, which is a PowerShell
alias for `Set-Content`):

```powershell
sc.exe failure sqi-worker reset= 86400 actions= restart/5000/restart/30000/restart/60000
sc.exe failureflag sqi-worker 1
```

The second command is what makes the SCM restart a service that stops itself
with an error rather than crashing.

### Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Error **1053**, "the service did not respond to the start or control request in a timely fashion" | The process never answered the SCM. Builds before native service support were plain console programs, so a service registered with `New-Service` timed out and stopped; upgrade `sqi-worker.exe`. On a current build, run it in a console (below) to see why it did not get that far. |
| Error **1069**, "the service did not start due to a logon failure" | The account's password is wrong or has changed, or the account no longer holds "Log on as a service" (a domain policy can remove it). Add the account to the policy if one applies, then run `service uninstall` and `service install --user …` again, which prompts for the password and grants the right. |
| The service stops straight after starting | `service status` shows `exit codes: 1066 (service-specific 1)`: the worker exited with an error. Read the [log](#logs); the reason is the last `"service exited with error"` line. The SCM keeps restarting it (5 s, 30 s, then every 60 s) until the configuration is fixed. |
| `stopped` with `exit codes: 0` that you did not ask for | Unless someone else stopped it, the worker lost its NATS connection for good and shut itself down cleanly, which the SCM does not treat as a failure. See the [known limitations](#security-notes-and-known-limitations). |
| `service already exists: …` | The name is taken. `service uninstall --name <name>` first, or choose another `--name`. |
| `must be run from an elevated (Administrator) shell` | Open PowerShell with *Run as administrator*. |
| `service uninstall` succeeds but the service is still listed | It is *marked for deletion* until every handle to it closes — typically `services.msc` or another management tool. Close them. |
| Jobs cannot find files | Mapped drive letters or share permissions — see *Choosing the account* and *Paths and drive letters*. |

**Running in a console to see startup errors.** Stop the service first — a
console run uses the same `worker.data_dir`, so it would register as the same
worker — then, from an elevated prompt:

```powershell
sqi-worker.exe service stop
sqi-worker.exe start --config C:\ProgramData\sqi\sqi-worker.yaml
```

Output goes to the console (unless `log.file` is set) and Ctrl-C drains and
stops it. It runs as *you*, not as the service account, so a problem specific to
that account (permissions, profile, mapped drives) will not reproduce.

### Security notes and known limitations

These describe what the Windows service support does today; none is hidden by
the defaults.

- **The log directory is shared.** The server and every worker on a host log to
  the one `C:\ProgramData\sqi\logs` directory. A service installed with
  `--user X` is granted an inheritable *Modify* permission on it (full control,
  if it created the directory), so `X` can read, rewrite and delete the *other*
  services' logs there — for example the LocalSystem `sqi-server`'s, which can
  carry job and environment detail. When the server and a `--user` worker share
  a host, give the worker its own `log.file` in a directory that only that
  account and administrators can write.
- **`C:\ProgramData\sqi` can be squatted.** `C:\ProgramData` lets any local user
  create folders, so a user can create `C:\ProgramData\sqi` before you do, and
  files created there inherit read access for ordinary users. The installer
  refuses a junction or symbolic link where it has to create the log directory
  or change its permissions (the directory's parent, and for `--user` the
  directory itself), but a real directory — or a junction — that already exists
  at `C:\ProgramData\sqi\logs` is used as it is, and the LocalSystem service then
  writes through it. And the service's working directory is the directory of its
  configuration file, `C:\ProgramData\sqi` by default, where the examples above
  also put `worker.data_dir` (the worker ID and, with broker authentication on,
  its credential). Create the directory yourself, administrators-only, **before**
  installing, and keep the configuration file and binaries somewhere only
  administrators can write:

  ```powershell
  New-Item -ItemType Directory -Force C:\ProgramData\sqi | Out-Null
  icacls "C:\ProgramData\sqi" /inheritance:r /grant:r "*S-1-5-18:(OI)(CI)F" "*S-1-5-32-544:(OI)(CI)F"
  ```

  (`*S-1-5-18` is SYSTEM and `*S-1-5-32-544` is Administrators.) For a `--user`
  service also grant that account what it needs, or better give it a directory
  of its own for its `--config`, `worker.data_dir` and `log.file`.
- **Losing NATS for good does not restart the worker.** When the worker's NATS
  connection is permanently lost — a finite `nats.max_reconnect_attempts` running
  out, or the server revoking its credential — it shuts down cleanly and exits
  with code 0, as it does on the console and under systemd's
  `Restart=on-failure`. The SCM's recovery actions fire only on a failure, so the
  service stays stopped. Watch the worker's online state in the web UI, or
  `service status`, and restart it with a wrapper or an external monitor.
- **`service status` and `install --start` know only the default log path.**
  With `log.file` set they still print and tail `C:\ProgramData\sqi\logs\<name>.log`,
  which may be missing or a stale file from an earlier run, and a fatal
  startup error is recorded there too. Read the file you configured.
- **Account rights are yours to grant.** The installer grants "Log on as a
  service" only. Other privileges a `--user` account needs (see [Choosing the
  account](#choosing-the-account)) are not granted for you, and a domain Group
  Policy can undo the one it grants.
- **PreShutdown is best effort.** Windows still limits the total time a shutdown
  may take, so a drain longer than the operating system allows is cut short. Tasks
  still running are then reassigned once the server notices the worker is gone.
- **`--name` addresses any service.** `service start`, `stop`, `status` and
  `uninstall` act on whatever service name you give them, like `sc.exe`:
  `sqi-worker service uninstall --name Spooler` would delete the print spooler.

---

## Docker

See [`docs/worker-docker.md`](worker-docker.md) for the full Docker deployment
guide including image details, required environment variables, volume mounts,
and network requirements.

Quick start:

```sh
docker run -d \
  --name sqi-worker \
  -e SQI_WORKER_NATS_URL=nats://sqi-server:4222 \
  -e SQI_WORKER_DISCOVERY_ENABLE_MDNS=false \
  -e SQI_WORKER_DATA_DIR=/var/lib/sqi-worker \
  -v sqi-worker-data:/var/lib/sqi-worker \
  ghcr.io/uberware/sqi/sqi-worker:latest
```

---

## Multiple workers on one host

A single worker already executes as many tasks concurrently as the host has CPU
cores — the server gates capacity by CPU-core fit — so one worker per host is
usually enough for throughput. Run *separate* worker processes only when you
want distinct identities: independent heartbeats and registrations, different
capability sets or compute locations, or separate queue assignments.

Each instance must have its own `worker.data_dir` (it holds the persistent
`worker.id` UUID) and its own `metrics.addr` (the local health/metrics port),
and should have a distinct `worker.name`. See
[Running multiple workers on one host](worker-configuration.md#running-multiple-workers-on-one-host)
for the full rationale.

On Linux the idiomatic approach is a **systemd template unit**, where the
instance identifier (`%i`) carries the per-instance metrics port. Create
`/etc/systemd/system/sqi-worker@.service`:

```ini
[Unit]
Description=sqi distributed task worker (instance %i)
Documentation=https://github.com/uberware/sqi
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=sqiworker
Group=sqiworker
ExecStart=/usr/local/bin/sqi-worker start --config /etc/sqi/sqi-worker.yaml
Restart=on-failure
RestartSec=5s
StandardOutput=journal
StandardError=journal
SyslogIdentifier=sqi-worker-%i

# Per-instance identity (%i is the metrics port, e.g. 9091)
Environment=SQI_WORKER_NAME=%H-worker-%i
Environment=SQI_WORKER_DATA_DIR=/var/lib/sqi-worker-%i
Environment=SQI_WORKER_METRICS_ADDR=127.0.0.1:%i

# Per-instance state directory: /var/lib/sqi-worker-<port>
StateDirectory=sqi-worker-%i
StateDirectoryMode=0750

# Security hardening
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/sqi-worker-%i

[Install]
WantedBy=multi-user.target
```

Enable one instance per port:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now sqi-worker@9091 sqi-worker@9092 sqi-worker@9093
```

The shared `/etc/sqi/sqi-worker.yaml` holds everything common (NATS URL,
capability tags, queues); the template's `Environment=` lines override only the
three settings that must differ per instance. Manage each with the usual
`systemctl status sqi-worker@9091` / `journalctl -u sqi-worker@9091`.

> **macOS:** the same rule applies — give each launchd plist a unique `Label`,
> `SQI_WORKER_DATA_DIR`, and `SQI_WORKER_METRICS_ADDR`.

> **Windows:** install each instance as its own service with its own `--name`
> and `--config`, and give each configuration file its own `worker.data_dir`,
> `metrics.addr` and `worker.name` (`service install` gives each instance its own
> configuration file, not per-instance environment variables):
>
> ```powershell
> & "C:\Program Files\sqi\sqi-worker.exe" service install `
>   --name sqi-worker-2 --config C:\ProgramData\sqi\sqi-worker-2.yaml --start
> ```
>
> The instance gets the display name `sqi Worker Agent (sqi-worker-2)`, logs to
> `C:\ProgramData\sqi\logs\sqi-worker-2.log`, and is managed with
> `sqi-worker service status --name sqi-worker-2`. See
> [Windows — Windows Service](#windows--windows-service).

> **Docker:** containers are already filesystem-isolated, so just run multiple
> containers with distinct `--name` values and separate data volumes — see
> [`docs/worker-docker.md`](worker-docker.md).

---

## Verifying the deployment

After starting the worker, confirm it registered with the server:

```sh
# Via REST API
curl -s http://sqi-server:8080/api/v1/workers | jq '.[].name'

# Via sqi-worker health probe (from the worker host)
curl -sf http://127.0.0.1:9091/healthz && echo healthy
curl -sf http://127.0.0.1:9091/readyz  && echo ready
```

The web UI at `http://sqi-server:8080` → **Workers** shows registered workers
with their capability tags and live task counts.

---

## Rolling restarts

`sqi-worker` handles `SIGTERM` gracefully: it stops accepting new assignments
and waits up to `worker.shutdown_grace_period` (default 30 s) for in-flight
tasks to complete. systemd sends `SIGTERM` before `SIGKILL`, so the default
`TimeoutStopSec=90s` in systemd is sufficient for most render workloads.

For long-running renders, increase `shutdown_grace_period` to match your
longest expected task duration and set `TimeoutStopSec` in the unit file
accordingly.

On Windows a service **Stop** and a reboot start the same drain. The service's
PreShutdown timeout is fixed when `service install` runs, so after raising
`shutdown_grace_period` run `service uninstall` and `service install` again — see
[Managing the service](#managing-the-service).

---

## See also

- [`docs/worker-configuration.md`](worker-configuration.md) — Every configuration option.
- [`docs/auth.md`](auth.md#broker-authentication-transport) — The broker authentication model in full: threat model, revocation, key rotation, and the auth-off asymmetry.
- [`docs/worker-capabilities.md`](worker-capabilities.md) — Capability tag reference.
- [`docs/worker-docker.md`](worker-docker.md) — Docker deployment details.
- [`docs/observability.md`](observability.md) — In-UI diagnostic panels, REST/WS log API, and log forwarding to journald, Docker, Loki, and ELK.
- [`config/sqi-worker.example.yaml`](https://github.com/uberware/sqi/blob/main/config/sqi-worker.example.yaml) — Annotated example config.
