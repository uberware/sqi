# SPDX-License-Identifier: AGPL-3.0-or-later
"""Shared fixtures: fake DCC host-module injection."""

from __future__ import annotations

import sys
import types
from collections.abc import Callable, Iterator

import pytest


@pytest.fixture
def fake_host_module(monkeypatch: pytest.MonkeyPatch) -> Callable[..., types.ModuleType]:
    """Install a fake module (e.g. ``hou``, ``nuke``, ``bpy``, ``maya.cmds``).

    Returns the created module so tests can attach attributes. Dotted names
    install every parent package and wire the child as an attribute.
    """

    def install(name: str, **attrs: object) -> types.ModuleType:
        module = types.ModuleType(name)
        for key, value in attrs.items():
            setattr(module, key, value)
        parts = name.split(".")
        for i in range(1, len(parts)):
            parent_name = ".".join(parts[:i])
            parent = sys.modules.get(parent_name) or types.ModuleType(parent_name)
            monkeypatch.setitem(sys.modules, parent_name, parent)
        monkeypatch.setitem(sys.modules, name, module)
        if len(parts) > 1:
            setattr(sys.modules[".".join(parts[:-1])], parts[-1], module)
        return module

    return install


@pytest.fixture
def _clean_settings(monkeypatch: pytest.MonkeyPatch, tmp_path: object) -> Iterator[None]:
    monkeypatch.setenv("SQI_SUBMITTER_SETTINGS", str(tmp_path) + "/settings.json")
    monkeypatch.delenv("SQI_SERVER_URL", raising=False)
    yield
