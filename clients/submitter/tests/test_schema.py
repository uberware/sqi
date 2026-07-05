# SPDX-License-Identifier: AGPL-3.0-or-later
"""Form model tests (widget resolution, defaults, validation, grouping)."""

from typing import Any

import pytest

from sqi_client.models import ParameterUserInterface, ProductParameter
from sqi_submitter.core.schema import FormModel


def _p(name: str = "P", type_: str = "STRING", **kw: Any) -> ProductParameter:
    ui = kw.pop("ui", None)
    return ProductParameter(
        name=name,
        type=type_,
        user_interface=ParameterUserInterface(**ui) if ui else None,
        **kw,
    )


@pytest.mark.parametrize(
    ("param", "widget"),
    [
        (_p(type_="STRING"), "LINE_EDIT"),
        (_p(type_="STRING", allowed_values=["a", "b"]), "DROPDOWN_LIST"),
        (_p(type_="INT"), "SPIN_BOX"),
        (_p(type_="FLOAT"), "SPIN_BOX"),
        (_p(type_="PATH"), "CHOOSE_INPUT_FILE"),
        (_p(type_="PATH", object_type="DIRECTORY"), "CHOOSE_DIRECTORY"),
        (_p(type_="PATH", object_type="FILE", data_flow="OUT"), "CHOOSE_OUTPUT_FILE"),
        (_p(type_="STRING", ui={"control": "MULTILINE_EDIT"}), "MULTILINE_EDIT"),
    ],
)
def test_widget_resolution(param: ProductParameter, widget: str) -> None:
    model = FormModel.from_parameters([param])
    assert model.fields[0].widget == widget


def test_defaults_populate_values_and_label_falls_back_to_name() -> None:
    model = FormModel.from_parameters([_p(name="Interpreter", default="python3")])
    assert model.values() == {"Interpreter": "python3"}
    assert model.fields[0].label == "Interpreter"
    assert not model.fields[0].required


def test_required_field_without_value_blocks_validation() -> None:
    model = FormModel.from_parameters([_p(name="SceneFile", type_="PATH")])
    assert model.fields[0].required
    assert not model.validate()
    assert "required" in model.errors()["SceneFile"].lower()
    model.set_value("SceneFile", "/a/b.ma")
    assert model.validate()


@pytest.mark.parametrize(
    ("type_", "kw", "value", "ok"),
    [
        ("INT", {}, "12", True),
        ("INT", {}, "twelve", False),
        ("FLOAT", {}, "1.5", True),
        ("FLOAT", {}, "x", False),
        ("INT", {"min_value": "1", "max_value": "10"}, "11", False),
        ("INT", {"min_value": "1", "max_value": "10"}, "10", True),
        ("STRING", {"min_length": 3}, "ab", False),
        ("STRING", {"max_length": 3}, "abcd", False),
        ("STRING", {"allowed_values": ["a", "b"]}, "c", False),
        ("STRING", {"allowed_values": ["a", "b"]}, "b", True),
    ],
)
def test_constraint_validation(type_: str, kw: dict[str, Any], value: str, ok: bool) -> None:
    model = FormModel.from_parameters([_p(name="X", type_=type_, **kw)])
    model.set_value("X", value)
    assert model.validate() is ok


def test_hidden_field_is_not_rendered_but_default_submits() -> None:
    model = FormModel.from_parameters([_p(name="Secret", default="v", ui={"control": "HIDDEN"})])
    assert model.fields[0].hidden
    assert model.values() == {"Secret": "v"}
    model.apply_prefill({"Secret": "hacked"})
    assert model.values() == {"Secret": "v"}


def test_prefill_marks_fields_and_groups_preserve_order() -> None:
    model = FormModel.from_parameters(
        [
            _p(name="SceneFile", type_="PATH", ui={"group_label": "Scene"}),
            _p(name="Frames", ui={"group_label": "Scene"}),
            _p(name="Renderer"),
        ]
    )
    model.apply_prefill({"Frames": "1-10"})
    frames = next(f for f in model.fields if f.parameter.name == "Frames")
    assert frames.prefilled and frames.value == "1-10"
    assert [g for g, _ in model.groups()] == ["Scene", ""]
