# SPDX-License-Identifier: AGPL-3.0-or-later
"""Maya adapter extraction tests against a fake maya.cmds."""

from typing import Any


def _install_fake_cmds(fake_host_module: Any, **overrides: Any) -> tuple[Any, list[bool]]:
    state: dict[Any, Any] = {
        ("file", ("query", "sceneName")): "/shows/a/shot_010.ma",
        ("file", ("query", "modified")): False,
        "defaultRenderGlobals.startFrame": 1.0,
        "defaultRenderGlobals.endFrame": 24.0,
        "defaultRenderGlobals.currentRenderer": "arnold",
        "workspace_root": "/shows/a",
        "workspace_images": "images",
        "layers": ["defaultRenderLayer", "charLayer", "refLayer"],
        "renderable": {"defaultRenderLayer": True, "charLayer": True, "refLayer": False},
        "referenced": {"refLayer": False, "defaultRenderLayer": False, "charLayer": False},
    }
    state.update(overrides)
    saved: list[bool] = []

    def file(
        *args: Any, **kw: Any
    ) -> Any:  # shadows the builtin deliberately: mirrors maya.cmds.file
        if kw.get("query") and kw.get("sceneName"):
            return state[("file", ("query", "sceneName"))]
        if kw.get("query") and kw.get("modified"):
            return state[("file", ("query", "modified"))]
        if kw.get("save"):
            saved.append(True)
            return state[("file", ("query", "sceneName"))]
        return None

    fake_host_module(
        "maya.cmds",
        file=file,
        getAttr=lambda attr: state[attr],
        workspace=lambda *a, **kw: (
            state["workspace_root"] if kw.get("rootDirectory") else state["workspace_images"]
        ),
        ls=lambda *a, **kw: list(state["layers"]) if kw.get("type") == "renderLayer" else [],
        referenceQuery=lambda node, **kw: state["referenced"][node],
    )
    return state, saved


def test_scene_context_extracts_basics(fake_host_module: Any) -> None:
    _install_fake_cmds(fake_host_module)
    from sqi_submitter.hosts.maya.adapter import MayaAdapter

    ctx = MayaAdapter().scene_context()
    assert ctx.scene_path == "/shows/a/shot_010.ma"
    assert ctx.frame_range == "1-24"
    assert ctx.output_path == "/shows/a/images"
    assert ctx.renderer == "arnold"


def test_render_targets_are_renderable_unreferenced_layers(fake_host_module: Any) -> None:
    def getAttr(attr: str) -> bool:
        if attr.endswith(".renderable"):
            return {"defaultRenderLayer": True, "charLayer": True, "refLayer": False}[
                attr.split(".")[0]
            ]
        raise KeyError(attr)

    _state, _ = _install_fake_cmds(fake_host_module)
    import sys

    sys.modules["maya.cmds"].getAttr = getAttr  # type: ignore[attr-defined]
    from sqi_submitter.hosts.maya.adapter import MayaAdapter

    targets = MayaAdapter().render_targets()
    assert [t.name for t in targets] == ["masterLayer", "charLayer"]
    assert targets[0].extra == {"RenderLayer": "masterLayer"}
    assert targets[1].extra == {"RenderLayer": "charLayer"}
    assert all(t.kind == "render_layer" for t in targets)


def test_render_targets_exclude_referenced_layers(fake_host_module: Any) -> None:
    # importedLayer is renderable but referenced: only the referenceQuery
    # filter can exclude it, so this test fails if that check is removed.
    _install_fake_cmds(
        fake_host_module,
        layers=["defaultRenderLayer", "charLayer", "importedLayer"],
        referenced={"defaultRenderLayer": False, "charLayer": False, "importedLayer": True},
        **{
            "defaultRenderLayer.renderable": True,
            "charLayer.renderable": True,
            "importedLayer.renderable": True,
        },
    )
    from sqi_submitter.hosts.maya.adapter import MayaAdapter

    targets = MayaAdapter().render_targets()
    assert [t.name for t in targets] == ["masterLayer", "charLayer"]


def test_save_scene_saves_when_named_and_fails_when_untitled(fake_host_module: Any) -> None:
    state, saved = _install_fake_cmds(fake_host_module)
    from sqi_submitter.hosts.maya.adapter import MayaAdapter

    assert MayaAdapter().save_scene() is True
    assert saved == [True]
    state[("file", ("query", "sceneName"))] = ""
    assert MayaAdapter().save_scene() is False
