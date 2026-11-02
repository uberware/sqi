# SPDX-License-Identifier: AGPL-3.0-or-later
"""The Unreal Editor handoff module and its startup hook, against a fake ``unreal`` module."""

from __future__ import annotations

import json
import sys
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

UNREAL_DIR = Path(__file__).resolve().parents[1] / "src" / "sqi_submitter" / "hosts" / "unreal"
MODULE = UNREAL_DIR / "sqi_unreal_submit.py"
HOOK = UNREAL_DIR / "init_unreal.py"

STILL_DIRTY = "Save your changes before submitting — the farm renders the saved project."
REFUSAL_HEAD = "These queue jobs cannot render on the farm and were left out:\n"
NO_JOBS = (
    "The Movie Render Queue has no enabled jobs. Add or enable a job (or load a saved queue) "
    "in Window > Cinematics > Movie Render Queue, then submit again."
)
NONE_RENDERABLE_HEAD = (
    "None of the queue's enabled jobs can render on the farm, so nothing was submitted:"
)


class Path_:
    """A ``SoftObjectPath``: ``export_text()`` is its asset path."""

    def __init__(self, text: str) -> None:
        self.text = text

    def export_text(self) -> str:
        return self.text


def _shot(outer_name: str, *, enabled: bool = True) -> SimpleNamespace:
    """A ``MoviePipelineExecutorShot`` entry of a job's ``shot_info``."""
    return SimpleNamespace(outer_name=outer_name, enabled=enabled)


