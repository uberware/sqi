#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Runs the TestNATSCluster_ integration tests against a real three-node
# nats-server cluster in Docker (deploy/docker-compose.nats-cluster.yml),
# configured the way an operator would configure it: the accounts and auth
# callout from `sqi-server nats external init`, and the TLS certificate from
# `sqi-server tls init`.
#
# Usage (normally through make):
#   make test-nats-cluster                                    # current image
#   make test-nats-cluster NATS_IMAGE='$(NATS_IMAGE_FLOOR)'   # floor image
#
# Environment:
#   NATS_IMAGE  nats-server image to run (required; the Makefile supplies it)
#   TEST_FLAGS  extra `go test` flags (the Makefile's, e.g. -race)
#
# Exits 0 with "docker unavailable" when there is no Docker, as the other
# container targets do. The tests skip without the cluster, so check for the
# six `--- PASS: TestNATSCluster_` lines (CI does).
#
# Docker Desktop and Colima share only some host directories into the VM that
# runs the containers (Colima shares $HOME), and a bind mount from anywhere
# else comes up empty. The cluster directory is therefore made under
# $HOME/.cache, not $TMPDIR, and removed on exit.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="${REPO_ROOT}/deploy/docker-compose.nats-cluster.yml"
NODE_CONF="${REPO_ROOT}/deploy/nats-cluster/nats.conf"

: "${NATS_IMAGE:?set NATS_IMAGE to the nats-server image to test (make test-nats-cluster sets it)}"
TEST_FLAGS="${TEST_FLAGS:-}"

log() { printf '[nats-cluster] %s\n' "$*" >&2; }

if ! docker info >/dev/null 2>&1; then
  echo "docker unavailable — skipping NATS cluster integration tests"
  exit 0
fi
command -v python3 >/dev/null 2>&1 || { log "python3 is required to pick free ports"; exit 1; }

# The Docker disk on a Colima VM is small and fixed; dangling images from
# earlier container targets fill it.
docker image prune -f >/dev/null

mkdir -p "${HOME}/.cache"
WORK="$(mktemp -d "${HOME}/.cache/sqi-nats-cluster.XXXXXX")"
PROJECT="sqi-nats-cluster-$$"
export NATS_CLUSTER_DIR="${WORK}/cluster"

compose() { docker compose -p "$PROJECT" -f "$COMPOSE_FILE" "$@"; }

cleanup() {
  local status=$?
  set +e
  if [ "$status" -ne 0 ]; then
    log "cluster logs (tail):"
    compose logs --tail 40 >&2 2>&1
  fi
  compose down -v --remove-orphans >/dev/null 2>&1
  rm -rf "$WORK"
  return "$status"
}
# cleanup runs once, on EXIT; a signal exits with the conventional status,
# which the EXIT trap then reports.
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Three distinct free loopback ports, fixed for the run: a node stopped and
# started again by TestNATSCluster_SurvivesNodeLoss must come back on the
# same one.
read -r P1 P2 P3 < <(python3 - <<'PY'
import socket
socks = []
for _ in range(3):
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

log "building sqi-server"
(cd "$REPO_ROOT" && go build -o "${WORK}/bin/sqi-server" ./cmd/sqi-server)
SERVER_BIN="${WORK}/bin/sqi-server"

# The operator path: generate the keys and the account fragment, and a
# certificate naming the address the clients dial.
log "generating the cluster configuration with sqi-server nats external init and tls init"
"$SERVER_BIN" nats external init --out "${WORK}/sqi" >/dev/null
"$SERVER_BIN" tls init --out "${WORK}/tls" --host 127.0.0.1 --host localhost >/dev/null

# Only what the nodes need goes into the mounted directory: the seeds stay
# with sqi-server.
mkdir -p "$NATS_CLUSTER_DIR"
cp "$NODE_CONF" "${WORK}/sqi/sqi-accounts.conf" "${WORK}/tls/server.crt" "${WORK}/tls/server.key" \
  "$NATS_CLUSTER_DIR/"

log "starting a three-node cluster on ${NATS_IMAGE} (client ports ${P1}, ${P2}, ${P3})"
docker pull -q "$NATS_IMAGE" >/dev/null
compose up -d

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
(cd "$REPO_ROOT" && go test $TEST_FLAGS -tags integration -run 'TestNATSCluster_' -v -timeout 20m ./test/integration/)
