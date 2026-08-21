# sqi-server Operations Guide

This document covers installing, running, upgrading, and maintaining
`sqi-server` in production.

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
tar -xzf sqi.tar.gz sqi-server
chmod +x sqi-server
sudo mv sqi-server /usr/local/bin/
```

Verify the download against the published checksums:

```sh
curl -Lo checksums.txt https://github.com/uberware/sqi/releases/latest/download/checksums.txt
sha256sum --check --ignore-missing checksums.txt
```

> On macOS, extract the matching `sqi_Darwin_<arch>` archive. The binaries are
> signed with an Apple Developer ID and notarized, so Gatekeeper allows them to
> run without the quarantine workaround.

### Docker

```sh
docker pull ghcr.io/uberware/sqi/sqi-server:latest

docker run -d \
  --name sqi-server \
  -p 8080:8080 \
  -v /data/sqi:/data \
  -e SQI_STORE_SQLITE_PATH=/data/sqi.db \
  -e SQI_NATS_DATA_DIR=/data/nats \
  ghcr.io/uberware/sqi/sqi-server:latest serve
```

### Build from source

Requirements: Go 1.26 or later (the `go` directive in `go.mod` pins 1.26.3)
and Node.js 24 or later with npm 11 or later (see `.nvmrc`), used to build the
web UI bundle that is embedded into the `sqi-server` binary.

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

Always take a backup before upgrading (see [Backup and restore](#backup-and-restore)).

---

## Backup and restore

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

## Worker broker credentials

`sqi-server worker` groups the offline CLI commands for NATS broker
authentication (`nats.auth.*` — see
[Broker authentication](auth.md#broker-authentication-transport) for the
full model). Like `migrate` and `backup`, these subcommands open the SQLite
database file directly and do not start an HTTP server or NATS broker, so
they work whether or not `sqi-server` is running — and, independently,
whether or not the user-facing `auth.enabled` is on.

> **These commands do not read the server's config file either, and unlike
> `backup` they create and migrate the database if it doesn't already
> exist.** `--db` defaults to `$SQI_SQLITE_PATH` (or `sqi.db` in the working
> directory) — not `store.sqlite_path`, and not `SQI_STORE_SQLITE_PATH` — and
> every `worker` subcommand applies pending migrations to whatever `--db`
> resolves to before doing anything else. Point one at the wrong path and it
> silently creates a fresh, empty database and prints a token or enrolls a
> worker the running server can never see or validate. Pass `--db`
> explicitly, or export `SQI_SQLITE_PATH` to match your deployment.

### Issue a join token

```sh
sqi-server worker token issue --db /data/sqi.db --ttl 1h
```

Prints the raw token to stdout exactly once — capture it
(`TOKEN=$(sqi-server worker token issue --db /data/sqi.db)`) or store it
securely; only its hash is kept in the database. `--ttl` defaults to `1h`
and is bounded 1 minute to 24 hours. Hand the token to a worker via
`nats.join_token_file` (preferred) or `nats.join_token`, or mint one over
REST instead with `POST /api/v1/workers/join-tokens` when `auth.enabled` is
also on.

### Enroll a worker manually

```sh
sqi-server worker enroll --db /data/sqi.db \
  --worker-id 3f2a... --public-key UABC...XYZ
