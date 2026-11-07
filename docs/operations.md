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
  -e SQI_NATS_EMBEDDED_DATA_DIR=/data/nats \
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

- `store.sqlite_path` — set to an absolute path on a local SSD, or set
  `store.driver: postgres` and `store.postgres.dsn` to keep the state in
  PostgreSQL instead (see [Running on PostgreSQL](#running-on-postgresql))
- `nats.embedded.data_dir` — set to a persistent directory for JetStream
  storage. To run on a NATS cluster of your own instead of the embedded
  broker, see [Running on an external NATS cluster](#running-on-an-external-nats-cluster)
- `http.addr` — restrict to `127.0.0.1:8080` if a reverse proxy handles TLS
- `http.tls` / `nats.embedded.tls` — terminate TLS in sqi instead, if there is
  no proxy. Both are off by default, so the API and the worker transport are
  plaintext until you set one or the other. See [`docs/tls.md`](tls.md);
  enabling TLS on a running farm takes a coordinated restart of the server and
  every worker

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

### Windows service

`sqi-server` runs as a native Windows service: the Service Control Manager
(SCM) starts it, and a service **Stop** — or a reboot — runs the
[graceful shutdown](#graceful-shutdown) sequence. Its `service` command group is
the same as `sqi-worker`'s, and
[worker deployment](worker-deployment.md#windows--windows-service) is the
reference for what the two share — flags, the password check for `--user`,
timeouts, log files and rotation, troubleshooting and the security notes — read
with `sqi-server` for `sqi-worker`. The worker-only parts do not apply to the
server: delayed start (the server starts automatically, not delayed),
`worker.shutdown_grace_period` (the server's drain is a fixed 30 s), the
NATS-loss exit, and the run-as-user privileges and the warning that names them.
The command a service runs is different too: the server's is `serve`, not
`start`, so the guide's console recipe for seeing startup errors becomes
`sqi-server.exe service stop`, `Set-Location C:\ProgramData\sqi`, then
`sqi-server.exe serve --config C:\ProgramData\sqi\sqi-server.yaml` (stop the
service first, or the two fight over the same ports and database).

**Prepare `C:\ProgramData\sqi` first**, exactly as in
[step 2 of the worker guide](worker-deployment.md#2-create-the-directory-administrators-only):
created by you, as administrator, administrators-only, with the owner checked.
The server's configuration, database and JetStream data end up there, and a user
who created it first can edit the configuration a LocalSystem service runs with,
and read or replace its database. Then, from an **elevated** PowerShell (every
`service` subcommand needs one, `status` included):

```powershell
& "C:\Program Files\sqi\sqi-server.exe" config print > C:\ProgramData\sqi\sqi-server.yaml
# edit the file, then:
& "C:\Program Files\sqi\sqi-server.exe" service install --start
sqi-server service status   # a few seconds later
```

`config print` writes the values in effect in the shell that ran it, `SQI_*`
variables included, so run it from a shell without any you do not want kept.
There is no separate migration step: the server applies pending migrations
when it starts. The service reports *Running* before the server has read its
configuration, so `service install --start` (and `service start`) watches it for
3 s after that: a configuration the server rejects stops it within that window
and is reported as a failed start, with the last lines of the log. A failure
that comes later, or in a restart the recovery actions make, shows only in
`service status` (it shows `exit codes: 1066 (service-specific 1)` after an
error) and the log, so check both a few seconds after starting. A failed service
is restarted after 5 s (then 30 s, then 60 s), and in that window
`service status` can show `running` or `start pending` for the restarted
process, so check again after the restart delay.

`service install` needs the configuration file to exist. It defaults to
`C:\ProgramData\sqi\sqi-server.yaml`; pass `--config` (`-c`) for another. A
service you register by hand must pass `--config` too: without it the server
refuses to start (`running as a Windows service requires --config`), because the
configuration search it would fall back to includes folders any local user can
create — see
[registering the service by hand](worker-deployment.md#registering-the-service-by-hand).
`service install` registers a service named `sqi-server` (display name
*sqi Server*; `--name` and `--display-name` change them) that:

- runs `sqi-server.exe serve --config <absolute path of the config file>`, as
  LocalSystem unless you pass `--user DOMAIN\name` (see
  [choosing the account](worker-deployment.md#choosing-the-account) for the
  logon right and the password check `--user` brings);
- starts automatically at boot and is restarted after a failure (5 s, 30 s, then
  60 s);
- is given 45 s to shut down when Windows reboots: the server's own 30 s
  shutdown bound (`server.ShutdownTimeout`, not configurable) plus 15 s;
- runs **in the configuration file's directory**, so the default relative
  `store.sqlite_path` (`sqi.db`) and `nats.embedded.data_dir` (`data\nats`) live beside the
  config — `C:\ProgramData\sqi` by default. Set absolute paths, for example on a
  data volume, if you would rather keep them elsewhere. A `--user` account
  cannot read the administrators-only directory: give the server its own, as
  [choosing the account](worker-deployment.md#choosing-the-account) does for a
  worker, with write access to the database and JetStream directories;
- logs to `C:\ProgramData\sqi\logs\sqi-server.log` unless `log.file` is set
  (see [Writing to a file](#writing-to-a-file)).

```powershell
sqi-server service status     # state, exit codes, account, start type, command line, log path
sqi-server service stop       # runs the graceful shutdown, and waits for the service to stop
sqi-server service start
sqi-server service uninstall  # stops it, then removes it; config, database and logs are kept
```

`Get-Service sqi-server`, `Start-Service`, `Stop-Service` and `services.msc` work
too. To upgrade, `service stop`, take a backup, replace `sqi-server.exe`, and
`service start`: see [Upgrade](#upgrade).
A service that stopped because of an error shows
`exit codes: 1066 (service-specific 1)` in `service status`, and its reason is
the last `"service exited with error"` line in the log — or, when the log
directory could not be used, in `sqi-server.trace.log` beside the configuration
file (the fallback is described under
[Logs](worker-deployment.md#logs) in the worker guide). Both are best effort.

> **`migrate`, `backup`, `restore`, `store copy` and `worker` run from a
> console use the *shell's* working directory, not the service's.** With the
> default relative `store.sqlite_path` and `nats.embedded.data_dir`,
> `sqi-server migrate up --config C:\ProgramData\sqi\sqi-server.yaml` looks for
> `sqi.db` in the directory the shell is in — an elevated PowerShell usually starts in
> `C:\Windows\System32` — and would *create* a new, empty database there, while
> `backup` and `worker` would report `no database at …`. `restore` would put the
> backup in `C:\Windows\System32` and leave the server's own database and NATS
> directory untouched. Either set absolute paths in the configuration file, or
> `Set-Location C:\ProgramData\sqi` first.

The server's logs can carry job and environment detail, and every sqi service on
a host shares the one `C:\ProgramData\sqi\logs` directory by default. Any service
installed with `--user` — a worker on the same host, say — is always granted
access to that directory, whatever its own `log.file` is. If the server shares a
host with one, give the server a `log.file` outside that directory, in one only
it and administrators can read **and** write (for example
`log.file: "C:\\ProgramData\\sqi\\sqi-server.log"`), restart the server
(`service stop`, then `service start`: while it runs it holds
`logs\sqi-server.log` open, and Windows will not let the file be moved or
deleted), then move or delete that `sqi-server.log` and its `.1` to `.N` backups
out of `logs\` (the account's permission already reaches those files), and read the
[security notes](worker-deployment.md#security-notes-and-known-limitations) for
what that costs. `service stop`, `start` and `uninstall` act on any service name
you give `--name`, like `sc.exe`.

---

## Upgrade

Schema migrations are forward-only. A new `sqi-server` applies its migrations
when it starts, and from then on an older binary refuses the database (see
[Rollback](#rollback)), so the only way back is the backup you take first.
Upgrading a single server is a stop and a start, not a rolling restart: two
servers cannot share one database (see
[One server per database](#one-server-per-database)), and on one host they
cannot share its ports either. A high-availability group is upgraded as
[Upgrading a group](#upgrading-a-group) describes.

Workers do not need to stop: they reconnect when the server is back. On a
current worker, a report it could not deliver while the server was down is
recovered by [reconciliation](#reconciliation), at the cost of running that
task again. Workers from v0.3.0 and earlier send no attempt list and are
never reconciled: a task whose report such a worker lost stays `running` until
that worker goes offline.

Upgrade the server and its workers together. The server discards the
messages of a worker whose wire protocol does not match its own, registration
included, logging `scheduler: discarding worker message — wire protocol
version mismatch`. A worker that relies on mDNS connects only to a server
that advertises its broker in a `nats_url` record, and skips an advertisement
without one; a worker with an explicit `nats.url` does not use discovery, but
is still subject to the protocol check.

After any server start, worker-offline detection waits about 3 minutes (at
the defaults) before acting, so a worker that died while the server was down
is marked offline, and its tasks reclaimed, that much later. See
[After a broker outage](architecture.md#after-a-broker-outage-the-reconnect-grace).

With an external broker (`nats.mode: external`) the cluster keeps
running while the server is stopped, workers stay connected to it, and
messages wait in its streams; the steps are the same. With broker
authentication on, a worker that reconnects to the cluster while the server
is stopped is refused, because nobody answers the auth callout, and retries
for 2 minutes before it exits, so keep the server's downtime under that or
let the workers' supervisor restart them. Upgrading the cluster itself is
separate; see [Upgrading the cluster](#upgrading-the-cluster).

Run the CLI commands as the account the server runs as (`sqi` in the systemd
unit above). On SQLite the files they create are readable by their owner only,
so a database or lock file created by `root` is one a server running as `sqi`
cannot open. On PostgreSQL, give that account the password the way the server
gets it (the configured DSN, or its `~/.pgpass`): `sudo` does not pass
`PGPASSWORD` through.

### Steps (Linux, systemd)

```sh
# 0. While the old server still runs, check that the new binary loads your
#    configuration (see below). It prints nothing here, and fails naming the
#    line and the value it cannot read.
sudo -u sqi ./sqi-server config print --config /etc/sqi/sqi-server.yaml > /dev/null

# 1. Stop the server.
sudo systemctl stop sqi-server

# 2. Back the database up with the binary still installed. This is the backup
#    a rollback restores. Name the database file with --db (see below).
sudo -u sqi sqi-server backup --db /var/lib/sqi/sqi.db \
  --out /backups/sqi/pre-upgrade-$(date +%Y%m%d-%H%M%S).db
ls -l /var/lib/sqi/sqi.db /backups/sqi/pre-upgrade-*.db   # check it (see below)

# 3. Keep the old binary, and install the new one.
sudo cp /usr/local/bin/sqi-server /usr/local/bin/sqi-server.old
sudo install -m 0755 ./sqi-server /usr/local/bin/sqi-server

# 4. Start it. Pending migrations run before it accepts requests.
sudo systemctl start sqi-server
```

**Check the configuration with the new binary first (step 0).** The server
refuses to start on a malformed duration in the config file, such as
`job_retention: 7d` (durations have no `d` unit: write `168h`); v0.3.0 and
earlier ignored such a value and used the default. Step 0 finds it while the
old server still runs, instead of in step 4, mid-upgrade.

**Name the database with `--db` in step 2.** The backup is taken by the binary
you are upgrading from, and the `backup` of v0.3.0 and earlier reads only
`--db`: it accepts `--config` but ignores it, and otherwise uses
`$SQI_SQLITE_PATH` (which `sudo` drops) or `sqi.db` in the current directory.
Without `--db`, in a directory with no database it creates an empty `sqi.db`
and fails (`open source database: ... no such table: farms`); in a directory
that holds some other sqi database, it backs that one up without complaint. So
check the result: the backup should be close to the live file's size (`VACUUM
INTO` compacts it, so it can be smaller), and any sqi database, even one with
no jobs, is several hundred kilobytes (385 KB for an empty farm at schema 39).
The new binary's `sqi-server migrate status --db <backup>` checks it without
changing it: on SQLite, `migrate status` only reads the database. Do not run
the *old* binary's `migrate status` on it: in v0.3.0 and earlier it opens the
file read-write and rewrites it. On PostgreSQL the old binary already supports
PostgreSQL, and its `backup` reads `--config` and `--dsn`, so
`sqi-server backup --config /etc/sqi/sqi-server.yaml --out
/backups/sqi/pre-upgrade-<time>.dump` works.

To watch the migrations run, or to run them before the start, put this between
steps 3 and 4. It prints one line per migration it applies (or `no pending
migrations`), and it takes the [single-server lock](#one-server-per-database),
so it refuses to run while a server uses the database:

```sh
sudo -u sqi sqi-server migrate up --config /etc/sqi/sqi-server.yaml
```

Take the backup with the server stopped, not online, so that it is exactly the
database the old binary last wrote and a rollback loses nothing.

### Steps (Windows service)

From an elevated PowerShell. `Set-Location` matters with the default relative
`store.sqlite_path`: see the note under [Windows service](#windows-service).

```powershell
# While the old server still runs, check that the new binary loads the
# configuration (it fails naming the line and the value it cannot read):
.\sqi-server.exe config print --config C:\ProgramData\sqi\sqi-server.yaml > $null
sqi-server service stop
Set-Location C:\ProgramData\sqi
sqi-server backup --db C:\ProgramData\sqi\sqi.db `
  --out "D:\backups\sqi\pre-upgrade-$(Get-Date -Format yyyyMMdd-HHmmss).db"
Get-ChildItem C:\ProgramData\sqi\sqi.db, D:\backups\sqi\pre-upgrade-*.db   # compare sizes
Copy-Item "C:\Program Files\sqi\sqi-server.exe" "C:\Program Files\sqi\sqi-server.exe.old"
# Replace C:\Program Files\sqi\sqi-server.exe with the new binary, then:
sqi-server service start
```

The backup directory must already exist; `backup` creates only the file.
`--db` names the database for the same reason as on Linux.

The upgrade steps above are not executed by a test; the [rollback](#rollback)
is.

---

## Rollback

There is no `migrate down`. The database records the newest migration applied
to it, and a binary whose newest migration is older refuses to start on it:

```
startup failed: open store: sqlite: database schema is version 39, newer than this sqi-server (38); this binary is older than the data: run the newer binary, or roll back by restoring the pre-upgrade backup (see docs/operations.md, Rollback)
```

The first number is the database's schema version, the second the newest
migration this binary carries (on PostgreSQL the prefix is `postgres:`).
Nothing was changed: the binary stopped before touching the data. `migrate
up`, `restore` (given a backup newer than the binary), `store copy` and the
`worker` credential commands refuse the same database with the same message.
`backup` does not: it never opens the store, so it can back up a database
newer than itself.

To go back to the older release, restore the backup taken before the upgrade,
with the **older** binary. Do not start a server until the last step:

```sh
sudo systemctl stop sqi-server

# Keep what the newer release wrote, in case you go forward again later.
# (This is still the newer binary, whose backup reads --config.)
sudo -u sqi sqi-server backup --config /etc/sqi/sqi-server.yaml \
  --out /backups/sqi/before-rollback-$(date +%Y%m%d-%H%M%S).db

# Put the old binary back.
sudo install -m 0755 /usr/local/bin/sqi-server.old /usr/local/bin/sqi-server

# PostgreSQL only: restore loads into an empty database. Keep the newer one
# aside (or dropdb it) and create an empty one, as under "Restoring PostgreSQL".
psql -h db.example.com -U postgres -d postgres \
  -c 'ALTER DATABASE sqi RENAME TO sqi_pre_rollback_20261013'
createdb -h db.example.com -U postgres -O sqi sqi

# Restore the pre-upgrade backup with the old binary.
sudo -u sqi sqi-server restore --config /etc/sqi/sqi-server.yaml \
  /backups/sqi/pre-upgrade-20261013-020000.db   # .dump on PostgreSQL

sudo systemctl start sqi-server
```

On SQLite there is nothing to prepare: `restore` moves the newer database
aside itself. On Windows, the same with `service stop`, `Set-Location
C:\ProgramData\sqi`, the backup, `sqi-server.exe.old` copied back over
`sqi-server.exe`, (on PostgreSQL) the empty database, `restore`, and
`service start`. Everything the farm did after the
pre-upgrade backup is gone, and the tasks that backup caught in flight run
again; see [What runs again](#what-runs-again). Executed by
`TestUpgrade_OlderBinaryRefused`, which stands in for the newer binary by
recording a schema version above its own.

The embedded broker's JetStream directory may not be readable by the older
binary: nats-server does not promise that an older release reads the file
store a newer one wrote. `restore` moves `nats.embedded.data_dir` aside, so
the older server starts with an empty one.

### Rolling back to v0.3.0 or earlier

v0.3.0 and earlier have neither the `restore` command nor the schema version
check: they start on a newer database without complaint and run against a
schema they do not know. They support only SQLite and only the embedded
broker, so put the pre-upgrade backup in place by hand, with the server
stopped. This procedure is not executed by a test.

Their configuration file is the one they had: v0.3.0 reads the embedded
broker's `addr`, `data_dir`, `max_store_mb` and `tls` directly under `nats`
and ignores `nats.embedded`, so put back the file you upgraded from, or the
older release silently runs on its defaults.

```sh
sudo systemctl stop sqi-server
# store.sqlite_path is /var/lib/sqi/sqi.db and the JetStream directory
# /var/lib/sqi/nats here; every step runs as the service account, which owns
# that directory.
stamp=$(date -u +%Y%m%dT%H%M%SZ)
for f in /var/lib/sqi/sqi.db /var/lib/sqi/sqi.db-wal /var/lib/sqi/sqi.db-shm /var/lib/sqi/nats; do
  if sudo -u sqi test -e "$f"; then sudo -u sqi mv "$f" "$f.pre-restore-$stamp"; fi
done
sudo -u sqi cp /backups/sqi/pre-upgrade-20261013-020000.db /var/lib/sqi/sqi.db
sudo install -m 0755 /usr/local/bin/sqi-server.old /usr/local/bin/sqi-server
```

Then stop every worker before you start the old server. Without `restore`
nothing closes the attempts the backup recorded as open (tasks assigned or
running), and v0.3.0 has no [reconciliation](#reconciliation): a task whose
worker stays online stays `running`. With the workers stopped, start the server and wait until the web
UI or `GET /api/v1/workers` shows them offline (about
`scheduler.heartbeat_timeout`, 30 s by default); taking them offline returns
their tasks to `ready`. Then start the workers again.

---

## Backup

`sqi-server backup` makes a consistent copy of the state store while the
server keeps running, on both backends. Workers do not need to pause. It
backs up the database only, with either broker mode: it never connects to
the broker, embedded or external; [What a backup leaves
out](#what-a-backup-leaves-out) says why that is enough.

```sh
# The store the configuration names (store.driver, store.sqlite_path or
# store.postgres.dsn):
sqi-server backup --config /etc/sqi/sqi-server.yaml \
  --out /backups/sqi/sqi-$(date +%Y%m%d-%H%M%S).db

# Or name the store explicitly:
sqi-server backup --db /data/sqi.db --out /backups/sqi/sqi-$(date +%Y%m%d-%H%M%S).db
sqi-server backup --dsn "postgres://sqi@db.example.com:5432/sqi?sslmode=verify-full" \
  --out /backups/sqi/sqi-$(date +%Y%m%d-%H%M%S).dump
```

```powershell
Set-Location C:\ProgramData\sqi
sqi-server backup --config C:\ProgramData\sqi\sqi-server.yaml `
  --out "D:\backups\sqi\sqi-$(Get-Date -Format yyyyMMdd-HHmmss).db"
```

The store is chosen as described under
[`store.driver`](configuration.md#storedriver): `--dsn` selects PostgreSQL,
`--db` a SQLite file, and otherwise the configured driver decides. For SQLite
the path then resolves through the same config layer the server uses (the root
`-c/--config` file and `SQI_STORE_SQLITE_PATH`, i.e. `store.sqlite_path`),
falling back to the legacy `SQI_SQLITE_PATH` environment variable and then to
`sqi.db` in the working directory.

The file given to `--out` must not exist: use a timestamped name, as above.
The backup holds password, API-key and session-token hashes, so the command
creates the file itself, readable and writable by its owner only (mode `0600`
on Linux and macOS; on Windows an ACL granting only the owner, `SYSTEM` and
`Administrators`), before any data is written, and removes it if the backup
fails. It exits non-zero if the source database does not exist or has no sqi
schema; it never creates one. It takes no lock and does not check the schema
version, so it also backs up a database a newer `sqi-server` migrated: the
backup to take before a [rollback](#rollback).

### Backing up SQLite

The backup is SQLite's `VACUUM INTO`: one fully checkpointed, self-contained
`.db` file, taken from a snapshot while the server keeps writing. The command
opens the source without applying migrations (SQLite still opens it
read-write and may create `-wal`/`-shm` sidecar files). Restore it with
[`sqi-server restore`](#restore), not by copying it over the live database.

### Backing up PostgreSQL

`sqi-server backup` on PostgreSQL runs `pg_dump`, which takes one consistent
snapshot while the server keeps running:

```sh
sqi-server backup \
  --dsn "postgres://sqi@db.example.com:5432/sqi?sslmode=verify-full" \
  --out /backups/sqi/sqi-$(date +%Y%m%d-%H%M%S).dump
```

- **`pg_dump` must be on `PATH`** where the command runs, at least the
  server's major version; an older one is refused with both versions named. It
  should be the **same** major version as the server: a newer `pg_dump` writes
  settings into the archive (`SET transaction_timeout`, for one) that an older
  server rejects when the archive is restored into it. The `sqi-server`
  container image does not include `pg_dump`, so run the backup from a host
  that has the PostgreSQL client tools.
- The archive is `pg_dump --format=custom --no-owner --no-password`. It is
  created owner-only, as for SQLite, and removed if `pg_dump` fails.
- **The password never reaches `pg_dump`'s command line**, which any local
  user can read from the process list. `pg_dump` gets the DSN with the password
  removed, and the password, as the driver resolves it (from the DSN,
  `PGPASSWORD` or a password file), in `PGPASSWORD` in its environment. For the
  same reason the command refuses a DSN that sets `sslpassword` (a client key's
  passphrase), and refuses to run while `PGSSLPASSWORD` is set: `pg_dump` reads
  the passphrase from no environment variable, only from its command line. Use
  an unencrypted client key for backups. It also refuses a URL DSN whose
  password it cannot locate reliably (see
  [`store.postgres.dsn`](configuration.md#storepostgresdsn)).
- **Settings only the Go driver understands break `pg_dump`.** The DSN is
  handed to `pg_dump`, which reads it with libpq. Keys that `pgx` accepts and
  libpq does not (`statement_cache_capacity`, `default_query_exec_mode`, the
  `database` alias for `dbname`, or a server runtime parameter given as a bare
  key) make `pg_dump` fail with a libpq error such as `invalid connection
  option`. Back up with a DSN that leaves them out.

### Automated daily backup (cron)

```cron
0 2 * * * sqi /usr/local/bin/sqi-server backup \
  --db /data/sqi.db \
  --out /backups/sqi/sqi-$(date +\%Y\%m\%d).db \
  && find /backups/sqi -name "*.db" -mtime +30 -delete
```

This keeps 30 days of daily backups and rotates older files automatically.

### What a backup leaves out

- **The broker's JetStream state** — the embedded broker's
  `nats.embedded.data_dir`, or sqi's streams on an external cluster. It holds
  only messages the server has not finished with: status reports, log chunks and worker
  registrations in transit (the log stream also keeps chunks for 96 h after
  they are stored, but nothing reads them back). Everything durable is in the
  database. A JetStream directory is also never correct beside a database from
  another moment: an older one replays reports for attempts the database has
  already closed or never saw, a newer one holds reports the database will
  never get. So it is not backed up; `restore` moves the directory aside, or
  purges the streams on an external cluster, and makes the database safe
  without it (see [Restore](#restore)). An external cluster's own
  configuration and the seeds `sqi-server nats external init` wrote are yours
  to back up.
- **Files.** The configuration file, TLS material (`sqi-server tls init`
  output), worker credential files on the workers, and log files are yours to
  back up.

The backup half of every runbook here is executed by the restore tests named
under [Restore](#restore); `make smoke` runs `sqi-server backup` and
`sqi-server restore` against a SQLite farm.

---

## Restore

`sqi-server restore BACKUP` puts a backup made by `sqi-server backup` in
place of the state store, with the server stopped, and makes the farm safe to
resume from it. In order, it:

1. takes the [single-server lock](#one-server-per-database), and refuses if a
   server (or another `restore`, `store copy` or `migrate up`) holds it;
2. checks the backup: an intact sqi backup no newer than this binary. SQLite:
   the backup is first copied to `<store.sqlite_path>.restore-tmp`, and that
   copy is checked (`PRAGMA integrity_check` and the schema version), so the
   backup file itself is never opened. PostgreSQL: `pg_restore` is checked
   against the server's version, then the archive's schema version is read
   with `pg_restore` without connecting to any database;
3. puts it in place. SQLite: the live database and whichever of its `-wal`
   and `-shm` files exist are renamed to `<name>.pre-restore-<UTC time>`, and
   the checked copy is renamed into place (a missing live database is fine:
   restoring onto a new host). PostgreSQL: the configured database must be
   empty, and the archive is loaded with `pg_restore --no-owner --no-acl
   --single-transaction --exit-on-error`, so a failed load leaves it empty;
4. applies any newer migrations, as `serve` would;
5. **fences**: every attempt the restored database still records as open
   (its task assigned or running) is closed as failed (`worker shut down`) and
   its task goes back to `ready`, without using a retry or counting toward the
   job's failure limit;
6. clears the broker's messages, which belong to a different moment than the
   restored database. Embedded broker: renames `nats.embedded.data_dir` to
   `<dir>.pre-restore-<UTC time>` (the same time as step 3). External broker:
   purges sqi's streams and deletes the server's durable consumers; see
   [Restoring with an external broker](#restoring-with-an-external-broker).

Nothing is deleted. Each step must succeed before the next, and a failure says
what state it left (see [When restore stops partway](#when-restore-stops-partway)).

**Which store and which NATS directory.** `restore` chooses the store as
`backup` does (`--db`, `--dsn`, `store.driver`; see [Backup](#backup)) and
reads `nats.mode` and `nats.embedded.data_dir` from the same configuration the
server reads (`--config`, `SQI_*` variables); `--nats-data-dir` overrides the
directory. Relative paths resolve from the current directory, as the server
resolves them, so run it from the directory the server runs in, with the
server's configuration. If the configuration cannot be loaded, `restore`
refuses rather than fall back to the built-in defaults, unless you name both
the store (`--db` or `--dsn`) and `--nats-data-dir`; an empty
`--nats-data-dir` is refused too. With `SQI_NATS_MODE=external` set, an
unloadable configuration is refused outright, since the defaults are an
embedded broker's and would skip the purge the cluster needs.

### Restoring SQLite

```sh
sudo systemctl stop sqi-server
sudo -u sqi sqi-server restore --config /etc/sqi/sqi-server.yaml \
  /backups/sqi/sqi-20261013-020000.db
sudo systemctl start sqi-server
```

```powershell
sqi-server service stop
Set-Location C:\ProgramData\sqi
sqi-server restore --config C:\ProgramData\sqi\sqi-server.yaml D:\backups\sqi\sqi-20261013-020000.db
sqi-server service start
```

Run it as the account the server runs as. The restored file is created
owner-only, so one restored by `root` cannot be opened by a server running as
`sqi`. On Windows the file grants its owner, `SYSTEM` and `Administrators`,
which a LocalSystem service can read; a service installed with `--user` needs
access granted to its account
(`icacls C:\ProgramData\sqi\sqi.db /grant "DOMAIN\name:M"`).

`restore` prints what it did:

```
restored a schema-39 backup; schema is now 39
previous database file moved to /var/lib/sqi/sqi.db.pre-restore-20261013T143216Z
fenced 1 attempt(s): their tasks are ready again
NATS data directory moved to /var/lib/sqi/nats.pre-restore-20261013T143216Z
```

There is one `previous database file` line for each file moved: a server that
did not stop cleanly, or a `store copy`, can also leave `sqi.db-wal` and
`sqi.db-shm`, which move with it. `no NATS data directory at <dir>; nothing to
move` means there was none at that path: if you expected one, check that
`nats.embedded.data_dir` and the directory you ran from are the server's.

`restore` copies only the backup's database file, so it refuses a backup with
a non-empty `-wal` file beside it (a live database's file, copied by hand): the
writes in that `-wal` would be lost, and the copy would still pass the
integrity check. Stop the server using it cleanly, which folds the `-wal` in,
or restore a backup made by `sqi-server backup`, which never has one. An empty
`-wal` is fine.

Executed by `TestBackupRestore_FarmResumes`, which backs up a farm while tasks
run, lets it move on, restores, and checks that no attempt is left open, that
every task runs to the end, and that a ghost run's late report changes
nothing; and by the restore round trip in `make smoke`.

### Restoring PostgreSQL

`restore` loads the archive only into an **empty** database (no tables at all
in its search path, sqi's or anyone's). sqi never drops a database: that needs
privileges it may not have, and it is your decision. Keep the current database
under another name, or drop it, then create an empty one owned by the role in
the DSN:

```sh
sudo systemctl stop sqi-server

# Keep the current database aside...
psql -h db.example.com -U postgres -d postgres \
  -c 'ALTER DATABASE sqi RENAME TO sqi_pre_restore_20261013'
# ...or discard it:
#   dropdb -h db.example.com -U postgres sqi
createdb -h db.example.com -U postgres -O sqi sqi

sudo -u sqi sqi-server restore --config /etc/sqi/sqi-server.yaml \
  /backups/sqi/sqi-20261013-030000.dump
sudo systemctl start sqi-server
```

```
restored a schema-39 backup; schema is now 39
fenced 0 attempt(s): their tasks are ready again
NATS data directory moved to /var/lib/sqi/nats.pre-restore-20261013T143307Z
```

A database that is not empty is refused before anything is loaded:

```
Error: restore: restore needs an empty database, and this one has 21 tables (api_keys, ...); drop and recreate it, then run restore again
```

The check reads the system catalog (`pg_class`), so it counts every table in
the search path, including ones the role has no privilege on.

- `pg_restore` must be on `PATH`, at least the server's major version, as
  `pg_dump` must be for [backup](#backing-up-postgresql); the password reaches
  it the same way, never on its command line. A missing `pg_restore`, or one
  older than the server, is reported as such before the archive is read. One
  older than the `pg_dump` that wrote the archive cannot read its format, and
  is reported as such when it first reads it, still before anything is
  loaded.
- If the load fails, the error shows `pg_restore`'s own messages but not the
  server's detail lines, which can quote rows of the database (and so
  password, API-key and session hashes); the count of lines left out is
  shown instead. To see them, run `pg_restore` by hand.
- **Grants are not restored** (`--no-acl`), just as ownership is not: a
  `GRANT` the source database gave another role (a reporting role, say) is
  left out, so a role the new cluster lacks cannot fail the load. Re-apply such
  grants after the restore.
- **Run `restore` on the server's host**, with the server's configuration, so
  that it moves the right `nats.embedded.data_dir`. If you run it from another
  host (one with the PostgreSQL client tools, say), move the server's
  `nats.embedded.data_dir` aside by hand before you start the server, or run
  `sqi-server restore --in-place` on the server's host afterwards. With an
  external broker any host that can reach the cluster with the server's
  configuration will do.

Executed by `TestPostgres_RestoreRoundTrip` and, on PostgreSQL 16 and 18, by
`TestBackupRestore_FarmResumes` in `make test-postgres`.

### Restoring in Docker Compose

The `sqi-server` image does not include `pg_restore`, so with
[`deploy/docker-compose.postgres.yml`](https://github.com/uberware/sqi/blob/main/deploy/docker-compose.postgres.yml)
`restore` runs in a one-off container of the `postgres` service, with the
server's binary and its data volume mounted into it. The file's header has the
commands; they are not executed by a test.

### Restoring with an external broker

With `nats.mode: external`, the broker's messages live on the cluster, so step
6 purges them instead of moving a directory: `restore` empties sqi's four
streams (`SQI_TASK`, `SQI_LOGS`, `SQI_WORKER`, `SQI_CANCEL`) and deletes the
server's three durable consumers (`sqi-task-status-srv`, `sqi-task-logs-srv`,
`sqi-worker-srv`), connecting with the server's own `nats.external`
configuration (its data credential and TLS settings). The streams keep their
configuration, replica count included. The steps are those above:

```sh
sudo systemctl stop sqi-server
sudo -u sqi sqi-server restore --config /etc/sqi/sqi-server.yaml \
  /backups/sqi/sqi-20261013-020000.db
sudo systemctl start sqi-server
```

```
restored a schema-39 backup; schema is now 39
previous database file moved to /var/lib/sqi/sqi.db.pre-restore-20261013T143216Z
fenced 1 attempt(s): their tasks are ready again
purged sqi's JetStream state on the external broker: its streams are empty and the server's durable consumers deleted
```

Before it changes anything, `restore` takes the account's
[guard](#one-server-per-broker-account) (unless given `--skip-nats-purge`,
below), and holds it until it finishes, so
no `sqi-server` can start on the account while the database is replaced and
the streams are purged; a server started meanwhile is refused naming the
restore. It therefore refuses, with nothing changed:

- while a server runs on the account:
  `restore changed nothing: bus: another sqi-server holds this broker account: host …, pid …, started … (instance …); it is still running, or exited less than 30s ago; stop that sqi-server before restoring`.
  A server that has just stopped uncleanly holds it for up to 30 s more;
- when it cannot reach the cluster, or the cluster refuses the data
  credential, within `nats.external.connect_timeout`.

If the cluster cannot be reached and the restore cannot wait, pass
`--skip-nats-purge`: `restore` does not connect to the broker at all, so it
takes no guard and nothing stops a server starting on the account meanwhile;
it puts the database in place and fences it, and
then prints what to purge by hand, with the `nats` CLI commands to do it as
sqi's data user (credentials in a URL are printed as `REDACTED`). Do that
before you start a server, or it replays messages from before the restore:

```
sqi's JetStream state on the external broker was not purged (--skip-nats-purge). Before starting a server, in account SQI:
  purge the streams SQI_TASK, SQI_LOGS, SQI_WORKER, SQI_CANCEL
  delete the durable consumers sqi-task-status-srv (stream SQI_TASK), sqi-task-logs-srv (stream SQI_LOGS), sqi-worker-srv (stream SQI_WORKER)
for example, with the nats CLI as sqi's data user:
  nats stream purge SQI_TASK -f --server tls://nats-a:4222,tls://nats-b:4222,tls://nats-c:4222 --nkey /etc/sqi/nats/sqi-data.nk --tlsca /etc/sqi/tls/ca.crt
  …
  nats consumer rm SQI_WORKER sqi-worker-srv -f --server … --nkey … --tlsca …
```

`--skip-nats-purge` is refused with an embedded broker, whose directory is
moved aside rather than purged, and `--nats-data-dir` is refused with an
external one. `--in-place` works the same way with an external broker: it
fences the database in place and purges the cluster.

Executed by `TestNATSCluster_RestorePurges` in `make test-nats-cluster`,
against a real three-node cluster.

### After point-in-time recovery or a snapshot: `--in-place`

A database put back by other means (PostgreSQL point-in-time recovery, a
volume or filesystem snapshot, a `pg_restore` of your own) has the same
problem a backup has: it records as open the attempts that were in flight
(assigned or running) at that moment. Before you start a server on it, run:

```sh
sudo -u sqi sqi-server restore --in-place --config /etc/sqi/sqi-server.yaml
```

```
fenced 1 attempt(s): their tasks are ready again
NATS data directory moved to /var/lib/sqi/nats.pre-restore-20261013T143953Z
```

`--in-place` takes the lock, migrates, fences and moves the NATS directory
aside or purges the external broker (steps 1 and 4 to 6 above); it loads
nothing. It refuses a SQLite path
with no database, and a database on either backend with no sqi schema (an
empty file, or one sqi never migrated), and checks this before it changes
anything.

On SQLite it also refuses a database whose `-wal` file is not empty. Nothing
ties a `-wal` to the database it was written for, and SQLite applies whatever
`-wal` it finds, so a database copied by hand over one whose server stopped
uncleanly would take that server's writes. If that is what happened, move the
old `-wal` and `-shm` aside together with the old database, or use
`sqi-server restore BACKUP`. If the `-wal` is this database's own (its server
crashed, or a snapshot took them together), run `sqi-server migrate up` on it,
which folds the `-wal` in, then run `--in-place` again. Do not start a server
on it instead: that would fold the `-wal` in too, but the server would run
unfenced. A cleanly stopped server leaves no `-wal`, or an empty one;
`--in-place` refuses any non-empty `-wal`, even one holding nothing the
database file lacks.

### What runs again

The restored farm is the farm at the moment of the backup:

- **Work done after the backup runs again.** A task that finished after the
  backup is back as the backup recorded it, and runs again. A task the backup
  caught in flight (assigned or running) is handed back by the fence and runs
  again, on any worker.
- **Ghost runs are ignored.** A worker that is still running a fenced task
  when the server comes back reports into an attempt that is now closed; its
  status reports are discarded and its log output stays on that closed
  attempt. Reports about attempts the backup never saw are discarded too. The
  server logs these, which is expected after a restore; for example, at Warn,
  `scheduler: task status rejected by state machine — discarding`,
  `scheduler: task.status for unknown attempt — discarding` and
  `scheduler: task.logs for unknown attempt — discarding`, and at Info,
  `scheduler: stale running report — discarding` and
  `scheduler: stale failure report — discarding`.
- **A ghost run occupies its worker until it is canceled.** The fence closes
  the attempt in the database, not the process. Once the restored server has
  run for [`scheduler.reconcile_grace`](configuration.md#schedulerreconcile_grace),
  it sends a cancel to each worker whose heartbeats still list a fenced
  attempt, or an attempt the backup never recorded (see
  [Reconciliation](#reconciliation)). Until the cancel lands, the
  scheduler counts a worker's busy cores from the database, so it can
  lease that worker more work than it has cores for, and the task's re-run on
  another worker can write the same outputs at the same time. Where that
  matters (long tasks, outputs written in place), stop the workers that were
  running tasks before you start the restored server, and start them again
  after: stopping `sqi-worker` ends its tasks once
  `worker.shutdown_grace_period` runs out.
- **Everything written after the backup is gone**: jobs submitted since, and
  accounts, API keys, sessions, products and worker broker credentials created
  since. With broker authentication on, a worker enrolled after the backup
  must enroll again.
- Jobs the retention sweep deleted after the backup reappear, and are deleted
  again when they qualify.
- A log client already paging a fenced attempt can miss a ghost run's late
  chunks: their `after_nats_seq` cursor values restart below the ones it has
  read (see [Tail task logs](api.md#tail-task-logs-polling)).

### Cleaning up

`restore` keeps everything it replaced. Once the farm is healthy, remove the
copies; the old database holds password and token hashes, so do not leave it
lying about:

```sh
sudo rm /var/lib/sqi/sqi.db*.pre-restore-*
sudo rm -r /var/lib/sqi/nats.pre-restore-*
dropdb -h db.example.com -U postgres sqi_pre_restore_20261013   # PostgreSQL, if you kept it
```

```powershell
Remove-Item C:\ProgramData\sqi\sqi.db*.pre-restore-*
Remove-Item -Recurse C:\ProgramData\sqi\data\nats.pre-restore-*
```

### When restore stops partway

- **Before anything moved** (the lock is held, the backup is not an sqi
  backup, is damaged or is newer than the binary, the PostgreSQL database is
  not empty, or `<sqi.db>.pre-restore-<time>` already exists): nothing
  changed. Fix the cause and run it again. An existing `.pre-restore-` name is
  refused rather than overwritten.
- **After the live SQLite database moved aside**: the error lists every file
  it moved. The database file moves first, then `-wal`, then `-shm`, so a
  failure moving a sidecar (including a `.pre-restore-` name that already
  exists for it) comes after the database itself has moved. A database and its
  `-wal`/`-shm` files are one database only together, so to go back, rename
  **all** the moved files back (no restored `sqi.db` is in place yet in that
  case; if one is, remove it first). If only the final rename failed, the
  checked copy of the backup is still at `<store.sqlite_path>.restore-tmp`.
- **After the backup was in place but before the fence finished**: the
  message ends `the database in place is not fenced, so do not start a server
  on it: fix the cause and run "sqi-server restore --in-place" to finish`.
- **After the fence, when the NATS directory could not be moved**: the
  database is restored and fenced; move the directory aside by hand, or run
  `sqi-server restore --in-place`, before starting a server.
- **After the fence, when the external broker could not be purged**: the
  database is restored and fenced, and the message says so. Run
  `sqi-server restore --in-place` once the broker is reachable, or purge by
  hand (`sqi-server restore --in-place --skip-nats-purge` prints what),
  before starting a server.

---

## Reconciliation

A server can believe a task is in flight (assigned or running) on a live
worker that no longer holds it: its completion report was lost with a
JetStream directory, the database was restored by hand, or the worker dropped
the task. The worker is healthy, so the offline sweep never reclaims it.

A worker lists, in every heartbeat, the attempts it holds, from the moment
it receives them until it has sent their final status and cleaned up their
session. On every
heartbeat-sweep tick the server reclaims an open attempt when the worker's
latest heartbeat is current, does not list it, and has not listed it for
[`scheduler.reconcile_grace`](configuration.md#schedulerreconcile_grace)
(measured from the later of the attempt's creation and the last heartbeat
that listed it). Reclaiming is what a worker shutdown does: the task returns to
`ready`, the attempt closes as failed (`worker shut down`), and no retry or
failure is counted. The server logs a Warn, `scheduler: reclaimed an attempt
its worker no longer reports`, with the worker, task, attempt and how long it
was open, and counts it in `sqi_scheduler_tasks_reconciled_total`.

The same tick does the reverse. When a worker's current heartbeat lists an
attempt the store has already closed (the task was canceled, reclaimed or
fenced by a restore, and the worker missed the cancel), the server sends that
worker a cancel naming the attempt, on every tick while the worker lists it.
It logs each at Info, `scheduler: cancel signal published to worker` with
`cause` `closed attempt still reported`. An attempt the store has no record of
(a database restored from a backup taken before its lease, or a job deleted
while it ran) is sent a cancel naming the attempt alone, with `cause`
`unknown attempt still reported`. Each is counted in
`sqi_scheduler_cancels_resent_total`, by `cause` (`closed` or `unknown`). Only
a heartbeat received after the store closed the attempt counts, so heartbeats
from while the attempt ran send nothing. A worker still lists a finished
attempt while it cleans up its session, so a heartbeat sent during that
cleanup can trigger one; that cancel names an attempt the worker no longer
runs and is ignored. A worker that is stopping a canceled run lists it, and is
sent the cancel again, until the process has exited and its session is cleaned
up. The counter therefore rises a little with ordinary cancels and
completions. A sustained rate while no job was canceled, no database restored
and no task reclaimed means workers are missing cancels.

- **On by default.** Left unset, `reconcile_grace` is 2 minutes, or
  `scheduler.heartbeat_timeout` when that is longer. `0` turns
  reconciliation off; any other value below `heartbeat_timeout` is refused at
  start-up.
- **Nothing is reclaimed during the first `reconcile_grace` after the server
  starts**, while heartbeats rebuild what each worker holds and any queued
  reports are processed.
- **The cost of a mistake is a re-run, never a lost result.** If the server
  processes status reports more than `reconcile_grace` behind (a badly
  overloaded server), a task that has finished can be reclaimed before its
  report is read; the late report is then discarded as belonging to a closed
  attempt, and the task runs again. If
  `sqi_scheduler_tasks_reconciled_total` rises while no worker was lost or
  restored, raise `reconcile_grace`.
- **Heartbeats with a skewed clock are ignored.** A heartbeat whose worker
  timestamp is more than `heartbeat_timeout` from the moment it arrives does
  not count (the server warns once until it recovers: `scheduler: worker
  heartbeat delayed or skewed`), so that worker's tasks are not reconciled
  until its clock or its delivery recovers. Keep worker clocks synchronized.
  A skewed clock does not take a worker offline: liveness is timed by the
  broker's timestamp for each heartbeat, not by the worker's.
- **A heartbeat listing more than 4,096 attempts is ignored.** No worker can
  hold that many (leasing is gated on cores), so the server treats the list as
  broken and logs a Warn, `scheduler: worker heartbeat lists more attempts
  than a worker can hold`. The worker stays online, but its tasks are not
  reconciled until it sends a shorter list.
- **Workers from v0.3.0 and earlier are never reconciled.** They send no
  attempt list, and are covered only by the offline sweep and by `restore`'s
  fence. A current worker holding nothing sends an empty list, which counts.

Executed by `TestRestore_MixedAgeNATSDir` (a database restored by hand beside a
newer NATS directory is recovered by reconciliation, and a replayed older one
duplicates no log row), for the late-report case by
`TestReconcileAttempts_LateTerminalReportIsDiscarded`, and, for the reverse
repair, by `TestReconcileAttempts_CancelsAReportedClosedAttempt`,
`TestReconcileAttempts_CancelsAReportedUnknownAttempt` and
`TestCancelResentToAWorkerHoldingAClosedAttempt`.

JetStream can deliver a log chunk to the server more than once (after a crash,
or when an acknowledgement is lost; not after a clean stop). Each chunk is
stored once, identified by its attempt and the worker's sequence number.

---

## One server per database

Only one `sqi-server` may use a database at a time, on either backend, unless
every server using it runs with
[high availability](#running-more-than-one-server-high-availability) on: then
any number of them share a PostgreSQL database, and one of them leads. The
same guard keeps the commands that rewrite the database (`migrate up`,
`restore`, `store copy`) from running beside any server, with high
availability or without:

| Takes the lock | Does not |
|---|---|
| `serve`, `migrate up`, `restore`, `store copy` (both its SQLite source and its PostgreSQL target) | `migrate status`, `backup`, `worker token issue`, `worker enroll`, `worker revoke`, `worker list`, `nats external init` |

The commands in the right-hand column work while a server runs.

A consequence for upgrades and supervisors of a single server: **no
overlapping restarts.** Stop the old server before starting the new one. A
supervisor that starts the replacement first (a rolling update with surge)
sees it refused until the old one exits. `systemctl restart` stops first and
is fine. A high-availability group rolls a release that keeps the schema, and
stops every server for one that changes it (see
[Upgrading a group](#upgrading-a-group)).

### SQLite: the lock file

The server takes an exclusive operating-system lock (`flock` on Linux and
macOS, `LockFileEx` on Windows) on `<database>.lock`, for example
`/var/lib/sqi/sqi.db.lock`, created owner-only beside the database. The path
is the database file's own, with symlinks resolved, so every symlinked name
for one database locks the same file. A hard link is the exception: the filesystem cannot tell it from the original, so it
gets a lock file of its own. A second server, or one of the commands above,
fails at once:

```
startup failed: open store: another sqi-server is using sqi.db (lock file /var/lib/sqi/sqi.db.lock is held); stop it first: fslock: lock is held
```

The lock, not the file, is the guard. The operating system drops it when the
process exits however it exits, so a crashed server leaves nothing stale, and
the file stays on disk. **Never delete it**: that frees nothing, and a
process opening it at that moment would lock a file the next one cannot see. To
find the holder on Linux, `sudo fuser -v /var/lib/sqi/sqi.db.lock`.

SQLite on a network filesystem is not supported, and the lock is only as good
as that filesystem's locking (NFS and SMB emulate it with varying fidelity).

### PostgreSQL: the advisory lock

At start-up a server takes the PostgreSQL advisory lock `0x7371692d73727672`
and holds it until it stops. A server without high availability takes it
exclusive, and a second server started on the same database exits at once,
logging `sqi-server exited with error` with this error:

```
startup failed: open store: another sqi-server, or a command that needs the database alone (migrate up, restore, store copy), is already using this database (PostgreSQL advisory lock 0x7371692d73727672 is held); stop it first. Servers can share a database only when high availability is enabled on all of them
```

Another server, or one that has not finished stopping, holds the lock on this
database. Every connection a server opens carries `application_name`
`sqi-server`, unless the DSN sets its own. To find the holder whatever it is
called, look for the connection holding the
advisory lock (its key's two 32-bit halves are `1936812333` and
`1936881266`) on the database in question:

```sql
SELECT a.pid, a.application_name, a.client_addr, a.backend_start
FROM   pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
WHERE  l.locktype = 'advisory' AND l.granted
  AND  l.classid = 1936812333 AND l.objid = 1936881266
  AND  l.database = (SELECT oid FROM pg_database WHERE datname = 'sqi');
```

**The lock goes with its connection.** If the connection holding it drops
(a network failure, the PostgreSQL server restarting, an administrator
terminating the backend), the server logs `store: lost the PostgreSQL server
lock's connection; another sqi-server could now start on this database` at
Error within about 30–35 s (it pings that connection every 30 s, with a 5 s
timeout), and keeps running. Nothing then stops a second
server starting on the same database. Restart the server to take the lock
again.

A server with `ha.enabled` takes the same lock shared, and the servers of
a group hold it together. Shared and exclusive exclude each other, so a
server with high availability and one without refuse each other in either
order, and `migrate up`, `restore` and `store copy`, which take it exclusive,
refuse to run beside any server of a group. An HA server asks for the lock
exclusive first: granted, it means no other server is running, so the server
applies pending migrations and then trades the exclusive lock for the shared
one, taking the shared lock before it drops the exclusive one. Otherwise it
joins the holders of the shared lock; if a session holds the lock exclusive
(a server that is migrating, or one of the commands), it keeps asking for up
to 30 s before it fails with the error above. A server that would need to
migrate the database refuses to join servers already using it:

```
postgres: schema is at version 41 and this sqi-server needs 42, but 2 other sqi-server(s) are using this database; stop them all to migrate, or run `sqi-server migrate up` once they are stopped
```

An HA server's sessions also set `idle_in_transaction_session_timeout` to a
third of `ha.lease_ttl` and `client_connection_check_interval` to 1 s, so
PostgreSQL ends the open transaction of a server that has frozen (a paused
VM) and releases its row locks, instead of leaving the server that takes over
waiting on it. A server without high availability sets neither.

When a group's lock connection is lost, each server logs the Error above and
keeps running, as a single server does, and that server no longer holds its
share: with every server's lost (PostgreSQL restarted, say), nothing stops a
server without high availability, or one of the commands, from taking the
database beside the group. Restart the servers, standbys first and the leader
last, to take the lock again.

Advisory locks belong to one database, so servers on different databases of
the same PostgreSQL cluster do not collide.

---

## One server per broker account

With an external broker, a broker account belongs to one database. A single
server has it to itself; the servers of a
[high-availability group](#running-more-than-one-server-high-availability)
share it, because they share the database, and only their leader works on it.
A server on any other database is refused. Two servers working one account
would both answer every lease and split the status messages between their
databases, losing work, and two servers with two databases never meet at
either database's lock, so this guard lives on the broker. It exists only in
external mode: an embedded broker cannot be shared.

The guard is the key `active` in the KV bucket `SQI_SERVER` in the account. A
single server takes it at startup, before it touches anything else there, and
refreshes it every 10 s. `sqi-server restore` takes the same key for as long
as it runs (except with `--skip-nats-purge`, which does not connect to the
broker). A second server, or a server started during a restore, exits at
once:

```
startup failed: start bus: bus: another sqi-server holds this broker account: host render-db-01, pid 4242, started 2026-11-02T09:14:03Z (instance 3f9c2a7d1e5b8046); it is still running, or exited less than 30s ago
```

The holder is named, so the other server can be found. A server that stops
cleanly deletes its key; one that crashed leaves it for at most 30 s, so a
supervisor restarting the server sees this refusal for up to 30 s and then
gets through. As with the database lock, do not overlap restarts of a single
server, and never point a copied configuration (a staging server, say) at a
live account.

If a running single server loses the key (its refreshes failed for more than
30 s during a partition and another server took the account), it logs
`bus: this sqi-server no longer holds the broker account; another sqi-server
took it` at Error and reports degraded on `/readyz` until it is restarted. It does
not stop on its own: stop one of the two. A server whose refreshes fail but
whose key nobody took keeps running, reports degraded while the last refresh
is more than 30 s old, and retakes the key with a Warn once it can.

In a high-availability group the leader holds the key, taking it as it
promotes itself, and the key's holder says which database's group the account
belongs to:

- A standby checks the key before it reports ready, and the server that
  promotes itself checks it again as it takes it. A key held by a member of
  the same database (the leader, or a leader that crashed) is overwritten at
  once on promotion, so a crashed leader's key costs a failover nothing.
- A key held by a server that is not a member of this database is watched
  for one guard TTL, 30 s. If its holder refreshes it in that time, the server
  refuses to start (or, at promotion, stops leading):

  ```
  startup failed: bus: another sqi-server holds this broker account: host render-db-01, pid 4242, started 2026-11-02T09:14:03Z (instance 3f9c2a7d1e5b8046); it is still running, or exited less than 30s ago and is not a member of this database: is this server pointed at the wrong database?
  ```

  If it is not refreshed (the last single server of a farm moving to high
  availability, or a restore that has finished), it is taken. So the first
  server of a new group can take up to 30 s longer to lead, and a server
  pointed at the wrong database is refused up to 30 s later than a single
  server would be.
- A leader whose key another server takes stops leading and exits with code
  75 (`guard_lost`).

See [The one-server-per-account guard](architecture.md#the-one-server-per-account-guard).

---

## Running on PostgreSQL

`sqi-server` keeps its state in embedded SQLite by default. Set
[`store.driver`](configuration.md#storedriver) to `postgres` to keep it in an
external PostgreSQL server instead. Everything else — the API, the scheduler,
workers, NATS — is the same on both. How the two stores differ inside is
described under [Two backends](architecture.md#two-backends).

**PostgreSQL 16 or later is required.** CI runs the PostgreSQL-backed test
packages and the integration suite against 16 and 18.

### Docker Compose

[`deploy/docker-compose.postgres.yml`](https://github.com/uberware/sqi/blob/main/deploy/docker-compose.postgres.yml)
runs a PostgreSQL 18 container beside the server and a worker, with the
server's store on it:

```sh
export SQI_POSTGRES_PASSWORD='choose-a-strong-password'
docker compose -f deploy/docker-compose.postgres.yml up -d --build
```

The password is required. It sets the database role's password and reaches
the server as `PGPASSWORD`, so the DSN in the file carries none. The server
creates the schema on first start. `--build` builds the images from your
checkout, since PostgreSQL support is not yet in a published release (the
latest is `v0.3.0`); once a release includes it, pin that tag in the file and
drop `--build`. The file's header has the commands to back the database up
from inside its container and to restore it with `sqi-server restore` (see
[Restoring in Docker Compose](#restoring-in-docker-compose)). The rest of this
section covers an external PostgreSQL server you run yourself.

### Creating the database

Create an empty database and a role for `sqi-server`. The role runs the
migrations, so it must be able to create tables, indexes and a domain in the
database's `public` schema. The simplest way is to make it the database's
owner (from PostgreSQL 15 on, an ordinary role does not have `CREATE` on
`public` by default):

```sql
CREATE ROLE sqi LOGIN PASSWORD 'change-me';
CREATE DATABASE sqi OWNER sqi;
```

A role that does not own the database needs `CREATE` on its `public` schema
instead (`GRANT CREATE ON SCHEMA public TO sqi;`, run in that database).

Any locale works. The schema declares every text column `COLLATE "C"`, so
ordering is byte order, as on SQLite, whatever the database's own collation,
and no `--locale=C` is needed. Use the `UTF8` encoding (the usual default):
job templates, names and task logs are UTF-8 text, and a narrower encoding
cannot hold every character they may contain.

### Configuring sqi-server

```yaml
store:
  driver: postgres
  postgres:
    dsn: "postgres://sqi@db.example.com:5432/sqi?sslmode=verify-full"
    max_conns: 16
```

The DSN may be a URL or a keyword/value string; see
[`store.postgres.dsn`](configuration.md#storepostgresdsn) for both forms, how
the password is redacted, and the characters to percent-encode. Set `sslmode`
explicitly, `verify-full` where the server has a certificate the host can
verify. The password can stay out of the file: set the whole DSN in
`SQI_STORE_POSTGRES_DSN`, or leave the password out of the DSN and supply it
through `PGPASSWORD` or a password file (`~/.pgpass`), which the driver reads
as any libpq client does.

`store.sqlite_path` and `store.checkpoint_interval` do nothing on PostgreSQL,
and the server warns at start-up about either one set explicitly.

The server creates the schema itself on its first start, as it does on SQLite.
To create it beforehand, or to see each migration as it is applied, run:

```sh
sqi-server migrate up --dsn "postgres://sqi@db.example.com:5432/sqi?sslmode=verify-full"
```

`migrate`, `backup`, `restore` and `worker` all take `--dsn`, which overrides
`store.postgres.dsn` (`store copy` takes `--to`); run with the server's
`--config` instead, they use the configured driver and DSN. `backup` and
`worker` never create a schema:
against an empty database they fail with `the database at <dsn> has no
schema; run "sqi-server migrate up" first`.

Leave the password out of `--dsn`: a command line is visible to any local user
in the process list for as long as the command runs. Supply it through
`PGPASSWORD`, a password file (`~/.pgpass`, or
`%APPDATA%\postgresql\pgpass.conf` on Windows), or the config file's
`store.postgres.dsn` instead; the driver reads the first two when the DSN
carries no password.

The PostgreSQL migrations start at `00037_baseline.sql`, which creates the
whole schema. As on SQLite, they are forward-only (see [Rollback](#rollback)).

Once the server is up, `/readyz` reports the store's check as `postgres`
(`{"status":"ok","checks":{"nats":{"status":"ok"},"postgres":{"status":"ok"}}}`), where a SQLite
server reports `sqlite`.

**Connections.** The [advisory lock](#postgresql-the-advisory-lock) holds one
connection of the write pool for the server's life, so
`store.postgres.max_conns` (default 16) leaves `max_conns - 1` for writes. The
server also opens a read pool of 4, so it can use up to `max_conns + 4`
connections. Each `migrate`, `backup`, `restore`, `store copy` or `worker`
command opens a few more while it runs.

---

## Moving to PostgreSQL

`sqi-server store copy` moves a farm from its SQLite file into an empty
PostgreSQL database: every row of every table, verbatim, in one PostgreSQL
transaction, with each table verified by row count and digest before it
commits. It runs with the server stopped, and it never writes to the SQLite
file, which stays as it was.

**PostgreSQL → SQLite is not supported.** Once the farm has run on
PostgreSQL, the way back to an earlier state is `backup` and `restore` on
PostgreSQL.

### Steps (Linux, systemd)

1. Create an empty database and a role for the server, as in [Creating the
   database](#creating-the-database).
2. Stop the server, and bring the SQLite file to this binary's schema (`store
   copy` refuses a file at an older version, and names this command):

   ```sh
   sudo systemctl stop sqi-server
   sudo -u sqi sqi-server migrate up --db /var/lib/sqi/sqi.db
   ```

3. In the configuration, set `store.driver: postgres` and
   [`store.postgres.dsn`](configuration.md#storepostgresdsn). Leave
   `store.sqlite_path` set for now: `store copy` reads its source from it. Keep
   the `nats` block as it is.

   ```yaml
   nats:
     embedded:
       data_dir: "/var/lib/sqi/nats"   # unchanged
   store:
     driver: postgres
     sqlite_path: "/var/lib/sqi/sqi.db"   # the copy's source; remove it afterwards
     postgres:
       dsn: "postgres://sqi@db.example.com:5432/sqi?sslmode=verify-full"
   ```

4. Copy. With the configuration above it needs no flags; `--from` (a SQLite
   file) and `--to` (a DSN) override it. Keep the password off the command
   line: put it in the configured DSN, or in the `sqi` account's password file
   (`~sqi/.pgpass`), since `sudo` does not pass `PGPASSWORD` through:

   ```sh
   sudo -u sqi sqi-server store copy --config /etc/sqi/sqi-server.yaml
   ```

   Each table is logged to stderr as it is copied; stdout gets the summary:

   ```
   audit_log              0 rows
   compute_locations      0 rows
   farms                  1 rows
   products               0 rows
   queues                 1 rows
   jobs                   1 rows
   job_dependencies       0 rows
   steps                  1 rows
   storage_locations      0 rows
   tasks                  8 rows
   task_attempts          9 rows
   task_logs              8 rows
   usage_pools            0 rows
   usage_claims           0 rows
   users                  0 rows
   api_keys               0 rows
   sessions               0 rows
   worker_credentials     0 rows
   worker_join_tokens     0 rows
   workers                1 rows
   copied and verified 30 rows in 145ms; set store.driver: postgres and start the server
   ```

5. Start the server. `/readyz` now reports the store's check as `postgres`.
   Remove `store.sqlite_path` from the configuration (the server warns that it
   is ignored), and keep the SQLite file until you no longer want the option
   of going back to it.

   ```sh
   sudo systemctl start sqi-server
   ```

**If `nats.embedded.data_dir` changes** (the server moves to another host or
into a container, or you start it with an empty directory), run `sqi-server
restore --in-place` with the new configuration before starting it. The copy is exact,
so a server that keeps its NATS directory continues every task that was in
flight; one that does not would wait for reports that are not there.
`--in-place` fences those tasks so they run again (see [After point-in-time
recovery or a snapshot](#after-point-in-time-recovery-or-a-snapshot---in-place)).

**Going back to SQLite** before anything has run on PostgreSQL is setting
`store.driver: sqlite` again: the SQLite file was never written. Once the farm
has run on PostgreSQL, that work exists only there.

On Windows, the same steps from an elevated PowerShell:

```powershell
sqi-server service stop
Set-Location C:\ProgramData\sqi
sqi-server migrate up --db C:\ProgramData\sqi\sqi.db
# Edit C:\ProgramData\sqi\sqi-server.yaml as in step 3, then:
$env:PGPASSWORD = '<the sqi role password>'
sqi-server store copy --config C:\ProgramData\sqi\sqi-server.yaml
Remove-Item Env:PGPASSWORD
sqi-server service start
```

Executed by `TestStoreCopy_SQLiteToPostgres`, which copies a farm with rows in
every table, compares the two databases and finishes its jobs on PostgreSQL.

### What `store copy` checks

Everything is checked before a row moves, and each refusal names its fix:

- **The source.** It takes the SQLite lock file, so it refuses while a server
  uses the file. It opens the file read-only, which may leave an empty `-wal`
  and a `-shm` index beside it; neither holds data. The file must pass SQLite's
  integrity check and be at this binary's schema version. A file that needs
  recovery a read-only reader cannot do (a crashed server's hot journal, or a
  `-wal` whose directory the command cannot write the `-shm` index into) is
  refused: start `sqi-server` on it once and stop it cleanly, or copy from a
  `sqi-server backup` of it.
- **The target.** It takes the PostgreSQL advisory lock, so it refuses while a
  server uses the database. An empty database is migrated by the command; one
  already migrated to this version must have no rows in any table; anything
  else is refused, as is a database holding tables sqi does not own:

  ```
  Error: store copy: the PostgreSQL database is not empty (farms has 1 rows); use an empty database
  ```

- **The shape.** Both databases must have the same tables with the same
  columns; any difference is named.

During the copy, a value PostgreSQL will not take (text that is not valid
UTF-8 or holds a NUL byte, a timestamp not in the stored form, a row whose
foreign key points at a missing row) aborts it, naming the table, the row's
primary key and, where it can, the column. **A failure commits no sqi rows.**
A schema the command migrated into an empty database stays, still empty, so
`store copy` can be run again once the cause is fixed; a migration that itself
failed partway needs the database dropped and created again.

**How long it takes.** As a sizing guide, on a 12-vCPU host with PostgreSQL
on tmpfs: about 5 s for 100,000 tasks (a 25 MB SQLite file) and 47 s for
1,000,000 tasks (245 MB), all of it one PostgreSQL transaction. The server
stays stopped for that long.

---

## Running on an external NATS cluster

`sqi-server` runs its own NATS broker by default. Set
[`nats.mode`](configuration.md#natsmode) to `external` to connect it to a
NATS server or cluster you run instead. The cluster then outlives
`sqi-server` restarts, workers connect to it rather than to `sqi-server`, and
with [`stream_replicas: 3`](configuration.md#natsexternalstream_replicas) on
three nodes, losing one node loses no message. The API, the scheduler,
enrollment, revocation and the workers' credentials are the same in both
modes. How the two topologies differ is described under
[Broker topologies](architecture.md#broker-topologies).

**sqi needs a dedicated account on the cluster.** `sqi-server`'s data
connection may publish and subscribe to every subject in its account, which is
safe only because nothing else lives there. Do not share the account with
another application.

What the cluster must provide:

- a [supported nats-server release](#supported-nats-server-versions) on
  every node;
- JetStream, enabled on sqi's account;
- with broker authentication on, sqi's accounts and auth-callout settings, in
  config-file accounts as `sqi-server nats external init` generates them. A
  cluster in NATS operator (JWT) mode can host sqi only with broker
  authentication off.

An external cluster is not, on its own, high availability for `sqi-server`:
without `ha.enabled` one server runs per account (see
[One server per broker account](#one-server-per-broker-account)), and while
it is stopped no work is leased. With it, several servers share the account
and a standby takes over (see
[Running more than one server](#running-more-than-one-server-high-availability)).

### Supported nats-server versions

| Release series | Minimum |
|---|---|
| 2.14 | 2.14.8 |
| 2.15 | 2.15.1 |
| 2.16 and later | any |

Each minimum is the first release of its series with upstream's fixes to the
auth callout, which sqi's worker authentication depends on. Series before
2.14 are refused. The external-mode tests run against 2.14.9 and against
2.15.2, the release `sqi-server` embeds. `sqi-server` reads the version from
the node it first reaches and refuses an older one at startup:

```
bus: nats-server 2.14.5 is older than the minimum 2.14.8 for its release series; upgrade the broker to a release still receiving security fixes (broker at tls://nats-a:4222)
```

A reconnect that lands on an older node later, mid-upgrade or mid-downgrade,
logs `bus: reconnected to a nats-server below the supported version` at Warn
rather than stopping the server. Keep every node on a supported release.

### Generating the cluster configuration

```sh
sqi-server nats external init --out /etc/sqi/nats
```

`--account` names the account (default `SQI`; it must match
[`nats.external.account`](configuration.md#natsexternalaccount)). The command
writes five files:

| File | What it is | Where it goes |
|---|---|---|
| `issuer.nk` | account nkey seed sqi signs worker authorizations with | `sqi-server` host only |
| `sqi-data.nk` | user nkey seed of `sqi-server`'s data connection (sqi's account) | `sqi-server` host only |
| `sqi-auth.nk` | user nkey seed of `sqi-server`'s callout connection (`AUTH` account) | `sqi-server` host only |
| `sqi-sys.nk` | user nkey seed of a system-account user, which lets `sqi-server` disconnect a revoked worker at once | `sqi-server` host only |
| `sqi-accounts.conf` | nats-server configuration: the accounts and the `auth_callout` block | every cluster node |

The four seeds are private keys, written owner-only (`0600`); they never
leave the `sqi-server` host, and the cluster never needs them.
`sqi-accounts.conf` holds only public keys and goes to every node. The
command refuses to replace files that already exist unless given `--force`,
because a regenerated key does not match the fragment the cluster already
loaded. It then prints the `include` line for the nodes and the `nats:` block
for `sqi-server.yaml`, with these files' paths filled in.

sqi's own users are nkey users because a cluster sends the nonce workers sign
only when it has nkey users of its own, so the fragment is also what makes
worker authentication possible.

### Configuring the cluster

Copy `sqi-accounts.conf` into the directory of every node's config file, and
include it from that config:

```
include "sqi-accounts.conf"
```

nats-server resolves an `include` path relative to the including file's
directory, even a path that looks absolute, so the fragment must sit beside
each node's config. The fragment defines sqi's account (with JetStream), the
`AUTH` and `SYS` accounts, `system_account` and the `authorization {
auth_callout }` block; the node's own config supplies the rest — listeners,
TLS, clustering and the JetStream `store_dir` — and must not define
`accounts`, `system_account` or `authorization` of its own: nats-server does
not merge a second `accounts` block.

The `auth_callout` block applies to the whole server: every connection by a
user not in its `auth_users` list is sent to `sqi-server`, which admits only
enrolled sqi workers. Any other user on the cluster must be added to
`auth_users`, or it is refused. Do not add a `no_auth_user` either: with the
callout it would let anonymous clients in.

The fragment is for broker authentication on. To run external mode with
`nats.auth.enabled: false`, replace its `authorization` block with
`no_auth_user` naming the data user's public key (the `nkey` in sqi's
account), so anonymous connections land in sqi's account. Any host that can
reach the cluster can then register as a worker, and `sqi-server` warns
about it at every start.

[`deploy/nats-cluster/nats.conf`](https://github.com/uberware/sqi/blob/main/deploy/nats-cluster/nats.conf)
is a working node config to start from.

### Configuring sqi-server

Put the printed block in `sqi-server.yaml`, with your nodes' URLs:

```yaml
nats:
  mode: external
  auth:
    enabled: true
  external:
    urls:
      - "tls://nats-a.example.internal:4222"
      - "tls://nats-b.example.internal:4222"
      - "tls://nats-c.example.internal:4222"
    account: "SQI"
    nkey_seed_file: "/etc/sqi/nats/sqi-data.nk"
    stream_replicas: 3
    tls:
      ca_file: "/etc/sqi/tls/ca.crt"
    callout:
      nkey_seed_file: "/etc/sqi/nats/sqi-auth.nk"
      issuer_seed_file: "/etc/sqi/nats/issuer.nk"
      system_creds_file: "/etc/sqi/nats/sqi-sys.nk"
```

Every key is described under
[`nats.external.*`](configuration.md#natsexternalurls). At startup
`sqi-server` dials the URLs until
[`connect_timeout`](configuration.md#natsexternalconnect_timeout) (30 s), checks
the broker's version, requires JetStream on the account, takes the
[account guard](#one-server-per-broker-account), creates or updates its
streams, and, with authentication on, starts answering the callout. It logs
`bus: connected to external nats` with the URL, version, cluster, account,
replica count and whether revocation is immediate. After startup it
reconnects without limit, and `/readyz` reports degraded while the data or
callout connection is down.

**List the nodes of one cluster.** Every `urls` entry must name a node of
the same NATS cluster. The server records the broker it runs against by the
cluster name of the node it connected to (`external:<account>@<cluster>`),
and treats a change of that name as a broker switch, which
[fences every open attempt](#switching-broker-mode). URLs that span a
gateway supercluster, two clusters joined by gateways, can land on a node of
the other cluster at the next start, so the identity flips between restarts
and each flip fails the farm's running work back to `ready`.

**Keep the seed files owner-only.** The data, callout, issuer and system
seed files are private keys. At every start `sqi-server` logs
`bus: an external broker seed file is readable beyond its owner` for each one
other accounts can read, naming the file and the fix (`chmod 600 <file>` on
Linux and macOS), and starts anyway: the files are yours to manage, and a
secrets mount may set their mode. `sqi-server nats external init` writes
them owner-only.

**Keep `system_creds_file`.** Without it, `sqi-server` cannot close a revoked
worker's connection on the cluster: revocation takes effect when the worker's
authorization expires, within
[`nats.auth.user_ttl`](configuration.md#natsauthuser_ttl), and the server
warns about that at every start. See
[Revocation](auth.md#revocation-two-paths-deliberately-distinct).

**Discovery.** mDNS advertises the first
[`advertise_urls`](configuration.md#natsexternaladvertise_urls) entry, else
the first `urls` entry, so a discovering worker reaches one node. Set
`advertise_urls` when workers must use other addresses than `sqi-server` does,
and when `urls` carry credentials (`user:password@` or `token@`): mDNS
publishes the advertised URL in plaintext, so the server refuses to start
when that URL carries any.

### Configuring workers

Workers connect to the cluster with the same credential they would use on an
embedded broker, and enroll the same way. List every node, comma-separated, in
the worker's `nats.url`, so it can move to another node when one is lost:

```yaml
nats:
  url: "tls://nats-a.example.internal:4222,tls://nats-b.example.internal:4222,tls://nats-c.example.internal:4222"
  tls_ca_file: "/etc/sqi/tls/ca.crt"
  server_url: "https://sqi-server.example.internal:8080"   # enrollment, over REST
```

A worker that connects while `sqi-server` is not running (with broker
authentication on) is refused, because nothing answers the callout. It
retries for 2 minutes before it exits, so keep server downtime short or let
the worker's supervisor restart it.

### The reference Compose cluster

[`deploy/docker-compose.nats-cluster.yml`](https://github.com/uberware/sqi/blob/main/deploy/docker-compose.nats-cluster.yml)
runs three nodes from `deploy/nats-cluster/nats.conf`, with JetStream, TLS on
the client port and the fragment from `nats external init`. Its header has the
steps. It secures the client port only: the routes between the nodes are
plaintext and unauthenticated, which holds only because the route port is not
published outside the Compose network. Nodes on separate hosts need TLS (or a
shared `authorization`) on their `cluster` block, with only the nodes able to
reach the route port. The client ports bind `127.0.0.1` unless
`NATS_CLUSTER_BIND` says otherwise, so set it for workers on other hosts.
`make test-nats-cluster` runs the external-mode tests against it.

### Upgrading the cluster

Restart nodes one at a time, and stop each through lame duck mode rather than
a plain stop:

```sh
nats-server --signal ldm=<pid>
```

Lame duck mode closes the node's listener and moves its clients to the other
nodes before it exits. A plain shutdown can crash nats-server 2.15.2 in the
auth callout when a client connects while the node shuts down, and workers
reconnect at any time, at every authorization expiry. `sqi-server`'s embedded
broker always stops through lame duck mode. With `stream_replicas: 3` the streams
stay available while one node is down. Stay within the
[supported versions](#supported-nats-server-versions) on every node.

### Troubleshooting

- **Workers fail with `nats: nkeys not supported by the server`.** The
  cluster sent no nonce, because it has no nkey users: its config does not
  include `sqi-accounts.conf`. `sqi-server` reports the same cause at startup
  (`the cluster issued no nonce; define sqi's own users as nkey users`).
- **`account "SQI" on the external broker has no JetStream`.** Enable
  JetStream on the node (a `jetstream { store_dir }` block) and on the account
  (`jetstream: enabled`, which the fragment has).
- **`the external cluster cannot place sqi's streams yet`** at startup: the
  cluster has fewer JetStream nodes up than `stream_replicas`. `sqi-server`
  keeps trying until `connect_timeout`.
- **`another sqi-server holds this broker account`**: see
  [One server per broker account](#one-server-per-broker-account).
- **`no server answered $SYS.REQ.SERVER.PING.VARZ … the system credential
  (system_creds_file) must be a user in the cluster's system account`** at
  startup: `system_creds_file` is not the seed of a user in the system
  account listed in the callout's `auth_users`, which `sqi-sys.nk` and the
  generated fragment are.

### Switching broker mode

Moving a farm between the embedded broker and an external one, in either
direction, leaves the in-flight messages (status reports, log chunks, cancels
for open attempts) behind in the broker the farm left. `sqi-server` handles
this itself:

1. Stop the server cleanly, so it drains what it can.
2. Change `nats.mode` and the `nats.embedded` or `nats.external` settings,
   and point the workers at the new broker (their `nats.url`, or let
   discovery advertise it).
3. Start the server.

On that first start the server finds that the broker differs from the one
its database last ran against, and fences, as `restore` does: every attempt
the database still records as open (its task assigned or running) closes as
failed, its task returns to `ready`, and no retry is used. It logs
`bus: broker layout changed; open attempts fenced` with the count and both
identities. Those tasks run again; a worker still running one is a ghost run
(see [What runs again](#what-runs-again)). Moving an external farm to another
cluster or account is the same switch.

A database with no broker identity recorded counts as a switch too, so its
open attempts are fenced once at the first start; the server logs
`bus: no broker identity recorded; open attempts fenced` with the count and
the identity it recorded. On a new database there is nothing to fence, and it
logs `bus: broker identity recorded` instead. sqi keeps the embedded broker's
streams in the `SQI` account, never nats-server's global account: if
`<data_dir>/jetstream/$G` exists, the server logs `bus: found JetStream
streams in the global account; they are abandoned and safe to delete` at Info
at every start, with its path, and leaves it there.

---

## Running more than one server (high availability)

With [`ha.enabled`](configuration.md#ha--high-availability), several
`sqi-server` processes share one PostgreSQL database and one account on an
external NATS cluster. One of them is the **leader**: it schedules, reads the
workers' reports, and serves the REST API, the web UI and the WebSocket
gateway. The others are **warm standbys**. When the leader stops, crashes,
freezes, or loses its database or its broker, a standby takes over, in seconds
(see [Failover timing](#failover-timing)). Tasks running on workers are not
touched by a failover: the workers keep running them, and their reports wait
in the broker until the new leader reads them.

High availability is off by default. A server with no `ha` block, or with
`ha.enabled: false`, runs exactly as a single server does.

### Prerequisites

- **PostgreSQL.** Every server of the group uses the same database
  (`store.driver: postgres`). SQLite cannot be shared.
- **An external NATS cluster**, with every server on the same account
  (`nats.mode: external`). Use `stream_replicas: 3` on a three-node cluster, so
  losing a node loses no message (see
  [Running on an external NATS cluster](#running-on-an-external-nats-cluster)).
- **One configuration file** for every server. Only
  [`ha.instance_name`](configuration.md#hainstance_name) differs, and it can
  come from `SQI_HA_INSTANCE_NAME`; give each server its own name when several
  share a host name, as containers started from one image do.
- **A supervisor that restarts a server when it exits**: systemd
  `Restart=on-failure` or `Restart=always`, Compose `restart: unless-stopped`,
  or the Windows service's recovery actions. A leader that loses its leadership
  exits with code `75` (see [Demotion](#demotion-exit-code-75)) and comes back,
  as a standby, only if something starts it again. Its stop timeout must also
  be longer than the server's 30 s shutdown deadline plus up to 7 s of handing
  over (stopping the scheduler, at most 2 s, and releasing the lease, at most
  5 s): the example unit's `TimeoutStopSec=60s` and the Windows service's 45 s
  PreShutdown timeout both are; give Compose a `stop_grace_period` of 45 s or
  more.
- **A load balancer that health-checks `GET /leaderz`**, which answers 200 on
  the leader only. Point clients (browsers, `sqi-cli`, the SDK, and workers
  enrolling with a join token through `nats.server_url`) at the balancer.
  Workers' broker connections go to the NATS cluster, not through it. A
  standby that receives a request (in the moment between a failover and the
  balancer's next check) answers 503 with `Retry-After: 2` and processes
  nothing, and the client retries it. Do not have the balancer retry 503s:
  the leader returns 503 for reasons of its own (an EXPR submission past
  `openjd.expr_submission_deadline`, say), and replaying those repeats the
  most expensive requests. Retrying a connection that could not be made is
  safe; retrying any other failure is safe only for `GET` and `HEAD`. The
  reference HAProxy configuration does exactly that.

`deploy/docker-compose.ha.yml` is a complete example: PostgreSQL, a
three-node NATS cluster, two servers, HAProxy
([`deploy/ha/haproxy.cfg`](https://github.com/uberware/sqi/blob/main/deploy/ha/haproxy.cfg))
and a worker. Its header has the setup steps. Kubernetes leader routing (a
Service that follows the leader) is not provided.

The `ha` part of the configuration is:

```yaml
ha:
  enabled: true
  lease_ttl: 15s          # the default
  # instance_name: sched-a  (or SQI_HA_INSTANCE_NAME)
store:
  driver: postgres
nats:
  mode: external
```

Every HA start logs one Warn,
`ha: a leader that loses its leadership exits with code 75, and stays stopped
unless a supervisor restarts it`, as a reminder of the supervisor requirement.

### Leader and standbys

The leader holds a lease kept in the database (the `server_lease` and
`server_epoch` rows), and every judgement of whether it has lapsed is made on
the database's clock, so servers whose clocks disagree still agree on who
leads. The leader renews it every third of `ha.lease_ttl` (5 s), retrying a
failed renewal every tenth (1.5 s). If no renewal has succeeded within 0.8 ×
`ha.lease_ttl` (12 s) of the start of the last successful one, the leader stops
itself, before a standby can take the lease. A standby tries to take it every
second. Each new leader leads at a higher **epoch**, and every write the
scheduler makes carries it: the database checks it last before committing, so
a write by a server that has been replaced rolls back instead, and that server
exits.

A standby is warm: it has the database open, has recorded itself in the
group's membership, is connected to the broker and has passed the broker
checks a leader needs (the version floor, JetStream on the account, and the
account's [guard](#one-server-per-broker-account)). It answers the auth
callout, so workers can connect while no server leads, and it collects worker
diagnostic logs, so a new leader's Server Log already has the workers'
history. It does not schedule, read worker reports, run sweeps or advertise
over mDNS. A standby starts even while the broker is down; it runs its broker
checks when it first connects and is not ready until they pass. A check that
fails for another reason than a refusal (a broker that is still starting, a
system credential the cluster does not accept) is retried every 2 s, with a
Warn each time.

On becoming leader a standby, in order: takes the broker account's guard,
provisions the streams, applies the
[broker-layout fence](architecture.md#changing-broker-identity-and-fencing)
if the broker changed, seeds the default farm and the first admin, runs the
scheduler, starts the session sweeper and the mDNS responder, and only then
opens its API and `/leaderz`. It logs
`ha: this sqi-server is now the leader` as it begins and
`sqi-server ready as the leader` when it is done. The liveness sweep's
[reconnect grace](architecture.md#after-a-broker-outage-the-reconnect-grace)
counts from the promotion, so a new leader does not mark workers offline
whose heartbeats it has not read yet.

The probes:

| | Leader | Standby | Without HA |
|---|---|---|---|
| `GET /healthz` | 200 | 200 | 200 |
| `GET /readyz` | 200 when its checks pass | 200 when warm and its checks pass | unchanged |
| `GET /leaderz` | 200 | 503 | 404 |
| anything else | served | 503, `Retry-After: 2`, `{"error":"standby","leader":"<name>"}` | served |

Under HA the `/readyz` and `/leaderz` bodies carry `role`, `leader` (the
leader's `ha.instance_name`, empty when none is known) and `epoch`. `/readyz`
says whether the process is healthy, not whether it leads: route traffic on
`/leaderz`. Without HA, `/readyz` is unchanged.

### Demotion: exit code 75

Any loss of authority ends a leader: a renewal finds the lease taken
(`renew_refused`), the self-fence deadline passes (`deadline`), a write is
refused for a stale epoch (`fenced`), another server takes the broker guard
(`guard_lost`), the broker is lost while a standby has it (`broker_lost`), or
a step of promotion fails (`promotion_failed`). The server logs exactly one
Error, `ha: lost leadership`, with the reason; stops its scheduler and broker
connections, each step bounded to 2 s; releases the lease, so a standby takes
over at its next poll; and exits with code 75. Its supervisor starts it
again and it rejoins as a standby. The code is distinct so alerting can tell a
demotion from a crash (exit code 1). As a Windows service the server reports
75 as its service-specific exit code; the service's recovery actions restart
it after 5 s, then 30 s, then 60 s (reset after a day), so a Windows server
demoted repeatedly comes back as a standby more slowly. That does not slow
down another server taking over.

A graceful stop (`SIGTERM`, `systemctl stop`, a service Stop) of the leader is
not a demotion: it answers the workers' waiting lease requests empty, releases
the lease, and exits 0. A standby takes over within about a second.

### Losing the broker

A leader whose own connection to the broker is down steps down only when both
hold: the connection has been down for longer than `ha.lease_ttl`, timed on
its own clock, and another server of the group has reached the broker within
`ha.lease_ttl`. It checks at each renewal, so at the defaults it releases the
lease and steps down about 20–25 s after losing the broker, and the standby
that has it leads about a second later. The old leader's own exit follows a
few seconds after that, since stopping its scheduler and draining a broker
connection that is down each take their full 2 s bound. When the broker is down for every server, the leader keeps
leading and reconnects as a single server would; stepping down then would only
turn a broker outage into a restart loop.

### Failover timing

At the default `lease_ttl` of 15 s:

| Event | Leader gone → new leader acting | Idle workers leased again |
|---|---|---|
| Graceful stop of the leader | about 1 s (one poll) plus the promotion (seconds) | at once: their waiting requests are answered empty as it stops |
| Crash or `kill -9` | up to 15 s (the lease) + 1 s (poll) + promotion, about 20 s | up to 35 s (the workers' lease-request timeout) |
| Leader frozen (a paused VM, `SIGSTOP`) | about 20 s, or about 25 s if it froze between a fence check and its commit (PostgreSQL ends that session after 5 s idle); on resuming it is fenced and exits 75 | up to 35 s |
| Leader loses PostgreSQL | it stops itself within 12 s; a standby that reaches PostgreSQL takes over at about 16 s, or once PostgreSQL is back | up to 35 s |
| Leader loses the broker, a standby has it | it steps down about 20–25 s after the broker went; the standby leads about 1 s later | up to 35 s |
| Broker down for every server | no change of leader | when the broker returns |

Running tasks are untouched in every row. Their reports wait in `SQI_TASK`
(kept 24 h) and heartbeats in `SQI_WORKER` (kept 2 min, longer than every
figure here; a failover longer than that drops queued heartbeats, which the
new leader's reconnect grace covers). A longer `lease_ttl` lengthens the rows
that wait on the lease and tolerates slower database round trips; a shorter
one does the opposite.

### What is lost on failover

- **The old leader's own diagnostic log records.** Every server collects the
  workers' diagnostics, so the Server Log keeps worker history; the old
  leader's own records go with it. The Server Log shows the server answering.
- **Prometheus counters** start again at the new leader, which is a new
  process. Scrape every server, and select the leader with
  `sqi_server_ha_leader == 1`.
- **WebSocket sessions** drop and reconnect through the balancer; the web UI
  fetches its data again when it reconnects.
- **An OIDC login in progress** fails once, because the state key is per
  process. **Retry-wake timers** fall back to the sweep.
- **A worker whose lease request was waiting on a crashed leader** waits out
  its 35 s request timeout before asking again. (A graceful stop answers it
  at once.)

### Seeing the group

- **`GET /api/v1/servers`** (permission `diagnostics.read`; served by the
  leader, like the rest of the API) lists every server that has run against
  the database, leader first, with its role, host, version, start time, how
  long ago it was last seen, and whether it reaches the broker, plus the
  current epoch. A server is `leader` while it holds the unexpired lease,
  `standby` while it checked in within `lease_ttl`, and `missing` otherwise; a
  crashed leader whose lease has lapsed is not shown as leader. Rows unseen
  for a day are deleted by the leader.
- **`sqi-server ha status`** prints the same table. It reads the database
  directly and takes no lock, so it works while every server is down. Run it with the servers' configuration file, so it
  judges the roles against the same `lease_ttl`:

  ```sh
  sqi-server ha status --config /etc/sqi/sqi-server.yaml
  ```

- **The web UI**: Admin → Servers.
- **Metrics**, registered only when HA is on: `sqi_server_ha_leader` (1 on
  the leader), `sqi_server_ha_epoch`, `sqi_server_ha_promotions_total` and
  `sqi_server_ha_demotions_total{reason}`. Alert on demotions and on no server
  reporting `sqi_server_ha_leader == 1`.

### Upgrading a group

- **A release with the same schema** (a patch release) rolls: upgrade and
  restart each standby in turn, then stop the leader; an upgraded standby takes
  over, and the old leader is then upgraded and rejoins. While versions are
  mixed, each standby logs `ha: the leader runs another version of sqi-server
  than this standby` once. A standby that would grant workers different broker
  permissions from the leader's leaves the auth callout's queue group (logging
  `ha: this standby grants workers different broker permissions than the
  leader; it answers no worker connects`) until it leads or they match again,
  so a worker is never admitted with permissions the leader does not expect.
- **A release that changes the schema** needs every server stopped. Back up,
  then run `sqi-server migrate up`, or let the first server to start apply the
  migrations, and start the rest. A server with pending migrations refuses to
  join servers already running:

  ```
  postgres: schema is at version 41 and this sqi-server needs 42, but 2 other sqi-server(s) are using this database; stop them all to migrate, or run `sqi-server migrate up` once they are stopped
  ```

  The [release notes](https://github.com/uberware/sqi/releases) say whether
  a release changes the schema; `sqi-server migrate status` shows what a
  binary would apply.
- **Rollback** is as for a single server: stop every server and
  [restore the pre-upgrade backup](#rollback).

### Moving a single server to HA

Stop the single server first: its database lock and an HA server's exclude
each other. Turn `ha.enabled` on in its configuration (it must already be on
PostgreSQL and an external broker; see
[Moving to PostgreSQL](#moving-to-postgresql) and
[Switching broker mode](#switching-broker-mode)), and start the servers of the
group. The single server's guard key is still on the account and names a
server that is not a member of the group, so the first server to lead watches
it for up to 30 s before taking it.

---

## Worker broker credentials

`sqi-server worker` groups the offline CLI commands for NATS broker
authentication (`nats.auth.*` — see
[Broker authentication](auth.md#broker-authentication-transport) for the
full model). Like `migrate` and `backup`, these subcommands open the state
store directly (the SQLite file, or the PostgreSQL database with `--dsn` or
`store.driver: postgres`) and do not start an HTTP server or NATS broker, so
they work whether or not `sqi-server` is running — and, independently,
whether or not the user-facing `auth.enabled` is on.

> **These commands choose their store the same way `backup` and `migrate`
> do** (see [`store.driver`](configuration.md#storedriver): `--dsn` or
> `store.driver: postgres` selects PostgreSQL). For SQLite the path is an
> explicit `--db`, then `store.sqlite_path` (the root
> `-c/--config` file and `SQI_STORE_SQLITE_PATH`), then the legacy
> `SQI_SQLITE_PATH` environment variable, then `sqi.db`. Unlike `migrate`,
> they never create or migrate the database: pointing one at a path with no
> database there is a clear error naming the resolved path, not a silently
> created empty one. Run `sqi-server migrate up` against the target database
> first if it does not exist yet.

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
with the worker's own ID and public key filled in. A running `sqi-server`
needs no restart: the auth callout reads the credential from the database at
every connect, so the worker is admitted at its next connect.

### Revoke a worker's credential

```sh
sqi-server worker revoke --db /data/sqi.db <worker-id>
```

Revokes a worker credential in the database. It closes no connection: a
running worker is refused at its next connect, within `nats.auth.user_ttl`
(1 hour by default) at the latest. To disconnect a worker at once against a
running server, use `DELETE /api/v1/workers/{id}/credential` instead (see
[Revocation](auth.md#revocation-two-paths-deliberately-distinct)).

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

### Writing to a file

To have the server write its own log file, and rotate it, set `log.file`:

```yaml
log:
  file: "D:\\sqi-logs\\sqi-server.log"  # empty (the default) = stderr
  max_size_mb: 100   # rotate when the file reaches this size
  max_backups: 5     # keep sqi-server.log.1 ... sqi-server.log.5
```

The environment variables are `SQI_LOG_FILE`, `SQI_LOG_MAX_SIZE_MB` and
`SQI_LOG_MAX_BACKUPS`; the reference is
[`log.file`](configuration.md#logfile),
[`log.max_size_mb`](configuration.md#logmax_size_mb) and
[`log.max_backups`](configuration.md#logmax_backups).

- **The directory must already exist.** Startup fails with a `log.file`
  validation error that names it otherwise, and likewise when `log.file` names
  an existing directory or ends with a path separator: it must name a file.
  A relative path resolves against the
  server's working directory (a Windows service runs in the directory that holds
  its config file).
- **Rotation is by size.** At `max_size_mb` the file becomes `<file>.1`, `.1`
  becomes `.2`, and so on up to `max_backups`, and the oldest is dropped.
  `max_backups: 0` keeps none: the file is discarded and started fresh.
- **A blocked rotation loses no lines.** On Windows a rename fails while another
  process, such as `Get-Content -Wait` or a log shipper, holds the file open
  without allowing it to be renamed. The server then keeps appending to the
  current file and tries again after another `max_size_mb` of output, so the file
  can temporarily grow past its limit.
- **A Windows service always logs to a file.** With `log.file` empty it writes
  `C:\ProgramData\sqi\logs\<service-name>.log` (`sqi-server.log` by default),
  creating that directory. See [Windows service](#windows-service).

### Routing logs

When running under systemd, logs flow to journald automatically:

```sh
# Live tail
journalctl -u sqi-server -f

# Last 1000 lines as JSON
journalctl -u sqi-server -n 1000 -o json
```

To write to a file instead, set `log.file` (above), redirect stderr in the
service unit, or use a log-forwarding agent (Fluentd, Vector, Promtail) reading
from journald.

### Log rotation

Unless `log.file` is set, `sqi-server` writes to stderr rather than a file, so
log rotation is handled outside the process. journald and logrotate apply only
in that case; with `log.file` set, the server rotates the file itself, as
described in [Writing to a file](#writing-to-a-file).

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
| `sqi_scheduler_tasks_reconciled_total` | counter | — | Attempts reclaimed because their live worker stopped reporting them (see [Reconciliation](#reconciliation)) |
| `sqi_scheduler_cancels_resent_total` | counter | `cause` | Cancels sent to a live worker still reporting an attempt the store has closed (`closed`) or has no record of (`unknown`) (see [Reconciliation](#reconciliation)) |
| `sqi_server_ha_leader` | gauge | — | 1 while this server leads its high-availability group, 0 otherwise. HA only |
| `sqi_server_ha_epoch` | gauge | — | The leadership epoch this server leads at, or last saw the leader hold. HA only |
| `sqi_server_ha_promotions_total` | counter | — | Promotions of this server to leader. HA only |
| `sqi_server_ha_demotions_total` | counter | `reason` | Losses of leadership by this server, by [reason](#demotion-exit-code-75). HA only |

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
5. Drain and flush sqi's JetStream consumers (in-flight messages are
   acknowledged or requeued), then shut the broker down. The embedded broker
   stops through lame duck mode; with an external broker, `sqi-server`
   releases the [account guard](#one-server-per-broker-account) and closes its
   connections, and the cluster keeps running.
6. SQLite: wait for the final WAL checkpoint, in TRUNCATE mode, to finish.
7. Close the store: both connection pools (write and read) and the
   [single-server lock](#one-server-per-database) (SQLite's lock file,
   PostgreSQL's advisory lock).
8. Exit with code 0.

The server imposes its own **30 s** internal drain deadline
(`server.ShutdownTimeout`, a compile-time constant — not a config key); past
that it logs `graceful shutdown timed out after 30s` and exits. The example
unit's `TimeoutStopSec=60s` is deliberate headroom over that 30 s so systemd
never SIGKILLs mid-drain; raising it further has no effect on how long the
server actually waits.

As a Windows service, a service **Stop** or a shutdown (PreShutdown) request
starts the same sequence. The service is registered with a 45 s PreShutdown
timeout: the 30 s deadline plus 15 s.

In a [high-availability group](#running-more-than-one-server-high-availability)
the leader hands over first: it closes its API and `/leaderz`, stops the
scheduler (answering workers' waiting lease requests empty, at most 2 s) and
releases the lease (at most 5 s), so a standby takes over at its next poll.
That can add up to 7 s to the 30 s deadline, which both the example unit's
`TimeoutStopSec` and the Windows PreShutdown timeout cover.

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

Secrets (the bootstrap admin password, the LDAP bind password, the OIDC client
secret and the PostgreSQL DSN's password) print as placeholders, so the output
is not a working config file until they are put back.

---

## Version information

```sh
sqi-server version
# sqi-server v0.3.0 (commit abc1234, built 2026-07-09, go1.26.3)
```
