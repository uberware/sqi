# SPDX-License-Identifier: AGPL-3.0-or-later
"""FormModel → Qt editors. All state lives in the FormModel, not the widgets."""

from __future__ import annotations

from sqi_submitter.core.schema import FormField, FormModel
from sqi_submitter.qt._compat import QtWidgets, require_qt

_FILE_PICKERS = {"CHOOSE_INPUT_FILE", "CHOOSE_OUTPUT_FILE", "CHOOSE_DIRECTORY"}
_PREFILL_TIP = "Pre-filled from the scene"


def build_form(
    model: FormModel,
    parent: QtWidgets.QWidget | None = None,
    hidden_names: frozenset[str] = frozenset(),
) -> QtWidgets.QWidget:
    require_qt()
    root = QtWidgets.QWidget(parent)
    outer = QtWidgets.QVBoxLayout(root)
    for group, fields in model.groups():
        visible = [f for f in fields if f.parameter.name not in hidden_names]
        if not visible:
            continue
        box: QtWidgets.QWidget = (
            QtWidgets.QGroupBox(group, root) if group else QtWidgets.QWidget(root)
        )
        layout = QtWidgets.QFormLayout(box)
        for f in visible:
            layout.addRow(f.label, _editor(f, model, box))
        outer.addWidget(box)
    return root


def refresh_form(root: QtWidgets.QWidget, model: FormModel) -> None:
    """Re-read model values into their editors (e.g. after a prefill/target change)."""
    for f in model.fields:
        if f.hidden:
            continue
        editor = _find_editor(root, f.parameter.name)
        if editor is None:
            continue
        editor.blockSignals(True)
        try:
            _write_value(editor, f)
        finally:
            editor.blockSignals(False)
        editor.setToolTip(_PREFILL_TIP if f.prefilled else "")


def _find_editor(root: QtWidgets.QWidget, name: str) -> QtWidgets.QWidget | None:
    for cls in (
        QtWidgets.QLineEdit,
        QtWidgets.QComboBox,
        QtWidgets.QCheckBox,
        QtWidgets.QPlainTextEdit,
        QtWidgets.QSpinBox,
        QtWidgets.QDoubleSpinBox,
    ):
        w = root.findChild(cls, f"field_{name}")
        if w is not None:
            return w
    return None


def _write_value(w: QtWidgets.QWidget, f: FormField) -> None:
    if isinstance(w, QtWidgets.QPlainTextEdit):
        w.setPlainText(f.value or "")
    elif isinstance(w, QtWidgets.QLineEdit):
        w.setText(f.value or "")
    elif isinstance(w, QtWidgets.QComboBox):
        allowed = f.parameter.allowed_values or []
        if f.value is not None and f.value in allowed:
            w.setCurrentText(f.value)
        else:
            w.setCurrentIndex(-1)
    elif isinstance(w, QtWidgets.QCheckBox):
        _, on = _check_states(f)
        w.setChecked(f.value == on)
    elif isinstance(w, QtWidgets.QSpinBox):
        if f.value is not None:
            w.setValue(int(float(f.value)))
    elif isinstance(w, QtWidgets.QDoubleSpinBox) and f.value is not None:
        w.setValue(float(f.value))


def _check_states(f: FormField) -> tuple[str, str]:
    """(off, on) model values: allowed_values[0] is unchecked, [1] is checked.

    Matches the web form's convention (web/src/components/ProductParamField.tsx
    ``const [off, on] = allowed_values``). Falls back to ("false", "true") when
    allowed_values is short.
    """
    allowed = f.parameter.allowed_values or []
    if len(allowed) >= 2:
        return allowed[0], allowed[1]
    return "false", "true"


def _editor(f: FormField, model: FormModel, parent: QtWidgets.QWidget) -> QtWidgets.QWidget:
    if f.widget in _FILE_PICKERS:
        return _path_row(f, model, parent, f.widget)
    w = _make_editor(f, model, parent)
    w.setObjectName(f"field_{f.parameter.name}")
    if f.prefilled:
        w.setToolTip(_PREFILL_TIP)
    return w


