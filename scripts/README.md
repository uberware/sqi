# `scripts/`

Developer and CI scripts that are easier to express as a script than as a Makefile recipe:

- `smoke.sh` — end-to-end smoke test against the built binaries (REST + WebSocket + an EXPR job); run it via `make smoke`
- `auth-demo.sh` — drives the auth surface against a live local farm; run it via `make auth-demo` (`KEEP=1` leaves the farm running)
- `macos-sign.sh` — Developer ID codesigning hook invoked by goreleaser's post-build hooks; a no-op for non-darwin targets and for snapshot builds
- `expr-oracle.py` — feeds `test/oracle/corpus.txt` to the pinned OpenJD reference implementation over JSON lines for `make test-expr-oracle`
- `test-isolation-windows.ps1` — runs the Windows run-as-user isolation suite (tier 1 as the elevated admin, tier 2 re-launched as SYSTEM via a scheduled task); run it via `make test-isolation-windows` from an **elevated** shell

Shell scripts here should be POSIX-portable where reasonable
(`#!/usr/bin/env bash`, `set -euo pipefail`) and document their inputs at the
top. The PowerShell and Python entries above are deliberate exceptions —
Windows-only and reference-implementation glue respectively.
