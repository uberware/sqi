# SPDX-License-Identifier: AGPL-3.0-or-later
"""The one submit flow shared by every UI (Qt dialog, bpy panel, headless)."""

from __future__ import annotations

from sqi_client.models import Job
from sqi_submitter.core.adapter import HostAdapter
from sqi_submitter.core.errors import FormInvalidError, SubmitterError
from sqi_submitter.core.mapping import is_output_path_param, is_scene_path_param
from sqi_submitter.core.schema import FormModel
from sqi_submitter.core.session import SubmitterSession


def submit_form(
    session: SubmitterSession,
    product_name: str,
    model: FormModel,
    *,
    farm_id: str,
    queue_id: str,
    job_name: str | None = None,
    adapter: HostAdapter | None = None,
    save_scene: bool = True,
) -> Job:
    """Save-if-needed → validate → submit. Raises SubmitterError variants."""
    if adapter is not None:
        ctx = adapter.scene_context()
        if not ctx.scene_path:
            raise SubmitterError(
                "Save your scene before submitting — the farm renders the file on disk."
            )
        for f in model.fields:
            if is_scene_path_param(f.parameter.name):
                # Scene path is host-authoritative: always the open file.
                model.set_value(f.parameter.name, ctx.scene_path)
            elif is_output_path_param(f.parameter.name) and not f.value and ctx.output_path:
                # Output left blank defaults to the scene's own output setting.
                model.set_value(f.parameter.name, ctx.output_path)
    if (
        adapter is not None
        and save_scene
        and adapter.is_scene_modified()
        and not adapter.save_scene()
    ):
        raise SubmitterError(
            "The scene could not be saved. Save it manually, then submit "
            "(the farm renders the file on disk)."
        )
    if not model.validate():
        raise FormInvalidError(model.errors())
    return session.submit(
        product_name,
        parameters=model.values(),
        farm_id=farm_id,
        queue_id=queue_id,
        job_name=job_name,
    )
