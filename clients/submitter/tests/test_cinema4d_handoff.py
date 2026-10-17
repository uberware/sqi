# SPDX-License-Identifier: AGPL-3.0-or-later
"""The Cinema 4D handoff plugin (a ``.pyp``), against a fake ``c4d`` module."""

from __future__ import annotations

import ast
import os
import sys
from collections.abc import Callable
from pathlib import Path
from types import ModuleType, SimpleNamespace
from typing import Any

import pytest

from sqi_submitter.core.handoff import load_context
from tests.conftest import load_host_file

PYP = (
    Path(__file__).resolve().parents[1]
    / "src"
    / "sqi_submitter"
    / "hosts"
    / "cinema4d"
    / "sqi_submit.pyp"
)
FakeHost = Callable[..., ModuleType]

UNSAVED = "Save your scene before submitting — the farm renders the file on disk."


class FakeTime:
    """A ``c4d.BaseTime``: ``GetFrame(fps)`` is the frame number."""

    def __init__(self, frame: int) -> None:
        self.frame = frame

    def GetFrame(self, fps: int) -> int:
        return self.frame


class FakeRenderData:
    """A ``c4d.documents.RenderData``: dict-backed ``__getitem__`` keyed by ``RDATA_*``."""

    def __init__(self, values: dict[int, object]) -> None:
        self.values = values

    def __getitem__(self, key: int) -> object:
        return self.values[key]

    def GetData(self) -> str:
        return "bc"


class FakeTake:
    def __init__(
        self, name: str, *, checked: bool = True, main: bool = False, children: Any = ()
    ) -> None:
        self.name, self.checked, self.main = name, checked, main
        self.children = list(children)
        self.next: FakeTake | None = None
        for left, right in zip(self.children, self.children[1:]):
            left.next = right

    def GetName(self) -> str:
        return self.name

    def IsChecked(self) -> bool:
        return self.checked

    def IsMain(self) -> bool:
        return self.main

    def GetDown(self) -> FakeTake | None:
        return self.children[0] if self.children else None

    def GetNext(self) -> FakeTake | None:
        return self.next


class FakeTakeData:
    def __init__(self, main: FakeTake, rendering: dict[str, FakeRenderData]) -> None:
        self.main, self.rendering = main, rendering

    def GetMainTake(self) -> FakeTake:
        return self.main

    def GetEffectiveRenderData(self, take: FakeTake) -> tuple[FakeRenderData, FakeTake]:
        return self.rendering[take.name], take


class FakeDoc:
    def __init__(
        self,
        *,
        path: str,
        name: str = "shot.c4d",
        changed: bool = False,
        takes: FakeTakeData,
        active: FakeRenderData,
    ) -> None:
        self.path, self.name, self.changed = path, name, changed
        self.takes, self.active = takes, active

    def GetDocumentPath(self) -> str:
        return self.path

    def GetDocumentName(self) -> str:
        return self.name

    def GetChanged(self) -> bool:
        return self.changed

    def GetTakeData(self) -> FakeTakeData:
        return self.takes

    def GetActiveRenderData(self) -> FakeRenderData:
        return self.active

    def GetFps(self) -> int:
        return 24

    def GetMinTime(self) -> FakeTime:
        return FakeTime(0)

    def GetMaxTime(self) -> FakeTime:
        return FakeTime(90)

    def GetLoopMinTime(self) -> FakeTime:
        return FakeTime(10)

    def GetLoopMaxTime(self) -> FakeTime:
        return FakeTime(20)

    def GetTime(self) -> FakeTime:
        return FakeTime(7)


MANUAL, CURRENT, ALL, PREVIEW = 0, 1, 2, 3
SEQUENCE, FROM, TO, PATH, SAVE = 100, 101, 102, 103, 104


def _rd(
    mode: int = MANUAL, frm: int = 1, to: int = 48, path: str = "/out/img", save: bool = True
) -> FakeRenderData:
    return FakeRenderData(
        {
            SEQUENCE: mode,
            FROM: FakeTime(frm),
            TO: FakeTime(to),
            PATH: path,
            SAVE: save,
        }
    )


