# SPDX-License-Identifier: AGPL-3.0-or-later
"""Blender add-on pure helpers (no bpy import needed)."""

from __future__ import annotations

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
