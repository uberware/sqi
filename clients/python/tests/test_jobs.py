# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Tests for job query and management.

HTTP is mocked at the transport layer with respx; no server runs.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import httpx
import pytest
import respx

from sqi_client.errors import ConflictError, NotFoundError
from sqi_client.models import Job, JobStatus, Page
from tests.conftest import BASE_URL, ClientFactory

_JOBS_URL = f"{BASE_URL}/api/v1/jobs"
_FIXTURES = Path(__file__).parent / "fixtures"


def _load(name: str) -> dict[str, Any]:
    data: dict[str, Any] = json.loads((_FIXTURES / f"{name}.json").read_text())
    return data


def _job(job_id: str, **overrides: Any) -> dict[str, Any]:
    data = _load("job")
    data["id"] = job_id
    data.update(overrides)
    return data


def _page(items: list[dict[str, Any]], *, total: int, limit: int, offset: int) -> httpx.Response:
    return httpx.Response(
        200,
        json={"items": items, "total": total, "limit": limit, "offset": offset},
    )


# ── list_jobs ───────────────────────────────────────────────────────


@respx.mock
def test_list_jobs_returns_page(make_client: ClientFactory) -> None:
    route = respx.get(_JOBS_URL).mock(
        return_value=_page([_job("a"), _job("b")], total=2, limit=50, offset=0)
    )
    client = make_client()

    page = client.list_jobs()

    assert isinstance(page, Page)
    assert page.total == 2
    assert [j.id for j in page.items] == ["a", "b"]
    assert all(isinstance(j, Job) for j in page.items)
    assert route.called


@respx.mock
def test_list_jobs_encodes_all_filters(make_client: ClientFactory) -> None:
    route = respx.get(_JOBS_URL).mock(return_value=_page([], total=0, limit=10, offset=5))
    client = make_client()

    client.list_jobs(
        status="running",
        farm_id="farm-1",
        queue_id="queue-1",
        owner="alice",
        project="my-film",
        sort_by="priority",
        sort_dir="desc",
        limit=10,
        offset=5,
    )

    params = route.calls.last.request.url.params
    assert params["status"] == "running"
    assert params["farm_id"] == "farm-1"
    assert params["queue_id"] == "queue-1"
    assert params["owner"] == "alice"
    assert params["project"] == "my-film"
    assert params["sort_by"] == "priority"
    assert params["sort_dir"] == "desc"
    assert params["limit"] == "10"
    assert params["offset"] == "5"


@respx.mock
def test_list_jobs_accepts_enum_status(make_client: ClientFactory) -> None:
    route = respx.get(_JOBS_URL).mock(return_value=_page([], total=0, limit=50, offset=0))
    client = make_client()

    client.list_jobs(status=JobStatus.RUNNING)

    # The enum is serialized to its wire value, not its repr.
    assert route.calls.last.request.url.params["status"] == "running"


@respx.mock
def test_list_jobs_omits_none_filters(make_client: ClientFactory) -> None:
    route = respx.get(_JOBS_URL).mock(return_value=_page([], total=0, limit=50, offset=0))
    client = make_client()

    client.list_jobs()

    params = route.calls.last.request.url.params
    for absent in ("status", "farm_id", "queue_id", "owner", "project", "sort_by", "sort_dir"):
        assert absent not in params


# ── iter_jobs ───────────────────────────────────────────────────────


@respx.mock
def test_iter_jobs_walks_all_pages(make_client: ClientFactory) -> None:
    ids = ["a", "b", "c", "d", "e"]

    def handler(request: httpx.Request) -> httpx.Response:
        offset = int(request.url.params.get("offset", "0"))
        limit = int(request.url.params.get("limit", "2"))
        window = ids[offset : offset + limit]
        items = [_job(i) for i in window]
        return _page(items, total=len(ids), limit=limit, offset=offset)

    respx.get(_JOBS_URL).mock(side_effect=handler)
    client = make_client()

    collected = [job.id for job in client.iter_jobs(limit=2)]

    assert collected == ids


