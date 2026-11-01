# SPDX-License-Identifier: AGPL-3.0-or-later
"""Shared fixtures: fake DCC host-module injection."""

from __future__ import annotations

import ast
import importlib.util
import sys
import types
from collections.abc import Callable, Iterator
from importlib.machinery import SourceFileLoader
from pathlib import Path
from typing import Any

import pytest

import sqi_submitter.hosts

FakeHost = Callable[..., types.ModuleType]
"""The ``fake_host_module`` fixture: ``install(name, **attrs)`` returns the fake module."""

HOSTS_DIR = Path(sqi_submitter.hosts.__file__).parent
HANDOFF_BEGIN = "# --- sqi handoff begin ---"


def handoff_host_files() -> list[Path]:
    """Every host file carrying the shared launch block, found by its begin marker.

    Found rather than listed, so a new handoff host cannot be left out of the checks on it.
    """
    return sorted(
        path
        for path in HOSTS_DIR.rglob("*")
        if path.suffix in (".py", ".pyp")
        and path.name != "_handoff_block.py"
        and HANDOFF_BEGIN in path.read_text(encoding="utf-8")
    )


def assert_no_sqi_submitter_import(path: Path) -> None:
    """A host file runs in the host's own Python, where ``sqi_submitter`` is not installed."""
    for node in ast.walk(ast.parse(path.read_text(encoding="utf-8"))):
        if isinstance(node, ast.Import):
            assert not [a for a in node.names if a.name.split(".")[0] == "sqi_submitter"]
        if isinstance(node, ast.ImportFrom):
            assert (node.module or "").split(".")[0] != "sqi_submitter"


def ascii_fallback(text: str) -> str:
    """What a handoff host prints on a narrow console: ASCII only, so it cannot raise."""
    return text.encode("ascii", "replace").decode("ascii")


def stub_launch(module: Any, monkeypatch: pytest.MonkeyPatch, events: list[str]) -> None:
    """Replace a handoff host's command lookup, context write and launch with recorders."""
    monkeypatch.setattr(module, "resolve_submit_command", lambda _c: "sqi")

    def write(data: dict[str, Any]) -> str:
        events.append(f"write:{len(data['targets'])}")
        return "ctx.json"

    monkeypatch.setattr(module, "write_context", write)
    monkeypatch.setattr(module, "launch", lambda cmd, ctx: events.append(f"launch:{cmd}:{ctx}"))


def no_qt(monkeypatch: pytest.MonkeyPatch) -> None:
    """Make the run look Qt-less: no binding, and importing the dialog fails."""
    import sqi_submitter.qt as qt_package
    from sqi_submitter.qt import _compat

    monkeypatch.setattr(_compat, "QT_BINDING", "")
    # Importing the dialog without a binding fails at import time: it must not be reached.
    monkeypatch.setitem(sys.modules, "sqi_submitter.qt.dialog", None)
    monkeypatch.delattr(qt_package, "dialog", raising=False)


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
def fake_host_module(monkeypatch: pytest.MonkeyPatch) -> FakeHost:
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
