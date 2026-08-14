<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
# Cross-Platform Validation Guide

This guide is the manual procedure for confirming `sqi` works on **Linux, macOS,
and Windows**. It closes the Phase 1 verification items that CI cannot fully
prove on its own:

- **Cross-platform runtime** (`phase1-tests.md` §3): the cross-compiled binaries
  actually *start and register*, not just compile.
- **Cross-browser web UI** (`phase1-tests.md` §4): the embedded UI works in
  Chromium, Firefox, and WebKit.

CI on the default Linux runner already exercises the unit suites, the Docker
smoke test, and (where wired) the Playwright suite. The steps below are what you
run on a **Linux VM** and a **Windows VM** (and natively on this Mac) to cover
all three platforms.

There are three independent checks, in increasing order of coverage:

| Check | What it proves | Time |
| --- | --- | --- |
| A. Binary runtime smoke | server + worker start, register, run a job, stream logs (REST + WS) | ~2 min |
| B. Playwright E2E | the embedded browser UI submits a job and shows it run live | ~3 min |
| C. Manual UI walk-through | a human confirms the UI in each browser | ~5 min |

Run **A** on every platform. Run **B** on every platform you can install browsers
on. **C** is the cross-browser sign-off.

---

## 0. Prerequisites per platform

You need the Go toolchain (1.26+), Node.js (the version pinned in `.nvmrc`), and
a POSIX shell for the smoke script.

| Platform | Toolchain | Shell for `scripts/smoke.sh` |
| --- | --- | --- |
| **Linux** | `go`, `node`, `npm`, `make`, `curl`, `jq` (or `python3`) | native `bash` |
| **macOS** | same (`brew install go node jq`) | native `bash`/`zsh` |
| **Windows** | `go`, `node`, `npm`; `make` optional | **Git Bash** or **WSL2** (the smoke script is bash; `make` may be absent — see §1.3) |

> **Spinning up VMs on this Mac:** UTM (Apple Silicon) or VirtualBox/VMware
> (Intel) both work. Use an `arm64` Linux/Windows image on Apple Silicon and an
> `amd64` image on Intel so you exercise the architecture your Mac builds for.
> Share the repo into the VM (shared folder or `git clone`) and run the steps
> below inside the guest.

---

## A. Binary runtime smoke (run on all three platforms)

This is the fastest, highest-signal check. It builds the binaries, boots a
server + worker, submits an `echo` job, and asserts the output is retrievable
over **both REST and WebSocket**.

### A.1 Linux / macOS

```sh
# from the repo root, inside the VM / on the Mac
make smoke
```

`make smoke` builds `bin/sqi-server` + `bin/sqi-worker` and runs
`scripts/smoke.sh` against them. Expected tail:

```
[smoke] SMOKE TEST PASSED
[smoke]   REST log assertion: PASSED
[smoke]   WS   log assertion: PASSED
[smoke]   EXPR phase-3 assertion: PASSED
```

The EXPR line is the newest leg: a second job declaring `extensions: [EXPR]`
whose `onRun` args the worker resolves at phase 3, asserted on the resolved text
rather than on the job completing.

Exit code `0` = pass. On failure the script prints the server log tail and exits
non-zero.

The WebSocket leg needs a `websockets`-capable Python. The script auto-selects,
in order: `$SQI_SMOKE_PYTHON` → `clients/python/.venv/bin/python` → system
`python3`. To guarantee it, create the client venv first:

```sh
make py-install            # creates clients/python/.venv with websockets
make smoke
```

If no `websockets`-capable interpreter is found, the WS assertion logs a SKIP
(REST stays a hard pass) — so on a bare VM, install the venv to exercise WS.

### A.2 Windows

`make` is usually absent on Windows. Build the binaries directly, then run the
script under Git Bash or WSL2:

```sh
# Git Bash, repo root
cd web && npm ci && npm run build && cd ..
go build -o bin/sqi-server.exe ./cmd/sqi-server
go build -o bin/sqi-worker.exe ./cmd/sqi-worker

# point the smoke script at the .exe binaries
SQI_SERVER_BIN=bin/sqi-server.exe SQI_WORKER_BIN=bin/sqi-worker.exe \
  bash scripts/smoke.sh
```

> **Windows note:** this exercises the worker's Windows process-termination path
> (`taskkill`/`TerminateProcess`) when the job's `echo` process is reaped — the
> code path that only has unit coverage elsewhere. A clean
> `[smoke] SMOKE TEST PASSED` confirms it works end to end on Windows.

### A.3 What "pass" means

- `GET /readyz` returned ready (SQLite + NATS up).
- The worker appears online in `GET /api/v1/workers`.
- The job reached `completed`.
- The sentinel string is present in `GET /api/v1/tasks/{id}/logs` (REST) **and**
  arrived live over `/api/v1/ws` (WebSocket).

---

## B. Playwright E2E (cross-browser UI, run per platform)

