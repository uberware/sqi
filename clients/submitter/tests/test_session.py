# SPDX-License-Identifier: AGPL-3.0-or-later
"""Session, settings, error-translation, and suggested-grouping tests."""

from pathlib import Path

import httpx
import pytest
import respx

from sqi_client import SqiClient
from sqi_client.models import Product
from sqi_submitter.core.errors import SubmitterError
from sqi_submitter.core.joboptions import JobOptions
from sqi_submitter.core.session import (
    Settings,
    SubmitterSession,
    group_products,
    resolve_server_url,
)

BASE = "http://test-server:8080"

pytestmark = pytest.mark.usefixtures("_clean_settings")


def _session() -> SubmitterSession:
    return SubmitterSession(server_url=BASE)


@respx.mock
def test_products_fetch_and_parameters() -> None:
    respx.get(f"{BASE}/api/v1/products").mock(
        return_value=httpx.Response(200, json=[{"name": "p1", "title": "P One"}])
    )
    respx.get(f"{BASE}/api/v1/products/p1/parameters").mock(
        return_value=httpx.Response(200, json=[{"name": "Frames", "type": "STRING"}])
    )
    s = _session()
    assert [p.name for p in s.products()] == ["p1"]
    assert s.parameters("p1")[0].name == "Frames"


@respx.mock
def test_submit_posts_parameters_and_returns_job() -> None:
    route = respx.post(f"{BASE}/api/v1/products/p1/jobs").mock(
        return_value=httpx.Response(201, json={"id": "j1", "name": "My Job"})
    )
    job = _session().submit(
        "p1", parameters={"Frames": "1-10"}, farm_id="f1", queue_id="q1", job_name="My Job"
    )
    assert job.id == "j1"
    import json

    body = json.loads(route.calls.last.request.content)
    assert body == {
        "farm_id": "f1",
        "queue_id": "q1",
        "name": "My Job",
        "parameters": {"Frames": "1-10"},
    }


@respx.mock
def test_submit_forwards_job_options_to_body() -> None:
    route = respx.post(f"{BASE}/api/v1/products/p1/jobs").mock(
        return_value=httpx.Response(201, json={"id": "j1", "name": "My Job"})
    )
    _session().submit(
        "p1",
        parameters={"Frames": "1-10"},
        farm_id="f1",
        queue_id="q1",
        job_options=JobOptions(priority=90, failure_limit=3),
    )
    import json

    body = json.loads(route.calls.last.request.content)
    assert body["priority"] == 90
    assert body["failure_limit"] == 3
    assert "owner" not in body  # unset -> omitted
    assert "max_attempts" not in body


@respx.mock
def test_connection_error_translates_to_user_message() -> None:
    respx.get(f"{BASE}/api/v1/products").mock(side_effect=httpx.ConnectError("boom"))
    with pytest.raises(SubmitterError) as exc:
        _session().products()
    assert BASE in exc.value.user_message
    assert "reach" in exc.value.user_message.lower()


@respx.mock
def test_server_422_message_is_surfaced_verbatim() -> None:
    respx.post(f"{BASE}/api/v1/products/p1/jobs").mock(
        return_value=httpx.Response(422, json={"error": "missing required parameter Frames"})
    )
    with pytest.raises(SubmitterError) as exc:
        _session().submit("p1", parameters={}, farm_id="f", queue_id="q")
    assert "missing required parameter Frames" in exc.value.user_message


@respx.mock
def test_missing_product_translates_to_no_longer_exists() -> None:
    respx.get(f"{BASE}/api/v1/products/gone/parameters").mock(
        return_value=httpx.Response(404, json={"error": "product not found"})
    )
    with pytest.raises(SubmitterError) as exc:
        _session().parameters("gone")
    assert "no longer exists" in exc.value.user_message


@respx.mock
def test_server_error_translates_to_submitter_error() -> None:
    respx.get(f"{BASE}/api/v1/products").mock(
        return_value=httpx.Response(500, json={"error": "internal server error"})
    )
    # max_attempts=1 disables the SDK's GET retry/backoff so the test is fast.
    session = SubmitterSession(server_url=BASE, client=SqiClient(BASE, max_attempts=1))
    with pytest.raises(SubmitterError) as exc:
        session.products()
    assert exc.value.user_message
    assert "500" in exc.value.user_message


@respx.mock
def test_farms_and_queues_fetch() -> None:
    respx.get(f"{BASE}/api/v1/farms").mock(
        return_value=httpx.Response(200, json=[{"id": "f1", "name": "Farm One"}])
    )
    respx.get(f"{BASE}/api/v1/queues", params={"farm_id": "f1"}).mock(
        return_value=httpx.Response(
            200,
            json={
                "items": [{"id": "q1", "farm_id": "f1", "name": "Queue One"}],
                "total": 1,
                "limit": 50,
                "offset": 0,
            },
        )
    )
    s = _session()
    assert [f.id for f in s.farms()] == ["f1"]
    assert [q.id for q in s.queues("f1")] == ["q1"]


def test_resolve_server_url_precedence(monkeypatch: pytest.MonkeyPatch, tmp_path: object) -> None:
    settings = Settings(path=str(Path(str(tmp_path)) / "s.json"))
    assert resolve_server_url(None, settings) == "http://localhost:8080"
    settings.set("server_url", "http://from-settings:1")
    assert resolve_server_url(None, settings) == "http://from-settings:1"
    monkeypatch.setenv("SQI_SERVER_URL", "http://from-env:2")
    assert resolve_server_url(None, settings) == "http://from-env:2"
    assert resolve_server_url("http://explicit:3", settings) == "http://explicit:3"


def test_settings_round_trip(tmp_path: object) -> None:
    path = str(Path(str(tmp_path)) / "s.json")
    Settings(path=path).set("last_product.maya", "maya-layer-render")
    assert Settings(path=path).get("last_product.maya") == "maya-layer-render"
    assert Settings(path=path).get("missing", "d") == "d"


def test_group_products_floats_host_matches_without_hiding_any() -> None:
    ps = [
        Product(name="acme-maya-render", description=""),
        Product(name="python", description=""),
        Product(name="char-anim", description="Maya character pipeline"),
    ]
    suggested, rest = group_products(ps, "maya")
    assert [p.name for p in suggested] == ["acme-maya-render", "char-anim"]
    assert [p.name for p in rest] == ["python"]
    s2, r2 = group_products(ps, "")
    assert s2 == [] and len(r2) == 3
