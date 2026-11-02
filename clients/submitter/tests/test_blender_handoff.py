# SPDX-License-Identifier: AGPL-3.0-or-later
"""The single-file Blender handoff add-on, against a fake bpy module."""

from __future__ import annotations

import importlib
import sys
from collections.abc import Callable
from pathlib import Path
from types import ModuleType, SimpleNamespace
from typing import Any

import pytest

from sqi_submitter.core.handoff import load_context
from tests.conftest import FakeHost, assert_no_sqi_submitter_import

ADDON = "sqi_submitter.hosts.blender.sqi_blender_addon"


def _install_fake_bpy(
    fake_host_module: FakeHost, filepath: str = "/shows/d/shot.blend", dirty: bool = True
) -> tuple[ModuleType, list[str]]:
    """A fake bpy with two scenes; ``events`` records ``save`` calls."""
    events: list[str] = []
    layer_a, layer_b = SimpleNamespace(name="ViewLayer"), SimpleNamespace(name="fx")
    scene = SimpleNamespace(
        name="Scene",
        frame_start=1,
        frame_end=48,
        render=SimpleNamespace(filepath="/renders/d/", engine="CYCLES"),
        view_layers=[layer_a, layer_b],
    )
    other = SimpleNamespace(
        name="Shot2",
        frame_start=10,
        frame_end=10,
        render=SimpleNamespace(filepath="/renders/e/", engine="BLENDER_EEVEE"),
        view_layers=[SimpleNamespace(name="ViewLayer")],
    )
    bpy = fake_host_module(
        "bpy",
        app=SimpleNamespace(version_string="4.2.1"),
        data=SimpleNamespace(filepath=filepath, is_dirty=dirty, scenes=[scene, other]),
        context=SimpleNamespace(scene=scene),
        ops=SimpleNamespace(wm=SimpleNamespace(save_mainfile=lambda: events.append("save"))),
    )
    return bpy, events


def _addon() -> Any:
    sys.modules.pop(ADDON, None)
    return importlib.import_module(ADDON)


def _reports() -> tuple[list[tuple[set[str], str]], Callable[[set[str], str], None]]:
    seen: list[tuple[set[str], str]] = []

    def report(level: set[str], message: str) -> None:
        seen.append((level, message))

    return seen, report


def test_blender_read_scene_targets(fake_host_module: FakeHost) -> None:
    _install_fake_bpy(fake_host_module)
    data = _addon().read_scene()
    assert data["version"] == 1
    assert data["host"] == {"name": "blender", "display_name": "Blender", "version": "4.2.1"}
    assert data["scene"] == {
        "path": "/shows/d/shot.blend",
        "frame_range": "1-48",
        "output_path": "/renders/d/",
        "renderer": "CYCLES",
    }
    targets: list[dict[str, Any]] = data["targets"]
    assert [t["name"] for t in targets] == ["Scene / ViewLayer", "Scene / fx", "Shot2 / ViewLayer"]
    assert all(t["kind"] == "view_layer" for t in targets)
    assert [t["extra"] for t in targets] == [
        {"Scene": "Scene", "ViewLayer": "ViewLayer"},
        {"Scene": "Scene", "ViewLayer": "fx"},
        {"Scene": "Shot2", "ViewLayer": "ViewLayer"},
    ]
    # Each target carries its own scene's frame range and output; the two layers that would
    # share /renders/d/ each get a folder of their own, the lone layer keeps its scene's.
    assert [t["frame_range"] for t in targets] == ["1-48", "1-48", "10"]
    assert [t["output_path"] for t in targets] == [
        "/renders/d/ViewLayer/",
        "/renders/d/fx/",
        "/renders/e/",
    ]
    assert data["scene"]["output_path"] == "/renders/d/"  # whole-scene output is untouched


def _install_scenes(fake_host_module: FakeHost, *scenes: tuple[str, str, list[str]]) -> None:
    """A fake bpy whose scenes are ``(name, render filepath, view layer names)``."""
    built = [
        SimpleNamespace(
            name=name,
            frame_start=1,
            frame_end=4,
            render=SimpleNamespace(filepath=filepath, engine="CYCLES"),
            view_layers=[SimpleNamespace(name=layer) for layer in layers],
        )
        for name, filepath, layers in scenes
    ]
    fake_host_module(
        "bpy",
        app=SimpleNamespace(version_string="5.2.2"),
        data=SimpleNamespace(filepath="/shows/d/shot.blend", is_dirty=False, scenes=built),
        context=SimpleNamespace(scene=built[0]),
        ops=SimpleNamespace(wm=SimpleNamespace(save_mainfile=lambda: None)),
    )


def _outputs() -> dict[str, object]:
    targets: list[dict[str, Any]] = _addon().read_scene()["targets"]
    return {str(t["name"]): t["output_path"] for t in targets}


