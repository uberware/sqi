#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Runs the TestHA_ integration tests: a high-availability group of real
# sqi-server binaries sharing one PostgreSQL database and one account on a
# real three-node NATS cluster, with a real sqi-worker. The cluster is the one
# scripts/test-nats-cluster.sh runs (deploy/docker-compose.nats-cluster.yml,
# configured from `sqi-server nats external init` and `tls init`), and
# PostgreSQL is a container of its own, as in scripts/test-postgres.sh, so the
# tests can stop and start it.
#
# Usage (normally through make):
#   make test-ha
#
# Environment:
#   NATS_IMAGE      nats-server image to run (required; the Makefile supplies it)
#   POSTGRES_IMAGE  PostgreSQL image (default below)
#   TEST_FLAGS      extra `go test` flags (the Makefile's, e.g. -race)
#   HA_RUN          the -run pattern (default TestHA_), to run one test
#   SQI_TEST_HA_LOG_DIR  a directory to keep every server's whole output in,
#                   one file per run (by default only a failure's tail is kept)
#
# Exits 0 with "docker unavailable" when there is no Docker, as the other
# container targets do. The tests skip without this script's environment, so
# check for the nine `--- PASS: TestHA_` lines (CI does).
#
# The PostgreSQL container keeps its data in the container (no tmpfs), so it
# survives the stop and start TestHA_PostgresRestart puts it through, and it is
# published on a fixed port for the same reason. Bind-mounted directories live
# under $HOME/.cache, as in scripts/test-nats-cluster.sh, because Colima shares
# only $HOME into its VM.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="${REPO_ROOT}/deploy/docker-compose.nats-cluster.yml"
NODE_CONF="${REPO_ROOT}/deploy/nats-cluster/nats.conf"
HA_COMPOSE_FILE="${REPO_ROOT}/deploy/docker-compose.ha.yml"

: "${NATS_IMAGE:?set NATS_IMAGE to the nats-server image to test (make test-ha sets it)}"
# Keep in step with the image in deploy/docker-compose.ha.yml and the
# ha-integration job in .github/workflows/ci.yml.
POSTGRES_IMAGE="${POSTGRES_IMAGE:-postgres:18.6}"
TEST_FLAGS="${TEST_FLAGS:-}"

log() { printf '[ha] %s\n' "$*" >&2; }

if ! docker info >/dev/null 2>&1; then
  echo "docker unavailable — skipping high-availability integration tests"
  exit 0
fi
command -v python3 >/dev/null 2>&1 || { log "python3 is required to pick free ports"; exit 1; }

# The Docker disk on a Colima VM is small and fixed; dangling images from
# earlier container targets fill it.
docker image prune -f >/dev/null

mkdir -p "${HOME}/.cache"
WORK="$(mktemp -d "${HOME}/.cache/sqi-ha.XXXXXX")"
PROJECT="sqi-ha-$$"
PG_NAME="sqi-ha-pg-$$"
export NATS_CLUSTER_DIR="${WORK}/cluster"

compose() { docker compose -p "$PROJECT" -f "$COMPOSE_FILE" "$@"; }

# pull fetches an image that is not already here. CI pulls every image itself
# beforehand; skipping a present one keeps a local run off the registry's rate
# limit.
pull() { docker image inspect "$1" >/dev/null 2>&1 || docker pull -q "$1" >/dev/null; }

cleanup() {
  local status=$?
  set +e
  if [ "$status" -ne 0 ]; then
    log "cluster logs (tail):"
    compose logs --tail 40 >&2 2>&1
    log "postgres logs (tail):"
    docker logs --tail 40 "$PG_NAME" >&2 2>&1
  fi
  compose down -v --remove-orphans >/dev/null 2>&1
  docker rm -f -v "$PG_NAME" >/dev/null 2>&1
  rm -rf "$WORK"
  return "$status"
}
# cleanup runs once, on EXIT; a signal exits with the conventional status,
# which the EXIT trap then reports.
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Four distinct free loopback ports, fixed for the run: three NATS client
# ports and PostgreSQL's. A node or the database stopped and started again by
# a test must come back on the same one.
read -r P1 P2 P3 PG_PORT < <(python3 - <<'PY'
import socket
socks = []
for _ in range(4):
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.bind(("127.0.0.1", 0))
    socks.append(s)
print(" ".join(str(s.getsockname()[1]) for s in socks))
for s in socks:
    s.close()
PY
)
export NATS_CLUSTER_BIND=127.0.0.1 NATS_CLUSTER_1_PORT="$P1" NATS_CLUSTER_2_PORT="$P2" NATS_CLUSTER_3_PORT="$P3"
export NATS_IMAGE

