# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Tests for task operations and log polling.

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
from sqi_client.models import CancelResult, LogChunk, LogPage, Page, RetryResult, Task, TaskStatus
from tests.conftest import BASE_URL, ClientFactory

_API = f"{BASE_URL}/api/v1"
_FIXTURES = Path(__file__).parent / "fixtures"


def _load(name: str) -> dict[str, Any]:
    data: dict[str, Any] = json.loads((_FIXTURES / f"{name}.json").read_text())
    return data


def _task(task_id: str, **overrides: Any) -> dict[str, Any]:
    data = _load("task")
    data["id"] = task_id
    data.update(overrides)
    return data


def _task_page(
    items: list[dict[str, Any]], *, total: int, limit: int, offset: int
) -> httpx.Response:
    return httpx.Response(
        200,
        json={"items": items, "total": total, "limit": limit, "offset": offset},
    )


def _chunk(nats_seq: int, **overrides: Any) -> dict[str, Any]:
    data = _load("log_chunk")
    data["nats_seq"] = nats_seq
    data["seq_num"] = nats_seq
    data.update(overrides)
    return data


def _logs(items: list[dict[str, Any]], *, after_nats_seq: int, limit: int = 100) -> httpx.Response:
    return httpx.Response(
        200, json={"items": items, "after_nats_seq": after_nats_seq, "limit": limit}
    )


# ── list_job_tasks / iter_job_tasks ─────────────────────────────────


@respx.mock
def test_list_job_tasks_returns_page(make_client: ClientFactory) -> None:
    route = respx.get(f"{_API}/jobs/j1/tasks").mock(
        return_value=_task_page([_task("a"), _task("b")], total=2, limit=50, offset=0)
    )
    client = make_client()

    page = client.list_job_tasks("j1")

    assert isinstance(page, Page)
    assert [t.id for t in page.items] == ["a", "b"]
    assert all(isinstance(t, Task) for t in page.items)
    assert route.called


@respx.mock
def test_list_job_tasks_encodes_filters(make_client: ClientFactory) -> None:
    route = respx.get(f"{_API}/jobs/j1/tasks").mock(
        return_value=_task_page([], total=0, limit=10, offset=5)
    )
    client = make_client()

    client.list_job_tasks(
        "j1",
        status=TaskStatus.FAILED,
        sort_by="updated_at",
        sort_dir="desc",
        limit=10,
        offset=5,
    )

    params = route.calls.last.request.url.params
    assert params["status"] == "failed"
    assert params["sort_by"] == "updated_at"
    assert params["sort_dir"] == "desc"
    assert params["limit"] == "10"
    assert params["offset"] == "5"


@respx.mock
def test_list_job_tasks_omits_none_filters(make_client: ClientFactory) -> None:
    route = respx.get(f"{_API}/jobs/j1/tasks").mock(
        return_value=_task_page([], total=0, limit=50, offset=0)
    )
    client = make_client()

    client.list_job_tasks("j1")

    params = route.calls.last.request.url.params
    for absent in ("status", "sort_by", "sort_dir"):
        assert absent not in params


@respx.mock
def test_iter_job_tasks_walks_all_pages(make_client: ClientFactory) -> None:
    ids = ["a", "b", "c", "d", "e"]

    def handler(request: httpx.Request) -> httpx.Response:
        offset = int(request.url.params.get("offset", "0"))
        limit = int(request.url.params.get("limit", "2"))
        window = ids[offset : offset + limit]
        return _task_page([_task(i) for i in window], total=len(ids), limit=limit, offset=offset)

    respx.get(f"{_API}/jobs/j1/tasks").mock(side_effect=handler)
    client = make_client()

    collected = [t.id for t in client.iter_job_tasks("j1", limit=2)]

    assert collected == ids


# ── get_task ────────────────────────────────────────────────────────


@respx.mock
def test_get_task_returns_task(make_client: ClientFactory) -> None:
    data = _load("task")
    respx.get(f"{_API}/tasks/{data['id']}").mock(return_value=httpx.Response(200, json=data))
    client = make_client()

    task = client.get_task(data["id"])

    assert isinstance(task, Task)
    assert task.id == data["id"]
    assert task.status is TaskStatus.RUNNING


@respx.mock
def test_get_task_missing_raises_not_found(make_client: ClientFactory) -> None:
    respx.get(f"{_API}/tasks/nope").mock(
        return_value=httpx.Response(
            404,
            json={
                "type": "about:blank",
                "title": "Not Found",
                "status": 404,
                "detail": "task not found",
            },
            headers={"Content-Type": "application/problem+json"},
        )
    )
    client = make_client()

    with pytest.raises(NotFoundError):
        client.get_task("nope")


