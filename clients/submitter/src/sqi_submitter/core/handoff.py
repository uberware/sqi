# SPDX-License-Identifier: AGPL-3.0-or-later
"""The handoff context file: how a host that cannot run Qt launches ``sqi-submit``.

The host writes a version-1 JSON document (``context.schema.json``) describing the
open scene and its render targets, then runs ``sqi-submit --context FILE``. The
dialog side reads it back through :class:`ContextAdapter`, which is an ordinary
:class:`HostAdapter`, so everything downstream is shared with in-process hosts.
"""

from __future__ import annotations

import contextlib
import json
import os
from collections.abc import Callable, Mapping
from pathlib import Path
from typing import Any

from sqi_submitter.core.adapter import HostAdapter
from sqi_submitter.core.context import RenderTarget, SceneContext
from sqi_submitter.core.errors import SubmitterError

__all__ = ["CONTEXT_VERSION", "ContextAdapter", "ContextError", "dump_context", "load_context"]

CONTEXT_VERSION = 1

_Fail = Callable[[str], "ContextError"]


class ContextError(SubmitterError):
    """The context file is unreadable or invalid; the message names the file."""


class ContextAdapter(HostAdapter):
    """A :class:`HostAdapter` backed by a context file.

    Saving happens in the host before launch, so the scene is never reported
    as modified and ``save_scene`` always succeeds.
    """

    def __init__(
        self,
        *,
        host_name: str,
        display_name: str,
        scene: SceneContext,
        targets: list[RenderTarget],
        server_url: str | None = None,
        host_version: str | None = None,
    ) -> None:
        self.host_name = host_name
        self.display_name = display_name
        self.server_url = server_url
        self.host_version = host_version
        self._scene = scene
        self._targets = targets

    def scene_context(self) -> SceneContext:
        return self._scene

    def render_targets(self) -> list[RenderTarget]:
        return list(self._targets)

    def is_scene_modified(self) -> bool:
        return False

    def save_scene(self) -> bool:
        return True


def load_context(path: str | os.PathLike[str]) -> ContextAdapter:
    """Read and validate a context file, then delete it.

    A file that fails to read or validate raises :class:`ContextError` and is
    left in place for debugging.
    """
    file = Path(path)
    adapter = _parse(file, _read_document(file))
    # Best effort: a file we cannot delete must not stop the artist submitting.
    with contextlib.suppress(OSError):
        file.unlink()
    return adapter


def dump_context(adapter: HostAdapter, path: str) -> None:
    """Write ``adapter`` as a version-1 context file (the inverse of :func:`load_context`)."""
    scene = adapter.scene_context()
    host: dict[str, Any] = {"name": adapter.host_name, "display_name": adapter.display_name}
    server_url: str | None = None
    if isinstance(adapter, ContextAdapter):
        server_url = adapter.server_url
        if adapter.host_version is not None:
            host["version"] = adapter.host_version
    document = {
        "version": CONTEXT_VERSION,
        "host": host,
        "scene": {
            "path": scene.scene_path,
            "frame_range": scene.frame_range,
            "output_path": scene.output_path,
            "renderer": scene.renderer,
        },
        "targets": [
            {
                "name": t.name,
                "kind": t.kind,
                "frame_range": t.frame_range,
                "output_path": t.output_path,
                "extra": dict(t.extra),
                "locked": sorted(t.locked),
            }
            for t in adapter.render_targets()
        ],
        "server_url": server_url,
    }
    Path(path).write_text(json.dumps(document, indent=2), encoding="utf-8")


# -- parsing ------------------------------------------------------------------


