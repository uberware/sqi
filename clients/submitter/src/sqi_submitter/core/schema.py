# SPDX-License-Identifier: AGPL-3.0-or-later
"""UI-agnostic form model built from a product's parameter schema.

Mirrors the web submission form's widget/validation semantics: explicit
userInterface control wins, otherwise a type-driven fallback; validation covers
required, numeric parsing, min/max, length limits, and allowed values.
"""

from __future__ import annotations

import json
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from typing import Any

from sqi_client.models import ProductParameter, ProductParameterItem
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
        # LINE_EDIT — not a legal control on PATH server-side, but tolerated
        # here defensively) derives the picker instead of falling
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
    if p.type.startswith("LIST["):
        return _validate_list(p, v)
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


def _validate_list(p: ProductParameter, text: str) -> str | None:
    """Check a ``LIST[*]`` value: JSON array text, its length, then each element.

    ``min_length``/``max_length`` on the parameter count items; the same keys on
    ``item`` bound a string element's characters or an inner list's items.
    """
    try:
        parsed = json.loads(text)
    except ValueError:
        return "Must be a list"
    return _check_list(parsed, p.type, p.min_length, p.max_length, p.item)


def _check_list(
    value: Any,
    list_type: str,
    min_items: int | None,
    max_items: int | None,
    item: ProductParameterItem | None,
) -> str | None:
    if not isinstance(value, list):
        return "Must be a list"
    if min_items is not None and len(value) < min_items:
        return f"Must have at least {min_items} {_items(min_items)}"
    if max_items is not None and len(value) > max_items:
        return f"Must have at most {max_items} {_items(max_items)}"
    element_type = list_type[len("LIST[") : -1]
    for i, element in enumerate(value, start=1):
        err = _check_element(element, element_type, item)
        if err is not None:
            return f"Item {i}: {err}"
    return None


def _items(n: int) -> str:
    return "item" if n == 1 else "items"


def _check_element(
    element: Any, element_type: str, item: ProductParameterItem | None
) -> str | None:
    if element_type.startswith("LIST["):
        return _check_list(
            element,
            element_type,
            item.min_length if item else None,
            item.max_length if item else None,
            item.item if item else None,
        )
    if element_type in ("INT", "FLOAT"):
        err = _check_number(element, element_type, item)
    elif element_type in ("STRING", "PATH"):
        err = _check_text(element, item)
    else:
        err = None
    if err is None and item is not None and item.allowed_values:
        shown = element if isinstance(element, str) else json.dumps(element)
        if shown not in item.allowed_values:
            return "Not an allowed value"
    return err


def _check_number(element: Any, element_type: str, item: ProductParameterItem | None) -> str | None:
    is_number = isinstance(element, (int, float)) and not isinstance(element, bool)
    if element_type == "INT":
        if not is_number or (isinstance(element, float) and not element.is_integer()):
            return "Must be an integer"
    elif not is_number:
        return "Must be a number"
    if item is not None:
        if item.min_value is not None and element < float(item.min_value):
            return f"Must be at least {item.min_value}"
        if item.max_value is not None and element > float(item.max_value):
            return f"Must be at most {item.max_value}"
    return None


def _check_text(element: Any, item: ProductParameterItem | None) -> str | None:
    if not isinstance(element, str):
        return "Must be text"
    if item is not None:
        if item.min_length is not None and len(element) < item.min_length:
            return f"Must be at least {item.min_length} characters"
        if item.max_length is not None and len(element) > item.max_length:
            return f"Must be at most {item.max_length} characters"
    return None
