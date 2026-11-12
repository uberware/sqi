<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
# Load and soak testing

`sqi-load` is sqi's load and soak harness. It drives a real `sqi-server` with a
fleet of simulated workers, a stream of job submissions and a crowd of
WebSocket and REST clients, measures what the server does under that load, and
checks that nothing was lost, double-run or stranded along the way.

This page covers what the harness is and is not, how to run it, the scenario
format and the built-in library, how to read a report, sweep and soak runs,
sharding one run across several machines, the CI jobs, baseline numbers, known
server defects, and open questions for large-scale runs.

---

## What it is, and what it is not

It is a repeatable tool that scales a farm along each of these axes: workers,
jobs, tasks per job, steps and dependencies, queues and usage pools, log
volume, UI clients, failure and churn rates, and run length. It reports:

- **throughput**: submissions, task completions and log lines emitted per
  second, over the window in which jobs were submitted;
- **latency distributions**: submit, submit to first lease, the server's
  ingest of a terminal status, task and job end to end, WebSocket event lag,
  and REST read latency per endpoint;
- **server resource use**: CPU, RSS, goroutines, open files, GC pauses,
  database, WAL and NATS data size, and JetStream bytes per stream;
- **correctness under load**: six invariants checked at the end of every run
  (see [Reading a report](#reading-a-report));
- **growth over time**: least-squares slopes, and findings for the ones that
  look like leaks.

Two runs of the same scenario can be compared (two server revisions, two
stores, two machines) with `sqi-load compare`.

It is not:

- **A fix for what it finds.** The harness measures and records. Known
  defects are listed under [Known server defects](#known-server-defects).
- **A fault-injection tool.** It simulates worker churn (crashes,
  disconnects, restarts) because that is a normal condition on a real farm.
  Killing the server, partitioning the broker and the rest of fault injection
  belong to the [chaos suite](chaos-testing.md), which is built on this
  harness.
- **A test of the web UI's rendering.** UI load is modelled as WebSocket
  subscribers and REST pollers, which is what the server sees.
- **A release artifact.** `sqi-load` lives in this repository
  (`cmd/sqi-load`) because it imports `internal/` packages, and it is not in
  the release archives.
- **A source of latency thresholds.** CI asserts invariants and run validity,
  never timings.

Simulated workers speak the real worker protocol over real NATS connections
(one connection per worker, so per-connection broker cost is measured) and
reuse the real worker packages for leasing, heartbeats, status, log streaming
and cancel. They run no processes: a task is a timer. The design target is
10,000 simulated workers per generator process; that scale is unmeasured (see
[Open questions](#open-questions)). Scenarios can also start a few real
`sqi-worker` processes beside the simulated fleet so the real execution path
sees the same load.

> A number from `sqi-load` describes the server only while the generator kept
> up. The harness samples its own CPU and timer lateness and marks a run
> INVALID when it was the bottleneck. Latencies from an INVALID run do not
> describe the server.

---

## Quick start

You need Go. The managed target builds `sqi-server`, `sqi-worker` and a no-op
task command itself, from the source tree, so none of this needs a prior
`make build`.

### The smoke run

```sh
make load-smoke
```

This runs the `smoke` scenario end to end as a Go test (build tag `load`,
never part of `make test`): 20 simulated workers, 50 jobs of 2 steps and 10
tasks, logs, 5 WebSocket clients and one expected worker crash, against a real
managed `sqi-server` on SQLite, in about a minute. It runs on PostgreSQL too
when `SQI_TEST_POSTGRES_DSN` points at a server (the harness uses the
database that DSN names, as it is). A second test, `TestLoadSmoke_VerifierCanFail`,
corrupts one expectation and checks that the verifier fails the run.

The run directories are temporary unless `SQI_LOAD_OUT` names a directory to
write them to (CI sets it, to upload them). A skipped test verifies nothing:
look for `--- PASS: TestLoadSmoke` in the output.

### Any scenario

```sh
make load SCENARIO=ci-medium              # one run; SCENARIO defaults to mixed-medium
make soak DURATION=8h                     # a soak; SCENARIO defaults to soak-steady here
make load-sweep SCENARIO=workers-ramp VARY='workers[0].count=500,1000,2000'
```

These wrap `go run ./cmd/sqi-load`, which you can also call directly. `go
run` turns any non-zero exit status into 1, so through `make` an invariant
failure and an INVALID run both exit 1; build the binary (`go build -o
sqi-load ./cmd/sqi-load`) when a script needs to tell them apart (see
[Exit codes](#exit-codes)).

```sh
go run ./cmd/sqi-load scenarios                       # list the built-ins
go run ./cmd/sqi-load scenarios --show ci-medium      # one, as resolved YAML
go run ./cmd/sqi-load run --scenario smoke
go run ./cmd/sqi-load run --scenario my-scenario.yaml --set workers[0].count=500
go run ./cmd/sqi-load compare runs/<a> runs/<b>
```

Each run writes a directory `runs/<run-id>/` (`--out` changes the parent). The
file to open first is `summary.md`. Binaries the managed target builds are
cached under `<out>/.bin`, keyed by git commit and rebuilt when tracked files
have changes (untracked files do not count). Runs that share the directory
take turns building, and a rebuild never overwrites a binary another run is
still running.

### PostgreSQL

Give a managed run a PostgreSQL store with `--set server.store=postgres` and
`--postgres-dsn`; one without the other is refused (exit 3), since a DSN on a
SQLite run would otherwise be ignored. A DSN that names the `postgres`
database is treated as an administrative one: the harness creates a
throwaway database named `sqi_load_<run-id>` for the run and drops it
afterwards. A DSN naming any other database is used as it is and left
holding the run, so it must be empty: the run is refused (exit 3) when that
database has any table in its `public` schema, which keeps a server's own
database from being migrated and filled with a load farm.

```sh
go run ./cmd/sqi-load run --scenario ci-medium \
  --set server.store=postgres \
  --postgres-dsn 'postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable'
```

### Attach mode

To drive a server somebody else runs (possibly remote, possibly on an external
NATS cluster), give `--server` and `--nats`:

```sh
sqi-load run --scenario ci-medium \
  --server https://sqi.example.net --nats nats://sqi.example.net:4222 \
  --api-key "$SQI_KEY" --join-token "$JOIN_TOKEN"
```

| Flag | Meaning |
|---|---|
| `--server URL` | The server's HTTP URL. Switches the run to attach mode. |
| `--nats URL` | The server's NATS URL. Required with `--server`. |
| `--api-key KEY` | API key sent as `Authorization: Bearer` on every REST call and WebSocket connection, for a server with the [auth gate](auth.md) on. |
| `--join-token TOKEN` | A reusable join token the simulated workers enroll with. Required unless the broker needs no credentials. |
| `--no-auth` | The broker needs no credentials, so no join token is needed. |
| `--data-dir DIR` | The server's data directory, when the generator can read it, for disk measurements. |
| `--ca-file FILE` | CA certificate the server's TLS certificates chain to. |
| `--keep` | Leave the run's farm on the server afterwards. |
| `--postgres-dsn DSN` | The attached server's database, so its size can be measured. |

Things to know before attaching:

- The harness creates its own farm, `load-<run-id>`, and works only inside it,
  so it never touches existing jobs. It deletes the farm (and the jobs and
  workers in it) at the end unless `--keep` is given.
- Usage pools are global, not per farm, and a job claims one by name, so
  the harness never uses the scenario's pool names as they are: a run's pools
  are its own, named `load_<run-id>_<pool>` (characters other than letters,
  digits and `_` become `_`), with the scenario's limits, and the job
  templates claim those names. A production pool called `arnold` is never
  touched by a scenario's `arnold`. If a pool of the run's own name already
  exists (a leftover of an earlier run with the same run ID), the run refuses
  to start (exit 3); remove the leftover with [`sqi-load cleanup`](#cleaning-up-after-an-attached-run).
  The shards of one sharded run share their run's pools.
- The scenario's `server` section does not apply: an attached server keeps
  its own store, authentication, TLS and configuration. The harness logs a
  warning naming every `server.*` setting it is ignoring, and the record's
  environment gives the store as `attached (unknown)`, or `postgres
  (attached)` when `--postgres-dsn` names the server's database.
- Server-process measurements come only from `/metrics`: no disk sizes without
  `--data-dir`, and no JetStream stream sizes when broker authentication is on
  (the observer connects anonymously). The report lists what was unavailable.
- Scenarios with `workers[].real` need a managed target and are rejected in
  attach mode. `--server-bin` and `--worker-bin` apply only to a managed run.
- Managed runs turn the HTTP rate limiter off (`http.rate_limit.enabled:
  false`). An attached server keeps its own: 20 requests per second per
  source address, bursts of 40, on every `/api/v1` route. The harness rides it
  out for its own calls: setting up the farm, enrolling simulated workers,
  the timed actions, the invariant checks and the teardown all retry a 429 or
  503 answer, waiting as the server's `Retry-After` says, for up to two
  minutes per call, so setup and teardown are slower but complete. The
  invariant checks also retry a read that fails with a connection error or
  any other 5xx, within the same two minutes. Job
  submissions are never retried: a 429 is counted, raising the `rate-limited`
  and `submissions-rejected` findings, because the throttled submissions are
  load the scenario offered and the server did not take. REST pollers count
  a 429 as an error. To measure an attached server at more than about 20
  submissions per second, turn its limiter off or shard the run across
  several source addresses.

#### Cleaning up after an attached run

A run kept with `--keep`, or one whose teardown failed (it ends with exit 3
and "farm load-... may be left on the server"), leaves its farm and pools on
the server. Remove them with the run's ID:

```sh
sqi-load cleanup --server https://sqi.example.net --api-key "$SQI_KEY" --run-id 20261104-120000-ab12
```

It deletes the farm `load-<run-id>` (or a sharded run's farms
`load-<run-id>-s<i>`) with its jobs, workers and queues, waiting up to two
minutes for workers still online, then the run's usage pools
`load_<run-id>_*`. Nothing else is touched: a pool of another load run whose
farm is still on the server is left alone even when its name starts the same
way, and the server refuses to delete a pool a job still claims. It takes
`--ca-file` for a server with a private CA, prints what it removed, and exits
0, or 3 when something could not be removed.

### Common flags

| Flag | Default | Meaning |
|---|---|---|
| `--scenario` | required | A built-in name or a scenario file. |
| `--set path=value` | | Override any scenario field; repeatable. |
| `--duration D` | scenario's | Override `duration`. |
| `--seed N` | derived from the run ID | Seeds every random choice. The seed is printed, so a run is reproducible. |
| `--out DIR` | `runs` | Where run directories go. |
| `--run-id ID` | `<UTC yyyymmdd-hhmmss>-<4 hex>` | Up to 64 letters, digits and dashes, not ending in `-s<digits>` (a shard's farm ends that way). Shards of one run share it. |
| `--shard i/n` | | Run one slice of the scenario; see [Sharding](#sharding-across-machines). |
| `--server-bin`, `--worker-bin` | built from source | Binaries of a managed run. Swapping `--server-bin` between two runs is how two server revisions are compared. |
| `--sample D` | `5s` | Observation sample interval. |
| `--profile-every D` | 15m in a soak; start and end only otherwise | Heap and goroutine profile period. |
| `--cpuprofile FILE` | | CPU profile of `sqi-load` itself, not the server. |
| `--allow-saturated` | off | Exit 0 for an INVALID run whose generator saturated (the finding is still recorded). |

The first Ctrl-C stops everything and writes the record with `complete: false`
and the invariants marked "not run (interrupted)". A second Ctrl-C aborts the
teardown waits and kills what is left.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | Valid, complete, and every invariant held. |
| 1 | An invariant failed. This wins over the others. |
| 2 | The run is INVALID or it was interrupted. INVALID means the generator saturated, or the scenario submits jobs but the server accepted none of them (a key without `jobs.write`, a template the server refuses, every submission throttled), so every invariant held vacuously. `--allow-saturated` accepts a saturated generator, but not an incomplete run nor one with no accepted submission. |
| 3 | A usage, setup or teardown error: a bad flag, a bad scenario, a target that would not start, a teardown that failed (the run's farm may be left on an attached server), or a cleanup that could not remove everything. |

When several apply, 1 wins over 3, which wins over 2. Findings never change
the exit status, whatever their severity: an `error` finding says the
result is unreliable or points at a defect, and is for a person to read in
`summary.md`. These are the exit statuses of the `sqi-load` binary; through
`go run`, and so through `make load` and `make soak`, every non-zero status
comes back as 1.

`sweep` exits 0 once every point has run, whatever the knee: points past the
knee are expected to fail.

---

## Scenario reference

A scenario is a YAML file. Built-ins are embedded in the binary and run by
name; `--scenario path.yaml` runs your own. An unknown key is an error, so a
misspelt field is reported. `--set` overrides any field after
parsing and before validation; it parses the value as YAML into the field's
type, so anything a file can say about a field, `--set` can too:

```sh
--set duration=10m
--set workers[0].count=500
--set jobs[1].task.fail_rate=0.05
--set 'jobs[0].task.duration={dist: lognormal, median: 30s, p95: 90s}'
--set workers[0].tags.gpu=true
--set server.config.scheduler.heartbeat_timeout=45s
```

Paths use YAML keys, `[n]` indexes a list, and a map is addressed by a key
after it. Validation errors name the offending field's path, for example
`jobs[0].queue: "comp2" is not in farm.queues`, and a scenario with several
problems reports all of them.

### Value forms

| Kind | Written as | Example |
|---|---|---|
| Duration | A Go duration string | `90s`, `5m`, `1h30m` |
| Rate | `<positive number>/<s, m or h>` | `2/s`, `0.12/s`, `120/m`, `7200/h` |
| Distribution | See below | `20s`, `200`, `{dist: lognormal, median: 20s, p95: 90s}` |
| Fraction | `{fraction: <0..1>}` | `{fraction: 0.1}` |

**Distributions** (`task.duration`, `task.log_lines`) take four forms:

| Form | Meaning |
|---|---|
| `20s` | A constant duration. |
| `200` | A constant count. |
| `{dist: uniform, min: 5s, max: 1m}` | Uniform between `min` and `max`. |
| `{dist: lognormal, median: 20s, p95: 90s}` | Lognormal with the given median and 95th percentile. |

A map without a `dist` key is lognormal when it has `median` and `p95`, and
uniform when it has `min` and `max`. The numbers in one map are all durations
or all plain numbers, never a mix. `task.duration` must be a duration
(`20s`, not `20`); `task.log_lines` must be a plain count (`200`, not `20s`).
A lognormal needs `median > 0` and `p95 >= median`; a uniform needs
`0 <= min <= max`. Every draw is from the run's seeded generator.

### Top level

| Field | Default | Meaning |
|---|---|---|
| `name` | required | The scenario's name. |
| `description` | empty | One line shown by `sqi-load scenarios`. |
| `duration` | required, above 0 | How long jobs are submitted. After it, submissions stop and the run waits for the jobs to finish (the drain) before checking invariants. A load run's hard stop; a soak's run length. |
| `warmup` | `0` | A prefix of the run excluded from throughput rates and from growth slopes. Must be shorter than `duration`. |
| `drain_timeout` | `10m` | How long, after submissions stop, every job has to reach a terminal status. |
| `server` | see below | The managed target's configuration. |
| `farm` | one queue, `default` | Queues and usage pools to create. |
| `workers` | none | Worker groups. |
| `jobs` | none | Job classes. |
| `job_deps` | `0` | The fraction (0 to 1) of jobs submitted with `depends_on` an earlier accepted job. |
| `clients` | none | WebSocket clients and REST pollers. |
| `actions` | none | Timed events. |
| `assert` | none | A map of name to non-negative duration. It is accepted and validated and recorded with the scenario, but nothing evaluates it. CI never uses it. |

### `server` (managed target only)

| Field | Default | Meaning |
|---|---|---|
| `store` | `sqlite` | `sqlite` or `postgres` (needs `--postgres-dsn`). |
| `broker_auth` | `false` | Turn on NATS broker authentication. The harness configures a reusable join token and enrolls every simulated worker through `POST /api/v1/workers/enroll`. |
| `http_auth` | `false` | Turn on the [auth gate](auth.md) with a first-admin bootstrap. The harness logs in once, creates an API key and sends it on every REST call and WebSocket connection, so jobs are owned by that key's user. Per-user identities are not simulated. |
| `tls` | `false` | TLS on the REST listener and the embedded broker, with a certificate the harness generates. |
| `config` | none | Extra `sqi-server` configuration merged over the harness's, key by key. |

`server.config` takes real `sqi-server` configuration keys (see
[Configuration](configuration.md)) and is checked: an unknown key is rejected
(the server refuses to start on it), and so are the keys the harness owns,
with a message naming the scenario field that controls it instead
(`http.addr`, `http.tls`, `nats.mode`, `nats.embedded.addr`,
`nats.embedded.data_dir`, `nats.embedded.tls`, `nats.auth.enabled`,
`store.driver`, `store.sqlite_path`, `store.postgres.dsn`, `auth.enabled`,
`auth.bootstrap`, `log.file`).

What a managed server is started with: loopback ports chosen by the harness,
the embedded broker, state under the run directory, `http.enable_pprof: true`,
`http.rate_limit.enabled: false`, discovery off, a diagnostics buffer of 1000,
and the scenario's security options. Everything else is the server's own
default, which is what a run measures.

### `farm`

| Field | Default | Meaning |
|---|---|---|
| `queues[].name` | required | Queue name; unique. |
| `queues[].max_concurrent_tasks` | `0` (unlimited) | The queue's concurrency cap. |
| `usage_pools[].name` | required | Pool name; unique. |
| `usage_pools[].limit` | `0` | The pool's limit. A task of a class with `usage_pool` claims one unit. |

An empty `queues` list becomes one queue named `default`.

### `workers[]`

| Field | Default | Meaning |
|---|---|---|
| `name` | none | Group name; unique. |
| `count` | `0` | Simulated workers. `count + real` must be above 0. |
| `real` | `0` | Real `sqi-worker` processes (managed target only). Only `name`, `real`, `queues`, `tags` and `heartbeat` apply to them. |
| `ramp` | none | Rate at which the group connects and registers, for example `200/s`. |
| `cpu` | `0` | Cores the worker reports. There is no default, so set it. A task with no core requirement costs the whole worker, so a worker runs one such task at a time; give a class `cores` for concurrency per worker. |
| `ram_mb` | `0` | RAM the worker reports. |
| `os` | `linux` | OS the worker reports. The architecture is fixed. |
| `tags` | none | Capability tags, for example `{arnold: "true"}`. |
| `queues` | empty | Queues the group leases from. Empty means every queue. |
| `heartbeat` | `15s` | Heartbeat interval. |
| `churn` | none | See below. |

**Churn** applies per worker, as independent random events over the run:

| Field | Default | Meaning |
|---|---|---|
| `crash_per_hour` | `0` | Expected crashes per worker per hour. A crash drops the connection without deregistering and stops heartbeats. |
| `restart_after` | `0` | How long a crashed worker stays down. It then reconnects with a new instance ID, so the server reclaims the old attempts. |
| `disconnect_per_hour` | `0` | Expected disconnects per worker per hour. A disconnect closes and reopens the connection with the same instance (a NATS reconnect). |

Give `restart_after` a realistic value (seconds). An instant restart can race
with a lease request still in flight from the old process; see
[Known server defects](#known-server-defects).

### `jobs[]`

Each class submits on its own schedule. Set exactly one of `rate` and `total`.

| Field | Default | Meaning |
|---|---|---|
| `name` | none | Class name; unique. |
| `rate` | none | Submissions per second, paced open loop: a late submission is recorded as lag, never silently skipped. |
| `total` | `0` | Submit this many jobs as fast as the server accepts them, with a bounded number in flight. |
| `queue` | the first queue | The queue the jobs go to. |
| `steps` | `1` | Steps per job; step i+1 depends on step i. |
| `tasks_per_step` | required, at least 1 | Tasks each step expands to. |
| `chunk` | `1` | Frames per task. Above 1, the template declares `TASK_CHUNKING`. |
| `cores` | `0` | `amount.worker.vcpu` minimum per task. `0` means the whole worker. |
| `usage_pool` | none | A pool from `farm.usage_pools`. |
| `expr` | `false` | Build the template with the `EXPR` extension (a `let` binding per step). |
| `task` | see below | How simulated workers run the class's tasks. |

Limits on shape. `steps x tasks_per_step` may not exceed 1,000,000, sqi's
limit on tasks per job. sqi also caps each task parameter at 1,024 values, so
the template spreads a step over two parameters, `Frame` and `Block`, whose
product is exactly `tasks_per_step`. Validation rejects a count that cannot be
split that way (a large prime, say), naming the divisor to aim for.

`task`:

| Field | Default | Meaning |
|---|---|---|
| `duration` | constant `5s` | How long a task runs. A distribution. |
| `fail_rate` | `0` | Probability (0 to 1) that an attempt fails. It applies to every attempt, since assignments carry no attempt number, so retries can fail too. |
| `hang_rate` | `0` | Probability (0 to 1) that a task runs until canceled or timed out. A scenario with `hang_rate > 0` gives the task an `onRun` timeout of max(60 s, 3 x the duration's upper value), and a hung task ends `failed` at the timeout like a real one. The lognormal tail beyond 3 x p95 can time out a non-hung task. |
| `log_lines` | constant `0` | Lines per task. A distribution; a plain count. |
| `log_line_bytes` | `120` | Bytes per line. |
| `progress_every` | `0` (off) | How often a running task writes an `openjd_progress` line, carrying its elapsed fraction of `duration` as a percentage. The lines go through the real worker interceptor, so its 2 s throttle decides how many become progress snapshots on the wire. Must not be negative. |

### `clients`

| Field | Default | Meaning |
|---|---|---|
| `websocket.count` | `0` | WebSocket clients speaking the real envelope protocol on `/api/v1/ws`. They measure event lag (server event time to receive time) and count gaps in the sequence as drops. |
| `websocket.subscribe` | none | Any of `jobs`, `workers`, `own-job-tasks`, `own-task-logs`. An unknown name fails the run at start. |
| `rest_pollers.count` | `0` | REST pollers. |
| `rest_pollers.interval` | none; must be positive when there are pollers | Time between polls per poller. |
| `rest_pollers.endpoints` | none | Any of `jobs-list`, `job-detail`, `task-list`, `workers-list`. Latency is recorded per endpoint. |

### `actions[]`

Timed events, `at` seconds into the run (not after `duration`). Each names at
least one thing to do:

| Field | Meaning |
|---|---|
| `at` | When, as a duration from the start of the run. |
| `cancel_jobs: {fraction: f}` | Cancel a random share of the jobs accepted so far. |
| `retry_failed: true` | Retry every job that is `failed`. |
| `pause_queue: NAME` | Pause a queue. It must be one of the farm's queues. A paused queue leases nothing. |
| `resume_queue: NAME` | Resume a queue. It must be one of the farm's queues. |
| `disable_workers: {fraction: f}` | Disable a random share of the workers. There is no action to enable them again. |

When submissions stop, the runner resumes every queue an action paused and
no later `resume_queue` resumed, before it waits for the jobs to finish (the
resume is recorded like an action). A queue left paused would otherwise hold
its jobs until the drain timeout, and the `terminal` invariant would report
them as stranded by the server.

### A complete example

```yaml
name: my-mixed
description: 300 workers, two job classes, churn and a mid-run cancel, 15 min
duration: 15m
warmup: 1m

farm:
  queues: [{name: render}, {name: comp, max_concurrent_tasks: 50}]
  usage_pools: [{name: arnold, limit: 100}]

workers:
  - name: linux-render
    count: 300
    ramp: 100/s
    cpu: 8
    os: linux
    tags: {arnold: "true"}
    churn: {crash_per_hour: 0.2, disconnect_per_hour: 0.4, restart_after: 30s}

jobs:
  - name: shot-render
    rate: 0.12/s
    queue: render
    steps: 2
    tasks_per_step: 20
    cores: 2
    usage_pool: arnold
    task:
      duration: {dist: lognormal, median: 10s, p95: 40s}
      fail_rate: 0.01
      log_lines: {median: 100, p95: 500}
  - name: comp
    rate: 0.07/s
    queue: comp
    tasks_per_step: 10
    cores: 1
    task:
      duration: {dist: uniform, min: 2s, max: 10s}
      log_lines: 30

clients:
  websocket: {count: 20, subscribe: [jobs, workers, own-job-tasks, own-task-logs]}
  rest_pollers: {count: 5, interval: 5s, endpoints: [jobs-list, job-detail]}

actions:
  - at: 5m
    cancel_jobs: {fraction: 0.1}
```

---

## The scenario library

Each built-in isolates one axis, plus mixed ones. `sqi-load scenarios` lists
them and `scenarios --show NAME` prints one as resolved YAML. Every scenario
runs on SQLite by default; `--set server.store=postgres` runs it on
PostgreSQL. Sizes are starting points.

| Scenario | Axis | Shape | Duration |
|---|---|---|---|
| `smoke` | all, tiny | 20 workers (8 cores each); 50 jobs of 2 steps x 10 tasks (1,000 tasks) with 20 log lines each; 5 WebSocket clients; crash rate sized for about one crash with a 10 s restart. Run by CI (`load-smoke`). | 1m |
| `workers-ramp` | nodes | 10,000 idle workers ramping at 250/s, then 1 job/s of 10 tasks; 2 pollers on `workers-list`. Heartbeat and parked-lease cost. | 20m |
| `workers-churn` | nodes | 2,000 workers with 6 crashes and 12 disconnects per worker per hour, 30 s restarts; 5 jobs/s of 2 x 20 tasks. Reclaim correctness. | 30m |
| `jobs-flood` | jobs | 200 workers (16 cores); 100,000 one-task jobs submitted as fast as accepted; 5 pollers on `jobs-list` and `job-detail`. Submission path and `GET /jobs` cost. | 1h |
| `tasks-wide` | tasks | 1,000 workers (32 cores); one job of 1,000,000 tasks (sqi's per-job ceiling), 1 s each. Submission write time and drain rate. | 1h |
| `steps-deep` | dependencies | 500 workers; 1,000 jobs of 20-step chains of 5 tasks, 30% of them depending on an earlier job (`job_deps: 0.3`). | 45m |
| `queues-many` | queues and pools | 200 queues with caps of 0, 10, 25, 50 and 100, 50 usage pools of 20 to 400, 500 workers, ten classes at 0.5 job/s each. Policy-gate cost. | 30m |
| `logs-heavy` | logs | 500 single-core workers; 1 job/s of 10 tasks, each 60 s long writing 8,000 lines of 128 bytes (about 1 MB per task); 10 WebSocket clients on `own-task-logs`. `SQI_LOGS` growth and eviction. | 1h |
| `progress-heavy` | task progress | 200 single-core workers; 0.15 jobs/s of 10 tasks (1.5 tasks/s against the 1.67 tasks/s the workers run, so about 180 are running), each 120 s long writing `openjd_progress` every second, which the worker throttles to about one snapshot per 2 s. Progress snapshot write rate and its cost to the server. Measurements: [Task progress snapshots](#task-progress-snapshots). | 30m |
| `ui-heavy` | UI | 500 workers; 3 jobs/s of 2 x 20 tasks; 1,000 WebSocket clients and 100 REST pollers on all four endpoints. Fan-out, drops and read latency. | 30m |
| `expr-mix` | submission CPU | 300 workers; `EXPR` templates (2 steps x 100 tasks at 2/s, 4 steps x 500 tasks at 0.2/s) beside plain ones (1/s). | 30m |
| `ci-medium` | mixed | 300 workers (8 cores); `shot-render` at 0.12/s (2 steps x 20 tasks, 2 cores, usage pool `arnold` limit 100) and `comp` at 0.07/s (10 tasks), about 5.5 tasks/s offered; churn, 20 WebSocket clients, 5 pollers, a 10% cancel at 5 min and a retry at 10 min. Sized to stay valid on a 4-vCPU CI runner. Run nightly (`load-nightly`). | 15m |
| `mixed-medium` | mixed | 1,800 simulated and 2 real workers (32 cores); `shot-render` at 1/min (2 x 100 tasks, 8 cores, pool limit 200) plus 3 jobs of 1,000 whole-worker tasks, about 5 tasks/s in all; churn; 200 WebSocket clients and 20 pollers; a cancel at 10 min and a retry at 20 min. The `make load` default. | 30m |
| `soak-steady` | time | 500 workers (8 cores); `shot-render` at 0.11/s (2 x 20 tasks) and `comp` at 0.06/s (10 tasks), about 5 tasks/s; light churn, 20 WebSocket clients, 5 pollers; warmup 10 m. Growth detection. The `make soak` default. | 8h |

Sizing of `ci-medium`, on WSL pinned to 4 CPUs of an Intel Core i7-10750H:

- At its rates (about 5.5 tasks/s offered, about 3.8 in the steady state), a
  5 minute run peaks at 0.27 generator cores and 2.3 server cores, VALID, with
  all six invariants holding.
- At 0.16 and 0.10 jobs/s (about 7.4 tasks/s) the server is near its knee.
- At 1 and 0.5 jobs/s the server needs about 3.4 cores, leaves a 4,584-task
  backlog, and 228 of 452 jobs are unfinished after the drain.

`mixed-medium` and `soak-steady`, the scenarios `make load` and `make soak`
run on a workstation, are sized from those numbers: each offers about 5
tasks/s, under 70% of that knee, and uses at most 70% of its own capacity
(worker cores and usage-pool limits). Each YAML file shows the arithmetic, and
a unit test checks it.

---

## Reading a report

A run writes `runs/<run-id>/`:

| File | Contents |
|---|---|
| `summary.md` | The human-readable report. Start here. |
| `run.json` | The whole record: the scenario as resolved, the environment (OS, CPUs, sqi version and commit, store, flags with secrets redacted), the run's start, the end of submissions (`window_end`) and its end after the drain, the counters at the end of warmup, at the end of submissions and at the end, the backlog when submissions stopped, every series, every histogram, invariant results and validity. `compare`, `merge` and `sweep` read it. |
| `series.csv` | Every sampled series, point by point. |
| `profiles/` | Server heap and goroutine profiles at the start and end (and periodically in a soak), and one CPU profile (at most 30 seconds) near the middle of the steady window. |
| `target/` | A managed run's server: `server.yaml` (the configuration written for it), `server.log`, `data/` and `workers/N.log` for real workers. |

`summary.md` opens with the run's times: when it started and ended, its
warmup, and when submissions stopped with how long the drain then took. Its
sections are Validity, Throughput, Latency, Server resources, Growth,
Invariants, Findings and Unavailable.

**The measured window** runs from the end of warmup to the end of
submissions (`started + duration`). Throughput rates and generator validity
are judged over it. The drain that follows, while the run waits for the jobs
to finish, is reported on its own and kept out of both: idle drain time
would dilute every rate, and two revisions differing only in drain time
would compare as a throughput change.

### Validity

A run is **INVALID** when the generator itself was the bottleneck, so its
numbers describe the harness rather than the server. In more than 10% of the
samples taken in the measured window, either:

- the generator used more than 85% of the machine's cores, or
- the 99th percentile of its timer lateness (schedule lag) exceeded 1 second.

The reason names every threshold hit and by how much. A run is INVALID too
when its scenario submits jobs and the server accepted none of them: every
invariant then holds vacuously and says nothing about the server. An INVALID
run exits 2; `--allow-saturated` downgrades a saturated generator (not a run
with no accepted submission) to a warning plus a `generator-saturated`
finding. The Validity section also shows the generator's peak CPU and peak p99
schedule lag, so you can see how much headroom a VALID run had. A run cut short
by Ctrl-C or a driver failure is marked INCOMPLETE.

### Throughput and latency

**Throughput** is taken over the measured window: each counter's growth from
the end of warmup to the end of submissions, divided by that window. The
table shows each counter's total at the end of the run (drain included), the
part of it in the window, and the rate. If the counter snapshot at warmup's
end could not be taken, rates are averages from the start to the end of
submissions and the report says so. The counters are `jobs.submitted`,
`tasks.terminal` (distinct tasks that reached a terminal status on the
server), `logs.lines` (log lines the simulated workers emitted in attempts
that ended: the log load offered; the `logs` invariant checks what the server
kept), `ws.events`, `ws.drops`, `ws.errors`, `ws.lag_negative`, `rest.errors`,
`workers.live` (a fleet size, so no rate) and `submit.status.<code>` per HTTP
status (0 is a submission that got no answer).

**Latency** histograms cover the whole run, warmup included, and show n,
p50, p95, p99 and max:

| Histogram | Measures |
|---|---|
| `submit_latency` | The HTTP round trip of a job submission. |
| `submit_lag` | How late a paced submission left the generator. |
| `first_lease` | A job's submission to its first task's lease. A sweep's knee reads its p95. |
| `task_e2e` | One simulated attempt, from its lease reaching the worker to the worker publishing its terminal status. It holds the scenario's sampled task duration, so it describes the scenario's input as much as the server; read `terminal_ingest` and `first_lease` for the server. |
| `terminal_ingest` | How long the server took to store a task's final status: the task's `updated_at` once terminal minus the moment the simulated worker published that status. One sample per task that ended succeeded or failed with a matching simulated report, read during the invariant checks. A clock difference between the generator and an attached server shifts it; a negative value counts as 0. |
| `job_e2e` | A job's submission to its terminal status. |
| `ws_lag` | WebSocket push lag: server event time to receive time. |
| `rest.<endpoint>` | REST poller latency per endpoint. |

### Invariants

Six invariants are checked at the end of every run, after the drain, with the
fleet still up. Any failure fails the run (exit 1), with evidence naming the
job, task, attempt, queue or pool, and at most 20 entries shown. The checks
that read tasks one by one (single-attempt and logs) cover every task up to
about 2,000 and a seeded random sample of that size beyond.

| Invariant | The exact rule |
|---|---|
| `terminal` | Every accepted job is `completed`, `failed` or `canceled` within `drain_timeout` after submissions stop. A job left non-terminal is stranded. |
| `outcomes` | Every task's final server status is the one the simulator decided (succeeded, failed after retries, or canceled). A task the simulator never leased can only be canceled, and a task of a job a cancel action targeted may be canceled whatever was decided. A `completed` job has only succeeded tasks. A job has exactly `steps x tasks_per_step` tasks, so no work is silently dropped. A disagreeing task is re-read for a few seconds before it counts. |
| `single-attempt` | On each sampled task, no two of the server's attempts are held at once; every attempt after the first follows a failed or canceled one (a retry or a reclaim); and a task in a terminal status holds no open attempt. On every task, the simulator never ran two attempts at once: an attempt leased while an earlier attempt of the same task had not ended (succeeded, failed, canceled or crashed) on its worker fails the check, naming both attempts and workers. The server's attempt list cannot show that case: a worker still running an attempt the server already reclaimed (its heartbeats lost under load) or canceled (the cancel lost) leaves the server with sequential attempts and its late report rejected. |
| `logs` | On each sampled task, the lines the server returns for the task's latest attempt equal the lines the simulator sent for it, in order. When the scenario overflows the broker's store on purpose, losing the attempt's oldest whole chunks is allowed and nothing else is. A chunk lost on an unplanned connection drop fails the check: workers publish log chunks with a core-NATS publish, which delivers at most once. |
| `workers-converge` | Within the server's heartbeat timeout plus the slowest worker heartbeat plus 10 s, the farm's online worker count equals the number of workers the harness has running. |
| `caps` | The most attempts the simulator held at once on a queue never exceeded `max_concurrent_tasks` (0 is unlimited), and on a usage pool never exceeded its `limit` (each task claims one unit). A canceled attempt counts as released when the cancel action started, since the server frees the slot when it commits the cancel. |

A late cancel (see [Known server defects](#known-server-defects)) can fail
`caps`: the server's own claim count stays at the limit while concurrent
execution exceeds it.

### Findings

Findings are rules applied to the record. An **error** points at a defect in
sqi or makes the result unreliable; a **warn** is something to look at.
Neither changes the exit status.

| Rule | Severity | Fires when |
|---|---|---|
| `ws-drops` | error | WebSocket clients saw gaps in the push sequence: the server dropped pushes to a client whose buffer was full. |
| `rate-limited` | error | Any submission was answered 429. The server's rate limiter throttled the run; set `http.rate_limit.enabled: false` or use more source addresses. |
| `submissions-rejected` | warn, or error above 1% | Submissions the server did not accept: any status outside 2xx, or no answer. The message counts them per status. Their jobs are not in the run, so it measured less load than the scenario offers. |
| `workers-offline-under-load` | error | The scenario has no churn, yet `workers.online` fell below the live fleet in some sample between warmup and the end of the run. |
| `churn-failed` | error | A churn event failed to restart or reconnect its worker, so the fleet ran short. |
| `goroutine-growth` | warn | Server goroutines rose steadily after warmup (slope above 0, r² above 0.8). |
| `rss-growth` | warn | Server RSS rose with r² of at least 0.5 and projects to more than 1 GiB of growth over 7 days. |
| `wal-never-shrank` | warn | On SQLite, the WAL grew over at least 5 minutes without once shrinking. |
| `stream-at-cap` | warn | The `SQI_LOGS` stream reached 95% of `nats.embedded.max_store_mb`, so the broker evicts the oldest log chunks from here on. |
| `generator-saturated` | warn | The generator saturated but the run was accepted with `--allow-saturated`. |
| `server-fell-behind` | warn | The server did not keep up with the load offered: the drain took more than 25% of the scenario's `duration`, or more tasks were open when submissions stopped than the server finished in the last 10% of the measured window. The run's latencies and slopes then describe a backlog; size the scenario down or the server up. |

The two growth rules need at least 10 samples spanning at least 30 minutes
after warmup, so only soaks and long runs raise them. Every statistic uses
only samples up to the end of the run.

### Unavailable

The report lists every measurement it could not make, with the reason, rather
than leaving it out. Common entries:

- `ws_lag` on Windows: the wall clock steps by about 0.5 ms in Go on a typical
  Windows host, so a push delivered within one step reads 0 (shown as the
  histogram's 1 microsecond floor). WSL and Linux resolve it.
- JetStream stream sizes when broker authentication is on; disk sizes against
  an attached server without `--data-dir`.
- `invariants`, when the run ended early.
- `lease_reply`, always: a lease request parks on the server until work
  appears, so its reply time measures how long the worker sat idle, not how
  fast the server answered. `first_lease` measures dispatch.
- `cpu_profile`, when the steady window is too short for a profile.
- `terminal_ingest`, when no task the server lists as succeeded or failed
  matched a terminal status a simulated worker published (only real workers
  ran the work, say).
- `window_counters` and `backlog`, when listing the farm's jobs at the end of
  submissions, or near it, failed.

### Gauges that mislead

`sched.queue_depth{queue}` is the server's `sqi_scheduler_queue_depth`. The
server sets the gauge of a queue that drains back to `0` on its next sweep, but
only for a queue it has seen since it started: a queue that was already empty
when the server started has no series. The report shows `-` for a queue's end
depth and no finding reads it, so a queue's end depth in the report does not
show whether the queue is stuck or drained; read the series in `series.csv`.

### Comparing runs

```sh
sqi-load compare runs/<a> runs/<b>
```

prints per-metric deltas for the p50, p95 and p99 of every histogram both runs
have, every comparable counter (steady-window rates when both runs carry the
snapshot, otherwise run averages; both over the measured window, never the
drain), and the peaks of the key server series (CPU, RSS, goroutines, open
files, heap, GC pause). A difference of 5% or less either way is reported as
"no change". When either run is INVALID or incomplete, `compare` prints a
warning above the table: the deltas are still shown, but they describe a
saturated generator or a run cut short. Compare runs of the same scenario
on comparable hardware; comparing server revisions with one harness is valid
only while both revisions speak the harness's worker protocol and accept its
configuration keys.

---

## Sweep

A sweep runs a scenario once per value of one parameter, each on a fresh
managed server, and finds where the farm stops coping.

```sh
sqi-load sweep --scenario workers-ramp \
  --vary 'workers[0].count=500,1000,2000,5000,10000'
```

Values run in the order given, so list them ascending; the first is the
baseline. Each point's record goes to `<out>/<sweep-id>/<i>-<value>/`, and the
sweep writes `sweep.csv` (throughput, first-lease p50, p95 and p99, validity
and invariants per value) and `sweep.md` with the knee called out. A value
cannot contain a comma, and a sweep is managed-only.

The knee is the first value where any of these holds:

- the run is INVALID (the generator saturated),
- an invariant failed, or
- the p95 of submit to first lease exceeds twice the first value's p95.

A point with no record is also the knee ("missing record"). If the baseline has
no first-lease data or a zero p95, the latency rule is off and only validity
and invariants decide.

---

## Soak

A soak is a long run at steady load, to find leaks and drift.

```sh
make soak DURATION=8h                        # soak-steady
make soak SCENARIO=workers-churn DURATION=2h
sqi-load soak --scenario soak-steady --duration 8h
```

It differs from `run` in taking heap and goroutine profiles of the server
every 15 minutes (`--profile-every` changes it) and marking the record as a
soak. Any `--duration` is accepted.

After warmup, the Growth section of `summary.md` reports the least-squares
slope per hour, with its R², of server RSS, goroutines and open files, the
SQLite database, WAL and NATS data sizes, the PostgreSQL database size, and
each JetStream stream's bytes. The `rss-growth`, `goroutine-growth`,
`wal-never-shrank` and `stream-at-cap` findings turn the ones that matter into
flags. Of the slopes, only RSS and goroutines raise a finding; read the others
from the table.

Plan for the generator too: the harness's own CPU and goroutines are sampled
for the whole soak, and an INVALID soak is as meaningless as an INVALID load
run.

---

## Sharding across machines

One generator machine simulates a bounded farm. To simulate a larger one, run
several generators against one shared, attached server, each taking a slice of
the scenario:

```sh
# on every generator machine i of n (here 2), same --run-id:
sqi-load run --scenario workers-ramp --run-id big-1 --shard 0/2 \
  --server https://sqi.example.net --nats nats://sqi.example.net:4222 \
  --join-token "$JOIN_TOKEN" --api-key "$SQI_KEY"

# then, with the shard records copied to one place (each shard writes
# runs/<run-id>-s<i>):
sqi-load merge runs/big-1-merged runs/big-1-s0 runs/big-1-s1
```

What a shard does:

- Worker counts (simulated and real), ramp rates, job rates and totals, and
  the numbers of WebSocket clients and REST pollers are divided by `n`, the
  remainders going to shard 0. A job class whose total divides to 0 in a shard
  is left out of it.
- Worker IDs and job names are prefixed per shard (`s0`, `s1`, ...) so shards
  never collide, and each shard creates its own farm, `load-<run-id>-s<i>`. A
  queue's `max_concurrent_tasks` is divided too, with a floor of 1 for a capped
  queue.
- `--shard` requires `--server`: a managed shard would start a server of its
  own, and merging would then add up counters from different servers.
- `merge` needs the records of the same run ID, ideally all `n`; given fewer, it
  says totals are understated and marks the result incomplete. Histograms
  merge, counters add, and the series, environment and scenario come from
  shard 0, so run shard 0 where the server observations (`/metrics`, and
  `--data-dir` if readable) should be taken. The merged scenario carries shard
  0's per-shard counts, not the whole farm's. The merged run is valid and
  complete only if every shard is, an invariant holds only if it held in every
  shard, and findings are recomputed from the merged data, except
  `churn-failed` and `generator-saturated`, which only a live run can see:
  those are carried over from the shards, labeled with their shard.

**Shared pools.** Usage pools are global to the server, so the shards of a run
share the run's pools, `load_<run-id>_<pool>`, and their limits are not
divided. Two consequences:

- A sharded run never deletes the usage pools it created, since one shard
  deleting a pool would pull it from under the others, and the server refuses
  while any claim names it. After every shard has finished, remove them with
  `sqi-load cleanup --run-id <run-id>` (see
  [Cleaning up after an attached run](#cleaning-up-after-an-attached-run)).
  The shards log their names.
- The `caps` invariant is checked per shard, against the pool's full limit, on
  that shard's own attempts. It cannot see two shards together exceed a pool's
  limit. A pool-limit finding on a sharded run needs the server's own claim
  counts (`sqi_usage_active_claims`) alongside.

Starting the machines is the operator's job.

---

## CI

Two jobs run the harness. Neither enforces latency.

**`load-smoke`** in `.github/workflows/ci.yml` runs on pushes and on pull
requests that touch Go code, `go.mod`, `go.sum`, the Makefile, `ci.yml`, or
anything under `internal/loadtest/` or `test/load/` (the scenario YAML included),
after the main `test` job. It runs `make load-smoke` (SQLite) with `SQI_LOAD_OUT` set,
uploads the run directories and the log as the `load-smoke-runs` artifact (7
days), and then asserts by name that `TestLoadSmoke` and
`TestLoadSmoke_VerifierCanFail` both passed, because a skipped or filtered test
would leave `go test` green. It fails only on an invariant failure, an invalid
run or a harness error.

**`load-nightly`** (`.github/workflows/load-nightly.yml`) runs daily at 03:30
UTC and on demand, on GitHub-hosted runners, as a matrix of SQLite and
PostgreSQL 18 (a service container; that leg passes `--set
server.store=postgres` with an administrative DSN, so the run gets a database
of its own). It runs `ci-medium` and uploads the run directories as
`load-runs-sqlite` and `load-runs-postgres` (30 days), with each `summary.md`
added to the job page. A non-zero exit from `sqi-load` (1 an invariant failed,
2 INVALID) fails the job; thresholds are only reported, since shared runners
are too noisy to gate on timings. Compare uploaded directories with `sqi-load
compare`. A new run never cancels one in progress: the scheduled run waits
for a dispatched soak to finish, and the reverse.

The nightly can fail on `logs` with every other invariant holding, and more
rarely on `caps`. `ci-medium` has cancels, churn and disconnects, and core
NATS delivers at most once: log chunks published during a forced disconnect
are lost, and a cancel published while its worker is disconnected arrives
only after the reconnect (see [Known server defects](#known-server-defects)).
The evidence names the tasks and attempts involved.

`workflow_dispatch` takes two inputs: `scenario` (default `ci-medium`) and
`duration`. Giving a duration runs the scenario as a soak of that length, up
to 5 hours 30 minutes, so the job (six hour cap) has room for the build and
the artifact upload. A longer or malformed duration fails the job before
anything builds.

The chaos suite has CI jobs of its own, `chaos-smoke` and a nightly `chaos` job
in the same workflow file; see [Chaos testing](chaos-testing.md#ci).

Every CI job is GitHub-hosted at about 4 vCPU, with no larger or self-hosted
runner, so `ci-medium` is sized to stay valid on one. `mixed-medium` and the
large scenarios are local or self-hosted runs.

---

## Baselines

Reference runs to compare later runs against, taken at the commit and on the
hardware stated below. A different host gives different numbers.

| Host | Store | Scenario | Tasks/s | Submit→first lease p50 / p95 / p99 | Job end-to-end p95 | Peak server RSS | Peak server CPU | Invariants |
|---|---|---|---|---|---|---|---|---|
| WSL | SQLite | smoke | 13.45 | 2.88 s / 9.16 s / 11.04 s | 15.33 s | 130 MiB | 1.47 cores | 6/6 |
| WSL | PostgreSQL 18 | smoke | 14.42 | 3.11 s / 10.23 s / 10.47 s | 16.74 s | 121 MiB | 1.54 cores | 6/6 |
| WSL | SQLite | ci-medium | 5.44 | 326 ms / 967 ms / 1.32 s | 2m50s | 665 MiB | 4.42 cores | 6/6 |
| WSL | PostgreSQL 18 | ci-medium | 5.17 | 12.99 s / 47.11 s / 53.02 s | 3m48s | 657 MiB | 1.81 cores | 6/6 |
| Windows | SQLite | smoke | 13.65 | 2.83 s / 9.09 s / 10.36 s | 14.93 s | 134 MiB | 2.42 cores | 6/6 |
| Windows | SQLite | ci-medium | 5.38 | 284 ms / 849 ms / 1.16 s | 2m57s | 681 MiB | 5.41 cores | 6/6 |

**Hardware.** Both hosts are the same physical laptop, and the runs were
sequential, never concurrent. All runs were `sqi-load run` against a managed
target with default seeds, at commit `365833d2`, on 2026-11-04.

- **Windows:** Intel Core i7-10750H at 2.60 GHz (6 cores, 12 threads), 16 GB
  RAM, a WDC PC SN530 NVMe SSD, Windows 11 Home, native Windows binaries.
- **WSL:** WSL2 Ubuntu (kernel 6.18, microsoft-standard-WSL2) on the same
  machine, with 12 vCPUs, 7.7 GiB RAM and ext4 on the same NVMe. The WSL runs
  were not pinned to a CPU set, so they are not comparable with the
  4-vCPU-pinned `ci-medium` sizing in [The scenario library](#the-scenario-library).
  PostgreSQL 18.6 ran in Docker on the same WSL VM.

There is no Windows baseline on PostgreSQL.

How to read the columns: **Tasks/s** is the rate of `tasks.terminal` over
the measured window, from the end of warmup to the end of submissions. The
latencies are the whole-run histograms. **Peak server CPU** is in cores, so
2.3 on a 4-CPU host is 58% of the machine. It is the `sqi-server` process's
CPU only: on a PostgreSQL row the database runs in its own process, which this
column does not include. **Invariants** is how many of the six held. Every row
is a VALID run: the generator peaked at 0.41 cores or less of 12.

### Observations

Each comes from a single run on one laptop.

- **PostgreSQL schedules far more slowly than SQLite at `ci-medium`** on the
  same host. Submit to first lease has a p50 of 12.99 s against 326 ms (about
  40 times) and a p95 of 47 s against 967 ms. Terminal ingest (the server's
  task `updated_at` minus the worker's terminal publish) has a p50 of 2.54 s
  against 15 ms. Throughput is similar (5.17 against 5.44 tasks/s) because it
  is bounded by the offered load, not by the scheduler. At `smoke` the two
  stores are close.
- **`GET /api/v1/jobs` (50 per page) is slow on PostgreSQL at `ci-medium`:**
  p50 2.79 s against 8.6 ms on SQLite, and p95 3.52 s against 280 ms. See
  question 5 under [Open questions](#open-questions).
- **Server CPU on SQLite is high:** peaks of 4.4 to 5.4 cores on `ci-medium`
  (unpinned) against 1.8 on PostgreSQL. SQLite's single writer runs inside the
  server process, while PostgreSQL's work happens in a separate database
  process that `server.cpu_cores` does not include, so the two figures are not
  a like-for-like measure of total cost.
- **Server RSS** is about 660 to 680 MiB at 300 workers and 120 to 135 MiB at
  20 workers.
- **RSS rises steadily through the 17-minute `ci-medium` run** (about 2 GiB/h,
  r² 0.97 to 0.99 in the growth table). That run is too short for the growth
  rule, which needs 30 minutes of steady samples, so no `rss-growth` finding
  is raised. Whether it plateaus is question 4 under
  [Open questions](#open-questions).
- **The SQLite WAL never shrinks** across the 17-minute WSL `ci-medium` run
  (3.9 to 9.6 MiB), and `wal-never-shrank` warns. See question 9.
- **The generator is cheap at small scale.** Smoke (20 simulated workers)
  costs the harness about 0.1 to 0.3 cores. Its cost at 10,000 workers per
  generator is unmeasured.

### Task progress snapshots

`progress-heavy` measures what a running task's progress snapshots cost the
server. Each simulated task writes `openjd_progress` every second through the
real `internal/worker/openjd` interceptor, whose throttle sends at most one
snapshot per 2 s per attempt, so a farm with about 180 tasks running sends
about 85 snapshots/s. The server records each with `RecordAttemptProgress`.

The scenario is 30 minutes with a 3 minute warmup. Each run below used
`--duration 8m --set warmup=1m` instead (8 minutes of submissions, 1 minute of
warmup, then a drain of 2 minutes). The first minute includes the fleet filling
with work, and growth over time is not covered. "Without" is the same scenario
with `--set jobs[0].task.progress_every=0s`. Each figure is a single run, with
the server on SQLite and on PostgreSQL 18.6 (Docker, same machine).

Host: Apple M2 Max, 12 CPUs, macOS (Darwin 25.6.0), native arm64 binaries.
Every run was valid, with 6 of 6 invariants held and the generator at 0.07
cores or less.

| Store | Progress | Tasks/s | `first_lease` p50 / p99 | `terminal_ingest` p50 / p99 | Server CPU, mean over the window | Server RSS peak |
|---|---|---|---|---|---|---|
| SQLite | with | 1.29 | 25.8 ms / 42.1 ms | 72 ms / 181 ms | 0.218 cores | 128 MiB |
| SQLite | without | 1.29 | 26.0 ms / 38.1 ms | 43 ms / 144 ms | 0.150 cores | 124 MiB |
| PostgreSQL | with | 1.30 | 110.9 ms / 137.9 ms | 263 ms / 627 ms | 0.261 cores | 102 MiB |
| PostgreSQL | without | 1.31 | 113.0 ms / 167.9 ms | 101 ms / 343 ms | 0.141 cores | 91 MiB |

- **Snapshot write rate:** about 85 snapshots/s, derived. The harness has no
  counter for snapshots (`SQI_TASK` is a work-queue stream, so its message
  count is always about 0), so this is the mean of 173 running
  tasks (200 workers less the mean idle count over the window) times the
  interceptor's one-per-2 s throttle (the dispatcher test sees 5 to 7
  snapshots in 10 s of 1 s progress writes).
- **Lease latency:** `first_lease` is the only dispatch-latency histogram (a
  lease request's own reply time is idle wait and is not reported). It has one
  sample per job, 72 or 73 per run, so its p99 is close to its maximum and a
  single run's ratio is noisy. p99 with progress is 1.10 times the p99 without
  on SQLite and 0.82 times on PostgreSQL. The p50s differ by under 2%.
- **Server CPU:** progress adds 0.068 cores on SQLite and 0.120 cores on
  PostgreSQL (`server.cpu_cores` excludes the PostgreSQL process). Divided by
  85 snapshots/s, that is about 0.8 ms (SQLite) and 1.4 ms (PostgreSQL) of
  server CPU per snapshot, which includes everything the snapshot touches,
  not only the store write.
- **Store write alone:** `BenchmarkRecordAttemptProgress` (SQLite, 500 running
  attempts, 10,000 iterations, one snapshot per iteration round-robin) takes
  72,433 ns/op. The write is under 10% of the per-snapshot CPU above.
- **Remaining-time inputs read:** `BenchmarkQueueJobTimings` (SQLite, one
  queue of 200 unfinished jobs of 100 tasks each, 50 of each job succeeded
  through real attempts and 2 running, 200 iterations, one `QueueJobTimings`
  call per iteration) takes 8,815,144 to 8,987,214 ns/op (about 9 ms). A task's run time is stored on the task row (`tasks.run_ms`) when it
  succeeds, and the per-step counts and measured sums come from the
  `tasks(step_id, status, run_ms)` index alone, so the cost is one index entry
  per task in the queue's unfinished jobs, with no attempt lookups. The queue
  read runs only when a queue list or get call asks for it with
  `include=timing`: the web Queues page asks once per farm, and the submit
  forms, the Python SDK by default and the DCC submitters do not. No cache
  exists.
- **Remaining-time inputs read, one large job:** `BenchmarkJobTimingsLargeJob`
  (SQLite, one unfinished job of 100,000 tasks, 50,000 succeeded through real
  attempts and 2 running, 200 iterations, one `JobTimings` call per iteration)
  takes 21,940,955 to 22,888,588 ns/op (about 22 ms). That is about 0.22 microseconds per task, succeeded or not.
  `GET /api/v1/jobs/{id}` always runs this read for an unfinished job, and the
  web job page refetches at most every 5 s while tasks change, so each open
  view of a job this size spends under 0.5% of one read connection.
  `GET /api/v1/jobs` runs it for the page's unfinished jobs only with
  `include=timing`: the web Jobs page asks, and the submit forms' dependency
  picker, the dashboard counts, the Python SDK by default and the DCC
  submitters do not.
- **Terminal ingest slows:** the median time to store a terminal status rose
  from 43 to 72 ms on SQLite and from 101 to 263 ms on PostgreSQL, and the p99
  from 144 to 181 ms and from 343 to 627 ms. Terminal statuses and snapshots
  share the `SQI_TASK` consumer, so terminal statuses queue behind snapshots.
  `js.pending` stayed at 0 throughout. A last-value-per-attempt subject for
  snapshots would stop them queueing ahead of terminal statuses.
- **Estimated drain of a 10 minute outage backlog:** 200 running tasks times
  300 snapshots each (one per 2 s for 10 minutes) is 60,000 snapshots. At the
  CPU cost per snapshot above, assuming one at a time, the drain is about
  47 s on SQLite and 84 s on PostgreSQL. At the benchmark's 72 microseconds it
  would be 4.3 s. Every figure is under the 10 minutes of outage the backlog
  is sized for, so the farm catches up faster than the outage lasted. These are
  estimates from the mean CPU cost, not a measured outage and recovery.
  The PostgreSQL figure is a lower bound: it divides server-process CPU, which
  leaves out PostgreSQL's own time, instead of measuring per-message wall
  latency. The terminal-ingest p50 above shows that latency per message is
  higher.

---

## Known server defects

Defects in `sqi-server` that a run can hit.

### A cancel sent during a disconnect arrives late

A cancel published while the worker running the attempt is disconnected never
reaches it. The server sends it again on the first heartbeat sweep after the
worker's heartbeats resume and still list the attempt (attempt
reconciliation), so the attempt runs on for up to a sweep interval plus a
heartbeat interval after the reconnect. The server has already released the
attempt's usage-pool claim, so in that window concurrent execution can exceed
a cap while the server's claim count stays at the limit, and `caps` can
fail.

### An instant restart once stranded an attempt

A lease request still in flight from a worker's old process could be served
after a restart's reclaim, leaving the re-leased attempt stranded as running.
Attempt reconciliation now repairs it: the server reclaims an attempt a live
worker's heartbeats stop reporting, after `scheduler.reconcile_grace` (default
2 minutes). `chaos-churn-instant-restart`, whose churn restarts a worker at
once, no longer strands one; it stays in the library as a regression guard.

### A graceful server stop can lose a terminal status (H13f1)

A task's terminal status is a core NATS publish with no acknowledgment, and
the embedded broker stops JetStream before it closes client connections. A
status in flight when the server stops gracefully is stored by no stream, and
the worker, whose publish returned no error, does not retry. The attempt is
reclaimed after the restart and the finished task re-runs. Found by
`chaos-server-term`, where it is a known violation of the rework bound.

### A server stall longer than the heartbeat timeout takes every worker offline (H13f2)

On SIGCONT after a stall longer than `heartbeat_timeout`, the heartbeat sweep
runs before the stall's backlog of heartbeats is consumed, marks every live
worker offline and reclaims their attempts. Work that finished during the stall
is rejected when its report lands and runs again, and an attempt reclaimed
while its lease reply is still in flight can run beside the task's next
attempt until the stale-running cancel stops it. Found by
`chaos-server-stall-long`.

### A worker the sweep marks offline stays offline (H13f3)

A live worker the heartbeat sweep marks offline stays offline while its
connection stays up: heartbeats never bring a worker back online, and it
re-registers only on a reconnect. Found by `chaos-server-stall-long` and
`chaos-worker-sleep`, where `workers-converge` fails.

### Killing sqi-worker on Linux leaves its task's processes running (H13f4)

On a bare-process Linux worker, killing `sqi-worker` leaves the process tree of
its task running. A child with no stdout never gets SIGPIPE, outlives the
worker's restart and keeps writing the task's output while the reclaimed task
re-runs on a live worker. Found by `chaos-worker-kill-mid-task`, where
`no-orphans` and `single-writer` fail on Linux.

### A data race at shutdown in the scheduler (H13f5)

`Scheduler.Stop` reads the scheduler's `cancel` function while
`Scheduler.Run` is still setting it, when the server shuts down right after it
launches the scheduler. The race detector reports it. Found while building the
chaos harness and present on `main`; no scenario depends on it.

---

## Open questions

The large scenarios, sweeps to the knee and an 8 hour soak answer these
questions with numbers. Each needs more hardware than the baselines used. The
Status column gives what the baselines show.

| # | Question | Scenario and signal | Status |
|---|---|---|---|
| 1 | Heartbeat ingest is one serialized store write per heartbeat. At what worker count does N workers / 15 s saturate it on SQLite and on PostgreSQL? The `SQI_WORKER` stream drops messages older than 2 minutes, so saturation means lost heartbeats and workers marked offline. | `workers-ramp` swept over `workers[0].count`; `workers-offline-under-load`, `workers.online`, `server.cpu_cores`. | Unmeasured. |
| 2 | What do parked lease goroutines and timers cost at 10,000 workers across queues, and what does `WakeQueue` waking every waiter on a busy queue cost? | `workers-ramp`; `server.goroutines`, `server.rss_bytes`, `first_lease`. | Unmeasured. |
| 3 | `sweepUnschedulable` considers at most 1,000 online workers. Above 1,000 workers, are tasks wrongly flagged unschedulable? | `workers-ramp`, `mixed-medium` (1,800 workers); watch for tasks flagged unschedulable that the fleet could run. | Unmeasured. |
| 4 | Per-worker `sync.Map` entries are never pruned. Does churn grow server memory without bound in a soak? | `soak-steady` or `workers-churn` as a soak; `rss-growth`, `goroutine-growth`. | Unmeasured. The 15 minute `ci-medium` run is too short for the growth rules, which need 30 minutes of steady samples. In the 17-minute baseline runs RSS rises at about 2 GiB/h (r² 0.97 to 0.99). |
| 5 | `GET /api/v1/jobs` issues two count queries per job on the page. How does read latency grow with job count? | `jobs-flood`; the `rest.jobs-list` histogram. | Unmeasured as job count grows. At `ci-medium` the baseline has `GET /jobs` (50 per page) at p50 2.79 s on PostgreSQL against 8.6 ms on SQLite, so the cost is visible on PostgreSQL at a modest job count. |
| 6 | WebSocket clients silently drop pushes when their 64-message buffer is full. At what fan-out does `ui-heavy` start dropping? | `ui-heavy` swept over `clients.websocket.count`; the `ws-drops` finding and `ws.drops`. | Unmeasured at fan-out. The drop counting itself is tested. |
| 7 | SQLite serializes writes behind a large submission (1,000,000 tasks takes about 1 min 20 s). What happens to heartbeats and status ingest meanwhile? | `tasks-wide` on SQLite and on PostgreSQL; `submit_latency`, `workers-converge`, `workers.online`. | Unmeasured. |
| 8 | How does `SQI_LOGS` growth toward `max_store_mb` (1024 by default) and eviction behave, and does log ingest keep up with `logs-heavy`? | `logs-heavy`; `stream-at-cap`, `js.bytes{SQI_LOGS}`, the `logs` invariant. | Unmeasured. |
| 9 | How large does the WAL grow between the 5 minute PASSIVE checkpoints under sustained writes? | Any SQLite run of 5 minutes or more; `disk.sqlite_wal_bytes`, `wal-never-shrank`. | In the baseline the WAL never shrinks across the 17-minute WSL `ci-medium` run (3.9 to 9.6 MiB) and `wal-never-shrank` warns. One run at this scale does not say how large it gets under longer sustained writes. |
| 10 | The diagnostics buffer has a 512-component ceiling. What happens with more than 512 workers publishing diagnostics? | Not testable with the harness as built. | Not covered: simulated workers do not publish worker diagnostics (`worker.diag.<workerID>`), so this needs either a simulator addition or real workers. |

Also unmeasured: the generator's own cost at 10,000 workers per process (the
design target), and the knee of each sweep.

---

## See also

- [Observability](observability.md): server metrics, including the five metric
  families declared but not recorded that the harness measures from the
  client side instead.
- [Chaos testing](chaos-testing.md): the fault-injection suite built on this
  harness.
- [Operations](operations.md): running and sizing `sqi-server`.
- [Configuration](configuration.md): the keys `server.config` accepts,
  including `http.rate_limit.enabled`.
- [Development](development.md): the Makefile targets and the test suites.
