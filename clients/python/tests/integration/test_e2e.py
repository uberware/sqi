# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""End-to-end integration tests against real sqi-server / sqi-worker binaries.

Run with ``pytest -m integration`` (deselected from the default unit run). The
harness fixtures (``server``, ``client``, ``worker_farm``) live in conftest.py
and skip cleanly when the binaries are unavailable.
"""

from __future__ import annotations

import time
import uuid

import pytest

from sqi_client import JobStatus, SqiClient, TaskStatus, WorkerStatus
from tests.integration.conftest import WorkerFarm

# These boot real binaries and run real jobs, so they need a wall-clock budget
# well above the unit suite's global timeout (and above the tests' own internal
# poll timeouts) to avoid the harness timeout pre-empting a clean failure.
pytestmark = [pytest.mark.integration, pytest.mark.timeout(180)]

# A single-step OpenJD job whose echo output we can assert on in the logs.
_ECHO_JOB = """specificationVersion: "jobtemplate-2023-09"
name: sqi-sdk integration echo
steps:
  - name: Run
    script:
      actions:
        onRun:
          command: echo
          args:
            - "hello from sqi-sdk integration"
"""

# A job whose command exits non-zero, so its task ends in the failed state.
_FAILING_JOB = """specificationVersion: "jobtemplate-2023-09"
name: sqi-sdk integration fail
steps:
  - name: Run
    script:
      actions:
        onRun:
          command: sh
          args:
            - "-c"
            - "exit 7"
"""

# A parameter-space job that expands to several sleeping tasks, each requiring a
# seat in a named usage pool. "POOL_NAME" is replaced with the test's unique
# pool name before submission. The worker has 4 concurrency slots, so absent
# usage gating these tasks would run in parallel — the pool's max_concurrent
# is the only thing that can hold them to a lower concurrency.
_USAGE_POOL_SLEEP_JOB = """specificationVersion: "jobtemplate-2023-09"
name: sqi-sdk integration usage pool sleep
steps:
  - name: Run
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: INT
          range: "1-3"
    hostRequirements:
      amounts:
        - name: amount.worker.usagepool.POOL_NAME
          min: 1
    script:
      actions:
        onRun:
          command: sh
          args:
            - "-c"
            - "sleep 2"
"""

_ECHO_TEXT = "hello from sqi-sdk integration"


def _poll_task_logs(client: SqiClient, task_id: str, needle: str, timeout: float) -> str:
    """Poll get_task_logs until ``needle`` appears, returning the full log text."""
    deadline = time.monotonic() + timeout
    text = ""
    while time.monotonic() < deadline:
        page = client.get_task_logs(task_id)
        text = "".join(chunk.data for chunk in page.items)
        if needle in text:
            return text
        time.sleep(0.5)
    return text


def _first_task_id(client: SqiClient, job_id: str, timeout: float) -> str:
    """Poll list_job_tasks until the job has at least one task; return its id."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        page = client.list_job_tasks(job_id)
        if page.items:
            return page.items[0].id
        time.sleep(0.3)
    raise AssertionError(f"job {job_id} produced no tasks within {timeout}s")


# ── Submit, execute, query logs ──────────────────────────────────────


def test_submit_execute_and_fetch_logs(client: SqiClient, worker_farm: WorkerFarm) -> None:
    job = client.submit_and_wait(
        _ECHO_JOB,
        farm_id=worker_farm.farm_id,
        queue_id=worker_farm.queue_id,
        owner="integration",
        poll_interval=0.5,
        timeout=60.0,
    )
    assert job.status is JobStatus.COMPLETED

    task = client.list_job_tasks(job.id).items[0]
    logs = _poll_task_logs(client, task.id, _ECHO_TEXT, timeout=15.0)
    assert _ECHO_TEXT in logs


# ── Every mutating endpoint ──────────────────────────────────────────