This drives the **real embedded production UI** (the React bundle compiled into
`sqi-server`) in Chromium, Firefox, and WebKit. It boots its own server + worker,
seeds a farm/queue, submits a job through the `/submit` form, watches the job
list row go to `Completed` live over WebSocket, and asserts the log output is
visible.

### B.1 Linux / macOS — self-booting mode (default)

```sh
cd web
npm ci
npx playwright install --with-deps   # Linux: installs browser system libs too
npm run test:e2e                      # runs chromium + firefox + webkit
```

Run a single engine while iterating:

```sh
npx playwright test --project=chromium
npx playwright test --project=firefox
npx playwright test --project=webkit
```

Expected: `6 passed` (2 specs × 3 engines). An HTML report lands in
`web/playwright-report/` (open with `npx playwright show-report`).

The suite's global setup builds the binaries if `bin/sqi-server`/`bin/sqi-worker`
are missing (or honors `SQI_SERVER_BIN`/`SQI_WORKER_BIN`), boots the stack on an
ephemeral loopback port, and tears it down afterward.

### B.2 Windows — external-server mode (recommended)

The Playwright global setup tears the stack down with a POSIX process-group kill,
which **does not work on Windows**. On Windows, start the stack yourself and
point Playwright at it via `SQI_BASE_URL`; in that mode global setup *attaches*
to your running server (and seeds a farm/queue only if none exist) and does not
manage process lifecycle.

```sh
# Git Bash — terminal 1: start the server (serves the embedded UI on :8080)
bin/sqi-server.exe serve

# terminal 2: start a worker (auto-discovers the server via mDNS, or pass --nats-url)
bin/sqi-worker.exe start --allow-root   # --allow-root only if running elevated

# terminal 3: run Playwright against the running server
cd web
npm ci
npx playwright install
SQI_BASE_URL=http://localhost:8080 npx playwright test
```

> Chromium and Firefox install cleanly on Windows. WebKit on Windows via
> Playwright is supported but heavier; if it is problematic in your VM, run
> `--project=chromium --project=firefox` on Windows and rely on the Mac for the
> WebKit engine (all three pass on macOS).

### B.3 Linux browser dependencies

On a fresh Linux VM, `npx playwright install --with-deps` pulls the system
libraries Firefox/WebKit need. If you used `install` without `--with-deps`, run:

```sh
npx playwright install-deps
```

---

## C. Manual cross-browser walk-through (sign-off)

For the human cross-browser sign-off (`phase1-tests.md` §4), in **each** of
Chrome/Edge, Firefox, and Safari (Safari on macOS only):

1. Start the stack: `make run-server` (terminal 1) and
   `bin/sqi-worker start` (terminal 2). Open `http://localhost:8080/`.
2. **Dashboard** loads; the connection badge is green (WebSocket connected); the
   seeded/started worker shows online.
3. **Submit** (`/submit`): pick a queue, use **Load example → single-step shell
   command**, submit. You are redirected to the job detail / see a success toast.
4. **Job list** (`/jobs`): the new job's status badge transitions to `Completed`
   without a manual refresh.
5. **Log viewer**: open the task's logs; the echoed output is visible, ANSI
   colors render, and copy-to-clipboard works.
6. Open the browser dev console and confirm **no uncaught errors** during the
   run (WebSocket reconnect, `ansi-to-html`, and the Clipboard API are the
   browser-variance hotspots).

---

## Known caveats (read before validating)

- **Deep links / page refresh now work (fixed 2026-06-13).** The embedded server
  serves the SPA shell for any extensionless non-asset path (`/jobs`, `/submit`,
  `/workers/{id}`, …), so loading or refreshing a sub-route URL directly now
  hydrates the app instead of returning a 404. Asset-looking misses (a path with a
  file extension, e.g. a stale `/assets/old.js`) still correctly 404. If you are
  validating an **older binary**, deep links may still 404 — always start at `/`
  there.
- **Windows process teardown.** Playwright self-boot teardown uses a negative-PID
  signal (POSIX only). Use external-server mode (`SQI_BASE_URL`) on Windows
  (§B.2).
- **macOS AppleDouble files.** On non-APFS volumes macOS writes `._*` sidecars;
  the tooling already filters them (Playwright `testIgnore`, Vitest exclude, the
  embed filter). Harmless on Linux/Windows.
- **`--allow-root`.** The worker refuses to run as root unless `--allow-root`
  (or `SQI_WORKER_ALLOW_ROOT=true`) is set — relevant when validating in a
  container or as Administrator/root in a VM.

---

## Reporting results

For each platform you validate, record:

| Platform / arch | A. smoke (`make smoke`) | B. Playwright (engines) | C. manual UI | Notes |
| --- | --- | --- | --- | --- |
| macOS arm64 | | chromium/firefox/webkit | | |
| Linux (arm64 or amd64) | | | | |
| Windows (amd64) | | chromium/firefox | | |

A platform is "validated" when **A passes** and **B passes for at least one
engine** (plus the manual **C** sign-off for the cross-browser claim).
