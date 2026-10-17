# SPDX-License-Identifier: AGPL-3.0-or-later
"""3ds Max scene extraction (pymxs imported lazily; never at module import)."""

from __future__ import annotations

import re
from typing import Any

from sqi_submitter.core import HostAdapter, RenderTarget, SceneContext, frame_range_str

# OpenJD range syntax as 3ds Max's "Frames" box writes it: "1,3,5-12" (no spaces).
_PICKUP_FRAMES = re.compile(r"^\d+(-\d+)?(,\d+(-\d+)?)*$")


def _frame(time: Any) -> int:
    """A pymxs time value as a frame number."""
    return int(time.frame)


def _frame_range(rt: Any) -> str | None:
    kind = rt.rendTimeType
    if kind == 1:  # single: the current frame
        current = _frame(rt.sliderTime)
        return frame_range_str(current, current)
    if kind == 2:  # active time segment
        span = rt.animationRange
        return frame_range_str(_frame(span.start), _frame(span.end))
    if kind == 3:  # range
        return frame_range_str(_frame(rt.rendStart), _frame(rt.rendEnd))
    if kind == 4:  # pickup frames, passed through only when they are valid range syntax
        pickup = str(rt.rendPickupFrames or "")
        return pickup if _PICKUP_FRAMES.fullmatch(pickup) else None
    return None


class MaxAdapter(HostAdapter):
    host_name = "3dsmax"
    display_name = "3ds Max"

    def scene_context(self) -> SceneContext:
        from pymxs import runtime as rt

        name = str(rt.maxFileName or "")
        scene = str(rt.maxFilePath or "") + name if name else None
        output = str(rt.rendOutputFilename or "") or None if rt.rendSaveFile else None
        renderer = str(rt.classOf(rt.renderers.current)) or None
        return SceneContext(
            scene_path=scene,
            frame_range=_frame_range(rt),
            output_path=output,
            renderer=renderer,
        )

    def render_targets(self) -> list[RenderTarget]:
        return []  # whole-scene only

    def is_scene_modified(self) -> bool:
        from pymxs import runtime as rt

        return bool(rt.getSaveRequired())

    def save_scene(self) -> bool:
        from pymxs import runtime as rt

        name = str(rt.maxFileName or "")
        if not name:
            return False  # untitled; the artist must save-as first
        rt.saveMaxFile(str(rt.maxFilePath) + name, quiet=True)
        return True