class C4D:
    """The fake ``c4d`` module plus the calls the plugin makes on it."""

    def __init__(self, fake_host_module: FakeHost) -> None:
        self.dialogs: list[str] = []
        self.saves: list[tuple[object, str, int, int]] = []
        self.save_result = True
        self.registered: list[dict[str, Any]] = []
        self.expand: Callable[[str, dict[str, Any]], str] = lambda path, _rpd: path

        def save(doc: object, path: str, flags: int, fmt: int) -> bool:
            self.saves.append((doc, path, flags, fmt))
            return self.save_result

        def convert(path: str, rpd: dict[str, Any]) -> str:
            return self.expand(path, rpd)

        def register(**kwargs: Any) -> bool:
            self.registered.append(kwargs)
            return True

        self.module = fake_host_module(
            "c4d",
            RDATA_FRAMESEQUENCE=SEQUENCE,
            RDATA_FRAMEFROM=FROM,
            RDATA_FRAMETO=TO,
            RDATA_PATH=PATH,
            RDATA_SAVEIMAGE=SAVE,
            RDATA_FRAMESEQUENCE_MANUAL=MANUAL,
            RDATA_FRAMESEQUENCE_CURRENTFRAME=CURRENT,
            RDATA_FRAMESEQUENCE_ALLFRAMES=ALL,
            RDATA_FRAMESEQUENCE_PREVIEWRANGE=PREVIEW,
            SAVEDOCUMENTFLAGS_NONE=0,
            FORMAT_C4DEXPORT=1001,
            GetC4DVersion=lambda: 2025100,
            documents=SimpleNamespace(SaveDocument=save),
            gui=SimpleNamespace(MessageDialog=self.dialogs.append),
            modules=SimpleNamespace(tokensystem=SimpleNamespace(FilenameConvertTokens=convert)),
            plugins=SimpleNamespace(
                CommandData=type("CommandData", (), {}), RegisterCommandPlugin=register
            ),
        )


@pytest.fixture
def c4d(fake_host_module: FakeHost) -> C4D:
    return C4D(fake_host_module)


def _plugin() -> Any:
    return load_host_file(PYP)


def _doc(
    path: str = "/shows/d",
    *,
    changed: bool = False,
    takes: list[FakeTake] | None = None,
    rendering: dict[str, FakeRenderData] | None = None,
    active: FakeRenderData | None = None,
) -> FakeDoc:
    main = FakeTake("Main", main=True, children=takes or [])
    return FakeDoc(
        path=path,
        changed=changed,
        takes=FakeTakeData(main, rendering or {}),
        active=active or _rd(),
    )


def test_c4d_plugin_id_is_a_marked_placeholder() -> None:
    plugin = _plugin()
    assert plugin.PLUGIN_ID == 1000001
    lines = PYP.read_text(encoding="utf-8").splitlines()
    index = next(i for i, line in enumerate(lines) if line.startswith("PLUGIN_ID"))
    assert lines[index - 1] == (
        "# TODO(release): replace with an ID registered at developers.maxon.net before release"
    )


def test_c4d_frame_range_modes(c4d: C4D) -> None:
    frame_range = _plugin().frame_range_from
    # mode, frm, to, doc_min, doc_max, preview_min, preview_max, current
    assert frame_range(MANUAL, 5, 30, 0, 90, 10, 20, 7) == "5-30"
    assert frame_range(ALL, 5, 30, 0, 90, 10, 20, 7) == "0-90"
    assert frame_range(PREVIEW, 5, 30, 0, 90, 10, 20, 7) == "10-20"
    assert frame_range(CURRENT, 5, 30, 0, 90, 10, 20, 7) == "7"
    # A one-frame range is spelled as the frame, and a reversed one is put in order.
    assert frame_range(MANUAL, 12, 12, 0, 90, 10, 20, 7) == "12"
    assert frame_range(MANUAL, 30, 5, 0, 90, 10, 20, 7) == "5-30"


def test_c4d_read_scene_checked_takes_only(c4d: C4D) -> None:
    beauty = FakeTake("beauty")
    off = FakeTake("off", checked=False)
    nested = FakeTake("nested")
    parent = FakeTake("grouped", children=[nested])
    rendering = {name: _rd() for name in ("beauty", "off", "nested", "grouped")}
    doc = _doc(takes=[beauty, off, parent], rendering=rendering)
    targets: list[dict[str, Any]] = _plugin().read_scene(doc)["targets"]
    assert [t["name"] for t in targets] == ["beauty", "grouped", "nested"]
    assert [t["extra"] for t in targets] == [
        {"Take": "beauty"},
        {"Take": "grouped"},
        {"Take": "nested"},
    ]
    assert all(t["kind"] == "take" and t["locked"] == [] for t in targets)


