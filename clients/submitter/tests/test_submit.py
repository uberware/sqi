# SPDX-License-Identifier: AGPL-3.0-or-later
"""submit_form: host-managed scene path forcing and save-required guard."""

from __future__ import annotations

from typing import Any

import pytest

from sqi_client.models import ProductParameter
from sqi_submitter.core.adapter import HostAdapter
from sqi_submitter.core.context import RenderTarget, SceneContext
from sqi_submitter.core.errors import SubmitterError
from sqi_submitter.core.schema import FormModel
from sqi_submitter.core.submit import submit_form


class _FakeAdapter(HostAdapter):
    host_name = "fake"

    def __init__(self, scene_path: str | None) -> None:
        self._scene_path = scene_path

    def scene_context(self) -> SceneContext:
        return SceneContext(scene_path=self._scene_path)

    def render_targets(self) -> list[RenderTarget]:
        return []


class _FakeSession:
    def __init__(self) -> None:
        self.calls: list[dict[str, Any]] = []

    def submit(self, product_name: str, **kwargs: Any) -> Any:
        self.calls.append({"product_name": product_name, **kwargs})
        return object()  # sentinel Job


def _model_with_scene(value: str | None = None) -> FormModel:
    model = FormModel.from_parameters(
        [
            ProductParameter(name="SceneFile", type="PATH"),
            ProductParameter(name="Frames", type="STRING", default="1-10"),
        ]
    )
    if value is not None:
        model.set_value("SceneFile", value)
    return model


def test_forces_scene_path_from_adapter() -> None:
    session = _FakeSession()
    model = _model_with_scene(value="/stale/old.blend")
    submit_form(
        session,  # type: ignore[arg-type]
        "blender-batch-render",
        model,
        farm_id="f",
        queue_id="q",
        adapter=_FakeAdapter("/shows/a/shot.blend"),
        save_scene=False,
    )
    assert session.calls[0]["parameters"]["SceneFile"] == "/shows/a/shot.blend"


def test_raises_when_scene_never_saved() -> None:
    session = _FakeSession()
    with pytest.raises(SubmitterError, match="Save your scene"):
        submit_form(
            session,  # type: ignore[arg-type]
            "blender-batch-render",
            _model_with_scene(),
            farm_id="f",
            queue_id="q",
            adapter=_FakeAdapter(None),
            save_scene=False,
        )
    assert session.calls == []  # never posted


def test_no_adapter_leaves_scene_field_alone() -> None:
    session = _FakeSession()
    model = _model_with_scene(value="/typed/by/hand.blend")
    submit_form(
        session,  # type: ignore[arg-type]
        "blender-batch-render",
        model,
        farm_id="f",
        queue_id="q",
        adapter=None,
    )
    assert session.calls[0]["parameters"]["SceneFile"] == "/typed/by/hand.blend"
