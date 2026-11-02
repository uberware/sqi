# SPDX-License-Identifier: AGPL-3.0-or-later
"""Katana project extraction (Katana's API imported lazily; never at module import).

Used inside Katana 8 and later, where the handoff module's click opens the dialog in Katana's
own Python. The project reading is the handoff module's own (``render_node_targets``,
``save_project``): that file ships in this package too, imports only the standard library, and
is imported here as part of the package, never as the copy Katana's startup hook loaded.
"""

from __future__ import annotations

from sqi_submitter.core import HostAdapter, RenderTarget, SceneContext, frame_range_str
from sqi_submitter.hosts.katana.sqi_katana_submit import render_node_targets, save_project


class KatanaAdapter(HostAdapter):
    host_name = "katana"
    display_name = "Katana"

    def _scene_path(self) -> str | None:
        from Katana import FarmAPI

        return str(FarmAPI.GetKatanaFileName() or "") or None

    def scene_context(self) -> SceneContext:
        from Katana import NodegraphAPI

        return SceneContext(
            scene_path=self._scene_path(),
            frame_range=frame_range_str(NodegraphAPI.GetInTime(), NodegraphAPI.GetOutTime()),
            output_path=None,
            renderer=None,
        )

    def render_targets(self) -> list[RenderTarget]:
        return [
            RenderTarget(
                name=name, kind="render_node", frame_range=frames, extra={"RenderNode": name}
            )
            for name, frames in render_node_targets()
        ]

    def is_scene_modified(self) -> bool:
        from Katana import KatanaFile

        return bool(KatanaFile.IsFileDirty())

    def save_scene(self) -> bool:
        path = self._scene_path()
        if path is None:
            return False  # untitled; the artist must save-as first
        return save_project(path)
