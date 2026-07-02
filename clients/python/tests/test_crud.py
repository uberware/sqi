# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Tests for farm/queue/storage-location/usage-pool CRUD.

HTTP is mocked at the transport layer with respx; no server runs. The generic
CRUD helper is exercised in depth via the farm methods; a parametrized smoke
test then confirms all four resource families are wired to the right paths and
models.
"""

from __future__ import annotations

import json
from collections.abc import Callable
from pathlib import Path
from typing import Any, NamedTuple

import httpx
import pytest
import respx

from sqi_client.client import SqiClient
from sqi_client.errors import BadRequestError, ConflictError, NotFoundError, ValidationError
from sqi_client.models import ComputeLocation, Farm, Page, Queue, StorageLocation, UsagePool
from tests.conftest import BASE_URL, ClientFactory

_API = f"{BASE_URL}/api/v1"
_FIXTURES = Path(__file__).parent / "fixtures"


def _load(name: str) -> dict[str, Any]:
    data: dict[str, Any] = json.loads((_FIXTURES / f"{name}.json").read_text())
    return data


def _problem(status: int, title: str, detail: str) -> httpx.Response:
    return httpx.Response(
        status,
        json={"type": "about:blank", "title": title, "status": status, "detail": detail},
        headers={"Content-Type": "application/problem+json"},
    )


# ── Generic helper, in depth via farms ──────────────────────────────


@respx.mock
def test_create_farm_posts_body_and_returns_model(make_client: ClientFactory) -> None:
    route = respx.post(f"{_API}/farms").mock(return_value=httpx.Response(201, json=_load("farm")))
    client = make_client()

    farm = client.create_farm(name="studio-a", description="Studio A", max_concurrent_tasks=200)

    assert isinstance(farm, Farm)
    body = json.loads(route.calls.last.request.content)
    assert body == {"name": "studio-a", "description": "Studio A", "max_concurrent_tasks": 200}


@respx.mock
def test_create_farm_omits_unset_optionals(make_client: ClientFactory) -> None:
    route = respx.post(f"{_API}/farms").mock(return_value=httpx.Response(201, json=_load("farm")))
    client = make_client()

    client.create_farm(name="studio-a")

    body = json.loads(route.calls.last.request.content)
    assert body == {"name": "studio-a", "max_concurrent_tasks": 0}
    assert "description" not in body


@respx.mock
def test_get_farm_returns_model(make_client: ClientFactory) -> None:
    fixture = _load("farm")
    respx.get(f"{_API}/farms/{fixture['id']}").mock(return_value=httpx.Response(200, json=fixture))
    client = make_client()

    farm = client.get_farm(fixture["id"])

    assert isinstance(farm, Farm)
    assert farm.id == fixture["id"]


@respx.mock
def test_get_farm_404(make_client: ClientFactory) -> None:
    respx.get(f"{_API}/farms/nope").mock(return_value=_problem(404, "Not Found", "farm not found"))
    client = make_client()

    with pytest.raises(NotFoundError):
        client.get_farm("nope")


@respx.mock
def test_update_farm_puts_full_body(make_client: ClientFactory) -> None:
    route = respx.put(f"{_API}/farms/f1").mock(return_value=httpx.Response(200, json=_load("farm")))
    client = make_client()

    farm = client.update_farm("f1", name="renamed", max_concurrent_tasks=10)

    assert isinstance(farm, Farm)
    assert route.calls.last.request.method == "PUT"
    body = json.loads(route.calls.last.request.content)
    assert body == {"name": "renamed", "max_concurrent_tasks": 10}


@respx.mock
def test_update_farm_409_conflict(make_client: ClientFactory) -> None:
    respx.put(f"{_API}/farms/f1").mock(return_value=_problem(409, "Conflict", "name already used"))
    client = make_client()

    with pytest.raises(ConflictError):
        client.update_farm("f1", name="dup")


@respx.mock
def test_create_farm_400_bad_request(make_client: ClientFactory) -> None:
    respx.post(f"{_API}/farms").mock(return_value=_problem(400, "Bad Request", "name is required"))
    client = make_client()

    with pytest.raises(BadRequestError):
        client.create_farm(name="")


@respx.mock
def test_create_farm_422_validation(make_client: ClientFactory) -> None:
    # The transport maps 422 → ValidationError uniformly, so CRUD surfaces it too.
    respx.post(f"{_API}/farms").mock(
        return_value=_problem(422, "Unprocessable Entity", "bad field")
    )
    client = make_client()

    with pytest.raises(ValidationError):
        client.create_farm(name="x")


@respx.mock
def test_delete_farm_204_returns_none(make_client: ClientFactory) -> None:
    route = respx.delete(f"{_API}/farms/f1").mock(return_value=httpx.Response(204))
    client = make_client()

    client.delete_farm("f1")

    assert route.calls.last.request.method == "DELETE"


@respx.mock
def test_delete_farm_404(make_client: ClientFactory) -> None:
    respx.delete(f"{_API}/farms/nope").mock(
        return_value=_problem(404, "Not Found", "farm not found")
    )
    client = make_client()

    with pytest.raises(NotFoundError):
        client.delete_farm("nope")


# ── List shapes: bare array vs paginated ──────────────────────────────────────


@respx.mock
def test_list_farms_returns_plain_list(make_client: ClientFactory) -> None:
    # listFarms returns a bare JSON array (no pagination wrapper).
    respx.get(f"{_API}/farms").mock(return_value=httpx.Response(200, json=[_load("farm")]))
    client = make_client()

    farms = client.list_farms()

    assert isinstance(farms, list)
    assert len(farms) == 1
    assert isinstance(farms[0], Farm)


@respx.mock
def test_iter_farms_yields_each(make_client: ClientFactory) -> None:
    respx.get(f"{_API}/farms").mock(
        return_value=httpx.Response(200, json=[_load("farm"), _load("farm")])
    )
    client = make_client()

    farms = list(client.iter_farms())

    assert len(farms) == 2
    assert all(isinstance(f, Farm) for f in farms)


@respx.mock
def test_list_farms_empty_array(make_client: ClientFactory) -> None:
    respx.get(f"{_API}/farms").mock(return_value=httpx.Response(200, json=[]))
    client = make_client()

    assert client.list_farms() == []


@respx.mock
def test_list_queues_returns_page_with_filters(make_client: ClientFactory) -> None:
    route = respx.get(f"{_API}/queues").mock(
        return_value=httpx.Response(
            200,
            json={"items": [_load("queue")], "total": 1, "limit": 50, "offset": 0},
        )
    )
    client = make_client()

    page = client.list_queues(farm_id="farm-1", paused=False, sort_by="priority", limit=10)

    assert isinstance(page, Page)
    assert isinstance(page.items[0], Queue)
    params = route.calls.last.request.url.params
    assert params["farm_id"] == "farm-1"
    assert params["paused"] == "false"
    assert params["sort_by"] == "priority"
    assert params["limit"] == "10"


@respx.mock
def test_iter_queues_walks_pages(make_client: ClientFactory) -> None:
    rows = ["a", "b", "c"]

    def handler(request: httpx.Request) -> httpx.Response:
        offset = int(request.url.params.get("offset", "0"))
        limit = int(request.url.params.get("limit", "2"))
        window = rows[offset : offset + limit]
        items = []
        for qid in window:
            q = _load("queue")
            q["id"] = qid
            items.append(q)
        return httpx.Response(
            200, json={"items": items, "total": len(rows), "limit": limit, "offset": offset}
        )

    respx.get(f"{_API}/queues").mock(side_effect=handler)
    client = make_client()

    assert [q.id for q in client.iter_queues(limit=2)] == rows


@respx.mock
def test_create_queue_requires_farm_id_in_body(make_client: ClientFactory) -> None:
    route = respx.post(f"{_API}/queues").mock(return_value=httpx.Response(201, json=_load("queue")))
    client = make_client()

    client.create_queue(farm_id="farm-1", name="renders", priority=50, max_concurrent_tasks=200)

    body = json.loads(route.calls.last.request.content)
    assert body["farm_id"] == "farm-1"
    assert body["name"] == "renders"
    assert body["priority"] == 50
    assert body["max_concurrent_tasks"] == 200
    assert body["paused"] is False


# ── Parametrized smoke across all four families ─────────────────────


class _Family(NamedTuple):
    """One resource family's wiring, for the parametrized smoke test."""

    path: str
    fixture: str
    model: type
    paginated: bool
    create: Callable[[SqiClient], Any]
    list_: Callable[[SqiClient], Any]
    iter_: Callable[[SqiClient], Any]
    get: Callable[[SqiClient, str], Any]
    update: Callable[[SqiClient, str], Any]
    delete: Callable[[SqiClient, str], Any]


