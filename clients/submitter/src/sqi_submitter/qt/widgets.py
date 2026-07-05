# SPDX-License-Identifier: AGPL-3.0-or-later
"""FormModel → Qt editors. All state lives in the FormModel, not the widgets."""

from __future__ import annotations

from typing import Any

from sqi_submitter.core.schema import FormField, FormModel
from sqi_submitter.qt._compat import QtWidgets, require_qt

_FILE_PICKERS = {"CHOOSE_INPUT_FILE", "CHOOSE_OUTPUT_FILE", "CHOOSE_DIRECTORY"}


def build_form(model: FormModel, parent: Any = None) -> Any:
    require_qt()
    root = QtWidgets.QWidget(parent)
    outer = QtWidgets.QVBoxLayout(root)
    for group, fields in model.groups():
        box = QtWidgets.QGroupBox(group, root) if group else QtWidgets.QWidget(root)
        layout = QtWidgets.QFormLayout(box)
        for f in fields:
            layout.addRow(f.label, _editor(f, model, box))
        outer.addWidget(box)
    return root


def refresh_form(root: Any, model: FormModel) -> None:
    """Re-read model values into their editors (e.g. after a prefill/target change)."""
    for f in model.fields:
        if f.hidden:
            continue
        name = f.parameter.name
        editor = root.findChild(QtWidgets.QLineEdit, f"field_{name}")
        if editor is not None:
            editor.blockSignals(True)
            editor.setText(f.value or "")
            editor.blockSignals(False)
            continue
        combo = root.findChild(QtWidgets.QComboBox, f"field_{name}")
        if combo is not None:
            combo.blockSignals(True)
            if f.value in (f.parameter.allowed_values or []):
                combo.setCurrentText(f.value)
            combo.blockSignals(False)
            continue
        check = root.findChild(QtWidgets.QCheckBox, f"field_{name}")
        if check is not None:
            allowed = f.parameter.allowed_values or ["true", "false"]
            check.blockSignals(True)
            check.setChecked(f.value == allowed[0])
            check.blockSignals(False)
            continue
        plain = root.findChild(QtWidgets.QPlainTextEdit, f"field_{name}")
        if plain is not None:
            plain.blockSignals(True)
            plain.setPlainText(f.value or "")
            plain.blockSignals(False)
            continue
        spin_int = root.findChild(QtWidgets.QSpinBox, f"field_{name}")
        if spin_int is not None:
            spin_int.blockSignals(True)
            if f.value is not None:
                spin_int.setValue(int(float(f.value)))
            spin_int.blockSignals(False)
            continue
        spin_float = root.findChild(QtWidgets.QDoubleSpinBox, f"field_{name}")
        if spin_float is not None:
            spin_float.blockSignals(True)
            if f.value is not None:
                spin_float.setValue(float(f.value))
            spin_float.blockSignals(False)
            continue


def _editor(f: FormField, model: FormModel, parent: Any) -> Any:
    name = f.parameter.name
    kind = f.widget
    w: Any
    if kind == "MULTILINE_EDIT":
        w = QtWidgets.QPlainTextEdit(f.value or "", parent)
        w.textChanged.connect(lambda: model.set_value(name, w.toPlainText()))
    elif kind == "SPIN_BOX" and f.parameter.type == "INT":
        w = QtWidgets.QSpinBox(parent)
        w.setRange(-(2**31), 2**31 - 1)
        if f.value is not None:
            w.setValue(int(float(f.value)))
        w.valueChanged.connect(lambda v: model.set_value(name, str(v)))
    elif kind == "SPIN_BOX":
        w = QtWidgets.QDoubleSpinBox(parent)
        w.setRange(-1e12, 1e12)
        if f.parameter.user_interface and f.parameter.user_interface.decimals is not None:
            w.setDecimals(f.parameter.user_interface.decimals)
        if f.value is not None:
            w.setValue(float(f.value))
        w.valueChanged.connect(lambda v: model.set_value(name, str(v)))
    elif kind == "CHECK_BOX":
        w = QtWidgets.QCheckBox(parent)
        allowed = f.parameter.allowed_values or ["true", "false"]
        w.setChecked(f.value == allowed[0])
        w.toggled.connect(lambda on: model.set_value(name, allowed[0] if on else allowed[1]))
    elif kind == "DROPDOWN_LIST":
        w = QtWidgets.QComboBox(parent)
        w.addItems(f.parameter.allowed_values or [])
        if f.value in (f.parameter.allowed_values or []):
            w.setCurrentText(f.value)
        w.currentTextChanged.connect(lambda t: model.set_value(name, t))
    elif kind in _FILE_PICKERS:
        w = _path_row(f, model, parent, kind)
        return w
    else:  # LINE_EDIT and unknown controls
        w = QtWidgets.QLineEdit(f.value or "", parent)
        w.editingFinished.connect(lambda: model.set_value(name, w.text()))
    w.setObjectName(f"field_{name}")
    if f.prefilled:
        w.setToolTip("Pre-filled from the scene")
    return w


def _path_row(f: FormField, model: FormModel, parent: Any, kind: str) -> Any:
    name = f.parameter.name
    row = QtWidgets.QWidget(parent)
    layout = QtWidgets.QHBoxLayout(row)
    layout.setContentsMargins(0, 0, 0, 0)

    line = QtWidgets.QLineEdit(f.value or "", row)
    line.setObjectName(f"field_{name}")
    line.editingFinished.connect(lambda: model.set_value(name, line.text()))
    if f.prefilled:
        line.setToolTip("Pre-filled from the scene")

    button = QtWidgets.QToolButton(row)
    button.setText("…")

    def _browse() -> None:
        parent_widget = row.window()
        caption = f.label
        if kind == "CHOOSE_INPUT_FILE":
            path, _ = QtWidgets.QFileDialog.getOpenFileName(parent_widget, caption, line.text())
        elif kind == "CHOOSE_OUTPUT_FILE":
            path, _ = QtWidgets.QFileDialog.getSaveFileName(parent_widget, caption, line.text())
        else:  # CHOOSE_DIRECTORY
            path = QtWidgets.QFileDialog.getExistingDirectory(parent_widget, caption, line.text())
        if path:
            line.setText(path)
            model.set_value(name, path)

    button.clicked.connect(_browse)

    layout.addWidget(line)
    layout.addWidget(button)
    return row
