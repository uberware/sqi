# SPDX-License-Identifier: AGPL-3.0-or-later
"""What a host adapter can tell us about the open scene."""

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import dataclass, field


@dataclass(frozen=True)
class SceneContext:
    """Best-effort scene facts; ``None`` means the host cannot cheaply know."""

    scene_path: str | None = None
    frame_range: str | None = None
    output_path: str | None = None
    renderer: str | None = None


@dataclass(frozen=True)
class RenderTarget:
    """One renderable unit (ROP, Write node, render layer, view layer).

    ``extra`` maps exact product-parameter names (e.g. ``RopPath``) to values;
    ``frame_range``/``output_path`` override the scene-level values when set.
    """

    name: str
    kind: str
    frame_range: str | None = None
    output_path: str | None = None
    extra: Mapping[str, str] = field(default_factory=dict)


def frame_range_str(start: float, end: float) -> str:
    """Format a start/end pair using the ``Frames`` convention ("1-100", "5")."""
    lo, hi = int(start), int(end)
    return str(lo) if lo == hi else f"{lo}-{hi}"
