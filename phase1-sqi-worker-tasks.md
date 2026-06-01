# Phase 1 — `sqi-worker` Initial Implementation Tasks

Detailed task breakdown for the second bullet of Section 17, Phase 1 of `sqi.md`:

> `sqi-worker`: pull-based worker, bare metal process executor, heartbeat

Each item is a single, discrete task. Completing all of these yields a buildable, testable, documented `sqi-worker` binary capable of: discovering and connecting to a running `sqi-server`, registering itself with its capability tags and compute location, pulling task assignments over NATS JetStream, executing bare-metal OS processes inside OpenJD sessions, streaming log output and status back to the server, and packaging as both a standalone binary and a Docker container image.

**Commit markers.** Suggested commit boundaries are called out inline as blockquoted lines, e.g. `> _Commit:_ `feat(...)` _— tasks X–Y_`. Each marker groups tasks that ship as one working, build-green unit. Messages follow Conventional Commits. Solo-friendly — these are about future-you's bisect, blame, and revert experience, not PR ceremony.

---

## Common Instructions

These apply to every task in this list:

- **No git operations.** Do not run `git add`, `git commit`, `git push`, or any other git command. All work is done locally in-place. Commit markers below define logical groupings for future reference only — they are not an instruction to commit.
- **Follow the design.** All work must fit the overall design as described in `sqi.md`. When a design choice is ambiguous, prefer the approach consistent with `sqi.md` and the existing `sqi-server` implementation. If a genuine conflict arises, pause and flag it rather than deciding unilaterally.
- **Reuse shared packages.** The worker lives in the same repository as the server. Shared internal packages (`internal/bus`, `internal/openjd`, `internal/store` types, etc.) should be reused rather than duplicated. Worker-specific logic belongs in `internal/worker`.
- **No sessions table.** Per `sqi.md §7.4`, sessions are worker-side runtime constructs in Phase 1. Do not create a `sessions` database table. The `session_id` column on `task_attempts` (already in the schema from server task 26) is sufficient.

---

## Tracking

- **Currently working on:** —
- **Last updated:** —
- **Conventions:**
  - `- [ ]` not started · `- [x]` complete
  - Task checkboxes tick when the code lands; commit checkboxes tick once the commit is made.
  - Counts in section headers (e.g., `0 / 4`) are updated by hand when convenient — checkboxes are the source of truth.
  - Update the "Currently working on" line at the start of a session, clear it at the end.

---

## 1. Worker Entry Point — 0 / 4

- [ ] **1.** Create `cmd/sqi-worker/main.go` with a `spf13/cobra` root command as the `sqi-worker` binary entry point, mirroring the structure used in `cmd/sqi-server`.
- [ ] **2.** Implement the `start` subcommand that boots the full worker agent with graceful shutdown on SIGINT/SIGTERM.
- [ ] **3.** Implement the `version` subcommand that prints embedded build metadata (version, commit, build date, Go version) using the same `ldflags` approach as the server.
- [ ] **4.** Implement a `config print` subcommand that emits the effective merged worker configuration for debugging, analogous to the server's `config print`.

> - [ ] _Commit:_ `feat(worker/cli): worker entry point with start/version/config subcommands` _— tasks 1–4_

---

## 2. CLI and Configuration — 0 / 14

- [ ] **5.** Define a typed `WorkerConfig` struct in `internal/worker/config` with fields for: server NATS URL, worker name, worker data directory, compute location name, maximum concurrent tasks, capability tag overrides, mDNS enable/disable, local metrics HTTP address, log level, and shutdown grace period.
- [ ] **6.** Implement worker ID persistence: on first start, generate a UUID and write it to `<data_dir>/worker.id`; on subsequent starts, read and reuse the same ID so the server can correlate a worker across restarts.
- [ ] **7.** Implement layered configuration loading: built-in defaults → YAML/JSON file → environment variables (`SQI_WORKER_*`) → CLI flags, in that override order, reusing the same viper/koanf pattern as the server.
- [ ] **8.** Implement startup validation that fails fast with actionable error messages for missing or invalid configuration (e.g., no server address configured and mDNS disabled, negative concurrency limit).
- [ ] **9.** Implement automatic capability detection that reads and reports the host OS, OS version, logical CPU count, total installed RAM, and GPU presence (GPU VRAM if detectable) at startup.
- [ ] **10.** Support manual capability tag overrides in configuration — a string list of arbitrary tags (e.g., `["maya-2025", "gpu", "highram"]`) that are merged with auto-detected capabilities when registering.
- [ ] **11.** Generate and check in a `config/sqi-worker.example.yaml` documenting every configuration option with inline comments, analogous to `config/sqi-server.example.yaml`.
- [ ] **12.** Implement a `--dry-run` flag on the `start` subcommand that resolves configuration, detects capabilities, and prints what would be registered — then exits without connecting to the server, useful for validating a worker config before deployment.
- [ ] **13.** Add unit tests for the configuration loader covering precedence (flag > env > file > default), validation error paths, worker ID persistence (create on first run, reload on second), and the `--dry-run` output.
- [ ] **14.** Add unit tests for capability auto-detection mocking out the OS-level syscalls (CPU count, RAM, GPU queries) with table-driven cases covering Linux, macOS, and Windows return values.

