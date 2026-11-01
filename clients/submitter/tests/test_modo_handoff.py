# SPDX-License-Identifier: AGPL-3.0-or-later
"""The Modo handoff kit (index.cfg and one Python command), against fake lx, lxu and modo."""

from __future__ import annotations

import ast
import io
import sys
import xml.etree.ElementTree as ET
from importlib.machinery import ModuleSpec
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest

from sqi_submitter import install
from sqi_submitter.core.handoff import load_context
from tests.conftest import (
    FakeHost,
    assert_no_sqi_submitter_import,
    load_host_file,
    stub_launch,
)

KIT_DIR = Path(__file__).resolve().parents[1] / "src" / "sqi_submitter" / "hosts" / "modo" / "sqi"
MODULE = KIT_DIR / "lxserv" / "sqi_modo_submit.py"
INDEX_CFG = KIT_DIR / "index.cfg"

UNSAVED = "Save your scene before submitting — the farm renders the file on disk."
SAVE_FAILED = "Modo could not save the scene, so nothing was submitted."


class Scene:
    """What ``lxu.select.SceneSelection().current()`` returns."""

    def __init__(self, path: str) -> None:
        self.path = path
        self.changes = 0

    def Filename(self) -> str:
        if not self.path:
            raise RuntimeError("scene has no file name")  # an unnamed scene is a failure code
        return self.path

    def HasChanged(self) -> int:
        return self.changes


class Lx:
    """The fake ``lx``/``lxu``/``modo`` modules plus a record of what the script did with them."""

    def __init__(self, fake_host_module: FakeHost) -> None:
        self.events: list[str] = []
        self.commands: list[str] = []
        self.blessed: list[tuple[type, str]] = []
        self.save_error: Exception | None = None
        self.items = ["mesh", "polyRender", "light"]
        self.channels = {"polyRender$first": 1001, "polyRender$last": 1010}
        self.current_item = -1
        self.selected: str | None = None
        self.scene = Scene("/p/shot.lxo")
        self.groups = [SimpleNamespace(name="Beauty"), SimpleNamespace(name="Fog")]
        # A plain (non-render) group: only reachable through items(), which sqi must not use.
        self.plain_groups = [SimpleNamespace(name="Lights")]
        outer = self

        def evaluate(command: str) -> Any:
            outer.commands.append(command)
            if command == "query sceneservice item.N ?":
                return len(outer.items)
            if command.startswith("query sceneservice item.type ? "):
                outer.current_item = int(command.rsplit(" ", 1)[1])
                return outer.items[outer.current_item]
            if command == "query sceneservice item.id ?":
                return f"item{outer.current_item}"
            if command.startswith("select.item "):
                outer.selected = command.split(" ", 1)[1]
                return None
            if command.startswith("item.channel ") and command.endswith(" ?"):
                assert outer.selected == "item1"  # the render item, not the first item
                return outer.channels[command.split(" ")[1]]
            if command == "scene.save":
                outer.events.append("save")
                if outer.save_error is not None:
                    raise outer.save_error
                return None
            return None

        lx = fake_host_module(
            "lx", eval=evaluate, bless=lambda cls, name: self.blessed.append((cls, name))
        )
        lx.__spec__ = ModuleSpec("lx", None)  # importlib.util.find_spec needs one on a fake

        class BasicCommand:
            pass

        fake_host_module("lxu")
        fake_host_module("lxu.command", BasicCommand=BasicCommand)
        fake_host_module(
            "lxu.select",
            SceneSelection=lambda: SimpleNamespace(current=lambda: outer.scene),
        )
        self.basic_command = BasicCommand
        modo_scene = SimpleNamespace(
            renderPassGroups=self.groups,
            items=lambda *a, **k: self.plain_groups + self.groups,
        )
        fake_host_module("modo", Scene=lambda: modo_scene)

    def dialogs(self) -> list[str]:
        """The text of every ``dialog.msg`` the script sent."""
        return [c[len("dialog.msg {") : -1] for c in self.commands if c.startswith("dialog.msg {")]


