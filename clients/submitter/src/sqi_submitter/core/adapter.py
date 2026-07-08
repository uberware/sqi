# SPDX-License-Identifier: AGPL-3.0-or-later
"""HostAdapter: the one interface a new Python tool implements to plug in."""

from __future__ import annotations

import abc

from sqi_submitter.core.context import RenderTarget, SceneContext


class HostAdapter(abc.ABC):
    """Scene extraction contract; everything else is shared core code.

    ``host_name`` is the lowercase token used for suggested-product grouping
    and per-host settings keys (e.g. "maya").
    """

    host_name: str = ""
    display_name: str = ""

    @abc.abstractmethod
    def scene_context(self) -> SceneContext:
        """Best-effort facts about the open scene (None = unknown)."""

    @abc.abstractmethod
    def render_targets(self) -> list[RenderTarget]:
        """Renderable units to offer in the target picker (may be empty)."""

    def is_scene_modified(self) -> bool:
        """Whether the scene has unsaved changes (default: assume saved)."""
        return False

    def save_scene(self) -> bool:
        """Save the scene; return False when it cannot be saved (no path)."""
        return True
