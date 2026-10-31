# SPDX-License-Identifier: AGPL-3.0-or-later
"""The Unreal Editor handoff module and its startup hook, against a fake ``unreal`` module."""

from __future__ import annotations

import sys
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest

from sqi_submitter import install
from sqi_submitter.core.handoff import load_context
from tests.conftest import FakeHost, assert_no_sqi_submitter_import, load_host_file

UNREAL_DIR = Path(__file__).resolve().parents[1] / "src" / "sqi_submitter" / "hosts" / "unreal"
MODULE = UNREAL_DIR / "sqi_unreal_submit.py"
HOOK = UNREAL_DIR / "init_unreal.py"

STILL_DIRTY = "Save your changes before submitting — the farm renders the saved project."
REFUSAL_HEAD = "These queue jobs cannot render on the farm and were left out:\n"


class Path_:
    """A ``SoftObjectPath``: ``export_text()`` is its asset path."""

    def __init__(self, text: str) -> None:
        self.text = text

    def export_text(self) -> str:
        return self.text


def _job(
    name: str = "Hero",
    *,
    sequence: str = "/Game/Seq/S1.S1",
    map_path: str = "/Game/Maps/Hero.Hero",
    config: str | None = "/Game/Cfg/Hi.Hi",
    enabled: bool = True,
) -> SimpleNamespace:
    origin = SimpleNamespace(get_path_name=lambda: config) if config is not None else None
    return SimpleNamespace(
        job_name=name,
        sequence=Path_(sequence),
        map=Path_(map_path),
        is_enabled=lambda: enabled,
        get_preset_origin=lambda: origin,
    )


class Unreal:
    """The fake ``unreal`` module plus the calls the module makes on it."""

    def __init__(self, fake_host_module: FakeHost) -> None:
        self.jobs: list[SimpleNamespace] = []
        self.map_dirty: list[object] = []
        self.content_dirty: list[object] = []
        self.save_clears = True
        self.answer = "OK"
        self.events: list[str] = []
        self.messages: list[tuple[str, str, object]] = []
        self.menus: dict[str, Any] = {}
        self.refreshed = 0

        def save(save_maps: bool, save_content: bool) -> bool:
            self.events.append(f"save:{save_maps}:{save_content}")
            if self.save_clears:
                self.map_dirty, self.content_dirty = [], []
            return True

        def show(title: str, text: str, kind: object) -> object:
            self.messages.append((title, text, kind))
            return "OK" if self.answer == "OK" else "CANCEL"

        owner = self

        class Entry:
            def __init__(self, **kwargs: Any) -> None:
                self.kwargs = kwargs
                self.label = ""
                self.command: tuple[object, str, str] | None = None

            def set_label(self, label: str) -> None:
                self.label = label

            def set_string_command(self, kind: object, custom: str, text: str) -> None:
                self.command = (kind, custom, text)

        class Menu:
            def __init__(self) -> None:
                self.entries: list[tuple[str, Entry]] = []

            def add_menu_entry(self, section: str, entry: Entry) -> None:
                self.entries.append((section, entry))

        class ToolMenus:
            @classmethod
            def get(cls) -> ToolMenus:
                return instance

            def find_menu(self, name: str) -> Menu | None:
                return owner.menus.get(name)

            def refresh_all_widgets(self) -> None:
                owner.refreshed += 1

        instance = ToolMenus()
        self.Entry, self.Menu = Entry, Menu
        queue = SimpleNamespace(get_jobs=lambda: self.jobs)
        subsystem = SimpleNamespace(get_queue=lambda: queue)
        self.warnings: list[str] = []
        self.module = fake_host_module(
            "unreal",
            MoviePipelineQueueSubsystem=type("MoviePipelineQueueSubsystem", (), {}),
            get_editor_subsystem=lambda _cls: subsystem,
            log_warning=self.warnings.append,
            Paths=SimpleNamespace(
                get_project_file_path=lambda: "../Proj/Proj.uproject",
                convert_relative_path_to_full=lambda p: (
                    "D:/Proj/Proj.uproject" if p == "../Proj/Proj.uproject" else p
                ),
            ),
            SystemLibrary=SimpleNamespace(get_engine_version=lambda: "5.4.2-12345+++UE5+Rel-5.4"),
            EditorLoadingAndSavingUtils=SimpleNamespace(
                get_dirty_map_packages=lambda: self.map_dirty,
                get_dirty_content_packages=lambda: self.content_dirty,
                save_dirty_packages_with_dialog=save,
            ),
            EditorDialog=SimpleNamespace(show_message=show),
            AppMsgType=SimpleNamespace(OK="MSG_OK", OK_CANCEL="MSG_OK_CANCEL"),
            AppReturnType=SimpleNamespace(OK="OK", CANCEL="CANCEL"),
            ToolMenus=ToolMenus,
            ToolMenuEntry=Entry,
            MultiBlockType=SimpleNamespace(MENU_ENTRY="MENU_ENTRY"),
            ToolMenuStringCommandType=SimpleNamespace(PYTHON="PYTHON"),
        )


