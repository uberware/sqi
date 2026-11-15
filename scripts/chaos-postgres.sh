#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Runs `go test "$@"` with the chaos suite on PostgreSQL: a throwaway server in
# Docker (PG_VERSION selects 16 or 18), or the one SQI_TEST_POSTGRES_DSN names.
# Unlike scripts/test-postgres.sh, missing Docker is an error: a chaos run that
# asked for PostgreSQL must not quietly run on nothing. Used by the Makefile's
# chaos targets with STORE=postgres.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/pg-throwaway.sh
. "$SCRIPT_DIR/lib/pg-throwaway.sh"

pg_throwaway_image || exit $?
rc=0
pg_throwaway_start || rc=$?
if [ "$rc" -eq 3 ]; then
  echo "docker unavailable: STORE=postgres needs Docker, or SQI_TEST_POSTGRES_DSN naming a PostgreSQL server" >&2
  exit 1
elif [ "$rc" -ne 0 ]; then
  exit "$rc"
fi
export SQI_CHAOS_STORE=postgres
export SQI_TEST_REQUIRE_POSTGRES=1

go test "$@"
