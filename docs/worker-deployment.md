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

Every archive holds the binaries, the example configuration files and the
service files for all three platforms (use the ones for yours):

| In the archive | What it is |
|---|---|
| `sqi-worker`, `sqi-server` (`.exe` on Windows) | The binaries. |
| `config/sqi-worker.example.yaml`, `config/sqi-server.example.yaml` | Every setting, annotated. |
| `deploy/systemd/sqi-worker.service`, `sqi-worker@.service`, `sqi-server.service` | Linux systemd units. |
| `deploy/launchd/net.uberware.sqi-worker.plist` | macOS launchd agent for the worker. |

```sh
# Linux (x86_64)
curl -Lo sqi.tar.gz https://github.com/uberware/sqi/releases/latest/download/sqi_Linux_x86_64.tar.gz
mkdir sqi
tar -xzf sqi.tar.gz -C sqi
cd sqi
sudo install -m 0755 sqi-worker /usr/local/bin/sqi-worker
```

Run the commands in the sections below from the extracted `sqi` directory, which
is where `deploy/` is.

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

`sqi-worker config init` writes a starter configuration file: the settings the
worker needs to start and nothing else. Every other setting keeps its default.

```sh
sudo sqi-worker config init --nats-url nats://sqi-server.example.com:4222
```

```text
wrote /etc/sqi/sqi-worker.yaml
next: sudo cp deploy/systemd/sqi-worker.service /etc/systemd/system/ && sudo systemctl enable --now sqi-worker
```

The file it writes holds `nats.url` (or a commented-out example, when you leave
`--nats-url` off and the worker finds the server by mDNS) and `worker.data_dir`:

| Platform | Default file | `worker.data_dir` it sets |
|---|---|---|
| Linux | `/etc/sqi/sqi-worker.yaml` | `/var/lib/sqi-worker` |
| macOS | `~/.sqi/sqi-worker.yaml` | `~/Library/Application Support/sqi-worker` |
| Windows | `C:\ProgramData\sqi\sqi-worker.yaml` | `C:\ProgramData\sqi\worker` |

The root `--config` flag writes the file somewhere else (the data directory then
moves too on Windows, where it sits beside the file), and the `next:` line names
that file: on Windows in the `service install` command, on Linux and macOS as
what to change in the unit or agent, which reads the default path. `--nats-url` takes a
comma-separated list for a NATS cluster. `config init` refuses to overwrite a
file that exists; `--force` replaces it. The platform sections below say what
each platform needs before and after this step, because the file's owner and
permissions differ.

