# SPDX-License-Identifier: AGPL-3.0-or-later
"""The VRED adapter, menu glue and script plugin, against fake VRED services (no VRED, no Qt)."""

from __future__ import annotations

import json
import sys
from collections.abc import Iterator
from pathlib import Path
from types import ModuleType, SimpleNamespace
from typing import Any

import httpx
import pytest
import respx

from sqi_submitter import install
from sqi_submitter.core import (
    FormModel,
    HostAdapter,
    SubmitterSession,
    prefill,
    submit_form,
)
from tests.conftest import load_host_file

BASE = "http://test-server:8080"
PLUGIN = (
    Path(__file__).resolve().parents[1]
    / "src"
    / "sqi_submitter"
    / "hosts"
    / "vred"
    / "SubmitToSqi"
    / "SubmitToSqi.py"
)


@pytest.fixture(autouse=True)
def no_real_qt(monkeypatch: pytest.MonkeyPatch) -> None:
    """CI has no Qt, and a developer machine's real one would open a blocking dialog."""
    # A real binding another test already imported would otherwise still resolve its submodules.
    for name in ("PySide6", "PySide6.QtWidgets", "PySide2", "PySide2.QtWidgets"):
        monkeypatch.setitem(sys.modules, name, None)


class FakeVred:
    """The four injected services, with a record of what the adapter did to them."""

    def __init__(self, **overrides: Any) -> None:
        self.saved: list[str] = []
        self.save_result = True
        self.file_name = "D:/shows/a/shot_010.vpb"
        self.animation = True
        self.start = 1
        self.stop = 100
        self.step = 1
        self.output = "D:/shows/a/out/shot.png"
        self.title = "Autodesk VRED Professional 2026 - shot_010.vpb"
        self.viewpoints = ["Front", "Top 02"]
        for key, value in overrides.items():
            setattr(self, key, value)

    def _save(self, path: str) -> bool:
        self.saved.append(path)
        return self.save_result

    @property
    def services(self) -> Any:
        from sqi_submitter.hosts.vred.adapter import VREDServices

        return VREDServices(
            file_io=SimpleNamespace(getFileName=lambda: self.file_name, saveFile=self._save),
            cameras=SimpleNamespace(
                getAllViewpoints=lambda: [
                    SimpleNamespace(getName=lambda n=n: n) for n in self.viewpoints
                ]
            ),
            render_settings=SimpleNamespace(
                getRenderAnimation=lambda: self.animation,
                getRenderStartFrame=lambda: self.start,
                getRenderStopFrame=lambda: self.stop,
                getRenderFrameStep=lambda: self.step,
                getRenderFilename=lambda: self.output,
            ),
            main_window=SimpleNamespace(windowTitle=lambda: self.title),
        )

    def adapter(self) -> Any:
        from sqi_submitter.hosts.vred.adapter import VREDAdapter

        return VREDAdapter(self.services)


def test_vred_is_a_host_adapter() -> None:
    from sqi_submitter.hosts.vred.adapter import VREDAdapter

    assert issubclass(VREDAdapter, HostAdapter)
    assert VREDAdapter.host_name == "vred"
    assert VREDAdapter.display_name == "VRED"


@pytest.mark.parametrize(
    ("animation", "step", "expected"),
    [
        (True, 1, "1-100"),
        (True, 2, "1-100:2"),
        (True, 0, "1-100"),  # a step below 1 is not a step
        (False, 1, None),
    ],
)
def test_vred_scene_context_animation_range(
    animation: bool, step: int, expected: str | None
) -> None:
    ctx = FakeVred(animation=animation, step=step).adapter().scene_context()
    assert ctx.frame_range == expected


def test_vred_scene_context_single_frame_range_is_one_number() -> None:
    ctx = FakeVred(start=5, stop=5).adapter().scene_context()
    assert ctx.frame_range == "5"


def test_vred_scene_context_path_output_and_renderer() -> None:
    ctx = FakeVred().adapter().scene_context()
    assert ctx.scene_path == "D:/shows/a/shot_010.vpb"
    assert ctx.output_path == "D:/shows/a/out/shot.png"
    assert ctx.renderer is None


def test_vred_untitled_scene_has_no_path_or_output() -> None:
    ctx = FakeVred(file_name="", output="").adapter().scene_context()
    assert ctx.scene_path is None
    assert ctx.output_path is None


def test_vred_viewpoints_are_targets() -> None:
    targets = FakeVred().adapter().render_targets()
    assert [(t.name, t.kind, t.extra) for t in targets] == [
        ("Front", "viewpoint", {"View": "Front"}),
        ("Top 02", "viewpoint", {"View": "Top 02"}),
    ]


def test_vred_no_viewpoints_means_no_targets() -> None:
    assert FakeVred(viewpoints=[]).adapter().render_targets() == []


