# sqi-server Operations Guide

This document covers installing, running, upgrading, and maintaining
`sqi-server` in production.

---

## Installation

### Pre-built binaries

Download the latest release from the GitHub Releases page. Binaries are
provided for Linux, macOS, and Windows on `amd64` and `arm64`.

```sh
# Linux (amd64)
curl -Lo sqi-server https://github.com/uberware/sqi/releases/latest/download/sqi-server_linux_amd64
chmod +x sqi-server
sudo mv sqi-server /usr/local/bin/
```

Verify the download against the published checksums:

```sh
curl -Lo checksums.txt https://github.com/uberware/sqi/releases/latest/download/checksums.txt
sha256sum --check --ignore-missing checksums.txt
```

### Docker

```sh
docker pull ghcr.io/uberware/sqi-server:latest

docker run -d \
  --name sqi-server \
  -p 8080:8080 \
  -v /data/sqi:/data \
  -e SQI_STORE_SQLITE_PATH=/data/sqi.db \
  -e SQI_NATS_DATA_DIR=/data/nats \
  ghcr.io/uberware/sqi-server:latest serve
```

### Build from source

Requirements: Go 1.23 or later (see `go.mod` for the pinned toolchain version).

```sh
git clone https://github.com/uberware/sqi.git
cd sqi
make build
# Binary is at ./bin/sqi-server
```

---

## First-run setup

### 1. Create a configuration file

Copy the example and edit it:

```sh
mkdir -p /etc/sqi
cp config/sqi-server.example.yaml /etc/sqi/sqi-server.yaml
$EDITOR /etc/sqi/sqi-server.yaml
```

Minimum required changes for production:

- `store.sqlite_path` — set to an absolute path on a local SSD
- `nats.data_dir` — set to a persistent directory for JetStream storage
- `http.addr` — restrict to `127.0.0.1:8080` if a reverse proxy handles TLS

See [`docs/configuration.md`](configuration.md) for all options.

### 2. Run schema migrations

Migrations run automatically at startup, but you can also run them explicitly:

```sh
sqi-server migrate up --config /etc/sqi/sqi-server.yaml
```

Check migration status at any time:

```sh
sqi-server migrate status --config /etc/sqi/sqi-server.yaml
```

### 3. Start the server

```sh
sqi-server serve --config /etc/sqi/sqi-server.yaml
```

Confirm it is healthy:

```sh
curl -sf http://localhost:8080/healthz && echo "alive"
curl -sf http://localhost:8080/readyz  && echo "ready"
```

---

## Running as a system service

### systemd (Linux)

Create `/etc/systemd/system/sqi-server.service`:

```ini
[Unit]
Description=sqi distributed task server
After=network.target
Wants=network.target

[Service]
Type=simple
User=sqi
Group=sqi
ExecStart=/usr/local/bin/sqi-server serve --config /etc/sqi/sqi-server.yaml
Restart=on-failure
RestartSec=5s

# Give the process time to drain in-flight work before SIGKILL
TimeoutStopSec=60s

# Logging goes to journald; no need to configure a log file.
StandardOutput=journal
StandardError=journal
SyslogIdentifier=sqi-server

# Resource limits — tune to your hardware
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
```

```sh
sudo useradd -r -s /sbin/nologin sqi
sudo systemctl daemon-reload
sudo systemctl enable --now sqi-server
sudo journalctl -u sqi-server -f
```

---

## Upgrade

`sqi-server` upgrades are zero-downtime when using a process supervisor: new
migrations are applied before the HTTP listener accepts traffic, so a rolling
restart is safe.

### Steps

1. Download (or build) the new binary.
2. Replace the binary on disk.
3. Restart the service — migrations apply automatically:

```sh
sudo systemctl restart sqi-server
```

If you prefer to run migrations explicitly before restarting:

