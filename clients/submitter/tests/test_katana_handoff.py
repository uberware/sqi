# SPDX-License-Identifier: AGPL-3.0-or-later
"""The Katana handoff module and its startup hook, against fake Katana modules."""

from __future__ import annotations

import ast
import sys
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest

from sqi_submitter import install
from sqi_submitter.core.handoff import load_context
from tests.conftest import FakeHost, assert_no_sqi_submitter_import, load_host_file

KATANA_DIR = Path(__file__).resolve().parents[1] / "src" / "sqi_submitter" / "hosts" / "katana"
MODULE = KATANA_DIR / "sqi_katana_submit.py"
HOOK = KATANA_DIR / "init.py"

UNSAVED = "Save your scene before submitting — the farm renders the file on disk."
SAVE_FAILED = "Katana could not save the project, so nothing was submitted."


def _ascii(text: str) -> str:
    """What the console fallback prints: ASCII only, so a narrow console cannot raise."""
    return text.encode("ascii", "replace").decode("ascii")


class Param:
    def __init__(self, value: object) -> None:
        self.value = value

    def getValue(self, _frame: float) -> object:
        return self.value


class Node:
    """A Render node: parameters are looked up by their dotted path."""

    def __init__(
        self,
        name: str,
        *,
        bypassed: bool = False,
        active: tuple[int, int] | None = None,
        group: str = "farmGlobalSettings",
        flag: object = "Yes",
    ) -> None:
        self.name = name
        self.bypassed = bypassed
        self.params: dict[str, Param] = {}
        if active is not None:
            self.params[group + ".setActiveFrameRange"] = Param(flag)
            self.params[group + ".activeFrameRange.start"] = Param(active[0])
            self.params[group + ".activeFrameRange.end"] = Param(active[1])

    def getName(self) -> str:
        return self.name

    def isBypassed(self) -> bool:
        return self.bypassed

    def getParameter(self, path: str) -> Param | None:
        return self.params.get(path)


class Katana:
    """The fake ``Katana`` package plus the calls the module makes on it."""

    def __init__(self, fake_host_module: FakeHost, monkeypatch: pytest.MonkeyPatch) -> None:
        self.nodes: list[Node] = []
        self.file_name = "D:/shots/s010.katana"
        self.dirty = False
        self.save_error: Exception | None = None
        self.events: list[str] = []
        self.warnings: list[str] = []
        self.menu_bar: Any = None
        monkeypatch.delenv("KATANA_RELEASE", raising=False)
        monkeypatch.setitem(sys.modules, "KatanaInfo", SimpleNamespace(version="7.5.2"))

        def save(path: str) -> None:
            self.events.append("save:" + path)
            if self.save_error is not None:
                raise self.save_error

        def nodes_by_type(kind: str) -> list[Node]:
            assert kind == "Render"
            return self.nodes

        self.module = fake_host_module(
            "Katana",
            FarmAPI=SimpleNamespace(GetKatanaFileName=lambda: self.file_name),
            NodegraphAPI=SimpleNamespace(
                GetAllNodesByType=nodes_by_type, GetInTime=lambda: 1, GetOutTime=lambda: 100
            ),
            KatanaFile=SimpleNamespace(IsFileDirty=lambda: self.dirty, Save=save),
        )

        # Qt is chosen by Katana's major version; the warning box is recorded, not shown.
        warning = SimpleNamespace(warning=lambda parent, title, text: self.warnings.append(text))
        widgets = SimpleNamespace(QMessageBox=warning)
        fake_host_module("PyQt5", QtWidgets=widgets)
        fake_host_module("PySide6", QtWidgets=widgets)

        class MainMenu:
            pass

        owner = self

        class Window:
            def findChild(self, kind: object) -> Any:
                assert kind is MainMenu
                return owner.menu_bar

        fake_host_module(
            "UI4.App",
            MainMenu=SimpleNamespace(MainMenu=MainMenu),
            MainWindow=SimpleNamespace(CurrentMainWindow=Window),
        )


class Signal:
    def __init__(self) -> None:
        self.slots: list[Any] = []

    def connect(self, slot: Any) -> None:
        self.slots.append(slot)


class Action:
    def __init__(self, text: str) -> None:
        self.text = text
        self.triggered = Signal()


class Menu:
    def __init__(self, title: str) -> None:
        self.title = title
        self.actions: list[Action] = []

    def addAction(self, text: str) -> Action:
        action = Action(text)
        self.actions.append(action)
        return action


class MenuBar:
    def __init__(self) -> None:
        self.menus: list[Menu] = []

    def addMenu(self, title: str) -> Menu:
        menu = Menu(title)
        self.menus.append(menu)
        return menu


