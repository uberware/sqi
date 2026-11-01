# SPDX-License-Identifier: AGPL-3.0-or-later
"""The Silhouette handoff action and its ``sfxuser.py`` startup hook, against a fake ``fx``."""

from __future__ import annotations

import sys
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest

from sqi_submitter import install
from sqi_submitter.core.handoff import load_context
from tests.conftest import (
    FakeHost,
    ascii_fallback,
    assert_no_sqi_submitter_import,
    load_host_file,
    stub_launch,
)

SILHOUETTE_DIR = (
    Path(__file__).resolve().parents[1] / "src" / "sqi_submitter" / "hosts" / "silhouette"
)
MODULE = SILHOUETTE_DIR / "sqi_silhouette_submit.py"
HOOK = SILHOUETTE_DIR / "sfxuser.py"

UNSAVED = "Save your scene before submitting — the farm renders the file on disk."
SAVE_FAILED = "Silhouette could not save the project, so nothing was submitted."


class Node:
    def __init__(self, label: str, kind: str = "OutputNode") -> None:
        self.label = label
        self.type = kind


class Session:
    def __init__(self, label: str, start: int, duration: int, nodes: list[Node]) -> None:
        self.label = label
        self.startFrame = start
        self.duration = duration
        self.nodes = nodes


class Project:
    def __init__(self, path: str, sessions: list[Session], events: list[str]) -> None:
        self.path = path
        self.sessions = sessions
        self.is_modified = False
        self.save_error: Exception | None = None
        self.save_keeps_changes = False  # a save that returns without saving
        self._events = events

    def save(self, *args: object) -> None:
        assert args == ()  # the documented no-argument form: save in place
        self._events.append("save")
        if self.save_error is not None:
            raise self.save_error
        if not self.save_keeps_changes:
            self.is_modified = False


class Fx:
    """The fake ``fx`` module plus a record of what the script did with it."""

    def __init__(self, fake_host_module: FakeHost) -> None:
        self.events: list[str] = []
        self.actions: list[Any] = []
        self.sessions = [
            Session(
                "Roto", 1001, 50, [Node("Out Main"), Node("Merge", "MergeNode"), Node("Out A")]
            ),
            Session("Paint", 1, 24, [Node("Out B")]),
        ]
        self.project: Project | None = Project(
            "/p/shot.sfx/project.sfx", self.sessions, self.events
        )
        outer = self

        class Action:
            def __init__(self, label: str) -> None:
                self.label = label

        self.module = fake_host_module(
            "fx",
            Action=Action,
            addAction=lambda action: outer.actions.append(action),
            activeProject=lambda: outer.project,
            activeSession=lambda: outer.sessions[0],
        )