> - [ ] _Commit:_ `feat(worker/config): typed config, worker id persistence, capability detection, example file, and tests` _— tasks 5–14_

---

## 3. Logging, Metrics, and Health — 0 / 5

- [ ] **15.** Adopt `log/slog` for structured logging with JSON output by default and human-readable output behind a `--log-format=text` flag, consistent with the server logging setup.
- [ ] **16.** Start an optional local HTTP server on a configurable address (default `127.0.0.1:9091`) exposing `/healthz` (liveness) and `/readyz` (readiness, verifying NATS connection is live) for monitoring and container orchestration probes.
- [ ] **17.** Expose Prometheus metrics at `/metrics` on the same local HTTP server, covering: active task count, total tasks completed, total tasks failed, total tasks canceled, process execution duration (histogram), NATS message publish/consume counts, and worker uptime.
- [ ] **18.** Add `pprof` endpoints on the local HTTP server gated behind a `--pprof` configuration flag for performance diagnostics under load.
- [ ] **19.** Add a request-scoped logger middleware to the local HTTP server that attaches request ID, method, path, status, and duration to every request log line.

> - [ ] _Commit:_ `feat(worker/obs): slog, health endpoints, prometheus metrics, pprof` _— tasks 15–19_

---

## 4. NATS Client Connection — 0 / 5

