#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Runs the PostgreSQL-backed tests against a throwaway server in Docker.
# PG_VERSION selects the major version (16 or 18). Without Docker it says so
# and exits 0, so a local run proves nothing on its own; CI asserts the tests
# by name. Set SQI_TEST_POSTGRES_DSN to use an existing server instead.
set -euo pipefail

PG_VERSION="${PG_VERSION:-18}"
# Pinned patch tags. Keep in step with the docker pull steps in
# .github/workflows/ci.yml (postgres-integration jobs).
case "$PG_VERSION" in
  16) IMAGE="postgres:16.15" ;;
  18) IMAGE="postgres:18.6" ;;
  *) echo "PG_VERSION must be 16 or 18" >&2; exit 2 ;;
esac
# -race unless TEST_FLAGS is set; make passes it empty for RACE=off.
RACE_FLAG="${TEST_FLAGS--race}"

if [ -z "${SQI_TEST_POSTGRES_DSN:-}" ]; then
  if ! docker info >/dev/null 2>&1; then
    echo "docker unavailable — skipping PostgreSQL tests"
    exit 0
  fi
  PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')"
  NAME="sqi-pg-test-${PORT}"
  docker run -d --rm --name "$NAME" --tmpfs /var/lib/postgresql \
    -e POSTGRES_PASSWORD=sqi -p "127.0.0.1:${PORT}:5432" "$IMAGE" >/dev/null
  trap 'docker rm -f "$NAME" >/dev/null 2>&1 || true' EXIT
  for _ in $(seq 1 60); do
    docker exec "$NAME" pg_isready -U postgres >/dev/null 2>&1 && break
    sleep 1
  done
  docker exec "$NAME" pg_isready -U postgres >/dev/null
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
