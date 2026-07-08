# SPDX-License-Identifier: AGPL-3.0-or-later
"""SceneContext / RenderTarget model tests."""

from sqi_submitter.core.context import RenderTarget, SceneContext, frame_range_str


def test_scene_context_defaults_to_all_unknown() -> None:
    ctx = SceneContext()
    assert ctx.scene_path is None
    assert ctx.frame_range is None
    assert ctx.output_path is None
    assert ctx.renderer is None


def test_render_target_carries_extra_params() -> None:
    t = RenderTarget(name="/out/mantra1", kind="rop", extra={"RopPath": "/out/mantra1"})
    assert t.extra["RopPath"] == "/out/mantra1"
    assert t.frame_range is None


def test_frame_range_str_formats_whole_floats_as_ints() -> None:
    assert frame_range_str(1.0, 100.0) == "1-100"
    assert frame_range_str(5, 5) == "5"
