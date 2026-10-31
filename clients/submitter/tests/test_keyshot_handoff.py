# SPDX-License-Identifier: AGPL-3.0-or-later
"""The KeyShot handoff script, against a fake ``lux`` module."""

from __future__ import annotations

import sys
from collections.abc import Iterator
from pathlib import Path
from typing import Any

import pytest

from sqi_submitter.core.handoff import load_context
from tests.conftest import FakeHost, assert_no_sqi_submitter_import, load_host_file

MODULE = (
    Path(__file__).resolve().parents[1]
    / "src"
    / "sqi_submitter"
    / "hosts"
    / "keyshot"
    / "sqi_keyshot_submit.py"
)

UNSAVED = "Save your scene before submitting — the farm renders the file on disk."
SAVE_FAILED = "KeyShot could not save the scene, so nothing was submitted."


class Lux:
    """The fake ``lux`` module plus a record of every call the script makes on it."""

    MESSAGE_BOX_INFO = 1
    MESSAGE_BOX_WARNING = 2
    MESSAGE_BOX_CRITICAL = 3

    def __init__(self, fake_host_module: FakeHost) -> None:
        self.calls: list[str] = []
        self.events: list[str] = []
        self.boxes: list[tuple[str, str, int]] = []
        self.scene: dict[str, object] = {"name": "car.bip", "file": "D:/shots/car.bip"}
        self.animation: dict[str, object] = {"frames": 120, "duration": 5.0}
        self.studios: list[str] = ["Hero", "Detail Shot"]
        self.changed = False
        self.save_error: Exception | None = None
        self.box_error: Exception | None = None
        self.module = fake_host_module(
            "lux",
            MESSAGE_BOX_INFO=self.MESSAGE_BOX_INFO,
            MESSAGE_BOX_WARNING=self.MESSAGE_BOX_WARNING,
            MESSAGE_BOX_CRITICAL=self.MESSAGE_BOX_CRITICAL,
            getSceneInfo=self._get_scene_info,
            getAnimationInfo=self._get_animation_info,
            getStudios=self._get_studios,
            isSceneChanged=self._is_scene_changed,
            saveFile=self._save_file,
            getMessageBox=self._get_message_box,
        )

    def _get_scene_info(self) -> dict[str, object]:
        self.calls.append("getSceneInfo")
        return self.scene

    def _get_animation_info(self) -> dict[str, object]:
        self.calls.append("getAnimationInfo")
        return self.animation

    def _get_studios(self) -> Iterator[str]:
        """The documented default "yields names": a generator, not a list."""
        self.calls.append("getStudios")
        return iter(self.studios)

    def _is_scene_changed(self) -> bool:
        self.calls.append("isSceneChanged")
        return self.changed

    def _save_file(self) -> None:
        self.calls.append("saveFile")
        self.events.append("save")
        if self.save_error is not None:
            raise self.save_error

    def _get_message_box(self, msg: str, title: str = "", type: int = 1) -> None:
        self.calls.append("getMessageBox")
        if self.box_error is not None:
            raise self.box_error
        self.boxes.append((msg, title, type))