def _job(
    name: str = "Hero",
    *,
    sequence: str = "/Game/Seq/S1.S1",
    map_path: str = "/Game/Maps/Hero.Hero",
    config: str | None = "/Game/Cfg/Hi.Hi",
    enabled: bool = True,
    shots: list[SimpleNamespace] | None = None,
) -> SimpleNamespace:
    """A queue job whose shot list is empty until ``update_job_shot_list_from_sequence`` runs.

    ``shots`` is what the sequence yields on that call (default: one enabled shot, ``sh010``).
    """
    origin = SimpleNamespace(get_path_name=lambda: config) if config is not None else None
    return SimpleNamespace(
        job_name=name,
        sequence=Path_(sequence),
        map=Path_(map_path),
        is_enabled=lambda: enabled,
        get_preset_origin=lambda: origin,
        shot_info=[],
        sequence_shots=[_shot("sh010")] if shots is None else shots,
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
        self.loaded: list[str] = []
        self.missing_assets: set[str] = set()
        self.updates: list[tuple[Any, SimpleNamespace]] = []

        def load_asset(path: str) -> object | None:
            self.loaded.append(path)
            return None if path in self.missing_assets else SimpleNamespace(path=path)

        def update_shots(sequence: Any, job: SimpleNamespace) -> None:
            self.updates.append((sequence, job))
            job.shot_info = list(job.sequence_shots)

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
            EditorAssetLibrary=SimpleNamespace(load_asset=load_asset),
            MoviePipelineLibrary=SimpleNamespace(update_job_shot_list_from_sequence=update_shots),
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
            "Shots": json.dumps(["sh010"]),
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


def _shots_of(unreal: Unreal, job: SimpleNamespace) -> list[str]:
    unreal.jobs = [job]
    data, reasons = _module().read_scene()
    assert reasons == []
    [target] = data["targets"]
    extra = target["extra"]
    assert isinstance(extra, dict)
    return list(json.loads(extra["Shots"]))


def test_unreal_target_carries_enabled_shots_in_order(unreal: Unreal) -> None:
    job = _job(shots=[_shot("sh010"), _shot("sh015", enabled=False), _shot("sh020")])
    assert _shots_of(unreal, job) == ["sh010", "sh020"]


def test_unreal_shots_drop_duplicates_keeping_the_first(unreal: Unreal) -> None:
    job = _job(shots=[_shot("sh020"), _shot("sh010"), _shot("sh020"), _shot("sh010")])
    assert _shots_of(unreal, job) == ["sh020", "sh010"]


def test_unreal_shot_without_outer_name_is_named_after_the_sequence(unreal: Unreal) -> None:
    job = _job(sequence="/Game/Cinematics/SEQ_Hero.SEQ_Hero", shots=[_shot(""), _shot("sh020")])
    assert _shots_of(unreal, job) == ["SEQ_Hero", "sh020"]


def test_unreal_sequence_name_stands_in_for_a_bare_package_path(unreal: Unreal) -> None:
    job = _job(sequence="/Game/Cinematics/SEQ_Hero", shots=[_shot("")])
    assert _shots_of(unreal, job) == ["SEQ_Hero"]


def test_unreal_shot_list_is_rebuilt_from_the_sequence_before_reading(unreal: Unreal) -> None:
    job = _job(sequence="/Game/Seq/S1.S1", shots=[_shot("sh010")])
    assert job.shot_info == []
    assert _shots_of(unreal, job) == ["sh010"]  # empty until the update ran
    assert unreal.loaded == ["/Game/Seq/S1.S1"]
    [(sequence, updated)] = unreal.updates
    assert sequence.path == "/Game/Seq/S1.S1" and updated is job


def test_unreal_bare_package_sequence_is_loaded_as_package_dot_asset(unreal: Unreal) -> None:
    _shots_of(unreal, _job(sequence="/Game/Seq/S1"))
    assert unreal.loaded == ["/Game/Seq/S1.S1"]


def test_unreal_job_with_every_shot_disabled_is_refused(unreal: Unreal) -> None:
    unreal.jobs = [
        _job("Off", shots=[_shot("sh010", enabled=False), _shot("sh020", enabled=False)]),
        _job("Fine"),
    ]
    data, reasons = _module().read_scene()
    assert [t["name"] for t in data["targets"]] == ["Fine"]
    assert reasons == ["Off: no enabled shots"]


def test_unreal_job_with_no_shots_at_all_is_refused(unreal: Unreal) -> None:
    unreal.jobs = [_job("Empty", shots=[])]
    data, reasons = _module().read_scene()
    assert data["targets"] == []
    assert reasons == ["Empty: no enabled shots"]


def test_unreal_job_whose_sequence_cannot_be_loaded_is_refused(unreal: Unreal) -> None:
    unreal.missing_assets = {"/Game/Seq/Gone.Gone"}
    unreal.jobs = [_job("Gone", sequence="/Game/Seq/Gone.Gone")]
    data, reasons = _module().read_scene()
    assert data["targets"] == []
    assert reasons == ["Gone: its level sequence could not be loaded"]
    assert unreal.updates == []


def test_unreal_failing_shot_read_offers_the_job_without_shots(unreal: Unreal) -> None:
    def broken(_sequence: object, _job: object) -> None:
        raise AttributeError("update_job_shot_list_from_sequence")

    unreal.module.MoviePipelineLibrary.update_job_shot_list_from_sequence = broken
    unreal.jobs = [_job("Hero")]
    data, reasons = _module().read_scene()
    assert reasons == []
    [target] = data["targets"]
    assert target["name"] == "Hero"
    extra = target["extra"]
    assert isinstance(extra, dict)
    assert "Shots" not in extra
    assert extra["LevelSequence"] == "/Game/Seq/S1.S1"
    assert unreal.warnings == [
        "sqi: could not read the shots of Hero: update_job_shot_list_from_sequence"
    ]


def test_unreal_shot_name_over_the_preset_limit_is_refused(unreal: Unreal) -> None:
    unreal.jobs = [_job("Long", shots=[_shot("a" * 256), _shot("b" * 257)]), _job("Fine")]
    data, reasons = _module().read_scene()
    assert [t["name"] for t in data["targets"]] == ["Fine"]
    assert reasons == ["Long: shot name longer than 256 characters"]


def test_unreal_shot_name_of_exactly_the_limit_is_kept(unreal: Unreal) -> None:
    assert _shots_of(unreal, _job(shots=[_shot("a" * 256)])) == ["a" * 256]


def test_unreal_more_shots_than_the_preset_limit_is_refused(unreal: Unreal) -> None:
    unreal.jobs = [_job("Many", shots=[_shot(f"sh{n:04d}") for n in range(1001)])]
    data, reasons = _module().read_scene()
    assert data["targets"] == []
    assert reasons == ["Many: more than 1000 shots"]


def test_unreal_exactly_the_shot_count_limit_is_kept(unreal: Unreal) -> None:
    names = [f"sh{n:04d}" for n in range(1000)]
    assert _shots_of(unreal, _job(shots=[_shot(n) for n in names])) == names


def test_unreal_shot_refusal_is_shown_through_submit(
    unreal: Unreal, monkeypatch: pytest.MonkeyPatch
) -> None:
    module = _module()
    unreal.jobs = [_job("Off", shots=[_shot("sh010", enabled=False)])]
    stub_launch(module, monkeypatch, unreal.events)
    assert module.submit() is False
    assert unreal.events == []
    assert unreal.messages == [("sqi", NONE_RENDERABLE_HEAD + "\nOff: no enabled shots", "MSG_OK")]


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
    assert reasons == ["Scratch: its map is not saved"]


def test_unreal_job_on_unsaved_sequence_is_excluded(unreal: Unreal) -> None:
    unreal.jobs = [_job("Seq", sequence="/Engine/Transient.Seq")]
    data, reasons = _module().read_scene()
    assert data["targets"] == []
    assert reasons == ["Seq: its level sequence is not saved"]


@pytest.mark.parametrize(
    ("path", "problem"),
    [
        ("", "is not saved"),
        ("/Temp/x", "is not saved"),
        ("/Engine/Transient.x", "is not saved"),
        ("Game/x", "is not an asset path"),
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
    stub_launch(module, monkeypatch, unreal.events)
    assert module.submit() is True
    assert unreal.events == ["save:True:True", "write:1", "launch:sqi:ctx.json"]
    assert unreal.messages == []


def test_unreal_clean_project_is_not_saved(unreal: Unreal, monkeypatch: pytest.MonkeyPatch) -> None:
    module = _module()
    unreal.jobs = [_job()]
    stub_launch(module, monkeypatch, unreal.events)
    assert module.submit() is True
    assert unreal.events == ["write:1", "launch:sqi:ctx.json"]


def test_unreal_still_dirty_refuses(unreal: Unreal, monkeypatch: pytest.MonkeyPatch) -> None:
    module = _module()
    unreal.jobs = [_job()]
    unreal.content_dirty = ["asset"]
    unreal.save_clears = False
    stub_launch(module, monkeypatch, unreal.events)
    assert module.submit() is False
    assert unreal.events == ["save:True:True"]  # nothing written, nothing launched
    assert unreal.messages == [("sqi", STILL_DIRTY, "MSG_OK")]


def test_unreal_refusals_cancel_stops_launch(
    unreal: Unreal, monkeypatch: pytest.MonkeyPatch
) -> None:
    module = _module()
    unreal.jobs = [_job("Edited", config=None), _job("Fine")]
    unreal.answer = "CANCEL"
    stub_launch(module, monkeypatch, unreal.events)
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
    stub_launch(module, monkeypatch, unreal.events)
    assert module.submit() is True
    assert unreal.events == ["write:1", "launch:sqi:ctx.json"]
    [(_title, text, _kind)] = unreal.messages
    lines = text.split("\n")
    assert lines[0] + "\n" == REFUSAL_HEAD
    assert len(lines) == 3 and lines[1].startswith("Edited: ") and lines[2].startswith("Scratch: ")


def test_unreal_empty_queue_launches_nothing(
    unreal: Unreal, monkeypatch: pytest.MonkeyPatch
) -> None:
    module = _module()
    stub_launch(module, monkeypatch, unreal.events)
    assert module.submit() is False
    assert unreal.events == []
    assert unreal.messages == [("sqi", NO_JOBS, "MSG_OK")]


def test_unreal_all_jobs_disabled_launches_nothing(
    unreal: Unreal, monkeypatch: pytest.MonkeyPatch
) -> None:
    module = _module()
    unreal.jobs = [_job("Off", enabled=False)]
    stub_launch(module, monkeypatch, unreal.events)
    assert module.submit() is False
    assert unreal.events == []
    assert unreal.messages == [("sqi", NO_JOBS, "MSG_OK")]


def test_unreal_all_jobs_refused_launches_nothing(
    unreal: Unreal, monkeypatch: pytest.MonkeyPatch
) -> None:
    """With every enabled job refused there is nothing to continue with, so no Cancel choice."""
    module = _module()
    unreal.jobs = [
        _job("Edited", config=None),
        _job("Scratch", map_path="/Temp/m.m"),
        _job("Off", enabled=False),
    ]
    stub_launch(module, monkeypatch, unreal.events)
    assert module.submit() is False
    assert unreal.events == []
    assert unreal.messages == [
        (
            "sqi",
            NONE_RENDERABLE_HEAD
            + "\nEdited: its render settings are not saved as a config asset"
            + "\nScratch: its map is not saved",
            "MSG_OK",
        )
    ]


@pytest.mark.parametrize("error", [AttributeError("get_dirty_map_packages"), ValueError("bad")])
def test_unreal_scene_check_failure_is_shown_not_raised(
    unreal: Unreal, monkeypatch: pytest.MonkeyPatch, error: Exception
) -> None:
    """The dirty-package check and the save call unverified Editor APIs: failures are shown."""
    module = _module()
    unreal.jobs = [_job()]
    stub_launch(module, monkeypatch, unreal.events)

    def broken() -> list[object]:
        raise error

    unreal.module.EditorLoadingAndSavingUtils.get_dirty_map_packages = broken
    assert module.submit() is False
    assert unreal.events == []  # nothing written, nothing launched
    assert unreal.messages == [("sqi", f"sqi could not read the scene: {error}", "MSG_OK")]


@pytest.mark.parametrize("error", [AttributeError("MoviePipelineQueueSubsystem"), ValueError("x")])
def test_unreal_scene_read_failure_is_shown_not_raised(
    unreal: Unreal, monkeypatch: pytest.MonkeyPatch, error: Exception
) -> None:
    module = _module()
    stub_launch(module, monkeypatch, unreal.events)

    def broken() -> object:
        raise error

    monkeypatch.setattr(module, "read_scene", broken)
    assert module.submit() is False
    assert unreal.events == []
    assert unreal.messages == [("sqi", f"sqi could not read the scene: {error}", "MSG_OK")]


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


def test_unreal_hook_failure_does_not_break_startup(
    monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    """A broken sqi install must not stop the studio's own startup code after the hook."""
    monkeypatch.setitem(sys.modules, "sqi_unreal_submit", None)  # the import raises
    monkeypatch.setattr(sys, "path", list(sys.path))
    namespace: dict[str, Any] = {"__name__": "init"}
    exec(compile(HOOK.read_text(encoding="utf-8"), "init_unreal.py", "exec"), namespace)
    assert "was not added" in capsys.readouterr().out


def test_unreal_module_imports_without_unreal(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setitem(sys.modules, "unreal", None)  # any `import unreal` now raises ImportError
    module = _module()
    assert callable(module.read_scene) and callable(module.submit)


def test_unreal_files_have_no_sqi_submitter_import() -> None:
    assert_no_sqi_submitter_import(MODULE)
    assert_no_sqi_submitter_import(HOOK)
