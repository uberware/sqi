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
# A local PG_VERSION=16 run needs a PG 16 pg_dump and pg_restore FIRST on PATH:
# TestPostgres_BackupRestoreRoundTrip restores a dump into the server, and a
# newer client's dump carries settings (e.g. SET transaction_timeout) that an
# older server rejects. CI installs the matching client for each leg.
set -euo pipefail

PG_VERSION="${PG_VERSION:-18}"
# Default pinned patch tags, used when SQI_TEST_POSTGRES_IMAGE is unset. Keep
# in step with the matrix images of the postgres-integration job in
# .github/workflows/ci.yml, which pulls (and passes) them.
case "$PG_VERSION" in
  16) IMAGE="postgres:16.15" ;;
  18) IMAGE="postgres:18.6" ;;
  *) echo "PG_VERSION must be 16 or 18" >&2; exit 2 ;;
esac
IMAGE="${SQI_TEST_POSTGRES_IMAGE:-$IMAGE}"
# -race unless TEST_FLAGS is set; make passes it empty for RACE=off.
RACE_FLAG="${TEST_FLAGS--race}"

if [ -z "${SQI_TEST_POSTGRES_DSN:-}" ]; then
  if ! docker info >/dev/null 2>&1; then
    echo "docker unavailable — skipping PostgreSQL tests"
    exit 0
  fi
  # Docker picks the host port (-p 127.0.0.1::5432) and it is read back below,
  # so there is no free-port probe to race and no python3 dependency.
  NAME="sqi-pg-test-$$"
  docker run -d --rm --name "$NAME" --tmpfs /var/lib/postgresql \
    -e POSTGRES_PASSWORD=sqi -p 127.0.0.1::5432 "$IMAGE" >/dev/null
  trap 'docker rm -f "$NAME" >/dev/null 2>&1 || true' EXIT
  # -h 127.0.0.1 makes pg_isready use TCP. The image's entrypoint first runs a
  # temporary server that listens on the Unix socket only, then restarts it;
  # the default (socket) probe passes during that temporary phase and the
  # tests would race the restart. Only the final server listens on TCP.
  for _ in $(seq 1 60); do
    docker exec "$NAME" pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1 && break
    sleep 1
  done
  docker exec "$NAME" pg_isready -h 127.0.0.1 -U postgres >/dev/null
  PORT="$(docker port "$NAME" 5432/tcp | head -n 1 | sed 's/.*://')"
  [ -n "$PORT" ] || { echo "could not read the published port of $NAME" >&2; exit 1; }
  export SQI_TEST_POSTGRES_DSN="postgres://postgres:sqi@127.0.0.1:${PORT}/postgres?sslmode=disable"
fi
export SQI_TEST_REQUIRE_POSTGRES=1

echo "== PostgreSQL ${PG_VERSION}: store, scheduler, api, server and cmd packages"
go test $RACE_FLAG -count=1 -v -timeout 30m \
  ./internal/store/... ./internal/scheduler/... ./internal/api/... ./internal/server/... ./cmd/...

echo "== PostgreSQL ${PG_VERSION}: store packages with every transaction rehearsed once"
SQI_TEST_PG_FORCE_TX_RETRY=1 go test $RACE_FLAG -count=1 -timeout 30m ./internal/store/...

echo "== PostgreSQL ${PG_VERSION}: integration suite with the server on PostgreSQL"
SQI_TEST_STORE_DRIVER=postgres go test $RACE_FLAG -count=1 -v -tags integration -timeout 20m \
  -skip 'TestLDAP_|TestOIDC_|TestIsolation_' ./test/integration/

echo "POSTGRES TESTS PASSED (PG_VERSION=${PG_VERSION})"
