# `scripts/`

Developer and CI scripts that are easier to express as a script than as a Makefile recipe:

- `smoke.sh` — end-to-end smoke test against the built binaries (REST + WebSocket + an EXPR job); run it via `make smoke`
- `auth-demo.sh` — drives the auth surface against a live local farm; run it via `make auth-demo` (`KEEP=1` leaves the farm running)
- `test-postgres.sh` — runs the PostgreSQL-backed tests against a throwaway server in Docker; run it via `make test-postgres`
- `chaos-postgres.sh` — runs the chaos suite on PostgreSQL; used by the Makefile's chaos targets with `STORE=postgres`
- `lib/pg-throwaway.sh` — the throwaway PostgreSQL server the two scripts above source
- `test-nats-cluster.sh` — runs the external-broker tests against a three-node NATS cluster in Docker; run it via `make test-nats-cluster`
- `test-ha.sh` — runs the high-availability tests against real servers on PostgreSQL and the NATS cluster; run it via `make test-ha`
- `macos-sign.sh` — Developer ID codesigning hook invoked by goreleaser's post-build hooks; a no-op for non-darwin targets and for snapshot builds
- `expr-oracle.py` — feeds `test/oracle/corpus.txt` to the pinned OpenJD reference implementation over JSON lines for `make test-expr-oracle`
- `assert-gated-tests.sh` — checks that named tests passed in a `go test -v` log, failing only for those listed in `GATED`; used by the chaos jobs of `load-nightly.yml`
- `test-isolation-windows.ps1` — runs the Windows run-as-user isolation suite (tier 1 as the elevated admin, tier 2 re-launched as SYSTEM via a scheduled task); run it via `make test-isolation-windows` from an elevated shell

Shell scripts here should be POSIX-portable where reasonable
(`#!/usr/bin/env bash`, `set -euo pipefail`) and document their inputs at the
top. The PowerShell and Python scripts are the exceptions: one is Windows-only,
the other drives the reference implementation.
