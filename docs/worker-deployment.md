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

   This writes `/var/lib/sqi-worker/worker.nk` (mode `0600`) and prints the
   worker's public key and the exact command to run next:

   ```
   Public key: UABC...XYZ
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
2. `sqi-worker keygen --force --data-dir <data-dir>` on the worker host.
   This overwrites `worker.nk` with a new keypair and prints the new
   public key and the `sqi-server worker enroll` command for step 3 — it
   must run *after* step 1, since the worker ID stays bound to the old
   public key on the server until that credential is revoked.
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

### 1. Install the binary

Copy `sqi-worker.exe` to a permanent location, e.g.:

```
C:\Program Files\sqi\sqi-worker.exe
```

### 2. Create a configuration file

```
C:\ProgramData\sqi\sqi-worker.yaml
```

At minimum:

```yaml
nats:
  url: "nats://sqi-server.example.com:4222"
discovery:
  enable_mdns: false
worker:
  data_dir: "C:\\ProgramData\\sqi\\worker"
```

### 3. Register as a Windows service

Open a PowerShell prompt **as Administrator**:

```powershell
New-Service `
  -Name "sqi-worker" `
  -DisplayName "sqi Worker Agent" `
  -Description "sqi distributed task worker" `
  -BinaryPathName '"C:\Program Files\sqi\sqi-worker.exe" start --config "C:\ProgramData\sqi\sqi-worker.yaml"' `
  -StartupType Automatic

Start-Service sqi-worker
```

Check status:

```powershell
Get-Service sqi-worker
```

View the Windows Event Log for output (the worker writes JSON to stderr, which
Windows services route to the Event Log when `StandardOutput` is not
redirected):

```powershell
Get-EventLog -LogName Application -Source sqi-worker -Newest 50
```

### 4. Stop and remove the service

```powershell
Stop-Service sqi-worker
Remove-Service sqi-worker
```

### 5. Auto-start on boot

The service is created with `StartupType Automatic`, so Windows starts it on
every boot without additional configuration.

To delay start until after network services are ready:

```powershell
Set-Service sqi-worker -StartupType AutomaticDelayedStart
```

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

> **macOS / Windows:** the same rule applies — give each launchd plist or
> Windows service a unique `Label`/service name, `SQI_WORKER_DATA_DIR`, and
> `SQI_WORKER_METRICS_ADDR`.

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

---

## See also

- [`docs/worker-configuration.md`](worker-configuration.md) — Every configuration option.
- [`docs/auth.md`](auth.md#broker-authentication-transport) — The broker authentication model in full: threat model, revocation, key rotation, and the auth-off asymmetry.
- [`docs/worker-capabilities.md`](worker-capabilities.md) — Capability tag reference.
- [`docs/worker-docker.md`](worker-docker.md) — Docker deployment details.
- [`docs/observability.md`](observability.md) — In-UI diagnostic panels, REST/WS log API, and log forwarding to journald, Docker, Loki, and ELK.
- [`config/sqi-worker.example.yaml`](https://github.com/uberware/sqi/blob/main/config/sqi-worker.example.yaml) — Annotated example config.
