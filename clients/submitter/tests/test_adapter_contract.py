# SPDX-License-Identifier: AGPL-3.0-or-later
"""Adapter contract: a minimal adapter drives the whole flow via public API."""

import json

import httpx
import pytest
import respx

from sqi_submitter.core import (
    FormInvalidError,
    FormModel,
    HostAdapter,
    RenderTarget,
    SceneContext,
    SubmitterError,
    SubmitterSession,
    prefill,
    submit_form,
)

BASE = "http://test-server:8080"

pytestmark = pytest.mark.usefixtures("_clean_settings")


class MiniAdapter(HostAdapter):
    host_name = "mini"
    display_name = "MiniTool"

    def __init__(self, modified: bool = False, can_save: bool = True) -> None:
        self.modified = modified
        self.can_save = can_save
        self.saved = False

    def scene_context(self) -> SceneContext:
        return SceneContext(scene_path="/x/s.mini", frame_range="1-4")

    def render_targets(self) -> list[RenderTarget]:
        return [RenderTarget(name="beauty", kind="pass", extra={"Pass": "beauty"})]

    def is_scene_modified(self) -> bool:
        return self.modified

    def save_scene(self) -> bool:
        self.saved = True
        return self.can_save


PARAMS = [
    {"name": "SceneFile", "type": "PATH"},
    {"name": "Frames", "type": "STRING"},
    {"name": "Pass", "type": "STRING", "default": "all"},
]


def _mock_server() -> respx.Route:
    respx.get(f"{BASE}/api/v1/products").mock(
        return_value=httpx.Response(200, json=[{"name": "mini-render"}])
    )
    respx.get(f"{BASE}/api/v1/products/mini-render/parameters").mock(
        return_value=httpx.Response(200, json=PARAMS)
    )
    return respx.post(f"{BASE}/api/v1/products/mini-render/jobs").mock(
        return_value=httpx.Response(201, json={"id": "j9", "name": "n"})
    )


@respx.mock
def test_minimal_adapter_full_flow() -> None:
    route = _mock_server()
    adapter = MiniAdapter(modified=True)
    session = SubmitterSession(server_url=BASE)

    product = session.products()[0]
    model = FormModel.from_parameters(session.parameters(product.name))
    model.apply_prefill(
        prefill(
            [f.parameter for f in model.fields],
            adapter.scene_context(),
            adapter.render_targets()[0],
        )
    )
    job = submit_form(
        session,
        product.name,
        model,
        farm_id="f1",
        queue_id="q1",
        job_name="mini shot",
        adapter=adapter,
    )
    assert job.id == "j9"
    assert adapter.saved
    body = json.loads(route.calls.last.request.content)
    assert body["parameters"] == {"SceneFile": "/x/s.mini", "Frames": "1-4", "Pass": "beauty"}


@respx.mock
def test_invalid_form_raises_with_field_errors_and_never_posts() -> None:
    route = _mock_server()
    session = SubmitterSession(server_url=BASE)
    model = FormModel.from_parameters(session.parameters("mini-render"))
    with pytest.raises(FormInvalidError) as exc:
        submit_form(session, "mini-render", model, farm_id="f", queue_id="q")
    assert "SceneFile" in exc.value.errors
    assert not route.called


@respx.mock
def test_unsavable_scene_blocks_submit() -> None:
    _mock_server()
    session = SubmitterSession(server_url=BASE)
    model = FormModel.from_parameters(session.parameters("mini-render"))
    model.set_value("SceneFile", "/x/s.mini")
    model.set_value("Frames", "1")
    adapter = MiniAdapter(modified=True, can_save=False)
    with pytest.raises(SubmitterError, match="save"):
        submit_form(session, "mini-render", model, farm_id="f", queue_id="q", adapter=adapter)
