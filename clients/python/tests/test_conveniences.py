# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Tests for the high-level conveniences wait_for_job / submit_and_wait.

HTTP is mocked at the transport layer with respx; the wall clock is faked so the
polling loops run instantly and deterministically.
"""

from __future__ import annotations

import json
from typing import Any

import httpx
import pytest
import respx

from sqi_client.errors import SqiTimeoutError
from sqi_client.models import Job, JobStatus
from tests.conftest import BASE_URL, ClientFactory

_JOBS_URL = f"{BASE_URL}/api/v1/jobs"


def _job(status: str, job_id: str = "j1", **overrides: Any) -> dict[str, Any]:
    data: dict[str, Any] = {
        "id": job_id,
        "farm_id": "farm-1",
        "queue_id": "queue-1",
        "name": "My Job",
        "owner": "alice",
        "submitter": "",
        "priority": 50,
        "status": status,
        "template_format": "yaml",
        "created_at": "2026-01-15T10:00:00Z",
        "updated_at": "2026-01-15T10:00:00Z",
    }
    data.update(overrides)
    return data


@pytest.fixture
def fake_clock(monkeypatch: pytest.MonkeyPatch) -> None:
    """Replace the client's clock so poll loops advance without real waiting."""
    now = {"t": 0.0}
    monkeypatch.setattr("sqi_client.client.time.monotonic", lambda: now["t"])

    def fake_sleep(seconds: float) -> None:
        now["t"] += seconds

    monkeypatch.setattr("sqi_client.client.time.sleep", fake_sleep)


# ── wait_for_job ────────────────────────────────────────────────────


@pytest.mark.parametrize("status", ["completed", "failed", "canceled"])
@respx.mock
def test_wait_for_job_returns_on_terminal_status(make_client: ClientFactory, status: str) -> None:
    respx.get(f"{_JOBS_URL}/j1").mock(return_value=httpx.Response(200, json=_job(status)))
    client = make_client()

    job = client.wait_for_job("j1", poll_interval=0.0)

    assert isinstance(job, Job)
    assert job.status == status


@respx.mock
def test_wait_for_job_polls_until_terminal(make_client: ClientFactory, fake_clock: None) -> None:
    # Two non-terminal polls, then completed.
    responses = iter(
        [
            httpx.Response(200, json=_job("pending")),
            httpx.Response(200, json=_job("running")),
            httpx.Response(200, json=_job("completed")),
        ]
    )
    route = respx.get(f"{_JOBS_URL}/j1").mock(side_effect=lambda request: next(responses))
    client = make_client()

    job = client.wait_for_job("j1", poll_interval=2.0)

    assert job.status is JobStatus.COMPLETED
    assert route.call_count == 3


@respx.mock
def test_wait_for_job_times_out(make_client: ClientFactory, fake_clock: None) -> None:
    # The job never finishes; the deadline elapses and a timeout is raised.
    route = respx.get(f"{_JOBS_URL}/j1").mock(
        return_value=httpx.Response(200, json=_job("running"))
    )
    client = make_client()

    with pytest.raises(SqiTimeoutError):
        client.wait_for_job("j1", poll_interval=1.0, timeout=5.0)

    # Polled at t=0,1,2,3,4 then raised at t=5 (one final poll at the deadline).
    assert route.call_count == 6


@respx.mock
def test_wait_for_job_poll_cadence(
    make_client: ClientFactory, monkeypatch: pytest.MonkeyPatch
) -> None:
    # Sleeps the poll_interval between non-terminal polls.
    sleeps: list[float] = []
    monkeypatch.setattr("sqi_client.client.time.sleep", lambda s: sleeps.append(s))
    responses = iter(
        [httpx.Response(200, json=_job("running")), httpx.Response(200, json=_job("completed"))]
    )
    respx.get(f"{_JOBS_URL}/j1").mock(side_effect=lambda request: next(responses))
    client = make_client()

    client.wait_for_job("j1", poll_interval=2.5)

    assert sleeps == [2.5]


# ── submit_and_wait ─────────────────────────────────────────────────


@respx.mock
def test_submit_and_wait_returns_finished_job(make_client: ClientFactory) -> None:
    submit_route = respx.post(_JOBS_URL).mock(
        return_value=httpx.Response(201, json=_job("pending"))
    )
    respx.get(f"{_JOBS_URL}/j1").mock(return_value=httpx.Response(200, json=_job("completed")))
    client = make_client()

    job = client.submit_and_wait(
        {"name": "My Job"},
        farm_id="farm-1",
        queue_id="queue-1",
        owner="alice",
        priority=90,
        poll_interval=0.0,
    )

    # The finished job is returned, not the freshly-submitted (pending) one.
    assert job.status is JobStatus.COMPLETED
    # submit_job's arguments are passed through.
    submit_request = submit_route.calls.last.request
    assert json.loads(submit_request.content) == {"name": "My Job"}
    params = submit_request.url.params
    assert params["farm_id"] == "farm-1"
    assert params["queue_id"] == "queue-1"
    assert params["owner"] == "alice"
    assert params["priority"] == "90"


@respx.mock
def test_submit_and_wait_forwards_retry_overrides(make_client: ClientFactory) -> None:
    submit_route = respx.post(_JOBS_URL).mock(
        return_value=httpx.Response(201, json=_job("pending"))
    )
    respx.get(f"{_JOBS_URL}/j1").mock(return_value=httpx.Response(200, json=_job("failed")))
    client = make_client()

    client.submit_and_wait(
        {"name": "My Job"},
        farm_id="farm-1",
        queue_id="queue-1",
        max_attempts=1,
        retry_delay_seconds=5,
        failure_limit=2,
        poll_interval=0.0,
    )

    params = submit_route.calls.last.request.url.params
    assert params["max_attempts"] == "1"
    assert params["retry_delay_seconds"] == "5"
    assert params["failure_limit"] == "2"


@respx.mock
def test_submit_and_wait_times_out(make_client: ClientFactory, fake_clock: None) -> None:
    respx.post(_JOBS_URL).mock(return_value=httpx.Response(201, json=_job("pending")))
    respx.get(f"{_JOBS_URL}/j1").mock(return_value=httpx.Response(200, json=_job("running")))
    client = make_client()

    with pytest.raises(SqiTimeoutError):
        client.submit_and_wait(
            {"name": "My Job"}, farm_id="farm-1", queue_id="queue-1", poll_interval=1.0, timeout=3.0
        )