@pytest.fixture
def katana(fake_host_module: FakeHost, monkeypatch: pytest.MonkeyPatch) -> Katana:
    return Katana(fake_host_module, monkeypatch)


def _module() -> Any:
    return load_host_file(MODULE)


def _stub_launch(module: Any, monkeypatch: pytest.MonkeyPatch, events: list[str]) -> None:
    monkeypatch.setattr(module, "resolve_submit_command", lambda _c: "sqi")

    def write(data: dict[str, Any]) -> str:
        events.append(f"write:{len(data['targets'])}")
        return "ctx.json"

    monkeypatch.setattr(module, "write_context", write)
    monkeypatch.setattr(module, "launch", lambda cmd, ctx: events.append(f"launch:{cmd}:{ctx}"))


@pytest.mark.parametrize("group", ["farmGlobalSettings", "farmSettings"])
def test_katana_read_scene_render_nodes(
    katana: Katana, monkeypatch: pytest.MonkeyPatch, tmp_path: Path, group: str
) -> None:
    monkeypatch.setattr("tempfile.gettempdir", lambda: str(tmp_path))
    monkeypatch.setenv("KATANA_RELEASE", "7.0v2")
    katana.nodes = [
        Node("Beauty", active=(10, 20), group=group),
        Node("Utility"),
        Node("Skipped", bypassed=True, active=(5, 6), group=group),
    ]
    module = _module()
    data = module.read_scene()
    assert [(t["name"], t["frame_range"]) for t in data["targets"]] == [
        ("Beauty", "10-20"),
        ("Utility", "1-100"),
    ]
    for target in data["targets"]:
        assert target["kind"] == "render_node"
        assert target["output_path"] is None
        assert target["locked"] == []
        assert target["extra"] == {"RenderNode": target["name"]}
    assert data["host"] == {"name": "katana", "display_name": "Katana", "version": "7.0v2"}
    assert data["scene"] == {
        "path": "D:/shots/s010.katana",
        "frame_range": "1-100",
        "output_path": None,
        "renderer": None,
    }
    assert data["version"] == 1 and data["server_url"] is None
    adapter = load_context(module.write_context(data))
    assert adapter.host_name == "katana"
    assert [t.name for t in adapter.render_targets()] == ["Beauty", "Utility"]