def test_management_endpoints(client: SqiClient, worker_farm: WorkerFarm) -> None:
    # Pause/resume/priority/cancel need a job that stays pending, so use a fresh
    # farm/queue with no worker bound to it.
    idle_farm = client.create_farm(name="sqi-sdk mgmt farm")
    idle_queue = client.create_queue(farm_id=idle_farm.id, name="sqi-sdk mgmt queue")

    job = client.submit_job(_ECHO_JOB, farm_id=idle_farm.id, queue_id=idle_queue.id)
    assert client.get_job(job.id).status is JobStatus.PENDING

    assert client.pause_job(job.id).status is JobStatus.PAUSED
    assert client.resume_job(job.id).status is JobStatus.PENDING
    assert client.set_job_priority(job.id, 90).priority == 90
    client.cancel_job(job.id)
    assert client.get_job(job.id).status is JobStatus.CANCELED

    # Retry needs a genuinely failed task, so run a failing job on the worker.
    failed = client.submit_and_wait(
        _FAILING_JOB,
        farm_id=worker_farm.farm_id,
        queue_id=worker_farm.queue_id,
        poll_interval=0.5,
        timeout=60.0,
    )
    assert failed.status is JobStatus.FAILED
    failed_task = next(
        t for t in client.list_job_tasks(failed.id).items if t.status is TaskStatus.FAILED
    )
    retry = client.retry_task(failed_task.id)
    assert retry.task_id == failed_task.id

    # Disable then re-enable the worker. The worker is session-scoped and shared
    # with the other tests, so always restore it — even if the disabled-state
    # assertion fails — to avoid cascading a hang into the next test.
    disabled = client.disable_worker(worker_farm.worker_id)
    restored = None
    try:
        assert disabled is not None
        assert disabled.status is WorkerStatus.DISABLED
    finally:
        restored = client.enable_worker(worker_farm.worker_id)
    assert restored is not None
    assert restored.status is not WorkerStatus.DISABLED


# ── Live WebSocket log tailing ───────────────────────────────────────


def test_websocket_live_log_tail(client: SqiClient, worker_farm: WorkerFarm) -> None:
    pytest.importorskip("websockets")

    job = client.submit_job(_ECHO_JOB, farm_id=worker_farm.farm_id, queue_id=worker_farm.queue_id)
    # The task exists at submit time, before the worker picks it up and executes,
    # so subscribing now (since_seq=0 is live-only) reliably precedes the echo
    # output, which then arrives live.
    task_id = _first_task_id(client, job.id, timeout=30.0)

    # Live-tail over WebSocket until the echoed line arrives.
    live_text = ""
    for chunk in client.tail_task_logs_live(task_id, from_seq=0):
        live_text += chunk.data
        if _ECHO_TEXT in chunk.data:
            break
    assert _ECHO_TEXT in live_text

    # What arrived live matches what polling returns after the job finishes.
    client.wait_for_job(job.id, poll_interval=0.5, timeout=60.0)
    polled = _poll_task_logs(client, task_id, _ECHO_TEXT, timeout=15.0)
    assert _ECHO_TEXT in polled


# ── Usage-pool concurrency gating ────────────────────────────────────


def test_usage_pool_caps_concurrent_tasks(client: SqiClient, worker_farm: WorkerFarm) -> None:
    # A pool with a single seat: at most one task holding it may run at a time,
    # globally across the farm.
    pool_name = f"it-pool-{uuid.uuid4().hex[:8]}"
    pool = client.create_usage_pool(name=pool_name, max_concurrent=1)

    template = _USAGE_POOL_SLEEP_JOB.replace("POOL_NAME", pool_name)
    job = client.submit_job(template, farm_id=worker_farm.farm_id, queue_id=worker_farm.queue_id)

    # Sample live utilization while the job runs. Each task sleeps ~2s and the
    # worker has 4 free slots, so without gating the in-use count would climb
    # above 1; gating must hold it at or below the pool's max_concurrent.
    terminal = {JobStatus.COMPLETED, JobStatus.FAILED, JobStatus.CANCELED}
    max_seen = 0
    deadline = time.monotonic() + 120.0
    while time.monotonic() < deadline:
        max_seen = max(max_seen, client.get_usage_pool(pool.id).in_use)
        if client.get_job(job.id).status in terminal:
            break
        time.sleep(0.2)

    job = client.wait_for_job(job.id, poll_interval=0.5, timeout=30.0)
    assert job.status is JobStatus.COMPLETED
    # The pool was genuinely exercised (a seat was held at least once)...
    assert max_seen >= 1, "expected at least one active usage claim during the run"
    # ...and never exceeded its single seat.
    assert max_seen <= 1, f"usage pool exceeded its cap: saw {max_seen} concurrent claims"

    # Every seat is released once the job reaches a terminal state — no leak.
    assert client.get_usage_pool(pool.id).in_use == 0