def test_c4d_main_take_is_never_a_target(c4d: C4D) -> None:
    # A main take that turns up in the walk (and reports itself checked) is still excluded.
    doc = _doc(
        takes=[FakeTake("beauty"), FakeTake("Main", main=True)],
        rendering={"beauty": _rd(), "Main": _rd()},
    )
    targets: list[dict[str, Any]] = _plugin().read_scene(doc)["targets"]
    assert [t["name"] for t in targets] == ["beauty"]


def test_c4d_take_inherits_render_data(c4d: C4D) -> None:
    doc = _doc(
        takes=[FakeTake("override"), FakeTake("inherit")],
        rendering={
            "override": _rd(MANUAL, 100, 120, path="/out/override"),
            "inherit": _rd(ALL, path="/out/main", save=False),
        },
        active=_rd(MANUAL, 1, 10, path="/out/scene"),
    )
    data = _plugin().read_scene(doc)
    override, inherit = data["targets"]
    assert (override["frame_range"], override["output_path"]) == ("100-120", "/out/override")
    # ALLFRAMES comes from the document; no saved image means the scene default.
    assert (inherit["frame_range"], inherit["output_path"]) == ("0-90", None)
    # The scene block reflects the active render data.
    assert data["scene"] == {
        "path": os.path.join("/shows/d", "shot.c4d"),
        "frame_range": "1-10",
        "output_path": "/out/scene",
        "renderer": None,
    }
    assert data["host"] == {"name": "cinema4d", "display_name": "Cinema 4D", "version": "2025"}


def test_c4d_output_tokens_are_expanded(c4d: C4D) -> None:
    seen: list[dict[str, Any]] = []

    def expand(path: str, rpd: dict[str, Any]) -> str:
        seen.append(rpd)
        return path.replace("$take", "beauty")

    c4d.expand = expand
    doc = _doc(takes=[FakeTake("beauty")], rendering={"beauty": _rd(path="/out/$take/img")})
    [target] = _plugin().read_scene(doc)["targets"]
    assert target["output_path"] == "/out/beauty/img"
    assert seen[0]["_doc"] is doc
    assert seen[0]["_rBc"] == "bc"


def test_c4d_output_token_failure_passes_the_path_through(c4d: C4D) -> None:
    def boom(_path: str, _rpd: dict[str, Any]) -> str:
        raise RuntimeError("no tokens")

    c4d.expand = boom
    doc = _doc(takes=[FakeTake("beauty")], rendering={"beauty": _rd(path="/out/$take/img")})
    [target] = _plugin().read_scene(doc)["targets"]
    assert target["output_path"] == "/out/$take/img"


def test_c4d_read_scene_unsaved_has_no_path(c4d: C4D) -> None:
    assert _plugin().read_scene(_doc(path=""))["scene"]["path"] is None