@pytest.fixture
def unreal(fake_host_module: FakeHost) -> Unreal:
    return Unreal(fake_host_module)


def _module() -> Any:
    return load_host_file(MODULE)


def _stub_launch(module: Any, monkeypatch: pytest.MonkeyPatch, events: list[str]) -> None:
    monkeypatch.setattr(module, "resolve_submit_command", lambda _c: "sqi")

    def write(data: dict[str, Any]) -> str:
        events.append(f"write:{len(data['targets'])}")
        return "ctx.json"

    monkeypatch.setattr(module, "write_context", write)
    monkeypatch.setattr(module, "launch", lambda cmd, ctx: events.append(f"launch:{cmd}:{ctx}"))


def test_unreal_read_scene_enabled_jobs_only(
    unreal: Unreal, monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    monkeypatch.setattr("tempfile.gettempdir", lambda: str(tmp_path))
    unreal.jobs = [_job("A"), _job("Off", enabled=False), _job("B")]
    module = _module()
    data, reasons = module.read_scene()
    assert reasons == []
    assert [t["name"] for t in data["targets"]] == ["A", "B"]
    for target in data["targets"]:
        assert target["kind"] == "mrq_job"
        assert target["frame_range"] is None and target["output_path"] is None
        assert target["locked"] == []
        assert target["extra"] == {
            "LevelSequence": "/Game/Seq/S1.S1",
            "Map": "/Game/Maps/Hero.Hero",
            "MovieRenderConfig": "/Game/Cfg/Hi.Hi",
        }
    assert data["host"] == {"name": "unreal", "display_name": "Unreal Editor", "version": "5.4.2"}
    assert data["scene"] == {
        "path": "D:/Proj/Proj.uproject",
        "frame_range": None,
        "output_path": None,
        "renderer": None,
    }
    adapter = load_context(module.write_context(data))
    assert adapter.host_name == "unreal"
    assert [t.name for t in adapter.render_targets()] == ["A", "B"]


def test_unreal_job_without_saved_config_is_excluded_and_named(unreal: Unreal) -> None:
    unreal.jobs = [_job("Edited", config=None), _job("Fine")]
    data, reasons = _module().read_scene()
    assert [t["name"] for t in data["targets"]] == ["Fine"]
    [reason] = reasons
    assert "Edited" in reason and "not saved as a config asset" in reason


def test_unreal_job_on_unsaved_map_is_excluded(unreal: Unreal) -> None:
    unreal.jobs = [_job("Scratch", map_path="/Temp/Untitled_1.Untitled_1"), _job("Fine")]
    data, reasons = _module().read_scene()
    assert [t["name"] for t in data["targets"]] == ["Fine"]
    assert reasons == ["Scratch: its map it is not saved"]


def test_unreal_job_on_unsaved_sequence_is_excluded(unreal: Unreal) -> None:
    unreal.jobs = [_job("Seq", sequence="/Engine/Transient.Seq")]
    data, reasons = _module().read_scene()
    assert data["targets"] == []
    assert reasons == ["Seq: its level sequence it is not saved"]


@pytest.mark.parametrize(
    ("path", "problem"),
    [
        ("", "it is not saved"),
        ("/Temp/x", "it is not saved"),
        ("/Engine/Transient.x", "it is not saved"),
        ("Game/x", "it is not an asset path"),
        ("/Game/x", None),
        ("/MyPlugin/x", None),
    ],
)
def test_unreal_asset_path_problem(path: str, problem: str | None) -> None:
    assert _module().asset_path_problem(path) == problem


def test_unreal_dirty_packages_saved_with_dialog_then_launch(
    unreal: Unreal, monkeypatch: pytest.MonkeyPatch
) -> None:
    module = _module()
    unreal.jobs = [_job()]
    unreal.map_dirty = ["map"]
    _stub_launch(module, monkeypatch, unreal.events)
    assert module.submit() is True
    assert unreal.events == ["save:True:True", "write:1", "launch:sqi:ctx.json"]
    assert unreal.messages == []


def test_unreal_clean_project_is_not_saved(unreal: Unreal, monkeypatch: pytest.MonkeyPatch) -> None:
    module = _module()
    unreal.jobs = [_job()]
    _stub_launch(module, monkeypatch, unreal.events)
    assert module.submit() is True
    assert unreal.events == ["write:1", "launch:sqi:ctx.json"]


def test_unreal_still_dirty_refuses(unreal: Unreal, monkeypatch: pytest.MonkeyPatch) -> None:
    module = _module()
    unreal.jobs = [_job()]
    unreal.content_dirty = ["asset"]
    unreal.save_clears = False
    _stub_launch(module, monkeypatch, unreal.events)
    assert module.submit() is False
    assert unreal.events == ["save:True:True"]  # nothing written, nothing launched
    assert unreal.messages == [("sqi", STILL_DIRTY, "MSG_OK")]


def test_unreal_refusals_cancel_stops_launch(
    unreal: Unreal, monkeypatch: pytest.MonkeyPatch
) -> None:
    module = _module()
    unreal.jobs = [_job("Edited", config=None), _job("Fine")]
    unreal.answer = "CANCEL"
    _stub_launch(module, monkeypatch, unreal.events)
    assert module.submit() is False
    assert unreal.events == []
    assert unreal.messages == [
        (
            "sqi",
            REFUSAL_HEAD + "Edited: its render settings are not saved as a config asset",
            "MSG_OK_CANCEL",
        )
    ]


def test_unreal_refusals_ok_launches_the_rest(
    unreal: Unreal, monkeypatch: pytest.MonkeyPatch
) -> None:
    module = _module()
    unreal.jobs = [
        _job("Edited", config=None),
        _job("Scratch", map_path="/Temp/m.m"),
        _job("Fine"),
    ]
    _stub_launch(module, monkeypatch, unreal.events)
    assert module.submit() is True
    assert unreal.events == ["write:1", "launch:sqi:ctx.json"]
    [(_title, text, _kind)] = unreal.messages
    lines = text.split("\n")
    assert lines[0] + "\n" == REFUSAL_HEAD
    assert len(lines) == 3 and lines[1].startswith("Edited: ") and lines[2].startswith("Scratch: ")


@pytest.mark.parametrize("error", [RuntimeError("sqi-submit was not found."), OSError("denied")])
def test_unreal_launch_problems_are_shown_not_raised(
    unreal: Unreal, monkeypatch: pytest.MonkeyPatch, error: Exception
) -> None:
    module = _module()
    unreal.jobs = [_job()]
    monkeypatch.setattr(module, "resolve_submit_command", lambda _c: "sqi")
    monkeypatch.setattr(module, "write_context", lambda _d: "ctx.json")

    def fail(*_args: object) -> None:
        raise error

    monkeypatch.setattr(module, "launch", fail)
    assert module.submit() is False
    assert unreal.messages == [("sqi", str(error), "MSG_OK")]


def test_unreal_menu_entry_runs_submit(unreal: Unreal) -> None:
    menu = unreal.Menu()
    unreal.menus["LevelEditor.MainMenu.Tools"] = menu
    _module().register_menu()
    [(section, entry)] = menu.entries
    assert section == "sqi"
    assert entry.label == "Submit to sqi…"
    assert entry.kwargs["type"] == "MENU_ENTRY"
    assert entry.command == (
        "PYTHON",
        "",
        "import sqi_unreal_submit; sqi_unreal_submit.submit()",
    )
    assert unreal.refreshed == 1


def test_unreal_missing_tools_menu_warns_instead_of_raising(unreal: Unreal) -> None:
    _module().register_menu()
    assert len(unreal.warnings) == 1 and "Tools" in unreal.warnings[0]
    assert unreal.refreshed == 0


def test_unreal_hook_registers_menu(monkeypatch: pytest.MonkeyPatch) -> None:
    calls: list[str] = []
    stub = SimpleNamespace(register_menu=lambda: calls.append("register_menu"))
    monkeypatch.setitem(sys.modules, "sqi_unreal_submit", stub)
    load_host_file(HOOK)
    assert calls == ["register_menu"]
    assert install.HOOK_MARKER in HOOK.read_text(encoding="utf-8").splitlines()


def test_unreal_module_imports_without_unreal(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setitem(sys.modules, "unreal", None)  # any `import unreal` now raises ImportError
    module = _module()
    assert callable(module.read_scene) and callable(module.submit)


def test_unreal_files_have_no_sqi_submitter_import() -> None:
    assert_no_sqi_submitter_import(MODULE)
    assert_no_sqi_submitter_import(HOOK)
