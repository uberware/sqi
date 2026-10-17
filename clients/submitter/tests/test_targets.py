# SPDX-License-Identifier: AGPL-3.0-or-later
"""Multi-target core: target parameters, per-target models, sequential submit."""

from __future__ import annotations

import json

import httpx
import pytest
import respx

from sqi_client.models import ProductParameter
from sqi_submitter.core.context import RenderTarget, SceneContext
from sqi_submitter.core.schema import FormModel
from sqi_submitter.core.session import SubmitterSession
from sqi_submitter.core.targets import (
    TargetResult,
    is_per_target,
    job_name_for,
    per_target_models,
    submit_targets,
    target_parameters,
)
from tests.test_adapter_contract import BASE, MiniAdapter

pytestmark = pytest.mark.usefixtures("_clean_settings")

CTX = SceneContext(scene_path="/x/s.scene", frame_range="1-100", output_path="/out")


def _p(name: str, type_: str = "STRING", default: str | None = None) -> ProductParameter:
    return ProductParameter(name=name, type=type_, default=default)


AE_PARAMS = [
    _p("SceneFile", "PATH"),
    _p("RenderQueueItem"),
    _p("FramesPerTask", "INT", "10"),
    _p("OutputPath", "PATH"),
]
BLENDER_PARAMS = [_p("SceneFile", "PATH"), _p("Scene"), _p("ViewLayer"), _p("Frames")]
MAYA_SCENE_PARAMS = [_p("SceneFile", "PATH"), _p("Frames"), _p("Renderer")]


def _values(model: FormModel) -> dict[str, str]:
    return model.values()


def test_target_parameters_needs_key_on_every_target() -> None:
    targets = [
        RenderTarget(
            name="comp1",
            kind="rq_item",
            extra={"RenderQueueItem": "1", "FramesPerTask": "240"},
        ),
        RenderTarget(name="comp2", kind="rq_item", extra={"RenderQueueItem": "2"}),
    ]
    assert target_parameters(AE_PARAMS, targets) == frozenset({"RenderQueueItem"})
    assert is_per_target(AE_PARAMS, targets)


def test_target_parameters_empty_without_targets() -> None:
    assert target_parameters(AE_PARAMS, []) == frozenset()
    assert not is_per_target(AE_PARAMS, [])


def test_whole_scene_product_is_not_per_target() -> None:
    targets = [
        RenderTarget(name="layerA", kind="render_layer", extra={"RenderLayer": "layerA"}),
        RenderTarget(name="layerB", kind="render_layer", extra={"RenderLayer": "layerB"}),
    ]
    assert target_parameters(MAYA_SCENE_PARAMS, targets) == frozenset()
    assert not is_per_target(MAYA_SCENE_PARAMS, targets)


def test_target_parameters_match_is_separator_insensitive() -> None:
    targets = [RenderTarget(name="a", kind="k", extra={"render_layer": "a"})]
    assert target_parameters([_p("RenderLayer")], targets) == frozenset({"RenderLayer"})


def _blender_targets() -> list[RenderTarget]:
    return [
        RenderTarget(
            name="Scene/A",
            kind="view_layer",
            frame_range="1-10",
            extra={"Scene": "Scene", "ViewLayer": "A"},
        ),
        RenderTarget(
            name="Scene/B",
            kind="view_layer",
            frame_range="20-30",
            extra={"Scene": "Scene", "ViewLayer": "B"},
        ),
    ]


def test_per_target_models_untouched_fields_follow_each_target() -> None:
    targets = _blender_targets()
    models = per_target_models(BLENDER_PARAMS, CTX, targets, {})
    assert [t for t, _ in models] == targets
    first, second = (_values(m) for _, m in models)
    assert first["Frames"] == "1-10"
    assert second["Frames"] == "20-30"
    assert (first["Scene"], first["ViewLayer"]) == ("Scene", "A")
    assert (second["Scene"], second["ViewLayer"]) == ("Scene", "B")
    assert first["SceneFile"] == "/x/s.scene"


def test_per_target_models_edit_applies_to_all() -> None:
    models = per_target_models(BLENDER_PARAMS, CTX, _blender_targets(), {"Frames": "1-5"})
    assert [_values(m)["Frames"] for _, m in models] == ["1-5", "1-5"]


def test_per_target_models_locked_value_beats_edit() -> None:
    targets = [
        RenderTarget(
            name="movie",
            kind="rq_item",
            extra={"RenderQueueItem": "1", "FramesPerTask": "240"},
            locked=frozenset({"FramesPerTask"}),
        ),
        RenderTarget(name="images", kind="rq_item", extra={"RenderQueueItem": "2"}),
    ]
    models = per_target_models(AE_PARAMS, CTX, targets, {"FramesPerTask": "5"})
    movie, images = (_values(m) for _, m in models)
    assert movie["FramesPerTask"] == "240"
    assert images["FramesPerTask"] == "5"


def test_target_without_prefill_key_gets_form_default() -> None:
    targets = [
        RenderTarget(
            name="movie",
            kind="rq_item",
            extra={"RenderQueueItem": "1", "FramesPerTask": "240"},
        ),
        RenderTarget(name="images", kind="rq_item", extra={"RenderQueueItem": "2"}),
    ]
    models = per_target_models(AE_PARAMS, CTX, targets, {})
    assert _values(models[0][1])["FramesPerTask"] == "240"
    assert _values(models[1][1])["FramesPerTask"] == "10"


