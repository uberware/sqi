<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
# Chaos testing

The chaos suite injects faults into a running sqi farm and checks that the
farm still converges: no job stranded, no completed work done twice, no task
process left behind, no database left inconsistent. It is built on the
[load harness](load-testing.md): a chaos scenario is a load scenario with a
`faults` list, an `expect` section and, for some job classes, a `real_task`.

This page covers what the suite is, how to run it, the scenario format for
faults, the eleven invariants and the rework bound, how known defects are
recorded, the scenario library, the CI jobs and what comes next.

---

## What it is, and what it is not

The suite runs real `sqi-server` and `sqi-worker` processes, so a fault is
real: a server is killed with SIGKILL, a worker is frozen with SIGSTOP, a
network path is cut by a TCP proxy that sits between the workers and the
broker. A scenario fires its faults at fixed times while jobs are being
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
- **A test of anything outside the portable core.** This first wave covers
  server, worker and network faults on SQLite or PostgreSQL. Database faults,
  resource limits, identity-provider faults, a container target and a Windows
  nightly come later (see [What comes next](#what-comes-next)).

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
make chaos                                # all 19, about two hours
```

These run the suite as a Go test (build tag `chaos`, never part of `make
test`), in `test/chaos`. Each scenario is a subtest of `TestChaos`. The
scenarios are in the load harness's built-in library, so
`go run ./cmd/sqi-load scenarios` lists them with the rest and `scenarios
--show chaos-net-blip` prints one as resolved YAML.

| Environment variable | Meaning |
|---|---|
| `SQI_LOAD_OUT` | A directory the run directories are written to, so they survive the test and CI can upload them. Without it each scenario writes to a temporary directory the test removes. |
| `SQI_CHAOS_STORE` | `sqlite` (default) or `postgres`. PostgreSQL needs `SQI_TEST_POSTGRES_DSN`, which names an empty database the harness uses as it is; without it the test skips, or fails when `SQI_TEST_REQUIRE_POSTGRES=1` is set. |

The test logs each run's directory, and on a failure its `summary.md`. A
skipped test verifies nothing: look for `--- PASS: TestChaos/chaos-...` lines
in the output.

**How long it takes.** A scenario's own `duration` is 3 to 4 minutes 45
seconds, but the server marks no worker offline for about 3 minutes 10
seconds after it starts, and the faults that depend on a worker being marked
offline fire after that, so a run is several minutes longer than its faults
need. Measured on Linux (WSL2): `chaos-server-kill-20s` 240 s,
`chaos-worker-gone` 232 s and `chaos-net-blip` 321 s, 13.2 minutes for the
three, plus the first binary build. The full suite of 19 takes about two
hours (an estimate from those three; `make chaos` allows three).

**Platforms.** Linux is the platform the suite is built for. On Windows the
scenarios that stall a process are skipped (SIGSTOP and SIGCONT do not exist
there); the rest run, killing a process with `TerminateProcess`. The known
violations for the leftover process tree are limited to Linux (`os: [linux]`).
On a Windows workstation, run the suite under WSL2.

---

## How a chaos run differs from a load run

A run is a **chaos run** when the target can inject faults (a managed target
does, an attached server does not) and the scenario has any of: a fault, a
`max_rework_attempts` bound, a known violation, or a job class with a
`real_task`. A chaos run does everything a load run does, plus:

1. Fires each fault at its time, concurrently with the others and with the
   submissions. `at` counts from the start of submissions.
2. Waits for every fault to heal before it starts the drain.
3. Finds the jobs the server created for submissions it gave no 2xx answer
   (a timeout, a dropped connection, a 5xx), called **orphans**, and waits for
   them too.
4. Checks the six load invariants with the changes described under
   [Invariants](#invariants), then `submission-atomic`.
5. Stops the real workers and then the server gracefully, keeps the database,
   and reads it to run `store-integrity` and `executions-recorded` and to
   measure [rework](#rework).
6. Reads the real tasks' markers to run `no-orphans` and `single-writer`.
7. Applies the scenario's `expect` section: the rework bound and the known
   violations.

The test passes when the record is complete and every result is OK once the
known violations are applied. A record marked invalid is logged but does not
fail the test: `chaos-server-kill-mid-submit` is invalid on purpose, since
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
| `duration` | stall, partition, blackhole, latency, flap | How long the fault lasts before it heals. Must be positive for these kinds. It is rejected on kill, term and power_cut. |
| `restart_after` | kill, term | How long after the process is stopped it is started again. `0` leaves a worker down for good; a server must be restarted, so it must be positive for one. It is rejected on every other kind. |
| `kill` | | `{server: true}` or `{group: NAME, real: N}`. SIGKILL (`TerminateProcess` on Windows). A killed worker leaves the tasks it started running. |
| `term` | | The same targets. A graceful stop, the way the process shuts down on an upgrade. |
| `stall` | | The same targets. SIGSTOP, then SIGCONT when the fault heals. A stalled server is stopped with its embedded broker; a stalled worker is stopped together with the process group of every task it is running, as a machine that goes to sleep stops. POSIX only. |
| `partition` | | `{groups: [...]}`. Cuts the groups' workers from the broker: every connection is closed and new ones are refused until the fault heals. |
| `blackhole` | | `{groups: [...]}`. Keeps the connections open but forwards nothing in either direction. A connection accepted during the fault is connected to the broker when it heals. |
| `latency` | | `{groups: [...], delay, jitter, bytes_per_sec}`. Delays every forwarded chunk by `delay` plus or minus `jitter` (order kept) and caps each direction at `bytes_per_sec` (0 is unlimited). |
| `flap` | | `{groups: [...], period}`. Cuts the groups for half of each `period` and opens them for the other half, for `duration`, and ends open. |
| `power_cut` | | `{order, down, gap}`. Kills the server and every running real worker and crashes the simulated fleet, all at once. After `down` it brings back the first side, and after `gap` more the second. `order` is `server-first` or `workers-first`. |

A process target names `server: true`, or a worker `group` and how many of
its real workers (`real`, 0 meaning all of them). A network fault names
`groups`; an empty list means every group.

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
- A fault that takes the server down (kill, term or stall of the server, or a
  power cut) needs `clients.websocket.count` and `clients.rest_pollers.count`
  at 0: UI clients do not survive a server outage.

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

A server fault (kill, term, stall, power cut) and a network fault (partition,
blackhole, latency, flap) leave a **fault window**: from when it fired to when
it healed. The `logs` invariant treats a window differently (see below).
Faults on real workers leave no window.

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

The children stay in the task's process group. Each real task process writes
**markers** under a directory the harness owns: one file per process (its
role, its PID and group, when it started, when it was last seen, and when it
ended and with what exit code), refreshed every 250 ms while the process
lives, and one output file per task, which every process writing that task's
output appends to. A task killed by a signal records no end, but its last
refresh shows when it was last alive. The `no-orphans` and `single-writer`
checks read the markers, and so does the rework measurement.

Real workers serve their own queue in the library scenarios (`queues:
[render-real]`), so every real task runs on a real worker while simulated
workers carry the background load.

---

## Invariants

A chaos run reports eleven invariants. The first six are the load harness's;
the rest are added by chaos runs. A failing invariant carries evidence naming
the job, task, attempt or process, at most 20 entries.

| Invariant | The exact rule |
|---|---|
| `terminal` | Every accepted job and every orphan is `completed`, `failed` or `canceled` within `drain_timeout` after submissions stop. A job left open is stranded. |
| `outcomes` | As in the load harness: every task's final server status is the one the simulator decided; a job has exactly `steps x tasks_per_step` tasks; a `completed` job has only succeeded tasks. A task a real worker ran has no ledger to compare with and is left out. |
| `single-attempt` | As in the load harness: on each sampled task, no two of the server's attempts are held at once, each attempt after the first follows a failed or canceled one, and a terminal task holds none. On every task, the simulator never ran two attempts at once. A task's attempts are ordered by the server's assignment time, since a lease reply held up in flight can reach the worker after the next attempt's. |
| `logs` | As in the load harness: on each sampled task, the lines the server returns for the latest attempt equal the lines the simulator sent, in order. **Inside a fault window** the lines need only be an in-order subsequence of those sent: none duplicated, reordered or corrupted, any missing. This applies when the attempt, from its lease to its end, overlaps a window that hit its worker (the worker's group is in the window's groups, or the window names none). A task still missing lines is re-read for the logs settle time, since a restarted server replays the log stream; lines still missing after that are counted in the `logs.lost_in_fault` counter, not failed. |
| `workers-converge` | The farm's online worker count equals the number of workers the harness has running. In a chaos run the allowance runs from the last time the server's broker connection came up (its start, and the heal of each server fault or power cut): the server's liveness grace of about 3 minutes 10 seconds (the worker timeout, a worker's longest redial and one heartbeat), one sweep interval and 10 seconds, or the load harness's allowance, whichever is longer. The check is made after the last fault has healed. |
| `caps` | As in the load harness, counting the attempts of orphans as well. |
| `submission-atomic` | Every job in the farm is an accepted job or an orphan, and every orphan holds all of its class's tasks (`steps x tasks_per_step`). A submission is one transaction, so one the server answered with no 2xx must have created its whole job or nothing; a second job for one submission, or a partial one, fails. |
| `executions-recorded` | Read from the stopped server's store. Every attempt a simulated worker leased is in the store and closed, and is not `succeeded` when the worker reported it failed (one the worker reported succeeded may have been reclaimed, so it may be `failed` or `canceled`). No real task ran more times than the store has attempts for it. Marker files that could not be parsed are noted and fail nothing. |
| `store-integrity` | Read from the stopped server's store. On SQLite `PRAGMA integrity_check` is `ok` and `PRAGMA foreign_key_check` finds nothing. On both backends: no active usage claim breaks the claim invariant (I3); a terminal step holds only terminal tasks; a terminal task has no running attempt; a completed job has only completed steps; a failed or canceled job has no step still open. |
| `no-orphans` | No process of a real task was seen alive more than 10 seconds after the worker incarnation that started it ended. An incarnation still running is not judged. |
| `single-writer` | For each task, no two process executions wrote the task's output over overlapping spans of time, the span being from a process's first line to its last. |

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

The library's bounds come from runs on Linux, with headroom, and a comment in
most scenarios shows the arithmetic: a rack partition's bound of 30 covers the
work in flight on the cut quarter of the fleet.

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
      reason: a terminal status still in flight when the embedded broker shuts down is lost and the task re-runs
      tracker: H13f1
```

| Field | Meaning |
|---|---|
| `check` | An invariant name from the table above, or `rework`. |
| `kind` | `must` (the default) or `may`. |
| `max_attempts` | On a `rework` entry, the most rework it excuses. Required there, and an error on any other check. |
| `os` | The operating systems (`linux`, `darwin`, `windows`) the entry applies on. Empty means all. On any other system the entry does nothing. |
| `reason` | What the defect does. Required. |
| `tracker` | The tracker item, such as `H13f1`. Required. |

How an entry is applied:

- A failed result that an entry names becomes OK. The summary shows it as
  `known (H13f1)` and keeps its evidence. Several entries for one check join
  their trackers.
- A `rework` entry excuses rework only up to its `max_attempts`. Rework above
  the scenario's `max_rework_attempts` and at or under the ceiling is the
  defect firing, and the summary notes the ceiling. Rework over the ceiling
  stays a failure, and its evidence says the known ceiling was exceeded. The
  ceiling must be greater than `max_rework_attempts`, which the scenario must
  set, so a regression far larger than the defect still fails the run.
- A check that could not be measured, or did not run, is never excused: its
  failure stays, and a `must` entry for it is not counted as fired. Rework
  that could not be measured is one, as is a store the checks could not open.
- A **`must`** entry whose check did not fail adds a failed result of its own,
  named `known:<check>`. This is the **stops-firing rule**: when the defect is
  fixed, the scenario fails until the entry is removed, so a fixed defect
  cannot stay on the list and an entry cannot quietly stop meaning anything.
  The same happens when the check did not run.
- A **`may`** entry that did not fire adds nothing. It is for a defect that is
  a race, whose effect varies from run to run.
- A failure no entry names fails the run.

`TestChaos_MustViolationThatDoesNotFireFails` proves the stops-firing rule is
enforced: it runs `chaos-server-term` with a `must` entry for `outcomes` that
cannot fire and asserts the run records a failed `known:outcomes`.

### Defects found so far

The first wave found five defects in sqi. None is fixed on the chaos branch.
Each is also listed under [Known server
defects](load-testing.md#known-server-defects).

| Tracker | Defect | Recorded in |
|---|---|---|
| H13f1 | A worker's terminal status in flight when the server stops gracefully is lost and the finished task re-runs. Task status is a core NATS publish with no acknowledgment, and the embedded broker stops JetStream before it closes client connections. | `chaos-server-term`, `rework`, `may` |
| H13f2 | On SIGCONT after a server stall longer than `heartbeat_timeout`, the heartbeat sweep runs before the stall's backlog of heartbeats is consumed and takes every live worker offline, so their work is reclaimed and re-run, and a reclaimed attempt can run beside the new one. | `chaos-server-stall-long`, `rework` and `single-attempt`, `must` |
| H13f3 | A live worker the sweep marks offline stays offline while its connection stays up: heartbeats never bring it back, and it re-registers only on a reconnect. | `chaos-server-stall-long` and `chaos-worker-sleep`, `workers-converge`, `must` |
| H13f4 | Killing `sqi-worker` on Linux leaves its task's process tree running. A silent child, which writes no stdout, never gets SIGPIPE, outlives the worker's restart and writes alongside the re-run. | `chaos-worker-kill-mid-task`, `no-orphans` and `single-writer`, `must`, `os: [linux]` |
| H13f5 | A data race: `Scheduler.Stop` reads `s.cancel` while `Scheduler.Run` is still setting it, when the server shuts down right after launching the scheduler. Found while building the harness and reproduces on `main`. | Not tied to a scenario. |

---

## The scenario library

The 19 chaos scenarios are built in and named `chaos-...`. The family letter in
each description is the suite's: **S** server, **W** worker, **N** network,
and the churn and logs scenarios stand alone. Unless noted, a scenario
runs on SQLite or PostgreSQL, with 100 simulated 8-core workers and about 2.5
tasks per second of 2-step jobs.

| Scenario | Fault | Bound and known violations | Duration |
|---|---|---|---|
| `chaos-server-kill-5s` | SIGKILL the server at 1m, restart it 5s later. | rework 0 | 3m |
| `chaos-server-kill-20s` | The same, restarted 20s later. **Smoke.** | rework 0 | 3m |
| `chaos-server-kill-90s` | The same, restarted 90s later: the outage outlasts a worker's reconnect buffer for some of what it publishes. | rework 0 | 3m30s |
| `chaos-server-kill-mid-submit` | One job of 100,000 tasks, the server killed 2s in, during the write, restarted 10s later. 200 workers; the drain allows 20 minutes. | rework 0 | 3m |
| `chaos-server-term` | A graceful stop at 1m, restarted 10s later: the upgrade path. | rework 0; H13f1 `may` on `rework`, up to 10 | 3m |
| `chaos-server-stall-short` | SIGSTOP the server for 15s at 3m15s, under `heartbeat_timeout`. POSIX. | rework 0 | 3m45s |
| `chaos-server-stall-long` | SIGSTOP for 60s at 3m15s, about twice `heartbeat_timeout`. POSIX. | rework 0; H13f2 `must` on `rework` (up to 60) and `single-attempt`; H13f3 `must` on `workers-converge` | 4m30s |
| `chaos-logs-during-restart` | 40 workers, tasks of 5,000 lines of 200 bytes, the server killed at 1m and restarted 20s later: every terminal status must still arrive. | rework 0 | 3m |
| `chaos-worker-kill-mid-task` | One of 2 real workers killed at 1m while it runs 60s process trees (a chatty and a silent child), restarted 10s later. 60 simulated workers carry the background load. | rework 8; H13f4 `must` on `no-orphans` and `single-writer`, `os: [linux]` | 3m |
| `chaos-worker-gone` | One of 2 real workers killed at 30s while it runs 30s tasks, never restarted: its work is reclaimed and the worker is shown offline. **Smoke.** | rework 4 | 3m |
| `chaos-worker-sleep` | SIGSTOP one of 2 real workers for 75s at 3m15s while it runs 90s tasks, then SIGCONT: stale attempts are canceled, nothing is credited twice. POSIX. | rework 8; H13f3 `must` on `workers-converge` | 4m45s |
| `chaos-churn-storm` | No fault: 100 simulated workers each crash about 30 times an hour and restart 10s later. | rework 5 | 3m |
| `chaos-churn-instant-restart` | The same with an instant restart: the stranded-attempt shape (see [Known server defects](load-testing.md#known-server-defects)). | rework 5 | 3m |
| `chaos-net-blip` | Cut every one of 120 workers from the broker for 5s at 3m15s and for 15s at 3m40s. No reclaim, no rework. **Smoke.** | rework 0 | 4m15s |
| `chaos-net-rack-partition` | Cut `rack-b`, 30 of 120 workers, for 60s at 3m15s, twice `heartbeat_timeout`: their work is reclaimed and re-run. | rework 30 | 4m30s |
| `chaos-net-slow-site` | 200ms plus or minus 50ms of latency and a 256 KiB/s cap on `remote`, 40 of 120 workers, from 30s for 4 minutes: no false offline, no reclaim, no lost log. | rework 0 | 4m45s |
| `chaos-net-flap` | Cut and restore `rack-b`, 30 of 120 workers, every 3s for a minute from 3m15s: converges with nothing stranded. | rework 3 | 4m30s |
| `chaos-power-cut-server-first` | At 1m kill the server and every worker (100 simulated, 2 real), bring the server back 20s later and the workers 10s after it. | rework 5 | 3m |
| `chaos-power-cut-workers-first` | The same with the workers back first, 20s later, and the server 10s after them. | rework 5 | 3m |

The scenarios that depend on the server having marked a worker offline fire at
3m15s, after the server's liveness grace of about 3m10s from its start: before
then a cut or a stall cannot be mistaken for a dead worker however long it
lasts. That is why the library's runs are longer than their faults need.

Every scenario also runs all the invariants above. `Duration` is the
scenario's `duration`, after which the run drains; the three runs measured
took 3 minutes 52 seconds to 5 minutes 21 seconds in all.

---

## Reading a chaos summary

A chaos run's `summary.md` has three sections beyond the load harness's.

**Faults** lists each fault that fired, with its kind, its target, when it
started and when it healed, as offsets from the start of submissions, and the
error that stopped it when there was one. A fault that never healed shows
`never`: a worker killed with no `restart_after` stays down.

**Rework** gives the attempts that ran to completion and did not count, the
seconds of work they cost, how many each fault is blamed for (`by fault: #0
3`, where `#0` is the first fault) and up to 20 examples, each naming the
attempt or the process.

**Invariants** lists every result as `PASS`, `FAIL`, or `known (H13f2)` for a
failure a known violation excused, with its evidence beneath. A line
`known:<check>` that fails is a `must` entry whose check held: the defect it
records may be fixed, and the entry should be removed.

The rest of the files in the run directory are the load harness's. A managed
run's `target/` holds the server's and the real workers' logs, and the markers.

---

## CI

**`chaos-smoke`** in `.github/workflows/ci.yml` runs three scenarios that carry
no known violation, on Linux with SQLite: `chaos-server-kill-20s`,
`chaos-worker-gone` and `chaos-net-blip`. It runs on every push to `main`, and on a pull
request that touches `internal/scheduler`, `internal/store`, `internal/bus`,
`internal/worker`, `internal/server`, `cmd/sqi-server`, `cmd/sqi-worker`,
`internal/loadtest`, `test/chaos`, `go.mod`, `go.sum` or `ci.yml`, so most pull
requests do not pay for it. It takes about 14 minutes with its build (the job
allows 45). It runs `make chaos-smoke` with `SQI_LOAD_OUT` set, uploads the run
directories and the log as `chaos-smoke-runs` (7 days), and asserts by name
that the three subtests passed, since a skipped or filtered test would leave
`go test` green. **The three smoke scenarios are the gated set**: they
carry no known violation, so a pass says something. The 20-run gate described
below for them is in progress.

**The nightly** (`chaos` in `.github/workflows/load-nightly.yml`) runs the
whole suite on Linux with SQLite, `make chaos`, at 03:30 UTC and on a manual
dispatch without a `duration`. It allows 240 minutes, uploads the run
directories as `chaos-runs-sqlite` (30 days) and writes a table of the results
to the job page. It ignores the dispatch's `scenario` input.

Only the **gated** scenarios fail the job:

- `chaos-server-kill-20s`, `chaos-worker-gone` and `chaos-net-blip`, and
- `TestChaos_MustViolationThatDoesNotFireFails`.

Every other scenario is **advisory**. The job does not fail on it, but raises a
warning annotation naming each advisory scenario that did not pass. The
advisory list is the other 16: `chaos-churn-instant-restart`,
`chaos-churn-storm`, `chaos-logs-during-restart`, `chaos-net-flap`,
`chaos-net-rack-partition`, `chaos-net-slow-site`,
`chaos-power-cut-server-first`, `chaos-power-cut-workers-first`,
`chaos-server-kill-5s`, `chaos-server-kill-90s`,
`chaos-server-kill-mid-submit`, `chaos-server-stall-long`,
`chaos-server-stall-short`, `chaos-server-term`, `chaos-worker-kill-mid-task`
and `chaos-worker-sleep`. The gated list is one list in the workflow, to be
edited when a scenario is promoted.

**The 20-run gate.** A scenario is promoted to gating only after it passes 20
consecutive runs on CI hardware with only its known violations firing: a chaos
scenario is timing-sensitive, and a shared runner is noisier than a
workstation. A scenario that cannot meet that stays advisory.

Result of the 20-run gate for the three smoke scenarios: in progress (not yet
recorded; to be filled in).

---

## What comes next

This is the first of two waves. The second adds the families this one leaves
out: database faults, resource-limit faults and identity-provider faults;
`make chaos-container`, which runs the resource-limit scenarios and S6 inside
a container, as the isolation tests do; PostgreSQL in the
nightly; and a Windows nightly for the portable subset (process kill, network,
database and identity faults, with no SIGSTOP and no container volumes).

The defects above are tracked outside this page; promoting a scenario out of
`known_violations` is part of fixing one.

---

## See also

- [Load and soak testing](load-testing.md): the harness the suite is built on,
  the scenario format, the six load invariants and the known server defects.
- [Operations](operations.md): the shutdown, restart and recovery behavior the
  scenarios exercise.
- [Development](development.md): the Makefile targets and the test suites.
