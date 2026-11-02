# SPDX-License-Identifier: AGPL-3.0-or-later
"""The Katana adapter and menu glue, against the handoff tests' fake Katana (no Katana, no Qt)."""

from __future__ import annotations

import json
import sys
from types import SimpleNamespace
from typing import Any

import httpx
import pytest
import respx

from sqi_submitter.core import FormModel, HostAdapter, SubmitterSession, prefill, submit_form
from tests.conftest import FakeHost
from tests.test_katana_handoff import Katana, Node

BASE = "http://test-server:8080"
PATH = "D:/shots/s010.katana"


@pytest.fixture
def katana(fake_host_module: FakeHost, monkeypatch: pytest.MonkeyPatch) -> Katana:
    return Katana(fake_host_module, monkeypatch)


def _adapter() -> Any:
    from sqi_submitter.hosts.katana.adapter import KatanaAdapter

    return KatanaAdapter()


def test_katana_is_a_host_adapter() -> None:
    from sqi_submitter.hosts.katana.adapter import KatanaAdapter

    assert issubclass(KatanaAdapter, HostAdapter)
    assert KatanaAdapter.host_name == "katana"
    assert KatanaAdapter.display_name == "Katana"


def test_katana_scene_context(katana: Katana) -> None:
    ctx = _adapter().scene_context()
    assert ctx.scene_path == PATH
    assert ctx.frame_range == "1-100"  # the project's in and out times
    assert ctx.output_path is None
    assert ctx.renderer is None


def test_katana_untitled_project_has_no_scene_path(katana: Katana) -> None:
    katana.file_name = ""
    assert _adapter().scene_context().scene_path is None


def test_katana_render_nodes_are_targets(katana: Katana) -> None:
    """Every Render node that is not bypassed, with its farm range when that is enabled."""
    katana.nodes = [
        Node("Beauty", active=(10, 20)),
        Node("Utility Pass"),
        Node("Skipped", bypassed=True, active=(5, 6)),
        Node("Farm", active=(30, 40), group="farmSettings"),
    ]
    targets = _adapter().render_targets()
    assert [(t.name, t.kind, t.frame_range, dict(t.extra)) for t in targets] == [
        ("Beauty", "render_node", "10-20", {"RenderNode": "Beauty"}),
        ("Utility Pass", "render_node", "1-100", {"RenderNode": "Utility Pass"}),
        ("Farm", "render_node", "30-40", {"RenderNode": "Farm"}),
    ]
    assert all(t.output_path is None and t.locked == frozenset() for t in targets)


def test_katana_no_render_nodes_means_no_targets(katana: Katana) -> None:
    assert _adapter().render_targets() == []


@pytest.mark.parametrize("dirty", [True, False])
def test_katana_modified_follows_the_dirty_flag(katana: Katana, dirty: bool) -> None:
    katana.dirty = dirty
    assert _adapter().is_scene_modified() is dirty


def test_katana_save_scene_saves_in_place(katana: Katana) -> None:
    katana.dirty = True
    assert _adapter().save_scene() is True
    assert katana.events == ["save:" + PATH]


def test_katana_save_scene_reports_failure(katana: Katana) -> None:
    katana.dirty = True
    katana.save_error = RuntimeError("disk full")
    assert _adapter().save_scene() is False
    assert katana.events == ["save:" + PATH]


def test_katana_save_that_returns_but_leaves_it_dirty_is_a_failure(katana: Katana) -> None:
    katana.dirty = True
    katana.save_keeps_dirty = True
    assert _adapter().save_scene() is False
    assert katana.events == ["save:" + PATH]


def test_katana_untitled_project_cannot_save(katana: Katana) -> None:
    katana.file_name = ""
    katana.dirty = True
    assert _adapter().save_scene() is False
    assert katana.events == []


@respx.mock
@pytest.mark.usefixtures("_clean_settings")
def test_katana_contract_full_flow(katana: Katana) -> None:
    respx.get(f"{BASE}/api/v1/products").mock(
        return_value=httpx.Response(200, json=[{"name": "katana-render"}])
    )
    respx.get(f"{BASE}/api/v1/products/katana-render/parameters").mock(
        return_value=httpx.Response(
            200,
            json=[
                {"name": "SceneFile", "type": "PATH"},
                {"name": "Frames", "type": "STRING"},
                {"name": "RenderNode", "type": "STRING"},
                {"name": "FramesPerTask", "type": "INT", "default": "1"},
            ],
        )
    )
    route = respx.post(f"{BASE}/api/v1/products/katana-render/jobs").mock(
        return_value=httpx.Response(201, json={"id": "j9", "name": "n"})
    )
    katana.nodes = [Node("Utility"), Node("Beauty", active=(10, 20))]
    katana.dirty = True
    adapter = _adapter()
    session = SubmitterSession(server_url=BASE)
    model = FormModel.from_parameters(session.parameters("katana-render"))
    model.apply_prefill(
        prefill(
            [f.parameter for f in model.fields],
            adapter.scene_context(),
            adapter.render_targets()[1],
        )
    )
    job = submit_form(
        session,
        "katana-render",
        model,
        farm_id="f1",
        queue_id="q1",
        job_name="katana shot",
        adapter=adapter,
    )
    assert job.id == "j9"
    assert katana.events == ["save:" + PATH]  # the dirty project was saved first
    parameters = json.loads(route.calls.last.request.content)["parameters"]
    assert parameters["SceneFile"] == PATH
    assert parameters["Frames"] == "10-20"  # the node's farm range, not the project's
    assert parameters["RenderNode"] == "Beauty"


def test_katana_menu_parents_to_main_window(
    katana: Katana, monkeypatch: pytest.MonkeyPatch
) -> None:
    calls: list[tuple[Any, Any]] = []

    def fake_open(adapter: Any, server_url: Any = None, parent: Any = None) -> None:
        calls.append((adapter, parent))

    # Stub the module: importing the real qt.dialog fails without a Qt binding (CI has none).
    monkeypatch.setitem(
        sys.modules, "sqi_submitter.qt.dialog", SimpleNamespace(open_for_adapter=fake_open)
    )
    from sqi_submitter.hosts.katana import menu
    from sqi_submitter.hosts.katana.adapter import KatanaAdapter

    menu.open_submitter()
    [(adapter, parent)] = calls
    assert isinstance(adapter, KatanaAdapter)
    assert parent is katana.window