def _read_document(file: Path) -> object:
    try:
        # utf-8-sig: a host writing from PowerShell may prepend a BOM.
        text = file.read_text(encoding="utf-8-sig")
    except (OSError, UnicodeDecodeError) as exc:
        raise ContextError(f"Cannot read the submit context file {file.name}: {exc}") from exc
    try:
        return json.loads(text)
    except json.JSONDecodeError as exc:
        raise ContextError(f"The submit context file {file.name} is not valid JSON: {exc}") from exc
    except (ValueError, RecursionError) as exc:
        # Valid JSON the decoder still refuses: nesting too deep for the stack, or (3.11+)
        # an integer longer than the interpreter's digit limit.
        raise ContextError(
            f"The submit context file {file.name} cannot be decoded as JSON: {exc}"
        ) from exc


def _parse(file: Path, document: object) -> ContextAdapter:
    def fail(problem: str) -> ContextError:
        return ContextError(f"The submit context file {file.name} is invalid: {problem}")

    if not isinstance(document, dict):
        raise fail("the top level must be a JSON object")
    version = document.get("version")
    if version != CONTEXT_VERSION or isinstance(version, bool):
        raise fail(f"unsupported version {version!r} (this sqi-submit reads version 1)")

    host = _obj(document, "host", fail, required=False)
    scene = _obj(document, "scene", fail, required=True)
    scene_path = scene.get("path")
    if not isinstance(scene_path, str) or not scene_path:
        raise fail("scene.path is missing or empty")

    scene_context = SceneContext(
        scene_path=scene_path,
        frame_range=_opt_str(scene, "frame_range", "scene.frame_range", fail),
        output_path=_opt_str(scene, "output_path", "scene.output_path", fail),
        renderer=_opt_str(scene, "renderer", "scene.renderer", fail),
    )
    targets_raw = document.get("targets", [])
    if not isinstance(targets_raw, list):
        raise fail("targets must be a list")
    targets = [_target(item, i, fail) for i, item in enumerate(targets_raw)]

    return ContextAdapter(
        host_name=_str(host, "name", "host.name", fail),
        display_name=_str(host, "display_name", "host.display_name", fail),
        scene=scene_context,
        targets=targets,
        server_url=_opt_str(document, "server_url", "server_url", fail),
        host_version=_opt_str(host, "version", "host.version", fail),
    )


def _obj(parent: Mapping[str, Any], key: str, fail: _Fail, *, required: bool) -> dict[str, Any]:
    value = parent.get(key)
    if value is None and not required:
        return {}
    if not isinstance(value, dict):
        raise fail(f"{key} must be an object")
    return value


def _opt_str(parent: Mapping[str, Any], key: str, where: str, fail: _Fail) -> str | None:
    value = parent.get(key)
    if value is None:
        return None
    if not isinstance(value, str):
        raise fail(f"{where} must be a string or null")
    return value


def _str(parent: Mapping[str, Any], key: str, where: str, fail: _Fail) -> str:
    return _opt_str(parent, key, where, fail) or ""


def _target(item: object, index: int, fail: _Fail) -> RenderTarget:
    where = f"targets[{index}]"
    if not isinstance(item, dict):
        raise fail(f"{where} must be an object")
    name, kind = item.get("name"), item.get("kind")
    if not isinstance(name, str) or not isinstance(kind, str):
        raise fail(f"{where} needs a string name and kind")
    extra_raw = item.get("extra", {})
    if not isinstance(extra_raw, dict):
        raise fail(f"{where}.extra must be an object")
    for key, value in extra_raw.items():
        if not isinstance(value, str):
            raise fail(f"{where}.extra[{key!r}] must be a string")
    locked_raw = item.get("locked", [])
    if not isinstance(locked_raw, list) or not all(isinstance(k, str) for k in locked_raw):
        raise fail(f"{where}.locked must be a list of strings")
    for key in locked_raw:
        if key not in extra_raw:
            raise fail(f"{where}.locked names {key!r}, which is not a key of extra")
    return RenderTarget(
        name=name,
        kind=kind,
        frame_range=_opt_str(item, "frame_range", f"{where}.frame_range", fail),
        output_path=_opt_str(item, "output_path", f"{where}.output_path", fail),
        extra=dict(extra_raw),
        locked=frozenset(locked_raw),
    )
