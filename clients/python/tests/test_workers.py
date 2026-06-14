# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Tests for worker operations.

HTTP is mocked at the transport layer with respx; no server runs.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import httpx
import pytest
import respx

from sqi_client.errors import NotFoundError
from sqi_client.models import Page, Worker, WorkerAction, WorkerStatus
from tests.conftest import BASE_URL, ClientFactory

_WORKERS_URL = f"{BASE_URL}/api/v1/workers"
_FIXTURES = Path(__file__).parent / "fixtures"


def _load(name: str) -> dict[str, Any]:
    data: dict[str, Any] = json.loads((_FIXTURES / f"{name}.json").read_text())
    return data


def _worker(worker_id: str, **overrides: Any) -> dict[str, Any]:
    data = _load("worker")
    data["id"] = worker_id
    data.update(overrides)
    return data


def _page(items: list[dict[str, Any]], *, total: int, limit: int, offset: int) -> httpx.Response:
    return httpx.Response(
        200,
        json={"items": items, "total": total, "limit": limit, "offset": offset},
    )


# ── list_workers / iter_workers ─────────────────────────────────────


@respx.mock
def test_list_workers_returns_page(make_client: ClientFactory) -> None:
    route = respx.get(_WORKERS_URL).mock(
        return_value=_page([_worker("a"), _worker("b")], total=2, limit=50, offset=0)
    )
    client = make_client()

    page = client.list_workers()

    assert isinstance(page, Page)
    assert [w.id for w in page.items] == ["a", "b"]
    assert all(isinstance(w, Worker) for w in page.items)
    assert route.called


@respx.mock
def test_list_workers_encodes_all_filters(make_client: ClientFactory) -> None:
    route = respx.get(_WORKERS_URL).mock(return_value=_page([], total=0, limit=10, offset=5))
    client = make_client()

    client.list_workers(
        farm_id="farm-1",
        queue_id="queue-1",
        compute_location="on-prem",
        status=WorkerStatus.DISABLED,
        sort_by="hostname",
        sort_dir="desc",
        limit=10,
        offset=5,
    )

    params = route.calls.last.request.url.params
    assert params["farm_id"] == "farm-1"
    assert params["queue_id"] == "queue-1"
    assert params["compute_location"] == "on-prem"
    assert params["status"] == "disabled"
    assert params["sort_by"] == "hostname"
    assert params["sort_dir"] == "desc"
    assert params["limit"] == "10"
    assert params["offset"] == "5"


@respx.mock
def test_list_workers_omits_none_filters(make_client: ClientFactory) -> None:
    route = respx.get(_WORKERS_URL).mock(return_value=_page([], total=0, limit=50, offset=0))
    client = make_client()

    client.list_workers()

    params = route.calls.last.request.url.params
    for absent in ("farm_id", "queue_id", "compute_location", "status", "sort_by", "sort_dir"):
        assert absent not in params


@respx.mock
def test_iter_workers_walks_all_pages(make_client: ClientFactory) -> None:
    ids = ["a", "b", "c", "d", "e"]

    def handler(request: httpx.Request) -> httpx.Response:
        offset = int(request.url.params.get("offset", "0"))
        limit = int(request.url.params.get("limit", "2"))
        window = ids[offset : offset + limit]
        return _page([_worker(i) for i in window], total=len(ids), limit=limit, offset=offset)

    respx.get(_WORKERS_URL).mock(side_effect=handler)
    client = make_client()

    collected = [w.id for w in client.iter_workers(limit=2)]

    assert collected == ids


