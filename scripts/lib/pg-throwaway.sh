# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Sourced by scripts/test-postgres.sh and scripts/chaos-postgres.sh: a
# throwaway PostgreSQL server in Docker.
#
# pg_throwaway_image   sets IMAGE from PG_VERSION (16 or 18) unless
#                      SQI_TEST_POSTGRES_IMAGE names one. The patch tags are
#                      pinned here; keep them in step with the matrix images of
#                      the postgres-integration job in .github/workflows/ci.yml.
# pg_throwaway_start   exports SQI_TEST_POSTGRES_DSN for a fresh server and
#                      removes the server when the shell exits. It does nothing
#                      when SQI_TEST_POSTGRES_DSN is already set, and returns 3
#                      when Docker is unavailable, 1 on any other failure.
#
# The caller runs under set -e, which does not apply inside a function called
# from a || list, so every step here returns its own status.

pg_throwaway_image() {
  case "${PG_VERSION:-18}" in
    16) IMAGE="postgres:16.15" ;;
    18) IMAGE="postgres:18.6" ;;
    *) echo "PG_VERSION must be 16 or 18" >&2; return 2 ;;
  esac
  IMAGE="${SQI_TEST_POSTGRES_IMAGE:-$IMAGE}"
}

pg_throwaway_start() {
  [ -z "${SQI_TEST_POSTGRES_DSN:-}" ] || return 0
  docker info >/dev/null 2>&1 || return 3
  # Docker picks the host port (-p 127.0.0.1::5432); it is read back below.
  NAME="sqi-pg-test-$$"
  docker run -d --rm --name "$NAME" --tmpfs /var/lib/postgresql \
    -e POSTGRES_PASSWORD=sqi -p 127.0.0.1::5432 "$IMAGE" >/dev/null || return $?
  trap 'docker rm -f "$NAME" >/dev/null 2>&1 || true' EXIT
  # -h 127.0.0.1 makes pg_isready use TCP. The image's entrypoint first runs a
  # temporary server that listens on the Unix socket only, then restarts it;
  # the default (socket) probe passes during that temporary phase and the
  # tests would race the restart. Only the final server listens on TCP.
  for _ in $(seq 1 60); do
    docker exec "$NAME" pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1 && break
    sleep 1
  done
  docker exec "$NAME" pg_isready -h 127.0.0.1 -U postgres >/dev/null || return $?
  PORT="$(docker port "$NAME" 5432/tcp | head -n 1 | sed 's/.*://')"
  [ -n "$PORT" ] || { echo "could not read the published port of $NAME" >&2; return 1; }
  export SQI_TEST_POSTGRES_DSN="postgres://postgres:sqi@127.0.0.1:${PORT}/postgres?sslmode=disable"
}