_FAMILIES: list[_Family] = [
    _Family(
        "farms",
        "farm",
        Farm,
        False,
        lambda c: c.create_farm(name="f"),
        lambda c: c.list_farms(),
        lambda c: c.iter_farms(),
        lambda c, i: c.get_farm(i),
        lambda c, i: c.update_farm(i, name="f2"),
        lambda c, i: c.delete_farm(i),
    ),
    _Family(
        "queues",
        "queue",
        Queue,
        True,
        lambda c: c.create_queue(farm_id="farm-1", name="q"),
        lambda c: c.list_queues(),
        lambda c: c.iter_queues(),
        lambda c, i: c.get_queue(i),
        lambda c, i: c.update_queue(i, farm_id="farm-1", name="q2"),
        lambda c, i: c.delete_queue(i),
    ),
    _Family(
        "storage-locations",
        "storage_location",
        StorageLocation,
        False,
        lambda c: c.create_storage_location(name="s"),
        lambda c: c.list_storage_locations(),
        lambda c: c.iter_storage_locations(),
        lambda c, i: c.get_storage_location(i),
        lambda c, i: c.update_storage_location(i, name="s2"),
        lambda c, i: c.delete_storage_location(i),
    ),
    _Family(
        "usage-pools",
        "usage_pool",
        UsagePool,
        False,
        lambda c: c.create_usage_pool(name="l", max_concurrent=4),
        lambda c: c.list_usage_pools(),
        lambda c: c.iter_usage_pools(),
        lambda c, i: c.get_usage_pool(i),
        lambda c, i: c.update_usage_pool(i, name="l2", max_concurrent=2),
        lambda c, i: c.delete_usage_pool(i),
    ),
    _Family(
        "compute-locations",
        "compute_location",
        ComputeLocation,
        False,
        lambda c: c.create_compute_location(name="cl"),
        lambda c: c.list_compute_locations(),
        lambda c: c.iter_compute_locations(),
        lambda c, i: c.get_compute_location(i),
        lambda c, i: c.update_compute_location(i, name="cl2"),
        lambda c, i: c.delete_compute_location(i),
    ),
]


