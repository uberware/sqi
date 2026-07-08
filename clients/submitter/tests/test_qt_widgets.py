# SPDX-License-Identifier: AGPL-3.0-or-later
"""Qt widget binding tests (skipped without a Qt binding; offscreen in CI)."""

import os

import pytest

from sqi_client.models import ParameterUserInterface, ProductParameter

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
compat = pytest.importorskip("sqi_submitter.qt._compat")
if not compat.QT_BINDING:
    pytest.skip("no Qt binding installed", allow_module_level=True)

from sqi_submitter.core.schema import FormModel  # noqa: E402
from sqi_submitter.qt.widgets import build_form, refresh_form  # noqa: E402

QtWidgets = compat.QtWidgets


@pytest.fixture(scope="module")
def app() -> object:
    return QtWidgets.QApplication.instance() or QtWidgets.QApplication([])


def _model() -> FormModel:
    return FormModel.from_parameters(
        [
            ProductParameter(name="Frames", type="STRING"),
            ProductParameter(name="Chunk", type="INT", default="1"),
            ProductParameter(
                name="Renderer",
                type="STRING",
                allowed_values=["arnold", "vray"],
                user_interface=ParameterUserInterface(control="DROPDOWN_LIST"),
            ),
        ]
    )


def test_build_form_creates_bound_editors(app: object) -> None:
    model = _model()
    form = build_form(model)
    line = form.findChild(QtWidgets.QLineEdit, "field_Frames")
    assert line is not None
    line.setText("1-10")
    line.editingFinished.emit()
    assert model.values()["Frames"] == "1-10"


def test_spin_and_dropdown_bind(app: object) -> None:
    model = _model()
    form = build_form(model)
    spin = form.findChild(QtWidgets.QSpinBox, "field_Chunk")
    assert spin is not None
    spin.setValue(5)
    combo = form.findChild(QtWidgets.QComboBox, "field_Renderer")
    assert combo is not None
    combo.setCurrentIndex(1)
    assert model.values()["Chunk"] == "5"
    assert model.values()["Renderer"] == "vray"


def _check_box_model(value: str) -> FormModel:
    # allowed_values[0] is the unchecked value, [1] is checked — matches the
    # web form's `const [off, on] = allowed_values` (ProductParamField.tsx).
    return FormModel.from_parameters(
        [
            ProductParameter(
                name="Skip",
                type="STRING",
                default=value,
                allowed_values=["off", "on"],
                user_interface=ParameterUserInterface(control="CHECK_BOX"),
            ),
        ]
    )


def test_check_box_initial_state_matches_web_order(app: object) -> None:
    model = _check_box_model("on")
    form = build_form(model)
    box = form.findChild(QtWidgets.QCheckBox, "field_Skip")
    assert box is not None
    assert box.isChecked() is True

    unchecked_model = _check_box_model("off")
    unchecked_form = build_form(unchecked_model)
    unchecked_box = unchecked_form.findChild(QtWidgets.QCheckBox, "field_Skip")
    assert unchecked_box is not None
    assert unchecked_box.isChecked() is False


def test_check_box_toggle_writes_web_order_values(app: object) -> None:
    model = _check_box_model("off")
    form = build_form(model)
    box = form.findChild(QtWidgets.QCheckBox, "field_Skip")
    assert box is not None

    box.setChecked(True)
    assert model.values()["Skip"] == "on"
    box.setChecked(False)
    assert model.values()["Skip"] == "off"


def test_check_box_refresh_form_matches_web_order(app: object) -> None:
    model = _check_box_model("off")
    form = build_form(model)
    box = form.findChild(QtWidgets.QCheckBox, "field_Skip")
    assert box is not None

    model.apply_prefill({"Skip": "on"})
    refresh_form(form, model)
    assert box.isChecked() is True


def test_refresh_form_shows_prefill(app: object) -> None:
    model = _model()
    form = build_form(model)
    model.apply_prefill({"Frames": "8-9"})
    refresh_form(form, model)
    line = form.findChild(QtWidgets.QLineEdit, "field_Frames")
    assert line is not None
    assert line.text() == "8-9"


def test_dropdown_without_default_shows_no_selection(app: object) -> None:
    model = _model()
    form = build_form(model)
    combo = form.findChild(QtWidgets.QComboBox, "field_Renderer")
    assert combo is not None
    assert combo.currentIndex() == -1
    assert "Renderer" not in model.values()
    assert model.validate() is False


def test_refresh_form_syncs_prefill_tooltip(app: object) -> None:
    model = _model()
    form = build_form(model)
    line = form.findChild(QtWidgets.QLineEdit, "field_Frames")
    assert line is not None
    assert line.toolTip() == ""
    model.apply_prefill({"Frames": "1-5"})
    refresh_form(form, model)
    assert line.toolTip() == "Pre-filled from the scene"
    model.set_value("Frames", "2-6")
    refresh_form(form, model)
    assert line.toolTip() == ""


def test_build_form_hides_named_fields(app: object) -> None:
    from sqi_submitter.qt.widgets import _find_editor

    model = FormModel.from_parameters(
        [
            ProductParameter(name="SceneFile", type="PATH"),
            ProductParameter(name="Frames", type="STRING", default="1-10"),
        ]
    )
    root = build_form(model, hidden_names=frozenset({"SceneFile"}))
    assert _find_editor(root, "SceneFile") is None
    assert _find_editor(root, "Frames") is not None


def test_labeled_path_renders_picker_with_browse_button(app: object) -> None:
    from sqi_submitter.qt.widgets import _find_editor

    model = FormModel.from_parameters(
        [
            ProductParameter(
                name="OutputDir",
                type="PATH",
                object_type="DIRECTORY",
                user_interface=ParameterUserInterface(
                    control="LINE_EDIT", label="Output Directory"
                ),
            )
        ]
    )
    root = build_form(model)
    line = _find_editor(root, "OutputDir")
    assert line is not None
    # _path_row wraps the line edit and a browse QToolButton in one parent row.
    parent = line.parent()
    assert parent is not None
    assert parent.findChild(QtWidgets.QToolButton) is not None
