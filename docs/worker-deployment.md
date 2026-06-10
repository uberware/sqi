# sqi-worker Deployment Guide

This document covers installing and running `sqi-worker` on bare metal (Linux,
macOS, Windows) and in Docker, including how to configure auto-start on boot in
each environment.

---

## Installation

### Pre-built binaries

Download the latest release from the GitHub Releases page. Binaries are
provided for Linux, macOS, and Windows on `amd64` and `arm64`.

```sh
# Linux (amd64)
curl -Lo sqi-worker https://github.com/uberware/sqi/releases/latest/download/sqi-worker_linux_amd64
chmod +x sqi-worker
sudo mv sqi-worker /usr/local/bin/
```

Verify the download against the published checksums:

```sh
curl -Lo checksums.txt https://github.com/uberware/sqi/releases/latest/download/checksums.txt
sha256sum --check --ignore-missing checksums.txt
```

### Build from source

Requirements: Go 1.26.3 or later (see `go.mod` for the pinned toolchain version).

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
  max_concurrent_tasks: 4
```

Validate before deploying:

```sh
sqi-worker start --dry-run --config /etc/sqi/sqi-worker.yaml
```

See [`docs/worker-configuration.md`](worker-configuration.md) for every
available option.

---

## Linux — systemd

### 1. Create a dedicated user

```sh
sudo useradd --system --no-create-home --shell /bin/false sqiworker
sudo mkdir -p /var/lib/sqi-worker
sudo chown sqiworker:sqiworker /var/lib/sqi-worker
```

### 2. Install the systemd unit file

Create `/etc/systemd/system/sqi-worker.service`:

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
  max_concurrent_tasks: 4
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
  -v sqi-worker-data:/var/lib/sqi-worker \
  ghcr.io/uberware/sqi/sqi-worker:latest
```

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
- [`docs/worker-capabilities.md`](worker-capabilities.md) — Capability tag reference.
- [`docs/worker-docker.md`](worker-docker.md) — Docker deployment details.
- [`config/sqi-worker.example.yaml`](../config/sqi-worker.example.yaml) — Annotated example config.
