# SPDX-License-Identifier: AGPL-3.0-or-later
"""Nuke scene extraction (nuke imported lazily; never at module import)."""

from __future__ import annotations

from sqi_submitter.core import HostAdapter, RenderTarget, SceneContext, frame_range_str


class NukeAdapter(HostAdapter):
    host_name = "nuke"
    display_name = "Nuke"

    def scene_context(self) -> SceneContext:
        import nuke

        root = nuke.root()
        script = root.name()
        scene_path = None if script == "Root" else script
        start = root["first_frame"].value()
        end = root["last_frame"].value()
        return SceneContext(
            scene_path=scene_path,
            frame_range=frame_range_str(start, end),
        )

    def render_targets(self) -> list[RenderTarget]:
        import nuke

        targets: list[RenderTarget] = []
        for node in nuke.allNodes("Write"):
            if node["disable"].value():
                continue
            output_path = node["file"].value() or None
            frame_range = None
            if node["use_limit"].value():
                frame_range = frame_range_str(node["first"].value(), node["last"].value())
            targets.append(
                RenderTarget(
                    name=node.name(),
                    kind="write_node",
                    frame_range=frame_range,
                    output_path=output_path,
                    extra={"WriteNode": node.name()},
                )
            )
        return targets

    def is_scene_modified(self) -> bool:
        import nuke

        return bool(nuke.root().modified())

    def save_scene(self) -> bool:
        import nuke

        root = nuke.root()
        if root.name() == "Root":
            return False
        _ = nuke.scriptSave()
        return True