@respx.mock
def test_iter_workers_passes_filters_on_every_page(make_client: ClientFactory) -> None:
    ids = ["a", "b", "c"]
    seen_params: list[dict[str, str]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen_params.append(dict(request.url.params))
        offset = int(request.url.params.get("offset", "0"))
        limit = int(request.url.params.get("limit", "2"))
        window = ids[offset : offset + limit]
        return _page([_worker(i) for i in window], total=len(ids), limit=limit, offset=offset)

    respx.get(_WORKERS_URL).mock(side_effect=handler)
    client = make_client()

    list(client.iter_workers(farm_id="farm-1", status=WorkerStatus.ONLINE, limit=2))

    assert len(seen_params) >= 2
    for params in seen_params:
        assert params["farm_id"] == "farm-1"
        assert params["status"] == "online"


# ── get_worker ──────────────────────────────────────────────────────


@respx.mock
def test_get_worker_returns_detail(make_client: ClientFactory) -> None:
    detail = _load("worker_detail")
    respx.get(f"{_WORKERS_URL}/{detail['id']}").mock(return_value=httpx.Response(200, json=detail))
    client = make_client()

    worker = client.get_worker(detail["id"])

    assert isinstance(worker, Worker)
    assert worker.id == detail["id"]
    assert worker.status is WorkerStatus.ONLINE
    # Detail includes the currently-executing task.
    assert worker.current_task is not None
    assert worker.current_task.id == detail["current_task"]["id"]


@respx.mock
def test_get_worker_missing_raises_not_found(make_client: ClientFactory) -> None:
    respx.get(f"{_WORKERS_URL}/nope").mock(
        return_value=httpx.Response(
            404,
            json={
                "type": "about:blank",
                "title": "Not Found",
                "status": 404,
                "detail": "worker not found",
            },
            headers={"Content-Type": "application/problem+json"},
        )
    )
    client = make_client()

    with pytest.raises(NotFoundError):
        client.get_worker("nope")


# ── enable / disable ────────────────────────────────────────────────


@respx.mock
def test_disable_worker_posts_and_returns_action(make_client: ClientFactory) -> None:
    route = respx.post(f"{_WORKERS_URL}/w1/disable").mock(
        return_value=httpx.Response(200, json={"id": "w1", "status": "disabled"})
    )
    client = make_client()

    action = client.disable_worker("w1")

    assert route.calls.last.request.method == "POST"
    assert isinstance(action, WorkerAction)
    assert action.id == "w1"
    assert action.status is WorkerStatus.DISABLED


@respx.mock
def test_enable_worker_posts_and_returns_action(make_client: ClientFactory) -> None:
    route = respx.post(f"{_WORKERS_URL}/w1/enable").mock(
        return_value=httpx.Response(200, json={"id": "w1", "status": "online"})
    )
    client = make_client()

    action = client.enable_worker("w1")

    assert route.calls.last.request.url.path.endswith("/workers/w1/enable")
    assert isinstance(action, WorkerAction)
    assert action.status is WorkerStatus.ONLINE


@respx.mock
def test_enable_worker_none_on_bare_success(make_client: ClientFactory) -> None:
    # A bare 204 (no body) yields None rather than an empty model.
    respx.post(f"{_WORKERS_URL}/w1/enable").mock(return_value=httpx.Response(204))
    client = make_client()

    assert client.enable_worker("w1") is None


@respx.mock
def test_enable_worker_missing_raises_not_found(make_client: ClientFactory) -> None:
    respx.post(f"{_WORKERS_URL}/nope/enable").mock(
        return_value=httpx.Response(
            404,
            json={
                "type": "about:blank",
                "title": "Not Found",
                "status": 404,
                "detail": "worker not found",
            },
            headers={"Content-Type": "application/problem+json"},
        )
    )
    client = make_client()

    with pytest.raises(NotFoundError):
        client.enable_worker("nope")


@respx.mock
def test_disable_worker_missing_raises_not_found(make_client: ClientFactory) -> None:
    respx.post(f"{_WORKERS_URL}/nope/disable").mock(
        return_value=httpx.Response(
            404,
            json={
                "type": "about:blank",
                "title": "Not Found",
                "status": 404,
                "detail": "worker not found",
            },
            headers={"Content-Type": "application/problem+json"},
        )
    )
    client = make_client()

    with pytest.raises(NotFoundError):
        client.disable_worker("nope")
