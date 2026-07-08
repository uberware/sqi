# SPDX-License-Identifier: AGPL-3.0-or-later
"""Name-convention pre-fill: SceneContext/RenderTarget → product parameters.

The convention is a documented, additive-only contract (docs/dcc-submitters.md):
pre-fill binds ONLY by parameter name, never by product identity.
"""

from __future__ import annotations

from collections.abc import Sequence

from sqi_client.models import ProductParameter
from sqi_submitter.core.context import RenderTarget, SceneContext

CONVENTION_ALIASES: dict[str, frozenset[str]] = {
    # "scene" is deliberately NOT an alias: hosts/blender/adapter.py emits a
    # target extra key "Scene" holding the Blender scene NAME (not a path),
    # and extras beat convention aliases in prefill() below. Keeping "scene"
    # here would silently overwrite a scene-name extra with a scene path for
    # any product parameter literally named "Scene".
    "scene_path": frozenset({"scenefile", "scenepath"}),
    "frame_range": frozenset({"frames", "framerange"}),
    "output_path": frozenset({"outputdir", "outputpath"}),
    "renderer": frozenset({"renderer"}),
}


def _norm(name: str) -> str:
    return name.lower().replace("_", "").replace("-", "")


def is_scene_path_param(name: str) -> bool:
    """True when a parameter name is the scene-file field by convention.

    Reuses CONVENTION_ALIASES["scene_path"] so hosts and UIs share one
    definition of "which field is the scene path".
    """
    return _norm(name) in CONVENTION_ALIASES["scene_path"]


def is_output_path_param(name: str) -> bool:
    """True when a parameter name is the output-path field by convention.

    Reuses CONVENTION_ALIASES["output_path"] so the "default output from the
    scene when left blank" rule shares one definition with prefill.
    """
    return _norm(name) in CONVENTION_ALIASES["output_path"]


def prefill(
    parameters: Sequence[ProductParameter],
    context: SceneContext,
    target: RenderTarget | None = None,
) -> dict[str, str]:
    """Return {product parameter name: value} for every convention match."""
    values: dict[str, str] = {}
    facts = {
        "scene_path": context.scene_path,
        "frame_range": context.frame_range,
        "output_path": context.output_path,
        "renderer": context.renderer,
    }
    if target is not None:
        if target.frame_range is not None:
            facts["frame_range"] = target.frame_range
        if target.output_path is not None:
            facts["output_path"] = target.output_path
    extras = {_norm(k): v for k, v in (target.extra if target else {}).items()}

    for param in parameters:
        key = _norm(param.name)
        if key in extras:
            values[param.name] = extras[key]
            continue
        for fact, aliases in CONVENTION_ALIASES.items():
            value = facts[fact]
            if value is not None and key in aliases:
                values[param.name] = value
                break
    return values