@respx.mock
def test_iter_jobs_passes_filters_on_every_page(make_client: ClientFactory) -> None:
    ids = ["a", "b", "c"]
    seen_params: list[dict[str, str]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen_params.append(dict(request.url.params))
        offset = int(request.url.params.get("offset", "0"))
        limit = int(request.url.params.get("limit", "2"))
        items = [_job(i) for i in ids[offset : offset + limit]]
        return _page(items, total=len(ids), limit=limit, offset=offset)

    respx.get(_JOBS_URL).mock(side_effect=handler)
    client = make_client()

    list(client.iter_jobs(status=JobStatus.FAILED, owner="bob", limit=2))

    # Filters are re-sent on each page request, not just the first.
    assert len(seen_params) >= 2
    for params in seen_params:
        assert params["status"] == "failed"
        assert params["owner"] == "bob"


# ── get_job ─────────────────────────────────────────────────────────


@respx.mock
def test_get_job_returns_detail(make_client: ClientFactory) -> None:
    detail = _load("job_detail")
    respx.get(f"{_JOBS_URL}/{detail['id']}").mock(return_value=httpx.Response(200, json=detail))
    client = make_client()

    job = client.get_job(detail["id"])

    assert isinstance(job, Job)
    assert job.id == detail["id"]
    assert job.steps is not None
    assert len(job.steps) == 2
    assert job.task_counts is not None
    assert job.task_counts.total == 100


@respx.mock
def test_get_job_missing_raises_not_found(make_client: ClientFactory) -> None:
    respx.get(f"{_JOBS_URL}/nope").mock(
        return_value=httpx.Response(
            404,
            json={
                "type": "about:blank",
                "title": "Not Found",
                "status": 404,
                "detail": "job not found",
            },
            headers={"Content-Type": "application/problem+json"},
        )
    )
    client = make_client()

    with pytest.raises(NotFoundError):
        client.get_job("nope")


# ── pause / resume ──────────────────────────────────────────────────


@respx.mock
def test_pause_job_sends_action(make_client: ClientFactory) -> None:
    route = respx.patch(f"{_JOBS_URL}/j1").mock(
        return_value=httpx.Response(200, json=_job("j1", status="paused"))
    )
    client = make_client()

    job = client.pause_job("j1")

    assert json.loads(route.calls.last.request.content) == {"action": "pause"}
    assert job.status is JobStatus.PAUSED


@respx.mock
def test_resume_job_sends_action(make_client: ClientFactory) -> None:
    route = respx.patch(f"{_JOBS_URL}/j1").mock(
        return_value=httpx.Response(200, json=_job("j1", status="running"))
    )
    client = make_client()

    job = client.resume_job("j1")

    assert json.loads(route.calls.last.request.content) == {"action": "resume"}
    assert job.status is JobStatus.RUNNING


# ── set_job_priority ────────────────────────────────────────────────


@respx.mock
def test_set_job_priority_sends_body(make_client: ClientFactory) -> None:
    route = respx.patch(f"{_JOBS_URL}/j1").mock(
        return_value=httpx.Response(200, json=_job("j1", priority=90))
    )
    client = make_client()

    job = client.set_job_priority("j1", 90)

    assert json.loads(route.calls.last.request.content) == {"priority": 90}
    assert job.priority == 90


@respx.mock
def test_set_job_priority_rejects_below_minimum(make_client: ClientFactory) -> None:
    route = respx.patch(f"{_JOBS_URL}/j1").mock(return_value=httpx.Response(200, json=_job("j1")))
    client = make_client()

    for bad in (0, -1, -100):
        with pytest.raises(ValueError, match="priority"):
            client.set_job_priority("j1", bad)
    # Rejected entirely client-side: no request is ever sent.
    assert not route.called


# ── cancel_job ──────────────────────────────────────────────────────


@respx.mock
def test_cancel_job_returns_none_on_204(make_client: ClientFactory) -> None:
    route = respx.delete(f"{_JOBS_URL}/j1").mock(return_value=httpx.Response(204))
    client = make_client()

    # Returns None on a 204 and issues exactly one DELETE.
    client.cancel_job("j1")

    assert route.called
    assert route.calls.last.request.method == "DELETE"


@respx.mock
def test_cancel_job_missing_raises_not_found(make_client: ClientFactory) -> None:
    respx.delete(f"{_JOBS_URL}/nope").mock(
        return_value=httpx.Response(
            404,
            json={
                "type": "about:blank",
                "title": "Not Found",
                "status": 404,
                "detail": "job not found",
            },
            headers={"Content-Type": "application/problem+json"},
        )
    )
    client = make_client()

    with pytest.raises(NotFoundError):
        client.cancel_job("nope")


@respx.mock
def test_cancel_job_terminal_raises_conflict(make_client: ClientFactory) -> None:
    # A completed/failed job cannot be canceled — the server returns 409.
    respx.delete(f"{_JOBS_URL}/done").mock(
        return_value=httpx.Response(
            409,
            json={
                "type": "about:blank",
                "title": "Conflict",
                "status": 409,
                "detail": "job has already completed and cannot be canceled",
            },
            headers={"Content-Type": "application/problem+json"},
        )
    )
    client = make_client()

    with pytest.raises(ConflictError):
        client.cancel_job("done")