@pytest.mark.parametrize("fam", _FAMILIES, ids=[f.path for f in _FAMILIES])
@respx.mock
def test_crud_family_smoke(make_client: ClientFactory, fam: _Family) -> None:
    body = _load(fam.fixture)
    list_body: Any = (
        {"items": [body], "total": 1, "limit": 50, "offset": 0} if fam.paginated else [body]
    )

    create_route = respx.post(f"{_API}/{fam.path}").mock(
        return_value=httpx.Response(201, json=body)
    )
    list_route = respx.get(f"{_API}/{fam.path}").mock(
        return_value=httpx.Response(200, json=list_body)
    )
    get_route = respx.get(f"{_API}/{fam.path}/x1").mock(return_value=httpx.Response(200, json=body))
    update_route = respx.put(f"{_API}/{fam.path}/x1").mock(
        return_value=httpx.Response(200, json=body)
    )
    delete_route = respx.delete(f"{_API}/{fam.path}/x1").mock(return_value=httpx.Response(204))

    client = make_client()

    created = fam.create(client)
    assert isinstance(created, fam.model)
    assert create_route.calls.last.request.method == "POST"

    listed = fam.list_(client)
    items = listed.items if isinstance(listed, Page) else listed
    assert isinstance(items[0], fam.model)

    iterated = list(fam.iter_(client))
    assert isinstance(iterated[0], fam.model)
    assert list_route.called

    got = fam.get(client, "x1")
    assert isinstance(got, fam.model)
    assert get_route.called

    updated = fam.update(client, "x1")
    assert isinstance(updated, fam.model)
    assert update_route.calls.last.request.method == "PUT"

    fam.delete(client, "x1")
    assert delete_route.calls.last.request.method == "DELETE"