```

Registers a worker's broker credential directly, by worker ID and public
key — the offline counterpart to self-service enrollment over REST. Run
`sqi-worker keygen` on the worker host first; it prints this exact command
with the worker's own ID and public key filled in. A **running**
`sqi-server` does not see the new credential until it restarts — the broker
builds its authorized-key set once at startup, and this command writes the
database from a separate process with no broker handle.

### Revoke a worker's credential

```sh
sqi-server worker revoke --db /data/sqi.db <worker-id>
```

Revokes a worker credential in the database. Takes effect the next time
`sqi-server` starts, not immediately — to disconnect a worker at once
against a running server, use `DELETE /api/v1/workers/{id}/credential`
instead.

### List worker credentials

```sh
sqi-server worker list --db /data/sqi.db
```

Lists every worker credential that has not been revoked: worker ID, name,
public key, enrollment time, and last-seen time. Last-seen is set on worker
registration (startup and reconnect) only, never by heartbeat — it answers
"when did this worker last (re)connect", not "is it up right now".

---

## Log management

See [`docs/observability.md`](observability.md) for the full observability
guide, including the in-UI diagnostic panels, the REST and WebSocket APIs, and
worked examples for wiring sqi logs to journald, Docker, Loki, and ELK.

### Output format

By default `sqi-server` writes structured JSON logs to **stderr**:

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

To write to a file instead, redirect stderr in the service unit or use a
log-forwarding agent (Fluentd, Vector, Promtail) reading from journald.

### Log rotation

Because `sqi-server` writes to stderr rather than a file, log rotation is
handled outside the process:

- **journald** rotates automatically; tune retention with `journald.conf`
  (`SystemMaxUse`, `MaxRetentionSec`).
- **File-based logging**: if you redirect stderr to a file, use `logrotate`
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

| Metric | Type | Labels | Description |
|---|---|---|---|
| `sqi_http_requests_total` | counter | `method`, `path`, `status_code` | HTTP requests by method, path, and status |
| `sqi_http_request_duration_seconds` | histogram | `method`, `path` | HTTP request latency |
| `sqi_scheduler_queue_depth` | gauge | `queue` | Leasable ready tasks waiting for assignment, by queue (excludes tasks in retry backoff and tasks under paused/parked jobs) |
| `sqi_scheduler_tasks_total` | counter | `queue`, `status` | Tasks processed by final status |
| `sqi_scheduler_assignment_duration_seconds` | histogram | `result` | Wall-clock time for a single task-assignment attempt, by `result` (`assigned`, `deferred`, `error`) — not queue residency |
| `sqi_scheduler_idle_workers` | gauge | `farm` | Workers online but not assigned a task |
| `sqi_workers_total` | gauge | `status` | Registered workers by status |
| `sqi_nats_published_total` | counter | `subject` | NATS messages published by subject |
| `sqi_nats_consumed_total` | counter | `subject` | NATS messages consumed by subject |
| `sqi_db_query_duration_seconds` | histogram | `operation` | SQLite query latency by operation |
| `sqi_usage_active_claims` | gauge | `pool` | Active usage-pool claims by pool |
| `sqi_scheduler_task_retries_total` | counter | `queue` | Tasks re-queued by automatic retry, by queue |
| `sqi_scheduler_jobs_autoparked_total` | counter | `queue` | Jobs auto-parked at their failure limit, by queue |

> **Five of these are registered but not yet populated**, so they emit no
> samples at all — a Prometheus `*Vec` with no children produces no series, and
> an alert written against one will sit silent rather than fire:
> `sqi_scheduler_tasks_total`, `sqi_scheduler_assignment_duration_seconds`,
> `sqi_nats_published_total`, `sqi_nats_consumed_total`, and
> `sqi_db_query_duration_seconds`. (The worker exports its own
> `sqi_worker_nats_published_total` / `sqi_worker_nats_consumed_total` on its
> metrics port; those are different metrics and are populated.)

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
        expr: sqi_scheduler_queue_depth > 1000
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Scheduler queue depth exceeds 1000 ready tasks"

      - alert: SqiNoIdleWorkers
        expr: sqi_scheduler_idle_workers == 0 and sqi_scheduler_queue_depth > 0
        for: 2m
        labels:
          severity: warning
        annotations:
          summary: "No idle workers while tasks are queued"
```

---

## Graceful shutdown

`sqi-server` handles `SIGINT` and `SIGTERM` with a graceful shutdown sequence:

1. Stop the mDNS responder (goodbye packets first, so discoverers drop the
   service immediately instead of waiting for the record to expire).
2. Stop accepting new HTTP and WebSocket connections.
3. Wait for in-flight HTTP requests to complete.
4. Stop the scheduler.
5. Drain and flush the embedded NATS JetStream (in-flight messages are
   acknowledged or requeued), then shut down the broker.
6. Run a final WAL checkpoint in TRUNCATE mode on the SQLite database.
7. Close both SQLite connection pools (write and read).
8. Exit with code 0.

The server imposes its own **30 s** internal drain deadline
(`server.ShutdownTimeout`, a compile-time constant — not a config key); past
that it logs `graceful shutdown timed out after 30s` and exits. The example
unit's `TimeoutStopSec=60s` is deliberate headroom over that 30 s so systemd
never SIGKILLs mid-drain; raising it further has no effect on how long the
server actually waits.

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
# sqi-server v0.3.0 (commit abc1234, built 2026-07-09, go1.26.3)
```