- [ ] **20.** Connect to the remote NATS instance (the server's embedded NATS) using the `nats-io/nats.go` client, bound to the server NATS URL from configuration; the worker does not embed its own NATS server.
- [ ] **21.** Implement exponential backoff reconnection with configurable max retries and a jitter factor so a worker restart storm does not spike load on the server.
- [ ] **22.** Support TLS for the NATS connection with configurable certificate paths and a `--nats-insecure-skip-verify` flag for development environments.
- [ ] **23.** Implement graceful NATS disconnect at shutdown: drain in-flight subscriptions and flush any pending publishes before closing the connection.
- [ ] **24.** Wire the NATS connection lifecycle into the worker's startup/shutdown sequence so that NATS connect failure at boot is a fatal error and NATS reconnect failures after initial connect are retried with backoff until the shutdown grace period expires.

> - [ ] _Commit:_ `feat(worker/nats): remote nats client with reconnect, tls, graceful drain` _— tasks 20–24_

---

## 5. Worker Registration and Capabilities — 0 / 5

- [ ] **25.** Implement the worker registration message published to the `worker.register` NATS subject at startup, including: worker ID, name, compute location, detected capabilities, max concurrent tasks, NATS client address, and server version compatibility header.
- [ ] **26.** Handle the server's registration acknowledgment response: on success, log the assigned worker record ID and proceed to the pull loop; on rejection (e.g., duplicate or incompatible worker ID), log the reason and exit with a non-zero code.
- [ ] **27.** Implement deregistration: publish a departure message to `worker.register` (or a dedicated `worker.deregister` subject if defined by the wire protocol) on graceful shutdown so the server can mark the worker offline immediately rather than waiting for heartbeat timeout.
- [ ] **28.** Implement a re-registration path for the case where the NATS connection drops and reconnects — re-publish the registration message so the server restores the worker's presence without requiring a process restart.
- [ ] **29.** Store the registered capability map in memory for the duration of the process so it can be included in heartbeats and referenced in log output when explaining why a task was or was not accepted.

> - [ ] _Commit:_ `feat(worker/registration): register, deregister, re-register on reconnect` _— tasks 25–29_

---

## 6. Heartbeat — 0 / 4

- [ ] **30.** Implement a periodic heartbeat publisher that publishes to the `worker.heartbeat` NATS subject at a configurable interval (default matching the server's heartbeat timeout sweep interval from `sqi-server` configuration).
- [ ] **31.** Include in each heartbeat message: worker ID, timestamp, active task count, max concurrent tasks, current task IDs, worker uptime, and last-assignment timestamp so the server has enough data to detect stale assignments without additional queries.
- [ ] **32.** Implement a watchdog goroutine that monitors the NATS connection state and triggers re-registration if the connection is lost and restored, ensuring the server never has a stale worker record after a reconnect.
- [ ] **33.** Log a warning if the heartbeat publish latency exceeds half the configured heartbeat interval, indicating the server's NATS instance or network path is under strain.

> - [ ] _Commit:_ `feat(worker/heartbeat): periodic heartbeat with status payload and reconnect watchdog` _— tasks 30–33_

---

## 7. mDNS Server Discovery — 0 / 4

- [ ] **34.** Implement an mDNS browser using `grandcat/zeroconf` (or equivalent) that discovers `_sqi._tcp` services on the local network — the same service type advertised by the server's mDNS responder (server task 89).
- [ ] **35.** At startup, if no explicit server address is configured, run the mDNS browse for a configurable timeout (default 5 s) and use the first discovered server's hostname and NATS port as the connection target.
- [ ] **36.** Support explicit server NATS URL configuration that bypasses mDNS entirely, required for cross-subnet and cloud deployments where multicast is unavailable.
- [ ] **37.** Make mDNS discovery toggleable via configuration; log a clear message when mDNS is disabled or when it times out with no result so operators know why auto-discovery was skipped.

> - [ ] _Commit:_ `feat(worker/discovery): mdns server discovery with explicit address fallback` _— tasks 34–37_

---

## 8. Work Assignment Pull — 0 / 5

- [ ] **38.** Subscribe to the `work.assign.<queue>` JetStream pull consumer for each queue this worker is assigned to serve (all queues if no queue restriction is configured), using the consumer groups defined by server task 37 so each task is delivered to at most one worker.
- [ ] **39.** Implement the work pull loop: fetch up to `max_concurrent - active_count` messages at a time so the worker never receives more assignments than it can execute, and block efficiently when at capacity.
- [ ] **40.** Acknowledge (ack) a task assignment NATS message immediately after validating the payload and starting the session, so the JetStream server does not redeliver it; nack with a configurable delay if pre-execution validation fails so the server can reassign the task to another worker.
- [ ] **41.** Handle task assignment payloads that reference a compute location this worker is no longer in (e.g., after a config change without restart) with a nack and a warning log rather than a silent failure.
- [ ] **42.** Implement a brief backoff (configurable, default 2 s) between pull attempts when the queue is empty to avoid tight polling on idle queues, resetting to zero backoff as soon as a task is received.

> - [ ] _Commit:_ `feat(worker/pull): work assignment pull loop with ack/nack semantics` _— tasks 38–42_

---

## 9. Session Management — 0 / 6

- [ ] **43.** Implement a `Session` struct in `internal/worker` with fields for: session ID (from the task assignment payload), working directory path, owning job ID, active task list, entered environments list, and creation timestamp.
- [ ] **44.** Create a uniquely named working directory under `<data_dir>/sessions/<session_id>/` for each new session, ensuring the directory is isolated per session and not shared across concurrent sessions.
- [ ] **45.** Implement environment entry: iterate the session's environment actions in declaration order at session start, executing each action's setup command as a child process with the session working directory and environment; abort the session and run teardown in reverse if any setup action fails.
- [ ] **46.** Implement environment exit: on session end (whether by completion, failure, or cancellation), execute each entered environment's teardown actions in reverse declaration order to restore host state predictably.
- [ ] **47.** Implement session cleanup that removes the session's working directory after environment teardown completes, logging the cleanup result; retain the directory on failure if a `--keep-failed-sessions` debug flag is set.
- [ ] **48.** Include the OpenJD-assigned session ID in all task status messages and log chunks published from within the session, so the server can group attempts by session for debugging (consistent with the `session_id` column on `task_attempts` from server task 26).

> - [ ] _Commit:_ `feat(worker/session): session lifecycle with working directory and environment setup/teardown` _— tasks 43–48_

---

## 10. Task Executor — Bare Metal Processes — 0 / 10

- [ ] **49.** Implement the bare-metal task executor in `internal/worker/executor` that starts OS processes via `os/exec` using the resolved command and argument list from the task assignment payload.
- [ ] **50.** Build the process environment by merging the task payload's explicit environment variables on top of the worker process's inherited environment, with task variables taking precedence, so DCC tools find their expected system variables.
- [ ] **51.** Set the process working directory to the session's working directory so relative file paths in commands resolve correctly.
- [ ] **52.** Capture both stdout and stderr from the process through separate readers wired to the log streamer and progress line parser, rather than inheriting the worker's own stdout/stderr, so output can be attributed, timestamped, and forwarded.
- [ ] **53.** Record the process PID, start time, end time, and exit code in the task attempt record, publishing them in terminal status messages so the server can persist accurate timing data.
- [ ] **54.** Treat a non-zero process exit code as task failure; include the exit code in the failure status message published to `task.status.<job>`.
- [ ] **55.** Implement a configurable per-task execution timeout: after the timeout elapses, send SIGTERM to the process; if the process does not exit within an additional grace period, send SIGKILL; publish a failed status with a timeout reason.
- [ ] **56.** Implement concurrent task execution: maintain a map of active sessions keyed by session ID and allow up to `max_concurrent` simultaneous process executions, each in its own goroutine, with a semaphore protecting the concurrency limit.
- [ ] **57.** Log a warning at startup (and refuse to proceed unless a `--allow-root` flag is set) if the worker process is running as the root user on Linux/macOS, since executing render processes as root is a security risk per `sqi.md §18, open question 2`.
- [ ] **58.** Add unit tests for the task executor using a simple test binary (e.g., `echo`, a compiled Go test helper) as the child process, asserting stdout/stderr capture, exit code propagation, timeout escalation to SIGKILL, and concurrent task count enforcement.

> - [ ] _Commit:_ `feat(worker/executor): bare-metal process executor with env, cwd, capture, timeout, concurrency, and tests` _— tasks 49–58_

---

## 11. Path Resolution — Resolved Mode — 0 / 5

- [ ] **59.** Parse the path map included in the task assignment payload (the resolved concrete paths the server computed from named storage locations per `sqi.md §8.3 resolved mode`) into a lookup table keyed by named location.
- [ ] **60.** Apply resolved-mode path substitution to all named-location references in the task command and argument strings before constructing the `os/exec` command, so the launched process sees only concrete filesystem paths.
- [ ] **61.** Write the standard OpenJD path mapping JSON file into the session working directory (`path_mapping.json`) using the same path pairs from the assignment payload, so applications with native OpenJD path mapping support can consume it directly without additional integration.
- [ ] **62.** Abort task execution with a clear error if any named location in the task payload has an empty or missing resolved path, publish a failed status immediately, and log which location was unresolvable to help operators diagnose misconfigured storage locations.
- [ ] **63.** Add unit tests for resolved-mode path substitution using a table of named locations and command strings, including edge cases (location appearing multiple times in one command, location with no mapping, empty path).

> - [ ] _Commit:_ `feat(worker/paths): resolved-mode path substitution, openjd path mapping file, and tests` _— tasks 59–63_

---

## 12. Log Streaming — 0 / 6

- [ ] **64.** Implement a log chunk publisher that reads process stdout and stderr line-by-line and publishes chunks to the `task.logs.<task_attempt_id>` NATS subject, tagging each chunk with whether the source was stdout or stderr.
- [ ] **65.** Assign monotonic sequence numbers to each published log chunk so the server can reassemble chunks in order even if NATS delivers them out of sequence (consistent with server task 59's log ingestion design).
- [ ] **66.** Implement configurable chunk sizing: batch up to N lines (default 50) or B bytes (default 16 KB) per NATS message to keep per-message overhead low while avoiding excessively large payloads from verbose processes.
- [ ] **67.** Buffer log output in a small in-memory ring for grouping into chunks while keeping end-to-end latency under a configurable flush interval (default 500 ms) so the web UI log viewer feels live.
- [ ] **68.** Flush all remaining buffered log chunks to NATS after the process exits and before publishing the terminal status message, so the server always receives complete log output before marking the task finished.
- [ ] **69.** Add unit tests for the log chunk publisher using a mock NATS connection, asserting sequence number monotonicity, correct chunk boundaries, and the flush-before-terminal-status ordering guarantee.

> - [ ] _Commit:_ `feat(worker/logs): log streaming to nats with sequence numbers, chunking, flush-on-exit, and tests` _— tasks 64–69_

---

## 13. OpenJD Progress Line Parsing — 0 / 5

- [ ] **70.** Intercept `openjd_progress: <0-100>` lines in the process stdout stream before forwarding to the log chunk publisher; parse the integer value and update the task's last-known progress percentage in memory.
- [ ] **71.** Intercept `openjd_status: <text>` lines and publish them as task status update messages to `task.status.<job>` with the status text attached, allowing the web UI to display a live human-readable status string alongside the progress bar.
- [ ] **72.** Intercept `openjd_fail: <text>` lines, immediately mark the task as failed with the provided message as the failure reason, send SIGTERM to the process, and begin the cancellation grace period — do not wait for the process to exit on its own.
- [ ] **73.** Pass all other stdout and stderr lines — including any lines that begin with `openjd_` but do not match a recognized directive — through to the log stream unmodified so no process output is silently discarded.
- [ ] **74.** Add unit tests for the OpenJD progress line parser covering `openjd_progress`, `openjd_status`, `openjd_fail`, unrecognized `openjd_` prefixes, and normal non-directive lines.

> - [ ] _Commit:_ `feat(worker/openjd): openjd progress, status, and fail line interception with tests` _— tasks 70–74_

---

## 14. Task Status Reporting — 0 / 5

- [ ] **75.** Implement a typed status message publisher for `task.status.<job>` covering the task state transitions the worker is responsible for: `running` (immediately after process launch), `succeeded`, `failed`, and `canceled`.
- [ ] **76.** Include in every status message: `task_id`, `attempt_id`, `session_id`, `worker_id`, `timestamp`, `exit_code` (on terminal states), `failure_reason` (on failure), and `last_progress` (last `openjd_progress` value seen, or `null` if none).
- [ ] **77.** Publish a `running` status message immediately after the process is successfully launched and before entering the log streaming loop, so the server can record the actual start time and the web UI reflects the task as active.
- [ ] **78.** On abnormal worker shutdown (SIGTERM received while tasks are active), publish a `failed` status with reason `worker_shutdown` for every in-flight task before the NATS connection is closed, so the server does not have to wait for heartbeat timeout to detect the loss.
- [ ] **79.** Implement retry logic for status message publishes: if a NATS publish fails (e.g., transient connectivity blip), retry up to three times with backoff before logging the failure and proceeding with shutdown — status loss is better than deadlocking the shutdown sequence.

> - [ ] _Commit:_ `feat(worker/status): task status publisher with progress, session id, and shutdown flush` _— tasks 75–79_

---

## 15. Task Cancellation Handling — 0 / 6

- [ ] **80.** Subscribe to a worker-specific cancel subject (e.g., `worker.<worker_id>.cancel`) for per-task cancel messages from the server, as published by the server's cancellation propagation logic (server task 54).
- [ ] **81.** On receipt of a cancel message, locate the matching in-progress task by `task_id`; if found, send SIGTERM to the running process; if not found (task already finished), ack the message silently.
- [ ] **82.** If the process does not exit within a configurable grace period (default 10 s) after SIGTERM, send SIGKILL, log the escalation with the task ID and process PID, and proceed with session teardown.
- [ ] **83.** After the process is terminated by cancellation, run the session's environment teardown in reverse order and remove the session working directory, then publish a `canceled` status message.
- [ ] **84.** Implement Windows-compatible process termination: since SIGTERM is not natively supported on Windows, use `taskkill /F /T /PID <pid>` (or `TerminateProcess` via syscall) in place of SIGTERM/SIGKILL in the cancellation and timeout paths.
- [ ] **85.** Add unit tests for the cancellation path using the same test binary, asserting SIGTERM delivery, grace period enforcement, SIGKILL escalation, and `canceled` status publication.

> - [ ] _Commit:_ `feat(worker/cancel): task cancellation with sigterm/sigkill grace period, windows support, and tests` _— tasks 80–85_

---

## 16. Graceful Worker Shutdown — 0 / 3

- [ ] **86.** Handle SIGINT and SIGTERM at the worker process level: stop the work pull loop immediately (accept no new task assignments), allow all currently executing tasks to run until they complete or until the shutdown grace period expires.
- [ ] **87.** Implement the shutdown grace period: if tasks are still running after the grace period elapses, send SIGTERM (then SIGKILL after a short additional wait) to each active process, publish `failed` statuses, and then shut down.
- [ ] **88.** Log the shutdown trigger (signal received), the number of tasks that completed cleanly, and the number that were forcibly terminated, so operators can tell from logs whether a rolling restart or hard kill occurred.

> - [ ] _Commit:_ `feat(worker/shutdown): graceful shutdown with in-flight task drain and force-kill fallback` _— tasks 86–88_

---

## 17. Build, Packaging, and Release — 0 / 7

- [ ] **89.** Add `sqi-worker` to the existing `goreleaser` configuration alongside `sqi-server` so it is cross-compiled for `linux`, `darwin`, and `windows` on `amd64` and `arm64` with the same version embedding via `-ldflags`.
- [ ] **90.** Write a `deploy/docker/worker/Dockerfile` for `sqi-worker` based on an Alpine base image (consistent with `sqi.md §9.3`), installing only the runtime dependencies needed for the worker agent itself (not any DCC software).
- [ ] **91.** Add the `sqi-worker` Docker image build and push to GHCR to the existing GitHub Actions release workflow, tagging images with the same version scheme as the server image.
- [ ] **92.** Add a `docker-compose.smoke.yml` in `deploy/` that starts one `sqi-server` container and one `sqi-worker` container linked together, verifying that the worker registers successfully and appears in the server's worker list — run this as a CI step against the built images.
- [ ] **93.** Add an integration test that boots a full `sqi-server` (via the integration harness from server task 101) and a real `sqi-worker` binary in the same test process, submits a minimal OpenJD job, and asserts: worker registers, task is assigned and executed, log output is streamed to the server, and the task reaches `succeeded` state with correct timing data.
- [ ] **94.** Run the full worker test suite with `-race` enabled in CI and enforce a minimum line-coverage threshold consistent with the server's threshold.
- [ ] **95.** Add fuzz targets for the task assignment payload decoder and the OpenJD progress line parser, since both consume untrusted data from the network.

> - [ ] _Commit:_ `release(worker): goreleaser cross-builds, alpine docker image, ci integration and tests` _— tasks 89–95_

---

## 18. Documentation — 0 / 7

- [ ] **96.** Write a top-level `README.md` for `cmd/sqi-worker` describing what it does, minimum configuration, how to run it against a local `sqi-server`, and how to verify registration via the web UI.
- [ ] **97.** Write `docs/worker-deployment.md` covering: bare-metal installation on Linux (with a systemd unit file template), macOS (with a launchd plist template), and Windows (with a Windows service registration command); Docker deployment; and how to configure auto-start on boot in each environment.
- [ ] **98.** Write `docs/worker-configuration.md` documenting every configuration option with its type, default value, environment variable name (`SQI_WORKER_*`), and a worked example, analogous to `docs/configuration.md` for the server.
- [ ] **99.** Write `docs/worker-capabilities.md` listing all auto-detected capability tags, their detection logic, and how to add, override, or suppress individual tags via configuration — the reference operators use when writing worker affinity rules in job submissions.
- [ ] **100.** Write `docs/worker-docker.md` covering the Docker image, required environment variables, volume mounts (data directory for worker ID persistence), network requirements (NATS port reachability), and a `docker run` quickstart.
- [ ] **101.** Generate Go package documentation (`pkg.go.dev`-compatible godoc comments) for any exported types in `internal/worker` that other packages or external integrations might reference.
- [ ] **102.** Add a `docs/development.md` section (or extend the server's version) describing how to run a worker locally against a dev server, how to write a new executor type, and how to add a new capability tag to the auto-detection logic.

> - [ ] _Commit:_ `docs(worker): readme, deployment, configuration, capabilities, docker, godoc, dev guide` _— tasks 96–102_

---

## 19. Final Verification — 0 / 5

- [ ] **103.** Execute an end-to-end smoke script that: downloads the built `sqi-worker` binary, starts it against a running `sqi-server`, confirms the worker appears in `GET /api/v1/workers`, submits a minimal OpenJD job via the REST API, and asserts the job reaches `succeeded` with log output retrievable over both REST and WebSocket.
- [ ] **104.** Verify the Docker worker image starts, registers with a containerized `sqi-server`, and executes a sample task successfully using the `docker-compose.smoke.yml` from task 92.
- [ ] **105.** Verify the cross-platform binaries produced by `goreleaser` start and register successfully on a Linux, macOS, and Windows host (or VM).
- [ ] **106.** Verify the test suite passes with `-race` enabled and that no `golangci-lint` or `go vet` warnings remain on `main` for the worker package.
- [ ] **107.** Verify every example command in the worker documentation executes as written against a freshly built binary and server.

> - [ ] _Commit:_ `test(worker): end-to-end smoke verification script` _— tasks 103–107_