def test_katana_node_name_with_space(
    katana: Katana, monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    monkeypatch.setattr("tempfile.gettempdir", lambda: str(tmp_path))
    katana.nodes = [Node("Beauty Pass")]
    module = _module()
    adapter = load_context(module.write_context(module.read_scene()))
    [target] = adapter.render_targets()
    assert target.name == "Beauty Pass"
    assert target.extra == {"RenderNode": "Beauty Pass"}


def test_katana_unsaved_project_has_no_scene_path(katana: Katana) -> None:
    katana.file_name = ""
    assert _module().read_scene()["scene"]["path"] is None


@pytest.mark.parametrize(
    ("node_kwargs", "expected"),
    [
        ({}, "3-9"),  # no farm parameters at all: the project's range
        ({"active": (10, 20)}, "10-20"),
        ({"active": (20, 10)}, "10-20"),  # ends are put low to high
        ({"active": (7, 7)}, "7"),
        ({"active": (10, 20), "flag": "No"}, "3-9"),  # the toggle is off: its values are ignored
        ({"active": (10, 20), "flag": 1}, "10-20"),
        ({"active": (10, 20), "flag": 0}, "3-9"),
    ],
)
def test_katana_node_frame_range(node_kwargs: dict[str, Any], expected: str) -> None:
    assert _module().node_frame_range(Node("N", **node_kwargs), 3, 9) == expected


def test_katana_version_falls_back_to_katanainfo(
    katana: Katana, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setitem(sys.modules, "KatanaInfo", SimpleNamespace(version="8.0.1"))
    assert _module().read_scene()["host"]["version"] == "8.0.1"


def test_katana_version_is_none_when_nothing_reports_it(
    katana: Katana, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setitem(sys.modules, "KatanaInfo", None)
    assert _module().read_scene()["host"]["version"] is None


def test_katana_unsaved_project_refused(katana: Katana, monkeypatch: pytest.MonkeyPatch) -> None:
    module = _module()
    katana.file_name = ""
    katana.dirty = True
    _stub_launch(module, monkeypatch, katana.events)
    assert module.submit() is False
    assert katana.events == []  # not saved, not written, not launched
    assert katana.warnings == [UNSAVED]


def test_katana_dirty_project_saved_before_launch(
    katana: Katana, monkeypatch: pytest.MonkeyPatch
) -> None:
    module = _module()
    katana.nodes = [Node("Beauty")]
    katana.dirty = True
    _stub_launch(module, monkeypatch, katana.events)
    assert module.submit() is True
    assert katana.events == ["save:D:/shots/s010.katana", "write:1", "launch:sqi:ctx.json"]
    assert katana.warnings == []


def test_katana_clean_project_is_not_saved(katana: Katana, monkeypatch: pytest.MonkeyPatch) -> None:
    module = _module()
    katana.nodes = [Node("Beauty")]
    _stub_launch(module, monkeypatch, katana.events)
    assert module.submit() is True
    assert katana.events == ["write:1", "launch:sqi:ctx.json"]


def test_katana_save_failure_submits_nothing(
    katana: Katana, monkeypatch: pytest.MonkeyPatch
) -> None:
    module = _module()
    katana.dirty = True
    katana.save_error = RuntimeError("disk full")
    _stub_launch(module, monkeypatch, katana.events)
    assert module.submit() is False
    assert katana.events == ["save:D:/shots/s010.katana"]
    assert katana.warnings == [SAVE_FAILED]


@pytest.mark.parametrize("error", [AttributeError("GetKatanaFileName"), ValueError("x")])
def test_katana_scene_check_failure_is_shown_not_raised(
    katana: Katana, monkeypatch: pytest.MonkeyPatch, error: Exception
) -> None:
    """``GetKatanaFileName`` is an unverified Katana API: whatever it raises is shown."""
    module = _module()
    _stub_launch(module, monkeypatch, katana.events)

    def broken() -> str:
        raise error

    katana.module.FarmAPI.GetKatanaFileName = broken
    assert module.submit() is False
    assert katana.events == []  # nothing written, nothing launched
    assert katana.warnings == [f"sqi could not read the scene: {error}"]


@pytest.mark.parametrize("error", [AttributeError("GetAllNodesByType"), ValueError("x")])
def test_katana_scene_read_failure_is_shown_not_raised(
    katana: Katana, monkeypatch: pytest.MonkeyPatch, error: Exception
) -> None:
    module = _module()
    _stub_launch(module, monkeypatch, katana.events)

    def broken() -> object:
        raise error

    monkeypatch.setattr(module, "read_scene", broken)
    assert module.submit() is False
    assert katana.events == []
    assert katana.warnings == [f"sqi could not read the scene: {error}"]


@pytest.mark.parametrize("error", [RuntimeError("sqi-submit was not found."), OSError("denied")])
def test_katana_launch_problems_are_shown_not_raised(
    katana: Katana, monkeypatch: pytest.MonkeyPatch, error: Exception
) -> None:
    module = _module()
    monkeypatch.setattr(module, "resolve_submit_command", lambda _c: "sqi")
    monkeypatch.setattr(module, "write_context", lambda _d: "ctx.json")

    def fail(*_args: object) -> None:
        raise error

    monkeypatch.setattr(module, "launch", fail)
    assert module.submit() is False
    assert katana.warnings == [str(error)]


@pytest.mark.parametrize(("version", "binding"), [("7.5.2", "PyQt5"), ("8.0.1", "PySide6")])
def test_katana_messages_use_the_binding_for_the_katana_version(
    katana: Katana, monkeypatch: pytest.MonkeyPatch, version: str, binding: str
) -> None:
    """Katana 8 moved to PySide6; the guide says to choose by version, never by try/except."""
    monkeypatch.setitem(sys.modules, "KatanaInfo", SimpleNamespace(version=version))
    shown: list[str] = []
    other = "PySide6" if binding == "PyQt5" else "PyQt5"
    monkeypatch.setattr(
        sys.modules[binding].QtWidgets.QMessageBox,
        "warning",
        lambda parent, title, text: shown.append(binding + ":" + title + ":" + text),
    )
    monkeypatch.setitem(sys.modules, other, None)  # importing the wrong binding would raise
    katana.file_name = ""
    assert _module().submit() is False
    assert shown == [f"{binding}:sqi:{UNSAVED}"]


def test_katana_message_without_qt_is_printed(
    katana: Katana, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    monkeypatch.setitem(sys.modules, "PyQt5", None)
    monkeypatch.setitem(sys.modules, "PySide6", None)
    katana.file_name = ""
    assert _module().submit() is False
    assert _ascii(UNSAVED) in capsys.readouterr().out


def test_katana_install_menu_adds_the_sqi_menu(
    katana: Katana, monkeypatch: pytest.MonkeyPatch
) -> None:
    module = _module()
    katana.menu_bar = MenuBar()
    calls: list[str] = []

    def submit() -> bool:
        calls.append("submit")
        return True

    monkeypatch.setattr(module, "submit", submit)
    module.install_menu(objectHash="ignored")  # Katana passes keyword arguments to callbacks
    [menu] = katana.menu_bar.menus
    assert menu.title == "sqi"
    [action] = menu.actions
    assert action.text == "Submit to sqi…"
    [slot] = action.triggered.slots
    slot(False)  # Qt's `triggered(checked)`
    assert calls == ["submit"]


def test_katana_install_menu_without_a_menu_bar_prints(
    katana: Katana, capsys: pytest.CaptureFixture[str]
) -> None:
    _module().install_menu()
    assert "menu bar" in capsys.readouterr().out


def _callbacks() -> tuple[SimpleNamespace, list[tuple[object, object]]]:
    registered: list[tuple[object, object]] = []
    callbacks = SimpleNamespace(
        Type=SimpleNamespace(onStartupComplete="onStartupComplete"),
        addCallback=lambda kind, func: registered.append((kind, func)),
    )
    return callbacks, registered


def test_katana_hook_registers_startup_callback(
    fake_host_module: FakeHost, monkeypatch: pytest.MonkeyPatch
) -> None:
    callbacks, registered = _callbacks()
    fake_host_module("Katana", Callbacks=callbacks)

    def install_menu(**_kwargs: object) -> None:
        return None

    stub = SimpleNamespace(install_menu=install_menu)
    monkeypatch.setitem(sys.modules, "sqi_katana_submit", stub)
    monkeypatch.setattr(sys, "path", list(sys.path))
    load_host_file(HOOK)
    assert registered == [("onStartupComplete", install_menu)]
    assert install.HOOK_MARKER in HOOK.read_text(encoding="utf-8").splitlines()
    assert str(KATANA_DIR) in sys.path  # its own folder, so the module beside it imports


def test_katana_hook_without_dunder_file_uses_the_user_startup_folder(
    fake_host_module: FakeHost, monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """Katana runs init.py with ``execfile``, which may leave ``__file__`` undefined."""
    callbacks, registered = _callbacks()
    fake_host_module("Katana", Callbacks=callbacks)
    stub = SimpleNamespace(install_menu=lambda **_kw: None)
    monkeypatch.setitem(sys.modules, "sqi_katana_submit", stub)
    monkeypatch.setattr(sys, "path", list(sys.path))
    monkeypatch.setenv("HOME", str(tmp_path))
    monkeypatch.setenv("USERPROFILE", str(tmp_path))
    namespace: dict[str, Any] = {"__name__": "init"}
    exec(compile(HOOK.read_text(encoding="utf-8"), "init.py", "exec"), namespace)
    assert str(tmp_path / ".katana" / "Startup") in sys.path
    assert [kind for kind, _func in registered] == ["onStartupComplete"]


def test_katana_hook_failure_does_not_break_startup(
    fake_host_module: FakeHost, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    """A broken sqi install must not stop the studio's own startup code after the hook."""
    fake_host_module("Katana", Callbacks=_callbacks()[0])
    monkeypatch.setitem(sys.modules, "sqi_katana_submit", None)  # the import raises
    monkeypatch.setattr(sys, "path", list(sys.path))
    namespace: dict[str, Any] = {"__name__": "init"}
    exec(compile(HOOK.read_text(encoding="utf-8"), "init.py", "exec"), namespace)
    assert "was not added" in capsys.readouterr().out


def test_katana_hook_is_valid_to_append_to_a_studio_file() -> None:
    """The hook's lines land after a studio's own file, with no ``__future__`` import."""
    text = HOOK.read_text(encoding="utf-8")
    lines = [
        line
        for line in text.splitlines()
        if line.strip() != install.HOOK_MARKER and not line.startswith("from __future__")
    ]
    studio = "import os\n\nSTUDIO = os.environ.get('STUDIO')\n"
    compile(studio + "\n".join(lines) + "\n", "init.py", "exec")
    for node in ast.walk(ast.parse("\n".join(lines))):
        assert not isinstance(node, ast.AnnAssign)


def test_katana_module_imports_without_katana(monkeypatch: pytest.MonkeyPatch) -> None:
    for name in ("Katana", "KatanaInfo", "UI4", "UI4.App"):
        monkeypatch.setitem(sys.modules, name, None)  # any `import` of these now raises
    module = _module()
    assert callable(module.read_scene) and callable(module.submit)
    assert callable(module.install_menu) and callable(module.node_frame_range)


def test_katana_files_have_no_sqi_submitter_import() -> None:
    assert_no_sqi_submitter_import(MODULE)
    assert_no_sqi_submitter_import(HOOK)
