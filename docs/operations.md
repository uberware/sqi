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

- `store.sqlite_path` — set to an absolute path on a local SSD, or set
  `store.driver: postgres` and `store.postgres.dsn` to keep the state in
  PostgreSQL instead (see [Running on PostgreSQL](#running-on-postgresql))
- `nats.data_dir` — set to a persistent directory for JetStream storage
- `http.addr` — restrict to `127.0.0.1:8080` if a reverse proxy handles TLS
- `http.tls` / `nats.tls` — terminate TLS in sqi instead, if there is no proxy.
  Both are off by default, so **the API and the worker transport are plaintext
  until you set one or the other**. See [`docs/tls.md`](tls.md); note that
  enabling TLS on a running farm is a coordinated restart, not a rolling one

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
  `store.sqlite_path` (`sqi.db`) and `nats.data_dir` (`data\nats`) live beside the
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
> default relative `store.sqlite_path` and `nats.data_dir`,
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
Upgrading is a stop and a start, not a rolling restart: two servers cannot
share one database (see [One server per database](#one-server-per-database)),
and on one host they cannot share its ports either.

Workers do not need to stop: they reconnect when the server is back. On a
worker of this release, a report it could not deliver while the server was
down is recovered by [reconciliation](#reconciliation), at the cost of running
that task again. Workers from v0.3.0 and earlier send no attempt list and are
never reconciled: a task whose report such a worker lost stays `running` until
that worker goes offline.

Run the CLI commands as the account the server runs as (`sqi` in the systemd
unit above). On SQLite the files they create are readable by their owner only,
so a database or lock file created by `root` is one a server running as `sqi`
cannot open. On PostgreSQL, give that account the password the way the server
gets it (the configured DSN, or its `~/.pgpass`): `sudo` does not pass
`PGPASSWORD` through.

### Steps (Linux, systemd)

```sh
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

**Name the database with `--db` in step 2.** The binary that takes this
backup is the one you are upgrading from, and the `backup` of v0.3.0 and
earlier reads only `--db`, defaulting to `$SQI_SQLITE_PATH` or `sqi.db` in the
current directory: it accepts `--config` but ignores it, and `sudo` drops
`SQI_SQLITE_PATH`. Run without `--db` from a directory with no database,
v0.3.0's `backup` creates an empty `sqi.db` there and fails (`open source
database: ... no such table: farms`); run from a directory that holds some
other sqi database, it backs that one up without complaint. So check the
result: the backup should be close to the live file's size (`VACUUM INTO`
compacts it, so it can be smaller), and any sqi database, even one with no
jobs, is several hundred kilobytes (385 KB for an empty farm at schema 38).
Do not check it with `sqi-server migrate status --db`: that opens the file
read-write and rewrites it. On PostgreSQL the old binary is at least the
release that added PostgreSQL support, whose `backup` reads `--config` and
`--dsn`, so `sqi-server backup --config /etc/sqi/sqi-server.yaml --out
/backups/sqi/pre-upgrade-<time>.dump` is right there.

To watch the migrations run, or to run them before the start, put this between
steps 3 and 4. It prints one line per migration it applies (or `no pending
migrations`), and it takes the [single-server lock](#one-server-per-database),
so it refuses to run while a server uses the database:

```sh
sudo -u sqi sqi-server migrate up --config /etc/sqi/sqi-server.yaml
```

Taking the backup with the server stopped, rather than online, means the
backup is exactly the database the old binary last wrote: nothing finished
between the backup and the upgrade can be lost by a rollback.

### Steps (Windows service)

From an elevated PowerShell. `Set-Location` matters with the default relative
`store.sqlite_path`: see the note under [Windows service](#windows-service).

```powershell
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

The rollback below (restoring a pre-upgrade backup with the older binary, and
a clean start on it) is executed by `TestUpgrade_OlderBinaryRefused`, which
simulates the newer binary by recording a schema version above the one it was
built with; the backup steps above are not.

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
again; [Restore](#restore) says exactly what that means. Executed by
`TestUpgrade_OlderBinaryRefused`.

### Rolling back to v0.3.0 or earlier

Releases before `restore` existed have neither the `restore` command nor the
version check: v0.3.0 starts on a newer database without complaint and runs
against a schema it does not know. They support only SQLite, so put the
pre-upgrade backup in place by hand, with the server stopped. This procedure is
not executed by a test.

```sh
sudo systemctl stop sqi-server
# store.sqlite_path is /var/lib/sqi/sqi.db and nats.data_dir /var/lib/sqi/nats
# here; every step runs as the service account, which owns that directory.
stamp=$(date -u +%Y%m%dT%H%M%SZ)
for f in /var/lib/sqi/sqi.db /var/lib/sqi/sqi.db-wal /var/lib/sqi/sqi.db-shm /var/lib/sqi/nats; do
  if sudo -u sqi test -e "$f"; then sudo -u sqi mv "$f" "$f.pre-restore-$stamp"; fi
done
sudo -u sqi cp /backups/sqi/pre-upgrade-20261013-020000.db /var/lib/sqi/sqi.db
sudo install -m 0755 /usr/local/bin/sqi-server.old /usr/local/bin/sqi-server
```

Then stop every worker before you start the old server. Without `restore`
nothing closes the attempts the backup recorded as open (tasks assigned or
running), and v0.3.0 has
no [reconciliation](#reconciliation): a task whose worker stays online stays
`running`. With the workers stopped, start the server and wait until the web
UI or `GET /api/v1/workers` shows them offline (about
`scheduler.heartbeat_timeout`, 30 s by default); taking them offline returns
their tasks to `ready`. Then start the workers again.

---

## Backup

`sqi-server backup` makes a consistent copy of the state store while the
server keeps running, on both backends. Workers do not need to pause. It
backs up the **database only**; [What a backup leaves
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

- **The NATS data directory (`nats.data_dir`).** JetStream holds only messages
  the server has not finished with: status reports, log chunks and worker
  registrations in transit (the log stream also keeps chunks for 96 h after
  they are stored, but nothing reads them back). Everything durable is in the
  database. A JetStream directory is also never correct beside a database from
  another moment: an older one replays reports for attempts the database has
  already closed or never saw, a newer one holds reports the database will
  never get. So it is not backed up; `restore` moves it aside instead and
  makes the database safe without it (see [Restore](#restore)).
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
6. renames `nats.data_dir` to `<dir>.pre-restore-<UTC time>` (the same time
   as step 3).

Nothing is deleted. Each step must succeed before the next, and a failure says
what state it left (see [When restore stops partway](#when-restore-stops-partway)).

**Which store and which NATS directory.** `restore` chooses the store as
`backup` does (`--db`, `--dsn`, `store.driver`; see [Backup](#backup)) and
reads `nats.data_dir` from the same configuration the server reads (`--config`,
`SQI_*` variables); `--nats-data-dir` overrides it. Relative paths resolve
from the current directory, as the server resolves them, so run it from the
directory the server runs in, with the server's configuration. If the
configuration cannot be loaded, `restore` refuses rather than fall back to the
built-in defaults, unless you name both the store (`--db` or `--dsn`) and
`--nats-data-dir`; an empty `--nats-data-dir` is refused too.

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
restored a schema-38 backup; schema is now 38
previous database file moved to /var/lib/sqi/sqi.db.pre-restore-20261013T143216Z
fenced 1 attempt(s): their tasks are ready again
NATS data directory moved to /var/lib/sqi/nats.pre-restore-20261013T143216Z
```

There is one `previous database file` line for each file moved: a server that
did not stop cleanly, or a `store copy`, can also leave `sqi.db-wal` and
`sqi.db-shm`, which move with it. `no NATS data directory at <dir>; nothing to
move` means there was none at that path: if you expected one, check that
`nats.data_dir` and the directory you ran from are the server's.

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
restored a schema-38 backup; schema is now 38
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
  it the same way, never on its command line. A missing or too old
  `pg_restore` is reported as such, before the archive is read.
- **Grants are not restored** (`--no-acl`), just as ownership is not: a
  `GRANT` the source database gave another role (a reporting role, say) is
  left out, so a role the new cluster lacks cannot fail the load. Re-apply such
  grants after the restore.
- **Run `restore` on the server's host**, with the server's configuration, so
  that it moves the right `nats.data_dir`. If you run it from another host
  (one with the PostgreSQL client tools, say), move the server's
  `nats.data_dir` aside by hand before you start the server, or run
  `sqi-server restore --in-place` on the server's host afterwards.

Executed by `TestPostgres_RestoreRoundTrip` and, on PostgreSQL 16 and 18, by
`TestBackupRestore_FarmResumes` in `make test-postgres`.

### Restoring in Docker Compose

The `sqi-server` image does not include `pg_restore`, so with
[`deploy/docker-compose.postgres.yml`](https://github.com/uberware/sqi/blob/main/deploy/docker-compose.postgres.yml)
`restore` runs in a one-off container of the `postgres` service, with the
server's binary and its data volume mounted into it. The file's header has the
commands, which were checked by hand against that file (they are not run in
CI).

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
aside (steps 1 and 4 to 6 above); it loads nothing. It refuses a SQLite path with no
database and a PostgreSQL database with no sqi schema, since there would be
nothing to fence.

On SQLite it also refuses a database whose `-wal` file is not empty. Nothing
ties a `-wal` to the database it was written for, and SQLite applies whatever
`-wal` it finds, so a database copied by hand over one whose server stopped
uncleanly would take that server's writes. If that is what happened, move the
old `-wal` and `-shm` aside together with the old database, or use
`sqi-server restore BACKUP`. If the `-wal` is this database's own (its server
crashed, or a snapshot took them together), run `sqi-server migrate up` on it
(or start the server and stop it cleanly once), which folds the `-wal` in, then
run `--in-place` again. A cleanly stopped server leaves no `-wal`, or an empty
one.

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

---

## Reconciliation

A server can believe a task is in flight (assigned or running) on a live
worker that no longer holds it: its completion report was lost with a JetStream directory, the
database was restored by hand, or the worker dropped the task. The worker is
healthy, so the offline sweep never reclaims it, and before this release such a
task stayed `running` for good.

Each worker of this release lists, in every heartbeat, the attempts it holds,
from the moment it receives them until their final status is sent. On every
heartbeat-sweep tick the server reclaims an open attempt when the worker's
latest heartbeat is current, does not list it, and has not listed it for
[`scheduler.reconcile_grace`](configuration.md#schedulerreconcile_grace)
(measured from the later of the attempt's creation and the last heartbeat
that listed it). Reclaiming is what a worker shutdown does: the task returns to
`ready`, the attempt closes as failed (`worker shut down`), and no retry or
failure is counted. The server logs a Warn, `scheduler: reclaimed an attempt
its worker no longer reports`, with the worker, task, attempt and how long it
was open, and counts it in `sqi_scheduler_tasks_reconciled_total`.

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
  heartbeat delayed or skewed`), so that worker's tasks are not reconciled until its clock or its
  delivery recovers. Keep worker clocks synchronized.
- **Older workers are never reconciled.** Workers from v0.3.0 and earlier send
  no attempt list. They are still covered by the offline sweep and by
  `restore`'s fence. A worker of this release holding nothing sends an empty
  list, which counts.

Executed by `TestRestore_MixedAgeNATSDir` (a database restored by hand beside a
newer NATS directory is recovered by reconciliation, and a replayed older one
duplicates no log row) and, for the late-report case, by
`TestReconcileAttempts_LateTerminalReportIsDiscarded`.

Log chunks can be delivered to the server more than once (after a restart,
JetStream can deliver chunks the server already stored). Each is stored once:
a chunk is identified by its attempt and the worker's sequence number.

---

## One server per database

Only one `sqi-server` may use a database at a time, on either backend. The
same guard keeps the commands that rewrite the database (`migrate up`,
`restore`, `store copy`) from running beside a server:

| Takes the lock | Does not |
|---|---|
| `serve`, `migrate up`, `restore`, `store copy` (both its SQLite source and its PostgreSQL target) | `migrate status`, `backup`, `worker token issue`, `worker enroll`, `worker revoke`, `worker list` |

A backup or a worker-credential change against a running server is
legitimate, so those work while it runs.

A consequence for upgrades and supervisors: **no overlapping restarts.** Stop
the old server before starting the new one. A supervisor that starts the
replacement first (a rolling update with surge) sees it refused until the old
one exits. `systemctl restart` stops first and is fine.

### SQLite: the lock file

The server takes an exclusive operating-system lock (`flock` on Linux and
macOS, `LockFileEx` on Windows) on `<store.sqlite_path>.lock`, for example
`sqi.db.lock`, created owner-only beside the database. A second server, or one
of the commands above, fails at once:

```
startup failed: open store: another sqi-server is using sqi.db (lock file sqi.db.lock is held); stop it first: fslock: lock is held
```

The lock, not the file, is the guard. The operating system drops it when the
process exits however it exits, so a crashed server leaves nothing stale, and
the file stays on disk for good. **Never delete it**: that frees nothing, and a
process opening it at that moment would lock a file the next one cannot see. To
find the holder on Linux, `sudo fuser -v /var/lib/sqi/sqi.db.lock`.

SQLite on a network filesystem is not supported, and the lock is only as good
as that filesystem's locking (NFS and SMB emulate it with varying fidelity).

### PostgreSQL: the advisory lock

At start-up the server takes the PostgreSQL advisory lock
`0x7371692d73727672` and holds it until it stops. A second server started on
the same database exits at once, logging `sqi-server exited with error` with
this error:

```
startup failed: open store: another sqi-server is already using this database (PostgreSQL advisory lock 0x7371692d73727672 is held); running more than one server on one database is not supported yet; stop it first
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

Advisory locks belong to one database, so servers on different databases of
the same PostgreSQL cluster do not collide. Running several servers against
one database, for high availability, is not supported yet.

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
   `nats.data_dir` as it is.

   ```yaml
   nats:
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

**If `nats.data_dir` changes** (the server moves to another host or into a
container, or you start it with an empty directory), run `sqi-server restore
--in-place` with the new configuration before starting it. The copy is exact,
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

**How long it takes.** Measured on a 12-vCPU WSL2 host with PostgreSQL 18 in
Docker on tmpfs: a small farm (1,232 rows) 0.21 s, 100,000 tasks (a 25 MB
SQLite file) 4.8 s, and 1,000,000 tasks (245 MB) 46.8 s, all of it one
PostgreSQL transaction.

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
