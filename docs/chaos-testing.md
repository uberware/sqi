<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
# Chaos testing

The chaos suite injects faults into a running sqi farm and checks that the
farm still converges: no job stranded, no completed work done twice, no task
process left behind, no database left inconsistent. It is built on the
[load harness](load-testing.md): a chaos scenario is a load scenario with a
`faults` list, an `expect` section and, for some job classes, a `real_task`.

This page covers what the suite is, how to run it, the scenario format for
faults, the invariants and the rework bound, how known defects are
recorded, the scenario library, the CI jobs and what the suite does not
cover.

---

## What it is, and what it is not

The suite runs real `sqi-server` and `sqi-worker` processes, so a fault is
real: a server is killed with SIGKILL, a worker is frozen with SIGSTOP, a
network path is cut by a TCP proxy that sits between the workers and the
broker, the database is locked from outside the server, a volume is filled to
its last byte. A scenario fires its faults at fixed times while jobs are being
submitted, waits for every fault to heal, waits for the farm to drain, then
checks what the server and the workers did.

It tests the behavior that normal load cannot reach: a server that dies in
the middle of a submission, a broker that comes back with a backlog, a worker
whose machine sleeps, a rack that loses its network for a minute.

It is not:

- **A fix for what it finds.** A defect the suite finds is recorded as a
  known violation against a tracker item (see [Known
  violations](#known-violations)) and fixed separately.
- **A latency test.** Like the load harness, it asserts invariants, never
  timings. A run's wall-clock times are not compared with anything.
- **A test of every kind of fault.** The suite covers server, worker, network,
  database, disk, file descriptor and credential faults, and workers whose
  clocks are wrong, on SQLite or PostgreSQL, on Linux and Windows. It has no
  identity-provider fault, no network file system fault and no server clock
  step (see [Not covered](#not-covered)).

The simulated fleet is still simulated: its workers speak the real protocol
and run no processes. A scenario that needs a real process (a task whose
children outlive it, a worker that is killed mid-task) starts real
`sqi-worker` processes beside the fleet (`workers[].real`) and gives the job
class a `real_task`.

---

## Quick start

You need Go. The managed target builds `sqi-server`, `sqi-worker` and a no-op
task command from the source tree, so nothing needs a prior `make build`.

```sh
make chaos-smoke                          # the three CI scenarios
make chaos SCENARIO=chaos-net-blip        # one scenario
make chaos                                # every scenario that applies here, about two hours
make chaos STORE=postgres                 # the same on PostgreSQL (needs Docker)
make chaos-container                      # the disk scenarios, on real size-limited volumes (needs Docker)
make chaos-framework                      # the suite's own framework test, no scenario
```

These run the suite as a Go test (build tag `chaos`, never part of `make
test`), in `test/chaos`, without the race detector whatever `RACE` says: the
test process hosts the harness, not the binaries under test, and `make test`
already runs the harness's packages with `-race`. Each scenario is a subtest of `TestChaos`. The
scenarios are in the load harness's built-in library, so
`go run ./cmd/sqi-load scenarios` lists them with the rest and `scenarios
--show chaos-net-blip` prints one as resolved YAML.

| Environment variable | Meaning |
|---|---|
| `SQI_LOAD_OUT` | A directory the run directories are written to, so they survive the test and CI can upload them. Without it each scenario writes to a temporary directory the test removes. |
| `SQI_CHAOS_STORE` | `sqlite` (default) or `postgres`. `make chaos STORE=postgres` sets it. PostgreSQL needs `SQI_TEST_POSTGRES_DSN`, which names an empty database the harness uses as it is; without it the test skips, or fails when `SQI_TEST_REQUIRE_POSTGRES=1` is set. |
| `SQI_CHAOS_CONTAINER` | `1` when the size-limited volumes of `make chaos-container` are mounted. The `disk_full` scenarios run only then. |
| `SQI_CHAOS_SERVER_VOLUME` | The directory of the size-limited volume that holds the server's data. Required with `SQI_CHAOS_CONTAINER=1`. |
| `SQI_CHAOS_WORKER_VOLUME` | The directory of the size-limited volume that holds the real workers' session directories. Required with `SQI_CHAOS_CONTAINER=1`. |

`make chaos STORE=postgres` runs through `scripts/chaos-postgres.sh`: it starts
a throwaway PostgreSQL in Docker (`PG_VERSION=16` or `18`, default 18), unless
`SQI_TEST_POSTGRES_DSN` names a server, and stops with an error when it has
neither. A database `drop` fault needs PostgreSQL; the other database faults
run on both stores.

`make chaos-container` builds an image from a staged copy of the source tree
and runs the disk scenarios (`chaos-disk-*`) as root in a container whose
server volume is a 512 MB tmpfs and whose worker volume is a 64 MB tmpfs with
8192 inodes. It also runs the ballast tests of `internal/loadtest/diskfill`,
which mount a tmpfs of their own, so the container gets `--cap-add SYS_ADMIN`
and nothing broader. The container exits non-zero when either set of tests
failed, and the target exits 0 with a message when Docker is not available:
look for `--- PASS: TestChaos/chaos-disk-` lines. Set `SQI_LOAD_OUT` to keep
the run directories; the container writes them as root. A Docker host that
applies its default AppArmor profile refuses the tmpfs mounts the ballast
tests make, so the `docker run` line passes `--security-opt
apparmor=unconfined`. Whether that is enough on GitHub's runners is
unverified. The container scenarios run on SQLite only:
`make chaos-container STORE=postgres` stops with an error.

The test logs each run's directory, and on a failure its `summary.md`. A
skipped test verifies nothing: look for `--- PASS: TestChaos/chaos-...` lines
in the output.

**How long it takes.** A scenario's own `duration` is 3 to 5 minutes 30
seconds, but the server marks no worker offline for about 3 minutes 10
seconds after it starts, and the faults that depend on a worker being marked
offline fire after that, so a run is several minutes longer than its faults
need. On Linux each of the three smoke scenarios takes about 4 to 5.5 minutes, about
13 minutes for the three plus the first binary build. A full pass of the
scenarios that apply on one platform and store takes about two hours; `make
chaos` allows three.

**Platforms.** Linux is the platform the suite is built for. A scenario is
skipped, with its reason in the test output, where its faults cannot be
injected:

- A `stall` fault on Windows: SIGSTOP and SIGCONT do not exist there.
- An `fd_limit` fault off Linux: it sets `RLIMIT_NOFILE` on the running server.
- A `disk_full` fault off Linux, or without the size-limited volumes of `make
  chaos-container`.
- A database `drop` fault on SQLite: it has no connections to drop.

On Windows the rest run, killing a process with `TerminateProcess`. The known
violations for the leftover process tree are limited to Linux (`os: [linux]`).
Which scenarios a given machine runs is a function of the library, and
`sqi-load scenarios --chaos-matrix --os linux --store postgres` prints them
as a JSON array (`--os` is `linux`, `windows` or `darwin`, `--store` is
`sqlite` or `postgres`, and `--container` adds the size-limited volumes).
Linux with SQLite runs 27 of the 31 scenarios, Linux with PostgreSQL 28, the
container 30 with SQLite, and Windows with SQLite 23. `--container --store
postgres` would print all 31, but `make chaos-container` runs SQLite only, so
no target runs that combination.

---

## How a chaos run differs from a load run

A run is a **chaos run** when the target can inject faults (a managed target
does, an attached server does not) and the scenario has any of: a fault, a
`max_rework_attempts` or `max_reclaims` bound, a known violation, or a job
class with a `real_task`. A chaos run does everything a load run does, plus:

1. Fires each fault at its time, concurrently with the others and with the
   submissions. `at` counts from the start of submissions.
2. Waits for every fault to heal before it starts the drain.
3. Checks the seven load invariants with the changes described under
   [Invariants](#invariants). A fault makes **orphans** likely: jobs the
   server created for submissions it gave no 2xx answer (a timeout, a dropped
   connection, a 5xx). Every run finds them, waits for them in the drain and
   checks them.
4. Reads the real tasks' markers to run `no-orphans` and `single-writer`,
   with the real workers still up.
5. Stops the real workers and then the server gracefully, keeps the database,
   and reads it to run `store-integrity` and `executions-recorded`, to
   measure [rework](#rework) and to count [reclaims](#reclaims).
6. Applies the scenario's `expect` section: the rework bound and the known
   violations.

The test passes when the record is complete and every result is OK once the
known violations are applied. A record marked invalid is logged but does not
fail the test: `chaos-server-kill-mid-submit` is always invalid, since
its one job is rolled back by the kill, and the invariants are still judged.

---

## Faults

`faults` is a list. Each entry sets exactly one kind and the fields that kind
takes. A scenario with faults needs a managed target.

```yaml
faults:
  - at: 3m15s
    partition: {groups: [rack-b]}
    duration: 60s
  - at: 1m
    kill: {server: true}
    restart_after: 20s
```

### Fields

| Field | Applies to | Meaning |
|---|---|---|
| `at` | all | When the fault fires, as a duration from the start of submissions. Must not be negative. |
| `duration` | stall, partition, blackhole, latency, flap, database, disk_full, fd_limit, revoke | How long the fault lasts before it heals. Must be positive for these kinds. For `revoke` it is how long the revoked worker keeps running before it is stopped. It is rejected on kill, term and power_cut. |
| `restart_after` | kill, term | How long after the process is stopped it is started again. `0` leaves a worker down for good; a server must be restarted, so it must be positive for one. It is rejected on every other kind. |
| `kill` | | `{server: true}` or `{group: NAME, real: N}`. SIGKILL (`TerminateProcess` on Windows). A killed worker leaves the tasks it started running. |
| `term` | | The same targets. A graceful stop, the way the process shuts down on an upgrade. |
| `stall` | | The same targets. SIGSTOP, then SIGCONT when the fault heals. A stalled server is stopped with its embedded broker; a stalled worker is stopped together with the process group of every task it is running, as a machine that goes to sleep stops. POSIX only. |
| `partition` | | `{groups: [...]}`. Cuts the groups' workers from the broker: every connection is closed and new ones are refused until the fault heals. |
| `blackhole` | | `{groups: [...]}`. Keeps the connections open but forwards nothing in either direction. A connection accepted during the fault is connected to the broker when it heals. |
| `latency` | | `{groups: [...], delay, jitter, bytes_per_sec}`. Delays every forwarded chunk by `delay` plus or minus `jitter` (order kept) and caps each direction at `bytes_per_sec` (0 is unlimited). |
| `flap` | | `{groups: [...], period}`. Cuts the groups for half of each `period` and opens them for the other half, for `duration`, and ends open. |
| `power_cut` | | `{order, down, gap}`. Kills the server and every running real worker and crashes the simulated fleet, all at once. After `down` it brings back the first side, and after `gap` more the second. `order` is `server-first` or `workers-first`. |
| `database` | | `{mode}`, `stall` or `drop`. A stall leaves the database unavailable without closing anything: on SQLite the harness takes the database's write lock from outside the server, on PostgreSQL a proxy between the server and the database keeps the connections open and forwards nothing. A drop closes the server's PostgreSQL connections and refuses new ones, and needs PostgreSQL. The database is available again when the fault ends. |
| `disk_full` | | `{volume, headroom_bytes, inodes, reason, min_failed, max_failed}`. Fills a size-limited volume with a ballast file until `headroom_bytes` are free (0 fills it) and, with `inodes: true`, uses up the free inodes too. The ballast is removed when the fault heals. `volume` is `server` (the server's data directory, which holds the database and the broker's data) or `workers` (the real workers' session directories). `reason`, `min_failed` and `max_failed` judge a `workers` volume: see `disk-full` under [Invariants](#invariants). Needs the size-limited volumes of `make chaos-container`. |
| `fd_limit` | | `{headroom}`. Lowers the running server's soft open-file limit (`RLIMIT_NOFILE`) so that it can open `headroom` more files, and restores it when the fault heals. Linux only. |
| `revoke` | | `{group, real}`. Revokes the broker credential of `real` of the group's real workers (0 meaning all of them) through the REST API, after removing each worker's join token so it cannot enroll again. The broker disconnects a revoked worker and refuses its reconnects; the worker process keeps running until `duration` has passed, then is stopped and not restarted. The scenario needs `server.broker_auth: true`. |

A process target names `server: true`, or a worker `group` and how many of
its real workers (`real`, 0 meaning all of them). A network fault names
`groups`; an empty list means every group.

### Skewed worker clocks

`workers[].clock_skew` is a duration, positive or negative and at most 48
hours, that offsets the clock of every simulated worker in the group. It is
not a fault: the offset holds for the whole run. It moves only the timestamps
a worker puts on the wire: its heartbeats, its task status reports and its log
chunks. Everything else a simulated worker does stays on the real clock: its
ledger of what it ran, its timers, when it was last assigned work and the
timestamps of the OpenJD progress snapshots. A group with real workers cannot
carry a skew. The `skew-warning` check expects the server to warn about every
skewed group, and `attempt-order` judges what the server recorded (see
[Invariants](#invariants)).

A power cut with `workers-first` boots the workers while the server is still
down. `sqi-worker` exits when it cannot reach the broker at start, and the
shipped systemd unit restarts it every 5 seconds, so the harness does the
same: it retries the workers every 5 seconds, up to 12 times, once the server
is back.

### Validation

A scenario with several problems reports all of them, each with its path
(`faults[1].restart_after`).

- Faults, `expect` and `real_task` need a managed target; an attached run
  rejects them.
- Exactly one kind is set.
- `at` is not negative, and the fault has healed by the scenario's
  `duration`: `at` plus `duration` plus `restart_after`, and for a power cut
  plus `down` plus `gap`.
- A kill, term or stall names exactly one of `server` and `group`. A group
  must be in `workers` and have real workers; `real` is not negative, is no
  more than the group's real workers, and does not apply to the server.
- `restart_after` is not negative and applies to kill and term only. A kill or
  term of the server needs a positive `restart_after`.
- Every group a network fault names is in `workers`.
- A latency has a non-negative `delay`, `jitter` and `bytes_per_sec`, sets at
  least one of `delay` and `bytes_per_sec`, and has `jitter` no greater than
  `delay`.
- A flap's `period` is positive and no longer than its `duration`.
- A power cut's `order` is one of the two, `down` is positive and `gap` is not
  negative.
- A `database` fault, a `disk_full` fault, an `fd_limit` fault and a `revoke`
  fault have a positive `duration`.
- A database `mode` is `stall` or `drop`.
- A `disk_full` fault's `volume` is `server` or `workers`, `headroom_bytes` is
  not negative, `reason` is a regular expression that compiles, and
  `max_failed` is not negative. `min_failed` is at least 1 and no more than
  `max_failed`. `reason`, `min_failed` and `max_failed` apply to a `workers`
  volume only, and a `workers` volume needs a group with real workers.
- An `fd_limit` fault's `headroom` is positive.
- A `revoke` names a group with real workers, never the server, and the
  scenario sets `server.broker_auth`.
- A group's `clock_skew` is at most 48 hours from the server's clock, and the
  group has no real workers.
- A fault that takes the server down (kill, term or stall of the server, a
  power cut, a full server volume or a lowered file descriptor limit) needs
  `clients.websocket.count` and `clients.rest_pollers.count` at 0: UI clients
  do not survive a server outage.

A stall is also refused at run time on Windows.

### Lanes

A scenario with a network fault starts one TCP proxy in front of the broker
for each worker group, a **lane**. Every worker of the group, simulated or
real, dials its group's lane instead of the broker, so a fault on a group
acts on that group's connections alone. The REST client the harness drives
the server with does not go through a lane.

Faults can overlap on one lane. The lane takes the strongest state of those
active: cut over blackhole over latency over open, and among latencies the one
that began last. When a fault ends, the lane returns to what the others still
want.

### Fault windows and logs

A server fault (kill, term, stall, power cut), a full server volume, a lowered
file descriptor limit and a network fault that can lose bytes (partition,
blackhole, flap) leave a **fault window**: from when it fired to when it
healed. The `logs` invariant treats a window differently (see below). A full
server volume and a lowered descriptor limit open a window over every group,
since the broker cannot store what any worker publishes. A latency fault
leaves none: it delays and rate-limits a lane and drops nothing, so a
scenario that only slows a group still has `logs` exact. A database fault
leaves none either: the log stream is durable, so the lines still arrive and
a loss is a finding. Faults on real workers (including a full workers volume
and a revoke) leave no window.

---

## Real tasks and markers

The no-op command a real `sqi-worker` runs prints a line and exits. A job
class with `real_task` changes what it does, so a scenario can kill a worker
in the middle of a real process tree:

```yaml
jobs:
  - name: tree
    rate: 6/m
    queue: render-real
    steps: 1
    tasks_per_step: 1
    cores: 1
    real_task: {duration: 60s, log_every: 1s, chatty_child: true, silent_child: true}
```

| Field | Meaning |
|---|---|
| `duration` | How long the task's main process runs. Must be positive. |
| `log_every` | The main process prints a line this often. `0` prints one line. Must not be negative. |
| `chatty_child` | Starts a child that prints to the task's stdout every 200 ms for `duration`. |
| `silent_child` | Starts a child with no stdio of the task's that writes the task's output file every 200 ms for `duration`. |
| `scratch_bytes` | How many bytes the main process writes into the task's working directory, spread over `duration`. A task that cannot write them fails with a non-zero exit. Must not be negative. `0` writes nothing. Used with a full workers volume. |

The children stay in the task's process group. Each real task process writes
**markers** under a directory the harness owns: one file per process (its
role, its PID and group, when it started, when it was last seen, and when it
ended and with what exit code), refreshed every 250 ms while the process
lives, and one output file per task, which every process writing that task's
output appends to. A task killed by a signal records no end, but its last
refresh shows when it was last alive. The `no-orphans` and `single-writer`
checks read the markers, and so does the rework measurement.

In the library scenarios that run a real task (`chaos-worker-kill-mid-task`,
`chaos-worker-gone`, `chaos-worker-sleep`, `chaos-worker-revoke` and the two
`chaos-disk-worker-full-*` scenarios) the real workers serve their own queue
(`queues: [render-real]`), so every real task runs on a real worker while
simulated workers carry the background load. In the two
`chaos-disk-worker-full-*` scenarios each real worker has a queue and a hold
job of its own (`render-real-a`, `render-real-b`), so both are given work
after the fault: a worker that stopped taking work leaves its queue's tasks
unfinished, which the `terminal` check fails. In the two power-cut scenarios the
real workers share the `render` queue with the simulated ones and run the
default no-op task, so they write no markers.

---

## Invariants

A chaos run reports ten invariants. The first seven are the load harness's;
`executions-recorded`, `store-integrity` and `attempt-order` are added by chaos
runs. A scenario with a `real_task` adds `no-orphans` and `single-writer`,
which read the real tasks' markers, one with `max_rework_attempts` adds
`rework`, one with `max_reclaims` adds `reclaims`, one with a `revoke` fault
adds `revoked`, one with a `clock_skew` group adds `skew-warning`, and one with
a `disk_full` fault on a workers volume adds `disk-full`; a `must` known
violation that did not fire adds a failed `known:<check>` (see below). A
failing invariant carries evidence naming the job, task, attempt or process,
at most 20 entries.

| Invariant | The exact rule |
|---|---|
| `terminal` | Every accepted job and every orphan is `completed`, `failed` or `canceled` within `drain_timeout` after submissions stop. A job left open is stranded. |
| `outcomes` | As in the load harness: every task's final server status is the one the simulator decided; a job has exactly `steps x tasks_per_step` tasks; a `completed` job has only succeeded tasks. A task a real worker ran has no ledger to compare with and is left out. |
| `single-attempt` | As in the load harness: on each sampled task, no two of the server's attempts are held at once, each attempt after the first follows a failed or canceled one, and a terminal task holds none. On every task, the simulator never ran two attempts at once. A task's attempts are ordered by the server's assignment time, since a lease reply held up in flight can reach the worker after the next attempt's. |
| `logs` | As in the load harness: on each sampled task, the lines the server returns for the latest attempt equal the lines the simulator sent, in order. Inside a fault window (a latency fault opens none) the lines need only be an in-order subsequence of those sent: none duplicated, reordered or corrupted, any missing. This applies when the attempt, from its lease to its end, overlaps a window that hit its worker (the worker's group is in the window's groups, or the window names none). A task still missing lines is re-read for the logs settle time, since a restarted server replays the log stream; lines still missing after that are counted in the `logs.lost_in_fault` counter, not failed. |
| `workers-converge` | The farm's online worker count equals the number of workers the harness has running. In a chaos run the allowance runs from the last time the server's broker connection came up (its start, and the heal of each server kill, term or power cut; a stall keeps the connection): the server's liveness grace of about 3 minutes 10 seconds (the worker timeout, a worker's longest redial and one heartbeat), one sweep interval and 10 seconds, or the load harness's allowance, whichever is longer. The check is made after the last fault has healed. |
| `caps` | As in the load harness, counting the attempts of orphans as well. |
| `submission-atomic` | As in the load harness: every job in the farm is an accepted job or an orphan, and every orphan holds all of its class's tasks. |
| `executions-recorded` | Read from the stopped server's store. Every attempt a simulated worker leased is in the store and closed, and is not `succeeded` when the worker reported it failed (one the worker reported succeeded may have been reclaimed, so it may be `failed` or `canceled`). No real task ran more times than the store has attempts for it. Marker files that could not be parsed are noted and fail nothing; a marker directory that cannot be read at all fails the check as unmeasured, which no known violation excuses. |
| `store-integrity` | Read from the stopped server's store. On SQLite `PRAGMA integrity_check` is `ok` and `PRAGMA foreign_key_check` finds nothing. On both backends: no active usage claim breaks the claim invariant (I3); a terminal step holds only terminal tasks; a terminal task has no running attempt; a completed job has only completed steps; a failed or canceled job has no step still open. |
| `no-orphans` | No process of a real task was seen alive more than 10 seconds after the worker incarnation that started it ended. An incarnation still running is not judged. |
| `single-writer` | For each task, no two process executions wrote the task's output over overlapping spans of time, the span being from a process's first line to its last. |
| `reclaims` | Read from the stopped server's store, only with `max_reclaims`: the server took back no more attempts from their workers than the bound (see [Reclaims](#reclaims)). |
| `attempt-order` | Read from the stopped server's store, on every attempt of every job in the farm: `started_at` is not more than 1 second before `created_at`, and `ended_at` is not more than 1 second before `started_at`. The server stamps `created_at` and `started_at` with its own clock and dates `ended_at` from the broker's stamp of the terminal status minus the worker's measured age of the end, so a worker whose clock is wrong cannot break the order; the check guards that. |
| `revoked` | Read from the stopped server's store, only with a `revoke` fault: for each revoked worker, no heartbeat was recorded and no attempt was created for it more than 5 seconds after its credential was revoked. The 5 seconds cover the revoke reaching the broker and the server's own bookkeeping. |
| `skew-warning` | Read from the server's log, only when a group has a `clock_skew`: for every skewed group, the server logged `scheduler: worker heartbeat delayed or skewed` for at least one of its workers. A log that cannot be read leaves the check unmeasured. |
| `disk-full` | Read from the stopped server's store, only with a `disk_full` fault on a `workers` volume. For each fault that fired and healed, over the attempts of the real workers: every attempt that failed and ended from the fault's start to 10 seconds after it healed names a cause matching the fault's `reason`, when it sets one; at least `min_failed` and no more than `max_failed` of them failed, when the fault sets those; some real worker succeeded an attempt that started after the fault healed; and every real worker given an attempt after the fault healed succeeded one. A real worker given no attempt after the heal is not judged, since the scheduler can hand every task to another worker. A fault that never healed fails the check, and a run with no real worker to judge leaves it unmeasured. Without `min_failed`, a run in which no attempt failed in the window passes the check. Its summary notes, for each fault, how many attempts failed in the window, so a pass shows whether the fault made a task fail. |

A store the checks cannot open or read fails `store-integrity` and
`executions-recorded` with the error as evidence.

---

## Rework

**Rework** is work the farm did twice: an attempt that ran to completion on a
worker and still did not count.

- A simulated worker's attempt is rework when the worker recorded success and
  the server's attempt is not `succeeded`: the server reclaimed it first.
- A real task's execution is rework when its main process completed (exit 0)
  more times than the server has succeeded attempts for the task. Each
  succeeded attempt credits the completed execution that ended nearest its
  end; the rest are rework.

Each costs the time it ran, and is attributed to the latest fault that began
before the worker finished it. A crash alone costs no rework, since a crashed
attempt never completes; only a completed result the farm lost does.

`expect.max_rework_attempts` bounds the count. A run with the bound gets a
`rework` result: OK when the count is within the bound, failed when it is
over (with the count, the seconds of work and examples), and failed with
"rework could not be measured" when the store could not be read. Without the
bound, rework is measured and reported but never fails the run. A bound of `0`
says no completed work may be redone.

The library's bounds include headroom over a Linux run, and a comment in most
scenarios shows the arithmetic: a rack partition's bound of 30 covers the work
in flight on the cut quarter of the fleet.

A count that leaves some real tasks' executions out (the markers could not be
read, or some exec files could not be parsed) is short. Within the bound, a
short count fails as unmeasured, and no known violation excuses a short count
over the bound, since the true count may be over the entry's ceiling too.

---

## Reclaims

A **reclaim** is an attempt the server closed as failed to take its task back
from a worker: the heartbeat sweep marked the worker offline (`worker went
offline`), the worker shut down or stopped reporting the attempt (`worker shut
down`), or it re-registered from a new process (`worker restarted`). The
stopped server's store records each with that message, and every chaos run
counts them, by reason, in `run.json` and the summary.

A reclaim is not always rework: when the cancel reaches the worker before the
task finishes, nothing completed is lost, so a scenario that must not take
work from a live worker cannot rely on its rework bound. `expect.max_reclaims`
bounds the count. A run with the bound gets a `reclaims` result: OK within it,
failed over it (with the count and examples), and failed as unmeasured when
the store could not be read. `chaos-net-blip`, `chaos-net-slow-site`,
`chaos-server-stall-short`, `chaos-db-stall-5s`, `chaos-db-stall-30s`,
`chaos-db-stall-2m`, `chaos-db-stall-3m`, `chaos-clock-skew` and
`chaos-session-expiry` set it to
`0`: none of their faults or conditions should take a worker offline.

---

## Known violations

When a chaos run finds a defect in sqi, the scenario records it rather than
fail forever. Each entry is a finding the scenario expects:

```yaml
expect:
  max_rework_attempts: 0
  known_violations:
    - check: rework
      kind: may
      max_attempts: 10
      reason: a task finished during the fault is run again
      tracker: H99x
```

| Field | Meaning |
|---|---|
| `check` | An invariant name from the table above, or `rework`. |
| `kind` | `must` (the default) or `may`. |
| `max_attempts` | On a `rework` entry, the most rework it excuses. Required there, and an error on any other check. |
| `match` | A regular expression (RE2) that scopes the entry to the violations it matches. Optional, and an error on a `rework` entry. |
| `os` | The operating systems (`linux`, `darwin`, `windows`) the entry applies on. Empty means all. On any other system the entry does nothing. |
| `reason` | What the defect does. Required. |
| `tracker` | The tracker item, such as `H99x`. Required. |

How an entry is applied:

- A failed result that an entry names becomes OK. The summary shows it as
  `known (H99x)` and keeps its evidence. Several entries for one check join
  their trackers.
- An entry with a `match` pattern excuses only the violations it matches.
  When every entry for a check has one, a failure is excused only when each
  of its violations matches one of them, including those past the 20 the
  evidence shows. An unmatched violation keeps the result failed, and its
  evidence counts the unmatched violations and names the first, so a new
  defect in the same check cannot hide behind the recorded one. An entry
  with no pattern still excuses every violation of its check. A result with
  more than 10,000 violations cannot be checked against a pattern and stays
  failed.
- A `rework` entry excuses rework only up to its `max_attempts`. Rework above
  the scenario's `max_rework_attempts` and at or under the ceiling is the
  defect firing, and the summary notes the ceiling. Rework over the ceiling
  stays a failure, and its evidence says the known ceiling was exceeded. The
  ceiling must be greater than `max_rework_attempts`, which the scenario must
  set, so a regression far larger than the defect still fails the run.
- A check that could not be measured, or did not run, is never excused: its
  failure stays, and a `must` entry for it is not counted as fired. Rework
  that could not be measured is one, as is a store the checks could not open.
- A `must` entry whose check did not fail adds a failed result of its own,
  named `known:<check>`, as does a `must` entry whose pattern matched none of
  its check's violations. This is the stops-firing rule: when the defect is
  fixed, the scenario fails until the entry is removed, so a fixed defect
  cannot stay on the list. The same happens when the check did not run.
- A `may` entry that did not fire adds nothing. It is for a defect that is
  a race, whose effect varies from run to run.
- A failure no entry names fails the run.

`TestChaos_MustViolationThatDoesNotFireFails` proves the stops-firing rule is
enforced end to end: it runs `chaos-server-term` with a `must` entry for
`outcomes` that cannot fire and asserts the run records a failed
`known:outcomes`. The rule itself is unit-tested in `internal/loadtest/chaos`.

### Recorded defects

The suite records one defect in sqi, also listed under [Known server
defects](load-testing.md#known-server-defects).

| Tracker | Defect | Recorded in |
|---|---|---|
| H13f4 | Killing `sqi-worker` on Linux leaves its task's process tree running. A silent child, which writes no stdout, never gets SIGPIPE, outlives the worker's restart and writes alongside the re-run. | `chaos-worker-kill-mid-task`, `no-orphans` and `single-writer`, `may`, `os: [linux]` |

---

## The scenario library

The 31 chaos scenarios are built in and named `chaos-...`. Unless noted, a
scenario runs on SQLite or PostgreSQL, with 100 simulated 8-core workers and
about 2.5 tasks per second of 2-step jobs. A scenario's Fault column says
where it needs more: PostgreSQL, Linux, or the container of `make
chaos-container`.

| Scenario | Fault | Bound and known violations | Duration |
|---|---|---|---|
| `chaos-server-kill-5s` | SIGKILL the server at 1m, restart it 5s later. | rework 0 | 3m |
| `chaos-server-kill-20s` | The same, restarted 20s later. **Smoke.** | rework 0 | 3m |
| `chaos-server-kill-90s` | The same, restarted 90s later: the outage outlasts a worker's reconnect buffer for some of what it publishes. | rework 0 | 3m30s |
| `chaos-server-kill-mid-submit` | One job of 100,000 tasks, the server killed 2s in, during the write, restarted 10s later. 200 workers; the drain allows 20 minutes. | rework 0 | 3m |
| `chaos-server-term` | A graceful stop at 1m, restarted 10s later: the upgrade path. | rework 0 | 3m |
| `chaos-server-stall-short` | SIGSTOP the server for 15s at 3m15s, under `heartbeat_timeout`. POSIX. | rework 0, reclaims 0 | 3m45s |
| `chaos-server-stall-long` | SIGSTOP for 60s at 3m15s, about twice `heartbeat_timeout`. POSIX. | rework 0 | 4m30s |
| `chaos-logs-during-restart` | 40 workers, tasks of 5,000 lines of 200 bytes, the server killed at 1m and restarted 20s later: every terminal status must still arrive. | rework 0 | 3m |
| `chaos-worker-kill-mid-task` | One of 2 real workers killed at 1m while it runs 60s process trees (a chatty and a silent child), restarted 10s later. 60 simulated workers carry the background load. | rework 8; H13f4 `may` on `no-orphans` and `single-writer`, `os: [linux]` | 3m |
| `chaos-worker-gone` | One of 2 real workers killed at 30s while it runs 30s tasks, never restarted: its work is reclaimed and the worker is shown offline. **Smoke.** | rework 4 | 3m |
| `chaos-worker-sleep` | SIGSTOP one of 2 real workers for 75s at 3m15s while it runs 90s tasks, then SIGCONT: stale attempts are canceled, nothing is credited twice. POSIX. | rework 8 | 4m45s |
| `chaos-churn-storm` | No fault: 100 simulated workers each crash about 30 times an hour and restart 10s later. | rework 5 | 3m |
| `chaos-churn-instant-restart` | The same with an instant restart: a regression guard for an attempt left running after the restart (see [Known server defects](load-testing.md#known-server-defects)). | rework 5 | 3m |
| `chaos-net-blip` | Cut every one of 120 workers from the broker for 5s at 3m15s and for 15s at 3m40s. No reclaim, no rework. **Smoke.** | rework 0 | 4m15s |
| `chaos-net-rack-partition` | Cut `rack-b`, 30 of 120 workers, for 60s at 3m15s, twice `heartbeat_timeout`: their work is reclaimed and re-run. | rework 30 | 4m30s |
| `chaos-net-slow-site` | 200ms plus or minus 50ms of latency and a 256 KiB/s cap on `remote`, 40 of 120 workers, from 30s for 4 minutes: no false offline, no reclaim, no lost log. | rework 0 | 4m45s |
| `chaos-net-flap` | Cut and restore `rack-b`, 30 of 120 workers, every 3s for a minute from 3m15s: converges with nothing stranded. | rework 3 | 4m30s |
| `chaos-power-cut-server-first` | At 1m kill the server and every worker (100 simulated, 2 real), bring the server back 20s later and the workers 10s after it. | rework 5 | 3m |
| `chaos-power-cut-workers-first` | The same with the workers back first, 20s later, and the server 10s after them. | rework 5 | 3m |
| `chaos-db-stall-5s` | Stall the database for 5s at 1m. | rework 0, reclaims 0 | 3m |
| `chaos-db-stall-30s` | Stall the database for 30s, `heartbeat_timeout`, at 3m15s. | rework 0, reclaims 0 | 4m15s |
| `chaos-db-stall-2m` | Stall the database for 2m at 3m15s: four `heartbeat_timeout`s, and `scheduler.reconcile_grace`. | rework 0, reclaims 0 | 5m30s |
| `chaos-db-stall-3m` | Stall the database for 3m at 3m15s: longer than `SQI_WORKER`'s 2-minute MaxAge, so heartbeats expire unread and the worker consumer must hold the heartbeat sweep after the skip. | rework 0, reclaims 0 | 6m45s |
| `chaos-db-drop` | Close the server's PostgreSQL connections and refuse new ones for 30s at 3m15s. PostgreSQL only. Submissions made meanwhile are refused with 500, which the summary reports as a finding. | rework 0 | 4m15s |
| `chaos-disk-server-full` | Leave the server's data volume no free space for 60s at 1m, under a verbose renderer (5,000 lines of 200 bytes per task) on 40 workers. Container. | every check must pass once the space returns | 3m |
| `chaos-disk-worker-full-at-setup` | Leave the 2 real workers' session volume no free space and no free inodes for 60s at 1m, so tasks fail at setup. Each failure must name "no space left on device", at most 12 may fail, and each real worker, on a queue of its own, must succeed a task afterwards. Container. | rework 0 | 3m |
| `chaos-disk-worker-full-mid-render` | Leave the 2 real workers' session volume 1 MiB free for 60s at 1m05s while their tasks write 8 MiB each, so tasks fail partway through. Each failure must name the process's exit, at most 16 may fail, and each real worker, on a queue of its own, must succeed a task afterwards. Container. | rework 0 | 3m |
| `chaos-server-fd-exhaustion` | Linux. Cut `rack-b`, 30 of 120 workers, for 20s at 3m15s and lower the server's open-file limit for 40s at 3m20s so it can open only 10 more files, so the rack redials into the limit. | rework 30 | 5m |
| `chaos-worker-revoke` | Broker auth on. Revoke one of 2 real workers' credentials at 3m21s and stop it 60s later, not restarting it. | rework 4 | 4m45s |
| `chaos-clock-skew` | No fault: 20 each of 120 workers run their clocks 10m ahead, 10m behind and a day ahead of the server's. | rework 0, reclaims 0 | 4m |
| `chaos-session-expiry` | No fault: broker auth with a 1m session lifetime (`nats.auth.user_ttl`), so each of 100 workers reconnects about four times in a run, with 30 to 90s tasks. | rework 0, reclaims 0 | 4m |

`chaos-server-fd-exhaustion` lowers the limit on the running server while a
rack redials, rather than starting a server with a low limit and more workers
than it can hold: the simulated fleet fails the whole run when a worker cannot
connect at start, so that shape cannot run. The limit comes 5 seconds after the
cut and lasts 40, so the rack's redial, whose backoff puts its first attempt
after the heal 22 to 34 seconds after the cut, meets it.

The scenarios that depend on the server having marked a worker offline fire at
3m15s, after the server's liveness grace of about 3m10s from its start: before
then a cut or a stall cannot be mistaken for a dead worker however long it
lasts. That is why the library's runs are longer than their faults need.

Every scenario also runs all the invariants above. `Duration` is the
scenario's `duration`, after which the run drains, so a run takes longer (see
[Quick start](#quick-start)).

---

## Reading a chaos summary

A chaos run's `summary.md` has four sections beyond the load harness's.

**Faults** lists each fault that fired, with its kind, its target, when it
started and when it healed, as offsets from the start of submissions, and the
error that stopped it when there was one. A fault that never healed shows
`never`: a worker killed with no `restart_after` stays down. A fault can carry
notes: a full server volume notes how many readiness probes ran and how many
found the server not ready (and each change of answer), and a lowered file
descriptor limit notes the server's open files and the limit it set.

**Rework** gives the attempts that ran to completion and did not count, the
seconds of work they cost, how many each fault is blamed for (`by fault: #0
3`, where `#0` is the first fault) and up to 20 examples, each naming the
attempt or the process.

**Reclaims** gives the attempts the server took back from a worker, by reason,
and up to 20 examples.

**Invariants** lists every result as `PASS`, `FAIL`, `UNMEASURED` for a check
that could not judge (no finding, and no known violation excuses it), or
`known (H99x)` for a failure a known violation excused, with its evidence
beneath. A line
`known:<check>` that fails is a `must` entry whose check held: the defect it
records may be fixed, and the entry should be removed.

The rest of the files in the run directory are the load harness's. A managed
run's `target/` holds the server's and the real workers' logs, and the markers.

---

## CI

Every chaos check in CI is advisory, because the scenarios are
timing-sensitive and a shared runner is noisier than a workstation. A scenario
gates only after it passes 20 consecutive runs on GitHub-hosted runners with
only its known violations firing; until then its failure is reported and
blocks no merge.

**`chaos-smoke`** in `.github/workflows/ci.yml` runs three scenarios that carry
no known violation, on Linux with SQLite: `chaos-server-kill-20s`,
`chaos-worker-gone` and `chaos-net-blip`. It runs on every push to `main`, and on a pull
request that touches `internal/scheduler`, `internal/store`, `internal/bus`,
`internal/worker`, `internal/server`, `cmd/sqi-server`, `cmd/sqi-worker`,
`internal/loadtest`, `test/chaos`, `go.mod`, `go.sum` or `ci.yml`, so most pull
requests do not pay for it. It needs only the path filter, not the `test` job.
Each scenario runs in a job of its own, the three at once (`make chaos
SCENARIO=...`), so the check takes about as long as the slowest scenario plus
its build, under 10 minutes (each job allows 25). Each job sets `SQI_LOAD_OUT`,
uploads the run directories and the log as `chaos-smoke-runs-<scenario>` (7
days), and asserts by name that its subtest passed, since a skipped or
filtered test would leave `go test` green. The job is advisory: it has `continue-on-error: true`, so
a failing scenario marks its step failed and keeps the artifacts but does not
fail the run.

**The nightly** (`load-nightly.yml`, 03:30 UTC and on a manual dispatch
without a `duration`) runs the whole suite in four jobs, one job per scenario,
all at once. A `chaos-plan` job lists the scenarios from the library with
`sqi-load scenarios --chaos-matrix`, so a new `chaos-*.yaml` joins without a
workflow edit and each job runs only what applies on its platform and store.
Each job allows 40 minutes (120 for the container job), uploads its run
directories (30 days) and writes its result to its job page. The nightly
ignores the dispatch's `scenario` input.

| Job | Runs on | Scenarios | Artifacts |
|---|---|---|---|
| `chaos` | Linux, SQLite | The 26 that apply, plus one job for the framework test (`make chaos-framework`). | `chaos-runs-sqlite-<scenario>` |
| `chaos-postgres` | Linux, PostgreSQL 18 in a service container | The 27 that apply, the database ones among them (`make chaos STORE=postgres`). | `chaos-runs-postgres-<scenario>` |
| `chaos-container` | Linux, Docker | The three `chaos-disk-*` scenarios and the ballast tests, on real size-limited volumes (`make chaos-container`). | `chaos-runs-container` |
| `chaos-windows` | Windows, SQLite | The 22 that apply (no stall, descriptor limit or disk fault). | `chaos-runs-windows-<scenario>` |

The container job asserts the three disk scenarios and the two ballast tests
(`TestFill_LeavesHeadroom`, `TestFill_Inodes`) by name, because the target
exits 0 when Docker is missing.

Nothing gates the nightly. All scenarios are advisory in all four jobs, and so
is `TestChaos_MustViolationThatDoesNotFireFails`, which checks the framework but
runs a real scenario (`chaos-server-term`) to do it, so it is held to the
same 20-run bar. A job whose test did not pass does not fail, but raises a
warning annotation.

**Promotion.** When a test has passed 20 consecutive runs on GitHub-hosted
runners, add its name (`TestChaos/<scenario>`, or
`TestChaos_MustViolationThatDoesNotFireFails`) to the `GATED` variable of the
job that runs it in `load-nightly.yml` (whitespace-separated). Each of the four
jobs sets one, on the step that asserts its gated scenarios, which runs
`scripts/assert-gated-tests.sh`. A scenario that runs in several jobs is
promoted per job. Once all three smoke
scenarios have their 20, also remove `continue-on-error` from the `chaos-smoke`
job in `ci.yml`, so a failing pull request is blocked. A scenario that cannot
meet 20 stays advisory.

---

## Not covered

The suite has no identity-provider fault and no network file system fault: a
volume that fills is covered, one that hangs on a stalled mount is not. It
steps no clock on the server (`clock_skew` offsets the clocks of simulated
workers only, not the server's and not a real worker's).

It also has these limits:

- Every fault has a fixed `at`. Fault times are not drawn from a seeded
  distribution.
- Nothing reports how often each `may` known violation fires, so an entry
  that never fires is not flagged.
- A `disk_full` fault on a workers volume without `min_failed` passes the
  `disk-full` check in a run in which the fault never made a task fail. The
  summary records the count for each fault window.
- Both of `chaos-worker-kill-mid-task`'s H13f4 entries are `may`: its real
  task is too short for the orphaned child to outlive the 10 s grace in every
  run.
- An entry without a `match` pattern excuses every failure of its check, so a
  new defect in the same check hides behind a recorded one.
- A `chaos-disk-*` scenario skips outside `make chaos-container`, so a run of
  `make chaos` alone never exercises the disk faults.

Fixing a defect includes removing its entries from `known_violations`.

---

## See also

- [Load and soak testing](load-testing.md): the harness the suite is built on,
  the scenario format, the seven load invariants and the known server defects.
- [Operations](operations.md): the shutdown, restart and recovery behavior the
  scenarios exercise.
- [Development](development.md): the Makefile targets and the test suites.