def test_target_parameter_edit_is_ignored() -> None:
    targets = _blender_targets()
    base = per_target_models(BLENDER_PARAMS, CTX, targets, {})
    edited = per_target_models(BLENDER_PARAMS, CTX, targets, {"ViewLayer": "ZZZ"})
    assert [_values(m) for _, m in edited] == [_values(m) for _, m in base]


def test_edit_for_unknown_parameter_is_ignored() -> None:
    models = per_target_models(BLENDER_PARAMS, CTX, _blender_targets(), {"Nope": "1"})
    assert all("Nope" not in _values(m) for _, m in models)


def test_job_name_for() -> None:
    target = RenderTarget(name="Scene/B", kind="view_layer")
    assert job_name_for("Blender", target) == "Blender - Scene/B"


def _three_targets() -> list[RenderTarget]:
    return [
        RenderTarget(name=n, kind="view_layer", extra={"Scene": "S", "ViewLayer": n})
        for n in ("A", "B", "C")
    ]


@respx.mock
def test_submit_targets_reports_partial_failure() -> None:
    route = respx.post(f"{BASE}/api/v1/products/blender/jobs").mock(
        side_effect=[
            httpx.Response(201, json={"id": "j1", "name": "n1"}),
            httpx.Response(400, json={"error": "bad layer"}),
            httpx.Response(201, json={"id": "j3", "name": "n3"}),
        ]
    )
    targets = _three_targets()
    models = per_target_models(BLENDER_PARAMS, CTX, targets, {"Frames": "1-5"})
    results = submit_targets(
        SubmitterSession(server_url=BASE),
        "blender",
        models,
        farm_id="f",
        queue_id="q",
        job_name_prefix="Blender",
        adapter=None,
        save_scene=False,
        job_options=None,
    )
    assert len(results) == 3
    assert all(isinstance(r, TargetResult) for r in results)
    assert [r.target for r in results] == targets
    assert results[0].job is not None and results[0].job.id == "j1"
    assert results[0].error is None
    assert results[1].job is None
    assert results[1].error is not None and "bad layer" in results[1].error
    assert results[2].job is not None and results[2].job.id == "j3"
    names = [json.loads(c.request.content)["name"] for c in route.calls]
    assert names == ["Blender - A", "Blender - B", "Blender - C"]


@respx.mock
def test_submit_targets_saves_once() -> None:
    respx.post(f"{BASE}/api/v1/products/blender/jobs").mock(
        return_value=httpx.Response(201, json={"id": "j", "name": "n"})
    )
    adapter = MiniAdapter(modified=True)
    saves: list[int] = []
    original = adapter.save_scene

    def counting_save() -> bool:
        saves.append(1)
        return original()

    adapter.save_scene = counting_save  # type: ignore[method-assign]
    models = per_target_models(BLENDER_PARAMS, CTX, _three_targets(), {})
    results = submit_targets(
        SubmitterSession(server_url=BASE),
        "blender",
        models,
        farm_id="f",
        queue_id="q",
        job_name_prefix="Blender",
        adapter=adapter,
        save_scene=True,
        job_options=None,
    )
    assert [r.error for r in results] == [None, None, None]
    assert len(saves) == 1


@respx.mock
def test_submit_targets_failed_save_submits_nothing() -> None:
    route = respx.post(f"{BASE}/api/v1/products/blender/jobs").mock(
        return_value=httpx.Response(201, json={"id": "j", "name": "n"})
    )
    adapter = MiniAdapter(modified=True, can_save=False)
    models = per_target_models(BLENDER_PARAMS, CTX, _three_targets(), {})
    results = submit_targets(
        SubmitterSession(server_url=BASE),
        "blender",
        models,
        farm_id="f",
        queue_id="q",
        job_name_prefix="Blender",
        adapter=adapter,
        save_scene=True,
        job_options=None,
    )
    assert route.call_count == 0
    assert len(results) == 3
    assert all(r.job is None for r in results)
    assert all(r.error is not None and "could not be saved" in r.error for r in results)


@respx.mock
def test_submit_targets_saves_even_when_first_target_fails_early() -> None:
    route = respx.post(f"{BASE}/api/v1/products/blender/jobs").mock(
        return_value=httpx.Response(201, json={"id": "j", "name": "n"})
    )
    params = [*BLENDER_PARAMS, _p("OutputPath", "PATH")]
    targets = [
        RenderTarget(name="A", kind="view_layer", extra={"Scene": "S", "ViewLayer": "A"}),
        RenderTarget(
            name="B",
            kind="view_layer",
            output_path="/o/b",
            extra={"Scene": "S", "ViewLayer": "B"},
        ),
        RenderTarget(
            name="C",
            kind="view_layer",
            output_path="/o/c",
            extra={"Scene": "S", "ViewLayer": "C"},
        ),
    ]
    ctx = SceneContext(scene_path="/x/s.scene", frame_range="1-4")
    adapter = MiniAdapter(modified=True)
    saves: list[int] = []
    original = adapter.save_scene

    def counting_save() -> bool:
        saves.append(1)
        return original()

    adapter.save_scene = counting_save  # type: ignore[method-assign]
    results = submit_targets(
        SubmitterSession(server_url=BASE),
        "blender",
        per_target_models(params, ctx, targets, {}),
        farm_id="f",
        queue_id="q",
        job_name_prefix="Blender",
        adapter=adapter,
        save_scene=True,
        job_options=None,
    )
    assert results[0].job is None and results[0].error is not None
    assert "output path" in results[0].error.lower()
    assert results[1].job is not None and results[2].job is not None
    assert len(saves) == 1
    assert route.call_count == 2