def test_vred_untitled_scene_cannot_save() -> None:
    vred = FakeVred(file_name="")
    assert vred.adapter().save_scene() is False
    assert vred.saved == []


def test_vred_save_scene_saves_in_place() -> None:
    vred = FakeVred()
    assert vred.adapter().save_scene() is True
    assert vred.saved == ["D:/shows/a/shot_010.vpb"]


def test_vred_save_scene_reports_failure() -> None:
    """saveFile returning False (read-only file, full disk) must not submit a stale scene."""
    vred = FakeVred(save_result=False)
    assert vred.adapter().save_scene() is False
    assert vred.saved == ["D:/shows/a/shot_010.vpb"]


@pytest.mark.parametrize(
    ("title", "modified"),
    [
        ("Autodesk VRED Professional 2026 - shot_010.vpb", False),
        ("Autodesk VRED Professional 2026 - shot_010.vpb*", True),
        ("Autodesk VRED Professional 2026 - *shot_010.vpb", True),
        ("", False),
    ],
)
def test_vred_modified_detection(title: str, modified: bool) -> None:
    """vrFileIOService has no unsaved-changes query; the "*" in the window title is the signal."""
    assert FakeVred(title=title).adapter().is_scene_modified() is modified


@respx.mock
@pytest.mark.usefixtures("_clean_settings")
def test_vred_contract_full_flow() -> None:
    respx.get(f"{BASE}/api/v1/products").mock(
        return_value=httpx.Response(200, json=[{"name": "vred-viewpoint-render"}])
    )
    respx.get(f"{BASE}/api/v1/products/vred-viewpoint-render/parameters").mock(
        return_value=httpx.Response(
            200,
            json=[
                {"name": "SceneFile", "type": "PATH"},
                {"name": "Frames", "type": "STRING"},
                {"name": "FramesPerTask", "type": "INT", "default": "10"},
                {"name": "OutputFile", "type": "PATH", "default": ""},
                {"name": "View", "type": "STRING"},
            ],
        )
    )
    route = respx.post(f"{BASE}/api/v1/products/vred-viewpoint-render/jobs").mock(
        return_value=httpx.Response(201, json={"id": "j9", "name": "n"})
    )
    vred = FakeVred(title="Autodesk VRED Professional 2026 - shot_010.vpb*")
    adapter = vred.adapter()
    session = SubmitterSession(server_url=BASE)
    model = FormModel.from_parameters(session.parameters("vred-viewpoint-render"))
    model.apply_prefill(
        prefill(
            [f.parameter for f in model.fields],
            adapter.scene_context(),
            adapter.render_targets()[1],
        )
    )
    job = submit_form(
        session,
        "vred-viewpoint-render",
        model,
        farm_id="f1",
        queue_id="q1",
        job_name="vred shot",
        adapter=adapter,
    )
    assert job.id == "j9"
    assert vred.saved == ["D:/shows/a/shot_010.vpb"]  # the modified scene was saved first
    parameters = json.loads(route.calls.last.request.content)["parameters"]
    assert parameters["SceneFile"] == "D:/shows/a/shot_010.vpb"
    assert parameters["Frames"] == "1-100"
    assert parameters["View"] == "Top 02"
    # The scene's own render filename stays the preset's default: no alias fills OutputFile.
    assert parameters.get("OutputFile", "") == ""


def test_vred_menu_parents_to_main_window(monkeypatch: pytest.MonkeyPatch) -> None:
    calls: list[tuple[Any, Any]] = []

    def fake_open(adapter: Any, server_url: Any = None, parent: Any = None) -> None:
        calls.append((adapter, parent))

    # Stub the module: importing the real qt.dialog fails without a Qt binding (CI has none).
    monkeypatch.setitem(
        sys.modules, "sqi_submitter.qt.dialog", SimpleNamespace(open_for_adapter=fake_open)
    )
    from sqi_submitter.hosts.vred import menu
    from sqi_submitter.hosts.vred.adapter import VREDAdapter

    vred = FakeVred()
    services = vred.services
    menu.open_submitter(services)
    assert len(calls) == 1
    adapter, parent = calls[0]
    assert isinstance(adapter, VREDAdapter)
    assert parent is services.main_window


# ---- the script plugin -------------------------------------------------------------------


def _plugin() -> ModuleType:
    return load_host_file(PLUGIN)


def test_vred_plugin_loads_outside_vred() -> None:
    """VRED's globals are absent in a plain interpreter: loading must register nothing."""
    module = _plugin()
    assert callable(module.load_submitter)
    assert callable(module.onDestroyVREDScriptPlugin)


