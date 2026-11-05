# sqi-load run run-1

- Scenario: unit (load)
- Run: 2026-11-03T10:00:00Z to 2026-11-03T10:12:00Z (12m0s); warmup 2m0s
- Submissions stopped: 2026-11-03T10:10:00Z (10m0s in); drain 2m0s
- Environment: linux/amd64, 8 CPUs, sqi v0.4.0 (abc1234), store sqlite
- Flags: --seed 7

## Validity

- VALID: the load generator kept up.
- Generator: peak CPU 1.50 cores of 8, peak p99 schedule lag 200ms.

## Throughput

Rates are steady-window rates: each counter's growth from the end of warmup to the end of submissions, divided by that window (8m0s). Total is the count at the end of the run, drain included; In window is the part the rate is taken over.

| Counter | Total | In window | Per second |
|---|---:|---:|---:|
| jobs.submitted | 600 | 384 | 0.80 |
| submit.status.201 | 600 | 384 | 0.80 |
| tasks.terminal | 12000 | 7680 | 16.00 |
| workers.live | 100 | - | - |

## Latency

Histograms cover the whole run, warmup included.

| Histogram | n | p50 | p95 | p99 | max |
|---|---:|---:|---:|---:|---:|
| first_lease | 2 | 850ms | 1.234s | 1.234s | 1.234s |
| rest.jobs-list | 1 | 40ms | 40ms | 40ms | 40ms |
| submit_latency | 3 | 8.012ms | 12ms | 12ms | 12ms |
| ws_lag | 0 | - | - | - | - |

## Server resources

| Series | Peak | End |
|---|---:|---:|
| disk.sqlite_wal_bytes | 5.9 KiB | 5.9 KiB |
| sched.queue_depth{default} | 500 | - |
| server.cpu_cores | 2.10 | 2.10 |
| server.gc_pause_p99_s | 3.5ms | 2ms |
| server.goroutines | 1000 | 1000 |
| server.rss_bytes | 244.0 MiB | 244.0 MiB |

A queue's end depth is not shown: the server keeps a drained queue's last non-zero value.

## Growth

Least-squares slope per hour from 2026-11-03T10:02:00Z (the end of warmup).

| Series | Slope | R² |
|---|---:|---:|
| disk.sqlite_wal_bytes | +234.4 KiB/h | 1.00 |
| server.goroutines | 0/h | 0.00 |
| server.rss_bytes | +240.0 MiB/h | 1.00 |

## Invariants

- PASS terminal
- FAIL logs
  - task t1: sent 10 lines, server returned 9

## Findings

- [warn] wal-never-shrank: WAL grew to 5.9 KiB and never shrank

## Unavailable

- pg.db_bytes: not a PostgreSQL run
