# SPDX-License-Identifier: AGPL-3.0-or-later
"""Blender add-on pure helpers (no bpy import needed)."""

from __future__ import annotations

from types import SimpleNamespace

import pytest

from sqi_client.models import ParameterUserInterface, ProductParameter
from sqi_submitter.core.schema import FormModel
from sqi_submitter.hosts.blender import addon


def _model() -> FormModel:
    return FormModel.from_parameters(
        [
            ProductParameter(name="SceneFile", type="PATH"),
            ProductParameter(name="Frames", type="STRING", default="1-10"),
            ProductParameter(
                name="Debug",
                type="STRING",
                user_interface=ParameterUserInterface(control="HIDDEN"),
            ),
        ]
    )


def test_visible_fields_excludes_scene_path_and_hidden() -> None:
    names = [f.parameter.name for f in addon._visible_fields(_model())]
    assert names == ["Frames"]


def test_field_layout_excludes_scene_path() -> None:
    rows = addon.field_layout(_model())
    assert [name for name, _label, _widget in rows] == ["Frames"]


def test_scene_prop_value_coerces_by_widget() -> None:
    model = FormModel.from_parameters(
        [
            ProductParameter(name="Frames", type="STRING", default="1-10"),
            ProductParameter(name="Samples", type="INT", default="64"),
            ProductParameter(name="Gamma", type="FLOAT", default="2.2"),
            ProductParameter(
                name="Denoise",
                type="STRING",
                allowed_values=["off", "on"],
                default="on",
                user_interface=ParameterUserInterface(control="CHECK_BOX"),
            ),
        ]
    )
    by_name = {f.parameter.name: f for f in model.fields}
    assert addon._scene_prop_value(by_name["Frames"]) == "1-10"
    assert addon._scene_prop_value(by_name["Samples"]) == 64
    assert addon._scene_prop_value(by_name["Gamma"]) == 2.2
    assert addon._scene_prop_value(by_name["Denoise"]) is True


def test_field_rows_attaches_errors() -> None:
    model = FormModel.from_parameters(
        [
            ProductParameter(name="Frames", type="STRING", default="1-10"),
            ProductParameter(name="OutputDir", type="PATH", object_type="DIRECTORY"),
        ]
    )
    rows = addon.field_rows(model, {"OutputDir": "Required"})
    assert rows == [
        ("Frames", "Frames", "LINE_EDIT", None),
        ("OutputDir", "OutputDir", "CHOOSE_DIRECTORY", "Required"),
    ]


@pytest.fixture(autouse=True)
def _reset_farm_queue_state() -> object:
    saved = (addon._state["farms"], addon._state["queues"])
    addon._state["farms"], addon._state["queues"] = [], []
    yield
    addon._state["farms"], addon._state["queues"] = saved


def _farm(id_: str, name: str) -> SimpleNamespace:
    return SimpleNamespace(id=id_, name=name)


def test_farm_enum_items_empty_prompts_refresh() -> None:
    addon._state["farms"] = []
    items = addon._farm_enum_items(None, None)
    assert items[0][0] == "NONE"


def test_farm_enum_items_lists_farms() -> None:
    addon._state["farms"] = [_farm("f1", "Farm A"), _farm("f2", "Farm B")]
    assert addon._farm_enum_items(None, None) == [("0", "Farm A", ""), ("1", "Farm B", "")]


def test_queue_enum_items_lists_queues() -> None:
    addon._state["queues"] = [_farm("q1", "Nightly"), _farm("q2", "Interactive")]
    assert addon._queue_enum_items(None, None) == [("0", "Nightly", ""), ("1", "Interactive", "")]


def test_selected_farm_by_index() -> None:
    addon._state["farms"] = [_farm("f1", "A"), _farm("f2", "B")]
    farm = addon._selected_farm(SimpleNamespace(farm="1"))
    assert farm is not None
    assert farm.id == "f2"


def test_selected_farm_none_when_no_farms() -> None:
    addon._state["farms"] = []
    assert addon._selected_farm(SimpleNamespace(farm="NONE")) is None


def test_selected_queue_by_index() -> None:
    addon._state["queues"] = [_farm("q1", "A"), _farm("q2", "B")]
    queue = addon._selected_queue(SimpleNamespace(queue="0"))
    assert queue is not None
    assert queue.id == "q1"
