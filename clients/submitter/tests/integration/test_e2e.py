# SPDX-License-Identifier: AGPL-3.0-or-later
"""End-to-end seams the unit mocks cannot prove (spec: integration test 1+3)."""

from __future__ import annotations

import contextlib
import os
import pathlib
import tempfile
import time

import pytest
import yaml

from sqi_client import JobStatus
from sqi_submitter.core import (
    FormModel,
    HostAdapter,
    RenderTarget,
    SceneContext,
    SubmitterSession,
    prefill,
    submit_form,
)
from tests.integration.conftest import PRESETS

pytestmark = pytest.mark.integration


class StubAdapter(HostAdapter):
    host_name = "stub"

    def __init__(self, ctx: SceneContext) -> None:
        self._ctx = ctx

    def scene_context(self) -> SceneContext:
        return self._ctx

    def render_targets(self) -> list[RenderTarget]:
        return []


def _install(session: SubmitterSession, path: pathlib.Path) -> str:
    doc = yaml.safe_load(path.read_text())
    name = f"it-{doc['name']}"
    template = yaml.safe_dump(doc["template"], sort_keys=False)
    with contextlib.suppress(Exception):  # absent on first run
        session.client.delete_product(name)
    session.client.create_product(
        name=name,
        template=template,
        format="yaml",
        title=doc["title"],
        category=doc["category"],
        version=doc["version"],
    )
    return name


@pytest.mark.parametrize("preset", PRESETS, ids=lambda p: p.stem)
def test_preset_round_trip(
    session: SubmitterSession, farm_and_queue: tuple[str, str], preset: pathlib.Path
) -> None:
    """Fixture YAML <-> server schema <-> SDK models <-> mapping <-> submit all agree."""
    name = _install(session, preset)
    params = session.parameters(name)
    model = FormModel.from_parameters(params)
    ctx = SceneContext(
        scene_path="/tmp/it/scene.file",
        frame_range="1-4",
        output_path="/tmp/it/out",
        renderer="file",
    )
    model.apply_prefill(prefill([f.parameter for f in model.fields], ctx))
    for f in model.fields:  # fill any required param the convention missed
        if f.required and f.value is None:
            model.set_value(f.parameter.name, "it-value")
    job = submit_form(
        session,
        name,
        model,
        farm_id=farm_and_queue[0],
        queue_id=farm_and_queue[1],
        job_name=f"it {preset.stem}",
    )
    assert job.id
    fetched = session.client.get_job(job.id)
    assert fetched.name == f"it {preset.stem}"
    session.client.cancel_job(job.id)


@pytest.mark.skipif(os.environ.get("SQI_TEST_BLENDER") != "1", reason="SQI_TEST_BLENDER != 1")
def test_blender_full_pipeline_renders_a_frame(
    session: SubmitterSession, farm_and_queue: tuple[str, str]
) -> None:
    """Real server + worker + blender: one frame renders to disk (spec test 3).

    Requires: a worker registered with capability tag blender=true and
    blender on PATH, sharing this machine's filesystem.
    """
    import subprocess

    out_dir = tempfile.mkdtemp(prefix="sqi-blender-it-")
    blend = os.path.join(out_dir, "cube.blend")
    subprocess.run(
        ["blender", "-b", "--factory-startup", "-P", "/dev/stdin"],
        input=f"import bpy; bpy.ops.wm.save_as_mainfile(filepath={blend!r})".encode(),
        check=True,
        timeout=120,
    )
    blender_preset = next(p for p in PRESETS if p.stem == "blender-batch-render")
    name = _install(session, blender_preset)
    model = FormModel.from_parameters(session.parameters(name))
    model.set_value("SceneFile", blend)
    model.set_value("Frames", "1")
    model.set_value("OutputDir", out_dir)
    job = submit_form(
        session,
        name,
        model,
        farm_id=farm_and_queue[0],
        queue_id=farm_and_queue[1],
    )
    deadline = time.time() + 300
    while time.time() < deadline:
        status = session.client.get_job(job.id).status
        if status == JobStatus.COMPLETED:
            break
        assert status != JobStatus.FAILED, "render job failed"
        time.sleep(5)
    else:
        pytest.fail("render did not finish in 300s")
    rendered = [f for f in os.listdir(out_dir) if f.startswith("frame_")]
    assert rendered, f"no frame written to {out_dir}"