```sh
# With the server stopped
sqi-server migrate up --config /etc/sqi/sqi-server.yaml
# Then restart
sudo systemctl start sqi-server
```

### Rolling back

Downgrades require a `migrate down` to the schema version of the older binary:

```sh
# Stop the new binary first
sudo systemctl stop sqi-server

# Roll back one migration step
sqi-server migrate down --config /etc/sqi/sqi-server.yaml

# Restore the old binary, then start
sudo systemctl start sqi-server
```

Always take a backup before upgrading (see [Backup and Restore](#backup-and-restore)).

---

## Backup and Restore

### Online backup with `sqi-server backup`

`sqi-server backup` uses SQLite's `VACUUM INTO` to snapshot the database while
the server is running. The backup is a fully checkpointed, self-contained
`.db` file — workers do not need to be paused.

```sh
sqi-server backup \
  --db /data/sqi.db \
  --out /backups/sqi/sqi-$(date +%Y%m%d-%H%M%S).db
```

The `--db` flag defaults to `$SQI_SQLITE_PATH` (or `sqi.db` if that variable is
unset). Note that this is a different environment variable from `SQI_STORE_SQLITE_PATH`
used by the running server; set `--db` explicitly or export `SQI_SQLITE_PATH` to
match your deployment.

The command opens the source database read-only and writes an identical clean
copy to the destination path. It exits non-zero if the destination file already
exists — use a timestamped filename or a fresh directory each time.

### Automated daily backup (cron)

```cron
0 2 * * * sqi /usr/local/bin/sqi-server backup \
  --db /data/sqi.db \
  --out /backups/sqi/sqi-$(date +\%Y\%m\%d).db \
  && find /backups/sqi -name "*.db" -mtime +30 -delete
```

This keeps 30 days of daily backups and rotates older files automatically.

### Backup alongside NATS data

The SQLite database holds all persistent job and task state. The NATS
JetStream data directory (`nats.data_dir`) holds in-flight messages; it does
not need to be backed up — workers re-register on reconnection and
`sqi-server` requeues any unacknowledged work on startup.

If you want to preserve in-flight state across a complete host failure, back up
the NATS data directory at the same time:

```sh
tar -czf /backups/nats/nats-$(date +%Y%m%d).tar.gz /data/nats
```

### Restore

To restore from a backup:

1. Stop `sqi-server`.
2. Replace the live database with the backup file:
   ```sh
   sudo systemctl stop sqi-server
   cp /backups/sqi/sqi-20260115.db /data/sqi.db
   ```
3. Optionally clear the NATS data directory (stale messages from after the
   backup timestamp will be replayed or discarded on reconnect):
   ```sh
   rm -rf /data/nats
   ```
4. Restart the server — migrations will run if the backup is from an older schema version:
   ```sh
   sudo systemctl start sqi-server
   ```

---

## Log management

### Output format

By default `sqi-server` writes structured JSON logs to stdout:

```json
{"time":"2026-01-15T10:00:00.000Z","level":"INFO","msg":"server started","addr":"0.0.0.0:8080"}
```

Switch to human-readable text for development:

```sh
sqi-server serve --log-format text --log-level debug
```

Or via config / environment:

```yaml
log:
  format: json    # json | text
  level: info     # debug | info | warn | error
```

```sh
SQI_LOG_LEVEL=debug SQI_LOG_FORMAT=text sqi-server serve
```

### Routing logs

When running under systemd, logs flow to journald automatically:

```sh
# Live tail
journalctl -u sqi-server -f

# Last 1000 lines as JSON
journalctl -u sqi-server -n 1000 -o json
```

To write to a file instead, redirect stdout in the service unit or use a
log-forwarding agent (Fluentd, Vector, Promtail) reading from journald.

### Log rotation

Because `sqi-server` writes to stdout rather than a file, log rotation is
handled outside the process:

- **journald** rotates automatically; tune retention with `journald.conf`
  (`SystemMaxUse`, `MaxRetentionSec`).
- **File-based logging**: if you redirect stdout to a file, use `logrotate`
  with `copytruncate` (no signal needed — the server does not hold a file
  descriptor to a log file):
  ```
  /var/log/sqi/sqi-server.log {
      daily
      rotate 14
      compress
      delaycompress
      copytruncate
      missingok
      notifempty
  }
  ```

---

## Metrics scraping

`sqi-server` exposes Prometheus metrics at `GET /metrics` (text format).

### Available metrics

| Metric | Type | Description |
|---|---|---|
| `http_requests_total` | counter | HTTP requests by method, path, and status |
| `http_request_duration_seconds` | histogram | HTTP request latency |
| `scheduler_queue_depth` | gauge | Ready tasks waiting for assignment, by queue |
| `scheduler_tasks_total` | counter | Tasks processed by final status |
| `scheduler_assignment_duration_seconds` | histogram | Time from ready → assigned |
| `scheduler_idle_workers` | gauge | Workers online but not assigned a task |
| `workers_total` | gauge | Registered workers by status |
| `nats_published_total` | counter | NATS messages published by subject |
| `nats_consumed_total` | counter | NATS messages consumed by subject |
| `db_query_duration_seconds` | histogram | SQLite query latency by operation |
| `license_active_checkouts` | gauge | Active license checkouts by pool |

### Prometheus scrape config

```yaml
# prometheus.yml
scrape_configs:
  - job_name: sqi-server
    static_configs:
      - targets: ["localhost:8080"]
    metrics_path: /metrics
    scrape_interval: 15s
```

### Example alert rules

```yaml
# sqi-server-alerts.yaml
groups:
  - name: sqi
    rules:
      - alert: SqiServerDown
        expr: up{job="sqi-server"} == 0
        for: 1m
        labels:
          severity: critical
        annotations:
          summary: "sqi-server is not reachable"

      - alert: SqiHighQueueDepth
        expr: scheduler_queue_depth > 1000
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Scheduler queue depth exceeds 1000 ready tasks"

      - alert: SqiNoIdleWorkers
        expr: scheduler_idle_workers == 0 and scheduler_queue_depth > 0
        for: 2m
        labels:
          severity: warning
        annotations:
          summary: "No idle workers while tasks are queued"
```

---

## Graceful shutdown

`sqi-server` handles `SIGINT` and `SIGTERM` with a graceful shutdown sequence:

1. Stop accepting new HTTP and WebSocket connections.
2. Wait for in-flight HTTP requests to complete.
3. Drain and flush the embedded NATS JetStream (in-flight messages are
   acknowledged or requeued).
4. Run a final WAL checkpoint on the SQLite database.
5. Close all open database connections.
6. Exit with code 0.

Under systemd the `TimeoutStopSec=60s` in the example unit file gives the
server 60 seconds to drain. Increase this if you have long-running HTTP
streams or large NATS queues.

---

## Diagnosing performance issues

Enable `pprof` endpoints temporarily for profiling (never leave enabled in
production):

```yaml
http:
  enable_pprof: true
```

Or via environment:

```sh
SQI_HTTP_ENABLE_PPROF=true sqi-server serve
```

Endpoints are available at `/debug/pprof/`. Use standard Go tools:

```sh
# CPU profile (30-second sample)
go tool pprof http://localhost:8080/debug/pprof/profile?seconds=30

# Heap profile
go tool pprof http://localhost:8080/debug/pprof/heap

# Goroutine dump
curl http://localhost:8080/debug/pprof/goroutine?debug=2
```

---

## Checking effective configuration

Print the fully-merged configuration (defaults + file + env) without starting
the server:

```sh
sqi-server config print --config /etc/sqi/sqi-server.yaml
```

---

## Version information

```sh
sqi-server version
# sqi-server v0.1.0-alpha (commit abc1234, built 2026-01-15, go1.23.0)
```