def _make_editor(f: FormField, model: FormModel, parent: QtWidgets.QWidget) -> QtWidgets.QWidget:
    kind = f.widget
    if kind == "MULTILINE_EDIT":
        return _multiline_edit(f, model, parent)
    if kind == "SPIN_BOX" and f.parameter.type == "INT":
        return _int_spin_box(f, model, parent)
    if kind == "SPIN_BOX":
        return _double_spin_box(f, model, parent)
    if kind == "CHECK_BOX":
        return _check_box(f, model, parent)
    if kind == "DROPDOWN_LIST":
        return _dropdown(f, model, parent)
    # LINE_EDIT and unknown controls
    return _line_edit(f, model, parent)


def _multiline_edit(
    f: FormField, model: FormModel, parent: QtWidgets.QWidget
) -> QtWidgets.QPlainTextEdit:
    name = f.parameter.name
    w = QtWidgets.QPlainTextEdit(f.value or "", parent)
    w.textChanged.connect(lambda: model.set_value(name, w.toPlainText()))
    return w


def _int_spin_box(f: FormField, model: FormModel, parent: QtWidgets.QWidget) -> QtWidgets.QSpinBox:
    name = f.parameter.name
    w = QtWidgets.QSpinBox(parent)
    w.setRange(-(2**31), 2**31 - 1)
    if f.value is not None:
        w.setValue(int(float(f.value)))
    w.valueChanged.connect(lambda v: model.set_value(name, str(v)))
    return w


def _double_spin_box(
    f: FormField, model: FormModel, parent: QtWidgets.QWidget
) -> QtWidgets.QDoubleSpinBox:
    name = f.parameter.name
    w = QtWidgets.QDoubleSpinBox(parent)
    w.setRange(-1e12, 1e12)
    ui = f.parameter.user_interface
    if ui is not None and ui.decimals is not None:
        w.setDecimals(ui.decimals)
    if f.value is not None:
        w.setValue(float(f.value))
    w.valueChanged.connect(lambda v: model.set_value(name, str(v)))
    return w


def _check_box(f: FormField, model: FormModel, parent: QtWidgets.QWidget) -> QtWidgets.QCheckBox:
    name = f.parameter.name
    off, on = _check_states(f)
    w = QtWidgets.QCheckBox(parent)
    w.setChecked(f.value == on)
    w.toggled.connect(lambda checked: model.set_value(name, on if checked else off))
    return w


def _dropdown(f: FormField, model: FormModel, parent: QtWidgets.QWidget) -> QtWidgets.QComboBox:
    name = f.parameter.name
    allowed = f.parameter.allowed_values or []
    w = QtWidgets.QComboBox(parent)
    w.addItems(allowed)
    if f.value is not None and f.value in allowed:
        w.setCurrentText(f.value)
    else:
        # View matches the empty model: no selection until the artist picks
        # (or a prefill lands), so required-field validation stays meaningful.
        w.setCurrentIndex(-1)
    w.currentTextChanged.connect(lambda t: model.set_value(name, t))
    return w


def _line_edit(f: FormField, model: FormModel, parent: QtWidgets.QWidget) -> QtWidgets.QLineEdit:
    name = f.parameter.name
    w = QtWidgets.QLineEdit(f.value or "", parent)
    w.editingFinished.connect(lambda: model.set_value(name, w.text()))
    return w


def _path_row(
    f: FormField, model: FormModel, parent: QtWidgets.QWidget, kind: str
) -> QtWidgets.QWidget:
    name = f.parameter.name
    row = QtWidgets.QWidget(parent)
    layout = QtWidgets.QHBoxLayout(row)
    layout.setContentsMargins(0, 0, 0, 0)

    line = QtWidgets.QLineEdit(f.value or "", row)
    line.setObjectName(f"field_{name}")
    line.editingFinished.connect(lambda: model.set_value(name, line.text()))
    if f.prefilled:
        line.setToolTip(_PREFILL_TIP)

    button = QtWidgets.QToolButton(row)
    button.setText("…")

    def _browse() -> None:
        caption = f.label
        if kind == "CHOOSE_INPUT_FILE":
            path, _ = QtWidgets.QFileDialog.getOpenFileName(row.window(), caption, line.text())
        elif kind == "CHOOSE_OUTPUT_FILE":
            path, _ = QtWidgets.QFileDialog.getSaveFileName(row.window(), caption, line.text())
        else:  # CHOOSE_DIRECTORY
            path = QtWidgets.QFileDialog.getExistingDirectory(row.window(), caption, line.text())
        if path:
            line.setText(path)
            model.set_value(name, path)

    button.clicked.connect(_browse)

    layout.addWidget(line)
    layout.addWidget(button)
    return row
