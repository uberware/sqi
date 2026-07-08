# SPDX-License-Identifier: AGPL-3.0-or-later
"""Maya scene extraction (maya.cmds imported lazily; never at module import)."""

from __future__ import annotations

import os

from sqi_submitter.core import HostAdapter, RenderTarget, SceneContext, frame_range_str


class MayaAdapter(HostAdapter):
    host_name = "maya"
    display_name = "Maya"

    def scene_context(self) -> SceneContext:
        from maya import cmds

        scene = cmds.file(query=True, sceneName=True) or None
        start = cmds.getAttr("defaultRenderGlobals.startFrame")
        end = cmds.getAttr("defaultRenderGlobals.endFrame")
        renderer = cmds.getAttr("defaultRenderGlobals.currentRenderer") or None
        root = cmds.workspace(query=True, rootDirectory=True)
        images = cmds.workspace(fileRuleEntry="images")
        output = os.path.join(root, images) if root and images else None
        return SceneContext(
            scene_path=scene,
            frame_range=frame_range_str(start, end),
            output_path=output,
            renderer=renderer,
        )

    def render_targets(self) -> list[RenderTarget]:
        from maya import cmds

        targets: list[RenderTarget] = []
        for layer in cmds.ls(type="renderLayer") or []:
            if cmds.referenceQuery(layer, isNodeReferenced=True):
                continue
            if not cmds.getAttr(f"{layer}.renderable"):
                continue
            name = "masterLayer" if layer == "defaultRenderLayer" else layer
            targets.append(
                RenderTarget(name=name, kind="render_layer", extra={"RenderLayer": name})
            )
        return targets

    def is_scene_modified(self) -> bool:
        from maya import cmds

        return bool(cmds.file(query=True, modified=True))

    def save_scene(self) -> bool:
        from maya import cmds

        if not cmds.file(query=True, sceneName=True):
            return False  # never saved; the artist must save-as first
        cmds.file(save=True)
        return True
