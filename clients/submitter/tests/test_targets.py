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


def _movie(name: str, item: str, per_task: str) -> RenderTarget:
    """An After Effects movie: one file, so its ``FramesPerTask`` is locked."""
    return RenderTarget(
        name=name,
        kind="rq_item",
        extra={"RenderQueueItem": item, "FramesPerTask": per_task},
        locked=frozenset({"FramesPerTask"}),
    )


def test_target_parameters_exclude_a_locked_key_every_target_carries() -> None:
    single = [_movie("movie", "1", "240")]
    assert target_parameters(AE_PARAMS, single) == frozenset({"RenderQueueItem"})
    movies = [_movie("m1", "1", "240"), _movie("m2", "2", "120")]
    assert target_parameters(AE_PARAMS, movies) == frozenset({"RenderQueueItem"})


def test_target_parameters_exclude_a_locked_key_by_normalized_name() -> None:
    movie = RenderTarget(
        name="movie",
        kind="rq_item",
        extra={"RenderQueueItem": "1", "frames_per_task": "240"},
        locked=frozenset({"frames_per_task"}),
    )
    assert target_parameters(AE_PARAMS, [movie]) == frozenset({"RenderQueueItem"})


def test_target_parameters_movie_and_images_mix() -> None:
    images = RenderTarget(name="images", kind="rq_item", extra={"RenderQueueItem": "2"})
    targets = [_movie("movie", "1", "240"), images]
    assert target_parameters(AE_PARAMS, targets) == frozenset({"RenderQueueItem"})


def test_locked_key_alone_never_makes_a_product_per_target() -> None:
    """A generic sequence product is whole-scene even for an all-movie queue."""
    sequence = [_p("Frames"), _p("FramesPerTask", "INT", "10")]
    movies = [_movie("m1", "1", "240"), _movie("m2", "2", "120")]
    assert not is_per_target(sequence, movies)
    assert not is_per_target(sequence, movies[:1])
    assert is_per_target(AE_PARAMS, movies)
    assert is_per_target(AE_PARAMS, movies[:1])


def test_per_target_models_single_locked_target_keeps_its_value() -> None:
    edited = {"FramesPerTask": "5", "OutputPath": "/edited"}
    (only,) = per_target_models(AE_PARAMS, CTX, [_movie("movie", "1", "240")], edited)
    values = only[1].values()
    assert values["FramesPerTask"] == "240"
    assert values["OutputPath"] == "/edited"
    assert values["RenderQueueItem"] == "1"


def test_per_target_models_locked_value_beats_edit_for_every_movie() -> None:
    movies = [_movie("m1", "1", "240"), _movie("m2", "2", "120")]
    models = per_target_models(AE_PARAMS, CTX, movies, {"FramesPerTask": "5"})
    assert [m.values()["FramesPerTask"] for _, m in models] == ["240", "120"]


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
    first, second = (m.values() for _, m in models)
    assert first["Frames"] == "1-10"
    assert second["Frames"] == "20-30"
    assert (first["Scene"], first["ViewLayer"]) == ("Scene", "A")
    assert (second["Scene"], second["ViewLayer"]) == ("Scene", "B")
    assert first["SceneFile"] == "/x/s.scene"


def test_per_target_models_edit_applies_to_all() -> None:
    models = per_target_models(BLENDER_PARAMS, CTX, _blender_targets(), {"Frames": "1-5"})
    assert [m.values()["Frames"] for _, m in models] == ["1-5", "1-5"]


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
    movie, images = (m.values() for _, m in models)
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
    assert models[0][1].values()["FramesPerTask"] == "240"
    assert models[1][1].values()["FramesPerTask"] == "10"


def test_target_parameter_edit_is_ignored() -> None:
    targets = _blender_targets()
    base = per_target_models(BLENDER_PARAMS, CTX, targets, {})
    edited = per_target_models(BLENDER_PARAMS, CTX, targets, {"ViewLayer": "ZZZ"})
    assert [m.values() for _, m in edited] == [m.values() for _, m in base]


def test_explicit_target_params_keep_edits_to_keys_a_subset_shares() -> None:
    params = [_p("SceneFile", "PATH"), _p("Pass"), _p("OutputPath", "PATH")]
    a = RenderTarget(name="A", kind="pass", extra={"Pass": "A", "OutputPath": "/a"})
    edited = {"OutputPath": "/edited", "Pass": "ZZZ"}
    # Derived from the given targets, A alone makes OutputPath a target parameter.
    (derived,) = per_target_models(params, CTX, [a], edited)
    assert derived[1].values()["OutputPath"] == "/a"
    # Given the set computed over every target, the edit is honoured.
    (explicit,) = per_target_models(params, CTX, [a], edited, target_params=frozenset({"Pass"}))
    assert explicit[1].values()["OutputPath"] == "/edited"
    assert explicit[1].values()["Pass"] == "A"  # a real target parameter stays the target's