def test_vred_plugin_reports_missing_sqi_submitter(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    monkeypatch.delenv("SQI_SUBMITTER_SITE", raising=False)
    plugin = _plugin()
    monkeypatch.setitem(sys.modules, "sqi_submitter", None)  # the import now fails
    loaded, message = plugin.load_submitter(str(tmp_path / "absent.json"))
    assert loaded is None
    assert message is not None
    assert "sandbox" in message.lower()
    assert "sqi-submitter" in message
    assert "pip install" in message
    assert "sqi_submitter_site" in message
    assert "%USERPROFILE%\\.sqi\\vred-site" in message  # real backslashes, not escapes


def test_vred_plugin_loads_the_menu_module_when_installed(tmp_path: Path) -> None:
    loaded, message = _plugin().load_submitter(str(tmp_path / "absent.json"))
    assert message is None
    assert loaded is not None
    assert callable(loaded.open_submitter)


@pytest.fixture
def restore_sys_path(monkeypatch: pytest.MonkeyPatch) -> Iterator[None]:
    monkeypatch.setattr(sys, "path", list(sys.path))
    yield


@pytest.mark.usefixtures("restore_sys_path")
def test_vred_plugin_adds_configured_site_to_path(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    monkeypatch.delenv("SQI_SUBMITTER_SITE", raising=False)
    site = tmp_path / "vred-site"
    site.mkdir()
    settings = tmp_path / "submitter.json"
    settings.write_text(json.dumps({"sqi_submitter_site": str(site)}), encoding="utf-8")
    plugin = _plugin()
    outcomes: list[object] = [ImportError("no sqi_submitter"), "the-menu-module"]
    seen_paths: list[bool] = []

    def importer() -> Any:
        seen_paths.append(str(site) in sys.path)
        outcome = outcomes.pop(0)
        if isinstance(outcome, Exception):
            raise outcome
        return outcome

    monkeypatch.setattr(plugin, "_import_menu", importer)
    loaded, message = plugin.load_submitter(str(settings))
    assert (loaded, message) == ("the-menu-module", None)
    assert seen_paths == [False, True]  # the site was added between the two attempts


@pytest.mark.usefixtures("restore_sys_path")
def test_vred_plugin_site_from_environment_wins(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    site = tmp_path / "env-site"
    site.mkdir()
    monkeypatch.setenv("SQI_SUBMITTER_SITE", str(site))
    plugin = _plugin()
    outcomes: list[object] = [ImportError("x"), "ok"]

    def importer() -> Any:
        outcome = outcomes.pop(0)
        if isinstance(outcome, Exception):
            raise outcome
        return outcome

    monkeypatch.setattr(plugin, "_import_menu", importer)
    assert plugin.load_submitter(str(tmp_path / "absent.json")) == ("ok", None)
    assert str(site) in sys.path


def test_vred_plugin_ignores_a_malformed_settings_file(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    monkeypatch.delenv("SQI_SUBMITTER_SITE", raising=False)
    settings = tmp_path / "submitter.json"
    settings.write_text("{not json", encoding="utf-8")
    plugin = _plugin()
    monkeypatch.setitem(sys.modules, "sqi_submitter", None)
    loaded, message = plugin.load_submitter(str(settings))
    assert loaded is None and message is not None


def test_vred_plugin_click_shows_the_message_instead_of_a_traceback(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    plugin = _plugin()
    shown: list[str] = []
    monkeypatch.setattr(plugin, "load_submitter", lambda path: (None, "install me"))
    monkeypatch.setattr(plugin, "_warn", shown.append)
    plugin.on_submit_clicked()  # must not raise
    assert shown == ["install me"]


def test_vred_plugin_click_opens_the_submitter_with_vreds_services(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    import builtins

    plugin = _plugin()
    opened: list[Any] = []
    menu_module = SimpleNamespace(open_submitter=opened.append)
    monkeypatch.setattr(plugin, "load_submitter", lambda path: (menu_module, None))
    window = object()
    monkeypatch.setattr(builtins, "vrFileIOService", "io", raising=False)
    monkeypatch.setattr(builtins, "vrCameraService", "cams", raising=False)
    monkeypatch.setattr(builtins, "vrMainWindow", window, raising=False)
    monkeypatch.setitem(sys.modules, "vrRenderSettings", ModuleType("vrRenderSettings"))
    plugin.on_submit_clicked()
    assert len(opened) == 1
    services = opened[0]
    assert (services.file_io, services.cameras, services.main_window) == ("io", "cams", window)
    assert services.render_settings is sys.modules["vrRenderSettings"]


def test_vred_plugin_click_reports_a_missing_vred_api(monkeypatch: pytest.MonkeyPatch) -> None:
    """Outside VRED (or on a build that renamed a service) the artist sees why, not a traceback."""
    plugin = _plugin()
    shown: list[str] = []
    monkeypatch.setattr(plugin, "load_submitter", lambda path: (SimpleNamespace(), None))
    monkeypatch.setattr(plugin, "_warn", shown.append)
    monkeypatch.setitem(sys.modules, "vrRenderSettings", None)
    plugin.on_submit_clicked()
    assert len(shown) == 1
    assert "VRED" in shown[0]


def test_vred_plugin_warn_prints_when_there_is_no_qt(capsys: pytest.CaptureFixture[str]) -> None:
    _plugin()._warn("something to read")
    assert "something to read" in capsys.readouterr().out


def test_vred_plugin_adds_the_sqi_menu_and_removes_it(monkeypatch: pytest.MonkeyPatch) -> None:
    class Signal:
        def __init__(self) -> None:
            self.slots: list[Any] = []

        def connect(self, slot: Any) -> None:
            self.slots.append(slot)

    class Menu:
        def __init__(self, title: str) -> None:
            self.title = title
            self.actions: list[tuple[str, Signal]] = []

        def addAction(self, text: str) -> SimpleNamespace:
            signal = Signal()
            self.actions.append((text, signal))
            return SimpleNamespace(triggered=signal)

        def menuAction(self) -> str:
            return "menu-action:" + self.title

    class MenuBar:
        def __init__(self) -> None:
            self.menus: list[Menu] = []
            self.removed: list[str] = []

        def addMenu(self, title: str) -> Menu:
            menu = Menu(title)
            self.menus.append(menu)
            return menu

        def removeAction(self, action: str) -> None:
            self.removed.append(action)

    bar = MenuBar()
    window = SimpleNamespace(menuBar=lambda: bar)
    plugin = _plugin()
    plugin.register_menu(window)
    assert [m.title for m in bar.menus] == ["sqi"]
    (text, signal) = bar.menus[0].actions[0]
    assert text == "Submit to sqi\u2026"
    assert signal.slots == [plugin.on_submit_clicked]
    plugin.unregister_menu()
    assert bar.removed == ["menu-action:sqi"]
    plugin.unregister_menu()  # a second call is harmless
    assert bar.removed == ["menu-action:sqi"]


def test_vred_plugin_does_not_leak_into_the_handoff_files() -> None:
    from sqi_submitter.install import HOST_FILES
    from tests.test_handoff_block import HANDOFF_HOST_FILES

    assert PLUGIN not in HANDOFF_HOST_FILES
    assert HOST_FILES["vred"] == ["hosts/vred/SubmitToSqi/SubmitToSqi.py"]


# ---- install-host ------------------------------------------------------------------------


def test_default_destination_vred(tmp_path: Path) -> None:
    docs = tmp_path / "Documents" / "Autodesk"
    (docs / "VRED-18.0").mkdir(parents=True)
    env = {"USERPROFILE": str(tmp_path)}
    assert install.default_destination("vred", "win32", env) == docs / "VRED-18.0" / "ScriptPlugins"


def test_default_destination_vred_zero_or_many_folders_is_none(tmp_path: Path) -> None:
    env = {"USERPROFILE": str(tmp_path)}
    assert install.default_destination("vred", "win32", env) is None  # no Autodesk folder
    docs = tmp_path / "Documents" / "Autodesk"
    docs.mkdir(parents=True)
    assert install.default_destination("vred", "win32", env) is None  # none inside
    (docs / "VRED-17.0").mkdir()
    (docs / "VRED-18.0").mkdir()
    assert install.default_destination("vred", "win32", env) is None  # ambiguous
    (docs / "VREDCore-18.0").mkdir()  # not a user folder, and does not match VRED-*


def test_default_destination_vred_is_windows_only(tmp_path: Path) -> None:
    (tmp_path / "Documents" / "Autodesk" / "VRED-18.0").mkdir(parents=True)
    assert install.default_destination("vred", "linux", {"HOME": str(tmp_path)}) is None
    assert install.default_destination("vred", "darwin", {"HOME": str(tmp_path)}) is None
    assert install.default_destination("vred", "win32", {}) is None


def test_install_host_vred_copies_the_plugin_into_its_own_folder(tmp_path: Path) -> None:
    import io

    dest = tmp_path / "ScriptPlugins"
    out = io.StringIO()
    assert install.install_host("vred", dest, out) == 0
    copied = dest / "SubmitToSqi" / "SubmitToSqi.py"
    assert copied.read_bytes() == PLUGIN.read_bytes()
    assert "Python sandbox" in out.getvalue()
    assert "Submit to sqi\u2026 appears in the sqi menu." in out.getvalue()


def test_install_host_vred_without_destination_explains_where(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    import io

    monkeypatch.setattr(install, "default_destination", lambda host, platform, env: None)
    out = io.StringIO()
    assert install.install_host("vred", None, out) == 0
    text = out.getvalue()
    assert "ScriptPlugins" in text
    assert "SubmitToSqi.py" in text
