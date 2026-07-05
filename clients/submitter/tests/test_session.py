# SPDX-License-Identifier: AGPL-3.0-or-later
"""Session, settings, error-translation, and suggested-grouping tests."""

from pathlib import Path

import httpx
import pytest
import respx

from sqi_client.models import Product
from sqi_submitter.core.errors import SubmitterError
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


def test_resolve_server_url_precedence(
    monkeypatch: pytest.MonkeyPatch, tmp_path: object
) -> None:
    settings = Settings(path=str(Path(str(tmp_path)) / "s.json"))
    assert resolve_server_url(None, settings) == "http://localhost:8080"
    settings.set("server_url", "http://from-settings:1")
    assert resolve_server_url(None, settings) == "http://from-settings:1"
    monkeypatch.setenv("SQI_SERVER_URL", "http://from-env:2")
    assert resolve_server_url(None, settings) == "http://from-env:2"
    assert resolve_server_url("http://explicit:3", settings) == "http://explicit:3"


def test_settings_round_trip(tmp_path: object) -> None:
    path = str(Path(str(tmp_path)) / "s.json")
    Settings(path=path).set("last_product.maya", "maya-batch-render")
    assert Settings(path=path).get("last_product.maya") == "maya-batch-render"
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
