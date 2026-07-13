# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Tests for the product CRUD, parameters, and product-submit SDK methods."""

from __future__ import annotations

import json

import httpx
import respx

from tests.conftest import BASE_URL, ClientFactory

API = f"{BASE_URL}/api/v1"


@respx.mock
def test_list_products(make_client: ClientFactory) -> None:
    respx.get(f"{API}/products").mock(
        return_value=httpx.Response(200, json=[{"name": "blender", "title": "Blender"}])
    )
    client = make_client()
    products = client.list_products()
    assert [p.name for p in products] == ["blender"]


@respx.mock
def test_get_product(make_client: ClientFactory) -> None:
    respx.get(f"{API}/products/blender").mock(
        return_value=httpx.Response(200, json={"name": "blender", "title": "Blender"})
    )
    client = make_client()
    assert client.get_product("blender").title == "Blender"


@respx.mock
def test_create_product(make_client: ClientFactory) -> None:
    route = respx.post(f"{API}/products").mock(
        return_value=httpx.Response(201, json={"name": "custom", "source": "custom"})
    )
    client = make_client()
    p = client.create_product(name="custom", title="Custom", template="tmpl", format="yaml")
    assert p.name == "custom"
    sent = json.loads(route.calls.last.request.content)
    assert sent["name"] == "custom"
    assert sent["template"] == "tmpl"


@respx.mock
def test_update_product(make_client: ClientFactory) -> None:
    respx.put(f"{API}/products/custom").mock(
        return_value=httpx.Response(200, json={"name": "custom", "title": "New"})
    )
    client = make_client()
    assert client.update_product("custom", title="New", template="t", format="yaml").title == "New"


@respx.mock
def test_delete_product(make_client: ClientFactory) -> None:
    route = respx.delete(f"{API}/products/custom").mock(return_value=httpx.Response(204))
    client = make_client()
    client.delete_product("custom")
    assert route.called


@respx.mock
def test_get_product_parameters(make_client: ClientFactory) -> None:
    respx.get(f"{API}/products/blender/parameters").mock(
        return_value=httpx.Response(
            200,
            json=[
                {
                    "name": "Quality",
                    "type": "STRING",
                    "default": "final",
                    "user_interface": {"control": "DROPDOWN_LIST", "label": "Quality"},
                }
            ],
        )
    )
    client = make_client()
    params = client.get_product_parameters("blender")
    assert params[0].name == "Quality"
    assert params[0].user_interface is not None
    assert params[0].user_interface.control == "DROPDOWN_LIST"


@respx.mock
def test_submit_product_job_includes_name(make_client: ClientFactory) -> None:
    route = respx.post(f"{API}/products/blender/jobs").mock(
        return_value=httpx.Response(201, json={"id": "job-1", "name": "Shot010"})
    )
    client = make_client()
    job = client.submit_product_job(
        "blender",
        job_name="Shot010",
        farm_id="f1",
        queue_id="q1",
        parameters={"Scene": "/proj/a.blend"},
    )
    assert job.id == "job-1"
    sent = json.loads(route.calls.last.request.content)
    assert sent["name"] == "Shot010"
    assert sent["farm_id"] == "f1"
    assert sent["parameters"] == {"Scene": "/proj/a.blend"}


@respx.mock
def test_submit_product_job_includes_retry_overrides(make_client: ClientFactory) -> None:
    route = respx.post(f"{API}/products/blender/jobs").mock(
        return_value=httpx.Response(201, json={"id": "job-1", "name": "x"})
    )
    client = make_client()
    client.submit_product_job(
        "blender",
        farm_id="f1",
        queue_id="q1",
        parameters={"Scene": "/a.blend"},
        max_attempts=5,
        retry_delay_seconds=30,
        failure_limit=7,
    )
    sent = json.loads(route.calls.last.request.content)
    assert sent["max_attempts"] == 5
    assert sent["retry_delay_seconds"] == 30
    assert sent["failure_limit"] == 7


@respx.mock
def test_submit_product_job_omits_unset_retry_overrides(make_client: ClientFactory) -> None:
    route = respx.post(f"{API}/products/blender/jobs").mock(
        return_value=httpx.Response(201, json={"id": "job-1", "name": "x"})
    )
    client = make_client()
    client.submit_product_job("blender", farm_id="f1", queue_id="q1")
    sent = json.loads(route.calls.last.request.content)
    assert "max_attempts" not in sent
    assert "retry_delay_seconds" not in sent
    assert "failure_limit" not in sent


@respx.mock
def test_submit_product_job_includes_depends_on(make_client: ClientFactory) -> None:
    route = respx.post(f"{API}/products/blender/jobs").mock(
        return_value=httpx.Response(201, json={"id": "job-1", "name": "x"})
    )
    client = make_client()
    client.submit_product_job(
        "blender",
        farm_id="f1",
        queue_id="q1",
        depends_on=["a", "b"],
    )
    sent = json.loads(route.calls.last.request.content)
    assert sent["depends_on"] == ["a", "b"]


@respx.mock
def test_submit_product_job_omits_unset_depends_on(make_client: ClientFactory) -> None:
    route = respx.post(f"{API}/products/blender/jobs").mock(
        return_value=httpx.Response(201, json={"id": "job-1", "name": "x"})
    )
    client = make_client()
    client.submit_product_job("blender", farm_id="f1", queue_id="q1")
    sent = json.loads(route.calls.last.request.content)
    assert "depends_on" not in sent