@pytest.mark.parametrize(
    ("filepath", "expected"),
    [
        ("//renders/shot_####", ["//renders/Fg/shot_####", "//renders/Bg/shot_####"]),
        ("//shot_", ["//Fg/shot_", "//Bg/shot_"]),
        ("/tmp/", ["/tmp/Fg/", "/tmp/Bg/"]),
        ("shot_####", ["Fg/shot_####", "Bg/shot_####"]),
        ("C:\\renders\\shot_####", ["C:\\renders\\Fg\\shot_####", "C:\\renders\\Bg\\shot_####"]),
    ],
)
def test_blender_layers_sharing_an_output_get_a_layer_folder(
    fake_host_module: FakeHost, filepath: str, expected: list[str]
) -> None:
    _install_scenes(fake_host_module, ("Shot", filepath, ["Fg", "Bg"]))
    assert list(_outputs().values()) == expected


def test_blender_unshared_outputs_are_unchanged(fake_host_module: FakeHost) -> None:
    _install_scenes(
        fake_host_module,
        ("Shot", "//renders/shot_####", ["Fg"]),
        ("Alt", "//renders/alt_####", ["AltLayer"]),
    )
    assert _outputs() == {
        "Shot / Fg": "//renders/shot_####",
        "Alt / AltLayer": "//renders/alt_####",
    }


def test_blender_scenes_sharing_an_output_keep_distinct_layer_names(
    fake_host_module: FakeHost,
) -> None:
    _install_scenes(
        fake_host_module,
        ("Shot", "//renders/img_####", ["Fg"]),
        ("Alt", "//renders/img_####", ["Bg"]),
    )
    assert _outputs() == {
        "Shot / Fg": "//renders/Fg/img_####",
        "Alt / Bg": "//renders/Bg/img_####",
    }


def test_blender_scenes_sharing_an_output_and_a_layer_name_get_scene_folders(
    fake_host_module: FakeHost,
) -> None:
    _install_scenes(
        fake_host_module,
        ("Shot", "//renders/img_####", ["ViewLayer", "Fg"]),
        ("Alt", "//renders/img_####", ["ViewLayer"]),
    )
    assert _outputs() == {
        "Shot / ViewLayer": "//renders/Shot/ViewLayer/img_####",
        "Shot / Fg": "//renders/Shot/Fg/img_####",
        "Alt / ViewLayer": "//renders/Alt/ViewLayer/img_####",
    }


def test_blender_layer_folder_names_are_safe_path_parts(fake_host_module: FakeHost) -> None:
    # A separator would nest folders, a reserved Windows character would fail to create the
    # folder, and a '#' would become part of Blender's frame number.
    _install_scenes(
        fake_host_module, ("Shot", "//r/s_", ["a/b\\c", 'x:*?"<>|#', "Bg 'quoted' layer"])
    )
    assert list(_outputs().values()) == [
        "//r/a_b_c/s_",
        "//r/x________/s_",
        "//r/Bg 'quoted' layer/s_",
    ]


