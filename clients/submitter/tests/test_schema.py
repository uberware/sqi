# SPDX-License-Identifier: AGPL-3.0-or-later
"""Form model tests (widget resolution, defaults, validation, grouping)."""

from __future__ import annotations

from typing import Any

import pytest

from sqi_client.models import ParameterUserInterface, ProductParameter, ProductParameterItem
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


def test_set_value_clears_prefilled_flag() -> None:
    model = FormModel.from_parameters([_p(name="Frames")])
    model.apply_prefill({"Frames": "1-10"})
    frames = model.fields[0]
    assert frames.prefilled is True
    model.set_value("Frames", "5")
    assert frames.prefilled is False
    assert frames.value == "5"


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


def test_form_field_is_scene_path() -> None:
    model = FormModel.from_parameters(
        [_p(name="SceneFile", type_="PATH"), _p(name="Frames", type_="STRING")]
    )
    scene, frames = model.fields
    assert scene.is_scene_path is True
    assert frames.is_scene_path is False


@pytest.mark.parametrize(
    ("param", "widget"),
    [
        # PATH is type-first: an absent control, or a stale LINE_EDIT (not a
        # legal control on PATH server-side, but tolerated here defensively),
        # must not suppress the derived picker.
        (_p(type_="PATH", ui={"control": "LINE_EDIT"}), "CHOOSE_INPUT_FILE"),
        (
            _p(type_="PATH", object_type="DIRECTORY", ui={"control": "LINE_EDIT"}),
            "CHOOSE_DIRECTORY",
        ),
        (
            _p(type_="PATH", object_type="FILE", data_flow="OUT", ui={"control": "LINE_EDIT"}),
            "CHOOSE_OUTPUT_FILE",
        ),
        # An explicit HIDDEN still wins over the picker.
        (_p(type_="PATH", ui={"control": "HIDDEN"}), "HIDDEN"),
    ],
)
def test_path_is_type_first(param: ProductParameter, widget: str) -> None:
    model = FormModel.from_parameters([param])
    assert model.fields[0].widget == widget


def test_labeled_path_keeps_label() -> None:
    # CHOOSE_DIRECTORY is the server-valid control for a labeled directory
    # PATH parameter (LINE_EDIT is not legal on PATH); it falls through to
    # `if control: return control` and keeps the label alongside it.
    param = _p(
        name="OutputDir",
        type_="PATH",
        object_type="DIRECTORY",
        ui={"control": "CHOOSE_DIRECTORY", "label": "Output Directory"},
    )
    field = FormModel.from_parameters([param]).fields[0]
    assert field.widget == "CHOOSE_DIRECTORY"
    assert field.label == "Output Directory"


def _list_error(
    type_: str, value: str, item: ProductParameterItem | None = None, **kw: Any
) -> str | None:
    model = FormModel.from_parameters([_p(name="L", type_=type_, item=item, **kw)])
    model.set_value("L", value)
    model.validate()
    return model.fields[0].error


@pytest.mark.parametrize(
    ("value", "error"),
    [
        ("[]", "Must have at least 1 item"),
        ('["a","b","c"]', "Must have at most 2 items"),
        ('["abcd"]', "Item 1: Must be at most 3 characters"),
        ('["a","abcd"]', "Item 2: Must be at most 3 characters"),
        ('["a",""]', "Item 2: Must be at least 1 characters"),
        ("not json", "Must be a list"),
        ('{"a": 1}', "Must be a list"),
        ('"a"', "Must be a list"),
        ('["a"]', None),
        ('["a","bcd"]', None),
        ("[1]", "Item 1: Must be text"),
    ],
)
def test_list_string_validation(value: str, error: str | None) -> None:
    item = ProductParameterItem(min_length=1, max_length=3)
    assert _list_error("LIST[STRING]", value, item, min_length=1, max_length=2) == error


def test_list_length_bounds_are_item_counts_not_characters() -> None:
    # A 2-item list is far longer than 2 characters; only the item count counts.
    assert _list_error("LIST[STRING]", '["abcdef","ghijkl"]', None, max_length=2) is None


def test_list_message_is_singular_for_one_item() -> None:
    assert _list_error("LIST[STRING]", '["a","b"]', None, max_length=1) == (
        "Must have at most 1 item"
    )


@pytest.mark.parametrize(
    ("value", "error"),
    [
        ("[1, 5, 10]", None),
        ("[0]", "Item 1: Must be at least 1"),
        ("[1, 11]", "Item 2: Must be at most 10"),
        ('[1, "x"]', "Item 2: Must be an integer"),
        ("[1.5]", "Item 1: Must be an integer"),
        ("[true]", "Item 1: Must be an integer"),
    ],
)
def test_list_int_items_are_checked_as_integers(value: str, error: str | None) -> None:
    item = ProductParameterItem(min_value="1", max_value="10")
    assert _list_error("LIST[INT]", value, item) == error


def test_list_float_items_are_checked_as_numbers() -> None:
    item = ProductParameterItem(min_value="0.5")
    assert _list_error("LIST[FLOAT]", "[1, 2.5]", item) is None
    assert _list_error("LIST[FLOAT]", "[0.25]", item) == "Item 1: Must be at least 0.5"
    assert _list_error("LIST[FLOAT]", '["x"]', item) == "Item 1: Must be a number"


def test_list_item_allowed_values() -> None:
    item = ProductParameterItem(allowed_values=["a", "b"])
    assert _list_error("LIST[STRING]", '["a","b"]', item) is None
    assert _list_error("LIST[STRING]", '["a","c"]', item) == "Item 2: Not an allowed value"


def test_list_without_item_constraints_checks_only_the_list() -> None:
    assert _list_error("LIST[PATH]", '["/a","/b"]') is None


def test_nested_list_validates_each_inner_list() -> None:
    item = ProductParameterItem(min_length=1, item=ProductParameterItem(min_value="0"))
    assert _list_error("LIST[LIST[INT]]", "[[1,2],[3]]", item) is None
    assert _list_error("LIST[LIST[INT]]", "[[1],[]]", item) == "Item 2: Must have at least 1 item"
    assert _list_error("LIST[LIST[INT]]", "[[1],[-1]]", item) == (
        "Item 2: Item 1: Must be at least 0"
    )
    assert _list_error("LIST[LIST[INT]]", "[1]", item) == "Item 1: Must be a list"


def test_empty_list_value_follows_the_required_rule() -> None:
    assert _list_error("LIST[STRING]", "") == "Required"
    model = FormModel.from_parameters([_p(name="L", type_="LIST[STRING]", default='["a"]')])
    model.set_value("L", "")
    assert model.validate()


@pytest.mark.parametrize(
    ("value", "error"),
    [
        ("[true, false]", None),
        ('["maybe"]', "Item 1: Must be true or false"),
        ('["True"]', "Item 1: Must be true or false"),
        ('[true, "false"]', "Item 2: Must be true or false"),
        ("[1]", "Item 1: Must be true or false"),
    ],
)
def test_list_bool_items_must_be_json_booleans(value: str, error: str | None) -> None:
    assert _list_error("LIST[BOOL]", value) == error
