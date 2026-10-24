# SPDX-License-Identifier: AGPL-3.0-or-later
"""HostAdapter: the one interface a new Python tool implements to plug in."""

from __future__ import annotations

import abc
from collections.abc import Sequence

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


class SnapshotAdapter(HostAdapter):
    """A frozen copy of an adapter that never calls the host: safe on any thread.

    Host APIs (pymxs, ``maya.cmds``, ``nuke``) are main-thread only, so a UI
    that submits off its GUI thread saves the scene and takes this snapshot on
    the GUI thread, then hands the snapshot to the worker. The scene context
    and targets are the captured ones. ``save_failed`` replays the save already
    done: when it failed the scene still reports modified and saving it again
    fails, so the shared submit flow reports that failure exactly as it would
    against the host; otherwise the scene is clean and a save succeeds.
    """

    def __init__(
        self,
        *,
        host_name: str,
        display_name: str,
        context: SceneContext,
        targets: Sequence[RenderTarget],
        save_failed: bool = False,
    ) -> None:
        self.host_name = host_name
        self.display_name = display_name
        self._context = context
        self._targets = list(targets)
        self._save_failed = save_failed

    @classmethod
    def capture(
        cls,
        adapter: HostAdapter,
        *,
        targets: Sequence[RenderTarget] | None = None,
        save_failed: bool = False,
    ) -> SnapshotAdapter:
        """Snapshot ``adapter`` with one ``scene_context`` call.

        ``render_targets`` is called only when ``targets`` (the ones the UI
        already holds) is not given.
        """
        return cls(
            host_name=adapter.host_name,
            display_name=adapter.display_name,
            context=adapter.scene_context(),
            targets=adapter.render_targets() if targets is None else targets,
            save_failed=save_failed,
        )

    def scene_context(self) -> SceneContext:
        return self._context

    def render_targets(self) -> list[RenderTarget]:
        return list(self._targets)

    def is_scene_modified(self) -> bool:
        return self._save_failed

    def save_scene(self) -> bool:
        return not self._save_failed
