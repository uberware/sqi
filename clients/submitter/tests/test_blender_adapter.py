# SPDX-License-Identifier: AGPL-3.0-or-later
"""Blender adapter extraction tests against a fake bpy module."""

from types import SimpleNamespace
from typing import Any


def _install_fake_bpy(
    fake_host_module: Any, filepath: str = "/shows/d/shot.blend", dirty: bool = True
) -> tuple[Any, list[bool]]:
    layer_a, layer_b = SimpleNamespace(name="ViewLayer"), SimpleNamespace(name="fx")
    scene = SimpleNamespace(
        name="Scene",
        frame_start=1,
        frame_end=48,
        render=SimpleNamespace(filepath="/renders/d/", engine="CYCLES"),
        view_layers=[layer_a, layer_b],
    )
    saved = []
    bpy = fake_host_module(
        "bpy",
        data=SimpleNamespace(filepath=filepath, is_dirty=dirty, scenes=[scene]),
        context=SimpleNamespace(scene=scene),
        ops=SimpleNamespace(wm=SimpleNamespace(save_mainfile=lambda: saved.append(True))),
    )
    return bpy, saved


def test_scene_context(fake_host_module: Any) -> None:
    _install_fake_bpy(fake_host_module)
    from sqi_submitter.hosts.blender.adapter import BlenderAdapter

    ctx = BlenderAdapter().scene_context()
    assert ctx.scene_path == "/shows/d/shot.blend"
    assert ctx.frame_range == "1-48"
    assert ctx.output_path == "/renders/d/"
    assert ctx.renderer == "CYCLES"


def test_targets_are_scene_by_view_layer(fake_host_module: Any) -> None:
    _install_fake_bpy(fake_host_module)
    from sqi_submitter.hosts.blender.adapter import BlenderAdapter

    targets = BlenderAdapter().render_targets()
    assert [t.name for t in targets] == ["Scene / ViewLayer", "Scene / fx"]
    assert targets[1].extra == {"Scene": "Scene", "ViewLayer": "fx"}


def test_unsaved_blend_cannot_save(fake_host_module: Any) -> None:
    _install_fake_bpy(fake_host_module, filepath="")
    from sqi_submitter.hosts.blender.adapter import BlenderAdapter

    adapter = BlenderAdapter()
    assert adapter.scene_context().scene_path is None
    assert adapter.save_scene() is False


def test_field_layout_rows_follow_form_model() -> None:
    from sqi_client.models import ProductParameter
    from sqi_submitter.core.schema import FormModel
    from sqi_submitter.hosts.blender.addon import field_layout

    model = FormModel.from_parameters(
        [
            ProductParameter(name="Frames", type="STRING"),
            ProductParameter(name="Chunk", type="INT", default="1"),
        ]
    )
    assert field_layout(model) == [
        ("Frames", "Frames", "LINE_EDIT"),
        ("Chunk", "Chunk", "SPIN_BOX"),
    ]