To change anything else, add it to the file: the annotated
[`config/sqi-worker.example.yaml`](https://github.com/uberware/sqi/blob/main/config/sqi-worker.example.yaml)
in the archive lists every setting. A worker that cannot find the server by mDNS
needs `nats.url`, which `--nats-url` sets.

`nats.url` is the broker's address: `sqi-server`'s own, or, when the server
runs with an external NATS cluster (`nats.mode: external`), the cluster's.
For a cluster, list every node, comma-separated, so the worker can move to
another node when the one it is connected to is lost:

```yaml
nats:
  url: "tls://nats-a.example.com:4222,tls://nats-b.example.com:4222,tls://nats-c.example.com:4222"
```

A worker that finds its server over mDNS uses the one broker URL the server
advertises instead.

A worker executes as many tasks concurrently as it has CPU cores; the server
gates capacity by CPU-core fit, so there is no per-worker concurrency knob to
set. On a multi-site farm you may also want to declare where this worker runs
so the server can resolve storage paths and honour location affinity:

```yaml
worker:
  compute_location: "onprem"      # matches a storage-location root name
  capability_tags: ["maya_2025", "arnold_7"]
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

Every enrollment failure past this point (an expired, unknown or
already-used token, or a worker ID already bound to a different key) is
fatal and explicit: the worker logs the cause and exits rather than looping
in the background with no visible reason. An unreachable *server* is different: that
still falls back to the ordinary reconnect-with-backoff behavior once a
credential exists. A broker that refuses the credential is retried for 2
minutes, with a WARN at each refusal, before the worker exits, so it rides out
a `sqi-server` or database restart; see
[How a connection is authorized](auth.md#how-a-connection-is-authorized).

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
   Once enrolled, the credential is accepted at this worker's next connect; sqi-server needs no restart.
   ```

2. Copy that command to the server host (out of band — SSH, a console, a
   provisioning script) and run it there:

   ```sh
   sqi-server worker enroll --worker-id 3f2a... --public-key UABC...XYZ
   ```

3. Start the worker. It finds the seed already on disk, skips enrollment
   entirely, and connects directly with the credential. A running server
   needs no restart: the broker asks `sqi-server` to authorize every
   connection, and `sqi-server` reads the credential from the database each
   time.

No join token and no REST call are involved in this path at any point.

### Revoking a worker's credential

- **Immediately**, against a running server: `DELETE
  /api/v1/workers/{id}/credential`. This disconnects the worker inside the
  call — its in-flight leases reclaim through the normal heartbeat-sweep
  path. On an external NATS cluster that needs the server's
  `nats.external.callout.system_creds_file`; without it the worker keeps its
  connection until its authorization expires, within `nats.auth.user_ttl`.
- **Offline**, without a running server (or as part of a maintenance
  script): `sqi-server worker revoke <worker-id>`. This writes the database
  directly and disconnects nothing: the worker is refused at its next
  connect, within `nats.auth.user_ttl` (1 hour by default) at the latest.

A revoked worker's reconnect is refused, and it exits after the 2-minute
grace.

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
4. Restart the worker. It finds the new seed `keygen` already wrote and
   connects directly, and the running server admits it. No server restart,
   join token or REST enrollment call is needed on this path.

The worker ID can be reused once its old credential is revoked; the old
public key itself can never be enrolled again, on this worker ID or any
other. To rotate via a fresh join token instead of `keygen`, remove
`worker.nk` before restarting so the worker re-enrolls as it did on first
boot (Path A above).

---

## Linux — systemd

Run these from the directory you extracted the release archive into (see
[Pre-built binaries](#pre-built-binaries)), with `sqi-worker` installed in
`/usr/local/bin`. The unit file the archive ships is
[`deploy/systemd/sqi-worker.service`](https://github.com/uberware/sqi/blob/main/deploy/systemd/sqi-worker.service).

### 1. Create a dedicated user

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin sqiworker
```

Do this before the next step: `config init` makes the configuration file
readable by the `sqiworker` group and stops with this same `useradd` command if
the group does not exist. The unit creates `/var/lib/sqi-worker` (the worker's
state directory) owned by that user, so there is nothing else to prepare.

### 2. Write the configuration

```sh
sudo sqi-worker config init --nats-url nats://sqi-server.example.com:4222
```

This writes `/etc/sqi/sqi-worker.yaml`, mode `0640`, owned by root and the
`sqiworker` group (the directory `/etc/sqi` is created `0755` if it is missing).
Leave `--nats-url` off to have the worker find the server by mDNS. See
[Configuration](#configuration) for what the file holds and for `--force` and
a cluster URL.

A file written to any other `--config` location is `0600` and owned by whoever
ran the command, so the service account cannot read it: keep the default
location for a systemd service.

### 3. Install the unit and start

```sh
sudo cp deploy/systemd/sqi-worker.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now sqi-worker
```

The unit runs `/usr/local/bin/sqi-worker start --config /etc/sqi/sqi-worker.yaml`
as `sqiworker`, restarts it after a failure, sends its output to journald and
confines it with `NoNewPrivileges`, `PrivateTmp` and `ProtectSystem=strict`,
leaving `/var/lib/sqi-worker` writable.

> **Note:** If `ProtectSystem=strict` prevents the worker from executing render
> tools, replace it with `ProtectSystem=full` or remove it. Render workers often
> need read access to shared network mounts.

Check status:

```sh
sudo systemctl status sqi-worker
sudo journalctl -u sqi-worker -f
```

### 4. Upgrade

Replace the binary and restart:

```sh
sudo systemctl stop sqi-worker
sudo install -m 0755 ./sqi-worker /usr/local/bin/sqi-worker
sudo systemctl start sqi-worker
```

A unit file that changed between releases is copied again, followed by
`sudo systemctl daemon-reload`.

---

## macOS — launchd

Run these from the directory you extracted the release archive into, with
`sqi-worker` installed in `/usr/local/bin`:

```sh
sudo mkdir -p /usr/local/bin
sudo install -m 0755 sqi-worker /usr/local/bin/sqi-worker
```

### 1. Write the configuration

```sh
sqi-worker config init --nats-url nats://sqi-server.example.com:4222
```

This writes `~/.sqi/sqi-worker.yaml`, mode `0600`, and creates the worker's data
directory, `~/Library/Application Support/sqi-worker`, mode `0700`. Leave
`--nats-url` off to have the worker find the server by mDNS. See
[Configuration](#configuration).

### 2. Install the launchd agent

The archive ships a per-user agent (it runs while the user is logged in),
[`deploy/launchd/net.uberware.sqi-worker.plist`](https://github.com/uberware/sqi/blob/main/deploy/launchd/net.uberware.sqi-worker.plist).
It runs `/usr/local/bin/sqi-worker start` with the configuration at
`$HOME/.sqi/sqi-worker.yaml` and has no placeholder to edit: launchd sets
`HOME` for an agent. It starts at load, restarts the worker after a failure, and
writes the worker's output to `/tmp/sqi-worker.log`.

```sh
mkdir -p ~/Library/LaunchAgents
cp deploy/launchd/net.uberware.sqi-worker.plist ~/Library/LaunchAgents/
```

For a system-wide daemon (runs without a user logged in), place a copy of the
plist at `/Library/LaunchDaemons/net.uberware.sqi-worker.plist` and run it as a
dedicated service account: add `UserName`, and replace the shell command with an
explicit `start --config` naming a configuration file that account can read
(write it with `config init --config`, run as that account).

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
`this command must be run from an elevated (Administrator) shell`. So does
`config init` whenever it has to create or protect a directory, which includes
the default location.

### 1. Install the binary

Copy `sqi-worker.exe` to a permanent location that only administrators can
write, from an **elevated PowerShell**:

```powershell
New-Item -ItemType Directory -Force -Path 'C:\Program Files\sqi' | Out-Null
Copy-Item .\sqi-worker.exe 'C:\Program Files\sqi\'
```

`service install` registers the copy you run it from, and the service runs it as
LocalSystem by default, so a binary that ordinary users can replace is a
privilege escalation. `service install` therefore refuses a binary that anyone
other than SYSTEM and Administrators can change, or that sits in a directory
they can change (see [What `service install` checks](#what-service-install-checks)).
That rules out running it from the folder you extracted the archive into when
that is under a user profile, such as `Downloads`. Copy the file from an
elevated PowerShell as above: a file created from an elevated Git Bash is owned
by your own account, which the check refuses.

### 2. Create the configuration directory and file

From an **elevated PowerShell**:

```powershell
& "C:\Program Files\sqi\sqi-worker.exe" config init --nats-url nats://sqi-server.example.com:4222
```

```text
wrote C:\ProgramData\sqi\sqi-worker.yaml
next: & 'C:\Program Files\sqi\sqi-worker.exe' service install --start
```

`config init` creates `C:\ProgramData\sqi` if it does not exist, owned by
Administrators, with permissions that let only SYSTEM and Administrators in
(full control, nothing inherited), and writes the configuration file into it:

```yaml
nats:
  url: 'nats://sqi-server.example.com:4222'

worker:
  data_dir: 'C:\ProgramData\sqi\worker'
```

(After a header comment. Leave `--nats-url` off and a comment shows where to put
the URL instead, for a worker that finds the server by mDNS.) `config init` also
creates the data directory, `C:\ProgramData\sqi\worker`, which inherits the
configuration directory's permissions.

The directory is administrators-only because the configuration file, `worker.data_dir`
(the worker ID, and the worker's credential when broker authentication is on)
and, for `sqi-server`, the database all live in it, and the service reads them
as LocalSystem. A user who could edit the file could point `nats.url` at a
server of their own, which would then hand the worker tasks to run as
LocalSystem. See [Why the directory is created for you](#why-the-directory-is-created-for-you).

`worker.data_dir` is written explicitly because left out it defaults to a
folder under the profile of whichever account loads the file, which for
LocalSystem is inside `C:\Windows\System32\config\systemprofile`;
`nats.credential_file` follows it (`<data_dir>\worker.nk`), which is where the
worker's private key goes when broker authentication is on. `service install`
refuses a configuration that leaves `worker.data_dir` out.

Options:

- `--config` (a flag of the root command) writes somewhere other than
  `C:\ProgramData\sqi\sqi-worker.yaml`. `worker.data_dir` becomes a `worker`
  folder beside the file, so each configuration file gets a data directory of
  its own. The path must be an ordinary local path on a drive letter: `\\?\` and
  `\\.\` paths, UNC paths (`\\server\share`) and NTFS stream names
  (`file:stream`) are refused.
- `--user ACCOUNT` prepares for a service that runs as that account; see
  [Choosing the account](#choosing-the-account).
- `--force` replaces a file that exists; without it `config init` stops. In a
  protected directory, the file being replaced must pass the same check as the
  directory first.

A directory under `%ProgramData%`, one that does not exist yet, or one for
`--user` is created administrators-only or, if it exists, must already be (see
below). Any other existing directory you name with `--config` is written into as
it is; `service install` decides later whether it is safe to run from.

Check the file with
`sqi-worker.exe start --dry-run --config C:\ProgramData\sqi\sqi-worker.yaml`.
See [`docs/worker-configuration.md`](worker-configuration.md) for every option.

The paths in the file are single-quoted YAML, where a backslash is literal. In a
double-quoted YAML string every backslash must be doubled
(`"C:\\ProgramData\\sqi\\worker"`). A single one starts an escape sequence:
either one YAML does not know, which is a parse error (`"C:\sqi\worker"` fails
with `found unknown escape character`), or, worse, one it does, which loads
silently as something else — `"D:\render\tmp"` becomes `D:`, a carriage return,
`ender`, a tab and `mp`.

> **`config print` is not a template for this file.** It prints the values in
> effect *in the shell that ran it*: your own profile as `worker.data_dir` and as
> `nats.credential_file`, the host name as `worker.name`, and any `SQI_WORKER_*`
> variables you have set. Installed as it is, a LocalSystem service would keep its
> credential in your profile. Use it to see every option; copy only the settings
> you mean to change into the file `config init` wrote.

#### Why the directory is created for you

`C:\ProgramData` lets **any local user create folders**, and whoever creates
`C:\ProgramData\sqi` first owns it: they keep the right to change its
permissions, and those of everything they put in it, however much you tighten it
afterwards. That includes a configuration file they put there in advance:
overwriting a file keeps its permissions, so it stays theirs to edit. A squatted
directory is code execution as SYSTEM, not just a look at some logs.

So `config init` makes the directory itself, as administrator, and
`service install` makes it too when it is missing. Neither repairs one that
already exists. An existing directory under `%ProgramData%` that anyone other than SYSTEM
and Administrators owns or can change is refused:

```text
Error: cannot use configuration directory C:\ProgramData\sqi:
config directory C:\ProgramData\sqi: BUILTIN\Users can write (WD,AD,WEA,WA)
Someone other than SYSTEM and Administrators can change it. Move it aside from an elevated PowerShell:
  Rename-Item -LiteralPath 'C:\ProgramData\sqi' -NewName ('sqi.suspect-' + (Get-Date -Format yyyyMMddHHmmss))
then run config init again.
```

**If you get this,** keep the directory only if you created it, as administrator,
and its contents are ones you recognise; otherwise treat whoever created it as
hostile. Run the `Rename-Item` command it prints, which renames a junction or
symbolic link as itself and touches nothing it points at; copy nothing out of the
directory, least of all a configuration file; and run `config init` again.
`cmd.exe /c rmdir "C:\ProgramData\sqi"` also removes a junction, and only the
link: it refuses a real directory that is not empty. Do not try to repair a
squatted directory in place with `icacls`: its creator owns everything they put
in it, and an owner can always rewrite the permissions again.

A folder you make by hand in `C:\ProgramData` (`New-Item`, Explorer) is refused
the same way, because `C:\ProgramData` gives every user write access to what is
created there. Let `config init` create it.

A service that runs as an ordinary account (`--user`) cannot read
`C:\ProgramData\sqi`; see [Choosing the account](#choosing-the-account) for
giving it a directory of its own.

### 3. Install and start the service

From an **elevated** PowerShell:

```powershell
& "C:\Program Files\sqi\sqi-worker.exe" service install --start
```

```text
installed service "sqi-worker"
  command: "C:\Program Files\sqi\sqi-worker.exe" start --config C:\ProgramData\sqi\sqi-worker.yaml
  account: LocalSystem
  log:     C:\ProgramData\sqi\logs\sqi-worker\sqi-worker.log (unless log.file is set)
service "sqi-worker" is running (pid 4812)
```

`service install` needs the configuration file to exist — a service cannot
prompt for one — and bakes its absolute path into the service. The default
location is `C:\ProgramData\sqi\sqi-worker.yaml` (`%ProgramData%\sqi\sqi-worker.yaml`);
pass `--config` (`-c`) for another. Before it changes anything it checks that
the binary, the configuration and everything the configuration names are in
places only administrators can change, and refuses otherwise; see
[What `service install` checks](#what-service-install-checks).

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
- is given 55 s at the default to drain when Windows shuts down:
  `worker.shutdown_grace_period` (30 s) for running tasks to finish, a third of
  that again (10 s) for any still running to close once asked to before they
  are killed, and 15 s for what follows: up to 2 s for each task's final
  status to be sent, up to 10 s more for any the broker has not yet stored
  (normally far less), and the deregistration and exit. This is the
  service's *PreShutdown* timeout (see [Managing the service](#managing-the-service)).

The flags of `service install`:

| Flag | Meaning |
|---|---|
| `--config`, `-c` | The worker's configuration file (a flag of the root command; default `C:\ProgramData\sqi\sqi-worker.yaml`). It must exist. If it does not, the error suggests `sqi-worker config init --config <path>`. |
| `--name` | Service name (default `sqi-worker`). See [Multiple workers on one host](#multiple-workers-on-one-host). |
| `--display-name` | Display name (default `sqi Worker Agent`). A service installed under another `--name` defaults to `sqi Worker Agent (<name>)`, because the SCM refuses two services with the same display name. |
| `--user` | Account to run as: `DOMAIN\name`, `.\name`, a bare local name (treated as `.\name`) or a UPN. Default LocalSystem. You are prompted for the password. See [Choosing the account](#choosing-the-account). |
| `--start` | Start the service after installing it, wait (up to 60 s) for it to run, then watch it for 3 s more (see below). |

`service install` refuses a name that is already registered — straight after the
elevation check, before any password prompt, permission change or other side
effect:

```text
Error: service already exists: sqi-worker; remove it first with: sqi-worker service uninstall --name sqi-worker
```

**`is running` means the service stayed up for 3 seconds.** The service reports
*Running* to the Service Control Manager as soon as its process is up, and the
worker reads its configuration and opens its log *after* that. So `--start` (and
`service start`) keeps watching a service that has reported *Running* for 3 s
more: a bad configuration stops it within that window, and the command reports
a failed start (below). A failure that comes later than that, or in a restart
the recovery actions make, it does not see: that shows up only in
`service status` (`exit codes: 1066 (service-specific 1)`) and in the
[log](#logs). A failed service is restarted after 5 s (then 30 s, then 60 s),
and in that window `service status` can show `running` or `start pending` for the
restarted process: check again after the restart delay before trusting it.

If the service does not start — it stops before it runs or within those 3 s, or
is still starting after 60 s — the command prints
`service "sqi-worker" did not start: …` (for a stop, with its exit codes) and the
last 20 lines of the [default log file](#logs), and exits with an error. The
service stays installed; fix the problem and run `service start`.

### What `service install` checks

A service that runs as LocalSystem trusts every file it reads and every program
it starts, so `service install` checks them before it does anything else: before
the password prompt, the logon right and the log directory. If `%ProgramData%\sqi`
does not exist it first creates it administrators-only, as `config init` does.
It then checks each path in this table and refuses to install if any fails,
listing every failing path with the setting that named it:

| Checked | Where it comes from |
|---|---|
| The configuration file and its directory | `--config` |
| The binary and its directory | The `.exe` you ran `service install` from |
| The log file's directory and its parent | `log.file`, when set |
| The service's own log directory, `C:\ProgramData\sqi\logs\<service-name>`, and `C:\ProgramData\sqi\logs` | Always: the service writes its log there |
| The worker's session directory | `worker.session_dir`, or its default when unset |
| Every file or directory the configuration names | TLS certificates, keys and CA files (`nats.tls_*`, `nats.server_tls_ca_file`, `metrics.tls.*`), the NATS credential and join-token files, `worker.data_dir`, `staging.scratch_dir`, `staging.sync_command`, `worker.power.sleep_command` and `shutdown_command`, `worker.commands.<name>.path` |

A command is checked only when it is written with a path separator. A bare name
such as `rsync` is looked up on the machine's `PATH` and is not checked.

For `sqi-server` the configuration paths are `http.tls.*`, `nats.embedded.*`
(`data_dir` and its TLS files), `nats.external.*` (key seeds, credentials files
and TLS files), `store.sqlite_path`, `auth.ldap.ca_file` and each
`capacity.providers.<name>.command`; it has no session directory.

**The rule.** A path passes when all of these hold:

- it is a real file or directory, never a junction, symbolic link or other
  reparse point, so a folder-mounted volume or a folder under OneDrive is
  refused;
- its owner is SYSTEM, Administrators or TrustedInstaller, or the `--user`
  account the service will run as, whose own files it may change;
- nobody else can write to it, delete it, change its permissions or take its
  ownership. For a directory, that includes anyone who can add or replace files
  in it.

A path that does not exist yet is judged by its nearest existing ancestor,
because whoever can create children there could create it first. **Every
directory above the path, up to the volume root, is judged too:** one fails if
someone else owns it or can delete or rename what it holds, because they could
move the path aside and plant their own. Being able to *add* files to a parent
is fine, which is why a path under `C:\ProgramData` can pass. (A volume
root's own DELETE right is ignored, since a root cannot be renamed.) A drive
letter that is an alias, such as a global `subst` or a mapped network drive,
counts as a volume root, so the directories above the real folder behind it
are not judged: keep service files on local NTFS volumes.

What this means in practice:

- **Binary and configuration under a user profile fail.** `C:\Users\<name>\...`
  is owned by that user, who can change everything in it. Put the binary in
  `C:\Program Files\sqi` (step 1) and the configuration in a directory
  `config init` creates.
- **A `C:\ProgramData` folder you made by hand fails**, because every user can
  write to what they create there. Let `config init` make it, or move the
  folder aside (above) and run it again.
- **Data on a mounted volume or under OneDrive fails**, as the path goes through
  a reparse point. Keep `worker.data_dir` and the other paths on an ordinary
  folder on a local drive.
- **The `--user` account passes in its own directory**: `config init --user`
  gives it exactly that (see [Choosing the account](#choosing-the-account)).

A refusal names the setting and the path, and says what to do. For a binary it
prints the `New-Item` and `Copy-Item` commands from step 1 for your copy of the
file. For a path under `C:\ProgramData` it prints the `Rename-Item` command above
and says to write a new configuration with `config init`, except for a file a
setting names, such as a TLS certificate, key or CA file or a credential, which
nothing creates again: for that it says to fix the file's permissions or move it
to an administrators-only folder. For anything else it asks you to make the path
administrators-only or to point the setting at a path that is.

**What the check does not cover.**

- It judges a directory and everything above it, **not what is already inside
  it**. If a setting names a folder that existed before you set the service up,
  look at what is in it.
- File paths inside `store.postgres.dsn` (`sslrootcert`, `sslcert`, `sslkey`,
  `passfile`) are not checked.
- Application paths a worker finds by itself from its capability detectors are
  not checked. A worker with no run-as-user queue runs those tasks as the
  service account.
- A bare command name resolves on the machine's `PATH`, which is not checked.
- A service registered by hand rather than with `service install` gets no check
  beyond the junction refusal on its log directory.
- It reads the configuration with the `SQI_*` variables of the shell you install
  from applied, which the running service does not see (it sees the machine's
  environment). Put settings in the file.

`service install --user` also gives the account a single read-attributes and
traverse entry on `C:\ProgramData\sqi` and on `C:\ProgramData\sqi\logs`, so that
its service can reach its own log directory below them. `service uninstall`
leaves those entries; they let the account open nothing else there.

### Choosing the account

The service runs as **LocalSystem** by default. LocalSystem reaches network
shares as the *computer* account (`DOMAIN\HOSTNAME$`), not as a user — if your
jobs read scenes or write renders on SMB shares that only grant users access,
run it as a user account. The account cannot read `C:\ProgramData\sqi`, which is
administrators-only, and you should not open that directory to it, so it gets a
directory of its own. Write the configuration for it, then install:

```powershell
& "C:\Program Files\sqi\sqi-worker.exe" config init --user STUDIO\render-svc `
  --nats-url nats://sqi-server.example.com:4222 `
  --config C:\ProgramData\sqi-render\sqi-worker.yaml
& "C:\Program Files\sqi\sqi-worker.exe" service install --user STUDIO\render-svc `
  --config C:\ProgramData\sqi-render\sqi-worker.yaml
```

`config init --user` does the same as without `--user`, in the directory you name,
and then:

- gives the account **read** on the configuration directory, so it reads the
  configuration file but cannot edit it (an account that can edit the file
  chooses what it runs, and `service install` would trust it for that);
- creates the worker's data directory, `C:\ProgramData\sqi-render\worker`, and
  gives the account **Modify** on it;
- creates a session directory, `C:\ProgramData\sqi-render\sessions`, gives the
  account **Modify** on it, and writes it as `worker.session_dir`: the default
  session root is under the administrators-only `C:\ProgramData\sqi`, where the
  account could not create the directory each task runs in;
- needs no password, and does not log the account on;
- refuses the default `C:\ProgramData\sqi`, which would let the account read every
  other service's configuration, and any directory inside it, which
  `service install --user` refuses (the default directory is recognised however
  its path is spelled), and an account name that does not resolve to a user.

It prints the `service install` command with `--user` and `--config` filled in,
without `--start`.
Run `service install` with the same `--config`. `service install --user` refuses
a configuration inside `C:\ProgramData\sqi`, which the account cannot read, and
says to write one with `config init --user`. The other way round, when
`service install` without `--user` refuses a data or session directory that
grants a user account Modify, the refusal adds that the configuration looks
written for `--user` with that account.

`--start` is left out on purpose: before the service first starts, give the
account whatever else it needs — the privilege below for run-as-user queues,
access to the shares its jobs use, Modify on a `log.file` directory — then run
`service start`, or add `--start` when it needs nothing more.

Anything else the worker writes needs Modify for the account too: a `log.file`
belongs in the data directory (`C:\ProgramData\sqi-render\worker`) or another directory the
account has Modify on, **not** in `C:\ProgramData\sqi-render` itself, where the
account can only read — the path passes validation there, and the service then
fails to open the file at every start.

The installer then, in this order:

1. refuses an existing service (above);
2. checks the configuration file: it must exist, and the worker reads
   `worker.shutdown_grace_period` from it; it must set `worker.data_dir`, and
   a `worker.session_dir` outside `C:\ProgramData\sqi`;
3. checks that the binary, the configuration and every path it names are in
   places only administrators (and this account, for its own files) can change
   (see [What `service install` checks](#what-service-install-checks));
4. prints a warning about run-as-user privileges (below) and prompts for the
   password — or reads it from piped standard input; an empty password is
   refused;
5. grants the account **Log on as a service** (`SeServiceLogonRight`);
6. checks the password with a service-type logon;
7. prepares the log directory's permissions (see [Logs](#logs)); and
8. creates the service.

The right is granted *before* the password is checked because the check needs
it. **A wrong password therefore stops the install at step 6 with the right
still granted; nothing is rolled back.** To take it back, remove the account
under *Local Security Policy* (`secpol.msc`) → *Local Policies* → *User Rights
Assignment* → *Log on as a service* (Windows Home has no `secpol.msc`).

A domain Group Policy that defines "Log on as a service" overwrites local grants
at its next refresh, after which the service fails to start with error **1069**.
The installer cannot fix that; add the account to the policy.

**Run-as-user queues.** If the farm uses run-as-user queues
([task isolation](worker-configuration.md#windows)), the worker's own account
needs `SeAssignPrimaryTokenPrivilege` — the installer prints a warning naming it
whenever you pass `--user`, because the worker config cannot tell whether a
queue will use isolation. The worker also checks for `SeIncreaseQuotaPrivilege`
before it runs a task as another account, and loading that account's profile
needs `SeBackupPrivilege` and `SeRestorePrivilege`. That is the least sqi itself
asks of the account, not a promise that granting it is enough: Microsoft
documents `LoadUserProfile` as callable only by an administrator or LocalSystem.
**LocalSystem is the supported account for a worker that uses run-as-user
isolation**; run it as a user account for the share access above when no queue
uses run-as-user, and the warning does not apply.

### Paths and drive letters

Services do not see drive letters mapped in a user's logon session. Use UNC
paths (`\\nas\shows\...`) in job parameters, or map them with
[storage locations](storage-locations.md) / path mapping.

### Logs

A service has no console, so it logs to a file. Unless `log.file` is set, that
is `C:\ProgramData\sqi\logs\<service-name>\<service-name>.log` — `<service-name>` being the
name the service was installed under (`sqi-worker\sqi-worker.log` by default) — rotated at
`log.max_size_mb` (default 100 MB), keeping `log.max_backups` (default 5) old
files:

```powershell
Get-Content C:\ProgramData\sqi\logs\sqi-worker\sqi-worker.log -Wait -Tail 50
```

- **Settings.** [`log.file`](worker-configuration.md#logfile),
  [`log.max_size_mb`](worker-configuration.md#logmax_size_mb) and
  [`log.max_backups`](worker-configuration.md#logmax_backups) (environment
  `SQI_WORKER_LOG_FILE`, `SQI_WORKER_LOG_MAX_SIZE_MB` and
  `SQI_WORKER_LOG_MAX_BACKUPS`) apply to the service like any other run. A
  relative `log.file` resolves against the service's working directory, which is
  the directory that holds its configuration file. An explicit `log.file` needs
  its directory to exist already.
- **The default directory** is the service's own: each service on a host has
  one, named after it, under `C:\ProgramData\sqi\logs`. It is created with
  permissions that grant full control to SYSTEM, Administrators and the
  service's account only, and nothing is inherited; `C:\ProgramData\sqi\logs`
  itself stays SYSTEM and Administrators only, so no service's account reaches
  another's logs. `service install` creates it; a hand-registered service
  creates it at its first start. A directory that already exists is left as it
  is, except that `service install --user` always adds its account to it,
  whatever `log.file` says, and that `service install` refuses one that anyone
  other than SYSTEM, Administrators and the service's account can change (see
  [What `service install` checks](#what-service-install-checks)) and both it and
  the starting service refuse one that is, or sits under, a junction or symbolic
  link (see the [known limitations](#security-notes-and-known-limitations)).
- **Rotation** is by size: the file becomes `<file>.1`, `.1` becomes `.2`, and so
  on up to `log.max_backups`, and the oldest is dropped. A rotation that fails
  — most often because something such as `Get-Content -Wait`, an editor or a log
  shipper holds the file open without allowing it to be renamed — is not an
  error and loses no lines: the worker keeps appending to the current file and
  tries again after another `log.max_size_mb` of output, so the file can
  temporarily grow past its limit.
- **Startup failures** are recorded even when the worker never got as far as
  opening its log: a worker that stops with an error (for example an invalid
  config) appends one line to `C:\ProgramData\sqi\logs\<service-name>\<service-name>.log`, such
  as `{"time":"…","level":"ERROR","msg":"service exited with error","error":"…"}`,
  and the service reports exit code `1066 (service-specific 1)`. When that file
  cannot be written — say its directory is, or sits under, a junction, which the
  service refuses, or the account may not write there — the line goes instead to `<service-name>.trace.log` in
  the service's working directory (the configuration file's directory), with a
  `log_error` field saying why the default log could not take it; the file gets
  that directory's permissions. Both writes are best effort: if neither
  succeeds, only the exit code is left.
- A console `sqi-worker start --dry-run` opens `log.file` when it is set, so it
  can create or append to the configured file; without `log.file` a console run
  logs to stderr.

`service status` shows, and `service start` and `install --start` tail, the
**default** log path. If you set `log.file`, read that file instead.

### Managing the service

From an elevated PowerShell (with `sqi-worker.exe` on your `PATH`, or by its full
path). Each command takes `--name` for a service installed under another name.

```powershell
sqi-worker service status     # state, exit codes, account, start type, command line, log path
sqi-worker service stop       # asks the worker to drain, and waits for the service to stop
sqi-worker service start      # waits (up to 60 s) for the service to run, then 3 s more
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
log:        C:\ProgramData\sqi\logs\sqi-worker\sqi-worker.log (unless log.file is set)
```

A service that stopped because of an error shows
`exit codes: 1066 (service-specific 1)`; one that was stopped on request shows
`0 (service-specific 0)`. That includes a Stop, or a shutdown, that arrives
while the worker is still starting up (say, still looking for the server): it is
a clean stop, never a failure the recovery actions would answer.

`Get-Service sqi-worker` (which needs no elevation), `Start-Service`,
`Stop-Service` and `services.msc` work too. To upgrade, `service stop`, replace
`sqi-worker.exe` (copy the new file in from an elevated PowerShell, so it stays
administrators-only), and `service start`; the registration keeps its path and
settings. The check in [What `service install` checks](#what-service-install-checks)
runs again whenever you run `service install`, so a binary that anyone other than
SYSTEM and Administrators can change is refused then.

**How long each command waits.** `start` waits up to 60 s for the service to
run, then watches it for 3 s more. `stop` and `uninstall` wait for the service
to stop for up to its PreShutdown timeout plus 15 s, and never less than 60 s
(70 s at the default grace period); after that they fail with
`service sqi-worker still stop pending after …`, and the worker carries on
draining — run `service status` until it shows `stopped`.

**The PreShutdown timeout is fixed at install time.** It is
`worker.shutdown_grace_period`, plus a third of it for tasks still running at
the end to close, plus 15 s — 55 s at the default (the server's is 45 s) —
worked out once by
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
above, whenever they contain spaces, and use absolute paths: a relative
`--config` is resolved against the directory the SCM starts the process in,
`C:\Windows\System32`. Such a service still answers the SCM natively, drains on
Stop and on shutdown, runs in the config file's directory and logs to the default
file. It does not get the checks `service install` makes, so keep its binary and
configuration where only administrators can change them (steps 1 and 2).

**`--config` is required.** A service registered without it refuses to start:
it stops at once with `exit codes: 1066 (service-specific 1)`, and its
`service exited with error` line reads
`running as a Windows service requires --config: …`. Without `--config` the
worker would search for its configuration, and two of the places it looks are
folders any local user can create: `config\sqi-worker.yaml` under the service's
working directory (`C:\ProgramData\sqi` when there is no `--config`) and
`\etc\sqi\sqi-worker.yaml` on that drive (`C:\etc\sqi`). Whoever put a file
there would choose what a LocalSystem service runs. (Before stopping, such a
service still changes into `C:\ProgramData\sqi`, creating it with ProgramData's
inherited permissions if it is missing, so write the configuration with
`config init` as in [step 2](#2-create-the-configuration-directory-and-file) either way.)

What only `service install` configures is missing: delayed start, the recovery
actions and the PreShutdown timeout. Set the first two yourself with `sc.exe`
(not `sc`, which is a PowerShell alias for `Set-Content`):

```powershell
sc.exe config sqi-worker start= delayed-auto
sc.exe failure sqi-worker reset= 86400 actions= restart/5000/restart/30000/restart/60000
sc.exe failureflag sqi-worker 1
```

`failureflag 1` is what makes the SCM restart a service that stops itself with an
error rather than crashing. Neither `New-Service` nor `sc.exe` sets the
PreShutdown timeout: to give the worker its full drain time on a reboot, use
`service install`. Without it, Windows may stop the worker before its drain
completes.

### Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Error **1053**, "the service did not respond to the start or control request in a timely fashion" | The process never answered the SCM. Builds before native service support were plain console programs, so a service registered with `New-Service` timed out and stopped; upgrade `sqi-worker.exe`. On a current build, run it in a console (below) to see why it did not get that far. |
| Error **1069**, "the service did not start due to a logon failure" | The account's password is wrong or has changed, or the account no longer holds "Log on as a service" (a domain policy can remove it). Add the account to the policy if one applies, then run `service uninstall` and `service install --user …` again, which prompts for the password and grants the right. |
| The service stops straight after starting | `service status` shows `exit codes: 1066 (service-specific 1)`: the worker exited with an error. `service start` and `install --start` report a stop within 3 s of the service running as a failed start; a later one they miss, having printed `is running` (see above). Read the [log](#logs); the reason is the last `"service exited with error"` line — or, when the log directory could not be used, the line in `sqi-worker.trace.log` beside the configuration file. The SCM keeps restarting it (5 s, 30 s, then every 60 s) until the configuration is fixed. |
| `stopped` with `exit codes: 0` that you did not ask for | Unless someone else stopped it, the worker lost its NATS connection for good and shut itself down cleanly, which the SCM does not treat as a failure. See the [known limitations](#security-notes-and-known-limitations). |
| `refusing to install sqi-worker: someone other than SYSTEM, Administrators and the service's account can change what it would trust or run` | `service install` found a path that someone else could change. The lines after it name each path and the setting it came from. See [What `service install` checks](#what-service-install-checks): typically the binary is in a user profile (copy it to `C:\Program Files\sqi` from an elevated PowerShell) or the configuration is in a `C:\ProgramData` folder you made by hand (write it with `config init`). |
| `cannot use configuration directory …` from `config init` | The directory exists and someone other than SYSTEM and Administrators can change it, or a directory above it. Run the `Rename-Item` command it prints, or pass `--config` for another directory. See [Why the directory is created for you](#why-the-directory-is-created-for-you). |
| `service already exists: …` | The name is taken. `service uninstall --name <name>` first, or choose another `--name`. |
| `must be run from an elevated (Administrator) shell` | Open PowerShell with *Run as administrator*. |
| `service uninstall` succeeds but the service is still listed | It is *marked for deletion* until every handle to it closes — typically `services.msc` or another management tool. Close them. |
| Jobs cannot find files | Mapped drive letters or share permissions — see *Choosing the account* and *Paths and drive letters*. |

**Running in a console to see startup errors.** Stop the service first — a
console run uses the same `worker.data_dir`, so it would register as the same
worker — then, from an elevated prompt:

```powershell
sqi-worker.exe service stop
Set-Location C:\ProgramData\sqi
sqi-worker.exe start --config C:\ProgramData\sqi\sqi-worker.yaml
```

Output goes to the console (unless `log.file` is set) and Ctrl-C drains and
stops it. Run it from the configuration file's directory, as the `Set-Location`
does: a service runs there, but a console run resolves relative paths — a
relative `log.file`, say — against the *shell's* directory, which for an elevated
prompt is `C:\Windows\System32`. It runs as *you*, not as the service account, so
a problem specific to that account (permissions, profile, mapped drives) will not
reproduce.

### Security notes and known limitations

These describe what the Windows service support does today; none is hidden by
the defaults.

- **`service install`'s checks are narrow, and a squatted `C:\ProgramData\sqi`
  is what they are for.** `config init` creating the directory, and
  [What `service install` checks](#what-service-install-checks) refusing a
  configuration, binary or data path that anyone else could change, are the
  mitigation; nothing stands in for them when you skip them. What they cannot
  close is a race: a path is checked and then used by name, so a directory swapped
  for a junction between the two — during the install, or while the service
  runs — still redirects what the service reads or writes, and someone who
  already owns a directory on the path can make that swap at will (which is why
  the check refuses an owner it does not trust). It also does not cover what that
  section lists as out of scope: file paths inside `store.postgres.dsn`,
  application paths a capability detector finds, bare command names resolved on
  the `PATH`, and the contents of a directory that already existed. A starting
  service checks only its default log directory for a junction or symbolic link,
  so a service registered by hand (which never ran `service install`) gets that
  and nothing more.
- **Losing NATS for good does not restart the worker.** When the worker's NATS
  connection is permanently lost — a finite `nats.max_reconnect_attempts` running
  out, or the server revoking its credential — it shuts down cleanly and exits
  with code 0, as it does on the console and under systemd's
  `Restart=on-failure`. The SCM's recovery actions fire only on a failure, so the
  service stays stopped. Watch the worker's online state in the web UI, or
  `service status`, and restart it with a wrapper or an external monitor.
- **`service status` prints, and `service start` and `install --start` tail,
  only the default log path.** With `log.file` set they still print and tail
  `C:\ProgramData\sqi\logs\<name>\<name>.log`, which may be missing or a stale file from
  an earlier run, and a fatal startup error is recorded there too (or, when that
  directory cannot be used, in `<name>.trace.log` beside the configuration file,
  which they never show). Read the file you configured.
- **Account rights are yours to grant.** The installer grants "Log on as a
  service" only. Other privileges a `--user` account needs (see [Choosing the
  account](#choosing-the-account)) are not granted for you, a domain Group Policy
  can undo the one it grants, and LocalSystem is the supported account for
  run-as-user isolation.
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
instance identifier (`%i`) carries the per-instance metrics port. The release
archive ships it as `deploy/systemd/sqi-worker@.service`; copy it to
`/etc/systemd/system/sqi-worker@.service`, or create that file from:

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
> configuration file, not per-instance environment variables). Write each file
> into a directory of its own with `config init`: its `worker.data_dir` is then
> the `worker` folder beside the file, so two instances never share one, and the
> directory is administrators-only as in
> [step 2](#2-create-the-configuration-directory-and-file). Add `metrics.addr` and `worker.name` to
> the file it writes, then install:
>
> ```powershell
> & "C:\Program Files\sqi\sqi-worker.exe" config init `
>   --nats-url nats://sqi-server.example.com:4222 `
>   --config C:\ProgramData\sqi-worker-2\sqi-worker.yaml
> # edit C:\ProgramData\sqi-worker-2\sqi-worker.yaml: set metrics.addr and worker.name
> & "C:\Program Files\sqi\sqi-worker.exe" service install `
>   --name sqi-worker-2 --config C:\ProgramData\sqi-worker-2\sqi-worker.yaml --start
> ```
>
> The instance gets the display name `sqi Worker Agent (sqi-worker-2)`, logs to
> `C:\ProgramData\sqi\logs\sqi-worker-2\sqi-worker-2.log`, and is managed with
> `sqi-worker service status --name sqi-worker-2`. See
> [Windows — Windows Service](#windows--windows-service).

> **Docker:** containers are already filesystem-isolated, so run multiple
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
