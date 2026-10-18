# SPDX-License-Identifier: AGPL-3.0-or-later
"""Shared fixtures: fake DCC host-module injection."""

from __future__ import annotations

import importlib.util
import sys
import types
from collections.abc import Callable, Iterator
from importlib.machinery import SourceFileLoader
from pathlib import Path

import pytest


def load_host_file(path: Path) -> types.ModuleType:
    """Load a host file that is not importable by name (a Cinema 4D ``.pyp``).

    The module is not added to ``sys.modules``, so each call returns a fresh copy and
    ``__name__`` is never ``"__main__"`` (the plugin's registration stays off).
    """
    name = "host_" + path.stem
    spec = importlib.util.spec_from_file_location(
        name, path, loader=SourceFileLoader(name, str(path))
    )
    assert spec is not None and spec.loader is not None, f"cannot load {path}"
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


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


@pytest.fixture(autouse=True)
def native_messages(monkeypatch: pytest.MonkeyPatch) -> list[str]:
    """Record failed-launch message boxes instead of showing them: a real one blocks the run.

    Tests of the real ``show_message`` restore it from a reference taken at import time.
    """
    from sqi_submitter import _failure

    shown: list[str] = []
    monkeypatch.setattr(_failure, "show_message", shown.append)
    return shown


@pytest.fixture
def _clean_settings(monkeypatch: pytest.MonkeyPatch, tmp_path: object) -> Iterator[None]:
    monkeypatch.setenv("SQI_SUBMITTER_SETTINGS", str(tmp_path) + "/settings.json")
    monkeypatch.delenv("SQI_SERVER_URL", raising=False)
    yield
