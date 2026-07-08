# SPDX-License-Identifier: AGPL-3.0-or-later
"""Houdini scene extraction (hou imported lazily; never at module import)."""

from __future__ import annotations

from sqi_submitter.core import HostAdapter, RenderTarget, SceneContext, frame_range_str


class HoudiniAdapter(HostAdapter):
    host_name = "houdini"
    display_name = "Houdini"

    def scene_context(self) -> SceneContext:
        import hou

        hip_file = hou.hipFile
        basename = hip_file.basename()
        scene_path = None if basename == "untitled.hip" else hip_file.path()
        start, end = hou.playbar.frameRange()
        return SceneContext(
            scene_path=scene_path,
            frame_range=frame_range_str(start, end),
        )

    def render_targets(self) -> list[RenderTarget]:
        import hou

        targets: list[RenderTarget] = []

        for net_path in ["/out", "/stage"]:
            net = hou.node(net_path)
            if net is None:
                continue

            for child in net.children():
                if not isinstance(child, hou.RopNode):
                    continue

                # Determine frame range from f1/f2 parms
                f1_parm = child.parm("f1")
                f2_parm = child.parm("f2")
                frame_range = None
                if f1_parm is not None and f2_parm is not None:
                    frame_range = frame_range_str(f1_parm.eval(), f2_parm.eval())

                # Determine output path from parm list
                output_path = None
                for parm_name in (
                    "vm_picture",
                    "picture",
                    "RS_outputFileNamePrefix",
                    "ar_picture",
                    "sopoutput",
                ):
                    parm = child.parm(parm_name)
                    if parm is not None:
                        output_path = parm.evalAsString()
                        break

                child_path = child.path()
                targets.append(
                    RenderTarget(
                        name=child_path.split("/")[-1],
                        kind="rop",
                        frame_range=frame_range,
                        output_path=output_path,
                        extra={"RopPath": child_path},
                    )
                )

        return targets

    def is_scene_modified(self) -> bool:
        import hou

        return bool(hou.hipFile.hasUnsavedChanges())

    def save_scene(self) -> bool:
        import hou

        if hou.hipFile.basename() == "untitled.hip":
            return False
        hou.hipFile.save()
        return True
