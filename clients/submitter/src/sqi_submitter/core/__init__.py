# SPDX-License-Identifier: AGPL-3.0-or-later
"""No-Qt, no-DCC core: everything a submitter UI or headless tool needs."""

from sqi_submitter.core.adapter import HostAdapter
from sqi_submitter.core.context import RenderTarget, SceneContext, frame_range_str
from sqi_submitter.core.errors import FormInvalidError, SubmitterError
from sqi_submitter.core.handoff import ContextAdapter, ContextError, dump_context, load_context
from sqi_submitter.core.joboptions import JobOptions
from sqi_submitter.core.mapping import (
    is_output_path_param,
    is_scene_path_param,
    normalize_name,
    prefill,
)
from sqi_submitter.core.schema import FormField, FormModel
from sqi_submitter.core.session import (
    Settings,
    SubmitterSession,
    group_products,
    resolve_server_url,
)
from sqi_submitter.core.submit import submit_form
from sqi_submitter.core.targets import (
    TargetResult,
    apply_edits,
    is_per_target,
    job_name_for,
    per_target_models,
    submit_targets,
    target_parameters,
)

__all__ = [
    "ContextAdapter",
    "ContextError",
    "FormField",
    "FormInvalidError",
    "FormModel",
    "HostAdapter",
    "JobOptions",
    "RenderTarget",
    "SceneContext",
    "Settings",
    "SubmitterError",
    "SubmitterSession",
    "TargetResult",
    "apply_edits",
    "dump_context",
    "frame_range_str",
    "group_products",
    "is_output_path_param",
    "is_per_target",
    "is_scene_path_param",
    "job_name_for",
    "load_context",
    "normalize_name",
    "per_target_models",
    "prefill",
    "resolve_server_url",
    "submit_form",
    "submit_targets",
    "target_parameters",
]