# ── retry_task ──────────────────────────────────────────────────────


@respx.mock
def test_retry_task_returns_result(make_client: ClientFactory) -> None:
    route = respx.post(f"{_API}/tasks/t1/retry").mock(
        return_value=httpx.Response(202, json={"task_id": "t1", "status": "ready"})
    )
    client = make_client()

    result = client.retry_task("t1")

    assert isinstance(result, RetryResult)
    assert result.task_id == "t1"
    assert result.status is TaskStatus.READY
    assert route.calls.last.request.method == "POST"


@respx.mock
def test_retry_task_invalid_state_raises_conflict(make_client: ClientFactory) -> None:
    respx.post(f"{_API}/tasks/t1/retry").mock(
        return_value=httpx.Response(
            409,
            json={
                "type": "about:blank",
                "title": "Conflict",
                "status": 409,
                "detail": "only failed or canceled tasks may be retried",
            },
            headers={"Content-Type": "application/problem+json"},
        )
    )
    client = make_client()

    with pytest.raises(ConflictError):
        client.retry_task("t1")


# ── cancel_task ─────────────────────────────────────────────────────


@respx.mock
def test_cancel_task_returns_result(make_client: ClientFactory) -> None:
    route = respx.post(f"{_API}/tasks/t1/cancel").mock(
        return_value=httpx.Response(202, json={"task_id": "t1", "status": "canceled"})
    )
    client = make_client()

    result = client.cancel_task("t1")

    assert isinstance(result, CancelResult)
    assert result.task_id == "t1"
    assert result.status is TaskStatus.CANCELED
    assert route.calls.last.request.method == "POST"


@respx.mock
def test_cancel_task_terminal_raises_conflict(make_client: ClientFactory) -> None:
    respx.post(f"{_API}/tasks/t1/cancel").mock(
        return_value=httpx.Response(
            409,
            json={
                "type": "about:blank",
                "title": "Conflict",
                "status": 409,
                "detail": "only non-terminal tasks may be canceled",
            },
            headers={"Content-Type": "application/problem+json"},
        )
    )
    client = make_client()

    with pytest.raises(ConflictError):
        client.cancel_task("t1")


@respx.mock
def test_cancel_task_missing_raises_not_found(make_client: ClientFactory) -> None:
    respx.post(f"{_API}/tasks/ghost/cancel").mock(
        return_value=httpx.Response(
            404,
            json={"type": "about:blank", "title": "Not Found", "status": 404},
            headers={"Content-Type": "application/problem+json"},
        )
    )
    client = make_client()

    with pytest.raises(NotFoundError):
        client.cancel_task("ghost")


# ── get_task_logs ───────────────────────────────────────────────────


@respx.mock
def test_get_task_logs_returns_log_page(make_client: ClientFactory) -> None:
    respx.get(f"{_API}/tasks/t1/logs").mock(
        return_value=_logs([_chunk(1), _chunk(2)], after_nats_seq=2)
    )
    client = make_client()

    page = client.get_task_logs("t1")

    assert isinstance(page, LogPage)
    assert page.after_nats_seq == 2
    assert [c.nats_seq for c in page.items] == [1, 2]
    assert all(isinstance(c, LogChunk) for c in page.items)


@respx.mock
def test_get_task_logs_encodes_cursor_and_limit(make_client: ClientFactory) -> None:
    route = respx.get(f"{_API}/tasks/t1/logs").mock(return_value=_logs([], after_nats_seq=7))
    client = make_client()

    client.get_task_logs("t1", limit=50, after_nats_seq=7)

    params = route.calls.last.request.url.params
    assert params["limit"] == "50"
    assert params["after_nats_seq"] == "7"
    # Phase 1 uses only the non-streaming JSON path: never request tail=true.
    assert "tail" not in params


# ── tail_task_logs polling ──────────────────────────────────────


@respx.mock
def test_tail_follow_false_stops_at_end_of_log(make_client: ClientFactory) -> None:
    # Two chunks, then an empty page → follow=False returns at end-of-log.
    pages = iter(
        [
            _logs([_chunk(1), _chunk(2)], after_nats_seq=2),
            _logs([], after_nats_seq=2),
        ]
    )
    respx.get(f"{_API}/tasks/t1/logs").mock(side_effect=lambda request: next(pages))
    client = make_client()

    chunks = list(client.tail_task_logs("t1", follow=False))

    assert [c.nats_seq for c in chunks] == [1, 2]