@pytest.fixture
def fx(fake_host_module: FakeHost, monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> Fx:
    # Tests that write a context file must never touch the real temp folder.
    monkeypatch.setattr("tempfile.gettempdir", lambda: str(tmp_path))
    # No Qt unless a test fakes one: an earlier dialog test can leave a real QApplication alive,
    # and a real modal QMessageBox would then block the run forever.
    monkeypatch.setitem(sys.modules, "PySide6", None)
    monkeypatch.setitem(sys.modules, "PySide2", None)
    return Fx(fake_host_module)


def _module() -> Any:
    return load_host_file(MODULE)


def _fake_qt(fake_host_module: FakeHost, binding: str, shown: list[str], app: bool = True) -> None:
    def warning(parent: object, title: str, text: str) -> None:
        shown.append(f"{binding}:{title}:{text}")

    box = SimpleNamespace(warning=warning)
    application = SimpleNamespace(instance=lambda: object() if app else None)
    fake_host_module(f"{binding}.QtWidgets", QMessageBox=box, QApplication=application)


@pytest.mark.parametrize(
    ("path", "expected"),
    [
        ("/p/shot.sfx/project.sfx", "/p/shot.sfx"),
        ("/p/shot.sfx", "/p/shot.sfx"),
        ("/p/shot.sfx/", "/p/shot.sfx"),
        ("C:\\shots\\shot.sfx\\project.sfx", "C:\\shots\\shot.sfx"),
        ("C:\\shots\\shot.sfx", "C:\\shots\\shot.sfx"),
        ("D:/shots/My Shot.sfx/Project.sfx", "D:/shots/My Shot.sfx"),
    ],
)
def test_silhouette_scene_path_is_the_bundle(path: str, expected: str) -> None:
    """A file path inside the bundle would fail every render, whatever OS made the path."""
    assert _module().bundle_path(path) == expected


def test_silhouette_scene_path_in_the_context_is_the_bundle(fx: Fx) -> None:
    assert _module().read_scene()["scene"]["path"] == "/p/shot.sfx"


def test_silhouette_targets_every_session_output_pair(fx: Fx) -> None:
    module = _module()
    data = module.read_scene()
    assert [t["name"] for t in data["targets"]] == [
        "Roto / Out Main",
        "Roto / Out A",
        "Paint / Out B",
    ]
    assert [t["frame_range"] for t in data["targets"]] == ["1001-1050", "1001-1050", "1-24"]
    for target in data["targets"]:
        assert target["kind"] == "output_node"
        assert target["output_path"] is None and target["locked"] == []
    assert data["targets"][1]["extra"] == {"Session": "Roto", "OutputNode": "Out A"}
    assert data["targets"][2]["extra"] == {"Session": "Paint", "OutputNode": "Out B"}
    assert data["host"]["name"] == "silhouette" and data["host"]["display_name"] == "Silhouette"
    assert data["scene"]["frame_range"] == "1001-1050"  # the active session's
    assert data["version"] == 1 and data["server_url"] is None


def test_silhouette_session_range_uses_session_numbering() -> None:
    session = SimpleNamespace(startFrame=1001, duration=50)
    assert _module().session_frame_range(session) == "1001-1050"
    assert _module().session_frame_range(SimpleNamespace(startFrame=1, duration=1)) == "1"


def test_silhouette_label_with_quote(fx: Fx) -> None:
    fx.sessions[0].label = 'Roto "A"'
    fx.sessions[0].nodes = [Node('Out "1"')]
    module = _module()
    adapter = load_context(module.write_context(module.read_scene()))
    target = adapter.render_targets()[0]
    assert target.name == 'Roto "A" / Out "1"'
    assert target.extra == {"Session": 'Roto "A"', "OutputNode": 'Out "1"'}


def test_silhouette_unsaved_project_refused(
    fx: Fx, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    module = _module()
    assert fx.project is not None
    fx.project.path = ""
    fx.project.is_modified = True
    stub_launch(module, monkeypatch, fx.events)
    assert module.submit() is False
    assert fx.events == []  # not saved, not written, not launched
    assert ascii_fallback(UNSAVED) in capsys.readouterr().out


def test_silhouette_no_project_refused(fx: Fx, monkeypatch: pytest.MonkeyPatch) -> None:
    module = _module()
    fx.project = None
    stub_launch(module, monkeypatch, fx.events)
    assert module.submit() is False
    assert fx.events == []


def test_silhouette_modified_project_saved_before_launch(
    fx: Fx, monkeypatch: pytest.MonkeyPatch
) -> None:
    module = _module()
    assert fx.project is not None
    fx.project.is_modified = True
    stub_launch(module, monkeypatch, fx.events)
    assert module.submit() is True
    assert fx.events == ["save", "write:3", "launch:sqi:ctx.json"]


def test_silhouette_unmodified_project_is_not_saved(
    fx: Fx, monkeypatch: pytest.MonkeyPatch
) -> None:
    module = _module()
    stub_launch(module, monkeypatch, fx.events)
    assert module.submit() is True
    assert fx.events == ["write:3", "launch:sqi:ctx.json"]


def test_silhouette_save_failure_submits_nothing(
    fx: Fx, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    module = _module()
    assert fx.project is not None
    fx.project.is_modified = True
    fx.project.save_error = RuntimeError("disk full")
    stub_launch(module, monkeypatch, fx.events)
    assert module.submit() is False
    assert fx.events == ["save"]
    assert ascii_fallback(SAVE_FAILED) in capsys.readouterr().out


def test_silhouette_save_that_returns_but_leaves_it_modified_submits_nothing(
    fx: Fx, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    module = _module()
    assert fx.project is not None
    fx.project.is_modified = True
    fx.project.save_keeps_changes = True
    stub_launch(module, monkeypatch, fx.events)
    assert module.submit() is False
    assert fx.events == ["save"]
    assert ascii_fallback(SAVE_FAILED) in capsys.readouterr().out


@pytest.mark.parametrize("error", [AttributeError("activeProject"), ValueError("x")])
def test_silhouette_scene_check_failure_is_shown_not_raised(
    fx: Fx,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
    error: Exception,
) -> None:
    """``activeProject`` is an unverified API on the scene check: whatever it raises is shown."""
    module = _module()
    stub_launch(module, monkeypatch, fx.events)

    def broken() -> object:
        raise error

    monkeypatch.setattr(fx.module, "activeProject", broken)
    assert module.submit() is False
    assert fx.events == []  # nothing written, nothing launched
    assert f"sqi could not read the scene: {error}" in capsys.readouterr().out


@pytest.mark.parametrize("error", [AttributeError("sessions"), ValueError("x")])
def test_silhouette_scene_read_failure_is_shown_not_raised(
    fx: Fx,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
    error: Exception,
) -> None:
    module = _module()
    stub_launch(module, monkeypatch, fx.events)

    def broken() -> object:
        raise error

    monkeypatch.setattr(module, "read_scene", broken)
    assert module.submit() is False
    assert fx.events == []
    assert f"sqi could not read the scene: {error}" in capsys.readouterr().out


@pytest.mark.parametrize("error", [RuntimeError("sqi-submit was not found."), OSError("denied")])
def test_silhouette_launch_problems_are_shown_not_raised(
    fx: Fx, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str], error: Exception
) -> None:
    module = _module()
    monkeypatch.setattr(module, "resolve_submit_command", lambda _c: "sqi")
    monkeypatch.setattr(module, "write_context", lambda _d: "ctx.json")

    def fail(*_args: object) -> None:
        raise error

    monkeypatch.setattr(module, "launch", fail)
    assert module.submit() is False
    assert str(error) in capsys.readouterr().out


@pytest.mark.parametrize("binding", ["PySide6", "PySide2"])
def test_silhouette_messages_use_the_bundled_qt(
    fx: Fx, fake_host_module: FakeHost, monkeypatch: pytest.MonkeyPatch, binding: str
) -> None:
    """Silhouette 2026 bundles PySide6, 2022 PySide2; whichever imports is the one shown."""
    shown: list[str] = []
    _fake_qt(fake_host_module, binding, shown)
    if binding == "PySide2":
        monkeypatch.setitem(sys.modules, "PySide6", None)  # not bundled: the import raises
    assert fx.project is not None
    fx.project.path = ""
    assert _module().submit() is False
    assert shown == [f"{binding}:sqi:{UNSAVED}"]


def test_silhouette_message_without_a_running_qt_is_printed(
    fx: Fx,
    fake_host_module: FakeHost,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    """No QApplication (headless ``sfxcmd -script``): a QMessageBox would crash, so print."""
    shown: list[str] = []
    _fake_qt(fake_host_module, "PySide6", shown, app=False)
    monkeypatch.setitem(sys.modules, "PySide2", None)
    assert fx.project is not None
    fx.project.path = ""
    assert _module().submit() is False
    assert shown == []
    assert ascii_fallback(UNSAVED) in capsys.readouterr().out


def test_silhouette_register_adds_action(fx: Fx, monkeypatch: pytest.MonkeyPatch) -> None:
    module = _module()
    calls: list[str] = []

    def submit() -> bool:
        calls.append("submit")
        return True

    monkeypatch.setattr(module, "submit", submit)
    module.register()
    assert len(fx.actions) == 1
    action = fx.actions[0]
    assert isinstance(action, fx.module.Action)
    assert action.label == "Submit to sqi…"
    assert action.available() is True  # the guide: an Action implements available() and execute()
    action.execute()
    assert calls == ["submit"]


def test_silhouette_module_imports_without_silhouette(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setitem(sys.modules, "fx", None)  # any `import fx` now raises
    module = _module()
    assert callable(module.read_scene) and callable(module.submit) and callable(module.register)


def test_silhouette_import_has_no_side_effects(fx: Fx) -> None:
    _module()
    assert fx.actions == [] and fx.events == []


def test_silhouette_hook_registers_the_action(fx: Fx, monkeypatch: pytest.MonkeyPatch) -> None:
    registered: list[str] = []
    stub = SimpleNamespace(register=lambda: registered.append("register"))
    monkeypatch.setitem(sys.modules, "sqi_silhouette_submit", stub)
    monkeypatch.setattr(sys, "path", list(sys.path))
    load_host_file(HOOK)
    assert registered == ["register"]
    assert install.HOOK_MARKER in HOOK.read_text(encoding="utf-8").splitlines()
    assert str(SILHOUETTE_DIR) in sys.path  # its own folder, so the module beside it imports


def test_silhouette_hook_without_dunder_file_uses_the_user_scripts_folder(
    fx: Fx, monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    registered: list[str] = []
    stub = SimpleNamespace(register=lambda: registered.append("register"))
    monkeypatch.setitem(sys.modules, "sqi_silhouette_submit", stub)
    monkeypatch.setattr(sys, "path", list(sys.path))
    monkeypatch.setenv("SFX_USER_PATH", str(tmp_path))
    namespace: dict[str, Any] = {"__name__": "sfxuser"}
    exec(compile(HOOK.read_text(encoding="utf-8"), "sfxuser.py", "exec"), namespace)
    assert str(tmp_path / "scripts") in sys.path
    assert registered == ["register"]


def test_silhouette_hook_failure_does_not_break_startup(
    fx: Fx, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    """A broken sqi install must not stop the artist's own ``sfxuser`` code after it."""
    monkeypatch.setitem(sys.modules, "sqi_silhouette_submit", None)  # the import raises
    load_host_file(HOOK)
    assert "was not added" in capsys.readouterr().out


def test_silhouette_files_have_no_sqi_submitter_import() -> None:
    assert_no_sqi_submitter_import(MODULE)
    assert_no_sqi_submitter_import(HOOK)


def test_default_destination_silhouette() -> None:
    assert (
        install.default_destination(
            "silhouette", "win32", {"SFX_USER_PATH": "C:/Users/a/Silhouette"}
        )
        == Path("C:/Users/a/Silhouette") / "scripts"
    )
    assert install.default_destination(
        "silhouette", "linux", {"SFX_USER_PATH": "/home/a/sfx"}
    ) == Path("/home/a/sfx/scripts")
    assert install.default_destination("silhouette", "darwin", {}) is None
    assert install.default_destination("silhouette", "win32", {"SFX_USER_PATH": ""}) is None


def test_silhouette_next_step_and_no_destination_hint(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(install, "default_destination", lambda host, platform, env: None)
    import io

    out = io.StringIO()
    assert install.install_host("silhouette", None, out) == 0
    text = out.getvalue()
    assert "sqi_silhouette_submit.py" in text and "sfxuser.py" in text
    assert "SFX_USER_PATH" in text
    assert "Restart Silhouette; Submit to sqi… appears in the Actions menu." in text


def test_install_host_copies_the_silhouette_files(tmp_path: Path) -> None:
    import io

    dest = tmp_path / "scripts"
    out = io.StringIO()
    assert install.install_host("silhouette", dest, out) == 0
    assert sorted(p.name for p in dest.iterdir()) == ["sfxuser.py", "sqi_silhouette_submit.py"]


def test_install_host_keeps_a_foreign_sfxuser(tmp_path: Path) -> None:
    import io

    dest = tmp_path / "scripts"
    dest.mkdir()
    studio = "import os\nSTUDIO = os.environ.get('STUDIO')\n"
    (dest / "sfxuser.py").write_text(studio, encoding="utf-8")
    out = io.StringIO()
    assert install.install_host("silhouette", dest, out) == 0
    text = out.getvalue()
    assert (dest / "sfxuser.py").read_text(encoding="utf-8") == studio
    assert (dest / "sqi_silhouette_submit.py").is_file()
    assert "from __future__" not in text
    assert "sqi_silhouette_submit" in text.split("append these lines to it:\n", 1)[1]