# The reference HA Compose file is not run here (the tests need to signal the
# servers, so they run them as processes), but it must stay valid.
log "validating deploy/docker-compose.ha.yml"
SQI_HA_DIR="${WORK}/compose" SQI_POSTGRES_PASSWORD=unused SQI_WORKER_JOIN_TOKEN=unused \
  docker compose -f "$HA_COMPOSE_FILE" config -q

log "building sqi-server"
(cd "$REPO_ROOT" && go build -o "${WORK}/bin/sqi-server" ./cmd/sqi-server)
SERVER_BIN="${WORK}/bin/sqi-server"

log "generating the cluster configuration with sqi-server nats external init and tls init"
"$SERVER_BIN" nats external init --out "${WORK}/sqi" >/dev/null
"$SERVER_BIN" tls init --out "${WORK}/tls" --host 127.0.0.1 --host localhost >/dev/null

mkdir -p "$NATS_CLUSTER_DIR"
cp "$NODE_CONF" "${WORK}/sqi/sqi-accounts.conf" "${WORK}/tls/server.crt" "${WORK}/tls/server.key" \
  "$NATS_CLUSTER_DIR/"

log "starting a three-node cluster on ${NATS_IMAGE} (client ports ${P1}, ${P2}, ${P3})"
pull "$NATS_IMAGE"
compose up -d

log "starting ${POSTGRES_IMAGE} on port ${PG_PORT}"
pull "$POSTGRES_IMAGE"
docker run -d --name "$PG_NAME" -e POSTGRES_PASSWORD=sqi \
  -p "127.0.0.1:${PG_PORT}:5432" "$POSTGRES_IMAGE" >/dev/null
# -h 127.0.0.1 makes pg_isready use TCP: the image first runs a temporary,
# socket-only server, and only the final one listens on TCP.
for _ in $(seq 1 60); do
  docker exec "$PG_NAME" pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1 && break
  sleep 1
done
docker exec "$PG_NAME" pg_isready -h 127.0.0.1 -U postgres >/dev/null

export SQI_TEST_HA=1
export SQI_TEST_HA_POSTGRES_CONTAINER="$PG_NAME"
export SQI_TEST_POSTGRES_DSN="postgres://postgres:sqi@127.0.0.1:${PG_PORT}/postgres?sslmode=disable"
export SQI_TEST_REQUIRE_POSTGRES=1

export SQI_TEST_NATS_URLS="tls://127.0.0.1:${P1},tls://127.0.0.1:${P2},tls://127.0.0.1:${P3}"
export SQI_TEST_NATS_NODES="nats-1,nats-2,nats-3"
export SQI_TEST_NATS_DATA_SEED="${WORK}/sqi/sqi-data.nk"
export SQI_TEST_NATS_CALLOUT_SEED="${WORK}/sqi/sqi-auth.nk"
export SQI_TEST_NATS_ISSUER_SEED="${WORK}/sqi/issuer.nk"
export SQI_TEST_NATS_SYSTEM_SEED="${WORK}/sqi/sqi-sys.nk"
export SQI_TEST_NATS_CA_FILE="${WORK}/tls/ca.crt"
export SQI_TEST_NATS_IMAGE="$NATS_IMAGE"
export SQI_TEST_NATS_COMPOSE_FILE="$COMPOSE_FILE"
export SQI_TEST_NATS_COMPOSE_PROJECT="$PROJECT"
export SQI_TEST_SERVER_BIN="$SERVER_BIN"

# shellcheck disable=SC2086 # TEST_FLAGS is a list of flags
(cd "$REPO_ROOT" && go test $TEST_FLAGS -count=1 -tags integration -run "${HA_RUN:-TestHA_}" -v -timeout 45m ./test/integration/)