@respx.mock
def test_tail_advances_cursor_across_pages(make_client: ClientFactory) -> None:
    seen_cursors: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        cursor = request.url.params.get("after_nats_seq", "0")
        seen_cursors.append(cursor)
        if cursor == "0":
            return _logs([_chunk(1), _chunk(2)], after_nats_seq=2)
        if cursor == "2":
            return _logs([_chunk(5)], after_nats_seq=5)
        return _logs([], after_nats_seq=int(cursor))

    respx.get(f"{_API}/tasks/t1/logs").mock(side_effect=handler)
    client = make_client()

    chunks = list(client.tail_task_logs("t1", follow=False))

    assert [c.nats_seq for c in chunks] == [1, 2, 5]
    # The cursor advanced by the chunks' nats_seq, not the echoed value.
    assert seen_cursors[:3] == ["0", "2", "5"]


def _drainable_logs(pages: list[httpx.Response]) -> Any:
    """Return a respx side_effect that serves ``pages`` then empty pages.

    Once the scripted pages are exhausted it keeps returning an empty page at
    the caller's cursor, so a tail loop terminates cleanly instead of raising
    StopIteration on an extra poll.
    """
    state = {"i": 0}

    def handler(request: httpx.Request) -> httpx.Response:
        i = state["i"]
        state["i"] += 1
        if i < len(pages):
            return pages[i]
        cursor = int(request.url.params.get("after_nats_seq", "0"))
        return _logs([], after_nats_seq=cursor)

    return handler


@respx.mock
def test_tail_follow_true_stops_when_task_terminal(make_client: ClientFactory) -> None:
    # A chunk arrives while running; an empty poll follows; another chunk arrives
    # right before the task turns terminal. follow=True must drain that last
    # chunk and then stop rather than spinning on the finished task.
    respx.get(f"{_API}/tasks/t1/logs").mock(
        side_effect=_drainable_logs(
            [
                _logs([_chunk(1)], after_nats_seq=1),
                _logs([], after_nats_seq=1),
                _logs([_chunk(2)], after_nats_seq=2),
            ]
        )
    )
    status_state = {"i": 0}

    def task_handler(request: httpx.Request) -> httpx.Response:
        i = status_state["i"]
        status_state["i"] += 1
        # Running on the first status check, terminal thereafter.
        return httpx.Response(200, json=_task("t1", status="running" if i == 0 else "succeeded"))

    respx.get(f"{_API}/tasks/t1").mock(side_effect=task_handler)
    client = make_client()

    chunks = list(client.tail_task_logs("t1", follow=True, poll_interval=0))

    assert [c.nats_seq for c in chunks] == [1, 2]


@respx.mock
def test_tail_sleeps_poll_interval_between_empty_polls(
    make_client: ClientFactory, monkeypatch: pytest.MonkeyPatch
) -> None:
    # While the task is still running and no new chunks have arrived, the tail
    # waits poll_interval between polls rather than busy-looping.
    respx.get(f"{_API}/tasks/t1/logs").mock(side_effect=_drainable_logs([]))
    status_state = {"i": 0}

    def task_handler(request: httpx.Request) -> httpx.Response:
        i = status_state["i"]
        status_state["i"] += 1
        return httpx.Response(200, json=_task("t1", status="running" if i == 0 else "succeeded"))

    respx.get(f"{_API}/tasks/t1").mock(side_effect=task_handler)
    sleeps: list[float] = []
    monkeypatch.setattr("sqi_client.client.time.sleep", lambda seconds: sleeps.append(seconds))
    client = make_client()

    list(client.tail_task_logs("t1", poll_interval=0.25, follow=True))

    assert sleeps == [0.25]


@respx.mock
def test_tail_follow_true_drains_already_terminal_task(make_client: ClientFactory) -> None:
    # An already-finished task: drain its logs once and stop, never blocking.
    respx.get(f"{_API}/tasks/t1/logs").mock(
        side_effect=_drainable_logs([_logs([_chunk(1), _chunk(2)], after_nats_seq=2)])
    )
    respx.get(f"{_API}/tasks/t1").mock(
        return_value=httpx.Response(200, json=_task("t1", status="failed"))
    )
    client = make_client()

    chunks = list(client.tail_task_logs("t1", follow=True, poll_interval=0))

    assert [c.nats_seq for c in chunks] == [1, 2]