def test_blender_read_scene_is_a_valid_context_file(
    fake_host_module: FakeHost, monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    _install_fake_bpy(fake_host_module)
    monkeypatch.setattr("tempfile.gettempdir", lambda: str(tmp_path))
    addon = _addon()
    adapter = load_context(addon.write_context(addon.read_scene()))
    assert adapter.host_name == "blender"
    assert adapter.scene_context().scene_path == "/shows/d/shot.blend"
    assert next(t.name for t in adapter.render_targets()) == "Scene / ViewLayer"


def test_blender_handoff_unsaved_scene(
    fake_host_module: FakeHost, monkeypatch: pytest.MonkeyPatch
) -> None:
    _, events = _install_fake_bpy(fake_host_module, filepath="")
    addon = _addon()
    monkeypatch.setattr(addon, "launch", lambda *_a: events.append("launch"))
    seen, report = _reports()
    assert addon.submit_scene(report, "") == {"CANCELLED"}
    assert seen == [
        (
            {"ERROR"},
            "Save your scene before submitting — the farm renders the file on disk.",
        )
    ]
    assert events == []


def test_blender_handoff_saves_dirty_then_launches(
    fake_host_module: FakeHost, monkeypatch: pytest.MonkeyPatch
) -> None:
    _, events = _install_fake_bpy(fake_host_module, dirty=True)
    addon = _addon()

    def resolve(configured: str) -> str:
        events.append(f"resolve:{configured}")
        return "/opt/sqi-submit"

    def write(data: dict[str, Any]) -> str:
        events.append("write")
        assert data["scene"]["path"] == "/shows/d/shot.blend"
        return "/tmp/ctx.json"

    monkeypatch.setattr(addon, "resolve_submit_command", resolve)
    monkeypatch.setattr(addon, "write_context", write)
    monkeypatch.setattr(addon, "launch", lambda cmd, ctx: events.append(f"launch:{cmd}:{ctx}"))
    seen, report = _reports()
    assert addon.submit_scene(report, "/configured") == {"FINISHED"}
    assert events == [
        "save",
        "resolve:/configured",
        "write",
        "launch:/opt/sqi-submit:/tmp/ctx.json",
    ]
    assert seen == []


def test_blender_handoff_clean_scene_is_not_saved(
    fake_host_module: FakeHost, monkeypatch: pytest.MonkeyPatch
) -> None:
    _, events = _install_fake_bpy(fake_host_module, dirty=False)
    addon = _addon()
    monkeypatch.setattr(addon, "resolve_submit_command", lambda _c: "sqi-submit")
    monkeypatch.setattr(addon, "write_context", lambda _d: "ctx.json")
    monkeypatch.setattr(addon, "launch", lambda *_a: events.append("launch"))
    _, report = _reports()
    assert addon.submit_scene(report, "") == {"FINISHED"}
    assert events == ["launch"]


@pytest.mark.parametrize(
    "error", [RuntimeError("sqi-submit was not found."), PermissionError("denied")]
)
def test_blender_launch_problems_are_reported_not_raised(
    fake_host_module: FakeHost, monkeypatch: pytest.MonkeyPatch, error: Exception
) -> None:
    _install_fake_bpy(fake_host_module, dirty=False)
    addon = _addon()
    monkeypatch.setattr(addon, "resolve_submit_command", lambda _c: "sqi")
    monkeypatch.setattr(addon, "write_context", lambda _d: "ctx.json")

    def fail(*_args: object) -> None:
        raise error

    monkeypatch.setattr(addon, "launch", fail)
    seen, report = _reports()
    assert addon.submit_scene(report, "") == {"CANCELLED"}
    assert seen == [({"ERROR"}, str(error))]


def test_blender_not_found_names_the_preference(
    fake_host_module: FakeHost, monkeypatch: pytest.MonkeyPatch
) -> None:
    _install_fake_bpy(fake_host_module, dirty=False)
    addon = _addon()

    def missing(_configured: str) -> str:
        raise addon.SubmitCommandNotFound(addon.NOT_FOUND_MESSAGE)

    monkeypatch.setattr(addon, "resolve_submit_command", missing)
    seen, report = _reports()
    assert addon.submit_scene(report, "") == {"CANCELLED"}
    [(level, message)] = seen
    assert level == {"ERROR"}
    assert message == addon.NOT_FOUND_MESSAGE + " Or set it in Edit > Preferences > Add-ons > sqi."


def test_blender_context_write_failure_is_reported(
    fake_host_module: FakeHost, monkeypatch: pytest.MonkeyPatch
) -> None:
    _, events = _install_fake_bpy(fake_host_module, dirty=False)
    addon = _addon()
    monkeypatch.setattr(addon, "resolve_submit_command", lambda _c: "sqi")

    def no_space(_data: dict[str, Any]) -> str:
        raise FileNotFoundError(2, "No such file or directory", "/tmp/sqi-handoff")

    monkeypatch.setattr(addon, "write_context", no_space)
    monkeypatch.setattr(addon, "launch", lambda *_a: events.append("launch"))
    seen, report = _reports()
    assert addon.submit_scene(report, "") == {"CANCELLED"}
    [(level, message)] = seen
    assert level == {"ERROR"} and "No such file or directory" in message
    assert events == []


def test_blender_register_adds_operator_panel_and_preferences(
    fake_host_module: FakeHost,
) -> None:
    registered: list[Any] = []
    fake_host_module(
        "bpy",
        types=SimpleNamespace(
            Operator=type("Operator", (), {}),
            Panel=type("Panel", (), {}),
            AddonPreferences=type("AddonPreferences", (), {}),
        ),
        props=SimpleNamespace(StringProperty=lambda **kw: ("prop", kw.get("subtype"))),
        utils=SimpleNamespace(
            register_class=registered.append,
            unregister_class=lambda cls: registered.remove(cls),
        ),
    )
    addon = _addon()
    addon.register()
    by_name = {cls.__name__: cls for cls in registered}
    assert set(by_name) == {"SQI_OT_submit", "SQI_PT_panel", "SQI_Preferences"}
    submit = by_name["SQI_OT_submit"]
    assert (submit.bl_idname, submit.bl_label) == ("sqi.submit", "Submit to sqi")
    panel = by_name["SQI_PT_panel"]
    assert (panel.bl_space_type, panel.bl_region_type, panel.bl_category) == (
        "VIEW_3D",
        "UI",
        "sqi",
    )
    prefs = by_name["SQI_Preferences"]
    assert prefs.bl_idname == ADDON
    assert "sqi_submit_command" in prefs.__annotations__
    addon.unregister()
    assert registered == []


def test_blender_addon_imports_without_bpy(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setitem(sys.modules, "bpy", None)  # any `import bpy` now raises ImportError
    addon = _addon()
    assert addon.bl_info["name"]
    assert addon.bl_info["category"] == "Render"
    assert callable(addon.read_scene)
    assert callable(addon.register)
    assert callable(addon.unregister)


def test_blender_addon_has_no_sqi_submitter_import() -> None:
    assert_no_sqi_submitter_import(Path(importlib.import_module(ADDON).__file__ or ""))
