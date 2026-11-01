# SPDX-License-Identifier: AGPL-3.0-or-later
"""VRED scene extraction over services the script plugin hands in.

VRED injects its API into a script plugin, not into ``sys.modules`` of a plain interpreter, so
the adapter never imports a VRED module: the plugin passes the objects in. That keeps it
importable and testable with no VRED present.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

from sqi_submitter.core import HostAdapter, RenderTarget, SceneContext, frame_range_str


@dataclass(frozen=True)
class VREDServices:
    """The VRED objects the adapter reads (all untyped: they exist only inside VRED).

    ``file_io`` is ``vrFileIOService``, ``cameras`` is ``vrCameraService``, ``render_settings``
    is the ``vrRenderSettings`` module and ``main_window`` is ``vrMainWindow``.
    """

    file_io: Any
    cameras: Any
    render_settings: Any
    main_window: Any


class VREDAdapter(HostAdapter):
    host_name = "vred"
    display_name = "VRED"

    def __init__(self, services: VREDServices) -> None:
        self._services = services

    def _scene_path(self) -> str | None:
        return str(self._services.file_io.getFileName() or "") or None

    def _frame_range(self) -> str | None:
        settings = self._services.render_settings
        if not settings.getRenderAnimation():
            return None
        start, stop = settings.getRenderStartFrame(), settings.getRenderStopFrame()
        frames = frame_range_str(start, stop)
        step = int(settings.getRenderFrameStep())
        # "5:2" is no range expression: a step belongs only on a range of two or more frames.
        return f"{frames}:{step}" if step > 1 and int(start) != int(stop) else frames

    def scene_context(self) -> SceneContext:
        output = str(self._services.render_settings.getRenderFilename() or "") or None
        return SceneContext(
            scene_path=self._scene_path(),
            frame_range=self._frame_range(),
            output_path=output,
            renderer=None,
        )

    def render_targets(self) -> list[RenderTarget]:
        targets: list[RenderTarget] = []
        for viewpoint in self._services.cameras.getAllViewpoints():
            name = str(viewpoint.getName())
            targets.append(RenderTarget(name=name, kind="viewpoint", extra={"View": name}))
        return targets

    def is_scene_modified(self) -> bool:
        # vrFileIOService has no unsaved-changes query; VRED shows one as a "*" in the window
        # title, which is also what AWS's own VRED submitter reads.
        return "*" in str(self._services.main_window.windowTitle())

    def save_scene(self) -> bool:
        path = self._scene_path()
        if path is None:
            return False  # untitled; the artist must save-as first
        # saveFile returns False when the save fails (read-only file, full disk).
        return bool(self._services.file_io.saveFile(path))
