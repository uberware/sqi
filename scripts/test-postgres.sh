#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Runs the PostgreSQL-backed tests against a throwaway server in Docker.
# PG_VERSION selects the major version (16 or 18). Without Docker it says so
# and exits 0, so a local run proves nothing on its own; CI asserts the tests
# by name. Set SQI_TEST_POSTGRES_DSN to use an existing server instead.
#
# SQI_TEST_POSTGRES_IMAGE overrides the image the throwaway container runs.
# CI passes its matrix image through it, so the image CI pulled is the image
# the suite ran on.
#
# A local PG_VERSION=16 run needs a PG 16 pg_dump and pg_restore first on PATH:
# TestPostgres_BackupRestoreRoundTrip restores a dump into the server, and a
# newer client's dump carries settings (e.g. SET transaction_timeout) that an
# older server rejects. CI installs the matching client for each leg.
#
# PG_PHASE selects what runs: "all" (the default) runs the three phases below
# in order; "packages", "retry" or "integration" runs just that one. CI runs
# the three as separate jobs so they overlap.
set -euo pipefail

PG_VERSION="${PG_VERSION:-18}"
PG_PHASE="${PG_PHASE:-all}"
case "$PG_PHASE" in
  all|packages|retry|integration) ;;
  *) echo "PG_PHASE must be all, packages, retry or integration" >&2; exit 2 ;;
esac
# runs_phase NAME: true when PG_PHASE selects the phase NAME.
runs_phase() { [ "$PG_PHASE" = all ] || [ "$PG_PHASE" = "$1" ]; }
# The image pins live in scripts/lib/pg-throwaway.sh; keep them in step with
# the matrix images of the postgres-integration job in .github/workflows/ci.yml.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/pg-throwaway.sh
. "$SCRIPT_DIR/lib/pg-throwaway.sh"
pg_throwaway_image || exit $?
# -race unless TEST_FLAGS is set; make passes it empty for RACE=off.
RACE_FLAG="${TEST_FLAGS--race}"

rc=0
pg_throwaway_start || rc=$?
if [ "$rc" -eq 3 ]; then
  echo "docker unavailable — skipping PostgreSQL tests"
  exit 0
elif [ "$rc" -ne 0 ]; then
  exit "$rc"
fi
export SQI_TEST_REQUIRE_POSTGRES=1

if runs_phase packages; then
  echo "== PostgreSQL ${PG_VERSION}: store, scheduler, api, server, restore and cmd packages"
  go test $RACE_FLAG -count=1 -v -timeout 30m \
    ./internal/store/... ./internal/scheduler/... ./internal/api/... ./internal/server/... ./internal/restore/... ./cmd/...
fi

# -v, and the BEGIN/END lines around it, let CI find this phase's own PASS and
# SKIP lines and check that its /postgres subtests really ran.
if runs_phase retry; then
  echo "== PostgreSQL ${PG_VERSION}: store packages with every transaction rehearsed once"
  echo "== BEGIN FORCED-RETRY PHASE"
  SQI_TEST_PG_FORCE_TX_RETRY=1 go test $RACE_FLAG -count=1 -v -timeout 30m ./internal/store/...
  echo "== END FORCED-RETRY PHASE"
fi

if runs_phase integration; then
  echo "== PostgreSQL ${PG_VERSION}: integration suite with the server on PostgreSQL"
  SQI_TEST_STORE_DRIVER=postgres go test $RACE_FLAG -count=1 -v -tags integration -timeout 30m \
    -skip 'TestLDAP_|TestOIDC_|TestIsolation_' ./test/integration/
fi

if [ "$PG_PHASE" = all ]; then
  echo "POSTGRES TESTS PASSED (PG_VERSION=${PG_VERSION})"
else
  echo "POSTGRES TESTS PASSED (PG_VERSION=${PG_VERSION} PHASE=${PG_PHASE})"
fi