@pytest.fixture
def lux(fake_host_module: FakeHost, monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> Lux:
    # Tests that write a context file must never touch the real temp folder.
    monkeypatch.setattr("tempfile.gettempdir", lambda: str(tmp_path))
    return Lux(fake_host_module)


def _module() -> Any:
    return load_host_file(MODULE)


def _stub_launch(module: Any, monkeypatch: pytest.MonkeyPatch, events: list[str]) -> None:
    monkeypatch.setattr(module, "resolve_submit_command", lambda _c: "sqi")

    def write(data: dict[str, Any]) -> str:
        events.append(f"write:{len(data['targets'])}")
        return "ctx.json"

    monkeypatch.setattr(module, "write_context", write)
    monkeypatch.setattr(module, "launch", lambda cmd, ctx: events.append(f"launch:{cmd}:{ctx}"))


def test_keyshot_read_scene_studios(lux: Lux) -> None:
    module = _module()
    data = module.read_scene()
    assert [t["name"] for t in data["targets"]] == ["Hero", "Detail Shot"]
    for target in data["targets"]:
        assert target["kind"] == "studio"
        assert target["frame_range"] == "1-120"
        assert target["output_path"] is None
        assert target["locked"] == []
        assert target["extra"] == {"Studio": target["name"]}
    assert data["host"]["name"] == "keyshot" and data["host"]["display_name"] == "KeyShot"
    assert data["scene"] == {
        "path": "D:/shots/car.bip",
        "frame_range": "1-120",
        "output_path": None,
        "renderer": None,
    }
    assert data["version"] == 1 and data["server_url"] is None
    adapter = load_context(module.write_context(data))
    assert adapter.host_name == "keyshot"
    studios = adapter.render_targets()
    assert [t.name for t in studios] == ["Hero", "Detail Shot"]
    assert studios[1].extra == {"Studio": "Detail Shot"}


@pytest.mark.parametrize(
    ("animation", "expected"),
    [
        ({"frames": 0, "duration": 0.0}, None),
        ({"frames": 1, "duration": 0.04}, "1"),
        ({"frames": 120, "duration": 5.0}, "1-120"),
        ({"duration": 5.0}, None),  # no frame count at all
        ({"frames": None}, None),
    ],
)
def test_keyshot_frame_range(lux: Lux, animation: dict[str, object], expected: str | None) -> None:
    lux.animation = animation
    data = _module().read_scene()
    assert data["scene"]["frame_range"] == expected
    assert [t["frame_range"] for t in data["targets"]] == [expected, expected]


def test_keyshot_unsaved_scene_has_no_scene_path(lux: Lux) -> None:
    lux.scene = {"name": "", "file": ""}
    assert _module().read_scene()["scene"]["path"] is None


def test_keyshot_version_from_display_version(lux: Lux) -> None:
    lux.module.getKeyShotDisplayVersion = lambda: (2024, 1)  # type: ignore[attr-defined]
    assert _module().read_scene()["host"]["version"] == "2024.1"


def test_keyshot_version_is_none_when_not_reported(lux: Lux) -> None:
    assert _module().read_scene()["host"]["version"] is None


def test_keyshot_unsaved_scene_refused(lux: Lux, monkeypatch: pytest.MonkeyPatch) -> None:
    module = _module()
    lux.scene = {"name": "", "file": ""}
    lux.changed = True
    _stub_launch(module, monkeypatch, lux.events)
    assert module.submit() is False
    assert lux.events == []  # not saved, not written, not launched
    assert lux.boxes == [(UNSAVED, "sqi", Lux.MESSAGE_BOX_WARNING)]


def test_keyshot_changed_scene_saved_before_launch(
    lux: Lux, monkeypatch: pytest.MonkeyPatch
) -> None:
    module = _module()
    lux.changed = True
    _stub_launch(module, monkeypatch, lux.events)
    assert module.submit() is True
    assert lux.events == ["save", "write:2", "launch:sqi:ctx.json"]
    assert lux.boxes == []


def test_keyshot_unchanged_scene_is_not_saved(lux: Lux, monkeypatch: pytest.MonkeyPatch) -> None:
    module = _module()
    _stub_launch(module, monkeypatch, lux.events)
    assert module.submit() is True
    assert lux.events == ["write:2", "launch:sqi:ctx.json"]
    assert "saveFile" not in lux.calls


def test_keyshot_save_failure_submits_nothing(lux: Lux, monkeypatch: pytest.MonkeyPatch) -> None:
    module = _module()
    lux.changed = True
    lux.save_error = RuntimeError("disk full")
    _stub_launch(module, monkeypatch, lux.events)
    assert module.submit() is False
    assert lux.events == ["save"]
    assert lux.boxes == [(SAVE_FAILED, "sqi", Lux.MESSAGE_BOX_WARNING)]


@pytest.mark.parametrize("error", [RuntimeError("sqi-submit was not found."), OSError("denied")])
def test_keyshot_launch_problems_are_shown_not_raised(
    lux: Lux, monkeypatch: pytest.MonkeyPatch, error: Exception
) -> None:
    module = _module()
    monkeypatch.setattr(module, "resolve_submit_command", lambda _c: "sqi")
    monkeypatch.setattr(module, "write_context", lambda _d: "ctx.json")

    def fail(*_args: object) -> None:
        raise error

    monkeypatch.setattr(module, "launch", fail)
    assert module.submit() is False
    assert lux.boxes == [(str(error), "sqi", Lux.MESSAGE_BOX_WARNING)]


def test_keyshot_message_without_a_message_box_is_printed(
    lux: Lux, capsys: pytest.CaptureFixture[str]
) -> None:
    lux.scene = {"name": "", "file": ""}
    lux.box_error = RuntimeError("no GUI in headless mode")
    assert _module().submit() is False
    assert UNSAVED in capsys.readouterr().out


def test_keyshot_main_runs_submit(lux: Lux, monkeypatch: pytest.MonkeyPatch) -> None:
    module = _module()
    calls: list[str] = []

    def submit() -> bool:
        calls.append("submit")
        return True

    monkeypatch.setattr(module, "submit", submit)
    module.main()
    assert calls == ["submit"]


def test_keyshot_import_has_no_side_effects(lux: Lux) -> None:
    """Loading the file (a ``__name__`` other than ``__main__``) calls nothing on ``lux``."""
    module = _module()
    assert lux.calls == []
    assert module.__name__ != "__main__"
    assert callable(module.read_scene) and callable(module.submit) and callable(module.main)


def test_keyshot_module_imports_without_keyshot(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setitem(sys.modules, "lux", None)  # any `import lux` now raises
    module = _module()
    assert callable(module.read_scene) and callable(module.submit)


def test_keyshot_files_have_no_sqi_submitter_import() -> None:
    assert_no_sqi_submitter_import(MODULE)


def test_keyshot_entry_point_is_guarded() -> None:
    """KeyShot runs a script as ``__main__``; the file must not run on import."""
    text = MODULE.read_text(encoding="utf-8").rstrip().splitlines()
    assert text[-2:] == ['if __name__ == "__main__":', "    main()"]
