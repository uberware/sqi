# SPDX-License-Identifier: AGPL-3.0-or-later
"""Blender scene extraction (bpy imported lazily; never at module import)."""

from __future__ import annotations

from sqi_submitter.core import HostAdapter, RenderTarget, SceneContext, frame_range_str


class BlenderAdapter(HostAdapter):
    host_name = "blender"
    display_name = "Blender"

    def scene_context(self) -> SceneContext:
        import bpy

        scene = bpy.context.scene
        return SceneContext(
            scene_path=bpy.data.filepath or None,
            frame_range=frame_range_str(scene.frame_start, scene.frame_end),
            output_path=scene.render.filepath,
            renderer=scene.render.engine,
        )

    def render_targets(self) -> list[RenderTarget]:
        import bpy

        targets: list[RenderTarget] = []
        for scene in bpy.data.scenes:
            frame_range = frame_range_str(scene.frame_start, scene.frame_end)
            for layer in scene.view_layers:
                targets.append(
                    RenderTarget(
                        name=f"{scene.name} / {layer.name}",
                        kind="view_layer",
                        frame_range=frame_range,
                        output_path=scene.render.filepath,
                        extra={"Scene": scene.name, "ViewLayer": layer.name},
                    )
                )
        return targets

    def is_scene_modified(self) -> bool:
        import bpy

        return bool(bpy.data.is_dirty)

    def save_scene(self) -> bool:
        import bpy

        if not bpy.data.filepath:
            return False  # never saved; the artist must save-as first
        bpy.ops.wm.save_mainfile()
        return True
