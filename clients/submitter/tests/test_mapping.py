# SPDX-License-Identifier: AGPL-3.0-or-later
"""Parameter-name convention pre-fill tests."""

from sqi_client.models import ProductParameter
from sqi_submitter.core.context import RenderTarget, SceneContext
from sqi_submitter.core.mapping import prefill


def _p(name: str, type_: str = "STRING") -> ProductParameter:
    return ProductParameter(name=name, type=type_)


CTX = SceneContext(
    scene_path="/shows/a/shot.ma",
    frame_range="1-100",
    output_path="/shows/a/images",
    renderer="arnold",
)


def test_prefill_matches_convention_names() -> None:
    params = [_p("SceneFile", "PATH"), _p("Frames"), _p("OutputDir", "PATH"), _p("Renderer")]
    got = prefill(params, CTX)
    assert got == {
        "SceneFile": "/shows/a/shot.ma",
        "Frames": "1-100",
        "OutputDir": "/shows/a/images",
        "Renderer": "arnold",
    }


def test_prefill_is_case_and_separator_insensitive() -> None:
    got = prefill([_p("scene_file", "PATH"), _p("FRAMERANGE")], CTX)
    assert got == {"scene_file": "/shows/a/shot.ma", "FRAMERANGE": "1-100"}


def test_prefill_skips_unknown_and_none_values() -> None:
    got = prefill([_p("Quality"), _p("Frames")], SceneContext(frame_range=None))
    assert got == {}


def test_target_overrides_scene_values_and_adds_extras() -> None:
    params = [_p("Frames"), _p("OutputDir", "PATH"), _p("WriteNode")]
    target = RenderTarget(
        name="Write1",
        kind="write_node",
        frame_range="10-20",
        output_path="/renders/w1",
        extra={"WriteNode": "Write1"},
    )
    got = prefill(params, CTX, target)
    assert got == {"Frames": "10-20", "OutputDir": "/renders/w1", "WriteNode": "Write1"}


def test_fork_with_convention_names_still_prefills() -> None:
    # A renamed studio fork keeps pre-fill by keeping the parameter names.
    got = prefill([_p("SceneFile", "PATH")], CTX)
    assert got == {"SceneFile": "/shows/a/shot.ma"}


def test_prefill_is_hyphen_insensitive() -> None:
    got = prefill([_p("Scene-File", "PATH"), _p("frame-range")], CTX)
    assert got == {"Scene-File": "/shows/a/shot.ma", "frame-range": "1-100"}


def test_extras_match_case_and_separator_insensitively() -> None:
    # target.extra key "WriteNode" must fill a parameter named "write_node".
    target = RenderTarget(name="Write1", kind="write_node", extra={"WriteNode": "Write1"})
    got = prefill([_p("write_node")], CTX, target)
    assert got == {"write_node": "Write1"}


def test_extras_win_over_convention_for_same_name() -> None:
    # "Frames" is both a convention-aliased key (frame_range) and, here, a
    # target extra. Extras must win: this is what makes the Blender "Scene"
    # extra (the scene NAME) safe now that "scene" is no longer a scene_path
    # alias — a fork adding a same-named extra always beats the convention.
    target = RenderTarget(name="T", kind="write_node", frame_range="10-20", extra={"Frames": "999"})
    got = prefill([_p("Frames")], CTX, target)
    assert got == {"Frames": "999"}


def test_scene_is_no_longer_a_scene_path_alias() -> None:
    # "scene" was removed from CONVENTION_ALIASES["scene_path"]: it collided
    # with hosts/blender/adapter.py's "Scene" extra (the scene NAME, not a
    # path). A parameter literally named "Scene" no longer prefills from
    # SceneContext.scene_path.
    got = prefill([_p("Scene")], CTX)
    assert got == {}