def test_edit_for_unknown_parameter_is_ignored() -> None:
    models = per_target_models(BLENDER_PARAMS, CTX, _blender_targets(), {"Nope": "1"})
    assert all("Nope" not in m.values() for _, m in models)


def test_job_name_for() -> None:
    target = RenderTarget(name="Scene/B", kind="view_layer")
    assert job_name_for("Blender", target) == "Blender - Scene/B"


def _jobs_route(side_effect: list[httpx.Response] | None = None) -> respx.Route:
    route = respx.post(f"{BASE}/api/v1/products/blender/jobs")
    if side_effect is not None:
        return route.mock(side_effect=side_effect)
    return route.mock(return_value=httpx.Response(201, json={"id": "j", "name": "n"}))


def _submit(
    models: list[tuple[RenderTarget, FormModel]],
    adapter: MiniAdapter | None = None,
    save_scene: bool = False,
) -> list[TargetResult]:
    return submit_targets(
        SubmitterSession(server_url=BASE),
        "blender",
        models,
        farm_id="f",
        queue_id="q",
        job_name_prefix="Blender",
        adapter=adapter,
        save_scene=save_scene,
        job_options=None,
    )


def _counting(adapter: MiniAdapter) -> list[int]:
    """Count the adapter's saves from now on."""
    saves: list[int] = []
    original = adapter.save_scene

    def counting_save() -> bool:
        saves.append(1)
        return original()

    adapter.save_scene = counting_save  # type: ignore[method-assign]
    return saves


def _three_targets() -> list[RenderTarget]:
    return [
        RenderTarget(name=n, kind="view_layer", extra={"Scene": "S", "ViewLayer": n})
        for n in ("A", "B", "C")
    ]


@respx.mock
def test_submit_targets_reports_partial_failure() -> None:
    route = _jobs_route(
        side_effect=[
            httpx.Response(201, json={"id": "j1", "name": "n1"}),
            httpx.Response(400, json={"error": "bad layer"}),
            httpx.Response(201, json={"id": "j3", "name": "n3"}),
        ]
    )
    targets = _three_targets()
    models = per_target_models(BLENDER_PARAMS, CTX, targets, {"Frames": "1-5"})
    results = _submit(models)
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
def test_submit_targets_names_the_invalid_fields() -> None:
    route = _jobs_route()
    params = [*BLENDER_PARAMS, _p("Chunk", "INT")]
    models = per_target_models(params, CTX, _three_targets()[:1], {"Chunk": "x"})
    (result,) = _submit(models)
    assert route.call_count == 0
    assert result.job is None
    # "Please fix the highlighted fields." tells the artist nothing in a list.
    assert result.error == "Invalid parameters — Chunk: Must be an integer"


@respx.mock
def test_submit_targets_saves_once() -> None:
    _jobs_route()
    adapter = MiniAdapter(modified=True)
    saves = _counting(adapter)
    models = per_target_models(BLENDER_PARAMS, CTX, _three_targets(), {})
    results = _submit(models, adapter, save_scene=True)
    assert [r.error for r in results] == [None, None, None]
    assert len(saves) == 1


@respx.mock
def test_submit_targets_failed_save_submits_nothing() -> None:
    route = _jobs_route()
    adapter = MiniAdapter(modified=True, can_save=False)
    models = per_target_models(BLENDER_PARAMS, CTX, _three_targets(), {})
    results = _submit(models, adapter, save_scene=True)
    assert route.call_count == 0
    assert len(results) == 3
    assert all(r.job is None for r in results)
    assert all(r.error is not None and "could not be saved" in r.error for r in results)


@respx.mock
def test_submit_targets_saves_even_when_first_target_fails_early() -> None:
    route = _jobs_route()
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
    saves = _counting(adapter)
    results = _submit(per_target_models(params, ctx, targets, {}), adapter, save_scene=True)
    assert results[0].job is None and results[0].error is not None
    assert "output path" in results[0].error.lower()
    assert results[1].job is not None and results[2].job is not None
    assert len(saves) == 1
    assert route.call_count == 2
