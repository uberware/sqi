# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Integration-test harness: boots real sqi-server / sqi-worker binaries.

These fixtures power the ``@pytest.mark.integration`` tests (deselected from the
default unit run via ``addopts = -m 'not integration'``). They locate the
binaries via ``SQI_SERVER_BIN`` / ``SQI_WORKER_BIN`` (defaulting to
``<repo-root>/bin/``), boot a server on free ports with a temp-directory SQLite
database, and optionally boot a worker bound to a test farm/queue over NATS.
The whole marker is skipped with a clear message when a binary is missing, so
the suite never fails merely because the binaries were not built.
"""

from __future__ import annotations

import contextlib
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
from collections.abc import Iterator
from dataclasses import dataclass
from pathlib import Path

import httpx
import pytest

from sqi_client import SqiClient, WorkerStatus

# clients/python/tests/integration/conftest.py → repo root is four levels up.
_REPO_ROOT = Path(__file__).resolve().parents[4]


def _binary(env_var: str, default_name: str) -> Path:
    override = os.environ.get(env_var)
    return Path(override) if override else _REPO_ROOT / "bin" / default_name


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        port: int = sock.getsockname()[1]
        return port


def _wait_for_ready(url: str, proc: subprocess.Popen[bytes], timeout: float) -> bool:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            return False  # process exited early (crash / bad config) — fail fast
        with contextlib.suppress(httpx.HTTPError):
            if httpx.get(url, timeout=2.0).status_code == 200:
                return True
        time.sleep(0.2)
    return False


def _terminate(proc: subprocess.Popen[bytes]) -> None:
    if proc.poll() is not None:
        return
    proc.terminate()
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait(timeout=5)


@dataclass(frozen=True)
class Server:
    """A running sqi-server: its REST base URL and embedded NATS address."""

    base_url: str
    nats_addr: str


@pytest.fixture(scope="session")
def server() -> Iterator[Server]:
    """Boot a real sqi-server on free ports with a temp SQLite DB."""
    server_bin = _binary("SQI_SERVER_BIN", "sqi-server")
    if not server_bin.exists():
        pytest.skip(
            f"sqi-server binary not found at {server_bin}; "
            "build it with `make build` or set SQI_SERVER_BIN"
        )

    http_addr = f"127.0.0.1:{_free_port()}"
    nats_addr = f"127.0.0.1:{_free_port()}"
    tmp = Path(tempfile.mkdtemp(prefix="sqi-it-server-"))
    log_path = tmp / "server.log"

    env = {
        **os.environ,
        "SQI_HTTP_ADDR": http_addr,
        "SQI_NATS_ADDR": nats_addr,
        "SQI_NATS_DATA_DIR": str(tmp / "nats"),
        "SQI_STORE_SQLITE_PATH": str(tmp / "sqi.db"),
        "SQI_DISCOVERY_ENABLED": "false",
        "SQI_SCHEDULER_TICK_INTERVAL": "100ms",
        "SQI_LOG_LEVEL": "warn",
    }
    base_url = f"http://{http_addr}"

    with log_path.open("wb") as log_file:
        proc = subprocess.Popen(
            [str(server_bin), "serve"], env=env, stdout=log_file, stderr=log_file
        )
        try:
            if not _wait_for_ready(f"{base_url}/readyz", proc, 30.0):
                _terminate(proc)
                tail = log_path.read_text(errors="replace")[-2000:]
                raise RuntimeError(
                    f"sqi-server did not become ready within 30s\n--- server log ---\n{tail}"
                )
            yield Server(base_url=base_url, nats_addr=nats_addr)
        finally:
            _terminate(proc)
            shutil.rmtree(tmp, ignore_errors=True)


@pytest.fixture(scope="session")
def client(server: Server) -> Iterator[SqiClient]:
    """A SqiClient pointed at the running server."""
    with SqiClient(server.base_url, timeout=15.0) as sqi:
        yield sqi


@dataclass(frozen=True)
class WorkerFarm:
    """A farm/queue with a real sqi-worker bound to and serving it."""

    farm_id: str
    queue_id: str
    worker_id: str


def _wait_for_online_worker(
    client: SqiClient, farm_id: str, proc: subprocess.Popen[bytes], timeout: float
) -> str:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            raise RuntimeError(
                f"sqi-worker exited early (code {proc.returncode}) before coming online"
            )
        for worker in client.list_workers(farm_id=farm_id).items:
            if worker.status is WorkerStatus.ONLINE:
                return worker.id
        time.sleep(0.5)
    raise RuntimeError(f"no worker came online for farm {farm_id} within {timeout}s")


@pytest.fixture(scope="session")
def worker_farm(server: Server, client: SqiClient) -> Iterator[WorkerFarm]:
    """Create a farm/queue and boot a real sqi-worker that serves it."""
    if sys.platform == "win32":
        pytest.skip("real-worker integration tests require a POSIX shell; skipping on Windows")
    worker_bin = _binary("SQI_WORKER_BIN", "sqi-worker")
    if not worker_bin.exists():
        pytest.skip(
            f"sqi-worker binary not found at {worker_bin}; "
            "build it with `make build` or set SQI_WORKER_BIN"
        )

    farm = client.create_farm(name="sqi-client integration farm")
    queue = client.create_queue(farm_id=farm.id, name="sqi-client integration queue")

    tmp = Path(tempfile.mkdtemp(prefix="sqi-it-worker-"))
    log_path = tmp / "worker.log"
    env = {
        **os.environ,
        "SQI_WORKER_NATS_URL": f"nats://{server.nats_addr}",
        "SQI_WORKER_DISCOVERY_ENABLE_MDNS": "false",
        "SQI_WORKER_FARM_ID": farm.id,
        "SQI_WORKER_QUEUE_IDS": queue.id,
        "SQI_WORKER_DATA_DIR": str(tmp / "data"),
        "SQI_WORKER_ALLOW_ROOT": "true",
        "SQI_WORKER_LOG_LEVEL": "warn",
        "SQI_WORKER_LOG_FORMAT": "text",
        "SQI_WORKER_HEARTBEAT_INTERVAL": "1s",
        "SQI_WORKER_PULL_IDLE_BACKOFF": "200ms",
        "SQI_WORKER_METRICS_ADDR": f"127.0.0.1:{_free_port()}",
    }

    with log_path.open("wb") as log_file:
        proc = subprocess.Popen(
            [str(worker_bin), "start"], env=env, stdout=log_file, stderr=log_file
        )
        try:
            try:
                worker_id = _wait_for_online_worker(client, farm.id, proc, 30.0)
            except RuntimeError as exc:
                tail = log_path.read_text(errors="replace")[-2000:]
                raise RuntimeError(f"{exc}\n--- worker log ---\n{tail}") from exc
            yield WorkerFarm(farm_id=farm.id, queue_id=queue.id, worker_id=worker_id)
        finally:
            _terminate(proc)
            shutil.rmtree(tmp, ignore_errors=True)