def test_c4d_read_scene_is_a_valid_context_file(
    c4d: C4D, monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    monkeypatch.setattr("tempfile.gettempdir", lambda: str(tmp_path))
    plugin = _plugin()
    doc = _doc(takes=[FakeTake("beauty")], rendering={"beauty": _rd()})
    adapter = load_context(plugin.write_context(plugin.read_scene(doc)))
    assert adapter.host_name == "cinema4d"
    assert [t.name for t in adapter.render_targets()] == ["beauty"]
    assert adapter.render_targets()[0].extra == {"Take": "beauty"}


def test_c4d_unsaved_document_refused(c4d: C4D, monkeypatch: pytest.MonkeyPatch) -> None:
    plugin = _plugin()
    launched: list[str] = []
    monkeypatch.setattr(plugin, "launch", lambda *_a: launched.append("launch"))
    assert plugin.submit_document(_doc(path="")) is False
    assert c4d.dialogs == [UNSAVED]
    assert c4d.saves == [] and launched == []


def test_c4d_dirty_document_saved_before_launch(c4d: C4D, monkeypatch: pytest.MonkeyPatch) -> None:
    plugin = _plugin()
    events: list[str] = []

    def resolve(_configured: str) -> str:
        events.append("resolve")
        return "sqi"

    monkeypatch.setattr(plugin, "resolve_submit_command", resolve)

    def write(data: dict[str, Any]) -> str:
        events.append("write")
        assert data["scene"]["path"] == os.path.join("/shows/d", "shot.c4d")
        return "ctx.json"

    monkeypatch.setattr(plugin, "write_context", write)
    monkeypatch.setattr(plugin, "launch", lambda cmd, ctx: events.append(f"launch:{cmd}:{ctx}"))
    doc = _doc(changed=True, takes=[FakeTake("beauty")], rendering={"beauty": _rd()})
    original = c4d.module.documents.SaveDocument

    def record_save(*args: Any) -> bool:
        events.append("save")
        return bool(original(*args))

    c4d.module.documents.SaveDocument = record_save
    assert plugin.submit_document(doc) is True
    assert events == ["save", "resolve", "write", "launch:sqi:ctx.json"]
    [(saved_doc, path, flags, fmt)] = c4d.saves
    assert saved_doc is doc
    assert path == os.path.join("/shows/d", "shot.c4d")
    assert (flags, fmt) == (0, 1001)  # SAVEDOCUMENTFLAGS_NONE, FORMAT_C4DEXPORT
    assert c4d.dialogs == []


def test_c4d_clean_document_is_not_saved(c4d: C4D, monkeypatch: pytest.MonkeyPatch) -> None:
    plugin = _plugin()
    launched: list[str] = []
    monkeypatch.setattr(plugin, "resolve_submit_command", lambda _c: "sqi")
    monkeypatch.setattr(plugin, "write_context", lambda _d: "ctx.json")
    monkeypatch.setattr(plugin, "launch", lambda *_a: launched.append("launch"))
    assert plugin.submit_document(_doc()) is True
    assert c4d.saves == [] and launched == ["launch"]


def test_c4d_failed_save_stops_the_handoff(c4d: C4D, monkeypatch: pytest.MonkeyPatch) -> None:
    plugin = _plugin()
    launched: list[str] = []
    monkeypatch.setattr(plugin, "launch", lambda *_a: launched.append("launch"))
    c4d.save_result = False
    assert plugin.submit_document(_doc(changed=True)) is False
    assert len(c4d.dialogs) == 1 and "could not save" in c4d.dialogs[0]
    assert launched == []


@pytest.mark.parametrize("error", [RuntimeError("sqi-submit was not found."), OSError("denied")])
def test_c4d_launch_problems_are_shown_not_raised(
    c4d: C4D, monkeypatch: pytest.MonkeyPatch, error: Exception
) -> None:
    plugin = _plugin()
    monkeypatch.setattr(plugin, "resolve_submit_command", lambda _c: "sqi")
    monkeypatch.setattr(plugin, "write_context", lambda _d: "ctx.json")

    def fail(*_args: object) -> None:
        raise error

    monkeypatch.setattr(plugin, "launch", fail)
    assert plugin.submit_document(_doc()) is False
    assert c4d.dialogs == [str(error)]


def test_c4d_register_adds_the_command(c4d: C4D, monkeypatch: pytest.MonkeyPatch) -> None:
    plugin = _plugin()
    assert plugin.register() is True
    [call] = c4d.registered
    assert call["id"] == plugin.PLUGIN_ID
    assert call["str"] == "Submit to sqi…"
    assert call["info"] == 0
    assert call["icon"] is None
    assert call["help"] == "Submit this scene to the sqi farm"
    command = call["dat"]
    seen: list[object] = []

    def submit(doc: object) -> bool:
        seen.append(doc)
        return True

    monkeypatch.setattr(plugin, "submit_document", submit)
    assert command.Execute("the-doc") is True
    assert seen == ["the-doc"]


def test_c4d_plugin_imports_without_c4d(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setitem(sys.modules, "c4d", None)  # any `import c4d` now raises ImportError
    plugin = _plugin()
    assert plugin.PLUGIN_ID
    assert callable(plugin.read_scene) and callable(plugin.register)


def test_c4d_plugin_has_no_sqi_submitter_import() -> None:
    source = PYP.read_text(encoding="utf-8")
    for node in ast.walk(ast.parse(source)):
        if isinstance(node, ast.Import):
            assert not [a for a in node.names if a.name.split(".")[0] == "sqi_submitter"]
        if isinstance(node, ast.ImportFrom):
            assert (node.module or "").split(".")[0] != "sqi_submitter"
