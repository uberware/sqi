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
    spin.setValue(5)
    combo = form.findChild(QtWidgets.QComboBox, "field_Renderer")
    combo.setCurrentIndex(1)
    assert model.values()["Chunk"] == "5"
    assert model.values()["Renderer"] == "vray"


def test_refresh_form_shows_prefill(app: object) -> None:
    model = _model()
    form = build_form(model)
    model.apply_prefill({"Frames": "8-9"})
    refresh_form(form, model)
    assert form.findChild(QtWidgets.QLineEdit, "field_Frames").text() == "8-9"
