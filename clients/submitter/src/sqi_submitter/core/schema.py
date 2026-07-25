# SPDX-License-Identifier: AGPL-3.0-or-later
"""UI-agnostic form model built from a product's parameter schema.

Mirrors the web submission form's widget/validation semantics (B2): explicit
userInterface control wins, otherwise a type-driven fallback; validation covers
required, numeric parsing, min/max, length limits, and allowed values.
"""

from __future__ import annotations

from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field

from sqi_client.models import ProductParameter
from sqi_submitter.core.mapping import is_scene_path_param


@dataclass
class FormField:
    """One parameter's presentation + editing state."""

    parameter: ProductParameter
    value: str | None = None
    error: str | None = None
    prefilled: bool = False

    @property
    def widget(self) -> str:
        ui = self.parameter.user_interface
        control = ui.control if (ui is not None and ui.control) else ""
        t = self.parameter.type
        # PATH is type-first: a PATH parameter with no control (or a stale
        # LINE_EDIT — no longer a legal control on PATH server-side, but
        # tolerated here defensively) derives the picker instead of falling
        # back to a plain text field. An explicit CHOOSE_* control falls
        # through to the `if control` branch below and yields the same
        # result; an explicit HIDDEN still wins. See docs/dcc-submitters.md.
        if t == "PATH" and control in ("", "LINE_EDIT"):
            if self.parameter.object_type == "DIRECTORY":
                return "CHOOSE_DIRECTORY"
            if self.parameter.data_flow == "OUT":
                return "CHOOSE_OUTPUT_FILE"
            return "CHOOSE_INPUT_FILE"
        if control:
            return control
        if t in ("INT", "FLOAT"):
            return "SPIN_BOX"
        if self.parameter.allowed_values:
            return "DROPDOWN_LIST"
        return "LINE_EDIT"

    @property
    def label(self) -> str:
        ui = self.parameter.user_interface
        return ui.label if ui is not None and ui.label else self.parameter.name

    @property
    def group(self) -> str:
        ui = self.parameter.user_interface
        return ui.group_label if ui is not None else ""

    @property
    def hidden(self) -> bool:
        return self.widget == "HIDDEN"

    @property
    def required(self) -> bool:
        return self.parameter.default is None

    @property
    def is_scene_path(self) -> bool:
        return is_scene_path_param(self.parameter.name)


@dataclass
class FormModel:
    """Ordered fields + values + validation; UIs bind to this, never to params."""

    fields: list[FormField] = field(default_factory=list)

    @classmethod
    def from_parameters(cls, parameters: Sequence[ProductParameter]) -> FormModel:
        return cls(fields=[FormField(parameter=p, value=p.default) for p in parameters])

    def _field(self, name: str) -> FormField:
        for f in self.fields:
            if f.parameter.name == name:
                return f
        raise KeyError(name)

    def set_value(self, name: str, value: str) -> None:
        f = self._field(name)
        f.value = value
        f.prefilled = False

    def apply_prefill(self, values: Mapping[str, str]) -> None:
        for f in self.fields:
            if f.hidden:
                continue
            if f.parameter.name in values:
                f.value = values[f.parameter.name]
                f.prefilled = True

    def validate(self) -> bool:
        ok = True
        for f in self.fields:
            f.error = _validate_field(f)
            ok = ok and f.error is None
        return ok

    def errors(self) -> dict[str, str]:
        return {f.parameter.name: f.error for f in self.fields if f.error}

    def values(self) -> dict[str, str]:
        return {f.parameter.name: f.value for f in self.fields if f.value is not None}

    def groups(self) -> list[tuple[str, list[FormField]]]:
        out: list[tuple[str, list[FormField]]] = []
        for f in self.fields:
            if f.hidden:
                continue
            if out and out[-1][0] == f.group:
                out[-1][1].append(f)
            else:
                out.append((f.group, [f]))
        return out


def _validate_field(f: FormField) -> str | None:
    p, v = f.parameter, f.value
    if v is None or v == "":
        return "Required" if f.required else None
    if p.type == "INT":
        try:
            num: float = int(v)
        except ValueError:
            return "Must be an integer"
    elif p.type == "FLOAT":
        try:
            num = float(v)
        except ValueError:
            return "Must be a number"
    if p.type in ("INT", "FLOAT"):
        if p.min_value is not None and num < float(p.min_value):
            return f"Must be at least {p.min_value}"
        if p.max_value is not None and num > float(p.max_value):
            return f"Must be at most {p.max_value}"
    if p.min_length is not None and len(v) < p.min_length:
        return f"Must be at least {p.min_length} characters"
    if p.max_length is not None and len(v) > p.max_length:
        return f"Must be at most {p.max_length} characters"
    if p.allowed_values and v not in p.allowed_values:
        return "Not an allowed value"
    return None