@pytest.fixture
def lx(fake_host_module: FakeHost, monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> Lx:
    # Tests that write a context file must never touch the real temp folder.
    monkeypatch.setattr("tempfile.gettempdir", lambda: str(tmp_path))
    return Lx(fake_host_module)


def _module() -> Any:
    return load_host_file(MODULE)


def test_modo_read_scene_pass_groups(lx: Lx) -> None:
    """Targets come from ``scene.renderPassGroups``, never from ``scene.items()``.

    sqi does no filtering of its own: leaving non-render groups out is delegated to Modo's
    ``renderPassGroups`` and is unverified on a real install. The fake's ``items()`` includes a
    plain group, so reading through it instead would put "Lights" in the targets.
    """
    data = _module().read_scene()
    assert [t["name"] for t in data["targets"]] == ["Beauty", "Fog"]
    for target in data["targets"]:
        assert target["kind"] == "pass_group"
        assert target["output_path"] is None and target["locked"] == []
        assert target["frame_range"] == "1001-1010"
    assert [t["extra"] for t in data["targets"]] == [{"PassGroup": "Beauty"}, {"PassGroup": "Fog"}]
    assert data["host"]["name"] == "modo" and data["host"]["display_name"] == "Modo"
    assert data["scene"]["path"] == "/p/shot.lxo"
    assert data["version"] == 1 and data["server_url"] is None


def test_modo_frame_range_from_render_item(lx: Lx) -> None:
    assert _module().read_scene()["scene"]["frame_range"] == "1001-1010"
    lx.channels = {"polyRender$first": 5, "polyRender$last": 5}
    assert _module().read_scene()["scene"]["frame_range"] == "5"


def test_modo_frame_range_without_a_render_item(lx: Lx) -> None:
    lx.items = ["mesh"]
    data = _module().read_scene()
    assert data["scene"]["frame_range"] is None
    assert [t["frame_range"] for t in data["targets"]] == [None, None]


def test_modo_unsaved_scene_path_is_none(lx: Lx) -> None:
    lx.scene.path = ""
    assert _module().read_scene()["scene"]["path"] is None


def test_modo_group_name_with_quote(lx: Lx) -> None:
    lx.groups[0].name = 'Beauty "A"'
    module = _module()
    adapter = load_context(module.write_context(module.read_scene()))
    target = adapter.render_targets()[0]
    assert target.name == 'Beauty "A"'
    assert target.extra == {"PassGroup": 'Beauty "A"'}


def test_modo_unsaved_scene_refused(lx: Lx, monkeypatch: pytest.MonkeyPatch) -> None:
    module = _module()
    lx.scene.path = ""
    lx.scene.changes = 3
    stub_launch(module, monkeypatch, lx.events)
    assert module.submit() is False
    assert lx.events == []  # not saved, not written, not launched
    assert lx.dialogs() == [UNSAVED]
    assert "dialog.open" in lx.commands


def test_modo_changed_scene_saved_before_launch(lx: Lx, monkeypatch: pytest.MonkeyPatch) -> None:
    module = _module()
    lx.scene.changes = 2
    stub_launch(module, monkeypatch, lx.events)
    assert module.submit() is True
    assert lx.events == ["save", "write:2", "launch:sqi:ctx.json"]
    assert lx.dialogs() == []


def test_modo_unchanged_scene_is_not_saved(lx: Lx, monkeypatch: pytest.MonkeyPatch) -> None:
    module = _module()
    stub_launch(module, monkeypatch, lx.events)
    assert module.submit() is True
    assert lx.events == ["write:2", "launch:sqi:ctx.json"]


@pytest.mark.parametrize("error", [AttributeError("renderPassGroups"), ValueError("no channel")])
def test_modo_scene_read_failure_is_shown_not_raised(
    lx: Lx, monkeypatch: pytest.MonkeyPatch, error: Exception
) -> None:
    """``read_scene`` calls Modo APIs unverified on a real install: any failure is shown."""
    module = _module()
    stub_launch(module, monkeypatch, lx.events)  # records write/launch if they are reached

    def broken() -> dict[str, object]:
        raise error

    monkeypatch.setattr(module, "read_scene", broken)
    assert module.submit() is False
    assert lx.events == []  # nothing written, nothing launched
    assert lx.dialogs() == [f"sqi could not read the scene: {error}"]


def test_modo_save_failure_submits_nothing(lx: Lx, monkeypatch: pytest.MonkeyPatch) -> None:
    module = _module()
    lx.scene.changes = 1
    lx.save_error = RuntimeError("disk full")
    stub_launch(module, monkeypatch, lx.events)
    assert module.submit() is False
    assert lx.events == ["save"]
    assert lx.dialogs() == [SAVE_FAILED]


@pytest.mark.parametrize("error", [RuntimeError("sqi-submit was not found."), OSError("denied")])
def test_modo_launch_problems_are_shown_not_raised(
    lx: Lx, monkeypatch: pytest.MonkeyPatch, error: Exception
) -> None:
    module = _module()
    monkeypatch.setattr(module, "resolve_submit_command", lambda _c: "sqi")
    monkeypatch.setattr(module, "write_context", lambda _d: "ctx.json")

    def fail(*_args: object) -> None:
        raise error

    monkeypatch.setattr(module, "launch", fail)
    assert module.submit() is False
    assert lx.dialogs() == [str(error)]


def test_modo_message_braces_cannot_break_the_command(lx: Lx) -> None:
    """Modo quotes a command argument in braces, so a brace in a message must not close it."""
    lx.scene.path = ""
    module = _module()
    module._alert("path {C:\\x} failed")
    assert lx.dialogs() == ["path (C:\\x) failed"]


def test_modo_message_falls_back_to_print_when_dialogs_fail(
    lx: Lx, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    module = _module()
    monkeypatch.setattr(sys.modules["lx"], "eval", lambda _c: (_ for _ in ()).throw(OSError("x")))
    module._alert(UNSAVED)
    out = capsys.readouterr().out
    assert out.startswith("sqi: Save your scene before submitting")
    out.encode("ascii")  # printed from an except handler: safe on a narrow console


def test_modo_register_blesses_command(lx: Lx, monkeypatch: pytest.MonkeyPatch) -> None:
    module = _module()
    lx.blessed.clear()  # loading the module with lx present already registered once
    calls: list[str] = []

    def submit() -> bool:
        calls.append("submit")
        return True

    monkeypatch.setattr(module, "submit", submit)
    module.register()
    assert [name for _cls, name in lx.blessed] == ["sqi.submit"]
    command_class: Any = lx.blessed[0][0]
    assert issubclass(command_class, lx.basic_command)
    command_class().basic_Execute(None, 0)
    assert calls == ["submit"]


def test_modo_import_with_lx_registers_once(lx: Lx) -> None:
    _module()
    assert [name for _cls, name in lx.blessed] == ["sqi.submit"]


def test_modo_import_without_lx_registers_nothing(lx: Lx, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setitem(sys.modules, "lx", None)  # any `import lx` now raises
    module = _module()
    assert lx.blessed == []
    assert callable(module.read_scene) and callable(module.submit) and callable(module.register)


def test_modo_index_cfg_names_the_kit() -> None:
    root = ET.parse(INDEX_CFG).getroot()
    assert root.tag == "configuration"
    assert root.attrib["kit"] == "sqi"
    assert [e.text for e in root.iter("import")] == ["kit_sqi:lxserv"]
    assert "SPDX-License-Identifier: AGPL-3.0-or-later" in INDEX_CFG.read_text(encoding="utf-8")


def test_modo_module_parses_as_python37() -> None:
    ast.parse(MODULE.read_text(encoding="utf-8"), feature_version=(3, 7))


def test_modo_files_have_no_sqi_submitter_import() -> None:
    assert_no_sqi_submitter_import(MODULE)


@pytest.mark.parametrize(
    ("platform", "env", "expected"),
    [
        ("win32", {"APPDATA": "C:/Users/a/AppData/Roaming"}, "C:/Users/a/AppData/Roaming"),
        ("darwin", {"HOME": "/Users/a"}, "/Users/a/Library/Application Support"),
        ("linux", {"HOME": "/home/a"}, "/home/a/.luxology"),
    ],
)
def test_default_destination_modo(platform: str, env: dict[str, str], expected: str) -> None:
    folder = "Luxology/Scripts" if platform != "linux" else "Scripts"
    assert install.default_destination("modo", platform, env) == Path(expected) / folder


def test_default_destination_modo_needs_the_user_folder() -> None:
    assert install.default_destination("modo", "win32", {}) is None
    assert install.default_destination("modo", "darwin", {"HOME": ""}) is None
    assert install.default_destination("modo", "linux", {}) is None


def test_modo_next_step_and_no_destination_hint(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(install, "default_destination", lambda host, platform, env: None)
    out = io.StringIO()
    assert install.install_host("modo", None, out) == 0
    text = out.getvalue()
    assert "index.cfg" in text and "sqi_modo_submit.py" in text
    assert "Restart Modo; run sqi.submit from the command entry, or map it to a key." in text


def test_install_modo_copies_the_kit_tree(tmp_path: Path) -> None:
    out = io.StringIO()
    assert install.install_host("modo", tmp_path, out) == 0
    assert (tmp_path / "sqi" / "index.cfg").is_file()
    assert (tmp_path / "sqi" / "lxserv" / "sqi_modo_submit.py").is_file()
    assert sorted(p.name for p in tmp_path.iterdir()) == ["sqi"]