@respx.mock
def test_optional_body_fields_included_when_supplied(make_client: ClientFactory) -> None:
    # The optional fields of each create body are sent only when provided.
    queue_route = respx.post(f"{_API}/queues").mock(
        return_value=httpx.Response(201, json=_load("queue"))
    )
    storage_route = respx.post(f"{_API}/storage-locations").mock(
        return_value=httpx.Response(201, json=_load("storage_location"))
    )
    usage_route = respx.post(f"{_API}/usage-pools").mock(
        return_value=httpx.Response(201, json=_load("usage_pool"))
    )
    client = make_client()

    client.create_queue(farm_id="farm-1", name="q", description="main queue")
    assert json.loads(queue_route.calls.last.request.content)["description"] == "main queue"

    client.create_storage_location(name="s", description="primary", roots={"on-prem": "/mnt/farm"})
    storage_body = json.loads(storage_route.calls.last.request.content)
    assert "type" not in storage_body
    assert storage_body["name"] == "s"
    assert storage_body["description"] == "primary"
    assert storage_body["roots"] == {"on-prem": "/mnt/farm"}

    client.create_usage_pool(name="l", max_concurrent=4, server_hint="27000@licsrv")
    assert json.loads(usage_route.calls.last.request.content)["server_hint"] == "27000@licsrv"


@respx.mock
def test_create_usage_pool_rejects_max_concurrent_below_minimum(
    make_client: ClientFactory,
) -> None:
    route = respx.post(f"{_API}/usage-pools").mock(
        return_value=httpx.Response(201, json=_load("usage_pool"))
    )
    client = make_client()

    for bad in (0, -1):
        with pytest.raises(ValueError, match="max_concurrent"):
            client.create_usage_pool(name="l", max_concurrent=bad)
        with pytest.raises(ValueError, match="max_concurrent"):
            client.update_usage_pool("p1", name="l", max_concurrent=bad)
    # Rejected entirely client-side: no request is ever sent.
    assert not route.called


@respx.mock
def test_get_usage_pool_parses_utilization_fields(make_client: ClientFactory) -> None:
    # The server reports live utilization on every usage-pool response; the
    # client surfaces it as the read-only in_use / available fields.
    respx.get(f"{_API}/usage-pools/p1").mock(
        return_value=httpx.Response(200, json=_load("usage_pool"))
    )
    client = make_client()

    pool = client.get_usage_pool("p1")

    assert isinstance(pool, UsagePool)
    assert pool.in_use == 3
    assert pool.available == 7


@respx.mock
def test_update_queue_clears_omitted_optionals(make_client: ClientFactory) -> None:
    # Full-replace PUT: omitting description sends no description key (clears it),
    # and unspecified priority/paused fall back to their defaults.
    route = respx.put(f"{_API}/queues/q1").mock(
        return_value=httpx.Response(200, json=_load("queue"))
    )
    client = make_client()

    client.update_queue("q1", farm_id="farm-1", name="renamed")

    body = json.loads(route.calls.last.request.content)
    assert "description" not in body
    assert body["priority"] == 0
    assert body["paused"] is False


@respx.mock
def test_resource_id_is_url_escaped(make_client: ClientFactory) -> None:
    # An ID containing path-significant characters is percent-encoded so it can't
    # corrupt the request path.
    route = respx.get(f"{_API}/farms/a%2Fb%20c").mock(
        return_value=httpx.Response(200, json=_load("farm"))
    )
    client = make_client()

    client.get_farm("a/b c")

    assert route.called


@respx.mock
def test_list_all_non_array_body_degrades_to_empty(make_client: ClientFactory) -> None:
    # A pathological non-array 200 body yields an empty list, not an exception.
    respx.get(f"{_API}/farms").mock(return_value=httpx.Response(200, json={"unexpected": True}))
    client = make_client()

    assert client.list_farms() == []
